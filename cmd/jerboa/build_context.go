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
//
// program.Walked is set when the program is a build output, which the build
// steps could have replaced: it is then streamed only if it is still the
// file checked after the build.
func buildContextReader(program pkg.File, programPath string, pkgFiles []pkg.File) *io.PipeReader {
	pr, pw := io.Pipe()
	go func() {
		tw := tar.NewWriter(pw)
		err := func() error {
			if err := addFileToTar(tw, program, buildProgramPath); err != nil {
				return err
			}
			files := make([]pkg.File, 0, len(pkgFiles))
			for _, f := range pkgFiles {
				if isProgramFile(f, program.HostPath, programPath) {
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
		if err := addFileToTar(tw, f, guestPath); err != nil {
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

// addFileToTar writes f's host file into tw under the slash-separated
// guestPath.
func addFileToTar(tw *tar.Writer, f pkg.File, guestPath string) error {
	file, info, err := openBuildFile(f)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	hdr := &tar.Header{
		Name:     filepath.ToSlash(guestPath),
		Mode:     int64(info.Mode().Perm()),
		Size:     info.Size(),
		Typeflag: tar.TypeReg,
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return fmt.Errorf("tar header %s: %w", guestPath, err)
	}
	if _, err := io.Copy(tw, file); err != nil {
		return fmt.Errorf("tar copy %s: %w", guestPath, err)
	}
	return nil
}

// openBuildFile opens f's host file for streaming into the build context.
// A file listed from the project (f.Walked set) must still be the file that
// was checked then: the project's build steps can write there, and on macOS
// a step can leave a process behind that swaps a checked file for a link to
// a host secret. Such a file is opened without following a link in its last
// component, and a link that was listed is resolved again and must still
// lead to a file inside f.LinkRoot. Package files come from the package
// store, which build steps cannot write to; they are opened as they are, and
// must be regular files.
func openBuildFile(f pkg.File) (*os.File, os.FileInfo, error) {
	if f.Walked == nil {
		file, err := os.Open(f.HostPath)
		if err != nil {
			return nil, nil, fmt.Errorf("open %s: %w", f.HostPath, err)
		}
		info, err := file.Stat()
		if err != nil {
			_ = file.Close()
			return nil, nil, fmt.Errorf("stat %s: %w", f.HostPath, err)
		}
		if !info.Mode().IsRegular() {
			_ = file.Close()
			return nil, nil, fmt.Errorf("open %s: not a regular file", f.HostPath)
		}
		return file, info, nil
	}
	path := f.HostPath
	if f.LinkRoot != "" {
		target, ok, err := linkedProjectFile(f.LinkRoot, f.HostPath)
		if err != nil {
			return nil, nil, err
		}
		if !ok {
			return nil, nil, fmt.Errorf("source symlink %s no longer points to a regular file inside the project: did a build step change it?", f.HostPath)
		}
		path = target
	}
	file, info, err := builder.OpenRegular(path)
	if err != nil {
		return nil, nil, fmt.Errorf("%w (it was a regular file when the build listed it: did a build step replace it?)", err)
	}
	if !os.SameFile(f.Walked, info) {
		_ = file.Close()
		return nil, nil, fmt.Errorf("open %s: the file changed after the build listed it: did a build step replace it?", f.HostPath)
	}
	return file, info, nil
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

// linkedProjectFile resolves the symlink at path and reports whether it
// leads to a regular file inside the project directory dir, and which.
func linkedProjectFile(dir, path string) (string, bool, error) {
	target, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", false, nil //nolint:nilerr // a dangling link is reported as unsupported
	}
	root, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", false, fmt.Errorf("resolve project dir: %w", err)
	}
	rel, err := filepath.Rel(root, target)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false, nil //nolint:nilerr // outside the project
	}
	info, err := os.Lstat(target)
	return target, err == nil && info.Mode().IsRegular(), nil
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
			// A directory the project ignores is skipped even when it cannot
			// be read (a database's data directory owned by another user).
			if info != nil && info.IsDir() {
				if rel, rerr := filepath.Rel(dir, path); rerr == nil && ignore.Match(filepath.ToSlash(rel), true) {
					return filepath.SkipDir
				}
			}
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
		// The file is streamed later, after other build steps may have run;
		// what was checked here is kept, so that the stream refuses a file
		// replaced in the meantime (see openBuildFile).
		f := pkg.File{HostPath: path, GuestPath: rel, FromContext: true, Walked: info}
		if info.Mode()&os.ModeSymlink != 0 {
			target, ok, lerr := linkedProjectFile(dir, path)
			if lerr != nil {
				return lerr
			}
			if !ok {
				return fmt.Errorf("unsupported source symlink %q: it must point to a regular file inside the project", rel)
			}
			walked, serr := os.Lstat(target)
			if serr != nil {
				return fmt.Errorf("source symlink %q: %w", rel, serr)
			}
			f.Walked, f.LinkRoot = walked, dir
		} else if !info.Mode().IsRegular() {
			return fmt.Errorf("unsupported non-regular source file %q", rel)
		}
		files = append(files, f)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walk source dir: %w", err)
	}
	return files, nil
}
