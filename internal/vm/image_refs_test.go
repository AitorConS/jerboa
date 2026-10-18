package vm

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestImageDigestsInUse(t *testing.T) {
	a := strings.Repeat("a", 64)
	b := strings.Repeat("b", 64)
	c := strings.Repeat("c", 64)
	got := ImageDigestsInUse([]*VM{
		nil,
		{Cfg: Config{ImageRef: "sha256:" + a}},
		{Cfg: Config{ImageRef: "web:latest", ImagePath: filepath.Join("/store", b, "disk.img")}},
		{Cfg: Config{ImageDigest: "sha256:" + c}},
		{Cfg: Config{ImagePath: "/srv/custom/raw.img"}},
	})
	require.Equal(t, map[string]bool{"sha256:" + a: true, "sha256:" + b: true, "sha256:" + c: true}, got)
}
