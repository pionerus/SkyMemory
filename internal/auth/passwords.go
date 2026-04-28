package auth

import (
	"errors"

	"golang.org/x/crypto/bcrypt"
)

// HashCost — bcrypt work factor. 12 = ~250ms on a modern CPU. Bump every few years.
const HashCost = 12

// HashPassword returns a bcrypt hash. Empty input is rejected — this catches
// programmer errors where pw is unset and would otherwise hash an empty string
// to a valid-looking value.
func HashPassword(pw string) (string, error) {
	if pw == "" {
		return "", errors.New("password is empty")
	}
	if len(pw) > 72 {
		// bcrypt silently truncates beyond 72 bytes. Reject explicitly so users
		// don't get a false sense of security from longer passwords.
		return "", errors.New("password too long (max 72 bytes)")
	}
	b, err := bcrypt.GenerateFromPassword([]byte(pw), HashCost)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// VerifyPassword returns true iff `pw` matches the stored bcrypt hash.
// Constant-time comparison is handled by bcrypt.CompareHashAndPassword.
func VerifyPassword(hash, pw string) bool {
	if hash == "" || pw == "" {
		return false
	}
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(pw)) == nil
}
