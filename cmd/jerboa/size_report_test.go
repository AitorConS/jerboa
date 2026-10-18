package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AitorConS/jerboa/internal/builder"
	pkg "github.com/AitorConS/jerboa/internal/package"
	"github.com/stretchr/testify/require"
)

func TestSizeGroupKey(t *testing.T) {
	ref := &pkg.Reference{Name: "node", Version: "20.11.0"}
	for f, want := range map[pkg.File]string{
		{GuestPath: "lib/libc.so.6", Reference: ref}:                                      "package node:20.11.0",
		{GuestPath: "etc/resolv.conf"}:                                                    "package files",
		{GuestPath: "node_modules/express/lib/router.js", FromContext: true}:              "npm express",
		{GuestPath: "node_modules/@aws-sdk/client-s3/dist/index.js", FromContext: true}:   "npm @aws-sdk/client-s3",
		{GuestPath: "node_modules/a/node_modules/b/index.js", FromContext: true}:          "npm b",
		{GuestPath: "packages/flask/app.py", FromContext: true}:                           "python flask",
		{GuestPath: "packages/Flask-3.0.3.dist-info/RECORD", FromContext: true}:           "python flask",
		{GuestPath: "packages/typing_extensions.py", FromContext: true}:                   "python typing_extensions",
		{GuestPath: "venv/lib/python3.12/site-packages/numpy/core.so", FromContext: true}: "python numpy",
		{GuestPath: "public/logo.png", FromContext: true}:                                 "public/",
		{GuestPath: "server.js", FromContext: true}:                                       "server.js",
	} {
		require.Equal(t, want, sizeGroupKey(f), f.GuestPath)
	}
}

func TestBuildSizeReport(t *testing.T) {
	dir := t.TempDir()
	write := func(rel string, n int) string {
		p := filepath.Join(dir, rel)
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
		require.NoError(t, os.WriteFile(p, bytes.Repeat([]byte("x"), n), 0o644))
		return p
	}
	bin := write("runtime/node", 5000)
	files := []pkg.File{
		{HostPath: bin, GuestPath: "program"}, // the program is counted once
		{HostPath: write("node_modules/express/index.js", 3000), GuestPath: "node_modules/express/index.js", FromContext: true},
		{HostPath: write("node_modules/express/lib/a.js", 1000), GuestPath: "node_modules/express/lib/a.js", FromContext: true},
		{HostPath: write("server.js", 500), GuestPath: "server.js", FromContext: true},
		{GuestPath: "data", IsDir: true},
	}
	r, err := buildSizeReport(bin, "", files)
	require.NoError(t, err)
	require.Equal(t, int64(9500), r.TotalBytes)
	require.Equal(t, 4, r.TotalFiles)
	require.Equal(t, []sizeGroup{
		{Group: "program node", Bytes: 5000, Files: 1},
		{Group: "npm express", Bytes: 4000, Files: 2},
		{Group: "server.js", Bytes: 500, Files: 1},
	}, r.Groups)

	var text bytes.Buffer
	require.NoError(t, writeSizeReport(&text, r, "text"))
	require.Contains(t, text.String(), "npm express")
	require.Contains(t, text.String(), "42.1%")

	var js bytes.Buffer
	require.NoError(t, writeSizeReport(&js, r, "json"))
	var decoded sizeReport
	require.NoError(t, json.Unmarshal(js.Bytes(), &decoded))
	require.Equal(t, r, decoded)
}

func TestSourceFilesInclude(t *testing.T) {
	dir := t.TempDir()
	for _, rel := range []string{"server.js", "lib/db.js", "test/db.test.js", "README.md"} {
		p := filepath.Join(dir, rel)
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
		require.NoError(t, os.WriteFile(p, []byte("x"), 0o644))
	}
	include, err := builder.NewIncludeMatcher([]string{"server.js", "lib"})
	require.NoError(t, err)
	files, err := sourceFiles(dir, builder.LangNode, include)
	require.NoError(t, err)
	var got []string
	for _, f := range files {
		got = append(got, f.GuestPath)
	}
	require.ElementsMatch(t, []string{"server.js", "lib/db.js"}, got, strings.Join(got, ","))
}

func TestSourceFilesNodeKeepsInstalledDependencies(t *testing.T) {
	dir := t.TempDir()
	for _, rel := range []string{"server.js", "node_modules/express/index.js", ".git/HEAD"} {
		p := filepath.Join(dir, rel)
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
		require.NoError(t, os.WriteFile(p, []byte("x"), 0o644))
	}
	guests := func(lang builder.Lang) []string {
		files, err := sourceFiles(dir, lang, nil)
		require.NoError(t, err)
		var got []string
		for _, f := range files {
			got = append(got, f.GuestPath)
		}
		return got
	}
	// The Node driver installs production dependencies into node_modules; the
	// image needs them.
	require.ElementsMatch(t, []string{"server.js", "node_modules/express/index.js"}, guests(builder.LangNode))
	// Other languages keep ignoring a stray node_modules.
	require.ElementsMatch(t, []string{"server.js"}, guests(builder.LangPython))

	// A project can still exclude it explicitly.
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".unignore"), []byte("node_modules\n"), 0o644))
	require.ElementsMatch(t, []string{"server.js", ".unignore"}, guests(builder.LangNode))
}

// npm links command-line launchers into node_modules/.bin. Node builds leave
// that directory out, package other symlinks to project files with their
// target's content, and reject links that leave the project or dangle.
func TestSourceFilesSymlinks(t *testing.T) {
	dir := t.TempDir()
	write := func(rel string) {
		p := filepath.Join(dir, rel)
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
		require.NoError(t, os.WriteFile(p, []byte("content of "+rel), 0o644))
	}
	link := func(target, rel string) {
		p := filepath.Join(dir, rel)
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
		require.NoError(t, os.Symlink(target, p))
	}
	write("server.js")
	write("node_modules/next/dist/bin/next")
	link("../next/dist/bin/next", "node_modules/.bin/next")
	write("node_modules/a/node_modules/b/cli.js")
	link("../b/cli.js", "node_modules/a/node_modules/.bin/b")
	link("server.js", "alias.js")

	files, err := sourceFiles(dir, builder.LangNode, nil)
	require.NoError(t, err)
	got := map[string]string{}
	for _, f := range files {
		data, err := os.ReadFile(f.HostPath)
		require.NoError(t, err)
		got[f.GuestPath] = string(data)
	}
	require.NotContains(t, got, "node_modules/.bin/next")
	require.Equal(t, "content of node_modules/a/node_modules/b/cli.js", got["node_modules/a/node_modules/.bin/b"])
	require.Equal(t, "content of server.js", got["alias.js"])

	outside := filepath.Join(t.TempDir(), "secret")
	require.NoError(t, os.WriteFile(outside, []byte("x"), 0o644))
	link(outside, "leak.js")
	_, err = sourceFiles(dir, builder.LangNode, nil)
	require.ErrorContains(t, err, "leak.js")

	// Paths outside include are skipped whatever they are.
	include, err := builder.NewIncludeMatcher([]string{"server.js"})
	require.NoError(t, err)
	files, err = sourceFiles(dir, builder.LangNode, include)
	require.NoError(t, err)
	require.Len(t, files, 1)

	require.NoError(t, os.Remove(filepath.Join(dir, "leak.js")))
	link("missing.js", "dangling.js")
	_, err = sourceFiles(dir, builder.LangNode, nil)
	require.ErrorContains(t, err, "dangling.js")
}
