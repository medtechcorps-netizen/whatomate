package protocol

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"sync"
	"time"
)

var (
	ErrGateAProductionUnavailable = errors.New("gate A has no production protocol adapter")
	ErrInjectedCrash              = errors.New("test oracle injected crash")
	ErrReplay                     = errors.New("authentication envelope replayed")
	ErrChallengeNotIssued         = errors.New("authentication challenge was not issued by this authority")
	ErrChallengeBinding           = errors.New("authentication challenge does not bind this operation and method")
	ErrOperationReuse             = errors.New("operation identifier reused with a different body")
	ErrQuarantined                = errors.New("operation is quarantined")
	ErrOperationMissing           = errors.New("operation does not exist")
	ErrStateTransition            = errors.New("invalid operation state transition")
)

const (
	BrokerCapabilitySchema       = "recovery-boundary-broker-capability/v1"
	BrokerReadbackSchema         = "recovery-boundary-broker-readback/v1"
	ValidatedAuthorizationSchema = "recovery-boundary-validated-authorization/v1"
	MaxBrokerPayloadBytes        = 4096
	MaxSealedMaterialBytes       = 4096
	AuthorityChallengeBytes      = 32

	BrokerOutcomeCommitted            = "committed"
	BrokerOutcomeUnchangedPredecessor = "unchanged-predecessor"
)

type OperationState string

const (
	StatePrepared          OperationState = "prepared"
	StateIssued            OperationState = "issued"
	StateAmbiguous         OperationState = "ambiguous"
	StateSideEffectStarted OperationState = "side-effect-started"
	StateTerminal          OperationState = "terminal"
	StateQuarantined       OperationState = "quarantined"
)

// DurabilityGuarantees describe the concrete requirements a later production
// adapter must prove. They are not self-authenticating, and Gate A never uses
// them to manufacture production authority. FaultInjectingStore is a test
// oracle, never production authority.
type DurabilityGuarantees struct {
	Transactional      bool
	CrashDurable       bool
	CASFencing         bool
	AppendOnlyAudit    bool
	EncryptionAtRest   bool
	BackupRecovery     bool
	KeyConfinement     bool
	CryptographicErase bool
	TestOnly           bool
}

type SignerGuarantees struct {
	RoleSeparated      bool
	ProviderConfined   bool
	Deterministic      bool
	NoAutomaticRetries bool
	TestOnly           bool
}

// BrokerGuarantees describe the separately deployed fixed-function broker.
// The authority never receives marker bytes; it accepts only a signed digest
// and opaque sealed reconciliation material from this authenticated boundary.
type BrokerGuarantees struct {
	SeparateSubstrate         bool
	AuthenticatedReadback     bool
	InternallyGeneratedMarker bool
	ExactMarkerBytes          int
	RolePhaseGenerationBound  bool
	OperationDigestBound      bool
	MutualTLSBound            bool
	CapabilityBound           bool
	DurableExecutionClaims    bool
	DurableContinuationClaims bool
	ProviderConfined          bool
	NoAutomaticRetries        bool
	TestOnly                  bool
}

type BrokerBinding struct {
	Role                 string
	IdentitySHA256       string
	MTLSPeerSHA256       string
	ProtectedPredecessor *ProtectedPredecessor
	PublicKey            ed25519.PublicKey
}

// ProtectedPredecessor is provider-side fixed adapter state. It is never
// decoded from OperationBody or any other caller-controlled request bytes.
// A nil pointer and an explicit empty predecessor intentionally remain
// distinct internal states.
type ProtectedPredecessor struct {
	Kind   string
	SHA256 string
}

func (p *ProtectedPredecessor) fields() (string, string, [32]byte, error) {
	if p == nil {
		return "nil", "", domainHash("protected-predecessor", []byte("nil")), nil
	}
	var canonical []byte
	switch p.Kind {
	case "empty":
		if p.SHA256 != "" {
			return "", "", [32]byte{}, errors.New("empty protected predecessor must not carry a digest")
		}
		canonical = []byte("empty")
	case "state":
		if !hexSHA256Pattern.MatchString(p.SHA256) {
			return "", "", [32]byte{}, errors.New("state protected predecessor requires a lowercase SHA-256")
		}
		canonical = []byte("state\x00" + p.SHA256)
	default:
		return "", "", [32]byte{}, errors.New("protected predecessor kind must be empty or state")
	}
	return p.Kind, p.SHA256, domainHash("protected-predecessor", canonical), nil
}

func (b BrokerBinding) validate(expectedRole string) error {
	if b.Role != expectedRole {
		return errors.New("broker role does not match authority role")
	}
	if !hexSHA256Pattern.MatchString(b.IdentitySHA256) || !hexSHA256Pattern.MatchString(b.MTLSPeerSHA256) {
		return errors.New("broker identity and mTLS peer must be exact lowercase SHA-256 values")
	}
	if len(b.PublicKey) != ed25519.PublicKeySize {
		return errors.New("broker requires an exact Ed25519 public key")
	}
	if _, _, _, err := b.ProtectedPredecessor.fields(); err != nil {
		return err
	}
	return nil
}

type BrokerTrust interface {
	Guarantees() BrokerGuarantees
	Binding() BrokerBinding
	// executeOnce submits the already-durable execution fence to the
	// separately durable broker. The broker, not the caller, reconciles and
	// performs the fixed provider wire at most once. The bool reports whether
	// this invocation performed that first wire; it never grants the caller
	// permission to issue a provider request itself.
	executeOnce(context.Context, brokerExecutionClaim) (bool, error)
	// continueOnce applies the same rule to the sole provider-internal
	// unchanged-predecessor continuation and returns its cached signed result.
	continueOnce(context.Context, brokerContinuationClaim) (BrokerReadback, error)
}

// brokerExecutionClaim is the authority-to-broker pre-wire fence. Repeating
// this exact claim is an idempotent broker submission; changing any bound
// field under the same claim digest is terminal misuse. It is deliberately an
// internal protocol value rather than a caller-selectable request field.
type brokerExecutionClaim struct {
	OperationBodySHA256 [32]byte
	PredecessorSHA256   [32]byte
	PreWireFenceSHA256  [32]byte
	ExecutionSHA256     [32]byte
	Capability          BrokerCapability
}

// brokerContinuationClaim is created only after a signed
// unchanged-predecessor readback has been durably recorded. The separately
// deployed broker must deduplicate it before making the sole second wire call.
type brokerContinuationClaim struct {
	OperationBodySHA256 [32]byte
	PreWireFenceSHA256  [32]byte
	ExecutionSHA256     [32]byte
	AmbiguitySHA256     [32]byte
	ContinuationSHA256  [32]byte
	Readback            BrokerReadback
}

type DurableStore interface {
	Guarantees() DurabilityGuarantees
	// IssueChallenge must durably bind an authority-generated challenge to one
	// exact operation body, endpoint, role, and authority root before the
	// challenge is returned.
	IssueChallenge(ctx context.Context, challengeSHA256, bindingSHA256 [32]byte) error
	// ClaimAuthentication must atomically and crash-durably persist both
	// replay keys and consume the previously issued challenge before returning
	// nil. It is a separate committed operation, never part of a later
	// endpoint-semantic transaction.
	ClaimAuthentication(ctx context.Context, jtiSHA256, challengeSHA256, bindingSHA256, callSHA256 [32]byte) error
	Transact(ctx context.Context, operationID string, fn func(LedgerTransaction) error) error
	Read(ctx context.Context, operationID string) (OperationSnapshot, bool, error)
}

type ReceiptSigner interface {
	Guarantees() SignerGuarantees
	PublicKey() ed25519.PublicKey
	Sign(ctx context.Context, domain string, canonicalPayload []byte) ([]byte, error)
}

type LedgerTransaction interface {
	Load() (storedOperation, bool)
	Save(storedOperation) error
	AppendAudit(AuditEvent) error
	EraseSealedReconciliation() error
}

type AuditEvent struct {
	Sequence            uint64
	OperationID         string
	OperationBodySHA256 string
	CallSHA256          string
	Event               string
	State               OperationState
}

type storedOperation struct {
	OperationID          string
	Generation           uint64
	BodySHA256           [32]byte
	CanonicalBody        []byte
	State                OperationState
	CapabilityPayload    []byte
	CapabilitySignature  []byte
	CapabilitySHA256     [32]byte
	PredecessorSHA256    [32]byte
	PreWireFenceSHA256   [32]byte
	ExecutionClaimSHA256 [32]byte
	ExecutionClaimCount  uint64
	AmbiguitySHA256      [32]byte
	AmbiguityCount       uint64
	ContinuationSHA256   [32]byte
	ContinuationCount    uint64
	BrokerReadbackSHA256 [32]byte
	MarkerSHA256         [32]byte
	sealedReconciliation []byte
	ReceiptPayload       []byte
	ReceiptSignature     []byte
	QuarantineReason     string
	Revision             uint64
	SealedMaterialErased bool
}

type OperationSnapshot struct {
	OperationID                 string
	Generation                  uint64
	OperationBodySHA256         string
	State                       OperationState
	CapabilityPayload           []byte
	CapabilitySignature         []byte
	CapabilitySHA256            string
	ProtectedPredecessorSHA256  string
	PreWireFenceSHA256          string
	ExecutionClaimSHA256        string
	ExecutionClaimCount         uint64
	AmbiguitySHA256             string
	AmbiguityCount              uint64
	ContinuationClaimSHA256     string
	ContinuationClaimCount      uint64
	BrokerReadbackSHA256        string
	MarkerSHA256                string
	ReceiptPayload              []byte
	ReceiptSignature            []byte
	QuarantineReason            string
	Revision                    uint64
	SealedReconciliationPresent bool
	SealedReconciliationErased  bool
}

