package model

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var protocolNow = time.Date(2032, 3, 4, 5, 6, 7, 123456789, time.UTC)
var protocolFixtureSequence atomic.Uint64

type authSequence struct{ next atomic.Uint64 }

func testAuthorityChallenge(label string, role UnitRole) AuthorityChallenge {
	entropy := HashString("synthetic-authority-challenge/v2\x00" + string(role) + "\x00" + label)
	seedByte, domain := byte(0x11), "writer-root/v2\x00"
	if role == ObserverAuthorityRole {
		seedByte, domain = byte(0x22), "observer-root/v2\x00"
	}
	privateKey := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{seedByte}, ed25519.SeedSize))
	root := HashBytes(append([]byte(domain), privateKey.Public().(ed25519.PublicKey)...))
	challenge, err := issueAuthorityChallenge(role, root, bytes.NewReader(entropy[:]))
	if err != nil {
		panic(err)
	}
	return challenge
}

func (s *authSequence) authFor(role UnitRole) AuthEnvelope {
	n := s.next.Add(1)
	return newAuthEnvelope(
		fmt.Sprintf("synthetic-jti-%016d", n),
		testAuthorityChallenge(fmt.Sprintf("%016d", n), role),
		protocolNow.Add(-time.Minute), protocolNow.Add(-time.Minute), protocolNow.Add(4*time.Minute),
	)
}

func (s *authSequence) auth() AuthEnvelope         { return s.authFor(WriterAuthorityRole) }
func (s *authSequence) observerAuth() AuthEnvelope { return s.authFor(ObserverAuthorityRole) }

func (s *authSequence) issued(effects *ProtocolCrashOracle, request MutationRequest, call CallKind) AuthEnvelope {
	if effects == nil || effects.signerRole == "" || effects.signerRoot.IsZero() {
		panic("synthetic challenge issuer is not bound")
	}
	n := s.next.Add(1)
	entropy := HashString(fmt.Sprintf("synthetic-bound-authority-challenge/v2\x00%s\x00%016d", effects.signerRole, n))
	challenge, err := effects.issueAuthorityChallenge(context.Background(), request, call, effects.signerRole, effects.signerRoot, bytes.NewReader(entropy[:]))
	if err != nil {
		panic(err)
	}
	return newAuthEnvelope(
		fmt.Sprintf("synthetic-jti-%016d", n), challenge,
		protocolNow.Add(-time.Minute), protocolNow.Add(-time.Minute), protocolNow.Add(4*time.Minute),
	)
}

type manualObservationClock struct {
	now  time.Time
	wait time.Duration
}

func (c *manualObservationClock) Now() time.Time           { return c.now }
func (c *manualObservationClock) Wait(delay time.Duration) { c.now = c.now.Add(delay + c.wait) }

type protocolFixture struct {
	writerAuthority, writerBroker, observerAuthority, observerBroker SubstrateUnit
	writerPrivate, observerPrivate                                   ed25519.PrivateKey
	boundary                                                         *ClosedBoundary
	effects                                                          *ProtocolCrashOracle
	store                                                            *SyntheticMarkerStore
	sealer                                                           *SyntheticMarkerSealer
	writerProvider                                                   *SyntheticWriterProvider
	body                                                             OperationBody
	auth                                                             authSequence
	clock                                                            *manualObservationClock
}

func newProtocolFixture(t *testing.T) *protocolFixture {
	t.Helper()
	fixtureID := protocolFixtureSequence.Add(1)
	unit := func(role UnitRole, domain NetworkDomain, id string) SubstrateUnit {
		value, err := NewSubstrateUnit(role, domain, id)
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	writerPrivate := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x11}, ed25519.SeedSize))
	observerPrivate := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x22}, ed25519.SeedSize))
	f := &protocolFixture{
		writerAuthority:   unit(WriterAuthorityRole, NoDataDomain, "synthetic://app/writer-authority"),
		writerBroker:      unit(WriterBrokerRole, SourceDomain, "synthetic://app/writer-broker"),
		observerAuthority: unit(ObserverAuthorityRole, NoDataDomain, "synthetic://app/observer-authority"),
		observerBroker:    unit(ObserverBrokerRole, RecoveryDomain, "synthetic://app/observer-broker"),
		writerPrivate:     writerPrivate, observerPrivate: observerPrivate,
		store: NewSyntheticMarkerStore(), body: testOperation("synthetic-protocol-operation"),
		clock: &manualObservationClock{now: protocolNow},
	}
	var err error
	f.boundary, err = NewClosedBoundary(f.writerAuthority, f.writerBroker, f.observerAuthority, f.observerBroker,
		fmt.Sprintf("synthetic://ledger/writer-%d", fixtureID), fmt.Sprintf("synthetic://ledger/observer-%d", fixtureID), fmt.Sprintf("synthetic://unit/provider-observer-%d", fixtureID),
		writerPrivate.Public().(ed25519.PublicKey), observerPrivate.Public().(ed25519.PublicKey))
	if err != nil {
		t.Fatal(err)
	}
	f.effects, err = newBoundProtocolCrashOracle(f.clock.Now, f.boundary.writerStore)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.effects.bindSigner(WriterAuthorityRole, f.writerPrivate, f.boundary.writerLedger, f.boundary.writerRoot); err != nil {
		t.Fatal(err)
	}
	f.sealer, err = NewSyntheticMarkerSealer()
	if err != nil {
		t.Fatal(err)
	}
	f.writerProvider, err = NewSyntheticWriterProvider(f.store.sourceBindingSHA256())
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func markerEntropy() []byte {
	result := make([]byte, 32)
	for i := range result {
		result[i] = byte(i + 1)
	}
	return result
}

func markerRequestForWriter(t *testing.T, writer *WriterProtocol, body OperationBody) MutationRequest {
	t.Helper()
	return markerRequest(body, writer.broker.target)
}

func markerAuth(f *protocolFixture, writer *WriterProtocol, body OperationBody, call CallKind) AuthEnvelope {
	return f.auth.issued(f.effects, markerRequest(body, writer.broker.target), call)
}

func markerAbortAuth(f *protocolFixture, controller *MarkerAbortController, body OperationBody, call CallKind) AuthEnvelope {
	return f.auth.issued(f.effects, markerAbortRequest(body, controller.sealer.BindingSHA256()), call)
}

func writerAbortAuth(f *protocolFixture, writer *WriterProtocol, body OperationBody, call CallKind) AuthEnvelope {
	controller, err := writer.abortController()
	if err != nil {
		panic(err)
	}
	return markerAbortAuth(f, controller, body, call)
}

func writerLifecycleAuth(f *protocolFixture, lifecycle *WriterLifecycle, effect Effect, call CallKind) AuthEnvelope {
	return f.auth.issued(f.effects, lifecycle.request(effect), call)
}

func wrappingKeySealerAuth(f *protocolFixture, lifecycle *WriterLifecycle, call CallKind) AuthEnvelope {
	return f.auth.issued(f.effects, lifecycle.wrappingKeySealerRequest(), call)
}

func terminalObservationAuth(f *protocolFixture, lifecycle *WriterLifecycle, call CallKind) AuthEnvelope {
	return f.auth.issued(f.effects, lifecycle.terminalObservationRequest(), call)
}

func terminalPublicationAuth(f *protocolFixture, lifecycle *WriterLifecycle, evidence WriterTerminalEvidence, call CallKind) AuthEnvelope {
	digest, err := evidence.Digest()
	if err != nil {
		panic(err)
	}
	request := derivedEffectRequest(lifecycle.base, lifecycle.projectionDigest, EffectEvidencePublish, digest)
	return f.auth.issued(f.effects, request, call)
}

func forkPostAuth(f *protocolFixture, controller *ForkController, call CallKind) AuthEnvelope {
	return f.auth.issued(f.effects, controller.request, call)
}

func forkAuthorizationAuth(f *protocolFixture, controller *ForkController, call CallKind) AuthEnvelope {
	return f.auth.issued(f.effects, controller.authorizationRequest, call)
}

func observerEvidenceAuth(f *protocolFixture, path *recoveryPath, call CallKind) AuthEnvelope {
	request, err := observerEvidenceRequest(path.observer.base, path.admission.Digest())
	if err != nil {
		panic(err)
	}
	return f.auth.issued(path.observerOracle, request, call)
}

func recoveryAdmissionAuth(f *protocolFixture, oracle *ProtocolCrashOracle, request ObserverAdmissionRequest, call CallKind) AuthEnvelope {
	publicationRequest, err := recoveryAdmissionSignatureRequest(request)
	if err != nil {
		panic(err)
	}
	return f.auth.issued(oracle, publicationRequest, call)
}

func (f *protocolFixture) forkProvider(t *testing.T, result Digest) *SyntheticForkProvider {
	t.Helper()
	provider, err := NewSyntheticForkProvider(f.store, result)
	if err != nil {
		t.Fatal(err)
	}
	return provider
}

func (f *protocolFixture) forkObserver(t *testing.T, provider *SyntheticForkProvider) *SyntheticForkObserver {
	t.Helper()
	facade, err := provider.FactsFacade()
	if err != nil {
		t.Fatal(err)
	}
	observer, err := NewSyntheticForkObserver(facade, f.boundary.providerObserver, f.clock.Now)
	if err != nil {
		t.Fatal(err)
	}
	return observer
}

func (f *protocolFixture) rawWriter(t *testing.T, entropy []byte) *WriterProtocol {
	t.Helper()
	authority, err := NewWriterAuthority(f.writerAuthority, f.writerPrivate)
	if err != nil {
		t.Fatal(err)
	}
	broker, err := NewWriterBroker(f.writerBroker, bytes.NewReader(entropy), f.store.SourceStore(), f.sealer)
	if err != nil {
		t.Fatal(err)
	}
	writer, err := NewWriterProtocol(f.boundary, f.effects, authority, broker)
	if err != nil {
		t.Fatal(err)
	}
	return writer
}

func (f *protocolFixture) writer(t *testing.T, entropy []byte) *WriterProtocol {
	t.Helper()
	writer := f.rawWriter(t, entropy)
	admission, err := NewWriterAdmission(writer, f.body, originalProjection(), f.writerProvider)
	if err != nil {
		t.Fatal(err)
	}
	for _, effect := range writerAdmissionEffects {
		if err := admission.Apply(context.Background(), effect, f.auth.issued(f.effects, admission.request(effect), MutationCall), FaultNone); err != nil {
			t.Fatalf("admission %s: %v", effect, err)
		}
	}
	if !admission.Ready() {
		t.Fatal("writer admission incomplete")
	}
	return writer
}

func originalProjection() PreOperationProjection {
	return PreOperationProjection{
		FirewallSHA256:     HashString("synthetic-original-firewall"),
		ActionLedgerSHA256: HashString("synthetic-original-action-ledger"),
		ProvenanceSHA256:   HashString("synthetic-provider-get-provenance"),
		CapturedAt:         time.Date(2032, 3, 4, 1, 0, 0, 0, time.UTC),
	}
}

func newWriterLifecycleForTest(t *testing.T, f *protocolFixture, writer *WriterProtocol, body OperationBody, projection PreOperationProjection) (*WriterLifecycle, *SyntheticWriterLifecycleProvider, *SyntheticWriterTerminalObserver) {
	t.Helper()
	config := HashString("synthetic-writer-lifecycle-config/v2")
	provider, err := NewSyntheticWriterLifecycleProvider(body, writer.broker.target, config, projection, f.clock.Now)
	if err != nil {
		t.Fatal(err)
	}
	facade, err := provider.TerminalFactsFacade()
	if err != nil {
		t.Fatal(err)
	}
	observer, err := NewSyntheticWriterTerminalObserver(facade, f.clock, writer.boundary.providerObserver)
	if err != nil {
		t.Fatal(err)
	}
	lifecycle, err := NewWriterLifecycle(writer, body, projection, provider, observer)
	if err != nil {
		t.Fatal(err)
	}
	return lifecycle, provider, observer
}

func recordLifecycleRevocationForTest(t *testing.T, lifecycle *WriterLifecycle, f *protocolFixture, kind RevocationKind, fault FaultMode) error {
	t.Helper()
	if kind == RevokeWrappingKey {
		if err := lifecycle.RecordWrappingKeySealerRevocation(context.Background(), f.auth.issued(f.effects, lifecycle.wrappingKeySealerRequest(), MutationCall), FaultNone); err != nil {
			return err
		}
	}
	effect, ok := revocationEffect(kind)
	if !ok {
		return ErrInvalid
	}
	return lifecycle.RecordRevocation(context.Background(), kind, f.auth.issued(f.effects, lifecycle.request(effect), MutationCall), fault)
}

func elapseWriterOldInstanceGrace(f *protocolFixture) {
	f.clock.now = f.clock.now.Add(syntheticWriterOldInstanceGrace)
}

func prepareTerminal(t *testing.T, f *protocolFixture, deleteFault DeleteResult, evidenceFault FaultMode) (*WriterProtocol, WriterTerminalEvidence, WriterTerminalReceipt) {
	t.Helper()
	ctx := context.Background()
	writer := f.writer(t, markerEntropy())
	if _, err := writer.GenerateCommitAndReadback(ctx, f.body, f.auth.issued(f.effects, markerRequestForWriter(t, writer, f.body), MutationCall), FaultNone); err != nil {
		t.Fatal(err)
	}
	projection := originalProjection()
	lifecycle, provider, observer := newWriterLifecycleForTest(t, f, writer, f.body, projection)
	for _, kind := range []RevocationKind{RevokeCapability, RevokeLeaf, RevokeMTLS, RevokeWrappingKey, RemoveBinding, RemoveCredential} {
		if err := recordLifecycleRevocationForTest(t, lifecycle, f, kind, FaultNone); err != nil {
			t.Fatalf("%s: %v", kind, err)
		}
	}
	if err := lifecycle.RecordFullRedeploy(ctx, f.auth.issued(f.effects, lifecycle.request(EffectFullRedeploy), MutationCall), FaultNone); err != nil {
		t.Fatal(err)
	}
	if err := lifecycle.RecordFirewallRestored(ctx, f.auth.issued(f.effects, lifecycle.request(EffectTrustedSourceDel), MutationCall), FaultNone); err != nil {
		t.Fatal(err)
	}
	err := lifecycle.RequestDelete(ctx, deleteFault, f.auth.issued(f.effects, lifecycle.request(EffectBrokerDelete), MutationCall))
	if deleteFault == DeleteDefinitiveSuccess {
		if err != nil {
			t.Fatal(err)
		}
	} else {
		if !errors.Is(err, ErrReconcileRequired) {
			t.Fatalf("ambiguous delete: %v", err)
		}
		if err := lifecycle.ReconcileDelete(ctx, f.auth.issued(f.effects, lifecycle.request(EffectBrokerDelete), StatusCall)); err != nil {
			t.Fatal(err)
		}
	}
	elapseWriterOldInstanceGrace(f)
	evidence, err := lifecycle.ObserveTerminalEvidence(context.Background(), f.auth.issued(f.effects, lifecycle.terminalObservationRequest(), MutationCall), FaultNone)
	if err != nil {
		t.Fatal(err)
	}
	evidenceDigest, _ := evidence.Digest()
	terminalRequest := derivedEffectRequest(lifecycle.base, lifecycle.projectionDigest, EffectEvidencePublish, evidenceDigest)
	receipt, err := lifecycle.AcceptTerminalEvidence(ctx, evidence, f.auth.issued(f.effects, terminalRequest, MutationCall), evidenceFault)
	if evidenceFault == FaultNone {
		if err != nil {
			t.Fatal(err)
		}
	} else {
		if !IsAmbiguousError(err) {
			t.Fatalf("terminal ambiguity: %v", err)
		}
		lifecycle, err = NewWriterLifecycle(writer, f.body, projection, provider, observer)
		if err != nil {
			t.Fatal(err)
		}
		receipt, err = lifecycle.ReconcileTerminalEvidence(ctx, evidence, f.auth.issued(f.effects, terminalRequest, StatusCall))
		if err != nil {
			t.Fatal(err)
		}
	}
	return writer, evidence, receipt
}

func TestClosedBoundaryRequiresFourAppsTwoLedgersAndTwoRootsPairwiseDistinct(t *testing.T) {
	f := newProtocolFixture(t)
	if f.boundary.writerLedger == f.boundary.observerLedger || f.boundary.writerRoot == f.boundary.observerRoot {
		t.Fatal("boundary identities collided")
	}
	_, err := NewClosedBoundary(f.writerAuthority, f.writerBroker, f.observerAuthority, f.observerBroker,
		"synthetic://ledger/shared", "synthetic://ledger/shared", "synthetic://unit/provider-observer", f.writerPrivate.Public().(ed25519.PublicKey), f.observerPrivate.Public().(ed25519.PublicKey))
	if !errors.Is(err, ErrRoleIsolation) {
		t.Fatalf("shared ledger accepted: %v", err)
	}
	_, err = NewClosedBoundary(f.writerAuthority, f.writerBroker, f.observerAuthority, f.observerBroker,
		"synthetic://ledger/writer", "synthetic://ledger/observer", "synthetic://unit/provider-observer", f.writerPrivate.Public().(ed25519.PublicKey), f.writerPrivate.Public().(ed25519.PublicKey))
	if !errors.Is(err, ErrRoleIsolation) {
		t.Fatalf("shared root accepted: %v", err)
	}
	_, err = NewClosedBoundary(f.writerAuthority, f.writerBroker, f.observerAuthority, f.observerBroker,
		"synthetic://app/writer-authority", "synthetic://ledger/observer", "synthetic://unit/provider-observer", f.writerPrivate.Public().(ed25519.PublicKey), f.observerPrivate.Public().(ed25519.PublicKey))
	if !errors.Is(err, ErrRoleIsolation) {
		t.Fatalf("ledger/app alias accepted: %v", err)
	}
	_, err = NewClosedBoundary(f.writerAuthority, f.writerBroker, f.observerAuthority, f.observerBroker,
		"synthetic://ledger/writer", "synthetic://ledger/observer", "synthetic://app/writer-authority", f.writerPrivate.Public().(ed25519.PublicKey), f.observerPrivate.Public().(ed25519.PublicKey))
	if !errors.Is(err, ErrRoleIsolation) {
		t.Fatalf("provider observer/app alias accepted: %v", err)
	}
	boundaryType := reflect.TypeOf(*f.boundary)
	rootFields, publicKeyFields := 0, 0
	for index := 0; index < boundaryType.NumField(); index++ {
		field := boundaryType.Field(index)
		if strings.HasSuffix(field.Name, "Root") {
			rootFields++
		}
		if strings.HasSuffix(field.Name, "Public") {
			publicKeyFields++
		}
	}
	if rootFields != 2 || publicKeyFields != 2 {
		t.Fatalf("boundary must contain exactly two signing roots/public keys: roots=%d public=%d", rootFields, publicKeyFields)
	}
	for _, forbidden := range []string{"providerRoot", "providerPublic"} {
		if _, present := boundaryType.FieldByName(forbidden); present {
			t.Fatalf("third provider signing-root field %q exists", forbidden)
		}
	}
	if _, err := NewSubstrateUnit(ObserverBrokerRole, SourceDomain, "synthetic://wrong-domain"); !errors.Is(err, ErrRoleIsolation) {
		t.Fatalf("source observer: %v", err)
	}
}

