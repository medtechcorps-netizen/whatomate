package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/config"
	"github.com/shridarpatil/whatomate/internal/database"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func phoneOnlyMessage(t *testing.T, phone string) IncomingTextMessage {
	return coexistenceAdmissionMessage(t, "wamid.phone-only-"+uuid.NewString(), phone, "", "Synthetic phone-only message")
}

func TestCoexistencePhoneOnlyFreshContinuationAndReplay(t *testing.T) {
	app, account, _ := whatsappIdentityFixture(t)
	phone := coexistenceAdmissionTestPhone()
	first := phoneOnlyMessage(t, phone)
	work, duplicate := admitCoexistenceAdmissionMessage(t, app, account, first, "Fresh sender")
	require.NotNil(t, work)
	require.False(t, duplicate)
	contact := loadCoexistenceAdmissionContact(t, app.DB, account.OrganizationID, work.Contact.ID)
	assert.Equal(t, phone, contact.PhoneNumber)
	assert.Empty(t, contact.BSUID)
	assert.NotNil(t, work.Persisted.InboxConversationID)
	assert.NotContains(t, work.Persisted.Metadata, incomingAutomaticAISuppressedKey)
	proof, valid := decodeCoexistencePhoneAdmissionProof(work.Persisted.Metadata[coexistencePhoneAdmissionKey])
	require.True(t, valid)
	assert.True(t, proof.Initial)
	assert.Equal(t, account.ID, proof.AccountID)
	assert.EqualValues(t, 1, proof.Cycle)
	job := loadInboundContinuationJob(t, app, account.OrganizationID, work.Persisted.ID)
	assert.Equal(t, work.Persisted.Metadata[coexistencePhoneAdmissionKey], job.Payload[coexistencePhoneAdmissionKey])

	second, duplicate := admitCoexistenceAdmissionMessage(t, app, account, phoneOnlyMessage(t, phone), "Same phone")
	require.NotNil(t, second)
	assert.False(t, duplicate)
	assert.Equal(t, work.Contact.ID, second.Contact.ID)
	assert.Equal(t, work.Persisted.InboxConversationID, second.Persisted.InboxConversationID)
	secondProof, valid := decodeCoexistencePhoneAdmissionProof(second.Persisted.Metadata[coexistencePhoneAdmissionKey])
	require.True(t, valid)
	assert.False(t, secondProof.Initial)
	replay, duplicate := admitCoexistenceAdmissionMessage(t, app, account, first, "Replay")
	assert.Nil(t, replay)
	assert.True(t, duplicate)
	assert.EqualValues(t, 2, countCoexistenceAdmissionRows(t, app.DB, &models.Message{}, "organization_id = ? AND contact_id = ? AND direction = ?", account.OrganizationID, work.Contact.ID, models.DirectionIncoming))
	assert.EqualValues(t, 1, countCoexistenceAdmissionRows(t, app.DB, &models.Contact{}, "organization_id = ? AND phone_number = ?", account.OrganizationID, phone))
	assert.Zero(t, countCoexistenceAdmissionRows(t, app.DB, &models.WhatsAppIdentityReviewHold{}, "organization_id = ?", account.OrganizationID))

	// Normal continuation retains the existing processor and never needs a
	// synthetic BSUID or a reviewed-route bypass.
	require.NoError(t, app.DB.Create(&models.ChatbotSettings{BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: account.OrganizationID, WhatsAppAccount: account.Name, IsEnabled: false, AI: models.AIConfig{Enabled: false}}).Error)
	processor := NewInboundContinuationProcessor(app, time.Hour)
	require.NoError(t, processor.ProcessMessage(context.Background(), account.OrganizationID, work.Persisted.ID))
	assert.Equal(t, models.ScheduledJobStatusCompleted, loadInboundContinuationJob(t, app, account.OrganizationID, work.Persisted.ID).Status)

	// A staff pause still admits the next message visibly with sticky AI
	// suppression; the new path does not resume automation.
	require.NoError(t, app.DB.Model(&models.InboxConversation{}).Where("id = ?", *work.Persisted.InboxConversationID).
		Update("config", models.JSONB{models.ConversationConfigAIPaused: true}).Error)
	paused, _ := admitCoexistenceAdmissionMessage(t, app, account, phoneOnlyMessage(t, phone), "Paused")
	require.NotNil(t, paused)
	assert.True(t, incomingMessageAutomaticAISuppressed(&paused.Persisted))
}

