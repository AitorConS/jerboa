package api

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
)

// authProofContext separates the handshake MAC from any other use of the token.
const authProofContext = "jerboa-auth-v1\n"

// NewAuthNonce returns a fresh random challenge for an Auth.Challenge reply.
func NewAuthNonce() []byte {
	nonce := make([]byte, 32)
	if _, err := rand.Read(nonce); err != nil {
		panic("api: crypto/rand failed: " + err.Error())
	}
	return nonce
}

// AuthProof is the client's answer to a challenge: an HMAC of the nonce keyed
// with the token, so the token itself never crosses the connection.
func AuthProof(token string, nonce []byte) string {
	mac := hmac.New(sha256.New, []byte(token))
	mac.Write([]byte(authProofContext))
	mac.Write(nonce)
	return hex.EncodeToString(mac.Sum(nil))
}

// CheckAuthProof reports, in constant time, whether proof answers nonce for token.
func CheckAuthProof(token string, nonce []byte, proof string) bool {
	got, err := hex.DecodeString(proof)
	if err != nil || len(nonce) == 0 {
		return false
	}
	want, _ := hex.DecodeString(AuthProof(token, nonce))
	return hmac.Equal(got, want)
}
