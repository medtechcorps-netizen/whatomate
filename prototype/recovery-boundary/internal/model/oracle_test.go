package model

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var modelNow = time.Date(2032, 3, 4, 5, 6, 7, 123456789, time.UTC)
var crashModelWriterRoot = HashString("synthetic-crash-model-writer-authority/v2")

func testOperation(id string) OperationBody {
	return OperationBody{
		Version: ProtocolVersion, OperationID: id, Generation: 1, Phase: "baseline",
		ConfigDigest: HashString("synthetic-config-v2"),
	}
}

func testRequest(effect Effect, ordinal int) MutationRequest {
	return MutationRequest{
		Operation: testOperation(fmt.Sprintf("synthetic-op-%03d", ordinal)),
		Effect:    effect, ParametersDigest: HashString("synthetic-parameters-" + string(effect)),
	}
}

func testAuth(ordinal int) AuthEnvelope {
	entropy := HashString(fmt.Sprintf("synthetic-authority-challenge-%08d", ordinal))
	challenge, err := issueAuthorityChallenge(WriterAuthorityRole, crashModelWriterRoot, bytes.NewReader(entropy[:]))
	if err != nil {
		panic(err)
	}
	return newAuthEnvelope(
		fmt.Sprintf("synthetic-jti-%08d", ordinal), challenge,
		modelNow.Add(-time.Minute), modelNow.Add(-30*time.Second), modelNow.Add(time.Minute),
	)
}

func issuedTestAuth(engine *Engine, request MutationRequest, call CallKind, ordinal int) AuthEnvelope {
	entropy := HashString(fmt.Sprintf("synthetic-authority-challenge-%08d", ordinal))
	challenge, err := engine.IssueChallenge(context.Background(), request, call, bytes.NewReader(entropy[:]))
	if err != nil {
		panic(err)
	}
	return newAuthEnvelope(
		fmt.Sprintf("synthetic-jti-%08d", ordinal), challenge,
		modelNow.Add(-time.Minute), modelNow.Add(-30*time.Second), modelNow.Add(time.Minute),
	)
}

func crashEngine(t *testing.T, oracle *CrashOracle) *Engine {
	t.Helper()
	engine, err := NewCrashModelEngine(oracle, WriterAuthorityRole, crashModelWriterRoot, func() time.Time { return modelNow })
	if err != nil {
		t.Fatal(err)
	}
	if !engine.IsCrashModelOnly() {
		t.Fatal("crash oracle was represented as production authority")
	}
	return engine
}

func TestCrashOracleCannotConstructProductionEngine(t *testing.T) {
	oracle := NewCrashOracle()
	if _, err := NewProductionEngine(oracle, oracle, func() time.Time { return modelNow }); !errors.Is(err, ErrProductionAdapter) {
		t.Fatalf("got %v, want production-adapter failure", err)
	}
}

type selfAttestedProductionAdapter struct{ *CrashOracle }

func (*selfAttestedProductionAdapter) Capabilities() AdapterCapabilities {
	return AdapterCapabilities{
		Transactional: true, Durable: true, Fenced: true, AppendOnlyAudit: true,
		EncryptedAtRest: true, BackupRecovery: true, KeyConfined: true, NoAutomaticRetries: true,
	}
}

func TestProductionEngineRejectsSelfAttestedCapabilitiesInGateA(t *testing.T) {
	adapter := &selfAttestedProductionAdapter{CrashOracle: NewCrashOracle()}
	if !adapter.Capabilities().productionReady() {
		t.Fatal("test adapter does not exercise the self-attested production-ready case")
	}
	if _, err := NewProductionEngine(adapter, adapter, func() time.Time { return modelNow }); !errors.Is(err, ErrProductionAdapter) {
		t.Fatalf("self-attested production adapter accepted: %v", err)
	}
}

