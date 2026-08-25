package handlers

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	channelapi "github.com/shridarpatil/whatomate/internal/channel"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/valyala/fasthttp"
	"github.com/zerodha/fastglue"
	"gorm.io/gorm"
)

const (
	legacyWhatsAppReplyOperationTimeout  = 30 * time.Second
	legacyWhatsAppReplyMaxRequestBytes   = 64 << 10
	legacyWhatsAppReplyMaxTextCharacters = 4096
)

// LegacyWhatsAppReplyRequest is deliberately text-only. Media, interactive,
// template, and account-selection fields belong to their existing dedicated
// flows and are not accepted by this exact-conversation endpoint.
type LegacyWhatsAppReplyRequest struct {
	IdempotencyKey string             `json:"idempotency_key"`
	Type           models.MessageType `json:"type"`
	Content        struct {
		Body string `json:"body"`
	} `json:"content"`
	ReplyToMessageID string `json:"reply_to_message_id,omitempty"`
}

type legacyWhatsAppReplySnapshot struct {
	userID            uuid.UUID
	conversationID    uuid.UUID
	channelAccountID  uuid.UUID
	whatsAppAccountID uuid.UUID
	account           models.WhatsAppAccount
	contact           models.Contact
	replyToMessage    *models.Message
}

type legacyWhatsAppReplyClientError struct {
	status  int
	message string
}

func (e *legacyWhatsAppReplyClientError) Error() string {
	if e == nil {
		return "legacy WhatsApp reply is unavailable"
	}
	return e.message
}

func newLegacyWhatsAppReplyClientError(status int, message string) error {
	return &legacyWhatsAppReplyClientError{status: status, message: message}
}

