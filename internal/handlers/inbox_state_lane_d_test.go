package handlers

import (
	"encoding/json"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/models"
	appwebsocket "github.com/shridarpatil/whatomate/internal/websocket"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

type laneDContactList struct {
	Contacts []ContactResponse `json:"contacts"`
	Total    int64             `json:"total"`
}

func laneDListContacts(
	t *testing.T,
	app *App,
	organizationID, userID uuid.UUID,
	params map[string]string,
) (laneDContactList, int) {
	t.Helper()
	request := testutil.NewGETRequest(t)
	testutil.SetAuthContext(request, organizationID, userID)
	for key, value := range params {
		testutil.SetQueryParam(request, key, value)
	}
	require.NoError(t, app.ListContacts(request))
	status := testutil.GetResponseStatusCode(request)
	var envelope struct {
		Data laneDContactList `json:"data"`
	}
	if status == fasthttp.StatusOK {
		require.NoError(t, json.Unmarshal(testutil.GetResponseBody(request), &envelope))
	}
	return envelope.Data, status
}

func laneDContactIDs(list laneDContactList) []uuid.UUID {
	ids := make([]uuid.UUID, 0, len(list.Contacts))
	for _, contact := range list.Contacts {
		ids = append(ids, contact.ID)
	}
	return ids
}

func laneDContactByID(t *testing.T, list laneDContactList, contactID uuid.UUID) ContactResponse {
	t.Helper()
	for _, contact := range list.Contacts {
		if contact.ID == contactID {
			return contact
		}
	}
	t.Fatalf("contact %s missing from list", contactID)
	return ContactResponse{}
}

func laneDCreateMessage(
	t *testing.T,
	db *gorm.DB,
	organizationID uuid.UUID,
	accountName string,
	contactID uuid.UUID,
	direction models.Direction,
	status models.MessageStatus,
) models.Message {
	t.Helper()
	message := models.Message{
		BaseModel:         models.BaseModel{ID: uuid.New()},
		OrganizationID:    organizationID,
		WhatsAppAccount:   accountName,
		ContactID:         contactID,
		ConversationID:    "lane-d-" + uuid.NewString(),
		WhatsAppMessageID: "wamid.lane-d." + uuid.NewString(),
		Direction:         direction,
		MessageType:       models.MessageTypeText,
		Content:           "lane D message",
		Status:            status,
		Metadata:          models.JSONB{},
	}
	require.NoError(t, db.Create(&message).Error)
	return message
}

func laneDCreateTransfer(
	t *testing.T,
	db *gorm.DB,
	contact *models.Contact,
	accountName string,
	agentID *uuid.UUID,
	status models.TransferStatus,
) {
	t.Helper()
	require.NoError(t, db.Create(&models.AgentTransfer{
		BaseModel:       models.BaseModel{ID: uuid.New()},
		OrganizationID:  contact.OrganizationID,
		ContactID:       contact.ID,
		WhatsAppAccount: accountName,
		PhoneNumber:     contact.PhoneNumber,
		Status:          status,
		Source:          models.TransferSourceManual,
		AgentID:         agentID,
		TransferredAt:   time.Now().UTC(),
	}).Error)
}

func laneDMarkRead(
	t *testing.T,
	app *App,
	organizationID, userID, contactID uuid.UUID,
	body map[string]any,
) (map[string]any, int) {
	t.Helper()
	request := testutil.NewJSONRequest(t, body)
	testutil.SetAuthContext(request, organizationID, userID)
	testutil.SetPathParam(request, "id", contactID.String())
	require.NoError(t, app.MarkContactRead(request))
	status := testutil.GetResponseStatusCode(request)
	var envelope struct {
		Data map[string]any `json:"data"`
	}
	if status == fasthttp.StatusOK {
		require.NoError(t, json.Unmarshal(testutil.GetResponseBody(request), &envelope))
	}
	return envelope.Data, status
}

func laneDManualUnreadStored(t *testing.T, db *gorm.DB, contactID uuid.UUID) bool {
	t.Helper()
	var count int64
	require.NoError(t, db.Model(&models.Contact{}).
		Where("id = ? AND metadata @> CAST(? AS jsonb)", contactID, contactManualUnreadJSON()).
		Count(&count).Error)
	return count == 1
}

func laneDSetManualUnread(t *testing.T, db *gorm.DB, contact *models.Contact, extra models.JSONB) {
	t.Helper()
	metadata := models.JSONB{contactManualUnreadKey: true}
	for key, value := range extra {
		metadata[key] = value
	}
	require.NoError(t, db.Model(&models.Contact{}).
		Where("id = ?", contact.ID).
		Update("metadata", metadata).Error)
}

func laneDHub(t *testing.T, organizationID uuid.UUID) (*appwebsocket.Hub, *appwebsocket.Client) {
	t.Helper()
	log := testutil.NopLogger()
	hub := appwebsocket.NewHub(log)
	go hub.Run()
	client := appwebsocket.NewClient(hub, nil, uuid.New(), organizationID)
	hub.Register(client)
	testutil.AssertEventually(t, func() bool { return hub.GetClientCount() == 1 }, 2*time.Second, "client registers")
	return hub, client
}

func laneDExpectContactUpdate(t *testing.T, client *appwebsocket.Client, contactID uuid.UUID, kind string) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case data := <-client.SendChan():
			var envelope struct {
				Type    string         `json:"type"`
				Payload map[string]any `json:"payload"`
			}
			require.NoError(t, json.Unmarshal(data, &envelope))
			if envelope.Type != appwebsocket.TypeContactUpdate {
				continue
			}
			assert.Equal(t, contactID.String(), envelope.Payload["contact_id"])
			assert.Equal(t, kind, envelope.Payload["kind"])
			assert.Len(t, envelope.Payload, 2, "the org-wide payload carries only the contact ID and kind")
			return
		case <-deadline:
			t.Fatalf("timed out waiting for contact_update (%s)", kind)
		}
	}
}

