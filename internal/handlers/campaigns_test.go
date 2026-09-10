package handlers_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/handlers"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/internal/queue"
	"github.com/shridarpatil/whatomate/internal/storage"
	"github.com/shridarpatil/whatomate/pkg/whatsapp"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
	"github.com/zerodha/fastglue"
	"gorm.io/gorm"
)

// createTestCampaign creates a test campaign in the database.
func createTestCampaign(t *testing.T, app *handlers.App, orgID, templateID, userID uuid.UUID, whatsappAccount string, status models.CampaignStatus) *models.BulkMessageCampaign {
	t.Helper()

	campaign := &models.BulkMessageCampaign{
		BaseModel:       models.BaseModel{ID: uuid.New()},
		OrganizationID:  orgID,
		Name:            "Test Campaign " + uuid.New().String()[:8],
		WhatsAppAccount: whatsappAccount,
		TemplateID:      templateID,
		Status:          status,
		CreatedBy:       userID,
	}
	require.NoError(t, app.DB.Create(campaign).Error)
	return campaign
}

// createTestRecipient creates a test recipient for a campaign.
func createTestRecipient(t *testing.T, app *handlers.App, campaignID uuid.UUID, phone string, status models.MessageStatus) *models.BulkMessageRecipient {
	t.Helper()

	recipient := &models.BulkMessageRecipient{
		BaseModel:     models.BaseModel{ID: uuid.New()},
		CampaignID:    campaignID,
		PhoneNumber:   phone,
		RecipientName: "Test Recipient",
		Status:        status,
	}
	require.NoError(t, app.DB.Create(recipient).Error)
	return recipient
}

// --- ListCampaigns Tests ---

func TestApp_ListCampaigns_Success(t *testing.T) {
	mockQueue := testutil.NewMockQueue()
	app := newTestApp(t, withQueue(mockQueue))
	org := testutil.CreateTestOrganization(t, app.DB)
	user := testutil.CreateTestUser(t, app.DB, org.ID, testutil.WithEmail(testutil.UniqueEmail("list-campaigns")), testutil.WithPassword("password"))
	account := testutil.CreateTestWhatsAppAccountWith(t, app.DB, org.ID, testutil.WithAccountName("test-account"))
	template := testutil.CreateTestTemplate(t, app.DB, org.ID, account.Name)

	// Create multiple campaigns
	createTestCampaign(t, app, org.ID, template.ID, user.ID, account.Name, models.CampaignStatusDraft)
	createTestCampaign(t, app, org.ID, template.ID, user.ID, account.Name, models.CampaignStatusCompleted)

	req := testutil.NewGETRequest(t)
	testutil.SetAuthContext(req, org.ID, user.ID)

	err := app.ListCampaigns(req)
	require.NoError(t, err)
	assert.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req))

	var resp struct {
		Data struct {
			Campaigns []handlers.CampaignResponse `json:"campaigns"`
			Total     int                         `json:"total"`
		} `json:"data"`
	}
	err = json.Unmarshal(testutil.GetResponseBody(req), &resp)
	require.NoError(t, err)
	assert.Equal(t, 2, resp.Data.Total)
	assert.Len(t, resp.Data.Campaigns, 2)
}

func TestApp_ListCampaigns_FilterByStatus(t *testing.T) {
	mockQueue := testutil.NewMockQueue()
	app := newTestApp(t, withQueue(mockQueue))
	org := testutil.CreateTestOrganization(t, app.DB)
	user := testutil.CreateTestUser(t, app.DB, org.ID, testutil.WithEmail(testutil.UniqueEmail("list-filter")), testutil.WithPassword("password"))
	account := testutil.CreateTestWhatsAppAccountWith(t, app.DB, org.ID, testutil.WithAccountName("test-account-filter"))
	template := testutil.CreateTestTemplate(t, app.DB, org.ID, account.Name)

	createTestCampaign(t, app, org.ID, template.ID, user.ID, account.Name, models.CampaignStatusDraft)
	createTestCampaign(t, app, org.ID, template.ID, user.ID, account.Name, models.CampaignStatusCompleted)

	req := testutil.NewGETRequest(t)
	testutil.SetAuthContext(req, org.ID, user.ID)
	testutil.SetQueryParam(req, "status", models.CampaignStatusDraft)

	err := app.ListCampaigns(req)
	require.NoError(t, err)
	assert.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req))

	var resp struct {
		Data struct {
			Campaigns []handlers.CampaignResponse `json:"campaigns"`
			Total     int                         `json:"total"`
		} `json:"data"`
	}
	err = json.Unmarshal(testutil.GetResponseBody(req), &resp)
	require.NoError(t, err)
	assert.Equal(t, 1, resp.Data.Total)
	assert.Equal(t, models.CampaignStatusDraft, resp.Data.Campaigns[0].Status)
}

func TestApp_ListCampaigns_Unauthorized(t *testing.T) {
	mockQueue := testutil.NewMockQueue()
	app := newTestApp(t, withQueue(mockQueue))

	req := testutil.NewGETRequest(t)
	// No auth context set

	err := app.ListCampaigns(req)
	require.NoError(t, err)
	assert.Equal(t, fasthttp.StatusUnauthorized, testutil.GetResponseStatusCode(req))
}

// --- CreateCampaign Tests ---

func TestApp_CreateCampaign_Success(t *testing.T) {
	mockQueue := testutil.NewMockQueue()
	app := newTestApp(t, withQueue(mockQueue))
	org := testutil.CreateTestOrganization(t, app.DB)
	user := testutil.CreateTestUser(t, app.DB, org.ID, testutil.WithEmail(testutil.UniqueEmail("create-campaign")), testutil.WithPassword("password"))
	account := testutil.CreateTestWhatsAppAccountWith(t, app.DB, org.ID, testutil.WithAccountName("create-account"))
	template := testutil.CreateTestTemplate(t, app.DB, org.ID, account.Name)

	req := testutil.NewJSONRequest(t, map[string]any{
		"name":             "Test Campaign",
		"whatsapp_account": account.Name,
		"template_id":      template.ID.String(),
	})
	testutil.SetAuthContext(req, org.ID, user.ID)

	err := app.CreateCampaign(req)
	require.NoError(t, err)
	assert.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req))

	var resp struct {
		Data handlers.CampaignResponse `json:"data"`
	}
	err = json.Unmarshal(testutil.GetResponseBody(req), &resp)
	require.NoError(t, err)
	assert.Equal(t, "Test Campaign", resp.Data.Name)
	assert.Equal(t, models.CampaignStatusDraft, resp.Data.Status)
	assert.Equal(t, template.ID, resp.Data.TemplateID)
}

func TestApp_CreateCampaign_WithScheduledAt(t *testing.T) {
	mockQueue := testutil.NewMockQueue()
	app := newTestApp(t, withQueue(mockQueue))
	org := testutil.CreateTestOrganization(t, app.DB)
	user := testutil.CreateTestUser(t, app.DB, org.ID, testutil.WithEmail(testutil.UniqueEmail("create-scheduled")), testutil.WithPassword("password"))
	account := testutil.CreateTestWhatsAppAccountWith(t, app.DB, org.ID, testutil.WithAccountName("scheduled-account"))
	template := testutil.CreateTestTemplate(t, app.DB, org.ID, account.Name)

	scheduledAt := time.Now().Add(24 * time.Hour).Format(time.RFC3339)

	req := testutil.NewJSONRequest(t, map[string]any{
		"name":             "Scheduled Campaign",
		"whatsapp_account": account.Name,
		"template_id":      template.ID.String(),
		"scheduled_at":     scheduledAt,
	})
	testutil.SetAuthContext(req, org.ID, user.ID)

	err := app.CreateCampaign(req)
	require.NoError(t, err)
	assert.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req))

	var resp struct {
		Data handlers.CampaignResponse `json:"data"`
	}
	err = json.Unmarshal(testutil.GetResponseBody(req), &resp)
	require.NoError(t, err)
	assert.NotNil(t, resp.Data.ScheduledAt)
}

func TestApp_CreateCampaign_InvalidTemplateID(t *testing.T) {
	mockQueue := testutil.NewMockQueue()
	app := newTestApp(t, withQueue(mockQueue))
	org := testutil.CreateTestOrganization(t, app.DB)
	user := testutil.CreateTestUser(t, app.DB, org.ID, testutil.WithEmail(testutil.UniqueEmail("invalid-template")), testutil.WithPassword("password"))
	account := testutil.CreateTestWhatsAppAccountWith(t, app.DB, org.ID, testutil.WithAccountName("invalid-template-account"))

	req := testutil.NewJSONRequest(t, map[string]any{
		"name":             "Test Campaign",
		"whatsapp_account": account.Name,
		"template_id":      "not-a-valid-uuid",
	})
	testutil.SetAuthContext(req, org.ID, user.ID)

	err := app.CreateCampaign(req)
	require.NoError(t, err)
	assert.Equal(t, fasthttp.StatusBadRequest, testutil.GetResponseStatusCode(req))
}

func TestApp_CreateCampaign_TemplateNotFound(t *testing.T) {
	mockQueue := testutil.NewMockQueue()
	app := newTestApp(t, withQueue(mockQueue))
	org := testutil.CreateTestOrganization(t, app.DB)
	user := testutil.CreateTestUser(t, app.DB, org.ID, testutil.WithEmail(testutil.UniqueEmail("template-not-found")), testutil.WithPassword("password"))
	account := testutil.CreateTestWhatsAppAccountWith(t, app.DB, org.ID, testutil.WithAccountName("no-template-account"))

	req := testutil.NewJSONRequest(t, map[string]any{
		"name":             "Test Campaign",
		"whatsapp_account": account.Name,
		"template_id":      uuid.New().String(),
	})
	testutil.SetAuthContext(req, org.ID, user.ID)

	err := app.CreateCampaign(req)
	require.NoError(t, err)
	assert.Equal(t, fasthttp.StatusNotFound, testutil.GetResponseStatusCode(req))
}

func TestApp_CreateCampaign_AccountNotFound(t *testing.T) {
	mockQueue := testutil.NewMockQueue()
	app := newTestApp(t, withQueue(mockQueue))
	org := testutil.CreateTestOrganization(t, app.DB)
	user := testutil.CreateTestUser(t, app.DB, org.ID, testutil.WithEmail(testutil.UniqueEmail("account-not-found")), testutil.WithPassword("password"))
	account := testutil.CreateTestWhatsAppAccountWith(t, app.DB, org.ID, testutil.WithAccountName("temp-account-for-template"))
	template := testutil.CreateTestTemplate(t, app.DB, org.ID, account.Name)

	req := testutil.NewJSONRequest(t, map[string]any{
		"name":             "Test Campaign",
		"whatsapp_account": "nonexistent-account",
		"template_id":      template.ID.String(),
	})
	testutil.SetAuthContext(req, org.ID, user.ID)

	err := app.CreateCampaign(req)
	require.NoError(t, err)
	assert.Equal(t, fasthttp.StatusBadRequest, testutil.GetResponseStatusCode(req))
}

func TestApp_CreateCampaign_InvalidRequestBody(t *testing.T) {
	mockQueue := testutil.NewMockQueue()
	app := newTestApp(t, withQueue(mockQueue))
	org := testutil.CreateTestOrganization(t, app.DB)
	user := testutil.CreateTestUser(t, app.DB, org.ID, testutil.WithEmail(testutil.UniqueEmail("invalid-body")), testutil.WithPassword("password"))

	req := testutil.NewRequest(t)
	req.RequestCtx.Request.SetBody([]byte("invalid json"))
	req.RequestCtx.Request.Header.SetContentType("application/json")
	testutil.SetAuthContext(req, org.ID, user.ID)

	err := app.CreateCampaign(req)
	require.NoError(t, err)
	assert.Equal(t, fasthttp.StatusBadRequest, testutil.GetResponseStatusCode(req))
}

// --- GetCampaign Tests ---

