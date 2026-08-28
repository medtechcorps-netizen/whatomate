package model

import (
	"context"
	"crypto/ed25519"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"
)

type observerLifecycleFixture struct {
	protocol   *protocolFixture
	boundary   *ObserverBoundary
	authority  *ObserverAuthority
	oracle     *ProtocolCrashOracle
	provider   *SyntheticObserverLifecycleProvider
	request    ObserverAdmissionRequest
	descriptor ObserverLifecycleDescriptor
	lifecycle  *ObserverLifecycle
}

func newObserverLifecycleFixture(t *testing.T) *observerLifecycleFixture {
	t.Helper()
	f := &observerLifecycleFixture{protocol: newProtocolFixture(t)}
	var err error
	f.boundary, err = f.protocol.boundary.ObserverView()
	if err != nil {
		t.Fatal(err)
	}
	f.authority, err = NewObserverAuthority(f.protocol.observerAuthority, f.protocol.observerPrivate)
	if err != nil {
		t.Fatal(err)
	}
	f.oracle, err = newBoundProtocolCrashOracle(func() time.Time { return protocolNow }, f.boundary.store)
	if err != nil {
		t.Fatal(err)
	}
	bodyDigest, err := f.protocol.body.Digest()
	if err != nil {
		t.Fatal(err)
	}
	recoveryTarget := HashString("synthetic-observer-lifecycle-recovery-target")
	terminalBinding := HashString("synthetic-observer-lifecycle-writer-terminal")
	forkRequest := HashString("synthetic-observer-lifecycle-fork-request")
	forkResult := HashString("synthetic-observer-lifecycle-fork-result")
	writerPublication := HashString("synthetic-observer-lifecycle-writer-publication")
	continuity := HashString("observer-admission-continuity/v2\x00" + writerPublication.String() + "\x00" +
		bodyDigest.String() + "\x00" + forkCompletionProof(terminalBinding, forkRequest, forkResult).String() + "\x00" + recoveryTarget.String())
	f.request = ObserverAdmissionRequest{
		version: ProtocolVersion, operationID: f.protocol.body.OperationID, phase: f.protocol.body.Phase,
		generation: f.protocol.body.Generation, operationBodySHA256: bodyDigest,
		projectionSHA256: HashString("synthetic-observer-lifecycle-projection"), terminalBindingSHA256: terminalBinding,
		forkRequestSHA256: forkRequest, forkResultSHA256: forkResult,
		forkProofSHA256:      forkCompletionProof(terminalBinding, forkRequest, forkResult),
		recoveryTargetSHA256: recoveryTarget, forkProviderSHA256: HashString("synthetic-observer-lifecycle-fork-provider"),
		observerAuthoritySHA256: f.boundary.authority.IdentityDigest(), observerBrokerSHA256: f.boundary.broker.IdentityDigest(),
		observerLedgerSHA256: f.boundary.ledger, observerRootSHA256: f.boundary.root,
		observerStoreSHA256: f.boundary.store, boundarySHA256: f.boundary.closed,
		outerEvidenceSHA256: writerPublication, continuityBindingSHA256: continuity,
		writerPublicationIssuedAt: protocolNow.Add(-time.Minute), writerPublicationNotAfter: protocolNow.Add(10 * time.Minute),
	}
	if err := validateObserverAdmissionRequest(f.boundary, f.request); err != nil {
		t.Fatalf("synthetic observer admission request: %v", err)
	}
	f.descriptor = ObserverLifecycleDescriptor{
		RecoveryTargetSHA256:           recoveryTarget,
		ProviderConfigSHA256:           HashString("synthetic-observer-lifecycle-provider-config"),
		ObserverAdmissionRequestSHA256: f.request.Digest(),
		ForkWriterPublicationSHA256:    f.request.outerEvidenceSHA256,
		ContinuityBindingSHA256:        f.request.continuityBindingSHA256,
	}
	f.provider, err = NewSyntheticObserverLifecycleProvider(
		bodyDigest, f.descriptor.RecoveryTargetSHA256, f.descriptor.ProviderConfigSHA256,
	)
	if err != nil {
		t.Fatal(err)
	}
	f.lifecycle, err = NewObserverLifecycle(
		f.boundary, f.oracle, f.authority, f.provider, f.protocol.body, f.descriptor, f.request,
	)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *observerLifecycleFixture) issued(stage string, list []Effect, effect Effect, call CallKind) AuthEnvelope {
	index := observerLifecycleEffectIndex(list, effect)
	if index < 0 {
		panic("invalid synthetic observer lifecycle effect")
	}
	return f.protocol.auth.issued(f.oracle, f.lifecycle.request(stage, index, effect), call)
}

