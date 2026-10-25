package pkg

import (
	"fmt"
	"net"
	"net/url"
	"regexp"
)

// maxIndexSize bounds the package index read into memory.
const maxIndexSize = 32 << 20

// maxOpsPackageSize bounds an ops package download, whose index gives no size.
const maxOpsPackageSize = 4 << 30

var sha256HexRE = regexp.MustCompile(`^[0-9a-fA-F]{64}$`)

// checkSource refuses an index entry whose archive could not be verified once
// downloaded: every archive must name its SHA-256 and size and be fetched over
// https. Plain http is accepted only from loopback, for local mirrors and tests.
func (p Package) checkSource() error {
	if !sha256HexRE.MatchString(p.SHA256) {
		return fmt.Errorf("package %s:%s: the index gives no valid sha256 for it", p.Name, p.Version)
	}
	if p.Size <= 0 {
		return fmt.Errorf("package %s:%s: the index gives no size for it", p.Name, p.Version)
	}
	return checkDownloadURL(p.URL)
}

func checkDownloadURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("package url: %w", err)
	}
	switch {
	case u.Scheme == "https" && u.Host != "":
		return nil
	case u.Scheme == "http" && isLoopbackHost(u.Hostname()):
		return nil
	}
	return fmt.Errorf("package url %s: only https downloads are accepted", u.Redacted())
}

func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// checkIndex refuses an index whose entries are filed under another name.
func checkIndex(idx *Index) error {
	for name, versions := range idx.Packages {
		for _, p := range versions {
			if p.Name != name {
				return fmt.Errorf("package index: entry %q is listed under %q", p.Name, name)
			}
		}
	}
	return nil
}
