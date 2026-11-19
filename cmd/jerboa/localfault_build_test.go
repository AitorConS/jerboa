package main

import (
	"archive/tar"
	"bytes"
	"context"
	"debug/elf"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/AitorConS/jerboa/internal/api"
	"github.com/AitorConS/jerboa/internal/builder"
	pkg "github.com/AitorConS/jerboa/internal/package"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests cover a build going wrong on the client's side: files of the
// build context that vanish or cannot be read, source trees with links that
// loop or lead out of the project, a toolchain that fails, and a daemon that
// fails while the context is being uploaded.

// lfWrite creates a file with its parent directories.
func lfWrite(t *testing.T, path, content string) string {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	return path
}

// lfSkipWithoutPermissions skips tests that rely on file modes being
// enforced, which they are not for root or on Windows.
func lfSkipWithoutPermissions(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("file modes are not enforced here")
	}
}

// lfELF writes the smallest ELF executable for this machine's architecture,
// which is what an image built here must contain.
func lfELF(t *testing.T, path string) string {
	t.Helper()
	machine := elf.EM_X86_64
	if runtime.GOARCH == "arm64" {
		machine = elf.EM_AARCH64
	}
	buf := make([]byte, 64)
	copy(buf, elf.ELFMAG)
	buf[elf.EI_CLASS] = byte(elf.ELFCLASS64)
	buf[elf.EI_DATA] = byte(elf.ELFDATA2LSB)
	buf[elf.EI_VERSION] = 1
	binary.LittleEndian.PutUint16(buf[16:], uint16(elf.ET_EXEC))
	binary.LittleEndian.PutUint16(buf[18:], uint16(machine))
	binary.LittleEndian.PutUint32(buf[20:], 1)
	binary.LittleEndian.PutUint16(buf[52:], 64)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, buf, 0o700)) //nolint:gosec // stands in for an executable
	return path
}

// lfTarEntries reads a context archive to its end.
func lfTarEntries(r io.Reader) (map[string]string, error) {
	out := map[string]string{}
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return out, err //nolint:wrapcheck // the caller asserts on the stream's own error
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			return out, err //nolint:wrapcheck // as above
		}
		out[hdr.Name] = string(data)
	}
}

// --- the context archive ---

// The archive streamed to the daemon holds the program under the reserved
// name and every file once, at its guest path.
func TestLocalFault_BuildContextContents(t *testing.T) {
	dir := t.TempDir()
	program := lfWrite(t, filepath.Join(dir, "bin", "app"), "program-bytes")
	lib := lfWrite(t, filepath.Join(dir, "lib", "libz.so"), "library-bytes")
	bare := lfWrite(t, filepath.Join(dir, "config.json"), "{}")

	r := buildContextReader(pkg.File{HostPath: program}, "bin/app", []pkg.File{
		{HostPath: program, GuestPath: "/bin/app"}, // the program itself: not sent twice
		{HostPath: lib, GuestPath: "usr/lib/libz.so"},
		{HostPath: bare},
		{GuestPath: "var/data", IsDir: true},
	})
	defer func() { _ = r.Close() }()
	entries, err := lfTarEntries(r)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{
		buildProgramPath:  "program-bytes",
		"usr/lib/libz.so": "library-bytes",
		"config.json":     "{}",
		"var/data/":       "",
	}, entries)
}

