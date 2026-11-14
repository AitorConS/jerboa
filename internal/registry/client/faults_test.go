package client

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AitorConS/jerboa/internal/registry/credentials"
	"github.com/AitorConS/jerboa/internal/registry/pack"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests are about a registry, or the network in front of it, that
// fails: errors that pass and errors that stay, answers that are not what was
// asked for, answers too large to be one, and requests never answered. The
// client must retry what may pass, give up at once on what will not, never
// hand its caller bytes that do not match the digest they were asked by, and
// never wait past its context.

const hexA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func sha(data string) string {
	sum := sha256.Sum256([]byte(data))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// faulty serves handler and counts the requests it gets.
func faulty(t *testing.T, handler func(n int32, w http.ResponseWriter, r *http.Request)) (*Client, *atomic.Int32, string) {
	t.Helper()
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { handler(n.Add(1), w, r) }))
	t.Cleanup(srv.Close)
	c := New(strings.TrimPrefix(srv.URL, "http://"))
	c.Backoff = func(int) time.Duration { return time.Millisecond }
	return c, &n, srv.URL
}

func hangUp(w http.ResponseWriter) {
	if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
		_ = conn.Close()
	}
}

// Every request the client makes goes through the same retry loop. These are
// the calls of a pull and a push, each made against the same failures.
var calls = map[string]func(ctx context.Context, c *Client, base string) error{
	"Tag": func(ctx context.Context, c *Client, base string) error {
		_, err := c.Tag(ctx, base, "a", "b", "v1")
		return err
	},
	"Recipe": func(ctx context.Context, c *Client, base string) error {
		_, err := c.Recipe(ctx, base, "sha256:"+hexA)
		return err
	},
	"Index": func(ctx context.Context, c *Client, base string) error {
		_, err := c.Index(ctx, base, "sha256:"+hexA)
		return err
	},
	"Locations": func(ctx context.Context, c *Client, base string) error {
		_, err := c.Locations(ctx, base, "sha256:"+hexA)
		return err
	},
	"ReadRange": func(ctx context.Context, c *Client, base string) error {
		_, err := c.ReadRange(ctx, base, "sha256:"+hexA, 0, 4)
		return err
	},
	"Missing": func(ctx context.Context, c *Client, _ string) error {
		_, err := c.Missing(ctx, "t", "a", "b", []string{"sha256:" + hexA})
		return err
	},
	"PutPack": func(ctx context.Context, c *Client, _ string) error {
		return c.PutPack(ctx, "t", "a", "b", pack.Pack{Digest: "sha256:" + hexA, Data: []byte("x")})
	},
	"PutRecipe": func(ctx context.Context, c *Client, _ string) error {
		return c.PutRecipe(ctx, "t", "a", "b", "sha256:"+hexA, []byte("{}"))
	},
	"PutTag": func(ctx context.Context, c *Client, _ string) error {
		return c.PutTag(ctx, "t", "a", "b", "v1", "sha256:"+hexA)
	},
	"PushToken": func(ctx context.Context, c *Client, _ string) error {
		_, err := c.PushToken(ctx, "refresh", "a", "b")
		return err
	},
}

// An error that will not pass is reported after a bounded number of tries,
// with the registry's last word, not retried for ever.
func TestFaults_PersistentServerErrorsAreBounded(t *testing.T) {
	for name, call := range calls {
		for _, code := range []int{http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout, http.StatusTooManyRequests} {
			t.Run(fmt.Sprintf("%s/%d", name, code), func(t *testing.T) {
				c, n, base := faulty(t, func(_ int32, w http.ResponseWriter, _ *http.Request) { w.WriteHeader(code) })
				err := call(context.Background(), c, base)
				require.ErrorContains(t, err, fmt.Sprint(code))
				assert.Equal(t, int32(4), n.Load(), "tries")
			})
		}
		t.Run(name+"/connection dropped", func(t *testing.T) {
			c, n, base := faulty(t, func(_ int32, w http.ResponseWriter, _ *http.Request) { hangUp(w) })
			require.Error(t, call(context.Background(), c, base))
			assert.Equal(t, int32(4), n.Load(), "tries")
		})
	}
}

