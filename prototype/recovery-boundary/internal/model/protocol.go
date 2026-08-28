package model

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"time"
)

// FixedCASV2Script is a model identifier, not an assertion that a provider
// supports Lua or that a fork copies the committed marker. Gate C must prove
// both properties against disposable nonproduction infrastructure.
const FixedCASV2Script = "rereply-fixed-one-key-cas/v2"

const (
	MinimumTerminalObservationDelay = 2 * time.Second
	MaximumTerminalObservationDelay = 30 * time.Second
	MinimumObserverReadDelay        = 2 * time.Second
	MaximumObserverReadDelay        = 30 * time.Second
)

type SourceMarkerStore interface {
	SourceBindingSHA256() Digest
	ProtectedPredecessor() (Predecessor, error)
	CompareAndSwapSource(Predecessor, [32]byte) error
	ReadSource() ([]byte, bool, error)
}

// RecoveryMarkerReader is recovery-only by construction.
type RecoveryMarkerReader interface {
	RecoveryBindingSHA256() Digest
	ReadRecovery() ([]byte, bool, error)
}

// syntheticMarkerBacking is shared only by the Gate-A loopback harness. The
// writer, observer, and fork provider receive distinct concrete facades, so an
// observer cannot recover source authority through a type assertion.
type syntheticMarkerBacking struct {
	mu                           sync.Mutex
	sourceExists, recoveryExists bool
	source, recovery             []byte
	sourceReads, recoveryReads   int
	sourceCASCalls               int
	sourceBinding                Digest
	recoveryBinding              Digest
	afterSourceCAS               func()
	afterSourceRead              func()
	afterRecoveryRead            func()
}

// SyntheticMarkerStore is a test coordinator, not a data-plane capability.
// It intentionally implements neither SourceMarkerStore nor
// RecoveryMarkerReader.
type SyntheticMarkerStore struct {
	backing  *syntheticMarkerBacking
	source   *syntheticSourceMarkerStore
	recovery *syntheticRecoveryMarkerReader
}

type syntheticSourceMarkerStore struct{ backing *syntheticMarkerBacking }
type syntheticRecoveryMarkerReader struct{ backing *syntheticMarkerBacking }

func NewSyntheticMarkerStore() *SyntheticMarkerStore {
	backing := &syntheticMarkerBacking{
		sourceBinding:   HashString("synthetic-source-target/v2"),
		recoveryBinding: HashString("synthetic-recovery-target/v2"),
	}
	return &SyntheticMarkerStore{
		backing:  backing,
		source:   &syntheticSourceMarkerStore{backing: backing},
		recovery: &syntheticRecoveryMarkerReader{backing: backing},
	}
}

func (s *SyntheticMarkerStore) SourceStore() SourceMarkerStore {
	if s == nil {
		return nil
	}
	return s.source
}

func (s *SyntheticMarkerStore) RecoveryReader() RecoveryMarkerReader {
	if s == nil {
		return nil
	}
	return s.recovery
}

func (s *SyntheticMarkerStore) sourceBindingSHA256() Digest {
	if s == nil || s.backing == nil {
		return Digest{}
	}
	return s.backing.sourceBinding
}

func (s *SyntheticMarkerStore) recoveryBindingSHA256() Digest {
	if s == nil || s.backing == nil {
		return Digest{}
	}
	return s.backing.recoveryBinding
}

func (s *syntheticSourceMarkerStore) SourceBindingSHA256() Digest {
	if s == nil || s.backing == nil {
		return Digest{}
	}
	return s.backing.sourceBinding
}

func (s *syntheticSourceMarkerStore) ProtectedPredecessor() (Predecessor, error) {
	if s == nil || s.backing == nil {
		return Predecessor{}, ErrInvalid
	}
	s.backing.mu.Lock()
	defer s.backing.mu.Unlock()
	if !s.backing.sourceExists {
		return NilPredecessor(), nil
	}
	if len(s.backing.source) == 0 {
		return EmptyPredecessor(), nil
	}
	return DigestPredecessor(HashBytes(s.backing.source)), nil
}

func (s *syntheticRecoveryMarkerReader) RecoveryBindingSHA256() Digest {
	if s == nil || s.backing == nil {
		return Digest{}
	}
	return s.backing.recoveryBinding
}

func (s *SyntheticMarkerStore) SeedSource(value []byte, exists bool) {
	s.backing.mu.Lock()
	defer s.backing.mu.Unlock()
	s.backing.sourceExists, s.backing.source = exists, append([]byte(nil), value...)
}

func (s *syntheticSourceMarkerStore) CompareAndSwapSource(predecessor Predecessor, marker [32]byte) error {
	if err := predecessor.validate(); err != nil {
		return err
	}
	s.backing.mu.Lock()
	s.backing.sourceCASCalls++
	match := false
	switch predecessor.Kind {
	case PredecessorNil:
		match = !s.backing.sourceExists
	case PredecessorEmpty:
		match = s.backing.sourceExists && len(s.backing.source) == 0
	case PredecessorDigest:
		match = s.backing.sourceExists && HashBytes(s.backing.source) == predecessor.Digest
	}
	if !match {
		s.backing.mu.Unlock()
		return ErrPredecessorMismatch
	}
	s.backing.sourceExists, s.backing.source = true, append(s.backing.source[:0], marker[:]...)
	hook := s.backing.afterSourceCAS
	s.backing.mu.Unlock()
	if hook != nil {
		hook()
	}
	return nil
}

func (s *syntheticSourceMarkerStore) ReadSource() ([]byte, bool, error) {
	s.backing.mu.Lock()
	s.backing.sourceReads++
	value, exists, hook := append([]byte(nil), s.backing.source...), s.backing.sourceExists, s.backing.afterSourceRead
	s.backing.mu.Unlock()
	if hook != nil {
		hook()
	}
	return value, exists, nil
}

func (s *syntheticRecoveryMarkerReader) ReadRecovery() ([]byte, bool, error) {
	s.backing.mu.Lock()
	s.backing.recoveryReads++
	value, exists, hook := append([]byte(nil), s.backing.recovery...), s.backing.recoveryExists, s.backing.afterRecoveryRead
	s.backing.mu.Unlock()
	if hook != nil {
		hook()
	}
	return value, exists, nil
}

func (s *SyntheticMarkerStore) setSourceHooks(afterCAS, afterRead func()) {
	s.backing.mu.Lock()
	defer s.backing.mu.Unlock()
	s.backing.afterSourceCAS, s.backing.afterSourceRead = afterCAS, afterRead
}

func (s *SyntheticMarkerStore) setRecoveryReadHook(afterRead func()) {
	s.backing.mu.Lock()
	defer s.backing.mu.Unlock()
	s.backing.afterRecoveryRead = afterRead
}

func (s *SyntheticMarkerStore) forkSourceToRecovery() error {
	s.backing.mu.Lock()
	defer s.backing.mu.Unlock()
	if !s.backing.sourceExists {
		return ErrNotObserved
	}
	s.backing.recoveryExists, s.backing.recovery = true, append(s.backing.recovery[:0], s.backing.source...)
	return nil
}

func (s *SyntheticMarkerStore) readSourceForTest() ([]byte, bool, error) {
	return s.source.ReadSource()
}

// RecoveryForkProvider is the selector-free provider boundary for the one
// configured source-to-recovery fork. Runtime callers cannot supply a source,
// destination, URL, or claimed result.
type RecoveryForkProvider interface {
	SourceBindingSHA256() Digest
	RecoveryBindingSHA256() Digest
	ProviderConfigSHA256() Digest
	CreateConfiguredFork() (Digest, error)
	ObserveConfiguredForks() ([]Digest, error)
}

// SyntheticForkProvider is a loopback-only Gate-A oracle. It proves the
// controller shape but is not evidence about a provider's real fork API.
type SyntheticForkProvider struct {
	mu                  sync.Mutex
	store               *SyntheticMarkerStore
	result              Digest
	created             bool
	observedOverride    []Digest
	hasObservedOverride bool
}

// SyntheticForkFactsFacade is a concrete GET-only projection of the fork
// provider. It has no create method and cannot be asserted to
// RecoveryForkProvider.
type SyntheticForkFactsFacade struct{ provider *SyntheticForkProvider }

type ForkObservationReceipt struct {
	Version                                                          string
	SourceBindingSHA256, RecoveryBindingSHA256, ProviderConfigSHA256 Digest
	ForkResultSHA256, ObserverIdentitySHA256                         Digest
	InventoryComplete                                                bool
	ForkCount, NonterminalOperationCount                             int
	ObservedAt                                                       time.Time
}

func (r ForkObservationReceipt) payload() []byte {
	when, _ := canonicalTime(r.ObservedAt)
	return []byte("fork-provider-observation/v2\x00" + r.Version + "\x00" + r.SourceBindingSHA256.String() + "\x00" +
		r.RecoveryBindingSHA256.String() + "\x00" + r.ProviderConfigSHA256.String() + "\x00" + r.ForkResultSHA256.String() + "\x00" +
		r.ObserverIdentitySHA256.String() + "\x00" + boolToken(r.InventoryComplete) + "\x00" +
		strconv.Itoa(r.ForkCount) + "\x00" + strconv.Itoa(r.NonterminalOperationCount) + "\x00" + when)
}
func (r ForkObservationReceipt) Digest() Digest {
	return HashBytes(r.payload())
}

func (p *SyntheticForkProvider) FactsFacade() (*SyntheticForkFactsFacade, error) {
	if p == nil || p.store == nil {
		return nil, ErrInvalid
	}
	return &SyntheticForkFactsFacade{provider: p}, nil
}
func (f *SyntheticForkFactsFacade) read() (Digest, Digest, Digest, []Digest, bool, int, error) {
	if f == nil || f.provider == nil {
		return Digest{}, Digest{}, Digest{}, nil, false, 0, ErrInvalid
	}
	p := f.provider
	p.mu.Lock()
	defer p.mu.Unlock()
	var candidates []Digest
	if p.hasObservedOverride {
		candidates = append([]Digest(nil), p.observedOverride...)
	} else if p.created {
		candidates = []Digest{p.result}
	}
	return p.SourceBindingSHA256(), p.RecoveryBindingSHA256(), p.ProviderConfigSHA256(), candidates, true, 0, nil
}

type SyntheticForkObserver struct {
	facade       *SyntheticForkFactsFacade
	identity     Digest
	now          func() time.Time
	mu           sync.Mutex
	reads        int
	observations int
}

func NewSyntheticForkObserver(facade *SyntheticForkFactsFacade, expectedIdentity Digest, now func() time.Time) (*SyntheticForkObserver, error) {
	if facade == nil || facade.provider == nil || expectedIdentity.IsZero() || now == nil {
		return nil, ErrInvalid
	}
	return &SyntheticForkObserver{facade: facade, identity: expectedIdentity, now: now}, nil
}
func (o *SyntheticForkObserver) observe(expected Digest) (ForkObservationReceipt, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.reads++
	source, recovery, config, candidates, complete, nonterminal, err := o.facade.read()
	if err != nil || !complete || nonterminal != 0 || len(candidates) != 1 || candidates[0] != expected {
		return ForkObservationReceipt{}, ErrForkNotAuthorized
	}
	receipt := ForkObservationReceipt{Version: ProtocolVersion, SourceBindingSHA256: source, RecoveryBindingSHA256: recovery,
		ProviderConfigSHA256: config, ForkResultSHA256: expected, ObserverIdentitySHA256: o.identity,
		InventoryComplete: true, ForkCount: 1, NonterminalOperationCount: nonterminal, ObservedAt: o.now()}
	o.observations++
	return receipt, nil
}

func (o *SyntheticForkObserver) counts() (int, int) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.reads, o.observations
}

func verifyForkObservation(boundary *ClosedBoundary, receipt ForkObservationReceipt, source, recovery, config, result Digest) error {
	if boundary == nil || receipt.Version != ProtocolVersion || receipt.SourceBindingSHA256 != source || receipt.RecoveryBindingSHA256 != recovery ||
		receipt.ProviderConfigSHA256 != config || receipt.ForkResultSHA256 != result || receipt.ObserverIdentitySHA256 != boundary.providerObserver ||
		!receipt.InventoryComplete || receipt.ForkCount != 1 || receipt.NonterminalOperationCount != 0 {
		return ErrForkNotAuthorized
	}
	if _, err := canonicalTime(receipt.ObservedAt); err != nil {
		return ErrForkNotAuthorized
	}
	return nil
}

func NewSyntheticForkProvider(store *SyntheticMarkerStore, result Digest) (*SyntheticForkProvider, error) {
	if store == nil || result.IsZero() || store.sourceBindingSHA256().IsZero() || store.recoveryBindingSHA256().IsZero() ||
		store.sourceBindingSHA256() == store.recoveryBindingSHA256() {
		return nil, ErrInvalid
	}
	return &SyntheticForkProvider{store: store, result: result}, nil
}

func (p *SyntheticForkProvider) SourceBindingSHA256() Digest { return p.store.sourceBindingSHA256() }
func (p *SyntheticForkProvider) RecoveryBindingSHA256() Digest {
	return p.store.recoveryBindingSHA256()
}
func (p *SyntheticForkProvider) ProviderConfigSHA256() Digest {
	if p == nil || p.store == nil || p.result.IsZero() {
		return Digest{}
	}
	return HashString("synthetic-fork-provider-config/v2\x00" + p.SourceBindingSHA256().String() + "\x00" +
		p.RecoveryBindingSHA256().String() + "\x00" + p.result.String())
}

func (p *SyntheticForkProvider) CreateConfiguredFork() (Digest, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.created {
		return Digest{}, ErrQuarantined
	}
	if err := p.store.forkSourceToRecovery(); err != nil {
		return Digest{}, err
	}
	p.created = true
	return p.result, nil
}

func (p *SyntheticForkProvider) ObserveConfiguredForks() ([]Digest, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.hasObservedOverride {
		return append([]Digest(nil), p.observedOverride...), nil
	}
	if !p.created {
		return nil, nil
	}
	return []Digest{p.result}, nil
}

// setObservedForks is test-only fault injection for zero/multiple provider
// inventory outcomes. It performs no fork or external operation.
func (p *SyntheticForkProvider) setObservedForks(candidates []Digest) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.hasObservedOverride = true
	p.observedOverride = append([]Digest(nil), candidates...)
}

func (s *SyntheticMarkerStore) TamperRecovery(value []byte) {
	s.backing.mu.Lock()
	defer s.backing.mu.Unlock()
	s.backing.recoveryExists, s.backing.recovery = true, append([]byte(nil), value...)
}

func (s *SyntheticMarkerStore) ReadCounts() (int, int) {
	s.backing.mu.Lock()
	defer s.backing.mu.Unlock()
	return s.backing.sourceReads, s.backing.recoveryReads
}

func (s *SyntheticMarkerStore) sourceCASCount() int {
	s.backing.mu.Lock()
	defer s.backing.mu.Unlock()
	return s.backing.sourceCASCalls
}

type BrokerCapability struct {
	Version                                                            string
	Role                                                               UnitRole
	Phase                                                              string
	Generation                                                         uint64
	OperationBodySHA256, AuthoritySHA256, BrokerSHA256, MTLSPeerSHA256 Digest
	TargetSHA256, AdmissionChainSHA256, AuthorizationSHA256            Digest
	AuthorizationIssuedAtUTC                                           time.Time
	AuthorizationNotBeforeUTC                                          time.Time
	AuthorizationNotAfter                                              time.Time
	Signature                                                          []byte
}

func (c BrokerCapability) payload() []byte {
	issuedAt, _ := canonicalTime(c.AuthorizationIssuedAtUTC)
	notBefore, _ := canonicalTime(c.AuthorizationNotBeforeUTC)
	notAfter, _ := canonicalTime(c.AuthorizationNotAfter)
	return []byte("broker-capability/v2\x00" + c.Version + "\x00" + string(c.Role) + "\x00" + c.Phase + "\x00" +
		strconv.FormatUint(c.Generation, 10) + "\x00" + c.OperationBodySHA256.String() + "\x00" +
		c.AuthoritySHA256.String() + "\x00" + c.BrokerSHA256.String() + "\x00" + c.MTLSPeerSHA256.String() + "\x00" + c.TargetSHA256.String() + "\x00" +
		c.AdmissionChainSHA256.String() + "\x00" + c.AuthorizationSHA256.String() + "\x00" + issuedAt + "\x00" + notBefore + "\x00" + notAfter)
}

func (c BrokerCapability) Digest() Digest { return HashBytes(append(c.payload(), c.Signature...)) }

func brokerCapabilitySkeleton(role UnitRole, body OperationBody, authority, broker, mtls, target, admissionChain, authorization Digest, authorizationIssuedAtUTC, authorizationNotBeforeUTC, authorizationNotAfter time.Time) (BrokerCapability, error) {
	bodyDigest, err := body.Digest()
	_, issuedErr := canonicalTime(authorizationIssuedAtUTC)
	_, notBeforeErr := canonicalTime(authorizationNotBeforeUTC)
	_, timeErr := canonicalTime(authorizationNotAfter)
	if err != nil || issuedErr != nil || notBeforeErr != nil || timeErr != nil || !authorizationIssuedAtUTC.Before(authorizationNotAfter) || !authorizationNotBeforeUTC.Before(authorizationNotAfter) || authority.IsZero() || broker.IsZero() || mtls.IsZero() || target.IsZero() || admissionChain.IsZero() || authorization.IsZero() {
		return BrokerCapability{}, ErrInvalid
	}
	return BrokerCapability{Version: ProtocolVersion, Role: role, Phase: body.Phase, Generation: body.Generation,
		OperationBodySHA256: bodyDigest, AuthoritySHA256: authority, BrokerSHA256: broker, MTLSPeerSHA256: mtls, TargetSHA256: target,
		AdmissionChainSHA256: admissionChain, AuthorizationSHA256: authorization, AuthorizationIssuedAtUTC: authorizationIssuedAtUTC, AuthorizationNotBeforeUTC: authorizationNotBeforeUTC, AuthorizationNotAfter: authorizationNotAfter}, nil
}

func issueBrokerCapability(privateKey ed25519.PrivateKey, role UnitRole, body OperationBody, authority, broker, mtls, target, admissionChain, authorization Digest, authorizationIssuedAtUTC, authorizationNotBeforeUTC, authorizationNotAfter time.Time) (BrokerCapability, error) {
	capability, err := brokerCapabilitySkeleton(role, body, authority, broker, mtls, target, admissionChain, authorization, authorizationIssuedAtUTC, authorizationNotBeforeUTC, authorizationNotAfter)
	if err != nil || len(privateKey) != ed25519.PrivateKeySize {
		return BrokerCapability{}, ErrInvalid
	}
	capability.Signature = ed25519.Sign(privateKey, capability.payload())
	return capability, nil
}

func verifyBrokerCapability(publicKey ed25519.PublicKey, c BrokerCapability, role UnitRole, body OperationBody, authority, broker, mtls, target, admissionChain Digest, now time.Time) error {
	bodyDigest, err := body.Digest()
	_, issuedErr := canonicalTime(c.AuthorizationIssuedAtUTC)
	_, notBeforeErr := canonicalTime(c.AuthorizationNotBeforeUTC)
	_, timeErr := canonicalTime(c.AuthorizationNotAfter)
	_, nowErr := canonicalTime(now)
	if err != nil || len(publicKey) != ed25519.PublicKeySize || len(c.Signature) != ed25519.SignatureSize ||
		c.Version != ProtocolVersion || c.Role != role || c.Phase != body.Phase || c.Generation != body.Generation ||
		c.OperationBodySHA256 != bodyDigest || c.AuthoritySHA256 != authority || c.BrokerSHA256 != broker ||
		c.MTLSPeerSHA256 != mtls || c.TargetSHA256 != target || c.AdmissionChainSHA256 != admissionChain ||
		c.AuthorizationSHA256.IsZero() || issuedErr != nil || notBeforeErr != nil || timeErr != nil || nowErr != nil ||
		!c.AuthorizationIssuedAtUTC.Before(c.AuthorizationNotAfter) || !c.AuthorizationNotBeforeUTC.Before(c.AuthorizationNotAfter) ||
		now.Before(c.AuthorizationIssuedAtUTC) || now.Before(c.AuthorizationNotBeforeUTC) || !now.Before(c.AuthorizationNotAfter) ||
		!ed25519.Verify(publicKey, c.payload(), c.Signature) {
		return ErrRoleIsolation
	}
	return nil
}

type WriterReceipt struct {
	Version, OperationID, Phase                                             string
	Generation                                                              uint64
	OperationBodySHA256, MarkerSHA256, FixedKeySHA256, SourceTargetSHA256   Digest
	CapabilitySHA256, AdmissionChainSHA256                                  Digest
	AuthoritySHA256, BrokerSHA256, LedgerSHA256, RootSHA256, BoundarySHA256 Digest
	AuthorizationIssuedAtUTC, AuthorizationNotBeforeUTC                     time.Time
	AuthorizationNotAfterUTC, MarkerCASCompletedAtUTC                       time.Time
	SourceReadCompletedAtUTC, BrokerActionAtUTC                             time.Time
	Signature                                                               []byte
}

func (r WriterReceipt) payload() []byte {
	issuedAt, _ := canonicalTime(r.AuthorizationIssuedAtUTC)
	notBefore, _ := canonicalTime(r.AuthorizationNotBeforeUTC)
	notAfter, _ := canonicalTime(r.AuthorizationNotAfterUTC)
	casCompletedAt, _ := canonicalTime(r.MarkerCASCompletedAtUTC)
	readCompletedAt, _ := canonicalTime(r.SourceReadCompletedAtUTC)
	actionAt, _ := canonicalTime(r.BrokerActionAtUTC)
	return []byte("writer-receipt/v2\x00" + r.Version + "\x00" + r.OperationID + "\x00" + strconv.FormatUint(r.Generation, 10) + "\x00" + r.Phase + "\x00" +
		r.OperationBodySHA256.String() + "\x00" + r.MarkerSHA256.String() + "\x00" + r.FixedKeySHA256.String() + "\x00" + r.SourceTargetSHA256.String() + "\x00" +
		r.CapabilitySHA256.String() + "\x00" + r.AdmissionChainSHA256.String() + "\x00" +
		r.AuthoritySHA256.String() + "\x00" + r.BrokerSHA256.String() + "\x00" + r.LedgerSHA256.String() + "\x00" +
		r.RootSHA256.String() + "\x00" + r.BoundarySHA256.String() + "\x00" + issuedAt + "\x00" + notBefore + "\x00" + notAfter + "\x00" +
		casCompletedAt + "\x00" + readCompletedAt + "\x00" + actionAt)
}

func writerReceiptDigest(receipt WriterReceipt) Digest {
	return HashBytes(append(receipt.payload(), receipt.Signature...))
}

func VerifyWriterReceipt(boundary *ClosedBoundary, receipt WriterReceipt) error {
	_, issuedErr := canonicalTime(receipt.AuthorizationIssuedAtUTC)
	_, notBeforeErr := canonicalTime(receipt.AuthorizationNotBeforeUTC)
	_, notAfterErr := canonicalTime(receipt.AuthorizationNotAfterUTC)
	_, casCompletedErr := canonicalTime(receipt.MarkerCASCompletedAtUTC)
	_, readCompletedErr := canonicalTime(receipt.SourceReadCompletedAtUTC)
	_, actionErr := canonicalTime(receipt.BrokerActionAtUTC)
	if boundary == nil || receipt.Version != ProtocolVersion || !validSyntheticToken(receipt.OperationID) || receipt.Generation == 0 || !validPhase(receipt.Phase) || receipt.OperationBodySHA256.IsZero() || len(receipt.Signature) != ed25519.SignatureSize ||
		receipt.MarkerSHA256.IsZero() || receipt.FixedKeySHA256 != HashString(FixedMarkerKey) || receipt.SourceTargetSHA256.IsZero() ||
		receipt.CapabilitySHA256.IsZero() || receipt.AdmissionChainSHA256.IsZero() ||
		receipt.AuthoritySHA256 != boundary.writerAuthority.IdentityDigest() || receipt.BrokerSHA256 != boundary.writerBroker.IdentityDigest() ||
		receipt.LedgerSHA256 != boundary.writerLedger || receipt.RootSHA256 != boundary.writerRoot || receipt.BoundarySHA256 != boundary.digest ||
		issuedErr != nil || notBeforeErr != nil || notAfterErr != nil || casCompletedErr != nil || readCompletedErr != nil || actionErr != nil || !receipt.AuthorizationIssuedAtUTC.Before(receipt.AuthorizationNotAfterUTC) ||
		!receipt.AuthorizationNotBeforeUTC.Before(receipt.AuthorizationNotAfterUTC) || receipt.BrokerActionAtUTC.Before(receipt.AuthorizationIssuedAtUTC) ||
		receipt.MarkerCASCompletedAtUTC.Before(receipt.AuthorizationIssuedAtUTC) || receipt.MarkerCASCompletedAtUTC.Before(receipt.AuthorizationNotBeforeUTC) || !receipt.MarkerCASCompletedAtUTC.Before(receipt.AuthorizationNotAfterUTC) ||
		receipt.SourceReadCompletedAtUTC.Before(receipt.MarkerCASCompletedAtUTC) || !receipt.SourceReadCompletedAtUTC.Before(receipt.AuthorizationNotAfterUTC) ||
		!receipt.BrokerActionAtUTC.Equal(receipt.SourceReadCompletedAtUTC) || receipt.BrokerActionAtUTC.Before(receipt.AuthorizationNotBeforeUTC) || !receipt.BrokerActionAtUTC.Before(receipt.AuthorizationNotAfterUTC) ||
		!ed25519.Verify(boundary.writerPublic, receipt.payload(), receipt.Signature) {
		return ErrInvalid
	}
	return nil
}

type WriterAuthority struct {
	unit       SubstrateUnit
	privateKey ed25519.PrivateKey
}

func NewWriterAuthority(unit SubstrateUnit, privateKey ed25519.PrivateKey) (*WriterAuthority, error) {
	if unit.Role() != WriterAuthorityRole || unit.Domain() != NoDataDomain || len(privateKey) != ed25519.PrivateKeySize {
		return nil, ErrRoleIsolation
	}
	return &WriterAuthority{unit: unit, privateKey: append(ed25519.PrivateKey(nil), privateKey...)}, nil
}

// IssueChallenge generates the fresh 256-bit authentication challenge inside
// the writer authority and durably binds it to one exact request and call kind
// before returning it. Callers can transport the opaque value but cannot mint
// or retarget it.
func (a *WriterAuthority) IssueChallenge(ctx context.Context, effects *ProtocolCrashOracle, request MutationRequest, call CallKind) (AuthorityChallenge, error) {
	if a == nil || effects == nil || a.unit.Role() != WriterAuthorityRole || len(a.privateKey) != ed25519.PrivateKeySize {
		return AuthorityChallenge{}, ErrRoleIsolation
	}
	root := HashBytes(append([]byte("writer-root/v2\x00"), a.privateKey.Public().(ed25519.PublicKey)...))
	return effects.issueAuthorityChallenge(ctx, request, call, WriterAuthorityRole, root, rand.Reader)
}

