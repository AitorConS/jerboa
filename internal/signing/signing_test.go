package signing

import (
	"crypto/ed25519"
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

func TestStoreSignAndVerifyDigest(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir)
	require.NoError(t, err)

	imageDir := filepath.Join(dir, "images", "abc123def456")
	require.NoError(t, os.MkdirAll(imageDir, 0o755))

	sig, err := s.SignDigest("sha256:abc123def4567890abcdef1234567890abcdef1234567890abcdef1234567890")
	require.NoError(t, err)
	require.Equal(t, "sha256:abc123def4567890abcdef1234567890abcdef1234567890abcdef1234567890", sig.Digest)

	verified, err := s.VerifyDigest("sha256:abc123def4567890abcdef1234567890abcdef1234567890abcdef1234567890")
	require.NoError(t, err)
	require.NotNil(t, verified)
	require.Equal(t, sig.Digest, verified.Digest)
	require.Equal(t, sig.KeyID, verified.KeyID)
}

func TestStoreVerifyDigestNoSignature(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir)
	require.NoError(t, err)

	imageDir := filepath.Join(dir, "images", "nosig")
	require.NoError(t, os.MkdirAll(imageDir, 0o755))

	sig, err := s.VerifyDigest("sha256:abc123def4567890abcdef1234567890abcdef1234567890abcdef1234567890")
	require.NoError(t, err)
	require.Nil(t, sig)
}

func TestStoreSignDigestAutoGenerateKey(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir)
	require.NoError(t, err)

	require.False(t, s.HasKeyPair())

	imageDir := filepath.Join(dir, "images", "autodigest")
	require.NoError(t, os.MkdirAll(imageDir, 0o755))

	_, err = s.SignDigest("sha256:autodigest1234567890abcdef1234567890abcdef1234567890abcdef12")
	require.NoError(t, err)
	require.True(t, s.HasKeyPair())
}
