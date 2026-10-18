package pkg

import (
	"log/slog"
	"strings"
)

// pythonBuildOnlyDirs are the children of a lib/pythonX.Y directory that a
// unikernel never uses at runtime, besides config-* (static libpython and
// build headers):
//   - test         — CPython's own test suite
//   - idlelib      — the IDLE editor
//   - tkinter, turtledemo — GUI toolkit bindings and demos (no display)
//   - ensurepip    — bundled pip wheels for creating environments
//   - lib2to3      — Python 2→3 migration tool
//   - pydoc_data   — HTML documentation data for pydoc
var pythonBuildOnlyDirs = map[string]bool{
	"test":       true,
	"idlelib":    true,
	"tkinter":    true,
	"turtledemo": true,
	"ensurepip":  true,
	"lib2to3":    true,
	"pydoc_data": true,
}

// isPythonLibDir reports whether a path component names a versioned Python
// library directory such as "python3" or "python3.12".
func isPythonLibDir(part string) bool {
	rest, ok := strings.CutPrefix(part, "python")
	return ok && rest != "" && rest[0] >= '0' && rest[0] <= '9'
}

// runtimeBloatDir reports whether a slash-separated package-relative path is
// a directory that is only needed at build time, not at runtime inside a
// unikernel. A walker skips the whole subtree when it returns true.
func runtimeBloatDir(rel string) bool {
	parts := strings.Split(rel, "/")
	for i := 0; i+1 < len(parts); i++ {
		if !isPythonLibDir(parts[i]) {
			continue
		}
		next := parts[i+1]
		if strings.HasPrefix(next, "config-") || pythonBuildOnlyDirs[next] {
			return true
		}
	}
	return false
}

// runtimeBloatFile reports whether a slash-separated path is a file a
// unikernel does not need: optimized bytecode (.opt-1.pyc, .opt-2.pyc) is only
// loaded under python -O, and the plain .pyc next to it is kept.
func runtimeBloatFile(rel string) bool {
	return strings.Contains(rel, "__pycache__/") &&
		(strings.HasSuffix(rel, ".opt-1.pyc") || strings.HasSuffix(rel, ".opt-2.pyc"))
}

// isRuntimeBloat reports whether a file at the slash-separated path rel, or
// any directory above it, is build-time only.
func isRuntimeBloat(rel string) bool {
	if runtimeBloatFile(rel) {
		return true
	}
	for i := len(rel) - 1; i > 0; i-- {
		if rel[i] == '/' && runtimeBloatDir(rel[:i]) {
			return true
		}
	}
	return false
}

// PruneRuntimeBloat drops build-time-only files (see runtimeBloatDir and
// runtimeBloatFile) from a package's file list, so a package created from a
// full interpreter install is as lean as one consumed from ops.
// The first file is the package program and is always kept.
func PruneRuntimeBloat(files []File) []File {
	kept := files[:0:0]
	pruned := 0
	for i, f := range files {
		name := tarEntryName(f.GuestPath)
		if i > 0 && (isRuntimeBloat(name) || f.IsDir && runtimeBloatDir(name)) {
			pruned++
			continue
		}
		kept = append(kept, f)
	}
	if pruned > 0 {
		slog.Info("pruned build-time files from package", "files", pruned)
	}
	return kept
}
