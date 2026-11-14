package attest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AitorConS/jerboa/internal/durable"
	"github.com/AitorConS/jerboa/internal/durable/durabletest"
	"github.com/AitorConS/jerboa/internal/naming"
	"github.com/AitorConS/jerboa/internal/registry/client"
	"github.com/AitorConS/jerboa/internal/registry/registrytest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests are about the ways verifying an image's signatures can go
// wrong: a bundle that was altered, signed by someone else, signed outside
// its certificate's life or under another root; a registry that lists,
// withholds or mangles attestations; and the local files that say who must
// have signed (the policy and the remembered signers) being damaged or half
// written. In every one of them the answer must be a refusal, never "this
// image needs no signature".

const (
	recordedWorkflow = "acme/app/.github/workflows/publish.yml"
	evilWorkflowURL  = "https://github.com/evil/app/.github/workflows/publish.yml@refs/heads/main"
)

func noWait(int) time.Duration { return time.Millisecond }

func digestOf(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// attested is a fake registry serving attestations the test puts in it.
type attested struct {
	reg *registrytest.Server
	cl  *client.Client
	cdn string
	ref naming.ImageReference
}

func newAttested(t *testing.T) *attested {
	t.Helper()
	reg := registrytest.New()
	t.Cleanup(reg.Close)
	cl := client.New(reg.Host())
	cl.Backoff = noWait
	return &attested{reg: reg, cl: cl, cdn: reg.URL + "/cdn",
		ref: naming.ImageReference{Registry: reg.Host(), Namespace: "acme", Repository: "app", Tag: "v1"}}
}

type listedBundle struct {
	identity string
	data     []byte
}

// list makes the registry list bundles as the attestations of subject, each
// recorded as uploaded by its identity.
func (a *attested) list(t *testing.T, subject string, bundles ...listedBundle) {
	t.Helper()
	prefix := "attestations/acme/app/" + strings.TrimPrefix(subject, "sha256:") + "/"
	entries := []map[string]any{}
	for _, b := range bundles {
		d := digestOf(b.data)
		a.reg.SetObject(prefix+strings.TrimPrefix(d, "sha256:")+".json", b.data)
		entries = append(entries, map[string]any{"digest": d, "predicateType": SBOMType, "size": len(b.data), "identity": b.identity})
	}
	index, err := json.Marshal(map[string]any{"attestations": entries})
	require.NoError(t, err)
	a.reg.SetObject(prefix+"index.json", index)
}

// edit rewrites one part of a bundle, as someone altering it in the registry
// or on the way would.
func edit(t *testing.T, bundle []byte, change func(b map[string]any)) []byte {
	t.Helper()
	var b map[string]any
	require.NoError(t, json.Unmarshal(bundle, &b))
	change(b)
	out, err := json.Marshal(b)
	require.NoError(t, err)
	return out
}

func part(b map[string]any, keys ...string) map[string]any {
	for _, k := range keys {
		b = b[k].(map[string]any)
	}
	return b
}

// The two ways a pull checks signatures: against an identity the user, the
// policy or an earlier pull named, and against the workflow the registry
// recorded as the uploader. Every bundle below must be refused in both.
func TestFaults_CheckRefusesEveryBadBundle(t *testing.T) {
	vs := virtualSigstore(t)
	elsewhere := virtualSigstore(t)
	now := time.Now().Add(5 * time.Minute)
	good := signedBundle(t, vs, workflow, GitHubIssuer, statement(t, subject), now)
	aboutOther := signedBundle(t, vs, workflow, GitHubIssuer, statement(t, other), now)
	byEvil := signedBundle(t, vs, evilWorkflowURL, GitHubIssuer, statement(t, subject), now)
	wantID, err := ParseIdentity(recordedWorkflow + "@refs/heads/main")
	require.NoError(t, err)

	otherStatement, err := NewStatement("registry.jerboa.dev/acme/app", subject, SBOMType, []byte(`{"bomFormat":"CycloneDX","components":["added later"]}`))
	require.NoError(t, err)

	cases := map[string]listedBundle{
		"statement replaced under the same signature": {recordedWorkflow, edit(t, good, func(b map[string]any) {
			part(b, "dsseEnvelope")["payload"] = otherStatement
		})},
		"signature of another statement": {recordedWorkflow, edit(t, good, func(b map[string]any) {
			var o map[string]any
			require.NoError(t, json.Unmarshal(aboutOther, &o))
			part(b, "dsseEnvelope")["signatures"] = part(o, "dsseEnvelope")["signatures"]
		})},
		"signature bytes damaged": {recordedWorkflow, edit(t, good, func(b map[string]any) {
			sigs := part(b, "dsseEnvelope")["signatures"].([]any)
			sigs[0].(map[string]any)["sig"] = []byte("not a signature at all, only bytes of about the right length........")
		})},
		"certificate of another signer": {recordedWorkflow, edit(t, good, func(b map[string]any) {
			var o map[string]any
			require.NoError(t, json.Unmarshal(byEvil, &o))
			part(b, "verificationMaterial")["certificate"] = part(o, "verificationMaterial")["certificate"]
		})},
		"log entry of another signature": {recordedWorkflow, edit(t, good, func(b map[string]any) {
			var o map[string]any
			require.NoError(t, json.Unmarshal(aboutOther, &o))
			part(b, "verificationMaterial")["tlogEntries"] = part(o, "verificationMaterial")["tlogEntries"]
		})},
		"no log entry":                    {recordedWorkflow, edit(t, good, func(b map[string]any) { delete(part(b, "verificationMaterial"), "tlogEntries") })},
		"no certificate":                  {recordedWorkflow, edit(t, good, func(b map[string]any) { delete(part(b, "verificationMaterial"), "certificate") })},
		"no signature":                    {recordedWorkflow, edit(t, good, func(b map[string]any) { part(b, "dsseEnvelope")["signatures"] = []any{} })},
		"about another image":             {recordedWorkflow, aboutOther},
		"signed by another workflow":      {recordedWorkflow, byEvil},
		"signed before the certificate":   {recordedWorkflow, signedBundle(t, vs, workflow, GitHubIssuer, statement(t, subject), time.Now().Add(-time.Hour))},
		"signed after the certificate":    {recordedWorkflow, signedBundle(t, vs, workflow, GitHubIssuer, statement(t, subject), time.Now().Add(time.Hour))},
		"issued by another OIDC provider": {recordedWorkflow, signedBundle(t, vs, workflow, "https://accounts.example.com", statement(t, subject), now)},
		"signed under another root":       {recordedWorkflow, signedBundle(t, elsewhere, workflow, GitHubIssuer, statement(t, subject), now)},
		"truncated":                       {recordedWorkflow, good[:len(good)/2]},
		"not a bundle":                    {recordedWorkflow, []byte(`{"mediaType":"` + BundleMediaType + `"}`)},
		"empty":                           {recordedWorkflow, nil},
	}

	a := newAttested(t)
	ctx := context.Background()

	// The bundle the cases are made from verifies both ways, so each refusal
	// below is caused by what the case changed.
	a.list(t, subject, listedBundle{recordedWorkflow, good})
	res, err := Check(ctx, a.cl, a.cdn, a.ref, subject, &wantID, vs)
	require.NoError(t, err)
	require.Len(t, res, 1)
	res, err = Check(ctx, a.cl, a.cdn, a.ref, subject, nil, vs)
	require.NoError(t, err)
	require.Equal(t, workflow, res[0].Signer)

	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			a.list(t, subject, c)
			res, err := Check(ctx, a.cl, a.cdn, a.ref, subject, &wantID, vs)
			require.Error(t, err, "accepted against a named identity")
			assert.Empty(t, res)
			res, err = Check(ctx, a.cl, a.cdn, a.ref, subject, nil, vs)
			require.Error(t, err, "accepted against the registry's record")
			assert.Empty(t, res)
		})
	}
}