func TestCoexistencePhoneOnlyRefusesOldContactsAndConflicts(t *testing.T) {
	cases := []struct {
		name    string
		prepare func(*testing.T, *App, *models.WhatsAppAccount, *models.Contact)
	}{
		{"address_book", func(t *testing.T, app *App, _ *models.WhatsAppAccount, c *models.Contact) {}},
		{"other_bsuid", func(t *testing.T, app *App, _ *models.WhatsAppAccount, c *models.Contact) {
			require.NoError(t, app.DB.Model(c).Update("bs_uid", "US.other-person").Error)
		}},
		{"deleted", func(t *testing.T, app *App, _ *models.WhatsAppAccount, c *models.Contact) {
			require.NoError(t, app.DB.Delete(c).Error)
		}},
		{"merged_alias", func(t *testing.T, app *App, a *models.WhatsAppAccount, c *models.Contact) {
			target := testutil.CreateTestContact(t, app.DB, a.OrganizationID)
			require.NoError(t, app.DB.Model(c).Update("merged_into_id", target.ID).Error)
		}},
		{"forged_contact_metadata", func(t *testing.T, app *App, a *models.WhatsAppAccount, c *models.Contact) {
			require.NoError(t, app.DB.Model(c).Update("metadata", models.JSONB{coexistencePhoneAdmissionKey: `{"account_id":"` + a.ID.String() + `","cycle":1,"initial":true,"webhook_sha256":"` + strings.Repeat("a", 64) + `"}`}).Error)
		}},
		{"phone_format_alias", func(t *testing.T, app *App, _ *models.WhatsAppAccount, c *models.Contact) {
			require.NoError(t, app.DB.Model(c).Update("phone_number", "+"+c.PhoneNumber).Error)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			app, account, _ := whatsappIdentityFixture(t)
			phone := coexistenceAdmissionTestPhone()
			contact := newWhatsAppIdentityReviewSelectorContact(account.OrganizationID, phone)
			require.NoError(t, app.DB.Create(&contact).Error)
			tc.prepare(t, app, account, &contact)
			before := countCoexistenceAdmissionRows(t, app.DB, &models.Contact{}, "organization_id = ?", account.OrganizationID)
			message := phoneOnlyMessage(t, phone)
			work, duplicate := admitCoexistenceAdmissionMessage(t, app, account, message, "Not adopted")
			assert.Nil(t, work)
			assert.False(t, duplicate)
			assert.EqualValues(t, 1, countCoexistenceStagedReceipts(t, app, account.OrganizationID, message.ID))
			assert.Equal(t, before, countCoexistenceAdmissionRows(t, app.DB, &models.Contact{}, "organization_id = ?", account.OrganizationID))
		})
	}
}