type WriterBroker struct {
	unit         SubstrateUnit
	mtlsPeer     Digest
	target       Digest
	authority    Digest
	public       ed25519.PublicKey
	markerRandom io.Reader
	source       SourceMarkerStore
	sealer       MarkerSealer
	now          func() time.Time
}

// MarkerSealer is the broker-confined wrapping boundary. Production-facing
// code has no default implementation: Gate C must supply a transactional,
// durable, role-bound key service. Gate A supplies only the fault-free
// process-local SyntheticMarkerSealer test oracle below.
type MarkerSealer interface {
	BindingSHA256() Digest
	Seal([]byte, []byte) ([]byte, []byte, error)
	Open([]byte, []byte, []byte) ([]byte, error)
	Revoke() error
	RevocationStatus() (MarkerSealerRevocationStatus, error)
	IsTestOracleOnly() bool
}

// MarkerSealerRevocationStatus is the exact status-only result used to
// reconcile an ambiguous wrapping-key revocation. A boolean cannot distinguish
// a proven terminal revocation from a failed or unavailable status read.
type MarkerSealerRevocationStatus struct {
	Version                                   string
	BindingSHA256, ActionSHA256, ResultSHA256 Digest
	Terminal                                  bool
	ObservedAt                                time.Time
}

func (s MarkerSealerRevocationStatus) Digest() (Digest, error) {
	when, err := canonicalTime(s.ObservedAt)
	if err != nil || s.Version != ProtocolVersion || s.BindingSHA256.IsZero() || s.ActionSHA256.IsZero() || s.ResultSHA256.IsZero() || !s.Terminal {
		return Digest{}, ErrInvalid
	}
	return HashString("marker-sealer-revocation-status/v2\x00" + s.Version + "\x00" + s.BindingSHA256.String() + "\x00" + s.ActionSHA256.String() + "\x00" + s.ResultSHA256.String() + "\x00" + when), nil
}

type SyntheticMarkerSealer struct {
	mu              sync.Mutex
	key             [32]byte
	binding         Digest
	revoked         bool
	revokedAt       time.Time
	now             func() time.Time
	revokeCalls     int
	statusCalls     int
	revokeAmbiguity ProviderAmbiguityKind
	statusFailure   error
}

func NewSyntheticMarkerSealer() (*SyntheticMarkerSealer, error) {
	sealer := &SyntheticMarkerSealer{}
	if _, err := io.ReadFull(rand.Reader, sealer.key[:]); err != nil {
		return nil, ErrInvalid
	}
	sealer.binding = HashBytes(append([]byte("synthetic-marker-sealer/v2\x00"), sealer.key[:]...))
	return sealer, nil
}

func (*SyntheticMarkerSealer) IsTestOracleOnly() bool { return true }
func (s *SyntheticMarkerSealer) BindingSHA256() Digest {
	if s == nil {
		return Digest{}
	}
	return s.binding
}
func (s *SyntheticMarkerSealer) Seal(aad, plaintext []byte) ([]byte, []byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.revoked || len(aad) == 0 || len(plaintext) != 32 {
		return nil, nil, ErrRoleIsolation
	}
	block, err := aes.NewCipher(s.key[:])
	if err != nil {
		return nil, nil, ErrInvalid
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, nil, ErrInvalid
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, nil, ErrInvalid
	}
	return nonce, aead.Seal(nil, nonce, plaintext, aad), nil
}
func (s *SyntheticMarkerSealer) Open(aad, nonce, ciphertext []byte) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.revoked || len(aad) == 0 {
		return nil, ErrRoleIsolation
	}
	block, err := aes.NewCipher(s.key[:])
	if err != nil {
		return nil, ErrInvalid
	}
	aead, err := cipher.NewGCM(block)
	if err != nil || len(nonce) != aead.NonceSize() {
		return nil, ErrInvalid
	}
	plaintext, err := aead.Open(nil, nonce, ciphertext, aad)
	if err != nil {
		return nil, ErrInvalid
	}
	return plaintext, nil
}
func (s *SyntheticMarkerSealer) Revoke() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.revoked {
		return ErrQuarantined
	}
	if s.now == nil {
		return ErrInvalid
	}
	s.revokeCalls++
	clear(s.key[:])
	s.revoked = true
	s.revokedAt = s.now()
	if s.revokeAmbiguity != "" {
		return &ProviderAmbiguityError{Kind: s.revokeAmbiguity}
	}
	return nil
}
func (s *SyntheticMarkerSealer) bindClock(now func() time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if now == nil {
		return ErrRoleIsolation
	}
	if s.now == nil {
		s.now = now
	}
	return nil
}
func (s *SyntheticMarkerSealer) RevocationStatus() (MarkerSealerRevocationStatus, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.statusCalls++
	if s.statusFailure != nil {
		return MarkerSealerRevocationStatus{}, s.statusFailure
	}
	if !s.revoked || s.revokedAt.IsZero() {
		return MarkerSealerRevocationStatus{}, ErrNotObserved
	}
	action := HashString("synthetic-marker-sealer-revoke-action/v2\x00" + s.binding.String())
	result := HashString("synthetic-marker-sealer-revoke-result/v2\x00" + s.binding.String() + "\x00" + action.String())
	return MarkerSealerRevocationStatus{Version: ProtocolVersion, BindingSHA256: s.binding, ActionSHA256: action, ResultSHA256: result, Terminal: true, ObservedAt: s.revokedAt}, nil
}

func (s *SyntheticMarkerSealer) setRevokeAmbiguity(kind ProviderAmbiguityKind) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.revokeAmbiguity = kind
}

func (s *SyntheticMarkerSealer) setStatusFailure(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.statusFailure = err
}

func (s *SyntheticMarkerSealer) callCounts() (int, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.revokeCalls, s.statusCalls
}

func markerSealerActive(sealer MarkerSealer) bool {
	if sealer == nil {
		return false
	}
	_, err := sealer.RevocationStatus()
	return errors.Is(err, ErrNotObserved)
}

func markerSealerTerminalStatus(sealer MarkerSealer) (MarkerSealerRevocationStatus, error) {
	if sealer == nil {
		return MarkerSealerRevocationStatus{}, ErrInvalid
	}
	status, err := sealer.RevocationStatus()
	if err != nil {
		return MarkerSealerRevocationStatus{}, err
	}
	if _, err := status.Digest(); err != nil || status.BindingSHA256 != sealer.BindingSHA256() {
		return MarkerSealerRevocationStatus{}, ErrInvalid
	}
	return status, nil
}

// sealedMarkerToken is opaque outside the fixed-function writer broker. The
// authority ledger may persist the authenticated ciphertext for status-only
// reconciliation, but never the marker preimage or a data-plane handle.
type sealedMarkerToken struct {
	version                                         string
	operationSHA256, capabilitySHA256, targetSHA256 Digest
	markerSHA256, sealerSHA256                      Digest
	predecessor                                     Predecessor
	brokerActionAtUTC                               time.Time
	nonce, ciphertext                               []byte
}

func (t sealedMarkerToken) clone() sealedMarkerToken {
	t.nonce = append([]byte(nil), t.nonce...)
	t.ciphertext = append([]byte(nil), t.ciphertext...)
	return t
}

func (t sealedMarkerToken) aad() []byte {
	actionAt, _ := canonicalTime(t.brokerActionAtUTC)
	predecessor, _ := t.predecessor.digest()
	return []byte("sealed-marker-token/v2\x00" + t.version + "\x00" + t.operationSHA256.String() + "\x00" +
		t.capabilitySHA256.String() + "\x00" + t.targetSHA256.String() + "\x00" + t.markerSHA256.String() + "\x00" + t.sealerSHA256.String() + "\x00" + predecessor.String() + "\x00" + actionAt)
}

func (t *sealedMarkerToken) erase() {
	clear(t.nonce)
	clear(t.ciphertext)
	*t = sealedMarkerToken{}
}

func NewWriterBroker(unit SubstrateUnit, markerRandom io.Reader, source SourceMarkerStore, sealer MarkerSealer) (*WriterBroker, error) {
	if unit.Role() != WriterBrokerRole || unit.Domain() != SourceDomain || source == nil || source.SourceBindingSHA256().IsZero() ||
		sealer == nil || !sealer.IsTestOracleOnly() || sealer.BindingSHA256().IsZero() || !markerSealerActive(sealer) {
		return nil, ErrRoleIsolation
	}
	if markerRandom == nil {
		markerRandom = rand.Reader
	}
	broker := &WriterBroker{unit: unit, mtlsPeer: HashString("mtls-peer/v2\x00" + unit.rawIdentifier),
		target: source.SourceBindingSHA256(), markerRandom: markerRandom, source: source, sealer: sealer}
	return broker, nil
}

func (b *WriterBroker) validateBinding() error {
	if b == nil || b.source == nil || b.sealer == nil || !markerSealerActive(b.sealer) || b.now == nil || b.target.IsZero() || b.authority.IsZero() || len(b.public) != ed25519.PublicKeySize ||
		b.source.SourceBindingSHA256() != b.target {
		return ErrRoleIsolation
	}
	return nil
}

func (b *WriterBroker) bindAuthority(authority Digest, public ed25519.PublicKey) error {
	if b == nil || authority.IsZero() || len(public) != ed25519.PublicKeySize {
		return ErrRoleIsolation
	}
	if (!b.authority.IsZero() && b.authority != authority) || (len(b.public) != 0 && !bytes.Equal(b.public, public)) {
		return ErrRoleIsolation
	}
	b.authority = authority
	b.public = append(ed25519.PublicKey(nil), public...)
	return nil
}

func (b *WriterBroker) sealMarker(body, capability, marker Digest, predecessor Predecessor, actionAt time.Time, raw []byte) (sealedMarkerToken, error) {
	if err := b.validateBinding(); err != nil || body.IsZero() || capability.IsZero() || marker.IsZero() || predecessor.validate() != nil || len(raw) != 32 {
		return sealedMarkerToken{}, ErrInvalid
	}
	if _, err := canonicalTime(actionAt); err != nil {
		return sealedMarkerToken{}, ErrInvalid
	}
	token := sealedMarkerToken{version: ProtocolVersion, operationSHA256: body, capabilitySHA256: capability,
		targetSHA256: b.target, markerSHA256: marker, sealerSHA256: b.sealer.BindingSHA256(), predecessor: predecessor, brokerActionAtUTC: actionAt}
	var err error
	token.nonce, token.ciphertext, err = b.sealer.Seal(token.aad(), raw)
	if err != nil {
		return sealedMarkerToken{}, err
	}
	return token, nil
}

func (b *WriterBroker) prepareMarker(body OperationBody, capability BrokerCapability) (Digest, sealedMarkerToken, error) {
	if err := b.validateBinding(); err != nil {
		return Digest{}, sealedMarkerToken{}, err
	}
	actionAt := b.now()
	if err := verifyBrokerCapability(b.public, capability, WriterBrokerRole, body, b.authority,
		b.unit.IdentityDigest(), b.mtlsPeer, b.target, capability.AdmissionChainSHA256, actionAt); err != nil {
		return Digest{}, sealedMarkerToken{}, err
	}
	predecessor, err := b.source.ProtectedPredecessor()
	if err != nil {
		return Digest{}, sealedMarkerToken{}, err
	}
	marker := make([]byte, 32)
	if _, err := io.ReadFull(b.markerRandom, marker); err != nil {
		clear(marker)
		return Digest{}, sealedMarkerToken{}, fmt.Errorf("marker generation failed")
	}
	digest := HashBytes(marker)
	if predecessor.Kind == PredecessorDigest && digest == predecessor.Digest {
		clear(marker)
		return Digest{}, sealedMarkerToken{}, ErrPredecessorMismatch
	}
	bodyDigest, _ := body.Digest()
	token, err := b.sealMarker(bodyDigest, capability.Digest(), digest, predecessor, actionAt, marker)
	clear(marker)
	if err != nil {
		return Digest{}, sealedMarkerToken{}, err
	}
	return digest, token, nil
}

func (b *WriterBroker) capabilityTime(body OperationBody, capability BrokerCapability, previous time.Time) (time.Time, error) {
	if err := b.validateBinding(); err != nil {
		return time.Time{}, err
	}
	observed := b.now()
	if (!previous.IsZero() && observed.Before(previous)) || verifyBrokerCapability(b.public, capability, WriterBrokerRole, body, b.authority,
		b.unit.IdentityDigest(), b.mtlsPeer, b.target, capability.AdmissionChainSHA256, observed) != nil {
		return time.Time{}, ErrRoleIsolation
	}
	return observed, nil
}

func (b *WriterBroker) openMarker(body OperationBody, capability BrokerCapability, token sealedMarkerToken) ([]byte, time.Time, error) {
	if err := b.validateBinding(); err != nil {
		return nil, time.Time{}, err
	}
	bodyDigest, err := body.Digest()
	beforeOpen, clockErr := b.capabilityTime(body, capability, token.brokerActionAtUTC)
	if err != nil || clockErr != nil || token.version != ProtocolVersion ||
		token.operationSHA256 != bodyDigest || token.capabilitySHA256 != capability.Digest() || token.targetSHA256 != b.target ||
		token.brokerActionAtUTC.Before(capability.AuthorizationIssuedAtUTC) || token.brokerActionAtUTC.Before(capability.AuthorizationNotBeforeUTC) || !token.brokerActionAtUTC.Before(capability.AuthorizationNotAfter) ||
		token.markerSHA256.IsZero() || token.sealerSHA256 != b.sealer.BindingSHA256() || token.predecessor.validate() != nil {
		return nil, time.Time{}, ErrRoleIsolation
	}
	expected, err := b.sealer.Open(token.aad(), token.nonce, token.ciphertext)
	afterOpen, clockErr := b.capabilityTime(body, capability, beforeOpen)
	if clockErr != nil {
		clear(expected)
		return nil, time.Time{}, clockErr
	}
	if err != nil || len(expected) != 32 || HashBytes(expected) != token.markerSHA256 {
		clear(expected)
		return nil, time.Time{}, ErrInvalid
	}
	return expected, afterOpen, nil
}

func (b *WriterBroker) commitPrepared(body OperationBody, capability BrokerCapability, token sealedMarkerToken) (Digest, time.Time, time.Time, error) {
	marker, openedAt, err := b.openMarker(body, capability, token)
	if err != nil {
		return Digest{}, time.Time{}, time.Time{}, err
	}
	defer clear(marker)
	var exact [32]byte
	copy(exact[:], marker)
	defer clear(exact[:])
	beforeCAS, clockErr := b.capabilityTime(body, capability, openedAt)
	if clockErr != nil {
		return Digest{}, time.Time{}, time.Time{}, clockErr
	}
	casErr := b.source.CompareAndSwapSource(token.predecessor, exact)
	afterCAS, clockErr := b.capabilityTime(body, capability, beforeCAS)
	if clockErr != nil {
		return Digest{}, time.Time{}, time.Time{}, clockErr
	}
	if casErr != nil {
		return Digest{}, time.Time{}, time.Time{}, casErr
	}
	beforeRead, clockErr := b.capabilityTime(body, capability, afterCAS)
	if clockErr != nil {
		return Digest{}, time.Time{}, time.Time{}, clockErr
	}
	readback, exists, err := b.source.ReadSource()
	defer clear(readback)
	afterRead, clockErr := b.capabilityTime(body, capability, beforeRead)
	if clockErr != nil {
		return Digest{}, time.Time{}, time.Time{}, clockErr
	}
	if err != nil || !exists || len(readback) != 32 || subtle.ConstantTimeCompare(readback, marker) != 1 {
		return Digest{}, time.Time{}, time.Time{}, ErrMarkerMismatch
	}
	return HashBytes(readback), afterCAS, afterRead, nil
}

func (b *WriterBroker) reconcileReadback(body OperationBody, capability BrokerCapability, token sealedMarkerToken) (Digest, bool, bool, time.Time, error) {
	expected, openedAt, err := b.openMarker(body, capability, token)
	if err != nil {
		return Digest{}, false, false, time.Time{}, err
	}
	defer clear(expected)
	beforeRead, clockErr := b.capabilityTime(body, capability, openedAt)
	if clockErr != nil {
		return Digest{}, false, false, time.Time{}, clockErr
	}
	observed, exists, err := b.source.ReadSource()
	defer clear(observed)
	afterRead, clockErr := b.capabilityTime(body, capability, beforeRead)
	if clockErr != nil {
		return Digest{}, false, false, time.Time{}, clockErr
	}
	if err != nil {
		return Digest{}, false, false, time.Time{}, err
	}
	if exists && len(observed) == 32 && subtle.ConstantTimeCompare(observed, expected) == 1 {
		return HashBytes(observed), true, false, afterRead, nil
	}
	predecessorUnchanged := false
	switch token.predecessor.Kind {
	case PredecessorNil:
		predecessorUnchanged = !exists
	case PredecessorEmpty:
		predecessorUnchanged = exists && len(observed) == 0
	case PredecessorDigest:
		predecessorUnchanged = exists && HashBytes(observed) == token.predecessor.Digest
	}
	return Digest{}, false, predecessorUnchanged, afterRead, nil
}

type WriterProtocol struct {
	boundary          *ClosedBoundary
	effects           *ProtocolCrashOracle
	authority         *WriterAuthority
	broker            *WriterBroker
	admissionProvider WriterProviderEffects
}

var writerAdmissionEffects = []Effect{
	EffectBrokerCreate, EffectBrokerUpdate, EffectTrustedSourceAdd,
	EffectBindingInstall, EffectCredentialInstall, EffectLeafIssue,
	EffectMTLSIssue, EffectCapabilityIssue,
}

// WriterProviderEffects is a selector-free provider adapter. The exact target
// and configuration are constructor-bound; runtime callers can select only a
// reviewed effect from writerAdmissionEffects. Gate A provides only the
// loopback oracle below.
type WriterProviderEffects interface {
	IdentitySHA256() Digest
	TargetSHA256() Digest
	ConfigSHA256() Digest
	ApplyConfigured(Effect) (Digest, error)
	ObserveConfigured(Effect) ([]Digest, error)
	IsTestOracleOnly() bool
}

type SyntheticWriterProvider struct {
	mu               sync.Mutex
	identity, target Digest
	config           Digest
	results          map[Effect]Digest
	issues           map[Effect]int
}

func NewSyntheticWriterProvider(target Digest) (*SyntheticWriterProvider, error) {
	if target.IsZero() {
		return nil, ErrInvalid
	}
	return &SyntheticWriterProvider{
		identity: HashString("synthetic-writer-provider/v2\x00" + target.String()), target: target,
		config:  HashString("synthetic-writer-provider-config/v2\x00" + target.String()),
		results: map[Effect]Digest{}, issues: map[Effect]int{},
	}, nil
}

func (*SyntheticWriterProvider) IsTestOracleOnly() bool   { return true }
func (p *SyntheticWriterProvider) IdentitySHA256() Digest { return p.identity }
func (p *SyntheticWriterProvider) TargetSHA256() Digest   { return p.target }
func (p *SyntheticWriterProvider) ConfigSHA256() Digest   { return p.config }
func (p *SyntheticWriterProvider) ApplyConfigured(effect Effect) (Digest, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if effect == EffectCapabilityIssue || !writerAdmissionEffectAllowed(effect) || p.issues[effect] != 0 {
		return Digest{}, ErrQuarantined
	}
	result := HashString("synthetic-writer-provider-result/v2\x00" + p.identity.String() + "\x00" + p.target.String() + "\x00" + p.config.String() + "\x00" + string(effect))
	p.issues[effect]++
	p.results[effect] = result
	return result, nil
}
func (p *SyntheticWriterProvider) ObserveConfigured(effect Effect) ([]Digest, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if effect == EffectCapabilityIssue || !writerAdmissionEffectAllowed(effect) {
		return nil, ErrInvalid
	}
	result := p.results[effect]
	if result.IsZero() {
		return nil, nil
	}
	return []Digest{result}, nil
}

func writerAdmissionEffectAllowed(effect Effect) bool {
	for _, candidate := range writerAdmissionEffects {
		if effect == candidate {
			return true
		}
	}
	return false
}

// writerAdmissionChainDigest commits the exact ordered request/result/
// completion-witness chain for every provider-side admission effect that must
// precede capability issuance. Set-like equality is intentionally
// insufficient: order, multiplicity, provider binding, and durable completion
// are all authority.
func writerAdmissionChainDigest(writer *WriterProtocol, base OperationBody, projection Digest) (Digest, error) {
	if writer == nil || writer.effects == nil || projection.IsZero() {
		return Digest{}, ErrInvalid
	}
	chain := []byte("writer-admission-chain/v2\x00")
	for _, effect := range writerAdmissionEffects {
		if effect == EffectCapabilityIssue {
			break
		}
		request := writerAdmissionRequest(writer, base, projection, effect)
		requestDigest, err := request.Digest()
		result, resultOK := writer.effects.result(request)
		witness, witnessOK := writer.effects.completionWitness(request)
		if err != nil || !resultOK || !witnessOK || result.IsZero() || witness.ResultSHA256 != result || witness.RequestSHA256 != requestDigest {
			return Digest{}, ErrForkNotAuthorized
		}
		chain = append(chain, []byte(string(effect)+"\x00"+requestDigest.String()+"\x00"+result.String()+"\x00"+witness.Digest().String()+"\x00")...)
	}
	return HashBytes(chain), nil
}

func writerAdmissionChainDigestLocked(writer *WriterProtocol, base OperationBody, projection Digest) (Digest, error) {
	if writer == nil || writer.effects == nil || projection.IsZero() {
		return Digest{}, ErrInvalid
	}
	chain := []byte("writer-admission-chain/v2\x00")
	for _, effect := range writerAdmissionEffects {
		if effect == EffectCapabilityIssue {
			break
		}
		request := writerAdmissionRequest(writer, base, projection, effect)
		requestDigest, err := request.Digest()
		record, recordOK := writer.effects.recordForRequestLocked(request)
		witness, witnessOK := writer.effects.completionWitnesses[request.key()]
		if err != nil || !recordOK || record.State != MutationCompleted || !witnessOK || record.Result.IsZero() || witness.ResultSHA256 != record.Result || witness.RequestSHA256 != requestDigest {
			return Digest{}, ErrForkNotAuthorized
		}
		chain = append(chain, []byte(string(effect)+"\x00"+requestDigest.String()+"\x00"+record.Result.String()+"\x00"+witness.Digest().String()+"\x00")...)
	}
	return HashBytes(chain), nil
}

// WriterAdmission models every pre-marker provider side effect through the
// same issue-once/status-only authority. It does not perform real provider
// work and is intentionally usable only with ProtocolCrashOracle.
type WriterAdmission struct {
	writer           *WriterProtocol
	provider         WriterProviderEffects
	base             OperationBody
	projection       PreOperationProjection
	projectionDigest Digest
}

func writerAdmissionRequest(writer *WriterProtocol, base OperationBody, projection Digest, effect Effect) MutationRequest {
	providerIdentity, providerTarget, providerConfig := Digest{}, Digest{}, Digest{}
	if writer != nil && writer.admissionProvider != nil {
		providerIdentity, providerTarget, providerConfig = writer.admissionProvider.IdentitySHA256(), writer.admissionProvider.TargetSHA256(), writer.admissionProvider.ConfigSHA256()
	}
	parameter := "admission-effect/v2\x00" + projection.String() + "\x00" + string(effect) + "\x00" + providerIdentity.String() + "\x00" + providerTarget.String() + "\x00" + providerConfig.String()
	return derivedEffectRequest(base, projection, effect, HashString(parameter))
}

func NewWriterAdmission(writer *WriterProtocol, base OperationBody, projection PreOperationProjection, provider WriterProviderEffects) (*WriterAdmission, error) {
	if writer == nil || provider == nil || !provider.IsTestOracleOnly() || provider.IdentitySHA256().IsZero() || provider.TargetSHA256().IsZero() || provider.ConfigSHA256().IsZero() || provider.TargetSHA256() != writer.broker.target {
		return nil, ErrInvalid
	}
	if writer.admissionProvider != nil && (writer.admissionProvider.IdentitySHA256() != provider.IdentitySHA256() || writer.admissionProvider.TargetSHA256() != provider.TargetSHA256() || writer.admissionProvider.ConfigSHA256() != provider.ConfigSHA256()) {
		return nil, ErrQuarantined
	}
	writer.admissionProvider = provider
	projectionDigest, err := projection.Digest()
	if err != nil {
		return nil, err
	}
	if err := writer.effects.BindProjection(base, projectionDigest); err != nil {
		return nil, err
	}
	return &WriterAdmission{writer: writer, provider: provider, base: base, projection: projection, projectionDigest: projectionDigest}, nil
}

func (a *WriterAdmission) request(effect Effect) MutationRequest {
	return writerAdmissionRequest(a.writer, a.base, a.projectionDigest, effect)
}

func (a *WriterAdmission) predecessorCompleteLocked(effect Effect) bool {
	for index, candidate := range writerAdmissionEffects {
		if candidate == effect {
			if index == 0 {
				return true
			}
			record, ok := a.writer.effects.recordForRequestLocked(a.request(writerAdmissionEffects[index-1]))
			return ok && record.State == MutationCompleted
		}
	}
	return false
}

func (a *WriterAdmission) Apply(ctx context.Context, effect Effect, auth AuthEnvelope, fault FaultMode) error {
	if a == nil || a.writer == nil || a.writer.effects == nil {
		return ErrInvalid
	}
	if !writerAdmissionEffectAllowed(effect) {
		return a.writer.effects.rejectAfterAuth(ctx, auth, ErrInvalid)
	}
	request := a.request(effect)
	authorization, authErr := auth.Digest()
	if authErr != nil {
		return authErr
	}
	chain := Digest{}
	_, err := a.writer.effects.authorizeAndConsumeChecked(ctx, request, auth, fault, Digest{}, func() error {
		if !a.predecessorCompleteLocked(effect) {
			return ErrForkNotAuthorized
		}
		if effect == EffectCapabilityIssue {
			var chainErr error
			chain, chainErr = writerAdmissionChainDigestLocked(a.writer, a.base, a.projectionDigest)
			return chainErr
		}
		return nil
	}, func() (Digest, error) {
		if effect == EffectCapabilityIssue {
			capability, err := issueBrokerCapability(a.writer.authority.privateKey, WriterBrokerRole, a.base,
				a.writer.authority.unit.IdentityDigest(), a.writer.broker.unit.IdentityDigest(), a.writer.broker.mtlsPeer, a.writer.broker.target,
				chain, authorization, a.writer.effects.now(), auth.NotBefore, auth.ExpiresAt)
			if err != nil {
				return Digest{}, err
			}
			return a.writer.effects.registerCapabilityLocked(request, capability)
		}
		return a.provider.ApplyConfigured(effect)
	})
	return err
}

