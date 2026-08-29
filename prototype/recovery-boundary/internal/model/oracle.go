package model

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"
)

// AdapterCapabilities are mandatory for any future production constructor.
// CrashOracle intentionally advertises none of them.
type AdapterCapabilities struct {
	Transactional      bool
	Durable            bool
	Fenced             bool
	AppendOnlyAudit    bool
	EncryptedAtRest    bool
	BackupRecovery     bool
	KeyConfined        bool
	NoAutomaticRetries bool
}

func (c AdapterCapabilities) productionReady() bool {
	return c.Transactional && c.Durable && c.Fenced && c.AppendOnlyAudit &&
		c.EncryptedAtRest && c.BackupRecovery && c.KeyConfined && c.NoAutomaticRetries
}

type MutationState string

const (
	MutationStarted     MutationState = "started"
	MutationAmbiguous   MutationState = "ambiguous"
	MutationCompleted   MutationState = "completed"
	MutationQuarantined MutationState = "quarantined"
)

// MutationRecord is persisted before the effect. It contains hashes and
// semantic labels only; authentication and protected identifiers are absent.
type MutationRecord struct {
	Key                 string
	BodyDigest          Digest
	Effect              Effect
	State               MutationState
	Receipt             EffectReceipt
	Prepared            bool
	Issued              bool
	ContinuationClaimed bool
	Ambiguity           string
	QuarantineCause     string
}

type AuditRecord struct {
	Sequence   uint64
	Event      string
	RecordKey  string
	BodyDigest Digest
	State      MutationState
}

type OperationClaim struct {
	RequestDigest Digest
	Effect        Effect
}

// IssuedEngineChallenge is a hash-only authorization grant for the public
// crash-model Engine. The bearer challenge itself is never persisted. Each
// grant is bound before return to one exact request, call kind, authority role,
// and authority root, and can be consumed only once.
type IssuedEngineChallenge struct {
	RequestDigest Digest
	Call          CallKind
	Role          UnitRole
	RootDigest    Digest
	Consumed      bool
}

type LedgerSnapshot struct {
	Records          map[string]MutationRecord
	OperationClaims  map[string]OperationClaim
	IssuedChallenges map[Digest]IssuedEngineChallenge
	UsedJTIs         map[Digest]bool
	UsedChallenges   map[Digest]bool
	QuarantinedOps   map[string]bool
	Audit            []AuditRecord
	NextAuditNumber  uint64
}

func newLedgerSnapshot() LedgerSnapshot {
	return LedgerSnapshot{
		Records:          map[string]MutationRecord{},
		OperationClaims:  map[string]OperationClaim{},
		IssuedChallenges: map[Digest]IssuedEngineChallenge{},
		UsedJTIs:         map[Digest]bool{},
		UsedChallenges:   map[Digest]bool{},
		QuarantinedOps:   map[string]bool{},
	}
}

func (s LedgerSnapshot) clone() LedgerSnapshot {
	next := newLedgerSnapshot()
	for key, value := range s.Records {
		next.Records[key] = value
	}
	for key, value := range s.OperationClaims {
		next.OperationClaims[key] = value
	}
	for key, value := range s.IssuedChallenges {
		next.IssuedChallenges[key] = value
	}
	for key, value := range s.UsedJTIs {
		next.UsedJTIs[key] = value
	}
	for key, value := range s.UsedChallenges {
		next.UsedChallenges[key] = value
	}
	for key, value := range s.QuarantinedOps {
		next.QuarantinedOps[key] = value
	}
	next.Audit = append([]AuditRecord(nil), s.Audit...)
	next.NextAuditNumber = s.NextAuditNumber
	return next
}

func (s *LedgerSnapshot) appendAudit(event string, record MutationRecord) {
	s.NextAuditNumber++
	s.Audit = append(s.Audit, AuditRecord{
		Sequence: s.NextAuditNumber, Event: event, RecordKey: record.Key,
		BodyDigest: record.BodyDigest, State: record.State,
	})
}

// LedgerAdapter must make the callback atomic. A production adapter must also
// provide durable fencing, append-only auditing, encryption, recovery, and key
// confinement represented by Capabilities.
type LedgerAdapter interface {
	Capabilities() AdapterCapabilities
	Transact(context.Context, func(*LedgerSnapshot) error) error
}