// A registry that records the attacker's workflow as the uploader makes an
// attacker's bundle consistent with its record. An identity the puller holds
// (a policy rule, a remembered signer, --identity) must still refuse it.
func TestFaults_ARegistryRecordDoesNotOverrideTheWantedIdentity(t *testing.T) {
	vs := virtualSigstore(t)
	byEvil := signedBundle(t, vs, evilWorkflowURL, GitHubIssuer, statement(t, subject), time.Now().Add(5*time.Minute))
	a := newAttested(t)
	a.list(t, subject, listedBundle{"evil/app/.github/workflows/publish.yml", byEvil})

	for _, want := range []string{"acme/app", recordedWorkflow, recordedWorkflow + "@refs/heads/main", "evil/app/.github/workflows/other.yml", "evil/app/.github/workflows/publish.yml@refs/tags/v1"} {
		id, err := ParseIdentity(want)
		require.NoError(t, err)
		_, err = Check(context.Background(), a.cl, a.cdn, a.ref, subject, &id, vs)
		require.ErrorContains(t, err, "is signed by "+want)
	}
	// A record that names no workflow, or nothing that parses, vouches for
	// nobody, whatever the certificate says.
	for _, record := range []string{"evil/app", "", "evil", "https://github.com/evil/app/.github/workflows/publish.yml"} {
		a.list(t, subject, listedBundle{record, byEvil})
		_, err := Check(context.Background(), a.cl, a.cdn, a.ref, subject, nil, vs)
		require.ErrorContains(t, err, "signed by the workflow that uploaded it", record)
	}
}