func snapshotOf(record storedOperation) OperationSnapshot {
	return OperationSnapshot{
		OperationID: record.OperationID, Generation: record.Generation,
		OperationBodySHA256: SHA256Hex(record.BodySHA256), State: record.State,
		CapabilityPayload:          append([]byte(nil), record.CapabilityPayload...),
		CapabilitySignature:        append([]byte(nil), record.CapabilitySignature...),
		CapabilitySHA256:           SHA256Hex(record.CapabilitySHA256),
		ProtectedPredecessorSHA256: SHA256Hex(record.PredecessorSHA256),
		PreWireFenceSHA256:         SHA256Hex(record.PreWireFenceSHA256),
		ExecutionClaimSHA256:       SHA256Hex(record.ExecutionClaimSHA256),
		ExecutionClaimCount:        record.ExecutionClaimCount,
		AmbiguitySHA256:            SHA256Hex(record.AmbiguitySHA256),
		AmbiguityCount:             record.AmbiguityCount,
		ContinuationClaimSHA256:    SHA256Hex(record.ContinuationSHA256),
		ContinuationClaimCount:     record.ContinuationCount,
		BrokerReadbackSHA256:       SHA256Hex(record.BrokerReadbackSHA256),
		MarkerSHA256:               SHA256Hex(record.MarkerSHA256),
		ReceiptPayload:             append([]byte(nil), record.ReceiptPayload...),
		ReceiptSignature:           append([]byte(nil), record.ReceiptSignature...),
		QuarantineReason:           record.QuarantineReason, Revision: record.Revision,
		SealedReconciliationPresent: len(record.sealedReconciliation) != 0,
		SealedReconciliationErased:  record.SealedMaterialErased,
	}
}

type BrokerCapabilityPayload struct {
	Schema                     string `json:"schema"`
	BrokerRole                 string `json:"broker_role"`
	Phase                      string `json:"phase"`
	Generation                 uint64 `json:"generation"`
	OperationID                string `json:"operation_id"`
	OperationBodySHA256        string `json:"operation_body_sha256"`
	AuthorizationSHA256        string `json:"authorization_sha256"`
	AuthorizationIssuedAtUTC   string `json:"authorization_issued_at_utc"`
	TokenNotBeforeUTC          string `json:"token_not_before_utc"`
	AuthorizationNotAfterUTC   string `json:"authorization_not_after_utc"`
	BrokerIdentitySHA256       string `json:"broker_identity_sha256"`
	MTLSPeerSHA256             string `json:"mtls_peer_sha256"`
	ProtectedPredecessorKind   string `json:"protected_predecessor_kind"`
	ProtectedPredecessorSHA256 string `json:"protected_predecessor_sha256"`
	ExecutionClaimSHA256       string `json:"execution_claim_sha256"`
}

// validatedAuthorizationStatement is derived only after the authority has
// independently validated the raw JWT and both canonical caller documents.
// It commits workload identity and freshness without persisting the raw JWT,
// JTI, challenge, or any other bearer material.
type validatedAuthorizationStatement struct {
	Schema               string `json:"schema"`
	Issuer               string `json:"issuer"`
	Audience             string `json:"audience"`
	Subject              string `json:"subject"`
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
	Method               string `json:"method"`
	OperationBodySHA256  string `json:"operation_body_sha256"`
	AuthEnvelopeSHA256   string `json:"auth_envelope_sha256"`
	JTIHashSHA256        string `json:"jti_hash_sha256"`
	ChallengeHashSHA256  string `json:"challenge_hash_sha256"`
	EnvelopeIssuedAtUTC  string `json:"envelope_issued_at_utc"`
	TokenIssuedAtUTC     string `json:"token_issued_at_utc"`
	TokenNotBeforeUTC    string `json:"token_not_before_utc"`
	TokenExpiresAtUTC    string `json:"token_expires_at_utc"`
}

type BrokerCapability struct {
	Payload   []byte
	Signature []byte
}

type BrokerReadbackPayload struct {
	Schema                     string `json:"schema"`
	BrokerRole                 string `json:"broker_role"`
	Phase                      string `json:"phase"`
	Generation                 uint64 `json:"generation"`
	OperationID                string `json:"operation_id"`
	OperationBodySHA256        string `json:"operation_body_sha256"`
	CapabilitySHA256           string `json:"capability_sha256"`
	BrokerIdentitySHA256       string `json:"broker_identity_sha256"`
	MTLSPeerSHA256             string `json:"mtls_peer_sha256"`
	Outcome                    string `json:"outcome"`
	ProtectedPredecessorKind   string `json:"protected_predecessor_kind"`
	ProtectedPredecessorSHA256 string `json:"protected_predecessor_sha256"`
	ExecutionClaimSHA256       string `json:"execution_claim_sha256"`
	ContinuationClaimSHA256    string `json:"continuation_claim_sha256"`
	BrokerActionAtUTC          string `json:"broker_action_at_utc"`
	MarkerMaterialBytes        uint64 `json:"marker_material_bytes"`
	MarkerSHA256               string `json:"marker_sha256"`
	SealedReconciliationSHA256 string `json:"sealed_reconciliation_sha256"`
}

// BrokerReadback intentionally has no marker-preimage field. The opaque
// sealed bytes exist only for reconciliation and are erased transactionally
// when terminal proof is committed.
type BrokerReadback struct {
	Payload              []byte
	Signature            []byte
	SealedReconciliation []byte
}

type Controller struct {
	role                string
	store               DurableStore
	signer              ReceiptSigner
	signerPublicKey     ed25519.PublicKey
	authorityRootSHA256 [32]byte
	verifier            RawRequestVerifier
	verifierBinding     RequestVerifierBinding
	broker              BrokerTrust
	brokerBinding       BrokerBinding
	entropy             io.Reader
}

// NewController is deliberately unavailable in Gate A. Declarative adapter
// guarantees cannot prove that an implementation is transactional, durable,
// provider-confined, or separately deployed. A later gate must add reviewed
// concrete adapters before this constructor may return authority.
func NewController(role string, store DurableStore, signer ReceiptSigner, verifier RawRequestVerifier, broker BrokerTrust) (*Controller, error) {
	return nil, ErrGateAProductionUnavailable
}

func isNilInterface(value any) bool {
	if value == nil {
		return true
	}
	rv := reflect.ValueOf(value)
	switch rv.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return rv.IsNil()
	default:
		return false
	}
}

// newTestController is the explicit, unexported writer test-oracle path. Every
// adapter must declare TestOnly; this constructor never yields production
// authority. It deliberately cannot synthesize observer authority without the
// separate durable recovery-admission boundary that the production API lacks.
func newTestController(role string, store *FaultInjectingStore, signer ReceiptSigner, verifier RawRequestVerifier, broker BrokerTrust) (*Controller, error) {
	return newTestControllerWithEntropy(role, store, signer, verifier, broker, rand.Reader)
}

// newTestControllerWithEntropy exists solely so tests can make authority
// challenge issuance deterministic. Production entropy is never supplied by a
// caller and the Gate-A production constructor remains unavailable.
func newTestControllerWithEntropy(role string, store *FaultInjectingStore, signer ReceiptSigner, verifier RawRequestVerifier, broker BrokerTrust, entropy io.Reader) (*Controller, error) {
	if role == RoleObserver {
		return nil, errors.New("observer test controller requires a distinct durable recovery-admission-bound API")
	}
	if role != RoleWriter {
		return nil, errors.New("test controller role must be writer")
	}
	return newTestControllerForRoleWithEntropy(role, store, signer, verifier, broker, entropy)
}

// newTestObserverController is an explicit test-only composition path for the
// observer raw-OIDC boundary. It does not make the production constructor
// available or claim that Gate A supplies a durable recovery-admission API.
func newTestObserverController(store *FaultInjectingStore, signer ReceiptSigner, verifier RawRequestVerifier, broker BrokerTrust) (*Controller, error) {
	return newTestControllerForRoleWithEntropy(RoleObserver, store, signer, verifier, broker, rand.Reader)
}

func newTestControllerForRoleWithEntropy(role string, store *FaultInjectingStore, signer ReceiptSigner, verifier RawRequestVerifier, broker BrokerTrust, entropy io.Reader) (*Controller, error) {
	if role != RoleWriter && role != RoleObserver {
		return nil, errors.New("test controller role must be writer or observer")
	}
	if store == nil || !store.Guarantees().TestOnly {
		return nil, errors.New("test controller requires the test-only fault store")
	}
	if isNilInterface(signer) || !signer.Guarantees().TestOnly {
		return nil, errors.New("test controller requires a test-only signer")
	}
	signerPublicKey := signer.PublicKey()
	if len(signerPublicKey) != ed25519.PublicKeySize {
		return nil, errors.New("test controller requires an exact Ed25519 signer public key")
	}
	if isNilInterface(verifier) || !verifier.Guarantees().TestOnly {
		return nil, errors.New("test controller requires a test-only raw request verifier")
	}
	verifierBinding := verifier.Binding()
	if err := verifierBinding.validate(); err != nil {
		return nil, err
	}
	if verifierBinding.Role != role {
		return nil, errors.New("raw request verifier binding role does not match authority role")
	}
	authorityRootSHA256 := sha256.Sum256(signerPublicKey)
	if verifierBinding.AuthorityRootSHA256 != authorityRootSHA256 {
		return nil, errors.New("raw request verifier binding does not match authority root")
	}
	if isNilInterface(broker) {
		return nil, errors.New("test controller requires test-only broker trust")
	}
	brokerGuarantees := broker.Guarantees()
	if !brokerGuarantees.TestOnly || !brokerGuarantees.DurableExecutionClaims || !brokerGuarantees.DurableContinuationClaims ||
		!brokerGuarantees.NoAutomaticRetries || !brokerGuarantees.CapabilityBound {
		return nil, errors.New("test controller requires durable fenced broker execution and continuation claims")
	}
	brokerBinding := broker.Binding()
	if err := brokerBinding.validate(role); err != nil {
		return nil, err
	}
	if bytes.Equal(signerPublicKey, brokerBinding.PublicKey) {
		return nil, errors.New("authority root and broker leaf keys must be distinct")
	}
	if entropy == nil {
		return nil, errors.New("test controller requires an explicit challenge entropy source")
	}
	return &Controller{
		role: role, store: store, signer: signer,
		signerPublicKey:     append(ed25519.PublicKey(nil), signerPublicKey...),
		authorityRootSHA256: authorityRootSHA256,
		verifier:            verifier,
		verifierBinding:     verifierBinding,
		broker:              broker,
		brokerBinding:       snapshotBrokerBinding(brokerBinding),
		entropy:             entropy,
	}, nil
}

