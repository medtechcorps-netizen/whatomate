package protocol

import (
	"bytes"
	"context"
	"crypto/ed25519"
	cryptorand "crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type testRawVerifier struct {
	calls          atomic.Int32
	prod           bool
	alter          bool
	binding        RequestVerifierBinding
	mutateIdentity func(*OIDCIdentity)
}

type deterministicEntropy struct {
	mu   sync.Mutex
	next uint64
}

func (r *deterministicEntropy) Read(dst []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.next++
	for index := range dst {
		shift := uint((index % 8) * 8)
		dst[index] = byte(r.next>>shift) ^ byte(index*31+17)
	}
	return len(dst), nil
}

func (v *testRawVerifier) Guarantees() RequestVerifierGuarantees {
	return RequestVerifierGuarantees{ValidatesRawJWT: true, ValidatesCanonical: true, PinsIssuerAndJWKSURI: true, BoundedJWKSRefresh: true, TestOnly: !v.prod}
}

func newTestRawVerifier(role string, signer ReceiptSigner) *testRawVerifier {
	binding := RequestVerifierBinding{
		Role: role, Audience: "urn:rereply:synthetic-writer",
		Subject: "repo:synthetic-owner/synthetic-repo:environment:synthetic-writer", Environment: "synthetic-writer",
		WorkflowRef: "synthetic-owner/synthetic-repo/.github/workflows/synthetic.tmpl@refs/heads/main",
		WorkflowSHA: strings.Repeat("a", 40), Workflow: "synthetic-recovery-boundary",
		AuthorityRootSHA256: sha256.Sum256(signer.PublicKey()),
	}
	if role == RoleObserver {
		binding.Audience = "urn:rereply:synthetic-observer"
		binding.Subject = "repo:synthetic-owner/synthetic-repo:environment:synthetic-observer"
		binding.Environment = "synthetic-observer"
		binding.WorkflowRef = "synthetic-owner/synthetic-repo/.github/workflows/synthetic-observer.tmpl@refs/heads/main"
		binding.Workflow = "synthetic-recovery-observer"
	}
	return &testRawVerifier{binding: binding}
}

func (v *testRawVerifier) Binding() RequestVerifierBinding {
	if v == nil {
		return RequestVerifierBinding{}
	}
	return v.binding
}

