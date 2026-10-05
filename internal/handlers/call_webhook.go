package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/internal/websocket"
	"gorm.io/gorm"
)

// processCallWebhook handles a call webhook event for both incoming and outgoing calls.
// It creates/updates the CallLog and delegates to the CallManager for WebRTC handling.
func (a *App) processCallWebhook(phoneNumberID string, call any) {
	if a.rlsEnabled() && !a.hasTenantScope() {
		if err := a.withPhoneTenant(phoneNumberID, func(scoped *App) error {
			scoped.processCallWebhook(phoneNumberID, call)
			return nil
		}); err != nil {
			a.Log.Error("Failed to scope call webhook to tenant", "error", err, "phone_id", phoneNumberID)
		}
		return
	}

	// The webhook handler passes an anonymous struct. Convert via JSON round-trip.
	type callEvent struct {
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
		Duration int `json:"duration,omitempty"`
		// BizOpaqueCallbackData is the opaque string we set as `payload` on a
		// voice_call interactive button. Meta echoes it back here when the
		// customer taps the button. Carries `agent:<uuid>` for sticky routing;
		// parsed and acted on in a later PR — for now just logged so we can
		// confirm Meta is round-tripping the value.
		BizOpaqueCallbackData string `json:"biz_opaque_callback_data,omitempty"`
	}

	var ce callEvent
	b, _ := json.Marshal(call)
	if err := json.Unmarshal(b, &ce); err != nil {
		a.Log.Error("Failed to parse call event", "error", err)
		return
	}

	// Log raw payload to debug SDP and field mapping
	a.Log.Debug("Raw call webhook payload", "payload", string(b))

	// Surface the voice_call payload at info level so it shows up in
	// production logs ahead of sticky-routing landing. Quiet when the
	// caller didn't initiate via a voice_call button.
	if ce.BizOpaqueCallbackData != "" {
		a.Log.Info("Incoming call carries biz_opaque_callback_data",
			"call_id", ce.ID, "payload", ce.BizOpaqueCallbackData)
	}

	// Check if this call_id belongs to an existing outgoing session
	if a.CallManager != nil {
		session := a.CallManager.GetSession(ce.ID)
		if session != nil {
			requestOrgID, accountErr := a.resolveCallWebhookOrganizationID(phoneNumberID)
			if accountErr != nil {
				a.Log.Warn("Rejected call webhook with unresolved tenant",
					"call_id", ce.ID, "phone_id", phoneNumberID, "error", accountErr)
				return
			}
			if session.OrganizationID != requestOrgID {
				a.Log.Warn("Rejected call webhook session from another tenant",
					"call_id", ce.ID,
					"session_organization_id", session.OrganizationID,
					"request_organization_id", requestOrgID,
				)
				return
			}
		}
		if session != nil && session.Direction == models.CallDirectionOutgoing {
			sdp := ""
			if ce.Session != nil {
				sdp = ce.Session.SDP
			}
			a.CallManager.HandleOutgoingCallWebhook(ce.ID, ce.Event, sdp)
			return
		}
	}

	// Handle business-initiated events when session is already cleaned up
	// (e.g., terminate webhook arrives after PeerConnection closed)
	if ce.Direction == "BUSINESS_INITIATED" {
		requestOrgID, accountErr := a.resolveCallWebhookOrganizationID(phoneNumberID)
		if accountErr != nil {
			a.Log.Warn("Rejected orphaned outgoing call webhook with unresolved tenant",
				"call_id", ce.ID, "phone_id", phoneNumberID, "error", accountErr)
			return
		}
		a.handleOrphanedOutgoingCallEvent(requestOrgID, ce.ID, ce.Event, ce.Duration)
		return
	}

	// --- Incoming call flow ---

	// Look up the WhatsApp account
	account, err := a.getWhatsAppAccountCached(phoneNumberID)
	if err != nil {
		a.Log.Error("Failed to find WhatsApp account for call", "error", err, "phone_id", phoneNumberID)
		return
	}

	// Skip if phone number is missing (username user — BSUID-only calling not yet supported)
	if ce.From == "" {
		a.Log.Warn("Incoming call without phone number (username user), skipping",
			"bsuid", ce.FromUserID, "call_id", ce.ID)
		return
	}

	// Get or create the contact and its lifecycle event atomically.
	contact, _, err := a.getOrCreateInboundContact(account, ce.From, "", ce.FromUserID)
	if err != nil {
		a.Log.Error("Failed to get or create contact for call", "phone", ce.From, "error", err)
		return
	}

	now := time.Now()

	// Ensure a CallLog exists for this call. WhatsApp may send "connect" as the
	// first event (skipping "ringing"), so we create the record on demand.
	callLog, created := a.getOrCreateCallLog(account, contact, ce.ID, ce.From, now)
	if callLog == nil {
		return
	}

	// Replay guard. Meta replays a whole POST until it is acknowledged, so an
	// event may arrive again after it, or a later event of the same call, was
	// applied. An ended call accepts no further event: a replayed ringing or
	// connect must not ring agents, reopen the call log or start a new WebRTC
	// session, and a replayed end must not move ended_at or the duration. Only
	// the idempotent session cleanup still runs for an end event, in case a
	// session outlived the call log's end (for example an error event ended it
	// first).
	if incomingCallLogEnded(callLog.Status) {
		if a.CallManager != nil && isIncomingCallEndEvent(ce.Event) {
			a.CallManager.EndCall(ce.ID)
		}
		a.Log.Info("Ignoring call event for a call that has already ended",
			"call_id", ce.ID, "event", ce.Event, "status", callLog.Status)
		return
	}

	switch ce.Event {
	case "ringing":
		// Only the event that created the call log rings agents. A ringing
		// event for an existing call log is a replay, or arrived after a
		// later event of the call.
		if !created {
			a.Log.Info("Ignoring repeated call ringing event", "call_id", ce.ID, "status", callLog.Status)
			break
		}
		// Broadcast incoming call via WebSocket (no SDP yet, WebRTC starts on "connect")
		payload := map[string]any{
			"call_log_id":  callLog.ID.String(),
			"call_id":      ce.ID,
			"caller_phone": ce.From,
			"contact_id":   contact.ID.String(),
			"contact_name": contact.ProfileName,
			"ivr_flow_id":  callLog.IVRFlowID,
			"started_at":   now.Format(time.RFC3339),
		}
		// Sticky routing: if the customer clicked a voice_call button whose
		// payload tags the originating agent, ring just that agent. Falls
		// back to the org-wide broadcast on any failure (malformed payload,
		// wrong org, agent offline / unavailable).
		stickyAgentID := a.resolveStickyAgent(context.Background(), ce.BizOpaqueCallbackData, account.OrganizationID, contact.PhoneNumber)
		if stickyAgentID != nil {
			payload["sticky_agent_id"] = stickyAgentID.String()
			a.Log.Info("Sticky-routing incoming call to originating agent",
				"call_id", ce.ID, "agent_id", *stickyAgentID)
			a.WSHub.BroadcastToUser(account.OrganizationID, *stickyAgentID, websocket.WSMessage{
				Type:    websocket.TypeCallIncoming,
				Payload: payload,
			})
		} else {
			a.broadcastCallEvent(account.OrganizationID, websocket.TypeCallIncoming, payload)
		}

	case "connect":
		// "connect" carries the SDP offer from the consumer in session.sdp.
		// Extract SDP and start WebRTC negotiation.
		sdpOffer := ""
		if ce.Session != nil && ce.Session.SDPType == "offer" {
			sdpOffer = ce.Session.SDP
		}

		// Update call status to answered. Only the first connect (or in_call)
		// of a call that has not ended claims the transition; a replay finds
		// answered_at already set and changes nothing.
		if !a.claimIncomingCallAnswered(callLog, now) {
			a.Log.Info("Ignoring repeated call connect event", "call_id", ce.ID, "status", callLog.Status)
			break
		}

		// Delegate to CallManager with the SDP offer. Resolve the sticky
		// agent again here — Meta echoes biz_opaque_callback_data on every
		// call event, so we don't need to plumb state across the ringing →
		// connect gap.
		if a.IsCallingEnabledForOrg(account.OrganizationID) && sdpOffer != "" {
			session := a.CallManager.GetSession(ce.ID)
			if session == nil {
				stickyAgentID := a.resolveStickyAgent(context.Background(), ce.BizOpaqueCallbackData, account.OrganizationID, contact.PhoneNumber)
				a.CallManager.HandleIncomingCall(account, contact, callLog, sdpOffer, stickyAgentID)
			} else {
				a.CallManager.HandleCallEvent(ce.ID, ce.Event)
			}
		}

		a.broadcastCallEvent(account.OrganizationID, websocket.TypeCallAnswered, map[string]any{
			"call_id":     ce.ID,
			"contact_id":  contact.ID.String(),
			"answered_at": now.Format(time.RFC3339),
		})

	case "in_call":
		// Update call status to answered (once, as for connect)
		if !a.claimIncomingCallAnswered(callLog, now) {
			a.Log.Info("Ignoring repeated call in_call event", "call_id", ce.ID, "status", callLog.Status)
			break
		}

		// Notify CallManager if session exists
		if a.IsCallingEnabledForOrg(account.OrganizationID) {
			if session := a.CallManager.GetSession(ce.ID); session != nil {
				a.CallManager.HandleCallEvent(ce.ID, ce.Event)
			}
		}

		a.broadcastCallEvent(account.OrganizationID, websocket.TypeCallAnswered, map[string]any{
			"call_id":     ce.ID,
			"contact_id":  contact.ID.String(),
			"answered_at": now.Format(time.RFC3339),
		})

	case "ended", "terminate":
		// Calculate duration and determine final status.
		// Re-read the call log to get the latest agent_id (may have been set
		// by transfer acceptance after our initial read).
		a.DB.First(callLog, callLog.ID)

		duration := 0
		if callLog.AnsweredAt != nil {
			duration = int(now.Sub(*callLog.AnsweredAt).Seconds())
		}

		// For incoming calls that were pre-accepted for WebRTC but never reached
		// an agent (no transfer connected), mark as missed instead of completed.
		finalStatus := models.CallStatusCompleted
		if callLog.Direction == models.CallDirectionIncoming && callLog.AgentID == nil &&
			callLog.Status != models.CallStatusTransferring {
			finalStatus = models.CallStatusMissed
		}

		updates := map[string]any{
			"status":   finalStatus,
			"ended_at": now,
			"duration": duration,
		}
		// Only set disconnected_by if not already set (agent hangup sets it first)
		if callLog.DisconnectedBy == "" {
			updates["disconnected_by"] = models.DisconnectedByClient
		}
		ended := a.endIncomingCallLog(callLog, updates)

		// Notify CallManager to clean up
		if a.CallManager != nil {
			a.CallManager.EndCall(ce.ID)
		}
		if !ended {
			// A concurrent delivery of this or another end event ended the
			// call first and already announced it.
			a.Log.Info("Ignoring repeated call end event", "call_id", ce.ID, "event", ce.Event)
			break
		}

		disconnectedBy := string(callLog.DisconnectedBy)
		if disconnectedBy == "" {
			disconnectedBy = "client"
		}
		a.broadcastCallEvent(account.OrganizationID, websocket.TypeCallEnded, map[string]any{
			"call_id":         ce.ID,
			"contact_id":      contact.ID.String(),
			"status":          string(finalStatus),
			"duration":        duration,
			"ended_at":        now.Format(time.RFC3339),
			"disconnected_by": disconnectedBy,
		})

	case "missed", "unanswered":
		if !a.endIncomingCallLog(callLog, map[string]any{
			"status":          models.CallStatusMissed,
			"ended_at":        now,
			"disconnected_by": models.DisconnectedByClient,
		}) {
			a.Log.Info("Ignoring repeated call end event", "call_id", ce.ID, "event", ce.Event)
			break
		}

		a.broadcastCallEvent(account.OrganizationID, websocket.TypeCallEnded, map[string]any{
			"call_id":    ce.ID,
			"contact_id": contact.ID.String(),
			"status":     string(models.CallStatusMissed),
			"ended_at":   now.Format(time.RFC3339),
		})

	default:
		a.Log.Warn("Unknown call event", "event", ce.Event, "call_id", ce.ID)
	}

	// Handle error in call event
	if ce.Error != nil {
		a.DB.Model(&models.CallLog{}).
			Where("whatsapp_call_id = ? AND organization_id = ?", ce.ID, account.OrganizationID).
			Updates(map[string]any{
				"status":          models.CallStatusFailed,
				"error_message":   ce.Error.Message,
				"ended_at":        now,
				"disconnected_by": models.DisconnectedBySystem,
			})
	}
}

