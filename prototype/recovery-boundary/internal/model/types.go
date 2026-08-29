// Package model contains a deliberately synthetic, fault-injecting model of
// the recovery-boundary protocol. It is a Gate-A test oracle, not a production
// authority implementation.
package model

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
)

const (
	// ProtocolVersion is intentionally literal. A v1 input is never upgraded.
	ProtocolVersion = "recovery-boundary/v2"
	// FixedMarkerKey is the only key the synthetic broker model can address.
	FixedMarkerKey = "synthetic:rereply:recovery:sentinel:v2"
)

var (
	ErrInvalid             = errors.New("recovery-boundary model: invalid input")
	ErrAuthReplay          = errors.New("recovery-boundary model: authentication envelope reused")
	ErrChallengeNotIssued  = errors.New("recovery-boundary model: authentication challenge was not durably issued")
	ErrChallengeBinding    = errors.New("recovery-boundary model: authentication challenge binding mismatch")
	ErrQuarantined         = errors.New("recovery-boundary model: operation quarantined")
	ErrReconcileRequired   = errors.New("recovery-boundary model: GET-only reconciliation required")
	ErrNotObserved         = errors.New("recovery-boundary model: side effect not observed")
	ErrMultipleObserved    = errors.New("recovery-boundary model: multiple side effects observed")
	ErrSimulatedCrash      = errors.New("recovery-boundary model: simulated crash")
	ErrProductionAdapter   = errors.New("recovery-boundary model: production durable adapter requirements not met")
	ErrForkNotAuthorized   = errors.New("recovery-boundary model: writer terminal proof required before fork")
	ErrForkAlreadyIssued   = errors.New("recovery-boundary model: fork POST was already issued")
	ErrMarkerMismatch      = errors.New("recovery-boundary model: marker continuity mismatch")
	ErrRoleIsolation       = errors.New("recovery-boundary model: authority/broker role isolation failed")
	ErrPredecessorMismatch = errors.New("recovery-boundary model: fixed-key predecessor mismatch")
	ErrCleanupRequired     = errors.New("recovery-boundary model: terminal cleanup reconciliation required")
)

// ProviderAmbiguityKind identifies a transport outcome for which the caller
// cannot know whether the provider committed the mutation. It never grants a
// retry; the only legal continuation is the request's status-only method.
type ProviderAmbiguityKind string

const (
	ProviderAmbiguousHTTP408 ProviderAmbiguityKind = "http-408"
	ProviderAmbiguousEOF     ProviderAmbiguityKind = "eof"
	ProviderAmbiguous5xx     ProviderAmbiguityKind = "http-5xx"
)

// ProviderAmbiguityError is returned by a single-call provider adapter when
// the mutation outcome is unknown. The adapter must not retry internally.
type ProviderAmbiguityError struct{ Kind ProviderAmbiguityKind }

func (e *ProviderAmbiguityError) Error() string {
	if e == nil {
		return "recovery-boundary model: ambiguous provider mutation"
	}
	return "recovery-boundary model: ambiguous provider mutation: " + string(e.Kind)
}

func (e *ProviderAmbiguityError) valid() bool {
	return e != nil && (e.Kind == ProviderAmbiguousHTTP408 || e.Kind == ProviderAmbiguousEOF || e.Kind == ProviderAmbiguous5xx)
}

// Digest is the only public representation of protected runtime/provider
// identifiers, request bodies, marker material, and configuration values.
type Digest [sha256.Size]byte

func HashBytes(value []byte) Digest { return sha256.Sum256(value) }

func HashString(value string) Digest { return HashBytes([]byte(value)) }

func (d Digest) String() string { return hex.EncodeToString(d[:]) }

func (d Digest) IsZero() bool { return d == Digest{} }

// UnitRole names four separate substrate units. Authorities and brokers may
// not share an app/substrate identity.
type UnitRole string

const (
	WriterAuthorityRole   UnitRole = "writer-authority"
	WriterBrokerRole      UnitRole = "writer-broker"
	ObserverAuthorityRole UnitRole = "observer-authority"
	ObserverBrokerRole    UnitRole = "observer-broker"
)

type NetworkDomain string

const (
	NoDataDomain   NetworkDomain = "no-data"
	SourceDomain   NetworkDomain = "source-only"
	RecoveryDomain NetworkDomain = "recovery-only"
)