func (v *testRawVerifier) VerifyRequest(_ context.Context, rawJWT string, canonicalEnvelope, canonicalBody []byte, method string) (verifiedRequest, error) {
	v.calls.Add(1)
	if !strings.HasPrefix(rawJWT, "synthetic.raw.jwt.") {
		return verifiedRequest{}, errors.New("test-only raw JWT verifier rejected token")
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
	if envelope.Method != method || envelope.OperationBodySHA256 != SHA256Hex(bodyDigest) {
		return verifiedRequest{}, errors.New("test verifier endpoint/body binding mismatch")
	}
	envelopeDigest, err := AuthEnvelopeDigest(canonicalEnvelope)
	if err != nil {
		return verifiedRequest{}, err
	}
	issuedAt := time.Date(2026, 1, 2, 3, 3, 6, 0, time.UTC)
	binding := v.Binding()
	request := verifiedRequest{
		body: body, bodyDigest: bodyDigest, envelope: envelope, envelopeDigest: envelopeDigest,
		identity: OIDCIdentity{
			Issuer: "https://synthetic.invalid", Audience: binding.Audience,
			Subject: binding.Subject,
			Actor:   "synthetic-actor", ActorID: "7007", BaseRef: "", HeadRef: "",
			Repository: "synthetic-owner/synthetic-repo", RepositoryID: "1001",
			RepositoryOwner: "synthetic-owner", RepositoryOwnerID: "2002", RepositoryVisibility: "private",
			Environment: binding.Environment, Ref: "refs/heads/main", RefType: "branch",
			WorkflowRef: binding.WorkflowRef,
			WorkflowSHA: binding.WorkflowSHA, Workflow: binding.Workflow, SHA: binding.WorkflowSHA,
			EventName: "workflow_dispatch", RunID: "3003", RunNumber: "4004", RunAttempt: "1",
			RunnerEnvironment: "github-hosted", JTI: envelope.JTI,
			IssuedAt: issuedAt, NotBefore: issuedAt, ExpiresAt: issuedAt.Add(2 * time.Minute),
		},
	}
	if v.mutateIdentity != nil {
		v.mutateIdentity(&request.identity)
	}
	if v.alter {
		request.body.Phase = "tampered"
	}
	return request, nil
}

type testBroker struct {
	mu                        sync.Mutex
	private                   ed25519.PrivateKey
	binding                   BrokerBinding
	prod                      bool
	executionClaims           map[[32]byte][32]byte
	continuationClaims        map[[32]byte]testContinuationRecord
	executionWireCalls        int
	continuationWireCalls     int
	failExecutionAfterWire    int
	failContinuationAfterWire int
	continuationOutcome       string
	afterContinuationWire     func()
}

type testContinuationRecord struct {
	Fingerprint [32]byte
	Readback    BrokerReadback
}

func newTestBroker(role string) *testBroker {
	seed := [32]byte{4, 3, 2, 1}
	private := ed25519.NewKeyFromSeed(seed[:])
	return &testBroker{private: private, executionClaims: make(map[[32]byte][32]byte), continuationClaims: make(map[[32]byte]testContinuationRecord), binding: BrokerBinding{
		Role: role, IdentitySHA256: strings.Repeat("b", 64), MTLSPeerSHA256: strings.Repeat("c", 64),
		PublicKey:            append(ed25519.PublicKey(nil), private.Public().(ed25519.PublicKey)...),
		ProtectedPredecessor: &ProtectedPredecessor{Kind: "empty"},
	}}
}

func (b *testBroker) Guarantees() BrokerGuarantees {
	return BrokerGuarantees{
		SeparateSubstrate: true, AuthenticatedReadback: true, InternallyGeneratedMarker: true, ExactMarkerBytes: 32,
		RolePhaseGenerationBound: true, OperationDigestBound: true, MutualTLSBound: true, CapabilityBound: true,
		DurableExecutionClaims: true, DurableContinuationClaims: true,
		ProviderConfined: b.prod, NoAutomaticRetries: true, TestOnly: !b.prod,
	}
}

func brokerExecutionFingerprint(claim brokerExecutionClaim) [32]byte {
	capabilityDigest := signedRecordDigest("broker-capability-record", claim.Capability.Payload, claim.Capability.Signature)
	payload := make([]byte, 0, 32*5)
	payload = append(payload, claim.OperationBodySHA256[:]...)
	payload = append(payload, claim.PredecessorSHA256[:]...)
	payload = append(payload, claim.PreWireFenceSHA256[:]...)
	payload = append(payload, claim.ExecutionSHA256[:]...)
	payload = append(payload, capabilityDigest[:]...)
	return domainHash("test-broker-execution-submission", payload)
}

func (b *testBroker) executeOnce(_ context.Context, claim brokerExecutionClaim) (bool, error) {
	if claim.OperationBodySHA256 == ([32]byte{}) || claim.PredecessorSHA256 == ([32]byte{}) ||
		claim.PreWireFenceSHA256 == ([32]byte{}) || claim.ExecutionSHA256 == ([32]byte{}) ||
		len(claim.Capability.Payload) == 0 || len(claim.Capability.Signature) == 0 {
		return false, errors.New("test broker rejected incomplete execution claim")
	}
	var capability BrokerCapabilityPayload
	if err := decodeCanonical(claim.Capability.Payload, MaxBrokerPayloadBytes, &capability); err != nil {
		return false, err
	}
	if capability.BrokerRole != b.binding.Role || capability.BrokerIdentitySHA256 != b.binding.IdentitySHA256 ||
		capability.MTLSPeerSHA256 != b.binding.MTLSPeerSHA256 ||
		capability.OperationBodySHA256 != SHA256Hex(claim.OperationBodySHA256) ||
		capability.ExecutionClaimSHA256 != SHA256Hex(claim.ExecutionSHA256) {
		return false, errors.New("test broker rejected drifted execution claim binding")
	}
	fingerprint := brokerExecutionFingerprint(claim)
	b.mu.Lock()
	defer b.mu.Unlock()
	if prior, exists := b.executionClaims[claim.ExecutionSHA256]; exists {
		if prior != fingerprint {
			return false, errors.New("test broker quarantined execution claim reuse with drift")
		}
		return false, nil
	}
	b.executionClaims[claim.ExecutionSHA256] = fingerprint
	b.executionWireCalls++
	if b.failExecutionAfterWire > 0 {
		b.failExecutionAfterWire--
		return false, errors.New("synthetic broker transport loss after execution wire")
	}
	return true, nil
}

func brokerContinuationFingerprint(claim brokerContinuationClaim) [32]byte {
	readbackDigest := signedRecordDigest("broker-readback-record", claim.Readback.Payload, claim.Readback.Signature)
	sealedDigest := sha256.Sum256(claim.Readback.SealedReconciliation)
	payload := make([]byte, 0, 32*7)
	payload = append(payload, claim.OperationBodySHA256[:]...)
	payload = append(payload, claim.PreWireFenceSHA256[:]...)
	payload = append(payload, claim.ExecutionSHA256[:]...)
	payload = append(payload, claim.AmbiguitySHA256[:]...)
	payload = append(payload, claim.ContinuationSHA256[:]...)
	payload = append(payload, readbackDigest[:]...)
	payload = append(payload, sealedDigest[:]...)
	return domainHash("test-broker-continuation-submission", payload)
}

func (b *testBroker) continueOnce(_ context.Context, claim brokerContinuationClaim) (BrokerReadback, error) {
	if claim.OperationBodySHA256 == ([32]byte{}) || claim.PreWireFenceSHA256 == ([32]byte{}) ||
		claim.ExecutionSHA256 == ([32]byte{}) || claim.AmbiguitySHA256 == ([32]byte{}) ||
		claim.ContinuationSHA256 == ([32]byte{}) {
		return BrokerReadback{}, errors.New("test broker rejected incomplete continuation claim")
	}
	if signedRecordDigest("broker-readback-record", claim.Readback.Payload, claim.Readback.Signature) != claim.AmbiguitySHA256 {
		return BrokerReadback{}, errors.New("test broker rejected continuation with the wrong ambiguity record")
	}
	if err := VerifyCanonical(b.binding.PublicKey, DomainBrokerReadback, claim.Readback.Payload, claim.Readback.Signature); err != nil {
		return BrokerReadback{}, err
	}
	var payload BrokerReadbackPayload
	if err := decodeCanonical(claim.Readback.Payload, MaxBrokerPayloadBytes, &payload); err != nil {
		return BrokerReadback{}, err
	}
	if payload.Outcome != BrokerOutcomeUnchangedPredecessor || payload.ContinuationClaimSHA256 != "" ||
		payload.OperationBodySHA256 != SHA256Hex(claim.OperationBodySHA256) ||
		payload.ExecutionClaimSHA256 != SHA256Hex(claim.ExecutionSHA256) {
		return BrokerReadback{}, errors.New("test broker rejected drifted continuation input")
	}
	fingerprint := brokerContinuationFingerprint(claim)
	b.mu.Lock()
	defer b.mu.Unlock()
	if prior, exists := b.continuationClaims[claim.ContinuationSHA256]; exists {
		if prior.Fingerprint != fingerprint {
			return BrokerReadback{}, errors.New("test broker quarantined continuation claim reuse with drift")
		}
		return cloneBrokerReadback(prior.Readback), nil
	}
	payload.Outcome = BrokerOutcomeCommitted
	payload.ContinuationClaimSHA256 = SHA256Hex(claim.ContinuationSHA256)
	payload.BrokerActionAtUTC = "2026-01-02T03:04:08Z"
	if b.continuationOutcome != "" {
		payload.Outcome = b.continuationOutcome
	}
	canonical, err := json.Marshal(payload)
	if err != nil {
		return BrokerReadback{}, err
	}
	signature, err := SignCanonical(b.private, DomainBrokerReadback, canonical)
	if err != nil {
		return BrokerReadback{}, err
	}
	result := BrokerReadback{
		Payload: canonical, Signature: signature,
		SealedReconciliation: append([]byte(nil), claim.Readback.SealedReconciliation...),
	}
	b.continuationClaims[claim.ContinuationSHA256] = testContinuationRecord{Fingerprint: fingerprint, Readback: cloneBrokerReadback(result)}
	b.continuationWireCalls++
	if b.afterContinuationWire != nil {
		callback := b.afterContinuationWire
		b.afterContinuationWire = nil
		callback()
	}
	if b.failContinuationAfterWire > 0 {
		b.failContinuationAfterWire--
		return BrokerReadback{}, errors.New("synthetic broker transport loss after continuation wire")
	}
	return cloneBrokerReadback(result), nil
}

func (b *testBroker) wireCounts() (int, int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.executionWireCalls, b.continuationWireCalls
}

func (b *testBroker) Binding() BrokerBinding {
	result := b.binding
	result.PublicKey = append(ed25519.PublicKey(nil), b.binding.PublicKey...)
	return result
}

type productionStore struct{ *FaultInjectingStore }

func (s productionStore) Guarantees() DurabilityGuarantees {
	return DurabilityGuarantees{Transactional: true, CrashDurable: true, CASFencing: true, AppendOnlyAudit: true, EncryptionAtRest: true, BackupRecovery: true, KeyConfinement: true, CryptographicErase: true}
}

type productionSigner struct{ *testEd25519Signer }

func (s productionSigner) Guarantees() SignerGuarantees {
	return SignerGuarantees{RoleSeparated: true, ProviderConfined: true, Deterministic: true, NoAutomaticRetries: true}
}

type corruptingSigner struct {
	*testEd25519Signer
	domain string
}

type failingSigner struct {
	*testEd25519Signer
	domain string
}

type faultArmingSigner struct {
	*testEd25519Signer
	store  *FaultInjectingStore
	domain string
	fault  FaultPoint
}

func (s *faultArmingSigner) Sign(ctx context.Context, domain string, payload []byte) ([]byte, error) {
	signature, err := s.testEd25519Signer.Sign(ctx, domain, payload)
	if err == nil && domain == s.domain {
		s.store.Inject(s.fault, 1)
	}
	return signature, err
}

func (s *failingSigner) Sign(ctx context.Context, domain string, payload []byte) ([]byte, error) {
	if domain == s.domain {
		return nil, errors.New("synthetic signer failure")
	}
	return s.testEd25519Signer.Sign(ctx, domain, payload)
}

func (s *corruptingSigner) Sign(ctx context.Context, domain string, payload []byte) ([]byte, error) {
	signature, err := s.testEd25519Signer.Sign(ctx, domain, payload)
	if err == nil && domain == s.domain {
		signature[0] ^= 1
	}
	return signature, err
}

func testController(t *testing.T, store *FaultInjectingStore) (*Controller, *testEd25519Signer, *testRawVerifier, *testBroker) {
	t.Helper()
	signer := newTestEd25519Signer([32]byte{9, 8, 7, 6})
	verifier := newTestRawVerifier(RoleWriter, signer)
	broker := newTestBroker(RoleWriter)
	controller, err := newTestControllerWithEntropy(RoleWriter, store, signer, verifier, broker, &deterministicEntropy{})
	if err != nil {
		t.Fatal(err)
	}
	return controller, signer, verifier, broker
}

func testCall(t *testing.T, controller *Controller, body []byte, method string, ordinal int) (string, []byte) {
	t.Helper()
	bodyDigest := mustOperationDigest(t, body)
	challenge, err := controller.IssueChallenge(context.Background(), body, method)
	if err != nil {
		t.Fatal(err)
	}
	envelope := syntheticAuth(bodyDigest, method, fmt.Sprintf("%032x", ordinal*2+1), challenge)
	canonical, err := MarshalAuthEnvelope(envelope)
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("synthetic.raw.jwt.%d", ordinal), canonical
}

func beginOperation(t *testing.T, controller *Controller, body []byte, ordinal int) BeginDecision {
	t.Helper()
	rawJWT, envelope := testCall(t, controller, body, MethodAuthorize, ordinal)
	decision, err := controller.BeginMutation(context.Background(), rawJWT, envelope, body)
	if err != nil {
		t.Fatal(err)
	}
	return decision
}

func markerFixture(offset byte) [32]byte {
	var value [32]byte
	for index := range value {
		value[index] = byte(index) + offset
	}
	return value
}

func makeReadback(t *testing.T, broker *testBroker, body []byte, decision BeginDecision, marker [32]byte, sealed []byte, mutate func(*BrokerReadbackPayload)) BrokerReadback {
	t.Helper()
	decodedBody, err := DecodeOperationBody(body)
	if err != nil {
		t.Fatal(err)
	}
	bodyDigest := mustOperationDigest(t, body)
	markerDigest := sha256.Sum256(marker[:])
	sealedDigest := sha256.Sum256(sealed)
	payload := BrokerReadbackPayload{
		Schema: BrokerReadbackSchema, BrokerRole: broker.binding.Role, Phase: decodedBody.Phase,
		Generation: decodedBody.Generation, OperationID: decodedBody.OperationID,
		OperationBodySHA256: SHA256Hex(bodyDigest), CapabilitySHA256: decision.Snapshot.CapabilitySHA256,
		BrokerIdentitySHA256: broker.binding.IdentitySHA256, MTLSPeerSHA256: broker.binding.MTLSPeerSHA256,
		Outcome:              BrokerOutcomeCommitted,
		ExecutionClaimSHA256: decision.Snapshot.ExecutionClaimSHA256,
		BrokerActionAtUTC:    "2026-01-02T03:04:07Z",
		MarkerMaterialBytes:  32, MarkerSHA256: SHA256Hex(markerDigest), SealedReconciliationSHA256: SHA256Hex(sealedDigest),
	}
	payload.ProtectedPredecessorKind, payload.ProtectedPredecessorSHA256, _, err = broker.binding.ProtectedPredecessor.fields()
	if err != nil {
		t.Fatal(err)
	}
	if mutate != nil {
		mutate(&payload)
	}
	canonical, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	signature, err := SignCanonical(broker.private, DomainBrokerReadback, canonical)
	if err != nil {
		t.Fatal(err)
	}
	return BrokerReadback{Payload: canonical, Signature: signature, SealedReconciliation: append([]byte(nil), sealed...)}
}

func acceptReadback(t *testing.T, controller *Controller, broker *testBroker, body []byte, decision BeginDecision, ordinal int) OperationSnapshot {
	t.Helper()
	readback := makeReadback(t, broker, body, decision, markerFixture(0), []byte("synthetic-opaque-sealed-reconciliation"), nil)
	rawJWT, envelope := testCall(t, controller, body, MethodBrokerReadback, ordinal)
	snapshot, err := controller.AcceptBrokerReadback(context.Background(), rawJWT, envelope, body, readback)
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func TestProductionControllerUnavailableEvenForSelfCertifiedAdapters(t *testing.T) {
	store := productionStore{NewFaultInjectingStore()}
	signer := productionSigner{newTestEd25519Signer([32]byte{1})}
	verifier := &testRawVerifier{prod: true}
	broker := newTestBroker(RoleWriter)
	broker.prod = true
	if _, err := NewController(RoleWriter, store, signer, verifier, broker); !errors.Is(err, ErrGateAProductionUnavailable) {
		t.Fatalf("self-certified adapters manufactured production authority: %v", err)
	}
	tests := []struct {
		name     string
		store    DurableStore
		signer   ReceiptSigner
		verifier RawRequestVerifier
		broker   BrokerTrust
	}{
		{"store", nil, signer, verifier, broker}, {"signer", store, nil, verifier, broker},
		{"verifier", store, signer, nil, broker}, {"broker", store, signer, verifier, nil},
		{"test store", NewFaultInjectingStore(), signer, verifier, broker},
		{"test signer", store, newTestEd25519Signer([32]byte{1}), verifier, broker},
		{"test verifier", store, signer, &testRawVerifier{}, broker},
		{"test broker", store, signer, verifier, newTestBroker(RoleWriter)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := NewController(RoleWriter, test.store, test.signer, test.verifier, test.broker); !errors.Is(err, ErrGateAProductionUnavailable) {
				t.Fatalf("Gate-A production constructor did not fail closed: %v", err)
			}
		})
	}
}

func TestGenericControllerRejectsObserverUntilDurableRecoveryAdmissionAPIExists(t *testing.T) {
	t.Run("production observer", func(t *testing.T) {
		store := productionStore{NewFaultInjectingStore()}
		signer := productionSigner{newTestEd25519Signer([32]byte{1})}
		verifier := &testRawVerifier{prod: true}
		broker := newTestBroker(RoleObserver)
		broker.prod = true
		if _, err := NewController(RoleObserver, store, signer, verifier, broker); !errors.Is(err, ErrGateAProductionUnavailable) {
			t.Fatalf("generic production constructor synthesized observer authority: %v", err)
		}
	})

	t.Run("test observer", func(t *testing.T) {
		if _, err := newTestController(
			RoleObserver,
			NewFaultInjectingStore(),
			newTestEd25519Signer([32]byte{1}),
			&testRawVerifier{},
			newTestBroker(RoleObserver),
		); err == nil || !strings.Contains(err.Error(), "distinct durable recovery-admission-bound API") {
			t.Fatalf("generic test constructor synthesized observer authority: %v", err)
		}
	})

	t.Run("only test writer remains supported", func(t *testing.T) {
		store := productionStore{NewFaultInjectingStore()}
		signer := productionSigner{newTestEd25519Signer([32]byte{1})}
		verifier := &testRawVerifier{prod: true}
		broker := newTestBroker(RoleWriter)
		broker.prod = true
		productionController, err := NewController(RoleWriter, store, signer, verifier, broker)
		if productionController != nil || !errors.Is(err, ErrGateAProductionUnavailable) {
			t.Fatalf("Gate A constructed writer production authority: controller=%#v err=%v", productionController, err)
		}

		testSigner := newTestEd25519Signer([32]byte{1})
		testController, err := newTestController(
			RoleWriter,
			NewFaultInjectingStore(),
			testSigner,
			newTestRawVerifier(RoleWriter, testSigner),
			newTestBroker(RoleWriter),
		)
		if err != nil || testController.role != RoleWriter {
			t.Fatalf("writer test constructor regressed: controller=%#v err=%v", testController, err)
		}
	})
}

func TestControllerPinsRawVerifierBindingAtComposition(t *testing.T) {
	t.Run("canonical binding", func(t *testing.T) {
		signer := newTestEd25519Signer([32]byte{9, 8, 7, 6})
		verifier := newTestRawVerifier(RoleWriter, signer)
		verifier.binding.Audience = ""
		if _, err := newTestController(RoleWriter, NewFaultInjectingStore(), signer, verifier, newTestBroker(RoleWriter)); err == nil || !strings.Contains(err.Error(), "audience") {
			t.Fatalf("controller accepted a noncanonical verifier binding: %v", err)
		}
	})

	t.Run("writer verifier at observer controller", func(t *testing.T) {
		signer := newTestEd25519Signer([32]byte{7, 6, 5, 4})
		verifier := newTestRawVerifier(RoleWriter, signer)
		if _, err := newTestObserverController(NewFaultInjectingStore(), signer, verifier, newTestBroker(RoleObserver)); err == nil || !strings.Contains(err.Error(), "role does not match") {
			t.Fatalf("observer controller accepted a writer verifier: %v", err)
		}
	})

	t.Run("observer verifier at writer controller", func(t *testing.T) {
		signer := newTestEd25519Signer([32]byte{9, 8, 7, 6})
		verifier := newTestRawVerifier(RoleObserver, signer)
		if _, err := newTestController(RoleWriter, NewFaultInjectingStore(), signer, verifier, newTestBroker(RoleWriter)); err == nil || !strings.Contains(err.Error(), "role does not match") {
			t.Fatalf("writer controller accepted an observer verifier: %v", err)
		}
	})

	t.Run("authority root", func(t *testing.T) {
		signer := newTestEd25519Signer([32]byte{9, 8, 7, 6})
		otherSigner := newTestEd25519Signer([32]byte{7, 6, 5, 4})
		verifier := newTestRawVerifier(RoleWriter, otherSigner)
		if _, err := newTestController(RoleWriter, NewFaultInjectingStore(), signer, verifier, newTestBroker(RoleWriter)); err == nil || !strings.Contains(err.Error(), "authority root") {
			t.Fatalf("controller accepted a verifier bound to another authority root: %v", err)
		}
	})

	t.Run("controller snapshot", func(t *testing.T) {
		store := NewFaultInjectingStore()
		signer := newTestEd25519Signer([32]byte{9, 8, 7, 6})
		verifier := newTestRawVerifier(RoleWriter, signer)
		controller, err := newTestController(RoleWriter, store, signer, verifier, newTestBroker(RoleWriter))
		if err != nil {
			t.Fatal(err)
		}
		verifier.binding.Audience = "urn:rereply:mutated-after-composition"
		body := mustOperationJSON(t, syntheticOperation())
		rawJWT, envelope := testCall(t, controller, body, MethodAuthorize, 1)
		if _, err := controller.BeginMutation(context.Background(), rawJWT, envelope, body); err == nil || !strings.Contains(err.Error(), "immutable verifier binding") {
			t.Fatalf("controller followed a mutable alternate verifier binding: %v", err)
		}
		if _, found, err := store.Read(context.Background(), "synthetic-operation-001"); err != nil || found {
			t.Fatalf("mutable verifier binding reached durable operation state: found=%v err=%v", found, err)
		}
	})
}

func TestControllerRejectsAlternateVerifierIdentityOutsideImmutableBinding(t *testing.T) {
	for name, mutate := range map[string]func(*OIDCIdentity){
		"audience":    func(identity *OIDCIdentity) { identity.Audience = "urn:rereply:synthetic-writer-alt" },
		"subject":     func(identity *OIDCIdentity) { identity.Subject += ":alt" },
		"environment": func(identity *OIDCIdentity) { identity.Environment = "synthetic-writer-alt" },
		"workflow ref": func(identity *OIDCIdentity) {
			identity.WorkflowRef = "synthetic-owner/synthetic-repo/.github/workflows/alt.tmpl@refs/heads/main"
		},
		"workflow SHA": func(identity *OIDCIdentity) { identity.WorkflowSHA = strings.Repeat("b", 40) },
		"workflow":     func(identity *OIDCIdentity) { identity.Workflow = "synthetic-recovery-boundary-alt" },
	} {
		t.Run(name, func(t *testing.T) {
			store := NewFaultInjectingStore()
			signer := newTestEd25519Signer([32]byte{9, 8, 7, 6})
			verifier := newTestRawVerifier(RoleWriter, signer)
			verifier.mutateIdentity = mutate
			controller, err := newTestController(RoleWriter, store, signer, verifier, newTestBroker(RoleWriter))
			if err != nil {
				t.Fatal(err)
			}
			body := mustOperationJSON(t, syntheticOperation())
			rawJWT, envelope := testCall(t, controller, body, MethodAuthorize, 1)
			if _, err := controller.BeginMutation(context.Background(), rawJWT, envelope, body); err == nil || !strings.Contains(err.Error(), "immutable verifier binding") {
				t.Fatalf("alternate verifier substituted %s outside its binding: %v", name, err)
			}
			if _, found, err := store.Read(context.Background(), "synthetic-operation-001"); err != nil || found {
				t.Fatalf("%s substitution reached durable operation state: found=%v err=%v", name, found, err)
			}
		})
	}
}

func TestControllerPinsBrokerBindingAndRejectsAuthorityBrokerKeyReuse(t *testing.T) {
	t.Run("key reuse", func(t *testing.T) {
		broker := newTestBroker(RoleWriter)
		signer := &testEd25519Signer{private: append(ed25519.PrivateKey(nil), broker.private...)}
		if _, err := newTestController(RoleWriter, NewFaultInjectingStore(), signer, newTestRawVerifier(RoleWriter, signer), broker); err == nil || !strings.Contains(err.Error(), "must be distinct") {
			t.Fatalf("authority/broker key reuse was accepted: %v", err)
		}
	})

	t.Run("mutable binding", func(t *testing.T) {
		store := NewFaultInjectingStore()
		signer := newTestEd25519Signer([32]byte{9, 8, 7, 6})
		broker := newTestBroker(RoleWriter)
		originalIdentity := broker.binding.IdentitySHA256
		controller, err := newTestController(RoleWriter, store, signer, newTestRawVerifier(RoleWriter, signer), broker)
		if err != nil {
			t.Fatal(err)
		}
		replacementSeed := [32]byte{8, 7, 6, 5}
		broker.private = ed25519.NewKeyFromSeed(replacementSeed[:])
		broker.binding.IdentitySHA256 = strings.Repeat("d", 64)
		broker.binding.MTLSPeerSHA256 = strings.Repeat("e", 64)
		broker.binding.PublicKey = append(ed25519.PublicKey(nil), broker.private.Public().(ed25519.PublicKey)...)

		body := mustOperationJSON(t, syntheticOperation())
		rawJWT, envelope := testCall(t, controller, body, MethodAuthorize, 1)
		decision, err := controller.BeginMutation(context.Background(), rawJWT, envelope, body)
		if err == nil || !strings.Contains(err.Error(), "drifted execution claim binding") {
			t.Fatalf("mutable broker identity reached the provider wire: %#v %v", decision, err)
		}
		var capability BrokerCapabilityPayload
		if err := decodeCanonical(decision.Snapshot.CapabilityPayload, MaxBrokerPayloadBytes, &capability); err != nil {
			t.Fatal(err)
		}
		if capability.BrokerIdentitySHA256 != originalIdentity {
			t.Fatal("durable capability followed a mutable broker binding after construction")
		}
		if executions, continuations := broker.wireCounts(); executions != 0 || continuations != 0 {
			t.Fatalf("mutable broker binding reached a wire call: execution=%d continuation=%d", executions, continuations)
		}
	})
}

func TestAuthorityIndependentlyVerifiesRawRequestBytes(t *testing.T) {
	store := NewFaultInjectingStore()
	controller, _, verifier, _ := testController(t, store)
	body := mustOperationJSON(t, syntheticOperation())
	_, envelope := testCall(t, controller, body, MethodAuthorize, 1)
	if _, err := controller.BeginMutation(context.Background(), "", envelope, body); err == nil {
		t.Fatal("authority accepted absent raw JWT")
	}
	verifier.alter = true
	if _, err := controller.BeginMutation(context.Background(), "synthetic.raw.jwt.2", envelope, body); err == nil || !strings.Contains(err.Error(), "different from canonical") {
		t.Fatalf("authority accepted verifier-substituted parsed request: %v", err)
	}
	if verifier.calls.Load() != 1 {
		t.Fatalf("unexpected raw-verifier call count %d", verifier.calls.Load())
	}
}

func TestProtectedNilAndEmptyPredecessorsRemainDistinctInternalBindings(t *testing.T) {
	_, _, nilDigest, err := (*ProtectedPredecessor)(nil).fields()
	if err != nil {
		t.Fatal(err)
	}
	_, _, emptyDigest, err := (&ProtectedPredecessor{Kind: "empty"}).fields()
	if err != nil {
		t.Fatal(err)
	}
	if nilDigest == emptyDigest {
		t.Fatal("protected nil and explicit empty predecessor bindings collapsed")
	}
}

func TestAuthorityRejectsUnverifiableSignerOutputBeforeCommitOrErasure(t *testing.T) {
	t.Run("broker capability", func(t *testing.T) {
		store := NewFaultInjectingStore()
		signer := &corruptingSigner{testEd25519Signer: newTestEd25519Signer([32]byte{9, 8, 7, 6}), domain: DomainBrokerCapability}
		controller, err := newTestController(RoleWriter, store, signer, newTestRawVerifier(RoleWriter, signer), newTestBroker(RoleWriter))
		if err != nil {
			t.Fatal(err)
		}
		body := mustOperationJSON(t, syntheticOperation())
		rawJWT, envelope := testCall(t, controller, body, MethodAuthorize, 1)
		if _, err := controller.BeginMutation(context.Background(), rawJWT, envelope, body); err == nil || !strings.Contains(err.Error(), "self-verify broker capability") {
			t.Fatalf("unverifiable capability was accepted: %v", err)
		}
		snapshot, found, err := store.Read(context.Background(), "synthetic-operation-001")
		if err != nil || !found || snapshot.State != StatePrepared || len(snapshot.CapabilitySignature) != 0 {
			t.Fatalf("unverifiable capability escaped its durable pre-wire fence: %#v found=%v err=%v", snapshot, found, err)
		}
	})

	t.Run("terminal receipt", func(t *testing.T) {
		store := NewFaultInjectingStore()
		controller, signer, verifier, broker := testController(t, store)
		body := mustOperationJSON(t, syntheticOperation())
		decision := beginOperation(t, controller, body, 1)
		acceptReadback(t, controller, broker, body, decision, 2)

		badSigner := &corruptingSigner{testEd25519Signer: signer, domain: DomainWriterReceipt}
		badController, err := newTestController(RoleWriter, store, badSigner, verifier, broker)
		if err != nil {
			t.Fatal(err)
		}
		rawJWT, envelope := testCall(t, controller, body, MethodTerminalProof, 3)
		if _, err := badController.CommitTerminalProof(context.Background(), rawJWT, envelope, body); err == nil || !strings.Contains(err.Error(), "self-verify terminal receipt") {
			t.Fatalf("unverifiable terminal receipt was accepted: %v", err)
		}
		snapshot, found, err := store.Read(context.Background(), "synthetic-operation-001")
		if err != nil || !found || snapshot.State != StateSideEffectStarted || !snapshot.SealedReconciliationPresent || snapshot.SealedReconciliationErased {
			t.Fatalf("terminal signing failure erased material or committed terminal state: %#v found=%v err=%v", snapshot, found, err)
		}
	})
}

func TestAuthorityUsesRawRS256OIDCValidatorBeforeLedgerConsumption(t *testing.T) {
	now, expectation, _, claims, key := oidcFixture(t)
	body := mustOperationJSON(t, syntheticOperation())
	bodyDigest := mustOperationDigest(t, body)
	store := NewFaultInjectingStore()
	signer := newTestEd25519Signer([32]byte{9, 8, 7, 6})
	expectation.AuthorityRootSHA256 = sha256.Sum256(signer.PublicKey())
	provider := &staticJWKSProvider{issuer: expectation.Issuer, uri: expectation.JWKSURI, snapshots: []JWKSSnapshot{{Keys: map[string]*rsa.PublicKey{"synthetic-kid": &key.PublicKey}, FetchedAt: now}}}
	validator, err := newTestOIDCValidator(expectation, provider, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	broker := newTestBroker(RoleWriter)
	controller, err := newTestController(RoleWriter, store, signer, validator, broker)
	if err != nil {
		t.Fatal(err)
	}
	challenge, err := controller.IssueChallenge(context.Background(), body, MethodAuthorize)
	if err != nil {
		t.Fatal(err)
	}
	envelope := syntheticAuth(bodyDigest, MethodAuthorize, claims.JTI, challenge)
	authJSON, err := MarshalAuthEnvelope(envelope)
	if err != nil {
		t.Fatal(err)
	}
	token := signSyntheticJWT(t, key, jwtHeader{Algorithm: "RS256", KeyID: "synthetic-kid", Type: "JWT"}, claims)
	parts := strings.Split(token, ".")
	parts[2] = strings.Repeat("A", len(parts[2]))
	if _, err := controller.BeginMutation(context.Background(), strings.Join(parts, "."), authJSON, body); err == nil {
		t.Fatal("authority consumed a request whose raw JWT signature was invalid")
	}
	if _, found, _ := store.Read(context.Background(), "synthetic-operation-001"); found {
		t.Fatal("failed raw JWT validation reached durable operation state")
	}
	if _, err := controller.BeginMutation(context.Background(), token, authJSON, body); err != nil {
		t.Fatalf("valid raw JWT request rejected: %v", err)
	}
	if _, err := controller.BeginMutation(context.Background(), token, authJSON, body); !errors.Is(err, ErrReplay) {
		t.Fatalf("validated raw JWT/JTI/challenge replay was not rejected durably: %v", err)
	}
}

func TestObserverControllerUsesRawRS256OIDCAndRejectsValidatorSubstitution(t *testing.T) {
	now, writerExpectation, _, writerClaims, writerKey := oidcFixture(t)
	writerSigner := newTestEd25519Signer([32]byte{9, 8, 7, 6})
	writerExpectation.AuthorityRootSHA256 = sha256.Sum256(writerSigner.PublicKey())
	writerProvider := &staticJWKSProvider{
		issuer: writerExpectation.Issuer, uri: writerExpectation.JWKSURI,
		snapshots: []JWKSSnapshot{{Keys: map[string]*rsa.PublicKey{"synthetic-writer-kid": &writerKey.PublicKey}, FetchedAt: now}},
	}
	writerValidator, err := newTestOIDCValidator(writerExpectation, writerProvider, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}

	observerKey, err := rsa.GenerateKey(cryptorand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	observerSigner := newTestEd25519Signer([32]byte{7, 6, 5, 4})
	observerExpectation := writerExpectation
	observerExpectation.Role = RoleObserver
	observerExpectation.AuthorityRootSHA256 = sha256.Sum256(observerSigner.PublicKey())
	observerExpectation.Audience = "urn:rereply:synthetic-observer"
	observerExpectation.Subject = "repo:synthetic-owner/synthetic-repo:environment:synthetic-observer"
	observerExpectation.Environment = "synthetic-observer"
	observerExpectation.WorkflowRef = "synthetic-owner/synthetic-repo/.github/workflows/synthetic-observer.tmpl@refs/heads/main"
	observerExpectation.Workflow = "synthetic-recovery-observer"
	observerExpectation.RunID = "3004"
	observerExpectation.RunNumber = "4005"
	observerClaims := writerClaims
	observerClaims.Audience = observerExpectation.Audience
	observerClaims.Subject = observerExpectation.Subject
	observerClaims.Environment = observerExpectation.Environment
	observerClaims.WorkflowRef = observerExpectation.WorkflowRef
	observerClaims.Workflow = observerExpectation.Workflow
	observerClaims.RunID = observerExpectation.RunID
	observerClaims.RunNumber = observerExpectation.RunNumber
	observerClaims.JTI = "00000000-0000-4000-8000-000000000002"
	observerProvider := &staticJWKSProvider{
		issuer: observerExpectation.Issuer, uri: observerExpectation.JWKSURI,
		snapshots: []JWKSSnapshot{{Keys: map[string]*rsa.PublicKey{"synthetic-observer-kid": &observerKey.PublicKey}, FetchedAt: now}},
	}
	observerValidator, err := newTestOIDCValidator(observerExpectation, observerProvider, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}

	if _, err := newTestObserverController(NewFaultInjectingStore(), observerSigner, writerValidator, newTestBroker(RoleObserver)); err == nil || !strings.Contains(err.Error(), "role does not match") {
		t.Fatalf("observer controller accepted the writer OIDC validator: %v", err)
	}
	if _, err := newTestController(RoleWriter, NewFaultInjectingStore(), writerSigner, observerValidator, newTestBroker(RoleWriter)); err == nil || !strings.Contains(err.Error(), "role does not match") {
		t.Fatalf("writer controller accepted the observer OIDC validator: %v", err)
	}
	wrongObserverSigner := newTestEd25519Signer([32]byte{6, 5, 4, 3})
	if _, err := newTestObserverController(NewFaultInjectingStore(), wrongObserverSigner, observerValidator, newTestBroker(RoleObserver)); err == nil || !strings.Contains(err.Error(), "authority root") {
		t.Fatalf("observer controller accepted a validator bound to another authority root: %v", err)
	}

	store := NewFaultInjectingStore()
	controller, err := newTestObserverController(store, observerSigner, observerValidator, newTestBroker(RoleObserver))
	if err != nil {
		t.Fatal(err)
	}
	bodyValue := syntheticOperation()
	bodyValue.OperationID = "synthetic-observer-operation-001"
	bodyValue.Role = RoleObserver
	bodyValue.Action = ActionMarkerRead
	body := mustOperationJSON(t, bodyValue)
	bodyDigest := mustOperationDigest(t, body)
	challenge, err := controller.IssueChallenge(context.Background(), body, MethodStatus)
	if err != nil {
		t.Fatal(err)
	}
	envelope := syntheticAuth(bodyDigest, MethodStatus, observerClaims.JTI, challenge)
	authJSON, err := MarshalAuthEnvelope(envelope)
	if err != nil {
		t.Fatal(err)
	}
	token := signSyntheticJWT(t, observerKey, jwtHeader{Algorithm: "RS256", KeyID: "synthetic-observer-kid", Type: "JWT"}, observerClaims)
	if _, err := controller.Status(context.Background(), token, authJSON, body); !errors.Is(err, ErrOperationMissing) {
		t.Fatalf("observer raw JWT did not reach the authenticated status boundary: %v", err)
	}
	if _, err := controller.Status(context.Background(), token, authJSON, body); !errors.Is(err, ErrReplay) {
		t.Fatalf("observer raw JWT/JTI/challenge was not durably burned: %v", err)
	}
}

func TestIssuedBrokerCapabilityBindsRolePhaseGenerationOperationAndMTLS(t *testing.T) {
	store := NewFaultInjectingStore()
	controller, signer, _, broker := testController(t, store)
	body := mustOperationJSON(t, syntheticOperation())
	decision := beginOperation(t, controller, body, 1)
	if err := VerifyCanonical(signer.PublicKey(), DomainBrokerCapability, decision.Capability.Payload, decision.Capability.Signature); err != nil {
		t.Fatal(err)
	}
	var payload BrokerCapabilityPayload
	if err := decodeCanonical(decision.Capability.Payload, MaxBrokerPayloadBytes, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.BrokerRole != RoleWriter || payload.Phase != "baseline" || payload.Generation != 1 ||
		payload.OperationID != "synthetic-operation-001" || payload.OperationBodySHA256 != decision.Snapshot.OperationBodySHA256 ||
		payload.BrokerIdentitySHA256 != broker.binding.IdentitySHA256 || payload.MTLSPeerSHA256 != broker.binding.MTLSPeerSHA256 ||
		payload.ProtectedPredecessorKind != "empty" || payload.ProtectedPredecessorSHA256 != "" ||
		decision.Snapshot.ProtectedPredecessorSHA256 == strings.Repeat("0", 64) ||
		decision.Snapshot.PreWireFenceSHA256 == strings.Repeat("0", 64) ||
		payload.ExecutionClaimSHA256 != decision.Snapshot.ExecutionClaimSHA256 || decision.Snapshot.ExecutionClaimCount != 1 ||
		!hexSHA256Pattern.MatchString(payload.AuthorizationSHA256) || payload.AuthorizationIssuedAtUTC != "2026-01-02T03:04:06Z" ||
		payload.TokenNotBeforeUTC != "2026-01-02T03:03:06Z" ||
		payload.AuthorizationNotAfterUTC != "2026-01-02T03:05:06Z" {
		t.Fatalf("capability lost exact broker/operation bindings: %#v", payload)
	}
}

func TestBrokerCapabilityCommitsValidatedIdentityAndExpiryWithoutBearerMaterial(t *testing.T) {
	body := mustOperationJSON(t, syntheticOperation())
	bodyDigest := mustOperationDigest(t, body)
	type variant struct {
		name   string
		mutate func(*OIDCIdentity)
	}
	variants := []variant{
		{name: "base"},
		{name: "actor", mutate: func(identity *OIDCIdentity) { identity.Actor = "synthetic-actor-alt" }},
		{name: "run", mutate: func(identity *OIDCIdentity) { identity.RunID = "3004" }},
		{name: "expiry", mutate: func(identity *OIDCIdentity) { identity.ExpiresAt = identity.ExpiresAt.Add(time.Second) }},
	}
	authorizations := make(map[string]string, len(variants))
	for _, item := range variants {
		store := NewFaultInjectingStore()
		signer := newTestEd25519Signer([32]byte{9, 8, 7, 6})
		verifier := newTestRawVerifier(RoleWriter, signer)
		verifier.mutateIdentity = item.mutate
		broker := newTestBroker(RoleWriter)
		controller, err := newTestController(RoleWriter, store, signer, verifier, broker)
		if err != nil {
			t.Fatal(err)
		}
		rawJWT, envelope := testCall(t, controller, body, MethodAuthorize, 1)
		decision, err := controller.BeginMutation(context.Background(), rawJWT, envelope, body)
		if err != nil {
			t.Fatalf("%s: %v", item.name, err)
		}
		if decision.Snapshot.OperationBodySHA256 != SHA256Hex(bodyDigest) {
			t.Fatalf("%s changed immutable operation identity", item.name)
		}
		var payload BrokerCapabilityPayload
		if err := decodeCanonical(decision.Capability.Payload, MaxBrokerPayloadBytes, &payload); err != nil {
			t.Fatal(err)
		}
		authorizations[item.name] = payload.AuthorizationSHA256
		if bytes.Contains(decision.Capability.Payload, []byte("synthetic.raw.jwt")) ||
			bytes.Contains(decision.Capability.Payload, []byte(strings.Repeat("1", 32))) ||
			bytes.Contains(decision.Capability.Payload, []byte(strings.Repeat("2", 32))) {
			t.Fatalf("%s capability retained bearer material", item.name)
		}
	}
	for _, name := range []string{"actor", "run", "expiry"} {
		if authorizations[name] == authorizations["base"] {
			t.Fatalf("%s identity change did not change authorization digest", name)
		}
	}
}

func TestControllerRejectsInvalidIdentityFromRawVerifier(t *testing.T) {
	body := mustOperationJSON(t, syntheticOperation())
	for name, mutate := range map[string]func(*OIDCIdentity){
		"zero repository ID": func(identity *OIDCIdentity) { identity.RepositoryID = "0" },
		"zero owner ID":      func(identity *OIDCIdentity) { identity.RepositoryOwnerID = "0" },
		"zero run ID":        func(identity *OIDCIdentity) { identity.RunID = "0" },
		"empty lifetime": func(identity *OIDCIdentity) {
			identity.NotBefore = identity.ExpiresAt
		},
	} {
		t.Run(name, func(t *testing.T) {
			store := NewFaultInjectingStore()
			signer := newTestEd25519Signer([32]byte{9, 8, 7, 6})
			verifier := newTestRawVerifier(RoleWriter, signer)
			verifier.mutateIdentity = mutate
			controller, err := newTestController(RoleWriter, store, signer, verifier, newTestBroker(RoleWriter))
			if err != nil {
				t.Fatal(err)
			}
			rawJWT, envelope := testCall(t, controller, body, MethodAuthorize, 1)
			if _, err := controller.BeginMutation(context.Background(), rawJWT, envelope, body); err == nil {
				t.Fatal("controller accepted invalid identity returned by raw verifier")
			}
			if _, found, err := store.Read(context.Background(), "synthetic-operation-001"); err != nil || found {
				t.Fatalf("invalid identity reached durable state: found=%v err=%v", found, err)
			}
		})
	}
}

func TestSameOperationSameBodyIsIdempotentWithFreshAuthentication(t *testing.T) {
	store := NewFaultInjectingStore()
	controller, _, _, broker := testController(t, store)
	body := mustOperationJSON(t, syntheticOperation())
	first := beginOperation(t, controller, body, 1)
	second := beginOperation(t, controller, body, 2)
	if !first.ExecuteSideEffect || second.ExecuteSideEffect || second.Snapshot.Revision != 2 {
		t.Fatalf("invalid idempotent decisions: %#v %#v", first, second)
	}
	if !bytes.Equal(first.Capability.Payload, second.Capability.Payload) || !bytes.Equal(first.Capability.Signature, second.Capability.Signature) {
		t.Fatal("idempotent replay changed signed capability")
	}
	if executions, continuations := broker.wireCounts(); executions != 1 || continuations != 0 {
		t.Fatalf("idempotent authorization repeated a provider wire: execution=%d continuation=%d", executions, continuations)
	}
}

func TestReusedAuthenticationEnvelopeIsRejected(t *testing.T) {
	store := NewFaultInjectingStore()
	controller, _, _, _ := testController(t, store)
	body := mustOperationJSON(t, syntheticOperation())
	rawJWT, envelope := testCall(t, controller, body, MethodAuthorize, 1)
	if _, err := controller.BeginMutation(context.Background(), rawJWT, envelope, body); err != nil {
		t.Fatal(err)
	}
	if _, err := controller.BeginMutation(context.Background(), rawJWT, envelope, body); !errors.Is(err, ErrReplay) {
		t.Fatalf("expected replay rejection, got %v", err)
	}
}

func TestConcurrentExactAuthenticationEnvelopeBurnsAtomicallyOnce(t *testing.T) {
	store := NewFaultInjectingStore()
	controller, _, _, _ := testController(t, store)
	body := mustOperationJSON(t, syntheticOperation())
	rawJWT, envelope := testCall(t, controller, body, MethodAuthorize, 1)
	var successful atomic.Int32
	var replayed atomic.Int32
	var unexpected atomic.Int32
	var wg sync.WaitGroup
	for index := 0; index < 32; index++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			decision, err := controller.BeginMutation(context.Background(), rawJWT, envelope, body)
			switch {
			case err == nil && decision.ExecuteSideEffect:
				successful.Add(1)
			case errors.Is(err, ErrReplay):
				replayed.Add(1)
			default:
				unexpected.Add(1)
			}
		}()
	}
	wg.Wait()
	if successful.Load() != 1 || replayed.Load() != 31 || unexpected.Load() != 0 {
		t.Fatalf("atomic burn outcomes success=%d replay=%d unexpected=%d", successful.Load(), replayed.Load(), unexpected.Load())
	}
}

func TestJTIAndChallengeAreIndependentDurableReplayKeys(t *testing.T) {
	store := NewFaultInjectingStore()
	controller, _, _, _ := testController(t, store)
	body := mustOperationJSON(t, syntheticOperation())
	bodyDigest := mustOperationDigest(t, body)
	firstChallenge, err := controller.IssueChallenge(context.Background(), body, MethodAuthorize)
	if err != nil {
		t.Fatal(err)
	}
	first := syntheticAuth(bodyDigest, MethodAuthorize, strings.Repeat("1", 32), firstChallenge)
	firstJSON, _ := MarshalAuthEnvelope(first)
	if _, err := controller.BeginMutation(context.Background(), "synthetic.raw.jwt.first", firstJSON, body); err != nil {
		t.Fatal(err)
	}

	freshChallenge, err := controller.IssueChallenge(context.Background(), body, MethodAuthorize)
	if err != nil {
		t.Fatal(err)
	}
	sameJTI := syntheticAuth(bodyDigest, MethodAuthorize, first.JTI, freshChallenge)
	sameJTIJSON, _ := MarshalAuthEnvelope(sameJTI)
	if _, err := controller.BeginMutation(context.Background(), "synthetic.raw.jwt.same-jti", sameJTIJSON, body); !errors.Is(err, ErrReplay) {
		t.Fatalf("reused JTI with fresh challenge = %v", err)
	}
	sameChallenge := syntheticAuth(bodyDigest, MethodAuthorize, strings.Repeat("4", 32), first.Challenge)
	sameChallengeJSON, _ := MarshalAuthEnvelope(sameChallenge)
	if _, err := controller.BeginMutation(context.Background(), "synthetic.raw.jwt.same-challenge", sameChallengeJSON, body); !errors.Is(err, ErrReplay) {
		t.Fatalf("reused challenge with fresh JTI = %v", err)
	}
}

func TestAuthorityIssuesAndDurablyBindsOneTimeChallenges(t *testing.T) {
	store := NewFaultInjectingStore()
	controller, _, _, _ := testController(t, store)
	body := mustOperationJSON(t, syntheticOperation())
	bodyDigest := mustOperationDigest(t, body)

	callerMinted := syntheticAuth(bodyDigest, MethodAuthorize, strings.Repeat("1", 32), strings.Repeat("f", 64))
	callerMintedJSON, err := MarshalAuthEnvelope(callerMinted)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := controller.BeginMutation(context.Background(), "synthetic.raw.jwt.caller-minted", callerMintedJSON, body); !errors.Is(err, ErrChallengeNotIssued) {
		t.Fatalf("caller-minted challenge was not rejected: %v", err)
	}

	challenge, err := controller.IssueChallenge(context.Background(), body, MethodAuthorize)
	if err != nil {
		t.Fatal(err)
	}
	if len(challenge) != AuthorityChallengeBytes*2 || !hexSHA256Pattern.MatchString(challenge) {
		t.Fatalf("authority challenge is not exactly 256 canonical bits: %q", challenge)
	}
	challengeDigest := sha256.Sum256([]byte(challenge))
	issued, ok := store.issuedChallenges[SHA256Hex(challengeDigest)]
	if !ok || issued.Consumed || issued.BindingSHA256 != challengeBindingDigest(bodyDigest, MethodAuthorize, RoleWriter, controller.authorityRootSHA256) {
		t.Fatalf("challenge was not durably issued with the exact binding: %#v", issued)
	}

	wrongMethod := syntheticAuth(bodyDigest, MethodStatus, strings.Repeat("2", 32), challenge)
	wrongMethodJSON, _ := MarshalAuthEnvelope(wrongMethod)
	if _, err := controller.Status(context.Background(), "synthetic.raw.jwt.wrong-method", wrongMethodJSON, body); !errors.Is(err, ErrChallengeBinding) {
		t.Fatalf("cross-method challenge reuse was accepted: %v", err)
	}

	drifted := syntheticOperation()
	drifted.ImageSHA256 = strings.Repeat("9", 64)
	driftedBody := mustOperationJSON(t, drifted)
	driftedDigest := mustOperationDigest(t, driftedBody)
	wrongBody := syntheticAuth(driftedDigest, MethodAuthorize, strings.Repeat("3", 32), challenge)
	wrongBodyJSON, _ := MarshalAuthEnvelope(wrongBody)
	if _, err := controller.BeginMutation(context.Background(), "synthetic.raw.jwt.wrong-body", wrongBodyJSON, driftedBody); !errors.Is(err, ErrChallengeBinding) {
		t.Fatalf("cross-body challenge reuse was accepted: %v", err)
	}

	exact := syntheticAuth(bodyDigest, MethodAuthorize, strings.Repeat("4", 32), challenge)
	exactJSON, _ := MarshalAuthEnvelope(exact)
	if _, err := controller.BeginMutation(context.Background(), "synthetic.raw.jwt.exact", exactJSON, body); err != nil {
		t.Fatalf("exact issued challenge was rejected after non-consuming binding probes: %v", err)
	}
	replayed := syntheticAuth(bodyDigest, MethodAuthorize, strings.Repeat("5", 32), challenge)
	replayedJSON, _ := MarshalAuthEnvelope(replayed)
	if _, err := controller.BeginMutation(context.Background(), "synthetic.raw.jwt.replayed", replayedJSON, body); !errors.Is(err, ErrReplay) {
		t.Fatalf("consumed challenge was reusable: %v", err)
	}
}

func TestAuthorityChallengeBindingIncludesExactAuthorityRoot(t *testing.T) {
	store := NewFaultInjectingStore()
	firstSigner := newTestEd25519Signer([32]byte{9, 8, 7, 6})
	firstController, err := newTestControllerWithEntropy(
		RoleWriter,
		store,
		firstSigner,
		newTestRawVerifier(RoleWriter, firstSigner),
		newTestBroker(RoleWriter),
		&deterministicEntropy{},
	)
	if err != nil {
		t.Fatal(err)
	}
	secondSigner := newTestEd25519Signer([32]byte{6, 7, 8, 9})
	secondController, err := newTestControllerWithEntropy(
		RoleWriter,
		store,
		secondSigner,
		newTestRawVerifier(RoleWriter, secondSigner),
		newTestBroker(RoleWriter),
		&deterministicEntropy{},
	)
	if err != nil {
		t.Fatal(err)
	}

	body := mustOperationJSON(t, syntheticOperation())
	bodyDigest := mustOperationDigest(t, body)
	challenge, err := firstController.IssueChallenge(context.Background(), body, MethodAuthorize)
	if err != nil {
		t.Fatal(err)
	}
	envelope := syntheticAuth(bodyDigest, MethodAuthorize, strings.Repeat("6", 32), challenge)
	canonicalEnvelope, err := MarshalAuthEnvelope(envelope)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := secondController.BeginMutation(context.Background(), "synthetic.raw.jwt.wrong-root", canonicalEnvelope, body); !errors.Is(err, ErrChallengeBinding) {
		t.Fatalf("same-role controller under a different authority root used the challenge: %v", err)
	}
	if _, err := firstController.BeginMutation(context.Background(), "synthetic.raw.jwt.exact-root", canonicalEnvelope, body); err != nil {
		t.Fatalf("wrong-root probe consumed the challenge or JTI before the exact root used it: %v", err)
	}
}

func TestPreparedCapabilityFenceSurvivesCrashBeforeAndAfterIssue(t *testing.T) {
	t.Run("prepared commit survives ambiguous response", func(t *testing.T) {
		store := NewFaultInjectingStore()
		controller, _, _, _ := testController(t, store)
		body := mustOperationJSON(t, syntheticOperation())
		store.Inject(FaultAfterCommit, 1)
		rawJWT, envelope := testCall(t, controller, body, MethodAuthorize, 1)
		if _, err := controller.BeginMutation(context.Background(), rawJWT, envelope, body); !errors.Is(err, ErrInjectedCrash) {
			t.Fatalf("expected ambiguous prepared commit, got %v", err)
		}
		prepared, found, err := store.Read(context.Background(), "synthetic-operation-001")
		if err != nil || !found || prepared.State != StatePrepared || prepared.Revision != 1 ||
			len(prepared.CapabilityPayload) == 0 || len(prepared.CapabilitySignature) != 0 ||
			prepared.PreWireFenceSHA256 == strings.Repeat("0", 64) {
			t.Fatalf("pre-wire preparation was not crash durable: %#v found=%v err=%v", prepared, found, err)
		}
		statusJWT, statusEnvelope := testCall(t, controller, body, MethodStatus, 2)
		status, err := controller.Status(context.Background(), statusJWT, statusEnvelope, body)
		if err != nil || status.State != StatePrepared || status.Revision != prepared.Revision {
			t.Fatalf("status did not reconcile prepared state without mutation: %#v err=%v", status, err)
		}
		resumed := beginOperation(t, controller, body, 3)
		if !resumed.ExecuteSideEffect || resumed.Snapshot.State != StateIssued || resumed.Snapshot.Revision != 2 ||
			resumed.Snapshot.PreWireFenceSHA256 != prepared.PreWireFenceSHA256 {
			t.Fatalf("fresh authentication did not issue the exact prepared fence once: %#v", resumed)
		}
	})

	for _, test := range []struct {
		name          string
		fault         FaultPoint
		expectedState OperationState
	}{
		{name: "issue save", fault: FaultSave, expectedState: StatePrepared},
		{name: "issue audit", fault: FaultAppendAudit, expectedState: StatePrepared},
		{name: "issue before commit", fault: FaultBeforeCommit, expectedState: StatePrepared},
		{name: "issue after commit", fault: FaultAfterCommit, expectedState: StateIssued},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := NewFaultInjectingStore()
			baseSigner := newTestEd25519Signer([32]byte{9, 8, 7, 6})
			broker := newTestBroker(RoleWriter)
			entropy := &deterministicEntropy{}
			armingSigner := &faultArmingSigner{testEd25519Signer: baseSigner, store: store, domain: DomainBrokerCapability, fault: test.fault}
			verifier := newTestRawVerifier(RoleWriter, armingSigner)
			controller, err := newTestControllerWithEntropy(RoleWriter, store, armingSigner, verifier, broker, entropy)
			if err != nil {
				t.Fatal(err)
			}
			body := mustOperationJSON(t, syntheticOperation())
			rawJWT, envelope := testCall(t, controller, body, MethodAuthorize, 10)
			if _, err := controller.BeginMutation(context.Background(), rawJWT, envelope, body); !errors.Is(err, ErrInjectedCrash) {
				t.Fatalf("expected ambiguous issue transaction, got %v", err)
			}
			snapshot, found, err := store.Read(context.Background(), "synthetic-operation-001")
			if err != nil || !found || snapshot.State != test.expectedState {
				t.Fatalf("issue transaction did not reconcile to its exact durable boundary: %#v found=%v err=%v", snapshot, found, err)
			}

			recovered, err := newTestControllerWithEntropy(RoleWriter, store, baseSigner, verifier, broker, entropy)
			if err != nil {
				t.Fatal(err)
			}
			statusJWT, statusEnvelope := testCall(t, recovered, body, MethodStatus, 11)
			status, err := recovered.Status(context.Background(), statusJWT, statusEnvelope, body)
			if err != nil || status.State != test.expectedState || status.Revision != snapshot.Revision {
				t.Fatalf("status did not reconcile the issue transaction without mutation: %#v err=%v", status, err)
			}
			resumed := beginOperation(t, recovered, body, 12)
			if !resumed.ExecuteSideEffect || resumed.Snapshot.State != StateIssued || resumed.Snapshot.Revision != 2 || resumed.Snapshot.ExecutionClaimCount != 1 {
				t.Fatalf("durable execution claim did not recover exactly once: %#v", resumed)
			}
			if executions, continuations := broker.wireCounts(); executions != 1 || continuations != 0 {
				t.Fatalf("recovered execution claim wire count execution=%d continuation=%d", executions, continuations)
			}
		})
	}
}

