package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/contactutil"
	"github.com/shridarpatil/whatomate/internal/database"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/internal/queue"
	"github.com/shridarpatil/whatomate/internal/templateutil"
	"github.com/shridarpatil/whatomate/internal/utils"
	"github.com/shridarpatil/whatomate/internal/websocket"
	"github.com/shridarpatil/whatomate/pkg/whatsapp"
	"github.com/valyala/fasthttp"
	"github.com/zerodha/fastglue"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// ============================================================================
// Unified Message Sending
// ============================================================================

// A provider attempt may consume or cancel the caller's deadline. Settlement
// recovery owns a fresh bounded context and never invokes the provider again.
const outgoingDeliveryRecoveryTimeout = 10 * time.Second

// OutgoingMessageRequest contains all parameters for sending any type of message
type OutgoingMessageRequest struct {
	// Required
	Account *models.WhatsAppAccount
	Contact *models.Contact

	// Message type determines which fields are used
	Type models.MessageType // text, image, video, audio, document, interactive, template

	// Text messages
	Content string

	// Media messages (image, video, audio, document)
	MediaID       string // WhatsApp media ID (if already uploaded)
	MediaData     []byte // Raw media data (if upload needed)
	MediaURL      string // Local media URL (for storage)
	MediaMimeType string
	MediaFilename string
	Caption       string

	// Interactive messages
	InteractiveType string            // "button", "list", "cta_url", "voice_call"
	BodyText        string            // Body text for interactive messages
	Buttons         []whatsapp.Button // For button/list messages
	ButtonText      string            // For CTA URL button
	URL             string            // For CTA URL button

	// voice_call interactive (WhatsApp Business Calling)
	DisplayText      string // Button face label
	TTLMinutes       int    // How long the button stays clickable; 0 ⇒ Meta default
	VoiceCallPayload string // Opaque round-trip string; server-set, e.g. "agent:<uuid>" for sticky routing

	// Template messages
	Template            *models.Template
	BodyParams          map[string]string // Parameter name -> value (supports both named and positional)
	HeaderParams        map[string]string // Header-only param values; falls back to BodyParams if empty (used for TEXT headers with a {{var}})
	HeaderMediaID       string            // WhatsApp media ID for template header (IMAGE/VIDEO/DOCUMENT)
	HeaderMediaFilename string            // Filename — required by Meta for DOCUMENT headers
	ButtonURLParams     map[string]string // Button index (as string) -> dynamic URL param value

	// WhatsApp Flow messages
	FlowID          string // Meta Flow ID
	FlowHeader      string // Optional header text for flow
	FlowCTA         string // CTA button text (max 20 chars)
	FlowToken       string // Unique token for flow response tracking
	FlowFirstScreen string // First screen name to navigate to

	// Reply context
	ReplyToMessage *models.Message

	// legacyWhatsAppReply is populated only by the fail-closed, exact
	// Omnichannel reply endpoint. Established Chat callers cannot opt into or
	// influence this server-derived binding.
	legacyWhatsAppReply *legacyWhatsAppReplyPolicy

	// deliveryOverride is an internal deterministic test seam for exercising
	// post-provider transaction failures. Public handlers never populate it.
	deliveryOverride func(context.Context, *models.Contact) (string, error)
}

// MessageSendOptions configures optional behaviors for message sending
type MessageSendOptions struct {
	// BroadcastWebSocket enables WebSocket broadcast to org (default: true)
	BroadcastWebSocket bool

	// DispatchWebhook enables webhook dispatch for message.sent event (default: true)
	DispatchWebhook bool

	// TrackSLA enables SLA tracking for chatbot messages (default: false)
	TrackSLA bool

	// SentByUserID sets the user who sent the message (for agent messages)
	SentByUserID *uuid.UUID

	// Async if true, sends in background goroutine and returns immediately
	// Message is persisted before send, status updated after
	Async bool

	// MarkIncomingRead marks the contact's incoming messages as read after a
	// successful send. Used for chatbot replies so a bot-handled exchange
	// doesn't leave an "unread" badge in the agent's contact list.
	MarkIncomingRead bool
}

var (
	errOutgoingContactNotFound      = errors.New("outgoing message contact not found")
	errOutgoingContactAccessRevoked = errors.New("outgoing message contact access revoked")
	errOutgoingReplyInvalid         = errors.New("outgoing reply message is invalid")
)

// OutgoingConsentError identifies an outbound message rejected by the
// canonical contact's current consent state. Callers can use errors.As to map
// this policy decision to a client error without string matching.
type OutgoingConsentError struct {
	ContactID uuid.UUID
}

func (e *OutgoingConsentError) Error() string {
	return "contact has opted out of marketing messages"
}

// DefaultSendOptions returns options suitable for agent UI sends
func DefaultSendOptions() MessageSendOptions {
	return MessageSendOptions{
		BroadcastWebSocket: true,
		DispatchWebhook:    true,
		TrackSLA:           false,
		Async:              true,
	}
}

// ChatbotSendOptions returns options suitable for chatbot sends
func ChatbotSendOptions() MessageSendOptions {
	return MessageSendOptions{
		BroadcastWebSocket: true,
		DispatchWebhook:    false,
		TrackSLA:           true,
		Async:              false,
		MarkIncomingRead:   true,
	}
}

// APISendOptions returns options suitable for API/template sends
func APISendOptions() MessageSendOptions {
	return MessageSendOptions{
		BroadcastWebSocket: false,
		DispatchWebhook:    true,
		TrackSLA:           false,
		Async:              true,
	}
}

// SLASendOptions returns options suitable for SLA system notifications
func SLASendOptions() MessageSendOptions {
	return MessageSendOptions{
		BroadcastWebSocket: true,
		DispatchWebhook:    false,
		TrackSLA:           false,
		Async:              false, // Sync to ensure message is sent before continuing
	}
}

