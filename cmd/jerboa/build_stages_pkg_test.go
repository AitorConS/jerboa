//go:build !windows

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/AitorConS/jerboa/internal/api"
	"github.com/AitorConS/jerboa/internal/builder"
	pkg "github.com/AitorConS/jerboa/internal/package"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A multi-stage build, like a single-language one, takes the runtime the
// user names with --pkg instead of looking up the automatic one, so a
// runtime the primary ops namespace lacks can come from another publisher
// explicitly.
func TestBuildStagesTakeTheRuntimeNamedWithPkg(t *testing.T) {
	elfData, err := os.ReadFile(lfELF(t, filepath.Join(t.TempDir(), "node")))
	require.NoError(t, err)
	archive := createOpsResolveArchive(t, map[string]string{"node": string(elfData)})
	sum := sha256.Sum256(archive)
	setupOpsResolveServer(t, pkg.OpsPackageList{Version: 1, Packages: []pkg.OpsPackage{
		{Namespace: "nanovms", Name: "node", Version: "v20.1.0", Language: "node", SHA256: hex.EncodeToString(sum[:]), Arch: pkg.ArchSlug()},
	}}, map[string][]byte{"/nanovms/node/v20.1.0.tar.gz": archive})
	withTempOpsStore(t)

	tools := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(tools, "npm"), []byte("#!/bin/sh\nexit 0\n"), 0o700)) //nolint:gosec // the test's own script must be executable
	t.Setenv("PATH", tools+string(os.PathListSeparator)+"/bin"+string(os.PathListSeparator)+"/usr/bin")

	project := filepath.Join(t.TempDir(), "svc")
	lfWrite(t, filepath.Join(project, "package.json"), `{"name":"svc","main":"index.js","engines":{"node":"20"}}`)
	lfWrite(t, filepath.Join(project, "index.js"), "console.log('hi')\n")
	lfWrite(t, filepath.Join(project, builder.ConfigFileName), "[[stages]]\nname = \"app\"\nlang = \"node\"\n")

	t.Run("without --pkg the primary namespace is required", func(t *testing.T) {
		d := fdNew(t).all(fdOK(nil))
		res := lfBuildWith(t, d.endpoint, "--no-preflight", project)
		fdRequireCleanFailure(t, res, "eyberg namespace", "--pkg <namespace>/node:20")
		assert.Empty(t, d.methods())
	})
	t.Run("--pkg names the runtime", func(t *testing.T) {
		d := fdNew(t).on("Image.Build", func(n int, req api.Request) fdReply {
			rep := fdOK(api.ImageManifestResult{Name: "svc", Tag: "latest", DiskDigest: "sha256:abc", DiskSize: 4096})(n, req)
			rep.drain = true
			return rep
		})
		res := lfBuildWith(t, d.endpoint, "--no-preflight", "--pkg", "nanovms/node:v20.1.0", project)
		require.NoError(t, res.err, "stderr: %s", res.stderr)
		var p api.BuildParams
		d.params("Image.Build", 0, &p)
		require.Len(t, p.Packages, 1)
		assert.Equal(t, "nanovms/node", p.Packages[0].Name)
	})
}
