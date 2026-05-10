package registry

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"

	"github.com/AitorConS/unikernel-engine/internal/image"
	"github.com/AitorConS/unikernel-engine/internal/ociblob"
	"github.com/AitorConS/unikernel-engine/internal/ociregistry"
)

// Server is an HTTP image registry backed by an image.Store.
//
// Routes:
//
//	GET    /v2/images           — list all images
//	GET    /v2/images/{ref}     — get manifest by name:tag or sha256
//	GET    /v2/images/{ref}/disk — download raw disk image
//	POST   /v2/images           — push image (multipart: manifest + disk)
//	DELETE /v2/images/{ref}     — remove image
type Server struct {
	store      *image.Store
	blobStore  *ociblob.Store
	manifestMu sync.RWMutex
	manifests  map[string]map[string]ociregistry.Manifest
	uploadMu   sync.Mutex
	uploads    map[string]struct{}
}

// Option configures a registry Server.
type Option func(*Server) error

// WithBlobStore configures the OCI blob store for v2 endpoints.
func WithBlobStore(store *ociblob.Store) Option {
	return func(s *Server) error {
		s.blobStore = store
		return nil
	}
}

// NewServer returns a Server backed by store.
func NewServer(store *image.Store, opts ...Option) *Server {
	srv := &Server{
		store:     store,
		manifests: make(map[string]map[string]ociregistry.Manifest),
		uploads:   make(map[string]struct{}),
	}
	for _, opt := range opts {
		if err := opt(srv); err != nil {
			slog.Warn("registry: apply option", "err", err)
		}
	}
	return srv
}

// Handler returns an http.Handler for the registry API.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v2/images", s.handleList)
	mux.HandleFunc("POST /v2/images", s.handlePush)
	mux.HandleFunc("GET /v2/images/{ref}", s.handleGetManifest)
	mux.HandleFunc("GET /v2/images/{ref}/disk", s.handleGetDisk)
	mux.HandleFunc("DELETE /v2/images/{ref}", s.handleRemove)
	mux.HandleFunc("/v2/", s.handleOCIV2)
	return mux
}

func (s *Server) handleOCIV2(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/v2/" && r.Method == http.MethodGet {
		s.handleOCIBase(w, r)
		return
	}
	if r.URL.Path == "/v2/_catalog" && r.Method == http.MethodGet {
		s.handleOCICatalog(w, r)
		return
	}

	path := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v2/"), "/")
	parts := strings.Split(path, "/")
	if len(parts) < 3 {
		http.NotFound(w, r)
		return
	}
	name := parts[0]
	kind := parts[1]

	switch kind {
	case "blobs":
		if len(parts) == 3 && parts[2] == "uploads" && r.Method == http.MethodPost {
			r.SetPathValue("name", name)
			s.handleOCIStartUpload(w, r)
			return
		}
		if len(parts) == 4 && parts[2] == "uploads" && r.Method == http.MethodPut {
			r.SetPathValue("name", name)
			r.SetPathValue("uuid", parts[3])
			s.handleOCICompleteUpload(w, r)
			return
		}
		if len(parts) == 3 {
			r.SetPathValue("name", name)
			r.SetPathValue("digest", parts[2])
			if r.Method == http.MethodGet {
				s.handleOCIGetBlob(w, r)
				return
			}
			if r.Method == http.MethodDelete {
				s.handleOCIDeleteBlob(w, r)
				return
			}
		}
	case "manifests":
		if len(parts) == 3 {
			r.SetPathValue("name", name)
			r.SetPathValue("ref", parts[2])
			switch r.Method {
			case http.MethodPut:
				s.handleOCIPutManifest(w, r)
				return
			case http.MethodGet:
				s.handleOCIGetManifest(w, r)
				return
			case http.MethodDelete:
				s.handleOCIDeleteManifest(w, r)
				return
			}
		}
	}

	http.NotFound(w, r)
}

func (s *Server) handleOCIBase(w http.ResponseWriter, _ *http.Request) {
	httpErr(w, http.StatusUnauthorized, "unauthorized")
}

func (s *Server) handleOCICatalog(w http.ResponseWriter, _ *http.Request) {
	s.manifestMu.RLock()
	repos := make([]string, 0, len(s.manifests))
	for name := range s.manifests {
		repos = append(repos, name)
	}
	s.manifestMu.RUnlock()
	writeJSON(w, http.StatusOK, map[string]any{"repositories": repos})
}

