package volume

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/AitorConS/jerboa/internal/naming"
)

// Import copies a quiescent raw disk into the store. The destination only becomes
// visible once the complete image and metadata have been written. Existing
// volumes are never replaced, and zero-filled regions remain sparse.
func (s *Store) Import(ctx context.Context, name, label string, size int64, src io.Reader) (*Volume, error) {
	dest, err := s.volumeDir(name)
	if err != nil {
		return nil, err
	}
	if size <= 0 || len(label) > maxLabelLen {
		return nil, fmt.Errorf("invalid volume size or label")
	}
	if err := naming.ValidateResourceName("volume label", label); err != nil {
		return nil, err
	}
	stage, err := os.MkdirTemp(s.root, ".import-")
	if err != nil {
		return nil, fmt.Errorf("create import staging directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(stage) }()
	f, err := os.OpenFile(filepath.Join(stage, "disk.img"), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("create import disk: %w", err)
	}
	defer func() { _ = f.Close() }()
	buf, zero := make([]byte, 1<<20), make([]byte, 1<<20)
	for remaining := size; remaining > 0; {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("volume import canceled: %w", err)
		}
		n := int64(len(buf))
		if remaining < n {
			n = remaining
		}
		if _, err := io.ReadFull(src, buf[:n]); err != nil {
			return nil, fmt.Errorf("incomplete volume: %w", err)
		}
		if bytes.Equal(buf[:n], zero[:n]) {
			_, err = f.Seek(n, io.SeekCurrent)
		} else {
			_, err = f.Write(buf[:n])
		}
		if err != nil {
			return nil, fmt.Errorf("write imported disk: %w", err)
		}
		remaining -= n
	}
	var extra [1]byte
	if n, err := io.ReadFull(src, extra[:]); n != 0 || err != io.EOF {
		return nil, fmt.Errorf("volume stream exceeds declared size or has no end marker")
	}
	if err := f.Truncate(size); err != nil {
		return nil, fmt.Errorf("set imported disk size: %w", err)
	}
	if err := f.Sync(); err != nil {
		return nil, fmt.Errorf("sync imported disk: %w", err)
	}
	if err := f.Close(); err != nil {
		return nil, fmt.Errorf("close imported disk: %w", err)
	}
	v := &Volume{ID: name, Label: label, SizeBytes: size, CreatedAt: time.Now().UTC(), DiskPath: filepath.Join(dest, "disk.img")}
	if err := writeMeta(stage, v); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("volume import canceled: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := os.Stat(dest); err == nil {
		return nil, fmt.Errorf("volume %q already exists", name)
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("stat import destination: %w", err)
	}
	if err := os.Rename(stage, dest); err != nil {
		return nil, fmt.Errorf("publish imported volume: %w", err)
	}
	return v, nil
}