func TestAuthoritiesIssueOpaqueRoleBoundChallengesForWriterAndObserverFlows(t *testing.T) {
	f := newProtocolFixture(t)
	writerAuthority, err := NewWriterAuthority(f.writerAuthority, f.writerPrivate)
	if err != nil {
		t.Fatal(err)
	}
	observerAuthority, err := NewObserverAuthority(f.observerAuthority, f.observerPrivate)
	if err != nil {
		t.Fatal(err)
	}
	writer := f.writer(t, markerEntropy())
	writerRequest := markerRequestForWriter(t, writer, f.body)
	writerChallenge, err := writerAuthority.IssueChallenge(context.Background(), f.effects, writerRequest, MutationCall)
	if err != nil {
		t.Fatal(err)
	}
	secondWriterChallenge, err := writerAuthority.IssueChallenge(context.Background(), f.effects, writerRequest, MutationCall)
	if err != nil {
		t.Fatal(err)
	}
	// Bind the observer authority to its distinct ledger before issuing.
	observerFixture := newProtocolFixture(t)
	path := prepareRecoveryPath(t, observerFixture)
	observerRequest, err := observerEvidenceRequest(path.observer.base, path.admission.Digest())
	if err != nil {
		t.Fatal(err)
	}
	observerChallenge, err := observerAuthority.IssueChallenge(context.Background(), path.observerOracle, observerRequest, MutationCall)
	if err != nil {
		t.Fatal(err)
	}
	if writerChallenge == secondWriterChallenge || writerChallenge.Hash().IsZero() || observerChallenge.Hash().IsZero() {
		t.Fatal("authority-issued 256-bit challenges are missing or repeated")
	}
	if writerChallenge.validateFor(WriterAuthorityRole, f.boundary.writerRoot) != nil ||
		observerChallenge.validateFor(ObserverAuthorityRole, f.boundary.observerRoot) != nil {
		t.Fatal("authority-issued challenge rejected its exact role/root")
	}
	if writerChallenge.validateFor(ObserverAuthorityRole, f.boundary.observerRoot) == nil ||
		observerChallenge.validateFor(WriterAuthorityRole, f.boundary.writerRoot) == nil {
		t.Fatal("cross-role challenge substitution accepted")
	}
	challengeType := reflect.TypeOf(writerChallenge)
	for index := 0; index < challengeType.NumField(); index++ {
		if challengeType.Field(index).IsExported() {
			t.Fatalf("caller can construct challenge field %s", challengeType.Field(index).Name)
		}
	}
	rawNonce := fmt.Sprintf("%x", writerChallenge.state.nonce)
	for _, format := range []string{"%s", "%q", "%v", "%+v", "%#v", "%d", "%x", "%X", "%o", "%b", "%100d", "%.3x"} {
		if rendered := fmt.Sprintf(format, writerChallenge); rendered != authorityChallengeRedaction ||
			strings.Contains(rendered, rawNonce) || strings.Contains(rendered, f.boundary.writerRoot.String()) {
			t.Fatalf("challenge formatter %q did not redact bearer material: %q", format, rendered)
		}
	}
	for _, format := range []string{
		"%p", "%+p", "%#p", "%020p", "%.3p", "%100.3p", "%[1]p",
		"%w", "%+w", "%#w", "%100w", "%.3w", "%100.3w", "%[1]w",
		"%T", "%t", "%c", "%U", "%e", "%E", "%f", "%F", "%g", "%G",
	} {
		for _, value := range []any{writerChallenge, &writerChallenge} {
			if rendered := fmt.Sprintf(format, value); strings.Contains(rendered, rawNonce) ||
				strings.Contains(rendered, f.boundary.writerRoot.String()) {
				t.Fatalf("challenge special formatter %q leaked bearer material: %q", format, rendered)
			}
		}
	}
	invalidWrapFormat := "wrapped: %w"
	if rendered := fmt.Sprintf("%v", fmt.Errorf(invalidWrapFormat, writerChallenge)); strings.Contains(rendered, rawNonce) || strings.Contains(rendered, f.boundary.writerRoot.String()) {
		t.Fatalf("wrapped challenge leaked bearer material: %q", rendered)
	}

	bodyDigestBefore, _ := f.body.Digest()
	writerAuth := newAuthEnvelope(
		"synthetic-authority-writer-jti", writerChallenge,
		protocolNow.Add(-time.Minute), protocolNow.Add(-time.Minute), protocolNow.Add(time.Minute),
	)
	for _, format := range []string{"%s", "%q", "%v", "%+v", "%#v", "%d", "%x", "%X", "%o", "%b", "%100d", "%.3x"} {
		if rendered := fmt.Sprintf(format, writerAuth); rendered != authEnvelopeRedaction ||
			strings.Contains(rendered, writerAuth.jtiValue()) || strings.Contains(rendered, rawNonce) {
			t.Fatalf("authentication formatter %q did not redact bearer material: %q", format, rendered)
		}
	}
	for _, format := range []string{
		"%p", "%+p", "%#p", "%020p", "%.3p", "%100.3p", "%[1]p",
		"%w", "%+w", "%#w", "%100w", "%.3w", "%100.3w", "%[1]w",
		"%T", "%t", "%c", "%U", "%e", "%E", "%f", "%F", "%g", "%G",
	} {
		for _, value := range []any{writerAuth, &writerAuth} {
			if rendered := fmt.Sprintf(format, value); strings.Contains(rendered, writerAuth.jtiValue()) ||
				strings.Contains(rendered, rawNonce) || strings.Contains(rendered, f.boundary.writerRoot.String()) {
				t.Fatalf("authentication special formatter %q leaked bearer material: %q", format, rendered)
			}
		}
	}
	if rendered := fmt.Sprintf("%v", fmt.Errorf(invalidWrapFormat, writerAuth)); strings.Contains(rendered, writerAuth.jtiValue()) || strings.Contains(rendered, rawNonce) ||
		strings.Contains(rendered, f.boundary.writerRoot.String()) {
		t.Fatalf("wrapped authentication envelope leaked bearer material: %q", rendered)
	}
	if _, err := writer.GenerateCommitAndReadback(context.Background(), f.body, writerAuth, FaultNone); err != nil {
		t.Fatalf("writer rejected authority-issued challenge: %v", err)
	}
	bodyDigestAfter, _ := f.body.Digest()
	if bodyDigestAfter != bodyDigestBefore {
		t.Fatal("fresh authority challenge changed immutable operation identity")
	}

	issuedObserverChallenge, err := path.observer.authority.IssueChallenge(context.Background(), path.observerOracle, observerRequest, MutationCall)
	if err != nil {
		t.Fatal(err)
	}
	observerNow := path.observerClock.Now()
	observerAuth := newAuthEnvelope(
		"synthetic-authority-observer-jti", issuedObserverChallenge,
		observerNow.Add(-time.Minute), observerNow.Add(-time.Minute), observerNow.Add(time.Minute),
	)
	if receipt, err := path.observer.ReadRecoveryTwice(context.Background(), observerFixture.body, observerAuth, FaultNone); err != nil || receipt.MarkerSHA256.IsZero() {
		t.Fatalf("observer rejected independently issued challenge: receipt=%+v err=%v", receipt, err)
	}
}

func TestDurableChallengeRequiresExactRequestAndCallBeforeAtomicBurn(t *testing.T) {
	f := newProtocolFixture(t)
	ctx := context.Background()
	request := testRequest(EffectBrokerCreate, 401)
	drift := request
	drift.Operation.Generation++
	drift.ParametersDigest = HashString("synthetic-cross-body-parameters")
	result := HashString("synthetic-exact-bound-result")
	applyCalls := 0
	apply := func() (Digest, error) {
		applyCalls++
		return result, nil
	}

	if _, err := f.effects.AuthorizeAndConsume(ctx, request, f.auth.auth(), FaultNone, Digest{}, apply); !errors.Is(err, ErrChallengeNotIssued) {
		t.Fatalf("caller-minted challenge accepted: %v", err)
	}
	if applyCalls != 0 {
		t.Fatal("unissued challenge reached the side effect")
	}

	bound := f.auth.issued(f.effects, request, MutationCall)
	if _, err := f.effects.AuthorizeAndConsume(ctx, drift, bound, FaultNone, Digest{}, apply); !errors.Is(err, ErrChallengeBinding) {
		t.Fatalf("cross-body challenge accepted: %v", err)
	}
	if applyCalls != 0 {
		t.Fatal("cross-body challenge reached the side effect")
	}
	if got, err := f.effects.AuthorizeAndConsume(ctx, request, bound, FaultNone, Digest{}, apply); err != nil || got != result {
		t.Fatalf("binding mismatch consumed the valid grant: got=%s err=%v", got, err)
	}
	if applyCalls != 1 {
		t.Fatalf("exact request applied %d times", applyCalls)
	}
	if _, err := f.effects.AuthorizeAndConsume(ctx, request, bound, FaultNone, Digest{}, apply); !errors.Is(err, ErrAuthReplay) {
		t.Fatalf("consumed challenge replay accepted: %v", err)
	}

	statusBound := f.auth.issued(f.effects, request, StatusCall)
	if _, err := f.effects.AuthorizeAndConsume(ctx, request, statusBound, FaultNone, Digest{}, apply); !errors.Is(err, ErrChallengeBinding) {
		t.Fatalf("status challenge crossed into mutation endpoint: %v", err)
	}
	if got, err := f.effects.ReconcileStatus(ctx, request, statusBound, func() ([]Digest, error) {
		t.Fatal("completed status unexpectedly called its observer")
		return nil, nil
	}); err != nil || got != result {
		t.Fatalf("cross-call mismatch consumed exact status grant: got=%s err=%v", got, err)
	}
	if applyCalls != 1 {
		t.Fatalf("status path repeated mutation: %d", applyCalls)
	}
}

func TestIssuedChallengeSurvivesReconstructionAndBurnSurvivesCrash(t *testing.T) {
	f, writer, lifecycle, provider, observer := lifecycleForCommittedWriter(t)
	auth := writerLifecycleAuth(f, lifecycle, EffectCapabilityRevoke, MutationCall)
	reconstructed, err := NewWriterLifecycle(writer, f.body, originalProjection(), provider, observer)
	if err != nil {
		t.Fatal(err)
	}
	if err := reconstructed.RecordRevocation(context.Background(), RevokeCapability, auth, FaultCrashAfterAuthorization); !errors.Is(err, ErrSimulatedCrash) {
		t.Fatalf("issued grant did not survive reconstruction: %v", err)
	}
	reconstructedAgain, err := NewWriterLifecycle(writer, f.body, originalProjection(), provider, observer)
	if err != nil {
		t.Fatal(err)
	}
	if err := reconstructedAgain.RecordRevocation(context.Background(), RevokeCapability, auth, FaultNone); !errors.Is(err, ErrAuthReplay) {
		t.Fatalf("crash rolled back the atomic challenge/JTI burn: %v", err)
	}
	if err := reconstructedAgain.ReconcileRevocation(context.Background(), RevokeCapability, writerLifecycleAuth(f, reconstructedAgain, EffectCapabilityRevoke, StatusCall)); !errors.Is(err, ErrNotObserved) {
		t.Fatalf("status after pre-effect crash fabricated completion: %v", err)
	}
	if apply, observe := provider.callCounts(EffectCapabilityRevoke); apply != 0 || observe != 1 {
		t.Fatalf("reconstruction repeated mutation: apply=%d observe=%d", apply, observe)
	}
}

func TestProtocolOracleObservationCannotCompleteBeforeWireEligibility(t *testing.T) {
	for index, fault := range []FaultMode{FaultCrashAfterAuthorization, FaultCrashAfterPreparation} {
		t.Run(string(fault), func(t *testing.T) {
			f := newProtocolFixture(t)
			_ = f.rawWriter(t, markerEntropy()) // bind the writer authority/root
			request := testRequest(EffectCredentialInstall, 960+index)
			observed := HashString("synthetic-stale-observation-" + string(fault))
			applyCalls := 0
			_, err := f.effects.AuthorizeAndConsume(
				context.Background(), request,
				f.auth.issued(f.effects, request, MutationCall), fault, Digest{},
				func() (Digest, error) {
					applyCalls++
					return observed, nil
				},
			)
			if !errors.Is(err, ErrSimulatedCrash) {
				t.Fatalf("setup fault = %v", err)
			}
			got, statusErr := f.effects.ReconcileStatus(
				context.Background(), request,
				f.auth.issued(f.effects, request, StatusCall),
				func() ([]Digest, error) { return []Digest{observed}, nil },
			)
			if !errors.Is(statusErr, ErrQuarantined) || !got.IsZero() {
				t.Fatalf("pre-wire observation completed: got=%s err=%v", got, statusErr)
			}
			if applyCalls != 0 || f.effects.Completed(request) {
				t.Fatalf("pre-wire record became authoritative: apply=%d completed=%v", applyCalls, f.effects.Completed(request))
			}
		})
	}
}

func TestObserverChallengeMustBeIssuedForItsExactEndpoint(t *testing.T) {
	f := newProtocolFixture(t)
	path := prepareRecoveryPath(t, f)
	request, err := observerEvidenceRequest(path.observer.base, path.admission.Digest())
	if err != nil {
		t.Fatal(err)
	}
	_, before := f.store.ReadCounts()
	if _, err := path.observer.ReadRecoveryTwice(context.Background(), f.body, f.auth.observerAuth(), FaultNone); !errors.Is(err, ErrChallengeNotIssued) {
		t.Fatalf("caller-minted observer challenge accepted: %v", err)
	}
	_, after := f.store.ReadCounts()
	if after != before {
		t.Fatal("unissued observer challenge reached recovery data")
	}

	challenge, err := path.observer.authority.IssueChallenge(context.Background(), path.observerOracle, request, StatusCall)
	if err != nil {
		t.Fatal(err)
	}
	now := path.observerClock.Now()
	statusAuth := newAuthEnvelope(
		"synthetic-observer-status-bound-jti", challenge,
		now.Add(-time.Minute), now.Add(-time.Minute), now.Add(time.Minute),
	)
	if _, err := path.observer.ReadRecoveryTwice(context.Background(), f.body, statusAuth, FaultNone); !errors.Is(err, ErrChallengeBinding) {
		t.Fatalf("status challenge crossed into observer mutation endpoint: %v", err)
	}
	if _, err := path.observerOracle.ReconcileStatus(context.Background(), request, statusAuth, func() ([]Digest, error) { return nil, nil }); !errors.Is(err, ErrInvalid) {
		t.Fatalf("exact observer status binding was not consumed at its endpoint: %v", err)
	}
	_, after = f.store.ReadCounts()
	if after != before {
		t.Fatal("observer call-kind mismatch reached recovery data")
	}

	if receipt, err := path.observer.ReadRecoveryTwice(context.Background(), f.body, observerEvidenceAuth(f, &path, MutationCall), FaultNone); err != nil || receipt.MarkerSHA256.IsZero() {
		t.Fatalf("exact observer mutation challenge failed: receipt=%+v err=%v", receipt, err)
	}
}

func TestBoundEndpointsAndLifecyclesRejectCrossRoleChallenges(t *testing.T) {
	t.Run("writer", func(t *testing.T) {
		f := newProtocolFixture(t)
		writer := f.writer(t, markerEntropy())
		markerRequest := markerRequestForWriter(t, writer, f.body)
		if _, err := writer.GenerateCommitAndReadback(context.Background(), f.body, f.auth.observerAuth(), FaultNone); !errors.Is(err, ErrInvalid) {
			t.Fatalf("writer endpoint accepted observer challenge: %v", err)
		}
		if f.effects.IssueCount(markerRequest) != 0 {
			t.Fatal("cross-role writer endpoint reached its durable side effect")
		}
		if _, err := writer.GenerateCommitAndReadback(context.Background(), f.body, f.auth.issued(f.effects, markerRequest, MutationCall), FaultNone); err != nil {
			t.Fatal(err)
		}
		lifecycle, _, _ := newWriterLifecycleForTest(t, f, writer, f.body, originalProjection())
		if err := lifecycle.RecordRevocation(context.Background(), RevokeCapability, f.auth.observerAuth(), FaultNone); !errors.Is(err, ErrInvalid) {
			t.Fatalf("writer lifecycle accepted observer challenge: %v", err)
		}
		if f.effects.IssueCount(lifecycle.request(EffectCapabilityRevoke)) != 0 {
			t.Fatal("cross-role writer lifecycle reached its durable side effect")
		}
	})

	t.Run("observer", func(t *testing.T) {
		f := newProtocolFixture(t)
		path := prepareRecoveryPath(t, f)
		cleanupEffect := observerCleanupEffects[0]
		cleanupRequest := path.lifecycle.request("cleanup", 0, cleanupEffect)
		if err := path.lifecycle.ApplyCleanup(context.Background(), cleanupEffect, f.auth.auth(), FaultNone); !errors.Is(err, ErrInvalid) {
			t.Fatalf("observer lifecycle accepted writer challenge: %v", err)
		}
		if path.observerOracle.IssueCount(cleanupRequest) != 0 {
			t.Fatal("cross-role observer lifecycle reached its durable side effect")
		}
		_, readsBefore := f.store.ReadCounts()
		if _, err := path.observer.ReadRecoveryTwice(context.Background(), f.body, f.auth.auth(), FaultNone); !errors.Is(err, ErrInvalid) {
			t.Fatalf("observer endpoint accepted writer challenge: %v", err)
		}
		_, readsAfter := f.store.ReadCounts()
		if readsAfter != readsBefore {
			t.Fatal("cross-role observer endpoint reached the recovery store")
		}
	})
}

func TestAuthorityBrokerCredentialAndDataHandlesStaySeparated(t *testing.T) {
	privateKey := reflect.TypeOf(ed25519.PrivateKey(nil))
	source := reflect.TypeOf((*SourceMarkerStore)(nil)).Elem()
	recovery := reflect.TypeOf((*RecoveryMarkerReader)(nil)).Elem()
	assertNo := func(object any, forbidden reflect.Type, interfaceType bool) {
		typeOf := reflect.TypeOf(object)
		if typeOf.Kind() == reflect.Pointer {
			typeOf = typeOf.Elem()
		}
		for index := 0; index < typeOf.NumField(); index++ {
			candidate := typeOf.Field(index).Type
			match := candidate == forbidden
			if interfaceType {
				match = candidate.Implements(forbidden)
			}
			if match {
				t.Fatalf("%s has %s", typeOf.Name(), typeOf.Field(index).Name)
			}
		}
	}
	assertNo(WriterAuthority{}, source, true)
	assertNo(WriterBroker{}, privateKey, false)
	assertNo(ObserverAuthority{}, recovery, true)
	assertNo(ObserverBroker{}, privateKey, false)
	assertNo(SyntheticForkObserver{}, privateKey, false)
	assertNo(SyntheticWriterTerminalObserver{}, privateKey, false)
}

func TestFixedOneKeyCASDistinguishesNilEmptyAndDigest(t *testing.T) {
	marker := [32]byte{1, 2, 3}
	absentHarness := NewSyntheticMarkerStore()
	absent := absentHarness.SourceStore()
	if err := absent.CompareAndSwapSource(EmptyPredecessor(), marker); !errors.Is(err, ErrPredecessorMismatch) {
		t.Fatalf("empty matched absent: %v", err)
	}
	if err := absent.CompareAndSwapSource(NilPredecessor(), marker); err != nil {
		t.Fatal(err)
	}
	presentEmptyHarness := NewSyntheticMarkerStore()
	presentEmptyHarness.SeedSource(nil, true)
	presentEmpty := presentEmptyHarness.SourceStore()
	if err := presentEmpty.CompareAndSwapSource(NilPredecessor(), marker); !errors.Is(err, ErrPredecessorMismatch) {
		t.Fatalf("nil matched empty: %v", err)
	}
	if err := presentEmpty.CompareAndSwapSource(EmptyPredecessor(), marker); err != nil {
		t.Fatal(err)
	}
	presentHarness := NewSyntheticMarkerStore()
	presentHarness.SeedSource([]byte("synthetic-prior"), true)
	present := presentHarness.SourceStore()
	if err := present.CompareAndSwapSource(DigestPredecessor(HashString("synthetic-wrong")), marker); !errors.Is(err, ErrPredecessorMismatch) {
		t.Fatalf("wrong digest: %v", err)
	}
	if err := present.CompareAndSwapSource(DigestPredecessor(HashBytes([]byte("synthetic-prior"))), marker); err != nil {
		t.Fatal(err)
	}
	if FixedCASV2Script != "rereply-fixed-one-key-cas/v2" || FixedMarkerKey != "synthetic:rereply:recovery:sentinel:v2" {
		t.Fatal("fixed CAS drift")
	}
}

