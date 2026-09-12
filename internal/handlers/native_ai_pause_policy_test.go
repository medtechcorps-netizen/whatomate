package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/config"
	"github.com/shridarpatil/whatomate/internal/database"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/pkg/whatsapp"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
	"gorm.io/gorm"
)

func TestIncomingAutomaticAISuppressionIsStickyMetadata(t *testing.T) {
	message := &models.Message{
		Direction: models.DirectionIncoming,
		Metadata: models.JSONB{
			incomingAutomaticAISuppressedKey:        true,
			incomingAutomaticAISuppressionReasonKey: "identity_review_hold",
			incomingAutomaticAISuppressedAtKey:      "2026-09-06T00:00:00Z",
		},
	}
	assert.True(t, incomingMessageAutomaticAISuppressed(message))

	// Later policy state is intentionally not consulted here. Once the exact
	// WAMID is admitted as suppressed, Resume can affect only later messages.
	message.Metadata["current_conversation_ai_state"] = "active"
	assert.True(t, incomingMessageAutomaticAISuppressed(message))

	message.Direction = models.DirectionOutgoing
	assert.False(t, incomingMessageAutomaticAISuppressed(message))
}

func TestInboundActionIdentityUsesDurableScopeNotPayload(t *testing.T) {
	messageID := uuid.New()
	newExecution := func() *App {
		return &App{inboundContinuation: &inboundContinuationExecution{
			OrganizationID: uuid.New(),
			ContactID:      uuid.New(),
			MessageID:      messageID,
			WAMID:          "wamid.stable-action",
			actionScope:    "chat-graph:flow:node:visit:7",
		}}
	}

	first, err := newExecution().nextInboundContinuationActionKey(
		"graph_api_call",
		map[string]any{"secret": "first"},
	)
	require.NoError(t, err)
	second, err := newExecution().nextInboundContinuationActionKey(
		"graph_api_call",
		map[string]any{"secret": "changed"},
	)
	require.NoError(t, err)
	assert.Equal(t, first, second)
	assert.NotContains(t, first.Key, "secret")
}

func TestChatGraphVisitOrdinalRequiresDurableCanonicalPath(t *testing.T) {
	session := &models.ChatbotSession{SessionData: models.JSONB{}}
	ordinal, err := chatGraphVisitOrdinal(session)
	require.NoError(t, err)
	assert.Zero(t, ordinal)

	session.SessionData["__path__"] = []any{
		map[string]any{"node": "first"},
		map[string]any{"node": "second"},
	}
	ordinal, err = chatGraphVisitOrdinal(session)
	require.NoError(t, err)
	assert.Equal(t, 2, ordinal)

	session.SessionData["__path__"] = "caller-selected-ordinal"
	_, err = chatGraphVisitOrdinal(session)
	require.Error(t, err)
}

func TestChatGraphPublicMappingCannotOverwriteReservedIdentity(t *testing.T) {
	destination := models.JSONB{"__path__": []any{"durable"}}
	copyChatGraphPublicSessionData(destination, map[string]any{
		"customer_tier": "gold",
		"__path__":      []any{"forged"},
		" __private":    "forged",
	})
	assert.Equal(t, "gold", destination["customer_tier"])
	assert.Equal(t, []any{"durable"}, destination["__path__"])
	assert.NotContains(t, destination, " __private")
}

// nativePausePolicyTransfer uses the same atomic finalizer as a manual Pause.
// Directly inserting an AgentTransfer would miss the message/session revocation
// whose ordering these regressions exercise.
func nativePausePolicyTransfer(t *testing.T, app *App, account *models.WhatsAppAccount, contact *models.Contact) *models.AgentTransfer {
	t.Helper()
	transfer := &models.AgentTransfer{
		BaseModel:       models.BaseModel{ID: uuid.New()},
		OrganizationID:  account.OrganizationID,
		ContactID:       contact.ID,
		WhatsAppAccount: account.Name,
		PhoneNumber:     contact.PhoneNumber,
		Status:          models.TransferStatusActive,
		Source:          models.TransferSourceManual,
		TransferredAt:   time.Now().UTC(),
	}
	require.NoError(t, app.saveAndFinalizeTransfer(transfer, account, contact, nil, true))
	return transfer
}

func nativePausePolicyResume(t *testing.T, app *App, transfer *models.AgentTransfer) {
	t.Helper()
	require.NoError(t, app.DB.Transaction(func(tx *gorm.DB) error {
		if err := database.LockOrganizationPolicyScope(tx, transfer.OrganizationID); err != nil {
			return err
		}
		return tx.Model(&models.AgentTransfer{}).
			Where("id = ? AND organization_id = ? AND status = ?", transfer.ID, transfer.OrganizationID, models.TransferStatusActive).
			Updates(map[string]any{"status": models.TransferStatusResumed, "resumed_at": time.Now().UTC()}).Error
	}))
}

