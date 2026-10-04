package handlers

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/contactutil"
	appcrypto "github.com/shridarpatil/whatomate/internal/crypto"
	"github.com/shridarpatil/whatomate/internal/database"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/internal/queue"
	"github.com/shridarpatil/whatomate/internal/websocket"
	"github.com/valyala/fasthttp"
	"github.com/zerodha/fastglue"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// WebhookVerify handles Meta's webhook verification challenge
func (a *App) WebhookVerify(r *fastglue.Request) error {
	mode := string(r.RequestCtx.QueryArgs().Peek("hub.mode"))
	token := string(r.RequestCtx.QueryArgs().Peek("hub.verify_token"))
	challenge := string(r.RequestCtx.QueryArgs().Peek("hub.challenge"))
	workspaceSelector := strings.TrimSpace(string(r.RequestCtx.QueryArgs().Peek(metaWebhookCallbackWorkspaceQueryKey)))

	if mode != "subscribe" || token == "" || challenge == "" {
		a.Log.Warn("Webhook verification failed - invalid mode", "mode", mode)
		return r.SendErrorEnvelope(fasthttp.StatusForbidden, "Verification failed", nil, "")
	}

	// Workspace-managed credentials are encrypted with randomized nonces, so
	// the public callback includes a non-secret workspace selector. The selector
	// establishes the tenant boundary before any credential is loaded.
	if workspaceSelector != "" {
		organizationID, err := uuid.Parse(workspaceSelector)
		if err == nil {
			managedToken, authoritative, resolveErr := a.resolveMetaWebhookVerifyToken(organizationID)
			if resolveErr == nil && authoritative && constantTimeWebhookTokenEqual(token, managedToken) {
				a.Log.Info("Webhook verified successfully (workspace credential)", "organization_id", organizationID)
				r.RequestCtx.SetStatusCode(fasthttp.StatusOK)
				r.RequestCtx.SetBodyString(challenge)
				return nil
			}
			if resolveErr == nil && !authoritative && a.verifyLegacyWebhookTokenForOrganization(organizationID, token) {
				a.Log.Info("Webhook verified successfully (legacy account fallback)", "organization_id", organizationID)
				r.RequestCtx.SetStatusCode(fasthttp.StatusOK)
				r.RequestCtx.SetBodyString(challenge)
				return nil
			}
		}
		a.Log.Warn("Webhook verification failed - workspace credential mismatch")
		return r.SendErrorEnvelope(fasthttp.StatusForbidden, "Verification failed", nil, "")
	}

	// Preserve the historical platform-wide callback while deployments migrate
	// to workspace-specific URLs. This is configuration fallback only; the
	// credential is never returned by an API.
	if a != nil && a.Config != nil && constantTimeWebhookTokenEqual(token, a.Config.WhatsApp.WebhookVerifyToken) {
		a.Log.Info("Webhook verified successfully (platform fallback)")
		r.RequestCtx.SetStatusCode(fasthttp.StatusOK)
		r.RequestCtx.SetBodyString(challenge)
		return nil
	}

	// Legacy callbacks have no selector. Resolve their tenant using the existing
	// account column, then verify inside that tenant. Once a central workspace
	// credential exists (or Meta is disabled), it is authoritative and the
	// account token can no longer authorize this callback.
	organizationID, resolveErr := a.resolveWebhookOrganization(token)
	if resolveErr == nil {
		_, authoritative, managedErr := a.resolveMetaWebhookVerifyToken(organizationID)
		if managedErr == nil && !authoritative && a.verifyLegacyWebhookTokenForOrganization(organizationID, token) {
			a.Log.Info("Webhook verified successfully (legacy account fallback)", "organization_id", organizationID)
			r.RequestCtx.SetStatusCode(fasthttp.StatusOK)
			r.RequestCtx.SetBodyString(challenge)
			return nil
		}
	}

	a.Log.Warn("Webhook verification failed - token not found")
	return r.SendErrorEnvelope(fasthttp.StatusForbidden, "Verification failed", nil, "")
}

func constantTimeWebhookTokenEqual(left, right string) bool {
	if left == "" || right == "" || len(left) != len(right) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(left), []byte(right)) == 1
}

// resolveMetaWebhookVerifyToken returns the effective workspace/platform
// credential and whether it is authoritative over legacy account-column
// values. A disabled managed integration and an undecryptable central value
// both fail closed without revealing credential state to the public caller.
func (a *App) resolveMetaWebhookVerifyToken(orgID uuid.UUID) (token string, authoritative bool, err error) {
	if orgID == uuid.Nil {
		return "", false, gorm.ErrRecordNotFound
	}
	err = a.WithTenantApp(orgID, func(scoped *App) error {
		var organization models.Organization
		if queryErr := scoped.DB.Where("id = ?", orgID).First(&organization).Error; queryErr != nil {
			return queryErr
		}

		var integration models.ProviderIntegration
		rowErr := scoped.DB.Select("enabled").
			Where("organization_id = ? AND provider = ?", orgID, integrationProviderMeta).
			First(&integration).Error
		if rowErr == nil && !integration.Enabled {
			authoritative = true
			return errMetaIntegrationDisabled
		}
		if rowErr != nil && !errors.Is(rowErr, gorm.ErrRecordNotFound) {
			return rowErr
		}

		workspaceManaged := metaWorkspaceAppManaged(&organization)
		if workspaceManaged && organization.Settings != nil {
			stored, _ := organization.Settings[metaWebhookVerifyTokenSetting].(string)
			stored = strings.TrimSpace(stored)
			if stored != "" {
				authoritative = true
				if !appcrypto.IsEncrypted(stored) || !scoped.hasIntegrationEncryptionKey() {
					return errors.New("managed Meta webhook credential is unavailable")
				}
				decrypted, decryptErr := appcrypto.Decrypt(stored, scoped.integrationEncryptionKey())
				if decryptErr != nil || strings.TrimSpace(decrypted) == "" {
					return errors.New("managed Meta webhook credential is unavailable")
				}
				token = strings.TrimSpace(decrypted)
				return nil
			}
		}

		if !workspaceManaged && scoped.Config != nil && strings.TrimSpace(scoped.Config.WhatsApp.WebhookVerifyToken) != "" {
			authoritative = true
			token = strings.TrimSpace(scoped.Config.WhatsApp.WebhookVerifyToken)
		}
		return nil
	})
	return token, authoritative, err
}

func (a *App) verifyLegacyWebhookTokenForOrganization(orgID uuid.UUID, token string) bool {
	if orgID == uuid.Nil || token == "" {
		return false
	}
	var count int64
	err := a.WithTenantApp(orgID, func(scoped *App) error {
		return scoped.DB.Model(&models.WhatsAppAccount{}).
			Where("organization_id = ? AND webhook_verify_token = ?", orgID, token).
			Limit(1).
			Count(&count).Error
	})
	return err == nil && count > 0
}

// WebhookStatusError represents an error in a status update
type WebhookStatusError struct {
	Code      int    `json:"code"`
	Title     string `json:"title"`
	Message   string `json:"message"`
	ErrorData struct {
		Details string `json:"details"`
	} `json:"error_data"`
}

// TemplateStatusUpdate represents a template status update from Meta webhook
type TemplateStatusUpdate struct {
	Event                   string `json:"event"`
	MessageTemplateID       int64  `json:"message_template_id"`
	MessageTemplateName     string `json:"message_template_name"`
	MessageTemplateLanguage string `json:"message_template_language"`
	Reason                  string `json:"reason,omitempty"`
}

// WebhookStatus represents a message status update from Meta
type WebhookStatus struct {
	ID           string `json:"id"`
	Status       string `json:"status"`
	Timestamp    string `json:"timestamp"`
	RecipientID  string `json:"recipient_id"`
	Conversation *struct {
		ID string `json:"id"`
	} `json:"conversation,omitempty"`
	Pricing *struct {
		Billable     bool   `json:"billable"`
		PricingModel string `json:"pricing_model"`
		Category     string `json:"category"`
	} `json:"pricing,omitempty"`
	Errors []WebhookStatusError `json:"errors,omitempty"`
}