// laneDExpectNoContactUpdate fails if a contact_update arrives within wait.
func laneDExpectNoContactUpdate(t *testing.T, client *appwebsocket.Client, wait time.Duration) {
	t.Helper()
	deadline := time.After(wait)
	for {
		select {
		case data := <-client.SendChan():
			var envelope struct {
				Type    string         `json:"type"`
				Payload map[string]any `json:"payload"`
			}
			require.NoError(t, json.Unmarshal(data, &envelope))
			if envelope.Type == appwebsocket.TypeContactUpdate {
				t.Fatalf("unexpected contact_update: %v", envelope.Payload)
			}
		case <-deadline:
			return
		}
	}
}

func TestLaneDListContactsUnreadFilterAndGroupedCounts(t *testing.T) {
	db := testutil.SetupTestDB(t)
	organization := testutil.CreateTestOrganization(t, db)
	otherOrganization := testutil.CreateTestOrganization(t, db)
	adminRole := testutil.CreateAdminRole(t, db, organization.ID)
	user := testutil.CreateTestUser(t, db, organization.ID, testutil.WithRoleID(&adminRole.ID))
	account := testutil.CreateTestWhatsAppAccount(t, db, organization.ID)
	otherAccount := testutil.CreateTestWhatsAppAccount(t, db, otherOrganization.ID)

	unread := testutil.CreateTestContactWith(t, db, organization.ID, testutil.WithContactAccount(account.Name))
	laneDCreateMessage(t, db, organization.ID, account.Name, unread.ID, models.DirectionIncoming, models.MessageStatusDelivered)
	laneDCreateMessage(t, db, organization.ID, account.Name, unread.ID, models.DirectionIncoming, models.MessageStatusReceived)
	laneDCreateMessage(t, db, organization.ID, account.Name, unread.ID, models.DirectionOutgoing, models.MessageStatusSent)

	read := testutil.CreateTestContactWith(t, db, organization.ID, testutil.WithContactAccount(account.Name))
	laneDCreateMessage(t, db, organization.ID, account.Name, read.ID, models.DirectionIncoming, models.MessageStatusRead)

	manual := testutil.CreateTestContactWith(t, db, organization.ID, testutil.WithContactAccount(account.Name))
	laneDSetManualUnread(t, db, manual, models.JSONB{"source": "import"})

	deleted := testutil.CreateTestContactWith(t, db, organization.ID, testutil.WithContactAccount(account.Name))
	deletedMessage := laneDCreateMessage(t, db, organization.ID, account.Name, deleted.ID, models.DirectionIncoming, models.MessageStatusDelivered)
	require.NoError(t, db.Delete(&deletedMessage).Error)

	outgoingOnly := testutil.CreateTestContactWith(t, db, organization.ID, testutil.WithContactAccount(account.Name))
	laneDCreateMessage(t, db, organization.ID, account.Name, outgoingOnly.ID, models.DirectionOutgoing, models.MessageStatusDelivered)

	foreign := testutil.CreateTestContactWith(t, db, otherOrganization.ID, testutil.WithContactAccount(otherAccount.Name))
	laneDCreateMessage(t, db, otherOrganization.ID, otherAccount.Name, foreign.ID, models.DirectionIncoming, models.MessageStatusDelivered)

	app := &App{DB: db, Log: testutil.NopLogger()}

	all, status := laneDListContacts(t, app, organization.ID, user.ID, nil)
	require.Equal(t, fasthttp.StatusOK, status)
	assert.Equal(t, int64(5), all.Total)
	assert.Equal(t, 2, laneDContactByID(t, all, unread.ID).UnreadCount)
	assert.Zero(t, laneDContactByID(t, all, read.ID).UnreadCount)
	assert.Zero(t, laneDContactByID(t, all, deleted.ID).UnreadCount, "deleted messages are not unread")
	assert.Zero(t, laneDContactByID(t, all, outgoingOnly.ID).UnreadCount)
	manualRow := laneDContactByID(t, all, manual.ID)
	assert.Zero(t, manualRow.UnreadCount)
	assert.True(t, manualRow.ManualUnread)
	assert.Equal(t, map[string]any{"source": "import"}, manualRow.Metadata, "the internal flag is not exposed as metadata")
	assert.False(t, laneDContactByID(t, all, unread.ID).ManualUnread)

	filtered, status := laneDListContacts(t, app, organization.ID, user.ID, map[string]string{"unread": "true"})
	require.Equal(t, fasthttp.StatusOK, status)
	assert.Equal(t, int64(2), filtered.Total)
	assert.ElementsMatch(t, []uuid.UUID{unread.ID, manual.ID}, laneDContactIDs(filtered))

	paged, status := laneDListContacts(t, app, organization.ID, user.ID, map[string]string{"unread": "true", "limit": "1"})
	require.Equal(t, fasthttp.StatusOK, status)
	assert.Equal(t, int64(2), paged.Total, "the filter applies before Count")
	assert.Len(t, paged.Contacts, 1)

	notFiltered, status := laneDListContacts(t, app, organization.ID, user.ID, map[string]string{"unread": "false"})
	require.Equal(t, fasthttp.StatusOK, status)
	assert.Equal(t, int64(5), notFiltered.Total)

	var stored models.Contact
	require.NoError(t, db.First(&stored, "id = ?", manual.ID).Error)
	single := app.buildContactResponse(&stored, organization.ID)
	assert.True(t, single.ManualUnread)
	assert.Equal(t, models.JSONB{"source": "import"}, single.Metadata)
	assert.Equal(t, models.JSONB{contactManualUnreadKey: true, "source": "import"}, stored.Metadata, "the stored row keeps the flag")
	stored = models.Contact{}
	require.NoError(t, db.First(&stored, "id = ?", unread.ID).Error)
	assert.Equal(t, 2, app.buildContactResponse(&stored, organization.ID).UnreadCount)
}

