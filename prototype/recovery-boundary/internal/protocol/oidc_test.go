package protocol

import (
	"bytes"
	"context"
	"crypto"
	cryptorand "crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"sync"
	"testing"
	"time"
)

const syntheticRSAPrivateKeyPEM = `-----BEGIN PRIVATE KEY-----
MIIEvwIBADANBgkqhkiG9w0BAQEFAASCBKkwggSlAgEAAoIBAQDcUtuOTLKj8Hcj
G0M1vsEVUxBVnW3s4V2FPTkrwbKsMoMjuHxMmA+AdUMjkmX22YSwpT3+AEoaZPCU
S7hEJOc6AZU3zmQF3N07IjezxUoVsZfyaUUXWLHhTYFIWWi4DQ46tdZneU+Varkc
mEOogaAZ/VUqgdRq//QrEWkpE1/PRpyY0jF3650hiZymAovTb2D+y4waU+DpQUfv
N/S+QWahpFAXfurT02gMKxLLr0JzhxZH50u9XXwR5FFbwQo/SnwyQZnRwvnOIcB5
n0Aglrxm86Tgykk+NcJimIMZRVANZBCjXF67r6H5troPddakoidT80xiC4/ULkP9
uXgiBz4ZAgMBAAECggEBANixYaGGS9izo+lSYfsVPwBDLviVmsz1Jq7p9TXVD28P
Sy2xwAbxM6XrLvpofYKYk0nNa7hK/pcRGhEwm+3hwc2qSuGVS4j8nlYPpGtaKjBF
+CUCZmK86E6olPPchAMpTApwV4xzotNZIPE/zKOJwjZtk/r3sD0Aulw1hpFQrdXE
UL1j4c0HddEZxl1J/jDrjTApjogW/nTkxBGLeinKNprFPiIn1dcPZ1H74lEJVTVK
/kS+RnP+DIBYsLmF9Bt5Bi7mK4FDCIKtnkBfaIYa1e7LNvZla4s7bq5kucqUKTTr
nMwIFhCkYpLv2mCH5hUZW9QeWqHPTYKPqkLo1ftRGFUCgYEA5Qe+SNzMNfDmvaLT
xoQuIHTUn0wA0U2NDIrkRbzq0rnoCxMPdJTs4uaeHjPiSXq+WkMzioMthqDnMtNu
p1wk7J83jfOQnz1CS6kyKhaE2wlUY0Pn+L8E1IBGyzLIPW/V1Do04GuaodAqJ4eI
be5RzZffWJtsG0HEnpq73b/a6W8CgYEA9kSmJs5gKk51/FzTPg1AUHputwHtBwGP
UNVjrbagTT7u8rclvIImumSWcUdpf3T+DS4Mev7nluw2S3JqYF8d8dExqnQt+FEO
WYhzDk30ph9KZCnmSlB7E3z9OJKEMNTUY1+757/AN5jgX2wg92e+wSRyrEIFoOVe
HPRgSGmGPPcCgYAf1FepoKXwyS4IJNzxteUDNblm+hUTAYgcuiDHYF3yM0wAXgHD
3f6d+hb3c5Z7R8e0m6pKEbj+ANagxamXMMMg72+1Fqh+uPDBux3xo3eLSVyk/wb6
FvIA5mLwUnppr2U0PXKjzdCLtHZnT/qx7HEJ9ZVgpxj7IMTGlhKN2t/9mQKBgQDz
WTK19gigxZdhIHi9QGrlG5Z70LNf0PLFdZdh+Ky+qAmGXeQ0Oof6d5sRpPdis0C3
1WEPyQMf55pfQ1hKkrMMWSMyxEsIrU/4uRS4dd/ip9ji0WR22sBDqaavWFi3yBd3
ewo7HwfZ6H8Oy9Jnp2SfhlyqSzM0onI1OmZKJ7w2UQKBgQCILgs443MCGAGhjrVp
ML6cfFubViGeiXOm4VDU+UnwCVN1hzot+QPNGK/rYiH8LDr0Vf+LP9E1SUcnVNj2
CgLv6mlRoi5BmlMur41TU1fcMb9Wo0qXwIDEbXUaqWT4owBh9u6cimIyEjUjmYTr
eQdIh3fU/qqmIkykSbJjAUCsLg==
-----END PRIVATE KEY-----`

type staticJWKSProvider struct {
	mu        sync.Mutex
	snapshots []JWKSSnapshot
	calls     []bool
	issuer    string
	uri       string
	prod      bool
}

func (p *staticJWKSProvider) Guarantees() JWKSProviderGuarantees {
	return JWKSProviderGuarantees{PinnedIssuerAndURI: true, TrustedTransport: p.prod, BoundedRefresh: true, TestOnly: !p.prod}
}

