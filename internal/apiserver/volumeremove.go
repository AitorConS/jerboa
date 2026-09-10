//go:build linux || (darwin && arm64)

package apiserver

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/AitorConS/jerboa/internal/api"
	"github.com/AitorConS/jerboa/internal/volume"
)

func (s *Server) volumeUnused(disk string) error {
	target, err := os.Stat(disk)
	if err != nil {
		return fmt.Errorf("volume stat: %w", err)
	}
	for _, v := range s.mgr.List() {
		for _, mount := range v.Cfg.Volumes {
			mounted, err := os.Stat(mount.DiskPath)
			if filepath.Clean(mount.DiskPath) == filepath.Clean(disk) || (err == nil && os.SameFile(target, mounted)) {
				return fmt.Errorf("volume is referenced by VM %s; remove the VM first", v.ID)
			}
		}
	}
	return nil
}
func (s *Server) handleVolumeRemove(raw json.RawMessage) (any, *api.RPCError) {
	s.resourceMu.Lock()
	defer s.resourceMu.Unlock()
	var p api.VolumeRemoveParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, &api.RPCError{Code: -32602, Message: err.Error()}
	}
	fail := func(err error) *api.RPCError { return &api.RPCError{Code: -32000, Message: err.Error()} }
	if err := s.volumeUnused(p.DiskPath); err != nil {
		return nil, fail(err)
	}
	store, err := volume.NewStore(filepath.Dir(filepath.Dir(p.DiskPath)))
	if err != nil {
		return nil, fail(err)
	}
	vol, err := store.Get(p.Name)
	if err != nil {
		return nil, fail(err)
	}
	if filepath.Clean(vol.DiskPath) != filepath.Clean(p.DiskPath) {
		return nil, fail(fmt.Errorf("volume path mismatch"))
	}
	if err := store.Remove(p.Name); err != nil {
		return nil, fail(err)
	}
	return map[string]string{"status": "ok"}, nil
}
