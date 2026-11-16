//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
)

// sbPlan and sbReport mirror testdata/sbprobe.
type sbPlan struct {
	WriteFiles  []string `json:"write_files"`
	Content     string   `json:"content"`
	ReadFiles   []string `json:"read_files"`
	UnixSockets []string `json:"unix_sockets"`
	TCPAddrs    []string `json:"tcp_addrs"`
}

type sbAttempt struct {
	Target string `json:"target"`
	Error  string `json:"error"`
	Data   string `json:"data"`
}

type sbReport struct {
	Fatal   string      `json:"fatal"`
	Writes  []sbAttempt `json:"writes"`
	Reads   []sbAttempt `json:"reads"`
	Sockets []sbAttempt `json:"sockets"`
	TCP     []sbAttempt `json:"tcp"`
	TempDir sbAttempt   `json:"temp_dir"`
}

// sandboxProject is a project whose [build] run step is sbprobe.
type sandboxProject struct {
	e   *env
	dir string
}

// requireBuildSandbox makes sure the build steps of this run are confined
// by the mechanism of the platform, bubblewrap on Linux: on a host without
// it Jerboa runs the steps directly with a warning, and every assertion
// about confinement would describe a host that has none.
func requireBuildSandbox(t *testing.T) string {
	t.Helper()
	switch runtime.GOOS {
	case "linux":
		bwrap, err := exec.LookPath("bwrap")
		if err != nil {
			missing(t, "bubblewrap (bwrap) is not installed")
		}
		if out, err := exec.Command(bwrap, "--ro-bind", "/", "/", "--unshare-net", "--", "true").CombinedOutput(); err != nil {
			missing(t, "bubblewrap cannot create a sandbox here (user namespaces disabled?): %v: %s", err, out)
		}
		return "bubblewrap"
	case "darwin":
		return "sandbox-exec"
	}
	t.Skipf("no build sandbox on %s", runtime.GOOS)
	return ""
}

// newSandboxProject writes the probe guest's project with sbprobe as its
// build step and extra appended to its [build] table.
func newSandboxProject(e *env, name, extra string, plan sbPlan) *sandboxProject {
	e.t.Helper()
	p := &sandboxProject{e: e, dir: filepath.Join(e.root, name)}
	writeProject(e.t, p.dir, "", "run = [\"./sbprobe\"]\n"+extra)
	build := exec.Command("go", "build", "-o", filepath.Join(p.dir, "sbprobe"), "./testdata/sbprobe")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	out, err := build.CombinedOutput()
	require.NoErrorf(e.t, err, "build sbprobe: %s", out)
	data, err := json.Marshal(plan)
	require.NoError(e.t, err)
	require.NoError(e.t, os.WriteFile(filepath.Join(p.dir, "sbprobe.json"), data, 0o600))
	return p
}

// ran reports whether the build step ran at all.
func (p *sandboxProject) ran() bool {
	_, err := os.Stat(filepath.Join(p.dir, "sbprobe-report.json"))
	return err == nil
}

func (p *sandboxProject) report() sbReport {
	p.e.t.Helper()
	data, err := os.ReadFile(filepath.Join(p.dir, "sbprobe-report.json"))
	require.NoError(p.e.t, err, "the build step left no report: it did not run")
	var r sbReport
	require.NoError(p.e.t, json.Unmarshal(data, &r))
	require.Empty(p.e.t, r.Fatal, "sbprobe could not run its plan")
	return r
}

func (p *sandboxProject) toml() string {
	p.e.t.Helper()
	data, err := os.ReadFile(filepath.Join(p.dir, "unikernel.toml"))
	require.NoError(p.e.t, err)
	return string(data)
}

// hasImage reports whether the store holds an image called name.
func (e *env) hasImage(name string) bool {
	e.t.Helper()
	for _, img := range e.images() {
		if img.Name == name {
			return true
		}
	}
	return false
}

// remoteAddr is an address on another network that nothing answers at
// (TEST-NET-1, RFC 5737): what tells a connection the sandbox refused from
// one the network could not complete.
const remoteAddr = "192.0.2.1:9"

// requireNetworkClosed checks the report of a step that had no network.
// bubblewrap gives the step a network namespace of its own, so not even the
// host answers; sandbox-exec leaves connections to this host open, by
// design, and refuses the others.
func requireNetworkClosed(t *testing.T, tcp []sbAttempt) {
	t.Helper()
	require.Len(t, tcp, 2)
	if runtime.GOOS == "linux" {
		require.NotEmptyf(t, tcp[0].Error, "the step reached the host's network at %s", tcp[0].Target)
		require.NotEmpty(t, tcp[1].Error)
		return
	}
	require.Containsf(t, tcp[1].Error, "operation not permitted", "the sandbox did not refuse the step's connection to %s", tcp[1].Target)
}