func (p *staticJWKSProvider) Snapshot(_ context.Context, issuer, uri string, force bool) (JWKSSnapshot, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if issuer != p.issuer || uri != p.uri {
		return JWKSSnapshot{}, errors.New("unpinned JWKS request")
	}
	p.calls = append(p.calls, force)
	index := len(p.calls) - 1
	if index >= len(p.snapshots) {
		index = len(p.snapshots) - 1
	}
	if index < 0 {
		return JWKSSnapshot{}, errors.New("no synthetic snapshot")
	}
	return p.snapshots[index], nil
}

func syntheticRSAKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	block, _ := pem.Decode([]byte(syntheticRSAPrivateKeyPEM))
	if block == nil {
		t.Fatal("decode synthetic RSA PEM")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	key, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		t.Fatal("synthetic key is not RSA")
	}
	return key
}

func oidcFixture(t *testing.T) (time.Time, OIDCExpectation, AuthEnvelope, jwtClaims, *rsa.PrivateKey) {
	t.Helper()
	now := time.Date(2026, 1, 2, 3, 4, 6, 0, time.UTC)
	bodyDigest := sha256.Sum256([]byte("synthetic immutable body digest fixture"))
	envelope := syntheticAuth(bodyDigest, MethodAuthorize, strings.Repeat("1", 32), strings.Repeat("2", 32))
	envelope.JTI = "00000000-0000-4000-8000-000000000001"
	expectation := OIDCExpectation{
		Role: RoleWriter, AuthorityRootSHA256: sha256.Sum256([]byte("synthetic-writer-authority-root")),
		Issuer: "https://issuer.synthetic.invalid", JWKSURI: "https://issuer.synthetic.invalid/.well-known/jwks",
		Audience: "urn:rereply:synthetic-writer", Subject: "repo:synthetic-owner/synthetic-repo:environment:synthetic-writer",
		Actor: "synthetic-actor", ActorID: "7007", BaseRef: "", HeadRef: "",
		Repository: "synthetic-owner/synthetic-repo", RepositoryID: "1001",
		RepositoryOwner: "synthetic-owner", RepositoryOwnerID: "2002", RepositoryVisibility: "private",
		Environment: "synthetic-writer", Ref: "refs/heads/main", RefType: "branch",
		WorkflowRef: "synthetic-owner/synthetic-repo/.github/workflows/synthetic.tmpl@refs/heads/main",
		WorkflowSHA: strings.Repeat("a", 40), Workflow: "synthetic-recovery-boundary", SHA: strings.Repeat("a", 40),
		EventName: "workflow_dispatch", RunID: "3003", RunNumber: "4004", RunAttempt: "1",
		RunnerEnvironment: "github-hosted", MaximumTokenTTL: 5 * time.Minute, MaximumClockSkew: 0,
		MaximumJWKSStale: time.Hour, MinimumJWKSRefresh: time.Second,
	}
	claims := jwtClaims{
		Issuer: expectation.Issuer, Audience: expectation.Audience, Subject: expectation.Subject,
		Actor: expectation.Actor, ActorID: expectation.ActorID, BaseRef: expectation.BaseRef, HeadRef: expectation.HeadRef,
		Repository: expectation.Repository, RepositoryID: expectation.RepositoryID,
		RepositoryOwner: expectation.RepositoryOwner, RepositoryOwnerID: expectation.RepositoryOwnerID,
		RepositoryVisibility: expectation.RepositoryVisibility,
		Environment:          expectation.Environment, Ref: expectation.Ref, RefType: expectation.RefType,
		WorkflowRef: expectation.WorkflowRef, WorkflowSHA: expectation.WorkflowSHA, Workflow: expectation.Workflow, SHA: expectation.SHA,
		EventName: expectation.EventName, RunID: expectation.RunID, RunNumber: expectation.RunNumber, RunAttempt: expectation.RunAttempt,
		RunnerEnvironment: expectation.RunnerEnvironment,
		IssuedAt:          now.Add(-time.Minute).Unix(), NotBefore: now.Add(-time.Minute).Unix(), ExpiresAt: now.Add(time.Minute).Unix(),
		JTI: envelope.JTI,
	}
	return now, expectation, envelope, claims, syntheticRSAKey(t)
}

