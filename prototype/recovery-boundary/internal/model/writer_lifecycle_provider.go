package model

import (
	"sync"
	"time"
)

const syntheticWriterOldInstanceGrace = time.Minute

// WriterLifecycleProvider is the selector-free provider mutation surface used
// by the Gate-A teardown model. The operation, writer source identity, provider
// configuration, and pre-operation projection are constructor-bound. Gate A
// deliberately supplies no production implementation.
type WriterLifecycleProvider interface {
	IsTestOracleOnly() bool
	IdentitySHA256() Digest
	OperationBodySHA256() Digest
	SourceTargetSHA256() Digest
	ProviderConfigSHA256() Digest
	OriginalProjectionSHA256() Digest
	ApplyConfigured(Effect) (Digest, error)
	ObserveConfigured(Effect) ([]Digest, error)
}

// writerTerminalFactsReader is intentionally GET-only. The terminal observer
// receives this reduced view and cannot reach ApplyConfigured through it.
type writerTerminalFactsReader interface {
	IsTestOracleOnly() bool
	IdentitySHA256() Digest
	OperationBodySHA256() Digest
	SourceTargetSHA256() Digest
	ProviderConfigSHA256() Digest
	OriginalProjectionSHA256() Digest
	ReadTerminalFacts() ([]writerTerminalProviderFacts, error)
}

// SyntheticWriterTerminalFactsFacade is a concrete reduced GET-only view.
// It deliberately exposes none of WriterLifecycleProvider's mutation methods,
// and a value passed to the terminal observer cannot be type-asserted back to
// that mutation interface.
type SyntheticWriterTerminalFactsFacade struct {
	provider *SyntheticWriterLifecycleProvider
}

type writerTerminalProviderFacts struct {
	OriginalProjection          PreOperationProjection
	EffectCompletedAt           map[Effect]time.Time
	WrappingProviderResult      Digest
	DeleteActionSHA256          Digest
	DeleteActionTerminal        bool
	DirectGETAbsent             bool
	AppInventoryComplete        bool
	DeploymentInventoryComplete bool
	PaginatedAppCount           int
	PaginatedDeployments        int
	RollbackCapableCount        int
	NoWriterOrBroadRules        bool
	CapabilityRevoked           bool
	LeafRevoked                 bool
	MTLSRevoked                 bool
	WrappingKeyRevoked          bool
	BindingAbsent               bool
	CredentialAbsent            bool
	FullRedeployComplete        bool
	ProviderProvenanceSHA256    Digest
	StableFirewallSHA256        Digest
	StableActionLedgerSHA256    Digest
	OldInstanceGraceElapsedAt   time.Time
	DirectGETAt                 time.Time
	InventoryAt                 time.Time
	DeleteActionAt              time.Time
	OperationInventoryComplete  bool
	ProviderOperationCount      int
	NonterminalOperationCount   int
}

func cloneWriterTerminalProviderFacts(source writerTerminalProviderFacts) writerTerminalProviderFacts {
	clone := source
	clone.EffectCompletedAt = make(map[Effect]time.Time, len(source.EffectCompletedAt))
	for effect, completedAt := range source.EffectCompletedAt {
		clone.EffectCompletedAt[effect] = completedAt
	}
	return clone
}

func writerLifecycleEffectAllowed(effect Effect) bool {
	if effect == EffectBrokerDelete {
		return true
	}
	for _, candidate := range writerTeardownEffects {
		if effect == candidate {
			return true
		}
	}
	return false
}

func writerTerminalProviderEffects() []Effect {
	return append(append([]Effect(nil), writerTeardownEffects...), EffectBrokerDelete)
}

func validateWriterTerminalProviderInventory(facts writerTerminalProviderFacts) error {
	expected := writerTerminalProviderEffects()
	if !facts.OperationInventoryComplete || facts.ProviderOperationCount != len(expected) ||
		facts.NonterminalOperationCount != 0 || len(facts.EffectCompletedAt) != len(expected) {
		return ErrForkNotAuthorized
	}
	for _, effect := range expected {
		completedAt, ok := facts.EffectCompletedAt[effect]
		if !ok || completedAt.IsZero() {
			return ErrForkNotAuthorized
		}
		if _, err := canonicalTime(completedAt); err != nil {
			return ErrForkNotAuthorized
		}
	}
	for effect := range facts.EffectCompletedAt {
		if !writerLifecycleEffectAllowed(effect) {
			return ErrForkNotAuthorized
		}
	}
	return nil
}

