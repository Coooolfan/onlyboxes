package registryauth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

// SignInput returns the canonical payload used by HMAC signing.
func SignInput(nodeID string, timestampUnixMS int64, nonce string) string {
	return fmt.Sprintf("%s\n%d\n%s", nodeID, timestampUnixMS, nonce)
}

// Sign computes a lowercase hex HMAC-SHA256 signature for the given fields.
func Sign(nodeID string, timestampUnixMS int64, nonce string, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(SignInput(nodeID, timestampUnixMS, nonce)))
	return hex.EncodeToString(mac.Sum(nil))
}

// Verify checks the provided lowercase hex HMAC-SHA256 signature.
func Verify(nodeID string, timestampUnixMS int64, nonce string, secret string, signature string) bool {
	rawSignature := strings.TrimSpace(signature)
	decodedSignature, err := hex.DecodeString(rawSignature)
	if err != nil {
		return false
	}

	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(SignInput(nodeID, timestampUnixMS, nonce)))
	expected := mac.Sum(nil)
	return hmac.Equal(decodedSignature, expected)
}