func snapshotBrokerBinding(binding BrokerBinding) BrokerBinding {
	binding.PublicKey = append(ed25519.PublicKey(nil), binding.PublicKey...)
	if binding.ProtectedPredecessor != nil {
		copy := *binding.ProtectedPredecessor
		binding.ProtectedPredecessor = &copy
	}
	return binding
}

type BeginDecision struct {
	// ExecuteSideEffect is historical naming retained inside the Gate-A test
	// oracle. True means the fixed broker performed its single fenced wire in
	// this invocation. It is evidence only, never caller-side mutation authority.
	ExecuteSideEffect bool
	Snapshot          OperationSnapshot
	Capability        BrokerCapability
}

// IssueChallenge creates the fresh authentication challenge. The request may
// reference the returned value, but cannot select it: the authority generates
// exactly 32 CSPRNG bytes and durably binds their hash to the canonical body,
// method, role, and authority-root public-key hash before returning the
// hexadecimal value.
func (c *Controller) IssueChallenge(ctx context.Context, canonicalBody []byte, method string) (string, error) {
	if c == nil || c.entropy == nil || c.store == nil {
		return "", errors.New("authority challenge issuer is unavailable")
	}
	if !validAuthMethod(method) {
		return "", errors.New("challenge method is not an approved protocol endpoint")
	}
	if _, err := DecodeOperationBody(canonicalBody); err != nil {
		return "", err
	}
	bodyDigest, err := OperationBodyDigest(canonicalBody)
	if err != nil {
		return "", err
	}
	var material [AuthorityChallengeBytes]byte
	if _, err := io.ReadFull(c.entropy, material[:]); err != nil {
		return "", fmt.Errorf("generate authority challenge: %w", err)
	}
	challenge := hex.EncodeToString(material[:])
	challengeDigest := sha256.Sum256([]byte(challenge))
	bindingDigest := challengeBindingDigest(bodyDigest, method, c.role, c.authorityRootSHA256)
	if err := c.store.IssueChallenge(ctx, challengeDigest, bindingDigest); err != nil {
		return "", err
	}
	return challenge, nil
}

func challengeBindingDigest(bodyDigest [32]byte, method, role string, authorityRootSHA256 [32]byte) [32]byte {
	payload := make([]byte, 0, len(bodyDigest)+len(method)+len(role)+len(authorityRootSHA256)+3)
	payload = append(payload, bodyDigest[:]...)
	payload = append(payload, 0)
	payload = append(payload, method...)
	payload = append(payload, 0)
	payload = append(payload, role...)
	payload = append(payload, 0)
	payload = append(payload, authorityRootSHA256[:]...)
	return domainHash("authority-challenge-binding", payload)
}

func executionClaimDigest(bodyDigest, predecessorDigest, authorizationDigest [32]byte, binding BrokerBinding) [32]byte {
	payload := make([]byte, 0, 32*3+len(binding.Role)+len(binding.IdentitySHA256)+len(binding.MTLSPeerSHA256)+3)
	payload = append(payload, bodyDigest[:]...)
	payload = append(payload, predecessorDigest[:]...)
	payload = append(payload, authorizationDigest[:]...)
	payload = append(payload, 0)
	payload = append(payload, binding.Role...)
	payload = append(payload, 0)
	payload = append(payload, binding.IdentitySHA256...)
	payload = append(payload, 0)
	payload = append(payload, binding.MTLSPeerSHA256...)
	return domainHash("broker-execution-claim", payload)
}

func continuationClaimDigest(record storedOperation) [32]byte {
	payload := make([]byte, 0, 32*7)
	payload = append(payload, record.BodySHA256[:]...)
	payload = append(payload, record.PredecessorSHA256[:]...)
	payload = append(payload, record.PreWireFenceSHA256[:]...)
	payload = append(payload, record.ExecutionClaimSHA256[:]...)
	payload = append(payload, record.CapabilitySHA256[:]...)
	payload = append(payload, record.AmbiguitySHA256[:]...)
	payload = append(payload, record.MarkerSHA256[:]...)
	return domainHash("broker-continuation-claim", payload)
}

func (c *Controller) BeginMutation(ctx context.Context, rawJWT string, canonicalEnvelope, canonicalBody []byte) (BeginDecision, error) {
	request, err := c.authenticateAndBurn(ctx, rawJWT, canonicalEnvelope, canonicalBody, MethodAuthorize)
	if err != nil {
		return BeginDecision{}, err
	}
	var decision BeginDecision
	var semanticErr error
	var preparedPayload []byte
	var preparedFence [32]byte
	var preparedExecutionClaim [32]byte
	var executionPredecessor [32]byte
	var executionFence [32]byte
	var executionDigest [32]byte
	needsIssue := false
	needsExecutionClaim := false
	err = c.store.Transact(ctx, request.body.OperationID, func(tx LedgerTransaction) error {
		if err := c.requireOwnedOperation(request.body); err != nil {
			semanticErr = err
			return tx.AppendAudit(rejectedAudit(request, "authorization-rejected-unowned-operation"))
		}
		record, exists := tx.Load()
		if exists {
			if record.BodySHA256 != request.bodyDigest || record.Generation != request.body.Generation {
				record.State = StateQuarantined
				record.QuarantineReason = "operation identifier reused with a different immutable body"
				record.Revision++
				if err := tx.Save(record); err != nil {
					return err
				}
				semanticErr = ErrOperationReuse
				decision.Snapshot = snapshotOf(record)
				return tx.AppendAudit(auditFor(record, request.envelopeDigest, "operation-reuse-quarantined"))
			}
			switch record.State {
			case StateQuarantined:
				semanticErr = ErrQuarantined
			case StatePrepared:
				preparedPayload = append([]byte(nil), record.CapabilityPayload...)
				preparedFence = record.PreWireFenceSHA256
				preparedExecutionClaim = record.ExecutionClaimSHA256
				needsIssue = true
			default:
				decision.Capability = BrokerCapability{Payload: append([]byte(nil), record.CapabilityPayload...), Signature: append([]byte(nil), record.CapabilitySignature...)}
				if record.State == StateIssued {
					if record.ExecutionClaimCount != 1 || record.ExecutionClaimSHA256 == ([32]byte{}) {
						return quarantine(tx, &record, request.envelopeDigest, "issued operation is missing its durable execution claim", ErrQuarantined, &decision.Snapshot, &semanticErr)
					}
					executionPredecessor = record.PredecessorSHA256
					executionFence = record.PreWireFenceSHA256
					executionDigest = record.ExecutionClaimSHA256
					needsExecutionClaim = true
				}
			}
			decision.Snapshot = snapshotOf(record)
			event := "idempotent-authorization"
			if record.State == StatePrepared {
				event = "prepared-authorization-recovered"
			}
			return tx.AppendAudit(auditFor(record, request.envelopeDigest, event))
		}
		capabilityPayload, predecessorDigest, executionClaim, err := c.prepareCapabilityPayload(request)
		if err != nil {
			return err
		}
		payloadDigest := domainHash("prepared-broker-capability", capabilityPayload)
		fencePayload := make([]byte, 0, len(request.bodyDigest)+len(payloadDigest)+len(predecessorDigest))
		fencePayload = append(fencePayload, request.bodyDigest[:]...)
		fencePayload = append(fencePayload, payloadDigest[:]...)
		fencePayload = append(fencePayload, predecessorDigest[:]...)
		preparedFence = domainHash("pre-wire-fence", fencePayload)
		record = storedOperation{
			OperationID: request.body.OperationID, Generation: request.body.Generation,
			BodySHA256: request.bodyDigest, CanonicalBody: append([]byte(nil), canonicalBody...),
			State: StatePrepared, CapabilityPayload: append([]byte(nil), capabilityPayload...),
			PredecessorSHA256: predecessorDigest, PreWireFenceSHA256: preparedFence,
			ExecutionClaimSHA256: executionClaim, Revision: 1,
		}
		if err := tx.Save(record); err != nil {
			return err
		}
		if err := tx.AppendAudit(auditFor(record, request.envelopeDigest, "operation-prepared-before-wire")); err != nil {
			return err
		}
		preparedPayload = append([]byte(nil), capabilityPayload...)
		preparedExecutionClaim = executionClaim
		needsIssue = true
		decision.Snapshot = snapshotOf(record)
		return nil
	})
	if err != nil {
		return BeginDecision{}, err
	}
	if semanticErr != nil {
		return decision, semanticErr
	}
	if !needsIssue {
		if !needsExecutionClaim {
			return decision, nil
		}
		claimed, err := c.broker.executeOnce(ctx, brokerExecutionClaim{
			OperationBodySHA256: request.bodyDigest,
			PredecessorSHA256:   executionPredecessor,
			PreWireFenceSHA256:  executionFence,
			ExecutionSHA256:     executionDigest,
			Capability:          decision.Capability,
		})
		if err != nil {
			return decision, fmt.Errorf("claim broker execution: %w", err)
		}
		decision.ExecuteSideEffect = claimed
		return decision, nil
	}
	capability, capabilityDigest, err := c.signPreparedCapability(ctx, preparedPayload)
	if err != nil {
		return BeginDecision{}, err
	}
	err = c.store.Transact(ctx, request.body.OperationID, func(tx LedgerTransaction) error {
		record, exists := tx.Load()
		if !exists {
			return ErrOperationMissing
		}
		if record.BodySHA256 != request.bodyDigest || record.Generation != request.body.Generation ||
			record.PreWireFenceSHA256 != preparedFence || record.ExecutionClaimSHA256 != preparedExecutionClaim ||
			!bytes.Equal(record.CapabilityPayload, preparedPayload) {
			return quarantine(tx, &record, request.envelopeDigest, "prepared capability fence changed before issue", ErrOperationReuse, &decision.Snapshot, &semanticErr)
		}
		if record.State == StateQuarantined {
			semanticErr = ErrQuarantined
			decision.Snapshot = snapshotOf(record)
			return tx.AppendAudit(auditFor(record, request.envelopeDigest, "capability-issue-rejected-quarantined"))
		}
		if record.State != StatePrepared {
			decision.Capability = BrokerCapability{Payload: append([]byte(nil), record.CapabilityPayload...), Signature: append([]byte(nil), record.CapabilitySignature...)}
			decision.Snapshot = snapshotOf(record)
			if record.State == StateIssued {
				if record.ExecutionClaimCount != 1 || record.ExecutionClaimSHA256 == ([32]byte{}) {
					return quarantine(tx, &record, request.envelopeDigest, "issued operation is missing its durable execution claim", ErrQuarantined, &decision.Snapshot, &semanticErr)
				}
				executionPredecessor = record.PredecessorSHA256
				executionFence = record.PreWireFenceSHA256
				executionDigest = record.ExecutionClaimSHA256
				needsExecutionClaim = true
			}
			return tx.AppendAudit(auditFor(record, request.envelopeDigest, "idempotent-capability-issue"))
		}
		record.State = StateIssued
		record.CapabilitySignature = append([]byte(nil), capability.Signature...)
		record.CapabilitySHA256 = capabilityDigest
		record.ExecutionClaimCount = 1
		record.Revision++
		if err := tx.Save(record); err != nil {
			return err
		}
		if err := tx.AppendAudit(auditFor(record, request.envelopeDigest, "broker-capability-issued-after-fence")); err != nil {
			return err
		}
		decision.Capability = capability
		decision.Snapshot = snapshotOf(record)
		executionPredecessor = record.PredecessorSHA256
		executionFence = record.PreWireFenceSHA256
		executionDigest = record.ExecutionClaimSHA256
		needsExecutionClaim = true
		return nil
	})
	if err != nil {
		return BeginDecision{}, err
	}
	if semanticErr != nil || !needsExecutionClaim {
		return decision, semanticErr
	}
	claimed, err := c.broker.executeOnce(ctx, brokerExecutionClaim{
		OperationBodySHA256: request.bodyDigest,
		PredecessorSHA256:   executionPredecessor,
		PreWireFenceSHA256:  executionFence,
		ExecutionSHA256:     executionDigest,
		Capability:          decision.Capability,
	})
	if err != nil {
		return decision, fmt.Errorf("claim broker execution: %w", err)
	}
	decision.ExecuteSideEffect = claimed
	return decision, nil
}