func TestApp_GetCampaign_Success(t *testing.T) {
	mockQueue := testutil.NewMockQueue()
	app := newTestApp(t, withQueue(mockQueue))
	org := testutil.CreateTestOrganization(t, app.DB)
	user := testutil.CreateTestUser(t, app.DB, org.ID, testutil.WithEmail(testutil.UniqueEmail("get-campaign")), testutil.WithPassword("password"))
	account := testutil.CreateTestWhatsAppAccountWith(t, app.DB, org.ID, testutil.WithAccountName("get-account"))
	template := testutil.CreateTestTemplate(t, app.DB, org.ID, account.Name)
	campaign := createTestCampaign(t, app, org.ID, template.ID, user.ID, account.Name, models.CampaignStatusDraft)

	// Set a non-zero read count to verify it is returned
	campaign.ReadCount = 10
	require.NoError(t, app.DB.Save(campaign).Error)

	req := testutil.NewGETRequest(t)
	testutil.SetAuthContext(req, org.ID, user.ID)
	testutil.SetPathParam(req, "id", campaign.ID.String())

	err := app.GetCampaign(req)
	require.NoError(t, err)
	assert.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req))

	var resp struct {
		Data handlers.CampaignResponse `json:"data"`
	}
	err = json.Unmarshal(testutil.GetResponseBody(req), &resp)
	require.NoError(t, err)
	assert.Equal(t, campaign.ID, resp.Data.ID)
	assert.Equal(t, campaign.Name, resp.Data.Name)
	assert.Equal(t, 10, resp.Data.ReadCount)
}

func TestApp_GetCampaign_NotFound(t *testing.T) {
	mockQueue := testutil.NewMockQueue()
	app := newTestApp(t, withQueue(mockQueue))
	org := testutil.CreateTestOrganization(t, app.DB)
	user := testutil.CreateTestUser(t, app.DB, org.ID, testutil.WithEmail(testutil.UniqueEmail("get-not-found")), testutil.WithPassword("password"))

	req := testutil.NewGETRequest(t)
	testutil.SetAuthContext(req, org.ID, user.ID)
	testutil.SetPathParam(req, "id", uuid.New().String())

	err := app.GetCampaign(req)
	require.NoError(t, err)
	assert.Equal(t, fasthttp.StatusNotFound, testutil.GetResponseStatusCode(req))
}

func TestApp_GetCampaign_InvalidID(t *testing.T) {
	mockQueue := testutil.NewMockQueue()
	app := newTestApp(t, withQueue(mockQueue))
	org := testutil.CreateTestOrganization(t, app.DB)
	user := testutil.CreateTestUser(t, app.DB, org.ID, testutil.WithEmail(testutil.UniqueEmail("get-invalid-id")), testutil.WithPassword("password"))

	req := testutil.NewGETRequest(t)
	testutil.SetAuthContext(req, org.ID, user.ID)
	testutil.SetPathParam(req, "id", "not-a-uuid")

	err := app.GetCampaign(req)
	require.NoError(t, err)
	assert.Equal(t, fasthttp.StatusBadRequest, testutil.GetResponseStatusCode(req))
}

// --- UpdateCampaign Tests ---

func TestApp_UpdateCampaign_Success(t *testing.T) {
	mockQueue := testutil.NewMockQueue()
	app := newTestApp(t, withQueue(mockQueue))
	org := testutil.CreateTestOrganization(t, app.DB)
	user := testutil.CreateTestUser(t, app.DB, org.ID, testutil.WithEmail(testutil.UniqueEmail("update-campaign")), testutil.WithPassword("password"))
	account := testutil.CreateTestWhatsAppAccountWith(t, app.DB, org.ID, testutil.WithAccountName("update-account"))
	template := testutil.CreateTestTemplate(t, app.DB, org.ID, account.Name)
	campaign := createTestCampaign(t, app, org.ID, template.ID, user.ID, account.Name, models.CampaignStatusDraft)

	// Set a non-zero read count to verify it is preserved/returned on update
	campaign.ReadCount = 12
	require.NoError(t, app.DB.Save(campaign).Error)

	req := testutil.NewJSONRequest(t, map[string]any{
		"name":             "Updated Campaign Name",
		"whatsapp_account": account.Name,
		"template_id":      template.ID.String(),
	})
	testutil.SetAuthContext(req, org.ID, user.ID)
	testutil.SetPathParam(req, "id", campaign.ID.String())

	err := app.UpdateCampaign(req)
	require.NoError(t, err)
	assert.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req))

	var resp struct {
		Data handlers.CampaignResponse `json:"data"`
	}
	err = json.Unmarshal(testutil.GetResponseBody(req), &resp)
	require.NoError(t, err)
	assert.Equal(t, "Updated Campaign Name", resp.Data.Name)
	assert.Equal(t, 12, resp.Data.ReadCount)
}

func TestApp_UpdateCampaign_NotDraft(t *testing.T) {
	mockQueue := testutil.NewMockQueue()
	app := newTestApp(t, withQueue(mockQueue))
	org := testutil.CreateTestOrganization(t, app.DB)
	user := testutil.CreateTestUser(t, app.DB, org.ID, testutil.WithEmail(testutil.UniqueEmail("update-not-draft")), testutil.WithPassword("password"))
	account := testutil.CreateTestWhatsAppAccountWith(t, app.DB, org.ID, testutil.WithAccountName("update-not-draft-account"))
	template := testutil.CreateTestTemplate(t, app.DB, org.ID, account.Name)
	campaign := createTestCampaign(t, app, org.ID, template.ID, user.ID, account.Name, models.CampaignStatusProcessing)

	req := testutil.NewJSONRequest(t, map[string]any{
		"name": "Updated Name",
	})
	testutil.SetAuthContext(req, org.ID, user.ID)
	testutil.SetPathParam(req, "id", campaign.ID.String())

	err := app.UpdateCampaign(req)
	require.NoError(t, err)
	assert.Equal(t, fasthttp.StatusBadRequest, testutil.GetResponseStatusCode(req))
}

func TestApp_UpdateCampaign_NotFound(t *testing.T) {
	mockQueue := testutil.NewMockQueue()
	app := newTestApp(t, withQueue(mockQueue))
	org := testutil.CreateTestOrganization(t, app.DB)
	user := testutil.CreateTestUser(t, app.DB, org.ID, testutil.WithEmail(testutil.UniqueEmail("update-not-found")), testutil.WithPassword("password"))

	req := testutil.NewJSONRequest(t, map[string]any{
		"name": "Updated Name",
	})
	testutil.SetAuthContext(req, org.ID, user.ID)
	testutil.SetPathParam(req, "id", uuid.New().String())

	err := app.UpdateCampaign(req)
	require.NoError(t, err)
	assert.Equal(t, fasthttp.StatusNotFound, testutil.GetResponseStatusCode(req))
}

func TestApp_UpdateCampaign_DoesNotOverwriteConcurrentStart(t *testing.T) {
	app := newTestApp(t, withQueue(testutil.NewMockQueue()))
	org := testutil.CreateTestOrganization(t, app.DB)
	user := testutil.CreateTestUser(t, app.DB, org.ID, testutil.WithEmail(testutil.UniqueEmail("update-start-race")), testutil.WithPassword("password"))
	account := testutil.CreateTestWhatsAppAccountWith(t, app.DB, org.ID, testutil.WithAccountName("update-start-race-"+uuid.NewString()[:8]))
	template := testutil.CreateTestTemplate(t, app.DB, org.ID, account.Name)
	campaign := createTestCampaign(t, app, org.ID, template.ID, user.ID, account.Name, models.CampaignStatusDraft)

	transitionStartedAt := time.Now().UTC().Truncate(time.Microsecond)
	transitionTx := app.DB.Begin()
	require.NoError(t, transitionTx.Error)
	t.Cleanup(func() { _ = transitionTx.Rollback().Error })
	require.NoError(t, transitionTx.Model(&models.BulkMessageCampaign{}).
		Where("id = ? AND organization_id = ? AND status = ?", campaign.ID, org.ID, models.CampaignStatusDraft).
		Updates(map[string]any{
			"status":     models.CampaignStatusProcessing,
			"started_at": transitionStartedAt,
		}).Error)

	req := testutil.NewJSONRequest(t, map[string]any{"name": "stale update must not commit"})
	testutil.SetAuthContext(req, org.ID, user.ID)
	testutil.SetPathParam(req, "id", campaign.ID.String())
	pid, done := runCampaignHandlerOnPinnedDB(app, req, (*handlers.App).UpdateCampaign)
	backendPID := <-pid
	require.Positive(t, backendPID)
	testutil.RequirePostgresBackendWaitingForLock(t, app.DB, backendPID)

	require.NoError(t, transitionTx.Commit().Error)
	awaitCampaignHandler(t, done)
	require.Equal(t, fasthttp.StatusBadRequest, testutil.GetResponseStatusCode(req))

	var stored models.BulkMessageCampaign
	require.NoError(t, app.DB.Where("id = ? AND organization_id = ?", campaign.ID, org.ID).First(&stored).Error)
	require.Equal(t, models.CampaignStatusProcessing, stored.Status)
	require.Equal(t, campaign.Name, stored.Name)
	require.NotNil(t, stored.StartedAt)
	require.WithinDuration(t, transitionStartedAt, stored.StartedAt.UTC(), time.Microsecond)
}

func TestApp_UpdateCampaign_AuthorityChangeClearsMediaProjection(t *testing.T) {
	app := newTestApp(t, withQueue(testutil.NewMockQueue()))
	org := testutil.CreateTestOrganization(t, app.DB)
	user := testutil.CreateTestUser(t, app.DB, org.ID, testutil.WithEmail(testutil.UniqueEmail("update-media-authority")), testutil.WithPassword("password"))
	oldAccount := testutil.CreateTestWhatsAppAccountWith(t, app.DB, org.ID, testutil.WithAccountName("update-media-old-"+uuid.NewString()[:8]))
	oldTemplate := testutil.CreateTestTemplate(t, app.DB, org.ID, oldAccount.Name)
	newAccount := testutil.CreateTestWhatsAppAccountWith(t, app.DB, org.ID, testutil.WithAccountName("update-media-new-"+uuid.NewString()[:8]))
	newTemplate := testutil.CreateTestTemplate(t, app.DB, org.ID, newAccount.Name)
	campaign := createTestCampaign(t, app, org.ID, oldTemplate.ID, user.ID, oldAccount.Name, models.CampaignStatusDraft)
	require.NoError(t, app.DB.Model(&models.BulkMessageCampaign{}).
		Where("id = ? AND organization_id = ?", campaign.ID, org.ID).
		Updates(map[string]any{
			"header_media_id":         "old-provider-id",
			"header_media_filename":   "old.jpg",
			"header_media_mime_type":  "image/jpeg",
			"header_media_local_path": "campaigns/old.jpg",
		}).Error)

	req := testutil.NewJSONRequest(t, map[string]any{
		"name":             "new authority",
		"whatsapp_account": newAccount.Name,
		"template_id":      newTemplate.ID.String(),
	})
	testutil.SetAuthContext(req, org.ID, user.ID)
	testutil.SetPathParam(req, "id", campaign.ID.String())
	require.NoError(t, app.UpdateCampaign(req))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req))

	var stored models.BulkMessageCampaign
	require.NoError(t, app.DB.Where("id = ? AND organization_id = ?", campaign.ID, org.ID).First(&stored).Error)
	require.Equal(t, newTemplate.ID, stored.TemplateID)
	require.Equal(t, newAccount.Name, stored.WhatsAppAccount)
	require.Empty(t, stored.HeaderMediaID)
	require.Empty(t, stored.HeaderMediaFilename)
	require.Empty(t, stored.HeaderMediaMimeType)
	require.Empty(t, stored.HeaderMediaLocalPath)
}

// --- DeleteCampaign Tests ---

