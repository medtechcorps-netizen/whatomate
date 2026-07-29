package handlers

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/audit"
	channelapi "github.com/shridarpatil/whatomate/internal/channel"
	"github.com/shridarpatil/whatomate/internal/database"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/valyala/fasthttp"
	"github.com/zerodha/fastglue"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type InboxConversationResponse struct {
	ID                     uuid.UUID                      `json:"id"`
	OrganizationID         uuid.UUID                      `json:"organization_id"`
	ChannelAccountID       uuid.UUID                      `json:"channel_account_id"`
	ChannelAccount         *ChannelAccountResponse        `json:"channel_account,omitempty"`
	ContactID              uuid.UUID                      `json:"contact_id"`
	Contact                *models.Contact                `json:"contact,omitempty"`
	ContactIdentityID      *uuid.UUID                     `json:"contact_identity_id,omitempty"`
	ContactIdentity        *models.ContactIdentity        `json:"contact_identity,omitempty"`
	Channel                models.Channel                 `json:"channel"`
	ExternalConversationID string                         `json:"external_conversation_id"`
	Status                 models.InboxConversationStatus `json:"status"`
	Subject                string                         `json:"subject,omitempty"`
	Priority               int                            `json:"priority"`
	AssignedUserID         *uuid.UUID                     `json:"assigned_user_id,omitempty"`
	AssignedTeamID         *uuid.UUID                     `json:"assigned_team_id,omitempty"`
	UnreadCount            int                            `json:"unread_count"`
	LastMessagePreview     string                         `json:"last_message_preview,omitempty"`
	OpenedAt               time.Time                      `json:"opened_at"`
	LastMessageAt          *time.Time                     `json:"last_message_at,omitempty"`
	LastInboundAt          *time.Time                     `json:"last_inbound_at,omitempty"`
	LastOutboundAt         *time.Time                     `json:"last_outbound_at,omitempty"`
	ServiceWindowEndsAt    *time.Time                     `json:"service_window_ends_at,omitempty"`
	AIPaused               bool                           `json:"ai_paused"`
	AIPauseReason          string                         `json:"ai_pause_reason,omitempty"`
	SnoozedUntil           *time.Time                     `json:"snoozed_until,omitempty"`
	ResolvedAt             *time.Time                     `json:"resolved_at,omitempty"`
	Metadata               models.JSONB                   `json:"metadata"`
	CreatedAt              time.Time                      `json:"created_at"`
	UpdatedAt              time.Time                      `json:"updated_at"`
}

type InboxMessageResponse struct {
	Message models.Message       `json:"message"`
	Parts   []models.MessagePart `json:"parts"`
}

type SendInboxConversationMessageRequest struct {
	IdempotencyKey    string                          `json:"idempotency_key,omitempty"`
	Purpose           models.ChannelPreferencePurpose `json:"purpose"`
	Subject           string                          `json:"subject,omitempty"`
	CC                []channelapi.Participant        `json:"cc,omitempty"`
	Parts             []channelapi.MessagePart        `json:"parts"`
	ReplyToMessageID  *uuid.UUID                      `json:"reply_to_message_id,omitempty"`
	ReplyToExternalID string                          `json:"reply_to_external_id,omitempty"`
}

type MarkInboxConversationReadRequest struct {
	ExternalMessageIDs []string `json:"external_message_ids,omitempty"`
}

var errChannelIdempotencyCollision = errors.New(
	"idempotency key was already used for a different conversation or message payload",
)

