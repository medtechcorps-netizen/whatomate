package model

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"fmt"
	"strings"
	"sync"
	"time"
)

// ObserverLifecycleDescriptor is the protected, selector-free identity of one
// observer lifecycle. The provider adapter is configured out of band; no
// lifecycle call accepts a target, endpoint, URL, key, or provider command.
type ObserverLifecycleDescriptor struct {
	RecoveryTargetSHA256           Digest
	ProviderConfigSHA256           Digest
	ObserverAdmissionRequestSHA256 Digest
	ForkWriterPublicationSHA256    Digest
	ContinuityBindingSHA256        Digest
}

func (d ObserverLifecycleDescriptor) validate() error {
	if d.RecoveryTargetSHA256.IsZero() || d.ProviderConfigSHA256.IsZero() ||
		d.RecoveryTargetSHA256 == d.ProviderConfigSHA256 || d.ObserverAdmissionRequestSHA256.IsZero() ||
		d.ForkWriterPublicationSHA256.IsZero() || d.ContinuityBindingSHA256.IsZero() {
		return ErrRoleIsolation
	}
	return nil
}

// ObserverLifecycleProvider is deliberately test-oracle-only in Gate A. Gate
// C must replace it with reviewed provider controllers and durable receipts.
// Apply and Observe accept only a closed lifecycle effect enum; all protected
// identifiers are fixed when the adapter is constructed.
type ObserverLifecycleProvider interface {
	IsTestOracleOnly() bool
	OperationBodySHA256() Digest
	RecoveryTargetSHA256() Digest
	ProviderConfigSHA256() Digest
	Apply(Effect) (Digest, error)
	Observe(Effect) ([]Digest, error)
}

// SyntheticObserverLifecycleProvider is a loopback-only deterministic
// provider oracle. It performs no network, provider, or data-plane operation.
type SyntheticObserverLifecycleProvider struct {
	mu              sync.Mutex
	operation       Digest
	target          Digest
	config          Digest
	applied         map[Effect]Digest
	applyCalls      map[Effect]int
	observeCalls    map[Effect]int
	observeFailures map[Effect]error
}

func NewSyntheticObserverLifecycleProvider(operation, target, config Digest) (*SyntheticObserverLifecycleProvider, error) {
	if operation.IsZero() || target.IsZero() || config.IsZero() || target == config {
		return nil, ErrInvalid
	}
	return &SyntheticObserverLifecycleProvider{
		operation: operation, target: target, config: config,
		applied: map[Effect]Digest{}, applyCalls: map[Effect]int{},
		observeCalls: map[Effect]int{}, observeFailures: map[Effect]error{},
	}, nil
}

func (*SyntheticObserverLifecycleProvider) IsTestOracleOnly() bool { return true }
func (p *SyntheticObserverLifecycleProvider) OperationBodySHA256() Digest {
	if p == nil {
		return Digest{}
	}
	return p.operation
}
func (p *SyntheticObserverLifecycleProvider) RecoveryTargetSHA256() Digest {
	if p == nil {
		return Digest{}
	}
	return p.target
}
func (p *SyntheticObserverLifecycleProvider) ProviderConfigSHA256() Digest {
	if p == nil {
		return Digest{}
	}
	return p.config
}

func observerLifecycleEffectAllowed(effect Effect) bool {
	return observerLifecycleEffectIndex(observerAdmissionEffects, effect) >= 0 ||
		observerLifecycleEffectIndex(observerCleanupEffects, effect) >= 0
}

func observerLifecycleProviderResult(operation, target, config Digest, effect Effect) Digest {
	return HashString("observer-lifecycle-provider-result/v2\x00" + operation.String() + "\x00" +
		target.String() + "\x00" + config.String() + "\x00" + string(effect))
}

func (p *SyntheticObserverLifecycleProvider) Apply(effect Effect) (Digest, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !observerLifecycleEffectAllowed(effect) {
		return Digest{}, ErrInvalid
	}
	if _, exists := p.applied[effect]; exists {
		return Digest{}, ErrQuarantined
	}
	p.applyCalls[effect]++
	result := observerLifecycleProviderResult(p.operation, p.target, p.config, effect)
	p.applied[effect] = result
	return result, nil
}

func (p *SyntheticObserverLifecycleProvider) Observe(effect Effect) ([]Digest, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !observerLifecycleEffectAllowed(effect) {
		return nil, ErrInvalid
	}
	p.observeCalls[effect]++
	if err := p.observeFailures[effect]; err != nil {
		return nil, err
	}
	result, exists := p.applied[effect]
	if !exists {
		return nil, nil
	}
	return []Digest{result}, nil
}