func (c *Controller) AcceptBrokerReadback(ctx context.Context, rawJWT string, canonicalEnvelope, canonicalBody []byte, readback BrokerReadback) (OperationSnapshot, error) {
	request, err := c.authenticateAndBurn(ctx, rawJWT, canonicalEnvelope, canonicalBody, MethodBrokerReadback)
	if err != nil {
		return OperationSnapshot{}, err
	}
	var snapshot OperationSnapshot
	var semanticErr error
	var continuation *brokerContinuationClaim
	err = c.store.Transact(ctx, request.body.OperationID, func(tx LedgerTransaction) error {
		if err := c.requireOwnedOperation(request.body); err != nil {
			semanticErr = err
			return tx.AppendAudit(rejectedAudit(request, "broker-readback-rejected-unowned-operation"))
		}
		record, exists := tx.Load()
		if !exists {
			semanticErr = ErrOperationMissing
			return tx.AppendAudit(rejectedAudit(request, "broker-readback-rejected-missing"))
		}
		if record.BodySHA256 != request.bodyDigest {
			return quarantine(tx, &record, request.envelopeDigest, "broker readback reused operation with a different body", ErrOperationReuse, &snapshot, &semanticErr)
		}
		if record.State == StateQuarantined {
			semanticErr = ErrQuarantined
			snapshot = snapshotOf(record)
			return tx.AppendAudit(auditFor(record, request.envelopeDigest, "broker-readback-rejected-quarantined"))
		}
		// A terminal receipt is immutable. Do not parse, verify, or otherwise
		// give meaning to later same-body readback bytes. Differing body reuse
		// was already quarantined above.
		if record.State == StateTerminal {
			semanticErr = ErrStateTransition
			snapshot = snapshotOf(record)
			return tx.AppendAudit(auditFor(record, request.envelopeDigest, "broker-readback-rejected-terminal-immutable"))
		}
		if record.State == StatePrepared {
			semanticErr = ErrStateTransition
			snapshot = snapshotOf(record)
			return tx.AppendAudit(auditFor(record, request.envelopeDigest, "broker-readback-rejected-prepared"))
		}
		verifiedReadback, err := c.verifyBrokerReadback(record, request.body, readback)
		if err != nil {
			return quarantine(tx, &record, request.envelopeDigest, "broker readback verification failed: "+err.Error(), ErrQuarantined, &snapshot, &semanticErr)
		}
		readbackDigest := verifiedReadback.RecordSHA256
		markerDigest := verifiedReadback.MarkerSHA256
		outcome := verifiedReadback.Outcome
		if verifiedReadback.PredecessorSHA256 != record.PredecessorSHA256 {
			return quarantine(tx, &record, request.envelopeDigest, "broker readback changed the protected predecessor binding", ErrQuarantined, &snapshot, &semanticErr)
		}
		if record.State == StateSideEffectStarted {
			committedReplay := outcome == BrokerOutcomeCommitted && record.BrokerReadbackSHA256 == readbackDigest
			ambiguityReplay := outcome == BrokerOutcomeUnchangedPredecessor && verifiedReadback.ContinuationClaimSHA256 == ([32]byte{}) &&
				record.ContinuationCount == 1 && record.AmbiguitySHA256 == readbackDigest
			if !committedReplay && !ambiguityReplay {
				return quarantine(tx, &record, request.envelopeDigest, "different broker readback reused completed side-effect transition", ErrQuarantined, &snapshot, &semanticErr)
			}
			snapshot = snapshotOf(record)
			event := "idempotent-broker-readback"
			if ambiguityReplay {
				event = "idempotent-ambiguity-readback-after-continuation"
			}
			return tx.AppendAudit(auditFor(record, request.envelopeDigest, event))
		}
		if outcome == BrokerOutcomeUnchangedPredecessor {
			if verifiedReadback.ContinuationClaimSHA256 != ([32]byte{}) {
				return quarantine(tx, &record, request.envelopeDigest, "unchanged-predecessor readback carried a continuation claim", ErrQuarantined, &snapshot, &semanticErr)
			}
			if record.State == StateAmbiguous {
				if record.AmbiguityCount != 1 || record.AmbiguitySHA256 != readbackDigest ||
					record.MarkerSHA256 != markerDigest || !bytes.Equal(record.sealedReconciliation, readback.SealedReconciliation) ||
					record.ContinuationCount != 1 || record.ContinuationSHA256 == ([32]byte{}) ||
					record.ContinuationSHA256 != continuationClaimDigest(record) {
					return quarantine(tx, &record, request.envelopeDigest, "different unchanged-predecessor evidence reused the ambiguity fence", ErrQuarantined, &snapshot, &semanticErr)
				}
				snapshot = snapshotOf(record)
				if err := tx.AppendAudit(auditFor(record, request.envelopeDigest, "idempotent-unchanged-predecessor-evidence")); err != nil {
					return err
				}
			} else if record.State != StateIssued {
				semanticErr = ErrStateTransition
				snapshot = snapshotOf(record)
				return tx.AppendAudit(auditFor(record, request.envelopeDigest, "unchanged-predecessor-rejected-state"))
			} else {
				record.State = StateAmbiguous
				record.AmbiguitySHA256 = readbackDigest
				record.AmbiguityCount = 1
				record.MarkerSHA256 = markerDigest
				record.sealedReconciliation = append([]byte(nil), readback.SealedReconciliation...)
				record.ContinuationSHA256 = continuationClaimDigest(record)
				record.ContinuationCount = 1
				record.Revision++
				if err := tx.Save(record); err != nil {
					return err
				}
				if err := tx.AppendAudit(auditFor(record, request.envelopeDigest, "unchanged-predecessor-continuation-claimed-before-second-wire")); err != nil {
					return err
				}
				snapshot = snapshotOf(record)
			}
			continuation = &brokerContinuationClaim{
				OperationBodySHA256: record.BodySHA256,
				PreWireFenceSHA256:  record.PreWireFenceSHA256,
				ExecutionSHA256:     record.ExecutionClaimSHA256,
				AmbiguitySHA256:     record.AmbiguitySHA256,
				ContinuationSHA256:  record.ContinuationSHA256,
				Readback:            cloneBrokerReadback(readback),
			}
			return nil
		}
		if record.State == StateAmbiguous {
			if record.AmbiguityCount != 1 || record.ContinuationCount != 1 ||
				record.MarkerSHA256 != markerDigest || !bytes.Equal(record.sealedReconciliation, readback.SealedReconciliation) ||
				verifiedReadback.ContinuationClaimSHA256 != record.ContinuationSHA256 {
				return quarantine(tx, &record, request.envelopeDigest, "committed continuation did not reuse the fenced sealed marker and continuation claim", ErrQuarantined, &snapshot, &semanticErr)
			}
		} else if verifiedReadback.ContinuationClaimSHA256 != ([32]byte{}) {
			return quarantine(tx, &record, request.envelopeDigest, "initial committed readback carried an unclaimed continuation", ErrQuarantined, &snapshot, &semanticErr)
		}
		if record.State != StateIssued && record.State != StateAmbiguous {
			semanticErr = ErrStateTransition
			snapshot = snapshotOf(record)
			return tx.AppendAudit(auditFor(record, request.envelopeDigest, "broker-readback-rejected-state"))
		}
		record.State = StateSideEffectStarted
		record.BrokerReadbackSHA256 = readbackDigest
		record.MarkerSHA256 = markerDigest
		record.sealedReconciliation = append([]byte(nil), readback.SealedReconciliation...)
		record.Revision++
		if err := tx.Save(record); err != nil {
			return err
		}
		if err := tx.AppendAudit(auditFor(record, request.envelopeDigest, "authenticated-broker-readback-accepted")); err != nil {
			return err
		}
		snapshot = snapshotOf(record)
		return nil
	})
	if err != nil {
		return OperationSnapshot{}, err
	}
	if semanticErr != nil || continuation == nil {
		return snapshot, semanticErr
	}
	continuedReadback, err := c.broker.continueOnce(ctx, *continuation)
	if err != nil {
		return snapshot, fmt.Errorf("continue fenced broker operation: %w", err)
	}
	return c.acceptBrokerContinuation(ctx, request, *continuation, continuedReadback)
}