func TestCoexistencePhoneOnlyRejectsIncompleteAndConflictingIdentity(t *testing.T) {
	cases := []struct {
		name   string
		change func(*IncomingTextMessage)
	}{
		{"no_phone", func(m *IncomingTextMessage) { m.From = "" }},
		{"invalid_phone", func(m *IncomingTextMessage) { m.From = "US.123456789" }},
		{"short_phone", func(m *IncomingTextMessage) { m.From = "123" }},
		{"long_phone", func(m *IncomingTextMessage) { m.From = "1234567890123456" }},
		{"leading_zero", func(m *IncomingTextMessage) { m.From = "0123456789" }},
		{"formatted_phone", func(m *IncomingTextMessage) { m.From = "+60123456789" }},
		{"parent_only", func(m *IncomingTextMessage) { m.FromParentUserID = "US.parent" }},
		{"username", func(m *IncomingTextMessage) { m.senderUsername = "someone" }},
		{"wa_id_disagrees", func(m *IncomingTextMessage) {
			*m = m.withWebhookSenderContact(&CoexistenceWebhookContact{WaID: "15550001111"})
		}},
		{"sidecar_user_id", func(m *IncomingTextMessage) {
			*m = m.withWebhookSenderContact(&CoexistenceWebhookContact{WaID: m.From, UserID: "US.sidecar"})
		}},
		{"unmatched_sidecar", func(m *IncomingTextMessage) {
			*m = m.withPhoneOnlyContactEvidence([]CoexistenceWebhookContact{{WaID: "15550001111"}})
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			app, account, _ := whatsappIdentityFixture(t)
			before := countCoexistenceAdmissionRows(t, app.DB, &models.Contact{}, "organization_id = ?", account.OrganizationID)
			message := phoneOnlyMessage(t, coexistenceAdmissionTestPhone())
			tc.change(&message)
			work, _ := admitCoexistenceAdmissionMessage(t, app, account, message, "Invalid")
			assert.Nil(t, work)
			assert.Equal(t, before, countCoexistenceAdmissionRows(t, app.DB, &models.Contact{}, "organization_id = ?", account.OrganizationID))
		})
	}
}

func TestCoexistencePhoneOnlyContinuationRequiresOriginalProof(t *testing.T) {
	cases := []struct {
		name   string
		change func(*testing.T, *App, *models.WhatsAppAccount, *persistedIncomingMessage)
	}{
		{"cycle_changed", func(t *testing.T, a *App, account *models.WhatsAppAccount, w *persistedIncomingMessage) {
			require.NoError(t, a.DB.Model(&models.WhatsAppCoexistenceState{}).Where("organization_id = ? AND whats_app_account_id = ?", account.OrganizationID, account.ID).Update("onboarding_cycle", 2).Error)
		}},
		{"job_missing", func(t *testing.T, a *App, account *models.WhatsAppAccount, w *persistedIncomingMessage) {
			require.NoError(t, a.DB.Where("organization_id = ? AND aggregate_id = ? AND kind = ?", account.OrganizationID, w.Persisted.ID, inboundContinuationJobKind).Delete(&models.ScheduledJob{}).Error)
		}},
		{"job_raw_sender_changed", func(t *testing.T, a *App, account *models.WhatsAppAccount, w *persistedIncomingMessage) {
			job := loadInboundContinuationJob(t, a, account.OrganizationID, w.Persisted.ID)
			raw := job.Payload["message"].(map[string]any)
			raw["from"] = "15550000000"
			require.NoError(t, a.DB.Model(&job).Update("payload", job.Payload).Error)
		}},
		{"job_cycle_changed", func(t *testing.T, a *App, account *models.WhatsAppAccount, w *persistedIncomingMessage) {
			job := loadInboundContinuationJob(t, a, account.OrganizationID, w.Persisted.ID)
			delete(job.Payload, coexistencePhoneAdmissionKey)
			require.NoError(t, a.DB.Model(&job).Update("payload", job.Payload).Error)
		}},
		{"contact_bsuid_learned", func(t *testing.T, a *App, account *models.WhatsAppAccount, w *persistedIncomingMessage) {
			require.NoError(t, a.DB.Model(&models.Contact{}).Where("id = ?", w.Contact.ID).Update("bs_uid", "US.learned").Error)
		}},
		{"identity_metadata", func(t *testing.T, a *App, account *models.WhatsAppAccount, w *persistedIncomingMessage) {
			require.NoError(t, a.DB.Model(&models.Contact{}).Where("id = ?", w.Contact.ID).Update("metadata", models.JSONB{"coexistence_phone_conflict": true}).Error)
		}},
		{"alias_added", func(t *testing.T, a *App, account *models.WhatsAppAccount, w *persistedIncomingMessage) {
			alias := testutil.CreateTestContact(t, a.DB, account.OrganizationID)
			require.NoError(t, a.DB.Model(alias).Update("merged_into_id", w.Contact.ID).Error)
		}},
		{"first_message_deleted", func(t *testing.T, a *App, account *models.WhatsAppAccount, w *persistedIncomingMessage) {
			require.NoError(t, a.DB.Delete(&w.Persisted).Error)
		}},
		{"later_bsuid_even_if_contact_cleared", func(t *testing.T, a *App, account *models.WhatsAppAccount, w *persistedIncomingMessage) {
			later := phoneOnlyMessage(t, w.Contact.PhoneNumber)
			later.FromUserID = "US.later-known"
			admitted, _ := admitCoexistenceAdmissionMessage(t, a, account, later, "Known")
			require.NotNil(t, admitted)
			assert.Equal(t, w.Contact.ID, admitted.Contact.ID)
			stored := loadCoexistenceAdmissionContact(t, a.DB, account.OrganizationID, w.Contact.ID)
			assert.Equal(t, later.FromUserID, stored.BSUID)
			later.ID = "wamid.same-bsuid-" + uuid.NewString()
			again, _ := admitCoexistenceAdmissionMessage(t, a, account, later, "Known again")
			require.NotNil(t, again)
			assert.Equal(t, w.Contact.ID, again.Contact.ID)
			require.NoError(t, a.DB.Model(&models.Contact{}).Where("id = ?", w.Contact.ID).Updates(map[string]any{"bs_uid": "", "metadata": models.JSONB{}}).Error)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			app, account, _ := whatsappIdentityFixture(t)
			phone := coexistenceAdmissionTestPhone()
			first, _ := admitCoexistenceAdmissionMessage(t, app, account, phoneOnlyMessage(t, phone), "Fresh")
			require.NotNil(t, first)
			tc.change(t, app, account, first)
			next := phoneOnlyMessage(t, phone)
			work, _ := admitCoexistenceAdmissionMessage(t, app, account, next, "Refused")
			assert.Nil(t, work)
			assert.EqualValues(t, 1, countCoexistenceStagedReceipts(t, app, account.OrganizationID, next.ID))
		})
	}
}