func nativePausePolicyProvider(t *testing.T, app *App, beforeResponse func()) *atomic.Int32 {
	t.Helper()
	calls := &atomic.Int32{}
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		call := calls.Add(1)
		if beforeResponse != nil {
			beforeResponse()
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"messages": []map[string]string{{"id": fmt.Sprintf("wamid.pause-policy-%d", call)}},
		})
	}))
	t.Cleanup(provider.Close)
	app.WhatsApp = whatsapp.NewWithBaseURL(app.Log, provider.URL)
	return calls
}

func TestNativePausePolicy_AdmittedOldWAMIDNeverReplaysAfterResume(t *testing.T) {
	app := newProcessorTestApp(t)
	// RLS-enabled webhook admission needs the migration-owned tenant resolver,
	// not only the ordinary table fixture. This existing helper installs the
	// normal RLS/routing contract and owns its role/policy cleanup. Keep this
	// schema-changing test serial, like the other runtime-role fixtures.
	testutil.CreateTestPlatformComplianceOrganization(t, app.DB, false, false)
	organization, account := createProcessorTestOrg(t, app)
	providerCalls := nativePausePolicyProvider(t, app, nil)
	phone := "6031" + uuid.NewString()[:8]
	oldInbound := inboundContinuationTextMessage(t, "wamid.pre-pause-"+uuid.NewString(), phone, "Old admitted work")
	work, duplicate, err := app.persistIncomingMessageBeforeAck(account.PhoneID, oldInbound, "Synthetic pause patient")
	require.NoError(t, err)
	require.False(t, duplicate)
	require.False(t, incomingMessageAutomaticAISuppressed(&work.Persisted))
	require.Equal(t, models.ScheduledJobStatusPending, loadInboundContinuationJob(t, app, organization.ID, work.Persisted.ID).Status)

	transfer := nativePausePolicyTransfer(t, app, account, &work.Contact)
	nativePausePolicyResume(t, app, transfer)
	var persisted models.Message
	require.NoError(t, app.DB.First(&persisted, "id = ? AND organization_id = ?", work.Persisted.ID, organization.ID).Error)
	require.True(t, incomingMessageAutomaticAISuppressed(&persisted))
	assert.Equal(t, database.AutomaticReplyBlockedHumanHandover, persisted.Metadata[incomingAutomaticAISuppressionReasonKey])
	assert.False(t, incomingMessageAutomaticAISuppressed(&work.Persisted), "the pre-Pause loaded copy deliberately remains stale")

	// Force the physical boundary directly with the already-loaded identity.
	// Merely skipping the processor from its initial metadata snapshot cannot
	// satisfy this requirement when Pause committed after loading that snapshot.
	loaded := app.scopedApp(app.DB, organization.ID)
	loaded.inboundContinuation = &inboundContinuationExecution{
		OrganizationID: organization.ID, ContactID: work.Contact.ID,
		MessageID: work.Persisted.ID, WAMID: oldInbound.ID,
	}
	err = loaded.sendAndSaveTextMessage(account, &work.Contact, "Forbidden stale execution")
	require.True(t, inboundContinuationStoppedByPolicy(err), "fresh durable suppression must be checked after Resume")
	assert.Zero(t, providerCalls.Load())

	// Exercise the production RLS outer transaction as well as its durable job
	// checkpoint. Duplicate wakeups consume the old work without any action.
	app.Config = &config.Config{Database: config.DatabaseConfig{RLSEnabled: true}}
	processor := NewInboundContinuationProcessor(app, time.Second)
	processor.process = func(_ context.Context, execution *App, item *persistedIncomingMessage) error {
		return execution.sendAndSaveTextMessage(account, &item.Contact, "A new eligible reply")
	}
	require.NoError(t, processor.ProcessMessage(context.Background(), organization.ID, work.Persisted.ID))
	require.NoError(t, processor.ProcessMessage(context.Background(), organization.ID, work.Persisted.ID))
	_, duplicate, err = app.persistIncomingMessageBeforeAck(account.PhoneID, oldInbound, "Duplicate delivery")
	require.NoError(t, err)
	require.True(t, duplicate)
	require.NoError(t, processor.ProcessMessage(context.Background(), organization.ID, work.Persisted.ID))
	assert.Zero(t, providerCalls.Load())
	assert.Equal(t, models.ScheduledJobStatusCompleted, loadInboundContinuationJob(t, app, organization.ID, work.Persisted.ID).Status)
	var oldActions int64
	require.NoError(t, app.DB.Model(&models.ScheduledJob{}).
		Where("organization_id = ? AND kind = ? AND aggregate_id = ?", organization.ID, inboundContinuationActionJobKind, work.Persisted.ID).
		Count(&oldActions).Error)
	assert.Zero(t, oldActions, "suppression must precede even durable provider-action admission")

	newInbound := inboundContinuationTextMessage(t, "wamid.post-resume-"+uuid.NewString(), phone, "New post-Resume work")
	newWork, duplicate, err := app.persistIncomingMessageBeforeAck(account.PhoneID, newInbound, "Synthetic pause patient")
	require.NoError(t, err)
	require.False(t, duplicate)
	require.False(t, incomingMessageAutomaticAISuppressed(&newWork.Persisted))
	require.NoError(t, processor.ProcessMessage(context.Background(), organization.ID, newWork.Persisted.ID))
	assert.EqualValues(t, 1, providerCalls.Load(), "Resume permits only a genuinely new inbound identity")
	require.NoError(t, app.DB.First(&persisted, "id = ?", work.Persisted.ID).Error)
	assert.True(t, incomingMessageAutomaticAISuppressed(&persisted), "new work must not clear the old message's suppression")
}

