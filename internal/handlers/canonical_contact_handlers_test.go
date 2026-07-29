package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
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
	"github.com/zerodha/fastglue"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type canonicalMessageCapture struct {
	mu       sync.Mutex
	messages []map[string]any
}

func newCanonicalMessageTestApp(
	t *testing.T,
) (*App, *canonicalMessageCapture) {
	t.Helper()
	capture := &canonicalMessageCapture{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err == nil {
			capture.mu.Lock()
			capture.messages = append(capture.messages, payload)
			capture.mu.Unlock()
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"messages": []map[string]string{{"id": "wamid.canonical-test"}},
		})
	}))
	t.Cleanup(server.Close)

	log := testutil.NopLogger()
	app := &App{
		DB:         testutil.SetupTestDB(t),
		Log:        log,
		WhatsApp:   whatsapp.NewWithBaseURL(log, server.URL),
		HTTPClient: &http.Client{Timeout: 5 * time.Second},
	}
	if redisClient := testutil.SetupTestRedis(t); redisClient != nil {
		app.Redis = redisClient
	}
	return app, capture
}

func (c *canonicalMessageCapture) snapshot() []map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]map[string]any, len(c.messages))
	copy(out, c.messages)
	return out
}

func createCanonicalReplyMessage(
	t *testing.T,
	app *App,
	orgID, contactID uuid.UUID,
	accountName string,
) *models.Message {
	t.Helper()
	message := &models.Message{
		BaseModel:       models.BaseModel{ID: uuid.New()},
		OrganizationID:  orgID,
		WhatsAppAccount: accountName,
		ContactID:       contactID,
		Direction:       models.DirectionIncoming,
		MessageType:     models.MessageTypeText,
		Content:         "reply source",
		Status:          models.MessageStatusDelivered,
	}
	require.NoError(t, app.DB.Create(message).Error)
	t.Cleanup(func() { _ = app.DB.Delete(message).Error })
	return message
}

func startPendingContactMerge(
	t *testing.T,
	app *App,
	orgID, sourceID, targetID uuid.UUID,
) *gorm.DB {
	t.Helper()
	tx := app.DB.Begin()
	require.NoError(t, tx.Error)
	t.Cleanup(func() { _ = tx.Rollback().Error })

	var source models.Contact
	require.NoError(t, tx.Unscoped().
		Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("id = ? AND organization_id = ?", sourceID, orgID).
		First(&source).Error)
	require.NoError(t, tx.Unscoped().Model(&models.Contact{}).
		Where("id = ? AND organization_id = ?", sourceID, orgID).
		Updates(map[string]any{
			"merged_into_id": targetID,
			"merged_at":      time.Now().UTC(),
			"deleted_at":     time.Now().UTC(),
		}).Error)
	return tx
}

func assertStillWaitingForContactMerge(
	t *testing.T,
	result <-chan error,
) {
	t.Helper()
	select {
	case err := <-result:
		t.Fatalf("handler bypassed the pending contact merge lock: %v", err)
	case <-time.After(75 * time.Millisecond):
	}
}

