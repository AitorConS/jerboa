package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/AitorConS/jerboa/internal/api"
	"github.com/AitorConS/jerboa/internal/compose"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- machine-readable output ---

// Scripts parse --output json. When there is nothing to list the output must
// still be one JSON document, and a list when the daemon sent a list.
func TestDaemonFault_JSONOutputForEmptyResults(t *testing.T) {
	// status wraps the VM list in an object of counters.
	d := fdNew(t).on("VM.List", fdOK([]any{}))
	res := fdRun(t, d.endpoint, "--output", "json", "status")
	require.NoError(t, res.err)
	overview, ok := fdRequireJSON(t, res).(map[string]any)
	require.True(t, ok)
	assert.EqualValues(t, 0, overview["total"])

	lists := []fdCommand{
		{"ps", []string{"ps"}},
		{"images", []string{"images"}},
		{"volume ls", []string{"volume", "ls"}},
		{"network ls", []string{"network", "ls"}},
		{"snapshot ls", []string{"snapshot", "ls"}},
		{"dns list", []string{"dns", "list"}},
		{"dns resolve-all", []string{"dns", "resolve-all", "web"}},
	}
	for _, c := range lists {
		t.Run(c.name+"/empty list", func(t *testing.T) {
			d := fdNew(t).all(fdOK([]any{}))
			res := fdRun(t, d.endpoint, append([]string{"--output", "json"}, c.args...)...)
			require.NoError(t, res.err)
			assert.Equal(t, []any{}, fdRequireJSON(t, res))
		})
		t.Run(c.name+"/null", func(t *testing.T) {
			d := fdNew(t).all(fdOK(nil))
			res := fdRun(t, d.endpoint, append([]string{"--output", "json"}, c.args...)...)
			require.NoError(t, res.err)
			fdRequireJSON(t, res)
		})
		t.Run(c.name+"/table", func(t *testing.T) {
			d := fdNew(t).all(fdOK([]any{}))
			res := fdRun(t, d.endpoint, c.args...)
			require.NoError(t, res.err)
			assert.Len(t, strings.Split(strings.TrimSpace(res.stdout), "\n"), 1, "an empty table is its header: %q", res.stdout)
		})
	}

	objects := []fdCommand{
		{"node ls", []string{"node", "ls"}},
		{"system df", []string{"system", "df"}},
		{"system prune", []string{"system", "prune", "--dry-run"}},
		{"system prune yes", []string{"system", "prune", "--yes"}},
		{"stats", []string{"stats", "vm-1"}},
		{"inspect", []string{"inspect", "vm-1"}},
		{"volume inspect", []string{"volume", "inspect", "data"}},
		{"network inspect", []string{"network", "inspect", "front"}},
		{"snapshot inspect", []string{"snapshot", "inspect", "snap"}},
	}
	for _, c := range objects {
		t.Run(c.name, func(t *testing.T) {
			d := fdNew(t).all(fdOK(map[string]any{}))
			res := fdRun(t, d.endpoint, append([]string{"--output", "json"}, c.args...)...)
			require.NoError(t, res.err)
			fdRequireJSON(t, res)
		})
	}
}

// The status overview lists each VM with a detail call. A VM removed between
// the list and the detail call is left out; it does not fail the overview.
func TestDaemonFault_StatusSurvivesAVMVanishing(t *testing.T) {
	d := fdNew(t).
		on("VM.List", fdOK([]api.VMInfo{{ID: "vm-1", State: "running", Name: "web"}, {ID: "vm-2", State: "stopped", Name: "db"}})).
		on("VM.Inspect", fdSeq(fdFail(-32000, "vm vm-1 not found"), fdOK(api.VMDetail{ID: "vm-2", RestartCount: 3})))
	res := fdRun(t, d.endpoint, "status")
	require.NoError(t, res.err)
	assert.Contains(t, res.stdout, "Total:")
	assert.Contains(t, res.stdout, "vm-2")
	assert.NotContains(t, strings.SplitN(res.stdout, "ID", 2)[1], "vm-1")
}

// --- logs ---

// Console output comes from the guest, which the user may not trust: control
// sequences must not reach the terminal unless --raw asks for them.
func TestDaemonFault_LogsStripTerminalControlSequences(t *testing.T) {
	hostile := "booting\x1b]0;owned\x07\x1b[2Jdone\n"
	d := fdNew(t).on("VM.Logs", fdOK(api.LogsResponse{ID: "vm-1", Logs: hostile}))
	res := fdRun(t, d.endpoint, "logs", "vm-1")
	require.NoError(t, res.err)
	assert.NotContains(t, res.stdout, "\x1b")
	assert.NotContains(t, res.stdout, "\x07")
	assert.Contains(t, res.stdout, "booting")
	assert.Contains(t, res.stdout, "done")

	res = fdRun(t, d.endpoint, "logs", "--raw", "vm-1")
	require.NoError(t, res.err)
	assert.Equal(t, hostile, res.stdout)
}

// Following logs polls the daemon. When the daemon dies between two polls the
// command ends with an error, and what was already printed stays printed.
func TestDaemonFault_LogsFollowDaemonDies(t *testing.T) {
	d := fdNew(t).
		on("VM.Logs", fdSeq(fdOK(api.LogsResponse{Logs: "line one\n"}), fdHangup())).
		on("VM.Get", fdOK(api.VMInfo{ID: "vm-1", State: "running"}))
	res := fdRun(t, d.endpoint, "logs", "--follow", "vm-1")
	fdRequireCleanFailure(t, res, "logs")
	assert.Equal(t, "line one\n", res.stdout)
}

