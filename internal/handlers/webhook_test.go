package handlers

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
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
	channelapi "github.com/shridarpatil/whatomate/internal/channel"
	"github.com/shridarpatil/whatomate/internal/config"
	appcrypto "github.com/shridarpatil/whatomate/internal/crypto"
	"github.com/shridarpatil/whatomate/internal/database"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/internal/websocket"
	"github.com/shridarpatil/whatomate/internal/whatsappaccount"
	"github.com/shridarpatil/whatomate/pkg/whatsapp"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

const webhookTestAppSecret = "webhook-test-app-secret"

func TestVerifyWebhookSignature(t *testing.T) {
	t.Parallel()

	// Test data
	appSecret := []byte("test_app_secret_12345")
	body := []byte(`{"object":"whatsapp_business_account","entry":[{"id":"123","changes":[]}]}`)

	// Compute valid signature
	mac := hmac.New(sha256.New, appSecret)
	mac.Write(body)
	validSig := "sha256=" + hex.EncodeToString(mac.Sum(nil))

	tests := []struct {
		name      string
		body      []byte
		signature []byte
		appSecret []byte
		want      bool
	}{
		{
			name:      "valid signature",
			body:      body,
			signature: []byte(validSig),
			appSecret: appSecret,
			want:      true,
		},
		{
			name:      "invalid signature - wrong hash",
			body:      body,
			signature: []byte("sha256=0000000000000000000000000000000000000000000000000000000000000000"),
			appSecret: appSecret,
			want:      false,
		},
		{
			name:      "invalid signature - wrong secret",
			body:      body,
			signature: []byte(validSig),
			appSecret: []byte("wrong_secret"),
			want:      false,
		},
		{
			name:      "invalid signature - modified body",
			body:      []byte(`{"object":"modified"}`),
			signature: []byte(validSig),
			appSecret: appSecret,
			want:      false,
		},
		{
			name:      "invalid signature - missing sha256 prefix",
			body:      body,
			signature: []byte(hex.EncodeToString(mac.Sum(nil))),
			appSecret: appSecret,
			want:      false,
		},
		{
			name:      "invalid signature - wrong prefix",
			body:      body,
			signature: []byte("sha1=" + hex.EncodeToString(mac.Sum(nil))),
			appSecret: appSecret,
			want:      false,
		},
		{
			name:      "empty signature",
			body:      body,
			signature: []byte{},
			appSecret: appSecret,
			want:      false,
		},
		{
			name: "empty body with valid signature for empty body",
			body: []byte{},
			signature: func() []byte {
				m := hmac.New(sha256.New, appSecret)
				m.Write([]byte{})
				return []byte("sha256=" + hex.EncodeToString(m.Sum(nil)))
			}(),
			appSecret: appSecret,
			want:      true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := verifyWebhookSignature(tt.body, tt.signature, tt.appSecret)
			assert.Equal(t, tt.want, got, "verifyWebhookSignature() = %v, want %v", got, tt.want)
		})
	}
}

func TestVerifyWebhookSignature_RealWorldExample(t *testing.T) {
	t.Parallel()

	// Simulate a real Meta webhook payload
	payload := `{"object":"whatsapp_business_account","entry":[{"id":"123456789","changes":[{"value":{"messaging_product":"whatsapp","metadata":{"display_phone_number":"15551234567","phone_number_id":"987654321"},"messages":[{"from":"15559876543","id":"wamid.abc123","timestamp":"1234567890","type":"text","text":{"body":"Hello"}}]},"field":"messages"}]}]}`
	appSecret := "my_app_secret_from_meta_dashboard"

	// Compute signature like Meta would
	mac := hmac.New(sha256.New, []byte(appSecret))
	mac.Write([]byte(payload))
	signature := "sha256=" + hex.EncodeToString(mac.Sum(nil))

	// Verify
	result := verifyWebhookSignature([]byte(payload), []byte(signature), []byte(appSecret))
	assert.True(t, result, "Should verify real-world webhook payload")
}

func TestVerifyWebhookSignature_TimingAttackResistance(t *testing.T) {
	t.Parallel()

	// This test ensures we use constant-time comparison
	// by verifying the function behaves correctly with similar signatures
	appSecret := []byte("test_secret")
	body := []byte("test body")

	mac := hmac.New(sha256.New, appSecret)
	mac.Write(body)
	validSig := "sha256=" + hex.EncodeToString(mac.Sum(nil))

	// Create a signature that differs only in the last character
	almostValidSig := validSig[:len(validSig)-1] + "0"

	assert.True(t, verifyWebhookSignature(body, []byte(validSig), appSecret))
	assert.False(t, verifyWebhookSignature(body, []byte(almostValidSig), appSecret))
}

// webhookTestApp creates a minimal App for webhook tests.
func webhookTestApp(t *testing.T) *App {
	t.Helper()
	db := testutil.SetupTestDB(t)
	redisClient := testutil.SetupTestRedis(t)
	if redisClient == nil {
		t.Skip("TEST_REDIS_URL not set, skipping test")
	}
	cfg := &config.Config{
		JWT: config.JWTConfig{
			Secret:            testutil.TestJWTSecret,
			AccessExpiryMins:  15,
			RefreshExpiryDays: 7,
		},
		App: config.AppConfig{
			EncryptionKey: "test-encryption-key-32-bytes-long",
		},
		WhatsApp: config.WhatsAppConfig{
			AppSecret: webhookTestAppSecret,
		},
	}
	return &App{
		Config: cfg,
		DB:     db,
		Log:    testutil.NopLogger(),
		Redis:  redisClient,
	}
}

// webhookTestData creates an organization, message with campaign metadata,
// campaign, and recipient. Returns all created records.
func webhookTestData(t *testing.T, app *App, msgStatus models.MessageStatus) (models.Organization, models.Message, models.BulkMessageCampaign, models.BulkMessageRecipient) {
	t.Helper()
	uid := uuid.New().String()[:8]

	org := models.Organization{
		BaseModel: models.BaseModel{ID: uuid.New()},
		Name:      "wh-test-" + uid,
		Slug:      "wh-test-" + uid,
	}
	require.NoError(t, app.DB.Create(&org).Error)

	contact := models.Contact{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: org.ID,
		PhoneNumber:    "919999" + uid,
		ProfileName:    "Test User",
	}
	require.NoError(t, app.DB.Create(&contact).Error)

	waAccount := models.WhatsAppAccount{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: org.ID,
		Name:           "wh-acct-" + uid,
		PhoneID:        "phone-" + uid,
		BusinessID:     "biz-" + uid,
		AccessToken:    "token",
	}
	require.NoError(t, app.DB.Create(&waAccount).Error)

	tmpl := models.Template{
		BaseModel:       models.BaseModel{ID: uuid.New()},
		OrganizationID:  org.ID,
		WhatsAppAccount: waAccount.Name,
		Name:            "tmpl-" + uid,
		Language:        "en",
		BodyContent:     "Hello {{1}}",
	}
	require.NoError(t, app.DB.Create(&tmpl).Error)

	user := models.User{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: org.ID,
		Email:          "wh-" + uid + "@test.com",
		FullName:       "Test User",
		PasswordHash:   "hash",
	}
	require.NoError(t, app.DB.Create(&user).Error)

	campaign := models.BulkMessageCampaign{
		BaseModel:       models.BaseModel{ID: uuid.New()},
		OrganizationID:  org.ID,
		WhatsAppAccount: waAccount.Name,
		Name:            "test-campaign-" + uid,
		TemplateID:      tmpl.ID,
		Status:          models.CampaignStatusCompleted,
		CreatedBy:       user.ID,
	}
	require.NoError(t, app.DB.Create(&campaign).Error)

	waMsgID := "wamid.test-" + uid

	msg := models.Message{
		BaseModel:         models.BaseModel{ID: uuid.NewSHA1(waAccount.ID, []byte("coexistence-message:"+waMsgID))},
		OrganizationID:    org.ID,
		WhatsAppAccount:   waAccount.Name,
		ContactID:         contact.ID,
		WhatsAppMessageID: waMsgID,
		Direction:         models.DirectionOutgoing,
		MessageType:       models.MessageTypeTemplate,
		Content:           "test message",
		Status:            msgStatus,
		Metadata: models.JSONB{
			"campaign_id":                          campaign.ID.String(),
			database.WhatsAppWAMIDOwnerMetadataKey: true,
		},
	}
	require.NoError(t, app.DB.Create(&msg).Error)

	recipient := models.BulkMessageRecipient{
		BaseModel:         models.BaseModel{ID: uuid.New()},
		CampaignID:        campaign.ID,
		MessageID:         &msg.ID,
		PhoneNumber:       contact.PhoneNumber,
		WhatsAppMessageID: waMsgID,
		Status:            msgStatus,
	}
	require.NoError(t, app.DB.Create(&recipient).Error)

	return org, msg, campaign, recipient
}

func processWebhookMessageStatus(t *testing.T, app *App, message models.Message, status string, statusErrors []WebhookStatusError) {
	t.Helper()
	var account models.WhatsAppAccount
	require.NoError(t, app.DB.Where("organization_id = ?", message.OrganizationID).Order("id").First(&account).Error)
	require.NoError(t, app.processStatusUpdate(account.PhoneID, WebhookStatus{ID: message.WhatsAppMessageID, Status: status, Errors: statusErrors}))
}

func TestCampaignStatusCounterTransition(t *testing.T) {
	t.Parallel()

	assert.Equal(t, campaignStatusCounters{delivered: 1, read: 1},
		campaignStatusCounterTransition(models.MessageStatusSent, models.MessageStatusRead))
	assert.Equal(t, campaignStatusCounters{sent: -1, delivered: -1, read: -1, failed: 1},
		campaignStatusCounterTransition(models.MessageStatusRead, models.MessageStatusFailed))
}

func TestIncrementCampaignStatPublishesOnlyAfterTenantCommit(t *testing.T) {
	app := newProcessorTestApp(t)
	organization, _, campaign, _ := webhookTestData(t, app, models.MessageStatusSent)

	hub := websocket.NewHub(app.Log)
	go hub.Run()
	app.WSHub = hub
	client := websocket.NewClient(hub, nil, uuid.New(), organization.ID)
	hub.Register(client)
	testutil.AssertEventually(t, func() bool {
		return hub.GetClientCount() == 1
	}, 2*time.Second, "campaign stats client registered")

	forcedRollback := fmt.Errorf("force campaign stat rollback")
	err := app.WithCommittedTenantApp(organization.ID, func(scoped *App) error {
		require.NoError(t, scoped.incrementCampaignStat(
			campaign.ID,
			organization.ID,
			string(models.MessageStatusDelivered),
		))
		select {
		case payload := <-client.SendChan():
			t.Fatalf("campaign stats published before rollback: %s", payload)
		default:
		}
		return forcedRollback
	})
	require.ErrorIs(t, err, forcedRollback)
	select {
	case payload := <-client.SendChan():
		t.Fatalf("rolled-back campaign stat was published: %s", payload)
	case <-time.After(100 * time.Millisecond):
	}

	var rolledBack models.BulkMessageCampaign
	require.NoError(t, app.DB.First(&rolledBack, campaign.ID).Error)
	assert.Zero(t, rolledBack.DeliveredCount)

	err = app.WithCommittedTenantApp(organization.ID, func(scoped *App) error {
		require.NoError(t, scoped.incrementCampaignStat(
			campaign.ID,
			organization.ID,
			string(models.MessageStatusDelivered),
		))
		select {
		case payload := <-client.SendChan():
			t.Fatalf("campaign stats published before commit: %s", payload)
		default:
		}
		return nil
	})
	require.NoError(t, err)

	select {
	case payload := <-client.SendChan():
		var message websocket.WSMessage
		require.NoError(t, json.Unmarshal(payload, &message))
		assert.Equal(t, websocket.TypeCampaignStatsUpdate, message.Type)
		body, ok := message.Payload.(map[string]any)
		require.True(t, ok)
		assert.Equal(t, campaign.ID.String(), body["campaign_id"])
		assert.Equal(t, float64(1), body["delivered_count"])
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for committed campaign stats")
	}

	var committed models.BulkMessageCampaign
	require.NoError(t, app.DB.First(&committed, campaign.ID).Error)
	assert.Equal(t, 1, committed.DeliveredCount)
}

func createStatusAuthority(t *testing.T, app *App, organizationID uuid.UUID, name, phoneID string) models.WhatsAppAccount {
	t.Helper()
	account := models.WhatsAppAccount{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: organizationID,
		Name:           name,
		PhoneID:        phoneID,
		BusinessID:     "status-business-" + uuid.NewString()[:8],
		AccessToken:    "status-token",
	}
	require.NoError(t, app.DB.Create(&account).Error)
	return account
}

func TestUpdateMessageStatus_DeliveredUpdatesRecipient(t *testing.T) {
	app := newProcessorTestApp(t)
	_, msg, campaign, recipient := webhookTestData(t, app, models.MessageStatusSent)

	processWebhookMessageStatus(t, app, msg, "delivered", nil)

	// Verify recipient status and delivered_at
	var updated models.BulkMessageRecipient
	require.NoError(t, app.DB.First(&updated, recipient.ID).Error)
	assert.Equal(t, models.MessageStatusDelivered, updated.Status)
	assert.NotNil(t, updated.DeliveredAt)
	assert.Nil(t, updated.ReadAt)

	// Verify campaign counter incremented
	var updatedCampaign models.BulkMessageCampaign
	require.NoError(t, app.DB.First(&updatedCampaign, campaign.ID).Error)
	assert.Equal(t, 1, updatedCampaign.DeliveredCount)
}

func TestUpdateMessageStatus_ReadUpdatesRecipient(t *testing.T) {
	app := newProcessorTestApp(t)
	_, msg, campaign, recipient := webhookTestData(t, app, models.MessageStatusDelivered)

	processWebhookMessageStatus(t, app, msg, "read", nil)

	// Verify recipient status and read_at
	var updated models.BulkMessageRecipient
	require.NoError(t, app.DB.First(&updated, recipient.ID).Error)
	assert.Equal(t, models.MessageStatusRead, updated.Status)
	assert.NotNil(t, updated.ReadAt)

	// Verify campaign counter incremented
	var updatedCampaign models.BulkMessageCampaign
	require.NoError(t, app.DB.First(&updatedCampaign, campaign.ID).Error)
	assert.Equal(t, 1, updatedCampaign.ReadCount)
}

func TestUpdateMessageStatus_NonCampaignMessageIgnoresRecipient(t *testing.T) {
	app := newProcessorTestApp(t)
	uid := uuid.New().String()[:8]

	org := models.Organization{
		BaseModel: models.BaseModel{ID: uuid.New()},
		Name:      "wh-nocampaign-" + uid,
		Slug:      "wh-nocampaign-" + uid,
	}
	require.NoError(t, app.DB.Create(&org).Error)

	contact := models.Contact{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: org.ID,
		PhoneNumber:    "918888" + uid,
		ProfileName:    "No Campaign",
	}
	require.NoError(t, app.DB.Create(&contact).Error)
	account := createStatusAuthority(t, app, org.ID, "no-campaign-account-"+uid, "no-campaign-phone-"+uid)

	waMsgID := "wamid.nocampaign-" + uid
	msg := models.Message{
		BaseModel:         models.BaseModel{ID: uuid.NewSHA1(account.ID, []byte("coexistence-message:"+waMsgID))},
		OrganizationID:    org.ID,
		WhatsAppAccount:   account.Name,
		ContactID:         contact.ID,
		WhatsAppMessageID: waMsgID,
		Direction:         models.DirectionOutgoing,
		MessageType:       models.MessageTypeText,
		Content:           "hello",
		Status:            models.MessageStatusSent,
		Metadata:          models.JSONB{database.WhatsAppWAMIDOwnerMetadataKey: true}, // no campaign_id
	}
	require.NoError(t, app.DB.Create(&msg).Error)

	// Should update message status but not panic or fail
	processWebhookMessageStatus(t, app, msg, "delivered", nil)

	var updated models.Message
	require.NoError(t, app.DB.First(&updated, msg.ID).Error)
	assert.Equal(t, models.MessageStatusDelivered, updated.Status)
}

func TestUpdateMessageStatus_StatusPriorityRespected(t *testing.T) {
	app := newProcessorTestApp(t)
	_, msg, _, recipient := webhookTestData(t, app, models.MessageStatusRead)

	// Attempt to downgrade from read -> delivered (should be ignored)
	processWebhookMessageStatus(t, app, msg, "delivered", nil)

	var updated models.BulkMessageRecipient
	require.NoError(t, app.DB.First(&updated, recipient.ID).Error)
	// Status should remain "read"
	assert.Equal(t, models.MessageStatusRead, updated.Status)
}

func TestUpdateMessageStatus_FailedUpdatesMessage(t *testing.T) {
	app := newProcessorTestApp(t)
	_, msg, campaign, recipient := webhookTestData(t, app, models.MessageStatusSent)

	errors := []WebhookStatusError{
		{Code: 131047, Title: "Re-engagement message", Message: "Message failed to send because more than 24 hours have passed"},
	}
	processWebhookMessageStatus(t, app, msg, "failed", errors)

	// Verify message status and error
	var updatedMsg models.Message
	require.NoError(t, app.DB.First(&updatedMsg, msg.ID).Error)
	assert.Equal(t, models.MessageStatusFailed, updatedMsg.Status)
	assert.Contains(t, updatedMsg.ErrorMessage, "more than 24 hours")

	// Verify recipient status updated
	var updatedRecipient models.BulkMessageRecipient
	require.NoError(t, app.DB.First(&updatedRecipient, recipient.ID).Error)
	assert.Equal(t, models.MessageStatusFailed, updatedRecipient.Status)
	assert.Contains(t, updatedRecipient.ErrorMessage, "more than 24 hours")

	// Verify campaign failed counter
	var updatedCampaign models.BulkMessageCampaign
	require.NoError(t, app.DB.First(&updatedCampaign, campaign.ID).Error)
	assert.Equal(t, 1, updatedCampaign.FailedCount)
}

func TestUpdateMessageStatus_DuplicateFailedReceiptIsIdempotent(t *testing.T) {
	app := newProcessorTestApp(t)
	_, msg, campaign, recipient := webhookTestData(t, app, models.MessageStatusSent)
	firstError := []WebhookStatusError{{
		Code:    131047,
		Title:   "First failure",
		Message: "first durable provider failure",
	}}
	secondError := []WebhookStatusError{{
		Code:    131048,
		Title:   "Duplicate failure",
		Message: "duplicate receipt must not replace the first failure",
	}}

	processWebhookMessageStatus(t, app, msg, "failed", firstError)
	processWebhookMessageStatus(t, app, msg, "failed", secondError)

	var storedMessage models.Message
	var storedRecipient models.BulkMessageRecipient
	var storedCampaign models.BulkMessageCampaign
	require.NoError(t, app.DB.First(&storedMessage, msg.ID).Error)
	require.NoError(t, app.DB.First(&storedRecipient, recipient.ID).Error)
	require.NoError(t, app.DB.First(&storedCampaign, campaign.ID).Error)
	assert.Equal(t, models.MessageStatusFailed, storedMessage.Status)
	assert.Equal(t, firstError[0].Message, storedMessage.ErrorMessage)
	assert.Equal(t, models.MessageStatusFailed, storedRecipient.Status)
	assert.Equal(t, firstError[0].Message, storedRecipient.ErrorMessage)
	assert.Equal(t, 1, storedCampaign.FailedCount)
}

func TestUpdateMessageStatus_ConcurrentFailedReceiptsIncrementCampaignOnce(t *testing.T) {
	app := newProcessorTestApp(t)
	_, msg, campaign, recipient := webhookTestData(t, app, models.MessageStatusSent)
	var account models.WhatsAppAccount
	require.NoError(t, app.DB.Where(
		"organization_id = ?", msg.OrganizationID,
	).Order("id").First(&account).Error)
	status := WebhookStatus{
		ID:     msg.WhatsAppMessageID,
		Status: "failed",
		Errors: []WebhookStatusError{{Message: "concurrent provider failure"}},
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	var wait sync.WaitGroup
	wait.Add(2)
	for range 2 {
		go func() {
			defer wait.Done()
			<-start
			results <- app.processStatusUpdate(account.PhoneID, status)
		}()
	}
	close(start)
	wait.Wait()
	close(results)
	for err := range results {
		require.NoError(t, err)
	}

	var storedMessage models.Message
	var storedRecipient models.BulkMessageRecipient
	var storedCampaign models.BulkMessageCampaign
	require.NoError(t, app.DB.First(&storedMessage, msg.ID).Error)
	require.NoError(t, app.DB.First(&storedRecipient, recipient.ID).Error)
	require.NoError(t, app.DB.First(&storedCampaign, campaign.ID).Error)
	assert.Equal(t, models.MessageStatusFailed, storedMessage.Status)
	assert.Equal(t, models.MessageStatusFailed, storedRecipient.Status)
	assert.Equal(t, 1, storedCampaign.FailedCount)
}

func TestUpdateMessageStatus_ConcurrentDeliveredAndReadKeepsCumulativeCounters(t *testing.T) {
	app := newProcessorTestApp(t)
	_, msg, campaign, recipient := webhookTestData(t, app, models.MessageStatusSent)
	require.NoError(t, app.DB.Model(&models.BulkMessageCampaign{}).
		Where("id = ?", campaign.ID).
		Update("sent_count", 1).Error)
	var account models.WhatsAppAccount
	require.NoError(t, app.DB.Where("organization_id = ?", msg.OrganizationID).Order("id").First(&account).Error)

	start := make(chan struct{})
	results := make(chan error, 2)
	var wait sync.WaitGroup
	for _, statusValue := range []string{"delivered", "read"} {
		wait.Add(1)
		go func(statusValue string) {
			defer wait.Done()
			<-start
			results <- app.processStatusUpdate(account.PhoneID, WebhookStatus{
				ID: msg.WhatsAppMessageID, Status: statusValue,
			})
		}(statusValue)
	}
	close(start)
	wait.Wait()
	close(results)
	for err := range results {
		require.NoError(t, err)
	}

	var storedMessage models.Message
	var storedRecipient models.BulkMessageRecipient
	var storedCampaign models.BulkMessageCampaign
	require.NoError(t, app.DB.First(&storedMessage, msg.ID).Error)
	require.NoError(t, app.DB.First(&storedRecipient, recipient.ID).Error)
	require.NoError(t, app.DB.First(&storedCampaign, campaign.ID).Error)
	assert.Equal(t, models.MessageStatusRead, storedMessage.Status)
	assert.Equal(t, models.MessageStatusRead, storedRecipient.Status)
	assert.Equal(t, 1, storedCampaign.SentCount)
	assert.Equal(t, 1, storedCampaign.DeliveredCount)
	assert.Equal(t, 1, storedCampaign.ReadCount)
	assert.Zero(t, storedCampaign.FailedCount)
}

func TestUpdateMessageStatus_FailedStillOverridesRead(t *testing.T) {
	app := newProcessorTestApp(t)
	_, msg, campaign, recipient := webhookTestData(t, app, models.MessageStatusRead)
	require.NoError(t, app.DB.Model(&models.BulkMessageCampaign{}).
		Where("id = ?", campaign.ID).
		Updates(map[string]any{
			"sent_count": 1, "delivered_count": 1, "read_count": 1,
		}).Error)

	processWebhookMessageStatus(t, app, msg, "failed", []WebhookStatusError{{
		Message: "terminal provider failure after read",
	}})

	var storedMessage models.Message
	var storedRecipient models.BulkMessageRecipient
	var storedCampaign models.BulkMessageCampaign
	require.NoError(t, app.DB.First(&storedMessage, msg.ID).Error)
	require.NoError(t, app.DB.First(&storedRecipient, recipient.ID).Error)
	require.NoError(t, app.DB.First(&storedCampaign, campaign.ID).Error)
	assert.Equal(t, models.MessageStatusFailed, storedMessage.Status)
	assert.Equal(t, models.MessageStatusFailed, storedRecipient.Status)
	assert.Zero(t, storedCampaign.SentCount)
	assert.Zero(t, storedCampaign.DeliveredCount)
	assert.Zero(t, storedCampaign.ReadCount)
	assert.Equal(t, 1, storedCampaign.FailedCount)
}

func TestWebhookStatusACKsDeletedCampaignProjectionAfterSettlingMessage(t *testing.T) {
	app := newProcessorTestApp(t)
	organization, msg, campaign, recipient := webhookTestData(t, app, models.MessageStatusSent)
	var account models.WhatsAppAccount
	require.NoError(t, app.DB.Where("organization_id = ?", organization.ID).First(&account).Error)
	app.Config = &config.Config{WhatsApp: config.WhatsAppConfig{AppSecret: webhookTestAppSecret}}
	account.AppSecret = webhookTestAppSecret
	require.NoError(t, app.DB.Model(&models.WhatsAppAccount{}).
		Where("id = ?", account.ID).
		Update("app_secret", account.AppSecret).Error)
	app.InvalidateWhatsAppAccountCache(account.PhoneID)
	require.NoError(t, app.DB.Delete(&recipient).Error)
	require.NoError(t, app.DB.Delete(&campaign).Error)

	body, err := json.Marshal(map[string]any{
		"object": "whatsapp_business_account",
		"entry": []any{map[string]any{
			"id": account.BusinessID,
			"changes": []any{map[string]any{
				"field": "messages",
				"value": map[string]any{
					"messaging_product": "whatsapp",
					"metadata":          map[string]any{"phone_number_id": account.PhoneID},
					"statuses": []any{map[string]any{
						"id": msg.WhatsAppMessageID, "status": "delivered",
					}},
				},
			}},
		}},
	})
	require.NoError(t, err)
	sendSignedWebhook(t, app, body)
	sendSignedWebhook(t, app, body)

	var stored models.Message
	require.NoError(t, app.DB.First(&stored, msg.ID).Error)
	assert.Equal(t, models.MessageStatusDelivered, stored.Status)
	failure, ok := stored.Metadata[campaignStatusProjectionFailureMetadataKey].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, campaign.ID.String(), failure["campaign_id"])
	assert.Equal(t, string(models.MessageStatusDelivered), failure["status"])
}

func TestProcessStatusUpdateRepairsRenamedAccountProjection(t *testing.T) {
	app := newProcessorTestApp(t)
	_, message, _, _ := webhookTestData(t, app, models.MessageStatusSent)

	var account models.WhatsAppAccount
	require.NoError(t, app.DB.Where("organization_id = ?", message.OrganizationID).First(&account).Error)
	renamed := "renamed-status-account-" + uuid.NewString()[:8]
	require.NoError(t, app.DB.Model(&models.WhatsAppAccount{}).Where("id = ?", account.ID).Update("name", renamed).Error)

	processWebhookMessageStatus(t, app, message, "delivered", nil)

	var stored models.Message
	require.NoError(t, app.DB.First(&stored, message.ID).Error)
	assert.Equal(t, models.MessageStatusDelivered, stored.Status)
	assert.Equal(t, renamed, stored.WhatsAppAccount)
}