func TestMarkerCASAmbiguitySurvivesControllerReconstruction(t *testing.T) {
	f := newProtocolFixture(t)
	ctx := context.Background()
	writer := f.writer(t, markerEntropy())
	if _, err := writer.GenerateCommitAndReadback(ctx, f.body, markerAuth(f, writer, f.body, MutationCall), FaultHTTP408AfterEffect); !errors.Is(err, ErrReconcileRequired) {
		t.Fatalf("after-effect fault: %v", err)
	}
	reconstructed := f.writer(t, bytes.Repeat([]byte{0xaa}, 32))
	if _, err := reconstructed.GenerateCommitAndReadback(ctx, f.body, markerAuth(f, reconstructed, f.body, MutationCall), FaultNone); !errors.Is(err, ErrReconcileRequired) {
		t.Fatalf("blind reissue: %v", err)
	}
	receipt, err := reconstructed.ReconcileCommit(ctx, f.body, markerAuth(f, reconstructed, f.body, StatusCall))
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyWriterReceipt(f.boundary, receipt); err != nil {
		t.Fatal(err)
	}
	request := markerRequestForWriter(t, writer, f.body)
	if f.effects.IssueCount(request) != 1 {
		t.Fatalf("marker issue count %d", f.effects.IssueCount(request))
	}
	stored, exists, _ := f.store.readSourceForTest()
	if !exists || len(stored) != 32 || receipt.MarkerSHA256 != HashBytes(stored) {
		t.Fatal("signed source readback mismatch")
	}
	if !reconstructed.SealedMarkerPresent(f.body) {
		t.Fatal("sealed reconciliation material missing")
	}
	for _, raw := range []string{"synthetic://app/writer-authority", "synthetic://app/writer-broker", "synthetic://ledger/writer"} {
		if strings.Contains(string(receipt.payload()), raw) || strings.Contains(fmt.Sprintf("%+v", receipt), raw) {
			t.Fatalf("raw identifier leaked: %s", raw)
		}
	}
}

func TestBeforeEffectMarkerStatusClaimsOneFencedSameOperationContinuation(t *testing.T) {
	f := newProtocolFixture(t)
	predecessor := bytes.Repeat([]byte{0x7c}, 32)
	f.body = testOperation("synthetic-existing-marker-operation")
	f.store.SeedSource(predecessor, true)
	writer := f.writer(t, markerEntropy())
	if _, err := writer.GenerateCommitAndReadback(context.Background(), f.body, markerAuth(f, writer, f.body, MutationCall), FaultHTTP408BeforeEffect); !errors.Is(err, ErrReconcileRequired) {
		t.Fatalf("before-effect marker ambiguity: %v", err)
	}
	if calls := f.store.sourceCASCount(); calls != 0 {
		t.Fatalf("before-effect ambiguity reached the CAS wire: %d", calls)
	}
	receipt, err := writer.ReconcileCommit(context.Background(), f.body, markerAuth(f, writer, f.body, StatusCall))
	if err != nil {
		t.Fatalf("single fenced continuation: %v", err)
	}
	if calls := f.store.sourceCASCount(); calls != 1 {
		t.Fatalf("continuation CAS calls=%d want=1", calls)
	}
	stored, exists, err := f.store.readSourceForTest()
	if err != nil || !exists || len(stored) != 32 || bytes.Equal(stored, predecessor) || HashBytes(stored) != receipt.MarkerSHA256 {
		t.Fatalf("continued marker mismatch: exists=%v err=%v", exists, err)
	}
	request := markerRequestForWriter(t, writer, f.body)
	f.effects.mu.Lock()
	record := f.effects.records[request.key()]
	f.effects.mu.Unlock()
	if !record.Prepared || !record.Issued || !record.ContinuationClaimed || record.State != MutationCompleted || record.IssueCount != 1 {
		t.Fatalf("durable continuation fence missing: %+v", record)
	}
	replayed, err := writer.ReconcileCommit(context.Background(), f.body, markerAuth(f, writer, f.body, StatusCall))
	if err != nil || writerReceiptDigest(replayed) != writerReceiptDigest(receipt) || f.store.sourceCASCount() != 1 {
		t.Fatalf("completed status was not idempotent: err=%v calls=%d", err, f.store.sourceCASCount())
	}
}

func TestPreparedOnlyCrashRecoversThroughIssuedContinuationFence(t *testing.T) {
	f := newProtocolFixture(t)
	writer := f.writer(t, markerEntropy())
	if _, err := writer.GenerateCommitAndReadback(context.Background(), f.body, markerAuth(f, writer, f.body, MutationCall), FaultCrashAfterPreparation); !errors.Is(err, ErrSimulatedCrash) {
		t.Fatalf("prepared-only crash: %v", err)
	}
	request := markerRequestForWriter(t, writer, f.body)
	f.effects.mu.Lock()
	record := f.effects.records[request.key()]
	f.effects.mu.Unlock()
	if !record.Prepared || record.Issued || record.ContinuationClaimed || f.store.sourceCASCount() != 0 {
		t.Fatalf("prepared-only fence mismatch: record=%+v calls=%d", record, f.store.sourceCASCount())
	}
	receipt, err := writer.ReconcileCommit(context.Background(), f.body, markerAuth(f, writer, f.body, StatusCall))
	if err != nil || receipt.MarkerSHA256.IsZero() {
		t.Fatalf("prepared-only recovery failed: receipt=%+v err=%v", receipt, err)
	}
	f.effects.mu.Lock()
	record = f.effects.records[request.key()]
	f.effects.mu.Unlock()
	if !record.Prepared || !record.Issued || !record.ContinuationClaimed || record.State != MutationCompleted || f.store.sourceCASCount() != 1 {
		t.Fatalf("prepared recovery did not claim exact issued continuation: record=%+v calls=%d", record, f.store.sourceCASCount())
	}
}

func TestFencedMarkerContinuationFailureCannotIssueTwice(t *testing.T) {
	f := newProtocolFixture(t)
	predecessor := bytes.Repeat([]byte{0x71}, 32)
	f.body = testOperation("synthetic-continuation-failure-operation")
	f.store.SeedSource(predecessor, true)
	writer := f.writer(t, markerEntropy())
	if _, err := writer.GenerateCommitAndReadback(context.Background(), f.body, markerAuth(f, writer, f.body, MutationCall), FaultHTTP408BeforeEffect); !errors.Is(err, ErrReconcileRequired) {
		t.Fatalf("before-effect marker ambiguity: %v", err)
	}
	// Simulate a crash-window drift after the one continuation CAS but before
	// its signed readback. The continuation fence must remain consumed.
	f.store.setSourceHooks(func() {
		f.store.SeedSource(bytes.Repeat([]byte{0x72}, 32), true)
	}, nil)
	if _, err := writer.ReconcileCommit(context.Background(), f.body, markerAuth(f, writer, f.body, StatusCall)); !errors.Is(err, ErrCleanupRequired) {
		t.Fatalf("failed continuation did not quarantine: %v", err)
	}
	f.store.setSourceHooks(nil, nil)
	request := markerRequestForWriter(t, writer, f.body)
	f.effects.mu.Lock()
	record := f.effects.records[request.key()]
	f.effects.mu.Unlock()
	if !record.Prepared || !record.Issued || !record.ContinuationClaimed || record.State != MutationQuarantined || f.store.sourceCASCount() != 1 {
		t.Fatalf("failed continuation fence/state mismatch: record=%+v calls=%d", record, f.store.sourceCASCount())
	}
	if _, err := writer.ReconcileCommit(context.Background(), f.body, markerAuth(f, writer, f.body, StatusCall)); err == nil {
		t.Fatalf("second continuation was not rejected: %v", err)
	}
	if f.store.sourceCASCount() != 1 {
		t.Fatalf("second reconciliation repeated CAS: %d", f.store.sourceCASCount())
	}
}

func TestMarkerDriftQuarantinesAndRequiresPortableSignedAbort(t *testing.T) {
	f := newProtocolFixture(t)
	predecessor := bytes.Repeat([]byte{0x7c}, 32)
	drifted := bytes.Repeat([]byte{0x6d}, 32)
	f.body = testOperation("synthetic-marker-drift-operation")
	f.store.SeedSource(predecessor, true)
	writer := f.writer(t, markerEntropy())
	if _, err := writer.GenerateCommitAndReadback(context.Background(), f.body, markerAuth(f, writer, f.body, MutationCall), FaultHTTP408BeforeEffect); !errors.Is(err, ErrReconcileRequired) {
		t.Fatalf("before-effect marker ambiguity: %v", err)
	}
	// Provider-side drift means the fixed key is no longer the protected
	// predecessor. The same-operation continuation must therefore remain fenced.
	f.store.SeedSource(drifted, true)
	if _, err := writer.ReconcileCommit(context.Background(), f.body, markerAuth(f, writer, f.body, StatusCall)); !errors.Is(err, ErrCleanupRequired) {
		t.Fatalf("drifted predecessor accepted for continuation: %v", err)
	}
	if f.store.sourceCASCount() != 0 {
		t.Fatalf("drifted status invoked CAS: %d", f.store.sourceCASCount())
	}
	stored, exists, err := f.store.readSourceForTest()
	if err != nil || !exists || !bytes.Equal(stored, drifted) {
		t.Fatalf("drifted source changed: exists=%v err=%v", exists, err)
	}
	if !writer.SealedMarkerPresent(f.body) {
		t.Fatal("quarantined ambiguity lost opaque reconciliation material")
	}
	if _, err := writer.AbortCommit(context.Background(), f.body, writerAbortAuth(f, writer, f.body, MutationCall), FaultHTTP408AfterEffect); !errors.Is(err, ErrReconcileRequired) {
		t.Fatalf("ambiguous separate marker abort: %v", err)
	}
	cleanup, err := NewMarkerAbortController(f.boundary, f.effects, f.sealer)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cleanup.AbortCommit(context.Background(), f.body, markerAbortAuth(f, cleanup, f.body, MutationCall), FaultNone); !errors.Is(err, ErrReconcileRequired) {
		t.Fatalf("cleanup controller repeated revocation: %v", err)
	}
	receipt, err := cleanup.ReconcileAbortCommit(context.Background(), f.body, markerAbortAuth(f, cleanup, f.body, StatusCall))
	if _, statusErr := markerSealerTerminalStatus(f.sealer); err != nil || statusErr != nil || receipt.Witness.MarkerSHA256.IsZero() || receipt.Witness.ReasonSHA256.IsZero() {
		t.Fatalf("separate marker abort reconciliation failed: receipt=%+v err=%v", receipt, err)
	}
	if err := VerifyMarkerAbortReceipt(f.boundary, f.body, receipt); err != nil {
		t.Fatalf("portable abort completion rejected: %v", err)
	}
	if !writer.SealedMarkerPresent(f.body) {
		t.Fatal("status reconciliation mutated local sealed-marker state")
	}
	if _, err := cleanup.AbortCommit(context.Background(), f.body, markerAbortAuth(f, cleanup, f.body, MutationCall), FaultNone); err != nil {
		t.Fatalf("authenticated terminal erasure: %v", err)
	}
	if writer.SealedMarkerPresent(f.body) {
		t.Fatal("authenticated terminal abort retained opaque reconciliation material")
	}
	for name, mutate := range map[string]func(*MarkerAbortReceipt){
		"source-target": func(candidate *MarkerAbortReceipt) {
			candidate.Witness.SourceTargetSHA256 = HashString("synthetic-wrong-source")
		},
		"capability": func(candidate *MarkerAbortReceipt) {
			candidate.Witness.CapabilitySHA256 = HashString("synthetic-wrong-capability")
		},
		"marker-request": func(candidate *MarkerAbortReceipt) {
			candidate.Witness.MarkerRequestSHA256 = HashString("synthetic-wrong-marker-request")
		},
		"revocation-binding": func(candidate *MarkerAbortReceipt) {
			candidate.RevocationStatus.BindingSHA256 = HashString("synthetic-wrong-revocation-binding")
		},
		"revocation-action": func(candidate *MarkerAbortReceipt) {
			candidate.RevocationStatus.ActionSHA256 = HashString("synthetic-wrong-revocation-action")
		},
		"revocation-result": func(candidate *MarkerAbortReceipt) {
			candidate.RevocationStatus.ResultSHA256 = HashString("synthetic-wrong-revocation-result")
		},
		"revocation-not-terminal": func(candidate *MarkerAbortReceipt) {
			candidate.RevocationStatus.Terminal = false
		},
		"revocation-chronology": func(candidate *MarkerAbortReceipt) {
			candidate.RevocationStatus.ObservedAt = candidate.Witness.AuthorizedAt.Add(-time.Nanosecond)
		},
		"completion-result": func(candidate *MarkerAbortReceipt) {
			candidate.CompletionWitness.ResultSHA256 = HashString("synthetic-wrong-terminal-result")
		},
		"completion-request": func(candidate *MarkerAbortReceipt) {
			candidate.CompletionWitness.RequestSHA256 = HashString("synthetic-wrong-abort-request")
		},
		"completion-binding": func(candidate *MarkerAbortReceipt) {
			candidate.CompletionWitness.TerminalSHA256 = HashString("synthetic-wrong-abort-binding")
		},
		"completion-chronology": func(candidate *MarkerAbortReceipt) {
			candidate.CompletionWitness.CompletedAt = candidate.RevocationStatus.ObservedAt.Add(-time.Nanosecond)
		},
		"completion-signature": func(candidate *MarkerAbortReceipt) {
			candidate.CompletionWitness.Signature[0] ^= 0x01
		},
	} {
		t.Run(name, func(t *testing.T) {
			candidate := receipt
			candidate.Witness.Signature = append([]byte(nil), receipt.Witness.Signature...)
			candidate.CompletionWitness.Signature = append([]byte(nil), receipt.CompletionWitness.Signature...)
			mutate(&candidate)
			if err := VerifyMarkerAbortReceipt(f.boundary, f.body, candidate); !errors.Is(err, ErrCleanupRequired) {
				t.Fatalf("tampered abort accepted: %v", err)
			}
		})
	}
}

func TestMarkerAbortBurnsAuthenticationBeforePendingMarkerLookup(t *testing.T) {
	f := newProtocolFixture(t)
	writer := f.writer(t, markerEntropy())
	if _, err := writer.GenerateCommitAndReadback(context.Background(), f.body, markerAuth(f, writer, f.body, MutationCall), FaultHTTP408BeforeEffect); !errors.Is(err, ErrReconcileRequired) {
		t.Fatal(err)
	}
	controller, err := NewMarkerAbortController(f.boundary, f.effects, f.sealer)
	if err != nil {
		t.Fatal(err)
	}
	drift := f.body
	drift.ConfigDigest = HashString("synthetic-marker-abort-drift")
	auth := markerAbortAuth(f, controller, drift, MutationCall)
	if _, err := controller.AbortCommit(context.Background(), drift, auth, FaultNone); !errors.Is(err, ErrCleanupRequired) {
		t.Fatalf("invalid pending marker accepted: %v", err)
	}
	if _, err := controller.AbortCommit(context.Background(), f.body, auth, FaultNone); !errors.Is(err, ErrAuthReplay) {
		t.Fatalf("semantic-error authentication was reusable: %v", err)
	}
	if _, err := markerSealerTerminalStatus(f.sealer); !errors.Is(err, ErrNotObserved) {
		t.Fatalf("semantic failure revoked the sealer: %v", err)
	}
}

func TestPreparedMarkerCannotEqualDigestPredecessor(t *testing.T) {
	f := newProtocolFixture(t)
	predecessor := markerEntropy()
	f.body = testOperation("synthetic-equal-marker-operation")
	f.store.SeedSource(predecessor, true)
	writer := f.writer(t, predecessor)
	if _, err := writer.GenerateCommitAndReadback(context.Background(), f.body, markerAuth(f, writer, f.body, MutationCall), FaultHTTP408BeforeEffect); !errors.Is(err, ErrPredecessorMismatch) {
		t.Fatalf("generated marker equal to predecessor was accepted: %v", err)
	}
	if writer.SealedMarkerPresent(f.body) {
		t.Fatal("rejected equal-predecessor marker left sealed material")
	}
	stored, exists, err := f.store.readSourceForTest()
	if err != nil || !exists || !bytes.Equal(stored, predecessor) {
		t.Fatalf("equal-predecessor rejection changed source: exists=%v err=%v", exists, err)
	}
}

func TestWriterAdmissionIsMandatoryIssueOnceAndReconstructable(t *testing.T) {
	f := newProtocolFixture(t)
	ctx := context.Background()
	writer := f.rawWriter(t, markerEntropy())
	if _, err := writer.GenerateCommitAndReadback(ctx, f.body, markerAuth(f, writer, f.body, MutationCall), FaultNone); !errors.Is(err, ErrForkNotAuthorized) {
		t.Fatalf("marker admitted before provider admission: %v", err)
	}
	admission, err := NewWriterAdmission(writer, f.body, originalProjection(), f.writerProvider)
	if err != nil {
		t.Fatal(err)
	}
	for index, effect := range writerAdmissionEffects {
		fault := FaultNone
		if index == 0 {
			fault = FaultHTTP408AfterEffect
		}
		err := admission.Apply(ctx, effect, f.auth.issued(f.effects, admission.request(effect), MutationCall), fault)
		if index == 0 {
			if !errors.Is(err, ErrReconcileRequired) {
				t.Fatalf("admission ambiguity: %v", err)
			}
			reconstructed, rebuildErr := NewWriterAdmission(writer, f.body, originalProjection(), f.writerProvider)
			if rebuildErr != nil {
				t.Fatal(rebuildErr)
			}
			if err := reconstructed.Apply(ctx, effect, f.auth.issued(f.effects, reconstructed.request(effect), MutationCall), FaultNone); !errors.Is(err, ErrReconcileRequired) {
				t.Fatalf("admission blindly reissued: %v", err)
			}
			if err := reconstructed.Reconcile(ctx, effect, f.auth.issued(f.effects, reconstructed.request(effect), StatusCall)); err != nil {
				t.Fatal(err)
			}
		} else if err != nil {
			t.Fatal(err)
		}
	}
	if !admission.Ready() {
		t.Fatal("completed admission not reconstructable")
	}
	if _, err := writer.GenerateCommitAndReadback(ctx, f.body, markerAuth(f, writer, f.body, MutationCall), FaultNone); err != nil {
		t.Fatal(err)
	}
}

