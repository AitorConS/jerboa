package ociregistry

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseManifest_Valid(t *testing.T) {
	t.Parallel()

	input := []byte(`{
	  "schemaVersion": 2,
	  "mediaType": "application/vnd.oci.image.manifest.v1+json",
	  "config": {
	    "mediaType": "application/vnd.oci.image.config.v1+json",
	    "digest": "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	    "size": 123
	  },
	  "layers": [
	    {
	      "mediaType": "application/vnd.oci.image.layer.v1.tar+gzip",
	      "digest": "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
	      "size": 456
	    }
	  ]
	}`)

	m, err := ParseManifest(input)
	require.NoError(t, err)
	require.Equal(t, OCIManifestSchemaVersion, m.SchemaVersion)
	require.Len(t, m.Layers, 1)
}

func TestParseManifest_Invalid(t *testing.T) {
	t.Parallel()

	_, err := ParseManifest([]byte(`{"schemaVersion":1}`))
	require.Error(t, err)
	require.Contains(t, err.Error(), "unsupported schemaVersion")
}

func TestValidateManifest(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		m    Manifest
		err  string
	}{
		{
			name: "missing layers",
			m: Manifest{
				SchemaVersion: OCIManifestSchemaVersion,
				Config: Descriptor{
					MediaType: MediaTypeImageConfig,
					Digest:    "sha256:aaa",
					Size:      1,
				},
			},
			err: "layers is required",
		},
		{
			name: "invalid config digest",
			m: Manifest{
				SchemaVersion: OCIManifestSchemaVersion,
				Config: Descriptor{
					MediaType: MediaTypeImageConfig,
					Digest:    "bad",
					Size:      1,
				},
				Layers: []Descriptor{{
					MediaType: MediaTypeImageLayerTarGzip,
					Digest:    "sha256:bbb",
					Size:      1,
				}},
			},
			err: "config.digest",
		},
		{
			name: "invalid layer size",
			m: Manifest{
				SchemaVersion: OCIManifestSchemaVersion,
				Config: Descriptor{
					MediaType: MediaTypeImageConfig,
					Digest:    "sha256:aaa",
					Size:      1,
				},
				Layers: []Descriptor{{
					MediaType: MediaTypeImageLayerTarGzip,
					Digest:    "sha256:bbb",
					Size:      0,
				}},
			},
			err: "layers[0].size",
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := ValidateManifest(tt.m)
			require.Error(t, err)
			require.Contains(t, err.Error(), tt.err)
		})
	}
}

func TestMarshalManifest(t *testing.T) {
	t.Parallel()

	m := Manifest{
		SchemaVersion: OCIManifestSchemaVersion,
		MediaType:     MediaTypeImageManifest,
		Config: Descriptor{
			MediaType: MediaTypeImageConfig,
			Digest:    "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			Size:      1,
		},
		Layers: []Descriptor{{
			MediaType: MediaTypeImageLayerTarGzip,
			Digest:    "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
			Size:      1,
		}},
	}

	data, err := MarshalManifest(m)
	require.NoError(t, err)
	require.Contains(t, string(data), "schemaVersion")
}