type EffectExecutor interface {
	Capabilities() AdapterCapabilities
	ApplyOnce(context.Context, string, Effect, Digest, bool) error
	Observe(context.Context, string, Effect, Digest) (int, error)
	ProtectedMarkerPredecessorUnchanged(context.Context, string, Digest) (bool, error)
}

// CrashOracle is an in-process, fault-injecting test oracle. It is not durable
// across process loss and can never satisfy NewProductionEngine.
type CrashOracle struct {
	mu         sync.Mutex
	ledger     LedgerSnapshot
	effects    map[string]int
	applyCalls map[string]int
	// markerPredecessorUnchanged is protected fault-oracle state, never a
	// caller-selected request field. Absence models the original predecessor.
	markerPredecessorUnchanged map[string]bool
}

func NewCrashOracle() *CrashOracle {
	return &CrashOracle{
		ledger: newLedgerSnapshot(), effects: map[string]int{}, applyCalls: map[string]int{}, markerPredecessorUnchanged: map[string]bool{},
	}
}

func (*CrashOracle) Capabilities() AdapterCapabilities { return AdapterCapabilities{} }

func (o *CrashOracle) Transact(ctx context.Context, fn func(*LedgerSnapshot) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	next := o.ledger.clone()
	if err := fn(&next); err != nil {
		return err
	}
	o.ledger = next
	return nil
}

func (o *CrashOracle) ApplyOnce(ctx context.Context, key string, effect Effect, body Digest, apply bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	o.applyCalls[key]++
	if apply {
		o.effects[key]++
	}
	return nil
}

func (o *CrashOracle) Observe(ctx context.Context, key string, effect Effect, body Digest) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.effects[key], nil
}

func (o *CrashOracle) ProtectedMarkerPredecessorUnchanged(ctx context.Context, key string, body Digest) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if key == "" || body.IsZero() {
		return false, ErrInvalid
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	unchanged, set := o.markerPredecessorUnchanged[key]
	if !set {
		return true, nil
	}
	return unchanged, nil
}

func (o *CrashOracle) ForceMarkerPredecessorUnchanged(key string, unchanged bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.markerPredecessorUnchanged[key] = unchanged
}

func (o *CrashOracle) ApplyCalls(key string) int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.applyCalls[key]
}

func (o *CrashOracle) EffectCount(key string) int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.effects[key]
}

func (o *CrashOracle) ForceEffectCount(key string, count int) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.effects[key] = count
}

func (o *CrashOracle) Snapshot() LedgerSnapshot {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.ledger.clone()
}

type FaultMode string

const (
	FaultNone                    FaultMode = "none"
	FaultCrashAfterAuthorization FaultMode = "crash-after-authorization"
	FaultCrashAfterPreparation   FaultMode = "crash-after-preparation"
	FaultCrashAfterEffect        FaultMode = "crash-after-effect"
	FaultHTTP408BeforeEffect     FaultMode = "http-408-before-effect"
	FaultHTTP408AfterEffect      FaultMode = "http-408-after-effect"
	FaultEOFBeforeEffect         FaultMode = "eof-before-effect"
	FaultEOFAfterEffect          FaultMode = "eof-after-effect"
	Fault5xxBeforeEffect         FaultMode = "http-5xx-before-effect"
	Fault5xxAfterEffect          FaultMode = "http-5xx-after-effect"
	FaultDefinitiveRejection     FaultMode = "definitive-rejection"
)

func (f FaultMode) applyEffect() bool {
	switch f {
	case FaultNone, FaultCrashAfterEffect, FaultHTTP408AfterEffect, FaultEOFAfterEffect, Fault5xxAfterEffect:
		return true
	default:
		return false
	}
}

func (f FaultMode) ambiguity() string {
	switch f {
	case FaultHTTP408BeforeEffect, FaultHTTP408AfterEffect:
		return "http-408"
	case FaultEOFBeforeEffect, FaultEOFAfterEffect:
		return "eof"
	case Fault5xxBeforeEffect, Fault5xxAfterEffect:
		return "http-5xx"
	case FaultCrashAfterAuthorization, FaultCrashAfterPreparation, FaultCrashAfterEffect:
		return "crash"
	default:
		return ""
	}
}

