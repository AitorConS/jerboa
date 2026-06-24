package signing

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	keyDirName    = "keys"
	privKeyFile   = "signing.key"
	pubKeyFile    = "signing.pub"
	sigExt        = ".sig"
	signatureType = "ed25519"
)

// Signature represents an Ed25519 signature over a manifest digest.
type Signature struct {
	Type      string `json:"type"`
	KeyID     string `json:"keyId"`
	Digest    string `json:"digest"`
	Signature string `json:"signature"`
}

// KeyPair holds an Ed25519 key pair and its key ID.
type KeyPair struct {
	PrivateKey ed25519.PrivateKey
	PublicKey  ed25519.PublicKey
	KeyID      string
}

// GenerateKeyPair creates a new Ed25519 key pair.
func GenerateKeyPair() (*KeyPair, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate ed25519 key pair: %w", err)
	}
	kp := &KeyPair{
		PrivateKey: priv,
		PublicKey:  pub,
		KeyID:      keyIDFromPublic(pub),
	}
	return kp, nil
}

// Sign signs the given digest with the private key and returns a Signature.
func Sign(kp *KeyPair, digest string) (*Signature, error) {
	if kp.PrivateKey == nil {
		return nil, fmt.Errorf("sign: private key is nil")
	}
	sig := ed25519.Sign(kp.PrivateKey, []byte(digest))
	return &Signature{
		Type:      signatureType,
		KeyID:     kp.KeyID,
		Digest:    digest,
		Signature: fmt.Sprintf("%x", sig),
	}, nil
}

// Verify checks that sig is a valid Ed25519 signature over digest by publicKey.
func Verify(publicKey ed25519.PublicKey, sig *Signature) error {
	if sig.Type != signatureType {
		return fmt.Errorf("verify: unsupported signature type %q", sig.Type)
	}
	if sig.KeyID != keyIDFromPublic(publicKey) {
		return fmt.Errorf("verify: key ID mismatch: expected %s, got %s", keyIDFromPublic(publicKey), sig.KeyID)
	}
	sigBytes, err := hexDecode(sig.Signature)
	if err != nil {
		return fmt.Errorf("verify: decode signature: %w", err)
	}
	if !ed25519.Verify(publicKey, []byte(sig.Digest), sigBytes) {
		return fmt.Errorf("verify: signature invalid")
	}
	return nil
}

// VerifyPolicy controls signature verification behavior.
type VerifyPolicy int

const (
	// VerifyOff skips signature verification.
	VerifyOff VerifyPolicy = iota
	// VerifyWarn logs a warning if signature is missing or invalid but does not fail.
	VerifyWarn
	// VerifyEnforce requires a valid signature; fails if missing or invalid.
	VerifyEnforce
)

// ParseVerifyPolicy parses a verification policy string.
func ParseVerifyPolicy(s string) (VerifyPolicy, error) {
	switch strings.ToLower(s) {
	case "off", "":
		return VerifyOff, nil
	case "warn":
		return VerifyWarn, nil
	case "enforce":
		return VerifyEnforce, nil
	default:
		return VerifyOff, fmt.Errorf("invalid verify policy %q: use off, warn, or enforce", s)
	}
}

// Store persists Ed25519 key pairs and signatures on disk.
type Store struct {
	root string
}

// NewStore creates a Store rooted at root (typically ~/.jerboa).
func NewStore(root string) (*Store, error) {
	if err := os.MkdirAll(filepath.Join(root, keyDirName), 0o700); err != nil {
		return nil, fmt.Errorf("signing store mkdir: %w", err)
	}
	return &Store{root: root}, nil
}

// DefaultStorePath returns the default store path (~/.jerboa).
func DefaultStorePath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ".jerboa"
	}
	return home + "/.jerboa"
}

// GenerateAndSave creates a new key pair and saves it to disk.
func (s *Store) GenerateAndSave() (*KeyPair, error) {
	kp, err := GenerateKeyPair()
	if err != nil {
		return nil, err
	}
	if err := s.SaveKeyPair(kp); err != nil {
		return nil, err
	}
	return kp, nil
}

