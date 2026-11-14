package preflight

import (
	"debug/elf"
	"encoding/binary"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	pkg "github.com/AitorConS/jerboa/internal/package"
)

// The preflight check reads files the user hands to a build: a program that
// may be truncated, corrupted, unreadable, or not a program at all. Whatever
// it is given it must answer with findings a user can act on, never crash the
// build, and never pass a program the kernel cannot boot.

// elfWithInterp writes an ELF64 executable whose only program header is a
// PT_INTERP naming interp, with filesz as the declared length of that name
// (0 means the real length).
func elfWithInterp(t *testing.T, interp string, filesz uint64) string {
	t.Helper()
	const ehsize, phsize = 64, 56
	name := append([]byte(interp), 0)
	if filesz == 0 {
		filesz = uint64(len(name))
	}
	buf := make([]byte, ehsize+phsize, ehsize+phsize+len(name))
	copy(buf, elf.ELFMAG)
	buf[elf.EI_CLASS] = byte(elf.ELFCLASS64)
	buf[elf.EI_DATA] = byte(elf.ELFDATA2LSB)
	buf[elf.EI_VERSION] = 1
	binary.LittleEndian.PutUint16(buf[16:], uint16(elf.ET_EXEC))
	binary.LittleEndian.PutUint16(buf[18:], uint16(elf.EM_X86_64))
	binary.LittleEndian.PutUint32(buf[20:], 1)
	binary.LittleEndian.PutUint64(buf[32:], ehsize) // e_phoff
	binary.LittleEndian.PutUint16(buf[52:], ehsize)
	binary.LittleEndian.PutUint16(buf[54:], phsize)
	binary.LittleEndian.PutUint16(buf[56:], 1) // e_phnum
	ph := buf[ehsize:]
	binary.LittleEndian.PutUint32(ph[0:], uint32(elf.PT_INTERP))
	binary.LittleEndian.PutUint64(ph[8:], ehsize+phsize) // p_offset
	binary.LittleEndian.PutUint64(ph[32:], filesz)
	binary.LittleEndian.PutUint64(ph[40:], filesz)
	buf = append(buf, name...)
	path := filepath.Join(t.TempDir(), "program")
	if err := os.WriteFile(path, buf, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func errorsOf(findings []Finding) []string {
	var out []string
	for _, f := range findings {
		if f.Severity == Error {
			out = append(out, f.Message)
		}
	}
	return out
}

func requireOneError(t *testing.T, findings []Finding, contains string) {
	t.Helper()
	msgs := errorsOf(findings)
	if len(msgs) != 1 || !strings.Contains(msgs[0], contains) {
		t.Fatalf("want one error containing %q, got %q", contains, msgs)
	}
	for _, f := range findings {
		if f.Hint == "" {
			t.Fatalf("finding %q tells the user nothing to do", f.Message)
		}
		if strings.Contains(f.Message, "\n") {
			t.Fatalf("finding spans several lines: %q", f.Message)
		}
	}
}

// Files that are not a bootable program, in every way a file can fail to be
// one, are refused with one finding naming the file.
func TestFaults_ProgramIsNotAUsableELF(t *testing.T) {
	valid, err := os.ReadFile(writeMinimalELF(t, t.TempDir(), "valid", elf.EM_X86_64, elf.ELFCLASS64))
	if err != nil {
		t.Fatal(err)
	}
	contents := map[string][]byte{
		"empty":            {},
		"one byte":         {0x7f},
		"magic only":       []byte(elf.ELFMAG),
		"truncated header": valid[:20],
		"shell script":     []byte("#!/bin/sh\nexec app\n"),
		"mach-o":           {0xcf, 0xfa, 0xed, 0xfe, 0x0c, 0x00, 0x00, 0x01},
		"pe":               []byte("MZ\x90\x00\x03\x00\x00\x00"),
		"bad class":        append(append([]byte(elf.ELFMAG), 9, 1, 1), make([]byte, 57)...),
		"bad byte order":   append(append([]byte(elf.ELFMAG), 2, 9, 1), make([]byte, 57)...),
		"gzip":             {0x1f, 0x8b, 0x08, 0x00, 0x00, 0x00, 0x00, 0x00},
	}
	for name, content := range contents {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "my-program")
			if err := os.WriteFile(path, content, 0o600); err != nil {
				t.Fatal(err)
			}
			requireOneError(t, CheckImage(path, nil, ""), "my-program is not a readable ELF binary")
		})
	}
}