func TestSendOutgoingMessage_WaitsForMergeAndPersistsAgainstCanonicalContact(t *testing.T) {
	app, capture := newCanonicalMessageTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	account := testutil.CreateTestWhatsAppAccount(t, app.DB, org.ID)
	canonical := testutil.CreateTestContactWith(
		t,
		app.DB,
		org.ID,
		testutil.WithPhoneNumber("+60111111111"),
		testutil.WithContactAccount(account.Name),
	)
	source := testutil.CreateTestContactWith(
		t,
		app.DB,
		org.ID,
		testutil.WithPhoneNumber("+60222222222"),
		testutil.WithContactAccount(account.Name),
	)

	mergeTx := startPendingContactMerge(t, app, org.ID, source.ID, canonical.ID)
	ctx := testutil.TestContext(t)
	type sendResult struct {
		message *models.Message
		err     error
	}
	result := make(chan sendResult, 1)
	go func() {
		message, err := app.SendOutgoingMessage(
			ctx,
			OutgoingMessageRequest{
				Account: account,
				Contact: source,
				Type:    models.MessageTypeText,
				Content: "canonical hello",
			},
			MessageSendOptions{},
		)
		result <- sendResult{message: message, err: err}
	}()

	select {
	case early := <-result:
		t.Fatalf("sender bypassed the pending contact merge lock: %v", early.err)
	case <-time.After(75 * time.Millisecond):
	}
	require.NoError(t, mergeTx.Commit().Error)

	var sent sendResult
	select {
	case sent = <-result:
	case <-time.After(5 * time.Second):
		t.Fatal("sender did not finish after contact merge committed")
	}
	require.NoError(t, sent.err)
	require.NotNil(t, sent.message)
	t.Cleanup(func() { _ = app.DB.Delete(sent.message).Error })
	assert.Equal(t, canonical.ID, sent.message.ContactID)

	var storedMessage models.Message
	require.NoError(t, app.DB.First(&storedMessage, sent.message.ID).Error)
	assert.Equal(t, canonical.ID, storedMessage.ContactID)

	var updatedCanonical models.Contact
	require.NoError(t, app.DB.First(&updatedCanonical, canonical.ID).Error)
	require.NotNil(t, updatedCanonical.LastMessageAt)
	assert.Equal(t, "canonical hello", updatedCanonical.LastMessagePreview)

	var sourceMessageCount int64
	require.NoError(t, app.DB.Model(&models.Message{}).
		Where("organization_id = ? AND contact_id = ?", org.ID, source.ID).
		Count(&sourceMessageCount).Error)
	assert.Zero(t, sourceMessageCount)

	sentPayloads := capture.snapshot()
	require.Len(t, sentPayloads, 1)
	assert.Equal(t, canonical.PhoneNumber, sentPayloads[0]["to"])
}

func TestSendOutgoingMessage_WaitsForMergeAndRejectsCanonicalMarketingOptOut(t *testing.T) {
	app, capture := newCanonicalMessageTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	account := testutil.CreateTestWhatsAppAccount(t, app.DB, org.ID)
	canonical := testutil.CreateTestContactWith(
		t,
		app.DB,
		org.ID,
		testutil.WithPhoneNumber("+60111111112"),
		testutil.WithContactAccount(account.Name),
	)
	require.NoError(t, app.DB.Model(canonical).Update("marketing_opt_out", true).Error)
	source := testutil.CreateTestContactWith(
		t,
		app.DB,
		org.ID,
		testutil.WithPhoneNumber("+60222222223"),
		testutil.WithContactAccount(account.Name),
	)
	require.False(t, source.MarketingOptOut)
	template := &models.Template{
		BaseModel:       models.BaseModel{ID: uuid.New()},
		OrganizationID:  org.ID,
		WhatsAppAccount: account.Name,
		Name:            "canonical_marketing_opt_out",
		DisplayName:     "Canonical Marketing Opt Out",
		Language:        "en",
		Category:        "MARKETING",
		Status:          string(models.TemplateStatusApproved),
		BodyContent:     "A marketing message that must not be delivered",
	}
	require.NoError(t, app.DB.Create(template).Error)

	mergeTx := startPendingContactMerge(t, app, org.ID, source.ID, canonical.ID)
	result := make(chan error, 1)
	go func() {
		_, sendErr := app.SendOutgoingMessage(
			testutil.TestContext(t),
			OutgoingMessageRequest{
				Account:  account,
				Contact:  source,
				Type:     models.MessageTypeTemplate,
				Template: template,
			},
			MessageSendOptions{},
		)
		result <- sendErr
	}()

	assertStillWaitingForContactMerge(t, result)
	require.NoError(t, mergeTx.Commit().Error)

	var sendErr error
	select {
	case sendErr = <-result:
	case <-time.After(5 * time.Second):
		t.Fatal("sender did not finish after contact merge committed")
	}
	require.Error(t, sendErr)
	var consentErr *OutgoingConsentError
	require.True(t, errors.As(sendErr, &consentErr))
	assert.Equal(t, canonical.ID, consentErr.ContactID)

	var messageCount int64
	require.NoError(t, app.DB.Model(&models.Message{}).
		Where("organization_id = ?", org.ID).
		Count(&messageCount).Error)
	assert.Zero(t, messageCount)
	assert.Empty(t, capture.snapshot())
}