func TestNativePausePolicy_StaleGraphPersistenceCannotResurrectCancelledSession(t *testing.T) {
	for _, outcome := range []string{"active_yield", "error_checkpoint", "completed"} {
		t.Run(outcome, func(t *testing.T) {
			app, _, account, contact, stale := newGraphTestFixtures(t)
			stale.SessionData["prior_checkpoint"] = "preserve"
			require.NoError(t, app.DB.Model(stale).Update("session_data", stale.SessionData).Error)
			nativePausePolicyTransfer(t, app, account, contact)
			var cancelled models.ChatbotSession
			require.NoError(t, app.DB.First(&cancelled, "id = ?", stale.ID).Error)
			require.Equal(t, models.SessionStatusCancelled, cancelled.Status)
			require.NotNil(t, cancelled.CompletedAt)
			cancelledAt := *cancelled.CompletedAt

			stale.CurrentStep = "stale-next-node"
			stale.SessionData["stale_overwrite"] = outcome
			if outcome == "completed" {
				stale.Status = models.SessionStatusCompleted
			}
			err := app.persistChatSession(stale)
			require.Error(t, err)
			require.True(t, inboundContinuationStoppedByPolicy(err))
			require.NoError(t, app.DB.First(&cancelled, "id = ?", stale.ID).Error)
			assert.Equal(t, models.SessionStatusCancelled, cancelled.Status)
			require.NotNil(t, cancelled.CompletedAt)
			assert.Equal(t, cancelledAt, *cancelled.CompletedAt)
			assert.NotEqual(t, "stale-next-node", cancelled.CurrentStep)
			assert.Equal(t, "preserve", cancelled.SessionData["prior_checkpoint"])
			assert.NotContains(t, cancelled.SessionData, "stale_overwrite")
		})
	}
}

func TestNativePausePolicy_CancelledGraphCannotExecuteOrYield(t *testing.T) {
	for _, nodeType := range []string{"message", "buttons"} {
		t.Run(nodeType, func(t *testing.T) {
			app, organization, account, contact, stale := newGraphTestFixtures(t)
			calls := nativePausePolicyProvider(t, app, nil)
			flow := &models.ChatbotFlow{
				BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: organization.ID,
				WhatsAppAccount: account.Name, Name: "cancelled-native-graph", IsEnabled: true,
				Graph: models.JSONB{
					"version": 2, "entry_node": "first",
					"nodes": []any{map[string]any{
						"id": "first", "type": nodeType, "label": "must not execute",
						"config": map[string]any{
							"message": "Forbidden cancelled message", "body": "Forbidden cancelled buttons",
							"buttons": []any{map[string]any{"id": "one", "title": "One"}},
						},
					}},
					"edges": []any{},
				},
			}
			require.NoError(t, app.DB.Create(flow).Error)
			nativePausePolicyTransfer(t, app, account, contact)
			err := app.runChatGraph(account, contact, stale, flow, "trigger", "", nil)
			require.Error(t, err)
			assert.True(t, inboundContinuationStoppedByPolicy(err))
			assert.Zero(t, calls.Load())
			var stored models.ChatbotSession
			require.NoError(t, app.DB.First(&stored, "id = ?", stale.ID).Error)
			assert.Equal(t, models.SessionStatusCancelled, stored.Status)
			assert.NotContains(t, stored.SessionData, "__path__", "a cancelled runner must not append a successful or yielded node")
		})
	}
}

