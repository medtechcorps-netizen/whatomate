package protocol

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

const (
	MaxOIDCHeaderBytes      = 2048
	MaxOIDCClaimsBytes      = 8192
	MaxOIDCSignatureBytes   = 512
	MaxOIDCRawTokenBytes    = 16384
	MinimumOIDCRSAKeyBits   = 2048
	MaximumOIDCRSAKeyBits   = 4096
	maxOIDCHeaderEncoded    = (MaxOIDCHeaderBytes*8 + 5) / 6
	maxOIDCClaimsEncoded    = (MaxOIDCClaimsBytes*8 + 5) / 6
	maxOIDCSignatureEncoded = (MaxOIDCSignatureBytes*8 + 5) / 6
)

type OIDCExpectation struct {
	Role                 string
	AuthorityRootSHA256  [32]byte
	Issuer               string
	JWKSURI              string
	Audience             string
	Subject              string
	Actor                string
	ActorID              string
	BaseRef              string
	Repository           string
	RepositoryID         string
	RepositoryOwner      string
	RepositoryOwnerID    string
	RepositoryVisibility string
	Environment          string
	Ref                  string
	RefType              string
	HeadRef              string
	WorkflowRef          string
	WorkflowSHA          string
	Workflow             string
	SHA                  string
	EventName            string
	RunID                string
	RunNumber            string
	RunAttempt           string
	RunnerEnvironment    string
	MaximumTokenTTL      time.Duration
	MaximumClockSkew     time.Duration
	MaximumJWKSStale     time.Duration
	MinimumJWKSRefresh   time.Duration
}

func (e OIDCExpectation) validate() error {
	if err := e.verifierBinding().validate(); err != nil {
		return err
	}
	if e.Issuer == "" || e.JWKSURI == "" || e.Audience == "" || e.Subject == "" {
		return errors.New("OIDC issuer, JWKS URI, audience, and subject must be pinned")
	}
	issuer, err := url.Parse(e.Issuer)
	if err != nil || issuer.Scheme != "https" || issuer.Host == "" || issuer.User != nil || issuer.RawQuery != "" || issuer.Fragment != "" {
		return errors.New("OIDC issuer must be a pinned absolute HTTPS URI without userinfo, query, or fragment")
	}
	parsed, err := url.Parse(e.JWKSURI)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return errors.New("JWKS URI must be a pinned absolute HTTPS URI without userinfo, query, or fragment")
	}
	if e.Ref != "refs/heads/main" {
		return errors.New("OIDC expectation must bind refs/heads/main")
	}
	if e.RunAttempt != "1" {
		return errors.New("OIDC expectation must bind run_attempt 1")
	}
	if !gitCommitPattern.MatchString(e.WorkflowSHA) {
		return errors.New("OIDC expectation must bind an exact lowercase Git commit SHA")
	}
	for name, value := range map[string]string{
		"actor": e.Actor, "actor_id": e.ActorID,
		"repository": e.Repository, "repository_id": e.RepositoryID,
		"repository_owner": e.RepositoryOwner, "repository_owner_id": e.RepositoryOwnerID,
		"repository_visibility": e.RepositoryVisibility, "environment": e.Environment,
		"ref_type":     e.RefType,
		"workflow_ref": e.WorkflowRef, "workflow_sha": e.WorkflowSHA,
		"workflow": e.Workflow, "sha": e.SHA,
		"event_name": e.EventName, "run_id": e.RunID, "run_number": e.RunNumber,
		"runner_environment": e.RunnerEnvironment,
	} {
		if value == "" {
			return fmt.Errorf("OIDC expectation %s must be exact", name)
		}
		if err := validateCanonicalString(value); err != nil {
			return fmt.Errorf("OIDC expectation %s: %w", name, err)
		}
	}
	for name, value := range map[string]string{
		"actor_id": e.ActorID, "repository_id": e.RepositoryID,
		"repository_owner_id": e.RepositoryOwnerID, "run_id": e.RunID,
		"run_number": e.RunNumber, "run_attempt": e.RunAttempt,
	} {
		if !positiveDecimalPattern.MatchString(value) {
			return fmt.Errorf("OIDC expectation %s must be a canonical positive decimal", name)
		}
	}
	if e.RefType != "branch" || !validRepositoryVisibility(e.RepositoryVisibility) || e.SHA != e.WorkflowSHA {
		return errors.New("OIDC expectation must bind a recognized repository visibility and branch workflow at the exact control SHA")
	}
	for name, value := range map[string]string{"base_ref": e.BaseRef, "head_ref": e.HeadRef} {
		if err := validateCanonicalString(value); err != nil {
			return fmt.Errorf("OIDC expectation %s: %w", name, err)
		}
	}
	if e.MaximumTokenTTL <= 0 || e.MaximumClockSkew < 0 || e.MaximumJWKSStale <= 0 || e.MinimumJWKSRefresh < 0 {
		return errors.New("OIDC time and JWKS bounds are invalid")
	}
	return nil
}

