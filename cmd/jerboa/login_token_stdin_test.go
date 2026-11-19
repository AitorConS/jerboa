package main

import (
	"os"
	"strings"
	"testing"

	"github.com/AitorConS/jerboa/internal/registry/credentials"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --token-stdin stores the token from the first line of standard input, so
// it never appears in the process list.
func TestLoginTokenStdin(t *testing.T) {
	for name, stdin := range map[string]string{
		"line":               lfPAT + "\n",
		"crlf":               lfPAT + "\r\n",
		"no newline":         lfPAT,
		"spaces, more lines": "  " + lfPAT + "  \nignored\n",
	} {
		t.Run(name, func(t *testing.T) {
			reg := lfHealthyRegistry(t)
			fdIsolate(t)
			res := fdExec(t, stdin, "login", "--token-stdin", reg.host)
			require.NoError(t, res.err)
			assert.Contains(t, res.stdout, "Stored a token for "+reg.host)
			assert.NotContains(t, res.stdout+res.stderr, lfPAT)
			assert.NotContains(t, res.stderr, "warning")
			f, err := credentials.Load()
			require.NoError(t, err)
			assert.Equal(t, lfPAT, f.Registries[reg.host].Token)
		})
	}
}

func TestLoginTokenStdinRefusesNoToken(t *testing.T) {
	for name, stdin := range map[string]string{"empty": "", "blank line": "\n", "spaces": "   \nsecond\n"} {
		t.Run(name, func(t *testing.T) {
			reg := lfHealthyRegistry(t)
			home := fdIsolate(t)
			res := fdExec(t, stdin, "login", "--token-stdin", reg.host)
			require.Error(t, res.err)
			assert.Contains(t, res.err.Error(), "--token-stdin")
			_, err := os.Stat(lfCredentialsPath(home))
			require.ErrorIs(t, err, os.ErrNotExist, "nothing is stored")
			assert.Empty(t, reg.requests(), "the registry is not asked")
		})
	}
	// Larger than a pipe's buffer, so it is read directly rather than
	// through the command's standard input.
	_, err := readTokenLine(strings.NewReader(strings.Repeat("x", maxTokenLine+10) + "\n"))
	require.Error(t, err, "a line longer than any token")
}

// --token keeps working, with a warning on standard error, and cannot be
// combined with --token-stdin.
func TestLoginTokenFlagIsDeprecated(t *testing.T) {
	reg := lfHealthyRegistry(t)
	fdIsolate(t)
	res := fdExec(t, "", "login", "--token", lfPAT, reg.host)
	require.NoError(t, res.err)
	assert.Contains(t, res.stderr, "--token-stdin")
	f, err := credentials.Load()
	require.NoError(t, err)
	assert.Equal(t, lfPAT, f.Registries[reg.host].Token)

	res = fdExec(t, lfPAT+"\n", "login", "--token", lfPAT, "--token-stdin", reg.host)
	require.ErrorContains(t, res.err, "not both")

	help := fdExec(t, "", "login", "--help")
	require.NoError(t, help.err)
	assert.Contains(t, help.stdout, "--token-stdin")
	assert.Contains(t, help.stdout, "deprecated")
}