func (s *Server) handleOCIStartUpload(w http.ResponseWriter, r *http.Request) {
	if s.blobStore == nil {
		httpErr(w, http.StatusNotImplemented, "OCI blobs not configured")
		return
	}
	name := r.PathValue("name")
	uuid, err := randomID()
	if err != nil {
		httpErr(w, http.StatusInternalServerError, "create upload id: "+err.Error())
		return
	}

	s.uploadMu.Lock()
	s.uploads[uuid] = struct{}{}
	s.uploadMu.Unlock()

	w.Header().Set("Location", fmt.Sprintf("/v2/%s/blobs/uploads/%s", name, uuid))
	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) handleOCICompleteUpload(w http.ResponseWriter, r *http.Request) {
	if s.blobStore == nil {
		httpErr(w, http.StatusNotImplemented, "OCI blobs not configured")
		return
	}
	uuid := r.PathValue("uuid")
	digest := r.URL.Query().Get("digest")
	if !strings.HasPrefix(digest, "sha256:") {
		httpErr(w, http.StatusBadRequest, "digest query param is required")
		return
	}

	s.uploadMu.Lock()
	_, ok := s.uploads[uuid]
	if ok {
		delete(s.uploads, uuid)
	}
	s.uploadMu.Unlock()
	if !ok {
		httpErr(w, http.StatusNotFound, "upload not found")
		return
	}

	gotDigest, _, err := s.blobStore.Put(r.Body)
	if err != nil {
		httpErr(w, http.StatusInternalServerError, "store blob: "+err.Error())
		return
	}
	if gotDigest != digest {
		httpErr(w, http.StatusBadRequest, fmt.Sprintf("digest mismatch: got %s", gotDigest))
		return
	}

	name := r.PathValue("name")
	w.Header().Set("Docker-Content-Digest", gotDigest)
	w.Header().Set("Location", fmt.Sprintf("/v2/%s/blobs/%s", name, gotDigest))
	w.WriteHeader(http.StatusCreated)
}

func (s *Server) handleOCIGetBlob(w http.ResponseWriter, r *http.Request) {
	if s.blobStore == nil {
		httpErr(w, http.StatusNotImplemented, "OCI blobs not configured")
		return
	}
	digest := r.PathValue("digest")
	if !strings.HasPrefix(digest, "sha256:") {
		digest = "sha256:" + digest
	}
	rc, err := s.blobStore.Open(digest)
	if err != nil {
		httpErr(w, http.StatusNotFound, err.Error())
		return
	}
	defer func() { _ = rc.Close() }()

	w.Header().Set("Docker-Content-Digest", digest)
	w.Header().Set("Content-Type", "application/octet-stream")
	w.WriteHeader(http.StatusOK)
	if _, err := io.Copy(w, rc); err != nil {
		slog.Warn("registry: stream OCI blob", "err", err)
	}
}

