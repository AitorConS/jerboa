package api

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
)

// authProofContext separates the handshake MAC from any other use of the token,
// and serverProofContext the daemon's answer from the client's, so neither
// side's proof can be reflected back as the other's.
const (
	authProofContext   = "jerboa-auth-v1\n"
	serverProofContext = "jerboa-auth-server-v1\n"
)

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

// ServerAuthProof is the daemon's answer to an authenticated client: an HMAC
// of both nonces keyed with the token. The client checks it before sending any
// request, so a process holding the daemon's address without the token (a
// squatter on its port while it is down) learns nothing the client sends.
func ServerAuthProof(token string, nonce, clientNonce []byte) string {
	mac := hmac.New(sha256.New, []byte(token))
	mac.Write([]byte(serverProofContext))
	mac.Write(nonce)
	mac.Write(clientNonce)
	return hex.EncodeToString(mac.Sum(nil))
}

// CheckServerAuthProof reports, in constant time, whether proof is the
// daemon's answer for token over nonce and clientNonce.
func CheckServerAuthProof(token string, nonce, clientNonce []byte, proof string) bool {
	got, err := hex.DecodeString(proof)
	if err != nil || len(nonce) == 0 || len(clientNonce) == 0 {
		return false
	}
	want, _ := hex.DecodeString(ServerAuthProof(token, nonce, clientNonce))
	return hmac.Equal(got, want)
}
