package signing

import (
	"crypto/ed25519"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestGenerateKeyPair(t *testing.T) {
	kp, err := GenerateKeyPair()
	require.NoError(t, err)
	require.NotNil(t, kp.PrivateKey)
	require.NotNil(t, kp.PublicKey)
	require.Len(t, kp.PublicKey, ed25519.PublicKeySize)
	require.NotEmpty(t, kp.KeyID)
}

func TestSignAndVerify(t *testing.T) {
	kp, err := GenerateKeyPair()
	require.NoError(t, err)

	digest := "sha256:abcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890"
	sig, err := Sign(kp, digest)
	require.NoError(t, err)
	require.Equal(t, signatureType, sig.Type)
	require.Equal(t, kp.KeyID, sig.KeyID)
	require.Equal(t, digest, sig.Digest)
	require.NotEmpty(t, sig.Signature)

	err = Verify(kp.PublicKey, sig)
	require.NoError(t, err)
}

func TestVerifyWrongKey(t *testing.T) {
	kp, err := GenerateKeyPair()
	require.NoError(t, err)

	otherKP, err := GenerateKeyPair()
	require.NoError(t, err)

	sig, err := Sign(kp, "sha256:abc")
	require.NoError(t, err)

	err = Verify(otherKP.PublicKey, sig)
	require.Error(t, err)
}

func TestVerifyTamperedDigest(t *testing.T) {
	kp, err := GenerateKeyPair()
	require.NoError(t, err)

	sig, err := Sign(kp, "sha256:original")
	require.NoError(t, err)

	tampered := &Signature{
		Type:      sig.Type,
		KeyID:     sig.KeyID,
		Digest:    "sha256:tampered",
		Signature: sig.Signature,
	}
	err = Verify(kp.PublicKey, tampered)
	require.Error(t, err)
}

func TestVerifyUnsupportedType(t *testing.T) {
	kp, err := GenerateKeyPair()
	require.NoError(t, err)

	sig := &Signature{
		Type:      "rsa",
		KeyID:     kp.KeyID,
		Digest:    "sha256:abc",
		Signature: "deadbeef",
	}
	err = Verify(kp.PublicKey, sig)
	require.Error(t, err)
}

func TestParseVerifyPolicy(t *testing.T) {
	tests := []struct {
		input    string
		expected VerifyPolicy
		wantErr  bool
	}{
		{"off", VerifyOff, false},
		{"", VerifyOff, false},
		{"warn", VerifyWarn, false},
		{"enforce", VerifyEnforce, false},
		{"WARN", VerifyWarn, false},
		{"Enforce", VerifyEnforce, false},
		{"invalid", VerifyOff, true},
	}
	for _, tt := range tests {
		p, err := ParseVerifyPolicy(tt.input)
		if tt.wantErr {
			require.Error(t, err)
		} else {
			require.NoError(t, err)
			require.Equal(t, tt.expected, p)
		}
	}
}

func TestStoreGenerateAndSave(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir)
	require.NoError(t, err)

	kp, err := s.GenerateAndSave()
	require.NoError(t, err)
	require.NotNil(t, kp)

	require.True(t, s.HasKeyPair())
}

func TestStoreLoadKeyPair(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir)
	require.NoError(t, err)

	kp, err := s.GenerateAndSave()
	require.NoError(t, err)

	loaded, err := s.LoadKeyPair()
	require.NoError(t, err)
	require.Equal(t, kp.KeyID, loaded.KeyID)
	require.Equal(t, kp.PublicKey, loaded.PublicKey)
	require.Equal(t, kp.PrivateKey, loaded.PrivateKey)
}

func TestStoreLoadKeyPairNotFound(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir)
	require.NoError(t, err)

	_, err = s.LoadKeyPair()
	require.Error(t, err)
}

func TestStoreSignAndVerifyManifest(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir)
	require.NoError(t, err)

	imageDir := filepath.Join(dir, "images", "abc123def456")
	require.NoError(t, os.MkdirAll(imageDir, 0o755))

	sig, err := s.SignManifest("sha256:abc123def4567890abcdef1234567890abcdef1234567890abcdef1234567890", imageDir)
	require.NoError(t, err)
	require.Equal(t, "sha256:abc123def4567890abcdef1234567890abcdef1234567890abcdef1234567890", sig.Digest)

	verified, err := s.VerifyManifest(imageDir)
	require.NoError(t, err)
	require.NotNil(t, verified)
	require.Equal(t, sig.Digest, verified.Digest)
	require.Equal(t, sig.KeyID, verified.KeyID)
}

func TestStoreVerifyManifestNoSignature(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir)
	require.NoError(t, err)

	imageDir := filepath.Join(dir, "images", "nosig")
	require.NoError(t, os.MkdirAll(imageDir, 0o755))

	sig, err := s.VerifyManifest(imageDir)
	require.NoError(t, err)
	require.Nil(t, sig)
}

func TestStoreSignManifestAutoGenerateKey(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir)
	require.NoError(t, err)

	require.False(t, s.HasKeyPair())

	imageDir := filepath.Join(dir, "images", "autodigest")
	require.NoError(t, os.MkdirAll(imageDir, 0o755))

	_, err = s.SignManifest("sha256:autodigest1234567890abcdef1234567890abcdef1234567890abcdef12", imageDir)
	require.NoError(t, err)
	require.True(t, s.HasKeyPair())
}

func TestPublicKeyPEM(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir)
	require.NoError(t, err)

	_, err = s.GenerateAndSave()
	require.NoError(t, err)

	pemBytes, err := s.PublicKeyPEM()
	require.NoError(t, err)
	require.Contains(t, string(pemBytes), "ED25519 PUBLIC KEY")

	block, _ := pem.Decode(pemBytes)
	require.NotNil(t, block)
	require.Equal(t, "ED25519 PUBLIC KEY", block.Type)
	require.Len(t, block.Bytes, ed25519.PublicKeySize)
}

func TestImportPublicKey(t *testing.T) {
	dir := t.TempDir()
	s1, err := NewStore(dir)
	require.NoError(t, err)

	kp, err := s1.GenerateAndSave()
	require.NoError(t, err)

	pemBytes, err := s1.PublicKeyPEM()
	require.NoError(t, err)

	dir2 := t.TempDir()
	s2, err := NewStore(dir2)
	require.NoError(t, err)

	err = s2.ImportPublicKey(pemBytes)
	require.NoError(t, err)

	verificationPath := filepath.Join(dir2, keyDirName, kp.KeyID+".pub")
	_, err = os.Stat(verificationPath)
	require.NoError(t, err)
}

func TestImportPublicKeyInvalidPEM(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir)
	require.NoError(t, err)

	err = s.ImportPublicKey([]byte("not a pem"))
	require.Error(t, err)
}
