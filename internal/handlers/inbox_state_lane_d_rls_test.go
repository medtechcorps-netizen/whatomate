package handlers

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	channelapi "github.com/shridarpatil/whatomate/internal/channel"
	"github.com/shridarpatil/whatomate/internal/config"
	"github.com/shridarpatil/whatomate/internal/database"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// laneDCreateUnlinkedMessage stores an incoming legacy message that the inbox
// mirror has not linked to a conversation yet.
func laneDCreateUnlinkedMessage(
	t *testing.T,
	db *gorm.DB,
	organizationID uuid.UUID,
	accountName string,
	contactID uuid.UUID,
	createdAt time.Time,
) models.Message {
	t.Helper()
	message := models.Message{
		BaseModel:         models.BaseModel{ID: uuid.New(), CreatedAt: createdAt, UpdatedAt: createdAt},
		OrganizationID:    organizationID,
		WhatsAppAccount:   accountName,
		ContactID:         contactID,
		ConversationID:    "lane-d-unlinked-" + uuid.NewString(),
		WhatsAppMessageID: "wamid.lane-d-unlinked." + uuid.NewString(),
		Direction:         models.DirectionIncoming,
		MessageType:       models.MessageTypeText,
		Content:           "unlinked legacy message",
		Status:            models.MessageStatusDelivered,
		Metadata:          models.JSONB{},
	}
	require.NoError(t, db.Create(&message).Error)
	require.Nil(t, message.InboxConversationID)
	return message
}

// laneDLockedTables names the tables among contacts and inbox_conversations
// whose rows a statement locks: an explicit row-locking SELECT (including
// joined tables and FOR ... OF), or an UPDATE of the table.
func laneDLockedTables(statement string) []string {
	rowLock := regexp.MustCompile(`(?i)\bFOR\s+(NO\s+KEY\s+UPDATE|KEY\s+SHARE|UPDATE|SHARE)\b`)
	upper := strings.ToUpper(strings.TrimSpace(statement))
	var tables []string
	for _, table := range []string{"contacts", "inbox_conversations"} {
		name := strings.ToUpper(table)
		quoted := `"` + name + `"`
		if strings.HasPrefix(upper, "UPDATE "+quoted) || strings.HasPrefix(upper, "UPDATE "+name+" ") {
			tables = append(tables, table)
			continue
		}
		if !rowLock.MatchString(statement) {
			continue
		}
		for _, marker := range []string{
			"FROM " + quoted, "FROM " + name + " ",
			"JOIN " + quoted, "JOIN " + name + " ",
			" OF " + quoted, " OF " + name,
		} {
			if strings.Contains(upper, marker) {
				tables = append(tables, table)
				break
			}
		}
	}
	return tables
}