func (a *App) ListInboxConversations(r *fastglue.Request) error {
	orgID, userID, err := a.requireAuth(r, models.ResourceConversations, models.ActionRead)
	if err != nil {
		return nil
	}
	pagination := parsePagination(r)
	readerKey := "user:" + userID.String()
	perUserUnreadSQL := `(SELECT COUNT(*) FROM messages AS unread_messages
		WHERE unread_messages.organization_id = inbox_conversations.organization_id
		  AND unread_messages.inbox_conversation_id = inbox_conversations.id
		  AND unread_messages.direction = ?
		  AND unread_messages.created_at > COALESCE(
			(SELECT conversation_reads.last_read_at
			 FROM conversation_reads
			 WHERE conversation_reads.organization_id = inbox_conversations.organization_id
			   AND conversation_reads.conversation_id = inbox_conversations.id
			   AND conversation_reads.reader_key = ?
			   AND conversation_reads.deleted_at IS NULL
			 LIMIT 1),
			TIMESTAMPTZ 'epoch'
		  ))`

	query := a.DB.Model(&models.InboxConversation{}).Where("organization_id = ?", orgID)
	if status := strings.TrimSpace(string(r.RequestCtx.QueryArgs().Peek("status"))); status != "" {
		query = query.Where("status = ?", strings.ToLower(status))
	}
	if channelName := strings.TrimSpace(string(r.RequestCtx.QueryArgs().Peek("channel"))); channelName != "" {
		query = query.Where("channel = ?", strings.ToLower(channelName))
	}
	if accountID := strings.TrimSpace(string(r.RequestCtx.QueryArgs().Peek("channel_account_id"))); accountID != "" {
		parsed, parseErr := uuid.Parse(accountID)
		if parseErr != nil {
			return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid channel account ID", nil, "")
		}
		query = query.Where("channel_account_id = ?", parsed)
	}
	if unreadOnly := strings.EqualFold(string(r.RequestCtx.QueryArgs().Peek("unread")), "true"); unreadOnly {
		query = query.Where(
			perUserUnreadSQL+" > 0",
			models.DirectionIncoming,
			readerKey,
		)
	}
	if search := strings.TrimSpace(string(r.RequestCtx.QueryArgs().Peek("search"))); search != "" {
		pattern := productCRMSearchPattern(search)
		query = query.Where(
			`(inbox_conversations.subject ILIKE ?
			  OR inbox_conversations.last_message_preview ILIKE ?
			  OR EXISTS (
				SELECT 1 FROM contacts
				WHERE contacts.id = inbox_conversations.contact_id
				  AND contacts.organization_id = inbox_conversations.organization_id
				  AND contacts.deleted_at IS NULL
				  AND (contacts.profile_name ILIKE ? OR contacts.phone_number ILIKE ?)
			  )
			  OR EXISTS (
				SELECT 1 FROM contact_identities
				WHERE contact_identities.id = inbox_conversations.contact_identity_id
				  AND contact_identities.organization_id = inbox_conversations.organization_id
				  AND contact_identities.deleted_at IS NULL
				  AND (
					contact_identities.display_name ILIKE ?
					OR contact_identities.external_id ILIKE ?
					OR contact_identities.normalized_address ILIKE ?
				  )
			  ))`,
			pattern,
			pattern,
			pattern,
			pattern,
			pattern,
			pattern,
			pattern,
		)
	}

	var total int64
	if err := query.Count(&total).Error; err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to list inbox conversations", nil, "")
	}

	var conversations []models.InboxConversation
	if err := pagination.Apply(query.Select(
		"inbox_conversations.*, "+perUserUnreadSQL+" AS unread_count",
		models.DirectionIncoming,
		readerKey,
	)).
		Preload("ChannelAccount", "organization_id = ?", orgID).
		Preload("Contact", "organization_id = ?", orgID).
		Preload("ContactIdentity", "organization_id = ?", orgID).
		Order("last_message_at DESC NULLS LAST, created_at DESC").
		Find(&conversations).Error; err != nil {
		a.Log.Error("Failed to list inbox conversations", "error", err, "organization_id", orgID)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to list inbox conversations", nil, "")
	}

	response := make([]InboxConversationResponse, len(conversations))
	for i := range conversations {
		response[i] = inboxConversationToResponse(&conversations[i])
	}
	return r.SendEnvelope(listEnvelope("conversations", response, total, pagination))
}

func (a *App) GetInboxConversationMessages(r *fastglue.Request) error {
	orgID, _, err := a.requireAuth(r, models.ResourceConversations, models.ActionRead)
	if err != nil {
		return nil
	}
	conversationID, err := parsePathUUID(r, "id", "conversation")
	if err != nil {
		return nil
	}
	if _, err := loadInboxConversation(a.DB, orgID, conversationID, false); err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusNotFound, "Conversation not found", nil, "")
	}
	pagination := parsePaginationWithDefaults(r, 50, 100)

	query := a.DB.Model(&models.Message{}).
		Where("organization_id = ? AND inbox_conversation_id = ?", orgID, conversationID)
	var total int64
	if err := query.Count(&total).Error; err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to load messages", nil, "")
	}
	if before := strings.TrimSpace(string(r.RequestCtx.QueryArgs().Peek("before"))); before != "" {
		beforeID, parseErr := uuid.Parse(before)
		if parseErr != nil || beforeID == uuid.Nil {
			return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "before must be a valid message ID", nil, "")
		}
		var cursor models.Message
		if err := a.DB.Select("id", "created_at").
			Where(
				"id = ? AND organization_id = ? AND inbox_conversation_id = ?",
				beforeID,
				orgID,
				conversationID,
			).
			First(&cursor).Error; err != nil {
			return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "before message was not found", nil, "")
		}
		query = query.Where(
			"(created_at < ? OR (created_at = ? AND id < ?))",
			cursor.CreatedAt,
			cursor.CreatedAt,
			cursor.ID,
		)
	}

	var messages []models.Message
	if err := pagination.Apply(query).
		Order("created_at DESC, id DESC").
		Find(&messages).Error; err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to load messages", nil, "")
	}

	messageIDs := make([]uuid.UUID, len(messages))
	for i := range messages {
		messageIDs[i] = messages[i].ID
	}
	partsByMessage := make(map[uuid.UUID][]models.MessagePart, len(messages))
	if len(messageIDs) > 0 {
		var parts []models.MessagePart
		if err := a.DB.
			Where("organization_id = ? AND conversation_id = ? AND message_id IN ?", orgID, conversationID, messageIDs).
			Order("message_id, position").
			Find(&parts).Error; err != nil {
			return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to load message parts", nil, "")
		}
		for _, part := range parts {
			partsByMessage[part.MessageID] = append(partsByMessage[part.MessageID], part)
		}
	}

	response := make([]InboxMessageResponse, len(messages))
	for i := range messages {
		response[i] = InboxMessageResponse{
			Message: messages[i],
			Parts:   partsByMessage[messages[i].ID],
		}
	}
	return r.SendEnvelope(listEnvelope("messages", response, total, pagination))
}

