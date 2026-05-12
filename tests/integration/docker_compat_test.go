//go:build integration

package integration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
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
	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/require"
)

func newDockerCompatServer(t *testing.T) *httptest.Server {
	t.Helper()
	store, err := image.NewStore(filepath.Join(t.TempDir(), "images"))
	require.NoError(t, err)
	blobs, err := ociblob.NewStore(filepath.Join(t.TempDir(), "blobs"))
	require.NoError(t, err)
	ociStore, err := registry.NewOCIStore(filepath.Join(t.TempDir(), "oci"))
	require.NoError(t, err)
	srv := httptest.NewServer(registry.NewServer(store, registry.WithBlobStore(blobs), registry.WithOCIStore(ociStore)).Handler())
	t.Cleanup(srv.Close)
	return srv
}

func newDockerCompatServerWithAuth(t *testing.T, token string) *httptest.Server {
	t.Helper()
	store, err := image.NewStore(filepath.Join(t.TempDir(), "images"))
	require.NoError(t, err)
	blobs, err := ociblob.NewStore(filepath.Join(t.TempDir(), "blobs"))
	require.NoError(t, err)
	ociStore, err := registry.NewOCIStore(filepath.Join(t.TempDir(), "oci"))
	require.NoError(t, err)
	srv := httptest.NewServer(
		registry.NewServer(store,
			registry.WithBlobStore(blobs),
			registry.WithOCIStore(ociStore),
			registry.WithBearerToken(token, "uni-test"),
		).Handler(),
	)
	t.Cleanup(srv.Close)
	return srv
}

func newDockerCompatServerWithJWT(t *testing.T, secret, realm, issuer, audience string) *httptest.Server {
	t.Helper()
	store, err := image.NewStore(filepath.Join(t.TempDir(), "images"))
	require.NoError(t, err)
	blobs, err := ociblob.NewStore(filepath.Join(t.TempDir(), "blobs"))
	require.NoError(t, err)
	ociStore, err := registry.NewOCIStore(filepath.Join(t.TempDir(), "oci"))
	require.NoError(t, err)
	opts := []registry.Option{
		registry.WithBlobStore(blobs),
		registry.WithOCIStore(ociStore),
		registry.WithJWTAuth(secret, realm),
	}
	if issuer != "" || audience != "" {
		opts = append(opts, registry.WithJWTValidation(issuer, audience))
	}
	srv := httptest.NewServer(registry.NewServer(store, opts...).Handler())
	t.Cleanup(srv.Close)
	return srv
}

func makeDiskFile(t *testing.T) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "disk-*.img")
	require.NoError(t, err)
	_, err = f.WriteString("fake disk image content for docker compat test")
	require.NoError(t, err)
	require.NoError(t, f.Close())
	return f.Name()
}

func pushTestImage(t *testing.T, srv *httptest.Server, name, tag string) {
	t.Helper()
	client := registry.NewClient(srv.URL)
	disk := makeDiskFile(t)
	m := image.Manifest{
		SchemaVersion: image.SchemaVersion,
		Name:          name,
		Tag:           tag,
		Created:       time.Now().UTC(),
		Config:        image.Config{Memory: "256M", CPUs: 1},
		DiskDigest:    "sha256:abc123def456abc123def456abc123def456abc123def456abc123def456abc1",
		DiskSize:      1024,
	}
	require.NoError(t, client.PushOCI(context.Background(), m, disk))
}

func pushTestImageWithToken(t *testing.T, srv *httptest.Server, token, name, tag string) {
	t.Helper()
	client := registry.NewClient(srv.URL)
	client.SetToken(token)
	disk := makeDiskFile(t)
	m := image.Manifest{
		SchemaVersion: image.SchemaVersion,
		Name:          name,
		Tag:           tag,
		Created:       time.Now().UTC(),
		Config:        image.Config{Memory: "256M", CPUs: 1},
		DiskDigest:    "sha256:abc123def456abc123def456abc123def456abc123def456abc123def456abc1",
		DiskSize:      1024,
	}
	require.NoError(t, client.PushOCI(context.Background(), m, disk))
}

func TestDockerCompat_V2BaseNoAuth(t *testing.T) {
	srv := newDockerCompatServer(t)

	resp, err := http.Get(srv.URL + "/v2/")
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })

	require.Equal(t, http.StatusOK, resp.StatusCode)

	var body map[string]string
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
	require.Empty(t, body)
}

