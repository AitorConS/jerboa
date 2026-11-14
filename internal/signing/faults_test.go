package signing

import (
	"crypto/ed25519"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/AitorConS/jerboa/internal/durable"
	"github.com/AitorConS/jerboa/internal/durable/durabletest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests are about the signing store when its files are written during
// a failure or found damaged later: the key pair must never be left in a
// state that blocks signing for good, and a signature that cannot be read
// whole, or that is not the signature of the digest asked about, must never
// count as a valid one.

const faultDigest = "sha256:1111111111111111111111111111111111111111111111111111111111111111"

func newFaultStore(t *testing.T) (*Store, string) {
	t.Helper()
	root := t.TempDir()
	s, err := NewStore(root)
	require.NoError(t, err)
	return s, root
}

func keyPath(root, name string) string { return filepath.Join(root, keyDirName, name) }

// Generating the first key pair writes two files. Whichever write fails, and
// however, the store must still be able to sign afterwards: with the pair it
// managed to save or with a fresh one, never stuck with half a pair.
func TestFaults_SaveKeyPairFailsAtEveryStep(t *testing.T) {
	for nth := range 2 {
		for _, f := range durabletest.Failures() {
			t.Run(fmt.Sprintf("write%d/%s", nth, f.Name), func(t *testing.T) {
				s, root := newFaultStore(t)
				inj := durabletest.Inject(t, f, nth, nil)
				kp, err := s.GenerateAndSave()
				require.True(t, inj.Fired(), "the failure was never reached")
				durable.SetFault(nil)

				if err == nil {
					loaded, lerr := s.LoadKeyPair()
					require.NoError(t, lerr, "the save reported success for a pair that does not load")
					assert.Equal(t, kp.KeyID, loaded.KeyID)
				}
				// A store that says it has a pair must be able to load it.
				if s.HasKeyPair() {
					_, lerr := s.LoadKeyPair()
					require.NoError(t, lerr, "half a key pair was left where a whole one is expected")
				}

				// Signing works again, and what it signs verifies.
				_, err = s.SignDigest(faultDigest)
				require.NoError(t, err, "a failed key save left the store unable to sign")
				sig, err := s.VerifyDigest(faultDigest)
				require.NoError(t, err)
				require.NotNil(t, sig)

				if !f.Crash() {
					entries, err := os.ReadDir(filepath.Join(root, keyDirName))
					require.NoError(t, err)
					for _, e := range entries {
						assert.False(t, durable.IsTemp(e.Name()), "a failed save left a copy of key material: %s", e.Name())
					}
				}
			})
		}
	}
}

// Replacing a key pair that exists (key rotation) must leave a pair that
// loads: the old one or the new one, never the public half of one with the
// private half of the other reported as usable.
func TestFaults_ReplacingAKeyPairFailsAtEveryStep(t *testing.T) {
	for nth := range 2 {
		for _, f := range durabletest.Failures() {
			t.Run(fmt.Sprintf("write%d/%s", nth, f.Name), func(t *testing.T) {
				s, _ := newFaultStore(t)
				old, err := s.GenerateAndSave()
				require.NoError(t, err)
				_, err = s.SignDigest(faultDigest)
				require.NoError(t, err)

				inj := durabletest.Inject(t, f, nth, nil)
				next, err := s.GenerateAndSave()
				require.True(t, inj.Fired())
				durable.SetFault(nil)

				loaded, lerr := s.LoadKeyPair()
				if lerr != nil {
					// The two files are not replaced together, so a failure
					// between them is detected as a mismatched pair rather
					// than used.
					require.Error(t, err, "the save reported success")
					require.ErrorContains(t, lerr, "does not belong")
					_, verr := s.VerifyDigest(faultDigest)
					require.Error(t, verr, "a signature verified against a broken key pair")
					return
				}
				if err == nil {
					assert.Equal(t, next.KeyID, loaded.KeyID)
				}
				if loaded.KeyID == old.KeyID {
					_, verr := s.VerifyDigest(faultDigest)
					require.NoError(t, verr, "the old key survived and its signature no longer verifies")
				}
			})
		}
	}
}

func TestFaults_KeyFilesKeepTheirModes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX modes")
	}
	s, root := newFaultStore(t)
	for range 2 {
		_, err := s.GenerateAndSave()
		require.NoError(t, err)
		priv, err := os.Stat(keyPath(root, privKeyFile))
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o600), priv.Mode().Perm(), "the private key is readable by others")
		pub, err := os.Stat(keyPath(root, pubKeyFile))
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o644), pub.Mode().Perm())
		// A private key found world-readable is tightened by the next save.
		require.NoError(t, os.Chmod(keyPath(root, privKeyFile), 0o644))
	}
	dir, err := os.Stat(filepath.Join(root, keyDirName))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o700), dir.Mode().Perm())
}