// WebhookPayload represents the incoming webhook from Meta
type WebhookPayload struct {
	Object string `json:"object"`
	Entry  []struct {
		ID      string `json:"id"`
		Time    int64  `json:"time,omitempty"`
		Changes []struct {
			Value struct {
				MessagingProduct string `json:"messaging_product"`
				Metadata         struct {
					DisplayPhoneNumber string `json:"display_phone_number"`
					PhoneNumberID      string `json:"phone_number_id"`
				} `json:"metadata"`
				// Template status update fields (when field == "message_template_status_update")
				Event                   string                      `json:"event,omitempty"`
				MessageTemplateID       int64                       `json:"message_template_id,omitempty"`
				MessageTemplateName     string                      `json:"message_template_name,omitempty"`
				MessageTemplateLanguage string                      `json:"message_template_language,omitempty"`
				Reason                  string                      `json:"reason,omitempty"`
				Contacts                []CoexistenceWebhookContact `json:"contacts"`
				Messages                []CoexistenceMessage        `json:"messages,omitempty"`
				Statuses                []WebhookStatus             `json:"statuses,omitempty"`
				UserPreferences         []struct {
					WaID      string `json:"wa_id"`
					UserID    string `json:"user_id,omitempty"`
					Category  string `json:"category"`
					Value     string `json:"value"`
					Timestamp int64  `json:"timestamp"`
				} `json:"user_preferences,omitempty"`
				Calls []struct {
					ID         string `json:"id"`
					From       string `json:"from"`
					FromUserID string `json:"from_user_id,omitempty"` // BSUID
					To         string `json:"to"`
					ToUserID   string `json:"to_user_id,omitempty"` // BSUID
					Timestamp  string `json:"timestamp"`
					Type       string `json:"type"`
					Event      string `json:"event"`
					Direction  string `json:"direction,omitempty"`
					Session    *struct {
						SDPType string `json:"sdp_type"`
						SDP     string `json:"sdp"`
					} `json:"session,omitempty"`
					Error *struct {
						Code    int    `json:"code"`
						Message string `json:"message"`
					} `json:"error,omitempty"`
					// Terminate webhook fields
					Status    json.RawMessage `json:"status,omitempty"`
					StartTime string          `json:"start_time,omitempty"`
					EndTime   string          `json:"end_time,omitempty"`
					Duration  int             `json:"duration,omitempty"`
				} `json:"calls,omitempty"`
				// WhatsApp Business App Coexistence fields.
				StateSync         []CoexistenceStateSyncItem `json:"state_sync,omitempty"`
				History           []CoexistenceHistoryBatch  `json:"history,omitempty"`
				PhoneNumber       string                     `json:"phone_number,omitempty"`
				DisconnectionInfo *CoexistenceDisconnection  `json:"disconnection_info,omitempty"`
				// Message echoes fields (when field == "smb_message_echoes")
				MessageEchoes []CoexistenceMessage `json:"message_echoes,omitempty"`
			} `json:"value"`
			Field string `json:"field"`
		} `json:"changes"`
	} `json:"entry"`
}