func TestDockerCompat_V2BaseWithAuth(t *testing.T) {
	srv := newDockerCompatServerWithAuth(t, "secret-token")

	resp, err := http.Get(srv.URL + "/v2/")
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })

	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	require.Contains(t, resp.Header.Get("WWW-Authenticate"), `Bearer realm="uni-test",service="uni-registry"`)
}

func TestDockerCompat_BlobUploadStart(t *testing.T) {
	srv := newDockerCompatServer(t)

	resp, err := http.Post(srv.URL+"/v2/testapp/blobs/uploads/", "application/octet-stream", nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })

	require.Equal(t, http.StatusAccepted, resp.StatusCode)
	loc := resp.Header.Get("Location")
	require.NotEmpty(t, loc, "Location header must be set for blob upload start")
	require.Contains(t, loc, "/v2/testapp/blobs/uploads/")
}

func TestDockerCompat_BlobUploadStartWithAuth(t *testing.T) {
	srv := newDockerCompatServerWithAuth(t, "secret-token")

	resp, err := http.Post(srv.URL+"/v2/testapp/blobs/uploads/", "application/octet-stream", nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })

	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)

	authedReq, err := http.NewRequest(http.MethodPost, srv.URL+"/v2/testapp/blobs/uploads/", nil)
	require.NoError(t, err)
	authedReq.Header.Set("Authorization", "Bearer secret-token")
	authedResp, err := http.DefaultClient.Do(authedReq)
	require.NoError(t, err)
	t.Cleanup(func() { _ = authedResp.Body.Close() })

	require.Equal(t, http.StatusAccepted, authedResp.StatusCode)
	require.NotEmpty(t, authedResp.Header.Get("Location"))
}

func TestDockerCompat_BlobUploadComplete(t *testing.T) {
	srv := newDockerCompatServer(t)

	data := []byte("hello docker compat blob")
	digest := sha256.Sum256(data)
	digestStr := "sha256:" + hex.EncodeToString(digest[:])

	startResp, err := http.Post(srv.URL+"/v2/testapp/blobs/uploads/", "application/octet-stream", nil)
	require.NoError(t, err)
	loc := startResp.Header.Get("Location")
	require.NoError(t, startResp.Body.Close())
	require.Equal(t, http.StatusAccepted, startResp.StatusCode)

	putURL := srv.URL + loc + "?digest=" + digestStr
	putReq, err := http.NewRequest(http.MethodPut, putURL, bytes.NewReader(data))
	require.NoError(t, err)
	putResp, err := http.DefaultClient.Do(putReq)
	require.NoError(t, err)
	t.Cleanup(func() { _ = putResp.Body.Close() })

	require.Equal(t, http.StatusCreated, putResp.StatusCode)
	require.Equal(t, digestStr, putResp.Header.Get("Docker-Content-Digest"))
	require.NotEmpty(t, putResp.Header.Get("Location"))
}

func TestDockerCompat_BlobHead(t *testing.T) {
	srv := newDockerCompatServer(t)

	data := []byte("hello docker compat head blob")
	digest := sha256.Sum256(data)
	digestStr := "sha256:" + hex.EncodeToString(digest[:])

	startResp, err := http.Post(srv.URL+"/v2/headapp/blobs/uploads/", "application/octet-stream", nil)
	require.NoError(t, err)
	loc := startResp.Header.Get("Location")
	require.NoError(t, startResp.Body.Close())

	putURL := srv.URL + loc + "?digest=" + digestStr
	putReq, err := http.NewRequest(http.MethodPut, putURL, bytes.NewReader(data))
	require.NoError(t, err)
	putResp, err := http.DefaultClient.Do(putReq)
	require.NoError(t, err)
	_ = putResp.Body.Close()
	require.Equal(t, http.StatusCreated, putResp.StatusCode)

	headReq, err := http.NewRequest(http.MethodHead, srv.URL+"/v2/headapp/blobs/"+digestStr, nil)
	require.NoError(t, err)
	headResp, err := http.DefaultClient.Do(headReq)
	require.NoError(t, err)
	t.Cleanup(func() { _ = headResp.Body.Close() })

	require.Equal(t, http.StatusOK, headResp.StatusCode)
	require.Equal(t, digestStr, headResp.Header.Get("Docker-Content-Digest"))
	require.Equal(t, "application/octet-stream", headResp.Header.Get("Content-Type"))
}

