package handlers

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/config"
	"github.com/shridarpatil/whatomate/internal/database"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/internal/storage"
	"github.com/shridarpatil/whatomate/internal/whatsappaccount"
	"github.com/shridarpatil/whatomate/pkg/whatsapp"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
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
	var auditErr *forwardAuditError
	require.True(t, errors.As(err, &auditErr), "got %v", err)
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

// Q5: a failure is classified by the typed phase record of this forward's own
// SendOutgoingMessage call (asynchronous), never by error text.
func TestLaneEForwardSendFailureClassification(t *testing.T) {
	timeout := fmt.Errorf("persist: %w", context.DeadlineExceeded)
	wedgeText := fmt.Errorf("wedge guard: no pooled database connection within 15s: %w", context.DeadlineExceeded)
	requested := forwardSendOutcome{writeRequested: true}
	rolledBack := forwardSendOutcome{writeRequested: true, writeStarted: true}
	atCommit := forwardSendOutcome{writeRequested: true, writeStarted: true, reachedCommit: true}
	inRequest := forwardSendOutcome{writeRequested: true, requestTransaction: true, writeStarted: true, reachedCommit: true}
	sendFailed := "" // the code an ordinary send returns: 500 "Failed to send message", no error type
	cases := []struct {
		name    string
		err     error
		async   bool
		outcome forwardSendOutcome
		status  int
		want    string
	}{
		{"inactive account: would repeat", fmt.Errorf("send: %w", whatsappaccount.ErrOutboundInactive), true, requested, 400, string(forwardErrorAccountUnavailable)},
		{"audit write failure: would repeat", fmt.Errorf("persist: %w", &forwardAuditError{err: errors.New("insert audit_logs")}), true, rolledBack, 500, sendFailed},
		{"audit write failure inside a request transaction: would repeat", &forwardAuditError{}, true, inRequest, 500, sendFailed},
		{"audit write hit a deadline: transient, rolled back", fmt.Errorf("record forward audit: %w", context.DeadlineExceeded), true, rolledBack, 503, string(forwardErrorTemporarilyUnavailable)},
		{"audit write deadlocked after retries (40P01): transient", fmt.Errorf("record forward audit: %w", laneESQLStateError("40P01")), true, rolledBack, 503, string(forwardErrorTemporarilyUnavailable)},
		{"rejected before any write: would repeat", errors.New("WhatsApp account is required"), true, forwardSendOutcome{}, 500, sendFailed},
		{"request transaction: rolled back with the error response", errors.New("savepoint failed"), true, forwardSendOutcome{writeRequested: true, requestTransaction: true, writeStarted: true}, 503, string(forwardErrorTemporarilyUnavailable)},
		{"request transaction: even after its write completed", timeout, true, inRequest, 503, string(forwardErrorTemporarilyUnavailable)},
		{"own transaction: no write began (no connection, BEGIN or tenant binding)", timeout, true, requested, 503, string(forwardErrorTemporarilyUnavailable)},
		{"own transaction: a statement in the write failed, so COMMIT was never sent", errors.New("insert failed"), true, rolledBack, 503, string(forwardErrorTemporarilyUnavailable)},
		// Discriminating controls for a statement rejected before COMMIT.
		{"before COMMIT, transient (deadlock left after retries): try again", fmt.Errorf("failed to create message: %w", laneESQLStateError("40P01")), true, rolledBack, 503, string(forwardErrorTemporarilyUnavailable)},
		{"before COMMIT, INSERT rejected by a constraint: definite, would repeat", fmt.Errorf("failed to create message: %w", laneESQLStateError("23514")), true, rolledBack, 500, sendFailed},
		{"before COMMIT, row-level-security policy rejection: definite, would repeat", fmt.Errorf("failed to create message: %w", laneESQLStateError("42501")), true, rolledBack, 500, sendFailed},
		{"before COMMIT, guard trigger raised: definite, would repeat", laneESQLStateError("P0001"), true, rolledBack, 500, sendFailed},
		{"request transaction, constraint rejection: definite, would repeat", laneESQLStateError("23505"), true, forwardSendOutcome{writeRequested: true, requestTransaction: true, writeStarted: true}, 500, sendFailed},
		{"at COMMIT, even a permanent SQLSTATE stays ambiguous", fmt.Errorf("commit: %w", laneESQLStateError("23514")), true, atCommit, 500, string(forwardErrorOutcomeUnknown)},
		{"own transaction: COMMIT itself returned an error", errors.New("commit: deferred constraint failed"), true, atCommit, 500, string(forwardErrorOutcomeUnknown)},
		{"own transaction: connection dropped after COMMIT was sent", errors.New("commit: connection reset"), true, atCommit, 500, string(forwardErrorOutcomeUnknown)},
		{"own transaction: context cancelled during COMMIT", fmt.Errorf("commit: %w", context.Canceled), true, atCommit, 500, string(forwardErrorOutcomeUnknown)},
		{"own transaction: error text is never trusted", wedgeText, true, atCommit, 500, string(forwardErrorOutcomeUnknown)},
		{"not the asynchronous forward call", errors.New("send failed"), false, requested, 500, string(forwardErrorOutcomeUnknown)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			refusal := forwardSendFailureRefusal(tc.err, tc.async, tc.outcome)
			require.NotNil(t, refusal)
			assert.Equal(t, tc.status, refusal.Status)
			assert.Equal(t, tc.want, string(refusal.ErrorType))
		})
	}
	assert.Nil(t, forwardSendFailureRefusal(nil, true, atCommit))
	assert.Equal(t, "Failed to send message", forwardSendFailedRefusal().Message)
	assert.Equal(t, "Failed to resolve WhatsApp account", forwardAccountUnavailableRefusal().Message)
	assert.Equal(t, 503, forwardNotAcceptedRefusal().Status)
}

// laneESQLStateError is an error carrying a PostgreSQL SQLSTATE, as pgx's are.
type laneESQLStateError string

func (e laneESQLStateError) Error() string    { return "synthetic SQLSTATE " + string(e) }
func (e laneESQLStateError) SQLState() string { return string(e) }

// E2R3-4 and Q5: only database rejections that would repeat are
// non-retryable; transient ones are classified by phase (a rolled-back write
// is a 503).
func TestLaneEForwardFailureRepeatsOnlyForPermanentClasses(t *testing.T) {
	cases := map[string]bool{
		"22001": true, "23505": true, "42501": true, "P0001": true,
		"40P01": false, "40001": false, "55P03": false, "57014": false, "08006": false, "25P02": false,
	}
	for code, repeats := range cases {
		assert.Equal(t, repeats, forwardFailureRepeats(fmt.Errorf("insert audit_logs: %w", laneESQLStateError(code))), code)
	}
	assert.False(t, forwardFailureRepeats(context.DeadlineExceeded))
	assert.False(t, forwardFailureRepeats(context.Canceled))
	assert.False(t, forwardFailureRepeats(errors.New("connection reset by peer")))
}