func TestBrokerCapabilityBindsExactAdmissionChainFreshAuthAndMarkerReceipt(t *testing.T) {
	f := newProtocolFixture(t)
	writer := f.rawWriter(t, markerEntropy())
	projection := originalProjection()
	projectionDigest, err := projection.Digest()
	if err != nil {
		t.Fatal(err)
	}
	admission, err := NewWriterAdmission(writer, f.body, projection, f.writerProvider)
	if err != nil {
		t.Fatal(err)
	}
	var capabilityAuth AuthEnvelope
	for _, effect := range writerAdmissionEffects {
		auth := f.auth.issued(f.effects, admission.request(effect), MutationCall)
		if effect == EffectCapabilityIssue {
			capabilityAuth = auth
		}
		if err := admission.Apply(context.Background(), effect, auth, FaultNone); err != nil {
			t.Fatalf("admission %s: %v", effect, err)
		}
	}
	capability, err := writer.capability(f.body)
	if err != nil {
		t.Fatal(err)
	}
	chain, err := writerAdmissionChainDigest(writer, f.body, projectionDigest)
	if err != nil {
		t.Fatal(err)
	}
	authorization, err := capabilityAuth.Digest()
	if err != nil {
		t.Fatal(err)
	}
	if capability.AdmissionChainSHA256 != chain || capability.AuthorizationSHA256 != authorization ||
		!capability.AuthorizationIssuedAtUTC.Equal(protocolNow) || !capability.AuthorizationNotBeforeUTC.Equal(capabilityAuth.NotBefore) ||
		!capability.AuthorizationNotAfter.Equal(capabilityAuth.ExpiresAt) {
		t.Fatal("capability did not bind exact admission chain and fresh authorization")
	}
	receipt, err := writer.GenerateCommitAndReadback(context.Background(), f.body, markerAuth(f, writer, f.body, MutationCall), FaultNone)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.CapabilitySHA256 != capability.Digest() || receipt.AdmissionChainSHA256 != chain {
		t.Fatal("writer receipt omitted admission/capability authority")
	}
	for name, mutate := range map[string]func(*WriterReceipt){
		"capability": func(candidate *WriterReceipt) { candidate.CapabilitySHA256 = HashString("synthetic-capability-drift") },
		"chain":      func(candidate *WriterReceipt) { candidate.AdmissionChainSHA256 = HashString("synthetic-chain-drift") },
		"cas-completed": func(candidate *WriterReceipt) {
			candidate.MarkerCASCompletedAtUTC = candidate.MarkerCASCompletedAtUTC.Add(time.Nanosecond)
		},
		"read-completed": func(candidate *WriterReceipt) {
			candidate.SourceReadCompletedAtUTC = candidate.SourceReadCompletedAtUTC.Add(time.Nanosecond)
		},
	} {
		t.Run(name, func(t *testing.T) {
			candidate := receipt
			candidate.Signature = append([]byte(nil), receipt.Signature...)
			mutate(&candidate)
			if err := VerifyWriterReceipt(f.boundary, candidate); err == nil {
				t.Fatal("tampered writer authority accepted")
			}
		})
	}

	first := writerAdmissionRequest(writer, f.body, projectionDigest, writerAdmissionEffects[0])
	f.effects.mu.Lock()
	record := f.effects.records[first.key()]
	record.Result = HashString("synthetic-tampered-admission-result")
	f.effects.records[first.key()] = record
	f.effects.mu.Unlock()
	if _, err := writer.capability(f.body); !errors.Is(err, ErrForkNotAuthorized) {
		t.Fatalf("tampered admission chain accepted: %v", err)
	}
}

func TestExpiredBrokerCapabilitiesFailAtExclusiveFractionalBoundary(t *testing.T) {
	f := newProtocolFixture(t)
	writer := f.writer(t, markerEntropy())
	capability, err := writer.capability(f.body)
	if err != nil {
		t.Fatal(err)
	}
	f.effects.now = func() time.Time { return capability.AuthorizationNotAfter }
	auth := markerAuth(f, writer, f.body, MutationCall)
	auth.IssuedAt = capability.AuthorizationNotAfter.Add(-time.Second)
	auth.NotBefore = auth.IssuedAt
	auth.ExpiresAt = capability.AuthorizationNotAfter.Add(time.Minute)
	if _, err := writer.GenerateCommitAndReadback(context.Background(), f.body, auth, FaultNone); !errors.Is(err, ErrRoleIsolation) {
		t.Fatalf("capability accepted at exclusive expiry: %v", err)
	}
}

func TestBrokerCapabilityActionChronologyUsesInclusiveIssueAndExclusiveFractionalExpiry(t *testing.T) {
	for _, testCase := range []struct {
		name    string
		action  func(BrokerCapability) time.Time
		wantErr error
	}{
		{name: "before-issued", action: func(c BrokerCapability) time.Time { return c.AuthorizationIssuedAtUTC.Add(-time.Nanosecond) }, wantErr: ErrRoleIsolation},
		{name: "exact-issued", action: func(c BrokerCapability) time.Time { return c.AuthorizationIssuedAtUTC }},
		{name: "fraction-before-expiry", action: func(c BrokerCapability) time.Time { return c.AuthorizationNotAfter.Add(-time.Nanosecond) }},
		{name: "exact-expiry", action: func(c BrokerCapability) time.Time { return c.AuthorizationNotAfter }, wantErr: ErrRoleIsolation},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			f := newProtocolFixture(t)
			writer := f.writer(t, markerEntropy())
			capability, err := writer.capability(f.body)
			if err != nil {
				t.Fatal(err)
			}
			actionAt := testCase.action(capability)
			clock := func() time.Time { return actionAt }
			f.effects.now = clock
			writer.broker.now = clock
			auth := markerAuth(f, writer, f.body, MutationCall)
			auth.IssuedAt = actionAt.Add(-time.Minute)
			auth.NotBefore = auth.IssuedAt
			auth.ExpiresAt = actionAt.Add(time.Minute)
			receipt, err := writer.GenerateCommitAndReadback(context.Background(), f.body, auth, FaultNone)
			if testCase.wantErr != nil {
				if !errors.Is(err, testCase.wantErr) {
					t.Fatalf("wanted %v, got %v", testCase.wantErr, err)
				}
				if _, replayErr := writer.GenerateCommitAndReadback(context.Background(), f.body, auth, FaultNone); !errors.Is(replayErr, ErrAuthReplay) {
					t.Fatalf("rejected chronology did not burn authentication: %v", replayErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !receipt.BrokerActionAtUTC.Equal(actionAt) || !receipt.AuthorizationIssuedAtUTC.Equal(capability.AuthorizationIssuedAtUTC) ||
				!receipt.AuthorizationNotBeforeUTC.Equal(capability.AuthorizationNotBeforeUTC) ||
				!receipt.AuthorizationNotAfterUTC.Equal(capability.AuthorizationNotAfter) ||
				!receipt.MarkerCASCompletedAtUTC.Equal(actionAt) || !receipt.SourceReadCompletedAtUTC.Equal(actionAt) {
				t.Fatal("writer receipt omitted the signed capability/action chronology")
			}
		})
	}
}

func TestWriterReceiptUsesPostReadTrustedTimeAndRejectsInCallClockDrift(t *testing.T) {
	t.Run("post-read-completion-is-signed", func(t *testing.T) {
		f := newProtocolFixture(t)
		writer := f.writer(t, markerEntropy())
		start := f.clock.now
		f.store.setSourceHooks(func() { f.clock.now = f.clock.now.Add(250 * time.Nanosecond) }, func() { f.clock.now = f.clock.now.Add(500 * time.Nanosecond) })
		receipt, err := writer.GenerateCommitAndReadback(context.Background(), f.body, markerAuth(f, writer, f.body, MutationCall), FaultNone)
		if err != nil {
			t.Fatal(err)
		}
		if !receipt.MarkerCASCompletedAtUTC.Equal(start.Add(250*time.Nanosecond)) ||
			!receipt.SourceReadCompletedAtUTC.Equal(start.Add(750*time.Nanosecond)) ||
			!receipt.BrokerActionAtUTC.Equal(receipt.SourceReadCompletedAtUTC) {
			t.Fatalf("receipt did not bind post-effect completions: cas=%s read=%s action=%s", receipt.MarkerCASCompletedAtUTC, receipt.SourceReadCompletedAtUTC, receipt.BrokerActionAtUTC)
		}
	})

	for _, testCase := range []struct {
		name string
		hook func(*protocolFixture, BrokerCapability)
	}{
		{name: "expiry-after-cas", hook: func(f *protocolFixture, capability BrokerCapability) {
			f.store.setSourceHooks(func() { f.clock.now = capability.AuthorizationNotAfter }, nil)
		}},
		{name: "rollback-after-cas", hook: func(f *protocolFixture, _ BrokerCapability) {
			f.store.setSourceHooks(func() { f.clock.now = f.clock.now.Add(-time.Nanosecond) }, nil)
		}},
		{name: "expiry-after-read", hook: func(f *protocolFixture, capability BrokerCapability) {
			f.store.setSourceHooks(nil, func() { f.clock.now = capability.AuthorizationNotAfter })
		}},
		{name: "rollback-after-read", hook: func(f *protocolFixture, _ BrokerCapability) {
			f.store.setSourceHooks(nil, func() { f.clock.now = f.clock.now.Add(-time.Nanosecond) })
		}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			f := newProtocolFixture(t)
			writer := f.writer(t, markerEntropy())
			capability, err := writer.capability(f.body)
			if err != nil {
				t.Fatal(err)
			}
			testCase.hook(f, capability)
			if _, err := writer.GenerateCommitAndReadback(context.Background(), f.body, markerAuth(f, writer, f.body, MutationCall), FaultNone); !errors.Is(err, ErrRoleIsolation) {
				t.Fatalf("in-call clock drift accepted: %v", err)
			}
			request := markerRequestForWriter(t, writer, f.body)
			if _, ok := f.effects.writerReceiptForRequest(request); ok {
				t.Fatal("failed post-effect chronology published writer authority")
			}
			bodyDigest, _ := f.body.Digest()
			if _, pending := f.effects.pendingMarkerAbort(bodyDigest); !pending {
				t.Fatal("post-effect chronology failure did not preserve cleanup authority")
			}
			f.store.setSourceHooks(nil, nil)
			cleanupNow := f.clock.now
			cleanupAuth := writerAbortAuth(f, writer, f.body, MutationCall)
			cleanupAuth = cleanupAuth.withJTI("synthetic-cleanup-jti-" + testCase.name)
			cleanupAuth.IssuedAt = cleanupNow.Add(-time.Minute)
			cleanupAuth.NotBefore = cleanupNow.Add(-time.Minute)
			cleanupAuth.ExpiresAt = cleanupNow.Add(time.Minute)
			if _, err := writer.AbortCommit(context.Background(), f.body, cleanupAuth, FaultNone); err != nil {
				t.Fatalf("cleanup failed after chronology rejection: %v", err)
			}
		})
	}
}

func TestWriterReconcileRevalidatesTrustedClockAfterActualSourceRead(t *testing.T) {
	f := newProtocolFixture(t)
	writer := f.writer(t, markerEntropy())
	capability, err := writer.capability(f.body)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.GenerateCommitAndReadback(context.Background(), f.body, markerAuth(f, writer, f.body, MutationCall), FaultHTTP408AfterEffect); !errors.Is(err, ErrReconcileRequired) {
		t.Fatalf("initial ambiguity: %v", err)
	}
	request := markerRequestForWriter(t, writer, f.body)
	f.store.setSourceHooks(nil, func() { f.clock.now = capability.AuthorizationNotAfter })
	if _, err := writer.ReconcileCommit(context.Background(), f.body, markerAuth(f, writer, f.body, StatusCall)); !errors.Is(err, ErrRoleIsolation) {
		t.Fatalf("expired post-GET reconcile accepted: %v", err)
	}
	if f.effects.Completed(request) {
		t.Fatal("expired status read published ambiguous writer authority")
	}
	bodyDigest, _ := f.body.Digest()
	if _, pending := f.effects.pendingMarkerAbort(bodyDigest); !pending {
		t.Fatal("expired status read did not preserve cleanup-only authority")
	}
	f.store.setSourceHooks(nil, nil)
	cleanupNow := f.clock.now
	cleanupAuth := writerAbortAuth(f, writer, f.body, MutationCall)
	cleanupAuth = cleanupAuth.withJTI("synthetic-expired-reconcile-cleanup-jti")
	cleanupAuth.IssuedAt = cleanupNow.Add(-time.Minute)
	cleanupAuth.NotBefore = cleanupNow.Add(-time.Minute)
	cleanupAuth.ExpiresAt = cleanupNow.Add(time.Minute)
	if _, err := writer.AbortCommit(context.Background(), f.body, cleanupAuth, FaultNone); err != nil {
		t.Fatalf("cleanup after expired reconciliation: %v", err)
	}
}

func TestMarkerAbortRevocationIsIssueOnceAndStatusCompletesExactAmbiguity(t *testing.T) {
	for _, kind := range []ProviderAmbiguityKind{ProviderAmbiguousHTTP408, ProviderAmbiguousEOF, ProviderAmbiguous5xx} {
		t.Run(string(kind), func(t *testing.T) {
			f := newProtocolFixture(t)
			writer := f.writer(t, markerEntropy())
			if _, err := writer.GenerateCommitAndReadback(context.Background(), f.body, markerAuth(f, writer, f.body, MutationCall), FaultHTTP408BeforeEffect); !errors.Is(err, ErrReconcileRequired) {
				t.Fatal(err)
			}
			f.store.SeedSource(bytes.Repeat([]byte{0xcc}, 32), true)
			if _, err := writer.ReconcileCommit(context.Background(), f.body, markerAuth(f, writer, f.body, StatusCall)); !errors.Is(err, ErrCleanupRequired) {
				t.Fatalf("before-effect status did not prepare cleanup: %v", err)
			}
			f.sealer.setRevokeAmbiguity(kind)
			if _, err := writer.AbortCommit(context.Background(), f.body, writerAbortAuth(f, writer, f.body, MutationCall), FaultNone); !errors.Is(err, ErrReconcileRequired) {
				t.Fatalf("ambiguous sealer revoke: %v", err)
			}
			if revoke, _ := f.sealer.callCounts(); revoke != 1 {
				t.Fatalf("revoke calls=%d", revoke)
			}
			if !writer.SealedMarkerPresent(f.body) {
				t.Fatal("ambiguous revocation erased material before terminal completion")
			}
			if _, err := writer.AbortCommit(context.Background(), f.body, writerAbortAuth(f, writer, f.body, MutationCall), FaultNone); !errors.Is(err, ErrReconcileRequired) {
				t.Fatalf("ambiguous revocation was reissued: %v", err)
			}
			transient := errors.New("synthetic sealer status failure")
			f.sealer.setStatusFailure(transient)
			if _, err := writer.ReconcileAbortCommit(context.Background(), f.body, writerAbortAuth(f, writer, f.body, StatusCall)); !errors.Is(err, transient) {
				t.Fatalf("transient status failure: %v", err)
			}
			f.sealer.setStatusFailure(nil)
			receipt, err := writer.ReconcileAbortCommit(context.Background(), f.body, writerAbortAuth(f, writer, f.body, StatusCall))
			if err != nil {
				t.Fatal(err)
			}
			if revoke, _ := f.sealer.callCounts(); revoke != 1 {
				t.Fatalf("status repeated revoke: %d", revoke)
			}
			if !writer.SealedMarkerPresent(f.body) || VerifyMarkerAbortReceipt(f.boundary, f.body, receipt) != nil {
				t.Fatal("terminal status did not complete exact abort without local mutation")
			}
			erasedReceipt, err := writer.AbortCommit(context.Background(), f.body, writerAbortAuth(f, writer, f.body, MutationCall), FaultNone)
			if err != nil {
				t.Fatalf("authenticated terminal erasure: %v", err)
			}
			if writer.SealedMarkerPresent(f.body) {
				t.Fatal("authenticated terminal abort retained opaque material")
			}
			secondReceipt, err := writer.AbortCommit(context.Background(), f.body, writerAbortAuth(f, writer, f.body, MutationCall), FaultNone)
			if err != nil || !reflect.DeepEqual(erasedReceipt, secondReceipt) {
				t.Fatalf("post-erasure abort was not exactly idempotent: err=%v", err)
			}
			if revoke, _ := f.sealer.callCounts(); revoke != 1 || writer.SealedMarkerPresent(f.body) {
				t.Fatalf("post-erasure abort repeated revoke or restored material: revoke=%d", revoke)
			}
			if len(f.effects.publishedTerminals) != 0 || len(f.effects.publishedAdmissions) != 0 {
				t.Fatal("abort receipt created terminal or fork publication authority")
			}
		})
	}
}

func TestMarkerAbortFaultBoundaryNeverRepeatsRevocation(t *testing.T) {
	for _, fault := range []FaultMode{FaultCrashAfterEffect, FaultHTTP408AfterEffect, FaultEOFAfterEffect, Fault5xxAfterEffect} {
		t.Run(string(fault), func(t *testing.T) {
			f := newProtocolFixture(t)
			writer := f.writer(t, markerEntropy())
			if _, err := writer.GenerateCommitAndReadback(context.Background(), f.body, markerAuth(f, writer, f.body, MutationCall), FaultHTTP408BeforeEffect); !errors.Is(err, ErrReconcileRequired) {
				t.Fatal(err)
			}
			f.store.SeedSource(bytes.Repeat([]byte{0xcc}, 32), true)
			if _, err := writer.ReconcileCommit(context.Background(), f.body, markerAuth(f, writer, f.body, StatusCall)); !errors.Is(err, ErrCleanupRequired) {
				t.Fatalf("before-effect status did not prepare cleanup: %v", err)
			}
			if _, err := writer.AbortCommit(context.Background(), f.body, writerAbortAuth(f, writer, f.body, MutationCall), fault); !errors.Is(err, ErrReconcileRequired) && !errors.Is(err, ErrSimulatedCrash) {
				t.Fatalf("after-effect fault was not ambiguous: %v", err)
			}
			if revoke, _ := f.sealer.callCounts(); revoke != 1 {
				t.Fatalf("revoke calls=%d", revoke)
			}
			if !writer.SealedMarkerPresent(f.body) {
				t.Fatal("ambiguous response erased material before completion witness")
			}
			if _, err := writer.ReconcileAbortCommit(context.Background(), f.body, writerAbortAuth(f, writer, f.body, StatusCall)); err != nil {
				t.Fatal(err)
			}
			if !writer.SealedMarkerPresent(f.body) {
				t.Fatal("status-only completion erased local material")
			}
			if _, err := writer.AbortCommit(context.Background(), f.body, writerAbortAuth(f, writer, f.body, MutationCall), FaultNone); err != nil {
				t.Fatalf("authenticated terminal erasure: %v", err)
			}
			if revoke, _ := f.sealer.callCounts(); revoke != 1 {
				t.Fatalf("status repeated revoke: %d", revoke)
			}
		})
	}
}

func TestMarkerAbortBeforeEffectFaultsNeverInvokeRevocation(t *testing.T) {
	for _, fault := range []FaultMode{FaultCrashAfterAuthorization, FaultHTTP408BeforeEffect, FaultEOFBeforeEffect, Fault5xxBeforeEffect} {
		t.Run(string(fault), func(t *testing.T) {
			f := newProtocolFixture(t)
			writer := f.writer(t, markerEntropy())
			if _, err := writer.GenerateCommitAndReadback(context.Background(), f.body, markerAuth(f, writer, f.body, MutationCall), FaultHTTP408BeforeEffect); !errors.Is(err, ErrReconcileRequired) {
				t.Fatal(err)
			}
			f.store.SeedSource(bytes.Repeat([]byte{0xcc}, 32), true)
			if _, err := writer.ReconcileCommit(context.Background(), f.body, markerAuth(f, writer, f.body, StatusCall)); !errors.Is(err, ErrCleanupRequired) {
				t.Fatalf("before-effect status did not prepare cleanup: %v", err)
			}
			_, err := writer.AbortCommit(context.Background(), f.body, writerAbortAuth(f, writer, f.body, MutationCall), fault)
			if fault == FaultCrashAfterAuthorization {
				if !errors.Is(err, ErrSimulatedCrash) {
					t.Fatalf("wanted crash, got %v", err)
				}
			} else if !errors.Is(err, ErrReconcileRequired) {
				t.Fatalf("wanted reconciliation, got %v", err)
			}
			if revoke, _ := f.sealer.callCounts(); revoke != 0 {
				t.Fatalf("before-effect fault invoked revoke %d times", revoke)
			}
			if _, err := writer.ReconcileAbortCommit(context.Background(), f.body, writerAbortAuth(f, writer, f.body, StatusCall)); !errors.Is(err, ErrCleanupRequired) {
				t.Fatalf("status fabricated absent revocation: %v", err)
			}
			if revoke, _ := f.sealer.callCounts(); revoke != 0 {
				t.Fatalf("status path invoked revoke %d times", revoke)
			}
			if !writer.SealedMarkerPresent(f.body) {
				t.Fatal("absent revocation erased sealed reconciliation material")
			}
		})
	}
}