func TestLaneDListContactsAssignedFilterAndAssigneeNames(t *testing.T) {
	db := testutil.SetupTestDB(t)
	organization := testutil.CreateTestOrganization(t, db)
	adminRole := testutil.CreateAdminRole(t, db, organization.ID)
	me := testutil.CreateTestUser(t, db, organization.ID, testutil.WithRoleID(&adminRole.ID), testutil.WithFullName("Aisyah Rahman"))
	teammate := testutil.CreateTestUser(t, db, organization.ID, testutil.WithFullName("Ben Lim"))
	account := testutil.CreateTestWhatsAppAccount(t, db, organization.ID)
	newContact := func() *models.Contact {
		return testutil.CreateTestContactWith(t, db, organization.ID, testutil.WithContactAccount(account.Name))
	}

	assignedToMe := newContact()
	require.NoError(t, db.Model(assignedToMe).Update("assigned_user_id", me.ID).Error)
	laneDCreateMessage(t, db, organization.ID, account.Name, assignedToMe.ID, models.DirectionIncoming, models.MessageStatusDelivered)
	transferredToMe := newContact()
	laneDCreateTransfer(t, db, transferredToMe, account.Name, &me.ID, models.TransferStatusActive)
	assignedToTeammate := newContact()
	require.NoError(t, db.Model(assignedToTeammate).Update("assigned_user_id", teammate.ID).Error)
	teammateButTransferredToMe := newContact()
	require.NoError(t, db.Model(teammateButTransferredToMe).Update("assigned_user_id", teammate.ID).Error)
	laneDCreateTransfer(t, db, teammateButTransferredToMe, account.Name, &me.ID, models.TransferStatusActive)
	nobody := newContact()
	queued := newContact()
	laneDCreateTransfer(t, db, queued, account.Name, nil, models.TransferStatusActive)
	resumed := newContact()
	laneDCreateTransfer(t, db, resumed, account.Name, &me.ID, models.TransferStatusResumed)

	app := &App{DB: db, Log: testutil.NopLogger()}

	mine, status := laneDListContacts(t, app, organization.ID, me.ID, map[string]string{"assigned": "me"})
	require.Equal(t, fasthttp.StatusOK, status)
	assert.Equal(t, int64(3), mine.Total)
	assert.ElementsMatch(t, []uuid.UUID{assignedToMe.ID, transferredToMe.ID, teammateButTransferredToMe.ID}, laneDContactIDs(mine))

	unassigned, status := laneDListContacts(t, app, organization.ID, me.ID, map[string]string{"assigned": "unassigned"})
	require.Equal(t, fasthttp.StatusOK, status)
	assert.Equal(t, int64(3), unassigned.Total)
	assert.ElementsMatch(t, []uuid.UUID{nobody.ID, queued.ID, resumed.ID}, laneDContactIDs(unassigned))

	mineUnread, status := laneDListContacts(t, app, organization.ID, me.ID, map[string]string{"assigned": "me", "unread": "true"})
	require.Equal(t, fasthttp.StatusOK, status)
	assert.Equal(t, []uuid.UUID{assignedToMe.ID}, laneDContactIDs(mineUnread))

	_, status = laneDListContacts(t, app, organization.ID, me.ID, map[string]string{"assigned": "everyone"})
	assert.Equal(t, fasthttp.StatusBadRequest, status)

	all, status := laneDListContacts(t, app, organization.ID, me.ID, nil)
	require.Equal(t, fasthttp.StatusOK, status)
	assert.Equal(t, int64(7), all.Total)
	assert.Equal(t, "Aisyah Rahman", laneDContactByID(t, all, assignedToMe.ID).AssignedUserName)
	assert.Equal(t, "Ben Lim", laneDContactByID(t, all, assignedToTeammate.ID).AssignedUserName)
	transferRow := laneDContactByID(t, all, transferredToMe.ID)
	assert.Empty(t, transferRow.AssignedUserName)
	require.NotNil(t, transferRow.TransferAgentID)
	assert.Equal(t, me.ID, *transferRow.TransferAgentID)
	assert.Equal(t, "Aisyah Rahman", transferRow.TransferAgentName)
	both := laneDContactByID(t, all, teammateButTransferredToMe.ID)
	assert.Equal(t, "Ben Lim", both.AssignedUserName)
	assert.Equal(t, "Aisyah Rahman", both.TransferAgentName)
	queuedRow := laneDContactByID(t, all, queued.ID)
	assert.Nil(t, queuedRow.TransferAgentID, "a team-queue transfer has no agent")
	assert.Nil(t, laneDContactByID(t, all, resumed.ID).TransferAgentID, "an ended transfer is not shown")

	// A user without contacts:read is already limited to their own contacts;
	// the Unassigned filter cannot widen that scope.
	agentRole := testutil.CreateTestRoleWithKeys(t, db, organization.ID, "lane-d-agent-"+uuid.NewString(), []string{
		models.ResourceChat + ":" + models.ActionRead,
	})
	agent := testutil.CreateTestUser(t, db, organization.ID, testutil.WithRoleID(&agentRole.ID))
	agentContact := newContact()
	laneDCreateTransfer(t, db, agentContact, account.Name, &agent.ID, models.TransferStatusActive)
	agentAll, status := laneDListContacts(t, app, organization.ID, agent.ID, nil)
	require.Equal(t, fasthttp.StatusOK, status)
	assert.Equal(t, []uuid.UUID{agentContact.ID}, laneDContactIDs(agentAll))
	agentUnassigned, status := laneDListContacts(t, app, organization.ID, agent.ID, map[string]string{"assigned": "unassigned"})
	require.Equal(t, fasthttp.StatusOK, status)
	assert.Empty(t, agentUnassigned.Contacts)
	assert.Zero(t, agentUnassigned.Total)
}