func TestCoexistencePhoneOnlyKeepsOldHeldWAMIDAndFutureHold(t *testing.T) {
	app, account, _ := whatsappIdentityFixture(t)
	phone := coexistenceAdmissionTestPhone()
	held := phoneOnlyMessage(t, phone)
	holdID := stageCoexistenceReceiptAsBeforeThisChange(t, app, account, held)
	replay, duplicate := admitCoexistenceAdmissionMessage(t, app, account, held, "Replay")
	assert.Nil(t, replay)
	assert.True(t, duplicate)
	next, _ := admitCoexistenceAdmissionMessage(t, app, account, phoneOnlyMessage(t, phone), "Still held")
	assert.Nil(t, next)
	var hold models.WhatsAppIdentityReviewHold
	require.NoError(t, app.DB.First(&hold, holdID).Error)
	assert.False(t, hold.Supported)
	assert.Equal(t, models.WhatsAppIdentityReviewDispositionOpen, hold.Disposition)
	assert.Zero(t, countCoexistenceAdmissionRows(t, app.DB, &models.Message{}, "organization_id = ? AND whats_app_message_id = ?", account.OrganizationID, held.ID))
}

func TestCoexistencePhoneOnlyScopesAccountAndTenant(t *testing.T) {
	app, account, _ := whatsappIdentityFixture(t)
	phone := coexistenceAdmissionTestPhone()
	first, _ := admitCoexistenceAdmissionMessage(t, app, account, phoneOnlyMessage(t, phone), "First")
	require.NotNil(t, first)
	other := *account
	other.ID = uuid.New()
	other.Name = "Other-" + uuid.NewString()
	other.PhoneID = testutil.NewTestGraphObjectID()
	require.NoError(t, app.DB.Create(&other).Error)
	require.NoError(t, app.DB.Create(&models.WhatsAppCoexistenceState{ID: uuid.New(), OrganizationID: other.OrganizationID, WhatsAppAccountID: other.ID, OnboardingCycle: 1, OnboardingStatus: models.CoexistenceOnboardingStatusConnected, LifecycleStatus: models.CoexistenceLifecycleStatusConnected, Version: 1}).Error)
	second, _ := admitCoexistenceAdmissionMessage(t, app, &other, phoneOnlyMessage(t, phone), "Other account")
	assert.Nil(t, second)
	otherApp, otherAccount, _ := whatsappIdentityFixture(t)
	separate, _ := admitCoexistenceAdmissionMessage(t, otherApp, otherAccount, phoneOnlyMessage(t, phone), "Other tenant")
	require.NotNil(t, separate)
	assert.NotEqual(t, first.Contact.ID, separate.Contact.ID)
	assert.Equal(t, otherAccount.OrganizationID, separate.Contact.OrganizationID)
}