// SendLegacyWhatsAppConversationReply sends a text reply through the exact
// established WhatsApp account bound to a meta_legacy inbox conversation.
// The account is server-derived; clients cannot name or override it.
func (a *App) SendLegacyWhatsAppConversationReply(r *fastglue.Request) error {
	// This provider-facing route is deliberately not wrapped by App.Tenant.
	// Resolve an explicit workspace first, then keep every database phase short
	// and committed so no tenant connection remains checked out during Meta I/O.
	orgID, err := a.requireExplicitOrganization(r)
	if err != nil {
		return nil
	}
	conversationID, err := parsePathUUID(r, "id", "conversation")
	if err != nil {
		return nil
	}

	var request LegacyWhatsAppReplyRequest
	if err := decodeLegacyWhatsAppReplyRequest(r, &request); err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid request body", nil, "")
	}
	if utf8.RuneCountInString(request.Content.Body) > legacyWhatsAppReplyMaxTextCharacters {
		return r.SendErrorEnvelope(
			fasthttp.StatusBadRequest,
			"Message text must be at most 4096 characters",
			nil,
			"",
		)
	}
	if strings.ContainsRune(request.Content.Body, '\x00') {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Message text contains an invalid character", nil, "")
	}
	if request.Type != models.MessageTypeText {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Only text replies are supported from this WhatsApp inbox", nil, "")
	}
	if strings.TrimSpace(request.Content.Body) == "" {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Message text is required", nil, "")
	}
	request.IdempotencyKey = strings.TrimSpace(request.IdempotencyKey)
	if request.IdempotencyKey == "" || len(request.IdempotencyKey) > 255 {
		return r.SendErrorEnvelope(
			fasthttp.StatusBadRequest,
			"Idempotency key must be non-empty and at most 255 characters",
			nil,
			"",
		)
	}
	parts := []channelapi.MessagePart{{Type: models.MessagePartTypeText, Text: request.Content.Body}}
	if err := channelapi.ValidateMessagePartsShape(parts); err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, err.Error(), nil, "")
	}

	var snapshot legacyWhatsAppReplySnapshot
	err = a.WithCommittedTenantApp(orgID, func(scoped *App) error {
		resolvedOrgID, userID, authErr := scoped.requireAuth(
			r,
			models.ResourceConversations,
			models.ActionWrite,
		)
		if authErr != nil {
			return authErr
		}
		if resolvedOrgID != orgID {
			_ = r.SendErrorEnvelope(fasthttp.StatusForbidden, "Selected organization is not available", nil, "")
			return errEnvelopeSent
		}
		if !scoped.legacyWhatsAppReplyEnabled(orgID) {
			return newLegacyWhatsAppReplyClientError(
				fasthttp.StatusNotFound,
				"WhatsApp Omnichannel replies are not enabled",
			)
		}
		if !scoped.HasPermission(userID, models.ResourceChat, models.ActionWrite, orgID) {
			return newLegacyWhatsAppReplyClientError(
				fasthttp.StatusForbidden,
				"You don't have permission to send chat messages",
			)
		}

		conversation, loadErr := loadInboxConversation(
			scoped.DB,
			orgID,
			conversationID,
			false,
		)
		if loadErr != nil || conversation.ChannelAccount == nil || conversation.Contact == nil {
			return newLegacyWhatsAppReplyClientError(
				fasthttp.StatusNotFound,
				"Conversation not found",
			)
		}
		shadow := conversation.ChannelAccount
		if conversation.Channel != models.ChannelWhatsApp ||
			shadow.Provider != channelapi.LegacyMetaProvider {
			return newLegacyWhatsAppReplyClientError(
				fasthttp.StatusConflict,
				"Conversation is not backed by an established WhatsApp account",
			)
		}
		if shadow.Status != models.ChannelAccountStatusActive ||
			stringConfigValue(shadow.Config, "reply_route") != "chat" ||
			!boolConfigValue(shadow.Config, "legacy_read_only") ||
			boolConfigValue(shadow.Config, "outbound_enabled") ||
			!boolConfigValue(shadow.Capabilities, "text") ||
			!boolConfigValue(shadow.Capabilities, "replies") ||
			!boolConfigValue(shadow.Capabilities, "service_window") {
			return newLegacyWhatsAppReplyClientError(
				fasthttp.StatusConflict,
				"WhatsApp text replies are not available for this conversation",
			)
		}
		legacyAccountID, bindingErr := channelapi.LegacyMetaWhatsAppAccountID(shadow)
		if bindingErr != nil {
			scoped.Log.Warn(
				"Rejected inconsistent legacy WhatsApp conversation binding",
				"error", bindingErr,
				"conversation_id", conversation.ID,
			)
			return newLegacyWhatsAppReplyClientError(
				fasthttp.StatusConflict,
				"WhatsApp account binding is unavailable",
			)
		}

		// Apply the same contact-visibility rule as the established Chat endpoint.
		var contact models.Contact
		contactQuery := scoped.DB.Where(
			"id = ? AND organization_id = ?",
			conversation.ContactID,
			orgID,
		)
		contactQuery = scoped.scopeAssignedContact(contactQuery, userID, orgID)
		if contactErr := contactQuery.First(&contact).Error; contactErr != nil {
			return newLegacyWhatsAppReplyClientError(
				fasthttp.StatusNotFound,
				"Conversation not found",
			)
		}

		var account models.WhatsAppAccount
		if accountErr := scoped.DB.Where(
			"id = ? AND organization_id = ?",
			legacyAccountID,
			orgID,
		).First(&account).Error; accountErr != nil {
			return newLegacyWhatsAppReplyClientError(
				fasthttp.StatusConflict,
				"WhatsApp account binding is unavailable",
			)
		}
		legacyAccountName, _ := shadow.Metadata["legacy_account_name"].(string)
		if strings.TrimSpace(account.Status) != "active" ||
			strings.TrimSpace(account.Name) == "" ||
			strings.TrimSpace(account.Name) != strings.TrimSpace(legacyAccountName) ||
			strings.TrimSpace(account.PhoneID) == "" {
			return newLegacyWhatsAppReplyClientError(
				fasthttp.StatusConflict,
				"WhatsApp account is not active for replies",
			)
		}
		if credentialErr := scoped.prepareWhatsAppAccountForRuntime(&account); credentialErr != nil {
			scoped.Log.Warn(
				"WhatsApp account is not ready for an omnichannel reply",
				"error", credentialErr,
				"account_id", account.ID,
			)
			return newLegacyWhatsAppReplyClientError(
				fasthttp.StatusConflict,
				"WhatsApp account credentials are unavailable",
			)
		}

		capabilities := channelapi.Capabilities{Text: true, Replies: true, ServiceWindow: true}
		if serviceWindowErr := channelapi.ValidateServiceWindow(
			capabilities,
			conversation.ServiceWindowEndsAt,
			parts,
			time.Now().UTC(),
		); serviceWindowErr != nil {
			return newLegacyWhatsAppReplyClientError(
				fasthttp.StatusConflict,
				serviceWindowErr.Error(),
			)
		}

		var replyToMessage *models.Message
		if strings.TrimSpace(request.ReplyToMessageID) != "" {
			replyID, parseErr := uuid.Parse(strings.TrimSpace(request.ReplyToMessageID))
			if parseErr != nil || replyID == uuid.Nil {
				return newLegacyWhatsAppReplyClientError(
					fasthttp.StatusBadRequest,
					"Invalid reply message ID",
				)
			}
			var reply models.Message
			if replyErr := scoped.DB.Where(
				"id = ? AND organization_id = ? AND contact_id = ? AND inbox_conversation_id = ?",
				replyID,
				orgID,
				contact.ID,
				conversation.ID,
			).First(&reply).Error; replyErr != nil || strings.TrimSpace(reply.WhatsAppMessageID) == "" {
				return newLegacyWhatsAppReplyClientError(
					fasthttp.StatusBadRequest,
					"Reply message is unavailable for this conversation",
				)
			}
			replyToMessage = &reply
		}

		snapshot = legacyWhatsAppReplySnapshot{
			userID:            userID,
			conversationID:    conversation.ID,
			channelAccountID:  shadow.ID,
			whatsAppAccountID: legacyAccountID,
			account:           account,
			contact:           contact,
			replyToMessage:    replyToMessage,
		}
		return nil
	})
	if errors.Is(err, errEnvelopeSent) {
		return nil
	}
	if err != nil {
		var clientErr *legacyWhatsAppReplyClientError
		if errors.As(err, &clientErr) {
			return r.SendErrorEnvelope(clientErr.status, clientErr.message, nil, "")
		}
		a.Log.Error(
			"Failed to prepare exact-account WhatsApp reply",
			"error", err,
			"organization_id", orgID,
			"conversation_id", conversationID,
		)
		return r.SendErrorEnvelope(
			fasthttp.StatusInternalServerError,
			"WhatsApp reply could not be prepared",
			nil,
			"",
		)
	}

	// fasthttp.RequestCtx is not a safe database/sql context when a handler is
	// invoked in-process (its Done method expects an attached server). The reply
	// already has all request-scoped identity above, so own a bounded standard
	// context for the durable claim plus synchronous provider settlement.
	operationContext, cancelOperation := context.WithTimeout(
		context.Background(),
		legacyWhatsAppReplyOperationTimeout,
	)
	defer cancelOperation()
	options := DefaultSendOptions()
	options.SentByUserID = &snapshot.userID
	// The strict route completes its durable provider settlement before the
	// HTTP response. This avoids stranding an accepted pending Message if the
	// replica exits between response write and goroutine scheduling.
	options.Async = false
	payloadDigest := legacyWhatsAppReplyPayloadDigest(
		orgID,
		snapshot.whatsAppAccountID,
		snapshot.conversationID,
		request.Content.Body,
		snapshot.replyToMessage,
	)
	message, err := a.SendOutgoingMessage(operationContext, OutgoingMessageRequest{
		Account:        &snapshot.account,
		Contact:        &snapshot.contact,
		Type:           models.MessageTypeText,
		Content:        request.Content.Body,
		ReplyToMessage: snapshot.replyToMessage,
		legacyWhatsAppReply: &legacyWhatsAppReplyPolicy{
			ConversationID:    snapshot.conversationID,
			ChannelAccountID:  snapshot.channelAccountID,
			WhatsAppAccountID: snapshot.whatsAppAccountID,
			IdempotencyKey:    request.IdempotencyKey,
			PayloadDigest:     payloadDigest,
		},
	}, options)
	if err != nil {
		switch {
		case errors.Is(err, errOutgoingContactNotFound), errors.Is(err, errOutgoingContactAccessRevoked):
			return r.SendErrorEnvelope(fasthttp.StatusNotFound, "Conversation not found", nil, "")
		case errors.Is(err, errOutgoingReplyInvalid), errors.Is(err, gorm.ErrRecordNotFound):
			return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Reply message is unavailable for this conversation", nil, "")
		case errors.Is(err, errLegacyReplyIdempotencyCollision):
			return r.SendErrorEnvelope(fasthttp.StatusConflict, errLegacyReplyIdempotencyCollision.Error(), nil, "")
		case errors.Is(err, errLegacyReplyEntitlementInactive):
			return r.SendErrorEnvelope(fasthttp.StatusForbidden, errLegacyReplyEntitlementInactive.Error(), nil, "")
		case errors.Is(err, errLegacyReplyBindingUnavailable):
			return r.SendErrorEnvelope(fasthttp.StatusConflict, "WhatsApp reply binding is no longer available", nil, "")
		default:
			a.Log.Error("Failed to send exact-account WhatsApp reply", "error", err, "conversation_id", snapshot.conversationID)
			return r.SendErrorEnvelope(fasthttp.StatusBadGateway, "WhatsApp reply could not be sent", nil, "")
		}
	}
	// SendOutgoingMessage deliberately does not mutate the shared in-memory
	// pending value while settling delivery. Return the canonical final row (or
	// the existing row on an idempotent replay) to this synchronous caller.
	var canonical models.Message
	err = a.WithCommittedTenantApp(orgID, func(scoped *App) error {
		return scoped.DB.Where(
			"id = ? AND organization_id = ? AND inbox_conversation_id = ?",
			message.ID,
			orgID,
			snapshot.conversationID,
		).First(&canonical).Error
	})
	if err != nil {
		a.Log.Error("Failed to reload exact-account WhatsApp reply", "error", err, "message_id", message.ID)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "WhatsApp reply result is unavailable", nil, "")
	}

	response := messageResponse(&canonical, snapshot.replyToMessage)
	return r.SendEnvelope(response)
}