func TestNativePausePolicy_SessionPersistenceRequiresExactAcquisition(t *testing.T) {
	for _, changed := range []string{"new_owner", "contact", "account", "organization"} {
		t.Run(changed, func(t *testing.T) {
			app := newProcessorTestApp(t)
			organization, account := createProcessorTestOrg(t, app)
			contact := testutil.CreateTestContactWith(t, app.DB, organization.ID, testutil.WithContactAccount(account.Name))
			stale, created, err := app.getOrCreateSession(organization.ID, contact.ID, account.Name, contact.PhoneNumber, 30)
			require.NoError(t, err)
			require.True(t, created)
			originalID := stale.ID
			expectedGeneration := stale.LastActivityAt
			switch changed {
			case "new_owner":
				owner, isNew, acquireErr := app.getOrCreateSession(organization.ID, contact.ID, account.Name, contact.PhoneNumber, 30)
				require.NoError(t, acquireErr)
				require.False(t, isNew)
				require.Equal(t, stale.ID, owner.ID)
				require.True(t, owner.LastActivityAt.After(stale.LastActivityAt))
				expectedGeneration = owner.LastActivityAt
			case "contact":
				stale.ContactID = uuid.New()
			case "account":
				stale.WhatsAppAccount = "wrong-synthetic-account"
			case "organization":
				stale.OrganizationID = uuid.New()
			}
			stale.SessionData["stale_owner"] = true
			err = app.persistChatSession(stale)
			require.Error(t, err)
			assert.True(t, inboundContinuationStoppedByPolicy(err))
			var stored models.ChatbotSession
			require.NoError(t, app.DB.First(&stored, "id = ?", originalID).Error)
			assert.Equal(t, models.SessionStatusActive, stored.Status)
			assert.True(t, expectedGeneration.Equal(stored.LastActivityAt))
			assert.NotContains(t, stored.SessionData, "stale_owner")
			assert.Equal(t, organization.ID, stored.OrganizationID)
			assert.Equal(t, contact.ID, stored.ContactID)
			assert.Equal(t, account.Name, stored.WhatsAppAccount)
		})
	}
}

func TestNativePausePolicy_SessionAcquisitionRechecksCommittedPause(t *testing.T) {
	app := newProcessorTestApp(t)
	organization, account := createProcessorTestOrg(t, app)
	contact := testutil.CreateTestContactWith(t, app.DB, organization.ID, testutil.WithContactAccount(account.Name))
	policy, err := database.EvaluateContactAutomaticReplyPolicy(app.DB, organization.ID, contact.ID)
	require.NoError(t, err)
	require.True(t, policy.Allowed, "initial chatbot policy read admits the message")
	transfer := nativePausePolicyTransfer(t, app, account, contact)
	session, isNew, err := app.getOrCreateSession(organization.ID, contact.ID, account.Name, contact.PhoneNumber, 30)
	require.Error(t, err)
	assert.True(t, inboundContinuationStoppedByPolicy(err))
	assert.Nil(t, session)
	assert.False(t, isNew)
	var active int64
	require.NoError(t, app.DB.Model(&models.ChatbotSession{}).
		Where("organization_id = ? AND contact_id = ? AND status = ?", organization.ID, contact.ID, models.SessionStatusActive).
		Count(&active).Error)
	assert.Zero(t, active)
	nativePausePolicyResume(t, app, transfer)
	session, isNew, err = app.getOrCreateSession(organization.ID, contact.ID, account.Name, contact.PhoneNumber, 30)
	require.NoError(t, err)
	require.NotNil(t, session)
	assert.True(t, isNew)
	assert.Equal(t, models.SessionStatusActive, session.Status)
}

func TestNativePausePolicy_SessionAcquisitionWaitsForUncommittedPause(t *testing.T) {
	app := newProcessorTestApp(t)
	organization, account := createProcessorTestOrg(t, app)
	contact := testutil.CreateTestContactWith(t, app.DB, organization.ID, testutil.WithContactAccount(account.Name))
	pauseTx := app.DB.Begin()
	require.NoError(t, pauseTx.Error)
	t.Cleanup(func() { _ = pauseTx.Rollback().Error })
	writer := app.scopedApp(pauseTx, organization.ID)
	nativePausePolicyTransfer(t, writer, account, contact)

	type result struct {
		session *models.ChatbotSession
		isNew   bool
		err     error
	}
	backendPID := make(chan int, 1)
	done := make(chan result, 1)
	go func() {
		var acquired result
		acquired.err = app.DB.Connection(func(connection *gorm.DB) error {
			scoped := app.scopedApp(connection.Session(&gorm.Session{NewDB: true}), organization.ID)
			var pid int
			if err := scoped.DB.Raw("SELECT pg_backend_pid()").Scan(&pid).Error; err != nil {
				backendPID <- 0
				return err
			}
			backendPID <- pid
			var err error
			acquired.session, acquired.isNew, err = scoped.getOrCreateSession(organization.ID, contact.ID, account.Name, contact.PhoneNumber, 30)
			return err
		})
		done <- acquired
	}()
	pid := <-backendPID
	require.Positive(t, pid)
	testutil.RequirePostgresBackendWaitingForLock(t, app.DB, pid)
	select {
	case acquired := <-done:
		t.Fatalf("session acquisition crossed the uncommitted policy boundary: %+v", acquired)
	default:
	}
	require.NoError(t, pauseTx.Commit().Error)
	select {
	case acquired := <-done:
		require.Error(t, acquired.err)
		assert.True(t, inboundContinuationStoppedByPolicy(acquired.err))
		assert.Nil(t, acquired.session)
		assert.False(t, acquired.isNew)
	case <-time.After(5 * time.Second):
		t.Fatal("session acquisition failed to observe the committed Pause")
	}
}