// The phase record is filled by SendOutgoingMessage itself: a rejection before
// any write is marked as such, so it is never answered with "try again".
func TestLaneEForwardUpFrontRejectionIsNotRetryable(t *testing.T) {
	app := &App{Log: testutil.NopLogger()}
	contact := &models.Contact{BaseModel: models.BaseModel{ID: uuid.New()}}
	account := &models.WhatsAppAccount{OrganizationID: uuid.New(), Status: whatsappaccount.StatusActive}

	outcome := &forwardSendOutcome{}
	opts := ChatbotSendOptions() // synchronous: the media guard refuses before any write
	_, err := app.SendOutgoingMessage(context.Background(), OutgoingMessageRequest{
		Account:        account,
		Contact:        contact,
		Type:           models.MessageTypeImage,
		forwardMedia:   &forwardMediaCopy{OrganizationID: account.OrganizationID},
		forwardOutcome: outcome,
	}, opts)
	require.Error(t, err)
	assert.False(t, outcome.writeRequested)
	upFront := forwardSendFailureRefusal(err, opts.Async, *outcome)
	assert.Equal(t, 500, upFront.Status)
	assert.Empty(t, upFront.ErrorType, "the ordinary send failure, never 503 try-again")

	outcome = &forwardSendOutcome{}
	inactive := &models.WhatsAppAccount{OrganizationID: uuid.New(), Status: "disconnected"}
	_, err = app.SendOutgoingMessage(context.Background(), OutgoingMessageRequest{
		Account:        inactive,
		Contact:        contact,
		Type:           models.MessageTypeText,
		forwardOutcome: outcome,
	}, DefaultSendOptions())
	require.ErrorIs(t, err, whatsappaccount.ErrOutboundInactive)
	refusal := forwardSendFailureRefusal(err, true, *outcome)
	assert.Equal(t, 400, refusal.Status)
	assert.Equal(t, forwardErrorAccountUnavailable, refusal.ErrorType)
}

// Q6: the read-time state of a stalled forward. Neither state says "not sent".
func TestLaneEForwardDerivedDeliveryState(t *testing.T) {
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
		{"queued past the stall age: delayed, status unknown (a paused worker may still send it)", forwarded(models.MessageStatusPending, old, old, queued), forwardDeliveryDelayed},
		{"provider phase recorded, no result: may have been sent", forwarded(models.MessageStatusPending, old, old, provider(old)), forwardDeliveryUncertain},
		{"recent: no state yet", forwarded(models.MessageStatusPending, fresh, fresh, queued), ""},
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

// ---------------------------------------------------------------------------
// DB tests (run at the R3 gates; they need TEST_DATABASE_URL).
//
// Two kinds of process-boundary test follow. The "InProcess" ones are
// emulations inside this test process: the delivery worker is a queued
// after-commit callback (dropped = it never ran, run twice = a duplicate
// worker), and runtime.Goexit inside the provider call stops that goroutine
// while still running its deferred calls (so GORM rolls the provider
// transaction back). They prove the claim fence and the stored states, not
// what a real exit or restart does. The "ProcessExit" tests at the end re-run
// this test binary as a child process that calls os.Exit at the boundary and
// restart through a second child that builds App like cmd/whatomate/main.go.
// ---------------------------------------------------------------------------

// laneEForwardWorld is one organization with a sendable account, a source
// message, and a target inside its 24-hour window.
type laneEForwardWorld struct {
	app     *App
	capture *canonicalMessageCapture
	org     *models.Organization
	user    *models.User
	account *models.WhatsAppAccount
	target  *models.Contact
	source  *models.Message
}

func laneENewForwardWorld(t *testing.T) *laneEForwardWorld {
	t.Helper()
	app, capture := newCanonicalMessageTestApp(t)
	app.Config = &config.Config{}
	org := testutil.CreateTestOrganization(t, app.DB)
	adminRole := testutil.CreateAdminRole(t, app.DB, org.ID)
	user := testutil.CreateTestUser(t, app.DB, org.ID, testutil.WithRoleID(&adminRole.ID))
	account := testutil.CreateTestWhatsAppAccount(t, app.DB, org.ID)
	sourceContact := testutil.CreateTestContactWith(t, app.DB, org.ID, testutil.WithContactAccount(account.Name))
	target := testutil.CreateTestContactWith(t, app.DB, org.ID, testutil.WithContactAccount(account.Name))
	require.NoError(t, app.DB.Model(&models.Contact{}).Where("id = ?", target.ID).
		Update("last_inbound_at", time.Now().Add(-time.Hour)).Error)
	source := createCanonicalReplyMessage(t, app, org.ID, sourceContact.ID, account.Name)
	return &laneEForwardWorld{app, capture, org, user, account, target, source}
}

// post forwards the source to the target through the HTTP handler, optionally
// inside the RLS Tenant wrapper, and waits for any delivery it started.
func (w *laneEForwardWorld) post(t *testing.T, app *App, viaTenant bool, body map[string]any) (int, string) {
	t.Helper()
	if body == nil {
		body = map[string]any{"forward_of_message_id": w.source.ID.String()}
	}
	req := testutil.NewJSONRequest(t, body)
	testutil.SetAuthContext(req, w.org.ID, w.user.ID)
	testutil.SetPathParam(req, "id", w.target.ID.String())
	done := make(chan error, 1)
	go func() {
		if viaTenant {
			done <- app.Tenant((*App).SendMessage)(req)
			return
		}
		done <- app.SendMessage(req)
	}()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("forward handler did not return")
	}
	app.WaitForBackgroundTasks()
	return testutil.GetResponseStatusCode(req), string(testutil.GetResponseBody(req))
}

// commitForward commits a forward's pending row as the request path does and
// returns, unrun, the callbacks it registered for after commit (the delivery
// worker is one of them).
func (w *laneEForwardWorld) commitForward(
	t *testing.T,
	deliver func(context.Context, *models.Contact) (string, error),
) (*models.Message, []func()) {
	t.Helper()
	account := *w.account
	account.Status = whatsappaccount.StatusActive
	opts := DefaultSendOptions()
	opts.SentByUserID = &w.user.ID
	scoped := w.app.scopedApp(w.app.DB, w.org.ID)
	msg, err := scoped.SendOutgoingMessage(context.Background(), OutgoingMessageRequest{
		Account:          &account,
		Contact:          w.target,
		Type:             models.MessageTypeText,
		Content:          w.source.Content,
		forwardedFrom:    &messageForwardSource{MessageID: w.source.ID, ContactID: w.source.ContactID},
		deliveryOverride: deliver,
	}, opts)
	require.NoError(t, err)
	callbacks := scoped.takeAfterCommit()
	require.NotEmpty(t, callbacks, "delivery starts only after commit")
	return msg, callbacks
}

// restarted is a fresh App value on the same database (in-process: it runs no
// startup path). The real restart is laneEChildApp in a child process.
func (w *laneEForwardWorld) restarted() *App {
	return &App{DB: w.app.DB, Log: testutil.NopLogger(), Config: w.app.Config, WhatsApp: w.app.WhatsApp}
}

func laneELoadMessage(t *testing.T, app *App, id uuid.UUID) *models.Message {
	t.Helper()
	var stored models.Message
	require.NoError(t, app.DB.First(&stored, "id = ?", id).Error)
	return &stored
}

func laneERun(app *App, callbacks []func()) {
	for _, callback := range callbacks {
		callback()
	}
	app.WaitForBackgroundTasks()
}

// laneEExitHere ends the delivery goroutine on the spot, as a process exit
// would: only deferred calls run, so the provider transaction rolls back.
func laneEExitHere() {
	runtime.Goexit()
}

func laneEArgsInclude(args []any, want string) bool {
	for _, arg := range args {
		if fmt.Sprint(arg) == want {
			return true
		}
	}
	return false
}

// laneEFailInserts installs a BEFORE INSERT trigger that raises for rows of
// table whose column equals value, and returns a counter of refused inserts
// (a sequence, so it survives the rollbacks).
func laneEFailInserts(t *testing.T, db *gorm.DB, table, column string, value uuid.UUID) func() int64 {
	t.Helper()
	return laneEInstallRefusal(t, db, table, column, value, false, "P0001")
}

