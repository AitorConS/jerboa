//go:build !unix

package upgradetest

import (
	"errors"
	"io/fs"
)

// Fixtures are recorded on Linux and macOS only; Restore skips before it
// would lock anything elsewhere.
func lock(string) (func(), error) {
	return nil, errors.New("upgrade fixtures cannot be restored on this system")
}

func ownedByUs(fs.FileInfo) bool { return false }
