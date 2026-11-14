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
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/AitorConS/jerboa/internal/durable"
	"github.com/AitorConS/jerboa/internal/naming"
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
	if err := naming.ValidateResourceName("volume", name); err != nil {
		return nil, err
	}
	if sizeBytes <= 0 {
		sizeBytes = defaultSizeBytes
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	dir, err := s.volumeDir(name)
	if err != nil {
		return nil, err
	}
	if _, statErr := os.Stat(dir); statErr == nil {
		return nil, fmt.Errorf("volume %q already exists", name)
	}

	// The volume is assembled in a staging directory and renamed into place,
	// so it exists complete or not at all: a full disk or a power cut half-way
	// cannot leave a name that is taken but holds no usable volume.
	stage, err := os.MkdirTemp(s.root, stagingPrefix+"create-")
	if err != nil {
		return nil, fmt.Errorf("volume create dir %s: %w", dir, err)
	}
	defer func() { _ = os.RemoveAll(stage) }()

	if err := allocate(filepath.Join(stage, "disk.img"), sizeBytes); err != nil {
		return nil, fmt.Errorf("volume create disk %s: %w", filepath.Join(dir, "disk.img"), err)
	}

	v := &Volume{
		ID:        name,
		DiskPath:  filepath.Join(dir, "disk.img"),
		SizeBytes: sizeBytes,
		CreatedAt: time.Now().UTC(),
		Label:     SanitizeLabel(name),
	}
	if err := writeMeta(stage, v); err != nil {
		return nil, fmt.Errorf("volume write meta: %w", err)
	}
	if err := os.Rename(stage, dir); err != nil {
		return nil, fmt.Errorf("volume create dir %s: %w", dir, err)
	}
	durable.SyncDir(s.root)
	return v, nil
}

// stagingPrefix starts the name of every directory a volume is assembled in
// before it is published. Volume names cannot start with a dot, so a staging
// directory is never taken for a volume.
const stagingPrefix = "."

// SweepStaging removes the staging directories that interrupted creations and
// imports left behind and returns how many it removed and the bytes they
// occupied. Only call it while no creation or import can be running.
func (s *Store) SweepStaging() (count int, bytes int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, dir := range s.stagingDirs() {
		size := dirAllocated(dir)
		if os.RemoveAll(dir) == nil {
			count++
			bytes += size
		}
	}
	return count, bytes
}

// Staging returns the staging directories currently in the store.
func (s *Store) Staging() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.stagingDirs()
}

func (s *Store) stagingDirs() []string {
	entries, err := os.ReadDir(s.root)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() && (strings.HasPrefix(e.Name(), stagingPrefix+"create-") || strings.HasPrefix(e.Name(), stagingPrefix+"import-")) {
			out = append(out, filepath.Join(s.root, e.Name()))
		}
	}
	return out
}

func dirAllocated(dir string) int64 {
	var total int64
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	for _, e := range entries {
		if info, err := e.Info(); err == nil && !e.IsDir() {
			total += info.Size()
		}
	}
	return total
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
		if !e.IsDir() || e.Name()[0] == '.' {
			continue
		}
		v, err := s.readMeta(e.Name())
		if err != nil {
			slog.Warn("volume store: skipping an unreadable volume", "name", e.Name(), "err", err)
			continue
		}
		out = append(out, v)
	}
	return out, nil
}

// Remove deletes the volume and its disk image.
func (s *Store) Remove(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	dir, err := s.volumeDir(name)
	if err != nil {
		return err
	}
	if _, statErr := os.Stat(dir); os.IsNotExist(statErr) {
		return fmt.Errorf("volume %q not found", name)
	}
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("volume remove %s: %w", name, err)
	}
	return nil
}

func (s *Store) volumeDir(name string) (string, error) {
	if err := naming.ValidateResourceName("volume", name); err != nil {
		return "", err
	}
	return naming.SafeJoin(s.root, name)
}

func (s *Store) readMeta(name string) (*Volume, error) {
	dir, err := s.volumeDir(name)
	if err != nil {
		return nil, err
	}
	path := filepath.Join(dir, metaFile)
	disk := filepath.Join(dir, "disk.img")
	data, err := os.ReadFile(path)
	if err == nil {
		var v Volume
		if uerr := json.Unmarshal(data, &v); uerr == nil && v.ID != "" {
			v.DiskPath = disk
			return &v, nil
		} else if uerr != nil {
			err = fmt.Errorf("corrupt meta: %w", uerr)
		} else {
			err = fmt.Errorf("corrupt meta: no volume id")
		}
	}
	// The metadata is a few lines describing the disk beside it; the disk is
	// the user's data. Losing the former must not cost access to the latter,
	// so it is written again from what the disk itself says.
	info, serr := os.Stat(disk)
	if serr != nil || !info.Mode().IsRegular() {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("volume %q not found: %w", name, err)
		}
		return nil, fmt.Errorf("volume %q %w", name, err)
	}
	v := &Volume{ID: name, DiskPath: disk, SizeBytes: info.Size(), CreatedAt: info.ModTime().UTC(), Label: SanitizeLabel(name)}
	slog.Warn("volume store: metadata unreadable, rebuilt from the disk", "name", name, "err", err)
	if werr := writeMeta(dir, v); werr != nil {
		slog.Warn("volume store: could not save the rebuilt metadata", "name", name, "err", werr)
	}
	return v, nil
}

func writeMeta(dir string, v *Volume) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal volume meta: %w", err)
	}
	if err := durable.WriteFile(filepath.Join(dir, metaFile), data, 0o600); err != nil {
		return fmt.Errorf("write volume meta: %w", err)
	}
	return nil
}

// allocate is allocateDisk, replaceable so tests can make allocation fail.
var allocate = allocateDisk

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