func TestMarkerGenerationIsExactly32BytesAndShortEntropyBurnsAttempt(t *testing.T) {
	f := newProtocolFixture(t)
	writer := f.writer(t, make([]byte, 31))
	request := markerRequestForWriter(t, writer, f.body)
	ctx := context.Background()
	auth := markerAuth(f, writer, f.body, MutationCall)
	if _, err := writer.GenerateCommitAndReadback(ctx, f.body, auth, FaultNone); err == nil {
		t.Fatal("short entropy accepted")
	}
	if !f.effects.usedJTIs[auth.JTIHash()] || !f.effects.usedChallenges[auth.ChallengeHash()] {
		t.Fatal("failed call did not durably burn authentication")
	}
	if _, err := writer.GenerateCommitAndReadback(ctx, f.body, markerAuth(f, writer, f.body, MutationCall), FaultNone); !errors.Is(err, ErrForkNotAuthorized) {
		t.Fatalf("failed effect reissued: %v", err)
	}
	if f.effects.IssueCount(request) != 0 {
		t.Fatal("quarantined attempt remained query-authoritative")
	}
}

func TestProtocolOracleBurnsAuthAndQuarantinesSameOperationDifferentEffect(t *testing.T) {
	f := newProtocolFixture(t)
	_ = f.rawWriter(t, markerEntropy()) // bind the writer ledger signer
	ctx := context.Background()
	missing := testRequest(EffectBrokerCreate, 91)
	auth := f.auth.issued(f.effects, missing, StatusCall)
	if _, err := f.effects.ReconcileStatus(ctx, missing, auth, func() ([]Digest, error) { return nil, nil }); !errors.Is(err, ErrInvalid) {
		t.Fatalf("missing status: %v", err)
	}
	if _, err := f.effects.ReconcileStatus(ctx, missing, auth, func() ([]Digest, error) { return nil, nil }); !errors.Is(err, ErrAuthReplay) {
		t.Fatalf("missing status auth rollback: %v", err)
	}
	request := testRequest(EffectBrokerCreate, 92)
	if _, err := f.effects.AuthorizeAndConsume(ctx, request, f.auth.issued(f.effects, request, MutationCall), FaultNone, Digest{}, func() (Digest, error) { return HashString("synthetic-created"), nil }); err != nil {
		t.Fatal(err)
	}
	drift := request
	drift.Effect = EffectBrokerDelete
	drift.ParametersDigest = HashString("synthetic-delete-parameters")
	driftAuth := f.auth.issued(f.effects, drift, MutationCall)
	if _, err := f.effects.AuthorizeAndConsume(ctx, drift, driftAuth, FaultNone, Digest{}, func() (Digest, error) { return HashString("synthetic-deleted"), nil }); !errors.Is(err, ErrQuarantined) {
		t.Fatalf("different effect reuse: %v", err)
	}
	if !f.effects.Completed(request) || f.effects.IssueCount(request) != 1 {
		t.Fatal("rejected conflict retroactively revoked completed evidence")
	}
	if got, ok := f.effects.result(request); !ok || got != HashString("synthetic-created") {
		t.Fatal("completed result changed after rejected conflict")
	}
	if _, ok := f.effects.CompletionTime(request); !ok {
		t.Fatal("completed time disappeared after rejected conflict")
	}
	if _, ok := f.effects.completionWitness(request); !ok {
		t.Fatal("portable completion witness disappeared after rejected conflict")
	}
	if _, err := f.effects.AuthorizeAndConsume(ctx, request, driftAuth, FaultNone, Digest{}, func() (Digest, error) { return HashString("synthetic-created"), nil }); !errors.Is(err, ErrAuthReplay) {
		t.Fatalf("quarantine did not burn auth: %v", err)
	}
}

func TestDurableQueryHelpersRequireCompleteRequestDigest(t *testing.T) {
	f := newProtocolFixture(t)
	request := testRequest(EffectBrokerCreate, 93)
	result := HashString("synthetic-exact-query-result")
	if _, err := f.effects.AuthorizeAndConsume(context.Background(), request, f.auth.issued(f.effects, request, MutationCall), FaultNone, Digest{}, func() (Digest, error) {
		return result, nil
	}); err != nil {
		t.Fatal(err)
	}
	drift := request
	drift.ParametersDigest = HashString("synthetic-different-query-parameters")
	if !f.effects.Completed(request) || f.effects.IssueCount(request) != 1 {
		t.Fatal("exact request state unavailable")
	}
	if got, ok := f.effects.result(request); !ok || got != result {
		t.Fatalf("exact result got=%v ok=%v", got, ok)
	}
	if f.effects.Completed(drift) || f.effects.IssueCount(drift) != 0 {
		t.Fatal("query helper accepted key-only request drift")
	}
	if got, ok := f.effects.result(drift); ok || !got.IsZero() {
		t.Fatalf("result helper accepted key-only request drift: got=%v ok=%v", got, ok)
	}

	terminalRequest := testRequest(EffectEvidencePublish, 94)
	terminalReceipt := HashString("synthetic-terminal-query-receipt")
	operationBody := HashString("synthetic-terminal-query-operation")
	evidence := HashString("synthetic-terminal-query-evidence")
	if _, err := f.effects.AuthorizeAndConsume(context.Background(), terminalRequest, f.auth.issued(f.effects, terminalRequest, MutationCall), FaultNone, Digest{}, func() (Digest, error) {
		token := sealedMarkerToken{version: ProtocolVersion, operationSHA256: operationBody,
			capabilitySHA256: HashString("synthetic-terminal-query-capability"), targetSHA256: HashString("synthetic-terminal-query-target"),
			markerSHA256: HashString("synthetic-terminal-query-marker"), sealerSHA256: HashString("synthetic-terminal-query-sealer"),
			predecessor:       NilPredecessor(),
			brokerActionAtUTC: protocolNow,
			nonce:             []byte{1}, ciphertext: []byte{2}}
		if err := f.effects.storeSealedMarkerTokenLocked(operationBody, token); err != nil {
			return Digest{}, err
		}
		if err := f.effects.registerTerminalAndEraseLocked(terminalReceipt, operationBody, evidence); err != nil {
			return Digest{}, err
		}
		return terminalReceipt, nil
	}); err != nil {
		t.Fatal(err)
	}
	terminalDrift := terminalRequest
	terminalDrift.ParametersDigest = HashString("synthetic-terminal-query-drift")
	if !f.effects.terminalPublished(terminalReceipt, operationBody, evidence, terminalRequest) ||
		f.effects.terminalPublished(terminalReceipt, operationBody, evidence, terminalDrift) {
		t.Fatal("terminal publication query did not bind the complete request")
	}

	admissionRequest := testRequest(EffectEvidencePublish, 95)
	admissionDigest := HashString("synthetic-admission-query-digest")
	binding := recoveryAdmissionBinding{
		OperationBody: HashString("synthetic-admission-query-operation"), TerminalBinding: HashString("synthetic-admission-query-terminal"),
		ForkRequest: HashString("synthetic-admission-query-fork-request"), ForkResult: HashString("synthetic-admission-query-fork-result"),
		ForkProof: HashString("synthetic-admission-query-fork-proof"), RecoveryTarget: HashString("synthetic-admission-query-target"),
		ForkProvider: HashString("synthetic-admission-query-provider"),
		Payload:      HashString("synthetic-admission-query-payload"),
	}
	if _, err := f.effects.AuthorizeAndConsume(context.Background(), admissionRequest, f.auth.issued(f.effects, admissionRequest, MutationCall), FaultNone, Digest{}, func() (Digest, error) {
		if err := f.effects.registerAdmissionLocked(admissionDigest, binding); err != nil {
			return Digest{}, err
		}
		return admissionDigest, nil
	}); err != nil {
		t.Fatal(err)
	}
	admissionDrift := admissionRequest
	admissionDrift.ParametersDigest = HashString("synthetic-admission-query-drift")
	if !f.effects.admissionPublished(admissionDigest, binding, admissionRequest) ||
		f.effects.admissionPublished(admissionDigest, binding, admissionDrift) {
		t.Fatal("admission publication query did not bind the complete request")
	}
}

func TestMutationReplayCannotChangeTerminalAuthority(t *testing.T) {
	cases := []struct {
		name          string
		first, second Digest
	}{
		{"different-nonzero", HashString("synthetic-terminal-a"), HashString("synthetic-terminal-b")},
		{"zero-to-nonzero", Digest{}, HashString("synthetic-terminal-a")},
		{"nonzero-to-zero", HashString("synthetic-terminal-a"), Digest{}},
	}
	for index, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newProtocolFixture(t)
			request := testRequest(EffectForkPOST, 3000+index)
			calls := 0
			apply := func() (Digest, error) {
				calls++
				return HashString("synthetic-terminal-bound-result"), nil
			}
			if _, err := f.effects.AuthorizeAndConsume(context.Background(), request, f.auth.issued(f.effects, request, MutationCall), FaultNone, tc.first, apply); err != nil {
				t.Fatal(err)
			}
			if _, err := f.effects.AuthorizeAndConsume(context.Background(), request, f.auth.issued(f.effects, request, MutationCall), FaultNone, tc.second, apply); !errors.Is(err, ErrQuarantined) {
				t.Fatalf("terminal drift accepted: %v", err)
			}
			if calls != 1 || f.effects.IssueCount(request) != 1 {
				t.Fatalf("mutation repeated: calls/issues=%d/%d", calls, f.effects.IssueCount(request))
			}
		})
	}
}

func TestProjectionBindingClaimsExactBaseOperationBeforeDerivedEffects(t *testing.T) {
	f := newProtocolFixture(t)
	writer := f.rawWriter(t, markerEntropy())
	if _, err := NewWriterAdmission(writer, f.body, originalProjection(), f.writerProvider); err != nil {
		t.Fatal(err)
	}
	drift := f.body
	drift.ConfigDigest = HashString("synthetic-drifted-base-config")
	if _, err := NewWriterAdmission(writer, drift, originalProjection(), f.writerProvider); !errors.Is(err, ErrQuarantined) {
		t.Fatalf("same operation/generation base drift accepted: %v", err)
	}
	projectionDigest, _ := originalProjection().Digest()
	for _, effect := range writerAdmissionEffects {
		if count := f.effects.IssueCount(writerAdmissionRequest(writer, drift, projectionDigest, effect)); count != 0 {
			t.Fatalf("drifted base reached %s: issues=%d", effect, count)
		}
	}
	originalAdmission, err := NewWriterAdmission(writer, f.body, originalProjection(), f.writerProvider)
	if err == nil {
		err = originalAdmission.Apply(context.Background(), EffectBrokerCreate, f.auth.issued(f.effects, originalAdmission.request(EffectBrokerCreate), MutationCall), FaultNone)
	}
	if !errors.Is(err, ErrQuarantined) {
		t.Fatalf("base-identity quarantine was not sticky: %v", err)
	}
}

func TestEveryMutationClassUsesIssueOnceAndStatusOnlyAfterAmbiguity(t *testing.T) {
	for index, effect := range allEffects {
		t.Run(string(effect), func(t *testing.T) {
			f := newProtocolFixture(t)
			ctx := context.Background()
			request := testRequest(effect, index+200)
			want := HashString("synthetic-outcome-" + string(effect))
			applyCount := 0
			_, err := f.effects.AuthorizeAndConsume(ctx, request, f.auth.issued(f.effects, request, MutationCall), FaultEOFAfterEffect, Digest{}, func() (Digest, error) { applyCount++; return want, nil })
			if !errors.Is(err, ErrReconcileRequired) {
				t.Fatalf("ambiguous: %v", err)
			}
			_, err = f.effects.AuthorizeAndConsume(ctx, request, f.auth.issued(f.effects, request, MutationCall), FaultNone, Digest{}, func() (Digest, error) { applyCount++; return want, nil })
			if !errors.Is(err, ErrReconcileRequired) {
				t.Fatalf("reissue admitted: %v", err)
			}
			got, err := f.effects.ReconcileStatus(ctx, request, f.auth.issued(f.effects, request, StatusCall), func() ([]Digest, error) { return []Digest{want}, nil })
			if err != nil || got != want {
				t.Fatalf("status got=%v err=%v", got, err)
			}
			if applyCount != 1 || f.effects.IssueCount(request) != 1 {
				t.Fatalf("apply/issue=%d/%d", applyCount, f.effects.IssueCount(request))
			}
		})
	}
}

func TestWriterLifecycleRequiresCapabilityAndAllRevocationsBeforeDelete(t *testing.T) {
	f := newProtocolFixture(t)
	ctx := context.Background()
	writer := f.writer(t, markerEntropy())
	if _, err := writer.GenerateCommitAndReadback(ctx, f.body, markerAuth(f, writer, f.body, MutationCall), FaultNone); err != nil {
		t.Fatal(err)
	}
	projection := originalProjection()
	lifecycle, provider, observer := newWriterLifecycleForTest(t, f, writer, f.body, projection)
	if err := lifecycle.RecordRevocation(ctx, RevokeLeaf, writerLifecycleAuth(f, lifecycle, EffectLeafRevoke, MutationCall), FaultNone); !errors.Is(err, ErrForkNotAuthorized) {
		t.Fatalf("leaf revoked before capability: %v", err)
	}
	if err := lifecycle.ReconcileRevocation(ctx, RevokeLeaf, writerLifecycleAuth(f, lifecycle, EffectLeafRevoke, StatusCall)); !errors.Is(err, ErrForkNotAuthorized) {
		t.Fatalf("leaf status completed before capability: %v", err)
	}
	if err := lifecycle.RecordFullRedeploy(ctx, writerLifecycleAuth(f, lifecycle, EffectFullRedeploy, MutationCall), FaultNone); !errors.Is(err, ErrForkNotAuthorized) {
		t.Fatalf("redeploy completed before teardown: %v", err)
	}
	if err := lifecycle.RecordFirewallRestored(ctx, writerLifecycleAuth(f, lifecycle, EffectTrustedSourceDel, MutationCall), FaultNone); !errors.Is(err, ErrForkNotAuthorized) {
		t.Fatalf("firewall restored before redeploy: %v", err)
	}
	for _, effect := range []Effect{EffectLeafRevoke, EffectFullRedeploy, EffectTrustedSourceDel} {
		if count := f.effects.IssueCount(lifecycle.request(effect)); count != 0 {
			t.Fatalf("out-of-order %s reached mutation authority: %d", effect, count)
		}
	}
	for _, kind := range []RevocationKind{RevokeCapability, RevokeLeaf, RevokeMTLS, RevokeWrappingKey, RemoveBinding, RemoveCredential} {
		if err := lifecycle.RequestDelete(ctx, DeleteDefinitiveSuccess, writerLifecycleAuth(f, lifecycle, EffectBrokerDelete, MutationCall)); !errors.Is(err, ErrForkNotAuthorized) {
			t.Fatalf("delete before %s: %v", kind, err)
		}
		if err := recordLifecycleRevocationForTest(t, lifecycle, f, kind, FaultNone); err != nil {
			t.Fatal(err)
		}
	}
	if !f.effects.CapabilityRevoked(lifecycle.capability) {
		t.Fatal("capability not durably revoked")
	}
	if err := lifecycle.RecordFullRedeploy(ctx, writerLifecycleAuth(f, lifecycle, EffectFullRedeploy, MutationCall), FaultNone); err != nil {
		t.Fatal(err)
	}
	if err := lifecycle.RecordFirewallRestored(ctx, writerLifecycleAuth(f, lifecycle, EffectTrustedSourceDel, MutationCall), FaultNone); err != nil {
		t.Fatal(err)
	}
	if err := lifecycle.RequestDelete(ctx, DeleteDefinitiveSuccess, writerLifecycleAuth(f, lifecycle, EffectBrokerDelete, MutationCall)); err != nil {
		t.Fatal(err)
	}
	drift := projection
	drift.ActionLedgerSHA256 = HashString("synthetic-drift")
	if _, err := NewWriterLifecycle(writer, f.body, drift, provider, observer); !errors.Is(err, ErrRoleIsolation) {
		t.Fatalf("projection drift accepted: %v", err)
	}
}

func TestTerminalEvidenceIsCompleteCanonicalSignedAndReconstructable(t *testing.T) {
	f := newProtocolFixture(t)
	writer, evidence, terminal := prepareTerminal(t, f, DeleteAmbiguous408, FaultHTTP408AfterEffect)
	if writer.SealedMarkerPresent(f.body) {
		t.Fatal("sealed marker survived terminal proof")
	}
	if err := VerifyWriterTerminalReceipt(f.boundary, terminal, evidence); err != nil {
		t.Fatal(err)
	}
	freshOracle, err := NewProtocolCrashOracle(func() time.Time { return protocolNow })
	if err != nil {
		t.Fatal(err)
	}
	freshProvider := f.forkProvider(t, HashString("synthetic-fresh-fork"))
	if _, err := NewForkController(f.boundary, freshOracle, freshProvider, f.forkObserver(t, freshProvider), f.body, terminal, evidence); !errors.Is(err, ErrForkNotAuthorized) {
		t.Fatalf("terminal receipt reset onto a fresh oracle: %v", err)
	}
	originalDigest, _ := evidence.Digest()
	if terminal.EvidenceSHA256 != originalDigest {
		t.Fatal("complete evidence digest not signed")
	}
	mutations := []func(*WriterTerminalEvidence){
		func(e *WriterTerminalEvidence) { e.NoWriterOrBroadRules = false }, func(e *WriterTerminalEvidence) { e.CapabilityRevoked = false },
		func(e *WriterTerminalEvidence) { e.LeafRevoked = false }, func(e *WriterTerminalEvidence) { e.MTLSRevoked = false },
		func(e *WriterTerminalEvidence) { e.WrappingKeyRevoked = false }, func(e *WriterTerminalEvidence) { e.BindingAbsent = false },
		func(e *WriterTerminalEvidence) { e.CredentialAbsent = false }, func(e *WriterTerminalEvidence) { e.FullRedeployComplete = false },
		func(e *WriterTerminalEvidence) { e.Deletion.DirectGETAbsent = false }, func(e *WriterTerminalEvidence) { e.Deletion.PaginatedAppCount = 1 },
		func(e *WriterTerminalEvidence) { e.Deletion.PaginatedDeployments = 1 }, func(e *WriterTerminalEvidence) { e.Deletion.DeleteActionTerminal = false },
		func(e *WriterTerminalEvidence) { e.Deletion.RollbackCapableCount = 1 }, func(e *WriterTerminalEvidence) { e.DeletionReconciled = false },
		func(e *WriterTerminalEvidence) { e.OldInstanceGraceElapsed = false }, func(e *WriterTerminalEvidence) { e.FirewallReadTwoSHA256 = HashString("synthetic-drift") },
		func(e *WriterTerminalEvidence) { e.ActionReadTwoSHA256 = HashString("synthetic-drift") }, func(e *WriterTerminalEvidence) { e.ProviderProvenanceSHA256 = HashString("synthetic-drift") },
		func(e *WriterTerminalEvidence) {
			e.OriginalProjection.ActionLedgerSHA256 = HashString("synthetic-drift")
		},
		func(e *WriterTerminalEvidence) { e.CapabilityRevokedAt = time.Time{} },
		func(e *WriterTerminalEvidence) { e.LeafRevokedAt = e.CapabilityRevokedAt.Add(-time.Nanosecond) },
		func(e *WriterTerminalEvidence) { e.MTLSRevokedAt = e.LeafRevokedAt.Add(-time.Nanosecond) },
		func(e *WriterTerminalEvidence) { e.WrappingKeyRevokedAt = e.MTLSRevokedAt.Add(-time.Nanosecond) },
		func(e *WriterTerminalEvidence) { e.BindingRemovedAt = e.WrappingKeyRevokedAt.Add(-time.Nanosecond) },
		func(e *WriterTerminalEvidence) { e.CredentialRemovedAt = e.BindingRemovedAt.Add(-time.Nanosecond) },
		func(e *WriterTerminalEvidence) { e.FullRedeployAt = e.CredentialRemovedAt.Add(-time.Nanosecond) },
		func(e *WriterTerminalEvidence) { e.FirewallRestoredAt = e.FullRedeployAt.Add(-time.Nanosecond) },
		func(e *WriterTerminalEvidence) { e.DeletionObservedAt = e.FirewallRestoredAt.Add(-time.Nanosecond) },
		func(e *WriterTerminalEvidence) {
			e.ReadTwoAt = e.ReadOneAt.Add(MinimumTerminalObservationDelay - time.Nanosecond)
		},
		func(e *WriterTerminalEvidence) {
			e.ReadTwoAt = e.ReadOneAt.Add(MaximumTerminalObservationDelay + time.Nanosecond)
		},
	}
	for index, mutate := range mutations {
		candidate := evidence
		mutate(&candidate)
		if err := VerifyWriterTerminalReceipt(f.boundary, terminal, candidate); err == nil {
			t.Fatalf("terminal mutation %d accepted", index)
		}
	}
}