// laneERejectInserts is laneEFailInserts with a chosen SQLSTATE, e.g. 23514
// (a check constraint) or 40P01 (a deadlock, which the write retries).
func laneERejectInserts(t *testing.T, db *gorm.DB, table, column string, value uuid.UUID, sqlstate string) func() int64 {
	t.Helper()
	return laneEInstallRefusal(t, db, table, column, value, false, sqlstate)
}

// laneEFailAtCommit is laneEFailInserts as a deferred constraint trigger: the
// INSERT succeeds and the transaction's COMMIT itself returns the error.
func laneEFailAtCommit(t *testing.T, db *gorm.DB, table, column string, value uuid.UUID) func() int64 {
	t.Helper()
	return laneEInstallRefusal(t, db, table, column, value, true, "P0001")
}

func laneEInstallRefusal(t *testing.T, db *gorm.DB, table, column string, value uuid.UUID, atCommit bool, sqlstate string) func() int64 {
	t.Helper()
	name := "lane_e_refuse_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	require.NoError(t, db.Exec("CREATE SEQUENCE "+name+"_count").Error)
	require.NoError(t, db.Exec(fmt.Sprintf(`CREATE FUNCTION %[1]s() RETURNS trigger LANGUAGE plpgsql AS $fn$
BEGIN
	IF NEW.%[2]s = '%[3]s'::uuid THEN
		PERFORM nextval('%[1]s_count');
		RAISE EXCEPTION 'lane-e synthetic insert failure' USING ERRCODE = '%[4]s';
	END IF;
	RETURN NEW;
END
$fn$`, name, column, value, sqlstate)).Error)
	trigger := "CREATE TRIGGER %[1]s BEFORE INSERT ON %[2]s FOR EACH ROW EXECUTE FUNCTION %[1]s()"
	if atCommit {
		trigger = "CREATE CONSTRAINT TRIGGER %[1]s AFTER INSERT ON %[2]s DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION %[1]s()"
	}
	require.NoError(t, db.Exec(fmt.Sprintf(trigger, name, table)).Error)
	t.Cleanup(func() {
		_ = db.Exec(fmt.Sprintf("DROP TRIGGER IF EXISTS %s ON %s", name, table)).Error
		_ = db.Exec("DROP FUNCTION IF EXISTS " + name + "()").Error
		_ = db.Exec("DROP SEQUENCE IF EXISTS " + name + "_count").Error
	})
	return func() int64 {
		var refused int64
		require.NoError(t, db.Raw("SELECT CASE WHEN is_called THEN last_value ELSE 0 END FROM "+name+"_count").Scan(&refused).Error)
		return refused
	}
}

// laneELostCommitPool makes the first transaction that runs a matching
// statement COMMIT for real and then report an error, as a connection lost
// after COMMIT was sent would.
type laneELostCommitPool struct {
	gorm.ConnPool
	matches    func(query string, args []any) bool
	lost       chan struct{} // one token: one lost confirmation
	beginFails chan struct{} // one token: one failed BEGIN
}

// laneEFailNextBegin makes the next BEGIN on db fail, as when no pooled
// connection (or tenant binding) is available: nothing is written.
func laneEFailNextBegin(db *gorm.DB) *gorm.DB {
	pool := &laneELostCommitPool{
		ConnPool:   db.Statement.ConnPool,
		matches:    func(string, []any) bool { return false },
		lost:       make(chan struct{}, 1),
		beginFails: make(chan struct{}, 1),
	}
	pool.beginFails <- struct{}{}
	failing := db.Session(&gorm.Session{NewDB: true, Context: context.Background()})
	failing.Statement.ConnPool = pool
	return failing
}

func laneELoseCommitConfirmation(db *gorm.DB, matches func(query string, args []any) bool) *gorm.DB {
	pool := &laneELostCommitPool{ConnPool: db.Statement.ConnPool, matches: matches, lost: make(chan struct{}, 1)}
	pool.lost <- struct{}{}
	lossy := db.Session(&gorm.Session{NewDB: true, Context: context.Background()})
	lossy.Statement.ConnPool = pool
	return lossy
}

func (p *laneELostCommitPool) BeginTx(ctx context.Context, opts *sql.TxOptions) (gorm.ConnPool, error) {
	select {
	case <-p.beginFails:
		return nil, errors.New("synthetic: no pooled database connection for BEGIN")
	default:
	}
	beginner, ok := p.ConnPool.(gorm.TxBeginner)
	if !ok {
		return nil, gorm.ErrInvalidTransaction
	}
	tx, err := beginner.BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	return &laneELostCommitTx{Tx: tx, pool: p}, nil
}

func (p *laneELostCommitPool) GetDBConn() (*sql.DB, error) {
	if db, ok := p.ConnPool.(*sql.DB); ok {
		return db, nil
	}
	return nil, gorm.ErrInvalidDB
}

type laneELostCommitTx struct {
	*sql.Tx
	pool    *laneELostCommitPool
	matched bool
}

func (tx *laneELostCommitTx) note(query string, args []any) {
	if tx.pool.matches(query, args) {
		tx.matched = true
	}
}

func (tx *laneELostCommitTx) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	tx.note(query, args)
	return tx.Tx.ExecContext(ctx, query, args...)
}

func (tx *laneELostCommitTx) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	tx.note(query, args)
	return tx.Tx.QueryContext(ctx, query, args...)
}

func (tx *laneELostCommitTx) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	tx.note(query, args)
	return tx.Tx.QueryRowContext(ctx, query, args...)
}

func (tx *laneELostCommitTx) Commit() error {
	if err := tx.Tx.Commit(); err != nil || !tx.matched {
		return err
	}
	select {
	case <-tx.pool.lost:
		return errors.New("synthetic: connection lost after COMMIT was sent")
	default:
		return nil
	}
}

// laneEInsertRejections are the discriminating controls for a pending-row
// INSERT that PostgreSQL rejects (before COMMIT): permanent rejections are
// definite and non-retryable (500 "Failed to send message", one attempt);
// a transient one is retried by the write and then answered 503.
type laneEInsertRejection struct {
	name     string
	sqlstate string
	status   int
	attempts int64
}

func laneEInsertRejections() []laneEInsertRejection {
	return []laneEInsertRejection{
		{"check constraint (23514): would repeat", "23514", 500, 1},
		{"guard or policy trigger (P0001): would repeat", "P0001", 500, 1},
		{"deadlock (40P01): transient, retried, then try again", "40P01", 503, canonicalContactWriteAttempts},
	}
}

func laneEAssertInsertRejection(t *testing.T, status int, body string, wantStatus int) {
	t.Helper()
	assert.Equal(t, wantStatus, status, body)
	if wantStatus == 503 {
		assert.Contains(t, body, string(forwardErrorTemporarilyUnavailable))
		return
	}
	assert.Contains(t, body, "Failed to send message")
	assert.NotContains(t, body, string(forwardErrorTemporarilyUnavailable))
	assert.NotContains(t, body, string(forwardErrorOutcomeUnknown))
}

// Q5, own transaction: a rejected INSERT never reaches COMMIT, so it is
// definite; whether it is retryable depends on whether it would repeat.
func TestLaneEForwardInsertRejectionsAreClassified(t *testing.T) {
	for _, tc := range laneEInsertRejections() {
		t.Run(tc.name, func(t *testing.T) {
			w := laneENewForwardWorld(t)
			refused := laneERejectInserts(t, w.app.DB, "messages", "contact_id", w.target.ID, tc.sqlstate)
			status, body := w.post(t, w.app, false, nil)
			laneEAssertInsertRejection(t, status, body, tc.status)
			assert.Equal(t, tc.attempts, refused())
			assert.Zero(t, laneECountMessages(t, w.app, w.target))
			assert.Empty(t, w.capture.snapshot(), "no provider attempt")
		})
	}
}

