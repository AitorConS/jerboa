// Package preflight statically validates a unikernel image's contents before
// mkfs runs, so problems that would otherwise only surface as a cryptic boot
// failure inside the guest are reported at build time with an explanation.
//
// The checks are intentionally host-independent: they parse the ELF headers of
// the program and its bundled libraries with debug/elf (which works on any
// build host, including Windows) instead of shelling out to ldd.
package preflight

import (
	"debug/elf"
	"fmt"
	"path"
	"path/filepath"
	"strings"

	pkg "github.com/AitorConS/jerboa/internal/package"
)

// Severity classifies a finding.
type Severity int

const (
	// Warning findings are printed but do not abort the build.
	Warning Severity = iota
	// Error findings abort the build (unless preflight is disabled).
	Error
)

// String returns the human-readable severity label.
func (s Severity) String() string {
	if s == Error {
		return "error"
	}
	return "warning"
}

// Finding is a single preflight diagnostic with an actionable hint.
type Finding struct {
	Severity Severity
	Message  string
	Hint     string
}

// HasErrors reports whether any finding is an Error.
func HasErrors(findings []Finding) bool {
	for _, f := range findings {
		if f.Severity == Error {
			return true
		}
	}
	return false
}

// Format renders findings as an indented multi-line block for terminal output.
func Format(findings []Finding) string {
	var b strings.Builder
	for _, f := range findings {
		fmt.Fprintf(&b, "  [%s] %s\n", f.Severity, f.Message)
		if f.Hint != "" {
			fmt.Fprintf(&b, "          %s\n", f.Hint)
		}
	}
	return b.String()
}

// guestTree indexes the files that will exist inside the image, both by their
// exact (slash-separated, no leading "/") guest path and by basename, so lookups
// mirror what the guest's dynamic loader can plausibly find.
type guestTree struct {
	byPath map[string]pkg.File
	byBase map[string][]pkg.File
}

func newGuestTree(files []pkg.File) *guestTree {
	t := &guestTree{
		byPath: make(map[string]pkg.File, len(files)),
		byBase: make(map[string][]pkg.File),
	}
	for _, f := range files {
		if f.IsDir {
			continue
		}
		gp := f.GuestPath
		if gp == "" {
			gp = filepath.Base(f.HostPath)
		}
		gp = strings.TrimPrefix(filepath.ToSlash(gp), "/")
		t.byPath[gp] = f
		base := path.Base(gp)
		t.byBase[base] = append(t.byBase[base], f)
	}
	return t
}

// lookup finds a library by exact guest path first, then by basename.
// Returns the host path backing it, or "" if the image will not contain it.
func (t *guestTree) lookup(ref string) (hostPath string, ok bool) {
	clean := cleanGuestPath(ref)
	if f, found := t.byPath[clean]; found {
		return f.HostPath, true
	}
	base := path.Base(clean)
	if fs := t.byBase[base]; len(fs) > 0 {
		return fs[0].HostPath, true
	}
	return "", false
}

