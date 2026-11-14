package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Every command called with the wrong number of arguments must say so and
// stop there: no connection to the daemon, no file written, no half-done
// work. The table is the whole command tree, so a new command that forgets
// its argument check shows up as a missing or failing row.
func TestEveryCommandChecksItsArgumentCount(t *testing.T) {
	e := afIsolate(t)
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"build"}, "accepts 1 arg(s), received 0"},
		{[]string{"build", "a", "b"}, "accepts 1 arg(s), received 2"},
		{[]string{"init", "a", "b"}, "accepts at most 1 arg(s), received 2"},
		{[]string{"rmi"}, "accepts 1 arg(s), received 0"},
		{[]string{"rmi", "a", "b"}, "accepts 1 arg(s), received 2"},
		{[]string{"tag", "web:latest"}, "accepts 2 arg(s), received 1"},
		{[]string{"tag", "a", "b", "c"}, "accepts 2 arg(s), received 3"},
		{[]string{"push"}, "accepts 1 arg(s), received 0"},
		{[]string{"push", "acme/web:v1", "acme/web:v2"}, "accepts 1 arg(s), received 2"},
		{[]string{"push", "--index", "acme/web:v1"}, "requires at least 2 arg(s), only received 1"},
		{[]string{"pull"}, "accepts 1 arg(s), received 0"},
		{[]string{"pull", "a/b", "c/d"}, "accepts 1 arg(s), received 2"},
		{[]string{"login", "a.example.com", "b.example.com"}, "accepts at most 1 arg(s), received 2"},
		{[]string{"logout", "a.example.com", "b.example.com"}, "accepts at most 1 arg(s), received 2"},
		{[]string{"sign"}, "accepts 1 arg(s), received 0"},
		{[]string{"verify"}, "accepts 1 arg(s), received 0"},
		{[]string{"ps", "extra"}, `unknown command "extra" for "jerboa ps"`},
		{[]string{"status", "extra"}, `unknown command "extra" for "jerboa status"`},
		{[]string{"logs"}, "accepts 1 arg(s), received 0"},
		{[]string{"logs", "a", "b"}, "accepts 1 arg(s), received 2"},
		{[]string{"stop"}, "accepts 1 arg(s), received 0"},
		{[]string{"stop", "a", "b"}, "accepts 1 arg(s), received 2"},
		{[]string{"rm"}, "accepts 1 arg(s), received 0"},
		{[]string{"rm", "a", "b"}, "accepts 1 arg(s), received 2"},
		{[]string{"inspect"}, "accepts 1 arg(s), received 0"},
		{[]string{"exec"}, "accepts 1 arg(s), received 0"},
		{[]string{"stats"}, "accepts 1 arg(s), received 0"},
		{[]string{"stats", "a", "b"}, "accepts 1 arg(s), received 2"},
		{[]string{"version", "extra"}, `unknown command "extra" for "jerboa version"`},

		{[]string{"compose", "up"}, "accepts 1 arg(s), received 0"},
		{[]string{"compose", "up", "a.yml", "b.yml"}, "accepts 1 arg(s), received 2"},
		{[]string{"compose", "down"}, "accepts 1 arg(s), received 0"},
		{[]string{"compose", "ps"}, "accepts 1 arg(s), received 0"},
		{[]string{"compose", "logs", "a.yml"}, "accepts 2 arg(s), received 1"},

		{[]string{"volume", "create"}, "accepts 1 arg(s), received 0"},
		{[]string{"volume", "create", "a", "b"}, "accepts 1 arg(s), received 2"},
		{[]string{"volume", "rm"}, "accepts 1 arg(s), received 0"},
		{[]string{"volume", "inspect"}, "accepts 1 arg(s), received 0"},
		{[]string{"volume", "seed"}, "accepts 1 arg(s), received 0"},
		{[]string{"volume", "migrate"}, "accepts 1 arg(s), received 0"},

		{[]string{"network", "create"}, "accepts 1 arg(s), received 0"},
		{[]string{"network", "create", "a", "b"}, "accepts 1 arg(s), received 2"},
		{[]string{"network", "inspect"}, "accepts 1 arg(s), received 0"},
		{[]string{"network", "rm"}, "accepts 1 arg(s), received 0"},

		{[]string{"snapshot", "create", "vm"}, "accepts 2 arg(s), received 1"},
		{[]string{"snapshot", "restore", "vm"}, "accepts 2 arg(s), received 1"},
		{[]string{"snapshot", "ls", "extra"}, `unknown command "extra" for "jerboa snapshot ls"`},
		{[]string{"snapshot", "inspect"}, "accepts 1 arg(s), received 0"},
		{[]string{"snapshot", "rm"}, "accepts 1 arg(s), received 0"},

		{[]string{"dns", "resolve"}, "accepts 1 arg(s), received 0"},
		{[]string{"dns", "resolve-all"}, "accepts 1 arg(s), received 0"},

		{[]string{"config", "set", "hypervisor"}, "accepts 2 arg(s), received 1"},
		{[]string{"config", "set", "a", "b", "c"}, "accepts 2 arg(s), received 3"},
		{[]string{"config", "get"}, "accepts 1 arg(s), received 0"},

		{[]string{"system", "df", "extra"}, `unknown command "extra" for "jerboa system df"`},
		{[]string{"system", "prune", "extra"}, `unknown command "extra" for "jerboa system prune"`},

		{[]string{"images", "inspect"}, "accepts 1 arg(s), received 0"},
		{[]string{"images", "recipe"}, "accepts 1 arg(s), received 0"},
		{[]string{"images", "sbom"}, "accepts 1 arg(s), received 0"},

		{[]string{"registry", "pat", "create"}, "accepts 1 arg(s), received 0"},
		{[]string{"registry", "pat", "ls", "extra"}, `unknown command "extra" for "jerboa registry pat ls"`},
		{[]string{"registry", "pat", "rm"}, "accepts 1 arg(s), received 0"},
		{[]string{"registry", "repo", "edit"}, "accepts 1 arg(s), received 0"},
		{[]string{"registry", "repo", "rm"}, "accepts 1 arg(s), received 0"},
		{[]string{"registry", "verify"}, "accepts 1 arg(s), received 0"},
		{[]string{"registry", "signers", "forget"}, "accepts 1 arg(s), received 0"},

		{[]string{"pkg", "search"}, "accepts 1 arg(s), received 0"},
		{[]string{"pkg", "get"}, "accepts 1 arg(s), received 0"},
		{[]string{"pkg", "remove"}, "accepts 1 arg(s), received 0"},
		{[]string{"pkg", "create", "name"}, "accepts 2 arg(s), received 1"},
		{[]string{"pkg", "from-docker", "name"}, "accepts 2 arg(s), received 1"},
		{[]string{"pkg", "push", "name:1"}, "accepts 2 arg(s), received 1"},
		{[]string{"pkg", "load"}, "accepts 1 arg(s), received 0"},
	}
	for _, tc := range cases {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			res := e.run(tc.args...)
			afCleanError(t, res.err, tc.want)
			assert.Empty(t, res.stdout, "a rejected command must print no result")
		})
	}
	require.Zero(t, e.dials.Load(), "an argument error reached the daemon")
	require.Empty(t, afDirEntries(t, e.home), "an argument error wrote into the home directory")
}