func writerLifecycleProviderResult(operation, source, config, projection, identity Digest, effect Effect) Digest {
	return HashString("writer-lifecycle-provider-result/v2\x00" + operation.String() + "\x00" + source.String() + "\x00" + config.String() + "\x00" + projection.String() + "\x00" + identity.String() + "\x00" + string(effect))
}

// SyntheticWriterLifecycleProvider is a deterministic, fault-injecting test
// oracle. It performs no network activity and must never be represented as a
// production provider adapter.
type SyntheticWriterLifecycleProvider struct {
	mu                                              sync.Mutex
	identity, operation, source, config, projection Digest
	original                                        PreOperationProjection
	now                                             func() time.Time
	results                                         map[Effect]Digest
	completedAt                                     map[Effect]time.Time
	applyCalls, observeCalls                        map[Effect]int
	applyAmbiguity                                  map[Effect]ProviderAmbiguityKind
	observeFailure                                  map[Effect]error
	observeOverride                                 map[Effect][]Digest
	terminalReadFailure                             error
	terminalReadOverride                            []writerTerminalProviderFacts
	terminalReadSequence                            [][]writerTerminalProviderFacts
	terminalReadCalls                               int
	terminalReadHook                                func(int)
}

func NewSyntheticWriterLifecycleProvider(base OperationBody, source, config Digest, original PreOperationProjection, now func() time.Time) (*SyntheticWriterLifecycleProvider, error) {
	operation, operationErr := base.Digest()
	projection, projectionErr := original.Digest()
	if operationErr != nil || projectionErr != nil || source.IsZero() || config.IsZero() || now == nil {
		return nil, ErrInvalid
	}
	identity := HashString("synthetic-writer-lifecycle-provider/v2\x00" + operation.String() + "\x00" + source.String() + "\x00" + config.String() + "\x00" + projection.String())
	return &SyntheticWriterLifecycleProvider{
		identity: identity, operation: operation, source: source, config: config, projection: projection,
		original: original, now: now, results: map[Effect]Digest{}, completedAt: map[Effect]time.Time{},
		applyCalls: map[Effect]int{}, observeCalls: map[Effect]int{}, applyAmbiguity: map[Effect]ProviderAmbiguityKind{},
		observeFailure: map[Effect]error{}, observeOverride: map[Effect][]Digest{},
	}, nil
}

func (*SyntheticWriterLifecycleProvider) IsTestOracleOnly() bool             { return true }
func (p *SyntheticWriterLifecycleProvider) IdentitySHA256() Digest           { return p.identity }
func (p *SyntheticWriterLifecycleProvider) OperationBodySHA256() Digest      { return p.operation }
func (p *SyntheticWriterLifecycleProvider) SourceTargetSHA256() Digest       { return p.source }
func (p *SyntheticWriterLifecycleProvider) ProviderConfigSHA256() Digest     { return p.config }
func (p *SyntheticWriterLifecycleProvider) OriginalProjectionSHA256() Digest { return p.projection }

func (p *SyntheticWriterLifecycleProvider) ApplyConfigured(effect Effect) (Digest, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !writerLifecycleEffectAllowed(effect) || p.applyCalls[effect] != 0 {
		return Digest{}, ErrQuarantined
	}
	if effect == EffectBrokerDelete {
		for _, predecessor := range writerTeardownEffects {
			if p.results[predecessor].IsZero() {
				return Digest{}, ErrForkNotAuthorized
			}
		}
	} else {
		index := -1
		for candidateIndex, candidate := range writerTeardownEffects {
			if candidate == effect {
				index = candidateIndex
				break
			}
		}
		if index < 0 || (index > 0 && p.results[writerTeardownEffects[index-1]].IsZero()) {
			return Digest{}, ErrForkNotAuthorized
		}
	}
	p.applyCalls[effect]++
	result := writerLifecycleProviderResult(p.operation, p.source, p.config, p.projection, p.identity, effect)
	p.results[effect] = result
	p.completedAt[effect] = p.now()
	if ambiguity := p.applyAmbiguity[effect]; ambiguity != "" {
		return Digest{}, &ProviderAmbiguityError{Kind: ambiguity}
	}
	return result, nil
}

