package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/config"
	"github.com/shridarpatil/whatomate/internal/models"
	appwebsocket "github.com/shridarpatil/whatomate/internal/websocket"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestOutgoingDeliveryRecoveryOwnsFreshContextAfterProviderAttempt(t *testing.T) {
	tests := []struct {
		name        string
		providerID  string
		providerErr error
		wantStatus  models.MessageStatus
	}{
		{name: "sent", providerID: "wamid.recovered", wantStatus: models.MessageStatusSent},
		{name: "failed", providerErr: errors.New("provider rejected recovered send"), wantStatus: models.MessageStatusFailed},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			db := testutil.SetupTestDB(t)
			organization := testutil.CreateTestOrganization(t, db)
			contact := testutil.CreateTestContact(t, db, organization.ID)
			log := testutil.NopLogger()
			hub := appwebsocket.NewHub(log)
			go hub.Run()
			client := appwebsocket.NewClient(hub, nil, uuid.New(), organization.ID)
			hub.Register(client)
			testutil.AssertEventually(t, func() bool { return hub.GetClientCount() == 1 }, 2*time.Second, "websocket client registered")

			app := &App{DB: db, Config: &config.Config{}, Log: log, WSHub: hub}
			account := &models.WhatsAppAccount{
				BaseModel:      models.BaseModel{ID: uuid.New()},
				OrganizationID: organization.ID,
				Name:           "recovery-account",
				Status:         "active",
			}
			providerContext, cancelProvider := context.WithCancel(context.Background())
			options := DefaultSendOptions()
			options.Async = false
			options.DispatchWebhook = false
			deliveryCalls := 0
			message, err := app.SendOutgoingMessage(providerContext, OutgoingMessageRequest{
				Account: account,
				Contact: contact,
				Type:    models.MessageTypeText,
				Content: "settle after caller cancellation",
				deliveryOverride: func(context.Context, *models.Contact) (string, error) {
					deliveryCalls++
					// Simulate a provider result arriving exactly as the caller deadline
					// expires. Persistence on the original transaction must fail, then
					// recovery must settle without invoking this function again.
					cancelProvider()
					return test.providerID, test.providerErr
				},
			}, options)
			require.NoError(t, err)
			require.NotNil(t, message)
			require.Error(t, providerContext.Err(), "provider attempt must expire the caller context")
			require.Equal(t, 1, deliveryCalls, "settlement recovery must never resend to the provider")
			assert.Equal(t, test.wantStatus, message.Status)

			var persisted models.Message
			require.NoError(t, db.First(&persisted, "id = ?", message.ID).Error)
			assert.Equal(t, test.wantStatus, persisted.Status)
			assert.Equal(t, test.providerID, persisted.WhatsAppMessageID)
			if test.providerErr != nil {
				assert.Equal(t, test.providerErr.Error(), persisted.ErrorMessage)
			}
			response := messageResponse(message, nil)
			assert.Equal(t, test.wantStatus, response.Status)

			statusEnvelope := receiveTerminalWSEnvelope(t, client.SendChan())
			newMessageEnvelope := receiveTerminalWSEnvelope(t, client.SendChan())
			assert.Equal(t, appwebsocket.TypeStatusUpdate, statusEnvelope.Type)
			assert.Equal(t, string(test.wantStatus), statusEnvelope.Payload["status"])
			assert.Equal(t, appwebsocket.TypeNewMessage, newMessageEnvelope.Type)
			assert.Equal(t, string(test.wantStatus), newMessageEnvelope.Payload["status"])
			assert.NotEqual(t, string(models.MessageStatusPending), newMessageEnvelope.Payload["status"])
		})
	}
}

