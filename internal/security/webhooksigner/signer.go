package webhooksigner

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"time"
)

const SignatureScheme = "sha256"

type Headers struct {
	Timestamp string
	Signature string
}

func Sign(secret string, payload []byte, now time.Time) Headers {
	timestamp := strconv.FormatInt(now.UTC().Unix(), 10)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(timestamp))
	mac.Write([]byte{'.'})
	mac.Write(payload)
	sum := mac.Sum(nil)
	return Headers{
		Timestamp: timestamp,
		Signature: SignatureScheme + "=" + hex.EncodeToString(sum),
	}
}
