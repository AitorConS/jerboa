// Package registrytest is an in-memory registry speaking the protocol of
// docs/registry-protocol.md, for tests of the client side. It accepts any
// non-empty bearer token for writes and serves its CDN layout under /cdn.
package registrytest

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/AitorConS/jerboa/internal/registry/pack"
)

// Server is a running fake registry.
type Server struct {
	*httptest.Server
	mu        sync.Mutex
	objects   map[string][]byte
	locations map[string]pack.Location
	repos     map[string]map[string]string
	pulls     map[string]int
	// Requests counts requests by "METHOD /first/segments".
	Requests map[string]int
}

// New starts a fake registry.
func New() *Server {
	s := &Server{objects: map[string][]byte{}, locations: map[string]pack.Location{}, repos: map[string]map[string]string{}, pulls: map[string]int{}, Requests: map[string]int{}}
	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	return s
}

// Host is the registry host for naming.ImageReference.
func (s *Server) Host() string { return strings.TrimPrefix(s.URL, "http://") }

// Object returns a stored object by key, for assertions.
func (s *Server) Object(key string) ([]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.objects[key]
	return b, ok
}

// Repo returns the fields PATCH /v2/<ns>/<repo> set on a repository.
func (s *Server) Repo(namespace, repository string) map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.repos[namespace+"/"+repository]
}

// Pulls returns how many pulls clients reported for a repository.
func (s *Server) Pulls(namespace, repository string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pulls[namespace+"/"+repository]
}

// Corrupt flips one byte of a stored object.
func (s *Server) Corrupt(key string, at int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.objects[key][at] ^= 0xff
}

func fail(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": code, "message": msg}})
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	s.mu.Lock()
	key := r.Method + " /" + strings.Join(parts[:min(len(parts), 1)], "/")
	if len(parts) >= 4 && parts[0] == "v2" {
		key = r.Method + " /v2/" + parts[3]
	}
	s.Requests[key]++
	s.mu.Unlock()

	switch {
	case r.URL.Path == "/v2/" && r.Method == http.MethodGet:
		_ = json.NewEncoder(w).Encode(map[string]any{"api": 1, "cdn": s.URL + "/cdn", "github_client_id": "test"})
	case parts[0] == "cdn":
		s.serveCDN(w, r, strings.Join(parts[1:], "/"))
	case parts[0] == "v2" && len(parts) == 3 && r.Method == http.MethodPatch:
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			fail(w, http.StatusUnauthorized, "UNAUTHORIZED", "missing token")
			return
		}
		var changes map[string]string
		if err := json.NewDecoder(r.Body).Decode(&changes); err != nil || len(changes) == 0 {
			fail(w, http.StatusBadRequest, "BAD_REQUEST", "nothing to change")
			return
		}
		s.mu.Lock()
		key := parts[1] + "/" + parts[2]
		if s.repos[key] == nil {
			s.repos[key] = map[string]string{}
		}
		for k, v := range changes {
			s.repos[key][k] = v
		}
		s.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]string{"namespace": parts[1], "name": parts[2]})
	case parts[0] == "v2" && len(parts) == 4 && parts[3] == "pulls" && r.Method == http.MethodPost:
		s.mu.Lock()
		s.pulls[parts[1]+"/"+parts[2]]++
		s.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	case parts[0] == "v2" && len(parts) >= 4:
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			fail(w, http.StatusUnauthorized, "UNAUTHORIZED", "missing token")
			return
		}
		s.serveWrite(w, r, parts[1], parts[2], parts[3], strings.Join(parts[4:], "/"))
	default:
		fail(w, http.StatusNotFound, "NOT_FOUND", r.URL.Path)
	}
}

func (s *Server) serveCDN(w http.ResponseWriter, r *http.Request, key string) {
	s.mu.Lock()
	data, ok := s.objects[key]
	s.mu.Unlock()
	if !ok {
		http.NotFound(w, r)
		return
	}
	if rg := r.Header.Get("Range"); rg != "" {
		var a, b int
		if _, err := fmt.Sscanf(rg, "bytes=%d-%d", &a, &b); err != nil || a > b || b >= len(data) {
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			return
		}
		w.Header().Set("Content-Range", "bytes "+strconv.Itoa(a)+"-"+strconv.Itoa(b)+"/"+strconv.Itoa(len(data)))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(data[a : b+1])
		return
	}
	_, _ = w.Write(data)
}

func (s *Server) serveWrite(w http.ResponseWriter, r *http.Request, ns, repo, kind, arg string) {
	body, _ := io.ReadAll(r.Body)
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case kind == "blobs" && arg == "missing":
		var req struct {
			Digests []string `json:"digests"`
		}
		_ = json.Unmarshal(body, &req)
		missing := []string{}
		for _, d := range req.Digests {
			if _, ok := s.locations[d]; !ok {
				missing = append(missing, d)
			}
		}
		_ = json.NewEncoder(w).Encode(map[string][]string{"missing": missing})
	case kind == "packs" && r.Method == http.MethodPut:
		p, err := pack.DecodeUpload(body)
		if err != nil || p.Digest != "sha256:"+arg {
			fail(w, http.StatusBadRequest, "PACK_INVALID", fmt.Sprint(err))
			return
		}
		s.objects["packs/"+arg] = p.Data
		for _, e := range p.Entries {
			if _, ok := s.locations[e.Digest]; !ok {
				s.locations[e.Digest] = pack.Location{Pack: p.Digest, Offset: e.Offset, Length: e.Length}
			}
		}
		w.WriteHeader(http.StatusCreated)
	case kind == "recipes" && r.Method == http.MethodPut:
		sum := sha256.Sum256(body)
		if hex.EncodeToString(sum[:]) != arg {
			fail(w, http.StatusBadRequest, "RECIPE_INVALID", "digest mismatch")
			return
		}
		var rec struct {
			Blobs []struct {
				Digest string `json:"digest"`
			} `json:"blobs"`
		}
		_ = json.Unmarshal(body, &rec)
		locs := map[string]pack.Location{}
		for _, b := range rec.Blobs {
			l, ok := s.locations[b.Digest]
			if !ok {
				fail(w, http.StatusConflict, "BLOB_UNKNOWN", b.Digest)
				return
			}
			locs[b.Digest] = l
		}
		doc, _ := json.Marshal(map[string]any{"blobs": locs})
		s.objects["recipes/"+arg+".json"] = body
		s.objects["locations/"+arg+".json"] = doc
		w.WriteHeader(http.StatusCreated)
	case kind == "tags" && r.Method == http.MethodPut:
		var req struct {
			Recipe string `json:"recipe"`
		}
		_ = json.Unmarshal(body, &req)
		if _, ok := s.objects["recipes/"+strings.TrimPrefix(req.Recipe, "sha256:")+".json"]; !ok {
			fail(w, http.StatusConflict, "RECIPE_UNKNOWN", req.Recipe)
			return
		}
		doc, _ := json.Marshal(map[string]string{"recipe": req.Recipe})
		s.objects["tags/"+ns+"/"+repo+"/"+arg] = doc
		_ = json.NewEncoder(w).Encode(map[string]string{"tag": arg, "recipe": req.Recipe})
	default:
		fail(w, http.StatusNotFound, "NOT_FOUND", r.URL.Path)
	}
}

// Packs lists the stored pack keys, sorted.
func (s *Server) Packs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var keys []string
	for k := range s.objects {
		if strings.HasPrefix(k, "packs/") {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	return keys
}
