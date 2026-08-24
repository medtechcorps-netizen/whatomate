package handlers

import (
	"sort"
	"time"

	"github.com/google/uuid"
	channelapi "github.com/shridarpatil/whatomate/internal/channel"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/internal/queue"
	"gorm.io/gorm"
)

// mirrorLegacyWhatsAppMessage is deliberately best-effort on live delivery
// paths. The established WhatsApp send/webhook flow remains authoritative and
// the idempotent migration backfill repairs a transient mirror failure.
func (a *App) mirrorLegacyWhatsAppMessage(
	account *models.WhatsAppAccount,
	messageID uuid.UUID,
) {
	if account == nil || messageID == uuid.Nil {
		return
	}
	err := persistLegacyWhatsAppMessageMirror(a.DB, account, messageID)
	a.logLegacyWhatsAppMessageMirrorError(err, account, messageID)
}

// mirrorLegacyWhatsAppMessageAfterCommit gives every live legacy bridge call
// the same lock boundary. Message/contact persistence commits first; the
// idempotent mirror then owns a fresh short tenant transaction whose global
// lock order is ChannelAccount -> ContactIdentity -> InboxConversation ->
// Contact -> Message, followed by the ConversationParticipant write.
// This is especially important for webhook and async-send paths which may
// already hold Contact or Message locks in their surrounding transaction.
func (a *App) mirrorLegacyWhatsAppMessageAfterCommit(
	account *models.WhatsAppAccount,
	messageID uuid.UUID,
) {
	if a == nil || account == nil || account.OrganizationID == uuid.Nil || messageID == uuid.Nil {
		return
	}
	accountCopy := *account
	organizationID := account.OrganizationID
	a.afterTenantCommit(func() {
		root := a.rootApp()
		if root == nil {
			return
		}
		if err := root.WithCommittedTenantApp(organizationID, func(scoped *App) error {
			scoped.mirrorLegacyWhatsAppMessage(&accountCopy, messageID)
			return nil
		}); err != nil {
			root.Log.Error(
				"Failed to run deferred legacy WhatsApp mirror phase",
				"error", err,
				"organization_id", organizationID,
				"message_id", messageID,
			)
		}
	})
}

func persistLegacyWhatsAppMessageMirror(
	db *gorm.DB,
	account *models.WhatsAppAccount,
	messageID uuid.UUID,
) error {
	if db == nil || account == nil || messageID == uuid.Nil {
		return nil
	}
	_, err := channelapi.MirrorLegacyWhatsAppMessage(
		db,
		channelapi.LegacyMetaAccountRef{
			ID:             account.ID,
			OrganizationID: account.OrganizationID,
			Name:           account.Name,
			Status:         account.Status,
		},
		messageID,
	)
	return err
}

func (a *App) logLegacyWhatsAppMessageMirrorError(
	err error,
	account *models.WhatsAppAccount,
	messageID uuid.UUID,
) {
	if err != nil && a != nil && account != nil {
		a.Log.Error(
			"Failed to mirror legacy WhatsApp message into omnichannel inbox",
			"error",
			err,
			"organization_id",
			account.OrganizationID,
			"message_id",
			messageID,
		)
	}
}

func (a *App) mirrorLegacyWhatsAppRead(
	organizationID uuid.UUID,
	contactID uuid.UUID,
) {
	if err := channelapi.MarkLegacyWhatsAppConversationRead(
		a.DB,
		organizationID,
		contactID,
	); err != nil {
		a.Log.Error(
			"Failed to mirror legacy WhatsApp read state into omnichannel inbox",
			"error",
			err,
			"organization_id",
			organizationID,
			"contact_id",
			contactID,
		)
	}
}

// syncLegacyVisibleMessagesRead advances the authenticated user's normalized
// inbox cursor only through legacy WhatsApp messages that the Chat caller
// explicitly identified as visible after rendering.
// A provider-qualified conversation join prevents a message linked to another
// inbox provider from changing that provider's cursor.
type legacyVisibleReadSyncResult struct {
	VerifiedMessageConversations map[uuid.UUID]uuid.UUID
	VerifiedMessages             map[uuid.UUID]models.Message
	AdvancedConversations        int
}

