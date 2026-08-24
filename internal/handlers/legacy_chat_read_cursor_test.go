package handlers

import (
	"testing"
	"time"

	"github.com/google/uuid"
	channelapi "github.com/shridarpatil/whatomate/internal/channel"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
	"gorm.io/gorm"
)

func TestLegacyChatExplicitVisibleMessageAdvancesOnlyTheCurrentReader(t *testing.T) {
	db := testutil.SetupTestDB(t)
	organization := testutil.CreateTestOrganization(t, db)
	adminRole := testutil.CreateAdminRole(t, db, organization.ID)
	reader := testutil.CreateTestUser(t, db, organization.ID, testutil.WithRoleID(&adminRole.ID))
	secondReader := testutil.CreateTestUser(t, db, organization.ID, testutil.WithRoleID(&adminRole.ID))
	legacyAccount := testutil.CreateTestWhatsAppAccount(t, db, organization.ID)
	contact := testutil.CreateTestContactWith(
		t,
		db,
		organization.ID,
		testutil.WithContactAccount(legacyAccount.Name),
	)
	conversation := createLegacyChatConversation(t, db, legacyAccount, contact.ID)

	baseTime := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	first := createLegacyChatMessage(
		t,
		db,
		organization.ID,
		legacyAccount.Name,
		contact.ID,
		conversation.ID,
		uuid.MustParse("00000000-0000-0000-0000-000000000101"),
		baseTime,
	)
	second := createLegacyChatMessage(
		t,
		db,
		organization.ID,
		legacyAccount.Name,
		contact.ID,
		conversation.ID,
		uuid.MustParse("00000000-0000-0000-0000-000000000102"),
		baseTime.Add(time.Second),
	)
	third := createLegacyChatMessage(
		t,
		db,
		organization.ID,
		legacyAccount.Name,
		contact.ID,
		conversation.ID,
		uuid.MustParse("00000000-0000-0000-0000-000000000103"),
		baseTime.Add(time.Second),
	)

	app := &App{DB: db, Log: testutil.NopLogger()}
	request := testutil.NewGETRequest(t)
	testutil.SetAuthContext(request, organization.ID, reader.ID)
	testutil.SetPathParam(request, "id", contact.ID.String())
	testutil.SetQueryParam(request, "account", legacyAccount.Name)
	testutil.SetQueryParam(request, "limit", 1)
	testutil.SetQueryParam(request, "page", 2)
	testutil.SetQueryParam(request, "acknowledge", "false")
	require.NoError(t, app.GetMessages(request))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(request))

	assertConversationReadCursorMissing(t, db, organization.ID, reader.ID, conversation.ID)
	var beforeExplicitMark []models.Message
	require.NoError(t, db.Where("id IN ?", []uuid.UUID{first.ID, second.ID, third.ID}).
		Order("created_at ASC, id ASC").Find(&beforeExplicitMark).Error)
	require.Len(t, beforeExplicitMark, 3)
	for i := range beforeExplicitMark {
		assert.NotEqual(t, models.MessageStatusRead, beforeExplicitMark[i].Status)
	}

	markLegacyChatVisible(t, app, organization.ID, reader.ID, contact.ID, second.ID, fasthttp.StatusOK)
	cursor := loadConversationReadCursor(t, db, organization.ID, reader.ID, conversation.ID)
	require.NotNil(t, cursor.LastReadMessageID)
	assert.Equal(t, second.ID, *cursor.LastReadMessageID)
	unreadCount, err := countUnreadInboxMessages(db, organization.ID, reader.ID, conversation.ID)
	require.NoError(t, err)
	assert.Equal(t, int64(1), unreadCount, "same-time higher-ID message must remain unread")

	var secondReaderCursorCount int64
	require.NoError(t, db.Model(&models.ConversationRead{}).Where(
		"organization_id = ? AND conversation_id = ? AND reader_key = ?",
		organization.ID,
		conversation.ID,
		"user:"+secondReader.ID.String(),
	).Count(&secondReaderCursorCount).Error)
	assert.Zero(t, secondReaderCursorCount)
	secondReaderUnread, err := countUnreadInboxMessages(
		db,
		organization.ID,
		secondReader.ID,
		conversation.ID,
	)
	require.NoError(t, err)
	assert.Equal(t, int64(3), secondReaderUnread)

	var persisted []models.Message
	require.NoError(t, db.Where("id IN ?", []uuid.UUID{first.ID, second.ID, third.ID}).
		Order("created_at ASC, id ASC").Find(&persisted).Error)
	require.Len(t, persisted, 3)
	assert.Equal(t, models.MessageStatusRead, persisted[0].Status)
	assert.Equal(t, models.MessageStatusRead, persisted[1].Status)
	assert.NotEqual(t, models.MessageStatusRead, persisted[2].Status)

	markLegacyChatVisible(t, app, organization.ID, reader.ID, contact.ID, first.ID, fasthttp.StatusOK)
	cursor = loadConversationReadCursor(t, db, organization.ID, reader.ID, conversation.ID)
	assert.Equal(t, second.ID, *cursor.LastReadMessageID, "stale explicit retry must not regress")

	markLegacyChatVisible(t, app, organization.ID, reader.ID, contact.ID, third.ID, fasthttp.StatusOK)
	cursor = loadConversationReadCursor(t, db, organization.ID, reader.ID, conversation.ID)
	assert.Equal(t, third.ID, *cursor.LastReadMessageID)
	unreadCount, err = countUnreadInboxMessages(db, organization.ID, reader.ID, conversation.ID)
	require.NoError(t, err)
	assert.Zero(t, unreadCount)
}

