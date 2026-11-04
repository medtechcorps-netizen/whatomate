package queue

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/zerodha/logf"
)

const (
	// CampaignStatsChannel is the Redis pub/sub channel for campaign stats updates
	CampaignStatsChannel = "whatomate:campaign_stats"
	// RealtimeChannel carries tenant-scoped invalidation hints between workers and
	// API replicas. PostgreSQL remains canonical; message bodies never use it.
	RealtimeChannel = "whatomate:realtime:v1"
	// RealtimeUserChannel is intentionally separate from RealtimeChannel. Older
	// replicas only understand organization-wide delivery and must never consume
	// a user-targeted event during a rolling deployment.
	RealtimeUserChannel = "whatomate:realtime:user:v1"

	realtimeSubscriberPingInterval = 5 * time.Second
	realtimeSubscriberPingTimeout  = 2 * time.Second
	realtimeSubscriberRetryMin     = 100 * time.Millisecond
	realtimeSubscriberRetryMax     = 2 * time.Second
)

// RealtimeEventKind identifies which canonical data clients should refresh.
type RealtimeEventKind string

const (
	RealtimeEventMessageCreated       RealtimeEventKind = "message_created"
	RealtimeEventMessageStatusChanged RealtimeEventKind = "message_status_changed"
	RealtimeEventConversationChanged  RealtimeEventKind = "conversation_changed"
)

// RealtimeEvent is a provider-neutral invalidation hint. It contains only
// tenant-scoped identifiers, never customer content or provider payloads.
type RealtimeEvent struct {
	EventID          uuid.UUID         `json:"event_id"`
	OrganizationID   uuid.UUID         `json:"organization_id"`
	UserID           *uuid.UUID        `json:"user_id,omitempty"`
	SourceID         string            `json:"source_id,omitempty"`
	Kind             RealtimeEventKind `json:"kind"`
	ChannelAccountID *uuid.UUID        `json:"channel_account_id,omitempty"`
	ConversationID   *uuid.UUID        `json:"conversation_id,omitempty"`
	ContactID        *uuid.UUID        `json:"contact_id,omitempty"`
	MessageID        *uuid.UUID        `json:"message_id,omitempty"`
	Status           string            `json:"status,omitempty"`
	EventCount       int               `json:"event_count,omitempty"`
	OccurredAt       time.Time         `json:"occurred_at"`
}

func validateRealtimeEventScope(event *RealtimeEvent) error {
	if event == nil {
		return errors.New("realtime event is required")
	}
	if event.OrganizationID == uuid.Nil {
		return errors.New("realtime event organization is required")
	}
	if event.UserID != nil && *event.UserID == uuid.Nil {
		return errors.New("realtime event user is invalid")
	}
	switch event.Kind {
	case RealtimeEventMessageCreated,
		RealtimeEventMessageStatusChanged,
		RealtimeEventConversationChanged:
	default:
		return errors.New("realtime event kind is invalid")
	}
	return nil
}

func prepareRealtimeEvent(event *RealtimeEvent) error {
	if err := validateRealtimeEventScope(event); err != nil {
		return err
	}
	if event.EventID == uuid.Nil {
		event.EventID = uuid.New()
	}
	if event.OccurredAt.IsZero() {
		event.OccurredAt = time.Now().UTC()
	} else {
		event.OccurredAt = event.OccurredAt.UTC()
	}
	return nil
}

func validateRealtimeEvent(event *RealtimeEvent) error {
	if err := validateRealtimeEventScope(event); err != nil {
		return err
	}
	if event.EventID == uuid.Nil {
		return errors.New("realtime event ID is required")
	}
	if event.OccurredAt.IsZero() {
		return errors.New("realtime event occurrence time is required")
	}
	return nil
}

// CampaignStatsUpdate represents a campaign stats update message
type CampaignStatsUpdate struct {
	CampaignID     string                `json:"campaign_id"`
	OrganizationID uuid.UUID             `json:"organization_id"`
	Status         models.CampaignStatus `json:"status"`
	SentCount      int                   `json:"sent_count"`
	DeliveredCount int                   `json:"delivered_count"`
	ReadCount      int                   `json:"read_count"`
	FailedCount    int                   `json:"failed_count"`
}

// Publisher publishes messages to Redis pub/sub channels
type Publisher struct {
	client *redis.Client
	log    logf.Logger
}

// NewPublisher creates a new Redis publisher
func NewPublisher(client *redis.Client, log logf.Logger) *Publisher {
	return &Publisher{
		client: client,
		log:    log,
	}
}

// PublishCampaignStats publishes a campaign stats update
func (p *Publisher) PublishCampaignStats(ctx context.Context, update *CampaignStatsUpdate) error {
	payload, err := json.Marshal(update)
	if err != nil {
		return err
	}

	if err := p.client.Publish(ctx, CampaignStatsChannel, payload).Err(); err != nil {
		p.log.Error("Failed to publish campaign stats", "error", err, "campaign_id", update.CampaignID)
		return err
	}

	p.log.Debug("Published campaign stats update", "campaign_id", update.CampaignID, "status", update.Status)
	return nil
}

// PublishRealtime publishes a tenant-scoped canonical-data invalidation hint.
func (p *Publisher) PublishRealtime(ctx context.Context, event *RealtimeEvent) error {
	if err := prepareRealtimeEvent(event); err != nil {
		return err
	}
	payload, err := json.Marshal(event)
	if err != nil {
		return err
	}
	if p == nil || p.client == nil {
		return errors.New("realtime publisher is unavailable")
	}
	channel := RealtimeChannel
	if event.UserID != nil {
		channel = RealtimeUserChannel
	}
	if err := p.client.Publish(ctx, channel, payload).Err(); err != nil {
		p.log.Error(
			"Failed to publish realtime event",
			"error", err,
			"event_id", event.EventID,
			"organization_id", event.OrganizationID,
			"kind", event.Kind,
		)
		return err
	}
	return nil
}
