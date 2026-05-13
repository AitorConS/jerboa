package image

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"
)

const SchemaVersion = 1

// Manifest describes a unikernel disk image.
type Manifest struct {
	// SchemaVersion must equal SchemaVersion (1).
	SchemaVersion int `json:"schemaVersion"`
	// Name is the image name (e.g. "hello").
	Name string `json:"name"`
	// Tag is the image tag (e.g. "latest").
	Tag string `json:"tag"`
	// Created is the build timestamp.
	Created time.Time `json:"created"`
	// Config holds default VM parameters.
	Config Config `json:"config"`
	// DiskDigest is the sha256 digest of the raw disk image ("sha256:<hex>").
	DiskDigest string `json:"diskDigest"`
	// DiskSize is the byte size of the raw disk image.
	DiskSize int64 `json:"diskSize"`
}

// Config holds default VM launch parameters for an image.
type Config struct {
	// Memory is the default QEMU memory string (e.g. "256M").
	Memory string `json:"memory"`
	// CPUs is the default number of virtual CPUs.
	CPUs int `json:"cpus"`
	// Env is the list of environment variables passed to the application.
	Env []string `json:"env,omitempty"`
}

// Ref returns the canonical name:tag reference string.
func (m Manifest) Ref() string {
	return m.Name + ":" + m.Tag
}

// Parse decodes and validates manifest JSON from data.
func Parse(data []byte) (Manifest, error) {
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return Manifest{}, fmt.Errorf("parse manifest: %w", err)
	}
	if err := validate(m); err != nil {
		return Manifest{}, fmt.Errorf("parse manifest: %w", err)
	}
	return m, nil
}

// Marshal serialises m to JSON.
func Marshal(m Manifest) ([]byte, error) {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal manifest: %w", err)
	}
	return data, nil
}

// DigestSHA256 returns the sha256 digest string for data in "sha256:<hex>" form.
func DigestSHA256(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func validate(m Manifest) error {
	if m.SchemaVersion != SchemaVersion {
		return fmt.Errorf("unsupported schemaVersion %d (want %d)", m.SchemaVersion, SchemaVersion)
	}
	if m.Name == "" {
		return fmt.Errorf("name is required")
	}
	if m.Tag == "" {
		return fmt.Errorf("tag is required")
	}
	if m.DiskDigest == "" {
		return fmt.Errorf("diskDigest is required")
	}
	if len(m.DiskDigest) < 8 || m.DiskDigest[:7] != "sha256:" {
		return fmt.Errorf("diskDigest must start with sha256")
	}
	if m.DiskSize <= 0 {
		return fmt.Errorf("diskSize must be positive")
	}
	return nil
}