func TestSendOutgoingMessage_HoldsConsentLockThroughProviderAttempt(t *testing.T) {
	providerStarted := make(chan struct{}, 1)
	releaseProvider := make(chan struct{})
	providerPayload := make(chan map[string]any, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		providerPayload <- payload
		providerStarted <- struct{}{}
		<-releaseProvider
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"messages": []map[string]string{{"id": "wamid.consent-lock"}},
		})
	}))
	t.Cleanup(func() {
		select {
		case <-releaseProvider:
		default:
			close(releaseProvider)
		}
		server.Close()
	})

	log := testutil.NopLogger()
	app := &App{
		DB:       testutil.SetupTestDB(t),
		Log:      log,
		WhatsApp: whatsapp.NewWithBaseURL(log, server.URL),
	}
	org := testutil.CreateTestOrganization(t, app.DB)
	account := testutil.CreateTestWhatsAppAccount(t, app.DB, org.ID)
	contact := testutil.CreateTestContactWith(
		t,
		app.DB,
		org.ID,
		testutil.WithContactAccount(account.Name),
	)
	template := &models.Template{
		BaseModel:       models.BaseModel{ID: uuid.New()},
		OrganizationID:  org.ID,
		WhatsAppAccount: account.Name,
		Name:            "consent_lock_marketing",
		DisplayName:     "Consent Lock Marketing",
		Language:        "en",
		Category:        "MARKETING",
		Status:          string(models.TemplateStatusApproved),
		BodyContent:     "Linearized marketing delivery",
	}
	require.NoError(t, app.DB.Create(template).Error)

	type sendResult struct {
		message *models.Message
		err     error
	}
	sendDone := make(chan sendResult, 1)
	go func() {
		message, err := app.SendOutgoingMessage(
			context.Background(),
			OutgoingMessageRequest{
				Account:  account,
				Contact:  contact,
				Type:     models.MessageTypeTemplate,
				Template: template,
			},
			MessageSendOptions{},
		)
		sendDone <- sendResult{message: message, err: err}
	}()

	select {
	case <-providerStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("provider attempt did not start")
	}

	updateStarted := make(chan struct{})
	optOutDone := make(chan error, 1)
	go func() {
		close(updateStarted)
		optOutDone <- app.DB.Model(&models.Contact{}).
			Where("id = ? AND organization_id = ?", contact.ID, org.ID).
			Update("marketing_opt_out", true).Error
	}()
	<-updateStarted
	select {
	case err := <-optOutDone:
		t.Fatalf("opt-out committed while provider attempt still held the canonical lock: %v", err)
	case <-time.After(75 * time.Millisecond):
	}

	close(releaseProvider)
	var sent sendResult
	select {
	case sent = <-sendDone:
	case <-time.After(5 * time.Second):
		t.Fatal("outgoing send did not finish")
	}
	require.NoError(t, sent.err)
	require.NotNil(t, sent.message)
	select {
	case err := <-optOutDone:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("opt-out did not commit after provider delivery released the lock")
	}

	select {
	case payload := <-providerPayload:
		assert.Equal(t, contact.PhoneNumber, payload["to"])
	default:
		t.Fatal("provider payload was not captured")
	}
	var storedMessage models.Message
	require.NoError(t, app.DB.First(&storedMessage, sent.message.ID).Error)
	assert.Equal(t, models.MessageStatusSent, storedMessage.Status)
	assert.Equal(t, "wamid.consent-lock", storedMessage.WhatsAppMessageID)
	var storedContact models.Contact
	require.NoError(t, app.DB.First(&storedContact, contact.ID).Error)
	assert.True(t, storedContact.MarketingOptOut)
}

func TestSendOutgoingMessage_CommitsProviderResultOutsideOuterTenantRollback(t *testing.T) {
	app, capture := newCanonicalMessageTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	account := testutil.CreateTestWhatsAppAccount(t, app.DB, org.ID)
	contact := testutil.CreateTestContactWith(
		t,
		app.DB,
		org.ID,
		testutil.WithContactAccount(account.Name),
	)
	template := &models.Template{
		BaseModel:       models.BaseModel{ID: uuid.New()},
		OrganizationID:  org.ID,
		WhatsAppAccount: account.Name,
		Name:            "outer_rollback_marketing",
		DisplayName:     "Outer Rollback Marketing",
		Language:        "en",
		Category:        "MARKETING",
		Status:          string(models.TemplateStatusApproved),
		BodyContent:     "Durable outside the request transaction",
	}
	require.NoError(t, app.DB.Create(template).Error)
	app.Config = &config.Config{
		Database: config.DatabaseConfig{RLSEnabled: true},
	}

	outer := app.DB.Begin()
	require.NoError(t, outer.Error)
	t.Cleanup(func() { _ = outer.Rollback().Error })
	require.NoError(t, database.SetTenantContext(outer, org.ID))
	scoped := app.scopedApp(outer, org.ID)
	message, err := scoped.SendOutgoingMessage(
		context.Background(),
		OutgoingMessageRequest{
			Account:  account,
			Contact:  contact,
			Type:     models.MessageTypeTemplate,
			Template: template,
		},
		MessageSendOptions{},
	)
	require.NoError(t, err)
	require.NotNil(t, message)
	require.NoError(t, outer.Rollback().Error)

	var stored models.Message
	require.NoError(t, app.DB.First(&stored, message.ID).Error)
	assert.Equal(t, models.MessageStatusSent, stored.Status)
	assert.Equal(t, "wamid.canonical-test", stored.WhatsAppMessageID)
	assert.Equal(t, contact.ID, stored.ContactID)
	assert.Len(t, capture.snapshot(), 1)
}