func TestLaneDManualMarkReadAndUnreadFollowTheVisibleCursorPath(t *testing.T) {
	db := testutil.SetupTestDB(t)
	organization := testutil.CreateTestOrganization(t, db)
	adminRole := testutil.CreateAdminRole(t, db, organization.ID)
	reader := testutil.CreateTestUser(t, db, organization.ID, testutil.WithRoleID(&adminRole.ID))
	account := testutil.CreateTestWhatsAppAccount(t, db, organization.ID)
	contact := testutil.CreateTestContactWith(t, db, organization.ID, testutil.WithContactAccount(account.Name))
	conversation := createLegacyChatConversation(t, db, account, contact.ID)
	baseTime := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	first := createLegacyChatMessage(t, db, organization.ID, account.Name, contact.ID, conversation.ID, uuid.New(), baseTime)
	latest := createLegacyChatMessage(t, db, organization.ID, account.Name, contact.ID, conversation.ID, uuid.New(), baseTime.Add(time.Second))

	hub, client := laneDHub(t, organization.ID)
	app := &App{DB: db, Log: testutil.NopLogger(), WSHub: hub}

	payload, status := laneDMarkRead(t, app, organization.ID, reader.ID, contact.ID, map[string]any{"mark": "read"})
	require.Equal(t, fasthttp.StatusOK, status)
	assert.Equal(t, true, payload["cursor_synced"])
	assert.Equal(t, false, payload["manual_unread"])
	cursor := loadConversationReadCursor(t, db, organization.ID, reader.ID, conversation.ID)
	require.NotNil(t, cursor.LastReadMessageID)
	assert.Equal(t, latest.ID, *cursor.LastReadMessageID, "manual read advances the reader's inbox cursor")
	for _, id := range []uuid.UUID{first.ID, latest.ID} {
		var message models.Message
		require.NoError(t, db.First(&message, "id = ?", id).Error)
		assert.Equal(t, models.MessageStatusRead, message.Status)
	}
	// Nothing was marked unread, so no read_state change reaches other clients.
	laneDExpectNoContactUpdate(t, client, 300*time.Millisecond)

	payload, status = laneDMarkRead(t, app, organization.ID, reader.ID, contact.ID, map[string]any{"mark": "unread"})
	require.Equal(t, fasthttp.StatusOK, status)
	assert.Equal(t, true, payload["manual_unread"])
	assert.True(t, laneDManualUnreadStored(t, db, contact.ID))
	laneDExpectContactUpdate(t, client, contact.ID, "read_state")
	var stillRead models.Message
	require.NoError(t, db.First(&stillRead, "id = ?", latest.ID).Error)
	assert.Equal(t, models.MessageStatusRead, stillRead.Status, "mark unread touches no message")

	unreadList, status := laneDListContacts(t, app, organization.ID, reader.ID, map[string]string{"unread": "true"})
	require.Equal(t, fasthttp.StatusOK, status)
	require.Equal(t, []uuid.UUID{contact.ID}, laneDContactIDs(unreadList))
	assert.True(t, unreadList.Contacts[0].ManualUnread)
	assert.Zero(t, unreadList.Contacts[0].UnreadCount)

	// The automatic read body is unchanged and also ends a manual mark, which
	// other clients learn about.
	markLegacyChatVisible(t, app, organization.ID, reader.ID, contact.ID, latest.ID, fasthttp.StatusOK)
	assert.False(t, laneDManualUnreadStored(t, db, contact.ID))
	laneDExpectContactUpdate(t, client, contact.ID, "read_state")
	// A later automatic read with no mark set stays quiet.
	markLegacyChatVisible(t, app, organization.ID, reader.ID, contact.ID, latest.ID, fasthttp.StatusOK)
	laneDExpectNoContactUpdate(t, client, 300*time.Millisecond)
	var refreshed models.Contact
	require.NoError(t, db.First(&refreshed, "id = ?", contact.ID).Error)
	assert.True(t, refreshed.IsRead)

	// A manual read with nothing left to read still clears the mark.
	_, status = laneDMarkRead(t, app, organization.ID, reader.ID, contact.ID, map[string]any{"mark": "unread"})
	require.Equal(t, fasthttp.StatusOK, status)
	require.True(t, laneDManualUnreadStored(t, db, contact.ID))
	laneDExpectContactUpdate(t, client, contact.ID, "read_state")
	_, status = laneDMarkRead(t, app, organization.ID, reader.ID, contact.ID, map[string]any{"mark": "read"})
	require.Equal(t, fasthttp.StatusOK, status)
	assert.False(t, laneDManualUnreadStored(t, db, contact.ID))
	laneDExpectContactUpdate(t, client, contact.ID, "read_state")
	laneDExpectNoContactUpdate(t, client, 300*time.Millisecond)

	_, status = laneDMarkRead(t, app, organization.ID, reader.ID, contact.ID, map[string]any{"mark": "later"})
	assert.Equal(t, fasthttp.StatusBadRequest, status)
	_, status = laneDMarkRead(t, app, organization.ID, reader.ID, contact.ID, map[string]any{
		"mark":                    "read",
		"last_visible_message_id": latest.ID.String(),
	})
	assert.Equal(t, fasthttp.StatusBadRequest, status)
}

