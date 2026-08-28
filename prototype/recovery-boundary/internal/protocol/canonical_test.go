package protocol

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

func syntheticOperation() OperationBody {
	return OperationBody{
		Schema: OperationSchema, OperationID: "synthetic-operation-001", Generation: 1,
		Role: RoleWriter, Phase: "baseline", Action: ActionMarkerCAS,
		ControlSHA: strings.Repeat("a", 40), RuntimeSourceSHA: strings.Repeat("b", 40),
		WorkflowDefinitionSHA256: strings.Repeat("c", 64), ConfigSHA256: strings.Repeat("d", 64),
		ImageSHA256: strings.Repeat("e", 64), AppSpecSHA256: strings.Repeat("f", 64),
		RequestedAtUTC: "2026-01-02T03:04:05.123456789Z",
	}
}

func syntheticAuth(bodyDigest [32]byte, method, jti, challenge string) AuthEnvelope {
	return AuthEnvelope{
		Schema: AuthSchema, OperationBodySHA256: SHA256Hex(bodyDigest), Method: method,
		JTI: jti, Challenge: challenge, IssuedAtUTC: "2026-01-02T03:04:06Z",
	}
}

func mustOperationJSON(t *testing.T, body OperationBody) []byte {
	t.Helper()
	value, err := MarshalOperationBody(body)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func mustOperationDigest(t *testing.T, bodyJSON []byte) [32]byte {
	t.Helper()
	digest, err := OperationBodyDigest(bodyJSON)
	if err != nil {
		t.Fatal(err)
	}
	return digest
}

func TestCanonicalOperationRoundTrip(t *testing.T) {
	raw := mustOperationJSON(t, syntheticOperation())
	decoded, err := DecodeOperationBody(raw)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.OperationID != "synthetic-operation-001" {
		t.Fatalf("unexpected decoded body: %#v", decoded)
	}
}

func TestCanonicalOperationRejectsAmbiguousEncodings(t *testing.T) {
	canonical := string(mustOperationJSON(t, syntheticOperation()))
	tests := map[string]string{
		"duplicate key":          strings.Replace(canonical, `"generation":1`, `"generation":1,"generation":1`, 1),
		"unknown key":            strings.TrimSuffix(canonical, "}") + `,"unknown":"value"}`,
		"float":                  strings.Replace(canonical, `"generation":1`, `"generation":1.0`, 1),
		"exponent":               strings.Replace(canonical, `"generation":1`, `"generation":1e0`, 1),
		"escaped ASCII":          strings.Replace(canonical, "synthetic-operation", `synthetic-\u006fperation`, 1),
		"non-ASCII":              strings.Replace(canonical, "baseline", "baselin\u00e9", 1),
		"noncanonical timestamp": strings.Replace(canonical, "2026-01-02T03:04:05.123456789Z", "2026-01-02T03:04:05.123456789+00:00", 1),
		"fraction trailing zero": strings.Replace(canonical, "2026-01-02T03:04:05.123456789Z", "2026-01-02T03:04:05.120Z", 1),
		"leading whitespace":     " " + canonical,
		"trailing JSON":          canonical + "{}",
	}
	for name, candidate := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeOperationBody([]byte(candidate)); err == nil {
				t.Fatalf("accepted noncanonical candidate: %s", candidate)
			}
		})
	}
}

func TestCanonicalOperationRejectsOversize(t *testing.T) {
	oversize := []byte(`{"schema":"` + strings.Repeat("a", MaxOperationBodyBytes) + `"}`)
	if _, err := DecodeOperationBody(oversize); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("expected body-size rejection, got %v", err)
	}
}

func TestCallerCannotSelectCASPredecessor(t *testing.T) {
	canonical := string(mustOperationJSON(t, syntheticOperation()))
	withPredecessor := strings.TrimSuffix(canonical, "}") + `,"predecessor":{"kind":"empty"}}`
	if _, err := DecodeOperationBody([]byte(withPredecessor)); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("caller-selected predecessor was accepted: %v", err)
	}
}

func TestOperationIdentityExcludesFreshAuthEnvelope(t *testing.T) {
	body := mustOperationJSON(t, syntheticOperation())
	bodyDigest := mustOperationDigest(t, body)
	first, err := MarshalAuthEnvelope(syntheticAuth(bodyDigest, MethodAuthorize, strings.Repeat("1", 32), strings.Repeat("2", 32)))
	if err != nil {
		t.Fatal(err)
	}
	second, err := MarshalAuthEnvelope(syntheticAuth(bodyDigest, MethodAuthorize, strings.Repeat("3", 32), strings.Repeat("4", 32)))
	if err != nil {
		t.Fatal(err)
	}
	firstDigest, _ := AuthEnvelopeDigest(first)
	secondDigest, _ := AuthEnvelopeDigest(second)
	if firstDigest == secondDigest {
		t.Fatal("fresh authentication envelopes must have distinct digests")
	}
	if got := mustOperationDigest(t, body); got != bodyDigest {
		t.Fatal("authentication envelope changed immutable operation digest")
	}
}