// WebhookHandler processes incoming webhook events from Meta
func (a *App) WebhookHandler(r *fastglue.Request) error {
	body := r.RequestCtx.PostBody()
	signature := r.RequestCtx.Request.Header.Peek("X-Hub-Signature-256")

	var payload WebhookPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		a.Log.Error("Failed to parse webhook payload", "error", err)
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid payload", nil, "")
	}

	// Meta signs every webhook POST. Verification is fail-closed: a missing
	// header, an unknown account, or an account without a usable app secret is
	// rejected before any event can be dispatched.
	if !a.verifyMetaWebhookPayload(body, signature, &payload) {
		a.Log.Warn("Invalid or unverifiable webhook signature")
		return r.SendErrorEnvelope(fasthttp.StatusForbidden, "Invalid signature", nil, "")
	}
	webhookBodyDigest := sha256.Sum256(body)
	webhookBodySHA256 := hex.EncodeToString(webhookBodyDigest[:])

	// statusRetryCarried is set once a status in this POST must be retried
	// (see the status loop below). That no longer ends the POST after the
	// status's own change. The remaining changes and entries are still walked,
	// but only smb_message_echoes changes are processed. Every other change is
	// skipped, as when the 503 ended the POST there, and the POST is answered
	// with a single 503 at its end.
	//
	// Echoes are the only exception. They are the event that the early 503
	// lost: on a Coexistence account Meta can batch a status ahead of the echo
	// that stores the very message the status belongs to. Meta replays a POST
	// in the same order, so the status could never resolve, and the echo was
	// lost once Meta stopped retrying. Every echo is stored, not only one whose
	// message has a pending status: a Coexistence status whose message is
	// never stored stays pending for Meta's whole retry window, and would
	// otherwise still lose every other echo behind it. Echoes are also safe to
	// run on the first attempt and again on every replay. An echo change
	// commits in one transaction before the acknowledgement, and an echo is
	// idempotent per WAMID: its Message has a deterministic id, the first
	// durable admission winner owns the WAMID, a replay only merges details and
	// moves the status forward, the new-message broadcast and outgoing webhook
	// fire only on the first insert, and the media hydration job is keyed for
	// idempotency. A revoke echo is final and a no-op once applied. An edit
	// echo is held back with the skipped changes, because its replay guard
	// remembers only the latest edit: replayed after a newer edit of the same
	// message, it would restore the older text.
	//
	// The skipped changes have no such guard, or depend on their order. Calls,
	// template status updates, call-permission replies and reactions have no
	// replay guard: run on the first attempt and again on each replay, they
	// could overwrite newer state with older state or repeat call events.
	// Inbound messages must not be admitted, and their automatic replies
	// started, ahead of Meta's replay, and later statuses keep per-message
	// order by waiting too.
	//
	// As when the 503 ended the POST at the status, a skipped change is
	// processed only by a replay that no longer has to retry a status, never
	// by an attempt that does. It now runs after the echoes that followed it
	// in the POST, though, because those were stored by an earlier attempt:
	// the order seen when Meta delivers such an echo in an earlier POST. For
	// an inbound message M and a later echo E to the same customer, both after
	// the pending status:
	//   - E is stored without its reply link to M, and a replay of E does not
	//     add a missing link;
	//   - the inbox conversation's last-message preview shows M instead of E,
	//     and its last inbound time ends up later than its last outbound time;
	//   - if M is the first message from a BSUID that no contact owns yet, it
	//     is admitted to the contact that E created instead of being staged
	//     for identity review.
	//
	// A persistence failure of a processed echo, like that of any change
	// before the status, still answers 503 at that point, and nothing after it
	// is processed in that attempt.
	statusRetryCarried := false

	// Process each entry
	for _, entry := range payload.Entry {
		for _, change := range entry.Changes {
			if statusRetryCarried && change.Field != "smb_message_echoes" {
				a.Log.Info("Deferred webhook change behind a WhatsApp status retry",
					"waba_id", entry.ID,
					"field", change.Field,
				)
				continue
			}

			// Coexistence lifecycle events are WABA-scoped and normally do not
			// include phone-number metadata. Persist them before acknowledging so
			// disconnected accounts cannot continue sending from a stale cache.
			if change.Field == "account_update" && isCoexistenceLifecycleEvent(change.Value.Event) {
				if err := a.persistCoexistenceLifecycleBeforeAck(
					entry.ID,
					entry.Time,
					change.Value.Event,
					change.Value.PhoneNumber,
					change.Value.DisconnectionInfo,
				); err != nil {
					a.Log.Error("Failed to durably process coexistence lifecycle event",
						"error", err,
						"waba_id", entry.ID,
						"event", change.Value.Event,
					)
					return r.SendErrorEnvelope(
						fasthttp.StatusServiceUnavailable,
						"Coexistence lifecycle persistence failed",
						nil,
						"",
					)
				}
				continue
			}

			// Handle template status updates
			if change.Field == "message_template_status_update" {
				a.Log.Info("Received template status update",
					"event", change.Value.Event,
					"template_name", change.Value.MessageTemplateName,
					"template_language", change.Value.MessageTemplateLanguage,
					"waba_id", entry.ID,
				)
				go a.processTemplateStatusUpdate(entry.ID, change.Value.Event, change.Value.MessageTemplateName, change.Value.MessageTemplateLanguage, change.Value.Reason)
				continue
			}

			// Handle user preferences (marketing opt-out/in)
			if change.Field == "user_preferences" {
				for _, pref := range change.Value.UserPreferences {
					if pref.Category == "marketing_messages" {
						if err := a.processMarketingPreference(
							change.Value.Metadata.PhoneNumberID,
							pref.WaID,
							pref.UserID,
							pref.Value,
						); err != nil {
							a.Log.Error(
								"Failed to durably process marketing preference before acknowledgement",
								"error", err,
								"phone_id", change.Value.Metadata.PhoneNumberID,
							)
							return r.SendErrorEnvelope(
								fasthttp.StatusServiceUnavailable,
								"Marketing preference persistence failed",
								nil,
								"",
							)
						}
					}
				}
				continue
			}

			// Handle voice call events (processed sequentially to preserve event order
			// and avoid race conditions between ringing/connect for the same call)
			if change.Field == "calls" {
				phoneNumberID := change.Value.Metadata.PhoneNumberID
				for _, call := range change.Value.Calls {
					a.Log.Info("Received call event",
						"call_id", call.ID,
						"from", call.From,
						"event", call.Event,
						"direction", call.Direction,
						"has_sdp", call.Session != nil && call.Session.SDP != "",
						"phone_number_id", phoneNumberID,
					)
					a.processCallWebhook(phoneNumberID, call)
				}

				// Business-initiated call status webhooks (RINGING/ACCEPTED/REJECTED)
				// arrive in the statuses array under field="calls"
				for _, status := range change.Value.Statuses {
					if status.Status == "" {
						continue
					}
					a.Log.Info("Received call status event",
						"call_id", status.ID,
						"status", status.Status,
					)
					a.processCallStatusWebhook(phoneNumberID, status)
				}
				continue
			}

			// Handle contact state sync (coexistence)
			if change.Field == "smb_app_state_sync" {
				phoneNumberID := change.Value.Metadata.PhoneNumberID
				a.Log.Info("Received smb_app_state_sync event",
					"phone_number_id", phoneNumberID,
					"item_count", len(change.Value.StateSync),
				)
				if err := a.persistContactStateSyncBeforeAck(phoneNumberID, change.Value.StateSync); err != nil {
					a.Log.Error("Failed to durably process contact state sync",
						"error", err,
						"phone_id", phoneNumberID,
					)
					return r.SendErrorEnvelope(
						fasthttp.StatusServiceUnavailable,
						"Contact state sync persistence failed",
						nil,
						"",
					)
				}
				continue
			}

			// Handle message echoes (coexistence)
			if change.Field == "smb_message_echoes" {
				phoneNumberID := change.Value.Metadata.PhoneNumberID
				for _, echo := range change.Value.MessageEchoes {
					a.Log.Info("Received message echo",
						"from", echo.From,
						"type", echo.Type,
						"phone_number_id", phoneNumberID,
					)
				}
				echoes := change.Value.MessageEchoes
				if statusRetryCarried {
					echoes = a.messageEchoesAheadOfStatusRetry(phoneNumberID, echoes)
					if len(echoes) == 0 {
						continue
					}
				}
				if err := a.persistMessageEchoesBeforeAck(
					phoneNumberID,
					echoes,
					change.Value.Contacts,
				); err != nil {
					a.Log.Error("Failed to durably process message echoes",
						"error", err,
						"phone_id", phoneNumberID,
					)
					return r.SendErrorEnvelope(
						fasthttp.StatusServiceUnavailable,
						"Message echo persistence failed",
						nil,
						"",
					)
				}
				continue
			}

			// Initial history arrives as history[] phase/chunk batches. Media
			// details can arrive later in value.messages with the same wamid.
			if change.Field == "history" {
				phoneNumberID := change.Value.Metadata.PhoneNumberID
				if err := a.persistCoexistenceHistoryBeforeAck(
					phoneNumberID,
					change.Value.Metadata.DisplayPhoneNumber,
					entry.Time,
					change.Value.History,
					change.Value.Messages,
					change.Value.Contacts,
				); err != nil {
					a.Log.Error("Failed to durably process coexistence history",
						"error", err,
						"phone_id", phoneNumberID,
					)
					return r.SendErrorEnvelope(
						fasthttp.StatusServiceUnavailable,
						"History sync persistence failed",
						nil,
						"",
					)
				}
				continue
			}

			if change.Field != "messages" {
				continue
			}

			phoneNumberID := change.Value.Metadata.PhoneNumberID

			// Process messages
			for _, msg := range change.Value.Messages {
				a.Log.Info("Received message",
					"from", msg.From,
					"type", msg.Type,
					"phone_number_id", phoneNumberID,
				)

				if strings.EqualFold(msg.Type, "edit") || strings.EqualFold(msg.Type, "revoke") {
					if err := a.persistCoexistenceInboundMutationBeforeAck(
						phoneNumberID,
						msg,
						change.Value.Contacts,
					); err != nil {
						a.Log.Error(
							"Failed to durably process WhatsApp message mutation",
							"error", err,
							"phone_id", phoneNumberID,
							"message_id", msg.ID,
							"type", msg.Type,
						)
						return r.SendErrorEnvelope(
							fasthttp.StatusServiceUnavailable,
							"Message mutation persistence failed",
							nil,
							"",
						)
					}
					continue
				}

				// Get contact profile name (match by phone or BSUID). The matched
				// entry's wa_id and profile.username also travel with the message
				// so a sender without "from" (WhatsApp username user) resolves
				// to their own contact rather than an empty-phone one.
				profileName := ""
				inbound := msg.IncomingTextMessage
				for i := range change.Value.Contacts {
					contact := &change.Value.Contacts[i]
					if (msg.From != "" && contact.WaID == msg.From) ||
						(msg.FromUserID != "" && contact.UserID == msg.FromUserID) ||
						(msg.FromParentUserID != "" && contact.ParentUserID == msg.FromParentUserID) {
						profileName = contact.Profile.Name
						inbound = inbound.withWebhookSenderContact(contact)
						break
					}
				}

				// Handle call permission replies before regular message processing
				if msg.Type == "interactive" && msg.Interactive != nil &&
					msg.Interactive.Type == "call_permission_reply" &&
					msg.Interactive.CallPermissionReply != nil {
					cpr := msg.Interactive.CallPermissionReply
					expTS, err := cpr.ExpirationTimestamp.Int64()
					if err != nil {
						a.Log.Error("Failed to parse call permission expiration timestamp", "error", err, "from", msg.From)
						continue
					}
					go a.processCallPermissionReply(phoneNumberID, msg.From, &CallPermissionReplyData{
						Response:            cpr.Response,
						IsPermanent:         cpr.IsPermanent,
						ExpirationTimestamp: expTS,
						ResponseSource:      cpr.ResponseSource,
					})
					continue
				}

				// Reactions update an existing row and retain their specialized
				// asynchronous path. Every regular inbound message crosses a
				// durable database boundary before Meta receives HTTP 200.
				if msg.Type == "reaction" && msg.Reaction != nil {
					a.startSpecializedIncomingMessage(phoneNumberID, msg.IncomingTextMessage, profileName)
					continue
				}

				// A message that names no sender at all (no from, wa_id, BSUID
				// or username) cannot be attributed or answered. Skip it, as
				// call_webhook does for sender-less calls, instead of failing
				// persistence and asking Meta to retry the delivery forever.
				if !inbound.hasSenderIdentity() {
					a.Log.Warn("Skipping incoming message without a sender identity",
						"phone_number_id", phoneNumberID,
						"message_id", msg.ID,
						"type", msg.Type,
					)
					continue
				}

				work, duplicate, err := a.persistAuthenticatedIncomingMessageBeforeAck(
					phoneNumberID,
					inbound,
					profileName,
					webhookBodySHA256,
				)
				if err != nil {
					a.Log.Error(
						"Failed to durably persist incoming message before acknowledgement",
						"error", err,
						"phone_id", phoneNumberID,
						"message_id", msg.ID,
					)
					// A non-2xx response asks Meta to retry. Returning 200 here
					// would create an unrecoverable ACK-before-commit window.
					return r.SendErrorEnvelope(
						fasthttp.StatusServiceUnavailable,
						"Incoming message persistence failed",
						nil,
						"",
					)
				}
				if duplicate {
					a.startPersistedIncomingMessageContinuation(work)
					continue
				}
				a.startPersistedIncomingMessageContinuation(work)
			}

			// Process status updates. Each status commits in its own transaction
			// and only ever applies a strict forward step, so a status that was
			// already applied is a no-op when Meta replays the POST. A status
			// that must be retried therefore does not stop the statuses of other
			// messages after it in this change: they are applied now, and the
			// POST is answered with 503 at its end (statusRetryCarried above
			// covers the changes after this one). Later statuses for the same
			// WAMID in this change wait for that retry instead, so per-message
			// order is kept: a higher status applied first would otherwise turn
			// the retried lower one into a no-op (a campaign recipient's
			// delivered_at would never be set, for example). Statuses in later
			// changes all wait for the retry, so retryWAMIDs is kept per change.
			var retryWAMIDs map[string]struct{}
			for _, status := range change.Value.Statuses {
				a.Log.Info("Received status update",
					"message_id", status.ID,
					"status", status.Status,
				)

				wamid := strings.TrimSpace(status.ID)
				if _, waiting := retryWAMIDs[wamid]; waiting {
					a.Log.Info("Deferred WhatsApp status behind an earlier status for the same message",
						"phone_id", phoneNumberID,
						"message_id", status.ID,
						"status", status.Status,
					)
					continue
				}
				if err := a.processStatusUpdate(phoneNumberID, status); err != nil {
					statusRetryCarried = true
					if retryWAMIDs == nil {
						retryWAMIDs = make(map[string]struct{})
					}
					retryWAMIDs[wamid] = struct{}{}
					if errors.Is(err, errWhatsAppStatusOwnerPending) {
						// processStatusUpdate already logged this wait at Info, or
						// at Warn when it needs attention; it is not a
						// persistence failure.
						continue
					}
					a.Log.Error(
						"Failed to durably process status update before acknowledgement",
						"error", err,
						"phone_id", phoneNumberID,
						"message_id", status.ID,
					)
				}
			}
		}
	}

	if statusRetryCarried {
		// A non-2xx response asks Meta to retry. Returning 200 before the
		// committed receipt mutation would lose an early receipt if this
		// process exits or the matching outgoing WAMID has not yet committed.
		// processStatusUpdate also returns nil without a mutation in other
		// cases, among them a durable tombstone, a duplicate or non-forward
		// status, an unknown status value and, on a classic account only, a
		// WAMID proven absent once its status is older than
		// whatsAppOrphanStatusGrace (see that constant for the bound). The 503
		// is sent only now, so that an smb_message_echoes change after the
		// status that stores its message is committed in this attempt and
		// Meta's replay can apply the status. Every other change after the
		// status was skipped and is processed by that replay (see
		// statusRetryCarried above).
		return r.SendErrorEnvelope(
			fasthttp.StatusServiceUnavailable,
			"Status update persistence failed",
			nil,
			"",
		)
	}

	// Always respond with 200 to acknowledge receipt
	return r.SendEnvelope(map[string]string{"status": "ok"})
}