// SaveKeyPair persists a key pair to disk.
func (s *Store) SaveKeyPair(kp *KeyPair) error {
	dir := filepath.Join(s.root, keyDirName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("save key pair mkdir: %w", err)
	}
	privBlock := &pem.Block{Type: "ED25519 PRIVATE KEY", Bytes: kp.PrivateKey}
	if err := os.WriteFile(filepath.Join(dir, privKeyFile), pem.EncodeToMemory(privBlock), 0o600); err != nil {
		return fmt.Errorf("save private key: %w", err)
	}
	pubBlock := &pem.Block{Type: "ED25519 PUBLIC KEY", Bytes: kp.PublicKey}
	if err := os.WriteFile(filepath.Join(dir, pubKeyFile), pem.EncodeToMemory(pubBlock), 0o644); err != nil {
		return fmt.Errorf("save public key: %w", err)
	}
	return nil
}

// LoadKeyPair loads the key pair from disk. Returns error if no key pair exists.
func (s *Store) LoadKeyPair() (*KeyPair, error) {
	dir := filepath.Join(s.root, keyDirName)
	privData, err := os.ReadFile(filepath.Join(dir, privKeyFile))
	if err != nil {
		return nil, fmt.Errorf("load private key: %w", err)
	}
	privBlock, _ := pem.Decode(privData)
	if privBlock == nil || privBlock.Type != "ED25519 PRIVATE KEY" {
		return nil, fmt.Errorf("load private key: invalid PEM block")
	}
	privKey := ed25519.PrivateKey(privBlock.Bytes)

	pubData, err := os.ReadFile(filepath.Join(dir, pubKeyFile))
	if err != nil {
		return nil, fmt.Errorf("load public key: %w", err)
	}
	pubBlock, _ := pem.Decode(pubData)
	if pubBlock == nil || pubBlock.Type != "ED25519 PUBLIC KEY" {
		return nil, fmt.Errorf("load public key: invalid PEM block")
	}
	pubKey := ed25519.PublicKey(pubBlock.Bytes)

	return &KeyPair{
		PrivateKey: privKey,
		PublicKey:  pubKey,
		KeyID:      keyIDFromPublic(pubKey),
	}, nil
}

// HasKeyPair reports whether a key pair exists on disk.
func (s *Store) HasKeyPair() bool {
	_, err := os.Stat(filepath.Join(s.root, keyDirName, privKeyFile))
	return err == nil
}

// SaveSignature writes a signature file next to the image manifest in the image store.
// The signature file is stored at <imageDir>/manifest.json.sig.
func (s *Store) SaveSignature(imageDir string, sig *Signature) error {
	data, err := json.MarshalIndent(sig, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal signature: %w", err)
	}
	path := filepath.Join(imageDir, "manifest.json"+sigExt)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("write signature: %w", err)
	}
	return nil
}

// LoadSignature reads a signature file from the image store directory.
func (s *Store) LoadSignature(imageDir string) (*Signature, error) {
	path := filepath.Join(imageDir, "manifest.json"+sigExt)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read signature: %w", err)
	}
	var sig Signature
	if err := json.Unmarshal(data, &sig); err != nil {
		return nil, fmt.Errorf("parse signature: %w", err)
	}
	return &sig, nil
}

const sigDirName = "signatures"

// signaturePath returns the on-disk path for a content digest's signature,
// keyed by the digest itself rather than a store directory. This lets the
// client sign and verify images held by a remote daemon.
func (s *Store) signaturePath(digest string) string {
	name := strings.ReplaceAll(digest, ":", "-") + sigExt
	return filepath.Join(s.root, sigDirName, name)
}

// loadOrGenerateKey returns the stored key pair, generating and saving one if
// none exists yet.
func (s *Store) loadOrGenerateKey() (*KeyPair, error) {
	if s.HasKeyPair() {
		return s.LoadKeyPair()
	}
	return s.GenerateAndSave()
}

// SignDigest signs a content digest (e.g. an image's disk digest) with the
// stored key pair and saves the signature keyed by that digest.
func (s *Store) SignDigest(digest string) (*Signature, error) {
	kp, err := s.loadOrGenerateKey()
	if err != nil {
		return nil, fmt.Errorf("sign digest: %w", err)
	}
	sig, err := Sign(kp, digest)
	if err != nil {
		return nil, fmt.Errorf("sign digest: %w", err)
	}
	path := s.signaturePath(digest)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("sign digest: %w", err)
	}
	data, err := json.MarshalIndent(sig, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("sign digest: %w", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return nil, fmt.Errorf("sign digest: write: %w", err)
	}
	return sig, nil
}

