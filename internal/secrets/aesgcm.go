// Package secrets wraps AES-GCM encryption around the app-wide master key
// stored in app_settings.storage_secret_key. Used by tenant_storage_configs,
// operator_drive_configs, and any other table that needs to keep operator-
// supplied credentials at rest.
//
// Format on disk: nonce || ciphertext || authTag (all concatenated). 12-byte
// nonce, AES-256-GCM. Master key MUST be 32 bytes; pulled fresh from
// app_settings on each Encrypt/Decrypt rather than cached so a key rotation
// is one UPDATE away.
package secrets

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
	"io"

	"github.com/pionerus/freefall/internal/db"
)

// LoadMasterKey reads app_settings.storage_secret_key. Caller decides
// caching policy; for low-traffic admin endpoints fetching per-call is fine.
func LoadMasterKey(ctx context.Context, pool *db.Pool) ([]byte, error) {
	var key []byte
	if err := pool.QueryRow(ctx,
		`SELECT storage_secret_key FROM app_settings WHERE id = 1`,
	).Scan(&key); err != nil {
		return nil, fmt.Errorf("load master key: %w", err)
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("master key must be 32 bytes, got %d", len(key))
	}
	return key, nil
}

// Encrypt seals plaintext with the given AES-256 key. Returns nonce || ct || tag.
// Empty plaintext yields empty output (callers can decode "no value" cleanly).
func Encrypt(masterKey, plaintext []byte) ([]byte, error) {
	if len(plaintext) == 0 {
		return nil, nil
	}
	if len(masterKey) != 32 {
		return nil, errors.New("master key must be 32 bytes")
	}
	block, err := aes.NewCipher(masterKey)
	if err != nil {
		return nil, fmt.Errorf("aes new: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("gcm new: %w", err)
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("nonce: %w", err)
	}
	// Seal appends ciphertext+tag onto its first arg; we want the whole blob
	// (nonce + ct + tag) in one slice so use append.
	return gcm.Seal(nonce, nonce, plaintext, nil), nil
}

// Decrypt reverses Encrypt. Returns ErrEmpty when input is empty so callers
// can distinguish "never set" from corrupted data.
func Decrypt(masterKey, blob []byte) ([]byte, error) {
	if len(blob) == 0 {
		return nil, ErrEmpty
	}
	if len(masterKey) != 32 {
		return nil, errors.New("master key must be 32 bytes")
	}
	block, err := aes.NewCipher(masterKey)
	if err != nil {
		return nil, fmt.Errorf("aes new: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("gcm new: %w", err)
	}
	if len(blob) < gcm.NonceSize() {
		return nil, errors.New("ciphertext shorter than nonce")
	}
	nonce, ct := blob[:gcm.NonceSize()], blob[gcm.NonceSize():]
	pt, err := gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		return nil, fmt.Errorf("gcm open (key rotated? blob corrupted?): %w", err)
	}
	return pt, nil
}

// ErrEmpty indicates Decrypt was handed a zero-length blob — caller likely
// has an "unset secret" rather than a corrupted one.
var ErrEmpty = errors.New("secrets: empty blob")