func TestApp_DeleteCampaign_Success(t *testing.T) {
	mockQueue := testutil.NewMockQueue()
	app := newTestApp(t, withQueue(mockQueue))
	org := testutil.CreateTestOrganization(t, app.DB)
	user := testutil.CreateTestUser(t, app.DB, org.ID, testutil.WithEmail(testutil.UniqueEmail("delete-campaign")), testutil.WithPassword("password"))
	account := testutil.CreateTestWhatsAppAccountWith(t, app.DB, org.ID, testutil.WithAccountName("delete-account"))
	template := testutil.CreateTestTemplate(t, app.DB, org.ID, account.Name)
	campaign := createTestCampaign(t, app, org.ID, template.ID, user.ID, account.Name, models.CampaignStatusDraft)

	req := testutil.NewGETRequest(t)
	testutil.SetAuthContext(req, org.ID, user.ID)
	testutil.SetPathParam(req, "id", campaign.ID.String())

	err := app.DeleteCampaign(req)
	require.NoError(t, err)
	assert.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req))

	// Verify campaign is deleted
	var count int64
	app.DB.Model(&models.BulkMessageCampaign{}).Where("id = ?", campaign.ID).Count(&count)
	assert.Equal(t, int64(0), count)
}

func TestApp_DeleteCampaign_WithRecipients(t *testing.T) {
	mockQueue := testutil.NewMockQueue()
	app := newTestApp(t, withQueue(mockQueue))
	org := testutil.CreateTestOrganization(t, app.DB)
	user := testutil.CreateTestUser(t, app.DB, org.ID, testutil.WithEmail(testutil.UniqueEmail("delete-with-recipients")), testutil.WithPassword("password"))
	account := testutil.CreateTestWhatsAppAccountWith(t, app.DB, org.ID, testutil.WithAccountName("delete-recipients-account"))
	template := testutil.CreateTestTemplate(t, app.DB, org.ID, account.Name)
	campaign := createTestCampaign(t, app, org.ID, template.ID, user.ID, account.Name, models.CampaignStatusDraft)
	createTestRecipient(t, app, campaign.ID, "+1234567890", models.MessageStatusPending)
	createTestRecipient(t, app, campaign.ID, "+0987654321", models.MessageStatusPending)

	req := testutil.NewGETRequest(t)
	testutil.SetAuthContext(req, org.ID, user.ID)
	testutil.SetPathParam(req, "id", campaign.ID.String())

	err := app.DeleteCampaign(req)
	require.NoError(t, err)
	assert.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req))

	// Verify recipients are also deleted
	var count int64
	app.DB.Model(&models.BulkMessageRecipient{}).Where("campaign_id = ?", campaign.ID).Count(&count)
	assert.Equal(t, int64(0), count)
}

func TestApp_DeleteCampaign_RunningCampaign(t *testing.T) {
	mockQueue := testutil.NewMockQueue()
	app := newTestApp(t, withQueue(mockQueue))
	org := testutil.CreateTestOrganization(t, app.DB)
	user := testutil.CreateTestUser(t, app.DB, org.ID, testutil.WithEmail(testutil.UniqueEmail("delete-running")), testutil.WithPassword("password"))
	account := testutil.CreateTestWhatsAppAccountWith(t, app.DB, org.ID, testutil.WithAccountName("delete-running-account"))
	template := testutil.CreateTestTemplate(t, app.DB, org.ID, account.Name)
	campaign := createTestCampaign(t, app, org.ID, template.ID, user.ID, account.Name, models.CampaignStatusProcessing)

	req := testutil.NewGETRequest(t)
	testutil.SetAuthContext(req, org.ID, user.ID)
	testutil.SetPathParam(req, "id", campaign.ID.String())

	err := app.DeleteCampaign(req)
	require.NoError(t, err)
	assert.Equal(t, fasthttp.StatusBadRequest, testutil.GetResponseStatusCode(req))
}

func TestApp_DeleteCampaign_NotFound(t *testing.T) {
	mockQueue := testutil.NewMockQueue()
	app := newTestApp(t, withQueue(mockQueue))
	org := testutil.CreateTestOrganization(t, app.DB)
	user := testutil.CreateTestUser(t, app.DB, org.ID, testutil.WithEmail(testutil.UniqueEmail("delete-not-found")), testutil.WithPassword("password"))

	req := testutil.NewGETRequest(t)
	testutil.SetAuthContext(req, org.ID, user.ID)
	testutil.SetPathParam(req, "id", uuid.New().String())

	err := app.DeleteCampaign(req)
	require.NoError(t, err)
	assert.Equal(t, fasthttp.StatusNotFound, testutil.GetResponseStatusCode(req))
}

// --- StartCampaign Tests ---

func TestApp_StartCampaign_Success(t *testing.T) {
	mockQueue := testutil.NewMockQueue()
	app := newTestApp(t, withQueue(mockQueue))
	org := testutil.CreateTestOrganization(t, app.DB)
	user := testutil.CreateTestUser(t, app.DB, org.ID, testutil.WithEmail(testutil.UniqueEmail("start-campaign")), testutil.WithPassword("password"))
	account := testutil.CreateTestWhatsAppAccountWith(t, app.DB, org.ID, testutil.WithAccountName("start-account"))
	template := testutil.CreateTestTemplate(t, app.DB, org.ID, account.Name)
	campaign := createTestCampaign(t, app, org.ID, template.ID, user.ID, account.Name, models.CampaignStatusDraft)
	createTestRecipient(t, app, campaign.ID, "+1234567890", models.MessageStatusPending)
	createTestRecipient(t, app, campaign.ID, "+0987654321", models.MessageStatusPending)

	req := testutil.NewJSONRequest(t, nil)
	testutil.SetAuthContext(req, org.ID, user.ID)
	testutil.SetPathParam(req, "id", campaign.ID.String())

	err := app.StartCampaign(req)
	require.NoError(t, err)
	assert.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req))

	// Verify jobs were enqueued
	assert.Len(t, mockQueue.Jobs, 2)

	// Verify campaign status changed
	var updated models.BulkMessageCampaign
	app.DB.Where("id = ?", campaign.ID).First(&updated)
	assert.Equal(t, models.CampaignStatusProcessing, updated.Status)
	assert.NotNil(t, updated.StartedAt)
}

func TestApp_StartCampaign_NoPendingRecipients(t *testing.T) {
	mockQueue := testutil.NewMockQueue()
	app := newTestApp(t, withQueue(mockQueue))
	org := testutil.CreateTestOrganization(t, app.DB)
	user := testutil.CreateTestUser(t, app.DB, org.ID, testutil.WithEmail(testutil.UniqueEmail("start-no-recipients")), testutil.WithPassword("password"))
	account := testutil.CreateTestWhatsAppAccountWith(t, app.DB, org.ID, testutil.WithAccountName("start-no-recipients-account"))
	template := testutil.CreateTestTemplate(t, app.DB, org.ID, account.Name)
	campaign := createTestCampaign(t, app, org.ID, template.ID, user.ID, account.Name, models.CampaignStatusDraft)
	// No recipients added

	req := testutil.NewJSONRequest(t, nil)
	testutil.SetAuthContext(req, org.ID, user.ID)
	testutil.SetPathParam(req, "id", campaign.ID.String())

	err := app.StartCampaign(req)
	require.NoError(t, err)
	assert.Equal(t, fasthttp.StatusBadRequest, testutil.GetResponseStatusCode(req))
}

func TestApp_StartCampaign_InvalidStatus(t *testing.T) {
	statuses := []models.CampaignStatus{models.CampaignStatusProcessing, models.CampaignStatusCompleted, models.CampaignStatusCancelled}

	for _, status := range statuses {
		t.Run("status_"+string(status), func(t *testing.T) {
			mockQueue := testutil.NewMockQueue()
			app := newTestApp(t, withQueue(mockQueue))
			org := testutil.CreateTestOrganization(t, app.DB)
			user := testutil.CreateTestUser(t, app.DB, org.ID, testutil.WithEmail(testutil.UniqueEmail("start-invalid-"+string(status))), testutil.WithPassword("password"))
			account := testutil.CreateTestWhatsAppAccountWith(t, app.DB, org.ID, testutil.WithAccountName("start-invalid-"+string(status)))
			template := testutil.CreateTestTemplate(t, app.DB, org.ID, account.Name)
			campaign := createTestCampaign(t, app, org.ID, template.ID, user.ID, account.Name, status)
			createTestRecipient(t, app, campaign.ID, "+1234567890", models.MessageStatusPending)

			req := testutil.NewJSONRequest(t, nil)
			testutil.SetAuthContext(req, org.ID, user.ID)
			testutil.SetPathParam(req, "id", campaign.ID.String())

			err := app.StartCampaign(req)
			require.NoError(t, err)
			assert.Equal(t, fasthttp.StatusBadRequest, testutil.GetResponseStatusCode(req))
		})
	}
}

func TestApp_StartCampaign_CanResumePaused(t *testing.T) {
	mockQueue := testutil.NewMockQueue()
	app := newTestApp(t, withQueue(mockQueue))
	org := testutil.CreateTestOrganization(t, app.DB)
	user := testutil.CreateTestUser(t, app.DB, org.ID, testutil.WithEmail(testutil.UniqueEmail("resume-paused")), testutil.WithPassword("password"))
	account := testutil.CreateTestWhatsAppAccountWith(t, app.DB, org.ID, testutil.WithAccountName("resume-paused-account"))
	template := testutil.CreateTestTemplate(t, app.DB, org.ID, account.Name)
	campaign := createTestCampaign(t, app, org.ID, template.ID, user.ID, account.Name, models.CampaignStatusPaused)
	createTestRecipient(t, app, campaign.ID, "+1234567890", models.MessageStatusPending)

	req := testutil.NewJSONRequest(t, nil)
	testutil.SetAuthContext(req, org.ID, user.ID)
	testutil.SetPathParam(req, "id", campaign.ID.String())

	err := app.StartCampaign(req)
	require.NoError(t, err)
	assert.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req))
	assert.Len(t, mockQueue.Jobs, 1)
}

// --- PauseCampaign Tests ---

func TestApp_PauseCampaign_Success(t *testing.T) {
	mockQueue := testutil.NewMockQueue()
	app := newTestApp(t, withQueue(mockQueue))
	org := testutil.CreateTestOrganization(t, app.DB)
	user := testutil.CreateTestUser(t, app.DB, org.ID, testutil.WithEmail(testutil.UniqueEmail("pause-campaign")), testutil.WithPassword("password"))
	account := testutil.CreateTestWhatsAppAccountWith(t, app.DB, org.ID, testutil.WithAccountName("pause-account"))
	template := testutil.CreateTestTemplate(t, app.DB, org.ID, account.Name)
	campaign := createTestCampaign(t, app, org.ID, template.ID, user.ID, account.Name, models.CampaignStatusProcessing)

	req := testutil.NewJSONRequest(t, nil)
	testutil.SetAuthContext(req, org.ID, user.ID)
	testutil.SetPathParam(req, "id", campaign.ID.String())

	err := app.PauseCampaign(req)
	require.NoError(t, err)
	assert.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req))

	var updated models.BulkMessageCampaign
	app.DB.Where("id = ?", campaign.ID).First(&updated)
	assert.Equal(t, models.CampaignStatusPaused, updated.Status)
}

func TestApp_PauseCampaign_NotRunning(t *testing.T) {
	mockQueue := testutil.NewMockQueue()
	app := newTestApp(t, withQueue(mockQueue))
	org := testutil.CreateTestOrganization(t, app.DB)
	user := testutil.CreateTestUser(t, app.DB, org.ID, testutil.WithEmail(testutil.UniqueEmail("pause-not-running")), testutil.WithPassword("password"))
	account := testutil.CreateTestWhatsAppAccountWith(t, app.DB, org.ID, testutil.WithAccountName("pause-not-running-account"))
	template := testutil.CreateTestTemplate(t, app.DB, org.ID, account.Name)
	campaign := createTestCampaign(t, app, org.ID, template.ID, user.ID, account.Name, models.CampaignStatusDraft)

	req := testutil.NewJSONRequest(t, nil)
	testutil.SetAuthContext(req, org.ID, user.ID)
	testutil.SetPathParam(req, "id", campaign.ID.String())

	err := app.PauseCampaign(req)
	require.NoError(t, err)
	assert.Equal(t, fasthttp.StatusBadRequest, testutil.GetResponseStatusCode(req))
}

// --- CancelCampaign Tests ---

