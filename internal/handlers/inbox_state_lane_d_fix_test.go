package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/pkg/whatsapp"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// laneDSQLRecorder records every statement a gorm session runs, in order.
type laneDSQLRecorder struct {
	logger.Interface
	mu         sync.Mutex
	statements []string
}

func (r *laneDSQLRecorder) LogMode(logger.LogLevel) logger.Interface { return r }

func (r *laneDSQLRecorder) Trace(_ context.Context, _ time.Time, fc func() (string, int64), _ error) {
	statement, _ := fc()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.statements = append(r.statements, statement)
}

func (r *laneDSQLRecorder) rowLocks() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var locks []string
	for _, statement := range r.statements {
		if strings.Contains(strings.ToUpper(statement), "FOR UPDATE") {
			locks = append(locks, statement)
		}
	}
	return locks
}

func TestLaneDManualReadClearsEveryConversationInLockOrder(t *testing.T) {
	db := testutil.SetupTestDB(t)
	organization := testutil.CreateTestOrganization(t, db)
	adminRole := testutil.CreateAdminRole(t, db, organization.ID)
	reader := testutil.CreateTestUser(t, db, organization.ID, testutil.WithRoleID(&adminRole.ID))
	firstAccount := testutil.CreateTestWhatsAppAccount(t, db, organization.ID)
	secondAccount := testutil.CreateTestWhatsAppAccount(t, db, organization.ID)
	contact := testutil.CreateTestContactWith(t, db, organization.ID, testutil.WithContactAccount(firstAccount.Name))
	firstConversation := createLegacyChatConversation(t, db, firstAccount, contact.ID)
	secondConversation := createLegacyChatConversation(t, db, secondAccount, contact.ID)
	base := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	firstOld := createLegacyChatMessage(t, db, organization.ID, firstAccount.Name, contact.ID, firstConversation.ID, uuid.New(), base)
	firstNewest := createLegacyChatMessage(t, db, organization.ID, firstAccount.Name, contact.ID, firstConversation.ID, uuid.New(), base.Add(time.Second))
	// The newest message overall is on the second number. Reading only the
	// newest conversation would leave the first number's messages unread.
	secondNewest := createLegacyChatMessage(t, db, organization.ID, secondAccount.Name, contact.ID, secondConversation.ID, uuid.New(), base.Add(2*time.Second))
	laneDSetManualUnread(t, db, contact, nil)

	recorder := &laneDSQLRecorder{Interface: logger.Discard}
	app := &App{DB: db.Session(&gorm.Session{Logger: recorder}), Log: testutil.NopLogger()}
	payload, status := laneDMarkRead(t, app, organization.ID, reader.ID, contact.ID, map[string]any{"mark": "read"})
	require.Equal(t, fasthttp.StatusOK, status)
	assert.Equal(t, true, payload["cursor_synced"])
	assert.Equal(t, false, payload["manual_unread"])

	for _, id := range []uuid.UUID{firstOld.ID, firstNewest.ID, secondNewest.ID} {
		var message models.Message
		require.NoError(t, db.First(&message, "id = ?", id).Error)
		assert.Equal(t, models.MessageStatusRead, message.Status, "message %s", id)
	}
	firstCursor := loadConversationReadCursor(t, db, organization.ID, reader.ID, firstConversation.ID)
	require.NotNil(t, firstCursor.LastReadMessageID)
	assert.Equal(t, firstNewest.ID, *firstCursor.LastReadMessageID)
	secondCursor := loadConversationReadCursor(t, db, organization.ID, reader.ID, secondConversation.ID)
	require.NotNil(t, secondCursor.LastReadMessageID)
	assert.Equal(t, secondNewest.ID, *secondCursor.LastReadMessageID)
	assert.False(t, laneDManualUnreadStored(t, db, contact.ID))
	unread, status := laneDListContacts(t, &App{DB: db, Log: testutil.NopLogger()}, organization.ID, reader.ID, map[string]string{"unread": "true"})
	require.Equal(t, fasthttp.StatusOK, status)
	assert.Empty(t, unread.Contacts)

	// Lock order of the shared read: both conversations in one statement,
	// ordered by id, then the contact; no conversation is locked after it.
	locks := recorder.rowLocks()
	contactLock := -1
	for i, statement := range locks {
		if strings.Contains(statement, "had_manual_unread") {
			contactLock = i
			break
		}
	}
	require.GreaterOrEqual(t, contactLock, 1, "row locks: %v", locks)
	both := locks[contactLock-1]
	assert.Contains(t, both, "inbox_conversations")
	assert.Contains(t, both, firstConversation.ID.String())
	assert.Contains(t, both, secondConversation.ID.String())
	assert.Contains(t, strings.ToUpper(both), "ORDER BY")
	for _, statement := range locks[contactLock+1:] {
		assert.NotContains(t, statement, "inbox_conversations", "a conversation was locked after the contact")
	}
}