func (a *WriterAdmission) Reconcile(ctx context.Context, effect Effect, auth AuthEnvelope) error {
	if a == nil || a.writer == nil || a.writer.effects == nil {
		return ErrInvalid
	}
	if !writerAdmissionEffectAllowed(effect) {
		return a.writer.effects.rejectAfterAuth(ctx, auth, ErrInvalid)
	}
	request := a.request(effect)
	_, err := a.writer.effects.reconcileStatusChecked(ctx, request, auth, func() error {
		if !a.predecessorCompleteLocked(effect) {
			return ErrForkNotAuthorized
		}
		return nil
	}, func() ([]Digest, error) {
		if effect == EffectCapabilityIssue {
			capability, ok := a.writer.effects.capabilityForRequestLocked(a.request(effect))
			if !ok {
				return nil, nil
			}
			return []Digest{capability.Digest()}, nil
		}
		return a.provider.ObserveConfigured(effect)
	})
	return err
}

func (a *WriterAdmission) Ready() bool {
	for _, effect := range writerAdmissionEffects {
		if !a.writer.effects.Completed(a.request(effect)) {
			return false
		}
	}
	return true
}

func NewWriterProtocol(boundary *ClosedBoundary, effects *ProtocolCrashOracle, authority *WriterAuthority, broker *WriterBroker) (*WriterProtocol, error) {
	if effects == nil || authority == nil || broker == nil {
		return nil, ErrInvalid
	}
	public := authority.privateKey.Public().(ed25519.PublicKey)
	if err := boundary.validateWriter(authority.unit, broker.unit, public); err != nil {
		return nil, err
	}
	if !effects.validInstance() || effects.identity != boundary.writerStore {
		return nil, ErrRoleIsolation
	}
	if err := broker.bindAuthority(authority.unit.IdentityDigest(), public); err != nil {
		return nil, err
	}
	clockBinder, ok := broker.sealer.(interface{ bindClock(func() time.Time) error })
	if !ok || clockBinder.bindClock(effects.now) != nil {
		return nil, ErrRoleIsolation
	}
	broker.now = effects.now
	if err := effects.bindSigner(WriterAuthorityRole, authority.privateKey, boundary.writerLedger, boundary.writerRoot); err != nil {
		return nil, err
	}
	return &WriterProtocol{boundary: boundary, effects: effects, authority: authority, broker: broker}, nil
}

func (w *WriterProtocol) capability(body OperationBody) (BrokerCapability, error) {
	projection, ok := w.effects.projectionFor(body)
	if !ok {
		return BrokerCapability{}, ErrForkNotAuthorized
	}
	capability, ok := w.effects.capabilityForRequest(writerAdmissionRequest(w, body, projection, EffectCapabilityIssue))
	if !ok {
		return BrokerCapability{}, ErrForkNotAuthorized
	}
	chain, chainErr := writerAdmissionChainDigest(w, body, projection)
	if chainErr != nil {
		return BrokerCapability{}, chainErr
	}
	if err := verifyBrokerCapability(w.authority.privateKey.Public().(ed25519.PublicKey), capability, WriterBrokerRole, body,
		w.authority.unit.IdentityDigest(), w.broker.unit.IdentityDigest(), w.broker.mtlsPeer, w.broker.target, chain, w.effects.now()); err != nil {
		return BrokerCapability{}, err
	}
	return capability, nil
}

func (w *WriterProtocol) capabilityLocked(body OperationBody) (BrokerCapability, error) {
	projection, ok := w.effects.projectionForLocked(body)
	if !ok {
		return BrokerCapability{}, ErrForkNotAuthorized
	}
	for _, effect := range writerAdmissionEffects {
		record, complete := w.effects.recordForRequestLocked(writerAdmissionRequest(w, body, projection, effect))
		if !complete || record.State != MutationCompleted {
			return BrokerCapability{}, ErrForkNotAuthorized
		}
	}
	request := writerAdmissionRequest(w, body, projection, EffectCapabilityIssue)
	capability, ok := w.effects.capabilityForRequestLocked(request)
	if !ok {
		return BrokerCapability{}, ErrForkNotAuthorized
	}
	chain, err := writerAdmissionChainDigestLocked(w, body, projection)
	if err != nil {
		return BrokerCapability{}, err
	}
	if err := verifyBrokerCapability(w.authority.privateKey.Public().(ed25519.PublicKey), capability, WriterBrokerRole, body,
		w.authority.unit.IdentityDigest(), w.broker.unit.IdentityDigest(), w.broker.mtlsPeer, w.broker.target, chain, w.effects.now()); err != nil {
		return BrokerCapability{}, err
	}
	issued, ok := w.effects.recordForRequestLocked(request)
	if !ok || issued.State != MutationCompleted || issued.Result != capability.Digest() || w.effects.revokedCapabilities[capability.Digest()] {
		return BrokerCapability{}, ErrQuarantined
	}
	return capability, nil
}

func markerRequest(body OperationBody, target Digest) MutationRequest {
	bodyDigest, _ := body.Digest()
	return MutationRequest{Operation: body, Effect: EffectMarkerCAS, ParametersDigest: HashString("fixed-marker-cas/v2\x00" + bodyDigest.String() + "\x00" + HashString(FixedMarkerKey).String() + "\x00" + target.String())}
}

func (w *WriterProtocol) markerApply(body OperationBody, capability *BrokerCapability) func() (Digest, error) {
	return func() (Digest, error) {
		if capability == nil || w.effects.revokedCapabilities[capability.Digest()] {
			return Digest{}, ErrQuarantined
		}
		bodyDigest, _ := body.Digest()
		token, ok := w.effects.sealedMarkerTokenLocked(bodyDigest)
		if !ok {
			return Digest{}, ErrInvalid
		}
		defer token.erase()
		marker, casCompletedAt, readCompletedAt, err := w.broker.commitPrepared(body, *capability, token)
		if err != nil || marker != token.markerSHA256 {
			if err != nil {
				return Digest{}, err
			}
			return Digest{}, ErrMarkerMismatch
		}
		receipt, receiptErr := w.receipt(body, marker, *capability, casCompletedAt, readCompletedAt)
		if receiptErr != nil {
			return Digest{}, receiptErr
		}
		return w.effects.registerPreparedWriterReceiptLocked(markerRequest(body, w.broker.target), receipt)
	}
}

func (w *WriterProtocol) markerPrepare(body OperationBody, capability *BrokerCapability) func() error {
	return func() error {
		if capability == nil {
			return ErrInvalid
		}
		marker, token, err := w.broker.prepareMarker(body, *capability)
		if err != nil {
			return err
		}
		defer token.erase()
		if marker != token.markerSHA256 {
			return ErrMarkerMismatch
		}
		bodyDigest, _ := body.Digest()
		if err := w.effects.storeSealedMarkerTokenLocked(bodyDigest, token); err != nil {
			return err
		}
		return nil
	}
}

func (w *WriterProtocol) receipt(body OperationBody, marker Digest, capability BrokerCapability, casCompletedAt, readCompletedAt time.Time) (WriterReceipt, error) {
	bodyDigest, err := body.Digest()
	if err != nil || marker.IsZero() || capability.Digest().IsZero() || capability.AdmissionChainSHA256.IsZero() ||
		casCompletedAt.Before(capability.AuthorizationIssuedAtUTC) || casCompletedAt.Before(capability.AuthorizationNotBeforeUTC) || !casCompletedAt.Before(capability.AuthorizationNotAfter) ||
		readCompletedAt.Before(casCompletedAt) || !readCompletedAt.Before(capability.AuthorizationNotAfter) {
		return WriterReceipt{}, ErrInvalid
	}
	r := WriterReceipt{Version: ProtocolVersion, OperationID: body.OperationID, Generation: body.Generation, Phase: body.Phase,
		OperationBodySHA256: bodyDigest, MarkerSHA256: marker, FixedKeySHA256: HashString(FixedMarkerKey), SourceTargetSHA256: w.broker.target,
		CapabilitySHA256: capability.Digest(), AdmissionChainSHA256: capability.AdmissionChainSHA256,
		AuthoritySHA256: w.authority.unit.IdentityDigest(), BrokerSHA256: w.broker.unit.IdentityDigest(),
		LedgerSHA256: w.boundary.writerLedger, RootSHA256: w.boundary.writerRoot, BoundarySHA256: w.boundary.digest,
		AuthorizationIssuedAtUTC: capability.AuthorizationIssuedAtUTC, AuthorizationNotBeforeUTC: capability.AuthorizationNotBeforeUTC,
		AuthorizationNotAfterUTC: capability.AuthorizationNotAfter,
		MarkerCASCompletedAtUTC:  casCompletedAt, SourceReadCompletedAtUTC: readCompletedAt,
		BrokerActionAtUTC: readCompletedAt}
	r.Signature = ed25519.Sign(w.authority.privateKey, r.payload())
	return r, nil
}

func (w *WriterProtocol) GenerateCommitAndReadback(ctx context.Context, body OperationBody, auth AuthEnvelope, fault FaultMode) (WriterReceipt, error) {
	if w == nil || w.effects == nil || w.broker == nil {
		return WriterReceipt{}, ErrRoleIsolation
	}
	if auth.Challenge.validateFor(WriterAuthorityRole, w.boundary.writerRoot) != nil {
		return WriterReceipt{}, w.effects.rejectAfterAuth(ctx, auth, ErrInvalid)
	}
	request := markerRequest(body, w.broker.target)
	var capability BrokerCapability
	receiptDigest, err := w.effects.authorizePreparedAndConsumeChecked(ctx, request, auth, fault, Digest{}, func() error {
		var capabilityErr error
		capability, capabilityErr = w.capabilityLocked(body)
		return capabilityErr
	},
		w.markerPrepare(body, &capability), w.markerApply(body, &capability))
	if err != nil {
		return WriterReceipt{}, err
	}
	receipt, ok := w.effects.writerReceiptForRequest(request)
	if !ok || writerReceiptDigest(receipt) != receiptDigest {
		return WriterReceipt{}, ErrQuarantined
	}
	return receipt, nil
}

func (w *WriterProtocol) ReconcileCommit(ctx context.Context, body OperationBody, auth AuthEnvelope) (WriterReceipt, error) {
	if w == nil || w.effects == nil || w.broker == nil {
		return WriterReceipt{}, ErrRoleIsolation
	}
	if auth.Challenge.validateFor(WriterAuthorityRole, w.boundary.writerRoot) != nil {
		return WriterReceipt{}, w.effects.rejectAfterAuth(ctx, auth, ErrInvalid)
	}
	request := markerRequest(body, w.broker.target)
	var capability BrokerCapability
	receiptDigest, err := w.effects.reconcilePreparedStatusChecked(ctx, request, auth, func() error {
		var capabilityErr error
		capability, capabilityErr = w.capabilityLocked(body)
		return capabilityErr
	}, func() ([]Digest, error) {
		bodyDigest, digestErr := body.Digest()
		if digestErr != nil {
			return nil, digestErr
		}
		token, ok := w.effects.sealedMarkerTokenLocked(bodyDigest)
		if !ok {
			return nil, nil
		}
		defer token.erase()
		observed, markerExists, predecessorUnchanged, _, observeErr := w.broker.reconcileReadback(body, capability, token)
		if observeErr != nil {
			return nil, observeErr
		}
		if predecessorUnchanged {
			return []Digest{markerContinuationCandidate}, nil
		}
		if !markerExists {
			return nil, nil
		}
		receipt, ok := w.effects.writerReceiptForRequestLocked(request)
		if !ok || receipt.MarkerSHA256 != observed {
			return nil, ErrQuarantined
		}
		return []Digest{writerReceiptDigest(receipt)}, nil
	}, w.markerApply(body, &capability))
	if err != nil {
		if errors.Is(err, ErrRoleIsolation) {
			w.effects.quarantineMarkerForCleanup(request, HashString("marker-abort-reason/v2\x00reconcile-chronology-rejected"))
		}
		return WriterReceipt{}, err
	}
	receipt, ok := w.effects.writerReceiptForRequest(request)
	if !ok || writerReceiptDigest(receipt) != receiptDigest {
		return WriterReceipt{}, ErrQuarantined
	}
	return receipt, nil
}

func markerAbortRequest(body OperationBody, sealer Digest) MutationRequest {
	bodyDigest, _ := body.Digest()
	abortBody := OperationBody{
		Version: ProtocolVersion, OperationID: "synthetic-marker-abort-" + bodyDigest.String()[:20],
		Generation: body.Generation, Phase: body.Phase,
		ConfigDigest: HashString("marker-abort-config/v2\x00" + bodyDigest.String() + "\x00" + sealer.String()),
	}
	parameters := HashString("marker-abort-parameters/v2\x00" + bodyDigest.String() + "\x00" + sealer.String())
	return MutationRequest{Operation: abortBody, Effect: EffectWrappingKeyRevoke, ParametersDigest: parameters}
}

func markerAbortTerminalBinding(body Digest, sealer Digest) Digest {
	return HashString("marker-abort-terminal-binding/v2\x00" + body.String() + "\x00" + sealer.String())
}

// MarkerAbortReceipt is portable completed cleanup evidence. The embedded
// witness alone is merely pre-effect authorization; the completion witness is
// mandatory proof that the exact revocation request reached durable success.
type MarkerAbortReceipt struct {
	Witness           MarkerAbortWitness
	RevocationStatus  MarkerSealerRevocationStatus
	CompletionWitness CompletionWitness
}

func VerifyMarkerAbortReceipt(boundary *ClosedBoundary, body OperationBody, receipt MarkerAbortReceipt) error {
	if boundary == nil {
		return ErrCleanupRequired
	}
	bodyDigest, err := body.Digest()
	request := markerAbortRequest(body, receipt.Witness.SealerSHA256)
	requestDigest, requestErr := request.Digest()
	_, timeErr := canonicalTime(receipt.Witness.AuthorizedAt)
	expectedMarkerRequest, markerRequestErr := markerRequest(body, receipt.Witness.SourceTargetSHA256).Digest()
	if err != nil || requestErr != nil || markerRequestErr != nil || timeErr != nil || bodyDigest != receipt.Witness.OperationBodySHA256 ||
		receipt.Witness.Version != ProtocolVersion || receipt.Witness.AbortRequestSHA256 != requestDigest ||
		receipt.Witness.MarkerRequestSHA256 != expectedMarkerRequest || receipt.Witness.MarkerSHA256.IsZero() || receipt.Witness.SourceTargetSHA256.IsZero() ||
		receipt.Witness.CapabilitySHA256.IsZero() || receipt.Witness.SealerSHA256.IsZero() || receipt.Witness.ReasonSHA256.IsZero() ||
		receipt.Witness.LedgerSHA256 != boundary.writerLedger || receipt.Witness.RootSHA256 != boundary.writerRoot ||
		receipt.Witness.OracleSHA256 != boundary.writerStore || len(receipt.Witness.Signature) != ed25519.SignatureSize ||
		!ed25519.Verify(boundary.writerPublic, receipt.Witness.payload(), receipt.Witness.Signature) {
		return ErrCleanupRequired
	}
	terminalResult, terminalErr := markerAbortTerminalResult(receipt.Witness, receipt.RevocationStatus)
	if terminalErr != nil || receipt.CompletionWitness.CompletedAt.Before(receipt.RevocationStatus.ObservedAt) {
		return ErrCleanupRequired
	}
	if err := verifyCompletionWitness(boundary.writerPublic, WriterAuthorityRole, boundary.writerLedger, boundary.writerRoot,
		boundary.writerStore, request, terminalResult, markerAbortTerminalBinding(bodyDigest, receipt.Witness.SealerSHA256), receipt.CompletionWitness); err != nil {
		return ErrCleanupRequired
	}
	return nil
}

// MarkerAbortController is a cleanup-only reconstruction path. It carries no
// source store, writer broker, marker plaintext, or capability-issuing API and
// remains constructible after the sealer reports terminal revocation.
type MarkerAbortController struct {
	boundary *ClosedBoundary
	effects  *ProtocolCrashOracle
	sealer   MarkerSealer
}

func NewMarkerAbortController(boundary *ClosedBoundary, effects *ProtocolCrashOracle, sealer MarkerSealer) (*MarkerAbortController, error) {
	if boundary == nil || effects == nil || !effects.validInstance() || effects.identity != boundary.writerStore ||
		sealer == nil || !sealer.IsTestOracleOnly() || sealer.BindingSHA256().IsZero() {
		return nil, ErrRoleIsolation
	}
	clockBinder, ok := sealer.(interface{ bindClock(func() time.Time) error })
	if !ok || clockBinder.bindClock(effects.now) != nil {
		return nil, ErrRoleIsolation
	}
	return &MarkerAbortController{boundary: boundary, effects: effects, sealer: sealer}, nil
}

func (c *MarkerAbortController) markerAbortContextLocked(body OperationBody) (pendingMarkerAbort, error) {
	if c == nil || c.effects == nil || c.sealer == nil {
		return pendingMarkerAbort{}, ErrCleanupRequired
	}
	bodyDigest, err := body.Digest()
	if err != nil {
		return pendingMarkerAbort{}, err
	}
	pending, ok := c.effects.pendingMarkerAborts[bodyDigest]
	if !ok {
		if witness, witnessed := c.effects.markerAbortWitnesses[bodyDigest]; witnessed {
			pending = pendingMarkerAbort{
				MarkerRequestSHA256: witness.MarkerRequestSHA256, OperationBodySHA256: witness.OperationBodySHA256,
				MarkerSHA256: witness.MarkerSHA256, SourceTargetSHA256: witness.SourceTargetSHA256,
				CapabilitySHA256: witness.CapabilitySHA256, SealerSHA256: witness.SealerSHA256, ReasonSHA256: witness.ReasonSHA256,
			}
			ok = true
		}
	}
	if !ok || pending.OperationBodySHA256 != bodyDigest || pending.SealerSHA256 != c.sealer.BindingSHA256() {
		return pendingMarkerAbort{}, ErrCleanupRequired
	}
	expectedMarkerRequest, digestErr := markerRequest(body, pending.SourceTargetSHA256).Digest()
	if digestErr != nil || pending.MarkerRequestSHA256 != expectedMarkerRequest {
		return pendingMarkerAbort{}, ErrCleanupRequired
	}
	return pending, nil
}

func (c *MarkerAbortController) completedAbortReceipt(body OperationBody, pending pendingMarkerAbort, result Digest, erase bool) (MarkerAbortReceipt, error) {
	bodyDigest, bodyErr := body.Digest()
	status, statusErr := markerSealerTerminalStatus(c.sealer)
	if bodyErr != nil || statusErr != nil {
		return MarkerAbortReceipt{}, ErrCleanupRequired
	}
	request := markerAbortRequest(body, c.sealer.BindingSHA256())
	c.effects.mu.Lock()
	witness, ok := c.effects.preparedMarkerAborts[request.key()]
	witness = cloneMarkerAbortWitness(witness)
	completion, completionOK := c.effects.completionWitnesses[request.key()]
	completion = cloneCompletionWitness(completion)
	if erase {
		finalized, finalizeErr := c.effects.finalizeMarkerAbortErasureLocked(request, pending, status)
		if finalizeErr != nil || finalized != result {
			c.effects.mu.Unlock()
			return MarkerAbortReceipt{}, ErrCleanupRequired
		}
	}
	c.effects.mu.Unlock()
	if !ok || !completionOK {
		return MarkerAbortReceipt{}, ErrCleanupRequired
	}
	if erase {
		witness, ok = c.effects.markerAbortWitness(bodyDigest)
	}
	expectedResult, resultErr := markerAbortTerminalResult(witness, status)
	if !ok || resultErr != nil || expectedResult != result {
		return MarkerAbortReceipt{}, ErrCleanupRequired
	}
	receipt := MarkerAbortReceipt{Witness: witness, RevocationStatus: status, CompletionWitness: completion}
	if err := VerifyMarkerAbortReceipt(c.boundary, body, receipt); err != nil {
		return MarkerAbortReceipt{}, err
	}
	return receipt, nil
}

// AbortCommit is the distinct issue-once cleanup operation for an unresolved
// prepared marker. It uses a fresh authentication envelope and never retries
// wrapping-key revocation after an ambiguous outcome.
func (c *MarkerAbortController) AbortCommit(ctx context.Context, body OperationBody, auth AuthEnvelope, fault FaultMode) (MarkerAbortReceipt, error) {
	if c == nil || c.effects == nil || c.sealer == nil {
		return MarkerAbortReceipt{}, ErrCleanupRequired
	}
	request := markerAbortRequest(body, c.sealer.BindingSHA256())
	var pending pendingMarkerAbort
	bodyDigest, bodyErr := body.Digest()
	if bodyErr != nil {
		return MarkerAbortReceipt{}, c.effects.rejectAfterAuth(ctx, auth, bodyErr)
	}
	result, err := c.effects.authorizePreparedAndConsumeChecked(ctx, request, auth, fault, markerAbortTerminalBinding(bodyDigest, c.sealer.BindingSHA256()), func() error {
		var contextErr error
		pending, contextErr = c.markerAbortContextLocked(body)
		return contextErr
	},
		func() error {
			_, prepareErr := c.effects.registerPreparedMarkerAbortLocked(request, pending)
			return prepareErr
		},
		func() (Digest, error) {
			if !markerSealerActive(c.sealer) {
				return Digest{}, ErrQuarantined
			}
			if revokeErr := c.sealer.Revoke(); revokeErr != nil {
				return Digest{}, revokeErr
			}
			status, statusErr := markerSealerTerminalStatus(c.sealer)
			if statusErr != nil {
				return Digest{}, statusErr
			}
			witness, ok := c.effects.preparedMarkerAborts[request.key()]
			if !ok {
				return Digest{}, ErrCleanupRequired
			}
			return markerAbortTerminalResult(witness, status)
		})
	if err != nil {
		return MarkerAbortReceipt{}, err
	}
	return c.completedAbortReceipt(body, pending, result, true)
}

// ReconcileAbortCommit is status-only. It observes whether the already-issued
// wrapping-key revocation took effect and never invokes Revoke itself.
func (c *MarkerAbortController) ReconcileAbortCommit(ctx context.Context, body OperationBody, auth AuthEnvelope) (MarkerAbortReceipt, error) {
	if c == nil || c.effects == nil || c.sealer == nil {
		return MarkerAbortReceipt{}, ErrCleanupRequired
	}
	request := markerAbortRequest(body, c.sealer.BindingSHA256())
	var pending pendingMarkerAbort
	result, err := c.effects.reconcileStatusChecked(ctx, request, auth, func() error {
		var contextErr error
		pending, contextErr = c.markerAbortContextLocked(body)
		return contextErr
	}, func() ([]Digest, error) {
		status, statusErr := markerSealerTerminalStatus(c.sealer)
		if errors.Is(statusErr, ErrNotObserved) {
			return nil, nil
		} else if statusErr != nil {
			return nil, statusErr
		}
		witness, ok := c.effects.preparedMarkerAborts[request.key()]
		if !ok {
			return nil, ErrCleanupRequired
		}
		witness = cloneMarkerAbortWitness(witness)
		digest, digestErr := markerAbortTerminalResult(witness, status)
		if digestErr != nil {
			return nil, digestErr
		}
		return []Digest{digest}, nil
	})
	if err != nil {
		if errors.Is(err, ErrNotObserved) {
			return MarkerAbortReceipt{}, ErrCleanupRequired
		}
		return MarkerAbortReceipt{}, err
	}
	// Reconciliation is strictly status-only: it may complete the exact durable
	// witness, but it never erases the sealed token or changes sealer state. A
	// subsequent authenticated AbortCommit call performs the idempotent local
	// erasure without reissuing Revoke.
	return c.completedAbortReceipt(body, pending, result, false)
}

func (w *WriterProtocol) abortController() (*MarkerAbortController, error) {
	if w == nil || w.broker == nil {
		return nil, ErrCleanupRequired
	}
	return NewMarkerAbortController(w.boundary, w.effects, w.broker.sealer)
}

func (w *WriterProtocol) AbortCommit(ctx context.Context, body OperationBody, auth AuthEnvelope, fault FaultMode) (MarkerAbortReceipt, error) {
	controller, err := w.abortController()
	if err != nil {
		return MarkerAbortReceipt{}, err
	}
	return controller.AbortCommit(ctx, body, auth, fault)
}

func (w *WriterProtocol) ReconcileAbortCommit(ctx context.Context, body OperationBody, auth AuthEnvelope) (MarkerAbortReceipt, error) {
	controller, err := w.abortController()
	if err != nil {
		return MarkerAbortReceipt{}, err
	}
	return controller.ReconcileAbortCommit(ctx, body, auth)
}

func (w *WriterProtocol) SealedMarkerPresent(body OperationBody) bool {
	d, err := body.Digest()
	return err == nil && w.effects.sealedMarkerTokenPresent(d)
}

// ObservationClock permits deterministic separation tests without sleeping.
type ObservationClock interface {
	Now() time.Time
	Wait(time.Duration)
}

// RecoveryAdmissionAuthorization is the opaque output of the durable
// writer-side fork controller. It contains hash-only claims and is the only
// value the observer authority accepts when minting a RecoveryAdmission.
// It carries no writer receipt, marker hash, marker preimage, or signing key.
type RecoveryAdmissionAuthorization struct {
	version, operationID, phase string
	generation                  uint64
	operationBodySHA256         Digest
	projectionSHA256            Digest
	terminalBindingSHA256       Digest
	forkRequestSHA256           Digest
	forkResultSHA256            Digest
	forkProofSHA256             Digest
	sourceTargetSHA256          Digest
	recoveryTargetSHA256        Digest
	forkProviderSHA256          Digest
	writerAuthoritySHA256       Digest
	writerLedgerSHA256          Digest
	writerRootSHA256            Digest
	writerOracleSHA256          Digest
	observerAuthoritySHA256     Digest
	observerBrokerSHA256        Digest
	observerLedgerSHA256        Digest
	observerRootSHA256          Digest
	observerStoreSHA256         Digest
	boundarySHA256              Digest
	forkObservationSHA256       Digest
	issuedAt                    time.Time
	authorizationNotAfter       time.Time
	writerSignature             []byte
	forkCompletionWitness       CompletionWitness
	forkObservation             ForkObservationReceipt
	writerCompletionWitness     CompletionWitness
	publicationRequest          MutationRequest
}