// Following ends by itself once the VM has stopped, after printing what the
// console still held, and prints nothing twice when the ring buffer rotates.
func TestDaemonFault_LogsFollowStopsWithTheVM(t *testing.T) {
	d := fdNew(t).
		on("VM.Logs", fdSeq(
			fdOK(api.LogsResponse{Logs: "aaaa\nbbbb\n"}),
			fdOK(api.LogsResponse{Logs: "cc\n"}), // shorter: the buffer rotated
		)).
		on("VM.Get", fdSeq(fdOK(api.VMInfo{State: "running"}), fdOK(api.VMInfo{State: "stopped"})))
	res := fdRun(t, d.endpoint, "logs", "--follow", "vm-1")
	require.NoError(t, res.err)
	assert.Equal(t, "aaaa\nbbbb\ncc\n", res.stdout)
	assert.Equal(t, 2, d.count("VM.Logs"))
}

// --- run ---

// A detached run prints the VM's ID and nothing else on stdout, and sends
// what the flags said.
func TestDaemonFault_RunSendsWhatWasAsked(t *testing.T) {
	d := fdNew(t).
		on("Network.Get", fdOK(api.NetworkInfo{Name: "front", Subnet: "10.9.0.0/24", Gateway: "10.9.0.1", Bridge: "jb0"})).
		on("VM.Run", fdOK(api.VMInfo{ID: "vm-77", State: "running"}))
	res := fdRun(t, d.endpoint, "run", "--name", "web", "--network", "front", "--ip", "10.9.0.5",
		"-p", "8080:80", "-e", "A=1", "--rm", "--cpus", "2", "--memory", "512M", "web:latest")
	require.NoError(t, res.err)
	assert.Equal(t, "vm-77\n", res.stdout)

	var p api.RunParams
	d.params("VM.Run", 0, &p)
	assert.Equal(t, "web:latest", p.Image)
	assert.Equal(t, "web", p.Name)
	assert.Equal(t, "front", p.NetworkName)
	assert.Equal(t, "10.9.0.5", p.IPAddress)
	assert.True(t, p.StaticIP)
	assert.Equal(t, "10.9.0.1", p.GatewayIP)
	assert.Equal(t, "jb0", p.BridgeName)
	assert.True(t, p.AutoRemove)
	assert.False(t, p.Attach)
	assert.Equal(t, 2, p.CPUs)
	assert.Equal(t, "512M", p.Memory)
	assert.Equal(t, []string{"A=1"}, p.Env)
	require.Len(t, p.PortMaps, 1)
	assert.EqualValues(t, 8080, p.PortMaps[0].HostPort)
}

// The network named by --network does not exist: no VM is requested.
func TestDaemonFault_RunUnknownNetwork(t *testing.T) {
	d := fdNew(t).
		on("Network.Get", fdFail(api.CodeNotFound, "network front not found")).
		on("VM.Run", fdOK(api.VMInfo{ID: "vm-1"}))
	res := fdRun(t, d.endpoint, "run", "--network", "front", "web:latest")
	fdRequireCleanFailure(t, res, `network "front" not found`)
	assert.Zero(t, d.count("VM.Run"))
	assert.Empty(t, res.stdout)
}

// With --verify enforce an image whose digest cannot be resolved, or that has
// no signature, is never run.
func TestDaemonFault_RunVerifyEnforceNeverRunsUnverified(t *testing.T) {
	d := fdNew(t).
		on("Image.Get", fdFail(-32000, "image web:latest not found")).
		on("VM.Run", fdOK(api.VMInfo{ID: "vm-1"}))
	res := fdRun(t, d.endpoint, "run", "--verify", "enforce", "web:latest")
	fdRequireCleanFailure(t, res, "not found")
	assert.Zero(t, d.count("VM.Run"))

	d = fdNew(t).
		on("Image.Get", fdOK(api.ImageManifestResult{Name: "web", Tag: "latest", DiskDigest: "sha256:" + strings.Repeat("ab", 32)})).
		on("VM.Run", fdOK(api.VMInfo{ID: "vm-1"}))
	res = fdRun(t, d.endpoint, "run", "--verify", "enforce", "web:latest")
	fdRequireCleanFailure(t, res)
	assert.Zero(t, d.count("VM.Run"), "an unsigned image was run under --verify enforce")
	assert.Empty(t, res.stdout)
}