// SubstrateUnit retains its raw identifier privately. Receipts expose only
// IdentityDigest. There is intentionally no RawIdentifier accessor.
type SubstrateUnit struct {
	role           UnitRole
	domain         NetworkDomain
	rawIdentifier  string
	identityDigest Digest
}

func NewSubstrateUnit(role UnitRole, domain NetworkDomain, rawIdentifier string) (SubstrateUnit, error) {
	if strings.TrimSpace(rawIdentifier) == "" || strings.ContainsAny(rawIdentifier, "\r\n\x00") {
		return SubstrateUnit{}, fmt.Errorf("%w: protected unit identifier", ErrInvalid)
	}
	want := map[UnitRole]NetworkDomain{
		WriterAuthorityRole:   NoDataDomain,
		WriterBrokerRole:      SourceDomain,
		ObserverAuthorityRole: NoDataDomain,
		ObserverBrokerRole:    RecoveryDomain,
	}
	if want[role] != domain {
		return SubstrateUnit{}, fmt.Errorf("%w: role %q cannot use domain %q", ErrRoleIsolation, role, domain)
	}
	return SubstrateUnit{
		role:           role,
		domain:         domain,
		rawIdentifier:  rawIdentifier,
		identityDigest: HashString("unit/v2\x00" + string(role) + "\x00" + rawIdentifier),
	}, nil
}

func (u SubstrateUnit) Role() UnitRole                   { return u.role }
func (u SubstrateUnit) Domain() NetworkDomain            { return u.domain }
func (u SubstrateUnit) IdentityDigest() Digest           { return u.identityDigest }
func (u SubstrateUnit) isZero() bool                     { return u.rawIdentifier == "" }
func (u SubstrateUnit) sameRuntime(v SubstrateUnit) bool { return u.rawIdentifier == v.rawIdentifier }

func RequireFourDistinctUnits(units ...SubstrateUnit) error {
	if len(units) != 4 {
		return fmt.Errorf("%w: exactly four units required", ErrRoleIsolation)
	}
	roles := map[UnitRole]bool{}
	ids := map[string]bool{}
	for _, unit := range units {
		if unit.isZero() || roles[unit.role] || ids[unit.rawIdentifier] {
			return ErrRoleIsolation
		}
		roles[unit.role] = true
		ids[unit.rawIdentifier] = true
	}
	for _, role := range []UnitRole{WriterAuthorityRole, WriterBrokerRole, ObserverAuthorityRole, ObserverBrokerRole} {
		if !roles[role] {
			return ErrRoleIsolation
		}
	}
	return nil
}

type PredecessorKind string

const (
	PredecessorNil    PredecessorKind = "nil"
	PredecessorEmpty  PredecessorKind = "empty"
	PredecessorDigest PredecessorKind = "sha256"
)

// Predecessor makes a missing key and a present zero-length value distinct.
type Predecessor struct {
	Kind   PredecessorKind
	Digest Digest
}

func NilPredecessor() Predecessor   { return Predecessor{Kind: PredecessorNil} }
func EmptyPredecessor() Predecessor { return Predecessor{Kind: PredecessorEmpty} }
func DigestPredecessor(d Digest) Predecessor {
	return Predecessor{Kind: PredecessorDigest, Digest: d}
}

func (p Predecessor) validate() error {
	switch p.Kind {
	case PredecessorNil, PredecessorEmpty:
		if !p.Digest.IsZero() {
			return fmt.Errorf("%w: nil/empty predecessor cannot carry digest", ErrInvalid)
		}
	case PredecessorDigest:
		if p.Digest.IsZero() {
			return fmt.Errorf("%w: digest predecessor is empty", ErrInvalid)
		}
	default:
		return fmt.Errorf("%w: predecessor kind", ErrInvalid)
	}
	return nil
}

func (p Predecessor) digest() (Digest, error) {
	if err := p.validate(); err != nil {
		return Digest{}, err
	}
	payload := "protected-predecessor/v2\x00" + string(p.Kind)
	if p.Kind == PredecessorDigest {
		payload += "\x00" + p.Digest.String()
	}
	return HashString(payload), nil
}

// OperationBody is immutable caller-visible operation identity. Authentication
// timestamps, JWT IDs, challenges, and the protected fixed-key predecessor are
// deliberately absent. The source adapter, not the caller, supplies the exact
// nil/empty/digest predecessor used by the one-key CAS.
type OperationBody struct {
	Version      string
	OperationID  string
	Generation   uint64
	Phase        string
	ConfigDigest Digest
}