func TestDockerCompat_BlobHeadNotFound(t *testing.T) {
	srv := newDockerCompatServer(t)

	headReq, err := http.NewRequest(http.MethodHead, srv.URL+"/v2/fakeapp/blobs/sha256:0000000000000000000000000000000000000000000000000000000000000000", nil)
	require.NoError(t, err)
	headResp, err := http.DefaultClient.Do(headReq)
	require.NoError(t, err)
	t.Cleanup(func() { _ = headResp.Body.Close() })

	require.Equal(t, http.StatusNotFound, headResp.StatusCode)
}

func TestDockerCompat_Catalog(t *testing.T) {
	srv := newDockerCompatServer(t)

	pushTestImage(t, srv, "catalog-app", "latest")

	resp, err := http.Get(srv.URL + "/v2/_catalog")
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })

	require.Equal(t, http.StatusOK, resp.StatusCode)

	var cat struct {
		Repositories []string `json:"repositories"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&cat))
	require.Contains(t, cat.Repositories, "catalog-app")
}

func TestDockerCompat_CatalogEmpty(t *testing.T) {
	srv := newDockerCompatServer(t)

	resp, err := http.Get(srv.URL + "/v2/_catalog")
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })

	require.Equal(t, http.StatusOK, resp.StatusCode)

	var cat struct {
		Repositories []string `json:"repositories"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&cat))
	require.Empty(t, cat.Repositories)
}

func TestDockerCompat_ManifestPutAndGet(t *testing.T) {
	srv := newDockerCompatServer(t)

	configData := []byte(`{"memory":"256M","cpus":1}`)
	configDigestHash := sha256.Sum256(configData)
	configDigest := "sha256:" + hex.EncodeToString(configDigestHash[:])

	startResp, err := http.Post(srv.URL+"/v2/manifestapp/blobs/uploads/", "application/octet-stream", nil)
	require.NoError(t, err)
	loc := startResp.Header.Get("Location")
	require.NoError(t, startResp.Body.Close())

	putURL := srv.URL + loc + "?digest=" + configDigest
	putReq, err := http.NewRequest(http.MethodPut, putURL, bytes.NewReader(configData))
	require.NoError(t, err)
	putResp, err := http.DefaultClient.Do(putReq)
	require.NoError(t, err)
	_ = putResp.Body.Close()
	require.Equal(t, http.StatusCreated, putResp.StatusCode)

	diskData := []byte("fake disk for manifest test")
	diskDigestHash := sha256.Sum256(diskData)
	diskDigest := "sha256:" + hex.EncodeToString(diskDigestHash[:])

	startResp2, err := http.Post(srv.URL+"/v2/manifestapp/blobs/uploads/", "application/octet-stream", nil)
	require.NoError(t, err)
	loc2 := startResp2.Header.Get("Location")
	require.NoError(t, startResp2.Body.Close())

	putURL2 := srv.URL + loc2 + "?digest=" + diskDigest
	putReq2, err := http.NewRequest(http.MethodPut, putURL2, bytes.NewReader(diskData))
	require.NoError(t, err)
	putResp2, err := http.DefaultClient.Do(putReq2)
	require.NoError(t, err)
	_ = putResp2.Body.Close()
	require.Equal(t, http.StatusCreated, putResp2.StatusCode)

	manifest := ociregistry.Manifest{
		SchemaVersion: ociregistry.OCIManifestSchemaVersion,
		MediaType:     ociregistry.MediaTypeImageManifest,
		Config: ociregistry.Descriptor{
			MediaType: ociregistry.MediaTypeImageConfig,
			Digest:    configDigest,
			Size:      int64(len(configData)),
		},
		Layers: []ociregistry.Descriptor{
			{
				MediaType: ociregistry.MediaTypeImageLayerTarGzip,
				Digest:    diskDigest,
				Size:      int64(len(diskData)),
			},
		},
		Annotations: map[string]string{"org.opencontainers.image.ref.name": "latest"},
	}
	manifestJSON, err := ociregistry.MarshalManifest(manifest)
	require.NoError(t, err)

	putManifestReq, err := http.NewRequest(http.MethodPut, srv.URL+"/v2/manifestapp/manifests/latest", bytes.NewReader(manifestJSON))
	require.NoError(t, err)
	putManifestReq.Header.Set("Content-Type", ociregistry.MediaTypeImageManifest)
	putManifestResp, err := http.DefaultClient.Do(putManifestReq)
	require.NoError(t, err)
	t.Cleanup(func() { _ = putManifestResp.Body.Close() })

	require.Equal(t, http.StatusCreated, putManifestResp.StatusCode)
	require.NotEmpty(t, putManifestResp.Header.Get("Docker-Content-Digest"))

	getResp, err := http.Get(srv.URL + "/v2/manifestapp/manifests/latest")
	require.NoError(t, err)
	t.Cleanup(func() { _ = getResp.Body.Close() })

	require.Equal(t, http.StatusOK, getResp.StatusCode)
	require.Equal(t, ociregistry.MediaTypeImageManifest, getResp.Header.Get("Content-Type"))
	require.NotEmpty(t, getResp.Header.Get("Docker-Content-Digest"))

	body, err := io.ReadAll(getResp.Body)
	require.NoError(t, err)
	parsed, err := ociregistry.ParseManifest(body)
	require.NoError(t, err)
	require.Equal(t, configDigest, parsed.Config.Digest)
	require.Len(t, parsed.Layers, 1)
	require.Equal(t, diskDigest, parsed.Layers[0].Digest)
}