func (a *App) SendInboxConversationMessage(r *fastglue.Request) error {
	orgID, userID, err := a.requireAuth(r, models.ResourceConversations, models.ActionWrite)
	if err != nil {
		return nil
	}
	conversationID, err := parsePathUUID(r, "id", "conversation")
	if err != nil {
		return nil
	}
	var request SendInboxConversationMessageRequest
	if err := a.decodeRequest(r, &request); err != nil {
		return nil
	}
	if len(request.Parts) == 0 {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "At least one message part is required", nil, "")
	}
	request.Purpose = models.ChannelPreferencePurpose(
		strings.ToLower(strings.TrimSpace(string(request.Purpose))),
	)
	if err := channelapi.ValidateOutboundPurpose(request.Purpose); err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, err.Error(), nil, "")
	}
	if request.IdempotencyKey == "" {
		request.IdempotencyKey = uuid.NewString()
	} else {
		request.IdempotencyKey = strings.TrimSpace(request.IdempotencyKey)
		if request.IdempotencyKey == "" || len(request.IdempotencyKey) > 512 {
			return r.SendErrorEnvelope(
				fasthttp.StatusBadRequest,
				"Idempotency key must be non-empty and at most 512 characters",
				nil,
				"",
			)
		}
	}

	conversation, err := loadInboxConversation(a.DB, orgID, conversationID, true)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusNotFound, "Conversation not found", nil, "")
	}
	if conversation.Channel == models.ChannelThreads {
		allowed, entitlementErr := a.HasProductEntitlement(
			userID,
			orgID,
			channelapi.ThreadsPublicEngagementEntitlementKey,
		)
		if entitlementErr != nil {
			a.Log.Error(
				"Failed to evaluate Threads public engagement entitlement",
				"error",
				entitlementErr,
				"organization_id",
				orgID,
				"conversation_id",
				conversation.ID,
			)
			return r.SendErrorEnvelope(
				fasthttp.StatusServiceUnavailable,
				"Threads public engagement entitlement could not be evaluated",
				nil,
				"",
			)
		}
		if !allowed {
			return r.SendErrorEnvelope(
				fasthttp.StatusPaymentRequired,
				"Threads public replies are not included in the organization's active plan",
				nil,
				"",
			)
		}
	}
	if err := validateThreadsPublicReplyTarget(conversation, &request); err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, err.Error(), nil, "")
	}
	if conversation.ChannelAccount == nil ||
		conversation.ChannelAccount.Status != models.ChannelAccountStatusActive {
		return r.SendErrorEnvelope(fasthttp.StatusConflict, "Channel account is not active", nil, "")
	}
	if conversation.ChannelAccount.Provider == channelapi.LegacyMetaProvider {
		return r.SendErrorEnvelope(
			fasthttp.StatusConflict,
			"Reply to this WhatsApp conversation in the established chat composer",
			map[string]any{"reply_path": "/chat/" + conversation.ContactID.String()},
			"",
		)
	}
	if !boolConfigValue(conversation.ChannelAccount.Config, "outbound_enabled") {
		return r.SendErrorEnvelope(fasthttp.StatusConflict, "Outbound delivery is not approved for this channel account", nil, "")
	}
	if currentChannelCredential(conversation.ChannelAccount) == nil {
		return r.SendErrorEnvelope(fasthttp.StatusConflict, "Channel account credentials are unavailable", nil, "")
	}
	adapter, err := a.channelAdapter(conversation.ChannelAccount)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusConflict, "No approved adapter is available for this provider", nil, "")
	}
	capabilities := adapter.Capabilities(conversation.ChannelAccount)
	if err := validateOutboundParts(capabilities, request); err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, err.Error(), nil, "")
	}
	if err := channelapi.ValidateServiceWindow(
		capabilities,
		conversation.ServiceWindowEndsAt,
		request.Parts,
		time.Now().UTC(),
	); err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusConflict, err.Error(), nil, "")
	}
	durablyEntitled := false
	consentAllowed := false
	consentReason := ""
	if err := database.WithTenant(a.DB, orgID, func(tx *gorm.DB) error {
		var entitlementErr error
		durablyEntitled, entitlementErr = channelapi.HasDurableOmnichannelEntitlement(
			tx,
			orgID,
			time.Now().UTC(),
		)
		if entitlementErr != nil || !durablyEntitled {
			return entitlementErr
		}
		var consentErr error
		consentAllowed, consentReason, consentErr = channelapi.OutboundConsentAllowed(
			tx,
			orgID,
			conversation.ContactID,
			conversation.ChannelAccountID,
			conversation.Channel,
			request.Purpose,
			time.Now().UTC(),
		)
		return consentErr
	}); err != nil {
		a.Log.Error(
			"Failed to evaluate channel delivery policy",
			"error",
			err,
			"organization_id",
			orgID,
			"conversation_id",
			conversation.ID,
		)
		return r.SendErrorEnvelope(
			fasthttp.StatusServiceUnavailable,
			"Message delivery policy could not be evaluated",
			nil,
			"",
		)
	}
	if !durablyEntitled {
		return r.SendErrorEnvelope(
			fasthttp.StatusForbidden,
			"Omnichannel subscription is not active",
			nil,
			"",
		)
	}
	if !consentAllowed {
		return r.SendErrorEnvelope(
			fasthttp.StatusConflict,
			"Contact consent does not permit this message purpose",
			map[string]any{"reason": consentReason},
			"",
		)
	}

	payloadDigest, err := channelSendRequestDigest(conversation.ID, request)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Message payload cannot be encoded", nil, "")
	}

	var existingJob models.OutboxJob
	if err := a.DB.
		Where(
			"organization_id = ? AND channel_account_id = ? AND idempotency_key = ?",
			orgID,
			conversation.ChannelAccountID,
			request.IdempotencyKey,
		).
		First(&existingJob).Error; err == nil {
		if err := validateChannelOutboxReplay(&existingJob, conversation.ID, payloadDigest); err != nil {
			return r.SendErrorEnvelope(fasthttp.StatusConflict, err.Error(), nil, "")
		}
		var existingMessage models.Message
		if existingJob.MessageID != nil {
			_ = a.DB.
				Where("id = ? AND organization_id = ? AND inbox_conversation_id = ?", *existingJob.MessageID, orgID, conversation.ID).
				First(&existingMessage).Error
		}
		return r.SendEnvelope(map[string]any{
			"message":    existingMessage,
			"outbox_job": existingJob,
			"idempotent": true,
		})
	} else if !errors.Is(err, gorm.ErrRecordNotFound) {
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to check message idempotency", nil, "")
	}

	now := time.Now().UTC()
	messageID := uuid.New()
	outbound := channelapi.OutboundMessage{
		OrganizationID: orgID,
		MessageID:      messageID,
		IdempotencyKey: request.IdempotencyKey,
		Purpose:        request.Purpose,
		Conversation: channelapi.ConversationRef{
			ID:         conversation.ID,
			ExternalID: conversation.ExternalConversationID,
			Subject:    request.Subject,
		},
		Recipient: channelapi.Participant{
			ID:          conversation.ContactID,
			ExternalID:  contactExternalID(conversation),
			Address:     contactAddress(conversation),
			DisplayName: contactDisplayName(conversation),
			Role:        models.ConversationParticipantRoleCustomer,
		},
		CC:                  request.CC,
		Subject:             request.Subject,
		Parts:               request.Parts,
		ReplyToExternalID:   request.ReplyToExternalID,
		ServiceWindowEndsAt: conversation.ServiceWindowEndsAt,
		Metadata: map[string]any{
			"sent_by_user_id": userID,
		},
	}
	payload, err := valueToJSONB(outbound)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Message payload cannot be encoded", nil, "")
	}

	message := legacyMessageFromParts(
		orgID,
		conversation,
		messageID,
		models.DirectionOutgoing,
		request.Parts,
		now,
	)
	message.SentByUserID = &userID
	message.Status = models.MessageStatusPending
	message.IsReply = request.ReplyToMessageID != nil || request.ReplyToExternalID != ""
	message.ReplyToMessageID = request.ReplyToMessageID
	message.Metadata = models.JSONB{
		"channel":         conversation.Channel,
		"provider":        conversation.ChannelAccount.Provider,
		"idempotency_key": request.IdempotencyKey,
		"purpose":         request.Purpose,
		"payload_digest":  payloadDigest,
	}

	parts := persistentMessageParts(orgID, conversation.ID, message.ID, request.Parts)
	outbox := models.OutboxJob{
		OrganizationID:   orgID,
		ChannelAccountID: conversation.ChannelAccountID,
		ConversationID:   conversation.ID,
		MessageID:        &message.ID,
		IdempotencyKey:   request.IdempotencyKey,
		PayloadDigest:    payloadDigest,
		Purpose:          request.Purpose,
		Status:           models.OutboxJobStatusPending,
		Priority:         conversation.Priority,
		AvailableAt:      now,
		MaxAttempts:      8,
		Payload:          payload,
	}

	err = a.DB.Transaction(func(tx *gorm.DB) error {
		// A reply can only target a message in the same tenant conversation.
		if request.ReplyToMessageID != nil {
			var count int64
			if err := tx.Model(&models.Message{}).
				Where(
					"id = ? AND organization_id = ? AND inbox_conversation_id = ?",
					*request.ReplyToMessageID,
					orgID,
					conversation.ID,
				).
				Count(&count).Error; err != nil {
				return err
			}
			if count != 1 {
				return errors.New("reply message was not found in this conversation")
			}
		}
		if _, err := setInboxConversationAIStateTx(
			tx,
			orgID,
			conversation.ID,
			true,
			&userID,
			"human_reply",
		); err != nil {
			return err
		}
		if err := tx.Create(&message).Error; err != nil {
			return err
		}
		if len(parts) > 0 {
			if err := tx.Create(&parts).Error; err != nil {
				return err
			}
		}
		if err := tx.Create(&outbox).Error; err != nil {
			return err
		}
		preview := messagePreview(request.Parts)
		if err := tx.Model(&models.InboxConversation{}).
			Where("id = ? AND organization_id = ?", conversation.ID, orgID).
			Updates(map[string]any{
				"last_message_at":      now,
				"last_outbound_at":     now,
				"last_message_preview": preview,
				"updated_at":           now,
			}).Error; err != nil {
			return err
		}
		if err := audit.LogAudit(
			tx,
			orgID,
			userID,
			audit.GetUserName(tx, userID),
			"inbox_message",
			message.ID,
			models.AuditActionCreated,
			nil,
			&message,
		); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		if strings.Contains(err.Error(), "reply message") {
			return r.SendErrorEnvelope(fasthttp.StatusBadRequest, err.Error(), nil, "")
		}
		// A concurrent request may have won the unique idempotency-key insert.
		// Re-read and return it only when both conversation and canonical
		// request digest match.
		var concurrent models.OutboxJob
		if lookupErr := a.DB.
			Where(
				"organization_id = ? AND channel_account_id = ? AND idempotency_key = ?",
				orgID,
				conversation.ChannelAccountID,
				request.IdempotencyKey,
			).
			First(&concurrent).Error; lookupErr == nil {
			if replayErr := validateChannelOutboxReplay(&concurrent, conversation.ID, payloadDigest); replayErr != nil {
				return r.SendErrorEnvelope(fasthttp.StatusConflict, replayErr.Error(), nil, "")
			}
			var existingMessage models.Message
			if concurrent.MessageID != nil {
				_ = a.DB.
					Where(
						"id = ? AND organization_id = ? AND inbox_conversation_id = ?",
						*concurrent.MessageID,
						orgID,
						conversation.ID,
					).
					First(&existingMessage).Error
			}
			return r.SendEnvelope(map[string]any{
				"message":    existingMessage,
				"outbox_job": concurrent,
				"idempotent": true,
			})
		}
		return r.SendErrorEnvelope(fasthttp.StatusConflict, "Failed to queue message", nil, "")
	}

	r.RequestCtx.SetStatusCode(fasthttp.StatusAccepted)
	return r.SendEnvelope(map[string]any{
		"message":    message,
		"parts":      parts,
		"outbox_job": outbox,
		"idempotent": false,
	})
}