// Attached runs read the console as a framed stream that ends with a status.
func TestDaemonFault_RunAttachStream(t *testing.T) {
	attach := func(result any, stream []byte) fdHandler {
		return func(int, api.Request) fdReply { return fdReply{result: result, after: stream, hangup: true} }
	}
	framed := map[string]bool{"framed": true}
	status := func(v api.AttachResult) []byte {
		raw, err := json.Marshal(v)
		require.NoError(t, err)
		return raw
	}
	cases := []struct {
		name    string
		handler fdHandler
		wantOut string
		wantErr string
	}{
		{"complete", attach(framed, append(fdFrames("hello ", "world\n"), status(api.AttachResult{Done: true})...)), "hello world\n", ""},
		{"guest failure reported at the end", attach(framed, append(fdFrames("oops\n"), status(api.AttachResult{Done: true, Error: "vm exited with status 3"})...)), "oops\n", "vm exited with status 3"},
		{"cut inside a frame", attach(framed, fdFrames("hello world\n")[:9]), "hello", "read attach output"},
		{"cut between frames", attach(framed, fdFrames("hello\n")[:10]), "hello\n", "read attach output"},
		{"cut before the status", attach(framed, fdFrames("hello\n")), "hello\n", "terminal status"},
		{"status says not done", attach(framed, append(fdFrames("x\n"), status(api.AttachResult{})...)), "x\n", "without terminal status"},
		{"oversized frame header", attach(framed, []byte{0xff, 0xff, 0xff, 0xff, 'x'}), "", "frame too large"},
		{"daemon from before framing", attach(map[string]bool{}, []byte("raw console\n")), "raw console\n", ""},
		{"daemon dies right after accepting", attach(framed, nil), "", "read attach output"},
		{"refused", fdFail(-32000, "vm vm-9 is not running"), "", "vm vm-9 is not running"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := fdNew(t).on("VM.Run", fdOK(api.VMInfo{ID: "vm-9", State: "running"})).on("VM.Attach", c.handler)
			res := fdRun(t, d.endpoint, "run", "--attach", "web:latest")
			fdRequireNoCrashText(t, res)
			assert.Equal(t, c.wantOut, res.stdout, "console output received before the failure must be shown, and nothing else")
			if c.wantErr == "" {
				require.NoError(t, res.err)
				return
			}
			fdRequireCleanFailure(t, res, "run: attach", c.wantErr)
		})
	}
}

// --- system prune ---

func fdPrunePreview() api.SystemPruneResult {
	return api.SystemPruneResult{DryRun: true, Reclaimed: 4096, Removed: []api.SystemPruneItem{{Kind: "image", Name: "old:1", Bytes: 4096}}}
}

// Prune removes nothing unless the user says yes to the list it shows: an
// answer of no, an empty answer and a closed stdin all cancel.
func TestDaemonFault_PruneNeedsAYes(t *testing.T) {
	for name, answer := range map[string]string{"no": "n\n", "empty line": "\n", "closed stdin": "", "something else": "maybe\n"} {
		t.Run(name, func(t *testing.T) {
			d := fdNew(t).on("System.Prune", fdOK(fdPrunePreview()))
			fdIsolate(t)
			res := fdExec(t, answer, "--host", d.endpoint, "system", "prune", "--all")
			fdRequireCleanFailure(t, res, "canceled")
			require.Equal(t, 1, d.count("System.Prune"), "only the preview may be requested")
			var p api.SystemPruneParams
			d.params("System.Prune", 0, &p)
			assert.True(t, p.DryRun)
			assert.True(t, p.All)
			assert.Contains(t, res.stderr, "old:1", "the user must see what would be removed before answering")
		})
	}
	t.Run("yes", func(t *testing.T) {
		done := fdPrunePreview()
		done.DryRun = false
		d := fdNew(t).on("System.Prune", fdSeq(fdOK(fdPrunePreview()), fdOK(done)))
		fdIsolate(t)
		res := fdExec(t, "y\n", "--host", d.endpoint, "system", "prune")
		require.NoError(t, res.err)
		var p api.SystemPruneParams
		d.params("System.Prune", 1, &p)
		assert.False(t, p.DryRun)
		assert.Contains(t, res.stdout, "Reclaimed")
	})
}

// The daemon dies between the preview and the removal, or removes only part:
// the command fails and never reports space as reclaimed that was not.
func TestDaemonFault_PruneFailsHalfway(t *testing.T) {
	t.Run("daemon dies before removing", func(t *testing.T) {
		d := fdNew(t).on("System.Prune", fdSeq(fdOK(fdPrunePreview()), fdHangup()))
		res := fdRun(t, d.endpoint, "system", "prune", "--yes")
		fdRequireCleanFailure(t, res, "system prune")
		assert.NotContains(t, res.stdout, "Reclaimed")
	})
	t.Run("some removals fail", func(t *testing.T) {
		partial := api.SystemPruneResult{Reclaimed: 10, Removed: []api.SystemPruneItem{{Kind: "image", Name: "old:1", Bytes: 10}}, Errors: []string{"volume data: device busy"}}
		d := fdNew(t).on("System.Prune", fdSeq(fdOK(fdPrunePreview()), fdOK(partial)))
		res := fdRun(t, d.endpoint, "system", "prune", "--yes")
		fdRequireCleanFailure(t, res, "1 of the removals failed")
		assert.Contains(t, res.stdout, "could not remove volume data: device busy")
		assert.Contains(t, res.stdout, "old:1")

		d = fdNew(t).on("System.Prune", fdSeq(fdOK(fdPrunePreview()), fdOK(partial)))
		res = fdRun(t, d.endpoint, "--output", "json", "system", "prune", "--yes")
		require.Error(t, res.err, "a partial prune must not exit zero in JSON mode either")
		fdRequireJSON(t, res)
	})
	t.Run("nothing to prune asks nothing", func(t *testing.T) {
		d := fdNew(t).on("System.Prune", fdOK(api.SystemPruneResult{DryRun: true}))
		res := fdRun(t, d.endpoint, "system", "prune")
		require.NoError(t, res.err)
		assert.Contains(t, res.stdout, "Nothing to prune.")
		assert.Equal(t, 1, d.count("System.Prune"))
	})
}

// --- compose ---