func (b OperationBody) Validate() error {
	if b.Version != ProtocolVersion || b.Generation == 0 || b.ConfigDigest.IsZero() {
		return ErrInvalid
	}
	if !validSyntheticToken(b.OperationID) || !validPhase(b.Phase) {
		return ErrInvalid
	}
	return nil
}

func validSyntheticToken(value string) bool {
	if len(value) < 3 || len(value) > 128 || !strings.HasPrefix(value, "synthetic-") {
		return false
	}
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			continue
		}
		return false
	}
	return true
}

func validPhase(value string) bool {
	switch value {
	case "baseline", "bridge", "backend", "ui":
		return true
	default:
		return false
	}
}

// CanonicalBytes is a fixed field-order, whitespace-free UTF-8 encoding.
func (b OperationBody) CanonicalBytes() ([]byte, error) {
	if err := b.Validate(); err != nil {
		return nil, err
	}
	canonical := `{"version":` + strconv.Quote(b.Version) +
		`,"operation_id":` + strconv.Quote(b.OperationID) +
		`,"generation":` + strconv.FormatUint(b.Generation, 10) +
		`,"phase":` + strconv.Quote(b.Phase) +
		`,"config_sha256":` + strconv.Quote(b.ConfigDigest.String()) + `}`
	return []byte(canonical), nil
}

func (b OperationBody) Digest() (Digest, error) {
	canonical, err := b.CanonicalBytes()
	if err != nil {
		return Digest{}, err
	}
	return HashBytes(canonical), nil
}

type CallKind string

const (
	MutationCall CallKind = "mutation"
	StatusCall   CallKind = "status"
)

func (c CallKind) validate() error {
	if c != MutationCall && c != StatusCall {
		return ErrInvalid
	}
	return nil
}

// AuthorityChallenge is an opaque, authority-issued 256-bit challenge. Its
// fields are intentionally private: a caller can carry a challenge but cannot
// choose or reconstruct its nonce through the public model API.
type AuthorityChallenge struct {
	state *authorityChallengeState
}

type authorityChallengeState struct {
	role      UnitRole
	authority Digest
	nonce     [32]byte
}

const (
	authorityChallengeRedaction = "[redacted-authority-challenge]"
	authEnvelopeRedaction       = "[redacted-auth-envelope]"
)

// String, GoString, and Format deliberately reveal no challenge material. The
// opaque value is a bearer authentication input and must remain redacted even
// when a caller accidentally uses a non-string fmt verb in a log or diagnostic.
func (AuthorityChallenge) String() string   { return authorityChallengeRedaction }
func (AuthorityChallenge) GoString() string { return authorityChallengeRedaction }
func (AuthorityChallenge) Format(state fmt.State, _ rune) {
	_, _ = io.WriteString(state, authorityChallengeRedaction)
}

func issueAuthorityChallenge(role UnitRole, authority Digest, random io.Reader) (AuthorityChallenge, error) {
	if (role != WriterAuthorityRole && role != ObserverAuthorityRole) || authority.IsZero() {
		return AuthorityChallenge{}, ErrInvalid
	}
	if random == nil {
		random = rand.Reader
	}
	state := &authorityChallengeState{role: role, authority: authority}
	if _, err := io.ReadFull(random, state.nonce[:]); err != nil || state.nonce == [32]byte{} {
		clear(state.nonce[:])
		return AuthorityChallenge{}, ErrInvalid
	}
	return AuthorityChallenge{state: state}, nil
}

func (c AuthorityChallenge) validate() error {
	if c.state == nil ||
		(c.state.role != WriterAuthorityRole && c.state.role != ObserverAuthorityRole) ||
		c.state.authority.IsZero() || c.state.nonce == [32]byte{} {
		return ErrInvalid
	}
	return nil
}

func (c AuthorityChallenge) Hash() Digest {
	if c.validate() != nil {
		return Digest{}
	}
	return HashBytes(append([]byte("authority-challenge/v2\x00"+string(c.state.role)+"\x00"+c.state.authority.String()+"\x00"), c.state.nonce[:]...))
}

func (c AuthorityChallenge) validateFor(role UnitRole, authority Digest) error {
	if c.validate() != nil || c.state.role != role || c.state.authority != authority {
		return ErrInvalid
	}
	return nil
}