func TestAuthenticationBurnSurvivesLaterAuthorizationFailures(t *testing.T) {
	tests := []struct {
		name         string
		fault        FaultPoint
		signerWrap   func(*testEd25519Signer) ReceiptSigner
		wantPrepared bool
	}{
		{name: "callback save", fault: FaultSave},
		{name: "callback audit", fault: FaultAppendAudit},
		{name: "before commit", fault: FaultBeforeCommit},
		{name: "signer", wantPrepared: true, signerWrap: func(signer *testEd25519Signer) ReceiptSigner {
			return &failingSigner{testEd25519Signer: signer, domain: DomainBrokerCapability}
		}},
		{name: "self verify", wantPrepared: true, signerWrap: func(signer *testEd25519Signer) ReceiptSigner {
			return &corruptingSigner{testEd25519Signer: signer, domain: DomainBrokerCapability}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := NewFaultInjectingStore()
			signer := newTestEd25519Signer([32]byte{9, 8, 7, 6})
			var receiptSigner ReceiptSigner = signer
			if test.signerWrap != nil {
				receiptSigner = test.signerWrap(signer)
			}
			controller, err := newTestController(RoleWriter, store, receiptSigner, newTestRawVerifier(RoleWriter, receiptSigner), newTestBroker(RoleWriter))
			if err != nil {
				t.Fatal(err)
			}
			if test.fault != "" {
				store.Inject(test.fault, 1)
			}
			body := mustOperationJSON(t, syntheticOperation())
			rawJWT, envelope := testCall(t, controller, body, MethodAuthorize, 1)
			if _, err := controller.BeginMutation(context.Background(), rawJWT, envelope, body); err == nil {
				t.Fatal("injected post-authentication failure was not observed")
			}
			if _, err := controller.BeginMutation(context.Background(), rawJWT, envelope, body); !errors.Is(err, ErrReplay) {
				t.Fatalf("later failure rolled back the authentication burn: %v", err)
			}
			snapshot, found, err := store.Read(context.Background(), "synthetic-operation-001")
			if err != nil || found != test.wantPrepared || (test.wantPrepared && snapshot.State != StatePrepared) {
				t.Fatalf("failed authorization violated durable prepared fence: %#v found=%v err=%v", snapshot, found, err)
			}
		})
	}
}

