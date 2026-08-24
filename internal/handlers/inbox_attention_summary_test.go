package handlers

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	channelapi "github.com/shridarpatil/whatomate/internal/channel"
	"github.com/shridarpatil/whatomate/internal/database"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type recordingReadReceiptAdapter struct {
	channelapi.Adapter
	calls      [][]string
	onMarkRead func()
}

func (adapter *recordingReadReceiptAdapter) Capabilities(*models.ChannelAccount) channelapi.Capabilities {
	return channelapi.Capabilities{ReadReceipts: true}
}

func (adapter *recordingReadReceiptAdapter) MarkRead(
	_ context.Context,
	_ *models.ChannelAccount,
	_ channelapi.ConversationRef,
	externalMessageIDs []string,
) error {
	if adapter.onMarkRead != nil {
		adapter.onMarkRead()
	}
	adapter.calls = append(adapter.calls, append([]string(nil), externalMessageIDs...))
	return nil
}

func TestInboxAttentionSummaryIsTenantAndReaderScoped(t *testing.T) {
	db := testutil.SetupTestDB(t)
	organization := testutil.CreateTestOrganization(t, db)
	role := testutil.CreateTestRoleWithKeys(
		t,
		db,
		organization.ID,
		"inbox-attention-"+uuid.NewString(),
		[]string{
			models.ResourceConversations + ":" + models.ActionRead,
		},
	)
	reader := testutil.CreateTestUser(t, db, organization.ID, testutil.WithRoleID(&role.ID))
	secondReader := testutil.CreateTestUser(t, db, organization.ID, testutil.WithRoleID(&role.ID))
	enableBookingCommerceTestEntitlement(
		t,
		db,
		organization.ID,
		reader.ID,
		"omnichannel.enabled",
	)

	contact := testutil.CreateTestContact(t, db, organization.ID)
	account := models.ChannelAccount{
		BaseModel:         models.BaseModel{ID: uuid.New()},
		OrganizationID:    organization.ID,
		Channel:           models.ChannelWhatsApp,
		Provider:          "relay",
		Name:              "Attention summary account",
		ExternalAccountID: "attention-" + uuid.NewString(),
		Status:            models.ChannelAccountStatusActive,
		Capabilities:      models.JSONB{},
		Config:            models.JSONB{},
		Metadata:          models.JSONB{},
	}
	require.NoError(t, db.Create(&account).Error)

	first := createAttentionConversation(t, db, organization.ID, account.ID, contact.ID, "first")
	second := createAttentionConversation(t, db, organization.ID, account.ID, contact.ID, "second")
	outgoingOnly := createAttentionConversation(t, db, organization.ID, account.ID, contact.ID, "outgoing")
	deletedOnly := createAttentionConversation(t, db, organization.ID, account.ID, contact.ID, "deleted")

	baseTime := time.Now().UTC().Add(-time.Minute).Truncate(time.Microsecond)
	firstOlder := createAttentionMessage(t, db, organization.ID, account.Name, contact.ID, first.ID, models.DirectionIncoming, baseTime)
	firstNewer := createAttentionMessage(t, db, organization.ID, account.Name, contact.ID, first.ID, models.DirectionIncoming, baseTime.Add(time.Second))
	secondMessage := createAttentionMessage(t, db, organization.ID, account.Name, contact.ID, second.ID, models.DirectionIncoming, baseTime.Add(2*time.Second))
	createAttentionMessage(t, db, organization.ID, account.Name, contact.ID, outgoingOnly.ID, models.DirectionOutgoing, baseTime.Add(3*time.Second))
	deletedMessage := createAttentionMessage(t, db, organization.ID, account.Name, contact.ID, deletedOnly.ID, models.DirectionIncoming, baseTime.Add(4*time.Second))
	require.NoError(t, db.Delete(&deletedMessage).Error)

	otherOrganization := testutil.CreateTestOrganization(t, db)
	otherContact := testutil.CreateTestContact(t, db, otherOrganization.ID)
	otherAccount := models.ChannelAccount{
		BaseModel:         models.BaseModel{ID: uuid.New()},
		OrganizationID:    otherOrganization.ID,
		Channel:           models.ChannelWhatsApp,
		Provider:          "relay",
		Name:              "Other tenant account",
		ExternalAccountID: "other-" + uuid.NewString(),
		Status:            models.ChannelAccountStatusActive,
		Capabilities:      models.JSONB{},
		Config:            models.JSONB{},
		Metadata:          models.JSONB{},
	}
	require.NoError(t, db.Create(&otherAccount).Error)
	otherConversation := createAttentionConversation(
		t,
		db,
		otherOrganization.ID,
		otherAccount.ID,
		otherContact.ID,
		"other-tenant",
	)
	createAttentionMessage(
		t,
		db,
		otherOrganization.ID,
		otherAccount.Name,
		otherContact.ID,
		otherConversation.ID,
		models.DirectionIncoming,
		baseTime,
	)

	app := &App{DB: db, Log: testutil.NopLogger()}
	readerSummary := getAttentionSummary(t, app, organization.ID, reader.ID)
	assert.Equal(t, int64(2), readerSummary.UnreadConversations)
	assert.False(t, readerSummary.AsOf.IsZero())
	listedUnread := getListedConversationUnreadCounts(t, app, organization.ID, reader.ID)
	assert.Equal(t, 2, listedUnread[first.ID])
	assert.Equal(t, 1, listedUnread[second.ID])
	assert.Zero(t, listedUnread[outgoingOnly.ID])
	assert.Zero(t, listedUnread[deletedOnly.ID])

	markAttentionConversationRead(
		t,
		app,
		organization.ID,
		secondReader.ID,
		first.ID,
		&firstNewer.ID,
	)
	markAttentionConversationRead(
		t,
		app,
		organization.ID,
		secondReader.ID,
		second.ID,
		&secondMessage.ID,
	)
	assert.Zero(t, getAttentionSummary(t, app, organization.ID, secondReader.ID).UnreadConversations)

	crossConversationRequest := testutil.NewJSONRequest(t, MarkInboxConversationReadRequest{
		LastVisibleMessageID: &secondMessage.ID,
	})
	testutil.SetAuthContext(crossConversationRequest, organization.ID, reader.ID)
	testutil.SetPathParam(crossConversationRequest, "id", first.ID.String())
	require.NoError(t, app.MarkInboxConversationRead(crossConversationRequest))
	assert.Equal(t, fasthttp.StatusBadRequest, testutil.GetResponseStatusCode(crossConversationRequest))

	olderResponse := markAttentionConversationRead(
		t,
		app,
		organization.ID,
		reader.ID,
		first.ID,
		&firstOlder.ID,
	)
	assert.Equal(t, int64(1), olderResponse.UnreadCount)
	assert.Equal(t, firstOlder.ID, *olderResponse.LastReadMessageID)
	assert.Equal(t, int64(2), getAttentionSummary(t, app, organization.ID, reader.ID).UnreadConversations)
	listedUnread = getListedConversationUnreadCounts(t, app, organization.ID, reader.ID)
	assert.Equal(t, 1, listedUnread[first.ID])

	newerResponse := markAttentionConversationRead(
		t,
		app,
		organization.ID,
		reader.ID,
		first.ID,
		&firstNewer.ID,
	)
	assert.Zero(t, newerResponse.UnreadCount)
	assert.Equal(t, firstNewer.ID, *newerResponse.LastReadMessageID)
	assert.Equal(t, int64(1), getAttentionSummary(t, app, organization.ID, reader.ID).UnreadConversations)

	staleResponse := markAttentionConversationRead(
		t,
		app,
		organization.ID,
		reader.ID,
		first.ID,
		&firstOlder.ID,
	)
	assert.Zero(t, staleResponse.UnreadCount)
	assert.Equal(t, firstNewer.ID, *staleResponse.LastReadMessageID)

	// Existing clients that omit a cursor retain their mark-through-latest
	// behavior, but now persist the exact latest message tuple.
	legacyResponse := markAttentionConversationRead(
		t,
		app,
		organization.ID,
		reader.ID,
		second.ID,
		nil,
	)
	assert.Zero(t, legacyResponse.UnreadCount)
	assert.Equal(t, secondMessage.ID, *legacyResponse.LastReadMessageID)
	assert.Zero(t, getAttentionSummary(t, app, organization.ID, reader.ID).UnreadConversations)
}

