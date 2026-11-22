package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/AitorConS/jerboa/internal/api"
	"github.com/AitorConS/jerboa/internal/registry/attest"
)

const (
	pfRecipe = "sha256:e9339d1efd2e354aecf987335301f9a71050939483454451e0ab447d619854cb"
	pfDisk   = "sha256:43873eae427fc44abf68f91f61501edfd216ffc408eeffe3d2ad3c931108244e"
)

func TestDescribeSignerAndRun(t *testing.T) {
	for in, want := range map[string]string{
		"https://github.com/AitorConS/jerboa-images/.github/workflows/publish.yml@refs/heads/main": "AitorConS/jerboa-images (publish.yml on main)",
		"https://github.com/acme/web/.github/workflows/release.yaml@refs/tags/v1.2.0":              "acme/web (release.yaml at v1.2.0)",
		"https://github.com/acme/web/.github/workflows/ci.yml":                                     "acme/web (ci.yml)",
		"https://gitlab.com/acme/web//.gitlab-ci.yml@refs/heads/main":                              "https://gitlab.com/acme/web//.gitlab-ci.yml@refs/heads/main",
		"not a url at all": "not a url at all",
	} {
		assert.Equal(t, want, describeSigner(in), in)
	}
	for in, want := range map[string]string{
		"https://github.com/AitorConS/jerboa-images/actions/runs/42870044937/attempts/4": "run 42870044937 (attempt 4)",
		"https://github.com/acme/web/actions/runs/7/attempts/1":                          "run 7",
		"https://github.com/acme/web/actions/runs/7":                                     "run 7",
		"https://example.com/build/9":                                                    "https://example.com/build/9",
	} {
		assert.Equal(t, want, describeRun(in), in)
	}
}

func TestSignatureSummary(t *testing.T) {
	signer := "https://github.com/AitorConS/jerboa-images/.github/workflows/publish.yml@refs/heads/main"
	run := "https://github.com/AitorConS/jerboa-images/actions/runs/42870044937/attempts/4"
	results := []attest.Result{
		{PredicateType: "https://slsa.dev/provenance/v1", Signer: signer, Commit: "cd87de450c0626bbb4e5035cd6a964dd763f69be", Run: run},
		{PredicateType: "https://cyclonedx.org/bom", Signer: signer, Commit: "cd87de450c0626bbb4e5035cd6a964dd763f69be", Run: run},
	}
	head, detail := signatureSummary(results)
	assert.Equal(t, "AitorConS/jerboa-images (publish.yml on main)", head)
	assert.Equal(t, predicateName(results[0].PredicateType)+" and "+predicateName(results[1].PredicateType)+
		" · commit cd87de450c06 · run 42870044937 (attempt 4)", detail)
}

// pfRegistry serves a registry with one tag, acme/web:v1, naming pfRecipe,
// and no attestations.
func pfRegistry(t *testing.T) *lfRegistry {
	t.Helper()
	var reg *lfRegistry
	reg = lfNewRegistry(t, func(_ int64, w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/":
			lfDiscovery(w, reg.host)
		case "/tags/acme/web/v1", "/tags/acme/web/latest":
			_ = json.NewEncoder(w).Encode(map[string]any{"recipe": pfRecipe})
		default:
			http.Error(w, `{"error":"unexpected request"}`, http.StatusTeapot)
		}
	})
	return reg
}

func pfPulled(host string) api.ImagePullResult {
	return api.ImagePullResult{
		Image:  api.ImageManifestResult{Name: host + "/acme/web", Tag: "v1", Platform: "linux/arm64", DiskDigest: pfDisk},
		Recipe: pfRecipe, Blobs: 6, BlobsFetched: 6, Requests: 5, BytesFetched: 40 << 20,
	}
}

// A pull says what it did one step a line, and ends with the image and its digest.
func TestPullPrintsEachStep(t *testing.T) {
	reg := pfRegistry(t)
	lfRegistryEnv(t, reg.host)
	d := fdNew(t).on("Image.Pull", fdOK(pfPulled(reg.host)))
	res := fdRunHere(t, d.endpoint, "pull", reg.host+"/acme/web:v1")
	require.NoError(t, res.err)
	for _, want := range []string{
		"Pulling " + reg.host + "/acme/web:v1\n",
		"✓ Resolved to sha256:e9339d1efd2e\n",
		"✓ Not signed: pulled without a signature check\n",
		"✓ Downloaded 6 of 6 files (40.0M) in 5 requests, ",
		"Pulled " + reg.host + "/acme/web:v1 (linux/arm64)\nDigest: " + pfDisk + "\n",
	} {
		assert.Contains(t, res.stdout, want)
	}
	assert.True(t, strings.HasSuffix(res.stdout, "Digest: "+pfDisk+"\n"), "the digest is the last line: %q", res.stdout)
	assert.NotContains(t, res.stdout, "\r", "no spinner frames outside a terminal")

	// Pulled again, nothing is downloaded.
	cached := pfPulled(reg.host)
	cached.BlobsFetched, cached.Requests, cached.BytesFetched = 0, 1, 0
	d2 := fdNew(t).on("Image.Pull", fdOK(cached))
	res = fdRunHere(t, d2.endpoint, "pull", reg.host+"/acme/web:v1")
	require.NoError(t, res.err)
	assert.Contains(t, res.stdout, "✓ Up to date: all 6 files were already here\n")
}

