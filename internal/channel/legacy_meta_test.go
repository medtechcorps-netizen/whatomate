package channel_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
	channelapi "github.com/shridarpatil/whatomate/internal/channel"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestLegacyMetaMirrorIsTenantScopedIdempotentAndDeliveryNeutral(t *testing.T) {
	db := testutil.SetupTestDB(t)

	orgA := createLegacyMetaTestOrganization(t, db, "mirror-a")
	orgB := createLegacyMetaTestOrganization(t, db, "mirror-b")
	accountA := createLegacyMetaTestAccount(t, db, orgA.ID, "Main A")
	accountB := createLegacyMetaTestAccount(t, db, orgB.ID, "Main B")
	contactA := createLegacyMetaTestContact(t, db, orgA.ID, accountA.Name, "601100000001", false)
	contactB := createLegacyMetaTestContact(t, db, orgB.ID, accountB.Name, "601100000001", false)
	messageA := createLegacyMetaTestMessage(
		t,
		db,
		orgA.ID,
		accountA.Name,
		contactA.ID,
		models.DirectionIncoming,
		"tenant A",
	)
	messageB := createLegacyMetaTestMessage(
		t,
		db,
		orgB.ID,
		accountB.Name,
		contactB.ID,
		models.DirectionIncoming,
		"tenant B",
	)

	resultA, err := channelapi.MirrorLegacyWhatsAppMessage(
		db,
		legacyMetaRef(accountA),
		messageA.ID,
	)
	require.NoError(t, err)
	assert.True(t, resultA.Linked)
	resultB, err := channelapi.MirrorLegacyWhatsAppMessage(
		db,
		legacyMetaRef(accountB),
		messageB.ID,
	)
	require.NoError(t, err)
	assert.True(t, resultB.Linked)
	assert.NotEqual(t, resultA.ChannelAccountID, resultB.ChannelAccountID)
	assert.NotEqual(t, resultA.ConversationID, resultB.ConversationID)

	replayed, err := channelapi.MirrorLegacyWhatsAppMessage(
		db,
		legacyMetaRef(accountA),
		messageA.ID,
	)
	require.NoError(t, err)
	assert.False(t, replayed.Linked)
	assert.Equal(t, resultA.ConversationID, replayed.ConversationID)

	var linkedMessage models.Message
	require.NoError(t, db.Where("id = ? AND organization_id = ?", messageA.ID, orgA.ID).
		First(&linkedMessage).Error)
	require.NotNil(t, linkedMessage.InboxConversationID)
	assert.Equal(t, resultA.ConversationID, *linkedMessage.InboxConversationID)

	var conversation models.InboxConversation
	require.NoError(t, db.Where("id = ? AND organization_id = ?", resultA.ConversationID, orgA.ID).
		First(&conversation).Error)
	assert.Equal(t, contactA.ID, conversation.ContactID)
	assert.Equal(t, models.ChannelWhatsApp, conversation.Channel)
	assert.Equal(t, 1, conversation.UnreadCount)
	assert.Equal(t, "tenant A", conversation.LastMessagePreview)

	var account models.ChannelAccount
	require.NoError(t, db.Where("id = ? AND organization_id = ?", resultA.ChannelAccountID, orgA.ID).
		First(&account).Error)
	assert.Equal(t, channelapi.LegacyMetaProvider, account.Provider)
	assert.Equal(t, false, account.Config["outbound_enabled"])
	assert.Equal(t, true, account.Config["legacy_read_only"])

	var credentials int64
	require.NoError(t, db.Model(&models.ChannelCredential{}).
		Where("organization_id = ? AND channel_account_id = ?", orgA.ID, account.ID).
		Count(&credentials).Error)
	assert.Zero(t, credentials)
	var outboxJobs int64
	require.NoError(t, db.Model(&models.OutboxJob{}).
		Where("organization_id = ?", orgA.ID).
		Count(&outboxJobs).Error)
	assert.Zero(t, outboxJobs, "mirroring must never enqueue a second delivery")
	var messages int64
	require.NoError(t, db.Model(&models.Message{}).
		Where("organization_id = ?", orgA.ID).
		Count(&messages).Error)
	assert.EqualValues(t, 1, messages, "mirroring must reuse the legacy message envelope")

	require.NoError(t, channelapi.MarkLegacyWhatsAppConversationRead(db, orgA.ID, contactA.ID))
	require.NoError(t, db.Where("id = ?", resultA.ConversationID).First(&conversation).Error)
	assert.Zero(t, conversation.UnreadCount)
	var otherConversation models.InboxConversation
	require.NoError(t, db.Where("id = ?", resultB.ConversationID).First(&otherConversation).Error)
	assert.Equal(t, 1, otherConversation.UnreadCount, "read mirroring must not cross tenants")
}