func nativePausePolicyTimerFixture(t *testing.T) (*App, *models.WhatsAppAccount, models.Contact, models.ChatbotSettings) {
	t.Helper()
	app := newProcessorTestApp(t)
	organization, account := createProcessorTestOrg(t, app)
	contact := testutil.CreateTestContactWith(t, app.DB, organization.ID, testutil.WithContactAccount(account.Name))
	inactiveSince := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Microsecond)
	lastInbound := inactiveSince.Add(-time.Minute)
	require.NoError(t, app.DB.Model(contact).Updates(map[string]any{
		"chatbot_last_message_at": inactiveSince,
		"chatbot_reminder_sent":   false,
		"last_inbound_at":         lastInbound,
	}).Error)
	require.NoError(t, app.DB.First(contact, "id = ?", contact.ID).Error)
	settings := models.ChatbotSettings{
		OrganizationID: organization.ID, WhatsAppAccount: account.Name,
		ClientInactivity: models.ClientInactivityConfig{
			ReminderEnabled: true, ReminderMinutes: 30,
			ReminderMessage:  "Synthetic chatbot reminder",
			AutoCloseMinutes: 60, AutoCloseMessage: "Synthetic chatbot auto-close",
		},
	}
	return app, account, *contact, settings
}

func nativePausePolicyIdentityHold(t *testing.T, app *App, account *models.WhatsAppAccount, contact *models.Contact) {
	t.Helper()
	state := models.WhatsAppCoexistenceState{
		ID: uuid.New(), OrganizationID: account.OrganizationID, WhatsAppAccountID: account.ID,
		OnboardingStatus: models.CoexistenceOnboardingStatusConnected, OnboardingCycle: 1,
		SyncStatus: models.CoexistenceSyncStatusNotRequested, ContactSyncStatus: models.CoexistenceSyncStatusNotRequested,
		HistoryConsent: models.CoexistenceHistoryConsentUnknown, HistorySyncStatus: models.CoexistenceSyncStatusNotRequested,
		LifecycleStatus: models.CoexistenceLifecycleStatusConnected, LifecycleMetadata: models.JSONB{}, Version: 1,
	}
	require.NoError(t, app.DB.Create(&state).Error)
	principal := "pause-timer-" + uuid.NewString()
	require.NoError(t, app.DB.Model(contact).UpdateColumn("bs_uid", principal).Error)
	claim := WhatsAppIdentityReviewClaim{
		OrganizationID: account.OrganizationID, WhatsAppAccountID: account.ID, OnboardingCycle: 1,
		DirectPrimaryBSUID: principal, VerifiedEventProvenance: WhatsAppIdentityReviewVerifiedMetaEvent,
		VerifiedEventDigest: strings.Repeat("7", 64), SelectorBodyDigest: strings.Repeat("8", 64),
	}
	require.NoError(t, app.DB.Transaction(func(tx *gorm.DB) error {
		hold, created, err := app.CreateOrReuseWhatsAppIdentityReviewHold(tx, &claim)
		if err != nil {
			return err
		}
		require.True(t, created)
		require.True(t, identityReviewContainsContact(hold.Candidates, contact.ID))
		return nil
	}))
	policy, err := database.EvaluateContactAutomaticReplyPolicy(app.DB, account.OrganizationID, contact.ID)
	require.NoError(t, err)
	require.False(t, policy.Allowed)
	require.Equal(t, database.AutomaticReplyBlockedIdentityHold, policy.Reason)
}