func TestFaults_ProgramCannotBeRead(t *testing.T) {
	dir := t.TempDir()
	requireOneError(t, CheckImage(filepath.Join(dir, "absent"), nil, ""), "absent is not a readable ELF binary")
	requireOneError(t, CheckImage(dir, nil, ""), "not a readable ELF binary")

	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		return // file modes do not stop the read
	}
	path := writeMinimalELF(t, dir, "locked", elf.EM_X86_64, elf.ELFCLASS64)
	if err := os.Chmod(path, 0o000); err != nil {
		t.Fatal(err)
	}
	requireOneError(t, CheckImage(path, nil, ""), "locked is not a readable ELF binary")

	loop := filepath.Join(dir, "loop")
	if err := os.Symlink(loop, loop); err != nil {
		t.Fatal(err)
	}
	requireOneError(t, CheckImage(loop, nil, ""), "loop is not a readable ELF binary")
}

// A dynamically linked program boots only if its loader is in the image at
// the exact path the program names.
func TestFaults_DynamicLoader(t *testing.T) {
	const interp = "/lib64/ld-linux-x86-64.so.2"
	bin := elfWithInterp(t, interp, 0)
	loader := filepath.Join(t.TempDir(), "ld.so")
	if err := os.WriteFile(loader, []byte("loader"), 0o600); err != nil {
		t.Fatal(err)
	}

	requireOneError(t, CheckImage(bin, nil, ""), "interpreter "+interp+" is not in the image")

	// Present under another directory is still missing: the kernel does not
	// search for it.
	elsewhere := []pkg.File{{HostPath: loader, GuestPath: "/usr/lib/ld-linux-x86-64.so.2"}}
	requireOneError(t, CheckImage(bin, elsewhere, ""), "interpreter "+interp+" is not in the image")

	// A directory at that path is not the loader.
	asDir := []pkg.File{{HostPath: filepath.Dir(loader), GuestPath: interp, IsDir: true}}
	requireOneError(t, CheckImage(bin, asDir, ""), "is not in the image")

	for _, guest := range []string{interp, "lib64/ld-linux-x86-64.so.2"} {
		present := []pkg.File{{HostPath: loader, GuestPath: guest}}
		if got := errorsOf(CheckImage(bin, present, "")); len(got) != 0 {
			t.Fatalf("loader at %q not recognized: %q", guest, got)
		}
	}

	// A missing loader and a missing entrypoint are both reported.
	if got := errorsOf(CheckImage(bin, nil, "/app/main.js")); len(got) != 2 {
		t.Fatalf("want the loader and the entrypoint reported, got %q", got)
	}
}

// A PT_INTERP segment that lies about its size must not make the check read
// outside the file or allocate what the header claims.
func TestFaults_DamagedInterpSegment(t *testing.T) {
	for name, filesz := range map[string]uint64{"past the end of the file": 2000, "huge": 1 << 40, "one more than the cap": 4097} {
		t.Run(name, func(t *testing.T) {
			bin := elfWithInterp(t, "/lib/ld.so", filesz)
			for _, f := range CheckImage(bin, nil, "") {
				if strings.Contains(f.Message, "\x00") {
					t.Fatalf("raw bytes in a finding: %q", f.Message)
				}
			}
		})
	}
}

// The entrypoint is checked against what is packed, whatever the spelling,
// and a directory with its name does not count.
func TestFaults_Entrypoint(t *testing.T) {
	bin := writeMinimalELF(t, t.TempDir(), "node", elf.EM_AARCH64, elf.ELFCLASS64)
	files := []pkg.File{
		{HostPath: "/host/app/main.js", GuestPath: "/app/main.js"},
		{HostPath: "/host/app/lib", GuestPath: "/app/lib", IsDir: true},
		{HostPath: "/host/top.js"},
	}
	for _, ok := range []string{"/app/main.js", "app/main.js", "/app/./main.js", "/app/lib/../main.js", "top.js", "/top.js"} {
		if got := CheckImage(bin, files, ok); len(got) != 0 {
			t.Fatalf("entrypoint %q not found: %v", ok, got)
		}
	}
	for _, missing := range []string{"/app/other.js", "main.js", "/app/lib", "/app/MAIN.JS", "../../etc/passwd", "/app/main.js/", "/"} {
		findings := CheckImage(bin, files, missing)
		if missing == "/app/main.js/" {
			// A trailing slash still names the file once the path is cleaned.
			if len(findings) != 0 {
				t.Fatalf("%q: %v", missing, findings)
			}
			continue
		}
		requireOneError(t, findings, "entrypoint "+missing+" is not among the files")
	}
}