func TestSendTemplateMessage_RLSPhoneOnlyContactIsVisibleToRootSend(t *testing.T) {
	app, capture := newCanonicalMessageTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	adminRole := testutil.CreateAdminRole(t, app.DB, org.ID)
	user := testutil.CreateTestUser(
		t,
		app.DB,
		org.ID,
		testutil.WithRoleID(&adminRole.ID),
	)
	account := testutil.CreateTestWhatsAppAccount(t, app.DB, org.ID)
	template := &models.Template{
		BaseModel:       models.BaseModel{ID: uuid.New()},
		OrganizationID:  org.ID,
		WhatsAppAccount: account.Name,
		Name:            "rls_phone_only_template",
		DisplayName:     "RLS Phone Only",
		Language:        "en",
		Category:        "UTILITY",
		Status:          string(models.TemplateStatusApproved),
		BodyContent:     "A phone-only template message",
	}
	require.NoError(t, app.DB.Create(template).Error)
	app.Config = &config.Config{
		Database: config.DatabaseConfig{RLSEnabled: true},
	}

	const phoneNumber = "601155501234"
	req := testutil.NewJSONRequest(t, map[string]any{
		"phone_number":  phoneNumber,
		"template_name": template.Name,
	})
	testutil.SetAuthContext(req, org.ID, user.ID)

	handlerDone := make(chan error, 1)
	go func() {
		handlerDone <- app.Tenant((*App).SendTemplateMessage)(req)
	}()
	select {
	case err := <-handlerDone:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("phone-only template handler deadlocked in the outer tenant transaction")
	}
	assert.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req))

	backgroundDone := make(chan struct{})
	go func() {
		app.WaitForBackgroundTasks()
		close(backgroundDone)
	}()
	select {
	case <-backgroundDone:
	case <-time.After(5 * time.Second):
		t.Fatal("phone-only template delivery did not finish")
	}

	var contact models.Contact
	require.NoError(t, app.DB.Where(
		"organization_id = ? AND phone_number = ?",
		org.ID,
		phoneNumber,
	).First(&contact).Error)
	var message models.Message
	require.NoError(t, app.DB.Where(
		"organization_id = ? AND contact_id = ? AND template_name = ?",
		org.ID,
		contact.ID,
		template.Name,
	).First(&message).Error)
	assert.Equal(t, models.MessageStatusSent, message.Status)
	assert.Equal(t, "wamid.canonical-test", message.WhatsAppMessageID)
	require.Len(t, capture.snapshot(), 1)
	assert.Equal(t, phoneNumber, capture.snapshot()[0]["to"])
}