const fdComposeFile = `version: "1"
services:
  db:
    image: db:latest
    networks: [back]
    volumes: ["data:/var/lib/db"]
  web:
    image: web:latest
    depends_on: [db]
    networks: [back]
networks:
  back:
    subnet: 10.50.0.0/24
volumes:
  data:
    size: 64M
`

// fdComposeProject writes a compose file and returns its path. Compose
// keeps project state in the user's home, so it also isolates the home once
// for the test, and the test runs commands with fdRunHere to keep it.
func fdComposeProject(t *testing.T, content string) string {
	t.Helper()
	fdIsolate(t)
	path := filepath.Join(t.TempDir(), "compose.yaml")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	return path
}

// fdComposeVM answers VM.Get as the daemon does for the VMs compose up
// started: the VM with ID "vm-<service>" is named after its service.
func fdComposeVM(state string) fdHandler {
	return func(_ int, req api.Request) fdReply {
		var p api.IDParams
		_ = json.Unmarshal(req.Params, &p)
		return fdReply{result: api.VMInfo{ID: p.ID, Name: strings.TrimPrefix(p.ID, "vm-"), State: state}}
	}
}

// fdScoped is the name compose up gives a network or volume of file.
func fdScoped(file, name string) string { return composeScopePrefix(file) + name }

func fdComposeState(t *testing.T, file string) compose.State {
	t.Helper()
	state, err := readState(file)
	require.NoError(t, err, "compose must leave a readable state file behind")
	return state
}

// fdComposeDaemon answers what compose up needs; run decides each VM.Run.
func fdComposeDaemon(t *testing.T, run fdHandler) *fdDaemon {
	t.Helper()
	return fdNew(t).
		on("Volume.Get", fdFail(api.CodeNotFound, "volume not found")).
		on("Volume.Create", fdOK(map[string]any{"id": "v"})).
		on("Network.Get", fdSeq(fdFail(api.CodeNotFound, "network not found"),
			fdOK(api.NetworkInfo{Subnet: "10.50.0.0/24", Gateway: "10.50.0.1", Bridge: "jb1"}))).
		on("Network.Create", fdOK(api.NetworkInfo{Subnet: "10.50.0.0/24"})).
		on("VM.Run", run)
}

// compose up fails at the second service, by refusal or because the daemon
// died. The state file must name everything that was created up to there,
// so that compose down can remove it; nothing may be orphaned.
func TestDaemonFault_ComposeUpFailsHalfway(t *testing.T) {
	failures := map[string]fdHandler{
		"daemon refuses":       fdFail(-32000, "image web:latest not found"),
		"daemon dies":          fdHangup(),
		"daemon dies mid-word": fdTruncated(),
	}
	for name, second := range failures {
		t.Run(name, func(t *testing.T) {
			file := fdComposeProject(t, fdComposeFile)
			d := fdComposeDaemon(t, fdSeq(fdOK(api.VMInfo{ID: "vm-db", State: "running"}), second))
			res := fdRunHere(t, d.endpoint, "compose", "up", file)
			fdRequireCleanFailure(t, res, `service "web"`)

			state := fdComposeState(t, file)
			assert.Equal(t, map[string]string{"db": "vm-db"}, state.Services)
			require.Len(t, state.CreatedVolumes, 1)
			require.Len(t, state.CreatedNetworks, 1)
			assert.True(t, strings.HasSuffix(state.CreatedVolumes[0], "data"))
			assert.NotEqual(t, "data", state.CreatedVolumes[0], "a project volume must be scoped to the project")

			// A second up must not start the stack again on top of the half.
			again := fdComposeDaemon(t, fdOK(api.VMInfo{ID: "vm-x"}))
			res = fdRunHere(t, again.endpoint, "compose", "up", file)
			fdRequireCleanFailure(t, res, "run down first")
			assert.Zero(t, again.count("VM.Run"))

			// down removes what the failed up created and then the state.
			down := fdNew(t).all(fdOK(nil)).on("VM.Get", fdComposeVM("running"))
			res = fdRunHere(t, down.endpoint, "compose", "down", "--volumes", file)
			require.NoError(t, res.err)
			assert.Equal(t, 1, down.count("VM.Stop"))
			assert.Equal(t, 1, down.count("VM.Remove"))
			assert.Equal(t, 1, down.count("Network.Remove"))
			assert.Equal(t, 1, down.count("Volume.Remove"))
			_, err := readState(file)
			require.ErrorIs(t, err, os.ErrNotExist)
		})
	}
}

// A compose file that is wrong is refused before the daemon is asked for
// anything and before any state is written.
func TestDaemonFault_ComposeUpRejectsBadFilesBeforeTheDaemon(t *testing.T) {
	files := map[string]string{
		"not yaml":           "services: [\n",
		"empty":              "",
		"no services":        "version: \"1\"\n",
		"unknown dependency": "version: \"1\"\nservices:\n  web:\n    image: web:latest\n    depends_on: [ghost]\n",
		"dependency cycle":   "version: \"1\"\nservices:\n  a:\n    image: a:1\n    depends_on: [b]\n  b:\n    image: b:1\n    depends_on: [a]\n",
		"undeclared network": "version: \"1\"\nservices:\n  web:\n    image: web:latest\n    networks: [ghost]\n",
		"service is a list":  "version: \"1\"\nservices:\n  - web\n",
		"wrong version":      "version: \"9\"\nservices:\n  web:\n    image: web:latest\n",
	}
	for name, content := range files {
		t.Run(name, func(t *testing.T) {
			file := fdComposeProject(t, content)
			d := fdNew(t).all(fdOK(nil))
			res := fdRunHere(t, d.endpoint, "compose", "up", file)
			// The YAML library reports the offending line on a line of its
			// own, so these errors may span two lines.
			fdRequireNoCrashText(t, res)
			require.ErrorContains(t, res.err, "compose up")
			assert.LessOrEqual(t, strings.Count(res.err.Error(), "\n"), 1)
			assert.Empty(t, d.methods(), "a refused compose file reached the daemon")
			_, err := os.Stat(stateFilePath(file))
			require.ErrorIs(t, err, os.ErrNotExist, "a refused compose file left state behind")
		})
	}
}