// requireNetworkOpen checks the report of a step that was given the network.
func requireNetworkOpen(t *testing.T, tcp []sbAttempt) {
	t.Helper()
	require.Len(t, tcp, 2)
	require.Emptyf(t, tcp[0].Error, "with the network allowed the step must reach %s", tcp[0].Target)
	require.NotContainsf(t, tcp[1].Error, "operation not permitted", "the sandbox refused the step's connection to %s although the network was allowed", tcp[1].Target)
}

// hostListener listens on every address of the host and returns an address
// of it that is not loopback: what a build step would reach on the network.
func hostListener(t *testing.T) string {
	t.Helper()
	var hostIP string
	addrs, err := net.InterfaceAddrs()
	require.NoError(t, err)
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() != nil && !ipn.IP.IsLoopback() && !ipn.IP.IsLinkLocalUnicast() {
			hostIP = ipn.IP.String()
			break
		}
	}
	if hostIP == "" {
		missing(t, "this host has no IPv4 address besides loopback")
	}
	ln, err := net.Listen("tcp4", "0.0.0.0:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()
	addr := net.JoinHostPort(hostIP, fmt.Sprint(ln.Addr().(*net.TCPAddr).Port))
	// The control: from outside any sandbox the address answers.
	conn, err := net.Dial("tcp", addr)
	require.NoErrorf(t, err, "the test's own listener is not reachable at %s", addr)
	_ = conn.Close()
	return addr
}

// TestBuildSandboxConfinesBuildSteps builds a project whose build step tries
// to write outside the project, read Jerboa's and a cloud provider's
// credentials, talk to the daemon and reach the network. With the default
// confinement none of it may work, while the build itself still succeeds
// and its image boots.
func TestBuildSandboxConfinesBuildSteps(t *testing.T) {
	mechanism := requireBuildSandbox(t)
	e := newEnv(t)

	outside := filepath.Join(e.root, "outside")
	require.NoError(t, os.MkdirAll(outside, 0o755))
	jerboaSecret := filepath.Join(e.home, ".jerboa", "registry-credentials")
	require.NoError(t, os.WriteFile(jerboaSecret, []byte("JERBOA-SECRET"), 0o600))
	awsSecret := filepath.Join(e.home, ".aws", "credentials")
	require.NoError(t, os.MkdirAll(filepath.Dir(awsSecret), 0o700))
	require.NoError(t, os.WriteFile(awsSecret, []byte("AWS-SECRET"), 0o600))
	// Not a credential store: the control that reads work at all.
	plain := filepath.Join(outside, "readable")
	require.NoError(t, os.WriteFile(plain, []byte("PLAIN"), 0o600))
	netAddr := hostListener(t)

	p := newSandboxProject(e, "confined", "", sbPlan{
		WriteFiles:  []string{filepath.Join(outside, "escaped"), filepath.Join(e.home, "escaped"), "inside-the-project"},
		Content:     "written by the build step",
		ReadFiles:   []string{jerboaSecret, awsSecret, plain},
		UnixSockets: []string{e.sock},
		TCPAddrs:    []string{netAddr, remoteAddr},
	})
	res := e.run(buildArgs(p.dir, "confined")...)
	require.Zerof(t, res.code, "a confined build must still succeed:\n%s", res.all())
	require.Containsf(t, res.stderr, "build steps run in a sandbox ("+mechanism+")", "the build must say how its steps are confined")

	r := p.report()
	require.Len(t, r.Writes, 3)
	require.NotEmpty(t, r.Writes[0].Error, "the step wrote outside the project")
	require.NoFileExists(t, filepath.Join(outside, "escaped"))
	require.NotEmpty(t, r.Writes[1].Error, "the step wrote to the home directory")
	require.NoFileExists(t, filepath.Join(e.home, "escaped"))
	require.Empty(t, r.Writes[2].Error, "the step must be able to write to the project")
	require.FileExists(t, filepath.Join(p.dir, "inside-the-project"))
	require.Empty(t, r.TempDir.Error, "the step must have a temporary directory it can write to")

	require.Len(t, r.Reads, 3)
	require.NotContains(t, r.Reads[0].Data, "JERBOA-SECRET", "the step read a file under ~/.jerboa")
	require.NotContains(t, r.Reads[1].Data, "AWS-SECRET", "the step read ~/.aws/credentials")
	require.Equal(t, "PLAIN", r.Reads[2].Data, "the step must be able to read the host outside the credential stores")

	require.Len(t, r.Sockets, 1)
	require.NotEmpty(t, r.Sockets[0].Error, "the step connected to the daemon's socket")
	requireNetworkClosed(t, r.TCP)

	// The secrets were hidden from the step, not removed.
	data, err := os.ReadFile(jerboaSecret)
	require.NoError(t, err)
	require.Equal(t, "JERBOA-SECRET", string(data))

	// And what the confined build produced is a working image.
	netName, _ := e.network()
	g := e.startGuest(guestOpts{image: "confined", network: netName, env: []string{"PROBE_MARKER=confined"}})
	require.Equal(t, "confined", g.status().Marker)
	e.stopAndRemove(g.id)
}

