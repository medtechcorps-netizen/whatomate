package handlers

import (
	"context"
	"encoding/hex"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/shridarpatil/whatomate/internal/config"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
	"github.com/zerodha/fastglue"
	"gorm.io/gorm"
)

// These tests use the repository's disposable PostgreSQL fixture and skip
// unless TEST_DATABASE_URL is explicitly supplied. They start no containers.
// Vocabulary rows here are synthetic test setup, never a runtime rollout.
type chatExportFixture struct {
	app              *App
	org              *models.Organization
	user             *models.User
	role             *models.CustomRole
	account          *models.WhatsAppAccount
	contact          *models.Contact
	exportPermission models.Permission
}

func chatExportGrant(t *testing.T, db *gorm.DB, role *models.CustomRole, resource, action string) models.Permission {
	t.Helper()
	permission := models.Permission{Resource: resource, Action: action}
	require.NoError(t, db.Where("resource = ? AND action = ?", resource, action).FirstOrCreate(&permission).Error)
	require.NoError(t, db.Model(role).Association("Permissions").Append(&permission))
	return permission
}

func newChatExportFixture(t *testing.T) chatExportFixture {
	t.Helper()
	db := testutil.SetupTestDB(t)
	org := testutil.CreateTestOrganization(t, db)
	role := &models.CustomRole{BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: org.ID, Name: "Synthetic export agent"}
	require.NoError(t, db.Create(role).Error)
	permission := chatExportGrant(t, db, role, "chat", "export")
	chatExportGrant(t, db, role, "chat", "read")
	user := testutil.CreateTestUser(t, db, org.ID, testutil.WithRoleID(&role.ID))
	account := testutil.CreateTestWhatsAppAccount(t, db, org.ID)
	contact := testutil.CreateTestContactWith(t, db, org.ID, testutil.WithContactAccount(account.Name))
	require.NoError(t, db.Model(contact).Update("assigned_user_id", user.ID).Error)
	return chatExportFixture{app: &App{DB: db, Config: &config.Config{}, Log: testutil.NopLogger()}, org: org, user: user, role: role, account: account, contact: contact, exportPermission: permission}
}

func (f chatExportFixture) message(t *testing.T, contact *models.Contact, account string, at time.Time) models.Message {
	t.Helper()
	message := models.Message{BaseModel: models.BaseModel{ID: uuid.New(), CreatedAt: at}, OrganizationID: contact.OrganizationID, ContactID: contact.ID, WhatsAppAccount: account, WhatsAppMessageID: uuid.NewString(), IngestedAt: &at, Direction: models.DirectionIncoming, MessageType: models.MessageTypeText, Content: "Synthetic Unicode 您好\nline two", Status: models.MessageStatusDelivered}
	require.NoError(t, f.app.DB.Create(&message).Error)
	return message
}

func (f chatExportFixture) snapshot(t *testing.T, req chatExportRequest) (*chatExportArchive, chatExportAccess) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	var archive *chatExportArchive
	var access chatExportAccess
	err := f.app.chatExportReadTransaction(ctx, f.org.ID, true, func(scoped *App) error {
		var err error
		access, err = scoped.chatExportAccess(f.org.ID, f.user.ID)
		if err != nil {
			return err
		}
		archive, err = newChatExportArchive(time.Now().UTC(), req)
		if err != nil {
			return err
		}
		return scoped.buildChatExport(ctx, f.org.ID, f.user.ID, req, access, archive)
	})
	if archive != nil {
		t.Cleanup(archive.cleanup)
	}
	require.NoError(t, err)
	require.NoError(t, archive.finish())
	return archive, access
}