// SendOutgoingMessage is the unified method for sending all types of WhatsApp messages.
// It handles: text, media (image/video/audio/document), interactive (buttons/list/cta_url), and template messages.
func (a *App) SendOutgoingMessage(ctx context.Context, req OutgoingMessageRequest, opts MessageSendOptions) (*models.Message, error) {
	if req.Account == nil || req.Account.OrganizationID == uuid.Nil {
		return nil, errors.New("WhatsApp account is required")
	}
	if req.Contact == nil || req.Contact.ID == uuid.Nil {
		return nil, fmt.Errorf("%w: contact is required", errOutgoingContactNotFound)
	}
	if ctx == nil {
		ctx = context.Background()
	}

	organizationID := req.Account.OrganizationID
	requestedContactID := req.Contact.ID
	preview := a.getMessagePreview(req)
	var (
		msg              *models.Message
		canonicalContact models.Contact
		validatedReply   *models.Message
	)

	// Message persistence and the contact-list preview are one fact. Resolve
	// and lock the canonical contact in the same transaction so a concurrent
	// merge cannot strand either write on a soft-deleted alias.
	err := a.outgoingPreProviderTransaction(ctx, organizationID, opts.Async, func(tx *gorm.DB) error {
		if req.legacyWhatsAppReply != nil {
			// The legacy bridge owns ChannelAccount before Conversation. Establish
			// that prefix before resolving (and locking) Contact.
			if lockErr := lockStrictLegacyReplyOrder(
				tx,
				organizationID,
				req.legacyWhatsAppReply,
			); lockErr != nil {
				return lockErr
			}
		}
		contact, resolveErr := contactutil.ResolveCanonicalContactForUpdate(
			tx,
			organizationID,
			requestedContactID,
		)
		if resolveErr != nil {
			if errors.Is(resolveErr, gorm.ErrRecordNotFound) {
				return fmt.Errorf("%w: %v", errOutgoingContactNotFound, resolveErr)
			}
			return resolveErr
		}

		if req.Type == models.MessageTypeTemplate &&
			req.Template != nil &&
			strings.EqualFold(req.Template.Category, "MARKETING") &&
			contact.MarketingOptOut {
			return &OutgoingConsentError{ContactID: contact.ID}
		}

		if opts.SentByUserID != nil {
			if visibilityErr := a.lockOutgoingContactVisibility(
				tx,
				organizationID,
				*opts.SentByUserID,
				contact,
			); visibilityErr != nil {
				return visibilityErr
			}
		}

		if req.legacyWhatsAppReply != nil {
			existing, claimErr := claimLegacyReplyIdempotency(
				tx,
				organizationID,
				req.legacyWhatsAppReply,
			)
			if claimErr != nil {
				return claimErr
			}
			if existing != nil {
				return &outgoingIdempotentReplay{
					message: *existing,
					contact: *contact,
				}
			}
			if _, policyErr := a.validateLegacyReplyPolicyTx(
				tx,
				&req,
				contact.ID,
				true,
			); policyErr != nil {
				return policyErr
			}
		}

		transactionalReq := req
		transactionalReq.Contact = contact
		validatedReply = nil
		if req.ReplyToMessage != nil {
			if req.ReplyToMessage.ID == uuid.Nil {
				return fmt.Errorf("%w: reply message ID is required", errOutgoingReplyInvalid)
			}
			var reply models.Message
			replyQuery := tx.Where(
				"id = ? AND organization_id = ? AND contact_id = ?",
				req.ReplyToMessage.ID,
				organizationID,
				contact.ID,
			)
			if req.legacyWhatsAppReply != nil {
				replyQuery = replyQuery.Where(
					"inbox_conversation_id = ? AND BTRIM(whats_app_message_id) <> ''",
					req.legacyWhatsAppReply.ConversationID,
				)
			}
			replyErr := replyQuery.First(&reply).Error
			if replyErr != nil {
				if errors.Is(replyErr, gorm.ErrRecordNotFound) {
					return fmt.Errorf(
						"%w: reply message does not belong to this contact",
						errOutgoingReplyInvalid,
					)
				}
				return replyErr
			}
			validatedReply = &reply
			transactionalReq.ReplyToMessage = validatedReply
		}

		candidate := a.createOutgoingMessage(transactionalReq, opts)
		if req.legacyWhatsAppReply != nil {
			conversationID := req.legacyWhatsAppReply.ConversationID
			candidate.InboxConversationID = &conversationID
			if candidate.Metadata == nil {
				candidate.Metadata = models.JSONB{}
			}
			candidate.Metadata[legacyReplyIdempotencyMetadataKey] = req.legacyWhatsAppReply.IdempotencyKey
			candidate.Metadata[legacyReplyPayloadDigestKey] = req.legacyWhatsAppReply.PayloadDigest
			candidate.Metadata[legacyReplyAccountMetadataKey] = req.legacyWhatsAppReply.WhatsAppAccountID.String()
			candidate.Metadata[legacyReplyConversationMetadataKey] = conversationID.String()
			candidate.Metadata["send_surface"] = "omnichannel_legacy_whatsapp"
		}
		if createErr := tx.Create(candidate).Error; createErr != nil {
			return fmt.Errorf("failed to create message: %w", createErr)
		}
		if req.legacyWhatsAppReply != nil {
			if mirrorErr := persistStrictLegacyReplyMirror(tx, &req, candidate); mirrorErr != nil {
				return mirrorErr
			}
		}

		lastMessageAt := time.Now().UTC()
		updateResult := tx.Model(&models.Contact{}).
			Where(
				"id = ? AND organization_id = ? AND merged_into_id IS NULL AND deleted_at IS NULL",
				contact.ID,
				organizationID,
			).
			Updates(map[string]any{
				"last_message_at":      lastMessageAt,
				"last_message_preview": preview,
			})
		if updateResult.Error != nil {
			return updateResult.Error
		}
		if updateResult.RowsAffected != 1 {
			return fmt.Errorf("%w: canonical contact changed before persistence", errOutgoingContactNotFound)
		}

		contact.LastMessageAt = &lastMessageAt
		contact.LastMessagePreview = preview
		msg = candidate
		canonicalContact = *contact
		return nil
	})
	if err != nil {
		var replay *outgoingIdempotentReplay
		if errors.As(err, &replay) {
			// A lost-response retry is an at-most-once replay: never invoke Meta,
			// but do rebroadcast the canonical row so every live client sees its
			// durable sent, failed, or provider-ambiguous pending state.
			if opts.BroadcastWebSocket {
				a.broadcastNewMessage(organizationID, &replay.message, &replay.contact)
			}
			return &replay.message, nil
		}
		a.Log.Error(
			"Failed to persist outgoing message",
			"error", err,
			"organization_id", organizationID,
			"contact_id", requestedContactID,
		)
		return nil, err
	}

	req.Contact = &canonicalContact
	req.ReplyToMessage = validatedReply
	if req.legacyWhatsAppReply == nil {
		a.mirrorLegacyWhatsAppMessageAfterCommit(req.Account, msg.ID)
	}

	// 2. Define the send function based on message type
	sendFn := func(sendCtx context.Context, deliveryContact *models.Contact) (string, error) {
		if req.deliveryOverride != nil {
			return req.deliveryOverride(sendCtx, deliveryContact)
		}
		waAccount := a.toWhatsAppAccount(req.Account)
		rcpt := whatsapp.Recipient{Phone: deliveryContact.PhoneNumber, BSUID: deliveryContact.BSUID}

		// Get reply-to message ID if this is a reply
		var replyToMsgID string
		if req.ReplyToMessage != nil && req.ReplyToMessage.WhatsAppMessageID != "" {
			replyToMsgID = req.ReplyToMessage.WhatsAppMessageID
		}

		switch req.Type {
		case models.MessageTypeText:
			return a.WhatsApp.SendTextMessage(sendCtx, waAccount, rcpt, req.Content, replyToMsgID)

		case models.MessageTypeImage, models.MessageTypeVideo, models.MessageTypeAudio, models.MessageTypeDocument:
			// Upload media if MediaData is provided and MediaID is not set
			mediaID := req.MediaID
			if mediaID == "" && len(req.MediaData) > 0 {
				var err error
				mediaID, err = a.WhatsApp.UploadMedia(sendCtx, waAccount, req.MediaData, req.MediaMimeType, req.MediaFilename)
				if err != nil {
					return "", fmt.Errorf("failed to upload media: %w", err)
				}
			}
			// Send the appropriate media type
			switch req.Type {
			case models.MessageTypeImage:
				return a.WhatsApp.SendImageMessage(sendCtx, waAccount, rcpt, mediaID, req.Caption)
			case models.MessageTypeVideo:
				return a.WhatsApp.SendVideoMessage(sendCtx, waAccount, rcpt, mediaID, req.Caption)
			case models.MessageTypeAudio:
				return a.WhatsApp.SendAudioMessage(sendCtx, waAccount, rcpt, mediaID)
			default: // document
				return a.WhatsApp.SendDocumentMessage(sendCtx, waAccount, rcpt, mediaID, req.MediaFilename, req.Caption)
			}

		case models.MessageTypeInteractive:
			switch req.InteractiveType {
			case "cta_url":
				return a.WhatsApp.SendCTAURLButton(sendCtx, waAccount, rcpt, req.BodyText, req.ButtonText, req.URL)
			case "voice_call":
				return a.WhatsApp.SendVoiceCallButton(sendCtx, waAccount, rcpt, req.BodyText, req.DisplayText, req.TTLMinutes, req.VoiceCallPayload)
			default: // "button" or "list"
				return a.WhatsApp.SendInteractiveButtons(sendCtx, waAccount, rcpt, req.BodyText, req.Buttons)
			}

		case models.MessageTypeTemplate:
			if req.Template == nil {
				return "", fmt.Errorf("template is required for template messages")
			}
			components, err := whatsapp.BuildTemplateComponents(
				req.BodyParams,
				req.Template.HeaderType, req.Template.HeaderContent,
				req.HeaderParams,
				req.HeaderMediaID, req.HeaderMediaFilename,
			)
			if err != nil {
				return "", fmt.Errorf("failed to build template components: %w", err)
			}
			// Add auto-generated button components (Flow needs flow_token)
			flowComponents := whatsapp.AutoButtonComponents(req.Template.Buttons)
			components = append(components, flowComponents...)
			// Add URL/COPY_CODE button components with dynamic params
			buttonComponents := whatsapp.ButtonURLParamsToComponents(req.ButtonURLParams, req.Template.Buttons)
			components = append(components, buttonComponents...)
			return a.WhatsApp.SendTemplateMessage(sendCtx, waAccount, rcpt, req.Template.Name, req.Template.Language, components)

		case models.MessageTypeFlow:
			if req.FlowID == "" {
				return "", fmt.Errorf("flow ID is required for flow messages")
			}
			return a.WhatsApp.SendFlowMessage(sendCtx, waAccount, rcpt, req.FlowID, req.FlowHeader, req.BodyText, req.FlowCTA, req.FlowToken, req.FlowFirstScreen)

		default:
			return "", fmt.Errorf("unsupported message type: %s", req.Type)
		}
	}

	// 3. Execute send (async or sync). An async send entered through Tenant
	// must not start its provider phase until the request transaction commits:
	// the pending row and its new_message event are the durable/UI boundary.
	var startAsyncDelivery func()
	if opts.Async {
		root := a.rootApp()
		startAsyncDelivery = func() {
			root.wg.Add(1)
			go func() {
				defer root.wg.Done()
				asyncCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()

				delivery, deliveryErr := root.deliverOutgoingMessage(
					asyncCtx,
					msg,
					req,
					opts,
					sendFn,
				)
				deliveryReq := req
				if delivery.contact.ID != uuid.Nil {
					deliveryReq.Contact = &delivery.contact
				}
				if deliveryErr != nil {
					if delivery.providerAttempted {
						root.Log.Error(
							"Provider delivery completed but its database result is unresolved; automatic resend is disabled",
							"error", deliveryErr,
							"organization_id", organizationID,
							"message_id", msg.ID,
						)
						return
					}
					if settlementErr := root.settleOutgoingFailureBeforeProvider(
						msg,
						deliveryReq,
						opts,
						deliveryErr,
					); settlementErr != nil {
						root.Log.Error(
							"Failed to settle outgoing message before provider attempt",
							"error", settlementErr,
							"delivery_error", deliveryErr,
							"organization_id", organizationID,
							"message_id", msg.ID,
						)
					}
					return
				}
				finalErr := delivery.sendErr
				if delivery.policyErr != nil {
					finalErr = delivery.policyErr
				}
				root.finalizeMessageSend(
					msg,
					deliveryReq,
					opts,
					delivery.whatsAppMessageID,
					finalErr,
					false,
				)
			}()
		}
	} else {
		delivery, deliveryErr := a.deliverOutgoingMessage(ctx, msg, req, opts, sendFn)
		if delivery.contact.ID != uuid.Nil {
			req.Contact = &delivery.contact
		}
		if deliveryErr != nil {
			if delivery.providerAttempted {
				a.Log.Error(
					"Provider delivery completed but its database result is unresolved; automatic resend is disabled",
					"error", deliveryErr,
					"organization_id", organizationID,
					"message_id", msg.ID,
				)
				return msg, nil
			}
			if settlementErr := a.settleOutgoingFailureBeforeProvider(
				msg,
				req,
				opts,
				deliveryErr,
			); settlementErr != nil {
				return nil, fmt.Errorf(
					"settle outgoing message before provider attempt: %w (delivery error: %v)",
					settlementErr,
					deliveryErr,
				)
			}
			return nil, deliveryErr
		}
		finalErr := delivery.sendErr
		if delivery.policyErr != nil {
			finalErr = delivery.policyErr
		}
		// Synchronous callers broadcast and return the settled envelope. Without
		// this copy, a final status_update was followed by a stale pending
		// new_message from the original in-memory value.
		msg.WhatsAppMessageID = delivery.whatsAppMessageID
		if finalErr != nil {
			msg.Status = models.MessageStatusFailed
			msg.ErrorMessage = finalErr.Error()
		} else {
			msg.Status = models.MessageStatusSent
			msg.ErrorMessage = ""
		}
		a.finalizeMessageSend(
			msg,
			req,
			opts,
			delivery.whatsAppMessageID,
			finalErr,
			false,
		)
		if delivery.policyErr != nil {
			return nil, delivery.policyErr
		}
	}

	// 4. Immediate actions (before send completes for async)
	if opts.BroadcastWebSocket {
		a.broadcastNewMessage(req.Account.OrganizationID, msg, req.Contact)
	}

	if opts.TrackSLA {
		a.UpdateContactChatbotMessage(req.Contact.ID)
	}
	if startAsyncDelivery != nil {
		a.afterTenantCommit(startAsyncDelivery)
	}

	return msg, nil
}

