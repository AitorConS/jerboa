package registry_test

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/AitorConS/unikernel-engine/internal/image"
	"github.com/AitorConS/unikernel-engine/internal/ociblob"
	"github.com/AitorConS/unikernel-engine/internal/ociregistry"
	"github.com/AitorConS/unikernel-engine/internal/registry"
	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/require"
)

func startOCIServer(t *testing.T) (*httptest.Server, *image.Store) {
	t.Helper()
	store := makeStore(t)
	root := t.TempDir()
	blobs, err := ociblob.NewStore(filepath.Join(t.TempDir(), "blobs"))
	require.NoError(t, err)
	ociStore, err := registry.NewOCIStore(filepath.Join(root, "oci"))
	require.NoError(t, err)
	h := registry.NewServer(store, registry.WithBlobStore(blobs), registry.WithOCIStore(ociStore)).Handler()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv, store
}

func TestOCIBaseAndCatalog(t *testing.T) {
	srv, _ := startOCIServer(t)

	baseResp, err := http.Get(srv.URL + "/v2/")
	require.NoError(t, err)
	t.Cleanup(func() { _ = baseResp.Body.Close() })
	require.Equal(t, http.StatusOK, baseResp.StatusCode)

	catResp, err := http.Get(srv.URL + "/v2/_catalog")
	require.NoError(t, err)
	t.Cleanup(func() { _ = catResp.Body.Close() })
	require.Equal(t, http.StatusOK, catResp.StatusCode)
}

func TestOCIAuthRequiresBearerToken(t *testing.T) {
	store := makeStore(t)
	blobs, err := ociblob.NewStore(filepath.Join(t.TempDir(), "blobs"))
	require.NoError(t, err)
	ociStore, err := registry.NewOCIStore(filepath.Join(t.TempDir(), "oci"))
	require.NoError(t, err)

	h := registry.NewServer(
		store,
		registry.WithBlobStore(blobs),
		registry.WithOCIStore(ociStore),
		registry.WithBearerToken("secret-token", "uni-test"),
	).Handler()
	srv := httptest.NewServer(h)
	defer srv.Close()

	baseResp, err := http.Get(srv.URL + "/v2/")
	require.NoError(t, err)
	t.Cleanup(func() { _ = baseResp.Body.Close() })
	require.Equal(t, http.StatusUnauthorized, baseResp.StatusCode)
	require.Equal(t, `Bearer realm="uni-test"`, baseResp.Header.Get("WWW-Authenticate"))

	client := registry.NewClient(srv.URL)
	client.SetToken("secret-token")

	disk := makeDiskFile(t)
	m := image.Manifest{
		SchemaVersion: image.SchemaVersion,
		Name:          "secureapp",
		Tag:           "latest",
		Created:       time.Now().UTC(),
		Config:        image.Config{Memory: "256M", CPUs: 1},
		DiskDigest:    "sha256:abc123def456abc123def456abc123def456abc123def456abc123def456abc1",
		DiskSize:      1024,
	}
	require.NoError(t, client.PushOCI(context.Background(), m, disk))
}