// A refusal is final: the same request with the same token gets the same
// answer, and a write that conflicts must not be sent four times.
func TestFaults_RefusalsAreNotRetried(t *testing.T) {
	for name, call := range calls {
		for _, code := range []int{http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusConflict, http.StatusRequestEntityTooLarge, http.StatusUnprocessableEntity} {
			t.Run(fmt.Sprintf("%s/%d", name, code), func(t *testing.T) {
				c, n, base := faulty(t, func(_ int32, w http.ResponseWriter, _ *http.Request) {
					w.WriteHeader(code)
					_, _ = w.Write([]byte(`{"error":{"code":"REFUSED","message":"no"}}`))
				})
				err := call(context.Background(), c, base)
				require.Error(t, err)
				assert.Equal(t, int32(1), n.Load(), "tries")
				if name != "ReadRange" && code != http.StatusNotFound {
					assert.True(t, IsStatus(err, code), "the caller cannot tell why it was refused: %v", err)
					assert.True(t, IsCode(err, "REFUSED"))
				}
			})
		}
	}
}

// A redirect that leads back to itself ends in an error, not a hang.
func TestFaults_RedirectLoopEnds(t *testing.T) {
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			c, n, base := faulty(t, func(_ int32, w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, r.URL.Path, http.StatusTemporaryRedirect)
			})
			require.Error(t, call(context.Background(), c, base))
			assert.Less(t, n.Load(), int32(100))
		})
	}
}

// The context ends while the client waits to retry, or while a request is
// never answered: the call returns the context's error at once.
func TestFaults_ContextEndsTheWait(t *testing.T) {
	for name, call := range calls {
		t.Run(name+"/between retries", func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			c, n, base := faulty(t, func(_ int32, w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusServiceUnavailable)
				cancel()
			})
			c.Backoff = func(int) time.Duration { return time.Hour }
			err := call(ctx, c, base)
			require.ErrorIs(t, err, context.Canceled)
			assert.Equal(t, int32(1), n.Load())
		})
		t.Run(name+"/never answered", func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			release := make(chan struct{})
			c, _, base := faulty(t, func(_ int32, _ http.ResponseWriter, r *http.Request) {
				cancel()
				select {
				case <-r.Context().Done():
				case <-release:
				}
			})
			// Registered after the server's own cleanup, so it runs first
			// and the server is not left waiting for this handler.
			t.Cleanup(func() { close(release) })
			done := make(chan error, 1)
			go func() { done <- call(ctx, c, base) }()
			select {
			case err := <-done:
				require.ErrorIs(t, err, context.Canceled)
			case <-time.After(30 * time.Second):
				t.Fatal("the call did not return after its context was canceled")
			}
		})
	}
}

// An error whose body is not the registry's error document (a proxy's page,
// half a document, nothing) is still an error with its status.
func TestFaults_ErrorBodiesThatAreNotErrors(t *testing.T) {
	bodies := map[string]string{
		"empty":         "",
		"html":          "<html>403 Forbidden</html>",
		"half document": `{"error":{"code":"DEN`,
		"wrong shape":   `{"error":"denied"}`,
		"no code":       `{"error":{"message":"denied"}}`,
		"null":          `null`,
		"array":         `[]`,
	}
	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			c, _, _ := faulty(t, func(_ int32, w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(body))
			})
			err := c.PutTag(context.Background(), "t", "a", "b", "v1", "sha256:"+hexA)
			var re *Error
			require.ErrorAs(t, err, &re)
			assert.Equal(t, http.StatusForbidden, re.Status)
			assert.Equal(t, "HTTP", re.Code)
			assert.Contains(t, err.Error(), "403")
		})
	}
}