func (a RecoveryAdmissionAuthorization) payload() []byte {
	return []byte("recovery-admission-authorization/v2\x00" + a.version + "\x00" + a.operationID + "\x00" +
		strconv.FormatUint(a.generation, 10) + "\x00" + a.phase + "\x00" + a.operationBodySHA256.String() + "\x00" + a.projectionSHA256.String() + "\x00" +
		a.terminalBindingSHA256.String() + "\x00" + a.forkRequestSHA256.String() + "\x00" + a.forkResultSHA256.String() + "\x00" +
		a.forkProofSHA256.String() + "\x00" + a.sourceTargetSHA256.String() + "\x00" + a.recoveryTargetSHA256.String() + "\x00" + a.forkProviderSHA256.String() + "\x00" + a.writerAuthoritySHA256.String() + "\x00" + a.writerLedgerSHA256.String() + "\x00" +
		a.writerRootSHA256.String() + "\x00" + a.writerOracleSHA256.String() + "\x00" +
		a.observerAuthoritySHA256.String() + "\x00" + a.observerBrokerSHA256.String() + "\x00" +
		a.observerLedgerSHA256.String() + "\x00" + a.observerRootSHA256.String() + "\x00" + a.observerStoreSHA256.String() + "\x00" + a.boundarySHA256.String() + "\x00" +
		a.forkCompletionWitness.Digest().String() + "\x00" + a.forkObservationSHA256.String() + "\x00" + mustCanonicalTime(a.issuedAt) + "\x00" + mustCanonicalTime(a.authorizationNotAfter))
}

func mustCanonicalTime(value time.Time) string { encoded, _ := canonicalTime(value); return encoded }

func (a RecoveryAdmissionAuthorization) ClaimsDigest() Digest { return HashBytes(a.payload()) }

func (a RecoveryAdmissionAuthorization) signingPayload() []byte {
	requestDigest, _ := a.publicationRequest.Digest()
	return []byte("recovery-admission-authorization-publication/v2\x00" + a.ClaimsDigest().String() + "\x00" +
		requestDigest.String() + "\x00" + a.writerLedgerSHA256.String() + "\x00" + a.writerRootSHA256.String() + "\x00" + a.writerOracleSHA256.String())
}

func (a RecoveryAdmissionAuthorization) Digest() Digest {
	return HashBytes(append(a.signingPayload(), a.writerSignature...))
}

func (a RecoveryAdmissionAuthorization) durableBinding() recoveryAdmissionBinding {
	return recoveryAdmissionBinding{
		OperationBody: a.operationBodySHA256, TerminalBinding: a.terminalBindingSHA256,
		ForkRequest: a.forkRequestSHA256, ForkResult: a.forkResultSHA256,
		ForkProof: a.forkProofSHA256, RecoveryTarget: a.recoveryTargetSHA256, ForkProvider: a.forkProviderSHA256, Payload: a.Digest(),
	}
}

func verifyRecoveryAdmissionAuthorizationClaims(boundary *ClosedBoundary, authorization RecoveryAdmissionAuthorization) error {
	expectedRequest, requestErr := recoveryAdmissionRequestFromClaims(authorization.operationID, authorization.generation, authorization.phase,
		authorization.projectionSHA256, authorization.forkRequestSHA256)
	expectedRequestDigest, expectedDigestErr := expectedRequest.Digest()
	actualRequestDigest, actualDigestErr := authorization.publicationRequest.Digest()
	issued, issuedErr := canonicalTime(authorization.issuedAt)
	notAfter, notAfterErr := canonicalTime(authorization.authorizationNotAfter)
	forkWitness := authorization.forkCompletionWitness
	if boundary == nil || authorization.version != ProtocolVersion || !validSyntheticToken(authorization.operationID) ||
		authorization.generation == 0 || !validPhase(authorization.phase) || authorization.operationBodySHA256.IsZero() || authorization.projectionSHA256.IsZero() ||
		authorization.terminalBindingSHA256.IsZero() || authorization.forkRequestSHA256.IsZero() || authorization.forkResultSHA256.IsZero() || authorization.sourceTargetSHA256.IsZero() ||
		authorization.forkProofSHA256 != forkCompletionProof(authorization.terminalBindingSHA256, authorization.forkRequestSHA256, authorization.forkResultSHA256) || authorization.recoveryTargetSHA256.IsZero() || authorization.forkProviderSHA256.IsZero() ||
		authorization.writerAuthoritySHA256 != boundary.writerAuthority.IdentityDigest() ||
		authorization.writerLedgerSHA256 != boundary.writerLedger || authorization.writerRootSHA256 != boundary.writerRoot ||
		authorization.writerOracleSHA256 != boundary.writerStore ||
		authorization.observerAuthoritySHA256 != boundary.observerAuthority.IdentityDigest() ||
		authorization.observerBrokerSHA256 != boundary.observerBroker.IdentityDigest() ||
		authorization.observerLedgerSHA256 != boundary.observerLedger || authorization.observerRootSHA256 != boundary.observerRoot ||
		authorization.observerStoreSHA256 != boundary.observerStore ||
		authorization.boundarySHA256 != boundary.digest ||
		authorization.forkObservationSHA256 != authorization.forkObservation.Digest() ||
		verifyForkObservation(boundary, authorization.forkObservation, authorization.sourceTargetSHA256, authorization.recoveryTargetSHA256, authorization.forkProviderSHA256, authorization.forkResultSHA256) != nil ||
		issuedErr != nil || notAfterErr != nil || issued == "" || notAfter == "" || !authorization.issuedAt.Before(authorization.authorizationNotAfter) ||
		forkWitness.Version != ProtocolVersion || forkWitness.Role != WriterAuthorityRole || forkWitness.Effect != EffectForkPOST || forkWitness.State != MutationCompleted ||
		forkWitness.RequestSHA256 != authorization.forkRequestSHA256 || forkWitness.ResultSHA256 != authorization.forkResultSHA256 || forkWitness.TerminalSHA256 != authorization.terminalBindingSHA256 ||
		forkWitness.LedgerSHA256 != boundary.writerLedger || forkWitness.RootSHA256 != boundary.writerRoot || forkWitness.OracleSHA256 != boundary.writerStore ||
		forkWitness.Sequence == 0 || len(forkWitness.Signature) != ed25519.SignatureSize || !ed25519.Verify(boundary.writerPublic, forkWitness.payload(), forkWitness.Signature) ||
		authorization.issuedAt.Before(forkWitness.CompletedAt) || authorization.issuedAt.Before(authorization.forkObservation.ObservedAt) ||
		requestErr != nil || expectedDigestErr != nil || actualDigestErr != nil || actualRequestDigest != expectedRequestDigest {
		return ErrForkNotAuthorized
	}
	return nil
}

func verifyRecoveryAdmissionAuthorizationSignature(boundary *ClosedBoundary, authorization RecoveryAdmissionAuthorization) error {
	if err := verifyRecoveryAdmissionAuthorizationClaims(boundary, authorization); err != nil ||
		len(authorization.writerSignature) != ed25519.SignatureSize ||
		!ed25519.Verify(boundary.writerPublic, authorization.signingPayload(), authorization.writerSignature) {
		return ErrForkNotAuthorized
	}
	return nil
}

func verifyRecoveryAdmissionAuthorization(boundary *ClosedBoundary, authorization RecoveryAdmissionAuthorization) error {
	if err := verifyRecoveryAdmissionAuthorizationSignature(boundary, authorization); err != nil {
		return ErrForkNotAuthorized
	}
	if err := verifyCompletionWitness(boundary.writerPublic, WriterAuthorityRole, authorization.writerLedgerSHA256,
		authorization.writerRootSHA256, authorization.writerOracleSHA256, authorization.publicationRequest,
		authorization.Digest(), recoveryAuthorizationTerminal(authorization.terminalBindingSHA256, authorization.forkRequestSHA256), authorization.writerCompletionWitness); err != nil {
		return ErrForkNotAuthorized
	}
	return nil
}

func recoveryAuthorizationForResult(boundary *ClosedBoundary, oracle *ProtocolCrashOracle, result Digest) (RecoveryAdmissionAuthorization, error) {
	authorization, ok := oracle.recoveryAuthorization(result)
	if !ok || authorization.Digest() != result {
		return RecoveryAdmissionAuthorization{}, ErrQuarantined
	}
	witness, ok := oracle.completionWitness(authorization.publicationRequest)
	if !ok {
		return RecoveryAdmissionAuthorization{}, ErrQuarantined
	}
	authorization.writerCompletionWitness = witness
	if err := verifyRecoveryAdmissionAuthorization(boundary, authorization); err != nil {
		return RecoveryAdmissionAuthorization{}, err
	}
	return authorization, nil
}

// ObserverAdmissionRequest is constructed by the outer protected verifier
// only after it validates the writer publication. It is the sole input to the
// recovery domain and deliberately contains no writer key, root, signature,
// completion witness, publication request, marker hash, or marker preimage.
type ObserverAdmissionRequest struct {
	version, operationID, phase string
	generation                  uint64
	operationBodySHA256         Digest
	projectionSHA256            Digest
	terminalBindingSHA256       Digest
	forkRequestSHA256           Digest
	forkResultSHA256            Digest
	forkProofSHA256             Digest
	recoveryTargetSHA256        Digest
	forkProviderSHA256          Digest
	observerAuthoritySHA256     Digest
	observerBrokerSHA256        Digest
	observerLedgerSHA256        Digest
	observerRootSHA256          Digest
	observerStoreSHA256         Digest
	boundarySHA256              Digest
	outerEvidenceSHA256         Digest
	continuityBindingSHA256     Digest
	writerPublicationIssuedAt   time.Time
	writerPublicationNotAfter   time.Time
}

func (r ObserverAdmissionRequest) payload() []byte {
	return []byte("observer-admission-request/v2\x00" + r.version + "\x00" + r.operationID + "\x00" + strconv.FormatUint(r.generation, 10) + "\x00" + r.phase + "\x00" +
		r.operationBodySHA256.String() + "\x00" + r.projectionSHA256.String() + "\x00" + r.terminalBindingSHA256.String() + "\x00" +
		r.forkRequestSHA256.String() + "\x00" + r.forkResultSHA256.String() + "\x00" + r.forkProofSHA256.String() + "\x00" +
		r.recoveryTargetSHA256.String() + "\x00" + r.forkProviderSHA256.String() + "\x00" + r.observerAuthoritySHA256.String() + "\x00" +
		r.observerBrokerSHA256.String() + "\x00" + r.observerLedgerSHA256.String() + "\x00" + r.observerRootSHA256.String() + "\x00" +
		r.observerStoreSHA256.String() + "\x00" + r.boundarySHA256.String() + "\x00" + r.outerEvidenceSHA256.String() + "\x00" + r.continuityBindingSHA256.String() + "\x00" +
		mustCanonicalTime(r.writerPublicationIssuedAt) + "\x00" + mustCanonicalTime(r.writerPublicationNotAfter))
}

func (r ObserverAdmissionRequest) Digest() Digest { return HashBytes(r.payload()) }

func PrepareObserverAdmission(boundary *ClosedBoundary, authorization RecoveryAdmissionAuthorization) (ObserverAdmissionRequest, error) {
	if err := verifyRecoveryAdmissionAuthorization(boundary, authorization); err != nil {
		return ObserverAdmissionRequest{}, ErrForkNotAuthorized
	}
	writerPublication := authorization.Digest()
	binding := HashString("observer-admission-continuity/v2\x00" + writerPublication.String() + "\x00" +
		authorization.operationBodySHA256.String() + "\x00" + authorization.forkProofSHA256.String() + "\x00" + authorization.recoveryTargetSHA256.String())
	return ObserverAdmissionRequest{
		version: authorization.version, operationID: authorization.operationID, phase: authorization.phase, generation: authorization.generation,
		operationBodySHA256: authorization.operationBodySHA256, projectionSHA256: authorization.projectionSHA256,
		terminalBindingSHA256: authorization.terminalBindingSHA256, forkRequestSHA256: authorization.forkRequestSHA256,
		forkResultSHA256: authorization.forkResultSHA256, forkProofSHA256: authorization.forkProofSHA256,
		recoveryTargetSHA256: authorization.recoveryTargetSHA256, forkProviderSHA256: authorization.forkProviderSHA256,
		observerAuthoritySHA256: authorization.observerAuthoritySHA256, observerBrokerSHA256: authorization.observerBrokerSHA256,
		observerLedgerSHA256: authorization.observerLedgerSHA256, observerRootSHA256: authorization.observerRootSHA256,
		observerStoreSHA256: authorization.observerStoreSHA256, boundarySHA256: authorization.boundarySHA256,
		outerEvidenceSHA256: writerPublication, continuityBindingSHA256: binding,
		writerPublicationIssuedAt: authorization.issuedAt, writerPublicationNotAfter: authorization.authorizationNotAfter,
	}, nil
}

func validateObserverAdmissionRequest(boundary *ObserverBoundary, request ObserverAdmissionRequest) error {
	expectedBinding := HashString("observer-admission-continuity/v2\x00" + request.outerEvidenceSHA256.String() + "\x00" +
		request.operationBodySHA256.String() + "\x00" + request.forkProofSHA256.String() + "\x00" + request.recoveryTargetSHA256.String())
	_, issuedErr := canonicalTime(request.writerPublicationIssuedAt)
	_, notAfterErr := canonicalTime(request.writerPublicationNotAfter)
	if boundary == nil || request.version != ProtocolVersion || !validSyntheticToken(request.operationID) || request.generation == 0 || !validPhase(request.phase) ||
		request.operationBodySHA256.IsZero() || request.projectionSHA256.IsZero() || request.terminalBindingSHA256.IsZero() || request.forkRequestSHA256.IsZero() ||
		request.forkResultSHA256.IsZero() || request.forkProofSHA256 != forkCompletionProof(request.terminalBindingSHA256, request.forkRequestSHA256, request.forkResultSHA256) ||
		request.recoveryTargetSHA256.IsZero() || request.forkProviderSHA256.IsZero() || request.observerAuthoritySHA256 != boundary.authority.IdentityDigest() ||
		request.observerBrokerSHA256 != boundary.broker.IdentityDigest() || request.observerLedgerSHA256 != boundary.ledger || request.observerRootSHA256 != boundary.root ||
		request.observerStoreSHA256 != boundary.store || request.boundarySHA256 != boundary.closed || request.outerEvidenceSHA256.IsZero() ||
		request.continuityBindingSHA256.IsZero() || request.continuityBindingSHA256 != expectedBinding || issuedErr != nil || notAfterErr != nil ||
		!request.writerPublicationIssuedAt.Before(request.writerPublicationNotAfter) {
		return ErrForkNotAuthorized
	}
	return nil
}

// RecoveryAdmission is observer-root authority only. The outer verifier binds
// it to the separately supplied writer publication by the two shared digests.
type RecoveryAdmission struct {
	request                   ObserverAdmissionRequest
	observerOracleSHA256      Digest
	signature                 []byte
	observerCompletionWitness CompletionWitness
	publicationRequest        MutationRequest
	lifecycleAdmissionSHA256  Digest
	issuedAt                  time.Time
	authorizationNotAfter     time.Time
}

func (a RecoveryAdmission) payload() []byte {
	return append([]byte("recovery-admission/v3\x00"+a.request.Digest().String()+"\x00"+a.observerOracleSHA256.String()+"\x00"+
		a.lifecycleAdmissionSHA256.String()+"\x00"+mustCanonicalTime(a.issuedAt)+"\x00"+mustCanonicalTime(a.authorizationNotAfter)+"\x00"), a.request.payload()...)
}

func (a RecoveryAdmission) Digest() Digest { return HashBytes(append(a.payload(), a.signature...)) }

func (a RecoveryAdmission) clone() RecoveryAdmission {
	a.signature = append([]byte(nil), a.signature...)
	a.observerCompletionWitness = cloneCompletionWitness(a.observerCompletionWitness)
	return a
}

func (a RecoveryAdmission) durableBinding() recoveryAdmissionBinding {
	return recoveryAdmissionBinding{
		OperationBody: a.request.operationBodySHA256, TerminalBinding: a.request.outerEvidenceSHA256,
		ForkRequest: a.request.forkRequestSHA256, ForkResult: a.request.forkResultSHA256,
		ForkProof: a.request.forkProofSHA256, RecoveryTarget: a.request.recoveryTargetSHA256,
		ForkProvider: a.request.forkProviderSHA256, Payload: HashBytes(a.payload()),
	}
}

func forkCompletionProof(terminal, request, result Digest) Digest {
	return HashString("fork-completion/v2\x00" + terminal.String() + "\x00" + request.String() + "\x00" + result.String())
}

func verifyRecoveryAdmissionSignature(boundary *ObserverBoundary, admission RecoveryAdmission) error {
	expectedRequest, requestErr := recoveryAdmissionSignatureRequest(admission.request)
	expectedRequestDigest, expectedDigestErr := expectedRequest.Digest()
	actualRequestDigest, actualDigestErr := admission.publicationRequest.Digest()
	_, issuedErr := canonicalTime(admission.issuedAt)
	_, notAfterErr := canonicalTime(admission.authorizationNotAfter)
	if validateObserverAdmissionRequest(boundary, admission.request) != nil || admission.observerOracleSHA256 != admission.request.observerStoreSHA256 ||
		admission.lifecycleAdmissionSHA256.IsZero() || issuedErr != nil || notAfterErr != nil ||
		admission.issuedAt.Before(admission.request.writerPublicationIssuedAt) || !admission.issuedAt.Before(admission.authorizationNotAfter) ||
		admission.authorizationNotAfter.After(admission.request.writerPublicationNotAfter) ||
		requestErr != nil || expectedDigestErr != nil || actualDigestErr != nil || actualRequestDigest != expectedRequestDigest ||
		len(admission.signature) != ed25519.SignatureSize ||
		!ed25519.Verify(boundary.public, admission.payload(), admission.signature) {
		return ErrForkNotAuthorized
	}
	return nil
}

func verifyRecoveryAdmission(boundary *ObserverBoundary, admission RecoveryAdmission) error {
	if err := verifyRecoveryAdmissionSignature(boundary, admission); err != nil {
		return err
	}
	if admission.publicationRequest.Validate() != nil {
		return ErrForkNotAuthorized
	}
	if err := verifyCompletionWitness(boundary.public, ObserverAuthorityRole, admission.request.observerLedgerSHA256,
		admission.request.observerRootSHA256, admission.observerOracleSHA256, admission.publicationRequest,
		admission.Digest(), admission.request.outerEvidenceSHA256, admission.observerCompletionWitness); err != nil {
		return ErrForkNotAuthorized
	}
	if admission.observerCompletionWitness.CompletedAt.Before(admission.issuedAt) || !admission.observerCompletionWitness.CompletedAt.Before(admission.authorizationNotAfter) {
		return ErrForkNotAuthorized
	}
	return nil
}

type ObserverReceipt struct {
	Version, OperationID, Phase                                                string
	Generation                                                                 uint64
	OperationBodySHA256, MarkerSHA256, FixedKeySHA256, RecoveryAdmissionSHA256 Digest
	AuthoritySHA256, BrokerSHA256, LedgerSHA256, RootSHA256, BoundarySHA256    Digest
	CapabilitySHA256, AdmissionChainSHA256                                     Digest
	PublicationRequestSHA256                                                   Digest
	PublicationOracleSHA256                                                    Digest
	RecoveryReadCount                                                          uint8
	AuthorizationIssuedAtUTC, AuthorizationNotBeforeUTC                        time.Time
	AuthorizationNotAfterUTC, ReadOneAt, ReadTwoAt                             time.Time
	Signature                                                                  []byte
	CompletionWitness                                                          CompletionWitness
	publicationRequest                                                         MutationRequest
}

func canonicalTime(value time.Time) (string, error) {
	if value.IsZero() || value.Location() != time.UTC {
		return "", ErrInvalid
	}
	return value.Format(time.RFC3339Nano), nil
}

func (r ObserverReceipt) payload() []byte {
	one, _ := canonicalTime(r.ReadOneAt)
	two, _ := canonicalTime(r.ReadTwoAt)
	issuedAt, _ := canonicalTime(r.AuthorizationIssuedAtUTC)
	notBefore, _ := canonicalTime(r.AuthorizationNotBeforeUTC)
	notAfter, _ := canonicalTime(r.AuthorizationNotAfterUTC)
	return []byte("observer-receipt/v2\x00" + r.Version + "\x00" + r.OperationID + "\x00" + strconv.FormatUint(r.Generation, 10) + "\x00" + r.Phase + "\x00" +
		r.OperationBodySHA256.String() + "\x00" + r.MarkerSHA256.String() + "\x00" + r.FixedKeySHA256.String() + "\x00" + r.RecoveryAdmissionSHA256.String() + "\x00" +
		r.AuthoritySHA256.String() + "\x00" + r.BrokerSHA256.String() + "\x00" + r.LedgerSHA256.String() + "\x00" +
		r.RootSHA256.String() + "\x00" + r.BoundarySHA256.String() + "\x00" + r.CapabilitySHA256.String() + "\x00" + r.AdmissionChainSHA256.String() + "\x00" +
		r.PublicationRequestSHA256.String() + "\x00" + r.PublicationOracleSHA256.String() + "\x00" +
		strconv.Itoa(int(r.RecoveryReadCount)) + "\x00" + issuedAt + "\x00" + notBefore + "\x00" + notAfter + "\x00" + one + "\x00" + two)
}

func observerReceiptDigest(receipt ObserverReceipt) Digest {
	return HashBytes(append(receipt.payload(), receipt.Signature...))
}

func verifyObserverObservation(boundary *ObserverBoundary, receipt ObserverReceipt) error {
	delay := receipt.ReadTwoAt.Sub(receipt.ReadOneAt)
	_, oneErr := canonicalTime(receipt.ReadOneAt)
	_, twoErr := canonicalTime(receipt.ReadTwoAt)
	_, issuedErr := canonicalTime(receipt.AuthorizationIssuedAtUTC)
	_, notBeforeErr := canonicalTime(receipt.AuthorizationNotBeforeUTC)
	_, notAfterErr := canonicalTime(receipt.AuthorizationNotAfterUTC)
	if boundary == nil || oneErr != nil || twoErr != nil || issuedErr != nil || notBeforeErr != nil || notAfterErr != nil || delay < MinimumObserverReadDelay || delay > MaximumObserverReadDelay ||
		receipt.Version != ProtocolVersion || !validSyntheticToken(receipt.OperationID) || receipt.Generation == 0 || !validPhase(receipt.Phase) || receipt.OperationBodySHA256.IsZero() || receipt.RecoveryReadCount != 2 || receipt.MarkerSHA256.IsZero() || receipt.FixedKeySHA256 != HashString(FixedMarkerKey) || receipt.RecoveryAdmissionSHA256.IsZero() ||
		receipt.AuthoritySHA256 != boundary.authority.IdentityDigest() || receipt.BrokerSHA256 != boundary.broker.IdentityDigest() ||
		receipt.LedgerSHA256 != boundary.ledger || receipt.RootSHA256 != boundary.root || receipt.BoundarySHA256 != boundary.closed || receipt.CapabilitySHA256.IsZero() || receipt.AdmissionChainSHA256.IsZero() ||
		!receipt.AuthorizationIssuedAtUTC.Before(receipt.AuthorizationNotAfterUTC) || !receipt.AuthorizationNotBeforeUTC.Before(receipt.AuthorizationNotAfterUTC) ||
		receipt.ReadOneAt.Before(receipt.AuthorizationIssuedAtUTC) || receipt.ReadOneAt.Before(receipt.AuthorizationNotBeforeUTC) || !receipt.ReadOneAt.Before(receipt.AuthorizationNotAfterUTC) ||
		receipt.ReadTwoAt.Before(receipt.AuthorizationIssuedAtUTC) || receipt.ReadTwoAt.Before(receipt.AuthorizationNotBeforeUTC) || !receipt.ReadTwoAt.Before(receipt.AuthorizationNotAfterUTC) ||
		receipt.PublicationRequestSHA256.IsZero() || receipt.PublicationOracleSHA256.IsZero() || receipt.publicationRequest.Validate() != nil ||
		len(receipt.Signature) != ed25519.SignatureSize || !ed25519.Verify(boundary.public, receipt.payload(), receipt.Signature) {
		return ErrInvalid
	}
	expectedRequest, expectedErr := observerEvidenceRequestFromClaims(receipt.OperationID, receipt.Generation, receipt.Phase,
		receipt.OperationBodySHA256, receipt.RecoveryAdmissionSHA256)
	expectedDigest, expectedDigestErr := expectedRequest.Digest()
	requestDigest, err := receipt.publicationRequest.Digest()
	if expectedErr != nil || expectedDigestErr != nil || err != nil || requestDigest != receipt.PublicationRequestSHA256 || requestDigest != expectedDigest {
		return ErrInvalid
	}
	return nil
}

func verifyObserverReceipt(boundary *ObserverBoundary, receipt ObserverReceipt) error {
	if err := verifyObserverObservation(boundary, receipt); err != nil {
		return ErrInvalid
	}
	if err := verifyCompletionWitness(boundary.public, ObserverAuthorityRole, receipt.LedgerSHA256,
		receipt.RootSHA256, receipt.PublicationOracleSHA256, receipt.publicationRequest,
		observerReceiptDigest(receipt), receipt.RecoveryAdmissionSHA256, receipt.CompletionWitness); err != nil {
		return ErrInvalid
	}
	return nil
}

func VerifyObserverReceipt(boundary *ClosedBoundary, receipt ObserverReceipt) error {
	view, err := boundary.ObserverView()
	if err != nil || receipt.PublicationOracleSHA256 != view.store {
		return ErrInvalid
	}
	return verifyObserverReceipt(view, receipt)
}

type ObserverAuthority struct {
	unit       SubstrateUnit
	privateKey ed25519.PrivateKey
}

func NewObserverAuthority(unit SubstrateUnit, privateKey ed25519.PrivateKey) (*ObserverAuthority, error) {
	if unit.Role() != ObserverAuthorityRole || unit.Domain() != NoDataDomain || len(privateKey) != ed25519.PrivateKeySize {
		return nil, ErrRoleIsolation
	}
	return &ObserverAuthority{unit: unit, privateKey: append(ed25519.PrivateKey(nil), privateKey...)}, nil
}

// IssueChallenge generates the observer authority's independent 256-bit
// challenge and durably binds it to one exact request and call kind before
// returning it. It cannot be substituted for a writer request or endpoint.
func (a *ObserverAuthority) IssueChallenge(ctx context.Context, effects *ProtocolCrashOracle, request MutationRequest, call CallKind) (AuthorityChallenge, error) {
	if a == nil || effects == nil || a.unit.Role() != ObserverAuthorityRole || len(a.privateKey) != ed25519.PrivateKeySize {
		return AuthorityChallenge{}, ErrRoleIsolation
	}
	root := HashBytes(append([]byte("observer-root/v2\x00"), a.privateKey.Public().(ed25519.PublicKey)...))
	return effects.issueAuthorityChallenge(ctx, request, call, ObserverAuthorityRole, root, rand.Reader)
}