func TestLegacyChatExplicitCursorRejectsScopeAndProviderLeakage(t *testing.T) {
	db := testutil.SetupTestDB(t)
	organization := testutil.CreateTestOrganization(t, db)
	adminRole := testutil.CreateAdminRole(t, db, organization.ID)
	reader := testutil.CreateTestUser(t, db, organization.ID, testutil.WithRoleID(&adminRole.ID))
	firstAccount := testutil.CreateTestWhatsAppAccount(t, db, organization.ID)
	secondAccount := testutil.CreateTestWhatsAppAccount(t, db, organization.ID)
	contact := testutil.CreateTestContactWith(
		t,
		db,
		organization.ID,
		testutil.WithContactAccount(firstAccount.Name),
	)
	firstConversation := createLegacyChatConversation(t, db, firstAccount, contact.ID)
	secondConversation := createLegacyChatConversation(t, db, secondAccount, contact.ID)

	baseTime := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	firstMessage := createLegacyChatMessage(
		t,
		db,
		organization.ID,
		firstAccount.Name,
		contact.ID,
		firstConversation.ID,
		uuid.New(),
		baseTime,
	)
	secondAccountMessage := createLegacyChatMessage(
		t,
		db,
		organization.ID,
		secondAccount.Name,
		contact.ID,
		secondConversation.ID,
		uuid.New(),
		baseTime.Add(time.Second),
	)

	relayAccount := models.ChannelAccount{
		BaseModel:         models.BaseModel{ID: uuid.New()},
		OrganizationID:    organization.ID,
		Channel:           models.ChannelWhatsApp,
		Provider:          channelapi.RelayProvider,
		Name:              "Relay account",
		ExternalAccountID: "relay-" + uuid.NewString(),
		Status:            models.ChannelAccountStatusActive,
		Capabilities:      models.JSONB{},
		Config:            models.JSONB{},
		Metadata:          models.JSONB{},
	}
	require.NoError(t, db.Create(&relayAccount).Error)
	relayConversation := createAttentionConversation(
		t,
		db,
		organization.ID,
		relayAccount.ID,
		contact.ID,
		"relay-provider-isolation",
	)
	relayMessage := createAttentionMessage(
		t,
		db,
		organization.ID,
		firstAccount.Name,
		contact.ID,
		relayConversation.ID,
		models.DirectionIncoming,
		baseTime.Add(2*time.Second),
	)

	app := &App{DB: db, Log: testutil.NopLogger()}
	markLegacyChatVisible(
		t,
		app,
		organization.ID,
		reader.ID,
		contact.ID,
		firstMessage.ID,
		fasthttp.StatusOK,
	)
	_ = loadConversationReadCursor(t, db, organization.ID, reader.ID, firstConversation.ID)
	assertConversationReadCursorMissing(t, db, organization.ID, reader.ID, secondConversation.ID)
	assertConversationReadCursorMissing(t, db, organization.ID, reader.ID, relayConversation.ID)

	syncResult, err := app.syncLegacyVisibleMessagesRead(
		organization.ID,
		reader.ID,
		contact.ID,
		[]models.Message{relayMessage},
		false,
	)
	require.NoError(t, err)
	assert.Zero(t, syncResult.AdvancedConversations)
	assert.Empty(t, syncResult.VerifiedMessageConversations)
	assertConversationReadCursorMissing(t, db, organization.ID, reader.ID, relayConversation.ID)
	require.NoError(t, db.First(&relayMessage, "id = ?", relayMessage.ID).Error)
	assert.NotEqual(t, models.MessageStatusRead, relayMessage.Status)
	require.NoError(t, db.First(&secondAccountMessage, "id = ?", secondAccountMessage.ID).Error)
	assert.NotEqual(t, models.MessageStatusRead, secondAccountMessage.Status)

	otherContact := testutil.CreateTestContactWith(
		t,
		db,
		organization.ID,
		testutil.WithContactAccount(firstAccount.Name),
	)
	otherConversation := createLegacyChatConversation(t, db, firstAccount, otherContact.ID)
	otherMessage := createLegacyChatMessage(
		t,
		db,
		organization.ID,
		firstAccount.Name,
		otherContact.ID,
		otherConversation.ID,
		uuid.New(),
		baseTime,
	)
	markLegacyChatVisible(
		t,
		app,
		organization.ID,
		reader.ID,
		contact.ID,
		otherMessage.ID,
		fasthttp.StatusBadRequest,
	)
	assertConversationReadCursorMissing(t, db, organization.ID, reader.ID, otherConversation.ID)

	deletedMessage := createLegacyChatMessage(
		t,
		db,
		organization.ID,
		firstAccount.Name,
		contact.ID,
		firstConversation.ID,
		uuid.New(),
		baseTime.Add(3*time.Second),
	)
	require.NoError(t, db.Delete(&deletedMessage).Error)
	markLegacyChatVisible(
		t,
		app,
		organization.ID,
		reader.ID,
		contact.ID,
		deletedMessage.ID,
		fasthttp.StatusBadRequest,
	)

	unlinkedMessage := models.Message{
		BaseModel: models.BaseModel{
			ID:        uuid.New(),
			CreatedAt: baseTime.Add(4 * time.Second),
			UpdatedAt: baseTime.Add(4 * time.Second),
		},
		OrganizationID:    organization.ID,
		WhatsAppAccount:   firstAccount.Name,
		ContactID:         contact.ID,
		ConversationID:    "legacy-unlinked-" + uuid.NewString(),
		Direction:         models.DirectionIncoming,
		MessageType:       models.MessageTypeText,
		Content:           "Unlinked visible message",
		Status:            models.MessageStatusDelivered,
		WhatsAppMessageID: "wamid." + uuid.NewString(),
		Metadata:          models.JSONB{},
	}
	require.NoError(t, db.Create(&unlinkedMessage).Error)
	markLegacyChatVisible(
		t,
		app,
		organization.ID,
		reader.ID,
		contact.ID,
		unlinkedMessage.ID,
		fasthttp.StatusOK,
	)
	require.NoError(t, db.First(&unlinkedMessage, "id = ?", unlinkedMessage.ID).Error)
	require.NotNil(t, unlinkedMessage.InboxConversationID)
	assert.Equal(t, models.MessageStatusRead, unlinkedMessage.Status)
	unlinkedCursor := loadConversationReadCursor(
		t,
		db,
		organization.ID,
		reader.ID,
		*unlinkedMessage.InboxConversationID,
	)
	require.NotNil(t, unlinkedCursor.LastReadMessageID)
	assert.Equal(t, unlinkedMessage.ID, *unlinkedCursor.LastReadMessageID)
}

