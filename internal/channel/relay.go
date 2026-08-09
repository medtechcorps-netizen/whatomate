package channel

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	appcrypto "github.com/shridarpatil/whatomate/internal/crypto"
	"github.com/shridarpatil/whatomate/internal/models"
)

const (
	RelayProvider                     = "relay"
	RelaySignatureHeader              = "X-ReReply-Signature-256"
	RelayMetaProviderProofHeader      = "X-ReReply-Meta-Provider-Proof-256"
	RelayMetaProviderProofKeyIDHeader = "X-ReReply-Meta-Provider-Proof-Key-ID"
	MetaProviderProofMetadataKey      = "meta_provider_proof_version"
	MetaProviderProofVersion          = "v1"
	RelayReadinessHeader              = "X-ReReply-Relay-Readiness"
	RelayChannelHeader                = "X-ReReply-Channel"
	RelayExternalAccountHeader        = "X-ReReply-External-Account-ID"
	RelayChannelAccountHeader         = "X-ReReply-Channel-Account-ID"
	RelayOrganizationHeader           = "X-ReReply-Organization-ID"
	RelayMetaBusinessHeader           = "X-ReReply-Meta-Business-ID"
	RelayReadinessVersion             = "v2"
	relaySignaturePrefix              = "sha256="
	metaProviderProofDomain           = "rereply-meta-provider-proof:v1"
	metaProviderProofKeyIDDomain      = "rereply-meta-provider-proof-key-id:v1"
	minimumProviderProofBytes         = 32
	defaultRelayBodyLimit             = int64(1 << 20)
	maxRelayReplyContextRunes         = 512
	maxRelayMessageContextSize        = 64 << 10

	// RelayWebhookMaxBodyBytes is the public ReReply webhook request limit.
	// Relay implementations that batch canonical events must stay below
	// RelayCanonicalWebhookMaxBodyBytes so protocol overhead cannot make an
	// already-accepted batch fail at the ReReply boundary.
	RelayWebhookMaxBodyBytes          = 2 << 20
	RelayCanonicalWebhookMaxBodyBytes = RelayWebhookMaxBodyBytes - (64 << 10)
)

var (
	ErrRelaySecretMissing            = errors.New("relay signing secret is not configured")
	ErrRelaySignatureMissing         = errors.New("relay signature is missing")
	ErrRelaySignatureInvalid         = errors.New("relay signature is invalid")
	ErrRelayMetaProviderProofMissing = errors.New("meta relay provider proof is missing")
	ErrRelayMetaProviderProofInvalid = errors.New("meta relay provider proof is invalid")
	ErrRelayMetaProviderProofSecret  = errors.New("meta relay provider proof secret is not configured securely")
	ErrRelayMetaTrustedBinding       = errors.New("meta relay trusted Business binding is not configured")
	ErrRelayOutboundDisabled         = errors.New("relay outbound delivery is not approved")
	ErrRelayURLInvalid               = errors.New("relay URL is invalid")
	ErrCredentialRefreshUnsupported  = errors.New("relay credentials cannot be refreshed automatically")
	errRelayOutgoingEcho             = errors.New("relay outgoing message echo")
)

// RelayAdapter implements a generic HMAC-signed HTTPS bridge for channels that
// do not yet have a first-party provider adapter.
type RelayAdapter struct {
	channel       models.Channel
	client        *http.Client
	encryptionKey string
	now           func() time.Time
	lookupIP      func(context.Context, string) ([]net.IPAddr, error)
	// expectedMetaBusinessID is supplied only by the application process's
	// protected Meta relay inventory. A tenant URL or readiness response cannot
	// choose this value.
	expectedMetaBusinessID string
	// metaProviderProofSecret is deployment-held and never tenant supplied. It
	// proves that canonical Meta traffic and readiness came from the configured
	// relay even though tenant administrators know the account-scoped HMAC.
	metaProviderProofSecret string
	// allowLocalhostDev is deliberately not sourced from ChannelAccount.Config.
	// It is an internal process-level switch used by package tests/local
	// development only; production handlers leave it false.
	allowLocalhostDev bool
}

// WithExpectedMetaBusinessID binds Meta readiness validation to protected,
// application-held ownership state. Meta relay adapters fail closed when this
// expectation is absent; generic relay channels remain backward compatible.
func (a *RelayAdapter) WithExpectedMetaBusinessID(metaBusinessID string) *RelayAdapter {
	if a != nil {
		a.expectedMetaBusinessID = strings.TrimSpace(metaBusinessID)
	}
	return a
}