// A service whose own settings are wrong is refused with the service named,
// before the daemon is asked for anything: no half-started stack, no state.
func TestDaemonFault_ComposeUpBadServiceSettings(t *testing.T) {
	settings := map[string]string{
		"port":         "    ports: [\"99999:80\"]\n",
		"volume":       "    volumes: [\"nopath\"]\n",
		"health check": "    health_check: \"icmp:1\"\n",
		"restart":      "    restart: \"sometimes\"\n",
	}
	for name, setting := range settings {
		t.Run(name, func(t *testing.T) {
			file := fdComposeProject(t, "version: \"1\"\nservices:\n  web:\n    image: web:latest\n"+setting)
			d := fdNew(t).all(fdOK(nil))
			res := fdRunHere(t, d.endpoint, "compose", "up", file)
			fdRequireCleanFailure(t, res, `service "web"`)
			assert.Empty(t, d.methods())
			_, err := os.Stat(stateFilePath(file))
			require.ErrorIs(t, err, os.ErrNotExist)
		})
	}
}

// compose down with a daemon that cannot stop one service: the others are
// still stopped, the failed one stays in the state for the next attempt, and
// the command fails.
func TestDaemonFault_ComposeDownKeepsWhatItCouldNotStop(t *testing.T) {
	file := fdComposeProject(t, fdComposeFile)
	require.NoError(t, writeState(file, compose.State{
		Services:        map[string]string{"db": "vm-db", "web": "vm-web"},
		CreatedNetworks: []string{fdScoped(file, "back")},
	}))
	d := fdNew(t).all(fdOK(nil)).
		on("VM.Get", fdComposeVM("running")).
		on("VM.Stop", func(_ int, req api.Request) fdReply {
			if strings.Contains(string(req.Params), "vm-db") {
				return fdReply{rpcErr: &api.RPCError{Code: -32000, Message: "vm vm-db: hypervisor does not answer"}}
			}
			return fdReply{}
		})
	res := fdRunHere(t, d.endpoint, "compose", "down", file)
	require.Nil(t, res.panicked)
	require.ErrorContains(t, res.err, "hypervisor does not answer")
	assert.Contains(t, res.stdout, "stopped web")
	assert.NotContains(t, res.stdout, "stopped db")
	assert.Equal(t, map[string]string{"db": "vm-db"}, fdComposeState(t, file).Services)
}

// compose down when the daemon is gone or dies: the state file is what lets
// the user retry, so it must survive, complete.
func TestDaemonFault_ComposeDownKeepsStateWhenTheDaemonIsGone(t *testing.T) {
	stateFor := func(file string) compose.State {
		return compose.State{Project: composeProject(file), Services: map[string]string{"db": "vm-db", "web": "vm-web"}, CreatedVolumes: []string{fdScoped(file, "data")}, CreatedNetworks: []string{fdScoped(file, "back")}}
	}
	t.Run("unreachable", func(t *testing.T) {
		file := fdComposeProject(t, fdComposeFile)
		want := stateFor(file)
		require.NoError(t, writeState(file, want))
		res := fdRunHere(t, "unix://"+filepath.Join(fdShortDir(t), "absent.sock"), "compose", "down", "--volumes", file)
		fdRequireCleanFailure(t, res, "connect to daemon")
		assert.Equal(t, want, fdComposeState(t, file))
	})
	t.Run("dies", func(t *testing.T) {
		file := fdComposeProject(t, fdComposeFile)
		want := stateFor(file)
		require.NoError(t, writeState(file, want))
		d := fdNew(t).all(fdHangup())
		res := fdRunHere(t, d.endpoint, "compose", "down", "--volumes", file)
		require.Nil(t, res.panicked)
		require.Error(t, res.err)
		assert.Equal(t, want, fdComposeState(t, file))
		assert.NotContains(t, res.stdout, "stopped")
	})
	t.Run("vm already gone", func(t *testing.T) {
		file := fdComposeProject(t, fdComposeFile)
		require.NoError(t, writeState(file, stateFor(file)))
		d := fdNew(t).all(fdOK(nil)).on("VM.Get", fdFail(-32000, "vm not found"))
		res := fdRunHere(t, d.endpoint, "compose", "down", file)
		require.NoError(t, res.err)
		assert.Zero(t, d.count("VM.Stop"))
		_, err := readState(file)
		require.ErrorIs(t, err, os.ErrNotExist)
	})
	t.Run("never brought up", func(t *testing.T) {
		// Nothing to do, so no daemon is needed either.
		res := fdRunHere(t, "unix://"+filepath.Join(fdShortDir(t), "absent.sock"), "compose", "down", fdComposeProject(t, fdComposeFile))
		require.NoError(t, res.err)
	})
}

