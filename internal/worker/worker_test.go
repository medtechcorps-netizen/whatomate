package worker

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/shridarpatil/whatomate/internal/config"
	"github.com/shridarpatil/whatomate/internal/crypto"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/internal/queue"
	"github.com/shridarpatil/whatomate/internal/templateutil"
	"github.com/shridarpatil/whatomate/internal/whatsappaccount"
	"github.com/shridarpatil/whatomate/pkg/whatsapp"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type campaignStatsCaptureHook struct {
	payloads chan []byte
}

func (h *campaignStatsCaptureHook) DialHook(next redis.DialHook) redis.DialHook {
	return next
}

func (h *campaignStatsCaptureHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		args := cmd.Args()
		if cmd.Name() != "publish" || len(args) != 3 || args[1] != queue.CampaignStatsChannel {
			return next(ctx, cmd)
		}
		payload, ok := args[2].([]byte)
		if !ok {
			return errors.New("campaign stats payload is not bytes")
		}
		h.payloads <- append([]byte(nil), payload...)
		cmd.SetErr(nil)
		return nil
	}
}

func (h *campaignStatsCaptureHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

func testWorker(t *testing.T) *Worker {
	t.Helper()
	db := testutil.SetupTestDB(t)
	log := testutil.NopLogger()

	w := &Worker{
		DB:       db,
		Log:      log,
		WhatsApp: whatsapp.New(log),
	}

	// Set up Publisher if Redis is available
	if rdb := testutil.SetupTestRedis(t); rdb != nil {
		w.Redis = rdb
		w.Publisher = queue.NewPublisher(rdb, log)
	}

	return w
}

func TestNewWhatsAppClientUsesConfiguredBaseURL(t *testing.T) {
	t.Parallel()

	type observedRequest struct {
		path          string
		authorization string
	}
	observed := make(chan observedRequest, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		observed <- observedRequest{
			path:          r.URL.Path,
			authorization: r.Header.Get("Authorization"),
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"messages":[{"id":"wamid.local-only"}]}`))
	}))
	t.Cleanup(server.Close)

	cfg := &config.Config{
		WhatsApp: config.WhatsAppConfig{BaseURL: "  " + server.URL + "/  "},
	}
	client := newWhatsAppClient(cfg, testutil.NopLogger())

	messageID, err := client.SendTextMessage(
		context.Background(),
		&whatsapp.Account{
			PhoneID:     "123456789",
			APIVersion:  "v99.0",
			AccessToken: "synthetic-local-token",
		},
		whatsapp.Recipient{Phone: "15550000000"},
		"local routing check",
	)
	require.NoError(t, err)
	assert.Equal(t, "wamid.local-only", messageID)

	request := <-observed
	assert.Equal(t, "/v99.0/123456789/messages", request.path)
	assert.Equal(t, "Bearer synthetic-local-token", request.authorization)
}

func TestNewWhatsAppClientAcceptsNilConfig(t *testing.T) {
	t.Parallel()

	assert.NotPanics(t, func() {
		client := newWhatsAppClient(nil, testutil.NopLogger())
		require.NotNil(t, client)
		require.NotNil(t, client.HTTPClient)
	})
}

func TestWorkerPublishHeartbeatCreatesFreshLease(t *testing.T) {
	rdb := testutil.SetupTestRedis(t)
	if rdb == nil {
		t.Skip("TEST_REDIS_URL not set")
	}
	ctx := context.Background()
	require.NoError(t, rdb.Del(ctx, queue.WorkerHeartbeatKey).Err())
	t.Cleanup(func() {
		_ = rdb.Del(ctx, queue.WorkerHeartbeatKey).Err()
	})

	w := &Worker{Redis: rdb}
	require.NoError(t, w.publishHeartbeat(ctx))

	value, err := rdb.Get(ctx, queue.WorkerHeartbeatKey).Result()
	require.NoError(t, err)
	heartbeatAt, err := time.Parse(time.RFC3339Nano, value)
	require.NoError(t, err)
	assert.WithinDuration(t, time.Now().UTC(), heartbeatAt, 5*time.Second)
	ttl, err := rdb.TTL(ctx, queue.WorkerHeartbeatKey).Result()
	require.NoError(t, err)
	assert.Positive(t, ttl)
	assert.LessOrEqual(t, ttl, queue.WorkerHeartbeatTTL)
}

// getOrCreateTestPermissions gets existing permissions or creates them for testing.
func getOrCreateTestPermissions(t *testing.T, w *Worker) []models.Permission {
	t.Helper()

	var existingPerms []models.Permission
	if err := w.DB.Order("resource, action").Find(&existingPerms).Error; err == nil && len(existingPerms) > 0 {
		return existingPerms
	}

	// Create all default permissions if none exist
	perms := models.DefaultPermissions()
	for i := range perms {
		perms[i].ID = uuid.New()
	}
	require.NoError(t, w.DB.Create(&perms).Error)
	return perms
}

// createTestRole creates an admin role with all permissions for testing.
func createTestRole(t *testing.T, w *Worker, orgID uuid.UUID) *models.CustomRole {
	t.Helper()

	// Get or create permissions
	perms := getOrCreateTestPermissions(t, w)

	role := &models.CustomRole{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: orgID,
		Name:           "admin_" + uuid.New().String()[:8],
		Description:    "Test admin role",
		IsSystem:       false,
		IsDefault:      false,
		Permissions:    perms,
	}
	require.NoError(t, w.DB.Create(role).Error)
	return role
}

func createTestCampaignData(t *testing.T, w *Worker) (*models.Organization, *models.WhatsAppAccount, *models.Template, *models.BulkMessageCampaign, *models.BulkMessageRecipient) {
	t.Helper()

	uniqueID := uuid.New().String()[:8]

	// Create organization
	org := &models.Organization{
		Name: "Test Org " + uniqueID,
		Slug: "test-org-" + uniqueID,
	}
	require.NoError(t, w.DB.Create(org).Error)

	// Create role for user
	role := createTestRole(t, w, org.ID)

	// Create user for CreatedBy foreign key
	user := &models.User{
		OrganizationID: org.ID,
		Email:          "test-" + uniqueID + "@example.com",
		PasswordHash:   "hashed",
		FullName:       "Test User",
		RoleID:         &role.ID,
		IsActive:       true,
	}
	require.NoError(t, w.DB.Create(user).Error)

	// Create WhatsApp account with unique name
	accountName := "test-account-" + uniqueID
	account := &models.WhatsAppAccount{
		OrganizationID: org.ID,
		Name:           accountName,
		PhoneID:        testutil.NewTestGraphObjectID(),
		BusinessID:     "business-" + uniqueID,
		AccessToken:    "test-token",
	}
	require.NoError(t, w.DB.Create(account).Error)

	// Create template
	template := &models.Template{
		OrganizationID:  org.ID,
		WhatsAppAccount: accountName,
		Name:            "test_template_" + uniqueID,
		Language:        "en",
		Category:        "MARKETING",
		Status:          "APPROVED",
		BodyContent:     "Hello {{1}}, your order {{2}} is ready!",
	}
	require.NoError(t, w.DB.Create(template).Error)

	// Create campaign with CreatedBy
	startedAt := time.Now().UTC()
	campaign := &models.BulkMessageCampaign{
		OrganizationID:  org.ID,
		Name:            "Test Campaign " + uniqueID,
		WhatsAppAccount: accountName,
		TemplateID:      template.ID,
		Status:          models.CampaignStatusProcessing,
		StartedAt:       &startedAt,
		TotalRecipients: 1,
		CreatedBy:       user.ID,
	}
	require.NoError(t, w.DB.Create(campaign).Error)

	// Create recipient
	recipient := &models.BulkMessageRecipient{
		CampaignID:    campaign.ID,
		PhoneNumber:   "1112223333",
		RecipientName: "Test User",
		Status:        models.MessageStatusPending,
		TemplateParams: models.JSONB{
			"1": "John",
			"2": "ORD-123",
		},
	}
	require.NoError(t, w.DB.Create(recipient).Error)

	// Reload campaign with template
	require.NoError(t, w.DB.Preload("Template").First(campaign, campaign.ID).Error)

	return org, account, template, campaign, recipient
}

func TestCampaignJobHasCurrentGeneration(t *testing.T) {
	startedAt := time.Date(2026, time.June, 1, 12, 0, 0, 0, time.UTC)
	currentCampaign := &models.BulkMessageCampaign{
		Status:    models.CampaignStatusProcessing,
		StartedAt: &startedAt,
	}

	for _, tc := range []struct {
		name     string
		campaign *models.BulkMessageCampaign
		job      *queue.RecipientJob
		want     bool
	}{
		{
			name:     "zero job generation fails closed",
			campaign: currentCampaign,
			job:      &queue.RecipientJob{},
		},
		{
			name:     "stale job generation fails closed",
			campaign: currentCampaign,
			job:      &queue.RecipientJob{EnqueuedAt: startedAt.Add(-time.Nanosecond)},
		},
		{
			name:     "current job generation proceeds",
			campaign: currentCampaign,
			job:      &queue.RecipientJob{EnqueuedAt: startedAt},
			want:     true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, campaignJobHasCurrentGeneration(tc.campaign, tc.job))
		})
	}
}

func TestWorker_HandleRecipientJob_CampaignPaused(t *testing.T) {
	w := testWorker(t)
	org, _, _, campaign, recipient := createTestCampaignData(t, w)

	// Pause the campaign
	require.NoError(t, w.DB.Model(campaign).Update("status", models.CampaignStatusPaused).Error)

	job := &queue.RecipientJob{
		CampaignID:     campaign.ID,
		RecipientID:    recipient.ID,
		OrganizationID: org.ID,
		PhoneNumber:    recipient.PhoneNumber,
		RecipientName:  recipient.RecipientName,
		EnqueuedAt:     campaign.StartedAt.UTC(),
	}

	err := w.HandleRecipientJob(context.Background(), job)
	require.NoError(t, err)

	// Recipient status should remain pending (job was skipped)
	var updatedRecipient models.BulkMessageRecipient
	require.NoError(t, w.DB.First(&updatedRecipient, recipient.ID).Error)
	assert.Equal(t, models.MessageStatusPending, updatedRecipient.Status)
}

func TestWorker_HandleRecipientJob_CampaignCancelled(t *testing.T) {
	w := testWorker(t)
	org, _, _, campaign, recipient := createTestCampaignData(t, w)

	// Cancel the campaign
	require.NoError(t, w.DB.Model(campaign).Update("status", models.CampaignStatusCancelled).Error)

	job := &queue.RecipientJob{
		CampaignID:     campaign.ID,
		RecipientID:    recipient.ID,
		OrganizationID: org.ID,
		PhoneNumber:    recipient.PhoneNumber,
		RecipientName:  recipient.RecipientName,
		EnqueuedAt:     campaign.StartedAt.UTC(),
	}

	err := w.HandleRecipientJob(context.Background(), job)
	require.NoError(t, err)

	// Recipient status should remain pending (job was skipped)
	var updatedRecipient models.BulkMessageRecipient
	require.NoError(t, w.DB.First(&updatedRecipient, recipient.ID).Error)
	assert.Equal(t, models.MessageStatusPending, updatedRecipient.Status)
}

func TestWorker_HandleRecipientJob_AccountNotFound(t *testing.T) {
	w := testWorker(t)
	org, _, _, campaign, recipient := createTestCampaignData(t, w)

	// Change campaign to use non-existent account
	campaign.WhatsAppAccount = "non-existent-account"
	require.NoError(t, w.DB.Save(campaign).Error)

	job := &queue.RecipientJob{
		CampaignID:     campaign.ID,
		RecipientID:    recipient.ID,
		OrganizationID: org.ID,
		PhoneNumber:    recipient.PhoneNumber,
		RecipientName:  recipient.RecipientName,
		EnqueuedAt:     campaign.StartedAt.UTC(),
	}

	err := w.HandleRecipientJob(context.Background(), job)
	require.NoError(t, err)

	// Verify recipient marked as failed
	var updatedRecipient models.BulkMessageRecipient
	require.NoError(t, w.DB.First(&updatedRecipient, recipient.ID).Error)
	assert.Equal(t, models.MessageStatusFailed, updatedRecipient.Status)
	assert.Contains(t, updatedRecipient.ErrorMessage, "WhatsApp account not found")
}

func TestWorker_HandleRecipientJob_DisconnectedAccountDoesNotCallGraph(t *testing.T) {
	w := testWorker(t)
	org, account, _, campaign, recipient := createTestCampaignData(t, w)
	require.NoError(t, w.DB.Model(&models.WhatsAppAccount{}).
		Where("id = ? AND organization_id = ?", account.ID, org.ID).
		Update("status", "disconnected").Error)

	var providerRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		providerRequests.Add(1)
		rw.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(rw).Encode(map[string]any{
			"messages": []map[string]any{{"id": "wamid.must-not-send"}},
		})
	}))
	defer server.Close()
	w.WhatsApp = whatsapp.NewWithBaseURL(w.Log, server.URL)

	job := &queue.RecipientJob{
		CampaignID:     campaign.ID,
		RecipientID:    recipient.ID,
		OrganizationID: org.ID,
		PhoneNumber:    recipient.PhoneNumber,
		RecipientName:  recipient.RecipientName,
		TemplateParams: recipient.TemplateParams,
		EnqueuedAt:     campaign.StartedAt.UTC(),
	}
	require.NoError(t, w.HandleRecipientJob(context.Background(), job))
	assert.Zero(t, providerRequests.Load(), "a disconnected account must make zero Graph requests")

	var storedRecipient models.BulkMessageRecipient
	require.NoError(t, w.DB.First(&storedRecipient, recipient.ID).Error)
	assert.Equal(t, models.MessageStatusFailed, storedRecipient.Status)
	assert.Equal(t, whatsappaccount.ErrOutboundInactive.Error(), storedRecipient.ErrorMessage)

	var storedCampaign models.BulkMessageCampaign
	require.NoError(t, w.DB.First(&storedCampaign, campaign.ID).Error)
	assert.Equal(t, 1, storedCampaign.FailedCount)
	assert.Zero(t, storedCampaign.SentCount)

	var messageCount int64
	require.NoError(t, w.DB.Model(&models.Message{}).
		Where("organization_id = ?", org.ID).
		Count(&messageCount).Error)
	assert.Zero(t, messageCount)
}

func TestWorker_HandleRecipientJob_CampaignNotFound(t *testing.T) {
	w := testWorker(t)

	job := &queue.RecipientJob{
		CampaignID:     uuid.New(), // Non-existent campaign
		RecipientID:    uuid.New(),
		OrganizationID: uuid.New(),
		PhoneNumber:    "1234567890",
		RecipientName:  "Test",
		EnqueuedAt:     time.Now().UTC(),
	}

	err := w.HandleRecipientJob(context.Background(), job)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to load campaign")
}

func TestWorker_HandleRecipientJob_RejectsCrossTenantCampaignJob(t *testing.T) {
	w := testWorker(t)
	campaignOrg, _, _, campaign, recipient := createTestCampaignData(t, w)
	jobOrg := testutil.CreateTestOrganization(t, w.DB)

	job := &queue.RecipientJob{
		CampaignID:     campaign.ID,
		RecipientID:    recipient.ID,
		OrganizationID: jobOrg.ID,
		PhoneNumber:    recipient.PhoneNumber,
		RecipientName:  recipient.RecipientName,
		TemplateParams: recipient.TemplateParams,
		EnqueuedAt:     campaign.StartedAt.UTC(),
	}

	err := w.HandleRecipientJob(context.Background(), job)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to load campaign")

	var storedRecipient models.BulkMessageRecipient
	require.NoError(t, w.DB.First(&storedRecipient, recipient.ID).Error)
	assert.Equal(t, models.MessageStatusPending, storedRecipient.Status)
	var storedCampaign models.BulkMessageCampaign
	require.NoError(t, w.DB.First(&storedCampaign, campaign.ID).Error)
	assert.Equal(t, campaignOrg.ID, storedCampaign.OrganizationID)
	assert.Zero(t, storedCampaign.SentCount)
	assert.Zero(t, storedCampaign.FailedCount)

	var messageCount int64
	require.NoError(t, w.DB.Model(&models.Message{}).
		Where("organization_id IN ?", []uuid.UUID{campaignOrg.ID, jobOrg.ID}).
		Count(&messageCount).Error)
	assert.Zero(t, messageCount)
}

// createMinimalCampaignData creates the minimum data needed for campaign tests
// Returns org, user, template, and campaign
func createMinimalCampaignData(t *testing.T, w *Worker, status models.CampaignStatus) (*models.Organization, *models.User, *models.Template, *models.BulkMessageCampaign) {
	t.Helper()
	uniqueID := uuid.New().String()[:8]

	org := &models.Organization{
		Name: "Test Org " + uniqueID,
		Slug: "test-org-" + uniqueID,
	}
	require.NoError(t, w.DB.Create(org).Error)

	// Create role for user
	role := createTestRole(t, w, org.ID)

	user := &models.User{
		OrganizationID: org.ID,
		Email:          "test-" + uniqueID + "@example.com",
		PasswordHash:   "hashed",
		FullName:       "Test User",
		RoleID:         &role.ID,
		IsActive:       true,
	}
	require.NoError(t, w.DB.Create(user).Error)

	accountName := "test-account-" + uniqueID
	account := &models.WhatsAppAccount{
		OrganizationID: org.ID,
		Name:           accountName,
		PhoneID:        testutil.NewTestGraphObjectID(),
		BusinessID:     "business-" + uniqueID,
		AccessToken:    "test-token",
	}
	require.NoError(t, w.DB.Create(account).Error)

	template := &models.Template{
		OrganizationID:  org.ID,
		WhatsAppAccount: accountName,
		Name:            "test_template_" + uniqueID,
		Language:        "en",
		Category:        string(models.TemplateCategoryMarketing),
		Status:          string(models.TemplateStatusApproved),
		BodyContent:     "Hello {{1}}!",
	}
	require.NoError(t, w.DB.Create(template).Error)

	campaign := &models.BulkMessageCampaign{
		OrganizationID:  org.ID,
		Name:            "Test Campaign " + uniqueID,
		WhatsAppAccount: accountName,
		TemplateID:      template.ID,
		Status:          status,
		CreatedBy:       user.ID,
	}
	require.NoError(t, w.DB.Create(campaign).Error)

	return org, user, template, campaign
}

func TestWorker_updateRecipientStatus_Sent(t *testing.T) {
	w := testWorker(t)

	// Create campaign data with proper foreign keys
	_, _, _, campaign := createMinimalCampaignData(t, w, models.CampaignStatusProcessing)

	recipient := &models.BulkMessageRecipient{
		CampaignID:  campaign.ID,
		PhoneNumber: "1234567890",
		Status:      models.MessageStatusPending,
	}
	require.NoError(t, w.DB.Create(recipient).Error)

	// Test updating to sent status
	w.updateRecipientStatus(recipient.ID, models.MessageStatusSent, "wamid.123", "")

	var updated models.BulkMessageRecipient
	require.NoError(t, w.DB.First(&updated, recipient.ID).Error)
	assert.Equal(t, models.MessageStatusSent, updated.Status)
	assert.Equal(t, "wamid.123", updated.WhatsAppMessageID)
	assert.NotNil(t, updated.SentAt)
}

func TestWorker_updateRecipientStatus_Failed(t *testing.T) {
	w := testWorker(t)

	// Create campaign data with proper foreign keys
	_, _, _, campaign := createMinimalCampaignData(t, w, models.CampaignStatusProcessing)

	recipient := &models.BulkMessageRecipient{
		CampaignID:  campaign.ID,
		PhoneNumber: "9876543210",
		Status:      models.MessageStatusPending,
	}
	require.NoError(t, w.DB.Create(recipient).Error)

	w.updateRecipientStatus(recipient.ID, models.MessageStatusFailed, "", "API error")

	var updated models.BulkMessageRecipient
	require.NoError(t, w.DB.First(&updated, recipient.ID).Error)
	assert.Equal(t, models.MessageStatusFailed, updated.Status)
	assert.Equal(t, "API error", updated.ErrorMessage)
}

func TestWorker_incrementCampaignCount(t *testing.T) {
	w := testWorker(t)

	// Create campaign data with proper foreign keys
	_, _, _, campaign := createMinimalCampaignData(t, w, models.CampaignStatusProcessing)

	// Increment sent count multiple times
	w.incrementCampaignCount(campaign.ID, "sent_count")
	w.incrementCampaignCount(campaign.ID, "sent_count")
	w.incrementCampaignCount(campaign.ID, "failed_count")

	var updated models.BulkMessageCampaign
	require.NoError(t, w.DB.First(&updated, campaign.ID).Error)
	assert.Equal(t, 2, updated.SentCount)
	assert.Equal(t, 1, updated.FailedCount)
}

func TestWorker_checkCampaignCompletion_CompletesWhenAllProcessed(t *testing.T) {
	w := testWorker(t)

	// Create campaign data with proper foreign keys
	org, _, _, campaign := createMinimalCampaignData(t, w, models.CampaignStatusProcessing)

	// Update campaign counts for this test
	require.NoError(t, w.DB.Model(campaign).Updates(map[string]any{
		"total_recipients": 2,
		"sent_count":       2,
	}).Error)

	// Create recipients that are already processed (not pending)
	recipient1 := &models.BulkMessageRecipient{
		CampaignID:  campaign.ID,
		PhoneNumber: "1111111111",
		Status:      models.MessageStatusSent,
	}
	recipient2 := &models.BulkMessageRecipient{
		CampaignID:  campaign.ID,
		PhoneNumber: "2222222222",
		Status:      models.MessageStatusSent,
	}
	require.NoError(t, w.DB.Create(recipient1).Error)
	require.NoError(t, w.DB.Create(recipient2).Error)

	// Check completion - should complete since no pending recipients
	update, err := w.checkCampaignCompletion(context.Background(), campaign.ID, org.ID)
	require.NoError(t, err)
	require.NotNil(t, update)
	assert.Equal(t, models.CampaignStatusCompleted, update.Status)

	var updated models.BulkMessageCampaign
	require.NoError(t, w.DB.First(&updated, campaign.ID).Error)
	assert.Equal(t, models.CampaignStatusCompleted, updated.Status)
	assert.NotNil(t, updated.CompletedAt)
}

func TestWorker_checkCampaignCompletion_DoesNotCompleteWithPending(t *testing.T) {
	w := testWorker(t)

	// Create campaign data with proper foreign keys
	org, _, _, campaign := createMinimalCampaignData(t, w, models.CampaignStatusProcessing)

	// Update campaign counts for this test
	require.NoError(t, w.DB.Model(campaign).Updates(map[string]any{
		"total_recipients": 2,
		"sent_count":       1,
	}).Error)

	// Create one processed and one pending recipient
	recipient1 := &models.BulkMessageRecipient{
		CampaignID:  campaign.ID,
		PhoneNumber: "1111111111",
		Status:      models.MessageStatusSent,
	}
	recipient2 := &models.BulkMessageRecipient{
		CampaignID:  campaign.ID,
		PhoneNumber: "2222222222",
		Status:      models.MessageStatusPending,
	}
	require.NoError(t, w.DB.Create(recipient1).Error)
	require.NoError(t, w.DB.Create(recipient2).Error)

	// Check completion - should NOT complete since there's a pending recipient
	update, err := w.checkCampaignCompletion(context.Background(), campaign.ID, org.ID)
	require.NoError(t, err)
	require.NotNil(t, update)
	assert.Equal(t, models.CampaignStatusProcessing, update.Status)

	var updated models.BulkMessageCampaign
	require.NoError(t, w.DB.First(&updated, campaign.ID).Error)
	assert.Equal(t, models.CampaignStatusProcessing, updated.Status)
	assert.Nil(t, updated.CompletedAt)
}

func TestWorker_checkCampaignCompletion_NotProcessingStatus(t *testing.T) {
	w := testWorker(t)

	// Create campaign data with proper foreign keys - status is paused
	org, _, _, campaign := createMinimalCampaignData(t, w, models.CampaignStatusPaused)

	// Should not change status since it's not models.CampaignStatusProcessing
	update, err := w.checkCampaignCompletion(context.Background(), campaign.ID, org.ID)
	require.NoError(t, err)
	assert.Nil(t, update)

	var updated models.BulkMessageCampaign
	require.NoError(t, w.DB.First(&updated, campaign.ID).Error)
	assert.Equal(t, models.CampaignStatusPaused, updated.Status)
}

func TestWorkerCampaignStatsPublishOnlyAfterRecipientTransactionCommits(t *testing.T) {
	w := testWorker(t)
	organization, _, _, campaign := createMinimalCampaignData(t, w, models.CampaignStatusProcessing)
	require.NoError(t, w.DB.Model(campaign).Updates(map[string]any{
		"total_recipients": 1,
		"sent_count":       1,
	}).Error)

	capture := &campaignStatsCaptureHook{payloads: make(chan []byte, 2)}
	redisClient := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})
	redisClient.AddHook(capture)
	t.Cleanup(func() { require.NoError(t, redisClient.Close()) })
	w.Publisher = queue.NewPublisher(redisClient, w.Log)

	forcedRollback := errors.New("force recipient finalization rollback")
	err := w.withRecipientTenantTransactionAndCampaignStats(
		context.Background(),
		organization.ID,
		func(tx *gorm.DB) (*queue.CampaignStatsUpdate, error) {
			scoped := *w
			scoped.DB = tx
			update, completionErr := scoped.checkCampaignCompletion(context.Background(), campaign.ID, organization.ID)
			require.NoError(t, completionErr)
			require.NotNil(t, update)
			select {
			case payload := <-capture.payloads:
				t.Fatalf("campaign stats published before rollback: %s", payload)
			default:
			}
			return update, forcedRollback
		},
	)
	require.ErrorIs(t, err, forcedRollback)
	select {
	case payload := <-capture.payloads:
		t.Fatalf("rolled-back campaign stats were published: %s", payload)
	default:
	}

	var rolledBack models.BulkMessageCampaign
	require.NoError(t, w.DB.First(&rolledBack, campaign.ID).Error)
	assert.Equal(t, models.CampaignStatusProcessing, rolledBack.Status)
	assert.Nil(t, rolledBack.CompletedAt)

	err = w.withRecipientTenantTransactionAndCampaignStats(
		context.Background(),
		organization.ID,
		func(tx *gorm.DB) (*queue.CampaignStatsUpdate, error) {
			scoped := *w
			scoped.DB = tx
			update, completionErr := scoped.checkCampaignCompletion(context.Background(), campaign.ID, organization.ID)
			require.NoError(t, completionErr)
			require.NotNil(t, update)
			select {
			case payload := <-capture.payloads:
				t.Fatalf("campaign stats published before commit: %s", payload)
			default:
			}
			return update, nil
		},
	)
	require.NoError(t, err)

	select {
	case payload := <-capture.payloads:
		var update queue.CampaignStatsUpdate
		require.NoError(t, json.Unmarshal(payload, &update))
		assert.Equal(t, campaign.ID.String(), update.CampaignID)
		assert.Equal(t, organization.ID, update.OrganizationID)
		assert.Equal(t, models.CampaignStatusCompleted, update.Status)
		assert.Equal(t, 1, update.SentCount)
	default:
		t.Fatal("committed campaign stats were not published")
	}

	var committed models.BulkMessageCampaign
	require.NoError(t, w.DB.First(&committed, campaign.ID).Error)
	assert.Equal(t, models.CampaignStatusCompleted, committed.Status)
	assert.NotNil(t, committed.CompletedAt)
}

func TestWorkerCampaignCompletionErrorsRollbackWithoutPublication(t *testing.T) {
	for _, stage := range []string{"count", "conditional update", "reload"} {
		t.Run(stage, func(t *testing.T) {
			w := testWorker(t)
			organization, _, _, campaign := createMinimalCampaignData(t, w, models.CampaignStatusProcessing)
			capture := &campaignStatsCaptureHook{payloads: make(chan []byte, 1)}
			redisClient := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})
			redisClient.AddHook(capture)
			t.Cleanup(func() { require.NoError(t, redisClient.Close()) })
			w.Publisher = queue.NewPublisher(redisClient, w.Log)

			forced := errors.New("forced campaign completion " + stage + " failure")
			callbackName := "test:campaign_completion_" + strings.ReplaceAll(stage, " ", "_") + ":" + uuid.NewString()
			var campaignQueries atomic.Int32
			var removeCallback func() error
			switch stage {
			case "count":
				require.NoError(t, w.DB.Callback().Query().Before("gorm:query").Register(callbackName, func(tx *gorm.DB) {
					if tx.Statement.Schema != nil && tx.Statement.Schema.Table == "bulk_message_recipients" {
						_ = tx.AddError(forced)
					}
				}))
				removeCallback = func() error { return w.DB.Callback().Query().Remove(callbackName) }
			case "conditional update":
				require.NoError(t, w.DB.Callback().Update().Before("gorm:update").Register(callbackName, func(tx *gorm.DB) {
					if tx.Statement.Schema != nil && tx.Statement.Schema.Table == "bulk_message_campaigns" {
						_ = tx.AddError(forced)
					}
				}))
				removeCallback = func() error { return w.DB.Callback().Update().Remove(callbackName) }
			case "reload":
				require.NoError(t, w.DB.Callback().Query().Before("gorm:query").Register(callbackName, func(tx *gorm.DB) {
					if tx.Statement.Schema != nil && tx.Statement.Schema.Table == "bulk_message_campaigns" &&
						campaignQueries.Add(1) == 2 {
						_ = tx.AddError(forced)
					}
				}))
				removeCallback = func() error { return w.DB.Callback().Query().Remove(callbackName) }
			}

			err := w.withRecipientTenantTransactionAndCampaignStats(
				context.Background(),
				organization.ID,
				func(tx *gorm.DB) (*queue.CampaignStatsUpdate, error) {
					scoped := *w
					scoped.DB = tx
					return scoped.checkCampaignCompletion(context.Background(), campaign.ID, organization.ID)
				},
			)
			require.NoError(t, removeCallback())
			require.ErrorIs(t, err, forced)
			select {
			case payload := <-capture.payloads:
				t.Fatalf("failed campaign completion published stats: %s", payload)
			default:
			}

			var stored models.BulkMessageCampaign
			require.NoError(t, w.DB.First(&stored, campaign.ID).Error)
			assert.Equal(t, models.CampaignStatusProcessing, stored.Status)
			assert.Nil(t, stored.CompletedAt)
		})
	}
}

func TestWorker_sendTemplateMessage_BuildsComponents(t *testing.T) {
	w := testWorker(t)

	// Create mock server
	var capturedBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&capturedBody)
		rw.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(rw).Encode(map[string]any{
			"messages": []map[string]any{
				{"id": "wamid.test123"},
			},
		})
	}))
	defer server.Close()

	// Create WhatsApp client pointing to mock server
	w.WhatsApp = whatsapp.NewWithBaseURL(w.Log, server.URL)

	account := &models.WhatsAppAccount{
		PhoneID:     "123",
		BusinessID:  "456",
		AccessToken: "token",
		APIVersion:  "v21.0",
	}

	template := &models.Template{
		Name:        "test_template",
		Language:    "en",
		BodyContent: "Hello {{1}}, welcome to {{2}}!",
	}

	recipient := &models.BulkMessageRecipient{
		PhoneNumber: "1234567890",
		TemplateParams: models.JSONB{
			"1": "Hello",
			"2": "World",
		},
	}

	msgID, err := w.sendTemplateMessage(context.Background(), account, template, recipient, "", "")
	require.NoError(t, err)
	assert.Equal(t, "wamid.test123", msgID)

	// Verify request structure
	templateData := capturedBody["template"].(map[string]any)
	assert.Equal(t, "test_template", templateData["name"])
	assert.Equal(t, "en", templateData["language"].(map[string]any)["code"])

	components := templateData["components"].([]any)
	require.Len(t, components, 1)

	bodyComponent := components[0].(map[string]any)
	assert.Equal(t, "body", bodyComponent["type"])

	params := bodyComponent["parameters"].([]any)
	require.Len(t, params, 2)
	assert.Equal(t, "Hello", params[0].(map[string]any)["text"])
	assert.Equal(t, "World", params[1].(map[string]any)["text"])
}

// When the recipient has explicit HeaderParams, the worker must use them
// for the TEXT-header component instead of falling back to TemplateParams.
// This protects positional templates where header {{1}} and body {{1}}
// would otherwise share the same value.
func TestWorker_sendTemplateMessage_HeaderParamsTakePrecedence(t *testing.T) {
	w := testWorker(t)

	var capturedBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&capturedBody)
		rw.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(rw).Encode(map[string]any{
			"messages": []map[string]any{{"id": "wamid.hp-explicit"}},
		})
	}))
	defer server.Close()
	w.WhatsApp = whatsapp.NewWithBaseURL(w.Log, server.URL)

	account := &models.WhatsAppAccount{PhoneID: "123", BusinessID: "456", AccessToken: "token", APIVersion: "v21.0"}
	template := &models.Template{
		Name:          "tpl_explicit_hp",
		Language:      "en",
		HeaderType:    "TEXT",
		HeaderContent: "Code {{1}}",
		BodyContent:   "Use {{1}} to redeem",
	}
	recipient := &models.BulkMessageRecipient{
		PhoneNumber:    "1234567890",
		HeaderParams:   models.JSONB{"1": "HEADER-VAL"},
		TemplateParams: models.JSONB{"1": "BODY-VAL"},
	}

	_, err := w.sendTemplateMessage(context.Background(), account, template, recipient, "", "")
	require.NoError(t, err)

	components := capturedBody["template"].(map[string]any)["components"].([]any)
	var headerComp, bodyComp map[string]any
	for _, c := range components {
		m := c.(map[string]any)
		switch m["type"] {
		case "header":
			headerComp = m
		case "body":
			bodyComp = m
		}
	}
	require.NotNil(t, headerComp)
	require.NotNil(t, bodyComp)
	assert.Equal(t, "HEADER-VAL", headerComp["parameters"].([]any)[0].(map[string]any)["text"])
	assert.Equal(t, "BODY-VAL", bodyComp["parameters"].([]any)[0].(map[string]any)["text"],
		"body {{1}} must keep its own value when HeaderParams is set")
}

// Legacy recipient rows persisted before HeaderParams existed only have
// TemplateParams. For NAMED templates the var name is unique across
// components, so the worker falls back to TemplateParams[name].
func TestWorker_sendTemplateMessage_HeaderParamsFallbackToTemplateParams(t *testing.T) {
	w := testWorker(t)

	var capturedBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&capturedBody)
		rw.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(rw).Encode(map[string]any{
			"messages": []map[string]any{{"id": "wamid.hp-fallback"}},
		})
	}))
	defer server.Close()
	w.WhatsApp = whatsapp.NewWithBaseURL(w.Log, server.URL)

	account := &models.WhatsAppAccount{PhoneID: "123", BusinessID: "456", AccessToken: "token", APIVersion: "v21.0"}
	template := &models.Template{
		Name:          "tpl_fallback_hp",
		Language:      "en",
		HeaderType:    "TEXT",
		HeaderContent: "Our {{season}} sale",
		BodyContent:   "Hi {{name}}",
	}
	recipient := &models.BulkMessageRecipient{
		PhoneNumber: "1234567890",
		// No HeaderParams — legacy row. season lives in TemplateParams.
		TemplateParams: models.JSONB{"season": "Summer", "name": "Alex"},
	}

	_, err := w.sendTemplateMessage(context.Background(), account, template, recipient, "", "")
	require.NoError(t, err)

	components := capturedBody["template"].(map[string]any)["components"].([]any)
	var headerComp map[string]any
	for _, c := range components {
		if m := c.(map[string]any); m["type"] == "header" {
			headerComp = m
			break
		}
	}
	require.NotNil(t, headerComp, "header component must still be emitted via fallback")
	params := headerComp["parameters"].([]any)[0].(map[string]any)
	assert.Equal(t, "Summer", params["text"])
	assert.Equal(t, "season", params["parameter_name"])
}

func TestWorker_sendTemplateMessage_NoParams(t *testing.T) {
	w := testWorker(t)

	// Create mock server
	var capturedBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&capturedBody)
		rw.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(rw).Encode(map[string]any{
			"messages": []map[string]any{
				{"id": "wamid.test456"},
			},
		})
	}))
	defer server.Close()

	w.WhatsApp = whatsapp.NewWithBaseURL(w.Log, server.URL)

	account := &models.WhatsAppAccount{
		PhoneID:     "123",
		BusinessID:  "456",
		AccessToken: "token",
		APIVersion:  "v21.0",
	}

	template := &models.Template{
		Name:     "simple_template",
		Language: "en",
	}

	recipient := &models.BulkMessageRecipient{
		PhoneNumber:    "1234567890",
		TemplateParams: nil, // No params
	}

	msgID, err := w.sendTemplateMessage(context.Background(), account, template, recipient, "", "")
	require.NoError(t, err)
	assert.Equal(t, "wamid.test456", msgID)

	// Verify no components when no params
	templateData := capturedBody["template"].(map[string]any)
	components, hasComponents := templateData["components"]
	if hasComponents {
		assert.Empty(t, components)
	}
}

func TestWorker_Close_NilConsumer(t *testing.T) {
	w := &Worker{
		Consumer: nil, // No consumer
	}

	err := w.Close()
	assert.NoError(t, err)
}

func TestWorker_HandleRecipientJob_Success(t *testing.T) {
	w := testWorker(t)
	org, account, template, campaign, recipient := createTestCampaignData(t, w)

	// Create mock server
	server := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		rw.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(rw).Encode(map[string]any{
			"messages": []map[string]any{
				{"id": "wamid.success123"},
			},
		})
	}))
	defer server.Close()

	// Update account's API version for URL building
	require.NoError(t, w.DB.Model(account).Update("api_version", "v21.0").Error)

	w.WhatsApp = whatsapp.NewWithBaseURL(w.Log, server.URL)

	job := &queue.RecipientJob{
		CampaignID:     campaign.ID,
		RecipientID:    recipient.ID,
		OrganizationID: org.ID,
		PhoneNumber:    recipient.PhoneNumber,
		RecipientName:  recipient.RecipientName,
		TemplateParams: recipient.TemplateParams,
		EnqueuedAt:     campaign.StartedAt.UTC(),
	}

	err := w.HandleRecipientJob(context.Background(), job)
	require.NoError(t, err)

	// Verify recipient status updated
	var updatedRecipient models.BulkMessageRecipient
	require.NoError(t, w.DB.First(&updatedRecipient, recipient.ID).Error)
	assert.Equal(t, models.MessageStatusSent, updatedRecipient.Status)
	assert.Equal(t, "wamid.success123", updatedRecipient.WhatsAppMessageID)

	// Verify campaign count incremented
	var updatedCampaign models.BulkMessageCampaign
	require.NoError(t, w.DB.First(&updatedCampaign, campaign.ID).Error)
	assert.Equal(t, 1, updatedCampaign.SentCount)

	// Verify message record created
	var message models.Message
	require.NoError(t, w.DB.Where("template_name = ?", template.Name).First(&message).Error)
	assert.Equal(t, models.MessageStatusSent, message.Status)
	assert.Equal(t, models.DirectionOutgoing, message.Direction)
	assert.Equal(t, models.MessageTypeTemplate, message.MessageType)
}

func TestWorker_HandleRecipientJob_RepairsMirrorBeforeProviderAttempt(t *testing.T) {
	w := testWorker(t)
	organization, account, template, campaign, recipient := createTestCampaignData(t, w)
	require.NoError(t, w.DB.Model(account).Update("api_version", "v21.0").Error)

	var providerRequests atomic.Int32
	mirrorVisibility := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		providerRequests.Add(1)
		// This query runs through the root DB while the provider transaction owns
		// its connection. It can observe the link only if the mirror committed in
		// the preceding independent transaction.
		var visible models.Message
		visibilityErr := w.DB.Select("id", "inbox_conversation_id").
			Where("organization_id = ? AND template_name = ?", organization.ID, template.Name).
			First(&visible).Error
		if visibilityErr == nil && visible.InboxConversationID == nil {
			visibilityErr = errors.New("provider observed an uncommitted campaign inbox mirror")
		}
		mirrorVisibility <- visibilityErr
		if visibilityErr != nil {
			http.Error(rw, "mirror not committed", http.StatusConflict)
			return
		}
		rw.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(rw).Encode(map[string]any{
			"messages": []map[string]any{{"id": "wamid.mirror-repair"}},
		})
	}))
	defer server.Close()
	w.WhatsApp = whatsapp.NewWithBaseURL(w.Log, server.URL)

	forcedMirrorFailure := errors.New("forced pre-provider mirror failure")
	var rejectMirror atomic.Bool
	rejectMirror.Store(true)
	callbackName := "test:reject_campaign_pre_provider_mirror:" + uuid.NewString()
	require.NoError(t, w.DB.Callback().Create().Before("gorm:create").Register(callbackName, func(tx *gorm.DB) {
		if rejectMirror.Load() && tx.Statement.Schema != nil && tx.Statement.Schema.Table == "channel_accounts" {
			_ = tx.AddError(forcedMirrorFailure)
		}
	}))
	t.Cleanup(func() { _ = w.DB.Callback().Create().Remove(callbackName) })

	job := &queue.RecipientJob{
		CampaignID:     campaign.ID,
		RecipientID:    recipient.ID,
		OrganizationID: organization.ID,
		PhoneNumber:    recipient.PhoneNumber,
		RecipientName:  recipient.RecipientName,
		TemplateParams: recipient.TemplateParams,
		EnqueuedAt:     campaign.StartedAt.UTC(),
	}
	err := w.HandleRecipientJob(context.Background(), job)
	require.ErrorIs(t, err, forcedMirrorFailure)
	assert.Zero(t, providerRequests.Load(), "Graph must not run when the inbox mirror fails")

	var released models.BulkMessageRecipient
	require.NoError(t, w.DB.First(&released, recipient.ID).Error)
	assert.Equal(t, models.MessageStatusPending, released.Status)
	assert.Nil(t, released.MessageID)
	var abandoned int64
	require.NoError(t, w.DB.Unscoped().Model(&models.Message{}).
		Where("organization_id = ? AND template_name = ?", organization.ID, template.Name).
		Count(&abandoned).Error)
	assert.Zero(t, abandoned)

	rejectMirror.Store(false)
	require.NoError(t, w.HandleRecipientJob(context.Background(), job))
	assert.Equal(t, int32(1), providerRequests.Load())
	require.NoError(t, <-mirrorVisibility)

	var message models.Message
	require.NoError(t, w.DB.Where(
		"organization_id = ? AND template_name = ?",
		organization.ID,
		template.Name,
	).First(&message).Error)
	assert.Equal(t, models.MessageStatusSent, message.Status)
	assert.Equal(t, "wamid.mirror-repair", message.WhatsAppMessageID)
	assert.NotNil(t, message.InboxConversationID)
}

func TestWorker_MirrorPreparedCampaignDelivery_ReleasesClaimAfterCallerCancellation(t *testing.T) {
	w := testWorker(t)
	organization, account, template, campaign, recipient := createTestCampaignData(t, w)
	contact := &models.Contact{
		BaseModel:       models.BaseModel{ID: uuid.New()},
		OrganizationID:  organization.ID,
		PhoneNumber:     recipient.PhoneNumber,
		ProfileName:     recipient.RecipientName,
		WhatsAppAccount: account.Name,
	}
	require.NoError(t, w.DB.Create(contact).Error)
	job := &queue.RecipientJob{
		CampaignID:     campaign.ID,
		RecipientID:    recipient.ID,
		OrganizationID: organization.ID,
		PhoneNumber:    recipient.PhoneNumber,
		RecipientName:  recipient.RecipientName,
		TemplateParams: recipient.TemplateParams,
		EnqueuedAt:     campaign.StartedAt.UTC(),
	}
	prepared, err := w.prepareCampaignRecipient(context.Background(), job, campaign, contact.ID)
	require.NoError(t, err)
	require.NotNil(t, prepared.message)

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	prepared, err = w.mirrorPreparedCampaignDelivery(cancelled, job, account, prepared)
	require.ErrorIs(t, err, context.Canceled)
	assert.Nil(t, prepared.message)

	var storedRecipient models.BulkMessageRecipient
	require.NoError(t, w.DB.First(&storedRecipient, recipient.ID).Error)
	assert.Equal(t, models.MessageStatusPending, storedRecipient.Status)
	assert.Nil(t, storedRecipient.MessageID)
	var messageCount int64
	require.NoError(t, w.DB.Unscoped().Model(&models.Message{}).
		Where("organization_id = ? AND template_name = ?", organization.ID, template.Name).
		Count(&messageCount).Error)
	assert.Zero(t, messageCount)
}

func TestWorker_AttemptPreparedCampaignDelivery_RecoversAfterCallerCancellation(t *testing.T) {
	w := testWorker(t)
	organization, account, _, campaign, recipient := createTestCampaignData(t, w)
	require.NoError(t, w.DB.Model(account).Update("api_version", "v21.0").Error)
	contact := &models.Contact{
		BaseModel:       models.BaseModel{ID: uuid.New()},
		OrganizationID:  organization.ID,
		PhoneNumber:     recipient.PhoneNumber,
		ProfileName:     recipient.RecipientName,
		WhatsAppAccount: account.Name,
	}
	require.NoError(t, w.DB.Create(contact).Error)
	job := &queue.RecipientJob{
		CampaignID:     campaign.ID,
		RecipientID:    recipient.ID,
		OrganizationID: organization.ID,
		PhoneNumber:    recipient.PhoneNumber,
		RecipientName:  recipient.RecipientName,
		TemplateParams: recipient.TemplateParams,
		EnqueuedAt:     campaign.StartedAt.UTC(),
	}
	prepared, err := w.prepareCampaignRecipient(context.Background(), job, campaign, contact.ID)
	require.NoError(t, err)
	prepared, err = w.mirrorPreparedCampaignDelivery(context.Background(), job, account, prepared)
	require.NoError(t, err)

	var providerRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		providerRequests.Add(1)
		rw.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(rw).Encode(map[string]any{
			"messages": []map[string]any{{"id": "wamid.cancelled-settlement"}},
		})
	}))
	defer server.Close()
	w.WhatsApp = whatsapp.NewWithBaseURL(w.Log, server.URL)

	ctx, cancel := context.WithCancel(context.Background())
	forcedOuterFailure := errors.New("forced post-provider transaction failure")
	var failSettlement atomic.Bool
	failSettlement.Store(true)
	callbackName := "test:cancel_post_provider_settlement:" + uuid.NewString()
	require.NoError(t, w.DB.Callback().Update().Before("gorm:update").Register(callbackName, func(tx *gorm.DB) {
		if tx.Statement.Schema != nil && tx.Statement.Schema.Table == "messages" &&
			failSettlement.CompareAndSwap(true, false) {
			cancel()
			_ = tx.AddError(forcedOuterFailure)
		}
	}))
	t.Cleanup(func() { _ = w.DB.Callback().Update().Remove(callbackName) })

	delivery, err := w.attemptPreparedCampaignDelivery(
		ctx,
		job,
		campaign,
		account,
		contact.ID,
		prepared,
	)
	require.NoError(t, err)
	require.NoError(t, delivery.sendErr)
	require.NotNil(t, delivery.message)
	assert.Equal(t, "wamid.cancelled-settlement", delivery.message.WhatsAppMessageID)
	assert.Equal(t, int32(1), providerRequests.Load())

	var storedRecipient models.BulkMessageRecipient
	require.NoError(t, w.DB.First(&storedRecipient, recipient.ID).Error)
	assert.Equal(t, models.MessageStatusSent, storedRecipient.Status)
	assert.Equal(t, "wamid.cancelled-settlement", storedRecipient.WhatsAppMessageID)
	var storedCampaign models.BulkMessageCampaign
	require.NoError(t, w.DB.First(&storedCampaign, campaign.ID).Error)
	assert.Equal(t, 1, storedCampaign.SentCount)
	assert.Zero(t, storedCampaign.FailedCount)
}

func TestWorker_AttemptPreparedCampaignDelivery_DisconnectedAccountDoesNotCallGraph(t *testing.T) {
	w := testWorker(t)
	org, account, _, campaign, recipient := createTestCampaignData(t, w)
	contact := &models.Contact{
		BaseModel:       models.BaseModel{ID: uuid.New()},
		OrganizationID:  org.ID,
		PhoneNumber:     recipient.PhoneNumber,
		ProfileName:     recipient.RecipientName,
		WhatsAppAccount: account.Name,
	}
	require.NoError(t, w.DB.Create(contact).Error)

	job := &queue.RecipientJob{
		CampaignID:     campaign.ID,
		RecipientID:    recipient.ID,
		OrganizationID: org.ID,
		PhoneNumber:    recipient.PhoneNumber,
		RecipientName:  recipient.RecipientName,
		TemplateParams: recipient.TemplateParams,
		EnqueuedAt:     campaign.StartedAt.UTC(),
	}
	prepared, err := w.prepareCampaignRecipient(
		context.Background(),
		job,
		campaign,
		contact.ID,
	)
	require.NoError(t, err)
	require.NotNil(t, prepared.message)
	prepared, err = w.mirrorPreparedCampaignDelivery(context.Background(), job, account, prepared)
	require.NoError(t, err)
	require.NotEqual(t, uuid.Nil, prepared.mirrorConversationID)

	// Simulate an offboarding/reconnect lifecycle event after the durable mirror
	// but before the provider phase starts.
	require.NoError(t, w.DB.Model(&models.WhatsAppAccount{}).
		Where("id = ? AND organization_id = ?", account.ID, org.ID).
		Update("status", "disconnected").Error)

	var providerRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		providerRequests.Add(1)
		rw.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(rw).Encode(map[string]any{
			"messages": []map[string]any{{"id": "wamid.must-not-send"}},
		})
	}))
	defer server.Close()
	w.WhatsApp = whatsapp.NewWithBaseURL(w.Log, server.URL)

	delivery, err := w.attemptPreparedCampaignDelivery(
		context.Background(),
		job,
		campaign,
		account,
		contact.ID,
		prepared,
	)
	require.NoError(t, err)
	require.ErrorIs(t, delivery.sendErr, whatsappaccount.ErrOutboundInactive)
	assert.Zero(t, providerRequests.Load(), "a disconnected account must make zero Graph requests")

	var storedMessage models.Message
	require.NoError(t, w.DB.First(&storedMessage, prepared.message.ID).Error)
	assert.Equal(t, models.MessageStatusFailed, storedMessage.Status)
	assert.Equal(t, whatsappaccount.ErrOutboundInactive.Error(), storedMessage.ErrorMessage)

	var storedRecipient models.BulkMessageRecipient
	require.NoError(t, w.DB.First(&storedRecipient, recipient.ID).Error)
	assert.Equal(t, models.MessageStatusFailed, storedRecipient.Status)
	assert.Equal(t, whatsappaccount.ErrOutboundInactive.Error(), storedRecipient.ErrorMessage)

	var storedCampaign models.BulkMessageCampaign
	require.NoError(t, w.DB.First(&storedCampaign, campaign.ID).Error)
	assert.Equal(t, 1, storedCampaign.FailedCount)
	assert.Zero(t, storedCampaign.SentCount)
}

func TestWorker_AttemptPreparedCampaignDelivery_UsesCredentialFromFinalLockedRow(t *testing.T) {
	w := testWorker(t)
	org, account, _, campaign, recipient := createTestCampaignData(t, w)
	contact := &models.Contact{
		BaseModel:       models.BaseModel{ID: uuid.New()},
		OrganizationID:  org.ID,
		PhoneNumber:     recipient.PhoneNumber,
		ProfileName:     recipient.RecipientName,
		WhatsAppAccount: account.Name,
	}
	require.NoError(t, w.DB.Create(contact).Error)

	job := &queue.RecipientJob{
		CampaignID:     campaign.ID,
		RecipientID:    recipient.ID,
		OrganizationID: org.ID,
		PhoneNumber:    recipient.PhoneNumber,
		RecipientName:  recipient.RecipientName,
		TemplateParams: recipient.TemplateParams,
		EnqueuedAt:     campaign.StartedAt.UTC(),
	}
	prepared, err := w.prepareCampaignRecipient(
		context.Background(),
		job,
		campaign,
		contact.ID,
	)
	require.NoError(t, err)
	require.NotNil(t, prepared.message)
	prepared, err = w.mirrorPreparedCampaignDelivery(context.Background(), job, account, prepared)
	require.NoError(t, err)
	require.NotEqual(t, uuid.Nil, prepared.mirrorConversationID)

	// The worker's earlier projection is stale, while the locked database row
	// contains the credential generation established by Embedded Signup.
	account.AccessToken = "superseded-token"
	observedAuthorization := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		observedAuthorization <- r.Header.Get("Authorization")
		rw.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(rw).Encode(map[string]any{
			"messages": []map[string]any{{"id": "wamid.current-credential"}},
		})
	}))
	defer server.Close()
	w.WhatsApp = whatsapp.NewWithBaseURL(w.Log, server.URL)

	delivery, err := w.attemptPreparedCampaignDelivery(
		context.Background(),
		job,
		campaign,
		account,
		contact.ID,
		prepared,
	)
	require.NoError(t, err)
	require.NoError(t, delivery.sendErr)
	assert.Equal(t, "Bearer test-token", <-observedAuthorization)
}

func TestWorker_AttemptPreparedCampaignDelivery_SerializesPauseAndCancel(t *testing.T) {
	for _, status := range []models.CampaignStatus{
		models.CampaignStatusPaused,
		models.CampaignStatusCancelled,
	} {
		t.Run(string(status), func(t *testing.T) {
			w := testWorker(t)
			organization, account, _, campaign, recipient := createTestCampaignData(t, w)
			require.NoError(t, w.DB.Model(account).Update("api_version", "v21.0").Error)
			contact := &models.Contact{
				BaseModel:       models.BaseModel{ID: uuid.New()},
				OrganizationID:  organization.ID,
				PhoneNumber:     recipient.PhoneNumber,
				ProfileName:     recipient.RecipientName,
				WhatsAppAccount: account.Name,
			}
			require.NoError(t, w.DB.Create(contact).Error)
			job := &queue.RecipientJob{
				CampaignID:     campaign.ID,
				RecipientID:    recipient.ID,
				OrganizationID: organization.ID,
				PhoneNumber:    recipient.PhoneNumber,
				RecipientName:  recipient.RecipientName,
				TemplateParams: recipient.TemplateParams,
				EnqueuedAt:     campaign.StartedAt.UTC(),
			}
			prepared, err := w.prepareCampaignRecipient(
				context.Background(),
				job,
				campaign,
				contact.ID,
			)
			require.NoError(t, err)
			require.NotNil(t, prepared.message)
			prepared, err = w.mirrorPreparedCampaignDelivery(context.Background(), job, account, prepared)
			require.NoError(t, err)
			require.NotEqual(t, uuid.Nil, prepared.mirrorConversationID)

			var providerRequests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
				providerRequests.Add(1)
				rw.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(rw).Encode(map[string]any{
					"messages": []map[string]any{{"id": "wamid.must-not-send"}},
				})
			}))
			defer server.Close()
			w.WhatsApp = whatsapp.NewWithBaseURL(w.Log, server.URL)

			statusTx := w.DB.Begin()
			require.NoError(t, statusTx.Error)
			t.Cleanup(func() { _ = statusTx.Rollback().Error })
			require.NoError(t, statusTx.Model(&models.BulkMessageCampaign{}).
				Where("id = ? AND organization_id = ?", campaign.ID, organization.ID).
				Update("status", status).Error)

			type attemptResult struct {
				delivery campaignRecipientDelivery
				err      error
			}
			result := make(chan attemptResult, 1)
			go func() {
				delivery, attemptErr := w.attemptPreparedCampaignDelivery(
					context.Background(),
					job,
					campaign,
					account,
					contact.ID,
					prepared,
				)
				result <- attemptResult{delivery: delivery, err: attemptErr}
			}()

			select {
			case attempt := <-result:
				t.Fatalf("provider phase bypassed pending %s commit: %#v", status, attempt)
			case <-time.After(75 * time.Millisecond):
			}
			assert.Zero(t, providerRequests.Load())
			require.NoError(t, statusTx.Commit().Error)

			select {
			case attempt := <-result:
				require.NoError(t, attempt.err)
				assert.True(t, attempt.delivery.campaignInactive)
				require.NotNil(t, attempt.delivery.message)
				assert.Equal(t, models.MessageStatusFailed, attempt.delivery.message.Status)
				assert.Equal(t, campaignInactiveBeforeDeliveryMessage, attempt.delivery.message.ErrorMessage)
				assert.Equal(t, prepared.mirrorConversationID, *attempt.delivery.message.InboxConversationID)
			case <-time.After(5 * time.Second):
				t.Fatalf("provider phase did not observe committed %s status", status)
			}
			assert.Zero(t, providerRequests.Load(), "inactive campaign must make zero Graph requests")

			var storedRecipient models.BulkMessageRecipient
			require.NoError(t, w.DB.First(&storedRecipient, recipient.ID).Error)
			assert.Equal(t, models.MessageStatusFailed, storedRecipient.Status)
			require.NotNil(t, storedRecipient.MessageID)
			assert.Equal(t, prepared.message.ID, *storedRecipient.MessageID)
			assert.Equal(t, campaignInactiveBeforeDeliveryMessage, storedRecipient.ErrorMessage)

			var storedMessage models.Message
			require.NoError(t, w.DB.First(&storedMessage, prepared.message.ID).Error)
			assert.Equal(t, models.MessageStatusFailed, storedMessage.Status)
			require.NotNil(t, storedMessage.InboxConversationID)
			assert.Equal(t, prepared.mirrorConversationID, *storedMessage.InboxConversationID)
			var linkedMessages int64
			require.NoError(t, w.DB.Model(&models.Message{}).
				Where("organization_id = ? AND inbox_conversation_id = ?", organization.ID, prepared.mirrorConversationID).
				Count(&linkedMessages).Error)
			assert.Equal(t, int64(1), linkedMessages, "terminal mirror must retain its exact message")

			var storedCampaign models.BulkMessageCampaign
			require.NoError(t, w.DB.First(&storedCampaign, campaign.ID).Error)
			assert.Equal(t, 1, storedCampaign.FailedCount)
			assert.Zero(t, storedCampaign.SentCount)
		})
	}
}

func startPendingWorkerContactMerge(
	t *testing.T,
	db *gorm.DB,
	organizationID, sourceID, targetID uuid.UUID,
) *gorm.DB {
	t.Helper()
	tx := db.Begin()
	require.NoError(t, tx.Error)
	t.Cleanup(func() { _ = tx.Rollback().Error })
	var source models.Contact
	require.NoError(t, tx.Unscoped().
		Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("id = ? AND organization_id = ?", sourceID, organizationID).
		First(&source).Error)
	mergedAt := time.Now().UTC()
	require.NoError(t, tx.Unscoped().Model(&models.Contact{}).
		Where("id = ? AND organization_id = ?", sourceID, organizationID).
		Updates(map[string]any{
			"merged_into_id": targetID,
			"merged_at":      mergedAt,
			"deleted_at":     mergedAt,
		}).Error)
	return tx
}

func TestWorker_HandleRecipientJob_WaitsForMergeAndHonorsCanonicalMarketingOptOut(t *testing.T) {
	w := testWorker(t)
	org, account, _, campaign, recipient := createTestCampaignData(t, w)
	require.NoError(t, w.DB.Model(account).Update("api_version", "v21.0").Error)

	source := &models.Contact{
		BaseModel:       models.BaseModel{ID: uuid.New()},
		OrganizationID:  org.ID,
		PhoneNumber:     recipient.PhoneNumber,
		ProfileName:     recipient.RecipientName,
		WhatsAppAccount: account.Name,
		MarketingOptOut: false,
	}
	require.NoError(t, w.DB.Create(source).Error)
	canonical := &models.Contact{
		BaseModel:       models.BaseModel{ID: uuid.New()},
		OrganizationID:  org.ID,
		PhoneNumber:     "60123456789",
		ProfileName:     "Canonical opted-out contact",
		WhatsAppAccount: account.Name,
		MarketingOptOut: true,
	}
	require.NoError(t, w.DB.Create(canonical).Error)

	var providerRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		providerRequests.Add(1)
		rw.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(rw).Encode(map[string]any{
			"messages": []map[string]any{{"id": "wamid.must-not-send"}},
		})
	}))
	defer server.Close()
	w.WhatsApp = whatsapp.NewWithBaseURL(w.Log, server.URL)

	mergeTx := startPendingWorkerContactMerge(t, w.DB, org.ID, source.ID, canonical.ID)

	job := &queue.RecipientJob{
		CampaignID:     campaign.ID,
		RecipientID:    recipient.ID,
		OrganizationID: org.ID,
		PhoneNumber:    recipient.PhoneNumber,
		RecipientName:  recipient.RecipientName,
		TemplateParams: recipient.TemplateParams,
		EnqueuedAt:     campaign.StartedAt.UTC(),
	}
	result := make(chan error, 1)
	go func() {
		result <- w.HandleRecipientJob(context.Background(), job)
	}()

	select {
	case err := <-result:
		t.Fatalf("campaign sender bypassed the pending contact merge: %v", err)
	case <-time.After(75 * time.Millisecond):
	}
	assert.Zero(t, providerRequests.Load())
	require.NoError(t, mergeTx.Commit().Error)

	select {
	case err := <-result:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("campaign sender did not finish after contact merge committed")
	}

	assert.Zero(t, providerRequests.Load())
	var updatedRecipient models.BulkMessageRecipient
	require.NoError(t, w.DB.First(&updatedRecipient, recipient.ID).Error)
	assert.Equal(t, models.MessageStatusFailed, updatedRecipient.Status)
	assert.Equal(t, campaignMarketingOptOutMessage, updatedRecipient.ErrorMessage)
	assert.Empty(t, updatedRecipient.WhatsAppMessageID)
	assert.Nil(t, updatedRecipient.MessageID)

	var updatedCampaign models.BulkMessageCampaign
	require.NoError(t, w.DB.First(&updatedCampaign, campaign.ID).Error)
	assert.Equal(t, 1, updatedCampaign.FailedCount)
	assert.Zero(t, updatedCampaign.SentCount)

	var messageCount int64
	require.NoError(t, w.DB.Model(&models.Message{}).
		Where("organization_id = ?", org.ID).
		Count(&messageCount).Error)
	assert.Zero(t, messageCount)
}

func TestWorker_HandleRecipientJob_DeadLettersUnresolvedClaimWithoutResend(t *testing.T) {
	w := testWorker(t)
	org, account, _, campaign, recipient := createTestCampaignData(t, w)
	contact := &models.Contact{
		BaseModel:       models.BaseModel{ID: uuid.New()},
		OrganizationID:  org.ID,
		PhoneNumber:     recipient.PhoneNumber,
		ProfileName:     recipient.RecipientName,
		WhatsAppAccount: account.Name,
	}
	require.NoError(t, w.DB.Create(contact).Error)

	prepared, err := w.prepareCampaignRecipient(
		context.Background(),
		&queue.RecipientJob{
			CampaignID:     campaign.ID,
			RecipientID:    recipient.ID,
			OrganizationID: org.ID,
			PhoneNumber:    recipient.PhoneNumber,
			RecipientName:  recipient.RecipientName,
			TemplateParams: recipient.TemplateParams,
			EnqueuedAt:     campaign.StartedAt.UTC(),
		},
		campaign,
		contact.ID,
	)
	require.NoError(t, err)
	require.NotNil(t, prepared.message)
	assert.Equal(t, models.MessageStatusPending, prepared.message.Status)

	var providerRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		providerRequests.Add(1)
		rw.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(rw).Encode(map[string]any{
			"messages": []map[string]any{{"id": "wamid.must-not-retry"}},
		})
	}))
	defer server.Close()
	w.WhatsApp = whatsapp.NewWithBaseURL(w.Log, server.URL)

	job := &queue.RecipientJob{
		CampaignID:     campaign.ID,
		RecipientID:    recipient.ID,
		OrganizationID: org.ID,
		PhoneNumber:    recipient.PhoneNumber,
		RecipientName:  recipient.RecipientName,
		TemplateParams: recipient.TemplateParams,
		EnqueuedAt:     campaign.StartedAt.UTC(),
	}
	require.NoError(t, w.HandleRecipientJob(context.Background(), job))
	assert.Zero(t, providerRequests.Load())

	var storedMessage models.Message
	require.NoError(t, w.DB.First(&storedMessage, prepared.message.ID).Error)
	assert.Equal(t, models.MessageStatusFailed, storedMessage.Status)
	assert.Equal(t, campaignAmbiguousDeliveryMessage, storedMessage.ErrorMessage)
	var storedRecipient models.BulkMessageRecipient
	require.NoError(t, w.DB.First(&storedRecipient, recipient.ID).Error)
	assert.Equal(t, models.MessageStatusFailed, storedRecipient.Status)
	assert.Equal(t, campaignAmbiguousDeliveryMessage, storedRecipient.ErrorMessage)
	require.NotNil(t, storedRecipient.MessageID)
	assert.Equal(t, storedMessage.ID, *storedRecipient.MessageID)
	var storedCampaign models.BulkMessageCampaign
	require.NoError(t, w.DB.First(&storedCampaign, campaign.ID).Error)
	assert.Equal(t, 1, storedCampaign.FailedCount)
	assert.Zero(t, storedCampaign.SentCount)
}

func TestWorker_HandleRecipientJob_WaitsForMergeAndPersistsCanonicalMessage(t *testing.T) {
	w := testWorker(t)
	org, account, template, campaign, recipient := createTestCampaignData(t, w)
	require.NoError(t, w.DB.Model(account).Update("api_version", "v21.0").Error)

	source := &models.Contact{
		BaseModel:       models.BaseModel{ID: uuid.New()},
		OrganizationID:  org.ID,
		PhoneNumber:     recipient.PhoneNumber,
		ProfileName:     recipient.RecipientName,
		WhatsAppAccount: account.Name,
	}
	require.NoError(t, w.DB.Create(source).Error)
	canonical := &models.Contact{
		BaseModel:       models.BaseModel{ID: uuid.New()},
		OrganizationID:  org.ID,
		PhoneNumber:     "60198765432",
		ProfileName:     "Canonical campaign contact",
		WhatsAppAccount: account.Name,
	}
	require.NoError(t, w.DB.Create(canonical).Error)

	providerPayload := make(chan map[string]any, 2)
	server := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("decode provider payload: %v", err)
			rw.WriteHeader(http.StatusBadRequest)
			return
		}
		providerPayload <- payload
		rw.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(rw).Encode(map[string]any{
			"messages": []map[string]any{{"id": "wamid.canonical-campaign"}},
		})
	}))
	defer server.Close()
	w.WhatsApp = whatsapp.NewWithBaseURL(w.Log, server.URL)

	mergeTx := startPendingWorkerContactMerge(t, w.DB, org.ID, source.ID, canonical.ID)
	job := &queue.RecipientJob{
		CampaignID:     campaign.ID,
		RecipientID:    recipient.ID,
		OrganizationID: org.ID,
		PhoneNumber:    recipient.PhoneNumber,
		RecipientName:  recipient.RecipientName,
		TemplateParams: recipient.TemplateParams,
		EnqueuedAt:     campaign.StartedAt.UTC(),
	}
	result := make(chan error, 1)
	go func() {
		result <- w.HandleRecipientJob(context.Background(), job)
	}()

	select {
	case err := <-result:
		t.Fatalf("campaign sender bypassed the pending contact merge: %v", err)
	case <-time.After(75 * time.Millisecond):
	}
	select {
	case payload := <-providerPayload:
		t.Fatalf("provider send started before the merge committed: %#v", payload)
	default:
	}
	require.NoError(t, mergeTx.Commit().Error)

	select {
	case err := <-result:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("campaign sender did not finish after contact merge committed")
	}

	select {
	case payload := <-providerPayload:
		assert.Equal(t, canonical.PhoneNumber, payload["to"])
	case <-time.After(time.Second):
		t.Fatal("provider did not receive the campaign message")
	}
	select {
	case payload := <-providerPayload:
		t.Fatalf("provider received a duplicate campaign message: %#v", payload)
	default:
	}

	var message models.Message
	require.NoError(t, w.DB.Where(
		"organization_id = ? AND template_name = ?",
		org.ID,
		template.Name,
	).First(&message).Error)
	assert.Equal(t, canonical.ID, message.ContactID)
	assert.Equal(t, "wamid.canonical-campaign", message.WhatsAppMessageID)
	assert.Equal(t, models.MessageStatusSent, message.Status)

	var sourceMessageCount int64
	require.NoError(t, w.DB.Model(&models.Message{}).
		Where("organization_id = ? AND contact_id = ?", org.ID, source.ID).
		Count(&sourceMessageCount).Error)
	assert.Zero(t, sourceMessageCount)

	var updatedRecipient models.BulkMessageRecipient
	require.NoError(t, w.DB.First(&updatedRecipient, recipient.ID).Error)
	require.NotNil(t, updatedRecipient.MessageID)
	assert.Equal(t, message.ID, *updatedRecipient.MessageID)
	assert.Equal(t, models.MessageStatusSent, updatedRecipient.Status)

	require.NoError(t, w.HandleRecipientJob(context.Background(), job))
	select {
	case payload := <-providerPayload:
		t.Fatalf("redelivery sent a duplicate provider message: %#v", payload)
	default:
	}
	var updatedCampaign models.BulkMessageCampaign
	require.NoError(t, w.DB.First(&updatedCampaign, campaign.ID).Error)
	assert.Equal(t, 1, updatedCampaign.SentCount)
	assert.Zero(t, updatedCampaign.FailedCount)
}

func TestWorker_HandleRecipientJob_WhatsAppError(t *testing.T) {
	w := testWorker(t)
	org, account, _, campaign, recipient := createTestCampaignData(t, w)

	// Create mock server that returns an error
	server := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		rw.Header().Set("Content-Type", "application/json")
		rw.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(rw).Encode(map[string]any{
			"error": map[string]any{
				"message": "Invalid phone number",
				"code":    100,
			},
		})
	}))
	defer server.Close()

	require.NoError(t, w.DB.Model(account).Update("api_version", "v21.0").Error)
	w.WhatsApp = whatsapp.NewWithBaseURL(w.Log, server.URL)

	job := &queue.RecipientJob{
		CampaignID:     campaign.ID,
		RecipientID:    recipient.ID,
		OrganizationID: org.ID,
		PhoneNumber:    recipient.PhoneNumber,
		RecipientName:  recipient.RecipientName,
		TemplateParams: recipient.TemplateParams,
		EnqueuedAt:     campaign.StartedAt.UTC(),
	}

	err := w.HandleRecipientJob(context.Background(), job)
	require.NoError(t, err) // Job handler returns nil to not retry

	// Verify recipient marked as failed
	var updatedRecipient models.BulkMessageRecipient
	require.NoError(t, w.DB.First(&updatedRecipient, recipient.ID).Error)
	assert.Equal(t, models.MessageStatusFailed, updatedRecipient.Status)
	assert.NotEmpty(t, updatedRecipient.ErrorMessage)

	// Verify campaign failed count incremented
	var updatedCampaign models.BulkMessageCampaign
	require.NoError(t, w.DB.First(&updatedCampaign, campaign.ID).Error)
	assert.Equal(t, 1, updatedCampaign.FailedCount)
}

func TestCampaignAmbiguousDeliveryErrorPreservesDurableMessage(t *testing.T) {
	const want = "Provider delivery outcome is unknown; message was not retried to prevent a duplicate"
	assert.Equal(t, want, campaignAmbiguousDeliveryMessage)
	assert.EqualError(t, campaignAmbiguousDeliveryError{}, want)
}

func TestWorker_HandleRecipientJob_AmbiguousProviderResultNeverCountsSent(t *testing.T) {
	for _, tc := range []struct {
		name             string
		configure        func(*testing.T, *Worker, *atomic.Int32)
		wantProviderCall int32
	}{
		{
			name: "blank provider message ID",
			configure: func(t *testing.T, w *Worker, requests *atomic.Int32) {
				server := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
					requests.Add(1)
					rw.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(rw).Encode(map[string]any{
						"messages": []map[string]any{{"id": "   "}},
					})
				}))
				t.Cleanup(server.Close)
				w.WhatsApp = whatsapp.NewWithBaseURL(w.Log, server.URL)
			},
			wantProviderCall: 1,
		},
		{
			name: "provider panic",
			configure: func(_ *testing.T, w *Worker, _ *atomic.Int32) {
				w.WhatsApp = nil
			},
			wantProviderCall: 0,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := testWorker(t)
			organization, account, _, campaign, recipient := createTestCampaignData(t, w)
			require.NoError(t, w.DB.Model(account).Update("api_version", "v21.0").Error)
			var providerRequests atomic.Int32
			tc.configure(t, w, &providerRequests)

			job := &queue.RecipientJob{
				CampaignID:     campaign.ID,
				RecipientID:    recipient.ID,
				OrganizationID: organization.ID,
				PhoneNumber:    recipient.PhoneNumber,
				RecipientName:  recipient.RecipientName,
				TemplateParams: recipient.TemplateParams,
				EnqueuedAt:     campaign.StartedAt.UTC(),
			}
			require.NotPanics(t, func() {
				require.NoError(t, w.HandleRecipientJob(context.Background(), job))
			})

			var storedRecipient models.BulkMessageRecipient
			require.NoError(t, w.DB.First(&storedRecipient, recipient.ID).Error)
			assert.Equal(t, models.MessageStatusFailed, storedRecipient.Status)
			assert.Empty(t, storedRecipient.WhatsAppMessageID)
			assert.Equal(t, campaignAmbiguousDeliveryMessage, storedRecipient.ErrorMessage)
			require.NotNil(t, storedRecipient.MessageID)

			var storedMessage models.Message
			require.NoError(t, w.DB.First(&storedMessage, *storedRecipient.MessageID).Error)
			assert.Equal(t, models.MessageStatusFailed, storedMessage.Status)
			assert.Empty(t, storedMessage.WhatsAppMessageID)
			assert.Equal(t, campaignAmbiguousDeliveryMessage, storedMessage.ErrorMessage)
			assert.NotNil(t, storedMessage.InboxConversationID)

			var storedCampaign models.BulkMessageCampaign
			require.NoError(t, w.DB.First(&storedCampaign, campaign.ID).Error)
			assert.Zero(t, storedCampaign.SentCount)
			assert.Equal(t, 1, storedCampaign.FailedCount)
			assert.Equal(t, tc.wantProviderCall, providerRequests.Load())

			// The durable terminal settlement is the no-resend fence.
			require.NoError(t, w.HandleRecipientJob(context.Background(), job))
			assert.Equal(t, tc.wantProviderCall, providerRequests.Load())
			require.NoError(t, w.DB.First(&storedCampaign, campaign.ID).Error)
			assert.Zero(t, storedCampaign.SentCount)
			assert.Equal(t, 1, storedCampaign.FailedCount)
		})
	}
}

func TestWorker_HandleRecipientJob_CreatesContact(t *testing.T) {
	w := testWorker(t)
	org, account, _, campaign, recipient := createTestCampaignData(t, w)

	// Create mock server
	server := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		rw.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(rw).Encode(map[string]any{
			"messages": []map[string]any{
				{"id": "wamid.contact123"},
			},
		})
	}))
	defer server.Close()

	require.NoError(t, w.DB.Model(account).Update("api_version", "v21.0").Error)
	w.WhatsApp = whatsapp.NewWithBaseURL(w.Log, server.URL)

	// Use a new phone number that doesn't have a contact
	newPhone := "9998887777"
	job := &queue.RecipientJob{
		CampaignID:     campaign.ID,
		RecipientID:    recipient.ID,
		OrganizationID: org.ID,
		PhoneNumber:    newPhone,
		RecipientName:  "New Contact",
		TemplateParams: recipient.TemplateParams,
		EnqueuedAt:     campaign.StartedAt.UTC(),
	}

	err := w.HandleRecipientJob(context.Background(), job)
	require.NoError(t, err)

	// Verify contact was created
	var contact models.Contact
	require.NoError(t, w.DB.Where("organization_id = ? AND phone_number = ?", org.ID, newPhone).First(&contact).Error)
	assert.Equal(t, "New Contact", contact.ProfileName)
}

func TestWorker_HandleRecipientJob_CampaignCompletion(t *testing.T) {
	w := testWorker(t)
	org, account, _, campaign, recipient := createTestCampaignData(t, w)

	// Create mock server
	server := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		rw.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(rw).Encode(map[string]any{
			"messages": []map[string]any{
				{"id": "wamid.complete123"},
			},
		})
	}))
	defer server.Close()

	require.NoError(t, w.DB.Model(account).Update("api_version", "v21.0").Error)
	w.WhatsApp = whatsapp.NewWithBaseURL(w.Log, server.URL)

	job := &queue.RecipientJob{
		CampaignID:     campaign.ID,
		RecipientID:    recipient.ID,
		OrganizationID: org.ID,
		PhoneNumber:    recipient.PhoneNumber,
		RecipientName:  recipient.RecipientName,
		TemplateParams: recipient.TemplateParams,
		EnqueuedAt:     campaign.StartedAt.UTC(),
	}

	err := w.HandleRecipientJob(context.Background(), job)
	require.NoError(t, err)

	// Verify campaign is marked as completed (all recipients processed)
	var updatedCampaign models.BulkMessageCampaign
	require.NoError(t, w.DB.First(&updatedCampaign, campaign.ID).Error)
	assert.Equal(t, models.CampaignStatusCompleted, updatedCampaign.Status)
	assert.NotNil(t, updatedCampaign.CompletedAt)
}

func TestWorker_HandleRecipientJob_TemplateParamSubstitution(t *testing.T) {
	w := testWorker(t)
	org, account, template, campaign, recipient := createTestCampaignData(t, w)

	// Create mock server
	server := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		rw.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(rw).Encode(map[string]any{
			"messages": []map[string]any{
				{"id": "wamid.subst123"},
			},
		})
	}))
	defer server.Close()

	require.NoError(t, w.DB.Model(account).Update("api_version", "v21.0").Error)
	w.WhatsApp = whatsapp.NewWithBaseURL(w.Log, server.URL)

	job := &queue.RecipientJob{
		CampaignID:     campaign.ID,
		RecipientID:    recipient.ID,
		OrganizationID: org.ID,
		PhoneNumber:    recipient.PhoneNumber,
		RecipientName:  recipient.RecipientName,
		TemplateParams: models.JSONB{
			"1": "Alice",
			"2": "ORD-456",
		},
		EnqueuedAt: campaign.StartedAt.UTC(),
	}

	err := w.HandleRecipientJob(context.Background(), job)
	require.NoError(t, err)

	// Verify message content has substituted params
	var message models.Message
	require.NoError(t, w.DB.Where("template_name = ?", template.Name).Order("created_at desc").First(&message).Error)
	assert.Contains(t, message.Content, "Alice")
	assert.Contains(t, message.Content, "ORD-456")
	assert.NotContains(t, message.Content, "{{1}}")
	assert.NotContains(t, message.Content, "{{2}}")
}

func TestWorker_DecryptAccountSecrets_WithEncryptionKey(t *testing.T) {
	w := &Worker{
		Config: &config.Config{
			App: config.AppConfig{EncryptionKey: "test-secret-key-for-aes256"},
		},
	}

	// Encrypt a token
	plainToken := "EAAI2ZCP4ZAMv8BQtest"
	plainSecret := "app-secret-123"
	encToken, err := crypto.Encrypt(plainToken, w.Config.App.EncryptionKey)
	require.NoError(t, err)
	encSecret, err := crypto.Encrypt(plainSecret, w.Config.App.EncryptionKey)
	require.NoError(t, err)

	// Verify they are actually encrypted
	assert.True(t, crypto.IsEncrypted(encToken))
	assert.True(t, crypto.IsEncrypted(encSecret))

	account := &models.WhatsAppAccount{
		AccessToken: encToken,
		AppSecret:   encSecret,
	}

	w.decryptAccountSecrets(account)

	assert.Equal(t, plainToken, account.AccessToken)
	assert.Equal(t, plainSecret, account.AppSecret)
}

func TestWorker_DecryptAccountSecrets_NilConfig(t *testing.T) {
	w := &Worker{}

	account := &models.WhatsAppAccount{
		AccessToken: "plain-token",
		AppSecret:   "plain-secret",
	}

	w.decryptAccountSecrets(account)

	// Should remain unchanged (no-op)
	assert.Equal(t, "plain-token", account.AccessToken)
	assert.Equal(t, "plain-secret", account.AppSecret)
}

func TestWorker_HandleRecipientJob_WithEncryptedToken(t *testing.T) {
	w := testWorker(t)

	encKey := "test-encryption-key-for-aes"
	w.Config = &config.Config{
		App: config.AppConfig{EncryptionKey: encKey},
	}

	org, account, template, campaign, recipient := createTestCampaignData(t, w)

	// Encrypt the token in the DB (simulating production)
	encToken, err := crypto.Encrypt("test-token", encKey)
	require.NoError(t, err)
	require.NoError(t, w.DB.Model(account).Updates(map[string]any{
		"access_token": encToken,
		"api_version":  "v21.0",
	}).Error)

	// Create mock server that verifies the decrypted token arrives
	var capturedAuth string
	server := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		capturedAuth = r.Header.Get("Authorization")
		rw.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(rw).Encode(map[string]any{
			"messages": []map[string]any{
				{"id": "wamid.encrypted123"},
			},
		})
	}))
	defer server.Close()

	w.WhatsApp = whatsapp.NewWithBaseURL(w.Log, server.URL)

	job := &queue.RecipientJob{
		CampaignID:     campaign.ID,
		RecipientID:    recipient.ID,
		OrganizationID: org.ID,
		PhoneNumber:    recipient.PhoneNumber,
		RecipientName:  recipient.RecipientName,
		TemplateParams: recipient.TemplateParams,
		EnqueuedAt:     campaign.StartedAt.UTC(),
	}

	err = w.HandleRecipientJob(context.Background(), job)
	require.NoError(t, err)

	// Verify the decrypted token was sent to Meta API (not the encrypted one)
	assert.Equal(t, "Bearer test-token", capturedAuth)
	assert.NotContains(t, capturedAuth, "enc:")

	// Verify recipient marked as sent
	var updatedRecipient models.BulkMessageRecipient
	require.NoError(t, w.DB.First(&updatedRecipient, recipient.ID).Error)
	assert.Equal(t, models.MessageStatusSent, updatedRecipient.Status)
	assert.Equal(t, "wamid.encrypted123", updatedRecipient.WhatsAppMessageID)

	// Verify message record created
	var message models.Message
	require.NoError(t, w.DB.Where("template_name = ?", template.Name).Order("created_at desc").First(&message).Error)
	assert.Equal(t, models.MessageStatusSent, message.Status)
}

// Unit tests for parameter resolution functions (no database required)

func TestResolveTemplateParams_NamedParams(t *testing.T) {
	bodyContent := "Hello {{name}}, your order {{order_id}} is ready!"
	params := models.JSONB{
		"name":     "John",
		"order_id": "ORD-123",
	}

	result := templateutil.ResolveParams(bodyContent, params)

	assert.Equal(t, []string{"John", "ORD-123"}, result)
}

func TestResolveTemplateParams_PositionalParams(t *testing.T) {
	bodyContent := "Hello {{1}}, your order {{2}} is ready!"
	params := models.JSONB{
		"1": "John",
		"2": "ORD-123",
	}

	result := templateutil.ResolveParams(bodyContent, params)

	assert.Equal(t, []string{"John", "ORD-123"}, result)
}

func TestResolveTemplateParams_FallbackToPositional(t *testing.T) {
	// Named params in template, but user provides positional params
	bodyContent := "Hello {{name}}, your order {{order_id}} is ready!"
	params := models.JSONB{
		"1": "John",
		"2": "ORD-123",
	}

	result := templateutil.ResolveParams(bodyContent, params)

	assert.Equal(t, []string{"John", "ORD-123"}, result)
}

func TestResolveTemplateParams_MixedParams(t *testing.T) {
	// User provides some named, some positional
	bodyContent := "Hello {{name}}, your order {{order_id}} is ready!"
	params := models.JSONB{
		"name": "John",
		"2":    "ORD-123", // Positional fallback for second param
	}

	result := templateutil.ResolveParams(bodyContent, params)

	assert.Equal(t, []string{"John", "ORD-123"}, result)
}

func TestResolveTemplateParams_NoParams(t *testing.T) {
	// Template without any parameters
	bodyContent := "Hello, your order is ready!"
	params := models.JSONB{
		"1": "John",
		"2": "ORD-123",
	}

	result := templateutil.ResolveParams(bodyContent, params)

	assert.Nil(t, result)
}

func TestResolveTemplateParams_EmptyParams(t *testing.T) {
	bodyContent := "Hello {{name}}!"
	params := models.JSONB{}

	result := templateutil.ResolveParams(bodyContent, params)

	assert.Nil(t, result)
}

func TestReplaceTemplateContent_NamedParams(t *testing.T) {
	bodyContent := "Hello {{name}}, your order {{order_id}} is ready!"
	content := "Hello {{name}}, your order {{order_id}} is ready!"
	params := models.JSONB{
		"name":     "John",
		"order_id": "ORD-123",
	}

	result := templateutil.ReplaceWithJSONBParams(bodyContent, content, params)

	assert.Equal(t, "Hello John, your order ORD-123 is ready!", result)
}

func TestReplaceTemplateContent_PositionalParams(t *testing.T) {
	bodyContent := "Hello {{1}}, your order {{2}} is ready!"
	content := "Hello {{1}}, your order {{2}} is ready!"
	params := models.JSONB{
		"1": "John",
		"2": "ORD-123",
	}

	result := templateutil.ReplaceWithJSONBParams(bodyContent, content, params)

	assert.Equal(t, "Hello John, your order ORD-123 is ready!", result)
}

func TestReplaceTemplateContent_NamedParamsWithPositionalInput(t *testing.T) {
	// Template has named placeholders but user provides positional params
	bodyContent := "Hello {{name}}, your order {{order_id}} is ready!"
	content := "Hello {{name}}, your order {{order_id}} is ready!"
	params := models.JSONB{
		"1": "John",
		"2": "ORD-123",
	}

	result := templateutil.ReplaceWithJSONBParams(bodyContent, content, params)

	assert.Equal(t, "Hello John, your order ORD-123 is ready!", result)
}

func TestReplaceTemplateContent_NoParams(t *testing.T) {
	// Template without any parameters
	bodyContent := "Hello, your order is ready!"
	content := "Hello, your order is ready!"
	params := models.JSONB{
		"1": "John",
		"2": "ORD-123",
	}

	result := templateutil.ReplaceWithJSONBParams(bodyContent, content, params)

	assert.Equal(t, "Hello, your order is ready!", result)
}
