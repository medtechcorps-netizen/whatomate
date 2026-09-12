package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/config"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/internal/storage"
	"github.com/shridarpatil/whatomate/pkg/whatsapp"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func coexistenceTombstoneTestEvents(t *testing.T, app *App, account models.WhatsAppAccount, message models.Message) (CoexistenceMessage, CoexistenceMessage, CoexistenceMessage) {
	t.Helper()
	require.Equal(t, account.OrganizationID, message.OrganizationID)
	require.Equal(t, models.DirectionOutgoing, message.Direction)
	require.Equal(t, uuid.NewSHA1(account.ID, []byte("coexistence-message:"+message.WhatsAppMessageID)), message.ID)
	var contact models.Contact
	require.NoError(t, app.DB.Where("id = ? AND organization_id = ?", message.ContactID, account.OrganizationID).First(&contact).Error)
	var revoke, edit, detail CoexistenceMessage
	require.NoError(t, json.Unmarshal([]byte(`{"id":"revoke-terminal","type":"revoke","revoke":{"original_message_id":"original"}}`), &revoke))
	require.NoError(t, json.Unmarshal([]byte(`{"id":"edit-delayed","type":"edit","edit":{"original_message_id":"original","message":{"type":"image","image":{"id":"delayed-media","mime_type":"image/jpeg","caption":"private content"}}}}`), &edit))
	require.NoError(t, json.Unmarshal([]byte(`{"type":"image","image":{"id":"delayed-media","mime_type":"image/jpeg","caption":"private content"}}`), &detail))
	revoke.Revoke.OriginalMessageID = message.WhatsAppMessageID
	edit.Edit.OriginalMessageID = message.WhatsAppMessageID
	detail.ID = message.WhatsAppMessageID
	for _, event := range []*CoexistenceMessage{&revoke, &edit, &detail} {
		event.From = "15550783881"
		event.To = contact.PhoneNumber
		event.ToUserID = contact.BSUID
	}
	return revoke, edit, detail
}

func assertCoexistenceTerminalTombstone(t *testing.T, app *App, message models.Message) {
	t.Helper()
	var stored models.Message
	require.NoError(t, app.DB.Where("id = ? AND organization_id = ?", message.ID, message.OrganizationID).First(&stored).Error)
	assert.Equal(t, "[Message deleted from WhatsApp Business App]", stored.Content)
	assert.Equal(t, models.MessageTypeText, stored.MessageType)
	assert.Equal(t, true, stored.Metadata[coexistenceMediaRevokedMetadataKey])
	assert.Equal(t, "revoke-terminal", stored.Metadata["coexistence_revoke_event_id"])
	assert.Empty(t, stored.MediaURL)
	assert.Empty(t, stored.MediaMimeType)
	assert.Empty(t, stored.MediaFilename)
	assert.Empty(t, stored.Metadata[coexistenceMediaProviderIDMetadataKey])
	assert.Empty(t, stored.Metadata[coexistenceMediaHydratedIDMetadataKey])
	assert.Empty(t, stored.Metadata["coexistence_media_sha256"])
	var jobs int64
	require.NoError(t, app.DB.Model(&models.ScheduledJob{}).Where(
		"organization_id = ? AND kind = ? AND aggregate_id = ?",
		message.OrganizationID, coexistenceMediaHydrationJobKind, message.ID,
	).Count(&jobs).Error)
	assert.Zero(t, jobs, "delayed content must not schedule revoked media hydration")
}

func TestCoexistenceRevokedMessageRejectsDelayedMutations(t *testing.T) {
	app, account, message, provider := newCoexistenceMediaFixture(t)
	revoke, edit, detail := coexistenceTombstoneTestEvents(t, app, account, message)
	require.NoError(t, app.WithCommittedTenantApp(account.OrganizationID, func(scoped *App) error {
		return scoped.persistCoexistenceRevoke(&account, revoke, nil, message.Direction, message.Status, "smb_message_echoes", false)
	}))
	mutations := []struct {
		name string
		run  func(*App) error
	}{
		{"edit", func(scoped *App) error {
			return scoped.persistCoexistenceEdit(&account, edit, nil, message.Direction, message.Status, "smb_message_echoes", false)
		}},
		{"history_detail", func(scoped *App) error {
			return scoped.persistHistoryMediaDetail(&account, "", detail, nil)
		}},
		{"stale_replay", func(scoped *App) error {
			stale := message
			return scoped.mergeCoexistenceMessageDetails(&account, &stale, detail, models.MessageStatusRead, models.JSONB{"coexistence_source": "history"})
		}},
	}
	for _, mutation := range mutations {
		t.Run(mutation.name, func(t *testing.T) {
			require.NoError(t, app.WithCommittedTenantApp(account.OrganizationID, mutation.run))
			assertCoexistenceTerminalTombstone(t, app, message)
		})
	}
	assert.Zero(t, provider.metadataCalls.Load())
	assert.Zero(t, provider.downloadCalls.Load())
}

func TestSoftDeletedCoexistenceOwnerIsImmutableReplayNoOp(t *testing.T) {
	for _, mutation := range []string{"echo", "edit", "revoke", "history_detail"} {
		t.Run(mutation, func(t *testing.T) {
			app, account, message, provider := newCoexistenceMediaFixture(t)
			revoke, edit, detail := coexistenceTombstoneTestEvents(t, app, account, message)
			require.NoError(t, app.DB.Delete(&models.Message{}, "organization_id = ? AND id = ?", account.OrganizationID, message.ID).Error)
			var before models.Message
			require.NoError(t, app.DB.Unscoped().First(&before, "organization_id = ? AND id = ?", account.OrganizationID, message.ID).Error)
			require.True(t, before.DeletedAt.Valid)
			beforeJSON, err := json.Marshal(before)
			require.NoError(t, err)
			var contactsBefore int64
			require.NoError(t, app.DB.Unscoped().Model(&models.Contact{}).Where("organization_id = ?", account.OrganizationID).Count(&contactsBefore).Error)

			require.NoError(t, app.WithCommittedTenantApp(account.OrganizationID, func(scoped *App) error {
				switch mutation {
				case "echo":
					persisted, created, err := scoped.persistCoexistenceMessage(
						&account,
						coexistenceContactIdentity{Phone: detail.To, UserID: detail.ToUserID},
						detail,
						message.Direction,
						message.Status,
						models.JSONB{"coexistence_source": "smb_message_echoes"},
						false,
					)
					require.Nil(t, persisted)
					require.False(t, created)
					return err
				case "edit":
					return scoped.persistCoexistenceEdit(&account, edit, nil, message.Direction, message.Status, "smb_message_echoes", false)
				case "revoke":
					return scoped.persistCoexistenceRevoke(&account, revoke, nil, message.Direction, message.Status, "smb_message_echoes", false)
				default:
					return scoped.persistHistoryMediaDetail(&account, "", detail, nil)
				}
			}))

			var after models.Message
			require.NoError(t, app.DB.Unscoped().First(&after, "organization_id = ? AND id = ?", account.OrganizationID, message.ID).Error)
			afterJSON, err := json.Marshal(after)
			require.NoError(t, err)
			assert.JSONEq(t, string(beforeJSON), string(afterJSON), "a replay or mutation must not revive or alter a soft-deleted WAMID owner")
			var contactsAfter, messages int64
			require.NoError(t, app.DB.Unscoped().Model(&models.Contact{}).Where("organization_id = ?", account.OrganizationID).Count(&contactsAfter).Error)
			require.NoError(t, app.DB.Unscoped().Model(&models.Message{}).Where("organization_id = ? AND whats_app_message_id = ?", account.OrganizationID, message.WhatsAppMessageID).Count(&messages).Error)
			assert.Equal(t, contactsBefore, contactsAfter)
			assert.EqualValues(t, 1, messages)
			assert.Zero(t, provider.metadataCalls.Load())
			assert.Zero(t, provider.downloadCalls.Load())
		})
	}
}