// WithMetaProviderProofSecret installs the deployment-held Meta relay proof
// key. Generic relay channels do not require it and remain backward compatible.
func (a *RelayAdapter) WithMetaProviderProofSecret(secret string) *RelayAdapter {
	if a != nil {
		a.metaProviderProofSecret = secret
	}
	return a
}

// WithLocalhostDevelopment explicitly permits loopback relay endpoints for a
// process-owned development environment. Tenant-controlled channel
// configuration must never select this option.
func (a *RelayAdapter) WithLocalhostDevelopment() *RelayAdapter {
	if a != nil {
		a.allowLocalhostDev = true
	}
	return a
}

// SignMetaProviderInboundProof signs the exact canonical body using a domain
// that cannot be confused with an account-scoped webhook signature or a
// readiness proof.
func SignMetaProviderInboundProof(secret string, body []byte) string {
	prefix := []byte(metaProviderProofDomain + "\ninbound\n")
	payload := make([]byte, 0, len(prefix)+len(body))
	payload = append(payload, prefix...)
	payload = append(payload, body...)
	return signRelayBody(secret, payload)
}

// SignMetaProviderOutboundProof authenticates ReReply itself to the Meta
// relay, preventing a tenant-held account HMAC from bypassing ReReply's send
// approval gate by calling the relay directly.
func SignMetaProviderOutboundProof(secret string, body []byte) string {
	prefix := []byte(metaProviderProofDomain + "\noutbound\n")
	payload := make([]byte, 0, len(prefix)+len(body))
	payload = append(payload, prefix...)
	payload = append(payload, body...)
	return signRelayBody(secret, payload)
}

// SignMetaProviderReadinessProof binds a proof to one exact protected mapping.
// Mapping fields are already constrained to newline-free canonical values by
// the application and relay startup validators.
func SignMetaProviderReadinessProof(
	secret string,
	channel models.Channel,
	externalAccountID, channelAccountID, organizationID, metaBusinessID string,
) string {
	payload := strings.Join([]string{
		metaProviderProofDomain,
		"readiness",
		string(channel),
		externalAccountID,
		channelAccountID,
		organizationID,
		metaBusinessID,
	}, "\n")
	return signRelayBody(secret, []byte(payload))
}

// MetaProviderProofKeyID returns a non-secret, domain-separated fingerprint of
// the deployment-held provider-proof key. ReReply and the relay expose this on
// readiness responses so deployment monitoring can detect a one-sided secret
// rotation without retrieving either runtime's secret value.
func MetaProviderProofKeyID(secret string) (string, error) {
	if len([]byte(secret)) < minimumProviderProofBytes || strings.TrimSpace(secret) != secret {
		return "", ErrRelayMetaProviderProofSecret
	}
	return signRelayBody(secret, []byte(metaProviderProofKeyIDDomain)), nil
}

func constantTimeRelaySignatureEqual(provided, expected string) bool {
	providedBytes := []byte(strings.TrimSpace(provided))
	expectedBytes := []byte(expected)
	return len(providedBytes) == len(expectedBytes) && hmac.Equal(providedBytes, expectedBytes)
}

func (a *RelayAdapter) requiresMetaProviderProof() bool {
	return a != nil &&
		(a.channel == models.ChannelInstagram || a.channel == models.ChannelMessenger)
}

func (a *RelayAdapter) validateMetaDeploymentTrust() error {
	if !a.requiresMetaProviderProof() {
		return nil
	}
	if !isCanonicalMetaBusinessID(a.expectedMetaBusinessID) {
		return ErrRelayMetaTrustedBinding
	}
	secret := a.metaProviderProofSecret
	if len([]byte(secret)) < minimumProviderProofBytes || strings.TrimSpace(secret) != secret {
		return ErrRelayMetaProviderProofSecret
	}
	return nil
}

func NewRelayAdapter(channel models.Channel, client *http.Client, encryptionKey string) *RelayAdapter {
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	return &RelayAdapter{
		channel:       channel,
		client:        client,
		encryptionKey: encryptionKey,
		now:           time.Now,
		lookupIP:      net.DefaultResolver.LookupIPAddr,
	}
}

func (a *RelayAdapter) Channel() models.Channel {
	return a.channel
}

func (a *RelayAdapter) Provider() string {
	return RelayProvider
}

