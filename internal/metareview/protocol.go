// Package metareview defines the staging-only credential broker protocol used
// by ReReply and the inbound-only Messenger App Review relay.
//
// The protocol deliberately carries no Meta Page access token and no relay
// outbound signing secret. ReReply decrypts only the selected account's inbound
// HMAC credential, seals it into a short-lived AEAD envelope, and returns only
// ciphertext to the relay. The browser-facing API never participates in this
// exchange.
package metareview

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
)

const (
	Version       = "v1"
	Mode          = "staging_messenger_review"
	Marker        = "staging_messenger_review_v1"
	ProvisionPath = "/api/internal/meta/messenger/review/provision"

	TimestampHeader         = "X-ReReply-Meta-Review-Timestamp"
	NonceHeader             = "X-ReReply-Meta-Review-Nonce"
	SignatureHeader         = "X-ReReply-Meta-Review-Signature-256"
	GenerationHeader        = "X-ReReply-Meta-Review-Generation"
	CredentialIDHeader      = "X-ReReply-Meta-Review-Credential-ID"
	CredentialVersionHeader = "X-ReReply-Meta-Review-Credential-Version"
	ReviewProofHeader       = "X-ReReply-Meta-Review-Proof-256"

	MaximumRequestBodyBytes  = 8 << 10
	MaximumResponseBodyBytes = 16 << 10
	MaximumInboundBodyBytes  = 2 << 20
	MaximumRequestSkew       = 60 * time.Second
	MaximumBundleTTL         = 5 * time.Minute
	minimumRootSecretBytes   = 32
	inboundSecretMinBytes    = 32
	inboundSecretMaxBytes    = 4 << 10
	maximumWebhookURLBytes   = 2 << 10
	nonceBytes               = 32
	signaturePrefix          = "sha256="

	requestAuthDomain              = "rereply-meta-review-provision-request:v1"
	bundleAEADDomain               = "rereply-meta-review-provision-bundle:v1"
	keyIDDomain                    = "rereply-meta-review-provision-key-id:v1"
	messengerAppSecretKeyIDDomain  = "rereply-meta-review-messenger-app-secret-key-id:v1"
	providerProofSecretKeyIDDomain = "rereply-meta-review-provider-proof-secret-key-id:v1"
	inboundProofDomain             = "rereply-meta-review-inbound-proof:v1"
	authHKDFSaltDomain             = "rereply-meta-review-auth-hkdf:v1"
	wrapHKDFSaltDomain             = "rereply-meta-review-wrap-hkdf:v1"
	providerHKDFSaltDomain         = "rereply-meta-review-provider-hkdf:v1"
)

var (
	ErrInvalidRootSecret  = errors.New("Meta review broker root secret is invalid")
	ErrInvalidRequest     = errors.New("Meta review provision request is invalid")
	ErrRequestExpired     = errors.New("Meta review provision request is outside the allowed time window")
	ErrInvalidSignature   = errors.New("Meta review provision signature is invalid")
	ErrInvalidResponse    = errors.New("Meta review provision response is invalid")
	ErrInvalidBundle      = errors.New("Meta review credential bundle is invalid")
	ErrInvalidProof       = errors.New("Meta review inbound proof is invalid")
	ErrInvalidProofSecret = errors.New("Meta review provider-proof secret is invalid")
)

// ProvisionTuple is deployment-owned authority. Every field must match on the
// relay, broker, managed ChannelAccount, and encrypted credential row before a
// credential bundle is issued or accepted.
type ProvisionTuple struct {
	OrganizationID   string `json:"organization_id"`
	MetaBusinessID   string `json:"meta_business_id"`
	PageID           string `json:"page_id"`
	MetaAppID        string `json:"meta_app_id"`
	ChannelAccountID string `json:"channel_account_id"`
	Generation       string `json:"generation"`
	ExpiresAt        string `json:"expires_at"`
}

// ProvisionRequest asks ReReply for the exact deployment-pinned review
// binding. The exact encoded bytes are authenticated; callers must sign the
// bytes they send rather than re-encoding after signing.
type ProvisionRequest struct {
	Version                  string         `json:"version"`
	Mode                     string         `json:"mode"`
	Tuple                    ProvisionTuple `json:"tuple"`
	MessengerAppSecretKeyID  string         `json:"messenger_app_secret_key_id"`
	ProviderProofSecretKeyID string         `json:"provider_proof_secret_key_id"`
}

