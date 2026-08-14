package metaregistry

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	TimestampHeader = "X-ReReply-Service-Timestamp"
	NonceHeader     = "X-ReReply-Service-Nonce"
	SignatureHeader = "X-ReReply-Service-Signature-256"
	ResponseHeader  = "X-ReReply-Service-Response-256"
	SignaturePrefix = "sha256="
	MaxClockSkew    = 2 * time.Minute
	// ReplayWindowFloor covers the full earliest-to-latest acceptance span for
	// a signed timestamp plus one minute of operational margin. A nonce first
	// seen near the negative-skew boundary therefore cannot expire while the
	// same signature is still within the positive-skew boundary.
	ReplayWindowFloor = 2*MaxClockSkew + time.Minute
)

func NewNonce() (string, error) {
	value := make([]byte, 24)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return hex.EncodeToString(value), nil
}

func SignRequest(secret, method, path string, timestamp time.Time, nonce string, body []byte) string {
	return SignaturePrefix + sign(secret, requestCanonical(method, path, timestamp, nonce, body))
}

func VerifyRequest(secret, method, path, rawTimestamp, nonce, signature string, body []byte, now time.Time) error {
	if len(secret) < 32 || nonce == "" || len(nonce) > 128 {
		return ErrInvalidRequest
	}
	unix, err := strconv.ParseInt(strings.TrimSpace(rawTimestamp), 10, 64)
	if err != nil {
		return ErrInvalidRequest
	}
	timestamp := time.Unix(unix, 0).UTC()
	delta := now.UTC().Sub(timestamp)
	if delta < -MaxClockSkew || delta > MaxClockSkew {
		return ErrInvalidRequest
	}
	expected := SignRequest(secret, method, path, timestamp, nonce, body)
	if !constantTimeString(expected, signature) {
		return ErrInvalidRequest
	}
	return nil
}

func ApplyRequestHeaders(request *http.Request, secret string, now time.Time, nonce string, body []byte) {
	timestamp := strconv.FormatInt(now.UTC().Unix(), 10)
	request.Header.Set(TimestampHeader, timestamp)
	request.Header.Set(NonceHeader, nonce)
	request.Header.Set(SignatureHeader, SignRequest(secret, request.Method, request.URL.Path, now, nonce, body))
}

func SignResponse(secret, nonce string, status int, body []byte) string {
	canonical := fmt.Sprintf("%s\n%d\n%s", nonce, status, digest(body))
	return SignaturePrefix + sign(secret, canonical)
}

func VerifyResponse(secret, nonce string, status int, body []byte, signature string) error {
	if len(secret) < 32 || nonce == "" || !constantTimeString(SignResponse(secret, nonce, status, body), signature) {
		return errors.New("invalid Meta registry response signature")
	}
	return nil
}

func requestCanonical(method, path string, timestamp time.Time, nonce string, body []byte) string {
	return strings.ToUpper(strings.TrimSpace(method)) + "\n" + path + "\n" +
		strconv.FormatInt(timestamp.UTC().Unix(), 10) + "\n" + nonce + "\n" + digest(body)
}

func digest(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

func sign(secret, canonical string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(canonical))
	return hex.EncodeToString(mac.Sum(nil))
}

func constantTimeString(left, right string) bool {
	return hmac.Equal([]byte(left), []byte(right))
}