func TestLaneDManualReadStateWithoutIncomingMessagesOrSharedWrite(t *testing.T) {
	db := testutil.SetupTestDB(t)
	organization := testutil.CreateTestOrganization(t, db)
	adminRole := testutil.CreateAdminRole(t, db, organization.ID)
	writer := testutil.CreateTestUser(t, db, organization.ID, testutil.WithRoleID(&adminRole.ID))
	account := testutil.CreateTestWhatsAppAccount(t, db, organization.ID)
	app := &App{DB: db, Log: testutil.NopLogger()}

	quiet := testutil.CreateTestContactWith(t, db, organization.ID, testutil.WithContactAccount(account.Name))
	payload, status := laneDMarkRead(t, app, organization.ID, writer.ID, quiet.ID, map[string]any{"mark": "unread"})
	require.Equal(t, fasthttp.StatusOK, status)
	assert.Equal(t, true, payload["manual_unread"])
	assert.True(t, laneDManualUnreadStored(t, db, quiet.ID))
	payload, status = laneDMarkRead(t, app, organization.ID, writer.ID, quiet.ID, map[string]any{"mark": "read"})
	require.Equal(t, fasthttp.StatusOK, status)
	assert.Equal(t, false, payload["cursor_synced"])
	assert.False(t, laneDManualUnreadStored(t, db, quiet.ID))

	// A reader without chat:write can neither set nor clear shared state, but
	// a manual read still advances their own inbox cursor.
	readOnlyRole := testutil.CreateTestRoleWithKeys(t, db, organization.ID, "lane-d-read-only-"+uuid.NewString(), []string{
		models.ResourceChat + ":" + models.ActionRead,
		models.ResourceContacts + ":" + models.ActionRead,
	})
	readOnly := testutil.CreateTestUser(t, db, organization.ID, testutil.WithRoleID(&readOnlyRole.ID))
	contact := testutil.CreateTestContactWith(t, db, organization.ID, testutil.WithContactAccount(account.Name))
	conversation := createLegacyChatConversation(t, db, account, contact.ID)
	message := createLegacyChatMessage(t, db, organization.ID, account.Name, contact.ID, conversation.ID, uuid.New(), time.Now().UTC().Add(-time.Minute).Truncate(time.Microsecond))
	laneDSetManualUnread(t, db, contact, nil)

	_, status = laneDMarkRead(t, app, organization.ID, readOnly.ID, contact.ID, map[string]any{"mark": "unread"})
	assert.Equal(t, fasthttp.StatusForbidden, status)
	payload, status = laneDMarkRead(t, app, organization.ID, readOnly.ID, contact.ID, map[string]any{"mark": "read"})
	require.Equal(t, fasthttp.StatusOK, status)
	assert.Equal(t, true, payload["manual_unread"])
	assert.True(t, laneDManualUnreadStored(t, db, contact.ID))
	cursor := loadConversationReadCursor(t, db, organization.ID, readOnly.ID, conversation.ID)
	require.NotNil(t, cursor.LastReadMessageID)
	assert.Equal(t, message.ID, *cursor.LastReadMessageID)
	require.NoError(t, db.First(&message, "id = ?", message.ID).Error)
	assert.NotEqual(t, models.MessageStatusRead, message.Status)
}