type outgoingDeliveryResult struct {
	contact           models.Contact
	whatsAppMessageID string
	sendErr           error
	policyErr         error
	providerAttempted bool
}

// deliverOutgoingMessage linearizes the final policy decision and provider
// attempt against contact merges and consent updates. The pending Message was
// durably created before this call. If persistence fails after the provider
// attempt, recovery records the captured result without calling Meta again.
func (a *App) deliverOutgoingMessage(
	ctx context.Context,
	msg *models.Message,
	req OutgoingMessageRequest,
	opts MessageSendOptions,
	sendFn func(context.Context, *models.Contact) (string, error),
) (outgoingDeliveryResult, error) {
	var result outgoingDeliveryResult
	if msg == nil || msg.ID == uuid.Nil {
		return result, errors.New("pending outgoing message is required")
	}

	err := a.outgoingProviderTransaction(
		ctx,
		req.Account.OrganizationID,
		func(tx *gorm.DB, providerAttempted *bool) error {
			if req.legacyWhatsAppReply != nil {
				if lockErr := lockStrictLegacyReplyOrder(
					tx,
					req.Account.OrganizationID,
					req.legacyWhatsAppReply,
				); lockErr != nil {
					return lockErr
				}
			}
			contact, resolveErr := contactutil.ResolveCanonicalContactForUpdate(
				tx,
				req.Account.OrganizationID,
				req.Contact.ID,
			)
			if resolveErr != nil {
				if errors.Is(resolveErr, gorm.ErrRecordNotFound) {
					return fmt.Errorf("%w: %v", errOutgoingContactNotFound, resolveErr)
				}
				return resolveErr
			}
			result.contact = *contact

			var stored models.Message
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
				Where(
					"id = ? AND organization_id = ?",
					msg.ID,
					req.Account.OrganizationID,
				).
				First(&stored).Error; err != nil {
				return fmt.Errorf("lock pending outgoing message: %w", err)
			}
			if stored.Status != models.MessageStatusPending {
				result.whatsAppMessageID = stored.WhatsAppMessageID
				if stored.Status == models.MessageStatusFailed && stored.ErrorMessage != "" {
					result.sendErr = errors.New(stored.ErrorMessage)
				}
				return nil
			}
			if req.legacyWhatsAppReply != nil {
				if !legacyReplyMessageMatchesPolicy(&stored, req.legacyWhatsAppReply) {
					policyErr := fmt.Errorf(
						"%w: pending message binding changed",
						errLegacyReplyBindingUnavailable,
					)
					result.policyErr = policyErr
					return persistOutgoingDeliveryResult(tx, &stored, contact.ID, "", policyErr)
				}
				if _, policyErr := a.validateLegacyReplyPolicyTx(
					tx,
					&req,
					contact.ID,
					false,
				); policyErr != nil {
					result.policyErr = policyErr
					return persistOutgoingDeliveryResult(tx, &stored, contact.ID, "", policyErr)
				}
			}

			if opts.SentByUserID != nil {
				if visibilityErr := a.lockOutgoingContactVisibility(
					tx,
					req.Account.OrganizationID,
					*opts.SentByUserID,
					contact,
				); visibilityErr != nil {
					result.policyErr = visibilityErr
					return persistOutgoingDeliveryResult(
						tx,
						&stored,
						contact.ID,
						"",
						visibilityErr,
					)
				}
			}
			if req.Type == models.MessageTypeTemplate &&
				req.Template != nil &&
				strings.EqualFold(req.Template.Category, "MARKETING") &&
				contact.MarketingOptOut {
				consentErr := &OutgoingConsentError{ContactID: contact.ID}
				result.policyErr = consentErr
				return persistOutgoingDeliveryResult(
					tx,
					&stored,
					contact.ID,
					"",
					consentErr,
				)
			}

			*providerAttempted = true
			result.providerAttempted = true
			result.whatsAppMessageID, result.sendErr = sendFn(ctx, contact)
			return persistOutgoingDeliveryResult(
				tx,
				&stored,
				contact.ID,
				result.whatsAppMessageID,
				result.sendErr,
			)
		},
	)
	if err == nil || !result.providerAttempted {
		return result, err
	}

	recoveryContext, cancelRecovery := context.WithTimeout(
		context.Background(),
		outgoingDeliveryRecoveryTimeout,
	)
	defer cancelRecovery()
	if recoveryErr := a.recoverOutgoingDeliveryResult(recoveryContext, msg, &result); recoveryErr != nil {
		return result, fmt.Errorf(
			"persist provider-attempt result after transaction failure: %w (original error: %v)",
			recoveryErr,
			err,
		)
	}
	return result, nil
}

