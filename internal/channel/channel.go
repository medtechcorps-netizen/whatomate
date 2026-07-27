// Package channel defines the provider-independent boundary used by the
// omnichannel inbox. Provider adapters translate their APIs and webhooks into
// these canonical types; business logic should not depend on provider payloads.
package channel

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/models"
)

// Capability is a stable feature key used by routing and UI feature gates.
type Capability string

const (
	CapabilityText                Capability = "text"
	CapabilityMedia               Capability = "media"
	CapabilityMultipleAttachments Capability = "multiple_attachments"
	CapabilityReplies             Capability = "replies"
	CapabilityReactions           Capability = "reactions"
	CapabilityReadReceipts        Capability = "read_receipts"
	CapabilityButtons             Capability = "buttons"
	CapabilityTemplates           Capability = "templates"
	CapabilityBusinessInitiation  Capability = "business_initiation"
	CapabilityServiceWindow       Capability = "service_window"
	CapabilityTyping              Capability = "typing"
	CapabilitySubjectAndCC        Capability = "subject_and_cc"
)

// Capabilities describes the behavior of one configured account. Providers can
// vary capabilities by account tier, permissions, or API approval, so adapters
// receive the account rather than returning a global constant.
type Capabilities struct {
	Text                bool     `json:"text"`
	Media               bool     `json:"media"`
	MultipleAttachments bool     `json:"multiple_attachments"`
	Replies             bool     `json:"replies"`
	Reactions           bool     `json:"reactions"`
	ReadReceipts        bool     `json:"read_receipts"`
	Buttons             bool     `json:"buttons"`
	Templates           bool     `json:"templates"`
	BusinessInitiation  bool     `json:"business_initiation"`
	ServiceWindow       bool     `json:"service_window"`
	Typing              bool     `json:"typing"`
	SubjectAndCC        bool     `json:"subject_and_cc"`
	MaxAttachments      int      `json:"max_attachments,omitempty"`
	MaxMediaBytes       int64    `json:"max_media_bytes,omitempty"`
	SupportedMediaTypes []string `json:"supported_media_types,omitempty"`
}

// Supports reports whether a named canonical feature is enabled.
func (c Capabilities) Supports(capability Capability) bool {
	switch capability {
	case CapabilityText:
		return c.Text
	case CapabilityMedia:
		return c.Media
	case CapabilityMultipleAttachments:
		return c.MultipleAttachments
	case CapabilityReplies:
		return c.Replies
	case CapabilityReactions:
		return c.Reactions
	case CapabilityReadReceipts:
		return c.ReadReceipts
	case CapabilityButtons:
		return c.Buttons
	case CapabilityTemplates:
		return c.Templates
	case CapabilityBusinessInitiation:
		return c.BusinessInitiation
	case CapabilityServiceWindow:
		return c.ServiceWindow
	case CapabilityTyping:
		return c.Typing
	case CapabilitySubjectAndCC:
		return c.SubjectAndCC
	default:
		return false
	}
}

// WebhookRouteHint contains only untrusted values extracted before account
// lookup. Callers must load the matching tenant account and invoke
// VerifyWebhook before persisting or processing the event.
type WebhookRouteHint struct {
	ExternalAccountID string         `json:"external_account_id,omitempty"`
	ExternalObjectID  string         `json:"external_object_id,omitempty"`
	Provider          string         `json:"provider,omitempty"`
	Challenge         string         `json:"challenge,omitempty"`
	Metadata          map[string]any `json:"metadata,omitempty"`
}

type ConversationRef struct {
	ID         uuid.UUID      `json:"id,omitempty"`
	ExternalID string         `json:"external_id,omitempty"`
	Subject    string         `json:"subject,omitempty"`
	Metadata   map[string]any `json:"metadata,omitempty"`
}

type Participant struct {
	ID          uuid.UUID                          `json:"id,omitempty"`
	ExternalID  string                             `json:"external_id,omitempty"`
	Address     string                             `json:"address,omitempty"`
	DisplayName string                             `json:"display_name,omitempty"`
	Role        models.ConversationParticipantRole `json:"role"`
	Metadata    map[string]any                     `json:"metadata,omitempty"`
}

