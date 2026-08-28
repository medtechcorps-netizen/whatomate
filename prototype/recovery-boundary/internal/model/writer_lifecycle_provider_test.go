package model

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func lifecycleForCommittedWriter(t *testing.T) (*protocolFixture, *WriterProtocol, *WriterLifecycle, *SyntheticWriterLifecycleProvider, *SyntheticWriterTerminalObserver) {
	t.Helper()
	f := newProtocolFixture(t)
	writer := f.writer(t, markerEntropy())
	if _, err := writer.GenerateCommitAndReadback(context.Background(), f.body, markerAuth(f, writer, f.body, MutationCall), FaultNone); err != nil {
		t.Fatal(err)
	}
	lifecycle, provider, observer := newWriterLifecycleForTest(t, f, writer, f.body, originalProjection())
	return f, writer, lifecycle, provider, observer
}

func applyLifecycleEffect(t *testing.T, lifecycle *WriterLifecycle, f *protocolFixture, effect Effect, fault FaultMode) error {
	t.Helper()
	switch effect {
	case EffectCapabilityRevoke:
		return lifecycle.RecordRevocation(context.Background(), RevokeCapability, writerLifecycleAuth(f, lifecycle, effect, MutationCall), fault)
	case EffectLeafRevoke:
		return lifecycle.RecordRevocation(context.Background(), RevokeLeaf, writerLifecycleAuth(f, lifecycle, effect, MutationCall), fault)
	case EffectMTLSRevoke:
		return lifecycle.RecordRevocation(context.Background(), RevokeMTLS, writerLifecycleAuth(f, lifecycle, effect, MutationCall), fault)
	case EffectWrappingKeyRevoke:
		if err := lifecycle.RecordWrappingKeySealerRevocation(context.Background(), wrappingKeySealerAuth(f, lifecycle, MutationCall), FaultNone); err != nil {
			return err
		}
		return lifecycle.RecordRevocation(context.Background(), RevokeWrappingKey, writerLifecycleAuth(f, lifecycle, effect, MutationCall), fault)
	case EffectBindingRemove:
		return lifecycle.RecordRevocation(context.Background(), RemoveBinding, writerLifecycleAuth(f, lifecycle, effect, MutationCall), fault)
	case EffectCredentialRemove:
		return lifecycle.RecordRevocation(context.Background(), RemoveCredential, writerLifecycleAuth(f, lifecycle, effect, MutationCall), fault)
	case EffectFullRedeploy:
		return lifecycle.RecordFullRedeploy(context.Background(), writerLifecycleAuth(f, lifecycle, effect, MutationCall), fault)
	case EffectTrustedSourceDel:
		return lifecycle.RecordFirewallRestored(context.Background(), writerLifecycleAuth(f, lifecycle, effect, MutationCall), fault)
	case EffectBrokerDelete:
		result := DeleteDefinitiveSuccess
		switch fault {
		case FaultHTTP408AfterEffect:
			result = DeleteAmbiguous408
		case FaultEOFAfterEffect:
			result = DeleteAmbiguousEOF
		case Fault5xxAfterEffect:
			result = DeleteAmbiguous5xx
		case FaultNone:
		default:
			return ErrInvalid
		}
		return lifecycle.RequestDelete(context.Background(), result, writerLifecycleAuth(f, lifecycle, effect, MutationCall))
	default:
		return ErrInvalid
	}
}

func reconcileLifecycleEffect(t *testing.T, lifecycle *WriterLifecycle, f *protocolFixture, effect Effect) error {
	t.Helper()
	switch effect {
	case EffectCapabilityRevoke:
		return lifecycle.ReconcileRevocation(context.Background(), RevokeCapability, writerLifecycleAuth(f, lifecycle, effect, StatusCall))
	case EffectLeafRevoke:
		return lifecycle.ReconcileRevocation(context.Background(), RevokeLeaf, writerLifecycleAuth(f, lifecycle, effect, StatusCall))
	case EffectMTLSRevoke:
		return lifecycle.ReconcileRevocation(context.Background(), RevokeMTLS, writerLifecycleAuth(f, lifecycle, effect, StatusCall))
	case EffectWrappingKeyRevoke:
		return lifecycle.ReconcileRevocation(context.Background(), RevokeWrappingKey, writerLifecycleAuth(f, lifecycle, effect, StatusCall))
	case EffectBindingRemove:
		return lifecycle.ReconcileRevocation(context.Background(), RemoveBinding, writerLifecycleAuth(f, lifecycle, effect, StatusCall))
	case EffectCredentialRemove:
		return lifecycle.ReconcileRevocation(context.Background(), RemoveCredential, writerLifecycleAuth(f, lifecycle, effect, StatusCall))
	case EffectFullRedeploy:
		return lifecycle.ReconcileFullRedeploy(context.Background(), writerLifecycleAuth(f, lifecycle, effect, StatusCall))
	case EffectTrustedSourceDel:
		return lifecycle.ReconcileFirewallRestored(context.Background(), writerLifecycleAuth(f, lifecycle, effect, StatusCall))
	case EffectBrokerDelete:
		return lifecycle.ReconcileDelete(context.Background(), writerLifecycleAuth(f, lifecycle, effect, StatusCall))
	default:
		return ErrInvalid
	}
}

func completeWriterLifecycle(t *testing.T, lifecycle *WriterLifecycle, f *protocolFixture) {
	t.Helper()
	for _, effect := range append(append([]Effect(nil), writerTeardownEffects...), EffectBrokerDelete) {
		if err := applyLifecycleEffect(t, lifecycle, f, effect, FaultNone); err != nil {
			t.Fatalf("complete %s: %v", effect, err)
		}
	}
	elapseWriterOldInstanceGrace(f)
}

func TestWriterLifecycleEverySideEffectUsesIssueOnceStatusOnlyReconciliation(t *testing.T) {
	f, _, lifecycle, provider, _ := lifecycleForCommittedWriter(t)
	faults := []FaultMode{FaultHTTP408AfterEffect, FaultEOFAfterEffect, Fault5xxAfterEffect}
	effects := append(append([]Effect(nil), writerTeardownEffects...), EffectBrokerDelete)
	for index, effect := range effects {
		fault := faults[index%len(faults)]
		if err := applyLifecycleEffect(t, lifecycle, f, effect, fault); !errors.Is(err, ErrReconcileRequired) {
			t.Fatalf("%s ambiguity did not require status: %v", effect, err)
		}
		if err := reconcileLifecycleEffect(t, lifecycle, f, effect); err != nil {
			t.Fatalf("%s status reconciliation: %v", effect, err)
		}
		applyCalls, observeCalls := provider.callCounts(effect)
		if applyCalls != 1 || observeCalls != 1 || f.effects.IssueCount(lifecycle.request(effect)) != 1 {
			t.Fatalf("%s repeated mutation or skipped status: apply=%d observe=%d issue=%d", effect, applyCalls, observeCalls, f.effects.IssueCount(lifecycle.request(effect)))
		}
		if err := applyLifecycleEffect(t, lifecycle, f, effect, FaultNone); err != nil {
			t.Fatalf("%s idempotent completed request: %v", effect, err)
		}
		applyCalls, _ = provider.callCounts(effect)
		if applyCalls != 1 {
			t.Fatalf("%s hidden retry reached provider: %d", effect, applyCalls)
		}
	}
}