// E2R3-4 on the real path: the forward audit INSERT is rejected with a chosen
// SQLSTATE, in the forward's own transaction and under RLS. Only a rejection
// that would repeat (P0001) is definite and non-retryable; a deadlock (40P01)
// is retried by the write and then answered 503; a cancellation (57014) is
// answered 503 at once. Reverting "wrap only repeatable causes" fails this.
func TestLaneEForwardAuditRejectionsAreClassified(t *testing.T) {
	cases := []laneEInsertRejection{
		{"deadlock (40P01): transient, retried, then try again", "40P01", 503, canonicalContactWriteAttempts},
		{"cancellation (57014): transient, try again", "57014", 503, 1},
		{"raised by a trigger or policy (P0001): would repeat", "P0001", 500, 1},
	}
	for _, rls := range []bool{false, true} {
		for _, tc := range cases {
			t.Run(fmt.Sprintf("rls=%t/%s", rls, tc.name), func(t *testing.T) {
				w := laneENewForwardWorld(t)
				if rls {
					w.app.Config = &config.Config{Database: config.DatabaseConfig{RLSEnabled: true}}
				}
				refused := laneERejectInserts(t, w.app.DB, "audit_logs", "user_id", w.user.ID, tc.sqlstate)
				status, body := w.post(t, w.app, rls, nil)
				laneEAssertInsertRejection(t, status, body, tc.status)
				assert.Equal(t, tc.attempts, refused())
				assert.Zero(t, laneECountMessages(t, w.app, w.target), "the message rolled back with its audit")
				assert.Empty(t, w.capture.snapshot(), "no provider attempt")
			})
		}
	}
}

// Q5 under RLS, with database-raised failures. A trigger error aborts the
// savepoint, so "no row" would hold whatever the savepoint or Tenant code
// did: these show the classification, the attempts and that nothing reached
// the provider, not the rollback path. The rollback path is
// TestLaneEForwardRLSGoLevelFailureRollsBack.
func TestLaneEForwardRLSTriggerRejectionsAreClassified(t *testing.T) {
	for _, tc := range laneEInsertRejections() {
		t.Run(tc.name, func(t *testing.T) {
			w := laneENewForwardWorld(t)
			w.app.Config = &config.Config{Database: config.DatabaseConfig{RLSEnabled: true}}
			refused := laneERejectInserts(t, w.app.DB, "messages", "contact_id", w.target.ID, tc.sqlstate)
			status, body := w.post(t, w.app, true, nil)
			laneEAssertInsertRejection(t, status, body, tc.status)
			assert.Equal(t, tc.attempts, refused())
			assert.Zero(t, laneECountMessages(t, w.app, w.target), "no durable pending row")
			assert.Empty(t, w.capture.snapshot(), "no provider attempt")
		})
	}
}

// Q5 under RLS, with a Go-level failure after the INSERT: the message and its
// audit row were written and the request transaction is still healthy (no
// SQL error aborted it). Only the savepoint rollback and Tenant's rollback of
// the error response keep the rows out; if neither ran, they would be there.
func TestLaneEForwardRLSGoLevelFailureRollsBack(t *testing.T) {
	w := laneENewForwardWorld(t)
	w.app.Config = &config.Config{Database: config.DatabaseConfig{RLSEnabled: true}}
	preview := "lane-e savepoint " + uuid.NewString()
	require.NoError(t, w.app.DB.Model(&models.Message{}).Where("id = ?", w.source.ID).Update("content", preview).Error)
	var before models.Contact
	require.NoError(t, w.app.DB.First(&before, "id = ?", w.target.ID).Error)

	target := w.target.ID
	inserted := 0
	insertHook := "laneE:count_forward_insert_" + uuid.NewString()
	require.NoError(t, w.app.DB.Callback().Create().After("gorm:create").Register(insertHook, func(db *gorm.DB) {
		if message, ok := db.Statement.Dest.(*models.Message); ok && message.ContactID == target && db.Error == nil {
			inserted++
		}
	}))
	t.Cleanup(func() { _ = w.app.DB.Callback().Create().Remove(insertHook) })
	failHook := "laneE:fail_contact_preview_" + uuid.NewString()
	require.NoError(t, w.app.DB.Callback().Update().Before("gorm:update").Register(failHook, func(db *gorm.DB) {
		dest, ok := db.Statement.Dest.(map[string]any)
		if ok && db.Statement.Table == "contacts" && fmt.Sprint(dest["last_message_preview"]) == preview {
			_ = db.AddError(errors.New("synthetic: contact preview update refused (no SQL ran)"))
		}
	}))
	t.Cleanup(func() { _ = w.app.DB.Callback().Update().Remove(failHook) })

	status, body := w.post(t, w.app, true, nil)
	assert.Equal(t, 503, status, body)
	assert.Contains(t, body, string(forwardErrorTemporarilyUnavailable))
	assert.Equal(t, 1, inserted, "the INSERT succeeded before the failure")
	assert.Zero(t, laneECountMessages(t, w.app, w.target), "no message row")
	var audits int64
	require.NoError(t, w.app.DB.Model(&models.AuditLog{}).
		Where("organization_id = ? AND user_id = ? AND resource_type = ?", w.org.ID, w.user.ID, "message").
		Count(&audits).Error)
	assert.Zero(t, audits, "no forward audit row")
	var after models.Contact
	require.NoError(t, w.app.DB.First(&after, "id = ?", w.target.ID).Error)
	assert.Equal(t, before.LastMessagePreview, after.LastMessagePreview, "contact preview unchanged")
	assert.Empty(t, w.capture.snapshot(), "no provider call")
}

// Q5 phase "no write began" on the real path: BEGIN itself fails, so nothing
// was written. A definite 503.
func TestLaneEForwardBeginFailureIsDefinite(t *testing.T) {
	w := laneENewForwardWorld(t)
	healthy := w.app.DB
	w.app.DB = laneEFailNextBegin(healthy)
	status, body := w.post(t, w.app, false, nil)
	w.app.DB = healthy

	assert.Equal(t, 503, status, body)
	assert.Contains(t, body, string(forwardErrorTemporarilyUnavailable))
	assert.Zero(t, laneECountMessages(t, w.app, w.target))
	assert.Empty(t, w.capture.snapshot())
}

// Q5: the pending row commits but the COMMIT confirmation is lost. The handler
// cannot tell this from a failed COMMIT, so it answers "outcome unknown" and
// makes no second attempt. The row is real; nothing sends it by itself.
func TestLaneEForwardLostCommitConfirmationStaysAmbiguous(t *testing.T) {
	w := laneENewForwardWorld(t)
	healthy := w.app.DB
	target := w.target.ID.String()
	w.app.DB = laneELoseCommitConfirmation(healthy, func(query string, args []any) bool {
		return strings.HasPrefix(strings.TrimSpace(query), `INSERT INTO "messages"`) && laneEArgsInclude(args, target)
	})
	status, body := w.post(t, w.app, false, nil)
	w.app.DB = healthy

	assert.Equal(t, 500, status, body)
	assert.Contains(t, body, string(forwardErrorOutcomeUnknown))
	var rows []models.Message
	require.NoError(t, healthy.Where("organization_id = ? AND contact_id = ?", w.org.ID, w.target.ID).Find(&rows).Error)
	require.Len(t, rows, 1, "committed once, never retried")
	assert.Equal(t, models.MessageStatusPending, rows[0].Status)
	assert.Equal(t, forwardDispatchQueued, rows[0].Metadata[forwardDispatchMetadataKey])
	assert.Empty(t, w.capture.snapshot(), "no delivery was started")
	// Once stale it reads as delayed (status unknown), never as not sent.
	assert.Equal(t, forwardDeliveryDelayed, forwardDeliveryState(&rows[0], time.Now().Add(10*time.Minute)))
}