func TestOperationBodyExcludesAuthenticationAndProtectedPredecessor(t *testing.T) {
	body := testOperation("synthetic-op-fixed")
	if _, selectable := reflect.TypeOf(body).FieldByName("Predecessor"); selectable {
		t.Fatal("caller-visible operation body still exposes predecessor selection")
	}
	canonical, err := body.CanonicalBytes()
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(canonical, []byte("predecessor")) || bytes.Contains(canonical, []byte("challenge")) {
		t.Fatal("caller-visible operation identity contains protected predecessor/authentication")
	}
	nilDigest, _ := NilPredecessor().digest()
	emptyDigest, _ := EmptyPredecessor().digest()
	if nilDigest == emptyDigest {
		t.Fatal("protected nil and empty predecessor representations collapsed")
	}
	request := MutationRequest{Operation: body, Effect: EffectMarkerCAS, ParametersDigest: HashString("synthetic-marker-cas")}
	digest1, _ := request.Digest()
	first := testAuth(1)
	second := testAuth(2)
	if first.JTIHash() == second.JTIHash() || first.ChallengeHash() == second.ChallengeHash() {
		t.Fatal("fresh authentication hashes collided")
	}
	digest2, _ := request.Digest()
	if digest1 != digest2 {
		t.Fatal("authentication changed immutable operation-body digest")
	}
}

func TestAuthExclusiveExpiryAndFractionalBoundary(t *testing.T) {
	auth := testAuth(10)
	if err := auth.Validate(auth.ExpiresAt.Add(-time.Nanosecond)); err != nil {
		t.Fatalf("last valid fractional instant rejected: %v", err)
	}
	if err := auth.Validate(auth.ExpiresAt); !errors.Is(err, ErrInvalid) {
		t.Fatalf("exclusive expiry accepted: %v", err)
	}
	if err := auth.Validate(auth.NotBefore.Add(-time.Nanosecond)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("pre-nbf instant accepted: %v", err)
	}
}

