package queue

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
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

// Subscriber subscribes to Redis pub/sub channels
type Subscriber struct {
	client     *redis.Client
	log        logf.Logger
	pubsubMu   sync.Mutex
	pubsub     *redis.PubSub
	done       chan struct{}
	doneOnce   sync.Once
	live       atomic.Bool
	reconnects atomic.Uint64
}

// NewSubscriber creates a new Redis subscriber
func NewSubscriber(client *redis.Client, log logf.Logger) *Subscriber {
	return &Subscriber{
		client: client,
		log:    log,
	}
}

// SubscribeCampaignStats subscribes to campaign stats updates
// The handler is called for each received update
func (s *Subscriber) SubscribeCampaignStats(ctx context.Context, handler func(update *CampaignStatsUpdate)) error {
	pubsub := s.client.Subscribe(ctx, CampaignStatsChannel)
	s.setPubSub(pubsub)

	// Wait for subscription confirmation
	_, err := pubsub.Receive(ctx)
	if err != nil {
		return err
	}

	s.log.Info("Subscribed to campaign stats channel")

	// Start receiving messages
	ch := pubsub.Channel()
	go func() {
		for {
			select {
			case <-ctx.Done():
				s.log.Info("Campaign stats subscriber shutting down")
				return
			case msg, ok := <-ch:
				if !ok {
					s.log.Info("Campaign stats channel closed")
					return
				}

				var update CampaignStatsUpdate
				if err := json.Unmarshal([]byte(msg.Payload), &update); err != nil {
					s.log.Error("Failed to unmarshal campaign stats update", "error", err)
					continue
				}

				handler(&update)
			}
		}
	}()

	return nil
}

// SubscribeRealtime subscribes an API replica to tenant-scoped invalidation
// hints. Invalid or unscoped messages are discarded before reaching the hub.
func (s *Subscriber) SubscribeRealtime(ctx context.Context, handler func(event *RealtimeEvent)) error {
	if s == nil || s.client == nil {
		return errors.New("realtime subscriber is unavailable")
	}
	if handler == nil {
		return errors.New("realtime subscriber handler is required")
	}
	pubsub, err := s.confirmRealtimeSubscription(ctx)
	if err != nil {
		return err
	}
	s.setPubSub(pubsub)

	s.log.Info("Subscribed to realtime channel")
	s.done = make(chan struct{})
	s.live.Store(true)
	go s.runRealtimeSubscriber(ctx, pubsub, handler)
	return nil
}

func (s *Subscriber) confirmRealtimeSubscription(ctx context.Context) (*redis.PubSub, error) {
	channels := []string{RealtimeChannel, RealtimeUserChannel}
	pubsub := s.client.Subscribe(ctx, channels...)
	confirmed := make(map[string]struct{}, len(channels))
	for len(confirmed) < len(channels) {
		received, err := pubsub.Receive(ctx)
		if err != nil {
			_ = pubsub.Close()
			return nil, err
		}
		subscription, ok := received.(*redis.Subscription)
		if !ok || subscription.Kind != "subscribe" {
			continue
		}
		for _, expected := range channels {
			if subscription.Channel == expected {
				confirmed[expected] = struct{}{}
				break
			}
		}
	}
	return pubsub, nil
}

func (s *Subscriber) runRealtimeSubscriber(
	ctx context.Context,
	pubsub *redis.PubSub,
	handler func(event *RealtimeEvent),
) {
	defer func() {
		s.live.Store(false)
		s.doneOnce.Do(func() { close(s.done) })
	}()

	for {
		ch := pubsub.Channel()
		pingTicker := time.NewTicker(realtimeSubscriberPingInterval)
		interrupted := false
		for !interrupted {
			select {
			case <-ctx.Done():
				pingTicker.Stop()
				s.log.Info("Realtime subscriber shutting down")
				return
			case <-pingTicker.C:
				pingContext, cancelPing := context.WithTimeout(ctx, realtimeSubscriberPingTimeout)
				pingErr := pubsub.Ping(pingContext)
				cancelPing()
				if pingErr != nil {
					s.log.Warn("Realtime subscription health check failed", "error", pingErr)
					interrupted = true
				}
			case msg, ok := <-ch:
				if !ok {
					s.log.Warn("Realtime subscription channel closed unexpectedly")
					interrupted = true
					continue
				}
				var event RealtimeEvent
				if err := json.Unmarshal([]byte(msg.Payload), &event); err != nil {
					s.log.Error("Failed to unmarshal realtime event", "error", err)
					continue
				}
				if err := validateRealtimeEvent(&event); err != nil {
					s.log.Warn("Discarding invalid realtime event", "error", err)
					continue
				}
				if (msg.Channel == RealtimeUserChannel && event.UserID == nil) ||
					(msg.Channel == RealtimeChannel && event.UserID != nil) {
					s.log.Warn(
						"Discarding realtime event from mismatched scope channel",
						"channel", msg.Channel,
						"event_id", event.EventID,
					)
					continue
				}
				func() {
					defer func() {
						if recovered := recover(); recovered != nil {
							s.log.Error("Realtime subscriber handler panicked", "panic", recovered)
						}
					}()
					handler(&event)
				}()
			}
		}
		pingTicker.Stop()
		s.live.Store(false)
		_ = pubsub.Close()
		if ctx.Err() != nil {
			return
		}

		retryDelay := realtimeSubscriberRetryMin
		for {
			timer := time.NewTimer(retryDelay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
			replacement, err := s.confirmRealtimeSubscription(ctx)
			if err == nil {
				pubsub = replacement
				s.setPubSub(replacement)
				s.reconnects.Add(1)
				s.live.Store(true)
				s.log.Info("Realtime subscription recovered", "reconnect_count", s.reconnects.Load())
				break
			}
			s.log.Warn("Realtime subscription recovery failed", "error", err, "retry_in", retryDelay)
			if retryDelay < realtimeSubscriberRetryMax {
				retryDelay *= 2
				if retryDelay > realtimeSubscriberRetryMax {
					retryDelay = realtimeSubscriberRetryMax
				}
			}
		}
	}
}

func (s *Subscriber) setPubSub(pubsub *redis.PubSub) {
	s.pubsubMu.Lock()
	s.pubsub = pubsub
	s.pubsubMu.Unlock()
}

// RealtimeLive reports whether the confirmed realtime subscription receive
// loop is still running.
func (s *Subscriber) RealtimeLive() bool {
	return s != nil && s.live.Load()
}

// RealtimeReconnectCount exposes confirmed receive-loop recoveries for health
// diagnostics and interruption tests.
func (s *Subscriber) RealtimeReconnectCount() uint64 {
	if s == nil {
		return 0
	}
	return s.reconnects.Load()
}

// Done is closed when the realtime receive loop exits.
func (s *Subscriber) Done() <-chan struct{} {
	if s == nil || s.done == nil {
		closed := make(chan struct{})
		close(closed)
		return closed
	}
	return s.done
}

// Close closes the subscriber
func (s *Subscriber) Close() error {
	if s == nil {
		return nil
	}
	s.pubsubMu.Lock()
	pubsub := s.pubsub
	s.pubsubMu.Unlock()
	if pubsub == nil {
		return nil
	}
	return pubsub.Close()
}
