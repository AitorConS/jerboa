package main

import (
	"archive/tar"
	"fmt"
	"io"
	"os"
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
	want := filepath.ToSlash(programPath)

	for _, f := range pkgFiles {
		if filepath.ToSlash(f.GuestPath) == want {
			return f.HostPath, filepath.ToSlash(f.GuestPath), nil
		}
	}
	for _, f := range pkgFiles {
		if strings.HasSuffix(filepath.ToSlash(f.GuestPath), "/"+want) {
			return f.HostPath, filepath.ToSlash(f.GuestPath), nil
		}
	}
	base := filepath.Base(want)
	for _, f := range pkgFiles {
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
func buildContextReader(binaryPath string, pkgFiles []pkg.File) *io.PipeReader {
	pr, pw := io.Pipe()
	go func() {
		tw := tar.NewWriter(pw)
		err := func() error {
			if err := addFileToTar(tw, binaryPath, buildProgramPath); err != nil {
				return err
			}
			for _, f := range pkgFiles {
				guestPath := f.GuestPath
				if guestPath == "" {
					guestPath = filepath.Base(f.HostPath)
				}
				if f.IsDir {
					if err := addDirToTar(tw, guestPath); err != nil {
						return err
					}
				} else {
					if err := addFileToTar(tw, f.HostPath, guestPath); err != nil {
						return err
					}
				}
			}
			return tw.Close()
		}()
		_ = pw.CloseWithError(err)
	}()
	return pr
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

// findRuntimeBinary searches the resolved package files for the runtime binary
// of the given language.
func findRuntimeBinary(pkgFiles []pkg.File, lang builder.Lang) (string, error) {
	candidates, ok := runtimeBinaryCandidates[lang]
	if !ok {
		return "", fmt.Errorf("language %s does not have a runtime binary", lang)
	}

	// Exact base-name match: try each candidate in priority order.
	for _, name := range candidates {
		for _, f := range pkgFiles {
			if filepath.Base(f.HostPath) == name {
				return f.HostPath, nil
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
			dir := filepath.Dir(f.HostPath)
			matches, _ := filepath.Glob(filepath.Join(dir, name+"*"))
			for _, match := range matches {
				suffix := strings.TrimPrefix(filepath.Base(match), name)
				if suffix == "" || strings.HasPrefix(suffix, ".") || (suffix[0] >= '0' && suffix[0] <= '9') {
					return match, nil
				}
			}
		}
	}

	return "", fmt.Errorf("runtime binary %q (or variant) not found in package files", candidates[0])
}

// sourceFiles collects application source files from dir for inclusion in the image.
// It reads .unignore patterns and excludes matching files and directories.
func sourceFiles(dir string) ([]pkg.File, error) {
	ignore, err := builder.LoadIgnoreFile(dir)
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
		// Only regular files can be streamed into the tar. filepath.Walk follows
		// the dir tree without following symlinks to non-regular targets, but a
		// FIFO/socket/device entry would otherwise be tarred as TypeReg and then
		// block or fail during os.Open/io.Copy.
		if info.Mode().IsRegular() {
			files = append(files, pkg.File{HostPath: path, GuestPath: rel})
		} else if !info.IsDir() {
			return fmt.Errorf("unsupported non-regular source file %q", rel)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walk source dir: %w", err)
	}
	return files, nil
}
