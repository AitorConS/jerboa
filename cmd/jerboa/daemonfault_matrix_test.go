package main

import (
	"net"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AitorConS/jerboa/internal/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests hold every daemon-backed command to one rule: whatever the
// daemon does (it is not there, it dies while answering, it refuses, it is an
// older build, it is not jerboad at all) the user gets an error that fits one
// line and names the failure. Never a panic, never a hang, never a result
// printed as if the command had worked.

// fdCommand is one way to invoke the CLI that needs the daemon and nothing
// else: no file on disk, no registry.
type fdCommand struct {
	name string
	args []string
}

// fdCommands lists every command whose first act is a daemon call.
func fdCommands() []fdCommand {
	return []fdCommand{
		{"ps", []string{"ps"}},
		{"ps json", []string{"--output", "json", "ps"}},
		{"status", []string{"status"}},
		{"status json", []string{"--output", "json", "status"}},
		{"logs", []string{"logs", "vm-1"}},
		{"logs follow", []string{"logs", "--follow", "vm-1"}},
		{"stop", []string{"stop", "vm-1"}},
		{"stop force", []string{"stop", "--force", "vm-1"}},
		{"rm", []string{"rm", "vm-1"}},
		{"inspect", []string{"inspect", "vm-1"}},
		{"exec", []string{"exec", "--signal", "SIGHUP", "vm-1"}},
		{"stats", []string{"stats", "vm-1"}},
		{"stats watch", []string{"stats", "--watch", "vm-1"}},
		{"run", []string{"run", "web:latest"}},
		{"run attach", []string{"run", "--attach", "web:latest"}},
		{"run network", []string{"run", "--network", "front", "web:latest"}},
		{"run verify", []string{"run", "--verify", "enforce", "web:latest"}},
		{"images", []string{"images"}},
		{"images json", []string{"--output", "json", "images"}},
		{"images inspect", []string{"images", "inspect", "web:latest"}},
		{"images recipe", []string{"images", "recipe", "web:latest"}},
		{"images sbom", []string{"images", "sbom", "web:latest"}},
		{"rmi", []string{"rmi", "web:latest"}},
		{"tag", []string{"tag", "web:latest", "web:v2"}},
		{"sign", []string{"sign", "web:latest"}},
		{"verify", []string{"verify", "web:latest"}},
		{"network create", []string{"network", "create", "front"}},
		{"network ls", []string{"network", "ls"}},
		{"network inspect", []string{"network", "inspect", "front"}},
		{"network rm", []string{"network", "rm", "front"}},
		{"snapshot create", []string{"snapshot", "create", "vm-1", "snap"}},
		{"snapshot restore", []string{"snapshot", "restore", "vm-1", "snap"}},
		{"snapshot ls", []string{"snapshot", "ls"}},
		{"snapshot inspect", []string{"snapshot", "inspect", "snap"}},
		{"snapshot rm", []string{"snapshot", "rm", "snap"}},
		{"dns resolve", []string{"dns", "resolve", "web"}},
		{"dns list", []string{"dns", "list"}},
		{"dns resolve-all", []string{"dns", "resolve-all", "web"}},
		{"node ls", []string{"node", "ls"}},
		{"system df", []string{"system", "df"}},
		{"system prune", []string{"system", "prune", "--yes"}},
		{"system prune dry-run", []string{"system", "prune", "--dry-run"}},
		{"volume create", []string{"volume", "create", "data", "--size", "64M"}},
		{"volume ls", []string{"volume", "ls"}},
		{"volume rm", []string{"volume", "rm", "data"}},
		{"volume inspect", []string{"volume", "inspect", "data"}},
		{"volume seed", []string{"volume", "seed", "data", "--pkg", "eyberg/none:1"}},
	}
}

// No daemon is listening: the socket does not exist. This is the first thing
// a new user meets, so the message must say that the daemon could not be
// reached, and where it was looked for.
func TestDaemonFault_Unreachable(t *testing.T) {
	sock := filepath.Join(fdShortDir(t), "absent.sock")
	cmds := append(fdCommands(), fdCommand{"pull", []string{"pull", "acme/web:v1"}})
	for _, c := range cmds {
		t.Run(c.name, func(t *testing.T) {
			res := fdRun(t, "unix://"+sock, c.args...)
			fdRequireCleanFailure(t, res)
			assert.Contains(t, res.err.Error(), "absent.sock", "the error must say which endpoint was tried")
			assert.Contains(t, res.err.Error(), "dial")
			assert.Empty(t, res.stdout, "a command that reached no daemon has nothing to print")
		})
	}
}

// The socket file is there but nothing accepts on it, which is what a daemon
// killed with SIGKILL leaves behind.
func TestDaemonFault_StaleSocket(t *testing.T) {
	d := fdNew(t)
	// Closing the listener would remove the socket file; keep the file and
	// stop accepting instead.
	ln, ok := d.ln.(*net.UnixListener)
	require.True(t, ok)
	ln.SetUnlinkOnClose(false)
	d.close()
	for _, c := range fdCommands() {
		t.Run(c.name, func(t *testing.T) {
			res := fdRun(t, d.endpoint, c.args...)
			fdRequireCleanFailure(t, res, "dial")
			assert.Empty(t, res.stdout)
		})
	}
}

// The endpoint is a TCP port nothing listens on.
func TestDaemonFault_TCPRefused(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0") //nolint:noctx // reserving a free port
	require.NoError(t, err)
	addr := ln.Addr().String()
	require.NoError(t, ln.Close())
	for _, c := range []fdCommand{{"ps", []string{"ps"}}, {"images", []string{"images"}}, {"volume ls", []string{"volume", "ls"}}} {
		t.Run(c.name, func(t *testing.T) {
			res := fdRun(t, "tcp://"+addr, c.args...)
			fdRequireCleanFailure(t, res, addr)
		})
	}
}

// An endpoint that cannot be an endpoint is refused before anything is
// dialed, with the value the user typed in the message.
func TestDaemonFault_MalformedEndpoint(t *testing.T) {
	for _, endpoint := range []string{
		"http://127.0.0.1:7890",
		"tcp://",
		"tcp://127.0.0.1",
		"tcp://127.0.0.1:port",
		"unix://",
		"://",
		"tcp://[::1",
	} {
		t.Run(endpoint, func(t *testing.T) {
			res := fdRun(t, endpoint, "ps")
			fdRequireCleanFailure(t, res)
		})
	}
}

// The daemon reads the request and dies before answering.
func TestDaemonFault_HangsUpBeforeAnswering(t *testing.T) {
	for _, c := range fdCommands() {
		t.Run(c.name, func(t *testing.T) {
			d := fdNew(t).all(fdHangup())
			res := fdRun(t, d.endpoint, c.args...)
			fdRequireCleanFailure(t, res)
			assert.Len(t, d.methods(), 1, "the command must stop at the first call that failed: %s", fdString(d.methods()))
			assert.Empty(t, res.stdout, "nothing may be printed as a result when the daemon never answered")
		})
	}
}

// The daemon dies while writing its answer: the client reads half a response.
func TestDaemonFault_ClosesMidResponse(t *testing.T) {
	for _, c := range fdCommands() {
		t.Run(c.name, func(t *testing.T) {
			d := fdNew(t).all(fdTruncated())
			res := fdRun(t, d.endpoint, c.args...)
			fdRequireCleanFailure(t, res)
			assert.Len(t, d.methods(), 1, fdString(d.methods()))
			assert.Empty(t, res.stdout, "half a response must not be rendered as a result")
		})
	}
}

// Whatever listens on the endpoint is not jerboad: it answers in another
// protocol altogether.
func TestDaemonFault_NotJerboad(t *testing.T) {
	answers := map[string]string{
		"http":       "HTTP/1.1 400 Bad Request\r\nContent-Type: text/plain\r\n\r\nBad Request\n",
		"binary":     "\x00\x01\x02\xff\xfe\x00SSH-2.0-OpenSSH_9.6\r\n",
		"json array": "[1,2,3]\n",
		"empty line": "\n",
	}
	for name, answer := range answers {
		for _, c := range fdCommands() {
			t.Run(name+"/"+c.name, func(t *testing.T) {
				d := fdNew(t).all(fdRaw(answer))
				res := fdRun(t, d.endpoint, c.args...)
				fdRequireCleanFailure(t, res)
				assert.Empty(t, res.stdout)
			})
		}
	}
}

// The daemon refuses the request: the user must read the daemon's reason,
// without the JSON-RPC code, which means nothing to them.
func TestDaemonFault_RPCError(t *testing.T) {
	const reason = "disk quota exceeded while writing state"
	for _, c := range fdCommands() {
		t.Run(c.name, func(t *testing.T) {
			d := fdNew(t).all(fdFail(-32000, reason))
			res := fdRun(t, d.endpoint, c.args...)
			fdRequireCleanFailure(t, res, reason)
			assert.NotContains(t, res.err.Error(), "-32000")
			assert.Len(t, d.methods(), 1, fdString(d.methods()))
			assert.Empty(t, res.stdout)
		})
	}
}

// A hostile or broken daemon puts terminal control sequences and line breaks
// in its error. They are the daemon's words, but the command must still fail
// rather than crash on them.
func TestDaemonFault_RPCErrorWithControlCharacters(t *testing.T) {
	d := fdNew(t).all(fdFail(-32000, "bad \x1b[2J\x1b]0;owned\x07 name"))
	for _, c := range fdCommands() {
		t.Run(c.name, func(t *testing.T) {
			res := fdRun(t, d.endpoint, c.args...)
			require.Nil(t, res.panicked)
			require.Error(t, res.err)
			assert.Empty(t, res.stdout)
		})
	}
}

// The running daemon is older than the CLI and does not know the method. The
// message must not read as a bug in the command the user typed.
func TestDaemonFault_OlderDaemon(t *testing.T) {
	for _, c := range fdCommands() {
		t.Run(c.name, func(t *testing.T) {
			d := fdNew(t)
			res := fdRun(t, d.endpoint, c.args...)
			fdRequireCleanFailure(t, res)
			msg := res.err.Error()
			assert.True(t, strings.Contains(msg, "method not found") || strings.Contains(msg, "older than this CLI"),
				"the error must say the daemon does not know the call: %s", msg)
			assert.Empty(t, res.stdout)
		})
	}
}

// The daemon requires a token and the CLI has none: every call is answered
// with "authentication required".
func TestDaemonFault_AuthenticationRequired(t *testing.T) {
	for _, c := range fdCommands() {
		t.Run(c.name, func(t *testing.T) {
			d := fdNew(t).all(fdFail(-32001, "authentication required"))
			res := fdRun(t, d.endpoint, c.args...)
			fdRequireCleanFailure(t, res, "authentication required")
			assert.Len(t, d.methods(), 1, fdString(d.methods()))
			assert.Empty(t, res.stdout)
		})
	}
}

// The daemon answers with a result of the wrong shape (a number where an
// object or a list is expected), as a daemon of another version might. A
// command may accept it only if it ignores the result; none may crash or
// print a made-up result.
func TestDaemonFault_ResultOfTheWrongShape(t *testing.T) {
	shapes := map[string]any{
		"number": 42,
		"string": "ok",
		"bool":   true,
		"list of numbers": []int{
			1, 2, 3,
		},
		"object of the wrong fields": map[string]any{"id": []int{1}, "state": 7, "nodes": "x", "removed": 3},
	}
	for name, shape := range shapes {
		for _, c := range fdCommands() {
			t.Run(name+"/"+c.name, func(t *testing.T) {
				d := fdNew(t).all(fdOK(shape))
				res := fdRun(t, d.endpoint, c.args...)
				fdRequireNoCrashText(t, res)
				if res.err != nil {
					assert.NotContains(t, res.err.Error(), "\n")
				}
			})
		}
	}
}

// The daemon answers every call with a null result. Commands that list
// things must treat it as nothing to list.
func TestDaemonFault_NullResult(t *testing.T) {
	for _, c := range fdCommands() {
		if c.name == "logs follow" || c.name == "stats watch" || c.name == "run attach" {
			// These keep polling a daemon that answers; covered on their own.
			continue
		}
		t.Run(c.name, func(t *testing.T) {
			d := fdNew(t).all(fdOK(nil))
			res := fdRun(t, d.endpoint, c.args...)
			fdRequireNoCrashText(t, res)
			if res.err != nil {
				assert.NotContains(t, res.err.Error(), "\n")
			}
		})
	}
}

// A daemon that answers an RPC error whose message is empty still fails the
// command, and the command still says which command failed.
func TestDaemonFault_EmptyErrorMessage(t *testing.T) {
	for _, c := range fdCommands() {
		t.Run(c.name, func(t *testing.T) {
			d := fdNew(t).all(func(int, api.Request) fdReply {
				return fdReply{rpcErr: &api.RPCError{Code: -32000}}
			})
			res := fdRun(t, d.endpoint, c.args...)
			require.Nil(t, res.panicked)
			require.Error(t, res.err, "an error response must fail the command even without a message")
			assert.Empty(t, res.stdout)
		})
	}
}