func signSyntheticJWT(t *testing.T, key *rsa.PrivateKey, header any, claims any) string {
	t.Helper()
	headerJSON, err := json.Marshal(header)
	if err != nil {
		t.Fatal(err)
	}
	claimsJSON, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	encodedHeader := base64.RawURLEncoding.EncodeToString(headerJSON)
	encodedClaims := base64.RawURLEncoding.EncodeToString(claimsJSON)
	signingInput := encodedHeader + "." + encodedClaims
	digest := sha256.Sum256([]byte(signingInput))
	signature, err := rsa.SignPKCS1v15(nil, key, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(signature)
}

func signSyntheticJWTBytes(t *testing.T, key *rsa.PrivateKey, headerJSON, claimsJSON []byte) string {
	t.Helper()
	encodedHeader := base64.RawURLEncoding.EncodeToString(headerJSON)
	encodedClaims := base64.RawURLEncoding.EncodeToString(claimsJSON)
	signingInput := encodedHeader + "." + encodedClaims
	digest := sha256.Sum256([]byte(signingInput))
	signature, err := rsa.SignPKCS1v15(nil, key, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(signature)
}

func TestOIDCValidatesExactRS256ClaimsAndPinnedJWKS(t *testing.T) {
	now, expectation, envelope, claims, key := oidcFixture(t)
	provider := &staticJWKSProvider{issuer: expectation.Issuer, uri: expectation.JWKSURI, snapshots: []JWKSSnapshot{{Keys: map[string]*rsa.PublicKey{"synthetic-kid": &key.PublicKey}, FetchedAt: now}}}
	validator, err := newTestOIDCValidator(expectation, provider, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	authJSON, _ := MarshalAuthEnvelope(envelope)
	token := signSyntheticJWT(t, key, jwtHeader{Algorithm: "RS256", KeyID: "synthetic-kid", Type: "JWT"}, claims)
	identity, err := validator.Validate(context.Background(), token, authJSON)
	if err != nil {
		t.Fatal(err)
	}
	if identity.Ref != "refs/heads/main" || len(provider.calls) != 1 || provider.calls[0] {
		t.Fatalf("unexpected validated identity/provider trace: %#v %#v", identity, provider.calls)
	}
	freshEnvelope := envelope
	freshEnvelope.Challenge = strings.Repeat("3", 32)
	freshAuthJSON, _ := MarshalAuthEnvelope(freshEnvelope)
	if _, err := validator.Validate(context.Background(), token, freshAuthJSON); err != nil {
		t.Fatalf("JWT incorrectly carried caller challenge identity: %v", err)
	}
}

func TestOIDCRejectsAlgorithmsHeadersClaimsAndTamper(t *testing.T) {
	now, expectation, envelope, claims, key := oidcFixture(t)
	authJSON, _ := MarshalAuthEnvelope(envelope)
	newValidator := func() *OIDCValidator {
		provider := &staticJWKSProvider{issuer: expectation.Issuer, uri: expectation.JWKSURI, snapshots: []JWKSSnapshot{{Keys: map[string]*rsa.PublicKey{"synthetic-kid": &key.PublicKey}, FetchedAt: now}}}
		validator, err := newTestOIDCValidator(expectation, provider, func() time.Time { return now })
		if err != nil {
			t.Fatal(err)
		}
		return validator
	}
	tests := map[string]string{
		"none algorithm": signSyntheticJWT(t, key, map[string]any{"alg": "none", "kid": "synthetic-kid", "typ": "JWT"}, claims),
		"HS algorithm":   signSyntheticJWT(t, key, map[string]any{"alg": "HS256", "kid": "synthetic-kid", "typ": "JWT"}, claims),
		"jku header":     signSyntheticJWT(t, key, map[string]any{"alg": "RS256", "kid": "synthetic-kid", "typ": "JWT", "jku": "https://synthetic.invalid/jwks"}, claims),
		"x5u header":     signSyntheticJWT(t, key, map[string]any{"alg": "RS256", "kid": "synthetic-kid", "typ": "JWT", "x5u": "https://synthetic.invalid/cert"}, claims),
	}
	claimMapJSON, _ := json.Marshal(claims)
	var claimMap map[string]any
	_ = json.Unmarshal(claimMapJSON, &claimMap)
	claimMap["ref_protected"] = "true"
	tests["invented ref_protected claim"] = signSyntheticJWT(t, key, jwtHeader{Algorithm: "RS256", KeyID: "synthetic-kid", Type: "JWT"}, claimMap)
	delete(claimMap, "ref_protected")
	claimMap["challenge"] = envelope.Challenge
	tests["invented challenge claim"] = signSyntheticJWT(t, key, jwtHeader{Algorithm: "RS256", KeyID: "synthetic-kid", Type: "JWT"}, claimMap)
	wrongClaims := claims
	wrongClaims.WorkflowSHA = strings.Repeat("b", 40)
	tests["workflow SHA drift"] = signSyntheticJWT(t, key, jwtHeader{Algorithm: "RS256", KeyID: "synthetic-kid", Type: "JWT"}, wrongClaims)
	zeroIDClaims := claims
	zeroIDClaims.RepositoryID = "0"
	tests["zero repository ID"] = signSyntheticJWT(t, key, jwtHeader{Algorithm: "RS256", KeyID: "synthetic-kid", Type: "JWT"}, zeroIDClaims)
	valid := signSyntheticJWT(t, key, jwtHeader{Algorithm: "RS256", KeyID: "synthetic-kid", Type: "JWT"}, claims)
	parts := strings.Split(valid, ".")
	signature, _ := base64.RawURLEncoding.DecodeString(parts[2])
	signature[0] ^= 1
	tests["signature tamper"] = parts[0] + "." + parts[1] + "." + base64.RawURLEncoding.EncodeToString(signature)
	for name, token := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := newValidator().Validate(context.Background(), token, authJSON); err == nil {
				t.Fatal("accepted invalid OIDC token")
			}
		})
	}
}