func TestProcessStatusUpdateRejectsProviderNeutralAndCrossTenantCollisions(t *testing.T) {
	app := newProcessorTestApp(t)
	_, owner, _, ownerRecipient := webhookTestData(t, app, models.MessageStatusSent)

	foreignOrganization := testutil.CreateTestOrganization(t, app.DB)
	foreignAccount := createStatusAuthority(t, app, foreignOrganization.ID, "foreign-status-account-"+uuid.NewString()[:8], "foreign-status-phone-"+uuid.NewString()[:8])
	foreignContact := testutil.CreateTestContact(t, app.DB, foreignOrganization.ID)
	// A provider-neutral duplicate has no deterministic or channel provenance.
	// The phone-bound receipt must leave it untouched even though its WAMID is
	// identical to the proven owner in the first tenant.
	foreign := models.Message{
		BaseModel:         models.BaseModel{ID: uuid.New()},
		OrganizationID:    foreignOrganization.ID,
		WhatsAppAccount:   foreignAccount.Name,
		ContactID:         foreignContact.ID,
		WhatsAppMessageID: owner.WhatsAppMessageID,
		Direction:         models.DirectionOutgoing,
		MessageType:       models.MessageTypeText,
		Content:           "provider-neutral collision",
		Status:            models.MessageStatusSent,
	}
	require.NoError(t, app.DB.Create(&foreign).Error)

	processWebhookMessageStatus(t, app, owner, "delivered", nil)

	var storedOwner models.Message
	var storedRecipient models.BulkMessageRecipient
	var storedForeign models.Message
	require.NoError(t, app.DB.First(&storedOwner, owner.ID).Error)
	require.NoError(t, app.DB.First(&storedRecipient, ownerRecipient.ID).Error)
	require.NoError(t, app.DB.First(&storedForeign, foreign.ID).Error)
	assert.Equal(t, models.MessageStatusDelivered, storedOwner.Status)
	assert.Equal(t, models.MessageStatusDelivered, storedRecipient.Status)
	assert.Equal(t, models.MessageStatusSent, storedForeign.Status)
}

func TestProcessStatusUpdateRejectsUnprovenSameTenantOwner(t *testing.T) {
	app := newProcessorTestApp(t)
	organization := testutil.CreateTestOrganization(t, app.DB)
	account := createStatusAuthority(
		t,
		app,
		organization.ID,
		"unproven-status-account-"+uuid.NewString()[:8],
		"unproven-status-phone-"+uuid.NewString()[:8],
	)
	contact := testutil.CreateTestContact(t, app.DB, organization.ID)
	wamid := "wamid.unproven-status-" + uuid.NewString()

	// A sole provider-neutral row is still not an authenticated owner. The
	// durable resolver must reject it rather than treating uniqueness alone as
	// account provenance.
	unproven := models.Message{
		BaseModel:         models.BaseModel{ID: uuid.New()},
		OrganizationID:    organization.ID,
		WhatsAppAccount:   account.Name,
		ContactID:         contact.ID,
		WhatsAppMessageID: wamid,
		Direction:         models.DirectionOutgoing,
		MessageType:       models.MessageTypeText,
		Content:           "unproven same-tenant owner",
		Status:            models.MessageStatusSent,
	}
	require.NoError(t, app.DB.Create(&unproven).Error)

	require.Error(t, app.processStatusUpdate(account.PhoneID, WebhookStatus{ID: wamid, Status: "delivered"}))

	var storedUnproven models.Message
	require.NoError(t, app.DB.First(&storedUnproven, unproven.ID).Error)
	assert.Equal(t, models.MessageStatusSent, storedUnproven.Status)
}

func TestProcessStatusUpdateRejectsCrossTenantPhoneCollision(t *testing.T) {
	app := newProcessorTestApp(t)
	organization, owner, _, recipient := webhookTestData(t, app, models.MessageStatusSent)
	var ownerAccount models.WhatsAppAccount
	require.NoError(t, app.DB.Where("organization_id = ?", organization.ID).First(&ownerAccount).Error)
	foreignOrganization := testutil.CreateTestOrganization(t, app.DB)
	duplicate := models.WhatsAppAccount{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: foreignOrganization.ID,
		Name:           "duplicate-phone-account-" + uuid.NewString()[:8],
		PhoneID:        ownerAccount.PhoneID,
		BusinessID:     "duplicate-phone-business-" + uuid.NewString()[:8],
		AccessToken:    "status-token",
	}
	// The global live-phone identity constraint rejects the collision before a
	// receipt can be routed through an ambiguous tenant authority.
	require.ErrorContains(t, app.DB.Create(&duplicate).Error, "uq_whatsapp_accounts_live_phone_id")

	processWebhookMessageStatus(t, app, owner, "delivered", nil)

	var storedOwner models.Message
	var storedRecipient models.BulkMessageRecipient
	require.NoError(t, app.DB.First(&storedOwner, owner.ID).Error)
	require.NoError(t, app.DB.First(&storedRecipient, recipient.ID).Error)
	assert.Equal(t, models.MessageStatusDelivered, storedOwner.Status)
	assert.Equal(t, models.MessageStatusDelivered, storedRecipient.Status)
}

func TestUpdateMessageStatus_FailedBroadcastsErrorMessageViaWebSocket(t *testing.T) {
	// Create app with a real WebSocket hub
	db := testutil.SetupTestDB(t)
	log := testutil.NopLogger()
	hub := websocket.NewHub(log)
	go hub.Run()

	app := &App{
		DB:    db,
		Log:   log,
		WSHub: hub,
	}

	uid := uuid.New().String()[:8]
	org := models.Organization{
		BaseModel: models.BaseModel{ID: uuid.New()},
		Name:      "ws-test-" + uid,
		Slug:      "ws-test-" + uid,
	}
	require.NoError(t, app.DB.Create(&org).Error)

	contact := models.Contact{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: org.ID,
		PhoneNumber:    "91777" + uid,
		ProfileName:    "WS Test",
	}
	require.NoError(t, app.DB.Create(&contact).Error)
	account := createStatusAuthority(t, app, org.ID, "ws-status-account-"+uid, "ws-status-phone-"+uid)

	waMsgID := "wamid.ws-test-" + uid
	msg := models.Message{
		BaseModel:         models.BaseModel{ID: uuid.NewSHA1(account.ID, []byte("coexistence-message:"+waMsgID))},
		OrganizationID:    org.ID,
		WhatsAppAccount:   account.Name,
		ContactID:         contact.ID,
		WhatsAppMessageID: waMsgID,
		Direction:         models.DirectionOutgoing,
		MessageType:       models.MessageTypeTemplate,
		Content:           "test message",
		Status:            models.MessageStatusSent,
		Metadata:          models.JSONB{database.WhatsAppWAMIDOwnerMetadataKey: true},
	}
	require.NoError(t, app.DB.Create(&msg).Error)

	// Register a WS client for this org
	userID := uuid.New()
	client := websocket.NewClient(hub, nil, userID, org.ID)
	hub.Register(client)

	// Wait for the client to be registered
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if hub.GetClientCount() == 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	require.Equal(t, 1, hub.GetClientCount())

	// Trigger a failed status update with error message
	errors := []WebhookStatusError{
		{Code: 131047, Title: "Re-engagement message", Message: "This message was not delivered to maintain healthy ecosystem engagement."},
	}
	processWebhookMessageStatus(t, app, msg, "failed", errors)

	// Read from the client's send channel and verify the WS broadcast
	select {
	case data := <-client.SendChan():
		var wsMsg websocket.WSMessage
		require.NoError(t, json.Unmarshal(data, &wsMsg))
		assert.Equal(t, websocket.TypeStatusUpdate, wsMsg.Type)

		payload, ok := wsMsg.Payload.(map[string]any)
		require.True(t, ok, "payload should be a map")
		assert.Equal(t, msg.ID.String(), payload["message_id"])
		assert.Equal(t, "failed", payload["status"])
		assert.Contains(t, payload["error_message"].(string), "healthy ecosystem engagement")
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for WebSocket broadcast")
	}
}

func TestUpdateMessageStatus_DeliveredBroadcastsViaWebSocket_NoErrorMessage(t *testing.T) {
	// Create app with a real WebSocket hub
	db := testutil.SetupTestDB(t)
	log := testutil.NopLogger()
	hub := websocket.NewHub(log)
	go hub.Run()

	app := &App{
		DB:    db,
		Log:   log,
		WSHub: hub,
	}

	uid := uuid.New().String()[:8]
	org := models.Organization{
		BaseModel: models.BaseModel{ID: uuid.New()},
		Name:      "ws-del-" + uid,
		Slug:      "ws-del-" + uid,
	}
	require.NoError(t, app.DB.Create(&org).Error)

	contact := models.Contact{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: org.ID,
		PhoneNumber:    "91888" + uid,
		ProfileName:    "WS Delivered",
	}
	require.NoError(t, app.DB.Create(&contact).Error)
	account := createStatusAuthority(t, app, org.ID, "ws-delivered-account-"+uid, "ws-delivered-phone-"+uid)

	waMsgID := "wamid.ws-del-" + uid
	msg := models.Message{
		BaseModel:         models.BaseModel{ID: uuid.NewSHA1(account.ID, []byte("coexistence-message:"+waMsgID))},
		OrganizationID:    org.ID,
		WhatsAppAccount:   account.Name,
		ContactID:         contact.ID,
		WhatsAppMessageID: waMsgID,
		Direction:         models.DirectionOutgoing,
		MessageType:       models.MessageTypeText,
		Content:           "hello",
		Status:            models.MessageStatusSent,
		Metadata:          models.JSONB{database.WhatsAppWAMIDOwnerMetadataKey: true},
	}
	require.NoError(t, app.DB.Create(&msg).Error)

	// Register a WS client for this org
	client := websocket.NewClient(hub, nil, uuid.New(), org.ID)
	hub.Register(client)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if hub.GetClientCount() == 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	require.Equal(t, 1, hub.GetClientCount())

	// Trigger a delivered status update (no errors)
	processWebhookMessageStatus(t, app, msg, "delivered", nil)

	// Read from the client's send channel and verify NO error_message
	select {
	case data := <-client.SendChan():
		var wsMsg websocket.WSMessage
		require.NoError(t, json.Unmarshal(data, &wsMsg))
		assert.Equal(t, websocket.TypeStatusUpdate, wsMsg.Type)

		payload, ok := wsMsg.Payload.(map[string]any)
		require.True(t, ok, "payload should be a map")
		assert.Equal(t, msg.ID.String(), payload["message_id"])
		assert.Equal(t, "delivered", payload["status"])
		_, hasError := payload["error_message"]
		assert.False(t, hasError, "delivered status should not have error_message")
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for WebSocket broadcast")
	}
}

func TestWebhookHandler_smb_message_echoes(t *testing.T) {
	app := webhookTestApp(t)

	// Create test org and account
	uid := uuid.New().String()[:8]
	org := models.Organization{
		BaseModel: models.BaseModel{ID: uuid.New()},
		Name:      "echo-org-" + uid,
		Slug:      "echo-org-" + uid,
	}
	require.NoError(t, app.DB.Create(&org).Error)

	waAccount := models.WhatsAppAccount{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: org.ID,
		Name:           "echo-acct-" + uid,
		PhoneID:        "phone-echo-" + uid,
		BusinessID:     "biz-echo-" + uid,
		AccessToken:    "token",
		Status:         "active",
		IsSMB:          true,
	}
	require.NoError(t, app.DB.Create(&waAccount).Error)

	// Construct message echo payload
	body := []byte(`{
		"object": "whatsapp_business_account",
		"entry": [{
			"id": "` + waAccount.BusinessID + `",
			"changes": [{
				"field": "smb_message_echoes",
				"value": {
					"messaging_product": "whatsapp",
					"metadata": {
						"display_phone_number": "15551234567",
						"phone_number_id": "` + waAccount.PhoneID + `"
					},
					"message_echoes": [{
						"from": "15551234567",
						"to": "9199998888",
						"id": "wamid.echo_test_12345",
						"timestamp": "1716152000",
						"type": "text",
						"text": {
							"body": "Hello from Business App!"
						}
					}]
				}
			}]
		}]
	}`)

	req := testutil.NewRequest(t)
	req.RequestCtx.Request.Header.SetMethod("POST")
	req.RequestCtx.Request.Header.SetContentType("application/json")
	req.RequestCtx.Request.Header.Set(
		"X-Hub-Signature-256",
		webhookTestSignature(body),
	)
	req.RequestCtx.Request.SetBody(body)

	// Execute WebhookHandler
	require.NoError(t, app.WebhookHandler(req))

	// Verify contact was created
	var contact models.Contact
	assert.Eventually(t, func() bool {
		return app.DB.Where("organization_id = ? AND phone_number = ?", org.ID, "9199998888").First(&contact).Error == nil
	}, 2*time.Second, 10*time.Millisecond)

	// Verify message was saved as outgoing and status sent
	var message models.Message
	assert.Eventually(t, func() bool {
		return app.DB.Where("organization_id = ? AND whats_app_message_id = ?", org.ID, "wamid.echo_test_12345").First(&message).Error == nil
	}, 2*time.Second, 10*time.Millisecond)
	assert.Equal(t, models.DirectionOutgoing, message.Direction)
	assert.Equal(t, models.MessageStatusSent, message.Status)
	assert.Equal(t, "Hello from Business App!", message.Content)
}

func TestWebhookHandler_smb_app_state_sync(t *testing.T) {
	app := webhookTestApp(t)

	// Create test org and account
	uid := uuid.New().String()[:8]
	org := models.Organization{
		BaseModel: models.BaseModel{ID: uuid.New()},
		Name:      "sync-org-" + uid,
		Slug:      "sync-org-" + uid,
	}
	require.NoError(t, app.DB.Create(&org).Error)

	waAccount := models.WhatsAppAccount{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: org.ID,
		Name:           "sync-acct-" + uid,
		PhoneID:        "phone-sync-" + uid,
		BusinessID:     "biz-sync-" + uid,
		AccessToken:    "token",
		Status:         "active",
		IsSMB:          true,
	}
	require.NoError(t, app.DB.Create(&waAccount).Error)

	// 1. Sync contact (ADD)
	bodyAdd := []byte(`{
		"object": "whatsapp_business_account",
		"entry": [{
			"id": "` + waAccount.BusinessID + `",
			"changes": [{
				"field": "smb_app_state_sync",
				"value": {
					"messaging_product": "whatsapp",
					"metadata": {
						"display_phone_number": "15551234567",
						"phone_number_id": "` + waAccount.PhoneID + `"
					},
					"state_sync": [{
						"type": "contact",
						"contact": {
							"full_name": "Synced Contact Name",
							"first_name": "Synced",
							"phone_number": "9199997777"
						},
						"action": "add",
						"metadata": {"timestamp": "1738346006"}
					}]
				}
			}]
		}]
	}`)

	req := testutil.NewRequest(t)
	req.RequestCtx.Request.Header.SetMethod("POST")
	req.RequestCtx.Request.Header.SetContentType("application/json")
	req.RequestCtx.Request.Header.Set(
		"X-Hub-Signature-256",
		webhookTestSignature(bodyAdd),
	)
	req.RequestCtx.Request.SetBody(bodyAdd)

	require.NoError(t, app.WebhookHandler(req))

	// Verify contact was synced (add)
	var contact models.Contact
	assert.Eventually(t, func() bool {
		return app.DB.Where("organization_id = ? AND phone_number = ?", org.ID, "9199997777").First(&contact).Error == nil
	}, 2*time.Second, 10*time.Millisecond)
	assert.Equal(t, "Synced Contact Name", contact.ProfileName)

	var syncState models.WhatsAppCoexistenceState
	require.NoError(t, app.DB.Where(
		"organization_id = ? AND whats_app_account_id = ?",
		org.ID,
		waAccount.ID,
	).First(&syncState).Error)
	assert.Equal(t, models.CoexistenceSyncStatusNotRequested, syncState.ContactSyncStatus)
	assert.Nil(t, syncState.ContactSyncCompletedAt)

	// 2. Sync contact (REMOVE)
	bodyRemove := []byte(`{
		"object": "whatsapp_business_account",
		"entry": [{
			"id": "` + waAccount.BusinessID + `",
			"changes": [{
				"field": "smb_app_state_sync",
				"value": {
					"messaging_product": "whatsapp",
					"metadata": {
						"display_phone_number": "15551234567",
						"phone_number_id": "` + waAccount.PhoneID + `"
					},
					"state_sync": [{
						"type": "contact",
						"contact": {"phone_number": "9199997777"},
						"action": "remove",
						"metadata": {"timestamp": "1738346020"}
					}]
				}
			}]
		}]
	}`)

	reqRemove := testutil.NewRequest(t)
	reqRemove.RequestCtx.Request.Header.SetMethod("POST")
	reqRemove.RequestCtx.Request.Header.SetContentType("application/json")
	reqRemove.RequestCtx.Request.Header.Set(
		"X-Hub-Signature-256",
		webhookTestSignature(bodyRemove),
	)
	reqRemove.RequestCtx.Request.SetBody(bodyRemove)

	require.NoError(t, app.WebhookHandler(reqRemove))

	// Removing a phone address-book entry must preserve the CRM contact and
	// its conversation identity; only the app-contact membership is cleared.
	var preserved models.Contact
	require.NoError(t, app.DB.Where(
		"organization_id = ? AND phone_number = ?",
		org.ID,
		"9199997777",
	).First(&preserved).Error)
	assert.False(t, preserved.DeletedAt.Valid)
	assert.Equal(t, false, preserved.Metadata["coexistence_app_contact"])
	assert.NotEmpty(t, preserved.Metadata["coexistence_app_contact_removed_at"])

	// A normal contact delta must not repair or complete a failed one-time
	// request: this webhook field has no request correlation or terminal marker.
	require.NoError(t, app.DB.Model(&models.WhatsAppCoexistenceState{}).Where(
		"organization_id = ? AND whats_app_account_id = ?",
		org.ID,
		waAccount.ID,
	).Updates(map[string]any{
		"contact_sync_status":     models.CoexistenceSyncStatusFailed,
		"contact_sync_error_code": "synthetic_failure",
	}).Error)
	sendSignedWebhook(t, app, bodyAdd)
	require.NoError(t, app.DB.Where(
		"organization_id = ? AND whats_app_account_id = ?",
		org.ID,
		waAccount.ID,
	).First(&syncState).Error)
	assert.Equal(t, models.CoexistenceSyncStatusFailed, syncState.ContactSyncStatus)
	assert.Equal(t, "synthetic_failure", syncState.ContactSyncErrorCode)
	assert.Nil(t, syncState.ContactSyncCompletedAt)
}

func TestWebhookHandler_CoexistenceFieldsIgnoreClassicAccount(t *testing.T) {
	app := webhookTestApp(t)
	uid := uuid.NewString()[:8]
	org := models.Organization{
		BaseModel: models.BaseModel{ID: uuid.New()},
		Name:      "classic-coexistence-org-" + uid,
		Slug:      "classic-coexistence-org-" + uid,
	}
	require.NoError(t, app.DB.Create(&org).Error)
	account := models.WhatsAppAccount{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: org.ID,
		Name:           "classic-coexistence-account-" + uid,
		PhoneID:        "classic-coexistence-phone-" + uid,
		BusinessID:     "classic-coexistence-waba-" + uid,
		AccessToken:    "token",
		Status:         "active",
		IsSMB:          false,
	}
	require.NoError(t, app.DB.Create(&account).Error)

	body := []byte(`{
		"object":"whatsapp_business_account",
		"entry":[{"id":"` + account.BusinessID + `","changes":[
			{"field":"smb_app_state_sync","value":{
				"metadata":{"phone_number_id":"` + account.PhoneID + `"},
				"state_sync":[{"type":"contact","action":"add",
					"contact":{"full_name":"Must be ignored","phone_number":"60119990000"},
					"metadata":{"timestamp":"1738346006"}}]
			}},
			{"field":"smb_message_echoes","value":{
				"metadata":{"phone_number_id":"` + account.PhoneID + `"},
				"message_echoes":[{"from":"15551234567","to":"60119990000",
					"id":"wamid.classic.ignore-` + uid + `","timestamp":"1738346006",
					"type":"text","text":{"body":"Must be ignored"}}]
			}}
		]}]
	}`)
	sendSignedWebhook(t, app, body)

	var contactCount, messageCount, stateCount int64
	require.NoError(t, app.DB.Model(&models.Contact{}).Where("organization_id = ?", org.ID).Count(&contactCount).Error)
	require.NoError(t, app.DB.Model(&models.Message{}).Where("organization_id = ?", org.ID).Count(&messageCount).Error)
	require.NoError(t, app.DB.Model(&models.WhatsAppCoexistenceState{}).Where(
		"organization_id = ? AND whats_app_account_id = ?",
		org.ID,
		account.ID,
	).Count(&stateCount).Error)
	assert.Zero(t, contactCount)
	assert.Zero(t, messageCount)
	assert.Zero(t, stateCount)
}

func TestWebhookPayload_OfficialCoexistenceShapes(t *testing.T) {
	t.Parallel()
	body := []byte(`{
		"object":"whatsapp_business_account",
		"entry":[{
			"id":"waba-1",
			"time":1768477203,
			"changes":[
				{"field":"smb_app_state_sync","value":{
					"metadata":{"display_phone_number":"15550783881","phone_number_id":"phone-1"},
					"state_sync":[{"type":"contact","contact":{"full_name":"Pablo Morales","first_name":"Pablo","phone_number":"16505551234"},"action":"add","metadata":{"timestamp":"1738346006"}}]
				}},
				{"field":"smb_message_echoes","value":{
					"metadata":{"display_phone_number":"15550783881","phone_number_id":"phone-1"},
					"message_echoes":[{"from":"15550783881","to":"16505551234","id":"wamid.echo","timestamp":"1700255121","type":"text","text":{"body":"hello"}}]
				}},
				{"field":"history","value":{
					"metadata":{"display_phone_number":"15550783881","phone_number_id":"phone-1"},
					"history":[{"metadata":{"phase":1,"chunk_order":7,"progress":55},"threads":[{"id":"16505551234","messages":[{"from":"16505551234","id":"wamid.history","timestamp":"1739230970","type":"text","text":{"body":"Thanks!"},"history_context":{"status":"READ"}}]}]}]
				}},
				{"field":"account_update","value":{"phone_number":"15550783881","event":"PARTNER_REMOVED","disconnection_info":{"reason":"PRIMARY_INACTIVITY","initiated_by":"SYSTEM"}}}
			]
		}]
	}`)

	var payload WebhookPayload
	require.NoError(t, json.Unmarshal(body, &payload))
	require.Len(t, payload.Entry, 1)
	assert.Equal(t, int64(1768477203), payload.Entry[0].Time)
	require.Len(t, payload.Entry[0].Changes, 4)

	stateSync := payload.Entry[0].Changes[0].Value.StateSync
	require.Len(t, stateSync, 1)
	assert.Equal(t, "Pablo Morales", stateSync[0].Contact.FullName)
	assert.Equal(t, "16505551234", stateSync[0].Contact.PhoneNumber)
	assert.Equal(t, "add", stateSync[0].Action)

	echoes := payload.Entry[0].Changes[1].Value.MessageEchoes
	require.Len(t, echoes, 1)
	assert.Equal(t, "15550783881", echoes[0].From)
	assert.Equal(t, "16505551234", echoes[0].To)
	require.NotNil(t, echoes[0].Text)
	assert.Equal(t, "hello", echoes[0].Text.Body)

	history := payload.Entry[0].Changes[2].Value.History
	require.Len(t, history, 1)
	assert.Equal(t, 1, history[0].Metadata.Phase)
	assert.Equal(t, 7, history[0].Metadata.ChunkOrder)
	assert.Equal(t, 55, history[0].Metadata.Progress)
	require.Len(t, history[0].Threads, 1)
	require.Len(t, history[0].Threads[0].Messages, 1)
	assert.Equal(t, "wamid.history", history[0].Threads[0].Messages[0].ID)
	require.NotNil(t, history[0].Threads[0].Messages[0].HistoryContext)
	assert.Equal(t, "READ", history[0].Threads[0].Messages[0].HistoryContext.Status)

	lifecycle := payload.Entry[0].Changes[3].Value
	assert.Equal(t, "PARTNER_REMOVED", lifecycle.Event)
	assert.Equal(t, "15550783881", lifecycle.PhoneNumber)
	require.NotNil(t, lifecycle.DisconnectionInfo)
	assert.Equal(t, "PRIMARY_INACTIVITY", lifecycle.DisconnectionInfo.Reason)
	assert.Equal(t, "SYSTEM", lifecycle.DisconnectionInfo.InitiatedBy)
}

func TestWebhookPayload_BSUIDOnlyCoexistenceShapes(t *testing.T) {
	t.Parallel()
	body := []byte(`{
		"object":"whatsapp_business_account",
		"entry":[{"id":"waba-1","changes":[
			{"field":"smb_app_state_sync","value":{
				"state_sync":[{"type":"contact","contact":{
					"full_name":"Username Customer","user_id":"bsuid-contact-1",
					"parent_user_id":"parent-contact-1","username":"customer_handle"
				},"action":"add"}]
			}},
			{"field":"smb_message_echoes","value":{
				"contacts":[{"profile":{"name":"Echo Customer","username":"echo_handle"},
					"user_id":"bsuid-echo-1","parent_user_id":"parent-echo-1"}],
				"message_echoes":[{"from":"15550783881","to_user_id":"bsuid-echo-1",
					"to_parent_user_id":"parent-echo-1","id":"wamid.echo.bsuid","timestamp":"1700255121",
					"type":"text","text":{"body":"hello"}}]
			}},
			{"field":"history","value":{
				"history":[{"metadata":{"phase":0,"chunk_order":1,"progress":10},"threads":[{
					"context":{"user_id":"bsuid-history-1","parent_user_id":"parent-history-1","username":"history_handle"},
					"messages":[{"from_user_id":"bsuid-history-1","from_parent_user_id":"parent-history-1",
						"id":"wamid.history.bsuid","timestamp":"1739230970","type":"text",
						"text":{"body":"Thanks!"},"history_context":{"status":"READ"}}]
				}]}]
			}}
		]}]
	}`)

	var payload WebhookPayload
	require.NoError(t, json.Unmarshal(body, &payload))
	require.Len(t, payload.Entry, 1)
	require.Len(t, payload.Entry[0].Changes, 3)

	stateContact := payload.Entry[0].Changes[0].Value.StateSync[0].Contact
	assert.Empty(t, stateContact.PhoneNumber)
	assert.Equal(t, "bsuid-contact-1", stateContact.UserID)
	assert.Equal(t, "parent-contact-1", stateContact.ParentUserID)
	assert.Equal(t, "customer_handle", stateContact.Username)

	echoValue := payload.Entry[0].Changes[1].Value
	require.Len(t, echoValue.Contacts, 1)
	assert.Equal(t, "echo_handle", echoValue.Contacts[0].Profile.Username)
	assert.Equal(t, "parent-echo-1", echoValue.Contacts[0].ParentUserID)
	require.Len(t, echoValue.MessageEchoes, 1)
	assert.Empty(t, echoValue.MessageEchoes[0].To)
	assert.Equal(t, "bsuid-echo-1", echoValue.MessageEchoes[0].ToUserID)
	assert.Equal(t, "parent-echo-1", echoValue.MessageEchoes[0].ToParentUserID)

	historyThread := payload.Entry[0].Changes[2].Value.History[0].Threads[0]
	assert.Empty(t, historyThread.ID)
	assert.Equal(t, "bsuid-history-1", historyThread.Context.UserID)
	assert.Equal(t, "history_handle", historyThread.Context.Username)
	require.Len(t, historyThread.Messages, 1)
	assert.Equal(t, "bsuid-history-1", historyThread.Messages[0].FromUserID)
	assert.Equal(t, "parent-history-1", historyThread.Messages[0].FromParentUserID)
}