func TestMutationSideEffectInventorySeparatesAuthenticatedReads(t *testing.T) {
	expected := []Effect{
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
	actual := MutationSideEffects()
	if len(actual) != len(expected) {
		t.Fatalf("mutation inventory length=%d want=%d", len(actual), len(expected))
	}
	seen := make(map[Effect]bool, len(actual))
	for index, effect := range actual {
		if effect != expected[index] || seen[effect] || effect == EffectEvidenceObserve {
			t.Fatalf("mutation inventory mismatch or duplicate at %d: %q", index, effect)
		}
		seen[effect] = true
	}
	actual[0] = EffectEvidenceObserve
	if MutationSideEffects()[0] != EffectAuthorityAppCreate {
		t.Fatal("mutation inventory caller could mutate shared authority")
	}
	operationCounts := map[Effect]int{}
	for _, effect := range allEffects {
		operationCounts[effect]++
	}
	if operationCounts[EffectEvidenceObserve] != 1 || len(allEffects) != len(expected)+1 {
		t.Fatalf("read-only evidence operation inventory mismatch: count=%d total=%d", operationCounts[EffectEvidenceObserve], len(allEffects))
	}
}

func TestEverySideEffectUsesOneShotAmbiguityReconciliation(t *testing.T) {
	faults := []FaultMode{
		FaultCrashAfterAuthorization, FaultCrashAfterPreparation, FaultCrashAfterEffect,
		FaultHTTP408BeforeEffect, FaultHTTP408AfterEffect,
		FaultEOFBeforeEffect, FaultEOFAfterEffect,
		Fault5xxBeforeEffect, Fault5xxAfterEffect,
	}
	ordinal := 100
	for _, effect := range allEffects {
		for _, fault := range faults {
			ordinal++
			t.Run(string(effect)+"/"+string(fault), func(t *testing.T) {
				oracle := NewCrashOracle()
				engine := crashEngine(t, oracle)
				request := testRequest(effect, ordinal)
				_, err := engine.Mutate(context.Background(), request, issuedTestAuth(engine, request, MutationCall, ordinal*10), fault)
				if !IsAmbiguousError(err) {
					t.Fatalf("mutation error = %v, want ambiguous/crash", err)
				}
				beforeStatusCalls := oracle.ApplyCalls(request.key())
				if fault == FaultCrashAfterAuthorization || fault == FaultCrashAfterPreparation {
					if beforeStatusCalls != 0 {
						t.Fatalf("mutation executed after authorization crash: %d", beforeStatusCalls)
					}
				} else if beforeStatusCalls != 1 {
					t.Fatalf("underlying mutation calls = %d, want 1", beforeStatusCalls)
				}
				receipt, statusErr := engine.Status(context.Background(), request, issuedTestAuth(engine, request, StatusCall, ordinal*10+1))
				applied := fault.applyEffect()
				continuableMarker := effect == EffectMarkerCAS && fault != FaultCrashAfterAuthorization && !applied
				if applied || continuableMarker {
					if statusErr != nil || receipt.ObservedCount != 1 {
						t.Fatalf("applied ambiguity did not reconcile: receipt=%+v err=%v", receipt, statusErr)
					}
				} else if effect == EffectMarkerCAS && fault == FaultCrashAfterAuthorization {
					if !errors.Is(statusErr, ErrQuarantined) {
						t.Fatalf("pre-wire marker status = %v, want quarantine", statusErr)
					}
				} else if !errors.Is(statusErr, ErrNotObserved) {
					t.Fatalf("unapplied ambiguity status = %v, want not observed", statusErr)
				}
				expectedCalls := beforeStatusCalls
				if continuableMarker {
					expectedCalls++
				}
				if got := oracle.ApplyCalls(request.key()); got != expectedCalls {
					t.Fatalf("status wire calls: got=%d want=%d", got, expectedCalls)
				}
				_, retryErr := engine.Mutate(context.Background(), request, issuedTestAuth(engine, request, MutationCall, ordinal*10+2), FaultNone)
				if applied || continuableMarker {
					if retryErr != nil {
						t.Fatalf("completed identical request was not idempotent: %v", retryErr)
					}
				} else if !errors.Is(retryErr, ErrQuarantined) {
					t.Fatalf("zero-observed request was repeatable: %v", retryErr)
				}
				if got := oracle.ApplyCalls(request.key()); got != expectedCalls {
					t.Fatalf("mutation was blindly repeated: want=%d got=%d", expectedCalls, got)
				}
			})
		}
	}
}

func TestMarkerContinuationRequiresProtectedPredecessorProof(t *testing.T) {
	oracle := NewCrashOracle()
	engine := crashEngine(t, oracle)
	request := testRequest(EffectMarkerCAS, 899)
	if _, err := engine.Mutate(context.Background(), request, issuedTestAuth(engine, request, MutationCall, 8990), FaultHTTP408BeforeEffect); !errors.Is(err, ErrReconcileRequired) {
		t.Fatalf("initial marker ambiguity: %v", err)
	}
	oracle.ForceMarkerPredecessorUnchanged(request.key(), false)
	if _, err := engine.Status(context.Background(), request, issuedTestAuth(engine, request, StatusCall, 8991)); !errors.Is(err, ErrNotObserved) {
		t.Fatalf("provider-side predecessor drift accepted: %v", err)
	}
	snapshot := oracle.Snapshot()
	record := snapshot.Records[request.key()]
	if record.State != MutationQuarantined || record.ContinuationClaimed || oracle.ApplyCalls(request.key()) != 1 {
		t.Fatalf("predecessor drift did not fail closed: record=%+v calls=%d", record, oracle.ApplyCalls(request.key()))
	}
}

func TestCompletedMutationIsExactlyIdempotentWithFreshAuth(t *testing.T) {
	oracle := NewCrashOracle()
	engine := crashEngine(t, oracle)
	request := testRequest(EffectBindingInstall, 900)
	first, err := engine.Mutate(context.Background(), request, issuedTestAuth(engine, request, MutationCall, 9000), FaultNone)
	if err != nil {
		t.Fatal(err)
	}
	second, err := engine.Mutate(context.Background(), request, issuedTestAuth(engine, request, MutationCall, 9001), FaultNone)
	if err != nil {
		t.Fatal(err)
	}
	if first != second || oracle.ApplyCalls(request.key()) != 1 || oracle.EffectCount(request.key()) != 1 {
		t.Fatalf("idempotency failure: first=%+v second=%+v calls=%d effects=%d", first, second, oracle.ApplyCalls(request.key()), oracle.EffectCount(request.key()))
	}
}

func TestBodyDriftQuarantinesWholeOperation(t *testing.T) {
	oracle := NewCrashOracle()
	engine := crashEngine(t, oracle)
	request := testRequest(EffectLeafIssue, 901)
	if _, err := engine.Mutate(context.Background(), request, issuedTestAuth(engine, request, MutationCall, 9010), FaultNone); err != nil {
		t.Fatal(err)
	}
	drift := request
	drift.ParametersDigest = HashString("synthetic-drift")
	if _, err := engine.Mutate(context.Background(), drift, issuedTestAuth(engine, drift, MutationCall, 9011), FaultNone); !errors.Is(err, ErrQuarantined) {
		t.Fatalf("body drift = %v, want quarantine", err)
	}
	other := request
	other.Effect = EffectLeafRevoke
	if _, err := engine.Mutate(context.Background(), other, issuedTestAuth(engine, other, MutationCall, 9012), FaultNone); !errors.Is(err, ErrQuarantined) {
		t.Fatalf("quarantined operation admitted another side effect: %v", err)
	}
	if oracle.ApplyCalls(request.key()) != 1 {
		t.Fatal("body drift repeated underlying effect")
	}
}

func TestJTIAndChallengeAreEachOneTimeAcrossCalls(t *testing.T) {
	oracle := NewCrashOracle()
	engine := crashEngine(t, oracle)
	firstRequest := testRequest(EffectBrokerCreate, 902)
	firstAuth := issuedTestAuth(engine, firstRequest, MutationCall, 9020)
	if _, err := engine.Mutate(context.Background(), firstRequest, firstAuth, FaultNone); err != nil {
		t.Fatal(err)
	}
	secondRequest := testRequest(EffectBrokerCreate, 903)
	jtiReplay := issuedTestAuth(engine, secondRequest, MutationCall, 9021).withJTI(firstAuth.jtiValue())
	if _, err := engine.Mutate(context.Background(), secondRequest, jtiReplay, FaultNone); !errors.Is(err, ErrAuthReplay) {
		t.Fatalf("JTI replay = %v", err)
	}
	challengeReplay := issuedTestAuth(engine, secondRequest, MutationCall, 9022).withChallenge(firstAuth.challengeValue())
	if _, err := engine.Mutate(context.Background(), secondRequest, challengeReplay, FaultNone); !errors.Is(err, ErrAuthReplay) {
		t.Fatalf("challenge replay = %v", err)
	}
	if oracle.ApplyCalls(secondRequest.key()) != 0 {
		t.Fatal("replayed authentication reached mutation plane")
	}
}

func TestCrashModelEngineRequiresExactIssuedChallengeBinding(t *testing.T) {
	ctx := context.Background()

	t.Run("unissued", func(t *testing.T) {
		oracle := NewCrashOracle()
		engine := crashEngine(t, oracle)
		request := testRequest(EffectBrokerCreate, 920)
		if _, err := engine.Mutate(ctx, request, testAuth(9200), FaultNone); !errors.Is(err, ErrChallengeNotIssued) {
			t.Fatalf("unissued challenge = %v", err)
		}
		if oracle.ApplyCalls(request.key()) != 0 {
			t.Fatal("unissued challenge reached the side effect")
		}
	})

	t.Run("request-and-call-kind", func(t *testing.T) {
		oracle := NewCrashOracle()
		engine := crashEngine(t, oracle)
		request := testRequest(EffectBindingInstall, 921)
		drift := testRequest(EffectBindingInstall, 922)
		bound := issuedTestAuth(engine, request, MutationCall, 9210)
		if _, err := engine.Mutate(ctx, drift, bound, FaultNone); !errors.Is(err, ErrChallengeBinding) {
			t.Fatalf("cross-request challenge = %v", err)
		}
		if _, err := engine.Mutate(ctx, request, bound, FaultNone); err != nil {
			t.Fatalf("binding mismatch consumed exact request grant: %v", err)
		}
		statusBound := issuedTestAuth(engine, request, StatusCall, 9211)
		if _, err := engine.Mutate(ctx, request, statusBound, FaultNone); !errors.Is(err, ErrChallengeBinding) {
			t.Fatalf("cross-call challenge = %v", err)
		}
		if _, err := engine.Status(ctx, request, statusBound); err != nil {
			t.Fatalf("cross-call mismatch consumed status grant: %v", err)
		}
	})

	t.Run("role-and-root", func(t *testing.T) {
		oracle := NewCrashOracle()
		writer := crashEngine(t, oracle)
		otherRoot, err := NewCrashModelEngine(oracle, WriterAuthorityRole, HashString("synthetic-other-writer-root/v2"), func() time.Time { return modelNow })
		if err != nil {
			t.Fatal(err)
		}
		observer, err := NewCrashModelEngine(oracle, ObserverAuthorityRole, crashModelWriterRoot, func() time.Time { return modelNow })
		if err != nil {
			t.Fatal(err)
		}

		rootRequest := testRequest(EffectLeafIssue, 923)
		rootBound := issuedTestAuth(writer, rootRequest, MutationCall, 9230)
		if _, err := otherRoot.Mutate(ctx, rootRequest, rootBound, FaultNone); !errors.Is(err, ErrInvalid) {
			t.Fatalf("cross-root challenge = %v", err)
		}
		if _, err := writer.Mutate(ctx, rootRequest, rootBound, FaultNone); err != nil {
			t.Fatalf("cross-root rejection consumed exact grant: %v", err)
		}

		roleRequest := testRequest(EffectLeafRevoke, 924)
		roleBound := issuedTestAuth(writer, roleRequest, MutationCall, 9240)
		if _, err := observer.Mutate(ctx, roleRequest, roleBound, FaultNone); !errors.Is(err, ErrInvalid) {
			t.Fatalf("cross-role challenge = %v", err)
		}
		if _, err := writer.Mutate(ctx, roleRequest, roleBound, FaultNone); err != nil {
			t.Fatalf("cross-role rejection consumed exact grant: %v", err)
		}
	})
}

func TestFailedMissingAndQuarantinedCallsDurablyBurnAuthentication(t *testing.T) {
	oracle := NewCrashOracle()
	engine := crashEngine(t, oracle)
	ctx := context.Background()
	missing := testRequest(EffectBrokerCreate, 701)
	auth := issuedTestAuth(engine, missing, StatusCall, 701)
	if _, err := engine.Status(ctx, missing, auth); !errors.Is(err, ErrInvalid) {
		t.Fatalf("missing status = %v", err)
	}
	if _, err := engine.Status(ctx, missing, auth); !errors.Is(err, ErrAuthReplay) {
		t.Fatalf("missing status did not burn auth: %v", err)
	}

	request := testRequest(EffectBrokerCreate, 702)
	if _, err := engine.Mutate(ctx, request, issuedTestAuth(engine, request, MutationCall, 702), FaultDefinitiveRejection); !errors.Is(err, ErrQuarantined) {
		t.Fatalf("definitive rejection = %v", err)
	}
	quarantineAuth := issuedTestAuth(engine, request, MutationCall, 703)
	if _, err := engine.Mutate(ctx, request, quarantineAuth, FaultNone); !errors.Is(err, ErrQuarantined) {
		t.Fatalf("quarantined call = %v", err)
	}
	if _, err := engine.Mutate(ctx, request, quarantineAuth, FaultNone); !errors.Is(err, ErrAuthReplay) {
		t.Fatalf("quarantined call did not burn auth: %v", err)
	}
}

func TestSameOperationGenerationCannotChangeEffectClass(t *testing.T) {
	oracle := NewCrashOracle()
	engine := crashEngine(t, oracle)
	ctx := context.Background()
	request := testRequest(EffectBrokerCreate, 704)
	if _, err := engine.Mutate(ctx, request, issuedTestAuth(engine, request, MutationCall, 704), FaultNone); err != nil {
		t.Fatal(err)
	}
	drift := request
	drift.Effect = EffectBrokerDelete
	drift.ParametersDigest = HashString("synthetic-effect-drift")
	if _, err := engine.Mutate(ctx, drift, issuedTestAuth(engine, drift, MutationCall, 705), FaultNone); !errors.Is(err, ErrQuarantined) {
		t.Fatalf("same operation changed effect: %v", err)
	}
	if oracle.ApplyCalls(drift.key()) != 0 {
		t.Fatal("drifted effect reached executor")
	}
}

func TestConcurrentIdenticalRequestsExecuteEffectOnce(t *testing.T) {
	oracle := NewCrashOracle()
	engine := crashEngine(t, oracle)
	request := testRequest(EffectTrustedSourceAdd, 904)
	var failures atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := engine.Mutate(context.Background(), request, issuedTestAuth(engine, request, MutationCall, 90400+i), FaultNone); err != nil {
				failures.Add(1)
			}
		}(i)
	}
	wg.Wait()
	if failures.Load() != 0 {
		t.Fatalf("concurrent identical requests failed: %d", failures.Load())
	}
	if oracle.ApplyCalls(request.key()) != 1 || oracle.EffectCount(request.key()) != 1 {
		t.Fatalf("concurrency repeated effect: calls=%d count=%d", oracle.ApplyCalls(request.key()), oracle.EffectCount(request.key()))
	}
}