func (p *SyntheticObserverLifecycleProvider) setObserveFailure(effect Effect, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.observeFailures[effect] = err
}

func (p *SyntheticObserverLifecycleProvider) callCounts(effect Effect) (int, int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.applyCalls[effect], p.observeCalls[effect]
}

var observerAdmissionEffects = []Effect{
	EffectBrokerCreate,
	EffectBrokerUpdate,
	EffectTrustedSourceAdd,
	EffectBindingInstall,
	EffectCredentialInstall,
	EffectLeafIssue,
	EffectMTLSIssue,
}

var observerCleanupEffects = []Effect{
	EffectCapabilityRevoke,
	EffectLeafRevoke,
	EffectMTLSRevoke,
	EffectBindingRemove,
	EffectCredentialRemove,
	EffectTrustedSourceDel,
	EffectCleanupDelete,
}

type ObserverLifecycleBinding struct {
	Version                                                         string
	OperationBodySHA256, RecoveryTargetSHA256, ProviderConfigSHA256 Digest
	ObserverAuthoritySHA256, ObserverBrokerSHA256                   Digest
	ObserverLedgerSHA256, ObserverRootSHA256, ObserverStoreSHA256   Digest
	BoundarySHA256                                                  Digest
	ObserverAdmissionRequestSHA256, ForkWriterPublicationSHA256     Digest
	ContinuityBindingSHA256                                         Digest
}

func (b ObserverLifecycleBinding) payload() []byte {
	return []byte("observer-lifecycle-binding/v2\x00" + b.Version + "\x00" + b.OperationBodySHA256.String() + "\x00" +
		b.RecoveryTargetSHA256.String() + "\x00" + b.ProviderConfigSHA256.String() + "\x00" +
		b.ObserverAuthoritySHA256.String() + "\x00" + b.ObserverBrokerSHA256.String() + "\x00" +
		b.ObserverLedgerSHA256.String() + "\x00" + b.ObserverRootSHA256.String() + "\x00" +
		b.ObserverStoreSHA256.String() + "\x00" + b.BoundarySHA256.String() + "\x00" + b.ObserverAdmissionRequestSHA256.String() + "\x00" +
		b.ForkWriterPublicationSHA256.String() + "\x00" + b.ContinuityBindingSHA256.String())
}

func (b ObserverLifecycleBinding) Digest() Digest { return HashBytes(b.payload()) }

func expectedObserverLifecycleBinding(boundary *ObserverBoundary, base OperationBody, descriptor ObserverLifecycleDescriptor) (ObserverLifecycleBinding, error) {
	if boundary == nil || descriptor.validate() != nil {
		return ObserverLifecycleBinding{}, ErrRoleIsolation
	}
	body, err := base.Digest()
	if err != nil {
		return ObserverLifecycleBinding{}, err
	}
	binding := ObserverLifecycleBinding{
		Version: ProtocolVersion, OperationBodySHA256: body,
		RecoveryTargetSHA256:    descriptor.RecoveryTargetSHA256,
		ProviderConfigSHA256:    descriptor.ProviderConfigSHA256,
		ObserverAuthoritySHA256: boundary.authority.IdentityDigest(),
		ObserverBrokerSHA256:    boundary.broker.IdentityDigest(),
		ObserverLedgerSHA256:    boundary.ledger, ObserverRootSHA256: boundary.root,
		ObserverStoreSHA256: boundary.store, BoundarySHA256: boundary.closed,
		ObserverAdmissionRequestSHA256: descriptor.ObserverAdmissionRequestSHA256,
		ForkWriterPublicationSHA256:    descriptor.ForkWriterPublicationSHA256,
		ContinuityBindingSHA256:        descriptor.ContinuityBindingSHA256,
	}
	if binding.Digest().IsZero() {
		return ObserverLifecycleBinding{}, ErrInvalid
	}
	return binding, nil
}

func observerLifecycleEffectIndex(list []Effect, effect Effect) int {
	for index, candidate := range list {
		if candidate == effect {
			return index
		}
	}
	return -1
}