func TestApp_CancelCampaign_Success(t *testing.T) {
	mockQueue := testutil.NewMockQueue()
	app := newTestApp(t, withQueue(mockQueue))
	org := testutil.CreateTestOrganization(t, app.DB)
	user := testutil.CreateTestUser(t, app.DB, org.ID, testutil.WithEmail(testutil.UniqueEmail("cancel-campaign")), testutil.WithPassword("password"))
	account := testutil.CreateTestWhatsAppAccountWith(t, app.DB, org.ID, testutil.WithAccountName("cancel-account"))
	template := testutil.CreateTestTemplate(t, app.DB, org.ID, account.Name)
	campaign := createTestCampaign(t, app, org.ID, template.ID, user.ID, account.Name, models.CampaignStatusProcessing)

	req := testutil.NewJSONRequest(t, nil)
	testutil.SetAuthContext(req, org.ID, user.ID)
	testutil.SetPathParam(req, "id", campaign.ID.String())

	err := app.CancelCampaign(req)
	require.NoError(t, err)
	assert.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req))

	var updated models.BulkMessageCampaign
	app.DB.Where("id = ?", campaign.ID).First(&updated)
	assert.Equal(t, models.CampaignStatusCancelled, updated.Status)
}

func TestApp_CancelCampaign_AlreadyFinished(t *testing.T) {
	finishedStatuses := []models.CampaignStatus{models.CampaignStatusCompleted, models.CampaignStatusCancelled}

	for _, status := range finishedStatuses {
		t.Run("status_"+string(status), func(t *testing.T) {
			mockQueue := testutil.NewMockQueue()
			app := newTestApp(t, withQueue(mockQueue))
			org := testutil.CreateTestOrganization(t, app.DB)
			user := testutil.CreateTestUser(t, app.DB, org.ID, testutil.WithEmail(testutil.UniqueEmail("cancel-finished-"+string(status))), testutil.WithPassword("password"))
			account := testutil.CreateTestWhatsAppAccountWith(t, app.DB, org.ID, testutil.WithAccountName("cancel-finished-"+string(status)))
			template := testutil.CreateTestTemplate(t, app.DB, org.ID, account.Name)
			campaign := createTestCampaign(t, app, org.ID, template.ID, user.ID, account.Name, status)

			req := testutil.NewJSONRequest(t, nil)
			testutil.SetAuthContext(req, org.ID, user.ID)
			testutil.SetPathParam(req, "id", campaign.ID.String())

			err := app.CancelCampaign(req)
			require.NoError(t, err)
			assert.Equal(t, fasthttp.StatusBadRequest, testutil.GetResponseStatusCode(req))
		})
	}
}

// --- ImportRecipients Tests ---

func TestApp_ImportRecipients_Success(t *testing.T) {
	mockQueue := testutil.NewMockQueue()
	app := newTestApp(t, withQueue(mockQueue))
	org := testutil.CreateTestOrganization(t, app.DB)
	user := testutil.CreateTestUser(t, app.DB, org.ID, testutil.WithEmail(testutil.UniqueEmail("import-recipients")), testutil.WithPassword("password"))
	account := testutil.CreateTestWhatsAppAccountWith(t, app.DB, org.ID, testutil.WithAccountName("import-account"))
	template := testutil.CreateTestTemplate(t, app.DB, org.ID, account.Name)
	campaign := createTestCampaign(t, app, org.ID, template.ID, user.ID, account.Name, models.CampaignStatusDraft)

	req := testutil.NewJSONRequest(t, map[string]any{
		"recipients": []map[string]any{
			{"phone_number": "+1234567890", "recipient_name": "John Doe"},
			{"phone_number": "+0987654321", "recipient_name": "Jane Doe"},
		},
	})
	testutil.SetAuthContext(req, org.ID, user.ID)
	testutil.SetPathParam(req, "id", campaign.ID.String())

	err := app.ImportRecipients(req)
	require.NoError(t, err)
	assert.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req))

	var resp struct {
		Data struct {
			Message         string `json:"message"`
			AddedCount      int    `json:"added_count"`
			TotalRecipients int64  `json:"total_recipients"`
		} `json:"data"`
	}
	err = json.Unmarshal(testutil.GetResponseBody(req), &resp)
	require.NoError(t, err)
	assert.Equal(t, 2, resp.Data.AddedCount)
	assert.Equal(t, int64(2), resp.Data.TotalRecipients)
}

func TestApp_ImportRecipients_WithTemplateParams(t *testing.T) {
	mockQueue := testutil.NewMockQueue()
	app := newTestApp(t, withQueue(mockQueue))
	org := testutil.CreateTestOrganization(t, app.DB)
	user := testutil.CreateTestUser(t, app.DB, org.ID, testutil.WithEmail(testutil.UniqueEmail("import-with-params")), testutil.WithPassword("password"))
	account := testutil.CreateTestWhatsAppAccountWith(t, app.DB, org.ID, testutil.WithAccountName("import-params-account"))
	template := testutil.CreateTestTemplate(t, app.DB, org.ID, account.Name)
	campaign := createTestCampaign(t, app, org.ID, template.ID, user.ID, account.Name, models.CampaignStatusDraft)

	req := testutil.NewJSONRequest(t, map[string]any{
		"recipients": []map[string]any{
			{
				"phone_number":    "+1234567890",
				"recipient_name":  "John Doe",
				"template_params": map[string]any{"1": "John", "2": "Welcome"},
			},
		},
	})
	testutil.SetAuthContext(req, org.ID, user.ID)
	testutil.SetPathParam(req, "id", campaign.ID.String())

	err := app.ImportRecipients(req)
	require.NoError(t, err)
	assert.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req))

	// Verify recipient has template params
	var recipient models.BulkMessageRecipient
	app.DB.Where("campaign_id = ?", campaign.ID).First(&recipient)
	assert.NotNil(t, recipient.TemplateParams)
}

// header_params must round-trip into the BulkMessageRecipient row so the
// worker can hand it to BuildTemplateComponents at send time. Without this,
// a positional {{1}} header collides with a positional {{1}} body parameter
// in the flat TemplateParams map (per-component indexing on Meta side).
func TestApp_ImportRecipients_WithHeaderParams(t *testing.T) {
	mockQueue := testutil.NewMockQueue()
	app := newTestApp(t, withQueue(mockQueue))
	org := testutil.CreateTestOrganization(t, app.DB)
	user := testutil.CreateTestUser(t, app.DB, org.ID, testutil.WithEmail(testutil.UniqueEmail("import-header-params")), testutil.WithPassword("password"))
	account := testutil.CreateTestWhatsAppAccountWith(t, app.DB, org.ID, testutil.WithAccountName("import-header-account"))
	template := testutil.CreateTestTemplate(t, app.DB, org.ID, account.Name)
	campaign := createTestCampaign(t, app, org.ID, template.ID, user.ID, account.Name, models.CampaignStatusDraft)

	req := testutil.NewJSONRequest(t, map[string]any{
		"recipients": []map[string]any{
			{
				"phone_number":    "+1234567890",
				"recipient_name":  "John Doe",
				"template_params": map[string]any{"1": "John", "2": "SAVE20"},
				"header_params":   map[string]any{"1": "Summer"},
			},
		},
	})
	testutil.SetAuthContext(req, org.ID, user.ID)
	testutil.SetPathParam(req, "id", campaign.ID.String())

	err := app.ImportRecipients(req)
	require.NoError(t, err)
	assert.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req))

	var recipient models.BulkMessageRecipient
	require.NoError(t, app.DB.Where("campaign_id = ?", campaign.ID).First(&recipient).Error)
	require.NotNil(t, recipient.HeaderParams)
	assert.Equal(t, "Summer", recipient.HeaderParams["1"])
	// TemplateParams stays separate from HeaderParams — same positional key
	// "1" must not collide.
	require.NotNil(t, recipient.TemplateParams)
	assert.Equal(t, "John", recipient.TemplateParams["1"])
	assert.Equal(t, "SAVE20", recipient.TemplateParams["2"])
}

func TestApp_ImportRecipients_NotDraft(t *testing.T) {
	mockQueue := testutil.NewMockQueue()
	app := newTestApp(t, withQueue(mockQueue))
	org := testutil.CreateTestOrganization(t, app.DB)
	user := testutil.CreateTestUser(t, app.DB, org.ID, testutil.WithEmail(testutil.UniqueEmail("import-not-draft")), testutil.WithPassword("password"))
	account := testutil.CreateTestWhatsAppAccountWith(t, app.DB, org.ID, testutil.WithAccountName("import-not-draft-account"))
	template := testutil.CreateTestTemplate(t, app.DB, org.ID, account.Name)
	campaign := createTestCampaign(t, app, org.ID, template.ID, user.ID, account.Name, models.CampaignStatusProcessing)

	req := testutil.NewJSONRequest(t, map[string]any{
		"recipients": []map[string]any{
			{"phone_number": "+1234567890"},
		},
	})
	testutil.SetAuthContext(req, org.ID, user.ID)
	testutil.SetPathParam(req, "id", campaign.ID.String())

	err := app.ImportRecipients(req)
	require.NoError(t, err)
	assert.Equal(t, fasthttp.StatusBadRequest, testutil.GetResponseStatusCode(req))
}

// --- GetCampaignRecipients Tests ---

func TestApp_GetCampaignRecipients_Success(t *testing.T) {
	mockQueue := testutil.NewMockQueue()
	app := newTestApp(t, withQueue(mockQueue))
	org := testutil.CreateTestOrganization(t, app.DB)
	user := testutil.CreateTestUser(t, app.DB, org.ID, testutil.WithEmail(testutil.UniqueEmail("get-recipients")), testutil.WithPassword("password"))
	account := testutil.CreateTestWhatsAppAccountWith(t, app.DB, org.ID, testutil.WithAccountName("get-recipients-account"))
	template := testutil.CreateTestTemplate(t, app.DB, org.ID, account.Name)
	campaign := createTestCampaign(t, app, org.ID, template.ID, user.ID, account.Name, models.CampaignStatusDraft)
	createTestRecipient(t, app, campaign.ID, "+1234567890", models.MessageStatusPending)
	createTestRecipient(t, app, campaign.ID, "+0987654321", models.MessageStatusSent)

	req := testutil.NewGETRequest(t)
	testutil.SetAuthContext(req, org.ID, user.ID)
	testutil.SetPathParam(req, "id", campaign.ID.String())

	err := app.GetCampaignRecipients(req)
	require.NoError(t, err)
	assert.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req))

	var resp struct {
		Data struct {
			Recipients []models.BulkMessageRecipient `json:"recipients"`
			Total      int                           `json:"total"`
		} `json:"data"`
	}
	err = json.Unmarshal(testutil.GetResponseBody(req), &resp)
	require.NoError(t, err)
	assert.Equal(t, 2, resp.Data.Total)
}

func TestApp_GetCampaignRecipients_CampaignNotFound(t *testing.T) {
	mockQueue := testutil.NewMockQueue()
	app := newTestApp(t, withQueue(mockQueue))
	org := testutil.CreateTestOrganization(t, app.DB)
	user := testutil.CreateTestUser(t, app.DB, org.ID, testutil.WithEmail(testutil.UniqueEmail("get-recipients-not-found")), testutil.WithPassword("password"))

	req := testutil.NewGETRequest(t)
	testutil.SetAuthContext(req, org.ID, user.ID)
	testutil.SetPathParam(req, "id", uuid.New().String())

	err := app.GetCampaignRecipients(req)
	require.NoError(t, err)
	assert.Equal(t, fasthttp.StatusNotFound, testutil.GetResponseStatusCode(req))
}

// --- RetryFailed Tests ---