// A success whose body is not what was asked for.
func TestFaults_AnswersThatAreNotAnswers(t *testing.T) {
	bodies := map[string]string{
		"empty":         "",
		"html":          "<html>Welcome to the hotel Wi-Fi</html>",
		"half document": `{"recipe":"sha256:aaaa`,
		"wrong type":    `{"recipe":42,"missing":"all","blobs":7,"token":[]}`,
		"array":         `[1,2,3]`,
	}
	jsonCalls := []string{"Tag", "Locations", "Missing", "PushToken"}
	for _, name := range jsonCalls {
		for bname, body := range bodies {
			t.Run(name+"/"+bname, func(t *testing.T) {
				c, n, base := faulty(t, func(_ int32, w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(body)) })
				err := calls[name](context.Background(), c, base)
				require.Error(t, err)
				assert.Equal(t, int32(1), n.Load(), "a malformed answer is not worth asking for again")
			})
		}
	}
}

// Documents named by digest are checked against it, whatever came back.
func TestFaults_ContentThatDoesNotMatchItsDigest(t *testing.T) {
	whole := `{"schemaVersion":1,"some":"document"}`
	digest := sha(whole)
	served := map[string]string{
		"another document": `{"schemaVersion":1,"some":"other document"}`,
		"truncated":        whole[:len(whole)/2],
		"empty":            "",
		"trailing newline": whole + "\n",
		"leading BOM":      "\xef\xbb\xbf" + whole,
		"error page":       "<html>not found</html>",
	}
	for name, body := range served {
		t.Run(name, func(t *testing.T) {
			c, _, base := faulty(t, func(_ int32, w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(body)) })
			ctx := context.Background()
			data, err := c.Recipe(ctx, base, digest)
			require.ErrorContains(t, err, "does not match its digest")
			assert.Nil(t, data, "the mismatched bytes were returned beside the error")
			data, err = c.Index(ctx, base, digest)
			require.ErrorContains(t, err, "does not match its digest")
			assert.Nil(t, data)
			data, err = c.Attestation(ctx, base, "a", "b", "sha256:"+hexA, digest)
			require.ErrorContains(t, err, "does not match its digest")
			assert.Nil(t, data)
		})
	}
	c, _, base := faulty(t, func(_ int32, w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(whole)) })
	data, err := c.Recipe(context.Background(), base, digest)
	require.NoError(t, err)
	assert.Equal(t, whole, string(data))
}

// A document larger than any document is cut off at the client's bound, so
// it fails its digest instead of filling memory.
func TestFaults_OversizedDocumentIsBounded(t *testing.T) {
	chunk := strings.Repeat("x", 1<<20)
	var sent atomic.Int64
	c, _, base := faulty(t, func(_ int32, w http.ResponseWriter, _ *http.Request) {
		for range 64 {
			n, err := w.Write([]byte(chunk))
			sent.Add(int64(n))
			if err != nil {
				return
			}
		}
	})
	data, err := c.Recipe(context.Background(), base, "sha256:"+hexA)
	require.ErrorContains(t, err, "does not match its digest")
	assert.Nil(t, data)
}

// A range read answers with exactly the bytes asked for or fails.
func TestFaults_ReadRangeAnswers(t *testing.T) {
	type answer func(w http.ResponseWriter)
	body := func(status int, b string) answer {
		return func(w http.ResponseWriter) { w.WriteHeader(status); _, _ = w.Write([]byte(b)) }
	}
	cases := map[string]struct {
		answer answer
		tries  int32
	}{
		"200 with the whole pack": {body(http.StatusOK, "abcdefgh"), 1},
		"200 with the right size": {body(http.StatusOK, "abcd"), 1},
		"206 with too much":       {body(http.StatusPartialContent, "abcde"), 1},
		"206 with too little":     {body(http.StatusPartialContent, "abc"), 4},
		"206 with nothing":        {body(http.StatusPartialContent, ""), 4},
		"416":                     {body(http.StatusRequestedRangeNotSatisfiable, ""), 1},
		"304":                     {body(http.StatusNotModified, ""), 1},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			cl, n, base := faulty(t, func(_ int32, w http.ResponseWriter, _ *http.Request) { c.answer(w) })
			data, err := cl.ReadRange(context.Background(), base, "sha256:"+hexA, 0, 4)
			require.Error(t, err)
			assert.Nil(t, data)
			assert.Equal(t, c.tries, n.Load())
		})
	}
}