type MessagePart struct {
	Type             models.MessagePartType `json:"type"`
	Text             string                 `json:"text,omitempty"`
	Caption          string                 `json:"caption,omitempty"`
	MediaURL         string                 `json:"media_url,omitempty"`
	ProviderMediaRef string                 `json:"provider_media_ref,omitempty"`
	MimeType         string                 `json:"mime_type,omitempty"`
	Filename         string                 `json:"filename,omitempty"`
	SizeBytes        int64                  `json:"size_bytes,omitempty"`
	Payload          map[string]any         `json:"payload,omitempty"`
}

type TemplateMessage struct {
	Name       string         `json:"name"`
	Language   string         `json:"language,omitempty"`
	Parameters map[string]any `json:"parameters,omitempty"`
}

type OutboundMessage struct {
	OrganizationID      uuid.UUID                       `json:"organization_id"`
	MessageID           uuid.UUID                       `json:"message_id,omitempty"`
	IdempotencyKey      string                          `json:"idempotency_key"`
	Purpose             models.ChannelPreferencePurpose `json:"purpose"`
	Conversation        ConversationRef                 `json:"conversation"`
	Recipient           Participant                     `json:"recipient"`
	CC                  []Participant                   `json:"cc,omitempty"`
	Subject             string                          `json:"subject,omitempty"`
	Parts               []MessagePart                   `json:"parts"`
	Template            *TemplateMessage                `json:"template,omitempty"`
	ReplyToExternalID   string                          `json:"reply_to_external_id,omitempty"`
	ServiceWindowEndsAt *time.Time                      `json:"service_window_ends_at,omitempty"`
	Metadata            map[string]any                  `json:"metadata,omitempty"`
}

type SendResult struct {
	ProviderMessageIDs     []string             `json:"provider_message_ids"`
	ExternalConversationID string               `json:"external_conversation_id,omitempty"`
	ProviderRequestID      string               `json:"provider_request_id,omitempty"`
	Status                 models.MessageStatus `json:"status"`
	AcceptedAt             time.Time            `json:"accepted_at"`
	Raw                    map[string]any       `json:"raw,omitempty"`
}

type NormalizedEventType string

const (
	NormalizedEventTypeMessage             NormalizedEventType = "message"
	NormalizedEventTypeMessageStatus       NormalizedEventType = "message_status"
	NormalizedEventTypeRead                NormalizedEventType = "read"
	NormalizedEventTypeReaction            NormalizedEventType = "reaction"
	NormalizedEventTypeConversationUpdated NormalizedEventType = "conversation_updated"
	NormalizedEventTypeIdentityUpdated     NormalizedEventType = "identity_updated"
	NormalizedEventTypeAccountUpdated      NormalizedEventType = "account_updated"
)

type InboundMessage struct {
	ExternalMessageID string           `json:"external_message_id"`
	Conversation      ConversationRef  `json:"conversation"`
	Sender            Participant      `json:"sender"`
	Recipients        []Participant    `json:"recipients,omitempty"`
	Direction         models.Direction `json:"direction"`
	Parts             []MessagePart    `json:"parts"`
	ReplyToExternalID string           `json:"reply_to_external_id,omitempty"`
	SentAt            time.Time        `json:"sent_at"`
	ReceivedAt        time.Time        `json:"received_at"`
	Metadata          map[string]any   `json:"metadata,omitempty"`
}

type MessageStatusUpdate struct {
	ExternalMessageID string                  `json:"external_message_id"`
	Type              models.MessageEventType `json:"type"`
	OccurredAt        time.Time               `json:"occurred_at"`
	ActorExternalID   string                  `json:"actor_external_id,omitempty"`
	ErrorCode         string                  `json:"error_code,omitempty"`
	ErrorMessage      string                  `json:"error_message,omitempty"`
	Metadata          map[string]any          `json:"metadata,omitempty"`
}