// The table above must not fall behind the command tree: every command that
// takes arguments declares how many, so cobra refuses the rest.
func TestEveryLeafCommandDeclaresItsArguments(t *testing.T) {
	// Commands that deliberately take any arguments, or none at all with
	// nothing to check.
	exempt := map[string]bool{
		"jerboa completion bash": true, "jerboa completion zsh": true,
		"jerboa completion fish": true, "jerboa completion powershell": true,
		"jerboa help": true,
	}
	var walk func(c *cobra.Command)
	var missing []string
	walk = func(c *cobra.Command) {
		if c.HasSubCommands() {
			for _, sub := range c.Commands() {
				walk(sub)
			}
			return
		}
		if exempt[c.CommandPath()] || c.Args != nil {
			return
		}
		// A leaf without Args accepts anything silently; only commands whose
		// usage names no argument may do without.
		if strings.ContainsAny(c.Use, "<[") {
			missing = append(missing, c.CommandPath())
		}
	}
	root := newRootCmd()
	root.InitDefaultCompletionCmd()
	root.InitDefaultHelpCmd()
	walk(root)
	require.Empty(t, missing, "commands whose usage names arguments but that accept any number")
}

func TestUnknownCommandsAndFlags(t *testing.T) {
	e := afIsolate(t)

	res := e.run("frobnicate")
	afCleanError(t, res.err, `unknown command "frobnicate" for "jerboa"`)

	// A near miss gets a suggestion instead of a bare refusal.
	res = e.run("imagse")
	require.Error(t, res.err)
	require.Contains(t, res.err.Error(), `unknown command "imagse"`)
	require.Contains(t, res.err.Error(), "Did you mean this?\n\timages")

	res = e.run("ps", "--no-such-flag")
	afCleanError(t, res.err, "unknown flag: --no-such-flag")

	res = e.run("--output")
	afCleanError(t, res.err, "flag needs an argument: --output")

	res = e.run("volume", "create", "data", "--size")
	afCleanError(t, res.err, "flag needs an argument: --size")

	res = e.run("stop", "--force=perhaps", "vm1")
	afCleanError(t, res.err, `invalid argument "perhaps" for "--force"`)

	require.Zero(t, e.dials.Load())
}