// Discovery decides where everything else is read from. A registry that
// speaks another protocol, names no CDN, or fails to answer is an error, and
// a failed discovery is not remembered in place of a good one.
func TestFaults_Discovery(t *testing.T) {
	bad := map[string]string{
		"another protocol":  `{"api":2,"cdn":"http://cdn.example"}`,
		"no protocol":       `{"cdn":"http://cdn.example"}`,
		"no cdn":            `{"api":1}`,
		"empty cdn":         `{"api":1,"cdn":""}`,
		"cdn of wrong type": `{"api":1,"cdn":["http://cdn.example"]}`,
		"html":              `<html>It works!</html>`,
		"empty":             ``,
	}
	for name, body := range bad {
		t.Run(name, func(t *testing.T) {
			ForgetDiscoveries()
			healthy := atomic.Bool{}
			c, _, _ := faulty(t, func(_ int32, w http.ResponseWriter, _ *http.Request) {
				if healthy.Load() {
					_, _ = w.Write([]byte(`{"api":1,"cdn":"http://cdn.example/","features":["index"]}`))
					return
				}
				_, _ = w.Write([]byte(body))
			})
			_, err := c.Discover(context.Background())
			require.Error(t, err)
			_, err = c.Discover(context.Background())
			require.Error(t, err, "the second answer came from a cache of the first, bad one")

			healthy.Store(true)
			d, err := c.Discover(context.Background())
			require.NoError(t, err, "a failed discovery was remembered")
			assert.Equal(t, "http://cdn.example", d.CDN)
			assert.True(t, d.Supports(FeatureIndex))
			assert.False(t, d.Supports(FeatureAttestations))
		})
	}
	ForgetDiscoveries()
}

// A caller that changes the features of a discovery it was given must not
// change what the next caller is told the registry supports.
func TestFaults_DiscoveryCacheIsNotSharedMemory(t *testing.T) {
	ForgetDiscoveries()
	t.Cleanup(ForgetDiscoveries)
	c, _, _ := faulty(t, func(_ int32, w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"api":1,"cdn":"http://cdn.example","features":["index","attestations"]}`))
	})
	d, err := c.Discover(context.Background())
	require.NoError(t, err)
	d.Features[0], d.Features[1] = "x", "y"
	again, err := c.Discover(context.Background())
	require.NoError(t, err)
	assert.True(t, again.Supports(FeatureAttestations))
}

// The registry-run sign-in, with a registry that fails while it is polled.
func TestFaults_WaitLogin(t *testing.T) {
	login := DeviceLogin{LoginID: "jdl_x", UserCode: "ABCD", VerificationURI: "https://example", ExpiresIn: 900, Interval: 1}
	noSleep := func(context.Context, time.Duration) error { return nil }
	cases := map[string]struct {
		status   int
		body     string
		contains string
	}{
		"denied":               {http.StatusForbidden, `{"error":{"code":"DENIED","message":"denied"}}`, "denied in the browser"},
		"expired":              {http.StatusGone, ``, "expired"},
		"forgotten":            {http.StatusNotFound, ``, "no longer knows"},
		"server keeps failing": {http.StatusInternalServerError, ``, "500"},
		"garbage":              {http.StatusOK, `<html>`, "decode answer"},
		"approved, no token":   {http.StatusOK, `{"status":"approved"}`, "no token"},
		"empty object":         {http.StatusOK, `{}`, "no token"},
		"token of wrong type":  {http.StatusOK, `{"token":12}`, "decode answer"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			cl, _, _ := faulty(t, func(_ int32, w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(c.status)
				_, _ = w.Write([]byte(c.body))
			})
			cl.sleep = noSleep
			token, who, err := cl.WaitLogin(context.Background(), login)
			require.ErrorContains(t, err, c.contains)
			assert.Empty(t, token)
			assert.Empty(t, who)
		})
	}

	t.Run("canceled while waiting", func(t *testing.T) {
		cl, n, _ := faulty(t, func(_ int32, w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"status":"pending"}`)) })
		cl.sleep = func(context.Context, time.Duration) error { return context.Canceled }
		_, _, err := cl.WaitLogin(context.Background(), login)
		require.ErrorIs(t, err, context.Canceled)
		assert.Zero(t, n.Load())
	})
	t.Run("a blip while pending does not end the login", func(t *testing.T) {
		cl, _, _ := faulty(t, func(n int32, w http.ResponseWriter, _ *http.Request) {
			switch {
			case n == 1:
				_, _ = w.Write([]byte(`{"status":"pending"}`))
			case n < 4:
				w.WriteHeader(http.StatusBadGateway)
			default:
				_, _ = w.Write([]byte(`{"token":"jrt_x","login":"ana"}`))
			}
		})
		cl.sleep = noSleep
		token, who, err := cl.WaitLogin(context.Background(), login)
		require.NoError(t, err)
		assert.Equal(t, "jrt_x", token)
		assert.Equal(t, "ana", who)
	})
	t.Run("start answers without a code", func(t *testing.T) {
		for _, body := range []string{`{}`, `{"login_id":"x"}`, `{"login_id":"x","user_code":"y"}`, `[]`, ``} {
			cl, _, _ := faulty(t, func(_ int32, w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(body)) })
			_, err := cl.StartLogin(context.Background())
			require.Error(t, err, body)
		}
	})
}