func persistOutgoingDeliveryResult(
	tx *gorm.DB,
	message *models.Message,
	contactID uuid.UUID,
	whatsAppMessageID string,
	sendErr error,
) error {
	status := models.MessageStatusSent
	errorMessage := ""
	if sendErr != nil {
		status = models.MessageStatusFailed
		errorMessage = sendErr.Error()
	}
	update := tx.Model(&models.Message{}).
		Where(
			"id = ? AND organization_id = ? AND status = ?",
			message.ID,
			message.OrganizationID,
			models.MessageStatusPending,
		).
		Updates(map[string]any{
			"contact_id":           contactID,
			"status":               status,
			"whats_app_message_id": whatsAppMessageID,
			"error_message":        errorMessage,
		})
	if update.Error != nil {
		return fmt.Errorf("persist outgoing delivery result: %w", update.Error)
	}
	if update.RowsAffected != 1 {
		return errors.New("pending outgoing message changed before delivery finalization")
	}
	return nil
}

func (a *App) recoverOutgoingDeliveryResult(
	ctx context.Context,
	pendingMessage *models.Message,
	result *outgoingDeliveryResult,
) error {
	return a.outgoingTenantTransaction(
		ctx,
		pendingMessage.OrganizationID,
		func(tx *gorm.DB) error {
			var message models.Message
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
				Where(
					"id = ? AND organization_id = ?",
					pendingMessage.ID,
					pendingMessage.OrganizationID,
				).
				First(&message).Error; err != nil {
				return err
			}
			if message.Status != models.MessageStatusPending {
				result.whatsAppMessageID = message.WhatsAppMessageID
				if message.Status == models.MessageStatusFailed && message.ErrorMessage != "" {
					result.sendErr = errors.New(message.ErrorMessage)
				}
				return nil
			}
			return persistOutgoingDeliveryResult(
				tx,
				&message,
				result.contact.ID,
				result.whatsAppMessageID,
				result.sendErr,
			)
		},
	)
}

// settleOutgoingFailureBeforeProvider owns the known-no-provider failure
// boundary. The failed delivery transaction may have never acquired a tenant
// connection or may have rolled back before its first query, so settlement
// must start from the root pool with a fresh bounded context. Realtime is
// emitted only after this transaction returns successfully (and therefore
// after the failed state is committed).
func (a *App) settleOutgoingFailureBeforeProvider(
	pendingMessage *models.Message,
	req OutgoingMessageRequest,
	opts MessageSendOptions,
	deliveryErr error,
) error {
	if a == nil || pendingMessage == nil || pendingMessage.ID == uuid.Nil ||
		req.Account == nil || req.Account.OrganizationID == uuid.Nil ||
		deliveryErr == nil {
		return errors.New("pre-provider failure settlement requires an exact message, account, and error")
	}
	organizationID := req.Account.OrganizationID
	if pendingMessage.OrganizationID != organizationID {
		return errors.New("pre-provider failure settlement organization does not match pending message")
	}

	settlementContext, cancelSettlement := context.WithTimeout(
		context.Background(),
		outgoingDeliveryRecoveryTimeout,
	)
	defer cancelSettlement()

	root := a.rootApp()
	var canonical models.Message
	err := root.outgoingTenantTransaction(
		settlementContext,
		organizationID,
		func(tx *gorm.DB) error {
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where(
				"id = ? AND organization_id = ? AND direction = ?",
				pendingMessage.ID,
				organizationID,
				models.DirectionOutgoing,
			).First(&canonical).Error; err != nil {
				return fmt.Errorf("lock pre-provider failure message: %w", err)
			}
			if req.legacyWhatsAppReply != nil &&
				!legacyReplyMessageMatchesPolicy(&canonical, req.legacyWhatsAppReply) {
				return fmt.Errorf(
					"%w: pre-provider failure message binding changed",
					errLegacyReplyBindingUnavailable,
				)
			}

			switch canonical.Status {
			case models.MessageStatusPending:
				update := tx.Model(&models.Message{}).Where(
					"id = ? AND organization_id = ? AND status = ?",
					canonical.ID,
					organizationID,
					models.MessageStatusPending,
				).Updates(map[string]any{
					"status":        models.MessageStatusFailed,
					"error_message": deliveryErr.Error(),
				})
				if update.Error != nil {
					return fmt.Errorf("persist pre-provider failure: %w", update.Error)
				}
				if update.RowsAffected != 1 {
					return errors.New("pending message changed before pre-provider failure settlement")
				}
				if err := tx.Where(
					"id = ? AND organization_id = ?",
					canonical.ID,
					organizationID,
				).First(&canonical).Error; err != nil {
					return fmt.Errorf("reload pre-provider failure message: %w", err)
				}
			case models.MessageStatusFailed:
				if strings.TrimSpace(canonical.ErrorMessage) == "" {
					return errors.New("canonical failed message is missing its failure reason")
				}
			default:
				return fmt.Errorf(
					"pre-provider failure message is already in terminal status %s",
					canonical.Status,
				)
			}
			return nil
		},
	)
	if err != nil {
		return err
	}

	terminalErr := errors.New(canonical.ErrorMessage)
	deliveryReq := req
	deliveryReq.Contact = &models.Contact{
		BaseModel: models.BaseModel{ID: canonical.ContactID},
	}
	root.finalizeMessageSend(
		&canonical,
		deliveryReq,
		opts,
		canonical.WhatsAppMessageID,
		terminalErr,
		false,
	)
	return nil
}

func (a *App) outgoingProviderTransaction(
	ctx context.Context,
	organizationID uuid.UUID,
	deliver func(tx *gorm.DB, providerAttempted *bool) error,
) error {
	var err error
	for attempt := 0; attempt < canonicalContactWriteAttempts; attempt++ {
		providerAttempted := false
		err = a.outgoingTenantTransaction(ctx, organizationID, func(tx *gorm.DB) error {
			return deliver(tx, &providerAttempted)
		})
		if err == nil || providerAttempted || !isRetryableCanonicalContactWrite(err) {
			return err
		}
	}
	return err
}

func (a *App) outgoingCanonicalTransaction(
	ctx context.Context,
	organizationID uuid.UUID,
	write func(tx *gorm.DB) error,
) error {
	var err error
	for attempt := 0; attempt < canonicalContactWriteAttempts; attempt++ {
		err = a.outgoingTenantTransaction(ctx, organizationID, write)
		if !isRetryableCanonicalContactWrite(err) {
			return err
		}
	}
	return err
}

// outgoingPreProviderTransaction keeps an asynchronous request's pending
// state inside an already-open RLS Tenant transaction. Starting a second root
// transaction while that request owns the pool's only connection would
// self-deadlock. A nested transaction gives retryable canonical-contact writes
// a savepoint, while the provider phase is scheduled separately after the
// outer request commits.
func (a *App) outgoingPreProviderTransaction(
	ctx context.Context,
	organizationID uuid.UUID,
	async bool,
	write func(tx *gorm.DB) error,
) error {
	if async && a != nil && a.rlsEnabled() && a.hasTenantScope() {
		if !a.usesCurrentTenantPreProvider(organizationID) {
			return errors.New("outgoing pre-provider tenant scope does not match account organization")
		}
		current := bindRealtimeAppToDB(a.DB.WithContext(ctx), a)
		var err error
		for attempt := 0; attempt < canonicalContactWriteAttempts; attempt++ {
			err = current.Transaction(write)
			if !isRetryableCanonicalContactWrite(err) {
				return err
			}
		}
		return err
	}
	return a.outgoingCanonicalTransaction(ctx, organizationID, write)
}

func (a *App) usesCurrentTenantPreProvider(organizationID uuid.UUID) bool {
	return a != nil && a.rlsEnabled() && a.DB != nil &&
		a.tenantOrgID != uuid.Nil && a.tenantOrgID == organizationID
}

// outgoingTenantTransaction normally starts from the root pool. In
// particular, provider delivery never becomes a savepoint inside a request
// Tenant transaction, so a provider attempt cannot be made durable or
// ambiguous only at the mercy of a later outer commit. Async request callers
// use outgoingPreProviderTransaction only for their pending state, then start
// provider delivery after that request commits. The inbound continuation
// exception below uses its current transaction because a separately committed
// action claim already provides its crash boundary.
func (a *App) outgoingTenantTransaction(
	ctx context.Context,
	organizationID uuid.UUID,
	write func(tx *gorm.DB) error,
) error {
	// Inbound continuation processing already owns an RLS tenant transaction
	// and may hold the canonical Contact lock before a chatbot response is
	// sent. Re-entering through the root pool would wait on our own outer lock.
	// Its separately committed action claim is the at-most-once boundary, so
	// keep the pending Message, provider attempt, and result in that current
	// transaction.
	if a.inboundContinuation != nil &&
		a.rlsEnabled() &&
		a.tenantOrgID == organizationID &&
		a.DB != nil {
		return write(a.DB.WithContext(ctx))
	}

	root := a.rootApp()
	db := root.DB.WithContext(ctx)
	if root.rlsEnabled() {
		return database.WithTenant(db, organizationID, write)
	}
	return db.Transaction(write)
}

