package ociregistry

import (
	"encoding/json"
	"fmt"
	"strings"
)

const (
	// OCIManifestSchemaVersion is the schema version for OCI image manifests.
	OCIManifestSchemaVersion = 2

	// MediaTypeImageManifest is the OCI image manifest media type.
	MediaTypeImageManifest = "application/vnd.oci.image.manifest.v1+json"
	// MediaTypeImageConfig is the OCI image config media type.
	MediaTypeImageConfig = "application/vnd.oci.image.config.v1+json"
	// MediaTypeImageLayerTarGzip is the OCI gzip-compressed tar layer media type.
	MediaTypeImageLayerTarGzip = "application/vnd.oci.image.layer.v1.tar+gzip"
)

// Descriptor identifies OCI content by digest, size, and media type.
type Descriptor struct {
	MediaType   string            `json:"mediaType"`
	Digest      string            `json:"digest"`
	Size        int64             `json:"size"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

// Manifest is an OCI image manifest.
type Manifest struct {
	SchemaVersion int               `json:"schemaVersion"`
	MediaType     string            `json:"mediaType"`
	Config        Descriptor        `json:"config"`
	Layers        []Descriptor      `json:"layers"`
	Annotations   map[string]string `json:"annotations,omitempty"`
}

// Config is a minimal OCI config payload.
type Config struct {
	Memory  string   `json:"memory,omitempty"`
	CPUs    int      `json:"cpus,omitempty"`
	Env     []string `json:"env,omitempty"`
	Created string   `json:"created,omitempty"`
}

// ParseManifest decodes and validates an OCI manifest.
func ParseManifest(data []byte) (Manifest, error) {
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return Manifest{}, fmt.Errorf("parse OCI manifest: %w", err)
	}
	if err := ValidateManifest(m); err != nil {
		return Manifest{}, fmt.Errorf("parse OCI manifest: %w", err)
	}
	return m, nil
}

// MarshalManifest serializes an OCI manifest.
func MarshalManifest(m Manifest) ([]byte, error) {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal OCI manifest: %w", err)
	}
	return data, nil
}

// ValidateManifest validates the required OCI manifest fields.
func ValidateManifest(m Manifest) error {
	if m.SchemaVersion != OCIManifestSchemaVersion {
		return fmt.Errorf("unsupported schemaVersion %d", m.SchemaVersion)
	}
	if m.MediaType != "" && m.MediaType != MediaTypeImageManifest {
		return fmt.Errorf("unsupported mediaType %q", m.MediaType)
	}
	if err := validateDescriptor(m.Config, "config"); err != nil {
		return err
	}
	if len(m.Layers) == 0 {
		return fmt.Errorf("layers is required")
	}
	for i, layer := range m.Layers {
		if err := validateDescriptor(layer, fmt.Sprintf("layers[%d]", i)); err != nil {
			return err
		}
	}
	return nil
}

func validateDescriptor(d Descriptor, field string) error {
	if d.MediaType == "" {
		return fmt.Errorf("%s.mediaType is required", field)
	}
	if !strings.HasPrefix(d.Digest, "sha256:") {
		return fmt.Errorf("%s.digest must start with sha256", field)
	}
	if d.Size <= 0 {
		return fmt.Errorf("%s.size must be positive", field)
	}
	return nil
}