func TestLaneDChatbotReadKeepsATeammatesManualUnread(t *testing.T) {
	db := testutil.SetupTestDB(t)
	organization := testutil.CreateTestOrganization(t, db)
	adminRole := testutil.CreateAdminRole(t, db, organization.ID)
	reader := testutil.CreateTestUser(t, db, organization.ID, testutil.WithRoleID(&adminRole.ID))
	account := testutil.CreateTestWhatsAppAccount(t, db, organization.ID)
	contact := testutil.CreateTestContactWith(t, db, organization.ID, testutil.WithContactAccount(account.Name))
	conversation := createLegacyChatConversation(t, db, account, contact.ID)
	app := &App{DB: db, Log: testutil.NopLogger()}

	_, status := laneDMarkRead(t, app, organization.ID, reader.ID, contact.ID, map[string]any{"mark": "unread"})
	require.Equal(t, fasthttp.StatusOK, status)

	// A new customer message that the chatbot answers is stored as read
	// through the whole-contact path (ChatbotSendOptions.MarkIncomingRead).
	incoming := createLegacyChatMessage(t, db, organization.ID, account.Name, contact.ID, conversation.ID, uuid.New(), time.Now().UTC().Add(-time.Minute).Truncate(time.Microsecond))
	var stored models.Contact
	require.NoError(t, db.First(&stored, "id = ?", contact.ID).Error)
	app.markMessagesAsRead(organization.ID, contact.ID, &stored)
	require.NoError(t, db.First(&incoming, "id = ?", incoming.ID).Error)
	assert.Equal(t, models.MessageStatusRead, incoming.Status)
	assert.True(t, laneDManualUnreadStored(t, db, contact.ID), "a chatbot reply keeps the teammate's reminder")
	unread, status := laneDListContacts(t, app, organization.ID, reader.ID, map[string]string{"unread": "true"})
	require.Equal(t, fasthttp.StatusOK, status)
	require.Equal(t, []uuid.UUID{contact.ID}, laneDContactIDs(unread))
	assert.Zero(t, unread.Contacts[0].UnreadCount)
	assert.True(t, unread.Contacts[0].ManualUnread)

	// A person reading the chat ends it.
	markLegacyChatVisible(t, app, organization.ID, reader.ID, contact.ID, incoming.ID, fasthttp.StatusOK)
	assert.False(t, laneDManualUnreadStored(t, db, contact.ID))
}

