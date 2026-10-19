package main

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLocalName(t *testing.T) {
	for in, want := range map[string][2]string{
		"aitorcons/web:v1":         {"aitorcons/web", "v1"},
		"aitorcons/web":            {"aitorcons/web", "latest"},
		"localhost:8788/a/web:dev": {"localhost:8788/a/web", "dev"},
		"web:v2":                   {"web", "v2"},
		"web":                      {"web", "latest"},
	} {
		name, tag := localName(in)
		require.Equal(t, want, [2]string{name, tag}, in)
	}
}

func TestSplitImageArgKeepsRegistryReferences(t *testing.T) {
	for _, ref := range []string{"aitorcons/web:v1", "localhost:8788/acme/api:dev", "web:latest"} {
		img, path, err := splitImageArg(ref)
		require.NoError(t, err)
		require.Equal(t, ref, img)
		require.Empty(t, path)
	}
	img, path, err := splitImageArg("./disk.img")
	require.NoError(t, err)
	require.Empty(t, img)
	require.Equal(t, "./disk.img", path)
}