func TestCoexistenceRevokeRacingFirstDeliverySanitizesInsertedMessage(t *testing.T) {
	app, account, message, _ := newCoexistenceMediaFixtureWithPersistence(t, false)
	var contact models.Contact
	require.NoError(t, app.DB.Where("id = ?", message.ContactID).First(&contact).Error)
	revoke, _, _ := coexistenceTombstoneTestEvents(t, app, account, message)
	revoke.To = contact.PhoneNumber
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	app.DB = app.DB.WithContext(ctx)
	deliveryWritten := make(chan struct{})
	releaseDelivery := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseDelivery) }) }
	defer release()
	deliveryResult := make(chan error, 1)
	go func() {
		deliveryResult <- app.WithCommittedTenantApp(account.OrganizationID, func(scoped *App) error {
			if err := scoped.DB.Create(&message).Error; err != nil {
				return err
			}
			close(deliveryWritten)
			select {
			case <-releaseDelivery:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
	}()
	select {
	case <-deliveryWritten:
	case err := <-deliveryResult:
		t.Fatalf("first delivery failed before its commit: %v", err)
	case <-ctx.Done():
		t.Fatal("first delivery did not reach its commit barrier")
	}
	revokePID := make(chan int, 1)
	revokeResult := make(chan error, 1)
	go func() {
		revokeResult <- app.WithCommittedTenantApp(account.OrganizationID, func(scoped *App) error {
			var pid int
			if err := scoped.DB.Raw("SELECT pg_backend_pid()").Scan(&pid).Error; err != nil {
				return err
			}
			revokePID <- pid
			return scoped.persistCoexistenceRevoke(&account, revoke, nil, message.Direction, message.Status, "smb_message_echoes", false)
		})
	}()
	var pid int
	select {
	case pid = <-revokePID:
	case err := <-revokeResult:
		t.Fatalf("revoke failed before first-delivery lookup: %v", err)
	case <-ctx.Done():
		t.Fatal("revoke did not start")
	}
	// Neither lookup can see the uncommitted first delivery. Wait until the
	// tombstone insert/contact resolution contends with that delivery, then
	// let the existing message win the insert conflict and require sanitization.
	require.Eventually(t, func() bool {
		var blocked bool
		err := app.DB.Raw("SELECT cardinality(pg_blocking_pids(?)) > 0", pid).Scan(&blocked).Error
		return err == nil && blocked
	}, 5*time.Second, 10*time.Millisecond)
	release()
	for _, result := range []chan error{deliveryResult, revokeResult} {
		select {
		case err := <-result:
			require.NoError(t, err)
		case <-ctx.Done():
			t.Fatal("first delivery and revoke did not complete")
		}
	}
	assertCoexistenceTerminalTombstone(t, app, message)
}

func TestCoexistenceConcurrentRevokeWinsOverDelayedMutations(t *testing.T) {
	for _, mutation := range []string{"edit", "history_detail", "stale_replay"} {
		t.Run(mutation, func(t *testing.T) {
			app, account, message, _ := newCoexistenceMediaFixture(t)
			revoke, edit, detail := coexistenceTombstoneTestEvents(t, app, account, message)
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			app.DB = app.DB.WithContext(ctx)
			revokeWritten := make(chan struct{})
			releaseRevoke := make(chan struct{})
			var releaseOnce sync.Once
			release := func() { releaseOnce.Do(func() { close(releaseRevoke) }) }
			defer release()
			revokeResult := make(chan error, 1)
			go func() {
				revokeResult <- app.WithCommittedTenantApp(account.OrganizationID, func(scoped *App) error {
					if err := scoped.persistCoexistenceRevoke(&account, revoke, nil, message.Direction, message.Status, "smb_message_echoes", false); err != nil {
						return err
					}
					close(revokeWritten)
					select {
					case <-releaseRevoke:
						return nil
					case <-ctx.Done():
						return ctx.Err()
					}
				})
			}()
			select {
			case <-revokeWritten:
			case err := <-revokeResult:
				t.Fatalf("revoke failed before holding its message lock: %v", err)
			case <-ctx.Done():
				t.Fatal("revoke did not acquire its message lock")
			}

			mutationPID := make(chan int, 1)
			mutationResult := make(chan error, 1)
			go func() {
				mutationResult <- app.WithCommittedTenantApp(account.OrganizationID, func(scoped *App) error {
					var pid int
					if err := scoped.DB.Raw("SELECT pg_backend_pid()").Scan(&pid).Error; err != nil {
						return err
					}
					mutationPID <- pid
					switch mutation {
					case "edit":
						return scoped.persistCoexistenceEdit(&account, edit, nil, message.Direction, message.Status, "smb_message_echoes", false)
					case "history_detail":
						return scoped.persistHistoryMediaDetail(&account, "", detail, nil)
					default:
						stale := message
						return scoped.mergeCoexistenceMessageDetails(&account, &stale, detail, models.MessageStatusRead, models.JSONB{"coexistence_source": "history"})
					}
				})
			}()
			var pid int
			select {
			case pid = <-mutationPID:
			case err := <-mutationResult:
				t.Fatalf("delayed mutation failed before reading its message: %v", err)
			case <-ctx.Done():
				t.Fatal("delayed mutation did not start")
			}
			// Observe a real database lock wait, not a timing assumption. Before
			// the fix, the stale UPDATE waits here and erases the revoke at commit;
			// now the SELECT waits and then sees the committed terminal metadata.
			require.Eventually(t, func() bool {
				var blocked bool
				err := app.DB.Raw("SELECT cardinality(pg_blocking_pids(?)) > 0", pid).Scan(&blocked).Error
				return err == nil && blocked
			}, 5*time.Second, 10*time.Millisecond, "delayed mutation must reach the locked message")
			release()
			for _, result := range []chan error{revokeResult, mutationResult} {
				select {
				case err := <-result:
					require.NoError(t, err)
				case <-ctx.Done():
					t.Fatal("concurrent message mutations did not complete")
				}
			}
			assertCoexistenceTerminalTombstone(t, app, message)
		})
	}
}

type coexistenceMediaServer struct {
	baseURL       string
	token         string
	failMetadata  atomic.Int32
	metadataCalls atomic.Int32
	downloadCalls atomic.Int32
}

type blockingCoexistenceMediaStore struct {
	putStarted  chan struct{}
	releasePut  chan struct{}
	putOnce     sync.Once
	releaseOnce sync.Once

	mu             sync.Mutex
	objects        map[string][]byte
	deleteCalls    int
	deleteFailures int
}

var _ storage.ObjectStore = (*blockingCoexistenceMediaStore)(nil)

func newBlockingCoexistenceMediaStore() *blockingCoexistenceMediaStore {
	return &blockingCoexistenceMediaStore{
		putStarted: make(chan struct{}),
		releasePut: make(chan struct{}),
		objects:    make(map[string][]byte),
	}
}

func (store *blockingCoexistenceMediaStore) Put(
	ctx context.Context,
	key string,
	data []byte,
	_ string,
) error {
	store.putOnce.Do(func() { close(store.putStarted) })
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-store.releasePut:
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	store.objects[key] = append([]byte(nil), data...)
	return nil
}

func (store *blockingCoexistenceMediaStore) Get(
	_ context.Context,
	key string,
) ([]byte, string, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	data, ok := store.objects[key]
	if !ok {
		return nil, "", storage.ErrObjectNotFound
	}
	return append([]byte(nil), data...), "image/jpeg", nil
}

func (store *blockingCoexistenceMediaStore) Delete(_ context.Context, key string) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.deleteCalls++
	if store.deleteFailures > 0 {
		store.deleteFailures--
		return errors.New("temporary object deletion failure")
	}
	delete(store.objects, key)
	return nil
}

func (store *blockingCoexistenceMediaStore) ListPrefix(
	_ context.Context,
	prefix string,
) ([]storage.ObjectInfo, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	var objects []storage.ObjectInfo
	for key, data := range store.objects {
		if strings.HasPrefix(key, prefix) {
			objects = append(objects, storage.ObjectInfo{Key: key, Size: int64(len(data))})
		}
	}
	return objects, nil
}

func (store *blockingCoexistenceMediaStore) counts() (objects, deletes int) {
	store.mu.Lock()
	defer store.mu.Unlock()
	return len(store.objects), store.deleteCalls
}

func (store *blockingCoexistenceMediaStore) release() {
	store.releaseOnce.Do(func() { close(store.releasePut) })
}

func (store *blockingCoexistenceMediaStore) failNextDeletes(count int) {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.deleteFailures = count
}

func (server *coexistenceMediaServer) handler(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer "+server.token {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/v21.0/") {
		server.metadataCalls.Add(1)
		if server.failMetadata.Load() > 0 {
			server.failMetadata.Add(-1)
			http.Error(w, "temporary Meta failure", http.StatusInternalServerError)
			return
		}
		mediaID := strings.TrimPrefix(r.URL.Path, "/v21.0/")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"url":               server.baseURL + "/cdn/" + mediaID,
			"mime_type":         "image/jpeg",
			"messaging_product": "whatsapp",
		})
		return
	}
	if strings.HasPrefix(r.URL.Path, "/cdn/") {
		server.downloadCalls.Add(1)
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write([]byte("coexistence-media-bytes"))
		return
	}
	http.NotFound(w, r)
}

func newCoexistenceMediaFixture(
	t *testing.T,
) (*App, models.WhatsAppAccount, models.Message, *coexistenceMediaServer) {
	return newCoexistenceMediaFixtureWithPersistence(t, true)
}