// CheckImage validates the program binary and the files that will be packed
// into the image. binaryPath is the host path of the program; files are the
// package/source files with their guest paths; entrypoint, when non-empty, is
// the interpreted-language script that must exist in the image (e.g. "app.js").
func CheckImage(binaryPath string, files []pkg.File, entrypoint string) []Finding {
	var findings []Finding
	tree := newGuestTree(files)

	f, err := elf.Open(binaryPath)
	if err != nil {
		return append(findings, Finding{
			Severity: Error,
			Message:  fmt.Sprintf("program %s is not a readable ELF binary: %v", filepath.Base(binaryPath), err),
			Hint:     "unikernel programs must be Linux ELF binaries; cross-compile with GOOS=linux (Go) or --target x86_64-unknown-linux-musl (Rust)",
		})
	}
	defer func() { _ = f.Close() }()

	if f.Class == elf.ELFCLASS32 {
		findings = append(findings, Finding{
			Severity: Error,
			Message:  "program is a 32-bit ELF binary",
			Hint:     "the Nanos kernel only boots 64-bit binaries; rebuild for x86_64 or arm64",
		})
	}
	if f.Machine != elf.EM_X86_64 && f.Machine != elf.EM_AARCH64 {
		findings = append(findings, Finding{
			Severity: Error,
			Message:  fmt.Sprintf("program is built for unsupported architecture %s", f.Machine),
			Hint:     "build for x86_64 (or arm64) Linux, e.g. GOOS=linux GOARCH=amd64",
		})
	}

	interp := programInterp(f)
	if interp == "" {
		// Fully static binary (Go with CGO_ENABLED=0, musl static Rust):
		// nothing else to resolve.
		if entrypoint != "" {
			findings = append(findings, checkEntrypoint(tree, entrypoint)...)
		}
		return findings
	}

	// Dynamic binary: the kernel loads the interpreter from inside the image at
	// the exact absolute path the binary requests, so it must be present there.
	if _, ok := tree.byPath[cleanGuestPath(interp)]; !ok {
		findings = append(findings, Finding{
			Severity: Error,
			Message:  fmt.Sprintf("program is dynamically linked and its interpreter %s is not in the image", interp),
			Hint:     "bundle the dynamic loader at that exact path (ops packages ship it in sysroot/), or link the program statically",
		})
	}

	// Walk the DT_NEEDED closure: every shared library the program (or any of
	// its libraries) needs must be somewhere in the image. Missing libraries
	// fail at boot with "error loading shared library".
	missing := map[string]string{} // lib name -> first requester
	visited := map[string]bool{}
	walkNeeded(f, filepath.Base(binaryPath), tree, visited, missing)

	for lib, requester := range missing {
		findings = append(findings, Finding{
			Severity: Error,
			Message:  fmt.Sprintf("shared library %s (needed by %s) is not in the image", lib, requester),
			Hint:     "add the package that ships it with --pkg / [build] pkgs, or use a statically linked program",
		})
	}

	if entrypoint != "" {
		findings = append(findings, checkEntrypoint(tree, entrypoint)...)
	}
	return findings
}

// cleanGuestPath normalizes a guest reference to the slash-separated, relative,
// dot-free form used as guestTree keys (e.g. "./index.js" → "index.js").
func cleanGuestPath(ref string) string {
	return strings.TrimPrefix(path.Clean("/"+filepath.ToSlash(ref)), "/")
}

// programInterp returns the PT_INTERP path of an ELF binary, or "" for static binaries.
func programInterp(f *elf.File) string {
	for _, p := range f.Progs {
		if p.Type != elf.PT_INTERP {
			continue
		}
		data := make([]byte, p.Filesz)
		if _, err := p.ReadAt(data, 0); err != nil {
			return ""
		}
		return strings.TrimRight(string(data), "\x00")
	}
	return ""
}

// walkNeeded recursively resolves the DT_NEEDED entries of f against the guest
// tree, recording unresolvable libraries in missing (keyed by library name,
// valued by the first binary that requested them). Libraries found in the tree
// are opened from their host path and their own dependencies walked in turn.
func walkNeeded(f *elf.File, requester string, tree *guestTree, visited map[string]bool, missing map[string]string) {
	needed, err := f.ImportedLibraries()
	if err != nil {
		// A binary with no dynamic section returns an error here; nothing to walk.
		return
	}
	for _, lib := range needed {
		if visited[lib] {
			continue
		}
		visited[lib] = true

		hostPath, ok := tree.lookup(lib)
		if !ok {
			if _, dup := missing[lib]; !dup {
				missing[lib] = requester
			}
			continue
		}
		child, err := elf.Open(hostPath)
		if err != nil {
			// Present but unreadable as ELF (e.g. a linker script): presence is
			// what boot needs; do not fail the closure on it.
			continue
		}
		walkNeeded(child, lib, tree, visited, missing)
		_ = child.Close()
	}
}

// checkEntrypoint verifies the interpreted-language entrypoint script exists in
// the image tree.
func checkEntrypoint(tree *guestTree, entrypoint string) []Finding {
	clean := cleanGuestPath(entrypoint)
	if _, ok := tree.byPath[clean]; ok {
		return nil
	}
	return []Finding{{
		Severity: Error,
		Message:  fmt.Sprintf("entrypoint %s is not among the files packed into the image", entrypoint),
		Hint:     "check [build] entrypoint in unikernel.toml and that the file is not excluded by .unignore",
	}}
}

// CheckImagePlatform checks the final guest paths using the same resolver as imports and the daemon.
func CheckImagePlatform(binary string, files []pkg.File, entrypoint, program, platform string) []Finding {
	_, err := pkg.ValidateImage(binary, program, files, platform)
	if err != nil {
		return []Finding{{Severity: Error, Message: err.Error()}}
	}
	if entrypoint != "" {
		return checkEntrypoint(newGuestTree(files), entrypoint)
	}
	return nil
}