func TestWebhookPayload_MessageFieldParsesEditAndRevoke(t *testing.T) {
	t.Parallel()
	body := []byte(`{
		"object":"whatsapp_business_account",
		"entry":[{"id":"waba-1","changes":[{"field":"messages","value":{
			"metadata":{"phone_number_id":"phone-1"},
			"messages":[
				{"from":"16505551234","id":"wamid.edit.event","timestamp":"1739231000","type":"edit",
					"edit":{"original_message_id":"wamid.original","message":{"type":"text","text":{"body":"corrected"}}}},
				{"from_user_id":"US.customer","id":"wamid.revoke.event","timestamp":"1739231010","type":"revoke",
					"revoke":{"original_message_id":"wamid.original"}}
			]
		}}]}]
	}`)

	var payload WebhookPayload
	require.NoError(t, json.Unmarshal(body, &payload))
	messages := payload.Entry[0].Changes[0].Value.Messages
	require.Len(t, messages, 2)
	require.NotNil(t, messages[0].Edit)
	assert.Equal(t, "wamid.original", messages[0].Edit.OriginalMessageID)
	require.NotNil(t, messages[0].Edit.Message.Text)
	assert.Equal(t, "corrected", messages[0].Edit.Message.Text.Body)
	require.NotNil(t, messages[1].Revoke)
	assert.Equal(t, "wamid.original", messages[1].Revoke.OriginalMessageID)
	assert.Equal(t, "US.customer", messages[1].FromUserID)
}

func TestCoexistenceBSUIDPlaceholderAndDirection(t *testing.T) {
	t.Parallel()
	identity := coexistenceContactIdentity{UserID: "US.business-scoped-user-123"}
	placeholder := coexistenceIdentityPlaceholder(identity)
	assert.Equal(t, placeholder, coexistenceIdentityPlaceholder(identity))
	assert.True(t, strings.HasPrefix(placeholder, "bsuid:"))
	assert.LessOrEqual(t, len(placeholder), 50)
	assert.NotEqual(t, placeholder, coexistenceIdentityPlaceholder(coexistenceContactIdentity{UserID: "US.other"}))

	outgoing := CoexistenceMessage{}
	outgoing.ToUserID = "US.recipient"
	assert.Equal(t, models.DirectionOutgoing, coexistenceEchoDirection(outgoing))

	thread := CoexistenceHistoryThread{}
	thread.Context.UserID = "US.customer"
	incoming := CoexistenceMessage{}
	incoming.FromUserID = "US.customer"
	assert.Equal(t, models.DirectionIncoming, coexistenceHistoryDirection("15550001111", thread, incoming))

	historyOutgoing := CoexistenceMessage{}
	historyOutgoing.ToUserID = "US.customer"
	assert.Equal(t, models.DirectionOutgoing, coexistenceHistoryDirection("15550001111", thread, historyOutgoing))
}

func TestCoexistenceBSUIDPersistenceAndPhoneReconciliation(t *testing.T) {
	db := testutil.SetupTestDB(t)
	app := &App{DB: db, Log: testutil.NopLogger()}
	uid := uuid.NewString()[:8]
	org := models.Organization{
		BaseModel: models.BaseModel{ID: uuid.New()},
		Name:      "bsuid-org-" + uid,
		Slug:      "bsuid-org-" + uid,
	}
	require.NoError(t, db.Create(&org).Error)
	account := models.WhatsAppAccount{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: org.ID,
		Name:           "bsuid-acct-" + uid,
		PhoneID:        "bsuid-phone-" + uid,
		BusinessID:     "bsuid-waba-" + uid,
		AccessToken:    "token",
		Status:         "active",
		IsSMB:          true,
	}
	require.NoError(t, db.Create(&account).Error)

	identity := coexistenceContactIdentity{
		UserID:       "US.customer-" + uid,
		ParentUserID: "US.parent-" + uid,
		Username:     "customer_" + uid,
		ProfileName:  "BSUID Customer",
	}
	placeholder, created, err := app.getOrCreateCoexistenceContact(&account, identity)
	require.NoError(t, err)
	require.True(t, created)
	assert.True(t, strings.HasPrefix(placeholder.PhoneNumber, "bsuid:"))
	assert.LessOrEqual(t, len(placeholder.PhoneNumber), 50)
	assert.Equal(t, identity.UserID, placeholder.BSUID)
	assert.Equal(t, identity.UserID, placeholder.Metadata["coexistence_user_id"])
	assert.Equal(t, identity.ParentUserID, placeholder.Metadata["coexistence_parent_user_id"])
	assert.Equal(t, identity.Username, placeholder.Metadata["coexistence_username"])
	assert.Equal(t, true, placeholder.Metadata["coexistence_phone_unavailable"])

	echo := CoexistenceMessage{}
	echo.ID = "wamid.bsuid.echo-" + uid
	echo.Timestamp = "1739230955"
	echo.Type = "text"
	echo.ToUserID = identity.UserID
	echo.ToParentUserID = identity.ParentUserID
	echo.Text = &struct {
		Body string `json:"body"`
	}{Body: "sent from the Business App"}
	var persistedEcho *models.Message
	var inserted bool
	err = app.WithCommittedTenantApp(org.ID, func(scoped *App) error {
		var persistErr error
		persistedEcho, inserted, persistErr = scoped.persistCoexistenceMessage(
			&account, identity, echo, models.DirectionOutgoing, models.MessageStatusSent,
			models.JSONB{"coexistence_source": "smb_message_echoes"}, false,
		)
		return persistErr
	})
	require.NoError(t, err)
	assert.True(t, inserted)
	assert.Equal(t, placeholder.ID, persistedEcho.ContactID)
	assert.Equal(t, models.DirectionOutgoing, persistedEcho.Direction)

	history := CoexistenceMessage{}
	history.ID = "wamid.bsuid.history-" + uid
	history.Timestamp = "1739230970"
	history.Type = "text"
	history.FromUserID = identity.UserID
	history.FromParentUserID = identity.ParentUserID
	history.Text = &struct {
		Body string `json:"body"`
	}{Body: "sent by the customer"}
	var persistedHistory *models.Message
	err = app.WithCommittedTenantApp(org.ID, func(scoped *App) error {
		var persistErr error
		persistedHistory, inserted, persistErr = scoped.persistCoexistenceMessage(
			&account, identity, history, models.DirectionIncoming, models.MessageStatusRead,
			models.JSONB{"coexistence_source": "history"}, false,
		)
		return persistErr
	})
	require.NoError(t, err)
	assert.True(t, inserted)
	assert.Equal(t, placeholder.ID, persistedHistory.ContactID)
	assert.Equal(t, models.DirectionIncoming, persistedHistory.Direction)

	realPhone := "6012" + uid
	withPhone := identity
	withPhone.Phone = realPhone
	reconciled, created, err := app.getOrCreateCoexistenceContact(&account, withPhone)
	require.NoError(t, err)
	assert.False(t, created)
	assert.Equal(t, placeholder.ID, reconciled.ID)
	assert.Equal(t, realPhone, reconciled.PhoneNumber)

	var stored models.Contact
	require.NoError(t, db.First(&stored, placeholder.ID).Error)
	assert.Equal(t, realPhone, stored.PhoneNumber)
	assert.Equal(t, identity.UserID, stored.BSUID)
	assert.Equal(t, false, stored.Metadata["coexistence_phone_unavailable"])

	ownerPhone := "6013" + uid
	owner := models.Contact{
		BaseModel:       models.BaseModel{ID: uuid.New()},
		OrganizationID:  org.ID,
		PhoneNumber:     ownerPhone,
		ProfileName:     "Existing Canonical",
		WhatsAppAccount: account.Name,
		BSUID:           "US.existing-owner-" + uid,
		Metadata:        models.JSONB{},
	}
	require.NoError(t, db.Create(&owner).Error)
	conflictingIdentity := coexistenceContactIdentity{
		UserID:      "US.conflict-" + uid,
		Username:    "conflict_" + uid,
		ProfileName: "Conflicting BSUID",
	}
	conflictingPlaceholder, _, err := app.getOrCreateCoexistenceContact(&account, conflictingIdentity)
	require.NoError(t, err)
	conflictingIdentity.Phone = ownerPhone
	resolved, _, err := app.getOrCreateCoexistenceContact(&account, conflictingIdentity)
	require.NoError(t, err)
	assert.Equal(t, conflictingPlaceholder.ID, resolved.ID,
		"a phone owned by a different BSUID must not replace that canonical contact")

	require.NoError(t, db.First(&owner, owner.ID).Error)
	assert.Equal(t, ownerPhone, owner.PhoneNumber)
	assert.Equal(t, "US.existing-owner-"+uid, owner.BSUID)
	stored = models.Contact{}
	require.NoError(t, db.First(&stored, conflictingPlaceholder.ID).Error)
	assert.True(t, strings.HasPrefix(stored.PhoneNumber, "bsuid:"))
	assert.Equal(t, conflictingIdentity.UserID, stored.BSUID)
	assert.Equal(t, true, stored.Metadata["coexistence_phone_conflict"])
	assert.Equal(t, owner.ID.String(), stored.Metadata["coexistence_phone_owner_contact_id"])
}

func mirroredCoexistenceEchoFixture(t *testing.T) (*App, models.WhatsAppAccount, models.Message, []byte, *atomic.Int32) {
	t.Helper()
	app := webhookTestApp(t)
	organization := testutil.CreateTestOrganization(t, app.DB)
	account := models.WhatsAppAccount{
		BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: organization.ID,
		Name: "mirrored-echo-" + uuid.NewString()[:8], PhoneID: testutil.NewTestGraphObjectID(),
		BusinessID: testutil.NewTestGraphObjectID(), AccessToken: "synthetic-echo-token", Status: "active", IsSMB: true,
	}
	require.NoError(t, app.DB.Create(&account).Error)
	count := &atomic.Int32{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload OutboundWebhookPayload
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil || payload.Event != "message.outgoing" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		count.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)
	app.HTTPClient = testutil.NewHTTPSRewriteClient(t, map[string]*httptest.Server{"https://echo-webhook.example.com": server})
	require.NoError(t, app.DB.Create(&models.Webhook{
		BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: organization.ID,
		Name: "echo-delivery-counter", URL: "https://echo-webhook.example.com/events",
		Events: models.StringArray{"message.outgoing"}, IsActive: true,
	}).Error)
	wamid := "wamid.mirrored-echo-" + uuid.NewString()
	body := []byte(fmt.Sprintf(`{"object":"whatsapp_business_account","entry":[{"id":%q,"changes":[{"field":"smb_message_echoes","value":{"metadata":{"phone_number_id":%q},"message_echoes":[{"id":%q,"from":"15550783881","to":"60123456789","timestamp":"1739230955","type":"text","text":{"body":"mirrored app echo"}}]}}]}]}`, account.BusinessID, account.PhoneID, wamid))
	sendSignedWebhook(t, app, body)
	app.WaitForBackgroundTasks()
	require.EqualValues(t, 1, count.Load(), "the first actual echo must publish one outgoing event")
	var message models.Message
	require.NoError(t, app.DB.Where("id = ? AND organization_id = ?", uuid.NewSHA1(account.ID, []byte("coexistence-message:"+wamid)), organization.ID).First(&message).Error)
	// A successful ACK alone is insufficient: the best-effort bridge can fail.
	// Assert the actual callback linked this same row and created the full chain.
	require.NotNil(t, message.InboxConversationID, "actual after-commit legacy mirror must succeed before replay")
	var conversation models.InboxConversation
	require.NoError(t, app.DB.First(&conversation, *message.InboxConversationID).Error)
	require.Equal(t, message.ContactID, conversation.ContactID)
	require.Equal(t, organization.ID, conversation.OrganizationID)
	require.NotNil(t, conversation.ContactIdentityID)
	var shadow models.ChannelAccount
	require.NoError(t, app.DB.First(&shadow, conversation.ChannelAccountID).Error)
	boundAccount, err := channelapi.LegacyMetaWhatsAppAccountID(&shadow)
	require.NoError(t, err)
	require.Equal(t, account.ID, boundAccount)
	return app, account, message, body, count
}

func TestWebhookHandler_CoexistenceEchoReplayAfterSuccessfulMirror(t *testing.T) {
	app, account, original, body, count := mirroredCoexistenceEchoFixture(t)
	sendSignedWebhook(t, app, body)
	app.WaitForBackgroundTasks()
	var messages []models.Message
	require.NoError(t, app.DB.Where("organization_id = ? AND whats_app_message_id = ?", account.OrganizationID, original.WhatsAppMessageID).Find(&messages).Error)
	require.Len(t, messages, 1)
	assert.Equal(t, original.ID, messages[0].ID)
	assert.Equal(t, original.ContactID, messages[0].ContactID)
	assert.Equal(t, original.InboxConversationID, messages[0].InboxConversationID)
	assert.Equal(t, models.DirectionOutgoing, messages[0].Direction)
	assert.Equal(t, original.Content, messages[0].Content)
	assert.EqualValues(t, 1, count.Load(), "an identical mirrored replay must not publish another outgoing event")
}

func TestWebhookHandler_CoexistenceMirroredReplayRejectsOwnershipMismatch(t *testing.T) {
	for _, mismatch := range []string{"payload_contact", "conflicting_account_provenance", "message_direction", "channel_binding", "channel_provider", "conversation_binding"} {
		t.Run(mismatch, func(t *testing.T) {
			app, account, original, body, count := mirroredCoexistenceEchoFixture(t)
			var conversation models.InboxConversation
			require.NoError(t, app.DB.First(&conversation, *original.InboxConversationID).Error)
			switch mismatch {
			case "payload_contact":
				body = []byte(strings.ReplaceAll(string(body), "60123456789", "60129876543"))
			case "conflicting_account_provenance":
				other := testutil.CreateTestWhatsAppAccount(t, app.DB, account.OrganizationID)
				createWhatsAppIdentityActivity(t, app.DB, other, &original)
			case "message_direction":
				require.NoError(t, app.DB.Model(&models.Message{}).Where("id = ?", original.ID).Update("direction", models.DirectionIncoming).Error)
			case "channel_binding":
				require.NoError(t, app.DB.Model(&models.ChannelAccount{}).Where("id = ?", conversation.ChannelAccountID).Update("metadata", models.JSONB{"legacy_account_id": uuid.NewString()}).Error)
			case "channel_provider":
				require.NoError(t, app.DB.Model(&models.ChannelAccount{}).Where("id = ?", conversation.ChannelAccountID).Update("provider", "unrelated-provider").Error)
			case "conversation_binding":
				require.NoError(t, app.DB.Model(&models.InboxConversation{}).Where("id = ?", conversation.ID).Update("external_conversation_id", "unrelated-conversation").Error)
			}
			var before models.Message
			require.NoError(t, app.DB.First(&before, original.ID).Error)
			req := testutil.NewRequest(t)
			req.RequestCtx.Request.Header.SetMethod("POST")
			req.RequestCtx.Request.Header.SetContentType("application/json")
			req.RequestCtx.Request.Header.Set("X-Hub-Signature-256", webhookTestSignature(body))
			req.RequestCtx.Request.SetBody(body)
			require.NoError(t, app.WebhookHandler(req))
			assert.Equal(t, http.StatusServiceUnavailable, req.RequestCtx.Response.StatusCode(), string(req.RequestCtx.Response.Body()))
			app.WaitForBackgroundTasks()
			var after models.Message
			require.NoError(t, app.DB.First(&after, original.ID).Error)
			assert.Equal(t, before, after, "a mismatched linked replay must not mutate the message")
			var messageCount int64
			require.NoError(t, app.DB.Model(&models.Message{}).Where("organization_id = ? AND whats_app_message_id = ?", account.OrganizationID, original.WhatsAppMessageID).Count(&messageCount).Error)
			assert.EqualValues(t, 1, messageCount)
			assert.EqualValues(t, 1, count.Load(), "ownership failures must not emit another outgoing event")
		})
	}
}

func TestWebhookHandler_CoexistenceBSUIDOnlyPersistsBeforeACK(t *testing.T) {
	app := webhookTestApp(t)
	uid := uuid.NewString()[:8]
	org := models.Organization{
		BaseModel: models.BaseModel{ID: uuid.New()},
		Name:      "bsuid-hook-org-" + uid,
		Slug:      "bsuid-hook-org-" + uid,
	}
	require.NoError(t, app.DB.Create(&org).Error)
	account := models.WhatsAppAccount{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: org.ID,
		Name:           "bsuid-hook-acct-" + uid,
		PhoneID:        "bsuid-hook-phone-" + uid,
		BusinessID:     "bsuid-hook-waba-" + uid,
		AccessToken:    "token",
		Status:         "active",
		IsSMB:          true,
	}
	require.NoError(t, app.DB.Create(&account).Error)
	require.NoError(t, app.DB.Create(&models.WhatsAppCoexistenceState{
		ID:                uuid.New(),
		OrganizationID:    org.ID,
		WhatsAppAccountID: account.ID,
		OnboardingStatus:  models.CoexistenceOnboardingStatusConnected,
		OnboardingCycle:   1,
		LifecycleStatus:   models.CoexistenceLifecycleStatusConnected,
		LifecycleMetadata: models.JSONB{},
		Version:           1,
	}).Error)
	userID := "US.webhook-customer-" + uid
	parentUserID := "US.webhook-parent-" + uid
	body := []byte(`{
		"object":"whatsapp_business_account",
		"entry":[{"id":"` + account.BusinessID + `","changes":[
			{"field":"smb_app_state_sync","value":{
				"metadata":{"phone_number_id":"` + account.PhoneID + `"},
				"state_sync":[{"type":"contact","contact":{"full_name":"BSUID-only Customer",
					"user_id":"` + userID + `","parent_user_id":"` + parentUserID + `","username":"bsuid_only"},
					"action":"add","metadata":{"timestamp":"1739230900"}}]
			}},
			{"field":"smb_message_echoes","value":{
				"metadata":{"display_phone_number":"15550783881","phone_number_id":"` + account.PhoneID + `"},
				"contacts":[{"profile":{"name":"BSUID-only Customer","username":"bsuid_only"},
					"user_id":"` + userID + `","parent_user_id":"` + parentUserID + `"}],
				"message_echoes":[{"from":"15550783881","to_user_id":"` + userID + `",
					"to_parent_user_id":"` + parentUserID + `","id":"wamid.bsuid.hook.echo-` + uid + `",
					"timestamp":"1739230955","type":"text","text":{"body":"hello from app"}}]
			}},
			{"field":"history","value":{
				"metadata":{"display_phone_number":"15550783881","phone_number_id":"` + account.PhoneID + `"},
				"history":[{"metadata":{"phase":0,"chunk_order":1,"progress":10},"threads":[{
					"context":{"user_id":"` + userID + `","parent_user_id":"` + parentUserID + `","username":"bsuid_only"},
					"messages":[{"from_user_id":"` + userID + `","from_parent_user_id":"` + parentUserID + `",
						"id":"wamid.bsuid.hook.history-` + uid + `","timestamp":"1739230970","type":"text",
						"text":{"body":"hello from customer"},"history_context":{"status":"READ"}}]
				}]}]
			}},
			{"field":"messages","value":{
				"messaging_product":"whatsapp",
				"metadata":{"display_phone_number":"15550783881","phone_number_id":"` + account.PhoneID + `"},
				"contacts":[{"profile":{"name":"BSUID-only Customer","username":"bsuid_only"},
					"user_id":"` + userID + `","parent_user_id":"` + parentUserID + `"}],
				"messages":[{"from_user_id":"` + userID + `","from_parent_user_id":"` + parentUserID + `",
					"id":"wamid.bsuid.hook.live-` + uid + `","timestamp":"1739230980","type":"text",
					"text":{"body":"live username message"}}]
			}}
		]}]
	}`)

	sendSignedWebhook(t, app, body)

	var contact models.Contact
	require.NoError(t, app.DB.Where("organization_id = ? AND bs_uid = ?", org.ID, userID).First(&contact).Error)
	assert.True(t, strings.HasPrefix(contact.PhoneNumber, "bsuid:"))
	assert.LessOrEqual(t, len(contact.PhoneNumber), 50)
	assert.Equal(t, "bsuid_only", contact.Metadata["coexistence_username"])

	var messages []models.Message
	require.NoError(t, app.DB.Where(
		"organization_id = ? AND whats_app_account = ?",
		org.ID,
		account.Name,
	).Order("created_at ASC").Find(&messages).Error)
	require.Len(t, messages, 3)
	assert.Equal(t, contact.ID, messages[0].ContactID)
	assert.Equal(t, contact.ID, messages[1].ContactID)
	assert.Equal(t, contact.ID, messages[2].ContactID)
	assert.Equal(t, models.DirectionOutgoing, messages[0].Direction)
	assert.Equal(t, models.DirectionIncoming, messages[1].Direction)
	assert.Equal(t, models.DirectionIncoming, messages[2].Direction)
}