func validRepositoryVisibility(value string) bool {
	return value == "private" || value == "internal" || value == "public"
}

type JWKSSnapshot struct {
	Keys      map[string]*rsa.PublicKey
	FetchedAt time.Time
}

// JWKSProviderGuarantees describe requirements for a future concrete
// transport boundary. They are not self-authenticating. Gate A intentionally
// supplies only a test-only, in-memory provider.
type JWKSProviderGuarantees struct {
	PinnedIssuerAndURI bool
	TrustedTransport   bool
	BoundedRefresh     bool
	TestOnly           bool
}

// JWKSProvider is a data-source abstraction only. Gate A intentionally
// contains no HTTP or DNS implementation.
type JWKSProvider interface {
	Guarantees() JWKSProviderGuarantees
	Snapshot(ctx context.Context, pinnedIssuer, pinnedJWKSURI string, forceRefresh bool) (JWKSSnapshot, error)
}

// RequestVerifierGuarantees describe future adapter requirements. A gateway's
// parsed claims cannot satisfy this interface: the verifier must consume the
// raw JWT and both canonical request documents itself. Gate A's production
// constructors remain unavailable regardless of these declarative values.
type RequestVerifierGuarantees struct {
	ValidatesRawJWT      bool
	ValidatesCanonical   bool
	PinsIssuerAndJWKSURI bool
	BoundedJWKSRefresh   bool
	TestOnly             bool
}

type verifiedRequest struct {
	body                     OperationBody
	bodyDigest               [32]byte
	envelope                 AuthEnvelope
	envelopeDigest           [32]byte
	identity                 OIDCIdentity
	authorizationDigest      [32]byte
	tokenNotBeforeUTC        string
	authorizationNotAfterUTC string
}

// RequestVerifierBinding is the immutable authority composition selected when
// a raw verifier is constructed. It deliberately contains only value fields so
// a Controller can snapshot it without retaining caller-owned mutable state.
// The Controller correlates the role and root digest with its own authority and
// every verified identity with the exact OIDC values below.
type RequestVerifierBinding struct {
	Role                string
	Audience            string
	Subject             string
	Environment         string
	WorkflowRef         string
	WorkflowSHA         string
	Workflow            string
	AuthorityRootSHA256 [32]byte
}

func (b RequestVerifierBinding) validate() error {
	if b.Role != RoleWriter && b.Role != RoleObserver {
		return errors.New("raw request verifier binding role must be writer or observer")
	}
	for name, value := range map[string]string{
		"audience": b.Audience, "subject": b.Subject, "environment": b.Environment,
		"workflow_ref": b.WorkflowRef, "workflow": b.Workflow,
	} {
		if value == "" {
			return fmt.Errorf("raw request verifier binding %s must be exact", name)
		}
		if err := validateCanonicalString(value); err != nil {
			return fmt.Errorf("raw request verifier binding %s: %w", name, err)
		}
	}
	if !gitCommitPattern.MatchString(b.WorkflowSHA) {
		return errors.New("raw request verifier binding workflow SHA must be an exact lowercase Git commit SHA")
	}
	if b.AuthorityRootSHA256 == ([32]byte{}) {
		return errors.New("raw request verifier binding requires an authority-root identity")
	}
	return nil
}