func TestLaneDReadStateToleratesScalarContactMetadata(t *testing.T) {
	db := testutil.SetupTestDB(t)
	organization := testutil.CreateTestOrganization(t, db)
	adminRole := testutil.CreateAdminRole(t, db, organization.ID)
	reader := testutil.CreateTestUser(t, db, organization.ID, testutil.WithRoleID(&adminRole.ID))
	account := testutil.CreateTestWhatsAppAccount(t, db, organization.ID)
	contact := testutil.CreateTestContactWith(t, db, organization.ID, testutil.WithContactAccount(account.Name))
	conversation := createLegacyChatConversation(t, db, account, contact.ID)
	latest := createLegacyChatMessage(t, db, organization.ID, account.Name, contact.ID, conversation.ID, uuid.New(), time.Now().UTC().Add(-time.Minute).Truncate(time.Microsecond))
	app := &App{DB: db, Log: testutil.NopLogger()}
	setJSONNull := func() {
		t.Helper()
		require.NoError(t, db.Exec("UPDATE contacts SET metadata = 'null'::jsonb WHERE id = ?", contact.ID).Error)
	}

	// A JSON null (not SQL NULL) must never make the automatic read fail.
	setJSONNull()
	markLegacyChatVisible(t, app, organization.ID, reader.ID, contact.ID, latest.ID, fasthttp.StatusOK)
	require.NoError(t, db.First(&latest, "id = ?", latest.ID).Error)
	assert.Equal(t, models.MessageStatusRead, latest.Status)

	_, status := laneDMarkRead(t, app, organization.ID, reader.ID, contact.ID, map[string]any{"mark": "unread"})
	require.Equal(t, fasthttp.StatusOK, status)
	assert.True(t, laneDManualUnreadStored(t, db, contact.ID))
	var stored models.Contact
	require.NoError(t, db.First(&stored, "id = ?", contact.ID).Error)
	assert.Equal(t, models.JSONB{contactManualUnreadKey: true}, stored.Metadata)

	setJSONNull()
	_, status = laneDMarkRead(t, app, organization.ID, reader.ID, contact.ID, map[string]any{"mark": "read"})
	require.Equal(t, fasthttp.StatusOK, status)
	all, status := laneDListContacts(t, app, organization.ID, reader.ID, map[string]string{"unread": "true"})
	require.Equal(t, fasthttp.StatusOK, status)
	assert.Empty(t, all.Contacts)
}

func TestLaneDAssignContactChecksOrganizationMembership(t *testing.T) {
	db := testutil.SetupTestDB(t)
	organization := testutil.CreateTestOrganization(t, db)
	otherOrganization := testutil.CreateTestOrganization(t, db)
	adminRole := testutil.CreateAdminRole(t, db, organization.ID)
	admin := testutil.CreateTestUser(t, db, organization.ID, testutil.WithRoleID(&adminRole.ID))
	contact := testutil.CreateTestContact(t, db, organization.ID)
	hub, client := laneDHub(t, organization.ID)
	app := &App{DB: db, Log: testutil.NopLogger(), WSHub: hub}

	assign := func(userID uuid.UUID) int {
		t.Helper()
		request := testutil.NewJSONRequest(t, map[string]any{"user_id": userID.String()})
		testutil.SetAuthContext(request, organization.ID, admin.ID)
		testutil.SetPathParam(request, "id", contact.ID.String())
		require.NoError(t, app.AssignContact(request))
		return testutil.GetResponseStatusCode(request)
	}
	assignedTo := func() *uuid.UUID {
		t.Helper()
		var stored models.Contact
		require.NoError(t, db.First(&stored, "id = ?", contact.ID).Error)
		return stored.AssignedUserID
	}

	// A member whose home is another organization is assignable here.
	crossMember := testutil.CreateTestUser(t, db, otherOrganization.ID)
	require.NoError(t, db.Create(&models.UserOrganization{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		UserID:         crossMember.ID,
		OrganizationID: organization.ID,
	}).Error)
	require.Equal(t, fasthttp.StatusOK, assign(crossMember.ID))
	require.NotNil(t, assignedTo())
	assert.Equal(t, crossMember.ID, *assignedTo())
	laneDExpectContactUpdate(t, client, contact.ID, "assignment")

	// A user of another organization without a membership is not.
	outsider := testutil.CreateTestUser(t, db, otherOrganization.ID)
	assert.Equal(t, fasthttp.StatusBadRequest, assign(outsider.ID))

	// A removed member is rejected even though this is their home organization.
	removed := testutil.CreateTestUser(t, db, organization.ID)
	require.NoError(t, db.Where("user_id = ? AND organization_id = ?", removed.ID, organization.ID).
		Delete(&models.UserOrganization{}).Error)
	assert.Equal(t, fasthttp.StatusBadRequest, assign(removed.ID))

	// A deactivated member is rejected.
	inactive := testutil.CreateTestUser(t, db, organization.ID, testutil.WithInactive())
	assert.Equal(t, fasthttp.StatusBadRequest, assign(inactive.ID))
	assert.Equal(t, crossMember.ID, *assignedTo(), "rejected assignments leave the contact unchanged")
}

// laneDOneConnectionDB opens a separate pool on the test database that can
// hold exactly one connection. A handler running inside a transaction on it
// blocks forever if it asks the pool for a second connection.
func laneDOneConnectionDB(t *testing.T) *gorm.DB {
	t.Helper()
	_ = testutil.SetupTestDB(t)
	parsed, err := url.Parse(os.Getenv("TEST_DATABASE_URL"))
	require.NoError(t, err)
	query := parsed.Query()
	query.Set("statement_cache_capacity", "0")
	query.Set("default_query_exec_mode", "describe_exec")
	parsed.RawQuery = query.Encode()
	db, err := gorm.Open(postgres.Open(parsed.String()), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = sqlDB.Close() })
	return db
}