// messageEchoesAheadOfStatusRetry returns the echoes of an smb_message_echoes
// change that are stored while an earlier status of the same POST waits for
// Meta's replay: every echo except an edit. An edit waits for that replay,
// like the changes WebhookHandler skips (see statusRetryCarried there).
func (a *App) messageEchoesAheadOfStatusRetry(
	phoneNumberID string,
	echoes []CoexistenceMessage,
) []CoexistenceMessage {
	kept := make([]CoexistenceMessage, 0, len(echoes))
	for _, echo := range echoes {
		if strings.EqualFold(strings.TrimSpace(echo.Type), "edit") {
			a.Log.Info("Deferred message echo edit behind a WhatsApp status retry",
				"phone_number_id", phoneNumberID,
				"message_id", echo.ID,
			)
			continue
		}
		kept = append(kept, echo)
	}
	return kept
}

func (a *App) processIncomingMessage(phoneNumberID string, msg IncomingTextMessage, profileName string) {
	if a.rlsEnabled() && !a.hasTenantScope() {
		if err := a.withPhoneTenant(phoneNumberID, func(scoped *App) error {
			scoped.processIncomingMessage(phoneNumberID, msg, profileName)
			return nil
		}); err != nil {
			a.Log.Error("Failed to scope incoming message to tenant", "error", err, "phone_id", phoneNumberID)
		}
		return
	}

	defer func() {
		if r := recover(); r != nil {
			a.Log.Error("Panic recovered in processIncomingMessage", "panic", r, "phone_id", phoneNumberID, "message_id", msg.ID)
		}
	}()

	// Process the message with chatbot logic
	a.processIncomingMessageFull(phoneNumberID, msg, profileName)
}

// startSpecializedIncomingMessage tracks reaction processing for graceful
// shutdown while retaining the existing panic and tenant-scope protections.
func (a *App) startSpecializedIncomingMessage(
	phoneNumberID string,
	msg IncomingTextMessage,
	profileName string,
) {
	root := a.rootApp()
	root.wg.Add(1)
	go func() {
		defer root.wg.Done()
		defer func() {
			if recovered := recover(); recovered != nil {
				root.Log.Error(
					"Panic recovered in specialized incoming message",
					"panic", recovered,
					"phone_id", phoneNumberID,
					"message_id", msg.ID,
				)
			}
		}()
		root.processIncomingMessage(phoneNumberID, msg, profileName)
	}()
}

// startPersistedIncomingMessageContinuation launches only after the synchronous
// persistence helper has committed. It re-enters tenant scope from the root
// pool, is tracked for graceful shutdown, and contains all media/provider work.
func (a *App) startPersistedIncomingMessageContinuation(work *persistedIncomingMessage) {
	if work == nil {
		return
	}

	root := a.rootApp()
	root.wg.Add(1)
	go func() {
		defer root.wg.Done()
		defer func() {
			if recovered := recover(); recovered != nil {
				root.Log.Error(
					"Panic recovered in incoming message continuation",
					"panic", recovered,
					"phone_id", work.PhoneNumberID,
					"message_id", work.Message.ID,
				)
			}
		}()

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		processor := NewInboundContinuationProcessor(
			root,
			defaultInboundContinuationPoll,
		)
		if err := processor.ProcessMessage(
			ctx,
			work.OrganizationID,
			work.Persisted.ID,
		); err != nil {
			root.Log.Error(
				"Failed to process durable incoming message continuation",
				"error", err,
				"organization_id", work.OrganizationID,
				"message_id", work.Message.ID,
			)
		}
	}()
}