func TestMarkContactReadRequiresChatReadPermission(t *testing.T) {
	db := testutil.SetupTestDB(t)
	organization := testutil.CreateTestOrganization(t, db)
	role := testutil.CreateTestRoleWithKeys(
		t,
		db,
		organization.ID,
		"no-chat-read-"+uuid.NewString(),
		[]string{models.ResourceContacts + ":" + models.ActionRead},
	)
	user := testutil.CreateTestUser(t, db, organization.ID, testutil.WithRoleID(&role.ID))
	contact := testutil.CreateTestContact(t, db, organization.ID)
	app := &App{DB: db, Log: testutil.NopLogger()}

	request := testutil.NewJSONRequest(t, nil)
	testutil.SetAuthContext(request, organization.ID, user.ID)
	testutil.SetPathParam(request, "id", contact.ID.String())
	require.NoError(t, app.MarkContactRead(request))
	assert.Equal(t, fasthttp.StatusForbidden, testutil.GetResponseStatusCode(request))
}

func TestLegacyChatReadOnlyUserAdvancesPersonalCursorWithoutSharedOrProviderMutation(t *testing.T) {
	db := testutil.SetupTestDB(t)
	organization := testutil.CreateTestOrganization(t, db)
	role := testutil.CreateTestRoleWithKeys(
		t,
		db,
		organization.ID,
		"chat-read-only-"+uuid.NewString(),
		[]string{models.ResourceChat + ":" + models.ActionRead},
	)
	user := testutil.CreateTestUser(t, db, organization.ID, testutil.WithRoleID(&role.ID))
	account := testutil.CreateTestWhatsAppAccount(t, db, organization.ID)
	contact := testutil.CreateTestContactWith(
		t,
		db,
		organization.ID,
		testutil.WithContactAccount(account.Name),
	)
	require.NoError(t, db.Model(&contact).Updates(map[string]any{
		"assigned_user_id": user.ID,
		"is_read":          false,
	}).Error)
	conversation := createLegacyChatConversation(t, db, account, contact.ID)
	visible := createLegacyChatMessage(
		t,
		db,
		organization.ID,
		account.Name,
		contact.ID,
		conversation.ID,
		uuid.New(),
		time.Now().UTC().Add(-time.Minute).Truncate(time.Microsecond),
	)
	app := &App{DB: db, Log: testutil.NopLogger()}

	// Cached GET behavior remains available to write-capable agents, but a
	// read-only principal cannot use it to mutate shared status or send a
	// provider acknowledgement.
	getRequest := testutil.NewGETRequest(t)
	testutil.SetAuthContext(getRequest, organization.ID, user.ID)
	testutil.SetPathParam(getRequest, "id", contact.ID.String())
	require.NoError(t, app.GetMessages(getRequest))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(getRequest))
	require.NoError(t, db.First(&visible, "id = ?", visible.ID).Error)
	assert.NotEqual(t, models.MessageStatusRead, visible.Status)

	markLegacyChatVisible(
		t,
		app,
		organization.ID,
		user.ID,
		contact.ID,
		visible.ID,
		fasthttp.StatusOK,
	)
	cursor := loadConversationReadCursor(t, db, organization.ID, user.ID, conversation.ID)
	require.NotNil(t, cursor.LastReadMessageID)
	assert.Equal(t, visible.ID, *cursor.LastReadMessageID)
	require.NoError(t, db.First(&visible, "id = ?", visible.ID).Error)
	assert.NotEqual(t, models.MessageStatusRead, visible.Status)
	require.NoError(t, db.First(&contact, "id = ?", contact.ID).Error)
	assert.False(t, contact.IsRead)
	var shadowCountBefore, identityCountBefore, conversationCountBefore int64
	require.NoError(t, db.Model(&models.ChannelAccount{}).
		Where("organization_id = ?", organization.ID).
		Count(&shadowCountBefore).Error)
	require.NoError(t, db.Model(&models.ContactIdentity{}).
		Where("organization_id = ?", organization.ID).
		Count(&identityCountBefore).Error)
	require.NoError(t, db.Model(&models.InboxConversation{}).
		Where("organization_id = ?", organization.ID).
		Count(&conversationCountBefore).Error)

	readOnlyUnlinked := models.Message{
		BaseModel: models.BaseModel{
			ID:        uuid.New(),
			CreatedAt: time.Now().UTC(),
			UpdatedAt: time.Now().UTC(),
		},
		OrganizationID:    organization.ID,
		WhatsAppAccount:   account.Name,
		ContactID:         contact.ID,
		ConversationID:    "read-only-unlinked-" + uuid.NewString(),
		WhatsAppMessageID: "wamid." + uuid.NewString(),
		Direction:         models.DirectionIncoming,
		MessageType:       models.MessageTypeText,
		Content:           "Read-only repair must be inert",
		Status:            models.MessageStatusDelivered,
		Metadata:          models.JSONB{},
	}
	require.NoError(t, db.Create(&readOnlyUnlinked).Error)
	markLegacyChatVisible(
		t,
		app,
		organization.ID,
		user.ID,
		contact.ID,
		readOnlyUnlinked.ID,
		fasthttp.StatusOK,
	)
	require.NoError(t, db.First(&readOnlyUnlinked, "id = ?", readOnlyUnlinked.ID).Error)
	assert.Nil(t, readOnlyUnlinked.InboxConversationID)
	assert.NotEqual(t, models.MessageStatusRead, readOnlyUnlinked.Status)
	var shadowCountAfter, identityCountAfter, conversationCountAfter int64
	require.NoError(t, db.Model(&models.ChannelAccount{}).
		Where("organization_id = ?", organization.ID).
		Count(&shadowCountAfter).Error)
	require.NoError(t, db.Model(&models.ContactIdentity{}).
		Where("organization_id = ?", organization.ID).
		Count(&identityCountAfter).Error)
	require.NoError(t, db.Model(&models.InboxConversation{}).
		Where("organization_id = ?", organization.ID).
		Count(&conversationCountAfter).Error)
	assert.Equal(t, shadowCountBefore, shadowCountAfter)
	assert.Equal(t, identityCountBefore, identityCountAfter)
	assert.Equal(t, conversationCountBefore, conversationCountAfter)
}