func (b RequestVerifierBinding) validateIdentity(identity OIDCIdentity) error {
	exact := []struct{ name, actual, wanted string }{
		{"audience", identity.Audience, b.Audience},
		{"subject", identity.Subject, b.Subject},
		{"environment", identity.Environment, b.Environment},
		{"workflow_ref", identity.WorkflowRef, b.WorkflowRef},
		{"workflow_sha", identity.WorkflowSHA, b.WorkflowSHA},
		{"workflow", identity.Workflow, b.Workflow},
	}
	for _, check := range exact {
		if check.actual != check.wanted {
			return fmt.Errorf("validated OIDC identity %s does not match immutable verifier binding", check.name)
		}
	}
	return nil
}

type RawRequestVerifier interface {
	Guarantees() RequestVerifierGuarantees
	Binding() RequestVerifierBinding
	VerifyRequest(ctx context.Context, rawJWT string, canonicalEnvelope, canonicalBody []byte, method string) (verifiedRequest, error)
}

type OIDCValidator struct {
	expectation OIDCExpectation
	binding     RequestVerifierBinding
	provider    JWKSProvider
	now         func() time.Time
	testOnly    bool
}

func NewOIDCValidator(expectation OIDCExpectation, provider JWKSProvider, now func() time.Time) (*OIDCValidator, error) {
	return nil, ErrGateAProductionUnavailable
}

// newTestOIDCValidator is an explicit test-only path. A validator constructed
// here is rejected by the production Controller constructor.
func newTestOIDCValidator(expectation OIDCExpectation, provider JWKSProvider, now func() time.Time) (*OIDCValidator, error) {
	if err := expectation.validate(); err != nil {
		return nil, err
	}
	if isNilInterface(provider) {
		return nil, errors.New("OIDC validator requires an explicit pinned JWKS provider")
	}
	if !provider.Guarantees().TestOnly {
		return nil, errors.New("test OIDC validator requires a test-only JWKS provider")
	}
	if now == nil {
		return nil, errors.New("OIDC validator requires an explicit clock")
	}
	return &OIDCValidator{expectation: expectation, binding: expectation.verifierBinding(), provider: provider, now: now, testOnly: true}, nil
}

func (v *OIDCValidator) Guarantees() RequestVerifierGuarantees {
	if v == nil {
		return RequestVerifierGuarantees{}
	}
	return RequestVerifierGuarantees{
		ValidatesRawJWT: true, ValidatesCanonical: true, PinsIssuerAndJWKSURI: true,
		BoundedJWKSRefresh: true, TestOnly: v.testOnly,
	}
}

func (e OIDCExpectation) verifierBinding() RequestVerifierBinding {
	return RequestVerifierBinding{
		Role: e.Role, Audience: e.Audience, Subject: e.Subject, Environment: e.Environment,
		WorkflowRef: e.WorkflowRef, WorkflowSHA: e.WorkflowSHA, Workflow: e.Workflow,
		AuthorityRootSHA256: e.AuthorityRootSHA256,
	}
}

func (v *OIDCValidator) Binding() RequestVerifierBinding {
	if v == nil {
		return RequestVerifierBinding{}
	}
	return v.binding
}

type OIDCIdentity struct {
	Issuer               string
	Audience             string
	Subject              string
	Actor                string
	ActorID              string
	BaseRef              string
	Repository           string
	RepositoryID         string
	RepositoryOwner      string
	RepositoryOwnerID    string
	RepositoryVisibility string
	Environment          string
	Ref                  string
	RefType              string
	HeadRef              string
	WorkflowRef          string
	WorkflowSHA          string
	Workflow             string
	SHA                  string
	EventName            string
	RunID                string
	RunNumber            string
	RunAttempt           string
	RunnerEnvironment    string
	JTI                  string
	IssuedAt             time.Time
	NotBefore            time.Time
	ExpiresAt            time.Time
}