func (a *App) MarkInboxConversationRead(r *fastglue.Request) error {
	orgID, userID, err := a.requireAuth(r, models.ResourceConversations, models.ActionWrite)
	if err != nil {
		return nil
	}
	conversationID, err := parsePathUUID(r, "id", "conversation")
	if err != nil {
		return nil
	}
	var request MarkInboxConversationReadRequest
	if len(r.RequestCtx.PostBody()) > 0 {
		if err := a.decodeRequest(r, &request); err != nil {
			return nil
		}
	}

	conversation, err := loadInboxConversation(a.DB, orgID, conversationID, true)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusNotFound, "Conversation not found", nil, "")
	}
	now := time.Now().UTC()
	read := models.ConversationRead{
		OrganizationID: orgID,
		ConversationID: conversation.ID,
		UserID:         &userID,
		ReaderKey:      "user:" + userID.String(),
		LastReadAt:     now,
		Metadata:       models.JSONB{},
	}

	err = a.DB.Transaction(func(tx *gorm.DB) error {
		var lastMessage models.Message
		if err := tx.
			Where("organization_id = ? AND inbox_conversation_id = ?", orgID, conversation.ID).
			Order("created_at DESC").
			First(&lastMessage).Error; err == nil {
			read.LastReadMessageID = &lastMessage.ID
			read.LastReadExternalID = lastMessage.WhatsAppMessageID
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		if err := tx.Clauses(clause.OnConflict{
			Columns: []clause.Column{
				{Name: "organization_id"},
				{Name: "conversation_id"},
				{Name: "reader_key"},
			},
			DoUpdates: clause.AssignmentColumns([]string{
				"user_id",
				"last_read_message_id",
				"last_read_external_id",
				"last_read_at",
				"metadata",
				"updated_at",
			}),
		}).Create(&read).Error; err != nil {
			return err
		}
		return audit.LogAudit(
			tx,
			orgID,
			userID,
			audit.GetUserName(tx, userID),
			"conversation_read",
			conversation.ID,
			models.AuditActionUpdated,
			nil,
			map[string]any{
				"reader_key":           read.ReaderKey,
				"last_read_message_id": read.LastReadMessageID,
				"last_read_at":         read.LastReadAt,
			},
		)
	})
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to mark conversation as read", nil, "")
	}

	legacyStateSynced := false
	if conversation.ChannelAccount != nil &&
		conversation.ChannelAccount.Provider == channelapi.LegacyMetaProvider &&
		conversation.Contact != nil {
		// The legacy chat is still the WhatsApp delivery authority. Keep its
		// contact/message read state aligned when an agent opens the mirrored
		// thread in the normalized inbox.
		a.markMessagesAsRead(orgID, conversation.ContactID, conversation.Contact)
		legacyStateSynced = true
	}

	providerSynced := false
	if len(request.ExternalMessageIDs) > 0 &&
		conversation.ChannelAccount != nil &&
		conversation.ChannelAccount.Status == models.ChannelAccountStatusActive {
		if adapter, adapterErr := a.channelAdapter(conversation.ChannelAccount); adapterErr == nil &&
			adapter.Capabilities(conversation.ChannelAccount).ReadReceipts {
			if markErr := adapter.MarkRead(
				requestContext(r),
				conversation.ChannelAccount,
				channelapi.ConversationRef{
					ID:         conversation.ID,
					ExternalID: conversation.ExternalConversationID,
				},
				request.ExternalMessageIDs,
			); markErr == nil {
				providerSynced = true
			} else {
				a.Log.Warn(
					"Failed to sync conversation read receipt",
					"error",
					markErr,
					"organization_id",
					orgID,
					"conversation_id",
					conversation.ID,
				)
			}
		}
	}

	return r.SendEnvelope(map[string]any{
		"read_at":             now,
		"provider_synced":     providerSynced,
		"legacy_state_synced": legacyStateSynced,
	})
}