// GitHub's device flow with GitHub failing.
func TestFaults_DeviceFlow(t *testing.T) {
	noSleep := func(context.Context, time.Duration) error { return nil }
	dc := DeviceCode{DeviceCode: "dev", UserCode: "ABCD", ExpiresIn: 900, Interval: 1}
	cases := map[string]struct {
		status   int
		body     string
		contains string
	}{
		"server error":   {http.StatusBadGateway, ``, "HTTP 502"},
		"rate limited":   {http.StatusTooManyRequests, ``, "HTTP 429"},
		"html":           {http.StatusOK, `<html>`, "decode"},
		"empty":          {http.StatusOK, ``, "decode"},
		"no token":       {http.StatusOK, `{}`, "no token"},
		"unknown error":  {http.StatusOK, `{"error":"unsupported_grant_type"}`, "unsupported_grant_type"},
		"incorrect code": {http.StatusOK, `{"error":"incorrect_device_code"}`, "incorrect_device_code"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(c.status)
				_, _ = w.Write([]byte(c.body))
			}))
			defer srv.Close()
			f := DeviceFlow{GitHub: srv.URL, sleep: noSleep}
			token, err := f.Wait(context.Background(), "client", dc)
			require.ErrorContains(t, err, c.contains)
			assert.Empty(t, token)
			if c.contains != "no token" && !strings.Contains(c.body, "error") {
				_, err = f.Start(context.Background(), "client")
				require.Error(t, err)
			}
		})
	}
	t.Run("start answers without a code", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"verification_uri":"https://github.com/login/device"}`))
		}))
		defer srv.Close()
		_, err := DeviceFlow{GitHub: srv.URL}.Start(context.Background(), "client")
		require.ErrorContains(t, err, "no device code")
	})
	t.Run("github unreachable", func(t *testing.T) {
		srv := httptest.NewServer(http.NotFoundHandler())
		url := srv.URL
		srv.Close()
		_, err := DeviceFlow{GitHub: url}.Start(context.Background(), "client")
		require.Error(t, err)
	})
	t.Run("canceled while waiting", func(t *testing.T) {
		f := DeviceFlow{GitHub: "http://127.0.0.1:1", sleep: func(context.Context, time.Duration) error { return context.DeadlineExceeded }}
		_, err := f.Wait(context.Background(), "client", dc)
		require.ErrorIs(t, err, context.DeadlineExceeded)
	})
}

// The GitHub Actions token endpoint failing, in a job that has no other
// credential: the push must fail with the cause, without contacting the
// registry with an empty token.
func TestFaults_ActionsTokenEndpoint(t *testing.T) {
	cases := map[string]struct {
		status int
		body   string
	}{
		"server error":        {http.StatusInternalServerError, ``},
		"forbidden":           {http.StatusForbidden, `{"message":"id-token permission missing"}`},
		"html":                {http.StatusOK, `<html>`},
		"empty":               {http.StatusOK, ``},
		"no value":            {http.StatusOK, `{}`},
		"empty value":         {http.StatusOK, `{"value":""}`},
		"value of wrong type": {http.StatusOK, `{"value":{"jwt":"x"}}`},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			actions := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(c.status)
				_, _ = w.Write([]byte(c.body))
			}))
			defer actions.Close()
			t.Setenv(credentials.EnvActionsTokenURL, actions.URL+"/token")
			t.Setenv(credentials.EnvActionsToken, "request-token")
			cl, n, _ := faulty(t, func(_ int32, w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"token":"push"}`)) })
			token, err := cl.PushToken(context.Background(), credentials.GitHubActions, "a", "b")
			require.ErrorContains(t, err, "github actions")
			assert.Empty(t, token)
			assert.Zero(t, n.Load(), "the registry was asked for a token without an identity")
		})
	}
	for _, url := range []string{"", "not a url", "http://actions.example/token", "ftp://127.0.0.1/token", "://"} {
		t.Setenv(credentials.EnvActionsTokenURL, url)
		t.Setenv(credentials.EnvActionsToken, "request-token")
		cl, n, _ := faulty(t, func(_ int32, w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"token":"push"}`)) })
		_, err := cl.PushToken(context.Background(), credentials.GitHubActions, "a", "b")
		require.Error(t, err, url)
		assert.Zero(t, n.Load())
	}
}

// Digests that are not digests never reach a URL.
func TestFaults_MalformedDigestsAreRefusedBeforeAnyRequest(t *testing.T) {
	c, n, base := faulty(t, func(_ int32, w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{}`)) })
	ctx := context.Background()
	for _, d := range []string{"", "sha256:", "sha256:../../x", "sha256:" + strings.ToUpper(hexA), "sha256:" + hexA + "0", "md5:" + hexA, hexA, "sha256:" + hexA[:63] + "/"} {
		require.Error(t, c.HasRecipe(ctx, base, "a", "b", d), d)
		_, err := c.Attestations(ctx, base, "a", "b", d)
		require.Error(t, err, d)
		_, err = c.Attestation(ctx, base, "a", "b", d, "sha256:"+hexA)
		require.Error(t, err, d)
		_, err = c.Attestation(ctx, base, "a", "b", "sha256:"+hexA, d)
		require.Error(t, err, d)
		_, err = c.PutAttestation(ctx, "t", "a", "b", d, []byte("{}"))
		require.Error(t, err, d)
		_, err = c.ReadRange(ctx, base, d, 0, 1)
		require.Error(t, err, d)
	}
	assert.Zero(t, n.Load())
}

// The pull counter is best effort: one request, no retries, and its failure
// is only an error value for the caller to ignore.
func TestFaults_CountPullNeverRetries(t *testing.T) {
	c, n, _ := faulty(t, func(_ int32, w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) })
	require.Error(t, c.CountPull(context.Background(), "a", "b"))
	assert.Equal(t, int32(1), n.Load())
}

func TestFaults_ErrorHelpersOnOtherErrors(t *testing.T) {
	plain := errors.New("dial tcp: connection refused")
	assert.False(t, IsStatus(plain, 0))
	assert.False(t, IsCode(plain, ""))
	assert.False(t, IsStatus(nil, http.StatusOK))
	wrapped := fmt.Errorf("pull: %w", &Error{Status: http.StatusForbidden, Code: "DENIED", Message: "no"})
	assert.True(t, IsStatus(wrapped, http.StatusForbidden))
	assert.True(t, IsCode(wrapped, "DENIED"))
}
