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
	r, err := buildSizeReport(bin, files)
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
	files, err := sourceFiles(dir, include)
	require.NoError(t, err)
	var got []string
	for _, f := range files {
		got = append(got, f.GuestPath)
	}
	require.ElementsMatch(t, []string{"server.js", "lib/db.js"}, got, strings.Join(got, ","))
}