// resolveCallWebhookOrganizationID returns the tenant established by the RLS
// wrapper, or resolves it from the webhook phone number when RLS is disabled.
// Call IDs are not tenant credentials and must never be used on their own to
// authorize an in-memory session or database record mutation.
func (a *App) resolveCallWebhookOrganizationID(phoneNumberID string) (uuid.UUID, error) {
	if a.tenantOrgID != uuid.Nil {
		return a.tenantOrgID, nil
	}
	account, err := a.getWhatsAppAccountCached(phoneNumberID)
	if err != nil {
		return uuid.Nil, err
	}
	return account.OrganizationID, nil
}

// endedIncomingCallStatuses are the call log statuses after which an incoming
// call accepts no further webhook event.
var endedIncomingCallStatuses = []models.CallStatus{
	models.CallStatusCompleted,
	models.CallStatusMissed,
	models.CallStatusRejected,
	models.CallStatusFailed,
}

func incomingCallLogEnded(status models.CallStatus) bool {
	for _, ended := range endedIncomingCallStatuses {
		if status == ended {
			return true
		}
	}
	return false
}

func isIncomingCallEndEvent(event string) bool {
	switch event {
	case "ended", "terminate", "missed", "unanswered":
		return true
	default:
		return false
	}
}

// claimIncomingCallAnswered marks the call log answered at now, unless it was
// already answered or has ended. It reports whether this event made the
// transition; a replayed connect or in_call does not. The check and the write
// are one statement, so two concurrent deliveries cannot both claim it. A
// failed write is logged and treated as claimed, as before this guard, so a
// database error never stops a call from being answered.
func (a *App) claimIncomingCallAnswered(callLog *models.CallLog, now time.Time) bool {
	result := a.DB.Model(&models.CallLog{}).
		Where("id = ? AND organization_id = ? AND answered_at IS NULL AND status NOT IN ?",
			callLog.ID, callLog.OrganizationID, endedIncomingCallStatuses).
		Updates(map[string]any{
			"status":      models.CallStatusAnswered,
			"answered_at": now,
		})
	if result.Error != nil {
		a.Log.Error("Failed to record answered call", "error", result.Error, "call_log_id", callLog.ID)
		return true
	}
	if result.RowsAffected == 0 {
		return false
	}
	callLog.Status = models.CallStatusAnswered
	callLog.AnsweredAt = &now
	return true
}