func newCoexistenceMediaFixtureWithPersistence(
	t *testing.T,
	persistMessage bool,
) (*App, models.WhatsAppAccount, models.Message, *coexistenceMediaServer) {
	t.Helper()
	db := testutil.SetupTestDB(t)
	organization := testutil.CreateTestOrganization(t, db)
	token := "coexistence-media-secret-" + uuid.NewString()
	provider := &coexistenceMediaServer{token: token}
	httpServer := httptest.NewServer(http.HandlerFunc(provider.handler))
	provider.baseURL = httpServer.URL
	t.Cleanup(httpServer.Close)

	account := models.WhatsAppAccount{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: organization.ID,
		Name:           "Coexistence Media " + uuid.NewString()[:8],
		PhoneID:        testutil.NewTestGraphObjectID(),
		BusinessID:     testutil.NewTestGraphObjectID(),
		AccessToken:    token,
		AppSecret:      "coexistence-app-secret-" + uuid.NewString(),
		APIVersion:     "v21.0",
		Status:         "active",
		IsSMB:          true,
	}
	require.NoError(t, db.Create(&account).Error)
	contact := testutil.CreateTestContact(t, db, organization.ID)
	mediaID := "media-" + uuid.NewString()
	wamid := "wamid." + uuid.NewString()
	message := models.Message{
		BaseModel:         models.BaseModel{ID: uuid.NewSHA1(account.ID, []byte("coexistence-message:"+wamid))},
		OrganizationID:    organization.ID,
		WhatsAppAccount:   account.Name,
		ContactID:         contact.ID,
		WhatsAppMessageID: wamid,
		Direction:         models.DirectionOutgoing,
		MessageType:       models.MessageTypeImage,
		Status:            models.MessageStatusSent,
		MediaMimeType:     "image/jpeg",
		Metadata: models.JSONB{
			coexistenceMediaProviderIDMetadataKey: mediaID,
			"coexistence_source":                  "smb_message_echoes",
		},
	}
	if persistMessage {
		require.NoError(t, db.Create(&message).Error)
	}

	app := &App{
		Config: &config.Config{
			Storage: config.StorageConfig{LocalPath: t.TempDir()},
		},
		DB:       db,
		Log:      testutil.NopLogger(),
		WhatsApp: whatsapp.NewWithBaseURL(testutil.NopLogger(), httpServer.URL),
	}
	return app, account, message, provider
}

func stagedIdentityReviewImageMessage(t *testing.T, wamid, mediaID string) IncomingTextMessage {
	t.Helper()
	var message IncomingTextMessage
	require.NoError(t, json.Unmarshal([]byte(fmt.Sprintf(
		`{"from_user_id":"US.unowned-%s","id":%q,"timestamp":"1739230980","type":"image","image":{"id":%q,"mime_type":"image/jpeg","sha256":%q,"caption":"held media"}}`,
		uuid.NewString(),
		wamid,
		mediaID,
		strings.Repeat("a", 64),
	)), &message))
	return message
}

func createStagedIdentityReviewMediaFixture(
	t *testing.T,
) (*App, models.WhatsAppAccount, models.InboundEvent, *coexistenceMediaServer) {
	t.Helper()
	app, account, _, provider := newCoexistenceMediaFixtureWithPersistence(t, false)
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
	message := stagedIdentityReviewImageMessage(
		t,
		"wamid.staged-media-"+uuid.NewString(),
		"staged-media-"+uuid.NewString(),
	)
	work, duplicate, err := app.persistAuthenticatedIncomingMessageBeforeAck(
		account.PhoneID,
		message,
		"Untrusted staged profile",
		strings.Repeat("a", 64),
	)
	require.NoError(t, err)
	assert.False(t, duplicate)
	assert.Nil(t, work)
	var event models.InboundEvent
	require.NoError(t, app.DB.Where(
		"organization_id = ? AND protocol = ? AND provider_event_id = ?",
		account.OrganizationID,
		models.WhatsAppIdentityReviewInboundProtocol,
		message.ID,
	).First(&event).Error)
	return app, account, event, provider
}

func TestCoexistenceStagedMediaHydratesWithoutContactOrMessage(t *testing.T) {
	app, account, event, provider := createStagedIdentityReviewMediaFixture(t)
	var contactsBefore, messagesBefore int64
	require.NoError(t, app.DB.Model(&models.Contact{}).Where(
		"organization_id = ?",
		account.OrganizationID,
	).Count(&contactsBefore).Error)
	require.NoError(t, app.DB.Model(&models.Message{}).Where(
		"organization_id = ?",
		account.OrganizationID,
	).Count(&messagesBefore).Error)

	processor := NewCoexistenceMediaProcessor(app, time.Hour)
	require.NoError(t, processor.ProcessStagedEvent(
		context.Background(),
		account.OrganizationID,
		event.ID,
	))

	var stored models.InboundEvent
	require.NoError(t, app.DB.First(&stored, event.ID).Error)
	assert.Equal(t, "ready", coexistenceMediaPayloadString(stored.Payload, "media_status"))
	assert.NotEmpty(t, coexistenceMediaPayloadString(stored.Payload, "media_url"))
	assert.Equal(
		t,
		coexistenceMediaPayloadString(stored.Payload, "media_id"),
		coexistenceMediaPayloadString(stored.Payload, "media_hydrated_id"),
	)
	var contactsAfter, messagesAfter int64
	require.NoError(t, app.DB.Model(&models.Contact{}).Where(
		"organization_id = ?",
		account.OrganizationID,
	).Count(&contactsAfter).Error)
	require.NoError(t, app.DB.Model(&models.Message{}).Where(
		"organization_id = ?",
		account.OrganizationID,
	).Count(&messagesAfter).Error)
	assert.Equal(t, contactsBefore, contactsAfter)
	assert.Equal(t, messagesBefore, messagesAfter)
	assert.EqualValues(t, 1, provider.metadataCalls.Load())
	assert.EqualValues(t, 1, provider.downloadCalls.Load())

	var job models.ScheduledJob
	require.NoError(t, app.DB.Where(
		"organization_id = ? AND kind = ? AND aggregate_type = ? AND aggregate_id = ?",
		account.OrganizationID,
		coexistenceMediaHydrationJobKind,
		coexistenceMediaReviewAggregate,
		event.ID,
	).First(&job).Error)
	assert.Equal(t, models.ScheduledJobStatusCompleted, job.Status)
}

func TestCoexistenceStagedMediaTerminalJobIsNotRevivedByReplay(t *testing.T) {
	app, account, event, _ := createStagedIdentityReviewMediaFixture(t)
	beforePayload, err := json.Marshal(event.Payload)
	require.NoError(t, err)

	var first models.ScheduledJob
	require.NoError(t, app.DB.Where(
		"organization_id = ? AND kind = ? AND aggregate_type = ? AND aggregate_id = ?",
		account.OrganizationID,
		coexistenceMediaHydrationJobKind,
		coexistenceMediaReviewAggregate,
		event.ID,
	).First(&first).Error)
	require.NoError(t, app.DB.Model(&models.ScheduledJob{}).Where(
		"id = ? AND organization_id = ?",
		first.ID,
		account.OrganizationID,
	).Update("status", models.ScheduledJobStatusCompleted).Error)
	for range 3 {
		require.NoError(t, app.WithCommittedTenantApp(account.OrganizationID, func(scoped *App) error {
			return scoped.ensureCoexistenceStagedMediaJob(&account, event.ID)
		}))
	}

	var jobs []models.ScheduledJob
	require.NoError(t, app.DB.Where(
		"organization_id = ? AND kind = ? AND aggregate_type = ? AND aggregate_id = ?",
		account.OrganizationID,
		coexistenceMediaHydrationJobKind,
		coexistenceMediaReviewAggregate,
		event.ID,
	).Order("created_at, id").Find(&jobs).Error)
	require.Len(t, jobs, 1)
	assert.Equal(t, first.ID, jobs[0].ID)
	assert.Equal(t, first.IdempotencyKey, jobs[0].IdempotencyKey)
	assert.Equal(t, models.ScheduledJobStatusCompleted, jobs[0].Status)
	generation, ok := coexistenceMediaPayloadUint64(jobs[0].Payload, "media_generation")
	require.True(t, ok)
	assert.EqualValues(t, 1, generation)

	var stored models.InboundEvent
	require.NoError(t, app.DB.Where(
		"id = ? AND organization_id = ?",
		event.ID,
		account.OrganizationID,
	).First(&stored).Error)
	afterPayload, err := json.Marshal(stored.Payload)
	require.NoError(t, err)
	assert.JSONEq(t, string(beforePayload), string(afterPayload),
		"an exact replay cannot mutate the reserved event or create another generation")
}

func TestDownloadAndSaveCoexistenceMediaUsesStableObjectKey(t *testing.T) {
	token := "stable-media-token"
	provider := &coexistenceMediaServer{token: token}
	httpServer := httptest.NewServer(http.HandlerFunc(provider.handler))
	provider.baseURL = httpServer.URL
	t.Cleanup(httpServer.Close)

	mediaRoot := t.TempDir()
	app := &App{
		Config: &config.Config{
			Storage: config.StorageConfig{LocalPath: mediaRoot},
		},
		Log:      testutil.NopLogger(),
		WhatsApp: whatsapp.NewWithBaseURL(testutil.NopLogger(), httpServer.URL),
	}
	messageID := uuid.New()
	organizationID := uuid.New()
	account := &whatsapp.Account{
		PhoneID:     testutil.NewTestGraphObjectID(),
		BusinessID:  testutil.NewTestGraphObjectID(),
		APIVersion:  "v21.0",
		AccessToken: token,
	}

	first, err := app.downloadAndSaveCoexistenceMedia(
		context.Background(),
		organizationID,
		messageID,
		"stable-media-id",
		"image/jpeg",
		account,
	)
	require.NoError(t, err)
	second, err := app.downloadAndSaveCoexistenceMedia(
		context.Background(),
		organizationID,
		messageID,
		"stable-media-id",
		"application/pdf",
		account,
	)
	require.NoError(t, err)
	assert.Equal(t, first, second)

	entries, err := os.ReadDir(filepath.Join(mediaRoot, "coexistence"))
	require.NoError(t, err)
	require.Len(t, entries, 1, "a retry must overwrite the same deterministic file")
	assert.Equal(t, filepath.Base(first), entries[0].Name())
	assert.Equal(t, ".bin", filepath.Ext(first))
}

