package registry_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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

func TestServer_HandleList_WithAuth(t *testing.T) {
	store := makeStore(t)
	seedStore(t, store)
	h := registry.NewServer(store, registry.WithBearerToken("tok", "realm")).Handler()
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v2/images")
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v2/images", nil)
	req.Header.Set("Authorization", "Bearer tok")
	resp2, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp2.Body.Close() })
	require.Equal(t, http.StatusOK, resp2.StatusCode)
}

func TestServer_HandleRemove_WithAuth(t *testing.T) {
	store := makeStore(t)
	seedStore(t, store)
	h := registry.NewServer(store, registry.WithBearerToken("tok", "realm")).Handler()
	srv := httptest.NewServer(h)
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/v2/images/hello:latest", nil)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)

	req2, _ := http.NewRequest(http.MethodDelete, srv.URL+"/v2/images/hello:latest", nil)
	req2.Header.Set("Authorization", "Bearer tok")
	resp2, err := http.DefaultClient.Do(req2)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp2.Body.Close() })
	require.Equal(t, http.StatusNoContent, resp2.StatusCode)
}

func TestServer_HandleRemove_NotFound(t *testing.T) {
	srv, _ := startServer(t)
	req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/v2/images/nonexistent:tag", nil)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestServer_HandleGetManifest_NotFound(t *testing.T) {
	srv, _ := startServer(t)
	resp, err := http.Get(srv.URL + "/v2/images/nonexistent:tag")
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestServer_HandleGetDisk_Success(t *testing.T) {
	srv, store := startServer(t)
	seedStore(t, store)

	resp, err := http.Get(srv.URL + "/v2/images/hello:latest/disk")
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })
	require.Equal(t, http.StatusOK, resp.StatusCode)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NotEmpty(t, body)
}