type ReadReceipt struct {
	Conversation       ConversationRef `json:"conversation"`
	ExternalMessageIDs []string        `json:"external_message_ids"`
	Reader             Participant     `json:"reader"`
	ReadAt             time.Time       `json:"read_at"`
}

type Reaction struct {
	ExternalMessageID string      `json:"external_message_id"`
	Sender            Participant `json:"sender"`
	Emoji             string      `json:"emoji"`
	Removed           bool        `json:"removed"`
	OccurredAt        time.Time   `json:"occurred_at"`
}

// InboundEvent is one canonical event emitted from a provider webhook. Only the
// payload matching Type is expected to be populated.
type InboundEvent struct {
	DedupeKey       string               `json:"dedupe_key"`
	ProviderEventID string               `json:"provider_event_id,omitempty"`
	Type            NormalizedEventType  `json:"type"`
	OccurredAt      time.Time            `json:"occurred_at"`
	Message         *InboundMessage      `json:"message,omitempty"`
	MessageStatus   *MessageStatusUpdate `json:"message_status,omitempty"`
	Read            *ReadReceipt         `json:"read,omitempty"`
	Reaction        *Reaction            `json:"reaction,omitempty"`
	Payload         map[string]any       `json:"payload,omitempty"`
}

type MediaRef struct {
	ID                string         `json:"id,omitempty"`
	URL               string         `json:"url,omitempty"`
	MimeType          string         `json:"mime_type,omitempty"`
	Filename          string         `json:"filename,omitempty"`
	ExpectedSizeBytes int64          `json:"expected_size_bytes,omitempty"`
	Metadata          map[string]any `json:"metadata,omitempty"`
}

// FetchedMedia is a streamed provider object. The caller owns Content and must
// close it. Adapters should not buffer large media solely to satisfy this API.
type FetchedMedia struct {
	Content   io.ReadCloser  `json:"-"`
	MimeType  string         `json:"mime_type,omitempty"`
	Filename  string         `json:"filename,omitempty"`
	SizeBytes int64          `json:"size_bytes,omitempty"`
	Checksum  string         `json:"checksum,omitempty"`
	ExpiresAt *time.Time     `json:"expires_at,omitempty"`
	Metadata  map[string]any `json:"metadata,omitempty"`
}

type AccountValidationResult struct {
	Valid        bool           `json:"valid"`
	Status       string         `json:"status,omitempty"`
	Capabilities Capabilities   `json:"capabilities"`
	Warnings     []string       `json:"warnings,omitempty"`
	CheckedAt    time.Time      `json:"checked_at"`
	Metadata     map[string]any `json:"metadata,omitempty"`
}

type CredentialRefreshResult struct {
	CredentialBlob models.JSONB `json:"-"`
	KeyVersion     string       `json:"key_version,omitempty"`
	ExpiresAt      *time.Time   `json:"expires_at,omitempty"`
	RefreshedAt    time.Time    `json:"refreshed_at"`
}

// Adapter translates one provider into the canonical channel contract.
// Implementations must be safe for concurrent use.
type Adapter interface {
	Channel() models.Channel
	Provider() string
	Capabilities(account *models.ChannelAccount) Capabilities

	RouteHint(headers http.Header, body []byte) (WebhookRouteHint, error)
	VerifyWebhook(account *models.ChannelAccount, headers http.Header, body []byte) error
	NormalizeWebhook(ctx context.Context, account *models.ChannelAccount, body []byte) ([]InboundEvent, error)

	Send(ctx context.Context, account *models.ChannelAccount, message OutboundMessage) (SendResult, error)
	MarkRead(ctx context.Context, account *models.ChannelAccount, conversation ConversationRef, externalMessageIDs []string) error
	FetchMedia(ctx context.Context, account *models.ChannelAccount, ref MediaRef) (FetchedMedia, error)

	ValidateAccount(ctx context.Context, account *models.ChannelAccount) (AccountValidationResult, error)
	Subscribe(ctx context.Context, account *models.ChannelAccount) error
	RefreshCredentials(ctx context.Context, account *models.ChannelAccount) (CredentialRefreshResult, error)
}