func TestAuthenticationBurnSurvivesLaterTerminalFailures(t *testing.T) {
	tests := []struct {
		name       string
		fault      FaultPoint
		signerWrap func(*testEd25519Signer) ReceiptSigner
	}{
		{name: "erase", fault: FaultEraseSealedReconciliation},
		{name: "store", fault: FaultSave},
		{name: "audit", fault: FaultAppendAudit},
		{name: "before commit", fault: FaultBeforeCommit},
		{name: "signer", signerWrap: func(signer *testEd25519Signer) ReceiptSigner {
			return &failingSigner{testEd25519Signer: signer, domain: DomainWriterReceipt}
		}},
		{name: "self verify", signerWrap: func(signer *testEd25519Signer) ReceiptSigner {
			return &corruptingSigner{testEd25519Signer: signer, domain: DomainWriterReceipt}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := NewFaultInjectingStore()
			controller, signer, verifier, broker := testController(t, store)
			body := mustOperationJSON(t, syntheticOperation())
			decision := beginOperation(t, controller, body, 1)
			acceptReadback(t, controller, broker, body, decision, 2)
			if test.signerWrap != nil {
				var err error
				controller, err = newTestController(RoleWriter, store, test.signerWrap(signer), verifier, broker)
				if err != nil {
					t.Fatal(err)
				}
			}
			if test.fault != "" {
				store.Inject(test.fault, 1)
			}
			rawJWT, envelope := testCall(t, controller, body, MethodTerminalProof, 3)
			if _, err := controller.CommitTerminalProof(context.Background(), rawJWT, envelope, body); err == nil {
				t.Fatal("injected post-authentication terminal failure was not observed")
			}
			if _, err := controller.CommitTerminalProof(context.Background(), rawJWT, envelope, body); !errors.Is(err, ErrReplay) {
				t.Fatalf("later terminal failure rolled back the authentication burn: %v", err)
			}
			snapshot, found, err := store.Read(context.Background(), "synthetic-operation-001")
			if err != nil || !found || snapshot.State != StateSideEffectStarted || !snapshot.SealedReconciliationPresent || snapshot.SealedReconciliationErased {
				t.Fatalf("failed terminal proof changed semantic state: %#v found=%v err=%v", snapshot, found, err)
			}
		})
	}
}