func (c *Controller) acceptBrokerContinuation(ctx context.Context, request verifiedRequest, claim brokerContinuationClaim, readback BrokerReadback) (OperationSnapshot, error) {
	var snapshot OperationSnapshot
	var semanticErr error
	err := c.store.Transact(ctx, request.body.OperationID, func(tx LedgerTransaction) error {
		record, exists := tx.Load()
		if !exists {
			return ErrOperationMissing
		}
		if record.BodySHA256 != request.bodyDigest || record.BodySHA256 != claim.OperationBodySHA256 ||
			record.PreWireFenceSHA256 != claim.PreWireFenceSHA256 || record.ExecutionClaimSHA256 != claim.ExecutionSHA256 ||
			record.AmbiguitySHA256 != claim.AmbiguitySHA256 || record.ContinuationSHA256 != claim.ContinuationSHA256 ||
			record.AmbiguityCount != 1 || record.ContinuationCount != 1 ||
			signedRecordDigest("broker-readback-record", claim.Readback.Payload, claim.Readback.Signature) != record.AmbiguitySHA256 ||
			!bytes.Equal(record.sealedReconciliation, claim.Readback.SealedReconciliation) {
			return quarantine(tx, &record, claim.ContinuationSHA256, "broker continuation claim changed after its durable pre-wire fence", ErrQuarantined, &snapshot, &semanticErr)
		}
		if record.State == StateQuarantined {
			semanticErr = ErrQuarantined
			snapshot = snapshotOf(record)
			return tx.AppendAudit(auditFor(record, claim.ContinuationSHA256, "broker-continuation-rejected-quarantined"))
		}
		verifiedReadback, err := c.verifyBrokerReadback(record, request.body, readback)
		if err != nil {
			return quarantine(tx, &record, claim.ContinuationSHA256, "broker continuation readback verification failed: "+err.Error(), ErrQuarantined, &snapshot, &semanticErr)
		}
		if record.State == StateSideEffectStarted {
			if verifiedReadback.Outcome != BrokerOutcomeCommitted || record.BrokerReadbackSHA256 != verifiedReadback.RecordSHA256 {
				return quarantine(tx, &record, claim.ContinuationSHA256, "different broker continuation reused completed side-effect transition", ErrQuarantined, &snapshot, &semanticErr)
			}
			snapshot = snapshotOf(record)
			return tx.AppendAudit(auditFor(record, claim.ContinuationSHA256, "idempotent-broker-continuation"))
		}
		if record.State != StateAmbiguous {
			semanticErr = ErrStateTransition
			snapshot = snapshotOf(record)
			return tx.AppendAudit(auditFor(record, claim.ContinuationSHA256, "broker-continuation-rejected-state"))
		}
		if verifiedReadback.Outcome != BrokerOutcomeCommitted ||
			verifiedReadback.PredecessorSHA256 != record.PredecessorSHA256 ||
			verifiedReadback.ContinuationClaimSHA256 != record.ContinuationSHA256 ||
			verifiedReadback.MarkerSHA256 != record.MarkerSHA256 ||
			!bytes.Equal(record.sealedReconciliation, readback.SealedReconciliation) {
			return quarantine(tx, &record, claim.ContinuationSHA256, "broker continuation did not commit the exact fenced marker", ErrQuarantined, &snapshot, &semanticErr)
		}
		record.State = StateSideEffectStarted
		record.BrokerReadbackSHA256 = verifiedReadback.RecordSHA256
		record.Revision++
		if err := tx.Save(record); err != nil {
			return err
		}
		if err := tx.AppendAudit(auditFor(record, claim.ContinuationSHA256, "fenced-broker-continuation-accepted")); err != nil {
			return err
		}
		snapshot = snapshotOf(record)
		return nil
	})
	if err != nil {
		return snapshot, err
	}
	return snapshot, semanticErr
}

func cloneBrokerReadback(readback BrokerReadback) BrokerReadback {
	return BrokerReadback{
		Payload:              append([]byte(nil), readback.Payload...),
		Signature:            append([]byte(nil), readback.Signature...),
		SealedReconciliation: append([]byte(nil), readback.SealedReconciliation...),
	}
}

type receiptPayload struct {
	Schema                     string `json:"schema"`
	Role                       string `json:"role"`
	OperationID                string `json:"operation_id"`
	Generation                 uint64 `json:"generation"`
	OperationBodySHA256        string `json:"operation_body_sha256"`
	CapabilitySHA256           string `json:"capability_sha256"`
	ProtectedPredecessorSHA256 string `json:"protected_predecessor_sha256"`
	PreWireFenceSHA256         string `json:"pre_wire_fence_sha256"`
	ExecutionClaimSHA256       string `json:"execution_claim_sha256"`
	ExecutionClaimCount        uint64 `json:"execution_claim_count"`
	AmbiguitySHA256            string `json:"ambiguity_sha256"`
	AmbiguityCount             uint64 `json:"ambiguity_count"`
	ContinuationClaimSHA256    string `json:"continuation_claim_sha256"`
	ContinuationClaimCount     uint64 `json:"continuation_claim_count"`
	BrokerReadbackSHA256       string `json:"broker_readback_sha256"`
	MarkerSHA256               string `json:"marker_sha256"`
	TerminalState              string `json:"terminal_state"`
}

func (c *Controller) CommitTerminalProof(ctx context.Context, rawJWT string, canonicalEnvelope, canonicalBody []byte) (OperationSnapshot, error) {
	request, err := c.authenticateAndBurn(ctx, rawJWT, canonicalEnvelope, canonicalBody, MethodTerminalProof)
	if err != nil {
		return OperationSnapshot{}, err
	}
	var snapshot OperationSnapshot
	var semanticErr error
	err = c.store.Transact(ctx, request.body.OperationID, func(tx LedgerTransaction) error {
		if err := c.requireOwnedOperation(request.body); err != nil {
			semanticErr = err
			return tx.AppendAudit(rejectedAudit(request, "terminal-proof-rejected-unowned-operation"))
		}
		record, exists := tx.Load()
		if !exists {
			semanticErr = ErrOperationMissing
			return tx.AppendAudit(rejectedAudit(request, "terminal-proof-rejected-missing"))
		}
		if record.BodySHA256 != request.bodyDigest {
			return quarantine(tx, &record, request.envelopeDigest, "terminal proof reused operation with a different body", ErrOperationReuse, &snapshot, &semanticErr)
		}
		if record.State == StateQuarantined {
			semanticErr = ErrQuarantined
			snapshot = snapshotOf(record)
			return tx.AppendAudit(auditFor(record, request.envelopeDigest, "terminal-proof-rejected-quarantined"))
		}
		if record.State == StateTerminal {
			snapshot = snapshotOf(record)
			return tx.AppendAudit(auditFor(record, request.envelopeDigest, "idempotent-terminal-proof"))
		}
		if record.State != StateSideEffectStarted || record.MarkerSHA256 == ([32]byte{}) || len(record.sealedReconciliation) == 0 {
			semanticErr = ErrStateTransition
			snapshot = snapshotOf(record)
			return tx.AppendAudit(auditFor(record, request.envelopeDigest, "terminal-proof-rejected-state"))
		}
		terminalState := "source-readback-proven"
		domain := DomainWriterReceipt
		if c.role == RoleObserver {
			terminalState = "recovery-readback-proven"
			domain = DomainObserverReceipt
		}
		payload := receiptPayload{
			Schema: "recovery-boundary-receipt/v1", Role: c.role,
			OperationID: record.OperationID, Generation: record.Generation,
			OperationBodySHA256: SHA256Hex(record.BodySHA256), CapabilitySHA256: SHA256Hex(record.CapabilitySHA256),
			ProtectedPredecessorSHA256: SHA256Hex(record.PredecessorSHA256), PreWireFenceSHA256: SHA256Hex(record.PreWireFenceSHA256),
			ExecutionClaimSHA256: SHA256Hex(record.ExecutionClaimSHA256), ExecutionClaimCount: record.ExecutionClaimCount,
			AmbiguitySHA256: SHA256Hex(record.AmbiguitySHA256), AmbiguityCount: record.AmbiguityCount,
			ContinuationClaimSHA256: SHA256Hex(record.ContinuationSHA256), ContinuationClaimCount: record.ContinuationCount,
			BrokerReadbackSHA256: SHA256Hex(record.BrokerReadbackSHA256), MarkerSHA256: SHA256Hex(record.MarkerSHA256),
			TerminalState: terminalState,
		}
		canonicalReceipt, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		signature, err := c.signer.Sign(ctx, domain, canonicalReceipt)
		if err != nil {
			return fmt.Errorf("sign terminal receipt: %w", err)
		}
		if err := VerifyCanonical(c.signerPublicKey, domain, canonicalReceipt, signature); err != nil {
			return fmt.Errorf("self-verify terminal receipt: %w", err)
		}
		if err := tx.EraseSealedReconciliation(); err != nil {
			return fmt.Errorf("erase sealed reconciliation material: %w", err)
		}
		record, _ = tx.Load()
		record.State = StateTerminal
		record.ReceiptPayload = append([]byte(nil), canonicalReceipt...)
		record.ReceiptSignature = append([]byte(nil), signature...)
		record.Revision++
		if err := tx.Save(record); err != nil {
			return err
		}
		if err := tx.AppendAudit(auditFor(record, request.envelopeDigest, "terminal-proof-committed")); err != nil {
			return err
		}
		snapshot = snapshotOf(record)
		return nil
	})
	if err != nil {
		return OperationSnapshot{}, err
	}
	return snapshot, semanticErr
}