// AuthEnvelope is fresh per call and is never part of OperationBody.Digest.
// The model stores only the JTI and opaque authority-challenge hashes.
type AuthEnvelope struct {
	jti       *authJTIState
	Challenge AuthorityChallenge
	IssuedAt  time.Time
	NotBefore time.Time
	ExpiresAt time.Time
}

type authJTIState struct {
	value string
}

func newAuthEnvelope(jti string, challenge AuthorityChallenge, issuedAt, notBefore, expiresAt time.Time) AuthEnvelope {
	return AuthEnvelope{
		jti:       &authJTIState{value: jti},
		Challenge: challenge,
		IssuedAt:  issuedAt,
		NotBefore: notBefore,
		ExpiresAt: expiresAt,
	}
}

func (a AuthEnvelope) jtiValue() string {
	if a.jti == nil {
		return ""
	}
	return a.jti.value
}

func (a AuthEnvelope) challengeValue() AuthorityChallenge { return a.Challenge }

func (a AuthEnvelope) withJTI(jti string) AuthEnvelope {
	a.jti = &authJTIState{value: jti}
	return a
}

func (a AuthEnvelope) withChallenge(challenge AuthorityChallenge) AuthEnvelope {
	a.Challenge = challenge
	return a
}

func (AuthEnvelope) String() string   { return authEnvelopeRedaction }
func (AuthEnvelope) GoString() string { return authEnvelopeRedaction }
func (AuthEnvelope) Format(state fmt.State, _ rune) {
	_, _ = io.WriteString(state, authEnvelopeRedaction)
}

func (a AuthEnvelope) Validate(now time.Time) error {
	if len(a.jtiValue()) < 16 || a.Challenge.validate() != nil {
		return ErrInvalid
	}
	if _, err := canonicalTime(a.IssuedAt); err != nil {
		return ErrInvalid
	}
	if _, err := canonicalTime(a.NotBefore); err != nil {
		return ErrInvalid
	}
	if _, err := canonicalTime(a.ExpiresAt); err != nil {
		return ErrInvalid
	}
	if _, err := canonicalTime(now); err != nil {
		return ErrInvalid
	}
	if a.IssuedAt.After(a.NotBefore) || a.ExpiresAt.Sub(a.IssuedAt) > 5*time.Minute {
		return ErrInvalid
	}
	// Expiry is exclusive. Fractional clocks are preserved by time.Time.
	if now.Before(a.NotBefore) || !now.Before(a.ExpiresAt) {
		return ErrInvalid
	}
	return nil
}

func (a AuthEnvelope) JTIHash() Digest       { return HashString("jti/v2\x00" + a.jtiValue()) }
func (a AuthEnvelope) ChallengeHash() Digest { return a.Challenge.Hash() }

// CanonicalBytes commits the fresh authentication envelope without retaining
// either bearer value. The digest is deliberately separate from OperationBody
// and can therefore change on an idempotent retry of the same operation.
func (a AuthEnvelope) CanonicalBytes() ([]byte, error) {
	issued, issuedErr := canonicalTime(a.IssuedAt)
	notBefore, notBeforeErr := canonicalTime(a.NotBefore)
	expires, expiresErr := canonicalTime(a.ExpiresAt)
	if len(a.jtiValue()) < 16 || a.Challenge.validate() != nil ||
		issuedErr != nil || notBeforeErr != nil || expiresErr != nil ||
		a.IssuedAt.After(a.NotBefore) || a.ExpiresAt.Sub(a.IssuedAt) > 5*time.Minute || !a.ExpiresAt.After(a.NotBefore) {
		return nil, ErrInvalid
	}
	return []byte("auth-envelope/v2\x00" + a.JTIHash().String() + "\x00" + a.ChallengeHash().String() + "\x00" + issued + "\x00" + notBefore + "\x00" + expires), nil
}

func (a AuthEnvelope) Digest() (Digest, error) {
	canonical, err := a.CanonicalBytes()
	if err != nil {
		return Digest{}, err
	}
	return HashBytes(canonical), nil
}

type Effect string