func TestWebhookHandler_MessageFieldEditAndRevokePersistBeforeACK(t *testing.T) {
	app := webhookTestApp(t)
	uid := uuid.NewString()[:8]
	org := models.Organization{
		BaseModel: models.BaseModel{ID: uuid.New()},
		Name:      "mutation-org-" + uid,
		Slug:      "mutation-org-" + uid,
	}
	require.NoError(t, app.DB.Create(&org).Error)
	account := models.WhatsAppAccount{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: org.ID,
		Name:           "mutation-acct-" + uid,
		PhoneID:        "mutation-phone-" + uid,
		BusinessID:     "mutation-waba-" + uid,
		AccessToken:    "token",
		Status:         "active",
		IsSMB:          true,
	}
	require.NoError(t, app.DB.Create(&account).Error)
	contact := models.Contact{
		BaseModel:       models.BaseModel{ID: uuid.New()},
		OrganizationID:  org.ID,
		PhoneNumber:     "6014" + uid,
		ProfileName:     "Editing Customer",
		WhatsAppAccount: account.Name,
		BSUID:           "US.editing-" + uid,
		Metadata:        models.JSONB{},
	}
	require.NoError(t, app.DB.Create(&contact).Error)
	originalWAMID := "wamid.mutation.original-" + uid
	original := models.Message{
		BaseModel:         models.BaseModel{ID: uuid.NewSHA1(account.ID, []byte("coexistence-message:"+originalWAMID))},
		OrganizationID:    org.ID,
		WhatsAppAccount:   account.Name,
		ContactID:         contact.ID,
		WhatsAppMessageID: originalWAMID,
		Direction:         models.DirectionIncoming,
		MessageType:       models.MessageTypeText,
		Content:           "orginal text",
		Status:            models.MessageStatusReceived,
		Metadata:          models.JSONB{},
	}
	require.NoError(t, app.DB.Create(&original).Error)

	// Account.Name is a mutable display label, while Meta's WAMID is the stable
	// message identity. Replays and mutations after a rename must still target
	// the pre-rename row rather than create a duplicate.
	renamedAccount := "mutation-renamed-" + uid
	require.NoError(t, app.DB.Model(&models.WhatsAppAccount{}).Where(
		"organization_id = ? AND id = ?",
		org.ID,
		account.ID,
	).Update("name", renamedAccount).Error)
	account.Name = renamedAccount

	replayPayload := []byte(`{
		"object":"whatsapp_business_account",
		"entry":[{"id":"` + account.BusinessID + `","changes":[{"field":"history","value":{
			"messaging_product":"whatsapp",
			"metadata":{"display_phone_number":"15550783881","phone_number_id":"` + account.PhoneID + `"},
			"history":[{"metadata":{"phase":0,"chunk_order":1,"progress":10},"threads":[{
				"id":"` + contact.PhoneNumber + `","messages":[{"from":"` + contact.PhoneNumber + `",
					"id":"` + originalWAMID + `","timestamp":"1739230990","type":"text",
					"text":{"body":"replayed original"},"history_context":{"status":"READ"}}]
			}]}]
		}}]}]
	}`)
	sendSignedWebhook(t, app, replayPayload)

	mediaDetailPayload := []byte(`{
		"object":"whatsapp_business_account",
		"entry":[{"id":"` + account.BusinessID + `","changes":[{"field":"history","value":{
			"messaging_product":"whatsapp",
			"metadata":{"display_phone_number":"15550783881","phone_number_id":"` + account.PhoneID + `"},
			"messages":[{"from":"` + contact.PhoneNumber + `","id":"` + originalWAMID + `",
				"timestamp":"1739230995","type":"video","video":{"caption":"rename media detail",
				"mime_type":"video/mp4","sha256":"rename-sha","id":"rename-media-` + uid + `"}}]
		}}]}]
	}`)
	sendSignedWebhook(t, app, mediaDetailPayload)

	var renameReplay models.Message
	require.NoError(t, app.DB.First(&renameReplay, original.ID).Error)
	assert.Equal(t, "rename-media-"+uid, renameReplay.Metadata["coexistence_media_id"])
	var renameReplayCount int64
	require.NoError(t, app.DB.Model(&models.Message{}).Where(
		"organization_id = ? AND whats_app_message_id = ?",
		org.ID,
		originalWAMID,
	).Count(&renameReplayCount).Error)
	assert.EqualValues(t, 1, renameReplayCount)

	editEventID := "wamid.mutation.edit-" + uid
	editPayload := []byte(`{
		"object":"whatsapp_business_account",
		"entry":[{"id":"` + account.BusinessID + `","changes":[{"field":"messages","value":{
			"messaging_product":"whatsapp",
			"metadata":{"phone_number_id":"` + account.PhoneID + `"},
			"messages":[{"from_user_id":"` + contact.BSUID + `","id":"` + editEventID + `",
				"timestamp":"1739231000","type":"edit","edit":{"original_message_id":"` + originalWAMID + `",
				"message":{"type":"text","text":{"body":"corrected text"}}}}]
		}}]}]
	}`)
	sendSignedWebhook(t, app, editPayload)
	sendSignedWebhook(t, app, editPayload)

	var stored models.Message
	require.NoError(t, app.DB.First(&stored, original.ID).Error)
	assert.Equal(t, models.DirectionIncoming, stored.Direction)
	assert.Equal(t, "corrected text", stored.Content)
	assert.Equal(t, editEventID, stored.Metadata["coexistence_edit_event_id"])
	assert.Equal(t, "messages", stored.Metadata["coexistence_source"])

	revokeEventID := "wamid.mutation.revoke-" + uid
	revokePayload := []byte(`{
		"object":"whatsapp_business_account",
		"entry":[{"id":"` + account.BusinessID + `","changes":[{"field":"messages","value":{
			"messaging_product":"whatsapp",
			"metadata":{"phone_number_id":"` + account.PhoneID + `"},
			"messages":[{"from_user_id":"` + contact.BSUID + `","id":"` + revokeEventID + `",
				"timestamp":"1739231010","type":"revoke","revoke":{"original_message_id":"` + originalWAMID + `"}}]
		}}]}]
	}`)
	sendSignedWebhook(t, app, revokePayload)
	sendSignedWebhook(t, app, revokePayload)

	require.NoError(t, app.DB.First(&stored, original.ID).Error)
	assert.Equal(t, models.DirectionIncoming, stored.Direction)
	assert.Equal(t, "[Message deleted from WhatsApp Business App]", stored.Content)
	assert.Equal(t, true, stored.Metadata["coexistence_revoked"])
	assert.Equal(t, revokeEventID, stored.Metadata["coexistence_revoke_event_id"])
	assert.Equal(t, "messages", stored.Metadata["coexistence_source"])
	assert.Equal(t, models.MessageTypeText, stored.MessageType)
	assert.Empty(t, stored.MediaURL)
	assert.Empty(t, stored.MediaMimeType)
	assert.Empty(t, stored.MediaFilename)
	assert.NotContains(t, stored.Metadata, coexistenceMediaProviderIDMetadataKey)
	assert.NotContains(t, stored.Metadata, coexistenceMediaHydratedIDMetadataKey)
	assert.NotContains(t, stored.Metadata, "coexistence_media_sha256")

	missingEditOriginal := "wamid.mutation.missing-edit-original-" + uid
	missingEditEvent := "wamid.mutation.missing-edit-event-" + uid
	missingEdit := []byte(`{
		"object":"whatsapp_business_account",
		"entry":[{"id":"` + account.BusinessID + `","changes":[{"field":"messages","value":{
			"messaging_product":"whatsapp","metadata":{"phone_number_id":"` + account.PhoneID + `"},
			"contacts":[{"profile":{"name":"Editing Customer"},"user_id":"` + contact.BSUID + `"}],
			"messages":[{"from_user_id":"` + contact.BSUID + `","id":"` + missingEditEvent + `",
				"timestamp":"1739231110","type":"edit","edit":{"original_message_id":"` + missingEditOriginal + `",
				"message":{"type":"text","text":{"body":"edited before original"}}}}]
		}}]}]
	}`)
	sendSignedWebhook(t, app, missingEdit)

	var pendingEdit models.Message
	require.NoError(t, app.DB.Where(
		"organization_id = ? AND whats_app_account = ? AND whats_app_message_id = ?",
		org.ID,
		account.Name,
		missingEditOriginal,
	).First(&pendingEdit).Error)
	assert.Equal(t, missingEditOriginal, pendingEdit.WhatsAppMessageID)
	assert.Equal(t, "edited before original", pendingEdit.Content)
	assert.Equal(t, missingEditEvent, pendingEdit.Metadata["coexistence_edit_event_id"])

	lateEditOriginal := []byte(`{
		"object":"whatsapp_business_account",
		"entry":[{"id":"` + account.BusinessID + `","changes":[{"field":"messages","value":{
			"messaging_product":"whatsapp","metadata":{"phone_number_id":"` + account.PhoneID + `"},
			"contacts":[{"profile":{"name":"Editing Customer"},"wa_id":"` + contact.PhoneNumber + `","user_id":"` + contact.BSUID + `"}],
			"messages":[{"from":"` + contact.PhoneNumber + `","from_user_id":"` + contact.BSUID + `",
				"id":"` + missingEditOriginal + `","timestamp":"1739231100","type":"text","text":{"body":"stale original"}}]
		}}]}]
	}`)
	sendSignedWebhook(t, app, lateEditOriginal)
	require.NoError(t, app.DB.First(&pendingEdit, pendingEdit.ID).Error)
	assert.Equal(t, "edited before original", pendingEdit.Content,
		"the late original must deduplicate against and not overwrite the newer edit")

	missingRevokeOriginal := "wamid.mutation.missing-revoke-original-" + uid
	missingRevokeEvent := "wamid.mutation.missing-revoke-event-" + uid
	missingRevoke := []byte(`{
		"object":"whatsapp_business_account",
		"entry":[{"id":"` + account.BusinessID + `","changes":[{"field":"messages","value":{
			"messaging_product":"whatsapp","metadata":{"phone_number_id":"` + account.PhoneID + `"},
			"contacts":[{"profile":{"name":"Editing Customer"},"user_id":"` + contact.BSUID + `"}],
			"messages":[{"from_user_id":"` + contact.BSUID + `","id":"` + missingRevokeEvent + `",
				"timestamp":"1739231210","type":"revoke","revoke":{"original_message_id":"` + missingRevokeOriginal + `"}}]
		}}]}]
	}`)
	sendSignedWebhook(t, app, missingRevoke)

	var pendingRevoke models.Message
	require.NoError(t, app.DB.Where(
		"organization_id = ? AND whats_app_account = ? AND whats_app_message_id = ?",
		org.ID,
		account.Name,
		missingRevokeOriginal,
	).First(&pendingRevoke).Error)
	assert.Equal(t, "[Message deleted from WhatsApp Business App]", pendingRevoke.Content)
	assert.Equal(t, missingRevokeEvent, pendingRevoke.Metadata["coexistence_revoke_event_id"])
	assert.Equal(t, true, pendingRevoke.Metadata["coexistence_revoked"])

	lateRevokeOriginal := []byte(`{
		"object":"whatsapp_business_account",
		"entry":[{"id":"` + account.BusinessID + `","changes":[{"field":"messages","value":{
			"messaging_product":"whatsapp","metadata":{"phone_number_id":"` + account.PhoneID + `"},
			"messages":[{"from":"` + contact.PhoneNumber + `","from_user_id":"` + contact.BSUID + `",
				"id":"` + missingRevokeOriginal + `","timestamp":"1739231200","type":"text","text":{"body":"deleted original"}}]
		}}]}]
	}`)
	sendSignedWebhook(t, app, lateRevokeOriginal)
	require.NoError(t, app.DB.First(&pendingRevoke, pendingRevoke.ID).Error)
	assert.Equal(t, "[Message deleted from WhatsApp Business App]", pendingRevoke.Content,
		"the late original must deduplicate against and not resurrect a revoke")

	var count int64
	require.NoError(t, app.DB.Model(&models.Message{}).Where(
		"organization_id = ? AND whats_app_message_id IN ?",
		org.ID,
		[]string{originalWAMID, missingEditOriginal, missingRevokeOriginal},
	).Count(&count).Error)
	assert.EqualValues(t, 3, count, "mutations and late originals must deduplicate by original WAMID")
}

func TestWebhookHandler_CoexistenceHistoryIngestsAndDeduplicates(t *testing.T) {
	app := webhookTestApp(t)
	uid := uuid.New().String()[:8]
	org := models.Organization{
		BaseModel: models.BaseModel{ID: uuid.New()},
		Name:      "history-org-" + uid,
		Slug:      "history-org-" + uid,
	}
	require.NoError(t, app.DB.Create(&org).Error)
	account := models.WhatsAppAccount{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: org.ID,
		Name:           "history-acct-" + uid,
		PhoneID:        "phone-history-" + uid,
		BusinessID:     "biz-history-" + uid,
		AccessToken:    "token",
		Status:         "active",
		IsSMB:          true,
	}
	require.NoError(t, app.DB.Create(&account).Error)
	state := models.WhatsAppCoexistenceState{
		ID:                uuid.New(),
		OrganizationID:    org.ID,
		WhatsAppAccountID: account.ID,
		OnboardingStatus:  models.CoexistenceOnboardingStatusSyncing,
		SyncStatus:        models.CoexistenceSyncStatusInProgress,
		ContactSyncStatus: models.CoexistenceSyncStatusRequested,
		HistoryConsent:    models.CoexistenceHistoryConsentUnknown,
		HistorySyncStatus: models.CoexistenceSyncStatusRequested,
		LifecycleStatus:   models.CoexistenceLifecycleStatusConnected,
		LifecycleMetadata: models.JSONB{},
		Version:           1,
	}
	require.NoError(t, app.DB.Create(&state).Error)

	initial := []byte(`{
		"object":"whatsapp_business_account",
		"entry":[{"id":"` + account.BusinessID + `","changes":[{"field":"history","value":{
			"messaging_product":"whatsapp",
			"metadata":{"display_phone_number":"15550783881","phone_number_id":"` + account.PhoneID + `"},
			"history":[{"metadata":{"phase":0,"chunk_order":1,"progress":55},"threads":[{
				"id":"16505551234",
				"messages":[
					{"from":"15550783881","id":"wamid.history.out","timestamp":"1739230955","type":"text","text":{"body":"Information"},"history_context":{"status":"DELIVERED"}},
					{"from":"15550783881","id":"wamid.history.media","timestamp":"1739230970","type":"media_placeholder","history_context":{"status":"PLAYED"}},
					{"from":"16505551234","id":"wamid.history.in","timestamp":"1739230980","type":"text","text":{"body":"Thanks!"},"history_context":{"status":"READ"}}
				]
			}]}]
		}}]}]
	}`)

	sendSignedWebhook(t, app, initial)
	sendSignedWebhook(t, app, initial)

	var messages []models.Message
	require.NoError(t, app.DB.Where(
		"organization_id = ? AND whats_app_account = ?",
		org.ID,
		account.Name,
	).Order("created_at").Find(&messages).Error)
	require.Len(t, messages, 3, "replayed history chunks must deduplicate by wamid")
	byWAMID := make(map[string]models.Message, len(messages))
	for _, message := range messages {
		byWAMID[message.WhatsAppMessageID] = message
	}
	assert.Equal(t, models.DirectionOutgoing, byWAMID["wamid.history.out"].Direction)
	assert.Equal(t, models.MessageStatusDelivered, byWAMID["wamid.history.out"].Status)
	assert.Equal(t, models.DirectionIncoming, byWAMID["wamid.history.in"].Direction)
	assert.Equal(t, models.MessageStatusRead, byWAMID["wamid.history.in"].Status)
	assert.Equal(t, models.MessageType("media_placeholder"), byWAMID["wamid.history.media"].MessageType)

	mediaDetail := []byte(`{
		"object":"whatsapp_business_account",
		"entry":[{"id":"` + account.BusinessID + `","changes":[{"field":"history","value":{
			"messaging_product":"whatsapp",
			"metadata":{"display_phone_number":"15550783881","phone_number_id":"` + account.PhoneID + `"},
			"messages":[{"from":"15550783881","to":"16505551234","id":"wamid.history.media","timestamp":"1739230970","type":"video","video":{"caption":"Demo","mime_type":"video/mp4","sha256":"abc123","id":"media-asset-1"}}]
		}}]}]
	}`)
	sendSignedWebhook(t, app, mediaDetail)

	var media models.Message
	require.NoError(t, app.DB.Where(
		"organization_id = ? AND whats_app_message_id = ?",
		org.ID,
		"wamid.history.media",
	).First(&media).Error)
	assert.Equal(t, models.MessageTypeVideo, media.MessageType)
	assert.Equal(t, "Demo", media.Content)
	assert.Equal(t, "video/mp4", media.MediaMimeType)
	assert.Equal(t, "media-asset-1", media.Metadata["coexistence_media_id"])

	completion := []byte(`{
		"object":"whatsapp_business_account",
		"entry":[{"id":"` + account.BusinessID + `","changes":[{"field":"history","value":{
			"messaging_product":"whatsapp",
			"metadata":{"display_phone_number":"15550783881","phone_number_id":"` + account.PhoneID + `"},
			"history":[{"metadata":{"phase":2,"chunk_order":3,"progress":100},"threads":[]}]
		}}]}]
	}`)
	sendSignedWebhook(t, app, completion)

	var updatedState models.WhatsAppCoexistenceState
	require.NoError(t, app.DB.Where(
		"organization_id = ? AND whats_app_account_id = ?",
		org.ID,
		account.ID,
	).First(&updatedState).Error)
	assert.Equal(t, models.CoexistenceSyncStatusCompleted, updatedState.HistorySyncStatus)
	assert.Equal(t, models.CoexistenceHistoryConsentGranted, updatedState.HistoryConsent)
	assert.Equal(t, 100, updatedState.HistoryProgressPercent)
	require.NotNil(t, updatedState.HistoryLastPhase)
	assert.Equal(t, 2, *updatedState.HistoryLastPhase)
	assert.Equal(t, models.CoexistenceSyncStatusCompleted, updatedState.SyncStatus)
	assert.Equal(t, models.CoexistenceOnboardingStatusReady, updatedState.OnboardingStatus)
	assert.NotNil(t, updatedState.HistoryCompletedAt)
	require.NotNil(t, updatedState.SyncCompletedAt)
	completedAt := *updatedState.HistoryCompletedAt
	syncCompletedAt := *updatedState.SyncCompletedAt

	late := []byte(`{
		"object":"whatsapp_business_account",
		"entry":[{"id":"` + account.BusinessID + `","changes":[{"field":"history","value":{
			"messaging_product":"whatsapp",
			"metadata":{"display_phone_number":"15550783881","phone_number_id":"` + account.PhoneID + `"},
			"history":[
				{"errors":[{"code":2593999,"title":"Late provider error","message":"late error"}]},
				{"metadata":{"phase":0,"chunk_order":1,"progress":10},"threads":[]}
			]
		}}]}]
	}`)
	sendSignedWebhook(t, app, late)
	require.NoError(t, app.DB.Where(
		"organization_id = ? AND whats_app_account_id = ?",
		org.ID,
		account.ID,
	).First(&updatedState).Error)
	assert.Equal(t, models.CoexistenceSyncStatusCompleted, updatedState.HistorySyncStatus)
	assert.Equal(t, models.CoexistenceHistoryConsentGranted, updatedState.HistoryConsent)
	assert.Equal(t, 100, updatedState.HistoryProgressPercent)
	require.NotNil(t, updatedState.HistoryCompletedAt)
	require.NotNil(t, updatedState.SyncCompletedAt)
	assert.True(t, updatedState.HistoryCompletedAt.Equal(completedAt))
	assert.True(t, updatedState.SyncCompletedAt.Equal(syncCompletedAt))
	assert.Empty(t, updatedState.HistorySyncErrorCode)
}

func TestWebhookHandler_CoexistenceHistoryFencesPriorOnboardingCycle(t *testing.T) {
	app := webhookTestApp(t)
	uid := uuid.NewString()[:8]
	org := models.Organization{
		BaseModel: models.BaseModel{ID: uuid.New()},
		Name:      "history-cycle-org-" + uid,
		Slug:      "history-cycle-org-" + uid,
	}
	require.NoError(t, app.DB.Create(&org).Error)
	account := models.WhatsAppAccount{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: org.ID,
		Name:           "history-cycle-account-" + uid,
		PhoneID:        "history-cycle-phone-" + uid,
		BusinessID:     "history-cycle-waba-" + uid,
		AccessToken:    "token",
		Status:         "active",
		IsSMB:          true,
	}
	require.NoError(t, app.DB.Create(&account).Error)
	onboardedAt := time.Unix(2_000_000_000, 0).UTC()
	require.NoError(t, app.DB.Create(&models.WhatsAppCoexistenceState{
		ID:                uuid.New(),
		OrganizationID:    org.ID,
		WhatsAppAccountID: account.ID,
		OnboardingStatus:  models.CoexistenceOnboardingStatusSyncing,
		OnboardedAt:       &onboardedAt,
		OnboardingCycle:   2,
		SyncStatus:        models.CoexistenceSyncStatusInProgress,
		ContactSyncStatus: models.CoexistenceSyncStatusRequested,
		HistoryConsent:    models.CoexistenceHistoryConsentUnknown,
		HistorySyncStatus: models.CoexistenceSyncStatusRequested,
		LifecycleStatus:   models.CoexistenceLifecycleStatusConnected,
		LifecycleMetadata: models.JSONB{},
		Version:           1,
	}).Error)

	oldCompletion := []byte(fmt.Sprintf(`{
		"object":"whatsapp_business_account",
		"entry":[{"id":"%s","time":%d,"changes":[{"field":"history","value":{
			"messaging_product":"whatsapp",
			"metadata":{"display_phone_number":"15550783881","phone_number_id":"%s"},
			"history":[{"metadata":{"phase":2,"chunk_order":4,"progress":100},"threads":[{
				"id":"16505551234","messages":[{"from":"16505551234",
					"id":"wamid.old-cycle-%s","timestamp":"1999999990","type":"text",
					"text":{"body":"persist old-cycle data"},"history_context":{"status":"READ"}}]
			}]}]
		}}]}]
	}`, account.BusinessID, onboardedAt.Unix()-10, account.PhoneID, uid))
	sendSignedWebhook(t, app, oldCompletion)

	oldError := []byte(fmt.Sprintf(`{
		"object":"whatsapp_business_account",
		"entry":[{"id":"%s","time":%d,"changes":[{"field":"history","value":{
			"metadata":{"phone_number_id":"%s"},
			"history":[{"errors":[{"code":2593999,"title":"old cycle failure","message":"old cycle failure"}]}]
		}}]}]
	}`, account.BusinessID, onboardedAt.Unix()-5, account.PhoneID))
	sendSignedWebhook(t, app, oldError)

	missingTimestamp := []byte(fmt.Sprintf(`{
		"object":"whatsapp_business_account",
		"entry":[{"id":"%s","changes":[{"field":"history","value":{
			"messaging_product":"whatsapp",
			"metadata":{"display_phone_number":"15550783881","phone_number_id":"%s"},
			"history":[{"metadata":{"phase":2,"chunk_order":5,"progress":100},"threads":[{
				"id":"16505551234","messages":[{"from":"16505551234",
					"id":"wamid.missing-cycle-time-%s","timestamp":"2000000001","type":"text",
					"text":{"body":"persist uncorrelated data"},"history_context":{"status":"READ"}}]
			}]}]
		}}]}]
	}`, account.BusinessID, account.PhoneID, uid))
	sendSignedWebhook(t, app, missingTimestamp)

	var state models.WhatsAppCoexistenceState
	require.NoError(t, app.DB.Where(
		"organization_id = ? AND whats_app_account_id = ?",
		org.ID,
		account.ID,
	).First(&state).Error)
	assert.Equal(t, models.CoexistenceSyncStatusRequested, state.HistorySyncStatus)
	assert.Equal(t, models.CoexistenceHistoryConsentUnknown, state.HistoryConsent)
	assert.Zero(t, state.HistoryProgressPercent)
	assert.Empty(t, state.HistorySyncErrorCode)
	assert.Nil(t, state.HistoryCompletedAt)
	assert.EqualValues(t, 1, state.Version)

	var persisted int64
	require.NoError(t, app.DB.Model(&models.Message{}).Where(
		"organization_id = ? AND whats_app_message_id IN ?",
		org.ID,
		[]string{"wamid.old-cycle-" + uid, "wamid.missing-cycle-time-" + uid},
	).Count(&persisted).Error)
	assert.EqualValues(t, 2, persisted, "fenced callbacks must still preserve message data")

	freshCompletion := []byte(fmt.Sprintf(`{
		"object":"whatsapp_business_account",
		"entry":[{"id":"%s","time":%d,"changes":[{"field":"history","value":{
			"metadata":{"phone_number_id":"%s"},
			"history":[{"metadata":{"phase":2,"chunk_order":6,"progress":100},"threads":[]}]
		}}]}]
	}`, account.BusinessID, onboardedAt.Unix(), account.PhoneID))
	sendSignedWebhook(t, app, freshCompletion)
	require.NoError(t, app.DB.Where(
		"organization_id = ? AND whats_app_account_id = ?",
		org.ID,
		account.ID,
	).First(&state).Error)
	assert.Equal(t, models.CoexistenceSyncStatusCompleted, state.HistorySyncStatus)
	assert.Equal(t, models.CoexistenceHistoryConsentGranted, state.HistoryConsent)
	assert.Equal(t, models.CoexistenceSyncStatusCompleted, state.SyncStatus)
	assert.Equal(t, models.CoexistenceOnboardingStatusReady, state.OnboardingStatus)
}

func TestWebhookHandler_CoexistenceHistoryDeclined(t *testing.T) {
	app := webhookTestApp(t)
	uid := uuid.New().String()[:8]
	org := models.Organization{
		BaseModel: models.BaseModel{ID: uuid.New()},
		Name:      "declined-org-" + uid,
		Slug:      "declined-org-" + uid,
	}
	require.NoError(t, app.DB.Create(&org).Error)
	account := models.WhatsAppAccount{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: org.ID,
		Name:           "declined-acct-" + uid,
		PhoneID:        "phone-declined-" + uid,
		BusinessID:     "biz-declined-" + uid,
		AccessToken:    "token",
		Status:         "active",
		IsSMB:          true,
	}
	require.NoError(t, app.DB.Create(&account).Error)
	require.NoError(t, app.DB.Create(&models.WhatsAppCoexistenceState{
		ID:                uuid.New(),
		OrganizationID:    org.ID,
		WhatsAppAccountID: account.ID,
		OnboardingStatus:  models.CoexistenceOnboardingStatusSyncing,
		SyncStatus:        models.CoexistenceSyncStatusInProgress,
		ContactSyncStatus: models.CoexistenceSyncStatusRequested,
		HistoryConsent:    models.CoexistenceHistoryConsentUnknown,
		HistorySyncStatus: models.CoexistenceSyncStatusRequested,
		LifecycleStatus:   models.CoexistenceLifecycleStatusConnected,
		LifecycleMetadata: models.JSONB{},
		Version:           1,
	}).Error)

	body := []byte(`{
		"object":"whatsapp_business_account",
		"entry":[{"id":"` + account.BusinessID + `","changes":[{"field":"history","value":{
			"messaging_product":"whatsapp",
			"metadata":{"display_phone_number":"15550783881","phone_number_id":"` + account.PhoneID + `"},
			"history":[{"errors":[{"code":2593109,"title":"History sync is turned off","message":"History sync is turned off","error_data":{"details":"History sharing is turned off by the business"}}]}]
		}}]}]
	}`)
	sendSignedWebhook(t, app, body)

	var state models.WhatsAppCoexistenceState
	require.NoError(t, app.DB.Where(
		"organization_id = ? AND whats_app_account_id = ?",
		org.ID,
		account.ID,
	).First(&state).Error)
	assert.Equal(t, models.CoexistenceSyncStatusDeclined, state.HistorySyncStatus)
	assert.Equal(t, models.CoexistenceHistoryConsentDeclined, state.HistoryConsent)
	assert.Equal(t, "2593109", state.HistorySyncErrorCode)
	assert.Contains(t, state.HistorySyncErrorMessage, "turned off")
	assert.Equal(t, models.CoexistenceSyncStatusCompleted, state.SyncStatus)
	assert.Equal(t, models.CoexistenceOnboardingStatusReady, state.OnboardingStatus)
	require.NotNil(t, state.HistoryConsentAt)
	consentAt := *state.HistoryConsentAt

	lateDetail := []byte(`{
		"object":"whatsapp_business_account",
		"entry":[{"id":"` + account.BusinessID + `","changes":[{"field":"history","value":{
			"messaging_product":"whatsapp",
			"metadata":{"display_phone_number":"15550783881","phone_number_id":"` + account.PhoneID + `"},
			"history":[{"metadata":{"phase":0,"chunk_order":1,"progress":25},"threads":[]}],
			"messages":[{"from":"16505550009","id":"wamid.declined.late-media-` + uid + `",
				"timestamp":"1739230970","type":"video","video":{"caption":"late","mime_type":"video/mp4","id":"late-media"}}]
		}}]}]
	}`)
	sendSignedWebhook(t, app, lateDetail)
	require.NoError(t, app.DB.Where(
		"organization_id = ? AND whats_app_account_id = ?",
		org.ID,
		account.ID,
	).First(&state).Error)
	assert.Equal(t, models.CoexistenceSyncStatusDeclined, state.HistorySyncStatus)
	assert.Equal(t, models.CoexistenceHistoryConsentDeclined, state.HistoryConsent)
	require.NotNil(t, state.HistoryConsentAt)
	assert.True(t, state.HistoryConsentAt.Equal(consentAt))
	assert.Equal(t, models.CoexistenceSyncStatusCompleted, state.SyncStatus)
	assert.Equal(t, models.CoexistenceOnboardingStatusReady, state.OnboardingStatus)
}