func TestWriterLifecycleProviderAmbiguityAndTransientStatusNeverRepeatMutation(t *testing.T) {
	f, _, lifecycle, provider, _ := lifecycleForCommittedWriter(t)
	provider.setApplyAmbiguity(EffectCapabilityRevoke, ProviderAmbiguousHTTP408)
	if err := lifecycle.RecordRevocation(context.Background(), RevokeCapability, writerLifecycleAuth(f, lifecycle, EffectCapabilityRevoke, MutationCall), FaultNone); !errors.Is(err, ErrReconcileRequired) {
		t.Fatalf("typed provider ambiguity: %v", err)
	}
	transient := errors.New("synthetic transient GET failure")
	provider.setObserveFailure(EffectCapabilityRevoke, transient)
	if err := lifecycle.ReconcileRevocation(context.Background(), RevokeCapability, writerLifecycleAuth(f, lifecycle, EffectCapabilityRevoke, StatusCall)); !errors.Is(err, transient) {
		t.Fatalf("transient observation result: %v", err)
	}
	provider.setObserveFailure(EffectCapabilityRevoke, nil)
	if err := lifecycle.ReconcileRevocation(context.Background(), RevokeCapability, writerLifecycleAuth(f, lifecycle, EffectCapabilityRevoke, StatusCall)); err != nil {
		t.Fatal(err)
	}
	applyCalls, observeCalls := provider.callCounts(EffectCapabilityRevoke)
	if applyCalls != 1 || observeCalls != 2 || !f.effects.CapabilityRevoked(lifecycle.capability) {
		t.Fatalf("provider ambiguity repeated or lost revocation: apply=%d observe=%d revoked=%v", applyCalls, observeCalls, f.effects.CapabilityRevoked(lifecycle.capability))
	}
}

func TestWriterLifecycleStatusRejectsZeroAndMultipleProviderMatches(t *testing.T) {
	for _, testCase := range []struct {
		name     string
		values   []Digest
		expected error
	}{
		{name: "zero", values: []Digest{}, expected: ErrNotObserved},
		{name: "multiple", values: []Digest{HashString("synthetic-one"), HashString("synthetic-two")}, expected: ErrMultipleObserved},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			f, _, lifecycle, provider, _ := lifecycleForCommittedWriter(t)
			provider.setApplyAmbiguity(EffectCapabilityRevoke, ProviderAmbiguousEOF)
			if err := lifecycle.RecordRevocation(context.Background(), RevokeCapability, writerLifecycleAuth(f, lifecycle, EffectCapabilityRevoke, MutationCall), FaultNone); !errors.Is(err, ErrReconcileRequired) {
				t.Fatal(err)
			}
			provider.setObserveOverride(EffectCapabilityRevoke, testCase.values)
			if err := lifecycle.ReconcileRevocation(context.Background(), RevokeCapability, writerLifecycleAuth(f, lifecycle, EffectCapabilityRevoke, StatusCall)); !errors.Is(err, testCase.expected) {
				t.Fatalf("wanted %v, got %v", testCase.expected, err)
			}
			applyCalls, _ := provider.callCounts(EffectCapabilityRevoke)
			if applyCalls != 1 {
				t.Fatalf("status path repeated mutation: %d", applyCalls)
			}
		})
	}
}

func TestWriterLifecycleStatusNeverMutatesCapabilityOrWrappingKey(t *testing.T) {
	f, _, lifecycle, provider, _ := lifecycleForCommittedWriter(t)
	if err := lifecycle.RecordRevocation(context.Background(), RevokeCapability, writerLifecycleAuth(f, lifecycle, EffectCapabilityRevoke, MutationCall), FaultHTTP408BeforeEffect); !errors.Is(err, ErrReconcileRequired) {
		t.Fatalf("capability before-effect ambiguity: %v", err)
	}
	if f.effects.CapabilityRevoked(lifecycle.capability) {
		t.Fatal("before-effect authorization mutated capability state")
	}
	if err := lifecycle.ReconcileRevocation(context.Background(), RevokeCapability, writerLifecycleAuth(f, lifecycle, EffectCapabilityRevoke, StatusCall)); !errors.Is(err, ErrNotObserved) {
		t.Fatalf("capability status absence: %v", err)
	}
	applyCalls, observeCalls := provider.callCounts(EffectCapabilityRevoke)
	if f.effects.CapabilityRevoked(lifecycle.capability) || applyCalls != 0 || observeCalls != 1 {
		t.Fatalf("status path mutated or reissued capability revocation: apply=%d observe=%d", applyCalls, observeCalls)
	}

	f, _, lifecycle, _, _ = lifecycleForCommittedWriter(t)
	// Complete the predecessors without ambiguity, then stop wrapping-key
	// revocation before its issue callback.
	for _, kind := range []RevocationKind{RevokeCapability, RevokeLeaf, RevokeMTLS} {
		if err := recordLifecycleRevocationForTest(t, lifecycle, f, kind, FaultNone); err != nil {
			t.Fatal(err)
		}
	}
	if err := lifecycle.RecordWrappingKeySealerRevocation(context.Background(), wrappingKeySealerAuth(f, lifecycle, MutationCall), FaultHTTP408BeforeEffect); !errors.Is(err, ErrReconcileRequired) {
		t.Fatalf("wrapping-key before-effect ambiguity: %v", err)
	}
	if _, err := markerSealerTerminalStatus(f.sealer); !errors.Is(err, ErrNotObserved) {
		t.Fatalf("wrapping key changed before status: %v", err)
	}
	if err := lifecycle.ReconcileWrappingKeySealerRevocation(context.Background(), wrappingKeySealerAuth(f, lifecycle, StatusCall)); !errors.Is(err, ErrNotObserved) {
		t.Fatalf("wrapping-key status absence: %v", err)
	}
	if _, err := markerSealerTerminalStatus(f.sealer); !errors.Is(err, ErrNotObserved) {
		t.Fatalf("status path revoked wrapping key: %v", err)
	}
}

func completeWrappingKeyPredecessors(t *testing.T, lifecycle *WriterLifecycle, f *protocolFixture) {
	t.Helper()
	for _, kind := range []RevocationKind{RevokeCapability, RevokeLeaf, RevokeMTLS} {
		if err := recordLifecycleRevocationForTest(t, lifecycle, f, kind, FaultNone); err != nil {
			t.Fatalf("complete %s: %v", kind, err)
		}
	}
}