type jwtHeader struct {
	Algorithm string `json:"alg"`
	KeyID     string `json:"kid"`
	Type      string `json:"typ"`
}

type jwtClaims struct {
	Issuer               string `json:"iss"`
	Audience             string `json:"aud"`
	Subject              string `json:"sub"`
	Actor                string `json:"actor"`
	ActorID              string `json:"actor_id"`
	BaseRef              string `json:"base_ref"`
	Repository           string `json:"repository"`
	RepositoryID         string `json:"repository_id"`
	RepositoryOwner      string `json:"repository_owner"`
	RepositoryOwnerID    string `json:"repository_owner_id"`
	RepositoryVisibility string `json:"repository_visibility"`
	Environment          string `json:"environment"`
	Ref                  string `json:"ref"`
	RefType              string `json:"ref_type"`
	HeadRef              string `json:"head_ref"`
	WorkflowRef          string `json:"workflow_ref"`
	WorkflowSHA          string `json:"workflow_sha"`
	Workflow             string `json:"workflow"`
	SHA                  string `json:"sha"`
	EventName            string `json:"event_name"`
	RunID                string `json:"run_id"`
	RunNumber            string `json:"run_number"`
	RunAttempt           string `json:"run_attempt"`
	RunnerEnvironment    string `json:"runner_environment"`
	IssuedAt             int64  `json:"iat"`
	NotBefore            int64  `json:"nbf"`
	ExpiresAt            int64  `json:"exp"`
	JTI                  string `json:"jti"`
}

func (v *OIDCValidator) Validate(ctx context.Context, rawToken string, canonicalEnvelope []byte) (OIDCIdentity, error) {
	if v == nil {
		return OIDCIdentity{}, errors.New("OIDC validator is nil")
	}
	envelope, err := DecodeAuthEnvelope(canonicalEnvelope)
	if err != nil {
		return OIDCIdentity{}, err
	}
	return v.validateRawToken(ctx, rawToken, envelope)
}

func (v *OIDCValidator) VerifyRequest(ctx context.Context, rawToken string, canonicalEnvelope, canonicalBody []byte, method string) (verifiedRequest, error) {
	if v == nil {
		return verifiedRequest{}, errors.New("OIDC validator is nil")
	}
	body, err := DecodeOperationBody(canonicalBody)
	if err != nil {
		return verifiedRequest{}, err
	}
	bodyDigest, err := OperationBodyDigest(canonicalBody)
	if err != nil {
		return verifiedRequest{}, err
	}
	envelope, err := DecodeAuthEnvelope(canonicalEnvelope)
	if err != nil {
		return verifiedRequest{}, err
	}
	if envelope.Method != method {
		return verifiedRequest{}, errors.New("authentication method does not match authority endpoint")
	}
	if envelope.OperationBodySHA256 != SHA256Hex(bodyDigest) {
		return verifiedRequest{}, errors.New("authentication envelope does not bind the canonical operation body")
	}
	identity, err := v.validateRawToken(ctx, rawToken, envelope)
	if err != nil {
		return verifiedRequest{}, err
	}
	if identity.WorkflowSHA != body.ControlSHA {
		return verifiedRequest{}, errors.New("OIDC workflow_sha does not bind the immutable control SHA")
	}
	envelopeDigest, err := AuthEnvelopeDigest(canonicalEnvelope)
	if err != nil {
		return verifiedRequest{}, err
	}
	return verifiedRequest{body: body, bodyDigest: bodyDigest, envelope: envelope, envelopeDigest: envelopeDigest, identity: identity}, nil
}