// A state file damaged by hand or by a crash is reported as such by every
// compose command; none treats it as an empty project.
func TestDaemonFault_ComposeDamagedState(t *testing.T) {
	for name, content := range map[string]string{"truncated": `{"services":{"web":"vm-`, "garbage": "\x00\x01not json", "wrong type": `{"services":["web"]}`} {
		for _, args := range [][]string{{"compose", "ps"}, {"compose", "down"}, {"compose", "logs"}} {
			t.Run(name+"/"+args[1], func(t *testing.T) {
				file := fdComposeProject(t, fdComposeFile)
				require.NoError(t, os.MkdirAll(filepath.Dir(stateFilePath(file)), 0o700))
				require.NoError(t, os.WriteFile(stateFilePath(file), []byte(content), 0o600))
				d := fdNew(t).all(fdOK(nil))
				argv := append(append([]string{}, args...), file)
				if args[1] == "logs" {
					argv = append(argv, "web")
				}
				res := fdRunHere(t, d.endpoint, argv...)
				fdRequireCleanFailure(t, res, "parse state")
				assert.Empty(t, d.methods())
				data, err := os.ReadFile(stateFilePath(file))
				require.NoError(t, err)
				assert.Equal(t, content, string(data), "a damaged state file must be left for the user to inspect")
			})
		}
	}
}

// compose ps reports a service whose VM the daemon no longer knows as
// unknown instead of failing the whole listing.
func TestDaemonFault_ComposePsWithLostVMs(t *testing.T) {
	file := fdComposeProject(t, fdComposeFile)
	require.NoError(t, writeState(file, compose.State{Services: map[string]string{"db": "vm-db", "web": "vm-web"}}))
	d := fdNew(t).on("VM.Get", fdSeq(fdFail(-32000, "vm vm-db not found"), fdComposeVM("running")))
	res := fdRunHere(t, d.endpoint, "--output", "json", "compose", "ps", file)
	require.NoError(t, res.err)
	rows, ok := fdRequireJSON(t, res).([]any)
	require.True(t, ok)
	require.Len(t, rows, 2)
	assert.Equal(t, map[string]any{"service": "db", "id": "vm-db", "state": "unknown"}, rows[0])
	assert.Equal(t, map[string]any{"service": "web", "id": "vm-web", "state": "running"}, rows[1])

	res = fdRunHere(t, d.endpoint, "compose", "logs", file, "ghost")
	fdRequireCleanFailure(t, res, `service "ghost" not found`)
}

// --- authentication and endpoint selection ---

const fdToken = "correct-horse-battery-staple-0123456789"

// A CLI holding the daemon's token authenticates without ever putting the
// token on the wire, and its command then runs.
func TestDaemonFault_AuthTokenNeverCrossesTheWire(t *testing.T) {
	d := fdNew(t).on("VM.List", fdOK([]api.VMInfo{{ID: "vm-1", State: "running"}}))
	d.secure(fdToken, "")
	fdIsolate(t)
	t.Setenv("JERBOA_AUTH_TOKEN", fdToken)
	res := fdExec(t, "", "--host", d.endpoint, "ps")
	require.NoError(t, res.err)
	assert.Contains(t, res.stdout, "vm-1")
	assert.NotContains(t, d.wire(), fdToken, "the token itself was sent to the daemon")
	assert.Contains(t, d.wire(), "Auth.Challenge")
}

// Every way the handshake can fail ends in one line that says what to do,
// and the command's request is never sent to a peer that did not prove
// itself.
func TestDaemonFault_AuthHandshakeFailures(t *testing.T) {
	cases := []struct {
		name        string
		daemonToken string
		mode        string
		want        string
	}{
		{"wrong token", "another-token-entirely-0123456789abcdef", "", "authentication failed"},
		{"daemon too old for challenges", fdToken, "old", "update jerboad"},
		{"daemon does not prove itself", fdToken, "no-proof", "did not prove it holds the token"},
		{"impostor daemon", fdToken, "bad-proof", "may not be jerboad"},
		{"challenge too short", fdToken, "short-nonce", "invalid challenge"},
		{"protocol mismatch", fdToken, "proto", "protocol version mismatch"},
		{"daemon busy", fdToken, "busy", "daemon busy"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := fdNew(t).all(fdOK([]any{}))
			d.secure(c.daemonToken, c.mode)
			fdIsolate(t)
			t.Setenv("JERBOA_AUTH_TOKEN", fdToken)
			res := fdExec(t, "", "--host", d.endpoint, "ps")
			fdRequireCleanFailure(t, res, c.want)
			assert.Empty(t, d.methods(), "a request was sent over a connection that failed authentication")
			assert.NotContains(t, d.wire(), fdToken)
			assert.NotContains(t, res.err.Error(), fdToken, "the token must not be printed")
		})
	}
}