// endIncomingCallLog applies updates (which end the call) unless the call log
// has already ended, and reports whether this event ended it. As for
// claimIncomingCallAnswered, a failed write is logged and treated as applied.
func (a *App) endIncomingCallLog(callLog *models.CallLog, updates map[string]any) bool {
	result := a.DB.Model(&models.CallLog{}).
		Where("id = ? AND organization_id = ? AND status NOT IN ?",
			callLog.ID, callLog.OrganizationID, endedIncomingCallStatuses).
		Updates(updates)
	if result.Error != nil {
		a.Log.Error("Failed to record ended call", "error", result.Error, "call_log_id", callLog.ID)
		return true
	}
	return result.RowsAffected > 0
}

// getOrCreateCallLog finds an existing CallLog by WhatsApp call ID, or creates one
// if it doesn't exist. This handles cases where WhatsApp skips the "ringing" event
// and sends "connect" as the first event. created reports whether this call
// created the CallLog.
func (a *App) getOrCreateCallLog(account *models.WhatsAppAccount, contact *models.Contact, callID, callerPhone string, now time.Time) (callLogResult *models.CallLog, created bool) {
	var callLog models.CallLog
	err := a.DB.Where("whatsapp_call_id = ? AND organization_id = ?", callID, account.OrganizationID).
		First(&callLog).Error
	if err == nil {
		return &callLog, false
	}

	// Create a new call log
	callLog = models.CallLog{
		BaseModel:       models.BaseModel{ID: uuid.New()},
		OrganizationID:  account.OrganizationID,
		WhatsAppAccount: account.Name,
		ContactID:       contact.ID,
		WhatsAppCallID:  callID,
		CallerPhone:     callerPhone,
		Status:          models.CallStatusRinging,
		StartedAt:       &now,
	}

	// Find the call-start IVR flow for this account (cached)
	if flow := a.CallManager.GetIVRFlowByConfig(account.OrganizationID, account.Name, "call_start"); flow != nil {
		callLog.IVRFlowID = &flow.ID
	}

	if err := a.DB.Create(&callLog).Error; err != nil {
		a.Log.Error("Failed to create call log", "error", err)
		return nil, false
	}

	a.Log.Info("Created call log", "call_id", callID, "call_log_id", callLog.ID)
	return &callLog, true
}