func TestApp_RetryFailed_Success(t *testing.T) {
	mockQueue := testutil.NewMockQueue()
	app := newTestApp(t, withQueue(mockQueue))
	org := testutil.CreateTestOrganization(t, app.DB)
	user := testutil.CreateTestUser(t, app.DB, org.ID, testutil.WithEmail(testutil.UniqueEmail("retry-failed")), testutil.WithPassword("password"))
	account := testutil.CreateTestWhatsAppAccountWith(t, app.DB, org.ID, testutil.WithAccountName("retry-account"))
	template := testutil.CreateTestTemplate(t, app.DB, org.ID, account.Name)
	campaign := createTestCampaign(t, app, org.ID, template.ID, user.ID, account.Name, models.CampaignStatusCompleted)
	createTestRecipient(t, app, campaign.ID, "+1234567890", models.MessageStatusSent)
	createTestRecipient(t, app, campaign.ID, "+0987654321", models.MessageStatusFailed)

	req := testutil.NewJSONRequest(t, nil)
	testutil.SetAuthContext(req, org.ID, user.ID)
	testutil.SetPathParam(req, "id", campaign.ID.String())

	err := app.RetryFailed(req)
	require.NoError(t, err)
	assert.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req))

	// Verify only failed recipients were enqueued
	assert.Len(t, mockQueue.Jobs, 1)
	assert.Equal(t, "+0987654321", mockQueue.Jobs[0].PhoneNumber)

	var resp struct {
		Data struct {
			RetryCount int    `json:"retry_count"`
			Status     string `json:"status"`
		} `json:"data"`
	}
	err = json.Unmarshal(testutil.GetResponseBody(req), &resp)
	require.NoError(t, err)
	assert.Equal(t, 1, resp.Data.RetryCount)
	assert.Equal(t, string(models.CampaignStatusProcessing), resp.Data.Status)
}

func TestApp_RetryFailed_NoFailedRecipients(t *testing.T) {
	mockQueue := testutil.NewMockQueue()
	app := newTestApp(t, withQueue(mockQueue))
	org := testutil.CreateTestOrganization(t, app.DB)
	user := testutil.CreateTestUser(t, app.DB, org.ID, testutil.WithEmail(testutil.UniqueEmail("retry-no-failed")), testutil.WithPassword("password"))
	account := testutil.CreateTestWhatsAppAccountWith(t, app.DB, org.ID, testutil.WithAccountName("retry-no-failed-account"))
	template := testutil.CreateTestTemplate(t, app.DB, org.ID, account.Name)
	campaign := createTestCampaign(t, app, org.ID, template.ID, user.ID, account.Name, models.CampaignStatusCompleted)
	createTestRecipient(t, app, campaign.ID, "+1234567890", models.MessageStatusSent)

	req := testutil.NewJSONRequest(t, nil)
	testutil.SetAuthContext(req, org.ID, user.ID)
	testutil.SetPathParam(req, "id", campaign.ID.String())

	err := app.RetryFailed(req)
	require.NoError(t, err)
	assert.Equal(t, fasthttp.StatusBadRequest, testutil.GetResponseStatusCode(req))
}

func TestApp_RetryFailed_InvalidStatus(t *testing.T) {
	mockQueue := testutil.NewMockQueue()
	app := newTestApp(t, withQueue(mockQueue))
	org := testutil.CreateTestOrganization(t, app.DB)
	user := testutil.CreateTestUser(t, app.DB, org.ID, testutil.WithEmail(testutil.UniqueEmail("retry-invalid-status")), testutil.WithPassword("password"))
	account := testutil.CreateTestWhatsAppAccountWith(t, app.DB, org.ID, testutil.WithAccountName("retry-invalid-account"))
	template := testutil.CreateTestTemplate(t, app.DB, org.ID, account.Name)
	campaign := createTestCampaign(t, app, org.ID, template.ID, user.ID, account.Name, models.CampaignStatusDraft)
	createTestRecipient(t, app, campaign.ID, "+1234567890", models.MessageStatusFailed)

	req := testutil.NewJSONRequest(t, nil)
	testutil.SetAuthContext(req, org.ID, user.ID)
	testutil.SetPathParam(req, "id", campaign.ID.String())

	err := app.RetryFailed(req)
	require.NoError(t, err)
	assert.Equal(t, fasthttp.StatusBadRequest, testutil.GetResponseStatusCode(req))
}

// --- Cross-Organization Tests ---

func TestApp_Campaign_CrossOrgIsolation(t *testing.T) {
	mockQueue := testutil.NewMockQueue()
	app := newTestApp(t, withQueue(mockQueue))

	// Create two organizations
	org1 := testutil.CreateTestOrganization(t, app.DB)
	org2 := testutil.CreateTestOrganization(t, app.DB)

	user1 := testutil.CreateTestUser(t, app.DB, org1.ID, testutil.WithEmail(testutil.UniqueEmail("cross-org-1")), testutil.WithPassword("password"))
	user2 := testutil.CreateTestUser(t, app.DB, org2.ID, testutil.WithEmail(testutil.UniqueEmail("cross-org-2")), testutil.WithPassword("password"))

	account1 := testutil.CreateTestWhatsAppAccountWith(t, app.DB, org1.ID, testutil.WithAccountName("cross-org-account-1"))
	template1 := testutil.CreateTestTemplate(t, app.DB, org1.ID, account1.Name)
	campaign1 := createTestCampaign(t, app, org1.ID, template1.ID, user1.ID, account1.Name, models.CampaignStatusDraft)

	// User from org2 tries to access org1's campaign
	req := testutil.NewGETRequest(t)
	testutil.SetAuthContext(req, org2.ID, user2.ID)
	testutil.SetPathParam(req, "id", campaign1.ID.String())

	err := app.GetCampaign(req)
	require.NoError(t, err)
	assert.Equal(t, fasthttp.StatusNotFound, testutil.GetResponseStatusCode(req))
}

type campaignMediaTestObject struct {
	data        []byte
	contentType string
}

type campaignMediaTestStore struct {
	mu         sync.Mutex
	objects    map[string]campaignMediaTestObject
	puts       []string
	deletes    []string
	putErr     error
	putThenErr bool
	deleteErr  error
}

var _ storage.ObjectStore = (*campaignMediaTestStore)(nil)

func newCampaignMediaTestStore() *campaignMediaTestStore {
	return &campaignMediaTestStore{objects: make(map[string]campaignMediaTestObject)}
}

func (s *campaignMediaTestStore) Put(_ context.Context, key string, data []byte, contentType string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.puts = append(s.puts, key)
	if s.putErr != nil && !s.putThenErr {
		return s.putErr
	}
	s.objects[key] = campaignMediaTestObject{data: append([]byte(nil), data...), contentType: contentType}
	if s.putErr != nil {
		return s.putErr
	}
	return nil
}

func (s *campaignMediaTestStore) Get(_ context.Context, key string) ([]byte, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	object, ok := s.objects[key]
	if !ok {
		return nil, "", storage.ErrObjectNotFound
	}
	return append([]byte(nil), object.data...), object.contentType, nil
}

func (s *campaignMediaTestStore) Delete(_ context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deletes = append(s.deletes, key)
	if s.deleteErr != nil {
		return s.deleteErr
	}
	delete(s.objects, key)
	return nil
}

func (s *campaignMediaTestStore) ListPrefix(_ context.Context, prefix string) ([]storage.ObjectInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	objects := make([]storage.ObjectInfo, 0)
	for key, object := range s.objects {
		if strings.HasPrefix(key, prefix) {
			objects = append(objects, storage.ObjectInfo{Key: key, Size: int64(len(object.data))})
		}
	}
	return objects, nil
}

func (s *campaignMediaTestStore) snapshot() (map[string]campaignMediaTestObject, []string, []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	objects := make(map[string]campaignMediaTestObject, len(s.objects))
	for key, object := range s.objects {
		objects[key] = campaignMediaTestObject{
			data:        append([]byte(nil), object.data...),
			contentType: object.contentType,
		}
	}
	return objects, append([]string(nil), s.puts...), append([]string(nil), s.deletes...)
}

func (s *campaignMediaTestStore) seed(key string, data []byte, contentType string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.objects[key] = campaignMediaTestObject{data: append([]byte(nil), data...), contentType: contentType}
}

type campaignMediaTestProvider struct {
	calls   atomic.Int32
	fail    atomic.Bool
	entered chan struct{}
	release <-chan struct{}
	server  *httptest.Server
}

func newCampaignMediaTestProvider(t *testing.T) *campaignMediaTestProvider {
	t.Helper()
	provider := &campaignMediaTestProvider{}
	provider.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/media") {
			http.NotFound(w, r)
			return
		}
		provider.calls.Add(1)
		if provider.entered != nil {
			select {
			case provider.entered <- struct{}{}:
			default:
			}
		}
		if provider.release != nil {
			<-provider.release
		}
		if provider.fail.Load() {
			http.Error(w, `{"error":{"message":"synthetic upload failure"}}`, http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"id": "campaign-media-id"})
	}))
	t.Cleanup(provider.server.Close)
	return provider
}

type campaignMediaTestFixture struct {
	app      *handlers.App
	org      *models.Organization
	user     *models.User
	account  *models.WhatsAppAccount
	template *models.Template
	campaign *models.BulkMessageCampaign
	store    *campaignMediaTestStore
	provider *campaignMediaTestProvider
}

func newCampaignMediaTestFixture(t *testing.T) *campaignMediaTestFixture {
	t.Helper()
	provider := newCampaignMediaTestProvider(t)
	app := newTestApp(t)
	app.Config.Storage.Type = "s3"
	store := newCampaignMediaTestStore()
	app.ObjectStore = store
	app.WhatsApp = whatsapp.NewWithBaseURL(app.Log, provider.server.URL)
	org := testutil.CreateTestOrganization(t, app.DB)
	user := testutil.CreateTestUser(
		t,
		app.DB,
		org.ID,
		testutil.WithEmail(testutil.UniqueEmail("campaign-media")),
		testutil.WithPassword("password"),
	)
	account := testutil.CreateTestWhatsAppAccountWith(
		t,
		app.DB,
		org.ID,
		testutil.WithAccountName("campaign-media-"+uuid.NewString()[:8]),
	)
	template := testutil.CreateTestTemplate(t, app.DB, org.ID, account.Name)
	require.NoError(t, app.DB.Model(template).Update("header_type", "IMAGE").Error)
	template.HeaderType = "IMAGE"
	campaign := createTestCampaign(t, app, org.ID, template.ID, user.ID, account.Name, models.CampaignStatusDraft)
	return &campaignMediaTestFixture{
		app: app, org: org, user: user, account: account, template: template,
		campaign: campaign, store: store, provider: provider,
	}
}