func observerLifecycleRequest(base OperationBody, binding ObserverLifecycleBinding, stage string, index int, effect Effect) MutationRequest {
	parameters := HashString("observer-lifecycle-step/v2\x00" + stage + "\x00" + fmt.Sprint(index) + "\x00" +
		binding.Digest().String() + "\x00" + string(effect))
	return derivedEffectRequest(base, binding.Digest(), effect, parameters)
}

func observerLifecycleStepTerminal(binding ObserverLifecycleBinding, stage string, index int, effect Effect) Digest {
	return HashString("observer-lifecycle-step-terminal/v2\x00" + binding.Digest().String() + "\x00" +
		stage + "\x00" + fmt.Sprint(index) + "\x00" + string(effect))
}

// ObserverLifecycle is a Gate-A state-machine model. It has no production
// constructor and accepts only the test-only ProtocolCrashOracle and provider.
type ObserverLifecycle struct {
	boundary *ObserverBoundary
	effects  *ProtocolCrashOracle
	provider ObserverLifecycleProvider
	base     OperationBody
	binding  ObserverLifecycleBinding
}

func NewObserverLifecycle(
	boundary *ObserverBoundary,
	effects *ProtocolCrashOracle,
	authority *ObserverAuthority,
	provider ObserverLifecycleProvider,
	base OperationBody,
	descriptor ObserverLifecycleDescriptor,
	admissionRequest ObserverAdmissionRequest,
) (*ObserverLifecycle, error) {
	if boundary == nil || effects == nil || authority == nil || provider == nil ||
		!effects.validInstance() || !effects.IsTestOracleOnly() || !provider.IsTestOracleOnly() {
		return nil, ErrProductionAdapter
	}
	binding, err := expectedObserverLifecycleBinding(boundary, base, descriptor)
	if err != nil {
		return nil, err
	}
	bodyDigest, err := base.Digest()
	if err != nil {
		return nil, err
	}
	public := authority.privateKey.Public().(ed25519.PublicKey)
	if validateObserverAdmissionRequest(boundary, admissionRequest) != nil || admissionRequest.Digest() != descriptor.ObserverAdmissionRequestSHA256 ||
		admissionRequest.outerEvidenceSHA256 != descriptor.ForkWriterPublicationSHA256 || admissionRequest.continuityBindingSHA256 != descriptor.ContinuityBindingSHA256 ||
		admissionRequest.recoveryTargetSHA256 != descriptor.RecoveryTargetSHA256 || admissionRequest.operationBodySHA256 != bodyDigest ||
		admissionRequest.operationID != base.OperationID || admissionRequest.generation != base.Generation || admissionRequest.phase != base.Phase ||
		!authority.unit.sameRuntime(boundary.authority) || !bytes.Equal(public, boundary.public) ||
		effects.identity != boundary.store || provider.OperationBodySHA256() != binding.OperationBodySHA256 ||
		provider.RecoveryTargetSHA256() != descriptor.RecoveryTargetSHA256 ||
		provider.ProviderConfigSHA256() != descriptor.ProviderConfigSHA256 {
		return nil, ErrRoleIsolation
	}
	if err := effects.bindSigner(ObserverAuthorityRole, authority.privateKey, boundary.ledger, boundary.root); err != nil {
		return nil, err
	}
	return &ObserverLifecycle{boundary: boundary, effects: effects, provider: provider, base: base, binding: binding}, nil
}

func (l *ObserverLifecycle) request(stage string, index int, effect Effect) MutationRequest {
	return observerLifecycleRequest(l.base, l.binding, stage, index, effect)
}

func (l *ObserverLifecycle) stageReady(effects []Effect, stage string) bool {
	for index, effect := range effects {
		if !l.effects.Completed(l.request(stage, index, effect)) {
			return false
		}
	}
	return true
}

func (l *ObserverLifecycle) AdmissionReady() bool {
	return l != nil && l.stageReady(observerAdmissionEffects, "admission")
}

func (l *ObserverLifecycle) CleanupReady() bool {
	return l != nil && l.stageReady(observerCleanupEffects, "cleanup")
}

// predecessorCompleteLocked is the auth-first variant used from a
// ProtocolCrashOracle semantic check while its durable ledger lock is held.
// It must not call the public Completed method, which would reacquire the same
// lock.
func (l *ObserverLifecycle) predecessorCompleteLocked(stage string, list []Effect, index int) bool {
	if index < 0 {
		return false
	}
	completed := func(request MutationRequest) bool {
		record, ok := l.effects.recordForRequestLocked(request)
		return ok && record.State == MutationCompleted
	}
	if stage == "cleanup" {
		for admissionIndex, admissionEffect := range observerAdmissionEffects {
			if !completed(l.request("admission", admissionIndex, admissionEffect)) {
				return false
			}
		}
	}
	if index == 0 {
		return true
	}
	return completed(l.request(stage, index-1, list[index-1]))
}