func TestAdvanceConversationReadCursorIsMonotonicUnderConcurrentCalls(t *testing.T) {
	db := testutil.SetupTestDB(t)
	organization := testutil.CreateTestOrganization(t, db)
	user := testutil.CreateTestUser(t, db, organization.ID)
	contact := testutil.CreateTestContact(t, db, organization.ID)
	account := models.ChannelAccount{
		BaseModel:         models.BaseModel{ID: uuid.New()},
		OrganizationID:    organization.ID,
		Channel:           models.ChannelWhatsApp,
		Provider:          "relay",
		Name:              "Concurrent cursor account",
		ExternalAccountID: "concurrent-" + uuid.NewString(),
		Status:            models.ChannelAccountStatusActive,
		Capabilities:      models.JSONB{},
		Config:            models.JSONB{},
		Metadata:          models.JSONB{},
	}
	require.NoError(t, db.Create(&account).Error)
	conversation := createAttentionConversation(
		t,
		db,
		organization.ID,
		account.ID,
		contact.ID,
		"concurrent",
	)

	baseTime := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	messages := make([]models.Message, 8)
	for i := range messages {
		messages[i] = createAttentionMessage(
			t,
			db,
			organization.ID,
			account.Name,
			contact.ID,
			conversation.ID,
			models.DirectionIncoming,
			baseTime.Add(time.Duration(i)*time.Second),
		)
	}

	acknowledgementStartedAt := time.Now().UTC()
	start := make(chan struct{})
	errorsByCall := make(chan error, len(messages))
	var wait sync.WaitGroup
	for i := range messages {
		message := messages[i]
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			errorsByCall <- db.Transaction(func(tx *gorm.DB) error {
				_, _, err := advanceConversationReadCursor(
					tx,
					organization.ID,
					user.ID,
					conversation.ID,
					&message,
					time.Time{},
				)
				return err
			})
		}()
	}
	close(start)
	wait.Wait()
	close(errorsByCall)
	for err := range errorsByCall {
		require.NoError(t, err)
	}

	var cursor models.ConversationRead
	require.NoError(t, db.Where(
		"organization_id = ? AND conversation_id = ? AND reader_key = ?",
		organization.ID,
		conversation.ID,
		"user:"+user.ID.String(),
	).First(&cursor).Error)
	require.NotNil(t, cursor.LastReadMessageID)
	assert.Equal(t, messages[len(messages)-1].ID, *cursor.LastReadMessageID)
	require.NotNil(t, cursor.LastReadIngestedAt)
	assert.Equal(t, messages[len(messages)-1].EffectiveIngestedAt(), cursor.LastReadIngestedAt.UTC())
	assert.False(t, cursor.LastReadAt.Before(acknowledgementStartedAt))
	assert.False(t, cursor.LastReadAt.After(time.Now().UTC()))
	assert.NotEqual(t, messages[len(messages)-1].CreatedAt, cursor.LastReadAt)

	require.NoError(t, db.Delete(&cursor).Error)
	require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
		_, advanced, err := advanceConversationReadCursor(
			tx,
			organization.ID,
			user.ID,
			conversation.ID,
			&messages[len(messages)-1],
			time.Time{},
		)
		assert.True(t, advanced)
		return err
	}))
	restored := loadConversationReadCursor(t, db, organization.ID, user.ID, conversation.ID)
	require.NotNil(t, restored.LastReadMessageID)
	assert.Equal(t, messages[len(messages)-1].ID, *restored.LastReadMessageID)
}

func TestMarkInboxConversationReadSeparatesLocalCursorFromProviderAuthority(t *testing.T) {
	db := testutil.SetupTestDB(t)
	organization := testutil.CreateTestOrganization(t, db)
	readRole := testutil.CreateTestRoleWithKeys(
		t,
		db,
		organization.ID,
		"inbox-read-only-"+uuid.NewString(),
		[]string{models.ResourceConversations + ":" + models.ActionRead},
	)
	writeRole := testutil.CreateTestRoleWithKeys(
		t,
		db,
		organization.ID,
		"inbox-read-write-"+uuid.NewString(),
		[]string{
			models.ResourceConversations + ":" + models.ActionRead,
			models.ResourceConversations + ":" + models.ActionWrite,
		},
	)
	readOnlyUser := testutil.CreateTestUser(t, db, organization.ID, testutil.WithRoleID(&readRole.ID))
	writeUser := testutil.CreateTestUser(t, db, organization.ID, testutil.WithRoleID(&writeRole.ID))
	enableBookingCommerceTestEntitlement(
		t,
		db,
		organization.ID,
		readOnlyUser.ID,
		"omnichannel.enabled",
	)

	contact := testutil.CreateTestContact(t, db, organization.ID)
	account := models.ChannelAccount{
		BaseModel:         models.BaseModel{ID: uuid.New()},
		OrganizationID:    organization.ID,
		Channel:           models.ChannelWhatsApp,
		Provider:          "synthetic-receipts",
		Name:              "Synthetic receipts account",
		ExternalAccountID: "receipts-" + uuid.NewString(),
		Status:            models.ChannelAccountStatusActive,
		Capabilities:      models.JSONB{},
		Config:            models.JSONB{},
		Metadata:          models.JSONB{},
	}
	require.NoError(t, db.Create(&account).Error)
	conversation := createAttentionConversation(
		t,
		db,
		organization.ID,
		account.ID,
		contact.ID,
		"provider-authority",
	)
	baseTime := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	first := createAttentionMessage(
		t, db, organization.ID, account.Name, contact.ID, conversation.ID,
		models.DirectionIncoming, baseTime,
	)
	second := createAttentionMessage(
		t, db, organization.ID, account.Name, contact.ID, conversation.ID,
		models.DirectionIncoming, baseTime.Add(time.Second),
	)
	third := createAttentionMessage(
		t, db, organization.ID, account.Name, contact.ID, conversation.ID,
		models.DirectionIncoming, baseTime.Add(2*time.Second),
	)
	for index, message := range []*models.Message{&first, &second, &third} {
		message.WhatsAppMessageID = "provider-message-" + string(rune('a'+index))
		require.NoError(t, db.Model(&models.Message{}).
			Where("id = ? AND organization_id = ?", message.ID, organization.ID).
			Update("whats_app_message_id", message.WhatsAppMessageID).Error)
	}

	adapter := &recordingReadReceiptAdapter{}
	providerObservedCommittedCursor := false
	adapter.onMarkRead = func() {
		var committed int64
		err := db.Session(&gorm.Session{NewDB: true}).Model(&models.ConversationRead{}).
			Where(
				"organization_id = ? AND conversation_id = ? AND reader_key = ? AND last_read_message_id = ?",
				organization.ID,
				conversation.ID,
				"user:"+writeUser.ID.String(),
				second.ID,
			).
			Count(&committed).Error
		providerObservedCommittedCursor = err == nil && committed == 1
	}
	app := &App{DB: db, Log: testutil.NopLogger()}
	app.channelAdapterFactory = func(*models.ChannelAccount) (channelapi.Adapter, error) {
		return adapter, nil
	}

	mark := func(userID uuid.UUID, visibleID uuid.UUID, externalIDs []string) markAttentionReadResponse {
		t.Helper()
		request := testutil.NewJSONRequest(t, MarkInboxConversationReadRequest{
			LastVisibleMessageID: &visibleID,
			ExternalMessageIDs:   externalIDs,
		})
		testutil.SetAuthContext(request, organization.ID, userID)
		testutil.SetPathParam(request, "id", conversation.ID.String())
		require.NoError(t, app.MarkInboxConversationRead(request))
		require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(request))
		var response struct {
			Data markAttentionReadResponse `json:"data"`
		}
		require.NoError(t, json.Unmarshal(testutil.GetResponseBody(request), &response))
		return response.Data
	}

	readOnlyResponse := mark(
		readOnlyUser.ID,
		first.ID,
		[]string{first.WhatsAppMessageID},
	)
	assert.Equal(t, first.ID, *readOnlyResponse.LastReadMessageID)
	assert.False(t, readOnlyResponse.ProviderSyncQueued)
	assert.False(t, readOnlyResponse.ProviderSynced)
	assert.Empty(t, adapter.calls, "read-only authority must not acknowledge at the provider")

	writeResponse := mark(
		writeUser.ID,
		second.ID,
		[]string{first.WhatsAppMessageID, first.WhatsAppMessageID, second.WhatsAppMessageID},
	)
	assert.True(t, writeResponse.ProviderSyncQueued)
	assert.True(t, writeResponse.ProviderSynced, "success is serialized only after post-commit provider work")
	require.Len(t, adapter.calls, 1)
	assert.True(t, providerObservedCommittedCursor)
	assert.Equal(
		t,
		[]string{first.WhatsAppMessageID, second.WhatsAppMessageID},
		adapter.calls[0],
	)

	invalidBoundary := mark(
		writeUser.ID,
		second.ID,
		[]string{third.WhatsAppMessageID},
	)
	assert.False(t, invalidBoundary.ProviderSyncQueued)
	assert.Len(t, adapter.calls, 1, "an unseen external ID must fail closed")

	tooManyIDs := make([]string, maxInboxReadReceiptMessageIDs+1)
	for index := range tooManyIDs {
		tooManyIDs[index] = first.WhatsAppMessageID
	}
	assert.False(t, mark(writeUser.ID, second.ID, tooManyIDs).ProviderSyncQueued)
	assert.False(t, mark(
		writeUser.ID,
		second.ID,
		[]string{string(make([]byte, maxInboxReadReceiptMessageIDLength+1))},
	).ProviderSyncQueued)
	assert.Len(t, adapter.calls, 1, "bounded validation must not reach SQL/provider work")
}