// One good attestation among bad ones is enough, and only the good one is
// reported: a damaged or foreign bundle beside it neither blocks the pull nor
// appears as having verified.
func TestFaults_BadBundlesBesideAGoodOne(t *testing.T) {
	vs := virtualSigstore(t)
	now := time.Now().Add(5 * time.Minute)
	good := signedBundle(t, vs, workflow, GitHubIssuer, statement(t, subject), now)
	byEvil := signedBundle(t, vs, evilWorkflowURL, GitHubIssuer, statement(t, subject), now)
	a := newAttested(t)
	a.list(t, subject,
		listedBundle{recordedWorkflow, []byte("garbage")},
		listedBundle{recordedWorkflow, byEvil},
		listedBundle{recordedWorkflow, good},
		listedBundle{recordedWorkflow, good[:100]},
	)
	res, err := Check(context.Background(), a.cl, a.cdn, a.ref, subject, nil, vs)
	require.NoError(t, err)
	require.Len(t, res, 1)
	assert.Equal(t, workflow, res[0].Signer)
	assert.JSONEq(t, string(statement(t, subject)), string(res[0].Statement))
}

// What the registry can do to the listing itself. A listing that cannot be
// read is an error; only a subject the registry says has no attestations is
// unsigned, and Check reports that as ErrUnsigned for the caller to decide.
func TestFaults_CheckWithAFailingRegistry(t *testing.T) {
	vs := virtualSigstore(t)
	good := signedBundle(t, vs, workflow, GitHubIssuer, statement(t, subject), time.Now().Add(5*time.Minute))
	goodDigest := digestOf(good)
	hex64 := strings.TrimPrefix(subject, "sha256:")
	indexOf := func(digests ...string) string {
		entries := []map[string]any{}
		for _, d := range digests {
			entries = append(entries, map[string]any{"digest": d, "predicateType": SBOMType, "identity": recordedWorkflow})
		}
		data, _ := json.Marshal(map[string]any{"attestations": entries})
		return string(data)
	}
	ref := naming.ImageReference{Namespace: "acme", Repository: "app", Tag: "v1"}
	id, err := ParseIdentity("acme/app")
	require.NoError(t, err)

	type answer struct {
		status int
		body   string
	}
	cases := map[string]struct {
		index, bundle answer
		unsigned      bool
		contains      string
	}{
		"no listing":                      {index: answer{404, ""}, unsigned: true},
		"empty listing":                   {index: answer{200, `{"attestations":[]}`}, unsigned: true},
		"listing is an error page":        {index: answer{200, "<html>502 Bad Gateway</html>"}, contains: "decode answer"},
		"listing cut short":               {index: answer{200, indexOf(goodDigest)[:40]}, contains: "decode answer"},
		"listing fails":                   {index: answer{500, ""}, contains: "HTTP 500"},
		"listing is forbidden":            {index: answer{403, ""}, contains: "HTTP 403"},
		"listing names a path":            {index: answer{200, indexOf("sha256:../../../recipes/x")}, contains: "invalid attestation digest"},
		"listed bundle is missing":        {index: answer{200, indexOf(goodDigest)}, bundle: answer{404, ""}, contains: "HTTP 404"},
		"listed bundle fails":             {index: answer{200, indexOf(goodDigest)}, bundle: answer{503, ""}, contains: "HTTP 503"},
		"bundle is not what was listed":   {index: answer{200, indexOf(goodDigest)}, bundle: answer{200, string(good) + " "}, contains: "does not match its digest"},
		"bundle cut short":                {index: answer{200, indexOf(goodDigest)}, bundle: answer{200, string(good[:len(good)/2])}, contains: "does not match its digest"},
		"another bundle under the digest": {index: answer{200, indexOf(goodDigest)}, bundle: answer{200, "{}"}, contains: "does not match its digest"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				a := c.bundle
				if strings.HasSuffix(r.URL.Path, "/"+hex64+"/index.json") {
					a = c.index
				}
				w.WriteHeader(a.status)
				_, _ = w.Write([]byte(a.body))
			}))
			defer srv.Close()
			cl := client.New(strings.TrimPrefix(srv.URL, "http://"))
			cl.Backoff = noWait
			for _, want := range []*Identity{&id, nil} {
				res, err := Check(context.Background(), cl, srv.URL, ref, subject, want, vs)
				require.Error(t, err)
				assert.Empty(t, res)
				assert.Equal(t, c.unsigned, errors.Is(err, ErrUnsigned), "only a subject without attestations is unsigned: %v", err)
				if c.contains != "" {
					assert.ErrorContains(t, err, c.contains)
				}
			}
		})
	}
}

