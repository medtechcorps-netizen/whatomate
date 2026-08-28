package model

import (
	"encoding/base64"
	"encoding/hex"
	"strings"
	"testing"
)

func TestSanitizeLogRemovesMarkerTokensCredentialsAndKeys(t *testing.T) {
	marker := []byte("synthetic-marker-preimage-32-byte")
	privateKey := []byte("synthetic-private-signing-material")
	jwt := "eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiJzeW50aGV0aWMifQ.c3ludGhldGljLXNpZ25hdHVyZQ"
	input := strings.Join([]string{
		"marker=" + string(marker),
		"marker_hex=" + hex.EncodeToString(marker),
		"marker_b64=" + base64.StdEncoding.EncodeToString(marker),
		"marker_url=" + base64.RawURLEncoding.EncodeToString(marker),
		"authorization=" + jwt,
		"password=synthetic-password",
		"private_key=" + string(privateKey),
	}, " ")
	output := SanitizeLog(input, marker, privateKey)
	for _, forbidden := range []string{
		string(marker), hex.EncodeToString(marker), base64.StdEncoding.EncodeToString(marker),
		base64.RawURLEncoding.EncodeToString(marker), jwt, "synthetic-password", string(privateKey),
	} {
		if strings.Contains(output, forbidden) {
			t.Fatalf("secret remained in sanitized output: %q", forbidden)
		}
	}
	if !strings.Contains(output, redacted) {
		t.Fatal("sanitizer did not mark redaction")
	}
}

func TestPublicDiagnosticNeverEchoesInput(t *testing.T) {
	secret := "synthetic-provider-endpoint-and-credential"
	if got := PublicDiagnostic(secret); strings.Contains(got, secret) || got != "recovery-boundary:internal" {
		t.Fatalf("unknown diagnostic leaked input: %q", got)
	}
	if got := PublicDiagnostic("reconcile-required"); got != "recovery-boundary:reconcile-required" {
		t.Fatalf("known diagnostic = %q", got)
	}
}