func (v *OIDCValidator) validateRawToken(ctx context.Context, rawToken string, envelope AuthEnvelope) (OIDCIdentity, error) {
	header, claims, signingInput, signature, err := parseStrictJWT(rawToken)
	if err != nil {
		return OIDCIdentity{}, err
	}
	if header.Algorithm != "RS256" {
		return OIDCIdentity{}, fmt.Errorf("OIDC algorithm must be RS256, got %q", header.Algorithm)
	}
	if header.Type != "JWT" || header.KeyID == "" {
		return OIDCIdentity{}, errors.New("OIDC header must contain exact typ JWT and a nonempty kid")
	}
	if err := validateCanonicalString(header.KeyID); err != nil {
		return OIDCIdentity{}, fmt.Errorf("OIDC kid: %w", err)
	}
	if err := v.validateClaims(claims, envelope); err != nil {
		return OIDCIdentity{}, err
	}
	key, err := v.resolveKey(ctx, header.KeyID)
	if err != nil {
		return OIDCIdentity{}, err
	}
	if key.N == nil || key.N.Sign() <= 0 || key.N.Bit(0) == 0 || key.N.BitLen() < MinimumOIDCRSAKeyBits || key.N.BitLen() > MaximumOIDCRSAKeyBits || key.E != 65537 {
		return OIDCIdentity{}, errors.New("OIDC RSA key does not meet pinned RS256 strength/profile")
	}
	if len(signature) != key.Size() {
		return OIDCIdentity{}, errors.New("OIDC signature length does not match the pinned RSA key")
	}
	digest := sha256.Sum256([]byte(signingInput))
	if err := rsa.VerifyPKCS1v15(key, crypto.SHA256, digest[:], signature); err != nil {
		return OIDCIdentity{}, errors.New("OIDC RS256 signature verification failed")
	}
	identity := OIDCIdentity{
		Issuer: claims.Issuer, Audience: claims.Audience, Subject: claims.Subject,
		Actor: claims.Actor, ActorID: claims.ActorID, BaseRef: claims.BaseRef,
		Repository: claims.Repository, RepositoryID: claims.RepositoryID,
		RepositoryOwner: claims.RepositoryOwner, RepositoryOwnerID: claims.RepositoryOwnerID,
		RepositoryVisibility: claims.RepositoryVisibility, Environment: claims.Environment,
		Ref: claims.Ref, RefType: claims.RefType, HeadRef: claims.HeadRef,
		WorkflowRef: claims.WorkflowRef, WorkflowSHA: claims.WorkflowSHA, Workflow: claims.Workflow,
		SHA: claims.SHA, EventName: claims.EventName, RunID: claims.RunID, RunNumber: claims.RunNumber,
		RunAttempt: claims.RunAttempt, RunnerEnvironment: claims.RunnerEnvironment, JTI: claims.JTI,
		IssuedAt: time.Unix(claims.IssuedAt, 0).UTC(), NotBefore: time.Unix(claims.NotBefore, 0).UTC(),
		ExpiresAt: time.Unix(claims.ExpiresAt, 0).UTC(),
	}
	return identity, nil
}