func (a *App) processStatusUpdate(phoneNumberID string, status WebhookStatus) (resultErr error) {
	defer func() {
		if r := recover(); r != nil {
			a.Log.Error("Panic recovered in processStatusUpdate", "panic", r, "phone_id", phoneNumberID, "status_id", status.ID)
			resultErr = fmt.Errorf("process WhatsApp status update panic: %v", r)
		}
	}()

	phoneNumberID = strings.TrimSpace(phoneNumberID)
	messageID := strings.TrimSpace(status.ID)
	if phoneNumberID == "" || messageID == "" {
		return errors.New("WhatsApp status update identity is incomplete")
	}
	if !a.rlsEnabled() {
		// The production resolver has an RLS-safe cardinality function. Mirror
		// that fail-closed property in non-RLS test/development mode instead of
		// allowing GORM's First() to choose a cross-tenant phone collision.
		var phoneOwnerCount int64
		if err := a.rootApp().DB.Model(&models.WhatsAppAccount{}).
			Where("BTRIM(phone_id) = ?", phoneNumberID).
			Count(&phoneOwnerCount).Error; err != nil {
			return fmt.Errorf("count WhatsApp status phone authority: %w", err)
		}
		if phoneOwnerCount != 1 {
			return fmt.Errorf(
				"WhatsApp status phone authority requires exactly one owner: got %d",
				phoneOwnerCount,
			)
		}
	}
	organizationID, err := a.resolveWhatsAppOrganization(phoneNumberID)
	if err != nil {
		return fmt.Errorf("resolve WhatsApp status organization: %w", err)
	}

	// A delivery receipt is a mutation. Always use a committed tenant
	// transaction, including in non-RLS development/test configurations: this
	// preserves the WAMID -> account -> contact -> message lock order and keeps
	// the message, recipient, and campaign counter atomically consistent.
	orphanAcknowledged := false
	orphanAge := time.Duration(0)
	orphanTimestampUsable := false
	orphanSettled := false
	orphanCoexistenceAccount := false
	err = a.WithCommittedTenantApp(organizationID, func(scoped *App) error {
		if err := database.LockWhatsAppWAMIDScopes(scoped.DB, organizationID, messageID); err != nil {
			return err
		}
		var accounts []models.WhatsAppAccount
		if err := scoped.DB.Where(
			"organization_id = ? AND BTRIM(phone_id) = ?", organizationID, phoneNumberID,
		).Order("id").Find(&accounts).Error; err != nil {
			return err
		}
		if len(accounts) != 1 {
			return fmt.Errorf("WhatsApp status account authority is ambiguous")
		}
		account := accounts[0]
		if err := scoped.prepareWhatsAppMessageAuthority(&account); err != nil {
			return err
		}
		resolved, err := scoped.resolveWhatsAppMessage(&account, whatsAppMessageLookup{
			WAMID:            messageID,
			Direction:        models.DirectionOutgoing,
			Lock:             true,
			RepairProjection: true,
		})
		if errors.Is(err, errWhatsAppMessageOwnerDeleted) {
			return nil // durable tombstones reserve the WAMID but accept no replay mutation.
		}
		if errors.Is(err, errWhatsAppMessageOwnerNotStored) {
			// No Message is a candidate owner of this WAMID yet, but one that
			// has not committed may still claim it: an early receipt for a
			// ReReply send inside the grace window, or, on a Coexistence
			// account, a status that overtook its smb_message_echoes echo,
			// whose ingestion has no bound short of Meta's own retry horizon.
			// Keep the error so Meta retries. Only a classic account past the
			// grace window may acknowledge, and only a proven absent owner;
			// any row carrying the WAMID keeps failing closed exactly as
			// before. A stored owner whose dependent lookup misses (for
			// example a soft-deleted contact) is not this case: its bare
			// gorm.ErrRecordNotFound is returned below as a real rejection.
			orphanAge, orphanTimestampUsable, orphanSettled = whatsAppOrphanStatusAge(status.Timestamp, time.Now())
			orphanCoexistenceAccount = account.IsSMB
			if account.IsSMB || !orphanSettled {
				return fmt.Errorf("%w: %w", errWhatsAppStatusOwnerPending, err)
			}
			absent, probeErr := scoped.whatsAppStatusOwnerAbsent(&account, messageID)
			if probeErr != nil {
				return fmt.Errorf("prove WhatsApp status owner absence: %w", probeErr)
			}
			if !absent {
				return err
			}
			orphanAcknowledged = true
			return nil
		}
		if err != nil {
			return err
		}
		return scoped.updateResolvedMessageStatus(&resolved.Message, status.Status, status.Errors)
	})
	if err != nil {
		if errors.Is(err, errWhatsAppStatusOwnerPending) {
			fields := []any{
				"error", err,
				"phone_id", phoneNumberID,
				"status_id", messageID,
				"status", status.Status,
				"age_seconds", int64(orphanAge / time.Second),
				"coexistence_account", orphanCoexistenceAccount,
			}
			switch {
			case !orphanTimestampUsable:
				// Meta stamps every status, so a missing, malformed or
				// far-future timestamp is an anomaly rather than an expected
				// wait: such a status can never settle and is retried for
				// Meta's whole retry window.
				a.Log.Warn("Retrying WhatsApp status without a usable timestamp",
					append(fields, "timestamp", status.Timestamp)...)
			case orphanSettled:
				// Only a Coexistence account defers a settled status; past
				// the grace window it points at a lagging echo ingestion.
				a.Log.Warn("Deferred WhatsApp status until its message is stored", fields...)
			default:
				// An expected wait, not a persistence failure.
				a.Log.Info("Deferred WhatsApp status until its message is stored", fields...)
			}
			return err
		}
		a.Log.Warn("Rejected WhatsApp status update", "error", err, "phone_id", phoneNumberID, "status_id", messageID)
		return err
	}
	if orphanAcknowledged {
		a.Log.Warn("Acknowledged WhatsApp status for a message ReReply never stored",
			"phone_id", phoneNumberID,
			"status_id", messageID,
			"status", status.Status,
			"age_seconds", int64(orphanAge/time.Second),
			"coexistence_account", orphanCoexistenceAccount,
		)
		return nil
	}
	a.Log.Info("Processed status update", "message_id", messageID, "status", status.Status, "phone_number_id", phoneNumberID)
	return nil
}

// errWhatsAppStatusOwnerPending marks a status whose WAMID no candidate
// Message carries yet but which a Message may still claim, so Meta must retry
// it. It wraps the resolver's errWhatsAppMessageOwnerNotStored (and so
// gorm.ErrRecordNotFound) and is an expected wait, not a persistence failure.
// A stored owner whose dependent lookup misses never gets this label.
var errWhatsAppStatusOwnerPending = errors.New("WhatsApp status owner is not stored yet")

// whatsAppOrphanStatusGrace bounds how long, on a classic (non-Coexistence)
// account, a status for a WAMID that no tenant Message carries may still be an
// early receipt. It applies to classic accounts only: on a Coexistence account
// the WhatsApp Business app echo that creates the Message has no such bound
// (it depends on Meta's retries and on our own admission and availability),
// so there an absent WAMID is never acknowledged.
//
// On a classic account only a ReReply send can create the owner. Every
// ReReply WhatsApp send that produces a Message commits it as pending (with
// an empty WAMID) before calling Graph and commits the returned WAMID right
// after: in the same provider transaction, or in a recovery or settlement
// bounded by outgoingDeliveryRecoveryTimeout and
// campaignDurableSettlementTimeout (10s), behind a Graph call bounded by
// whatsapp.DefaultTimeout (30s) or, for automatic AI replies,
// defaultInboundAIAttemptTimeout (2m). Meta stamps a status with the time of
// its event, which is never earlier than Meta accepting the send. Once a
// status is older than this window and its WAMID is still absent, no ReReply
// send can claim it any more: it belongs to a message ReReply never stored (a
// WhatsApp Business app reply on a classic account, a reaction or call
// permission request, which produce no Message, or a send by another app on
// the number), so a retry can never succeed.
//
// Assumptions: Meta's retries resend the original status timestamp (were a
// retry ever re-stamped, its status would stay inside the window and keep the
// previous retry behaviour), and the server clock is NTP-synchronised to
// within a few minutes. A clock running fast by more than the window would
// acknowledge genuine early receipts; a slow clock only delays acknowledgement.
const whatsAppOrphanStatusGrace = 15 * time.Minute

// whatsAppStatusClockSkew is how far in the future a status timestamp may lie
// and still count as a fresh event rather than an unusable one. It only
// chooses the log level: a future timestamp is never settled.
const whatsAppStatusClockSkew = time.Minute

// whatsAppOrphanStatusAge reports the age of a status event, whether its
// timestamp is usable, and whether it is past whatsAppOrphanStatusGrace. A
// missing, malformed, zero or future timestamp is never settled, so such a
// status keeps the retry contract; one that is missing, malformed, zero or
// more than whatsAppStatusClockSkew in the future (for example milliseconds
// read as seconds) is also reported as unusable.
func whatsAppOrphanStatusAge(rawTimestamp string, now time.Time) (age time.Duration, usable, settled bool) {
	seconds, err := strconv.ParseInt(strings.TrimSpace(rawTimestamp), 10, 64)
	if err != nil || seconds <= 0 {
		return 0, false, false
	}
	age = now.Sub(time.Unix(seconds, 0))
	if age < -whatsAppStatusClockSkew {
		return age, false, false
	}
	return age, true, age >= whatsAppOrphanStatusGrace
}

// whatsAppStatusOwnerAbsent proves, inside the status transaction, that no
// Message in the tenant (live or soft-deleted, linked or unlinked) carries
// this WAMID or its deterministic Coexistence identity. The resolver's
// gorm.ErrRecordNotFound alone is not that proof: a row that is not a resolver
// candidate (a linked row of another provider, live or soft-deleted, whose
// WAMID may even be untrimmed because chk_messages_whatsapp_message_id_trimmed
// exempts it) can still carry the WAMID, and such a status must keep failing
// closed. The deterministic-id clause is defensive only: the resolver's
// candidate query already returns any row with that id, and the WAMID advisory
// lock held by the caller stops one from appearing in between.
//
// The proof requires READ COMMITTED. The caller takes the WAMID advisory lock
// first, and only a fresh per-statement snapshot sees an owner that committed
// while this transaction waited for that lock; a REPEATABLE READ snapshot could
// predate it, so any other isolation level fails closed.
func (a *App) whatsAppStatusOwnerAbsent(account *models.WhatsAppAccount, wamid string) (bool, error) {
	wamid = strings.TrimSpace(wamid)
	if a == nil || a.DB == nil || account == nil || account.ID == uuid.Nil ||
		account.OrganizationID == uuid.Nil || wamid == "" {
		return false, errors.New("WhatsApp status owner absence proof is incomplete")
	}
	var isolation string
	if err := a.DB.Raw("SHOW transaction_isolation").Scan(&isolation).Error; err != nil {
		return false, fmt.Errorf("read status transaction isolation: %w", err)
	}
	if isolation != "read committed" {
		return false, fmt.Errorf(
			"WhatsApp status owner absence proof requires read committed isolation, got %q",
			isolation,
		)
	}
	var owners int64
	if err := a.DB.Unscoped().Model(&models.Message{}).Where(
		"organization_id = ? AND (id = ? OR BTRIM(whats_app_message_id) = ?)",
		account.OrganizationID,
		uuid.NewSHA1(account.ID, []byte("coexistence-message:"+wamid)),
		wamid,
	).Count(&owners).Error; err != nil {
		return false, err
	}
	return owners == 0, nil
}