func (l *ObserverLifecycle) apply(ctx context.Context, stage string, list []Effect, effect Effect, auth AuthEnvelope, fault FaultMode) error {
	if l == nil {
		return ErrInvalid
	}
	index := observerLifecycleEffectIndex(list, effect)
	if index < 0 {
		return l.effects.rejectAfterAuth(ctx, auth, ErrInvalid)
	}
	request := l.request(stage, index, effect)
	terminal := observerLifecycleStepTerminal(l.binding, stage, index, effect)
	_, err := l.effects.authorizeAndConsumeChecked(ctx, request, auth, fault, terminal, func() error {
		if !l.predecessorCompleteLocked(stage, list, index) {
			return ErrForkNotAuthorized
		}
		return nil
	}, func() (Digest, error) {
		return l.provider.Apply(effect)
	})
	return err
}

func (l *ObserverLifecycle) observe(ctx context.Context, stage string, list []Effect, effect Effect, auth AuthEnvelope) error {
	if l == nil {
		return ErrInvalid
	}
	index := observerLifecycleEffectIndex(list, effect)
	if index < 0 {
		return l.effects.rejectAfterAuth(ctx, auth, ErrInvalid)
	}
	request := l.request(stage, index, effect)
	_, err := l.effects.reconcileStatusChecked(ctx, request, auth, func() error {
		if !l.predecessorCompleteLocked(stage, list, index) {
			return ErrForkNotAuthorized
		}
		return nil
	}, func() ([]Digest, error) {
		return l.provider.Observe(effect)
	})
	return err
}

func (l *ObserverLifecycle) ApplyAdmission(ctx context.Context, effect Effect, auth AuthEnvelope, fault FaultMode) error {
	return l.apply(ctx, "admission", observerAdmissionEffects, effect, auth, fault)
}

func (l *ObserverLifecycle) ObserveAdmission(ctx context.Context, effect Effect, auth AuthEnvelope) error {
	return l.observe(ctx, "admission", observerAdmissionEffects, effect, auth)
}

func (l *ObserverLifecycle) ApplyCleanup(ctx context.Context, effect Effect, auth AuthEnvelope, fault FaultMode) error {
	return l.apply(ctx, "cleanup", observerCleanupEffects, effect, auth, fault)
}

func (l *ObserverLifecycle) ObserveCleanup(ctx context.Context, effect Effect, auth AuthEnvelope) error {
	return l.observe(ctx, "cleanup", observerCleanupEffects, effect, auth)
}

type ObserverLifecycleStepProof struct {
	Stage         string
	Index         uint8
	Effect        Effect
	RequestSHA256 Digest
	ResultSHA256  Digest
	Completion    CompletionWitness
}

type ObserverLifecycleProof struct {
	Version   string
	Binding   ObserverLifecycleBinding
	Admission []ObserverLifecycleStepProof
	Cleanup   []ObserverLifecycleStepProof
}

func (p ObserverLifecycleProof) Digest() Digest {
	parts := []string{"observer-lifecycle-proof/v2", p.Version, p.Binding.Digest().String()}
	for _, group := range [][]ObserverLifecycleStepProof{p.Admission, p.Cleanup} {
		parts = append(parts, fmt.Sprint(len(group)))
		for _, step := range group {
			parts = append(parts, step.Stage, fmt.Sprint(step.Index), string(step.Effect), step.RequestSHA256.String(), step.ResultSHA256.String(), step.Completion.Digest().String())
		}
	}
	return HashString(strings.Join(parts, "\x00"))
}