func TestWriterLifecycleWrappingKeyBeforeAndAfterFaultsAreIssueOnce(t *testing.T) {
	for _, fault := range []FaultMode{FaultCrashAfterAuthorization, FaultHTTP408BeforeEffect, FaultEOFBeforeEffect, Fault5xxBeforeEffect} {
		t.Run("before-"+string(fault), func(t *testing.T) {
			f, _, lifecycle, provider, _ := lifecycleForCommittedWriter(t)
			completeWrappingKeyPredecessors(t, lifecycle, f)
			err := lifecycle.RecordWrappingKeySealerRevocation(context.Background(), wrappingKeySealerAuth(f, lifecycle, MutationCall), fault)
			if fault == FaultCrashAfterAuthorization {
				if !errors.Is(err, ErrSimulatedCrash) {
					t.Fatalf("wanted crash, got %v", err)
				}
			} else if !errors.Is(err, ErrReconcileRequired) {
				t.Fatalf("wanted reconciliation, got %v", err)
			}
			if revoke, _ := f.sealer.callCounts(); revoke != 0 {
				t.Fatalf("before-effect fault revoked sealer %d times", revoke)
			}
			if apply, _ := provider.callCounts(EffectWrappingKeyRevoke); apply != 0 {
				t.Fatalf("before-effect fault invoked provider %d times", apply)
			}
			if err := lifecycle.ReconcileWrappingKeySealerRevocation(context.Background(), wrappingKeySealerAuth(f, lifecycle, StatusCall)); !errors.Is(err, ErrNotObserved) {
				t.Fatalf("status fabricated absent composite: %v", err)
			}
			if revoke, _ := f.sealer.callCounts(); revoke != 0 {
				t.Fatalf("status revoked sealer %d times", revoke)
			}
		})
	}

	for _, fault := range []FaultMode{FaultCrashAfterEffect, FaultHTTP408AfterEffect, FaultEOFAfterEffect, Fault5xxAfterEffect} {
		t.Run("after-"+string(fault), func(t *testing.T) {
			f, _, lifecycle, provider, _ := lifecycleForCommittedWriter(t)
			completeWrappingKeyPredecessors(t, lifecycle, f)
			if err := lifecycle.RecordWrappingKeySealerRevocation(context.Background(), wrappingKeySealerAuth(f, lifecycle, MutationCall), FaultNone); err != nil {
				t.Fatal(err)
			}
			err := lifecycle.RecordRevocation(context.Background(), RevokeWrappingKey, writerLifecycleAuth(f, lifecycle, EffectWrappingKeyRevoke, MutationCall), fault)
			if fault == FaultCrashAfterEffect {
				if !errors.Is(err, ErrSimulatedCrash) {
					t.Fatalf("wanted crash, got %v", err)
				}
			} else if !errors.Is(err, ErrReconcileRequired) {
				t.Fatalf("wanted reconciliation, got %v", err)
			}
			if revoke, _ := f.sealer.callCounts(); revoke != 1 {
				t.Fatalf("after-effect fault revoke calls=%d", revoke)
			}
			if apply, _ := provider.callCounts(EffectWrappingKeyRevoke); apply != 1 {
				t.Fatalf("after-effect fault provider calls=%d", apply)
			}
			request := lifecycle.wrappingKeySealerRequest()
			lifecycle.writer.effects.mu.Lock()
			intent, ok := lifecycle.writer.effects.preparedLifecycleSealerRevocations[request.key()]
			lifecycle.writer.effects.mu.Unlock()
			if !ok || intent.RequestSHA256.IsZero() || intent.Signature == nil {
				t.Fatal("wrapping-key revocation lacked a durable pre-effect intent")
			}
			if err := lifecycle.ReconcileRevocation(context.Background(), RevokeWrappingKey, writerLifecycleAuth(f, lifecycle, EffectWrappingKeyRevoke, StatusCall)); err != nil {
				t.Fatal(err)
			}
			if revoke, _ := f.sealer.callCounts(); revoke != 1 {
				t.Fatalf("status repeated sealer revocation: %d", revoke)
			}
			if apply, observe := provider.callCounts(EffectWrappingKeyRevoke); apply != 1 || observe != 1 {
				t.Fatalf("status did not reconcile exact provider fact: apply=%d observe=%d", apply, observe)
			}
		})
	}
}

func TestWriterLifecycleSealerAfterEffectFaultsReconcileThenContinueProviderExactlyOnce(t *testing.T) {
	for _, fault := range []FaultMode{FaultCrashAfterEffect, FaultHTTP408AfterEffect, FaultEOFAfterEffect, Fault5xxAfterEffect} {
		t.Run(string(fault), func(t *testing.T) {
			f, _, lifecycle, provider, _ := lifecycleForCommittedWriter(t)
			completeWrappingKeyPredecessors(t, lifecycle, f)
			err := lifecycle.RecordWrappingKeySealerRevocation(context.Background(), wrappingKeySealerAuth(f, lifecycle, MutationCall), fault)
			if fault == FaultCrashAfterEffect {
				if !errors.Is(err, ErrSimulatedCrash) {
					t.Fatalf("wanted crash, got %v", err)
				}
			} else if !errors.Is(err, ErrReconcileRequired) {
				t.Fatalf("wanted reconciliation, got %v", err)
			}
			if revoke, _ := f.sealer.callCounts(); revoke != 1 {
				t.Fatalf("sealer calls=%d", revoke)
			}
			if apply, _ := provider.callCounts(EffectWrappingKeyRevoke); apply != 0 {
				t.Fatalf("provider ran inside sealer stage: %d", apply)
			}
			if err := lifecycle.ReconcileWrappingKeySealerRevocation(context.Background(), wrappingKeySealerAuth(f, lifecycle, StatusCall)); err != nil {
				t.Fatalf("sealer status reconciliation: %v", err)
			}
			if err := lifecycle.RecordRevocation(context.Background(), RevokeWrappingKey, writerLifecycleAuth(f, lifecycle, EffectWrappingKeyRevoke, MutationCall), FaultNone); err != nil {
				t.Fatalf("provider continuation: %v", err)
			}
			if revoke, _ := f.sealer.callCounts(); revoke != 1 {
				t.Fatalf("continuation repeated sealer revoke: %d", revoke)
			}
			if apply, _ := provider.callCounts(EffectWrappingKeyRevoke); apply != 1 {
				t.Fatalf("provider continuation calls=%d", apply)
			}
		})
	}
}