func newCampaignMediaUploadRequest(
	t *testing.T,
	fixture *campaignMediaTestFixture,
	filename, contentType string,
	data []byte,
) *fastglue.Request {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename=%q`, filename))
	header.Set("Content-Type", contentType)
	part, err := writer.CreatePart(header)
	require.NoError(t, err)
	_, err = part.Write(data)
	require.NoError(t, err)
	require.NoError(t, writer.Close())

	req := testutil.NewRequest(t)
	req.RequestCtx.Request.Header.SetMethod(fasthttp.MethodPost)
	req.RequestCtx.Request.Header.SetContentType(writer.FormDataContentType())
	req.RequestCtx.Request.SetBody(body.Bytes())
	testutil.SetAuthContext(req, fixture.org.ID, fixture.user.ID)
	testutil.SetPathParam(req, "id", fixture.campaign.ID.String())
	return req
}

func campaignMediaJPEGTestPath(fixture *campaignMediaTestFixture, data []byte) string {
	digest := sha256.Sum256(data)
	return fmt.Sprintf(
		"organizations/%s/campaigns/%s-%x.jpg",
		fixture.org.ID,
		fixture.campaign.ID,
		digest,
	)
}

func runCampaignHandlerOnPinnedDB(
	app *handlers.App,
	req *fastglue.Request,
	invoke func(*handlers.App, *fastglue.Request) error,
) (<-chan int, <-chan error) {
	pid := make(chan int, 1)
	done := make(chan error, 1)
	go func() {
		done <- app.DB.Connection(func(connection *gorm.DB) error {
			session := connection.Session(&gorm.Session{NewDB: true})
			var backendPID int
			if err := session.Raw("SELECT pg_backend_pid()").Scan(&backendPID).Error; err != nil {
				pid <- 0
				return err
			}
			pid <- backendPID
			pinned := &handlers.App{
				Config:      app.Config,
				DB:          session,
				Redis:       app.Redis,
				Log:         app.Log,
				WhatsApp:    app.WhatsApp,
				Queue:       app.Queue,
				HTTPClient:  app.HTTPClient,
				ObjectStore: app.ObjectStore,
			}
			return invoke(pinned, req)
		})
	}()
	return pid, done
}

func awaitCampaignHandler(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		require.Fail(t, "campaign handler did not finish")
	}
}

type campaignGenerationQueue struct {
	calls        atomic.Int32
	firstEntered chan<- struct{}
	releaseFirst <-chan struct{}
}

var _ queue.Queue = (*campaignGenerationQueue)(nil)

func (q *campaignGenerationQueue) EnqueueRecipient(ctx context.Context, job *queue.RecipientJob) error {
	return q.EnqueueRecipients(ctx, []*queue.RecipientJob{job})
}

func (q *campaignGenerationQueue) EnqueueRecipients(_ context.Context, _ []*queue.RecipientJob) error {
	switch q.calls.Add(1) {
	case 1:
		q.firstEntered <- struct{}{}
		<-q.releaseFirst
		return errors.New("synthetic first-generation queue failure")
	case 2:
		return nil
	default:
		return errors.New("unexpected queue invocation")
	}
}

func (q *campaignGenerationQueue) Close() error { return nil }

func isCampaignMediaProjectionUpdate(tx *gorm.DB) bool {
	if tx == nil || tx.Statement == nil || tx.Statement.Schema == nil ||
		tx.Statement.Schema.Table != "bulk_message_campaigns" {
		return false
	}
	updates, ok := tx.Statement.Dest.(map[string]any)
	if !ok {
		return false
	}
	_, ok = updates["header_media_id"]
	return ok
}

func TestApp_PauseAndCancel_DoNotOverwriteConcurrentCompletion(t *testing.T) {
	tests := []struct {
		name   string
		invoke func(*handlers.App, *fastglue.Request) error
	}{
		{name: "pause", invoke: (*handlers.App).PauseCampaign},
		{name: "cancel", invoke: (*handlers.App).CancelCampaign},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			app := newTestApp(t, withQueue(testutil.NewMockQueue()))
			org := testutil.CreateTestOrganization(t, app.DB)
			user := testutil.CreateTestUser(
				t,
				app.DB,
				org.ID,
				testutil.WithEmail(testutil.UniqueEmail("campaign-completion-race")),
				testutil.WithPassword("password"),
			)
			account := testutil.CreateTestWhatsAppAccountWith(t, app.DB, org.ID, testutil.WithAccountName("completion-race-"+uuid.NewString()[:8]))
			template := testutil.CreateTestTemplate(t, app.DB, org.ID, account.Name)
			campaign := createTestCampaign(t, app, org.ID, template.ID, user.ID, account.Name, models.CampaignStatusProcessing)

			completedAt := time.Now().UTC().Truncate(time.Microsecond)
			completionTx := app.DB.Begin()
			require.NoError(t, completionTx.Error)
			t.Cleanup(func() { _ = completionTx.Rollback().Error })
			require.NoError(t, completionTx.Model(&models.BulkMessageCampaign{}).
				Where("id = ? AND organization_id = ? AND status = ?", campaign.ID, org.ID, models.CampaignStatusProcessing).
				Updates(map[string]any{
					"status":       models.CampaignStatusCompleted,
					"completed_at": completedAt,
				}).Error)

			req := testutil.NewJSONRequest(t, nil)
			testutil.SetAuthContext(req, org.ID, user.ID)
			testutil.SetPathParam(req, "id", campaign.ID.String())
			pid, done := runCampaignHandlerOnPinnedDB(app, req, test.invoke)
			backendPID := <-pid
			require.Positive(t, backendPID)
			testutil.RequirePostgresBackendWaitingForLock(t, app.DB, backendPID)

			require.NoError(t, completionTx.Commit().Error)
			awaitCampaignHandler(t, done)
			require.Equal(t, fasthttp.StatusBadRequest, testutil.GetResponseStatusCode(req))

			var stored models.BulkMessageCampaign
			require.NoError(t, app.DB.Where("id = ? AND organization_id = ?", campaign.ID, org.ID).First(&stored).Error)
			require.Equal(t, models.CampaignStatusCompleted, stored.Status)
			require.NotNil(t, stored.CompletedAt)
			require.WithinDuration(t, completedAt, *stored.CompletedAt, time.Microsecond)
		})
	}
}

func TestApp_StartCampaign_DoesNotOverwriteConcurrentCancel(t *testing.T) {
	mockQueue := testutil.NewMockQueue()
	app := newTestApp(t, withQueue(mockQueue))
	org := testutil.CreateTestOrganization(t, app.DB)
	user := testutil.CreateTestUser(
		t,
		app.DB,
		org.ID,
		testutil.WithEmail(testutil.UniqueEmail("start-cancel-race")),
		testutil.WithPassword("password"),
	)
	account := testutil.CreateTestWhatsAppAccountWith(t, app.DB, org.ID, testutil.WithAccountName("start-cancel-race-"+uuid.NewString()[:8]))
	template := testutil.CreateTestTemplate(t, app.DB, org.ID, account.Name)
	campaign := createTestCampaign(t, app, org.ID, template.ID, user.ID, account.Name, models.CampaignStatusDraft)
	createTestRecipient(t, app, campaign.ID, "+1234567890", models.MessageStatusPending)

	cancelTx := app.DB.Begin()
	require.NoError(t, cancelTx.Error)
	t.Cleanup(func() { _ = cancelTx.Rollback().Error })
	require.NoError(t, cancelTx.Model(&models.BulkMessageCampaign{}).
		Where("id = ? AND organization_id = ? AND status = ?", campaign.ID, org.ID, models.CampaignStatusDraft).
		Update("status", models.CampaignStatusCancelled).Error)

	req := testutil.NewJSONRequest(t, nil)
	testutil.SetAuthContext(req, org.ID, user.ID)
	testutil.SetPathParam(req, "id", campaign.ID.String())
	pid, done := runCampaignHandlerOnPinnedDB(app, req, (*handlers.App).StartCampaign)
	backendPID := <-pid
	require.Positive(t, backendPID)
	testutil.RequirePostgresBackendWaitingForLock(t, app.DB, backendPID)

	require.NoError(t, cancelTx.Commit().Error)
	awaitCampaignHandler(t, done)
	require.Equal(t, fasthttp.StatusBadRequest, testutil.GetResponseStatusCode(req))
	require.Zero(t, mockQueue.JobCount())

	var stored models.BulkMessageCampaign
	require.NoError(t, app.DB.Where("id = ? AND organization_id = ?", campaign.ID, org.ID).First(&stored).Error)
	require.Equal(t, models.CampaignStatusCancelled, stored.Status)
}

func TestApp_StartCampaign_QueueFailureDoesNotRevertConcurrentCancel(t *testing.T) {
	queueEntered := make(chan struct{}, 1)
	releaseQueue := make(chan struct{})
	var releaseQueueOnce sync.Once
	releaseQueueCall := func() { releaseQueueOnce.Do(func() { close(releaseQueue) }) }
	t.Cleanup(releaseQueueCall)
	mockQueue := testutil.NewMockQueue()
	mockQueue.EnqueuesFunc = func(context.Context, []*queue.RecipientJob) error {
		queueEntered <- struct{}{}
		<-releaseQueue
		return errors.New("synthetic queue failure")
	}
	app := newTestApp(t, withQueue(mockQueue))
	org := testutil.CreateTestOrganization(t, app.DB)
	user := testutil.CreateTestUser(
		t,
		app.DB,
		org.ID,
		testutil.WithEmail(testutil.UniqueEmail("start-revert-race")),
		testutil.WithPassword("password"),
	)
	account := testutil.CreateTestWhatsAppAccountWith(t, app.DB, org.ID, testutil.WithAccountName("start-revert-race-"+uuid.NewString()[:8]))
	template := testutil.CreateTestTemplate(t, app.DB, org.ID, account.Name)
	campaign := createTestCampaign(t, app, org.ID, template.ID, user.ID, account.Name, models.CampaignStatusDraft)
	createTestRecipient(t, app, campaign.ID, "+1234567890", models.MessageStatusPending)

	startReq := testutil.NewJSONRequest(t, nil)
	testutil.SetAuthContext(startReq, org.ID, user.ID)
	testutil.SetPathParam(startReq, "id", campaign.ID.String())
	startDone := make(chan error, 1)
	go func() { startDone <- app.StartCampaign(startReq) }()
	select {
	case <-queueEntered:
	case <-time.After(5 * time.Second):
		require.Fail(t, "start did not reach queue")
	}

	cancelReq := testutil.NewJSONRequest(t, nil)
	testutil.SetAuthContext(cancelReq, org.ID, user.ID)
	testutil.SetPathParam(cancelReq, "id", campaign.ID.String())
	require.NoError(t, app.CancelCampaign(cancelReq))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(cancelReq))
	releaseQueueCall()
	awaitCampaignHandler(t, startDone)
	require.Equal(t, fasthttp.StatusInternalServerError, testutil.GetResponseStatusCode(startReq))

	var stored models.BulkMessageCampaign
	require.NoError(t, app.DB.Where("id = ? AND organization_id = ?", campaign.ID, org.ID).First(&stored).Error)
	require.Equal(t, models.CampaignStatusCancelled, stored.Status)
}

func TestApp_StartCampaign_QueueFailureDoesNotRevertNewerProcessingGeneration(t *testing.T) {
	firstQueueEntered := make(chan struct{}, 1)
	releaseFirstQueue := make(chan struct{})
	var releaseQueueOnce sync.Once
	releaseQueue := func() { releaseQueueOnce.Do(func() { close(releaseFirstQueue) }) }
	t.Cleanup(releaseQueue)
	generationQueue := &campaignGenerationQueue{
		firstEntered: firstQueueEntered,
		releaseFirst: releaseFirstQueue,
	}
	app := newTestApp(t, withQueue(generationQueue))
	org := testutil.CreateTestOrganization(t, app.DB)
	user := testutil.CreateTestUser(
		t,
		app.DB,
		org.ID,
		testutil.WithEmail(testutil.UniqueEmail("start-generation-fence")),
		testutil.WithPassword("password"),
	)
	account := testutil.CreateTestWhatsAppAccountWith(t, app.DB, org.ID, testutil.WithAccountName("start-generation-fence-"+uuid.NewString()[:8]))
	template := testutil.CreateTestTemplate(t, app.DB, org.ID, account.Name)
	campaign := createTestCampaign(t, app, org.ID, template.ID, user.ID, account.Name, models.CampaignStatusDraft)
	createTestRecipient(t, app, campaign.ID, "+1234567890", models.MessageStatusPending)

	firstReq := testutil.NewJSONRequest(t, nil)
	testutil.SetAuthContext(firstReq, org.ID, user.ID)
	testutil.SetPathParam(firstReq, "id", campaign.ID.String())
	firstDone := make(chan error, 1)
	go func() { firstDone <- app.StartCampaign(firstReq) }()
	select {
	case <-firstQueueEntered:
	case <-time.After(5 * time.Second):
		require.Fail(t, "first start did not reach queue")
	}

	var firstGeneration models.BulkMessageCampaign
	require.NoError(t, app.DB.Where("id = ? AND organization_id = ?", campaign.ID, org.ID).First(&firstGeneration).Error)
	require.Equal(t, models.CampaignStatusProcessing, firstGeneration.Status)
	require.NotNil(t, firstGeneration.StartedAt)

	pauseReq := testutil.NewJSONRequest(t, nil)
	testutil.SetAuthContext(pauseReq, org.ID, user.ID)
	testutil.SetPathParam(pauseReq, "id", campaign.ID.String())
	require.NoError(t, app.PauseCampaign(pauseReq))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(pauseReq))

	// PostgreSQL stores timestamps at microsecond precision. Ensure the resumed
	// processing generation cannot normalize to the first start's fence value.
	time.Sleep(2 * time.Millisecond)
	secondReq := testutil.NewJSONRequest(t, nil)
	testutil.SetAuthContext(secondReq, org.ID, user.ID)
	testutil.SetPathParam(secondReq, "id", campaign.ID.String())
	require.NoError(t, app.StartCampaign(secondReq))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(secondReq))

	var secondGeneration models.BulkMessageCampaign
	require.NoError(t, app.DB.Where("id = ? AND organization_id = ?", campaign.ID, org.ID).First(&secondGeneration).Error)
	require.Equal(t, models.CampaignStatusProcessing, secondGeneration.Status)
	require.NotNil(t, secondGeneration.StartedAt)
	require.NotEqual(t, firstGeneration.StartedAt.UTC(), secondGeneration.StartedAt.UTC())

	releaseQueue()
	awaitCampaignHandler(t, firstDone)
	require.Equal(t, fasthttp.StatusInternalServerError, testutil.GetResponseStatusCode(firstReq))
	require.EqualValues(t, 2, generationQueue.calls.Load())

	var stored models.BulkMessageCampaign
	require.NoError(t, app.DB.Where("id = ? AND organization_id = ?", campaign.ID, org.ID).First(&stored).Error)
	require.Equal(t, models.CampaignStatusProcessing, stored.Status,
		"the first queue failure must not revert a newer processing generation")
	require.NotNil(t, stored.StartedAt)
	require.WithinDuration(t, secondGeneration.StartedAt.UTC(), stored.StartedAt.UTC(), time.Microsecond)
}

func TestApp_UploadCampaignMedia_StagesBeforeProviderAndReusesIdenticalRetry(t *testing.T) {
	fixture := newCampaignMediaTestFixture(t)
	oldPath := fmt.Sprintf("organizations/%s/campaigns/old-preview.jpg", fixture.org.ID)
	fixture.store.seed(oldPath, []byte("old-preview"), "image/jpeg")
	require.NoError(t, fixture.app.DB.Model(&models.BulkMessageCampaign{}).
		Where("id = ? AND organization_id = ?", fixture.campaign.ID, fixture.org.ID).
		Updates(map[string]any{
			"header_media_id":         "old-media-id",
			"header_media_filename":   "old.jpg",
			"header_media_mime_type":  "image/jpeg",
			"header_media_local_path": oldPath,
		}).Error)

	providerEntered := make(chan struct{}, 1)
	releaseProvider := make(chan struct{})
	var releaseProviderOnce sync.Once
	releaseProviderCall := func() { releaseProviderOnce.Do(func() { close(releaseProvider) }) }
	t.Cleanup(releaseProviderCall)
	fixture.provider.entered = providerEntered
	fixture.provider.release = releaseProvider
	media := []byte("new-campaign-preview")
	firstReq := newCampaignMediaUploadRequest(t, fixture, "campaign.jpg", "image/jpeg", media)
	firstDone := make(chan error, 1)
	go func() { firstDone <- fixture.app.UploadCampaignMedia(firstReq) }()

	select {
	case <-providerEntered:
	case <-time.After(5 * time.Second):
		require.Fail(t, "upload did not reach provider")
	}
	objects, puts, deletes := fixture.store.snapshot()
	require.Len(t, puts, 1)
	require.Contains(t, objects, puts[0], "preview must be durable before the provider call")
	require.Contains(t, objects, oldPath, "replacement must preserve the prior referenced preview")
	require.Empty(t, deletes)

	secondReq := newCampaignMediaUploadRequest(t, fixture, "campaign.jpg", "image/jpeg", media)
	secondPID, secondDone := runCampaignHandlerOnPinnedDB(fixture.app, secondReq, (*handlers.App).UploadCampaignMedia)
	backendPID := <-secondPID
	require.Positive(t, backendPID)
	testutil.RequirePostgresBackendWaitingForLock(t, fixture.app.DB, backendPID)

	releaseProviderCall()
	awaitCampaignHandler(t, firstDone)
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(firstReq))
	awaitCampaignHandler(t, secondDone)
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(secondReq))
	require.EqualValues(t, 1, fixture.provider.calls.Load(), "concurrent identical retry must not call Meta again")

	objects, puts, deletes = fixture.store.snapshot()
	require.Len(t, puts, 1, "concurrent identical retry must not restage the preview")
	require.Empty(t, deletes)
	require.Contains(t, objects, oldPath)

	var stored models.BulkMessageCampaign
	require.NoError(t, fixture.app.DB.
		Where("id = ? AND organization_id = ?", fixture.campaign.ID, fixture.org.ID).
		First(&stored).Error)
	require.Equal(t, "campaign-media-id", stored.HeaderMediaID)
	require.Equal(t, "campaign.jpg", stored.HeaderMediaFilename)
	require.Equal(t, "image/jpeg", stored.HeaderMediaMimeType)
	require.Equal(t, puts[0], stored.HeaderMediaLocalPath)
	require.NotEqual(t, oldPath, stored.HeaderMediaLocalPath)
}

func TestApp_UploadCampaignMedia_IdenticalRetryRestoresMissingDurablePreview(t *testing.T) {
	fixture := newCampaignMediaTestFixture(t)
	media := []byte("restore-missing-durable-preview")
	candidatePath := campaignMediaJPEGTestPath(fixture, media)
	require.NoError(t, fixture.app.DB.Model(&models.BulkMessageCampaign{}).
		Where("id = ? AND organization_id = ?", fixture.campaign.ID, fixture.org.ID).
		Updates(map[string]any{
			"header_media_id":         "already-uploaded-media-id",
			"header_media_filename":   "campaign.jpg",
			"header_media_mime_type":  "image/jpeg",
			"header_media_local_path": candidatePath,
		}).Error)

	req := newCampaignMediaUploadRequest(t, fixture, "campaign.jpg", "image/jpeg", media)
	require.NoError(t, fixture.app.UploadCampaignMedia(req))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req))
	require.Zero(t, fixture.provider.calls.Load(), "a committed identical retry must not call Meta again")

	objects, puts, deletes := fixture.store.snapshot()
	require.Equal(t, []string{candidatePath}, puts)
	require.Empty(t, deletes)
	require.Contains(t, objects, candidatePath)
	require.Equal(t, media, objects[candidatePath].data)
}

func TestApp_UploadCampaignMedia_FailuresRespectCleanupAuthority(t *testing.T) {
	tests := []struct {
		name              string
		configure         func(*testing.T, *campaignMediaTestFixture)
		expectedMetaCalls int32
		expectedDeletes   int
		expectStage       bool
	}{
		{
			name: "store",
			configure: func(_ *testing.T, fixture *campaignMediaTestFixture) {
				fixture.store.putErr = errors.New("synthetic object-store failure")
			},
			expectedMetaCalls: 0,
			expectedDeletes:   0,
			expectStage:       false,
		},
		{
			name: "provider",
			configure: func(_ *testing.T, fixture *campaignMediaTestFixture) {
				fixture.provider.fail.Store(true)
			},
			expectedMetaCalls: 1,
			expectedDeletes:   0,
			expectStage:       true,
		},
		{
			name: "update",
			configure: func(t *testing.T, fixture *campaignMediaTestFixture) {
				var fail atomic.Bool
				fail.Store(true)
				callbackName := "campaign_media_update_failure_" + strings.ReplaceAll(uuid.NewString(), "-", "")
				require.NoError(t, fixture.app.DB.Callback().Update().Before("gorm:update").Register(callbackName, func(tx *gorm.DB) {
					if isCampaignMediaProjectionUpdate(tx) && fail.CompareAndSwap(true, false) {
						_ = tx.AddError(errors.New("synthetic campaign media update failure"))
					}
				}))
				t.Cleanup(func() { _ = fixture.app.DB.Callback().Update().Remove(callbackName) })
			},
			expectedMetaCalls: 1,
			expectedDeletes:   1,
			expectStage:       false,
		},
		{
			name: "commit",
			configure: func(t *testing.T, fixture *campaignMediaTestFixture) {
				suffix := strings.ReplaceAll(uuid.NewString(), "-", "")
				parentTable := "campaign_media_commit_parent_" + suffix
				childTable := "campaign_media_commit_child_" + suffix
				require.NoError(t, fixture.app.DB.Exec(fmt.Sprintf("CREATE TABLE %s (id integer PRIMARY KEY)", parentTable)).Error)
				require.NoError(t, fixture.app.DB.Exec(fmt.Sprintf(
					"CREATE TABLE %s (id integer REFERENCES %s(id) DEFERRABLE INITIALLY DEFERRED)",
					childTable,
					parentTable,
				)).Error)
				t.Cleanup(func() {
					_ = fixture.app.DB.Exec(fmt.Sprintf("DROP TABLE IF EXISTS %s", childTable)).Error
					_ = fixture.app.DB.Exec(fmt.Sprintf("DROP TABLE IF EXISTS %s", parentTable)).Error
				})

				var fail atomic.Bool
				fail.Store(true)
				callbackName := "campaign_media_commit_failure_" + suffix
				require.NoError(t, fixture.app.DB.Callback().Update().After("gorm:update").Register(callbackName, func(tx *gorm.DB) {
					if isCampaignMediaProjectionUpdate(tx) && fail.CompareAndSwap(true, false) {
						_ = tx.AddError(tx.Session(&gorm.Session{NewDB: true}).Exec(fmt.Sprintf("INSERT INTO %s (id) VALUES (1)", childTable)).Error)
					}
				}))
				t.Cleanup(func() { _ = fixture.app.DB.Callback().Update().Remove(callbackName) })
			},
			expectedMetaCalls: 1,
			expectedDeletes:   0,
			expectStage:       true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newCampaignMediaTestFixture(t)
			oldPath := fmt.Sprintf("organizations/%s/campaigns/old-preview.jpg", fixture.org.ID)
			fixture.store.seed(oldPath, []byte("old-preview"), "image/jpeg")
			require.NoError(t, fixture.app.DB.Model(&models.BulkMessageCampaign{}).
				Where("id = ? AND organization_id = ?", fixture.campaign.ID, fixture.org.ID).
				Updates(map[string]any{
					"header_media_id":         "old-media-id",
					"header_media_filename":   "old.jpg",
					"header_media_mime_type":  "image/jpeg",
					"header_media_local_path": oldPath,
				}).Error)
			test.configure(t, fixture)

			req := newCampaignMediaUploadRequest(t, fixture, "replacement.jpg", "image/jpeg", []byte("replacement-preview"))
			require.NoError(t, fixture.app.UploadCampaignMedia(req))
			require.Equal(t, fasthttp.StatusInternalServerError, testutil.GetResponseStatusCode(req))
			require.Equal(t, test.expectedMetaCalls, fixture.provider.calls.Load())

			objects, puts, deletes := fixture.store.snapshot()
			require.Len(t, puts, 1)
			require.Len(t, deletes, test.expectedDeletes)
			if test.expectedDeletes != 0 {
				require.Equal(t, []string{puts[0]}, deletes)
			}
			if test.expectStage {
				require.Contains(t, objects, puts[0])
			} else {
				require.NotContains(t, objects, puts[0])
			}
			require.Contains(t, objects, oldPath)

			var stored models.BulkMessageCampaign
			require.NoError(t, fixture.app.DB.
				Where("id = ? AND organization_id = ?", fixture.campaign.ID, fixture.org.ID).
				First(&stored).Error)
			require.Equal(t, "old-media-id", stored.HeaderMediaID)
			require.Equal(t, "old.jpg", stored.HeaderMediaFilename)
			require.Equal(t, "image/jpeg", stored.HeaderMediaMimeType)
			require.Equal(t, oldPath, stored.HeaderMediaLocalPath)
		})
	}
}

func TestApp_UploadCampaignMedia_AmbiguousPutDoesNotDeleteCandidate(t *testing.T) {
	fixture := newCampaignMediaTestFixture(t)
	media := []byte("ambiguous-store-preview")
	candidatePath := campaignMediaJPEGTestPath(fixture, media)
	fixture.store.putErr = errors.New("synthetic transport EOF after write")
	fixture.store.putThenErr = true

	req := newCampaignMediaUploadRequest(t, fixture, "campaign.jpg", "image/jpeg", media)
	require.NoError(t, fixture.app.UploadCampaignMedia(req))
	require.Equal(t, fasthttp.StatusInternalServerError, testutil.GetResponseStatusCode(req))
	require.Zero(t, fixture.provider.calls.Load())

	objects, puts, deletes := fixture.store.snapshot()
	require.Equal(t, []string{candidatePath}, puts)
	require.Empty(t, deletes, "an ambiguous Put result cannot grant deletion authority")
	require.Contains(t, objects, candidatePath)
}

func TestApp_UploadCampaignMedia_PreexistingUnreferencedObjectIsNeverCleanupOwned(t *testing.T) {
	fixture := newCampaignMediaTestFixture(t)
	media := []byte("preexisting-unreferenced-preview")
	candidatePath := campaignMediaJPEGTestPath(fixture, media)
	fixture.store.seed(candidatePath, media, "image/jpeg")

	var fail atomic.Bool
	fail.Store(true)
	callbackName := "campaign_media_preexisting_update_failure_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	require.NoError(t, fixture.app.DB.Callback().Update().Before("gorm:update").Register(callbackName, func(tx *gorm.DB) {
		if isCampaignMediaProjectionUpdate(tx) && fail.CompareAndSwap(true, false) {
			_ = tx.AddError(errors.New("synthetic campaign media update failure"))
		}
	}))
	t.Cleanup(func() { _ = fixture.app.DB.Callback().Update().Remove(callbackName) })

	req := newCampaignMediaUploadRequest(t, fixture, "campaign.jpg", "image/jpeg", media)
	require.NoError(t, fixture.app.UploadCampaignMedia(req))
	require.Equal(t, fasthttp.StatusInternalServerError, testutil.GetResponseStatusCode(req))
	require.EqualValues(t, 1, fixture.provider.calls.Load())

	objects, puts, deletes := fixture.store.snapshot()
	require.Empty(t, puts, "the invocation did not create the candidate object")
	require.Empty(t, deletes, "a pre-existing object is never owned by this invocation")
	require.Contains(t, objects, candidatePath)
}

func TestApp_UploadCampaignMedia_ReferencedMissingDigestRestoreIsNeverCleanupOwned(t *testing.T) {
	fixture := newCampaignMediaTestFixture(t)
	media := []byte("historical-campaign-preview")
	candidatePath := campaignMediaJPEGTestPath(fixture, media)
	oldPath := fmt.Sprintf("organizations/%s/campaigns/current-preview.jpg", fixture.org.ID)
	fixture.store.seed(oldPath, []byte("current-preview"), "image/jpeg")
	require.NoError(t, fixture.app.DB.Model(&models.BulkMessageCampaign{}).
		Where("id = ? AND organization_id = ?", fixture.campaign.ID, fixture.org.ID).
		Updates(map[string]any{
			"header_media_id":         "current-media-id",
			"header_media_filename":   "current.jpg",
			"header_media_mime_type":  "image/jpeg",
			"header_media_local_path": oldPath,
		}).Error)
	contact := testutil.CreateTestContactWith(t, fixture.app.DB, fixture.org.ID, testutil.WithContactAccount(fixture.account.Name))
	require.NoError(t, fixture.app.DB.Create(&models.Message{
		BaseModel:       models.BaseModel{ID: uuid.New()},
		OrganizationID:  fixture.org.ID,
		WhatsAppAccount: fixture.account.Name,
		ContactID:       contact.ID,
		Direction:       models.DirectionOutgoing,
		MessageType:     models.MessageTypeImage,
		MediaURL:        candidatePath,
		MediaMimeType:   "image/jpeg",
		Status:          models.MessageStatusSent,
	}).Error)
	fixture.provider.fail.Store(true)

	req := newCampaignMediaUploadRequest(t, fixture, "historical.jpg", "image/jpeg", media)
	require.NoError(t, fixture.app.UploadCampaignMedia(req))
	require.Equal(t, fasthttp.StatusInternalServerError, testutil.GetResponseStatusCode(req))
	require.EqualValues(t, 1, fixture.provider.calls.Load())

	objects, puts, deletes := fixture.store.snapshot()
	require.Equal(t, []string{candidatePath}, puts, "the missing referenced object should be restored")
	require.Empty(t, deletes, "a historical message reference forbids candidate cleanup")
	require.Contains(t, objects, candidatePath)
	require.Contains(t, objects, oldPath)
}

func TestApp_UploadCampaignMedia_StatusTransitionFirstPreventsSideEffects(t *testing.T) {
	tests := []struct {
		name   string
		status models.CampaignStatus
	}{
		{name: "start", status: models.CampaignStatusProcessing},
		{name: "cancel", status: models.CampaignStatusCancelled},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newCampaignMediaTestFixture(t)
			transitionTx := fixture.app.DB.Begin()
			require.NoError(t, transitionTx.Error)
			t.Cleanup(func() { _ = transitionTx.Rollback().Error })
			require.NoError(t, transitionTx.Model(&models.BulkMessageCampaign{}).
				Where("id = ? AND organization_id = ? AND status = ?", fixture.campaign.ID, fixture.org.ID, models.CampaignStatusDraft).
				Update("status", test.status).Error)

			req := newCampaignMediaUploadRequest(t, fixture, "campaign.jpg", "image/jpeg", []byte("status-first"))
			pid, done := runCampaignHandlerOnPinnedDB(fixture.app, req, (*handlers.App).UploadCampaignMedia)
			backendPID := <-pid
			require.Positive(t, backendPID)
			testutil.RequirePostgresBackendWaitingForLock(t, fixture.app.DB, backendPID)
			_, puts, _ := fixture.store.snapshot()
			require.Empty(t, puts)
			require.Zero(t, fixture.provider.calls.Load())

			require.NoError(t, transitionTx.Commit().Error)
			awaitCampaignHandler(t, done)
			require.Equal(t, fasthttp.StatusBadRequest, testutil.GetResponseStatusCode(req))
			_, puts, deletes := fixture.store.snapshot()
			require.Empty(t, puts)
			require.Empty(t, deletes)
			require.Zero(t, fixture.provider.calls.Load())
		})
	}
}

func TestApp_UploadCampaignMedia_FirstSerializesStartAndCancel(t *testing.T) {
	tests := []struct {
		name           string
		invoke         func(*handlers.App, *fastglue.Request) error
		expectedStatus models.CampaignStatus
	}{
		{name: "start", invoke: (*handlers.App).StartCampaign, expectedStatus: models.CampaignStatusProcessing},
		{name: "cancel", invoke: (*handlers.App).CancelCampaign, expectedStatus: models.CampaignStatusCancelled},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newCampaignMediaTestFixture(t)
			mockQueue := testutil.NewMockQueue()
			fixture.app.Queue = mockQueue
			if test.name == "start" {
				createTestRecipient(t, fixture.app, fixture.campaign.ID, "+1234567890", models.MessageStatusPending)
			}

			providerEntered := make(chan struct{}, 1)
			releaseProvider := make(chan struct{})
			var releaseProviderOnce sync.Once
			releaseProviderCall := func() { releaseProviderOnce.Do(func() { close(releaseProvider) }) }
			t.Cleanup(releaseProviderCall)
			fixture.provider.entered = providerEntered
			fixture.provider.release = releaseProvider
			uploadReq := newCampaignMediaUploadRequest(t, fixture, "campaign.jpg", "image/jpeg", []byte("upload-first"))
			uploadDone := make(chan error, 1)
			go func() { uploadDone <- fixture.app.UploadCampaignMedia(uploadReq) }()
			select {
			case <-providerEntered:
			case <-time.After(5 * time.Second):
				require.Fail(t, "upload did not reach provider")
			}

			transitionReq := testutil.NewJSONRequest(t, nil)
			testutil.SetAuthContext(transitionReq, fixture.org.ID, fixture.user.ID)
			testutil.SetPathParam(transitionReq, "id", fixture.campaign.ID.String())
			pid, transitionDone := runCampaignHandlerOnPinnedDB(fixture.app, transitionReq, test.invoke)
			backendPID := <-pid
			require.Positive(t, backendPID)
			testutil.RequirePostgresBackendWaitingForLock(t, fixture.app.DB, backendPID)

			releaseProviderCall()
			awaitCampaignHandler(t, uploadDone)
			require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(uploadReq))
			awaitCampaignHandler(t, transitionDone)
			require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(transitionReq))
			require.EqualValues(t, 1, fixture.provider.calls.Load())
			if test.name == "start" {
				require.Equal(t, 1, mockQueue.JobCount())
			}

			var stored models.BulkMessageCampaign
			require.NoError(t, fixture.app.DB.
				Where("id = ? AND organization_id = ?", fixture.campaign.ID, fixture.org.ID).
				First(&stored).Error)
			require.Equal(t, test.expectedStatus, stored.Status)
			require.Equal(t, "campaign-media-id", stored.HeaderMediaID)
			require.NotEmpty(t, stored.HeaderMediaLocalPath)
		})
	}
}

func TestApp_UploadCampaignMedia_RevalidatesLockedTemplateAndAccount(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*testing.T, *gorm.DB, *campaignMediaTestFixture)
	}{
		{
			name: "template becomes text",
			mutate: func(t *testing.T, tx *gorm.DB, fixture *campaignMediaTestFixture) {
				require.NoError(t, tx.Model(&models.Template{}).
					Where("id = ? AND organization_id = ?", fixture.template.ID, fixture.org.ID).
					Update("header_type", "TEXT").Error)
			},
		},
		{
			name: "template becomes video for image upload",
			mutate: func(t *testing.T, tx *gorm.DB, fixture *campaignMediaTestFixture) {
				require.NoError(t, tx.Model(&models.Template{}).
					Where("id = ? AND organization_id = ?", fixture.template.ID, fixture.org.ID).
					Update("header_type", "VIDEO").Error)
			},
		},
		{
			name: "template becomes unsupported audio header",
			mutate: func(t *testing.T, tx *gorm.DB, fixture *campaignMediaTestFixture) {
				require.NoError(t, tx.Model(&models.Template{}).
					Where("id = ? AND organization_id = ?", fixture.template.ID, fixture.org.ID).
					Update("header_type", "AUDIO").Error)
			},
		},
		{
			name: "template becomes unapproved",
			mutate: func(t *testing.T, tx *gorm.DB, fixture *campaignMediaTestFixture) {
				require.NoError(t, tx.Model(&models.Template{}).
					Where("id = ? AND organization_id = ?", fixture.template.ID, fixture.org.ID).
					Update("status", string(models.TemplateStatusRejected)).Error)
			},
		},
		{
			name: "template account ownership drifts",
			mutate: func(t *testing.T, tx *gorm.DB, fixture *campaignMediaTestFixture) {
				require.NoError(t, tx.Model(&models.Template{}).
					Where("id = ? AND organization_id = ?", fixture.template.ID, fixture.org.ID).
					Update("whats_app_account", "other-account-"+uuid.NewString()[:8]).Error)
			},
		},
		{
			name: "account becomes inactive",
			mutate: func(t *testing.T, tx *gorm.DB, fixture *campaignMediaTestFixture) {
				require.NoError(t, tx.Model(&models.WhatsAppAccount{}).
					Where("id = ? AND organization_id = ?", fixture.account.ID, fixture.org.ID).
					Update("status", "inactive").Error)
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newCampaignMediaTestFixture(t)
			driftTx := fixture.app.DB.Begin()
			require.NoError(t, driftTx.Error)
			t.Cleanup(func() { _ = driftTx.Rollback().Error })
			test.mutate(t, driftTx, fixture)

			req := newCampaignMediaUploadRequest(t, fixture, "campaign.jpg", "image/jpeg", []byte("drift"))
			pid, done := runCampaignHandlerOnPinnedDB(fixture.app, req, (*handlers.App).UploadCampaignMedia)
			backendPID := <-pid
			require.Positive(t, backendPID)
			testutil.RequirePostgresBackendWaitingForLock(t, fixture.app.DB, backendPID)
			_, puts, _ := fixture.store.snapshot()
			require.Empty(t, puts)
			require.Zero(t, fixture.provider.calls.Load())

			require.NoError(t, driftTx.Commit().Error)
			awaitCampaignHandler(t, done)
			require.Equal(t, fasthttp.StatusBadRequest, testutil.GetResponseStatusCode(req))
			_, puts, deletes := fixture.store.snapshot()
			require.Empty(t, puts)
			require.Empty(t, deletes)
			require.Zero(t, fixture.provider.calls.Load())
		})
	}
}