func TestLegacyMetaBackfillDoesNotCopyCredentialsAndIsIdempotent(t *testing.T) {
	db := testutil.SetupTestDB(t)

	org := createLegacyMetaTestOrganization(t, db, "backfill")
	account := createLegacyMetaTestAccount(t, db, org.ID, "Backfill")
	contact := createLegacyMetaTestContact(t, db, org.ID, account.Name, "601100000002", true)
	createLegacyMetaTestMessage(
		t,
		db,
		org.ID,
		account.Name,
		contact.ID,
		models.DirectionIncoming,
		"hello",
	)
	createLegacyMetaTestMessage(
		t,
		db,
		org.ID,
		account.Name,
		contact.ID,
		models.DirectionOutgoing,
		"welcome",
	)

	stats, err := channelapi.BackfillLegacyWhatsAppInbox(db, 1)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, stats.Accounts, 1)
	assert.Equal(t, 2, stats.Messages)
	assert.Equal(t, 2, stats.Linked)

	var shadow models.ChannelAccount
	require.NoError(t, db.Where(
		"organization_id = ? AND channel = ? AND provider = ?",
		org.ID,
		models.ChannelWhatsApp,
		channelapi.LegacyMetaProvider,
	).First(&shadow).Error)
	serialized, err := json.Marshal(shadow)
	require.NoError(t, err)
	shadowJSON := string(serialized)
	for _, secret := range []string{
		account.AccessToken,
		account.AppSecret,
		account.Pin,
		account.WebhookVerifyToken,
	} {
		assert.NotContains(t, shadowJSON, secret)
	}
	assert.NotContains(t, strings.ToLower(shadowJSON), "access_token")
	assert.NotContains(t, strings.ToLower(shadowJSON), "app_secret")

	var credentialCount int64
	require.NoError(t, db.Model(&models.ChannelCredential{}).
		Where("organization_id = ? AND channel_account_id = ?", org.ID, shadow.ID).
		Count(&credentialCount).Error)
	assert.Zero(t, credentialCount)
	var linkedCount int64
	require.NoError(t, db.Model(&models.Message{}).
		Where("organization_id = ? AND inbox_conversation_id IS NOT NULL", org.ID).
		Count(&linkedCount).Error)
	assert.EqualValues(t, 2, linkedCount)

	replayed, err := channelapi.BackfillLegacyWhatsAppInbox(db, 100)
	require.NoError(t, err)
	assert.Zero(t, replayed.Messages)
	assert.Zero(t, replayed.Linked)
}