func (a *ObserverAuthority) recoveryAdmissionSkeleton(boundary *ObserverBoundary, broker *ObserverBroker, effects *ProtocolCrashOracle, base OperationBody, request ObserverAdmissionRequest, descriptor ObserverLifecycleDescriptor, lifecycle ObserverLifecycleProof) (RecoveryAdmission, error) {
	if a == nil || broker == nil || effects == nil || !effects.validInstance() {
		return RecoveryAdmission{}, ErrInvalid
	}
	public := a.privateKey.Public().(ed25519.PublicKey)
	if err := boundary.validate(a.unit, broker.unit, public); err != nil {
		return RecoveryAdmission{}, err
	}
	if err := validateObserverAdmissionRequest(boundary, request); err != nil {
		return RecoveryAdmission{}, err
	}
	if effects.identity != request.observerStoreSHA256 {
		return RecoveryAdmission{}, ErrRoleIsolation
	}
	bodyDigest, bodyErr := base.Digest()
	if bodyErr != nil || bodyDigest != request.operationBodySHA256 || base.OperationID != request.operationID || base.Generation != request.generation || base.Phase != request.phase ||
		descriptor.ObserverAdmissionRequestSHA256 != request.Digest() || descriptor.ForkWriterPublicationSHA256 != request.outerEvidenceSHA256 ||
		descriptor.ContinuityBindingSHA256 != request.continuityBindingSHA256 ||
		VerifyObserverLifecycleProof(boundary, base, descriptor, lifecycle, false) != nil {
		return RecoveryAdmission{}, ErrForkNotAuthorized
	}
	// Verify against the exact original body is performed by callers; here the
	// proof must at minimum be exact to the admission request and observer trust.
	if lifecycle.Version != ProtocolVersion || lifecycle.Binding.ObserverAdmissionRequestSHA256 != request.Digest() || len(lifecycle.Admission) != len(observerAdmissionEffects) || len(lifecycle.Cleanup) != 0 {
		return RecoveryAdmission{}, ErrForkNotAuthorized
	}
	last := lifecycle.Admission[len(lifecycle.Admission)-1].Completion.CompletedAt
	if last.Before(request.writerPublicationIssuedAt) || !last.Before(request.writerPublicationNotAfter) {
		return RecoveryAdmission{}, ErrForkNotAuthorized
	}
	admission := RecoveryAdmission{request: request, observerOracleSHA256: effects.identity, lifecycleAdmissionSHA256: lifecycle.Digest(),
		issuedAt: last, authorizationNotAfter: request.writerPublicationNotAfter}
	publicationRequest, err := recoveryAdmissionSignatureRequest(request)
	if err != nil {
		return RecoveryAdmission{}, err
	}
	admission.publicationRequest = publicationRequest
	return admission, nil
}

func recoveryAdmissionForResult(boundary *ObserverBoundary, oracle *ProtocolCrashOracle, result Digest) (RecoveryAdmission, error) {
	admission, ok := oracle.recoveryAdmission(result)
	if !ok || admission.Digest() != result {
		return RecoveryAdmission{}, ErrQuarantined
	}
	witness, ok := oracle.completionWitness(admission.publicationRequest)
	if !ok {
		return RecoveryAdmission{}, ErrQuarantined
	}
	admission.observerCompletionWitness = witness
	if err := verifyRecoveryAdmission(boundary, admission); err != nil {
		return RecoveryAdmission{}, err
	}
	return admission, nil
}

// MintRecoveryAdmission signs only the durable controller's opaque hash-only
// authorization and durably publishes that exact signature through the
// separate observer oracle. The observer authority never receives terminal
// evidence, writer receipts, a marker hash, or a marker preimage.
func (a *ObserverAuthority) MintRecoveryAdmission(ctx context.Context, boundary *ObserverBoundary, broker *ObserverBroker, effects *ProtocolCrashOracle, base OperationBody, request ObserverAdmissionRequest, descriptor ObserverLifecycleDescriptor, lifecycle ObserverLifecycleProof, auth AuthEnvelope, fault FaultMode) (RecoveryAdmission, error) {
	if effects == nil {
		return RecoveryAdmission{}, ErrInvalid
	}
	if boundary == nil || auth.Challenge.validateFor(ObserverAuthorityRole, boundary.root) != nil {
		return RecoveryAdmission{}, effects.rejectAfterAuth(ctx, auth, ErrInvalid)
	}
	publicationRequest, requestErr := recoveryAdmissionSignatureRequest(request)
	if requestErr != nil {
		return RecoveryAdmission{}, effects.rejectAfterAuth(ctx, auth, requestErr)
	}
	var admission RecoveryAdmission
	result, err := effects.authorizeAndConsumeChecked(ctx, publicationRequest, auth, fault, request.outerEvidenceSHA256, func() error {
		var skeletonErr error
		admission, skeletonErr = a.recoveryAdmissionSkeleton(boundary, broker, effects, base, request, descriptor, lifecycle)
		return skeletonErr
	}, func() (Digest, error) {
		if !effects.now().Before(admission.authorizationNotAfter) {
			return Digest{}, ErrForkNotAuthorized
		}
		// Signing is the issue-once side effect. It occurs only after the
		// observer ledger has consumed fresh authentication and durably entered
		// the started state; ambiguity can thereafter use status only.
		signature, signErr := effects.signLocked(ObserverAuthorityRole, boundary.root, admission.payload())
		if signErr != nil {
			return Digest{}, signErr
		}
		admission.signature = signature
		if err := verifyRecoveryAdmissionSignature(boundary, admission); err != nil {
			return Digest{}, err
		}
		return effects.registerRecoveryAdmissionLocked(admission.publicationRequest, admission)
	})
	if err != nil {
		return RecoveryAdmission{}, err
	}
	return recoveryAdmissionForResult(boundary, effects, result)
}

// ReconcileRecoveryAdmission is status-only and never signs a new admission.
// It can return only the deterministic signature registered by the original
// mint callback.
func (a *ObserverAuthority) ReconcileRecoveryAdmission(ctx context.Context, boundary *ObserverBoundary, broker *ObserverBroker, effects *ProtocolCrashOracle, base OperationBody, request ObserverAdmissionRequest, descriptor ObserverLifecycleDescriptor, lifecycle ObserverLifecycleProof, auth AuthEnvelope) (RecoveryAdmission, error) {
	if effects == nil {
		return RecoveryAdmission{}, ErrInvalid
	}
	if boundary == nil || auth.Challenge.validateFor(ObserverAuthorityRole, boundary.root) != nil {
		return RecoveryAdmission{}, effects.rejectAfterAuth(ctx, auth, ErrInvalid)
	}
	publicationRequest, requestErr := recoveryAdmissionSignatureRequest(request)
	if requestErr != nil {
		return RecoveryAdmission{}, effects.rejectAfterAuth(ctx, auth, requestErr)
	}
	result, err := effects.reconcileStatusChecked(ctx, publicationRequest, auth, func() error {
		_, skeletonErr := a.recoveryAdmissionSkeleton(boundary, broker, effects, base, request, descriptor, lifecycle)
		return skeletonErr
	}, func() ([]Digest, error) {
		digest, ok := effects.recoveryAdmissionForRequestLocked(publicationRequest)
		if !ok {
			return nil, nil
		}
		return []Digest{digest}, nil
	})
	if err != nil {
		return RecoveryAdmission{}, err
	}
	return recoveryAdmissionForResult(boundary, effects, result)
}

type ObserverBroker struct {
	unit         SubstrateUnit
	mtlsPeer     Digest
	target       Digest
	authority    Digest
	public       ed25519.PublicKey
	recovery     RecoveryMarkerReader
	now          func() time.Time
	authorityNow func() time.Time
}

func NewObserverBroker(unit SubstrateUnit, recovery RecoveryMarkerReader) (*ObserverBroker, error) {
	if unit.Role() != ObserverBrokerRole || unit.Domain() != RecoveryDomain || recovery == nil || recovery.RecoveryBindingSHA256().IsZero() {
		return nil, ErrRoleIsolation
	}
	return &ObserverBroker{unit: unit, mtlsPeer: HashString("mtls-peer/v2\x00" + unit.rawIdentifier),
		target: recovery.RecoveryBindingSHA256(), recovery: recovery}, nil
}

func (b *ObserverBroker) bindAuthority(authority Digest, public ed25519.PublicKey) error {
	if b == nil || authority.IsZero() || len(public) != ed25519.PublicKeySize {
		return ErrRoleIsolation
	}
	if (!b.authority.IsZero() && b.authority != authority) || (len(b.public) != 0 && !bytes.Equal(b.public, public)) {
		return ErrRoleIsolation
	}
	b.authority = authority
	b.public = append(ed25519.PublicKey(nil), public...)
	return nil
}

func (b *ObserverBroker) validateBinding() error {
	if b == nil || b.recovery == nil || b.now == nil || b.authorityNow == nil || b.target.IsZero() || b.authority.IsZero() || len(b.public) != ed25519.PublicKeySize ||
		b.recovery.RecoveryBindingSHA256() != b.target {
		return ErrRoleIsolation
	}
	return nil
}

// readRecoveryTwice is the only recovery data-plane operation. Raw values
// remain broker-local; the authority receives only their common digest and
// the two observation timestamps.
func (b *ObserverBroker) readRecoveryTwice(body OperationBody, capability BrokerCapability, clock ObservationClock) (Digest, time.Time, time.Time, error) {
	if err := b.validateBinding(); err != nil || clock == nil {
		return Digest{}, time.Time{}, time.Time{}, ErrRoleIsolation
	}
	readOneStartedAt := clock.Now()
	if !readOneStartedAt.Equal(b.authorityNow()) {
		return Digest{}, time.Time{}, time.Time{}, ErrForkNotAuthorized
	}
	if err := verifyBrokerCapability(b.public, capability, ObserverBrokerRole, body, b.authority,
		b.unit.IdentityDigest(), b.mtlsPeer, b.target, capability.AdmissionChainSHA256, readOneStartedAt); err != nil {
		return Digest{}, time.Time{}, time.Time{}, err
	}
	first, exists, err := b.recovery.ReadRecovery()
	one := clock.Now()
	if one.Before(readOneStartedAt) || !one.Equal(b.authorityNow()) {
		clear(first)
		return Digest{}, time.Time{}, time.Time{}, ErrForkNotAuthorized
	}
	if err := verifyBrokerCapability(b.public, capability, ObserverBrokerRole, body, b.authority,
		b.unit.IdentityDigest(), b.mtlsPeer, b.target, capability.AdmissionChainSHA256, one); err != nil {
		clear(first)
		return Digest{}, time.Time{}, time.Time{}, err
	}
	if err != nil || !exists || len(first) != 32 {
		clear(first)
		return Digest{}, time.Time{}, time.Time{}, ErrNotObserved
	}
	clock.Wait(MinimumObserverReadDelay)
	readTwoStartedAt := clock.Now()
	if readTwoStartedAt.Before(one) || !readTwoStartedAt.Equal(b.authorityNow()) {
		clear(first)
		return Digest{}, time.Time{}, time.Time{}, ErrForkNotAuthorized
	}
	if err := verifyBrokerCapability(b.public, capability, ObserverBrokerRole, body, b.authority,
		b.unit.IdentityDigest(), b.mtlsPeer, b.target, capability.AdmissionChainSHA256, readTwoStartedAt); err != nil {
		clear(first)
		return Digest{}, time.Time{}, time.Time{}, err
	}
	second, exists, err := b.recovery.ReadRecovery()
	two := clock.Now()
	if two.Before(readTwoStartedAt) || !two.Equal(b.authorityNow()) {
		clear(first)
		clear(second)
		return Digest{}, time.Time{}, time.Time{}, ErrForkNotAuthorized
	}
	if err := verifyBrokerCapability(b.public, capability, ObserverBrokerRole, body, b.authority,
		b.unit.IdentityDigest(), b.mtlsPeer, b.target, capability.AdmissionChainSHA256, two); err != nil {
		clear(first)
		clear(second)
		return Digest{}, time.Time{}, time.Time{}, err
	}
	if err != nil || !exists || len(second) != 32 || subtle.ConstantTimeCompare(first, second) != 1 {
		clear(first)
		clear(second)
		return Digest{}, time.Time{}, time.Time{}, ErrMarkerMismatch
	}
	digest := HashBytes(second)
	clear(first)
	clear(second)
	return digest, one, two, nil
}

type ObserverProtocol struct {
	boundary           *ObserverBoundary
	effects            *ProtocolCrashOracle
	authority          *ObserverAuthority
	broker             *ObserverBroker
	clock              ObservationClock
	admission          RecoveryAdmission
	base               OperationBody
	descriptor         ObserverLifecycleDescriptor
	lifecycleAdmission ObserverLifecycleProof
}

func NewObserverProtocol(boundary *ObserverBoundary, effects *ProtocolCrashOracle, authority *ObserverAuthority, broker *ObserverBroker, clock ObservationClock, base OperationBody, descriptor ObserverLifecycleDescriptor, lifecycle ObserverLifecycleProof, admission RecoveryAdmission) (*ObserverProtocol, error) {
	if effects == nil || authority == nil || broker == nil || clock == nil {
		return nil, ErrInvalid
	}
	public := authority.privateKey.Public().(ed25519.PublicKey)
	if err := boundary.validate(authority.unit, broker.unit, public); err != nil {
		return nil, err
	}
	if err := broker.bindAuthority(authority.unit.IdentityDigest(), public); err != nil {
		return nil, err
	}
	broker.now = effects.now
	broker.authorityNow = effects.now
	if err := verifyRecoveryAdmission(boundary, admission); err != nil || admission.observerOracleSHA256 != effects.identity {
		return nil, ErrForkNotAuthorized
	}
	bodyDigest, bodyErr := base.Digest()
	now := clock.Now()
	if bodyErr != nil || bodyDigest != admission.request.operationBodySHA256 || descriptor.ObserverAdmissionRequestSHA256 != admission.request.Digest() ||
		VerifyObserverLifecycleProof(boundary, base, descriptor, lifecycle, false) != nil || lifecycle.Digest() != admission.lifecycleAdmissionSHA256 ||
		len(lifecycle.Admission) == 0 || lifecycle.Admission[len(lifecycle.Admission)-1].Completion.CompletedAt.After(admission.observerCompletionWitness.CompletedAt) ||
		!now.Equal(effects.now()) || !now.Before(admission.authorizationNotAfter) || broker.target != admission.request.recoveryTargetSHA256 {
		return nil, ErrForkNotAuthorized
	}
	return &ObserverProtocol{boundary: boundary, effects: effects, authority: authority, broker: broker, clock: clock, admission: admission.clone(), base: base, descriptor: descriptor, lifecycleAdmission: lifecycle}, nil
}

func purposeOperationBody(base OperationBody, purpose string) (OperationBody, error) {
	if err := base.Validate(); err != nil || purpose == "" {
		return OperationBody{}, ErrInvalid
	}
	identity := base.OperationID + "\x00" + strconv.FormatUint(base.Generation, 10) + "\x00" + purpose
	return OperationBody{
		Version: ProtocolVersion, OperationID: "synthetic-" + purpose + "-" + HashString(identity).String()[:20],
		Generation: base.Generation, Phase: base.Phase,
		ConfigDigest: HashString("purpose-config/v2\x00" + identity),
	}, nil
}

func purposeOperationBodyFromClaims(operationID string, generation uint64, phase, purpose string) (OperationBody, error) {
	base := OperationBody{Version: ProtocolVersion, OperationID: operationID, Generation: generation, Phase: phase,
		ConfigDigest: HashString("purpose-claims-validation/v2\x00" + operationID)}
	return purposeOperationBody(base, purpose)
}

func recoveryAdmissionSignatureRequest(admission ObserverAdmissionRequest) (MutationRequest, error) {
	base := OperationBody{
		Version: ProtocolVersion, OperationID: admission.operationID, Generation: admission.generation,
		Phase:        admission.phase,
		ConfigDigest: HashString("recovery-admission-signature-base/v2\x00" + admission.operationBodySHA256.String()),
	}
	purpose, err := purposeOperationBody(base, "recovery-admission-signature")
	if err != nil {
		return MutationRequest{}, err
	}
	return MutationRequest{Operation: purpose, Effect: EffectEvidencePublish,
		ParametersDigest: HashString("recovery-admission-signature/v2\x00" + admission.Digest().String())}, nil
}

func observerEvidenceRequest(body OperationBody, admission Digest) (MutationRequest, error) {
	bodyDigest, err := body.Digest()
	if err != nil || admission.IsZero() {
		return MutationRequest{}, ErrInvalid
	}
	purpose, err := purposeOperationBody(body, "observer-evidence")
	if err != nil {
		return MutationRequest{}, err
	}
	return MutationRequest{Operation: purpose, Effect: EffectEvidencePublish,
		ParametersDigest: HashString("observer-evidence/v2\x00" + bodyDigest.String() + "\x00" + admission.String())}, nil
}

func observerEvidenceRequestFromClaims(operationID string, generation uint64, phase string, operationBody, admission Digest) (MutationRequest, error) {
	if operationBody.IsZero() || admission.IsZero() {
		return MutationRequest{}, ErrInvalid
	}
	purpose, err := purposeOperationBodyFromClaims(operationID, generation, phase, "observer-evidence")
	if err != nil {
		return MutationRequest{}, err
	}
	return MutationRequest{Operation: purpose, Effect: EffectEvidencePublish,
		ParametersDigest: HashString("observer-evidence/v2\x00" + operationBody.String() + "\x00" + admission.String())}, nil
}

func (o *ObserverProtocol) capability(body OperationBody, auth AuthEnvelope) (BrokerCapability, error) {
	authorization, err := auth.Digest()
	if err != nil {
		return BrokerCapability{}, err
	}
	notAfter := auth.ExpiresAt
	if o.admission.authorizationNotAfter.Before(notAfter) {
		notAfter = o.admission.authorizationNotAfter
	}
	return issueBrokerCapability(o.authority.privateKey, ObserverBrokerRole, body, o.authority.unit.IdentityDigest(),
		o.broker.unit.IdentityDigest(), o.broker.mtlsPeer, o.broker.target, o.admission.Digest(), authorization, o.effects.now(), auth.NotBefore, notAfter)
}

func (o *ObserverProtocol) buildReceipt(body OperationBody, request MutationRequest, auth AuthEnvelope) (ObserverReceipt, error) {
	bodyDigest, err := body.Digest()
	if err != nil || body.OperationID != o.admission.request.operationID || body.Generation != o.admission.request.generation ||
		body.Phase != o.admission.request.phase || bodyDigest != o.admission.request.operationBodySHA256 || request.Validate() != nil {
		return ObserverReceipt{}, ErrForkNotAuthorized
	}
	requestDigest, err := request.Digest()
	if err != nil {
		return ObserverReceipt{}, err
	}
	capability, err := o.capability(body, auth)
	if err != nil {
		return ObserverReceipt{}, err
	}
	marker, one, two, err := o.broker.readRecoveryTwice(body, capability, o.clock)
	if err != nil {
		return ObserverReceipt{}, err
	}
	receipt := ObserverReceipt{Version: ProtocolVersion, OperationID: body.OperationID, Generation: body.Generation, Phase: body.Phase,
		OperationBodySHA256: bodyDigest, MarkerSHA256: marker, FixedKeySHA256: HashString(FixedMarkerKey), RecoveryAdmissionSHA256: o.admission.Digest(),
		AuthoritySHA256: o.authority.unit.IdentityDigest(), BrokerSHA256: o.broker.unit.IdentityDigest(), LedgerSHA256: o.boundary.ledger,
		RootSHA256: o.boundary.root, BoundarySHA256: o.boundary.closed,
		CapabilitySHA256: capability.Digest(), AdmissionChainSHA256: capability.AdmissionChainSHA256,
		AuthorizationIssuedAtUTC: capability.AuthorizationIssuedAtUTC, AuthorizationNotBeforeUTC: capability.AuthorizationNotBeforeUTC,
		AuthorizationNotAfterUTC: capability.AuthorizationNotAfter,
		PublicationRequestSHA256: requestDigest, PublicationOracleSHA256: o.effects.identity,
		RecoveryReadCount: 2, ReadOneAt: one, ReadTwoAt: two, publicationRequest: request}
	receipt.Signature = ed25519.Sign(o.authority.privateKey, receipt.payload())
	if err := verifyObserverObservation(o.boundary, receipt); err != nil {
		return ObserverReceipt{}, err
	}
	return receipt, nil
}

func (o *ObserverProtocol) receiptForResult(result Digest) (ObserverReceipt, error) {
	receipt, ok := o.effects.observerReceipt(result)
	if !ok || observerReceiptDigest(receipt) != result || receipt.RecoveryAdmissionSHA256 != o.admission.Digest() {
		return ObserverReceipt{}, ErrQuarantined
	}
	witness, ok := o.effects.completionWitness(receipt.publicationRequest)
	if !ok {
		return ObserverReceipt{}, ErrQuarantined
	}
	receipt.CompletionWitness = witness
	if err := verifyObserverReceipt(o.boundary, receipt); err != nil {
		return ObserverReceipt{}, err
	}
	return receipt, nil
}

// ReadRecoveryTwice never receives source trust, a writer receipt, an expected
// marker, or a source reader. The exact signed receipt is durably registered
// before any after-effect ambiguity is returned by the fault oracle.
func (o *ObserverProtocol) ReadRecoveryTwice(ctx context.Context, body OperationBody, auth AuthEnvelope, fault FaultMode) (ObserverReceipt, error) {
	if o == nil || o.effects == nil || o.boundary == nil {
		return ObserverReceipt{}, ErrRoleIsolation
	}
	if auth.Challenge.validateFor(ObserverAuthorityRole, o.boundary.root) != nil {
		return ObserverReceipt{}, o.effects.rejectAfterAuth(ctx, auth, ErrInvalid)
	}
	request, err := observerEvidenceRequest(o.base, o.admission.Digest())
	if err != nil {
		return ObserverReceipt{}, err
	}
	check := func() error {
		bodyDigest, bodyErr := body.Digest()
		expectedDigest, _ := o.base.Digest()
		if bodyErr != nil || bodyDigest != expectedDigest || body.OperationID != o.base.OperationID || body.Generation != o.base.Generation || body.Phase != o.base.Phase {
			return ErrQuarantined
		}
		return nil
	}
	result, err := o.effects.authorizeAndConsumeChecked(ctx, request, auth, fault, o.admission.Digest(), check, func() (Digest, error) {
		receipt, buildErr := o.buildReceipt(body, request, auth)
		if buildErr != nil {
			return Digest{}, buildErr
		}
		if buildErr = verifyObserverObservation(o.boundary, receipt); buildErr != nil {
			return Digest{}, buildErr
		}
		return o.effects.registerObserverReceiptLocked(request, receipt)
	})
	if err != nil {
		return ObserverReceipt{}, err
	}
	return o.receiptForResult(result)
}

// ReconcileRecoveryEvidence is status-only: it can return only an exact
// receipt persisted by the original issue-once callback. It never reads the
// recovery store and never signs new evidence.
func (o *ObserverProtocol) ReconcileRecoveryEvidence(ctx context.Context, body OperationBody, auth AuthEnvelope) (ObserverReceipt, error) {
	if o == nil || o.effects == nil || o.boundary == nil {
		return ObserverReceipt{}, ErrRoleIsolation
	}
	if auth.Challenge.validateFor(ObserverAuthorityRole, o.boundary.root) != nil {
		return ObserverReceipt{}, o.effects.rejectAfterAuth(ctx, auth, ErrInvalid)
	}
	request, err := observerEvidenceRequest(o.base, o.admission.Digest())
	if err != nil {
		return ObserverReceipt{}, err
	}
	result, err := o.effects.reconcileStatusChecked(ctx, request, auth, func() error {
		bodyDigest, bodyErr := body.Digest()
		expectedDigest, _ := o.base.Digest()
		if bodyErr != nil || bodyDigest != expectedDigest || body.OperationID != o.base.OperationID || body.Generation != o.base.Generation || body.Phase != o.base.Phase {
			return ErrForkNotAuthorized
		}
		return nil
	}, func() ([]Digest, error) {
		digest, ok := o.effects.observerReceiptForRequestLocked(request)
		if !ok {
			return nil, nil
		}
		return []Digest{digest}, nil
	})
	if err != nil {
		return ObserverReceipt{}, err
	}
	return o.receiptForResult(result)
}

// VerifyRecoveryBoundary is the sole public continuity composition. It
// requires the complete writer terminal proof, the fork-bound admission, and
// the independently signed recovery-only observation.
func VerifyRecoveryBoundary(boundary *ClosedBoundary, base OperationBody, descriptor ObserverLifecycleDescriptor, lifecycle ObserverLifecycleProof, terminal WriterTerminalReceipt, evidence WriterTerminalEvidence, writerPublication RecoveryAdmissionAuthorization, admission RecoveryAdmission, observer ObserverReceipt) error {
	if err := VerifyWriterTerminalReceipt(boundary, terminal, evidence); err != nil {
		return err
	}
	if err := verifyRecoveryAdmissionAuthorization(boundary, writerPublication); err != nil {
		return err
	}
	expectedAdmissionRequest, err := PrepareObserverAdmission(boundary, writerPublication)
	if err != nil || admission.request != expectedAdmissionRequest {
		return ErrForkNotAuthorized
	}
	view, err := boundary.ObserverView()
	if err != nil {
		return err
	}
	if err := verifyRecoveryAdmission(view, admission); err != nil {
		return err
	}
	if err := VerifyObserverReceipt(boundary, observer); err != nil {
		return err
	}
	if VerifyObserverLifecycleProof(view, base, descriptor, lifecycle, true) != nil || len(lifecycle.Admission) == 0 || len(lifecycle.Cleanup) == 0 {
		return ErrForkNotAuthorized
	}
	admissionOnly := lifecycle
	admissionOnly.Cleanup = nil
	firstAdmission := lifecycle.Admission[0].Completion
	lastAdmission := lifecycle.Admission[len(lifecycle.Admission)-1].Completion
	firstCleanup := lifecycle.Cleanup[0].Completion
	if admission.lifecycleAdmissionSHA256 != admissionOnly.Digest() ||
		evidence.ReadTwoAt.After(writerPublication.forkCompletionWitness.CompletedAt) ||
		writerPublication.forkCompletionWitness.CompletedAt.After(writerPublication.issuedAt) ||
		writerPublication.writerCompletionWitness.CompletedAt.Before(writerPublication.issuedAt) ||
		firstAdmission.CompletedAt.Before(writerPublication.writerCompletionWitness.CompletedAt) ||
		lastAdmission.CompletedAt.After(admission.issuedAt) ||
		admission.observerCompletionWitness.CompletedAt.Before(admission.issuedAt) ||
		observer.ReadOneAt.Before(admission.observerCompletionWitness.CompletedAt) || !observer.ReadOneAt.Before(observer.ReadTwoAt) ||
		!observer.ReadOneAt.Before(admission.authorizationNotAfter) || !observer.ReadTwoAt.Before(admission.authorizationNotAfter) ||
		observer.CompletionWitness.CompletedAt.Before(observer.ReadTwoAt) || firstCleanup.CompletedAt.Before(observer.CompletionWitness.CompletedAt) {
		return ErrForkNotAuthorized
	}
	if admission.request.outerEvidenceSHA256 != writerPublication.Digest() ||
		admission.request.terminalBindingSHA256 != terminalReceiptDigest(terminal) ||
		admission.request.operationID != terminal.OperationID || admission.request.generation != terminal.Generation || admission.request.phase != terminal.Phase ||
		admission.request.operationBodySHA256 != terminal.OperationBodySHA256 || observer.OperationID != terminal.OperationID ||
		observer.Generation != terminal.Generation || observer.Phase != terminal.Phase || observer.OperationBodySHA256 != terminal.OperationBodySHA256 ||
		observer.RecoveryAdmissionSHA256 != admission.Digest() || observer.FixedKeySHA256 != HashString(FixedMarkerKey) ||
		observer.PublicationOracleSHA256 != admission.observerOracleSHA256 || observer.PublicationOracleSHA256 != admission.request.observerStoreSHA256 ||
		observer.MarkerSHA256 != terminal.MarkerSHA256 {
		return ErrMarkerMismatch
	}
	return nil
}

