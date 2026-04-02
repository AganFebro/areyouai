package secretcipher

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"io"
	"strings"
)

const (
	prefix             = "enc:v1:"
	defaultKeyMaterial = "areyouai-dev-webhook-secret-encryption-key"
	nonceSize          = 12
)

type Cipher struct {
	key [32]byte
}

func New(keyMaterial string) *Cipher {
	keyMaterial = strings.TrimSpace(keyMaterial)
	if keyMaterial == "" {
		keyMaterial = defaultKeyMaterial
	}
	return &Cipher{
		key: sha256.Sum256([]byte(keyMaterial)),
	}
}

func (c *Cipher) Encrypt(plaintext string) (string, error) {
	block, err := aes.NewCipher(c.key[:])
	if err != nil {
		return "", err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}

	nonce := make([]byte, nonceSize)
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	sealed := aead.Seal(nil, nonce, []byte(plaintext), nil)
	buf := append(nonce, sealed...)
	return prefix + base64.RawURLEncoding.EncodeToString(buf), nil
}

func (c *Cipher) Decrypt(ciphertext string) (string, error) {
	ciphertext = strings.TrimSpace(ciphertext)
	if ciphertext == "" {
		return "", errors.New("empty ciphertext")
	}
	if !strings.HasPrefix(ciphertext, prefix) {
		// Legacy compatibility for rows created before secret encryption.
		return ciphertext, nil
	}

	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(ciphertext, prefix))
	if err != nil {
		return "", err
	}
	if len(raw) < nonceSize {
		return "", errors.New("ciphertext too short")
	}
	nonce := raw[:nonceSize]
	sealed := raw[nonceSize:]

	block, err := aes.NewCipher(c.key[:])
	if err != nil {
		return "", err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	plain, err := aead.Open(nil, nonce, sealed, nil)
	if err != nil {
		return "", err
	}
	return string(plain), nil
}