func (p *SyntheticWriterLifecycleProvider) ObserveConfigured(effect Effect) ([]Digest, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !writerLifecycleEffectAllowed(effect) {
		return nil, ErrInvalid
	}
	p.observeCalls[effect]++
	if err := p.observeFailure[effect]; err != nil {
		return nil, err
	}
	if override, ok := p.observeOverride[effect]; ok {
		return append([]Digest(nil), override...), nil
	}
	if result := p.results[effect]; !result.IsZero() {
		return []Digest{result}, nil
	}
	return nil, nil
}

func (p *SyntheticWriterLifecycleProvider) terminalFacts() ([]writerTerminalProviderFacts, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.terminalReadCalls++
	if p.terminalReadHook != nil {
		p.terminalReadHook(p.terminalReadCalls)
	}
	if p.terminalReadFailure != nil {
		return nil, p.terminalReadFailure
	}
	if len(p.terminalReadSequence) != 0 {
		selected := p.terminalReadSequence[0]
		p.terminalReadSequence = p.terminalReadSequence[1:]
		result := make([]writerTerminalProviderFacts, len(selected))
		for index := range selected {
			result[index] = cloneWriterTerminalProviderFacts(selected[index])
		}
		return result, nil
	}
	if p.terminalReadOverride != nil {
		result := make([]writerTerminalProviderFacts, len(p.terminalReadOverride))
		for index := range p.terminalReadOverride {
			result[index] = cloneWriterTerminalProviderFacts(p.terminalReadOverride[index])
		}
		return result, nil
	}
	for _, effect := range writerTerminalProviderEffects() {
		if p.results[effect].IsZero() || p.completedAt[effect].IsZero() {
			return nil, nil
		}
	}
	deletedAt := p.completedAt[EffectBrokerDelete]
	facts := writerTerminalProviderFacts{
		OriginalProjection: p.original, EffectCompletedAt: make(map[Effect]time.Time, len(p.completedAt)),
		WrappingProviderResult: p.results[EffectWrappingKeyRevoke],
		DeleteActionSHA256:     p.results[EffectBrokerDelete],
		DeleteActionTerminal:   true, DirectGETAbsent: true, AppInventoryComplete: true, DeploymentInventoryComplete: true,
		PaginatedAppCount: 0, PaginatedDeployments: 0, RollbackCapableCount: 0,
		NoWriterOrBroadRules: true, CapabilityRevoked: true, LeafRevoked: true, MTLSRevoked: true, WrappingKeyRevoked: true,
		BindingAbsent: true, CredentialAbsent: true, FullRedeployComplete: true,
		ProviderProvenanceSHA256: p.original.ProvenanceSHA256, StableFirewallSHA256: p.original.FirewallSHA256, StableActionLedgerSHA256: p.original.ActionLedgerSHA256,
		DirectGETAt: deletedAt.Add(time.Second), InventoryAt: deletedAt.Add(2 * time.Second), DeleteActionAt: deletedAt.Add(3 * time.Second),
		OldInstanceGraceElapsedAt:  deletedAt.Add(syntheticWriterOldInstanceGrace),
		OperationInventoryComplete: true, ProviderOperationCount: len(p.results), NonterminalOperationCount: 0,
	}
	for effect, completedAt := range p.completedAt {
		facts.EffectCompletedAt[effect] = completedAt
	}
	return []writerTerminalProviderFacts{facts}, nil
}

func (p *SyntheticWriterLifecycleProvider) TerminalFactsFacade() (*SyntheticWriterTerminalFactsFacade, error) {
	if p == nil || p.identity.IsZero() {
		return nil, ErrInvalid
	}
	return &SyntheticWriterTerminalFactsFacade{provider: p}, nil
}

func (f *SyntheticWriterTerminalFactsFacade) IsTestOracleOnly() bool {
	return f != nil && f.provider != nil
}
func (f *SyntheticWriterTerminalFactsFacade) IdentitySHA256() Digest {
	if f == nil || f.provider == nil {
		return Digest{}
	}
	return f.provider.IdentitySHA256()
}
func (f *SyntheticWriterTerminalFactsFacade) OperationBodySHA256() Digest {
	if f == nil || f.provider == nil {
		return Digest{}
	}
	return f.provider.OperationBodySHA256()
}
func (f *SyntheticWriterTerminalFactsFacade) SourceTargetSHA256() Digest {
	if f == nil || f.provider == nil {
		return Digest{}
	}
	return f.provider.SourceTargetSHA256()
}
func (f *SyntheticWriterTerminalFactsFacade) ProviderConfigSHA256() Digest {
	if f == nil || f.provider == nil {
		return Digest{}
	}
	return f.provider.ProviderConfigSHA256()
}
func (f *SyntheticWriterTerminalFactsFacade) OriginalProjectionSHA256() Digest {
	if f == nil || f.provider == nil {
		return Digest{}
	}
	return f.provider.OriginalProjectionSHA256()
}
func (f *SyntheticWriterTerminalFactsFacade) ReadTerminalFacts() ([]writerTerminalProviderFacts, error) {
	if f == nil || f.provider == nil {
		return nil, ErrInvalid
	}
	return f.provider.terminalFacts()
}