func TestMarkContactReadHonorsCurrentConversationCutoff(t *testing.T) {
	db := testutil.SetupTestDB(t)
	organization := testutil.CreateTestOrganization(t, db)
	role := testutil.CreateTestRoleWithKeys(
		t,
		db,
		organization.ID,
		"chat-current-conversation-"+uuid.NewString(),
		[]string{
			models.ResourceChat + ":" + models.ActionRead,
			models.ResourceChat + ":" + models.ActionWrite,
		},
	)
	user := testutil.CreateTestUser(t, db, organization.ID, testutil.WithRoleID(&role.ID))
	account := testutil.CreateTestWhatsAppAccount(t, db, organization.ID)
	contact := testutil.CreateTestContactWith(
		t,
		db,
		organization.ID,
		testutil.WithContactAccount(account.Name),
	)
	require.NoError(t, db.Model(&contact).Update("assigned_user_id", user.ID).Error)
	conversation := createLegacyChatConversation(t, db, account, contact.ID)
	require.NoError(t, db.Create(&models.ChatbotSettings{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: organization.ID,
		AgentAssignment: models.AgentAssignmentConfig{
			CurrentConversationOnly: true,
		},
	}).Error)

	cutoff := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	require.NoError(t, db.Create(&models.AgentTransfer{
		BaseModel:       models.BaseModel{ID: uuid.New()},
		OrganizationID:  organization.ID,
		ContactID:       contact.ID,
		WhatsAppAccount: account.Name,
		PhoneNumber:     contact.PhoneNumber,
		Status:          models.TransferStatusActive,
		Source:          models.TransferSourceManual,
		AgentID:         &user.ID,
		TransferredAt:   cutoff,
	}).Error)
	older := createLegacyChatMessage(
		t,
		db,
		organization.ID,
		account.Name,
		contact.ID,
		conversation.ID,
		uuid.New(),
		cutoff.Add(-time.Minute),
	)
	current := createLegacyChatMessage(
		t,
		db,
		organization.ID,
		account.Name,
		contact.ID,
		conversation.ID,
		uuid.New(),
		cutoff.Add(time.Minute),
	)

	app := &App{DB: db, Log: testutil.NopLogger()}
	markLegacyChatVisible(
		t,
		app,
		organization.ID,
		user.ID,
		contact.ID,
		older.ID,
		fasthttp.StatusBadRequest,
	)
	assertConversationReadCursorMissing(t, db, organization.ID, user.ID, conversation.ID)
	markLegacyChatVisible(
		t,
		app,
		organization.ID,
		user.ID,
		contact.ID,
		current.ID,
		fasthttp.StatusOK,
	)
	cursor := loadConversationReadCursor(t, db, organization.ID, user.ID, conversation.ID)
	require.NotNil(t, cursor.LastReadMessageID)
	assert.Equal(t, current.ID, *cursor.LastReadMessageID)
	require.NoError(t, db.First(&older, "id = ?", older.ID).Error)
	require.NoError(t, db.First(&current, "id = ?", current.ID).Error)
	assert.NotEqual(t, models.MessageStatusRead, older.Status)
	assert.Equal(t, models.MessageStatusRead, current.Status)
}