func completeObserverLifecycleStage(t *testing.T, fixture *observerLifecycleFixture, cleanup bool) {
	t.Helper()
	effects := observerAdmissionEffects
	apply := fixture.lifecycle.ApplyAdmission
	if cleanup {
		effects = observerCleanupEffects
		apply = fixture.lifecycle.ApplyCleanup
	}
	stage := "admission"
	if cleanup {
		stage = "cleanup"
	}
	for _, effect := range effects {
		if err := apply(context.Background(), effect, fixture.issued(stage, effects, effect, MutationCall), FaultNone); err != nil {
			t.Fatalf("complete %s: %v", effect, err)
		}
	}
}

func cloneObserverLifecycleProof(proof ObserverLifecycleProof) ObserverLifecycleProof {
	cloneSteps := func(source []ObserverLifecycleStepProof) []ObserverLifecycleStepProof {
		result := append([]ObserverLifecycleStepProof(nil), source...)
		for index := range result {
			result[index].Completion = cloneCompletionWitness(result[index].Completion)
		}
		return result
	}
	proof.Admission = cloneSteps(proof.Admission)
	proof.Cleanup = cloneSteps(proof.Cleanup)
	return proof
}

func TestObserverLifecycleOrdersEffectsAndReconcilesIssueOnce(t *testing.T) {
	f := newObserverLifecycleFixture(t)
	ctx := context.Background()

	if err := f.lifecycle.ApplyCleanup(ctx, EffectCapabilityRevoke, f.issued("cleanup", observerCleanupEffects, EffectCapabilityRevoke, MutationCall), FaultNone); !errors.Is(err, ErrForkNotAuthorized) {
		t.Fatalf("cleanup before admission = %v", err)
	}
	if err := f.lifecycle.ApplyAdmission(ctx, EffectBrokerUpdate, f.issued("admission", observerAdmissionEffects, EffectBrokerUpdate, MutationCall), FaultNone); !errors.Is(err, ErrForkNotAuthorized) {
		t.Fatalf("out-of-order admission = %v", err)
	}
	if applied, observed := f.provider.callCounts(EffectBrokerUpdate); applied != 0 || observed != 0 {
		t.Fatalf("out-of-order call reached provider: apply=%d observe=%d", applied, observed)
	}

	if err := f.lifecycle.ApplyAdmission(ctx, EffectBrokerCreate, f.issued("admission", observerAdmissionEffects, EffectBrokerCreate, MutationCall), FaultHTTP408AfterEffect); !errors.Is(err, ErrReconcileRequired) {
		t.Fatalf("ambiguous create = %v", err)
	}
	if err := f.lifecycle.ApplyAdmission(ctx, EffectBrokerCreate, f.issued("admission", observerAdmissionEffects, EffectBrokerCreate, MutationCall), FaultNone); !errors.Is(err, ErrReconcileRequired) {
		t.Fatalf("mutation retry = %v", err)
	}
	sentinel := errors.New("synthetic status transport failure")
	f.provider.setObserveFailure(EffectBrokerCreate, sentinel)
	if err := f.lifecycle.ObserveAdmission(ctx, EffectBrokerCreate, f.issued("admission", observerAdmissionEffects, EffectBrokerCreate, StatusCall)); !errors.Is(err, sentinel) {
		t.Fatalf("failed status observation = %v", err)
	}
	f.provider.setObserveFailure(EffectBrokerCreate, nil)
	if err := f.lifecycle.ObserveAdmission(ctx, EffectBrokerCreate, f.issued("admission", observerAdmissionEffects, EffectBrokerCreate, StatusCall)); err != nil {
		t.Fatalf("status reconciliation: %v", err)
	}
	if applied, observed := f.provider.callCounts(EffectBrokerCreate); applied != 1 || observed != 2 {
		t.Fatalf("issue-once counts: apply=%d observe=%d", applied, observed)
	}
	request := f.lifecycle.request("admission", 0, EffectBrokerCreate)
	if got := f.oracle.IssueCount(request); got != 1 {
		t.Fatalf("durable issue count = %d", got)
	}

	for _, effect := range observerAdmissionEffects[1:] {
		if err := f.lifecycle.ApplyAdmission(ctx, effect, f.issued("admission", observerAdmissionEffects, effect, MutationCall), FaultNone); err != nil {
			t.Fatalf("admission %s: %v", effect, err)
		}
	}
	if !f.lifecycle.AdmissionReady() || f.lifecycle.CleanupReady() {
		t.Fatal("unexpected lifecycle readiness")
	}

	admissionProof, err := f.lifecycle.PortableProof(false)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyObserverLifecycleProof(f.boundary, f.protocol.body, f.descriptor, admissionProof, false); err != nil {
		t.Fatalf("verify admission proof: %v", err)
	}
	if admissionProof.Binding.ObserverAdmissionRequestSHA256 != f.request.Digest() ||
		admissionProof.Binding.ForkWriterPublicationSHA256 != f.request.outerEvidenceSHA256 ||
		admissionProof.Binding.ContinuityBindingSHA256 != f.request.continuityBindingSHA256 {
		t.Fatal("portable proof lost exact admission/publication continuity bindings")
	}
	if !reflect.DeepEqual(observerLifecycleProofEffects(admissionProof.Admission), observerAdmissionEffects) || len(admissionProof.Cleanup) != 0 {
		t.Fatal("portable admission array differs from exact order")
	}

	completeObserverLifecycleStage(t, f, true)
	if !f.lifecycle.CleanupReady() {
		t.Fatal("cleanup not ready")
	}
	fullProof, err := f.lifecycle.PortableProof(true)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyObserverLifecycleProof(f.boundary, f.protocol.body, f.descriptor, fullProof, true); err != nil {
		t.Fatalf("verify full proof: %v", err)
	}
	if !reflect.DeepEqual(observerLifecycleProofEffects(fullProof.Cleanup), observerCleanupEffects) {
		t.Fatal("portable cleanup array differs from exact order")
	}
	for _, effect := range append(append([]Effect(nil), observerAdmissionEffects...), observerCleanupEffects...) {
		applied, _ := f.provider.callCounts(effect)
		if applied != 1 {
			t.Fatalf("provider issued %s %d times", effect, applied)
		}
	}
}