// The token and the endpoint are found where the user configured them, in
// the documented order, and a damaged config file does not stop the CLI from
// using what the flags and the environment say.
func TestDaemonFault_EndpointAndTokenResolution(t *testing.T) {
	writeConfig := func(t *testing.T, home, content string) {
		t.Helper()
		require.NoError(t, os.MkdirAll(filepath.Join(home, ".jerboa"), 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(home, ".jerboa", "config.toml"), []byte(content), 0o600))
	}
	t.Run("config file", func(t *testing.T) {
		d := fdNew(t).on("VM.List", fdOK([]any{}))
		d.secure(fdToken, "")
		home := fdIsolate(t)
		writeConfig(t, home, "[daemon]\nendpoint = \""+d.endpoint+"\"\ntoken = \""+fdToken+"\"\n")
		res := fdExec(t, "", "ps")
		require.NoError(t, res.err)
		assert.Equal(t, 1, d.count("VM.List"))
	})
	t.Run("environment beats config file", func(t *testing.T) {
		d := fdNew(t).on("VM.List", fdOK([]any{}))
		home := fdIsolate(t)
		writeConfig(t, home, "[daemon]\nendpoint = \"unix:///nonexistent/from-config.sock\"\n")
		t.Setenv("JERBOA_HOST", d.endpoint)
		res := fdExec(t, "", "ps")
		require.NoError(t, res.err)
	})
	t.Run("flag beats environment", func(t *testing.T) {
		d := fdNew(t).on("VM.List", fdOK([]any{}))
		fdIsolate(t)
		t.Setenv("JERBOA_HOST", "unix:///nonexistent/from-env.sock")
		res := fdExec(t, "", "--host", d.endpoint, "ps")
		require.NoError(t, res.err)
	})
	t.Run("damaged config file", func(t *testing.T) {
		d := fdNew(t).on("VM.List", fdOK([]any{}))
		for name, content := range map[string]string{"not toml": "[daemon\nendpoint = ", "binary": "\x00\xff\x00", "wrong types": "[daemon]\nendpoint = 7\ntoken = [1]\n"} {
			t.Run(name, func(t *testing.T) {
				home := fdIsolate(t)
				writeConfig(t, home, content)
				res := fdExec(t, "", "--host", d.endpoint, "ps")
				require.NoError(t, res.err, "a damaged config file must not disable an explicit --host")
				fdRequireNoCrashText(t, res)
			})
		}
	})
	t.Run("default socket and rendezvous token", func(t *testing.T) {
		if runtime.GOOS != "darwin" {
			t.Skip("only macOS keeps the daemon socket and its token under the home directory")
		}
		home := fdShortDir(t)
		fdIsolate(t)
		t.Setenv("HOME", home)
		require.NoError(t, os.MkdirAll(filepath.Join(home, ".jerboa"), 0o700))
		d := fdNewAt(t, filepath.Join(home, ".jerboa", "jerboad.sock")).on("VM.List", fdOK([]any{}))
		d.secure(fdToken, "")

		// Without the rendezvous file the CLI has no token.
		res := fdExec(t, "", "ps")
		fdRequireCleanFailure(t, res, "authentication required")

		for name, content := range map[string]string{"truncated": `{"token":"` + fdToken, "empty": "", "not an object": `["x"]`} {
			require.NoError(t, os.WriteFile(filepath.Join(home, ".jerboa", "daemon.json"), []byte(content), 0o600))
			res = fdExec(t, "", "ps")
			fdRequireCleanFailure(t, res, "authentication required")
			assert.Zero(t, d.count("VM.List"), "damaged rendezvous file (%s)", name)
		}

		raw, err := json.Marshal(map[string]string{"token": fdToken, "endpoint": d.endpoint})
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(home, ".jerboa", "daemon.json"), raw, 0o600))
		res = fdExec(t, "", "ps")
		require.NoError(t, res.err)
		assert.Equal(t, 1, d.count("VM.List"))
	})
}

// --- doctor, sign, images ---

// The doctor is what a user runs when things are broken, so a broken daemon
// must become a finding in a complete report, in both formats.
func TestDaemonFault_DoctorReportsABrokenDaemon(t *testing.T) {
	cases := map[string]struct {
		setup func(d *fdDaemon)
		want  string
		fails bool
	}{
		"dies":          {func(d *fdDaemon) { d.all(fdHangup()) }, "does not answer", true},
		"not jerboad":   {func(d *fdDaemon) { d.all(fdRaw("HTTP/1.1 400 Bad Request\r\n\r\n")) }, "does not answer", true},
		"needs a token": {func(d *fdDaemon) { d.all(fdFail(-32001, "authentication required")) }, "authentication required", true},
		"older daemon":  {func(d *fdDaemon) { d.on("Daemon.Version", fdOK(map[string]string{"version": "dev"})) }, "older than this CLI", false},
		"check fails": {func(d *fdDaemon) {
			d.on("Daemon.Version", fdOK(map[string]string{"version": "dev"})).on("System.Check", fdHangup())
		}, "could not check its state", true},
		"damaged finding": {func(d *fdDaemon) {
			d.on("Daemon.Version", fdOK(map[string]string{"version": "dev"})).on("System.Check", fdOK(map[string]any{"findings": []map[string]string{{"check": "images", "severity": "error", "message": "manifest of web:latest is damaged"}}}))
		}, "manifest of web:latest is damaged", true},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			d := fdNew(t)
			c.setup(d)
			res := fdRun(t, d.endpoint, "doctor")
			fdRequireNoCrashText(t, res)
			assert.Contains(t, res.stdout, c.want)
			assert.Contains(t, res.stdout, "cli", "the client-side checks must still be reported")
			if c.fails {
				fdRequireCleanFailure(t, res, "problem(s) found")
			} else {
				require.NoError(t, res.err)
			}

			d = fdNew(t)
			c.setup(d)
			res = fdRun(t, d.endpoint, "--output", "json", "doctor")
			report, ok := fdRequireJSON(t, res).(map[string]any)
			require.True(t, ok)
			assert.Equal(t, !c.fails, report["ok"])
		})
	}
}

