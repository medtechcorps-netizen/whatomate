// Package metarelay implements the small, text-only Meta transport used by
// ReReply's generic relay channel adapter.
package metarelay

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	channelapi "github.com/shridarpatil/whatomate/internal/channel"
	"github.com/shridarpatil/whatomate/internal/models"
)

const (
	MetaSignatureHeader    = "X-Hub-Signature-256"
	ReReplySignatureHeader = "X-ReReply-Signature-256"
	signaturePrefix        = "sha256="

	defaultInboundBodyLimit  = int64(channelapi.RelayWebhookMaxBodyBytes)
	defaultOutboundBodyLimit = int64(1 << 20)
)

var (
	ErrUnknownAccount       = errors.New("unknown Meta account")
	ErrWebhookAppMismatch   = errors.New("meta account is bound to a different webhook app")
	ErrUnsupportedObject    = errors.New("unsupported Meta webhook object")
	ErrInvalidMetaPayload   = errors.New("invalid Meta webhook payload")
	ErrInvalidSignature     = errors.New("invalid webhook signature")
	ErrCanonicalJobTooLarge = errors.New("canonical webhook event exceeds the ReReply body limit")
	ErrOutboundCollision    = errors.New("outbound idempotency key collision")
	ErrOutboundInFlight     = errors.New("outbound delivery is already in flight")
	ErrOutboundAmbiguous    = errors.New("outbound delivery outcome is ambiguous")
	ErrOutboundStoreFailure = errors.New("outbound idempotency store failure")
)

// WebhookApp identifies the Meta application whose secret and verify token
// protect a webhook route. The Messenger parent app also owns Instagram
// accounts connected through Facebook Login. Instagram Login accounts belong
// exclusively to the separate Instagram app.
type WebhookApp string

const RelayServiceTokenHeader = "X-ReReply-Relay-Service-Token"

const InboundJobSchemaVersion = 2

const (
	WebhookAppMessenger      WebhookApp = "messenger"
	WebhookAppInstagramLogin WebhookApp = "instagram_login"
)

// InstagramAPIMode is an explicit, immutable binding between an Instagram
// account, its token family, and the Graph host used for every request.
type InstagramAPIMode string

const (
	InstagramAPIModeInstagramLogin InstagramAPIMode = "instagram_login"
	InstagramAPIModeFacebookLogin  InstagramAPIMode = "facebook_login"
)

// AccountConfig maps one Meta Page or Instagram professional account to one
// ReReply relay ChannelAccount. Secret values are resolved from the named
// environment variables and are never accepted inside the JSON mapping.
type AccountConfig struct {
	Key                       string           `json:"key"`
	Channel                   models.Channel   `json:"channel"`
	ExternalAccountID         string           `json:"external_account_id"`
	InstagramAPIMode          InstagramAPIMode `json:"instagram_api_mode,omitempty"`
	ReReplyWebhookURL         string           `json:"rereply_webhook_url"`
	AccessTokenEnv            string           `json:"access_token_env"`
	ReReplyInboundSecretEnv   string           `json:"rereply_inbound_secret_env"`
	ReReplyOutboundSecretEnv  string           `json:"rereply_outbound_secret_env"`
	accessToken               string
	reReplyInboundSecret      string
	reReplyOutboundSecret     string
	registryCredentialID      string
	registryCredentialVersion int
	registryWebhookID         string
	registryWebhookVersion    int
	registryLeaseExpiresAt    time.Time
	registryManaged           bool
}

// Config is intentionally environment-only. Production does not have a flag
// that permits plaintext HTTP provider or ReReply endpoints.
type Config struct {
	ListenAddr                string
	RedisURL                  string
	RedisPrefix               string
	MessengerAppSecret        string
	MessengerVerifyToken      string
	InstagramLoginAppSecret   string
	InstagramLoginVerifyToken string
	GraphAPIVersion           string
	Accounts                  []AccountConfig
	InboundRetention          time.Duration
	OutboundRetention         time.Duration
	ProcessingLease           time.Duration
	ForwardTimeout            time.Duration
	PollInterval              time.Duration
	WorkerConcurrency         int
	MaxAttempts               int
	RegistryURL               string
	RegistrySecret            string
	RegistryEdgeSecret        string
	RegistryCacheTTL          time.Duration
	RegistryTimeout           time.Duration

	// allowInsecureTestEndpoints can only be set by package tests/options. It
	// is deliberately not sourced from an environment variable.
	allowInsecureTestEndpoints bool
	// registryLifecycleTestMode is package-private so no deployment setting can
	// enable the incomplete split-A lifecycle. Tests may exercise the protocol
	// while production LoadConfig remains static-only.
	registryLifecycleTestMode bool
	byExternal                map[string]*AccountConfig
	byKey                     map[string]*AccountConfig
}