// VerifyDigest verifies the signature stored for a content digest. It returns
// (nil, nil) when no signature exists, the signature on success, and an error
// when a signature exists but is invalid.
func (s *Store) VerifyDigest(digest string) (*Signature, error) {
	data, err := os.ReadFile(s.signaturePath(digest))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("verify digest: read: %w", err)
	}
	var sig Signature
	if err := json.Unmarshal(data, &sig); err != nil {
		return nil, fmt.Errorf("verify digest: parse: %w", err)
	}
	if sig.Digest != digest {
		return &sig, fmt.Errorf("verify digest: signature is for %s, not %s", sig.Digest, digest)
	}
	kp, err := s.LoadKeyPair()
	if err != nil {
		return nil, fmt.Errorf("verify digest: load key pair: %w", err)
	}
	if err := Verify(kp.PublicKey, &sig); err != nil {
		return &sig, fmt.Errorf("verify digest: %w", err)
	}
	return &sig, nil
}

// SignManifest signs the manifest digest using the stored key pair and saves the signature.
// imageDir is the directory containing the manifest (typically <store>/images/<hex-digest>).
// If no key pair exists, it generates one first.
func (s *Store) SignManifest(digest, imageDir string) (*Signature, error) {
	var kp *KeyPair
	var err error
	if s.HasKeyPair() {
		kp, err = s.LoadKeyPair()
	} else {
		kp, err = s.GenerateAndSave()
	}
	if err != nil {
		return nil, fmt.Errorf("sign manifest: %w", err)
	}
	sig, err := Sign(kp, digest)
	if err != nil {
		return nil, fmt.Errorf("sign manifest: %w", err)
	}
	if err := s.SaveSignature(imageDir, sig); err != nil {
		return nil, fmt.Errorf("sign manifest: %w", err)
	}
	return sig, nil
}

// VerifyManifest verifies the signature for a manifest in the image store.
// Returns nil if signature is valid, an error if invalid, and (nil, nil) if
// no signature exists.
func (s *Store) VerifyManifest(imageDir string) (*Signature, error) {
	sig, err := s.LoadSignature(imageDir)
	if err != nil {
		return nil, err
	}
	if sig == nil {
		return nil, nil
	}
	kp, err := s.LoadKeyPair()
	if err != nil {
		return nil, fmt.Errorf("verify: load key pair: %w", err)
	}
	if err := Verify(kp.PublicKey, sig); err != nil {
		return sig, fmt.Errorf("verify: %w", err)
	}
	return sig, nil
}

// PublicKeyPEM exports the public key as PEM-encoded bytes.
func (s *Store) PublicKeyPEM() ([]byte, error) {
	kp, err := s.LoadKeyPair()
	if err != nil {
		return nil, err
	}
	block := &pem.Block{Type: "ED25519 PUBLIC KEY", Bytes: kp.PublicKey}
	return pem.EncodeToMemory(block), nil
}

// ImportPublicKey imports a PEM-encoded public key for verification.
// The key is stored as a separate verification key file.
func (s *Store) ImportPublicKey(pemData []byte) error {
	block, _ := pem.Decode(pemData)
	if block == nil || block.Type != "ED25519 PUBLIC KEY" {
		return fmt.Errorf("import: invalid PEM block, expected ED25519 PUBLIC KEY")
	}
	pub := ed25519.PublicKey(block.Bytes)
	dir := filepath.Join(s.root, keyDirName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("import mkdir: %w", err)
	}
	verificationKeyID := keyIDFromPublic(pub)
	verificationPath := filepath.Join(dir, verificationKeyID+".pub")
	if err := os.WriteFile(verificationPath, pemData, 0o644); err != nil {
		return fmt.Errorf("import: write key: %w", err)
	}
	return nil
}

func keyIDFromPublic(pub ed25519.PublicKey) string {
	return fmt.Sprintf("%x", pub)[:16]
}

func hexDecode(s string) ([]byte, error) {
	if len(s)%2 != 0 {
		return nil, fmt.Errorf("odd length hex string")
	}
	b := make([]byte, len(s)/2)
	for i := 0; i < len(s); i += 2 {
		hi := hexVal(s[i])
		lo := hexVal(s[i+1])
		if hi < 0 || lo < 0 {
			return nil, fmt.Errorf("invalid hex char at position %d", i)
		}
		b[i/2] = byte(hi<<4 | lo)
	}
	return b, nil
}

func hexVal(c byte) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'a' && c <= 'f':
		return int(c - 'a' + 10)
	case c >= 'A' && c <= 'F':
		return int(c - 'A' + 10)
	default:
		return -1
	}
}