// Key files as a power cut, a failing disk or a bad restore leave them. None
// of them may load as a key pair, and none may make a signature verify.
func TestFaults_DamagedKeyFilesAreRefused(t *testing.T) {
	otherPub, otherPriv, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	pemOf := func(kind string, b []byte) []byte {
		return pem.EncodeToMemory(&pem.Block{Type: kind, Bytes: b})
	}
	type damage func(t *testing.T, path string, whole []byte)
	write := func(content func(whole []byte) []byte) damage {
		return func(t *testing.T, path string, whole []byte) {
			require.NoError(t, os.WriteFile(path, content(whole), 0o600))
		}
	}
	damages := map[string]damage{
		"missing":   func(t *testing.T, path string, _ []byte) { require.NoError(t, os.Remove(path)) },
		"empty":     write(func([]byte) []byte { return nil }),
		"truncated": write(func(b []byte) []byte { return b[:len(b)/2] }),
		"zeroed":    write(func(b []byte) []byte { return make([]byte, len(b)) }),
		"garbage":   write(func([]byte) []byte { return []byte("not a PEM file\n") }),
		"short key": write(func([]byte) []byte { return pemOf("ED25519 PRIVATE KEY", []byte("short")) }),
		"another kind of key": write(func([]byte) []byte {
			return pemOf("RSA PRIVATE KEY", make([]byte, ed25519.PrivateKeySize))
		}),
		"a directory": func(t *testing.T, path string, _ []byte) {
			require.NoError(t, os.Remove(path))
			require.NoError(t, os.Mkdir(path, 0o700))
		},
	}
	for _, file := range []string{privKeyFile, pubKeyFile} {
		for name, apply := range damages {
			t.Run(file+"/"+name, func(t *testing.T) {
				s, root := newFaultStore(t)
				_, err := s.SignDigest(faultDigest)
				require.NoError(t, err)
				path := keyPath(root, file)
				whole, err := os.ReadFile(path)
				require.NoError(t, err)
				apply(t, path, whole)

				kp, err := s.LoadKeyPair()
				require.Error(t, err)
				assert.Nil(t, kp)
				sig, err := s.VerifyDigest(faultDigest)
				require.Error(t, err, "a signature verified without a usable key")
				assert.Nil(t, sig)
			})
		}
	}

	// Each half replaced by the matching half of another pair: well formed,
	// the right type and size, and not this store's key.
	swaps := map[string][]byte{
		privKeyFile: pemOf("ED25519 PRIVATE KEY", otherPriv),
		pubKeyFile:  pemOf("ED25519 PUBLIC KEY", otherPub),
	}
	for file, content := range swaps {
		t.Run(file+"/of another pair", func(t *testing.T) {
			s, root := newFaultStore(t)
			_, err := s.SignDigest(faultDigest)
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(keyPath(root, file), content, 0o600))
			_, err = s.LoadKeyPair()
			require.ErrorContains(t, err, "does not belong")
			_, err = s.VerifyDigest(faultDigest)
			require.Error(t, err)
		})
	}
}

func TestFaults_UnreadablePrivateKey(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs POSIX permissions and a non-root user")
	}
	s, root := newFaultStore(t)
	_, err := s.SignDigest(faultDigest)
	require.NoError(t, err)
	require.NoError(t, os.Chmod(keyPath(root, privKeyFile), 0o000))
	_, err = s.LoadKeyPair()
	require.Error(t, err)
	// The key exists, so signing must fail rather than quietly generate a
	// new key and orphan every signature made with the old one.
	_, err = s.SignDigest("sha256:" + strings.Repeat("2", 64))
	require.Error(t, err)
	require.NoError(t, os.Chmod(keyPath(root, privKeyFile), 0o600))
	_, err = s.VerifyDigest(faultDigest)
	require.NoError(t, err, "the old signature no longer verifies")
}

