package main

import (
	"testing"

	pkg "github.com/AitorConS/jerboa/internal/package"
	"github.com/stretchr/testify/require"
)

// A bare runtime name resolves only in the primary ops namespace: a gap
// there (a version or an architecture it lacks) must not hand the image's
// runtime to another publisher.
func TestLookupOpsPackageStaysInThePrimaryNamespace(t *testing.T) {
	manifest := &pkg.OpsPackageList{Packages: []pkg.OpsPackage{
		{Namespace: "eyberg", Name: "node", Version: "20.5.0", Arch: "amd64"},
		{Namespace: "nanovms", Name: "node", Version: "22.1.0", Arch: "amd64"},
		{Namespace: "myuniverse", Name: "node", Version: "20.9.0", Arch: "arm64"},
		{Namespace: "eyberg", Name: "python3", Version: "3.12.0", Arch: "amd64"},
		{Namespace: "myuniverse", Name: "python", Version: "3.12.1", Arch: "amd64"},
	}}
	got := lookupOpsPackage(manifest, "node", "20", "amd64")
	require.NotNil(t, got)
	require.Equal(t, "eyberg", got.Namespace)

	require.Nil(t, lookupOpsPackage(manifest, "node", "22", "amd64"), "a version only another namespace has")
	require.Nil(t, lookupOpsPackage(manifest, "node", "20", "arm64"), "an architecture only another namespace has")

	got = lookupOpsPackage(manifest, "python", "3.12", "amd64")
	require.NotNil(t, got)
	require.Equal(t, "eyberg", got.Namespace, "an alias in the primary namespace wins over the name in another")
	require.Equal(t, "python3", got.Name)
}