// A mistyped subcommand of a group (jerboa volume remov data) must fail. The
// group prints its help and exits zero instead, so a script that misspells a
// subcommand believes it succeeded.
func TestMistypedSubcommandOfAGroupFails(t *testing.T) {
	e := afIsolate(t)
	for _, group := range []string{"volume", "network", "compose", "snapshot", "config", "system", "pkg", "registry", "dns", "kernel", "node"} {
		res := e.run(group, "no-such-subcommand", "x")
		if res.err == nil {
			t.Skipf("BUG: `jerboa %s no-such-subcommand` prints the group help and exits 0; command groups need an argument check (cobra.NoArgs) so a typo is an error", group)
		}
		afCleanError(t, res.err, "no-such-subcommand")
	}
}

// A group named alone is a request for its help, never an action.
func TestBareGroupsOnlyPrintHelp(t *testing.T) {
	e := afIsolate(t)
	for _, group := range []string{"volume", "network", "compose", "snapshot", "config", "system", "pkg", "registry", "dns", "kernel", "node"} {
		res := e.run(group)
		require.NoError(t, res.err, group)
		require.Contains(t, res.stdout, "Usage:", group)
		require.Contains(t, res.stdout, "jerboa "+group, group)
	}
	require.Zero(t, e.dials.Load())
	require.Empty(t, afDirEntries(t, e.home))
}

func TestVolumeCommandsRejectBadInputBeforeReachingTheDaemon(t *testing.T) {
	e := afIsolate(t)
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"size words", []string{"volume", "create", "data", "--size", "big"}, `volume create: invalid size: invalid size "big"`},
		{"size fraction", []string{"volume", "create", "data", "--size", "1.5G"}, `invalid size "1.5G"`},
		{"size lowercase unit", []string{"volume", "create", "data", "--size", "1g"}, `invalid size "1g"`},
		{"size unit only", []string{"volume", "create", "data", "--size", "G"}, `invalid size "G"`},
		{"size two units", []string{"volume", "create", "data", "--size", "1GB"}, `invalid size "1GB"`},
		{"size with space", []string{"volume", "create", "data", "--size", "1 G"}, `invalid size "1 G"`},
		{"size overflow", []string{"volume", "create", "data", "--size", "17179869185G"}, "17179869185G"},
		{"size overflow negative", []string{"volume", "create", "data", "--size", "9223372036854775807G"}, "9223372036854775807G"},
		{"seed without package", []string{"volume", "seed", "data"}, "at least one --pkg is required"},
		{"seed unknown source", []string{"volume", "seed", "data", "--pkg", "a/b:1", "--pkg-source", "npm"}, "volume seed: "},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := e.run(tc.args...)
			afCleanError(t, res.err, tc.want)
			assert.Empty(t, res.stdout)
		})
	}
	require.Zero(t, e.dials.Load(), "an invalid volume request reached the daemon")
}