func TestAuthenticatedSemanticRejectionDurablyConsumesJTIAndChallenge(t *testing.T) {
	semantics := []struct {
		name string
		body OperationBody
		want string
	}{
		{name: "wrong role", body: func() OperationBody {
			body := syntheticOperation()
			body.Role = RoleObserver
			body.Action = ActionMarkerRead
			return body
		}(), want: "role"},
		{name: "wrong action", body: func() OperationBody {
			body := syntheticOperation()
			body.Action = ActionMarkerRead
			return body
		}(), want: "action"},
	}
	type invoke func(*Controller, string, []byte, []byte) error
	endpoints := []struct {
		name, method, event string
		invoke              invoke
	}{
		{name: "authorize", method: MethodAuthorize, event: "authorization-rejected-unowned-operation", invoke: func(controller *Controller, raw string, envelope, body []byte) error {
			_, err := controller.BeginMutation(context.Background(), raw, envelope, body)
			return err
		}},
		{name: "broker readback", method: MethodBrokerReadback, event: "broker-readback-rejected-unowned-operation", invoke: func(controller *Controller, raw string, envelope, body []byte) error {
			_, err := controller.AcceptBrokerReadback(context.Background(), raw, envelope, body, BrokerReadback{})
			return err
		}},
		{name: "terminal proof", method: MethodTerminalProof, event: "terminal-proof-rejected-unowned-operation", invoke: func(controller *Controller, raw string, envelope, body []byte) error {
			_, err := controller.CommitTerminalProof(context.Background(), raw, envelope, body)
			return err
		}},
		{name: "status", method: MethodStatus, event: "status-rejected-unowned-operation", invoke: func(controller *Controller, raw string, envelope, body []byte) error {
			_, err := controller.Status(context.Background(), raw, envelope, body)
			return err
		}},
	}
	for _, semantic := range semantics {
		for _, endpoint := range endpoints {
			t.Run(semantic.name+"/"+endpoint.name, func(t *testing.T) {
				store := NewFaultInjectingStore()
				controller, _, _, _ := testController(t, store)
				body := mustOperationJSON(t, semantic.body)
				rawJWT, envelope := testCall(t, controller, body, endpoint.method, 1)
				if err := endpoint.invoke(controller, rawJWT, envelope, body); err == nil || !strings.Contains(err.Error(), semantic.want) {
					t.Fatalf("authenticated semantic rejection = %v", err)
				}
				if _, found, err := store.Read(context.Background(), semantic.body.OperationID); err != nil || found {
					t.Fatalf("semantic rejection created operation: found=%v err=%v", found, err)
				}
				if err := endpoint.invoke(controller, rawJWT, envelope, body); !errors.Is(err, ErrReplay) {
					t.Fatalf("semantic rejection did not durably burn auth envelope: %v", err)
				}
				if audit := store.Audit(); len(audit) != 1 || audit[0].Event != endpoint.event {
					t.Fatalf("semantic rejection audit = %#v", audit)
				}
			})
		}
	}
}