// A file of the context that is gone, unreadable or not a file by the time
// it is streamed fails the upload with an error naming it. The daemon must
// never be handed an archive that silently lacks a file.
func TestLocalFault_BuildContextUnreadableFiles(t *testing.T) {
	dir := t.TempDir()
	program := lfWrite(t, filepath.Join(dir, "app"), "program")
	good := lfWrite(t, filepath.Join(dir, "good"), "good")
	missing := filepath.Join(dir, "missing")

	cases := map[string]struct {
		program string
		files   []pkg.File
		names   string
	}{
		"program missing":       {missing, nil, missing},
		"program is directory":  {dir, nil, ""},
		"package file missing":  {program, []pkg.File{{HostPath: good, GuestPath: "a"}, {HostPath: missing, GuestPath: "b"}}, missing},
		"package file is a dir": {program, []pkg.File{{HostPath: dir, GuestPath: "b"}}, ""},
	}
	if runtime.GOOS != "windows" && os.Geteuid() != 0 {
		locked := lfWrite(t, filepath.Join(dir, "locked"), "secret")
		require.NoError(t, os.Chmod(locked, 0o000))
		cases["package file unreadable"] = struct {
			program string
			files   []pkg.File
			names   string
		}{program, []pkg.File{{HostPath: locked, GuestPath: "c"}}, locked}

		loop := filepath.Join(dir, "loop")
		require.NoError(t, os.Symlink(loop, loop))
		cases["package file is a symlink loop"] = struct {
			program string
			files   []pkg.File
			names   string
		}{program, []pkg.File{{HostPath: loop, GuestPath: "d"}}, loop}
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			r := buildContextReader(pkg.File{HostPath: c.program}, "", c.files)
			defer func() { _ = r.Close() }()
			_, err := lfTarEntries(r)
			require.Error(t, err, "an incomplete context was streamed as if it were whole")
			assert.Contains(t, err.Error(), c.names)

			if c.program == program {
				s := seedContextReader(c.files)
				defer func() { _ = s.Close() }()
				_, err = lfTarEntries(s)
				require.Error(t, err)
			}
		})
	}
}

// The daemon stops reading (it died, or refused the build) while a large
// context is being streamed: closing the reader must unblock and end the
// writer rather than leave it writing into nothing.
func TestLocalFault_BuildContextReaderClosedEarly(t *testing.T) {
	dir := t.TempDir()
	program := lfWrite(t, filepath.Join(dir, "app"), strings.Repeat("x", 4<<20))
	r := buildContextReader(pkg.File{HostPath: program}, "", nil)
	buf := make([]byte, 1024)
	_, err := io.ReadFull(r, buf)
	require.NoError(t, err)
	require.NoError(t, r.Close())
	_, err = r.Read(buf)
	require.ErrorIs(t, err, io.ErrClosedPipe)
}

// --- the source tree ---

func TestLocalFault_SourceTreeUnreadable(t *testing.T) {
	_, err := sourceFiles(filepath.Join(t.TempDir(), "absent"), builder.LangNode, nil)
	require.ErrorContains(t, err, "walk source dir")

	lfSkipWithoutPermissions(t)
	dir := t.TempDir()
	lfWrite(t, filepath.Join(dir, "index.js"), "1")
	lfWrite(t, filepath.Join(dir, "private", "data.js"), "2")
	require.NoError(t, os.Chmod(filepath.Join(dir, "private"), 0o000))
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(dir, "private"), 0o700) })
	files, err := sourceFiles(dir, builder.LangNode, nil)
	require.Error(t, err, "a directory that could not be read was left out of the image without a word")
	assert.Contains(t, err.Error(), "private")
	assert.Nil(t, files)

	// The same directory, ignored, is no obstacle.
	lfWrite(t, filepath.Join(dir, ".unignore"), "private/\n")
	files, err = sourceFiles(dir, builder.LangNode, nil)
	require.NoError(t, err)
	var guests []string
	for _, f := range files {
		guests = append(guests, f.GuestPath)
	}
	assert.Contains(t, guests, "index.js")
	assert.NotContains(t, strings.Join(guests, " "), "private")

	// An unreadable ignore file is an error, not "nothing is ignored".
	require.NoError(t, os.Chmod(filepath.Join(dir, ".unignore"), 0o000))
	_, err = sourceFiles(dir, builder.LangNode, nil)
	require.ErrorContains(t, err, "load ignore file")
}

// --- helpers of the build command ---

func TestLocalFault_WriteSBOM(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "sbom.json")
	for name, doc := range map[string]string{"empty": "", "truncated": `{"bomFormat":"CycloneDX","compon`, "not json": "<xml/>"} {
		require.Error(t, writeSBOM(out, []byte(doc)), name)
		_, err := os.Stat(out)
		require.ErrorIs(t, err, os.ErrNotExist, "%s: a damaged SBOM was written to the file the user asked for", name)
	}
	require.ErrorContains(t, writeSBOM(filepath.Join(dir, "no", "such", "dir", "sbom.json"), []byte(`{}`)), "write sbom")
	require.Error(t, writeSBOM(dir, []byte(`{}`)), "a directory is not a file to write to")

	require.NoError(t, writeSBOM(out, []byte(`{"bomFormat":"CycloneDX"}`)))
	data, err := os.ReadFile(out)
	require.NoError(t, err)
	assert.JSONEq(t, `{"bomFormat":"CycloneDX"}`, string(data))
	assert.True(t, bytes.HasSuffix(data, []byte("\n")))
}

