package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
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

func TestNormalizeOutgoingProviderResultRequiresReceiptAuthority(t *testing.T) {
	t.Parallel()

	wamid, err := normalizeOutgoingProviderResult(" \t ", nil)
	require.ErrorIs(t, err, errOutgoingProviderReceiptMissing)
	assert.Empty(t, wamid)

	wamid, err = normalizeOutgoingProviderResult("  wamid.authoritative  ", nil)
	require.NoError(t, err)
	assert.Equal(t, "wamid.authoritative", wamid)
}

func TestOutgoingProviderPanicContainmentPreservesAttemptBoundary(t *testing.T) {
	t.Parallel()

	for _, afterAttempt := range []bool{false, true} {
		t.Run(fmt.Sprintf("after_attempt_%t", afterAttempt), func(t *testing.T) {
			attempted := false
			err := containOutgoingProviderPanic(&attempted, func() error {
				if afterAttempt {
					attempted = true
				}
				panic("synthetic provider panic")
			})

			var panicErr *outgoingProviderPanicError
			require.ErrorAs(t, err, &panicErr)
			assert.Equal(t, afterAttempt, panicErr.providerAttempted)
			assert.Equal(t, afterAttempt, attempted)
		})
	}
}

func TestOutgoingBlankProviderSuccessSettlesWithoutSentAuthority(t *testing.T) {
	for _, automatic := range []bool{false, true} {
		t.Run(fmt.Sprintf("automatic_%t", automatic), func(t *testing.T) {
			app, account, contact := whatsappIdentityFixture(t)
			options := DefaultSendOptions()
			options.Async = false
			options.BroadcastWebSocket = false
			options.DispatchWebhook = false
			options.TrackSLA = false
			if automatic {
				app.inboundContinuation = &inboundContinuationExecution{
					OrganizationID: account.OrganizationID,
					ContactID:      contact.ID,
					attemptGuarded: true,
				}
				options = ChatbotSendOptions()
				options.BroadcastWebSocket = false
				options.DispatchWebhook = false
				options.TrackSLA = false
				options.MarkIncomingRead = false
			}

			message, err := app.SendOutgoingMessage(context.Background(), OutgoingMessageRequest{
				Account: account,
				Contact: contact,
				Type:    models.MessageTypeText,
				Content: "blank provider receipt authority " + uuid.NewString(),
				deliveryOverride: func(context.Context, *models.Contact) (string, error) {
					return "  ", nil
				},
			}, options)
			require.NoError(t, err)
			require.NotNil(t, message)

			var stored models.Message
			require.NoError(t, app.DB.First(&stored, "id = ?", message.ID).Error)
			assert.Equal(t, models.MessageStatusFailed, stored.Status)
			assert.Empty(t, stored.WhatsAppMessageID)
			assert.Equal(t, errOutgoingProviderReceiptMissing.Error(), stored.ErrorMessage)
			if automatic {
				assert.Equal(
					t,
					automaticAIDispatchStateUncertain,
					stored.Metadata[automaticAIDispatchStateMetadataKey],
				)
			}
		})
	}
}

func TestAsyncOutgoingProviderPanicIsContainedAndSettled(t *testing.T) {
	app, account, contact := whatsappIdentityFixture(t)
	options := DefaultSendOptions()
	options.BroadcastWebSocket = false
	options.DispatchWebhook = false
	options.TrackSLA = false

	message, err := app.SendOutgoingMessage(context.Background(), OutgoingMessageRequest{
		Account: account,
		Contact: contact,
		Type:    models.MessageTypeText,
		Content: "contained async provider panic " + uuid.NewString(),
		deliveryOverride: func(context.Context, *models.Contact) (string, error) {
			panic("synthetic async provider panic")
		},
	}, options)
	require.NoError(t, err)
	require.NotNil(t, message)
	app.WaitForBackgroundTasks()

	var stored models.Message
	require.NoError(t, app.DB.First(&stored, "id = ?", message.ID).Error)
	assert.Equal(t, models.MessageStatusFailed, stored.Status)
	assert.Empty(t, stored.WhatsAppMessageID)
	assert.Contains(t, stored.ErrorMessage, "synthetic async provider panic")
}

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
				Name:           "recovery-account-" + test.name,
				PhoneID:        testutil.NewTestGraphObjectID(),
				BusinessID:     testutil.NewTestGraphObjectID(),
				AccessToken:    "synthetic-recovery-token",
				Status:         "active",
			}
			require.NoError(t, db.Create(account).Error)
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

