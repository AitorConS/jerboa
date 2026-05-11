package main

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseRegistryQuery(t *testing.T) {
	url, query, err := parseRegistryQuery("registry.example.com:5000/hello")
	require.NoError(t, err)
	require.Equal(t, "http://registry.example.com:5000", url)
	require.Equal(t, "hello", query)
}

func TestParseRegistryQuery_WithScheme(t *testing.T) {
	url, query, err := parseRegistryQuery("https://registry.example.com/hello")
	require.NoError(t, err)
	require.Equal(t, "https://registry.example.com", url)
	require.Equal(t, "hello", query)
}

func TestParseRegistryQuery_Invalid(t *testing.T) {
	_, _, err := parseRegistryQuery("registry.example.com")
	require.Error(t, err)
}