// statusPriority returns the priority of a status (higher = more progressed)
func statusPriority(status models.MessageStatus) int {
	switch status {
	case models.MessageStatusPending:
		return 0
	case models.MessageStatusSent:
		return 1
	case models.MessageStatusDelivered:
		return 2
	case models.MessageStatusRead:
		return 3
	case models.MessageStatusFailed:
		return 4 // Failed can override any status
	default:
		return -1
	}
}

const campaignStatusProjectionFailureMetadataKey = "campaign_status_projection_failure"

type campaignStatusCounters struct {
	sent      int
	delivered int
	read      int
	failed    int
}

func campaignStatusCounterMembership(status models.MessageStatus) campaignStatusCounters {
	switch status {
	case models.MessageStatusSent:
		return campaignStatusCounters{sent: 1}
	case models.MessageStatusDelivered:
		return campaignStatusCounters{sent: 1, delivered: 1}
	case models.MessageStatusRead:
		return campaignStatusCounters{sent: 1, delivered: 1, read: 1}
	case models.MessageStatusFailed:
		return campaignStatusCounters{failed: 1}
	default:
		return campaignStatusCounters{}
	}
}

func campaignStatusCounterTransition(
	currentStatus,
	newStatus models.MessageStatus,
) campaignStatusCounters {
	current := campaignStatusCounterMembership(currentStatus)
	next := campaignStatusCounterMembership(newStatus)
	return campaignStatusCounters{
		sent:      next.sent - current.sent,
		delivered: next.delivered - current.delivered,
		read:      next.read - current.read,
		failed:    next.failed - current.failed,
	}
}

// updateResolvedMessageStatus mutates a message only after processStatusUpdate
// has proven its exact WAMID owner under the stable phone/account authority.
// It deliberately accepts no caller-selected WAMID or account display name.
func (a *App) updateResolvedMessageStatus(message *models.Message, statusValue string, statusErrors []WebhookStatusError) error {
	if message == nil || message.ID == uuid.Nil || message.OrganizationID == uuid.Nil || strings.TrimSpace(message.WhatsAppMessageID) == "" {
		return errors.New("resolved WhatsApp message status owner is incomplete")
	}

	newStatus := models.MessageStatus(statusValue)
	currentStatus := message.Status
	currentPriority := statusPriority(currentStatus)
	newPriority := statusPriority(newStatus)

	// Accept only a strict progression. Failed already has the highest priority,
	// so it can override every delivery status while duplicate failure receipts
	// remain idempotent for recipient state and campaign counters.
	if newPriority <= currentPriority {
		a.Log.Debug("Ignoring status update - not a progression",
			"message_id", message.ID,
			"current_status", message.Status,
			"new_status", statusValue)
		return nil
	}

	updates := map[string]any{}

	switch newStatus {
	case models.MessageStatusSent:
		updates["status"] = models.MessageStatusSent
	case models.MessageStatusDelivered:
		updates["status"] = models.MessageStatusDelivered
	case models.MessageStatusRead:
		updates["status"] = models.MessageStatusRead
	case models.MessageStatusFailed:
		updates["status"] = models.MessageStatusFailed
		if len(statusErrors) > 0 {
			// Prefer error_data.details (most descriptive), then Message, then Title.
			errText := statusErrors[0].ErrorData.Details
			if errText == "" {
				errText = statusErrors[0].Message
			}
			if errText == "" || errText == statusErrors[0].Title {
				errText = statusErrors[0].Title
			}

			updates["error_message"] = errText
		}
	default:
		a.Log.Debug("Ignoring message status update", "status", statusValue)
		return nil
	}

	if err := a.DB.Model(&models.Message{}).Where("id = ? AND organization_id = ?", message.ID, message.OrganizationID).Updates(updates).Error; err != nil {
		a.Log.Error("Failed to update message status", "error", err, "message_id", message.ID)
		return err
	}
	message.Status = newStatus
	if errorMessage, ok := updates["error_message"].(string); ok {
		message.ErrorMessage = errorMessage
	}

	a.Log.Info("Updated message status", "message_id", message.ID, "status", statusValue)

	// Update campaign stats and recipient status if this is a campaign message.
	// The Message is the receipt authority; campaign rows are a denormalized
	// projection. Isolate that projection behind a savepoint so a deleted or
	// inconsistent campaign cannot make an otherwise-valid provider receipt an
	// endless retry. A durable marker on the Message preserves repair evidence.
	if message.Metadata != nil {
		if campaignID, ok := message.Metadata["campaign_id"].(string); ok && campaignID != "" {
			campaignUUID, err := uuid.Parse(campaignID)
			var campaign models.BulkMessageCampaign
			projectionErr := err
			if projectionErr == nil {
				projectionErr = a.DB.Transaction(func(projectionTx *gorm.DB) error {
					var projectionErr error
					campaign, projectionErr = updateCampaignStatusProjection(
						projectionTx,
						message,
						campaignUUID,
						currentStatus,
						newStatus,
						updates,
					)
					return projectionErr
				})
			}
			if projectionErr != nil {
				if markerErr := a.persistCampaignStatusProjectionFailure(
					message,
					campaignID,
					newStatus,
					projectionErr,
				); markerErr != nil {
					return fmt.Errorf(
						"persist campaign status projection failure: %w (projection error: %v)",
						markerErr,
						projectionErr,
					)
				}
				a.Log.Warn(
					"Deferred campaign status projection after settling provider receipt",
					"error", projectionErr,
					"campaign_id", campaignID,
					"message_id", message.ID,
					"status", newStatus,
				)
			} else {
				if err := a.clearCampaignStatusProjectionFailure(message); err != nil {
					return err
				}
				a.broadcastCampaignStatusProjection(campaign)
			}
		}
	}

	wsPayload := map[string]any{
		"message_id": message.ID.String(),
		"status":     statusValue,
	}
	if errMsg, ok := updates["error_message"].(string); ok && errMsg != "" {
		wsPayload["error_message"] = errMsg
	}
	fallback := websocket.WSMessage{Type: websocket.TypeStatusUpdate, Payload: wsPayload}
	contactID := message.ContactID
	a.publishRealtimeEvent(queue.RealtimeEvent{
		OrganizationID: message.OrganizationID,
		Kind:           queue.RealtimeEventMessageStatusChanged,
		ContactID:      &contactID,
		MessageID:      &message.ID,
		Status:         statusValue,
		OccurredAt:     time.Now().UTC(),
	}, &fallback)
	return nil
}

func updateCampaignStatusProjection(
	tx *gorm.DB,
	message *models.Message,
	campaignID uuid.UUID,
	currentStatus,
	newStatus models.MessageStatus,
	messageUpdates map[string]any,
) (models.BulkMessageCampaign, error) {
	var campaign models.BulkMessageCampaign
	if tx == nil || message == nil {
		return campaign, errors.New("campaign status projection identity is incomplete")
	}
	if err := tx.Where(
		"id = ? AND organization_id = ?",
		campaignID,
		message.OrganizationID,
	).First(&campaign).Error; err != nil {
		return campaign, fmt.Errorf("load campaign status owner: %w", err)
	}

	recipientUpdates := map[string]any{"status": newStatus}
	now := time.Now().UTC()
	switch newStatus {
	case models.MessageStatusDelivered:
		recipientUpdates["delivered_at"] = now
	case models.MessageStatusRead:
		recipientUpdates["read_at"] = now
	case models.MessageStatusFailed:
		if errMsg, ok := messageUpdates["error_message"].(string); ok && errMsg != "" {
			recipientUpdates["error_message"] = errMsg
		}
	}
	recipientResult := tx.Model(&models.BulkMessageRecipient{}).
		Where(
			"campaign_id = ? AND message_id = ? AND whats_app_message_id = ?",
			campaign.ID,
			message.ID,
			message.WhatsAppMessageID,
		).
		Updates(recipientUpdates)
	if recipientResult.Error != nil {
		return campaign, fmt.Errorf("update campaign recipient status: %w", recipientResult.Error)
	}
	if recipientResult.RowsAffected != 1 {
		return campaign, errors.New("campaign recipient status owner is missing or ambiguous")
	}

	delta := campaignStatusCounterTransition(currentStatus, newStatus)
	counterUpdates := make(map[string]any, 4)
	addCampaignCounterDelta(counterUpdates, "sent_count", delta.sent)
	addCampaignCounterDelta(counterUpdates, "delivered_count", delta.delivered)
	addCampaignCounterDelta(counterUpdates, "read_count", delta.read)
	addCampaignCounterDelta(counterUpdates, "failed_count", delta.failed)
	if len(counterUpdates) == 0 {
		return campaign, nil
	}
	result := tx.Model(&campaign).
		Clauses(clause.Returning{}).
		Where("id = ? AND organization_id = ?", campaign.ID, message.OrganizationID).
		Updates(counterUpdates)
	if result.Error != nil {
		return campaign, fmt.Errorf("update campaign status counters: %w", result.Error)
	}
	if result.RowsAffected != 1 {
		return campaign, errors.New("campaign counter owner changed")
	}
	return campaign, nil
}