func (a *RelayAdapter) Capabilities(account *models.ChannelAccount) Capabilities {
	capabilities := defaultRelayCapabilities(a.channel)
	if account == nil {
		return ApplyMandatoryProviderCapabilities(a.channel, capabilities)
	}

	setCapability := func(key string, target *bool) {
		if value, ok := account.Capabilities[key].(bool); ok {
			*target = value
		}
	}
	setCapability(string(CapabilityText), &capabilities.Text)
	setCapability(string(CapabilityMedia), &capabilities.Media)
	setCapability(string(CapabilityMultipleAttachments), &capabilities.MultipleAttachments)
	setCapability(string(CapabilityReplies), &capabilities.Replies)
	setCapability(string(CapabilityReactions), &capabilities.Reactions)
	setCapability(string(CapabilityReadReceipts), &capabilities.ReadReceipts)
	setCapability(string(CapabilityButtons), &capabilities.Buttons)
	setCapability(string(CapabilityTemplates), &capabilities.Templates)
	setCapability(string(CapabilityBusinessInitiation), &capabilities.BusinessInitiation)
	setCapability(string(CapabilityServiceWindow), &capabilities.ServiceWindow)
	setCapability(string(CapabilityTyping), &capabilities.Typing)
	setCapability(string(CapabilitySubjectAndCC), &capabilities.SubjectAndCC)

	if value, ok := numericConfig(account.Capabilities, "max_attachments"); ok {
		capabilities.MaxAttachments = int(value)
	}
	if value, ok := numericConfig(account.Capabilities, "max_media_bytes"); ok {
		capabilities.MaxMediaBytes = int64(value)
	}
	if values, ok := account.Capabilities["supported_media_types"].([]any); ok {
		for _, value := range values {
			if mediaType, ok := value.(string); ok {
				capabilities.SupportedMediaTypes = append(capabilities.SupportedMediaTypes, mediaType)
			}
		}
	}
	return ApplyMandatoryProviderCapabilities(a.channel, capabilities)
}

func defaultRelayCapabilities(channel models.Channel) Capabilities {
	switch channel {
	case models.ChannelInstagram, models.ChannelMessenger:
		// Meta relay accounts start text-only. Additional capabilities require
		// an explicit, reviewed account configuration.
		return Capabilities{Text: true, ServiceWindow: true}
	case models.ChannelEmail:
		// The signed Gmail relay starts with reply-only text support. Email
		// media, read receipts, typing, CC, and new-message composition must
		// not be advertised until the relay implements them end to end.
		return Capabilities{Text: true, Replies: true}
	default:
		return Capabilities{
			Text:         true,
			Media:        true,
			Replies:      true,
			ReadReceipts: true,
			Typing:       true,
		}
	}
}

func (a *RelayAdapter) RouteHint(_ http.Header, body []byte) (WebhookRouteHint, error) {
	var envelope relayWebhookEnvelope
	if err := json.Unmarshal(body, &envelope); err != nil {
		return WebhookRouteHint{}, fmt.Errorf("decode relay webhook route: %w", err)
	}
	if strings.TrimSpace(envelope.ExternalAccountID) == "" {
		return WebhookRouteHint{}, errors.New("relay webhook external_account_id is required")
	}
	return WebhookRouteHint{
		ExternalAccountID: envelope.ExternalAccountID,
		Provider:          RelayProvider,
	}, nil
}

func (a *RelayAdapter) VerifyWebhook(account *models.ChannelAccount, headers http.Header, body []byte) error {
	signature := strings.TrimSpace(headers.Get(RelaySignatureHeader))
	if signature == "" {
		return ErrRelaySignatureMissing
	}
	if !strings.HasPrefix(strings.ToLower(signature), relaySignaturePrefix) {
		return ErrRelaySignatureInvalid
	}

	provided, err := hex.DecodeString(signature[len(relaySignaturePrefix):])
	if err != nil || len(provided) != sha256.Size {
		return ErrRelaySignatureInvalid
	}
	secret, err := a.credentialValue(account, "inbound_secret")
	if err != nil {
		return err
	}

	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(body)
	if !hmac.Equal(provided, mac.Sum(nil)) {
		return ErrRelaySignatureInvalid
	}
	if a.requiresMetaProviderProof() {
		if err := a.validateMetaDeploymentTrust(); err != nil {
			return err
		}
		proof := strings.TrimSpace(headers.Get(RelayMetaProviderProofHeader))
		if proof == "" {
			return ErrRelayMetaProviderProofMissing
		}
		expected := SignMetaProviderInboundProof(a.metaProviderProofSecret, body)
		if !constantTimeRelaySignatureEqual(proof, expected) {
			return ErrRelayMetaProviderProofInvalid
		}
	}
	return nil
}