// ProvisionResponse contains ciphertext only. KeyID is a domain-separated
// HMAC fingerprint and does not reveal the broker root secret.
type ProvisionResponse struct {
	Version   string `json:"version"`
	Mode      string `json:"mode"`
	KeyID     string `json:"key_id"`
	IssuedAt  string `json:"issued_at"`
	ExpiresAt string `json:"expires_at"`
	Envelope  string `json:"envelope"`
}

// CredentialBundle is the only plaintext sealed by this package. It contains
// one inbound signing credential and never contains a Meta token, app secret,
// verify token, provider-proof secret, or outbound signing credential.
type CredentialBundle struct {
	Version                  string         `json:"version"`
	Mode                     string         `json:"mode"`
	Tuple                    ProvisionTuple `json:"tuple"`
	MessengerAppSecretKeyID  string         `json:"messenger_app_secret_key_id"`
	ProviderProofSecretKeyID string         `json:"provider_proof_secret_key_id"`
	CredentialID             string         `json:"credential_id"`
	CredentialVersion        int            `json:"credential_version"`
	ReReplyWebhookURL        string         `json:"rereply_webhook_url"`
	InboundSecret            string         `json:"inbound_secret"`
	RequestNonce             string         `json:"request_nonce"`
	IssuedAt                 string         `json:"issued_at"`
	ExpiresAt                string         `json:"expires_at"`
}

// Protocol holds only derived keys. Request authentication and bundle wrapping
// start from two independently generated deployment secrets; HKDF then keeps
// each purpose within those roots domain-separated.
type Protocol struct {
	authKey  []byte
	aeadKey  []byte
	keyIDKey []byte
	keyID    string
}

// NewProtocol validates two independent deployment-held roots and derives the
// protocol keys. Neither root may be a tenant setting or browser variable.
func NewProtocol(authSecret, wrapSecret string) (*Protocol, error) {
	if !validRootSecret(authSecret) || !validRootSecret(wrapSecret) ||
		constantTimeSecretEqual(authSecret, wrapSecret) {
		return nil, ErrInvalidRootSecret
	}
	derive := func(root, salt, info string) ([]byte, error) {
		return hkdf.Key(sha256.New, []byte(root), []byte(salt), info, 32)
	}
	authKey, err := derive(authSecret, authHKDFSaltDomain, requestAuthDomain)
	if err != nil {
		return nil, ErrInvalidRootSecret
	}
	aeadKey, err := derive(wrapSecret, wrapHKDFSaltDomain, bundleAEADDomain)
	if err != nil {
		return nil, ErrInvalidRootSecret
	}
	keyIDKey, err := derive(wrapSecret, wrapHKDFSaltDomain, keyIDDomain)
	if err != nil {
		return nil, ErrInvalidRootSecret
	}
	identifier := hmac.New(sha256.New, keyIDKey)
	_, _ = identifier.Write([]byte(keyIDDomain))
	return &Protocol{
		authKey:  authKey,
		aeadKey:  aeadKey,
		keyIDKey: keyIDKey,
		keyID:    signaturePrefix + hex.EncodeToString(identifier.Sum(nil)),
	}, nil
}

// KeyID returns a non-secret, domain-separated fingerprint used to reject a
// one-sided broker-key rotation without exposing either runtime's key.
func (p *Protocol) KeyID() string {
	if p == nil {
		return ""
	}
	return p.keyID
}

// MessengerAppSecretKeyID returns a non-secret, domain-separated fingerprint
// that lets the relay and ReReply attest that they hold the same Messenger App
// Secret without placing that secret on the wire.
func MessengerAppSecretKeyID(secret string) (string, error) {
	return sharedSecretKeyID(secret, messengerAppSecretKeyIDDomain)
}

// ProviderProofSecretKeyID returns a non-secret, domain-separated fingerprint
// that lets the relay and ReReply attest that they hold the same provider-proof
// secret without placing that secret on the wire.
func ProviderProofSecretKeyID(secret string) (string, error) {
	return sharedSecretKeyID(secret, providerProofSecretKeyIDDomain)
}

