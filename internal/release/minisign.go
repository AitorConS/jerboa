package release

import (
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
)

// minisign signature algorithms. We sign only the (small) channel manifest, so
// the legacy mode — Ed25519 over the raw message — is all we need and all we
// emit in CI. Prehashed signatures (BLAKE2b-512 of the message) are detected and
// rejected with an actionable error rather than silently accepted.
const (
	algLegacy    = "Ed"
	algPrehashed = "ED"
)

const (
	pubKeyLen = 42 // 2 (alg) + 8 (key id) + 32 (ed25519 public key)
	sigLen    = 74 // 2 (alg) + 8 (key id) + 64 (ed25519 signature)
)

// PublicKey is a parsed minisign public key.
type PublicKey struct {
	keyID [8]byte
	key   ed25519.PublicKey
}

// ParsePublicKey parses a minisign public key. It accepts either the full
// two-line .pub file (untrusted-comment line + base64 payload) or just the
// base64 payload line.
func ParsePublicKey(s string) (PublicKey, error) {
	line := lastNonEmptyLine(s)
	if line == "" {
		return PublicKey{}, errors.New("release: empty public key")
	}
	raw, err := base64.StdEncoding.DecodeString(line)
	if err != nil {
		return PublicKey{}, fmt.Errorf("release: decode public key: %w", err)
	}
	if len(raw) != pubKeyLen {
		return PublicKey{}, fmt.Errorf("release: public key length %d, want %d", len(raw), pubKeyLen)
	}
	if string(raw[0:2]) != algLegacy {
		return PublicKey{}, fmt.Errorf("release: unsupported public key algorithm %q", raw[0:2])
	}
	var pk PublicKey
	copy(pk.keyID[:], raw[2:10])
	pk.key = ed25519.PublicKey(append([]byte(nil), raw[10:42]...))
	return pk, nil
}

// Verify checks a minisign signature (.minisig file contents) over message.
// It validates both the message signature and the global signature that binds
// the trusted comment, so neither the payload nor the trusted comment can be
// swapped independently.
func (pk PublicKey) Verify(message, sigFile []byte) error {
	_, err := pk.VerifyComment(message, sigFile)
	return err
}

// VerifyComment is Verify that also returns the authenticated trusted
// comment. The same key signs several kinds of document (release manifests,
// toolsets, the package index), distinguished by that comment, so a caller
// that accepts only one kind checks it.
func (pk PublicKey) VerifyComment(message, sigFile []byte) (string, error) {
	if pk.key == nil {
		return "", errors.New("release: no public key configured")
	}
	lines := nonEmptyLines(string(sigFile))
	if len(lines) < 4 {
		return "", errors.New("release: malformed signature file (want 4 lines)")
	}
	// Line layout: [0] untrusted comment, [1] signature, [2] trusted comment,
	// [3] global signature.
	sigBin, err := base64.StdEncoding.DecodeString(lines[1])
	if err != nil {
		return "", fmt.Errorf("release: decode signature: %w", err)
	}
	if len(sigBin) != sigLen {
		return "", fmt.Errorf("release: signature length %d, want %d", len(sigBin), sigLen)
	}
	alg := string(sigBin[0:2])
	var keyID [8]byte
	copy(keyID[:], sigBin[2:10])
	if keyID != pk.keyID {
		return "", errors.New("release: signature key id does not match public key")
	}
	sig := sigBin[10:74]

	switch alg {
	case algLegacy:
		if !ed25519.Verify(pk.key, message, sig) {
			return "", errors.New("release: signature verification failed")
		}
	case algPrehashed:
		return "", errors.New("release: prehashed signatures are not supported — " +
			"sign the manifest with legacy minisign (no -H flag)")
	default:
		return "", fmt.Errorf("release: unsupported signature algorithm %q", alg)
	}

	// Global signature binds sig || trusted-comment text.
	const tcPrefix = "trusted comment: "
	if !strings.HasPrefix(lines[2], tcPrefix) {
		return "", errors.New("release: missing trusted comment line")
	}
	tc := strings.TrimPrefix(lines[2], tcPrefix)
	global, err := base64.StdEncoding.DecodeString(lines[3])
	if err != nil {
		return "", fmt.Errorf("release: decode global signature: %w", err)
	}
	if len(global) != ed25519.SignatureSize {
		return "", fmt.Errorf("release: global signature length %d, want %d", len(global), ed25519.SignatureSize)
	}
	globalMsg := make([]byte, 0, len(sig)+len(tc))
	globalMsg = append(globalMsg, sig...)
	globalMsg = append(globalMsg, tc...)
	if !ed25519.Verify(pk.key, globalMsg, global) {
		return "", errors.New("release: global signature verification failed")
	}
	return tc, nil
}

// nonEmptyLines splits s on newlines (CRLF-tolerant) and drops blank lines.
func nonEmptyLines(s string) []string {
	var out []string
	for _, l := range strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n") {
		if strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	return out
}

// lastNonEmptyLine returns the last non-blank line of s (the base64 payload of a
// minisign key/signature file, after any comment lines).
func lastNonEmptyLine(s string) string {
	lines := nonEmptyLines(s)
	if len(lines) == 0 {
		return ""
	}
	return strings.TrimSpace(lines[len(lines)-1])
}