func createLegacyChatConversation(
	t *testing.T,
	db *gorm.DB,
	account *models.WhatsAppAccount,
	contactID uuid.UUID,
) models.InboxConversation {
	t.Helper()
	externalAccountID := "legacy-account:" + account.ID.String()
	var shadow models.ChannelAccount
	findShadow := db.Where(
		"organization_id = ? AND channel = ? AND provider = ? AND external_account_id = ? AND deleted_at IS NULL",
		account.OrganizationID,
		models.ChannelWhatsApp,
		channelapi.LegacyMetaProvider,
		externalAccountID,
	).First(&shadow)
	if findShadow.Error != nil {
		require.ErrorIs(t, findShadow.Error, gorm.ErrRecordNotFound)
		shadow = models.ChannelAccount{
			BaseModel:         models.BaseModel{ID: uuid.New()},
			OrganizationID:    account.OrganizationID,
			Channel:           models.ChannelWhatsApp,
			Provider:          channelapi.LegacyMetaProvider,
			Name:              account.Name,
			ExternalAccountID: externalAccountID,
			Status:            models.ChannelAccountStatusActive,
			Capabilities:      models.JSONB{},
			Config:            models.JSONB{"legacy_read_only": true},
			Metadata:          models.JSONB{"legacy_account_id": account.ID.String()},
		}
		require.NoError(t, db.Create(&shadow).Error)
	}
	conversation := models.InboxConversation{
		BaseModel:              models.BaseModel{ID: uuid.New()},
		OrganizationID:         account.OrganizationID,
		ChannelAccountID:       shadow.ID,
		ContactID:              contactID,
		Channel:                models.ChannelWhatsApp,
		ExternalConversationID: "legacy-contact:" + contactID.String(),
		Status:                 models.InboxConversationStatusOpen,
		OpenedAt:               time.Now().UTC(),
		Config:                 models.JSONB{"legacy_read_only": true, "reply_route": "chat"},
		Metadata:               models.JSONB{"legacy_contact_id": contactID.String()},
	}
	require.NoError(t, db.Create(&conversation).Error)
	return conversation
}

