package model

import (
	"bytes"
	"crypto/ed25519"
	"fmt"
)

// ClosedBoundary is the mandatory outer consumer for the four independent
// apps, two ledgers, and two signing roots. Raw identifiers remain private;
// only domain-separated hashes can enter receipts.
type ClosedBoundary struct {
	writerAuthority   SubstrateUnit
	writerBroker      SubstrateUnit
	observerAuthority SubstrateUnit
	observerBroker    SubstrateUnit
	writerLedger      Digest
	observerLedger    Digest
	writerStore       Digest
	observerStore     Digest
	writerRoot        Digest
	observerRoot      Digest
	providerObserver  Digest
	writerPublic      ed25519.PublicKey
	observerPublic    ed25519.PublicKey
	digest            Digest
}

// ObserverBoundary is the deliberately reduced trust view installed in the
// recovery-only domain. It contains no writer public key, writer root, writer
// ledger, writer store, or writer unit identity.
type ObserverBoundary struct {
	authority SubstrateUnit
	broker    SubstrateUnit
	ledger    Digest
	store     Digest
	root      Digest
	public    ed25519.PublicKey
	closed    Digest
}

func (b *ClosedBoundary) ObserverView() (*ObserverBoundary, error) {
	if b == nil || len(b.observerPublic) != ed25519.PublicKeySize {
		return nil, ErrRoleIsolation
	}
	return &ObserverBoundary{
		authority: b.observerAuthority, broker: b.observerBroker,
		ledger: b.observerLedger, store: b.observerStore, root: b.observerRoot,
		public: append(ed25519.PublicKey(nil), b.observerPublic...), closed: b.digest,
	}, nil
}

func NewClosedBoundary(
	writerAuthority, writerBroker, observerAuthority, observerBroker SubstrateUnit,
	writerLedgerID, observerLedgerID string,
	providerObserverID string,
	writerPublic, observerPublic ed25519.PublicKey,
) (*ClosedBoundary, error) {
	if err := RequireFourDistinctUnits(writerAuthority, writerBroker, observerAuthority, observerBroker); err != nil {
		return nil, err
	}
	if writerLedgerID == "" || observerLedgerID == "" || providerObserverID == "" ||
		len(writerPublic) != ed25519.PublicKeySize || len(observerPublic) != ed25519.PublicKeySize {
		return nil, ErrRoleIsolation
	}
	// Pairwise comparison uses the protected source values before hashing. It
	// prevents a ledger name or root byte string from aliasing any app ID.
	raw := [][]byte{
		[]byte(writerAuthority.rawIdentifier), []byte(writerBroker.rawIdentifier),
		[]byte(observerAuthority.rawIdentifier), []byte(observerBroker.rawIdentifier),
		[]byte(writerLedgerID), []byte(observerLedgerID), []byte(providerObserverID), writerPublic, observerPublic,
	}
	for i := range raw {
		if len(raw[i]) == 0 {
			return nil, ErrRoleIsolation
		}
		for j := 0; j < i; j++ {
			if bytes.Equal(raw[i], raw[j]) {
				return nil, ErrRoleIsolation
			}
		}
	}
	b := &ClosedBoundary{
		writerAuthority: writerAuthority, writerBroker: writerBroker,
		observerAuthority: observerAuthority, observerBroker: observerBroker,
		writerLedger:     HashString("writer-ledger/v2\x00" + writerLedgerID),
		observerLedger:   HashString("observer-ledger/v2\x00" + observerLedgerID),
		writerStore:      HashString("writer-durable-store/v2\x00" + writerLedgerID),
		observerStore:    HashString("observer-durable-store/v2\x00" + observerLedgerID),
		writerRoot:       HashBytes(append([]byte("writer-root/v2\x00"), writerPublic...)),
		observerRoot:     HashBytes(append([]byte("observer-root/v2\x00"), observerPublic...)),
		providerObserver: HashString("provider-observer-unit/v2\x00" + providerObserverID),
		writerPublic:     append(ed25519.PublicKey(nil), writerPublic...),
		observerPublic:   append(ed25519.PublicKey(nil), observerPublic...),
	}
	b.digest = HashString("closed-boundary/v2\x00" +
		writerAuthority.IdentityDigest().String() + "\x00" + writerBroker.IdentityDigest().String() + "\x00" +
		observerAuthority.IdentityDigest().String() + "\x00" + observerBroker.IdentityDigest().String() + "\x00" +
		b.writerLedger.String() + "\x00" + b.observerLedger.String() + "\x00" + b.writerStore.String() + "\x00" + b.observerStore.String() + "\x00" +
		b.writerRoot.String() + "\x00" + b.observerRoot.String() + "\x00" + b.providerObserver.String())
	return b, nil
}

func (b *ClosedBoundary) validateWriter(authority, broker SubstrateUnit, public ed25519.PublicKey) error {
	if b == nil || !authority.sameRuntime(b.writerAuthority) || !broker.sameRuntime(b.writerBroker) ||
		authority.Role() != WriterAuthorityRole || broker.Role() != WriterBrokerRole ||
		!bytes.Equal(public, b.writerPublic) {
		return ErrRoleIsolation
	}
	return nil
}

func (b *ObserverBoundary) validate(authority, broker SubstrateUnit, public ed25519.PublicKey) error {
	if b == nil || !authority.sameRuntime(b.authority) || !broker.sameRuntime(b.broker) ||
		authority.Role() != ObserverAuthorityRole || broker.Role() != ObserverBrokerRole ||
		!bytes.Equal(public, b.public) {
		return ErrRoleIsolation
	}
	return nil
}

func (b *ClosedBoundary) String() string {
	if b == nil {
		return "closed-boundary/<nil>"
	}
	return fmt.Sprintf("closed-boundary/%s", b.digest.String())
}