// Q5 commit boundary: the write reached COMMIT and COMMIT itself returned an
// error. Here it really failed, but the server cannot know that from the
// error alone (a dropped connection looks the same), so the answer is
// "outcome unknown" and there is no second attempt.
func TestLaneEForwardCommitFailureStaysAmbiguous(t *testing.T) {
	w := laneENewForwardWorld(t)
	refused := laneEFailAtCommit(t, w.app.DB, "messages", "contact_id", w.target.ID)

	status, body := w.post(t, w.app, false, nil)
	assert.Equal(t, 500, status, body)
	assert.Contains(t, body, string(forwardErrorOutcomeUnknown))
	assert.Equal(t, int64(1), refused(), "one COMMIT attempt, never retried")
	assert.Zero(t, laneECountMessages(t, w.app, w.target))
	assert.Empty(t, w.capture.snapshot(), "no delivery was started")
}

// Q5 (item 7): under RLS, failures that would repeat are never "try again".
// They return what an ordinary send returns for the same condition.
func TestLaneEForwardRLSPermanentFailuresAreNotRetryable(t *testing.T) {
	rls := &config.Config{Database: config.DatabaseConfig{RLSEnabled: true}}

	t.Run("inactive account", func(t *testing.T) {
		w := laneENewForwardWorld(t)
		w.app.Config = rls
		require.NoError(t, w.app.DB.Model(&models.WhatsAppAccount{}).Where("id = ?", w.account.ID).
			Update("status", "disconnected").Error)
		status, body := w.post(t, w.app, true, nil)
		assert.Equal(t, 400, status, body)
		assert.Contains(t, body, string(forwardErrorAccountUnavailable))
		assert.Contains(t, body, "Failed to resolve WhatsApp account")
		assert.Zero(t, laneECountMessages(t, w.app, w.target))
		assert.Empty(t, w.capture.snapshot())
	})

	t.Run("strict audit insert failure", func(t *testing.T) {
		w := laneENewForwardWorld(t)
		w.app.Config = rls
		refused := laneEFailInserts(t, w.app.DB, "audit_logs", "user_id", w.user.ID)
		status, body := w.post(t, w.app, true, nil)
		assert.Equal(t, 500, status, body)
		assert.Contains(t, body, "Failed to send message")
		assert.NotContains(t, body, string(forwardErrorTemporarilyUnavailable))
		assert.NotContains(t, body, string(forwardErrorOutcomeUnknown))
		assert.Equal(t, int64(1), refused())
		assert.Zero(t, laneECountMessages(t, w.app, w.target), "the savepoint and the request rolled back")
		assert.Empty(t, w.capture.snapshot())
	})

	t.Run("tenant scope mismatch", func(t *testing.T) {
		w := laneENewForwardWorld(t)
		w.app.Config = rls
		other := testutil.CreateTestOrganization(t, w.app.DB)
		outer := w.app.DB.Begin()
		require.NoError(t, outer.Error)
		t.Cleanup(func() { _ = outer.Rollback().Error })
		require.NoError(t, database.SetTenantContext(outer, w.org.ID))
		status, body := w.post(t, w.app.scopedApp(outer, other.ID), false, nil)
		assert.Equal(t, 500, status, body)
		assert.Contains(t, body, "Failed to send message")
		assert.NotContains(t, body, string(forwardErrorTemporarilyUnavailable))
		require.NoError(t, outer.Rollback().Error)
		assert.Zero(t, laneECountMessages(t, w.app, w.target))
		assert.Empty(t, w.capture.snapshot())
	})
}

// Q6 claim policy: queued -> provider moves exactly one row, once.
func TestLaneEForwardProviderClaimIsSingleUse(t *testing.T) {
	w := laneENewForwardWorld(t)
	msg, _ := w.commitForward(t, nil)

	claimed, err := w.app.claimForwardProviderPhase(w.org.ID, msg.ID)
	require.NoError(t, err)
	assert.True(t, claimed)
	stored := laneELoadMessage(t, w.app, msg.ID)
	assert.Equal(t, forwardDispatchProvider, stored.Metadata[forwardDispatchMetadataKey])
	assert.NotEmpty(t, stored.Metadata[forwardDispatchAtMetadataKey])

	claimed, err = w.app.claimForwardProviderPhase(w.org.ID, msg.ID)
	require.NoError(t, err)
	assert.False(t, claimed, "a second claim moves nothing")
	claimed, err = w.app.claimForwardProviderPhase(w.org.ID, uuid.New())
	require.NoError(t, err)
	assert.False(t, claimed, "a missing row moves nothing")
}

// Q6 re-entry policy: a worker whose row is no longer pending and queued stops
// without calling the provider or touching the row.
func TestLaneEForwardWorkerStopsWhenItsClaimMovesNothing(t *testing.T) {
	w := laneENewForwardWorld(t)
	calls := 0
	msg, pending := w.commitForward(t, func(context.Context, *models.Contact) (string, error) {
		calls++
		return "wamid.lane-e-mismatch", nil
	})
	require.NoError(t, w.app.DB.Model(&models.Message{}).Where("id = ?", msg.ID).
		UpdateColumns(map[string]any{"status": models.MessageStatusFailed, "error_message": "settled elsewhere"}).Error)
	before := laneELoadMessage(t, w.app, msg.ID)

	laneERun(w.app, pending)
	after := laneELoadMessage(t, w.app, msg.ID)
	assert.Zero(t, calls)
	assert.Equal(t, before.Status, after.Status)
	assert.Equal(t, before.ErrorMessage, after.ErrorMessage)
	assert.Equal(t, forwardDispatchQueued, after.Metadata[forwardDispatchMetadataKey])
	assert.True(t, before.UpdatedAt.Equal(after.UpdatedAt), "the row is untouched")
}

// Q6: the marker's COMMIT is ambiguous (here it committed and the answer was
// lost). The worker fails closed: it stops before the provider and records
// "not sent", which is true because it never called the provider.
func TestLaneEForwardAmbiguousMarkerStopsBeforeProvider(t *testing.T) {
	w := laneENewForwardWorld(t)
	calls := 0
	msg, pending := w.commitForward(t, func(context.Context, *models.Contact) (string, error) {
		calls++
		return "wamid.lane-e-marker", nil
	})
	healthy := w.app.DB
	id := msg.ID.String()
	w.app.DB = laneELoseCommitConfirmation(healthy, func(query string, args []any) bool {
		return strings.HasPrefix(strings.TrimSpace(query), `UPDATE "messages"`) &&
			strings.Contains(query, "jsonb_build_object") && laneEArgsInclude(args, id)
	})
	laneERun(w.app, pending)
	w.app.DB = healthy

	stored := laneELoadMessage(t, w.app, msg.ID)
	assert.Zero(t, calls, "no provider call")
	assert.Equal(t, forwardDispatchProvider, stored.Metadata[forwardDispatchMetadataKey], "the marker did commit")
	assert.Equal(t, models.MessageStatusFailed, stored.Status)
	assert.Equal(t, forwardServerBusyError().Error(), stored.ErrorMessage, "user-safe text, never the raw error")
}