func TestWebhookHandler_CoexistenceLifecycleAuthenticatesDispatchWABA(t *testing.T) {
	app := webhookTestApp(t)
	for _, tc := range []struct {
		name         string
		foreignWABA  bool
		sharedSecret bool
		sharedWABA   bool
		staleCache   bool
		omitPhone    bool
		wantStatus   int
	}{
		{name: "tenant A phone cannot offboard tenant B", foreignWABA: true, wantStatus: 403},
		{name: "shared app secret cannot substitute a different WABA", foreignWABA: true, sharedSecret: true, wantStatus: 403},
		{name: "stale phone cache cannot authorize previous WABA", foreignWABA: true, sharedSecret: true, staleCache: true, wantStatus: 403},
		{name: "matching phone still verifies every WABA tenant", sharedWABA: true, wantStatus: 403},
		{name: "matching phone and managed WABA secret", wantStatus: 200},
		{name: "WABA only with managed secret", omitPhone: true, wantStatus: 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			uid := uuid.NewString()[:8]
			secrets := []string{"lifecycle-tenant-a-" + uid, "lifecycle-tenant-b-" + uid}
			if tc.sharedSecret {
				secrets[1] = secrets[0]
			}
			accounts := make([]models.WhatsAppAccount, 2)
			for i := range accounts {
				org := testutil.CreateTestOrganization(t, app.DB)
				ciphertext, err := appcrypto.Encrypt(secrets[i], app.Config.App.EncryptionKey)
				require.NoError(t, err)
				require.NoError(t, app.DB.Model(org).Update("settings", models.JSONB{
					"meta_app_secret_encrypted": ciphertext,
				}).Error)
				require.NoError(t, app.DB.Create(&models.ProviderIntegration{
					BaseModel:      models.BaseModel{ID: uuid.New()},
					OrganizationID: org.ID,
					Provider:       integrationProviderMeta,
					Enabled:        true,
					Config:         models.JSONB{},
					CredentialData: models.JSONB{},
				}).Error)
				accounts[i] = models.WhatsAppAccount{
					BaseModel:      models.BaseModel{ID: uuid.New()},
					OrganizationID: org.ID,
					Name:           fmt.Sprintf("auth-lifecycle-%s-%d", uid, i),
					PhoneID:        fmt.Sprintf("auth-phone-%s-%d", uid, i),
					BusinessID:     fmt.Sprintf("auth-waba-%s-%d", uid, i),
					AppSecret:      "stale-legacy-secret",
					AccessToken:    "synthetic-token",
					Status:         "active",
					IsSMB:          true,
				}
				if tc.sharedWABA && i == 1 {
					accounts[i].BusinessID = accounts[0].BusinessID
				}
				require.NoError(t, app.DB.Create(&accounts[i]).Error)
			}
			wabaID := accounts[0].BusinessID
			if tc.foreignWABA {
				wabaID = accounts[1].BusinessID
			}
			if tc.staleCache {
				stale := accounts[0]
				stale.BusinessID = wabaID
				cached, err := json.Marshal(whatsAppAccountCache{
					WhatsAppAccount: stale,
					AccessToken:     stale.AccessToken,
					AppSecret:       stale.AppSecret,
				})
				require.NoError(t, err)
				require.NoError(t, app.Redis.Set(context.Background(),
					whatsappAccountCachePrefix+stale.PhoneID, cached, time.Hour).Err())
			}
			value := map[string]any{"event": "ACCOUNT_OFFBOARDED"}
			if !tc.omitPhone {
				value["metadata"] = map[string]string{"phone_number_id": accounts[0].PhoneID}
			}
			body, err := json.Marshal(map[string]any{
				"object": "whatsapp_business_account",
				"entry": []any{map[string]any{
					"id": wabaID, "time": 1768477403,
					"changes": []any{map[string]any{"field": "account_update", "value": value}},
				}},
			})
			require.NoError(t, err)
			mac := hmac.New(sha256.New, []byte(secrets[0]))
			_, err = mac.Write(body)
			require.NoError(t, err)
			req := testutil.NewRequest(t)
			req.RequestCtx.Request.Header.SetMethod("POST")
			req.RequestCtx.Request.Header.SetContentType("application/json")
			req.RequestCtx.Request.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
			req.RequestCtx.Request.SetBody(body)
			require.NoError(t, app.WebhookHandler(req))
			require.Equal(t, tc.wantStatus, req.RequestCtx.Response.StatusCode(), string(req.RequestCtx.Response.Body()))
			for i := range accounts {
				var stored models.WhatsAppAccount
				require.NoError(t, app.DB.First(&stored, accounts[i].ID).Error)
				if tc.wantStatus == 200 && i == 0 {
					assert.Equal(t, "disconnected", stored.Status)
					var state models.WhatsAppCoexistenceState
					require.NoError(t, app.DB.Where("whats_app_account_id = ?", stored.ID).First(&state).Error)
					assert.Equal(t, models.CoexistenceLifecycleStatusOffboarded, state.LifecycleStatus)
				} else {
					assert.Equal(t, "active", stored.Status)
					var count int64
					require.NoError(t, app.DB.Model(&models.WhatsAppCoexistenceState{}).
						Where("whats_app_account_id = ?", stored.ID).Count(&count).Error)
					assert.Zero(t, count, "rejected signatures must not create or mutate lifecycle state")
				}
			}
		})
	}
}

func TestWebhookHandler_CoexistenceLifecycle(t *testing.T) {
	app := webhookTestApp(t)
	uid := uuid.New().String()[:8]
	org := models.Organization{
		BaseModel: models.BaseModel{ID: uuid.New()},
		Name:      "lifecycle-org-" + uid,
		Slug:      "lifecycle-org-" + uid,
	}
	require.NoError(t, app.DB.Create(&org).Error)
	account := models.WhatsAppAccount{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: org.ID,
		Name:           "lifecycle-acct-" + uid,
		PhoneID:        "phone-lifecycle-" + uid,
		BusinessID:     "biz-lifecycle-" + uid,
		AccessToken:    "token",
		Status:         "active",
		IsSMB:          true,
	}
	require.NoError(t, app.DB.Create(&account).Error)
	require.NoError(t, app.DB.Create(&models.WhatsAppCoexistenceState{
		ID:                uuid.New(),
		OrganizationID:    org.ID,
		WhatsAppAccountID: account.ID,
		OnboardingStatus:  models.CoexistenceOnboardingStatusReady,
		SyncStatus:        models.CoexistenceSyncStatusCompleted,
		ContactSyncStatus: models.CoexistenceSyncStatusCompleted,
		HistoryConsent:    models.CoexistenceHistoryConsentGranted,
		HistorySyncStatus: models.CoexistenceSyncStatusCompleted,
		LifecycleStatus:   models.CoexistenceLifecycleStatusConnected,
		LifecycleMetadata: models.JSONB{},
		Version:           1,
	}).Error)

	removed := []byte(`{
		"object":"whatsapp_business_account",
		"entry":[{"id":"` + account.BusinessID + `","time":1768477203,"changes":[{"field":"account_update","value":{
			"phone_number":"15550783881",
			"event":"PARTNER_REMOVED",
			"disconnection_info":{"reason":"PRIMARY_INACTIVITY","initiated_by":"SYSTEM"}
		}}]}]
	}`)
	sendSignedWebhook(t, app, removed)

	var disconnected models.WhatsAppAccount
	require.NoError(t, app.DB.First(&disconnected, account.ID).Error)
	assert.Equal(t, "disconnected", disconnected.Status)
	var state models.WhatsAppCoexistenceState
	require.NoError(t, app.DB.Where("whats_app_account_id = ?", account.ID).First(&state).Error)
	assert.Equal(t, models.CoexistenceLifecycleStatusDisconnected, state.LifecycleStatus)
	assert.Equal(t, "PARTNER_REMOVED", state.LastLifecycleEvent)
	assert.Equal(t, "PRIMARY_INACTIVITY", state.DisconnectReasonCode)
	assert.Equal(t, "SYSTEM", state.LifecycleMetadata["initiated_by"])
	assert.NotNil(t, state.DisconnectedAt)

	reconnected := []byte(`{
		"object":"whatsapp_business_account",
		"entry":[{"id":"` + account.BusinessID + `","time":1768477303,"changes":[{"field":"account_update","value":{"event":"ACCOUNT_RECONNECTED"}}]}]
	}`)
	sendSignedWebhook(t, app, reconnected)

	require.NoError(t, app.DB.First(&disconnected, account.ID).Error)
	assert.Equal(t, "disconnected", disconnected.Status, "reconnect does not prove stored API credentials")
	require.NoError(t, app.DB.Where("whats_app_account_id = ?", account.ID).First(&state).Error)
	assert.Equal(t, models.CoexistenceLifecycleStatusConnected, state.LifecycleStatus)
	assert.Equal(t, "ACCOUNT_RECONNECTED", state.LastLifecycleEvent)
	assert.Equal(t, models.CoexistenceOnboardingStatusPending, state.OnboardingStatus,
		"the mobile companion reconnect does not refresh ReReply credentials")
	assert.Empty(t, state.DisconnectReasonCode)
	assert.NotNil(t, state.ReconnectedAt)

	// Even if a stale local row is active, a processed reconnect event proves
	// only the mobile companion link. It must fail closed until Embedded Signup
	// records a newer credential-refresh watermark.
	require.NoError(t, app.DB.Model(&models.WhatsAppAccount{}).Where("id = ?", account.ID).
		Update("status", "active").Error)
	reconnectedAfterRefresh := []byte(`{
		"object":"whatsapp_business_account",
		"entry":[{"id":"` + account.BusinessID + `","time":1768477353,"changes":[{"field":"account_update","value":{"event":"ACCOUNT_RECONNECTED"}}]}]
	}`)
	sendSignedWebhook(t, app, reconnectedAfterRefresh)
	require.NoError(t, app.DB.First(&disconnected, account.ID).Error)
	assert.Equal(t, "disconnected", disconnected.Status,
		"a reconnect event must require a new Embedded Signup credential exchange")
	require.ErrorIs(t, app.prepareWhatsAppAccountForOutbound(&disconnected), whatsappaccount.ErrOutboundInactive)
	require.NoError(t, app.DB.Where("whats_app_account_id = ?", account.ID).First(&state).Error)
	assert.Equal(t, models.CoexistenceOnboardingStatusPending, state.OnboardingStatus)

	staleRemoval := []byte(`{
		"object":"whatsapp_business_account",
		"entry":[{"id":"` + account.BusinessID + `","time":1768477250,"changes":[{"field":"account_update","value":{
			"phone_number":"15550783881",
			"event":"PARTNER_REMOVED",
			"disconnection_info":{"reason":"PRIMARY_INACTIVITY","initiated_by":"SYSTEM"}
		}}]}]
	}`)
	sendSignedWebhook(t, app, staleRemoval)

	require.NoError(t, app.DB.Where("whats_app_account_id = ?", account.ID).First(&state).Error)
	assert.Equal(t, models.CoexistenceLifecycleStatusConnected, state.LifecycleStatus)
	assert.Equal(t, "ACCOUNT_RECONNECTED", state.LastLifecycleEvent)
	assert.Equal(t, int64(1768477353), state.LastLifecycleEventAt.Unix())

	offboarded := []byte(`{
		"object":"whatsapp_business_account",
		"entry":[{"id":"` + account.BusinessID + `","time":1768477403,"changes":[{"field":"account_update","value":{"event":"ACCOUNT_OFFBOARDED"}}]}]
	}`)
	sendSignedWebhook(t, app, offboarded)
	require.NoError(t, app.DB.Where("whats_app_account_id = ?", account.ID).First(&state).Error)
	assert.Equal(t, models.CoexistenceLifecycleStatusOffboarded, state.LifecycleStatus)
	assert.Equal(t, models.CoexistenceOnboardingStatusOffboarded, state.OnboardingStatus)
	assert.Equal(t, "ACCOUNT_OFFBOARDED", state.LastLifecycleEvent)
	assert.NotNil(t, state.OffboardedAt)
}

func TestCoexistenceReconnectInCredentialRefreshSecondFailsOutboundClosed(t *testing.T) {
	app := webhookTestApp(t)
	uid := uuid.NewString()[:8]
	org := models.Organization{
		BaseModel: models.BaseModel{ID: uuid.New()},
		Name:      "same-second-org-" + uid,
		Slug:      "same-second-org-" + uid,
	}
	require.NoError(t, app.DB.Create(&org).Error)
	account := models.WhatsAppAccount{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: org.ID,
		Name:           "same-second-acct-" + uid,
		PhoneID:        "same-second-phone-" + uid,
		BusinessID:     "same-second-waba-" + uid,
		AccessToken:    "token",
		Status:         "active",
		IsSMB:          true,
	}
	require.NoError(t, app.DB.Create(&account).Error)
	const lifecycleSecond int64 = 1768477353
	credentialRefreshAt := time.Unix(lifecycleSecond, 900_000_000).UTC()
	require.NoError(t, app.DB.Create(&models.WhatsAppCoexistenceState{
		ID:                   uuid.New(),
		OrganizationID:       org.ID,
		WhatsAppAccountID:    account.ID,
		OnboardingStatus:     models.CoexistenceOnboardingStatusReady,
		SyncStatus:           models.CoexistenceSyncStatusCompleted,
		ContactSyncStatus:    models.CoexistenceSyncStatusCompleted,
		HistoryConsent:       models.CoexistenceHistoryConsentGranted,
		HistorySyncStatus:    models.CoexistenceSyncStatusCompleted,
		LifecycleStatus:      models.CoexistenceLifecycleStatusConnected,
		LastLifecycleEvent:   "EMBEDDED_SIGNUP_CREDENTIAL_REFRESH",
		LastLifecycleEventAt: &credentialRefreshAt,
		LifecycleMetadata:    models.JSONB{},
		Version:              1,
	}).Error)

	reconnected := []byte(`{
		"object":"whatsapp_business_account",
		"entry":[{"id":"` + account.BusinessID + `","time":1768477353,"changes":[{"field":"account_update","value":{"event":"ACCOUNT_RECONNECTED"}}]}]
	}`)
	sendSignedWebhook(t, app, reconnected)

	var disconnected models.WhatsAppAccount
	require.NoError(t, app.DB.First(&disconnected, account.ID).Error)
	assert.Equal(t, "disconnected", disconnected.Status)
	require.ErrorIs(t, app.prepareWhatsAppAccountForOutbound(&disconnected), whatsappaccount.ErrOutboundInactive)

	var state models.WhatsAppCoexistenceState
	require.NoError(t, app.DB.Where("whats_app_account_id = ?", account.ID).First(&state).Error)
	assert.Equal(t, models.CoexistenceOnboardingStatusPending, state.OnboardingStatus)
	assert.Equal(t, "ACCOUNT_RECONNECTED", state.LastLifecycleEvent)
	require.NotNil(t, state.LastLifecycleEventAt)
	assert.Equal(t, lifecycleSecond, state.LastLifecycleEventAt.Unix())
}

func TestCoexistenceLifecycleRoutesPartnerRemovalByBusinessPhone(t *testing.T) {
	app := webhookTestApp(t)
	uid := uuid.NewString()[:8]
	org := models.Organization{
		BaseModel: models.BaseModel{ID: uuid.New()},
		Name:      "route-org-" + uid,
		Slug:      "route-org-" + uid,
	}
	require.NoError(t, app.DB.Create(&org).Error)
	wabaID := "route-waba-" + uid
	accounts := []models.WhatsAppAccount{
		{
			BaseModel:      models.BaseModel{ID: uuid.New()},
			OrganizationID: org.ID,
			Name:           "route-match-" + uid,
			PhoneID:        "route-match-phone-" + uid,
			BusinessID:     wabaID,
			AccessToken:    "token",
			Status:         "active",
			IsSMB:          true,
		},
		{
			BaseModel:      models.BaseModel{ID: uuid.New()},
			OrganizationID: org.ID,
			Name:           "route-other-" + uid,
			PhoneID:        "route-other-phone-" + uid,
			BusinessID:     wabaID,
			AccessToken:    "token",
			Status:         "active",
			IsSMB:          true,
		},
		{
			BaseModel:      models.BaseModel{ID: uuid.New()},
			OrganizationID: org.ID,
			Name:           "route-classic-" + uid,
			PhoneID:        "route-classic-phone-" + uid,
			BusinessID:     wabaID,
			AccessToken:    "token",
			Status:         "active",
			IsSMB:          false,
		},
	}
	for index := range accounts {
		require.NoError(t, app.DB.Create(&accounts[index]).Error)
	}
	states := []models.WhatsAppCoexistenceState{
		{
			ID:                  uuid.New(),
			OrganizationID:      org.ID,
			WhatsAppAccountID:   accounts[0].ID,
			BusinessPhoneNumber: "15550000001",
			OnboardingStatus:    models.CoexistenceOnboardingStatusReady,
			SyncStatus:          models.CoexistenceSyncStatusCompleted,
			ContactSyncStatus:   models.CoexistenceSyncStatusCompleted,
			HistoryConsent:      models.CoexistenceHistoryConsentGranted,
			HistorySyncStatus:   models.CoexistenceSyncStatusCompleted,
			LifecycleStatus:     models.CoexistenceLifecycleStatusConnected,
			LifecycleMetadata:   models.JSONB{},
			Version:             1,
		},
		{
			ID:                  uuid.New(),
			OrganizationID:      org.ID,
			WhatsAppAccountID:   accounts[1].ID,
			BusinessPhoneNumber: "15550000002",
			OnboardingStatus:    models.CoexistenceOnboardingStatusReady,
			SyncStatus:          models.CoexistenceSyncStatusCompleted,
			ContactSyncStatus:   models.CoexistenceSyncStatusCompleted,
			HistoryConsent:      models.CoexistenceHistoryConsentGranted,
			HistorySyncStatus:   models.CoexistenceSyncStatusCompleted,
			LifecycleStatus:     models.CoexistenceLifecycleStatusConnected,
			LifecycleMetadata:   models.JSONB{},
			Version:             1,
		},
	}
	for index := range states {
		require.NoError(t, app.DB.Create(&states[index]).Error)
	}

	require.NoError(t, app.persistCoexistenceLifecycleBeforeAck(
		wabaID,
		1768477503,
		"PARTNER_REMOVED",
		"+1 (555) 000-0001",
		&CoexistenceDisconnection{Reason: "OWNER_ACTION", InitiatedBy: "BUSINESS"},
	))

	var matchedAccount, otherAccount, classicAccount models.WhatsAppAccount
	require.NoError(t, app.DB.First(&matchedAccount, accounts[0].ID).Error)
	require.NoError(t, app.DB.First(&otherAccount, accounts[1].ID).Error)
	require.NoError(t, app.DB.First(&classicAccount, accounts[2].ID).Error)
	assert.Equal(t, "disconnected", matchedAccount.Status)
	assert.Equal(t, "active", otherAccount.Status)
	assert.Equal(t, "active", classicAccount.Status)

	var matchedState, otherState models.WhatsAppCoexistenceState
	require.NoError(t, app.DB.Where("whats_app_account_id = ?", accounts[0].ID).First(&matchedState).Error)
	require.NoError(t, app.DB.Where("whats_app_account_id = ?", accounts[1].ID).First(&otherState).Error)
	assert.Equal(t, models.CoexistenceLifecycleStatusDisconnected, matchedState.LifecycleStatus)
	assert.Equal(t, "PARTNER_REMOVED", matchedState.LastLifecycleEvent)
	assert.Equal(t, models.CoexistenceLifecycleStatusConnected, otherState.LifecycleStatus)
	assert.Empty(t, otherState.LastLifecycleEvent)

	var classicStateCount int64
	require.NoError(t, app.DB.Model(&models.WhatsAppCoexistenceState{}).
		Where("whats_app_account_id = ?", accounts[2].ID).Count(&classicStateCount).Error)
	assert.Zero(t, classicStateCount, "classic Cloud API accounts must not receive coexistence lifecycle state")
}