func accountIndexKey(channel models.Channel, externalID string) string {
	return string(channel) + "\x00" + externalID
}

func (c *Config) account(channel models.Channel, externalID string) (*AccountConfig, bool) {
	if c == nil {
		return nil, false
	}
	account, ok := c.byExternal[accountIndexKey(channel, externalID)]
	return account, ok
}

func (c *Config) accountByKey(key string) (*AccountConfig, bool) {
	if c == nil {
		return nil, false
	}
	account, ok := c.byKey[key]
	return account, ok
}

func (a *AccountConfig) webhookApp() WebhookApp {
	if a != nil &&
		a.Channel == models.ChannelInstagram &&
		a.InstagramAPIMode == InstagramAPIModeInstagramLogin {
		return WebhookAppInstagramLogin
	}
	return WebhookAppMessenger
}

// InboundJob is the durable unit forwarded to one ReReply channel webhook.
// Body is the exact canonical JSON signed and sent to ReReply.
type InboundJob struct {
	SchemaVersion     int            `json:"schema_version,omitempty"`
	ID                string         `json:"id"`
	AccountKey        string         `json:"account_key"`
	Channel           models.Channel `json:"channel,omitempty"`
	ExternalAccountID string         `json:"external_account_id,omitempty"`
	RegistryFence     string         `json:"registry_fence,omitempty"`
	Body              []byte         `json:"body"`
	Attempts          int            `json:"-"`
}

type AcceptanceStore interface {
	Ping(ctx context.Context) error
	AcceptInbound(ctx context.Context, acceptanceID string, jobs []InboundJob) (bool, error)
}

type QueueStore interface {
	ClaimInbound(ctx context.Context, now time.Time, lease time.Duration, limit int) ([]InboundJob, error)
	RequeueExpired(ctx context.Context, now time.Time, limit int) (int, error)
	CompleteInbound(ctx context.Context, jobID string) error
	RetryInbound(ctx context.Context, jobID string, next time.Time, maxAttempts int, reason string) (attempts int, dead bool, err error)
	ParkInbound(ctx context.Context, jobID string, next time.Time) error
	DeadInbound(ctx context.Context, jobID, reason string) error
}

type OutboundClaimState string

const (
	OutboundClaimAcquired  OutboundClaimState = "acquired"
	OutboundClaimCompleted OutboundClaimState = "completed"
	OutboundClaimRejected  OutboundClaimState = "rejected"
	OutboundClaimInFlight  OutboundClaimState = "in_flight"
	OutboundClaimAmbiguous OutboundClaimState = "ambiguous"
	OutboundClaimCollision OutboundClaimState = "collision"
)

type OutboundClaim struct {
	State  OutboundClaimState
	Status int
	Result json.RawMessage
}

type OutboundStore interface {
	ClaimOutbound(ctx context.Context, key, digest string, now time.Time) (OutboundClaim, error)
	CompleteOutbound(ctx context.Context, key, digest string, status int, result []byte, rejected bool) error
	ReleaseOutbound(ctx context.Context, key, digest string) error
	MarkOutboundAmbiguous(ctx context.Context, key, digest string) error
}

type ServerStore interface {
	AcceptanceStore
	OutboundStore
}

type relayOutboundEnvelope struct {
	Type              string          `json:"type"`
	Channel           models.Channel  `json:"channel"`
	ExternalAccountID string          `json:"external_account_id"`
	Data              json.RawMessage `json:"data"`
}

type graphSendResponse struct {
	RecipientID string `json:"recipient_id"`
	MessageID   string `json:"message_id"`
}

type relaySendResponse struct {
	ProviderMessageIDs     []string `json:"provider_message_ids"`
	ExternalConversationID string   `json:"external_conversation_id,omitempty"`
	ProviderRequestID      string   `json:"provider_request_id,omitempty"`
}

type Server struct {
	config             *Config
	store              ServerStore
	client             *http.Client
	logger             *slog.Logger
	now                func() time.Time
	facebookGraphBase  string
	instagramGraphBase string
	outboundTimeout    time.Duration
	settlementTimeout  time.Duration
	registry           RegistryResolver
}

type Worker struct {
	config   *Config
	store    QueueStore
	client   *http.Client
	logger   *slog.Logger
	now      func() time.Time
	registry RegistryResolver
}
