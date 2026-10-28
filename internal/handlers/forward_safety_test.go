package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/config"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/internal/storage"
	appwebsocket "github.com/shridarpatil/whatomate/internal/websocket"
	"github.com/shridarpatil/whatomate/internal/whatsappaccount"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
)

func TestForwardAdmissionBoundsReservationsAndActiveWorkers(t *testing.T) {
	app := &App{}
	first, err := app.reserveForward()
	require.NoError(t, err)
	second, err := (&App{root: app}).reserveForward()
	require.NoError(t, err)
	t.Cleanup(first.release)
	t.Cleanup(second.release)
	_, err = app.reserveForward()
	require.ErrorIs(t, err, errForwardCapacity)
	require.True(t, first.start())
	require.False(t, first.start(), "duplicate callback cannot start another worker")
	require.Equal(t, uint32(1), first.state.Load(), "duplicate callback is not expiry")
	first.expireReservation() // A timer racing a running worker cannot release it.
	_, err = app.reserveForward()
	require.ErrorIs(t, err, errForwardCapacity)
	second.expireReservation() // Outer rollback/abandoned commit callback.
	require.False(t, second.start(), "expired reservation cannot dispatch")
	third, err := app.reserveForward()
	require.NoError(t, err)
	t.Cleanup(third.release)
	first.release()
	first.release()
	third.release()
	require.Empty(t, app.forwardAdmission.slots)
}

func TestForwardAdmissionStartExpiryRaceNeverOverReleases(t *testing.T) {
	for i := 0; i < 100; i++ {
		app := &App{}
		lease, err := app.reserveForward()
		require.NoError(t, err)
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); lease.start() }()
		go func() { defer wg.Done(); lease.expireReservation() }()
		wg.Wait()
		lease.release()
		require.Empty(t, app.forwardAdmission.slots)
	}
}

type forwardFaultStore struct {
	listErr error
	gets    int
	limited int
	size    int64
	key     string
}

func (*forwardFaultStore) Put(context.Context, string, []byte, string) error { return nil }
func (*forwardFaultStore) Delete(context.Context, string) error              { return nil }
func (s *forwardFaultStore) Get(context.Context, string) ([]byte, string, error) {
	s.gets++
	return nil, "", errors.New("unbounded read forbidden")
}
func (s *forwardFaultStore) GetLimited(context.Context, string, int64) ([]byte, string, error) {
	s.limited++
	return nil, "", storage.ErrObjectTooLarge
}
func (s *forwardFaultStore) ListPrefix(context.Context, string) ([]storage.ObjectInfo, error) {
	return []storage.ObjectInfo{{Key: s.key, Size: s.size}}, s.listErr
}

func TestForwardSizeLookupFailureNeverDownloads(t *testing.T) {
	for _, cause := range []error{errors.New("AccessDenied private endpoint"), context.DeadlineExceeded} {
		orgID := uuid.New()
		store := &forwardFaultStore{listErr: cause, key: tenantObjectKey(orgID, "messages", "documents", "test.pdf")}
		app := &App{ObjectStore: store, Log: testutil.NopLogger()}
		err := app.prepareForwardedMedia(&OutgoingMessageRequest{
			Account: &models.WhatsAppAccount{}, Type: models.MessageTypeDocument,
			forwardMedia: &forwardMediaCopy{OrganizationID: orgID, SourceKey: store.key},
		})
		require.ErrorContains(t, err, "size could not be checked")
		require.NotContains(t, err.Error(), cause.Error())
		require.Zero(t, store.gets)
		require.Zero(t, store.limited)
	}
}

func TestForwardStaleSizeUsesBoundedReader(t *testing.T) {
	orgID := uuid.New()
	store := &forwardFaultStore{size: 1, key: tenantObjectKey(orgID, "messages", "documents", "test.pdf")}
	app := &App{ObjectStore: store, Log: testutil.NopLogger()}
	err := app.prepareForwardedMedia(&OutgoingMessageRequest{
		Account: &models.WhatsAppAccount{}, Type: models.MessageTypeDocument,
		forwardMedia: &forwardMediaCopy{OrganizationID: orgID, SourceKey: store.key},
	})
	require.ErrorContains(t, err, "larger than WhatsApp allows")
	require.Zero(t, store.gets)
	require.Equal(t, 1, store.limited)
}