// handleOrphanedOutgoingCallEvent handles business-initiated call webhooks
// when the session has already been cleaned up (e.g., terminate arrives after
// PeerConnection closed). Updates the call log and broadcasts WebSocket events.
func (a *App) handleOrphanedOutgoingCallEvent(organizationID uuid.UUID, callID, event string, duration int) {
	if organizationID == uuid.Nil {
		a.Log.Warn("Rejected orphaned outgoing call event without tenant", "call_id", callID, "event", event)
		return
	}

	// Find the call log by WhatsApp call ID
	var callLog models.CallLog
	if err := a.DB.Where("whatsapp_call_id = ? AND organization_id = ?", callID, organizationID).
		First(&callLog).Error; err != nil {
		a.Log.Debug("No call log found for orphaned outgoing event", "call_id", callID, "event", event)
		return
	}

	now := time.Now()

	switch event {
	case "terminate":
		finalStatus := models.CallStatusCompleted
		if callLog.AnsweredAt == nil {
			finalStatus = models.CallStatusMissed
		}

		// Replay guard: a terminate that would change nothing but ended_at
		// was already applied (by this event's earlier delivery, or by an
		// agent hangup that recorded the same outcome) and was announced.
		if callLog.EndedAt != nil && callLog.Status == finalStatus &&
			(duration <= 0 || callLog.Duration == duration) {
			a.Log.Info("Ignoring repeated orphaned outgoing call terminate", "call_id", callID, "status", callLog.Status)
			return
		}

		// A call that has already ended keeps its first ended_at; this
		// terminate only corrects its status or duration (Meta's duration
		// replaces the one an agent hangup computed).
		endedAt := now
		updates := map[string]any{
			"status": finalStatus,
		}
		if callLog.EndedAt != nil {
			endedAt = *callLog.EndedAt
		} else {
			updates["ended_at"] = now
		}
		if duration > 0 {
			updates["duration"] = duration
		}
		// Only set disconnected_by if not already set (agent hangup sets it first)
		if callLog.DisconnectedBy == "" {
			updates["disconnected_by"] = models.DisconnectedByClient
		}
		// The statement repeats the replay guard, so that of two concurrent
		// deliveries of this terminate only one applies and announces it. A
		// failed write is logged and announced, as before this guard.
		result := a.DB.Model(&models.CallLog{}).
			Where("id = ? AND organization_id = ?", callLog.ID, organizationID).
			Where("NOT (ended_at IS NOT NULL AND status = ? AND (? <= 0 OR duration = ?))",
				finalStatus, duration, duration).
			Updates(updates)
		if result.Error != nil {
			a.Log.Error("Failed to record orphaned outgoing call terminate", "error", result.Error, "call_id", callID)
		} else if result.RowsAffected == 0 {
			a.Log.Info("Ignoring repeated orphaned outgoing call terminate", "call_id", callID, "status", finalStatus)
			return
		}

		a.broadcastCallEvent(callLog.OrganizationID, websocket.TypeOutgoingCallEnded, map[string]any{
			"call_log_id": callLog.ID.String(),
			"call_id":     callID,
			"status":      string(finalStatus),
			"duration":    duration,
			"ended_at":    endedAt.Format(time.RFC3339),
		})

		a.Log.Info("Handled orphaned outgoing call terminate", "call_id", callID, "duration", duration)
	default:
		a.Log.Debug("Ignoring orphaned outgoing call event", "call_id", callID, "event", event)
	}
}