func (a *RelayAdapter) NormalizeWebhook(_ context.Context, account *models.ChannelAccount, body []byte) ([]InboundEvent, error) {
	var envelope relayWebhookEnvelope
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, fmt.Errorf("decode relay webhook: %w", err)
	}
	if account == nil || envelope.ExternalAccountID != account.ExternalAccountID {
		return nil, errors.New("relay webhook account does not match route")
	}
	if len(envelope.Events) == 0 {
		return nil, errors.New("relay webhook contains no events")
	}

	now := a.now().UTC()
	seen := make(map[string]struct{}, len(envelope.Events))
	normalized := make([]InboundEvent, 0, len(envelope.Events))
	for i := range envelope.Events {
		event := &envelope.Events[i]
		event.DedupeKey = strings.TrimSpace(event.DedupeKey)
		if event.DedupeKey == "" {
			return nil, fmt.Errorf("relay event %d dedupe_key is required", i)
		}
		if _, duplicate := seen[event.DedupeKey]; duplicate {
			return nil, fmt.Errorf("relay event %d duplicates dedupe_key %q", i, event.DedupeKey)
		}
		seen[event.DedupeKey] = struct{}{}
		event.OccurredAt = capRelayTimestamp(event.OccurredAt, now)
		switch event.Type {
		case NormalizedEventTypeMessage:
			if err := validateRelayMessage(event.Message, i, now); err != nil {
				if errors.Is(err, errRelayOutgoingEcho) {
					// Provider echoes of messages sent by ReReply are not
					// customer inbound events and must never enter the inbox as
					// incoming messages.
					continue
				}
				return nil, err
			}
		case NormalizedEventTypeMessageStatus:
			if event.MessageStatus == nil {
				return nil, fmt.Errorf("relay event %d message_status payload is required", i)
			}
			event.MessageStatus.OccurredAt = capRelayTimestamp(event.MessageStatus.OccurredAt, now)
		case NormalizedEventTypeRead:
			if event.Read == nil {
				return nil, fmt.Errorf("relay event %d read payload is required", i)
			}
			event.Read.ReadAt = capRelayTimestamp(event.Read.ReadAt, now)
		case NormalizedEventTypeReaction:
			if event.Reaction == nil {
				return nil, fmt.Errorf("relay event %d reaction payload is required", i)
			}
			event.Reaction.OccurredAt = capRelayTimestamp(event.Reaction.OccurredAt, now)
		default:
			return nil, fmt.Errorf("relay event %d type %q is not supported", i, event.Type)
		}
		normalized = append(normalized, *event)
	}
	return normalized, nil
}

func (a *RelayAdapter) Send(ctx context.Context, account *models.ChannelAccount, message OutboundMessage) (SendResult, error) {
	if account == nil || !boolConfig(account.Config, "outbound_enabled") {
		return SendResult{}, ErrRelayOutboundDisabled
	}
	if strings.TrimSpace(message.IdempotencyKey) == "" || len(message.Parts) == 0 {
		return SendResult{}, errors.New("relay outbound message requires idempotency_key and parts")
	}

	var response struct {
		ProviderMessageIDs     []string `json:"provider_message_ids"`
		ExternalConversationID string   `json:"external_conversation_id"`
		ProviderRequestID      string   `json:"provider_request_id"`
	}
	if err := a.post(ctx, account, "message", message, &response); err != nil {
		return SendResult{}, err
	}
	if len(response.ProviderMessageIDs) == 0 {
		return SendResult{}, errors.New("relay response contains no provider message ID")
	}
	return SendResult{
		ProviderMessageIDs:     response.ProviderMessageIDs,
		ExternalConversationID: response.ExternalConversationID,
		ProviderRequestID:      response.ProviderRequestID,
		Status:                 models.MessageStatusSent,
		AcceptedAt:             a.now().UTC(),
	}, nil
}

func (a *RelayAdapter) MarkRead(ctx context.Context, account *models.ChannelAccount, conversation ConversationRef, externalMessageIDs []string) error {
	if account == nil || !boolConfig(account.Config, "outbound_enabled") {
		return ErrRelayOutboundDisabled
	}
	if len(externalMessageIDs) == 0 {
		return nil
	}
	return a.post(ctx, account, "read", map[string]any{
		"conversation":         conversation,
		"external_message_ids": externalMessageIDs,
	}, nil)
}