func TestBeforeEffectTerminalStatusCannotCreatePublication(t *testing.T) {
	f := newProtocolFixture(t)
	ctx := context.Background()
	writer := f.writer(t, markerEntropy())
	if _, err := writer.GenerateCommitAndReadback(ctx, f.body, markerAuth(f, writer, f.body, MutationCall), FaultNone); err != nil {
		t.Fatal(err)
	}
	projection := originalProjection()
	lifecycle, provider, observer := newWriterLifecycleForTest(t, f, writer, f.body, projection)
	for _, kind := range []RevocationKind{RevokeCapability, RevokeLeaf, RevokeMTLS, RevokeWrappingKey, RemoveBinding, RemoveCredential} {
		if err := recordLifecycleRevocationForTest(t, lifecycle, f, kind, FaultNone); err != nil {
			t.Fatal(err)
		}
	}
	if err := lifecycle.RecordFullRedeploy(ctx, writerLifecycleAuth(f, lifecycle, EffectFullRedeploy, MutationCall), FaultNone); err != nil {
		t.Fatal(err)
	}
	if err := lifecycle.RecordFirewallRestored(ctx, writerLifecycleAuth(f, lifecycle, EffectTrustedSourceDel, MutationCall), FaultNone); err != nil {
		t.Fatal(err)
	}
	if err := lifecycle.RequestDelete(ctx, DeleteDefinitiveSuccess, writerLifecycleAuth(f, lifecycle, EffectBrokerDelete, MutationCall)); err != nil {
		t.Fatal(err)
	}
	elapseWriterOldInstanceGrace(f)
	evidence, err := lifecycle.ObserveTerminalEvidence(context.Background(), terminalObservationAuth(f, lifecycle, MutationCall), FaultNone)
	if err != nil {
		t.Fatal(err)
	}
	fabricated := evidence
	for _, target := range []*time.Time{
		&fabricated.CapabilityRevokedAt, &fabricated.LeafRevokedAt, &fabricated.MTLSRevokedAt,
		&fabricated.WrappingKeyRevokedAt, &fabricated.BindingRemovedAt, &fabricated.CredentialRemovedAt,
		&fabricated.FullRedeployAt, &fabricated.FirewallRestoredAt, &fabricated.DeletionObservedAt,
	} {
		*target = target.Add(time.Nanosecond)
	}
	// The fabricated evidence has no canonical digest, so no exact publication
	// request exists for the authority to bind. The endpoint rejects it before
	// entering the issued mutation path.
	if _, err := lifecycle.AcceptTerminalEvidence(ctx, fabricated, terminalPublicationAuth(f, lifecycle, evidence, MutationCall), FaultNone); !errors.Is(err, ErrForkNotAuthorized) {
		t.Fatalf("fabricated monotonic completion times accepted: %v", err)
	}
	if _, err := lifecycle.AcceptTerminalEvidence(ctx, evidence, terminalPublicationAuth(f, lifecycle, evidence, MutationCall), FaultEOFBeforeEffect); !errors.Is(err, ErrReconcileRequired) {
		t.Fatalf("before-effect terminal ambiguity: %v", err)
	}
	reconstructed, err := NewWriterLifecycle(writer, f.body, projection, provider, observer)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reconstructed.ReconcileTerminalEvidence(ctx, evidence, terminalPublicationAuth(f, reconstructed, evidence, StatusCall)); !errors.Is(err, ErrNotObserved) {
		t.Fatalf("status created missing terminal publication: %v", err)
	}
	receipt, err := reconstructed.terminalReceipt(evidence)
	if err != nil {
		t.Fatal(err)
	}
	unpublishedProvider := f.forkProvider(t, HashString("synthetic-unpublished-fork"))
	if _, err := NewForkController(f.boundary, f.effects, unpublishedProvider, f.forkObserver(t, unpublishedProvider), f.body, receipt, evidence); !errors.Is(err, ErrForkNotAuthorized) {
		t.Fatalf("unpublished terminal authorized fork: %v", err)
	}
	if !writer.SealedMarkerPresent(f.body) {
		t.Fatal("failed terminal status erased reconciliation material")
	}
}

func TestForkTerminalConsumptionIssueOnceSurvivesReconstructionAndConcurrency(t *testing.T) {
	f := newProtocolFixture(t)
	_, evidence, terminal := prepareTerminal(t, f, DeleteAmbiguousEOF, FaultNone)
	ctx := context.Background()
	candidate := HashString("synthetic-recovery-fork")
	provider := f.forkProvider(t, candidate)
	forkObserver := f.forkObserver(t, provider)
	controller, err := NewForkController(f.boundary, f.effects, provider, forkObserver, f.body, terminal, evidence)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := controller.PostOnce(ctx, forkPostAuth(f, controller, MutationCall), FaultHTTP408AfterEffect); !errors.Is(err, ErrReconcileRequired) {
		t.Fatalf("fork ambiguity: %v", err)
	}
	reconstructed, err := NewForkController(f.boundary, f.effects, provider, forkObserver, f.body, terminal, evidence)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reconstructed.PostOnce(ctx, forkPostAuth(f, reconstructed, MutationCall), FaultNone); !errors.Is(err, ErrReconcileRequired) {
		t.Fatalf("reconstructed POST reissued: %v", err)
	}
	got, err := reconstructed.Reconcile(ctx, forkPostAuth(f, reconstructed, StatusCall))
	if err != nil || got != candidate {
		t.Fatalf("fork status got=%v err=%v", got, err)
	}
	if reconstructed.PostCount() != 1 {
		t.Fatalf("POST count=%d", reconstructed.PostCount())
	}
	const callers = 16
	var wg sync.WaitGroup
	var completed atomic.Int64
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if got, err := reconstructed.PostOnce(ctx, forkPostAuth(f, reconstructed, MutationCall), FaultNone); err == nil && got == candidate {
				completed.Add(1)
			}
		}()
	}
	wg.Wait()
	if completed.Load() != callers || reconstructed.PostCount() != 1 {
		t.Fatalf("idempotent callers=%d post=%d", completed.Load(), reconstructed.PostCount())
	}
	other := reconstructed.request
	other.ParametersDigest = HashString("synthetic-other-terminal-use")
	if _, err := f.effects.AuthorizeAndConsume(ctx, other, f.auth.issued(f.effects, other, MutationCall), FaultNone, reconstructed.terminalDigest, func() (Digest, error) {
		return HashString("synthetic-forbidden-second-fork"), nil
	}); !errors.Is(err, ErrQuarantined) {
		t.Fatalf("terminal rebound: %v", err)
	}
}

func TestForkAmbiguityZeroAndMultipleQuarantine(t *testing.T) {
	for _, count := range []int{0, 2} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			f := newProtocolFixture(t)
			_, evidence, terminal := prepareTerminal(t, f, DeleteAmbiguous5xx, FaultNone)
			provider := f.forkProvider(t, HashString("synthetic-ambiguous-fork"))
			controller, err := NewForkController(f.boundary, f.effects, provider, f.forkObserver(t, provider), f.body, terminal, evidence)
			if err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			if _, err := controller.PostOnce(ctx, forkPostAuth(f, controller, MutationCall), Fault5xxBeforeEffect); !errors.Is(err, ErrReconcileRequired) {
				t.Fatal(err)
			}
			candidates := make([]Digest, count)
			for i := range candidates {
				candidates[i] = HashString(fmt.Sprintf("synthetic-candidate-%d", i))
			}
			provider.setObservedForks(candidates)
			_, err = controller.Reconcile(ctx, forkPostAuth(f, controller, StatusCall))
			if count == 0 && !errors.Is(err, ErrNotObserved) {
				t.Fatalf("zero=%v", err)
			}
			if count == 2 && !errors.Is(err, ErrMultipleObserved) {
				t.Fatalf("multiple=%v", err)
			}
			if controller.PostCount() != 0 {
				t.Fatal("quarantined fork remained query-authoritative")
			}
			f.effects.mu.Lock()
			record := f.effects.records[controller.request.key()]
			f.effects.mu.Unlock()
			if record.IssueCount != 1 {
				t.Fatal("fork was reissued")
			}
		})
	}
}

func TestForkSemanticFailureBurnsAuthenticationBeforeProviderMutation(t *testing.T) {
	f := newProtocolFixture(t)
	_, evidence, terminal := prepareTerminal(t, f, DeleteDefinitiveSuccess, FaultNone)
	provider := f.forkProvider(t, HashString("synthetic-auth-burn-fork"))
	controller, err := NewForkController(f.boundary, f.effects, provider, f.forkObserver(t, provider), f.body, terminal, evidence)
	if err != nil {
		t.Fatal(err)
	}
	original := provider.result
	provider.result = HashString("synthetic-drifted-fork-config")
	auth := forkPostAuth(f, controller, MutationCall)
	if _, err := controller.PostOnce(context.Background(), auth, FaultNone); !errors.Is(err, ErrForkNotAuthorized) {
		t.Fatalf("provider binding drift: %v", err)
	}
	provider.result = original
	if _, err := controller.PostOnce(context.Background(), auth, FaultNone); !errors.Is(err, ErrAuthReplay) {
		t.Fatalf("semantic-error authentication was reusable: %v", err)
	}
	if controller.PostCount() != 0 {
		t.Fatal("semantic failure created a fork mutation record")
	}
}

func TestConcurrentReconstructedForkControllersShareOneDurableIssue(t *testing.T) {
	f := newProtocolFixture(t)
	_, evidence, terminal := prepareTerminal(t, f, DeleteDefinitiveSuccess, FaultNone)
	want := HashString("synthetic-single-concurrent-fork")
	provider := f.forkProvider(t, want)
	forkObserver := f.forkObserver(t, provider)
	const callers = 24
	controllers := make([]*ForkController, callers)
	for index := range controllers {
		controller, err := NewForkController(f.boundary, f.effects, provider, forkObserver, f.body, terminal, evidence)
		if err != nil {
			t.Fatal(err)
		}
		controllers[index] = controller
	}
	var wg sync.WaitGroup
	results := make(chan Digest, callers)
	errorsSeen := make(chan error, callers)
	for _, controller := range controllers {
		wg.Add(1)
		go func(controller *ForkController) {
			defer wg.Done()
			got, err := controller.PostOnce(context.Background(), forkPostAuth(f, controller, MutationCall), FaultNone)
			if err != nil {
				errorsSeen <- err
				return
			}
			results <- got
		}(controller)
	}
	wg.Wait()
	close(results)
	close(errorsSeen)
	for err := range errorsSeen {
		t.Fatalf("concurrent controller failed: %v", err)
	}
	count := 0
	for got := range results {
		count++
		if got != want {
			t.Fatalf("concurrent result %v", got)
		}
	}
	if count != callers || controllers[0].PostCount() != 1 {
		t.Fatalf("results/issues=%d/%d", count, controllers[0].PostCount())
	}
}

func TestAmbiguousEffectCannotReconcileToDifferentResult(t *testing.T) {
	f := newProtocolFixture(t)
	request := testRequest(EffectForkPOST, 990)
	first := HashString("synthetic-first-provider-result")
	_, err := f.effects.AuthorizeAndConsume(context.Background(), request, f.auth.issued(f.effects, request, MutationCall), FaultHTTP408AfterEffect, HashString("synthetic-terminal"), func() (Digest, error) {
		return first, nil
	})
	if !errors.Is(err, ErrReconcileRequired) {
		t.Fatal(err)
	}
	_, err = f.effects.ReconcileStatus(context.Background(), request, f.auth.issued(f.effects, request, StatusCall), func() ([]Digest, error) {
		return []Digest{HashString("synthetic-different-provider-result")}, nil
	})
	if !errors.Is(err, ErrQuarantined) {
		t.Fatalf("different status result accepted: %v", err)
	}
}

type alternatingRecoveryReader struct {
	values  [][]byte
	index   int
	binding Digest
}

func (r *alternatingRecoveryReader) RecoveryBindingSHA256() Digest {
	return r.binding
}

func (r *alternatingRecoveryReader) ReadRecovery() ([]byte, bool, error) {
	value := r.values[r.index]
	r.index++
	return append([]byte(nil), value...), true, nil
}

type recoveryPath struct {
	terminal           WriterTerminalReceipt
	evidence           WriterTerminalEvidence
	fork               *ForkController
	forkResult         Digest
	writerPublication  RecoveryAdmissionAuthorization
	observerBoundary   *ObserverBoundary
	admissionRequest   ObserverAdmissionRequest
	authority          *ObserverAuthority
	broker             *ObserverBroker
	observerOracle     *ProtocolCrashOracle
	observerClock      *manualObservationClock
	descriptor         ObserverLifecycleDescriptor
	lifecycle          *ObserverLifecycle
	lifecycleAdmission ObserverLifecycleProof
	admission          RecoveryAdmission
	observer           *ObserverProtocol
}

func prepareObserverLifecycleAdmission(t *testing.T, f *protocolFixture, observerBoundary *ObserverBoundary, authority *ObserverAuthority, observerOracle *ProtocolCrashOracle, request ObserverAdmissionRequest) (ObserverLifecycleDescriptor, *ObserverLifecycle, ObserverLifecycleProof) {
	t.Helper()
	bodyDigest, err := f.body.Digest()
	if err != nil {
		t.Fatal(err)
	}
	descriptor := ObserverLifecycleDescriptor{
		RecoveryTargetSHA256:           request.recoveryTargetSHA256,
		ProviderConfigSHA256:           HashString("synthetic-observer-lifecycle-provider-config/v2"),
		ObserverAdmissionRequestSHA256: request.Digest(),
		ForkWriterPublicationSHA256:    request.outerEvidenceSHA256,
		ContinuityBindingSHA256:        request.continuityBindingSHA256,
	}
	provider, err := NewSyntheticObserverLifecycleProvider(bodyDigest, descriptor.RecoveryTargetSHA256, descriptor.ProviderConfigSHA256)
	if err != nil {
		t.Fatal(err)
	}
	lifecycle, err := NewObserverLifecycle(observerBoundary, observerOracle, authority, provider, f.body, descriptor, request)
	if err != nil {
		t.Fatal(err)
	}
	for _, effect := range observerAdmissionEffects {
		index := observerLifecycleEffectIndex(observerAdmissionEffects, effect)
		if err := lifecycle.ApplyAdmission(context.Background(), effect, f.auth.issued(observerOracle, lifecycle.request("admission", index, effect), MutationCall), FaultNone); err != nil {
			t.Fatalf("observer admission %s: %v", effect, err)
		}
	}
	proof, err := lifecycle.PortableProof(false)
	if err != nil {
		t.Fatal(err)
	}
	return descriptor, lifecycle, proof
}

func completeRecoveryLifecycle(t *testing.T, f *protocolFixture, path *recoveryPath) ObserverLifecycleProof {
	t.Helper()
	for _, effect := range observerCleanupEffects {
		index := observerLifecycleEffectIndex(observerCleanupEffects, effect)
		if err := path.lifecycle.ApplyCleanup(context.Background(), effect, f.auth.issued(path.observerOracle, path.lifecycle.request("cleanup", index, effect), MutationCall), FaultNone); err != nil {
			t.Fatalf("observer cleanup %s: %v", effect, err)
		}
	}
	proof, err := path.lifecycle.PortableProof(true)
	if err != nil {
		t.Fatal(err)
	}
	return proof
}

func prepareCompletedFork(t *testing.T, f *protocolFixture) (WriterTerminalEvidence, WriterTerminalReceipt, *ForkController, Digest) {
	t.Helper()
	_, evidence, terminal := prepareTerminal(t, f, DeleteAmbiguous408, FaultHTTP408AfterEffect)
	result := HashString("synthetic-recovery-fork-result")
	provider := f.forkProvider(t, result)
	fork, err := NewForkController(f.boundary, f.effects, provider, f.forkObserver(t, provider), f.body, terminal, evidence)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := fork.PostOnce(context.Background(), f.auth.issued(f.effects, fork.request, MutationCall), FaultNone); err != nil || got != result {
		t.Fatalf("fork: got=%s err=%v", got, err)
	}
	return evidence, terminal, fork, result
}

func prepareRecoveryPath(t *testing.T, f *protocolFixture) recoveryPath {
	t.Helper()
	evidence, terminal, fork, forkResult := prepareCompletedFork(t, f)
	authority, err := NewObserverAuthority(f.observerAuthority, f.observerPrivate)
	if err != nil {
		t.Fatal(err)
	}
	broker, err := NewObserverBroker(f.observerBroker, f.store.RecoveryReader())
	if err != nil {
		t.Fatal(err)
	}
	observerClock := f.clock
	observerOracle, err := newBoundProtocolCrashOracle(observerClock.Now, f.boundary.observerStore)
	if err != nil {
		t.Fatal(err)
	}
	authorization, err := fork.AuthorizeRecoveryAdmission(context.Background(), f.auth.issued(f.effects, fork.authorizationRequest, MutationCall), FaultNone)
	if err != nil {
		t.Fatalf("writer publication: %v", err)
	}
	observerBoundary, err := f.boundary.ObserverView()
	if err != nil {
		t.Fatal(err)
	}
	admissionRequest, err := PrepareObserverAdmission(f.boundary, authorization)
	if err != nil {
		t.Fatalf("prepare observer admission: %v", err)
	}
	descriptor, lifecycle, lifecycleAdmission := prepareObserverLifecycleAdmission(t, f, observerBoundary, authority, observerOracle, admissionRequest)
	publicationRequest, requestErr := recoveryAdmissionSignatureRequest(admissionRequest)
	if requestErr != nil {
		t.Fatal(requestErr)
	}
	admission, err := authority.MintRecoveryAdmission(context.Background(), observerBoundary, broker, observerOracle, f.body, admissionRequest, descriptor, lifecycleAdmission, f.auth.issued(observerOracle, publicationRequest, MutationCall), FaultNone)
	if err != nil {
		t.Fatalf("mint observer admission: %v", err)
	}
	observer, err := NewObserverProtocol(observerBoundary, observerOracle, authority, broker, observerClock, f.body, descriptor, lifecycleAdmission, admission)
	if err != nil {
		t.Fatalf("new observer protocol: %v", err)
	}
	return recoveryPath{terminal: terminal, evidence: evidence, fork: fork, forkResult: forkResult, writerPublication: authorization,
		observerBoundary: observerBoundary, admissionRequest: admissionRequest, authority: authority, broker: broker,
		observerOracle: observerOracle, observerClock: observerClock, descriptor: descriptor, lifecycle: lifecycle,
		lifecycleAdmission: lifecycleAdmission, admission: admission, observer: observer}
}