func TestCoexistencePhoneOnlyConcurrentFirstMessages(t *testing.T) {
	app, account, _ := whatsappIdentityFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	app.DB = app.DB.WithContext(ctx)
	phone := coexistenceAdmissionTestPhone()
	messages := []IncomingTextMessage{phoneOnlyMessage(t, phone), phoneOnlyMessage(t, phone), phoneOnlyMessage(t, phone)}
	messages = append(messages, messages[0])
	blocker := app.DB.Begin()
	require.NoError(t, blocker.Error)
	defer blocker.Rollback()
	require.NoError(t, database.LockOrganizationPolicyScope(blocker, account.OrganizationID))
	type result struct {
		work      *persistedIncomingMessage
		duplicate bool
		err       error
	}
	results := make(chan result, len(messages))
	var wg sync.WaitGroup
	for _, message := range messages {
		wg.Add(1)
		go func(message IncomingTextMessage) {
			defer wg.Done()
			var out result
			for attempt := 0; attempt < 8; attempt++ {
				out.work, out.duplicate, out.err = app.persistAuthenticatedIncomingMessageBeforeAck(account.PhoneID, message, "Concurrent", strings.Repeat("a", 64))
				if !isRetryableCanonicalContactWrite(out.err) {
					break
				}
			}
			results <- out
		}(message)
	}
	time.Sleep(100 * time.Millisecond)
	require.NoError(t, blocker.Rollback().Error)
	joined := make(chan struct{})
	go func() { wg.Wait(); close(joined) }()
	select {
	case <-joined:
	case <-ctx.Done():
		t.Fatal("concurrent phone-only admission exceeded its lock budget")
	}
	close(results)
	ids := map[uuid.UUID]bool{}
	duplicates := 0
	for r := range results {
		require.NoError(t, r.err)
		if r.duplicate {
			duplicates++
			continue
		}
		require.NotNil(t, r.work)
		ids[r.work.Contact.ID] = true
	}
	assert.Len(t, ids, 1)
	assert.Equal(t, 1, duplicates)
	assert.EqualValues(t, 3, countCoexistenceAdmissionRows(t, app.DB, &models.ScheduledJob{}, "organization_id = ? AND kind = ?", account.OrganizationID, inboundContinuationJobKind))
}

func TestCoexistencePhoneOnlySignedWebhookBoundary(t *testing.T) {
	app, account, _ := whatsappIdentityFixture(t)
	app.Config = &config.Config{WhatsApp: config.WhatsAppConfig{AppSecret: webhookTestAppSecret}}
	require.NoError(t, app.DB.Model(account).Update("app_secret", webhookTestAppSecret).Error)
	app.InvalidateWhatsAppAccountCache(account.PhoneID)
	phone := coexistenceAdmissionTestPhone()
	message := phoneOnlyMessage(t, phone)
	body, err := json.Marshal(map[string]any{"object": "whatsapp_business_account", "entry": []any{map[string]any{"id": account.BusinessID, "changes": []any{map[string]any{"field": "messages", "value": map[string]any{"messaging_product": "whatsapp", "metadata": map[string]any{"phone_number_id": account.PhoneID}, "contacts": []any{map[string]any{"wa_id": phone, "profile": map[string]any{"name": "Fresh"}}}, "messages": []any{message}}}}}}})
	require.NoError(t, err)
	request := testutil.NewRequest(t)
	request.RequestCtx.Request.Header.SetMethod("POST")
	request.RequestCtx.Request.Header.SetContentType("application/json")
	request.RequestCtx.Request.Header.Set("X-Hub-Signature-256", "sha256="+strings.Repeat("0", 64))
	request.RequestCtx.Request.SetBody(body)
	require.NoError(t, app.WebhookHandler(request))
	assert.Equal(t, http.StatusForbidden, request.RequestCtx.Response.StatusCode())
	assert.Zero(t, countCoexistenceAdmissionRows(t, app.DB, &models.Contact{}, "organization_id = ? AND phone_number = ?", account.OrganizationID, phone))
	assert.Equal(t, http.StatusOK, signedWebhookStatusCode(t, app, body))
	assert.EqualValues(t, 1, countCoexistenceAdmissionRows(t, app.DB, &models.Contact{}, "organization_id = ? AND phone_number = ?", account.OrganizationID, phone))
	assert.EqualValues(t, 1, countCoexistenceAdmissionRows(t, app.DB, &models.Message{}, "organization_id = ? AND whats_app_message_id = ?", account.OrganizationID, message.ID), fmt.Sprintf("signed inbound %s", message.ID))
}