func TestNativePausePolicy_ChatbotTimersRecheckSelectedPolicyAndGeneration(t *testing.T) {
	for _, kind := range []string{"reminder", "auto_close"} {
		for _, changed := range []string{"pause", "identity_hold", "pause_resume", "new_generation", "reminder_state"} {
			t.Run(kind+"/"+changed, func(t *testing.T) {
				app, account, selected, settings := nativePausePolicyTimerFixture(t)
				calls := nativePausePolicyProvider(t, app, nil)
				current := selected
				switch changed {
				case "pause":
					nativePausePolicyTransfer(t, app, account, &current)
				case "identity_hold":
					nativePausePolicyIdentityHold(t, app, account, &current)
				case "pause_resume":
					transfer := nativePausePolicyTransfer(t, app, account, &current)
					nativePausePolicyResume(t, app, transfer)
				case "new_generation":
					require.NoError(t, app.DB.Model(&models.Contact{}).Where("id = ?", selected.ID).
						Updates(map[string]any{"chatbot_last_message_at": selected.ChatbotLastMessageAt.Add(time.Minute), "chatbot_reminder_sent": false}).Error)
				case "reminder_state":
					require.NoError(t, app.DB.Model(&models.Contact{}).Where("id = ?", selected.ID).Update("chatbot_reminder_sent", true).Error)
				}
				var expected models.Contact
				require.NoError(t, app.DB.First(&expected, "id = ?", selected.ID).Error)
				processor := NewSLAProcessor(app, time.Second)
				if kind == "reminder" {
					processor.sendChatbotReminder(selected, settings)
				} else {
					processor.autoCloseChatbotSession(selected, settings)
				}
				assert.Zero(t, calls.Load(), "a timer selected before policy or generation change must never deliver")
				var stored models.Contact
				require.NoError(t, app.DB.First(&stored, "id = ?", selected.ID).Error)
				assert.Equal(t, expected.ChatbotLastMessageAt, stored.ChatbotLastMessageAt)
				assert.Equal(t, expected.ChatbotReminderSent, stored.ChatbotReminderSent)
				var outgoing int64
				require.NoError(t, app.DB.Model(&models.Message{}).
					Where("organization_id = ? AND contact_id = ? AND direction = ?", selected.OrganizationID, selected.ID, models.DirectionOutgoing).
					Count(&outgoing).Error)
				assert.Zero(t, outgoing, "a rejected timer must not create a synthetic outgoing message")
			})
		}
	}
}

func TestNativePausePolicy_ChatbotTimerCompletionCannotClearNewerTracking(t *testing.T) {
	for _, kind := range []string{"reminder", "auto_close"} {
		t.Run(kind, func(t *testing.T) {
			app, _, selected, _ := nativePausePolicyTimerFixture(t)
			newer := selected.ChatbotLastMessageAt.Add(time.Minute)
			processor := NewSLAProcessor(app, time.Second)
			var callbackCount int
			completed, err := processor.withChatbotInactivityAttempt(selected, func(_ context.Context, scoped *App, current *models.Contact) error {
				callbackCount++
				// The ordinary sender completes independently of the guard. Model
				// a newer chatbot response committing before the old timer's final
				// bookkeeping, without sleeping or manufacturing a provider retry.
				if err := app.DB.Model(&models.Contact{}).Where("id = ?", current.ID).
					Updates(map[string]any{"chatbot_last_message_at": newer, "chatbot_reminder_sent": false}).Error; err != nil {
					return err
				}
				updates := map[string]any{"chatbot_reminder_sent": true}
				if kind == "auto_close" {
					updates = map[string]any{"chatbot_last_message_at": nil, "chatbot_reminder_sent": false}
				}
				return updateChatbotInactivityGeneration(scoped.DB, current, updates)
			})
			require.NoError(t, err)
			assert.False(t, completed, "stale completion must not retire the newer generation")
			assert.Equal(t, 1, callbackCount)
			var stored models.Contact
			require.NoError(t, app.DB.First(&stored, "id = ?", selected.ID).Error)
			require.NotNil(t, stored.ChatbotLastMessageAt)
			assert.True(t, newer.Equal(*stored.ChatbotLastMessageAt))
			assert.False(t, stored.ChatbotReminderSent)
		})
	}
}

