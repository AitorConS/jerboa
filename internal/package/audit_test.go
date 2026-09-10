package pkg

import (
	"archive/tar"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAuditScratchRelativeEntrypoint(t *testing.T) {
	archive := writeSyntheticTar(t, []tar.Header{{Name: "bin/app", Typeflag: tar.TypeReg}, {Name: "bin/link", Typeflag: tar.TypeSymlink, Linkname: "app"}}, map[string]string{"bin/app": "binary"})
	fs := newContainerFS(t, archive)
	cfg := &DockerImageConfig{Env: []string{"PATH=/bin"}}
	p, err := resolveDockerProgram(fs, cfg, "app")
	require.NoError(t, err)
	require.Equal(t, "/bin/app", p)
	p, err = resolveDockerProgram(fs, cfg, "link")
	require.NoError(t, err)
	require.Equal(t, "/bin/app", p)
	_, err = resolveDockerProgram(fs, cfg, "missing")
	require.Error(t, err)
}
