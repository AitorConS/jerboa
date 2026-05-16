package registry_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AitorConS/unikernel-engine/internal/image"
	"github.com/AitorConS/unikernel-engine/internal/ociblob"
	"github.com/AitorConS/unikernel-engine/internal/ociregistry"
	"github.com/AitorConS/unikernel-engine/internal/registry"
	"github.com/stretchr/testify/require"
)

func TestClient_SetInsecureSkipVerify(t *testing.T) {
	c := registry.NewClient("https://example.com")
	c.SetInsecureSkipVerify(true)
}

func TestClient_SetCACertFile_InvalidPath(t *testing.T) {
	c := registry.NewClient("https://example.com")
	err := c.SetCACertFile("/nonexistent/ca.pem")
	require.Error(t, err)
	require.Contains(t, err.Error(), "read CA cert")
}

func TestClient_SetCACertFile_InvalidPEM(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "ca-*.pem")
	require.NoError(t, err)
	_, err = f.WriteString("not a valid PEM")
	require.NoError(t, err)
	require.NoError(t, f.Close())

	c := registry.NewClient("https://example.com")
	err = c.SetCACertFile(f.Name())
	require.Error(t, err)
	require.Contains(t, err.Error(), "parse CA cert PEM")
}

func TestClient_SetCACertFile_EmptyPath(t *testing.T) {
	c := registry.NewClient("https://example.com")
	err := c.SetCACertFile("")
	require.NoError(t, err)
}

func TestClient_EnsureTransport(t *testing.T) {
	c := registry.NewClient("https://example.com")
	c.SetInsecureSkipVerify(true)
	c.SetInsecureSkipVerify(false)
}

func TestClient_ListRepositories(t *testing.T) {
	store := makeStore(t)
	blobs, err := ociblob.NewStore(filepath.Join(t.TempDir(), "blobs"))
	require.NoError(t, err)
	ociStore, err := registry.NewOCIStore(filepath.Join(t.TempDir(), "oci"))
	require.NoError(t, err)
	h := registry.NewServer(store, registry.WithBlobStore(blobs), registry.WithOCIStore(ociStore)).Handler()
	srv := httptest.NewServer(h)
	defer srv.Close()

	client := registry.NewClient(srv.URL)
	repos, err := client.ListRepositories(context.Background())
	require.NoError(t, err)
	require.Empty(t, repos)

	disk := makeDiskFile(t)
	m := image.Manifest{
		SchemaVersion: image.SchemaVersion,
		Name:          "catalogapp",
		Tag:           "latest",
		Created:       time.Now().UTC(),
		Config:        image.Config{Memory: "256M", CPUs: 1},
		DiskDigest:    "sha256:aaa",
		DiskSize:      1024,
	}
	require.NoError(t, client.PushOCI(context.Background(), m, disk))

	repos, err = client.ListRepositories(context.Background())
	require.NoError(t, err)
	require.Contains(t, repos, "catalogapp")
}

func TestClient_ListRepositories_ErrorPath(t *testing.T) {
	c := registry.NewClient("http://127.0.0.1:1")
	_, err := c.ListRepositories(context.Background())
	require.Error(t, err)
}

func TestClient_ListRepositories_BadStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()
	c := registry.NewClient(srv.URL)
	_, err := c.ListRepositories(context.Background())
	require.Error(t, err)
	require.Contains(t, err.Error(), "403")
}

func TestClient_ListRepositories_BadDecode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("not json"))
	}))
	defer srv.Close()
	c := registry.NewClient(srv.URL)
	_, err := c.ListRepositories(context.Background())
	require.Error(t, err)
	require.Contains(t, err.Error(), "decode")
}

func TestClient_List_ErrorPaths(t *testing.T) {
	t.Run("network error", func(t *testing.T) {
		c := registry.NewClient("http://127.0.0.1:1")
		_, err := c.List(context.Background())
		require.Error(t, err)
	})

	t.Run("bad status", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer srv.Close()
		c := registry.NewClient(srv.URL)
		_, err := c.List(context.Background())
		require.Error(t, err)
		require.Contains(t, err.Error(), "500")
	})

	t.Run("bad decode", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("not json"))
		}))
		defer srv.Close()
		c := registry.NewClient(srv.URL)
		_, err := c.List(context.Background())
		require.Error(t, err)
		require.Contains(t, err.Error(), "decode")
	})
}

