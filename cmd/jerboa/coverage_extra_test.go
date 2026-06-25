package main

import (
	"strings"
	"testing"

	"github.com/AitorConS/jerboa/internal/api"
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

func TestPrintServiceInfo(t *testing.T) {
	// Exercises both the base fields and the optional replica-ID branch; output
	// goes to stdout, so this just guards against a formatting panic.
	require.NotPanics(t, func() {
		printServiceInfo(api.ServiceInfoResult{
			Name:            "web",
			Image:           "nginx",
			DesiredReplicas: 3,
			ReadyReplicas:   2,
			Strategy:        "rolling",
			Health:          "healthy",
			CreatedAt:       "now",
			UpdatedAt:       "later",
			ReplicaIDs:      []string{"r1", "r2"},
		})
	})
	require.NotPanics(t, func() {
		printServiceInfo(api.ServiceInfoResult{Name: "empty"})
	})
}