func sharedSecretKeyID(secret, domain string) (string, error) {
	if !validRootSecret(secret) || strings.TrimSpace(domain) == "" {
		return "", ErrInvalidRootSecret
	}
	identifier := hmac.New(sha256.New, []byte(secret))
	_, _ = identifier.Write([]byte(domain))
	return signaturePrefix + hex.EncodeToString(identifier.Sum(nil)), nil
}

// NewNonce returns a canonical base64url nonce with 256 bits of entropy.
func NewNonce() (string, error) {
	value := make([]byte, nonceBytes)
	if _, err := io.ReadFull(rand.Reader, value); err != nil {
		return "", errors.New("Meta review provision nonce could not be generated")
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}

// ReplayKey returns a non-secret Redis key suffix. The raw nonce is never
// needed in Redis for the SET NX replay gate.
func ReplayKey(nonce string) (string, error) {
	if err := validateNonce(nonce); err != nil {
		return "", err
	}
	digest := sha256.Sum256([]byte(nonce))
	return "meta-review:provision:nonce:" + hex.EncodeToString(digest[:]), nil
}

// EncodeProvisionRequest validates and deterministically encodes a request.
func EncodeProvisionRequest(request ProvisionRequest, now time.Time) ([]byte, error) {
	if err := ValidateProvisionRequest(request, now); err != nil {
		return nil, err
	}
	return json.Marshal(request)
}

// DecodeProvisionRequest strictly decodes a bounded request body.
func DecodeProvisionRequest(body []byte, now time.Time) (ProvisionRequest, error) {
	var request ProvisionRequest
	if err := decodeStrict(body, MaximumRequestBodyBytes, &request); err != nil {
		return request, ErrInvalidRequest
	}
	if err := ValidateProvisionRequest(request, now); err != nil {
		return request, err
	}
	return request, nil
}

// DecodeProvisionResponse strictly decodes a bounded ciphertext response.
func DecodeProvisionResponse(body []byte, now time.Time) (ProvisionResponse, error) {
	var response ProvisionResponse
	if err := decodeStrict(body, MaximumResponseBodyBytes, &response); err != nil {
		return response, ErrInvalidResponse
	}
	if _, _, err := validateResponse(response, now); err != nil {
		return response, err
	}
	return response, nil
}

// ValidateProvisionRequest requires the exact staging mode and a complete,
// canonical, unexpired deployment tuple.
func ValidateProvisionRequest(request ProvisionRequest, now time.Time) error {
	if request.Version != Version || request.Mode != Mode {
		return ErrInvalidRequest
	}
	if !validSignatureText(request.MessengerAppSecretKeyID) ||
		!validSignatureText(request.ProviderProofSecretKeyID) {
		return ErrInvalidRequest
	}
	if err := request.Tuple.Validate(now); err != nil {
		return ErrInvalidRequest
	}
	return nil
}

// Validate checks that every authority value is canonical and still current.
func (tuple ProvisionTuple) Validate(now time.Time) error {
	if err := tuple.validateStructure(); err != nil {
		return err
	}
	expiresAt, err := parseCanonicalUTC(tuple.ExpiresAt)
	if err != nil || !expiresAt.After(normalizeNow(now)) {
		return ErrInvalidRequest
	}
	return nil
}

// SignProvisionRequest authenticates the exact body and freshness headers for
// the fixed broker path. It never accepts an arbitrary method or URL.
func (p *Protocol) SignProvisionRequest(timestamp int64, nonce string, body []byte) (string, error) {
	if p == nil || len(p.authKey) != sha256.Size || timestamp <= 0 ||
		len(body) == 0 || len(body) > MaximumRequestBodyBytes {
		return "", ErrInvalidRequest
	}
	if err := validateNonce(nonce); err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, p.authKey)
	_, _ = mac.Write(canonicalRequest(timestamp, nonce, body))
	return signaturePrefix + hex.EncodeToString(mac.Sum(nil)), nil
}

