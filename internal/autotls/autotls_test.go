package autotls

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestEnsureCertGeneratesNew(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "cert.pem")
	keyPath := filepath.Join(dir, "key.pem")

	cert, err := EnsureCert(certPath, keyPath)
	require.NoError(t, err)
	require.NotEmpty(t, cert.Certificate)

	_, err = os.Stat(certPath)
	require.NoError(t, err)
	_, err = os.Stat(keyPath)
	require.NoError(t, err)
}

func TestEnsureCertLoadsExisting(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "cert.pem")
	keyPath := filepath.Join(dir, "key.pem")

	_, err := EnsureCert(certPath, keyPath)
	require.NoError(t, err)

	_, err = EnsureCert(certPath, keyPath)
	require.NoError(t, err)
}

func TestEnsureCertReusesFiles(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "cert.pem")
	keyPath := filepath.Join(dir, "key.pem")

	_, err := EnsureCert(certPath, keyPath)
	require.NoError(t, err)

	certData, err := os.ReadFile(certPath)
	require.NoError(t, err)

	_, err = EnsureCert(certPath, keyPath)
	require.NoError(t, err)

	certData2, err := os.ReadFile(certPath)
	require.NoError(t, err)
	require.Equal(t, certData, certData2)
}

func TestEnsureCertDirectoryCreation(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "nested", "dir", "cert.pem")
	keyPath := filepath.Join(dir, "nested", "dir", "key.pem")

	_, err := EnsureCert(certPath, keyPath)
	require.NoError(t, err)

	_, err = os.Stat(certPath)
	require.NoError(t, err)
}

func TestDefaultCertDir(t *testing.T) {
	dir := DefaultCertDir()
	require.Contains(t, dir, ".jerboa")
	require.Contains(t, dir, "registry")
	require.Contains(t, dir, "tls")
}
