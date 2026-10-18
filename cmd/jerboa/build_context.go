package main

import (
	"archive/tar"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/AitorConS/jerboa/internal/builder"
	pkg "github.com/AitorConS/jerboa/internal/package"
)

// findProgramBinary locates the program binary among resolved package files by
// exact guest path, a guest path suffix (preserving directory structure, e.g.
// "jdk-21/bin/java"), or a basename match (e.g. "java"). It returns both the
// host path (to stream into the build context) and the matched in-image guest
// path, so the program can be executed from its real location.
func findProgramBinary(pkgFiles []pkg.File, programPath string) (hostPath, guestPath string, err error) {
	// Package guest paths are slash-style and relative (no leading "/"), but the
	// most intuitive way to name the program is its absolute in-image path
	// (e.g. "/usr/local/pgsql/bin/postgres" — what it actually is once booted).
	// Normalize the leading slash off both sides so an absolute path still
	// matches the relative guest path by exact path or path suffix.
	want := strings.TrimPrefix(filepath.ToSlash(programPath), "/")

	// Directory placeholders (e.g. volume mount points from [build] dirs) have no
	// executable host file, so they can never satisfy a program lookup.
	for _, f := range pkgFiles {
		if f.IsDir {
			continue
		}
		if strings.TrimPrefix(filepath.ToSlash(f.GuestPath), "/") == want {
			return f.HostPath, filepath.ToSlash(f.GuestPath), nil
		}
	}
	for _, f := range pkgFiles {
		if f.IsDir {
			continue
		}
		if strings.HasSuffix(strings.TrimPrefix(filepath.ToSlash(f.GuestPath), "/"), "/"+want) {
			return f.HostPath, filepath.ToSlash(f.GuestPath), nil
		}
	}
	base := filepath.Base(want)
	for _, f := range pkgFiles {
		if f.IsDir {
			continue
		}
		if filepath.Base(f.HostPath) == base {
			return f.HostPath, filepath.ToSlash(f.GuestPath), nil
		}
	}
	return "", "", fmt.Errorf("program %q not found in resolved packages (--pkg)", programPath)
}

// buildContextReader returns a streaming tar archive of the build context: the
// program binary at buildProgramPath plus each package/source file at its guest
// path. Files are streamed via a pipe, so the archive is never fully buffered.
// The caller must Close the returned reader to release the writer goroutine.
//
// programPath is the in-image path the daemon places the program at (empty
// means /program). A package file that is the program itself at that same path
// is not streamed a second time: the daemon writes the program there anyway.
func buildContextReader(binaryPath, programPath string, pkgFiles []pkg.File) *io.PipeReader {
	pr, pw := io.Pipe()
	go func() {
		tw := tar.NewWriter(pw)
		err := func() error {
			if err := addFileToTar(tw, binaryPath, buildProgramPath); err != nil {
				return err
			}
			files := make([]pkg.File, 0, len(pkgFiles))
			for _, f := range pkgFiles {
				if isProgramFile(f, binaryPath, programPath) {
					continue
				}
				files = append(files, f)
			}
			if err := writePkgFiles(tw, files); err != nil {
				return err
			}
			return tw.Close()
		}()
		_ = pw.CloseWithError(err)
	}()
	return pr
}

// seedContextReader returns a streaming tar archive of files for a volume seed:
// each file/dir at its guest path, with no program binary (unlike
// buildContextReader). The caller must Close the returned reader.
func seedContextReader(files []pkg.File) *io.PipeReader {
	pr, pw := io.Pipe()
	go func() {
		tw := tar.NewWriter(pw)
		err := func() error {
			if err := writePkgFiles(tw, files); err != nil {
				return err
			}
			return tw.Close()
		}()
		_ = pw.CloseWithError(err)
	}()
	return pr
}

func writePkgFiles(tw *tar.Writer, files []pkg.File) error {
	for _, f := range files {
		guestPath := f.GuestPath
		if guestPath == "" {
			guestPath = filepath.Base(f.HostPath)
		}
		if f.IsDir {
			if err := addDirToTar(tw, guestPath); err != nil {
				return err
			}
			continue
		}
		if err := addFileToTar(tw, f.HostPath, guestPath); err != nil {
			return err
		}
	}
	return nil
}

// addDirToTar writes a directory entry into tw at the slash-separated guestPath.
// Used for empty directories from package sysroots (IsDir: true) that have no
// host file but must exist in the image for the program to write into them.
func addDirToTar(tw *tar.Writer, guestPath string) error {
	hdr := &tar.Header{
		Name:     filepath.ToSlash(guestPath) + "/",
		Typeflag: tar.TypeDir,
		Mode:     0o755,
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return fmt.Errorf("tar dir header %s: %w", guestPath, err)
	}
	return nil
}

// addFileToTar writes hostPath into tw under the slash-separated guestPath.
func addFileToTar(tw *tar.Writer, hostPath, guestPath string) error {
	f, err := os.Open(hostPath)
	if err != nil {
		return fmt.Errorf("open %s: %w", hostPath, err)
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("stat %s: %w", hostPath, err)
	}
	hdr := &tar.Header{
		Name:     filepath.ToSlash(guestPath),
		Mode:     int64(info.Mode().Perm()),
		Size:     info.Size(),
		Typeflag: tar.TypeReg,
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return fmt.Errorf("tar header %s: %w", guestPath, err)
	}
	if _, err := io.Copy(tw, f); err != nil {
		return fmt.Errorf("tar copy %s: %w", guestPath, err)
	}
	return nil
}

