package handlers

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/internal/queue"
	"github.com/shridarpatil/whatomate/internal/websocket"
	"gorm.io/gorm"
)

const realtimePublishTimeout = 500 * time.Millisecond

type realtimeTransactionAppKey struct{}

func bindRealtimeAppToDB(db *gorm.DB, app *App) *gorm.DB {
	if db == nil || app == nil {
		return db
	}
	parent := context.Background()
	if db.Statement != nil && db.Statement.Context != nil {
		parent = db.Statement.Context
	}
	return db.WithContext(context.WithValue(parent, realtimeTransactionAppKey{}, app))
}

// publishTerminalMessageBatchTx records one bounded invalidation for a bulk
// terminal transition. The DB context is bound by scopedApp; publishRealtimeEvent
// therefore queues the hint until the outer tenant transaction commits. Raw
// transaction callers without an owner deliberately emit nothing rather than
// risk advertising rolled-back state.
func publishTerminalMessageBatchTx(
	tx *gorm.DB,
	organizationID uuid.UUID,
	channelAccountID, conversationID *uuid.UUID,
	affected int64,
) {
	if tx == nil || affected <= 0 || organizationID == uuid.Nil || tx.Statement == nil ||
		tx.Statement.Context == nil {
		return
	}
	app, _ := tx.Statement.Context.Value(realtimeTransactionAppKey{}).(*App)
	if app == nil {
		return
	}
	app.publishRealtimeEvent(queue.RealtimeEvent{
		OrganizationID:   organizationID,
		Kind:             queue.RealtimeEventMessageStatusChanged,
		ChannelAccountID: channelAccountID,
		ConversationID:   conversationID,
		Status:           string(models.MessageStatusFailed),
		EventCount:       int(affected),
	}, nil)
}

func (a *App) realtimeSourceID() string {
	root := a.rootApp()
	if root == nil {
		return ""
	}
	root.realtimeSourceOnce.Do(func() {
		if root.RealtimeSourceID == "" {
			root.RealtimeSourceID = uuid.NewString()
		}
	})
	return root.RealtimeSourceID
}

// publishRealtimeEvent defers publication until the surrounding tenant
// transaction commits. Local delivery does not depend on Redis; Redis fans the
// identifier-only invalidation out to clients connected to other replicas.
func (a *App) publishRealtimeEvent(event queue.RealtimeEvent, fallback *websocket.WSMessage) {
	if a == nil || event.OrganizationID == uuid.Nil {
		return
	}
	eventCopy := event
	a.afterTenantCommit(func() {
		root := a.rootApp()
		if root == nil {
			return
		}
		eventCopy.SourceID = root.realtimeSourceID()
		if eventCopy.EventID == uuid.Nil {
			eventCopy.EventID = uuid.New()
		}
		if eventCopy.OccurredAt.IsZero() {
			eventCopy.OccurredAt = time.Now().UTC()
		}

		if root.WSHub != nil {
			if fallback != nil && fallback.Type != "" {
				root.WSHub.BroadcastToOrg(eventCopy.OrganizationID, *fallback)
			} else {
				root.WSHub.BroadcastToOrg(eventCopy.OrganizationID, websocket.WSMessage{
					Type:    websocket.TypeRealtimeSync,
					Payload: eventCopy,
				})
			}
		}

		if root.Redis == nil {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), realtimePublishTimeout)
		err := queue.NewPublisher(root.Redis, root.Log).PublishRealtime(ctx, &eventCopy)
		cancel()
		if err != nil {
			root.Log.Warn(
				"Realtime event publish failed; remote replicas will catch up on reconnect",
				"error", err,
				"organization_id", eventCopy.OrganizationID,
				"kind", eventCopy.Kind,
			)
		}
	})
}

// StartRealtimeSubscriber fans shared invalidation hints into this replica's
// in-memory WebSocket hub. The subscriber validates tenant scope first.
func (a *App) StartRealtimeSubscriber() error {
	if a == nil || a.WSHub == nil {
		return errors.New("realtime WebSocket hub is unavailable")
	}
	root := a.rootApp()
	root.realtimeSubscriberLive.Store(false)
	root.realtimeSourceID()
	ctx, cancel := context.WithCancel(context.Background())
	subscriber := queue.NewSubscriber(root.Redis, root.Log)
	if err := subscriber.SubscribeRealtime(ctx, func(event *queue.RealtimeEvent) {
		if event == nil || event.OrganizationID == uuid.Nil {
			return
		}
		if event.SourceID != "" && event.SourceID == root.realtimeSourceID() {
			return
		}
		root.WSHub.BroadcastToOrg(event.OrganizationID, websocket.WSMessage{
			Type:    websocket.TypeRealtimeSync,
			Payload: event,
		})
	}); err != nil {
		cancel()
		return err
	}
	root.realtimeSubscriber.Store(subscriber)
	root.realtimeSubscriberLive.Store(subscriber.RealtimeLive())
	go func() {
		<-subscriber.Done()
		root.realtimeSubscriberLive.Store(false)
		root.Log.Error("Required realtime subscriber stopped")
	}()
	root.RealtimeSubCancel = func() {
		root.realtimeSubscriberLive.Store(false)
		cancel()
		_ = subscriber.Close()
	}
	root.Log.Info("Realtime subscriber started")
	return nil
}

// StopRealtimeSubscriber stops this replica's Redis subscription.
func (a *App) StopRealtimeSubscriber() {
	root := a.rootApp()
	if root != nil && root.RealtimeSubCancel != nil {
		root.RealtimeSubCancel()
		root.RealtimeSubCancel = nil
	}
}