func loadInboxConversation(db *gorm.DB, orgID, conversationID uuid.UUID, credentials bool) (*models.InboxConversation, error) {
	query := db.
		Preload("Contact", "organization_id = ?", orgID).
		Preload("ContactIdentity", "organization_id = ?", orgID)
	if credentials {
		now := time.Now().UTC()
		query = query.
			Preload("ChannelAccount", "organization_id = ?", orgID).
			Preload("ChannelAccount.Credentials", func(credentials *gorm.DB) *gorm.DB {
				return credentials.
					Where(
						"organization_id = ? AND status IN ? AND (expires_at IS NULL OR expires_at > ?)",
						orgID,
						[]models.ChannelCredentialStatus{
							models.ChannelCredentialStatusActive,
							models.ChannelCredentialStatusExpiring,
						},
						now,
					).
					Order("version DESC").
					Order("id ASC")
			})
	} else {
		query = query.Preload("ChannelAccount", "organization_id = ?", orgID)
	}
	var conversation models.InboxConversation
	if err := query.
		Where("id = ? AND organization_id = ?", conversationID, orgID).
		First(&conversation).Error; err != nil {
		return nil, err
	}
	return &conversation, nil
}

func inboxConversationToResponse(conversation *models.InboxConversation) InboxConversationResponse {
	var account *ChannelAccountResponse
	if conversation.ChannelAccount != nil {
		value := channelAccountToResponse(conversation.ChannelAccount)
		account = &value
	}
	return InboxConversationResponse{
		ID:                     conversation.ID,
		OrganizationID:         conversation.OrganizationID,
		ChannelAccountID:       conversation.ChannelAccountID,
		ChannelAccount:         account,
		ContactID:              conversation.ContactID,
		Contact:                conversation.Contact,
		ContactIdentityID:      conversation.ContactIdentityID,
		ContactIdentity:        conversation.ContactIdentity,
		Channel:                conversation.Channel,
		ExternalConversationID: conversation.ExternalConversationID,
		Status:                 conversation.Status,
		Subject:                conversation.Subject,
		Priority:               conversation.Priority,
		AssignedUserID:         conversation.AssignedUserID,
		AssignedTeamID:         conversation.AssignedTeamID,
		UnreadCount:            conversation.UnreadCount,
		LastMessagePreview:     conversation.LastMessagePreview,
		OpenedAt:               conversation.OpenedAt,
		LastMessageAt:          conversation.LastMessageAt,
		LastInboundAt:          conversation.LastInboundAt,
		LastOutboundAt:         conversation.LastOutboundAt,
		ServiceWindowEndsAt:    conversation.ServiceWindowEndsAt,
		AIPaused:               inboxConversationAIIsPaused(conversation.Config),
		AIPauseReason:          inboxConversationAIString(conversation.Config, models.ConversationConfigAIPauseReason),
		SnoozedUntil:           conversation.SnoozedUntil,
		ResolvedAt:             conversation.ResolvedAt,
		Metadata:               cloneJSONB(conversation.Metadata),
		CreatedAt:              conversation.CreatedAt,
		UpdatedAt:              conversation.UpdatedAt,
	}
}