// Status consumes a fresh raw-JWT authentication envelope but never creates
// an operation or repeats a mutation. It is the only reconciliation call after
// HTTP 408, EOF, transport ambiguity, or 5xx.
func (c *Controller) Status(ctx context.Context, rawJWT string, canonicalEnvelope, canonicalBody []byte) (OperationSnapshot, error) {
	request, err := c.authenticateAndBurn(ctx, rawJWT, canonicalEnvelope, canonicalBody, MethodStatus)
	if err != nil {
		return OperationSnapshot{}, err
	}
	var snapshot OperationSnapshot
	var semanticErr error
	err = c.store.Transact(ctx, request.body.OperationID, func(tx LedgerTransaction) error {
		if err := c.requireOwnedOperation(request.body); err != nil {
			semanticErr = err
			return tx.AppendAudit(rejectedAudit(request, "status-rejected-unowned-operation"))
		}
		record, exists := tx.Load()
		if !exists {
			semanticErr = ErrOperationMissing
			return tx.AppendAudit(rejectedAudit(request, "status-missing"))
		}
		if record.BodySHA256 != request.bodyDigest {
			return quarantine(tx, &record, request.envelopeDigest, "status reused operation with a different body", ErrOperationReuse, &snapshot, &semanticErr)
		}
		snapshot = snapshotOf(record)
		return tx.AppendAudit(auditFor(record, request.envelopeDigest, "status-read"))
	})
	if err != nil {
		return OperationSnapshot{}, err
	}
	return snapshot, semanticErr
}

func (c *Controller) authenticate(ctx context.Context, rawJWT string, canonicalEnvelope, canonicalBody []byte, method string) (verifiedRequest, error) {
	if c == nil || isNilInterface(c.verifier) {
		return verifiedRequest{}, errors.New("authority has no raw request verifier")
	}
	if rawJWT == "" {
		return verifiedRequest{}, errors.New("raw OIDC JWT is required")
	}
	request, err := c.verifier.VerifyRequest(ctx, rawJWT, canonicalEnvelope, canonicalBody, method)
	if err != nil {
		return verifiedRequest{}, err
	}
	// Re-parse all caller bytes inside the authority even after raw-token
	// verification. This prevents substitution of gateway-parsed values.
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
	envelopeDigest, err := AuthEnvelopeDigest(canonicalEnvelope)
	if err != nil {
		return verifiedRequest{}, err
	}
	if !reflect.DeepEqual(request.body, body) || request.bodyDigest != bodyDigest || request.envelope != envelope || request.envelopeDigest != envelopeDigest {
		return verifiedRequest{}, errors.New("raw request verifier returned data different from canonical caller bytes")
	}
	if envelope.Method != method || envelope.OperationBodySHA256 != SHA256Hex(bodyDigest) {
		return verifiedRequest{}, errors.New("canonical authentication envelope does not bind endpoint and operation body")
	}
	if request.identity.JTI != envelope.JTI {
		return verifiedRequest{}, errors.New("validated OIDC identity does not bind the fresh envelope")
	}
	if err := c.verifierBinding.validateIdentity(request.identity); err != nil {
		return verifiedRequest{}, err
	}
	if request.identity.WorkflowSHA != body.ControlSHA {
		return verifiedRequest{}, errors.New("validated OIDC identity does not bind the control SHA")
	}
	authorizationDigest, tokenNotBeforeUTC, notAfterUTC, err := validatedAuthorizationDigest(request)
	if err != nil {
		return verifiedRequest{}, err
	}
	request.authorizationDigest = authorizationDigest
	request.tokenNotBeforeUTC = tokenNotBeforeUTC
	request.authorizationNotAfterUTC = notAfterUTC
	return request, nil
}

// authenticateAndBurn validates the complete raw request first, then commits
// the one-time JTI and challenge in an independent durable operation. The burn
// is intentionally outside the endpoint's semantic transaction: once raw
// authentication succeeds, no later semantic, signing, persistence, audit, or
// crash failure can make the same authentication envelope usable again.
func (c *Controller) authenticateAndBurn(ctx context.Context, rawJWT string, canonicalEnvelope, canonicalBody []byte, method string) (verifiedRequest, error) {
	request, err := c.authenticate(ctx, rawJWT, canonicalEnvelope, canonicalBody, method)
	if err != nil {
		return verifiedRequest{}, err
	}
	jti := sha256.Sum256([]byte(request.envelope.JTI))
	challenge := sha256.Sum256([]byte(request.envelope.Challenge))
	binding := challengeBindingDigest(request.bodyDigest, method, c.role, c.authorityRootSHA256)
	if err := c.store.ClaimAuthentication(ctx, jti, challenge, binding, request.envelopeDigest); err != nil {
		return verifiedRequest{}, err
	}
	return request, nil
}

func validatedAuthorizationDigest(request verifiedRequest) ([32]byte, string, string, error) {
	var zero [32]byte
	identity := request.identity
	for name, value := range map[string]string{
		"issuer": identity.Issuer, "audience": identity.Audience, "subject": identity.Subject,
		"actor": identity.Actor, "actor_id": identity.ActorID,
		"repository": identity.Repository, "repository_id": identity.RepositoryID,
		"repository_owner": identity.RepositoryOwner, "repository_owner_id": identity.RepositoryOwnerID,
		"repository_visibility": identity.RepositoryVisibility, "environment": identity.Environment,
		"ref": identity.Ref, "ref_type": identity.RefType,
		"workflow_ref": identity.WorkflowRef, "workflow_sha": identity.WorkflowSHA,
		"workflow": identity.Workflow, "sha": identity.SHA,
		"event_name": identity.EventName, "run_id": identity.RunID, "run_number": identity.RunNumber,
		"run_attempt":        identity.RunAttempt,
		"runner_environment": identity.RunnerEnvironment,
	} {
		if value == "" {
			return zero, "", "", fmt.Errorf("validated OIDC identity %s is empty", name)
		}
		if err := validateCanonicalString(value); err != nil {
			return zero, "", "", fmt.Errorf("validated OIDC identity %s: %w", name, err)
		}
	}
	for name, value := range map[string]string{
		"actor_id": identity.ActorID, "repository_id": identity.RepositoryID,
		"repository_owner_id": identity.RepositoryOwnerID, "run_id": identity.RunID,
		"run_number": identity.RunNumber, "run_attempt": identity.RunAttempt,
	} {
		if !positiveDecimalPattern.MatchString(value) {
			return zero, "", "", fmt.Errorf("validated OIDC identity %s is not a canonical positive decimal", name)
		}
	}
	if identity.RunAttempt != "1" || identity.Ref != "refs/heads/main" || identity.RefType != "branch" ||
		!validRepositoryVisibility(identity.RepositoryVisibility) || identity.SHA != identity.WorkflowSHA ||
		!gitCommitPattern.MatchString(identity.WorkflowSHA) {
		return zero, "", "", errors.New("validated OIDC identity does not bind exact attempt-1 protected-main workload identity")
	}
	for name, value := range map[string]string{"base_ref": identity.BaseRef, "head_ref": identity.HeadRef} {
		if err := validateCanonicalString(value); err != nil {
			return zero, "", "", fmt.Errorf("validated OIDC identity %s: %w", name, err)
		}
	}
	canonicalTime := func(name string, value time.Time) (string, error) {
		if value.IsZero() || value.Location() != time.UTC || value.Nanosecond() != 0 {
			return "", fmt.Errorf("validated OIDC %s is not an exact whole-second UTC instant", name)
		}
		encoded := value.Format(time.RFC3339Nano)
		if _, err := parseCanonicalUTC(encoded); err != nil {
			return "", fmt.Errorf("validated OIDC %s: %w", name, err)
		}
		return encoded, nil
	}
	issuedAtUTC, err := canonicalTime("issued_at", identity.IssuedAt)
	if err != nil {
		return zero, "", "", err
	}
	notBeforeUTC, err := canonicalTime("not_before", identity.NotBefore)
	if err != nil {
		return zero, "", "", err
	}
	expiresAtUTC, err := canonicalTime("expires_at", identity.ExpiresAt)
	if err != nil {
		return zero, "", "", err
	}
	if !identity.IssuedAt.Before(identity.ExpiresAt) || !identity.NotBefore.Before(identity.ExpiresAt) {
		return zero, "", "", errors.New("validated OIDC identity has an invalid token lifetime")
	}
	envelopeIssuedAt, err := parseCanonicalUTC(request.envelope.IssuedAtUTC)
	if err != nil {
		return zero, "", "", fmt.Errorf("validated authentication envelope timestamp: %w", err)
	}
	if envelopeIssuedAt.Before(identity.IssuedAt) || !envelopeIssuedAt.Before(identity.ExpiresAt) {
		return zero, "", "", errors.New("validated authentication envelope timestamp is outside the token lifetime")
	}
	jtiHash := sha256.Sum256([]byte(identity.JTI))
	challengeHash := sha256.Sum256([]byte(request.envelope.Challenge))
	statement := validatedAuthorizationStatement{
		Schema: ValidatedAuthorizationSchema, Issuer: identity.Issuer, Audience: identity.Audience,
		Subject: identity.Subject, Actor: identity.Actor, ActorID: identity.ActorID, BaseRef: identity.BaseRef,
		Repository: identity.Repository, RepositoryID: identity.RepositoryID,
		RepositoryOwner: identity.RepositoryOwner, RepositoryOwnerID: identity.RepositoryOwnerID,
		RepositoryVisibility: identity.RepositoryVisibility, Environment: identity.Environment, Ref: identity.Ref,
		RefType: identity.RefType, HeadRef: identity.HeadRef,
		WorkflowRef: identity.WorkflowRef, WorkflowSHA: identity.WorkflowSHA, Workflow: identity.Workflow,
		SHA: identity.SHA, EventName: identity.EventName, RunID: identity.RunID, RunNumber: identity.RunNumber,
		RunAttempt: identity.RunAttempt, RunnerEnvironment: identity.RunnerEnvironment,
		Method: request.envelope.Method, OperationBodySHA256: SHA256Hex(request.bodyDigest),
		AuthEnvelopeSHA256: SHA256Hex(request.envelopeDigest), JTIHashSHA256: SHA256Hex(jtiHash),
		ChallengeHashSHA256: SHA256Hex(challengeHash), EnvelopeIssuedAtUTC: request.envelope.IssuedAtUTC,
		TokenIssuedAtUTC: issuedAtUTC, TokenNotBeforeUTC: notBeforeUTC, TokenExpiresAtUTC: expiresAtUTC,
	}
	canonical, err := json.Marshal(statement)
	if err != nil {
		return zero, "", "", fmt.Errorf("encode validated authorization statement: %w", err)
	}
	return domainHash("validated-authorization/v1", canonical), notBeforeUTC, expiresAtUTC, nil
}