func TestDifferingOperationReuseIsDurablyQuarantined(t *testing.T) {
	store := NewFaultInjectingStore()
	controller, _, _, _ := testController(t, store)
	body := mustOperationJSON(t, syntheticOperation())
	beginOperation(t, controller, body, 1)
	changed := syntheticOperation()
	changed.ImageSHA256 = strings.Repeat("f", 64)
	changedBody := mustOperationJSON(t, changed)
	rawJWT, envelope := testCall(t, controller, changedBody, MethodAuthorize, 2)
	if _, err := controller.BeginMutation(context.Background(), rawJWT, envelope, changedBody); !errors.Is(err, ErrOperationReuse) {
		t.Fatalf("expected immutable-identity reuse quarantine, got %v", err)
	}
	snapshot, found, err := store.Read(context.Background(), changed.OperationID)
	if err != nil || !found || snapshot.State != StateQuarantined || snapshot.QuarantineReason == "" {
		t.Fatalf("quarantine not durable: %#v %v %v", snapshot, found, err)
	}
}

func TestStatusNeverCreatesOperationAndConsumesFreshAuth(t *testing.T) {
	store := NewFaultInjectingStore()
	controller, _, _, _ := testController(t, store)
	body := mustOperationJSON(t, syntheticOperation())
	rawJWT, envelope := testCall(t, controller, body, MethodStatus, 1)
	if _, err := controller.Status(context.Background(), rawJWT, envelope, body); !errors.Is(err, ErrOperationMissing) {
		t.Fatalf("expected missing status, got %v", err)
	}
	if _, found, _ := store.Read(context.Background(), "synthetic-operation-001"); found {
		t.Fatal("status created an operation")
	}
	if _, err := controller.Status(context.Background(), rawJWT, envelope, body); !errors.Is(err, ErrReplay) {
		t.Fatalf("missing status did not consume authentication: %v", err)
	}
}

func TestAuthorizationCrashWindowsRequireStatusReconciliation(t *testing.T) {
	for _, fault := range []FaultPoint{FaultBeforeCommit, FaultAfterCommit} {
		t.Run(string(fault), func(t *testing.T) {
			store := NewFaultInjectingStore()
			controller, _, _, _ := testController(t, store)
			body := mustOperationJSON(t, syntheticOperation())
			store.Inject(fault, 1)
			rawJWT, envelope := testCall(t, controller, body, MethodAuthorize, 1)
			if _, err := controller.BeginMutation(context.Background(), rawJWT, envelope, body); !errors.Is(err, ErrInjectedCrash) {
				t.Fatalf("expected injected crash, got %v", err)
			}
			if _, err := controller.BeginMutation(context.Background(), rawJWT, envelope, body); !errors.Is(err, ErrReplay) {
				t.Fatalf("ambiguous transaction outcome rolled back authentication burn: %v", err)
			}
			statusJWT, statusEnvelope := testCall(t, controller, body, MethodStatus, 2)
			snapshot, err := controller.Status(context.Background(), statusJWT, statusEnvelope, body)
			if fault == FaultBeforeCommit && !errors.Is(err, ErrOperationMissing) {
				t.Fatalf("before-commit ambiguity did not reconcile absence: %#v %v", snapshot, err)
			}
			if fault == FaultAfterCommit && (err != nil || snapshot.State != StatePrepared) {
				t.Fatalf("after-commit ambiguity did not reconcile the durable pre-wire preparation: %#v %v", snapshot, err)
			}
		})
	}
}

func TestConcurrentAuthorizationExecutesExactlyOnce(t *testing.T) {
	store := NewFaultInjectingStore()
	controller, _, _, _ := testController(t, store)
	body := mustOperationJSON(t, syntheticOperation())
	var executes atomic.Int32
	var failures atomic.Int32
	var wg sync.WaitGroup
	for index := 0; index < 32; index++ {
		rawJWT, envelope := testCall(t, controller, body, MethodAuthorize, 100+index)
		wg.Add(1)
		go func(raw string, auth []byte) {
			defer wg.Done()
			decision, err := controller.BeginMutation(context.Background(), raw, auth, body)
			if err != nil {
				failures.Add(1)
			} else if decision.ExecuteSideEffect {
				executes.Add(1)
			}
		}(rawJWT, envelope)
	}
	wg.Wait()
	if failures.Load() != 0 || executes.Load() != 1 {
		t.Fatalf("concurrent authorization failures=%d executes=%d", failures.Load(), executes.Load())
	}
}

func TestBrokerReadbackBindsRolePhaseGenerationDigestMTLSAndCapability(t *testing.T) {
	mutations := map[string]func(*BrokerReadbackPayload){
		"role":             func(p *BrokerReadbackPayload) { p.BrokerRole = RoleObserver },
		"phase":            func(p *BrokerReadbackPayload) { p.Phase = "bridge" },
		"generation":       func(p *BrokerReadbackPayload) { p.Generation++ },
		"operation digest": func(p *BrokerReadbackPayload) { p.OperationBodySHA256 = strings.Repeat("d", 64) },
		"capability":       func(p *BrokerReadbackPayload) { p.CapabilitySHA256 = strings.Repeat("d", 64) },
		"identity":         func(p *BrokerReadbackPayload) { p.BrokerIdentitySHA256 = strings.Repeat("d", 64) },
		"mTLS":             func(p *BrokerReadbackPayload) { p.MTLSPeerSHA256 = strings.Repeat("d", 64) },
		"execution claim":  func(p *BrokerReadbackPayload) { p.ExecutionClaimSHA256 = strings.Repeat("d", 64) },
		"unclaimed continuation": func(p *BrokerReadbackPayload) {
			p.ContinuationClaimSHA256 = strings.Repeat("d", 64)
		},
		"31-byte marker": func(p *BrokerReadbackPayload) { p.MarkerMaterialBytes = 31 },
		"33-byte marker": func(p *BrokerReadbackPayload) { p.MarkerMaterialBytes = 33 },
	}
	for name, mutation := range mutations {
		t.Run(name, func(t *testing.T) {
			store := NewFaultInjectingStore()
			controller, _, _, broker := testController(t, store)
			body := mustOperationJSON(t, syntheticOperation())
			decision := beginOperation(t, controller, body, 1)
			readback := makeReadback(t, broker, body, decision, markerFixture(0), []byte("opaque-sealed"), mutation)
			rawJWT, envelope := testCall(t, controller, body, MethodBrokerReadback, 2)
			snapshot, err := controller.AcceptBrokerReadback(context.Background(), rawJWT, envelope, body, readback)
			if !errors.Is(err, ErrQuarantined) || snapshot.State != StateQuarantined {
				t.Fatalf("accepted unbound broker readback: %#v %v", snapshot, err)
			}
		})
	}

	t.Run("sealed material tamper", func(t *testing.T) {
		store := NewFaultInjectingStore()
		controller, _, _, broker := testController(t, store)
		body := mustOperationJSON(t, syntheticOperation())
		decision := beginOperation(t, controller, body, 10)
		readback := makeReadback(t, broker, body, decision, markerFixture(0), []byte("opaque-sealed"), nil)
		readback.SealedReconciliation[0] ^= 1
		rawJWT, envelope := testCall(t, controller, body, MethodBrokerReadback, 11)
		if snapshot, err := controller.AcceptBrokerReadback(context.Background(), rawJWT, envelope, body, readback); !errors.Is(err, ErrQuarantined) || snapshot.State != StateQuarantined {
			t.Fatalf("accepted sealed-material tamper: %#v %v", snapshot, err)
		}
	})

	t.Run("signature tamper", func(t *testing.T) {
		store := NewFaultInjectingStore()
		controller, _, _, broker := testController(t, store)
		body := mustOperationJSON(t, syntheticOperation())
		decision := beginOperation(t, controller, body, 20)
		readback := makeReadback(t, broker, body, decision, markerFixture(0), []byte("opaque-sealed"), nil)
		readback.Signature[0] ^= 1
		rawJWT, envelope := testCall(t, controller, body, MethodBrokerReadback, 21)
		if _, err := controller.AcceptBrokerReadback(context.Background(), rawJWT, envelope, body, readback); !errors.Is(err, ErrQuarantined) {
			t.Fatalf("accepted readback signature tamper: %v", err)
		}
	})
}