func decodeLegacyWhatsAppReplyRequest(
	r *fastglue.Request,
	request *LegacyWhatsAppReplyRequest,
) error {
	if r == nil || request == nil {
		return errors.New("legacy WhatsApp reply request is required")
	}
	body := r.RequestCtx.PostBody()
	if len(body) == 0 || len(body) > legacyWhatsAppReplyMaxRequestBytes {
		return errors.New("legacy WhatsApp reply request body is outside safe bounds")
	}
	if !utf8.Valid(body) {
		return errors.New("legacy WhatsApp reply request body must be valid UTF-8")
	}
	if err := rejectDuplicateLegacyWhatsAppReplyJSONKeys(body); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(request); err != nil {
		return err
	}
	if err := rejectTrailingLegacyWhatsAppReplyJSON(decoder); err != nil {
		return err
	}
	return nil
}

func rejectTrailingLegacyWhatsAppReplyJSON(decoder *json.Decoder) error {
	if decoder == nil {
		return errors.New("JSON decoder is required")
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values are not allowed")
		}
		return err
	}
	return nil
}

func rejectDuplicateLegacyWhatsAppReplyJSONKeys(body []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(body))
	var consumeValue func([]string) error
	consumeValue = func(path []string) error {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		delimiter, composite := token.(json.Delim)
		if !composite {
			if len(path) == 0 {
				return errors.New("legacy WhatsApp reply JSON root must be an object")
			}
			if len(path) == 1 && path[0] == "content" {
				return errors.New("legacy WhatsApp reply content must be an object")
			}
			return nil
		}
		if delimiter != '{' {
			return errors.New("legacy WhatsApp reply JSON arrays are not allowed")
		}
		if !legacyWhatsAppReplyJSONObjectAllowed(path) {
			return errors.New("legacy WhatsApp reply JSON contains an unexpected object")
		}
		switch delimiter {
		case '{':
			seen := make(map[string]struct{})
			for decoder.More() {
				keyToken, keyErr := decoder.Token()
				if keyErr != nil {
					return keyErr
				}
				key, ok := keyToken.(string)
				if !ok {
					return errors.New("JSON object key must be a string")
				}
				if !legacyWhatsAppReplyJSONKeyAllowed(path, key) {
					return fmt.Errorf("unknown JSON key %q", key)
				}
				if _, duplicate := seen[key]; duplicate {
					return fmt.Errorf("duplicate JSON key %q", key)
				}
				seen[key] = struct{}{}
				if valueErr := consumeValue(append(path, key)); valueErr != nil {
					return valueErr
				}
			}
			closing, closeErr := decoder.Token()
			if closeErr != nil || closing != json.Delim('}') {
				if closeErr != nil {
					return closeErr
				}
				return errors.New("invalid JSON object")
			}
		default:
			return errors.New("invalid JSON delimiter")
		}
		return nil
	}
	if err := consumeValue(nil); err != nil {
		return err
	}
	if token, err := decoder.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			return fmt.Errorf("unexpected trailing JSON token %v", token)
		}
		return err
	}
	return nil
}