func addCampaignCounterDelta(updates map[string]any, column string, delta int) {
	if delta == 0 {
		return
	}
	updates[column] = gorm.Expr("GREATEST("+column+" + ?, 0)", delta)
}

func (a *App) persistCampaignStatusProjectionFailure(
	message *models.Message,
	campaignID string,
	status models.MessageStatus,
	projectionErr error,
) error {
	if message == nil || projectionErr == nil {
		return errors.New("campaign projection failure marker is incomplete")
	}
	metadata := cloneOutgoingMessageMetadata(message.Metadata)
	metadata[campaignStatusProjectionFailureMetadataKey] = map[string]any{
		"campaign_id": campaignID,
		"status":      string(status),
		"error":       projectionErr.Error(),
		"recorded_at": time.Now().UTC().Format(time.RFC3339Nano),
	}
	result := a.DB.Model(&models.Message{}).
		Where("id = ? AND organization_id = ?", message.ID, message.OrganizationID).
		Update("metadata", metadata)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return errors.New("campaign projection failure message owner changed")
	}
	message.Metadata = metadata
	return nil
}

func (a *App) clearCampaignStatusProjectionFailure(message *models.Message) error {
	if message == nil || message.Metadata == nil {
		return nil
	}
	if _, exists := message.Metadata[campaignStatusProjectionFailureMetadataKey]; !exists {
		return nil
	}
	metadata := cloneOutgoingMessageMetadata(message.Metadata)
	delete(metadata, campaignStatusProjectionFailureMetadataKey)
	result := a.DB.Model(&models.Message{}).
		Where("id = ? AND organization_id = ?", message.ID, message.OrganizationID).
		Update("metadata", metadata)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return errors.New("campaign projection repair message owner changed")
	}
	message.Metadata = metadata
	return nil
}

func (a *App) broadcastCampaignStatusProjection(campaign models.BulkMessageCampaign) {
	if a == nil || a.WSHub == nil || campaign.ID == uuid.Nil || campaign.OrganizationID == uuid.Nil {
		return
	}
	a.afterTenantCommit(func() {
		a.rootApp().WSHub.BroadcastToOrg(campaign.OrganizationID, websocket.WSMessage{
			Type: websocket.TypeCampaignStatsUpdate,
			Payload: map[string]any{
				"campaign_id":     campaign.ID.String(),
				"status":          campaign.Status,
				"sent_count":      campaign.SentCount,
				"delivered_count": campaign.DeliveredCount,
				"read_count":      campaign.ReadCount,
				"failed_count":    campaign.FailedCount,
			},
		})
	})
}

// processTemplateStatusUpdate updates template status when Meta sends a status update webhook
func (a *App) processTemplateStatusUpdate(wabaID, event, templateName, templateLanguage, reason string) {
	if a.rlsEnabled() && !a.hasTenantScope() {
		organizationIDs, err := a.resolveWABAOrganizations(wabaID)
		if err != nil {
			a.Log.Error("Failed to resolve template status tenant", "error", err, "waba_id", wabaID)
			return
		}
		for _, organizationID := range organizationIDs {
			if err := a.WithTenantApp(organizationID, func(scoped *App) error {
				scoped.processTemplateStatusUpdate(wabaID, event, templateName, templateLanguage, reason)
				return nil
			}); err != nil {
				a.Log.Error("Failed to process tenant template status", "error", err, "organization_id", organizationID)
			}
		}
		return
	}

	if templateName == "" {
		a.Log.Warn("Template status update missing template name")
		return
	}

	// Keep status uppercase to match existing template status format
	// Events: APPROVED, REJECTED, PENDING, DISABLED, PENDING_DELETION, DELETED, REINSTATED, FLAGGED
	status := strings.ToUpper(event)

	// Find WhatsApp accounts that use this WABA ID (business_id field)
	var accounts []models.WhatsAppAccount
	if err := a.DB.Where("business_id = ?", wabaID).Find(&accounts).Error; err != nil {
		a.Log.Error("Failed to find WhatsApp accounts for WABA", "error", err, "waba_id", wabaID)
		return
	}

	if len(accounts) == 0 {
		a.Log.Warn("No WhatsApp accounts found for WABA", "waba_id", wabaID)
		return
	}

	// Update template for each account that has it
	for _, account := range accounts {
		// Find and update the template
		result := a.DB.Model(&models.Template{}).
			Where("whats_app_account = ? AND name = ? AND language = ?", account.Name, templateName, templateLanguage).
			Update("status", status)

		if result.Error != nil {
			a.Log.Error("Failed to update template status",
				"error", result.Error,
				"account", account.Name,
				"template", templateName,
				"language", templateLanguage,
			)
			continue
		}

		if result.RowsAffected > 0 {
			a.Log.Info("Updated template status from webhook",
				"account", account.Name,
				"template", templateName,
				"language", templateLanguage,
				"status", status,
				"reason", reason,
			)
		}
	}
}

// verifyMetaWebhookPayload verifies every dispatch target with its current
// organization-level Meta credential. A managed Integration Center row is
// authoritative; legacy account secrets are consulted only for organizations
// that have never created such a row. The platform secret remains available
// for payloads without any tenant/account target.
//
// It is not sufficient for any one referenced account to validate: a caller
// with one tenant's app secret could otherwise append forged changes for a
// different tenant and have the payload-wide signature accepted.
func (a *App) verifyMetaWebhookPayload(body, signature []byte, payload *WebhookPayload) bool {
	if len(signature) == 0 || payload == nil {
		return false
	}

	globalVerified := a.Config != nil && a.Config.WhatsApp.AppSecret != "" &&
		verifyWebhookSignature(body, signature, []byte(a.Config.WhatsApp.AppSecret))

	verifiedAnyTarget := false
	verifiedPhoneIDs := make(map[string]bool)
	verifiedWABAIDs := make(map[string]bool)
	for _, entry := range payload.Entry {
		for _, change := range entry.Changes {
			verifiedAnyTarget = true
			lifecycleChange := change.Field == "account_update" && isCoexistenceLifecycleEvent(change.Value.Event)
			phoneNumberID := strings.TrimSpace(change.Value.Metadata.PhoneNumberID)
			if lifecycleChange && phoneNumberID != "" {
				// Lifecycle dispatch targets entry.ID, not the metadata phone.
				// Check current ownership in the DB: a stale phone cache after a
				// WABA rebind must not authorize offboarding the prior WABA.
				orgID, err := a.resolveWhatsAppOrganization(phoneNumberID)
				if err != nil {
					return false
				}
				phoneVerified := false
				err = a.WithTenantApp(orgID, func(scoped *App) error {
					var account models.WhatsAppAccount
					if err := scoped.DB.Where(
						"organization_id = ? AND BTRIM(phone_id) = ? AND business_id = ?",
						orgID, phoneNumberID, strings.TrimSpace(entry.ID),
					).First(&account).Error; err != nil {
						return err
					}
					_, effectiveSecret, _, err := scoped.resolveEffectiveMetaAppCredsScoped(&account)
					phoneVerified = err == nil && strings.TrimSpace(effectiveSecret) != "" &&
						verifyWebhookSignature(body, signature, []byte(effectiveSecret))
					return err
				})
				if err != nil || !phoneVerified {
					return false
				}
			}
			if phoneNumberID != "" && !lifecycleChange {
				verified, checked := verifiedPhoneIDs[phoneNumberID]
				if !checked {
					account, err := a.getWhatsAppAccountCached(phoneNumberID)
					if err == nil {
						_, effectiveSecret, _, resolveErr := a.resolveEffectiveMetaAppCreds(account)
						verified = resolveErr == nil &&
							strings.TrimSpace(effectiveSecret) != "" &&
							verifyWebhookSignature(body, signature, []byte(effectiveSecret))
					}
					verifiedPhoneIDs[phoneNumberID] = verified
				}
				if !verified {
					return false
				}
				continue
			}

			wabaID := strings.TrimSpace(entry.ID)
			if wabaID == "" {
				return false
			}
			verified, checked := verifiedWABAIDs[wabaID]
			if !checked {
				verified = a.verifyMetaWABAPayload(body, signature, wabaID)
				verifiedWABAIDs[wabaID] = verified
			}
			if !verified {
				return false
			}
		}
	}

	// Payloads without a tenant/account target (for example an empty control
	// envelope) can only be authenticated by the platform-level secret.
	if !verifiedAnyTarget {
		return globalVerified
	}
	return true
}

