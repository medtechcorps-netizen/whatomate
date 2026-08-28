package protocol

import (
	"crypto/ed25519"
	"encoding/binary"
	"errors"
	"fmt"
)

const (
	DomainWriterReceipt    = "writer-receipt/v1"
	DomainObserverReceipt  = "observer-receipt/v1"
	DomainLeafCertificate  = "leaf-certificate/v1"
	DomainBrokerCapability = "broker-capability/v1"
	DomainBrokerReadback   = "broker-readback/v1"
)

func signatureMessage(domain string, canonicalPayload []byte) ([]byte, error) {
	switch domain {
	case DomainWriterReceipt, DomainObserverReceipt, DomainLeafCertificate, DomainBrokerCapability, DomainBrokerReadback:
	default:
		return nil, fmt.Errorf("unapproved signature domain %q", domain)
	}
	if len(canonicalPayload) == 0 {
		return nil, errors.New("canonical payload is empty")
	}
	if len(domain) > 0xffff {
		return nil, errors.New("signature domain is too long")
	}
	prefix := []byte("rereply-recovery-boundary/signature/v1\x00")
	message := make([]byte, 0, len(prefix)+2+len(domain)+8+len(canonicalPayload))
	message = append(message, prefix...)
	var domainLength [2]byte
	binary.BigEndian.PutUint16(domainLength[:], uint16(len(domain)))
	message = append(message, domainLength[:]...)
	message = append(message, domain...)
	var payloadLength [8]byte
	binary.BigEndian.PutUint64(payloadLength[:], uint64(len(canonicalPayload)))
	message = append(message, payloadLength[:]...)
	message = append(message, canonicalPayload...)
	return message, nil
}

func SignCanonical(privateKey ed25519.PrivateKey, domain string, canonicalPayload []byte) ([]byte, error) {
	if len(privateKey) != ed25519.PrivateKeySize {
		return nil, errors.New("invalid Ed25519 private key")
	}
	message, err := signatureMessage(domain, canonicalPayload)
	if err != nil {
		return nil, err
	}
	return ed25519.Sign(privateKey, message), nil
}

func VerifyCanonical(publicKey ed25519.PublicKey, domain string, canonicalPayload, signature []byte) error {
	if len(publicKey) != ed25519.PublicKeySize {
		return errors.New("invalid Ed25519 public key")
	}
	if len(signature) != ed25519.SignatureSize {
		return errors.New("invalid Ed25519 signature size")
	}
	message, err := signatureMessage(domain, canonicalPayload)
	if err != nil {
		return err
	}
	if !ed25519.Verify(publicKey, message, signature) {
		return errors.New("Ed25519 signature verification failed")
	}
	return nil
}
