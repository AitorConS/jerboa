package pkg

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ContentDigestPrefix is the algorithm prefix of a package content digest.
const ContentDigestPrefix = "sha256:"

// contentEntry is one regular file as it lands in the image: its guest path,
// whether it is executable, and the SHA-256 of its bytes.
type contentEntry struct {
	name string
	exec bool
	sum  [32]byte
}

// digestEntries returns the content digest of entries: SHA-256 over one line
// per file, "<mode> <sha256> <path>\n", sorted by path. Modes are normalized
// to 755 or 644, the only distinction an archive keeps.
func digestEntries(entries []contentEntry) string {
	sorted := append([]contentEntry(nil), entries...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].name < sorted[j].name })
	h := sha256.New()
	for _, e := range sorted {
		mode := "644"
		if e.exec {
			mode = "755"
		}
		_, _ = fmt.Fprintf(h, "%s %s %s\n", mode, hex.EncodeToString(e.sum[:]), e.name)
	}
	return ContentDigestPrefix + hex.EncodeToString(h.Sum(nil))
}

// ContentDigest identifies a package by the files it places in an image rather
// than by the bytes of its archive, so recompressing or repacking the same
// files keeps the same identity. Directories are not part of it: extraction
// does not keep them as entries.
func ContentDigest(files []File) (string, error) {
	entries := make([]contentEntry, 0, len(files))
	for _, f := range files {
		if f.IsDir {
			continue
		}
		info, err := os.Stat(f.HostPath)
		if err != nil {
			return "", fmt.Errorf("content digest: %w", err)
		}
		if info.IsDir() {
			continue
		}
		name := tarEntryName(f.GuestPath)
		if name == "" {
			return "", fmt.Errorf("content digest: file has an empty guest path (host %q)", f.HostPath)
		}
		sum, err := fileDigest(f.HostPath)
		if err != nil {
			return "", fmt.Errorf("content digest: %w", err)
		}
		entries = append(entries, contentEntry{name: name, exec: info.Mode().Perm()&0o111 != 0, sum: sum})
	}
	return digestEntries(entries), nil
}

// ValidContentDigest reports whether d is a well-formed content digest.
func ValidContentDigest(d string) bool {
	hexPart, ok := strings.CutPrefix(d, ContentDigestPrefix)
	if !ok || len(hexPart) != 64 {
		return false
	}
	_, err := hex.DecodeString(hexPart)
	return err == nil
}

// archiveTime is the modification time written into package archives and the
// creation time of new packages: SOURCE_DATE_EPOCH when set, so rebuilding the
// same files gives a byte-identical archive. Without it archive entries use
// the Unix epoch and the package records the current time.
func archiveTime() (entries, created time.Time) {
	if v := strings.TrimSpace(os.Getenv("SOURCE_DATE_EPOCH")); v != "" {
		if secs, err := strconv.ParseInt(v, 10, 64); err == nil && secs >= 0 {
			t := time.Unix(secs, 0).UTC()
			return t, t
		}
	}
	return time.Unix(0, 0).UTC(), time.Now().UTC()
}
