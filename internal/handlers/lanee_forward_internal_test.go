package handlers

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/config"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/internal/storage"
	"github.com/shridarpatil/whatomate/internal/whatsappaccount"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The planned key is stored as media_url before the copy runs, so it must be
// exactly the key saveTenantMedia later returns for the same plan.
func TestLaneEPlanForwardMediaMatchesStorageLayout(t *testing.T) {
	orgID := uuid.New()
	source := &models.Message{MediaURL: "images/source.jpg", MediaMimeType: "image/jpeg", MediaFilename: "scan.jpg"}

	local := &App{Config: &config.Config{Storage: config.StorageConfig{LocalPath: t.TempDir()}}, Log: testutil.NopLogger()}
	plan := local.planForwardMedia(orgID, source)
	assert.Equal(t, "image/jpeg", plan.MimeType)
	assert.True(t, strings.HasPrefix(plan.DestinationKey, "images/") && strings.HasSuffix(plan.DestinationKey, ".jpg"), plan.DestinationKey)
	key, err := local.saveTenantMedia(context.Background(), orgID, plan.ObjectSubdir, plan.LocalSubdir, plan.ObjectName, []byte("x"), plan.MimeType)
	require.NoError(t, err)
	assert.Equal(t, plan.DestinationKey, key)
	_, err = os.Stat(filepath.Join(local.Config.Storage.LocalPath, filepath.FromSlash(key)))
	require.NoError(t, err)

	s3 := &App{Config: &config.Config{Storage: config.StorageConfig{Type: "s3"}}}
	objectPlan := s3.planForwardMedia(orgID, &models.Message{MediaURL: "organizations/x/messages/documents/a", MediaFilename: "Consent.PDF"})
	assert.Equal(t, tenantObjectKey(orgID, "messages", "documents", objectPlan.ObjectName), objectPlan.DestinationKey)
	assert.True(t, strings.HasSuffix(objectPlan.ObjectName, ".pdf"), objectPlan.ObjectName)
	assert.NotContains(t, objectPlan.DestinationKey, "organizations/x/", "the copy never reuses the source key")
}

func TestLaneEForwardedMediaIsAsyncOnly(t *testing.T) {
	app := &App{Log: testutil.NopLogger()}
	account := &models.WhatsAppAccount{OrganizationID: uuid.New(), Status: whatsappaccount.StatusActive}
	_, err := app.SendOutgoingMessage(context.Background(), OutgoingMessageRequest{
		Account:      account,
		Contact:      &models.Contact{BaseModel: models.BaseModel{ID: uuid.New()}},
		Type:         models.MessageTypeImage,
		forwardMedia: &forwardMediaCopy{OrganizationID: account.OrganizationID},
	}, ChatbotSendOptions())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "asynchronous delivery")
}

func laneEInternalForwardFixture(t *testing.T, lastInbound *time.Time) (*App, *models.WhatsAppAccount, *models.Contact) {
	t.Helper()
	db := testutil.SetupTestDB(t)
	org := testutil.CreateTestOrganization(t, db)
	account := testutil.CreateTestWhatsAppAccount(t, db, org.ID)
	account.Status = whatsappaccount.StatusActive
	contact := testutil.CreateTestContactWith(t, db, org.ID, testutil.WithContactAccount(account.Name))
	if lastInbound != nil {
		require.NoError(t, db.Model(&models.Contact{}).Where("id = ?", contact.ID).Update("last_inbound_at", *lastInbound).Error)
	}
	return &App{DB: db, Log: testutil.NopLogger(), Config: &config.Config{}}, account, contact
}

func laneEForwardRequest(account *models.WhatsAppAccount, contact *models.Contact) OutgoingMessageRequest {
	return OutgoingMessageRequest{
		Account:       account,
		Contact:       contact,
		Type:          models.MessageTypeText,
		Content:       "forwarded text",
		forwardedFrom: &messageForwardSource{MessageID: uuid.New(), ContactID: uuid.New()},
	}
}

func laneECountMessages(t *testing.T, app *App, contact *models.Contact) int64 {
	t.Helper()
	var count int64
	require.NoError(t, app.DB.Model(&models.Message{}).Where("contact_id = ?", contact.ID).Count(&count).Error)
	return count
}

// The request-level checks ran on the path contact; the send path repeats
// them on the canonical contact under its row lock, before any insert.
func TestLaneEForwardRechecksWindowUnderCanonicalLock(t *testing.T) {
	expired := time.Now().Add(-25 * time.Hour)
	app, account, contact := laneEInternalForwardFixture(t, &expired)
	opts := DefaultSendOptions()
	opts.Async = true
	_, err := app.SendOutgoingMessage(context.Background(), laneEForwardRequest(account, contact), opts)
	var refused *forwardTargetRefusedError
	require.True(t, errors.As(err, &refused), "got %v", err)
	assert.Equal(t, forwardErrorWindowClosed, refused.refusal.ErrorType)
	assert.Zero(t, laneECountMessages(t, app, contact))
}

