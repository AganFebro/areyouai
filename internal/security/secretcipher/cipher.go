package secretcipher

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
)

const (
	prefix              = "enc:v1:"
	defaultKeyID        = "v1"
	rotatedPrimaryKeyID = "current"
	defaultKeyMaterial  = "areyouai-dev-webhook-secret-encryption-key"
	nonceSize           = 12
)

type Cipher struct {
	primaryKeyID string
	keyByID      map[string][32]byte
}

func New(keyMaterial string) *Cipher {
	return NewWithKeyset(keyMaterial, "")
}

// NewWithKeyset supports key rotation. keysetRaw format:
// "kid_a=material_a,kid_b=material_b". The single keyMaterial remains the
// primary encryption key for backward compatibility.
func NewWithKeyset(keyMaterial, keysetRaw string) *Cipher {
	keys := map[string]string{}
	for id, material := range parseKeyset(keysetRaw) {
		keys[id] = material
	}
	primaryKeyID := ""
	if v := strings.TrimSpace(keyMaterial); v != "" {
		if _, ok := keys[defaultKeyID]; ok {
			// Preserve the legacy v1 binding so existing ciphertext stays decryptable.
			primaryKeyID = rotatedPrimaryKeyID
			for i := 0; ; i++ {
				candidate := primaryKeyID
				if i > 0 {
					candidate = fmt.Sprintf("%s-%d", rotatedPrimaryKeyID, i)
				}
				if _, exists := keys[candidate]; !exists {
					primaryKeyID = candidate
					keys[candidate] = v
					break
				}
			}
		} else {
			keys[defaultKeyID] = v
			primaryKeyID = defaultKeyID
		}
	}
	if len(keys) == 0 {
		keys[defaultKeyID] = defaultKeyMaterial
	}
	if primaryKeyID == "" {
		primaryKeyID = defaultKeyID
		if _, ok := keys[primaryKeyID]; !ok {
			ordered := sortedKeyIDs(keys)
			primaryKeyID = ordered[0]
		}
	}

	keyByID := make(map[string][32]byte, len(keys))
	for id, material := range keys {
		keyByID[id] = sha256.Sum256([]byte(material))
	}
	return &Cipher{
		primaryKeyID: primaryKeyID,
		keyByID:      keyByID,
	}
}

func (c *Cipher) Encrypt(plaintext string) (string, error) {
	key, ok := c.keyByID[c.primaryKeyID]
	if !ok {
		return "", errors.New("primary key not found")
	}
	sealed, err := encryptWithKey(key, plaintext)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s%s:%s", prefix, c.primaryKeyID, sealed), nil
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
	payload := strings.TrimPrefix(ciphertext, prefix)
	if payload == "" {
		return "", errors.New("empty encrypted payload")
	}

	// Current format: enc:v1:<key_id>:<base64(nonce|sealed)>
	if keyID, encoded, ok := splitKeyedPayload(payload); ok {
		if plain, err := c.decryptWithKeyIDFallback(encoded, keyID); err == nil {
			return plain, nil
		} else {
			return "", err
		}
	}

	// Legacy encrypted format: enc:v1:<base64(nonce|sealed)>.
	ordered := c.decryptKeyIDs()
	var lastErr error
	for _, keyID := range ordered {
		plain, err := decryptWithKey(payload, c.keyByID[keyID])
		if err == nil {
			return plain, nil
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = errors.New("unable to decrypt with configured keys")
	}
	return "", lastErr
}

func (c *Cipher) decryptWithKeyIDFallback(encoded, keyID string) (string, error) {
	if key, exists := c.keyByID[keyID]; exists {
		if plain, err := decryptWithKey(encoded, key); err == nil {
			return plain, nil
		} else {
			lastErr := err
			for _, candidateID := range c.decryptKeyIDs() {
				if candidateID == keyID {
					continue
				}
				plain, candErr := decryptWithKey(encoded, c.keyByID[candidateID])
				if candErr == nil {
					return plain, nil
				}
				lastErr = candErr
			}
			return "", lastErr
		}
	}
	var lastErr error
	for _, candidateID := range c.decryptKeyIDs() {
		plain, candErr := decryptWithKey(encoded, c.keyByID[candidateID])
		if candErr == nil {
			return plain, nil
		}
		lastErr = candErr
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("unknown key id %q", keyID)
	}
	return "", lastErr
}

func encryptWithKey(key [32]byte, plaintext string) (string, error) {
	block, err := aes.NewCipher(key[:])
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
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

func decryptWithKey(encoded string, key [32]byte) (string, error) {
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(encoded))
	if err != nil {
		return "", err
	}
	if len(raw) < nonceSize {
		return "", errors.New("ciphertext too short")
	}
	nonce := raw[:nonceSize]
	sealed := raw[nonceSize:]

	block, err := aes.NewCipher(key[:])
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

func parseKeyset(raw string) map[string]string {
	out := map[string]string{}
	for _, item := range strings.Split(strings.TrimSpace(raw), ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		parts := strings.SplitN(item, "=", 2)
		if len(parts) != 2 {
			continue
		}
		keyID := strings.TrimSpace(parts[0])
		material := strings.TrimSpace(parts[1])
		if keyID == "" || material == "" {
			continue
		}
		out[keyID] = material
	}
	return out
}

func splitKeyedPayload(payload string) (string, string, bool) {
	parts := strings.SplitN(payload, ":", 2)
	if len(parts) != 2 {
		return "", "", false
	}
	keyID := strings.TrimSpace(parts[0])
	encoded := strings.TrimSpace(parts[1])
	if keyID == "" || encoded == "" {
		return "", "", false
	}
	return keyID, encoded, true
}

func sortedKeyIDs(keys map[string]string) []string {
	out := make([]string, 0, len(keys))
	for id := range keys {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

func (c *Cipher) decryptKeyIDs() []string {
	out := make([]string, 0, len(c.keyByID))
	if _, ok := c.keyByID[c.primaryKeyID]; ok {
		out = append(out, c.primaryKeyID)
	}
	ids := make([]string, 0, len(c.keyByID))
	for id := range c.keyByID {
		if id == c.primaryKeyID {
			continue
		}
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return append(out, ids...)
}
