// Package volume manages persistent virtio-blk disk volumes for unikernel VMs.
// A volume is a raw disk image file in the volume root directory, identified by
// a human-readable name. Volumes survive VM restarts; they are only deleted
// when the user explicitly runs "jerboa volume rm".
package volume

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"
)

const (
	defaultSizeBytes = 1 << 30 // 1 GiB
	metaFile         = "meta.json"
	// maxLabelLen mirrors the kernel's VOLUME_LABEL_MAX_LEN (32, NUL-terminated),
	// so the usable label length is 31 bytes.
	maxLabelLen = 31
)

// tfsMagic is the 6-byte signature at offset 0 of a TFS-formatted volume
// (kernel src/fs/tlog.c). A freshly allocated (zero-filled) disk lacks it, so a
// magic check is a cheap, reliable "is this volume already formatted?" probe.
var tfsMagic = []byte("NVMTFS")

// Formatter formats the raw disk at diskPath as an empty TFS filesystem labeled
// label with a minimum size of sizeBytes. Implemented by internal/tools using
// the mkfs toolchain; the daemon supplies one at run time (mkfs is Linux-only,
// so formatting cannot happen on a Windows client at create time).
type Formatter func(ctx context.Context, diskPath, label string, sizeBytes int64) error

// Seeder formats the raw disk at diskPath as a TFS filesystem labeled label and
// populated with the files described by manifest (a children-only Nanos
// manifest, e.g. from image.BuildVolumeManifest). Implemented by internal/tools
// via mkfs; the daemon supplies one at run time. sizeBytes is the minimum image
// size. Unlike Formatter, which produces an empty volume, a Seeder pre-populates
// the volume so initialized data (a database cluster, seed files) is present the
// first time it is mounted.
type Seeder func(ctx context.Context, diskPath, label string, sizeBytes int64, manifest string) error

// SanitizeLabel trims name to a valid TFS label (<= maxLabelLen bytes). The
// kernel's volume_match compares the label verbatim, so it must equal the value
// injected into the guest mount config.
func SanitizeLabel(name string) string {
	if len(name) > maxLabelLen {
		return name[:maxLabelLen]
	}
	return name
}

// Volume is a named persistent disk image.
type Volume struct {
	// ID is the volume name (unique within the store).
	ID string `json:"id"`
	// DiskPath is the absolute path to the raw disk image file.
	DiskPath string `json:"disk_path"`
	// SizeBytes is the allocated size of the disk image.
	SizeBytes int64 `json:"size_bytes"`
	// CreatedAt is when the volume was created.
	CreatedAt time.Time `json:"created_at"`
	// Label is the TFS filesystem label written when the volume is formatted.
	// It defaults to the volume ID and is the key the guest kernel matches to
	// mount this disk. Capped to maxLabelLen.
	Label string `json:"label,omitempty"`
}

// Store manages volumes on disk.
type Store struct {
	root string
	mu   sync.RWMutex
}

// NewStore returns a Store rooted at dir, creating it if needed.
func NewStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("volume store mkdir %s: %w", dir, err)
	}
	return &Store{root: dir}, nil
}

// Create allocates a new volume named name with the given size in bytes.
// If sizeBytes is 0 the default (1 GiB) is used.
func (s *Store) Create(name string, sizeBytes int64) (*Volume, error) {
	if name == "" {
		return nil, fmt.Errorf("volume name must not be empty")
	}
	if sizeBytes <= 0 {
		sizeBytes = defaultSizeBytes
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	dir := s.volumeDir(name)
	if _, err := os.Stat(dir); err == nil {
		return nil, fmt.Errorf("volume %q already exists", name)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("volume create dir %s: %w", dir, err)
	}

	diskPath := filepath.Join(dir, "disk.img")
	if err := allocateDisk(diskPath, sizeBytes); err != nil {
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("volume create disk %s: %w", diskPath, err)
	}

	v := &Volume{
		ID:        name,
		DiskPath:  diskPath,
		SizeBytes: sizeBytes,
		CreatedAt: time.Now().UTC(),
		Label:     SanitizeLabel(name),
	}
	if err := writeMeta(dir, v); err != nil {
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("volume write meta: %w", err)
	}
	return v, nil
}

// Get returns the volume with the given name.
func (s *Store) Get(name string) (*Volume, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.readMeta(name)
}

// List returns all volumes in the store.
func (s *Store) List() ([]*Volume, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	entries, err := os.ReadDir(s.root)
	if err != nil {
		return nil, fmt.Errorf("volume list: %w", err)
	}
	out := make([]*Volume, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		v, err := s.readMeta(e.Name())
		if err != nil {
			continue // skip corrupt entries
		}
		out = append(out, v)
	}
	return out, nil
}

// Remove deletes the volume and its disk image.
func (s *Store) Remove(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	dir := s.volumeDir(name)
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		return fmt.Errorf("volume %q not found", name)
	}
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("volume remove %s: %w", name, err)
	}
	return nil
}