func TestDockerCompat_ManifestHead(t *testing.T) {
	srv := newDockerCompatServer(t)

	pushTestImage(t, srv, "head-manifest-app", "v1")

	headReq, err := http.NewRequest(http.MethodHead, srv.URL+"/v2/head-manifest-app/manifests/v1", nil)
	require.NoError(t, err)
	headResp, err := http.DefaultClient.Do(headReq)
	require.NoError(t, err)
	t.Cleanup(func() { _ = headResp.Body.Close() })

	require.Equal(t, http.StatusOK, headResp.StatusCode)
	require.Equal(t, ociregistry.MediaTypeImageManifest, headResp.Header.Get("Content-Type"))
	require.NotEmpty(t, headResp.Header.Get("Docker-Content-Digest"))
}

func TestDockerCompat_ManifestDelete(t *testing.T) {
	srv := newDockerCompatServer(t)

	pushTestImage(t, srv, "delete-app", "latest")

	delReq, err := http.NewRequest(http.MethodDelete, srv.URL+"/v2/delete-app/manifests/latest", nil)
	require.NoError(t, err)
	delResp, err := http.DefaultClient.Do(delReq)
	require.NoError(t, err)
	t.Cleanup(func() { _ = delResp.Body.Close() })

	require.Equal(t, http.StatusAccepted, delResp.StatusCode)

	getResp, err := http.Get(srv.URL + "/v2/delete-app/manifests/latest")
	require.NoError(t, err)
	t.Cleanup(func() { _ = getResp.Body.Close() })
	require.Equal(t, http.StatusNotFound, getResp.StatusCode)
}

func TestDockerCompat_BlobDownload(t *testing.T) {
	srv := newDockerCompatServer(t)

	blobData := []byte("docker compat blob download test")
	blobDigestHash := sha256.Sum256(blobData)
	blobDigest := "sha256:" + hex.EncodeToString(blobDigestHash[:])

	startResp, err := http.Post(srv.URL+"/v2/downloadapp/blobs/uploads/", "application/octet-stream", nil)
	require.NoError(t, err)
	loc := startResp.Header.Get("Location")
	require.NoError(t, startResp.Body.Close())

	putURL := srv.URL + loc + "?digest=" + blobDigest
	putReq, err := http.NewRequest(http.MethodPut, putURL, bytes.NewReader(blobData))
	require.NoError(t, err)
	putResp, err := http.DefaultClient.Do(putReq)
	require.NoError(t, err)
	_ = putResp.Body.Close()
	require.Equal(t, http.StatusCreated, putResp.StatusCode)

	getResp, err := http.Get(srv.URL + "/v2/downloadapp/blobs/" + blobDigest)
	require.NoError(t, err)
	t.Cleanup(func() { _ = getResp.Body.Close() })

	require.Equal(t, http.StatusOK, getResp.StatusCode)
	require.Equal(t, blobDigest, getResp.Header.Get("Docker-Content-Digest"))
	require.Equal(t, "application/octet-stream", getResp.Header.Get("Content-Type"))

	body, err := io.ReadAll(getResp.Body)
	require.NoError(t, err)
	require.Equal(t, blobData, body)
}