func TestLegacyMetaMirrorRejectsAccountAndExistingLinkConflicts(t *testing.T) {
	db := testutil.SetupTestDB(t)

	org := createLegacyMetaTestOrganization(t, db, "conflict")
	account := createLegacyMetaTestAccount(t, db, org.ID, "Expected")
	contact := createLegacyMetaTestContact(t, db, org.ID, account.Name, "601100000003", true)
	message := createLegacyMetaTestMessage(
		t,
		db,
		org.ID,
		account.Name,
		contact.ID,
		models.DirectionIncoming,
		"conflict",
	)

	wrongAccount := legacyMetaRef(account)
	wrongAccount.Name = "Another account"
	_, err := channelapi.MirrorLegacyWhatsAppMessage(db, wrongAccount, message.ID)
	require.ErrorIs(t, err, channelapi.ErrLegacyMetaBridgeConflict)

	first, err := channelapi.MirrorLegacyWhatsAppMessage(db, legacyMetaRef(account), message.ID)
	require.NoError(t, err)
	secondConversation := models.InboxConversation{
		OrganizationID:         org.ID,
		ChannelAccountID:       first.ChannelAccountID,
		ContactID:              contact.ID,
		Channel:                models.ChannelWhatsApp,
		ExternalConversationID: "manual-conflict:" + uuid.NewString(),
		Status:                 models.InboxConversationStatusOpen,
		OpenedAt:               message.CreatedAt,
		Config:                 models.JSONB{},
		Metadata:               models.JSONB{},
	}
	require.NoError(t, db.Create(&secondConversation).Error)
	require.NoError(t, db.Model(&models.Message{}).
		Where("id = ? AND organization_id = ?", message.ID, org.ID).
		Update("inbox_conversation_id", secondConversation.ID).Error)

	_, err = channelapi.MirrorLegacyWhatsAppMessage(db, legacyMetaRef(account), message.ID)
	require.ErrorIs(t, err, channelapi.ErrLegacyMetaBridgeConflict)
}

func legacyMetaRef(account models.WhatsAppAccount) channelapi.LegacyMetaAccountRef {
	return channelapi.LegacyMetaAccountRef{
		ID:             account.ID,
		OrganizationID: account.OrganizationID,
		Name:           account.Name,
		Status:         account.Status,
	}
}

func createLegacyMetaTestOrganization(
	t *testing.T,
	db *gorm.DB,
	suffix string,
) models.Organization {
	t.Helper()
	organization := models.Organization{
		Name:     "Legacy Meta " + suffix,
		Slug:     "legacy-meta-" + suffix + "-" + uuid.NewString(),
		Settings: models.JSONB{},
	}
	require.NoError(t, db.Create(&organization).Error)
	return organization
}

func createLegacyMetaTestAccount(
	t *testing.T,
	db *gorm.DB,
	organizationID uuid.UUID,
	name string,
) models.WhatsAppAccount {
	t.Helper()
	unique := uuid.NewString()
	account := models.WhatsAppAccount{
		OrganizationID:     organizationID,
		Name:               name,
		AppID:              "app-" + unique,
		PhoneID:            "phone-" + unique,
		BusinessID:         "business-" + unique,
		AccessToken:        "secret-access-" + unique,
		AppSecret:          "secret-app-" + unique,
		WebhookVerifyToken: "secret-webhook-" + unique,
		APIVersion:         "v21.0",
		Status:             "active",
		Pin:                "secret-pin-" + unique,
	}
	require.NoError(t, db.Create(&account).Error)
	return account
}

func createLegacyMetaTestContact(
	t *testing.T,
	db *gorm.DB,
	organizationID uuid.UUID,
	accountName string,
	phone string,
	isRead bool,
) models.Contact {
	t.Helper()
	contact := models.Contact{
		OrganizationID:  organizationID,
		PhoneNumber:     phone,
		ProfileName:     "Test Contact",
		WhatsAppAccount: accountName,
		IsRead:          true,
		Tags:            models.JSONBArray{},
		Metadata:        models.JSONB{},
	}
	require.NoError(t, db.Create(&contact).Error)
	require.NoError(t, db.Model(&models.Contact{}).
		Where("id = ? AND organization_id = ?", contact.ID, organizationID).
		Update("is_read", isRead).Error)
	contact.IsRead = isRead
	return contact
}

func createLegacyMetaTestMessage(
	t *testing.T,
	db *gorm.DB,
	organizationID uuid.UUID,
	accountName string,
	contactID uuid.UUID,
	direction models.Direction,
	content string,
) models.Message {
	t.Helper()
	status := models.MessageStatusReceived
	if direction == models.DirectionOutgoing {
		status = models.MessageStatusSent
	}
	message := models.Message{
		OrganizationID:  organizationID,
		WhatsAppAccount: accountName,
		ContactID:       contactID,
		Direction:       direction,
		MessageType:     models.MessageTypeText,
		Content:         content,
		Status:          status,
		Metadata:        models.JSONB{},
	}
	require.NoError(t, db.Create(&message).Error)
	return message
}