type PreOperationProjection struct {
	FirewallSHA256, ActionLedgerSHA256, ProvenanceSHA256 Digest
	CapturedAt                                           time.Time
}

func (p PreOperationProjection) CanonicalBytes() ([]byte, error) {
	when, err := canonicalTime(p.CapturedAt)
	if err != nil || p.FirewallSHA256.IsZero() || p.ActionLedgerSHA256.IsZero() || p.ProvenanceSHA256.IsZero() {
		return nil, ErrInvalid
	}
	return []byte("pre-operation-projection/v2\x00" + p.FirewallSHA256.String() + "\x00" + p.ActionLedgerSHA256.String() + "\x00" + p.ProvenanceSHA256.String() + "\x00" + when), nil
}
func (p PreOperationProjection) Digest() (Digest, error) {
	b, err := p.CanonicalBytes()
	if err != nil {
		return Digest{}, err
	}
	return HashBytes(b), nil
}

type RevocationKind string

const (
	RevokeCapability  RevocationKind = "capability"
	RevokeLeaf        RevocationKind = "leaf"
	RevokeMTLS        RevocationKind = "mtls"
	RevokeWrappingKey RevocationKind = "wrapping-key"
	RemoveBinding     RevocationKind = "binding"
	RemoveCredential  RevocationKind = "credential"
)

func revocationEffect(kind RevocationKind) (Effect, bool) {
	switch kind {
	case RevokeCapability:
		return EffectCapabilityRevoke, true
	case RevokeLeaf:
		return EffectLeafRevoke, true
	case RevokeMTLS:
		return EffectMTLSRevoke, true
	case RevokeWrappingKey:
		return EffectWrappingKeyRevoke, true
	case RemoveBinding:
		return EffectBindingRemove, true
	case RemoveCredential:
		return EffectCredentialRemove, true
	}
	return "", false
}

type DeleteResult string

const (
	DeleteDefinitiveSuccess DeleteResult = "definitive-success"
	DeleteAmbiguous408      DeleteResult = "ambiguous-408"
	DeleteAmbiguousEOF      DeleteResult = "ambiguous-eof"
	DeleteAmbiguous5xx      DeleteResult = "ambiguous-5xx"
)

type DeleteObservation struct {
	DirectGETAbsent                          bool
	PaginatedAppCount, PaginatedDeployments  int
	DeleteActionTerminal                     bool
	RollbackCapableCount                     int
	DeleteActionSHA256                       Digest
	DirectGETAt, InventoryAt, DeleteActionAt time.Time
}

func (o DeleteObservation) provesDeletion() bool {
	return o.DirectGETAbsent && o.PaginatedAppCount == 0 && o.PaginatedDeployments == 0 && o.DeleteActionTerminal && o.RollbackCapableCount == 0 && !o.DeleteActionSHA256.IsZero()
}

type WriterTerminalEvidence struct {
	OriginalProjection                                                                    PreOperationProjection
	FirewallReadOneSHA256, FirewallReadTwoSHA256                                          Digest
	ActionReadOneSHA256, ActionReadTwoSHA256                                              Digest
	ProviderProvenanceSHA256                                                              Digest
	NoWriterOrBroadRules, CapabilityRevoked, LeafRevoked, MTLSRevoked, WrappingKeyRevoked bool
	BindingAbsent, CredentialAbsent, FullRedeployComplete                                 bool
	Deletion                                                                              DeleteObservation
	DeletionRequestSHA256                                                                 Digest
	DeletionCompletionWitness                                                             CompletionWitness
	DeletionReconciled, OldInstanceGraceElapsed                                           bool
	AppInventoryReadOneComplete, AppInventoryReadTwoComplete                              bool
	DeploymentInventoryReadOneComplete, DeploymentInventoryReadTwoComplete                bool
	AppInventoryCountOne, AppInventoryCountTwo                                            int
	DeploymentInventoryCountOne, DeploymentInventoryCountTwo                              int
	ProviderOperationReadOneComplete, ProviderOperationReadTwoComplete                    bool
	ProviderOperationCountOne, ProviderOperationCountTwo                                  int
	ProviderNonterminalCountOne, ProviderNonterminalCountTwo                              int
	WrappingKeyProviderObservedResultSHA256                                               Digest
	WrappingKeySealerRequestSHA256, WrappingKeySealerResultSHA256                         Digest
	WrappingKeyProviderRequestSHA256, WrappingKeyProviderResultSHA256                     Digest
	WrappingKeySealerIntent                                                               lifecycleSealerRevocationIntent
	WrappingKeySealerStatus                                                               MarkerSealerRevocationStatus
	WrappingKeySealerCompletionWitness, WrappingKeyProviderCompletionWitness              CompletionWitness
	CapabilityRevokedAt, LeafRevokedAt, MTLSRevokedAt, WrappingKeyRevokedAt               time.Time
	BindingRemovedAt, CredentialRemovedAt, FullRedeployAt, FirewallRestoredAt             time.Time
	DeletionObservedAt, OldInstanceGraceElapsedAt, ReadOneAt, ReadTwoAt                   time.Time
	ProviderObservation                                                                   WriterTerminalObservationReceipt
	// DurableCompletionAt is writer-ledger state, deliberately outside the
	// independently collected GET-only provider facts. Each value must be at or
	// after the corresponding provider-observed effect time.
	DurableCompletionAt map[Effect]time.Time
}

func boolToken(value bool) string {
	if value {
		return "true"
	}
	return "false"
}
func timeToken(value time.Time) (string, error) { return canonicalTime(value) }

func (e WriterTerminalEvidence) canonicalFactsBytes() ([]byte, error) {
	projection, err := e.OriginalProjection.CanonicalBytes()
	if err != nil {
		return nil, ErrForkNotAuthorized
	}
	times := []time.Time{
		e.CapabilityRevokedAt, e.LeafRevokedAt, e.MTLSRevokedAt, e.WrappingKeyRevokedAt,
		e.BindingRemovedAt, e.CredentialRemovedAt, e.FullRedeployAt, e.FirewallRestoredAt,
		e.Deletion.DirectGETAt, e.Deletion.InventoryAt, e.Deletion.DeleteActionAt,
		e.DeletionObservedAt, e.OldInstanceGraceElapsedAt, e.ReadOneAt, e.ReadTwoAt,
	}
	encoded := make([]string, len(times))
	for i, v := range times {
		encoded[i], err = timeToken(v)
		if err != nil {
			return nil, ErrForkNotAuthorized
		}
	}
	if e.ProviderProvenanceSHA256.IsZero() || e.ProviderProvenanceSHA256 != e.OriginalProjection.ProvenanceSHA256 ||
		e.FirewallReadOneSHA256 != e.OriginalProjection.FirewallSHA256 || e.FirewallReadTwoSHA256 != e.OriginalProjection.FirewallSHA256 ||
		e.ActionReadOneSHA256 != e.OriginalProjection.ActionLedgerSHA256 || e.ActionReadTwoSHA256 != e.OriginalProjection.ActionLedgerSHA256 ||
		!e.NoWriterOrBroadRules || !e.CapabilityRevoked || !e.LeafRevoked || !e.MTLSRevoked || !e.WrappingKeyRevoked || !e.BindingAbsent || !e.CredentialAbsent || !e.FullRedeployComplete ||
		!e.Deletion.provesDeletion() || !e.DeletionReconciled || !e.OldInstanceGraceElapsed ||
		!e.AppInventoryReadOneComplete || !e.AppInventoryReadTwoComplete ||
		!e.DeploymentInventoryReadOneComplete || !e.DeploymentInventoryReadTwoComplete ||
		e.AppInventoryCountOne != 0 || e.AppInventoryCountTwo != 0 ||
		e.DeploymentInventoryCountOne != 0 || e.DeploymentInventoryCountTwo != 0 ||
		!e.ProviderOperationReadOneComplete || !e.ProviderOperationReadTwoComplete ||
		e.ProviderOperationCountOne != len(writerTerminalProviderEffects()) ||
		e.ProviderOperationCountTwo != len(writerTerminalProviderEffects()) ||
		e.WrappingKeyProviderObservedResultSHA256.IsZero() ||
		e.ProviderNonterminalCountOne != 0 || e.ProviderNonterminalCountTwo != 0 ||
		e.CapabilityRevokedAt.Before(e.OriginalProjection.CapturedAt) || e.LeafRevokedAt.Before(e.CapabilityRevokedAt) ||
		e.MTLSRevokedAt.Before(e.LeafRevokedAt) || e.WrappingKeyRevokedAt.Before(e.MTLSRevokedAt) ||
		e.BindingRemovedAt.Before(e.WrappingKeyRevokedAt) || e.CredentialRemovedAt.Before(e.BindingRemovedAt) ||
		e.FullRedeployAt.Before(e.CredentialRemovedAt) || e.FirewallRestoredAt.Before(e.FullRedeployAt) ||
		e.DeletionObservedAt.Before(e.FirewallRestoredAt) ||
		e.DeletionObservedAt.Before(e.OriginalProjection.CapturedAt) || e.Deletion.DirectGETAt.Before(e.DeletionObservedAt) || e.Deletion.InventoryAt.Before(e.DeletionObservedAt) || e.Deletion.DeleteActionAt.Before(e.DeletionObservedAt) ||
		e.Deletion.DirectGETAt.After(e.OldInstanceGraceElapsedAt) || e.Deletion.InventoryAt.After(e.OldInstanceGraceElapsedAt) || e.Deletion.DeleteActionAt.After(e.OldInstanceGraceElapsedAt) ||
		e.OldInstanceGraceElapsedAt.Before(e.DeletionObservedAt) || e.ReadOneAt.Before(e.OldInstanceGraceElapsedAt) || e.ReadTwoAt.Sub(e.ReadOneAt) < MinimumTerminalObservationDelay || e.ReadTwoAt.Sub(e.ReadOneAt) > MaximumTerminalObservationDelay {
		return nil, ErrForkNotAuthorized
	}
	payload := "writer-terminal-provider-facts/v2\x00" + string(projection) + "\x00" + e.FirewallReadOneSHA256.String() + "\x00" + e.FirewallReadTwoSHA256.String() + "\x00" + e.ActionReadOneSHA256.String() + "\x00" + e.ActionReadTwoSHA256.String() + "\x00" + e.ProviderProvenanceSHA256.String() + "\x00" +
		strings.Join([]string{boolToken(e.NoWriterOrBroadRules), boolToken(e.CapabilityRevoked), boolToken(e.LeafRevoked), boolToken(e.MTLSRevoked), boolToken(e.WrappingKeyRevoked), boolToken(e.BindingAbsent), boolToken(e.CredentialAbsent), boolToken(e.FullRedeployComplete)}, "\x00") + "\x00" +
		boolToken(e.Deletion.DirectGETAbsent) + "\x00" + strconv.Itoa(e.Deletion.PaginatedAppCount) + "\x00" + strconv.Itoa(e.Deletion.PaginatedDeployments) + "\x00" + boolToken(e.Deletion.DeleteActionTerminal) + "\x00" + strconv.Itoa(e.Deletion.RollbackCapableCount) + "\x00" + e.Deletion.DeleteActionSHA256.String() + "\x00" +
		boolToken(e.DeletionReconciled) + "\x00" + boolToken(e.OldInstanceGraceElapsed) + "\x00" +
		boolToken(e.AppInventoryReadOneComplete) + "\x00" + boolToken(e.AppInventoryReadTwoComplete) + "\x00" +
		boolToken(e.DeploymentInventoryReadOneComplete) + "\x00" + boolToken(e.DeploymentInventoryReadTwoComplete) + "\x00" +
		strconv.Itoa(e.AppInventoryCountOne) + "\x00" + strconv.Itoa(e.AppInventoryCountTwo) + "\x00" +
		strconv.Itoa(e.DeploymentInventoryCountOne) + "\x00" + strconv.Itoa(e.DeploymentInventoryCountTwo) + "\x00" +
		boolToken(e.ProviderOperationReadOneComplete) + "\x00" + boolToken(e.ProviderOperationReadTwoComplete) + "\x00" +
		strconv.Itoa(e.ProviderOperationCountOne) + "\x00" + strconv.Itoa(e.ProviderOperationCountTwo) + "\x00" +
		strconv.Itoa(e.ProviderNonterminalCountOne) + "\x00" + strconv.Itoa(e.ProviderNonterminalCountTwo) + "\x00" +
		e.WrappingKeyProviderObservedResultSHA256.String() + "\x00" + strings.Join(encoded, "\x00")
	return []byte(payload), nil
}

func (e WriterTerminalEvidence) durableCompletionBytes() ([]byte, error) {
	ordered := append(append([]Effect(nil), writerTeardownEffects...), EffectBrokerDelete)
	if len(e.DurableCompletionAt) != len(ordered) {
		return nil, ErrForkNotAuthorized
	}
	parts := []string{"writer-terminal-durable-completions/v2"}
	observed := map[Effect]time.Time{
		EffectCapabilityRevoke: e.CapabilityRevokedAt, EffectLeafRevoke: e.LeafRevokedAt,
		EffectMTLSRevoke: e.MTLSRevokedAt, EffectWrappingKeyRevoke: e.WrappingKeyRevokedAt,
		EffectBindingRemove: e.BindingRemovedAt, EffectCredentialRemove: e.CredentialRemovedAt,
		EffectFullRedeploy: e.FullRedeployAt, EffectTrustedSourceDel: e.FirewallRestoredAt,
		EffectBrokerDelete: e.DeletionObservedAt,
	}
	for _, effect := range ordered {
		completed, ok := e.DurableCompletionAt[effect]
		encoded, err := canonicalTime(completed)
		providerObserved, observedOK := observed[effect]
		if !ok || err != nil || !observedOK || providerObserved.IsZero() || completed.Before(providerObserved) {
			return nil, ErrForkNotAuthorized
		}
		parts = append(parts, string(effect), encoded)
	}
	// The complete portable wrapping-key proof is verified by
	// verifyWrappingKeyLifecycleProof, which has access to the two-root
	// boundary. Its exact request/result/witness digests are nevertheless part
	// of this canonical evidence payload.
	if e.WrappingKeySealerRequestSHA256.IsZero() || e.WrappingKeySealerResultSHA256.IsZero() ||
		e.WrappingKeyProviderRequestSHA256.IsZero() || e.WrappingKeyProviderResultSHA256.IsZero() ||
		e.WrappingKeySealerRequestSHA256 == e.WrappingKeyProviderRequestSHA256 {
		return nil, ErrForkNotAuthorized
	}
	sealerStatusDigest, sealerStatusErr := e.WrappingKeySealerStatus.Digest()
	if sealerStatusErr != nil {
		return nil, ErrForkNotAuthorized
	}
	parts = append(parts,
		"wrapping-sealer-request", e.WrappingKeySealerRequestSHA256.String(),
		"wrapping-sealer-result", e.WrappingKeySealerResultSHA256.String(),
		"wrapping-sealer-intent", e.WrappingKeySealerIntent.Digest().String(),
		"wrapping-sealer-status", sealerStatusDigest.String(),
		"wrapping-sealer-completion", e.WrappingKeySealerCompletionWitness.Digest().String(),
		"wrapping-provider-request", e.WrappingKeyProviderRequestSHA256.String(),
		"wrapping-provider-observed-result", e.WrappingKeyProviderObservedResultSHA256.String(),
		"wrapping-provider-result", e.WrappingKeyProviderResultSHA256.String(),
		"wrapping-provider-completion", e.WrappingKeyProviderCompletionWitness.Digest().String(),
	)
	deleteCompletedAt := e.DurableCompletionAt[EffectBrokerDelete]
	deleteWitness := e.DeletionCompletionWitness
	if e.DeletionRequestSHA256.IsZero() || deleteWitness.Version != ProtocolVersion || deleteWitness.Role != WriterAuthorityRole ||
		deleteWitness.Effect != EffectBrokerDelete || deleteWitness.State != MutationCompleted ||
		deleteWitness.RequestSHA256 != e.DeletionRequestSHA256 || deleteWitness.ResultSHA256 != e.Deletion.DeleteActionSHA256 ||
		!deleteWitness.TerminalSHA256.IsZero() || !deleteWitness.CompletedAt.Equal(deleteCompletedAt) ||
		deleteWitness.Sequence == 0 || len(deleteWitness.Signature) != ed25519.SignatureSize {
		return nil, ErrForkNotAuthorized
	}
	parts = append(parts, "broker-delete-request", e.DeletionRequestSHA256.String(), "broker-delete-completion", deleteWitness.Digest().String())
	return []byte(strings.Join(parts, "\x00")), nil
}

func verifyPortableCompletionWitness(boundary *ClosedBoundary, witness CompletionWitness, request, result Digest, completedAt time.Time) error {
	_, timeErr := canonicalTime(witness.CompletedAt)
	if boundary == nil || request.IsZero() || result.IsZero() || witness.Version != ProtocolVersion || witness.Role != WriterAuthorityRole ||
		witness.Effect != EffectWrappingKeyRevoke || witness.State != MutationCompleted || witness.RequestSHA256 != request || witness.ResultSHA256 != result ||
		!witness.TerminalSHA256.IsZero() || witness.LedgerSHA256 != boundary.writerLedger || witness.RootSHA256 != boundary.writerRoot ||
		witness.OracleSHA256 != boundary.writerStore || witness.Sequence == 0 || timeErr != nil || !witness.CompletedAt.Equal(completedAt) ||
		len(witness.Signature) != ed25519.SignatureSize || !ed25519.Verify(boundary.writerPublic, witness.payload(), witness.Signature) {
		return ErrForkNotAuthorized
	}
	return nil
}

func verifyWrappingKeyLifecycleProof(boundary *ClosedBoundary, e WriterTerminalEvidence) error {
	sealerResult, err := verifyLifecycleSealerTerminalResult(boundary, e.ProviderObservation.MutationProviderSHA256,
		e.ProviderObservation.ProviderConfigSHA256, e.WrappingKeySealerStatus.BindingSHA256,
		e.WrappingKeySealerRequestSHA256, e.WrappingKeySealerIntent, e.WrappingKeySealerStatus)
	if err != nil {
		return fmt.Errorf("wrapping-key sealer terminal proof: %w", ErrForkNotAuthorized)
	}
	providerResult, providerErr := lifecycleSealerProviderResult(sealerResult, e.WrappingKeyProviderObservedResultSHA256)
	if providerErr != nil || sealerResult != e.WrappingKeySealerResultSHA256 || providerResult != e.WrappingKeyProviderResultSHA256 {
		return fmt.Errorf("wrapping-key result binding: %w", ErrForkNotAuthorized)
	}
	providerCompletedAt := e.DurableCompletionAt[EffectWrappingKeyRevoke]
	if verifyPortableCompletionWitness(boundary, e.WrappingKeySealerCompletionWitness, e.WrappingKeySealerRequestSHA256,
		e.WrappingKeySealerResultSHA256, e.WrappingKeySealerCompletionWitness.CompletedAt) != nil {
		return fmt.Errorf("wrapping-key sealer completion witness: %w", ErrForkNotAuthorized)
	}
	if verifyPortableCompletionWitness(boundary, e.WrappingKeyProviderCompletionWitness, e.WrappingKeyProviderRequestSHA256,
		e.WrappingKeyProviderResultSHA256, providerCompletedAt) != nil {
		return fmt.Errorf("wrapping-key provider completion witness: %w", ErrForkNotAuthorized)
	}
	if e.WrappingKeySealerCompletionWitness.Sequence >= e.WrappingKeyProviderCompletionWitness.Sequence {
		return fmt.Errorf("wrapping-key completion sequence: %w", ErrForkNotAuthorized)
	}
	if e.WrappingKeySealerCompletionWitness.CompletedAt.Before(e.WrappingKeySealerStatus.ObservedAt) ||
		e.WrappingKeyRevokedAt.Before(e.WrappingKeySealerCompletionWitness.CompletedAt) ||
		providerCompletedAt.Before(e.WrappingKeyRevokedAt) {
		return fmt.Errorf("wrapping-key completion chronology: %w", ErrForkNotAuthorized)
	}
	return nil
}

func (e WriterTerminalEvidence) CanonicalBytes() ([]byte, error) {
	facts, err := e.canonicalFactsBytes()
	if err != nil {
		return nil, err
	}
	if err := verifyWriterTerminalObservationReceipt(e.ProviderObservation, HashBytes(facts)); err != nil {
		return nil, err
	}
	// A boundary-independent canonical digest carries the full signed witness
	// material. The pinned two-root verification occurs in every outer receipt
	// verifier before the evidence is consumable.
	durable, err := e.durableCompletionBytes()
	if err != nil {
		return nil, err
	}
	return []byte("writer-terminal-evidence/v4\x00" + string(facts) + "\x00" + e.ProviderObservation.Digest().String() + "\x00" + string(durable)), nil
}
func (e WriterTerminalEvidence) Digest() (Digest, error) {
	b, err := e.CanonicalBytes()
	if err != nil {
		return Digest{}, err
	}
	return HashBytes(b), nil
}
func (e WriterTerminalEvidence) Validate() error { _, err := e.CanonicalBytes(); return err }

type WriterTerminalReceipt struct {
	Version, OperationID, Phase                                                                     string
	Generation                                                                                      uint64
	OperationBodySHA256, MarkerSHA256, SourceTargetSHA256, EvidenceSHA256, OriginalProjectionSHA256 Digest
	WriterAuthoritySHA256, WriterBrokerSHA256, WriterLedgerSHA256, WriterRootSHA256, BoundarySHA256 Digest
	Signature                                                                                       []byte
}

func (r WriterTerminalReceipt) payload() []byte {
	return []byte("writer-terminal/v2\x00" + r.Version + "\x00" + r.OperationID + "\x00" + strconv.FormatUint(r.Generation, 10) + "\x00" + r.Phase + "\x00" + r.OperationBodySHA256.String() + "\x00" + r.MarkerSHA256.String() + "\x00" + r.SourceTargetSHA256.String() + "\x00" + r.EvidenceSHA256.String() + "\x00" + r.OriginalProjectionSHA256.String() + "\x00" + r.WriterAuthoritySHA256.String() + "\x00" + r.WriterBrokerSHA256.String() + "\x00" + r.WriterLedgerSHA256.String() + "\x00" + r.WriterRootSHA256.String() + "\x00" + r.BoundarySHA256.String())
}

func terminalReceiptDigest(receipt WriterTerminalReceipt) Digest {
	return HashBytes(append(receipt.payload(), receipt.Signature...))
}

func verifyTerminalDeleteCompletion(boundary *ClosedBoundary, evidence WriterTerminalEvidence) error {
	if boundary == nil {
		return ErrForkNotAuthorized
	}
	witness := evidence.DeletionCompletionWitness
	if evidence.DeletionRequestSHA256.IsZero() || witness.Version != ProtocolVersion || witness.Role != WriterAuthorityRole ||
		witness.Effect != EffectBrokerDelete || witness.State != MutationCompleted || witness.RequestSHA256 != evidence.DeletionRequestSHA256 ||
		witness.ResultSHA256 != evidence.Deletion.DeleteActionSHA256 || !witness.TerminalSHA256.IsZero() ||
		witness.LedgerSHA256 != boundary.writerLedger || witness.RootSHA256 != boundary.writerRoot || witness.OracleSHA256 != boundary.writerStore ||
		witness.Sequence == 0 || len(witness.Signature) != ed25519.SignatureSize ||
		!ed25519.Verify(boundary.writerPublic, witness.payload(), witness.Signature) {
		return ErrForkNotAuthorized
	}
	return nil
}

func VerifyWriterTerminalReceipt(boundary *ClosedBoundary, receipt WriterTerminalReceipt, evidence WriterTerminalEvidence) error {
	evidenceDigest, err := evidence.Digest()
	if err != nil {
		return err
	}
	projectionDigest, _ := evidence.OriginalProjection.Digest()
	facts, factsErr := evidence.canonicalFactsBytes()
	if boundary == nil || receipt.Version != ProtocolVersion || !validSyntheticToken(receipt.OperationID) ||
		receipt.Generation == 0 || !validPhase(receipt.Phase) || receipt.OperationBodySHA256.IsZero() || receipt.MarkerSHA256.IsZero() || receipt.SourceTargetSHA256.IsZero() ||
		factsErr != nil || verifyPinnedWriterTerminalObservation(boundary, evidence.ProviderObservation, HashBytes(facts)) != nil || verifyWrappingKeyLifecycleProof(boundary, evidence) != nil ||
		verifyTerminalDeleteCompletion(boundary, evidence) != nil ||
		receipt.EvidenceSHA256 != evidenceDigest || receipt.OriginalProjectionSHA256 != projectionDigest || receipt.WriterAuthoritySHA256 != boundary.writerAuthority.IdentityDigest() || receipt.WriterBrokerSHA256 != boundary.writerBroker.IdentityDigest() || receipt.WriterLedgerSHA256 != boundary.writerLedger || receipt.WriterRootSHA256 != boundary.writerRoot || receipt.BoundarySHA256 != boundary.digest || len(receipt.Signature) != ed25519.SignatureSize || !ed25519.Verify(boundary.writerPublic, receipt.payload(), receipt.Signature) {
		return ErrForkNotAuthorized
	}
	return nil
}