func (c *Controller) requireOwnedOperation(body OperationBody) error {
	if body.Role != c.role {
		return errors.New("operation role does not belong to this authority")
	}
	if (c.role == RoleWriter && body.Action != ActionMarkerCAS) || (c.role == RoleObserver && body.Action != ActionMarkerRead) {
		return errors.New("operation action does not belong to this authority")
	}
	return nil
}

func (c *Controller) prepareCapabilityPayload(request verifiedRequest) ([]byte, [32]byte, [32]byte, error) {
	binding := c.brokerBinding
	predecessorKind, predecessorSHA256, predecessorDigest, err := binding.ProtectedPredecessor.fields()
	if err != nil {
		return nil, [32]byte{}, [32]byte{}, err
	}
	executionClaim := executionClaimDigest(request.bodyDigest, predecessorDigest, request.authorizationDigest, binding)
	payload := BrokerCapabilityPayload{
		Schema: BrokerCapabilitySchema, BrokerRole: binding.Role, Phase: request.body.Phase,
		Generation: request.body.Generation, OperationID: request.body.OperationID,
		OperationBodySHA256: SHA256Hex(request.bodyDigest), AuthorizationSHA256: SHA256Hex(request.authorizationDigest),
		AuthorizationIssuedAtUTC: request.envelope.IssuedAtUTC, TokenNotBeforeUTC: request.tokenNotBeforeUTC,
		AuthorizationNotAfterUTC: request.authorizationNotAfterUTC,
		BrokerIdentitySHA256:     binding.IdentitySHA256, MTLSPeerSHA256: binding.MTLSPeerSHA256,
		ProtectedPredecessorKind: predecessorKind, ProtectedPredecessorSHA256: predecessorSHA256,
		ExecutionClaimSHA256: SHA256Hex(executionClaim),
	}
	canonical, err := json.Marshal(payload)
	if err != nil {
		return nil, [32]byte{}, [32]byte{}, err
	}
	return canonical, predecessorDigest, executionClaim, nil
}

func (c *Controller) signPreparedCapability(ctx context.Context, canonical []byte) (BrokerCapability, [32]byte, error) {
	signature, err := c.signer.Sign(ctx, DomainBrokerCapability, canonical)
	if err != nil {
		return BrokerCapability{}, [32]byte{}, fmt.Errorf("sign broker capability: %w", err)
	}
	if err := VerifyCanonical(c.signerPublicKey, DomainBrokerCapability, canonical, signature); err != nil {
		return BrokerCapability{}, [32]byte{}, fmt.Errorf("self-verify broker capability: %w", err)
	}
	capability := BrokerCapability{Payload: canonical, Signature: signature}
	return capability, signedRecordDigest("broker-capability-record", canonical, signature), nil
}

type verifiedBrokerReadback struct {
	RecordSHA256            [32]byte
	MarkerSHA256            [32]byte
	PredecessorSHA256       [32]byte
	ContinuationClaimSHA256 [32]byte
	Outcome                 string
}

func (c *Controller) verifyBrokerReadback(record storedOperation, body OperationBody, readback BrokerReadback) (verifiedBrokerReadback, error) {
	if len(readback.SealedReconciliation) == 0 || len(readback.SealedReconciliation) > MaxSealedMaterialBytes {
		return verifiedBrokerReadback{}, errors.New("opaque sealed reconciliation material is empty or oversized")
	}
	if len(record.CapabilityPayload) == 0 || len(record.CapabilitySignature) == 0 {
		return verifiedBrokerReadback{}, errors.New("durable broker capability is missing")
	}
	if err := VerifyCanonical(c.signerPublicKey, DomainBrokerCapability, record.CapabilityPayload, record.CapabilitySignature); err != nil {
		return verifiedBrokerReadback{}, fmt.Errorf("stored capability signature: %w", err)
	}
	if signedRecordDigest("broker-capability-record", record.CapabilityPayload, record.CapabilitySignature) != record.CapabilitySHA256 {
		return verifiedBrokerReadback{}, errors.New("stored capability digest mismatch")
	}
	var capability BrokerCapabilityPayload
	if err := decodeCanonical(record.CapabilityPayload, MaxBrokerPayloadBytes, &capability); err != nil {
		return verifiedBrokerReadback{}, fmt.Errorf("stored capability payload: %w", err)
	}
	binding := c.brokerBinding
	predecessorKind, predecessorSHA256, predecessorDigest, err := binding.ProtectedPredecessor.fields()
	if err != nil {
		return verifiedBrokerReadback{}, err
	}
	capabilityChecks := []struct{ name, actual, expected string }{
		{"schema", capability.Schema, BrokerCapabilitySchema}, {"broker_role", capability.BrokerRole, binding.Role},
		{"phase", capability.Phase, body.Phase}, {"operation_id", capability.OperationID, body.OperationID},
		{"operation_body_sha256", capability.OperationBodySHA256, SHA256Hex(record.BodySHA256)},
		{"broker_identity_sha256", capability.BrokerIdentitySHA256, binding.IdentitySHA256},
		{"mtls_peer_sha256", capability.MTLSPeerSHA256, binding.MTLSPeerSHA256},
		{"protected_predecessor_kind", capability.ProtectedPredecessorKind, predecessorKind},
		{"protected_predecessor_sha256", capability.ProtectedPredecessorSHA256, predecessorSHA256},
		{"execution_claim_sha256", capability.ExecutionClaimSHA256, SHA256Hex(record.ExecutionClaimSHA256)},
	}
	for _, check := range capabilityChecks {
		if check.actual != check.expected {
			return verifiedBrokerReadback{}, fmt.Errorf("stored capability %s binding mismatch", check.name)
		}
	}
	if predecessorDigest != record.PredecessorSHA256 {
		return verifiedBrokerReadback{}, errors.New("stored protected predecessor digest mismatch")
	}
	if record.ExecutionClaimCount != 1 || record.ExecutionClaimSHA256 == ([32]byte{}) {
		return verifiedBrokerReadback{}, errors.New("stored execution claim fence is invalid")
	}
	if capability.Generation != body.Generation {
		return verifiedBrokerReadback{}, errors.New("stored capability generation mismatch")
	}
	authorizationDigest, err := parseSHA256(capability.AuthorizationSHA256)
	if err != nil || authorizationDigest == ([32]byte{}) {
		return verifiedBrokerReadback{}, errors.New("stored capability authorization digest is invalid")
	}
	authorizationIssuedAt, err := parseCanonicalUTC(capability.AuthorizationIssuedAtUTC)
	if err != nil {
		return verifiedBrokerReadback{}, errors.New("stored capability authorization issued bound is invalid")
	}
	tokenNotBefore, err := parseCanonicalUTC(capability.TokenNotBeforeUTC)
	if err != nil {
		return verifiedBrokerReadback{}, errors.New("stored capability token not-before bound is invalid")
	}
	authorizationNotAfter, err := parseCanonicalUTC(capability.AuthorizationNotAfterUTC)
	if err != nil || !authorizationIssuedAt.Before(authorizationNotAfter) || !tokenNotBefore.Before(authorizationNotAfter) {
		return verifiedBrokerReadback{}, errors.New("stored capability authorization expiry is invalid")
	}
	authorizationLowerBound := authorizationIssuedAt
	if tokenNotBefore.After(authorizationLowerBound) {
		authorizationLowerBound = tokenNotBefore
	}
	var payload BrokerReadbackPayload
	if err := decodeCanonical(readback.Payload, MaxBrokerPayloadBytes, &payload); err != nil {
		return verifiedBrokerReadback{}, fmt.Errorf("broker readback payload: %w", err)
	}
	if err := VerifyCanonical(binding.PublicKey, DomainBrokerReadback, readback.Payload, readback.Signature); err != nil {
		return verifiedBrokerReadback{}, fmt.Errorf("broker readback signature: %w", err)
	}
	sealedDigest := sha256.Sum256(readback.SealedReconciliation)
	checks := []struct{ name, actual, expected string }{
		{"schema", payload.Schema, BrokerReadbackSchema}, {"broker_role", payload.BrokerRole, binding.Role},
		{"phase", payload.Phase, body.Phase}, {"operation_id", payload.OperationID, body.OperationID},
		{"operation_body_sha256", payload.OperationBodySHA256, SHA256Hex(record.BodySHA256)},
		{"capability_sha256", payload.CapabilitySHA256, SHA256Hex(record.CapabilitySHA256)},
		{"broker_identity_sha256", payload.BrokerIdentitySHA256, binding.IdentitySHA256},
		{"mtls_peer_sha256", payload.MTLSPeerSHA256, binding.MTLSPeerSHA256},
		{"protected_predecessor_kind", payload.ProtectedPredecessorKind, predecessorKind},
		{"protected_predecessor_sha256", payload.ProtectedPredecessorSHA256, predecessorSHA256},
		{"execution_claim_sha256", payload.ExecutionClaimSHA256, SHA256Hex(record.ExecutionClaimSHA256)},
		{"sealed_reconciliation_sha256", payload.SealedReconciliationSHA256, hex.EncodeToString(sealedDigest[:])},
	}
	for _, check := range checks {
		if check.actual != check.expected {
			return verifiedBrokerReadback{}, fmt.Errorf("broker readback %s binding mismatch", check.name)
		}
	}
	if payload.Generation != body.Generation {
		return verifiedBrokerReadback{}, errors.New("broker readback generation mismatch")
	}
	if payload.Outcome != BrokerOutcomeCommitted && payload.Outcome != BrokerOutcomeUnchangedPredecessor {
		return verifiedBrokerReadback{}, errors.New("broker readback outcome is not an approved fixed CAS result")
	}
	brokerActionAt, err := parseCanonicalUTC(payload.BrokerActionAtUTC)
	if err != nil {
		return verifiedBrokerReadback{}, errors.New("broker action timestamp is invalid")
	}
	if brokerActionAt.Before(authorizationLowerBound) || !brokerActionAt.Before(authorizationNotAfter) {
		return verifiedBrokerReadback{}, errors.New("broker action timestamp is outside the inclusive-issued-and-not-before/exclusive-expiry authorization interval")
	}
	if payload.MarkerMaterialBytes != 32 {
		return verifiedBrokerReadback{}, errors.New("broker must attest an internally generated marker of exactly 32 bytes")
	}
	markerDigest, err := parseSHA256(payload.MarkerSHA256)
	if err != nil || markerDigest == ([32]byte{}) {
		return verifiedBrokerReadback{}, errors.New("broker marker digest is invalid")
	}
	var continuationClaim [32]byte
	if payload.ContinuationClaimSHA256 != "" {
		continuationClaim, err = parseSHA256(payload.ContinuationClaimSHA256)
		if err != nil || continuationClaim == ([32]byte{}) {
			return verifiedBrokerReadback{}, errors.New("broker continuation claim digest is invalid")
		}
	}
	return verifiedBrokerReadback{
		RecordSHA256: signedRecordDigest("broker-readback-record", readback.Payload, readback.Signature),
		MarkerSHA256: markerDigest, PredecessorSHA256: predecessorDigest,
		ContinuationClaimSHA256: continuationClaim, Outcome: payload.Outcome,
	}, nil
}

