package handlers

import (
	"context"
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
	"github.com/shridarpatil/whatomate/internal/metaregistry"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/internal/queue"
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

// InboxAttentionSummaryResponse is intentionally content-free so the shared
// navigation can poll it without loading customer or message records.
type InboxAttentionSummaryResponse struct {
	UnreadConversations int64     `json:"unread_conversations"`
	AsOf                time.Time `json:"as_of"`
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
	LastVisibleMessageID *uuid.UUID `json:"last_visible_message_id,omitempty"`
	ExternalMessageIDs   []string   `json:"external_message_ids,omitempty"`
}

type markInboxConversationReadResponse struct {
	ReadAt             time.Time  `json:"read_at"`
	LastReadIngestedAt *time.Time `json:"last_read_ingested_at,omitempty"`
	LastReadMessageID  *uuid.UUID `json:"last_read_message_id,omitempty"`
	UnreadCount        int64      `json:"unread_count"`
	ProviderSynced     bool       `json:"provider_synced"`
	ProviderSyncQueued bool       `json:"provider_sync_queued"`
	LegacyStateSynced  bool       `json:"legacy_state_synced"`
}

var errChannelIdempotencyCollision = errors.New(
	"idempotency key was already used for a different conversation or message payload",
)

var errChannelAccountUnavailableAtEnqueue = errors.New(
	"channel account became unavailable before the message was queued",
)

var errInboxReadCursorMessageNotFound = errors.New(
	"last visible message was not found in the conversation",
)

const (
	maxInboxReadReceiptMessageIDs      = 500
	maxInboxReadReceiptMessageIDLength = 255
)

func (a *App) ListInboxConversations(r *fastglue.Request) error {
	orgID, userID, err := a.requireAuth(r, models.ResourceConversations, models.ActionRead)
	if err != nil {
		return nil
	}
	pagination := parsePagination(r)
	readerKey := "user:" + userID.String()
	perUserUnreadSQL := `(SELECT COUNT(*) FROM messages AS unread_messages
		LEFT JOIN conversation_reads AS read_cursor
		  ON read_cursor.organization_id = inbox_conversations.organization_id
		 AND read_cursor.conversation_id = inbox_conversations.id
		 AND read_cursor.reader_key = ?
		 AND read_cursor.deleted_at IS NULL
		WHERE unread_messages.organization_id = inbox_conversations.organization_id
		  AND unread_messages.inbox_conversation_id = inbox_conversations.id
		  AND unread_messages.deleted_at IS NULL
		  AND unread_messages.direction = ?
		  AND (
			read_cursor.id IS NULL
			OR COALESCE(unread_messages.ingested_at, unread_messages.created_at) >
			   COALESCE(read_cursor.last_read_ingested_at, read_cursor.last_read_at)
			OR (
			  COALESCE(unread_messages.ingested_at, unread_messages.created_at) =
			  COALESCE(read_cursor.last_read_ingested_at, read_cursor.last_read_at)
			  AND (
				read_cursor.last_read_message_id IS NULL
				OR unread_messages.id > read_cursor.last_read_message_id
			  )
			)
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
			readerKey,
			models.DirectionIncoming,
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
		readerKey,
		models.DirectionIncoming,
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
		response[i] = a.inboxConversationToResponse(&conversations[i])
	}
	return r.SendEnvelope(listEnvelope("conversations", response, total, pagination))
}

// GetInboxAttentionSummary returns the current user's unread conversation
// count for the authenticated tenant. Read cursors are user-specific, so this
// must not use the legacy shared unread_count column.
func (a *App) GetInboxAttentionSummary(r *fastglue.Request) error {
	orgID, userID, err := a.requireAuth(r, models.ResourceConversations, models.ActionRead)
	if err != nil {
		return nil
	}

	response := InboxAttentionSummaryResponse{AsOf: time.Now().UTC()}
	readerKey := "user:" + userID.String()
	if err := a.DB.Raw(`
		SELECT COUNT(*) AS unread_conversations
		FROM inbox_conversations AS inbox_conversation
		WHERE inbox_conversation.organization_id = ?
		  AND inbox_conversation.deleted_at IS NULL
		  AND EXISTS (
			SELECT 1
			FROM messages AS unread_message
			LEFT JOIN conversation_reads AS read_cursor
			  ON read_cursor.organization_id = inbox_conversation.organization_id
			 AND read_cursor.conversation_id = inbox_conversation.id
			 AND read_cursor.reader_key = ?
			 AND read_cursor.deleted_at IS NULL
			WHERE unread_message.organization_id = inbox_conversation.organization_id
			  AND unread_message.inbox_conversation_id = inbox_conversation.id
			  AND unread_message.deleted_at IS NULL
			  AND unread_message.direction = ?
			  AND (
				read_cursor.id IS NULL
				OR COALESCE(unread_message.ingested_at, unread_message.created_at) >
				   COALESCE(read_cursor.last_read_ingested_at, read_cursor.last_read_at)
				OR (
				  COALESCE(unread_message.ingested_at, unread_message.created_at) =
				  COALESCE(read_cursor.last_read_ingested_at, read_cursor.last_read_at)
				  AND (
					read_cursor.last_read_message_id IS NULL
					OR unread_message.id > read_cursor.last_read_message_id
				  )
				)
			  )
		  )`,
		orgID,
		readerKey,
		models.DirectionIncoming,
	).Scan(&response).Error; err != nil {
		a.Log.Error(
			"Failed to load inbox attention summary",
			"error", err,
			"organization_id", orgID,
			"user_id", userID,
		)
		return r.SendErrorEnvelope(
			fasthttp.StatusInternalServerError,
			"Failed to load inbox attention summary",
			nil,
			"",
		)
	}

	return r.SendEnvelope(response)
}

func countUnreadInboxMessages(
	db *gorm.DB,
	organizationID, userID, conversationID uuid.UUID,
) (int64, error) {
	var unreadCount int64
	err := db.Raw(`
		SELECT COUNT(*)
		FROM messages AS unread_message
		LEFT JOIN conversation_reads AS read_cursor
		  ON read_cursor.organization_id = unread_message.organization_id
		 AND read_cursor.conversation_id = unread_message.inbox_conversation_id
		 AND read_cursor.reader_key = ?
		 AND read_cursor.deleted_at IS NULL
		WHERE unread_message.organization_id = ?
		  AND unread_message.inbox_conversation_id = ?
		  AND unread_message.deleted_at IS NULL
		  AND unread_message.direction = ?
		  AND (
			read_cursor.id IS NULL
			OR COALESCE(unread_message.ingested_at, unread_message.created_at) >
			   COALESCE(read_cursor.last_read_ingested_at, read_cursor.last_read_at)
			OR (
			  COALESCE(unread_message.ingested_at, unread_message.created_at) =
			  COALESCE(read_cursor.last_read_ingested_at, read_cursor.last_read_at)
			  AND (
				read_cursor.last_read_message_id IS NULL
				OR unread_message.id > read_cursor.last_read_message_id
			  )
			)
		  )`,
		"user:"+userID.String(),
		organizationID,
		conversationID,
		models.DirectionIncoming,
	).Scan(&unreadCount).Error
	return unreadCount, err
}

// validateInboxReadReceiptMessageIDs proves that every opaque provider ID in a
// receipt request belongs to an incoming message in the exact tenant and
// conversation, and that no requested message is newer than what the agent
// explicitly reported as visible. The boolean deliberately does not describe
// which ID failed validation.
func validateInboxReadReceiptMessageIDs(
	db *gorm.DB,
	organizationID, conversationID uuid.UUID,
	visibleMessage *models.Message,
	requestedIDs []string,
) ([]string, bool, error) {
	if db == nil || organizationID == uuid.Nil || conversationID == uuid.Nil ||
		visibleMessage == nil || visibleMessage.ID == uuid.Nil ||
		visibleMessage.OrganizationID != organizationID ||
		visibleMessage.InboxConversationID == nil ||
		*visibleMessage.InboxConversationID != conversationID ||
		len(requestedIDs) == 0 || len(requestedIDs) > maxInboxReadReceiptMessageIDs {
		return nil, false, nil
	}

	uniqueIDs := make([]string, 0, len(requestedIDs))
	requestedSet := make(map[string]struct{}, len(requestedIDs))
	for _, externalID := range requestedIDs {
		if strings.TrimSpace(externalID) == "" ||
			len(externalID) > maxInboxReadReceiptMessageIDLength {
			return nil, false, nil
		}
		if _, exists := requestedSet[externalID]; exists {
			continue
		}
		requestedSet[externalID] = struct{}{}
		uniqueIDs = append(uniqueIDs, externalID)
	}

	visibleIngestedAt := visibleMessage.EffectiveIngestedAt()
	var matchedIDs []string
	if err := db.Model(&models.Message{}).
		Distinct("whats_app_message_id").
		Where(
			`organization_id = ?
			 AND inbox_conversation_id = ?
			 AND deleted_at IS NULL
			 AND direction = ?
			 AND whats_app_message_id IN ?
			 AND (
				COALESCE(ingested_at, created_at) < ?
				OR (COALESCE(ingested_at, created_at) = ? AND id <= ?)
			 )`,
			organizationID,
			conversationID,
			models.DirectionIncoming,
			uniqueIDs,
			visibleIngestedAt,
			visibleIngestedAt,
			visibleMessage.ID,
		).
		Pluck("whats_app_message_id", &matchedIDs).Error; err != nil {
		return nil, false, err
	}
	if len(matchedIDs) != len(uniqueIDs) {
		return nil, false, nil
	}
	for _, externalID := range matchedIDs {
		if _, requested := requestedSet[externalID]; !requested {
			return nil, false, nil
		}
	}
	return uniqueIDs, true, nil
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
		if err := a.DB.Select("id", "created_at", "ingested_at").
			Where(
				"id = ? AND organization_id = ? AND inbox_conversation_id = ?",
				beforeID,
				orgID,
				conversationID,
			).
			First(&cursor).Error; err != nil {
			return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "before message was not found", nil, "")
		}
		cursorIngestedAt := cursor.EffectiveIngestedAt()
		query = query.Where(
			`(COALESCE(ingested_at, created_at) < ?
			 OR (COALESCE(ingested_at, created_at) = ? AND id < ?))`,
			cursorIngestedAt,
			cursorIngestedAt,
			cursor.ID,
		)
	}

	var messages []models.Message
	if err := pagination.Apply(query).
		Order("COALESCE(ingested_at, created_at) DESC, id DESC").
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
	outboundSubject := strings.TrimSpace(request.Subject)
	if outboundSubject == "" && conversation.Channel == models.ChannelEmail {
		outboundSubject = strings.TrimSpace(conversation.Subject)
	}
	outbound := channelapi.OutboundMessage{
		OrganizationID: orgID,
		MessageID:      messageID,
		IdempotencyKey: request.IdempotencyKey,
		Purpose:        request.Purpose,
		Conversation: channelapi.ConversationRef{
			ID:         conversation.ID,
			ExternalID: conversation.ExternalConversationID,
			Subject:    outboundSubject,
		},
		Recipient: channelapi.Participant{
			ID:          conversation.ContactID,
			ExternalID:  contactExternalID(conversation),
			Address:     contactAddress(conversation),
			DisplayName: contactDisplayName(conversation),
			Role:        models.ConversationParticipantRoleCustomer,
		},
		CC:                  request.CC,
		Subject:             outboundSubject,
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
		// The tenant organization row is the shared linearization point for
		// channel enqueue and managed-Meta lifecycle mutations. Acquire it before
		// the account row so disconnect/quarantine and enqueue have one stable
		// winner and never invert their lock order.
		if err := lockChannelAIOrganizationScopeTx(tx, orgID); err != nil {
			return err
		}
		enqueueFenceAt := time.Now().UTC()
		if err := lockChannelAccountForMessageEnqueue(
			tx,
			orgID,
			conversation.ChannelAccountID,
			conversation.Channel,
			enqueueFenceAt,
			a,
		); err != nil {
			return err
		}
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
		if errors.Is(err, errChannelAccountUnavailableAtEnqueue) {
			return r.SendErrorEnvelope(
				fasthttp.StatusConflict,
				"Channel account is no longer available for outbound delivery",
				nil,
				"",
			)
		}
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

	accountID := conversation.ChannelAccountID
	contactID := conversation.ContactID
	a.publishRealtimeEvent(queue.RealtimeEvent{
		OrganizationID:   orgID,
		Kind:             queue.RealtimeEventMessageCreated,
		ChannelAccountID: &accountID,
		ConversationID:   &conversationID,
		ContactID:        &contactID,
		MessageID:        &message.ID,
		Status:           string(message.Status),
		OccurredAt:       now,
	}, nil)

	r.RequestCtx.SetStatusCode(fasthttp.StatusAccepted)
	return r.SendEnvelope(map[string]any{
		"message":    message,
		"parts":      parts,
		"outbox_job": outbox,
		"idempotent": false,
	})
}

func lockChannelAccountForMessageEnqueue(
	tx *gorm.DB,
	orgID, accountID uuid.UUID,
	channel models.Channel,
	now time.Time,
	app *App,
) error {
	// Serialize enqueue with account disconnect and credential rotation. If
	// disconnect commits first, this observes the disabled account and creates
	// no message/job. If enqueue commits first, disconnect's cancellation scan
	// necessarily sees the new job.
	var account models.ChannelAccount
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
		Select(
			"id", "organization_id", "channel", "provider", "external_account_id",
			"status", "config", "metadata",
		).
		Where("id = ? AND organization_id = ?", accountID, orgID).
		First(&account).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return errChannelAccountUnavailableAtEnqueue
		}
		return err
	}
	if account.Status != models.ChannelAccountStatusActive ||
		account.Channel != channel ||
		!boolConfigValue(account.Config, "outbound_enabled") {
		return errChannelAccountUnavailableAtEnqueue
	}
	var credentials []models.ChannelCredential
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
		Where(
			"organization_id = ? AND channel_account_id = ? AND status IN ? AND (expires_at IS NULL OR expires_at > ?)",
			orgID,
			account.ID,
			[]models.ChannelCredentialStatus{
				models.ChannelCredentialStatusActive,
				models.ChannelCredentialStatusExpiring,
			},
			now,
		).
		Order("version DESC").
		Order("id ASC").
		Find(&credentials).Error; err != nil {
		return err
	}
	managedMetaIntent := managedMetaControlPlaneIntent(&account)
	if !managedMetaIntent {
		// Legacy/static accounts retain their original one-current-credential
		// contract. Managed Meta is the sole two-credential exception.
		if len(credentials) != 1 {
			return errChannelAccountUnavailableAtEnqueue
		}
		return nil
	}
	if !boolConfigValue(account.Config, "meta_registry_managed") ||
		stringConfigValue(account.Config, "meta_management_mode") !=
			metaregistry.ManagementModePlatformOAuth {
		return errChannelAccountUnavailableAtEnqueue
	}
	oauth := currentMetaRegistryCredential(credentials, models.ChannelCredentialKindOAuth, now)
	webhook := currentMetaRegistryCredential(credentials, models.ChannelCredentialKindWebhook, now)
	if len(credentials) != 2 || oauth == nil || webhook == nil || oauth.ID == webhook.ID {
		return errChannelAccountUnavailableAtEnqueue
	}
	if account.Channel == models.ChannelInstagram &&
		!metaInstagramCredentialPairGenerationValid(oauth, webhook, now) {
		return errChannelAccountUnavailableAtEnqueue
	}
	if stringConfigValue(account.Metadata, "meta_ownership_state") != metaregistry.OwnershipVerified ||
		stringConfigValue(account.Metadata, "meta_deauthorized_at") != "" ||
		stringConfigValue(account.Metadata, metaMessengerSubscriptionDesiredStateKey) !=
			metaMessengerSubscriptionDesiredSubscribed ||
		stringConfigValue(account.Metadata, metaMessengerSubscriptionOperationStateKey) !=
			metaMessengerSubscriptionSubscribeComplete ||
		stringConfigValue(account.Metadata, metaMessengerSubscriptionRemoteStateKey) !=
			metaMessengerSubscriptionRemoteSubscribed {
		return errChannelAccountUnavailableAtEnqueue
	}
	if account.Channel == models.ChannelInstagram {
		if app == nil || app.metaInstagramManagedURLBindingReason(&account) != "" ||
			app.metaInstagramReleaseGuardReason(account, orgID) != "" ||
			!metaInstagramSubscribedOperationMatchesCredentials(account.Metadata, *oauth, *webhook) {
			return errChannelAccountUnavailableAtEnqueue
		}
	}
	return nil
}

// MarkInboxConversationRead owns one short committed tenant phase for the
// local cursor/state transition. Provider I/O is registered as an after-commit
// callback and therefore never runs while that tenant transaction is open.
func (a *App) MarkInboxConversationRead(r *fastglue.Request) error {
	orgID, err := a.getOrgID(r)
	if err != nil || orgID == uuid.Nil {
		return r.SendErrorEnvelope(fasthttp.StatusUnauthorized, "Unauthorized", nil, "")
	}
	var handlerErr error
	var response *markInboxConversationReadResponse
	err = a.WithCommittedTenantApp(orgID, func(scoped *App) error {
		response, handlerErr = scoped.markInboxConversationReadCommitted(r)
		if handlerErr != nil {
			return handlerErr
		}
		if r.RequestCtx.Response.StatusCode() >= fasthttp.StatusBadRequest {
			return errTenantResponseRollback
		}
		return nil
	})
	if errors.Is(err, errTenantResponseRollback) {
		return nil
	}
	if err != nil {
		if handlerErr != nil {
			return handlerErr
		}
		a.Log.Error(
			"Committed conversation read phase failed",
			"error", err,
			"organization_id", orgID,
		)
		return r.SendErrorEnvelope(
			fasthttp.StatusInternalServerError,
			"Failed to mark conversation as read",
			nil,
			"",
		)
	}
	if response == nil {
		return handlerErr
	}
	return r.SendEnvelope(response)
}

func (a *App) markInboxConversationReadCommitted(
	r *fastglue.Request,
) (*markInboxConversationReadResponse, error) {
	orgID, userID, err := a.requireAuth(r, models.ResourceConversations, models.ActionRead)
	if err != nil {
		return nil, nil
	}
	conversationID, err := parsePathUUID(r, "id", "conversation")
	if err != nil {
		return nil, nil
	}
	var request MarkInboxConversationReadRequest
	if len(r.RequestCtx.PostBody()) > 0 {
		if err := a.decodeRequest(r, &request); err != nil {
			return nil, nil
		}
	}

	conversation, err := loadInboxConversation(a.DB, orgID, conversationID, true)
	if err != nil {
		return nil, r.SendErrorEnvelope(fasthttp.StatusNotFound, "Conversation not found", nil, "")
	}

	var visibleMessage *models.Message
	var effectiveRead models.ConversationRead
	var cursorAdvanced bool
	err = a.DB.Transaction(func(tx *gorm.DB) error {
		if err := lockInboxConversationReadOrder(tx, orgID, conversation.ID); err != nil {
			return err
		}
		var target models.Message
		targetQuery := tx.Clauses(clause.Locking{Strength: "SHARE"}).Where(
			"organization_id = ? AND inbox_conversation_id = ?",
			orgID,
			conversation.ID,
		)
		if request.LastVisibleMessageID != nil {
			if *request.LastVisibleMessageID == uuid.Nil {
				return errInboxReadCursorMessageNotFound
			}
			targetQuery = targetQuery.Where("id = ?", *request.LastVisibleMessageID)
		} else {
			targetQuery = targetQuery.Order("COALESCE(ingested_at, created_at) DESC, id DESC")
		}
		if err := targetQuery.First(&target).Error; err == nil {
			visibleMessage = &target
		} else if request.LastVisibleMessageID != nil || !errors.Is(err, gorm.ErrRecordNotFound) {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return errInboxReadCursorMessageNotFound
			}
			return err
		}

		var advanceErr error
		effectiveRead, cursorAdvanced, advanceErr = advanceConversationReadCursor(
			tx,
			orgID,
			userID,
			conversation.ID,
			visibleMessage,
			time.Now().UTC(),
		)
		return advanceErr
	})
	if errors.Is(err, errInboxReadCursorMessageNotFound) {
		return nil, r.SendErrorEnvelope(
			fasthttp.StatusBadRequest,
			"Last visible message was not found in this conversation",
			nil,
			"",
		)
	}
	if err != nil {
		return nil, r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to mark conversation as read", nil, "")
	}
	unreadCount, err := countUnreadInboxMessages(a.DB, orgID, userID, conversation.ID)
	if err != nil {
		return nil, r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to load unread message count", nil, "")
	}
	response := &markInboxConversationReadResponse{
		ReadAt:             effectiveRead.LastReadAt,
		LastReadIngestedAt: effectiveRead.LastReadIngestedAt,
		LastReadMessageID:  effectiveRead.LastReadMessageID,
		UnreadCount:        unreadCount,
	}

	// Queue the local invalidation before any optional shared/provider effects
	// so a slow receipt endpoint cannot delay the user's cross-tab/navbar update
	// after the durable cursor commit.
	if cursorAdvanced {
		a.publishRealtimeEvent(queue.RealtimeEvent{
			OrganizationID: orgID,
			UserID:         &userID,
			Kind:           queue.RealtimeEventConversationChanged,
			ConversationID: &conversationID,
			Status:         "read_cursor_changed",
			OccurredAt:     time.Now().UTC(),
		}, nil)
	}

	if a.HasPermission(userID, models.ResourceConversations, models.ActionWrite, orgID) &&
		conversation.ChannelAccount != nil &&
		conversation.ChannelAccount.Provider == channelapi.LegacyMetaProvider &&
		conversation.Contact != nil {
		// The legacy chat is still the WhatsApp delivery authority. Keep its
		// contact/message read state aligned when an agent opens the mirrored
		// thread in the normalized inbox.
		if visibleMessage != nil {
			verified, verifyErr := a.syncLegacyVisibleMessagesRead(
				orgID,
				userID,
				conversation.ContactID,
				[]models.Message{*visibleMessage},
				false,
			)
			verifiedConversationID, bindingVerified :=
				verified.VerifiedMessageConversations[visibleMessage.ID]
			markErr := verifyErr
			if markErr == nil && (!bindingVerified || verifiedConversationID != conversation.ID) {
				markErr = errors.New("legacy message binding is not verified for this conversation")
			}
			if verifiedMessage, ok := verified.VerifiedMessages[visibleMessage.ID]; ok {
				visibleMessage = &verifiedMessage
			}
			if markErr == nil {
				markErr = a.markMessagesAsReadThrough(
					orgID,
					conversation.ContactID,
					conversation.Contact,
					visibleMessage,
					conversation.ID,
					nil,
				)
			}
			response.LegacyStateSynced = markErr == nil
			if markErr != nil {
				a.Log.Warn(
					"Failed to synchronize legacy message read state",
					"error", markErr,
					"organization_id", orgID,
					"conversation_id", conversation.ID,
				)
			}
		} else {
			// An empty normalized conversation has no exact legacy message that
			// can safely authorize a contact/account-wide acknowledgement.
			response.LegacyStateSynced = true
		}
	}
	if len(request.ExternalMessageIDs) > 0 &&
		a.HasPermission(userID, models.ResourceConversations, models.ActionWrite, orgID) &&
		conversation.ChannelAccount != nil &&
		conversation.ChannelAccount.Status == models.ChannelAccountStatusActive {
		validatedExternalIDs, valid, validationErr := validateInboxReadReceiptMessageIDs(
			a.DB,
			orgID,
			conversation.ID,
			visibleMessage,
			request.ExternalMessageIDs,
		)
		if validationErr != nil {
			a.Log.Warn(
				"Failed to validate conversation read receipts",
				"error", validationErr,
				"organization_id", orgID,
				"conversation_id", conversation.ID,
			)
		}

		if validationErr == nil && valid {
			conversationCopy := *conversation
			accountCopy := *conversation.ChannelAccount
			accountCopy.Credentials = append(
				[]models.ChannelCredential(nil),
				conversation.ChannelAccount.Credentials...,
			)
			conversationCopy.ChannelAccount = &accountCopy
			validatedIDsCopy := append([]string(nil), validatedExternalIDs...)
			response.ProviderSyncQueued = true
			a.afterTenantCommit(func() {
				root := a.rootApp()
				if root == nil {
					return
				}
				var markErr error
				if managedInstagramReadReceiptRequiresFence(conversationCopy.ChannelAccount) {
					response.ProviderSynced, markErr = root.markManagedInstagramConversationRead(
						requestContext(r),
						orgID,
						&conversationCopy,
						validatedIDsCopy,
					)
				} else {
					adapter, adapterErr := root.channelAdapter(conversationCopy.ChannelAccount)
					if adapterErr != nil {
						markErr = adapterErr
					} else if adapter.Capabilities(conversationCopy.ChannelAccount).ReadReceipts {
						markErr = adapter.MarkRead(
							requestContext(r),
							conversationCopy.ChannelAccount,
							channelapi.ConversationRef{
								ID:         conversationCopy.ID,
								ExternalID: conversationCopy.ExternalConversationID,
							},
							validatedIDsCopy,
						)
						response.ProviderSynced = markErr == nil
					}
				}
				if markErr != nil {
					root.Log.Warn(
						"Failed to sync conversation read receipt",
						"error", markErr,
						"organization_id", orgID,
						"conversation_id", conversationCopy.ID,
					)
				}
			})
		}
	}

	return response, nil
}

// advanceConversationReadCursor performs a monotonic cursor upsert. The
// conditional conflict update is evaluated by the database, so a stale or
// concurrently delayed request cannot move an existing cursor backwards.
func advanceConversationReadCursor(
	tx *gorm.DB,
	organizationID, userID, conversationID uuid.UUID,
	visibleMessage *models.Message,
	fallbackReadAt time.Time,
) (models.ConversationRead, bool, error) {
	var effective models.ConversationRead
	if tx == nil || organizationID == uuid.Nil || userID == uuid.Nil || conversationID == uuid.Nil {
		return effective, false, errors.New("conversation read cursor scope is required")
	}
	if err := lockInboxConversationReadOrder(tx, organizationID, conversationID); err != nil {
		return effective, false, err
	}
	// Cursor advancement and hard deletion share a single lock order:
	// Conversation -> target Message -> ConversationRead. Reload the exact
	// visible envelope under FOR SHARE before touching the cursor row. Besides
	// preventing callers from advancing with stale message metadata, this means
	// a delete that already owns Message can finish its cursor cleanup without
	// deadlocking against a reader that owns ConversationRead and is waiting for
	// the same Message's tenant FK check.
	if visibleMessage != nil {
		if visibleMessage.ID == uuid.Nil {
			return effective, false, errInboxReadCursorMessageNotFound
		}
		var lockedVisibleMessage models.Message
		if err := tx.Clauses(clause.Locking{Strength: "SHARE"}).Where(
			"id = ? AND organization_id = ? AND inbox_conversation_id = ?",
			visibleMessage.ID,
			organizationID,
			conversationID,
		).First(&lockedVisibleMessage).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return effective, false, errInboxReadCursorMessageNotFound
			}
			return effective, false, err
		}
		visibleMessage = &lockedVisibleMessage
	}

	candidate := models.ConversationRead{
		OrganizationID: organizationID,
		ConversationID: conversationID,
		UserID:         &userID,
		ReaderKey:      "user:" + userID.String(),
		LastReadAt:     fallbackReadAt.UTC(),
		Metadata:       models.JSONB{},
	}
	if visibleMessage != nil {
		messageID := visibleMessage.ID
		ingestedAt := visibleMessage.EffectiveIngestedAt()
		candidate.LastReadMessageID = &messageID
		candidate.LastReadExternalID = visibleMessage.WhatsAppMessageID
		candidate.LastReadIngestedAt = &ingestedAt
	}
	if candidate.LastReadAt.IsZero() {
		candidate.LastReadAt = time.Now().UTC()
	}
	if candidate.LastReadIngestedAt == nil {
		ingestedAt := candidate.LastReadAt
		candidate.LastReadIngestedAt = &ingestedAt
	}

	result := tx.Clauses(clause.OnConflict{
		Columns: []clause.Column{
			{Name: "organization_id"},
			{Name: "conversation_id"},
			{Name: "reader_key"},
		},
		DoUpdates: clause.AssignmentColumns([]string{
			"user_id",
			"last_read_message_id",
			"last_read_external_id",
			"last_read_ingested_at",
			"last_read_at",
			"metadata",
			"updated_at",
			"deleted_at",
		}),
		Where: clause.Where{Exprs: []clause.Expression{clause.Expr{SQL: `
			COALESCE(conversation_reads.last_read_ingested_at, conversation_reads.last_read_at) <
			COALESCE(excluded.last_read_ingested_at, excluded.last_read_at)
			OR (
				COALESCE(conversation_reads.last_read_ingested_at, conversation_reads.last_read_at) =
				COALESCE(excluded.last_read_ingested_at, excluded.last_read_at)
				AND (
					(
						excluded.last_read_message_id IS NOT NULL
						AND (
							conversation_reads.last_read_message_id IS NULL
							OR conversation_reads.last_read_message_id < excluded.last_read_message_id
						)
					)
					OR (
						conversation_reads.deleted_at IS NOT NULL
						AND (
							conversation_reads.last_read_message_id = excluded.last_read_message_id
							OR (
								conversation_reads.last_read_message_id IS NULL
								AND excluded.last_read_message_id IS NULL
							)
						)
					)
				)
			)`}}},
	}).Create(&candidate)
	if result.Error != nil {
		return effective, false, result.Error
	}
	advanced := result.RowsAffected > 0

	if err := tx.Where(
		"organization_id = ? AND conversation_id = ? AND reader_key = ?",
		organizationID,
		conversationID,
		candidate.ReaderKey,
	).First(&effective).Error; err != nil {
		return effective, false, err
	}
	if !advanced {
		return effective, false, nil
	}

	if err := audit.LogAudit(
		tx,
		organizationID,
		userID,
		audit.GetUserName(tx, userID),
		"conversation_read",
		conversationID,
		models.AuditActionUpdated,
		nil,
		map[string]any{
			"reader_key":            effective.ReaderKey,
			"last_read_message_id":  effective.LastReadMessageID,
			"last_read_ingested_at": effective.LastReadIngestedAt,
			"last_read_at":          effective.LastReadAt,
		},
	); err != nil {
		return effective, false, err
	}
	return effective, true, nil
}

func lockInboxConversationReadOrder(
	tx *gorm.DB,
	organizationID, conversationID uuid.UUID,
) error {
	if tx == nil || organizationID == uuid.Nil || conversationID == uuid.Nil {
		return errors.New("conversation read-order lock scope is required")
	}
	var locked models.InboxConversation
	return tx.Clauses(clause.Locking{Strength: "UPDATE"}).
		Select("id").
		Where("id = ? AND organization_id = ?", conversationID, organizationID).
		First(&locked).Error
}

func managedInstagramReadReceiptRequiresFence(account *models.ChannelAccount) bool {
	if account == nil || account.Channel != models.ChannelInstagram ||
		account.Provider != channelapi.RelayProvider {
		return false
	}
	// Include partially downgraded managed rows in the fenced path. A lifecycle
	// mutation must not be able to escape the late check merely by clearing one
	// of the two control-plane config markers before this request reloads it.
	return managedInstagramControlPlaneIntent(account)
}

// markManagedInstagramConversationRead holds the same organizations.id mutex
// used by lifecycle cancellation from the fresh-account check through the
// relay call. There is no durable read-receipt dispatch row on which to CAS, so
// retaining the mutex across MarkRead is what gives the provider attempt and a
// concurrent lifecycle downgrade a deterministic winner. The local
// ConversationRead has already committed before this optional provider sync.
func (a *App) markManagedInstagramConversationRead(
	ctx context.Context,
	organizationID uuid.UUID,
	conversation *models.InboxConversation,
	externalMessageIDs []string,
) (bool, error) {
	if a == nil || a.DB == nil || conversation == nil ||
		conversation.ChannelAccountID == uuid.Nil || len(externalMessageIDs) == 0 {
		return false, errChannelAccountUnavailableAtEnqueue
	}

	providerSynced := false
	err := database.WithTenantReadCommitted(
		a.rootApp().DB,
		organizationID,
		func(tx *gorm.DB) error {
			// Lock order is organization -> account -> credentials, exactly as
			// enqueue and lifecycle control-plane transactions require.
			if err := lockChannelAIOrganizationScopeTx(tx, organizationID); err != nil {
				return err
			}
			now := time.Now().UTC()
			if err := lockChannelAccountForMessageEnqueue(
				tx,
				organizationID,
				conversation.ChannelAccountID,
				models.ChannelInstagram,
				now,
				a,
			); err != nil {
				return err
			}

			var account models.ChannelAccount
			if err := tx.Where(
				"id = ? AND organization_id = ?",
				conversation.ChannelAccountID,
				organizationID,
			).First(&account).Error; err != nil {
				return err
			}
			if !managedInstagramReadReceiptRequiresFence(&account) {
				return errChannelAccountUnavailableAtEnqueue
			}
			if err := tx.Where(
				"organization_id = ? AND channel_account_id = ? AND status IN ? AND (expires_at IS NULL OR expires_at > ?)",
				organizationID,
				account.ID,
				[]models.ChannelCredentialStatus{
					models.ChannelCredentialStatusActive,
					models.ChannelCredentialStatusExpiring,
				},
				now,
			).
				Order("version DESC").
				Order("id ASC").
				Find(&account.Credentials).Error; err != nil {
				return err
			}

			adapter, err := a.channelAdapter(&account)
			if err != nil {
				return err
			}
			if !adapter.Capabilities(&account).ReadReceipts {
				return nil
			}
			if err := adapter.MarkRead(
				ctx,
				&account,
				channelapi.ConversationRef{
					ID:         conversation.ID,
					ExternalID: conversation.ExternalConversationID,
				},
				externalMessageIDs,
			); err != nil {
				return err
			}
			providerSynced = true
			return nil
		},
	)
	return providerSynced, err
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

func (a *App) inboxConversationToResponse(conversation *models.InboxConversation) InboxConversationResponse {
	response := inboxConversationToResponse(conversation)
	if conversation != nil && conversation.ChannelAccount != nil {
		account := a.channelAccountToResponse(conversation.ChannelAccount)
		response.ChannelAccount = &account
	}
	return response
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
		!strings.EqualFold(conversation.ChannelAccount.Provider, channelapi.ThreadsProvider) {
		return errors.New("threads public replies require an OAuth-managed Threads account")
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