type reconstructedObserveBarrier struct {
	*CrashOracle
	target  string
	mu      sync.Mutex
	arrived int
	both    chan struct{}
	release chan struct{}
}

func newReconstructedObserveBarrier(oracle *CrashOracle, target string) *reconstructedObserveBarrier {
	return &reconstructedObserveBarrier{
		CrashOracle: oracle,
		target:      target,
		both:        make(chan struct{}),
		release:     make(chan struct{}),
	}
}

func (b *reconstructedObserveBarrier) Observe(ctx context.Context, key string, effect Effect, body Digest) (int, error) {
	count, err := b.CrashOracle.Observe(ctx, key, effect, body)
	if err != nil || key != b.target {
		return count, err
	}
	b.mu.Lock()
	b.arrived++
	arrival := b.arrived
	if arrival == 2 {
		close(b.both)
	}
	b.mu.Unlock()
	if arrival <= 2 {
		select {
		case <-b.release:
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}
	return count, nil
}

func TestReconstructedEnginesCannotQuarantineConcurrentCompletion(t *testing.T) {
	oracle := NewCrashOracle()
	request := testRequest(EffectMarkerCAS, 929)
	first := crashEngine(t, oracle)
	second := crashEngine(t, oracle)
	barrier := newReconstructedObserveBarrier(oracle, request.key())
	first.executor = barrier
	second.executor = barrier

	if _, err := first.Mutate(context.Background(), request, issuedTestAuth(first, request, MutationCall, 9290), FaultHTTP408BeforeEffect); !errors.Is(err, ErrReconcileRequired) {
		t.Fatalf("initial ambiguity = %v", err)
	}
	firstAuth := issuedTestAuth(first, request, StatusCall, 9291)
	secondAuth := issuedTestAuth(second, request, StatusCall, 9292)
	type result struct {
		receipt EffectReceipt
		err     error
	}
	results := make(chan result, 2)
	go func() {
		receipt, err := first.Status(context.Background(), request, firstAuth)
		results <- result{receipt: receipt, err: err}
	}()
	go func() {
		receipt, err := second.Status(context.Background(), request, secondAuth)
		results <- result{receipt: receipt, err: err}
	}()
	select {
	case <-barrier.both:
		close(barrier.release)
	case <-time.After(5 * time.Second):
		t.Fatal("reconstructed status calls did not reach the shared observation barrier")
	}
	firstResult, secondResult := <-results, <-results
	successes := 0
	for _, got := range []result{firstResult, secondResult} {
		if got.err == nil {
			successes++
			if got.receipt.ObservedCount != 1 {
				t.Fatalf("successful receipt = %+v", got.receipt)
			}
			continue
		}
		if !errors.Is(got.err, ErrReconcileRequired) {
			t.Fatalf("concurrent reconstructed status = %v", got.err)
		}
	}
	if successes == 0 {
		t.Fatal("neither reconstructed controller completed reconciliation")
	}
	snapshot := oracle.Snapshot()
	record := snapshot.Records[request.key()]
	if record.State != MutationCompleted || snapshot.QuarantinedOps[operationKey(request)] || oracle.EffectCount(request.key()) != 1 {
		t.Fatalf("winner completion was overwritten: record=%+v quarantined=%v effects=%d", record, snapshot.QuarantinedOps[operationKey(request)], oracle.EffectCount(request.key()))
	}
}

func TestCrashStateSurvivesControllerRestartWithinOracle(t *testing.T) {
	oracle := NewCrashOracle()
	request := testRequest(EffectBrokerDelete, 905)
	firstEngine := crashEngine(t, oracle)
	if _, err := firstEngine.Mutate(context.Background(), request, issuedTestAuth(firstEngine, request, MutationCall, 9050), FaultCrashAfterEffect); !errors.Is(err, ErrSimulatedCrash) {
		t.Fatal(err)
	}
	secondEngine := crashEngine(t, oracle)
	if _, err := secondEngine.Mutate(context.Background(), request, issuedTestAuth(secondEngine, request, MutationCall, 9051), FaultNone); !errors.Is(err, ErrReconcileRequired) {
		t.Fatalf("restart blindly repeated mutation: %v", err)
	}
	receipt, err := secondEngine.Status(context.Background(), request, issuedTestAuth(secondEngine, request, StatusCall, 9052))
	if err != nil || receipt.ObservedCount != 1 {
		t.Fatalf("restart reconciliation: receipt=%+v err=%v", receipt, err)
	}
	if oracle.ApplyCalls(request.key()) != 1 {
		t.Fatalf("delete repeated after crash: %d", oracle.ApplyCalls(request.key()))
	}
}

func TestCrashModelObservationCannotCompleteBeforeWireEligibility(t *testing.T) {
	for index, fault := range []FaultMode{FaultCrashAfterAuthorization, FaultCrashAfterPreparation} {
		t.Run(string(fault), func(t *testing.T) {
			oracle := NewCrashOracle()
			engine := crashEngine(t, oracle)
			request := testRequest(EffectCredentialInstall, 930+index)
			if _, err := engine.Mutate(context.Background(), request, issuedTestAuth(engine, request, MutationCall, 9300+index*10), fault); !errors.Is(err, ErrSimulatedCrash) {
				t.Fatalf("setup fault = %v", err)
			}
			// Model a stale or injected provider match that was not created by this
			// operation. It cannot become completion authority before both fences.
			oracle.ForceEffectCount(request.key(), 1)
			if _, err := engine.Status(context.Background(), request, issuedTestAuth(engine, request, StatusCall, 9301+index*10)); !errors.Is(err, ErrQuarantined) {
				t.Fatalf("pre-wire observation = %v, want quarantine", err)
			}
			record := oracle.Snapshot().Records[request.key()]
			if record.State != MutationQuarantined || record.Receipt.ObservedCount != 0 || oracle.ApplyCalls(request.key()) != 0 {
				t.Fatalf("pre-wire observation fabricated completion: record=%+v calls=%d", record, oracle.ApplyCalls(request.key()))
			}
		})
	}
}

func TestStatusQuarantinesMultipleObservedEffects(t *testing.T) {
	oracle := NewCrashOracle()
	engine := crashEngine(t, oracle)
	request := testRequest(EffectForkPOST, 906)
	if _, err := engine.Mutate(context.Background(), request, issuedTestAuth(engine, request, MutationCall, 9060), FaultHTTP408AfterEffect); !errors.Is(err, ErrReconcileRequired) {
		t.Fatal(err)
	}
	oracle.ForceEffectCount(request.key(), 2)
	if _, err := engine.Status(context.Background(), request, issuedTestAuth(engine, request, StatusCall, 9061)); !errors.Is(err, ErrMultipleObserved) {
		t.Fatalf("multiple fork candidates = %v", err)
	}
	if _, err := engine.Mutate(context.Background(), request, issuedTestAuth(engine, request, MutationCall, 9062), FaultNone); !errors.Is(err, ErrQuarantined) {
		t.Fatalf("multiple-observed operation remained mutable: %v", err)
	}
}