// lockOutgoingContactVisibility rechecks an agent's live claim after the
// canonical contact row is locked. Assignment is protected by that contact
// lock; an active transfer is share-locked so it cannot be resumed between
// authorization and message commit.
func (a *App) lockOutgoingContactVisibility(
	tx *gorm.DB,
	orgID, userID uuid.UUID,
	contact *models.Contact,
) error {
	if userID == uuid.Nil || contact == nil {
		return errOutgoingContactAccessRevoked
	}
	if a.scopedApp(tx, orgID).HasPermission(
		userID,
		models.ResourceContacts,
		models.ActionRead,
		orgID,
	) {
		return nil
	}
	if contact.AssignedUserID != nil && *contact.AssignedUserID == userID {
		return nil
	}

	var transfer models.AgentTransfer
	err := tx.Clauses(clause.Locking{Strength: "SHARE"}).
		Select("id").
		Where(
			"organization_id = ? AND contact_id = ? AND agent_id = ? AND status = ?",
			orgID,
			contact.ID,
			userID,
			models.TransferStatusActive,
		).
		First(&transfer).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return errOutgoingContactAccessRevoked
	}
	return err
}

// ============================================================================
// Internal Helpers
// ============================================================================

// toWhatsAppAccount converts models.WhatsAppAccount to whatsapp.Account
func (a *App) toWhatsAppAccount(account *models.WhatsAppAccount) *whatsapp.Account {
	return account.ToWAAccount()
}

// toWhatsAppAccountWithMetaApp overlays the effective organization-level App
// ID for endpoints that require it (resumable uploads). It never mutates or
// persists the source account, so central credential changes take effect
// immediately without copying app credentials into account rows.
func (a *App) toWhatsAppAccountWithMetaApp(account *models.WhatsAppAccount) (*whatsapp.Account, error) {
	if account == nil {
		return nil, errors.New("WhatsApp account is required")
	}
	appID, _, _, err := a.resolveEffectiveMetaAppCreds(account)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(appID) == "" {
		return nil, errMetaAppIDNotConfigured
	}
	result := account.ToWAAccount()
	result.AppID = strings.TrimSpace(appID)
	return result, nil
}

// createOutgoingMessage creates a Message model from the request
func (a *App) createOutgoingMessage(req OutgoingMessageRequest, opts MessageSendOptions) *models.Message {
	msg := &models.Message{
		BaseModel:       models.BaseModel{ID: uuid.New()},
		OrganizationID:  req.Account.OrganizationID,
		WhatsAppAccount: req.Account.Name,
		ContactID:       req.Contact.ID,
		Direction:       models.DirectionOutgoing,
		MessageType:     req.Type,
		Status:          models.MessageStatusPending,
		SentByUserID:    opts.SentByUserID,
	}

	// Set content based on message type
	switch req.Type {
	case models.MessageTypeText:
		msg.Content = req.Content

	case models.MessageTypeImage, models.MessageTypeVideo, models.MessageTypeAudio, models.MessageTypeDocument:
		msg.Content = req.Caption
		msg.MediaURL = req.MediaURL
		msg.MediaMimeType = req.MediaMimeType
		msg.MediaFilename = req.MediaFilename

	case models.MessageTypeInteractive:
		msg.Content = req.BodyText
		msg.InteractiveData = a.buildInteractiveData(req)

	case models.MessageTypeFlow:
		msg.Content = req.BodyText
		msg.InteractiveData = models.JSONB{
			"type":        "flow",
			"body":        req.BodyText,
			"button_text": req.FlowCTA,
			"flow_id":     req.FlowID,
		}

	case models.MessageTypeTemplate:
		if req.Template != nil {
			// Store actual rendered content instead of just template name
			content := templateutil.ReplaceWithStringParams(req.Template.BodyContent, req.BodyParams)
			if content == "" {
				content = fmt.Sprintf("[Template: %s]", req.Template.DisplayName)
			}
			msg.Content = content
			msg.TemplateName = req.Template.Name
			msg.Metadata = models.JSONB{
				"template_name": req.Template.Name,
				"template_id":   req.Template.ID.String(),
			}
			// Store header media so it renders in the chat bubble
			if req.MediaURL != "" {
				msg.MediaURL = req.MediaURL
				msg.MediaMimeType = req.MediaMimeType
			}
			// Store template buttons so they render in the chat bubble
			if len(req.Template.Buttons) > 0 {
				msg.InteractiveData = a.buildInteractiveData(req)
			}
		}
	}

	// Handle reply context
	if req.ReplyToMessage != nil {
		msg.IsReply = true
		replyID := req.ReplyToMessage.ID
		msg.ReplyToMessageID = &replyID
	}

	return msg
}

// buildInteractiveData creates the InteractiveData JSONB for interactive and template messages
func (a *App) buildInteractiveData(req OutgoingMessageRequest) models.JSONB {
	// Template buttons: stored as JSONBArray on Template.Buttons
	// Resolve dynamic URL params (e.g., {{1}}) before storing
	if req.Template != nil && len(req.Template.Buttons) > 0 {
		buttons := make([]any, len(req.Template.Buttons))
		for i, btn := range req.Template.Buttons {
			btnMap, ok := btn.(map[string]any)
			if !ok {
				buttons[i] = btn
				continue
			}
			resolved := make(map[string]any, len(btnMap))
			maps.Copy(resolved, btnMap)
			if resolved["type"] == "URL" {
				if urlStr, ok := resolved["url"].(string); ok {
					idx := fmt.Sprintf("%d", i)
					if val, exists := req.ButtonURLParams[idx]; exists {
						resolved["url"] = templateutil.ParameterPattern.ReplaceAllString(urlStr, val)
					}
				}
			}
			buttons[i] = resolved
		}
		return models.JSONB{
			"type":    "button",
			"buttons": buttons,
		}
	}

	switch req.InteractiveType {
	case "cta_url":
		return models.JSONB{
			"type":        "cta_url",
			"body":        req.BodyText,
			"button_text": req.ButtonText,
			"url":         req.URL,
		}
	case "voice_call":
		// Don't store the payload — it carries server-only context (the
		// originating agent id) and the chat history doesn't need it.
		out := models.JSONB{
			"type":         "voice_call",
			"body":         req.BodyText,
			"display_text": req.DisplayText,
		}
		if req.TTLMinutes > 0 {
			out["ttl_minutes"] = req.TTLMinutes
		}
		return out
	case "list":
		rows := make([]any, len(req.Buttons))
		for i, btn := range req.Buttons {
			rows[i] = map[string]string{"id": btn.ID, "title": btn.Title}
		}
		return models.JSONB{
			"type": "list",
			"body": req.BodyText,
			"rows": rows,
		}
	default: // "button"
		buttons := make([]any, len(req.Buttons))
		for i, btn := range req.Buttons {
			buttons[i] = map[string]string{"id": btn.ID, "title": btn.Title}
		}
		return models.JSONB{
			"type":    "button",
			"body":    req.BodyText,
			"buttons": buttons,
		}
	}
}