func TestAuthenticationEnvelopeAcceptsCanonicalGitHubUUIDJTIOnly(t *testing.T) {
	body := mustOperationJSON(t, syntheticOperation())
	bodyDigest := mustOperationDigest(t, body)
	valid := syntheticAuth(bodyDigest, MethodAuthorize, "00000000-0000-4000-8000-000000000001", strings.Repeat("2", 32))
	if _, err := MarshalAuthEnvelope(valid); err != nil {
		t.Fatalf("canonical GitHub-style UUID JTI rejected: %v", err)
	}
	for name, jti := range map[string]string{
		"uppercase": strings.TrimSuffix(valid.JTI, "1") + "A",
		"braces":    "{" + valid.JTI + "}",
		"short":     "11111111-2222-4333-8444-55555555555",
	} {
		t.Run(name, func(t *testing.T) {
			candidate := valid
			candidate.JTI = jti
			if _, err := MarshalAuthEnvelope(candidate); err == nil {
				t.Fatal("accepted noncanonical GitHub-style JTI")
			}
		})
	}
}

func TestOperationIdentitySeparatelyBindsControlRuntimeAndRuntimeArtifacts(t *testing.T) {
	base := syntheticOperation()
	baseDigest := mustOperationDigest(t, mustOperationJSON(t, base))
	mutations := map[string]func(*OperationBody){
		"control":             func(body *OperationBody) { body.ControlSHA = strings.Repeat("1", 40) },
		"runtime source":      func(body *OperationBody) { body.RuntimeSourceSHA = strings.Repeat("2", 40) },
		"workflow definition": func(body *OperationBody) { body.WorkflowDefinitionSHA256 = strings.Repeat("3", 64) },
		"config":              func(body *OperationBody) { body.ConfigSHA256 = strings.Repeat("4", 64) },
		"image":               func(body *OperationBody) { body.ImageSHA256 = strings.Repeat("5", 64) },
		"app spec":            func(body *OperationBody) { body.AppSpecSHA256 = strings.Repeat("6", 64) },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			candidate := base
			mutate(&candidate)
			if digest := mustOperationDigest(t, mustOperationJSON(t, candidate)); digest == baseDigest {
				t.Fatal("immutable identity field did not affect operation digest")
			}
		})
	}
}

func TestDomainSeparatedEd25519KnownAnswer(t *testing.T) {
	var seed [ed25519.SeedSize]byte
	for index := range seed {
		seed[index] = byte(index)
	}
	privateKey := ed25519.NewKeyFromSeed(seed[:])
	payload := []byte(`{"marker_sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","operation_id":"synthetic-operation-001"}`)
	message, err := signatureMessage(DomainWriterReceipt, payload)
	if err != nil {
		t.Fatal(err)
	}
	messageSHA256 := sha256.Sum256(message)
	const expectedMessageSHA256 = "d95bc4eeb1c883cfc65d0217cac27f2f47525a65a3c1714989d567ebf893df9a"
	if actual := hex.EncodeToString(messageSHA256[:]); actual != expectedMessageSHA256 {
		t.Fatalf("known-answer message mismatch: %s", actual)
	}
	signature, err := SignCanonical(privateKey, DomainWriterReceipt, payload)
	if err != nil {
		t.Fatal(err)
	}
	const expectedHex = "d39cfe5bc3588d3938ee8c755fc9c01f87cfd2966a3e35c5844490bc3ee98ddee5ac7c74ffb8e09655a14e7c156a748a1f3be44943406fef4409e89f33fab00e"
	if actual := hex.EncodeToString(signature); actual != expectedHex {
		t.Fatalf("known-answer mismatch: %s", actual)
	}
	publicKey := privateKey.Public().(ed25519.PublicKey)
	if err := VerifyCanonical(publicKey, DomainWriterReceipt, payload, signature); err != nil {
		t.Fatal(err)
	}
	if err := VerifyCanonical(publicKey, DomainObserverReceipt, payload, signature); err == nil {
		t.Fatal("writer signature verified in observer domain")
	}
	tampered := append([]byte(nil), payload...)
	tampered[len(tampered)-2] ^= 1
	if err := VerifyCanonical(publicKey, DomainWriterReceipt, tampered, signature); err == nil {
		t.Fatal("tampered payload verified")
	}
}

func TestCrossRoleAndKeyReuseRejected(t *testing.T) {
	writerSeed := [32]byte{1, 2, 3}
	observerSeed := [32]byte{4, 5, 6}
	writer := ed25519.NewKeyFromSeed(writerSeed[:])
	observer := ed25519.NewKeyFromSeed(observerSeed[:])
	payload := []byte(`{"synthetic":"receipt"}`)
	signature, err := SignCanonical(writer, DomainWriterReceipt, payload)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyCanonical(observer.Public().(ed25519.PublicKey), DomainWriterReceipt, payload, signature); err == nil {
		t.Fatal("writer signature verified with observer key")
	}
}