// Signing must sign the digest the daemon reports, and must write no
// signature when the daemon could not name one.
func TestDaemonFault_SignAndVerify(t *testing.T) {
	digest := "sha256:" + strings.Repeat("cd", 32)
	t.Run("daemon cannot resolve the image", func(t *testing.T) {
		d := fdNew(t).on("Image.Get", fdFail(-32000, "image web:latest not found"))
		fdIsolate(t)
		home := os.Getenv("HOME")
		res := fdExec(t, "", "--host", d.endpoint, "sign", "web:latest")
		fdRequireCleanFailure(t, res, "sign", "not found")
		entries, err := os.ReadDir(filepath.Join(home, ".jerboa"))
		if err == nil {
			assert.Empty(t, entries, "a failed sign created signing material")
		}
	})
	t.Run("daemon reports no digest", func(t *testing.T) {
		d := fdNew(t).on("Image.Get", fdOK(api.ImageManifestResult{Name: "web", Tag: "latest"}))
		res := fdRun(t, d.endpoint, "sign", "web:latest")
		fdRequireCleanFailure(t, res, "sign")
		assert.NotContains(t, res.stdout, "signed")
	})
	t.Run("sign then verify, then the image changes", func(t *testing.T) {
		d := fdNew(t).on("Image.Get", fdOK(api.ImageManifestResult{Name: "web", Tag: "latest", DiskDigest: digest}))
		fdIsolate(t)
		res := fdExec(t, "", "--host", d.endpoint, "sign", "web:latest")
		require.NoError(t, res.err)
		res = fdExec(t, "", "--host", d.endpoint, "verify", "web:latest")
		require.NoError(t, res.err)
		assert.Contains(t, res.stdout, "verified web:latest")

		d.on("Image.Get", fdOK(api.ImageManifestResult{Name: "web", Tag: "latest", DiskDigest: "sha256:" + strings.Repeat("ef", 32)}))
		res = fdExec(t, "", "--host", d.endpoint, "verify", "web:latest")
		fdRequireCleanFailure(t, res, "verify")
		assert.NotContains(t, res.stdout, "verified")
	})
}

// Recipes and SBOMs are stored documents the CLI prints as they are. One the
// daemon could not produce must be an error, not an empty document.
func TestDaemonFault_ImageDocuments(t *testing.T) {
	for _, sub := range []string{"recipe", "sbom"} {
		t.Run(sub+"/missing", func(t *testing.T) {
			d := fdNew(t).all(func(int, api.Request) fdReply {
				return fdReply{raw: []byte(`{"jsonrpc":"2.0","id":1}` + "\n")}
			})
			res := fdRun(t, d.endpoint, "images", sub, "web:latest")
			fdRequireCleanFailure(t, res, "images "+sub)
			assert.Empty(t, res.stdout)
		})
		t.Run(sub+"/present", func(t *testing.T) {
			d := fdNew(t).all(fdOK(map[string]any{"bomFormat": "CycloneDX", "components": []any{}}))
			res := fdRun(t, d.endpoint, "images", sub, "web:latest")
			require.NoError(t, res.err)
			assert.Equal(t, map[string]any{"bomFormat": "CycloneDX", "components": []any{}}, fdRequireJSON(t, res))
		})
	}
}

// --- volumes ---

// Seeding uploads a stream on a second connection. The daemon refusing, or
// dying before the upload, must fail the command and report nothing seeded.
func TestDaemonFault_VolumeSeedVolumeMissing(t *testing.T) {
	d := fdNew(t).on("Volume.Get", fdFail(api.CodeNotFound, "volume data not found"))
	res := fdRun(t, d.endpoint, "volume", "seed", "data", "--pkg", "eyberg/none:1")
	fdRequireCleanFailure(t, res, `volume "data" not found`, "jerboa volume create data")
	assert.Zero(t, d.count("Volume.Seed"))
	assert.NotContains(t, res.stdout, "seeded")
}

// volume create --seed-pkg creates and then seeds. With an unknown package
// source nothing must be sent after the create, and the error must say that
// the source is the problem.
func TestDaemonFault_VolumeCreateWithBadSeedSource(t *testing.T) {
	d := fdNew(t).all(fdOK(map[string]any{"id": "data"}))
	res := fdRun(t, d.endpoint, "volume", "create", "data", "--seed-pkg", "x/y:1", "--pkg-source", "nowhere")
	fdRequireCleanFailure(t, res, "nowhere")
	assert.Zero(t, d.count("Volume.Seed"))
}

// Volumes listed by the daemon render, including ones with a zero time and a
// size no real volume has.
func TestDaemonFault_VolumeListOddValues(t *testing.T) {
	d := fdNew(t).on("Volume.List", fdOK([]map[string]any{
		{"id": "data", "size_bytes": 1 << 30},
		{"id": "neg", "size_bytes": -5},
		{},
	}))
	res := fdRun(t, d.endpoint, "volume", "ls")
	require.NoError(t, res.err)
	fdRequireNoCrashText(t, res)
	assert.Len(t, strings.Split(strings.TrimSpace(res.stdout), "\n"), 4)

	d.on("Volume.List", fdOK([]any{nil, map[string]any{"id": "data"}}))
	res = fdRun(t, d.endpoint, "volume", "ls")
	require.Nil(t, res.panicked, "a null entry in the daemon's list crashed the listing")
}