// finalizeMessageSend persists the result when requested and triggers
// post-send actions. Policy-aware delivery normally persists inside the
// canonical-contact transaction and passes persist=false.
func (a *App) finalizeMessageSend(
	msg *models.Message,
	req OutgoingMessageRequest,
	opts MessageSendOptions,
	wamid string,
	err error,
	persist bool,
) {
	// Use Where instead of Model(msg) to avoid mutating the shared msg struct,
	// which may be read concurrently by the caller when sending is async.
	if err != nil {
		errMsg := err.Error()
		statusPersisted := true

		if persist {
			persistErr := a.DB.Model(&models.Message{}).Where("id = ?", msg.ID).Updates(map[string]any{
				"status":        models.MessageStatusFailed,
				"error_message": errMsg,
			}).Error
			statusPersisted = persistErr == nil
			if persistErr != nil {
				a.Log.Error("Failed to persist message failure", "error", persistErr, "message_id", msg.ID)
			}
		}
		a.Log.Error("Failed to send message", "error", err, "message_id", msg.ID, "type", msg.MessageType)

		if opts.BroadcastWebSocket && statusPersisted {
			fallback := websocket.WSMessage{
				Type: websocket.TypeStatusUpdate,
				Payload: map[string]any{
					"message_id":    msg.ID,
					"contact_id":    req.Contact.ID,
					"status":        models.MessageStatusFailed,
					"error_message": errMsg,
				},
			}
			contactID := req.Contact.ID
			a.publishRealtimeEvent(queue.RealtimeEvent{
				OrganizationID: req.Account.OrganizationID,
				Kind:           queue.RealtimeEventMessageStatusChanged,
				ContactID:      &contactID,
				MessageID:      &msg.ID,
				Status:         string(models.MessageStatusFailed),
			}, &fallback)
		}
		return
	}

	statusPersisted := true
	if persist {
		persistErr := a.DB.Model(&models.Message{}).Where("id = ?", msg.ID).Updates(map[string]any{
			"status":               models.MessageStatusSent,
			"whats_app_message_id": wamid,
		}).Error
		statusPersisted = persistErr == nil
		if persistErr != nil {
			a.Log.Error("Failed to persist message success", "error", persistErr, "message_id", msg.ID)
		}
	}
	a.Log.Info("Message sent", "message_id", msg.ID, "wa_message_id", wamid, "type", msg.MessageType)

	// Dispatch webhook for successful send
	if opts.DispatchWebhook {
		a.dispatchMessageSentWebhook(req.Account, req.Contact, msg)
	}

	if opts.BroadcastWebSocket && statusPersisted {
		fallback := websocket.WSMessage{
			Type: websocket.TypeStatusUpdate,
			Payload: map[string]any{
				"message_id": msg.ID,
				"contact_id": req.Contact.ID,
				"status":     models.MessageStatusSent,
				"wamid":      wamid,
			},
		}
		contactID := req.Contact.ID
		a.publishRealtimeEvent(queue.RealtimeEvent{
			OrganizationID: req.Account.OrganizationID,
			Kind:           queue.RealtimeEventMessageStatusChanged,
			ContactID:      &contactID,
			MessageID:      &msg.ID,
			Status:         string(models.MessageStatusSent),
		}, &fallback)
	}

	// Mark the contact's incoming messages as read once a chatbot reply has
	// gone out. Keeps the agent's contact-list unread count clean for
	// conversations the bot is auto-handling. See issue #280.
	if opts.MarkIncomingRead {
		a.markMessagesAsRead(req.Account.OrganizationID, req.Contact.ID, req.Contact)
	}
}

// broadcastNewMessage broadcasts a new message via WebSocket
func (a *App) broadcastNewMessage(orgID uuid.UUID, msg *models.Message, contact *models.Contact) {
	if a == nil || msg == nil || contact == nil {
		return
	}

	var assignedUserIDStr string
	if contact.AssignedUserID != nil {
		assignedUserIDStr = contact.AssignedUserID.String()
	}
	profileName := contact.ProfileName
	if a.ShouldMaskPhoneNumbers(orgID) {
		profileName = utils.MaskIfPhoneNumber(profileName)
	}

	payload := map[string]any{
		"id":               msg.ID.String(),
		"contact_id":       contact.ID.String(),
		"whatsapp_account": msg.WhatsAppAccount,
		"assigned_user_id": assignedUserIDStr,
		"profile_name":     profileName,
		"direction":        msg.Direction,
		"message_type":     msg.MessageType,
		"content":          map[string]string{"body": msg.Content},
		"media_url":        msg.MediaURL,
		"media_mime_type":  msg.MediaMimeType,
		"media_filename":   msg.MediaFilename,
		"interactive_data": msg.InteractiveData,
		"status":           msg.Status,
		"wamid":            msg.WhatsAppMessageID,
		"ingested_at":      msg.IngestedAt,
		"created_at":       msg.CreatedAt,
		"updated_at":       msg.UpdatedAt,
		"is_reply":         msg.IsReply,
	}

	// Add interactive data
	if msg.InteractiveData != nil {
		payload["interactive_data"] = msg.InteractiveData
	}

	// Add reply context
	if msg.IsReply && msg.ReplyToMessageID != nil {
		payload["reply_to_message_id"] = msg.ReplyToMessageID.String()

		// Include reply preview for UI
		var replyToMsg models.Message
		if err := a.DB.First(&replyToMsg, msg.ReplyToMessageID).Error; err == nil {
			payload["reply_to_message"] = map[string]any{
				"id":           replyToMsg.ID.String(),
				"content":      map[string]string{"body": replyToMsg.Content},
				"message_type": replyToMsg.MessageType,
				"direction":    replyToMsg.Direction,
			}
		}
	}

	fallback := websocket.WSMessage{
		Type:    websocket.TypeNewMessage,
		Payload: payload,
	}
	contactID := contact.ID
	a.publishRealtimeEvent(queue.RealtimeEvent{
		OrganizationID: orgID,
		Kind:           queue.RealtimeEventMessageCreated,
		ContactID:      &contactID,
		MessageID:      &msg.ID,
		Status:         string(msg.Status),
		OccurredAt:     msg.EffectiveIngestedAt(),
	}, &fallback)
}

// broadcastReactionUpdate broadcasts a reaction update via WebSocket
func (a *App) broadcastReactionUpdate(orgID uuid.UUID, messageID, contactID uuid.UUID, reactions any) {
	if a.WSHub == nil {
		return
	}
	a.WSHub.BroadcastToOrg(orgID, websocket.WSMessage{
		Type: "reaction_update",
		Payload: map[string]any{
			"message_id": messageID.String(),
			"contact_id": contactID.String(),
			"reactions":  reactions,
		},
	})
}

// dispatchMessageSentWebhook dispatches webhook for message.sent event
func (a *App) dispatchMessageSentWebhook(account *models.WhatsAppAccount, contact *models.Contact, msg *models.Message) {
	var sentByUserID string
	if msg.SentByUserID != nil {
		sentByUserID = msg.SentByUserID.String()
	}

	a.DispatchWebhook(account.OrganizationID, models.WebhookEventMessageSent, MessageEventData{
		MessageID:       msg.ID.String(),
		ContactID:       contact.ID.String(),
		ContactPhone:    contact.PhoneNumber,
		ContactName:     contact.ProfileName,
		MessageType:     msg.MessageType,
		Content:         msg.Content,
		WhatsAppAccount: account.Name,
		Direction:       models.DirectionOutgoing,
		SentByUserID:    sentByUserID,
	})
}

// getMessagePreview returns a preview string for the message
func (a *App) getMessagePreview(req OutgoingMessageRequest) string {
	switch req.Type {
	case models.MessageTypeText:
		return truncateString(req.Content, 100)
	case models.MessageTypeImage:
		if req.Caption != "" {
			return truncateString(req.Caption, 100)
		}
		return "[Image]"
	case models.MessageTypeVideo:
		if req.Caption != "" {
			return truncateString(req.Caption, 100)
		}
		return "[Video]"
	case models.MessageTypeAudio:
		return "[Audio]"
	case models.MessageTypeDocument:
		if req.MediaFilename != "" {
			return "[Document: " + req.MediaFilename + "]"
		}
		return "[Document]"
	case models.MessageTypeInteractive:
		return truncateString(req.BodyText, 100)
	case models.MessageTypeFlow:
		return truncateString(req.BodyText, 100)
	case models.MessageTypeTemplate:
		if req.Template != nil {
			return fmt.Sprintf("[Template: %s]", req.Template.DisplayName)
		}
		return "[Template]"
	default:
		return "[Message]"
	}
}

// ============================================================================
// HTTP Handlers
// ============================================================================