func (a *RelayAdapter) FetchMedia(ctx context.Context, account *models.ChannelAccount, ref MediaRef) (FetchedMedia, error) {
	if account == nil {
		return FetchedMedia{}, errors.New("relay account is required")
	}
	allowLocalhost := a.allowLocalhostDev
	mediaURL, err := validateRelayURL(ref.URL, allowLocalhost)
	if err != nil {
		return FetchedMedia{}, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, mediaURL.String(), nil)
	if err != nil {
		return FetchedMedia{}, err
	}
	secret, err := a.outboundSecret(account)
	if err != nil {
		return FetchedMedia{}, err
	}
	req.Header.Set(RelaySignatureHeader, signRelayBody(secret, nil))
	req.Header.Set("User-Agent", "ReReply-Relay/1.0")

	client, err := a.safeHTTPClient(allowLocalhost)
	if err != nil {
		return FetchedMedia{}, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return FetchedMedia{}, &ProviderError{
			Operation: "fetch_media",
			Provider:  RelayProvider,
			Message:   err.Error(),
			Retryable: true,
			Cause:     err,
		}
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		defer func() {
			_ = resp.Body.Close()
		}()
		return FetchedMedia{}, relayHTTPError("fetch_media", resp)
	}
	return FetchedMedia{
		Content:   resp.Body,
		MimeType:  firstNonEmpty(resp.Header.Get("Content-Type"), ref.MimeType),
		Filename:  ref.Filename,
		SizeBytes: resp.ContentLength,
	}, nil
}

func (a *RelayAdapter) ValidateAccount(ctx context.Context, account *models.ChannelAccount) (AccountValidationResult, error) {
	result := AccountValidationResult{
		Capabilities: a.Capabilities(account),
		CheckedAt:    a.now().UTC(),
	}
	if account == nil {
		return result, errors.New("relay account is required")
	}
	if err := a.validateMetaDeploymentTrust(); err != nil {
		return result, err
	}
	if _, err := a.credentialValue(account, "inbound_secret"); err != nil {
		return result, err
	}

	rawURL := stringConfig(account.Config, "health_url")
	if rawURL == "" {
		rawURL = stringConfig(account.Config, "relay_url")
	}
	target, err := validateRelayURL(rawURL, a.allowLocalhostDev)
	if err != nil {
		return result, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodHead, target.String(), nil)
	if err != nil {
		return result, err
	}
	secret, err := a.outboundSecret(account)
	if err != nil {
		return result, err
	}
	req.Header.Set(RelaySignatureHeader, signRelayBody(secret, nil))
	req.Header.Set("User-Agent", "ReReply-Relay/1.0")
	client, err := a.safeHTTPClient(a.allowLocalhostDev)
	if err != nil {
		return result, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return result, &ProviderError{
			Operation: "validate_account",
			Provider:  RelayProvider,
			Message:   err.Error(),
			Retryable: true,
			Cause:     err,
		}
	}
	defer func() {
		_ = resp.Body.Close()
	}()
	if (resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices) &&
		resp.StatusCode != http.StatusMethodNotAllowed {
		return result, relayHTTPError("validate_account", resp)
	}
	if a.channel == models.ChannelInstagram || a.channel == models.ChannelMessenger {
		if err := validateMetaRelayReadinessHeaders(
			resp.Header,
			account,
			a.expectedMetaBusinessID,
			a.metaProviderProofSecret,
		); err != nil {
			return result, err
		}
	}
	result.Valid = true
	result.Status = string(models.ChannelAccountStatusActive)
	return result, nil
}

// validateMetaRelayReadinessHeaders binds a successful Meta relay health
// response to the exact tenant ChannelAccount and provider asset being tested.
// A generic 2xx/405 endpoint is not sufficient for Messenger or Instagram:
// only the production mapping that owns the account-scoped outbound secret may
// attest this tuple after checking its provider-side readiness.
func validateMetaRelayReadinessHeaders(
	headers http.Header,
	account *models.ChannelAccount,
	expectedMetaBusinessID string,
	providerProofSecret string,
) error {
	if account == nil ||
		account.ID == uuid.Nil ||
		account.OrganizationID == uuid.Nil ||
		!isCanonicalMetaBusinessID(expectedMetaBusinessID) ||
		strings.TrimSpace(headers.Get(RelayReadinessHeader)) != RelayReadinessVersion ||
		!strings.EqualFold(strings.TrimSpace(headers.Get(RelayChannelHeader)), string(account.Channel)) ||
		strings.TrimSpace(headers.Get(RelayExternalAccountHeader)) != strings.TrimSpace(account.ExternalAccountID) ||
		strings.TrimSpace(headers.Get(RelayChannelAccountHeader)) != account.ID.String() ||
		strings.TrimSpace(headers.Get(RelayOrganizationHeader)) != account.OrganizationID.String() ||
		strings.TrimSpace(headers.Get(RelayMetaBusinessHeader)) != expectedMetaBusinessID {
		return errors.New("meta relay production mapping or readiness attestation is missing or mismatched")
	}
	expectedProof := SignMetaProviderReadinessProof(
		providerProofSecret,
		account.Channel,
		strings.TrimSpace(account.ExternalAccountID),
		account.ID.String(),
		account.OrganizationID.String(),
		expectedMetaBusinessID,
	)
	if !constantTimeRelaySignatureEqual(
		headers.Get(RelayMetaProviderProofHeader),
		expectedProof,
	) {
		return errors.New("meta relay provider readiness proof is missing or mismatched")
	}
	return nil
}

