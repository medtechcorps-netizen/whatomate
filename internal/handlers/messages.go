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
	channelapi "github.com/shridarpatil/whatomate/internal/channel"
	"github.com/shridarpatil/whatomate/internal/contactutil"
	"github.com/shridarpatil/whatomate/internal/database"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/internal/queue"
	"github.com/shridarpatil/whatomate/internal/templateutil"
	"github.com/shridarpatil/whatomate/internal/utils"
	"github.com/shridarpatil/whatomate/internal/websocket"
	"github.com/shridarpatil/whatomate/internal/whatsappaccount"
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

	// Media messages (image, video, audio, document). MediaData and
	// MediaMimeType also carry a template's raw header media into the fenced
	// provider phase when HeaderMediaID is not already available.
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

	// forwardedFrom is set only by the forward handler from a server-loaded
	// source message and is recorded as metadata.forwarded_from.
	forwardedFrom *messageForwardSource

	// forwardMedia defers a forwarded attachment's storage copy and Meta
	// upload to the asynchronous delivery goroutine, after the request
	// committed and before any provider transaction opens.
	forwardMedia *forwardMediaCopy

	// forwardOutcome, set only by the forward handler, records which phase of
	// the pending-message write a failure happened in (see forwardSendOutcome).
	forwardOutcome *forwardSendOutcome

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

	// AutomaticAI identifies a provider attempt governed by the inbound
	// Pause/Resume fence. Only the server-owned chatbot continuation may set
	// it. Manual/API/campaign/SLA delivery remains outside this policy.
	AutomaticAI bool
}

var (
	errOutgoingContactNotFound        = errors.New("outgoing message contact not found")
	errOutgoingContactAccessRevoked   = errors.New("outgoing message contact access revoked")
	errOutgoingReplyInvalid           = errors.New("outgoing reply message is invalid")
	errOutgoingProviderReceiptMissing = errors.New(
		"provider returned success without a WhatsApp message ID",
	)
)

type outgoingProviderPanicError struct {
	value             any
	providerAttempted bool
}