func TestLocalFault_RunBuildCommand(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses sh")
	}
	off := builder.Sandbox{Off: true}
	dir := t.TempDir()

	var out bytes.Buffer
	err := runBuildCommand(context.Background(), off, false, dir, "echo compiling; echo 'ld: symbol not found' >&2; exit 3", &out)
	require.ErrorContains(t, err, "run command")
	assert.Contains(t, err.Error(), "exit status 3")
	assert.Contains(t, out.String(), "compiling")
	assert.Contains(t, out.String(), "ld: symbol not found", "what the command printed before failing must reach the user")

	out.Reset()
	err = runBuildCommand(context.Background(), off, false, dir, "definitely-not-a-command-xyz", &out)
	require.Error(t, err)
	assert.Contains(t, out.String(), "definitely-not-a-command-xyz")

	// The command runs in the project directory, and a failure in the middle
	// of a script stops it.
	out.Reset()
	err = runBuildCommand(context.Background(), off, false, dir, "set -e; touch before; false; touch after", &out)
	require.Error(t, err)
	_, statErr := os.Stat(filepath.Join(dir, "before"))
	require.NoError(t, statErr)
	_, statErr = os.Stat(filepath.Join(dir, "after"))
	require.ErrorIs(t, statErr, os.ErrNotExist)

	err = runBuildCommand(context.Background(), off, false, filepath.Join(dir, "absent"), "true", &out)
	require.Error(t, err, "a missing project directory must fail the command")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.Error(t, runBuildCommand(ctx, off, false, dir, "exec sleep 60", &out))
}

// --- jerboa build, end to end ---

// lfFakeGo puts a go command on PATH that "compiles" by running body with
// the output path in $OUT.
func lfFakeGo(t *testing.T, body string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake toolchain is a shell script")
	}
	dir := t.TempDir()
	script := "#!/bin/sh\necho run >> '" + dir + "/calls'\nwhile [ $# -gt 0 ] && [ \"$1\" != \"-o\" ]; do shift; done\nOUT=\"$2\"\n" + body + "\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "go"), []byte(script), 0o700)) //nolint:gosec // the test's own script must be executable
	t.Setenv("PATH", dir+string(os.PathListSeparator)+"/bin"+string(os.PathListSeparator)+"/usr/bin")
	return dir
}

// lfGoProject is a Go project whose build steps run without the sandbox, so
// the fake compiler is what runs.
func lfGoProject(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "svc")
	lfWrite(t, filepath.Join(dir, "go.mod"), "module example.com/svc\n")
	lfWrite(t, filepath.Join(dir, "main.go"), "package main\nfunc main() {}\n")
	lfWrite(t, filepath.Join(dir, builder.ConfigFileName), "[build]\nlang = \"go\"\nsandbox = false\n")
	return dir
}

// lfBuild runs jerboa build in an isolated home, keeping the PATH the test
// prepared. It allows the unconfined build steps lfGoProject asks for.
func lfBuild(t *testing.T, endpoint string, args ...string) fdResult {
	t.Helper()
	return lfBuildWith(t, endpoint, append([]string{"--allow-unsandboxed"}, args...)...)
}

// lfBuildWith is lfBuild without --allow-unsandboxed.
func lfBuildWith(t *testing.T, endpoint string, args ...string) fdResult {
	t.Helper()
	path := os.Getenv("PATH")
	fdIsolate(t)
	t.Setenv("PATH", path)
	return fdExec(t, "", append([]string{"--host", endpoint, "build"}, args...)...)
}