// A bundle larger than a bundle may be vouches for nothing, and a listing of
// any length costs a bounded number of downloads.
func TestFaults_CheckBoundsWhatItReads(t *testing.T) {
	vs := virtualSigstore(t)
	good := signedBundle(t, vs, workflow, GitHubIssuer, statement(t, subject), time.Now().Add(5*time.Minute))
	a := newAttested(t)
	ctx := context.Background()

	padded := edit(t, good, func(b map[string]any) { b["padding"] = strings.Repeat("x", MaxBundle) })
	a.list(t, subject, listedBundle{recordedWorkflow, padded})
	_, err := Check(ctx, a.cl, a.cdn, a.ref, subject, nil, vs)
	require.ErrorIs(t, err, ErrUnsigned)

	many := []listedBundle{}
	for i := range 3 * maxChecked {
		many = append(many, listedBundle{recordedWorkflow, fmt.Appendf(nil, `{"junk":%d}`, i)})
	}
	a.list(t, subject, append(many, listedBundle{recordedWorkflow, good})...)
	before := a.reg.Requests["GET /cdn"]
	res, err := Check(ctx, a.cl, a.cdn, a.ref, subject, nil, vs)
	require.NoError(t, err, "the newest attestation is among those read")
	require.Len(t, res, 1)
	assert.LessOrEqual(t, a.reg.Requests["GET /cdn"]-before, maxChecked+1)
}

type signerFunc func(context.Context, []byte) ([]byte, error)

func (f signerFunc) Sign(ctx context.Context, statement []byte) ([]byte, error) {
	return f(ctx, statement)
}