func TestCoexistenceMediaReplacementClearsObsoleteMetadata(t *testing.T) {
	for _, replacement := range []string{
		`{"type":"text","text":{"body":"replacement text"},"image":{"id":"undeclared-stale-image","sha256":"undeclared-sha"}}`,
		`{"type":"image","image":{"id":"replacement-B","mime_type":"image/jpeg"}}`,
	} {
		t.Run(replacement, func(t *testing.T) {
			existing := models.Message{MessageType: models.MessageTypeImage, MediaURL: "old-A.jpg", Metadata: models.JSONB{
				coexistenceMediaProviderIDMetadataKey: "original-A", coexistenceMediaHydratedIDMetadataKey: "original-A", "coexistence_media_sha256": "original-A-sha",
			}}
			var inbound IncomingTextMessage
			require.NoError(t, json.Unmarshal([]byte(replacement), &inbound))
			metadata := cloneMessageMetadata(existing.Metadata)
			extracted := (&App{Log: testutil.NopLogger()}).extractMessageContentForPersistence(inbound)
			updates := coexistenceMediaReplacementUpdates(&existing, inbound, extracted, metadata, true)
			require.Contains(t, updates, "media_url", "replacement must explicitly clear the persisted column")
			assert.Empty(t, updates["media_url"])
			assert.Empty(t, metadata["coexistence_media_sha256"])
			assert.Empty(t, metadata[coexistenceMediaHydratedIDMetadataKey])
			if inbound.Type == "text" {
				assert.Empty(t, metadata[coexistenceMediaProviderIDMetadataKey])
				assert.Empty(t, updates["media_mime_type"])
				assert.Empty(t, updates["media_filename"])
			} else {
				assert.Equal(t, "replacement-B", metadata[coexistenceMediaProviderIDMetadataKey])
				assert.Equal(t, "image/jpeg", updates["media_mime_type"])
			}
		})
	}
}

func TestCoexistenceMediaReplacementComparesWholeStoredRevision(t *testing.T) {
	for _, change := range []string{"content", "new_empty_edit_marker", "new_edit_marker", "edit_marker_id", "mime", "filename", "identical", "unapplied_replay_content"} {
		t.Run(change, func(t *testing.T) {
			existing := models.Message{
				MessageType: models.MessageTypeDocument, Content: "same caption", MediaURL: "hydrated-original.pdf",
				MediaMimeType: "application/pdf", MediaFilename: "original.pdf",
				Metadata: models.JSONB{
					coexistenceMediaProviderIDMetadataKey: "same-media-ID", "coexistence_media_sha256": "same-media-SHA",
					coexistenceMediaHydratedIDMetadataKey: "same-media-ID",
				},
			}
			if change == "edit_marker_id" {
				existing.Metadata["coexistence_edit_event_id"] = "edit-A"
			}
			metadata := cloneMessageMetadata(existing.Metadata)
			var inbound IncomingTextMessage
			require.NoError(t, json.Unmarshal([]byte(`{"type":"document","document":{"id":"same-media-ID","sha256":"same-media-SHA","mime_type":"application/pdf","filename":"original.pdf","caption":"same caption"}}`), &inbound))
			replaceContent := true
			switch change {
			case "content":
				inbound.Document.Caption = "changed caption"
			case "new_empty_edit_marker":
				metadata["coexistence_edit_event_id"] = ""
			case "new_edit_marker", "edit_marker_id":
				metadata["coexistence_edit_event_id"] = "edit-B"
			case "mime":
				inbound.Document.MimeType = "application/octet-stream"
			case "filename":
				inbound.Document.Filename = "renamed.pdf"
			case "unapplied_replay_content":
				inbound.Document.Caption = "ignored historical caption"
				replaceContent = false
			}
			extracted := (&App{Log: testutil.NopLogger()}).extractMessageContentForPersistence(inbound)
			updates := coexistenceMediaReplacementUpdates(&existing, inbound, extracted, metadata, replaceContent)
			_, clearsURL := updates["media_url"]
			wantClear := change != "identical" && change != "unapplied_replay_content"
			assert.Equal(t, wantClear, clearsURL, "hydration follows the actual full committed revision")
			if wantClear {
				assert.Empty(t, updates["media_url"])
				assert.Empty(t, metadata[coexistenceMediaHydratedIDMetadataKey])
			} else {
				assert.Equal(t, "same-media-ID", metadata[coexistenceMediaHydratedIDMetadataKey])
			}
			assert.Equal(t, "same-media-ID", metadata[coexistenceMediaProviderIDMetadataKey])
			assert.Equal(t, "same-media-SHA", metadata["coexistence_media_sha256"])
			assert.Equal(t, "hydrated-original.pdf", existing.MediaURL, "projection must not mutate the input snapshot")
		})
	}
}

func coexistenceMediaReplacementEvent(t *testing.T, app *App, account models.WhatsAppAccount, message models.Message, replacement map[string]any) CoexistenceMessage {
	t.Helper()
	var contact models.Contact
	require.NoError(t, app.DB.First(&contact, message.ContactID).Error)
	encoded, err := json.Marshal(map[string]any{
		"id": "edit-" + uuid.NewString(), "type": "edit", "from": "15550783881", "to": contact.PhoneNumber,
		"edit": map[string]any{"original_message_id": message.WhatsAppMessageID, "message": replacement},
	})
	require.NoError(t, err)
	var event CoexistenceMessage
	require.NoError(t, json.Unmarshal(encoded, &event))
	require.Equal(t, account.OrganizationID, message.OrganizationID)
	return event
}

func TestCoexistenceMediaWorkerMediaToTextFencesQueuedAndInFlightWork(t *testing.T) {
	for _, phase := range []string{"before_claim", "during_upload", "during_upload_cleanup_retry"} {
		t.Run(phase, func(t *testing.T) {
			app, account, message, provider := newCoexistenceMediaFixture(t)
			message.Metadata["coexistence_media_sha256"] = "obsolete-sha"
			message.Metadata[coexistenceMediaHydratedIDMetadataKey] = "obsolete-hydrated-revision"
			message.MediaFilename = "obsolete.jpg"
			require.NoError(t, app.DB.Save(&message).Error)
			require.NoError(t, app.WithCommittedTenantApp(account.OrganizationID, func(scoped *App) error {
				return scoped.ensureCoexistenceMediaJob(&account, message.ID)
			}))
			var originalJob models.ScheduledJob
			require.NoError(t, app.DB.Where("kind = ? AND aggregate_id = ?", coexistenceMediaHydrationJobKind, message.ID).First(&originalJob).Error)
			store := newBlockingCoexistenceMediaStore()
			defer store.release()
			if phase == "during_upload_cleanup_retry" {
				store.failNextDeletes(1)
			}
			app.Config.Storage.Type = "s3"
			app.ObjectStore = store
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			app.DB = app.DB.WithContext(ctx)
			processor := NewCoexistenceMediaProcessor(app, time.Hour)
			result := make(chan error, 1)
			if phase != "before_claim" {
				go func() { result <- processor.ProcessMessage(ctx, account.OrganizationID, message.ID) }()
				select {
				case <-store.putStarted:
				case err := <-result:
					t.Fatalf("worker stopped before upload barrier: %v", err)
				case <-ctx.Done():
					t.Fatal("durable worker never reached upload barrier")
				}
			}
			edit := coexistenceMediaReplacementEvent(t, app, account, message, map[string]any{
				"type": "text", "text": map[string]any{"body": "text supersedes media"},
			})
			require.NoError(t, app.WithCommittedTenantApp(account.OrganizationID, func(scoped *App) error {
				return scoped.persistCoexistenceEdit(&account, edit, nil, message.Direction, message.Status, "smb_message_echoes", false)
			}))
			store.release()
			if phase == "before_claim" {
				require.NoError(t, processor.ProcessMessage(ctx, account.OrganizationID, message.ID))
			} else {
				select {
				case err := <-result:
					if phase == "during_upload_cleanup_retry" {
						require.ErrorContains(t, err, "temporary object deletion failure")
					} else {
						require.NoError(t, err)
					}
				case <-ctx.Done():
					t.Fatal("durable worker never completed obsolete upload")
				}
			}
			if phase == "during_upload_cleanup_retry" {
				var retry models.ScheduledJob
				require.NoError(t, app.DB.First(&retry, originalJob.ID).Error)
				require.Equal(t, models.ScheduledJobStatusPending, retry.Status)
				require.NoError(t, app.DB.Model(&retry).Update("run_at", time.Now().UTC().Add(-time.Minute)).Error)
				require.NoError(t, processor.ProcessMessage(ctx, account.OrganizationID, message.ID))
			}
			var stored models.Message
			require.NoError(t, app.DB.First(&stored, message.ID).Error)
			assert.Equal(t, models.MessageTypeText, stored.MessageType)
			assert.Equal(t, "text supersedes media", stored.Content)
			assert.Empty(t, stored.MediaURL)
			assert.Empty(t, stored.MediaMimeType)
			assert.Empty(t, stored.MediaFilename)
			for _, key := range []string{coexistenceMediaProviderIDMetadataKey, coexistenceMediaHydratedIDMetadataKey, "coexistence_media_sha256"} {
				assert.Empty(t, stored.Metadata[key], "obsolete media metadata %s must be cleared atomically", key)
			}
			var jobs []models.ScheduledJob
			require.NoError(t, app.DB.Where("kind = ? AND aggregate_id = ?", coexistenceMediaHydrationJobKind, message.ID).Find(&jobs).Error)
			require.Len(t, jobs, 1, "text edit must not enqueue another obsolete media revision")
			assert.Equal(t, originalJob.ID, jobs[0].ID)
			assert.Equal(t, models.ScheduledJobStatusCompleted, jobs[0].Status)
			objects, deletes := store.counts()
			assert.Zero(t, objects)
			wantCalls, wantDeletes := int32(1), 1
			switch phase {
			case "before_claim":
				wantCalls = 0
			case "during_upload_cleanup_retry":
				wantDeletes = 2
			}
			assert.Equal(t, wantDeletes, deletes)
			assert.Equal(t, wantCalls, provider.metadataCalls.Load())
			assert.Equal(t, wantCalls, provider.downloadCalls.Load())
			require.NoError(t, processor.ProcessMessage(ctx, account.OrganizationID, message.ID))
			assert.Equal(t, wantCalls, provider.downloadCalls.Load(), "terminal/cleanup replay must not redownload")
		})
	}
}