// runtimeBinaryCandidates maps a language to an ordered list of binary names to
// search for within its package files. The first exact match wins; if none is
// found, a prefix-glob is tried for each candidate in order.
// Multiple candidates handle packages that ship python2.x, python3.x, etc.
var runtimeBinaryCandidates = map[builder.Lang][]string{
	builder.LangNode:   {"node"},
	builder.LangPython: {"python3", "python2", "python"},
}

// programGuestPath returns the normalized in-image path of the program:
// programPath, or "program" when none is set.
func programGuestPath(programPath string) string {
	if programPath == "" {
		programPath = "program"
	}
	return cleanGuestPath(programPath)
}

// cleanGuestPath normalizes a guest path to a slash-separated path relative to
// the image root.
func cleanGuestPath(p string) string {
	return strings.TrimPrefix(path.Clean("/"+filepath.ToSlash(p)), "/")
}

// fileGuestPath returns where f is placed inside the image.
func fileGuestPath(f pkg.File) string {
	if f.GuestPath != "" {
		return cleanGuestPath(f.GuestPath)
	}
	return cleanGuestPath(filepath.Base(f.HostPath))
}

// isProgramFile reports whether f is the program binary itself at the path the
// program occupies in the image, so the two would be one file on disk.
func isProgramFile(f pkg.File, binaryPath, programPath string) bool {
	return !f.IsDir && f.HostPath == binaryPath && fileGuestPath(f) == programGuestPath(programPath)
}

// findRuntimeBinary searches the resolved package files for the runtime binary
// of the given language. It returns the binary's host path and its in-image
// guest path, so the build runs the runtime from where its package placed it
// instead of storing a second copy at /program. guestPath is empty when the
// binary was found on disk next to a package file but is not itself one.
func findRuntimeBinary(pkgFiles []pkg.File, lang builder.Lang) (hostPath, guestPath string, err error) {
	candidates, ok := runtimeBinaryCandidates[lang]
	if !ok {
		return "", "", fmt.Errorf("language %s does not have a runtime binary", lang)
	}

	// Exact base-name match: try each candidate in priority order.
	for _, name := range candidates {
		for _, f := range pkgFiles {
			if !f.IsDir && filepath.Base(f.HostPath) == name {
				return f.HostPath, fileGuestPath(f), nil
			}
		}
	}

	// Prefix-glob on disk: try each candidate prefix in priority order.
	// This catches versioned binaries like python2.7 or python3.12 while
	// rejecting helper scripts like python3-config: a valid runtime suffix is
	// empty or starts with a dot or digit ('-' sorts before '.', so a bare
	// matches[0] would otherwise pick python3-config over python3.12).
	for _, name := range candidates {
		for _, f := range pkgFiles {
			// Directory placeholders have no host path; globbing filepath.Dir("")
			// would search the caller's working directory.
			if f.IsDir || f.HostPath == "" {
				continue
			}
			dir := filepath.Dir(f.HostPath)
			matches, _ := filepath.Glob(filepath.Join(dir, name+"*"))
			for _, match := range matches {
				suffix := strings.TrimPrefix(filepath.Base(match), name)
				if suffix == "" || strings.HasPrefix(suffix, ".") || (suffix[0] >= '0' && suffix[0] <= '9') {
					for _, g := range pkgFiles {
						if !g.IsDir && g.HostPath == match {
							return match, fileGuestPath(g), nil
						}
					}
					return match, "", nil
				}
			}
		}
	}

	return "", "", fmt.Errorf("runtime binary %q (or variant) not found in package files", candidates[0])
}

// symlinkToProjectFile reports whether the symlink at path resolves to a
// regular file inside the project directory dir.
func symlinkToProjectFile(dir, path string) (bool, error) {
	target, err := filepath.EvalSymlinks(path)
	if err != nil {
		return false, nil //nolint:nilerr // a dangling link is reported as unsupported
	}
	root, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return false, fmt.Errorf("resolve project dir: %w", err)
	}
	rel, err := filepath.Rel(root, target)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false, nil //nolint:nilerr // outside the project
	}
	info, err := os.Stat(target)
	return err == nil && info.Mode().IsRegular(), nil
}

// sourceFiles collects application source files from dir for inclusion in the image.
// It reads .unignore patterns and excludes matching files and directories; when
// include is active, only files it matches are kept.
// Every file is marked FromContext, so it shadows a package file that would
// otherwise land at the same guest path (see pkg.ApplyContextPrecedence).
func sourceFiles(dir string, lang builder.Lang, include *builder.IncludeMatcher) ([]pkg.File, error) {
	ignore, err := builder.LoadIgnoreFileFor(dir, lang)
	if err != nil {
		return nil, fmt.Errorf("load ignore file: %w", err)
	}

	var files []pkg.File
	err = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(dir, path)
		if rerr != nil {
			return fmt.Errorf("source file rel path: %w", rerr)
		}
		rel = filepath.ToSlash(rel)

		if ignore.Match(rel, info.IsDir()) {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if info.IsDir() {
			return nil
		}
		if !include.Match(rel) {
			return nil
		}
		// Only regular files can be streamed into the tar: a FIFO/socket/device
		// entry would be tarred as TypeReg and then block or fail during
		// os.Open/io.Copy. A symlink to a regular file inside the project is
		// packaged with its target's content; filepath.Walk does not follow it.
		if info.Mode()&os.ModeSymlink != 0 {
			ok, lerr := symlinkToProjectFile(dir, path)
			if lerr != nil {
				return lerr
			}
			if !ok {
				return fmt.Errorf("unsupported source symlink %q: it must point to a regular file inside the project", rel)
			}
		} else if !info.Mode().IsRegular() {
			return fmt.Errorf("unsupported non-regular source file %q", rel)
		}
		files = append(files, pkg.File{HostPath: path, GuestPath: rel, FromContext: true})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walk source dir: %w", err)
	}
	return files, nil
}