func (p *SyntheticWriterLifecycleProvider) setApplyAmbiguity(effect Effect, kind ProviderAmbiguityKind) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.applyAmbiguity[effect] = kind
}

func (p *SyntheticWriterLifecycleProvider) setObserveFailure(effect Effect, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.observeFailure[effect] = err
}

func (p *SyntheticWriterLifecycleProvider) setObserveOverride(effect Effect, values []Digest) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.observeOverride[effect] = append([]Digest(nil), values...)
}

func (p *SyntheticWriterLifecycleProvider) callCounts(effect Effect) (int, int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.applyCalls[effect], p.observeCalls[effect]
}

func (p *SyntheticWriterLifecycleProvider) setTerminalReadFailure(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.terminalReadFailure = err
}

func (p *SyntheticWriterLifecycleProvider) setTerminalReadOverride(values []writerTerminalProviderFacts) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.terminalReadOverride = values
}

func (p *SyntheticWriterLifecycleProvider) setTerminalReadSequence(values ...[]writerTerminalProviderFacts) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.terminalReadSequence = append([][]writerTerminalProviderFacts(nil), values...)
}

func (p *SyntheticWriterLifecycleProvider) setTerminalReadHook(hook func(int)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.terminalReadHook = hook
}

// WriterTerminalObservationReceipt is the complete GET-only protected-
// workflow facts envelope. It has no third signing root: the outer verifier
// independently rechecks these bindings, while the portable terminal receipt
// remains signed by the existing writer-domain root.
type WriterTerminalObservationReceipt struct {
	Version                                                                                 string
	OperationBodySHA256, SourceTargetSHA256, ProviderConfigSHA256, OriginalProjectionSHA256 Digest
	MutationProviderSHA256, ObserverIdentitySHA256, FactsSHA256                             Digest
	ReadOneAt, ReadTwoAt                                                                    time.Time
}

func (r WriterTerminalObservationReceipt) payload() []byte {
	first, _ := canonicalTime(r.ReadOneAt)
	second, _ := canonicalTime(r.ReadTwoAt)
	return []byte("writer-terminal-provider-observation/v3\x00" + r.Version + "\x00" + r.OperationBodySHA256.String() + "\x00" + r.SourceTargetSHA256.String() + "\x00" + r.ProviderConfigSHA256.String() + "\x00" + r.OriginalProjectionSHA256.String() + "\x00" + r.MutationProviderSHA256.String() + "\x00" + r.ObserverIdentitySHA256.String() + "\x00" + r.FactsSHA256.String() + "\x00" + first + "\x00" + second)
}

func (r WriterTerminalObservationReceipt) Digest() Digest {
	return HashBytes(r.payload())
}

func verifyWriterTerminalObservationReceipt(receipt WriterTerminalObservationReceipt, facts Digest) error {
	if receipt.Version != ProtocolVersion || receipt.OperationBodySHA256.IsZero() || receipt.SourceTargetSHA256.IsZero() || receipt.ProviderConfigSHA256.IsZero() || receipt.OriginalProjectionSHA256.IsZero() || receipt.MutationProviderSHA256.IsZero() ||
		receipt.ObserverIdentitySHA256.IsZero() || receipt.FactsSHA256 != facts ||
		receipt.ReadTwoAt.Sub(receipt.ReadOneAt) < MinimumTerminalObservationDelay || receipt.ReadTwoAt.Sub(receipt.ReadOneAt) > MaximumTerminalObservationDelay ||
		receipt.Digest().IsZero() {
		return ErrForkNotAuthorized
	}
	return nil
}

