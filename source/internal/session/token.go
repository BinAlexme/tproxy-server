package session

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
)

type TokenClass byte

const (
	TokenExternal TokenClass = iota
	TokenBootstrap
	TokenSession
)

func (m *Manager) tokenMAC(kind TokenClass, nonce []byte) []byte {
	mac := hmac.New(sha256.New, m.tokenKey[:])
	_, _ = mac.Write([]byte("tproxy-server-token-v1\x00"))
	_, _ = mac.Write([]byte{byte(kind)})
	_, _ = mac.Write(nonce)
	return mac.Sum(nil)[:16]
}

func (m *Manager) newToken(kind TokenClass) (string, [sha256.Size]byte, error) {
	var input [32]byte
	if _, err := rand.Read(input[:16]); err != nil {
		return "", [sha256.Size]byte{}, fmt.Errorf("random token: %w", err)
	}
	copy(input[16:], m.tokenMAC(kind, input[:16]))
	return base64.RawURLEncoding.EncodeToString(input[:]), sha256.Sum256(input[:]), nil
}

// Classification authenticates provenance, not liveness or permission to use
// a carrier. Both MACs are checked even on expired and misplaced credentials.
// Lenient base64 decoding here also contains noncanonical spellings locally;
// tokenHash still requires canonical encoding when authorizing an operation.
func (m *Manager) ClassifyToken(token string) TokenClass {
	decoded, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(decoded) != 32 {
		return TokenExternal
	}
	bootstrap := subtle.ConstantTimeCompare(decoded[16:], m.tokenMAC(TokenBootstrap, decoded[:16]))
	session := subtle.ConstantTimeCompare(decoded[16:], m.tokenMAC(TokenSession, decoded[:16]))
	if bootstrap == 1 {
		return TokenBootstrap
	}
	if session == 1 {
		return TokenSession
	}
	return TokenExternal
}