func TestObserverLifecycleSemanticFailureBurnsAuthentication(t *testing.T) {
	f := newObserverLifecycleFixture(t)
	auth := f.issued("admission", observerAdmissionEffects, EffectBrokerUpdate, MutationCall)
	if err := f.lifecycle.ApplyAdmission(context.Background(), EffectBrokerUpdate, auth, FaultNone); !errors.Is(err, ErrForkNotAuthorized) {
		t.Fatalf("out-of-order admission effect: %v", err)
	}
	if err := f.lifecycle.ApplyAdmission(context.Background(), EffectBrokerCreate, auth, FaultNone); !errors.Is(err, ErrAuthReplay) {
		t.Fatalf("semantic-error authentication was reusable: %v", err)
	}
	createApply, _ := f.provider.callCounts(EffectBrokerCreate)
	updateApply, _ := f.provider.callCounts(EffectBrokerUpdate)
	if createApply != 0 || updateApply != 0 {
		t.Fatalf("semantic rejection reached provider: create=%d update=%d", createApply, updateApply)
	}
}

func observerLifecycleProofEffects(proofs []ObserverLifecycleStepProof) []Effect {
	result := make([]Effect, len(proofs))
	for index := range proofs {
		result[index] = proofs[index].Effect
	}
	return result
}