// A project's unikernel.toml cannot relax the confinement of its own build
// steps without the operator's flag: the build stops before running
// anything, names the flag, and the flag lets the same build through.
func TestLocalFault_BuildPolicyNeedsTheOperator(t *testing.T) {
	t.Run("sandbox = false", func(t *testing.T) {
		tool := lfFakeGo(t, "exit 1")
		d := fdNew(t).all(fdOK(nil))
		res := lfBuildWith(t, d.endpoint, lfGoProject(t))
		fdRequireCleanFailure(t, res, "[build] sandbox = false", "--allow-unsandboxed")
		assert.Empty(t, d.methods())
		_, err := os.Stat(filepath.Join(tool, "calls"))
		require.ErrorIs(t, err, os.ErrNotExist, "a build step ran unconfined without the flag")

		res = lfBuildWith(t, d.endpoint, "--allow-unsandboxed", lfGoProject(t))
		fdRequireCleanFailure(t, res, "go build")
		assert.Contains(t, res.stderr, "build steps run directly on the host", "the mode must be shown without --verbose")
	})
	t.Run("network = true", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "svc")
		marker := filepath.Join(t.TempDir(), "ran")
		lfWrite(t, filepath.Join(dir, "go.mod"), "module example.com/svc\n")
		lfWrite(t, filepath.Join(dir, builder.ConfigFileName), "[build]\nlang = \"go\"\nnetwork = true\nrun = [\"touch '"+marker+"'\"]\n")
		d := fdNew(t).all(fdOK(nil))
		res := lfBuildWith(t, d.endpoint, dir)
		fdRequireCleanFailure(t, res, "[build] network = true", "--allow-build-network")
		assert.Empty(t, d.methods())
		_, err := os.Stat(marker)
		require.ErrorIs(t, err, os.ErrNotExist, "a [build] run command ran without the flag")
	})
}

// A confined build step cannot change the build's policy files. Where the
// sandbox can refuse the write it does (macOS, and bubblewrap for a file that
// exists); where it cannot (bubblewrap binds only existing files read-only,
// and a host without a sandbox confines nothing) the build fails naming the
// file, and a policy file the step created is removed so it does not steer
// the next build. Either way the daemon is not contacted and the project's
// policy is what it was.
func TestLocalFault_BuildStepWritesThePolicy(t *testing.T) {
	const original = "[build]\nlang = \"go\"\n"
	newProject := func(t *testing.T) string {
		dir := filepath.Join(t.TempDir(), "svc")
		lfWrite(t, filepath.Join(dir, "go.mod"), "module example.com/svc\n")
		lfWrite(t, filepath.Join(dir, "main.go"), "package main\nfunc main() {}\n")
		lfWrite(t, filepath.Join(dir, builder.ConfigFileName), original)
		return dir
	}
	// refused reports whether the sandbox stopped the write itself: the
	// build then fails as the step's own failure, not as a policy change.
	refused := func(res fdResult) bool {
		return res.err != nil && !strings.Contains(res.err.Error(), "policy files")
	}
	t.Run("creates .unignore", func(t *testing.T) {
		lfFakeGo(t, "printf '!.env\\n' > .unignore\nexit 1")
		project := newProject(t)
		d := fdNew(t).all(fdOK(nil))
		res := lfBuildWith(t, d.endpoint, project)
		if refused(res) {
			fdRequireCleanFailure(t, res, "go build")
		} else {
			fdRequireCleanFailure(t, res, "policy files", builder.UnignoreFile, "created; removed")
		}
		assert.Empty(t, d.methods())
		_, err := os.Lstat(filepath.Join(project, builder.UnignoreFile))
		require.ErrorIs(t, err, os.ErrNotExist, "the created policy file was left for the next build")
	})
	t.Run("rewrites unikernel.toml", func(t *testing.T) {
		lfFakeGo(t, "printf '[build]\\nlang = \"go\"\\nsandbox = false\\n' > "+builder.ConfigFileName+"\nexit 1")
		project := newProject(t)
		d := fdNew(t).all(fdOK(nil))
		res := lfBuildWith(t, d.endpoint, project)
		if refused(res) {
			fdRequireCleanFailure(t, res, "go build")
			got, err := os.ReadFile(filepath.Join(project, builder.ConfigFileName))
			require.NoError(t, err)
			assert.Equal(t, original, string(got), "the sandbox let the step rewrite the policy and the build did not notice")
		} else {
			fdRequireCleanFailure(t, res, "policy files", builder.ConfigFileName)
		}
		assert.Empty(t, d.methods())
	})
}