// Item 6: the marker fails before its UPDATE is sent (as when no pool
// connection frees up within its deadline). The marker certainly did not
// commit; the worker stops before the provider and stores user-safe text.
func TestLaneEForwardMarkerFailureBeforeUpdateStopsWithSafeText(t *testing.T) {
	w := laneENewForwardWorld(t)
	calls := 0
	msg, pending := w.commitForward(t, func(context.Context, *models.Contact) (string, error) {
		calls++
		return "wamid.lane-e-before-update", nil
	})
	once := make(chan struct{}, 1)
	once <- struct{}{}
	name := "laneE:marker_before_update_" + uuid.NewString()
	require.NoError(t, w.app.DB.Callback().Update().Before("gorm:update").Register(name, func(db *gorm.DB) {
		dest, ok := db.Statement.Dest.(map[string]interface{})
		if !ok || db.Statement.Table != "messages" || !strings.Contains(fmt.Sprintf("%v", dest["metadata"]), "jsonb_build_object") {
			return
		}
		select {
		case <-once:
			_ = db.AddError(fmt.Errorf("synthetic: no pooled database connection: %w", context.DeadlineExceeded))
		default:
		}
	}))
	t.Cleanup(func() { _ = w.app.DB.Callback().Update().Remove(name) })

	laneERun(w.app, pending)
	stored := laneELoadMessage(t, w.app, msg.ID)
	assert.Zero(t, calls, "no provider call")
	assert.Equal(t, forwardDispatchQueued, stored.Metadata[forwardDispatchMetadataKey], "the marker never committed")
	assert.Equal(t, models.MessageStatusFailed, stored.Status)
	assert.Equal(t, forwardServerBusyError().Error(), stored.ErrorMessage)
	assert.NotContains(t, stored.ErrorMessage, "deadline")
}

// The claim's error says which side of COMMIT it failed on.
func TestLaneEForwardClaimErrorReportsCommitBoundary(t *testing.T) {
	before := &forwardClaimError{err: context.DeadlineExceeded}
	after := &forwardClaimError{reachedCommit: true, err: errors.New("connection reset")}
	var claimErr *forwardClaimError
	require.True(t, errors.As(fmt.Errorf("x: %w", before), &claimErr))
	assert.False(t, claimErr.reachedCommit)
	assert.ErrorIs(t, before, context.DeadlineExceeded)
	require.True(t, errors.As(after, &claimErr))
	assert.True(t, claimErr.reachedCommit)
}

// Q6 boundary 1, in-process emulation: the worker callback is dropped. This
// shows the stored state (queued, read as delayed) and that no in-process
// path resends a queued forward. It cannot show what a real restart does:
// that is TestLaneEForwardProcessExitBeforeMarker.
func TestLaneEForwardInProcessExitBeforeMarkerHasNoResendPath(t *testing.T) {
	w := laneENewForwardWorld(t)
	calls := 0
	msg, _ := w.commitForward(t, func(context.Context, *models.Contact) (string, error) {
		calls++
		return "wamid.lane-e-b1", nil
	})
	// The queued worker never runs.
	restarted := w.restarted()
	stored := laneELoadMessage(t, restarted, msg.ID)
	assert.Equal(t, models.MessageStatusPending, stored.Status)
	assert.Equal(t, forwardDispatchQueued, stored.Metadata[forwardDispatchMetadataKey])
	assert.Equal(t, forwardDeliveryDelayed, forwardDeliveryState(stored, time.Now().Add(10*time.Minute)))
	restarted.WaitForBackgroundTasks()
	assert.Zero(t, calls)
	assert.Empty(t, w.capture.snapshot())
}

// Q6 boundary 2, in-process emulation (Goexit runs the deferred rollback): the
// marker committed, so the row reads uncertain, and the claim fence stops any
// later worker before the provider. Real exit:
// TestLaneEForwardProcessExitAfterMarkerBeforeProvider.
func TestLaneEForwardInProcessClaimFenceAfterMarker(t *testing.T) {
	w := laneENewForwardWorld(t)
	entered := 0
	msg, pending := w.commitForward(t, func(context.Context, *models.Contact) (string, error) {
		entered++
		laneEExitHere() // inside the provider transaction, before any provider effect
		return "", nil
	})
	laneERun(w.app, pending)
	require.Equal(t, 1, entered)

	restarted := w.restarted()
	stored := laneELoadMessage(t, restarted, msg.ID)
	assert.Equal(t, models.MessageStatusPending, stored.Status)
	assert.Equal(t, forwardDispatchProvider, stored.Metadata[forwardDispatchMetadataKey])
	assert.Equal(t, forwardDeliveryUncertain, forwardDeliveryState(stored, time.Now().Add(10*time.Minute)))
	claimed, err := restarted.claimForwardProviderPhase(w.org.ID, msg.ID)
	require.NoError(t, err)
	assert.False(t, claimed, "a recorded provider phase is never claimed again")

	laneERun(w.app, pending) // a duplicate worker for the same row
	assert.Equal(t, 1, entered, "the duplicate stopped at its claim")
	assert.Equal(t, models.MessageStatusPending, laneELoadMessage(t, restarted, msg.ID).Status)
}

// Q6 boundary 3, in-process emulation: the provider accepted and the worker
// stopped before recording the result. The row stays uncertain and the claim
// fence prevents a second provider effect. Real exit:
// TestLaneEForwardProcessExitAfterProviderAccepted.
func TestLaneEForwardInProcessClaimFenceAfterProviderAccepted(t *testing.T) {
	w := laneENewForwardWorld(t)
	accepted := 0
	msg, pending := w.commitForward(t, func(context.Context, *models.Contact) (string, error) {
		accepted++ // the provider effect happened
		laneEExitHere()
		return "wamid.lane-e-b3", nil
	})
	laneERun(w.app, pending)
	require.Equal(t, 1, accepted)

	restarted := w.restarted()
	stored := laneELoadMessage(t, restarted, msg.ID)
	assert.Equal(t, models.MessageStatusPending, stored.Status, "the result was never recorded")
	assert.Equal(t, forwardDeliveryUncertain, forwardDeliveryState(stored, time.Now().Add(10*time.Minute)))
	claimed, err := restarted.claimForwardProviderPhase(w.org.ID, msg.ID)
	require.NoError(t, err)
	assert.False(t, claimed)

	laneERun(w.app, pending) // a duplicate worker for the same row
	assert.Equal(t, 1, accepted, "no second provider effect")
	assert.Equal(t, forwardDeliveryUncertain,
		forwardDeliveryState(laneELoadMessage(t, restarted, msg.ID), time.Now().Add(10*time.Minute)))
}

// Q6, in-process: "delayed" is not "not sent". A worker paused past the stall
// age still owns its queued row: when it resumes it sends, exactly once. Real
// processes: TestLaneEForwardProcessPausedWorkerResumes.
func TestLaneEForwardInProcessPausedWorkerResumesAndSendsOnce(t *testing.T) {
	w := laneENewForwardWorld(t)
	calls := 0
	wamid := "wamid.lane-e-paused-" + uuid.NewString()[:8]
	msg, pending := w.commitForward(t, func(context.Context, *models.Contact) (string, error) {
		calls++
		return wamid, nil
	})
	require.NoError(t, w.app.DB.Model(&models.Message{}).Where("id = ?", msg.ID).
		UpdateColumn("created_at", time.Now().Add(-10*time.Minute)).Error)
	assert.Equal(t, forwardDeliveryDelayed,
		forwardDeliveryState(laneELoadMessage(t, w.restarted(), msg.ID), time.Now()), "shown as delayed while paused")

	laneERun(w.app, pending) // the worker resumes
	stored := laneELoadMessage(t, w.app, msg.ID)
	assert.Equal(t, models.MessageStatusSent, stored.Status, stored.ErrorMessage)
	assert.Equal(t, wamid, stored.WhatsAppMessageID)
	assert.Equal(t, 1, calls)
	assert.Empty(t, forwardDeliveryState(stored, time.Now()))
}