type EffectReceipt struct {
	RequestDigest Digest
	Effect        Effect
	ObservedCount int
	ReceiptDigest Digest
}

func makeEffectReceipt(requestDigest Digest, effect Effect, count int) EffectReceipt {
	payload := []byte("effect-receipt/v2\x00" + requestDigest.String() + "\x00" + string(effect) + "\x00" + fmt.Sprint(count))
	return EffectReceipt{RequestDigest: requestDigest, Effect: effect, ObservedCount: count, ReceiptDigest: HashBytes(payload)}
}

// Engine serializes local calls, while durable cross-instance exclusion is a
// responsibility of a production adapter's fencing capability.
type Engine struct {
	mu            sync.Mutex
	ledger        LedgerAdapter
	executor      EffectExecutor
	now           func() time.Time
	authorityRole UnitRole
	authorityRoot Digest
	crashOnly     bool
}

func NewCrashModelEngine(oracle *CrashOracle, role UnitRole, authorityRoot Digest, now func() time.Time) (*Engine, error) {
	if oracle == nil || now == nil || authorityRoot.IsZero() ||
		(role != WriterAuthorityRole && role != ObserverAuthorityRole) {
		return nil, ErrInvalid
	}
	return &Engine{
		ledger: oracle, executor: oracle, now: now,
		authorityRole: role, authorityRoot: authorityRoot, crashOnly: true,
	}, nil
}

func NewProductionEngine(ledger LedgerAdapter, executor EffectExecutor, now func() time.Time) (*Engine, error) {
	// Gate A defines the required adapter contract, but deliberately ships no
	// concrete transactional ledger/signer or fenced effect adapter. Capability
	// self-reporting is therefore never production authority.
	return nil, ErrProductionAdapter
}

func (e *Engine) IsCrashModelOnly() bool { return e.crashOnly }

func consumeAuth(
	state *LedgerSnapshot,
	request MutationRequest,
	call CallKind,
	role UnitRole,
	root Digest,
	auth AuthEnvelope,
) error {
	jti := auth.JTIHash()
	challenge := auth.ChallengeHash()
	issued, ok := state.IssuedChallenges[challenge]
	if !ok {
		return ErrChallengeNotIssued
	}
	if issued.Consumed || state.UsedJTIs[jti] || state.UsedChallenges[challenge] {
		return ErrAuthReplay
	}
	requestDigest, err := request.Digest()
	if err != nil {
		return err
	}
	if issued.RequestDigest != requestDigest || issued.Call != call || issued.Role != role || issued.RootDigest != root {
		return ErrChallengeBinding
	}
	issued.Consumed = true
	state.IssuedChallenges[challenge] = issued
	state.UsedJTIs[jti] = true
	state.UsedChallenges[challenge] = true
	return nil
}

func operationKey(request MutationRequest) string {
	return request.Operation.OperationID + "/" + fmt.Sprint(request.Operation.Generation)
}

// IssueChallenge durably records an exact request/call/role/root grant before
// returning its opaque 256-bit bearer value. This Engine is only a Gate-A
// fault oracle; no production constructor is available.
func (e *Engine) IssueChallenge(ctx context.Context, request MutationRequest, call CallKind, random io.Reader) (AuthorityChallenge, error) {
	if err := request.Validate(); err != nil || call.validate() != nil || e.authorityRoot.IsZero() {
		return AuthorityChallenge{}, ErrInvalid
	}
	challenge, err := issueAuthorityChallenge(e.authorityRole, e.authorityRoot, random)
	if err != nil {
		return AuthorityChallenge{}, err
	}
	requestDigest, err := request.Digest()
	if err != nil {
		return AuthorityChallenge{}, err
	}
	challengeDigest := challenge.Hash()
	err = e.ledger.Transact(ctx, func(state *LedgerSnapshot) error {
		if _, exists := state.IssuedChallenges[challengeDigest]; exists {
			return ErrAuthReplay
		}
		state.IssuedChallenges[challengeDigest] = IssuedEngineChallenge{
			RequestDigest: requestDigest,
			Call:          call,
			Role:          e.authorityRole,
			RootDigest:    e.authorityRoot,
		}
		return nil
	})
	if err != nil {
		return AuthorityChallenge{}, err
	}
	return challenge, nil
}