func TestMarkInboxConversationReadOnlyAuthorityDoesNotMutateLegacySharedState(t *testing.T) {
	db := testutil.SetupTestDB(t)
	organization := testutil.CreateTestOrganization(t, db)
	role := testutil.CreateTestRoleWithKeys(
		t,
		db,
		organization.ID,
		"legacy-inbox-read-only-"+uuid.NewString(),
		[]string{models.ResourceConversations + ":" + models.ActionRead},
	)
	user := testutil.CreateTestUser(t, db, organization.ID, testutil.WithRoleID(&role.ID))
	enableBookingCommerceTestEntitlement(
		t, db, organization.ID, user.ID, "omnichannel.enabled",
	)
	legacyAccount := testutil.CreateTestWhatsAppAccount(t, db, organization.ID)
	contact := testutil.CreateTestContactWith(
		t,
		db,
		organization.ID,
		testutil.WithContactAccount(legacyAccount.Name),
	)
	require.NoError(t, db.Model(&models.Contact{}).
		Where("id = ? AND organization_id = ?", contact.ID, organization.ID).
		Update("is_read", false).Error)
	conversation := createLegacyChatConversation(t, db, legacyAccount, contact.ID)
	message := createLegacyChatMessage(
		t,
		db,
		organization.ID,
		legacyAccount.Name,
		contact.ID,
		conversation.ID,
		uuid.New(),
		time.Now().UTC().Add(-time.Minute),
	)

	app := &App{DB: db, Log: testutil.NopLogger()}
	response := markAttentionConversationRead(
		t,
		app,
		organization.ID,
		user.ID,
		conversation.ID,
		&message.ID,
	)
	assert.Equal(t, message.ID, *response.LastReadMessageID)
	assert.False(t, response.LegacyStateSynced)

	require.NoError(t, db.First(&message, "id = ?", message.ID).Error)
	assert.Equal(t, models.MessageStatusDelivered, message.Status)
	require.NoError(t, db.First(&contact, "id = ?", contact.ID).Error)
	assert.False(t, contact.IsRead)
}

func TestMarkInboxConversationReadRejectsCrossAccountLegacyRows(t *testing.T) {
	db := testutil.SetupTestDB(t)
	organization := testutil.CreateTestOrganization(t, db)
	role := testutil.CreateTestRoleWithKeys(
		t,
		db,
		organization.ID,
		"legacy-inbox-write-"+uuid.NewString(),
		[]string{
			models.ResourceConversations + ":" + models.ActionRead,
			models.ResourceConversations + ":" + models.ActionWrite,
		},
	)
	user := testutil.CreateTestUser(t, db, organization.ID, testutil.WithRoleID(&role.ID))
	enableBookingCommerceTestEntitlement(
		t, db, organization.ID, user.ID, "omnichannel.enabled",
	)
	firstAccount := testutil.CreateTestWhatsAppAccount(t, db, organization.ID)
	secondAccount := testutil.CreateTestWhatsAppAccount(t, db, organization.ID)
	require.NoError(t, db.Model(&models.WhatsAppAccount{}).
		Where("id IN ?", []uuid.UUID{firstAccount.ID, secondAccount.ID}).
		Update("auto_read_receipt", false).Error)
	contact := testutil.CreateTestContactWith(
		t,
		db,
		organization.ID,
		testutil.WithContactAccount(firstAccount.Name),
	)
	conversation := createLegacyChatConversation(t, db, firstAccount, contact.ID)
	baseTime := time.Now().UTC().Add(-time.Minute).Truncate(time.Microsecond)
	corruptCrossAccount := createLegacyChatMessage(
		t,
		db,
		organization.ID,
		secondAccount.Name,
		contact.ID,
		conversation.ID,
		uuid.MustParse("00000000-0000-0000-0000-000000000201"),
		baseTime,
	)
	visible := createLegacyChatMessage(
		t,
		db,
		organization.ID,
		firstAccount.Name,
		contact.ID,
		conversation.ID,
		uuid.MustParse("00000000-0000-0000-0000-000000000202"),
		baseTime.Add(time.Second),
	)

	app := &App{DB: db, Log: testutil.NopLogger()}
	response := markAttentionConversationRead(
		t,
		app,
		organization.ID,
		user.ID,
		conversation.ID,
		&visible.ID,
	)
	assert.True(t, response.LegacyStateSynced)
	require.NoError(t, db.First(&visible, "id = ?", visible.ID).Error)
	require.NoError(t, db.First(&corruptCrossAccount, "id = ?", corruptCrossAccount.ID).Error)
	assert.Equal(t, models.MessageStatusRead, visible.Status)
	assert.Equal(t, models.MessageStatusDelivered, corruptCrossAccount.Status)

	corruptResponse := markAttentionConversationRead(
		t,
		app,
		organization.ID,
		user.ID,
		conversation.ID,
		&corruptCrossAccount.ID,
	)
	assert.False(t, corruptResponse.LegacyStateSynced)
	require.NoError(t, db.First(&corruptCrossAccount, "id = ?", corruptCrossAccount.ID).Error)
	assert.Equal(t, models.MessageStatusDelivered, corruptCrossAccount.Status)
}