func validateOutboundParts(capabilities channelapi.Capabilities, request SendInboxConversationMessageRequest) error {
	if err := channelapi.ValidateMessagePartsShape(request.Parts); err != nil {
		return err
	}
	if utf8.RuneCountInString(request.Subject) > 998 {
		return errors.New("message subject is too long")
	}
	if len(request.Parts) > 1 && !capabilities.MultipleAttachments {
		return errors.New("this channel does not support multiple message parts")
	}
	if len(request.CC) > 0 && !capabilities.SubjectAndCC {
		return errors.New("this channel does not support CC recipients")
	}
	if request.Subject != "" && !capabilities.SubjectAndCC {
		return errors.New("this channel does not support message subjects")
	}
	if (request.ReplyToMessageID != nil || request.ReplyToExternalID != "") && !capabilities.Replies {
		return errors.New("this channel does not support replies")
	}
	attachmentCount := 0
	for index, part := range request.Parts {
		switch part.Type {
		case models.MessagePartTypeText, models.MessagePartTypeHTML:
			if !capabilities.Text {
				return errors.New("this channel does not support text")
			}
			if strings.TrimSpace(part.Text) == "" {
				return fmt.Errorf("message part %d text is required", index)
			}
		case models.MessagePartTypeImage,
			models.MessagePartTypeVideo,
			models.MessagePartTypeAudio,
			models.MessagePartTypeDocument:
			attachmentCount++
			if !capabilities.Media {
				return errors.New("this channel does not support media")
			}
			if part.MediaURL == "" && part.ProviderMediaRef == "" {
				return fmt.Errorf("message part %d media reference is required", index)
			}
			if capabilities.MaxMediaBytes > 0 && part.SizeBytes > capabilities.MaxMediaBytes {
				return fmt.Errorf("message part %d exceeds the provider media size limit", index)
			}
			if part.MimeType != "" &&
				len(capabilities.SupportedMediaTypes) > 0 &&
				!channelSupportsMediaType(capabilities.SupportedMediaTypes, part.MimeType) {
				return fmt.Errorf("message part %d media type is not supported", index)
			}
		case models.MessagePartTypeInteractive:
			if !capabilities.Buttons {
				return errors.New("this channel does not support interactive buttons")
			}
		case models.MessagePartTypeTemplate:
			if !capabilities.Templates {
				return errors.New("this channel does not support templates")
			}
		case models.MessagePartTypeReaction:
			if !capabilities.Reactions {
				return errors.New("this channel does not support reactions")
			}
		case models.MessagePartTypeLocation, models.MessagePartTypeContact:
			return fmt.Errorf("message part %d type %q is not supported for outbound delivery", index, part.Type)
		default:
			return fmt.Errorf("message part %d type %q is not supported", index, part.Type)
		}
	}
	if capabilities.MaxAttachments > 0 && attachmentCount > capabilities.MaxAttachments {
		return errors.New("message exceeds the provider attachment limit")
	}
	for index, participant := range request.CC {
		if utf8.RuneCountInString(participant.ExternalID) > 512 ||
			utf8.RuneCountInString(participant.Address) > 320 ||
			utf8.RuneCountInString(participant.DisplayName) > 255 {
			return fmt.Errorf("cc recipient %d contains an overlong field", index)
		}
	}
	return nil
}