// The manual read of a patient on two numbers runs in the production request
// shape: RLS on and one tenant transaction for the whole request. The newest
// message on each number is not linked to a conversation yet, which used to
// make the manual read repair the mirror per message (conversation, then
// contact, per number) and so lock the second conversation after a contact.
// It is not parallel: applying and removing RLS policies changes shared schema.
func TestLaneDManualReadLockOrderWithRuntimeRoleRLS(t *testing.T) {
	adminDB := testutil.SetupTestDB(t)
	testutil.TruncateTables(adminDB)
	runtimeRole := "rereply_laned_" + uuid.NewString()[:8]
	runtimePassword := "synthetic" + uuid.NewString()[:8]
	require.NoError(t, adminDB.Exec(fmt.Sprintf(
		"CREATE ROLE %s LOGIN PASSWORD '%s' NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT NOBYPASSRLS",
		runtimeRole,
		runtimePassword,
	)).Error)
	t.Cleanup(func() {
		// Best-effort so a test failure is not hidden by teardown.
		_ = database.RemoveTenantRLS(adminDB)
		_ = adminDB.Exec("DROP OWNED BY " + runtimeRole).Error
		_ = adminDB.Exec("DROP ROLE IF EXISTS " + runtimeRole).Error
		testutil.TruncateTables(adminDB)
	})

	reseller := testutil.CreateTestReseller(t, adminDB)
	organization := testutil.CreateTestOrganizationForReseller(t, adminDB, reseller.ID)
	adminRole := testutil.CreateAdminRole(t, adminDB, organization.ID)
	reader := testutil.CreateTestUser(t, adminDB, organization.ID, testutil.WithRoleID(&adminRole.ID))
	firstAccount := testutil.CreateTestWhatsAppAccount(t, adminDB, organization.ID)
	secondAccount := testutil.CreateTestWhatsAppAccount(t, adminDB, organization.ID)
	contact := testutil.CreateTestContactWith(t, adminDB, organization.ID, testutil.WithContactAccount(firstAccount.Name))
	firstConversation := createLegacyChatConversation(t, adminDB, firstAccount, contact.ID)
	secondConversation := createLegacyChatConversation(t, adminDB, secondAccount, contact.ID)
	base := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	firstLinked := createLegacyChatMessage(t, adminDB, organization.ID, firstAccount.Name, contact.ID, firstConversation.ID, uuid.New(), base)
	secondLinked := createLegacyChatMessage(t, adminDB, organization.ID, secondAccount.Name, contact.ID, secondConversation.ID, uuid.New(), base.Add(time.Second))
	firstUnlinked := laneDCreateUnlinkedMessage(t, adminDB, organization.ID, firstAccount.Name, contact.ID, base.Add(2*time.Second))
	secondUnlinked := laneDCreateUnlinkedMessage(t, adminDB, organization.ID, secondAccount.Name, contact.ID, base.Add(3*time.Second))
	laneDSetManualUnread(t, adminDB, contact, nil)

	require.NoError(t, database.ApplyTenantRLS(adminDB, runtimeRole))
	runtimeDB := openRuntimeRoleTestDB(t, runtimeRole, runtimePassword)
	require.NoError(t, database.VerifyTenantRLS(runtimeDB, runtimeRole))
	recorder := &laneDSQLRecorder{Interface: logger.Discard}
	app := &App{
		DB:  runtimeDB.Session(&gorm.Session{Logger: recorder}),
		Log: testutil.NopLogger(),
		Config: &config.Config{
			App: config.AppConfig{EncryptionKey: "test-encryption-key-32-bytes-long"},
		},
	}
	app.Config.Database.RLSEnabled = true
	app.Config.Database.RuntimeRole = runtimeRole

	request := testutil.NewJSONRequest(t, map[string]any{"mark": "read"})
	testutil.SetAuthContext(request, organization.ID, reader.ID)
	testutil.SetPathParam(request, "id", contact.ID.String())
	require.NoError(t, app.Tenant((*App).MarkContactRead)(request))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(request), string(testutil.GetResponseBody(request)))

	for _, id := range []uuid.UUID{firstLinked.ID, secondLinked.ID, firstUnlinked.ID, secondUnlinked.ID} {
		var message models.Message
		require.NoError(t, adminDB.First(&message, "id = ?", id).Error)
		assert.Equal(t, models.MessageStatusRead, message.Status, "message %s", id)
	}
	firstCursor := loadConversationReadCursor(t, adminDB, organization.ID, reader.ID, firstConversation.ID)
	require.NotNil(t, firstCursor.LastReadMessageID)
	assert.Equal(t, firstLinked.ID, *firstCursor.LastReadMessageID)
	secondCursor := loadConversationReadCursor(t, adminDB, organization.ID, reader.ID, secondConversation.ID)
	require.NotNil(t, secondCursor.LastReadMessageID)
	assert.Equal(t, secondLinked.ID, *secondCursor.LastReadMessageID)
	assert.False(t, laneDManualUnreadStored(t, adminDB, contact.ID))

	recorder.mu.Lock()
	statements := append([]string(nil), recorder.statements...)
	recorder.mu.Unlock()
	conversationIDs := []string{firstConversation.ID.String(), secondConversation.ID.String()}
	sort.Strings(conversationIDs)
	contactLock := ""
	var firstLocked []string
	seen := map[string]bool{}
	conversationLocks := 0
	for _, statement := range statements {
		for _, table := range laneDLockedTables(statement) {
			switch table {
			case "contacts":
				if contactLock == "" {
					contactLock = statement
				}
			case "inbox_conversations":
				conversationLocks++
				require.Empty(t, contactLock,
					"inbox_conversations locked after a contacts lock\ncontact lock: %s\nconversation lock: %s",
					contactLock, statement)
				var present []string
				for _, id := range conversationIDs {
					if strings.Contains(statement, id) {
						present = append(present, id)
					}
				}
				if len(present) > 1 {
					assert.Contains(t, strings.ToUpper(statement), "ORDER BY", "a multi-row conversation lock must be ordered")
				}
				for _, id := range present {
					if !seen[id] {
						seen[id] = true
						firstLocked = append(firstLocked, id)
					}
				}
			}
		}
	}
	require.NotEmpty(t, contactLock, "the shared read locks the contact")
	require.Positive(t, conversationLocks)
	assert.Equal(t, conversationIDs, firstLocked, "conversations must be first locked in id order")
}

