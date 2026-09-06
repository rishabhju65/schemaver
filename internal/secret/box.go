// Package secret encrypts credentials at rest.
//
// The key never lives in the database it protects: it comes from the process
// environment, so a leaked database dump yields ciphertext and nothing else.
// Every ciphertext records which key produced it, so a key rotation can find the
// rows it still owes work to.
package secret

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
)

// KeyEnvVar names the environment variable holding the base64-encoded 32-byte
// encryption key.
const KeyEnvVar = "SCHEMAVER_ENCRYPTION_KEY"

// Box seals and opens credential secrets.
type Box struct {
	aead  cipher.AEAD
	keyID string
}

// New builds a Box from a 32-byte key. keyID is stored alongside each ciphertext
// and is only an identifier — it is not secret and must not be derived from the
// key material.
func New(key []byte, keyID string) (*Box, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("encryption key must be 32 bytes, got %d", len(key))
	}
	if keyID == "" {
		return nil, errors.New("encryption key needs an id, so rotation can identify what it encrypted")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("build cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("build gcm: %w", err)
	}
	return &Box{aead: aead, keyID: keyID}, nil
}

// FromEnv builds a Box from KeyEnvVar, which must hold a base64-encoded 32-byte
// key.
//
// The error is deliberately instructive: an operator hitting this on first run
// needs to be told how to generate a key, not merely that one is missing.
func FromEnv() (*Box, error) {
	raw := os.Getenv(KeyEnvVar)
	if raw == "" {
		return nil, fmt.Errorf(
			"%s is not set; generate one with:\n  openssl rand -base64 32\n"+
				"Store it safely — losing it makes every stored credential unrecoverable",
			KeyEnvVar)
	}
	key, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return nil, fmt.Errorf("%s is not valid base64: %w", KeyEnvVar, err)
	}
	return New(key, keyID(key))
}

// keyID derives a short, non-reversible label for a key so that rotation can
// tell ciphertexts apart without the key itself identifying anything.
func keyID(key []byte) string {
	sum := sha256Sum(key)
	return base64.RawURLEncoding.EncodeToString(sum[:6])
}

// KeyID reports the identifier of the key this Box holds.
func (b *Box) KeyID() string { return b.keyID }

// Seal encrypts plaintext. The nonce is prepended to the returned ciphertext.
func (b *Box) Seal(plaintext string) ([]byte, error) {
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("generate nonce: %w", err)
	}
	return b.aead.Seal(nonce, nonce, []byte(plaintext), nil), nil
}

// Open decrypts a ciphertext produced by Seal.
func (b *Box) Open(ciphertext []byte) (string, error) {
	n := b.aead.NonceSize()
	if len(ciphertext) < n {
		return "", errors.New("ciphertext is shorter than its nonce; it is truncated or corrupt")
	}
	plaintext, err := b.aead.Open(nil, ciphertext[:n], ciphertext[n:], nil)
	if err != nil {
		// Most often this means the key changed, not that the data is damaged.
		return "", fmt.Errorf("decrypt failed (wrong %s?): %w", KeyEnvVar, err)
	}
	return string(plaintext), nil
}
