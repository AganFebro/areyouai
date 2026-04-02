package secretcipher

import "testing"

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