func (s *Store) volumeDir(name string) string {
	return filepath.Join(s.root, name)
}

func (s *Store) readMeta(name string) (*Volume, error) {
	path := filepath.Join(s.volumeDir(name), metaFile)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("volume %q not found: %w", name, err)
	}
	var v Volume
	if err := json.Unmarshal(data, &v); err != nil {
		return nil, fmt.Errorf("volume %q corrupt meta: %w", name, err)
	}
	return &v, nil
}

func writeMeta(dir string, v *Volume) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal volume meta: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, metaFile), data, 0o600); err != nil {
		return fmt.Errorf("write volume meta: %w", err)
	}
	return nil
}

// allocateDisk creates a sparse raw disk image of exactly sizeBytes.
func allocateDisk(path string, sizeBytes int64) error {
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("create disk file: %w", err)
	}
	defer func() { _ = f.Close() }()
	// Seek to end-1 and write a zero byte produces a sparse file on most
	// filesystems; on Windows it pre-allocates via SetEndOfFile.
	if _, err := f.Seek(sizeBytes-1, 0); err != nil {
		return fmt.Errorf("seek disk file: %w", err)
	}
	if _, err := f.Write([]byte{0}); err != nil {
		return fmt.Errorf("write disk tail: %w", err)
	}
	return nil
}

// IsFormatted reports whether the disk at path already carries a TFS
// filesystem (its first bytes match the TFS magic). A missing file or a
// freshly allocated zero-filled disk reports false.
func IsFormatted(path string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("open volume %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	head := make([]byte, len(tfsMagic))
	n, err := f.Read(head)
	if err != nil || n < len(tfsMagic) {
		return false, nil // too short to be formatted; treat as unformatted
	}
	return bytes.Equal(head, tfsMagic), nil
}

// EnsureFormatted formats the disk at diskPath as an empty TFS volume labeled
// label if it is not already formatted. It is idempotent: a volume that already
// carries a TFS filesystem is left untouched so existing data is preserved.
// sizeBytes is the minimum image size; when <= 0 the current file size is used.
// format must be non-nil (the mkfs toolchain is supplied by the daemon).
func EnsureFormatted(ctx context.Context, diskPath, label string, sizeBytes int64, format Formatter) error {
	formatted, err := IsFormatted(diskPath)
	if err != nil {
		return err
	}
	if formatted {
		return nil
	}
	if format == nil {
		return fmt.Errorf("volume %s not formatted and no formatter available", diskPath)
	}
	if sizeBytes <= 0 {
		if st, statErr := os.Stat(diskPath); statErr == nil {
			sizeBytes = st.Size()
		}
	}
	if label == "" {
		return fmt.Errorf("volume %s: empty label", diskPath)
	}
	if err := format(ctx, diskPath, label, sizeBytes); err != nil {
		return fmt.Errorf("format volume %s: %w", diskPath, err)
	}
	return nil
}

// ParseSize parses a human size string ("512M", "1G", "2048K") into bytes.
// Supported suffixes: K (kibibytes), M (mebibytes), G (gibibytes).
// A bare integer is interpreted as bytes. Unknown suffixes are an error.
func ParseSize(s string) (int64, error) {
	if s == "" {
		return 0, nil
	}
	suffixes := map[byte]int64{'K': 1 << 10, 'M': 1 << 20, 'G': 1 << 30}
	last := s[len(s)-1]
	if mul, ok := suffixes[last]; ok {
		n, err := strconv.ParseInt(s[:len(s)-1], 10, 64)
		if err != nil {
			return 0, fmt.Errorf("invalid size %q", s)
		}
		return n * mul, nil
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid size %q", s)
	}
	return n, nil
}