func TestDockerCompat_ChunkedUpload(t *testing.T) {
	srv := newDockerCompatServer(t)

	startResp, err := http.Post(srv.URL+"/v2/chunkapp/blobs/uploads/", "application/octet-stream", nil)
	require.NoError(t, err)
	loc := startResp.Header.Get("Location")
	require.NoError(t, startResp.Body.Close())
	require.Equal(t, http.StatusAccepted, startResp.StatusCode)

	chunk1 := []byte("chunk1_data_")
	patchReq, err := http.NewRequest(http.MethodPatch, srv.URL+loc, bytes.NewReader(chunk1))
	require.NoError(t, err)
	patchResp, err := http.DefaultClient.Do(patchReq)
	require.NoError(t, err)
	t.Cleanup(func() { _ = patchResp.Body.Close() })
	require.Equal(t, http.StatusAccepted, patchResp.StatusCode)
	require.NotEmpty(t, patchResp.Header.Get("Docker-Upload-UUID"))
	require.NotEmpty(t, patchResp.Header.Get("Range"))

	chunk2 := []byte("chunk2_data")
	allData := append(chunk1, chunk2...)
	fullDigestHash := sha256.Sum256(allData)
	fullDigest := "sha256:" + hex.EncodeToString(fullDigestHash[:])

	putReq, err := http.NewRequest(http.MethodPut, srv.URL+loc+"?digest="+fullDigest, bytes.NewReader(chunk2))
	require.NoError(t, err)
	putResp, err := http.DefaultClient.Do(putReq)
	require.NoError(t, err)
	t.Cleanup(func() { _ = putResp.Body.Close() })
	require.Equal(t, http.StatusCreated, putResp.StatusCode)
	require.Equal(t, fullDigest, putResp.Header.Get("Docker-Content-Digest"))

	headReq, err := http.NewRequest(http.MethodHead, srv.URL+"/v2/chunkapp/blobs/"+fullDigest, nil)
	require.NoError(t, err)
	headResp, err := http.DefaultClient.Do(headReq)
	require.NoError(t, err)
	t.Cleanup(func() { _ = headResp.Body.Close() })
	require.Equal(t, http.StatusOK, headResp.StatusCode)
}

func TestDockerCompat_AuthChallengeWithScope(t *testing.T) {
	srv := newDockerCompatServerWithAuth(t, "test-token")

	resp, err := http.Get(srv.URL + "/v2/")
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)

	wwwAuth := resp.Header.Get("WWW-Authenticate")
	require.Contains(t, wwwAuth, `Bearer realm="uni-test"`)
	require.Contains(t, wwwAuth, `service="uni-registry"`)
	require.NotContains(t, wwwAuth, "scope=")

	manifestResp, err := http.Get(srv.URL + "/v2/myapp/manifests/latest")
	require.NoError(t, err)
	t.Cleanup(func() { _ = manifestResp.Body.Close() })
	require.Equal(t, http.StatusUnauthorized, manifestResp.StatusCode)

	wwwAuth2 := manifestResp.Header.Get("WWW-Authenticate")
	require.Contains(t, wwwAuth2, `Bearer realm="uni-test"`)
	require.Contains(t, wwwAuth2, `service="uni-registry"`)
	require.Contains(t, wwwAuth2, `scope="repository:myapp:pull"`)
}

func TestDockerCompat_AuthChallengeForPush(t *testing.T) {
	srv := newDockerCompatServerWithAuth(t, "push-token")

	putReq, err := http.NewRequest(http.MethodPut, srv.URL+"/v2/pushapp/manifests/latest", bytes.NewReader([]byte("{}")))
	require.NoError(t, err)
	putResp, err := http.DefaultClient.Do(putReq)
	require.NoError(t, err)
	t.Cleanup(func() { _ = putResp.Body.Close() })
	require.Equal(t, http.StatusUnauthorized, putResp.StatusCode)

	wwwAuth := putResp.Header.Get("WWW-Authenticate")
	require.Contains(t, wwwAuth, `scope="repository:pushapp:push"`)
}