// Whatever is wrong with what the user points the build at, the daemon is
// not contacted and the error says what is wrong.
func TestLocalFault_BuildRejectsBadInputBeforeTheDaemon(t *testing.T) {
	dir := t.TempDir()
	binary := lfELF(t, filepath.Join(dir, "app"))
	script := lfWrite(t, filepath.Join(dir, "run.sh"), "#!/bin/sh\necho hi\n")
	empty := lfWrite(t, filepath.Join(dir, "empty"), "")
	emptyDir := filepath.Join(dir, "emptydir")
	require.NoError(t, os.Mkdir(emptyDir, 0o700))
	badConfig := filepath.Join(dir, "badcfg")
	lfWrite(t, filepath.Join(badConfig, builder.ConfigFileName), "[build\nlang = ")
	badLang := filepath.Join(dir, "badlang")
	lfWrite(t, filepath.Join(badLang, builder.ConfigFileName), "[build]\nlang = \"cobol\"\n")
	sbomDir := filepath.Join(dir, "sbomdir")
	require.NoError(t, os.Mkdir(sbomDir, 0o700))

	cases := []struct {
		name string
		args []string
		want string
	}{
		{"path does not exist", []string{filepath.Join(dir, "absent")}, "absent"},
		{"not an ELF", []string{script}, "elf"},
		{"empty file", []string{empty}, "build"},
		{"config flag with a binary", []string{"-f", filepath.Join(badConfig, builder.ConfigFileName), binary}, "only applies when building from a source directory"},
		{"unknown package source", []string{"--pkg-source", "nowhere", binary}, "nowhere"},
		{"unknown platform", []string{"--platform", "plan9/mips", binary}, "plan9/mips"},
		{"platform of another architecture", []string{"--platform", lfOtherPlatform(), binary}, "conflicts"},
		{"unknown layout", []string{"--layout", "floppy", binary}, "floppy"},
		{"unknown size report", []string{"--size-report=yaml", binary}, "--size-report must be text or json"},
		{"sbom path is a directory", []string{"--sbom", sbomDir, binary}, "write sbom"},
		{"directory without a project", []string{emptyDir}, "build"},
		{"damaged configuration", []string{badConfig}, builder.ConfigFileName},
		{"configuration names an unknown language", []string{badLang}, "cobol"},
		{"configuration file missing", []string{"-f", filepath.Join(dir, "nope.toml"), emptyDir}, "nope.toml"},
		{"unknown language flag", []string{"--lang", "cobol", emptyDir}, "cobol"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := fdNew(t).all(fdOK(nil))
			res := lfBuild(t, d.endpoint, c.args...)
			fdRequireCleanFailure(t, res, c.want)
			assert.Empty(t, d.methods(), "a build that cannot succeed reached the daemon")
			assert.Empty(t, res.stdout)
		})
	}
}

func lfOtherPlatform() string {
	if runtime.GOARCH == "arm64" {
		return "linux/amd64"
	}
	return "linux/arm64"
}

// The compiler fails: the build stops there, shows why, and uploads nothing.
func TestLocalFault_BuildToolchainFails(t *testing.T) {
	t.Run("compiler error", func(t *testing.T) {
		tool := lfFakeGo(t, "echo './main.go:2:1: undefined: nope' >&2\nexit 1")
		d := fdNew(t).all(fdOK(nil))
		res := lfBuild(t, d.endpoint, "--verbose", lfGoProject(t))
		fdRequireCleanFailure(t, res, "go build")
		assert.Contains(t, res.stderr, "undefined: nope", "the compiler's message must be shown")
		assert.Empty(t, d.methods())
		calls, err := os.ReadFile(filepath.Join(tool, "calls"))
		require.NoError(t, err)
		assert.Equal(t, "run\n", string(calls))
	})
	t.Run("compiler not installed", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("PATH handling differs")
		}
		t.Setenv("PATH", t.TempDir())
		d := fdNew(t).all(fdOK(nil))
		res := lfBuild(t, d.endpoint, lfGoProject(t))
		fdRequireCleanFailure(t, res, "go build", "executable file not found")
		assert.Empty(t, d.methods())
	})
	t.Run("compiler writes something that is not a program", func(t *testing.T) {
		lfFakeGo(t, "printf 'not an elf' > \"$OUT\"")
		d := fdNew(t).all(fdOK(nil))
		res := lfBuild(t, d.endpoint, lfGoProject(t))
		fdRequireCleanFailure(t, res, "build")
		assert.Empty(t, d.methods(), "something that is not a program was uploaded")
	})
}