func TestRecoveryAdmissionCannotExistBeforeDurableTerminalAndFork(t *testing.T) {
	f := newProtocolFixture(t)
	_, evidence, terminal := prepareTerminal(t, f, DeleteDefinitiveSuccess, FaultNone)
	provider := f.forkProvider(t, HashString("synthetic-not-yet-created-fork"))
	fork, err := NewForkController(f.boundary, f.effects, provider, f.forkObserver(t, provider), f.body, terminal, evidence)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fork.AuthorizeRecoveryAdmission(context.Background(), forkAuthorizationAuth(f, fork, MutationCall), FaultNone); !errors.Is(err, ErrForkNotAuthorized) {
		t.Fatalf("admission before fork: %v", err)
	}

	for _, candidates := range [][]Digest{nil, {HashString("synthetic-first"), HashString("synthetic-second")}} {
		other := newProtocolFixture(t)
		_, otherEvidence, otherTerminal := prepareTerminal(t, other, DeleteDefinitiveSuccess, FaultNone)
		otherProvider := other.forkProvider(t, HashString("synthetic-invalid-fork"))
		otherFork, err := NewForkController(other.boundary, other.effects, otherProvider, other.forkObserver(t, otherProvider), other.body, otherTerminal, otherEvidence)
		if err != nil {
			t.Fatal(err)
		}
		_, postErr := otherFork.PostOnce(context.Background(), forkPostAuth(other, otherFork, MutationCall), Fault5xxBeforeEffect)
		if !errors.Is(postErr, ErrReconcileRequired) {
			t.Fatalf("fork precondition: %v", postErr)
		}
		otherProvider.setObservedForks(candidates)
		_, postErr = otherFork.Reconcile(context.Background(), forkPostAuth(other, otherFork, StatusCall))
		if postErr == nil {
			t.Fatal("zero/multiple fork outcome accepted")
		}
		if _, err := otherFork.AuthorizeRecoveryAdmission(context.Background(), forkAuthorizationAuth(other, otherFork, MutationCall), FaultNone); !errors.Is(err, ErrForkNotAuthorized) {
			t.Fatalf("admission after invalid fork: %v", err)
		}
	}
}

func TestForkControllerCannotReceiveObserverSigningAuthority(t *testing.T) {
	typeOfController := reflect.TypeOf((*ForkController)(nil))
	if _, ok := typeOfController.MethodByName("IssueRecoveryAdmission"); ok {
		t.Fatal("obsolete controller-side signed-admission path remains public")
	}
	method, ok := typeOfController.MethodByName("AuthorizeRecoveryAdmission")
	if !ok {
		t.Fatal("hash-only admission authorization method missing")
	}
	observerAuthorityType := reflect.TypeOf((*ObserverAuthority)(nil))
	privateKeyType := reflect.TypeOf(ed25519.PrivateKey(nil))
	for i := 0; i < method.Type.NumIn(); i++ {
		if method.Type.In(i) == observerAuthorityType || method.Type.In(i) == privateKeyType {
			t.Fatalf("fork controller receives observer signing authority in argument %d", i)
		}
	}
}

func TestForkFactsFacadeIsConcreteGetOnlyAndObservationIsExact(t *testing.T) {
	f := newProtocolFixture(t)
	provider := f.forkProvider(t, HashString("synthetic-facade-fork"))
	facade, err := provider.FactsFacade()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := any(facade).(RecoveryForkProvider); ok {
		t.Fatal("GET-only fork facts facade is type-assertable to mutation authority")
	}

	_, _, fork, _ := prepareCompletedFork(t, f)
	authorization, err := fork.AuthorizeRecoveryAdmission(context.Background(), forkAuthorizationAuth(f, fork, MutationCall), FaultNone)
	if err != nil {
		t.Fatal(err)
	}
	mutations := []struct {
		name string
		edit func(*RecoveryAdmissionAuthorization)
	}{
		{name: "incomplete-inventory", edit: func(a *RecoveryAdmissionAuthorization) { a.forkObservation.InventoryComplete = false }},
		{name: "zero-fork-cardinality", edit: func(a *RecoveryAdmissionAuthorization) { a.forkObservation.ForkCount = 0 }},
		{name: "multiple-fork-cardinality", edit: func(a *RecoveryAdmissionAuthorization) { a.forkObservation.ForkCount = 2 }},
		{name: "nonterminal-operation", edit: func(a *RecoveryAdmissionAuthorization) { a.forkObservation.NonterminalOperationCount = 1 }},
		{name: "observer-identity", edit: func(a *RecoveryAdmissionAuthorization) {
			a.forkObservation.ObserverIdentitySHA256 = HashString("synthetic-wrong-provider-observer")
		}},
		{name: "observation-time", edit: func(a *RecoveryAdmissionAuthorization) {
			a.forkObservation.ObservedAt = a.forkObservation.ObservedAt.Add(time.Nanosecond)
		}},
		{name: "completion-witness", edit: func(a *RecoveryAdmissionAuthorization) {
			a.forkCompletionWitness.CompletedAt = a.forkCompletionWitness.CompletedAt.Add(time.Nanosecond)
		}},
	}
	for _, mutation := range mutations {
		t.Run(mutation.name, func(t *testing.T) {
			candidate := authorization
			candidate.forkCompletionWitness.Signature = append([]byte(nil), authorization.forkCompletionWitness.Signature...)
			candidate.writerSignature = append([]byte(nil), authorization.writerSignature...)
			mutation.edit(&candidate)
			if _, err := PrepareObserverAdmission(f.boundary, candidate); !errors.Is(err, ErrForkNotAuthorized) {
				t.Fatalf("tampered independent fork proof accepted: %v", err)
			}
		})
	}
}

func TestRecoveryAdmissionAuthorizationHasNoCallerSelectedObserverOracle(t *testing.T) {
	method, ok := reflect.TypeOf((*ForkController)(nil)).MethodByName("AuthorizeRecoveryAdmission")
	if !ok {
		t.Fatal("authorization method missing")
	}
	digestType := reflect.TypeOf(Digest{})
	for i := 1; i < method.Type.NumIn(); i++ {
		if method.Type.In(i) == digestType {
			t.Fatalf("caller-selectable identity digest remains at argument %d", i)
		}
	}
}

func TestRecoveryAdmissionIsDurableIssueOnceAndStatusOnlyAfterAmbiguity(t *testing.T) {
	f := newProtocolFixture(t)
	_, _, fork, _ := prepareCompletedFork(t, f)
	authority, _ := NewObserverAuthority(f.observerAuthority, f.observerPrivate)
	broker, _ := NewObserverBroker(f.observerBroker, f.store.RecoveryReader())
	observerOracle, _ := newBoundProtocolCrashOracle(f.clock.Now, f.boundary.observerStore)
	f.effects.mu.Lock()
	unpublished, err := fork.recoveryAdmissionAuthorizationLocked()
	f.effects.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := PrepareObserverAdmission(f.boundary, unpublished); !errors.Is(err, ErrForkNotAuthorized) {
		t.Fatalf("outer verifier accepted unpublished authorization: %v", err)
	}
	if _, err := fork.AuthorizeRecoveryAdmission(context.Background(), forkAuthorizationAuth(f, fork, MutationCall), FaultHTTP408AfterEffect); !errors.Is(err, ErrReconcileRequired) {
		t.Fatalf("admission ambiguity: %v", err)
	}
	reconstructed, err := NewForkController(f.boundary, f.effects, fork.provider, fork.observer, f.body, fork.terminal, fork.evidence)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reconstructed.AuthorizeRecoveryAdmission(context.Background(), forkAuthorizationAuth(f, reconstructed, MutationCall), FaultNone); !errors.Is(err, ErrReconcileRequired) {
		t.Fatalf("admission mutation repeated: %v", err)
	}
	authorization, err := reconstructed.ReconcileRecoveryAdmissionAuthorization(context.Background(), forkAuthorizationAuth(f, reconstructed, StatusCall))
	if err != nil {
		t.Fatal(err)
	}
	if reconstructed.effects.IssueCount(authorization.publicationRequest) != 1 {
		t.Fatal("admission authorization was not issued exactly once")
	}
	observerBoundary, _ := f.boundary.ObserverView()
	admissionRequest, err := PrepareObserverAdmission(f.boundary, authorization)
	if err != nil {
		t.Fatal(err)
	}
	descriptor, _, lifecycleProof := prepareObserverLifecycleAdmission(t, f, observerBoundary, authority, observerOracle, admissionRequest)
	if _, err := authority.MintRecoveryAdmission(context.Background(), observerBoundary, broker, observerOracle, f.body, admissionRequest, descriptor, lifecycleProof, recoveryAdmissionAuth(f, observerOracle, admissionRequest, MutationCall), FaultHTTP408AfterEffect); !errors.Is(err, ErrReconcileRequired) {
		t.Fatalf("signed admission ambiguity: %v", err)
	}
	if _, err := authority.MintRecoveryAdmission(context.Background(), observerBoundary, broker, observerOracle, f.body, admissionRequest, descriptor, lifecycleProof, recoveryAdmissionAuth(f, observerOracle, admissionRequest, MutationCall), FaultNone); !errors.Is(err, ErrReconcileRequired) {
		t.Fatalf("signed admission mutation repeated: %v", err)
	}
	admission, err := authority.ReconcileRecoveryAdmission(context.Background(), observerBoundary, broker, observerOracle, f.body, admissionRequest, descriptor, lifecycleProof, recoveryAdmissionAuth(f, observerOracle, admissionRequest, StatusCall))
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyRecoveryAdmission(observerBoundary, admission); err != nil {
		t.Fatal(err)
	}
	if observerOracle.IssueCount(admission.publicationRequest) != 1 {
		t.Fatal("signed admission was not issued exactly once")
	}
	replayed, err := authority.MintRecoveryAdmission(context.Background(), observerBoundary, broker, observerOracle, f.body, admissionRequest, descriptor, lifecycleProof, recoveryAdmissionAuth(f, observerOracle, admissionRequest, MutationCall), FaultNone)
	if err != nil || !reflect.DeepEqual(admission, replayed) {
		t.Fatalf("signed admission replay changed: same=%v err=%v", reflect.DeepEqual(admission, replayed), err)
	}
	tamperedRequest := admissionRequest
	tamperedRequest.terminalBindingSHA256 = HashString("synthetic-wrong-terminal-binding")
	if _, err := authority.MintRecoveryAdmission(context.Background(), observerBoundary, broker, observerOracle, f.body, tamperedRequest, descriptor, lifecycleProof, recoveryAdmissionAuth(f, observerOracle, tamperedRequest, MutationCall), FaultNone); !errors.Is(err, ErrForkNotAuthorized) {
		t.Fatalf("tampered observer admission request accepted: %v", err)
	}

	before := newProtocolFixture(t)
	_, _, beforeFork, _ := prepareCompletedFork(t, before)
	if _, err := beforeFork.AuthorizeRecoveryAdmission(context.Background(), forkAuthorizationAuth(before, beforeFork, MutationCall), FaultEOFBeforeEffect); !errors.Is(err, ErrReconcileRequired) {
		t.Fatalf("before-effect admission: %v", err)
	}
	if _, err := beforeFork.ReconcileRecoveryAdmissionAuthorization(context.Background(), forkAuthorizationAuth(before, beforeFork, StatusCall)); !errors.Is(err, ErrNotObserved) {
		t.Fatalf("unpublished admission reconciled: %v", err)
	}

	signedBefore := newProtocolFixture(t)
	_, _, signedBeforeFork, _ := prepareCompletedFork(t, signedBefore)
	signedBeforeAuthority, _ := NewObserverAuthority(signedBefore.observerAuthority, signedBefore.observerPrivate)
	signedBeforeBroker, _ := NewObserverBroker(signedBefore.observerBroker, signedBefore.store.RecoveryReader())
	signedBeforeOracle, _ := newBoundProtocolCrashOracle(signedBefore.clock.Now, signedBefore.boundary.observerStore)
	signedBeforeAuthorization, err := signedBeforeFork.AuthorizeRecoveryAdmission(context.Background(), forkAuthorizationAuth(signedBefore, signedBeforeFork, MutationCall), FaultNone)
	if err != nil {
		t.Fatal(err)
	}
	signedBeforeBoundary, _ := signedBefore.boundary.ObserverView()
	signedBeforeRequest, err := PrepareObserverAdmission(signedBefore.boundary, signedBeforeAuthorization)
	if err != nil {
		t.Fatal(err)
	}
	signedBeforeDescriptor, _, signedBeforeLifecycle := prepareObserverLifecycleAdmission(t, signedBefore, signedBeforeBoundary, signedBeforeAuthority, signedBeforeOracle, signedBeforeRequest)
	if _, err := signedBeforeAuthority.MintRecoveryAdmission(context.Background(), signedBeforeBoundary, signedBeforeBroker, signedBeforeOracle, signedBefore.body, signedBeforeRequest, signedBeforeDescriptor, signedBeforeLifecycle, recoveryAdmissionAuth(signedBefore, signedBeforeOracle, signedBeforeRequest, MutationCall), Fault5xxBeforeEffect); !errors.Is(err, ErrReconcileRequired) {
		t.Fatalf("before-effect signed admission: %v", err)
	}
	if _, err := signedBeforeAuthority.ReconcileRecoveryAdmission(context.Background(), signedBeforeBoundary, signedBeforeBroker, signedBeforeOracle, signedBefore.body, signedBeforeRequest, signedBeforeDescriptor, signedBeforeLifecycle, recoveryAdmissionAuth(signedBefore, signedBeforeOracle, signedBeforeRequest, StatusCall)); !errors.Is(err, ErrNotObserved) {
		t.Fatalf("status minted missing signed admission: %v", err)
	}
}

func TestForkPublicationObservationOccursOnlyInsideIssueOnceAndStatusNeverCreatesIt(t *testing.T) {
	for _, fault := range []FaultMode{FaultCrashAfterAuthorization, FaultHTTP408BeforeEffect, FaultEOFBeforeEffect, Fault5xxBeforeEffect} {
		t.Run(string(fault), func(t *testing.T) {
			f := newProtocolFixture(t)
			_, _, fork, _ := prepareCompletedFork(t, f)
			reads, observations := fork.observer.counts()
			if reads != 0 || observations != 0 {
				t.Fatal("fork observer ran before publication authority")
			}
			_, err := fork.AuthorizeRecoveryAdmission(context.Background(), forkAuthorizationAuth(f, fork, MutationCall), fault)
			if fault == FaultCrashAfterAuthorization {
				if !errors.Is(err, ErrSimulatedCrash) {
					t.Fatalf("wanted crash, got %v", err)
				}
			} else if !errors.Is(err, ErrReconcileRequired) {
				t.Fatalf("wanted reconciliation, got %v", err)
			}
			reads, observations = fork.observer.counts()
			if reads != 0 || observations != 0 {
				t.Fatalf("before-effect fault performed GET/observation: %d/%d", reads, observations)
			}
			if _, err := fork.ReconcileRecoveryAdmissionAuthorization(context.Background(), forkAuthorizationAuth(f, fork, StatusCall)); !errors.Is(err, ErrNotObserved) {
				t.Fatalf("status fabricated publication: %v", err)
			}
			reads, observations = fork.observer.counts()
			if reads != 0 || observations != 0 {
				t.Fatalf("status performed GET/observation: %d/%d", reads, observations)
			}
		})
	}

	for _, fault := range []FaultMode{FaultCrashAfterEffect, FaultHTTP408AfterEffect, FaultEOFAfterEffect, Fault5xxAfterEffect} {
		t.Run(string(fault), func(t *testing.T) {
			f := newProtocolFixture(t)
			_, _, fork, _ := prepareCompletedFork(t, f)
			_, err := fork.AuthorizeRecoveryAdmission(context.Background(), forkAuthorizationAuth(f, fork, MutationCall), fault)
			if fault == FaultCrashAfterEffect {
				if !errors.Is(err, ErrSimulatedCrash) {
					t.Fatalf("wanted crash, got %v", err)
				}
			} else if !errors.Is(err, ErrReconcileRequired) {
				t.Fatalf("wanted reconciliation, got %v", err)
			}
			reads, observations := fork.observer.counts()
			if reads != 1 || observations != 1 {
				t.Fatalf("issue-once GET/observation=%d/%d", reads, observations)
			}
			if _, err := fork.ReconcileRecoveryAdmissionAuthorization(context.Background(), forkAuthorizationAuth(f, fork, StatusCall)); err != nil {
				t.Fatal(err)
			}
			reads, observations = fork.observer.counts()
			if reads != 1 || observations != 1 {
				t.Fatalf("status repeated GET/observation=%d/%d", reads, observations)
			}
		})
	}
}

func TestRecoveryAdmissionStatusDoesNotInstallSignerState(t *testing.T) {
	f := newProtocolFixture(t)
	_, _, fork, _ := prepareCompletedFork(t, f)
	authorization, err := fork.AuthorizeRecoveryAdmission(context.Background(), forkAuthorizationAuth(f, fork, MutationCall), FaultNone)
	if err != nil {
		t.Fatal(err)
	}
	boundary, err := f.boundary.ObserverView()
	if err != nil {
		t.Fatal(err)
	}
	authority, _ := NewObserverAuthority(f.observerAuthority, f.observerPrivate)
	broker, _ := NewObserverBroker(f.observerBroker, f.store.RecoveryReader())
	request, err := PrepareObserverAdmission(f.boundary, authorization)
	if err != nil {
		t.Fatal(err)
	}
	proofOracle, _ := newBoundProtocolCrashOracle(f.clock.Now, f.boundary.observerStore)
	descriptor, _, proof := prepareObserverLifecycleAdmission(t, f, boundary, authority, proofOracle, request)
	statusOracle, _ := newProtocolCrashOracle(f.clock.Now, f.boundary.observerStore)
	if statusOracle.signerRole != "" || len(statusOracle.signerPrivate) != 0 {
		t.Fatal("fresh status oracle unexpectedly has signer state")
	}
	if _, err := authority.ReconcileRecoveryAdmission(context.Background(), boundary, broker, statusOracle, f.body, request, descriptor, proof, f.auth.observerAuth()); !errors.Is(err, ErrChallengeNotIssued) {
		t.Fatal("fresh status oracle fabricated a missing admission")
	}
	if statusOracle.signerRole != "" || len(statusOracle.signerPrivate) != 0 || !statusOracle.signerRoot.IsZero() || !statusOracle.signerLedger.IsZero() {
		t.Fatal("status reconciliation installed signer/key state")
	}
}

func TestRecoveryAdmissionRejectsUnpublishedWriterSignature(t *testing.T) {
	f := newProtocolFixture(t)
	_, _, fork, _ := prepareCompletedFork(t, f)

	f.effects.mu.Lock()
	forged, err := fork.recoveryAdmissionAuthorizationLocked()
	f.effects.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	// Even possession of a syntactically valid writer-root signature is not a
	// substitute for the exact writer-ledger issue-once publication record.
	forged.writerSignature = ed25519.Sign(f.writerPrivate, forged.signingPayload())
	if err := verifyRecoveryAdmissionAuthorizationSignature(f.boundary, forged); err != nil {
		t.Fatalf("forged test setup is not cryptographically valid: %v", err)
	}
	if _, err := PrepareObserverAdmission(f.boundary, forged); !errors.Is(err, ErrForkNotAuthorized) {
		t.Fatalf("outer verifier accepted unpublished writer authorization: %v", err)
	}
}