func (v *OIDCValidator) validateClaims(claims jwtClaims, envelope AuthEnvelope) error {
	expected := v.expectation
	exact := []struct{ name, actual, wanted string }{
		{"iss", claims.Issuer, expected.Issuer}, {"aud", claims.Audience, expected.Audience},
		{"sub", claims.Subject, expected.Subject}, {"repository", claims.Repository, expected.Repository},
		{"actor", claims.Actor, expected.Actor}, {"actor_id", claims.ActorID, expected.ActorID},
		{"base_ref", claims.BaseRef, expected.BaseRef}, {"head_ref", claims.HeadRef, expected.HeadRef},
		{"repository_id", claims.RepositoryID, expected.RepositoryID},
		{"repository_owner", claims.RepositoryOwner, expected.RepositoryOwner},
		{"repository_owner_id", claims.RepositoryOwnerID, expected.RepositoryOwnerID},
		{"repository_visibility", claims.RepositoryVisibility, expected.RepositoryVisibility},
		{"environment", claims.Environment, expected.Environment}, {"ref", claims.Ref, expected.Ref},
		{"ref_type", claims.RefType, expected.RefType},
		{"workflow_ref", claims.WorkflowRef, expected.WorkflowRef}, {"workflow_sha", claims.WorkflowSHA, expected.WorkflowSHA},
		{"workflow", claims.Workflow, expected.Workflow}, {"sha", claims.SHA, expected.SHA},
		{"event_name", claims.EventName, expected.EventName}, {"run_id", claims.RunID, expected.RunID},
		{"run_number", claims.RunNumber, expected.RunNumber},
		{"run_attempt", claims.RunAttempt, expected.RunAttempt},
		{"runner_environment", claims.RunnerEnvironment, expected.RunnerEnvironment},
		{"jti", claims.JTI, envelope.JTI},
	}
	for _, check := range exact {
		if check.actual != check.wanted {
			return fmt.Errorf("OIDC claim %s does not match exact expectation", check.name)
		}
	}
	for _, decimal := range []struct{ name, value string }{{"actor_id", claims.ActorID}, {"repository_id", claims.RepositoryID}, {"repository_owner_id", claims.RepositoryOwnerID}, {"run_id", claims.RunID}, {"run_number", claims.RunNumber}, {"run_attempt", claims.RunAttempt}} {
		if !positiveDecimalPattern.MatchString(decimal.value) {
			return fmt.Errorf("OIDC claim %s is not a canonical positive decimal", decimal.name)
		}
	}
	if claims.RunAttempt != "1" {
		return errors.New("OIDC run_attempt must equal 1")
	}
	now := v.now().UTC()
	iat := time.Unix(claims.IssuedAt, 0).UTC()
	nbf := time.Unix(claims.NotBefore, 0).UTC()
	exp := time.Unix(claims.ExpiresAt, 0).UTC()
	if claims.IssuedAt <= 0 || claims.NotBefore <= 0 || claims.ExpiresAt <= 0 || exp.Sub(iat) <= 0 || exp.Sub(iat) > expected.MaximumTokenTTL {
		return errors.New("OIDC token lifetime is invalid")
	}
	if nbf.Before(iat.Add(-expected.MaximumClockSkew)) || !nbf.Before(exp) {
		return errors.New("OIDC nbf is outside the token lifetime")
	}
	if iat.After(now.Add(expected.MaximumClockSkew)) || nbf.After(now.Add(expected.MaximumClockSkew)) {
		return errors.New("OIDC token is not yet valid")
	}
	// Expiry is exclusive. At exp with zero skew, the token is expired.
	if !now.Add(-expected.MaximumClockSkew).Before(exp) {
		return errors.New("OIDC token is expired")
	}
	issuedAt, err := parseCanonicalUTC(envelope.IssuedAtUTC)
	if err != nil {
		return err
	}
	if issuedAt.Before(iat) || issuedAt.After(now.Add(expected.MaximumClockSkew)) {
		return errors.New("authentication envelope timestamp is outside validated OIDC time")
	}
	return nil
}

func (v *OIDCValidator) resolveKey(ctx context.Context, keyID string) (*rsa.PublicKey, error) {
	now := v.now().UTC()
	snapshot, err := v.provider.Snapshot(ctx, v.expectation.Issuer, v.expectation.JWKSURI, false)
	if err != nil {
		return nil, fmt.Errorf("read pinned JWKS snapshot: %w", err)
	}
	key := snapshot.Keys[keyID]
	stale := snapshot.FetchedAt.IsZero() || now.Sub(snapshot.FetchedAt) > v.expectation.MaximumJWKSStale || snapshot.FetchedAt.After(now.Add(v.expectation.MaximumClockSkew))
	canRefresh := snapshot.FetchedAt.IsZero() || now.Sub(snapshot.FetchedAt) >= v.expectation.MinimumJWKSRefresh
	if (key == nil || stale) && canRefresh {
		snapshot, err = v.provider.Snapshot(ctx, v.expectation.Issuer, v.expectation.JWKSURI, true)
		if err != nil {
			return nil, fmt.Errorf("refresh pinned JWKS snapshot: %w", err)
		}
		key = snapshot.Keys[keyID]
		stale = snapshot.FetchedAt.IsZero() || now.Sub(snapshot.FetchedAt) > v.expectation.MaximumJWKSStale || snapshot.FetchedAt.After(now.Add(v.expectation.MaximumClockSkew))
	}
	if stale {
		return nil, errors.New("pinned JWKS snapshot is stale")
	}
	if key == nil {
		return nil, errors.New("OIDC kid not found in pinned JWKS snapshot")
	}
	return key, nil
}

