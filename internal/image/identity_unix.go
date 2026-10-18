//go:build unix

package image

import "golang.org/x/sys/unix"

// fileIdentity identifies the exact file at path: device, inode, size,
// modification and status-change times. Any write, truncate, chmod or
// replacement changes it, and the status-change time cannot be set back by a
// process, so an unchanged identity means unchanged contents.
func fileIdentity(path string) (diskIdentity, bool) {
	var st unix.Stat_t
	if err := unix.Stat(path, &st); err != nil {
		return diskIdentity{}, false
	}
	return diskIdentity{
		Dev:      uint64(st.Dev), //nolint:gosec,unconvert // int32 on darwin, uint64 on linux
		Ino:      st.Ino,
		Size:     st.Size,
		ModNs:    st.Mtim.Nano(),
		ChangeNs: st.Ctim.Nano(),
	}, true
}