func TestCoexistenceStoredEditWinsOverDelayedOriginalAndHistory(t *testing.T) {
	for _, editFirst := range []bool{false, true} {
		t.Run(fmt.Sprintf("edit_before_original=%t", editFirst), func(t *testing.T) {
			app, account, original, provider := newCoexistenceMediaFixtureWithPersistence(t, !editFirst)
			original.Metadata["coexistence_media_sha256"] = "original-A-sha"
			if editFirst {
				// The edit creates the immutable WAMID owner before the delayed
				// original arrives, so no historical owner is hard-deleted.
			} else {
				require.NoError(t, app.DB.Save(&original).Error)
				require.NoError(t, app.WithCommittedTenantApp(account.OrganizationID, func(scoped *App) error {
					return scoped.ensureCoexistenceMediaJob(&account, original.ID)
				}))
			}
			edit := coexistenceMediaReplacementEvent(t, app, account, original, map[string]any{
				"type": "image", "image": map[string]any{"id": "edited-B-media", "mime_type": "image/jpeg", "caption": "edited B wins"},
			})
			require.NoError(t, app.WithCommittedTenantApp(account.OrganizationID, func(scoped *App) error {
				return scoped.persistCoexistenceEdit(&account, edit, nil, original.Direction, original.Status, "smb_message_echoes", false)
			}))
			var contact models.Contact
			require.NoError(t, app.DB.First(&contact, original.ContactID).Error)
			encoded, err := json.Marshal(map[string]any{
				"id": original.WhatsAppMessageID, "from": "15550783881", "to": contact.PhoneNumber,
				"type": "image", "image": map[string]any{
					"id": original.Metadata[coexistenceMediaProviderIDMetadataKey], "mime_type": "image/jpeg", "sha256": "original-A-sha", "caption": "late original A",
				},
			})
			require.NoError(t, err)
			var delayed CoexistenceMessage
			require.NoError(t, json.Unmarshal(encoded, &delayed))
			for i := 0; i < 2; i++ {
				require.NoError(t, app.WithCommittedTenantApp(account.OrganizationID, func(scoped *App) error {
					winner, _, err := scoped.persistCoexistenceMessage(&account, coexistenceContactIdentity{Phone: contact.PhoneNumber}, delayed,
						original.Direction, original.Status, models.JSONB{"coexistence_source": "history"}, false)
					if err == nil && winner.ID != original.ID {
						return fmt.Errorf("late original changed winner ID")
					}
					return err
				}))
				require.NoError(t, app.WithCommittedTenantApp(account.OrganizationID, func(scoped *App) error {
					return scoped.persistHistoryMediaDetail(&account, "", delayed, nil)
				}))
			}
			var edited models.Message
			require.NoError(t, app.DB.First(&edited, original.ID).Error)
			assert.Equal(t, "edited B wins", edited.Content)
			assert.Equal(t, models.MessageTypeImage, edited.MessageType)
			assert.Equal(t, "edited-B-media", edited.Metadata[coexistenceMediaProviderIDMetadataKey])
			assert.Empty(t, edited.Metadata["coexistence_media_sha256"], "SHA-less replacement must not retain A's SHA")
			assert.Equal(t, edit.ID, edited.Metadata["coexistence_edit_event_id"])
			processor := NewCoexistenceMediaProcessor(app, time.Hour)
			// At most A's obsolete job plus B's current job exist. The real
			// processor must skip A and hydrate B without a custom load seam.
			for i := 0; i < 3; i++ {
				require.NoError(t, processor.ProcessMessage(context.Background(), account.OrganizationID, original.ID))
			}
			require.NoError(t, app.DB.First(&edited, original.ID).Error)
			assert.Equal(t, "edited-B-media", edited.Metadata[coexistenceMediaHydratedIDMetadataKey])
			_, revision, supported := coexistenceMediaRevision(&edited)
			require.True(t, supported)
			storageID := coexistenceMediaStorageRevisionID(original.ID, revision)
			assert.Contains(t, edited.MediaURL, uuid.NewSHA1(storageID, []byte("edited-B-media")).String())
			assert.EqualValues(t, 1, provider.metadataCalls.Load())
			assert.EqualValues(t, 1, provider.downloadCalls.Load())
			assert.Equal(t, "edited B wins", edited.Content)
			var messageCount int64
			require.NoError(t, app.DB.Model(&models.Message{}).Where("organization_id = ? AND whats_app_message_id = ?", account.OrganizationID, original.WhatsAppMessageID).Count(&messageCount).Error)
			assert.EqualValues(t, 1, messageCount)
		})
	}
}

func TestCoexistenceMediaWorkerSameIDEditRevisionFencesOldUpload(t *testing.T) {
	app, account, message, provider := newCoexistenceMediaFixture(t)
	require.NoError(t, app.WithCommittedTenantApp(account.OrganizationID, func(scoped *App) error {
		return scoped.ensureCoexistenceMediaJob(&account, message.ID)
	}))
	store := newBlockingCoexistenceMediaStore()
	defer store.release()
	app.Config.Storage.Type = "s3"
	app.ObjectStore = store
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	app.DB = app.DB.WithContext(ctx)
	processor := NewCoexistenceMediaProcessor(app, time.Hour)
	result := make(chan error, 1)
	go func() { result <- processor.ProcessMessage(ctx, account.OrganizationID, message.ID) }()
	select {
	case <-store.putStarted:
	case err := <-result:
		t.Fatalf("worker stopped before upload: %v", err)
	case <-ctx.Done():
		t.Fatal("worker did not reach upload barrier")
	}
	edit := coexistenceMediaReplacementEvent(t, app, account, message, map[string]any{
		"type": "image", "image": map[string]any{
			"id": message.Metadata[coexistenceMediaProviderIDMetadataKey], "mime_type": "image/jpeg", "caption": "same media ID, committed new edit",
		},
	})
	require.NoError(t, app.WithCommittedTenantApp(account.OrganizationID, func(scoped *App) error {
		return scoped.persistCoexistenceEdit(&account, edit, nil, message.Direction, message.Status, "smb_message_echoes", false)
	}))
	store.release()
	select {
	case err := <-result:
		require.NoError(t, err)
	case <-ctx.Done():
		t.Fatal("old revision worker did not finish")
	}
	var stored models.Message
	require.NoError(t, app.DB.First(&stored, message.ID).Error)
	assert.Empty(t, stored.MediaURL, "same provider ID is insufficient to attach an obsolete revision")
	assert.Equal(t, "same media ID, committed new edit", stored.Content)
	_, deletes := store.counts()
	assert.Equal(t, 1, deletes, "cleanup must remove only the obsolete revision's separate storage key")
	require.NoError(t, processor.ProcessMessage(ctx, account.OrganizationID, message.ID))
	require.NoError(t, app.DB.First(&stored, message.ID).Error)
	assert.NotEmpty(t, stored.MediaURL)
	assert.Equal(t, edit.ID, stored.Metadata["coexistence_edit_event_id"])
	assert.EqualValues(t, 2, provider.metadataCalls.Load(), "the new revision needs its own authoritative hydration")
	assert.EqualValues(t, 2, provider.downloadCalls.Load())
	objects, deletes := store.counts()
	assert.Equal(t, 1, objects)
	assert.Equal(t, 1, deletes)
}