func isCanonicalMetaBusinessID(value string) bool {
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

func (a *RelayAdapter) Subscribe(ctx context.Context, account *models.ChannelAccount) error {
	if account == nil || !boolConfig(account.Config, "outbound_enabled") {
		return ErrRelayOutboundDisabled
	}
	return a.post(ctx, account, "subscribe", map[string]any{
		"external_account_id": account.ExternalAccountID,
		"channel":             account.Channel,
	}, nil)
}

func (a *RelayAdapter) RefreshCredentials(_ context.Context, _ *models.ChannelAccount) (CredentialRefreshResult, error) {
	return CredentialRefreshResult{}, ErrCredentialRefreshUnsupported
}

type relayWebhookEnvelope struct {
	ExternalAccountID string         `json:"external_account_id"`
	Events            []InboundEvent `json:"events"`
}

type relayOutboundEnvelope struct {
	Type              string         `json:"type"`
	Channel           models.Channel `json:"channel"`
	ExternalAccountID string         `json:"external_account_id"`
	Data              any            `json:"data"`
}

func (a *RelayAdapter) post(ctx context.Context, account *models.ChannelAccount, eventType string, data any, response any) error {
	if err := a.validateMetaDeploymentTrust(); err != nil {
		return err
	}
	rawURL := stringConfig(account.Config, "relay_url")
	target, err := validateRelayURL(rawURL, a.allowLocalhostDev)
	if err != nil {
		return err
	}
	payload, err := json.Marshal(relayOutboundEnvelope{
		Type:              eventType,
		Channel:           account.Channel,
		ExternalAccountID: account.ExternalAccountID,
		Data:              data,
	})
	if err != nil {
		return fmt.Errorf("encode relay request: %w", err)
	}
	secret, err := a.outboundSecret(account)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target.String(), bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "ReReply-Relay/1.0")
	req.Header.Set(RelaySignatureHeader, signRelayBody(secret, payload))
	if a.requiresMetaProviderProof() {
		req.Header.Set(
			RelayMetaProviderProofHeader,
			SignMetaProviderOutboundProof(a.metaProviderProofSecret, payload),
		)
	}

	client, err := a.safeHTTPClient(a.allowLocalhostDev)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return &ProviderError{
			Operation: eventType,
			Provider:  RelayProvider,
			Message:   err.Error(),
			Retryable: true,
			Cause:     err,
		}
	}
	defer func() {
		_ = resp.Body.Close()
	}()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return relayHTTPError(eventType, resp)
	}
	if response == nil || resp.StatusCode == http.StatusNoContent {
		return nil
	}
	decoder := json.NewDecoder(io.LimitReader(resp.Body, defaultRelayBodyLimit))
	if err := decoder.Decode(response); err != nil {
		return fmt.Errorf("decode relay response: %w", err)
	}
	return nil
}

func (a *RelayAdapter) outboundSecret(account *models.ChannelAccount) (string, error) {
	if secret, err := a.credentialValue(account, "outbound_secret"); err == nil && secret != "" {
		return secret, nil
	}
	return a.credentialValue(account, "inbound_secret")
}

func (a *RelayAdapter) credentialValue(account *models.ChannelAccount, key string) (string, error) {
	if account == nil {
		return "", ErrRelaySecretMissing
	}
	now := a.now().UTC()
	for _, index := range CredentialIndexesByPriority(account.Credentials) {
		credential := &account.Credentials[index]
		if credential.Kind != models.ChannelCredentialKindWebhook ||
			!CredentialIsCurrent(credential, now) {
			continue
		}
		value, ok := credential.CredentialBlob[key].(string)
		if !ok || strings.TrimSpace(value) == "" {
			continue
		}
		if appcrypto.IsEncrypted(value) && strings.TrimSpace(a.encryptionKey) == "" {
			return "", errors.New("relay credential encryption key is unavailable")
		}
		decrypted, err := appcrypto.Decrypt(value, a.encryptionKey)
		if err != nil {
			return "", fmt.Errorf("decrypt relay credential: %w", err)
		}
		return decrypted, nil
	}
	return "", ErrRelaySecretMissing
}