func (e *Engine) burnAuth(ctx context.Context, request MutationRequest, call CallKind, auth AuthEnvelope) error {
	if err := auth.Validate(e.now()); err != nil {
		return err
	}
	if request.Validate() != nil || call.validate() != nil ||
		auth.Challenge.validateFor(e.authorityRole, e.authorityRoot) != nil {
		return ErrInvalid
	}
	return e.ledger.Transact(ctx, func(state *LedgerSnapshot) error {
		return consumeAuth(state, request, call, e.authorityRole, e.authorityRoot, auth)
	})
}

func (e *Engine) Mutate(ctx context.Context, request MutationRequest, auth AuthEnvelope, fault FaultMode) (EffectReceipt, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	// A valid authentication envelope is durably consumed before semantic
	// validation or lookup. No failed, missing, or quarantined call can roll it
	// back for reuse.
	if err := e.burnAuth(ctx, request, MutationCall, auth); err != nil {
		return EffectReceipt{}, err
	}
	if err := request.Validate(); err != nil {
		return EffectReceipt{}, err
	}
	bodyDigest, _ := request.Digest()
	key := request.key()
	var cached *EffectReceipt
	var mustReconcile bool
	var semanticErr error
	err := e.ledger.Transact(ctx, func(state *LedgerSnapshot) error {
		if state.QuarantinedOps[operationKey(request)] {
			semanticErr = ErrQuarantined
			return nil
		}
		claim := OperationClaim{RequestDigest: bodyDigest, Effect: request.Effect}
		if prior, ok := state.OperationClaims[operationKey(request)]; ok && prior != claim {
			state.QuarantinedOps[operationKey(request)] = true
			record := MutationRecord{Key: key, BodyDigest: bodyDigest, Effect: request.Effect, State: MutationQuarantined, QuarantineCause: "operation-effect-or-body-reuse"}
			state.Records[key] = record
			state.appendAudit("quarantine-operation-reuse", record)
			semanticErr = ErrQuarantined
			return nil
		}
		state.OperationClaims[operationKey(request)] = claim
		if record, ok := state.Records[key]; ok {
			if record.BodyDigest != bodyDigest {
				record.State = MutationQuarantined
				record.QuarantineCause = "canonical-body-drift"
				state.Records[key] = record
				state.QuarantinedOps[operationKey(request)] = true
				state.appendAudit("quarantine-body-drift", record)
				semanticErr = ErrQuarantined
				return nil
			}
			switch record.State {
			case MutationCompleted:
				copy := record.Receipt
				cached = &copy
				return nil
			case MutationStarted, MutationAmbiguous:
				mustReconcile = true
				return nil
			default:
				semanticErr = ErrQuarantined
				return nil
			}
		}
		record := MutationRecord{Key: key, BodyDigest: bodyDigest, Effect: request.Effect, State: MutationStarted}
		state.Records[key] = record
		state.appendAudit("authorization-consumed", record)
		return nil
	})
	if err != nil {
		return EffectReceipt{}, err
	}
	if semanticErr != nil {
		return EffectReceipt{}, semanticErr
	}
	if cached != nil {
		return *cached, nil
	}
	if mustReconcile {
		return EffectReceipt{}, ErrReconcileRequired
	}
	if fault == FaultCrashAfterAuthorization {
		return EffectReceipt{}, ErrSimulatedCrash
	}
	if fault == FaultDefinitiveRejection {
		if err := e.quarantine(ctx, request, bodyDigest, "definitive-rejection"); err != nil {
			return EffectReceipt{}, err
		}
		return EffectReceipt{}, ErrQuarantined
	}
	if err := e.ledger.Transact(ctx, func(state *LedgerSnapshot) error {
		record := state.Records[key]
		record.Prepared = true
		state.Records[key] = record
		state.appendAudit("prepared", record)
		return nil
	}); err != nil {
		return EffectReceipt{}, err
	}
	if fault == FaultCrashAfterPreparation {
		return EffectReceipt{}, ErrSimulatedCrash
	}
	if err := e.ledger.Transact(ctx, func(state *LedgerSnapshot) error {
		record := state.Records[key]
		if !record.Prepared || record.Issued {
			return ErrQuarantined
		}
		record.Issued = true
		state.Records[key] = record
		state.appendAudit("issued", record)
		return nil
	}); err != nil {
		return EffectReceipt{}, err
	}
	if err := e.executor.ApplyOnce(ctx, key, request.Effect, bodyDigest, fault.applyEffect()); err != nil {
		return EffectReceipt{}, err
	}
	if fault == FaultCrashAfterEffect {
		return EffectReceipt{}, ErrSimulatedCrash
	}
	if ambiguity := fault.ambiguity(); ambiguity != "" {
		if err := e.ledger.Transact(ctx, func(state *LedgerSnapshot) error {
			record := state.Records[key]
			record.State = MutationAmbiguous
			record.Ambiguity = ambiguity
			state.Records[key] = record
			state.appendAudit("ambiguous-"+ambiguity, record)
			return nil
		}); err != nil {
			return EffectReceipt{}, err
		}
		return EffectReceipt{}, ErrReconcileRequired
	}
	receipt := makeEffectReceipt(bodyDigest, request.Effect, 1)
	if err := e.complete(ctx, request, bodyDigest, receipt); err != nil {
		return EffectReceipt{}, err
	}
	return receipt, nil
}

