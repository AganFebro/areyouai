package webhooksigner

import (
	"strings"
	"testing"
	"time"
)

func TestSignDeterministic(t *testing.T) {
	now := time.Unix(1712016000, 0).UTC()
	headersA := Sign("secret", []byte(`{"ok":true}`), now)
	headersB := Sign("secret", []byte(`{"ok":true}`), now)

	if headersA.Timestamp != "1712016000" {
		t.Fatalf("timestamp=%q want=1712016000", headersA.Timestamp)
	}
	if headersA.Signature != headersB.Signature {
		t.Fatalf("signature mismatch a=%q b=%q", headersA.Signature, headersB.Signature)
	}
	if !strings.HasPrefix(headersA.Signature, SignatureScheme+"=") {
		t.Fatalf("signature=%q missing scheme prefix", headersA.Signature)
	}
}