func verifyPinnedWriterTerminalObservation(boundary *ClosedBoundary, receipt WriterTerminalObservationReceipt, facts Digest) error {
	if boundary == nil || receipt.ObserverIdentitySHA256 != boundary.providerObserver {
		return ErrForkNotAuthorized
	}
	return verifyWriterTerminalObservationReceipt(receipt, facts)
}

type WriterTerminalObservationAdapter interface {
	IsTestOracleOnly() bool
	IdentitySHA256() Digest
	OperationBodySHA256() Digest
	SourceTargetSHA256() Digest
	ProviderConfigSHA256() Digest
	OriginalProjectionSHA256() Digest
	MutationProviderSHA256() Digest
	observeTerminal(time.Time, time.Time, time.Time) (WriterTerminalEvidence, error)
}

type SyntheticWriterTerminalObserver struct {
	reader                              writerTerminalFactsReader
	clock                               ObservationClock
	authorityNow                        func() time.Time
	identity, operation, source, config Digest
	projection, mutationProvider        Digest
}

func NewSyntheticWriterTerminalObserver(reader *SyntheticWriterTerminalFactsFacade, clock ObservationClock, expectedIdentity Digest) (*SyntheticWriterTerminalObserver, error) {
	if reader == nil || !reader.IsTestOracleOnly() || clock == nil || reader.IdentitySHA256().IsZero() || reader.OperationBodySHA256().IsZero() || reader.SourceTargetSHA256().IsZero() || reader.ProviderConfigSHA256().IsZero() || reader.OriginalProjectionSHA256().IsZero() {
		return nil, ErrInvalid
	}
	identity := expectedIdentity
	if identity.IsZero() {
		return nil, ErrRoleIsolation
	}
	return &SyntheticWriterTerminalObserver{reader: reader, clock: clock, identity: identity,
		operation: reader.OperationBodySHA256(), source: reader.SourceTargetSHA256(), config: reader.ProviderConfigSHA256(), projection: reader.OriginalProjectionSHA256(), mutationProvider: reader.IdentitySHA256()}, nil
}

func (*SyntheticWriterTerminalObserver) IsTestOracleOnly() bool             { return true }
func (o *SyntheticWriterTerminalObserver) IdentitySHA256() Digest           { return o.identity }
func (o *SyntheticWriterTerminalObserver) OperationBodySHA256() Digest      { return o.operation }
func (o *SyntheticWriterTerminalObserver) SourceTargetSHA256() Digest       { return o.source }
func (o *SyntheticWriterTerminalObserver) ProviderConfigSHA256() Digest     { return o.config }
func (o *SyntheticWriterTerminalObserver) OriginalProjectionSHA256() Digest { return o.projection }
func (o *SyntheticWriterTerminalObserver) MutationProviderSHA256() Digest   { return o.mutationProvider }

func (o *SyntheticWriterTerminalObserver) bindAuthorityClock(now func() time.Time) error {
	if o == nil || now == nil {
		return ErrRoleIsolation
	}
	if o.authorityNow != nil {
		if !o.authorityNow().Equal(now()) {
			return ErrRoleIsolation
		}
		return nil
	}
	o.authorityNow = now
	return nil
}

func (o *SyntheticWriterTerminalObserver) trustedObservationTime(previous, issuedAt, notBefore, notAfter time.Time) (time.Time, error) {
	if o == nil || o.clock == nil || o.authorityNow == nil {
		return time.Time{}, ErrRoleIsolation
	}
	observed := o.clock.Now()
	trusted := o.authorityNow()
	if !observed.Equal(trusted) || (!previous.IsZero() && observed.Before(previous)) ||
		observed.Before(issuedAt) || observed.Before(notBefore) || !observed.Before(notAfter) {
		return time.Time{}, ErrForkNotAuthorized
	}
	if _, err := canonicalTime(observed); err != nil {
		return time.Time{}, ErrForkNotAuthorized
	}
	return observed, nil
}

