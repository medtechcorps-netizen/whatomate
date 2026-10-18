package handlers

import (
	"context"
	"errors"
	"fmt"
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

// Q5: a failure proves "not accepted" by its phase, never by a timeout alone.
func TestLaneEForwardSendFailureClassification(t *testing.T) {
	// The exact shape H1's withBoundedRootConnection returns when no pooled
	// connection was ever acquired (nothing ran).
	wedge := fmt.Errorf("wedge guard: no pooled database connection within 15s while this goroutine may hold another: %w", context.DeadlineExceeded)
	cases := []struct {
		name string
		err  error
		inTx bool
		want bool
	}{
		{"request transaction: any error is rolled back with the response", errors.New("insert failed"), true, true},
		{"request transaction: a timeout too", fmt.Errorf("persist: %w", context.DeadlineExceeded), true, true},
		{"own transaction: a plain error may follow a successful COMMIT", errors.New("commit: connection reset"), false, false},
		{"own transaction: a bare timeout proves nothing", fmt.Errorf("persist: %w", context.DeadlineExceeded), false, false},
		{"own transaction: a wedge-guard rejection never acquired a connection", fmt.Errorf("persist pending message: %w", wedge), false, true},
		{"own transaction: wedge text without a deadline is not trusted", errors.New("wedge guard: no pooled database connection"), false, false},
		{"own transaction: a later wedge guard (locks inside a tx) is not definite", fmt.Errorf("wedge guard: chatbot session fence and locks not acquired: %w", context.DeadlineExceeded), false, false},
		{"no error", nil, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, forwardSendFailureIsDefinite(tc.err, tc.inTx))
		})
	}
	refusal := forwardNotAcceptedRefusal()
	assert.Equal(t, 503, refusal.Status)
	assert.Equal(t, forwardErrorTemporarilyUnavailable, refusal.ErrorType)
}

// Q6: what a reader sees for a forward stalled by a process exit.
func TestLaneEForwardDeliveryStateAfterProcessExit(t *testing.T) {
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	old := now.Add(-10 * time.Minute)
	fresh := now.Add(-30 * time.Second)
	forwarded := func(status models.MessageStatus, created, updated time.Time, extra models.JSONB) *models.Message {
		metadata := models.JSONB{forwardedFromMetadataKey: map[string]any{"message_id": "m", "contact_id": "c"}}
		for key, value := range extra {
			metadata[key] = value
		}
		return &models.Message{
			BaseModel: models.BaseModel{ID: uuid.New(), CreatedAt: created, UpdatedAt: updated},
			Status:    status,
			Metadata:  metadata,
		}
	}
	queued := models.JSONB{forwardDispatchMetadataKey: forwardDispatchQueued}
	provider := func(at time.Time) models.JSONB {
		return models.JSONB{forwardDispatchMetadataKey: forwardDispatchProvider, forwardDispatchAtMetadataKey: at.Format(time.RFC3339Nano)}
	}
	cases := []struct {
		name string
		msg  *models.Message
		want string
	}{
		{"(a) exit before the provider call: not sent", forwarded(models.MessageStatusPending, old, old, queued), forwardDeliveryInterrupted},
		{"(b) exit after the provider phase began: may have been sent", forwarded(models.MessageStatusPending, old, old, provider(old)), forwardDeliveryUncertain},
		{"still within the bounded phases: no state yet", forwarded(models.MessageStatusPending, fresh, fresh, queued), ""},
		{"provider phase just recorded on an old row: not yet uncertain", forwarded(models.MessageStatusPending, old, old, provider(fresh)), ""},
		{"provider phase without a timestamp falls back to updated_at", forwarded(models.MessageStatusPending, old, old, models.JSONB{forwardDispatchMetadataKey: forwardDispatchProvider}), forwardDeliveryUncertain},
		{"settled sent", forwarded(models.MessageStatusSent, old, old, provider(old)), ""},
		{"settled failed", forwarded(models.MessageStatusFailed, old, old, queued), ""},
		{"a forward from before markers existed", forwarded(models.MessageStatusPending, old, old, nil), ""},
		{"not a forward", &models.Message{BaseModel: models.BaseModel{CreatedAt: old, UpdatedAt: old}, Status: models.MessageStatusPending, Metadata: queued}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, forwardDeliveryState(tc.msg, now))
		})
	}
}

// Every forward row is created with the "queued" marker in its own insert.
func TestLaneEForwardRowStartsQueued(t *testing.T) {
	app := &App{Log: testutil.NopLogger()}
	msg := app.createOutgoingMessage(OutgoingMessageRequest{
		Account:       &models.WhatsAppAccount{OrganizationID: uuid.New(), Name: "acct"},
		Contact:       &models.Contact{BaseModel: models.BaseModel{ID: uuid.New()}},
		Type:          models.MessageTypeText,
		Content:       "hi",
		forwardedFrom: &messageForwardSource{MessageID: uuid.New(), ContactID: uuid.New()},
	}, DefaultSendOptions())
	assert.Equal(t, forwardDispatchQueued, msg.Metadata[forwardDispatchMetadataKey])
	assert.True(t, messageIsForwarded(msg.Metadata))
}