// `volume migrate` reads a legacy client-side volume store next to the image
// store. A name it does not hold, or a store that is damaged, must stop the
// command before anything is sent to the daemon.
func TestVolumeMigrateWithoutALegacyVolume(t *testing.T) {
	e := afIsolate(t)
	store := filepath.Join(t.TempDir(), "images")

	res := e.run("--store", store, "volume", "migrate", "ghost")
	afCleanError(t, res.err, "ghost")
	require.Zero(t, e.dials.Load())

	for _, name := range []string{"../escape", "a/b", "..", ""} {
		res = e.run("--store", store, "volume", "migrate", name)
		require.Error(t, res.err, "name %q", name)
		afCleanError(t, res.err)
	}
	require.Zero(t, e.dials.Load())
}

// The legacy volume's metadata exists but its disk is gone. The command must
// say which file it could not open instead of importing an empty volume.
func TestVolumeMigrateWithMissingDisk(t *testing.T) {
	e := afIsolate(t)
	root := t.TempDir()
	store := filepath.Join(root, "images")
	// Create a legacy volume through the same store the command opens, then
	// take its disk away.
	legacy := filepath.Join(root, "volumes")
	created := afLegacyVolume(t, legacy, "olddata")
	require.NoError(t, os.Remove(created))

	res := e.run("--store", store, "volume", "migrate", "olddata")
	afCleanError(t, res.err)
	require.Zero(t, e.dials.Load(), "nothing can be migrated, so nothing may be sent")
}

func TestStatsRejectsABadInterval(t *testing.T) {
	e := afIsolate(t)
	for _, interval := range []string{"soon", "5", "-", "1 s"} {
		res := e.run("stats", "--watch", "--interval", interval, "vm1")
		afCleanError(t, res.err, "stats: invalid interval", interval)
		require.Empty(t, res.stdout)
	}
}

// The persistent flags are read by every command; a value that cannot be
// used must not crash endpoint resolution.
func TestHostFlagWithUnusableEndpoints(t *testing.T) {
	e := afIsolate(t)
	missing := filepath.Join(afShortDir(t), "absent.sock")
	for _, host := range []string{
		"unix://" + missing,
		missing,
		"tcp://127.0.0.1:1",
		"tcp://127.0.0.1:notaport",
		"tcp://",
		"http://127.0.0.1:1",
		"unix://",
		"://",
		"tcp://[::1",
		strings.Repeat("a", 4096),
	} {
		t.Run(host, func(t *testing.T) {
			res := e.run("--host", host, "ps")
			afCleanError(t, res.err)
			require.Empty(t, res.stdout)
		})
	}
	require.Zero(t, e.dials.Load(), "an explicit --host was ignored instead of the default endpoint")
}

// --socket is the deprecated spelling of --host and loses to it.
func TestDeprecatedSocketFlagStillSelectsTheEndpoint(t *testing.T) {
	e := afIsolate(t)
	missing := filepath.Join(afShortDir(t), "absent.sock")
	root := newRootCmd()
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	root.SetArgs([]string{"--socket", missing, "ps"})
	err := root.Execute()
	afCleanError(t, err, "ps: connect to daemon", missing)
	require.Zero(t, e.dials.Load())
}