func (e *outgoingProviderPanicError) Error() string {
	if e == nil {
		return "outgoing provider operation panicked"
	}
	return fmt.Sprintf("outgoing provider operation panicked: %v", e.value)
}

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
		AutomaticAI:        true,
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
	if err := whatsappaccount.RequireActiveForOutbound(req.Account); err != nil {
		return nil, err
	}
	if req.Contact == nil || req.Contact.ID == uuid.Nil {
		return nil, fmt.Errorf("%w: contact is required", errOutgoingContactNotFound)
	}
	if req.forwardMedia != nil && !opts.Async {
		return nil, errors.New("a forwarded attachment requires asynchronous delivery")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if opts.AutomaticAI {
		if a.inboundContinuation == nil ||
			!a.inboundContinuation.attemptGuarded {
			return nil, errors.New(
				"automatic AI send requires the physical organization attempt fence",
			)
		}
		if opts.Async || opts.SentByUserID != nil ||
			req.legacyWhatsAppReply != nil {
			return nil, errors.New(
				"automatic AI send cannot use async, agent, or legacy-reply delivery",
			)
		}
	}

	organizationID := req.Account.OrganizationID
	var forwardLease *forwardWorkLease
	forwardHandedOff := false
	if req.forwardedFrom != nil && opts.Async {
		var admissionErr error
		forwardLease, admissionErr = a.reserveForward()
		if admissionErr != nil {
			return nil, admissionErr
		}
		defer func() {
			if !forwardHandedOff {
				forwardLease.release()
			}
		}()
	}
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
	if req.forwardOutcome != nil {
		// Mirrors outgoingPreProviderTransaction's branch: an asynchronous send
		// entered through an RLS Tenant request writes its pending row as a
		// savepoint of that request transaction.
		req.forwardOutcome.writeRequested = true
		req.forwardOutcome.requestTransaction = opts.Async && a.rlsEnabled() && a.hasTenantScope()
	}
	err := a.outgoingPreProviderTransaction(ctx, organizationID, opts.Async, func(tx *gorm.DB) error {
		if req.forwardOutcome != nil {
			req.forwardOutcome.writeStarted = true
			req.forwardOutcome.reachedCommit = false
		}
		if req.legacyWhatsAppReply != nil {
			// The pending Message insert below locks the organization row (write
			// guard and foreign keys) while this transaction owns the shadow, so
			// own that row before the shadow, as the mirror does, and never queue
			// for a shadow that another reply holds across Meta while holding the
			// organization, except in the helper's bounded fallback after
			// lockLegacyMetaShadowRounds lost rounds. This transaction never calls
			// Meta. An incomplete policy is rejected below with its usual binding
			// error.
			if req.legacyWhatsAppReply.ChannelAccountID != uuid.Nil {
				if lockErr := channelapi.LockLegacyMetaOrganizationAndShadow(
					tx,
					organizationID,
					req.legacyWhatsAppReply.ChannelAccountID,
				); lockErr != nil {
					return lockErr
				}
			}
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

		if req.forwardedFrom != nil {
			// The request checked the path contact; this is the canonical one,
			// locked, on this transaction's own connection.
			if refusal := a.checkForwardTargetTx(tx, organizationID, contact, time.Now()); refusal != nil {
				return &forwardTargetRefusedError{refusal: refusal}
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
		if req.forwardedFrom != nil {
			// PDPA: no audit entry, no forward. Same transaction as the insert.
			if auditErr := recordForwardAudit(tx, organizationID, opts.SentByUserID, candidate, req.forwardedFrom); auditErr != nil {
				return auditErr
			}
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
		if req.forwardOutcome != nil {
			// Returning nil hands the transaction to COMMIT. From here on an
			// error (from COMMIT itself or after it was sent) is ambiguous.
			req.forwardOutcome.reachedCommit = true
		}
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

	// 2. Define the send function based on message type
	sendFn := func(
		sendCtx context.Context,
		deliveryContact *models.Contact,
		lockedAccount *models.WhatsAppAccount,
	) (string, error) {
		if req.deliveryOverride != nil {
			return req.deliveryOverride(sendCtx, deliveryContact)
		}
		waAccount := a.toWhatsAppAccount(lockedAccount)
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
				return a.WhatsApp.SendImageMessage(sendCtx, waAccount, rcpt, mediaID, req.Caption, replyToMsgID)
			case models.MessageTypeVideo:
				return a.WhatsApp.SendVideoMessage(sendCtx, waAccount, rcpt, mediaID, req.Caption, replyToMsgID)
			case models.MessageTypeAudio:
				return a.WhatsApp.SendAudioMessage(sendCtx, waAccount, rcpt, mediaID, replyToMsgID)
			default: // document
				return a.WhatsApp.SendDocumentMessage(sendCtx, waAccount, rcpt, mediaID, req.MediaFilename, req.Caption, replyToMsgID)
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
			headerMediaID := req.HeaderMediaID
			if headerMediaID == "" && len(req.MediaData) > 0 {
				var err error
				headerMediaID, err = a.WhatsApp.UploadMedia(
					sendCtx,
					waAccount,
					req.MediaData,
					req.MediaMimeType,
					"header",
				)
				if err != nil {
					return "", fmt.Errorf("failed to upload template header media: %w", err)
				}
			}
			components, err := whatsapp.BuildTemplateComponents(
				req.BodyParams,
				req.Template.HeaderType, req.Template.HeaderContent,
				req.HeaderParams,
				headerMediaID, req.HeaderMediaFilename,
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
			if forwardLease != nil && !forwardLease.start() {
				if forwardLease.state.Load() != 3 {
					// Duplicate callbacks cannot cancel an already running worker.
					return
				}
				// An outer transaction outlived its reservation. No provider work
				// has started; settle safely instead of starting an unbounded worker.
				if err := root.settleOutgoingFailureBeforeProvider(msg, req, opts, forwardServerBusyError()); err != nil {
					root.Log.Error("Failed to settle expired forward reservation", "error", err, "message_id", msg.ID)
				}
				return
			}
			root.wg.Add(1)
			go func() {
				defer root.wg.Done()
				defer forwardLease.release()
				// No transaction is open here: the request has committed and the
				// provider transaction starts in deliverOutgoingMessage.
				if req.forwardMedia != nil {
					if prepareErr := root.prepareForwardedMedia(&req); prepareErr != nil {
						if settlementErr := root.settleOutgoingFailureBeforeProvider(
							msg,
							req,
							opts,
							prepareErr,
						); settlementErr != nil {
							root.Log.Error(
								"Failed to settle forwarded message after media preparation failed",
								"error", settlementErr,
								"prepare_error", prepareErr,
								"organization_id", organizationID,
								"message_id", msg.ID,
							)
						}
						return
					}
				}
				if req.forwardedFrom != nil {
					// Single-use queued -> provider transition, committed before the
					// provider transaction opens. Fail closed: if the marker commit
					// is ambiguous this worker stops (and records "not sent", which
					// is true because it never calls the provider); if the row is no
					// longer pending and queued, it stops without touching it.
					claimed, markErr := root.claimForwardProviderPhase(organizationID, msg.ID)
					if markErr != nil {
						// Either way the provider is never called. Before COMMIT
						// (e.g. no pool connection within the deadline) the marker
						// certainly did not commit; after it, it may have. The row
						// is settled "not sent" with user-safe text, which is true:
						// a settled row can never be claimed or delivered. If this
						// settlement fails too, the row stays pending and later
						// reads as delayed (queued) or uncertain (provider).
						var claimErr *forwardClaimError
						root.Log.Error(
							"Forward provider-phase claim failed; not dispatching",
							"error", markErr,
							"marker_may_have_committed", errors.As(markErr, &claimErr) && claimErr.reachedCommit,
							"organization_id", organizationID,
							"message_id", msg.ID,
						)
						if settlementErr := root.settleOutgoingFailureBeforeProvider(
							msg,
							req,
							opts,
							forwardServerBusyError(),
						); settlementErr != nil {
							root.Log.Error(
								"Failed to settle forwarded message after its dispatch marker failed",
								"error", settlementErr,
								"marker_error", markErr,
								"organization_id", organizationID,
								"message_id", msg.ID,
							)
						}
						return
					}
					if !claimed {
						root.Log.Warn(
							"Forwarded message is no longer pending and queued; not dispatching",
							"organization_id", organizationID,
							"message_id", msg.ID,
						)
						return
					}
				}
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
		forwardHandedOff = true
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
	sendFn func(context.Context, *models.Contact, *models.WhatsAppAccount) (string, error),
) (outgoingDeliveryResult, error) {
	var result outgoingDeliveryResult
	if msg == nil || msg.ID == uuid.Nil {
		return result, errors.New("pending outgoing message is required")
	}
	if req.legacyWhatsAppReply == nil {
		if err := a.requireLegacyWhatsAppMessageMirror(ctx, req.Account, msg.ID); err != nil {
			return result, err
		}
	}
	if opts.AutomaticAI {
		return a.deliverAutomaticAIOutgoingMessage(
			ctx,
			msg,
			req,
			sendFn,
		)
	}

	err := a.outgoingProviderTransaction(
		ctx,
		req.Account.OrganizationID,
		func(tx *gorm.DB, providerAttempted *bool) error {
			if req.legacyWhatsAppReply != nil {
				// Unlike the pre-provider transaction, this one does not take the
				// organization row: it would then be held across the Meta call and
				// stall every policy fence in the tenant. Its only later access is
				// the WAMID owner trigger's FOR SHARE NOWAIT, which cannot wait
				// inside a lock cycle; if a fence holder makes it fail,
				// recoverOutgoingDeliveryResult waits for the row and records the
				// captured provider result instead.
				if lockErr := lockStrictLegacyReplyOrder(
					tx,
					req.Account.OrganizationID,
					req.legacyWhatsAppReply,
				); lockErr != nil {
					return lockErr
				}
			}
			// Hold a shared account lock across the final provider attempt so a
			// committed Coexistence disconnect always wins before Graph is called.
			// For legacy replies this follows their established ChannelAccount
			// prefix and precedes the Contact lock.
			lockedAccount, err := whatsappaccount.LockAndLoadActiveForOutbound(
				tx,
				req.Account.OrganizationID,
				req.Account.ID,
			)
			if err != nil {
				return err
			}
			if err := a.prepareWhatsAppAccountForRuntime(lockedAccount); err != nil {
				return err
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
			result.whatsAppMessageID, result.sendErr = normalizeOutgoingProviderResult(
				sendFn(ctx, contact, lockedAccount),
			)
			return persistOutgoingDeliveryResult(
				tx,
				&stored,
				contact.ID,
				result.whatsAppMessageID,
				result.sendErr,
			)
		},
	)
	result.captureProviderPanic(err)
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

const (
	automaticAIDispatchStateMetadataKey = "automatic_ai_dispatch_state"
	automaticAIDispatchStartedAtKey     = "automatic_ai_dispatch_started_at"
	automaticAIDispatchSettledAtKey     = "automatic_ai_dispatch_settled_at"
	automaticAIDispatchStateDispatching = "dispatching"
	automaticAIDispatchStateResolved    = "resolved"
	automaticAIDispatchStateRejected    = "rejected"
	automaticAIDispatchStateUncertain   = "uncertain"
)

// deliverAutomaticAIOutgoingMessage uses three physical phases:
//
//  1. independently commit the exact pending-message/policy decision;
//  2. invoke Meta while the caller holds the organization NO KEY UPDATE fence
//     and this function holds a fresh shared account-lifecycle lock (no
//     Contact or Message lock crosses the network);
//  3. independently commit the provider result or uncertainty.
//
// The surrounding inbound action ledger is the durable at-most-once identity.
// A committed dispatching marker can never be reclaimed as a fresh attempt.
func (a *App) deliverAutomaticAIOutgoingMessage(
	ctx context.Context,
	msg *models.Message,
	req OutgoingMessageRequest,
	sendFn func(context.Context, *models.Contact, *models.WhatsAppAccount) (string, error),
) (outgoingDeliveryResult, error) {
	var result outgoingDeliveryResult
	if a == nil || a.inboundContinuation == nil ||
		!a.inboundContinuation.attemptGuarded {
		return result, errors.New("automatic AI provider attempt is not fenced")
	}
	if req.Account == nil || req.Contact == nil || sendFn == nil ||
		req.Account.OrganizationID == uuid.Nil {
		return result, errors.New("automatic AI provider attempt identity is incomplete")
	}
	organizationID := req.Account.OrganizationID
	startedAt := time.Now().UTC()

	preflightErr := a.outgoingCanonicalTransaction(
		ctx,
		organizationID,
		func(tx *gorm.DB) error {
			if err := a.validateNativeBookingDispatchTx(tx, msg); err != nil {
				return err
			}
			account, err := whatsappaccount.LockAndLoadActiveForOutbound(
				tx,
				organizationID,
				req.Account.ID,
			)
			if err != nil {
				return err
			}
			if err := a.prepareWhatsAppAccountForRuntime(account); err != nil {
				return err
			}
			contact, err := contactutil.ResolveCanonicalContactForUpdate(
				tx,
				organizationID,
				req.Contact.ID,
			)
			if err != nil {
				return err
			}
			policy, err := database.EvaluateContactAutomaticReplyPolicy(
				tx,
				organizationID,
				contact.ID,
			)
			if err != nil {
				return err
			}
			if !policy.Allowed {
				result.policyErr = &inboundContinuationPolicyStop{
					Reason: policy.Reason,
				}
				return result.policyErr
			}

			var stored models.Message
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
				Where(
					"id = ? AND organization_id = ? AND direction = ?",
					msg.ID,
					organizationID,
					models.DirectionOutgoing,
				).
				First(&stored).Error; err != nil {
				return err
			}
			if stored.Status != models.MessageStatusPending {
				return errors.New("automatic AI message is no longer pending")
			}
			if err := validateNativeBookingMessageIdentity(&stored, msg); err != nil {
				return err
			}
			if state, _ := stored.Metadata[automaticAIDispatchStateMetadataKey].(string); strings.TrimSpace(state) != "" {
				return errors.New(
					"automatic AI message already has a physical attempt",
				)
			}
			metadata := cloneOutgoingMessageMetadata(stored.Metadata)
			if err := a.claimNativeBookingDispatchTx(tx, &stored); err != nil {
				return err
			}
			metadata[automaticAIDispatchStateMetadataKey] =
				automaticAIDispatchStateDispatching
			metadata[automaticAIDispatchStartedAtKey] =
				startedAt.Format(time.RFC3339Nano)
			update := tx.Model(&models.Message{}).
				Where(
					"id = ? AND organization_id = ? AND status = ?",
					stored.ID,
					organizationID,
					models.MessageStatusPending,
				).
				Update("metadata", metadata)
			if update.Error != nil {
				return update.Error
			}
			if update.RowsAffected != 1 {
				return errors.New(
					"automatic AI dispatch claim changed before commit",
				)
			}
			result.contact = *contact
			return nil
		},
	)
	if preflightErr != nil {
		return result, preflightErr
	}

	// Reacquire the account from durable state and retain its shared lock for
	// the complete provider call. The independently committed dispatch marker
	// remains the at-most-once crash boundary, while this second transaction
	// closes the preflight-to-provider gap in which a disconnect or credential
	// rotation could otherwise commit and leave the copied account usable.
	attemptErr := a.outgoingProviderTransaction(
		ctx,
		organizationID,
		func(tx *gorm.DB, providerAttempted *bool) error {
			if err := a.validateNativeBookingDispatchTx(tx, msg); err != nil {
				return err
			}
			account, err := whatsappaccount.LockAndLoadActiveForOutbound(
				tx,
				organizationID,
				req.Account.ID,
			)
			if err != nil {
				return err
			}
			if err := a.prepareWhatsAppAccountForRuntime(account); err != nil {
				return err
			}
			// The provider transaction may have waited after the outer guard and
			// independent dispatch claim. Recheck the worker on this same fresh
			// transaction before starting I/O; settlement remains independent.
			if err := a.requireInboundContinuationOwnershipTx(tx); err != nil {
				return err
			}
			*providerAttempted = true
			result.providerAttempted = true
			result.whatsAppMessageID, result.sendErr = normalizeOutgoingProviderResult(
				sendFn(
					ctx,
					&result.contact,
					account,
				),
			)
			return nil
		},
	)
	result.captureProviderPanic(attemptErr)
	if attemptErr != nil && !result.providerAttempted {
		// Admission failed after the durable attempt was claimed, but before the
		// provider boundary. Settle that known result instead of leaving the
		// message in dispatching or allowing the action ledger to reclaim it.
		result.sendErr = fmt.Errorf(
			"automatic AI provider admission failed: %w",
			attemptErr,
		)
	}

	settlementErr := a.settleAutomaticAIOutgoingMessage(
		context.Background(),
		msg,
		&result,
	)
	if settlementErr != nil {
		// Reconcile only the database result. The provider call above is never
		// repeated after its durable dispatching marker was committed.
		reconcileErr := a.settleAutomaticAIOutgoingMessage(
			context.Background(),
			msg,
			&result,
		)
		if reconcileErr != nil {
			return result, errors.Join(settlementErr, reconcileErr)
		}
	}
	return result, nil
}

func (a *App) settleAutomaticAIOutgoingMessage(
	ctx context.Context,
	pendingMessage *models.Message,
	result *outgoingDeliveryResult,
) error {
	if pendingMessage == nil || result == nil {
		return errors.New("automatic AI settlement identity is incomplete")
	}
	settlementCtx, cancel := context.WithTimeout(
		ctx,
		outgoingDeliveryRecoveryTimeout,
	)
	defer cancel()
	return a.outgoingTenantTransaction(
		settlementCtx,
		pendingMessage.OrganizationID,
		func(tx *gorm.DB) error {
			var stored models.Message
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
				Where(
					"id = ? AND organization_id = ? AND direction = ?",
					pendingMessage.ID,
					pendingMessage.OrganizationID,
					models.DirectionOutgoing,
				).
				First(&stored).Error; err != nil {
				return err
			}
			state, _ := stored.Metadata[automaticAIDispatchStateMetadataKey].(string)
			if stored.Status != models.MessageStatusPending {
				if state == automaticAIDispatchStateResolved ||
					state == automaticAIDispatchStateRejected ||
					state == automaticAIDispatchStateUncertain {
					result.whatsAppMessageID = stored.WhatsAppMessageID
					if stored.Status == models.MessageStatusFailed {
						result.sendErr = errors.New(stored.ErrorMessage)
					}
					return nil
				}
				return errors.New(
					"automatic AI settlement reached an unrelated terminal state",
				)
			}
			if state != automaticAIDispatchStateDispatching {
				return errors.New(
					"automatic AI settlement lost its dispatching authority",
				)
			}
			metadata := cloneOutgoingMessageMetadata(stored.Metadata)
			if !result.providerAttempted {
				metadata[automaticAIDispatchStateMetadataKey] =
					automaticAIDispatchStateRejected
			} else if result.sendErr == nil &&
				strings.TrimSpace(result.whatsAppMessageID) != "" {
				metadata[automaticAIDispatchStateMetadataKey] =
					automaticAIDispatchStateResolved
			} else {
				metadata[automaticAIDispatchStateMetadataKey] =
					automaticAIDispatchStateUncertain
			}
			metadata[automaticAIDispatchSettledAtKey] =
				time.Now().UTC().Format(time.RFC3339Nano)
			update := tx.Model(&models.Message{}).
				Where(
					"id = ? AND organization_id = ? AND status = ?",
					stored.ID,
					stored.OrganizationID,
					models.MessageStatusPending,
				).
				Updates(map[string]any{
					"contact_id":           result.contact.ID,
					"status":               outgoingMessageResultStatus(result.sendErr),
					"whats_app_message_id": result.whatsAppMessageID,
					"error_message":        outgoingMessageResultError(result.sendErr),
					"metadata":             metadata,
				})
			if update.Error != nil {
				return update.Error
			}
			if update.RowsAffected != 1 {
				return errors.New("automatic AI settlement was lost")
			}
			return nil
		},
	)
}

func cloneOutgoingMessageMetadata(source models.JSONB) models.JSONB {
	result := make(models.JSONB, len(source)+3)
	for key, value := range source {
		result[key] = value
	}
	return result
}

func outgoingMessageResultStatus(sendErr error) models.MessageStatus {
	if sendErr != nil {
		return models.MessageStatusFailed
	}
	return models.MessageStatusSent
}

func outgoingMessageResultError(sendErr error) string {
	if sendErr == nil {
		return ""
	}
	return sendErr.Error()
}

func normalizeOutgoingProviderResult(
	whatsAppMessageID string,
	sendErr error,
) (string, error) {
	whatsAppMessageID = strings.TrimSpace(whatsAppMessageID)
	if sendErr == nil && whatsAppMessageID == "" {
		return "", errOutgoingProviderReceiptMissing
	}
	return whatsAppMessageID, sendErr
}

func (result *outgoingDeliveryResult) captureProviderPanic(err error) {
	if result == nil || !result.providerAttempted || result.sendErr != nil ||
		strings.TrimSpace(result.whatsAppMessageID) != "" {
		return
	}
	var panicErr *outgoingProviderPanicError
	if errors.As(err, &panicErr) {
		result.sendErr = panicErr
	}
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
			// Assigning the WAMID makes the message owner trigger take the
			// organization row FOR SHARE NOWAIT. The provider transaction fails
			// there whenever a policy fence (for example a Coexistence admission
			// waiting for the shadow that transaction held across Meta) owns the
			// row. Wait for it here, before any other lock, so the captured result
			// is recorded rather than lost to the same NOWAIT. Residual: the wait
			// is bounded by outgoingDeliveryRecoveryTimeout (10 s), while a fence
			// holder can itself wait up to the 30 s Meta timeout for a shadow that
			// another reply holds across Meta. Recovery then times out and the
			// Meta-accepted reply stays pending without its WAMID, as it did
			// before this wait existed.
			if err := channelapi.LockLegacyMetaOrganization(tx, pendingMessage.OrganizationID); err != nil {
				return err
			}
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
			return containOutgoingProviderPanic(&providerAttempted, func() error {
				return deliver(tx, &providerAttempted)
			})
		})
		if err == nil || providerAttempted || !isRetryableCanonicalContactWrite(err) {
			return err
		}
	}
	return err
}

func containOutgoingProviderPanic(providerAttempted *bool, operation func() error) (resultErr error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			attempted := providerAttempted != nil && *providerAttempted
			resultErr = &outgoingProviderPanicError{
				value:             recovered,
				providerAttempted: attempted,
			}
		}
	}()
	if operation == nil {
		return errors.New("outgoing provider operation is required")
	}
	return operation()
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
// Contact creation in this phase takes the policy fence before contact rows;
// the shared bridge protocol is documented in channel/legacy_meta_fence.go.
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
// provider delivery after that request commits. Automatic AI delivery also
// starts from the root pool so its pending/dispatch/result facts are real
// commits rather than savepoints inside the continuation transaction.
func (a *App) outgoingTenantTransaction(
	ctx context.Context,
	organizationID uuid.UUID,
	write func(tx *gorm.DB) error,
) error {
	root := a.rootApp()
	return a.withBoundedRootConnection(ctx, 0, func(db *gorm.DB) error {
		if root.rlsEnabled() {
			return database.WithTenant(db, organizationID, write)
		}
		return db.Transaction(write)
	})
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

	if req.forwardedFrom != nil {
		if msg.Metadata == nil {
			msg.Metadata = models.JSONB{}
		}
		msg.Metadata[forwardedFromMetadataKey] = req.forwardedFrom.metadata()
		// Durable phase marker: "queued" until the provider phase is recorded.
		msg.Metadata[forwardDispatchMetadataKey] = forwardDispatchQueued
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
		if a.inboundContinuation != nil {
			// H1: markMessagesAsRead's continuation branch (contacts.go:1078-1085), entered
			// through this nested App so its pool wait is bounded. Inside the AI-attempt
			// fence this goroutine holds the organization KEY SHARE that policy writers
			// queue behind while they pin pooled connections, so this best-effort write
			// waits at most 2s there (the unread badge then stays) instead of 45s.
			orgID := req.Account.OrganizationID
			root := a.rootApp()
			readWait := time.Duration(0) // derived: 15s nested
			if a.inboundContinuation.attemptGuarded {
				readWait = 2 * time.Second
			}
			var afterCommit []func()
			// The root pool's own context, as in WithTenantApp (never a nil Context).
			rootCtx := context.Background()
			if root.DB != nil && root.DB.Statement != nil && root.DB.Statement.Context != nil {
				rootCtx = root.DB.Statement.Context
			}
			err := a.withBoundedRootConnection(rootCtx, readWait, func(db *gorm.DB) error {
				if orgID == uuid.Nil {
					return database.ErrMissingTenant
				}
				phase := func(tx *gorm.DB) error {
					if err := requireOrdinaryTenantOrganization(tx, orgID); err != nil {
						return err
					}
					scoped := root.scopedApp(tx, orgID)
					if err := scoped.markMessagesAsReadInCurrentScope(orgID, req.Contact.ID, req.Contact); err != nil {
						return err
					}
					afterCommit = scoped.takeAfterCommit()
					return nil
				}
				if a.rlsEnabled() {
					return database.WithTenant(db, orgID, phase)
				}
				return db.Transaction(phase)
			})
			if err != nil {
				a.Log.Error("Failed to mark independently committed inbound messages as read", "error", err, "organization_id", orgID)
			} else {
				a.runAfterCommit(afterCommit)
			}
		} else {
			a.markMessagesAsRead(req.Account.OrganizationID, req.Contact.ID, req.Contact)
		}
	}
}

// broadcastNewMessage broadcasts a new message via WebSocket
func (a *App) broadcastNewMessage(orgID uuid.UUID, msg *models.Message, contact *models.Contact) {
	if a != nil && a.inboundContinuation != nil && a.inboundContinuation.detachedGeneratedApply {
		if orgID != a.inboundContinuation.OrganizationID {
			a.Log.Error("Detached generated message event has a different organization", "organization_id", orgID)
			return
		}
		// Payload construction reads the organization's masking setting. Enter
		// a bounded tenant scope without retaining a third connection across
		// provider I/O. The history finalizer still owns event publication.
		var afterCommit []func()
		err := a.withInboundContinuationDetachedApplyRead(func(db *gorm.DB) error {
			scoped := a.scopedApp(db, orgID)
			scoped.broadcastNewMessageInCurrentScope(orgID, msg, contact)
			afterCommit = scoped.takeAfterCommit()
			return nil
		})
		if err != nil {
			a.Log.Error("Failed to prepare detached generated message event", "error", err, "organization_id", orgID)
			return
		}
		for _, callback := range afterCommit {
			a.afterTenantCommit(callback)
		}
		return
	}
	a.broadcastNewMessageInCurrentScope(orgID, msg, contact)
}

func (a *App) broadcastNewMessageInCurrentScope(orgID uuid.UUID, msg *models.Message, contact *models.Contact) {
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
		"is_forwarded":     messageIsForwarded(msg.Metadata),
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

// broadcastContactUpdate tells connected clients that a contact's inbox state
// (assignment, transfer agent or manual read state) changed. It reaches every
// socket of the organization, including agents limited to their own chats, so
// the payload is only the contact ID and the kind of change; clients refetch
// the canonical row through their own scoped API. It is queued until the
// surrounding tenant transaction commits and does no database work, so it is
// safe on paths that already hold a transaction.
func (a *App) broadcastContactUpdate(orgID, contactID uuid.UUID, kind string) {
	if a == nil || orgID == uuid.Nil || contactID == uuid.Nil {
		return
	}
	payload := map[string]any{
		"contact_id": contactID.String(),
		"kind":       kind,
	}
	a.afterTenantCommit(func() {
		root := a.rootApp()
		if root == nil || root.WSHub == nil {
			return
		}
		root.WSHub.BroadcastToOrg(orgID, websocket.WSMessage{
			Type:    websocket.TypeContactUpdate,
			Payload: payload,
		})
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
				if err := database.LockOrganizationPolicyScope(tx, orgID); err != nil {
					return err
				}
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
		// Keep raw bytes local to the request here. The unified sender uploads
		// them only after the pending message/receipt mirror is durable and the
		// final account, visibility, and contact-policy checks have succeeded.
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
		MediaData:           headerMediaData,
		MediaURL:            headerMediaPath,
		MediaMimeType:       headerMimeType,
		ButtonURLParams:     buttonParams,
	}

	opts := DefaultSendOptions()
	opts.SentByUserID = &userID

	ctx := context.Background()
	message, err := a.SendOutgoingMessage(ctx, msgReq, opts)
	if err != nil {
		if headerMediaPath != "" {
			if cleanupErr := a.cleanupRejectedMessageMedia(orgID, headerMediaPath); cleanupErr != nil {
				a.Log.Error(
					"Failed to clean unreferenced rejected template media",
					"error", cleanupErr,
					"media_path", headerMediaPath,
					"org_id", orgID,
				)
			}
		}
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

// ============================================================================
// Message forwarding
// ============================================================================

// The WhatsApp Cloud API has no forward call and no settable "Forwarded"
// label, so a forward is a new outbound message built from a stored message of
// the same organization. Text is copied; media bytes are read from tenant
// storage, stored again for the new row, and uploaded to Meta through the
// MediaData path. Inbound Meta media IDs are never reused.

const (
	forwardedFromMetadataKey = "forwarded_from"

	// forwardMediaCopyTimeout bounds the tenant-storage read and write of one
	// forwarded attachment; forwardMediaUploadTimeout bounds its Meta upload.
	// Both run after the request commits, with no transaction open.
	forwardMediaCopyTimeout   = 20 * time.Second
	forwardMediaUploadTimeout = 30 * time.Second

	// forwardRequestTimeout bounds every statement the forward request runs:
	// lookups, the pending-message write and the audit entry.
	forwardRequestTimeout = 15 * time.Second

	// mediaReplyPreviewTimeout bounds the reply-preview reload of a media send.
	mediaReplyPreviewTimeout = 5 * time.Second

	// forwardAccountCheckTimeout bounds the account read before a forwarded
	// attachment's upload; forwardRateLimitTimeout bounds the Redis counter.
	forwardAccountCheckTimeout = 5 * time.Second
	forwardRateLimitTimeout    = 2 * time.Second

	// forwardRateLimitPerMinute caps forwards per user. The UI sends at most
	// five per action, one request each.
	forwardRateLimitPerMinute = 30

	// WhatsApp Cloud API media limits. A larger attachment is refused before
	// its bytes are read, so a delivery goroutine never holds an oversized copy.
	forwardImageMaxBytes      = 5 << 20
	forwardAudioVideoMaxBytes = 16 << 20
	forwardDocumentMaxBytes   = 100 << 20

	forwardErrorInvalid            fastglue.ErrorType = "forward_invalid"
	forwardErrorSourceNotFound     fastglue.ErrorType = "forward_source_not_found"
	forwardErrorUnsupportedType    fastglue.ErrorType = "forward_unsupported_type"
	forwardErrorMediaPending       fastglue.ErrorType = "forward_media_pending"
	forwardErrorWindowClosed       fastglue.ErrorType = "forward_window_closed"
	forwardErrorIdentityReview     fastglue.ErrorType = "forward_identity_review"
	forwardErrorTargetNotFound     fastglue.ErrorType = "forward_target_not_found"
	forwardErrorAccountUnavailable fastglue.ErrorType = "forward_account_unavailable"
	forwardErrorSourceDeleted      fastglue.ErrorType = "forward_source_deleted"
	forwardErrorRateLimited        fastglue.ErrorType = "forward_rate_limited"

	// forwardErrorTemporarilyUnavailable: definitely not accepted (nothing
	// committed, no provider effect); the client may offer a manual retry.
	// forwardErrorOutcomeUnknown: the server cannot prove either way; the
	// client shows "may have been forwarded" and never retries by itself.
	forwardErrorTemporarilyUnavailable fastglue.ErrorType = "forward_temporarily_unavailable"
	forwardErrorOutcomeUnknown         fastglue.ErrorType = "forward_outcome_unknown"
)

// messageForwardSource is the server-derived provenance of a forwarded message.
type messageForwardSource struct {
	MessageID uuid.UUID
	ContactID uuid.UUID
}

func (s *messageForwardSource) metadata() map[string]any {
	return map[string]any{
		"message_id": s.MessageID.String(),
		"contact_id": s.ContactID.String(),
	}
}

// messageIsForwarded reports whether stored metadata carries forward provenance.
func messageIsForwarded(metadata models.JSONB) bool {
	if metadata == nil {
		return false
	}
	value, ok := metadata[forwardedFromMetadataKey]
	return ok && value != nil
}

// forwardableMessageType lists the message types a forward can rebuild from
// stored content. Stickers, locations, contacts, interactive and template
// messages need provider objects or approvals this server cannot replay.
func forwardableMessageType(messageType models.MessageType) bool {
	switch messageType {
	case models.MessageTypeText,
		models.MessageTypeImage,
		models.MessageTypeVideo,
		models.MessageTypeAudio,
		models.MessageTypeDocument:
		return true
	default:
		return false
	}
}

// messageForwardRefusal is a forward the server declines before it persists a
// message or calls Meta.
type messageForwardRefusal struct {
	Status    int
	ErrorType fastglue.ErrorType
	Message   string
}

func (refusal *messageForwardRefusal) send(r *fastglue.Request) error {
	return r.SendErrorEnvelope(refusal.Status, refusal.Message, nil, refusal.ErrorType)
}

// messageForwardAudit is the audit-log payload for one forwarded message. It
// records provenance only, never message content.
type messageForwardAudit struct {
	Operation              string `json:"operation"`
	ForwardedFromMessageID string `json:"forwarded_from_message_id"`
	ForwardedFromContactID string `json:"forwarded_from_contact_id"`
	TargetContactID        string `json:"target_contact_id"`
	MessageType            string `json:"message_type"`
}

// loadForwardSource loads the message to forward and checks that the user may
// read it: same organization, a contact visible to the user, and inside the
// user's current-conversation window. Every miss is the same 404 so a forward
// cannot probe for messages the user cannot see.
func (a *App) loadForwardSource(
	orgID, userID, sourceID uuid.UUID,
) (*models.Message, *messageForwardRefusal, error) {
	notFound := &messageForwardRefusal{
		Status:    fasthttp.StatusNotFound,
		ErrorType: forwardErrorSourceNotFound,
		Message:   "Message to forward not found",
	}
	var source models.Message
	if err := a.DB.Where("id = ? AND organization_id = ?", sourceID, orgID).
		First(&source).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, notFound, nil
		}
		return nil, nil, fmt.Errorf("load forward source: %w", err)
	}

	var visible int64
	visibleQuery := a.scopeAssignedContact(
		a.DB.Model(&models.Contact{}).Where("id = ? AND organization_id = ?", source.ContactID, orgID),
		userID,
		orgID,
	)
	if err := visibleQuery.Count(&visible).Error; err != nil {
		return nil, nil, fmt.Errorf("check forward source visibility: %w", err)
	}
	if visible == 0 {
		return nil, notFound, nil
	}
	restricted, cutoff, err := a.customerConversationScope(orgID, userID, []uuid.UUID{source.ContactID})
	if err != nil {
		return nil, nil, fmt.Errorf("scope forward source conversation: %w", err)
	}
	if (restricted && cutoff == nil) || (cutoff != nil && source.CreatedAt.Before(cutoff.UTC())) {
		return nil, notFound, nil
	}

	if revoked, _ := source.Metadata[coexistenceMediaRevokedMetadataKey].(bool); revoked {
		return nil, &messageForwardRefusal{
			Status:    fasthttp.StatusGone,
			ErrorType: forwardErrorSourceDeleted,
			Message:   "This message was deleted and cannot be forwarded",
		}, nil
	}
	if !forwardableMessageType(source.MessageType) {
		return nil, &messageForwardRefusal{
			Status:    fasthttp.StatusUnprocessableEntity,
			ErrorType: forwardErrorUnsupportedType,
			Message:   fmt.Sprintf("%s messages cannot be forwarded", source.MessageType),
		}, nil
	}
	if source.MessageType == models.MessageTypeText {
		if strings.TrimSpace(source.Content) == "" {
			return nil, &messageForwardRefusal{
				Status:    fasthttp.StatusUnprocessableEntity,
				ErrorType: forwardErrorUnsupportedType,
				Message:   "This message has no text to forward",
			}, nil
		}
		return &source, nil, nil
	}
	if strings.TrimSpace(source.MediaURL) == "" {
		// Coexistence and inbound media are hydrated asynchronously; until the
		// bytes are in tenant storage there is nothing this server may resend.
		return nil, &messageForwardRefusal{
			Status:    fasthttp.StatusConflict,
			ErrorType: forwardErrorMediaPending,
			Message:   "This attachment has not finished downloading yet. Try again shortly.",
		}, nil
	}
	return &source, nil, nil
}

// checkForwardTarget refuses a target contact that cannot receive a free-form
// message now. Meta reports a closed customer-service window (131047) only
// asynchronously, after the message was accepted, so the window is checked
// before anything is persisted.
func (a *App) checkForwardTarget(orgID uuid.UUID, contact *models.Contact, now time.Time) *messageForwardRefusal {
	return a.checkForwardTargetTx(a.DB, orgID, contact, now)
}

// checkForwardTargetTx is checkForwardTarget on an explicit session, so the
// send path can repeat it on the canonical contact under its row lock.
func (a *App) checkForwardTargetTx(db *gorm.DB, orgID uuid.UUID, contact *models.Contact, now time.Time) *messageForwardRefusal {
	state, err := a.GetWhatsAppIdentityReviewEffectiveState(db, orgID, contact.ID)
	if err != nil {
		// Fail closed, but say what happened: nothing was sent.
		a.Log.Error("Failed to read identity-review state for forward target",
			"error", err, "organization_id", orgID, "contact_id", contact.ID)
		return forwardNotAcceptedRefusal()
	}
	if state.Blocked {
		return &messageForwardRefusal{
			Status:    fasthttp.StatusConflict,
			ErrorType: forwardErrorIdentityReview,
			Message:   "This contact's WhatsApp identity is under review. Resolve the review before forwarding to it.",
		}
	}
	if contact.LastInboundAt == nil || now.Sub(*contact.LastInboundAt) >= 24*time.Hour {
		return &messageForwardRefusal{
			Status:    fasthttp.StatusConflict,
			ErrorType: forwardErrorWindowClosed,
			Message:   "The 24-hour service window for this contact is closed. Send an approved template instead.",
		}
	}
	return nil
}

// forwardMediaCopy is the deferred copy of a forwarded attachment: read
// SourceKey, write the new message's own object, then upload the bytes to Meta.
// DestinationKey is planned in the request (no I/O there) and stored as the
// new row's media_url, so the copy needs no later database write.
type forwardMediaCopy struct {
	OrganizationID uuid.UUID
	SourceKey      string
	ObjectSubdir   string
	LocalSubdir    string
	ObjectName     string
	DestinationKey string
	MimeType       string
}

// planForwardMedia names the forwarded attachment's own object with the same
// layout saveMessageMedia uses, without touching storage.
func (a *App) planForwardMedia(orgID uuid.UUID, source *models.Message) *forwardMediaCopy {
	mimeType := strings.TrimSpace(source.MediaMimeType)
	if mimeType == "" {
		mimeType = getMimeTypeFromExtension(forwardMediaExtension(source.MediaURL))
	}
	subdir := "documents"
	switch {
	case strings.HasPrefix(mimeType, "image/"):
		subdir = "images"
	case strings.HasPrefix(mimeType, "video/"):
		subdir = "videos"
	case strings.HasPrefix(mimeType, "audio/"):
		subdir = "audio"
	}
	ext := getExtensionFromMimeType(mimeType)
	if ext == "" {
		ext = forwardMediaExtension(source.MediaFilename)
	}
	if ext == "" {
		ext = ".bin"
	}
	plan := &forwardMediaCopy{
		OrganizationID: orgID,
		SourceKey:      source.MediaURL,
		ObjectSubdir:   "messages/" + subdir,
		LocalSubdir:    subdir,
		ObjectName:     uuid.New().String() + ext,
		MimeType:       mimeType,
	}
	if a.Config != nil && strings.EqualFold(a.Config.Storage.Type, "s3") {
		plan.DestinationKey = tenantObjectKey(orgID, plan.ObjectSubdir, plan.ObjectName)
	} else {
		plan.DestinationKey = plan.LocalSubdir + "/" + plan.ObjectName
	}
	return plan
}

func forwardMediaExtension(name string) string {
	dot := strings.LastIndex(name, ".")
	if dot < 0 || strings.ContainsAny(name[dot:], "/\\") {
		return ""
	}
	return strings.ToLower(name[dot:])
}

// outgoingSessionContext is the context of a request session, so a derived deadline
// keeps the tenant and realtime values bound to it.
func outgoingSessionContext(db *gorm.DB) context.Context {
	if db != nil && db.Statement != nil && db.Statement.Context != nil {
		return db.Statement.Context
	}
	return context.Background()
}

// forwardSessionApp is a read view of this request's session under ctx: the
// same connection (the RLS request transaction when there is one), with a
// deadline on every statement. The request App itself is left untouched, so
// delivery started after commit never sees the deadline.
func (a *App) forwardSessionApp(ctx context.Context) *App {
	session := a.rootApp().scopedApp(a.DB.WithContext(ctx), a.tenantOrgID)
	session.tenantOrgID = a.tenantOrgID
	session.afterCommitScoped = a.afterCommitScoped
	return session
}

// forwardMessage serves POST /api/contacts/{id}/messages with
// forward_of_message_id: {id} is the target contact. SendMessage has already
// checked chat:write and parsed the body. One request forwards to one target;
// the UI sends at most five, one by one, and reports each result.
func (a *App) forwardMessage(
	r *fastglue.Request,
	orgID, userID, targetContactID uuid.UUID,
	req *SendMessageRequest,
) error {
	// This handler opens no transaction. Under RLS the Tenant route wrapper
	// already owns one request transaction; every statement below runs on that
	// one connection under one deadline, and storage or Meta I/O happens only
	// after commit, in the delivery goroutine.
	ctx, cancel := context.WithTimeout(outgoingSessionContext(a.DB), forwardRequestTimeout)
	defer cancel()
	session := a.forwardSessionApp(ctx)

	if !session.HasPermission(userID, models.ResourceChat, models.ActionRead, orgID) {
		return r.SendErrorEnvelope(fasthttp.StatusForbidden, "You don't have permission to view messages", nil, "")
	}
	if !a.allowForward(orgID, userID) {
		return r.SendErrorEnvelope(fasthttp.StatusTooManyRequests,
			"Too many forwards in the last minute. Wait a moment and try again.", nil, forwardErrorRateLimited)
	}
	sourceID, err := uuid.Parse(strings.TrimSpace(req.ForwardOfMessageID))
	if err != nil || sourceID == uuid.Nil {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid forward message ID", nil, forwardErrorInvalid)
	}
	if strings.TrimSpace(req.ReplyToMessageID) != "" ||
		req.Interactive != nil ||
		strings.TrimSpace(req.Content.Body) != "" {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest,
			"A forward cannot also carry a reply, new text, or interactive content", nil, forwardErrorInvalid)
	}

	// Every failure before SendOutgoingMessage is definite: nothing was
	// persisted and nothing was sent.
	source, refusal, err := session.loadForwardSource(orgID, userID, sourceID)
	if err != nil {
		a.Log.Error("Failed to load message to forward", "error", err, "organization_id", orgID)
		return forwardNotAcceptedRefusal().send(r)
	}
	if refusal != nil {
		return refusal.send(r)
	}

	var contact models.Contact
	targetQuery := session.scopeAssignedContact(
		session.DB.Where("id = ? AND organization_id = ?", targetContactID, orgID),
		userID,
		orgID,
	)
	if err := targetQuery.First(&contact).Error; err != nil {
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			a.Log.Error("Failed to load forward target", "error", err, "organization_id", orgID)
			return forwardNotAcceptedRefusal().send(r)
		}
		return r.SendErrorEnvelope(fasthttp.StatusNotFound, "Contact not found", nil, forwardErrorTargetNotFound)
	}
	if refusal := session.checkForwardTarget(orgID, &contact, time.Now()); refusal != nil {
		return refusal.send(r)
	}

	accountName := contact.WhatsAppAccount
	if req.WhatsAppAccount != "" {
		accountName = req.WhatsAppAccount
	}
	account, refusal := session.resolveForwardAccount(orgID, accountName)
	if refusal != nil {
		return refusal.send(r)
	}
	if a.rlsEnabled() && a.hasTenantScope() && !a.usesCurrentTenantPreProvider(account.OrganizationID) {
		// Would fail the same way every time: not a "try again" condition.
		return forwardSendFailedRefusal().send(r)
	}

	msgReq := OutgoingMessageRequest{
		Account: account,
		Contact: &contact,
		Type:    source.MessageType,
		forwardedFrom: &messageForwardSource{
			MessageID: source.ID,
			ContactID: source.ContactID,
		},
	}
	if source.MessageType == models.MessageTypeText {
		msgReq.Content = source.Content
	} else {
		plan := a.planForwardMedia(orgID, source)
		msgReq.forwardMedia = plan
		msgReq.MediaURL = plan.DestinationKey
		msgReq.MediaMimeType = plan.MimeType
		msgReq.MediaFilename = source.MediaFilename
		if source.MessageType != models.MessageTypeAudio {
			msgReq.Caption = source.Content
		}
	}

	// The failure contract below depends on this exact call: Async=true, so
	// the pending row is written by outgoingPreProviderTransaction and provider
	// delivery is only registered (startAsyncDelivery) after it returns.
	opts := DefaultSendOptions()
	opts.SentByUserID = &userID
	outcome := &forwardSendOutcome{}
	msgReq.forwardOutcome = outcome

	message, err := a.SendOutgoingMessage(ctx, msgReq, opts)
	if err != nil {
		if errors.Is(err, errForwardCapacity) {
			return forwardNotAcceptedRefusal().send(r)
		}
		var refused *forwardTargetRefusedError
		if errors.As(err, &refused) {
			return refused.refusal.send(r)
		}
		a.Log.Error("Failed to forward message", "error", err, "organization_id", orgID, "source_message_id", source.ID)
		if errors.Is(err, errOutgoingContactNotFound) ||
			errors.Is(err, errOutgoingContactAccessRevoked) {
			return r.SendErrorEnvelope(fasthttp.StatusNotFound, "Contact not found", nil, forwardErrorTargetNotFound)
		}
		return forwardSendFailureRefusal(err, opts.Async, *outcome).send(r)
	}

	return r.SendEnvelope(MessageResponse{
		ID:              message.ID,
		ContactID:       message.ContactID,
		Direction:       message.Direction,
		MessageType:     message.MessageType,
		Content:         map[string]string{"body": message.Content},
		MediaURL:        message.MediaURL,
		MediaMimeType:   message.MediaMimeType,
		MediaFilename:   message.MediaFilename,
		Status:          message.Status,
		WhatsAppAccount: message.WhatsAppAccount,
		IngestedAt:      message.IngestedAt,
		CreatedAt:       message.CreatedAt,
		UpdatedAt:       message.UpdatedAt,
		IsForwarded:     true,
		ForwardDispatch: forwardDispatchQueued,
	})
}

// attachMediaReplyPreview fills the reply fields of a media-send response. The
// unified sender already validated the reply; a failed reload only drops the
// preview and never undoes the send.
func (a *App) attachMediaReplyPreview(orgID uuid.UUID, message *models.Message, response *MessageResponse) {
	if message == nil || response == nil || !message.IsReply || message.ReplyToMessageID == nil {
		return
	}
	response.IsReply = true
	replyToID := message.ReplyToMessageID.String()
	response.ReplyToMessageID = &replyToID
	ctx, cancel := context.WithTimeout(outgoingSessionContext(a.DB), mediaReplyPreviewTimeout)
	defer cancel()
	var reply models.Message
	if err := a.DB.WithContext(ctx).Where(
		"id = ? AND organization_id = ? AND contact_id = ?",
		*message.ReplyToMessageID,
		orgID,
		message.ContactID,
	).First(&reply).Error; err != nil {
		a.Log.Error("Failed to reload validated reply message", "error", err, "message_id", message.ID)
		return
	}
	response.ReplyToMessage = &ReplyPreview{
		ID:          reply.ID.String(),
		Content:     map[string]string{"body": reply.Content},
		MessageType: reply.MessageType,
		Direction:   reply.Direction,
	}
}

// forwardTargetRefusedError carries a target refusal found under the
// canonical-contact lock back to the forward handler.
type forwardTargetRefusedError struct {
	refusal *messageForwardRefusal
}

func (e *forwardTargetRefusedError) Error() string {
	if e == nil || e.refusal == nil {
		return "forward target refused"
	}
	return e.refusal.Message
}

// recordForwardAudit writes the per-target forward audit entry on the
// pending-message transaction: provenance ids only, never content.
func recordForwardAudit(
	tx *gorm.DB,
	orgID uuid.UUID,
	userID *uuid.UUID,
	message *models.Message,
	source *messageForwardSource,
) error {
	if userID == nil || *userID == uuid.Nil || message == nil || source == nil {
		return &forwardAuditError{err: errors.New("forward audit requires the forwarding user and message")}
	}
	if err := logAuditStrict(tx, orgID, *userID, "message", message.ID, models.AuditActionCreated, nil, messageForwardAudit{
		Operation:              "forwarded",
		ForwardedFromMessageID: source.MessageID.String(),
		ForwardedFromContactID: source.ContactID.String(),
		TargetContactID:        message.ContactID.String(),
		MessageType:            string(message.MessageType),
	}); err != nil {
		if forwardFailureRepeats(err) {
			return &forwardAuditError{err: err}
		}
		// Transient (deadline, cancellation, dropped connection, retryable
		// SQLSTATE): classified by phase, so the client may try again.
		return fmt.Errorf("record forward audit: %w", err)
	}
	return nil
}

// forwardFailureRepeats reports whether PostgreSQL rejected a forward's write
// in a way that would repeat on a retry: a data error (22), an integrity
// constraint (23), a syntax, privilege or row-level-security policy error
// (42), or an exception raised by a trigger or guard function (P0). Anything
// else, such as a deadline, a cancellation (57014), a dropped connection, or a
// deadlock or lock timeout left after retries (40P01, 55P03), is transient.
func forwardFailureRepeats(err error) bool {
	code := postgresErrorCode(err)
	if len(code) != 5 {
		return false
	}
	switch code[:2] {
	case "22", "23", "42", "P0":
		return true
	default:
		return false
	}
}

// requireForwardAccountActive re-reads the sending account in one short
// committed transaction before a forwarded attachment's upload, so a
// disconnected account is not used; no transaction is held during the upload.
// The provider transaction re-checks under its account lock before the send.
func (a *App) requireForwardAccountActive(orgID, accountID uuid.UUID) error {
	var account models.WhatsAppAccount
	err := a.withForwardTenantTx(orgID, func(tx *gorm.DB) error {
		return tx.Select("id", "status").
			Where("id = ? AND organization_id = ?", accountID, orgID).
			First(&account).Error
	})
	if err != nil {
		a.Log.Error("Failed to check WhatsApp account before a forwarded upload",
			"error", err, "organization_id", orgID)
		return forwardServerBusyError()
	}
	return whatsappaccount.RequireActiveForOutbound(&account)
}

// withForwardTenantTx runs one short committed tenant transaction for the
// delivery goroutine, which holds no other connection. The deadline covers the
// pool acquisition, BEGIN, the tenant binding and the statements.
func (a *App) withForwardTenantTx(orgID uuid.UUID, fn func(tx *gorm.DB) error) error {
	ctx, cancel := context.WithTimeout(context.Background(), forwardAccountCheckTimeout)
	defer cancel()
	root := a.rootApp()
	if root == nil || root.DB == nil {
		return errors.New("database connection is required")
	}
	run := func(tx *gorm.DB) error {
		if err := requireOrdinaryTenantOrganization(tx, orgID); err != nil {
			return err
		}
		return fn(tx)
	}
	db := root.DB.WithContext(ctx)
	if root.rlsEnabled() {
		return database.WithTenant(db, orgID, run)
	}
	return db.Transaction(run)
}

// allowForward counts a user's forwards per minute in Redis. Without Redis,
// or if Redis fails, it allows the forward: the limit is a brake on misuse,
// and every forward is still individually checked and audited.
func (a *App) allowForward(orgID, userID uuid.UUID) bool {
	root := a.rootApp()
	if root == nil || root.Redis == nil {
		return true
	}
	ctx, cancel := context.WithTimeout(context.Background(), forwardRateLimitTimeout)
	defer cancel()
	key := fmt.Sprintf("rereply:forward_rate:%s:%s:%d", orgID, userID, time.Now().Unix()/60)
	count, err := root.Redis.Incr(ctx, key).Result()
	if err != nil {
		a.Log.Warn("Forward rate limit unavailable", "error", err, "organization_id", orgID)
		return true
	}
	if count == 1 {
		_ = root.Redis.Expire(ctx, key, 2*time.Minute).Err()
	}
	return count <= forwardRateLimitPerMinute
}

// forwardMediaSizeLimit is the WhatsApp Cloud API limit for a media type.
func forwardMediaSizeLimit(messageType models.MessageType) int64 {
	switch messageType {
	case models.MessageTypeImage:
		return forwardImageMaxBytes
	case models.MessageTypeAudio, models.MessageTypeVideo:
		return forwardAudioVideoMaxBytes
	default:
		return forwardDocumentMaxBytes
	}
}

func forwardTooLargeError(limit int64) error {
	return fmt.Errorf("this attachment is larger than WhatsApp allows (%d MB) and cannot be forwarded", limit>>20)
}

// ---- Forward capacity contract and dispatch phases -------------------------

const (
	forwardDispatchMetadataKey   = "forward_dispatch"
	forwardDispatchAtMetadataKey = "forward_dispatch_at"
	forwardDispatchQueued        = "queued"
	forwardDispatchProvider      = "provider"

	// Each delivery phase has its own deadline, but nothing bounds the time
	// before a phase starts or a paused process. Past this age a pending
	// forward is shown as stalled, never as "not sent".
	forwardDeliveryStalledAfter = 3 * time.Minute

	forwardDeliveryDelayed   = "delayed"
	forwardDeliveryUncertain = "uncertain"
)

// forwardNotAcceptedRefusal is a definite pre-acceptance failure: nothing was
// committed and nothing reached Meta, so a manual retry is safe.
func forwardNotAcceptedRefusal() *messageForwardRefusal {
	return &messageForwardRefusal{
		Status:    fasthttp.StatusServiceUnavailable,
		ErrorType: forwardErrorTemporarilyUnavailable,
		Message:   "The server is busy, so this forward was not sent. Try again in a moment.",
	}
}

// forwardSendOutcome is the forward call path's typed phase record, filled by
// SendOutgoingMessage, so a failure is classified by where it happened instead
// of by error text.
type forwardSendOutcome struct {
	// writeRequested: the call passed its up-front checks and asked for the
	// pending-row write. A failure before that is an input or invariant
	// rejection, which would repeat.
	writeRequested bool
	// requestTransaction: the pending row is a savepoint of the caller's RLS
	// Tenant request transaction (asynchronous send).
	requestTransaction bool
	// writeStarted: the pre-provider write began on an acquired connection.
	writeStarted bool
	// reachedCommit: the last write attempt returned nil, so its transaction
	// went on to COMMIT. Before that, an error comes from a statement inside
	// the write; GORM then rolls the transaction back and never sends COMMIT,
	// so nothing was committed. After it, an error comes from COMMIT itself or
	// from anything after COMMIT was sent (a dropped connection, a context
	// cancelled during COMMIT): the row may have committed. Reset per retry.
	reachedCommit bool
}

// forwardAuditError is a per-target forward audit failure that would repeat
// (a missing actor or message, or a permanent SQLSTATE class; see
// forwardFailureRepeats). Transient audit failures are not wrapped.
type forwardAuditError struct {
	err error
}

func (e *forwardAuditError) Error() string {
	if e == nil || e.err == nil {
		return "record forward audit"
	}
	return "record forward audit: " + e.err.Error()
}

func (e *forwardAuditError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.err
}

// forwardSendFailedRefusal is a definite failure that would repeat (the
// audit cannot be written, an up-front check, a tenant-scope mismatch, a
// permanent database rejection before COMMIT). It is
// the response an ordinary send gives for a failed send; it never says "try
// again", and the client never retries a 500 by itself.
func forwardSendFailedRefusal() *messageForwardRefusal {
	return &messageForwardRefusal{
		Status:  fasthttp.StatusInternalServerError,
		Message: "Failed to send message",
	}
}

// forwardServerBusyError is the stored, user-facing reason for a forward the
// delivery worker gave up before the provider because the server was busy
// (raw database errors are logged, never stored).
func forwardServerBusyError() error {
	return errors.New("Not sent: the server was too busy to send this forward. Forward it again from the original message.") //nolint:staticcheck // ST1005: persisted user-facing sentence, displayed verbatim with punctuation.
}

// forwardAttachmentCopyError is the stored, user-facing reason for a
// forwarded attachment that could not be copied before the upload.
func forwardAttachmentCopyError() error {
	return errors.New("Not sent: the attachment could not be copied for this forward. Forward it again from the original message.") //nolint:staticcheck // ST1005: persisted user-facing sentence, displayed verbatim with punctuation.
}

func forwardOutcomeUnknownRefusal() *messageForwardRefusal {
	return &messageForwardRefusal{
		Status:    fasthttp.StatusInternalServerError,
		ErrorType: forwardErrorOutcomeUnknown,
		Message:   "The server could not confirm this forward. It may have been sent: check the chat before forwarding again.",
	}
}

// forwardAccountUnavailableRefusal: no usable WhatsApp account (missing,
// inactive or without valid credentials). Definite and non-retryable; the
// same status and message an ordinary send returns.
func forwardAccountUnavailableRefusal() *messageForwardRefusal {
	return &messageForwardRefusal{
		Status:    fasthttp.StatusBadRequest,
		ErrorType: forwardErrorAccountUnavailable,
		Message:   "Failed to resolve WhatsApp account",
	}
}

// forwardSendFailureRefusal maps an error from the forward handler's own
// SendOutgoingMessage call to the client contract. It is valid only for that
// call (asynchronous, DefaultSendOptions), whose invariants are:
//   - provider delivery is registered (startAsyncDelivery) only after the
//     pending-row write returns, so an error means no provider effect;
//   - inside an RLS Tenant request, the pending row is a savepoint of the
//     request transaction, and Tenant rolls an error response back.
//
// Failures that would repeat are definite and non-retryable, with the codes
// an ordinary send returns: an inactive account, the audit write, an up-front
// invariant, or a statement PostgreSQL rejected permanently before COMMIT (a
// constraint, a policy, a guard trigger; see forwardFailureRepeats). Other
// definite failures are 503 "try again". Outside a request transaction, any
// error once the write reached COMMIT (from COMMIT itself, or after COMMIT was
// sent) is ambiguous, whatever its SQLSTATE: 500 forward_outcome_unknown.
func forwardSendFailureRefusal(err error, async bool, outcome forwardSendOutcome) *messageForwardRefusal {
	var auditErr *forwardAuditError
	switch {
	case err == nil:
		return nil
	case errors.Is(err, whatsappaccount.ErrOutboundInactive):
		return forwardAccountUnavailableRefusal()
	case errors.As(err, &auditErr), !outcome.writeRequested:
		return forwardSendFailedRefusal()
	case !async:
		return forwardOutcomeUnknownRefusal()
	case outcome.requestTransaction, !outcome.writeStarted, !outcome.reachedCommit:
		if forwardFailureRepeats(err) {
			return forwardSendFailedRefusal()
		}
		return forwardNotAcceptedRefusal()
	default:
		return forwardOutcomeUnknownRefusal()
	}
}

// claimForwardProviderPhase is the single-use queued -> provider transition,
// committed in its own short transaction before the provider transaction
// opens. It reports claimed only when exactly one row moved, i.e. the forward
// was still pending and still queued.
//
// Re-entry policy: a second claim, a settled row or a missing row moves
// nothing, and the caller must not call the provider or touch the row. Any
// error (a *forwardClaimError) stops the caller before the provider.
func (a *App) claimForwardProviderPhase(orgID, messageID uuid.UUID) (bool, error) {
	var moved int64
	reachedCommit := false
	err := a.withForwardTenantTx(orgID, func(tx *gorm.DB) error {
		result := tx.Model(&models.Message{}).
			Where(
				"organization_id = ? AND id = ? AND status = ? AND metadata ->> ? = ?",
				orgID, messageID, models.MessageStatusPending,
				forwardDispatchMetadataKey, forwardDispatchQueued,
			).
			Update("metadata", gorm.Expr(
				"COALESCE(metadata, '{}'::jsonb) || jsonb_build_object(?::text, ?::text, ?::text, ?::text)",
				forwardDispatchMetadataKey, forwardDispatchProvider,
				forwardDispatchAtMetadataKey, time.Now().UTC().Format(time.RFC3339Nano),
			))
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected > 1 {
			return fmt.Errorf("forward dispatch claim moved %d rows", result.RowsAffected)
		}
		moved = result.RowsAffected
		reachedCommit = true
		return nil
	})
	if err != nil {
		return false, &forwardClaimError{reachedCommit: reachedCommit, err: err}
	}
	return moved == 1, nil
}

// forwardClaimError is a failed provider-phase claim. Without reachedCommit it
// failed before or inside its UPDATE (pool acquisition, BEGIN, tenant binding,
// the statement), so the marker certainly did not commit. With it, COMMIT
// failed or its answer was lost, so the marker may have committed.
type forwardClaimError struct {
	reachedCommit bool
	err           error
}

func (e *forwardClaimError) Error() string {
	if e == nil || e.err == nil {
		return "claim forward provider phase"
	}
	return "claim forward provider phase: " + e.err.Error()
}

func (e *forwardClaimError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.err
}

// forwardDeliveryState derives, read-only, what a stalled forward means for the
// people reading the chat. Neither state claims the message was not sent, and
// nothing ever resends it automatically:
//   - "delayed": still "queued" (no provider phase recorded) long after it
//     should have been sent. Its worker may be dead or merely paused; a paused
//     worker can still send it later. Status unknown.
//   - "uncertain": the provider phase was recorded but no result was. It may
//     have been sent.
//
// A settled (sent/failed) or recent forward has no derived state.
func forwardDeliveryState(message *models.Message, now time.Time) string {
	if message == nil || message.Status != models.MessageStatusPending || !messageIsForwarded(message.Metadata) {
		return ""
	}
	phase, _ := message.Metadata[forwardDispatchMetadataKey].(string)
	switch phase {
	case forwardDispatchQueued:
		if now.Sub(message.CreatedAt) > forwardDeliveryStalledAfter {
			return forwardDeliveryDelayed
		}
	case forwardDispatchProvider:
		since := message.UpdatedAt
		if raw, ok := message.Metadata[forwardDispatchAtMetadataKey].(string); ok {
			if at, err := time.Parse(time.RFC3339Nano, raw); err == nil {
				since = at
			}
		}
		if now.Sub(since) > forwardDeliveryStalledAfter {
			return forwardDeliveryUncertain
		}
	}
	return ""
}

// resolveForwardAccount mirrors resolveWhatsAppAccount for the forward
// handler, keeping the cause: a missing or unusable account is a definite,
// non-retryable refusal; a failed lookup (database or deadline) is a definite
// 503, because nothing has been persisted yet.
func (a *App) resolveForwardAccount(orgID uuid.UUID, accountName string) (*models.WhatsAppAccount, *messageForwardRefusal) {
	var account models.WhatsAppAccount
	var err error
	if accountName != "" {
		err = a.DB.Where("name = ? AND organization_id = ?", accountName, orgID).First(&account).Error
	} else {
		err = a.DB.Where("organization_id = ? AND is_default_outgoing = ?", orgID, true).First(&account).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			err = a.DB.Where("organization_id = ?", orgID).First(&account).Error
		}
	}
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, forwardAccountUnavailableRefusal()
		}
		a.Log.Error("Failed to resolve WhatsApp account for forward", "error", err, "organization_id", orgID)
		return nil, forwardNotAcceptedRefusal()
	}
	if account.OrganizationID != orgID {
		return nil, forwardSendFailedRefusal()
	}
	if err := a.prepareWhatsAppAccountForOutbound(&account); err != nil {
		a.Log.Warn("WhatsApp account cannot send a forward", "error", err, "organization_id", orgID, "account", account.Name)
		return nil, forwardAccountUnavailableRefusal()
	}
	return &account, nil
}