// VerifyProvisionRequest authenticates before a handler touches Redis or the
// tenant database. A valid caller must then consume ReplayKey with SET NX.
func (p *Protocol) VerifyProvisionRequest(
	timestamp int64,
	nonce string,
	body []byte,
	signature string,
	now time.Time,
) error {
	if err := ValidateRequestTime(timestamp, now); err != nil {
		return err
	}
	expected, err := p.SignProvisionRequest(timestamp, nonce, body)
	if err != nil {
		return err
	}
	provided := strings.TrimSpace(signature)
	if len(provided) != len(expected) ||
		subtle.ConstantTimeCompare([]byte(provided), []byte(expected)) != 1 {
		return ErrInvalidSignature
	}
	return nil
}

// ValidateRequestTime applies the symmetric request freshness window.
func ValidateRequestTime(timestamp int64, now time.Time) error {
	if timestamp <= 0 {
		return ErrRequestExpired
	}
	requestTime := time.Unix(timestamp, 0).UTC()
	now = normalizeNow(now)
	if requestTime.Before(now.Add(-MaximumRequestSkew)) ||
		requestTime.After(now.Add(MaximumRequestSkew)) {
		return ErrRequestExpired
	}
	return nil
}

// SignInboundProof authenticates a canonical inbound body together with the
// complete deployment authority and exact encrypted credential generation.
// It is intentionally additional to, and domain-separated from, both the
// account-scoped webhook HMAC and the generic Meta provider proof.
func SignInboundProof(
	providerProofSecret string,
	tuple ProvisionTuple,
	credentialID string,
	credentialVersion int,
	canonicalBody []byte,
) (string, error) {
	if !validRootSecret(providerProofSecret) {
		return "", ErrInvalidProofSecret
	}
	if err := validateInboundProofInputs(tuple, credentialID, credentialVersion, canonicalBody); err != nil {
		return "", err
	}
	proofKey, err := hkdf.Key(
		sha256.New,
		[]byte(providerProofSecret),
		[]byte(providerHKDFSaltDomain),
		inboundProofDomain,
		sha256.Size,
	)
	if err != nil {
		return "", ErrInvalidProofSecret
	}
	mac := hmac.New(sha256.New, proofKey)
	_, _ = mac.Write(canonicalInboundProof(
		tuple,
		credentialID,
		credentialVersion,
		canonicalBody,
	))
	return signaturePrefix + hex.EncodeToString(mac.Sum(nil)), nil
}

// VerifyInboundProof performs a constant-time comparison against the exact
// deployment tuple, current credential row/version, and received body.
func VerifyInboundProof(
	providerProofSecret string,
	tuple ProvisionTuple,
	credentialID string,
	credentialVersion int,
	canonicalBody []byte,
	proof string,
) error {
	expected, err := SignInboundProof(
		providerProofSecret,
		tuple,
		credentialID,
		credentialVersion,
		canonicalBody,
	)
	if err != nil {
		return err
	}
	provided := strings.TrimSpace(proof)
	if len(provided) != len(expected) ||
		subtle.ConstantTimeCompare([]byte(provided), []byte(expected)) != 1 {
		return ErrInvalidProof
	}
	return nil
}