func validateRelayMessage(message *InboundMessage, index int, now time.Time) error {
	if message == nil {
		return fmt.Errorf("relay event %d message payload is required", index)
	}
	switch message.Direction {
	case models.DirectionOutgoing:
		return errRelayOutgoingEcho
	case models.DirectionIncoming:
		// Continue with strict customer-inbound validation.
	case "":
		return fmt.Errorf("relay event %d message direction is required", index)
	default:
		return fmt.Errorf("relay event %d message direction %q is not supported", index, message.Direction)
	}
	if message.Sender.Role != models.ConversationParticipantRoleCustomer {
		return fmt.Errorf("relay event %d sender role must be customer", index)
	}
	if strings.TrimSpace(message.ExternalMessageID) == "" {
		return fmt.Errorf("relay event %d external_message_id is required", index)
	}
	if utf8.RuneCountInString(message.ExternalMessageID) > 255 {
		return fmt.Errorf("relay event %d external_message_id is too long", index)
	}
	if strings.TrimSpace(message.Sender.ExternalID) == "" {
		return fmt.Errorf("relay event %d sender external_id is required", index)
	}
	if utf8.RuneCountInString(message.Sender.ExternalID) > 255 ||
		utf8.RuneCountInString(message.Sender.Address) > 320 ||
		utf8.RuneCountInString(message.Sender.DisplayName) > 255 {
		return fmt.Errorf("relay event %d sender identity field is too long", index)
	}
	if strings.TrimSpace(message.Conversation.ExternalID) == "" {
		return fmt.Errorf("relay event %d conversation external_id is required", index)
	}
	if utf8.RuneCountInString(message.Conversation.ExternalID) > 512 ||
		utf8.RuneCountInString(message.Conversation.Subject) > 998 {
		return fmt.Errorf("relay event %d conversation field is too long", index)
	}
	if utf8.RuneCountInString(message.ReplyToExternalID) > maxRelayReplyContextRunes {
		return fmt.Errorf("relay event %d reply context is too long", index)
	}
	contextPayload, err := json.Marshal(struct {
		Message      map[string]any `json:"message,omitempty"`
		Conversation map[string]any `json:"conversation,omitempty"`
		Sender       map[string]any `json:"sender,omitempty"`
	}{
		Message:      message.Metadata,
		Conversation: message.Conversation.Metadata,
		Sender:       message.Sender.Metadata,
	})
	if err != nil {
		return fmt.Errorf("relay event %d message context cannot be encoded", index)
	}
	if len(contextPayload) > maxRelayMessageContextSize {
		return fmt.Errorf("relay event %d message context is too large", index)
	}
	if len(message.Parts) == 0 {
		return fmt.Errorf("relay event %d message parts are required", index)
	}
	if err := ValidateMessagePartsShape(message.Parts); err != nil {
		return fmt.Errorf("relay event %d: %w", index, err)
	}
	for partIndex, part := range message.Parts {
		switch part.Type {
		case models.MessagePartTypeText, models.MessagePartTypeHTML:
			if strings.TrimSpace(part.Text) == "" {
				return fmt.Errorf("relay event %d part %d text is required", index, partIndex)
			}
		case models.MessagePartTypeImage,
			models.MessagePartTypeVideo,
			models.MessagePartTypeAudio,
			models.MessagePartTypeDocument:
			if strings.TrimSpace(part.MediaURL) == "" && strings.TrimSpace(part.ProviderMediaRef) == "" {
				return fmt.Errorf("relay event %d part %d media reference is required", index, partIndex)
			}
		case models.MessagePartTypeLocation,
			models.MessagePartTypeContact,
			models.MessagePartTypeInteractive,
			models.MessagePartTypeTemplate,
			models.MessagePartTypeReaction:
			if len(part.Payload) == 0 {
				return fmt.Errorf("relay event %d part %d payload is required", index, partIndex)
			}
		default:
			return fmt.Errorf("relay event %d part %d type %q is not supported", index, partIndex, part.Type)
		}
	}
	message.SentAt = capRelayTimestamp(message.SentAt, now)
	message.ReceivedAt = capRelayTimestamp(message.ReceivedAt, now)
	return nil
}

func capRelayTimestamp(value, now time.Time) time.Time {
	now = now.UTC()
	if value.IsZero() || value.After(now) {
		return now
	}
	return value.UTC()
}

func validateRelayURL(rawURL string, allowLocalhost bool) (*url.URL, error) {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil ||
		parsed.Hostname() == "" ||
		parsed.User != nil ||
		parsed.Fragment != "" ||
		parsed.RawQuery != "" {
		return nil, ErrRelayURLInvalid
	}

	if isLocalhost(parsed.Hostname()) {
		if allowLocalhost && (strings.EqualFold(parsed.Scheme, "http") || strings.EqualFold(parsed.Scheme, "https")) {
			return parsed, nil
		}
		return nil, fmt.Errorf("%w: localhost requires explicit development approval", ErrRelayURLInvalid)
	}
	if strings.EqualFold(parsed.Scheme, "https") {
		if ip := net.ParseIP(parsed.Hostname()); ip != nil && unsafeRelayIP(ip) {
			return nil, fmt.Errorf("%w: private IP targets are not allowed", ErrRelayURLInvalid)
		}
		return parsed, nil
	}
	return nil, fmt.Errorf("%w: HTTPS is required", ErrRelayURLInvalid)
}