// processCallStatusWebhook handles business-initiated call status webhooks
// (RINGING, ACCEPTED, REJECTED) that arrive in the statuses array under field="calls".
func (a *App) processCallStatusWebhook(phoneNumberID string, status WebhookStatus) {
	if a.rlsEnabled() && !a.hasTenantScope() {
		if err := a.withPhoneTenant(phoneNumberID, func(scoped *App) error {
			scoped.processCallStatusWebhook(phoneNumberID, status)
			return nil
		}); err != nil {
			a.Log.Error("Failed to scope call status webhook to tenant",
				"error", err, "phone_id", phoneNumberID, "call_id", status.ID)
		}
		return
	}

	if a.CallManager == nil {
		return
	}

	// Map uppercase status to event name used by HandleOutgoingCallWebhook
	var event string
	switch status.Status {
	case "RINGING":
		event = "ringing"
	case "ACCEPTED":
		event = "accepted"
	case "REJECTED":
		event = "rejected"
	default:
		a.Log.Warn("Unknown call status", "status", status.Status, "call_id", status.ID)
		return
	}

	session := a.CallManager.GetSession(status.ID)
	if session == nil {
		a.Log.Debug("Ignoring call status webhook for unknown session", "call_id", status.ID, "status", status.Status)
		return
	}
	if session.Direction != models.CallDirectionOutgoing {
		a.Log.Warn("Rejected outgoing call status for non-outgoing session",
			"call_id", status.ID, "direction", session.Direction)
		return
	}

	requestOrgID, err := a.resolveCallWebhookOrganizationID(phoneNumberID)
	if err != nil {
		a.Log.Warn("Rejected call status webhook with unresolved tenant",
			"call_id", status.ID, "phone_id", phoneNumberID, "error", err)
		return
	}
	if session.OrganizationID != requestOrgID {
		a.Log.Warn("Rejected call status webhook session from another tenant",
			"call_id", status.ID,
			"session_organization_id", session.OrganizationID,
			"request_organization_id", requestOrgID,
		)
		return
	}

	a.CallManager.HandleOutgoingCallWebhook(status.ID, event, "")
}