func TestClient_Push_NetworkError(t *testing.T) {
	c := registry.NewClient("http://127.0.0.1:1")
	m := image.Manifest{
		SchemaVersion: image.SchemaVersion, Name: "x", Tag: "1",
		Created: time.Now().UTC(), Config: image.Config{Memory: "256M", CPUs: 1},
		DiskDigest: "sha256:aaa", DiskSize: 1,
	}
	err := c.Push(context.Background(), m, makeDiskFile(t))
	require.Error(t, err)
}

func TestClient_Push_BadStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	srcStore := makeStore(t)
	disk := makeDiskFile(t)
	m := image.Manifest{
		SchemaVersion: image.SchemaVersion, Name: "x", Tag: "1",
		Created: time.Now().UTC(), Config: image.Config{Memory: "256M", CPUs: 1},
		DiskDigest: "sha256:aaa", DiskSize: 1,
	}
	require.NoError(t, srcStore.Put("x", "1", m, disk))
	pushed, _, _ := srcStore.Get("x:1")
	c := registry.NewClient(srv.URL)
	err := c.Push(context.Background(), pushed, disk)
	require.Error(t, err)
	require.Contains(t, err.Error(), "500")
}

func TestClient_Pull_NetworkError(t *testing.T) {
	c := registry.NewClient("http://127.0.0.1:1")
	dst := makeStore(t)
	_, err := c.Pull(context.Background(), "hello:latest", dst)
	require.Error(t, err)
}

func TestClient_Pull_BadManifestStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/disk") {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("disk"))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	c := registry.NewClient(srv.URL)
	dst := makeStore(t)
	_, err := c.Pull(context.Background(), "hello:latest", dst)
	require.Error(t, err)
	require.Contains(t, err.Error(), "404")
}

func TestClient_Pull_BadDiskStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/disk") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusOK)
		m := image.Manifest{
			SchemaVersion: image.SchemaVersion, Name: "hello", Tag: "latest",
			Created: time.Now().UTC(), Config: image.Config{Memory: "256M", CPUs: 1},
			DiskDigest: "sha256:aaa", DiskSize: 1,
		}
		data, _ := image.Marshal(m)
		_, _ = w.Write(data)
	}))
	defer srv.Close()
	c := registry.NewClient(srv.URL)
	dst := makeStore(t)
	_, err := c.Pull(context.Background(), "hello:latest", dst)
	require.Error(t, err)
	require.Contains(t, err.Error(), "404")
}

func TestClient_PullOCI_InvalidRef(t *testing.T) {
	c := registry.NewClient("http://localhost:1")
	dst := makeStore(t)
	_, err := c.PullOCI(context.Background(), "invalidref", dst)
	require.Error(t, err)
	require.Contains(t, err.Error(), "invalid ref")
}

func TestClient_PullOCI_NetworkError(t *testing.T) {
	c := registry.NewClient("http://127.0.0.1:1")
	dst := makeStore(t)
	_, err := c.PullOCI(context.Background(), "app:latest", dst)
	require.Error(t, err)
}

func TestClient_PushOCI_NetworkError(t *testing.T) {
	c := registry.NewClient("http://127.0.0.1:1")
	m := image.Manifest{
		SchemaVersion: image.SchemaVersion, Name: "x", Tag: "1",
		Created: time.Now().UTC(), Config: image.Config{Memory: "256M", CPUs: 1},
		DiskDigest: "sha256:aaa", DiskSize: 1,
	}
	err := c.PushOCI(context.Background(), m, makeDiskFile(t))
	require.Error(t, err)
}

func TestClient_PushOCI_BadUploadStart(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()
	c := registry.NewClient(srv.URL)
	m := image.Manifest{
		SchemaVersion: image.SchemaVersion, Name: "x", Tag: "1",
		Created: time.Now().UTC(), Config: image.Config{Memory: "256M", CPUs: 1},
		DiskDigest: "sha256:aaa", DiskSize: 1,
	}
	err := c.PushOCI(context.Background(), m, makeDiskFile(t))
	require.Error(t, err)
	require.Contains(t, err.Error(), "start upload")
}

func TestClient_PushOCI_MissingLocationHeader(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()
	c := registry.NewClient(srv.URL)
	m := image.Manifest{
		SchemaVersion: image.SchemaVersion, Name: "x", Tag: "1",
		Created: time.Now().UTC(), Config: image.Config{Memory: "256M", CPUs: 1},
		DiskDigest: "sha256:aaa", DiskSize: 1,
	}
	err := c.PushOCI(context.Background(), m, makeDiskFile(t))
	require.Error(t, err)
	require.Contains(t, err.Error(), "missing Location header")
}