func TestCoexistenceMediaHydratedSameIDEditCreatesNewRevision(t *testing.T) {
	app, account, message, provider := newCoexistenceMediaFixture(t)
	message.Content = "caption A"
	message.Metadata["coexistence_media_sha256"] = "unchanged-media-sha"
	require.NoError(t, app.DB.Save(&message).Error)
	require.NoError(t, app.WithCommittedTenantApp(account.OrganizationID, func(scoped *App) error {
		return scoped.ensureCoexistenceMediaJob(&account, message.ID)
	}))
	processor := NewCoexistenceMediaProcessor(app, time.Hour)
	require.NoError(t, processor.ProcessMessage(context.Background(), account.OrganizationID, message.ID))
	var hydratedA models.Message
	require.NoError(t, app.DB.First(&hydratedA, message.ID).Error)
	require.NotEmpty(t, hydratedA.MediaURL)
	mediaID := coexistenceMediaMetadataString(hydratedA.Metadata, coexistenceMediaProviderIDMetadataKey)
	require.Equal(t, mediaID, hydratedA.Metadata[coexistenceMediaHydratedIDMetadataKey])
	_, revisionA, supported := coexistenceMediaRevision(&hydratedA)
	require.True(t, supported)

	edit := coexistenceMediaReplacementEvent(t, app, account, hydratedA, map[string]any{
		"type": "image", "image": map[string]any{
			"id": mediaID, "sha256": "unchanged-media-sha", "mime_type": "image/jpeg", "caption": "caption B",
		},
	})
	require.NoError(t, app.WithCommittedTenantApp(account.OrganizationID, func(scoped *App) error {
		return scoped.persistCoexistenceEdit(&account, edit, nil, message.Direction, message.Status, "smb_message_echoes", false)
	}))
	var editedB models.Message
	require.NoError(t, app.DB.First(&editedB, message.ID).Error)
	assert.Equal(t, "caption B", editedB.Content)
	assert.Empty(t, editedB.MediaURL, "same provider ID/SHA does not authorize retaining another committed revision's bytes")
	assert.Empty(t, editedB.Metadata[coexistenceMediaHydratedIDMetadataKey])
	assert.Equal(t, mediaID, editedB.Metadata[coexistenceMediaProviderIDMetadataKey])
	assert.Equal(t, "unchanged-media-sha", editedB.Metadata["coexistence_media_sha256"])
	_, revisionB, supported := coexistenceMediaRevision(&editedB)
	require.True(t, supported)
	assert.NotEqual(t, revisionA, revisionB)
	var jobs []models.ScheduledJob
	require.NoError(t, app.DB.Where("kind = ? AND aggregate_id = ?", coexistenceMediaHydrationJobKind, message.ID).Find(&jobs).Error)
	require.Len(t, jobs, 2, "distinct committed edit must enqueue its own revision without resetting A's completed job")
	for _, job := range jobs {
		if job.Payload["media_revision"] == revisionA {
			assert.Equal(t, models.ScheduledJobStatusCompleted, job.Status)
		} else {
			assert.Equal(t, revisionB, job.Payload["media_revision"])
			assert.Equal(t, models.ScheduledJobStatusPending, job.Status)
		}
	}
	require.NoError(t, processor.ProcessMessage(context.Background(), account.OrganizationID, message.ID))
	require.NoError(t, app.DB.First(&editedB, message.ID).Error)
	assert.NotEmpty(t, editedB.MediaURL)
	assert.NotEqual(t, hydratedA.MediaURL, editedB.MediaURL)
	storageID := coexistenceMediaStorageRevisionID(message.ID, revisionB)
	assert.Contains(t, editedB.MediaURL, storageID.String())
	assert.Equal(t, mediaID, editedB.Metadata[coexistenceMediaHydratedIDMetadataKey])
	assert.Equal(t, edit.ID, editedB.Metadata["coexistence_edit_event_id"])
	assert.EqualValues(t, 2, provider.metadataCalls.Load())
	assert.EqualValues(t, 2, provider.downloadCalls.Load())
	// Completed replay cannot attach A or download either revision again.
	require.NoError(t, processor.ProcessMessage(context.Background(), account.OrganizationID, message.ID))
	assert.EqualValues(t, 2, provider.downloadCalls.Load())
}

func TestCoexistenceMediaHydrationDurableAndIdempotent(t *testing.T) {
	app, account, message, provider := newCoexistenceMediaFixture(t)

	require.NoError(t, app.WithCommittedTenantApp(account.OrganizationID, func(scoped *App) error {
		if err := scoped.ensureCoexistenceMediaJob(&account, message.ID); err != nil {
			return err
		}
		return scoped.ensureCoexistenceMediaJob(&account, message.ID)
	}))

	var jobs []models.ScheduledJob
	require.NoError(t, app.DB.Where(
		"organization_id = ? AND kind = ? AND aggregate_id = ?",
		account.OrganizationID,
		coexistenceMediaHydrationJobKind,
		message.ID,
	).Find(&jobs).Error)
	require.Len(t, jobs, 1)
	payload, err := json.Marshal(jobs[0].Payload)
	require.NoError(t, err)
	assert.NotContains(t, string(payload), account.AccessToken)
	assert.NotContains(t, string(payload), account.AppSecret)
	assert.Equal(t, account.ID.String(), jobs[0].Payload["account_id"])
	assert.Equal(t, message.ID.String(), jobs[0].Payload["message_id"])

	processor := NewCoexistenceMediaProcessor(app, time.Hour)
	require.NoError(t, processor.ProcessMessage(context.Background(), account.OrganizationID, message.ID))

	var hydrated models.Message
	require.NoError(t, app.DB.Where(
		"id = ? AND organization_id = ?",
		message.ID,
		account.OrganizationID,
	).First(&hydrated).Error)
	mediaID := coexistenceMediaMetadataString(
		message.Metadata,
		coexistenceMediaProviderIDMetadataKey,
	)
	_, revision, supported := coexistenceMediaRevision(&message)
	require.True(t, supported)
	storageID := coexistenceMediaStorageRevisionID(message.ID, revision)
	versionID := uuid.NewSHA1(storageID, []byte(mediaID))
	expectedRelativePath := filepath.Join(
		"coexistence",
		storageID.String()+"-"+versionID.String()+".bin",
	)
	assert.Equal(t, filepath.ToSlash(expectedRelativePath), filepath.ToSlash(hydrated.MediaURL))
	assert.Equal(t, mediaID, hydrated.Metadata[coexistenceMediaHydratedIDMetadataKey])
	stored, err := os.ReadFile(filepath.Join(app.Config.Storage.LocalPath, expectedRelativePath))
	require.NoError(t, err)
	assert.Equal(t, []byte("coexistence-media-bytes"), stored)
	assert.EqualValues(t, 1, provider.metadataCalls.Load())
	assert.EqualValues(t, 1, provider.downloadCalls.Load())

	require.NoError(t, app.DB.Where("id = ?", jobs[0].ID).First(&jobs[0]).Error)
	assert.Equal(t, models.ScheduledJobStatusCompleted, jobs[0].Status)
	assert.Equal(t, 1, jobs[0].Attempts)

	// A completed job is not claimable and cannot redownload the same media.
	require.NoError(t, processor.ProcessMessage(context.Background(), account.OrganizationID, message.ID))
	assert.EqualValues(t, 1, provider.metadataCalls.Load())
	assert.EqualValues(t, 1, provider.downloadCalls.Load())
}

func TestCoexistenceMediaHydrationRetriesTransientProviderFailure(t *testing.T) {
	app, account, message, provider := newCoexistenceMediaFixture(t)
	provider.failMetadata.Store(1)
	require.NoError(t, app.WithCommittedTenantApp(account.OrganizationID, func(scoped *App) error {
		return scoped.ensureCoexistenceMediaJob(&account, message.ID)
	}))

	processor := NewCoexistenceMediaProcessor(app, time.Hour)
	err := processor.ProcessMessage(context.Background(), account.OrganizationID, message.ID)
	require.Error(t, err)

	var job models.ScheduledJob
	require.NoError(t, app.DB.Where(
		"organization_id = ? AND kind = ? AND aggregate_id = ?",
		account.OrganizationID,
		coexistenceMediaHydrationJobKind,
		message.ID,
	).First(&job).Error)
	assert.Equal(t, models.ScheduledJobStatusPending, job.Status)
	assert.Equal(t, 1, job.Attempts)
	assert.NotEmpty(t, job.LastError)
	require.NoError(t, app.DB.Model(&models.ScheduledJob{}).Where("id = ?", job.ID).
		Update("run_at", time.Now().UTC().Add(-time.Minute)).Error)

	require.NoError(t, processor.ProcessMessage(context.Background(), account.OrganizationID, message.ID))
	require.NoError(t, app.DB.Where("id = ?", job.ID).First(&job).Error)
	assert.Equal(t, models.ScheduledJobStatusCompleted, job.Status)
	assert.Equal(t, 2, job.Attempts)
	assert.EqualValues(t, 2, provider.metadataCalls.Load())
	assert.EqualValues(t, 1, provider.downloadCalls.Load())
}