func (l *ObserverLifecycle) stageProof(stage string, effects []Effect) ([]ObserverLifecycleStepProof, error) {
	proofs := make([]ObserverLifecycleStepProof, 0, len(effects))
	for index, effect := range effects {
		request := l.request(stage, index, effect)
		result, ok := l.effects.result(request)
		if !ok {
			return nil, ErrForkNotAuthorized
		}
		witness, ok := l.effects.completionWitness(request)
		if !ok {
			return nil, ErrForkNotAuthorized
		}
		requestDigest, err := request.Digest()
		if err != nil || result != observerLifecycleProviderResult(l.binding.OperationBodySHA256,
			l.binding.RecoveryTargetSHA256, l.binding.ProviderConfigSHA256, effect) {
			return nil, ErrQuarantined
		}
		if err := verifyCompletionWitness(l.boundary.public, ObserverAuthorityRole, l.boundary.ledger, l.boundary.root,
			l.boundary.store, request, result, observerLifecycleStepTerminal(l.binding, stage, index, effect), witness); err != nil {
			return nil, ErrQuarantined
		}
		proofs = append(proofs, ObserverLifecycleStepProof{
			Stage: stage, Index: uint8(index), Effect: effect, RequestSHA256: requestDigest,
			ResultSHA256: result, Completion: cloneCompletionWitness(witness),
		})
	}
	return proofs, nil
}

// PortableProof returns exact signed completion arrays. Cleanup is included
// only when requested and must then be completely terminal.
func (l *ObserverLifecycle) PortableProof(includeCleanup bool) (ObserverLifecycleProof, error) {
	if l == nil || !l.AdmissionReady() {
		return ObserverLifecycleProof{}, ErrForkNotAuthorized
	}
	admission, err := l.stageProof("admission", observerAdmissionEffects)
	if err != nil {
		return ObserverLifecycleProof{}, err
	}
	proof := ObserverLifecycleProof{Version: ProtocolVersion, Binding: l.binding, Admission: admission}
	if includeCleanup {
		if !l.CleanupReady() {
			return ObserverLifecycleProof{}, ErrForkNotAuthorized
		}
		proof.Cleanup, err = l.stageProof("cleanup", observerCleanupEffects)
		if err != nil {
			return ObserverLifecycleProof{}, err
		}
	}
	return proof, nil
}

func verifyObserverLifecycleSteps(
	boundary *ObserverBoundary,
	base OperationBody,
	binding ObserverLifecycleBinding,
	stage string,
	effects []Effect,
	proofs []ObserverLifecycleStepProof,
	lastSequence *uint64,
	lastCompleted *time.Time,
) error {
	if len(proofs) != len(effects) {
		return ErrForkNotAuthorized
	}
	for index, effect := range effects {
		proof := proofs[index]
		request := observerLifecycleRequest(base, binding, stage, index, effect)
		requestDigest, err := request.Digest()
		result := observerLifecycleProviderResult(binding.OperationBodySHA256,
			binding.RecoveryTargetSHA256, binding.ProviderConfigSHA256, effect)
		if err != nil || proof.Stage != stage || int(proof.Index) != index || proof.Effect != effect ||
			proof.RequestSHA256 != requestDigest || proof.ResultSHA256 != result ||
			proof.Completion.Sequence <= *lastSequence ||
			(!lastCompleted.IsZero() && proof.Completion.CompletedAt.Before(*lastCompleted)) {
			return ErrForkNotAuthorized
		}
		if err := verifyCompletionWitness(boundary.public, ObserverAuthorityRole, boundary.ledger, boundary.root,
			boundary.store, request, result, observerLifecycleStepTerminal(binding, stage, index, effect), proof.Completion); err != nil {
			return ErrForkNotAuthorized
		}
		*lastSequence = proof.Completion.Sequence
		*lastCompleted = proof.Completion.CompletedAt
	}
	return nil
}

func VerifyObserverLifecycleProof(
	boundary *ObserverBoundary,
	base OperationBody,
	descriptor ObserverLifecycleDescriptor,
	proof ObserverLifecycleProof,
	requireCleanup bool,
) error {
	expected, err := expectedObserverLifecycleBinding(boundary, base, descriptor)
	if err != nil || proof.Version != ProtocolVersion || proof.Binding != expected {
		return ErrForkNotAuthorized
	}
	if requireCleanup {
		if len(proof.Cleanup) != len(observerCleanupEffects) {
			return ErrForkNotAuthorized
		}
	} else if len(proof.Cleanup) != 0 {
		return ErrForkNotAuthorized
	}
	var sequence uint64
	var completed time.Time
	if err := verifyObserverLifecycleSteps(boundary, base, expected, "admission", observerAdmissionEffects,
		proof.Admission, &sequence, &completed); err != nil {
		return err
	}
	if requireCleanup {
		if err := verifyObserverLifecycleSteps(boundary, base, expected, "cleanup", observerCleanupEffects,
			proof.Cleanup, &sequence, &completed); err != nil {
			return err
		}
	}
	return nil
}