func validateThreadsPublicReplyTarget(
	conversation *models.InboxConversation,
	request *SendInboxConversationMessageRequest,
) error {
	if conversation == nil || conversation.Channel != models.ChannelThreads {
		return nil
	}
	if request == nil {
		return errors.New("threads public replies require an existing reply or mention target")
	}
	if conversation.ChannelAccount == nil ||
		!strings.EqualFold(conversation.ChannelAccount.Provider, channelapi.RelayProvider) {
		return errors.New("threads public replies require a compatible signed provider relay")
	}
	if stringConfigValue(conversation.ChannelAccount.Config, "engagement_mode") != threadsPublicEngagementMode {
		return errors.New("threads channel is not configured for public replies and mentions")
	}

	target := request.ReplyToExternalID
	conversationTarget := conversation.ExternalConversationID
	if strings.TrimSpace(target) == "" || strings.TrimSpace(conversationTarget) == "" {
		return errors.New("threads public replies require an existing reply or mention target")
	}
	if target != conversationTarget {
		return errors.New("threads reply target must match the selected public conversation")
	}

	engagementType, _ := conversation.Metadata["engagement_type"].(string)
	switch strings.ToLower(strings.TrimSpace(engagementType)) {
	case "reply", "mention":
		request.ReplyToExternalID = conversationTarget
		return nil
	default:
		return errors.New("threads direct messages and standalone posts are not supported; select a public reply or mention")
	}
}

func channelSupportsMediaType(supported []string, value string) bool {
	for _, mediaType := range supported {
		if strings.EqualFold(strings.TrimSpace(mediaType), strings.TrimSpace(value)) {
			return true
		}
	}
	return false
}

func channelSendRequestDigest(
	conversationID uuid.UUID,
	request SendInboxConversationMessageRequest,
) (string, error) {
	canonical := struct {
		ConversationID    uuid.UUID                       `json:"conversation_id"`
		Purpose           models.ChannelPreferencePurpose `json:"purpose"`
		Subject           string                          `json:"subject,omitempty"`
		CC                []channelapi.Participant        `json:"cc,omitempty"`
		Parts             []channelapi.MessagePart        `json:"parts"`
		ReplyToMessageID  *uuid.UUID                      `json:"reply_to_message_id,omitempty"`
		ReplyToExternalID string                          `json:"reply_to_external_id,omitempty"`
	}{
		ConversationID:    conversationID,
		Purpose:           request.Purpose,
		Subject:           request.Subject,
		CC:                request.CC,
		Parts:             request.Parts,
		ReplyToMessageID:  request.ReplyToMessageID,
		ReplyToExternalID: request.ReplyToExternalID,
	}
	encoded, err := json.Marshal(canonical)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}