func TestObserverRequiresOpaqueForkAdmissionAndRecoveryOnlyReads(t *testing.T) {
	f := newProtocolFixture(t)
	path := prepareRecoveryPath(t, f)
	sourceBefore, recoveryBefore := f.store.ReadCounts()
	receipt, err := path.observer.ReadRecoveryTwice(context.Background(), f.body, observerEvidenceAuth(f, &path, MutationCall), FaultNone)
	if err != nil {
		t.Fatal(err)
	}
	sourceAfter, recoveryAfter := f.store.ReadCounts()
	if sourceAfter != sourceBefore || recoveryAfter-recoveryBefore != 2 {
		t.Fatalf("observer source/recovery=%d/%d", sourceAfter-sourceBefore, recoveryAfter-recoveryBefore)
	}
	if receipt.ReadTwoAt.Sub(receipt.ReadOneAt) != MinimumObserverReadDelay {
		t.Fatal("unsigned/unbounded observer delay")
	}
	if receipt.CapabilitySHA256.IsZero() || receipt.AdmissionChainSHA256 != path.admission.Digest() ||
		receipt.ReadOneAt.Before(receipt.AuthorizationIssuedAtUTC) || !receipt.ReadOneAt.Before(receipt.AuthorizationNotAfterUTC) ||
		receipt.ReadTwoAt.Before(receipt.AuthorizationIssuedAtUTC) || !receipt.ReadTwoAt.Before(receipt.AuthorizationNotAfterUTC) {
		t.Fatal("observer receipt omitted the signed capability/read chronology")
	}
	lifecycleProof := completeRecoveryLifecycle(t, f, &path)
	if err := VerifyRecoveryBoundary(f.boundary, f.body, path.descriptor, lifecycleProof, path.terminal, path.evidence, path.writerPublication, path.admission, receipt); err != nil {
		t.Fatal(err)
	}

	admissionType := reflect.TypeOf(path.admission)
	authorization, err := path.fork.ReconcileRecoveryAdmissionAuthorization(context.Background(), forkAuthorizationAuth(f, path.fork, StatusCall))
	if err != nil {
		t.Fatal(err)
	}
	authorizationType := reflect.TypeOf(authorization)
	writerReceiptType := reflect.TypeOf(WriterReceipt{})
	terminalReceiptType := reflect.TypeOf(WriterTerminalReceipt{})
	for _, candidate := range []reflect.Type{authorizationType, admissionType} {
		for i := 0; i < candidate.NumField(); i++ {
			field := candidate.Field(i)
			if strings.Contains(strings.ToLower(field.Name), "marker") || field.Type == writerReceiptType || field.Type == terminalReceiptType {
				t.Fatalf("admission exposes writer/marker material through %s", field.Name)
			}
		}
	}

	tampered := receipt
	tampered.ReadTwoAt = tampered.ReadOneAt.Add(MaximumObserverReadDelay + time.Nanosecond)
	if err := VerifyObserverReceipt(f.boundary, tampered); err == nil {
		t.Fatal("oversize read window accepted")
	}
	tampered = receipt
	tampered.RecoveryAdmissionSHA256 = HashString("synthetic-wrong-admission")
	if err := VerifyRecoveryBoundary(f.boundary, f.body, path.descriptor, lifecycleProof, path.terminal, path.evidence, path.writerPublication, path.admission, tampered); err == nil {
		t.Fatal("wrong admission binding accepted")
	}
	badAdmission := path.admission.clone()
	badAdmission.signature[0] ^= 0xff
	if _, err := NewObserverProtocol(path.observerBoundary, path.observerOracle, path.authority, path.broker, path.observerClock, f.body, path.descriptor, path.lifecycleAdmission, badAdmission); !errors.Is(err, ErrForkNotAuthorized) {
		t.Fatalf("invalid admission signature accepted: %v", err)
	}
	if err := VerifyRecoveryBoundary(f.boundary, f.body, path.descriptor, lifecycleProof, WriterTerminalReceipt{}, path.evidence, path.writerPublication, path.admission, receipt); err == nil {
		t.Fatal("continuity accepted without terminal receipt")
	}

	otherOracle, _ := NewProtocolCrashOracle(func() time.Time { return protocolNow })
	if _, err := NewObserverProtocol(path.observerBoundary, otherOracle, path.authority, path.broker, path.observerClock, f.body, path.descriptor, path.lifecycleAdmission, path.admission); !errors.Is(err, ErrForkNotAuthorized) {
		t.Fatalf("reset observer oracle accepted: %v", err)
	}
	if _, err := NewObserverProtocol(path.observerBoundary, f.effects, path.authority, path.broker, path.observerClock, f.body, path.descriptor, path.lifecycleAdmission, path.admission); !errors.Is(err, ErrForkNotAuthorized) {
		t.Fatalf("writer oracle reused by observer: %v", err)
	}
	if _, err := NewObserverProtocol(path.observerBoundary, path.observerOracle, path.authority, path.broker, path.observerClock, f.body, path.descriptor, path.lifecycleAdmission, RecoveryAdmission{}); !errors.Is(err, ErrForkNotAuthorized) {
		t.Fatalf("missing admission accepted: %v", err)
	}
}

func TestObserverUsesOneTrustedClockAndRevalidatesBeforeEveryRead(t *testing.T) {
	f := newProtocolFixture(t)
	path := prepareRecoveryPath(t, f)
	divergent := &manualObservationClock{now: path.observerClock.Now()}
	path.observer.clock = divergent
	_, before := f.store.ReadCounts()
	auth := observerEvidenceAuth(f, &path, MutationCall)
	if _, err := path.observer.ReadRecoveryTwice(context.Background(), f.body, auth, FaultNone); !errors.Is(err, ErrForkNotAuthorized) {
		t.Fatalf("divergent observer clock accepted: %v", err)
	}
	_, after := f.store.ReadCounts()
	if after-before != 1 {
		t.Fatalf("second read occurred after trusted-clock divergence: %d", after-before)
	}
}

func TestObserverReadPostCallChronologyUsesIssuedNotBeforeAndExclusiveExpiry(t *testing.T) {
	t.Run("issued-before-nbf", func(t *testing.T) {
		f := newProtocolFixture(t)
		path := prepareRecoveryPath(t, f)
		now := path.observerClock.Now()
		auth := observerEvidenceAuth(f, &path, MutationCall)
		auth = auth.withJTI("synthetic-observer-nbf-jti")
		auth.IssuedAt = now.Add(-2 * time.Minute)
		auth.NotBefore = now
		auth.ExpiresAt = now.Add(time.Minute)
		receipt, err := path.observer.ReadRecoveryTwice(context.Background(), f.body, auth, FaultNone)
		if err != nil {
			t.Fatal(err)
		}
		if !auth.IssuedAt.Before(auth.NotBefore) || receipt.AuthorizationIssuedAtUTC.Before(auth.IssuedAt) ||
			!receipt.ReadOneAt.Equal(receipt.AuthorizationNotBeforeUTC) || !receipt.ReadTwoAt.Before(receipt.AuthorizationNotAfterUTC) {
			t.Fatalf("observer receipt chronology mismatch: issued=%s nbf=%s one=%s two=%s", receipt.AuthorizationIssuedAtUTC, receipt.AuthorizationNotBeforeUTC, receipt.ReadOneAt, receipt.ReadTwoAt)
		}
	})

	for _, targetRead := range []int{1, 2} {
		t.Run(fmt.Sprintf("expiry-after-read-%d", targetRead), func(t *testing.T) {
			f := newProtocolFixture(t)
			path := prepareRecoveryPath(t, f)
			now := path.observerClock.Now()
			auth := observerEvidenceAuth(f, &path, MutationCall)
			auth = auth.withJTI(fmt.Sprintf("synthetic-observer-expiry-jti-%d", targetRead))
			auth.IssuedAt = now.Add(-time.Minute)
			auth.NotBefore = now.Add(-time.Minute)
			auth.ExpiresAt = now.Add(10 * time.Second)
			calls := 0
			f.store.setRecoveryReadHook(func() {
				calls++
				if calls == targetRead {
					path.observerClock.now = auth.ExpiresAt
				}
			})
			if _, err := path.observer.ReadRecoveryTwice(context.Background(), f.body, auth, FaultNone); !errors.Is(err, ErrRoleIsolation) {
				t.Fatalf("post-read exclusive expiry accepted: %v", err)
			}
			if calls != targetRead {
				t.Fatalf("recovery reads=%d want=%d", calls, targetRead)
			}
		})
	}
}

func TestObserverAdmissionExpiryIsExclusiveAtConstructionAndEveryRead(t *testing.T) {
	t.Run("second-read-crosses-expiry", func(t *testing.T) {
		f := newProtocolFixture(t)
		path := prepareRecoveryPath(t, f)
		path.observerClock.now = path.admission.authorizationNotAfter.Add(-time.Nanosecond)
		_, before := f.store.ReadCounts()
		auth := observerEvidenceAuth(f, &path, MutationCall)
		auth = auth.withJTI("synthetic-expiry-read-jti")
		auth.IssuedAt = path.admission.authorizationNotAfter.Add(-4 * time.Minute)
		auth.NotBefore = path.admission.authorizationNotAfter.Add(-4 * time.Minute)
		auth.ExpiresAt = path.admission.authorizationNotAfter.Add(time.Minute)
		if _, err := path.observer.ReadRecoveryTwice(context.Background(), f.body, auth, FaultNone); !errors.Is(err, ErrRoleIsolation) {
			t.Fatalf("second read at/after exclusive expiry accepted: %v", err)
		}
		_, after := f.store.ReadCounts()
		if after-before != 1 {
			t.Fatalf("second read occurred after admission expiry: %d", after-before)
		}
	})

	t.Run("constructor-at-expiry", func(t *testing.T) {
		f := newProtocolFixture(t)
		path := prepareRecoveryPath(t, f)
		path.observerClock.now = path.admission.authorizationNotAfter
		if _, err := NewObserverProtocol(path.observerBoundary, path.observerOracle, path.authority, path.broker, path.observerClock,
			f.body, path.descriptor, path.lifecycleAdmission, path.admission); !errors.Is(err, ErrForkNotAuthorized) {
			t.Fatalf("observer constructed at exclusive admission expiry: %v", err)
		}
	})
}

func TestOuterVerifierRejectsSignedButUnpublishedObserverReceipt(t *testing.T) {
	f := newProtocolFixture(t)
	path := prepareRecoveryPath(t, f)
	request, err := observerEvidenceRequest(f.body, path.admission.Digest())
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := path.observer.buildReceipt(f.body, request, observerEvidenceAuth(f, &path, MutationCall))
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyObserverObservation(path.observerBoundary, receipt); err != nil {
		t.Fatalf("test receipt lacks a valid observation signature: %v", err)
	}
	if err := VerifyObserverReceipt(f.boundary, receipt); err == nil {
		t.Fatal("signed but unregistered observer receipt accepted")
	}
	if err := VerifyRecoveryBoundary(f.boundary, f.body, path.descriptor, path.lifecycleAdmission, path.terminal, path.evidence, path.writerPublication, path.admission, receipt); err == nil {
		t.Fatal("outer verifier accepted signed but unpublished observer receipt")
	}
}

func TestObserverReceiptSurvivesAmbiguityAndControllerReconstructionExactly(t *testing.T) {
	for _, fault := range []FaultMode{FaultCrashAfterEffect, FaultHTTP408AfterEffect, FaultEOFAfterEffect, Fault5xxAfterEffect} {
		t.Run(string(fault), func(t *testing.T) {
			f := newProtocolFixture(t)
			path := prepareRecoveryPath(t, f)
			_, recoveryBefore := f.store.ReadCounts()
			if _, err := path.observer.ReadRecoveryTwice(context.Background(), f.body, observerEvidenceAuth(f, &path, MutationCall), fault); !IsAmbiguousError(err) {
				t.Fatalf("initial ambiguity: %v", err)
			}
			_, afterIssue := f.store.ReadCounts()
			if afterIssue-recoveryBefore != 2 {
				t.Fatalf("issue reads=%d", afterIssue-recoveryBefore)
			}
			reconstructed, err := NewObserverProtocol(path.observerBoundary, path.observerOracle, path.authority, path.broker, path.observerClock, f.body, path.descriptor, path.lifecycleAdmission, path.admission)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := reconstructed.ReadRecoveryTwice(context.Background(), f.body, observerEvidenceAuth(f, &path, MutationCall), FaultNone); !errors.Is(err, ErrReconcileRequired) {
				t.Fatalf("mutation reissued after ambiguity: %v", err)
			}
			receipt, err := reconstructed.ReconcileRecoveryEvidence(context.Background(), f.body, observerEvidenceAuth(f, &path, StatusCall))
			if err != nil {
				t.Fatal(err)
			}
			replayed, err := reconstructed.ReadRecoveryTwice(context.Background(), f.body, observerEvidenceAuth(f, &path, MutationCall), FaultNone)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(receipt, replayed) {
				t.Fatal("idempotent replay changed exact signed receipt")
			}
			_, afterReplay := f.store.ReadCounts()
			if afterReplay != afterIssue {
				t.Fatalf("replay performed recovery reads: %d", afterReplay-afterIssue)
			}
			if path.observerOracle.IssueCount(observerMustRequest(t, f.body, path.admission)) != 1 {
				t.Fatal("observer evidence issued more than once")
			}
		})
	}
}

func observerMustRequest(t *testing.T, body OperationBody, admission RecoveryAdmission) MutationRequest {
	t.Helper()
	request, err := observerEvidenceRequest(body, admission.Digest())
	if err != nil {
		t.Fatal(err)
	}
	return request
}

func TestObserverBeforeEffectAmbiguityCannotCreateEvidenceDuringStatus(t *testing.T) {
	for _, fault := range []FaultMode{FaultCrashAfterAuthorization, FaultHTTP408BeforeEffect, FaultEOFBeforeEffect, Fault5xxBeforeEffect} {
		t.Run(string(fault), func(t *testing.T) {
			f := newProtocolFixture(t)
			path := prepareRecoveryPath(t, f)
			_, before := f.store.ReadCounts()
			if _, err := path.observer.ReadRecoveryTwice(context.Background(), f.body, observerEvidenceAuth(f, &path, MutationCall), fault); !IsAmbiguousError(err) {
				t.Fatalf("initial ambiguity: %v", err)
			}
			if _, err := path.observer.ReconcileRecoveryEvidence(context.Background(), f.body, observerEvidenceAuth(f, &path, StatusCall)); !errors.Is(err, ErrNotObserved) {
				t.Fatalf("status created missing receipt: %v", err)
			}
			_, after := f.store.ReadCounts()
			if after != before {
				t.Fatalf("status performed recovery reads: %d", after-before)
			}
			if _, err := path.observer.ReadRecoveryTwice(context.Background(), f.body, observerEvidenceAuth(f, &path, MutationCall), FaultNone); !errors.Is(err, ErrQuarantined) {
				t.Fatalf("missing effect not quarantined: %v", err)
			}
		})
	}
}

func TestConcurrentObserverCallsShareOneExactReceiptAndTwoReads(t *testing.T) {
	f := newProtocolFixture(t)
	path := prepareRecoveryPath(t, f)
	const callers = 24
	receipts := make(chan ObserverReceipt, callers)
	errs := make(chan error, callers)
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			receipt, err := path.observer.ReadRecoveryTwice(context.Background(), f.body, observerEvidenceAuth(f, &path, MutationCall), FaultNone)
			receipts <- receipt
			errs <- err
		}()
	}
	wg.Wait()
	close(receipts)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	var first ObserverReceipt
	for receipt := range receipts {
		if first.Version == "" {
			first = receipt
		} else if !reflect.DeepEqual(first, receipt) {
			t.Fatal("concurrent callers received different receipts")
		}
	}
	_, recoveryReads := f.store.ReadCounts()
	if recoveryReads != 2 {
		t.Fatalf("concurrent recovery reads=%d", recoveryReads)
	}
	if path.observerOracle.IssueCount(observerMustRequest(t, f.body, path.admission)) != 1 {
		t.Fatal("concurrent calls issued more than once")
	}
}

func TestObserverBodyDriftIsRejectedWithoutRevokingCompletedReceipt(t *testing.T) {
	f := newProtocolFixture(t)
	path := prepareRecoveryPath(t, f)
	receipt, err := path.observer.ReadRecoveryTwice(context.Background(), f.body, observerEvidenceAuth(f, &path, MutationCall), FaultNone)
	if err != nil {
		t.Fatal(err)
	}
	_, reads := f.store.ReadCounts()
	f.store.TamperRecovery(bytes.Repeat([]byte{0x77}, 32))
	replayed, err := path.observer.ReadRecoveryTwice(context.Background(), f.body, observerEvidenceAuth(f, &path, MutationCall), FaultNone)
	if err != nil || !reflect.DeepEqual(receipt, replayed) {
		t.Fatalf("completed receipt replaced: receipt=%v err=%v", reflect.DeepEqual(receipt, replayed), err)
	}
	_, afterReplay := f.store.ReadCounts()
	if afterReplay != reads {
		t.Fatal("completed replay reread changed recovery state")
	}

	drift := f.body
	drift.ConfigDigest = HashString("synthetic-observer-body-drift")
	if _, err := path.observer.ReadRecoveryTwice(context.Background(), drift, observerEvidenceAuth(f, &path, MutationCall), FaultNone); !errors.Is(err, ErrQuarantined) {
		t.Fatalf("body drift not quarantined: %v", err)
	}
	replayed, err = path.observer.ReadRecoveryTwice(context.Background(), f.body, observerEvidenceAuth(f, &path, MutationCall), FaultNone)
	if err != nil || !reflect.DeepEqual(receipt, replayed) {
		t.Fatalf("rejected conflict revoked immutable receipt: equal=%v err=%v", reflect.DeepEqual(receipt, replayed), err)
	}
}

func TestObserverSemanticFailureBurnsAuthenticationBeforeAnyRead(t *testing.T) {
	f := newProtocolFixture(t)
	path := prepareRecoveryPath(t, f)
	drift := f.body
	drift.ConfigDigest = HashString("synthetic-auth-burn-observer-drift")
	auth := observerEvidenceAuth(f, &path, MutationCall)
	_, before := f.store.ReadCounts()
	if _, err := path.observer.ReadRecoveryTwice(context.Background(), drift, auth, FaultNone); !errors.Is(err, ErrQuarantined) {
		t.Fatalf("observer semantic rejection: %v", err)
	}
	if _, err := path.observer.ReadRecoveryTwice(context.Background(), f.body, auth, FaultNone); !errors.Is(err, ErrAuthReplay) {
		t.Fatalf("observer semantic-error authentication was reusable: %v", err)
	}
	_, after := f.store.ReadCounts()
	if after != before {
		t.Fatalf("semantic rejection performed recovery reads: %d", after-before)
	}
}

func TestObserverRejectsDifferentRecoveryReadsAndOuterVerifierRejectsWrongStableMarker(t *testing.T) {
	f := newProtocolFixture(t)
	path := prepareRecoveryPath(t, f)
	reader := &alternatingRecoveryReader{values: [][]byte{bytes.Repeat([]byte{1}, 32), bytes.Repeat([]byte{2}, 32)}, binding: f.store.recoveryBindingSHA256()}
	badBroker, _ := NewObserverBroker(f.observerBroker, reader)
	badObserver, err := NewObserverProtocol(path.observerBoundary, path.observerOracle, path.authority, badBroker, path.observerClock, f.body, path.descriptor, path.lifecycleAdmission, path.admission)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := badObserver.ReadRecoveryTwice(context.Background(), f.body, observerEvidenceAuth(f, &path, MutationCall), FaultNone); !errors.Is(err, ErrMarkerMismatch) {
		t.Fatalf("different reads: %v", err)
	}

	other := newProtocolFixture(t)
	otherPath := prepareRecoveryPath(t, other)
	other.store.TamperRecovery(bytes.Repeat([]byte{0x44}, 32))
	wrong, err := otherPath.observer.ReadRecoveryTwice(context.Background(), other.body, observerEvidenceAuth(other, &otherPath, MutationCall), FaultNone)
	if err != nil {
		t.Fatal(err)
	}
	otherLifecycle := completeRecoveryLifecycle(t, other, &otherPath)
	if err := VerifyRecoveryBoundary(other.boundary, other.body, otherPath.descriptor, otherLifecycle, otherPath.terminal, otherPath.evidence, otherPath.writerPublication, otherPath.admission, wrong); !errors.Is(err, ErrMarkerMismatch) {
		t.Fatalf("wrong stable marker passed outer verifier: %v", err)
	}
}