func TestDelayedProviderTimestampUsesIngestionOrderForUnreadAndTranscript(t *testing.T) {
	db := testutil.SetupTestDB(t)
	organization := testutil.CreateTestOrganization(t, db)
	role := testutil.CreateTestRoleWithKeys(
		t,
		db,
		organization.ID,
		"delayed-inbox-order-"+uuid.NewString(),
		[]string{models.ResourceConversations + ":" + models.ActionRead},
	)
	user := testutil.CreateTestUser(t, db, organization.ID, testutil.WithRoleID(&role.ID))
	enableBookingCommerceTestEntitlement(
		t, db, organization.ID, user.ID, "omnichannel.enabled",
	)
	contact := testutil.CreateTestContact(t, db, organization.ID)
	account := models.ChannelAccount{
		BaseModel:         models.BaseModel{ID: uuid.New()},
		OrganizationID:    organization.ID,
		Channel:           models.ChannelWhatsApp,
		Provider:          channelapi.RelayProvider,
		Name:              "Delayed order account",
		ExternalAccountID: "delayed-order-" + uuid.NewString(),
		Status:            models.ChannelAccountStatusActive,
		Capabilities:      models.JSONB{},
		Config:            models.JSONB{},
		Metadata:          models.JSONB{},
	}
	require.NoError(t, db.Create(&account).Error)
	conversation := createAttentionConversation(
		t, db, organization.ID, account.ID, contact.ID, "delayed-order",
	)
	providerTime := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	first := createAttentionMessage(
		t, db, organization.ID, account.Name, contact.ID, conversation.ID,
		models.DirectionIncoming, providerTime,
	)
	app := &App{DB: db, Log: testutil.NopLogger()}
	// Begin before the acknowledgement to prove PostgreSQL transaction-start
	// time is not used as the new row's ingestion position.
	earlyTransaction := db.Begin()
	require.NoError(t, earlyTransaction.Error)
	t.Cleanup(func() { _ = earlyTransaction.Rollback().Error })
	require.NoError(t, earlyTransaction.Exec("SELECT 1").Error)
	markAttentionConversationRead(
		t, app, organization.ID, user.ID, conversation.ID, &first.ID,
	)
	delayedConversationID := conversation.ID
	delayed := models.Message{
		BaseModel: models.BaseModel{
			ID:        uuid.New(),
			CreatedAt: providerTime.Add(-24 * time.Hour),
			UpdatedAt: providerTime.Add(-24 * time.Hour),
		},
		OrganizationID:      organization.ID,
		WhatsAppAccount:     account.Name,
		ContactID:           contact.ID,
		ConversationID:      conversation.ID.String(),
		InboxConversationID: &delayedConversationID,
		WhatsAppMessageID:   "delayed-" + uuid.NewString(),
		Direction:           models.DirectionIncoming,
		MessageType:         models.MessageTypeText,
		Content:             "Delayed provider message",
		Status:              models.MessageStatusReceived,
		Metadata:            models.JSONB{},
	}
	require.NoError(t, earlyTransaction.Create(&delayed).Error)
	require.NoError(t, earlyTransaction.Commit().Error)
	require.NoError(t, db.First(&delayed, "id = ?", delayed.ID).Error)
	require.NotNil(t, delayed.IngestedAt)
	require.NotNil(t, first.IngestedAt)
	assert.True(t, delayed.IngestedAt.After(*first.IngestedAt))

	unreadCount, err := countUnreadInboxMessages(
		db, organization.ID, user.ID, conversation.ID,
	)
	require.NoError(t, err)
	assert.Equal(t, int64(1), unreadCount)
	assert.Equal(t, int64(1), getAttentionSummary(t, app, organization.ID, user.ID).UnreadConversations)
	assert.Equal(t, 1, getListedConversationUnreadCounts(t, app, organization.ID, user.ID)[conversation.ID])

	request := testutil.NewGETRequest(t)
	testutil.SetAuthContext(request, organization.ID, user.ID)
	testutil.SetPathParam(request, "id", conversation.ID.String())
	testutil.SetQueryParam(request, "limit", 10)
	require.NoError(t, app.GetInboxConversationMessages(request))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(request))
	var response struct {
		Data struct {
			Messages []InboxMessageResponse `json:"messages"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(testutil.GetResponseBody(request), &response))
	require.Len(t, response.Data.Messages, 2)
	assert.Equal(t, delayed.ID, response.Data.Messages[0].Message.ID)
	assert.Equal(t, first.ID, response.Data.Messages[1].Message.ID)
	assert.Equal(t, delayed.CreatedAt, response.Data.Messages[0].Message.CreatedAt)
	assert.NotNil(t, response.Data.Messages[0].Message.IngestedAt)

	beforeRequest := testutil.NewGETRequest(t)
	testutil.SetAuthContext(beforeRequest, organization.ID, user.ID)
	testutil.SetPathParam(beforeRequest, "id", conversation.ID.String())
	testutil.SetQueryParam(beforeRequest, "before", delayed.ID.String())
	testutil.SetQueryParam(beforeRequest, "limit", 10)
	require.NoError(t, app.GetInboxConversationMessages(beforeRequest))
	var beforeResponse struct {
		Data struct {
			Messages []InboxMessageResponse `json:"messages"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(testutil.GetResponseBody(beforeRequest), &beforeResponse))
	require.Len(t, beforeResponse.Data.Messages, 1)
	assert.Equal(t, first.ID, beforeResponse.Data.Messages[0].Message.ID)
}

func TestConversationReadTriggerPreservesRollingAndDeletedMessageCursorSafety(t *testing.T) {
	db := testutil.SetupTestDB(t)
	organization := testutil.CreateTestOrganization(t, db)
	user := testutil.CreateTestUser(t, db, organization.ID)
	contact := testutil.CreateTestContact(t, db, organization.ID)
	account := models.ChannelAccount{
		BaseModel:         models.BaseModel{ID: uuid.New()},
		OrganizationID:    organization.ID,
		Channel:           models.ChannelWhatsApp,
		Provider:          channelapi.RelayProvider,
		Name:              "Rolling cursor account",
		ExternalAccountID: "rolling-cursor-" + uuid.NewString(),
		Status:            models.ChannelAccountStatusActive,
		Capabilities:      models.JSONB{},
		Config:            models.JSONB{},
		Metadata:          models.JSONB{},
	}
	require.NoError(t, db.Create(&account).Error)
	conversation := createAttentionConversation(
		t, db, organization.ID, account.ID, contact.ID, "rolling-cursor",
	)
	baseTime := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	first := createAttentionMessage(
		t, db, organization.ID, account.Name, contact.ID, conversation.ID,
		models.DirectionIncoming, baseTime,
	)
	second := createAttentionMessage(
		t, db, organization.ID, account.Name, contact.ID, conversation.ID,
		models.DirectionIncoming, baseTime.Add(time.Second),
	)
	third := createAttentionMessage(
		t, db, organization.ID, account.Name, contact.ID, conversation.ID,
		models.DirectionIncoming, baseTime.Add(2*time.Second),
	)

	ackStartedAt := time.Now().UTC()
	require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
		_, _, err := advanceConversationReadCursor(
			tx, organization.ID, user.ID, conversation.ID, &second, time.Time{},
		)
		return err
	}))
	cursor := loadConversationReadCursor(t, db, organization.ID, user.ID, conversation.ID)
	require.NotNil(t, cursor.LastReadIngestedAt)
	assert.Equal(t, second.EffectiveIngestedAt(), cursor.LastReadIngestedAt.UTC())
	assert.False(t, cursor.LastReadAt.Before(ackStartedAt))
	acknowledgedAt := cursor.LastReadAt

	// The cleanup marker is a transaction-local custom setting and can be set by
	// the application role. It must not authorize an ordinary direct cursor
	// rewrite: only the nested trigger fired by Message BEFORE DELETE may use it.
	require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec(
			"SELECT set_config('rereply.message_cursor_cleanup', ?, true)",
			second.ID.String(),
		).Error; err != nil {
			return err
		}
		return tx.Model(&models.ConversationRead{}).
			Where("id = ?", cursor.ID).
			Update("last_read_message_id", nil).Error
	}))
	cursor = loadConversationReadCursor(t, db, organization.ID, user.ID, conversation.ID)
	require.NotNil(t, cursor.LastReadMessageID)
	assert.Equal(t, second.ID, *cursor.LastReadMessageID)
	require.NotNil(t, cursor.LastReadIngestedAt)
	assert.Equal(t, second.EffectiveIngestedAt(), cursor.LastReadIngestedAt.UTC())

	// Simulate an old replica's unconditional conflict update. It omits the new
	// ingestion column and presents a stale message with a newer wall-clock ack.
	require.NoError(t, db.Exec(`
		INSERT INTO conversation_reads (
			id, organization_id, conversation_id, user_id, reader_key,
			last_read_message_id, last_read_external_id, last_read_at,
			metadata, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, '{}'::jsonb, clock_timestamp(), clock_timestamp())
		ON CONFLICT (organization_id, conversation_id, reader_key)
		DO UPDATE SET
			user_id = EXCLUDED.user_id,
			last_read_message_id = EXCLUDED.last_read_message_id,
			last_read_external_id = EXCLUDED.last_read_external_id,
			last_read_at = EXCLUDED.last_read_at,
			updated_at = clock_timestamp()
	`,
		uuid.New(), organization.ID, conversation.ID, user.ID,
		"user:"+user.ID.String(), first.ID, first.WhatsAppMessageID,
		time.Now().UTC().Add(time.Minute),
	).Error)
	cursor = loadConversationReadCursor(t, db, organization.ID, user.ID, conversation.ID)
	require.NotNil(t, cursor.LastReadMessageID)
	assert.Equal(t, second.ID, *cursor.LastReadMessageID)
	require.NotNil(t, cursor.LastReadIngestedAt)
	assert.Equal(t, second.EffectiveIngestedAt(), cursor.LastReadIngestedAt.UTC())
	assert.Equal(t, acknowledgedAt, cursor.LastReadAt)

	// A rolling old row whose new column is still NULL must retain the original
	// LastReadAt+message-ID fallback behavior.
	require.NoError(t, db.Model(&models.ConversationRead{}).
		Where("id = ?", cursor.ID).
		Update("last_read_ingested_at", nil).Error)
	unreadCount, err := countUnreadInboxMessages(
		db, organization.ID, user.ID, conversation.ID,
	)
	require.NoError(t, err)
	assert.Equal(t, int64(0), unreadCount, "wall-clock fallback is deliberately legacy-compatible")
	// Restore the durable ordering cursor before exercising the tenant-safe
	// composite FK cleanup. The referential action clears only the optional
	// message ID and must not acquire Conversation behind a held Message row.
	require.NoError(t, db.Model(&models.ConversationRead{}).
		Where("id = ?", cursor.ID).
		Update("last_read_ingested_at", second.EffectiveIngestedAt()).Error)

	heldConversation := db.Begin()
	require.NoError(t, heldConversation.Error)
	require.NoError(t, lockInboxConversationReadOrder(
		heldConversation, organization.ID, conversation.ID,
	))
	deleteDone := make(chan error, 1)
	go func() {
		deleteDone <- db.Session(&gorm.Session{NewDB: true}).
			Unscoped().
			Delete(&models.Message{}, "id = ?", second.ID).Error
	}()
	select {
	case err := <-deleteDone:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		_ = heldConversation.Rollback().Error
		require.NoError(t, <-deleteDone)
		require.Fail(t, "hard-delete FK cleanup waited on the conversation lock")
	}
	require.NoError(t, heldConversation.Commit().Error)
	cursor = loadConversationReadCursor(t, db, organization.ID, user.ID, conversation.ID)
	assert.Nil(t, cursor.LastReadMessageID)
	require.NotNil(t, cursor.LastReadIngestedAt)
	assert.Equal(t, second.EffectiveIngestedAt(), cursor.LastReadIngestedAt.UTC())
	unreadCount, err = countUnreadInboxMessages(
		db, organization.ID, user.ID, conversation.ID,
	)
	require.NoError(t, err)
	assert.Equal(t, int64(1), unreadCount)

	// A Message delete may itself be fired by a parent/retention trigger. In
	// that case the Message cleanup trigger depth is greater than one and the
	// nested ConversationRead trigger depth is greater than two. The provenance
	// guard must accept this exact nested marker while still rejecting the
	// direct depth-one spoof above.
	require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
		_, _, advanceErr := advanceConversationReadCursor(
			tx, organization.ID, user.ID, conversation.ID, &third, time.Time{},
		)
		return advanceErr
	}))
	require.NoError(t, db.Connection(func(session *gorm.DB) error {
		if err := session.Exec(`
			CREATE TEMP TABLE rereply_nested_message_delete_probe (
				message_id uuid NOT NULL
			) ON COMMIT PRESERVE ROWS
		`).Error; err != nil {
			return err
		}
		if err := session.Exec(`
			CREATE OR REPLACE FUNCTION pg_temp.rereply_nested_message_delete()
			RETURNS trigger
			LANGUAGE plpgsql
			AS $function$
			BEGIN
				DELETE FROM messages WHERE id = NEW.message_id;
				RETURN NEW;
			END
			$function$
		`).Error; err != nil {
			return err
		}
		if err := session.Exec(`
			CREATE TRIGGER trg_rereply_nested_message_delete
			AFTER INSERT ON rereply_nested_message_delete_probe
			FOR EACH ROW
			EXECUTE FUNCTION pg_temp.rereply_nested_message_delete()
		`).Error; err != nil {
			return err
		}
		return session.Exec(
			"INSERT INTO rereply_nested_message_delete_probe (message_id) VALUES (?)",
			third.ID,
		).Error
	}))
	cursor = loadConversationReadCursor(t, db, organization.ID, user.ID, conversation.ID)
	assert.Nil(t, cursor.LastReadMessageID)
	require.NotNil(t, cursor.LastReadIngestedAt)
	assert.Equal(t, third.EffectiveIngestedAt(), cursor.LastReadIngestedAt.UTC())
}