func TestLaneDManualReadSendsNoWhatsAppReadReceipts(t *testing.T) {
	var mu sync.Mutex
	var receipts []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["status"] == "read" {
			mu.Lock()
			receipts = append(receipts, fmt.Sprint(body["message_id"]))
			mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true}`))
	}))
	defer server.Close()
	sent := func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), receipts...)
	}

	db := testutil.SetupTestDB(t)
	organization := testutil.CreateTestOrganization(t, db)
	adminRole := testutil.CreateAdminRole(t, db, organization.ID)
	reader := testutil.CreateTestUser(t, db, organization.ID, testutil.WithRoleID(&adminRole.ID))
	account := testutil.CreateTestWhatsAppAccount(t, db, organization.ID)
	require.NoError(t, db.Model(account).Update("auto_read_receipt", true).Error)
	contact := testutil.CreateTestContactWith(t, db, organization.ID, testutil.WithContactAccount(account.Name))
	conversation := createLegacyChatConversation(t, db, account, contact.ID)
	app := &App{DB: db, Log: testutil.NopLogger(), WhatsApp: whatsapp.NewWithBaseURL(testutil.NopLogger(), server.URL)}

	// Positive control: an automatic read with Auto Read Receipt on sends a
	// receipt for the message the reader saw.
	base := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	seen := createLegacyChatMessage(t, db, organization.ID, account.Name, contact.ID, conversation.ID, uuid.New(), base)
	markLegacyChatVisible(t, app, organization.ID, reader.ID, contact.ID, seen.ID, fasthttp.StatusOK)
	app.wg.Wait()
	require.Equal(t, []string{seen.WhatsAppMessageID}, sent())

	// A manual read marks the new messages read but nobody opened them, so the
	// customer gets no blue ticks.
	unseen := createLegacyChatMessage(t, db, organization.ID, account.Name, contact.ID, conversation.ID, uuid.New(), base.Add(time.Second))
	_, status := laneDMarkRead(t, app, organization.ID, reader.ID, contact.ID, map[string]any{"mark": "read"})
	require.Equal(t, fasthttp.StatusOK, status)
	app.wg.Wait()
	require.NoError(t, db.First(&unseen, "id = ?", unseen.ID).Error)
	assert.Equal(t, models.MessageStatusRead, unseen.Status)
	assert.Equal(t, []string{seen.WhatsAppMessageID}, sent(), "manual read sent a read receipt")
}

func TestLaneDContactEditKeepsTheStoredManualMark(t *testing.T) {
	db := testutil.SetupTestDB(t)
	organization := testutil.CreateTestOrganization(t, db)
	adminRole := testutil.CreateAdminRole(t, db, organization.ID)
	admin := testutil.CreateTestUser(t, db, organization.ID, testutil.WithRoleID(&adminRole.ID))
	contact := testutil.CreateTestContact(t, db, organization.ID)
	hub, client := laneDHub(t, organization.ID, admin.ID)
	app := &App{DB: db, Log: testutil.NopLogger(), WSHub: hub}

	update := func(body map[string]any) (ContactResponse, int) {
		t.Helper()
		request := testutil.NewJSONRequest(t, body)
		testutil.SetAuthContext(request, organization.ID, admin.ID)
		testutil.SetPathParam(request, "id", contact.ID.String())
		require.NoError(t, app.UpdateContact(request))
		status := testutil.GetResponseStatusCode(request)
		var envelope struct {
			Data ContactResponse `json:"data"`
		}
		if status == fasthttp.StatusOK {
			require.NoError(t, json.Unmarshal(testutil.GetResponseBody(request), &envelope))
		}
		return envelope.Data, status
	}
	storedMetadata := func() models.JSONB {
		t.Helper()
		var stored models.Contact
		require.NoError(t, db.First(&stored, "id = ?", contact.ID).Error)
		return stored.Metadata
	}

	// The edit form never sees the mark, so its metadata lacks it: keep it.
	laneDSetManualUnread(t, db, contact, models.JSONB{"source": "import"})
	response, status := update(map[string]any{"metadata": map[string]any{"source": "crm"}})
	require.Equal(t, fasthttp.StatusOK, status)
	assert.True(t, response.ManualUnread)
	assert.Equal(t, map[string]any{"source": "crm"}, response.Metadata)
	assert.Equal(t, models.JSONB{"source": "crm", contactManualUnreadKey: true}, storedMetadata())

	// A client cannot set the mark through an edit...
	require.NoError(t, db.Model(&models.Contact{}).Where("id = ?", contact.ID).
		Update("metadata", models.JSONB{"source": "crm"}).Error)
	response, status = update(map[string]any{"metadata": map[string]any{"source": "crm2", contactManualUnreadKey: true}})
	require.Equal(t, fasthttp.StatusOK, status)
	assert.False(t, response.ManualUnread)
	assert.Equal(t, models.JSONB{"source": "crm2"}, storedMetadata())

	// ...nor clear it by sending false.
	laneDSetManualUnread(t, db, contact, models.JSONB{"source": "crm2"})
	_, status = update(map[string]any{"metadata": map[string]any{"source": "crm3", contactManualUnreadKey: false}})
	require.Equal(t, fasthttp.StatusOK, status)
	assert.Equal(t, models.JSONB{"source": "crm3", contactManualUnreadKey: true}, storedMetadata())
	laneDExpectNoContactUpdate(t, client, 200*time.Millisecond)

	// An assignment through the edit follows the membership rule and is
	// broadcast.
	removed := testutil.CreateTestUser(t, db, organization.ID)
	require.NoError(t, db.Where("user_id = ? AND organization_id = ?", removed.ID, organization.ID).
		Delete(&models.UserOrganization{}).Error)
	_, status = update(map[string]any{"assigned_user_id": removed.ID.String()})
	assert.Equal(t, fasthttp.StatusBadRequest, status)
	teammate := testutil.CreateTestUser(t, db, organization.ID)
	response, status = update(map[string]any{"assigned_user_id": teammate.ID.String()})
	require.Equal(t, fasthttp.StatusOK, status)
	require.NotNil(t, response.AssignedUserID)
	assert.Equal(t, teammate.ID, *response.AssignedUserID)
	laneDExpectContactUpdate(t, client, contact.ID, "assignment")
}

func TestLaneDCustomActionContextOmitsTheManualMark(t *testing.T) {
	marked := models.Contact{Metadata: models.JSONB{contactManualUnreadKey: true, "plan": "gold"}}
	context := buildActionContext(marked, models.User{}, models.Organization{})
	contactContext := context["contact"].(map[string]any)
	assert.Equal(t, models.JSONB{"plan": "gold"}, contactContext["metadata"])
	assert.Equal(t, true, marked.Metadata[contactManualUnreadKey], "the stored map is not mutated")

	plain := models.Contact{Metadata: models.JSONB{"plan": "gold"}}
	context = buildActionContext(plain, models.User{}, models.Organization{})
	assert.Equal(t, models.JSONB{"plan": "gold"}, context["contact"].(map[string]any)["metadata"])
}

func TestLaneDContactListStateDegradesInsideASavepoint(t *testing.T) {
	db := testutil.SetupTestDB(t)
	organization := testutil.CreateTestOrganization(t, db)
	adminRole := testutil.CreateAdminRole(t, db, organization.ID)
	reader := testutil.CreateTestUser(t, db, organization.ID, testutil.WithRoleID(&adminRole.ID))
	account := testutil.CreateTestWhatsAppAccount(t, db, organization.ID)
	contact := testutil.CreateTestContactWith(t, db, organization.ID, testutil.WithContactAccount(account.Name))
	laneDCreateMessage(t, db, organization.ID, account.Name, contact.ID, models.DirectionIncoming, models.MessageStatusDelivered)
	laneDCreateTransfer(t, db, contact, account.Name, &reader.ID, models.TransferStatusActive)

	// Another session holds agent_transfers exclusively, so the transfer
	// lookup inside the savepoint fails on lock_timeout (an SQL error).
	locker := db.Begin()
	require.NoError(t, locker.Error)
	defer locker.Rollback()
	require.NoError(t, locker.Exec("LOCK TABLE agent_transfers IN ACCESS EXCLUSIVE MODE").Error)

	root := &App{DB: db, Log: testutil.NopLogger()}
	var list laneDContactList
	status := 0
	// Mirror the Tenant wrapper: one request transaction owns the work.
	err := db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec("SET LOCAL lock_timeout = '200ms'").Error; err != nil {
			return err
		}
		scoped := root.scopedApp(tx, organization.ID)
		request := testutil.NewGETRequest(t)
		testutil.SetAuthContext(request, organization.ID, reader.ID)
		if err := scoped.ListContacts(request); err != nil {
			return err
		}
		status = testutil.GetResponseStatusCode(request)
		var envelope struct {
			Data laneDContactList `json:"data"`
		}
		if err := json.Unmarshal(testutil.GetResponseBody(request), &envelope); err != nil {
			return err
		}
		list = envelope.Data
		// The request transaction is still usable after the failed lookup.
		return tx.Exec("SELECT 1").Error
	})
	require.NoError(t, err, "the request transaction must survive a failed optional lookup")
	require.Equal(t, fasthttp.StatusOK, status)
	row := laneDContactByID(t, list, contact.ID)
	assert.Equal(t, 1, row.UnreadCount, "the unread count read before the failure is kept")
	assert.Nil(t, row.TransferAgentID, "the failed lookup degrades to no transfer agent")
}
