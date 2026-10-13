package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"strings"
)

// keyBox encrypts users' API keys at rest. A key has to be sent to Anthropic
// as-is, so it cannot be hashed like a password; it is sealed with a secret
// held in the container template instead, so the database and its backups
// alone do not reveal it.
type keyBox struct{ aead cipher.AEAD }

func newKeyBox(secret string) (*keyBox, error) {
	if len(secret) < 16 {
		return nil, errors.New("KEY_SECRET must be at least 16 characters")
	}
	k := sha256.Sum256([]byte(secret))
	block, err := aes.NewCipher(k[:])
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &keyBox{aead}, nil
}

const sealedPrefix = "v1:"

func (b *keyBox) seal(plain string) string {
	nonce := make([]byte, b.aead.NonceSize())
	rand.Read(nonce)
	return sealedPrefix + base64.StdEncoding.EncodeToString(b.aead.Seal(nonce, nonce, []byte(plain), nil))
}

// open fails when the secret has changed since the key was saved; the key
// then has to be entered again.
func (b *keyBox) open(sealed string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(sealed, sealedPrefix))
	if err != nil || !strings.HasPrefix(sealed, sealedPrefix) || len(raw) < b.aead.NonceSize() {
		return "", errors.New("stored key is not readable")
	}
	n := b.aead.NonceSize()
	plain, err := b.aead.Open(nil, raw[:n], raw[n:], nil)
	if err != nil {
		return "", errors.New("stored key cannot be decrypted; KEY_SECRET has changed, enter the key again")
	}
	return string(plain), nil
}
