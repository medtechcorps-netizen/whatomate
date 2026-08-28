package model

import (
	"encoding/base64"
	"encoding/hex"
	"regexp"
	"strings"
)

const redacted = "[REDACTED]"

var (
	jwtPattern              = regexp.MustCompile(`(?i)\beyJ[a-z0-9_-]{6,}\.[a-z0-9_-]{6,}\.[a-z0-9_-]{6,}\b`)
	secretAssignmentPattern = regexp.MustCompile(`(?i)\b(password|passwd|token|authorization|credential|private[_-]?key|client[_-]?secret)\s*[:=]\s*([^\s,;]+)`)
)

// SanitizeLog redacts explicit secret bytes in literal, hex, standard-base64,
// and raw-URL-base64 forms, then removes JWTs and common secret assignments.
// It must be applied before a modeled diagnostic leaves the fixed boundary.
func SanitizeLog(input string, secrets ...[]byte) string {
	output := input
	for _, secret := range secrets {
		if len(secret) == 0 {
			continue
		}
		encodings := []string{
			string(secret),
			hex.EncodeToString(secret),
			base64.StdEncoding.EncodeToString(secret),
			base64.RawURLEncoding.EncodeToString(secret),
		}
		for _, encoded := range encodings {
			if encoded != "" {
				output = strings.ReplaceAll(output, encoded, redacted)
			}
		}
	}
	output = jwtPattern.ReplaceAllString(output, redacted)
	output = secretAssignmentPattern.ReplaceAllString(output, `$1=`+redacted)
	return output
}

// PublicDiagnostic intentionally admits only a small semantic code; raw
// provider/runtime errors are never returned by the model.
func PublicDiagnostic(code string) string {
	switch code {
	case "invalid", "quarantined", "reconcile-required", "not-observed", "multiple-observed", "marker-mismatch":
		return "recovery-boundary:" + code
	default:
		return "recovery-boundary:internal"
	}
}