func TestOIDCRejectsOversizedRawSegmentsAndMalformedRSAProfile(t *testing.T) {
	now, expectation, envelope, claims, key := oidcFixture(t)
	authJSON, _ := MarshalAuthEnvelope(envelope)
	newValidator := func(publicKey *rsa.PublicKey) *OIDCValidator {
		provider := &staticJWKSProvider{issuer: expectation.Issuer, uri: expectation.JWKSURI, snapshots: []JWKSSnapshot{{Keys: map[string]*rsa.PublicKey{"synthetic-kid": publicKey}, FetchedAt: now}}}
		validator, err := newTestOIDCValidator(expectation, provider, func() time.Time { return now })
		if err != nil {
			t.Fatal(err)
		}
		return validator
	}
	valid := signSyntheticJWT(t, key, jwtHeader{Algorithm: "RS256", KeyID: "synthetic-kid", Type: "JWT"}, claims)
	parts := strings.Split(valid, ".")
	tokens := map[string]string{
		"raw":       strings.Repeat("A", MaxOIDCRawTokenBytes+1),
		"many dots": strings.Repeat("a.", 1000) + "a",
		"header":    strings.Repeat("A", maxOIDCHeaderEncoded+1) + "." + parts[1] + "." + parts[2],
		"claims":    parts[0] + "." + strings.Repeat("A", maxOIDCClaimsEncoded+1) + "." + parts[2],
		"signature": parts[0] + "." + parts[1] + "." + strings.Repeat("A", maxOIDCSignatureEncoded+1),
	}
	for name, token := range tokens {
		t.Run(name, func(t *testing.T) {
			if _, err := newValidator(&key.PublicKey).Validate(context.Background(), token, authJSON); err == nil {
				t.Fatal("accepted oversized or structurally unbounded OIDC token")
			}
		})
	}
	for name, publicKey := range map[string]*rsa.PublicKey{
		"nil modulus":  {N: nil, E: 65537},
		"even modulus": {N: new(big.Int).Lsh(big.NewInt(1), 2048), E: 65537},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := newValidator(publicKey).Validate(context.Background(), valid, authJSON); err == nil {
				t.Fatal("accepted malformed RSA key profile")
			}
		})
	}
	decodedSignature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatal(err)
	}
	shortToken := parts[0] + "." + parts[1] + "." + base64.RawURLEncoding.EncodeToString(decodedSignature[:len(decodedSignature)-1])
	if _, err := newValidator(&key.PublicKey).Validate(context.Background(), shortToken, authJSON); err == nil || !strings.Contains(err.Error(), "signature length") {
		t.Fatalf("accepted non-exact RSA signature length: %v", err)
	}
}

func TestOIDCFractionalClockBoundariesAndBoundedRefresh(t *testing.T) {
	now, expectation, envelope, claims, key := oidcFixture(t)
	authJSON, _ := MarshalAuthEnvelope(envelope)
	header := jwtHeader{Algorithm: "RS256", KeyID: "synthetic-kid", Type: "JWT"}
	claims.NotBefore = now.Unix()
	claims.ExpiresAt = now.Add(time.Minute).Unix()
	token := signSyntheticJWT(t, key, header, claims)
	provider := &staticJWKSProvider{issuer: expectation.Issuer, uri: expectation.JWKSURI, snapshots: []JWKSSnapshot{
		{Keys: map[string]*rsa.PublicKey{}, FetchedAt: now.Add(-2 * time.Second)},
		{Keys: map[string]*rsa.PublicKey{"synthetic-kid": &key.PublicKey}, FetchedAt: now},
	}}
	validator, _ := newTestOIDCValidator(expectation, provider, func() time.Time { return now })
	if _, err := validator.Validate(context.Background(), token, authJSON); err != nil {
		t.Fatalf("nbf inclusive boundary rejected: %v", err)
	}
	if len(provider.calls) != 2 || provider.calls[0] || !provider.calls[1] {
		t.Fatalf("expected one bounded forced refresh, got %#v", provider.calls)
	}
	expiredValidator, _ := newTestOIDCValidator(expectation, &staticJWKSProvider{issuer: expectation.Issuer, uri: expectation.JWKSURI, snapshots: []JWKSSnapshot{{Keys: map[string]*rsa.PublicKey{"synthetic-kid": &key.PublicKey}, FetchedAt: now.Add(time.Minute)}}}, func() time.Time { return now.Add(time.Minute) })
	if _, err := expiredValidator.Validate(context.Background(), token, authJSON); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("exclusive expiry boundary accepted: %v", err)
	}
	emptyLifetime := claims
	emptyLifetime.NotBefore = emptyLifetime.ExpiresAt
	skewedExpectation := expectation
	skewedExpectation.MaximumClockSkew = 2 * time.Minute
	emptyToken := signSyntheticJWT(t, key, header, emptyLifetime)
	emptyValidator, _ := newTestOIDCValidator(skewedExpectation, &staticJWKSProvider{issuer: expectation.Issuer, uri: expectation.JWKSURI, snapshots: []JWKSSnapshot{{Keys: map[string]*rsa.PublicKey{"synthetic-kid": &key.PublicKey}, FetchedAt: now}}}, func() time.Time { return now })
	if _, err := emptyValidator.Validate(context.Background(), emptyToken, authJSON); err == nil || !strings.Contains(err.Error(), "lifetime") {
		t.Fatalf("empty nbf/exp validity interval accepted: %v", err)
	}
}