func TestCoexistenceMediaHydrationSkipsAlreadyHydratedMessage(t *testing.T) {
	app, account, message, provider := newCoexistenceMediaFixture(t)
	require.NoError(t, app.WithCommittedTenantApp(account.OrganizationID, func(scoped *App) error {
		return scoped.ensureCoexistenceMediaJob(&account, message.ID)
	}))

	mediaID := coexistenceMediaMetadataString(
		message.Metadata,
		coexistenceMediaProviderIDMetadataKey,
	)
	metadata := cloneMessageMetadata(message.Metadata)
	metadata[coexistenceMediaHydratedIDMetadataKey] = mediaID
	require.NoError(t, app.DB.Model(&models.Message{}).Where("id = ?", message.ID).
		Updates(map[string]any{
			"media_url": "images/already-present.jpg",
			"metadata":  metadata,
		}).Error)

	processor := NewCoexistenceMediaProcessor(app, time.Hour)
	require.NoError(t, processor.ProcessMessage(context.Background(), account.OrganizationID, message.ID))
	assert.Zero(t, provider.metadataCalls.Load())
	assert.Zero(t, provider.downloadCalls.Load())

	var job models.ScheduledJob
	require.NoError(t, app.DB.Where(
		"organization_id = ? AND kind = ? AND aggregate_id = ?",
		account.OrganizationID,
		coexistenceMediaHydrationJobKind,
		message.ID,
	).First(&job).Error)
	assert.Equal(t, models.ScheduledJobStatusCompleted, job.Status)
}

func TestCoexistenceMediaHydrationCompletesInactiveAccountWithoutProviderCall(t *testing.T) {
	app, account, message, provider := newCoexistenceMediaFixture(t)
	require.NoError(t, app.WithCommittedTenantApp(account.OrganizationID, func(scoped *App) error {
		return scoped.ensureCoexistenceMediaJob(&account, message.ID)
	}))
	require.NoError(t, app.DB.Model(&models.WhatsAppAccount{}).Where(
		"id = ? AND organization_id = ?",
		account.ID,
		account.OrganizationID,
	).Updates(map[string]any{
		"status":       "disconnected",
		"access_token": "enc:deliberately-unusable-after-disconnect",
	}).Error)

	processor := NewCoexistenceMediaProcessor(app, time.Hour)
	require.NoError(t, processor.ProcessMessage(context.Background(), account.OrganizationID, message.ID))
	assert.Zero(t, provider.metadataCalls.Load())
	assert.Zero(t, provider.downloadCalls.Load())

	var job models.ScheduledJob
	require.NoError(t, app.DB.Where(
		"organization_id = ? AND kind = ? AND aggregate_id = ?",
		account.OrganizationID,
		coexistenceMediaHydrationJobKind,
		message.ID,
	).First(&job).Error)
	assert.Equal(t, models.ScheduledJobStatusCompleted, job.Status)
	assert.Empty(t, job.LastError)

	var stored models.Message
	require.NoError(t, app.DB.Where("id = ?", message.ID).First(&stored).Error)
	assert.Empty(t, stored.MediaURL)
}

func TestCoexistenceMediaHydrationCredentialRotationAtProviderBoundaryUsesLiveToken(t *testing.T) {
	app, account, message, provider := newCoexistenceMediaFixture(t)
	require.NoError(t, app.WithCommittedTenantApp(account.OrganizationID, func(scoped *App) error {
		return scoped.ensureCoexistenceMediaJob(&account, message.ID)
	}))
	rotatedToken := "coexistence-media-rotated-" + uuid.NewString()
	// The provider accepts only the new credential. The caller-owned account
	// value and the worker's first committed snapshot intentionally remain old.
	provider.token = rotatedToken

	blocker := app.DB.Begin()
	require.NoError(t, blocker.Error)
	committed := false
	defer func() {
		if !committed {
			_ = blocker.Rollback().Error
		}
	}()
	var blockerPID int
	require.NoError(t, blocker.Raw("SELECT pg_backend_pid()").Scan(&blockerPID).Error)
	require.NoError(t, blocker.Model(&models.WhatsAppAccount{}).
		Where("id = ? AND organization_id = ?", account.ID, account.OrganizationID).
		Update("access_token", rotatedToken).Error)

	processor := NewCoexistenceMediaProcessor(app, time.Hour)
	result := make(chan error, 1)
	go func() {
		result <- processor.ProcessMessage(context.Background(), account.OrganizationID, message.ID)
	}()
	waitForCoexistenceLifecycleBlocker(t, app.DB, blockerPID)
	require.NoError(t, blocker.Commit().Error)
	committed = true
	select {
	case processErr := <-result:
		require.NoError(t, processErr)
	case <-time.After(5 * time.Second):
		t.Fatal("coexistence media hydration did not finish after credential rotation")
	}

	assert.EqualValues(t, 1, provider.metadataCalls.Load())
	assert.EqualValues(t, 1, provider.downloadCalls.Load())
	var stored models.Message
	require.NoError(t, app.DB.Where("id = ? AND organization_id = ?", message.ID, account.OrganizationID).
		First(&stored).Error)
	assert.NotEmpty(t, stored.MediaURL)
	var job models.ScheduledJob
	require.NoError(t, app.DB.Where(
		"organization_id = ? AND kind = ? AND aggregate_id = ?",
		account.OrganizationID,
		coexistenceMediaHydrationJobKind,
		message.ID,
	).First(&job).Error)
	assert.Equal(t, models.ScheduledJobStatusCompleted, job.Status)
}

func TestCoexistenceMediaHydrationRechecksInactiveAccountAndCleansObject(t *testing.T) {
	app, account, message, provider := newCoexistenceMediaFixture(t)
	require.NoError(t, app.WithCommittedTenantApp(account.OrganizationID, func(scoped *App) error {
		return scoped.ensureCoexistenceMediaJob(&account, message.ID)
	}))
	store := newBlockingCoexistenceMediaStore()
	t.Cleanup(store.release)
	app.Config.Storage.Type = "s3"
	app.ObjectStore = store
	processor := NewCoexistenceMediaProcessor(app, time.Hour)

	result := make(chan error, 1)
	go func() {
		result <- processor.ProcessMessage(
			context.Background(),
			account.OrganizationID,
			message.ID,
		)
	}()
	select {
	case <-store.putStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("media object write did not start")
	}
	require.NoError(t, app.DB.Model(&models.WhatsAppAccount{}).Where(
		"id = ? AND organization_id = ?",
		account.ID,
		account.OrganizationID,
	).Update("status", "disconnected").Error)
	store.release()
	select {
	case err := <-result:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("media hydration did not finish")
	}

	var storedMessage models.Message
	require.NoError(t, app.DB.Where("id = ?", message.ID).First(&storedMessage).Error)
	assert.Empty(t, storedMessage.MediaURL)
	objects, deletes := store.counts()
	assert.Zero(t, objects, "the just-written unreferenced object must be removed")
	assert.Equal(t, 1, deletes)
	assert.EqualValues(t, 1, provider.metadataCalls.Load())
	assert.EqualValues(t, 1, provider.downloadCalls.Load())

	var job models.ScheduledJob
	require.NoError(t, app.DB.Where(
		"organization_id = ? AND kind = ? AND aggregate_id = ?",
		account.OrganizationID,
		coexistenceMediaHydrationJobKind,
		message.ID,
	).First(&job).Error)
	assert.Equal(t, models.ScheduledJobStatusCompleted, job.Status)
}

