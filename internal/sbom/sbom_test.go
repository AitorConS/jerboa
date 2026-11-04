package sbom

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	pkg "github.com/AitorConS/jerboa/internal/package"
	"github.com/stretchr/testify/require"
)

func write(t *testing.T, dir, rel, content string) pkg.File {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(rel))
	require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
	require.NoError(t, os.WriteFile(p, []byte(content), 0o644))
	return pkg.File{HostPath: p, GuestPath: rel, FromContext: true}
}

func decode(t *testing.T, data []byte) BOM {
	t.Helper()
	var bom BOM
	require.NoError(t, json.Unmarshal(data, &bom))
	return bom
}

func purls(bom BOM) []string {
	out := make([]string, len(bom.Components))
	for i, c := range bom.Components {
		out[i] = c.PURL
	}
	return out
}

func TestGenerate(t *testing.T) {
	dir := t.TempDir()
	files := []pkg.File{
		write(t, dir, "node_modules/express/package.json", `{"name":"express","version":"4.19.2","license":"MIT"}`),
		write(t, dir, "node_modules/express/node_modules/debug/package.json", `{"name":"debug","version":"2.6.9","license":{"type":"MIT"}}`),
		// Not a module's own manifest.
		write(t, dir, "node_modules/express/lib/package.json", `{"name":"inner","version":"1.0.0"}`),
		write(t, dir, "node_modules/@types/node/package.json", `{"name":"@types/node","version":"20.1.0"}`),
		// The same module installed twice is one component.
		write(t, dir, "node_modules/x/node_modules/debug/package.json", `{"name":"debug","version":"2.6.9"}`),
		write(t, dir, "lib/python3.12/site-packages/Flask_Cors-4.0.1.dist-info/METADATA",
			"Metadata-Version: 2.1\nName: Flask_Cors\nVersion: 4.0.1\nLicense: MIT\n\nName: not a header\n"),
		write(t, dir, "app.py", "print('hi')\n"),
	}
	src := t.TempDir()
	write(t, src, "Cargo.toml", "[package]\nname = \"app\"\n")
	write(t, src, "Cargo.lock", `version = 3

[[package]]
name = "app"
version = "0.1.0"

[[package]]
name = "serde"
version = "1.0.200"
source = "registry+https://github.com/rust-lang/crates.io-index"
checksum = "`+strings.Repeat("ab", 32)+`"
`)
	program := write(t, dir, "bin/app", "not an ELF")
	in := Input{
		Platform:  "linux/arm64",
		Program:   program.HostPath,
		Files:     files,
		Packages:  []pkg.Reference{{Source: "ops", Name: "eyberg/python", Version: "3.12.0", Platform: "linux/arm64", SHA256: strings.Repeat("cd", 32)}},
		SourceDir: src,
		Tool:      "v0.57.0",
		Created:   "2026-11-04T00:00:00Z",
	}
	data, err := Generate(in)
	require.NoError(t, err)
	require.NoError(t, Check(data))
	again, err := Generate(in)
	require.NoError(t, err)
	require.Equal(t, string(data), string(again), "the same inputs give the same document")

	bom := decode(t, data)
	require.Equal(t, "CycloneDX", bom.BOMFormat)
	require.Equal(t, "1.6", bom.SpecVersion)
	require.Regexp(t, `^urn:uuid:[0-9a-f]{8}-[0-9a-f]{4}-8[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`, bom.SerialNumber)
	require.Equal(t, "2026-11-04T00:00:00Z", bom.Metadata.Timestamp)
	require.Equal(t, "app", bom.Metadata.Component.Name)
	require.Len(t, bom.Metadata.Component.Hashes, 1)
	require.Equal(t, []Property{{Name: "jerboa:platform", Value: "linux/arm64"}}, bom.Metadata.Properties)
	require.Equal(t, []string{
		"pkg:cargo/serde@1.0.200",
		"pkg:generic/eyberg/python@3.12.0?platform=linux%2Farm64",
		"pkg:npm/%40types/node@20.1.0",
		"pkg:npm/debug@2.6.9",
		"pkg:npm/express@4.19.2",
		"pkg:pypi/flask-cors@4.0.1",
	}, purls(bom))
	for _, c := range bom.Components {
		switch c.Name {
		case "express", "debug", "Flask_Cors":
			require.Equal(t, []License{{License: LicenseName{Name: "MIT"}}}, c.Licenses, c.Name)
		case "serde":
			require.Equal(t, []Hash{{Alg: "SHA-256", Content: strings.Repeat("ab", 32)}}, c.Hashes)
		}
	}

	// No timestamp without SOURCE_DATE_EPOCH, and a different document.
	in.Created = ""
	other, err := Generate(in)
	require.NoError(t, err)
	require.NotContains(t, string(other), `"timestamp"`)
	require.NotEqual(t, bom.SerialNumber, decode(t, other).SerialNumber)
}

func TestGoModules(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not available")
	}
	dir := t.TempDir()
	write(t, dir, "go.mod", "module example.com/hello\n\ngo 1.22\n")
	write(t, dir, "main.go", "package main\n\nfunc main() {}\n")
	bin := filepath.Join(dir, "hello")
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Dir = dir
	build.Env = append(os.Environ(), "GOOS=linux", "GOARCH=arm64", "CGO_ENABLED=0", "GOFLAGS=-mod=mod", "GOWORK=off")
	out, err := build.CombinedOutput()
	require.NoError(t, err, string(out))

	data, err := Generate(Input{Platform: "linux/arm64", Program: bin})
	require.NoError(t, err)
	bom := decode(t, data)
	require.Equal(t, "hello", bom.Metadata.Component.Name)
	require.Len(t, bom.Components, 1)
	require.Equal(t, "stdlib", bom.Components[0].Name)
	require.True(t, strings.HasPrefix(bom.Components[0].PURL, "pkg:golang/stdlib@1."), bom.Components[0].PURL)
}

func TestCheck(t *testing.T) {
	require.ErrorContains(t, Check([]byte(`{"bomFormat":"SPDX"}`)), "not a CycloneDX")
	require.ErrorContains(t, Check([]byte(`nope`)), "not JSON")
	require.ErrorContains(t, Check(make([]byte, MaxSize+1)), "limit")
	require.NoError(t, Check([]byte(`{"bomFormat":"CycloneDX","specVersion":"1.6"}`)))
}

func TestPURL(t *testing.T) {
	require.Equal(t, "pkg:golang/github.com/x/y@v1.0.0%2Bincompatible", purl("golang", "github.com/x/y", "v1.0.0+incompatible"))
	require.Equal(t, "pkg:npm/%40scope/name", purl("npm", "@scope/name", ""))
}