// SealCredentialBundle returns only ciphertext and public validity metadata.
// expiresAt must be no more than five minutes after issuance and no later than
// the deployment tuple's own authority expiry.
func (p *Protocol) SealCredentialBundle(
	requestBody []byte,
	requestTimestamp int64,
	requestNonce string,
	bundle CredentialBundle,
	issuedAt, expiresAt time.Time,
) (ProvisionResponse, error) {
	var response ProvisionResponse
	if p == nil || len(p.aeadKey) != 32 {
		return response, ErrInvalidBundle
	}
	request, err := DecodeProvisionRequest(requestBody, issuedAt)
	if err != nil || validateNonce(requestNonce) != nil || requestTimestamp <= 0 {
		return response, ErrInvalidBundle
	}
	issuedAt = normalizeNow(issuedAt)
	expiresAt = expiresAt.UTC()
	authorityExpiry, parseErr := parseCanonicalUTC(request.Tuple.ExpiresAt)
	if parseErr != nil || expiresAt.After(authorityExpiry) ||
		!expiresAt.After(issuedAt) || expiresAt.Sub(issuedAt) > MaximumBundleTTL {
		return response, ErrInvalidBundle
	}
	issuedText := issuedAt.Format(time.RFC3339Nano)
	expiresText := expiresAt.Format(time.RFC3339Nano)
	bundle.Version = Version
	bundle.Mode = Mode
	bundle.Tuple = request.Tuple
	bundle.RequestNonce = requestNonce
	bundle.IssuedAt = issuedText
	bundle.ExpiresAt = expiresText
	if err := bundle.Validate(issuedAt); err != nil {
		return response, err
	}
	plaintext, err := json.Marshal(bundle)
	if err != nil || len(plaintext) > MaximumResponseBodyBytes/2 {
		return response, ErrInvalidBundle
	}
	block, err := aes.NewCipher(p.aeadKey)
	if err != nil {
		return response, ErrInvalidBundle
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return response, ErrInvalidBundle
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return response, ErrInvalidBundle
	}
	response = ProvisionResponse{
		Version:   Version,
		Mode:      Mode,
		KeyID:     p.keyID,
		IssuedAt:  issuedText,
		ExpiresAt: expiresText,
	}
	aad := bundleAAD(
		requestTimestamp,
		requestNonce,
		requestBody,
		response.KeyID,
		response.IssuedAt,
		response.ExpiresAt,
	)
	sealed := aead.Seal(nonce, nonce, plaintext, aad)
	response.Envelope = base64.RawURLEncoding.EncodeToString(sealed)
	return response, nil
}

// OpenCredentialBundle authenticates the response metadata and ciphertext,
// then revalidates every duplicated request and authority field.
func (p *Protocol) OpenCredentialBundle(
	requestBody []byte,
	requestTimestamp int64,
	requestNonce string,
	response ProvisionResponse,
	now time.Time,
) (CredentialBundle, error) {
	var bundle CredentialBundle
	if p == nil || len(p.aeadKey) != 32 || response.KeyID != p.keyID {
		return bundle, ErrInvalidResponse
	}
	request, err := DecodeProvisionRequest(requestBody, now)
	if err != nil || validateNonce(requestNonce) != nil || requestTimestamp <= 0 {
		return bundle, ErrInvalidResponse
	}
	issuedAt, expiresAt, err := validateResponse(response, now)
	if err != nil {
		return bundle, err
	}
	authorityExpiry, err := parseCanonicalUTC(request.Tuple.ExpiresAt)
	if err != nil || expiresAt.After(authorityExpiry) || expiresAt.Sub(issuedAt) > MaximumBundleTTL {
		return bundle, ErrInvalidResponse
	}
	sealed, err := base64.RawURLEncoding.DecodeString(response.Envelope)
	if err != nil || base64.RawURLEncoding.EncodeToString(sealed) != response.Envelope {
		return bundle, ErrInvalidResponse
	}
	block, err := aes.NewCipher(p.aeadKey)
	if err != nil {
		return bundle, ErrInvalidResponse
	}
	aead, err := cipher.NewGCM(block)
	if err != nil || len(sealed) < aead.NonceSize()+aead.Overhead() {
		return bundle, ErrInvalidResponse
	}
	aad := bundleAAD(
		requestTimestamp,
		requestNonce,
		requestBody,
		response.KeyID,
		response.IssuedAt,
		response.ExpiresAt,
	)
	plaintext, err := aead.Open(nil, sealed[:aead.NonceSize()], sealed[aead.NonceSize():], aad)
	if err != nil {
		return bundle, ErrInvalidResponse
	}
	if err := decodeStrict(plaintext, MaximumResponseBodyBytes, &bundle); err != nil {
		return bundle, ErrInvalidBundle
	}
	if err := bundle.Validate(now); err != nil {
		return bundle, err
	}
	if bundle.Tuple != request.Tuple || bundle.RequestNonce != requestNonce ||
		bundle.MessengerAppSecretKeyID != request.MessengerAppSecretKeyID ||
		bundle.ProviderProofSecretKeyID != request.ProviderProofSecretKeyID ||
		bundle.IssuedAt != response.IssuedAt || bundle.ExpiresAt != response.ExpiresAt {
		return CredentialBundle{}, ErrInvalidBundle
	}
	return bundle, nil
}