func TestAdvanceCursorLocksTargetMessageBeforeConversationReadDuringHardDelete(t *testing.T) {
	db := testutil.SetupTestDB(t)
	organization := testutil.CreateTestOrganization(t, db)
	user := testutil.CreateTestUser(t, db, organization.ID)
	contact := testutil.CreateTestContact(t, db, organization.ID)
	account := models.ChannelAccount{
		BaseModel:         models.BaseModel{ID: uuid.New()},
		OrganizationID:    organization.ID,
		Channel:           models.ChannelWhatsApp,
		Provider:          channelapi.RelayProvider,
		Name:              "Delete/read lock order account",
		ExternalAccountID: "delete-read-lock-" + uuid.NewString(),
		Status:            models.ChannelAccountStatusActive,
		Capabilities:      models.JSONB{},
		Config:            models.JSONB{},
		Metadata:          models.JSONB{},
	}
	require.NoError(t, db.Create(&account).Error)
	conversation := createAttentionConversation(
		t, db, organization.ID, account.ID, contact.ID, "delete-read-lock",
	)
	first := createAttentionMessage(
		t, db, organization.ID, account.Name, contact.ID, conversation.ID,
		models.DirectionIncoming, time.Now().UTC().Add(-time.Minute),
	)
	target := createAttentionMessage(
		t, db, organization.ID, account.Name, contact.ID, conversation.ID,
		models.DirectionIncoming, time.Now().UTC(),
	)
	require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
		_, _, err := advanceConversationReadCursor(
			tx, organization.ID, user.ID, conversation.ID, &first, time.Time{},
		)
		return err
	}))
	cursor := loadConversationReadCursor(t, db, organization.ID, user.ID, conversation.ID)

	// Delete the prospective cursor target but keep the transaction open. The
	// Message row is now exclusively locked and invisible only after commit.
	deleter := db.Begin()
	require.NoError(t, deleter.Error)
	t.Cleanup(func() { _ = deleter.Rollback().Error })
	require.NoError(t, deleter.Unscoped().Delete(
		&models.Message{}, "id = ? AND organization_id = ?", target.ID, organization.ID,
	).Error)

	readDone := make(chan error, 1)
	go func() {
		readDone <- db.Session(&gorm.Session{NewDB: true}).Transaction(func(tx *gorm.DB) error {
			_, _, err := advanceConversationReadCursor(
				tx, organization.ID, user.ID, conversation.ID, &target, time.Time{},
			)
			return err
		})
	}()
	select {
	case err := <-readDone:
		require.Failf(t, "cursor read bypassed the deleting Message lock", "result: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	// While the reader waits on Message, it must not own ConversationRead. This
	// is the central ordering guarantee that removes the prior FK deadlock.
	probe := db.Begin()
	require.NoError(t, probe.Error)
	t.Cleanup(func() { _ = probe.Rollback().Error })
	var probedCursorIDText string
	require.NoError(t, probe.Raw(`
		SELECT id
		FROM conversation_reads
		WHERE id = ?
		FOR UPDATE NOWAIT
	`, cursor.ID).Scan(&probedCursorIDText).Error)
	probedCursorID, err := uuid.Parse(probedCursorIDText)
	require.NoError(t, err)
	assert.Equal(t, cursor.ID, probedCursorID)
	require.NoError(t, probe.Rollback().Error)

	require.NoError(t, deleter.Commit().Error)
	select {
	case err := <-readDone:
		require.ErrorIs(t, err, errInboxReadCursorMessageNotFound)
	case <-time.After(2 * time.Second):
		require.Fail(t, "cursor read did not finish after the hard delete committed")
	}
	cursor = loadConversationReadCursor(t, db, organization.ID, user.ID, conversation.ID)
	require.NotNil(t, cursor.LastReadMessageID)
	assert.Equal(t, first.ID, *cursor.LastReadMessageID)
}

func TestOldCursorWriterFailsFastInsteadOfDeadlockingHardDelete(t *testing.T) {
	db := testutil.SetupTestDB(t)
	organization := testutil.CreateTestOrganization(t, db)
	user := testutil.CreateTestUser(t, db, organization.ID)
	contact := testutil.CreateTestContact(t, db, organization.ID)
	account := models.ChannelAccount{
		BaseModel:         models.BaseModel{ID: uuid.New()},
		OrganizationID:    organization.ID,
		Channel:           models.ChannelWhatsApp,
		Provider:          channelapi.RelayProvider,
		Name:              "Old writer delete account",
		ExternalAccountID: "old-writer-delete-" + uuid.NewString(),
		Status:            models.ChannelAccountStatusActive,
		Capabilities:      models.JSONB{},
		Config:            models.JSONB{},
		Metadata:          models.JSONB{},
	}
	require.NoError(t, db.Create(&account).Error)
	conversation := createAttentionConversation(
		t, db, organization.ID, account.ID, contact.ID, "old-writer-delete",
	)
	deletingTarget := createAttentionMessage(
		t, db, organization.ID, account.Name, contact.ID, conversation.ID,
		models.DirectionIncoming, time.Now().UTC().Add(-time.Minute),
	)
	newTarget := createAttentionMessage(
		t, db, organization.ID, account.Name, contact.ID, conversation.ID,
		models.DirectionIncoming, time.Now().UTC(),
	)
	require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
		_, _, err := advanceConversationReadCursor(
			tx, organization.ID, user.ID, conversation.ID, &deletingTarget, time.Time{},
		)
		return err
	}))
	// A true old-replica cursor has never populated the additive ingestion
	// column, so the rolling trigger must derive its OLD boundary from the
	// referenced Message while processing the update.
	require.NoError(t, db.Model(&models.ConversationRead{}).
		Where(
			"organization_id = ? AND conversation_id = ? AND reader_key = ?",
			organization.ID,
			conversation.ID,
			"user:"+user.ID.String(),
		).
		Update("last_read_ingested_at", nil).Error)
	cursor := loadConversationReadCursor(t, db, organization.ID, user.ID, conversation.ID)
	assert.Nil(t, cursor.LastReadIngestedAt)
	legacyBoundary := cursor.LastReadAt

	// Model an old replica after its UPDATE has acquired ConversationRead but
	// before its trigger-derived FK checks. The delete takes Message first and
	// then waits for this cursor row in the PG14 cleanup trigger.
	oldWriter := db.Begin()
	require.NoError(t, oldWriter.Error)
	t.Cleanup(func() { _ = oldWriter.Rollback().Error })
	var lockedCursor models.ConversationRead
	require.NoError(t, oldWriter.Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("id = ?", cursor.ID).
		First(&lockedCursor).Error)

	deletePID := make(chan int, 1)
	deleteDone := make(chan error, 1)
	go func() {
		deleteDone <- db.Connection(func(session *gorm.DB) error {
			deleter := session.Begin()
			if deleter.Error != nil {
				return deleter.Error
			}
			defer func() { _ = deleter.Rollback().Error }()
			var backendPID int
			if err := deleter.Raw("SELECT pg_backend_pid()").Scan(&backendPID).Error; err != nil {
				return err
			}
			deletePID <- backendPID
			if err := deleter.Unscoped().Delete(
				&models.Message{},
				"id = ? AND organization_id = ?",
				deletingTarget.ID,
				organization.ID,
			).Error; err != nil {
				return err
			}
			return deleter.Commit().Error
		})
	}()
	testutil.RequirePostgresBackendWaitingForLock(t, db, <-deletePID)

	// The old writer knows neither the application-side Message lock nor the
	// ingestion cursor column. Its trigger must NOWAIT on the deleting OLD target
	// and abort with lock_not_available, releasing ConversationRead so the delete
	// can complete instead of forming ConversationRead <-> Message deadlock.
	err := oldWriter.Exec(`
		UPDATE conversation_reads
		SET last_read_message_id = ?,
			last_read_at = clock_timestamp(),
			updated_at = clock_timestamp()
		WHERE id = ? AND organization_id = ?
	`, newTarget.ID, cursor.ID, organization.ID).Error
	require.Error(t, err)
	var postgresErr *pgconn.PgError
	require.ErrorAs(t, err, &postgresErr)
	assert.Equal(t, "55P03", postgresErr.Code)
	require.NoError(t, oldWriter.Rollback().Error)

	select {
	case err := <-deleteDone:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		require.Fail(t, "hard delete did not complete after the old writer failed fast")
	}
	cursor = loadConversationReadCursor(t, db, organization.ID, user.ID, conversation.ID)
	assert.Nil(t, cursor.LastReadMessageID)
	require.NotNil(t, cursor.LastReadIngestedAt)
	assert.True(t, legacyBoundary.Equal(cursor.LastReadIngestedAt.UTC()))
}