func parseStrictJWT(raw string) (jwtHeader, jwtClaims, string, []byte, error) {
	if len(raw) == 0 || len(raw) > MaxOIDCRawTokenBytes {
		return jwtHeader{}, jwtClaims{}, "", nil, errors.New("OIDC token is empty or oversized")
	}
	parts := strings.Split(raw, ".")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return jwtHeader{}, jwtClaims{}, "", nil, errors.New("OIDC token must have exactly three nonempty segments")
	}
	encodedLimits := [...]int{maxOIDCHeaderEncoded, maxOIDCClaimsEncoded, maxOIDCSignatureEncoded}
	decoded := make([][]byte, 3)
	for index, part := range parts {
		if len(part) > encodedLimits[index] {
			return jwtHeader{}, jwtClaims{}, "", nil, errors.New("OIDC token segment is oversized")
		}
		if strings.Contains(part, "=") {
			return jwtHeader{}, jwtClaims{}, "", nil, errors.New("OIDC token segments must use unpadded base64url")
		}
		value, err := base64.RawURLEncoding.DecodeString(part)
		if err != nil || base64.RawURLEncoding.EncodeToString(value) != part {
			return jwtHeader{}, jwtClaims{}, "", nil, errors.New("OIDC token segment is not canonical base64url")
		}
		decoded[index] = value
	}
	var header jwtHeader
	if err := decodeStrictJWTObject(decoded[0], MaxOIDCHeaderBytes, &header); err != nil {
		return jwtHeader{}, jwtClaims{}, "", nil, fmt.Errorf("OIDC header: %w", err)
	}
	var claims jwtClaims
	if err := decodeStrictJWTObject(decoded[1], MaxOIDCClaimsBytes, &claims); err != nil {
		return jwtHeader{}, jwtClaims{}, "", nil, fmt.Errorf("OIDC claims: %w", err)
	}
	return header, claims, parts[0] + "." + parts[1], decoded[2], nil
}

func decodeStrictJWTObject(raw []byte, limit int, dst any) error {
	if len(raw) == 0 || len(raw) > limit {
		return errors.New("JWT JSON object is empty or oversized")
	}
	if err := inspectJSON(raw); err != nil {
		return err
	}
	if err := validateFlatJWTObjectKeyEncodings(raw); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	decoder.UseNumber()
	if err := decoder.Decode(dst); err != nil {
		return err
	}
	if err := requireJSONEOF(decoder); err != nil {
		return err
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, raw); err != nil || !bytes.Equal(raw, compact.Bytes()) {
		return errors.New("JWT JSON encoding contains noncanonical whitespace")
	}
	canonical, err := json.Marshal(dst)
	if err != nil {
		return fmt.Errorf("encode canonical JWT JSON: %w", err)
	}
	var actualFields, canonicalFields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &actualFields); err != nil {
		return fmt.Errorf("decode JWT field encodings: %w", err)
	}
	if err := json.Unmarshal(canonical, &canonicalFields); err != nil {
		return fmt.Errorf("decode canonical JWT field encodings: %w", err)
	}
	if len(actualFields) != len(canonicalFields) {
		return errors.New("JWT JSON field inventory is not exact")
	}
	for key, expected := range canonicalFields {
		actual, ok := actualFields[key]
		if !ok || !bytes.Equal(actual, expected) {
			return fmt.Errorf("JWT JSON field %q is not canonically encoded", key)
		}
	}
	return nil
}