var (
	ErrNilAdapter               = errors.New("channel adapter is nil")
	ErrInvalidAdapterIdentity   = errors.New("channel adapter must provide a channel and provider")
	ErrAdapterAlreadyRegistered = errors.New("channel adapter already registered")
)

type registryKey struct {
	channel  models.Channel
	provider string
}

// Registry is a concurrency-safe adapter registry keyed by channel and
// provider. A duplicate is rejected rather than silently replacing live
// behavior.
type Registry struct {
	mu       sync.RWMutex
	adapters map[registryKey]Adapter
}

func NewRegistry() *Registry {
	return &Registry{adapters: make(map[registryKey]Adapter)}
}

func (r *Registry) Register(adapter Adapter) error {
	if isNilAdapter(adapter) {
		return ErrNilAdapter
	}

	key := adapterKey(adapter.Channel(), adapter.Provider())
	if key.channel == "" || key.provider == "" {
		return ErrInvalidAdapterIdentity
	}
	if r == nil {
		return errors.New("channel registry is nil")
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.adapters == nil {
		r.adapters = make(map[registryKey]Adapter)
	}
	if _, exists := r.adapters[key]; exists {
		return fmt.Errorf(
			"%w: channel=%s provider=%s",
			ErrAdapterAlreadyRegistered,
			key.channel,
			key.provider,
		)
	}
	r.adapters[key] = adapter
	return nil
}

func (r *Registry) Get(channel models.Channel, provider string) (Adapter, bool) {
	if r == nil {
		return nil, false
	}

	key := adapterKey(channel, provider)
	r.mu.RLock()
	defer r.mu.RUnlock()
	adapter, ok := r.adapters[key]
	return adapter, ok
}

// List returns a stable snapshot ordered by channel and provider.
func (r *Registry) List() []Adapter {
	if r == nil {
		return nil
	}

	r.mu.RLock()
	entries := make([]struct {
		key     registryKey
		adapter Adapter
	}, 0, len(r.adapters))
	for key, adapter := range r.adapters {
		entries = append(entries, struct {
			key     registryKey
			adapter Adapter
		}{key: key, adapter: adapter})
	}
	r.mu.RUnlock()

	sort.Slice(entries, func(i, j int) bool {
		if entries[i].key.channel == entries[j].key.channel {
			return entries[i].key.provider < entries[j].key.provider
		}
		return entries[i].key.channel < entries[j].key.channel
	})

	adapters := make([]Adapter, len(entries))
	for i := range entries {
		adapters[i] = entries[i].adapter
	}
	return adapters
}

// Capabilities resolves an adapter and returns account-specific capabilities.
func (r *Registry) Capabilities(channel models.Channel, provider string, account *models.ChannelAccount) (Capabilities, bool) {
	adapter, ok := r.Get(channel, provider)
	if !ok {
		return Capabilities{}, false
	}
	return adapter.Capabilities(account), true
}

func adapterKey(channel models.Channel, provider string) registryKey {
	return registryKey{
		channel:  models.Channel(strings.ToLower(strings.TrimSpace(string(channel)))),
		provider: strings.ToLower(strings.TrimSpace(provider)),
	}
}

func isNilAdapter(adapter Adapter) bool {
	if adapter == nil {
		return true
	}
	value := reflect.ValueOf(adapter)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

// ProviderError exposes provider failures without forcing callers to parse
// human-readable strings. Cause and RetryAfter are intentionally not serialized.
type ProviderError struct {
	Operation  string        `json:"operation,omitempty"`
	Provider   string        `json:"provider,omitempty"`
	Code       string        `json:"code,omitempty"`
	Message    string        `json:"message"`
	Retryable  bool          `json:"retryable"`
	RetryAfter time.Duration `json:"-"`
	Cause      error         `json:"-"`
}

func (e *ProviderError) Error() string {
	if e == nil {
		return ""
	}
	if e.Provider == "" {
		return e.Message
	}
	if e.Operation == "" {
		return fmt.Sprintf("%s: %s", e.Provider, e.Message)
	}
	return fmt.Sprintf("%s %s: %s", e.Provider, e.Operation, e.Message)
}

func (e *ProviderError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}