func TestUnchangedPredecessorIsReconciledOnlyInsideTheFencedBrokerOperation(t *testing.T) {
	store := NewFaultInjectingStore()
	controller, _, _, broker := testController(t, store)
	body := mustOperationJSON(t, syntheticOperation())
	decision := beginOperation(t, controller, body, 1)
	marker := markerFixture(17)
	sealed := []byte("synthetic-sealed-marker-for-one-internal-continuation")
	ambiguousReadback := makeReadback(t, broker, body, decision, marker, sealed, func(payload *BrokerReadbackPayload) {
		payload.Outcome = BrokerOutcomeUnchangedPredecessor
	})
	store.Inject(FaultAfterCommit, 1)
	rawJWT, envelope := testCall(t, controller, body, MethodBrokerReadback, 2)
	if _, err := controller.AcceptBrokerReadback(context.Background(), rawJWT, envelope, body, ambiguousReadback); !errors.Is(err, ErrInjectedCrash) {
		t.Fatalf("expected ambiguous response after durable continuation claim, got %v", err)
	}
	ambiguous, found, err := store.Read(context.Background(), "synthetic-operation-001")
	if err != nil || !found || ambiguous.State != StateAmbiguous || ambiguous.AmbiguityCount != 1 || ambiguous.ContinuationClaimCount != 1 ||
		ambiguous.AmbiguitySHA256 == strings.Repeat("0", 64) || !ambiguous.SealedReconciliationPresent {
		t.Fatalf("unchanged-predecessor continuation was not durably fenced before the second wire: %#v found=%v err=%v", ambiguous, found, err)
	}
	if executions, continuations := broker.wireCounts(); executions != 1 || continuations != 0 {
		t.Fatalf("durable ambiguity claim touched the second wire early: execution=%d continuation=%d", executions, continuations)
	}
	statusJWT, statusEnvelope := testCall(t, controller, body, MethodStatus, 3)
	status, err := controller.Status(context.Background(), statusJWT, statusEnvelope, body)
	if err != nil || status.State != StateAmbiguous || status.Revision != ambiguous.Revision {
		t.Fatalf("status was not snapshot-only at the ambiguity fence: %#v err=%v", status, err)
	}
	if executions, continuations := broker.wireCounts(); executions != 1 || continuations != 0 {
		t.Fatalf("status repeated a provider wire: execution=%d continuation=%d", executions, continuations)
	}
	if _, err := MarshalAuthEnvelope(syntheticAuth(mustOperationDigest(t, body), "claim-continuation", strings.Repeat("8", 32), strings.Repeat("9", 64))); err == nil {
		t.Fatal("public continuation authentication method was accepted")
	}
	replayJWT, replayEnvelope := testCall(t, controller, body, MethodBrokerReadback, 4)
	committed, err := controller.AcceptBrokerReadback(context.Background(), replayJWT, replayEnvelope, body, ambiguousReadback)
	if err != nil || committed.State != StateSideEffectStarted || committed.AmbiguityCount != 1 || committed.ContinuationClaimCount != 1 ||
		committed.MarkerSHA256 != ambiguous.MarkerSHA256 {
		t.Fatalf("exact durable continuation did not complete once: %#v err=%v", committed, err)
	}
	if executions, continuations := broker.wireCounts(); executions != 1 || continuations != 1 {
		t.Fatalf("continuation wire was not exactly once: execution=%d continuation=%d", executions, continuations)
	}
	idempotentJWT, idempotentEnvelope := testCall(t, controller, body, MethodBrokerReadback, 5)
	idempotent, err := controller.AcceptBrokerReadback(context.Background(), idempotentJWT, idempotentEnvelope, body, ambiguousReadback)
	if err != nil || idempotent.State != StateSideEffectStarted || idempotent.Revision != committed.Revision {
		t.Fatalf("exact ambiguity replay after completion changed state: %#v err=%v", idempotent, err)
	}
	if executions, continuations := broker.wireCounts(); executions != 1 || continuations != 1 {
		t.Fatalf("exact replay repeated a provider wire: execution=%d continuation=%d", executions, continuations)
	}
	idempotentAuthorize := beginOperation(t, controller, body, 6)
	if idempotentAuthorize.ExecuteSideEffect || idempotentAuthorize.Snapshot.State != StateSideEffectStarted || idempotentAuthorize.Snapshot.Revision != committed.Revision {
		t.Fatalf("fresh authorize escaped the completed broker fence: %#v", idempotentAuthorize)
	}
}

func TestExecutionClaimTransportLossDoesNotRepeatProviderWire(t *testing.T) {
	store := NewFaultInjectingStore()
	controller, _, _, broker := testController(t, store)
	broker.failExecutionAfterWire = 1
	body := mustOperationJSON(t, syntheticOperation())
	rawJWT, envelope := testCall(t, controller, body, MethodAuthorize, 1)
	failed, err := controller.BeginMutation(context.Background(), rawJWT, envelope, body)
	if err == nil || failed.Snapshot.State != StateIssued || failed.ExecuteSideEffect || failed.Snapshot.ExecutionClaimCount != 1 {
		t.Fatalf("post-wire execution ambiguity was not durably recoverable: %#v err=%v", failed, err)
	}
	if executions, continuations := broker.wireCounts(); executions != 1 || continuations != 0 {
		t.Fatalf("unexpected provider wire count after ambiguous execution: execution=%d continuation=%d", executions, continuations)
	}
	statusJWT, statusEnvelope := testCall(t, controller, body, MethodStatus, 2)
	if status, err := controller.Status(context.Background(), statusJWT, statusEnvelope, body); err != nil || status.State != StateIssued {
		t.Fatalf("status did not reconcile the durable execution claim: %#v err=%v", status, err)
	}
	recovered := beginOperation(t, controller, body, 3)
	if recovered.ExecuteSideEffect || recovered.Snapshot.State != StateIssued || recovered.Snapshot.ExecutionClaimSHA256 != failed.Snapshot.ExecutionClaimSHA256 {
		t.Fatalf("execution claim retry was not exact and idempotent: %#v", recovered)
	}
	if executions, continuations := broker.wireCounts(); executions != 1 || continuations != 0 {
		t.Fatalf("execution ambiguity repeated a provider wire: execution=%d continuation=%d", executions, continuations)
	}
}

func TestContinuationTransportLossDoesNotRepeatProviderWire(t *testing.T) {
	store := NewFaultInjectingStore()
	controller, _, _, broker := testController(t, store)
	body := mustOperationJSON(t, syntheticOperation())
	decision := beginOperation(t, controller, body, 1)
	broker.failContinuationAfterWire = 1
	readback := makeReadback(t, broker, body, decision, markerFixture(19), []byte("synthetic-sealed-continuation"), func(payload *BrokerReadbackPayload) {
		payload.Outcome = BrokerOutcomeUnchangedPredecessor
	})
	rawJWT, envelope := testCall(t, controller, body, MethodBrokerReadback, 2)
	ambiguous, err := controller.AcceptBrokerReadback(context.Background(), rawJWT, envelope, body, readback)
	if err == nil || ambiguous.State != StateAmbiguous || ambiguous.ContinuationClaimCount != 1 {
		t.Fatalf("broker continuation transport loss did not preserve the exact durable claim: %#v err=%v", ambiguous, err)
	}
	if executions, continuations := broker.wireCounts(); executions != 1 || continuations != 1 {
		t.Fatalf("unexpected wire count after continuation transport loss: execution=%d continuation=%d", executions, continuations)
	}
	retryJWT, retryEnvelope := testCall(t, controller, body, MethodBrokerReadback, 3)
	recovered, err := controller.AcceptBrokerReadback(context.Background(), retryJWT, retryEnvelope, body, readback)
	if err != nil || recovered.State != StateSideEffectStarted || recovered.ContinuationClaimSHA256 != ambiguous.ContinuationClaimSHA256 {
		t.Fatalf("cached exact continuation could not recover transport loss: %#v err=%v", recovered, err)
	}
	if executions, continuations := broker.wireCounts(); executions != 1 || continuations != 1 {
		t.Fatalf("continuation transport recovery repeated a provider wire: execution=%d continuation=%d", executions, continuations)
	}
}

func TestContinuationCommitResponseLossDoesNotRepeatProviderWire(t *testing.T) {
	store := NewFaultInjectingStore()
	controller, _, _, broker := testController(t, store)
	body := mustOperationJSON(t, syntheticOperation())
	decision := beginOperation(t, controller, body, 1)
	broker.afterContinuationWire = func() { store.Inject(FaultAfterCommit, 1) }
	readback := makeReadback(t, broker, body, decision, markerFixture(29), []byte("synthetic-sealed-post-commit"), func(payload *BrokerReadbackPayload) {
		payload.Outcome = BrokerOutcomeUnchangedPredecessor
	})
	rawJWT, envelope := testCall(t, controller, body, MethodBrokerReadback, 2)
	committed, err := controller.AcceptBrokerReadback(context.Background(), rawJWT, envelope, body, readback)
	if !errors.Is(err, ErrInjectedCrash) || committed.State != StateSideEffectStarted {
		t.Fatalf("expected lost response after durable continuation commit: %#v err=%v", committed, err)
	}
	if executions, continuations := broker.wireCounts(); executions != 1 || continuations != 1 {
		t.Fatalf("unexpected wire count at lost continuation response: execution=%d continuation=%d", executions, continuations)
	}
	retryJWT, retryEnvelope := testCall(t, controller, body, MethodBrokerReadback, 3)
	recovered, err := controller.AcceptBrokerReadback(context.Background(), retryJWT, retryEnvelope, body, readback)
	if err != nil || recovered.State != StateSideEffectStarted || recovered.Revision != committed.Revision {
		t.Fatalf("lost continuation response was not reconciled idempotently: %#v err=%v", recovered, err)
	}
	if executions, continuations := broker.wireCounts(); executions != 1 || continuations != 1 {
		t.Fatalf("lost continuation response repeated a provider wire: execution=%d continuation=%d", executions, continuations)
	}
}

func TestAmbiguousContinuationQuarantinesSecondAmbiguityOrMarkerDrift(t *testing.T) {
	for _, test := range []struct {
		name      string
		committed bool
		mutate    func(*BrokerReadbackPayload)
		marker    [32]byte
		sealed    []byte
	}{
		{
			name: "second ambiguity", marker: markerFixture(23), sealed: []byte("synthetic-sealed-marker"),
			mutate: func(payload *BrokerReadbackPayload) {
				payload.Outcome = BrokerOutcomeUnchangedPredecessor
				payload.BrokerActionAtUTC = "2026-01-02T03:04:08Z"
			},
		},
		{name: "different committed marker", committed: true, marker: markerFixture(24), sealed: []byte("synthetic-sealed-marker")},
		{name: "different sealed material", committed: true, marker: markerFixture(23), sealed: []byte("different-synthetic-sealed-marker")},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := NewFaultInjectingStore()
			controller, _, _, broker := testController(t, store)
			body := mustOperationJSON(t, syntheticOperation())
			decision := beginOperation(t, controller, body, 10)
			originalMarker := markerFixture(23)
			originalSealed := []byte("synthetic-sealed-marker")
			first := makeReadback(t, broker, body, decision, originalMarker, originalSealed, func(payload *BrokerReadbackPayload) {
				payload.Outcome = BrokerOutcomeUnchangedPredecessor
			})
			store.Inject(FaultAfterCommit, 1)
			rawJWT, envelope := testCall(t, controller, body, MethodBrokerReadback, 11)
			if _, err := controller.AcceptBrokerReadback(context.Background(), rawJWT, envelope, body, first); !errors.Is(err, ErrInjectedCrash) {
				t.Fatalf("failed to stop after durable ambiguity fence: %v", err)
			}
			if snapshot, found, err := store.Read(context.Background(), "synthetic-operation-001"); err != nil || !found || snapshot.State != StateAmbiguous {
				t.Fatalf("failed to establish ambiguity fence: %#v found=%v err=%v", snapshot, found, err)
			}

			mutate := test.mutate
			if !test.committed && mutate == nil {
				mutate = func(payload *BrokerReadbackPayload) { payload.Outcome = BrokerOutcomeUnchangedPredecessor }
			}
			second := makeReadback(t, broker, body, decision, test.marker, test.sealed, mutate)
			secondJWT, secondEnvelope := testCall(t, controller, body, MethodBrokerReadback, 12)
			snapshot, err := controller.AcceptBrokerReadback(context.Background(), secondJWT, secondEnvelope, body, second)
			if !errors.Is(err, ErrQuarantined) || snapshot.State != StateQuarantined {
				t.Fatalf("ambiguous continuation drift was not quarantined: %#v err=%v", snapshot, err)
			}
		})
	}
}

func TestProviderInternalContinuationQuarantinesASecondAmbiguity(t *testing.T) {
	store := NewFaultInjectingStore()
	controller, _, _, broker := testController(t, store)
	body := mustOperationJSON(t, syntheticOperation())
	decision := beginOperation(t, controller, body, 1)
	broker.continuationOutcome = BrokerOutcomeUnchangedPredecessor
	readback := makeReadback(t, broker, body, decision, markerFixture(31), []byte("synthetic-sealed-second-ambiguity"), func(payload *BrokerReadbackPayload) {
		payload.Outcome = BrokerOutcomeUnchangedPredecessor
	})
	rawJWT, envelope := testCall(t, controller, body, MethodBrokerReadback, 2)
	snapshot, err := controller.AcceptBrokerReadback(context.Background(), rawJWT, envelope, body, readback)
	if !errors.Is(err, ErrQuarantined) || snapshot.State != StateQuarantined ||
		!strings.Contains(snapshot.QuarantineReason, "did not commit the exact fenced marker") {
		t.Fatalf("provider-internal second ambiguity did not quarantine: %#v err=%v", snapshot, err)
	}
	if executions, continuations := broker.wireCounts(); executions != 1 || continuations != 1 {
		t.Fatalf("second ambiguity used an unexpected wire count: execution=%d continuation=%d", executions, continuations)
	}
}

func TestBrokerReadbackActionTimeUsesInclusiveIssuedAndNotBeforeExclusiveExpiry(t *testing.T) {
	tests := []struct {
		name     string
		actionAt string
		valid    bool
	}{
		{name: "at issued lower bound", actionAt: "2026-01-02T03:04:06Z", valid: true},
		{name: "valid fractional instant", actionAt: "2026-01-02T03:05:05.999999999Z", valid: true},
		{name: "before issued lower bound", actionAt: "2026-01-02T03:04:05.999999999Z"},
		{name: "equal exclusive expiry", actionAt: "2026-01-02T03:05:06Z"},
		{name: "after exclusive expiry", actionAt: "2026-01-02T03:05:06.000000001Z"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := NewFaultInjectingStore()
			controller, _, _, broker := testController(t, store)
			body := mustOperationJSON(t, syntheticOperation())
			decision := beginOperation(t, controller, body, 1)
			readback := makeReadback(t, broker, body, decision, markerFixture(0), []byte("opaque-sealed"), func(payload *BrokerReadbackPayload) {
				payload.BrokerActionAtUTC = test.actionAt
			})
			rawJWT, envelope := testCall(t, controller, body, MethodBrokerReadback, 2)
			snapshot, err := controller.AcceptBrokerReadback(context.Background(), rawJWT, envelope, body, readback)
			if test.valid {
				if err != nil || snapshot.State != StateSideEffectStarted {
					t.Fatalf("valid broker action instant rejected: %#v err=%v", snapshot, err)
				}
				return
			}
			if !errors.Is(err, ErrQuarantined) || snapshot.State != StateQuarantined ||
				!strings.Contains(snapshot.QuarantineReason, "inclusive-issued-and-not-before/exclusive-expiry") {
				t.Fatalf("out-of-window broker action instant accepted: %#v err=%v", snapshot, err)
			}
		})
	}
}