func equalWriterTerminalProviderFacts(left, right writerTerminalProviderFacts) bool {
	if left.OriginalProjection != right.OriginalProjection || left.WrappingProviderResult != right.WrappingProviderResult || left.DeleteActionSHA256 != right.DeleteActionSHA256 || left.DeleteActionTerminal != right.DeleteActionTerminal || left.DirectGETAbsent != right.DirectGETAbsent || left.AppInventoryComplete != right.AppInventoryComplete || left.DeploymentInventoryComplete != right.DeploymentInventoryComplete || left.PaginatedAppCount != right.PaginatedAppCount || left.PaginatedDeployments != right.PaginatedDeployments || left.RollbackCapableCount != right.RollbackCapableCount || left.NoWriterOrBroadRules != right.NoWriterOrBroadRules || left.CapabilityRevoked != right.CapabilityRevoked || left.LeafRevoked != right.LeafRevoked || left.MTLSRevoked != right.MTLSRevoked || left.WrappingKeyRevoked != right.WrappingKeyRevoked || left.BindingAbsent != right.BindingAbsent || left.CredentialAbsent != right.CredentialAbsent || left.FullRedeployComplete != right.FullRedeployComplete || left.ProviderProvenanceSHA256 != right.ProviderProvenanceSHA256 || left.StableFirewallSHA256 != right.StableFirewallSHA256 || left.StableActionLedgerSHA256 != right.StableActionLedgerSHA256 || left.OldInstanceGraceElapsedAt != right.OldInstanceGraceElapsedAt || left.DirectGETAt != right.DirectGETAt || left.InventoryAt != right.InventoryAt || left.DeleteActionAt != right.DeleteActionAt || left.OperationInventoryComplete != right.OperationInventoryComplete || left.ProviderOperationCount != right.ProviderOperationCount || left.NonterminalOperationCount != right.NonterminalOperationCount || len(left.EffectCompletedAt) != len(right.EffectCompletedAt) {
		return false
	}
	for effect, completedAt := range left.EffectCompletedAt {
		if right.EffectCompletedAt[effect] != completedAt {
			return false
		}
	}
	return true
}

func terminalEvidenceFromProviderFacts(first, second writerTerminalProviderFacts, firstRead, secondRead time.Time) WriterTerminalEvidence {
	return WriterTerminalEvidence{
		OriginalProjection:    first.OriginalProjection,
		FirewallReadOneSHA256: first.StableFirewallSHA256, FirewallReadTwoSHA256: second.StableFirewallSHA256,
		ActionReadOneSHA256: first.StableActionLedgerSHA256, ActionReadTwoSHA256: second.StableActionLedgerSHA256,
		ProviderProvenanceSHA256:                first.ProviderProvenanceSHA256,
		WrappingKeyProviderObservedResultSHA256: first.WrappingProviderResult,
		NoWriterOrBroadRules:                    first.NoWriterOrBroadRules, CapabilityRevoked: first.CapabilityRevoked, LeafRevoked: first.LeafRevoked,
		MTLSRevoked: first.MTLSRevoked, WrappingKeyRevoked: first.WrappingKeyRevoked, BindingAbsent: first.BindingAbsent,
		CredentialAbsent: first.CredentialAbsent, FullRedeployComplete: first.FullRedeployComplete,
		Deletion: DeleteObservation{DirectGETAbsent: first.DirectGETAbsent, PaginatedAppCount: first.PaginatedAppCount, PaginatedDeployments: first.PaginatedDeployments,
			DeleteActionTerminal: first.DeleteActionTerminal, RollbackCapableCount: first.RollbackCapableCount, DeleteActionSHA256: first.DeleteActionSHA256,
			DirectGETAt: first.DirectGETAt, InventoryAt: first.InventoryAt, DeleteActionAt: first.DeleteActionAt},
		DeletionReconciled: true, OldInstanceGraceElapsed: !firstRead.Before(first.OldInstanceGraceElapsedAt),
		AppInventoryReadOneComplete: first.AppInventoryComplete, AppInventoryReadTwoComplete: second.AppInventoryComplete,
		DeploymentInventoryReadOneComplete: first.DeploymentInventoryComplete, DeploymentInventoryReadTwoComplete: second.DeploymentInventoryComplete,
		AppInventoryCountOne: first.PaginatedAppCount, AppInventoryCountTwo: second.PaginatedAppCount,
		DeploymentInventoryCountOne: first.PaginatedDeployments, DeploymentInventoryCountTwo: second.PaginatedDeployments,
		ProviderOperationReadOneComplete: first.OperationInventoryComplete, ProviderOperationReadTwoComplete: second.OperationInventoryComplete,
		ProviderOperationCountOne: first.ProviderOperationCount, ProviderOperationCountTwo: second.ProviderOperationCount,
		ProviderNonterminalCountOne: first.NonterminalOperationCount, ProviderNonterminalCountTwo: second.NonterminalOperationCount,
		CapabilityRevokedAt: first.EffectCompletedAt[EffectCapabilityRevoke], LeafRevokedAt: first.EffectCompletedAt[EffectLeafRevoke],
		MTLSRevokedAt: first.EffectCompletedAt[EffectMTLSRevoke], WrappingKeyRevokedAt: first.EffectCompletedAt[EffectWrappingKeyRevoke],
		BindingRemovedAt: first.EffectCompletedAt[EffectBindingRemove], CredentialRemovedAt: first.EffectCompletedAt[EffectCredentialRemove],
		FullRedeployAt: first.EffectCompletedAt[EffectFullRedeploy], FirewallRestoredAt: first.EffectCompletedAt[EffectTrustedSourceDel],
		DeletionObservedAt: first.EffectCompletedAt[EffectBrokerDelete], OldInstanceGraceElapsedAt: first.OldInstanceGraceElapsedAt,
		ReadOneAt: firstRead, ReadTwoAt: secondRead,
	}
}