func TestCoexistencePhoneOnlySignedSidecarConflictsStayHeld(t *testing.T) {
	for _, kind := range []string{"mismatch", "duplicate", "user_id", "parent_id", "username"} {
		t.Run(kind, func(t *testing.T) {
			app, account, _ := whatsappIdentityFixture(t)
			app.Config = &config.Config{WhatsApp: config.WhatsAppConfig{AppSecret: webhookTestAppSecret}}
			require.NoError(t, app.DB.Model(account).Update("app_secret", webhookTestAppSecret).Error)
			app.InvalidateWhatsAppAccountCache(account.PhoneID)
			phone := coexistenceAdmissionTestPhone()
			message := phoneOnlyMessage(t, phone)
			contact := map[string]any{"wa_id": phone, "profile": map[string]any{"name": "Held"}}
			contacts := []any{contact}
			switch kind {
			case "mismatch":
				contact["wa_id"] = "15550001111"
			case "duplicate":
				contacts = append(contacts, contact)
			case "user_id":
				contact["user_id"] = "US.sidecar"
			case "parent_id":
				contact["parent_user_id"] = "US.parent"
			case "username":
				contact["profile"] = map[string]any{"username": "unknown-handle"}
			}
			body, err := json.Marshal(map[string]any{"object": "whatsapp_business_account", "entry": []any{map[string]any{"id": account.BusinessID, "changes": []any{map[string]any{"field": "messages", "value": map[string]any{"messaging_product": "whatsapp", "metadata": map[string]any{"phone_number_id": account.PhoneID}, "contacts": contacts, "messages": []any{message}}}}}}})
			require.NoError(t, err)
			assert.Equal(t, http.StatusOK, signedWebhookStatusCode(t, app, body))
			assert.Zero(t, countCoexistenceAdmissionRows(t, app.DB, &models.Contact{}, "organization_id = ? AND phone_number = ?", account.OrganizationID, phone))
			assert.EqualValues(t, 1, countCoexistenceStagedReceipts(t, app, account.OrganizationID, message.ID))
			assert.Zero(t, countCoexistenceAdmissionRows(t, app.DB, &models.ScheduledJob{}, "organization_id = ? AND kind = ?", account.OrganizationID, inboundContinuationJobKind))
		})
	}
}