func derivedEffectRequest(base OperationBody, projection Digest, effect Effect, parameters Digest) MutationRequest {
	baseDigest, _ := base.Digest()
	suffix := HashString("derived-effect/v2\x00" + baseDigest.String() + "\x00" + string(effect)).String()[:20]
	body := OperationBody{Version: ProtocolVersion, OperationID: "synthetic-effect-" + suffix, Generation: base.Generation, Phase: base.Phase, ConfigDigest: HashString("derived-config/v2\x00" + baseDigest.String() + "\x00" + projection.String())}
	return MutationRequest{Operation: body, Effect: effect, ParametersDigest: parameters}
}

type WriterLifecycle struct {
	writer           *WriterProtocol
	provider         WriterLifecycleProvider
	observer         WriterTerminalObservationAdapter
	base             OperationBody
	projection       PreOperationProjection
	projectionDigest Digest
	capability       Digest
}

var writerTeardownEffects = []Effect{
	EffectCapabilityRevoke, EffectLeafRevoke, EffectMTLSRevoke, EffectWrappingKeyRevoke,
	EffectBindingRemove, EffectCredentialRemove, EffectFullRedeploy, EffectTrustedSourceDel,
}

// requestCompletedLocked is used only from durable-oracle callbacks, which
// already hold the oracle mutex. Calling the public Completed helper there
// would attempt to acquire the same non-reentrant lock.
func (l *WriterLifecycle) requestCompletedLocked(effect Effect) bool {
	record, ok := l.writer.effects.recordForRequestLocked(l.request(effect))
	return ok && record.State == MutationCompleted
}

func (l *WriterLifecycle) predecessorCompleteLocked(effect Effect) bool {
	for index, candidate := range writerTeardownEffects {
		if candidate != effect {
			continue
		}
		return index == 0 || l.requestCompletedLocked(writerTeardownEffects[index-1])
	}
	return false
}

func (l *WriterLifecycle) allPreDeleteCompleteLocked() bool {
	for _, effect := range writerTeardownEffects {
		if !l.requestCompletedLocked(effect) {
			return false
		}
	}
	return true
}

func NewWriterLifecycle(writer *WriterProtocol, base OperationBody, projection PreOperationProjection, provider WriterLifecycleProvider, observer WriterTerminalObservationAdapter) (*WriterLifecycle, error) {
	if writer == nil || provider == nil || observer == nil {
		return nil, ErrInvalid
	}
	bodyDigest, err := base.Digest()
	if err != nil {
		return nil, err
	}
	capability, err := writer.capability(base)
	if err != nil || !writer.effects.Completed(markerRequest(base, writer.broker.target)) {
		return nil, ErrInvalid
	}
	projectionDigest, err := projection.Digest()
	if err != nil {
		return nil, err
	}
	if !provider.IsTestOracleOnly() || !observer.IsTestOracleOnly() {
		return nil, ErrProductionAdapter
	}
	if provider.IdentitySHA256().IsZero() || provider.ProviderConfigSHA256().IsZero() || observer.IdentitySHA256().IsZero() ||
		provider.OperationBodySHA256() != bodyDigest || provider.SourceTargetSHA256() != writer.broker.target || provider.OriginalProjectionSHA256() != projectionDigest ||
		observer.OperationBodySHA256() != bodyDigest || observer.SourceTargetSHA256() != writer.broker.target || observer.ProviderConfigSHA256() != provider.ProviderConfigSHA256() ||
		observer.OriginalProjectionSHA256() != projectionDigest || observer.MutationProviderSHA256() != provider.IdentitySHA256() ||
		observer.IdentitySHA256() != writer.boundary.providerObserver {
		return nil, ErrRoleIsolation
	}
	clockBinder, ok := observer.(interface{ bindAuthorityClock(func() time.Time) error })
	if !ok || clockBinder.bindAuthorityClock(writer.effects.now) != nil {
		return nil, ErrRoleIsolation
	}
	if err := writer.effects.BindProjection(base, projectionDigest); err != nil {
		return nil, err
	}
	return &WriterLifecycle{writer: writer, provider: provider, observer: observer, base: base, projection: projection, projectionDigest: projectionDigest, capability: capability.Digest()}, nil
}

// NewWriterLifecycleContinuation reconstructs only the post-sealer lifecycle
// controller. It deliberately accepts no source marker store or marker entropy
// and returns no WriterProtocol, so a terminal key service cannot be converted
// back into source mutation authority. The exact admission provider binding is
// needed solely to re-derive the already-completed capability request.
func NewWriterLifecycleContinuation(
	boundary *ClosedBoundary,
	effects *ProtocolCrashOracle,
	authority *WriterAuthority,
	brokerUnit SubstrateUnit,
	sealer MarkerSealer,
	admissionProvider WriterProviderEffects,
	base OperationBody,
	projection PreOperationProjection,
	provider WriterLifecycleProvider,
	observer WriterTerminalObservationAdapter,
) (*WriterLifecycle, error) {
	if boundary == nil || effects == nil || authority == nil || sealer == nil || admissionProvider == nil || provider == nil || observer == nil ||
		!effects.validInstance() || effects.identity != boundary.writerStore || !sealer.IsTestOracleOnly() || sealer.BindingSHA256().IsZero() ||
		!admissionProvider.IsTestOracleOnly() || admissionProvider.TargetSHA256().IsZero() || admissionProvider.TargetSHA256() != provider.SourceTargetSHA256() {
		return nil, ErrRoleIsolation
	}
	if _, err := markerSealerTerminalStatus(sealer); err != nil {
		return nil, ErrRoleIsolation
	}
	public := authority.privateKey.Public().(ed25519.PublicKey)
	if err := boundary.validateWriter(authority.unit, brokerUnit, public); err != nil {
		return nil, err
	}
	clockBinder, ok := sealer.(interface{ bindClock(func() time.Time) error })
	if !ok || clockBinder.bindClock(effects.now) != nil ||
		effects.bindSigner(WriterAuthorityRole, authority.privateKey, boundary.writerLedger, boundary.writerRoot) != nil {
		return nil, ErrRoleIsolation
	}
	broker := &WriterBroker{
		unit: brokerUnit, mtlsPeer: HashString("mtls-peer/v2\x00" + brokerUnit.rawIdentifier),
		target: admissionProvider.TargetSHA256(), sealer: sealer, now: effects.now,
		authority: authority.unit.IdentityDigest(), public: append(ed25519.PublicKey(nil), public...),
	}
	writer := &WriterProtocol{boundary: boundary, effects: effects, authority: authority, broker: broker, admissionProvider: admissionProvider}
	lifecycle, err := NewWriterLifecycle(writer, base, projection, provider, observer)
	if err != nil {
		return nil, err
	}
	if _, complete := effects.result(lifecycle.wrappingKeySealerRequest()); !complete {
		return nil, ErrForkNotAuthorized
	}
	return lifecycle, nil
}
func (l *WriterLifecycle) request(effect Effect) MutationRequest {
	parameters := HashString("lifecycle-effect/v3\x00" + l.projectionDigest.String() + "\x00" + l.capability.String() + "\x00" + l.provider.IdentitySHA256().String() + "\x00" + l.provider.SourceTargetSHA256().String() + "\x00" + l.provider.ProviderConfigSHA256().String() + "\x00" + l.observer.IdentitySHA256().String() + "\x00" + string(effect))
	return derivedEffectRequest(l.base, l.projectionDigest, effect, parameters)
}
func (l *WriterLifecycle) applyLocalState(effect Effect) error {
	switch effect {
	case EffectCapabilityRevoke:
		l.writer.effects.revokedCapabilities[l.capability] = true
	}
	return nil
}

func verifyLifecycleSealerTerminalResult(boundary *ClosedBoundary, providerIdentity, providerConfig, sealerBinding, requestDigest Digest, intent lifecycleSealerRevocationIntent, status MarkerSealerRevocationStatus) (Digest, error) {
	statusDigest, statusErr := status.Digest()
	if boundary == nil || statusErr != nil || requestDigest.IsZero() || providerIdentity.IsZero() || providerConfig.IsZero() || sealerBinding.IsZero() ||
		intent.Version != ProtocolVersion || intent.RequestSHA256 != requestDigest || intent.SealerSHA256 != sealerBinding ||
		intent.ProviderSHA256 != providerIdentity || intent.ProviderConfigSHA256 != providerConfig ||
		intent.LedgerSHA256 != boundary.writerLedger || intent.RootSHA256 != boundary.writerRoot || intent.OracleSHA256 != boundary.writerStore ||
		len(intent.Signature) != ed25519.SignatureSize || !ed25519.Verify(boundary.writerPublic, intent.payload(), intent.Signature) ||
		status.BindingSHA256 != intent.SealerSHA256 || status.ObservedAt.Before(intent.AuthorizedAt) {
		return Digest{}, ErrQuarantined
	}
	return HashString("writer-lifecycle-sealer-result/v2\x00" + intent.Digest().String() + "\x00" + statusDigest.String()), nil
}

func (l *WriterLifecycle) lifecycleSealerTerminalResult(request MutationRequest, intent lifecycleSealerRevocationIntent, status MarkerSealerRevocationStatus) (Digest, error) {
	requestDigest, err := request.Digest()
	if err != nil || l == nil || l.writer == nil || l.writer.boundary == nil {
		return Digest{}, ErrQuarantined
	}
	return verifyLifecycleSealerTerminalResult(l.writer.boundary, l.provider.IdentitySHA256(), l.provider.ProviderConfigSHA256(),
		l.writer.broker.sealer.BindingSHA256(), requestDigest, intent, status)
}

func lifecycleSealerProviderResult(sealerResult, providerResult Digest) (Digest, error) {
	if sealerResult.IsZero() || providerResult.IsZero() {
		return Digest{}, ErrQuarantined
	}
	return HashString("writer-lifecycle-sealer-provider-result/v3\x00" + sealerResult.String() + "\x00" + providerResult.String()), nil
}

// wrappingKeySealerRequest is a distinct durable mutation from the provider
// wrapping-key configuration effect. They intentionally share the public
// semantic effect label while using different operation identities, records,
// completion witnesses, and fresh authentication envelopes.
func (l *WriterLifecycle) wrappingKeySealerRequest() MutationRequest {
	baseDigest, _ := l.base.Digest()
	body := OperationBody{
		Version: ProtocolVersion, OperationID: "synthetic-sealer-revoke-" + baseDigest.String()[:20],
		Generation: l.base.Generation, Phase: l.base.Phase,
		ConfigDigest: HashString("lifecycle-sealer-config/v3\x00" + baseDigest.String() + "\x00" + l.projectionDigest.String()),
	}
	parameters := HashString("lifecycle-sealer-effect/v3\x00" + l.projectionDigest.String() + "\x00" + l.capability.String() + "\x00" +
		l.writer.broker.sealer.BindingSHA256().String() + "\x00" + l.provider.IdentitySHA256().String() + "\x00" + l.provider.ProviderConfigSHA256().String())
	return MutationRequest{Operation: body, Effect: EffectWrappingKeyRevoke, ParametersDigest: parameters}
}

func (l *WriterLifecycle) wrappingKeySealerCompleteLocked() (Digest, bool) {
	record, ok := l.writer.effects.recordForRequestLocked(l.wrappingKeySealerRequest())
	return record.Result, ok && record.State == MutationCompleted && !record.Result.IsZero()
}

// recordWrappingKeySealerRevocation performs only the key-service mutation.
// Provider configuration is a separate durable request and cannot start until
// this request has a terminal signed completion witness.
func (l *WriterLifecycle) recordWrappingKeySealerRevocation(ctx context.Context, auth AuthEnvelope, fault FaultMode) error {
	request := l.wrappingKeySealerRequest()
	check := func() error {
		if !l.requestCompletedLocked(EffectMTLSRevoke) {
			return ErrForkNotAuthorized
		}
		return nil
	}
	_, err := l.writer.effects.authorizePreparedAndConsumeChecked(ctx, request, auth, fault, Digest{}, check, func() error {
		_, prepareErr := l.writer.effects.registerLifecycleSealerRevocationIntentLocked(request, l.writer.broker.sealer.BindingSHA256(), l.provider.IdentitySHA256(), l.provider.ProviderConfigSHA256())
		return prepareErr
	}, func() (Digest, error) {
		if !markerSealerActive(l.writer.broker.sealer) {
			return Digest{}, ErrQuarantined
		}
		if err := l.writer.broker.sealer.Revoke(); err != nil {
			return Digest{}, err
		}
		status, statusErr := markerSealerTerminalStatus(l.writer.broker.sealer)
		if statusErr != nil {
			return Digest{}, statusErr
		}
		intent, ok := l.writer.effects.preparedLifecycleSealerRevocations[request.key()]
		if !ok {
			return Digest{}, ErrQuarantined
		}
		return l.lifecycleSealerTerminalResult(request, intent, status)
	})
	return err
}

func (l *WriterLifecycle) reconcileWrappingKeySealerRevocation(ctx context.Context, auth AuthEnvelope) error {
	request := l.wrappingKeySealerRequest()
	_, err := l.writer.effects.reconcileStatusChecked(ctx, request, auth, func() error {
		if !l.requestCompletedLocked(EffectMTLSRevoke) {
			return ErrForkNotAuthorized
		}
		return nil
	}, func() ([]Digest, error) {
		status, statusErr := markerSealerTerminalStatus(l.writer.broker.sealer)
		if errors.Is(statusErr, ErrNotObserved) {
			return nil, nil
		}
		if statusErr != nil {
			return nil, statusErr
		}
		intent, ok := l.writer.effects.preparedLifecycleSealerRevocations[request.key()]
		if !ok {
			return nil, ErrQuarantined
		}
		result, resultErr := l.lifecycleSealerTerminalResult(request, intent, status)
		if resultErr != nil {
			return nil, resultErr
		}
		return []Digest{result}, nil
	})
	return err
}

func (l *WriterLifecycle) apply(ctx context.Context, effect Effect, auth AuthEnvelope, fault FaultMode) error {
	check := func() error {
		if effect != EffectBrokerDelete && !l.predecessorCompleteLocked(effect) {
			return ErrForkNotAuthorized
		}
		if effect == EffectBrokerDelete && !l.allPreDeleteCompleteLocked() {
			return ErrForkNotAuthorized
		}
		if effect == EffectWrappingKeyRevoke {
			if _, ok := l.wrappingKeySealerCompleteLocked(); !ok {
				return ErrForkNotAuthorized
			}
		}
		return nil
	}
	request := l.request(effect)
	apply := func() (Digest, error) {
		if err := l.applyLocalState(effect); err != nil {
			return Digest{}, err
		}
		if effect == EffectWrappingKeyRevoke {
			sealerResult, ok := l.wrappingKeySealerCompleteLocked()
			if !ok {
				return Digest{}, ErrForkNotAuthorized
			}
			providerResult, providerErr := l.provider.ApplyConfigured(effect)
			if providerErr != nil {
				return Digest{}, providerErr
			}
			return lifecycleSealerProviderResult(sealerResult, providerResult)
		}
		result, providerErr := l.provider.ApplyConfigured(effect)
		if providerErr != nil {
			return Digest{}, providerErr
		}
		return result, nil
	}
	_, err := l.writer.effects.authorizeAndConsumeChecked(ctx, request, auth, fault, Digest{}, check, apply)
	return err
}
func (l *WriterLifecycle) reconcile(ctx context.Context, effect Effect, auth AuthEnvelope) error {
	check := func() error {
		if effect != EffectBrokerDelete && !l.predecessorCompleteLocked(effect) {
			return ErrForkNotAuthorized
		}
		if effect == EffectBrokerDelete && !l.allPreDeleteCompleteLocked() {
			return ErrForkNotAuthorized
		}
		if effect == EffectWrappingKeyRevoke {
			if _, ok := l.wrappingKeySealerCompleteLocked(); !ok {
				return ErrForkNotAuthorized
			}
		}
		return nil
	}
	request := l.request(effect)
	_, err := l.writer.effects.reconcileStatusChecked(ctx, request, auth, check, func() ([]Digest, error) {
		providerResults, providerErr := l.provider.ObserveConfigured(effect)
		if providerErr != nil || effect != EffectWrappingKeyRevoke {
			return providerResults, providerErr
		}
		if len(providerResults) != 1 {
			return nil, nil
		}
		sealerResult, ok := l.wrappingKeySealerCompleteLocked()
		if !ok {
			return nil, ErrForkNotAuthorized
		}
		combined, combineErr := lifecycleSealerProviderResult(sealerResult, providerResults[0])
		if combineErr != nil {
			return nil, combineErr
		}
		return []Digest{combined}, nil
	})
	return err
}

// RecordWrappingKeySealerRevocation issues only the key-service revocation.
// A separate fresh RecordRevocation(RevokeWrappingKey, ...) call is required
// to mutate provider configuration after this durable stage completes.
func (l *WriterLifecycle) RecordWrappingKeySealerRevocation(ctx context.Context, auth AuthEnvelope, fault FaultMode) error {
	if l == nil || l.writer == nil || l.writer.effects == nil {
		return ErrInvalid
	}
	return l.recordWrappingKeySealerRevocation(ctx, auth, fault)
}

// ReconcileWrappingKeySealerRevocation is status-only and never invokes the
// sealer or the provider mutation adapter.
func (l *WriterLifecycle) ReconcileWrappingKeySealerRevocation(ctx context.Context, auth AuthEnvelope) error {
	if l == nil || l.writer == nil || l.writer.effects == nil {
		return ErrInvalid
	}
	return l.reconcileWrappingKeySealerRevocation(ctx, auth)
}
func (l *WriterLifecycle) RecordRevocation(ctx context.Context, kind RevocationKind, auth AuthEnvelope, fault FaultMode) error {
	effect, ok := revocationEffect(kind)
	if !ok {
		if l != nil && l.writer != nil && l.writer.effects != nil {
			return l.writer.effects.rejectAfterAuth(ctx, auth, ErrInvalid)
		}
		return ErrInvalid
	}
	return l.apply(ctx, effect, auth, fault)
}
func (l *WriterLifecycle) ReconcileRevocation(ctx context.Context, kind RevocationKind, auth AuthEnvelope) error {
	effect, ok := revocationEffect(kind)
	if !ok {
		if l != nil && l.writer != nil && l.writer.effects != nil {
			return l.writer.effects.rejectAfterAuth(ctx, auth, ErrInvalid)
		}
		return ErrInvalid
	}
	return l.reconcile(ctx, effect, auth)
}
func (l *WriterLifecycle) RecordFullRedeploy(ctx context.Context, auth AuthEnvelope, fault FaultMode) error {
	return l.apply(ctx, EffectFullRedeploy, auth, fault)
}
func (l *WriterLifecycle) ReconcileFullRedeploy(ctx context.Context, auth AuthEnvelope) error {
	return l.reconcile(ctx, EffectFullRedeploy, auth)
}
func (l *WriterLifecycle) RecordFirewallRestored(ctx context.Context, auth AuthEnvelope, fault FaultMode) error {
	return l.apply(ctx, EffectTrustedSourceDel, auth, fault)
}
func (l *WriterLifecycle) ReconcileFirewallRestored(ctx context.Context, auth AuthEnvelope) error {
	return l.reconcile(ctx, EffectTrustedSourceDel, auth)
}

func (l *WriterLifecycle) allPreDeleteComplete() bool {
	for _, effect := range writerTeardownEffects {
		if !l.writer.effects.Completed(l.request(effect)) {
			return false
		}
	}
	return true
}

func (l *WriterLifecycle) terminalCompletionTimesMatch(e WriterTerminalEvidence) bool {
	checks := []struct {
		effect Effect
		claim  time.Time
	}{
		{EffectCapabilityRevoke, e.CapabilityRevokedAt}, {EffectLeafRevoke, e.LeafRevokedAt},
		{EffectMTLSRevoke, e.MTLSRevokedAt}, {EffectWrappingKeyRevoke, e.WrappingKeyRevokedAt},
		{EffectBindingRemove, e.BindingRemovedAt}, {EffectCredentialRemove, e.CredentialRemovedAt},
		{EffectFullRedeploy, e.FullRedeployAt}, {EffectTrustedSourceDel, e.FirewallRestoredAt},
		{EffectBrokerDelete, e.DeletionObservedAt},
	}
	for _, check := range checks {
		completedAt, ok := l.writer.effects.CompletionTime(l.request(check.effect))
		durableAt, durableOK := e.DurableCompletionAt[check.effect]
		if !ok || !durableOK || !completedAt.Equal(durableAt) || durableAt.Before(check.claim) || e.ReadOneAt.Before(durableAt) {
			return false
		}
	}
	return true
}
func (l *WriterLifecycle) RequestDelete(ctx context.Context, result DeleteResult, auth AuthEnvelope) error {
	fault := FaultNone
	switch result {
	case DeleteDefinitiveSuccess:
	case DeleteAmbiguous408:
		fault = FaultHTTP408AfterEffect
	case DeleteAmbiguousEOF:
		fault = FaultEOFAfterEffect
	case DeleteAmbiguous5xx:
		fault = Fault5xxAfterEffect
	default:
		if l != nil && l.writer != nil && l.writer.effects != nil {
			return l.writer.effects.rejectAfterAuth(ctx, auth, ErrInvalid)
		}
		return ErrInvalid
	}
	return l.apply(ctx, EffectBrokerDelete, auth, fault)
}
func (l *WriterLifecycle) ReconcileDelete(ctx context.Context, auth AuthEnvelope) error {
	return l.reconcile(ctx, EffectBrokerDelete, auth)
}

func (l *WriterLifecycle) terminalObservationRequest() MutationRequest {
	parameters := HashString("writer-terminal-observation/v3\x00" + l.projectionDigest.String() + "\x00" + l.capability.String() + "\x00" +
		l.provider.IdentitySHA256().String() + "\x00" + l.provider.ProviderConfigSHA256().String() + "\x00" +
		l.observer.IdentitySHA256().String())
	return derivedEffectRequest(l.base, l.projectionDigest, EffectEvidenceObserve, parameters)
}

func (l *WriterLifecycle) terminalObservationReadyLocked() error {
	if !l.allPreDeleteCompleteLocked() {
		return fmt.Errorf("terminal teardown incomplete: %w", ErrForkNotAuthorized)
	}
	if !l.requestCompletedLocked(EffectBrokerDelete) {
		return fmt.Errorf("terminal delete incomplete: %w", ErrForkNotAuthorized)
	}
	if !l.writer.effects.revokedCapabilities[l.capability] {
		return fmt.Errorf("terminal capability active: %w", ErrForkNotAuthorized)
	}
	return nil
}

func (l *WriterLifecycle) terminalCompletionTimesMatchLocked(e WriterTerminalEvidence) bool {
	checks := []struct {
		effect Effect
		claim  time.Time
	}{
		{EffectCapabilityRevoke, e.CapabilityRevokedAt}, {EffectLeafRevoke, e.LeafRevokedAt},
		{EffectMTLSRevoke, e.MTLSRevokedAt}, {EffectWrappingKeyRevoke, e.WrappingKeyRevokedAt},
		{EffectBindingRemove, e.BindingRemovedAt}, {EffectCredentialRemove, e.CredentialRemovedAt},
		{EffectFullRedeploy, e.FullRedeployAt}, {EffectTrustedSourceDel, e.FirewallRestoredAt},
		{EffectBrokerDelete, e.DeletionObservedAt},
	}
	for _, check := range checks {
		record, ok := l.writer.effects.recordForRequestLocked(l.request(check.effect))
		durableAt, durableOK := e.DurableCompletionAt[check.effect]
		if !ok || record.State != MutationCompleted || !durableOK || !record.CompletedAt.Equal(durableAt) || durableAt.Before(check.claim) || e.ReadOneAt.Before(durableAt) {
			return false
		}
	}
	return true
}

func (l *WriterLifecycle) validateTerminalStateLocked(e WriterTerminalEvidence) error {
	if err := l.terminalObservationReadyLocked(); err != nil {
		return fmt.Errorf("terminal readiness: %w", ErrForkNotAuthorized)
	}
	if !l.terminalCompletionTimesMatchLocked(e) {
		return fmt.Errorf("terminal durable completion times: %w", ErrForkNotAuthorized)
	}
	deleteRequest := l.request(EffectBrokerDelete)
	deleteRequestDigest, deleteRequestErr := deleteRequest.Digest()
	deleteRecord, deleteRecordOK := l.writer.effects.recordForRequestLocked(deleteRequest)
	deleteWitness, deleteWitnessOK := l.writer.effects.completionWitnesses[deleteRequest.key()]
	if deleteRequestErr != nil || !deleteRecordOK || deleteRecord.State != MutationCompleted || !deleteWitnessOK ||
		e.DeletionRequestSHA256 != deleteRequestDigest || e.Deletion.DeleteActionSHA256 != deleteRecord.Result ||
		e.DeletionCompletionWitness.Digest() != deleteWitness.Digest() || verifyTerminalDeleteCompletion(l.writer.boundary, e) != nil {
		return fmt.Errorf("terminal delete proof: %w", ErrForkNotAuthorized)
	}
	projectionDigest, err := e.OriginalProjection.Digest()
	if err != nil || projectionDigest != l.projectionDigest {
		return fmt.Errorf("terminal projection binding: %w", ErrForkNotAuthorized)
	}
	if err := e.Validate(); err != nil {
		return fmt.Errorf("terminal canonical evidence: %w", ErrForkNotAuthorized)
	}
	bodyDigest, _ := l.base.Digest()
	observation := e.ProviderObservation
	if observation.OperationBodySHA256 != bodyDigest || observation.SourceTargetSHA256 != l.writer.broker.target ||
		observation.ProviderConfigSHA256 != l.provider.ProviderConfigSHA256() || observation.OriginalProjectionSHA256 != l.projectionDigest ||
		observation.MutationProviderSHA256 != l.provider.IdentitySHA256() || observation.ObserverIdentitySHA256 != l.observer.IdentitySHA256() ||
		!observation.ReadOneAt.Equal(e.ReadOneAt) || !observation.ReadTwoAt.Equal(e.ReadTwoAt) {
		return fmt.Errorf("terminal observation binding: %w", ErrForkNotAuthorized)
	}
	facts, factsErr := e.canonicalFactsBytes()
	if factsErr != nil {
		return fmt.Errorf("terminal canonical facts: %w", ErrForkNotAuthorized)
	}
	if verifyPinnedWriterTerminalObservation(l.writer.boundary, observation, HashBytes(facts)) != nil {
		return fmt.Errorf("terminal protected-workflow observation: %w", ErrForkNotAuthorized)
	}
	if err := verifyWrappingKeyLifecycleProof(l.writer.boundary, e); err != nil {
		return err
	}
	return nil
}