func TestWriterLifecycleWrappingKeyRequiresBothSealerAndProviderFacts(t *testing.T) {
	for _, kind := range []ProviderAmbiguityKind{ProviderAmbiguousHTTP408, ProviderAmbiguousEOF, ProviderAmbiguous5xx} {
		t.Run("sealer-"+string(kind), func(t *testing.T) {
			f, _, lifecycle, provider, observer := lifecycleForCommittedWriter(t)
			completeWrappingKeyPredecessors(t, lifecycle, f)
			f.sealer.setRevokeAmbiguity(kind)
			if err := lifecycle.RecordWrappingKeySealerRevocation(context.Background(), wrappingKeySealerAuth(f, lifecycle, MutationCall), FaultNone); !errors.Is(err, ErrReconcileRequired) {
				t.Fatalf("sealer ambiguity: %v", err)
			}
			if revoke, _ := f.sealer.callCounts(); revoke != 1 {
				t.Fatalf("sealer revoke calls=%d", revoke)
			}
			if apply, _ := provider.callCounts(EffectWrappingKeyRevoke); apply != 0 {
				t.Fatalf("provider ran after ambiguous sealer result: %d", apply)
			}
			if err := lifecycle.ReconcileWrappingKeySealerRevocation(context.Background(), wrappingKeySealerAuth(f, lifecycle, StatusCall)); err != nil {
				t.Fatalf("sealer status did not complete its independent stage: %v", err)
			}
			if revoke, _ := f.sealer.callCounts(); revoke != 1 {
				t.Fatalf("status repeated sealer revocation: %d", revoke)
			}
			authority, err := NewWriterAuthority(f.writerAuthority, f.writerPrivate)
			if err != nil {
				t.Fatal(err)
			}
			lifecycle, err = NewWriterLifecycleContinuation(f.boundary, f.effects, authority, f.writerBroker, f.sealer,
				f.writerProvider, f.body, originalProjection(), provider, observer)
			if err != nil {
				t.Fatalf("reconstruct terminal-sealer lifecycle: %v", err)
			}
			if lifecycle.writer.broker.source != nil {
				t.Fatal("terminal-sealer continuation retained a source mutation handle")
			}
			if err := lifecycle.RecordRevocation(context.Background(), RevokeWrappingKey, writerLifecycleAuth(f, lifecycle, EffectWrappingKeyRevoke, MutationCall), FaultNone); err != nil {
				t.Fatalf("provider continuation after sealer reconciliation: %v", err)
			}
			if apply, _ := provider.callCounts(EffectWrappingKeyRevoke); apply != 1 {
				t.Fatalf("provider continuation calls=%d", apply)
			}
		})
	}

	for _, kind := range []ProviderAmbiguityKind{ProviderAmbiguousHTTP408, ProviderAmbiguousEOF, ProviderAmbiguous5xx} {
		t.Run("provider-"+string(kind), func(t *testing.T) {
			f, _, lifecycle, provider, _ := lifecycleForCommittedWriter(t)
			completeWrappingKeyPredecessors(t, lifecycle, f)
			if err := lifecycle.RecordWrappingKeySealerRevocation(context.Background(), wrappingKeySealerAuth(f, lifecycle, MutationCall), FaultNone); err != nil {
				t.Fatal(err)
			}
			provider.setApplyAmbiguity(EffectWrappingKeyRevoke, kind)
			if err := lifecycle.RecordRevocation(context.Background(), RevokeWrappingKey, writerLifecycleAuth(f, lifecycle, EffectWrappingKeyRevoke, MutationCall), FaultNone); !errors.Is(err, ErrReconcileRequired) {
				t.Fatalf("provider ambiguity: %v", err)
			}
			transient := errors.New("synthetic wrapping status failure")
			provider.setObserveFailure(EffectWrappingKeyRevoke, transient)
			if err := lifecycle.ReconcileRevocation(context.Background(), RevokeWrappingKey, writerLifecycleAuth(f, lifecycle, EffectWrappingKeyRevoke, StatusCall)); !errors.Is(err, transient) {
				t.Fatalf("transient status error: %v", err)
			}
			provider.setObserveFailure(EffectWrappingKeyRevoke, nil)
			if err := lifecycle.ReconcileRevocation(context.Background(), RevokeWrappingKey, writerLifecycleAuth(f, lifecycle, EffectWrappingKeyRevoke, StatusCall)); err != nil {
				t.Fatal(err)
			}
			if revoke, _ := f.sealer.callCounts(); revoke != 1 {
				t.Fatalf("provider status path repeated sealer revocation: %d", revoke)
			}
			if apply, observe := provider.callCounts(EffectWrappingKeyRevoke); apply != 1 || observe != 2 {
				t.Fatalf("provider status counts: apply=%d observe=%d", apply, observe)
			}
		})
	}
}

func TestWriterLifecycleWrappingKeyStagesHaveDistinctDurableRequestsAndAuthentication(t *testing.T) {
	f, _, lifecycle, provider, _ := lifecycleForCommittedWriter(t)
	completeWrappingKeyPredecessors(t, lifecycle, f)
	reused := writerLifecycleAuth(f, lifecycle, EffectWrappingKeyRevoke, MutationCall)
	if err := lifecycle.RecordRevocation(context.Background(), RevokeWrappingKey, reused, FaultNone); !errors.Is(err, ErrForkNotAuthorized) {
		t.Fatalf("provider stage started before sealer completion: %v", err)
	}
	if err := lifecycle.RecordWrappingKeySealerRevocation(context.Background(), reused, FaultNone); !errors.Is(err, ErrAuthReplay) {
		t.Fatalf("semantic failure did not burn provider-stage auth: %v", err)
	}
	if err := lifecycle.RecordWrappingKeySealerRevocation(context.Background(), wrappingKeySealerAuth(f, lifecycle, MutationCall), FaultNone); err != nil {
		t.Fatal(err)
	}
	if err := lifecycle.RecordRevocation(context.Background(), RevokeWrappingKey, writerLifecycleAuth(f, lifecycle, EffectWrappingKeyRevoke, MutationCall), FaultNone); err != nil {
		t.Fatal(err)
	}
	sealerRequest := lifecycle.wrappingKeySealerRequest()
	providerRequest := lifecycle.request(EffectWrappingKeyRevoke)
	sealerDigest, _ := sealerRequest.Digest()
	providerDigest, _ := providerRequest.Digest()
	if sealerDigest == providerDigest || f.effects.IssueCount(sealerRequest) != 1 || f.effects.IssueCount(providerRequest) != 1 {
		t.Fatal("sealer and provider revocation did not use distinct issue-once durable requests")
	}
	if revoke, _ := f.sealer.callCounts(); revoke != 1 {
		t.Fatalf("sealer calls=%d", revoke)
	}
	if apply, _ := provider.callCounts(EffectWrappingKeyRevoke); apply != 1 {
		t.Fatalf("provider calls=%d", apply)
	}
}

func TestWriterLifecycleSemanticFailureBurnsAuthentication(t *testing.T) {
	f, _, lifecycle, provider, _ := lifecycleForCommittedWriter(t)
	auth := writerLifecycleAuth(f, lifecycle, EffectLeafRevoke, MutationCall)
	if err := lifecycle.RecordRevocation(context.Background(), RevokeLeaf, auth, FaultNone); !errors.Is(err, ErrForkNotAuthorized) {
		t.Fatalf("out-of-order lifecycle mutation: %v", err)
	}
	if err := lifecycle.RecordRevocation(context.Background(), RevokeCapability, auth, FaultNone); !errors.Is(err, ErrAuthReplay) {
		t.Fatalf("semantic-error authentication was reusable: %v", err)
	}
	capabilityApply, _ := provider.callCounts(EffectCapabilityRevoke)
	leafApply, _ := provider.callCounts(EffectLeafRevoke)
	if capabilityApply != 0 || leafApply != 0 || f.effects.CapabilityRevoked(lifecycle.capability) {
		t.Fatalf("semantic failure reached provider/local state: capability=%d leaf=%d revoked=%v", capabilityApply, leafApply, f.effects.CapabilityRevoked(lifecycle.capability))
	}
}