func TestCoexistencePhoneOnlyPersistenceFailureRollsBackFreshContact(t *testing.T) {
	app, account, _ := whatsappIdentityFixture(t)
	phone := coexistenceAdmissionTestPhone()
	message := phoneOnlyMessage(t, phone)
	name := "phone_failure_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	require.NoError(t, app.DB.Exec(fmt.Sprintf(`CREATE FUNCTION %s() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.whats_app_message_id = '%s' THEN RAISE EXCEPTION 'synthetic persistence failure'; END IF; RETURN NEW; END; $$`, name, message.ID)).Error)
	require.NoError(t, app.DB.Exec(fmt.Sprintf(`CREATE TRIGGER %s BEFORE INSERT ON messages FOR EACH ROW EXECUTE FUNCTION %s()`, name, name)).Error)
	t.Cleanup(func() {
		app.DB.Exec(fmt.Sprintf("DROP TRIGGER IF EXISTS %s ON messages", name))
		app.DB.Exec(fmt.Sprintf("DROP FUNCTION IF EXISTS %s()", name))
	})
	work, duplicate, err := app.persistAuthenticatedIncomingMessageBeforeAck(account.PhoneID, message, "Rolled back", strings.Repeat("a", 64))
	require.Error(t, err)
	assert.Nil(t, work)
	assert.False(t, duplicate)
	assert.Zero(t, countCoexistenceAdmissionRows(t, app.DB, &models.Contact{}, "organization_id = ? AND phone_number = ?", account.OrganizationID, phone))
	assert.Zero(t, countCoexistenceAdmissionRows(t, app.DB, &models.ScheduledJob{}, "organization_id = ? AND kind = ?", account.OrganizationID, inboundContinuationJobKind))
	require.NoError(t, app.DB.Exec(fmt.Sprintf("DROP TRIGGER %s ON messages", name)).Error)
	work, duplicate = admitCoexistenceAdmissionMessage(t, app, account, message, "Retry")
	require.NotNil(t, work)
	assert.False(t, duplicate)
}

func TestCoexistencePhoneOnlyHoldMemberWithDifferentPhoneBlocksContinuation(t *testing.T) {
	app, account, _ := whatsappIdentityFixture(t)
	phone := coexistenceAdmissionTestPhone()
	first, _ := admitCoexistenceAdmissionMessage(t, app, account, phoneOnlyMessage(t, phone), "Fresh")
	require.NotNil(t, first)
	bsuid := "US.former-identity-" + uuid.NewString()
	require.NoError(t, app.DB.Model(&models.Contact{}).Where("id = ?", first.Contact.ID).Update("bs_uid", bsuid).Error)
	conflict := phoneOnlyMessage(t, coexistenceAdmissionTestPhone())
	conflict.FromUserID = "US.conflicting-" + uuid.NewString()
	conflict.FromParentUserID = bsuid
	// Create the immutable review evidence directly, as a concurrent review
	// path would; the sender's current phone is deliberately different.
	require.NoError(t, app.WithCommittedTenantApp(account.OrganizationID, func(scoped *App) error {
		claim := WhatsAppIdentityReviewClaim{OrganizationID: account.OrganizationID, WhatsAppAccountID: account.ID, OnboardingCycle: 1, DirectPrimaryBSUID: conflict.FromUserID, ParentBSUID: bsuid, Phone: conflict.From, VerifiedEventProvenance: WhatsAppIdentityReviewVerifiedMetaEvent, VerifiedEventDigest: strings.Repeat("a", 64), SelectorBodyDigest: strings.Repeat("b", 64)}
		_, _, err := scoped.CreateOrReuseWhatsAppIdentityReviewHold(scoped.DB, &claim)
		return err
	}))
	require.NoError(t, app.DB.Model(&models.Contact{}).Where("id = ?", first.Contact.ID).Update("bs_uid", "").Error)
	next, _ := admitCoexistenceAdmissionMessage(t, app, account, phoneOnlyMessage(t, phone), "Still blocked")
	assert.Nil(t, next)
}

