package image

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func validManifest() Manifest {
	return Manifest{
		SchemaVersion: SchemaVersion,
		Name:          "hello",
		Tag:           "latest",
		Created:       time.Now().UTC(),
		Config:        Config{Memory: "256M", CPUs: 1},
		DiskDigest:    "sha256:abc123def456abc123def456abc123def456abc123def456abc123def456abc1",
		DiskSize:      1 << 20,
	}
}

func TestParse_valid(t *testing.T) {
	data, err := Marshal(validManifest())
	require.NoError(t, err)
	m, err := Parse(data)
	require.NoError(t, err)
	require.Equal(t, "hello", m.Name)
	require.Equal(t, "latest", m.Tag)
}

func TestParse_invalid_json(t *testing.T) {
	_, err := Parse([]byte(`{invalid json`))
	require.Error(t, err)
}

func TestParse_table(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*Manifest)
		wantErr string
	}{
		{
			name:    "wrong schema version",
			mutate:  func(m *Manifest) { m.SchemaVersion = 99 },
			wantErr: "unsupported schemaVersion",
		},
		{
			name:    "missing name",
			mutate:  func(m *Manifest) { m.Name = "" },
			wantErr: "name is required",
		},
		{
			name:    "missing tag",
			mutate:  func(m *Manifest) { m.Tag = "" },
			wantErr: "tag is required",
		},
		{
			name:    "missing disk digest",
			mutate:  func(m *Manifest) { m.DiskDigest = "" },
			wantErr: "diskDigest is required",
		},
		{
			name:    "invalid disk digest prefix",
			mutate:  func(m *Manifest) { m.DiskDigest = "md5:abc123" },
			wantErr: "diskDigest must start with sha256",
		},
		{
			name:    "zero disk size",
			mutate:  func(m *Manifest) { m.DiskSize = 0 },
			wantErr: "diskSize must be positive",
		},
		{
			name:    "negative disk size",
			mutate:  func(m *Manifest) { m.DiskSize = -1 },
			wantErr: "diskSize must be positive",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := validManifest()
			tc.mutate(&m)
			data, err := Marshal(m)
			require.NoError(t, err)
			_, err = Parse(data)
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

func TestManifest_Ref(t *testing.T) {
	m := validManifest()
	require.Equal(t, "hello:latest", m.Ref())
}

func TestDigestSHA256(t *testing.T) {
	d := DigestSHA256([]byte("hello"))
	require.Greater(t, len(d), 7)
	require.Equal(t, "sha256:", d[:7])
}

func TestMarshal_roundtrip(t *testing.T) {
	m := validManifest()
	m.Config.Env = []string{"K1=V1", "K2=V2"}
	data, err := Marshal(m)
	require.NoError(t, err)

	var raw map[string]interface{}
	require.NoError(t, json.Unmarshal(data, &raw))
	require.Equal(t, "hello", raw["name"])

	got, err := Parse(data)
	require.NoError(t, err)
	require.Equal(t, m.Name, got.Name)
	require.Equal(t, m.Tag, got.Tag)
	require.Equal(t, m.DiskDigest, got.DiskDigest)
	require.Equal(t, m.DiskSize, got.DiskSize)
	require.Equal(t, m.Config.Env, got.Config.Env)
}

func TestMarshal_emptyEnv(t *testing.T) {
	m := validManifest()
	data, err := Marshal(m)
	require.NoError(t, err)
	require.NotContains(t, string(data), `"env"`)
}

func TestMarshal_withEnv(t *testing.T) {
	m := validManifest()
	m.Config.Env = []string{"FOO=bar"}
	data, err := Marshal(m)
	require.NoError(t, err)
	require.Contains(t, string(data), `"env"`)
}

func TestParse_diskDigestShort(t *testing.T) {
	cases := []struct {
		name    string
		digest  string
		wantErr bool
	}{
		{"too short", "sha256:", true},
		{"wrong prefix", "sha1:abc123def456abc123def456abc123def456abc1", true},
		{"valid length", "sha256:abc123def456abc123def456abc123def456abc123def456abc123def456abc1", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := validManifest()
			m.DiskDigest = tc.digest
			data, err := Marshal(m)
			require.NoError(t, err)
			_, err = Parse(data)
			if tc.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestConfig_zeroValues(t *testing.T) {
	var c Config
	require.Empty(t, c.Memory)
	require.Equal(t, 0, c.CPUs)
	require.Nil(t, c.Env)
}