// SendTemplateMessageRequest represents the request to send a template message
type SendTemplateMessageRequest struct {
	ContactID      string            `json:"contact_id"`
	PhoneNumber    string            `json:"phone_number"`    // Alternative to contact_id - send to phone directly
	TemplateName   string            `json:"template_name"`   // Template name
	TemplateID     string            `json:"template_id"`     // Alternative: template UUID
	TemplateParams map[string]string `json:"template_params"` // Named or positional params
	ButtonParams   map[string]string `json:"button_params"`   // Button index -> dynamic URL param value
	AccountName    string            `json:"account_name"`    // Optional: specific WhatsApp account

	// Header media for templates with IMAGE/VIDEO/DOCUMENT headers.
	// Three options (in priority order):
	//   1. header_media_id  — pre-uploaded WhatsApp media ID (skip upload)
	//   2. header_media_url — URL to fetch the media from (server downloads & uploads to WhatsApp)
	//   3. multipart header_file — raw file upload via multipart/form-data
	HeaderMediaID       string `json:"header_media_id"`       // Already-uploaded WhatsApp media ID
	HeaderMediaURL      string `json:"header_media_url"`      // URL to download media from
	HeaderMediaFilename string `json:"header_media_filename"` // Filename — required by Meta for DOCUMENT headers (#351)

	// Header text parameter values for TEXT headers that contain a {{var}}.
	// Meta only permits one variable in a TEXT header. Keyed by the variable's
	// name (named templates) or by "1" (positional). Optional — if absent, the
	// value is looked up in TemplateParams as a fallback.
	HeaderParams map[string]string `json:"header_params"`
}

const (
	templateHeaderMediaMaxBytes = 16 << 20
	templateHeaderMediaTimeout  = 30 * time.Second
)

var (
	errTemplateHeaderMediaInvalidURL        = errors.New("template header media URL is invalid")
	errTemplateHeaderMediaClientUnavailable = errors.New("template header media HTTP client is unavailable")
	errTemplateHeaderMediaDownloadFailed    = errors.New("template header media download failed")
	errTemplateHeaderMediaUnexpectedStatus  = errors.New("template header media URL did not return HTTP 200")
	errTemplateHeaderMediaTooLarge          = errors.New("template header media exceeds the 16 MiB limit")
)

func readTemplateHeaderMedia(reader io.Reader) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(reader, templateHeaderMediaMaxBytes+1))
	if err != nil {
		return nil, errTemplateHeaderMediaDownloadFailed
	}
	if len(data) > templateHeaderMediaMaxBytes {
		return nil, errTemplateHeaderMediaTooLarge
	}
	return data, nil
}

func (a *App) downloadTemplateHeaderMedia(parentCtx context.Context, rawURL string) ([]byte, string, error) {
	mediaURL := strings.TrimSpace(rawURL)
	if err := validateWebhookRuntimeURL(mediaURL); err != nil {
		return nil, "", errTemplateHeaderMediaInvalidURL
	}
	if a == nil || a.HTTPClient == nil || a.HTTPClient.Transport == nil {
		return nil, "", errTemplateHeaderMediaClientUnavailable
	}

	if parentCtx == nil {
		parentCtx = context.Background()
	}
	downloadCtx, cancel := context.WithTimeout(parentCtx, templateHeaderMediaTimeout)
	defer cancel()

	request, err := http.NewRequestWithContext(downloadCtx, http.MethodGet, mediaURL, nil)
	if err != nil {
		return nil, "", errTemplateHeaderMediaInvalidURL
	}

	// Preserve the injected SSRF-safe transport and connection pool while
	// preventing a validated public URL from redirecting to a different target.
	client := *a.HTTPClient
	client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}

	response, err := client.Do(request)
	if err != nil {
		return nil, "", errTemplateHeaderMediaDownloadFailed
	}
	defer func() { _ = response.Body.Close() }()

	if response.StatusCode != http.StatusOK {
		return nil, "", errTemplateHeaderMediaUnexpectedStatus
	}
	if response.ContentLength > templateHeaderMediaMaxBytes {
		return nil, "", errTemplateHeaderMediaTooLarge
	}

	data, err := readTemplateHeaderMedia(response.Body)
	if err != nil {
		return nil, "", err
	}
	mimeType := response.Header.Get("Content-Type")
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}
	return data, mimeType, nil
}

