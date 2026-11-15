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

// writeLegacyState puts a state file where earlier versions kept it, next
// to the compose file, as a project author can.
func writeLegacyState(t *testing.T, file string, state compose.State, mode os.FileMode) string {
	t.Helper()
	data, err := json.Marshal(state)
	require.NoError(t, err)
	path := legacyStateFilePath(file)
	require.NoError(t, os.WriteFile(path, data, 0o600))
	require.NoError(t, os.Chmod(path, mode))
	return path
}

// fdVictimVM answers VM.Get for a VM of someone else: whatever the
// reference, the daemon resolves it to this VM.
func fdVictimVM(id, name string) fdHandler {
	return fdOK(api.VMInfo{ID: id, Name: name, State: "running"})
}

// A state file shipped with a project (or left by another user) must not
// make compose act on resources compose up did not create for the project:
// down, ps and logs refuse it before stopping, removing or reading
// anything, and leave the file for the user to inspect.
func TestComposeForeignStateIsRefused(t *testing.T) {
	type foreign struct {
		state func(file string) compose.State
		mode  os.FileMode
		vmGet fdHandler
	}
	valid := func(file string) compose.State {
		return compose.State{Project: composeProject(file), Services: map[string]string{"web": "vm-web"}}
	}
	cases := map[string]foreign{
		"another project": {state: func(string) compose.State {
			return compose.State{Project: "/elsewhere/.jerboa-compose-0.json", Services: map[string]string{"web": "vm-web"}}
		}},
		"unscoped volume": {state: func(file string) compose.State {
			s := valid(file)
			s.CreatedVolumes = []string{"victim-data"}
			return s
		}},
		"other project's network": {state: func(file string) compose.State {
			s := valid(file)
			s.CreatedNetworks = []string{composeScopePrefix(file+".other") + "back"}
			return s
		}},
		"vm by name": {state: func(file string) compose.State {
			s := valid(file)
			s.Services = map[string]string{"web": "victim"}
			return s
		}, vmGet: fdVictimVM("0123456789ab", "victim")},
		"vm by id prefix": {state: func(file string) compose.State {
			s := valid(file)
			s.Services = map[string]string{"web": "0123"}
			return s
		}, vmGet: fdVictimVM("0123456789ab", "web")},
		"vm of another name": {state: func(file string) compose.State {
			s := valid(file)
			s.Services = map[string]string{"web": "0123456789ab"}
			return s
		}, vmGet: fdVictimVM("0123456789ab", "victim")},
		"service not in the file": {state: func(file string) compose.State {
			s := valid(file)
			s.Services = map[string]string{"victim": "0123456789ab"}
			return s
		}, vmGet: fdVictimVM("0123456789ab", "victim")},
	}
	if runtime.GOOS != "windows" {
		cases["group writable"] = foreign{state: valid, mode: 0o664, vmGet: fdComposeVM("running")}
	}
	for name, c := range cases {
		for _, args := range [][]string{{"compose", "down", "--volumes"}, {"compose", "ps"}, {"compose", "logs"}} {
			t.Run(name+"/"+args[1], func(t *testing.T) {
				file := fdComposeProject(t, fdComposeFile)
				mode := c.mode
				if mode == 0 {
					mode = 0o600
				}
				path := writeLegacyState(t, file, c.state(file), mode)
				before, err := os.ReadFile(path)
				require.NoError(t, err)
				vmGet := c.vmGet
				if vmGet == nil {
					vmGet = fdComposeVM("running")
				}
				d := fdNew(t).all(fdOK(nil)).on("VM.Get", vmGet)
				argv := append(append([]string{}, args...), file)
				if args[1] == "logs" {
					argv = append(argv, "web")
				}
				res := fdRunHere(t, d.endpoint, argv...)
				if name == "service not in the file" && args[1] == "logs" {
					// The service asked for is not in the state at all.
					require.Error(t, res.err)
				} else {
					fdRequireCleanFailure(t, res, "not written by compose up for this project")
				}
				for _, m := range []string{"VM.Stop", "VM.Remove", "VM.Logs", "Volume.Remove", "Network.Remove"} {
					assert.Zero(t, d.count(m), "%s was sent on a foreign state", m)
				}
				after, err := os.ReadFile(path)
				require.NoError(t, err)
				assert.Equal(t, before, after)
				_, err = os.Stat(stateFilePath(file))
				require.ErrorIs(t, err, os.ErrNotExist, "a foreign state must not be taken over")
			})
		}
	}
}