func (o *SyntheticWriterTerminalObserver) observeTerminal(issuedAt, notBefore, notAfter time.Time) (WriterTerminalEvidence, error) {
	readOneStartedAt, clockErr := o.trustedObservationTime(time.Time{}, issuedAt, notBefore, notAfter)
	if clockErr != nil {
		return WriterTerminalEvidence{}, clockErr
	}
	firstCandidates, err := o.reader.ReadTerminalFacts()
	firstRead, clockErr := o.trustedObservationTime(readOneStartedAt, issuedAt, notBefore, notAfter)
	if clockErr != nil {
		return WriterTerminalEvidence{}, clockErr
	}
	if err != nil {
		return WriterTerminalEvidence{}, err
	}
	if len(firstCandidates) == 0 {
		return WriterTerminalEvidence{}, ErrNotObserved
	}
	if len(firstCandidates) != 1 {
		return WriterTerminalEvidence{}, ErrMultipleObserved
	}
	if err := validateWriterTerminalProviderInventory(firstCandidates[0]); err != nil {
		return WriterTerminalEvidence{}, err
	}
	o.clock.Wait(MinimumTerminalObservationDelay)
	readTwoStartedAt, clockErr := o.trustedObservationTime(firstRead, issuedAt, notBefore, notAfter)
	if clockErr != nil {
		return WriterTerminalEvidence{}, clockErr
	}
	secondCandidates, err := o.reader.ReadTerminalFacts()
	secondRead, clockErr := o.trustedObservationTime(readTwoStartedAt, issuedAt, notBefore, notAfter)
	if clockErr != nil {
		return WriterTerminalEvidence{}, clockErr
	}
	if err != nil {
		return WriterTerminalEvidence{}, err
	}
	if len(secondCandidates) == 0 {
		return WriterTerminalEvidence{}, ErrNotObserved
	}
	if len(secondCandidates) != 1 {
		return WriterTerminalEvidence{}, ErrMultipleObserved
	}
	if err := validateWriterTerminalProviderInventory(secondCandidates[0]); err != nil {
		return WriterTerminalEvidence{}, err
	}
	if !equalWriterTerminalProviderFacts(firstCandidates[0], secondCandidates[0]) {
		return WriterTerminalEvidence{}, ErrQuarantined
	}
	evidence := terminalEvidenceFromProviderFacts(firstCandidates[0], secondCandidates[0], firstRead, secondRead)
	facts, factsErr := evidence.canonicalFactsBytes()
	if factsErr != nil {
		return WriterTerminalEvidence{}, factsErr
	}
	receipt := WriterTerminalObservationReceipt{
		Version: ProtocolVersion, OperationBodySHA256: o.operation, SourceTargetSHA256: o.source, ProviderConfigSHA256: o.config,
		OriginalProjectionSHA256: o.projection, MutationProviderSHA256: o.mutationProvider, ObserverIdentitySHA256: o.identity,
		FactsSHA256: HashBytes(facts), ReadOneAt: firstRead, ReadTwoAt: secondRead,
	}
	evidence.ProviderObservation = receipt
	return evidence, nil
}

var _ WriterLifecycleProvider = (*SyntheticWriterLifecycleProvider)(nil)
var _ writerTerminalFactsReader = (*SyntheticWriterTerminalFactsFacade)(nil)
var _ WriterTerminalObservationAdapter = (*SyntheticWriterTerminalObserver)(nil)
