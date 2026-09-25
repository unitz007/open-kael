// Package crypto encrypts real credentials (an OAuth access token today)
// before they ever touch Postgres — Integration.ConnectionRef stores the
// output of Encrypt, never a plaintext secret. AES-256-GCM with a
// server-held key; this is deliberately NOT a full secrets-management
// system (no key rotation, no per-tenant keys, no KMS) — it's the minimum
// that keeps a database dump from also being a token dump, sized to what
// this pass actually needs.
package crypto

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
)

// KeySize is the required key length for Encrypt/Decrypt — AES-256.
const KeySize = 32

// Encrypt returns plaintext encrypted with key, base64-encoded (nonce
// prepended to the ciphertext, standard AES-GCM convention) so the result
// is a plain string safe to store in a TEXT column.
func Encrypt(key []byte, plaintext string) (string, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", fmt.Errorf("crypto: new cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("crypto: new gcm: %w", err)
	}

	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("crypto: generate nonce: %w", err)
	}

	ciphertext := gcm.Seal(nonce, nonce, []byte(plaintext), nil)
	return base64.StdEncoding.EncodeToString(ciphertext), nil
}

// Decrypt reverses Encrypt.
func Decrypt(key []byte, encoded string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", fmt.Errorf("crypto: decode base64: %w", err)
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return "", fmt.Errorf("crypto: new cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("crypto: new gcm: %w", err)
	}

	nonceSize := gcm.NonceSize()
	if len(raw) < nonceSize {
		return "", errors.New("crypto: ciphertext too short")
	}
	nonce, ciphertext := raw[:nonceSize], raw[nonceSize:]

	plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return "", fmt.Errorf("crypto: decrypt: %w", err)
	}
	return string(plaintext), nil
}

// Resolver implements domain.CredentialResolver using Encrypt/Decrypt.
// Integration.ConnectionRef holds the output of Encrypt; ResolveToken
// decrypts it back to the real token.
type Resolver struct {
	Key []byte
}

func NewResolver(key []byte) *Resolver {
	return &Resolver{Key: key}
}

func (r *Resolver) ResolveToken(_ context.Context, connectionRef string) (string, error) {
	return Decrypt(r.Key, connectionRef)
}

func (r *Resolver) EncryptToken(_ context.Context, plaintext string) (string, error) {
	return Encrypt(r.Key, plaintext)
}
