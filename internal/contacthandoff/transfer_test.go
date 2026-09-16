package contacthandoff

import (
	"errors"
	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
	"testing"
	"time"
)

func TestPureHandoffSuppressionFailsClosed(t *testing.T) {
	require.True(t, MessageSuppressed(nil))
	require.False(t, MessageSuppressed(&models.Message{}))
	require.False(t, MessageSuppressed(&models.Message{Metadata: models.JSONB{SuppressionKey: false}}))
	for _, value := range []any{true, "false", nil, 1} {
		require.True(t, MessageSuppressed(&models.Message{Metadata: models.JSONB{SuppressionKey: value}}))
	}
	_, err := HandoffTx(nil, Request{})
	require.Error(t, err)
	suppressed, err := SuppressedTx(nil, uuid.New(), uuid.New())
	require.Error(t, err)
	require.True(t, suppressed)
}

func TestHandoffCallerTransactionCutoffAndResume(t *testing.T) {
	db := testutil.SetupTestDB(t)
	org := testutil.CreateTestOrganization(t, db)
	contact := testutil.CreateTestContact(t, db, org.ID)
	now := time.Now().UTC().Add(-time.Minute)
	message := models.Message{BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: org.ID, ContactID: contact.ID, WhatsAppAccount: "synthetic", Direction: models.DirectionIncoming, MessageType: models.MessageTypeText, Status: models.MessageStatusReceived, Content: "Please contact staff", IngestedAt: &now}
	require.NoError(t, db.Create(&message).Error)
	job := models.ScheduledJob{BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: org.ID, Kind: "inbound_message.continuation", AggregateType: "message", AggregateID: &message.ID, RunAt: now, Status: models.ScheduledJobStatusPending, IdempotencyKey: uuid.NewString(), Version: 1}
	require.NoError(t, db.Create(&job).Error)
	// A pre-cutoff inbound whose job is admitted later must also stay blocked.
	late := message
	late.ID = uuid.New()
	require.NoError(t, db.Create(&late).Error)
	session := models.ChatbotSession{BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: org.ID, ContactID: contact.ID, WhatsAppAccount: "synthetic", PhoneNumber: contact.PhoneNumber, Status: models.SessionStatusActive, SessionData: models.JSONB{"__ai_booking_proposal": models.JSONB{"consumed": "keep"}}}
	require.NoError(t, db.Create(&session).Error)
	account := models.ChannelAccount{BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: org.ID, Channel: models.ChannelMessenger, Provider: "meta", Name: "Synthetic managed", ExternalAccountID: uuid.NewString()}
	require.NoError(t, db.Create(&account).Error)
	conversation := models.InboxConversation{BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: org.ID, ChannelAccountID: account.ID, ContactID: contact.ID, Channel: account.Channel, ExternalConversationID: uuid.NewString(), OpenedAt: now, Metadata: models.JSONB{"ai_booking_session_v1": models.JSONB{"consumed": "keep"}}}
	require.NoError(t, db.Create(&conversation).Error)
	input := Request{OrganizationID: org.ID, ContactID: contact.ID, AccountName: "synthetic", Reason: "Customer asked for staff"}
	aborted := errors.New("atomic handoff failure")
	require.ErrorIs(t, db.Transaction(func(tx *gorm.DB) error {
		_, err := HandoffTx(tx, input)
		if err != nil {
			return err
		}
		return aborted
	}), aborted)
	var count int64
	require.NoError(t, db.Model(&models.AgentTransfer{}).Where("organization_id = ?", org.ID).Count(&count).Error)
	require.Zero(t, count)
	require.NoError(t, db.First(&message, "id = ?", message.ID).Error)
	require.False(t, MessageSuppressed(&message))
	var result Result
	require.NoError(t, db.Transaction(func(tx *gorm.DB) error { var err error; result, err = HandoffTx(tx, input); return err }))
	require.True(t, result.Created)
	require.Nil(t, result.Transfer.AgentID)
	require.NotEmpty(t, result.Contact.Metadata[CutoffKey])
	require.NoError(t, db.First(&message, "id = ?", message.ID).Error)
	require.True(t, MessageSuppressed(&message))
	require.NoError(t, db.First(&session, "id = ?", session.ID).Error)
	require.Equal(t, models.SessionStatusCancelled, session.Status)
	require.Equal(t, true, session.SessionData[ProposalInvalidatedKey])
	require.Contains(t, session.SessionData, "__ai_booking_proposal")
	require.NoError(t, db.First(&conversation, "id = ?", conversation.ID).Error)
	require.Equal(t, true, conversation.Metadata[ProposalInvalidatedKey])
	require.Contains(t, conversation.Metadata, "ai_booking_session_v1")
	firstCutoff := result.Contact.Metadata[CutoffKey].(string)
	require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
		replay, err := HandoffTx(tx, input)
		if err == nil {
			require.False(t, replay.Created)
			require.Equal(t, result.Transfer.ID, replay.Transfer.ID)
		}
		return err
	}))
	require.NoError(t, db.Model(&models.AgentTransfer{}).Where("id = ?", result.Transfer.ID).Update("status", models.TransferStatusResumed).Error)
	for _, id := range []uuid.UUID{message.ID, late.ID} {
		suppressed, err := SuppressedTx(db, org.ID, id)
		require.NoError(t, err)
		require.True(t, suppressed, "Resume cannot revive a previously admitted or late-queued old turn")
	}
	require.NoError(t, db.First(contact, "id = ?", contact.ID).Error)
	laterCutoff := contact.Metadata[CutoffKey].(string)
	firstAt, err := time.Parse(time.RFC3339Nano, firstCutoff)
	require.NoError(t, err)
	lastAt, err := time.Parse(time.RFC3339Nano, laterCutoff)
	require.NoError(t, err)
	require.True(t, lastAt.After(firstAt))
	// A genuinely new inbound is eligible for ordinary authority checks, not
	// permanently suppressed; its provider display time is irrelevant.
	fresh := message
	fresh.ID = uuid.New()
	fresh.Metadata = models.JSONB{}
	after := lastAt.Add(time.Second)
	fresh.IngestedAt = &after
	require.NoError(t, db.Create(&fresh).Error)
	suppressed, err := SuppressedTx(db, org.ID, fresh.ID)
	require.NoError(t, err)
	require.False(t, suppressed)
	other := testutil.CreateTestOrganization(t, db)
	suppressed, err = SuppressedTx(db, other.ID, message.ID)
	require.Error(t, err)
	require.True(t, suppressed)
	bad := input
	bad.OrganizationID = other.ID
	require.Error(t, db.Transaction(func(tx *gorm.DB) error { _, err := HandoffTx(tx, bad); return err }))
	require.NoError(t, db.Model(&models.Contact{}).Where("id = ?", contact.ID).Update("metadata", models.JSONB{CutoffKey: 42}).Error)
	suppressed, err = SuppressedTx(db, org.ID, fresh.ID)
	require.Error(t, err)
	require.True(t, suppressed)
}