// compose up reports a foreign state file as such instead of sending the
// user to compose down, which would act on it.
func TestComposeUpReportsForeignState(t *testing.T) {
	file := fdComposeProject(t, fdComposeFile)
	writeLegacyState(t, file, compose.State{Project: composeProject(file), Services: map[string]string{"web": "victim"}}, 0o600)
	d := fdComposeDaemon(t, fdOK(api.VMInfo{ID: "vm-x"})).on("VM.Get", fdVictimVM("0123456789ab", "victim"))
	res := fdRunHere(t, d.endpoint, "compose", "up", file)
	fdRequireCleanFailure(t, res, "not written by compose up for this project", legacyStateFilePath(file))
	assert.NotContains(t, res.err.Error(), "run down first")
	assert.Zero(t, d.count("VM.Run"))
	assert.Zero(t, d.count("Volume.Create"))
}

// A state file an earlier version left next to the compose file is taken
// over once it checks out: compose moves it to the user's state directory.
func TestComposeLegacyStateIsTakenOver(t *testing.T) {
	file := fdComposeProject(t, fdComposeFile)
	want := compose.State{
		Project:         composeProject(file),
		Services:        map[string]string{"db": "vm-db", "web": "vm-web"},
		CreatedVolumes:  []string{fdScoped(file, "data")},
		CreatedNetworks: []string{fdScoped(file, "back")},
	}
	legacy := writeLegacyState(t, file, want, 0o600)
	d := fdNew(t).all(fdOK(nil)).on("VM.Get", fdComposeVM("running"))
	res := fdRunHere(t, d.endpoint, "compose", "ps", file)
	require.NoError(t, res.err)
	assert.Contains(t, res.stdout, "vm-web")
	_, err := os.Stat(legacy)
	require.ErrorIs(t, err, os.ErrNotExist)
	assert.Equal(t, want, fdComposeState(t, file))

	res = fdRunHere(t, d.endpoint, "compose", "down", "--volumes", file)
	require.NoError(t, res.err)
	assert.Equal(t, 2, d.count("VM.Stop"))
	assert.Equal(t, 1, d.count("Volume.Remove"))
	assert.Equal(t, 1, d.count("Network.Remove"))
	_, err = readState(file)
	require.ErrorIs(t, err, os.ErrNotExist)
}

// compose up keeps its state in the user's private state directory, not in
// the project directory.
func TestComposeStateLivesInTheUserHome(t *testing.T) {
	file := fdComposeProject(t, fdComposeFile)
	d := fdComposeDaemon(t, fdComposeVMRun())
	res := fdRunHere(t, d.endpoint, "compose", "up", file)
	require.NoError(t, res.err)
	home, err := os.UserHomeDir()
	require.NoError(t, err)
	path := stateFilePath(file)
	assert.True(t, strings.HasPrefix(path, filepath.Join(home, ".jerboa", "compose")+string(filepath.Separator)))
	fi, err := os.Stat(path)
	require.NoError(t, err)
	_, err = os.Stat(legacyStateFilePath(file))
	require.ErrorIs(t, err, os.ErrNotExist)
	if runtime.GOOS != "windows" {
		assert.Equal(t, os.FileMode(0o600), fi.Mode().Perm())
		dir, err := os.Stat(filepath.Dir(path))
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o700), dir.Mode().Perm())
	}
	state := fdComposeState(t, file)
	assert.Equal(t, map[string]string{"db": "vm-db", "web": "vm-web"}, state.Services)
}

// fdComposeVMRun answers VM.Run as the daemon does, with an ID made from
// the requested name.
func fdComposeVMRun() fdHandler {
	return func(_ int, req api.Request) fdReply {
		var p api.RunParams
		_ = json.Unmarshal(req.Params, &p)
		return fdReply{result: api.VMInfo{ID: "vm-" + p.Name, Name: p.Name, State: "running"}}
	}
}

// Text from the daemon, a registry or a project reaches the terminal
// without control sequences.
func TestComposeOutputIsSanitized(t *testing.T) {
	file := fdComposeProject(t, fdComposeFile)
	require.NoError(t, writeState(file, compose.State{Services: map[string]string{"web": "vm-web"}}))
	d := fdNew(t).
		on("VM.Get", fdOK(api.VMInfo{ID: "vm-web", Name: "web", State: "run\x1b]0;pwned\x07ning"})).
		on("VM.Logs", fdOK(api.LogsResponse{Logs: "hello \x1b[2J\x1b]52;c;ZXZpbA==\x07world\n"}))
	res := fdRunHere(t, d.endpoint, "compose", "logs", file, "web")
	require.NoError(t, res.err)
	assert.Equal(t, "hello world\n", res.stdout)

	res = fdRunHere(t, d.endpoint, "compose", "logs", "--raw", file, "web")
	require.NoError(t, res.err)
	assert.Contains(t, res.stdout, "\x1b[2J")

	res = fdRunHere(t, d.endpoint, "compose", "ps", file)
	require.NoError(t, res.err)
	assert.NotContains(t, res.stdout, "\x1b")
	assert.Contains(t, res.stdout, "running")
}
