package handlers

import (
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func inboundImageMessage(t *testing.T, wamid, from string) IncomingTextMessage {
	t.Helper()

	var msg IncomingTextMessage
	require.NoError(t, json.Unmarshal([]byte(`{
		"from": "`+from+`",
		"id": "`+wamid+`",
		"timestamp": "1722222222",
		"type": "image",
		"image": {
			"id": "media-provider-id",
			"mime_type": "image/jpeg",
			"sha256": "payload-sha",
			"caption": "A progress photo"
		}
	}`), &msg))
	return msg
}

func inboundDurabilityEventAndOutbox(
	t *testing.T,
	db *gorm.DB,
	organizationID uuid.UUID,
	contactID uuid.UUID,
	eventType models.CustomerActivityEventType,
	sourceType string,
	sourceID uuid.UUID,
) (models.CustomerActivityEvent, models.OutboxEvent) {
	t.Helper()

	var event models.CustomerActivityEvent
	require.NoError(t, db.Where(
		"organization_id = ? AND contact_id = ? AND event_type = ? AND source_object_type = ? AND source_object_id = ?",
		organizationID,
		contactID,
		eventType,
		sourceType,
		sourceID,
	).First(&event).Error)

	var outbox models.OutboxEvent
	require.NoError(t, db.Where(
		"organization_id = ? AND idempotency_key = ?",
		organizationID,
		"customer-activity-webhook:"+event.ID.String(),
	).First(&outbox).Error)
	require.NotNil(t, outbox.AggregateID)
	assert.Equal(t, sourceID, *outbox.AggregateID)
	assert.Equal(t, sourceType, outbox.AggregateType)
	assert.Equal(t, string(eventType), outbox.EventType)

	return event, outbox
}

func TestInboundDurability_ContactCreationActivityAndOutboxAreAtomic(t *testing.T) {
	app := newProcessorTestApp(t)
	organization, account := createProcessorTestOrg(t, app)
	rootDB := app.DB

	outerTransaction := rootDB.Begin()
	require.NoError(t, outerTransaction.Error)
	app.DB = outerTransaction
	t.Cleanup(func() {
		_ = outerTransaction.Rollback().Error
		app.DB = rootDB
	})

	phoneNumber := "+6012" + uuid.New().String()[:8]
	profileName := "Inbound durability patient"
	bsuid := "bsuid-" + uuid.New().String()
	contact, created, err := app.getOrCreateInboundContact(
		account,
		phoneNumber,
		profileName,
		bsuid,
	)
	require.NoError(t, err)
	require.True(t, created)
	require.NotNil(t, contact)
	assert.Equal(t, phoneNumber[1:], contact.PhoneNumber)
	assert.Equal(t, profileName, contact.ProfileName)
	assert.Equal(t, bsuid, contact.BSUID)

	event, outbox := inboundDurabilityEventAndOutbox(
		t,
		outerTransaction,
		organization.ID,
		contact.ID,
		models.CustomerActivityContactCreated,
		"contact",
		contact.ID,
	)
	assert.Equal(t, "contact-created:"+contact.ID.String(), event.IdempotencyKey)
	assert.Equal(t, string(models.CustomerActivityActorContact), string(event.ActorType))
	assert.Equal(t, event.ID.String(), outbox.Payload["activity_event_id"])
	assert.Equal(t, contact.ID.String(), outbox.Payload["contact_id"])
	assert.Equal(t, contact.PhoneNumber, outbox.Payload["contact_phone"])
	assert.Equal(t, profileName, outbox.Payload["contact_name"])
	assert.Equal(t, account.Name, outbox.Payload["whatsapp_account"])

	sameContact, createdAgain, err := app.getOrCreateInboundContact(
		account,
		phoneNumber,
		profileName,
		bsuid,
	)
	require.NoError(t, err)
	require.False(t, createdAgain)
	require.NotNil(t, sameContact)
	assert.Equal(t, contact.ID, sameContact.ID)

	var contactCount, activityCount, outboxCount int64
	require.NoError(t, outerTransaction.Model(&models.Contact{}).
		Where("organization_id = ? AND id = ?", organization.ID, contact.ID).
		Count(&contactCount).Error)
	require.NoError(t, outerTransaction.Model(&models.CustomerActivityEvent{}).
		Where("organization_id = ? AND idempotency_key = ?", organization.ID, event.IdempotencyKey).
		Count(&activityCount).Error)
	require.NoError(t, outerTransaction.Model(&models.OutboxEvent{}).
		Where("organization_id = ? AND idempotency_key = ?", organization.ID, outbox.IdempotencyKey).
		Count(&outboxCount).Error)
	assert.Equal(t, int64(1), contactCount)
	assert.Equal(t, int64(1), activityCount)
	assert.Equal(t, int64(1), outboxCount)

	require.NoError(t, outerTransaction.Rollback().Error)
	app.DB = rootDB

	require.NoError(t, rootDB.Model(&models.Contact{}).
		Unscoped().
		Where("organization_id = ? AND id = ?", organization.ID, contact.ID).
		Count(&contactCount).Error)
	require.NoError(t, rootDB.Model(&models.CustomerActivityEvent{}).
		Where("organization_id = ? AND idempotency_key = ?", organization.ID, event.IdempotencyKey).
		Count(&activityCount).Error)
	require.NoError(t, rootDB.Model(&models.OutboxEvent{}).
		Where("organization_id = ? AND idempotency_key = ?", organization.ID, outbox.IdempotencyKey).
		Count(&outboxCount).Error)
	assert.Zero(t, contactCount, "contact must roll back with its activity and outbox")
	assert.Zero(t, activityCount, "activity must roll back with its contact and outbox")
	assert.Zero(t, outboxCount, "outbox must roll back with its contact and activity")
}

func TestInboundDurability_IncomingMessageCreatesActivityAndLegacyOutbox(t *testing.T) {
	app := newProcessorTestApp(t)
	organization, account := createProcessorTestOrg(t, app)
	contact := testutil.CreateTestContact(t, app.DB, organization.ID)
	whatsAppMessageID := "wamid.inbound-durable-" + uuid.New().String()
	content := "Please book my follow-up"

	require.True(t, app.saveIncomingMessage(
		account,
		contact,
		whatsAppMessageID,
		"text",
		content,
		nil,
		"",
	))

	var message models.Message
	require.NoError(t, app.DB.Where(
		"organization_id = ? AND whats_app_message_id = ?",
		organization.ID,
		whatsAppMessageID,
	).First(&message).Error)
	assert.Equal(t, contact.ID, message.ContactID)
	assert.Equal(t, content, message.Content)

	event, outbox := inboundDurabilityEventAndOutbox(
		t,
		app.DB,
		organization.ID,
		contact.ID,
		models.CustomerActivityMessageIncoming,
		"message",
		message.ID,
	)
	assert.Equal(t, "Message received", event.Title)
	assert.Equal(t, content, event.Summary)
	assert.Equal(t, event.ID.String(), outbox.Payload["activity_event_id"])
	assert.Equal(t, message.ID.String(), outbox.Payload["message_id"])
	assert.Equal(t, contact.ID.String(), outbox.Payload["contact_id"])
	assert.Equal(t, contact.PhoneNumber, outbox.Payload["contact_phone"])
	assert.Equal(t, contact.ProfileName, outbox.Payload["contact_name"])
	assert.Equal(t, string(models.MessageTypeText), outbox.Payload["message_type"])
	assert.Equal(t, content, outbox.Payload["content"])
	assert.Equal(t, account.Name, outbox.Payload["whatsapp_account"])
	assert.Equal(t, string(models.DirectionIncoming), outbox.Payload["direction"])
}

func TestInboundDurability_DuplicateWAMIDCreatesExactlyOneDurableMessage(t *testing.T) {
	app := newProcessorTestApp(t)
	organization, account := createProcessorTestOrg(t, app)
	contact := testutil.CreateTestContact(t, app.DB, organization.ID)
	whatsAppMessageID := "wamid.duplicate-" + uuid.New().String()
	firstContent := "First delivery"

	require.True(t, app.saveIncomingMessage(
		account,
		contact,
		whatsAppMessageID,
		"text",
		firstContent,
		nil,
		"",
	))
	require.False(t, app.saveIncomingMessage(
		account,
		contact,
		whatsAppMessageID,
		"text",
		"Duplicate delivery must not persist",
		nil,
		"",
	))

	idempotencyKey := "message-incoming:" +
		uuid.NewSHA1(account.ID, []byte(whatsAppMessageID)).String()
	var messageCount, activityCount, outboxCount int64
	require.NoError(t, app.DB.Model(&models.Message{}).
		Where(
			"organization_id = ? AND whats_app_message_id = ?",
			organization.ID,
			whatsAppMessageID,
		).
		Count(&messageCount).Error)
	require.NoError(t, app.DB.Model(&models.CustomerActivityEvent{}).
		Where(
			"organization_id = ? AND idempotency_key = ?",
			organization.ID,
			idempotencyKey,
		).
		Count(&activityCount).Error)
	require.NoError(t, app.DB.Model(&models.OutboxEvent{}).
		Where(
			"organization_id = ? AND event_type = ?",
			organization.ID,
			models.CustomerActivityMessageIncoming,
		).
		Count(&outboxCount).Error)
	assert.Equal(t, int64(1), messageCount)
	assert.Equal(t, int64(1), activityCount)
	assert.Equal(t, int64(1), outboxCount)

	var message models.Message
	require.NoError(t, app.DB.Where(
		"organization_id = ? AND whats_app_message_id = ?",
		organization.ID,
		whatsAppMessageID,
	).First(&message).Error)
	assert.Equal(t, firstContent, message.Content)

	var event models.CustomerActivityEvent
	require.NoError(t, app.DB.Where(
		"organization_id = ? AND idempotency_key = ?",
		organization.ID,
		idempotencyKey,
	).First(&event).Error)
	assert.Equal(t, firstContent, event.Summary)

	var refreshedContact models.Contact
	require.NoError(t, app.DB.First(&refreshedContact, contact.ID).Error)
	assert.Equal(t, firstContent, refreshedContact.LastMessagePreview)
}

func TestInboundDurability_ReplyLookupIsTenantScoped(t *testing.T) {
	app := newProcessorTestApp(t)
	organizationA, accountA := createProcessorTestOrg(t, app)
	organizationB, accountB := createProcessorTestOrg(t, app)
	contactA := testutil.CreateTestContact(t, app.DB, organizationA.ID)
	contactB := testutil.CreateTestContact(t, app.DB, organizationB.ID)

	foreignWAMID := "wamid.foreign-reply-target-" + uuid.New().String()
	foreignMessage := models.Message{
		BaseModel:         models.BaseModel{ID: uuid.New()},
		OrganizationID:    organizationB.ID,
		WhatsAppAccount:   accountB.Name,
		ContactID:         contactB.ID,
		WhatsAppMessageID: foreignWAMID,
		Direction:         models.DirectionOutgoing,
		MessageType:       models.MessageTypeText,
		Content:           "Other tenant message",
		Status:            models.MessageStatusSent,
	}
	require.NoError(t, app.DB.Create(&foreignMessage).Error)

	crossTenantReplyWAMID := "wamid.cross-tenant-reply-" + uuid.New().String()
	require.True(t, app.saveIncomingMessage(
		accountA,
		contactA,
		crossTenantReplyWAMID,
		"text",
		"Must not link across tenants",
		nil,
		foreignWAMID,
	))

	var crossTenantReply models.Message
	require.NoError(t, app.DB.Where(
		"organization_id = ? AND whats_app_message_id = ?",
		organizationA.ID,
		crossTenantReplyWAMID,
	).First(&crossTenantReply).Error)
	assert.False(t, crossTenantReply.IsReply)
	assert.Nil(t, crossTenantReply.ReplyToMessageID)

	localWAMID := "wamid.local-reply-target-" + uuid.New().String()
	localMessage := models.Message{
		BaseModel:         models.BaseModel{ID: uuid.New()},
		OrganizationID:    organizationA.ID,
		WhatsAppAccount:   accountA.Name,
		ContactID:         contactA.ID,
		WhatsAppMessageID: localWAMID,
		Direction:         models.DirectionOutgoing,
		MessageType:       models.MessageTypeText,
		Content:           "Same tenant message",
		Status:            models.MessageStatusSent,
	}
	require.NoError(t, app.DB.Create(&localMessage).Error)

	localReplyWAMID := "wamid.local-reply-" + uuid.New().String()
	require.True(t, app.saveIncomingMessage(
		accountA,
		contactA,
		localReplyWAMID,
		"text",
		"Link within the tenant",
		nil,
		localWAMID,
	))

	var localReply models.Message
	require.NoError(t, app.DB.Where(
		"organization_id = ? AND whats_app_message_id = ?",
		organizationA.ID,
		localReplyWAMID,
	).First(&localReply).Error)
	require.True(t, localReply.IsReply)
	require.NotNil(t, localReply.ReplyToMessageID)
	assert.Equal(t, localMessage.ID, *localReply.ReplyToMessageID)
}

func TestInboundDurability_BeforeAckBoundaryPersistsWithoutMediaNetwork(t *testing.T) {
	app := newProcessorTestApp(t)
	organization, account := createProcessorTestOrg(t, app)

	// The synchronous ACK path must not need either provider client. A media
	// download here would panic or fail instead of reaching the assertions.
	app.WhatsApp = nil
	app.ObjectStore = nil

	wamid := "wamid.before-ack-" + uuid.New().String()
	phone := "6012" + uuid.New().String()[:8]
	msg := inboundImageMessage(t, wamid, phone)

	work, duplicate, err := app.persistIncomingMessageBeforeAck(
		account.PhoneID,
		msg,
		"Durable media patient",
	)
	require.NoError(t, err)
	require.False(t, duplicate)
	require.NotNil(t, work)
	assert.Equal(t, organization.ID, work.OrganizationID)
	assert.Equal(t, wamid, work.Persisted.WhatsAppMessageID)
	assert.Equal(t, models.MessageTypeImage, work.Persisted.MessageType)
	assert.Equal(t, "A progress photo", work.Persisted.Content)
	assert.Equal(t, "image/jpeg", work.Persisted.MediaMimeType)
	assert.Empty(t, work.Persisted.MediaURL, "media hydration belongs to the asynchronous continuation")

	var persisted models.Message
	require.NoError(t, app.DB.Where(
		"organization_id = ? AND id = ?",
		organization.ID,
		work.Persisted.ID,
	).First(&persisted).Error)
	assert.Equal(t, work.Contact.ID, persisted.ContactID)
	assert.Empty(t, persisted.MediaURL)

	_, _ = inboundDurabilityEventAndOutbox(
		t,
		app.DB,
		organization.ID,
		work.Contact.ID,
		models.CustomerActivityContactCreated,
		"contact",
		work.Contact.ID,
	)
	_, _ = inboundDurabilityEventAndOutbox(
		t,
		app.DB,
		organization.ID,
		work.Contact.ID,
		models.CustomerActivityMessageIncoming,
		"message",
		persisted.ID,
	)
}

func TestInboundDurability_BeforeAckDuplicateIsSuccessfulNoOp(t *testing.T) {
	app := newProcessorTestApp(t)
	organization, account := createProcessorTestOrg(t, app)
	wamid := "wamid.before-ack-duplicate-" + uuid.New().String()
	msg := inboundImageMessage(t, wamid, "6013"+uuid.New().String()[:8])

	first, duplicate, err := app.persistIncomingMessageBeforeAck(
		account.PhoneID,
		msg,
		"Replay-safe patient",
	)
	require.NoError(t, err)
	require.False(t, duplicate)
	require.NotNil(t, first)

	second, duplicate, err := app.persistIncomingMessageBeforeAck(
		account.PhoneID,
		msg,
		"Replay-safe patient",
	)
	require.NoError(t, err, "a duplicate delivery must still be safe to acknowledge")
	require.True(t, duplicate)
	require.NotNil(t, second, "duplicates must wake the durable continuation")
	assert.Equal(t, first.Persisted.ID, second.Persisted.ID)

	idempotencyKey := "message-incoming:" +
		uuid.NewSHA1(account.ID, []byte(wamid)).String()
	var messageCount, activityCount, outboxCount int64
	require.NoError(t, app.DB.Model(&models.Message{}).
		Where(
			"organization_id = ? AND whats_app_account = ? AND whats_app_message_id = ?",
			organization.ID,
			account.Name,
			wamid,
		).
		Count(&messageCount).Error)
	require.NoError(t, app.DB.Model(&models.CustomerActivityEvent{}).
		Where(
			"organization_id = ? AND idempotency_key = ?",
			organization.ID,
			idempotencyKey,
		).
		Count(&activityCount).Error)
	require.NoError(t, app.DB.Model(&models.OutboxEvent{}).
		Where(
			"organization_id = ? AND event_type = ? AND aggregate_id = ?",
			organization.ID,
			models.CustomerActivityMessageIncoming,
			first.Persisted.ID,
		).
		Count(&outboxCount).Error)
	assert.Equal(t, int64(1), messageCount)
	assert.Equal(t, int64(1), activityCount)
	assert.Equal(t, int64(1), outboxCount)

	var continuationCount int64
	require.NoError(t, app.DB.Model(&models.ScheduledJob{}).
		Where(
			"organization_id = ? AND kind = ? AND aggregate_id = ?",
			organization.ID,
			inboundContinuationJobKind,
			first.Persisted.ID,
		).
		Count(&continuationCount).Error)
	assert.Equal(t, int64(1), continuationCount)
}

func TestInboundDurability_WAMIDReuseAcrossContactsRollsBack(t *testing.T) {
	app := newProcessorTestApp(t)
	organization, account := createProcessorTestOrg(t, app)
	wamid := "wamid.cross-contact-reuse-" + uuid.New().String()
	firstPhone := "6014" + uuid.New().String()[:8]
	secondPhone := "6015" + uuid.New().String()[:8]

	first, duplicate, err := app.persistIncomingMessageBeforeAck(
		account.PhoneID,
		inboundImageMessage(t, wamid, firstPhone),
		"Original sender",
	)
	require.NoError(t, err)
	require.False(t, duplicate)
	require.NotNil(t, first)

	second, duplicate, err := app.persistIncomingMessageBeforeAck(
		account.PhoneID,
		inboundImageMessage(t, wamid, secondPhone),
		"Different sender",
	)
	require.Error(t, err, "a WAMID cannot be acknowledged as the same fact for another contact")
	require.False(t, duplicate)
	assert.Nil(t, second)

	var secondContactCount int64
	require.NoError(t, app.DB.Unscoped().Model(&models.Contact{}).
		Where(
			"organization_id = ? AND phone_number IN ?",
			organization.ID,
			[]string{secondPhone, "+" + secondPhone},
		).
		Count(&secondContactCount).Error)
	assert.Zero(t, secondContactCount, "the outer ACK transaction must roll back the phantom contact")
}
