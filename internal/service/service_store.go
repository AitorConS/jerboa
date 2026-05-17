package service

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// FileStore persists services to disk as JSON files under a root directory.
type FileStore struct {
	root string
	mu   sync.RWMutex
}

// NewFileStore creates a new FileStore rooted at dir.
func NewFileStore(dir string) (*FileStore, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("service store mkdir %s: %w", dir, err)
	}
	return &FileStore{root: dir}, nil
}

// Save writes a service to disk.
func (s *FileStore) Save(svc *Service) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	dir := filepath.Join(s.root, svc.Name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("service store save mkdir %s: %w", dir, err)
	}

	data, err := json.MarshalIndent(svc, "", "  ")
	if err != nil {
		return fmt.Errorf("service store marshal: %w", err)
	}

	path := filepath.Join(dir, "service.json")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("service store write %s: %w", path, err)
	}
	return nil
}

// Get reads a service by name from disk.
func (s *FileStore) Get(name string) (*Service, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	path := filepath.Join(s.root, name, "service.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("service store get %s: %w", name, err)
	}

	var svc Service
	if err := json.Unmarshal(data, &svc); err != nil {
		return nil, fmt.Errorf("service store parse %s: %w", name, err)
	}
	return &svc, nil
}

// List returns all services from disk.
func (s *FileStore) List() ([]*Service, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	entries, err := os.ReadDir(s.root)
	if err != nil {
		return nil, fmt.Errorf("service store list: %w", err)
	}

	var result []*Service
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		path := filepath.Join(s.root, e.Name(), "service.json")
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var svc Service
		if err := json.Unmarshal(data, &svc); err != nil {
			continue
		}
		result = append(result, &svc)
	}
	return result, nil
}

// Delete removes a service directory from disk.
func (s *FileStore) Delete(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	dir := filepath.Join(s.root, name)
	if err := os.RemoveAll(dir); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("service store delete %s: %w", name, err)
	}
	return nil
}