func TestWriterTerminalFactsRequireIndependentExactProtectedWorkflowObservation(t *testing.T) {
	f, _, lifecycle, provider, observer := lifecycleForCommittedWriter(t)
	completeWriterLifecycle(t, lifecycle, f)
	evidence, err := lifecycle.ObserveTerminalEvidence(context.Background(), terminalObservationAuth(f, lifecycle, MutationCall), FaultNone)
	if err != nil {
		t.Fatal(err)
	}
	if evidence.ProviderObservation.ObserverIdentitySHA256 != observer.IdentitySHA256() || evidence.ProviderObservation.MutationProviderSHA256 != provider.IdentitySHA256() {
		t.Fatal("terminal observation is not bound to the independent observer and mutation provider")
	}
	terminalReceipt, err := lifecycle.AcceptTerminalEvidence(context.Background(), evidence, terminalPublicationAuth(f, lifecycle, evidence, MutationCall), FaultNone)
	if err != nil {
		t.Fatal(err)
	}

	mutations := map[string]func(*WriterTerminalEvidence){
		"fact": func(candidate *WriterTerminalEvidence) { candidate.BindingAbsent = false },
		"app-inventory-one-incomplete": func(candidate *WriterTerminalEvidence) {
			candidate.AppInventoryReadOneComplete = false
		},
		"app-inventory-two-incomplete": func(candidate *WriterTerminalEvidence) {
			candidate.AppInventoryReadTwoComplete = false
		},
		"deployment-inventory-one-incomplete": func(candidate *WriterTerminalEvidence) {
			candidate.DeploymentInventoryReadOneComplete = false
		},
		"deployment-inventory-two-incomplete": func(candidate *WriterTerminalEvidence) {
			candidate.DeploymentInventoryReadTwoComplete = false
		},
		"app-inventory-one-nonzero": func(candidate *WriterTerminalEvidence) { candidate.AppInventoryCountOne = 1 },
		"app-inventory-two-nonzero": func(candidate *WriterTerminalEvidence) { candidate.AppInventoryCountTwo = 1 },
		"deployment-inventory-one-nonzero": func(candidate *WriterTerminalEvidence) {
			candidate.DeploymentInventoryCountOne = 1
		},
		"deployment-inventory-two-nonzero": func(candidate *WriterTerminalEvidence) {
			candidate.DeploymentInventoryCountTwo = 1
		},
		"operation": func(candidate *WriterTerminalEvidence) {
			candidate.ProviderObservation.OperationBodySHA256 = HashString("synthetic-wrong-operation")
		},
		"provider": func(candidate *WriterTerminalEvidence) {
			candidate.ProviderObservation.MutationProviderSHA256 = HashString("synthetic-wrong-provider")
		},
		"projection": func(candidate *WriterTerminalEvidence) {
			candidate.ProviderObservation.OriginalProjectionSHA256 = HashString("synthetic-wrong-projection")
		},
		"timestamp": func(candidate *WriterTerminalEvidence) {
			candidate.CapabilityRevokedAt = candidate.CapabilityRevokedAt.Add(time.Nanosecond)
		},
		"incomplete-provider-operation-inventory": func(candidate *WriterTerminalEvidence) {
			candidate.ProviderOperationReadTwoComplete = false
		},
		"nonterminal-provider-operation": func(candidate *WriterTerminalEvidence) {
			candidate.ProviderNonterminalCountTwo = 1
		},
		"provider-operation-count-drift": func(candidate *WriterTerminalEvidence) {
			candidate.ProviderOperationCountTwo++
		},
		"wrapping-provider-observed-result": func(candidate *WriterTerminalEvidence) {
			candidate.WrappingKeyProviderObservedResultSHA256 = HashString("synthetic-wrong-provider-observed-result")
		},
		"wrapping-sealer-request": func(candidate *WriterTerminalEvidence) {
			candidate.WrappingKeySealerRequestSHA256 = HashString("synthetic-wrong-sealer-request")
		},
		"wrapping-sealer-result": func(candidate *WriterTerminalEvidence) {
			candidate.WrappingKeySealerResultSHA256 = HashString("synthetic-wrong-sealer-result")
		},
		"wrapping-sealer-intent-signature": func(candidate *WriterTerminalEvidence) {
			candidate.WrappingKeySealerIntent.Signature[0] ^= 0x01
		},
		"wrapping-sealer-status-result": func(candidate *WriterTerminalEvidence) {
			candidate.WrappingKeySealerStatus.ResultSHA256 = HashString("synthetic-wrong-sealer-status-result")
		},
		"wrapping-sealer-completion": func(candidate *WriterTerminalEvidence) {
			candidate.WrappingKeySealerCompletionWitness.Signature[0] ^= 0x01
		},
		"wrapping-provider-request": func(candidate *WriterTerminalEvidence) {
			candidate.WrappingKeyProviderRequestSHA256 = HashString("synthetic-wrong-provider-request")
		},
		"wrapping-provider-result": func(candidate *WriterTerminalEvidence) {
			candidate.WrappingKeyProviderResultSHA256 = HashString("synthetic-wrong-provider-result")
		},
		"wrapping-provider-completion": func(candidate *WriterTerminalEvidence) {
			candidate.WrappingKeyProviderCompletionWitness.Signature[0] ^= 0x01
		},
		"observer-identity": func(candidate *WriterTerminalEvidence) {
			candidate.ProviderObservation.ObserverIdentitySHA256 = HashString("synthetic-wrong-observer")
		},
		"delete-request": func(candidate *WriterTerminalEvidence) {
			candidate.DeletionRequestSHA256 = HashString("synthetic-wrong-delete-request")
		},
		"delete-result": func(candidate *WriterTerminalEvidence) {
			candidate.Deletion.DeleteActionSHA256 = HashString("synthetic-wrong-delete-result")
		},
		"delete-completion-request": func(candidate *WriterTerminalEvidence) {
			candidate.DeletionCompletionWitness.RequestSHA256 = HashString("synthetic-wrong-completion-request")
		},
		"delete-completion-result": func(candidate *WriterTerminalEvidence) {
			candidate.DeletionCompletionWitness.ResultSHA256 = HashString("synthetic-wrong-completion-result")
		},
		"delete-completion-time": func(candidate *WriterTerminalEvidence) {
			candidate.DeletionCompletionWitness.CompletedAt = candidate.DeletionCompletionWitness.CompletedAt.Add(time.Nanosecond)
		},
		"delete-completion-signature": func(candidate *WriterTerminalEvidence) {
			candidate.DeletionCompletionWitness.Signature[0] ^= 0x01
		},
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			candidate := evidence
			candidate.WrappingKeySealerIntent.Signature = append([]byte(nil), evidence.WrappingKeySealerIntent.Signature...)
			candidate.WrappingKeySealerCompletionWitness.Signature = append([]byte(nil), evidence.WrappingKeySealerCompletionWitness.Signature...)
			candidate.WrappingKeyProviderCompletionWitness.Signature = append([]byte(nil), evidence.WrappingKeyProviderCompletionWitness.Signature...)
			candidate.DeletionCompletionWitness.Signature = append([]byte(nil), evidence.DeletionCompletionWitness.Signature...)
			mutate(&candidate)
			if err := VerifyWriterTerminalReceipt(f.boundary, terminalReceipt, candidate); !errors.Is(err, ErrForkNotAuthorized) {
				t.Fatalf("tampered provider evidence accepted: %v", err)
			}
		})
	}

	t.Run("durable-reconciliation-cannot-precede-provider-observation", func(t *testing.T) {
		candidate := evidence
		candidate.DurableCompletionAt = make(map[Effect]time.Time, len(evidence.DurableCompletionAt))
		for effect, completedAt := range evidence.DurableCompletionAt {
			candidate.DurableCompletionAt[effect] = completedAt
		}
		candidate.DurableCompletionAt[EffectCapabilityRevoke] = evidence.CapabilityRevokedAt.Add(-time.Nanosecond)
		if err := candidate.Validate(); !errors.Is(err, ErrForkNotAuthorized) {
			t.Fatalf("advanced provider clock accepted: %v", err)
		}
	})
}