func TestProductionOIDCConstructorRejectsTestOnlyJWKSProvider(t *testing.T) {
	now, expectation, _, _, key := oidcFixture(t)
	provider := &staticJWKSProvider{issuer: expectation.Issuer, uri: expectation.JWKSURI, snapshots: []JWKSSnapshot{{Keys: map[string]*rsa.PublicKey{"synthetic-kid": &key.PublicKey}, FetchedAt: now}}}
	if _, err := NewOIDCValidator(expectation, provider, func() time.Time { return now }); !errors.Is(err, ErrGateAProductionUnavailable) {
		t.Fatalf("production OIDC constructor did not fail closed: %v", err)
	}
	provider.prod = true
	if _, err := NewOIDCValidator(expectation, provider, func() time.Time { return now }); !errors.Is(err, ErrGateAProductionUnavailable) {
		t.Fatalf("self-certified JWKS provider manufactured production authority: %v", err)
	}
}

func TestOIDCExpectationRejectsZeroIdentityIDs(t *testing.T) {
	now, expectation, _, _, key := oidcFixture(t)
	for name, mutate := range map[string]func(*OIDCExpectation){
		"repository":       func(value *OIDCExpectation) { value.RepositoryID = "0" },
		"repository owner": func(value *OIDCExpectation) { value.RepositoryOwnerID = "0" },
		"run":              func(value *OIDCExpectation) { value.RunID = "0" },
	} {
		t.Run(name, func(t *testing.T) {
			candidate := expectation
			mutate(&candidate)
			provider := &staticJWKSProvider{issuer: candidate.Issuer, uri: candidate.JWKSURI, snapshots: []JWKSSnapshot{{Keys: map[string]*rsa.PublicKey{"synthetic-kid": &key.PublicKey}, FetchedAt: now}}}
			if _, err := newTestOIDCValidator(candidate, provider, func() time.Time { return now }); err == nil || !strings.Contains(err.Error(), "positive decimal") {
				t.Fatalf("zero identity expectation accepted: %v", err)
			}
		})
	}
}

func TestOIDCRejectsSignedButNoncanonicalHeaderAndClaimsBytes(t *testing.T) {
	now, expectation, envelope, claims, key := oidcFixture(t)
	authJSON, _ := MarshalAuthEnvelope(envelope)
	newValidator := func() *OIDCValidator {
		provider := &staticJWKSProvider{issuer: expectation.Issuer, uri: expectation.JWKSURI, snapshots: []JWKSSnapshot{{Keys: map[string]*rsa.PublicKey{"synthetic-kid": &key.PublicKey}, FetchedAt: now}}}
		validator, err := newTestOIDCValidator(expectation, provider, func() time.Time { return now })
		if err != nil {
			t.Fatal(err)
		}
		return validator
	}
	canonicalHeader, _ := json.Marshal(jwtHeader{Algorithm: "RS256", KeyID: "synthetic-kid", Type: "JWT"})
	canonicalClaims, _ := json.Marshal(claims)
	claimWithEscapedASCII := bytes.Replace(canonicalClaims, []byte("https://issuer.synthetic.invalid"), []byte(`https:\/\/issuer.synthetic.invalid`), 1)
	tests := map[string]string{
		"header whitespace":    signSyntheticJWTBytes(t, key, append([]byte{' '}, canonicalHeader...), canonicalClaims),
		"claims whitespace":    signSyntheticJWTBytes(t, key, canonicalHeader, append([]byte{' '}, canonicalClaims...)),
		"claims escaped ASCII": signSyntheticJWTBytes(t, key, canonicalHeader, claimWithEscapedASCII),
	}
	for name, token := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := newValidator().Validate(context.Background(), token, authJSON); err == nil || !strings.Contains(err.Error(), "canonical") {
				t.Fatalf("accepted signed noncanonical JWT JSON: %v", err)
			}
		})
	}

	duplicateClaim := append(append([]byte(nil), canonicalClaims[:len(canonicalClaims)-1]...), []byte(`,"jti":"`+claims.JTI+`"}`)...)
	fractionalTime := bytes.Replace(canonicalClaims, []byte(`"iat":`+fmt.Sprint(claims.IssuedAt)), []byte(`"iat":1.0`), 1)
	exponentTime := bytes.Replace(canonicalClaims, []byte(`"iat":`+fmt.Sprint(claims.IssuedAt)), []byte(`"iat":1e3`), 1)
	escapedKey := bytes.Replace(canonicalClaims, []byte(`"iss"`), []byte(`"\u0069ss"`), 1)
	for name, claimsJSON := range map[string][]byte{
		"duplicate claim": duplicateClaim,
		"fractional time": fractionalTime,
		"exponent time":   exponentTime,
		"escaped key":     escapedKey,
	} {
		t.Run(name, func(t *testing.T) {
			token := signSyntheticJWTBytes(t, key, canonicalHeader, claimsJSON)
			if _, err := newValidator().Validate(context.Background(), token, authJSON); err == nil {
				t.Fatal("accepted ambiguous JWT claim encoding")
			}
		})
	}
}

