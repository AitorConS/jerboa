//go:build linux

package main

import (
	"testing"

	"github.com/AitorConS/jerboa/internal/api"
	"github.com/AitorConS/jerboa/internal/vm"
	"github.com/stretchr/testify/require"
)

func TestParseRestartPolicy(t *testing.T) {
	cases := []struct {
		input string
		want  api.RestartSpec
	}{
		{"never", api.RestartSpec{Policy: "never"}},
		{"on-failure", api.RestartSpec{Policy: "on-failure"}},
		{"always", api.RestartSpec{Policy: "always"}},
		{"always:5", api.RestartSpec{Policy: "always", MaxRetries: 5}},
		{"on-failure:3", api.RestartSpec{Policy: "on-failure", MaxRetries: 3}},
		{"on-failure:0", api.RestartSpec{Policy: "on-failure", MaxRetries: 0}},
	}
	for _, tc := range cases {
		got, err := parseRestartPolicy(tc.input)
		require.NoError(t, err, "parseRestartPolicy(%q)", tc.input)
		require.Equal(t, tc.want, got, "parseRestartPolicy(%q)", tc.input)
	}
}

func TestParseRestartPolicy_Invalid(t *testing.T) {
	_, err := parseRestartPolicy("unsupported")
	require.Error(t, err)

	_, err = parseRestartPolicy("always:abc")
	require.Error(t, err)

	_, err = parseRestartPolicy("always:-1")
	require.Error(t, err)
}

func TestParseRestartPolicy_CaseInsensitive(t *testing.T) {
	got, err := parseRestartPolicy("Always")
	require.NoError(t, err)
	require.Equal(t, string(vm.RestartAlways), got.Policy)

	got, err = parseRestartPolicy("ON-FAILURE")
	require.NoError(t, err)
	require.Equal(t, string(vm.RestartOnFailure), got.Policy)
}