func TestConversationReadAndIncomingInsertSerializeOnConversationRow(t *testing.T) {
	db := testutil.SetupTestDB(t)
	organization := testutil.CreateTestOrganization(t, db)
	user := testutil.CreateTestUser(t, db, organization.ID)
	contact := testutil.CreateTestContact(t, db, organization.ID)
	account := models.ChannelAccount{
		BaseModel:         models.BaseModel{ID: uuid.New()},
		OrganizationID:    organization.ID,
		Channel:           models.ChannelWhatsApp,
		Provider:          channelapi.RelayProvider,
		Name:              "Serialized cursor account",
		ExternalAccountID: "serialized-cursor-" + uuid.NewString(),
		Status:            models.ChannelAccountStatusActive,
		Capabilities:      models.JSONB{},
		Config:            models.JSONB{},
		Metadata:          models.JSONB{},
	}
	require.NoError(t, db.Create(&account).Error)
	conversation := createAttentionConversation(
		t, db, organization.ID, account.ID, contact.ID, "serialized-cursor",
	)

	inbound := db.Begin()
	require.NoError(t, inbound.Error)
	t.Cleanup(func() { _ = inbound.Rollback().Error })
	conversationID := conversation.ID
	message := models.Message{
		BaseModel:           models.BaseModel{ID: uuid.New()},
		OrganizationID:      organization.ID,
		WhatsAppAccount:     account.Name,
		ContactID:           contact.ID,
		ConversationID:      conversation.ID.String(),
		InboxConversationID: &conversationID,
		WhatsAppMessageID:   "serialized-" + uuid.NewString(),
		Direction:           models.DirectionIncoming,
		MessageType:         models.MessageTypeText,
		Content:             "Uncommitted incoming message",
		Status:              models.MessageStatusReceived,
		Metadata:            models.JSONB{},
	}
	require.NoError(t, inbound.Create(&message).Error)

	readDone := make(chan error, 1)
	readStarted := make(chan int, 1)
	go func() {
		readDone <- db.Session(&gorm.Session{NewDB: true}).Transaction(func(tx *gorm.DB) error {
			var backendPID int
			if err := tx.Raw("SELECT pg_backend_pid()").Scan(&backendPID).Error; err != nil {
				return err
			}
			readStarted <- backendPID
			if err := lockInboxConversationReadOrder(tx, organization.ID, conversation.ID); err != nil {
				return err
			}
			var visible models.Message
			if err := tx.Where(
				"organization_id = ? AND inbox_conversation_id = ?",
				organization.ID,
				conversation.ID,
			).Order("COALESCE(ingested_at, created_at) DESC, id DESC").First(&visible).Error; err != nil {
				return err
			}
			_, _, err := advanceConversationReadCursor(
				tx, organization.ID, user.ID, conversation.ID, &visible, time.Time{},
			)
			return err
		})
	}()
	backendPID := <-readStarted
	testutil.RequirePostgresBackendWaitingForLock(t, db, backendPID)
	require.NoError(t, inbound.Commit().Error)
	select {
	case err := <-readDone:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		require.Fail(t, "serialized conversation read did not complete")
	}
	cursor := loadConversationReadCursor(t, db, organization.ID, user.ID, conversation.ID)
	require.NotNil(t, cursor.LastReadMessageID)
	assert.Equal(t, message.ID, *cursor.LastReadMessageID)
	unreadCount, err := countUnreadInboxMessages(
		db, organization.ID, user.ID, conversation.ID,
	)
	require.NoError(t, err)
	assert.Zero(t, unreadCount)
}

func TestLaterInboundAllocatesBeyondFutureCursorDespiteClockOrUUIDOrder(t *testing.T) {
	db := testutil.SetupTestDB(t)
	organization := testutil.CreateTestOrganization(t, db)
	user := testutil.CreateTestUser(t, db, organization.ID)
	contact := testutil.CreateTestContact(t, db, organization.ID)
	account := models.ChannelAccount{
		BaseModel:         models.BaseModel{ID: uuid.New()},
		OrganizationID:    organization.ID,
		Channel:           models.ChannelWhatsApp,
		Provider:          channelapi.RelayProvider,
		Name:              "Monotonic ingestion account",
		ExternalAccountID: "monotonic-ingestion-" + uuid.NewString(),
		Status:            models.ChannelAccountStatusActive,
		Capabilities:      models.JSONB{},
		Config:            models.JSONB{},
		Metadata:          models.JSONB{},
	}
	require.NoError(t, db.Create(&account).Error)
	conversation := createAttentionConversation(
		t, db, organization.ID, account.ID, contact.ID, "monotonic-ingestion",
	)
	first := createAttentionMessage(
		t,
		db,
		organization.ID,
		account.Name,
		contact.ID,
		conversation.ID,
		models.DirectionIncoming,
		time.Now().UTC().Add(-24*time.Hour),
	)
	// Simulate a clock value already ahead of this replica. The next UUID is
	// deliberately the smallest possible non-zero value, so a timestamp tie
	// cannot be rescued by the ID tie-break.
	futureIngestion := time.Now().UTC().Add(24 * time.Hour).Truncate(time.Microsecond)
	require.NoError(t, db.Model(&models.Message{}).
		Where("id = ? AND organization_id = ?", first.ID, organization.ID).
		Update("ingested_at", futureIngestion).Error)
	require.NoError(t, db.First(&first, "id = ?", first.ID).Error)
	require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
		_, _, err := advanceConversationReadCursor(
			tx,
			organization.ID,
			user.ID,
			conversation.ID,
			&first,
			time.Now().UTC(),
		)
		return err
	}))

	conversationID := conversation.ID
	later := models.Message{
		BaseModel: models.BaseModel{
			ID:        uuid.MustParse("00000000-0000-0000-0000-000000000001"),
			CreatedAt: time.Now().UTC().Add(-7 * 24 * time.Hour),
			UpdatedAt: time.Now().UTC(),
		},
		OrganizationID:      organization.ID,
		WhatsAppAccount:     account.Name,
		ContactID:           contact.ID,
		ConversationID:      conversation.ID.String(),
		InboxConversationID: &conversationID,
		WhatsAppMessageID:   "monotonic-later-" + uuid.NewString(),
		Direction:           models.DirectionIncoming,
		MessageType:         models.MessageTypeText,
		Content:             "Later accepted inbound with an older provider timestamp",
		Status:              models.MessageStatusReceived,
		Metadata:            models.JSONB{},
	}
	require.NoError(t, db.Create(&later).Error)
	require.NoError(t, db.First(&later, "id = ?", later.ID).Error)
	require.NotNil(t, later.IngestedAt)
	assert.True(t, later.IngestedAt.After(futureIngestion), "accepted order must advance beyond the active cursor")
	unreadCount, err := countUnreadInboxMessages(
		db, organization.ID, user.ID, conversation.ID,
	)
	require.NoError(t, err)
	assert.Equal(t, int64(1), unreadCount)
}