// Status performs a GET-only observation. For a prepared+issued marker whose
// provider-owned predecessor is proven unchanged, the same status handler may
// claim and internally execute one fenced continuation; callers never receive
// a second mutation directive.
func (e *Engine) Status(ctx context.Context, request MutationRequest, auth AuthEnvelope) (EffectReceipt, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.burnAuth(ctx, request, StatusCall, auth); err != nil {
		return EffectReceipt{}, err
	}
	if err := request.Validate(); err != nil {
		return EffectReceipt{}, err
	}
	bodyDigest, _ := request.Digest()
	key := request.key()
	var existing MutationRecord
	var semanticErr error
	err := e.ledger.Transact(ctx, func(state *LedgerSnapshot) error {
		if state.QuarantinedOps[operationKey(request)] {
			semanticErr = ErrQuarantined
			return nil
		}
		claim := OperationClaim{RequestDigest: bodyDigest, Effect: request.Effect}
		if prior, ok := state.OperationClaims[operationKey(request)]; ok && prior != claim {
			state.QuarantinedOps[operationKey(request)] = true
			semanticErr = ErrQuarantined
			return nil
		}
		record, ok := state.Records[key]
		if !ok {
			semanticErr = ErrInvalid
			return nil
		}
		if record.BodyDigest != bodyDigest {
			record.State = MutationQuarantined
			record.QuarantineCause = "status-body-drift"
			state.Records[key] = record
			state.QuarantinedOps[operationKey(request)] = true
			state.appendAudit("quarantine-status-drift", record)
			semanticErr = ErrQuarantined
			return nil
		}
		if record.State == MutationQuarantined || state.QuarantinedOps[operationKey(request)] {
			semanticErr = ErrQuarantined
			return nil
		}
		existing = record
		return nil
	})
	if err != nil {
		return EffectReceipt{}, err
	}
	if semanticErr != nil {
		return EffectReceipt{}, semanticErr
	}
	if existing.State == MutationCompleted {
		return existing.Receipt, nil
	}
	count, err := e.executor.Observe(ctx, key, request.Effect, bodyDigest)
	if err != nil {
		return EffectReceipt{}, err
	}
	switch count {
	case 1:
		if !existing.Prepared || !existing.Issued {
			if err := e.quarantine(ctx, request, bodyDigest, "observed-before-wire-eligibility"); err != nil {
				return EffectReceipt{}, err
			}
			return EffectReceipt{}, ErrQuarantined
		}
		receipt := makeEffectReceipt(bodyDigest, request.Effect, count)
		if err := e.complete(ctx, request, bodyDigest, receipt); err != nil {
			return EffectReceipt{}, err
		}
		return receipt, nil
	case 0:
		if request.Effect == EffectMarkerCAS {
			predecessorUnchanged, proofErr := e.executor.ProtectedMarkerPredecessorUnchanged(ctx, key, bodyDigest)
			if proofErr != nil {
				return EffectReceipt{}, proofErr
			}
			if !predecessorUnchanged {
				if err := e.quarantine(ctx, request, bodyDigest, "protected-predecessor-drift"); err != nil {
					return EffectReceipt{}, err
				}
				return EffectReceipt{}, ErrNotObserved
			}
			var concurrentReceipt *EffectReceipt
			var alreadyClaimed bool
			var prewireRejected bool
			claimErr := e.ledger.Transact(ctx, func(state *LedgerSnapshot) error {
				record, ok := state.Records[key]
				if !ok || record.BodyDigest != bodyDigest {
					return ErrQuarantined
				}
				if record.State == MutationCompleted {
					copy := record.Receipt
					concurrentReceipt = &copy
					return nil
				}
				if record.State == MutationQuarantined || state.QuarantinedOps[operationKey(request)] {
					return ErrQuarantined
				}
				if !record.Prepared {
					record.State = MutationQuarantined
					record.QuarantineCause = "continuation-before-prepared-fence"
					state.Records[key] = record
					state.QuarantinedOps[operationKey(request)] = true
					state.appendAudit("quarantine-continuation-before-prepared", record)
					prewireRejected = true
					return nil
				}
				if record.ContinuationClaimed {
					alreadyClaimed = true
					return nil
				}
				if !record.Issued {
					record.Issued = true
					state.Records[key] = record
					state.appendAudit("issued-by-recovery", record)
				}
				record.ContinuationClaimed = true
				state.Records[key] = record
				state.appendAudit("continuation-claimed", record)
				return nil
			})
			if claimErr != nil {
				return EffectReceipt{}, claimErr
			}
			if concurrentReceipt != nil {
				return *concurrentReceipt, nil
			}
			if prewireRejected {
				return EffectReceipt{}, ErrQuarantined
			}
			if alreadyClaimed {
				return EffectReceipt{}, ErrReconcileRequired
			}
			if claimErr == nil {
				if err := e.executor.ApplyOnce(ctx, key, request.Effect, bodyDigest, true); err == nil {
					continuedCount, observeErr := e.executor.Observe(ctx, key, request.Effect, bodyDigest)
					if observeErr == nil && continuedCount == 1 {
						receipt := makeEffectReceipt(bodyDigest, request.Effect, continuedCount)
						if err := e.complete(ctx, request, bodyDigest, receipt); err == nil {
							return receipt, nil
						}
					}
				}
			}
		}
		if err := e.quarantine(ctx, request, bodyDigest, "zero-observed-after-ambiguity"); err != nil {
			return EffectReceipt{}, err
		}
		return EffectReceipt{}, ErrNotObserved
	default:
		if err := e.quarantine(ctx, request, bodyDigest, "multiple-observed-after-ambiguity"); err != nil {
			return EffectReceipt{}, err
		}
		return EffectReceipt{}, ErrMultipleObserved
	}
}