func validateChannelOutboxReplay(
	job *models.OutboxJob,
	conversationID uuid.UUID,
	payloadDigest string,
) error {
	if job == nil ||
		job.ConversationID != conversationID ||
		job.PayloadDigest == "" ||
		job.PayloadDigest != payloadDigest {
		return errChannelIdempotencyCollision
	}
	return nil
}

func legacyMessageFromParts(
	orgID uuid.UUID,
	conversation *models.InboxConversation,
	messageID uuid.UUID,
	direction models.Direction,
	parts []channelapi.MessagePart,
	when time.Time,
) models.Message {
	message := models.Message{
		BaseModel: models.BaseModel{
			ID:        messageID,
			CreatedAt: when,
			UpdatedAt: when,
		},
		OrganizationID:      orgID,
		WhatsAppAccount:     conversation.ChannelAccount.Name,
		ContactID:           conversation.ContactID,
		ConversationID:      legacyConversationID(conversation.ExternalConversationID),
		InboxConversationID: &conversation.ID,
		Direction:           direction,
		MessageType:         models.MessageTypeText,
		Status:              models.MessageStatusPending,
		Metadata:            models.JSONB{},
	}
	if len(parts) == 0 {
		return message
	}
	first := parts[0]
	message.MessageType = legacyMessageType(first.Type)
	message.Content = first.Text
	if message.Content == "" {
		message.Content = first.Caption
	}
	message.MediaURL = first.MediaURL
	message.MediaMimeType = truncateChannelRunes(first.MimeType, 100)
	message.MediaFilename = truncateChannelRunes(first.Filename, 255)
	return message
}

func persistentMessageParts(
	orgID, conversationID, messageID uuid.UUID,
	parts []channelapi.MessagePart,
) []models.MessagePart {
	result := make([]models.MessagePart, len(parts))
	for i, part := range parts {
		result[i] = models.MessagePart{
			OrganizationID:   orgID,
			MessageID:        messageID,
			ConversationID:   conversationID,
			Position:         i,
			Type:             part.Type,
			Status:           models.MessagePartStatusReady,
			Text:             part.Text,
			Caption:          part.Caption,
			MediaURL:         part.MediaURL,
			ProviderMediaRef: part.ProviderMediaRef,
			MimeType:         part.MimeType,
			Filename:         part.Filename,
			SizeBytes:        part.SizeBytes,
			Payload:          mapToJSONB(part.Payload),
		}
	}
	return result
}

func legacyMessageType(partType models.MessagePartType) models.MessageType {
	switch partType {
	case models.MessagePartTypeImage:
		return models.MessageTypeImage
	case models.MessagePartTypeVideo:
		return models.MessageTypeVideo
	case models.MessagePartTypeAudio:
		return models.MessageTypeAudio
	case models.MessagePartTypeDocument:
		return models.MessageTypeDocument
	case models.MessagePartTypeLocation:
		return models.MessageTypeLocation
	case models.MessagePartTypeContact:
		return models.MessageTypeContact
	case models.MessagePartTypeInteractive:
		return models.MessageTypeInteractive
	case models.MessagePartTypeTemplate:
		return models.MessageTypeTemplate
	case models.MessagePartTypeReaction:
		return models.MessageTypeReaction
	default:
		return models.MessageTypeText
	}
}

func messagePreview(parts []channelapi.MessagePart) string {
	if len(parts) == 0 {
		return ""
	}
	preview := strings.TrimSpace(parts[0].Text)
	if preview == "" {
		preview = strings.TrimSpace(parts[0].Caption)
	}
	if preview == "" {
		preview = "[" + string(parts[0].Type) + "]"
	}
	const maxPreviewRunes = 500
	runes := []rune(preview)
	if len(runes) > maxPreviewRunes {
		preview = string(runes[:maxPreviewRunes])
	}
	return preview
}

func valueToJSONB(value any) (models.JSONB, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var result models.JSONB
	if err := json.Unmarshal(encoded, &result); err != nil {
		return nil, err
	}
	return result, nil
}

func contactExternalID(conversation *models.InboxConversation) string {
	if conversation.ContactIdentity != nil && conversation.ContactIdentity.ExternalID != "" {
		return conversation.ContactIdentity.ExternalID
	}
	if conversation.Contact != nil {
		return conversation.Contact.PhoneNumber
	}
	return ""
}

func contactAddress(conversation *models.InboxConversation) string {
	if conversation.ContactIdentity != nil && conversation.ContactIdentity.Address != "" {
		return conversation.ContactIdentity.Address
	}
	if conversation.Contact != nil {
		return conversation.Contact.PhoneNumber
	}
	return ""
}

func contactDisplayName(conversation *models.InboxConversation) string {
	if conversation.ContactIdentity != nil && conversation.ContactIdentity.DisplayName != "" {
		return conversation.ContactIdentity.DisplayName
	}
	if conversation.Contact != nil {
		return conversation.Contact.ProfileName
	}
	return ""
}

func legacyConversationID(value string) string {
	if utf8.RuneCountInString(value) <= 255 {
		return value
	}
	digest := sha256.Sum256([]byte(value))
	return "channel:" + hex.EncodeToString(digest[:])
}

func truncateChannelRunes(value string, limit int) string {
	if limit <= 0 {
		return ""
	}
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit])
}
