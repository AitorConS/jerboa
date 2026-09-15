package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/AitorConS/jerboa/internal/api"
	"github.com/AitorConS/jerboa/internal/config"
	pkg "github.com/AitorConS/jerboa/internal/package"
	"github.com/stretchr/testify/require"
)

func reviewPackage(t *testing.T, platform string) (*pkg.Store, pkg.Package) {
	t.Helper()
	s, err := pkg.NewStore(pkgStorePath())
	require.NoError(t, err)
	if platform != "" {
		s, err = s.ForPlatform(platform)
		require.NoError(t, err)
	}
	b := make([]byte, 64)
	copy(b, []byte{0x7f, 'E', 'L', 'F', 2, 1, 1})
	b[16] = 2
	b[18] = 183
	b[20] = 1
	b[52] = 64
	if platform == "linux/amd64" {
		b[18] = 62
	}
	binary := filepath.Join(t.TempDir(), "service")
	require.NoError(t, os.WriteFile(binary, b, 0755))
	require.NoError(t, s.Create("service", "1.2.3", binary, nil, "fixture", ""))
	data, err := os.ReadFile(filepath.Join(s.PackageDir("service", "1.2.3"), "meta.json"))
	require.NoError(t, err)
	var p pkg.Package
	require.NoError(t, json.Unmarshal(data, &p))
	return s, p
}

func TestPkgLoadRootInitialization(t *testing.T) {
	for _, mode := range []string{"flag", "environment", "config"} {
		t.Run(mode, func(t *testing.T) {
			withTempPkgStore(t)
			reviewPackage(t, "linux/arm64")
			t.Setenv("HOME", t.TempDir())
			t.Setenv("JERBOA_AUTH_TOKEN", "")
			t.Setenv("JERBOA_HOST", "")
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			require.NoError(t, err)
			defer listener.Close()
			endpoint := "tcp://" + listener.Addr().String()
			cfg := &config.Config{Daemon: config.DaemonConfig{Endpoint: endpoint, Token: "config-secret"}}
			args := []string{"pkg", "load", "service:1.2.3", "--source", "jerboa", "--platform", "linux/arm64", "-d"}
			if mode == "flag" {
				cfg.Daemon.Endpoint = "invalid-config"
				t.Setenv("JERBOA_HOST", "invalid-env")
				args = append([]string{"--host", endpoint}, args...)
			}
			if mode == "environment" {
				cfg.Daemon.Endpoint = "invalid-config"
				t.Setenv("JERBOA_HOST", endpoint)
			}
			require.NoError(t, config.Save(config.DefaultPath(), cfg))
			received := make(chan api.Request, 1)
			go func() {
				c, e := listener.Accept()
				if e != nil {
					return
				}
				defer c.Close()
				_ = c.SetDeadline(time.Now().Add(5 * time.Second))
				var req api.Request
				_ = json.NewDecoder(c).Decode(&req)
				received <- req
				_ = json.NewEncoder(c).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "error": map[string]any{"code": -32000, "message": "review-auth-reached"}})
			}()
			root := newRootCmd()
			root.SetOut(io.Discard)
			root.SetErr(io.Discard)
			root.SetArgs(args)
			err = root.Execute()
			require.ErrorContains(t, err, "review-auth-reached")
			select {
			case req := <-received:
				require.Equal(t, "Auth.Hello", req.Method)
				var auth api.AuthParams
				require.NoError(t, json.Unmarshal(req.Params, &auth))
				require.Equal(t, "config-secret", auth.Token)
			case <-time.After(5 * time.Second):
				t.Fatal("root never connected to selected endpoint")
			}
		})
	}
}

func TestPkgRootRejectsInvalidPlatform(t *testing.T) {
	root := newRootCmd()
	root.SetArgs([]string{"pkg", "list", "--platform", "darwin/arm64"})
	root.SetOut(io.Discard)
	require.ErrorContains(t, root.Execute(), "platform")
}

func TestPkgPushRootLocalVariants(t *testing.T) {
	for _, tc := range []struct {
		name           string
		variants       []string
		explicit, want string
		missing        bool
	}{
		{name: "unique", variants: []string{"linux/arm64"}, want: "linux/arm64"},
		{name: "unique-other", variants: []string{"linux/amd64"}, want: "linux/amd64"},
		{name: "coexist", variants: []string{"linux/amd64", "linux/arm64"}, want: "linux/arm64"},
		{name: "explicit", variants: []string{"linux/amd64", "linux/arm64"}, explicit: "linux/amd64", want: "linux/amd64"},
		{name: "legacy", variants: []string{""}, want: ""},
		{name: "modern-before-legacy", variants: []string{"", "linux/arm64"}, want: "linux/arm64"},
		{name: "legacy-explicit", variants: []string{""}, explicit: "linux/arm64", want: ""},
		{name: "missing", missing: true},
		{name: "missing-explicit", variants: []string{"linux/arm64"}, explicit: "linux/amd64", missing: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withTempPkgStore(t)
			t.Setenv("HOME", t.TempDir())
			t.Setenv("JERBOA_PACKAGE_ARCH", "arm64")
			t.Setenv("JERBOA_AUTH_TOKEN", "")
			var wantMeta pkg.Package
			var wantArchive []byte
			for _, platform := range tc.variants {
				s, p := reviewPackage(t, platform)
				if platform == tc.want {
					wantMeta = p
					var err error
					wantArchive, err = os.ReadFile(filepath.Join(s.PackageDir(p.Name, p.Version), "files.tar.gz"))
					require.NoError(t, err)
				}
			}
			type upload struct {
				method, path  string
				meta, archive []byte
				err           error
			}
			uploads := make(chan upload, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				u := upload{method: r.Method, path: r.URL.Path}
				mr, e := r.MultipartReader()
				u.err = e
				if e == nil {
					for {
						part, e := mr.NextPart()
						if errors.Is(e, io.EOF) {
							break
						}
						if e != nil {
							u.err = e
							break
						}
						data, e := io.ReadAll(part)
						if e != nil {
							u.err = e
						}
						switch part.FormName() {
						case "metadata":
							u.meta = data
						case "archive":
							u.archive = data
						}
					}
				}
				uploads <- u
				w.WriteHeader(http.StatusCreated)
			}))
			defer server.Close()
			args := []string{"pkg", "push", "service:1.2.3", server.URL}
			if tc.explicit != "" {
				args = append(args, "--platform", tc.explicit)
			}
			root := newRootCmd()
			root.SetArgs(args)
			root.SetOut(new(bytes.Buffer))
			root.SetErr(io.Discard)
			err := root.Execute()
			if tc.missing {
				require.ErrorContains(t, err, "not found locally")
				select {
				case <-uploads:
					t.Fatal("uploaded missing package")
				default:
				}
				return
			}
			require.NoError(t, err)
			select {
			case u := <-uploads:
				require.NoError(t, u.err)
				require.Equal(t, "POST", u.method)
				require.Equal(t, "/packages", u.path)
				var got pkg.Package
				require.NoError(t, json.Unmarshal(u.meta, &got))
				require.Equal(t, wantMeta, got)
				require.Equal(t, wantArchive, u.archive)
				require.NotContains(t, string(u.meta), pkgStorePath())
			default:
				t.Fatal("no multipart upload")
			}
		})
	}
}