func (e *Engine) complete(ctx context.Context, request MutationRequest, bodyDigest Digest, receipt EffectReceipt) error {
	return e.ledger.Transact(ctx, func(state *LedgerSnapshot) error {
		record, ok := state.Records[request.key()]
		if !ok || record.BodyDigest != bodyDigest || record.State == MutationQuarantined || !record.Prepared || !record.Issued {
			return ErrQuarantined
		}
		if record.State == MutationCompleted {
			if record.Receipt != receipt {
				return ErrQuarantined
			}
			return nil
		}
		record.State = MutationCompleted
		record.Receipt = receipt
		state.Records[request.key()] = record
		state.appendAudit("completed", record)
		return nil
	})
}

func (e *Engine) quarantine(ctx context.Context, request MutationRequest, bodyDigest Digest, cause string) error {
	return e.ledger.Transact(ctx, func(state *LedgerSnapshot) error {
		record := state.Records[request.key()]
		// A stale controller may never replace a successfully completed durable
		// receipt with quarantine after another controller won reconciliation.
		if record.State == MutationCompleted && record.BodyDigest == bodyDigest {
			return nil
		}
		record.Key = request.key()
		record.BodyDigest = bodyDigest
		record.Effect = request.Effect
		record.State = MutationQuarantined
		record.QuarantineCause = cause
		state.Records[request.key()] = record
		state.QuarantinedOps[operationKey(request)] = true
		state.appendAudit("quarantined", record)
		return nil
	})
}

func IsAmbiguousError(err error) bool {
	return errors.Is(err, ErrReconcileRequired) || errors.Is(err, ErrSimulatedCrash)
}