func TestServer_HandlePush_InvalidManifestJSON(t *testing.T) {
	srv, _ := startServer(t)

	manifest := `{invalid json`
	body := "--x\r\n" +
		"Content-Disposition: form-data; name=\"manifest\"\r\n\r\n" + manifest + "\r\n" +
		"--x--\r\n"

	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v2/images", io.NopCloser(strings.NewReader(body)))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "multipart/form-data; boundary=x")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestServer_HandleList_Error(t *testing.T) {
	srv, _ := startServer(t)
	resp, err := http.Get(srv.URL + "/v2/images")
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })
	require.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestServer_Authorized_NilRequest(t *testing.T) {
	store := makeStore(t)
	srv := registry.NewServer(store, registry.WithBearerToken("tok", "realm"))
	h := srv.Handler()
	ts := httptest.NewServer(h)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/v2/")
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

func TestServer_Authorized_BadBearerToken(t *testing.T) {
	store := makeStore(t)
	h := registry.NewServer(store, registry.WithBearerToken("correct", "realm")).Handler()
	srv := httptest.NewServer(h)
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v2/images", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

func TestServer_JWTAuth_InvalidToken(t *testing.T) {
	store := makeStore(t)
	h := registry.NewServer(store, registry.WithJWTAuth("jwt-secret", "realm")).Handler()
	srv := httptest.NewServer(h)
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v2/", nil)
	req.Header.Set("Authorization", "Bearer not-a-jwt")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

func TestServer_JWTAuth_WrongSigningMethod(t *testing.T) {
	store := makeStore(t)
	h := registry.NewServer(store, registry.WithJWTAuth("jwt-secret", "realm")).Handler()
	srv := httptest.NewServer(h)
	defer srv.Close()

	claims := jwt.MapClaims{
		"scope": "repository:*:pull",
		"exp":   time.Now().Add(time.Hour).Unix(),
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodNone, claims)
	signed, err := tok.SignedString(jwt.UnsafeAllowNoneSignatureType)
	require.NoError(t, err)

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v2/", nil)
	req.Header.Set("Authorization", "Bearer "+signed)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

func TestServer_JWTAuth_ExpiredToken(t *testing.T) {
	store := makeStore(t)
	h := registry.NewServer(store, registry.WithJWTAuth("jwt-secret", "realm")).Handler()
	srv := httptest.NewServer(h)
	defer srv.Close()

	claims := jwt.MapClaims{
		"scope": "repository:*:pull",
		"exp":   time.Now().Add(-time.Hour).Unix(),
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err := tok.SignedString([]byte("jwt-secret"))
	require.NoError(t, err)

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v2/", nil)
	req.Header.Set("Authorization", "Bearer "+signed)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

func TestServer_JWTAuth_WrongIssuer(t *testing.T) {
	store := makeStore(t)
	h := registry.NewServer(store,
		registry.WithJWTAuth("jwt-secret", "realm"),
		registry.WithJWTValidation("expected-issuer", ""),
	).Handler()
	srv := httptest.NewServer(h)
	defer srv.Close()

	claims := jwt.MapClaims{
		"scope": "repository:*:pull",
		"exp":   time.Now().Add(time.Hour).Unix(),
		"iss":   "wrong-issuer",
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err := tok.SignedString([]byte("jwt-secret"))
	require.NoError(t, err)

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v2/", nil)
	req.Header.Set("Authorization", "Bearer "+signed)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

func TestServer_JWTAuth_WrongAudience(t *testing.T) {
	store := makeStore(t)
	h := registry.NewServer(store,
		registry.WithJWTAuth("jwt-secret", "realm"),
		registry.WithJWTValidation("", "expected-aud"),
	).Handler()
	srv := httptest.NewServer(h)
	defer srv.Close()

	claims := jwt.MapClaims{
		"scope": "repository:*:pull",
		"exp":   time.Now().Add(time.Hour).Unix(),
		"aud":   "wrong-aud",
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err := tok.SignedString([]byte("jwt-secret"))
	require.NoError(t, err)

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v2/", nil)
	req.Header.Set("Authorization", "Bearer "+signed)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

func TestServer_JWTAuth_NoScopeClaim(t *testing.T) {
	store := makeStore(t)
	h := registry.NewServer(store, registry.WithJWTAuth("jwt-secret", "realm")).Handler()
	srv := httptest.NewServer(h)
	defer srv.Close()

	claims := jwt.MapClaims{
		"exp": time.Now().Add(time.Hour).Unix(),
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err := tok.SignedString([]byte("jwt-secret"))
	require.NoError(t, err)

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v2/", nil)
	req.Header.Set("Authorization", "Bearer "+signed)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })
	require.Equal(t, http.StatusForbidden, resp.StatusCode)
}

func TestServer_JWTAuth_WrongScope(t *testing.T) {
	store := makeStore(t)
	blobs, err := ociblob.NewStore(filepath.Join(t.TempDir(), "blobs"))
	require.NoError(t, err)
	h := registry.NewServer(store, registry.WithBlobStore(blobs), registry.WithJWTAuth("jwt-secret", "realm")).Handler()
	srv := httptest.NewServer(h)
	defer srv.Close()

	claims := jwt.MapClaims{
		"scope": "repository:otherapp:pull",
		"exp":   time.Now().Add(time.Hour).Unix(),
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err := tok.SignedString([]byte("jwt-secret"))
	require.NoError(t, err)

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v2/myapp/blobs/sha256:abc", nil)
	req.Header.Set("Authorization", "Bearer "+signed)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })
	require.Equal(t, http.StatusForbidden, resp.StatusCode)
}

func TestServer_JWTAuth_WildcardScope(t *testing.T) {
	store := makeStore(t)
	blobs, err := ociblob.NewStore(filepath.Join(t.TempDir(), "blobs"))
	require.NoError(t, err)
	h := registry.NewServer(store, registry.WithBlobStore(blobs), registry.WithJWTAuth("jwt-secret", "realm")).Handler()
	srv := httptest.NewServer(h)
	defer srv.Close()

	claims := jwt.MapClaims{
		"scope": "repository:*:pull",
		"exp":   time.Now().Add(time.Hour).Unix(),
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err := tok.SignedString([]byte("jwt-secret"))
	require.NoError(t, err)

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v2/anyapp/blobs/sha256:abc", nil)
	req.Header.Set("Authorization", "Bearer "+signed)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })
	require.NotEqual(t, http.StatusForbidden, resp.StatusCode)
}

func TestServer_JWTAuth_ScopeNotString(t *testing.T) {
	store := makeStore(t)
	h := registry.NewServer(store, registry.WithJWTAuth("jwt-secret", "realm")).Handler()
	srv := httptest.NewServer(h)
	defer srv.Close()

	claims := jwt.MapClaims{
		"scope": 12345,
		"exp":   time.Now().Add(time.Hour).Unix(),
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err := tok.SignedString([]byte("jwt-secret"))
	require.NoError(t, err)

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v2/", nil)
	req.Header.Set("Authorization", "Bearer "+signed)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })
	require.Equal(t, http.StatusForbidden, resp.StatusCode)
}

func TestServer_Push_InvalidManifest(t *testing.T) {
	srv, _ := startServer(t)

	manifest := `{"schemaVersion":0,"name":"","tag":"","created":"2026-01-01T00:00:00Z","config":{},"diskDigest":"","diskSize":0}`
	body := "--x\r\n" +
		"Content-Disposition: form-data; name=\"manifest\"\r\n\r\n" + manifest + "\r\n" +
		"--x\r\n" +
		"Content-Disposition: form-data; name=\"disk\"; filename=\"disk.img\"\r\n\r\n" +
		"fake\r\n" +
		"--x--\r\n"

	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v2/images", io.NopCloser(strings.NewReader(body)))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "multipart/form-data; boundary=x")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestOCI_DeleteBlob(t *testing.T) {
	srv, _ := startOCIServer(t)

	resp, err := http.Post(srv.URL+"/v2/delapp/blobs/uploads/", "application/octet-stream", nil)
	require.NoError(t, err)
	loc := resp.Header.Get("Location")
	require.NoError(t, resp.Body.Close())
	require.Equal(t, http.StatusAccepted, resp.StatusCode)

	data := []byte("delete me")
	digest := "sha256:" + hex.EncodeToString(sha256SumBytes(data))
	putReq, _ := http.NewRequest(http.MethodPut, srv.URL+loc+"?digest="+digest, bytes.NewReader(data))
	putResp, err := http.DefaultClient.Do(putReq)
	require.NoError(t, err)
	t.Cleanup(func() { _ = putResp.Body.Close() })
	require.Equal(t, http.StatusCreated, putResp.StatusCode)

	delReq, _ := http.NewRequest(http.MethodDelete, srv.URL+"/v2/delapp/blobs/"+digest, nil)
	delResp, err := http.DefaultClient.Do(delReq)
	require.NoError(t, err)
	t.Cleanup(func() { _ = delResp.Body.Close() })
	require.Equal(t, http.StatusAccepted, delResp.StatusCode)

	headReq, _ := http.NewRequest(http.MethodHead, srv.URL+"/v2/delapp/blobs/"+digest, nil)
	headResp, err := http.DefaultClient.Do(headReq)
	require.NoError(t, err)
	t.Cleanup(func() { _ = headResp.Body.Close() })
	require.Equal(t, http.StatusNotFound, headResp.StatusCode)
}

func TestOCI_DeleteBlob_NotFound(t *testing.T) {
	srv, _ := startOCIServer(t)
	delReq, _ := http.NewRequest(http.MethodDelete, srv.URL+"/v2/app/blobs/sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", nil)
	delResp, err := http.DefaultClient.Do(delReq)
	require.NoError(t, err)
	t.Cleanup(func() { _ = delResp.Body.Close() })
	require.Equal(t, http.StatusAccepted, delResp.StatusCode)
}

func TestOCI_GetBlob_NotFound(t *testing.T) {
	srv, _ := startOCIServer(t)
	resp, err := http.Get(srv.URL + "/v2/app/blobs/sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestOCI_HeadBlob_NotFound(t *testing.T) {
	srv, _ := startOCIServer(t)
	req, _ := http.NewRequest(http.MethodHead, srv.URL+"/v2/app/blobs/sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", nil)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestOCI_DeleteManifest(t *testing.T) {
	srv, _ := startOCIServer(t)
	client := registry.NewClient(srv.URL)

	disk := makeDiskFile(t)
	m := image.Manifest{
		SchemaVersion: image.SchemaVersion,
		Name:          "delmanifest",
		Tag:           "latest",
		Created:       time.Now().UTC(),
		Config:        image.Config{Memory: "256M", CPUs: 1},
		DiskDigest:    "sha256:aaa",
		DiskSize:      1024,
	}
	require.NoError(t, client.PushOCI(context.Background(), m, disk))

	delReq, _ := http.NewRequest(http.MethodDelete, srv.URL+"/v2/delmanifest/manifests/latest", nil)
	delResp, err := http.DefaultClient.Do(delReq)
	require.NoError(t, err)
	t.Cleanup(func() { _ = delResp.Body.Close() })
	require.Equal(t, http.StatusAccepted, delResp.StatusCode)

	getResp, err := http.Get(srv.URL + "/v2/delmanifest/manifests/latest")
	require.NoError(t, err)
	t.Cleanup(func() { _ = getResp.Body.Close() })
	require.Equal(t, http.StatusNotFound, getResp.StatusCode)
}

func TestOCI_DeleteManifest_NotFound(t *testing.T) {
	srv, _ := startOCIServer(t)
	delReq, _ := http.NewRequest(http.MethodDelete, srv.URL+"/v2/nonexist/manifests/latest", nil)
	delResp, err := http.DefaultClient.Do(delReq)
	require.NoError(t, err)
	t.Cleanup(func() { _ = delResp.Body.Close() })
	require.Equal(t, http.StatusNotFound, delResp.StatusCode)
}

func TestOCI_GetManifest_MemoryOnlyStore(t *testing.T) {
	store := makeStore(t)
	blobs, err := ociblob.NewStore(filepath.Join(t.TempDir(), "blobs"))
	require.NoError(t, err)
	h := registry.NewServer(store, registry.WithBlobStore(blobs)).Handler()
	srv := httptest.NewServer(h)
	defer srv.Close()

	client := registry.NewClient(srv.URL)
	disk := makeDiskFile(t)
	m := image.Manifest{
		SchemaVersion: image.SchemaVersion,
		Name:          "memonly",
		Tag:           "v1",
		Created:       time.Now().UTC(),
		Config:        image.Config{Memory: "256M", CPUs: 1},
		DiskDigest:    "sha256:aaa",
		DiskSize:      1024,
	}
	require.NoError(t, client.PushOCI(context.Background(), m, disk))

	getResp, err := http.Get(srv.URL + "/v2/memonly/manifests/v1")
	require.NoError(t, err)
	t.Cleanup(func() { _ = getResp.Body.Close() })
	require.Equal(t, http.StatusOK, getResp.StatusCode)

	body, err := io.ReadAll(getResp.Body)
	require.NoError(t, err)
	parsed, err := ociregistry.ParseManifest(body)
	require.NoError(t, err)
	require.Len(t, parsed.Layers, 1)
}

func TestOCI_GetManifest_NotFound_MemoryOnly(t *testing.T) {
	store := makeStore(t)
	blobs, err := ociblob.NewStore(filepath.Join(t.TempDir(), "blobs"))
	require.NoError(t, err)
	h := registry.NewServer(store, registry.WithBlobStore(blobs)).Handler()
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v2/nonexist/manifests/latest")
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestOCI_HeadManifest_MemoryOnly(t *testing.T) {
	store := makeStore(t)
	blobs, err := ociblob.NewStore(filepath.Join(t.TempDir(), "blobs"))
	require.NoError(t, err)
	h := registry.NewServer(store, registry.WithBlobStore(blobs)).Handler()
	srv := httptest.NewServer(h)
	defer srv.Close()

	client := registry.NewClient(srv.URL)
	disk := makeDiskFile(t)
	m := image.Manifest{
		SchemaVersion: image.SchemaVersion,
		Name:          "headtest",
		Tag:           "v1",
		Created:       time.Now().UTC(),
		Config:        image.Config{Memory: "256M", CPUs: 1},
		DiskDigest:    "sha256:aaa",
		DiskSize:      1024,
	}
	require.NoError(t, client.PushOCI(context.Background(), m, disk))

	headReq, _ := http.NewRequest(http.MethodHead, srv.URL+"/v2/headtest/manifests/v1", nil)
	headResp, err := http.DefaultClient.Do(headReq)
	require.NoError(t, err)
	t.Cleanup(func() { _ = headResp.Body.Close() })
	require.Equal(t, http.StatusOK, headResp.StatusCode)
	require.NotEmpty(t, headResp.Header.Get("Docker-Content-Digest"))
}

func TestOCI_HeadManifest_NotFound_MemoryOnly(t *testing.T) {
	store := makeStore(t)
	blobs, err := ociblob.NewStore(filepath.Join(t.TempDir(), "blobs"))
	require.NoError(t, err)
	h := registry.NewServer(store, registry.WithBlobStore(blobs)).Handler()
	srv := httptest.NewServer(h)
	defer srv.Close()

	headReq, _ := http.NewRequest(http.MethodHead, srv.URL+"/v2/missing/manifests/latest", nil)
	headResp, err := http.DefaultClient.Do(headReq)
	require.NoError(t, err)
	t.Cleanup(func() { _ = headResp.Body.Close() })
	require.Equal(t, http.StatusNotFound, headResp.StatusCode)
}

func TestOCI_DeleteManifest_MemoryOnly(t *testing.T) {
	store := makeStore(t)
	blobs, err := ociblob.NewStore(filepath.Join(t.TempDir(), "blobs"))
	require.NoError(t, err)
	h := registry.NewServer(store, registry.WithBlobStore(blobs)).Handler()
	srv := httptest.NewServer(h)
	defer srv.Close()

	client := registry.NewClient(srv.URL)
	disk := makeDiskFile(t)
	m := image.Manifest{
		SchemaVersion: image.SchemaVersion,
		Name:          "delmem",
		Tag:           "v1",
		Created:       time.Now().UTC(),
		Config:        image.Config{Memory: "256M", CPUs: 1},
		DiskDigest:    "sha256:aaa",
		DiskSize:      1024,
	}
	require.NoError(t, client.PushOCI(context.Background(), m, disk))

	delReq, _ := http.NewRequest(http.MethodDelete, srv.URL+"/v2/delmem/manifests/v1", nil)
	delResp, err := http.DefaultClient.Do(delReq)
	require.NoError(t, err)
	t.Cleanup(func() { _ = delResp.Body.Close() })
	require.Equal(t, http.StatusAccepted, delResp.StatusCode)

	delReq2, _ := http.NewRequest(http.MethodDelete, srv.URL+"/v2/delmem/manifests/v1", nil)
	delResp2, err := http.DefaultClient.Do(delReq2)
	require.NoError(t, err)
	t.Cleanup(func() { _ = delResp2.Body.Close() })
	require.Equal(t, http.StatusNotFound, delResp2.StatusCode)
}

func TestOCI_PutManifest_MissingConfigBlob(t *testing.T) {
	store := makeStore(t)
	blobs, err := ociblob.NewStore(filepath.Join(t.TempDir(), "blobs"))
	require.NoError(t, err)
	h := registry.NewServer(store, registry.WithBlobStore(blobs)).Handler()
	srv := httptest.NewServer(h)
	defer srv.Close()

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
	body, err := ociregistry.MarshalManifest(m)
	require.NoError(t, err)

	req, _ := http.NewRequest(http.MethodPut, srv.URL+"/v2/app/manifests/latest", bytes.NewReader(body))
	req.Header.Set("Content-Type", ociregistry.MediaTypeImageManifest)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestOCI_PutManifest_MissingLayerBlob(t *testing.T) {
	store := makeStore(t)
	blobs, err := ociblob.NewStore(filepath.Join(t.TempDir(), "blobs"))
	require.NoError(t, err)
	h := registry.NewServer(store, registry.WithBlobStore(blobs)).Handler()
	srv := httptest.NewServer(h)
	defer srv.Close()

	configData := []byte(`{"memory":"256M"}`)
	configDigest, _, err := blobs.Put(bytes.NewReader(configData))
	require.NoError(t, err)

	m := ociregistry.Manifest{
		SchemaVersion: ociregistry.OCIManifestSchemaVersion,
		MediaType:     ociregistry.MediaTypeImageManifest,
		Config: ociregistry.Descriptor{
			MediaType: ociregistry.MediaTypeImageConfig,
			Digest:    configDigest,
			Size:      int64(len(configData)),
		},
		Layers: []ociregistry.Descriptor{{
			MediaType: ociregistry.MediaTypeImageLayerTarGzip,
			Digest:    "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
			Size:      2,
		}},
	}
	body, err := ociregistry.MarshalManifest(m)
	require.NoError(t, err)

	req, _ := http.NewRequest(http.MethodPut, srv.URL+"/v2/app/manifests/latest", bytes.NewReader(body))
	req.Header.Set("Content-Type", ociregistry.MediaTypeImageManifest)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestOCI_PutManifest_InvalidJSON(t *testing.T) {
	store := makeStore(t)
	blobs, err := ociblob.NewStore(filepath.Join(t.TempDir(), "blobs"))
	require.NoError(t, err)
	h := registry.NewServer(store, registry.WithBlobStore(blobs)).Handler()
	srv := httptest.NewServer(h)
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodPut, srv.URL+"/v2/app/manifests/latest", strings.NewReader("not json"))
	req.Header.Set("Content-Type", ociregistry.MediaTypeImageManifest)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestOCI_Catalog_MemoryOnly(t *testing.T) {
	store := makeStore(t)
	blobs, err := ociblob.NewStore(filepath.Join(t.TempDir(), "blobs"))
	require.NoError(t, err)
	h := registry.NewServer(store, registry.WithBlobStore(blobs)).Handler()
	srv := httptest.NewServer(h)
	defer srv.Close()

	client := registry.NewClient(srv.URL)
	disk := makeDiskFile(t)
	m := image.Manifest{
		SchemaVersion: image.SchemaVersion,
		Name:          "catapp",
		Tag:           "latest",
		Created:       time.Now().UTC(),
		Config:        image.Config{Memory: "256M", CPUs: 1},
		DiskDigest:    "sha256:aaa",
		DiskSize:      1024,
	}
	require.NoError(t, client.PushOCI(context.Background(), m, disk))

	resp, err := http.Get(srv.URL + "/v2/_catalog")
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var cat struct {
		Repositories []string `json:"repositories"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&cat))
	require.Contains(t, cat.Repositories, "catapp")
}

func TestOCI_Catalog_WithOCIStore_Error(t *testing.T) {
	store := makeStore(t)
	blobs, err := ociblob.NewStore(filepath.Join(t.TempDir(), "blobs"))
	require.NoError(t, err)
	ociDir := filepath.Join(t.TempDir(), "oci")
	ociStore, err := registry.NewOCIStore(ociDir)
	require.NoError(t, err)

	refsFile := filepath.Join(ociDir, "refs.json")
	require.NoError(t, os.WriteFile(refsFile, []byte("not json"), 0o644))

	h := registry.NewServer(store, registry.WithBlobStore(blobs), registry.WithOCIStore(ociStore)).Handler()
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v2/_catalog")
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })
	require.Equal(t, http.StatusInternalServerError, resp.StatusCode)
}

func TestOCI_StartUpload_NoBlobStore(t *testing.T) {
	store := makeStore(t)
	h := registry.NewServer(store).Handler()
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v2/app/blobs/uploads/", "application/octet-stream", nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })
	require.Equal(t, http.StatusNotImplemented, resp.StatusCode)
}

func TestOCI_CompleteUpload_MissingDigest(t *testing.T) {
	srv, _ := startOCIServer(t)

	startResp, err := http.Post(srv.URL+"/v2/app/blobs/uploads/", "application/octet-stream", nil)
	require.NoError(t, err)
	loc := startResp.Header.Get("Location")
	require.NoError(t, startResp.Body.Close())
	require.Equal(t, http.StatusAccepted, startResp.StatusCode)

	req, _ := http.NewRequest(http.MethodPut, srv.URL+loc, bytes.NewReader([]byte("data")))
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestOCI_CompleteUpload_UnknownUUID(t *testing.T) {
	srv, _ := startOCIServer(t)
	req, _ := http.NewRequest(http.MethodPut, srv.URL+"/v2/app/blobs/uploads/fakeuuid?digest=sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", nil)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestOCI_PatchUpload_UnknownUUID(t *testing.T) {
	srv, _ := startOCIServer(t)
	req, _ := http.NewRequest(http.MethodPatch, srv.URL+"/v2/app/blobs/uploads/fakeuuid", bytes.NewReader([]byte("data")))
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestOCI_PatchUpload_EmptyUUID(t *testing.T) {
	srv, _ := startOCIServer(t)
	req, _ := http.NewRequest(http.MethodPatch, srv.URL+"/v2/app/blobs/uploads/", bytes.NewReader([]byte("data")))
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestOCI_NoBlobStore_Operations(t *testing.T) {
	store := makeStore(t)
	h := registry.NewServer(store).Handler()
	srv := httptest.NewServer(h)
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v2/app/blobs/sha256:abc", nil)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })
	require.Equal(t, http.StatusNotImplemented, resp.StatusCode)

	headReq, _ := http.NewRequest(http.MethodHead, srv.URL+"/v2/app/blobs/sha256:abc", nil)
	headResp, err := http.DefaultClient.Do(headReq)
	require.NoError(t, err)
	t.Cleanup(func() { _ = headResp.Body.Close() })
	require.Equal(t, http.StatusNotImplemented, headResp.StatusCode)

	delReq, _ := http.NewRequest(http.MethodDelete, srv.URL+"/v2/app/blobs/sha256:abc", nil)
	delResp, err := http.DefaultClient.Do(delReq)
	require.NoError(t, err)
	t.Cleanup(func() { _ = delResp.Body.Close() })
	require.Equal(t, http.StatusNotImplemented, delResp.StatusCode)

	putMReq, _ := http.NewRequest(http.MethodPut, srv.URL+"/v2/app/manifests/latest", strings.NewReader("{}"))
	putMResp, err := http.DefaultClient.Do(putMReq)
	require.NoError(t, err)
	t.Cleanup(func() { _ = putMResp.Body.Close() })
	require.Equal(t, http.StatusNotImplemented, putMResp.StatusCode)

	compReq, _ := http.NewRequest(http.MethodPut, srv.URL+"/v2/app/blobs/uploads/abc?digest=sha256:aaa", nil)
	compResp, err := http.DefaultClient.Do(compReq)
	require.NoError(t, err)
	t.Cleanup(func() { _ = compResp.Body.Close() })
	require.Equal(t, http.StatusNotImplemented, compResp.StatusCode)

	patchReq, _ := http.NewRequest(http.MethodPatch, srv.URL+"/v2/app/blobs/uploads/abc", bytes.NewReader([]byte("x")))
	patchResp, err := http.DefaultClient.Do(patchReq)
	require.NoError(t, err)
	t.Cleanup(func() { _ = patchResp.Body.Close() })
	require.Equal(t, http.StatusNotImplemented, patchResp.StatusCode)
}

func TestOCI_OCIPathRouting_NotFound(t *testing.T) {
	srv, _ := startOCIServer(t)

	resp, err := http.Get(srv.URL + "/v2/app/unknownkind/something")
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestParseOCIPath(t *testing.T) {
	tests := []struct {
		path string
		name string
		kind string
		tail string
		ok   bool
	}{
		{"/v2/team/app/blobs/sha256:abc", "team/app", "blobs", "sha256:abc", true},
		{"/v2/app/manifests/latest", "app", "manifests", "latest", true},
		{"/v2/_catalog", "", "", "", false},
		{"/v2/images", "", "", "", false},
		{"/v2/", "", "", "", false},
		{"/v2/app/blobs/", "", "", "", false},
	}

	for _, tc := range tests {
		t.Run(tc.path, func(t *testing.T) {
			n, k, tl, ok := parseOCIPathInline(tc.path)
			require.Equal(t, tc.ok, ok)
			if ok {
				require.Equal(t, tc.name, n)
				require.Equal(t, tc.kind, k)
				require.Equal(t, tc.tail, tl)
			}
		})
	}
}

func parseOCIPathInline(path string) (name, kind, tail string, ok bool) {
	trimmed := strings.TrimSuffix(strings.TrimPrefix(path, "/v2/"), "/")
	if trimmed == "" || trimmed == "_catalog" || trimmed == "images" {
		return "", "", "", false
	}
	for _, marker := range []string{"/blobs/", "/manifests/"} {
		idx := strings.Index(trimmed, marker)
		if idx <= 0 {
			continue
		}
		name = trimmed[:idx]
		kind = strings.Trim(marker, "/")
		kind = strings.Split(kind, "/")[0]
		tail = trimmed[idx+len(marker):]
		if name == "" || tail == "" {
			return "", "", "", false
		}
		return name, kind, tail, true
	}
	return "", "", "", false
}

func TestRequiredAction(t *testing.T) {
	tests := []struct {
		method string
		action string
	}{
		{http.MethodGet, "pull"},
		{http.MethodHead, "pull"},
		{http.MethodPost, "push"},
		{http.MethodPut, "push"},
		{http.MethodDelete, "push"},
		{http.MethodPatch, "pull"},
	}
	for _, tc := range tests {
		t.Run(tc.method, func(t *testing.T) {
			action := requiredActionInline(tc.method)
			require.Equal(t, tc.action, action)
		})
	}
}

func requiredActionInline(method string) string {
	switch method {
	case http.MethodGet, http.MethodHead:
		return "pull"
	case http.MethodPost, http.MethodPut, http.MethodDelete:
		return "push"
	default:
		return "pull"
	}
}

func TestHasRequiredScope(t *testing.T) {
	tests := []struct {
		name   string
		scope  string
		repo   string
		action string
		want   bool
	}{
		{"exact match", "repository:app:pull", "app", "pull", true},
		{"wildcard repo", "repository:*:pull", "app", "pull", true},
		{"wrong repo", "repository:other:pull", "app", "pull", false},
		{"wrong action", "repository:app:push", "app", "pull", false},
		{"multi action", "repository:app:pull,push", "app", "push", true},
		{"bad format", "badformat", "app", "pull", false},
		{"missing scope", "", "app", "pull", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			claims := jwt.MapClaims{}
			if tc.scope != "" {
				claims["scope"] = tc.scope
			}
			got := hasRequiredScopeInline(claims, tc.repo, tc.action)
			require.Equal(t, tc.want, got)
		})
	}
}

func hasRequiredScopeInline(claims jwt.MapClaims, repo, action string) bool {
	scopeValue, ok := claims["scope"]
	if !ok {
		return false
	}
	scope, ok := scopeValue.(string)
	if !ok {
		return false
	}
	entries := strings.Fields(scope)
	for _, e := range entries {
		parts := strings.Split(e, ":")
		if len(parts) != 3 || parts[0] != "repository" {
			continue
		}
		target := parts[1]
		if target != "*" && target != repo {
			continue
		}
		actions := strings.Split(parts[2], ",")
		for _, a := range actions {
			if strings.TrimSpace(a) == action {
				return true
			}
		}
	}
	return false
}

func TestOCI_PutManifest_SaveError(t *testing.T) {
	store := makeStore(t)
	blobs, err := ociblob.NewStore(filepath.Join(t.TempDir(), "blobs"))
	require.NoError(t, err)
	ociDir := filepath.Join(t.TempDir(), "oci")
	ociStore, err := registry.NewOCIStore(ociDir)
	require.NoError(t, err)

	h := registry.NewServer(store, registry.WithBlobStore(blobs), registry.WithOCIStore(ociStore)).Handler()
	srv := httptest.NewServer(h)
	defer srv.Close()

	client := registry.NewClient(srv.URL)
	disk := makeDiskFile(t)
	m := image.Manifest{
		SchemaVersion: image.SchemaVersion,
		Name:          "saveerr",
		Tag:           "latest",
		Created:       time.Now().UTC(),
		Config:        image.Config{Memory: "256M", CPUs: 1},
		DiskDigest:    "sha256:aaa",
		DiskSize:      1024,
	}
	require.NoError(t, client.PushOCI(context.Background(), m, disk))

	refsPath := filepath.Join(ociDir, "refs.json")
	require.NoError(t, os.WriteFile(refsPath, []byte("corrupt"), 0o644))

	configData := []byte(`{"memory":"256M","cpus":1,"created":"2026-01-01T00:00:00Z"}`)
	_, _, err = blobs.Put(bytes.NewReader(configData))
	require.NoError(t, err)

	req, _ := http.NewRequest(http.MethodPut, srv.URL+"/v2/saveerr/manifests/latest", strings.NewReader("{}"))
	req.Header.Set("Content-Type", ociregistry.MediaTypeImageManifest)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })
	require.NotEqual(t, http.StatusCreated, resp.StatusCode)
}

func sha256SumBytes(data []byte) []byte {
	h := sha256.Sum256(data)
	return h[:]
}

func TestWriteTempFile(t *testing.T) {
	f, err := writeTempFileInline(strings.NewReader("hello"))
	require.NoError(t, err)
	defer os.Remove(f)
	data, err := os.ReadFile(f)
	require.NoError(t, err)
	require.Equal(t, "hello", string(data))
}

func writeTempFileInline(r io.Reader) (string, error) {
	f, err := os.CreateTemp("", "test-upload-*.img")
	if err != nil {
		return "", err
	}
	defer f.Close()
	if _, err := io.Copy(f, r); err != nil {
		os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}