func TestNativePausePolicy_StartedChatbotTimerMakesPauseWait(t *testing.T) {
	for _, kind := range []string{"reminder", "auto_close"} {
		t.Run(kind, func(t *testing.T) {
			app, account, selected, settings := nativePausePolicyTimerFixture(t)
			app.Config = &config.Config{Database: config.DatabaseConfig{RLSEnabled: true}}
			entered := make(chan struct{})
			release := make(chan struct{})
			var enteredOnce, releaseOnce sync.Once
			calls := nativePausePolicyProvider(t, app, func() {
				enteredOnce.Do(func() { close(entered) })
				<-release
			})
			t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
			timerDone := make(chan struct{})
			go func() {
				defer close(timerDone)
				processor := NewSLAProcessor(app, time.Second)
				if kind == "reminder" {
					processor.sendChatbotReminder(selected, settings)
				} else {
					processor.autoCloseChatbotSession(selected, settings)
				}
			}()
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("eligible timer did not reach the provider; this also detects an inherited-transaction self-deadlock")
			}

			backendPID := make(chan int, 1)
			writerDone := make(chan error, 1)
			go func() {
				writerDone <- app.DB.Connection(func(connection *gorm.DB) error {
					writer := app.scopedApp(connection.Session(&gorm.Session{NewDB: true}), selected.OrganizationID)
					var pid int
					if err := writer.DB.Raw("SELECT pg_backend_pid()").Scan(&pid).Error; err != nil {
						backendPID <- 0
						return err
					}
					backendPID <- pid
					contact := selected
					transfer := &models.AgentTransfer{
						BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: selected.OrganizationID,
						ContactID: selected.ID, WhatsAppAccount: account.Name, PhoneNumber: selected.PhoneNumber,
						Status: models.TransferStatusActive, Source: models.TransferSourceManual, TransferredAt: time.Now().UTC(),
					}
					return writer.saveAndFinalizeTransfer(transfer, account, &contact, nil, true)
				})
			}()
			pid := <-backendPID
			require.Positive(t, pid)
			testutil.RequirePostgresBackendWaitingForLock(t, app.DB, pid)
			select {
			case err := <-writerDone:
				t.Fatalf("Pause crossed the already-started chatbot timer: %v", err)
			default:
			}
			var active int64
			require.NoError(t, app.DB.Model(&models.AgentTransfer{}).
				Where("organization_id = ? AND contact_id = ? AND status = ?", selected.OrganizationID, selected.ID, models.TransferStatusActive).
				Count(&active).Error)
			assert.Zero(t, active, "Pause cannot be acknowledged while its provider attempt is in flight")
			releaseOnce.Do(func() { close(release) })
			select {
			case <-timerDone:
			case <-time.After(5 * time.Second):
				t.Fatal("chatbot timer did not finish after provider release")
			}
			select {
			case err := <-writerDone:
				require.NoError(t, err)
			case <-time.After(5 * time.Second):
				t.Fatal("Pause did not finish after the timer's physical fence released")
			}
			assert.EqualValues(t, 1, calls.Load())
			var stored models.Contact
			require.NoError(t, app.DB.First(&stored, "id = ?", selected.ID).Error)
			assert.Nil(t, stored.ChatbotLastMessageAt, "Pause invalidates tracking only after the admitted timer settles")
			assert.False(t, stored.ChatbotReminderSent)
		})
	}
}

func TestNativePausePolicy_HumanTransferSLASendRemainsExempt(t *testing.T) {
	app, account, selected, _ := nativePausePolicyTimerFixture(t)
	calls := nativePausePolicyProvider(t, app, nil)
	transfer := nativePausePolicyTransfer(t, app, account, &selected)
	NewSLAProcessor(app, time.Second).sendSLATextToCustomer(*transfer, "Synthetic human SLA", "Your human team is still reviewing this case")
	assert.EqualValues(t, 1, calls.Load(), "the chatbot-only guard must not suppress human-transfer SLA notices")
	var outgoing int64
	require.NoError(t, app.DB.Model(&models.Message{}).
		Where("organization_id = ? AND contact_id = ? AND direction = ? AND status = ?", selected.OrganizationID, selected.ID, models.DirectionOutgoing, models.MessageStatusSent).
		Count(&outgoing).Error)
	assert.EqualValues(t, 1, outgoing)
	assert.False(t, SLASendOptions().AutomaticAI)
}