func TestChatExportPostgresSnapshotAllPagesTiesTenantAndAccount(t *testing.T) {
	f := newChatExportFixture(t)
	at := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	var expected []uuid.UUID
	for index := 0; index < 45; index++ {
		contact := testutil.CreateTestContactWith(t, f.app.DB, f.org.ID, testutil.WithContactAccount(f.account.Name))
		require.NoError(t, f.app.DB.Model(contact).Updates(map[string]any{"assigned_user_id": f.user.ID, "metadata": models.JSONB{"manual_unread": true}}).Error)
		for message := 0; message < 5; message++ {
			expected = append(expected, f.message(t, contact, f.account.Name, at).ID)
		}
	}
	otherOrg := testutil.CreateTestOrganization(t, f.app.DB)
	foreign := testutil.CreateTestContact(t, f.app.DB, otherOrg.ID)
	f.message(t, foreign, f.account.Name, at)
	otherAccount := testutil.CreateTestWhatsAppAccount(t, f.app.DB, f.org.ID)
	f.message(t, f.contact, otherAccount.Name, at)
	req := chatExportRequest{Scope: "all", Account: "whatsapp:" + hex.EncodeToString([]byte(f.account.Name))}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	var archive *chatExportArchive
	err := f.app.chatExportReadTransaction(ctx, f.org.ID, true, func(scoped *App) error {
		access, err := scoped.chatExportAccess(f.org.ID, f.user.ID)
		if err != nil {
			return err
		}
		first, err := scoped.chatExportPreview(f.org.ID, f.user.ID, req, access)
		if err != nil {
			return err
		}
		require.Equal(t, int64(225), first.Messages)
		require.Equal(t, int64(45), first.Conversations)
		require.Len(t, first.Rows, 40)
		require.NotEmpty(t, first.Next)
		page := req
		page.After = first.Next
		second, err := scoped.chatExportPreview(f.org.ID, f.user.ID, page, access)
		if err != nil {
			return err
		}
		require.Len(t, second.Rows, 5)
		require.Empty(t, second.Next)
		require.Equal(t, first.Messages, second.Messages)
		// Separate writer commits after the snapshot starts, with the exact same
		// provider/ingestion timestamp. It must not appear on a later keyset page.
		late := models.Message{BaseModel: models.BaseModel{ID: uuid.New(), CreatedAt: at}, OrganizationID: f.org.ID, ContactID: f.contact.ID, WhatsAppAccount: f.account.Name, IngestedAt: &at, Direction: models.DirectionIncoming, MessageType: models.MessageTypeText, Content: "arrived after snapshot", Status: models.MessageStatusDelivered}
		if err = f.app.DB.WithContext(ctx).Create(&late).Error; err != nil {
			return err
		}
		archive, err = newChatExportArchive(time.Now().UTC(), req)
		if err != nil {
			return err
		}
		return scoped.buildChatExport(ctx, f.org.ID, f.user.ID, req, access, archive)
	})
	if archive != nil {
		t.Cleanup(archive.cleanup)
	}
	require.NoError(t, err)
	require.ElementsMatch(t, expected, archive.IDs)
	require.Len(t, archive.IDs, 225)
	var read int64
	require.NoError(t, f.app.DB.Model(&models.Message{}).Where("id IN ? AND status = 'read'", expected).Count(&read).Error)
	require.Zero(t, read, "export must not acknowledge incoming messages")
	var untouched int64
	require.NoError(t, f.app.DB.Model(&models.Contact{}).Where("organization_id = ? AND metadata @> '{\"manual_unread\":true}'::jsonb", f.org.ID).Count(&untouched).Error)
	require.Equal(t, int64(45), untouched)
	forged := chatExportRequest{Scope: "selected", Selected: []string{"whatsapp:" + foreign.ID.String() + ":" + hex.EncodeToString([]byte(f.account.Name))}}
	_, err = f.app.chatExportPreview(f.org.ID, f.user.ID, forged, chatExportAccess{AllContacts: true})
	require.ErrorIs(t, err, errChatExportScopeChanged)
}

func TestChatExportPostgresExactSessionAndActiveTransferCutoff(t *testing.T) {
	f := newChatExportFixture(t)
	cutoff := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	settings := models.ChatbotSettings{OrganizationID: f.org.ID, AgentAssignment: models.AgentAssignmentConfig{CurrentConversationOnly: true}}
	require.NoError(t, f.app.DB.Create(&settings).Error)
	session := models.ChatbotSession{OrganizationID: f.org.ID, ContactID: f.contact.ID, WhatsAppAccount: f.account.Name, PhoneNumber: f.contact.PhoneNumber, StartedAt: cutoff.Add(-time.Hour)}
	require.NoError(t, f.app.DB.Create(&session).Error)
	transfer := models.AgentTransfer{OrganizationID: f.org.ID, ContactID: f.contact.ID, WhatsAppAccount: f.account.Name, PhoneNumber: f.contact.PhoneNumber, AgentID: &f.user.ID, Status: models.TransferStatusActive, TransferredAt: cutoff}
	require.NoError(t, f.app.DB.Create(&transfer).Error)
	require.NoError(t, f.app.DB.Model(f.contact).Update("assigned_user_id", nil).Error)
	f.message(t, f.contact, f.account.Name, cutoff.Add(-time.Microsecond))
	at := f.message(t, f.contact, f.account.Name, cutoff)
	after := f.message(t, f.contact, f.account.Name, cutoff.Add(time.Microsecond))
	archive, access := f.snapshot(t, chatExportRequest{Scope: "all"})
	require.True(t, access.CurrentConversationOnly)
	require.ElementsMatch(t, []uuid.UUID{at.ID, after.ID}, archive.IDs)
	require.NoError(t, f.app.DB.Model(&transfer).Update("status", "resumed").Error)
	require.ErrorIs(t, f.app.revalidateChatExport(t.Context(), f.org.ID, f.user.ID, chatExportRequest{Scope: "all"}, access, archive.IDs), errChatExportScopeChanged)
}