func TestBulkTerminalPathsPublishOneHintOnlyAfterCommit(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*testing.T, *models.InboxConversation, *gorm.DB)
		run   func(*App, *models.InboxConversation) error
	}{
		{
			name: "managed Meta generation cancellation",
			run: func(scoped *App, conversation *models.InboxConversation) error {
				return cancelManagedMetaQueuedWorkForAccountTx(
					scoped.DB, conversation.OrganizationID, conversation.ChannelAccountID, "test fence",
				)
			},
		},
		{
			name: "AI outbox cancellation",
			run: func(scoped *App, conversation *models.InboxConversation) error {
				return cancelChannelAIOutboxJobsForConversationTx(
					scoped.DB, conversation.OrganizationID, conversation.ID, "test pause",
				)
			},
		},
		{
			name: "Threads disconnect cancellation",
			setup: func(t *testing.T, conversation *models.InboxConversation, db *gorm.DB) {
				require.NoError(t, db.Model(&models.ChannelAccount{}).Where(
					"id = ? AND organization_id = ?", conversation.ChannelAccountID, conversation.OrganizationID,
				).Updates(map[string]any{
					"channel":  models.ChannelThreads,
					"provider": "threads",
				}).Error)
			},
			run: func(scoped *App, conversation *models.InboxConversation) error {
				var account models.ChannelAccount
				if err := scoped.DB.First(&account, "id = ?", conversation.ChannelAccountID).Error; err != nil {
					return err
				}
				return disconnectLockedThreadsChannelAccounts(
					scoped.DB,
					conversation.OrganizationID,
					nil,
					time.Now().UTC(),
					[]models.ChannelAccount{account},
					nil,
				)
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			db := testutil.SetupTestDB(t)
			organization := testutil.CreateTestOrganization(t, db)
			conversation := createAIControlConversation(t, db, organization.ID)
			if test.setup != nil {
				test.setup(t, conversation, db)
			}
			_, committedMessage := createAIControlOutboxJob(t, db, conversation, models.OutboxJobStatusPending)
			log := testutil.NopLogger()
			hub := appwebsocket.NewHub(log)
			go hub.Run()
			client := appwebsocket.NewClient(hub, nil, uuid.New(), organization.ID)
			hub.Register(client)
			testutil.AssertEventually(t, func() bool { return hub.GetClientCount() == 1 }, 2*time.Second, "websocket client registered")
			app := &App{DB: db, Config: &config.Config{}, Log: log, WSHub: hub}

			require.NoError(t, app.WithCommittedTenantApp(organization.ID, func(scoped *App) error {
				return test.run(scoped, conversation)
			}))
			event := receiveRealtimeEnvelope(t, client.SendChan())
			assert.Equal(t, string(models.MessageStatusFailed), event.Payload.Status)
			assert.Equal(t, 1, event.Payload.EventCount)
			var persisted models.Message
			require.NoError(t, db.First(&persisted, "id = ?", committedMessage.ID).Error)
			assert.Equal(t, models.MessageStatusFailed, persisted.Status)

			_, rolledBackMessage := createAIControlOutboxJob(t, db, conversation, models.OutboxJobStatusPending)
			rollbackErr := errors.New("force terminal transition rollback")
			err := app.WithCommittedTenantApp(organization.ID, func(scoped *App) error {
				if err := test.run(scoped, conversation); err != nil {
					return err
				}
				return rollbackErr
			})
			require.ErrorIs(t, err, rollbackErr)
			persisted = models.Message{}
			require.NoError(t, db.First(&persisted, "id = ?", rolledBackMessage.ID).Error)
			assert.Equal(t, models.MessageStatusPending, persisted.Status)
			select {
			case unexpected := <-client.SendChan():
				t.Fatalf("rolled-back terminal transition emitted realtime: %s", unexpected)
			case <-time.After(150 * time.Millisecond):
			}
		})
	}
}

type terminalWSEnvelope struct {
	Type    string         `json:"type"`
	Payload map[string]any `json:"payload"`
}

func receiveTerminalWSEnvelope(t *testing.T, messages <-chan []byte) terminalWSEnvelope {
	t.Helper()
	select {
	case data := <-messages:
		var envelope terminalWSEnvelope
		require.NoError(t, json.Unmarshal(data, &envelope))
		return envelope
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for terminal WebSocket envelope")
		return terminalWSEnvelope{}
	}
}