// The program is built and the daemon then fails in each way it can while
// the image is assembled. The build fails with the daemon's reason, prints
// no image as built, and leaves no build artifact in the project.
func TestLocalFault_BuildDaemonFailsDuringAssembly(t *testing.T) {
	elfPath := lfELF(t, filepath.Join(t.TempDir(), "prebuilt"))
	drainThen := func(h fdHandler) fdHandler {
		return func(n int, req api.Request) fdReply {
			rep := h(n, req)
			rep.drain = true
			return rep
		}
	}
	failures := map[string]struct {
		handler fdHandler
		want    string
	}{
		// A daemon that answers and hangs up without reading the context races
		// the client's upload: the client reports either the daemon's refusal
		// or the broken pipe, depending on which it meets first. The real
		// daemon drains the context before refusing, which is the next case.
		"refuses before reading the context": {fdFail(-32601, "image build disabled"), "build"},
		"refuses after reading the context":  {drainThen(fdFail(-32601, "image build disabled")), "image build disabled"},
		"hangs up before reading":            {fdHangup(), "build"},
		"mkfs fails":                         {drainThen(fdFail(-32000, "mkfs: no space left on device")), "no space left on device"},
		"dies after reading the context":     {drainThen(fdHangup()), "build"},
		"answers half a response":            {drainThen(fdTruncated()), "build"},
		"answers something else":             {drainThen(fdRaw("HTTP/1.1 500 Internal Server Error\r\n\r\n")), "build"},
	}
	for name, f := range failures {
		t.Run(name, func(t *testing.T) {
			lfFakeGo(t, "cp '"+elfPath+"' \"$OUT\"")
			project := lfGoProject(t)
			d := fdNew(t).on("Image.Build", f.handler)
			res := lfBuild(t, d.endpoint, "--no-preflight", project)
			fdRequireCleanFailure(t, res, f.want)
			assert.Empty(t, res.stdout, "an image was reported as built")
			assert.Equal(t, 1, d.count("Image.Build"), "a failed assembly must not be retried behind the user's back")
			_, err := os.Stat(filepath.Join(project, ".jerboa-build-binary"))
			require.ErrorIs(t, err, os.ErrNotExist, "the compiled binary was left in the project")
		})
	}

	t.Run("daemon unreachable", func(t *testing.T) {
		lfFakeGo(t, "cp '"+elfPath+"' \"$OUT\"")
		project := lfGoProject(t)
		res := lfBuild(t, "unix://"+filepath.Join(fdShortDir(t), "absent.sock"), "--no-preflight", project)
		fdRequireCleanFailure(t, res, "build: connect to daemon")
		_, err := os.Stat(filepath.Join(project, ".jerboa-build-binary"))
		require.ErrorIs(t, err, os.ErrNotExist)
	})

	t.Run("success", func(t *testing.T) {
		lfFakeGo(t, "cp '"+elfPath+"' \"$OUT\"")
		project := lfGoProject(t)
		d := fdNew(t).on("Image.Build", drainThen(fdOK(api.ImageManifestResult{Name: "svc", Tag: "latest", DiskDigest: "sha256:abc", DiskSize: 4096})))
		sbomPath := filepath.Join(t.TempDir(), "sbom.json")
		res := lfBuild(t, d.endpoint, "--no-preflight", "--memory", "512M", "--sbom", sbomPath, project)
		require.NoError(t, res.err, "stderr: %s", res.stderr)
		assert.Contains(t, res.stdout, "sha256:abc  svc:latest")

		var p api.BuildParams
		d.params("Image.Build", 0, &p)
		assert.Equal(t, "svc", p.Name, "the image is named after the project directory")
		assert.Equal(t, "latest", p.Tag)
		assert.Equal(t, "512M", p.Memory)
		assert.Equal(t, buildProgramPath, p.Program)
		assert.Equal(t, "linux/"+runtime.GOARCH, p.Platform)
		data, err := os.ReadFile(sbomPath)
		require.NoError(t, err)
		assert.True(t, json.Valid(data), "the SBOM written for the user is not JSON")
		_, err = os.Stat(filepath.Join(project, ".jerboa-build-binary"))
		require.ErrorIs(t, err, os.ErrNotExist)
	})
}