// CallPermissionReplyData holds the parsed call_permission_reply webhook data.
type CallPermissionReplyData struct {
	Response            string `json:"response"`
	IsPermanent         bool   `json:"is_permanent"`
	ExpirationTimestamp int64  `json:"expiration_timestamp,omitempty"`
	ResponseSource      string `json:"response_source"`
	// RepliedAt is the reply message's own timestamp (Unix seconds), which
	// Meta keeps on its retries; 0 when the payload carries none.
	RepliedAt int64 `json:"-"`
}

// callPermissionReplyRequestSkew is how much earlier than a permission row's
// requested_at a reply may be stamped and still answer that request. The row
// is created only after the Graph send returns (bounded by
// whatsapp.DefaultTimeout, 30s), while Meta may answer an automatic reply as
// soon as it accepts the request; the margin also absorbs clock skew.
const callPermissionReplyRequestSkew = 2 * time.Minute

// callPermissionReplyIsStale reports whether a reply stamped repliedAt must not
// be applied to permission, the newest permission row of the contact: it is a
// replay of, or older than, the response already stored (responded_at is the
// stored reply's own time, or the time it was processed for rows stored
// before replies were ordered), or it predates the request the row records.
// A reply without a timestamp is never stale, as before this guard.
func callPermissionReplyIsStale(permission *models.CallPermission, status models.CallPermissionStatus, repliedAt int64) bool {
	if permission == nil || repliedAt <= 0 {
		return false
	}
	if permission.RespondedAt != nil {
		stored := permission.RespondedAt.Unix()
		return repliedAt < stored || (repliedAt == stored && permission.Status == status)
	}
	if permission.RequestedAt.IsZero() {
		return false
	}
	return time.Unix(repliedAt, 0).Before(permission.RequestedAt.Add(-callPermissionReplyRequestSkew))
}