func TestOldTargetlessReadCannotHideConcurrentFirstInbound(t *testing.T) {
	db := testutil.SetupTestDB(t)
	organization := testutil.CreateTestOrganization(t, db)
	user := testutil.CreateTestUser(t, db, organization.ID)
	contact := testutil.CreateTestContact(t, db, organization.ID)
	account := models.ChannelAccount{
		BaseModel:         models.BaseModel{ID: uuid.New()},
		OrganizationID:    organization.ID,
		Channel:           models.ChannelWhatsApp,
		Provider:          channelapi.RelayProvider,
		Name:              "Targetless cursor account",
		ExternalAccountID: "targetless-cursor-" + uuid.NewString(),
		Status:            models.ChannelAccountStatusActive,
		Capabilities:      models.JSONB{},
		Config:            models.JSONB{},
		Metadata:          models.JSONB{},
	}
	require.NoError(t, db.Create(&account).Error)
	conversation := createAttentionConversation(
		t, db, organization.ID, account.ID, contact.ID, "targetless-cursor",
	)

	oldReader := db.Begin()
	require.NoError(t, oldReader.Error)
	t.Cleanup(func() { _ = oldReader.Rollback().Error })
	var oldVisibleCount int64
	require.NoError(t, oldReader.Model(&models.Message{}).Where(
		"organization_id = ? AND inbox_conversation_id = ?",
		organization.ID,
		conversation.ID,
	).Count(&oldVisibleCount).Error)
	require.Zero(t, oldVisibleCount)

	message := createAttentionMessage(
		t,
		db,
		organization.ID,
		account.Name,
		contact.ID,
		conversation.ID,
		models.DirectionIncoming,
		time.Now().UTC(),
	)
	require.NoError(t, oldReader.Exec(`
		INSERT INTO conversation_reads (
			id, organization_id, conversation_id, user_id, reader_key,
			last_read_at, metadata, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, clock_timestamp(), '{}'::jsonb, clock_timestamp(), clock_timestamp())
	`, uuid.New(), organization.ID, conversation.ID, user.ID, "user:"+user.ID.String()).Error)
	require.NoError(t, oldReader.Commit().Error)

	cursor := loadConversationReadCursor(t, db, organization.ID, user.ID, conversation.ID)
	assert.Nil(t, cursor.LastReadMessageID)
	require.NotNil(t, cursor.LastReadIngestedAt)
	assert.Equal(t, time.Unix(0, 0).UTC(), cursor.LastReadIngestedAt.UTC())
	assert.Equal(t, time.Unix(0, 0).UTC(), cursor.LastReadAt.UTC())
	unreadCount, err := countUnreadInboxMessages(
		db, organization.ID, user.ID, conversation.ID,
	)
	require.NoError(t, err)
	assert.Equal(t, int64(1), unreadCount)
	var oldReplicaUnread int64
	require.NoError(t, db.Raw(`
		SELECT COUNT(*)
		FROM messages
		WHERE organization_id = ?
		  AND inbox_conversation_id = ?
		  AND deleted_at IS NULL
		  AND direction = ?
		  AND created_at > ?
	`,
		organization.ID,
		conversation.ID,
		models.DirectionIncoming,
		cursor.LastReadAt,
	).Scan(&oldReplicaUnread).Error)
	assert.Equal(t, int64(1), oldReplicaUnread)
	_ = message
}

func TestMessageIngestionMigrationInstallsRollingSafePostgresContract(t *testing.T) {
	db := testutil.SetupTestDB(t)
	// Re-run the exact preparation + model migration sequence to prove GORM does
	// not recreate the retired single-column SET NULL relationship.
	require.NoError(t, database.PrepareMessageIngestionOrder(db))
	require.NoError(t, db.AutoMigrate(&models.ConversationRead{}))
	type columnContract struct {
		ColumnName    string `gorm:"column:column_name"`
		IsNullable    string `gorm:"column:is_nullable"`
		ColumnDefault string `gorm:"column:column_default"`
	}
	var messageColumn columnContract
	require.NoError(t, db.Raw(`
		SELECT column_name, is_nullable, COALESCE(column_default, '') AS column_default
		FROM information_schema.columns
		WHERE table_schema = current_schema()
		  AND table_name = 'messages'
		  AND column_name = 'ingested_at'
	`).Scan(&messageColumn).Error)
	assert.Equal(t, "ingested_at", messageColumn.ColumnName)
	assert.Equal(t, "YES", messageColumn.IsNullable)
	assert.Contains(t, messageColumn.ColumnDefault, "clock_timestamp")

	var readColumn columnContract
	require.NoError(t, db.Raw(`
		SELECT column_name, is_nullable, COALESCE(column_default, '') AS column_default
		FROM information_schema.columns
		WHERE table_schema = current_schema()
		  AND table_name = 'conversation_reads'
		  AND column_name = 'last_read_ingested_at'
	`).Scan(&readColumn).Error)
	assert.Equal(t, "last_read_ingested_at", readColumn.ColumnName)
	assert.Equal(t, "YES", readColumn.IsNullable)

	var triggers []string
	require.NoError(t, db.Raw(`
		SELECT DISTINCT trigger_name
		FROM information_schema.triggers
		WHERE event_object_schema = current_schema()
		  AND trigger_name IN (
			'trg_messages_ingestion_order',
			'trg_conversation_reads_ingestion_order',
			'trg_messages_cleanup_read_cursors'
		  )
		ORDER BY trigger_name
	`).Pluck("trigger_name", &triggers).Error)
	assert.Equal(t, []string{
		"trg_conversation_reads_ingestion_order",
		"trg_messages_cleanup_read_cursors",
		"trg_messages_ingestion_order",
	}, triggers)
	var readTriggerDefinition string
	require.NoError(t, db.Raw(`
		SELECT pg_get_functiondef(
			'rereply_set_conversation_read_ingestion_order()'::regprocedure
		)
	`).Scan(&readTriggerDefinition).Error)
	assert.Contains(t, readTriggerDefinition, "pg_trigger_depth() >= 2")
	assert.Contains(t, readTriggerDefinition, "FOR UPDATE NOWAIT")
	assert.Contains(t, readTriggerDefinition, "FOR KEY SHARE NOWAIT")

	type foreignKeyContract struct {
		Name         string `gorm:"column:conname"`
		DeleteAction string `gorm:"column:delete_action"`
	}
	var messageForeignKeys []foreignKeyContract
	require.NoError(t, db.Raw(`
		SELECT DISTINCT foreign_key.conname,
		       CAST(foreign_key.confdeltype AS text) AS delete_action
		FROM pg_catalog.pg_constraint AS foreign_key
		JOIN pg_catalog.pg_attribute AS child_column
		  ON child_column.attrelid = foreign_key.conrelid
		 AND child_column.attnum = ANY(foreign_key.conkey)
		WHERE foreign_key.contype = 'f'
		  AND foreign_key.conrelid = 'conversation_reads'::regclass
		  AND foreign_key.confrelid = 'messages'::regclass
		  AND child_column.attname = 'last_read_message_id'
		ORDER BY foreign_key.conname
	`).Scan(&messageForeignKeys).Error)
	require.Equal(t, []foreignKeyContract{{
		Name:         "fk_conversation_reads_message_tenant",
		DeleteAction: "r",
	}}, messageForeignKeys, "one tenant-composite RESTRICT FK must own the cursor target")

	var invalidIndexes int64
	require.NoError(t, db.Raw(`
		SELECT COUNT(*)
		FROM pg_catalog.pg_index AS index_state
		JOIN pg_catalog.pg_class AS index_class
		  ON index_class.oid = index_state.indexrelid
		WHERE index_class.relname IN (
			'idx_messages_org_inbox_ingested',
			'idx_messages_org_inbox_ingested_highwater',
			'idx_messages_org_inbox_incoming_ingested',
			'idx_messages_org_contact_ingested',
			'idx_messages_org_contact_account_ingested'
		)
		  AND NOT index_state.indisvalid
	`).Scan(&invalidIndexes).Error)
	assert.Zero(t, invalidIndexes)
}