func TestCoexistenceLifecycleRejectsAmbiguousLegacyPhoneRouting(t *testing.T) {
	app := webhookTestApp(t)
	uid := uuid.NewString()[:8]
	org := models.Organization{
		BaseModel: models.BaseModel{ID: uuid.New()},
		Name:      "route-legacy-org-" + uid,
		Slug:      "route-legacy-org-" + uid,
	}
	require.NoError(t, app.DB.Create(&org).Error)
	wabaID := "route-legacy-waba-" + uid
	for index := 0; index < 2; index++ {
		account := models.WhatsAppAccount{
			BaseModel:      models.BaseModel{ID: uuid.New()},
			OrganizationID: org.ID,
			Name:           fmt.Sprintf("route-legacy-%s-%d", uid, index),
			PhoneID:        fmt.Sprintf("route-legacy-phone-%s-%d", uid, index),
			BusinessID:     wabaID,
			AccessToken:    "token",
			Status:         "active",
			IsSMB:          true,
		}
		require.NoError(t, app.DB.Create(&account).Error)
		require.NoError(t, app.DB.Create(&models.WhatsAppCoexistenceState{
			ID:                uuid.New(),
			OrganizationID:    org.ID,
			WhatsAppAccountID: account.ID,
			OnboardingStatus:  models.CoexistenceOnboardingStatusReady,
			SyncStatus:        models.CoexistenceSyncStatusCompleted,
			ContactSyncStatus: models.CoexistenceSyncStatusCompleted,
			HistoryConsent:    models.CoexistenceHistoryConsentGranted,
			HistorySyncStatus: models.CoexistenceSyncStatusCompleted,
			LifecycleStatus:   models.CoexistenceLifecycleStatusConnected,
			LifecycleMetadata: models.JSONB{},
			Version:           1,
		}).Error)
	}

	err := app.persistCoexistenceLifecycleBeforeAck(
		wabaID,
		1768477603,
		"PARTNER_REMOVED",
		"15550000009",
		nil,
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cannot safely route PARTNER_REMOVED")

	var disconnectedCount int64
	require.NoError(t, app.DB.Model(&models.WhatsAppAccount{}).Where(
		"organization_id = ? AND business_id = ? AND status = ?",
		org.ID,
		wabaID,
		"disconnected",
	).Count(&disconnectedCount).Error)
	assert.Zero(t, disconnectedCount, "ambiguous routing must roll back without mutating either account")
}

func whatsappIdentityFixture(t *testing.T) (*App, *models.WhatsAppAccount, *models.Contact) {
	t.Helper()
	app := newProcessorTestApp(t)
	_, account := createProcessorTestOrg(t, app)
	account.IsSMB = true
	account.BusinessID = testutil.NewTestGraphObjectID()
	require.NoError(t, app.DB.Save(account).Error)
	require.NoError(t, app.DB.Create(&models.WhatsAppCoexistenceState{
		ID:                uuid.New(),
		OrganizationID:    account.OrganizationID,
		WhatsAppAccountID: account.ID,
		OnboardingStatus:  models.CoexistenceOnboardingStatusConnected,
		OnboardingCycle:   1,
		LifecycleStatus:   models.CoexistenceLifecycleStatusConnected,
		LifecycleMetadata: models.JSONB{},
		Version:           1,
	}).Error)
	contact := testutil.CreateTestContact(t, app.DB, account.OrganizationID)
	contact.PhoneNumber = "60" + testutil.NewTestGraphObjectID()
	contact.WhatsAppAccount = account.Name
	require.NoError(t, app.DB.Save(contact).Error)
	return app, account, contact
}

func createWhatsAppIdentityActivity(t *testing.T, db *gorm.DB, account *models.WhatsAppAccount, message *models.Message) {
	t.Helper()
	require.NoError(t, db.Create(&models.CustomerActivityEvent{
		ID: uuid.New(), OrganizationID: account.OrganizationID, ContactID: message.ContactID,
		EventType: models.CustomerActivityMessageIncoming, Category: models.CustomerActivityCategoryMessage,
		Title: "Message received", ActorType: models.CustomerActivityActorContact,
		SourceObjectType: "message", SourceObjectID: &message.ID, OccurredAt: time.Now().UTC(),
		Metadata: models.JSONB{}, IdempotencyKey: "message-incoming:" + uuid.NewSHA1(account.ID, []byte(message.WhatsAppMessageID)).String(),
	}).Error)
}

func persistWhatsAppIdentityHistory(t *testing.T, app *App, account *models.WhatsAppAccount, contact *models.Contact, inbound IncomingTextMessage) models.Message {
	t.Helper()
	var stored *models.Message
	require.NoError(t, app.WithCommittedTenantApp(account.OrganizationID, func(scoped *App) error {
		if err := scoped.prepareWhatsAppMessageAuthority(account); err != nil {
			return err
		}
		var err error
		stored, _, err = scoped.persistCoexistenceMessage(account,
			coexistenceContactIdentity{Phone: contact.PhoneNumber}, CoexistenceMessage{IncomingTextMessage: inbound},
			models.DirectionIncoming, models.MessageStatusRead, models.JSONB{"coexistence_source": "history"}, false)
		return err
	}))
	require.NotNil(t, stored)
	return *stored
}

func renameWhatsAppIdentityAccount(t *testing.T, app *App, account *models.WhatsAppAccount) {
	t.Helper()
	account.Name = "renamed-" + uuid.NewString()[:8]
	require.NoError(t, app.DB.Model(&models.WhatsAppAccount{}).Where("id = ?", account.ID).Update("name", account.Name).Error)
	app.InvalidateWhatsAppAccountCache(account.PhoneID)
}

func TestWhatsAppMessageIdentityHistoryLiveOverlapAfterRename(t *testing.T) {
	for _, linked := range []bool{false, true} {
		for _, historyFirst := range []bool{false, true} {
			t.Run(fmt.Sprintf("linked=%t/history_first=%t", linked, historyFirst), func(t *testing.T) {
				app, account, contact := whatsappIdentityFixture(t)
				inbound := inboundContinuationTextMessage(t, "wamid.identity-order-"+uuid.NewString(), contact.PhoneNumber, "same delivery")
				var original models.Message
				if historyFirst {
					original = persistWhatsAppIdentityHistory(t, app, account, contact, inbound)
					var jobs int64
					require.NoError(t, app.DB.Model(&models.ScheduledJob{}).Where("aggregate_id = ? AND kind = ?", original.ID, inboundContinuationJobKind).Count(&jobs).Error)
					assert.Zero(t, jobs, "history import alone must not enqueue live processing")
				} else {
					work, duplicate, err := app.persistIncomingMessageBeforeAck(account.PhoneID, inbound, "Patient")
					require.NoError(t, err)
					require.False(t, duplicate)
					original = work.Persisted
				}
				if linked {
					require.NoError(t, persistLegacyWhatsAppMessageMirror(app.DB, account, original.ID))
					require.NoError(t, app.DB.First(&original, original.ID).Error)
					require.NotNil(t, original.InboxConversationID)
				} else if !historyFirst {
					// Live persistence normally mirrors after commit. Explicitly model
					// an unlinked legacy row while retaining its real live provenance.
					proof, err := app.resolveWhatsAppMessage(account, whatsAppMessageLookup{
						WAMID: inbound.ID, MessageID: original.ID, ContactID: contact.ID,
						Direction: models.DirectionIncoming,
					})
					require.NoError(t, err)
					require.True(t, proof.IncomingActivity)
					require.NotNil(t, proof.Continuation)
					require.NoError(t, app.DB.Model(&models.Message{}).Where(
						"organization_id = ? AND id = ?", account.OrganizationID, original.ID,
					).Update("inbox_conversation_id", nil).Error)
				}
				renameWhatsAppIdentityAccount(t, app, account)
				if !historyFirst {
					replayed := persistWhatsAppIdentityHistory(t, app, account, contact, inbound)
					assert.Equal(t, original.ID, replayed.ID)
				}
				proof, err := app.resolveWhatsAppMessage(account, whatsAppMessageLookup{
					WAMID: inbound.ID, MessageID: original.ID, ContactID: contact.ID,
					Direction: models.DirectionIncoming,
				})
				require.NoError(t, err)
				assert.Equal(t, linked, proof.Message.InboxConversationID != nil,
					"fixture must have the requested linked state at live replay")
				assert.Equal(t, !historyFirst, proof.IncomingActivity)
				assert.Equal(t, !historyFirst, proof.Continuation != nil)
				work, duplicate, err := app.persistIncomingMessageBeforeAck(account.PhoneID, inbound, "Patient")
				require.NoError(t, err)
				require.Equal(t, !historyFirst, duplicate,
					"history alone must not count as a processed live delivery")
				require.NotNil(t, work)
				assert.Equal(t, original.ID, work.Persisted.ID)
				assert.Equal(t, original.ContactID, work.Contact.ID)
				job := loadInboundContinuationJob(t, app, account.OrganizationID, original.ID)
				assert.Equal(t, models.ScheduledJobStatusPending, job.Status,
					"history-present is not the same as live processing completed")
				var processed int
				processor := NewInboundContinuationProcessor(app, time.Second)
				processor.process = func(_ context.Context, _ *App, loaded *persistedIncomingMessage) error {
					processed++
					assert.Equal(t, account.ID, loaded.Account.ID)
					assert.Equal(t, original.ID, loaded.Persisted.ID)
					return nil
				}
				require.NoError(t, processor.ProcessMessage(context.Background(), account.OrganizationID, original.ID))
				_, _, err = app.persistIncomingMessageBeforeAck(account.PhoneID, inbound, "Patient")
				require.NoError(t, err)
				require.NoError(t, processor.ProcessMessage(context.Background(), account.OrganizationID, original.ID))
				assert.Equal(t, 1, processed)
				var count int64
				require.NoError(t, app.DB.Model(&models.Message{}).Where("organization_id = ? AND whats_app_message_id = ?", account.OrganizationID, inbound.ID).Count(&count).Error)
				assert.EqualValues(t, 1, count)
			})
		}
	}
}

func TestWhatsAppMessageIdentityIgnoresProviderNeutralCollisionForResolutionAndReaction(t *testing.T) {
	app, account, contact := whatsappIdentityFixture(t)
	wamid := "wamid.provider-neutral-collision-" + uuid.NewString()
	channel := models.ChannelAccount{
		BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: account.OrganizationID,
		Channel: models.ChannelMessenger, Provider: "meta_graph", Name: "provider-neutral-collision",
		ExternalAccountID: "provider-neutral-collision-" + uuid.NewString(),
		Status:            models.ChannelAccountStatusActive, Capabilities: models.JSONB{}, Config: models.JSONB{}, Metadata: models.JSONB{},
	}
	require.NoError(t, app.DB.Create(&channel).Error)
	conversation := models.InboxConversation{
		BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: account.OrganizationID,
		ChannelAccountID: channel.ID, ContactID: contact.ID, Channel: models.ChannelMessenger,
		ExternalConversationID: "provider-neutral-collision-" + uuid.NewString(),
		Status:                 models.InboxConversationStatusOpen, OpenedAt: time.Now().UTC(),
		Config: models.JSONB{}, Metadata: models.JSONB{},
	}
	require.NoError(t, app.DB.Create(&conversation).Error)
	collision := models.Message{
		BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: account.OrganizationID,
		ContactID: contact.ID, WhatsAppAccount: account.Name, InboxConversationID: &conversation.ID,
		WhatsAppMessageID: wamid, Direction: models.DirectionOutgoing, MessageType: models.MessageTypeText,
		Content: "provider-neutral", Status: models.MessageStatusSent, Metadata: models.JSONB{},
	}
	require.NoError(t, app.DB.Create(&collision).Error)
	owner := models.Message{
		BaseModel:      models.BaseModel{ID: uuid.NewSHA1(account.ID, []byte("coexistence-message:"+wamid))},
		OrganizationID: account.OrganizationID, ContactID: contact.ID, WhatsAppAccount: account.Name,
		WhatsAppMessageID: wamid, Direction: models.DirectionOutgoing, MessageType: models.MessageTypeText,
		Content: "WhatsApp owner", Status: models.MessageStatusSent, Metadata: models.JSONB{},
	}
	require.NoError(t, app.DB.Create(&owner).Error,
		"a provider-neutral external ID must not reserve a WhatsApp WAMID")

	var resolved *whatsAppMessageResolution
	require.NoError(t, app.WithCommittedTenantApp(account.OrganizationID, func(scoped *App) error {
		if err := scoped.prepareWhatsAppMessageAuthority(account); err != nil {
			return err
		}
		var err error
		resolved, err = scoped.resolveWhatsAppMessage(account, whatsAppMessageLookup{
			WAMID: wamid, MessageID: owner.ID, ContactID: contact.ID,
			Identity:  coexistenceContactIdentity{Phone: contact.PhoneNumber},
			Direction: models.DirectionOutgoing,
		})
		return err
	}))
	require.NotNil(t, resolved)
	assert.Equal(t, owner.ID, resolved.Message.ID)

	app.handleIncomingReaction(account, contact.PhoneNumber, wamid, "👍", "Patient")
	var storedOwner, storedCollision models.Message
	require.NoError(t, app.DB.First(&storedOwner, "id = ?", owner.ID).Error)
	require.NoError(t, app.DB.First(&storedCollision, "id = ?", collision.ID).Error)
	assert.NotEmpty(t, storedOwner.Metadata["reactions"],
		"the authenticated reaction must mutate the exact WhatsApp owner")
	assert.NotContains(t, storedCollision.Metadata, "reactions",
		"a provider-neutral collision must never be selected by WhatsApp reaction fallback")
}

func TestWhatsAppMessageIdentityRenameAfterEnqueuePreservesTerminalJobs(t *testing.T) {
	for _, terminal := range []string{"completed", "manual_review"} {
		t.Run(terminal, func(t *testing.T) {
			app, account, contact := whatsappIdentityFixture(t)
			inbound := inboundContinuationTextMessage(t, "wamid.identity-job-"+uuid.NewString(), contact.PhoneNumber, "process once")
			work, _, err := app.persistIncomingMessageBeforeAck(account.PhoneID, inbound, "Patient")
			require.NoError(t, err)
			originalJob := loadInboundContinuationJob(t, app, account.OrganizationID, work.Persisted.ID)
			renameWhatsAppIdentityAccount(t, app, account)
			var calls int
			processor := NewInboundContinuationProcessor(app, time.Second)
			processor.process = func(_ context.Context, _ *App, loaded *persistedIncomingMessage) error {
				calls++
				assert.Equal(t, account.Name, loaded.Account.Name)
				assert.Equal(t, work.Persisted.ID, loaded.Persisted.ID)
				if terminal == "manual_review" {
					return &inboundContinuationManualReviewError{Reason: "synthetic ambiguous prior provider attempt"}
				}
				return nil
			}
			err = processor.ProcessMessage(context.Background(), account.OrganizationID, work.Persisted.ID)
			if terminal == "manual_review" {
				require.True(t, inboundContinuationRequiresManualReview(err))
			} else {
				require.NoError(t, err)
			}
			renameWhatsAppIdentityAccount(t, app, account)
			_, duplicate, err := app.persistIncomingMessageBeforeAck(account.PhoneID, inbound, "Patient")
			require.NoError(t, err)
			require.True(t, duplicate)
			require.NoError(t, processor.ProcessMessage(context.Background(), account.OrganizationID, work.Persisted.ID))
			stored := loadInboundContinuationJob(t, app, account.OrganizationID, work.Persisted.ID)
			assert.Equal(t, originalJob.ID, stored.ID)
			assert.Equal(t, originalJob.IdempotencyKey, stored.IdempotencyKey)
			if terminal == "manual_review" {
				assert.Equal(t, models.ScheduledJobStatusFailed, stored.Status)
				assert.Equal(t, true, stored.Payload["manual_review_required"])
			} else {
				assert.Equal(t, models.ScheduledJobStatusCompleted, stored.Status)
			}
			assert.Equal(t, 1, calls, "rename/redelivery must not resurrect terminal work")
		})
	}
}

func TestWhatsAppMessageIdentityRejectsUnprovenAndConflictingRows(t *testing.T) {
	for _, mismatch := range []string{"unproven_random_uuid", "metadata_only", "account", "tenant", "contact", "direction", "activity_target", "foreign_account_activity", "continuation_target", "reused_account_name"} {
		t.Run(mismatch, func(t *testing.T) {
			app, account, contact := whatsappIdentityFixture(t)
			inbound := inboundContinuationTextMessage(t, "wamid.identity-proof-"+uuid.NewString(), contact.PhoneNumber, "preserve me")
			original := models.Message{
				BaseModel:      models.BaseModel{ID: uuid.NewSHA1(account.ID, []byte("coexistence-message:"+inbound.ID))},
				OrganizationID: account.OrganizationID, ContactID: contact.ID, WhatsAppAccount: account.Name,
				WhatsAppMessageID: inbound.ID, Direction: models.DirectionIncoming, MessageType: models.MessageTypeText,
				Content: "preserve me", Status: models.MessageStatusRead, Metadata: models.JSONB{},
			}
			if mismatch == "unproven_random_uuid" || mismatch == "metadata_only" {
				original.ID = uuid.New()
				if mismatch == "metadata_only" {
					original.Metadata = models.JSONB{"legacy_account_id": account.ID.String(), "coexistence_source": "history"}
				}
			}
			require.NoError(t, app.DB.Create(&original).Error)
			require.NoError(t, app.DB.First(&original, original.ID).Error)
			lookup := whatsAppMessageLookup{WAMID: inbound.ID, MessageID: original.ID, ContactID: contact.ID, Direction: models.DirectionIncoming, Lock: true, RepairProjection: true}
			requestedAccount := *account
			switch mismatch {
			case "account", "reused_account_name":
				oldName := account.Name
				renameWhatsAppIdentityAccount(t, app, account)
				other := testutil.CreateTestWhatsAppAccount(t, app.DB, account.OrganizationID)
				if mismatch == "reused_account_name" {
					require.NoError(t, app.DB.Model(other).Update("name", oldName).Error)
					other.Name = oldName
				}
				requestedAccount = *other
			case "tenant":
				otherOrg := testutil.CreateTestOrganization(t, app.DB)
				requestedAccount = *testutil.CreateTestWhatsAppAccount(t, app.DB, otherOrg.ID)
			case "contact":
				lookup.ContactID = testutil.CreateTestContact(t, app.DB, account.OrganizationID).ID
			case "direction":
				lookup.Direction = models.DirectionOutgoing
			case "activity_target":
				otherMessage := original
				otherMessage.ID = uuid.New()
				otherMessage.WhatsAppMessageID = "wamid.other-" + uuid.NewString()
				require.NoError(t, app.DB.Create(&otherMessage).Error)
				otherMessage.WhatsAppMessageID = inbound.ID // key points to a different stored WAMID.
				createWhatsAppIdentityActivity(t, app.DB, account, &otherMessage)
			case "foreign_account_activity":
				other := testutil.CreateTestWhatsAppAccount(t, app.DB, account.OrganizationID)
				createWhatsAppIdentityActivity(t, app.DB, other, &original)
			case "continuation_target":
				otherID := uuid.New()
				require.NoError(t, app.DB.Create(&models.ScheduledJob{
					BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: account.OrganizationID,
					Kind: inboundContinuationJobKind, AggregateType: "message", AggregateID: &otherID,
					IdempotencyKey: "inbound-message-continuation:" + uuid.NewSHA1(account.ID, []byte(inbound.ID)).String(),
					RunAt:          time.Now(), Status: models.ScheduledJobStatusPending,
					Payload: models.JSONB{"phone_number_id": account.PhoneID, "wamid": inbound.ID, "message_id": otherID.String()},
				}).Error)
			}
			err := app.WithCommittedTenantApp(requestedAccount.OrganizationID, func(scoped *App) error {
				if err := scoped.prepareWhatsAppMessageAuthority(&requestedAccount); err != nil {
					return err
				}
				_, err := scoped.resolveWhatsAppMessage(&requestedAccount, lookup)
				return err
			})
			require.Error(t, err, "names/contact similarity cannot substitute stable ownership")
			var after models.Message
			require.NoError(t, app.DB.First(&after, original.ID).Error)
			assert.Equal(t, original, after, "rejected provenance must leave the row unchanged")
		})
	}
}

func TestWhatsAppMessageIdentityIndependentStableProvenance(t *testing.T) {
	for _, proof := range []string{"activity", "continuation", "deterministic", "joined_channel"} {
		t.Run(proof, func(t *testing.T) {
			app, account, contact := whatsappIdentityFixture(t)
			wamid := "wamid.identity-single-proof-" + uuid.NewString()
			message := models.Message{
				BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: account.OrganizationID,
				ContactID: contact.ID, WhatsAppAccount: account.Name, WhatsAppMessageID: wamid,
				Direction: models.DirectionIncoming, MessageType: models.MessageTypeText,
				Content: "stable provenance", Status: models.MessageStatusReceived, Metadata: models.JSONB{},
			}
			if proof == "deterministic" {
				message.ID = uuid.NewSHA1(account.ID, []byte("coexistence-message:"+wamid))
			}
			require.NoError(t, app.DB.Create(&message).Error)
			switch proof {
			case "activity":
				createWhatsAppIdentityActivity(t, app.DB, account, &message)
			case "continuation":
				require.NoError(t, app.DB.Create(&models.ScheduledJob{
					BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: account.OrganizationID,
					Kind: inboundContinuationJobKind, AggregateType: "message", AggregateID: &message.ID,
					IdempotencyKey: "inbound-message-continuation:" + uuid.NewSHA1(account.ID, []byte(wamid)).String(),
					RunAt:          time.Now(), Status: models.ScheduledJobStatusPending,
					Payload: models.JSONB{"phone_number_id": account.PhoneID, "wamid": wamid, "message_id": message.ID.String(),
						"message": map[string]any{"id": wamid, "from": contact.PhoneNumber, "type": "text"}},
				}).Error)
			case "joined_channel":
				require.NoError(t, persistLegacyWhatsAppMessageMirror(app.DB, account, message.ID))
				require.NoError(t, app.DB.First(&message, message.ID).Error)
				require.NotNil(t, message.InboxConversationID)
			}
			renameWhatsAppIdentityAccount(t, app, account)
			var resolved *whatsAppMessageResolution
			require.NoError(t, app.WithCommittedTenantApp(account.OrganizationID, func(scoped *App) error {
				if err := scoped.prepareWhatsAppMessageAuthority(account); err != nil {
					return err
				}
				var err error
				resolved, err = scoped.resolveWhatsAppMessage(account, whatsAppMessageLookup{
					WAMID: wamid, MessageID: message.ID, ContactID: contact.ID,
					Direction: models.DirectionIncoming, Lock: true, RepairProjection: true,
				})
				return err
			}))
			require.NotNil(t, resolved)
			assert.Equal(t, message.ID, resolved.Message.ID)
			assert.Equal(t, contact.ID, resolved.Contact.ID)
			assert.Equal(t, account.Name, resolved.Message.WhatsAppAccount)
			assert.Equal(t, proof == "activity", resolved.IncomingActivity)
			assert.Equal(t, proof == "continuation", resolved.Continuation != nil)
			var stored models.Message
			require.NoError(t, app.DB.First(&stored, message.ID).Error)
			assert.Equal(t, account.Name, stored.WhatsAppAccount, "repair only the proven row's persisted display projection")
		})
	}
}

func TestWhatsAppMessageIdentityConcurrentLiveDelivery(t *testing.T) {
	app, account, contact := whatsappIdentityFixture(t)
	inbound := inboundContinuationTextMessage(t, "wamid.identity-concurrent-"+uuid.NewString(), contact.PhoneNumber, "concurrent delivery")
	start := make(chan struct{})
	type result struct {
		work      *persistedIncomingMessage
		duplicate bool
		err       error
	}
	results := make(chan result, 2)
	var workers sync.WaitGroup
	for range 2 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			work, duplicate, err := app.persistIncomingMessageBeforeAck(account.PhoneID, inbound, "Patient")
			results <- result{work, duplicate, err}
		}()
	}
	close(start)
	workers.Wait()
	close(results)
	var messageID uuid.UUID
	duplicates := 0
	for result := range results {
		require.NoError(t, result.err, "a concurrent uniqueness loser must recover outside the aborted transaction")
		require.NotNil(t, result.work)
		if messageID == uuid.Nil {
			messageID = result.work.Persisted.ID
		}
		assert.Equal(t, messageID, result.work.Persisted.ID)
		if result.duplicate {
			duplicates++
		}
	}
	assert.Equal(t, 1, duplicates)
	var messages, jobs int64
	require.NoError(t, app.DB.Model(&models.Message{}).Where("organization_id = ? AND whats_app_message_id = ?", account.OrganizationID, inbound.ID).Count(&messages).Error)
	require.NoError(t, app.DB.Model(&models.ScheduledJob{}).Where("organization_id = ? AND kind = ? AND aggregate_id = ?", account.OrganizationID, inboundContinuationJobKind, messageID).Count(&jobs).Error)
	assert.EqualValues(t, 1, messages)
	assert.EqualValues(t, 1, jobs)
}

func TestWhatsAppMessageIdentitySignedLinkedRenameMutationsAndReply(t *testing.T) {
	app, account, original, echoBody, outgoingCount := mirroredCoexistenceEchoFixture(t)
	renameWhatsAppIdentityAccount(t, app, &account)
	sendSignedWebhook(t, app, echoBody)
	app.WaitForBackgroundTasks()
	var renamed models.Message
	require.NoError(t, app.DB.First(&renamed, original.ID).Error)
	assert.Equal(t, account.Name, renamed.WhatsAppAccount)
	assert.Equal(t, original.InboxConversationID, renamed.InboxConversationID)
	assert.EqualValues(t, 1, outgoingCount.Load(), "actual linked echo replay after rename is not a second send")

	webhookBody := func(field, array string, event map[string]any) []byte {
		t.Helper()
		encoded, err := json.Marshal(map[string]any{
			"object": "whatsapp_business_account",
			"entry": []any{map[string]any{"id": account.BusinessID, "changes": []any{map[string]any{
				"field": field, "value": map[string]any{
					"metadata": map[string]any{"phone_number_id": account.PhoneID, "display_phone_number": "15550783881"},
					array:      []any{event},
				},
			}}}},
		})
		require.NoError(t, err)
		return encoded
	}
	baseEvent := func(wamid, messageType string) map[string]any {
		return map[string]any{"id": wamid, "type": messageType, "from": "15550783881", "to": "60123456789", "timestamp": "1739231100"}
	}
	mediaEvent := baseEvent(original.WhatsAppMessageID, "video")
	mediaEvent["video"] = map[string]any{"id": "identity-linked-media", "mime_type": "video/mp4", "caption": "linked detail"}
	mediaBody := webhookBody("history", "messages", mediaEvent)
	sendSignedWebhook(t, app, mediaBody)
	require.NoError(t, app.DB.First(&renamed, original.ID).Error)
	assert.Equal(t, models.MessageTypeVideo, renamed.MessageType)
	assert.Equal(t, "identity-linked-media", renamed.Metadata[coexistenceMediaProviderIDMetadataKey])
	var mediaJob models.ScheduledJob
	require.NoError(t, app.DB.Where("organization_id = ? AND aggregate_id = ? AND kind = ?",
		account.OrganizationID, original.ID, coexistenceMediaHydrationJobKind).First(&mediaJob).Error)
	assert.Equal(t, account.ID.String(), mediaJob.Payload["account_id"])
	assert.Equal(t, original.ID.String(), mediaJob.Payload["message_id"])
	assert.Equal(t, account.OrganizationID, mediaJob.OrganizationID)
	require.NotNil(t, mediaJob.AggregateID)
	assert.Equal(t, original.ID, *mediaJob.AggregateID)

	edit := baseEvent("wamid.identity-linked-edit-"+uuid.NewString(), "edit")
	edit["edit"] = map[string]any{"original_message_id": original.WhatsAppMessageID,
		"message": map[string]any{"type": "text", "text": map[string]string{"body": "linked edit"}}}
	sendSignedWebhook(t, app, webhookBody("smb_message_echoes", "message_echoes", edit))
	require.NoError(t, app.DB.First(&renamed, original.ID).Error)
	assert.Equal(t, "linked edit", renamed.Content)
	assert.Equal(t, original.InboxConversationID, renamed.InboxConversationID)

	replyWAMID := "wamid.identity-linked-reply-" + uuid.NewString()
	reply := baseEvent(replyWAMID, "text")
	reply["text"] = map[string]string{"body": "reply to linked message"}
	reply["context"] = map[string]string{"id": original.WhatsAppMessageID}
	sendSignedWebhook(t, app, webhookBody("smb_message_echoes", "message_echoes", reply))
	app.WaitForBackgroundTasks()
	var storedReply models.Message
	require.NoError(t, app.DB.Where("organization_id = ? AND whats_app_message_id = ?", account.OrganizationID, replyWAMID).First(&storedReply).Error)
	require.NotNil(t, storedReply.ReplyToMessageID)
	assert.Equal(t, original.ID, *storedReply.ReplyToMessageID)
	require.NotNil(t, storedReply.InboxConversationID, "new reply must complete the actual mirror too")
	assert.Equal(t, original.InboxConversationID, storedReply.InboxConversationID)

	revoke := baseEvent("wamid.identity-linked-revoke-"+uuid.NewString(), "revoke")
	revoke["revoke"] = map[string]string{"original_message_id": original.WhatsAppMessageID}
	sendSignedWebhook(t, app, webhookBody("smb_message_echoes", "message_echoes", revoke))
	// Both delayed history details and edits retain the exact linked tombstone.
	sendSignedWebhook(t, app, mediaBody)
	sendSignedWebhook(t, app, webhookBody("smb_message_echoes", "message_echoes", edit))
	require.NoError(t, app.DB.First(&renamed, original.ID).Error)
	assert.Equal(t, original.ID, renamed.ID)
	assert.Equal(t, original.InboxConversationID, renamed.InboxConversationID)
	assert.Equal(t, "[Message deleted from WhatsApp Business App]", renamed.Content)
	assert.Equal(t, models.MessageTypeText, renamed.MessageType)
	assert.Equal(t, true, renamed.Metadata[coexistenceMediaRevokedMetadataKey])
	assert.Empty(t, renamed.MediaURL)
	assert.Empty(t, renamed.MediaMimeType)
	assert.NotContains(t, renamed.Metadata, coexistenceMediaProviderIDMetadataKey)
	var originalCount int64
	require.NoError(t, app.DB.Model(&models.Message{}).Where("organization_id = ? AND whats_app_message_id = ?", account.OrganizationID, original.WhatsAppMessageID).Count(&originalCount).Error)
	assert.EqualValues(t, 1, originalCount)
	app.WaitForBackgroundTasks()
	assert.EqualValues(t, 2, outgoingCount.Load(), "only the original echo and new reply emit outgoing events")
}

func TestWhatsAppMessageIdentityProviderCollisionNeverResends(t *testing.T) {
	app, account, contact := whatsappIdentityFixture(t)
	inbound := inboundContinuationTextMessage(t, "wamid.identity-provider-input-"+uuid.NewString(), contact.PhoneNumber, "reply once")
	work, _, err := app.persistIncomingMessageBeforeAck(account.PhoneID, inbound, "Patient")
	require.NoError(t, err)
	collisionWAMID := "wamid.identity-provider-collision-" + uuid.NewString()
	collision := models.Message{
		BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: account.OrganizationID,
		ContactID: contact.ID, WhatsAppAccount: account.Name, WhatsAppMessageID: collisionWAMID,
		Direction: models.DirectionOutgoing, MessageType: models.MessageTypeText, Content: "existing provider identity",
		Status: models.MessageStatusSent, Metadata: models.JSONB{},
	}
	require.NoError(t, app.DB.Create(&collision).Error)
	var providerCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/messages") {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		providerCalls.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"messages": []any{map[string]string{"id": collisionWAMID}}})
	}))
	t.Cleanup(server.Close)
	app.WhatsApp = whatsapp.NewWithBaseURL(app.Log, server.URL)

	// Pending-message creation succeeds. The real unique index rejects only
	// the provider result UPDATE, after this one HTTP attempt has occurred.
	execution := app.scopedApp(app.DB, account.OrganizationID)
	execution.inboundContinuation = &inboundContinuationExecution{
		OrganizationID: account.OrganizationID,
		ContactID:      contact.ID,
		MessageID:      work.Persisted.ID,
		WAMID:          inbound.ID,
	}
	err = execution.sendAndSaveTextMessage(account, contact, "send once despite local collision")
	require.True(t, inboundContinuationRequiresManualReview(err), "a post-provider persistence collision must remain terminal")
	assert.EqualValues(t, 1, providerCalls.Load())
	var action models.ScheduledJob
	require.NoError(t, app.DB.Where("organization_id = ? AND kind = ? AND aggregate_id = ?",
		account.OrganizationID, inboundContinuationActionJobKind, work.Persisted.ID).First(&action).Error)
	assert.Equal(t, models.ScheduledJobStatusFailed, action.Status)
	assert.Equal(t, inboundActionStateManualReview, action.Payload["state"])
	assert.Equal(t, true, action.Payload["manual_review_required"])
	renameWhatsAppIdentityAccount(t, app, account)
	replay := app.scopedApp(app.DB, account.OrganizationID)
	replay.inboundContinuation = &inboundContinuationExecution{
		OrganizationID: account.OrganizationID,
		ContactID:      contact.ID,
		MessageID:      work.Persisted.ID,
		WAMID:          inbound.ID,
	}
	err = replay.sendAndSaveTextMessage(account, contact, "send once despite local collision")
	require.True(t, inboundContinuationRequiresManualReview(err))
	assert.EqualValues(t, 1, providerCalls.Load(), "identity reconciliation must never call the provider again")
	var persistedCollision models.Message
	require.NoError(t, app.DB.First(&persistedCollision, collision.ID).Error)
	assert.Equal(t, collisionWAMID, persistedCollision.WhatsAppMessageID)
	assert.Equal(t, "existing provider identity", persistedCollision.Content)
}