func TestSendOutgoingMessage_RechecksAgentVisibilityAfterMerge(t *testing.T) {
	app, capture := newCanonicalMessageTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	account := testutil.CreateTestWhatsAppAccount(t, app.DB, org.ID)
	limitedRole := testutil.CreateTestRoleExact(
		t,
		app.DB,
		org.ID,
		"canonical-send-limited",
		false,
		false,
		nil,
	)
	agent := testutil.CreateTestUser(t, app.DB, org.ID, testutil.WithRoleID(&limitedRole.ID))
	otherAgent := testutil.CreateTestUser(t, app.DB, org.ID)
	canonical := testutil.CreateTestContactWith(
		t,
		app.DB,
		org.ID,
		testutil.WithContactAccount(account.Name),
	)
	require.NoError(t, app.DB.Model(canonical).Update("assigned_user_id", otherAgent.ID).Error)
	source := testutil.CreateTestContactWith(
		t,
		app.DB,
		org.ID,
		testutil.WithContactAccount(account.Name),
	)
	require.NoError(t, app.DB.Model(source).Update("assigned_user_id", agent.ID).Error)

	mergeTx := startPendingContactMerge(t, app, org.ID, source.ID, canonical.ID)
	ctx := testutil.TestContext(t)
	result := make(chan error, 1)
	go func() {
		_, sendErr := app.SendOutgoingMessage(
			ctx,
			OutgoingMessageRequest{
				Account: account,
				Contact: source,
				Type:    models.MessageTypeText,
				Content: "must not send",
			},
			MessageSendOptions{SentByUserID: &agent.ID},
		)
		result <- sendErr
	}()

	assertStillWaitingForContactMerge(t, result)
	require.NoError(t, mergeTx.Commit().Error)
	sendErr := <-result
	require.Error(t, sendErr)
	assert.True(t, errors.Is(sendErr, errOutgoingContactAccessRevoked))

	var messageCount int64
	require.NoError(t, app.DB.Model(&models.Message{}).
		Where("organization_id = ?", org.ID).
		Count(&messageCount).Error)
	assert.Zero(t, messageCount)
	assert.Empty(t, capture.snapshot())
}

func TestSendOutgoingMessage_RejectsReplyFromAnotherContactOrTenant(t *testing.T) {
	app, capture := newCanonicalMessageTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	otherOrg := testutil.CreateTestOrganization(t, app.DB)
	account := testutil.CreateTestWhatsAppAccount(t, app.DB, org.ID)
	contact := testutil.CreateTestContactWith(
		t,
		app.DB,
		org.ID,
		testutil.WithContactAccount(account.Name),
	)
	otherContact := testutil.CreateTestContact(t, app.DB, org.ID)
	foreignContact := testutil.CreateTestContact(t, app.DB, otherOrg.ID)
	otherContactReply := createCanonicalReplyMessage(
		t,
		app,
		org.ID,
		otherContact.ID,
		account.Name,
	)
	foreignReply := createCanonicalReplyMessage(
		t,
		app,
		otherOrg.ID,
		foreignContact.ID,
		"foreign-account",
	)

	for name, reply := range map[string]*models.Message{
		"another contact": otherContactReply,
		"another tenant":  foreignReply,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := app.SendOutgoingMessage(
				testutil.TestContext(t),
				OutgoingMessageRequest{
					Account:        account,
					Contact:        contact,
					Type:           models.MessageTypeText,
					Content:        "invalid reply",
					ReplyToMessage: reply,
				},
				MessageSendOptions{},
			)
			require.Error(t, err)
			assert.True(t, errors.Is(err, errOutgoingReplyInvalid))
		})
	}

	var messageCount int64
	require.NoError(t, app.DB.Model(&models.Message{}).
		Where("organization_id = ? AND contact_id = ?", org.ID, contact.ID).
		Count(&messageCount).Error)
	assert.Zero(t, messageCount)
	assert.Empty(t, capture.snapshot())
}

func TestSendMessage_MapsMergeRevokedVisibilityToNotFound(t *testing.T) {
	app, _ := newCanonicalMessageTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	account := testutil.CreateTestWhatsAppAccount(t, app.DB, org.ID)
	limitedRole := testutil.CreateTestRoleWithKeys(
		t,
		app.DB,
		org.ID,
		"canonical-http-limited",
		[]string{models.ResourceChat + ":" + models.ActionWrite},
	)
	agent := testutil.CreateTestUser(t, app.DB, org.ID, testutil.WithRoleID(&limitedRole.ID))
	canonical := testutil.CreateTestContactWith(
		t,
		app.DB,
		org.ID,
		testutil.WithContactAccount(account.Name),
	)
	source := testutil.CreateTestContactWith(
		t,
		app.DB,
		org.ID,
		testutil.WithContactAccount(account.Name),
	)
	require.NoError(t, app.DB.Model(source).Update("assigned_user_id", agent.ID).Error)

	mergeTx := startPendingContactMerge(t, app, org.ID, source.ID, canonical.ID)
	req := testutil.NewJSONRequest(t, map[string]any{
		"type":    models.MessageTypeText,
		"content": map[string]string{"body": "stale access"},
	})
	testutil.SetAuthContext(req, org.ID, agent.ID)
	testutil.SetPathParam(req, "id", source.ID.String())

	result := make(chan error, 1)
	go func() { result <- app.SendMessage(req) }()
	assertStillWaitingForContactMerge(t, result)
	require.NoError(t, mergeTx.Commit().Error)
	require.NoError(t, <-result)
	assert.Equal(t, fasthttp.StatusNotFound, testutil.GetResponseStatusCode(req))
}