func TestClient_PushOCI_BadUploadCompleteStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.Header().Set("Location", "/v2/x/blobs/uploads/abc")
			w.WriteHeader(http.StatusAccepted)
			return
		}
		if r.Method == http.MethodPut && strings.Contains(r.URL.Path, "blobs/uploads") {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()
	c := registry.NewClient(srv.URL)
	m := image.Manifest{
		SchemaVersion: image.SchemaVersion, Name: "x", Tag: "1",
		Created: time.Now().UTC(), Config: image.Config{Memory: "256M", CPUs: 1},
		DiskDigest: "sha256:aaa", DiskSize: 1,
	}
	err := c.PushOCI(context.Background(), m, makeDiskFile(t))
	require.Error(t, err)
	require.Contains(t, err.Error(), "complete upload")
}

func TestClient_PullOCI_BadManifestStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	c := registry.NewClient(srv.URL)
	dst := makeStore(t)
	_, err := c.PullOCI(context.Background(), "app:latest", dst)
	require.Error(t, err)
	require.Contains(t, err.Error(), "get manifest returned 404")
}

func TestClient_PullOCI_BadManifestJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "manifests") {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("not json"))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	c := registry.NewClient(srv.URL)
	dst := makeStore(t)
	_, err := c.PullOCI(context.Background(), "app:latest", dst)
	require.Error(t, err)
	require.Contains(t, err.Error(), "parse manifest")
}

func TestClient_PullOCI_ManifestNoLayers(t *testing.T) {
	m := ociregistry.Manifest{
		SchemaVersion: ociregistry.OCIManifestSchemaVersion,
		MediaType:     ociregistry.MediaTypeImageManifest,
		Config: ociregistry.Descriptor{
			MediaType: ociregistry.MediaTypeImageConfig,
			Digest:    "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			Size:      1,
		},
		Layers: []ociregistry.Descriptor{},
	}
	data, err := ociregistry.MarshalManifest(m)
	require.NoError(t, err)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "manifests") {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(data)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	c := registry.NewClient(srv.URL)
	dst := makeStore(t)
	_, err = c.PullOCI(context.Background(), "app:latest", dst)
	require.Error(t, err)
	require.Contains(t, err.Error(), "layers")
}

func TestClient_PullOCI_BlobDownloadBadStatus(t *testing.T) {
	m := ociregistry.Manifest{
		SchemaVersion: ociregistry.OCIManifestSchemaVersion,
		MediaType:     ociregistry.MediaTypeImageManifest,
		Config: ociregistry.Descriptor{
			MediaType: ociregistry.MediaTypeImageConfig,
			Digest:    "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			Size:      1,
		},
		Layers: []ociregistry.Descriptor{{
			MediaType: ociregistry.MediaTypeImageLayerTarGzip,
			Digest:    "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
			Size:      2,
		}},
	}
	data, err := ociregistry.MarshalManifest(m)
	require.NoError(t, err)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "manifests") {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(data)
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	c := registry.NewClient(srv.URL)
	dst := makeStore(t)
	_, err = c.PullOCI(context.Background(), "app:latest", dst)
	require.Error(t, err)
	require.Contains(t, err.Error(), "download blob")
}

func TestClient_PullOCI_BlobDownloadNetworkError(t *testing.T) {
	called := false
	m := ociregistry.Manifest{
		SchemaVersion: ociregistry.OCIManifestSchemaVersion,
		MediaType:     ociregistry.MediaTypeImageManifest,
		Config: ociregistry.Descriptor{
			MediaType: ociregistry.MediaTypeImageConfig,
			Digest:    "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			Size:      1,
		},
		Layers: []ociregistry.Descriptor{{
			MediaType: ociregistry.MediaTypeImageLayerTarGzip,
			Digest:    "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
			Size:      2,
		}},
	}
	data, err := ociregistry.MarshalManifest(m)
	require.NoError(t, err)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "manifests") && !called {
			called = true
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(data)
			return
		}
		hj, ok := w.(http.Hijacker)
		if !ok {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		conn, _, _ := hj.Hijack()
		_ = conn.Close()
	}))
	defer srv.Close()
	c := registry.NewClient(srv.URL)
	dst := makeStore(t)
	_, err = c.PullOCI(context.Background(), "app:latest", dst)
	require.Error(t, err)
}

