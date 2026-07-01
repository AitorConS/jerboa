package main

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

func TestNeedsDaemon(t *testing.T) {
	// Build a small command tree rooted at "jerboa" to mirror the real CLI: the
	// root and offline groups (sign, pkg, …) need no daemon, while a remote verb
	// like "run" does.
	root := &cobra.Command{Use: "jerboa"}

	sign := &cobra.Command{Use: "sign"}
	signAdd := &cobra.Command{Use: "add"}
	sign.AddCommand(signAdd)

	run := &cobra.Command{Use: "run"}
	root.AddCommand(sign, run)

	require.False(t, needsDaemon(root), "root itself needs no daemon")
	require.False(t, needsDaemon(signAdd), "offline group descendants need no daemon")
	require.False(t, needsDaemon(sign), "offline group needs no daemon")
	require.True(t, needsDaemon(run), "remote verbs need the daemon")
}

func TestSigningStorePath(t *testing.T) {
	require.True(t, strings.HasSuffix(signingStorePath(), ".jerboa"),
		"signing store lives under the user's .jerboa directory")
}
