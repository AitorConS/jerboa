package pkg

import (
	"debug/elf"
	"encoding/binary"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestELFPlatformExplainsHostExecutables(t *testing.T) {
	root := t.TempDir()
	for name, tc := range map[string]struct {
		data   []byte
		format string
	}{
		"macho":     {[]byte{0xcf, 0xfa, 0xed, 0xfe, 0x0c, 0x00, 0x00, 0x01}, "macOS Mach-O"},
		"universal": {[]byte{0xca, 0xfe, 0xba, 0xbe, 0x00, 0x00, 0x00, 0x02}, "macOS Mach-O"},
		"pe":        {[]byte{'M', 'Z', 0x90, 0x00}, "Windows PE"},
	} {
		p := putFixture(t, root, name, tc.data)
		_, err := ELFPlatform(p)
		require.ErrorContains(t, err, "is a "+tc.format+" executable, not a Linux ELF binary")
		require.ErrorContains(t, err, "GOOS=linux")
		_, err = ValidateImage(p, "program", nil, "linux/arm64")
		require.ErrorContains(t, err, tc.format)
	}
}

// syntheticELF builds parsable ELF headers and dynamic sections on every host.
func syntheticELF(machine elf.Machine, interp string, needed []string, runpath string) []byte {
	b := make([]byte, 2048)
	copy(b, []byte{0x7f, 'E', 'L', 'F', 2, 1, 1})
	put16 := func(off int, v uint16) { binary.LittleEndian.PutUint16(b[off:], v) }
	put32 := func(off int, v uint32) { binary.LittleEndian.PutUint32(b[off:], v) }
	put64 := func(off int, v uint64) { binary.LittleEndian.PutUint64(b[off:], v) }
	put16(16, 2)
	put16(18, uint16(machine))
	put32(20, 1)
	put64(32, 64)
	put64(40, 128)
	put16(52, 64)
	put16(54, 56)
	put16(58, 64)
	put16(60, 3)
	if interp != "" {
		put16(56, 1)
		put32(64, uint32(elf.PT_INTERP))
		put64(72, 400)
		put64(96, uint64(len(interp)+1))
		copy(b[400:], interp)
	}
	// section 1 strings at 512, section 2 dynamic at 1024, linked to strings.
	put32(196, uint32(elf.SHT_STRTAB))
	put64(216, 512)
	strings := []byte{0}
	dyn := []uint64{}
	add := func(tag elf.DynTag, s string) {
		dyn = append(dyn, uint64(tag), uint64(len(strings)))
		strings = append(strings, []byte(s)...)
		strings = append(strings, 0)
	}
	for _, s := range needed {
		add(elf.DT_NEEDED, s)
	}
	if runpath != "" {
		add(elf.DT_RUNPATH, runpath)
	}
	dyn = append(dyn, 0, 0)
	put64(224, uint64(len(strings)))
	copy(b[512:], strings)
	put32(260, uint32(elf.SHT_DYNAMIC))
	put64(280, 1024)
	put64(288, uint64(len(dyn)*8))
	put32(296, 1)
	put64(312, 16)
	for i, v := range dyn {
		put64(1024+i*8, v)
	}
	return b
}
func putFixture(t *testing.T, root, name string, data []byte) string {
	t.Helper()
	p := filepath.Join(root, name)
	require.NoError(t, os.MkdirAll(filepath.Dir(p), 0755))
	require.NoError(t, os.WriteFile(p, data, 0755))
	return p
}

func TestLocalStaticPlatformAndVariants(t *testing.T) {
	root := t.TempDir()
	store, err := NewStore(filepath.Join(root, "store"))
	require.NoError(t, err)
	for _, tc := range []struct {
		arch    string
		machine elf.Machine
	}{{"arm64", elf.EM_AARCH64}, {"amd64", elf.EM_X86_64}} {
		bin := putFixture(t, root, tc.arch, syntheticELF(tc.machine, "", nil, ""))
		files, p, err := LocalFiles(bin, "", "bin/service", nil)
		require.NoError(t, err)
		require.Equal(t, "linux/"+tc.arch, p)
		selected, err := store.ForPlatform(p)
		require.NoError(t, err)
		require.NoError(t, selected.CreateFromFiles("service", "1.2.3", files, "", ""))
	}
	list, err := store.List()
	require.NoError(t, err)
	require.Len(t, list, 2)
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, "offline", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	prev := IndexURL
	IndexURL = server.URL
	t.Cleanup(func() { IndexURL = prev })
	for _, platform := range []string{"linux/arm64", "linux/amd64"} {
		files, ref, err := store.Resolve("service", "", platform)
		require.NoError(t, err)
		require.Len(t, files, 1)
		require.Equal(t, "1.2.3", ref.Version)
		require.Len(t, ref.SHA256, 64)
		require.Equal(t, "local", ref.Provenance.Kind)
	}
	require.Zero(t, calls.Load())
}

func TestLocalClosureAliasesOriginTransitiveAndFinalTree(t *testing.T) {
	root := t.TempDir()
	bin := putFixture(t, root, "bin/service", syntheticELF(elf.EM_AARCH64, "/lib/ld.so", []string{"libone.so"}, "$ORIGIN/../private"))
	putFixture(t, root, "usr/lib/ld-real.so", syntheticELF(elf.EM_AARCH64, "", nil, ""))
	require.NoError(t, os.Symlink("usr/lib", filepath.Join(root, "lib")))
	require.NoError(t, os.Symlink("ld-real.so", filepath.Join(root, "usr/lib/ld.so")))
	putFixture(t, root, "private/libone.so", syntheticELF(elf.EM_AARCH64, "", []string{"libtwo.so"}, "${ORIGIN}"))
	two := putFixture(t, root, "private/libtwo.so", syntheticELF(elf.EM_AARCH64, "", nil, ""))
	files, platform, err := LocalFiles(bin, root, "bin/service", nil)
	require.NoError(t, err)
	require.Len(t, files, 4)
	require.Equal(t, "lib/ld.so", files[1].GuestPath)
	_, err = ValidateImage(bin, "bin/service", files, platform)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(two, syntheticELF(elf.EM_X86_64, "", nil, ""), 0755))
	_, _, err = LocalFiles(bin, root, "bin/service", nil)
	require.ErrorContains(t, err, "mixed architectures")
	require.NoError(t, os.Remove(two))
	_, _, err = LocalFiles(bin, root, "bin/service", nil)
	require.ErrorContains(t, err, "libtwo.so")
}