func TestDockerCompat_JWTAuthChallengedFlow(t *testing.T) {
	srv := newDockerCompatServerWithJWT(t, "jwt-secret", "uni-test", "uni-issuer", "uni-audience")

	resp, err := http.Get(srv.URL + "/v2/")
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	require.Contains(t, resp.Header.Get("WWW-Authenticate"), `Bearer realm="uni-test",service="uni-registry"`)

	token := mustSignJWTCompat(t, "jwt-secret", "repository:jwtapp:pull,push", "uni-issuer", "uni-audience")
	client := registry.NewClient(srv.URL)
	client.SetToken(token)
	disk := makeDiskFile(t)
	m := image.Manifest{
		SchemaVersion: image.SchemaVersion,
		Name:          "jwtapp",
		Tag:           "latest",
		Created:       time.Now().UTC(),
		Config:        image.Config{Memory: "256M", CPUs: 1},
		DiskDigest:    "sha256:abc123def456abc123def456abc123def456abc123def456abc123def456abc1",
		DiskSize:      1024,
	}
	require.NoError(t, client.PushOCI(context.Background(), m, disk))

	getReq, err := http.NewRequest(http.MethodGet, srv.URL+"/v2/jwtapp/manifests/latest", nil)
	require.NoError(t, err)
	getReq.Header.Set("Authorization", "Bearer "+token)
	getResp, err := http.DefaultClient.Do(getReq)
	require.NoError(t, err)
	t.Cleanup(func() { _ = getResp.Body.Close() })
	require.Equal(t, http.StatusOK, getResp.StatusCode)
	require.Equal(t, ociregistry.MediaTypeImageManifest, getResp.Header.Get("Content-Type"))
}

func TestDockerCompat_JWTAuthScopeDenied(t *testing.T) {
	srv := newDockerCompatServerWithJWT(t, "jwt-secret2", "uni-test", "", "")

	pullToken := mustSignJWTCompat(t, "jwt-secret2", "repository:deniedapp:pull", "", "")
	pushReq, err := http.NewRequest(http.MethodPut, srv.URL+"/v2/deniedapp/manifests/latest", bytes.NewReader([]byte("{}")))
	require.NoError(t, err)
	pushReq.Header.Set("Authorization", "Bearer "+pullToken)
	pushResp, err := http.DefaultClient.Do(pushReq)
	require.NoError(t, err)
	t.Cleanup(func() { _ = pushResp.Body.Close() })

	require.Equal(t, http.StatusForbidden, pushResp.StatusCode)
}

func TestDockerCompat_NestedRepository(t *testing.T) {
	srv := newDockerCompatServer(t)

	pushTestImage(t, srv, "org/nested-app", "v2.1")

	getResp, err := http.Get(srv.URL + "/v2/org/nested-app/manifests/v2.1")
	require.NoError(t, err)
	t.Cleanup(func() { _ = getResp.Body.Close() })
	require.Equal(t, http.StatusOK, getResp.StatusCode)
	require.Equal(t, ociregistry.MediaTypeImageManifest, getResp.Header.Get("Content-Type"))

	catResp, err := http.Get(srv.URL + "/v2/_catalog")
	require.NoError(t, err)
	t.Cleanup(func() { _ = catResp.Body.Close() })
	require.Equal(t, http.StatusOK, catResp.StatusCode)

	var cat struct {
		Repositories []string `json:"repositories"`
	}
	require.NoError(t, json.NewDecoder(catResp.Body).Decode(&cat))
	require.Contains(t, cat.Repositories, "org/nested-app")
}

func TestDockerCompat_ManifestGetByDigest(t *testing.T) {
	srv := newDockerCompatServer(t)

	pushTestImage(t, srv, "digest-app", "v3")

	getResp, err := http.Get(srv.URL + "/v2/digest-app/manifests/v3")
	require.NoError(t, err)
	t.Cleanup(func() { _ = getResp.Body.Close() })
	require.Equal(t, http.StatusOK, getResp.StatusCode)

	digest := getResp.Header.Get("Docker-Content-Digest")
	require.NotEmpty(t, digest)
	require.True(t, strings.HasPrefix(digest, "sha256:"))

	digestResp, err := http.Get(srv.URL + "/v2/digest-app/manifests/" + digest)
	require.NoError(t, err)
	t.Cleanup(func() { _ = digestResp.Body.Close() })
	require.Equal(t, http.StatusOK, digestResp.StatusCode)
	require.Equal(t, ociregistry.MediaTypeImageManifest, getResp.Header.Get("Content-Type"))
}

