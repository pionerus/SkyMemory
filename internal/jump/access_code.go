package jump

import (
	"crypto/rand"
	"errors"
	"strings"
)

// Access code = 8 chars Crockford Base32 (0-9 + A-Z minus I/L/O/U). 32^8 ≈
// 1.1 × 10^12 codes. With rate limiting this is uncrackable; UNIQUE constraint
// catches the rare collision and we retry up to 3 times in the handler.
const accessCodeLen = 8

// Crockford Base32 alphabet — exactly 32 chars, all alphanumeric, no I/L/O/U
// (those are confusable with 1/O/0/V and Crockford parsers normalize them on input).
var accessCodeAlphabet = []byte("0123456789ABCDEFGHJKMNPQRSTVWXYZ")

func init() {
	if len(accessCodeAlphabet) != 32 {
		panic("access code alphabet must be exactly 32 chars")
	}
}

// NewAccessCode generates a fresh random 8-char access code, e.g. "K7M2-PXQF".
// Format inserts a dash for human readability — DB stores the dashless canonical form.
func NewAccessCode() (canonical, formatted string, err error) {
	b := make([]byte, accessCodeLen)
	if _, err := rand.Read(b); err != nil {
		return "", "", err
	}
	out := make([]byte, accessCodeLen)
	for i := 0; i < accessCodeLen; i++ {
		out[i] = accessCodeAlphabet[int(b[i])&31]
	}
	canonical = string(out)
	formatted = canonical[:4] + "-" + canonical[4:]
	return canonical, formatted, nil
}

// CanonicalizeAccessCode strips dashes/spaces and uppercases. Used when the client
// portal receives a code from a URL or a typed-in form.
func CanonicalizeAccessCode(s string) (string, error) {
	s = strings.ToUpper(strings.NewReplacer("-", "", " ", "").Replace(s))
	if len(s) != accessCodeLen {
		return "", errors.New("access code must be 8 chars")
	}
	return s, nil
}