// Saving a signature fails at every step: a signature that was there stays
// valid, and no half-written signature file is ever read as one.
func TestFaults_SignDigestFailsAtEveryStep(t *testing.T) {
	for _, f := range durabletest.Failures() {
		t.Run(f.Name, func(t *testing.T) {
			s, _ := newFaultStore(t)
			_, err := s.GenerateAndSave()
			require.NoError(t, err)
			_, err = s.SignDigest(faultDigest)
			require.NoError(t, err)
			fresh := "sha256:" + strings.Repeat("3", 64)

			inj := durabletest.Inject(t, f, 0, nil)
			_, signErr := s.SignDigest(fresh)
			_, resignErr := s.SignDigest(faultDigest)
			require.True(t, inj.Fired())
			durable.SetFault(nil)
			require.NoError(t, resignErr, "only the first write was to fail")

			sig, err := s.VerifyDigest(faultDigest)
			require.NoError(t, err, "a signature saved before the failure was lost")
			require.NotNil(t, sig)

			sig, err = s.VerifyDigest(fresh)
			require.NoError(t, err, "a failed save left a signature file that does not verify")
			if signErr != nil && f.Step != durable.StepSyncDir {
				assert.Nil(t, sig, "a signature appeared although saving it failed")
			}
			_, err = s.SignDigest(fresh)
			require.NoError(t, err)
			sig, err = s.VerifyDigest(fresh)
			require.NoError(t, err)
			require.NotNil(t, sig)
		})
	}
}

// Signature files found damaged, or holding a signature of something else.
// VerifyDigest returns (nil, nil) only for "there is no signature"; every
// case here is a signature that exists and must fail.
func TestFaults_DamagedSignatureFilesAreRefused(t *testing.T) {
	s, root := newFaultStore(t)
	good, err := s.SignDigest(faultDigest)
	require.NoError(t, err)
	otherDigest := "sha256:" + strings.Repeat("4", 64)
	other, err := s.SignDigest(otherDigest)
	require.NoError(t, err)
	stranger, err := GenerateKeyPair()
	require.NoError(t, err)
	foreign, err := Sign(stranger, faultDigest)
	require.NoError(t, err)

	encode := func(sig Signature) []byte {
		data, err := json.Marshal(sig)
		require.NoError(t, err)
		return data
	}
	whole := encode(*good)
	edit := func(change func(*Signature)) []byte {
		sig := *good
		change(&sig)
		return encode(sig)
	}
	cases := map[string][]byte{
		"empty":                          nil,
		"truncated":                      whole[:len(whole)/2],
		"zeroed":                         make([]byte, len(whole)),
		"garbage":                        []byte("\x00\xff not json"),
		"an array":                       []byte(`[]`),
		"null":                           []byte(`null`),
		"signature of another digest":    encode(*other),
		"digest swapped, signature kept": edit(func(sig *Signature) { sig.Digest = otherDigest }),
		"signed by another key":          encode(*foreign),
		"another key claiming our id":    edit(func(sig *Signature) { sig.Signature = foreign.Signature }),
		"signature cut short":            edit(func(sig *Signature) { sig.Signature = sig.Signature[:64] }),
		"signature not hex":              edit(func(sig *Signature) { sig.Signature = strings.Repeat("zz", 64) }),
		"signature odd length":           edit(func(sig *Signature) { sig.Signature = sig.Signature[:127] }),
		"signature empty":                edit(func(sig *Signature) { sig.Signature = "" }),
		"one bit flipped": edit(func(sig *Signature) {
			b := []byte(sig.Signature)
			if b[0] == '0' {
				b[0] = '1'
			} else {
				b[0] = '0'
			}
			sig.Signature = string(b)
		}),
		"legacy format": edit(func(sig *Signature) { sig.Type = legacySignatureType }),
		"unknown type":  edit(func(sig *Signature) { sig.Type = "rsa" }),
		"no type":       edit(func(sig *Signature) { sig.Type = "" }),
		"no key id":     edit(func(sig *Signature) { sig.KeyID = "" }),
	}
	path := s.signaturePath(faultDigest)
	require.FileExists(t, path)
	require.Equal(t, filepath.Join(root, sigDirName), filepath.Dir(path))
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			require.NoError(t, os.WriteFile(path, content, 0o644))
			_, err := s.VerifyDigest(faultDigest)
			require.Error(t, err, "a damaged or foreign signature was accepted or taken for no signature")
		})
	}

	// The untouched file verifies, so each case fails for what it changed.
	require.NoError(t, os.WriteFile(path, whole, 0o644))
	sig, err := s.VerifyDigest(faultDigest)
	require.NoError(t, err)
	require.NotNil(t, sig)
}

