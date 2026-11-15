//go:build !windows

package wsldistro

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests replace wsl.exe with a script that fails the ways the real one
// does (it is missing, it exits non-zero with a UTF-16 message, it prints
// nothing, it prints rubbish) and check that every operation reports the
// failure with WSL's own words readable in it, and never takes a failed
// command's output for a result.

// fakeWSL puts a wsl command first on PATH that prints stdout, exits with
// code, records its arguments and keeps what it was fed on stdin.
type fakeWSL struct {
	dir string
}

func newFakeWSL(t *testing.T, stdout []byte, code string) *fakeWSL {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "stdout"), stdout, 0o600))
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$@\" >> '" + dir + "/args'\n" +
		"cat > '" + dir + "/stdin'\n" +
		"cat '" + dir + "/stdout'\n" +
		"exit " + code + "\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "wsl"), []byte(script), 0o700)) //nolint:gosec // the test's own script must be executable
	t.Setenv("PATH", dir+string(os.PathListSeparator)+"/bin"+string(os.PathListSeparator)+"/usr/bin")
	return &fakeWSL{dir: dir}
}

func (f *fakeWSL) args(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(f.dir, "args"))
	if errors.Is(err, os.ErrNotExist) {
		return ""
	}
	require.NoError(t, err)
	return string(data)
}

func (f *fakeWSL) stdin(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(f.dir, "stdin"))
	require.NoError(t, err)
	return data
}

// utf16 encodes s the way wsl.exe writes its own messages.
func utf16(s string) []byte {
	out := make([]byte, 0, 2*len(s))
	for _, r := range s {
		out = append(out, byte(r), 0) //nolint:gosec // ASCII test strings
	}
	return out
}

// noWSL leaves PATH without any wsl command, as on a machine where the
// Windows feature is not installed.
func noWSL(t *testing.T) {
	t.Helper()
	t.Setenv("PATH", t.TempDir())
}

func TestFaults_WSLMissing(t *testing.T) {
	noWSL(t)
	rootfs := filepath.Join(t.TempDir(), "rootfs.tar.gz")
	require.NoError(t, os.WriteFile(rootfs, []byte("x"), 0o600))
	binary := filepath.Join(t.TempDir(), "jerboad")
	require.NoError(t, os.WriteFile(binary, []byte("x"), 0o600))

	ops := map[string]func() error{
		"list":       func() error { _, err := List(); return err },
		"exists":     func() error { _, err := Exists(); return err },
		"ip":         func() error { _, err := IP(); return err },
		"import":     func() error { return Import(filepath.Join(t.TempDir(), "distro"), rootfs) },
		"install":    func() error { return InstallDaemonBinary(binary) },
		"export":     func() error { return ExportData(&bytes.Buffer{}) },
		"importdata": func() error { return ImportData(strings.NewReader("x")) },
		"unregister": Unregister,
	}
	for name, op := range ops {
		t.Run(name, func(t *testing.T) {
			err := op()
			require.Error(t, err)
			assert.Contains(t, err.Error(), "wsldistro:")
			assert.Contains(t, err.Error(), "wsl", "the error must say which program is missing")
			assert.NotContains(t, err.Error(), "\n")
		})
	}
}

// wsl.exe exits non-zero and explains why in UTF-16. The explanation must be
// readable in the error, not a string of NUL bytes.
func TestFaults_WSLFails(t *testing.T) {
	const reason = "The Windows Subsystem for Linux has not been enabled."
	rootfs := filepath.Join(t.TempDir(), "rootfs.tar.gz")
	require.NoError(t, os.WriteFile(rootfs, []byte("x"), 0o600))
	binary := filepath.Join(t.TempDir(), "jerboad")
	require.NoError(t, os.WriteFile(binary, []byte("x"), 0o600))

	ops := map[string]func() error{
		"list":       func() error { _, err := List(); return err },
		"exists":     func() error { _, err := Exists(); return err },
		"ip":         func() error { _, err := IP(); return err },
		"import":     func() error { return Import(filepath.Join(t.TempDir(), "distro"), rootfs) },
		"install":    func() error { return InstallDaemonBinary(binary) },
		"importdata": func() error { return ImportData(strings.NewReader("x")) },
		"unregister": Unregister,
	}
	for name, op := range ops {
		t.Run(name, func(t *testing.T) {
			newFakeWSL(t, utf16(reason+"\r\n"), "1")
			err := op()
			require.Error(t, err)
			assert.Contains(t, err.Error(), reason)
			assert.NotContains(t, err.Error(), "\x00")
			assert.Contains(t, err.Error(), "exit status 1")
		})
	}
}