func TestNativePausePolicy_AssignmentWaitsForPolicyFenceBeforeContactLock(t *testing.T) {
	app := newProcessorTestApp(t)
	organization, account := createProcessorTestOrg(t, app)
	adminRole := testutil.CreateAdminRole(t, app.DB, organization.ID)
	manager := testutil.CreateTestUser(t, app.DB, organization.ID, testutil.WithRoleID(&adminRole.ID))
	agentRole := testutil.CreateAgentRole(t, app.DB, organization.ID)
	agent := testutil.CreateTestUser(t, app.DB, organization.ID, testutil.WithRoleID(&agentRole.ID))
	contact := testutil.CreateTestContactWith(t, app.DB, organization.ID, testutil.WithContactAccount(account.Name))
	transfer := nativePausePolicyTransfer(t, app, account, contact)
	req := testutil.NewJSONRequest(t, map[string]any{"agent_id": agent.ID.String()})
	testutil.SetAuthContext(req, organization.ID, manager.ID)
	testutil.SetPathParam(req, "id", transfer.ID.String())

	// Model a policy writer before it reaches Contact/Transfer. The assignment
	// must join that organization-first order, not hold Contact while waiting
	// for the writer's Transfer row and form an assign/resume deadlock cycle.
	policyTx := app.DB.Begin()
	require.NoError(t, policyTx.Error)
	t.Cleanup(func() { _ = policyTx.Rollback().Error })
	require.NoError(t, database.LockOrganizationPolicyScope(policyTx, organization.ID))
	var blockerPID int
	require.NoError(t, policyTx.Raw("SELECT pg_backend_pid()").Scan(&blockerPID).Error)
	require.Positive(t, blockerPID)

	type assignmentResult struct {
		status int
		err    error
	}
	backendPID := make(chan int, 1)
	done := make(chan assignmentResult, 1)
	go func() {
		result := assignmentResult{}
		result.err = app.DB.Connection(func(connection *gorm.DB) error {
			scoped := &App{
				DB: connection.Session(&gorm.Session{NewDB: true}), Log: app.Log,
				Config: app.Config, Redis: app.Redis, root: app,
			}
			var pid int
			if err := scoped.DB.Raw("SELECT pg_backend_pid()").Scan(&pid).Error; err != nil {
				backendPID <- 0
				return err
			}
			backendPID <- pid
			err := scoped.AssignAgentTransfer(req)
			result.status = testutil.GetResponseStatusCode(req)
			return err
		})
		done <- result
	}()
	pid := <-backendPID
	require.Positive(t, pid)
	testutil.RequirePostgresBackendWaitingForLock(t, app.DB, pid)
	var blockingMatches int64
	require.NoError(t, app.DB.Raw(`
		SELECT COUNT(*)
		  FROM pg_catalog.unnest(pg_catalog.pg_blocking_pids(?)) AS blocker(pid)
		 WHERE blocker.pid = ?
	`, pid, blockerPID).Scan(&blockingMatches).Error)
	require.EqualValues(t, 1, blockingMatches, "the assignment must wait on this exact policy writer")
	select {
	case result := <-done:
		t.Fatalf("assignment bypassed the organization policy fence: %+v", result)
	default:
	}

	// A third independent connection proves that the waiting assignment has
	// acquired no Contact row lock. NOWAIT turns an inversion into an immediate
	// assertion failure instead of depending on PostgreSQL deadlock detection.
	require.NoError(t, app.DB.Transaction(func(probe *gorm.DB) error {
		var locked struct{ ID uuid.UUID }
		if err := probe.Raw(`
			SELECT id FROM contacts
			 WHERE id = ? AND organization_id = ?
			 FOR UPDATE NOWAIT
		`, contact.ID, organization.ID).Scan(&locked).Error; err != nil {
			return err
		}
		require.Equal(t, contact.ID, locked.ID)
		return nil
	}), "assignment must not lock Contact before its policy fence")

	var before models.AgentTransfer
	require.NoError(t, app.DB.First(&before, "id = ?", transfer.ID).Error)
	require.Equal(t, models.TransferStatusActive, before.Status)
	require.Nil(t, before.AgentID)
	require.NoError(t, policyTx.Commit().Error)
	select {
	case result := <-done:
		require.NoError(t, result.err)
		require.Equal(t, fasthttp.StatusOK, result.status)
	case <-time.After(5 * time.Second):
		t.Fatal("assignment did not complete after the policy fence released")
	}
	app.wg.Wait() // Drain the fixture's empty webhook dispatch before teardown.
	var stored models.AgentTransfer
	require.NoError(t, app.DB.First(&stored, "id = ?", transfer.ID).Error)
	assert.Equal(t, models.TransferStatusActive, stored.Status)
	require.NotNil(t, stored.AgentID)
	assert.Equal(t, agent.ID, *stored.AgentID)
	assert.Nil(t, stored.ResumedAt)
	assert.Nil(t, stored.ResumedBy)

	// Ordinary queue/availability writes may already hold the platform guard's
	// organization SHARE lock. Assignment changes no active-policy state and
	// must not request UPDATE (or an advisory writer mutex) against that reader.
	shareTx := app.DB.Begin()
	require.NoError(t, shareTx.Error)
	t.Cleanup(func() { _ = shareTx.Rollback().Error })
	var shared struct{ ID uuid.UUID }
	require.NoError(t, shareTx.Raw(`
		SELECT id FROM organizations WHERE id = ? FOR SHARE
	`, organization.ID).Scan(&shared).Error)
	require.Equal(t, organization.ID, shared.ID)
	sharedReq := testutil.NewJSONRequest(t, map[string]any{"agent_id": agent.ID.String()})
	testutil.SetAuthContext(sharedReq, organization.ID, manager.ID)
	testutil.SetPathParam(sharedReq, "id", transfer.ID.String())
	sharedDone := make(chan assignmentResult, 1)
	go func() {
		err := app.AssignAgentTransfer(sharedReq)
		sharedDone <- assignmentResult{status: testutil.GetResponseStatusCode(sharedReq), err: err}
	}()
	select {
	case result := <-sharedDone:
		require.NoError(t, result.err)
		require.Equal(t, fasthttp.StatusOK, result.status)
	case <-time.After(5 * time.Second):
		t.Fatal("assignment conflicted with the ordinary tenant write guard")
	}
	require.NoError(t, shareTx.Commit().Error)
	app.wg.Wait()
}