func newWhatsAppIdentityIncomingMediaFixture(t *testing.T) (*App, *models.WhatsAppAccount, *persistedIncomingMessage, *coexistenceMediaServer) {
	t.Helper()
	app, account, message, provider := newCoexistenceMediaFixture(t)
	var contact models.Contact
	require.NoError(t, app.DB.First(&contact, message.ContactID).Error)
	message.Direction = models.DirectionIncoming
	message.Status = models.MessageStatusReceived
	message.Metadata["coexistence_source"] = "history"
	require.NoError(t, app.DB.Save(&message).Error)
	mediaID := coexistenceMediaMetadataString(message.Metadata, coexistenceMediaProviderIDMetadataKey)
	encoded, err := json.Marshal(map[string]any{
		"id": message.WhatsAppMessageID, "from": contact.PhoneNumber, "type": "image",
		"image": map[string]any{"id": mediaID, "mime_type": "image/jpeg", "caption": "original image"},
	})
	require.NoError(t, err)
	var inbound IncomingTextMessage
	require.NoError(t, json.Unmarshal(encoded, &inbound))
	// Keep the actual imported deterministic row and establish the same stable
	// live activity/job proofs that ordinary delivery adds before hydration.
	createWhatsAppIdentityActivity(t, app.DB, &account, &message)
	require.NoError(t, app.ensureInboundContinuationJob(&account, &message, inbound, contact.ProfileName))
	job := loadInboundContinuationJob(t, app, account.OrganizationID, message.ID)
	_, err = validateInboundContinuationJobProof(&job, &account, message.ID, inbound.ID)
	require.NoError(t, err)
	work := &persistedIncomingMessage{
		OrganizationID: account.OrganizationID, PhoneNumberID: account.PhoneID,
		Account: account, Contact: contact, Message: inbound, Persisted: message,
		Extracted: app.extractMessageContentForPersistence(inbound),
	}
	return app, &account, work, provider
}

func TestWhatsAppMessageIdentityIncomingHydrationRejectsEditedMediaType(t *testing.T) {
	app, account, work, provider := newWhatsAppIdentityIncomingMediaFixture(t)
	var edit CoexistenceMessage
	require.NoError(t, json.Unmarshal([]byte(`{"id":"edit-before-live-hydration","type":"edit","edit":{"original_message_id":"original","message":{"type":"text","text":{"body":"image replaced by text"}}}}`), &edit))
	edit.From, edit.Edit.OriginalMessageID = work.Contact.PhoneNumber, work.Message.ID
	require.NoError(t, app.WithCommittedTenantApp(account.OrganizationID, func(scoped *App) error {
		return scoped.persistCoexistenceEdit(account, edit, nil, models.DirectionIncoming, models.MessageStatusReceived, "history", false)
	}))

	err := app.hydratePersistedIncomingMedia(context.Background(), work, account)
	require.Error(t, err, "the queued image cannot hydrate a message subsequently edited to text")
	assert.Zero(t, provider.metadataCalls.Load())
	assert.Zero(t, provider.downloadCalls.Load())
	var stored models.Message
	require.NoError(t, app.DB.First(&stored, work.Persisted.ID).Error)
	assert.Equal(t, models.MessageTypeText, stored.MessageType)
	assert.Equal(t, "image replaced by text", stored.Content)
	assert.Empty(t, stored.MediaURL)
	assert.Empty(t, work.Extracted.Media.MediaURL)
}

func TestWhatsAppMessageIdentityIncomingHydrationDiscardsStaleUpload(t *testing.T) {
	for _, change := range []string{"revoke", "supersede", "credential_rotation", "disconnect"} {
		t.Run(change, func(t *testing.T) {
			app, account, work, provider := newWhatsAppIdentityIncomingMediaFixture(t)
			store := newBlockingCoexistenceMediaStore()
			defer store.release()
			app.Config.Storage.Type = "s3"
			app.ObjectStore = store
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			app.DB = app.DB.WithContext(ctx)
			mutationAccount := *account
			messageID, wamid, phone := work.Persisted.ID, work.Message.ID, work.Contact.PhoneNumber
			result := make(chan error, 1)
			go func() {
				result <- app.hydratePersistedIncomingMedia(ctx, work, account)
			}()
			select {
			case <-store.putStarted:
			case err := <-result:
				t.Fatalf("hydration stopped before the upload barrier: %v", err)
			case <-ctx.Done():
				t.Fatal("incoming media upload did not reach the barrier")
			}
			// Provider download has finished; mutate authority/content while the
			// new random-key upload is still private to this hydration attempt.
			switch change {
			case "revoke":
				var event CoexistenceMessage
				require.NoError(t, json.Unmarshal([]byte(`{"id":"revoke-during-live-hydration","type":"revoke","revoke":{"original_message_id":"original"}}`), &event))
				event.From, event.Revoke.OriginalMessageID = phone, wamid
				require.NoError(t, app.WithCommittedTenantApp(mutationAccount.OrganizationID, func(scoped *App) error {
					return scoped.persistCoexistenceRevoke(&mutationAccount, event, nil, models.DirectionIncoming, models.MessageStatusReceived, "history", false)
				}))
			case "supersede":
				var event CoexistenceMessage
				require.NoError(t, json.Unmarshal([]byte(`{"id":"edit-during-live-hydration","type":"edit","edit":{"original_message_id":"original","message":{"type":"image","image":{"id":"replacement-live-media","mime_type":"image/jpeg","caption":"replacement image"}}}}`), &event))
				event.From, event.Edit.OriginalMessageID = phone, wamid
				require.NoError(t, app.WithCommittedTenantApp(mutationAccount.OrganizationID, func(scoped *App) error {
					return scoped.persistCoexistenceEdit(&mutationAccount, event, nil, models.DirectionIncoming, models.MessageStatusReceived, "history", false)
				}))
			case "credential_rotation":
				require.NoError(t, app.DB.Model(&models.WhatsAppAccount{}).Where(
					"organization_id = ? AND id = ?", mutationAccount.OrganizationID, mutationAccount.ID,
				).Update("access_token", "rotated-live-media-token-"+uuid.NewString()).Error)
			case "disconnect":
				require.NoError(t, app.DB.Model(&models.WhatsAppAccount{}).Where(
					"organization_id = ? AND id = ?", mutationAccount.OrganizationID, mutationAccount.ID,
				).Update("status", "disconnected").Error)
			}
			store.release()
			select {
			case err := <-result:
				if change == "revoke" {
					require.NoError(t, err)
				} else {
					require.Error(t, err, "stale content or credentials must be rejected after cleanup")
				}
			case <-ctx.Done():
				t.Fatal("incoming media discard did not finish")
			}
			var stored models.Message
			require.NoError(t, app.DB.First(&stored, messageID).Error)
			assert.Empty(t, stored.MediaURL, "old bytes must never be attached to the current row")
			assert.Empty(t, work.Extracted.Media.MediaURL)
			assert.Empty(t, work.Persisted.MediaURL)
			switch change {
			case "revoke":
				assert.Equal(t, true, stored.Metadata[coexistenceMediaRevokedMetadataKey])
				assert.Equal(t, models.MessageTypeText, stored.MessageType)
				assert.Empty(t, stored.Metadata[coexistenceMediaProviderIDMetadataKey])
			case "supersede":
				assert.Equal(t, "replacement-live-media", stored.Metadata[coexistenceMediaProviderIDMetadataKey])
				assert.Equal(t, "replacement image", stored.Content)
			}
			objects, deletes := store.counts()
			assert.Zero(t, objects, "the confirmed unreferenced upload must be removed")
			assert.Equal(t, 1, deletes, "cleanup must delete only the new attempt's object")
			assert.EqualValues(t, 1, provider.metadataCalls.Load())
			assert.EqualValues(t, 1, provider.downloadCalls.Load())
		})
	}
}

func TestCanEnrichProvenWhatsAppMessageBSUID(t *testing.T) {
	for _, tc := range []struct {
		name, canonicalPhone, existingBSUID, phone, userID string
		want                                               bool
	}{
		{"exact_phone", "60123456789", "", "60123456789", "US.new", true},
		{"normalized_phone", "+60123456789", "", "60123456789", "US.new", true},
		{"absent_phone", "60123456789", "", "", "US.new", false},
		{"different_phone", "60123456789", "", "60129999999", "US.new", false},
		{"placeholder", "bsuid:US.old", "", "bsuid:US.old", "US.new", false},
		{"existing_bsuid", "60123456789", "US.old", "60123456789", "US.new", false},
		{"no_optional_id", "60123456789", "", "60123456789", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			contact := &models.Contact{PhoneNumber: tc.canonicalPhone, BSUID: tc.existingBSUID}
			assert.Equal(t, tc.want, canEnrichProvenWhatsAppMessageBSUID(contact, coexistenceContactIdentity{Phone: tc.phone, UserID: tc.userID}))
		})
	}
	assert.False(t, canEnrichProvenWhatsAppMessageBSUID(nil, coexistenceContactIdentity{Phone: "60123456789", UserID: "US.new"}))
}

func TestWhatsAppIdentityReviewVisibleIntakeTargetRequiresExactReviewedMember(t *testing.T) {
	t.Parallel()

	directID := uuid.MustParse("00000000-0000-4000-8000-000000000101")
	otherID := uuid.MustParse("00000000-0000-4000-8000-000000000102")
	missingID := uuid.MustParse("00000000-0000-4000-8000-000000000103")
	tests := []struct {
		name       string
		route      *uuid.UUID
		candidates []WhatsAppIdentityReviewCandidate
		want       uuid.UUID
		visible    bool
	}{
		{
			name:  "direct_phone_conflict_route",
			route: &directID,
			candidates: []WhatsAppIdentityReviewCandidate{{
				ContactID: directID, SelectorReasons: models.WhatsAppIdentityReviewSelectorPrimaryBSUID,
			}, {ContactID: otherID, SelectorReasons: models.WhatsAppIdentityReviewSelectorPhone}},
			want: directID, visible: true,
		},
		{
			name:  "reviewed_non_direct_member_route",
			route: &otherID,
			candidates: []WhatsAppIdentityReviewCandidate{
				{ContactID: directID, SelectorReasons: models.WhatsAppIdentityReviewSelectorPrimaryBSUID},
				{ContactID: otherID, SelectorReasons: models.WhatsAppIdentityReviewSelectorPhone},
			},
			want: otherID, visible: true,
		},
		{
			name:  "missing_route_stages",
			route: &missingID,
			candidates: []WhatsAppIdentityReviewCandidate{
				{ContactID: directID, SelectorReasons: models.WhatsAppIdentityReviewSelectorPrimaryBSUID},
				{ContactID: otherID, SelectorReasons: models.WhatsAppIdentityReviewSelectorPhone},
			},
		},
		{
			name:  "duplicate_candidate_stages",
			route: &directID,
			candidates: []WhatsAppIdentityReviewCandidate{
				{ContactID: directID, SelectorReasons: models.WhatsAppIdentityReviewSelectorPrimaryBSUID},
				{ContactID: directID, SelectorReasons: models.WhatsAppIdentityReviewSelectorPhone},
			},
		},
		{
			name: "nil_route_stages",
			candidates: []WhatsAppIdentityReviewCandidate{{
				ContactID: otherID, SelectorReasons: models.WhatsAppIdentityReviewSelectorParentBSUID | models.WhatsAppIdentityReviewSelectorPhone,
			}},
		},
		{
			name:  "invalid_reason_stages",
			route: &directID,
			candidates: []WhatsAppIdentityReviewCandidate{{
				ContactID: directID, SelectorReasons: models.WhatsAppIdentityReviewSelectorReasonMask | 8,
			}},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, visible := whatsappIdentityReviewVisibleIntakeTarget(test.route, test.candidates)
			assert.Equal(t, test.visible, visible)
			assert.Equal(t, test.want, got)
		})
	}
}

func TestWhatsAppIdentityReviewAdmissionDigestsBindRawBodyAndPersistedCycle(t *testing.T) {
	t.Parallel()

	organizationID := uuid.MustParse("00000000-0000-4000-8000-000000000201")
	accountID := uuid.MustParse("00000000-0000-4000-8000-000000000202")
	verified := whatsAppIdentityReviewVerifiedBinding{
		Protocol:          models.WhatsAppIdentityReviewInboundProtocol,
		OrganizationID:    organizationID,
		WhatsAppAccountID: accountID,
		WAMID:             "wamid.digest-test",
		WebhookBodySHA256: strings.Repeat("a", 64),
	}
	first, err := whatsappIdentityReviewBindingDigest(verified)
	require.NoError(t, err)
	second, err := whatsappIdentityReviewBindingDigest(verified)
	require.NoError(t, err)
	assert.Equal(t, first, second)
	assert.Len(t, first, sha256.Size*2)

	changedBody := verified
	changedBody.WebhookBodySHA256 = strings.Repeat("b", 64)
	changed, err := whatsappIdentityReviewBindingDigest(changedBody)
	require.NoError(t, err)
	assert.NotEqual(t, first, changed)

	selector := whatsAppIdentityReviewSelectorBinding{
		Protocol: models.WhatsAppIdentityReviewInboundProtocol, OrganizationID: organizationID,
		WhatsAppAccountID: accountID, OnboardingCycle: 7, WAMID: verified.WAMID,
		DirectPrimaryBSUID: "US.direct", ParentBSUID: "US.parent", Phone: "60123456789",
	}
	cycleSeven, err := whatsappIdentityReviewBindingDigest(selector)
	require.NoError(t, err)
	selector.OnboardingCycle++
	cycleEight, err := whatsappIdentityReviewBindingDigest(selector)
	require.NoError(t, err)
	assert.NotEqual(t, cycleSeven, cycleEight)
}

func TestWhatsAppIdentityReviewPhoneConflictPersistsVisibleSuppressedWinner(t *testing.T) {
	app, account, direct := whatsappIdentityFixture(t)
	direct.BSUID = "US.direct-" + uuid.NewString()
	require.NoError(t, app.DB.Model(&models.Contact{}).Where(
		"organization_id = ? AND id = ?",
		account.OrganizationID,
		direct.ID,
	).Update("bs_uid", direct.BSUID).Error)
	phoneOwner := testutil.CreateTestContactWith(
		t,
		app.DB,
		account.OrganizationID,
		testutil.WithPhoneNumber("+60"+testutil.NewTestGraphObjectID()[:10]),
	)
	inbound := inboundContinuationTextMessage(
		t,
		"wamid.review-phone-"+uuid.NewString(),
		phoneOwner.PhoneNumber,
		"keep visible but suppress AI",
	)
	inbound.FromUserID = direct.BSUID

	work, duplicate, err := app.persistAuthenticatedIncomingMessageBeforeAck(
		account.PhoneID,
		inbound,
		"Direct owner",
		strings.Repeat("a", sha256.Size*2),
	)
	require.NoError(t, err)
	require.False(t, duplicate)
	require.NotNil(t, work)
	assert.Equal(t, direct.ID, work.Persisted.ContactID)
	assert.Equal(t, true, work.Persisted.Metadata[incomingAutomaticAISuppressedKey])
	assert.Equal(t, "whatsapp_identity_review:phone_selector_conflict", work.Persisted.Metadata[incomingAutomaticAISuppressionReasonKey])

	var holds []models.WhatsAppIdentityReviewHold
	require.NoError(t, app.DB.Where("organization_id = ?", account.OrganizationID).Find(&holds).Error)
	require.Len(t, holds, 1)
	assert.Equal(t, holds[0].ID.String(), work.Persisted.Metadata[incomingIdentityReviewHoldIDKey])
	assert.Equal(t, "phone_selector_conflict", work.Persisted.Metadata[incomingIdentityReviewReasonKey])
	assert.Equal(t, incomingIdentityReviewRouteHeldDirect, work.Persisted.Metadata[incomingIdentityReviewRouteModeKey])
	selectorDigest, err := whatsappIdentityReviewBindingDigest(whatsAppIdentityReviewSelectorBinding{
		Protocol: models.WhatsAppIdentityReviewInboundProtocol, OrganizationID: account.OrganizationID,
		WhatsAppAccountID: account.ID, OnboardingCycle: holds[0].OnboardingCycle, WAMID: inbound.ID,
		DirectPrimaryBSUID: inbound.FromUserID, ParentBSUID: inbound.FromParentUserID,
		Phone: normalizeIdentityReviewPhone(inbound.From),
	})
	require.NoError(t, err)
	assert.Equal(t, selectorDigest, work.Persisted.Metadata[incomingIdentityReviewSelectorDigestKey])
	var members []models.WhatsAppIdentityReviewMember
	require.NoError(t, app.DB.Where(
		"organization_id = ? AND hold_id = ?",
		account.OrganizationID,
		holds[0].ID,
	).Order("contact_id").Find(&members).Error)
	require.Len(t, members, 2)
	assert.ElementsMatch(t, []uuid.UUID{direct.ID, phoneOwner.ID}, []uuid.UUID{members[0].ContactID, members[1].ContactID})

	var stagedCount, messageCount, continuationCount int64
	require.NoError(t, app.DB.Model(&models.InboundEvent{}).Where(
		"organization_id = ? AND protocol = ? AND provider_event_id = ?",
		account.OrganizationID,
		models.WhatsAppIdentityReviewInboundProtocol,
		inbound.ID,
	).Count(&stagedCount).Error)
	require.NoError(t, app.DB.Model(&models.Message{}).Where(
		"organization_id = ? AND whats_app_message_id = ?",
		account.OrganizationID,
		inbound.ID,
	).Count(&messageCount).Error)
	require.NoError(t, app.DB.Model(&models.ScheduledJob{}).Where(
		"organization_id = ? AND kind = ? AND aggregate_id = ?",
		account.OrganizationID,
		inboundContinuationJobKind,
		work.Persisted.ID,
	).Count(&continuationCount).Error)
	assert.Zero(t, stagedCount)
	assert.EqualValues(t, 1, messageCount)
	assert.EqualValues(t, 1, continuationCount)

	beforeMessage := work.Persisted
	replayed, replayDuplicate, err := app.persistAuthenticatedIncomingMessageBeforeAck(
		account.PhoneID,
		inbound,
		"Changed profile must not matter",
		strings.Repeat("b", sha256.Size*2),
	)
	require.NoError(t, err)
	assert.True(t, replayDuplicate)
	assert.Nil(t, replayed)
	var afterMessage models.Message
	require.NoError(t, app.DB.First(&afterMessage, beforeMessage.ID).Error)
	assert.Equal(t, beforeMessage, afterMessage)

	identity := coexistenceContactIdentity{
		Phone: inbound.From, UserID: inbound.FromUserID, ParentUserID: inbound.FromParentUserID,
	}
	for _, mutation := range []struct {
		name   string
		mutate func(*models.Message, *coexistenceContactIdentity)
	}{
		{name: "missing_hold", mutate: func(message *models.Message, _ *coexistenceContactIdentity) {
			delete(message.Metadata, incomingIdentityReviewHoldIDKey)
		}},
		{name: "foreign_hold", mutate: func(message *models.Message, _ *coexistenceContactIdentity) {
			message.Metadata[incomingIdentityReviewHoldIDKey] = uuid.NewString()
		}},
		{name: "wrong_mode", mutate: func(message *models.Message, _ *coexistenceContactIdentity) {
			message.Metadata[incomingIdentityReviewRouteModeKey] = incomingIdentityReviewRouteReviewedFuture
		}},
		{name: "unknown_reason", mutate: func(message *models.Message, _ *coexistenceContactIdentity) {
			message.Metadata[incomingIdentityReviewReasonKey] = "unknown"
		}},
		{name: "lost_suppression", mutate: func(message *models.Message, _ *coexistenceContactIdentity) {
			message.Metadata[incomingAutomaticAISuppressedKey] = false
		}},
		{name: "wrong_selector_digest", mutate: func(message *models.Message, _ *coexistenceContactIdentity) {
			message.Metadata[incomingIdentityReviewSelectorDigestKey] = strings.Repeat("f", sha256.Size*2)
		}},
		{name: "wrong_contact", mutate: func(message *models.Message, _ *coexistenceContactIdentity) {
			message.ContactID = uuid.New()
		}},
		{name: "wrong_wamid", mutate: func(message *models.Message, _ *coexistenceContactIdentity) {
			message.WhatsAppMessageID = "wamid.changed"
		}},
		{name: "wrong_raw_selector", mutate: func(_ *models.Message, identity *coexistenceContactIdentity) {
			identity.Phone = "60111111111"
		}},
	} {
		t.Run("reject_"+mutation.name, func(t *testing.T) {
			candidate := beforeMessage
			candidate.Metadata = cloneMessageMetadata(beforeMessage.Metadata)
			candidateIdentity := identity
			mutation.mutate(&candidate, &candidateIdentity)
			accepted, proofErr := app.validatePersistedWhatsAppIdentityReviewRoute(account, &candidate, candidateIdentity)
			assert.False(t, accepted)
			require.Error(t, proofErr)
		})
	}

	job := loadInboundContinuationJob(t, app, account.OrganizationID, beforeMessage.ID)
	originalPayloadJSON, err := json.Marshal(job.Payload)
	require.NoError(t, err)
	var tamperedPayload models.JSONB
	require.NoError(t, json.Unmarshal(originalPayloadJSON, &tamperedPayload))
	rawMessage, ok := tamperedPayload["message"].(map[string]any)
	require.True(t, ok)
	rawMessage["from"] = "60111111111"
	require.NoError(t, app.DB.Model(&models.ScheduledJob{}).Where(
		"id = ? AND organization_id = ?", job.ID, account.OrganizationID,
	).Update("payload", tamperedPayload).Error)
	currentAccount := *account
	err = app.WithCommittedTenantApp(account.OrganizationID, func(scoped *App) error {
		if prepareErr := scoped.prepareWhatsAppMessageAuthority(&currentAccount); prepareErr != nil {
			return prepareErr
		}
		_, resolveErr := scoped.resolveWhatsAppMessage(&currentAccount, whatsAppMessageLookup{
			WAMID: inbound.ID, MessageID: beforeMessage.ID, ContactID: direct.ID,
			Direction: models.DirectionIncoming, Identity: identity, Lock: true, RepairProjection: true,
		})
		return resolveErr
	})
	require.ErrorContains(t, err, "reviewed continuation selector proof changed")
	var originalPayload models.JSONB
	require.NoError(t, json.Unmarshal(originalPayloadJSON, &originalPayload))
	require.NoError(t, app.DB.Model(&models.ScheduledJob{}).Where(
		"id = ? AND organization_id = ?", job.ID, account.OrganizationID,
	).Update("payload", originalPayload).Error)

	processor := NewInboundContinuationProcessor(app, time.Hour)
	require.NoError(t, processor.ProcessMessage(context.Background(), account.OrganizationID, beforeMessage.ID))
	completed := loadInboundContinuationJob(t, app, account.OrganizationID, beforeMessage.ID)
	assert.Equal(t, models.ScheduledJobStatusCompleted, completed.Status)
	assert.Equal(t, 1, completed.Attempts)
	var holdCount int64
	require.NoError(t, app.DB.Model(&models.WhatsAppIdentityReviewHold{}).Where(
		"organization_id = ?",
		account.OrganizationID,
	).Count(&holdCount).Error)
	assert.EqualValues(t, 1, holdCount)
}

func TestWhatsAppIdentityReviewAmbiguityStagesContactFreeAndKeepsCrossStoreWinner(t *testing.T) {
	for _, variant := range []string{"primary_parent_conflict", "no_owner"} {
		t.Run(variant, func(t *testing.T) {
			app, account, direct := whatsappIdentityFixture(t)
			direct.BSUID = "US.direct-" + uuid.NewString()
			require.NoError(t, app.DB.Model(&models.Contact{}).Where(
				"organization_id = ? AND id = ?",
				account.OrganizationID,
				direct.ID,
			).Update("bs_uid", direct.BSUID).Error)
			parent := testutil.CreateTestContact(t, app.DB, account.OrganizationID)
			parent.BSUID = "US.parent-" + uuid.NewString()
			require.NoError(t, app.DB.Model(&models.Contact{}).Where(
				"organization_id = ? AND id = ?",
				account.OrganizationID,
				parent.ID,
			).Update("bs_uid", parent.BSUID).Error)

			inbound := inboundContinuationTextMessage(
				t,
				"wamid.review-stage-"+uuid.NewString(),
				"",
				"contact-free review",
			)
			switch variant {
			case "primary_parent_conflict":
				inbound.FromUserID = direct.BSUID
				inbound.FromParentUserID = parent.BSUID
			case "no_owner":
				inbound.FromUserID = "US.unknown-" + uuid.NewString()
			}
			var contactsBefore int64
			require.NoError(t, app.DB.Model(&models.Contact{}).Where(
				"organization_id = ?",
				account.OrganizationID,
			).Count(&contactsBefore).Error)

			work, duplicate, err := app.persistAuthenticatedIncomingMessageBeforeAck(
				account.PhoneID,
				inbound,
				"Untrusted profile",
				strings.Repeat("c", sha256.Size*2),
			)
			require.NoError(t, err)
			assert.False(t, duplicate)
			assert.Nil(t, work)

			var contactsAfter, messages int64
			require.NoError(t, app.DB.Model(&models.Contact{}).Where(
				"organization_id = ?",
				account.OrganizationID,
			).Count(&contactsAfter).Error)
			require.NoError(t, app.DB.Model(&models.Message{}).Where(
				"organization_id = ? AND whats_app_message_id = ?",
				account.OrganizationID,
				inbound.ID,
			).Count(&messages).Error)
			assert.Equal(t, contactsBefore, contactsAfter)
			assert.Zero(t, messages)

			var events []models.InboundEvent
			require.NoError(t, app.DB.Where(
				"organization_id = ? AND protocol = ? AND provider_event_id = ?",
				account.OrganizationID,
				models.WhatsAppIdentityReviewInboundProtocol,
				inbound.ID,
			).Find(&events).Error)
			require.Len(t, events, 1)
			assert.NotNil(t, events[0].ReviewHoldID)
			assert.NotContains(t, events[0].Payload, "from")
			assert.NotContains(t, events[0].Payload, "from_user_id")
			assert.NotContains(t, events[0].Payload, "from_parent_user_id")
			assert.NotContains(t, events[0].Payload, "profile_name")

			// A history/echo replay may not promote the reserved winner or create
			// a Contact, even when it carries a newly addressable phone.
			echo := CoexistenceMessage{IncomingTextMessage: inbound}
			echo.From = "6012" + testutil.NewTestGraphObjectID()
			echo.To = "15550783881"
			require.NoError(t, app.persistMessageEchoesBeforeAck(
				account.PhoneID,
				[]CoexistenceMessage{echo},
				nil,
			))
			require.NoError(t, app.DB.Model(&models.Contact{}).Where(
				"organization_id = ?",
				account.OrganizationID,
			).Count(&contactsAfter).Error)
			assert.Equal(t, contactsBefore, contactsAfter)
			require.NoError(t, app.DB.Model(&models.Message{}).Where(
				"organization_id = ? AND whats_app_message_id = ?",
				account.OrganizationID,
				inbound.ID,
			).Count(&messages).Error)
			assert.Zero(t, messages)

			replayWork, replayDuplicate, err := app.persistAuthenticatedIncomingMessageBeforeAck(
				account.PhoneID,
				inbound,
				"Another profile",
				strings.Repeat("d", sha256.Size*2),
			)
			require.NoError(t, err)
			assert.True(t, replayDuplicate)
			assert.Nil(t, replayWork)
		})
	}
}