func (s *Server) handleOCIDeleteBlob(w http.ResponseWriter, r *http.Request) {
	if s.blobStore == nil {
		httpErr(w, http.StatusNotImplemented, "OCI blobs not configured")
		return
	}
	digest := r.PathValue("digest")
	if !strings.HasPrefix(digest, "sha256:") {
		digest = "sha256:" + digest
	}
	if err := s.blobStore.Delete(digest); err != nil {
		httpErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) handleOCIPutManifest(w http.ResponseWriter, r *http.Request) {
	if s.blobStore == nil {
		httpErr(w, http.StatusNotImplemented, "OCI blobs not configured")
		return
	}
	name := r.PathValue("name")
	ref := r.PathValue("ref")
	data, err := io.ReadAll(r.Body)
	if err != nil {
		httpErr(w, http.StatusBadRequest, "read manifest body: "+err.Error())
		return
	}
	m, err := ociregistry.ParseManifest(data)
	if err != nil {
		httpErr(w, http.StatusBadRequest, "invalid OCI manifest: "+err.Error())
		return
	}
	if !s.blobStore.Exists(m.Config.Digest) {
		httpErr(w, http.StatusBadRequest, "config blob not found: "+m.Config.Digest)
		return
	}
	for _, layer := range m.Layers {
		if !s.blobStore.Exists(layer.Digest) {
			httpErr(w, http.StatusBadRequest, "layer blob not found: "+layer.Digest)
			return
		}
	}
	manifestDigest := image.DigestSHA256(data)

	s.manifestMu.Lock()
	if _, ok := s.manifests[name]; !ok {
		s.manifests[name] = make(map[string]ociregistry.Manifest)
	}
	s.manifests[name][ref] = m
	s.manifests[name][manifestDigest] = m
	s.manifestMu.Unlock()

	w.Header().Set("Docker-Content-Digest", manifestDigest)
	w.Header().Set("Content-Type", ociregistry.MediaTypeImageManifest)
	w.WriteHeader(http.StatusCreated)
}

func (s *Server) handleOCIGetManifest(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	ref := r.PathValue("ref")
	s.manifestMu.RLock()
	repo, ok := s.manifests[name]
	if !ok {
		s.manifestMu.RUnlock()
		httpErr(w, http.StatusNotFound, "repository not found")
		return
	}
	m, ok := repo[ref]
	s.manifestMu.RUnlock()
	if !ok {
		httpErr(w, http.StatusNotFound, "manifest not found")
		return
	}
	data, err := ociregistry.MarshalManifest(m)
	if err != nil {
		httpErr(w, http.StatusInternalServerError, "marshal OCI manifest: "+err.Error())
		return
	}
	w.Header().Set("Content-Type", ociregistry.MediaTypeImageManifest)
	w.Header().Set("Docker-Content-Digest", image.DigestSHA256(data))
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(data); err != nil {
		slog.Warn("registry: write OCI manifest", "err", err)
	}
}

func (s *Server) handleOCIDeleteManifest(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	ref := r.PathValue("ref")
	s.manifestMu.Lock()
	defer s.manifestMu.Unlock()
	repo, ok := s.manifests[name]
	if !ok {
		httpErr(w, http.StatusNotFound, "repository not found")
		return
	}
	if _, ok := repo[ref]; !ok {
		httpErr(w, http.StatusNotFound, "manifest not found")
		return
	}
	delete(repo, ref)
	if len(repo) == 0 {
		delete(s.manifests, name)
	}
	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) handleList(w http.ResponseWriter, _ *http.Request) {
	list, err := s.store.List()
	if err != nil {
		httpErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) handleGetManifest(w http.ResponseWriter, r *http.Request) {
	ref := r.PathValue("ref")
	m, _, err := s.store.Get(ref)
	if err != nil {
		httpErr(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, m)
}

func (s *Server) handleGetDisk(w http.ResponseWriter, r *http.Request) {
	ref := r.PathValue("ref")
	_, diskPath, err := s.store.Get(ref)
	if err != nil {
		httpErr(w, http.StatusNotFound, err.Error())
		return
	}
	f, err := os.Open(diskPath)
	if err != nil {
		httpErr(w, http.StatusInternalServerError, "open disk: "+err.Error())
		return
	}
	defer func() {
		if err := f.Close(); err != nil {
			slog.Warn("registry: close disk file", "err", err)
		}
	}()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.WriteHeader(http.StatusOK)
	if _, err := io.Copy(w, f); err != nil {
		slog.Warn("registry: stream disk", "err", err)
	}
}

func (s *Server) handlePush(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(512 << 20); err != nil {
		httpErr(w, http.StatusBadRequest, "parse multipart: "+err.Error())
		return
	}
	manifestJSON := r.FormValue("manifest")
	if manifestJSON == "" {
		httpErr(w, http.StatusBadRequest, "manifest field is required")
		return
	}
	m, err := image.Parse([]byte(manifestJSON))
	if err != nil {
		httpErr(w, http.StatusBadRequest, "invalid manifest: "+err.Error())
		return
	}
	file, _, err := r.FormFile("disk")
	if err != nil {
		httpErr(w, http.StatusBadRequest, "disk field is required: "+err.Error())
		return
	}
	defer func() {
		if err := file.Close(); err != nil {
			slog.Warn("registry: close upload", "err", err)
		}
	}()

	tmp, err := writeTempFile(file)
	if err != nil {
		httpErr(w, http.StatusInternalServerError, "store upload: "+err.Error())
		return
	}
	defer func() { _ = os.Remove(tmp) }()

	if err := s.store.Put(m.Name, m.Tag, m, tmp); err != nil {
		httpErr(w, http.StatusInternalServerError, "store put: "+err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, m)
}

func (s *Server) handleRemove(w http.ResponseWriter, r *http.Request) {
	ref := r.PathValue("ref")
	if err := s.store.Remove(ref); err != nil {
		httpErr(w, http.StatusNotFound, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func httpErr(w http.ResponseWriter, code int, msg string) {
	http.Error(w, fmt.Sprintf(`{"error":%q}`, msg), code)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Warn("registry: encode response", "err", err)
	}
}

func writeTempFile(r io.Reader) (string, error) {
	f, err := os.CreateTemp("", "uni-registry-upload-*.img")
	if err != nil {
		return "", fmt.Errorf("create temp: %w", err)
	}
	defer func() {
		if err := f.Close(); err != nil {
			_ = err // best effort
		}
	}()
	if _, err := io.Copy(f, r); err != nil {
		_ = os.Remove(f.Name())
		return "", fmt.Errorf("write temp: %w", err)
	}
	return f.Name(), nil
}

func randomID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("read random: %w", err)
	}
	return fmt.Sprintf("%x", b), nil
}