func TestForwardLocalReadIsBoundedAndCancellable(t *testing.T) {
	app := &App{Config: &config.Config{Storage: config.StorageConfig{LocalPath: t.TempDir()}}, Log: testutil.NopLogger()}
	key, err := app.saveTenantMedia(context.Background(), uuid.New(), "messages", "messages", "test.txt", []byte("oversized"), "text/plain")
	require.NoError(t, err)
	_, _, err = app.loadTenantMediaWithLimit(context.Background(), uuid.New(), key, 4)
	require.ErrorIs(t, err, storage.ErrObjectTooLarge)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err = app.loadTenantMediaWithLimit(ctx, uuid.New(), key, 20)
	require.ErrorIs(t, err, context.Canceled)
}

func TestCopilotForwardContextKeepsDeliveryUncertainty(t *testing.T) {
	cases := []struct {
		status          models.MessageStatus
		dispatch, label string
	}{
		{models.MessageStatusPending, forwardDispatchQueued, "queued; not yet confirmed sent"},
		{models.MessageStatusPending, forwardDispatchProvider, "unconfirmed; may not have been sent"},
		{models.MessageStatusFailed, forwardDispatchProvider, "failed; do not assume delivery"},
		{models.MessageStatusSent, forwardDispatchProvider, "sent; delivery not confirmed"},
		{models.MessageStatusDelivered, forwardDispatchProvider, "Business [delivered]"},
		{models.MessageStatusRead, forwardDispatchProvider, "Business [read]"},
	}
	for _, tc := range cases {
		t.Run(string(tc.status)+tc.dispatch, func(t *testing.T) {
			message := models.Message{Direction: models.DirectionOutgoing, Status: tc.status, Content: "Synthetic offered appointment", Metadata: models.JSONB{forwardDispatchMetadataKey: tc.dispatch, forwardedFromMetadataKey: map[string]any{"message_id": uuid.New().String(), "contact_id": uuid.New().String()}}}
			got := buildCopilotContext(&models.Contact{}, []models.Message{message}, nil)
			require.Contains(t, got, tc.label)
			require.NotContains(t, got, "Business: Synthetic")
			require.Equal(t, tc.status, message.Status, "projection never repairs stored status")
		})
	}
}

// Real PostgreSQL fixtures: capacity is refused before pending insertion,
// audit, preview changes or provider work. This is not a query-string test.
func TestForwardCapacityRefusalHasNoDatabaseOrProviderEffect(t *testing.T) {
	w := laneENewForwardWorld(t)
	for i := 0; i < forwardMaxConcurrent; i++ {
		lease, err := w.app.reserveForward()
		require.NoError(t, err)
		require.True(t, lease.start())
		t.Cleanup(lease.release)
	}
	var before int64
	require.NoError(t, w.app.DB.Model(&models.Message{}).Where("organization_id = ?", w.org.ID).Count(&before).Error)
	var auditBefore int64
	require.NoError(t, w.app.DB.Model(&models.AuditLog{}).Where("organization_id = ?", w.org.ID).Count(&auditBefore).Error)
	var contactBefore models.Contact
	require.NoError(t, w.app.DB.First(&contactBefore, "id = ?", w.target.ID).Error)
	status, body := w.post(t, w.app, false, nil)
	require.Equal(t, fasthttp.StatusServiceUnavailable, status)
	require.Contains(t, body, string(forwardErrorTemporarilyUnavailable))
	var after int64
	require.NoError(t, w.app.DB.Model(&models.Message{}).Where("organization_id = ?", w.org.ID).Count(&after).Error)
	require.Equal(t, before, after)
	var auditAfter int64
	require.NoError(t, w.app.DB.Model(&models.AuditLog{}).Where("organization_id = ?", w.org.ID).Count(&auditAfter).Error)
	require.Equal(t, auditBefore, auditAfter)
	var contactAfter models.Contact
	require.NoError(t, w.app.DB.First(&contactAfter, "id = ?", w.target.ID).Error)
	require.Equal(t, contactBefore.LastMessageAt, contactAfter.LastMessageAt)
	require.Equal(t, contactBefore.LastMessagePreview, contactAfter.LastMessagePreview)
	w.capture.mu.Lock()
	defer w.capture.mu.Unlock()
	require.Empty(t, w.capture.messages)
}