func TestChatExportPostgresFreshRevocationBeforeResponse(t *testing.T) {
	for _, mutation := range []string{"permission", "assignment", "membership", "new_session"} {
		t.Run(mutation, func(t *testing.T) {
			f := newChatExportFixture(t)
			at := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
			if mutation == "new_session" {
				require.NoError(t, f.app.DB.Create(&models.ChatbotSettings{OrganizationID: f.org.ID, AgentAssignment: models.AgentAssignmentConfig{CurrentConversationOnly: true}}).Error)
				require.NoError(t, f.app.DB.Create(&models.ChatbotSession{OrganizationID: f.org.ID, ContactID: f.contact.ID, WhatsAppAccount: f.account.Name, PhoneNumber: f.contact.PhoneNumber, StartedAt: at.Add(-time.Hour)}).Error)
			}
			f.message(t, f.contact, f.account.Name, at)
			archive, access := f.snapshot(t, chatExportRequest{Scope: "all"})
			require.Len(t, archive.IDs, 1)
			switch mutation {
			case "permission":
				require.NoError(t, f.app.DB.Model(f.role).Association("Permissions").Delete(&f.exportPermission))
			case "assignment":
				require.NoError(t, f.app.DB.Model(f.contact).Update("assigned_user_id", nil).Error)
			case "membership":
				require.NoError(t, f.app.DB.Where("organization_id = ? AND user_id = ?", f.org.ID, f.user.ID).Delete(&models.UserOrganization{}).Error)
			case "new_session":
				require.NoError(t, f.app.DB.Create(&models.ChatbotSession{OrganizationID: f.org.ID, ContactID: f.contact.ID, WhatsAppAccount: f.account.Name, PhoneNumber: f.contact.PhoneNumber, StartedAt: at.Add(time.Second)}).Error)
			}
			err := f.app.revalidateChatExport(t.Context(), f.org.ID, f.user.ID, chatExportRequest{Scope: "all"}, access, archive.IDs)
			require.Error(t, err)
			require.True(t, errors.Is(err, errChatExportDenied) || errors.Is(err, errChatExportScopeChanged))
		})
	}
}

func (f chatExportFixture) conversation(t *testing.T, contact *models.Contact, provider string) models.InboxConversation {
	t.Helper()
	account := models.ChannelAccount{OrganizationID: contact.OrganizationID, Channel: models.ChannelWhatsApp, Provider: provider, Name: "Synthetic inbox " + uuid.NewString(), ExternalAccountID: uuid.NewString(), Status: models.ChannelAccountStatusActive}
	if provider == "meta_legacy" {
		legacy := f.account
		if contact.OrganizationID != f.org.ID {
			legacy = testutil.CreateTestWhatsAppAccount(t, f.app.DB, contact.OrganizationID)
		}
		account.ExternalAccountID = "legacy-account:" + legacy.ID.String()
		account.Metadata = models.JSONB{"legacy_account_id": legacy.ID.String(), "legacy_account_name": legacy.Name}
	}
	require.NoError(t, f.app.DB.Where("organization_id = ? AND provider = ? AND external_account_id = ?", account.OrganizationID, provider, account.ExternalAccountID).FirstOrCreate(&account).Error)
	conversation := models.InboxConversation{OrganizationID: contact.OrganizationID, ChannelAccountID: account.ID, ContactID: contact.ID, Channel: models.ChannelWhatsApp, ExternalConversationID: uuid.NewString(), OpenedAt: time.Now().UTC()}
	require.NoError(t, f.app.DB.Create(&conversation).Error)
	return conversation
}