func TestRejectFormatCollisionAndEscapingSymlink(t *testing.T) {
	root := t.TempDir()
	bin := putFixture(t, root, "bin", syntheticELF(elf.EM_AARCH64, "", nil, ""))
	for _, data := range [][]byte{{0xcf, 0xfa, 0xed, 0xfe}, func() []byte { b := syntheticELF(elf.EM_AARCH64, "", nil, ""); b[4] = 1; return b }()} {
		p := putFixture(t, root, "bad", data)
		_, err := ELFPlatform(p)
		require.Error(t, err)
	}
	_, err := ValidateImage(bin, "program", nil, "linux/amd64")
	require.Error(t, err)
	other := putFixture(t, root, "other", []byte("different"))
	require.Error(t, ValidateFiles([]File{{HostPath: bin, GuestPath: "x"}, {HostPath: other, GuestPath: "x"}}, "linux/arm64"))
	require.Error(t, ValidateFiles([]File{{HostPath: bin, GuestPath: "x"}, {HostPath: other, GuestPath: "x/y"}}, "linux/arm64"))
	sysroot := filepath.Join(root, "sysroot")
	require.NoError(t, os.Mkdir(sysroot, 0755))
	require.NoError(t, os.Symlink(bin, filepath.Join(sysroot, "escape")))
	c := &containerFS{root: sysroot, index: map[string]cfsEntry{}, hosts: map[string]string{}}
	_, err = c.resolve("/escape")
	require.Error(t, err)
}

func TestLegacyVerificationDoesNotRewrite(t *testing.T) {
	root := t.TempDir()
	bin := putFixture(t, root, "bin", syntheticELF(elf.EM_X86_64, "", nil, ""))
	store, err := NewStore(filepath.Join(root, "store"))
	require.NoError(t, err)
	require.NoError(t, store.Create("old", "1", bin, nil, "", ""))
	before, err := os.ReadFile(filepath.Join(store.PackageDir("old", "1"), "meta.json"))
	require.NoError(t, err)
	list, err := store.List()
	require.NoError(t, err)
	_, _, err = store.Verify(list[0], "linux/arm64")
	require.Error(t, err)
	_, ref, err := store.Verify(list[0], "linux/amd64")
	require.NoError(t, err)
	require.Equal(t, "linux/amd64", ref.Platform)
	after, err := os.ReadFile(filepath.Join(store.PackageDir("old", "1"), "meta.json"))
	require.NoError(t, err)
	require.Equal(t, before, after)
}

func TestRpathInheritance(t *testing.T) {
	root := t.TempDir()
	data := syntheticELF(elf.EM_AARCH64, "", []string{"libone.so"}, "$ORIGIN/../private")
	// Convert the program's RUNPATH tag into the inherited RPATH tag.
	binary.LittleEndian.PutUint64(data[1040:], uint64(elf.DT_RPATH))
	bin := putFixture(t, root, "bin/service", data)
	putFixture(t, root, "private/libone.so", syntheticELF(elf.EM_AARCH64, "", []string{"libtwo.so"}, ""))
	putFixture(t, root, "private/libtwo.so", syntheticELF(elf.EM_AARCH64, "", nil, ""))
	files, _, err := LocalFiles(bin, root, "bin/service", nil)
	require.NoError(t, err)
	require.Len(t, files, 3)
	binary.LittleEndian.PutUint64(data[1040:], uint64(elf.DT_RUNPATH))
	require.NoError(t, os.WriteFile(bin, data, 0755))
	_, _, err = LocalFiles(bin, root, "bin/service", nil)
	require.ErrorContains(t, err, "libtwo.so")
}