func terminalProviderReadCount(provider *SyntheticWriterLifecycleProvider) int {
	provider.mu.Lock()
	defer provider.mu.Unlock()
	return provider.terminalReadCalls
}

func TestTerminalObservationIsAuthFirstIssueOnceAndStatusOnlyAcrossFaults(t *testing.T) {
	for _, fault := range []FaultMode{FaultCrashAfterAuthorization, FaultHTTP408BeforeEffect, FaultEOFBeforeEffect, Fault5xxBeforeEffect} {
		t.Run("before-"+string(fault), func(t *testing.T) {
			f, _, lifecycle, provider, _ := lifecycleForCommittedWriter(t)
			completeWriterLifecycle(t, lifecycle, f)
			_, err := lifecycle.ObserveTerminalEvidence(context.Background(), terminalObservationAuth(f, lifecycle, MutationCall), fault)
			if fault == FaultCrashAfterAuthorization {
				if !errors.Is(err, ErrSimulatedCrash) {
					t.Fatalf("wanted crash, got %v", err)
				}
			} else if !errors.Is(err, ErrReconcileRequired) {
				t.Fatalf("wanted reconciliation, got %v", err)
			}
			if got := terminalProviderReadCount(provider); got != 0 {
				t.Fatalf("before-effect observation reads=%d", got)
			}
			if _, err := lifecycle.ReconcileTerminalObservation(context.Background(), terminalObservationAuth(f, lifecycle, StatusCall)); !errors.Is(err, ErrNotObserved) {
				t.Fatalf("status fabricated evidence: %v", err)
			}
			if got := terminalProviderReadCount(provider); got != 0 {
				t.Fatalf("status performed provider reads=%d", got)
			}
		})
	}

	for _, fault := range []FaultMode{FaultCrashAfterEffect, FaultHTTP408AfterEffect, FaultEOFAfterEffect, Fault5xxAfterEffect} {
		t.Run("after-"+string(fault), func(t *testing.T) {
			f, writer, lifecycle, provider, observer := lifecycleForCommittedWriter(t)
			completeWriterLifecycle(t, lifecycle, f)
			_, err := lifecycle.ObserveTerminalEvidence(context.Background(), terminalObservationAuth(f, lifecycle, MutationCall), fault)
			if fault == FaultCrashAfterEffect {
				if !errors.Is(err, ErrSimulatedCrash) {
					t.Fatalf("wanted crash, got %v", err)
				}
			} else if !errors.Is(err, ErrReconcileRequired) {
				t.Fatalf("wanted reconciliation, got %v", err)
			}
			if got := terminalProviderReadCount(provider); got != 2 {
				t.Fatalf("issue-once provider reads=%d", got)
			}
			reconstructed, err := NewWriterLifecycle(writer, f.body, originalProjection(), provider, observer)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := reconstructed.ReconcileTerminalObservation(context.Background(), terminalObservationAuth(f, reconstructed, StatusCall)); err != nil {
				t.Fatal(err)
			}
			if got := terminalProviderReadCount(provider); got != 2 {
				t.Fatalf("status repeated provider reads=%d", got)
			}
		})
	}
}

func TestTerminalObservationBurnsEarlyInvalidAuthAndIsConcurrentIssueOnce(t *testing.T) {
	f, _, lifecycle, provider, _ := lifecycleForCommittedWriter(t)
	auth := terminalObservationAuth(f, lifecycle, MutationCall)
	if _, err := lifecycle.ObserveTerminalEvidence(context.Background(), auth, FaultNone); !errors.Is(err, ErrForkNotAuthorized) {
		t.Fatalf("incomplete lifecycle accepted: %v", err)
	}
	if got := terminalProviderReadCount(provider); got != 0 {
		t.Fatalf("semantic failure reached provider: %d", got)
	}
	completeWriterLifecycle(t, lifecycle, f)
	if _, err := lifecycle.ObserveTerminalEvidence(context.Background(), auth, FaultNone); !errors.Is(err, ErrAuthReplay) {
		t.Fatalf("semantic-failure auth was reusable: %v", err)
	}

	const callers = 16
	var wg sync.WaitGroup
	errs := make(chan error, callers)
	for index := 0; index < callers; index++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := lifecycle.ObserveTerminalEvidence(context.Background(), terminalObservationAuth(f, lifecycle, MutationCall), FaultNone)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := terminalProviderReadCount(provider); got != 2 {
		t.Fatalf("concurrent issue performed %d provider reads", got)
	}
	replay := terminalObservationAuth(f, lifecycle, MutationCall)
	if _, err := lifecycle.ObserveTerminalEvidence(context.Background(), replay, FaultNone); err != nil {
		t.Fatal(err)
	}
	if _, err := lifecycle.ObserveTerminalEvidence(context.Background(), replay, FaultNone); !errors.Is(err, ErrAuthReplay) {
		t.Fatalf("exact envelope replay accepted: %v", err)
	}
}

func TestTerminalObservationRejectsFractionalClockDivergenceBeforeProviderRead(t *testing.T) {
	f, _, lifecycle, provider, observer := lifecycleForCommittedWriter(t)
	completeWriterLifecycle(t, lifecycle, f)
	observer.clock = &manualObservationClock{now: protocolNow.Add(-MinimumTerminalObservationDelay), wait: -MinimumTerminalObservationDelay - time.Nanosecond}
	if _, err := lifecycle.ObserveTerminalEvidence(context.Background(), terminalObservationAuth(f, lifecycle, MutationCall), FaultNone); !errors.Is(err, ErrForkNotAuthorized) {
		t.Fatalf("clock rollback accepted: %v", err)
	}
	if got := terminalProviderReadCount(provider); got != 0 {
		t.Fatalf("provider read occurred after trusted-clock divergence, got %d", got)
	}
}