// Validate rejects ambiguous credential generations, weak secrets, non-HTTPS
// callbacks, and any callback that is not for the exact pinned account UUID.
func (bundle CredentialBundle) Validate(now time.Time) error {
	if bundle.Version != Version || bundle.Mode != Mode ||
		bundle.Tuple.Validate(now) != nil ||
		!validSignatureText(bundle.MessengerAppSecretKeyID) ||
		!validSignatureText(bundle.ProviderProofSecretKeyID) ||
		!canonicalUUID(bundle.CredentialID) || bundle.CredentialVersion <= 0 ||
		len([]byte(bundle.InboundSecret)) < inboundSecretMinBytes ||
		len([]byte(bundle.InboundSecret)) > inboundSecretMaxBytes ||
		containsWhitespace(bundle.InboundSecret) ||
		validateNonce(bundle.RequestNonce) != nil {
		return ErrInvalidBundle
	}
	issuedAt, err := parseCanonicalUTC(bundle.IssuedAt)
	if err != nil {
		return ErrInvalidBundle
	}
	expiresAt, err := parseCanonicalUTC(bundle.ExpiresAt)
	if err != nil || !expiresAt.After(normalizeNow(now)) || !expiresAt.After(issuedAt) ||
		expiresAt.Sub(issuedAt) > MaximumBundleTTL {
		return ErrInvalidBundle
	}
	authorityExpiry, err := parseCanonicalUTC(bundle.Tuple.ExpiresAt)
	if err != nil || expiresAt.After(authorityExpiry) {
		return ErrInvalidBundle
	}
	if err := validateWebhookURL(bundle.ReReplyWebhookURL, bundle.Tuple.ChannelAccountID); err != nil {
		return ErrInvalidBundle
	}
	return nil
}

// CredentialGeneration is a non-secret immutable cache/job binding. A relay
// must not use a cached secret for a different credential row or version.
func (bundle CredentialBundle) CredentialGeneration() string {
	if !canonicalUUID(bundle.CredentialID) || bundle.CredentialVersion <= 0 {
		return ""
	}
	return bundle.CredentialID + ":" + strconv.Itoa(bundle.CredentialVersion)
}

func validateResponse(response ProvisionResponse, now time.Time) (time.Time, time.Time, error) {
	if response.Version != Version || response.Mode != Mode ||
		!validSignatureText(response.KeyID) || strings.TrimSpace(response.Envelope) == "" ||
		len(response.Envelope) > MaximumResponseBodyBytes {
		return time.Time{}, time.Time{}, ErrInvalidResponse
	}
	issuedAt, err := parseCanonicalUTC(response.IssuedAt)
	if err != nil {
		return time.Time{}, time.Time{}, ErrInvalidResponse
	}
	expiresAt, err := parseCanonicalUTC(response.ExpiresAt)
	if err != nil || !expiresAt.After(normalizeNow(now)) || !expiresAt.After(issuedAt) ||
		expiresAt.Sub(issuedAt) > MaximumBundleTTL {
		return time.Time{}, time.Time{}, ErrInvalidResponse
	}
	return issuedAt, expiresAt, nil
}

func canonicalRequest(timestamp int64, nonce string, body []byte) []byte {
	digest := sha256.Sum256(body)
	return []byte(strings.Join([]string{
		requestAuthDomain,
		"POST",
		ProvisionPath,
		strconv.FormatInt(timestamp, 10),
		nonce,
		hex.EncodeToString(digest[:]),
	}, "\n"))
}

func canonicalInboundProof(
	tuple ProvisionTuple,
	credentialID string,
	credentialVersion int,
	body []byte,
) []byte {
	digest := sha256.Sum256(body)
	return []byte(strings.Join([]string{
		inboundProofDomain,
		Marker,
		Mode,
		tuple.OrganizationID,
		tuple.MetaBusinessID,
		tuple.PageID,
		tuple.MetaAppID,
		tuple.ChannelAccountID,
		tuple.Generation,
		tuple.ExpiresAt,
		credentialID,
		strconv.Itoa(credentialVersion),
		hex.EncodeToString(digest[:]),
	}, "\n"))
}