// processCallPermissionReply handles the call_permission_reply interactive webhook.
// Updates the CallPermission record in the DB when the user accepts or rejects.
func (a *App) processCallPermissionReply(phoneNumberID, fromPhone string, reply *CallPermissionReplyData) {
	if a.rlsEnabled() && !a.hasTenantScope() {
		if err := a.withPhoneTenant(phoneNumberID, func(scoped *App) error {
			scoped.processCallPermissionReply(phoneNumberID, fromPhone, reply)
			return nil
		}); err != nil {
			a.Log.Error("Failed to scope call permission reply to tenant", "error", err, "phone_id", phoneNumberID)
		}
		return
	}

	account, err := a.getWhatsAppAccountCached(phoneNumberID)
	if err != nil {
		a.Log.Error("Failed to find account for call permission reply", "error", err)
		return
	}

	// Find the most recent pending permission for this contact
	var contact models.Contact
	if err := a.DB.Where("organization_id = ? AND phone_number = ?", account.OrganizationID, fromPhone).
		First(&contact).Error; err != nil {
		a.Log.Warn("No contact found for call permission reply", "phone", fromPhone)
		return
	}

	// Load the most recent permission request for this contact. If none exists
	// (e.g. the permission prompt was sent out-of-band by Meta, or the request
	// went out while outside business calling hours), create one from the reply
	// so the grant/decline is captured instead of being dropped.
	var permission models.CallPermission
	isNewPermission := false
	if err := a.DB.Where("organization_id = ? AND contact_id = ?", account.OrganizationID, contact.ID).
		Order("created_at DESC").
		First(&permission).Error; err != nil {
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			a.Log.Error("Failed to load call permission for reply", "error", err, "contact_id", contact.ID)
			return
		}
		isNewPermission = true
		permission = models.CallPermission{
			BaseModel:       models.BaseModel{ID: uuid.New()},
			OrganizationID:  account.OrganizationID,
			ContactID:       contact.ID,
			WhatsAppAccount: account.Name,
			Status:          models.CallPermissionPending,
		}
		a.Log.Info("No prior permission record for reply; creating one (out-of-band grant)", "contact_id", contact.ID)
	}

	newStatus := models.CallPermissionDeclined
	if reply.Response == "accept" {
		newStatus = models.CallPermissionAccepted
	}
	// Replay guard: Meta replays a whole POST until it is acknowledged, and
	// this reply may also arrive after a newer one. Only a reply newer than
	// the stored response, and not older than the request, is applied.
	if !isNewPermission && callPermissionReplyIsStale(&permission, newStatus, reply.RepliedAt) {
		a.Log.Info("Ignoring replayed or older call permission reply",
			"contact_id", contact.ID,
			"permission_id", permission.ID,
			"replied_at", reply.RepliedAt,
			"stored_status", permission.Status,
		)
		return
	}
	previousRespondedAt := permission.RespondedAt

	// responded_at records the reply's own time when Meta stamped it, so that
	// the next reply is ordered against it rather than against the time this
	// one happened to be processed.
	now := time.Now()
	if reply.RepliedAt > 0 {
		now = time.Unix(reply.RepliedAt, 0)
	}
	permission.RespondedAt = &now

	var expiresAt *time.Time
	if reply.Response == "accept" {
		permission.Status = models.CallPermissionAccepted
		if reply.ExpirationTimestamp > 0 {
			t := time.Unix(reply.ExpirationTimestamp, 0)
			expiresAt = &t
			permission.ExpiresAt = &t
		}
		a.Log.Info("Call permission accepted",
			"contact_id", contact.ID,
			"is_permanent", reply.IsPermanent,
			"expiration", reply.ExpirationTimestamp,
		)
	} else {
		permission.Status = models.CallPermissionDeclined
		a.Log.Info("Call permission declined", "contact_id", contact.ID)
	}

	if isNewPermission {
		if err := a.DB.Create(&permission).Error; err != nil {
			a.Log.Error("Failed to create call permission from reply", "error", err, "contact_id", contact.ID)
			return
		}
	} else {
		updates := map[string]any{
			"status":       permission.Status,
			"responded_at": now,
		}
		if expiresAt != nil {
			updates["expires_at"] = *expiresAt
		}
		// Compare-and-set on the responded_at read above, so that a concurrent
		// delivery of this reply (each runs in its own goroutine) applies and
		// announces it once.
		result := a.DB.Model(&models.CallPermission{}).
			Where("id = ? AND organization_id = ? AND responded_at IS NOT DISTINCT FROM ?",
				permission.ID, account.OrganizationID, previousRespondedAt).
			Updates(updates)
		if result.Error != nil {
			a.Log.Error("Failed to update call permission from reply", "error", result.Error, "permission_id", permission.ID)
			return
		}
		if result.RowsAffected == 0 {
			a.Log.Info("Call permission changed concurrently; reply not applied", "permission_id", permission.ID)
			return
		}
	}

	// Broadcast permission update to agents via WebSocket
	wsPayload := map[string]any{
		"contact_id":    contact.ID,
		"contact_phone": contact.PhoneNumber,
		"contact_name":  contact.ProfileName,
		"status":        permission.Status,
	}
	if expiresAt != nil {
		wsPayload["expires_at"] = expiresAt.Format(time.RFC3339)
	}
	a.broadcastCallEvent(account.OrganizationID, websocket.TypeCallPermissionUpdate, wsPayload)
}

// validateStickyAgent runs the per-call eligibility checks (same-org,
// IsActive, IsAvailable, online) on a candidate agent. Returns the id on
// pass, nil on fail (with the reason logged). Used by both sticky-agent
// sources in resolveStickyAgent.
func (a *App) validateStickyAgent(agentID, orgID uuid.UUID) *uuid.UUID {
	var user models.User
	if err := a.DB.Where(
		"id = ? AND organization_id = ? AND is_active = ? AND is_available = ?",
		agentID, orgID, true, true,
	).First(&user).Error; err != nil {
		a.Log.Info("Sticky-route skipped: agent not eligible",
			"agent_id", agentID, "org_id", orgID, "reason", err.Error())
		return nil
	}
	if a.WSHub == nil || !a.WSHub.IsUserOnline(orgID, agentID) {
		a.Log.Info("Sticky-route skipped: agent offline",
			"agent_id", agentID)
		return nil
	}
	return &agentID
}