// ---------------------------------------------------------------------------
// Real process-exit tests (R3 gate). The parent re-runs this test binary as a
// child process (LANE_E_EXIT_CHILD) that commits a forward and calls os.Exit
// at one boundary: no deferred call runs, PostgreSQL sees the connection drop
// and aborts any open transaction. A second child then plays the restarted
// server: it builds App as cmd/whatomate/main.go does (database.NewPostgres,
// then the App fields a forward uses) and serves the row on the read path,
// which is all a server does with a stalled forward (no startup step resends
// or settles one). Provider calls go to a Graph stub in the parent, so a
// provider effect survives the child's exit.
// ---------------------------------------------------------------------------

// laneEExitAtHook is the exit code a child uses at its boundary, so the parent
// can tell that exit from a failure.
const laneEExitAtHook = 3

type laneEExitWorld struct {
	*laneEForwardWorld
	graph   *httptest.Server
	effects *atomic.Int64 // provider sends the Graph stub accepted
}

func laneENewExitWorld(t *testing.T) *laneEExitWorld {
	t.Helper()
	w := laneENewForwardWorld(t)
	effects := &atomic.Int64{}
	graph := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/messages") {
			rw.WriteHeader(http.StatusNotFound)
			return
		}
		n := effects.Add(1)
		rw.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(rw, `{"messages":[{"id":"wamid.lane-e-exit-%d-%s"}]}`, n, uuid.NewString()[:8])
	}))
	t.Cleanup(graph.Close)
	return &laneEExitWorld{laneEForwardWorld: w, graph: graph, effects: effects}
}

func (x *laneEExitWorld) child(ctx context.Context, mode string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestLaneEForwardExitChild$", "-test.count=1", "-test.v")
	cmd.Env = append(os.Environ(),
		"LANE_E_EXIT_CHILD="+mode,
		"LANE_E_EXIT_GRAPH="+x.graph.URL,
		"LANE_E_EXIT_ORG="+x.org.ID.String(),
		"LANE_E_EXIT_USER="+x.user.ID.String(),
		"LANE_E_EXIT_ACCOUNT="+x.account.ID.String(),
		"LANE_E_EXIT_TARGET="+x.target.ID.String(),
		"LANE_E_EXIT_SOURCE="+x.source.ID.String(),
	)
	return cmd
}

// run starts a child, waits for it and returns its exit code and output.
func (x *laneEExitWorld) run(t *testing.T, mode string) (int, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	out, err := x.child(ctx, mode).CombinedOutput()
	return laneEExitCode(t, err, string(out)), string(out)
}

func laneEExitCode(t *testing.T, err error, out string) int {
	t.Helper()
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	require.True(t, errors.As(err, &exitErr), "child did not run: %v\n%s", err, out)
	return exitErr.ExitCode()
}

// laneEChildValue reads a "LANE_E_<key>=value" line a child printed.
func laneEChildValue(out, key string) string {
	for _, line := range strings.Split(out, "\n") {
		if value, ok := strings.CutPrefix(strings.TrimSpace(line), "LANE_E_"+key+"="); ok {
			return value
		}
	}
	return ""
}

// targetForward is the one forward a child committed for this world's target.
func (x *laneEExitWorld) targetForward(t *testing.T) *models.Message {
	t.Helper()
	var rows []models.Message
	require.NoError(t, x.app.DB.Where("organization_id = ? AND contact_id = ?", x.org.ID, x.target.ID).Find(&rows).Error)
	require.Len(t, rows, 1)
	return &rows[0]
}

// age moves a stalled forward's clocks past the stall age.
func (x *laneEExitWorld) age(t *testing.T, id uuid.UUID) {
	t.Helper()
	past := time.Now().Add(-10 * time.Minute).UTC()
	require.NoError(t, x.app.DB.Exec(`UPDATE messages
		SET created_at = ?, updated_at = ?,
		    metadata = CASE WHEN metadata ->> 'forward_dispatch_at' IS NOT NULL
		      THEN jsonb_set(metadata, '{forward_dispatch_at}', to_jsonb(?::text)) ELSE metadata END
		WHERE id = ?`, past, past, past.Format(time.RFC3339Nano), id).Error)
}

// laneEChildApp builds App as cmd/whatomate/main.go does, for what a forward
// needs: database.NewPostgres from the database configuration, then the App
// fields. Redis, the websocket hub and the job queue are left out; a forward's
// delivery runs without them.
func laneEChildApp(t *testing.T) *App {
	t.Helper()
	cfg := &config.Config{Database: config.DatabaseConfig{
		URL:             os.Getenv("TEST_DATABASE_URL"),
		MaxOpenConns:    5,
		MaxIdleConns:    2,
		ConnMaxLifetime: 60,
	}}
	db, err := database.NewPostgres(&cfg.Database, false)
	require.NoError(t, err)
	log := testutil.NopLogger()
	return &App{
		Config:     cfg,
		DB:         db,
		Log:        log,
		WhatsApp:   whatsapp.NewWithBaseURL(log, os.Getenv("LANE_E_EXIT_GRAPH")),
		HTTPClient: &http.Client{Timeout: 5 * time.Second},
	}
}

// TestLaneEForwardExitChild is the child half of the process-exit tests. It
// does nothing unless the parent set LANE_E_EXIT_CHILD.
func TestLaneEForwardExitChild(t *testing.T) {
	mode := os.Getenv("LANE_E_EXIT_CHILD")
	if mode == "" {
		t.Skip("child process for the TestLaneEForwardProcess* tests")
	}
	id := func(key string) uuid.UUID {
		value, err := uuid.Parse(os.Getenv("LANE_E_EXIT_" + key))
		require.NoError(t, err, key)
		return value
	}
	app := laneEChildApp(t)
	orgID, targetID := id("ORG"), id("TARGET")
	if mode == "restart" || mode == "restart-claim" {
		laneEChildRestart(t, app, orgID, targetID, mode == "restart-claim")
		return
	}

	var account models.WhatsAppAccount
	require.NoError(t, app.DB.First(&account, "id = ? AND organization_id = ?", id("ACCOUNT"), orgID).Error)
	require.NoError(t, app.prepareWhatsAppAccountForOutbound(&account))
	var target models.Contact
	require.NoError(t, app.DB.First(&target, "id = ? AND organization_id = ?", targetID, orgID).Error)
	var source models.Message
	require.NoError(t, app.DB.First(&source, "id = ? AND organization_id = ?", id("SOURCE"), orgID).Error)

	var deliver func(context.Context, *models.Contact) (string, error)
	switch mode {
	case "exit-after-marker":
		deliver = func(context.Context, *models.Contact) (string, error) {
			// Inside the provider transaction, before any provider effect.
			os.Exit(laneEExitAtHook)
			return "", nil
		}
	case "exit-after-provider":
		deliver = func(ctx context.Context, contact *models.Contact) (string, error) {
			wamid, err := app.WhatsApp.SendTextMessage(ctx, app.toWhatsAppAccount(&account),
				whatsapp.Recipient{Phone: contact.PhoneNumber, BSUID: contact.BSUID}, source.Content, "")
			if err == nil {
				// The provider accepted; its result is never recorded.
				os.Exit(laneEExitAtHook)
			}
			return wamid, err
		}
	}

	userID := id("USER")
	opts := DefaultSendOptions()
	opts.SentByUserID = &userID
	scoped := app.scopedApp(app.DB, orgID)
	msg, err := scoped.SendOutgoingMessage(context.Background(), OutgoingMessageRequest{
		Account:          &account,
		Contact:          &target,
		Type:             models.MessageTypeText,
		Content:          source.Content,
		forwardedFrom:    &messageForwardSource{MessageID: source.ID, ContactID: source.ContactID},
		deliveryOverride: deliver,
	}, opts)
	require.NoError(t, err)
	worker := scoped.takeAfterCommit()
	fmt.Printf("LANE_E_MESSAGE=%s\n", msg.ID)

	switch mode {
	case "exit-before-marker":
		// Committed; the delivery worker never starts.
		os.Exit(laneEExitAtHook)
	case "pause-before-worker":
		// A live process whose worker has not run yet, until the parent says so.
		var line string
		_, _ = fmt.Fscanln(os.Stdin, &line)
	}
	for _, callback := range worker {
		callback()
	}
	app.WaitForBackgroundTasks()
}