func TestChatExportPostgresBridgeDedupAndMismatchExclusion(t *testing.T) {
	f := newChatExportFixture(t)
	chatExportGrant(t, f.app.DB, f.role, "contacts", "read")
	chatExportGrant(t, f.app.DB, f.role, "conversations", "read")
	enableBookingCommerceTestEntitlement(t, f.app.DB, f.org.ID, f.user.ID, "omnichannel.enabled")
	at := time.Now().UTC().Add(-time.Hour)
	bridge := f.conversation(t, f.contact, "meta_legacy")
	legacy := f.message(t, f.contact, f.account.Name, at)
	require.NoError(t, f.app.DB.Model(&legacy).Update("inbox_conversation_id", bridge.ID).Error)
	native := f.conversation(t, f.contact, "synthetic_native")
	nativeMessage := f.message(t, f.contact, "", at)
	require.NoError(t, f.app.DB.Model(&nativeMessage).Update("inbox_conversation_id", native.ID).Error)
	wrongContact := testutil.CreateTestContact(t, f.app.DB, f.org.ID)
	wrongConversation := f.conversation(t, wrongContact, "meta_legacy")
	mismatch := f.message(t, f.contact, f.account.Name, at)
	require.NoError(t, f.app.DB.Model(&mismatch).Update("inbox_conversation_id", wrongConversation.ID).Error)
	// Incoming cross-organization links are rejected by the ingestion trigger.
	// Keep the foreign bridge valid so tenant exclusion is tested against a
	// persisted row rather than bypassing that database invariant.
	foreign := newChatExportFixture(t)
	foreignConversation := foreign.conversation(t, foreign.contact, "meta_legacy")
	foreignLink := foreign.message(t, foreign.contact, foreign.account.Name, at)
	require.NoError(t, f.app.DB.Model(&foreignLink).Update("inbox_conversation_id", foreignConversation.ID).Error)
	// A rejected cross-org relink must also leave the valid local bridge intact.
	err := f.app.DB.Model(&legacy).Update("inbox_conversation_id", foreignConversation.ID).Error
	require.ErrorContains(t, err, "SQLSTATE 23503")
	var persistedLegacy models.Message
	require.NoError(t, f.app.DB.First(&persistedLegacy, "id = ?", legacy.ID).Error)
	require.NotNil(t, persistedLegacy.InboxConversationID)
	require.Equal(t, bridge.ID, *persistedLegacy.InboxConversationID)
	wrongAccount := testutil.CreateTestWhatsAppAccount(t, f.app.DB, f.org.ID)
	wrongAccountLink := f.message(t, f.contact, wrongAccount.Name, at)
	require.NoError(t, f.app.DB.Model(&wrongAccountLink).Update("inbox_conversation_id", bridge.ID).Error)
	archive, access := f.snapshot(t, chatExportRequest{Scope: "all"})
	require.True(t, access.NativeInbox)
	require.ElementsMatch(t, []uuid.UUID{legacy.ID, nativeMessage.ID}, archive.IDs)
	// A retained role grant cannot reopen a suspended product, including when
	// that suspension happens after the archive snapshot has been built.
	require.NoError(t, f.app.DB.Model(&models.Subscription{}).Where("organization_id = ?", f.org.ID).Update("status", "paused").Error)
	require.ErrorIs(t, f.app.revalidateChatExport(t.Context(), f.org.ID, f.user.ID, chatExportRequest{Scope: "all"}, access, archive.IDs), errChatExportScopeChanged)
	legacyOnly, revoked := f.snapshot(t, chatExportRequest{Scope: "all"})
	require.False(t, revoked.NativeInbox)
	require.Equal(t, []uuid.UUID{legacy.ID}, legacyOnly.IDs)
}

func TestChatExportPostgresAdmissionReadOnlyAndMissingVocabulary(t *testing.T) {
	f := newChatExportFixture(t)
	f.app.Config.Database.RLSEnabled = true
	require.NoError(t, f.app.chatExportReadTransaction(t.Context(), f.org.ID, true, func(scoped *App) error {
		var tenant, readOnly, isolation string
		require.NoError(t, scoped.DB.Raw("SELECT current_setting('app.current_organization_id')").Scan(&tenant).Error)
		require.NoError(t, scoped.DB.Raw("SHOW transaction_read_only").Scan(&readOnly).Error)
		require.NoError(t, scoped.DB.Raw("SHOW transaction_isolation").Scan(&isolation).Error)
		require.Equal(t, f.org.ID.String(), tenant)
		require.Equal(t, "on", readOnly)
		require.Equal(t, "repeatable read", isolation)
		return nil
	}))
	// Delete only inside a rollback-only test transaction so another fixture
	// never observes vocabulary removal. Even superadmin cannot bypass absence.
	tx := f.app.DB.Begin()
	require.NoError(t, tx.Error)
	defer tx.Rollback()
	require.NoError(t, tx.Model(f.user).Update("is_super_admin", true).Error)
	require.NoError(t, tx.Delete(&f.exportPermission).Error)
	_, err := (&App{DB: tx, Log: testutil.NopLogger()}).chatExportAccess(f.org.ID, f.user.ID)
	require.ErrorIs(t, err, errChatExportUnavailable)
}