func TestOCIAuthJWTScopes(t *testing.T) {
	store := makeStore(t)
	blobs, err := ociblob.NewStore(filepath.Join(t.TempDir(), "blobs"))
	require.NoError(t, err)
	ociStore, err := registry.NewOCIStore(filepath.Join(t.TempDir(), "oci"))
	require.NoError(t, err)

	h := registry.NewServer(
		store,
		registry.WithBlobStore(blobs),
		registry.WithOCIStore(ociStore),
		registry.WithJWTAuth("jwt-secret", "uni-test"),
		registry.WithJWTValidation("uni-issuer", "uni-audience"),
	).Handler()
	srv := httptest.NewServer(h)
	defer srv.Close()

	pullOnly := mustSignJWT(t, "jwt-secret", "repository:secureapp:pull", "uni-issuer", "uni-audience")
	pushOnly := mustSignJWT(t, "jwt-secret", "repository:secureapp:push", "uni-issuer", "uni-audience")
	wrongAud := mustSignJWT(t, "jwt-secret", "repository:secureapp:pull", "uni-issuer", "other-audience")

	client := registry.NewClient(srv.URL)
	client.SetToken(pushOnly)
	disk := makeDiskFile(t)
	m := image.Manifest{
		SchemaVersion: image.SchemaVersion,
		Name:          "secureapp",
		Tag:           "latest",
		Created:       time.Now().UTC(),
		Config:        image.Config{Memory: "256M", CPUs: 1},
		DiskDigest:    "sha256:abc123def456abc123def456abc123def456abc123def456abc123def456abc1",
		DiskSize:      1024,
	}
	require.NoError(t, client.PushOCI(context.Background(), m, disk))

	headReq, err := http.NewRequest(http.MethodHead, srv.URL+"/v2/secureapp/manifests/latest", nil)
	require.NoError(t, err)
	headReq.Header.Set("Authorization", "Bearer "+pushOnly)
	headResp, err := http.DefaultClient.Do(headReq)
	require.NoError(t, err)
	t.Cleanup(func() { _ = headResp.Body.Close() })
	require.Equal(t, http.StatusForbidden, headResp.StatusCode)

	headReq2, err := http.NewRequest(http.MethodHead, srv.URL+"/v2/secureapp/manifests/latest", nil)
	require.NoError(t, err)
	headReq2.Header.Set("Authorization", "Bearer "+pullOnly)
	headResp2, err := http.DefaultClient.Do(headReq2)
	require.NoError(t, err)
	t.Cleanup(func() { _ = headResp2.Body.Close() })
	require.Equal(t, http.StatusOK, headResp2.StatusCode)

	headReq3, err := http.NewRequest(http.MethodHead, srv.URL+"/v2/secureapp/manifests/latest", nil)
	require.NoError(t, err)
	headReq3.Header.Set("Authorization", "Bearer "+wrongAud)
	headResp3, err := http.DefaultClient.Do(headReq3)
	require.NoError(t, err)
	t.Cleanup(func() { _ = headResp3.Body.Close() })
	require.Equal(t, http.StatusUnauthorized, headResp3.StatusCode)
}

func mustSignJWT(t *testing.T, secret, scope, issuer, audience string) string {
	t.Helper()
	claims := jwt.MapClaims{
		"scope": scope,
		"exp":   time.Now().Add(1 * time.Hour).Unix(),
		"iss":   issuer,
		"aud":   audience,
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err := tok.SignedString([]byte(secret))
	require.NoError(t, err)
	return signed
}

func TestOCIUploadBlobRoundTrip(t *testing.T) {
	srv, _ := startOCIServer(t)
	client := registry.NewClient(srv.URL)

	disk := makeDiskFile(t)
	m := image.Manifest{
		SchemaVersion: image.SchemaVersion,
		Name:          "ociapp",
		Tag:           "latest",
		Created:       time.Now().UTC(),
		Config:        image.Config{Memory: "256M", CPUs: 1},
		DiskDigest:    "sha256:abc123def456abc123def456abc123def456abc123def456abc123def456abc1",
		DiskSize:      1024,
	}

	require.NoError(t, client.PushOCI(context.Background(), m, disk))

	resp, err := http.Get(srv.URL + "/v2/ociapp/manifests/latest")
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "application/vnd.oci.image.manifest.v1+json", resp.Header.Get("Content-Type"))
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	ociManifest, err := ociregistry.ParseManifest(body)
	require.NoError(t, err)
	require.NotEmpty(t, ociManifest.Layers)

	localStore := makeStore(t)
	pulled, err := client.PullOCI(context.Background(), "ociapp:latest", localStore)
	require.NoError(t, err)
	require.Equal(t, "ociapp", pulled.Name)
	require.Equal(t, "latest", pulled.Tag)

	headManifestReq, err := http.NewRequest(http.MethodHead, srv.URL+"/v2/ociapp/manifests/latest", nil)
	require.NoError(t, err)
	headManifestResp, err := http.DefaultClient.Do(headManifestReq)
	require.NoError(t, err)
	t.Cleanup(func() { _ = headManifestResp.Body.Close() })
	require.Equal(t, http.StatusOK, headManifestResp.StatusCode)
	require.Equal(t, "application/vnd.oci.image.manifest.v1+json", headManifestResp.Header.Get("Content-Type"))
	require.NotEmpty(t, headManifestResp.Header.Get("Docker-Content-Digest"))

	headBlobReq, err := http.NewRequest(http.MethodHead, srv.URL+"/v2/ociapp/blobs/"+ociManifest.Layers[0].Digest, nil)
	require.NoError(t, err)
	headBlobResp, err := http.DefaultClient.Do(headBlobReq)
	require.NoError(t, err)
	t.Cleanup(func() { _ = headBlobResp.Body.Close() })
	require.Equal(t, http.StatusOK, headBlobResp.StatusCode)
	require.Equal(t, ociManifest.Layers[0].Digest, headBlobResp.Header.Get("Docker-Content-Digest"))
}