func TestDockerCompat_ManifestNotFound(t *testing.T) {
	srv := newDockerCompatServer(t)

	resp, err := http.Get(srv.URL + "/v2/nonexistent/manifests/latest")
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestDockerCompat_BlobDelete(t *testing.T) {
	srv := newDockerCompatServer(t)

	data := []byte("blob to delete")
	digestHash := sha256.Sum256(data)
	digest := "sha256:" + hex.EncodeToString(digestHash[:])

	startResp, err := http.Post(srv.URL+"/v2/delblob/blobs/uploads/", "application/octet-stream", nil)
	require.NoError(t, err)
	loc := startResp.Header.Get("Location")
	require.NoError(t, startResp.Body.Close())

	putReq, err := http.NewRequest(http.MethodPut, srv.URL+loc+"?digest="+digest, bytes.NewReader(data))
	require.NoError(t, err)
	putResp, err := http.DefaultClient.Do(putReq)
	require.NoError(t, err)
	_ = putResp.Body.Close()
	require.Equal(t, http.StatusCreated, putResp.StatusCode)

	headReq, err := http.NewRequest(http.MethodHead, srv.URL+"/v2/delblob/blobs/"+digest, nil)
	require.NoError(t, err)
	headResp, err := http.DefaultClient.Do(headReq)
	require.NoError(t, err)
	_ = headResp.Body.Close()
	require.Equal(t, http.StatusOK, headResp.StatusCode)

	delReq, err := http.NewRequest(http.MethodDelete, srv.URL+"/v2/delblob/blobs/"+digest, nil)
	require.NoError(t, err)
	delResp, err := http.DefaultClient.Do(delReq)
	require.NoError(t, err)
	_ = delResp.Body.Close()
	require.Equal(t, http.StatusAccepted, delResp.StatusCode)
}

func TestDockerCompat_BlobUploadDigestMismatch(t *testing.T) {
	srv := newDockerCompatServer(t)

	startResp, err := http.Post(srv.URL+"/v2/mismatch/blobs/uploads/", "application/octet-stream", nil)
	require.NoError(t, err)
	loc := startResp.Header.Get("Location")
	require.NoError(t, startResp.Body.Close())

	wrongDigest := "sha256:0000000000000000000000000000000000000000000000000000000000000000"
	putReq, err := http.NewRequest(http.MethodPut, srv.URL+loc+"?digest="+wrongDigest, bytes.NewReader([]byte("actual data")))
	require.NoError(t, err)
	putResp, err := http.DefaultClient.Do(putReq)
	require.NoError(t, err)
	t.Cleanup(func() { _ = putResp.Body.Close() })
	require.Equal(t, http.StatusBadRequest, putResp.StatusCode)
}

func TestDockerCompat_AuthenticatedPullWithBearerToken(t *testing.T) {
	srv := newDockerCompatServerWithAuth(t, "pull-token")

	pushTestImageWithToken(t, srv, "pull-token", "pull-secure-app", "latest")

	resp, err := http.Get(srv.URL + "/v2/pull-secure-app/manifests/latest")
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)

	authedReq, err := http.NewRequest(http.MethodGet, srv.URL+"/v2/pull-secure-app/manifests/latest", nil)
	require.NoError(t, err)
	authedReq.Header.Set("Authorization", "Bearer pull-token")
	authedResp, err := http.DefaultClient.Do(authedReq)
	require.NoError(t, err)
	t.Cleanup(func() { _ = authedResp.Body.Close() })
	require.Equal(t, http.StatusOK, authedResp.StatusCode)
	require.Equal(t, ociregistry.MediaTypeImageManifest, authedResp.Header.Get("Content-Type"))
}