func signedRecordDigest(domain string, payload, signature []byte) [32]byte {
	combined := make([]byte, 0, len(payload)+1+len(signature))
	combined = append(combined, payload...)
	combined = append(combined, 0)
	combined = append(combined, signature...)
	return domainHash(domain, combined)
}

func parseSHA256(value string) ([32]byte, error) {
	var result [32]byte
	if !hexSHA256Pattern.MatchString(value) {
		return result, errors.New("not a lowercase SHA-256")
	}
	decoded, err := hex.DecodeString(value)
	if err != nil || len(decoded) != len(result) {
		return result, errors.New("invalid SHA-256")
	}
	copy(result[:], decoded)
	return result, nil
}

func quarantine(tx LedgerTransaction, record *storedOperation, callDigest [32]byte, reason string, semantic error, snapshot *OperationSnapshot, semanticErr *error) error {
	record.State = StateQuarantined
	record.QuarantineReason = reason
	record.Revision++
	if err := tx.Save(*record); err != nil {
		return err
	}
	*semanticErr = semantic
	*snapshot = snapshotOf(*record)
	return tx.AppendAudit(auditFor(*record, callDigest, "operation-quarantined"))
}

func auditFor(record storedOperation, callDigest [32]byte, event string) AuditEvent {
	return AuditEvent{OperationID: record.OperationID, OperationBodySHA256: SHA256Hex(record.BodySHA256), CallSHA256: SHA256Hex(callDigest), Event: event, State: record.State}
}

func rejectedAudit(request verifiedRequest, event string) AuditEvent {
	return AuditEvent{OperationID: request.body.OperationID, OperationBodySHA256: SHA256Hex(request.bodyDigest), CallSHA256: SHA256Hex(request.envelopeDigest), Event: event}
}

type FaultPoint string

const (
	FaultBeforeCommit              FaultPoint = "before-commit"
	FaultAfterCommit               FaultPoint = "after-commit"
	FaultSave                      FaultPoint = "save"
	FaultAppendAudit               FaultPoint = "append-audit"
	FaultEraseSealedReconciliation FaultPoint = "erase-sealed-reconciliation"
)

// FaultInjectingStore is a transactional in-memory test oracle only. It is
// accepted solely by newTestController; NewController is unavailable in Gate A.
type FaultInjectingStore struct {
	mu               sync.Mutex
	operations       map[string]storedOperation
	authClaims       map[string][32]byte
	issuedChallenges map[string]issuedChallenge
	audit            []AuditEvent
	fault            FaultPoint
	faultCount       int
	erasedBytes      int
}

type issuedChallenge struct {
	BindingSHA256 [32]byte
	Consumed      bool
}

func NewFaultInjectingStore() *FaultInjectingStore {
	return &FaultInjectingStore{
		operations: make(map[string]storedOperation), authClaims: make(map[string][32]byte),
		issuedChallenges: make(map[string]issuedChallenge),
	}
}

func (s *FaultInjectingStore) Guarantees() DurabilityGuarantees {
	return DurabilityGuarantees{Transactional: true, CASFencing: true, AppendOnlyAudit: true, TestOnly: true}
}

func (s *FaultInjectingStore) Inject(point FaultPoint, count int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fault = point
	s.faultCount = count
}

func (s *FaultInjectingStore) shouldFault(point FaultPoint) bool {
	if s.fault == point && s.faultCount > 0 {
		s.faultCount--
		return true
	}
	return false
}

// IssueChallenge is the test-oracle durable issuance ledger. It stores only a
// hash and exact body/method/role/authority-root binding, never the bearer
// challenge value.
func (s *FaultInjectingStore) IssueChallenge(ctx context.Context, challengeSHA256, bindingSHA256 [32]byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := hex.EncodeToString(challengeSHA256[:])
	if _, exists := s.issuedChallenges[key]; exists {
		return ErrReplay
	}
	s.issuedChallenges[key] = issuedChallenge{BindingSHA256: bindingSHA256}
	return nil
}

// ClaimAuthentication atomically commits both replay keys before any
// operation transaction begins. It deliberately does not share transaction
// fault injection with operation semantics: those later failures must never
// roll back an already validated authentication burn.
func (s *FaultInjectingStore) ClaimAuthentication(ctx context.Context, jtiSHA256, challengeSHA256, bindingSHA256, callSHA256 [32]byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	jtiKey := "jti:" + hex.EncodeToString(jtiSHA256[:])
	if _, exists := s.authClaims[jtiKey]; exists {
		return ErrReplay
	}
	challengeKey := hex.EncodeToString(challengeSHA256[:])
	issued, exists := s.issuedChallenges[challengeKey]
	if !exists {
		return ErrChallengeNotIssued
	}
	if issued.Consumed {
		return ErrReplay
	}
	if issued.BindingSHA256 != bindingSHA256 {
		return ErrChallengeBinding
	}
	issued.Consumed = true
	s.issuedChallenges[challengeKey] = issued
	s.authClaims[jtiKey] = callSHA256
	return nil
}

func (s *FaultInjectingStore) Transact(ctx context.Context, operationID string, fn func(LedgerTransaction) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	operations := cloneOperations(s.operations)
	tx := &faultTransaction{operationID: operationID, operations: operations, audit: append([]AuditEvent(nil), s.audit...), shouldFault: s.shouldFault}
	if err := fn(tx); err != nil {
		return err
	}
	if s.shouldFault(FaultBeforeCommit) {
		return ErrInjectedCrash
	}
	s.operations = operations
	s.audit = tx.audit
	s.erasedBytes += tx.erasedBytes
	if s.shouldFault(FaultAfterCommit) {
		return ErrInjectedCrash
	}
	return nil
}

func (s *FaultInjectingStore) Read(ctx context.Context, operationID string) (OperationSnapshot, bool, error) {
	if err := ctx.Err(); err != nil {
		return OperationSnapshot{}, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	record, ok := s.operations[operationID]
	if !ok {
		return OperationSnapshot{}, false, nil
	}
	return snapshotOf(record), true, nil
}

func (s *FaultInjectingStore) Audit() []AuditEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]AuditEvent(nil), s.audit...)
}

func (s *FaultInjectingStore) ErasedBytes() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.erasedBytes
}

type faultTransaction struct {
	operationID string
	operations  map[string]storedOperation
	audit       []AuditEvent
	erasedBytes int
	shouldFault func(FaultPoint) bool
}

func (tx *faultTransaction) Load() (storedOperation, bool) {
	record, ok := tx.operations[tx.operationID]
	return cloneOperation(record), ok
}

func (tx *faultTransaction) Save(record storedOperation) error {
	if tx.shouldFault != nil && tx.shouldFault(FaultSave) {
		return ErrInjectedCrash
	}
	if record.OperationID != tx.operationID {
		return errors.New("transaction operation identifier mismatch")
	}
	tx.operations[tx.operationID] = cloneOperation(record)
	return nil
}
func (tx *faultTransaction) AppendAudit(event AuditEvent) error {
	if tx.shouldFault != nil && tx.shouldFault(FaultAppendAudit) {
		return ErrInjectedCrash
	}
	event.Sequence = uint64(len(tx.audit) + 1)
	tx.audit = append(tx.audit, event)
	return nil
}

func (tx *faultTransaction) EraseSealedReconciliation() error {
	if tx.shouldFault != nil && tx.shouldFault(FaultEraseSealedReconciliation) {
		return ErrInjectedCrash
	}
	record, exists := tx.operations[tx.operationID]
	if !exists {
		return ErrOperationMissing
	}
	for index := range record.sealedReconciliation {
		record.sealedReconciliation[index] = 0
		tx.erasedBytes++
	}
	record.sealedReconciliation = nil
	record.SealedMaterialErased = true
	tx.operations[tx.operationID] = record
	return nil
}

func cloneOperation(record storedOperation) storedOperation {
	record.CanonicalBody = append([]byte(nil), record.CanonicalBody...)
	record.CapabilityPayload = append([]byte(nil), record.CapabilityPayload...)
	record.CapabilitySignature = append([]byte(nil), record.CapabilitySignature...)
	record.sealedReconciliation = append([]byte(nil), record.sealedReconciliation...)
	record.ReceiptPayload = append([]byte(nil), record.ReceiptPayload...)
	record.ReceiptSignature = append([]byte(nil), record.ReceiptSignature...)
	return record
}

func cloneOperations(source map[string]storedOperation) map[string]storedOperation {
	result := make(map[string]storedOperation, len(source))
	for key, record := range source {
		result[key] = cloneOperation(record)
	}
	return result
}

type testEd25519Signer struct {
	private ed25519.PrivateKey
}

func newTestEd25519Signer(seed [32]byte) *testEd25519Signer {
	return &testEd25519Signer{private: ed25519.NewKeyFromSeed(seed[:])}
}

func (s *testEd25519Signer) Guarantees() SignerGuarantees {
	return SignerGuarantees{RoleSeparated: true, Deterministic: true, NoAutomaticRetries: true, TestOnly: true}
}

func (s *testEd25519Signer) PublicKey() ed25519.PublicKey {
	return append(ed25519.PublicKey(nil), s.private.Public().(ed25519.PublicKey)...)
}

func (s *testEd25519Signer) Sign(_ context.Context, domain string, payload []byte) ([]byte, error) {
	return SignCanonical(s.private, domain, payload)
}

var _ DurableStore = (*FaultInjectingStore)(nil)
var _ ReceiptSigner = (*testEd25519Signer)(nil)