// A distro list that failed must never be read as "the distro is not
// installed": the caller would then import over an existing distro.
func TestFaults_ExistsDoesNotGuess(t *testing.T) {
	newFakeWSL(t, utf16("jerboa\r\n"), "3")
	exists, err := Exists()
	require.Error(t, err)
	assert.False(t, exists)

	cases := map[string]struct {
		out  []byte
		want bool
	}{
		"present":                {utf16("Ubuntu\r\njerboa\r\n"), true},
		"present, other case":    {utf16("Ubuntu\r\nJerboa\r\n"), true},
		"absent":                 {utf16("Ubuntu\r\ndocker-desktop\r\n"), false},
		"similar name":           {utf16("jerboa-old\r\nmy-jerboa\r\n"), false},
		"no distros":             {nil, false},
		"blank lines":            {utf16("\r\n\r\n  \r\n"), false},
		"plain utf-8":            {[]byte("jerboa\n"), true},
		"name as part of a line": {utf16("Ubuntu jerboa\r\n"), false},
		"binary rubbish":         {[]byte{0xff, 0xfe, 0x00, 0xd8, 0x00, 0x00, '\n'}, false},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			newFakeWSL(t, c.out, "0")
			exists, err := Exists()
			require.NoError(t, err)
			assert.Equal(t, c.want, exists)
		})
	}
}

// The distro's address is the first one it reports; a distro that is up but
// has no address yet is an error, not an empty address to dial.
func TestFaults_IP(t *testing.T) {
	cases := map[string]struct {
		out  string
		want string
	}{
		"one address":       {"172.20.0.2 \n", "172.20.0.2"},
		"several addresses": {"172.20.0.2 10.0.0.9 fd00::1\n", "172.20.0.2"},
		"none yet":          {"\n", ""},
		"nothing at all":    {"", ""},
		"only blanks":       {"  \t \n", ""},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFakeWSL(t, utf16(c.out), "0")
			ip, err := IP()
			if c.want == "" {
				require.ErrorContains(t, err, "has no IP yet")
				assert.Empty(t, ip)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, c.want, ip)
			assert.Contains(t, f.args(t), "-d\n"+Name+"\n", "the address asked for must be the jerboa distro's")
		})
	}
}

// Import refuses a rootfs that is not there before it creates anything or
// runs wsl, and reports where it was told to look.
func TestFaults_ImportPreconditions(t *testing.T) {
	t.Run("rootfs missing", func(t *testing.T) {
		f := newFakeWSL(t, nil, "0")
		installDir := filepath.Join(t.TempDir(), "distro")
		missing := filepath.Join(t.TempDir(), "absent.tar.gz")
		err := Import(installDir, missing)
		require.ErrorIs(t, err, os.ErrNotExist)
		assert.Contains(t, err.Error(), missing)
		assert.Empty(t, f.args(t), "wsl was run without a rootfs to import")
		_, statErr := os.Stat(installDir)
		require.ErrorIs(t, statErr, os.ErrNotExist, "the install directory was created for an import that cannot happen")
	})
	t.Run("install dir cannot be created", func(t *testing.T) {
		f := newFakeWSL(t, nil, "0")
		rootfs := filepath.Join(t.TempDir(), "rootfs.tar.gz")
		require.NoError(t, os.WriteFile(rootfs, []byte("x"), 0o600))
		blocker := filepath.Join(t.TempDir(), "file")
		require.NoError(t, os.WriteFile(blocker, nil, 0o600))
		err := Import(filepath.Join(blocker, "distro"), rootfs)
		require.ErrorContains(t, err, "create install dir")
		assert.Empty(t, f.args(t))
	})
	t.Run("passes what it was given", func(t *testing.T) {
		f := newFakeWSL(t, nil, "0")
		rootfs := filepath.Join(t.TempDir(), "rootfs.tar.gz")
		require.NoError(t, os.WriteFile(rootfs, []byte("x"), 0o600))
		installDir := filepath.Join(t.TempDir(), "a dir with spaces")
		require.NoError(t, Import(installDir, rootfs))
		assert.Equal(t, "--import\n"+Name+"\n"+installDir+"\n"+rootfs+"\n--version\n2\n", f.args(t))
	})
}