func TestOutgoingDeliveryRequiresReceiptAuthorityBeforeProvider(t *testing.T) {
	for _, async := range []bool{false, true} {
		t.Run(fmt.Sprintf("async_%t", async), func(t *testing.T) {
			db := testutil.SetupTestDB(t)
			organization := testutil.CreateTestOrganization(t, db)
			contact := testutil.CreateTestContact(t, db, organization.ID)
			account := &models.WhatsAppAccount{
				BaseModel:      models.BaseModel{ID: uuid.New()},
				OrganizationID: organization.ID,
				Name:           "receipt-fence-" + uuid.NewString()[:8],
				PhoneID:        testutil.NewTestGraphObjectID(),
				BusinessID:     testutil.NewTestGraphObjectID(),
				AccessToken:    "synthetic-receipt-fence-token",
				Status:         "active",
			}
			require.NoError(t, db.Create(account).Error)
			app := &App{DB: db, Config: &config.Config{}, Log: testutil.NopLogger()}

			mirrorErr := errors.New("forced receipt-authority mirror failure")
			callbackName := "test:receipt-authority-mirror:" + uuid.NewString()
			require.NoError(t, db.Callback().Create().Before("gorm:create").Register(
				callbackName,
				func(tx *gorm.DB) {
					if tx.Statement.Schema != nil && tx.Statement.Schema.Table == "channel_accounts" {
						_ = tx.AddError(mirrorErr)
					}
				},
			))
			t.Cleanup(func() { _ = db.Callback().Create().Remove(callbackName) })

			options := DefaultSendOptions()
			options.Async = async
			options.BroadcastWebSocket = false
			options.DispatchWebhook = false
			options.TrackSLA = false
			deliveryCalls := 0
			content := "receipt authority fence " + uuid.NewString()
			message, err := app.SendOutgoingMessage(context.Background(), OutgoingMessageRequest{
				Account: account,
				Contact: contact,
				Type:    models.MessageTypeText,
				Content: content,
				deliveryOverride: func(context.Context, *models.Contact) (string, error) {
					deliveryCalls++
					return "wamid.must-not-be-sent", nil
				},
			}, options)
			if async {
				require.NoError(t, err)
				require.NotNil(t, message)
				app.WaitForBackgroundTasks()
			} else {
				require.ErrorContains(t, err, mirrorErr.Error())
			}
			assert.Zero(t, deliveryCalls, "mirror failure must remain before the provider boundary")

			var stored models.Message
			require.NoError(t, db.Where(
				"organization_id = ? AND contact_id = ? AND content = ?",
				organization.ID,
				contact.ID,
				content,
			).First(&stored).Error)
			assert.Equal(t, models.MessageStatusFailed, stored.Status)
			assert.Empty(t, stored.WhatsAppMessageID)
			assert.ErrorContains(t, errors.New(stored.ErrorMessage), mirrorErr.Error())
			assert.Nil(t, stored.InboxConversationID)
		})
	}
}

func TestOutgoingDeliveryCreatesImmediateReceiptAndReactionAuthority(t *testing.T) {
	app, account, contact := whatsappIdentityFixture(t)
	options := DefaultSendOptions()
	options.Async = false
	options.BroadcastWebSocket = false
	options.DispatchWebhook = false
	options.TrackSLA = false
	wamid := "wamid.ordinary-outgoing-" + uuid.NewString()

	message, err := app.SendOutgoingMessage(context.Background(), OutgoingMessageRequest{
		Account: account,
		Contact: contact,
		Type:    models.MessageTypeText,
		Content: "ordinary outgoing receipt authority",
		deliveryOverride: func(context.Context, *models.Contact) (string, error) {
			return wamid, nil
		},
	}, options)
	require.NoError(t, err)
	require.NotNil(t, message)

	var stored models.Message
	require.NoError(t, app.DB.First(&stored, "id = ?", message.ID).Error)
	require.NotNil(t, stored.InboxConversationID,
		"a successful provider attempt must already have durable receipt authority")

	require.NoError(t, app.processStatusUpdate(account.PhoneID, WebhookStatus{ID: wamid, Status: "delivered"}))
	require.NoError(t, app.DB.First(&stored, "id = ?", message.ID).Error)
	assert.Equal(t, models.MessageStatusDelivered, stored.Status)

	app.handleIncomingReaction(account, contact.PhoneNumber, wamid, "👍", "Patient")
	require.NoError(t, app.DB.First(&stored, "id = ?", message.ID).Error)
	assert.NotEmpty(t, stored.Metadata["reactions"])
}