// verifyMetaWABAPayload validates all accounts that a WABA-only event would
// update. Requiring every resolved account to share the signing authority keeps
// processTemplateStatusUpdate from crossing an app-secret boundary.
func (a *App) verifyMetaWABAPayload(body, signature []byte, wabaID string) bool {
	organizationIDs, err := a.resolveWABAOrganizations(wabaID)
	if err != nil || len(organizationIDs) == 0 {
		return false
	}

	verifiedAccounts := 0
	for _, organizationID := range organizationIDs {
		tenantVerified := false
		if err := a.WithTenantApp(organizationID, func(scoped *App) error {
			var accounts []models.WhatsAppAccount
			if err := scoped.DB.Where("organization_id = ? AND business_id = ?", organizationID, wabaID).Find(&accounts).Error; err != nil {
				return err
			}
			if len(accounts) == 0 {
				return nil
			}

			managed, err := scoped.metaIntegrationManaged(organizationID)
			if err != nil {
				return err
			}
			if managed {
				_, effectiveSecret, _, err := scoped.resolveEffectiveMetaAppCredsScoped(&accounts[0])
				if err != nil || strings.TrimSpace(effectiveSecret) == "" ||
					!verifyWebhookSignature(body, signature, []byte(effectiveSecret)) {
					return nil
				}
				tenantVerified = true
				verifiedAccounts += len(accounts)
				return nil
			}

			for i := range accounts {
				_, effectiveSecret, _, err := scoped.resolveEffectiveMetaAppCredsScoped(&accounts[i])
				if err != nil || strings.TrimSpace(effectiveSecret) == "" ||
					!verifyWebhookSignature(body, signature, []byte(effectiveSecret)) {
					return nil
				}
			}
			tenantVerified = true
			verifiedAccounts += len(accounts)
			return nil
		}); err != nil || !tenantVerified {
			return false
		}
	}
	return verifiedAccounts > 0
}

// verifyWebhookSignature verifies the X-Hub-Signature-256 header from Meta.
// The signature is HMAC-SHA256 of the request body using the App Secret.
func verifyWebhookSignature(body, signature, appSecret []byte) bool {
	// Signature format: "sha256=<hex_signature>"
	prefix := []byte("sha256=")
	if !bytes.HasPrefix(signature, prefix) {
		return false
	}

	expectedSig := bytes.TrimPrefix(signature, prefix)

	// Compute HMAC-SHA256
	mac := hmac.New(sha256.New, appSecret)
	mac.Write(body)
	computedSig := make([]byte, hex.EncodedLen(mac.Size()))
	hex.Encode(computedSig, mac.Sum(nil))

	// Constant-time comparison to prevent timing attacks
	return hmac.Equal(expectedSig, computedSig)
}

// processMarketingPreference updates a contact's marketing opt-out status
// based on the user_preferences webhook from Meta.
func (a *App) processMarketingPreference(
	phoneNumberID, userPhone, bsuid, value string,
) error {
	if a.rlsEnabled() && !a.hasTenantScope() {
		return a.withPhoneTenant(phoneNumberID, func(scoped *App) error {
			return scoped.processMarketingPreference(
				phoneNumberID,
				userPhone,
				bsuid,
				value,
			)
		})
	}

	optOut, err := marketingPreferenceOptOut(value)
	if err != nil {
		return err
	}

	// Find the WhatsApp account by phone_number_id
	var account models.WhatsAppAccount
	if err := a.DB.Where("phone_id = ?", phoneNumberID).First(&account).Error; err != nil {
		return fmt.Errorf("find account for marketing preference: %w", err)
	}

	if strings.TrimSpace(userPhone) == "" && strings.TrimSpace(bsuid) == "" {
		return errors.New("marketing preference has no phone or BSUID")
	}

	var updated models.Contact
	err = canonicalContactWriteTransaction(a.DB, func(tx *gorm.DB) error {
		// Marketing consent is a tenant-wide automatic-reply policy mutation.
		// Take the organization writer lock before any account or contact lock so
		// a STOP/RESUME webhook cannot commit across an in-flight AI provider
		// attempt that already made its final policy decision.
		if lockErr := database.LockOrganizationPolicyScope(
			tx,
			account.OrganizationID,
		); lockErr != nil {
			return fmt.Errorf("lock marketing preference policy scope: %w", lockErr)
		}

		contact, findErr := findMarketingPreferenceContact(
			tx,
			account.OrganizationID,
			userPhone,
			bsuid,
		)
		if errors.Is(findErr, gorm.ErrRecordNotFound) &&
			strings.TrimSpace(userPhone) != "" {
			contact, _, findErr = contactutil.GetOrCreateContact(
				tx,
				account.OrganizationID,
				userPhone,
				"",
			)
		}
		if findErr != nil {
			return fmt.Errorf("find marketing preference contact: %w", findErr)
		}

		canonical, resolveErr := contactutil.ResolveCanonicalContactForUpdate(
			tx,
			account.OrganizationID,
			contact.ID,
		)
		if resolveErr != nil {
			return fmt.Errorf(
				"resolve marketing preference contact: %w",
				resolveErr,
			)
		}

		updates := map[string]any{
			"marketing_opt_out": optOut,
		}
		if strings.TrimSpace(bsuid) != "" && canonical.BSUID != bsuid {
			updates["bs_uid"] = strings.TrimSpace(bsuid)
		}
		if canonical.WhatsAppAccount == "" {
			updates["whats_app_account"] = account.Name
		}
		result := tx.Model(&models.Contact{}).
			Where(
				"id = ? AND organization_id = ? AND merged_into_id IS NULL AND deleted_at IS NULL",
				canonical.ID,
				account.OrganizationID,
			).
			Updates(updates)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return contactutil.ErrCanonicalContactChanged
		}

		updated = *canonical
		updated.MarketingOptOut = optOut
		if resolvedBSUID, ok := updates["bs_uid"].(string); ok {
			updated.BSUID = resolvedBSUID
		}
		if resolvedAccount, ok := updates["whats_app_account"].(string); ok {
			updated.WhatsAppAccount = resolvedAccount
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("persist marketing preference: %w", err)
	}

	a.Log.Info("Marketing preference updated",
		"contact_id", updated.ID,
		"phone", userPhone,
		"bsuid", bsuid,
		"opt_out", optOut,
	)
	return nil
}

func findMarketingPreferenceContact(
	tx *gorm.DB,
	organizationID uuid.UUID,
	userPhone, bsuid string,
) (*models.Contact, error) {
	normalizedPhone := strings.TrimPrefix(strings.TrimSpace(userPhone), "+")
	if normalizedPhone != "" {
		var contact models.Contact
		err := tx.Unscoped().
			Where(
				"organization_id = ? AND phone_number IN ?",
				organizationID,
				[]string{normalizedPhone, "+" + normalizedPhone},
			).
			Order("CASE WHEN deleted_at IS NULL THEN 0 ELSE 1 END, created_at").
			First(&contact).Error
		if err == nil {
			return &contact, nil
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, err
		}
	}

	normalizedBSUID := strings.TrimSpace(bsuid)
	if normalizedBSUID != "" {
		var contact models.Contact
		err := tx.Unscoped().
			Where(
				"organization_id = ? AND bs_uid = ?",
				organizationID,
				normalizedBSUID,
			).
			Order("CASE WHEN deleted_at IS NULL THEN 0 ELSE 1 END, created_at").
			First(&contact).Error
		if err == nil {
			return &contact, nil
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, err
		}
	}
	return nil, gorm.ErrRecordNotFound
}

func marketingPreferenceOptOut(value string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "stop", "opt_out", "opted_out", "unsubscribe", "unsubscribed":
		return true, nil
	case "start", "resume", "opt_in", "opted_in", "subscribe", "subscribed":
		return false, nil
	default:
		return false, fmt.Errorf("unsupported marketing preference %q", value)
	}
}