// GitHub's issuer does not promise JSON object field order. JWT header and
// claim objects are therefore compared as exact unordered field inventories,
// while every key and scalar value must still use its one canonical encoding.
// The reviewed direct-workflow claim set is flat; nested objects or arrays are
// not part of this Gate-A model.
func validateFlatJWTObjectKeyEncodings(raw []byte) error {
	if len(raw) < 2 || raw[0] != '{' || raw[len(raw)-1] != '}' {
		return errors.New("JWT JSON must be a canonically encoded object")
	}
	index := 1
	if index == len(raw)-1 {
		return nil
	}
	for index < len(raw)-1 {
		if raw[index] != '"' {
			return errors.New("JWT JSON object key is not canonically encoded")
		}
		end, err := scanJSONString(raw, index)
		if err != nil {
			return err
		}
		encodedKey := raw[index:end]
		var key string
		if err := json.Unmarshal(encodedKey, &key); err != nil {
			return fmt.Errorf("decode JWT JSON object key: %w", err)
		}
		canonicalKey, err := json.Marshal(key)
		if err != nil || !bytes.Equal(encodedKey, canonicalKey) {
			return errors.New("JWT JSON object key is not canonically encoded")
		}
		index = end
		if index >= len(raw)-1 || raw[index] != ':' {
			return errors.New("JWT JSON object key is not followed by a canonical colon")
		}
		index++
		if index >= len(raw)-1 {
			return errors.New("JWT JSON object value is missing")
		}
		switch raw[index] {
		case '"':
			index, err = scanJSONString(raw, index)
			if err != nil {
				return err
			}
		case '{', '[':
			return errors.New("nested JWT JSON claims are outside the reviewed Gate-A claim set")
		default:
			for index < len(raw)-1 && raw[index] != ',' && raw[index] != '}' {
				index++
			}
		}
		if index == len(raw)-1 {
			return nil
		}
		if raw[index] != ',' {
			return errors.New("JWT JSON field separator is not canonical")
		}
		index++
	}
	return errors.New("JWT JSON object is malformed")
}

// scanJSONString returns the index immediately after a JSON string. Full JSON
// validity is checked by inspectJSON before this lexical canonicality pass.
func scanJSONString(raw []byte, start int) (int, error) {
	if start >= len(raw) || raw[start] != '"' {
		return 0, errors.New("JWT JSON string is malformed")
	}
	for index := start + 1; index < len(raw); index++ {
		switch raw[index] {
		case '\\':
			index++
			if index >= len(raw) {
				return 0, errors.New("JWT JSON escape is truncated")
			}
		case '"':
			return index + 1, nil
		}
	}
	return 0, errors.New("JWT JSON string is unterminated")
}

// RefProtectionEvidence is deliberately separate from OIDCIdentity because
// GitHub's OIDC token does not carry a ref_protected claim. Provider evidence
// must never claim that it observed branch protection.
type RefProtectionEvidence struct {
	Ref               string
	Protected         bool
	ExpectedMainSHA   string
	BeforeLiveMainSHA string
	AfterLiveMainSHA  string
	BeforeObservedAt  time.Time
	AfterObservedAt   time.Time
}

func ValidateRefProtection(evidence RefProtectionEvidence, maximumObservationSpan time.Duration) error {
	if evidence.Ref != "refs/heads/main" || !evidence.Protected {
		return errors.New("GitHub API/workflow guard did not independently prove protected main")
	}
	if !gitCommitPattern.MatchString(evidence.ExpectedMainSHA) || evidence.BeforeLiveMainSHA != evidence.ExpectedMainSHA || evidence.AfterLiveMainSHA != evidence.ExpectedMainSHA {
		return errors.New("live main did not remain at the exact expected SHA")
	}
	if evidence.BeforeObservedAt.IsZero() || evidence.AfterObservedAt.Before(evidence.BeforeObservedAt) || evidence.AfterObservedAt.Sub(evidence.BeforeObservedAt) > maximumObservationSpan {
		return errors.New("ref-protection observation window is invalid")
	}
	return nil
}

// BuildUnsignedJWT is intentionally absent. Tests construct synthetic tokens
// with a test-only helper so production code cannot accidentally mint OIDC.