// TestBuildSandboxOffNeedsTheOperator checks that a project cannot switch
// its own confinement off: [build] sandbox = false is refused before any
// build step runs, and takes effect only with --allow-unsandboxed.
func TestBuildSandboxOffNeedsTheOperator(t *testing.T) {
	requireBuildSandbox(t)
	e := newEnv(t)
	outside := filepath.Join(e.root, "outside")
	require.NoError(t, os.MkdirAll(outside, 0o755))
	escaped := filepath.Join(outside, "escaped")
	p := newSandboxProject(e, "unconfined", "sandbox = false\n", sbPlan{WriteFiles: []string{escaped}, Content: "x"})

	out := e.fails(buildArgs(p.dir, "unconfined")...)
	require.Contains(t, out, "--allow-unsandboxed")
	require.False(t, p.ran(), "the build step ran although the build was refused")
	require.NoFileExists(t, escaped)
	require.False(t, e.hasImage("unconfined"), "a refused build must not leave an image")

	res := e.run(append(buildArgs(p.dir, "unconfined"), "--allow-unsandboxed")...)
	require.Zerof(t, res.code, "build --allow-unsandboxed:\n%s", res.all())
	require.Contains(t, res.stderr, "unconfined")
	require.Empty(t, p.report().Writes[0].Error, "with --allow-unsandboxed the step runs directly on the host")
	require.FileExists(t, escaped)
	require.True(t, e.hasImage("unconfined"))
}

// TestBuildNetworkNeedsTheOperator checks the network of [build] run steps:
// closed by default, refused when only the project asks for it, and open
// with --allow-build-network, the rest of the confinement unchanged.
func TestBuildNetworkNeedsTheOperator(t *testing.T) {
	requireBuildSandbox(t)
	e := newEnv(t)
	netAddr := hostListener(t)
	outside := filepath.Join(e.root, "outside")
	require.NoError(t, os.MkdirAll(outside, 0o755))
	escaped := filepath.Join(outside, "escaped")
	p := newSandboxProject(e, "networked", "network = true\n", sbPlan{TCPAddrs: []string{netAddr, remoteAddr}, WriteFiles: []string{escaped}, Content: "x"})

	out := e.fails(buildArgs(p.dir, "networked")...)
	require.Contains(t, out, "--allow-build-network")
	require.False(t, p.ran(), "the build step ran although the build was refused")
	require.False(t, e.hasImage("networked"))

	res := e.run(append(buildArgs(p.dir, "networked"), "--allow-build-network")...)
	require.Zerof(t, res.code, "build --allow-build-network:\n%s", res.all())
	require.Contains(t, res.stderr, "may reach the network")
	r := p.report()
	requireNetworkOpen(t, r.TCP)
	require.NotEmpty(t, r.Writes[0].Error, "the network was allowed, not writes outside the project")
	require.NoFileExists(t, escaped)
	require.True(t, e.hasImage("networked"))
}

// TestBuildStepCannotRewritePolicy builds a project whose build step
// rewrites the files that decide how the next build is confined and what
// goes into the image: unikernel.toml (to switch the sandbox off) and
// .unignore (which does not exist yet). Whether the sandbox denies the
// writes or the build notices them afterwards, the policy files must be
// what they were, and a step that did change one must fail the build
// without leaving an image.
func TestBuildStepCannotRewritePolicy(t *testing.T) {
	requireBuildSandbox(t)
	e := newEnv(t)
	// Relative to the project, where the step runs.
	p := newSandboxProject(e, "rewriter", "", sbPlan{
		WriteFiles: []string{"unikernel.toml", ".unignore"},
		Content:    "[build]\nlang = \"go\"\nsandbox = false\n",
	})
	unignore := filepath.Join(p.dir, ".unignore")
	before := p.toml()

	res := e.run(buildArgs(p.dir, "rewriter")...)
	r := p.report()
	require.Len(t, r.Writes, 2)
	changed := r.Writes[0].Error == "" || r.Writes[1].Error == ""

	require.Equal(t, before, p.toml(), "a build step changed unikernel.toml")
	require.NoFileExists(t, unignore, "a build step left a .unignore behind")
	if changed {
		require.Equalf(t, 1, res.code, "a build whose step changed a policy file must fail:\n%s", res.all())
		require.Contains(t, res.all(), "policy files")
		require.False(t, e.hasImage("rewriter"), "a build whose step changed a policy file must not produce an image")
	} else {
		require.Zerof(t, res.code, "the sandbox denied the writes, so the build must succeed:\n%s", res.all())
	}

	// The next build is confined as before: the step did not buy itself out.
	res = e.run(buildArgs(p.dir, "rewriter")...)
	require.Contains(t, res.stderr, "build steps run in a sandbox")
	require.NotContains(t, res.stderr, "unconfined")
}