// The daemon binary is streamed to the distro on stdin and moved into place
// only if the whole copy succeeded, so an interrupted update cannot leave a
// truncated jerboad as the installed one.
func TestFaults_InstallDaemonBinary(t *testing.T) {
	payload := bytes.Repeat([]byte("jerboad binary "), 4096)
	binary := filepath.Join(t.TempDir(), "jerboad")
	require.NoError(t, os.WriteFile(binary, payload, 0o600))

	f := newFakeWSL(t, nil, "0")
	require.NoError(t, InstallDaemonBinary(binary))
	assert.Equal(t, payload, f.stdin(t), "the binary must reach the distro byte for byte")
	script := f.args(t)
	assert.Contains(t, script, "cat > "+DaemonBinaryPath+".new && chmod 0755 "+DaemonBinaryPath+".new && mv -f "+DaemonBinaryPath+".new "+DaemonBinaryPath,
		"the binary must be written beside its destination and renamed over it only on success")
	assert.Contains(t, script, "-u\nroot\n")

	newFakeWSL(t, utf16("cat: write error: No space left on device\n"), "1")
	err := InstallDaemonBinary(binary)
	require.ErrorContains(t, err, "No space left on device")
}

// Exporting the distro's data writes the archive to the caller and nothing
// else: a failure must not be reported as an empty, successful export.
func TestFaults_ExportData(t *testing.T) {
	t.Run("archive passes through", func(t *testing.T) {
		archive := []byte{0x1f, 0x8b, 0x08, 0x00, 0x00, 0x00, 0xff, 0x00, 0x01}
		f := newFakeWSL(t, archive, "0")
		var out bytes.Buffer
		require.NoError(t, ExportData(&out))
		assert.Equal(t, archive, out.Bytes(), "the archive must not be decoded or altered")
		for _, dir := range DataDirs {
			assert.Contains(t, string(f.stdin(t)), dir)
		}
		assert.NotContains(t, string(f.stdin(t)), "tools", "the tools cache must not be exported")
	})
	t.Run("nothing to export", func(t *testing.T) {
		newFakeWSL(t, nil, "0")
		var out bytes.Buffer
		require.NoError(t, ExportData(&out))
		assert.Zero(t, out.Len())
	})
	t.Run("tar fails half way", func(t *testing.T) {
		newFakeWSL(t, []byte("partial archive"), "2")
		var out bytes.Buffer
		err := ExportData(&out)
		require.ErrorContains(t, err, "export data")
		assert.Contains(t, err.Error(), "exit status 2")
	})
	t.Run("destination fails", func(t *testing.T) {
		newFakeWSL(t, bytes.Repeat([]byte("x"), 1<<20), "0")
		err := ExportData(failingWriter{})
		require.Error(t, err, "a write error on the destination was swallowed")
	})
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("disk full") }

// Importing data feeds the archive to tar inside the distro; tar refusing it
// is reported with tar's message.
func TestFaults_ImportData(t *testing.T) {
	archive := bytes.Repeat([]byte("archive"), 10000)
	f := newFakeWSL(t, nil, "0")
	require.NoError(t, ImportData(bytes.NewReader(archive)))
	assert.Equal(t, archive, f.stdin(t))

	newFakeWSL(t, utf16("tar: Unexpected EOF in archive\n"), "2")
	err := ImportData(bytes.NewReader(archive[:10]))
	require.ErrorContains(t, err, "Unexpected EOF in archive")
}

// --- .wslconfig ---

func wslHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	return home
}

// A .wslconfig that cannot be read or written is reported with its path, and
// is never replaced by a fresh file that would drop the user's settings.
func TestFaults_WSLConfigUnusable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores file modes")
	}
	t.Run("unreadable", func(t *testing.T) {
		home := wslHome(t)
		path := filepath.Join(home, ".wslconfig")
		require.NoError(t, os.WriteFile(path, []byte("[wsl2]\nmemory=8GB\n"), 0o000))
		changed, got, err := EnsureNestedVirtualization()
		require.ErrorContains(t, err, path)
		assert.False(t, changed)
		assert.Equal(t, path, got)
		require.NoError(t, os.Chmod(path, 0o600))
		data, err := os.ReadFile(path)
		require.NoError(t, err)
		assert.Equal(t, "[wsl2]\nmemory=8GB\n", string(data))
	})
	t.Run("read-only file", func(t *testing.T) {
		home := wslHome(t)
		path := filepath.Join(home, ".wslconfig")
		require.NoError(t, os.WriteFile(path, []byte("[wsl2]\nmemory=8GB\n"), 0o400))
		changed, _, err := EnsureNestedVirtualization()
		require.ErrorContains(t, err, "write "+path)
		assert.False(t, changed, "a write that failed was reported as a change")
		data, err := os.ReadFile(path)
		require.NoError(t, err)
		assert.Equal(t, "[wsl2]\nmemory=8GB\n", string(data))
	})
	t.Run("read-only home", func(t *testing.T) {
		home := wslHome(t)
		require.NoError(t, os.Chmod(home, 0o500))
		t.Cleanup(func() { _ = os.Chmod(home, 0o700) })
		changed, _, err := EnsureNestedVirtualization()
		require.Error(t, err)
		assert.False(t, changed)
	})
	t.Run("is a directory", func(t *testing.T) {
		home := wslHome(t)
		require.NoError(t, os.Mkdir(filepath.Join(home, ".wslconfig"), 0o700))
		changed, _, err := EnsureNestedVirtualization()
		require.Error(t, err)
		assert.False(t, changed)
	})
	t.Run("already set changes nothing", func(t *testing.T) {
		home := wslHome(t)
		path := filepath.Join(home, ".wslconfig")
		// Read-only on purpose: a file that needs no change must not be
		// rewritten at all.
		require.NoError(t, os.WriteFile(path, []byte("[wsl2]\nnestedVirtualization = TRUE\n"), 0o400))
		changed, _, err := EnsureNestedVirtualization()
		require.NoError(t, err)
		assert.False(t, changed)
	})
}

