package model

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"
)

// markerContinuationCandidate is an internal status sentinel. It is never a
// provider observation or public receipt value; it asks the durable authority
// to claim the one prepared+issued same-operation continuation.
var markerContinuationCandidate = HashString("marker-continuation-candidate/v2")

// DurableEffectAuthority is the protocol's persistence boundary. A real
// implementation would have to be transactional, durable, fenced,
// append-only, encrypted, recoverable, and retry-free. Gate A deliberately
// provides no production implementation; ProtocolCrashOracle below is only a
// reconstructable, fault-injecting test oracle.
type DurableEffectAuthority interface {
	AuthorizeAndConsume(context.Context, MutationRequest, AuthEnvelope, FaultMode, Digest, func() (Digest, error)) (Digest, error)
	AuthorizePreparedAndConsume(context.Context, MutationRequest, AuthEnvelope, FaultMode, Digest, func() error, func() (Digest, error)) (Digest, error)
	ReconcileStatus(context.Context, MutationRequest, AuthEnvelope, func() ([]Digest, error)) (Digest, error)
	Completed(MutationRequest) bool
	IssueCount(MutationRequest) int
	CompletionTime(MutationRequest) (time.Time, bool)
	BindProjection(OperationBody, Digest) error
	CapabilityRevoked(Digest) bool
}

type protocolEffectRecord struct {
	RequestDigest       Digest
	Effect              Effect
	Terminal            Digest
	State               MutationState
	Result              Digest
	IssueCount          int
	Prepared            bool
	Issued              bool
	ContinuationClaimed bool
	Ambiguity           string
	CompletedAt         time.Time
}

// issuedAuthorityChallenge is the durable, hash-only authorization grant.
// The bearer nonce is never retained. A challenge is usable for exactly one
// request digest and one call kind under the oracle's bound authority root.
type issuedAuthorityChallenge struct {
	RequestSHA256 Digest
	Call          CallKind
	Role          UnitRole
	RootSHA256    Digest
	Consumed      bool
}

// CompletionWitness is the portable, signer-confined proof that one exact
// request/result/terminal tuple reached the durable completed state. It
// exposes no ledger handle or secret material.
type CompletionWitness struct {
	Version                                     string
	Role                                        UnitRole
	Effect                                      Effect
	State                                       MutationState
	RequestSHA256, ResultSHA256, TerminalSHA256 Digest
	LedgerSHA256, RootSHA256, OracleSHA256      Digest
	Sequence                                    uint64
	CompletedAt                                 time.Time
	Signature                                   []byte
}

func (w CompletionWitness) payload() []byte {
	when, _ := canonicalTime(w.CompletedAt)
	return []byte("durable-completion-witness/v2\x00" + w.Version + "\x00" + string(w.Role) + "\x00" + string(w.Effect) + "\x00" + string(w.State) + "\x00" +
		w.RequestSHA256.String() + "\x00" + w.ResultSHA256.String() + "\x00" + w.TerminalSHA256.String() + "\x00" +
		w.LedgerSHA256.String() + "\x00" + w.RootSHA256.String() + "\x00" + w.OracleSHA256.String() + "\x00" +
		fmt.Sprint(w.Sequence) + "\x00" + when)
}

func (w CompletionWitness) Digest() Digest {
	return HashBytes(append(w.payload(), w.Signature...))
}

func cloneCompletionWitness(witness CompletionWitness) CompletionWitness {
	witness.Signature = append([]byte(nil), witness.Signature...)
	return witness
}

func verifyCompletionWitness(public ed25519.PublicKey, role UnitRole, ledger, root, oracle Digest, request MutationRequest, result, terminal Digest, witness CompletionWitness) error {
	requestDigest, err := request.Digest()
	_, timeErr := canonicalTime(witness.CompletedAt)
	if err != nil || timeErr != nil || len(public) != ed25519.PublicKeySize ||
		witness.Version != ProtocolVersion || witness.Role != role || witness.Effect != request.Effect || witness.State != MutationCompleted ||
		witness.RequestSHA256 != requestDigest || witness.ResultSHA256 != result || result.IsZero() || witness.TerminalSHA256 != terminal ||
		witness.LedgerSHA256 != ledger || witness.RootSHA256 != root || witness.OracleSHA256 != oracle ||
		witness.Sequence == 0 ||
		len(witness.Signature) != ed25519.SignatureSize || !ed25519.Verify(public, witness.payload(), witness.Signature) {
		return ErrInvalid
	}
	return nil
}

type terminalBinding struct {
	OperationBody Digest
	Evidence      Digest
}

type recoveryAdmissionBinding struct {
	OperationBody   Digest
	TerminalBinding Digest
	ForkRequest     Digest
	ForkResult      Digest
	ForkProof       Digest
	RecoveryTarget  Digest
	ForkProvider    Digest
	Payload         Digest
}

type storedCapability struct {
	RequestSHA256 Digest
	Capability    BrokerCapability
}

type storedWriterReceipt struct {
	RequestSHA256 Digest
	Receipt       WriterReceipt
}

type storedTerminalReceipt struct {
	RequestSHA256 Digest
	Receipt       WriterTerminalReceipt
}

type MarkerAbortWitness struct {
	Version                                              string
	AbortRequestSHA256, MarkerRequestSHA256              Digest
	OperationBodySHA256, MarkerSHA256                    Digest
	SourceTargetSHA256, CapabilitySHA256                 Digest
	SealerSHA256, ReasonSHA256, LedgerSHA256, RootSHA256 Digest
	OracleSHA256                                         Digest
	AuthorizedAt                                         time.Time
	Signature                                            []byte
}

// lifecycleSealerRevocationIntent is the exact signed pre-effect intent for
// the writer-lifecycle wrapping-key revocation. The provider-side revocation
// result is consumable only together with the exact terminal sealer status.
type lifecycleSealerRevocationIntent struct {
	Version                                                           string
	RequestSHA256, SealerSHA256, ProviderSHA256, ProviderConfigSHA256 Digest
	LedgerSHA256, RootSHA256, OracleSHA256                            Digest
	AuthorizedAt                                                      time.Time
	Signature                                                         []byte
}

func (i lifecycleSealerRevocationIntent) payload() []byte {
	when, _ := canonicalTime(i.AuthorizedAt)
	return []byte("lifecycle-sealer-revocation-intent/v2\x00" + i.Version + "\x00" + i.RequestSHA256.String() + "\x00" +
		i.SealerSHA256.String() + "\x00" + i.ProviderSHA256.String() + "\x00" + i.ProviderConfigSHA256.String() + "\x00" +
		i.LedgerSHA256.String() + "\x00" + i.RootSHA256.String() + "\x00" + i.OracleSHA256.String() + "\x00" + when)
}

func (i lifecycleSealerRevocationIntent) Digest() Digest {
	return HashBytes(append(i.payload(), i.Signature...))
}

func cloneLifecycleSealerRevocationIntent(intent lifecycleSealerRevocationIntent) lifecycleSealerRevocationIntent {
	intent.Signature = append([]byte(nil), intent.Signature...)
	return intent
}

func (w MarkerAbortWitness) payload() []byte {
	when, _ := canonicalTime(w.AuthorizedAt)
	return []byte("marker-abort-witness/v2\x00" + w.Version + "\x00" + w.AbortRequestSHA256.String() + "\x00" + w.MarkerRequestSHA256.String() + "\x00" +
		w.OperationBodySHA256.String() + "\x00" + w.MarkerSHA256.String() + "\x00" + w.SourceTargetSHA256.String() + "\x00" + w.CapabilitySHA256.String() + "\x00" + w.SealerSHA256.String() + "\x00" +
		w.ReasonSHA256.String() + "\x00" + w.LedgerSHA256.String() + "\x00" + w.RootSHA256.String() + "\x00" + w.OracleSHA256.String() + "\x00" + when)
}

func (w MarkerAbortWitness) Digest() Digest {
	return HashBytes(append(w.payload(), w.Signature...))
}

type pendingMarkerAbort struct {
	MarkerRequestKey    string
	MarkerRequestSHA256 Digest
	OperationBodySHA256 Digest
	MarkerSHA256        Digest
	SourceTargetSHA256  Digest
	CapabilitySHA256    Digest
	SealerSHA256        Digest
	ReasonSHA256        Digest
}