func TestWhatsAppIdentityReviewDecisionRoutesFuturePrimaryParentConflict(t *testing.T) {
	app, account, direct := whatsappIdentityFixture(t)
	direct.BSUID = "US.direct-" + uuid.NewString()
	require.NoError(t, app.DB.Model(&models.Contact{}).Where(
		"organization_id = ? AND id = ?", account.OrganizationID, direct.ID,
	).Update("bs_uid", direct.BSUID).Error)
	parent := testutil.CreateTestContact(t, app.DB, account.OrganizationID)
	parent.BSUID = "US.parent-" + uuid.NewString()
	require.NoError(t, app.DB.Model(&models.Contact{}).Where(
		"organization_id = ? AND id = ?", account.OrganizationID, parent.ID,
	).Update("bs_uid", parent.BSUID).Error)

	first := inboundContinuationTextMessage(
		t,
		"wamid.review-decision-"+uuid.NewString(),
		"",
		"stage the authenticated conflict",
	)
	first.FromUserID = direct.BSUID
	first.FromParentUserID = parent.BSUID
	work, duplicate, err := app.persistAuthenticatedIncomingMessageBeforeAck(
		account.PhoneID,
		first,
		"Untrusted profile",
		strings.Repeat("a", sha256.Size*2),
	)
	require.NoError(t, err)
	require.False(t, duplicate)
	require.Nil(t, work)

	var holds []models.WhatsAppIdentityReviewHold
	require.NoError(t, app.DB.Where(
		"organization_id = ? AND whats_app_account_id = ?", account.OrganizationID, account.ID,
	).Find(&holds).Error)
	require.Len(t, holds, 1)
	require.Equal(t, models.WhatsAppIdentityReviewDispositionOpen, holds[0].Disposition)
	resolver := createWhatsAppIdentityReviewResolver(t, app.DB, account.OrganizationID)
	decision := decideWhatsAppIdentityReviewForTest(
		t, app, app.DB, account.OrganizationID, resolver.ID, holds[0].ID, parent.ID,
	)
	require.Equal(t, models.WhatsAppIdentityReviewDispositionFutureRouting, decision.Snapshot.Disposition)
	require.NotNil(t, decision.Snapshot.DecisionTargetContactID)
	require.Equal(t, parent.ID, *decision.Snapshot.DecisionTargetContactID)

	future := first
	future.ID = "wamid.review-future-" + uuid.NewString()
	future.Text.Body = "route the reviewed future message"
	work, duplicate, err = app.persistAuthenticatedIncomingMessageBeforeAck(
		account.PhoneID,
		future,
		"Reviewed profile",
		strings.Repeat("b", sha256.Size*2),
	)
	require.NoError(t, err)
	require.False(t, duplicate)
	require.NotNil(t, work)
	require.Equal(t, parent.ID, work.Persisted.ContactID)
	require.Equal(t, holds[0].ID.String(), work.Persisted.Metadata[incomingIdentityReviewHoldIDKey])
	require.Equal(t, "reviewed_future_route", work.Persisted.Metadata[incomingIdentityReviewReasonKey])
	require.Equal(t, incomingIdentityReviewRouteReviewedFuture, work.Persisted.Metadata[incomingIdentityReviewRouteModeKey])

	var stagedFirst, stagedFuture, persistedFuture, continuationCount int64
	require.NoError(t, app.DB.Model(&models.InboundEvent{}).Where(
		"organization_id = ? AND protocol = ? AND provider_event_id = ?",
		account.OrganizationID, models.WhatsAppIdentityReviewInboundProtocol, first.ID,
	).Count(&stagedFirst).Error)
	require.NoError(t, app.DB.Model(&models.InboundEvent{}).Where(
		"organization_id = ? AND protocol = ? AND provider_event_id = ?",
		account.OrganizationID, models.WhatsAppIdentityReviewInboundProtocol, future.ID,
	).Count(&stagedFuture).Error)
	require.NoError(t, app.DB.Model(&models.Message{}).Where(
		"organization_id = ? AND whats_app_message_id = ?", account.OrganizationID, future.ID,
	).Count(&persistedFuture).Error)
	require.NoError(t, app.DB.Model(&models.ScheduledJob{}).Where(
		"organization_id = ? AND kind = ? AND aggregate_id = ?",
		account.OrganizationID, inboundContinuationJobKind, work.Persisted.ID,
	).Count(&continuationCount).Error)
	require.EqualValues(t, 1, stagedFirst)
	require.Zero(t, stagedFuture)
	require.EqualValues(t, 1, persistedFuture)
	require.EqualValues(t, 1, continuationCount)

	// The identity-review assertion continues through the durable inbound worker.
	// Give that worker the same explicit account policy required in production;
	// disabled chatbot settings make the post-routing action deterministic and
	// avoid coupling this ownership test to an unrelated chatbot response path.
	require.NoError(t, app.DB.Create(&models.ChatbotSettings{
		BaseModel:       models.BaseModel{ID: uuid.New()},
		OrganizationID:  account.OrganizationID,
		WhatsAppAccount: account.Name,
		IsEnabled:       false,
		AI:              models.AIConfig{Enabled: false},
	}).Error)

	processor := NewInboundContinuationProcessor(app, time.Hour)
	require.NoError(t, processor.ProcessMessage(context.Background(), account.OrganizationID, work.Persisted.ID))
	completed := loadInboundContinuationJob(t, app, account.OrganizationID, work.Persisted.ID)
	require.Equal(t, models.ScheduledJobStatusCompleted, completed.Status)
	require.Equal(t, 1, completed.Attempts)
}

func TestWhatsAppIdentityReviewReservedWAMIDIsUniqueAcrossAccountShadows(t *testing.T) {
	app, first, _ := whatsappIdentityFixture(t)
	second := models.WhatsAppAccount{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: first.OrganizationID,
		Name:           "second-shadow-" + uuid.NewString()[:8],
		PhoneID:        testutil.NewTestGraphObjectID(),
		BusinessID:     first.BusinessID,
		AccessToken:    "second-token",
		Status:         "active",
		IsSMB:          true,
	}
	require.NoError(t, app.DB.Create(&second).Error)
	require.NoError(t, app.DB.Create(&models.WhatsAppCoexistenceState{
		ID:                uuid.New(),
		OrganizationID:    second.OrganizationID,
		WhatsAppAccountID: second.ID,
		OnboardingStatus:  models.CoexistenceOnboardingStatusConnected,
		OnboardingCycle:   1,
		LifecycleStatus:   models.CoexistenceLifecycleStatusConnected,
		LifecycleMetadata: models.JSONB{},
		Version:           1,
	}).Error)
	inbound := inboundContinuationTextMessage(
		t,
		"wamid.cross-shadow-"+uuid.NewString(),
		"",
		"one tenant-wide reserved winner",
	)
	inbound.FromUserID = "US.unowned-" + uuid.NewString()

	work, duplicate, err := app.persistAuthenticatedIncomingMessageBeforeAck(
		first.PhoneID, inbound, "", strings.Repeat("e", sha256.Size*2),
	)
	require.NoError(t, err)
	assert.False(t, duplicate)
	assert.Nil(t, work)
	work, duplicate, err = app.persistAuthenticatedIncomingMessageBeforeAck(
		second.PhoneID, inbound, "", strings.Repeat("f", sha256.Size*2),
	)
	require.NoError(t, err)
	assert.True(t, duplicate)
	assert.Nil(t, work)

	var events []models.InboundEvent
	require.NoError(t, app.DB.Where(
		"organization_id = ? AND protocol = ? AND provider_event_id = ?",
		first.OrganizationID,
		models.WhatsAppIdentityReviewInboundProtocol,
		inbound.ID,
	).Find(&events).Error)
	require.Len(t, events, 1)
	var firstShadow models.ChannelAccount
	require.NoError(t, app.DB.Where(
		"organization_id = ? AND id = ?",
		first.OrganizationID,
		events[0].ChannelAccountID,
	).First(&firstShadow).Error)
	boundAccountID, err := channelapi.LegacyMetaWhatsAppAccountID(&firstShadow)
	require.NoError(t, err)
	assert.Equal(t, first.ID, boundAccountID)
}

func signedWhatsAppIdentityMessageBody(t *testing.T, account *models.WhatsAppAccount, inbound IncomingTextMessage) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"object": "whatsapp_business_account",
		"entry": []any{map[string]any{"id": account.BusinessID, "changes": []any{map[string]any{
			"field": "messages", "value": map[string]any{
				"metadata": map[string]any{"phone_number_id": account.PhoneID}, "messages": []any{inbound},
			},
		}}}},
	})
	require.NoError(t, err)
	return body
}

func TestWhatsAppMessageIdentitySignedReplayPreservesFirstAdmissionWinner(t *testing.T) {
	for _, historyFirst := range []bool{false, true} {
		t.Run(fmt.Sprintf("history_first=%t", historyFirst), func(t *testing.T) {
			app, account, contact := whatsappIdentityFixture(t)
			app.Config = &config.Config{WhatsApp: config.WhatsAppConfig{AppSecret: webhookTestAppSecret}}
			inbound := inboundContinuationTextMessage(t, "wamid.optional-signed-"+uuid.NewString(), contact.PhoneNumber, "same live fact")
			var original *models.Message
			if historyFirst {
				stored := persistWhatsAppIdentityHistory(t, app, account, contact, inbound)
				original = &stored
			} else {
				sendSignedWebhook(t, app, signedWhatsAppIdentityMessageBody(t, account, inbound))
				app.WaitForBackgroundTasks()
			}
			inbound.FromUserID = "US.optional-" + uuid.NewString()
			inbound.FromParentUserID = "US.parent-" + uuid.NewString()
			for i := 0; i < 3; i++ {
				sendSignedWebhook(t, app, signedWhatsAppIdentityMessageBody(t, account, inbound))
				app.WaitForBackgroundTasks()
			}
			var storedContact models.Contact
			require.NoError(t, app.DB.First(&storedContact, contact.ID).Error)
			assert.Empty(t, storedContact.BSUID,
				"a later selector sidecar must not mutate the first durable destination")
			assert.Equal(t, contact.PhoneNumber, storedContact.PhoneNumber)
			var messages []models.Message
			require.NoError(t, app.DB.Where("organization_id = ? AND whats_app_message_id = ?", account.OrganizationID, inbound.ID).Find(&messages).Error)
			var reviews []models.InboundEvent
			require.NoError(t, app.DB.Where(
				"organization_id = ? AND protocol = ? AND provider_event_id = ?",
				account.OrganizationID,
				models.WhatsAppIdentityReviewInboundProtocol,
				inbound.ID,
			).Find(&reviews).Error)
			if historyFirst {
				require.Len(t, messages, 1)
				require.NotNil(t, original)
				assert.Equal(t, original.ID, messages[0].ID)
				assert.Equal(t, contact.ID, messages[0].ContactID)
				assert.Empty(t, reviews)
			} else {
				assert.Empty(t, messages)
				require.Len(t, reviews, 1)
				assert.Equal(t, models.WhatsAppIdentityReviewPendingEvent, reviews[0].EventType)
				assert.NotNil(t, reviews[0].ReviewHoldID)
			}
			var activities, jobs int64
			require.NoError(t, app.DB.Model(&models.CustomerActivityEvent{}).Where(
				"organization_id = ? AND idempotency_key = ?", account.OrganizationID, "message-incoming:"+uuid.NewSHA1(account.ID, []byte(inbound.ID)).String(),
			).Count(&activities).Error)
			require.NoError(t, app.DB.Model(&models.ScheduledJob{}).Where(
				"organization_id = ? AND kind = ?", account.OrganizationID, inboundContinuationJobKind,
			).Count(&jobs).Error)
			assert.Zero(t, activities)
			assert.Zero(t, jobs)
		})
	}
}

func TestWhatsAppMessageIdentityOptionalBSUIDRejectsConflictingProof(t *testing.T) {
	for _, mismatch := range []string{"absent_phone", "different_phone", "existing_user", "foreign_user_alias", "foreign_parent_alias", "foreign_account", "foreign_tenant", "unproven_random_uuid"} {
		t.Run(mismatch, func(t *testing.T) {
			app, account, contact := whatsappIdentityFixture(t)
			inbound := inboundContinuationTextMessage(t, "wamid.optional-negative-"+uuid.NewString(), contact.PhoneNumber, "unchanged")
			original := persistWhatsAppIdentityHistory(t, app, account, contact, inbound)
			identity := coexistenceContactIdentity{Phone: contact.PhoneNumber, UserID: "US.new-" + uuid.NewString(), ParentUserID: "US.parent-" + uuid.NewString()}
			expected := *account
			switch mismatch {
			case "absent_phone":
				identity.Phone = ""
			case "different_phone":
				identity.Phone = "60" + testutil.NewTestGraphObjectID()
			case "existing_user":
				require.NoError(t, app.DB.Model(contact).Update("bs_uid", "US.existing-"+uuid.NewString()).Error)
			case "foreign_user_alias", "foreign_parent_alias":
				other := testutil.CreateTestContact(t, app.DB, account.OrganizationID)
				conflict := identity.UserID
				if mismatch == "foreign_parent_alias" {
					conflict = identity.ParentUserID
				}
				require.NoError(t, app.DB.Model(other).Update("bs_uid", conflict).Error)
			case "foreign_account":
				expected = *testutil.CreateTestWhatsAppAccount(t, app.DB, account.OrganizationID)
			case "foreign_tenant":
				otherOrg := testutil.CreateTestOrganization(t, app.DB)
				expected = *testutil.CreateTestWhatsAppAccount(t, app.DB, otherOrg.ID)
			case "unproven_random_uuid":
				newID := uuid.New()
				require.NoError(t, app.DB.Model(&models.Message{}).Where("id = ?", original.ID).Update("id", newID).Error)
				original.ID = newID
			}
			var beforeContact models.Contact
			require.NoError(t, app.DB.First(&beforeContact, contact.ID).Error)
			require.NoError(t, app.DB.First(&original, original.ID).Error)
			err := app.WithCommittedTenantApp(expected.OrganizationID, func(scoped *App) error {
				if err := scoped.prepareWhatsAppMessageAuthority(&expected); err != nil {
					return err
				}
				_, err := scoped.resolveWhatsAppMessage(&expected, whatsAppMessageLookup{
					WAMID: inbound.ID, MessageID: original.ID, ContactID: contact.ID, Identity: identity,
					Direction: models.DirectionIncoming, Lock: true, RepairProjection: true,
				})
				return err
			})
			require.Error(t, err)
			var afterContact models.Contact
			var afterMessage models.Message
			require.NoError(t, app.DB.First(&afterContact, contact.ID).Error)
			require.NoError(t, app.DB.First(&afterMessage, original.ID).Error)
			assert.Equal(t, beforeContact, afterContact, "rejected optional identity must not mutate any contact field")
			assert.Equal(t, original, afterMessage)
		})
	}
}

func TestWhatsAppMessageIdentityOptionalBSUIDRejectsConcurrentCanonicalChange(t *testing.T) {
	app, account, contact := whatsappIdentityFixture(t)
	inbound := inboundContinuationTextMessage(t, "wamid.optional-canonical-"+uuid.NewString(), contact.PhoneNumber, "preserved winner")
	original := persistWhatsAppIdentityHistory(t, app, account, contact, inbound)
	replacement := testutil.CreateTestContact(t, app.DB, account.OrganizationID)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	app.DB = app.DB.WithContext(ctx)
	atContactLock, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	var blocked atomic.Bool
	callbackName := "test:optional-canonical-lock:" + uuid.NewString()
	require.NoError(t, app.DB.Callback().Query().Before("gorm:query").Register(callbackName, func(tx *gorm.DB) {
		if tx.Statement.Schema == nil || tx.Statement.Schema.Table != "contacts" {
			return
		}
		if _, locked := tx.Statement.Clauses["FOR"]; !locked || !blocked.CompareAndSwap(false, true) {
			return
		}
		close(atContactLock)
		select {
		case <-release:
		case <-ctx.Done():
			_ = tx.AddError(ctx.Err())
		}
	}))
	t.Cleanup(func() { _ = app.DB.Callback().Query().Remove(callbackName) })
	result := make(chan error, 1)
	go func() {
		result <- app.WithCommittedTenantApp(account.OrganizationID, func(scoped *App) error {
			if err := scoped.prepareWhatsAppMessageAuthority(account); err != nil {
				return err
			}
			_, err := scoped.resolveWhatsAppMessage(account, whatsAppMessageLookup{
				WAMID: inbound.ID, MessageID: original.ID, ContactID: contact.ID,
				Identity:  coexistenceContactIdentity{Phone: contact.PhoneNumber, UserID: "US.concurrent-" + original.ID.String()},
				Direction: models.DirectionIncoming, Lock: true, RepairProjection: true,
			})
			return err
		})
	}()
	select {
	case <-atContactLock:
	case err := <-result:
		t.Fatalf("resolver ended before canonical lock barrier: %v", err)
	case <-ctx.Done():
		t.Fatal("resolver never reached canonical lock barrier")
	}
	require.NoError(t, app.DB.Model(&models.Contact{}).Where("organization_id = ? AND id = ?", account.OrganizationID, contact.ID).
		Update("merged_into_id", replacement.ID).Error)
	unblock()
	select {
	case err := <-result:
		require.Error(t, err, "a changed canonical owner invalidates the earlier optional metadata proof")
	case <-ctx.Done():
		t.Fatal("resolver did not finish after canonical change")
	}
	var storedContact, storedReplacement models.Contact
	var storedMessage models.Message
	require.NoError(t, app.DB.First(&storedContact, contact.ID).Error)
	require.NoError(t, app.DB.First(&storedReplacement, replacement.ID).Error)
	require.NoError(t, app.DB.First(&storedMessage, original.ID).Error)
	assert.Empty(t, storedContact.BSUID)
	assert.Empty(t, storedReplacement.BSUID)
	require.True(t, original.CreatedAt.Equal(storedMessage.CreatedAt))
	require.True(t, original.UpdatedAt.Equal(storedMessage.UpdatedAt))
	require.Equal(t, original.IngestedAt == nil, storedMessage.IngestedAt == nil)
	if original.IngestedAt != nil {
		require.True(t, original.IngestedAt.Equal(*storedMessage.IngestedAt))
	}
	// PostgreSQL preserves the instant but the driver may reload it with the
	// local Location. Normalize only the already-proven timestamps before the
	// exact structural comparison below.
	storedMessage.CreatedAt = original.CreatedAt
	storedMessage.UpdatedAt = original.UpdatedAt
	storedMessage.IngestedAt = original.IngestedAt
	assert.Equal(t, original, storedMessage)
}

func TestWhatsAppMessageIdentityOptionalBSUIDFailurePreservesContinuation(t *testing.T) {
	for _, state := range []string{"pending", "processing", "completed", "manual_review"} {
		t.Run(state, func(t *testing.T) {
			app, account, contact := whatsappIdentityFixture(t)
			inbound := inboundContinuationTextMessage(t, "wamid.optional-failure-"+uuid.NewString(), contact.PhoneNumber, "persist exactly once")
			first, _, err := app.persistIncomingMessageBeforeAck(account.PhoneID, inbound, "Patient")
			require.NoError(t, err)
			job := loadInboundContinuationJob(t, app, account.OrganizationID, first.Persisted.ID)
			updates := map[string]any{}
			switch state {
			case "processing":
				updates = map[string]any{"status": models.ScheduledJobStatusProcessing, "locked_at": time.Now().UTC(), "locked_by": "existing-claim"}
			case "completed":
				updates = map[string]any{"status": models.ScheduledJobStatusCompleted, "completed_at": time.Now().UTC()}
			case "manual_review":
				payload := cloneMessageMetadata(job.Payload)
				payload["manual_review_required"] = true
				updates = map[string]any{"status": models.ScheduledJobStatusFailed, "payload": payload}
			}
			if len(updates) != 0 {
				require.NoError(t, app.DB.Model(&job).Updates(updates).Error)
			}
			require.NoError(t, app.DB.First(&job, job.ID).Error)
			inbound.FromUserID = "US.failed-enrichment-" + uuid.NewString()
			callbackName := "test:optional-bsuid-failure:" + uuid.NewString()
			var failedWrites atomic.Int32
			require.NoError(t, app.DB.Callback().Update().Before("gorm:update").Register(callbackName, func(tx *gorm.DB) {
				if tx.Statement.Schema == nil || tx.Statement.Schema.Table != "contacts" {
					return
				}
				fields, ok := tx.Statement.Dest.(map[string]any)
				if !ok || fields["bs_uid"] != inbound.FromUserID {
					return
				}
				failedWrites.Add(1)
				// A real PostgreSQL statement error poisons the transaction unless
				// the optional metadata write rolls back its own savepoint.
				_ = tx.AddError(tx.Exec("SELECT 1 / 0").Error)
			}))
			t.Cleanup(func() { _ = app.DB.Callback().Update().Remove(callbackName) })
			for i := 0; i < 2; i++ {
				replay, duplicate, err := app.persistIncomingMessageBeforeAck(account.PhoneID, inbound, "Patient")
				require.NoError(t, err, "optional enrichment failure cannot break unchanged enqueue proof")
				require.True(t, duplicate)
				assert.Equal(t, first.Persisted.ID, replay.Persisted.ID)
				assert.Empty(t, replay.Contact.BSUID, "failed metadata write must not be represented as committed")
			}
			require.Positive(t, failedWrites.Load(), "fixture must exercise the failing optional write")
			var processed int
			processor := NewInboundContinuationProcessor(app, time.Hour)
			processor.process = func(_ context.Context, _ *App, work *persistedIncomingMessage) error {
				processed++
				assert.Equal(t, first.Persisted.ID, work.Persisted.ID)
				assert.Equal(t, inbound.FromUserID, work.Message.FromUserID)
				assert.Empty(t, work.Contact.BSUID)
				return nil
			}
			require.NoError(t, processor.ProcessMessage(context.Background(), account.OrganizationID, first.Persisted.ID))
			stored := loadInboundContinuationJob(t, app, account.OrganizationID, first.Persisted.ID)
			assert.Equal(t, job.ID, stored.ID)
			assert.Equal(t, job.IdempotencyKey, stored.IdempotencyKey)
			if state == "pending" {
				assert.Equal(t, 1, processed, "unchanged worker proof must accept the failed optional enrichment")
				assert.Equal(t, models.ScheduledJobStatusCompleted, stored.Status)
			} else {
				assert.Zero(t, processed)
				assert.Equal(t, job, stored, "replay must preserve processing and terminal job state exactly")
			}
			var savedContact models.Contact
			require.NoError(t, app.DB.First(&savedContact, contact.ID).Error)
			assert.Empty(t, savedContact.BSUID)
			var activities, jobs int64
			require.NoError(t, app.DB.Model(&models.CustomerActivityEvent{}).Where("organization_id = ? AND source_object_id = ? AND event_type = ?", account.OrganizationID, first.Persisted.ID, models.CustomerActivityMessageIncoming).Count(&activities).Error)
			require.NoError(t, app.DB.Model(&models.ScheduledJob{}).Where("kind = ? AND aggregate_id = ?", inboundContinuationJobKind, first.Persisted.ID).Count(&jobs).Error)
			assert.EqualValues(t, 1, activities)
			assert.EqualValues(t, 1, jobs)
		})
	}
}

func TestWhatsAppMessageIdentityContinuationProof(t *testing.T) {
	account := models.WhatsAppAccount{BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: uuid.New(), PhoneID: "123456789", Name: "before rename"}
	messageID := uuid.New()
	wamid := "wamid.pure-identity"
	for _, mutation := range []string{"valid_after_rename", "wrong_tenant", "wrong_account_key", "wrong_aggregate", "wrong_payload_id", "wrong_payload_wamid", "wrong_phone", "wrong_raw_wamid", "missing_sender"} {
		t.Run(mutation, func(t *testing.T) {
			job := models.ScheduledJob{
				OrganizationID: account.OrganizationID, Kind: inboundContinuationJobKind,
				AggregateType: "message", AggregateID: &messageID,
				IdempotencyKey: "inbound-message-continuation:" + uuid.NewSHA1(account.ID, []byte(wamid)).String(),
				Payload: models.JSONB{"message_id": messageID.String(), "wamid": wamid, "phone_number_id": account.PhoneID,
					"message": map[string]any{"id": wamid, "from": "60123456789", "type": "text"}},
			}
			current := account
			current.Name = "after rename"
			switch mutation {
			case "wrong_tenant":
				job.OrganizationID = uuid.New()
			case "wrong_account_key":
				job.IdempotencyKey = "inbound-message-continuation:" + uuid.NewSHA1(uuid.New(), []byte(wamid)).String()
			case "wrong_aggregate":
				otherID := uuid.New()
				job.AggregateID = &otherID
			case "wrong_payload_id":
				job.Payload["message_id"] = uuid.NewString()
			case "wrong_payload_wamid":
				job.Payload["wamid"] = "wamid.other"
			case "wrong_phone":
				job.Payload["phone_number_id"] = "987654321"
			case "wrong_raw_wamid":
				job.Payload["message"] = map[string]any{"id": "wamid.other", "from": "60123456789"}
			case "missing_sender":
				job.Payload["message"] = map[string]any{"id": wamid}
			}
			inbound, err := validateInboundContinuationJobProof(&job, &current, messageID, wamid)
			if mutation == "valid_after_rename" {
				require.NoError(t, err)
				assert.Equal(t, wamid, inbound.ID)
			} else {
				require.Error(t, err)
			}
		})
	}
}

func sendSignedWebhook(t *testing.T, app *App, body []byte) {
	t.Helper()
	req := testutil.NewRequest(t)
	req.RequestCtx.Request.Header.SetMethod("POST")
	req.RequestCtx.Request.Header.SetContentType("application/json")
	req.RequestCtx.Request.Header.Set("X-Hub-Signature-256", webhookTestSignature(body))
	req.RequestCtx.Request.SetBody(body)
	require.NoError(t, app.WebhookHandler(req))
	assert.Equal(t, 200, req.RequestCtx.Response.StatusCode(), string(req.RequestCtx.Response.Body()))
}

func webhookTestSignature(body []byte) string {
	mac := hmac.New(sha256.New, []byte(webhookTestAppSecret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}