// ValidateRelayEndpoint validates a tenant-supplied relay endpoint using the
// production network policy. It performs syntactic/private-literal checks;
// every runtime dial additionally resolves and validates all DNS addresses.
func ValidateRelayEndpoint(rawURL string) error {
	_, err := validateRelayURL(rawURL, false)
	return err
}

func isLocalhost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func signRelayBody(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(body)
	return relaySignaturePrefix + hex.EncodeToString(mac.Sum(nil))
}

func relayHTTPError(operation string, response *http.Response) error {
	// Provider bodies are deliberately discarded: they can contain echoed
	// credentials, tokens, or customer content and must not reach API errors or
	// application logs.
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	return &ProviderError{
		Operation: operation,
		Provider:  RelayProvider,
		Code:      fmt.Sprintf("http_%d", response.StatusCode),
		Message:   fmt.Sprintf("relay returned HTTP %d", response.StatusCode),
		Retryable: response.StatusCode == http.StatusTooManyRequests || response.StatusCode >= http.StatusInternalServerError,
	}
}

func (a *RelayAdapter) safeHTTPClient(allowLocalhost bool) (*http.Client, error) {
	if a == nil || a.client == nil {
		return nil, errors.New("relay HTTP client is not configured")
	}
	client := *a.client

	var transport *http.Transport
	switch configured := a.client.Transport.(type) {
	case nil:
		defaultTransport, ok := http.DefaultTransport.(*http.Transport)
		if !ok {
			return nil, errors.New("default HTTP transport cannot enforce relay network policy")
		}
		transport = defaultTransport.Clone()
	case *http.Transport:
		transport = configured.Clone()
	default:
		return nil, errors.New("custom HTTP transport cannot enforce relay network policy")
	}
	transport.Proxy = nil
	transport.DialContext = a.safeDialContext(allowLocalhost)
	transport.DialTLSContext = nil
	transport.DialTLS = nil //nolint:staticcheck // Clear any cloned legacy hook so HTTPS uses the guarded DialContext.
	client.Transport = transport

	// Every relay request carries a tenant HMAC signature. Do not replay that
	// signature or payload to a redirect target, even when the target itself is
	// otherwise network-safe. Operators must configure the final relay URL.
	client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return &client, nil
}

func (a *RelayAdapter) safeDialContext(allowLocalhost bool) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, fmt.Errorf("%w: invalid relay address", ErrRelayURLInvalid)
		}
		if a.lookupIP == nil {
			return nil, errors.New("relay DNS resolver is not configured")
		}
		addresses, err := a.lookupIP(ctx, host)
		if err != nil {
			return nil, fmt.Errorf("resolve relay host: %w", err)
		}
		if len(addresses) == 0 {
			return nil, errors.New("relay host resolved to no addresses")
		}

		localDevelopmentHost := allowLocalhost && isLocalhost(host)
		for _, resolved := range addresses {
			if localDevelopmentHost {
				if !resolved.IP.IsLoopback() {
					return nil, fmt.Errorf("%w: localhost resolved outside loopback", ErrRelayURLInvalid)
				}
				continue
			}
			if unsafeRelayIP(resolved.IP) {
				return nil, fmt.Errorf("%w: relay host resolved to a private or local address", ErrRelayURLInvalid)
			}
		}

		dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
		var lastErr error
		for _, resolved := range addresses {
			connection, dialErr := dialer.DialContext(ctx, network, net.JoinHostPort(resolved.IP.String(), port))
			if dialErr == nil {
				return connection, nil
			}
			lastErr = dialErr
		}
		return nil, lastErr
	}
}

func unsafeRelayIP(ip net.IP) bool {
	return ip == nil ||
		ip.IsUnspecified() ||
		ip.IsLoopback() ||
		ip.IsPrivate() ||
		ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() ||
		ip.IsMulticast()
}

func boolConfig(config models.JSONB, key string) bool {
	value, _ := config[key].(bool)
	return value
}

func stringConfig(config models.JSONB, key string) string {
	value, _ := config[key].(string)
	return strings.TrimSpace(value)
}

func numericConfig(config models.JSONB, key string) (float64, bool) {
	switch value := config[key].(type) {
	case float64:
		return value, true
	case int:
		return float64(value), true
	case int64:
		return float64(value), true
	default:
		return 0, false
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