func (l *WriterLifecycle) terminalObservationForResult(result Digest) (WriterTerminalEvidence, error) {
	evidence, ok := l.writer.effects.terminalObservation(result)
	if !ok {
		return WriterTerminalEvidence{}, ErrQuarantined
	}
	digest, err := evidence.Digest()
	if err != nil || digest != result || l.validateTerminalState(evidence) != nil {
		return WriterTerminalEvidence{}, ErrQuarantined
	}
	return evidence, nil
}

// ObserveTerminalEvidence is an issue-once endpoint. Fresh authentication is
// durably consumed before either provider GET; the exact protected-workflow
// two-read
// evidence is persisted before any response ambiguity can be returned.
func (l *WriterLifecycle) ObserveTerminalEvidence(ctx context.Context, auth AuthEnvelope, fault FaultMode) (WriterTerminalEvidence, error) {
	if l == nil || l.writer == nil || l.writer.effects == nil {
		return WriterTerminalEvidence{}, ErrForkNotAuthorized
	}
	request := l.terminalObservationRequest()
	terminal := HashString("writer-terminal-observation-terminal/v2\x00" + mustRequestDigest(l.request(EffectBrokerDelete)).String() + "\x00" + l.capability.String())
	result, err := l.writer.effects.authorizeAndConsumeChecked(ctx, request, auth, fault, terminal, l.terminalObservationReadyLocked, func() (Digest, error) {
		evidence, observeErr := l.observer.observeTerminal(auth.IssuedAt, auth.NotBefore, auth.ExpiresAt)
		if observeErr != nil {
			return Digest{}, observeErr
		}
		if evidence.ReadOneAt.Before(auth.IssuedAt) || evidence.ReadOneAt.Before(auth.NotBefore) || !evidence.ReadOneAt.Before(auth.ExpiresAt) ||
			evidence.ReadTwoAt.Before(auth.IssuedAt) || evidence.ReadTwoAt.Before(auth.NotBefore) || !evidence.ReadTwoAt.Before(auth.ExpiresAt) {
			return Digest{}, fmt.Errorf("terminal observation auth window: %w", ErrForkNotAuthorized)
		}
		evidence.DurableCompletionAt = make(map[Effect]time.Time, len(writerTeardownEffects)+1)
		for _, effect := range append(append([]Effect(nil), writerTeardownEffects...), EffectBrokerDelete) {
			effectRequest := l.request(effect)
			record, ok := l.writer.effects.recordForRequestLocked(effectRequest)
			if !ok || record.State != MutationCompleted {
				return Digest{}, fmt.Errorf("terminal durable record %s: %w", effect, ErrForkNotAuthorized)
			}
			evidence.DurableCompletionAt[effect] = record.CompletedAt
			if effect == EffectBrokerDelete {
				requestDigest, requestErr := effectRequest.Digest()
				witness, witnessOK := l.writer.effects.completionWitnesses[effectRequest.key()]
				if requestErr != nil || !witnessOK || record.Result != evidence.Deletion.DeleteActionSHA256 ||
					witness.RequestSHA256 != requestDigest || witness.ResultSHA256 != record.Result {
					return Digest{}, fmt.Errorf("terminal delete witness extraction: %w", ErrForkNotAuthorized)
				}
				evidence.DeletionRequestSHA256 = requestDigest
				evidence.DeletionCompletionWitness = cloneCompletionWitness(witness)
			}
		}
		sealerRequest := l.wrappingKeySealerRequest()
		providerRequest := l.request(EffectWrappingKeyRevoke)
		sealerRequestDigest, sealerRequestErr := sealerRequest.Digest()
		providerRequestDigest, providerRequestErr := providerRequest.Digest()
		sealerRecord, sealerRecordOK := l.writer.effects.recordForRequestLocked(sealerRequest)
		providerRecord, providerRecordOK := l.writer.effects.recordForRequestLocked(providerRequest)
		sealerWitness, sealerWitnessOK := l.writer.effects.completionWitnesses[sealerRequest.key()]
		providerWitness, providerWitnessOK := l.writer.effects.completionWitnesses[providerRequest.key()]
		intent, intentOK := l.writer.effects.preparedLifecycleSealerRevocations[sealerRequest.key()]
		status, statusErr := markerSealerTerminalStatus(l.writer.broker.sealer)
		if sealerRequestErr != nil || providerRequestErr != nil || !sealerRecordOK || !providerRecordOK ||
			sealerRecord.State != MutationCompleted || providerRecord.State != MutationCompleted ||
			!sealerWitnessOK || !providerWitnessOK || !intentOK || statusErr != nil {
			return Digest{}, fmt.Errorf("terminal wrapping witness extraction: %w", ErrForkNotAuthorized)
		}
		evidence.WrappingKeySealerRequestSHA256 = sealerRequestDigest
		evidence.WrappingKeySealerResultSHA256 = sealerRecord.Result
		evidence.WrappingKeyProviderRequestSHA256 = providerRequestDigest
		evidence.WrappingKeyProviderResultSHA256 = providerRecord.Result
		evidence.WrappingKeySealerIntent = cloneLifecycleSealerRevocationIntent(intent)
		evidence.WrappingKeySealerStatus = status
		evidence.WrappingKeySealerCompletionWitness = cloneCompletionWitness(sealerWitness)
		evidence.WrappingKeyProviderCompletionWitness = cloneCompletionWitness(providerWitness)
		if validateErr := l.validateTerminalStateLocked(evidence); validateErr != nil {
			return Digest{}, validateErr
		}
		return l.writer.effects.registerTerminalObservationLocked(request, evidence)
	})
	if err != nil {
		return WriterTerminalEvidence{}, err
	}
	return l.terminalObservationForResult(result)
}

// ReconcileTerminalObservation is status-only. It never calls the provider
// observer and cannot create or sign terminal evidence.
func (l *WriterLifecycle) ReconcileTerminalObservation(ctx context.Context, auth AuthEnvelope) (WriterTerminalEvidence, error) {
	if l == nil || l.writer == nil || l.writer.effects == nil {
		return WriterTerminalEvidence{}, ErrForkNotAuthorized
	}
	request := l.terminalObservationRequest()
	result, err := l.writer.effects.reconcileStatusChecked(ctx, request, auth, l.terminalObservationReadyLocked, func() ([]Digest, error) {
		digest, ok := l.writer.effects.terminalObservationForRequestLocked(request)
		if !ok {
			return nil, nil
		}
		return []Digest{digest}, nil
	})
	if err != nil {
		return WriterTerminalEvidence{}, err
	}
	return l.terminalObservationForResult(result)
}

func (l *WriterLifecycle) terminalReceiptFromWriterReceipt(evidence WriterTerminalEvidence, writerReceipt WriterReceipt) (WriterTerminalReceipt, error) {
	evidenceDigest, err := evidence.Digest()
	if err != nil {
		return WriterTerminalReceipt{}, err
	}
	projectionDigest, _ := evidence.OriginalProjection.Digest()
	bodyDigest, _ := l.base.Digest()
	if writerReceipt.MarkerSHA256.IsZero() {
		return WriterTerminalReceipt{}, ErrForkNotAuthorized
	}
	r := WriterTerminalReceipt{Version: ProtocolVersion, OperationID: l.base.OperationID, Generation: l.base.Generation, Phase: l.base.Phase, OperationBodySHA256: bodyDigest, MarkerSHA256: writerReceipt.MarkerSHA256, SourceTargetSHA256: l.writer.broker.target, EvidenceSHA256: evidenceDigest, OriginalProjectionSHA256: projectionDigest, WriterAuthoritySHA256: l.writer.boundary.writerAuthority.IdentityDigest(), WriterBrokerSHA256: l.writer.boundary.writerBroker.IdentityDigest(), WriterLedgerSHA256: l.writer.boundary.writerLedger, WriterRootSHA256: l.writer.boundary.writerRoot, BoundarySHA256: l.writer.boundary.digest}
	r.Signature = ed25519.Sign(l.writer.authority.privateKey, r.payload())
	return r, nil
}

func (l *WriterLifecycle) terminalReceipt(evidence WriterTerminalEvidence) (WriterTerminalReceipt, error) {
	writerReceipt, ok := l.writer.effects.writerReceiptForRequest(markerRequest(l.base, l.writer.broker.target))
	if !ok {
		return WriterTerminalReceipt{}, ErrForkNotAuthorized
	}
	return l.terminalReceiptFromWriterReceipt(evidence, writerReceipt)
}
func (l *WriterLifecycle) validateTerminalState(e WriterTerminalEvidence) error {
	if !l.allPreDeleteComplete() || !l.writer.effects.Completed(l.request(EffectBrokerDelete)) ||
		!l.writer.effects.CapabilityRevoked(l.capability) || !l.terminalCompletionTimesMatch(e) {
		return ErrForkNotAuthorized
	}
	projectionDigest, err := e.OriginalProjection.Digest()
	if err != nil || projectionDigest != l.projectionDigest {
		return ErrForkNotAuthorized
	}
	if err := e.Validate(); err != nil {
		return err
	}
	deleteRequest := l.request(EffectBrokerDelete)
	deleteRequestDigest, deleteRequestErr := deleteRequest.Digest()
	deleteResult, deleteResultOK := l.writer.effects.result(deleteRequest)
	deleteWitness, deleteWitnessOK := l.writer.effects.completionWitness(deleteRequest)
	if deleteRequestErr != nil || !deleteResultOK || !deleteWitnessOK || e.DeletionRequestSHA256 != deleteRequestDigest ||
		e.Deletion.DeleteActionSHA256 != deleteResult || e.DeletionCompletionWitness.Digest() != deleteWitness.Digest() ||
		verifyTerminalDeleteCompletion(l.writer.boundary, e) != nil {
		return ErrForkNotAuthorized
	}
	bodyDigest, _ := l.base.Digest()
	observation := e.ProviderObservation
	if observation.OperationBodySHA256 != bodyDigest || observation.SourceTargetSHA256 != l.writer.broker.target ||
		observation.ProviderConfigSHA256 != l.provider.ProviderConfigSHA256() || observation.OriginalProjectionSHA256 != l.projectionDigest ||
		observation.MutationProviderSHA256 != l.provider.IdentitySHA256() || observation.ObserverIdentitySHA256 != l.observer.IdentitySHA256() ||
		!observation.ReadOneAt.Equal(e.ReadOneAt) || !observation.ReadTwoAt.Equal(e.ReadTwoAt) {
		return ErrForkNotAuthorized
	}
	facts, err := e.canonicalFactsBytes()
	if err != nil || verifyPinnedWriterTerminalObservation(l.writer.boundary, observation, HashBytes(facts)) != nil || verifyWrappingKeyLifecycleProof(l.writer.boundary, e) != nil {
		return ErrForkNotAuthorized
	}
	return nil
}
func (l *WriterLifecycle) AcceptTerminalEvidence(ctx context.Context, evidence WriterTerminalEvidence, auth AuthEnvelope, fault FaultMode) (WriterTerminalReceipt, error) {
	if l == nil || l.writer == nil || l.writer.effects == nil {
		return WriterTerminalReceipt{}, ErrForkNotAuthorized
	}
	evidenceDigest, digestErr := evidence.Digest()
	if digestErr != nil {
		return WriterTerminalReceipt{}, l.writer.effects.rejectAfterAuth(ctx, auth, digestErr)
	}
	req := derivedEffectRequest(l.base, l.projectionDigest, EffectEvidencePublish, evidenceDigest)
	result, err := l.writer.effects.authorizeAndConsumeChecked(ctx, req, auth, fault, Digest{}, func() error {
		return l.validateTerminalStateLocked(evidence)
	}, func() (Digest, error) {
		writerReceipt, ok := l.writer.effects.writerReceiptForRequestLocked(markerRequest(l.base, l.writer.broker.target))
		if !ok {
			return Digest{}, ErrForkNotAuthorized
		}
		receipt, receiptErr := l.terminalReceiptFromWriterReceipt(evidence, writerReceipt)
		if receiptErr != nil {
			return Digest{}, receiptErr
		}
		return l.writer.effects.registerTerminalReceiptLocked(req, receipt, evidenceDigest)
	})
	if err != nil {
		return WriterTerminalReceipt{}, err
	}
	receipt, ok := l.writer.effects.terminalReceiptForRequest(req)
	if !ok || result != terminalReceiptDigest(receipt) {
		return WriterTerminalReceipt{}, ErrQuarantined
	}
	return receipt, nil
}
func (l *WriterLifecycle) ReconcileTerminalEvidence(ctx context.Context, evidence WriterTerminalEvidence, auth AuthEnvelope) (WriterTerminalReceipt, error) {
	if l == nil || l.writer == nil || l.writer.effects == nil {
		return WriterTerminalReceipt{}, ErrForkNotAuthorized
	}
	evidenceDigest, digestErr := evidence.Digest()
	if digestErr != nil {
		return WriterTerminalReceipt{}, l.writer.effects.rejectAfterAuth(ctx, auth, digestErr)
	}
	req := derivedEffectRequest(l.base, l.projectionDigest, EffectEvidencePublish, evidenceDigest)
	result, err := l.writer.effects.reconcileStatusChecked(ctx, req, auth, func() error {
		return l.validateTerminalStateLocked(evidence)
	}, func() ([]Digest, error) {
		receipt, ok := l.writer.effects.terminalReceiptForRequestLocked(req)
		if !ok {
			return nil, nil
		}
		want := terminalReceiptDigest(receipt)
		if !l.writer.effects.terminalRegisteredLocked(want, receipt.OperationBodySHA256, evidenceDigest) {
			return nil, nil
		}
		return []Digest{want}, nil
	})
	if err != nil {
		return WriterTerminalReceipt{}, err
	}
	receipt, ok := l.writer.effects.terminalReceiptForRequest(req)
	if !ok || result != terminalReceiptDigest(receipt) {
		return WriterTerminalReceipt{}, ErrQuarantined
	}
	return receipt, nil
}

type ForkController struct {
	boundary             *ClosedBoundary
	effects              *ProtocolCrashOracle
	provider             RecoveryForkProvider
	observer             *SyntheticForkObserver
	sourceTarget         Digest
	recoveryTarget       Digest
	providerConfig       Digest
	terminal             WriterTerminalReceipt
	evidence             WriterTerminalEvidence
	base                 OperationBody
	request              MutationRequest
	authorizationRequest MutationRequest
	terminalDigest       Digest
}

func NewForkController(boundary *ClosedBoundary, effects *ProtocolCrashOracle, provider RecoveryForkProvider, observer *SyntheticForkObserver, base OperationBody, terminal WriterTerminalReceipt, evidence WriterTerminalEvidence) (*ForkController, error) {
	if boundary == nil || effects == nil || !effects.validInstance() || provider == nil || provider.SourceBindingSHA256().IsZero() || provider.RecoveryBindingSHA256().IsZero() || provider.ProviderConfigSHA256().IsZero() ||
		observer == nil || provider.SourceBindingSHA256() == provider.RecoveryBindingSHA256() || provider.SourceBindingSHA256() != terminal.SourceTargetSHA256 {
		return nil, ErrInvalid
	}
	if effects.identity != boundary.writerStore {
		return nil, ErrForkNotAuthorized
	}
	if err := VerifyWriterTerminalReceipt(boundary, terminal, evidence); err != nil {
		return nil, err
	}
	bodyDigest, err := base.Digest()
	if err != nil || bodyDigest != terminal.OperationBodySHA256 || base.OperationID != terminal.OperationID || base.Generation != terminal.Generation || base.Phase != terminal.Phase {
		return nil, ErrForkNotAuthorized
	}
	projectionDigest, _ := evidence.OriginalProjection.Digest()
	terminalDigest := terminalReceiptDigest(terminal)
	request := derivedEffectRequest(base, projectionDigest, EffectForkPOST,
		HashString("configured-fork-post/v2\x00"+terminalDigest.String()+"\x00"+provider.SourceBindingSHA256().String()+"\x00"+provider.RecoveryBindingSHA256().String()+"\x00"+provider.ProviderConfigSHA256().String()))
	requestDigest, requestErr := request.Digest()
	authorizationRequest, authorizationRequestErr := recoveryAdmissionRequest(base, projectionDigest, requestDigest)
	publishRequest := derivedEffectRequest(base, projectionDigest, EffectEvidencePublish, terminal.EvidenceSHA256)
	if requestErr != nil || authorizationRequestErr != nil || !effects.terminalPublished(terminalDigest, bodyDigest, terminal.EvidenceSHA256, publishRequest) {
		return nil, ErrForkNotAuthorized
	}
	return &ForkController{boundary: boundary, effects: effects, provider: provider, observer: observer,
		sourceTarget: provider.SourceBindingSHA256(), recoveryTarget: provider.RecoveryBindingSHA256(),
		providerConfig: provider.ProviderConfigSHA256(),
		terminal:       terminal, evidence: evidence, base: base, request: request, authorizationRequest: authorizationRequest, terminalDigest: terminalDigest}, nil
}
func (f *ForkController) providerBindingsMatch() bool {
	return f != nil && f.provider != nil && f.sourceTarget == f.provider.SourceBindingSHA256() &&
		f.recoveryTarget == f.provider.RecoveryBindingSHA256() && f.providerConfig == f.provider.ProviderConfigSHA256() &&
		!f.sourceTarget.IsZero() && !f.recoveryTarget.IsZero() && !f.providerConfig.IsZero() &&
		f.sourceTarget != f.recoveryTarget
}
func (f *ForkController) PostOnce(ctx context.Context, auth AuthEnvelope, fault FaultMode) (Digest, error) {
	if f == nil || f.effects == nil {
		return Digest{}, ErrForkNotAuthorized
	}
	return f.effects.authorizeAndConsumeChecked(ctx, f.request, auth, fault, f.terminalDigest, func() error {
		if !f.providerBindingsMatch() {
			return ErrForkNotAuthorized
		}
		return nil
	}, f.provider.CreateConfiguredFork)
}
func (f *ForkController) Reconcile(ctx context.Context, auth AuthEnvelope) (Digest, error) {
	if f == nil || f.effects == nil {
		return Digest{}, ErrForkNotAuthorized
	}
	return f.effects.reconcileStatusChecked(ctx, f.request, auth, func() error {
		if !f.providerBindingsMatch() {
			return ErrForkNotAuthorized
		}
		return nil
	}, f.provider.ObserveConfiguredForks)
}
func (f *ForkController) PostCount() int { return f.effects.IssueCount(f.request) }

func recoveryAdmissionRequest(base OperationBody, projection, forkRequest Digest) (MutationRequest, error) {
	if projection.IsZero() || forkRequest.IsZero() {
		return MutationRequest{}, ErrInvalid
	}
	purpose, err := purposeOperationBody(base, "recovery-admission")
	if err != nil {
		return MutationRequest{}, err
	}
	return MutationRequest{Operation: purpose, Effect: EffectEvidencePublish,
		ParametersDigest: HashString("recovery-admission-publish/v2\x00" + projection.String() + "\x00" + forkRequest.String())}, nil
}

func recoveryAdmissionRequestFromClaims(operationID string, generation uint64, phase string, projection, forkRequest Digest) (MutationRequest, error) {
	if projection.IsZero() || forkRequest.IsZero() {
		return MutationRequest{}, ErrInvalid
	}
	purpose, err := purposeOperationBodyFromClaims(operationID, generation, phase, "recovery-admission")
	if err != nil {
		return MutationRequest{}, err
	}
	return MutationRequest{Operation: purpose, Effect: EffectEvidencePublish,
		ParametersDigest: HashString("recovery-admission-publish/v2\x00" + projection.String() + "\x00" + forkRequest.String())}, nil
}

func recoveryAuthorizationTerminal(terminalBinding, forkRequest Digest) Digest {
	return HashString("recovery-admission-publication-terminal/v2\x00" + terminalBinding.String() + "\x00" + forkRequest.String())
}

func mustRequestDigest(request MutationRequest) Digest {
	digest, _ := request.Digest()
	return digest
}

func (f *ForkController) recoveryAdmissionReadyLocked() error {
	record, complete := f.effects.recordForRequestLocked(f.request)
	if !complete || record.State != MutationCompleted {
		return ErrForkNotAuthorized
	}
	if !f.providerBindingsMatch() {
		return ErrForkNotAuthorized
	}
	if record.Result.IsZero() {
		return ErrForkNotAuthorized
	}
	bodyDigest, err := f.base.Digest()
	if err != nil || bodyDigest != f.terminal.OperationBodySHA256 {
		return ErrForkNotAuthorized
	}
	if _, err := f.request.Digest(); err != nil {
		return err
	}
	if _, err := f.evidence.OriginalProjection.Digest(); err != nil {
		return err
	}
	_, ok := f.effects.completionWitnesses[f.request.key()]
	if !ok {
		return ErrForkNotAuthorized
	}
	return nil
}

// recoveryAdmissionAuthorizationLocked performs the exact provider GET,
// observation signing, and authorization construction only from the
// issue-once apply callback after fresh authentication has been durably
// consumed and the publication record has entered MutationStarted.
func (f *ForkController) recoveryAdmissionAuthorizationLocked() (RecoveryAdmissionAuthorization, error) {
	if err := f.recoveryAdmissionReadyLocked(); err != nil {
		return RecoveryAdmissionAuthorization{}, err
	}
	record, _ := f.effects.recordForRequestLocked(f.request)
	forkResult := record.Result
	bodyDigest, _ := f.base.Digest()
	forkRequestDigest, _ := f.request.Digest()
	projectionDigest, _ := f.evidence.OriginalProjection.Digest()
	forkCompletion := f.effects.completionWitnesses[f.request.key()]
	forkObservation, err := f.observer.observe(forkResult)
	if err != nil || verifyForkObservation(f.boundary, forkObservation, f.sourceTarget, f.recoveryTarget, f.providerConfig, forkResult) != nil {
		return RecoveryAdmissionAuthorization{}, ErrForkNotAuthorized
	}
	issuedAt := forkCompletion.CompletedAt
	if forkObservation.ObservedAt.After(issuedAt) {
		issuedAt = forkObservation.ObservedAt
	}
	authorization := RecoveryAdmissionAuthorization{
		version: ProtocolVersion, operationID: f.base.OperationID, generation: f.base.Generation, phase: f.base.Phase,
		operationBodySHA256: bodyDigest, projectionSHA256: projectionDigest, terminalBindingSHA256: f.terminalDigest,
		forkRequestSHA256: forkRequestDigest, forkResultSHA256: forkResult,
		forkProofSHA256: forkCompletionProof(f.terminalDigest, forkRequestDigest, forkResult), sourceTargetSHA256: f.sourceTarget,
		recoveryTargetSHA256: f.recoveryTarget, forkProviderSHA256: f.providerConfig,
		writerAuthoritySHA256: f.boundary.writerAuthority.IdentityDigest(), writerLedgerSHA256: f.boundary.writerLedger,
		writerRootSHA256: f.boundary.writerRoot, writerOracleSHA256: f.effects.identity,
		observerAuthoritySHA256: f.boundary.observerAuthority.IdentityDigest(), observerBrokerSHA256: f.boundary.observerBroker.IdentityDigest(),
		observerLedgerSHA256: f.boundary.observerLedger, observerRootSHA256: f.boundary.observerRoot,
		observerStoreSHA256: f.boundary.observerStore,
		boundarySHA256:      f.boundary.digest, forkCompletionWitness: forkCompletion, forkObservation: forkObservation,
		forkObservationSHA256: forkObservation.Digest(), issuedAt: issuedAt, authorizationNotAfter: issuedAt.Add(10 * time.Minute),
	}
	authorization.publicationRequest = f.authorizationRequest
	if authorization.publicationRequest.Validate() != nil {
		return RecoveryAdmissionAuthorization{}, ErrForkNotAuthorized
	}
	if err := verifyRecoveryAdmissionAuthorizationClaims(f.boundary, authorization); err != nil {
		return RecoveryAdmissionAuthorization{}, err
	}
	return authorization, nil
}

// AuthorizeRecoveryAdmission consumes the completed fork proof exactly once
// and publishes only an opaque hash-only authorization. It never receives or
// uses the observer signing key.
func (f *ForkController) AuthorizeRecoveryAdmission(ctx context.Context, auth AuthEnvelope, fault FaultMode) (RecoveryAdmissionAuthorization, error) {
	if f == nil || f.effects == nil {
		return RecoveryAdmissionAuthorization{}, ErrForkNotAuthorized
	}
	var authorization RecoveryAdmissionAuthorization
	result, err := f.effects.authorizeAndConsumeChecked(ctx, f.authorizationRequest, auth, fault,
		recoveryAuthorizationTerminal(f.terminalDigest, mustRequestDigest(f.request)), f.recoveryAdmissionReadyLocked, func() (Digest, error) {
			var authorizationErr error
			authorization, authorizationErr = f.recoveryAdmissionAuthorizationLocked()
			if authorizationErr != nil {
				return Digest{}, authorizationErr
			}
			if !f.effects.now().Before(authorization.authorizationNotAfter) {
				return Digest{}, ErrForkNotAuthorized
			}
			signature, signErr := f.effects.signLocked(WriterAuthorityRole, f.boundary.writerRoot, authorization.signingPayload())
			if signErr != nil {
				return Digest{}, signErr
			}
			authorization.writerSignature = signature
			if err := verifyRecoveryAdmissionAuthorizationSignature(f.boundary, authorization); err != nil {
				return Digest{}, err
			}
			return f.effects.registerRecoveryAuthorizationLocked(authorization.publicationRequest, authorization)
		})
	if err != nil {
		return RecoveryAdmissionAuthorization{}, err
	}
	return recoveryAuthorizationForResult(f.boundary, f.effects, result)
}

// ReconcileRecoveryAdmissionAuthorization is status-only. It can complete
// only an authorization that the original issue-once callback registered.
func (f *ForkController) ReconcileRecoveryAdmissionAuthorization(ctx context.Context, auth AuthEnvelope) (RecoveryAdmissionAuthorization, error) {
	if f == nil || f.effects == nil {
		return RecoveryAdmissionAuthorization{}, ErrForkNotAuthorized
	}
	result, err := f.effects.reconcileStatusChecked(ctx, f.authorizationRequest, auth, func() error {
		record, ok := f.effects.recordForRequestLocked(f.request)
		if !ok || record.State != MutationCompleted || !f.providerBindingsMatch() {
			return ErrForkNotAuthorized
		}
		return nil
	}, func() ([]Digest, error) {
		digest, ok := f.effects.recoveryAuthorizationForRequestLocked(f.authorizationRequest)
		if !ok {
			return nil, nil
		}
		return []Digest{digest}, nil
	})
	if err != nil {
		return RecoveryAdmissionAuthorization{}, err
	}
	return recoveryAuthorizationForResult(f.boundary, f.effects, result)
}