// SendTemplateMessage sends a template message to a contact or phone number.
// Accepts either JSON body or multipart/form-data (when a header media file is included).
func (a *App) SendTemplateMessage(r *fastglue.Request) error {
	orgID, userID, err := a.requireAuth(r, models.ResourceChat, models.ActionWrite)
	if err != nil {
		return nil
	}

	var req SendTemplateMessageRequest
	var headerFileData []byte
	var headerFileMimeType string
	var headerFileFilename string

	contentType := string(r.RequestCtx.Request.Header.ContentType())
	if strings.HasPrefix(contentType, "multipart/form-data") {
		// Parse multipart form — used when template has a media header
		form, err := r.RequestCtx.MultipartForm()
		if err != nil {
			return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid multipart form", nil, "")
		}
		if v := form.Value["contact_id"]; len(v) > 0 {
			req.ContactID = v[0]
		}
		if v := form.Value["phone_number"]; len(v) > 0 {
			req.PhoneNumber = v[0]
		}
		if v := form.Value["template_name"]; len(v) > 0 {
			req.TemplateName = v[0]
		}
		if v := form.Value["template_id"]; len(v) > 0 {
			req.TemplateID = v[0]
		}
		if v := form.Value["account_name"]; len(v) > 0 {
			req.AccountName = v[0]
		}
		// Parse template_params from JSON string
		if v := form.Value["template_params"]; len(v) > 0 && v[0] != "" {
			if err := json.Unmarshal([]byte(v[0]), &req.TemplateParams); err != nil {
				return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid template_params JSON", nil, "")
			}
		}
		// Parse button_params from JSON string
		if v := form.Value["button_params"]; len(v) > 0 && v[0] != "" {
			if err := json.Unmarshal([]byte(v[0]), &req.ButtonParams); err != nil {
				return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid button_params JSON", nil, "")
			}
		}
		// Parse header_params from JSON string
		if v := form.Value["header_params"]; len(v) > 0 && v[0] != "" {
			if err := json.Unmarshal([]byte(v[0]), &req.HeaderParams); err != nil {
				return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid header_params JSON", nil, "")
			}
		}
		// Read header media file
		if files := form.File["header_file"]; len(files) > 0 {
			fh := files[0]
			if fh.Size > templateHeaderMediaMaxBytes {
				return r.SendErrorEnvelope(fasthttp.StatusRequestEntityTooLarge, "Header file is too large. Maximum size is 16 MiB", nil, "")
			}
			f, err := fh.Open()
			if err != nil {
				return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Failed to read header file", nil, "")
			}
			defer f.Close() //nolint:errcheck
			headerFileData, err = readTemplateHeaderMedia(f)
			if err != nil {
				if errors.Is(err, errTemplateHeaderMediaTooLarge) {
					return r.SendErrorEnvelope(fasthttp.StatusRequestEntityTooLarge, "Header file is too large. Maximum size is 16 MiB", nil, "")
				}
				a.Log.Error("Failed to read header file", "error", err)
				return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to read header file", nil, "")
			}
			headerFileMimeType = fh.Header.Get("Content-Type")
			if headerFileMimeType == "" {
				headerFileMimeType = "application/octet-stream"
			}
			headerFileFilename = fh.Filename
		}
		if v := form.Value["header_media_filename"]; len(v) > 0 {
			req.HeaderMediaFilename = v[0]
		}
	} else {
		if err := a.decodeRequest(r, &req); err != nil {
			return nil
		}
	}

	// Must have either contact_id or phone_number
	if req.ContactID == "" && req.PhoneNumber == "" {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Either contact_id or phone_number is required", nil, "")
	}

	// Must have either template_name or template_id
	if req.TemplateName == "" && req.TemplateID == "" {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Either template_name or template_id is required", nil, "")
	}

	// Get template
	var template models.Template
	if req.TemplateID != "" {
		templateID, err := uuid.Parse(req.TemplateID)
		if err != nil {
			return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid template_id", nil, "")
		}
		t, err := findByIDAndOrg[models.Template](a.DB, r, templateID, orgID, "Template")
		if err != nil {
			return nil
		}
		template = *t
	} else {
		if err := a.DB.Where("name = ? AND organization_id = ?", req.TemplateName, orgID).First(&template).Error; err != nil {
			return r.SendErrorEnvelope(fasthttp.StatusNotFound, "Template not found", nil, "")
		}
	}

	// Check template is approved
	if template.Status != "APPROVED" {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, fmt.Sprintf("Template is not approved (status: %s)", template.Status), nil, "")
	}

	// Get contact or use phone number directly
	var contact *models.Contact

	if req.ContactID != "" {
		cID, err := uuid.Parse(req.ContactID)
		if err != nil {
			return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid contact_id", nil, "")
		}
		c, err := findByIDAndOrg[models.Contact](a.DB, r, cID, orgID, "Contact")
		if err != nil {
			return nil
		}
		contact = c
	} else {
		// This handler sends asynchronously. Under an RLS Tenant request the
		// phone-only contact therefore belongs to the same pre-provider outer
		// transaction as the pending Message; delivery is started only after that
		// transaction commits. Root/background callers still receive an
		// independent committed transaction.
		var created bool
		err := a.outgoingPreProviderTransaction(
			context.Background(),
			orgID,
			true,
			func(tx *gorm.DB) error {
				var resolveErr error
				contact, created, resolveErr = contactutil.GetOrCreateContact(
					tx,
					orgID,
					req.PhoneNumber,
					"",
				)
				return resolveErr
			},
		)
		if err != nil {
			a.Log.Error(
				"Failed to find or create contact",
				"error", err,
				"phone", req.PhoneNumber,
			)
			return r.SendErrorEnvelope(
				fasthttp.StatusInternalServerError,
				"Failed to create contact",
				nil,
				"",
			)
		}
		if created {
			a.Log.Info(
				"Contact created from API",
				"contact_id", contact.ID,
				"phone", contact.PhoneNumber,
			)
		}
	}

	// Determine which WhatsApp account to use (explicit > template > contact > default)
	accountName := req.AccountName
	if accountName == "" {
		accountName = template.WhatsAppAccount
	}
	if accountName == "" && contact != nil {
		accountName = contact.WhatsAppAccount
	}

	account, err := a.resolveWhatsAppAccount(orgID, accountName)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, err.Error(), nil, "")
	}

	// Extract parameter names and resolve values
	paramNames := templateutil.ExtParamNames(template.BodyContent)
	bodyParams := templateutil.ResolveParamsFromMap(paramNames, req.TemplateParams)

	// Validate that all required parameters are provided
	if len(paramNames) > 0 {
		var missingParams []string
		for i, name := range paramNames {
			if i >= len(bodyParams) || bodyParams[i] == "" {
				missingParams = append(missingParams, name)
			}
		}
		if len(missingParams) > 0 {
			return r.SendErrorEnvelope(fasthttp.StatusBadRequest,
				fmt.Sprintf("Missing template parameters: %s. Expected parameters: %v", strings.Join(missingParams, ", "), paramNames),
				nil, "")
		}
	}

	// Validate the header variable (TEXT headers only). Meta allows at most one
	// variable in a TEXT header — surface a clean 400 if it's missing.
	if template.HeaderType == "TEXT" {
		headerNames := templateutil.ExtParamNames(template.HeaderContent)
		if len(headerNames) > 1 {
			return r.SendErrorEnvelope(fasthttp.StatusBadRequest,
				fmt.Sprintf("Template header text contains %d variables; Meta allows at most 1", len(headerNames)),
				nil, "")
		}
		if len(headerNames) == 1 {
			name := headerNames[0]
			if req.HeaderParams[name] == "" && req.TemplateParams[name] == "" {
				return r.SendErrorEnvelope(fasthttp.StatusBadRequest,
					fmt.Sprintf("Missing header parameter %q. Pass it in header_params or template_params.", name),
					nil, "")
			}
		}
	}

	// Resolve header media for templates with IMAGE/VIDEO/DOCUMENT headers.
	// Priority: header_media_id > header_media_url > multipart header_file
	var headerMediaID string
	var headerMediaData []byte
	var headerMimeType string
	headerMediaURL := strings.TrimSpace(req.HeaderMediaURL)
	if template.HeaderType == "IMAGE" || template.HeaderType == "VIDEO" || template.HeaderType == "DOCUMENT" {
		if req.HeaderMediaID != "" {
			// Option 1: Pre-uploaded WhatsApp media ID — use directly (no local preview)
			headerMediaID = req.HeaderMediaID
		} else if headerMediaURL != "" {
			// Option 2: Download from URL, then upload to WhatsApp
			headerMediaData, headerMimeType, err = a.downloadTemplateHeaderMedia(requestContext(r), headerMediaURL)
			if err != nil {
				switch {
				case errors.Is(err, errTemplateHeaderMediaInvalidURL):
					return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid header media URL", nil, "")
				case errors.Is(err, errTemplateHeaderMediaClientUnavailable):
					return r.SendErrorEnvelope(fasthttp.StatusServiceUnavailable, "Header media download is unavailable", nil, "")
				case errors.Is(err, errTemplateHeaderMediaUnexpectedStatus):
					return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Header media URL must return HTTP 200", nil, "")
				case errors.Is(err, errTemplateHeaderMediaTooLarge):
					return r.SendErrorEnvelope(fasthttp.StatusRequestEntityTooLarge, "Header media is too large. Maximum size is 16 MiB", nil, "")
				default:
					a.Log.Error("Failed to download template header media", "error", err)
					return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Failed to download header media", nil, "")
				}
			}
		} else if len(headerFileData) > 0 {
			// Option 3: Multipart file upload
			headerMediaData = headerFileData
			headerMimeType = headerFileMimeType
		}

		// Upload to WhatsApp if we have raw data (options 2 & 3)
		if len(headerMediaData) > 0 {
			waAcct := a.toWhatsAppAccount(account)
			mediaID, err := a.WhatsApp.UploadMedia(context.Background(), waAcct, headerMediaData, headerMimeType, "header")
			if err != nil {
				a.Log.Error("Failed to upload template header media", "error", err)
				return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to upload header media to WhatsApp", nil, "")
			}
			headerMediaID = mediaID
		}
	}

	// Persist header media so it can be served for chat preview.
	var headerMediaPath string
	if len(headerMediaData) > 0 {
		mediaPath, err := a.saveMessageMedia(r.RequestCtx, orgID, headerMediaData, headerMimeType, "header")
		if err != nil {
			a.Log.Error("Failed to store template header media", "error", err, "org_id", orgID)
			// Non-fatal — message will still send, just won't show preview
		} else {
			headerMediaPath = mediaPath
		}
	}

	// For authentication templates with OTP COPY_CODE buttons, Meta expects
	// a button component with sub_type "url" and the OTP code as a text parameter.
	// Auto-populate from template_params["1"] so callers don't need button_params.
	buttonParams := req.ButtonParams
	if strings.EqualFold(template.Category, "AUTHENTICATION") && len(buttonParams) == 0 {
		if code, ok := req.TemplateParams["1"]; ok && code != "" {
			for i, raw := range template.Buttons {
				if btn, ok := raw.(map[string]any); ok {
					btnType, _ := btn["type"].(string)
					if strings.EqualFold(btnType, "OTP") {
						if buttonParams == nil {
							buttonParams = make(map[string]string)
						}
						buttonParams[fmt.Sprintf("%d", i)] = code
						break
					}
				}
			}
		}
	}

	// Resolve filename for DOCUMENT headers — required by Meta (#351).
	// Caller-supplied wins, then the multipart filename.
	headerMediaFilename := req.HeaderMediaFilename
	if headerMediaFilename == "" {
		headerMediaFilename = headerFileFilename
	}

	// Send using unified message sender
	msgReq := OutgoingMessageRequest{
		Account:             account,
		Contact:             contact,
		Type:                models.MessageTypeTemplate,
		Template:            &template,
		BodyParams:          req.TemplateParams,
		HeaderParams:        req.HeaderParams,
		HeaderMediaID:       headerMediaID,
		HeaderMediaFilename: headerMediaFilename,
		MediaURL:            headerMediaPath,
		MediaMimeType:       headerMimeType,
		ButtonURLParams:     buttonParams,
	}

	opts := DefaultSendOptions()
	opts.SentByUserID = &userID

	ctx := context.Background()
	message, err := a.SendOutgoingMessage(ctx, msgReq, opts)
	if err != nil {
		var consentErr *OutgoingConsentError
		if errors.As(err, &consentErr) {
			return r.SendErrorEnvelope(fasthttp.StatusBadRequest, consentErr.Error(), nil, "")
		}
		a.Log.Error("Failed to send template message", "error", err)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to send template message", nil, "")
	}

	// Build full message response (same shape as SendMessage)
	response := MessageResponse{
		ID:              message.ID,
		ContactID:       message.ContactID,
		Direction:       message.Direction,
		MessageType:     message.MessageType,
		Content:         map[string]string{"body": message.Content},
		InteractiveData: message.InteractiveData,
		Status:          message.Status,
		IsReply:         message.IsReply,
		WhatsAppAccount: message.WhatsAppAccount,
		IngestedAt:      message.IngestedAt,
		CreatedAt:       message.CreatedAt,
		UpdatedAt:       message.UpdatedAt,
	}
	return r.SendEnvelope(response)
}
