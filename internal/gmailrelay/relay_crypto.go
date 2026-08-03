package gmailrelay

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

const (
	ReReplySignatureHeader = "X-ReReply-Signature-256"
	SetupKeyHeader         = "X-ReReply-Setup-Key"
	relaySignaturePrefix   = "sha256="
)

func signRelayBody(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(body)
	return relaySignaturePrefix + hex.EncodeToString(mac.Sum(nil))
}

func verifyRelayBody(secret, signature string, body []byte) bool {
	if secret == "" || !strings.HasPrefix(signature, relaySignaturePrefix) {
		return false
	}
	provided, err := hex.DecodeString(strings.TrimPrefix(signature, relaySignaturePrefix))
	if err != nil || len(provided) != sha256.Size {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(body)
	return hmac.Equal(provided, mac.Sum(nil))
}

func constantTimeSecretEqual(left, right string) bool {
	return len(left) == len(right) && hmac.Equal([]byte(left), []byte(right))
}