func TestDockerCompat_AuthenticatedBlobHead(t *testing.T) {
	srv := newDockerCompatServerWithAuth(t, "head-token")

	pushTestImageWithToken(t, srv, "head-token", "auth-head-app", "v1")

	data := []byte("authenticated head test")
	digestHash := sha256.Sum256(data)
	digest := "sha256:" + hex.EncodeToString(digestHash[:])

	startReq, err := http.NewRequest(http.MethodPost, srv.URL+"/v2/auth-head-app/blobs/uploads/", nil)
	require.NoError(t, err)
	startReq.Header.Set("Authorization", "Bearer head-token")
	startResp, err := http.DefaultClient.Do(startReq)
	require.NoError(t, err)
	require.Equal(t, http.StatusAccepted, startResp.StatusCode)
	loc := startResp.Header.Get("Location")
	require.NoError(t, startResp.Body.Close())

	putURL := srv.URL + loc + "?digest=" + digest
	putReq, err := http.NewRequest(http.MethodPut, putURL, bytes.NewReader(data))
	require.NoError(t, err)
	putReq.Header.Set("Authorization", "Bearer head-token")
	putResp, err := http.DefaultClient.Do(putReq)
	require.NoError(t, err)
	_ = putResp.Body.Close()

	headReq, err := http.NewRequest(http.MethodHead, srv.URL+"/v2/auth-head-app/blobs/"+digest, nil)
	require.NoError(t, err)
	headReq.Header.Set("Authorization", "Bearer head-token")
	headResp, err := http.DefaultClient.Do(headReq)
	require.NoError(t, err)
	t.Cleanup(func() { _ = headResp.Body.Close() })
	require.Equal(t, http.StatusOK, headResp.StatusCode)
	require.Equal(t, digest, headResp.Header.Get("Docker-Content-Digest"))
}

func TestDockerCompat_FullPushPullRoundTrip(t *testing.T) {
	srv := newDockerCompatServer(t)

	client := registry.NewClient(srv.URL)
	disk := makeDiskFile(t)
	m := image.Manifest{
		SchemaVersion: image.SchemaVersion,
		Name:          "roundtrip-app",
		Tag:           "v1",
		Created:       time.Now().UTC(),
		Config:        image.Config{Memory: "512M", CPUs: 2},
		DiskDigest:    "sha256:abc123def456abc123def456abc123def456abc123def456abc123def456abc1",
		DiskSize:      1024,
	}
	require.NoError(t, client.PushOCI(context.Background(), m, disk))

	localStore, err := image.NewStore(filepath.Join(t.TempDir(), "pull-store"))
	require.NoError(t, err)
	pulled, err := client.PullOCI(context.Background(), "roundtrip-app:v1", localStore)
	require.NoError(t, err)
	require.Equal(t, "roundtrip-app", pulled.Name)
	require.Equal(t, "v1", pulled.Tag)

	catResp, err := http.Get(srv.URL + "/v2/_catalog")
	require.NoError(t, err)
	t.Cleanup(func() { _ = catResp.Body.Close() })
	var cat struct {
		Repositories []string `json:"repositories"`
	}
	require.NoError(t, json.NewDecoder(catResp.Body).Decode(&cat))
	require.Contains(t, cat.Repositories, "roundtrip-app")
}

func TestDockerCompat_AuthChallengeUnscopedForV2(t *testing.T) {
	srv := newDockerCompatServerWithAuth(t, "scope-token")

	resp, err := http.Get(srv.URL + "/v2/")
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)

	wwwAuth := resp.Header.Get("WWW-Authenticate")
	require.Contains(t, wwwAuth, fmt.Sprintf(`Bearer realm="uni-test",service="uni-registry"`))
	require.NotContains(t, wwwAuth, "scope=",
		"unauthenticated GET /v2/ challenge must not include repository scope")

	catResp, err := http.Get(srv.URL + "/v2/_catalog")
	require.NoError(t, err)
	t.Cleanup(func() { _ = catResp.Body.Close() })
	require.Equal(t, http.StatusUnauthorized, catResp.StatusCode)
	catWWWAuth := catResp.Header.Get("WWW-Authenticate")
	require.NotContains(t, catWWWAuth, "scope=",
		"catalog endpoint challenge must not include repository scope")
}

func mustSignJWTCompat(t *testing.T, secret, scope, issuer, audience string) string {
	t.Helper()
	claims := jwt.MapClaims{
		"scope": scope,
		"exp":   time.Now().Add(1 * time.Hour).Unix(),
	}
	if issuer != "" {
		claims["iss"] = issuer
	}
	if audience != "" {
		claims["aud"] = audience
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err := tok.SignedString([]byte(secret))
	require.NoError(t, err)
	return signed
}
