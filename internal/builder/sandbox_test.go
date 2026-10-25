package builder

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestBwrapArgs(t *testing.T) {
	home := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(home, ".aws"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(home, ".docker"), nil, 0o600))
	st := Step{Dir: "/src/app", Writable: []string{"/cache/go"}}
	argv := bwrapArgs("/usr/bin/bwrap", st, hiddenPaths(home, st), []string{"/tmp/step"}, "go", []string{"build", "./..."})
	line := strings.Join(argv, " ")

	require.Equal(t, "/usr/bin/bwrap", argv[0])
	require.Contains(t, line, "--ro-bind / /")
	require.NotContains(t, line, "--tmpfs /tmp ")
	require.Contains(t, line, "--bind /tmp/step /tmp/step")
	require.Contains(t, line, "--unshare-net")
	require.Contains(t, line, "--bind /cache/go /cache/go")
	require.Contains(t, line, "--bind /src/app /src/app --chdir /src/app")
	require.Contains(t, line, "--tmpfs "+filepath.Join(home, ".aws"))
	require.Contains(t, line, "--ro-bind /dev/null "+filepath.Join(home, ".docker"))
	require.True(t, strings.HasSuffix(line, "-- go build ./..."), line)

	st.Network = true
	require.NotContains(t, strings.Join(bwrapArgs("bwrap", st, nil, nil, "go", nil), " "), "--unshare-net")
}

func TestSeatbeltProfile(t *testing.T) {
	st := Step{Dir: "/src/a \"b\"", Writable: []string{"/cache"}}
	p := seatbeltProfile(st, []string{"/home/u/.aws"}, []string{"/tmp/step"})
	require.Contains(t, p, "(deny file-write*)")
	require.Contains(t, p, `(subpath "/src/a \"b\"")`)
	require.Contains(t, p, `(subpath "/cache")`)
	require.Contains(t, p, `(subpath "/tmp/step")`)
	require.Contains(t, p, `(deny file-read* (subpath "/home/u/.aws"))`)
	require.Contains(t, p, "(deny network*)")
	require.NotContains(t, p, "local ip")

	st.Network = true
	require.NotContains(t, seatbeltProfile(st, nil, nil), "network")
}

func TestHiddenPathsSparesTheProjectAndCaches(t *testing.T) {
	home := t.TempDir()
	for _, d := range []string{".jerboa", ".kube", ".docker"} {
		require.NoError(t, os.MkdirAll(filepath.Join(home, d), 0o700))
	}
	st := Step{Dir: filepath.Join(home, ".jerboa", "src"), Writable: []string{filepath.Join(home, ".docker")}}
	require.Equal(t, []string{filepath.Join(home, ".kube")}, hiddenPaths(home, st))
}

func TestSandboxOffRunsTheToolDirectly(t *testing.T) {
	cmd, done, err := Sandbox{Off: true}.Command(context.Background(), Step{Dir: t.TempDir(), Env: []string{"A=1"}}, "go", "version")
	require.NoError(t, err)
	defer done()
	require.Equal(t, "go", filepath.Base(cmd.Args[0]))
	require.Equal(t, []string{"A=1"}, cmd.Env)
}

func TestSandboxCreatesWritableDirsAndAPrivateTmp(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "npm")
	cmd, done, err := Sandbox{}.Command(context.Background(), Step{Dir: t.TempDir(), Writable: []string{cache}, Env: []string{}}, "true")
	require.NoError(t, err)
	require.DirExists(t, cache)
	var tmp string
	for _, kv := range cmd.Env {
		if v, ok := strings.CutPrefix(kv, "TMPDIR="); ok {
			tmp = v
		}
	}
	require.DirExists(t, tmp)
	done()
	require.NoDirExists(t, tmp)
}

func TestBuildSandboxFromConfig(t *testing.T) {
	var nilCfg *Config
	require.False(t, nilCfg.BuildSandbox().Off)
	for body, off := range map[string]bool{
		"[build]\nlang = \"go\"\n":                   false,
		"[build]\nsandbox = true\n":                  false,
		"[build]\nsandbox = false\nnetwork = true\n": true,
	} {
		cfg, err := parseConfig("unikernel.toml", []byte(body))
		require.NoError(t, err, body)
		require.Equal(t, off, cfg.BuildSandbox().Off, body)
	}
	cfg, err := parseConfig("unikernel.toml", []byte("[build]\nnetwork = true\n"))
	require.NoError(t, err)
	require.True(t, cfg.Build.Network)
}