// CheckImagePlatform reports an unusable program as a finding too, with or
// without an entrypoint to look for.
func TestFaults_CheckImagePlatform(t *testing.T) {
	notELF := filepath.Join(t.TempDir(), "program")
	if err := os.WriteFile(notELF, []byte("#!/bin/sh\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, platform := range []string{"linux/amd64", "linux/arm64", "", "plan9/mips", "linux/amd64/v9"} {
		findings := CheckImagePlatform(notELF, nil, "/app/main.js", "program", platform)
		if !HasErrors(findings) {
			t.Fatalf("platform %q: a shell script passed the platform check", platform)
		}
	}
	amd64 := writeMinimalELF(t, t.TempDir(), "program", elf.EM_X86_64, elf.ELFCLASS64)
	if findings := CheckImagePlatform(amd64, nil, "", "program", "linux/arm64"); !HasErrors(findings) {
		t.Fatal("an x86_64 program passed as linux/arm64")
	}
}

// Formatting never loses a finding, and an empty list formats to nothing so
// callers can print the result unconditionally.
func TestFaults_Format(t *testing.T) {
	if Format(nil) != "" {
		t.Fatalf("no findings formatted as %q", Format(nil))
	}
	findings := []Finding{
		{Severity: Error, Message: "first"},
		{Severity: Warning, Message: "second", Hint: "do this"},
		{Severity: Severity(42), Message: "third"},
	}
	out := Format(findings)
	for _, want := range []string{"[error] first", "[warning] second", "do this", "third"} {
		if !strings.Contains(out, want) {
			t.Fatalf("%q missing from %q", want, out)
		}
	}
	if strings.Count(out, "\n") != 4 {
		t.Fatalf("want one line per finding and per hint, got %q", out)
	}
	if HasErrors(findings[1:]) {
		t.Fatal("an unknown severity counted as an error")
	}
}

// FuzzCheckImage feeds corrupted programs to the check. It must return, and
// what it returns must be printable findings.
func FuzzCheckImage(f *testing.F) {
	dir := f.TempDir()
	for _, seed := range []string{
		writeMinimalELFSeed(f, dir, "amd64", elf.EM_X86_64, elf.ELFCLASS64),
		writeMinimalELFSeed(f, dir, "arm64", elf.EM_AARCH64, elf.ELFCLASS64),
		writeMinimalELFSeed(f, dir, "i386", elf.EM_386, elf.ELFCLASS32),
	} {
		data, err := os.ReadFile(seed)
		if err != nil {
			f.Fatal(err)
		}
		f.Add(data, "/app/main")
	}
	if self, err := os.Executable(); err == nil {
		if data, err := os.ReadFile(self); err == nil && len(data) > 4096 {
			f.Add(data[:4096], "")
		}
	}
	f.Add([]byte("\x7fELF\x02\x01\x01"), "x")
	f.Fuzz(func(t *testing.T, data []byte, entrypoint string) {
		path := filepath.Join(t.TempDir(), "program")
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		lib := []pkg.File{{HostPath: path, GuestPath: "/lib/libc.so.6"}, {HostPath: path, GuestPath: "/lib64/ld-linux-x86-64.so.2"}}
		findings := CheckImage(path, lib, entrypoint)
		for _, finding := range findings {
			if finding.Message == "" {
				t.Fatal("finding without a message")
			}
		}
		if Format(findings) == "" && len(findings) > 0 {
			t.Fatal("findings formatted to nothing")
		}
	})
}

func writeMinimalELFSeed(f *testing.F, dir, name string, machine elf.Machine, class elf.Class) string {
	f.Helper()
	buf := make([]byte, 64)
	copy(buf, elf.ELFMAG)
	buf[elf.EI_CLASS] = byte(class)
	buf[elf.EI_DATA] = byte(elf.ELFDATA2LSB)
	buf[elf.EI_VERSION] = 1
	binary.LittleEndian.PutUint16(buf[16:], uint16(elf.ET_EXEC))
	binary.LittleEndian.PutUint16(buf[18:], uint16(machine))
	binary.LittleEndian.PutUint32(buf[20:], 1)
	binary.LittleEndian.PutUint16(buf[52:], 64)
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, buf, 0o600); err != nil {
		f.Fatal(err)
	}
	return path
}