func (a *App) syncLegacyVisibleMessagesRead(
	organizationID, userID, contactID uuid.UUID,
	visibleMessages []models.Message,
	allowSharedRepair bool,
) (legacyVisibleReadSyncResult, error) {
	result := legacyVisibleReadSyncResult{}
	if a == nil || a.DB == nil || organizationID == uuid.Nil ||
		userID == uuid.Nil || contactID == uuid.Nil || len(visibleMessages) == 0 {
		return result, nil
	}

	visibleMessageIDs := make([]uuid.UUID, 0, len(visibleMessages))
	repairCandidates := make([]models.Message, 0, len(visibleMessages))
	for i := range visibleMessages {
		message := visibleMessages[i]
		if message.ID == uuid.Nil || message.OrganizationID != organizationID ||
			message.ContactID != contactID || message.DeletedAt.Valid {
			continue
		}
		if allowSharedRepair && message.InboxConversationID == nil && message.WhatsAppAccount != "" {
			repairCandidates = append(repairCandidates, message)
		}
		visibleMessageIDs = append(visibleMessageIDs, message.ID)
	}
	// A future batch caller must not acquire multiple account/conversation lock
	// prefixes in request order. Keep repair order deterministic across replicas.
	sort.Slice(repairCandidates, func(i, j int) bool {
		if repairCandidates[i].WhatsAppAccount != repairCandidates[j].WhatsAppAccount {
			return repairCandidates[i].WhatsAppAccount < repairCandidates[j].WhatsAppAccount
		}
		return repairCandidates[i].ID.String() < repairCandidates[j].ID.String()
	})
	for i := range repairCandidates {
		message := repairCandidates[i]
		if message.WhatsAppAccount != "" {
			account, err := a.resolveWhatsAppAccount(organizationID, message.WhatsAppAccount)
			if err == nil {
				if err := persistLegacyWhatsAppMessageMirror(a.DB, account, message.ID); err != nil {
					return result, err
				}
			}
		}
	}
	if len(visibleMessageIDs) == 0 {
		return result, nil
	}

	// Re-read the candidate envelopes and prove the complete immutable legacy
	// binding. Comparing the shadow metadata/external ID to the established
	// WhatsApp account prevents a corrupt same-contact link from advancing a
	// different account's conversation.
	var verifiedMessages []models.Message
	if err := a.DB.Model(&models.Message{}).
		Select("messages.*").
		Joins(`JOIN inbox_conversations AS inbox_conversation
			ON inbox_conversation.id = messages.inbox_conversation_id
		   AND inbox_conversation.organization_id = messages.organization_id`).
		Joins(`JOIN channel_accounts AS channel_account
			ON channel_account.id = inbox_conversation.channel_account_id
		   AND channel_account.organization_id = inbox_conversation.organization_id`).
		Joins(`JOIN whatsapp_accounts AS legacy_account
			ON legacy_account.organization_id = channel_account.organization_id
		   AND CAST(legacy_account.id AS text) = channel_account.metadata ->> 'legacy_account_id'
		   AND messages.whats_app_account = legacy_account.name`).
		Where(
			`messages.id IN ?
			 AND messages.organization_id = ?
			 AND messages.contact_id = ?
			 AND messages.deleted_at IS NULL
			 AND inbox_conversation.organization_id = ?
			 AND inbox_conversation.contact_id = ?
			 AND inbox_conversation.channel = ?
			 AND inbox_conversation.external_conversation_id = 'legacy-contact:' || CAST(messages.contact_id AS text)
			 AND inbox_conversation.deleted_at IS NULL
			 AND channel_account.channel = ?
			 AND channel_account.provider = ?
			 AND channel_account.deleted_at IS NULL
			 AND channel_account.external_account_id = 'legacy-account:' || CAST(legacy_account.id AS text)
			 AND legacy_account.deleted_at IS NULL`,
			visibleMessageIDs,
			organizationID,
			contactID,
			organizationID,
			contactID,
			models.ChannelWhatsApp,
			models.ChannelWhatsApp,
			channelapi.LegacyMetaProvider,
		).
		Find(&verifiedMessages).Error; err != nil {
		return result, err
	}
	if len(verifiedMessages) == 0 {
		return result, nil
	}

	candidates := make(map[uuid.UUID]models.Message)
	result.VerifiedMessageConversations = make(map[uuid.UUID]uuid.UUID, len(verifiedMessages))
	result.VerifiedMessages = make(map[uuid.UUID]models.Message, len(verifiedMessages))
	for i := range verifiedMessages {
		message := verifiedMessages[i]
		if message.InboxConversationID == nil {
			continue
		}
		conversationID := *message.InboxConversationID
		result.VerifiedMessageConversations[message.ID] = conversationID
		result.VerifiedMessages[message.ID] = message
		current, exists := candidates[conversationID]
		messageIngestedAt := message.EffectiveIngestedAt()
		currentIngestedAt := current.EffectiveIngestedAt()
		if !exists || messageIngestedAt.After(currentIngestedAt) ||
			(messageIngestedAt.Equal(currentIngestedAt) && message.ID.String() > current.ID.String()) {
			candidates[conversationID] = message
		}
	}

	conversationIDs := make([]uuid.UUID, 0, len(candidates))
	for conversationID := range candidates {
		conversationIDs = append(conversationIDs, conversationID)
	}
	sort.Slice(conversationIDs, func(i, j int) bool {
		return conversationIDs[i].String() < conversationIDs[j].String()
	})
	advancedConversationIDs := make([]uuid.UUID, 0, len(candidates))
	if err := a.DB.Transaction(func(tx *gorm.DB) error {
		for _, conversationID := range conversationIDs {
			message := candidates[conversationID]
			_, advanced, err := advanceConversationReadCursor(
				tx,
				organizationID,
				userID,
				conversationID,
				&message,
				time.Time{},
			)
			if err != nil {
				return err
			}
			if advanced {
				advancedConversationIDs = append(advancedConversationIDs, conversationID)
			}
		}
		return nil
	}); err != nil {
		return legacyVisibleReadSyncResult{}, err
	}

	for i := range advancedConversationIDs {
		conversationID := advancedConversationIDs[i]
		a.publishRealtimeEvent(queue.RealtimeEvent{
			OrganizationID: organizationID,
			UserID:         &userID,
			Kind:           queue.RealtimeEventConversationChanged,
			ConversationID: &conversationID,
			Status:         "read_cursor_changed",
			OccurredAt:     time.Now().UTC(),
		}, nil)
	}
	result.AdvancedConversations = len(advancedConversationIDs)
	return result, nil
}
