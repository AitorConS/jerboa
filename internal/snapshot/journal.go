package snapshot

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Operation kinds recorded in the durable journal.
const (
	OperationCreate  = "create"
	OperationRestore = "restore"
)

var vmIDRE = regexp.MustCompile(`^[0-9a-f]{12}$`)

// Operation is the durable record of a create or restore in progress. It is
// written before each externally visible effect so daemon start-up can finish
// the rollback or adopt the result. At most one operation exists per VM.
type Operation struct {
	Version   int       `json:"version"`
	Kind      string    `json:"kind"`
	VMID      string    `json:"vm_id"`
	Snapshot  string    `json:"snapshot"`
	Phase     string    `json:"phase"`
	PID       int       `json:"pid,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (s *Store) operationPath(vmID string) (string, error) {
	if !vmIDRE.MatchString(vmID) {
		return "", fmt.Errorf("invalid VM ID %q for snapshot journal", vmID)
	}
	return filepath.Join(s.root, operationsDir, vmID+".json"), nil
}

// WriteOperation durably replaces the journal record for op.VMID.
func (s *Store) WriteOperation(op Operation) error {
	path, err := s.operationPath(op.VMID)
	if err != nil {
		return err
	}
	if op.Kind != OperationCreate && op.Kind != OperationRestore {
		return fmt.Errorf("invalid snapshot operation kind %q", op.Kind)
	}
	op.Version = 1
	op.UpdatedAt = time.Now().UTC()
	data, err := json.Marshal(op)
	if err != nil {
		return fmt.Errorf("journal operation: %w", err)
	}
	_ = os.Remove(path + ".tmp")
	if err := writeFileSync(path, data); err != nil {
		return fmt.Errorf("snapshot journal: %w", err)
	}
	return nil
}

// RemoveOperation deletes the journal record for vmID. Missing is not an error.
func (s *Store) RemoveOperation(vmID string) error {
	path, err := s.operationPath(vmID)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("snapshot journal: %w", err)
	}
	syncDir(filepath.Dir(path))
	return nil
}

// Operations returns every journal record. Unreadable records are errors:
// recovery must not silently ignore an interrupted lifecycle.
func (s *Store) Operations() ([]Operation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.operationsLocked()
}

func (s *Store) operationsLocked() ([]Operation, error) {
	dir := filepath.Join(s.root, operationsDir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("snapshot journal: %w", err)
	}
	var out []Operation
	for _, e := range entries {
		name := e.Name()
		if strings.HasSuffix(name, ".tmp") {
			_ = os.Remove(filepath.Join(dir, name))
			continue
		}
		if !strings.HasSuffix(name, ".json") || !vmIDRE.MatchString(strings.TrimSuffix(name, ".json")) {
			return nil, integrity("unexpected journal entry %q", name)
		}
		data, err := readPrivateFile(filepath.Join(dir, name), maxSidecar)
		if err != nil {
			return nil, err
		}
		var op Operation
		dec := json.NewDecoder(bytes.NewReader(data))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&op); err != nil || op.Version != 1 || op.VMID+".json" != name {
			return nil, integrity("invalid journal entry %q", name)
		}
		out = append(out, op)
	}
	return out, nil
}