func TestCoexistencePhoneOnlyLongHistoryDoesNotHideOldContradiction(t *testing.T) {
	app, account, _ := whatsappIdentityFixture(t)
	phone := coexistenceAdmissionTestPhone()
	first, _ := admitCoexistenceAdmissionMessage(t, app, account, phoneOnlyMessage(t, phone), "Fresh")
	require.NotNil(t, first)
	proof, valid := decodeCoexistencePhoneAdmissionProof(first.Persisted.Metadata[coexistencePhoneAdmissionKey])
	require.True(t, valid)
	proof.Initial = false
	encoded, err := json.Marshal(proof)
	require.NoError(t, err)
	// A synthetic long history verifies that no latest-N evidence window
	// strands a busy sender or conceals an old contradictory admission.
	rows := make([]models.Message, 1000)
	for i := range rows {
		rows[i] = models.Message{BaseModel: models.BaseModel{ID: uuid.New(), CreatedAt: time.Now().Add(-time.Duration(1000-i) * time.Minute)},
			OrganizationID: account.OrganizationID, WhatsAppAccount: account.Name, ContactID: first.Contact.ID,
			WhatsAppMessageID: "wamid.synthetic-history-" + uuid.NewString(), Direction: models.DirectionIncoming,
			MessageType: models.MessageTypeText, Status: models.MessageStatusReceived,
			Metadata: models.JSONB{coexistencePhoneAdmissionKey: string(encoded)}}
	}
	require.NoError(t, app.DB.CreateInBatches(&rows, 100).Error)
	started := time.Now()
	next, _ := admitCoexistenceAdmissionMessage(t, app, account, phoneOnlyMessage(t, phone), "After long history")
	require.NotNil(t, next)
	t.Logf("synthetic 1000-row history admission %s; contact=%s org=%s", time.Since(started), first.Contact.ID, account.OrganizationID)
	require.NoError(t, app.DB.Model(&rows[0]).Update("metadata", models.JSONB{}).Error)
	blocked, _ := admitCoexistenceAdmissionMessage(t, app, account, phoneOnlyMessage(t, phone), "Old contradiction")
	assert.Nil(t, blocked)
}

func TestCoexistencePhoneOnlyQueuedWorkStopsAfterIdentityHold(t *testing.T) {
	app, account, _ := whatsappIdentityFixture(t)
	phone := coexistenceAdmissionTestPhone()
	first, _ := admitCoexistenceAdmissionMessage(t, app, account, phoneOnlyMessage(t, phone), "Fresh")
	require.NotNil(t, first)
	assert.False(t, incomingMessageAutomaticAISuppressed(&first.Persisted))
	conflict := phoneOnlyMessage(t, phone)
	conflict.FromParentUserID = "US.unproven-parent"
	held, _ := admitCoexistenceAdmissionMessage(t, app, account, conflict, "Conflict")
	assert.Nil(t, held)
	providerCalls := 0
	guardEntered := false
	processor := NewInboundContinuationProcessor(app, time.Hour)
	processor.process = func(ctx context.Context, scoped *App, _ *persistedIncomingMessage) error {
		guardEntered = true
		return scoped.withInboundContinuationPhysicalAIAttempt(ctx, func() error { providerCalls++; return nil })
	}
	require.NoError(t, processor.ProcessMessage(context.Background(), account.OrganizationID, first.Persisted.ID))
	assert.True(t, guardEntered)
	assert.Zero(t, providerCalls)
	assert.Equal(t, models.ScheduledJobStatusCompleted, loadInboundContinuationJob(t, app, account.OrganizationID, first.Persisted.ID).Status)
}

func TestCoexistencePhoneOnlyPromotionRefusesAnotherUser(t *testing.T) {
	app, account, _ := whatsappIdentityFixture(t)
	phone := coexistenceAdmissionTestPhone()
	first, _ := admitCoexistenceAdmissionMessage(t, app, account, phoneOnlyMessage(t, phone), "Fresh")
	require.NotNil(t, first)
	known := phoneOnlyMessage(t, phone)
	known.FromUserID = "US.first-user"
	promoted, _ := admitCoexistenceAdmissionMessage(t, app, account, known, "Known")
	require.NotNil(t, promoted)
	assert.Equal(t, first.Contact.ID, promoted.Contact.ID)
	other := phoneOnlyMessage(t, phone)
	other.FromUserID = "US.different-user"
	held, _ := admitCoexistenceAdmissionMessage(t, app, account, other, "Other user")
	assert.Nil(t, held)
	stored := loadCoexistenceAdmissionContact(t, app.DB, account.OrganizationID, first.Contact.ID)
	assert.Equal(t, known.FromUserID, stored.BSUID)
	assert.EqualValues(t, 1, countCoexistenceStagedReceipts(t, app, account.OrganizationID, other.ID))
}
