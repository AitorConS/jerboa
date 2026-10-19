// Package credentials keeps the CLI's registry credentials: refresh tokens
// from jerboa login, or personal access tokens, per registry host. Only the
// CLI reads them; the daemon only ever receives short-lived push tokens.
package credentials

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// EnvToken overrides the stored credential, for CI.
const EnvToken = "JERBOA_REGISTRY_TOKEN" //nolint:gosec // the name of an environment variable, not a credential

// Entry is the credential for one registry.
type Entry struct {
	Token string `json:"token"`
	Login string `json:"login,omitempty"`
}

// File is the credentials file.
type File struct {
	Registries map[string]Entry `json:"registries"`
}

// Path is ~/.jerboa/credentials.json.
func Path() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("credentials: %w", err)
	}
	return filepath.Join(home, ".jerboa", "credentials.json"), nil
}

// Load reads the credentials file; a missing file is empty.
func Load() (File, error) {
	f := File{Registries: map[string]Entry{}}
	p, err := Path()
	if err != nil {
		return f, err
	}
	data, err := os.ReadFile(p) //nolint:gosec // the user's own credentials file
	if errors.Is(err, fs.ErrNotExist) {
		return f, nil
	}
	if err != nil {
		return f, fmt.Errorf("credentials: %w", err)
	}
	if err := json.Unmarshal(data, &f); err != nil {
		return f, fmt.Errorf("credentials: %s: %w", p, err)
	}
	if f.Registries == nil {
		f.Registries = map[string]Entry{}
	}
	return f, nil
}

// Save writes the file readable by its owner only.
func (f File) Save() error {
	p, err := Path()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return fmt.Errorf("credentials: %w", err)
	}
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return fmt.Errorf("credentials: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), ".credentials-*")
	if err != nil {
		return fmt.Errorf("credentials: %w", err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("credentials: %w", err)
	}
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("credentials: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("credentials: %w", err)
	}
	if err := os.Rename(tmp.Name(), p); err != nil {
		return fmt.Errorf("credentials: %w", err)
	}
	return nil
}

// For returns the credential to use with registry: JERBOA_REGISTRY_TOKEN
// when set, otherwise the stored one.
func For(registry string) (string, error) {
	if t := os.Getenv(EnvToken); t != "" {
		return t, nil
	}
	f, err := Load()
	if err != nil {
		return "", err
	}
	if e, ok := f.Registries[registry]; ok && e.Token != "" {
		return e.Token, nil
	}
	return "", fmt.Errorf("not logged in to %s; run jerboa login %s, or set %s", registry, registry, EnvToken)
}