// ProtocolCrashOracle persists for as long as the test object is retained, so
// reconstructed controllers share authorization, effect, and receipt state.
// It is not durable across process loss and must never be represented as a
// production adapter.
type ProtocolCrashOracle struct {
	self                               *ProtocolCrashOracle
	mu                                 sync.Mutex
	now                                func() time.Time
	identity                           Digest
	records                            map[string]protocolEffectRecord
	operationClaims                    map[string]Digest
	baseOperationClaims                map[string]Digest
	quarantined                        map[string]bool
	usedJTIs                           map[Digest]bool
	usedChallenges                     map[Digest]bool
	issuedChallenges                   map[Digest]issuedAuthorityChallenge
	projectionBindings                 map[Digest]Digest
	terminalConsumption                map[Digest]Digest
	revokedCapabilities                map[Digest]bool
	sealedMarkerTokens                 map[Digest]sealedMarkerToken
	erasedMarkerTokens                 map[Digest]Digest
	publishedTerminals                 map[Digest]terminalBinding
	publishedAdmissions                map[Digest]recoveryAdmissionBinding
	recoveryAdmissions                 map[Digest]RecoveryAdmission
	recoveryRequests                   map[string]Digest
	observerReceipts                   map[Digest]ObserverReceipt
	observerRequests                   map[string]Digest
	terminalObservations               map[Digest]WriterTerminalEvidence
	terminalObservationRequests        map[string]Digest
	recoveryAuthorizations             map[Digest]RecoveryAdmissionAuthorization
	authorizationRequests              map[string]Digest
	signerRole                         UnitRole
	signerLedger                       Digest
	signerRoot                         Digest
	signerPrivate                      ed25519.PrivateKey
	completionSequence                 uint64
	completionWitnesses                map[string]CompletionWitness
	issuedCapabilities                 map[string]storedCapability
	preparedWriterReceipts             map[string]storedWriterReceipt
	terminalReceipts                   map[string]storedTerminalReceipt
	pendingMarkerAborts                map[Digest]pendingMarkerAbort
	preparedMarkerAborts               map[string]MarkerAbortWitness
	markerAbortWitnesses               map[Digest]MarkerAbortWitness
	preparedLifecycleSealerRevocations map[string]lifecycleSealerRevocationIntent
}

var protocolCrashOracleSequence atomic.Uint64
var boundProtocolCrashOracles sync.Map

func NewProtocolCrashOracle(now func() time.Time) (*ProtocolCrashOracle, error) {
	sequence := protocolCrashOracleSequence.Add(1)
	return newProtocolCrashOracle(now, HashString(fmt.Sprintf("protocol-crash-oracle/v2\x00%d", sequence)))
}

// newBoundProtocolCrashOracle is available only to the Gate-A package and
// binds the fault oracle to an immutable protected durable-store identity.
// It is not a production constructor.
func newBoundProtocolCrashOracle(now func() time.Time, identity Digest) (*ProtocolCrashOracle, error) {
	oracle, err := newProtocolCrashOracle(now, identity)
	if err != nil {
		return nil, err
	}
	if _, loaded := boundProtocolCrashOracles.LoadOrStore(identity, oracle); loaded {
		return nil, ErrRoleIsolation
	}
	return oracle, nil
}

func newProtocolCrashOracle(now func() time.Time, identity Digest) (*ProtocolCrashOracle, error) {
	if now == nil || identity.IsZero() {
		return nil, ErrInvalid
	}
	oracle := &ProtocolCrashOracle{
		now: now, identity: identity,
		records: map[string]protocolEffectRecord{}, operationClaims: map[string]Digest{}, baseOperationClaims: map[string]Digest{},
		quarantined: map[string]bool{}, usedJTIs: map[Digest]bool{}, usedChallenges: map[Digest]bool{},
		issuedChallenges:   map[Digest]issuedAuthorityChallenge{},
		projectionBindings: map[Digest]Digest{}, terminalConsumption: map[Digest]Digest{},
		revokedCapabilities: map[Digest]bool{}, sealedMarkerTokens: map[Digest]sealedMarkerToken{}, erasedMarkerTokens: map[Digest]Digest{},
		publishedTerminals:  map[Digest]terminalBinding{},
		publishedAdmissions: map[Digest]recoveryAdmissionBinding{},
		recoveryAdmissions:  map[Digest]RecoveryAdmission{}, recoveryRequests: map[string]Digest{},
		observerReceipts: map[Digest]ObserverReceipt{}, observerRequests: map[string]Digest{},
		terminalObservations: map[Digest]WriterTerminalEvidence{}, terminalObservationRequests: map[string]Digest{},
		recoveryAuthorizations: map[Digest]RecoveryAdmissionAuthorization{}, authorizationRequests: map[string]Digest{},
		completionWitnesses: map[string]CompletionWitness{},
		issuedCapabilities:  map[string]storedCapability{}, preparedWriterReceipts: map[string]storedWriterReceipt{},
		terminalReceipts:                   map[string]storedTerminalReceipt{},
		pendingMarkerAborts:                map[Digest]pendingMarkerAbort{},
		preparedMarkerAborts:               map[string]MarkerAbortWitness{},
		markerAbortWitnesses:               map[Digest]MarkerAbortWitness{},
		preparedLifecycleSealerRevocations: map[string]lifecycleSealerRevocationIntent{},
	}
	oracle.self = oracle
	return oracle, nil
}

func cloneBrokerCapability(capability BrokerCapability) BrokerCapability {
	capability.Signature = append([]byte(nil), capability.Signature...)
	return capability
}

func cloneWriterReceipt(receipt WriterReceipt) WriterReceipt {
	receipt.Signature = append([]byte(nil), receipt.Signature...)
	return receipt
}

func cloneWriterTerminalReceipt(receipt WriterTerminalReceipt) WriterTerminalReceipt {
	receipt.Signature = append([]byte(nil), receipt.Signature...)
	return receipt
}

func cloneWriterTerminalEvidence(evidence WriterTerminalEvidence) WriterTerminalEvidence {
	evidence.DeletionCompletionWitness = cloneCompletionWitness(evidence.DeletionCompletionWitness)
	evidence.WrappingKeySealerIntent = cloneLifecycleSealerRevocationIntent(evidence.WrappingKeySealerIntent)
	evidence.WrappingKeySealerCompletionWitness = cloneCompletionWitness(evidence.WrappingKeySealerCompletionWitness)
	evidence.WrappingKeyProviderCompletionWitness = cloneCompletionWitness(evidence.WrappingKeyProviderCompletionWitness)
	if evidence.DurableCompletionAt != nil {
		cloned := make(map[Effect]time.Time, len(evidence.DurableCompletionAt))
		for effect, completedAt := range evidence.DurableCompletionAt {
			cloned[effect] = completedAt
		}
		evidence.DurableCompletionAt = cloned
	}
	return evidence
}

func (o *ProtocolCrashOracle) registerCapabilityLocked(request MutationRequest, capability BrokerCapability) (Digest, error) {
	requestDigest, err := request.Digest()
	if err != nil || len(capability.Signature) != ed25519.SignatureSize {
		return Digest{}, ErrInvalid
	}
	digest := capability.Digest()
	key := request.key()
	binding := storedCapability{RequestSHA256: requestDigest, Capability: cloneBrokerCapability(capability)}
	if prior, ok := o.issuedCapabilities[key]; ok {
		if prior.RequestSHA256 != requestDigest || prior.Capability.Digest() != digest || !bytes.Equal(prior.Capability.Signature, capability.Signature) {
			return Digest{}, ErrQuarantined
		}
		return digest, nil
	}
	o.issuedCapabilities[key] = binding
	return digest, nil
}

func (o *ProtocolCrashOracle) capabilityForRequestLocked(request MutationRequest) (BrokerCapability, bool) {
	requestDigest, err := request.Digest()
	binding, ok := o.issuedCapabilities[request.key()]
	if err != nil || !ok || binding.RequestSHA256 != requestDigest {
		return BrokerCapability{}, false
	}
	return cloneBrokerCapability(binding.Capability), true
}