func legacyWhatsAppReplyJSONObjectAllowed(path []string) bool {
	return len(path) == 0 || (len(path) == 1 && path[0] == "content")
}

func legacyWhatsAppReplyJSONKeyAllowed(path []string, key string) bool {
	if len(path) == 0 {
		switch key {
		case "idempotency_key", "type", "content", "reply_to_message_id":
			return true
		}
		return false
	}
	return len(path) == 1 && path[0] == "content" && key == "body"
}

func (a *App) legacyWhatsAppReplyEnabled(organizationID uuid.UUID) bool {
	root := a.rootApp()
	return root != nil && root.Config != nil &&
		root.Config.LegacyWhatsAppReply.OrganizationEnabled(organizationID.String())
}

func legacyWhatsAppReplyPayloadDigest(
	organizationID, accountID, conversationID uuid.UUID,
	body string,
	replyToMessage *models.Message,
) string {
	replyID := ""
	if replyToMessage != nil {
		replyID = replyToMessage.ID.String()
	}
	payload := organizationID.String() + "\x00" +
		accountID.String() + "\x00" +
		conversationID.String() + "\x00text\x00" +
		body + "\x00" + replyID
	digest := sha256.Sum256([]byte(payload))
	return hex.EncodeToString(digest[:])
}

func messageResponse(message *models.Message, replyToMessage *models.Message) MessageResponse {
	response := MessageResponse{
		ID:                  message.ID,
		ContactID:           message.ContactID,
		Direction:           message.Direction,
		MessageType:         message.MessageType,
		Content:             map[string]string{"body": message.Content},
		MediaURL:            message.MediaURL,
		MediaMimeType:       message.MediaMimeType,
		MediaFilename:       message.MediaFilename,
		InteractiveData:     message.InteractiveData,
		Status:              message.Status,
		WAMID:               message.WhatsAppMessageID,
		Error:               message.ErrorMessage,
		IsReply:             message.IsReply,
		WhatsAppAccount:     message.WhatsAppAccount,
		InboxConversationID: message.InboxConversationID,
		IngestedAt:          message.IngestedAt,
		CreatedAt:           message.CreatedAt,
		UpdatedAt:           message.UpdatedAt,
	}
	if message.IsReply && message.ReplyToMessageID != nil && replyToMessage != nil {
		replyToID := message.ReplyToMessageID.String()
		response.ReplyToMessageID = &replyToID
		response.ReplyToMessage = &ReplyPreview{
			ID:          replyToMessage.ID.String(),
			Content:     map[string]string{"body": replyToMessage.Content},
			MessageType: replyToMessage.MessageType,
			Direction:   replyToMessage.Direction,
		}
	}
	return response
}