// A legacy mirror that cannot complete (here, a shadow inbox account bound to
// another WhatsApp account) used to fail every manual read of an unlinked
// message with a 500. The manual read no longer repairs the mirror.
func TestLaneDManualReadSucceedsWhenTheLegacyMirrorFails(t *testing.T) {
	db := testutil.SetupTestDB(t)
	organization := testutil.CreateTestOrganization(t, db)
	adminRole := testutil.CreateAdminRole(t, db, organization.ID)
	reader := testutil.CreateTestUser(t, db, organization.ID, testutil.WithRoleID(&adminRole.ID))
	account := testutil.CreateTestWhatsAppAccount(t, db, organization.ID)
	require.NoError(t, db.Create(&models.ChannelAccount{
		BaseModel:         models.BaseModel{ID: uuid.New()},
		OrganizationID:    organization.ID,
		Channel:           models.ChannelWhatsApp,
		Provider:          channelapi.LegacyMetaProvider,
		Name:              account.Name,
		ExternalAccountID: "legacy-account:" + account.ID.String(),
		Status:            models.ChannelAccountStatusActive,
		Capabilities:      models.JSONB{},
		Config:            models.JSONB{"legacy_read_only": true},
		Metadata:          models.JSONB{"legacy_account_id": uuid.NewString()},
	}).Error)
	contact := testutil.CreateTestContactWith(t, db, organization.ID, testutil.WithContactAccount(account.Name))
	unlinked := laneDCreateUnlinkedMessage(t, db, organization.ID, account.Name, contact.ID, time.Now().UTC().Add(-time.Minute).Truncate(time.Microsecond))
	// The mirror really fails for this message.
	require.Error(t, persistLegacyWhatsAppMessageMirror(db, account, unlinked.ID))
	laneDSetManualUnread(t, db, contact, nil)

	app := &App{DB: db, Log: testutil.NopLogger()}
	payload, status := laneDMarkRead(t, app, organization.ID, reader.ID, contact.ID, map[string]any{"mark": "read"})
	require.Equal(t, fasthttp.StatusOK, status)
	assert.Equal(t, false, payload["cursor_synced"], "nothing is linked, so there is no inbox cursor to advance")
	assert.Equal(t, false, payload["manual_unread"])
	require.NoError(t, db.First(&unlinked, "id = ?", unlinked.ID).Error)
	assert.Equal(t, models.MessageStatusRead, unlinked.Status)
	assert.Nil(t, unlinked.InboxConversationID, "the manual read does not repair the mirror")
	assert.False(t, laneDManualUnreadStored(t, db, contact.ID))
}