func createLegacyChatMessage(
	t *testing.T,
	db *gorm.DB,
	organizationID uuid.UUID,
	accountName string,
	contactID, conversationID, messageID uuid.UUID,
	createdAt time.Time,
) models.Message {
	t.Helper()
	conversationIDCopy := conversationID
	message := models.Message{
		BaseModel: models.BaseModel{
			ID:        messageID,
			CreatedAt: createdAt,
			UpdatedAt: createdAt,
		},
		OrganizationID:      organizationID,
		WhatsAppAccount:     accountName,
		ContactID:           contactID,
		ConversationID:      conversationID.String(),
		InboxConversationID: &conversationIDCopy,
		WhatsAppMessageID:   "wamid." + messageID.String(),
		Direction:           models.DirectionIncoming,
		MessageType:         models.MessageTypeText,
		Content:             "Visible legacy Chat message",
		Status:              models.MessageStatusDelivered,
		Metadata:            models.JSONB{},
	}
	require.NoError(t, db.Create(&message).Error)
	require.NoError(t, db.Model(&models.Message{}).
		Where("id = ? AND organization_id = ?", message.ID, organizationID).
		Update("ingested_at", createdAt.UTC()).Error)
	createdAtUTC := createdAt.UTC()
	message.IngestedAt = &createdAtUTC
	return message
}

func markLegacyChatVisible(
	t *testing.T,
	app *App,
	organizationID, userID, contactID, messageID uuid.UUID,
	expectedStatus int,
) {
	t.Helper()
	request := testutil.NewJSONRequest(t, MarkContactReadRequest{
		LastVisibleMessageID: &messageID,
	})
	testutil.SetAuthContext(request, organizationID, userID)
	testutil.SetPathParam(request, "id", contactID.String())
	require.NoError(t, app.MarkContactRead(request))
	require.Equal(t, expectedStatus, testutil.GetResponseStatusCode(request))
}

func loadConversationReadCursor(
	t *testing.T,
	db *gorm.DB,
	organizationID, userID, conversationID uuid.UUID,
) models.ConversationRead {
	t.Helper()
	var cursor models.ConversationRead
	require.NoError(t, db.Where(
		"organization_id = ? AND conversation_id = ? AND reader_key = ?",
		organizationID,
		conversationID,
		"user:"+userID.String(),
	).First(&cursor).Error)
	return cursor
}

func assertConversationReadCursorMissing(
	t *testing.T,
	db *gorm.DB,
	organizationID, userID, conversationID uuid.UUID,
) {
	t.Helper()
	var count int64
	require.NoError(t, db.Model(&models.ConversationRead{}).Where(
		"organization_id = ? AND conversation_id = ? AND reader_key = ?",
		organizationID,
		conversationID,
		"user:"+userID.String(),
	).Count(&count).Error)
	assert.Zero(t, count)
}