func TestOutgoingStatusReceiptRetriesUntilProviderWAMIDCommits(t *testing.T) {
	app, account, contact := whatsappIdentityFixture(t)
	app.Config = &config.Config{
		WhatsApp: config.WhatsAppConfig{AppSecret: webhookTestAppSecret},
	}
	account.AppSecret = webhookTestAppSecret
	require.NoError(t, app.DB.Model(&models.WhatsAppAccount{}).Where(
		"id = ? AND organization_id = ?",
		account.ID,
		account.OrganizationID,
	).Update("app_secret", account.AppSecret).Error)
	app.InvalidateWhatsAppAccountCache(account.PhoneID)

	wamid := "wamid.receipt-before-sender-commit-" + uuid.NewString()
	body, err := json.Marshal(map[string]any{
		"object": "whatsapp_business_account",
		"entry": []any{map[string]any{
			"id": account.BusinessID,
			"changes": []any{map[string]any{
				"field": "messages",
				"value": map[string]any{
					"messaging_product": "whatsapp",
					"metadata": map[string]any{
						"phone_number_id": account.PhoneID,
					},
					"statuses": []any{map[string]any{
						"id":     wamid,
						"status": "delivered",
					}},
				},
			}},
		}},
	})
	require.NoError(t, err)
	sendReceipt := func() (int, error) {
		req := testutil.NewRequest(t)
		req.RequestCtx.Request.Header.SetMethod(http.MethodPost)
		req.RequestCtx.Request.Header.SetContentType("application/json")
		req.RequestCtx.Request.Header.Set(
			"X-Hub-Signature-256",
			webhookTestSignature(body),
		)
		req.RequestCtx.Request.SetBody(body)
		err := app.WebhookHandler(req)
		return req.RequestCtx.Response.StatusCode(), err
	}

	options := DefaultSendOptions()
	options.Async = false
	options.BroadcastWebSocket = false
	options.DispatchWebhook = false
	options.TrackSLA = false
	firstReceiptStatus := 0
	var firstReceiptErr error
	message, err := app.SendOutgoingMessage(context.Background(), OutgoingMessageRequest{
		Account: account,
		Contact: contact,
		Type:    models.MessageTypeText,
		Content: "receipt can arrive before sender commit",
		deliveryOverride: func(context.Context, *models.Contact) (string, error) {
			firstReceiptStatus, firstReceiptErr = sendReceipt()
			return wamid, nil
		},
	}, options)
	require.NoError(t, err)
	require.NotNil(t, message)
	require.NoError(t, firstReceiptErr)
	assert.Equal(t, http.StatusServiceUnavailable, firstReceiptStatus,
		"a receipt that cannot yet resolve its WAMID must not be acknowledged")

	replayedStatus, replayedErr := sendReceipt()
	require.NoError(t, replayedErr)
	assert.Equal(t, http.StatusOK, replayedStatus)
	var stored models.Message
	require.NoError(t, app.DB.First(&stored, "id = ?", message.ID).Error)
	assert.Equal(t, models.MessageStatusDelivered, stored.Status)
	assert.Equal(t, wamid, stored.WhatsAppMessageID)
}

func TestAutomaticAIProviderAttemptHoldsAccountLifecycleLock(t *testing.T) {
	app, account, contact := whatsappIdentityFixture(t)
	app.inboundContinuation = &inboundContinuationExecution{
		OrganizationID: account.OrganizationID,
		ContactID:      contact.ID,
		attemptGuarded: true,
	}

	disconnectPID := make(chan int, 1)
	disconnectDone := make(chan error, 1)
	providerCalls := 0
	lockObserved := false
	options := ChatbotSendOptions()
	options.BroadcastWebSocket = false
	options.DispatchWebhook = false
	options.TrackSLA = false
	wamid := "wamid.automatic-account-lock-" + uuid.NewString()
	message, err := app.SendOutgoingMessage(context.Background(), OutgoingMessageRequest{
		Account: account,
		Contact: contact,
		Type:    models.MessageTypeText,
		Content: "automatic send remains fenced through provider",
		deliveryOverride: func(context.Context, *models.Contact) (string, error) {
			providerCalls++
			go func() {
				disconnectDone <- app.DB.Connection(func(connection *gorm.DB) error {
					session := connection.Session(&gorm.Session{NewDB: true})
					var backendPID int
					if err := session.Raw("SELECT pg_backend_pid()").Scan(&backendPID).Error; err != nil {
						disconnectPID <- 0
						return err
					}
					disconnectPID <- backendPID
					return session.Model(&models.WhatsAppAccount{}).Where(
						"id = ? AND organization_id = ?",
						account.ID,
						account.OrganizationID,
					).Update("status", "disconnected").Error
				})
			}()
			backendPID := <-disconnectPID
			if backendPID <= 0 {
				return "", errors.New("failed to identify disconnect backend")
			}
			testutil.RequirePostgresBackendWaitingForLock(t, app.DB, backendPID)
			select {
			case disconnectErr := <-disconnectDone:
				return "", fmt.Errorf(
					"disconnect bypassed provider account lock: %w",
					disconnectErr,
				)
			default:
				lockObserved = true
			}
			return wamid, nil
		},
	}, options)
	require.NoError(t, err)
	require.NotNil(t, message)
	require.True(t, lockObserved)
	require.Equal(t, 1, providerCalls)

	select {
	case disconnectErr := <-disconnectDone:
		require.NoError(t, disconnectErr)
	case <-time.After(5 * time.Second):
		require.Fail(t, "disconnect did not resume after provider attempt released its account lock")
	}
	var storedAccount models.WhatsAppAccount
	require.NoError(t, app.DB.First(&storedAccount, "id = ?", account.ID).Error)
	assert.Equal(t, "disconnected", storedAccount.Status)
	var storedMessage models.Message
	require.NoError(t, app.DB.First(&storedMessage, "id = ?", message.ID).Error)
	assert.Equal(t, models.MessageStatusSent, storedMessage.Status)
	assert.Equal(t, wamid, storedMessage.WhatsAppMessageID)
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
