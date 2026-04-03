package secretcipher

import (
	"strings"
	"testing"
)

func TestCipherEncryptDecryptRoundTrip(t *testing.T) {
	t.Parallel()

	c := New("test-key")
	enc, err := c.Encrypt("super-secret")
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if enc == "super-secret" {
		t.Fatal("expected encrypted value to differ from plaintext")
	}
	dec, err := c.Decrypt(enc)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if dec != "super-secret" {
		t.Fatalf("decrypt=%q want=%q", dec, "super-secret")
	}
}

func TestCipherDecryptLegacyPlaintext(t *testing.T) {
	t.Parallel()

	c := New("test-key")
	dec, err := c.Decrypt("legacy-plain-secret")
	if err != nil {
		t.Fatalf("decrypt legacy: %v", err)
	}
	if dec != "legacy-plain-secret" {
		t.Fatalf("legacy decrypt=%q want=%q", dec, "legacy-plain-secret")
	}
}

func TestCipherEncryptIncludesKeyID(t *testing.T) {
	t.Parallel()

	c := NewWithKeyset("primary-key", "old=old-key")
	enc, err := c.Encrypt("value")
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if got, wantPrefix := enc, "enc:v1:v1:"; len(got) < len(wantPrefix) || got[:len(wantPrefix)] != wantPrefix {
		t.Fatalf("ciphertext prefix=%q want=%q", enc, wantPrefix)
	}
}

func TestCipherDecryptLegacyEncryptedWithoutKeyID(t *testing.T) {
	t.Parallel()

	legacy := New("legacy-key")
	oldEncrypted, err := legacy.Encrypt("legacy-secret")
	if err != nil {
		t.Fatalf("legacy encrypt: %v", err)
	}
	// Simulate historical payload layout before key ID embedding:
	// enc:v1:<base64>
	oldEncrypted = "enc:v1:" + oldEncrypted[len("enc:v1:v1:"):]

	upgraded := NewWithKeyset("", "legacy=legacy-key")
	plain, err := upgraded.Decrypt(oldEncrypted)
	if err != nil {
		t.Fatalf("decrypt old format: %v", err)
	}
	if plain != "legacy-secret" {
		t.Fatalf("plain=%q want=%q", plain, "legacy-secret")
	}
}

func TestCipherDecryptAfterKeyRotation(t *testing.T) {
	t.Parallel()

	legacy := New("legacy-key")
	oldEncrypted, err := legacy.Encrypt("legacy-secret")
	if err != nil {
		t.Fatalf("legacy encrypt: %v", err)
	}

	rotated := NewWithKeyset("current-key", "v1=legacy-key")
	plain, err := rotated.Decrypt(oldEncrypted)
	if err != nil {
		t.Fatalf("decrypt rotated legacy payload: %v", err)
	}
	if plain != "legacy-secret" {
		t.Fatalf("plain=%q want=%q", plain, "legacy-secret")
	}

	currentEncrypted, err := rotated.Encrypt("current-secret")
	if err != nil {
		t.Fatalf("rotated encrypt: %v", err)
	}
	if !strings.HasPrefix(currentEncrypted, "enc:v1:current:") {
		t.Fatalf("ciphertext=%q want prefix %q", currentEncrypted, "enc:v1:current:")
	}

	remapped := "enc:v1:v1:" + currentEncrypted[len("enc:v1:current:"):]
	plain, err = rotated.Decrypt(remapped)
	if err != nil {
		t.Fatalf("decrypt fallback payload: %v", err)
	}
	if plain != "current-secret" {
		t.Fatalf("plain=%q want=%q", plain, "current-secret")
	}
}