// stickyCallKey returns the Redis key for a pending voice_call sticky
// route. Keyed by (org, caller-phone) because the incoming-call webhook
// gives us the phone before any contact lookup, and it's the same value
// the sender writes (contact.PhoneNumber).
func stickyCallKey(orgID uuid.UUID, phone string) string {
	return "vc_sticky:" + orgID.String() + ":" + phone
}

// MarkPendingStickyCall stores the originating agent id when an outbound
// voice_call button is sent, with a TTL matching the button's clickable
// lifetime. When the customer taps the button and our number rings, the
// call_webhook handler reads this back to route directly to the agent
// who sent it.
//
// Best-effort: a Redis failure logs and degrades to today's default
// (org-wide broadcast + IVR). Doesn't error out a successful send.
func (a *App) MarkPendingStickyCall(ctx context.Context, orgID uuid.UUID, phone string, agentID uuid.UUID, ttlMinutes int) {
	if a.Redis == nil || phone == "" {
		return
	}
	if ttlMinutes <= 0 {
		ttlMinutes = 15 // Meta's default for voice_call buttons
	}
	if err := a.Redis.Set(ctx, stickyCallKey(orgID, phone), agentID.String(),
		time.Duration(ttlMinutes)*time.Minute).Err(); err != nil {
		a.Log.Warn("Failed to mark pending sticky call in Redis",
			"error", err, "phone", phone)
	}
}

// findStickyAgentInRedis returns the agent id stored when the outbound
// voice_call button was sent (set by MarkPendingStickyCall), or nil if
// no key, expired, malformed, or Redis is unhealthy. Nil is a graceful
// signal — the caller falls through to today's default routing.
func (a *App) findStickyAgentInRedis(ctx context.Context, orgID uuid.UUID, phone string) *uuid.UUID {
	if a.Redis == nil || phone == "" {
		return nil
	}
	val, err := a.Redis.Get(ctx, stickyCallKey(orgID, phone)).Result()
	if err != nil {
		return nil
	}
	agentID, err := uuid.Parse(val)
	if err != nil {
		a.Log.Warn("Stored sticky-call agent id is malformed", "value", val)
		return nil
	}
	return &agentID
}

// resolveStickyAgent picks the agent (if any) who should receive this
// incoming call. Two sources are tried in order:
//
//  1. The voice_call button's `payload` echoed back by Meta. As of
//     2026-05, Meta does not surface this on the call webhook; we keep
//     the parsing for forward-compat in case they add it later.
//  2. Redis: a key set by MarkPendingStickyCall when the outbound
//     button was sent, with a TTL matching the button's clickable
//     lifetime.
//
// Either source's result goes through validateStickyAgent so the agent
// must still be in the same org, on-shift, and online. On any failure
// return nil and let the caller fall back to today's org-wide broadcast.
//
// Why Redis instead of a DB lookup: O(1) GET vs an unindexed JSONB scan
// of `messages`, and the TTL is enforced by Redis natively (no
// "last 60 min" window math).
func (a *App) resolveStickyAgent(ctx context.Context, rawPayload string, orgID uuid.UUID, callerPhone string) *uuid.UUID {
	// Source 1: Meta-echoed payload.
	if suffix, ok := strings.CutPrefix(rawPayload, "agent:"); ok {
		if agentID, err := uuid.Parse(suffix); err == nil {
			if validated := a.validateStickyAgent(agentID, orgID); validated != nil {
				a.Log.Info("Sticky-route: matched on Meta-echoed payload",
					"agent_id", agentID, "phone", callerPhone)
				return validated
			}
		} else {
			a.Log.Info("Sticky-route: malformed agent id in payload",
				"payload", rawPayload, "error", err)
		}
	}

	// Source 2: pending sticky-call key set when the button was sent.
	if originator := a.findStickyAgentInRedis(ctx, orgID, callerPhone); originator != nil {
		if validated := a.validateStickyAgent(*originator, orgID); validated != nil {
			a.Log.Info("Sticky-route: matched on Redis pending key",
				"agent_id", *originator, "phone", callerPhone)
			return validated
		}
	}

	return nil
}

// broadcastCallEvent sends a call event to all connected clients in an organization
func (a *App) broadcastCallEvent(orgID uuid.UUID, eventType string, payload map[string]any) {
	if a.WSHub == nil {
		return
	}
	a.WSHub.BroadcastToOrg(orgID, websocket.WSMessage{
		Type:    eventType,
		Payload: payload,
	})
}