const (
	EffectAuthorityAppCreate Effect = "authority-app-create"
	EffectAuthorityAppUpdate Effect = "authority-app-update"
	EffectAuthorityAppDelete Effect = "authority-app-delete"
	EffectBrokerCreate       Effect = "broker-app-create"
	EffectBrokerUpdate       Effect = "broker-app-update"
	EffectFullRedeploy       Effect = "full-redeploy"
	EffectBrokerDelete       Effect = "broker-app-delete"
	EffectTrustedSourceAdd   Effect = "trusted-source-add"
	EffectTrustedSourceDel   Effect = "trusted-source-delete"
	EffectBindingInstall     Effect = "binding-install"
	EffectBindingRemove      Effect = "binding-remove"
	EffectCredentialInstall  Effect = "credential-install"
	EffectCredentialRemove   Effect = "credential-remove"
	EffectLeafIssue          Effect = "leaf-issue"
	EffectLeafRevoke         Effect = "leaf-revoke"
	EffectCapabilityIssue    Effect = "capability-issue"
	EffectCapabilityRevoke   Effect = "capability-revoke"
	EffectMTLSIssue          Effect = "mtls-issue"
	EffectMTLSRevoke         Effect = "mtls-revoke"
	EffectWrappingKeyRevoke  Effect = "wrapping-key-revoke"
	EffectMarkerCAS          Effect = "marker-cas-v2"
	EffectForkPOST           Effect = "fork-post"
	EffectEvidenceObserve    Effect = "evidence-observe"
	EffectEvidencePublish    Effect = "evidence-publish"
	EffectCleanupDelete      Effect = "cleanup-delete"
)

// mutatingEffects is the exact low-level side-effect inventory. GET-only
// evidence observation is a valid authenticated operation kind, but is not a
// mutation and therefore must never be represented in this inventory.
var mutatingEffects = []Effect{
	EffectAuthorityAppCreate, EffectAuthorityAppUpdate, EffectAuthorityAppDelete,
	EffectBrokerCreate, EffectBrokerUpdate, EffectFullRedeploy, EffectBrokerDelete,
	EffectTrustedSourceAdd, EffectTrustedSourceDel,
	EffectBindingInstall, EffectBindingRemove,
	EffectCredentialInstall, EffectCredentialRemove,
	EffectLeafIssue, EffectLeafRevoke,
	EffectCapabilityIssue, EffectCapabilityRevoke,
	EffectMTLSIssue, EffectMTLSRevoke, EffectWrappingKeyRevoke,
	EffectMarkerCAS, EffectForkPOST, EffectEvidencePublish, EffectCleanupDelete,
}

// allEffects contains every valid durable operation kind. It is deliberately
// broader than mutatingEffects by exactly the GET-only observation kind.
var allEffects = append(append([]Effect(nil), mutatingEffects...), EffectEvidenceObserve)

// MutationSideEffects returns a defensive copy of the exact Gate-A mutation
// inventory. Read-only authenticated operation kinds such as evidence-observe
// are intentionally excluded.
func MutationSideEffects() []Effect {
	return append([]Effect(nil), mutatingEffects...)
}

func effectValid(effect Effect) bool {
	for _, candidate := range allEffects {
		if effect == candidate {
			return true
		}
	}
	return false
}

// MutationRequest binds a side-effect class to the immutable operation body.
// ParametersDigest represents a reviewed, selector-free semantic payload.
type MutationRequest struct {
	Operation        OperationBody
	Effect           Effect
	ParametersDigest Digest
}

func (r MutationRequest) Validate() error {
	if err := r.Operation.Validate(); err != nil {
		return err
	}
	if !effectValid(r.Effect) || r.ParametersDigest.IsZero() {
		return ErrInvalid
	}
	return nil
}

func (r MutationRequest) CanonicalBytes() ([]byte, error) {
	if err := r.Validate(); err != nil {
		return nil, err
	}
	op, _ := r.Operation.CanonicalBytes()
	canonical := `{"operation":` + string(op) +
		`,"effect":` + strconv.Quote(string(r.Effect)) +
		`,"parameters_sha256":` + strconv.Quote(r.ParametersDigest.String()) + `}`
	return []byte(canonical), nil
}

func (r MutationRequest) Digest() (Digest, error) {
	canonical, err := r.CanonicalBytes()
	if err != nil {
		return Digest{}, err
	}
	return HashBytes(canonical), nil
}

func (r MutationRequest) key() string {
	return r.Operation.OperationID + "/" + strconv.FormatUint(r.Operation.Generation, 10) + "/" + string(r.Effect)
}