func TestSplitRef(t *testing.T) {
	tests := []struct {
		input string
		name  string
		tag   string
	}{
		{"app:latest", "app", "latest"},
		{"team/app:v1", "team/app", "v1"},
		{"", "", ""},
		{"nocolon", "", ""},
		{":tag", "", ""},
		{"name:", "", ""},
	}

	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			n, tg := splitRefInline(tc.input)
			require.Equal(t, tc.name, n)
			require.Equal(t, tc.tag, tg)
		})
	}
}

func splitRefInline(ref string) (string, string) {
	idx := strings.LastIndex(ref, ":")
	if idx <= 0 || idx >= len(ref)-1 {
		return "", ""
	}
	return ref[:idx], ref[idx+1:]
}

func TestClient_SetCACertFile_ValidPEM(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "test"},
		NotBefore:    time.Now(),
		NotAfter:     time.Now().Add(time.Hour),
	}
	certDER, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})

	f, err := os.CreateTemp(t.TempDir(), "ca-*.pem")
	require.NoError(t, err)
	_, err = f.Write(certPEM)
	require.NoError(t, err)
	require.NoError(t, f.Close())

	c := registry.NewClient("https://example.com")
	err = c.SetCACertFile(f.Name())
	require.NoError(t, err)
}

func TestClient_ListRepositories_AuthEnabled(t *testing.T) {
	store := makeStore(t)
	h := registry.NewServer(store, registry.WithBearerToken("tok", "realm")).Handler()
	srv := httptest.NewServer(h)
	defer srv.Close()

	c := registry.NewClient(srv.URL)
	_, err := c.ListRepositories(context.Background())
	require.Error(t, err)

	c.SetToken("tok")
	repos, err := c.ListRepositories(context.Background())
	require.NoError(t, err)
	require.Empty(t, repos)
}

func TestClient_AuthOnLegacyEndpoints(t *testing.T) {
	store := makeStore(t)
	h := registry.NewServer(store, registry.WithBearerToken("tok", "realm")).Handler()
	srv := httptest.NewServer(h)
	defer srv.Close()

	c := registry.NewClient(srv.URL)
	_, err := c.List(context.Background())
	require.Error(t, err)

	c.SetToken("wrong")
	_, err = c.List(context.Background())
	require.Error(t, err)

	c.SetToken("tok")
	list, err := c.List(context.Background())
	require.NoError(t, err)
	require.Empty(t, list)
}

func TestBuildMultipart_MissingDisk(t *testing.T) {
	_, _, err := buildMultipartInline([]byte(`{"schemaVersion":1}`), "/nonexistent/disk.img")
	require.Error(t, err)
	require.Contains(t, err.Error(), "open disk")
}

func buildMultipartInline(_ []byte, diskPath string) (io.Reader, string, error) {
	f, err := os.Open(diskPath)
	if err != nil {
		return nil, "", fmt.Errorf("open disk %s: %w", diskPath, err)
	}
	_ = f.Close()
	return nil, "", nil
}

func TestClient_TLSIntegration(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test-registry"},
		NotBefore:             time.Now(),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
	}
	certDER, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})

	certFile := filepath.Join(t.TempDir(), "cert.pem")
	keyFile := filepath.Join(t.TempDir(), "key.pem")
	require.NoError(t, os.WriteFile(certFile, certPEM, 0o644))
	require.NoError(t, os.WriteFile(keyFile, keyPEM, 0o600))

	tlsCert, err := tls.X509KeyPair(certPEM, keyPEM)
	require.NoError(t, err)

	store := makeStore(t)
	h := registry.NewServer(store).Handler()
	srv := httptest.NewUnstartedServer(h)
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{tlsCert}}
	srv.StartTLS()
	defer srv.Close()

	c := registry.NewClient(srv.URL)
	err = c.SetCACertFile(certFile)
	require.NoError(t, err)

	list, err := c.List(context.Background())
	require.NoError(t, err)
	require.Empty(t, list)
}

func TestClient_TLSInsecureSkipVerify(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test-registry"},
		NotBefore:             time.Now(),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
	}
	certDER, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})

	tlsCert, err := tls.X509KeyPair(certPEM, keyPEM)
	require.NoError(t, err)

	store := makeStore(t)
	h := registry.NewServer(store).Handler()
	srv := httptest.NewUnstartedServer(h)
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{tlsCert}}
	srv.StartTLS()
	defer srv.Close()

	c := registry.NewClient(srv.URL)
	c.SetInsecureSkipVerify(true)

	list, err := c.List(context.Background())
	require.NoError(t, err)
	require.Empty(t, list)
}