func TestObserverLifecycleConcurrentReplayDoesNotRepeatProviderMutation(t *testing.T) {
	f := newObserverLifecycleFixture(t)
	const callers = 12
	errorsSeen := make(chan error, callers)
	var group sync.WaitGroup
	for index := 0; index < callers; index++ {
		group.Add(1)
		go func() {
			defer group.Done()
			errorsSeen <- f.lifecycle.ApplyAdmission(
				context.Background(), EffectBrokerCreate, f.issued("admission", observerAdmissionEffects, EffectBrokerCreate, MutationCall), FaultNone,
			)
		}()
	}
	group.Wait()
	close(errorsSeen)
	for err := range errorsSeen {
		if err != nil {
			t.Fatalf("concurrent idempotent replay: %v", err)
		}
	}
	if applied, _ := f.provider.callCounts(EffectBrokerCreate); applied != 1 {
		t.Fatalf("concurrent provider apply count = %d", applied)
	}
	if got := f.oracle.IssueCount(f.lifecycle.request("admission", 0, EffectBrokerCreate)); got != 1 {
		t.Fatalf("concurrent durable issue count = %d", got)
	}
}

func TestObserverLifecycleBeforeEffectAmbiguityIsStatusOnly(t *testing.T) {
	f := newObserverLifecycleFixture(t)
	ctx := context.Background()
	if err := f.lifecycle.ApplyAdmission(ctx, EffectBrokerCreate, f.issued("admission", observerAdmissionEffects, EffectBrokerCreate, MutationCall), FaultHTTP408BeforeEffect); !errors.Is(err, ErrReconcileRequired) {
		t.Fatalf("before-effect ambiguity = %v", err)
	}
	if err := f.lifecycle.ApplyAdmission(ctx, EffectBrokerCreate, f.issued("admission", observerAdmissionEffects, EffectBrokerCreate, MutationCall), FaultNone); !errors.Is(err, ErrReconcileRequired) {
		t.Fatalf("forbidden retry = %v", err)
	}
	if applied, _ := f.provider.callCounts(EffectBrokerCreate); applied != 0 {
		t.Fatalf("provider mutation issued after before-effect ambiguity: %d", applied)
	}
	if err := f.lifecycle.ObserveAdmission(ctx, EffectBrokerCreate, f.issued("admission", observerAdmissionEffects, EffectBrokerCreate, StatusCall)); !errors.Is(err, ErrNotObserved) {
		t.Fatalf("status-only absence = %v", err)
	}
	if err := f.lifecycle.ApplyAdmission(ctx, EffectBrokerCreate, f.issued("admission", observerAdmissionEffects, EffectBrokerCreate, MutationCall), FaultNone); !errors.Is(err, ErrQuarantined) {
		t.Fatalf("quarantined retry = %v", err)
	}
}

