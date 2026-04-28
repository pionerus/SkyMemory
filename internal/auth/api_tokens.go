package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"
)

// Token format: "ffsk_<43 chars base64url>" — 32 bytes of randomness.
// SHA256 hex of the full token is what's stored in license_tokens.token_hash.
//
// We use SHA256 instead of bcrypt because:
//   1. The token entropy is 256 bits, no need for a slow KDF
//   2. We need indexable lookup (bcrypt salt prevents that)
const (
	TokenPrefix     = "ffsk_"
	tokenRandomBits = 32
)

// GenerateLicenseToken produces a fresh token + its SHA256 hex hash.
// The plaintext token is shown to the operator ONCE on creation; the server
// only ever sees the hash again.
func GenerateLicenseToken() (plaintext, hash string, err error) {
	b := make([]byte, tokenRandomBits)
	if _, err := rand.Read(b); err != nil {
		return "", "", err
	}
	plaintext = TokenPrefix + base64.RawURLEncoding.EncodeToString(b)
	hash = HashLicenseToken(plaintext)
	return plaintext, hash, nil
}

// HashLicenseToken returns the canonical SHA256 hex of a token.
// Identical inputs always produce identical output — that's how we look it up.
func HashLicenseToken(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:])
}

// ParseLicenseTokenFromHeader extracts the token from "Authorization: Bearer <token>"
// or returns an error indicating exactly what's wrong (so middleware can produce
// a helpful 401 message).
func ParseLicenseTokenFromHeader(authHeader string) (string, error) {
	if authHeader == "" {
		return "", errors.New("missing Authorization header")
	}
	if !strings.HasPrefix(authHeader, "Bearer ") {
		return "", errors.New("Authorization header must start with 'Bearer '")
	}
	token := strings.TrimSpace(strings.TrimPrefix(authHeader, "Bearer "))
	if !strings.HasPrefix(token, TokenPrefix) {
		return "", errors.New("token has wrong prefix (expected " + TokenPrefix + ")")
	}
	return token, nil
}