func TestChatExportPostgresExactWorkspaceNeverFallsBack(t *testing.T) {
	f := newChatExportFixture(t)
	foreign := testutil.CreateTestOrganization(t, f.app.DB)
	for _, header := range []string{"", "not-a-uuid", foreign.ID.String()} {
		r := testutil.NewJSONRequest(t, chatExportRequest{Scope: "all"})
		testutil.SetAuthContext(r, f.org.ID, f.user.ID)
		if header != "" {
			r.RequestCtx.Request.Header.Set("X-Organization-ID", header)
		}
		require.NoError(t, f.app.PreviewChatExport(r))
		require.Equal(t, fasthttp.StatusForbidden, r.RequestCtx.Response.StatusCode())
		require.False(t, r.RequestCtx.Response.IsBodyStream())
	}
	require.NoError(t, f.app.DB.Model(f.role).Association("Permissions").Delete(&f.exportPermission))
	r := testutil.NewGETRequest(t)
	testutil.SetAuthContext(r, f.org.ID, f.user.ID)
	r.RequestCtx.Request.Header.Set("X-Organization-ID", f.org.ID.String())
	require.NoError(t, f.app.GetChatExportCapabilities(r))
	var capability struct {
		Available bool `json:"available"`
	}
	testutil.ParseEnvelopeResponse(t, r, &capability)
	require.False(t, capability.Available)
}

func chatExportTestRedis(t *testing.T) *redis.Client {
	t.Helper()
	url := os.Getenv("TEST_REDIS_URL")
	if url == "" {
		t.Skip("TEST_REDIS_URL required for export coordination integration")
	}
	options, err := redis.ParseURL(url)
	require.NoError(t, err)
	client := redis.NewClient(options)
	t.Cleanup(func() { _ = client.Close() })
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	require.NoError(t, client.Ping(ctx).Err())
	return client
}

func TestChatExportPostgresRedisRetainsArchiveAndOwnerUntilClose(t *testing.T) {
	client := chatExportTestRedis(t)
	fixtures := []chatExportFixture{newChatExportFixture(t), newChatExportFixture(t), newChatExportFixture(t)}
	var responses []*fastglue.Request
	t.Cleanup(func() {
		for _, response := range responses {
			_ = response.RequestCtx.Response.CloseBodyStream()
		}
	})
	request := func(f chatExportFixture) *fastglue.Request {
		f.app.Redis = client
		r := testutil.NewJSONRequest(t, chatExportRequest{Scope: "all", RequestID: uuid.New()})
		testutil.SetAuthContext(r, f.org.ID, f.user.ID)
		r.RequestCtx.Request.Header.Set("X-Organization-ID", f.org.ID.String())
		require.NoError(t, f.app.DownloadChatExport(r))
		responses = append(responses, r)
		return r
	}
	for _, f := range fixtures {
		f.message(t, f.contact, f.account.Name, time.Now().UTC().Add(-time.Hour))
	}
	first := request(fixtures[0])
	require.Equal(t, fasthttp.StatusOK, first.RequestCtx.Response.StatusCode())
	require.True(t, first.RequestCtx.Response.IsBodyStream())
	require.Equal(t, 1, len(chatExportArchives))
	require.Zero(t, len(chatExportSlots), "no DB builder slot remains while streaming")
	owner := chatExportOwnerKey(fixtures[0].org.ID, fixtures[0].user.ID)
	exists, err := client.Exists(t.Context(), owner).Result()
	require.NoError(t, err)
	require.Equal(t, int64(1), exists)
	duplicate := request(fixtures[0])
	require.Equal(t, fasthttp.StatusTooManyRequests, duplicate.RequestCtx.Response.StatusCode())
	second := request(fixtures[1])
	require.Equal(t, fasthttp.StatusOK, second.RequestCtx.Response.StatusCode())
	require.Equal(t, 2, len(chatExportArchives))
	third := request(fixtures[2])
	require.Equal(t, fasthttp.StatusTooManyRequests, third.RequestCtx.Response.StatusCode())
	stream, ok := first.RequestCtx.Response.BodyStream().(*chatExportDownload)
	require.True(t, ok)
	archivePath := stream.archive.File.Name()
	require.NoError(t, first.RequestCtx.Response.CloseBodyStream())
	require.Equal(t, 1, len(chatExportArchives))
	_, err = os.Stat(archivePath)
	require.ErrorIs(t, err, os.ErrNotExist)
	exists, err = client.Exists(t.Context(), owner).Result()
	require.NoError(t, err)
	require.Zero(t, exists)
	fourth := request(fixtures[2])
	require.Equal(t, fasthttp.StatusOK, fourth.RequestCtx.Response.StatusCode())
}