// laneEChildRestart is the restarted server: it serves the target's forward
// on the read path and, if asked, tries to claim it again.
func laneEChildRestart(t *testing.T, app *App, orgID, targetID uuid.UUID, tryClaim bool) {
	t.Helper()
	var messages []models.Message
	require.NoError(t, app.DB.Where("organization_id = ? AND contact_id = ?", orgID, targetID).Find(&messages).Error)
	require.Len(t, messages, 1)
	response := app.buildMessagesResponse(messages)[0]
	fmt.Printf("LANE_E_STATUS=%s\nLANE_E_STATE=%s\nLANE_E_DISPATCH=%s\n",
		response.Status, response.ForwardDeliveryState, response.ForwardDispatch)
	if tryClaim {
		claimed, err := app.claimForwardProviderPhase(orgID, messages[0].ID)
		require.NoError(t, err)
		fmt.Printf("LANE_E_CLAIM=%t\n", claimed)
	}
	app.WaitForBackgroundTasks()
}

// Q6 boundary 1, real exit: the request committed and the process exited
// before its worker started. A restarted server reads the forward as delayed
// and nothing sends it.
func TestLaneEForwardProcessExitBeforeMarker(t *testing.T) {
	x := laneENewExitWorld(t)
	code, out := x.run(t, "exit-before-marker")
	require.Equal(t, laneEExitAtHook, code, out)
	row := x.targetForward(t)
	assert.Equal(t, row.ID.String(), laneEChildValue(out, "MESSAGE"))
	assert.Equal(t, models.MessageStatusPending, row.Status)
	assert.Equal(t, forwardDispatchQueued, row.Metadata[forwardDispatchMetadataKey])

	x.age(t, row.ID)
	code, out = x.run(t, "restart")
	require.Zero(t, code, out)
	assert.Equal(t, string(models.MessageStatusPending), laneEChildValue(out, "STATUS"), out)
	assert.Equal(t, forwardDeliveryDelayed, laneEChildValue(out, "STATE"), out)
	assert.Equal(t, forwardDispatchQueued, laneEChildValue(out, "DISPATCH"), out)
	assert.Zero(t, x.effects.Load(), "no provider call, before or after the restart")
}

// Q6 boundary 2, real exit: the worker committed the marker, then the process
// died inside the provider transaction before any provider effect. PostgreSQL
// aborts that transaction; the row reads uncertain after a restart and can
// never be claimed again.
func TestLaneEForwardProcessExitAfterMarkerBeforeProvider(t *testing.T) {
	x := laneENewExitWorld(t)
	code, out := x.run(t, "exit-after-marker")
	require.Equal(t, laneEExitAtHook, code, out)
	row := x.targetForward(t)
	assert.Equal(t, models.MessageStatusPending, row.Status)
	assert.Equal(t, forwardDispatchProvider, row.Metadata[forwardDispatchMetadataKey], "the marker committed before the provider transaction")

	x.age(t, row.ID)
	code, out = x.run(t, "restart-claim")
	require.Zero(t, code, out)
	assert.Equal(t, forwardDeliveryUncertain, laneEChildValue(out, "STATE"), out)
	assert.Equal(t, "false", laneEChildValue(out, "CLAIM"), "the restarted server cannot claim it again")
	assert.Zero(t, x.effects.Load())
}

// Q6 boundary 3, real exit: the provider accepted the message and the process
// died before recording the result. It stays uncertain across a restart and
// there is never a second provider effect.
func TestLaneEForwardProcessExitAfterProviderAccepted(t *testing.T) {
	x := laneENewExitWorld(t)
	code, out := x.run(t, "exit-after-provider")
	require.Equal(t, laneEExitAtHook, code, out)
	require.Equal(t, int64(1), x.effects.Load(), "the provider accepted the message")
	row := x.targetForward(t)
	assert.Equal(t, models.MessageStatusPending, row.Status, "its result was never recorded")
	assert.Equal(t, forwardDispatchProvider, row.Metadata[forwardDispatchMetadataKey])

	x.age(t, row.ID)
	code, out = x.run(t, "restart-claim")
	require.Zero(t, code, out)
	assert.Equal(t, forwardDeliveryUncertain, laneEChildValue(out, "STATE"), out)
	assert.Equal(t, "false", laneEChildValue(out, "CLAIM"))
	assert.Equal(t, int64(1), x.effects.Load(), "no second provider effect")
}

// Q6 paused worker, real processes: a live process whose worker has not run
// yet. While it is paused, a second process reads the forward as delayed
// (never "not sent"); when the first resumes, it sends exactly once.
func TestLaneEForwardProcessPausedWorkerResumes(t *testing.T) {
	x := laneENewExitWorld(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	// Registered before StdinPipe and Start, so a failure there leaves no
	// timer or child behind; it also kills a child that is still running.
	defer cancel()
	paused := x.child(ctx, "pause-before-worker")
	resume, err := paused.StdinPipe()
	require.NoError(t, err)
	var pausedOut strings.Builder
	paused.Stdout, paused.Stderr = &pausedOut, &pausedOut
	require.NoError(t, paused.Start())
	// Unconditional, so a failed assertion below never leaks the child: the
	// cancel kills it if it is still running, and Wait reaps it.
	waited := false
	t.Cleanup(func() {
		cancel()
		if !waited {
			_ = paused.Wait()
		}
	})

	var row models.Message
	require.Eventually(t, func() bool {
		return x.app.DB.Where("organization_id = ? AND contact_id = ?", x.org.ID, x.target.ID).
			Limit(1).Find(&row).Error == nil && row.ID != uuid.Nil
	}, time.Minute, 100*time.Millisecond, "the paused process committed its forward")
	x.age(t, row.ID)
	code, out := x.run(t, "restart")
	require.Zero(t, code, out)
	assert.Equal(t, forwardDeliveryDelayed, laneEChildValue(out, "STATE"), out)
	assert.Zero(t, x.effects.Load())

	_, err = fmt.Fprintln(resume, "resume")
	require.NoError(t, err)
	waitErr := paused.Wait()
	waited = true
	require.Zero(t, laneEExitCode(t, waitErr, pausedOut.String()), pausedOut.String())
	stored := x.targetForward(t)
	assert.Equal(t, models.MessageStatusSent, stored.Status, stored.ErrorMessage)
	assert.Equal(t, int64(1), x.effects.Load(), "sent exactly once")
}
