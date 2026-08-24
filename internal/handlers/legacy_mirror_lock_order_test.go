package handlers

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	channelapi "github.com/shridarpatil/whatomate/internal/channel"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// The legacy bridge prefix is ChannelAccount -> ContactIdentity ->
// InboxConversation -> Contact -> Message, followed by participant persistence.
// This regression holds each prefix row in turn and proves the strict reply
// path has not acquired the later Conversation lock while it waits.
func TestStrictLegacyReplyUsesBridgePrefixBeforeConversation(t *testing.T) {
	db := testutil.SetupTestDB(t)
	organization := testutil.CreateTestOrganization(t, db)
	account := testutil.CreateTestWhatsAppAccount(t, db, organization.ID)
	contact := testutil.CreateTestContactWith(
		t,
		db,
		organization.ID,
		testutil.WithContactAccount(account.Name),
	)
	message := models.Message{
		BaseModel:         models.BaseModel{ID: uuid.New()},
		OrganizationID:    organization.ID,
		WhatsAppAccount:   account.Name,
		ContactID:         contact.ID,
		Direction:         models.DirectionOutgoing,
		MessageType:       models.MessageTypeText,
		Content:           "lock-order probe",
		Status:            models.MessageStatusPending,
		WhatsAppMessageID: "wamid." + uuid.NewString(),
		ConversationID:    uuid.NewString(),
		InteractiveData:   models.JSONB{},
		Metadata:          models.JSONB{},
	}
	require.NoError(t, db.Create(&message).Error)
	mirror, err := channelapi.MirrorLegacyWhatsAppMessage(
		db,
		channelapi.LegacyMetaAccountRef{
			ID:             account.ID,
			OrganizationID: organization.ID,
			Name:           account.Name,
			Status:         account.Status,
		},
		message.ID,
	)
	require.NoError(t, err)

	policy := &legacyWhatsAppReplyPolicy{
		ConversationID:    mirror.ConversationID,
		ChannelAccountID:  mirror.ChannelAccountID,
		WhatsAppAccountID: account.ID,
	}
	holder := db.Begin()
	require.NoError(t, holder.Error)
	t.Cleanup(func() { _ = holder.Rollback().Error })
	var shadow models.ChannelAccount
	require.NoError(t, holder.Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("id = ? AND organization_id = ?", mirror.ChannelAccountID, organization.ID).
		First(&shadow).Error)

	waiterDone := make(chan error, 1)
	go func() {
		waiter := db.Session(&gorm.Session{NewDB: true}).Begin()
		if waiter.Error != nil {
			waiterDone <- waiter.Error
			return
		}
		defer func() { _ = waiter.Rollback().Error }()
		waiterDone <- lockStrictLegacyReplyOrder(waiter, organization.ID, policy)
	}()
	select {
	case err := <-waiterDone:
		require.Failf(t, "strict lock bypassed held shadow account", "result: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	// If the waiter acquired Conversation before blocking on ChannelAccount,
	// this same-order acquisition would wait or deadlock. It must remain free.
	lockContext, cancelLock := context.WithTimeout(context.Background(), time.Second)
	defer cancelLock()
	var conversation models.InboxConversation
	require.NoError(t, holder.WithContext(lockContext).
		Clauses(clause.Locking{Strength: "UPDATE"}).
		Select("id").
		Where("id = ? AND organization_id = ?", mirror.ConversationID, organization.ID).
		First(&conversation).Error)
	require.NoError(t, holder.Commit().Error)

	select {
	case err := <-waiterDone:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		require.Fail(t, "strict reply lock order did not complete after shadow release")
	}

	var persistedConversation models.InboxConversation
	require.NoError(t, db.Select("id, contact_identity_id").
		Where("id = ? AND organization_id = ?", mirror.ConversationID, organization.ID).
		First(&persistedConversation).Error)
	require.NotNil(t, persistedConversation.ContactIdentityID)
	identityHolder := db.Begin()
	require.NoError(t, identityHolder.Error)
	t.Cleanup(func() { _ = identityHolder.Rollback().Error })
	var identity models.ContactIdentity
	require.NoError(t, identityHolder.Clauses(clause.Locking{Strength: "UPDATE"}).
		Select("id").
		Where("id = ? AND organization_id = ?", *persistedConversation.ContactIdentityID, organization.ID).
		First(&identity).Error)

	identityWaiterDone := make(chan error, 1)
	go func() {
		waiter := db.Session(&gorm.Session{NewDB: true}).Begin()
		if waiter.Error != nil {
			identityWaiterDone <- waiter.Error
			return
		}
		defer func() { _ = waiter.Rollback().Error }()
		identityWaiterDone <- lockStrictLegacyReplyOrder(waiter, organization.ID, policy)
	}()
	select {
	case err := <-identityWaiterDone:
		require.Failf(t, "strict lock bypassed held contact identity", "result: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	// The strict waiter must not own Conversation while it waits for Identity.
	lockContext, cancelLock = context.WithTimeout(context.Background(), time.Second)
	defer cancelLock()
	require.NoError(t, identityHolder.WithContext(lockContext).
		Clauses(clause.Locking{Strength: "UPDATE"}).
		Select("id").
		Where("id = ? AND organization_id = ?", mirror.ConversationID, organization.ID).
		First(&conversation).Error)
	require.NoError(t, identityHolder.Commit().Error)
	select {
	case err := <-identityWaiterDone:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		require.Fail(t, "strict reply identity lock order did not complete")
	}
}