func TestContactReadStateCannotRaceConcurrentIncomingInsertion(t *testing.T) {
	db := testutil.SetupTestDB(t)
	organization := testutil.CreateTestOrganization(t, db)
	contact := testutil.CreateTestContact(t, db, organization.ID)
	account := models.ChannelAccount{
		BaseModel:         models.BaseModel{ID: uuid.New()},
		OrganizationID:    organization.ID,
		Channel:           models.ChannelWhatsApp,
		Provider:          channelapi.RelayProvider,
		Name:              "Contact read race account",
		ExternalAccountID: "contact-read-race-" + uuid.NewString(),
		Status:            models.ChannelAccountStatusActive,
		Capabilities:      models.JSONB{},
		Config:            models.JSONB{},
		Metadata:          models.JSONB{},
	}
	require.NoError(t, db.Create(&account).Error)
	conversation := createAttentionConversation(
		t, db, organization.ID, account.ID, contact.ID, "contact-read-race",
	)
	older := createAttentionMessage(
		t,
		db,
		organization.ID,
		account.Name,
		contact.ID,
		conversation.ID,
		models.DirectionIncoming,
		time.Now().UTC().Add(-time.Minute),
	)

	inbound := db.Begin()
	require.NoError(t, inbound.Error)
	t.Cleanup(func() { _ = inbound.Rollback().Error })
	conversationID := conversation.ID
	newer := models.Message{
		BaseModel:           models.BaseModel{ID: uuid.New()},
		OrganizationID:      organization.ID,
		WhatsAppAccount:     account.Name,
		ContactID:           contact.ID,
		ConversationID:      conversation.ID.String(),
		InboxConversationID: &conversationID,
		Direction:           models.DirectionIncoming,
		MessageType:         models.MessageTypeText,
		Content:             "Concurrent unread message",
		Status:              models.MessageStatusReceived,
		Metadata:            models.JSONB{},
	}
	require.NoError(t, inbound.Create(&newer).Error)

	app := &App{DB: db, Log: testutil.NopLogger()}
	markDone := make(chan error, 1)
	go func() {
		markDone <- app.markSelectedMessagesAsRead(
			organization.ID,
			contact.ID,
			contact,
			[]models.Message{older},
			false,
		)
	}()
	select {
	case err := <-markDone:
		require.Failf(t, "read-state update bypassed conversation lock", "result: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	require.NoError(t, inbound.Commit().Error)
	select {
	case err := <-markDone:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		require.Fail(t, "contact read-state update did not complete")
	}

	require.NoError(t, db.First(&older, "id = ?", older.ID).Error)
	require.NoError(t, db.First(&newer, "id = ?", newer.ID).Error)
	require.NoError(t, db.First(contact, "id = ?", contact.ID).Error)
	assert.Equal(t, models.MessageStatusRead, older.Status)
	assert.NotEqual(t, models.MessageStatusRead, newer.Status)
	assert.False(t, contact.IsRead)
}

type markAttentionReadResponse struct {
	ReadAt             time.Time  `json:"read_at"`
	LastReadIngestedAt *time.Time `json:"last_read_ingested_at"`
	LastReadMessageID  *uuid.UUID `json:"last_read_message_id"`
	UnreadCount        int64      `json:"unread_count"`
	ProviderSynced     bool       `json:"provider_synced"`
	ProviderSyncQueued bool       `json:"provider_sync_queued"`
	LegacyStateSynced  bool       `json:"legacy_state_synced"`
}

func markAttentionConversationRead(
	t *testing.T,
	app *App,
	organizationID, userID, conversationID uuid.UUID,
	lastVisibleMessageID *uuid.UUID,
) markAttentionReadResponse {
	t.Helper()
	request := testutil.NewJSONRequest(t, MarkInboxConversationReadRequest{
		LastVisibleMessageID: lastVisibleMessageID,
	})
	testutil.SetAuthContext(request, organizationID, userID)
	testutil.SetPathParam(request, "id", conversationID.String())
	require.NoError(t, app.MarkInboxConversationRead(request))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(request))

	var response struct {
		Data markAttentionReadResponse `json:"data"`
	}
	require.NoError(t, json.Unmarshal(testutil.GetResponseBody(request), &response))
	return response.Data
}

func createAttentionConversation(
	t *testing.T,
	db *gorm.DB,
	organizationID, accountID, contactID uuid.UUID,
	externalID string,
) models.InboxConversation {
	t.Helper()
	conversation := models.InboxConversation{
		BaseModel:              models.BaseModel{ID: uuid.New()},
		OrganizationID:         organizationID,
		ChannelAccountID:       accountID,
		ContactID:              contactID,
		Channel:                models.ChannelWhatsApp,
		ExternalConversationID: externalID + "-" + uuid.NewString(),
		Status:                 models.InboxConversationStatusOpen,
		OpenedAt:               time.Now().UTC(),
		Config:                 models.JSONB{},
		Metadata:               models.JSONB{},
	}
	require.NoError(t, db.Create(&conversation).Error)
	return conversation
}

func createAttentionMessage(
	t *testing.T,
	db *gorm.DB,
	organizationID uuid.UUID,
	accountName string,
	contactID, conversationID uuid.UUID,
	direction models.Direction,
	createdAt time.Time,
) models.Message {
	t.Helper()
	conversationIDCopy := conversationID
	message := models.Message{
		BaseModel: models.BaseModel{
			ID:        uuid.New(),
			CreatedAt: createdAt,
			UpdatedAt: createdAt,
		},
		OrganizationID:      organizationID,
		WhatsAppAccount:     accountName,
		ContactID:           contactID,
		ConversationID:      conversationID.String(),
		InboxConversationID: &conversationIDCopy,
		Direction:           direction,
		MessageType:         models.MessageTypeText,
		Content:             "Synthetic attention message",
		Status:              models.MessageStatusReceived,
		Metadata:            models.JSONB{},
	}
	require.NoError(t, db.Create(&message).Error)
	// Most cursor tests need deterministic tuples. Production trigger behavior
	// is exercised separately; updating only ingested_at does not invoke it.
	require.NoError(t, db.Model(&models.Message{}).
		Where("id = ? AND organization_id = ?", message.ID, organizationID).
		Update("ingested_at", createdAt.UTC()).Error)
	createdAtUTC := createdAt.UTC()
	message.IngestedAt = &createdAtUTC
	return message
}

func getAttentionSummary(
	t *testing.T,
	app *App,
	organizationID, userID uuid.UUID,
) InboxAttentionSummaryResponse {
	t.Helper()
	request := testutil.NewGETRequest(t)
	testutil.SetAuthContext(request, organizationID, userID)
	require.NoError(t, app.GetInboxAttentionSummary(request))
	assert.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(request))

	var response struct {
		Data InboxAttentionSummaryResponse `json:"data"`
	}
	require.NoError(t, json.Unmarshal(testutil.GetResponseBody(request), &response))
	return response.Data
}

func getListedConversationUnreadCounts(
	t *testing.T,
	app *App,
	organizationID, userID uuid.UUID,
) map[uuid.UUID]int {
	t.Helper()
	request := testutil.NewGETRequest(t)
	testutil.SetAuthContext(request, organizationID, userID)
	testutil.SetQueryParam(request, "limit", 100)
	require.NoError(t, app.ListInboxConversations(request))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(request))

	var response struct {
		Data struct {
			Conversations []InboxConversationResponse `json:"conversations"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(testutil.GetResponseBody(request), &response))
	counts := make(map[uuid.UUID]int, len(response.Data.Conversations))
	for i := range response.Data.Conversations {
		conversation := response.Data.Conversations[i]
		counts[conversation.ID] = conversation.UnreadCount
	}
	return counts
}