func TestTerminalObservationUsesIssuedNotBeforeAndExclusivePostReadExpiry(t *testing.T) {
	t.Run("issued-before-nbf-at-fractional-boundary", func(t *testing.T) {
		f, _, lifecycle, _, _ := lifecycleForCommittedWriter(t)
		completeWriterLifecycle(t, lifecycle, f)
		auth := terminalObservationAuth(f, lifecycle, MutationCall)
		auth.IssuedAt = f.clock.now.Add(-2 * time.Minute)
		auth.NotBefore = f.clock.now
		auth.ExpiresAt = f.clock.now.Add(time.Minute)
		evidence, err := lifecycle.ObserveTerminalEvidence(context.Background(), auth, FaultNone)
		if err != nil {
			t.Fatal(err)
		}
		if !evidence.ReadOneAt.Equal(auth.NotBefore) || !evidence.ReadTwoAt.After(evidence.ReadOneAt) || !evidence.ReadTwoAt.Before(auth.ExpiresAt) {
			t.Fatalf("observer chronology mismatch: one=%s two=%s", evidence.ReadOneAt, evidence.ReadTwoAt)
		}
	})

	for _, read := range []int{1, 2} {
		t.Run("expiry-after-read-"+string(rune('0'+read)), func(t *testing.T) {
			f, _, lifecycle, provider, _ := lifecycleForCommittedWriter(t)
			completeWriterLifecycle(t, lifecycle, f)
			auth := terminalObservationAuth(f, lifecycle, MutationCall)
			provider.setTerminalReadHook(func(call int) {
				if call == read {
					f.clock.now = auth.ExpiresAt
				}
			})
			if _, err := lifecycle.ObserveTerminalEvidence(context.Background(), auth, FaultNone); !errors.Is(err, ErrForkNotAuthorized) {
				t.Fatalf("post-GET exclusive expiry accepted: %v", err)
			}
			if got := terminalProviderReadCount(provider); got != read {
				t.Fatalf("provider reads=%d want=%d", got, read)
			}
		})
	}

	t.Run("observer-authority-diverge-after-get", func(t *testing.T) {
		f, _, lifecycle, provider, observer := lifecycleForCommittedWriter(t)
		completeWriterLifecycle(t, lifecycle, f)
		isolated := &manualObservationClock{now: f.clock.now}
		observer.clock = isolated
		provider.setTerminalReadHook(func(call int) {
			if call == 1 {
				isolated.now = isolated.now.Add(time.Nanosecond)
			}
		})
		if _, err := lifecycle.ObserveTerminalEvidence(context.Background(), terminalObservationAuth(f, lifecycle, MutationCall), FaultNone); !errors.Is(err, ErrForkNotAuthorized) {
			t.Fatalf("post-GET clock divergence accepted: %v", err)
		}
		if got := terminalProviderReadCount(provider); got != 1 {
			t.Fatalf("provider reads=%d want=1", got)
		}
	})
}

func TestTerminalObserverFacadeCannotBeAssertedToMutationAuthority(t *testing.T) {
	f, _, lifecycle, provider, observer := lifecycleForCommittedWriter(t)
	_ = f
	_ = lifecycle
	facade, err := provider.TerminalFactsFacade()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := any(facade).(WriterLifecycleProvider); ok {
		t.Fatal("GET-only terminal facade is type-assertable to writer mutation authority")
	}
	if _, ok := observer.reader.(WriterLifecycleProvider); ok {
		t.Fatal("terminal observer retained a mutation-capable provider")
	}
}

func TestWriterTerminalObserverRejectsTransientZeroMultipleAndUnstableReads(t *testing.T) {
	for _, testCase := range []struct {
		name      string
		configure func(*SyntheticWriterLifecycleProvider)
		expected  error
	}{
		{name: "transient", configure: func(provider *SyntheticWriterLifecycleProvider) {
			provider.setTerminalReadFailure(errors.New("synthetic GET failure"))
		}, expected: errors.New("synthetic GET failure")},
		{name: "zero", configure: func(provider *SyntheticWriterLifecycleProvider) {
			provider.setTerminalReadOverride([]writerTerminalProviderFacts{})
		}, expected: ErrNotObserved},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			f, _, lifecycle, provider, _ := lifecycleForCommittedWriter(t)
			completeWriterLifecycle(t, lifecycle, f)
			testCase.configure(provider)
			_, err := lifecycle.ObserveTerminalEvidence(context.Background(), terminalObservationAuth(f, lifecycle, MutationCall), FaultNone)
			if testCase.name == "transient" {
				if err == nil || err.Error() != testCase.expected.Error() {
					t.Fatalf("wanted transient failure, got %v", err)
				}
			} else if !errors.Is(err, testCase.expected) {
				t.Fatalf("wanted %v, got %v", testCase.expected, err)
			}
		})
	}

	f, _, lifecycle, provider, _ := lifecycleForCommittedWriter(t)
	completeWriterLifecycle(t, lifecycle, f)
	facade, err := provider.TerminalFactsFacade()
	if err != nil {
		t.Fatal(err)
	}
	facts, err := facade.ReadTerminalFacts()
	if err != nil || len(facts) != 1 {
		t.Fatal(err)
	}
	provider.setTerminalReadOverride([]writerTerminalProviderFacts{facts[0], facts[0]})
	if _, err := lifecycle.ObserveTerminalEvidence(context.Background(), terminalObservationAuth(f, lifecycle, MutationCall), FaultNone); !errors.Is(err, ErrMultipleObserved) {
		t.Fatalf("multiple provider projections accepted: %v", err)
	}

	f, _, lifecycle, provider, _ = lifecycleForCommittedWriter(t)
	completeWriterLifecycle(t, lifecycle, f)
	facade, err = provider.TerminalFactsFacade()
	if err != nil {
		t.Fatal(err)
	}
	facts, err = facade.ReadTerminalFacts()
	if err != nil || len(facts) != 1 {
		t.Fatal(err)
	}
	drift := cloneWriterTerminalProviderFacts(facts[0])
	drift.StableActionLedgerSHA256 = HashString("synthetic-unstable-action-ledger")
	provider.setTerminalReadSequence([]writerTerminalProviderFacts{facts[0]}, []writerTerminalProviderFacts{drift})
	if _, err := lifecycle.ObserveTerminalEvidence(context.Background(), terminalObservationAuth(f, lifecycle, MutationCall), FaultNone); !errors.Is(err, ErrQuarantined) {
		t.Fatalf("unstable two-read projection accepted: %v", err)
	}
}

func TestWriterTerminalObserverRequiresCompleteZeroAppAndDeploymentInventoriesOnBothReads(t *testing.T) {
	type mutation func(*writerTerminalProviderFacts)
	tests := []struct {
		name   string
		mutate mutation
	}{
		{name: "app-incomplete", mutate: func(facts *writerTerminalProviderFacts) { facts.AppInventoryComplete = false }},
		{name: "deployment-incomplete", mutate: func(facts *writerTerminalProviderFacts) { facts.DeploymentInventoryComplete = false }},
		{name: "app-nonzero", mutate: func(facts *writerTerminalProviderFacts) { facts.PaginatedAppCount = 1 }},
		{name: "deployment-nonzero", mutate: func(facts *writerTerminalProviderFacts) { facts.PaginatedDeployments = 1 }},
	}
	for _, testCase := range tests {
		for _, target := range []string{"first", "second", "both"} {
			t.Run(testCase.name+"-"+target, func(t *testing.T) {
				f, _, lifecycle, provider, _ := lifecycleForCommittedWriter(t)
				completeWriterLifecycle(t, lifecycle, f)
				facts, err := provider.terminalFacts()
				if err != nil || len(facts) != 1 {
					t.Fatalf("terminal fixture facts: %v", err)
				}
				first := cloneWriterTerminalProviderFacts(facts[0])
				second := cloneWriterTerminalProviderFacts(facts[0])
				if target == "first" || target == "both" {
					testCase.mutate(&first)
				}
				if target == "second" || target == "both" {
					testCase.mutate(&second)
				}
				provider.setTerminalReadSequence([]writerTerminalProviderFacts{first}, []writerTerminalProviderFacts{second})
				if _, err := lifecycle.ObserveTerminalEvidence(context.Background(), terminalObservationAuth(f, lifecycle, MutationCall), FaultNone); err == nil {
					t.Fatal("incomplete or nonzero paginated inventory was accepted")
				}
			})
		}
	}
}