func (o *ProtocolCrashOracle) capabilityForRequest(request MutationRequest) (BrokerCapability, bool) {
	if !o.validInstance() {
		return BrokerCapability{}, false
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.capabilityForRequestLocked(request)
}

func (o *ProtocolCrashOracle) registerPreparedWriterReceiptLocked(request MutationRequest, receipt WriterReceipt) (Digest, error) {
	requestDigest, err := request.Digest()
	if err != nil || len(receipt.Signature) != ed25519.SignatureSize {
		return Digest{}, ErrInvalid
	}
	digest := writerReceiptDigest(receipt)
	binding := storedWriterReceipt{RequestSHA256: requestDigest, Receipt: cloneWriterReceipt(receipt)}
	if prior, ok := o.preparedWriterReceipts[request.key()]; ok {
		if prior.RequestSHA256 != requestDigest || writerReceiptDigest(prior.Receipt) != digest || !bytes.Equal(prior.Receipt.Signature, receipt.Signature) {
			return Digest{}, ErrQuarantined
		}
		return digest, nil
	}
	o.preparedWriterReceipts[request.key()] = binding
	return digest, nil
}

func (o *ProtocolCrashOracle) writerReceiptForRequestLocked(request MutationRequest) (WriterReceipt, bool) {
	requestDigest, err := request.Digest()
	binding, ok := o.preparedWriterReceipts[request.key()]
	if err != nil || !ok || binding.RequestSHA256 != requestDigest {
		return WriterReceipt{}, false
	}
	return cloneWriterReceipt(binding.Receipt), true
}

func (o *ProtocolCrashOracle) writerReceiptForRequest(request MutationRequest) (WriterReceipt, bool) {
	if !o.validInstance() {
		return WriterReceipt{}, false
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.writerReceiptForRequestLocked(request)
}

func (o *ProtocolCrashOracle) registerTerminalReceiptLocked(request MutationRequest, receipt WriterTerminalReceipt, evidence Digest) (Digest, error) {
	requestDigest, err := request.Digest()
	if err != nil || evidence.IsZero() || receipt.EvidenceSHA256 != evidence || len(receipt.Signature) != ed25519.SignatureSize {
		return Digest{}, ErrInvalid
	}
	digest := terminalReceiptDigest(receipt)
	if err := o.registerTerminalAndEraseLocked(digest, receipt.OperationBodySHA256, evidence); err != nil {
		return Digest{}, err
	}
	binding := storedTerminalReceipt{RequestSHA256: requestDigest, Receipt: cloneWriterTerminalReceipt(receipt)}
	if prior, ok := o.terminalReceipts[request.key()]; ok {
		if prior.RequestSHA256 != requestDigest || terminalReceiptDigest(prior.Receipt) != digest || !bytes.Equal(prior.Receipt.Signature, receipt.Signature) {
			return Digest{}, ErrQuarantined
		}
		return digest, nil
	}
	o.terminalReceipts[request.key()] = binding
	return digest, nil
}

func (o *ProtocolCrashOracle) terminalReceiptForRequestLocked(request MutationRequest) (WriterTerminalReceipt, bool) {
	requestDigest, err := request.Digest()
	binding, ok := o.terminalReceipts[request.key()]
	if err != nil || !ok || binding.RequestSHA256 != requestDigest {
		return WriterTerminalReceipt{}, false
	}
	return cloneWriterTerminalReceipt(binding.Receipt), true
}

func (o *ProtocolCrashOracle) terminalReceiptForRequest(request MutationRequest) (WriterTerminalReceipt, bool) {
	if !o.validInstance() {
		return WriterTerminalReceipt{}, false
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.terminalReceiptForRequestLocked(request)
}

func (o *ProtocolCrashOracle) registerTerminalObservationLocked(request MutationRequest, evidence WriterTerminalEvidence) (Digest, error) {
	requestDigest, err := request.Digest()
	digest, evidenceErr := evidence.Digest()
	if err != nil || evidenceErr != nil || digest.IsZero() {
		return Digest{}, ErrInvalid
	}
	if priorDigest, ok := o.terminalObservationRequests[request.key()]; ok && priorDigest != digest {
		o.quarantined[operationGenerationKey(request.Operation)] = true
		return Digest{}, ErrQuarantined
	}
	if prior, ok := o.terminalObservations[digest]; ok {
		priorBytes, priorErr := prior.CanonicalBytes()
		currentBytes, currentErr := evidence.CanonicalBytes()
		if priorErr != nil || currentErr != nil || !bytes.Equal(priorBytes, currentBytes) {
			o.quarantined[operationGenerationKey(request.Operation)] = true
			return Digest{}, ErrQuarantined
		}
	}
	_ = requestDigest
	o.terminalObservationRequests[request.key()] = digest
	o.terminalObservations[digest] = cloneWriterTerminalEvidence(evidence)
	return digest, nil
}

func (o *ProtocolCrashOracle) terminalObservationForRequestLocked(request MutationRequest) (Digest, bool) {
	requestDigest, err := request.Digest()
	digest, ok := o.terminalObservationRequests[request.key()]
	if err != nil || !ok {
		return Digest{}, false
	}
	record, recordOK := o.records[request.key()]
	if !recordOK || record.RequestDigest != requestDigest || record.Result != digest {
		return Digest{}, false
	}
	_, ok = o.terminalObservations[digest]
	return digest, ok
}

func (o *ProtocolCrashOracle) terminalObservation(digest Digest) (WriterTerminalEvidence, bool) {
	if !o.validInstance() {
		return WriterTerminalEvidence{}, false
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	evidence, ok := o.terminalObservations[digest]
	return cloneWriterTerminalEvidence(evidence), ok
}

func (*ProtocolCrashOracle) IsTestOracleOnly() bool { return true }

func (o *ProtocolCrashOracle) IdentityDigest() Digest {
	if !o.validInstance() {
		return Digest{}
	}
	return o.identity
}

func (o *ProtocolCrashOracle) validInstance() bool { return o != nil && o.self == o }

func (o *ProtocolCrashOracle) bindSigner(role UnitRole, privateKey ed25519.PrivateKey, ledger, root Digest) error {
	if !o.validInstance() || len(privateKey) != ed25519.PrivateKeySize || ledger.IsZero() || root.IsZero() ||
		(role != WriterAuthorityRole && role != ObserverAuthorityRole) {
		return ErrRoleIsolation
	}
	public := privateKey.Public().(ed25519.PublicKey)
	domain := "writer-root/v2\x00"
	if role == ObserverAuthorityRole {
		domain = "observer-root/v2\x00"
	}
	if HashBytes(append([]byte(domain), public...)) != root {
		return ErrRoleIsolation
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.signerRole != "" && (o.signerRole != role || o.signerLedger != ledger || o.signerRoot != root || !bytes.Equal(o.signerPrivate, privateKey)) {
		return ErrRoleIsolation
	}
	o.signerRole, o.signerLedger, o.signerRoot = role, ledger, root
	o.signerPrivate = append(ed25519.PrivateKey(nil), privateKey...)
	return nil
}

// signLocked models the key-confined ledger signer. It is callable only while
// the original oracle instance holds its transaction lock.
func (o *ProtocolCrashOracle) signLocked(role UnitRole, root Digest, payload []byte) ([]byte, error) {
	if !o.validInstance() || o.signerRole != role || o.signerLedger.IsZero() || o.signerRoot != root || len(o.signerPrivate) != ed25519.PrivateKeySize || len(payload) == 0 {
		return nil, ErrRoleIsolation
	}
	return ed25519.Sign(o.signerPrivate, payload), nil
}

func (o *ProtocolCrashOracle) completeRecordLocked(request MutationRequest, record protocolEffectRecord) (protocolEffectRecord, error) {
	// A provider observation is completion authority only after this exact
	// operation crossed both durable pre-wire fences. This prevents an injected
	// or stale matching observation from completing a crash-after-authorization
	// or crash-after-preparation record. Marker continuation establishes its
	// separately fenced issued claim before reaching this helper.
	if !record.Prepared || !record.Issued {
		return record, ErrQuarantined
	}
	record.State = MutationCompleted
	record.CompletedAt = o.now()
	if o.signerRole == "" {
		return record, nil
	}
	requestDigest, err := request.Digest()
	if err != nil || record.Result.IsZero() {
		return record, ErrInvalid
	}
	o.completionSequence++
	witness := CompletionWitness{
		Version: ProtocolVersion, Role: o.signerRole, Effect: request.Effect, State: MutationCompleted,
		RequestSHA256: requestDigest, ResultSHA256: record.Result, TerminalSHA256: record.Terminal,
		LedgerSHA256: o.signerLedger, RootSHA256: o.signerRoot, OracleSHA256: o.identity,
		Sequence: o.completionSequence, CompletedAt: record.CompletedAt,
	}
	witness.Signature, err = o.signLocked(o.signerRole, o.signerRoot, witness.payload())
	if err != nil {
		return record, err
	}
	o.completionWitnesses[request.key()] = cloneCompletionWitness(witness)
	return record, nil
}

func operationGenerationKey(body OperationBody) string {
	return body.OperationID + "/" + fmt.Sprint(body.Generation)
}

// issueAuthorityChallenge generates and durably binds a fresh opaque challenge
// before returning it. Only the authority already bound to this oracle may
// issue a grant, and no wildcard body or call-kind binding exists.
func (o *ProtocolCrashOracle) issueAuthorityChallenge(
	ctx context.Context,
	request MutationRequest,
	call CallKind,
	role UnitRole,
	root Digest,
	random io.Reader,
) (AuthorityChallenge, error) {
	if !o.validInstance() {
		return AuthorityChallenge{}, ErrRoleIsolation
	}
	if err := ctx.Err(); err != nil {
		return AuthorityChallenge{}, err
	}
	if err := request.Validate(); err != nil || call.validate() != nil {
		return AuthorityChallenge{}, ErrInvalid
	}
	requestDigest, err := request.Digest()
	if err != nil {
		return AuthorityChallenge{}, err
	}
	challenge, err := issueAuthorityChallenge(role, root, random)
	if err != nil {
		return AuthorityChallenge{}, err
	}
	challengeDigest := challenge.Hash()
	if challengeDigest.IsZero() {
		return AuthorityChallenge{}, ErrInvalid
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.signerRole != role || o.signerRoot != root || o.signerLedger.IsZero() {
		return AuthorityChallenge{}, ErrRoleIsolation
	}
	if _, exists := o.issuedChallenges[challengeDigest]; exists {
		return AuthorityChallenge{}, ErrAuthReplay
	}
	o.issuedChallenges[challengeDigest] = issuedAuthorityChallenge{
		RequestSHA256: requestDigest,
		Call:          call,
		Role:          role,
		RootSHA256:    root,
	}
	return challenge, nil
}

func (o *ProtocolCrashOracle) burnAuthLocked(request MutationRequest, call CallKind, auth AuthEnvelope) error {
	if err := auth.Validate(o.now()); err != nil {
		return err
	}
	if err := request.Validate(); err != nil || call.validate() != nil {
		return ErrInvalid
	}
	// A signer-bound durable oracle is the common authorization boundary for
	// writer and observer endpoints and lifecycles. Enforce its exact role/root
	// centrally so a lower-level caller cannot bypass an outer endpoint check.
	if o.signerRole != "" && auth.Challenge.validateFor(o.signerRole, o.signerRoot) != nil {
		return ErrInvalid
	}
	jti, challenge := auth.JTIHash(), auth.ChallengeHash()
	issued, ok := o.issuedChallenges[challenge]
	if !ok {
		return ErrChallengeNotIssued
	}
	if issued.Consumed || o.usedJTIs[jti] || o.usedChallenges[challenge] {
		return ErrAuthReplay
	}
	requestDigest, err := request.Digest()
	if err != nil {
		return err
	}
	if issued.RequestSHA256 != requestDigest || issued.Call != call || issued.Role != o.signerRole || issued.RootSHA256 != o.signerRoot {
		return ErrChallengeBinding
	}
	// Authentication is consumed before every semantic lookup. Missing,
	// failed, or quarantined operations cannot roll it back.
	issued.Consumed = true
	o.issuedChallenges[challenge] = issued
	o.usedJTIs[jti] = true
	o.usedChallenges[challenge] = true
	return nil
}

func (o *ProtocolCrashOracle) claimLocked(request MutationRequest, requestDigest Digest) error {
	opKey := operationGenerationKey(request.Operation)
	if o.quarantined[opKey] {
		return ErrQuarantined
	}
	if prior, ok := o.operationClaims[opKey]; ok && prior != requestDigest {
		// A completed publication is immutable, portable evidence. A later
		// conflicting reuse is rejected, but cannot retroactively revoke the
		// already signed completion witness. Before completion, the same drift
		// makes the unresolved operation globally unusable.
		for _, record := range o.records {
			if record.RequestDigest == prior && record.State == MutationCompleted {
				return ErrQuarantined
			}
		}
		o.quarantined[opKey] = true
		for key, record := range o.records {
			if record.RequestDigest == prior {
				record.State = MutationQuarantined
				o.records[key] = record
			}
		}
		return ErrQuarantined
	}
	o.operationClaims[opKey] = requestDigest
	return nil
}

func (o *ProtocolCrashOracle) AuthorizeAndConsume(
	ctx context.Context,
	request MutationRequest,
	auth AuthEnvelope,
	fault FaultMode,
	terminal Digest,
	apply func() (Digest, error),
) (Digest, error) {
	return o.authorizeAndConsume(ctx, request, auth, fault, terminal, nil, nil, apply)
}

// authorizeAndConsumeChecked burns fresh authentication before evaluating a
// semantic precondition, but does not claim or create a mutation record when
// that precondition fails. It lets exported controllers fail closed without
// either making invalid authentication replayable or poisoning the later
// correctly ordered request.
func (o *ProtocolCrashOracle) authorizeAndConsumeChecked(
	ctx context.Context,
	request MutationRequest,
	auth AuthEnvelope,
	fault FaultMode,
	terminal Digest,
	check func() error,
	apply func() (Digest, error),
) (Digest, error) {
	return o.authorizeAndConsume(ctx, request, auth, fault, terminal, nil, check, apply)
}

// AuthorizePreparedAndConsume models a durable prepare transition before the
// external mutation. The prepare callback may persist only opaque
// reconciliation material; it performs no external side effect.
func (o *ProtocolCrashOracle) AuthorizePreparedAndConsume(
	ctx context.Context,
	request MutationRequest,
	auth AuthEnvelope,
	fault FaultMode,
	terminal Digest,
	prepare func() error,
	apply func() (Digest, error),
) (Digest, error) {
	if prepare == nil {
		return Digest{}, ErrInvalid
	}
	return o.authorizeAndConsume(ctx, request, auth, fault, terminal, prepare, nil, apply)
}

func (o *ProtocolCrashOracle) authorizePreparedAndConsumeChecked(
	ctx context.Context,
	request MutationRequest,
	auth AuthEnvelope,
	fault FaultMode,
	terminal Digest,
	check func() error,
	prepare func() error,
	apply func() (Digest, error),
) (Digest, error) {
	if prepare == nil {
		return Digest{}, ErrInvalid
	}
	return o.authorizeAndConsume(ctx, request, auth, fault, terminal, prepare, check, apply)
}

func (o *ProtocolCrashOracle) authorizeAndConsume(
	ctx context.Context,
	request MutationRequest,
	auth AuthEnvelope,
	fault FaultMode,
	terminal Digest,
	prepare func() error,
	check func() error,
	apply func() (Digest, error),
) (Digest, error) {
	if !o.validInstance() {
		return Digest{}, ErrRoleIsolation
	}
	if err := ctx.Err(); err != nil {
		return Digest{}, err
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if err := o.burnAuthLocked(request, MutationCall, auth); err != nil {
		return Digest{}, err
	}
	if check != nil {
		if err := check(); err != nil {
			return Digest{}, err
		}
	}
	if err := request.Validate(); err != nil {
		return Digest{}, err
	}
	requestDigest, _ := request.Digest()
	if err := o.claimLocked(request, requestDigest); err != nil {
		return Digest{}, err
	}
	if record, ok := o.records[request.key()]; ok {
		if record.RequestDigest != requestDigest || record.Effect != request.Effect || record.Terminal != terminal {
			if record.State == MutationCompleted {
				// The signed completed tuple is immutable. Reject an out-of-band
				// terminal substitution without revoking already published proof.
				return Digest{}, ErrQuarantined
			}
			o.quarantined[operationGenerationKey(request.Operation)] = true
			record.State = MutationQuarantined
			o.records[request.key()] = record
			return Digest{}, ErrQuarantined
		}
		switch record.State {
		case MutationCompleted:
			return record.Result, nil
		case MutationStarted, MutationAmbiguous:
			return Digest{}, ErrReconcileRequired
		default:
			return Digest{}, ErrQuarantined
		}
	}
	if !terminal.IsZero() {
		if prior, ok := o.terminalConsumption[terminal]; ok && prior != requestDigest {
			o.quarantined[operationGenerationKey(request.Operation)] = true
			return Digest{}, ErrQuarantined
		}
		o.terminalConsumption[terminal] = requestDigest
	}
	record := protocolEffectRecord{RequestDigest: requestDigest, Effect: request.Effect, Terminal: terminal, State: MutationStarted, IssueCount: 1}
	o.records[request.key()] = record
	if fault == FaultCrashAfterAuthorization {
		return Digest{}, ErrSimulatedCrash
	}
	if fault == FaultDefinitiveRejection {
		record.State = MutationQuarantined
		o.records[request.key()] = record
		o.quarantined[operationGenerationKey(request.Operation)] = true
		return Digest{}, ErrQuarantined
	}
	if prepare != nil {
		if err := prepare(); err != nil {
			o.markMarkerAbortPendingLocked(request, HashString("marker-abort-reason/v2\x00prepare-rejected"))
			record.State = MutationQuarantined
			o.records[request.key()] = record
			o.quarantined[operationGenerationKey(request.Operation)] = true
			return Digest{}, err
		}
	}
	// The durable prepared fence is committed before the separate issued fence.
	// A reconstruction may continue a marker wire operation only when both
	// fences exist and it atomically claims the one continuation below.
	record.Prepared = true
	o.records[request.key()] = record
	if fault == FaultCrashAfterPreparation {
		return Digest{}, ErrSimulatedCrash
	}
	record.Issued = true
	o.records[request.key()] = record
	if fault == FaultHTTP408BeforeEffect || fault == FaultEOFBeforeEffect || fault == Fault5xxBeforeEffect {
		record.State = MutationAmbiguous
		record.Ambiguity = fault.ambiguity()
		o.records[request.key()] = record
		return Digest{}, ErrReconcileRequired
	}
	if apply == nil {
		record.State = MutationQuarantined
		o.records[request.key()] = record
		o.quarantined[operationGenerationKey(request.Operation)] = true
		return Digest{}, ErrQuarantined
	}
	result, err := apply()
	if err != nil {
		var ambiguous *ProviderAmbiguityError
		if errors.As(err, &ambiguous) && ambiguous.valid() {
			record.State = MutationAmbiguous
			record.Ambiguity = string(ambiguous.Kind)
			o.records[request.key()] = record
			return Digest{}, ErrReconcileRequired
		}
		record.State = MutationQuarantined
		o.markMarkerAbortPendingLocked(request, HashString("marker-abort-reason/v2\x00definitive-apply-failure"))
		o.records[request.key()] = record
		o.quarantined[operationGenerationKey(request.Operation)] = true
		return Digest{}, err
	}
	if result.IsZero() {
		record.State = MutationQuarantined
		o.markMarkerAbortPendingLocked(request, HashString("marker-abort-reason/v2\x00zero-result"))
		o.records[request.key()] = record
		o.quarantined[operationGenerationKey(request.Operation)] = true
		return Digest{}, ErrQuarantined
	}
	record.Result = result
	if fault == FaultCrashAfterEffect || fault == FaultHTTP408AfterEffect || fault == FaultEOFAfterEffect || fault == Fault5xxAfterEffect {
		record.State = MutationAmbiguous
		record.Ambiguity = fault.ambiguity()
		o.records[request.key()] = record
		if fault == FaultCrashAfterEffect {
			return Digest{}, ErrSimulatedCrash
		}
		return Digest{}, ErrReconcileRequired
	}
	record, err = o.completeRecordLocked(request, record)
	if err != nil {
		record.State = MutationQuarantined
		o.markMarkerAbortPendingLocked(request, HashString("marker-abort-reason/v2\x00completion-failure"))
		o.records[request.key()] = record
		o.quarantined[operationGenerationKey(request.Operation)] = true
		return Digest{}, ErrQuarantined
	}
	o.records[request.key()] = record
	return result, nil
}

// ReconcileStatus is observation-only. It never invokes a mutation callback.
func (o *ProtocolCrashOracle) ReconcileStatus(
	ctx context.Context,
	request MutationRequest,
	auth AuthEnvelope,
	observe func() ([]Digest, error),
) (Digest, error) {
	return o.reconcileStatus(ctx, request, auth, nil, observe, nil)
}

func (o *ProtocolCrashOracle) reconcileStatusChecked(
	ctx context.Context,
	request MutationRequest,
	auth AuthEnvelope,
	check func() error,
	observe func() ([]Digest, error),
) (Digest, error) {
	return o.reconcileStatus(ctx, request, auth, check, observe, nil)
}

func (o *ProtocolCrashOracle) reconcilePreparedStatusChecked(
	ctx context.Context,
	request MutationRequest,
	auth AuthEnvelope,
	check func() error,
	observe func() ([]Digest, error),
	continueOnce func() (Digest, error),
) (Digest, error) {
	if request.Effect != EffectMarkerCAS || continueOnce == nil {
		return Digest{}, ErrInvalid
	}
	return o.reconcileStatus(ctx, request, auth, check, observe, continueOnce)
}

// rejectAfterAuth burns a valid one-time authentication envelope before an
// exported protocol wrapper returns a semantic rejection discovered before a
// durable request can be constructed. It never creates or mutates an effect
// record. Reusing the envelope therefore returns ErrAuthReplay even though the
// original call failed before provider mutation.
func (o *ProtocolCrashOracle) rejectAfterAuth(ctx context.Context, auth AuthEnvelope, semantic error) error {
	if semantic == nil {
		return nil
	}
	if !o.validInstance() {
		return ErrRoleIsolation
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// Structurally invalid input has no valid immutable request digest to which
	// an authority could have issued a challenge. It is rejected before the
	// durable authentication boundary rather than weakening issuance with a
	// wildcard binding.
	if err := auth.Validate(o.now()); err != nil {
		return err
	}
	return semantic
}

func (o *ProtocolCrashOracle) reconcileStatus(
	ctx context.Context,
	request MutationRequest,
	auth AuthEnvelope,
	check func() error,
	observe func() ([]Digest, error),
	continueOnce func() (Digest, error),
) (Digest, error) {
	if !o.validInstance() {
		return Digest{}, ErrRoleIsolation
	}
	if err := ctx.Err(); err != nil {
		return Digest{}, err
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if err := o.burnAuthLocked(request, StatusCall, auth); err != nil {
		return Digest{}, err
	}
	if check != nil {
		if err := check(); err != nil {
			return Digest{}, err
		}
	}
	if err := request.Validate(); err != nil {
		return Digest{}, err
	}
	requestDigest, _ := request.Digest()
	if err := o.claimLocked(request, requestDigest); err != nil {
		return Digest{}, err
	}
	record, ok := o.records[request.key()]
	if !ok {
		return Digest{}, ErrInvalid
	}
	if record.RequestDigest != requestDigest || record.Effect != request.Effect {
		o.quarantined[operationGenerationKey(request.Operation)] = true
		return Digest{}, ErrQuarantined
	}
	if record.State == MutationCompleted {
		return record.Result, nil
	}
	if record.State == MutationQuarantined {
		return Digest{}, ErrQuarantined
	}
	if observe == nil {
		return Digest{}, ErrInvalid
	}
	candidates, err := observe()
	if err != nil {
		if request.Effect == EffectMarkerCAS && continueOnce != nil {
			// The original wire outcome was already ambiguous. Any ambiguity or
			// validation failure during its one status reconstruction consumes the
			// recovery opportunity and requires cleanup; it is never retryable.
			record.State = MutationQuarantined
			record.Ambiguity = "status-observation-failed"
			o.markMarkerAbortPendingLocked(request, HashString("marker-abort-reason/v2\x00status-observation-failed"))
			o.records[request.key()] = record
			o.quarantined[operationGenerationKey(request.Operation)] = true
		}
		return Digest{}, err
	}
	switch len(candidates) {
	case 0:
		record.State = MutationQuarantined
		o.markMarkerAbortPendingLocked(request, HashString("marker-abort-reason/v2\x00status-not-observed"))
		o.records[request.key()] = record
		o.quarantined[operationGenerationKey(request.Operation)] = true
		if request.Effect == EffectMarkerCAS {
			return Digest{}, ErrCleanupRequired
		}
		return Digest{}, ErrNotObserved
	case 1:
		if request.Effect == EffectMarkerCAS && candidates[0] == markerContinuationCandidate {
			if continueOnce == nil || !record.Prepared || record.ContinuationClaimed || !record.Result.IsZero() {
				record.State = MutationQuarantined
				o.markMarkerAbortPendingLocked(request, HashString("marker-abort-reason/v2\x00invalid-continuation-fence"))
				o.records[request.key()] = record
				o.quarantined[operationGenerationKey(request.Operation)] = true
				return Digest{}, ErrCleanupRequired
			}
			// This write is the durable recovery claim. It is persisted before the
			// sole same-operation continuation reaches the protected adapter.
			if !record.Issued {
				record.Issued = true
			}
			record.ContinuationClaimed = true
			o.records[request.key()] = record
			result, continuationErr := continueOnce()
			if continuationErr != nil || result.IsZero() {
				record.State = MutationQuarantined
				record.Ambiguity = "continuation-failed"
				o.markMarkerAbortPendingLocked(request, HashString("marker-abort-reason/v2\x00continuation-failed"))
				o.records[request.key()] = record
				o.quarantined[operationGenerationKey(request.Operation)] = true
				return Digest{}, ErrCleanupRequired
			}
			record.Result = result
			record, err = o.completeRecordLocked(request, record)
			if err != nil {
				record.State = MutationQuarantined
				record.Ambiguity = "continuation-completion-failed"
				o.markMarkerAbortPendingLocked(request, HashString("marker-abort-reason/v2\x00continuation-completion-failed"))
				o.records[request.key()] = record
				o.quarantined[operationGenerationKey(request.Operation)] = true
				return Digest{}, ErrCleanupRequired
			}
			o.records[request.key()] = record
			return result, nil
		}
		if candidates[0].IsZero() {
			record.State = MutationQuarantined
			o.markMarkerAbortPendingLocked(request, HashString("marker-abort-reason/v2\x00status-zero-candidate"))
			o.records[request.key()] = record
			o.quarantined[operationGenerationKey(request.Operation)] = true
			if request.Effect == EffectMarkerCAS {
				return Digest{}, ErrCleanupRequired
			}
			return Digest{}, ErrNotObserved
		}
		if !record.Result.IsZero() && record.Result != candidates[0] {
			record.State = MutationQuarantined
			o.markMarkerAbortPendingLocked(request, HashString("marker-abort-reason/v2\x00status-result-mismatch"))
			o.records[request.key()] = record
			o.quarantined[operationGenerationKey(request.Operation)] = true
			return Digest{}, ErrQuarantined
		}
		record.Result = candidates[0]
		record, err = o.completeRecordLocked(request, record)
		if err != nil {
			record.State = MutationQuarantined
			o.markMarkerAbortPendingLocked(request, HashString("marker-abort-reason/v2\x00status-completion-failure"))
			o.records[request.key()] = record
			o.quarantined[operationGenerationKey(request.Operation)] = true
			return Digest{}, ErrQuarantined
		}
		o.records[request.key()] = record
		return record.Result, nil
	default:
		record.State = MutationQuarantined
		o.markMarkerAbortPendingLocked(request, HashString("marker-abort-reason/v2\x00status-multiple-observed"))
		o.records[request.key()] = record
		o.quarantined[operationGenerationKey(request.Operation)] = true
		if request.Effect == EffectMarkerCAS {
			return Digest{}, ErrCleanupRequired
		}
		return Digest{}, ErrMultipleObserved
	}
}

// recordForRequestLocked returns a record only when the complete immutable
// request, not merely its operation/effect lookup key, matches the record that
// was durably authorized. Callers must hold o.mu.
func (o *ProtocolCrashOracle) recordForRequestLocked(request MutationRequest) (protocolEffectRecord, bool) {
	if !o.validInstance() || o.quarantined[operationGenerationKey(request.Operation)] {
		return protocolEffectRecord{}, false
	}
	if err := request.Validate(); err != nil {
		return protocolEffectRecord{}, false
	}
	requestDigest, err := request.Digest()
	if err != nil {
		return protocolEffectRecord{}, false
	}
	record, ok := o.records[request.key()]
	return record, ok && record.RequestDigest == requestDigest && record.Effect == request.Effect
}

func (o *ProtocolCrashOracle) Completed(request MutationRequest) bool {
	if !o.validInstance() {
		return false
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	record, ok := o.recordForRequestLocked(request)
	return ok && record.State == MutationCompleted
}

func (o *ProtocolCrashOracle) IssueCount(request MutationRequest) int {
	if !o.validInstance() {
		return 0
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	record, ok := o.recordForRequestLocked(request)
	if !ok {
		return 0
	}
	return record.IssueCount
}

func (o *ProtocolCrashOracle) CompletionTime(request MutationRequest) (time.Time, bool) {
	if !o.validInstance() {
		return time.Time{}, false
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	record, ok := o.recordForRequestLocked(request)
	if !ok || record.State != MutationCompleted || record.CompletedAt.IsZero() {
		return time.Time{}, false
	}
	return record.CompletedAt, true
}

func (o *ProtocolCrashOracle) BindProjection(body OperationBody, projection Digest) error {
	if !o.validInstance() {
		return ErrRoleIsolation
	}
	if err := body.Validate(); err != nil || projection.IsZero() {
		return ErrInvalid
	}
	bodyDigest, _ := body.Digest()
	o.mu.Lock()
	defer o.mu.Unlock()
	opKey := operationGenerationKey(body)
	if o.quarantined[opKey] {
		return ErrQuarantined
	}
	if prior, ok := o.baseOperationClaims[opKey]; ok && prior != bodyDigest {
		o.quarantined[opKey] = true
		return ErrQuarantined
	}
	o.baseOperationClaims[opKey] = bodyDigest
	if prior, ok := o.projectionBindings[bodyDigest]; ok && prior != projection {
		o.quarantined[opKey] = true
		return ErrQuarantined
	}
	o.projectionBindings[bodyDigest] = projection
	return nil
}

func (o *ProtocolCrashOracle) projectionFor(body OperationBody) (Digest, bool) {
	bodyDigest, err := body.Digest()
	if err != nil {
		return Digest{}, false
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if !o.validInstance() || o.quarantined[operationGenerationKey(body)] {
		return Digest{}, false
	}
	projection, ok := o.projectionBindings[bodyDigest]
	return projection, ok
}

func (o *ProtocolCrashOracle) projectionForLocked(body OperationBody) (Digest, bool) {
	bodyDigest, err := body.Digest()
	if err != nil || !o.validInstance() || o.quarantined[operationGenerationKey(body)] {
		return Digest{}, false
	}
	projection, ok := o.projectionBindings[bodyDigest]
	return projection, ok
}

// completionWitness returns only the immutable proof produced by the bound
// signer in the same locked transition that completed the exact request.
func (o *ProtocolCrashOracle) completionWitness(request MutationRequest) (CompletionWitness, bool) {
	if !o.validInstance() {
		return CompletionWitness{}, false
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	record, ok := o.recordForRequestLocked(request)
	witness, witnessed := o.completionWitnesses[request.key()]
	requestDigest, digestErr := request.Digest()
	if !ok || !witnessed || digestErr != nil || record.State != MutationCompleted || record.Result.IsZero() ||
		witness.RequestSHA256 != requestDigest || witness.ResultSHA256 != record.Result || witness.TerminalSHA256 != record.Terminal {
		return CompletionWitness{}, false
	}
	return cloneCompletionWitness(witness), true
}

func (o *ProtocolCrashOracle) result(request MutationRequest) (Digest, bool) {
	if !o.validInstance() {
		return Digest{}, false
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	record, ok := o.recordForRequestLocked(request)
	if !ok || record.State != MutationCompleted {
		return Digest{}, false
	}
	return record.Result, true
}

func (o *ProtocolCrashOracle) CapabilityRevoked(capability Digest) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.revokedCapabilities[capability]
}

func (o *ProtocolCrashOracle) storeSealedMarkerTokenLocked(body Digest, token sealedMarkerToken) error {
	if body.IsZero() || token.version != ProtocolVersion || token.operationSHA256 != body || token.markerSHA256.IsZero() || token.sealerSHA256.IsZero() ||
		token.predecessor.validate() != nil || len(token.nonce) == 0 || len(token.ciphertext) == 0 {
		return ErrInvalid
	}
	if _, err := canonicalTime(token.brokerActionAtUTC); err != nil {
		return ErrInvalid
	}
	if _, exists := o.sealedMarkerTokens[body]; exists || !o.erasedMarkerTokens[body].IsZero() {
		return ErrQuarantined
	}
	o.sealedMarkerTokens[body] = token.clone()
	return nil
}

func (o *ProtocolCrashOracle) sealedMarkerTokenLocked(body Digest) (sealedMarkerToken, bool) {
	token, ok := o.sealedMarkerTokens[body]
	return token.clone(), ok
}

// markMarkerAbortPendingLocked records only a durable cleanup requirement. It
// never invokes the sealer or any other external side effect. Cleanup is a
// separate issue-once operation with a fresh authentication envelope.
func (o *ProtocolCrashOracle) markMarkerAbortPendingLocked(request MutationRequest, reason Digest) {
	if request.Effect != EffectMarkerCAS || reason.IsZero() {
		return
	}
	body, err := request.Operation.Digest()
	if err != nil {
		return
	}
	token, ok := o.sealedMarkerTokens[body]
	if !ok {
		return
	}
	requestDigest, err := request.Digest()
	if err != nil {
		return
	}
	pending := pendingMarkerAbort{
		MarkerRequestKey:    request.key(),
		MarkerRequestSHA256: requestDigest,
		OperationBodySHA256: body,
		MarkerSHA256:        token.markerSHA256,
		SourceTargetSHA256:  token.targetSHA256,
		CapabilitySHA256:    token.capabilitySHA256,
		SealerSHA256:        token.sealerSHA256,
		ReasonSHA256:        reason,
	}
	if prior, exists := o.pendingMarkerAborts[body]; exists && prior != pending {
		o.quarantined[operationGenerationKey(request.Operation)] = true
		return
	}
	o.pendingMarkerAborts[body] = pending
}

// quarantineMarkerForCleanup converts a marker operation that can no longer
// pass its trusted chronology checks into cleanup-only state. It publishes no
// writer receipt and preserves the sealed token solely for the separate,
// freshly authenticated abort operation.
func (o *ProtocolCrashOracle) quarantineMarkerForCleanup(request MutationRequest, reason Digest) {
	if !o.validInstance() || request.Effect != EffectMarkerCAS || reason.IsZero() {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	record, ok := o.recordForRequestLocked(request)
	if !ok || record.State == MutationCompleted {
		return
	}
	o.markMarkerAbortPendingLocked(request, reason)
	record.State = MutationQuarantined
	o.records[request.key()] = record
	o.quarantined[operationGenerationKey(request.Operation)] = true
}

func cloneMarkerAbortWitness(witness MarkerAbortWitness) MarkerAbortWitness {
	witness.Signature = append([]byte(nil), witness.Signature...)
	return witness
}

func (o *ProtocolCrashOracle) pendingMarkerAbort(body Digest) (pendingMarkerAbort, bool) {
	if !o.validInstance() || body.IsZero() {
		return pendingMarkerAbort{}, false
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	pending, ok := o.pendingMarkerAborts[body]
	if ok {
		return pending, true
	}
	witness, ok := o.markerAbortWitnesses[body]
	if !ok {
		return pendingMarkerAbort{}, false
	}
	return pendingMarkerAbort{
		MarkerRequestSHA256: witness.MarkerRequestSHA256,
		OperationBodySHA256: witness.OperationBodySHA256,
		MarkerSHA256:        witness.MarkerSHA256,
		SourceTargetSHA256:  witness.SourceTargetSHA256,
		CapabilitySHA256:    witness.CapabilitySHA256,
		SealerSHA256:        witness.SealerSHA256,
		ReasonSHA256:        witness.ReasonSHA256,
	}, true
}

// registerPreparedMarkerAbortLocked signs and persists the exact abort intent
// before the wrapping-key revocation. The signature is not proof of execution;
// only the separate durable completion witness makes it consumable evidence.
func (o *ProtocolCrashOracle) registerPreparedMarkerAbortLocked(request MutationRequest, pending pendingMarkerAbort) (MarkerAbortWitness, error) {
	if o.signerRole != WriterAuthorityRole || pending.OperationBodySHA256.IsZero() || pending.MarkerSHA256.IsZero() || pending.SourceTargetSHA256.IsZero() || pending.CapabilitySHA256.IsZero() || pending.SealerSHA256.IsZero() || pending.ReasonSHA256.IsZero() {
		return MarkerAbortWitness{}, ErrCleanupRequired
	}
	requestDigest, err := request.Digest()
	if err != nil {
		return MarkerAbortWitness{}, ErrCleanupRequired
	}
	if prior, ok := o.preparedMarkerAborts[request.key()]; ok {
		if prior.AbortRequestSHA256 != requestDigest || prior.MarkerRequestSHA256 != pending.MarkerRequestSHA256 ||
			prior.OperationBodySHA256 != pending.OperationBodySHA256 || prior.MarkerSHA256 != pending.MarkerSHA256 ||
			prior.SourceTargetSHA256 != pending.SourceTargetSHA256 || prior.CapabilitySHA256 != pending.CapabilitySHA256 ||
			prior.SealerSHA256 != pending.SealerSHA256 || prior.ReasonSHA256 != pending.ReasonSHA256 {
			return MarkerAbortWitness{}, ErrQuarantined
		}
		return cloneMarkerAbortWitness(prior), nil
	}
	witness := MarkerAbortWitness{
		Version: ProtocolVersion, AbortRequestSHA256: requestDigest, MarkerRequestSHA256: pending.MarkerRequestSHA256,
		OperationBodySHA256: pending.OperationBodySHA256, MarkerSHA256: pending.MarkerSHA256,
		SourceTargetSHA256: pending.SourceTargetSHA256, CapabilitySHA256: pending.CapabilitySHA256,
		SealerSHA256: pending.SealerSHA256, ReasonSHA256: pending.ReasonSHA256,
		LedgerSHA256: o.signerLedger, RootSHA256: o.signerRoot, OracleSHA256: o.identity, AuthorizedAt: o.now(),
	}
	witness.Signature, err = o.signLocked(WriterAuthorityRole, o.signerRoot, witness.payload())
	if err != nil {
		return MarkerAbortWitness{}, ErrCleanupRequired
	}
	o.preparedMarkerAborts[request.key()] = cloneMarkerAbortWitness(witness)
	return witness, nil
}

func (o *ProtocolCrashOracle) registerLifecycleSealerRevocationIntentLocked(request MutationRequest, sealer, provider, providerConfig Digest) (lifecycleSealerRevocationIntent, error) {
	requestDigest, err := request.Digest()
	if err != nil || request.Effect != EffectWrappingKeyRevoke || sealer.IsZero() || provider.IsZero() || providerConfig.IsZero() ||
		o.signerRole != WriterAuthorityRole || len(o.signerPrivate) != ed25519.PrivateKeySize {
		return lifecycleSealerRevocationIntent{}, ErrInvalid
	}
	if prior, ok := o.preparedLifecycleSealerRevocations[request.key()]; ok {
		if prior.RequestSHA256 != requestDigest || prior.SealerSHA256 != sealer || prior.ProviderSHA256 != provider || prior.ProviderConfigSHA256 != providerConfig {
			return lifecycleSealerRevocationIntent{}, ErrQuarantined
		}
		return cloneLifecycleSealerRevocationIntent(prior), nil
	}
	intent := lifecycleSealerRevocationIntent{
		Version: ProtocolVersion, RequestSHA256: requestDigest, SealerSHA256: sealer,
		ProviderSHA256: provider, ProviderConfigSHA256: providerConfig,
		LedgerSHA256: o.signerLedger, RootSHA256: o.signerRoot, OracleSHA256: o.identity, AuthorizedAt: o.now(),
	}
	intent.Signature, err = o.signLocked(WriterAuthorityRole, o.signerRoot, intent.payload())
	if err != nil {
		return lifecycleSealerRevocationIntent{}, err
	}
	o.preparedLifecycleSealerRevocations[request.key()] = cloneLifecycleSealerRevocationIntent(intent)
	return intent, nil
}

// markerAbortTerminalResult binds the prepared signed intent to the complete
// status-only revocation result. It does not erase reconciliation material.
func markerAbortTerminalResult(witness MarkerAbortWitness, status MarkerSealerRevocationStatus) (Digest, error) {
	statusDigest, err := status.Digest()
	if err != nil || witness.Digest().IsZero() || status.BindingSHA256 != witness.SealerSHA256 || status.ObservedAt.Before(witness.AuthorizedAt) {
		return Digest{}, ErrCleanupRequired
	}
	return HashString("marker-abort-terminal-result/v2\x00" + witness.Digest().String() + "\x00" + statusDigest.String()), nil
}

// finalizeMarkerAbortErasureLocked erases local reconciliation material only
// after the exact revocation status has produced a signed durable completion
// witness. It never calls Revoke and is safe to repeat with the identical
// completed tuple.
func (o *ProtocolCrashOracle) finalizeMarkerAbortErasureLocked(request MutationRequest, pending pendingMarkerAbort, status MarkerSealerRevocationStatus) (Digest, error) {
	witness, ok := o.preparedMarkerAborts[request.key()]
	if !ok || witness.MarkerRequestSHA256 != pending.MarkerRequestSHA256 || witness.OperationBodySHA256 != pending.OperationBodySHA256 ||
		witness.MarkerSHA256 != pending.MarkerSHA256 || witness.SourceTargetSHA256 != pending.SourceTargetSHA256 || witness.CapabilitySHA256 != pending.CapabilitySHA256 ||
		witness.SealerSHA256 != pending.SealerSHA256 || witness.ReasonSHA256 != pending.ReasonSHA256 {
		return Digest{}, ErrCleanupRequired
	}
	result, resultErr := markerAbortTerminalResult(witness, status)
	if resultErr != nil {
		return Digest{}, resultErr
	}
	if len(o.signerPrivate) != ed25519.PrivateKeySize {
		return Digest{}, ErrCleanupRequired
	}
	record, recordOK := o.recordForRequestLocked(request)
	completion, completionOK := o.completionWitnesses[request.key()]
	if !recordOK || record.State != MutationCompleted || record.Result != result || !completionOK ||
		verifyCompletionWitness(o.signerPrivate.Public().(ed25519.PublicKey), WriterAuthorityRole,
			o.signerLedger, o.signerRoot, o.identity, request, result,
			markerAbortTerminalBinding(pending.OperationBodySHA256, pending.SealerSHA256), completion) != nil ||
		completion.CompletedAt.Before(status.ObservedAt) {
		return Digest{}, ErrCleanupRequired
	}
	if prior := o.erasedMarkerTokens[pending.OperationBodySHA256]; !prior.IsZero() {
		stored, ok := o.markerAbortWitnesses[pending.OperationBodySHA256]
		if prior == result && ok && stored.Digest() == witness.Digest() {
			return result, nil
		}
		return Digest{}, ErrCleanupRequired
	}
	token, exists := o.sealedMarkerTokens[pending.OperationBodySHA256]
	if !exists || token.markerSHA256 != pending.MarkerSHA256 || token.targetSHA256 != pending.SourceTargetSHA256 || token.capabilitySHA256 != pending.CapabilitySHA256 ||
		token.sealerSHA256 != pending.SealerSHA256 || token.operationSHA256 != pending.OperationBodySHA256 {
		return Digest{}, ErrCleanupRequired
	}
	token.erase()
	delete(o.sealedMarkerTokens, pending.OperationBodySHA256)
	if pending.MarkerRequestKey != "" {
		delete(o.preparedWriterReceipts, pending.MarkerRequestKey)
	}
	delete(o.pendingMarkerAborts, pending.OperationBodySHA256)
	o.erasedMarkerTokens[pending.OperationBodySHA256] = result
	o.markerAbortWitnesses[pending.OperationBodySHA256] = cloneMarkerAbortWitness(witness)
	return result, nil
}

func (o *ProtocolCrashOracle) markerAbortWitness(body Digest) (MarkerAbortWitness, bool) {
	if !o.validInstance() || body.IsZero() {
		return MarkerAbortWitness{}, false
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	witness, ok := o.markerAbortWitnesses[body]
	if !ok {
		return MarkerAbortWitness{}, false
	}
	return cloneMarkerAbortWitness(witness), true
}

func (o *ProtocolCrashOracle) eraseSealedMarkerTokenLocked(body, terminal Digest) error {
	if body.IsZero() || terminal.IsZero() {
		return ErrInvalid
	}
	token, ok := o.sealedMarkerTokens[body]
	if !ok {
		if o.erasedMarkerTokens[body] == terminal {
			return nil
		}
		return ErrInvalid
	}
	token.erase()
	delete(o.sealedMarkerTokens, body)
	o.erasedMarkerTokens[body] = terminal
	return nil
}

func (o *ProtocolCrashOracle) sealedMarkerTokenPresent(body Digest) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	_, ok := o.sealedMarkerTokens[body]
	return ok
}

func (o *ProtocolCrashOracle) markerErasedForTerminalLocked(body, terminal Digest) bool {
	return !body.IsZero() && !terminal.IsZero() && o.erasedMarkerTokens[body] == terminal
}

// registerTerminalAndEraseLocked models one transaction: the exact terminal
// binding cannot become visible unless the opaque reconciliation token is
// cryptographically erased and the erasure is durably bound to that receipt.
func (o *ProtocolCrashOracle) registerTerminalAndEraseLocked(receipt, operationBody, evidence Digest) error {
	if receipt.IsZero() || operationBody.IsZero() || evidence.IsZero() {
		return ErrInvalid
	}
	binding := terminalBinding{OperationBody: operationBody, Evidence: evidence}
	if prior, ok := o.publishedTerminals[receipt]; ok && prior != binding {
		return ErrQuarantined
	}
	if prior := o.erasedMarkerTokens[operationBody]; !prior.IsZero() && prior != receipt {
		return ErrQuarantined
	}
	if _, ok := o.sealedMarkerTokens[operationBody]; !ok && o.erasedMarkerTokens[operationBody] != receipt {
		return ErrInvalid
	}
	if err := o.eraseSealedMarkerTokenLocked(operationBody, receipt); err != nil {
		return err
	}
	o.publishedTerminals[receipt] = binding
	return nil
}

func (o *ProtocolCrashOracle) terminalRegisteredLocked(receipt, operationBody, evidence Digest) bool {
	binding, ok := o.publishedTerminals[receipt]
	return ok && binding == (terminalBinding{OperationBody: operationBody, Evidence: evidence}) &&
		o.markerErasedForTerminalLocked(operationBody, receipt)
}

func (o *ProtocolCrashOracle) terminalPublished(receipt, operationBody, evidence Digest, publishRequest MutationRequest) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	binding, ok := o.publishedTerminals[receipt]
	record, completed := o.recordForRequestLocked(publishRequest)
	return ok && binding == (terminalBinding{OperationBody: operationBody, Evidence: evidence}) &&
		o.markerErasedForTerminalLocked(operationBody, receipt) &&
		completed && record.State == MutationCompleted && record.Result == receipt
}

// registerAdmissionLocked is called only from the durable admission-publish
// effect callback while the crash oracle lock is held.
func (o *ProtocolCrashOracle) registerAdmissionLocked(digest Digest, binding recoveryAdmissionBinding) error {
	if digest.IsZero() || binding.OperationBody.IsZero() || binding.TerminalBinding.IsZero() ||
		binding.ForkRequest.IsZero() || binding.ForkResult.IsZero() || binding.ForkProof.IsZero() || binding.RecoveryTarget.IsZero() || binding.ForkProvider.IsZero() || binding.Payload.IsZero() {
		return ErrInvalid
	}
	if prior, ok := o.publishedAdmissions[digest]; ok && prior != binding {
		return ErrQuarantined
	}
	o.publishedAdmissions[digest] = binding
	return nil
}

func (o *ProtocolCrashOracle) admissionPublished(digest Digest, binding recoveryAdmissionBinding, request MutationRequest) bool {
	if !o.validInstance() {
		return false
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	prior, ok := o.publishedAdmissions[digest]
	record, completed := o.recordForRequestLocked(request)
	return ok && prior == binding && completed && record.State == MutationCompleted && record.Result == digest
}

func cloneRecoveryAdmissionAuthorization(authorization RecoveryAdmissionAuthorization) RecoveryAdmissionAuthorization {
	authorization.writerSignature = append([]byte(nil), authorization.writerSignature...)
	authorization.writerCompletionWitness = cloneCompletionWitness(authorization.writerCompletionWitness)
	authorization.forkCompletionWitness = cloneCompletionWitness(authorization.forkCompletionWitness)
	return authorization
}

// registerRecoveryAuthorizationLocked persists the exact writer-signed
// handoff. It is called only from the writer-ledger issue-once callback.
func (o *ProtocolCrashOracle) registerRecoveryAuthorizationLocked(request MutationRequest, authorization RecoveryAdmissionAuthorization) (Digest, error) {
	if err := request.Validate(); err != nil {
		return Digest{}, err
	}
	digest := authorization.Digest()
	if digest.IsZero() || len(authorization.writerSignature) != ed25519.SignatureSize {
		return Digest{}, ErrInvalid
	}
	if priorDigest, ok := o.authorizationRequests[request.key()]; ok && priorDigest != digest {
		o.quarantined[operationGenerationKey(request.Operation)] = true
		return Digest{}, ErrQuarantined
	}
	if prior, ok := o.recoveryAuthorizations[digest]; ok &&
		(!bytes.Equal(prior.signingPayload(), authorization.signingPayload()) || !bytes.Equal(prior.writerSignature, authorization.writerSignature)) {
		o.quarantined[operationGenerationKey(request.Operation)] = true
		return Digest{}, ErrQuarantined
	}
	if err := o.registerAdmissionLocked(digest, authorization.durableBinding()); err != nil {
		return Digest{}, err
	}
	o.authorizationRequests[request.key()] = digest
	o.recoveryAuthorizations[digest] = cloneRecoveryAdmissionAuthorization(authorization)
	return digest, nil
}

func (o *ProtocolCrashOracle) recoveryAuthorizationForRequestLocked(request MutationRequest) (Digest, bool) {
	digest, ok := o.authorizationRequests[request.key()]
	if !ok {
		return Digest{}, false
	}
	_, ok = o.recoveryAuthorizations[digest]
	return digest, ok
}

func (o *ProtocolCrashOracle) recoveryAuthorization(digest Digest) (RecoveryAdmissionAuthorization, bool) {
	if !o.validInstance() {
		return RecoveryAdmissionAuthorization{}, false
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	authorization, ok := o.recoveryAuthorizations[digest]
	return cloneRecoveryAdmissionAuthorization(authorization), ok
}

func (o *ProtocolCrashOracle) registerRecoveryAdmissionLocked(request MutationRequest, admission RecoveryAdmission) (Digest, error) {
	if err := request.Validate(); err != nil {
		return Digest{}, err
	}
	digest := admission.Digest()
	if digest.IsZero() {
		return Digest{}, ErrInvalid
	}
	if priorDigest, ok := o.recoveryRequests[request.key()]; ok && priorDigest != digest {
		o.quarantined[operationGenerationKey(request.Operation)] = true
		return Digest{}, ErrQuarantined
	}
	if prior, ok := o.recoveryAdmissions[digest]; ok &&
		(!bytes.Equal(prior.payload(), admission.payload()) || !bytes.Equal(prior.signature, admission.signature)) {
		o.quarantined[operationGenerationKey(request.Operation)] = true
		return Digest{}, ErrQuarantined
	}
	if err := o.registerAdmissionLocked(digest, admission.durableBinding()); err != nil {
		return Digest{}, err
	}
	o.recoveryRequests[request.key()] = digest
	o.recoveryAdmissions[digest] = admission.clone()
	return digest, nil
}

func (o *ProtocolCrashOracle) recoveryAdmissionForRequestLocked(request MutationRequest) (Digest, bool) {
	digest, ok := o.recoveryRequests[request.key()]
	if !ok {
		return Digest{}, false
	}
	_, ok = o.recoveryAdmissions[digest]
	return digest, ok
}

func (o *ProtocolCrashOracle) recoveryAdmission(digest Digest) (RecoveryAdmission, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	admission, ok := o.recoveryAdmissions[digest]
	return admission.clone(), ok
}

func cloneObserverReceipt(receipt ObserverReceipt) ObserverReceipt {
	receipt.Signature = append([]byte(nil), receipt.Signature...)
	receipt.CompletionWitness = cloneCompletionWitness(receipt.CompletionWitness)
	return receipt
}

// registerObserverReceiptLocked persists the exact signed receipt before an
// ambiguous response can be injected. A request may never be rebound to a
// different receipt, including across reconstructed controllers.
func (o *ProtocolCrashOracle) registerObserverReceiptLocked(request MutationRequest, receipt ObserverReceipt) (Digest, error) {
	if err := request.Validate(); err != nil {
		return Digest{}, err
	}
	digest := observerReceiptDigest(receipt)
	if digest.IsZero() {
		return Digest{}, ErrInvalid
	}
	if priorDigest, ok := o.observerRequests[request.key()]; ok && priorDigest != digest {
		o.quarantined[operationGenerationKey(request.Operation)] = true
		return Digest{}, ErrQuarantined
	}
	if prior, ok := o.observerReceipts[digest]; ok &&
		(!bytes.Equal(prior.payload(), receipt.payload()) || !bytes.Equal(prior.Signature, receipt.Signature)) {
		o.quarantined[operationGenerationKey(request.Operation)] = true
		return Digest{}, ErrQuarantined
	}
	o.observerRequests[request.key()] = digest
	o.observerReceipts[digest] = cloneObserverReceipt(receipt)
	return digest, nil
}

// observerReceiptForRequestLocked is used only by ReconcileStatus while the
// oracle lock is already held. It never creates or signs evidence.
func (o *ProtocolCrashOracle) observerReceiptForRequestLocked(request MutationRequest) (Digest, bool) {
	digest, ok := o.observerRequests[request.key()]
	if !ok {
		return Digest{}, false
	}
	_, ok = o.observerReceipts[digest]
	return digest, ok
}

func (o *ProtocolCrashOracle) observerReceipt(digest Digest) (ObserverReceipt, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	receipt, ok := o.observerReceipts[digest]
	return cloneObserverReceipt(receipt), ok
}