func TestLaneDInboxStateWritesHoldOnlyTheRequestConnection(t *testing.T) {
	setupDB := testutil.SetupTestDB(t)
	organization := testutil.CreateTestOrganization(t, setupDB)
	adminRole := testutil.CreateAdminRole(t, setupDB, organization.ID)
	reader := testutil.CreateTestUser(t, setupDB, organization.ID, testutil.WithRoleID(&adminRole.ID))
	teammate := testutil.CreateTestUser(t, setupDB, organization.ID)
	account := testutil.CreateTestWhatsAppAccount(t, setupDB, organization.ID)
	contact := testutil.CreateTestContactWith(t, setupDB, organization.ID, testutil.WithContactAccount(account.Name))
	conversation := createLegacyChatConversation(t, setupDB, account, contact.ID)
	latest := createLegacyChatMessage(t, setupDB, organization.ID, account.Name, contact.ID, conversation.ID, uuid.New(), time.Now().UTC().Add(-time.Minute).Truncate(time.Microsecond))

	oneConnection := laneDOneConnectionDB(t)
	hub, client := laneDHub(t, organization.ID)
	root := &App{DB: oneConnection, Log: testutil.NopLogger(), WSHub: hub}

	// Mirror the Tenant wrapper: the handler runs on a request-scoped App
	// whose DB is the transaction that owns the pool's only connection, and
	// after-commit callbacks run once it commits.
	run := func(name string, handler func(*App) int) int {
		t.Helper()
		done := make(chan int, 1)
		go func() {
			status := 0
			var afterCommit []func()
			err := oneConnection.Transaction(func(tx *gorm.DB) error {
				scoped := root.scopedApp(tx, organization.ID)
				status = handler(scoped)
				afterCommit = scoped.takeAfterCommit()
				return nil
			})
			if err != nil {
				status = -1
			}
			root.runAfterCommit(afterCommit)
			done <- status
		}()
		select {
		case status := <-done:
			return status
		case <-time.After(20 * time.Second):
			t.Fatalf("%s blocked: it asked for a second pooled connection while holding the request transaction", name)
			return 0
		}
	}
	markRead := func(body map[string]any) func(*App) int {
		return func(scoped *App) int {
			request := testutil.NewJSONRequest(t, body)
			testutil.SetAuthContext(request, organization.ID, reader.ID)
			testutil.SetPathParam(request, "id", contact.ID.String())
			assert.NoError(t, scoped.MarkContactRead(request))
			return testutil.GetResponseStatusCode(request)
		}
	}

	require.Equal(t, fasthttp.StatusOK, run("manual unread", markRead(map[string]any{"mark": "unread"})))
	laneDExpectContactUpdate(t, client, contact.ID, "read_state")
	require.True(t, laneDManualUnreadStored(t, setupDB, contact.ID))
	require.Equal(t, fasthttp.StatusOK, run("manual read", markRead(map[string]any{"mark": "read"})))
	laneDExpectContactUpdate(t, client, contact.ID, "read_state")
	require.False(t, laneDManualUnreadStored(t, setupDB, contact.ID))
	require.NoError(t, setupDB.First(&latest, "id = ?", latest.ID).Error)
	assert.Equal(t, models.MessageStatusRead, latest.Status)

	laneDSetManualUnread(t, setupDB, contact, nil)
	require.Equal(t, fasthttp.StatusOK, run("automatic read", markRead(map[string]any{"last_visible_message_id": latest.ID.String()})))
	require.False(t, laneDManualUnreadStored(t, setupDB, contact.ID))
	laneDExpectContactUpdate(t, client, contact.ID, "read_state")

	require.Equal(t, fasthttp.StatusOK, run("assign", func(scoped *App) int {
		request := testutil.NewJSONRequest(t, map[string]any{"user_id": teammate.ID.String()})
		testutil.SetAuthContext(request, organization.ID, reader.ID)
		testutil.SetPathParam(request, "id", contact.ID.String())
		assert.NoError(t, scoped.AssignContact(request))
		return testutil.GetResponseStatusCode(request)
	}))
	laneDExpectContactUpdate(t, client, contact.ID, "assignment")

	require.Equal(t, fasthttp.StatusOK, run("list", func(scoped *App) int {
		request := testutil.NewGETRequest(t)
		testutil.SetAuthContext(request, organization.ID, reader.ID)
		testutil.SetQueryParam(request, "unread", "true")
		testutil.SetQueryParam(request, "assigned", "unassigned")
		assert.NoError(t, scoped.ListContacts(request))
		return testutil.GetResponseStatusCode(request)
	}))
}