func bundleAAD(
	timestamp int64,
	nonce string,
	body []byte,
	keyID, issuedAt, expiresAt string,
) []byte {
	digest := sha256.Sum256(body)
	return []byte(strings.Join([]string{
		bundleAEADDomain,
		strconv.FormatInt(timestamp, 10),
		nonce,
		hex.EncodeToString(digest[:]),
		keyID,
		issuedAt,
		expiresAt,
	}, "\n"))
}

func validateNonce(nonce string) error {
	decoded, err := base64.RawURLEncoding.DecodeString(nonce)
	if err != nil || len(decoded) != nonceBytes ||
		base64.RawURLEncoding.EncodeToString(decoded) != nonce {
		return ErrInvalidRequest
	}
	return nil
}

func validSignatureText(value string) bool {
	if len(value) != len(signaturePrefix)+(sha256.Size*2) ||
		!strings.HasPrefix(value, signaturePrefix) {
		return false
	}
	decoded, err := hex.DecodeString(strings.TrimPrefix(value, signaturePrefix))
	return err == nil && len(decoded) == sha256.Size
}

func validateInboundProofInputs(
	tuple ProvisionTuple,
	credentialID string,
	credentialVersion int,
	body []byte,
) error {
	if tuple.validateStructure() != nil ||
		!canonicalUUID(credentialID) || credentialVersion <= 0 ||
		len(body) == 0 || len(body) > MaximumInboundBodyBytes {
		return ErrInvalidProof
	}
	return nil
}

func (tuple ProvisionTuple) validateStructure() error {
	if !canonicalUUID(tuple.OrganizationID) ||
		!canonicalUUID(tuple.ChannelAccountID) ||
		!canonicalUUID(tuple.Generation) ||
		!canonicalMetaID(tuple.MetaBusinessID) ||
		!canonicalMetaID(tuple.PageID) ||
		!canonicalMetaID(tuple.MetaAppID) {
		return ErrInvalidRequest
	}
	if _, err := parseCanonicalUTC(tuple.ExpiresAt); err != nil {
		return ErrInvalidRequest
	}
	return nil
}

func validRootSecret(secret string) bool {
	return len([]byte(secret)) >= minimumRootSecretBytes && !containsWhitespace(secret)
}

func containsWhitespace(value string) bool {
	for _, character := range value {
		if unicode.IsSpace(character) {
			return true
		}
	}
	return false
}

// Hashing first ensures secret equality always compares a fixed number of
// bytes even when deployments accidentally supply different-length values.
func constantTimeSecretEqual(first, second string) bool {
	firstDigest := sha256.Sum256([]byte(first))
	secondDigest := sha256.Sum256([]byte(second))
	return subtle.ConstantTimeCompare(firstDigest[:], secondDigest[:]) == 1
}

func canonicalUUID(value string) bool {
	parsed, err := uuid.Parse(value)
	return err == nil && parsed != uuid.Nil && parsed.String() == value
}

func canonicalMetaID(value string) bool {
	if value == "" || len(value) > 32 || value[0] == '0' {
		return false
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			return false
		}
	}
	return true
}

func parseCanonicalUTC(value string) (time.Time, error) {
	if !strings.HasSuffix(value, "Z") {
		return time.Time{}, errors.New("timestamp is not canonical UTC")
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil || parsed.Format(time.RFC3339Nano) != value {
		return time.Time{}, errors.New("timestamp is not canonical UTC")
	}
	return parsed.UTC(), nil
}

func normalizeNow(now time.Time) time.Time {
	if now.IsZero() {
		return time.Now().UTC()
	}
	return now.UTC()
}

func validateWebhookURL(raw, accountID string) error {
	if len(raw) == 0 || len(raw) > maximumWebhookURLBytes {
		return errors.New("webhook URL is invalid")
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" ||
		parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" ||
		parsed.RawPath != "" ||
		parsed.Path != "/api/webhooks/channels/"+accountID {
		return errors.New("webhook URL is invalid")
	}
	return nil
}

func decodeStrict(body []byte, maximum int, destination any) error {
	if len(body) == 0 || len(body) > maximum {
		return errors.New("body is invalid")
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return fmt.Errorf("multiple JSON values are not allowed")
		}
		return err
	}
	return nil
}