func TestBrokerReadbackActionTimeHonorsDelayedTokenNotBefore(t *testing.T) {
	tests := []struct {
		name     string
		actionAt string
		valid    bool
	}{
		{name: "fractional instant before delayed not-before", actionAt: "2026-01-02T03:04:06.999999999Z"},
		{name: "equal delayed not-before", actionAt: "2026-01-02T03:04:07Z", valid: true},
		{name: "fractional instant after delayed not-before", actionAt: "2026-01-02T03:04:07.000000001Z", valid: true},
		{name: "fractional instant before expiry", actionAt: "2026-01-02T03:05:05.999999999Z", valid: true},
		{name: "equal exclusive expiry", actionAt: "2026-01-02T03:05:06Z"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := NewFaultInjectingStore()
			signer := newTestEd25519Signer([32]byte{9, 8, 7, 6})
			verifier := newTestRawVerifier(RoleWriter, signer)
			verifier.mutateIdentity = func(identity *OIDCIdentity) {
				identity.NotBefore = time.Date(2026, 1, 2, 3, 4, 7, 0, time.UTC)
			}
			broker := newTestBroker(RoleWriter)
			controller, err := newTestController(RoleWriter, store, signer, verifier, broker)
			if err != nil {
				t.Fatal(err)
			}
			body := mustOperationJSON(t, syntheticOperation())
			decision := beginOperation(t, controller, body, 1)
			var capability BrokerCapabilityPayload
			if err := decodeCanonical(decision.Capability.Payload, MaxBrokerPayloadBytes, &capability); err != nil {
				t.Fatal(err)
			}
			if capability.AuthorizationIssuedAtUTC != "2026-01-02T03:04:06Z" ||
				capability.TokenNotBeforeUTC != "2026-01-02T03:04:07Z" ||
				capability.AuthorizationNotAfterUTC != "2026-01-02T03:05:06Z" {
				t.Fatalf("capability did not bind the delayed not-before interval: %#v", capability)
			}
			readback := makeReadback(t, broker, body, decision, markerFixture(0), []byte("opaque-sealed"), func(payload *BrokerReadbackPayload) {
				payload.BrokerActionAtUTC = test.actionAt
			})
			rawJWT, envelope := testCall(t, controller, body, MethodBrokerReadback, 2)
			snapshot, err := controller.AcceptBrokerReadback(context.Background(), rawJWT, envelope, body, readback)
			if test.valid {
				if err != nil || snapshot.State != StateSideEffectStarted {
					t.Fatalf("valid delayed-not-before broker action instant rejected: %#v err=%v", snapshot, err)
				}
				return
			}
			if !errors.Is(err, ErrQuarantined) || snapshot.State != StateQuarantined ||
				!strings.Contains(snapshot.QuarantineReason, "inclusive-issued-and-not-before/exclusive-expiry") {
				t.Fatalf("out-of-window delayed-not-before broker action instant accepted: %#v err=%v", snapshot, err)
			}
		})
	}
}

func TestAuthorityNeverReceivesMarkerPreimageAndErasesSealedMaterial(t *testing.T) {
	store := NewFaultInjectingStore()
	controller, signer, _, broker := testController(t, store)
	body := mustOperationJSON(t, syntheticOperation())
	decision := beginOperation(t, controller, body, 1)
	if decision.Snapshot.MarkerSHA256 != strings.Repeat("0", 64) || decision.Snapshot.SealedReconciliationPresent {
		t.Fatal("authorization synthesized marker authority before broker readback")
	}
	marker := markerFixture(0)
	readback := makeReadback(t, broker, body, decision, marker, []byte("opaque-sealed-reconciliation"), nil)
	readJWT, readEnvelope := testCall(t, controller, body, MethodBrokerReadback, 2)
	readSnapshot, err := controller.AcceptBrokerReadback(context.Background(), readJWT, readEnvelope, body, readback)
	if err != nil {
		t.Fatal(err)
	}
	expectedDigest := sha256.Sum256(marker[:])
	if readSnapshot.MarkerSHA256 != SHA256Hex(expectedDigest) || !readSnapshot.SealedReconciliationPresent {
		t.Fatalf("broker readback not durably bound: %#v", readSnapshot)
	}
	if bytes.Contains(readSnapshot.CapabilityPayload, marker[:]) || bytes.Contains(readSnapshot.ReceiptPayload, marker[:]) {
		t.Fatal("authority snapshot exposed marker preimage")
	}
	commitJWT, commitEnvelope := testCall(t, controller, body, MethodTerminalProof, 3)
	terminal, err := controller.CommitTerminalProof(context.Background(), commitJWT, commitEnvelope, body)
	if err != nil {
		t.Fatal(err)
	}
	if terminal.State != StateTerminal || terminal.SealedReconciliationPresent || !terminal.SealedReconciliationErased || store.ErasedBytes() != len("opaque-sealed-reconciliation") {
		t.Fatalf("transactional erasure failed: %#v erased=%d", terminal, store.ErasedBytes())
	}
	if err := VerifyCanonical(signer.PublicKey(), DomainWriterReceipt, terminal.ReceiptPayload, terminal.ReceiptSignature); err != nil {
		t.Fatal(err)
	}
	retryJWT, retryEnvelope := testCall(t, controller, body, MethodTerminalProof, 4)
	retry, err := controller.CommitTerminalProof(context.Background(), retryJWT, retryEnvelope, body)
	if err != nil || !bytes.Equal(retry.ReceiptPayload, terminal.ReceiptPayload) || !bytes.Equal(retry.ReceiptSignature, terminal.ReceiptSignature) {
		t.Fatalf("terminal proof was not deterministic/idempotent: %#v %v", retry, err)
	}
	statusJWT, statusEnvelope := testCall(t, controller, body, MethodStatus, 5)
	status, err := controller.Status(context.Background(), statusJWT, statusEnvelope, body)
	if err != nil || !bytes.Equal(status.ReceiptPayload, terminal.ReceiptPayload) || !bytes.Equal(status.ReceiptSignature, terminal.ReceiptSignature) {
		t.Fatalf("status did not reconcile persisted terminal receipt: %#v %v", status, err)
	}
}

func TestDifferentBrokerReadbackReuseQuarantines(t *testing.T) {
	store := NewFaultInjectingStore()
	controller, _, _, broker := testController(t, store)
	body := mustOperationJSON(t, syntheticOperation())
	decision := beginOperation(t, controller, body, 1)
	first := makeReadback(t, broker, body, decision, markerFixture(0), []byte("opaque-one"), nil)
	rawJWT, envelope := testCall(t, controller, body, MethodBrokerReadback, 2)
	if _, err := controller.AcceptBrokerReadback(context.Background(), rawJWT, envelope, body, first); err != nil {
		t.Fatal(err)
	}
	rawJWT, envelope = testCall(t, controller, body, MethodBrokerReadback, 3)
	if snapshot, err := controller.AcceptBrokerReadback(context.Background(), rawJWT, envelope, body, first); err != nil || snapshot.State != StateSideEffectStarted {
		t.Fatalf("identical broker readback was not idempotent: %#v %v", snapshot, err)
	}
	second := makeReadback(t, broker, body, decision, markerFixture(1), []byte("opaque-two"), nil)
	rawJWT, envelope = testCall(t, controller, body, MethodBrokerReadback, 4)
	if snapshot, err := controller.AcceptBrokerReadback(context.Background(), rawJWT, envelope, body, second); !errors.Is(err, ErrQuarantined) || snapshot.State != StateQuarantined {
		t.Fatalf("different readback reuse not quarantined: %#v %v", snapshot, err)
	}
}

func TestTerminalBrokerReadbackIsImmutableBeforeNewBytesAreParsed(t *testing.T) {
	t.Run("same body malformed readback", func(t *testing.T) {
		store := NewFaultInjectingStore()
		controller, _, _, broker := testController(t, store)
		body := mustOperationJSON(t, syntheticOperation())
		decision := beginOperation(t, controller, body, 1)
		acceptReadback(t, controller, broker, body, decision, 2)
		commitJWT, commitEnvelope := testCall(t, controller, body, MethodTerminalProof, 3)
		terminal, err := controller.CommitTerminalProof(context.Background(), commitJWT, commitEnvelope, body)
		if err != nil {
			t.Fatal(err)
		}

		rawJWT, envelope := testCall(t, controller, body, MethodBrokerReadback, 4)
		got, err := controller.AcceptBrokerReadback(context.Background(), rawJWT, envelope, body, BrokerReadback{
			Payload: []byte("not-json"), Signature: []byte("invalid"), SealedReconciliation: nil,
		})
		if !errors.Is(err, ErrStateTransition) || got.State != StateTerminal || got.Revision != terminal.Revision ||
			got.QuarantineReason != "" || !bytes.Equal(got.ReceiptPayload, terminal.ReceiptPayload) ||
			!bytes.Equal(got.ReceiptSignature, terminal.ReceiptSignature) {
			t.Fatalf("later malformed readback changed terminal record: %#v err=%v", got, err)
		}
	})

	t.Run("different body still quarantines", func(t *testing.T) {
		store := NewFaultInjectingStore()
		controller, _, _, broker := testController(t, store)
		body := mustOperationJSON(t, syntheticOperation())
		decision := beginOperation(t, controller, body, 10)
		acceptReadback(t, controller, broker, body, decision, 11)
		commitJWT, commitEnvelope := testCall(t, controller, body, MethodTerminalProof, 12)
		if _, err := controller.CommitTerminalProof(context.Background(), commitJWT, commitEnvelope, body); err != nil {
			t.Fatal(err)
		}
		changed := syntheticOperation()
		changed.ImageSHA256 = strings.Repeat("9", 64)
		changedBody := mustOperationJSON(t, changed)
		rawJWT, envelope := testCall(t, controller, changedBody, MethodBrokerReadback, 13)
		got, err := controller.AcceptBrokerReadback(context.Background(), rawJWT, envelope, changedBody, BrokerReadback{})
		if !errors.Is(err, ErrOperationReuse) || got.State != StateQuarantined {
			t.Fatalf("different terminal body reuse did not quarantine: %#v err=%v", got, err)
		}
	})
}

func TestTerminalCommitCrashPointsAreTransactional(t *testing.T) {
	for _, fault := range []FaultPoint{FaultBeforeCommit, FaultAfterCommit} {
		t.Run(string(fault), func(t *testing.T) {
			store := NewFaultInjectingStore()
			controller, _, _, broker := testController(t, store)
			body := mustOperationJSON(t, syntheticOperation())
			decision := beginOperation(t, controller, body, 10)
			acceptReadback(t, controller, broker, body, decision, 11)
			store.Inject(fault, 1)
			rawJWT, envelope := testCall(t, controller, body, MethodTerminalProof, 12)
			if _, err := controller.CommitTerminalProof(context.Background(), rawJWT, envelope, body); !errors.Is(err, ErrInjectedCrash) {
				t.Fatalf("expected injected crash, got %v", err)
			}
			if _, err := controller.CommitTerminalProof(context.Background(), rawJWT, envelope, body); !errors.Is(err, ErrReplay) {
				t.Fatalf("ambiguous terminal outcome rolled back authentication burn: %v", err)
			}
			statusJWT, statusEnvelope := testCall(t, controller, body, MethodStatus, 13)
			status, err := controller.Status(context.Background(), statusJWT, statusEnvelope, body)
			if err != nil {
				t.Fatal(err)
			}
			if fault == FaultBeforeCommit && (status.State != StateSideEffectStarted || !status.SealedReconciliationPresent) {
				t.Fatalf("before-commit crash advanced erasure: %#v", status)
			}
			if fault == FaultAfterCommit && (status.State != StateTerminal || !status.SealedReconciliationErased) {
				t.Fatalf("after-commit crash lost terminal erasure: %#v", status)
			}
		})
	}
}

func TestAuditSequenceIsAppendOnlyAndExact(t *testing.T) {
	store := NewFaultInjectingStore()
	controller, _, _, _ := testController(t, store)
	body := mustOperationJSON(t, syntheticOperation())
	beginOperation(t, controller, body, 1)
	rawJWT, envelope := testCall(t, controller, body, MethodStatus, 2)
	_, _ = controller.Status(context.Background(), rawJWT, envelope, body)
	audit := store.Audit()
	if len(audit) != 3 {
		t.Fatalf("unexpected audit length %d", len(audit))
	}
	for index, event := range audit {
		if event.Sequence != uint64(index+1) || event.OperationID != "synthetic-operation-001" || event.CallSHA256 == strings.Repeat("0", 64) {
			t.Fatalf("invalid audit event %#v", event)
		}
	}
}
