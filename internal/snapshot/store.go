package snapshot

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	// SidecarName is Jerboa's metadata file next to the fork artifact.
	SidecarName = "jerboa-snapshot.json"
	// ArtifactName is the fork snapshot directory inside a store entry.
	ArtifactName = "vm.snapshot"
	// SidecarVersion is the sidecar schema version written by this build.
	SidecarVersion = 1

	stagingDir    = ".staging"
	operationsDir = ".operations"
	maxSidecar    = 1 << 20
)

var (
	// ErrExists is returned when a snapshot name is already used or reserved.
	ErrExists = errors.New("snapshot already exists")
	// ErrNotFound is returned when a snapshot does not exist.
	ErrNotFound = errors.New("snapshot not found")
	// ErrQuota is returned when a snapshot would exceed the store limits.
	ErrQuota = errors.New("snapshot quota exceeded")
	// ErrInUse is returned when a snapshot is referenced by an operation.
	ErrInUse = errors.New("snapshot is in use")
	// ErrInvalidName is returned for names outside the resource grammar.
	ErrInvalidName = errors.New("invalid snapshot name")

	nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,62}$`)
)

// PortInfo records one published port of the snapshotted VM.
type PortInfo struct {
	HostPort  uint16 `json:"host_port"`
	GuestPort uint16 `json:"guest_port"`
	Protocol  string `json:"protocol,omitempty"`
	BindAddr  string `json:"bind_addr,omitempty"`
}

// NetworkInfo records the network identity the restore must preserve.
type NetworkInfo struct {
	Name    string   `json:"name"`
	Subnet  string   `json:"subnet"`
	Gateway string   `json:"gateway"`
	IP      string   `json:"ip"`
	MAC     string   `json:"mac"`
	Aliases []string `json:"aliases,omitempty"`
}

// Info is the sidecar written atomically beside each artifact. It never
// contains socket paths, temporary paths or environment values.
type Info struct {
	Version        int                  `json:"version"`
	Name           string               `json:"name"`
	CreatedAt      time.Time            `json:"created_at"`
	VMID           string               `json:"vm_id"`
	VMName         string               `json:"vm_name,omitempty"`
	Backend        string               `json:"backend"`
	Image          string               `json:"image,omitempty"`
	ImageDigest    string               `json:"image_digest,omitempty"`
	Memory         string               `json:"memory"`
	CPUs           int                  `json:"cpus"`
	ConfigSHA256   string               `json:"config_sha256"`
	Network        NetworkInfo          `json:"network"`
	Ports          []PortInfo           `json:"ports,omitempty"`
	ManifestSHA256 string               `json:"manifest_sha256"`
	Compatibility  json.RawMessage      `json:"compatibility"`
	Components     map[string]Component `json:"components"`
	SizeBytes      int64                `json:"size_bytes"`
}

// Limits bounds the store. Zero values disable the corresponding bound.
type Limits struct {
	MaxCount int
	MaxBytes int64
}

// Store is a daemon-private snapshot directory.
type Store struct {
	root   string
	limits Limits

	mu       sync.Mutex
	reserved map[string]int64
	inUse    map[string]int
}

// ValidateName enforces the resource grammar; names never become paths
// outside the store.
func ValidateName(name string) error {
	if !nameRE.MatchString(name) || strings.Contains(name, "..") {
		return fmt.Errorf("%w %q: use 1-63 lowercase letters, digits, '.', '_' or '-', starting with a letter or digit", ErrInvalidName, name)
	}
	return nil
}

// Open creates (if needed) and validates a private store rooted at root.
func Open(root string, limits Limits) (*Store, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("snapshot store: %w", err)
	}
	for _, dir := range []string{root, filepath.Join(root, stagingDir), filepath.Join(root, operationsDir)} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("snapshot store: %w", err)
		}
		if info, err := os.Lstat(dir); err == nil && info.IsDir() && info.Mode()&os.ModeSymlink == 0 {
			if uid, _, ok := owner(info); ok && int(uid) == os.Geteuid() {
				_ = os.Chmod(dir, 0o700)
			}
		}
		if err := privateDir(dir); err != nil {
			return nil, fmt.Errorf("snapshot store: %w", err)
		}
	}
	return &Store{root: root, limits: limits, reserved: map[string]int64{}, inUse: map[string]int{}}, nil
}

// Root returns the absolute store root.
func (s *Store) Root() string { return s.root }

// Limits returns the configured limits.
func (s *Store) Limits() Limits { return s.limits }

// RecoverStaging removes incomplete creations and interrupted removals. Call
// it at daemon start after interrupted operations have been resolved.
func (s *Store) RecoverStaging() ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	dir := filepath.Join(s.root, stagingDir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("snapshot staging: %w", err)
	}
	var removed []string
	for _, e := range entries {
		path := filepath.Join(dir, e.Name())
		if err := os.RemoveAll(path); err != nil {
			return removed, fmt.Errorf("snapshot staging cleanup %s: %w", e.Name(), err)
		}
		removed = append(removed, e.Name())
	}
	return removed, nil
}

// Reservation owns a name and a quota estimate until Publish or Abort.
type Reservation struct {
	store *Store
	name  string
	dir   string
	done  bool
}

// Reserve claims name and estimate bytes against the quota and creates a
// private staging directory for the fork to publish into.
func (s *Store) Reserve(name string, estimate int64) (*Reservation, error) {
	if err := ValidateName(name); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.reserved[name]; ok {
		return nil, fmt.Errorf("%w: %s", ErrExists, name)
	}
	if _, err := os.Lstat(filepath.Join(s.root, name)); err == nil {
		return nil, fmt.Errorf("%w: %s", ErrExists, name)
	}
	count, used, err := s.usageLocked()
	if err != nil {
		return nil, err
	}
	var pending int64
	for _, b := range s.reserved {
		pending += b
	}
	if s.limits.MaxCount > 0 && count+len(s.reserved)+1 > s.limits.MaxCount {
		return nil, fmt.Errorf("%w: at most %d snapshots", ErrQuota, s.limits.MaxCount)
	}
	if s.limits.MaxBytes > 0 && used+pending+estimate > s.limits.MaxBytes {
		return nil, fmt.Errorf("%w: %d bytes used, %d reserved, about %d required, limit %d", ErrQuota, used, pending, estimate, s.limits.MaxBytes)
	}
	token := make([]byte, 6)
	if _, err := rand.Read(token); err != nil {
		return nil, fmt.Errorf("snapshot reserve: %w", err)
	}
	dir := filepath.Join(s.root, stagingDir, name+"-"+hex.EncodeToString(token))
	if err := os.Mkdir(dir, 0o700); err != nil {
		return nil, fmt.Errorf("snapshot reserve: %w", err)
	}
	s.reserved[name] = estimate
	return &Reservation{store: s, name: name, dir: dir}, nil
}

// ArtifactPath is the absent destination the fork must publish to.
func (r *Reservation) ArtifactPath() string { return filepath.Join(r.dir, ArtifactName) }

// Abort removes staged data and releases the reservation. It is idempotent.
func (r *Reservation) Abort() {
	r.store.mu.Lock()
	defer r.store.mu.Unlock()
	if r.done {
		return
	}
	r.done = true
	delete(r.store.reserved, r.name)
	_ = os.RemoveAll(r.dir)
}

// Publish verifies the staged artifact, enforces the quota with its real
// size, writes the sidecar atomically and exposes the entry under its name.
// info supplies the VM identity; integrity fields are filled here. check, when
// non-nil, validates the verified artifact against the caller's VM before the
// entry becomes visible.
func (r *Reservation) Publish(info Info, check func(Artifact) error) (Info, error) {
	artifact, err := VerifyArtifact(r.ArtifactPath())
	if err == nil && check != nil {
		err = check(artifact)
	}
	if err != nil {
		r.Abort()
		return Info{}, err
	}
	info.Version = SidecarVersion
	info.Name = r.name
	info.ManifestSHA256 = artifact.ManifestSHA256
	info.Compatibility = artifact.Manifest.Compatibility
	info.Components = artifact.Manifest.Components
	info.SizeBytes = artifact.SizeBytes

	s := r.store
	s.mu.Lock()
	defer s.mu.Unlock()
	if r.done {
		return Info{}, fmt.Errorf("snapshot %s: reservation already released", r.name)
	}
	fail := func(err error) error {
		r.done = true
		delete(s.reserved, r.name)
		_ = os.RemoveAll(r.dir)
		return err
	}
	_, used, err := s.usageLocked()
	if err != nil {
		return Info{}, fail(err)
	}
	var others int64
	for name, b := range s.reserved {
		if name != r.name {
			others += b
		}
	}
	if s.limits.MaxBytes > 0 && used+others+info.SizeBytes > s.limits.MaxBytes {
		return Info{}, fail(fmt.Errorf("%w: captured snapshot is %d bytes; %d bytes used, limit %d", ErrQuota, info.SizeBytes, used, s.limits.MaxBytes))
	}
	data, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		return Info{}, fail(err)
	}
	if err := writeFileSync(filepath.Join(r.dir, SidecarName), data); err != nil {
		return Info{}, fail(err)
	}
	dest := filepath.Join(s.root, r.name)
	if _, err := os.Lstat(dest); err == nil {
		return Info{}, fail(fmt.Errorf("%w: %s", ErrExists, r.name))
	}
	if err := os.Rename(r.dir, dest); err != nil {
		return Info{}, fail(fmt.Errorf("publish snapshot %s: %w", r.name, err))
	}
	syncDir(s.root)
	r.done = true
	delete(s.reserved, r.name)
	return info, nil
}

// Get reads and validates a sidecar without hashing the artifact.
func (s *Store) Get(name string) (Info, error) {
	if err := ValidateName(name); err != nil {
		return Info{}, err
	}
	return s.readInfo(name)
}

func (s *Store) readInfo(name string) (Info, error) {
	dir := filepath.Join(s.root, name)
	if _, err := os.Lstat(dir); errors.Is(err, fs.ErrNotExist) {
		return Info{}, fmt.Errorf("%w: %s", ErrNotFound, name)
	}
	if err := privateDir(dir); err != nil {
		return Info{}, err
	}
	data, err := readPrivateFile(filepath.Join(dir, SidecarName), maxSidecar)
	if err != nil {
		return Info{}, err
	}
	var info Info
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&info); err != nil {
		return Info{}, integrity("parse sidecar: %v", err)
	}
	if info.Version != SidecarVersion || info.Name != name {
		return Info{}, integrity("sidecar version or name mismatch")
	}
	return info, nil
}

// Verify re-validates permissions and every component digest against both
// the manifest and the sidecar. It returns the artifact path for the loader.
func (s *Store) Verify(name string) (Info, Artifact, string, error) {
	info, err := s.Get(name)
	if err != nil {
		return Info{}, Artifact{}, "", err
	}
	path := filepath.Join(s.root, name, ArtifactName)
	artifact, err := VerifyArtifact(path)
	if err != nil {
		return Info{}, Artifact{}, "", err
	}
	entries, err := os.ReadDir(filepath.Join(s.root, name))
	if err != nil {
		return Info{}, Artifact{}, "", integrity("read snapshot: %v", err)
	}
	for _, e := range entries {
		if e.Name() != SidecarName && e.Name() != ArtifactName {
			return Info{}, Artifact{}, "", integrity("unexpected snapshot entry %q", e.Name())
		}
	}
	if artifact.ManifestSHA256 != info.ManifestSHA256 || artifact.SizeBytes != info.SizeBytes || len(artifact.Manifest.Components) != len(info.Components) {
		return Info{}, Artifact{}, "", integrity("artifact does not match its sidecar")
	}
	for k, c := range artifact.Manifest.Components {
		if info.Components[k] != c {
			return Info{}, Artifact{}, "", integrity("component %q does not match its sidecar", k)
		}
	}
	return info, artifact, path, nil
}

// Entry is one List result; Err is set for entries that fail validation.
type Entry struct {
	Info Info
	Err  error
}

// List returns every published entry, sorted by name.
func (s *Store) List() ([]Entry, error) {
	entries, err := os.ReadDir(s.root)
	if err != nil {
		return nil, fmt.Errorf("list snapshots: %w", err)
	}
	var out []Entry
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			continue
		}
		info, err := s.Get(e.Name())
		if err != nil {
			info = Info{Name: e.Name()}
		}
		out = append(out, Entry{Info: info, Err: err})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Info.Name < out[j].Info.Name })
	return out, nil
}

// Acquire marks name as in use until release is called.
func (s *Store) Acquire(name string) (func(), error) {
	if err := ValidateName(name); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := os.Lstat(filepath.Join(s.root, name)); err != nil {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, name)
	}
	s.inUse[name]++
	var once sync.Once
	return func() {
		once.Do(func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			if s.inUse[name]--; s.inUse[name] <= 0 {
				delete(s.inUse, name)
			}
		})
	}, nil
}

// Remove deletes a validated store entry that no operation references. The
// entry is first moved into staging so a crash never exposes a partial one.
func (s *Store) Remove(name string) error {
	if err := ValidateName(name); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	dir := filepath.Join(s.root, name)
	if _, err := os.Lstat(dir); errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("%w: %s", ErrNotFound, name)
	}
	if s.inUse[name] > 0 {
		return fmt.Errorf("%w: %s", ErrInUse, name)
	}
	ops, err := s.operationsLocked()
	if err != nil {
		return err
	}
	for _, op := range ops {
		if op.Snapshot == name && op.Kind == OperationRestore {
			return fmt.Errorf("%w: %s is referenced by an interrupted restore of VM %s", ErrInUse, name, op.VMID)
		}
	}
	if err := privateDir(dir); err != nil {
		return err
	}
	token := make([]byte, 6)
	if _, err := rand.Read(token); err != nil {
		return fmt.Errorf("snapshot store operation: %w", err)
	}
	trash := filepath.Join(s.root, stagingDir, "removing-"+name+"-"+hex.EncodeToString(token))
	if err := os.Rename(dir, trash); err != nil {
		return fmt.Errorf("remove snapshot %s: %w", name, err)
	}
	syncDir(s.root)
	if err := os.RemoveAll(trash); err != nil {
		slog.Warn("snapshot remove: staging cleanup deferred to next start", "name", name, "err", err)
	}
	return nil
}

// usageLocked counts published entries and measures real on-disk bytes
// (regular files only) rather than trusting sidecars.
func (s *Store) usageLocked() (int, int64, error) {
	entries, err := os.ReadDir(s.root)
	if err != nil {
		return 0, 0, fmt.Errorf("snapshot usage: %w", err)
	}
	count, total := 0, int64(0)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			continue
		}
		count++
		_ = filepath.WalkDir(filepath.Join(s.root, e.Name()), func(_ string, d fs.DirEntry, err error) error {
			if err != nil || !d.Type().IsRegular() {
				return nil //nolint:nilerr // unreadable entries are counted by name only
			}
			if info, err := d.Info(); err == nil {
				total += info.Size()
			}
			return nil
		})
	}
	return count, total, nil
}

// Usage reports the number of published entries and their on-disk bytes.
func (s *Store) Usage() (int, int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.usageLocked()
}

func writeFileSync(path string, data []byte) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) //nolint:gosec // private store path
	if err != nil {
		return fmt.Errorf("snapshot store operation: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("snapshot store operation: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("snapshot store operation: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("snapshot store operation: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("snapshot store operation: %w", err)
	}
	syncDir(filepath.Dir(path))
	return nil
}

func syncDir(path string) {
	if d, err := os.Open(path); err == nil { //nolint:gosec // private store path
		_ = d.Sync()
		_ = d.Close()
	}
}