// PDPA: the forward audit is written in the pending-message transaction, so a
// forward whose audit cannot be written leaves no message behind.
func TestLaneEForwardAuditFailsClosed(t *testing.T) {
	recent := time.Now().Add(-time.Hour)
	app, account, contact := laneEInternalForwardFixture(t, &recent)
	opts := DefaultSendOptions() // no SentByUserID: the audit cannot name an actor
	_, err := app.SendOutgoingMessage(context.Background(), laneEForwardRequest(account, contact), opts)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "forward audit")
	assert.Zero(t, laneECountMessages(t, app, contact))
}

func TestLaneEForwardRateLimitPerUser(t *testing.T) {
	redisClient := testutil.SetupTestRedis(t)
	if redisClient == nil {
		t.Skip("TEST_REDIS_URL not set")
	}
	app := &App{Redis: redisClient, Log: testutil.NopLogger()}
	// The counter is per clock minute; do not straddle a boundary.
	if second := time.Now().Second(); second >= 57 {
		time.Sleep(time.Duration(61-second) * time.Second)
	}
	orgID, userID, otherUser := uuid.New(), uuid.New(), uuid.New()
	for i := 0; i < forwardRateLimitPerMinute; i++ {
		require.True(t, app.allowForward(orgID, userID), "forward %d", i+1)
	}
	assert.False(t, app.allowForward(orgID, userID))
	assert.True(t, app.allowForward(orgID, otherUser), "the limit is per user")
	assert.True(t, (&App{Log: testutil.NopLogger()}).allowForward(orgID, userID), "no Redis: allowed")
}

// laneESizedStore reports object sizes from its listing and counts downloads.
type laneESizedStore struct {
	sizes map[string]int64
	gets  int
}

func (s *laneESizedStore) Put(context.Context, string, []byte, string) error { return nil }
func (s *laneESizedStore) Delete(context.Context, string) error              { return nil }
func (s *laneESizedStore) Get(_ context.Context, key string) ([]byte, string, error) {
	s.gets++
	return make([]byte, s.sizes[key]), "", nil
}
func (s *laneESizedStore) ListPrefix(_ context.Context, prefix string) ([]storage.ObjectInfo, error) {
	var objects []storage.ObjectInfo
	for key, size := range s.sizes {
		if strings.HasPrefix(key, prefix) {
			objects = append(objects, storage.ObjectInfo{Key: key, Size: size})
		}
	}
	return objects, nil
}

func TestLaneEForwardChecksObjectSizeBeforeDownloading(t *testing.T) {
	orgID := uuid.New()
	videoKey := tenantObjectKey(orgID, "messages", "videos", "clip.mp4")
	docKey := tenantObjectKey(orgID, "messages", "documents", "report.pdf")
	store := &laneESizedStore{sizes: map[string]int64{
		videoKey:         16<<20 + 1,
		videoKey + ".v2": 1, // same prefix, different object
		docKey:           99 << 20,
	}}
	app := &App{Log: testutil.NopLogger(), ObjectStore: store, Config: &config.Config{Storage: config.StorageConfig{Type: "s3"}}}
	account := &models.WhatsAppAccount{BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: orgID}

	video := OutgoingMessageRequest{Account: account, Type: models.MessageTypeVideo,
		forwardMedia: &forwardMediaCopy{OrganizationID: orgID, SourceKey: videoKey, MimeType: "video/mp4"}}
	err := app.prepareForwardedMedia(&video)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "larger than WhatsApp allows (16 MB)")
	assert.Zero(t, store.gets, "an oversized object is never downloaded")

	size, known := app.forwardSourceSize(context.Background(), &forwardMediaCopy{OrganizationID: orgID, SourceKey: docKey})
	assert.True(t, known)
	assert.Equal(t, int64(99<<20), size)
	assert.LessOrEqual(t, size, forwardMediaSizeLimit(models.MessageTypeDocument))

	_, known = app.forwardSourceSize(context.Background(), &forwardMediaCopy{OrganizationID: uuid.New(), SourceKey: docKey})
	assert.False(t, known, "another tenant's key is never listed")
	assert.Equal(t, int64(5<<20), forwardMediaSizeLimit(models.MessageTypeImage))
	assert.Equal(t, int64(16<<20), forwardMediaSizeLimit(models.MessageTypeAudio))
}