func TestObserverLifecycleProofRejectsBindingOrderAndSignatureTamper(t *testing.T) {
	f := newObserverLifecycleFixture(t)
	completeObserverLifecycleStage(t, f, false)
	completeObserverLifecycleStage(t, f, true)
	proof, err := f.lifecycle.PortableProof(true)
	if err != nil {
		t.Fatal(err)
	}

	mutations := map[string]func(*ObserverLifecycleProof){
		"binding": func(candidate *ObserverLifecycleProof) {
			candidate.Binding.ProviderConfigSHA256 = HashString("synthetic-drifted-provider-config")
		},
		"admission-request-binding": func(candidate *ObserverLifecycleProof) {
			candidate.Binding.ObserverAdmissionRequestSHA256 = HashString("synthetic-drifted-admission-request")
		},
		"writer-publication-binding": func(candidate *ObserverLifecycleProof) {
			candidate.Binding.ForkWriterPublicationSHA256 = HashString("synthetic-drifted-writer-publication")
		},
		"continuity-binding": func(candidate *ObserverLifecycleProof) {
			candidate.Binding.ContinuityBindingSHA256 = HashString("synthetic-drifted-continuity-binding")
		},
		"admission-order": func(candidate *ObserverLifecycleProof) {
			candidate.Admission[0], candidate.Admission[1] = candidate.Admission[1], candidate.Admission[0]
		},
		"cleanup-order": func(candidate *ObserverLifecycleProof) {
			candidate.Cleanup[0], candidate.Cleanup[1] = candidate.Cleanup[1], candidate.Cleanup[0]
		},
		"request": func(candidate *ObserverLifecycleProof) {
			candidate.Admission[0].RequestSHA256 = HashString("synthetic-drifted-request")
		},
		"result": func(candidate *ObserverLifecycleProof) {
			candidate.Cleanup[0].ResultSHA256 = HashString("synthetic-drifted-result")
		},
		"signature": func(candidate *ObserverLifecycleProof) {
			candidate.Admission[0].Completion.Signature[0] ^= 0x80
		},
		"sequence": func(candidate *ObserverLifecycleProof) {
			candidate.Cleanup[0].Completion.Sequence = candidate.Admission[len(candidate.Admission)-1].Completion.Sequence
		},
		"signed-backward-chronology": func(candidate *ObserverLifecycleProof) {
			candidate.Admission[1].Completion.CompletedAt = candidate.Admission[0].Completion.CompletedAt.Add(-time.Nanosecond)
			candidate.Admission[1].Completion.Signature = ed25519.Sign(
				f.protocol.observerPrivate, candidate.Admission[1].Completion.payload(),
			)
		},
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			candidate := cloneObserverLifecycleProof(proof)
			mutate(&candidate)
			if err := VerifyObserverLifecycleProof(f.boundary, f.protocol.body, f.descriptor, candidate, true); !errors.Is(err, ErrForkNotAuthorized) {
				t.Fatalf("tampered proof = %v", err)
			}
		})
	}

	admissionOnly := cloneObserverLifecycleProof(proof)
	admissionOnly.Cleanup = nil
	if err := VerifyObserverLifecycleProof(f.boundary, f.protocol.body, f.descriptor, admissionOnly, true); !errors.Is(err, ErrForkNotAuthorized) {
		t.Fatalf("missing required cleanup = %v", err)
	}
	if err := VerifyObserverLifecycleProof(f.boundary, f.protocol.body, f.descriptor, proof, false); !errors.Is(err, ErrForkNotAuthorized) {
		t.Fatalf("unexpected cleanup accepted = %v", err)
	}
	wrongDescriptor := f.descriptor
	wrongDescriptor.RecoveryTargetSHA256 = HashString("synthetic-other-recovery-target")
	if err := VerifyObserverLifecycleProof(f.boundary, f.protocol.body, wrongDescriptor, proof, true); !errors.Is(err, ErrForkNotAuthorized) {
		t.Fatalf("wrong descriptor accepted = %v", err)
	}
}

type nonTestObserverLifecycleProvider struct{ ObserverLifecycleProvider }

func (nonTestObserverLifecycleProvider) IsTestOracleOnly() bool { return false }