func TestCachedPackageTamperingRejected(t *testing.T) {
	root := t.TempDir()
	bin := putFixture(t, root, "app", syntheticELF(elf.EM_AARCH64, "", nil, ""))
	s, err := NewStore(filepath.Join(root, "store"))
	require.NoError(t, err)
	selected, err := s.ForPlatform("linux/arm64")
	require.NoError(t, err)
	require.NoError(t, selected.CreateFromFiles("app", "1", []File{{HostPath: bin, GuestPath: "app"}}, "", ""))
	files, _, err := s.Resolve("app", "1", "linux/arm64")
	require.NoError(t, err)
	data := syntheticELF(elf.EM_AARCH64, "", nil, "")
	data[1500] = 1
	require.NoError(t, os.WriteFile(files[0].HostPath, data, 0755))
	_, _, err = s.Resolve("app", "1", "linux/arm64")
	require.ErrorContains(t, err, "differs from archive")
}

func TestLocalProgramSymlinkStaysInsideSysroot(t *testing.T) {
	root := t.TempDir()
	real := putFixture(t, root, "usr/service", syntheticELF(elf.EM_AARCH64, "", nil, ""))
	input := filepath.Join(root, "service")
	require.NoError(t, os.Symlink("/usr/service", input))
	files, platform, err := LocalFiles(input, root, "opt/service", nil)
	require.NoError(t, err)
	require.Equal(t, "linux/arm64", platform)
	require.Equal(t, real, files[0].HostPath)
	outside := putFixture(t, t.TempDir(), "outside", syntheticELF(elf.EM_AARCH64, "", nil, ""))
	require.NoError(t, os.Remove(input))
	require.NoError(t, os.Symlink(outside, input))
	_, _, err = LocalFiles(input, root, "opt/service", nil)
	require.Error(t, err)
}

// A project that ships its own README.md on top of an ops package carrying one
// (eyberg/postgresql, eyberg/python3 and eyberg/mongodb all do) must still
// build: the project's copy wins and the package entry is dropped.
func TestContextFileShadowsPackageFileAtSamePath(t *testing.T) {
	root := t.TempDir()
	pkgReadme := putFixture(t, root, "pkg/README.md", []byte("package docs"))
	srcReadme := putFixture(t, root, "src/README.md", []byte("project docs"))
	files := []File{
		{HostPath: pkgReadme, GuestPath: "README.md"},
		{HostPath: srcReadme, GuestPath: "README.md", FromContext: true},
	}
	require.ErrorContains(t, ValidateFiles(files, "linux/arm64"), "different content collides at /README.md")

	resolved := ApplyContextPrecedence(files)
	require.Len(t, resolved, 1)
	require.Equal(t, srcReadme, resolved[0].HostPath)
	require.NoError(t, ValidateFiles(resolved, "linux/arm64"))
}

func TestContextPrecedenceLeavesGenuineCollisions(t *testing.T) {
	root := t.TempDir()
	one := putFixture(t, root, "one/conf", []byte("one"))
	two := putFixture(t, root, "two/conf", []byte("two"))
	other := putFixture(t, root, "one/other", []byte("other"))

	// Two packages at one path: no precedence to apply, so the collision stands.
	pkgs := []File{{HostPath: one, GuestPath: "conf"}, {HostPath: two, GuestPath: "conf"}}
	require.Equal(t, pkgs, ApplyContextPrecedence(pkgs))
	require.ErrorContains(t, ValidateFiles(pkgs, "linux/arm64"), "different content collides at /conf")

	// Two context files at one path: likewise the caller's own conflict.
	ctx := []File{
		{HostPath: one, GuestPath: "conf", FromContext: true},
		{HostPath: two, GuestPath: "conf", FromContext: true},
	}
	require.Equal(t, ctx, ApplyContextPrecedence(ctx))
	require.ErrorContains(t, ValidateFiles(ctx, "linux/arm64"), "different content collides at /conf")

	// Shadowing is per guest path: untouched package files and the directory
	// entries that mkfs needs all survive, in order.
	mixed := []File{
		{HostPath: one, GuestPath: "conf"},
		{HostPath: other, GuestPath: "other"},
		{GuestPath: "db", IsDir: true},
		{HostPath: two, GuestPath: "./conf", FromContext: true},
	}
	resolved := ApplyContextPrecedence(mixed)
	require.Equal(t, []File{mixed[1], mixed[2], mixed[3]}, resolved)
	require.NoError(t, ValidateFiles(resolved, "linux/arm64"))
}

// Guest paths default to the host basename, and that is the path shadowing has
// to compare on — the ops store maps a package's top-level files that way.
func TestContextPrecedenceUsesEffectiveGuestPath(t *testing.T) {
	root := t.TempDir()
	pkgReadme := putFixture(t, root, "pkg/README.md", []byte("package docs"))
	srcReadme := putFixture(t, root, "src/README.md", []byte("project docs"))
	resolved := ApplyContextPrecedence([]File{
		{HostPath: pkgReadme},
		{HostPath: srcReadme, GuestPath: "/README.md", FromContext: true},
	})
	require.Len(t, resolved, 1)
	require.Equal(t, srcReadme, resolved[0].HostPath)
}
