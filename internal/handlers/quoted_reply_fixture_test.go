package handlers

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/internal/storage"
	"github.com/shridarpatil/whatomate/pkg/whatsapp"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/require"
	"github.com/zerodha/fastglue"
)

type quoteMediaFixtureBlob struct {
	body []byte
	mime string
}

type quoteMediaFixtureRead struct {
	key   string
	limit int64
}

type quoteMediaFixtureStore struct {
	storage.ObjectStore
	objects      map[string]quoteMediaFixtureBlob
	reads        []quoteMediaFixtureRead
	unbounded    int
	beforeReturn func(context.Context) error
}

func (s *quoteMediaFixtureStore) Get(context.Context, string) ([]byte, string, error) {
	s.unbounded++
	return nil, "", storage.ErrObjectNotFound
}

func (s *quoteMediaFixtureStore) GetLimited(ctx context.Context, key string, limit int64) ([]byte, string, error) {
	s.reads = append(s.reads, quoteMediaFixtureRead{key, limit})
	blob, ok := s.objects[key]
	if !ok {
		return nil, "", storage.ErrObjectNotFound
	}
	if s.beforeReturn != nil {
		if err := s.beforeReturn(ctx); err != nil {
			return nil, "", err
		}
	}
	body, err := storage.ReadLimited(ctx, bytes.NewReader(blob.body), limit)
	return body, blob.mime, err
}

type quoteMediaFixture struct {
	app       *App
	store     *quoteMediaFixtureStore
	home, org *models.Organization
	user      *models.User
	account   *models.WhatsAppAccount
	contact   *models.Contact
	both      *models.CustomRole
	chatOnly  *models.CustomRole
	cutoff    time.Time
}

func newQuoteMediaFixture(t *testing.T) *quoteMediaFixture {
	t.Helper()
	db := testutil.SetupTestDB(t)
	log := testutil.NopLogger()
	var providerCalls atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		providerCalls.Add(1)
		w.WriteHeader(http.StatusBadRequest)
	}))
	t.Cleanup(func() {
		provider.Close()
		require.Zero(t, providerCalls.Load(), "reading an attachment must never contact WhatsApp")
	})
	store := &quoteMediaFixtureStore{objects: make(map[string]quoteMediaFixtureBlob)}
	app := &App{DB: db, Log: log, ObjectStore: store, WhatsApp: whatsapp.NewWithBaseURL(log, provider.URL)}
	reseller := testutil.CreateTestReseller(t, db)
	home := testutil.CreateTestOrganizationForReseller(t, db, reseller.ID)
	org := testutil.CreateTestOrganizationForReseller(t, db, reseller.ID)
	const chatRead = models.ResourceChat + ":" + models.ActionRead
	const inboxRead = models.ResourceConversations + ":" + models.ActionRead
	both := testutil.CreateTestRoleWithKeys(t, db, org.ID, "quote-both", []string{chatRead, inboxRead})
	chatOnly := testutil.CreateTestRoleWithKeys(t, db, org.ID, "quote-chat", []string{chatRead})
	homeRole := testutil.CreateTestRoleWithKeys(t, db, home.ID, "quote-home", []string{inboxRead})
	user := testutil.CreateTestUser(t, db, home.ID, testutil.WithRoleID(&homeRole.ID))
	require.NoError(t, db.Create(&models.UserOrganization{
		BaseModel: models.BaseModel{ID: uuid.New()}, UserID: user.ID, OrganizationID: org.ID, RoleID: &both.ID,
	}).Error)
	enableCustomerWorkspaceEntitlements(t, db, org.ID, user.ID, "omnichannel.enabled")

	account := testutil.CreateTestWhatsAppAccount(t, db, org.ID)
	contact := testutil.CreateTestContactWith(t, db, org.ID, testutil.WithContactAccount(account.Name))
	cutoff := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	require.NoError(t, db.Create(&models.ChatbotSettings{
		BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: org.ID,
		AgentAssignment: models.AgentAssignmentConfig{CurrentConversationOnly: true},
	}).Error)
	require.NoError(t, db.Create(&models.AgentTransfer{
		BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: org.ID, ContactID: contact.ID,
		WhatsAppAccount: account.Name, PhoneNumber: contact.PhoneNumber, Status: models.TransferStatusActive,
		Source: models.TransferSourceManual, AgentID: &user.ID, TransferredAt: cutoff,
	}).Error)
	return &quoteMediaFixture{
		app: app, store: store, home: home, org: org, user: user, account: account, contact: contact,
		both: both, chatOnly: chatOnly, cutoff: cutoff,
	}
}

func (f *quoteMediaFixture) setRole(t *testing.T, role *models.CustomRole) {
	t.Helper()
	result := f.app.DB.Model(&models.UserOrganization{}).
		Where("user_id = ? AND organization_id = ?", f.user.ID, f.org.ID).Update("role_id", role.ID)
	require.NoError(t, result.Error)
	require.EqualValues(t, 1, result.RowsAffected)
}

func (f *quoteMediaFixture) message(
	t *testing.T, account *models.WhatsAppAccount, contact *models.Contact,
	kind models.MessageType, key string, at time.Time, linked bool,
) models.Message {
	t.Helper()
	filename, mimeType := "folder\\receipt\u202e.pdf", "application/pdf"
	if kind == models.MessageTypeImage {
		filename, mimeType = "photo.png", "image/png"
	}
	message := models.Message{
		BaseModel:      models.BaseModel{ID: uuid.New(), CreatedAt: at, UpdatedAt: at},
		OrganizationID: account.OrganizationID, ContactID: contact.ID, WhatsAppAccount: account.Name,
		WhatsAppMessageID: "wamid.omni-attachment-" + uuid.NewString(), Direction: models.DirectionIncoming,
		MessageType: kind, Status: models.MessageStatusDelivered, Content: "synthetic caption",
		MediaURL: key, MediaMimeType: mimeType, MediaFilename: filename,
		Metadata: models.JSONB{"whatsapp_customer_forwarded": true},
	}
	require.NoError(t, f.app.DB.Create(&message).Error)
	if linked {
		require.NoError(t, f.app.WithCommittedTenantApp(account.OrganizationID, func(scoped *App) error {
			return persistLegacyWhatsAppMessageMirror(scoped.DB, account, message.ID)
		}))
		require.NoError(t, f.app.DB.First(&message, "id = ?", message.ID).Error)
		require.NotNil(t, message.InboxConversationID)
	}
	return message
}

func (f *quoteMediaFixture) request(t *testing.T, selected, conversation, message string) *fastglue.Request {
	t.Helper()
	req := testutil.NewGETRequest(t)
	testutil.SetAuthContext(req, f.home.ID, f.user.ID)
	if selected != "" {
		testutil.SetHeader(req, "X-Organization-ID", selected)
	}
	testutil.SetPathParam(req, "id", conversation)
	testutil.SetPathParam(req, "message_id", message)
	t.Cleanup(func() { require.NoError(t, req.RequestCtx.Response.CloseBodyStream()) })
	return req
}

func (f *quoteMediaFixture) assertNoReadState(t *testing.T, message models.Message) {
	t.Helper()
	var row models.Message
	require.NoError(t, f.app.DB.First(&row, "id = ?", message.ID).Error)
	require.Equal(t, models.MessageStatusDelivered, row.Status)
	var reads int64
	require.NoError(t, f.app.DB.Model(&models.ConversationRead{}).
		Where("organization_id = ? AND conversation_id = ?", f.org.ID, *message.InboxConversationID).
		Count(&reads).Error)
	require.Zero(t, reads)
	require.Zero(t, f.store.unbounded)
}