func TestForwardAdmissionRemainsHeldThroughProviderCompletion(t *testing.T) {
	w := laneENewForwardWorld(t)
	started, finish := make(chan struct{}), make(chan struct{})
	var finishOnce sync.Once
	t.Cleanup(func() { finishOnce.Do(func() { close(finish) }) })
	_, first := w.commitForward(t, func(ctx context.Context, _ *models.Contact) (string, error) {
		close(started)
		select {
		case <-finish:
			return "wamid.synthetic", nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	})
	_, second := w.commitForward(t, func(context.Context, *models.Contact) (string, error) { return "wamid.synthetic.second", nil })
	for _, callback := range first {
		callback()
	}
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("provider did not start")
	}
	_, err := w.app.reserveForward()
	require.ErrorIs(t, err, errForwardCapacity)
	finishOnce.Do(func() { close(finish) })
	done := make(chan struct{})
	go func() { w.app.WaitForBackgroundTasks(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("provider did not release worker")
	}
	lease, err := w.app.reserveForward()
	require.NoError(t, err)
	lease.release()
	laneERun(w.app, second)
	require.Empty(t, w.app.forwardAdmission.slots)
}

func TestForwardBroadcastCarriesOnlyBooleanProvenanceToOtherAgent(t *testing.T) {
	w := laneENewForwardWorld(t)
	hub := appwebsocket.NewHub(testutil.NopLogger())
	go hub.Run()
	client := appwebsocket.NewClient(hub, nil, uuid.New(), w.org.ID)
	otherTenant := appwebsocket.NewClient(hub, nil, uuid.New(), uuid.New())
	hub.Register(client)
	hub.Register(otherTenant)
	require.Eventually(t, func() bool { return hub.GetClientCount() == 2 }, 2*time.Second, 10*time.Millisecond)
	w.app.WSHub = hub
	msg := &models.Message{BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: w.org.ID, ContactID: w.target.ID, Direction: models.DirectionOutgoing, Status: models.MessageStatusPending, Metadata: models.JSONB{forwardedFromMetadataKey: (&messageForwardSource{MessageID: w.source.ID, ContactID: w.source.ContactID}).metadata()}}
	w.app.broadcastNewMessageInCurrentScope(w.org.ID, msg, w.target)
	select {
	case data := <-client.SendChan():
		var envelope struct {
			Type    string         `json:"type"`
			Payload map[string]any `json:"payload"`
		}
		require.NoError(t, json.Unmarshal(data, &envelope))
		require.Equal(t, appwebsocket.TypeNewMessage, envelope.Type)
		require.Equal(t, true, envelope.Payload["is_forwarded"])
		require.NotContains(t, envelope.Payload, "metadata")
		require.NotContains(t, string(data), w.source.ID.String())
	case <-time.After(2 * time.Second):
		t.Fatal("second agent did not receive forwarded message")
	}
	select {
	case <-otherTenant.SendChan():
		t.Fatal("another tenant received the message")
	default:
	}
}

func TestForwardExpiredAfterCommitCallbackSettlesWithoutProvider(t *testing.T) {
	w := laneENewForwardWorld(t)
	var providerCalls atomic.Int32
	msg, callbacks := w.commitForward(t, func(context.Context, *models.Contact) (string, error) {
		providerCalls.Add(1)
		return "wamid.must-not-send", nil
	})
	// Exercise the real timer on the lease captured by SendOutgoingMessage,
	// rather than only the admission primitive. The pending row is durable,
	// but its outer-commit callback has not been allowed to start work yet.
	require.Eventually(t, func() bool { return len(w.app.forwardAdmission.slots) == 0 }, forwardRequestTimeout+5*time.Second, 25*time.Millisecond)
	laneERun(w.app, callbacks)
	require.Zero(t, providerCalls.Load())
	stored := laneELoadMessage(t, w.app, msg.ID)
	require.Equal(t, models.MessageStatusFailed, stored.Status)
	require.Equal(t, forwardServerBusyError().Error(), stored.ErrorMessage)
	require.Equal(t, forwardDispatchQueued, stored.Metadata[forwardDispatchMetadataKey])
	require.Empty(t, stored.WhatsAppMessageID)
	require.Empty(t, w.app.forwardAdmission.slots)
}

func TestOrdinaryAsyncSendDoesNotUseOrReleaseForwardLease(t *testing.T) {
	w := laneENewForwardWorld(t)
	for i := 0; i < forwardMaxConcurrent; i++ {
		lease, err := w.app.reserveForward()
		require.NoError(t, err)
		require.True(t, lease.start())
		t.Cleanup(lease.release)
	}
	account := *w.account
	account.Status = whatsappaccount.StatusActive
	opts := DefaultSendOptions()
	opts.SentByUserID = &w.user.ID
	var providerCalls atomic.Int32
	msg, err := w.app.SendOutgoingMessage(context.Background(), OutgoingMessageRequest{
		Account: &account, Contact: w.target, Type: models.MessageTypeText, Content: "Synthetic ordinary async send",
		deliveryOverride: func(context.Context, *models.Contact) (string, error) {
			providerCalls.Add(1)
			return "wamid.ordinary", nil
		},
	}, opts)
	require.NoError(t, err)
	done := make(chan struct{})
	go func() { w.app.WaitForBackgroundTasks(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("ordinary async send did not finish")
	}
	require.Equal(t, int32(1), providerCalls.Load())
	require.Equal(t, models.MessageStatusSent, laneELoadMessage(t, w.app, msg.ID).Status)
	require.Len(t, w.app.forwardAdmission.slots, forwardMaxConcurrent, "nil lease cannot release another forward's reservation")
}

func TestSLAWarningPendingForwardDoesNotCountUntilConfirmed(t *testing.T) {
	app, org, createTransfer, _ := slaAfterCommitFixture(t)
	escalated := time.Now().UTC().Add(-time.Minute)
	transfer := createTransfer(models.SLATracking{EscalationLevel: 1, EscalatedAt: &escalated})
	user := testutil.CreateTestUser(t, app.DB, org.ID)
	msg := models.Message{BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: org.ID, ContactID: transfer.ContactID, WhatsAppAccount: transfer.WhatsAppAccount, Direction: models.DirectionOutgoing, MessageType: models.MessageTypeText, Content: "Synthetic forward", Status: models.MessageStatusPending, SentByUserID: &user.ID, Metadata: models.JSONB{forwardDispatchMetadataKey: forwardDispatchProvider}}
	require.NoError(t, app.DB.Create(&msg).Error)
	for _, status := range []models.MessageStatus{models.MessageStatusPending, models.MessageStatusFailed, models.MessageStatusSent, models.MessageStatusDelivered, models.MessageStatusRead} {
		require.NoError(t, app.DB.Model(&msg).Update("status", status).Error)
		current, err := slaWarningNoticeStillCurrent(app.DB, *transfer)
		require.NoError(t, err)
		require.Equal(t, status == models.MessageStatusPending || status == models.MessageStatusFailed, current, string(status))
	}
}

// Ensure context labels do not interpolate provider metadata or error text.
func TestCopilotContextDoesNotExposeForwardErrorMetadata(t *testing.T) {
	msg := models.Message{Direction: models.DirectionOutgoing, Status: models.MessageStatusFailed, ErrorMessage: "private-provider-url", Metadata: models.JSONB{"raw": "secret"}}
	got := buildCopilotContext(&models.Contact{}, []models.Message{msg}, nil)
	require.False(t, strings.Contains(got, "private-provider-url") || strings.Contains(got, "secret"))
}