// Malformed and unusual .wslconfig contents: whatever is there, the result
// enables nested virtualization in [wsl2], keeps every other line the user
// wrote, and applying it again changes nothing.
func TestFaults_WSLConfigMalformed(t *testing.T) {
	inputs := map[string]string{
		"empty":                    "",
		"only blanks":              "\n\n   \n",
		"no trailing newline":      "[wsl2]\nmemory=8GB",
		"crlf":                     "[wsl2]\r\nmemory=8GB\r\n[experimental]\r\nsparseVhd=true\r\n",
		"bom":                      bom + "[wsl2]\nmemory=8GB\n",
		"key in the wrong section": "[experimental]\nnestedVirtualization=true\n",
		"key before any section":   "nestedVirtualization=true\n[wsl2]\nmemory=4GB\n",
		"false":                    "[wsl2]\nnestedVirtualization=false\n",
		"no value":                 "[wsl2]\nnestedVirtualization=\n",
		"commented out":            "[wsl2]\n# nestedVirtualization=false\n",
		"two wsl2 sections":        "[wsl2]\nmemory=1GB\n[other]\nx=1\n[wsl2]\nnestedVirtualization=false\n",
		"unterminated header":      "[wsl2\nmemory=8GB\n",
		"header with comment":      "[wsl2] # mine\nmemory=8GB\n",
		"not ini at all":           "{\"wsl2\": {\"memory\": \"8GB\"}}\n",
		"binary":                   "\x00\x01\x02[wsl2]\xff\n",
		"upper case section":       "[WSL2]\nMemory=8GB\n",
	}
	for name, input := range inputs {
		t.Run(name, func(t *testing.T) {
			out, changed := setNestedVirtualization(input)
			require.True(t, changed)
			requireNestedVirtEnabled(t, out)
			for _, line := range strings.Split(strings.TrimPrefix(input, bom), "\n") {
				trimmed := strings.TrimSpace(line)
				if trimmed == "" || isNestedVirtKey(trimmed) {
					continue
				}
				assert.Contains(t, out, strings.TrimRight(line, "\r"), "a line the user wrote was lost")
			}
			again, changedAgain := setNestedVirtualization(out)
			assert.False(t, changedAgain, "the edit is not stable: a second pass changed %q", out)
			assert.Equal(t, out, again)
		})
	}
}

// requireNestedVirtEnabled reads content the way WSL does: the key counts
// only inside a [wsl2] section.
func requireNestedVirtEnabled(t *testing.T, content string) {
	t.Helper()
	section := ""
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
			section = strings.ToLower(trimmed)
			continue
		}
		if section == "[wsl2]" && isNestedVirtKey(trimmed) && nestedVirtTrue(trimmed) {
			return
		}
	}
	require.Failf(t, "nested virtualization is not enabled in [wsl2]", "%q", content)
}

// FuzzSetNestedVirtualization holds the .wslconfig edit to its contract on
// arbitrary files: the result enables the setting and a second pass is a
// no-op, so repeated installs never keep growing or rewriting the file.
func FuzzSetNestedVirtualization(f *testing.F) {
	for _, seed := range []string{"", "[wsl2]\n", "[wsl2]\nnestedVirtualization=false\n", "[a]\n[wsl2]\n[b]\n", bom + "[wsl2]\r\n", "nestedVirtualization=true", "[wsl2]\nNESTEDVIRTUALIZATION = True \n", "[wsl2]\n=\n", "[]\n", "[wsl2]]\n"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, input string) {
		out, changed := setNestedVirtualization(input)
		if !changed {
			if out != input {
				t.Fatalf("reported unchanged but differs: %q -> %q", input, out)
			}
			return
		}
		again, changedAgain := setNestedVirtualization(out)
		if changedAgain || again != out {
			t.Fatalf("not idempotent: %q -> %q -> %q", input, out, again)
		}
	})
}