func TestContactMutators_WaitForMergeAndWriteCanonicalActivity(t *testing.T) {
	tests := []struct {
		name          string
		body          map[string]any
		call          func(*App, *fastglue.Request) error
		activityTitle string
		assertContact func(*testing.T, *models.Contact)
	}{
		{
			name: "assign contact",
			body: map[string]any{},
			call: func(app *App, req *fastglue.Request) error {
				return app.AssignContact(req)
			},
			activityTitle: "Contact assignment updated",
		},
		{
			name: "update tags",
			body: map[string]any{"tags": []string{"vip", "renewal"}},
			call: func(app *App, req *fastglue.Request) error {
				return app.UpdateContactTags(req)
			},
			activityTitle: "Contact tags updated",
			assertContact: func(t *testing.T, contact *models.Contact) {
				t.Helper()
				assert.ElementsMatch(t, []any{"vip", "renewal"}, []any(contact.Tags))
			},
		},
		{
			name: "update contact",
			body: map[string]any{"profile_name": "Canonical Profile"},
			call: func(app *App, req *fastglue.Request) error {
				return app.UpdateContact(req)
			},
			activityTitle: "Contact updated",
			assertContact: func(t *testing.T, contact *models.Contact) {
				t.Helper()
				assert.Equal(t, "Canonical Profile", contact.ProfileName)
			},
		},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			app := newProcessorTestApp(t)
			org := testutil.CreateTestOrganization(t, app.DB)
			adminRole := testutil.CreateAdminRole(t, app.DB, org.ID)
			actor := testutil.CreateTestUser(
				t,
				app.DB,
				org.ID,
				testutil.WithRoleID(&adminRole.ID),
			)
			assignee := testutil.CreateTestUser(t, app.DB, org.ID)
			canonical := testutil.CreateTestContact(t, app.DB, org.ID)
			source := testutil.CreateTestContact(t, app.DB, org.ID)

			body := make(map[string]any, len(testCase.body)+1)
			for key, value := range testCase.body {
				body[key] = value
			}
			if testCase.name == "assign contact" {
				body["user_id"] = assignee.ID.String()
				testCase.assertContact = func(t *testing.T, contact *models.Contact) {
					t.Helper()
					require.NotNil(t, contact.AssignedUserID)
					assert.Equal(t, assignee.ID, *contact.AssignedUserID)
				}
			}

			mergeTx := startPendingContactMerge(t, app, org.ID, source.ID, canonical.ID)
			req := testutil.NewJSONRequest(t, body)
			testutil.SetAuthContext(req, org.ID, actor.ID)
			testutil.SetPathParam(req, "id", source.ID.String())
			result := make(chan error, 1)
			go func() { result <- testCase.call(app, req) }()

			assertStillWaitingForContactMerge(t, result)
			require.NoError(t, mergeTx.Commit().Error)
			require.NoError(t, <-result)
			assert.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req))

			var updatedCanonical models.Contact
			require.NoError(t, app.DB.First(&updatedCanonical, canonical.ID).Error)
			require.NotNil(t, testCase.assertContact)
			testCase.assertContact(t, &updatedCanonical)

			var activity models.CustomerActivityEvent
			require.NoError(t, app.DB.Where(
				"organization_id = ? AND contact_id = ? AND title = ?",
				org.ID,
				canonical.ID,
				testCase.activityTitle,
			).First(&activity).Error)
			require.NotNil(t, activity.SourceObjectID)
			assert.Equal(t, canonical.ID, *activity.SourceObjectID)

			var sourceActivityCount int64
			require.NoError(t, app.DB.Model(&models.CustomerActivityEvent{}).
				Where("organization_id = ? AND contact_id = ?", org.ID, source.ID).
				Count(&sourceActivityCount).Error)
			assert.Zero(t, sourceActivityCount)
		})
	}
}