func TestObserverLifecycleConstructorFailsClosed(t *testing.T) {
	f := newObserverLifecycleFixture(t)
	bodyDigest, err := f.protocol.body.Digest()
	if err != nil {
		t.Fatal(err)
	}
	wrongProvider, err := NewSyntheticObserverLifecycleProvider(
		bodyDigest, f.descriptor.RecoveryTargetSHA256, HashString("synthetic-wrong-provider-config"),
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewObserverLifecycle(f.boundary, f.oracle, f.authority, wrongProvider, f.protocol.body, f.descriptor, f.request); !errors.Is(err, ErrRoleIsolation) {
		t.Fatalf("provider binding mismatch = %v", err)
	}
	wrapped := nonTestObserverLifecycleProvider{ObserverLifecycleProvider: f.provider}
	if _, err := NewObserverLifecycle(f.boundary, f.oracle, f.authority, wrapped, f.protocol.body, f.descriptor, f.request); !errors.Is(err, ErrProductionAdapter) {
		t.Fatalf("production-like provider accepted = %v", err)
	}
	unbound, err := NewProtocolCrashOracle(func() time.Time { return protocolNow })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewObserverLifecycle(f.boundary, unbound, f.authority, f.provider, f.protocol.body, f.descriptor, f.request); !errors.Is(err, ErrRoleIsolation) {
		t.Fatalf("unbound durable oracle accepted = %v", err)
	}
	invalid := f.descriptor
	invalid.ProviderConfigSHA256 = invalid.RecoveryTargetSHA256
	if _, err := NewObserverLifecycle(f.boundary, f.oracle, f.authority, f.provider, f.protocol.body, invalid, f.request); !errors.Is(err, ErrRoleIsolation) {
		t.Fatalf("invalid descriptor accepted = %v", err)
	}

	t.Run("exact-admission-and-publication-continuity", func(t *testing.T) {
		mutations := map[string]func(*ObserverLifecycleDescriptor, *ObserverAdmissionRequest){
			"request-digest": func(_ *ObserverLifecycleDescriptor, request *ObserverAdmissionRequest) {
				request.projectionSHA256 = HashString("synthetic-drifted-admission-projection")
			},
			"descriptor-request-hash": func(descriptor *ObserverLifecycleDescriptor, _ *ObserverAdmissionRequest) {
				descriptor.ObserverAdmissionRequestSHA256 = HashString("synthetic-drifted-admission-request")
			},
			"writer-publication": func(descriptor *ObserverLifecycleDescriptor, _ *ObserverAdmissionRequest) {
				descriptor.ForkWriterPublicationSHA256 = HashString("synthetic-drifted-writer-publication")
			},
			"continuity": func(descriptor *ObserverLifecycleDescriptor, _ *ObserverAdmissionRequest) {
				descriptor.ContinuityBindingSHA256 = HashString("synthetic-drifted-continuity-binding")
			},
			"request-continuity": func(_ *ObserverLifecycleDescriptor, request *ObserverAdmissionRequest) {
				request.continuityBindingSHA256 = HashString("synthetic-drifted-request-continuity")
			},
			"invalid-publication-window": func(descriptor *ObserverLifecycleDescriptor, request *ObserverAdmissionRequest) {
				request.writerPublicationNotAfter = request.writerPublicationIssuedAt
				descriptor.ObserverAdmissionRequestSHA256 = request.Digest()
			},
		}
		for name, mutate := range mutations {
			t.Run(name, func(t *testing.T) {
				descriptor := f.descriptor
				request := f.request
				mutate(&descriptor, &request)
				if _, err := NewObserverLifecycle(
					f.boundary, f.oracle, f.authority, f.provider, f.protocol.body, descriptor, request,
				); !errors.Is(err, ErrRoleIsolation) {
					t.Fatalf("drift accepted: %v", err)
				}
			})
		}

		t.Run("internally-valid-other-operation", func(t *testing.T) {
			descriptor := f.descriptor
			request := f.request
			request.operationBodySHA256 = HashString("synthetic-other-operation-body")
			request.continuityBindingSHA256 = HashString("observer-admission-continuity/v2\x00" +
				request.outerEvidenceSHA256.String() + "\x00" + request.operationBodySHA256.String() + "\x00" +
				request.forkProofSHA256.String() + "\x00" + request.recoveryTargetSHA256.String())
			descriptor.ObserverAdmissionRequestSHA256 = request.Digest()
			descriptor.ContinuityBindingSHA256 = request.continuityBindingSHA256
			if err := validateObserverAdmissionRequest(f.boundary, request); err != nil {
				t.Fatalf("drifted request was not internally valid: %v", err)
			}
			if _, err := NewObserverLifecycle(
				f.boundary, f.oracle, f.authority, f.provider, f.protocol.body, descriptor, request,
			); !errors.Is(err, ErrRoleIsolation) {
				t.Fatalf("other operation admitted: %v", err)
			}
		})
	})
}