func TestOCIUploadBlobRoundTrip_NestedRepository(t *testing.T) {
	srv, _ := startOCIServer(t)
	client := registry.NewClient(srv.URL)

	disk := makeDiskFile(t)
	m := image.Manifest{
		SchemaVersion: image.SchemaVersion,
		Name:          "team/ociapp",
		Tag:           "latest",
		Created:       time.Now().UTC(),
		Config:        image.Config{Memory: "256M", CPUs: 1},
		DiskDigest:    "sha256:abc123def456abc123def456abc123def456abc123def456abc123def456abc1",
		DiskSize:      1024,
	}

	require.NoError(t, client.PushOCI(context.Background(), m, disk))

	resp, err := http.Get(srv.URL + "/v2/team/ociapp/manifests/latest")
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })
	require.Equal(t, http.StatusOK, resp.StatusCode)

	localStore := makeStore(t)
	pulled, err := client.PullOCI(context.Background(), "team/ociapp:latest", localStore)
	require.NoError(t, err)
	require.Equal(t, "team/ociapp", pulled.Name)
	require.Equal(t, "latest", pulled.Tag)
}

func TestOCICompleteUploadDigestMismatch(t *testing.T) {
	srv, _ := startOCIServer(t)

	startResp, err := http.Post(srv.URL+"/v2/app/blobs/uploads/", "application/octet-stream", nil)
	require.NoError(t, err)
	loc := startResp.Header.Get("Location")
	require.NoError(t, startResp.Body.Close())
	require.Equal(t, http.StatusAccepted, startResp.StatusCode)

	req, err := http.NewRequest(http.MethodPut, srv.URL+loc+"?digest=sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", bytes.NewReader([]byte("abc")))
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestOCIPersistedManifestAcrossServerRestart(t *testing.T) {
	root := t.TempDir()
	store := makeStore(t)
	blobs, err := ociblob.NewStore(filepath.Join(root, "blobs"))
	require.NoError(t, err)
	ociStore, err := registry.NewOCIStore(filepath.Join(root, "oci"))
	require.NoError(t, err)

	srv1 := httptest.NewServer(registry.NewServer(store, registry.WithBlobStore(blobs), registry.WithOCIStore(ociStore)).Handler())
	client1 := registry.NewClient(srv1.URL)
	disk := makeDiskFile(t)
	m := image.Manifest{
		SchemaVersion: image.SchemaVersion,
		Name:          "persistapp",
		Tag:           "v1",
		Created:       time.Now().UTC(),
		Config:        image.Config{Memory: "256M", CPUs: 1},
		DiskDigest:    "sha256:abc123def456abc123def456abc123def456abc123def456abc123def456abc1",
		DiskSize:      1024,
	}
	require.NoError(t, client1.PushOCI(context.Background(), m, disk))
	srv1.Close()

	store2 := makeStore(t)
	blobs2, err := ociblob.NewStore(filepath.Join(root, "blobs"))
	require.NoError(t, err)
	ociStore2, err := registry.NewOCIStore(filepath.Join(root, "oci"))
	require.NoError(t, err)
	srv2 := httptest.NewServer(registry.NewServer(store2, registry.WithBlobStore(blobs2), registry.WithOCIStore(ociStore2)).Handler())
	defer srv2.Close()

	resp, err := http.Get(srv2.URL + "/v2/persistapp/manifests/v1")
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })
	require.Equal(t, http.StatusOK, resp.StatusCode)
}