// Publishing signs and uploads each predicate in turn. A signer that fails
// uploads nothing; an upload that fails reports what was published before it
// so the caller does not claim attestations that are not there.
func TestFaults_PublishStopsAtTheFirstFailure(t *testing.T) {
	var puts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if puts.Add(1) > 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"attestation":"` + other + `"}`))
	}))
	defer srv.Close()
	cl := client.New(strings.TrimPrefix(srv.URL, "http://"))
	cl.Backoff = noWait
	ref := naming.ImageReference{Registry: "r", Namespace: "acme", Repository: "app"}
	predicates := []Predicate{{Type: ProvenanceType, Body: []byte(`{}`)}, {Type: SBOMType, Body: []byte(`{}`)}, {Type: "third", Body: []byte(`{}`)}}
	ctx := context.Background()

	boom := errors.New("fulcio is down")
	out, err := Publish(ctx, cl, "token", ref, subject, predicates, signerFunc(func(context.Context, []byte) ([]byte, error) { return nil, boom }))
	require.ErrorIs(t, err, boom)
	assert.Empty(t, out)
	assert.Zero(t, puts.Load(), "something was uploaded without a signature")

	signed := 0
	out, err = Publish(ctx, cl, "token", ref, subject, predicates, signerFunc(func(_ context.Context, st []byte) ([]byte, error) {
		signed++
		return st, nil
	}))
	require.ErrorContains(t, err, "upload the "+SBOMType+" attestation")
	require.Len(t, out, 1)
	assert.Equal(t, ProvenanceType, out[0].PredicateType)
	assert.Equal(t, 2, signed, "nothing is signed after an upload failed")

	_, err = Publish(ctx, cl, "token", ref, "sha256:short", predicates, signerFunc(func(context.Context, []byte) ([]byte, error) { return nil, nil }))
	require.ErrorContains(t, err, "invalid subject digest")
	_, err = Publish(ctx, cl, "token", ref, subject, []Predicate{{Type: SBOMType, Body: []byte("{half")}}, signerFunc(func(context.Context, []byte) ([]byte, error) { return nil, nil }))
	require.ErrorContains(t, err, "not JSON")
}

var signersRef = naming.ImageReference{Registry: "registry.jerboa.dev", Namespace: "acme", Repository: "web", Tag: "v1"}

func seedSigners(t *testing.T) (string, Identity) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "jerboa", "signers.json")
	s, err := LoadSigners(path)
	require.NoError(t, err)
	id, err := ParseIdentity("acme/images/.github/workflows/publish.yml")
	require.NoError(t, err)
	require.NoError(t, s.Remember(signersRef, id))
	return path, id
}

// The remembered signers are what turns "whoever signs it" into "the same
// workflow as before". A save that fails or is cut by a crash must leave the
// file readable and must not lose a signer remembered earlier: losing one
// would let the next pull trust a new signer silently.
func TestFaults_SignersSaveFailsAtEveryStep(t *testing.T) {
	otherRef := naming.ImageReference{Registry: "registry.jerboa.dev", Namespace: "acme", Repository: "api"}
	newID, err := ParseIdentity("acme/api/.github/workflows/release.yml")
	require.NoError(t, err)
	ops := map[string]func(s *Signers) error{
		"remember another": func(s *Signers) error { return s.Remember(otherRef, newID) },
		"forget":           func(s *Signers) error { _, err := s.Forget(SignerKey(signersRef)); return err },
	}
	for opName, op := range ops {
		for _, f := range durabletest.Failures() {
			t.Run(opName+"/"+f.Name, func(t *testing.T) {
				path, id := seedSigners(t)
				s, err := LoadSigners(path)
				require.NoError(t, err)
				before := s.List()

				inj := durabletest.Inject(t, f, 0, nil)
				opErr := op(s)
				require.True(t, inj.Fired())
				durable.SetFault(nil)

				after, err := LoadSigners(path)
				require.NoError(t, err, "a failed save left signers that do not load")
				if opErr != nil && f.Step != durable.StepSyncDir {
					assert.Equal(t, before, after.List(), "a failed save changed who is remembered")
					got, ok := after.Get(signersRef)
					require.True(t, ok)
					assert.Equal(t, id, got)
				} else {
					assert.Equal(t, s.List(), after.List())
				}
				if !f.Crash() {
					entries, err := os.ReadDir(filepath.Dir(path))
					require.NoError(t, err)
					assert.Len(t, entries, 1, "a failed save left a temporary file")
				}
				// The operation can be repeated on what was loaded.
				require.NoError(t, op(after))
			})
		}
	}
}

// A signers file that cannot be read whole is an error. Treating it as "no
// signer remembered" would downgrade every repository to trust on first use.
func TestFaults_DamagedSignersFileIsAnError(t *testing.T) {
	path, _ := seedSigners(t)
	whole, err := os.ReadFile(path)
	require.NoError(t, err)
	cases := map[string][]byte{
		"empty":               nil,
		"truncated":           whole[:len(whole)/2],
		"zeroed":              make([]byte, len(whole)),
		"garbage":             {0xff, 0xfe, 0x00},
		"an array":            []byte(`[]`),
		"signers is a string": []byte(`{"signers":"acme/images"}`),
		"an identity damaged": []byte(`{"signers":{"registry.jerboa.dev/acme/web":"acme"}}`),
		"an identity emptied": []byte(`{"signers":{"registry.jerboa.dev/acme/web":""}}`),
		"two documents":       append(append([]byte{}, whole...), whole...),
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			require.NoError(t, os.WriteFile(path, content, 0o600))
			s, err := LoadSigners(path)
			require.Error(t, err)
			assert.Nil(t, s, "a damaged file must not yield a usable, empty set of signers")
			assert.ErrorContains(t, err, filepath.Base(path), "the error must say which file is damaged")
		})
	}
}

func TestFaults_UnreadableSignersFileIsAnError(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs POSIX permissions and a non-root user")
	}
	path, _ := seedSigners(t)
	require.NoError(t, os.Chmod(path, 0o000))
	_, err := LoadSigners(path)
	require.Error(t, err)

	// A directory where the file should be, as a bad restore leaves.
	dir := filepath.Join(t.TempDir(), "signers.json")
	require.NoError(t, os.Mkdir(dir, 0o700))
	_, err = LoadSigners(dir)
	require.Error(t, err)
}

// The file names who the user trusts: it stays private however it was found,
// and a save into a directory that refuses it reports the failure.
func TestFaults_SignersFileStaysPrivate(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs POSIX permissions and a non-root user")
	}
	path, id := seedSigners(t)
	require.NoError(t, os.Chmod(path, 0o644))
	s, err := LoadSigners(path)
	require.NoError(t, err)
	require.NoError(t, s.Remember(naming.ImageReference{Registry: "r", Namespace: "n", Repository: "x"}, id))
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())

	require.NoError(t, os.Chmod(filepath.Dir(path), 0o500))
	t.Cleanup(func() { _ = os.Chmod(filepath.Dir(path), 0o700) })
	require.Error(t, s.Remember(naming.ImageReference{Registry: "r", Namespace: "n", Repository: "y"}, id))
	again, err := LoadSigners(path)
	require.NoError(t, err)
	assert.Len(t, again.List(), 2, "the failed save changed the file")
}

// A reference pinned to a tag or digest is remembered without it, so a
// signer of one tag is required of every tag of the repository.
func TestFaults_RememberedSignerHoldsForEveryTagOfTheRepository(t *testing.T) {
	path, _ := seedSigners(t)
	s, err := LoadSigners(path)
	require.NoError(t, err)
	for _, ref := range []naming.ImageReference{
		{Registry: "registry.jerboa.dev", Namespace: "acme", Repository: "web", Tag: "v2"},
		{Registry: "registry.jerboa.dev", Namespace: "acme", Repository: "web", Digest: subject},
	} {
		_, ok := s.Get(ref)
		assert.True(t, ok, ref)
	}
	for _, ref := range []naming.ImageReference{
		{Registry: "other.example", Namespace: "acme", Repository: "web", Tag: "v1"},
		{Registry: "registry.jerboa.dev", Namespace: "acme2", Repository: "web", Tag: "v1"},
		{Registry: "registry.jerboa.dev", Namespace: "acme", Repository: "web2", Tag: "v1"},
	} {
		_, ok := s.Get(ref)
		assert.False(t, ok, ref)
	}
}

// A policy that cannot be read whole is an error, never an empty policy:
// an empty policy requires no signatures of anyone but the official images.
func TestFaults_DamagedPolicyIsAnError(t *testing.T) {
	whole := []byte(`{"require":[{"namespace":"acme","identity":"acme/infra"},{"registry":"localhost:8787","namespace":"dev","identity":"acme/dev"}]}`)
	cases := map[string][]byte{
		"empty":                  nil,
		"truncated":              whole[:len(whole)/2],
		"truncated in a string":  whole[:30],
		"zeroed":                 make([]byte, len(whole)),
		"garbage":                {0xff, 0xfe, 0x00},
		"an array":               []byte(`[]`),
		"rule without identity":  []byte(`{"require":[{"namespace":"acme"}]}`),
		"rule without a name":    []byte(`{"require":[{"identity":"acme/infra"}]}`),
		"misspelled key":         []byte(`{"require":[{"namespace":"acme","identity":"acme/infra","registri":"x"}]}`),
		"identity of wrong type": []byte(`{"require":[{"namespace":"acme","identity":["acme/infra"]}]}`),
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "policy.json")
			require.NoError(t, os.WriteFile(path, content, 0o600))
			p, err := LoadPolicy(path)
			require.Error(t, err)
			assert.Empty(t, p.Require)
			assert.ErrorContains(t, err, "policy")
		})
	}

	// The whole file loads, so the cases above fail for what they damage.
	path := filepath.Join(t.TempDir(), "policy.json")
	require.NoError(t, os.WriteFile(path, whole, 0o600))
	p, err := LoadPolicy(path)
	require.NoError(t, err)
	require.Len(t, p.Require, 2)

	if runtime.GOOS != "windows" && os.Geteuid() != 0 {
		require.NoError(t, os.Chmod(path, 0o000))
		_, err = LoadPolicy(path)
		require.Error(t, err, "an unreadable policy is not an empty one")
	}
	_, err = LoadPolicy(t.TempDir())
	require.Error(t, err, "a directory is not an empty policy")
}

// The official rule holds with no policy, a damaged policy's zero value, and
// a policy that tries to redirect it by naming the registry explicitly.
func TestFaults_OfficialRuleSurvivesAnyPolicy(t *testing.T) {
	policies := []Policy{
		{},
		{Require: []Rule{{Namespace: "jerboa", Identity: "evil/images"}}},
		{Require: []Rule{{Registry: naming.DefaultRegistry, Namespace: "jerboa", Identity: "evil/images/.github/workflows/publish.yml@refs/heads/main"}}},
	}
	for _, p := range policies {
		id, ok, err := p.For(naming.DefaultRegistry, "jerboa")
		require.NoError(t, err)
		require.True(t, ok)
		assert.Equal(t, Official.Identity, id.String())
	}
	// The same namespace on another registry is not the official one.
	_, ok, err := Policy{}.For("registry.example", "jerboa")
	require.NoError(t, err)
	assert.False(t, ok)
}

func FuzzParseIdentity(f *testing.F) {
	for _, seed := range []string{
		"acme/app",
		"acme/app/.github/workflows/publish.yml",
		"acme/app/.github/workflows/publish.yml@refs/heads/main",
		"acme/app/.github/workflows/a/b.yaml@refs/tags/v1.0.0",
		"acme/app@refs/heads/main",
		"acme/app/.github/workflows/../../evil.yml",
		"acme/app/.github/workflows/x.yml@refs/heads/main@refs/heads/dev",
		"acme/.*/.github/workflows/x.yml",
		"acme/app/.github/workflows/x.yml@refs/heads/.+",
		"a/b/\n.github/workflows/x.yml",
		"/", "", "@", "a//b",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, s string) {
		id, err := ParseIdentity(s)
		if err != nil {
			require.Equal(t, Identity{}, id)
			return
		}
		// What parses is written back in a form that reads the same. (The
		// text itself may differ: "owner/repo/" is read as "owner/repo".)
		again, err := ParseIdentity(id.String())
		require.NoError(t, err)
		require.Equal(t, id, again)
		if id.Ref != "" {
			require.NotEmpty(t, id.Workflow, "a ref without a workflow")
		}

		// The identity becomes a pattern for the certificate's SAN. Whatever
		// characters it holds, the pattern compiles and matches only URLs of
		// that repository.
		_, err = id.certificate()
		require.NoError(t, err)
	})
}

func FuzzParseBundle(f *testing.F) {
	real, err := os.ReadFile(filepath.Join("testdata", "bundle.json"))
	require.NoError(f, err)
	f.Add(real)
	f.Add(real[:len(real)/2])
	f.Add([]byte(`{}`))
	f.Add([]byte(`{"mediaType":"application/vnd.dev.sigstore.bundle.v0.3+json","verificationMaterial":{},"dsseEnvelope":{}}`))
	f.Add([]byte(`{"mediaType":"application/vnd.dev.sigstore.bundle.v0.3+json","verificationMaterial":{"certificate":{"rawBytes":"AAAA"},"tlogEntries":[{}]},"dsseEnvelope":{"payload":"e30=","signatures":[{"sig":"AAAA"}]}}`))
	f.Add([]byte(`null`))
	f.Fuzz(func(t *testing.T, data []byte) {
		b, err := ParseBundle(data)
		if err != nil {
			require.Nil(t, b)
			return
		}
		require.LessOrEqual(t, len(data), MaxBundle)
		// A bundle that parses can be asked for its parts without a panic,
		// which is all Verify does before the cryptography.
		_, _ = b.VerificationContent()
		_, _ = b.SignatureContent()
		_, _ = b.TlogEntries()
		_, _ = b.Timestamps()
	})
}