// A run of an image the daemon does not store pulls it first, with the same
// steps as jerboa pull, then runs the name:tag the daemon stored.
func TestRunPullsAMissingImage(t *testing.T) {
	reg := pfRegistry(t)
	lfRegistryEnv(t, reg.host)
	d := fdNew(t).
		on("Image.Get", fdFail(api.CodeNotFound, "image store resolve x: x not found")).
		on("Image.Pull", fdOK(pfPulled(reg.host))).
		on("VM.Run", fdOK(api.VMInfo{ID: "vm-9", State: "running"}))
	res := fdRunHere(t, d.endpoint, "run", reg.host+"/acme/web:v1")
	require.NoError(t, res.err)
	assert.Equal(t, []string{"Image.Get", "Image.Pull", "VM.Run"}, d.methods())
	assert.Contains(t, res.stdout, "Pulling "+reg.host+"/acme/web:v1\n")
	assert.True(t, strings.HasSuffix(res.stdout, "vm-9\n"), "the VM id is still the last line: %q", res.stdout)
	var p api.RunParams
	d.params("VM.Run", 0, &p)
	assert.Equal(t, reg.host+"/acme/web:v1", p.Image)
}

func TestRunPullPolicy(t *testing.T) {
	t.Run("stored image is not pulled", func(t *testing.T) {
		d := fdNew(t).
			on("Image.Get", fdOK(api.ImageManifestResult{Name: "acme/web", Tag: "v1", DiskDigest: pfDisk})).
			on("VM.Run", fdOK(api.VMInfo{ID: "vm-1"}))
		res := fdRun(t, d.endpoint, "run", "acme/web:v1")
		require.NoError(t, res.err)
		assert.Equal(t, []string{"Image.Get", "VM.Run"}, d.methods())
		assert.Equal(t, "vm-1\n", res.stdout)
	})
	t.Run("never asks nothing", func(t *testing.T) {
		d := fdNew(t).on("VM.Run", fdOK(api.VMInfo{ID: "vm-2"}))
		res := fdRun(t, d.endpoint, "run", "--pull", "never", "acme/web:v1")
		require.NoError(t, res.err)
		assert.Equal(t, []string{"VM.Run"}, d.methods())
	})
	t.Run("a local name that is missing is not pulled", func(t *testing.T) {
		d := fdNew(t).on("Image.Get", fdFail(api.CodeNotFound, "image store resolve web:latest: web:latest not found"))
		res := fdRun(t, d.endpoint, "run", "web:latest")
		fdRequireCleanFailure(t, res, "web:latest is not stored here", "jerboa build")
		assert.Equal(t, []string{"Image.Get"}, d.methods())
	})
	t.Run("other failures end the run", func(t *testing.T) {
		d := fdNew(t).on("Image.Get", fdFail(-32000, "disk quota exceeded"))
		res := fdRun(t, d.endpoint, "run", "acme/web:v1")
		fdRequireCleanFailure(t, res, "disk quota exceeded")
		assert.Equal(t, []string{"Image.Get"}, d.methods())
	})
	t.Run("a bad policy is refused before the daemon", func(t *testing.T) {
		d := fdNew(t)
		res := fdRun(t, d.endpoint, "run", "--pull", "sometimes", "acme/web:v1")
		fdRequireCleanFailure(t, res, "--pull must be missing, always or never")
		assert.Empty(t, d.methods())
	})
}

// A wrong number of arguments says which ones arrived and the likely cause,
// in three lines at most.
func TestArgumentCountErrorsExplainThemselves(t *testing.T) {
	e := afIsolate(t)
	for _, tc := range []struct {
		args []string
		want []string
		not  []string
	}{
		{
			args: []string{"run", "jerboa/traefik", "--network", "app", "-p", "80:80", " "},
			want: []string{`accepts 1 arg(s), received 2: "jerboa/traefik", " "`, `argument 2 is only whitespace`, `remove the space after it`, "usage: jerboa run <image> [flags]"},
		},
		{
			args: []string{"run", "jerboa/traefik", ""},
			want: []string{`argument 2 is empty`},
		},
		{
			args: []string{"run", "jerboa/traefik", "8080:8080"},
			want: []string{`received 2: "jerboa/traefik", "8080:8080"`, "repeat the flag (-p 80:80 -p 443:443)"},
		},
		{
			args: []string{"rmi", "a", "b"},
			want: []string{"repeat the flag; quote"},
			not:  []string{"-p 80:80"},
		},
		{
			args: []string{"tag", "web"},
			want: []string{`accepts 2 arg(s), received 1: "web"`, "usage: jerboa tag <image> <target>"},
			not:  []string{"repeat the flag"},
		},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			res := e.run(tc.args...)
			afCleanError(t, res.err, tc.want...)
			for _, n := range tc.not {
				assert.NotContains(t, res.err.Error(), n)
			}
		})
	}
}