// A digest comes from the daemon, which may be remote. One shaped like a
// path must not make the store write a signature, or read one, outside its
// signatures directory.
func TestFaults_DigestCannotNameAPathOutsideTheStore(t *testing.T) {
	s, root := newFaultStore(t)
	outside := filepath.Join(filepath.Dir(root), "planted")
	hostile := []string{
		"../../" + filepath.Base(filepath.Dir(root)) + "/planted",
		"../planted",
		"..",
		".",
		"",
		"a/b",
		`a\b`,
		"sha256:../../x",
		"x\x00y",
	}
	for _, d := range hostile {
		_, err := s.SignDigest(d)
		require.Error(t, err, "%q", d)
		sig, err := s.VerifyDigest(d)
		require.Error(t, err, "%q", d)
		assert.Nil(t, sig)
	}
	// Nothing was written beside the store, in it, or above it.
	matches, err := filepath.Glob(outside + "*")
	require.NoError(t, err)
	assert.Empty(t, matches)
	entries, err := os.ReadDir(root)
	require.NoError(t, err)
	for _, e := range entries {
		assert.Contains(t, []string{keyDirName, sigDirName}, e.Name())
	}
	if sigs, err := os.ReadDir(filepath.Join(root, sigDirName)); err == nil {
		assert.Empty(t, sigs)
	}
	// An ordinary digest is still signed.
	_, err = s.SignDigest(faultDigest)
	require.NoError(t, err)
}

// Sign and Verify with keys that are not Ed25519 keys of the right size, as
// a truncated key file would hand them over if it were loaded unchecked.
func TestFaults_SignAndVerifyRefuseMalformedKeys(t *testing.T) {
	kp, err := GenerateKeyPair()
	require.NoError(t, err)
	sig, err := Sign(kp, faultDigest)
	require.NoError(t, err)
	for _, n := range []int{0, 1, ed25519.PublicKeySize - 1, ed25519.PublicKeySize + 1, ed25519.PrivateKeySize} {
		require.Error(t, Verify(make(ed25519.PublicKey, n), sig), "public key of %d bytes", n)
	}
	for _, n := range []int{0, ed25519.PublicKeySize, ed25519.PrivateKeySize - 1, ed25519.PrivateKeySize + 1} {
		_, err := Sign(&KeyPair{PrivateKey: make(ed25519.PrivateKey, n)}, faultDigest)
		require.Error(t, err, "private key of %d bytes", n)
	}
	// An all-zero public key of the right size is a key, and not the signer.
	require.Error(t, Verify(make(ed25519.PublicKey, ed25519.PublicKeySize), sig))
}

func FuzzVerifySignatureFile(f *testing.F) {
	kp, err := GenerateKeyPair()
	require.NoError(f, err)
	sig, err := Sign(kp, faultDigest)
	require.NoError(f, err)
	whole, err := json.Marshal(sig)
	require.NoError(f, err)
	f.Add(whole)
	f.Add(whole[:len(whole)/2])
	f.Add([]byte(`{"type":"jerboa-ed25519-v1","keyId":"","digest":"","signature":""}`))
	f.Add([]byte(`{"type":"ed25519"}`))
	f.Add([]byte(`null`))
	f.Fuzz(func(t *testing.T, data []byte) {
		var got Signature
		if json.Unmarshal(data, &got) != nil {
			return
		}
		// Whatever the file says, it verifies only if it is the signature
		// this key made over this digest.
		if Verify(kp.PublicKey, &got) == nil {
			require.Equal(t, sig.Signature, strings.ToLower(got.Signature))
			require.Equal(t, sig.Digest, got.Digest)
		}
	})
}