func TestOIDCAcceptsArbitraryIssuerObjectFieldOrderWithoutWeakeningFieldEncoding(t *testing.T) {
	now, expectation, envelope, claims, key := oidcFixture(t)
	authJSON, _ := MarshalAuthEnvelope(envelope)
	provider := &staticJWKSProvider{issuer: expectation.Issuer, uri: expectation.JWKSURI, snapshots: []JWKSSnapshot{{Keys: map[string]*rsa.PublicKey{"synthetic-kid": &key.PublicKey}, FetchedAt: now}}}
	validator, err := newTestOIDCValidator(expectation, provider, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	header := []byte(`{"kid":"synthetic-kid","typ":"JWT","alg":"RS256"}`)
	canonicalClaims, _ := json.Marshal(claims)
	var claimMap map[string]any
	if err := json.Unmarshal(canonicalClaims, &claimMap); err != nil {
		t.Fatal(err)
	}
	reorderedClaims, err := json.Marshal(claimMap)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(reorderedClaims, canonicalClaims) {
		t.Fatal("test did not vary claims object field order")
	}
	token := signSyntheticJWTBytes(t, key, header, reorderedClaims)
	if _, err := validator.Validate(context.Background(), token, authJSON); err != nil {
		t.Fatalf("rejected exact claims solely because issuer JSON object field order varied: %v", err)
	}
}

func TestRawOIDCRequestVerificationBindsCanonicalBodyEnvelopeAndControlSHA(t *testing.T) {
	now, expectation, _, claims, key := oidcFixture(t)
	body := mustOperationJSON(t, syntheticOperation())
	bodyDigest := mustOperationDigest(t, body)
	envelope := syntheticAuth(bodyDigest, MethodAuthorize, claims.JTI, strings.Repeat("2", 32))
	authJSON, err := MarshalAuthEnvelope(envelope)
	if err != nil {
		t.Fatal(err)
	}
	expectation.WorkflowSHA = strings.Repeat("a", 40)
	claims.WorkflowSHA = expectation.WorkflowSHA
	provider := &staticJWKSProvider{issuer: expectation.Issuer, uri: expectation.JWKSURI, snapshots: []JWKSSnapshot{{Keys: map[string]*rsa.PublicKey{"synthetic-kid": &key.PublicKey}, FetchedAt: now}}}
	validator, err := newTestOIDCValidator(expectation, provider, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	token := signSyntheticJWT(t, key, jwtHeader{Algorithm: "RS256", KeyID: "synthetic-kid", Type: "JWT"}, claims)
	request, err := validator.VerifyRequest(context.Background(), token, authJSON, body, MethodAuthorize)
	if err != nil || request.bodyDigest != bodyDigest || request.identity.WorkflowSHA != syntheticOperation().ControlSHA {
		t.Fatalf("raw request binding failed: %#v %v", request, err)
	}
	drifted := syntheticOperation()
	drifted.ControlSHA = strings.Repeat("9", 40)
	driftedBody := mustOperationJSON(t, drifted)
	driftedDigest := mustOperationDigest(t, driftedBody)
	driftedEnvelope := syntheticAuth(driftedDigest, MethodAuthorize, claims.JTI, strings.Repeat("4", 32))
	driftedAuthJSON, _ := MarshalAuthEnvelope(driftedEnvelope)
	if _, err := validator.VerifyRequest(context.Background(), token, driftedAuthJSON, driftedBody, MethodAuthorize); err == nil || !strings.Contains(err.Error(), "control SHA") {
		t.Fatalf("accepted body/control drift from raw OIDC identity: %v", err)
	}
}

func TestOIDCVerifierBindingIsCanonicalAndImmutable(t *testing.T) {
	now, expectation, _, _, key := oidcFixture(t)
	provider := &staticJWKSProvider{
		issuer: expectation.Issuer, uri: expectation.JWKSURI,
		snapshots: []JWKSSnapshot{{Keys: map[string]*rsa.PublicKey{"synthetic-kid": &key.PublicKey}, FetchedAt: now}},
	}
	validator, err := newTestOIDCValidator(expectation, provider, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	want := expectation.verifierBinding()
	if got := validator.Binding(); got != want {
		t.Fatalf("validator binding does not exactly snapshot its expectation: got=%#v want=%#v", got, want)
	}

	expectation.Role = RoleObserver
	expectation.Audience = "urn:rereply:mutated"
	expectation.AuthorityRootSHA256 = sha256.Sum256([]byte("mutated-authority-root"))
	returned := validator.Binding()
	returned.Workflow = "mutated-workflow"
	if got := validator.Binding(); got != want {
		t.Fatalf("validator binding changed through caller-owned values: got=%#v want=%#v", got, want)
	}

	for name, mutate := range map[string]func(*OIDCExpectation){
		"role":           func(e *OIDCExpectation) { e.Role = "reader" },
		"audience":       func(e *OIDCExpectation) { e.Audience = "" },
		"subject":        func(e *OIDCExpectation) { e.Subject = "" },
		"environment":    func(e *OIDCExpectation) { e.Environment = "" },
		"workflow ref":   func(e *OIDCExpectation) { e.WorkflowRef = "" },
		"workflow SHA":   func(e *OIDCExpectation) { e.WorkflowSHA = strings.Repeat("A", 40) },
		"workflow":       func(e *OIDCExpectation) { e.Workflow = "" },
		"authority root": func(e *OIDCExpectation) { e.AuthorityRootSHA256 = [32]byte{} },
	} {
		t.Run(name, func(t *testing.T) {
			invalidNow, invalid, _, _, invalidKey := oidcFixture(t)
			invalidProvider := &staticJWKSProvider{
				issuer: invalid.Issuer, uri: invalid.JWKSURI,
				snapshots: []JWKSSnapshot{{Keys: map[string]*rsa.PublicKey{"synthetic-kid": &invalidKey.PublicKey}, FetchedAt: invalidNow}},
			}
			mutate(&invalid)
			if _, err := newTestOIDCValidator(invalid, invalidProvider, func() time.Time { return now }); err == nil {
				t.Fatal("accepted a noncanonical verifier binding")
			}
		})
	}
}

func TestWriterAndObserverOIDCCompositionUsesDistinctRolesAudiencesAndKeys(t *testing.T) {
	now, writerExpectation, _, writerClaims, writerKey := oidcFixture(t)
	observerKey, err := rsa.GenerateKey(cryptorand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	if writerKey.N.Cmp(observerKey.N) == 0 {
		t.Fatal("writer and observer unexpectedly reused one RSA trust root")
	}

	writerBody := mustOperationJSON(t, syntheticOperation())
	writerDigest := mustOperationDigest(t, writerBody)
	writerEnvelope := syntheticAuth(writerDigest, MethodAuthorize, writerClaims.JTI, strings.Repeat("2", 64))
	writerEnvelopeJSON, err := MarshalAuthEnvelope(writerEnvelope)
	if err != nil {
		t.Fatal(err)
	}
	writerProvider := &staticJWKSProvider{
		issuer: writerExpectation.Issuer, uri: writerExpectation.JWKSURI,
		snapshots: []JWKSSnapshot{{Keys: map[string]*rsa.PublicKey{"synthetic-writer-kid": &writerKey.PublicKey}, FetchedAt: now}},
	}
	writerValidator, err := newTestOIDCValidator(writerExpectation, writerProvider, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	writerToken := signSyntheticJWT(t, writerKey, jwtHeader{Algorithm: "RS256", KeyID: "synthetic-writer-kid", Type: "JWT"}, writerClaims)

	observerExpectation := writerExpectation
	observerExpectation.Role = RoleObserver
	observerExpectation.AuthorityRootSHA256 = sha256.Sum256([]byte("synthetic-observer-authority-root"))
	observerExpectation.Audience = "urn:rereply:synthetic-observer"
	observerExpectation.Subject = "repo:synthetic-owner/synthetic-repo:environment:synthetic-observer"
	observerExpectation.Environment = "synthetic-observer"
	observerExpectation.WorkflowRef = "synthetic-owner/synthetic-repo/.github/workflows/synthetic-observer.tmpl@refs/heads/main"
	observerExpectation.Workflow = "synthetic-recovery-observer"
	observerExpectation.RunID = "3004"
	observerExpectation.RunNumber = "4005"
	observerClaims := writerClaims
	observerClaims.Audience = observerExpectation.Audience
	observerClaims.Subject = observerExpectation.Subject
	observerClaims.Environment = observerExpectation.Environment
	observerClaims.WorkflowRef = observerExpectation.WorkflowRef
	observerClaims.Workflow = observerExpectation.Workflow
	observerClaims.RunID = observerExpectation.RunID
	observerClaims.RunNumber = observerExpectation.RunNumber
	observerClaims.JTI = "00000000-0000-4000-8000-000000000002"
	observerBodyValue := syntheticOperation()
	observerBodyValue.OperationID = "synthetic-observer-operation-001"
	observerBodyValue.Role = RoleObserver
	observerBodyValue.Action = ActionMarkerRead
	observerBody := mustOperationJSON(t, observerBodyValue)
	observerDigest := mustOperationDigest(t, observerBody)
	observerEnvelope := syntheticAuth(observerDigest, MethodStatus, observerClaims.JTI, strings.Repeat("3", 64))
	observerEnvelopeJSON, err := MarshalAuthEnvelope(observerEnvelope)
	if err != nil {
		t.Fatal(err)
	}
	observerProvider := &staticJWKSProvider{
		issuer: observerExpectation.Issuer, uri: observerExpectation.JWKSURI,
		snapshots: []JWKSSnapshot{{Keys: map[string]*rsa.PublicKey{"synthetic-observer-kid": &observerKey.PublicKey}, FetchedAt: now}},
	}
	observerValidator, err := newTestOIDCValidator(observerExpectation, observerProvider, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	observerToken := signSyntheticJWT(t, observerKey, jwtHeader{Algorithm: "RS256", KeyID: "synthetic-observer-kid", Type: "JWT"}, observerClaims)
	if writerValidator.Binding().Role != RoleWriter || observerValidator.Binding().Role != RoleObserver ||
		writerValidator.Binding().AuthorityRootSHA256 == observerValidator.Binding().AuthorityRootSHA256 {
		t.Fatal("writer and observer validators did not retain distinct authority bindings")
	}

	writerRequest, err := writerValidator.VerifyRequest(context.Background(), writerToken, writerEnvelopeJSON, writerBody, MethodAuthorize)
	if err != nil || writerRequest.body.Role != RoleWriter || writerRequest.body.Action != ActionMarkerCAS || writerRequest.identity.Audience != writerExpectation.Audience {
		t.Fatalf("writer raw JWT/canonical request composition failed: %#v err=%v", writerRequest, err)
	}
	observerRequest, err := observerValidator.VerifyRequest(context.Background(), observerToken, observerEnvelopeJSON, observerBody, MethodStatus)
	if err != nil || observerRequest.body.Role != RoleObserver || observerRequest.body.Action != ActionMarkerRead || observerRequest.identity.Audience != observerExpectation.Audience {
		t.Fatalf("observer raw JWT/canonical request composition failed: %#v err=%v", observerRequest, err)
	}

	for name, attempt := range map[string]func() error{
		"writer token at observer authority": func() error {
			_, err := observerValidator.VerifyRequest(context.Background(), writerToken, observerEnvelopeJSON, observerBody, MethodStatus)
			return err
		},
		"observer token at writer authority": func() error {
			_, err := writerValidator.VerifyRequest(context.Background(), observerToken, writerEnvelopeJSON, writerBody, MethodAuthorize)
			return err
		},
		"writer envelope with observer body": func() error {
			_, err := writerValidator.VerifyRequest(context.Background(), writerToken, writerEnvelopeJSON, observerBody, MethodAuthorize)
			return err
		},
		"observer method substitution": func() error {
			_, err := observerValidator.VerifyRequest(context.Background(), observerToken, observerEnvelopeJSON, observerBody, MethodAuthorize)
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := attempt(); err == nil {
				t.Fatal("cross-role OIDC/canonical binding was accepted")
			}
		})
	}
}

func TestRefProtectionIsIndependentOfOIDC(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 4, 6, 0, time.UTC)
	evidence := RefProtectionEvidence{
		Ref: "refs/heads/main", Protected: true, ExpectedMainSHA: strings.Repeat("a", 40),
		BeforeLiveMainSHA: strings.Repeat("a", 40), AfterLiveMainSHA: strings.Repeat("a", 40),
		BeforeObservedAt: now, AfterObservedAt: now.Add(time.Second),
	}
	if err := ValidateRefProtection(evidence, 2*time.Second); err != nil {
		t.Fatal(err)
	}
	evidence.Protected = false
	if err := ValidateRefProtection(evidence, 2*time.Second); err == nil {
		t.Fatal("accepted unprotected ref")
	}
}