func TestWriterTerminalObserverRequiresExactNineEffectProviderInventoryOnBothReads(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*writerTerminalProviderFacts)
	}{
		{name: "inventory-incomplete", mutate: func(facts *writerTerminalProviderFacts) { facts.OperationInventoryComplete = false }},
		{name: "equal-zero-count", mutate: func(facts *writerTerminalProviderFacts) { facts.ProviderOperationCount = 0 }},
		{name: "equal-excess-count", mutate: func(facts *writerTerminalProviderFacts) { facts.ProviderOperationCount = 10 }},
		{name: "missing-effect", mutate: func(facts *writerTerminalProviderFacts) { delete(facts.EffectCompletedAt, EffectLeafRevoke) }},
		{name: "extra-effect", mutate: func(facts *writerTerminalProviderFacts) {
			facts.EffectCompletedAt[EffectEvidenceObserve] = facts.EffectCompletedAt[EffectBrokerDelete]
		}},
	}
	for _, testCase := range tests {
		for _, target := range []string{"first", "second", "both"} {
			t.Run(testCase.name+"-"+target, func(t *testing.T) {
				f, _, lifecycle, provider, _ := lifecycleForCommittedWriter(t)
				completeWriterLifecycle(t, lifecycle, f)
				facts, err := provider.terminalFacts()
				if err != nil || len(facts) != 1 {
					t.Fatalf("terminal fixture facts: %v", err)
				}
				first := cloneWriterTerminalProviderFacts(facts[0])
				second := cloneWriterTerminalProviderFacts(facts[0])
				if target == "first" || target == "both" {
					testCase.mutate(&first)
				}
				if target == "second" || target == "both" {
					testCase.mutate(&second)
				}
				provider.setTerminalReadSequence([]writerTerminalProviderFacts{first}, []writerTerminalProviderFacts{second})
				if _, err := lifecycle.ObserveTerminalEvidence(context.Background(), terminalObservationAuth(f, lifecycle, MutationCall), FaultNone); !errors.Is(err, ErrForkNotAuthorized) {
					t.Fatalf("wrong provider-operation inventory accepted: %v", err)
				}
			})
		}
	}
}

type nonTestWriterLifecycleProvider struct{ WriterLifecycleProvider }

func (nonTestWriterLifecycleProvider) IsTestOracleOnly() bool { return false }

type nonTestWriterTerminalObserver struct {
	WriterTerminalObservationAdapter
}

func (nonTestWriterTerminalObserver) IsTestOracleOnly() bool { return false }

func TestWriterLifecycleConstructorRejectsWrongBindingsAndProductionClaims(t *testing.T) {
	f := newProtocolFixture(t)
	writer := f.writer(t, markerEntropy())
	if _, err := writer.GenerateCommitAndReadback(context.Background(), f.body, markerAuth(f, writer, f.body, MutationCall), FaultNone); err != nil {
		t.Fatal(err)
	}
	projection := originalProjection()
	provider, err := NewSyntheticWriterLifecycleProvider(f.body, writer.broker.target, HashString("synthetic-right-config"), projection, func() time.Time { return protocolNow })
	if err != nil {
		t.Fatal(err)
	}
	facade, err := provider.TerminalFactsFacade()
	if err != nil {
		t.Fatal(err)
	}
	observer, err := NewSyntheticWriterTerminalObserver(facade, &manualObservationClock{now: protocolNow.Add(-MinimumTerminalObservationDelay)}, f.boundary.providerObserver)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewWriterLifecycle(writer, f.body, projection, nonTestWriterLifecycleProvider{provider}, observer); !errors.Is(err, ErrProductionAdapter) {
		t.Fatalf("production provider claim accepted: %v", err)
	}
	if _, err := NewWriterLifecycle(writer, f.body, projection, provider, nonTestWriterTerminalObserver{observer}); !errors.Is(err, ErrProductionAdapter) {
		t.Fatalf("production observer claim accepted: %v", err)
	}
	wrongProvider, err := NewSyntheticWriterLifecycleProvider(f.body, HashString("synthetic-wrong-source"), provider.ProviderConfigSHA256(), projection, func() time.Time { return protocolNow })
	if err != nil {
		t.Fatal(err)
	}
	wrongFacade, err := wrongProvider.TerminalFactsFacade()
	if err != nil {
		t.Fatal(err)
	}
	wrongObserver, err := NewSyntheticWriterTerminalObserver(wrongFacade, &manualObservationClock{now: protocolNow.Add(-MinimumTerminalObservationDelay)}, f.boundary.providerObserver)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewWriterLifecycle(writer, f.body, projection, wrongProvider, wrongObserver); !errors.Is(err, ErrRoleIsolation) {
		t.Fatalf("wrong provider source accepted: %v", err)
	}
	otherProvider, err := NewSyntheticWriterLifecycleProvider(f.body, writer.broker.target, HashString("synthetic-other-config"), projection, func() time.Time { return protocolNow })
	if err != nil {
		t.Fatal(err)
	}
	otherFacade, err := otherProvider.TerminalFactsFacade()
	if err != nil {
		t.Fatal(err)
	}
	otherObserver, err := NewSyntheticWriterTerminalObserver(otherFacade, &manualObservationClock{now: protocolNow.Add(-MinimumTerminalObservationDelay)}, f.boundary.providerObserver)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewWriterLifecycle(writer, f.body, projection, provider, otherObserver); !errors.Is(err, ErrRoleIsolation) {
		t.Fatalf("observer/provider binding mismatch accepted: %v", err)
	}
}

func TestWriterLifecycleConcurrentSameRequestCallsProviderExactlyOnce(t *testing.T) {
	f, _, lifecycle, provider, _ := lifecycleForCommittedWriter(t)
	const callers = 12
	var wait sync.WaitGroup
	errorsSeen := make(chan error, callers)
	wait.Add(callers)
	for index := 0; index < callers; index++ {
		go func() {
			defer wait.Done()
			errorsSeen <- lifecycle.RecordRevocation(context.Background(), RevokeCapability, writerLifecycleAuth(f, lifecycle, EffectCapabilityRevoke, MutationCall), FaultNone)
		}()
	}
	wait.Wait()
	close(errorsSeen)
	for err := range errorsSeen {
		if err != nil {
			t.Fatalf("concurrent idempotent request: %v", err)
		}
	}
	applyCalls, observeCalls := provider.callCounts(EffectCapabilityRevoke)
	if applyCalls != 1 || observeCalls != 0 || f.effects.IssueCount(lifecycle.request(EffectCapabilityRevoke)) != 1 {
		t.Fatalf("concurrency repeated provider mutation: apply=%d observe=%d issue=%d", applyCalls, observeCalls, f.effects.IssueCount(lifecycle.request(EffectCapabilityRevoke)))
	}
}
