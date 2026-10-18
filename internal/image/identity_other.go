//go:build !unix

package image

// fileIdentity is unavailable here, so every verification re-hashes the disk.
func fileIdentity(string) (diskIdentity, bool) { return diskIdentity{}, false }