func TestCoexistenceMediaHydrationRejectsRotatedCredentialGeneration(t *testing.T) {
	app, account, message, provider := newCoexistenceMediaFixture(t)
	require.NoError(t, app.WithCommittedTenantApp(account.OrganizationID, func(scoped *App) error {
		return scoped.ensureCoexistenceMediaJob(&account, message.ID)
	}))
	store := newBlockingCoexistenceMediaStore()
	t.Cleanup(store.release)
	app.Config.Storage.Type = "s3"
	app.ObjectStore = store
	processor := NewCoexistenceMediaProcessor(app, time.Hour)

	result := make(chan error, 1)
	go func() {
		result <- processor.ProcessMessage(context.Background(), account.OrganizationID, message.ID)
	}()
	select {
	case <-store.putStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("media object write did not start")
	}
	require.NoError(t, app.DB.Model(&models.WhatsAppAccount{}).Where(
		"id = ? AND organization_id = ?",
		account.ID,
		account.OrganizationID,
	).Update("access_token", "rotated-token-"+uuid.NewString()).Error)
	store.release()
	select {
	case err := <-result:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("media hydration did not finish")
	}

	var storedMessage models.Message
	require.NoError(t, app.DB.Where("id = ?", message.ID).First(&storedMessage).Error)
	assert.Empty(t, storedMessage.MediaURL)
	objects, deletes := store.counts()
	assert.Zero(t, objects)
	assert.Equal(t, 1, deletes)
	assert.EqualValues(t, 1, provider.metadataCalls.Load())
	assert.EqualValues(t, 1, provider.downloadCalls.Load())

	var job models.ScheduledJob
	require.NoError(t, app.DB.Where(
		"organization_id = ? AND kind = ? AND aggregate_id = ?",
		account.OrganizationID,
		coexistenceMediaHydrationJobKind,
		message.ID,
	).First(&job).Error)
	assert.Equal(t, models.ScheduledJobStatusCompleted, job.Status)
}

func TestCoexistenceMediaHydrationRetriesDiscardCleanupWithRenewedLease(t *testing.T) {
	app, account, message, provider := newCoexistenceMediaFixture(t)
	require.NoError(t, app.WithCommittedTenantApp(account.OrganizationID, func(scoped *App) error {
		return scoped.ensureCoexistenceMediaJob(&account, message.ID)
	}))
	store := newBlockingCoexistenceMediaStore()
	t.Cleanup(store.release)
	store.failNextDeletes(1)
	app.Config.Storage.Type = "s3"
	app.ObjectStore = store
	processor := NewCoexistenceMediaProcessor(app, time.Hour)

	result := make(chan error, 1)
	go func() {
		result <- processor.ProcessMessage(context.Background(), account.OrganizationID, message.ID)
	}()
	select {
	case <-store.putStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("media object write did not start")
	}
	require.NoError(t, app.DB.Model(&models.WhatsAppAccount{}).Where(
		"id = ? AND organization_id = ?",
		account.ID,
		account.OrganizationID,
	).Update("status", "disconnected").Error)
	store.release()
	select {
	case err := <-result:
		require.Error(t, err)
		assert.ErrorContains(t, err, "temporary object deletion failure")
	case <-time.After(5 * time.Second):
		t.Fatal("media hydration did not finish")
	}

	var job models.ScheduledJob
	require.NoError(t, app.DB.Where(
		"organization_id = ? AND kind = ? AND aggregate_id = ?",
		account.OrganizationID,
		coexistenceMediaHydrationJobKind,
		message.ID,
	).First(&job).Error)
	assert.Equal(t, models.ScheduledJobStatusPending, job.Status,
		"fail must CAS against the lease generation renewed by persistence")
	require.NoError(t, app.DB.Model(&models.ScheduledJob{}).Where("id = ?", job.ID).
		Update("run_at", time.Now().UTC().Add(-time.Minute)).Error)

	require.NoError(t, processor.ProcessMessage(context.Background(), account.OrganizationID, message.ID))
	require.NoError(t, app.DB.Where("id = ?", job.ID).First(&job).Error)
	assert.Equal(t, models.ScheduledJobStatusCompleted, job.Status)
	objects, deletes := store.counts()
	assert.Zero(t, objects)
	assert.Equal(t, 2, deletes)
	assert.EqualValues(t, 1, provider.metadataCalls.Load(), "cleanup retry must not call Meta again")
	assert.EqualValues(t, 1, provider.downloadCalls.Load(), "cleanup retry must not call Meta again")
}

func TestCoexistenceMediaHydrationCleansSupersededObject(t *testing.T) {
	app, account, message, _ := newCoexistenceMediaFixture(t)
	require.NoError(t, app.WithCommittedTenantApp(account.OrganizationID, func(scoped *App) error {
		return scoped.ensureCoexistenceMediaJob(&account, message.ID)
	}))
	store := newBlockingCoexistenceMediaStore()
	t.Cleanup(store.release)
	app.Config.Storage.Type = "s3"
	app.ObjectStore = store
	processor := NewCoexistenceMediaProcessor(app, time.Hour)

	result := make(chan error, 1)
	go func() {
		result <- processor.ProcessMessage(
			context.Background(),
			account.OrganizationID,
			message.ID,
		)
	}()
	select {
	case <-store.putStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("media object write did not start")
	}
	replacementMetadata := cloneMessageMetadata(message.Metadata)
	replacementMetadata[coexistenceMediaProviderIDMetadataKey] = "replacement-media-id"
	require.NoError(t, app.DB.Model(&models.Message{}).Where(
		"id = ? AND organization_id = ?",
		message.ID,
		account.OrganizationID,
	).Update("metadata", replacementMetadata).Error)
	store.release()
	select {
	case err := <-result:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("media hydration did not finish")
	}

	var storedMessage models.Message
	require.NoError(t, app.DB.Where("id = ?", message.ID).First(&storedMessage).Error)
	assert.Empty(t, storedMessage.MediaURL)
	assert.Equal(
		t,
		"replacement-media-id",
		storedMessage.Metadata[coexistenceMediaProviderIDMetadataKey],
	)
	objects, deletes := store.counts()
	assert.Zero(t, objects, "a superseded deterministic object must be removed")
	assert.Equal(t, 1, deletes)

	var job models.ScheduledJob
	require.NoError(t, app.DB.Where(
		"organization_id = ? AND kind = ? AND aggregate_id = ?",
		account.OrganizationID,
		coexistenceMediaHydrationJobKind,
		message.ID,
	).First(&job).Error)
	assert.Equal(t, models.ScheduledJobStatusCompleted, job.Status)
}

func TestCoexistenceMediaHydrationStaleOwnerCannotCommit(t *testing.T) {
	app, account, message, _ := newCoexistenceMediaFixture(t)
	require.NoError(t, app.WithCommittedTenantApp(account.OrganizationID, func(scoped *App) error {
		return scoped.ensureCoexistenceMediaJob(&account, message.ID)
	}))
	store := newBlockingCoexistenceMediaStore()
	t.Cleanup(store.release)
	app.Config.Storage.Type = "s3"
	app.ObjectStore = store
	processor := NewCoexistenceMediaProcessor(app, time.Hour)

	result := make(chan error, 1)
	go func() {
		result <- processor.ProcessMessage(
			context.Background(),
			account.OrganizationID,
			message.ID,
		)
	}()
	select {
	case <-store.putStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("media object write did not start")
	}
	var claimed models.ScheduledJob
	require.NoError(t, app.DB.Where(
		"organization_id = ? AND kind = ? AND aggregate_id = ?",
		account.OrganizationID,
		coexistenceMediaHydrationJobKind,
		message.ID,
	).First(&claimed).Error)
	require.Equal(t, models.ScheduledJobStatusProcessing, claimed.Status)
	require.NoError(t, app.DB.Model(&models.ScheduledJob{}).Where(
		"id = ? AND organization_id = ?",
		claimed.ID,
		account.OrganizationID,
	).Updates(map[string]any{
		"attempts":  claimed.Attempts + 1,
		"locked_at": time.Now().UTC(),
		"locked_by": "replacement-worker",
		"version":   claimed.Version + 1,
	}).Error)
	store.release()
	select {
	case err := <-result:
		require.Error(t, err)
		assert.ErrorContains(t, err, errCoexistenceMediaLeaseLost.Error())
	case <-time.After(5 * time.Second):
		t.Fatal("media hydration did not finish")
	}

	var storedMessage models.Message
	require.NoError(t, app.DB.Where("id = ?", message.ID).First(&storedMessage).Error)
	assert.Empty(t, storedMessage.MediaURL, "a stale worker must not publish media_ready")
	assert.Empty(t, storedMessage.Metadata[coexistenceMediaHydratedIDMetadataKey])
	objects, deletes := store.counts()
	assert.Equal(t, 1, objects, "cleanup is unsafe once another worker owns the stable key")
	assert.Zero(t, deletes)

	require.NoError(t, app.DB.Where("id = ?", claimed.ID).First(&claimed).Error)
	assert.Equal(t, models.ScheduledJobStatusProcessing, claimed.Status)
	assert.Equal(t, "replacement-worker", claimed.LockedBy)
}

func TestCoexistenceMediaErrorTextRedactsSignedURLs(t *testing.T) {
	signedURL := "https://lookaside.fbsbx.com/whatsapp_business/attachments/?mid=123&ext=secret"
	err := errors.New("download " + signedURL + ": upstream failure")

	value := coexistenceMediaErrorText(err)

	assert.NotContains(t, value, "lookaside.fbsbx.com")
	assert.NotContains(t, value, "ext=secret")
	assert.Contains(t, value, "<redacted_url>")
}

func TestCoexistenceMediaProcessorWaitReturnsAfterStop(t *testing.T) {
	processor := NewCoexistenceMediaProcessor(nil, time.Hour)
	processor.Stop()
	go processor.Start(context.Background())

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	require.NoError(t, processor.Wait(ctx))
}
