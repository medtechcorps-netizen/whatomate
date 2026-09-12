package handlers

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/contactutil"
	"github.com/shridarpatil/whatomate/internal/database"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/internal/websocket"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// SLAProcessor handles periodic SLA checks and escalations
type SLAProcessor struct {
	app      *App
	interval time.Duration
	stopCh   chan struct{}
}

// NewSLAProcessor creates a new SLA processor
func NewSLAProcessor(app *App, interval time.Duration) *SLAProcessor {
	return &SLAProcessor{
		app:      app,
		interval: interval,
		stopCh:   make(chan struct{}),
	}
}

// Start begins the SLA processing loop
func (p *SLAProcessor) Start(ctx context.Context) {
	p.app.Log.Info("SLA processor started", "interval", p.interval)

	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			p.app.Log.Info("SLA processor stopped by context")
			return
		case <-p.stopCh:
			p.app.Log.Info("SLA processor stopped")
			return
		case <-ticker.C:
			p.processStaleTransfers()
		}
	}
}

// Stop stops the SLA processor
func (p *SLAProcessor) Stop() {
	select {
	case <-p.stopCh:
	default:
		close(p.stopCh)
	}
}

// processStaleTransfers checks for transfers that need escalation or auto-close
func (p *SLAProcessor) processStaleTransfers() {
	now := time.Now()

	// Resolve organizations from the control plane, then enter explicit tenant
	// scopes for human SLA work and chatbot timer selection/attempts.
	var organizationIDs []uuid.UUID
	if err := p.app.rootApp().DB.Scopes(database.ExcludePlatformComplianceOrganizations).
		Model(&models.Organization{}).
		Pluck("id", &organizationIDs).Error; err != nil {
		p.app.Log.Error("Failed to list organizations for SLA processing", "error", err)
		return
	}

	for _, organizationID := range organizationIDs {
		var inactivitySettings *models.ChatbotSettings
		if err := p.app.WithTenantApp(organizationID, func(scoped *App) error {
			settings, err := scoped.getChatbotSettingsCached(organizationID, "")
			if err != nil {
				return nil // Organization has no chatbot/SLA settings yet.
			}
			if !settings.SLA.Enabled && !settings.ClientInactivity.ReminderEnabled {
				return nil
			}
			tenantProcessor := *p
			tenantProcessor.app = scoped
			tenantProcessor.processOrganizationSLA(*settings, now)
			if settings.ClientInactivity.ReminderEnabled {
				copiedSettings := *settings
				inactivitySettings = &copiedSettings
			}
			return nil
		}); err != nil {
			p.app.Log.Error("Failed to process organization SLA",
				"error", err,
				"organization_id", organizationID,
			)
			continue
		}
		// Human SLA mutations may retain organization/contact locks until the
		// tenant transaction commits. Never nest an independent timer attempt
		// inside it, or pin a third connection while its sender needs the pool.
		if inactivitySettings != nil {
			p.processClientInactivity(organizationID, *inactivitySettings, now)
		}
	}
}

// processOrganizationSLA processes SLA for a single organization
func (p *SLAProcessor) processOrganizationSLA(settings models.ChatbotSettings, now time.Time) {
	orgID := settings.OrganizationID

	// 1. Auto-close expired transfers
	if settings.SLA.AutoCloseHours > 0 {
		p.autoCloseExpiredTransfers(orgID, settings, now)
	}

	// 2. Escalate transfers past escalation deadline
	if settings.SLA.EscalationMinutes > 0 {
		p.escalateTransfers(orgID, settings, now)
	}

	// 3. Mark SLA breached for transfers past response deadline
	if settings.SLA.ResponseMinutes > 0 {
		p.markSLABreached(orgID, now)
	}
}

// autoCloseExpiredTransfers closes transfers that have exceeded their expiry time
func (p *SLAProcessor) autoCloseExpiredTransfers(orgID uuid.UUID, settings models.ChatbotSettings, now time.Time) {
	var transfers []models.AgentTransfer
	if err := p.app.DB.Where(
		"organization_id = ? AND status = ? AND expires_at IS NOT NULL AND expires_at < ?",
		orgID, models.TransferStatusActive, now,
	).Find(&transfers).Error; err != nil {
		p.app.Log.Error("Failed to find expired transfers", "error", err, "org_id", orgID)
		return
	}

	closedCount := 0
	for _, transfer := range transfers {
		// Check if the assigned agent has been actively responding.
		// If so, extend the expiry deadline instead of auto-closing.
		deadline := transfer.SLA.ExpiresAt
		if deadline != nil && p.agentRespondedSince(transfer, deadline.Add(-time.Duration(settings.SLA.AutoCloseHours)*time.Hour)) {
			newExpiry := now.Add(time.Duration(settings.SLA.AutoCloseHours) * time.Hour)
			if err := p.app.DB.Model(&transfer).Update("expires_at", newExpiry).Error; err != nil {
				p.app.Log.Error("Failed to extend transfer expiry", "error", err, "transfer_id", transfer.ID)
			} else {
				p.app.Log.Info("Extended transfer expiry due to agent activity",
					"transfer_id", transfer.ID,
					"new_expires_at", newExpiry,
				)
			}
			// Also record first_response_at if not yet set
			p.app.UpdateSLAOnFirstResponse(&transfer)
			if transfer.SLA.FirstResponseAt != nil {
				p.app.DB.Model(&transfer).Update("first_response_at", transfer.SLA.FirstResponseAt)
			}
			continue
		}

		// Send auto-close message to customer if configured
		if settings.SLA.AutoCloseMessage != "" {
			p.sendSLATextToCustomer(transfer, "SLA auto-close message", settings.SLA.AutoCloseMessage)
		}

		// Expiry changes the physical-contact AI policy. Serialize it against a
		// currently running AI provider attempt, but keep the human SLA message
		// above outside that fence.
		expireErr := database.WithTenantReadCommitted(
			p.app.DB,
			orgID,
			func(tx *gorm.DB) error {
				if err := database.LockOrganizationPolicyScope(tx, orgID); err != nil {
					return err
				}
				if err := database.LockContactPolicyScope(
					tx,
					orgID,
					transfer.ContactID,
				); err != nil {
					return err
				}
				result := tx.Model(&models.AgentTransfer{}).Where(
					"id = ? AND organization_id = ? AND status = ?",
					transfer.ID,
					orgID,
					models.TransferStatusActive,
				).Updates(map[string]any{
					"status":     models.TransferStatusExpired,
					"resumed_at": now,
					"notes":      transfer.Notes + "\n[Auto-closed: No agent response within SLA]",
				})
				if result.Error != nil {
					return result.Error
				}
				if result.RowsAffected != 1 {
					return errors.New("transfer expiry lost its active policy state")
				}
				return nil
			},
		)
		if expireErr != nil {
			p.app.Log.Error("Failed to expire transfer", "error", expireErr, "transfer_id", transfer.ID)
			continue
		}

		closedCount++
		p.app.Log.Info("Transfer auto-closed due to expiry",
			"transfer_id", transfer.ID,
			"contact_id", transfer.ContactID,
			"expires_at", transfer.SLA.ExpiresAt,
		)

		// Broadcast update
		p.broadcastTransferUpdate(transfer, websocket.TypeTransferExpired)
	}

	if closedCount > 0 {
		p.app.Log.Info("Auto-closed expired transfers", "count", closedCount, "org_id", orgID)
	}
}

// escalateTransfers escalates transfers past their escalation deadline
func (p *SLAProcessor) escalateTransfers(orgID uuid.UUID, settings models.ChatbotSettings, now time.Time) {
	var transfers []models.AgentTransfer
	if err := p.app.DB.Where(
		"organization_id = ? AND status = ? AND sla_escalation_at IS NOT NULL AND sla_escalation_at < ? AND escalation_level < 2",
		orgID, models.TransferStatusActive, now,
	).Find(&transfers).Error; err != nil {
		p.app.Log.Error("Failed to find transfers for escalation", "error", err, "org_id", orgID)
		return
	}

	escalatedCount := 0
	for _, transfer := range transfers {
		// Anchor the escalation window at the customer's last incoming message
		// (or the transfer's start when the customer hasn't spoken since). The
		// agent is "responsive" only if they've replied *after* the customer's
		// latest message — so a conversation where both sides have wound down
		// stays in steady state instead of repeatedly warning the customer that
		// "we'll respond shortly" when there's nothing pending. Previous
		// behavior keyed off the escalation deadline minus EscalationMinutes,
		// which meant agent silence alone triggered the warning regardless of
		// whether the customer was actually waiting.
		since := p.customerLastSpokeAt(transfer)
		if p.agentRespondedSince(transfer, since) {
			newEscalation := now.Add(time.Duration(settings.SLA.EscalationMinutes) * time.Minute)
			if err := p.app.DB.Model(&transfer).Update("sla_escalation_at", newEscalation).Error; err != nil {
				p.app.Log.Error("Failed to extend transfer escalation", "error", err, "transfer_id", transfer.ID)
			} else {
				p.app.Log.Info("Extended transfer escalation — agent has replied since customer's last message",
					"transfer_id", transfer.ID,
					"new_escalation_at", newEscalation,
				)
			}
			// Also record first_response_at if not yet set
			p.app.UpdateSLAOnFirstResponse(&transfer)
			if transfer.SLA.FirstResponseAt != nil {
				p.app.DB.Model(&transfer).Update("first_response_at", transfer.SLA.FirstResponseAt)
			}
			continue
		}

		newLevel := transfer.SLA.EscalationLevel + 1

		// Update transfer
		updates := map[string]any{
			"escalation_level": newLevel,
			"escalated_at":     now,
		}

		// If not yet breached and past response deadline, mark as breached
		if !transfer.SLA.Breached && transfer.SLA.ResponseDeadline != nil && now.After(*transfer.SLA.ResponseDeadline) {
			updates["sla_breached"] = true
			updates["sla_breached_at"] = now
		}

		if err := p.app.DB.Model(&transfer).Updates(updates).Error; err != nil {
			p.app.Log.Error("Failed to escalate transfer", "error", err, "transfer_id", transfer.ID)
			continue
		}

		escalatedCount++
		p.app.Log.Warn("Transfer escalated",
			"transfer_id", transfer.ID,
			"contact_id", transfer.ContactID,
			"new_level", newLevel,
			"escalation_at", transfer.SLA.EscalationAt,
		)

		// Send notification to escalation contacts
		p.notifyEscalation(transfer, settings, newLevel)

		// Broadcast update
		p.broadcastTransferUpdate(transfer, websocket.TypeTransferEscalated)

		// Send warning message to customer if configured
		if newLevel == 1 && settings.SLA.WarningMessage != "" {
			p.sendSLATextToCustomer(transfer, "SLA warning message", settings.SLA.WarningMessage)
		}
	}

	if escalatedCount > 0 {
		p.app.Log.Info("Escalated transfers", "count", escalatedCount, "org_id", orgID)
	}
}

// markSLABreached marks transfers as SLA breached when past response deadline
func (p *SLAProcessor) markSLABreached(orgID uuid.UUID, now time.Time) {
	result := p.app.DB.Model(&models.AgentTransfer{}).Where(
		"organization_id = ? AND status = ? AND sla_breached = ? AND sla_response_deadline IS NOT NULL AND sla_response_deadline < ? AND agent_id IS NULL",
		orgID, models.TransferStatusActive, false, now,
	).Updates(map[string]any{
		"sla_breached":    true,
		"sla_breached_at": now,
	})

	if result.Error != nil {
		p.app.Log.Error("Failed to mark SLA breached", "error", result.Error, "org_id", orgID)
		return
	}

	if result.RowsAffected > 0 {
		p.app.Log.Warn("Marked transfers as SLA breached", "count", result.RowsAffected, "org_id", orgID)
	}
}

// notifyEscalation sends notifications to escalation contacts via WebSocket broadcast
func (p *SLAProcessor) notifyEscalation(transfer models.AgentTransfer, settings models.ChatbotSettings, level int) {
	if len(settings.SLA.EscalationNotifyIDs) == 0 {
		return
	}

	// Get contact info for the notification
	var contact models.Contact
	if err := p.app.DB.Where("id = ?", transfer.ContactID).First(&contact).Error; err != nil {
		p.app.Log.Error("Failed to load contact for escalation notification", "error", err)
		return
	}

	// Prepare notification payload
	levelName := "warning"
	if level >= 2 {
		levelName = "critical"
	}

	// Broadcast escalation notification to the organization
	// Escalation contacts will receive this via the org-wide broadcast
	contactName, phoneNumber := p.app.MaskContactFields(transfer.OrganizationID, contact.ProfileName, contact.PhoneNumber)

	payload := map[string]any{
		"id":                    transfer.ID.String(),
		"contact_id":            transfer.ContactID.String(),
		"contact_name":          contactName,
		"phone_number":          phoneNumber,
		"escalation_level":      level,
		"level_name":            levelName,
		"waiting_since":         transfer.TransferredAt.Format(time.RFC3339),
		"escalation_notify_ids": settings.SLA.EscalationNotifyIDs,
	}
	if transfer.TeamID != nil {
		payload["team_id"] = transfer.TeamID.String()
	}
	p.app.WSHub.BroadcastToOrg(transfer.OrganizationID, websocket.WSMessage{
		Type:    websocket.TypeTransferEscalation,
		Payload: payload,
	})

	p.app.Log.Info("Escalation notification sent",
		"transfer_id", transfer.ID,
		"level", level,
		"notify_count", len(settings.SLA.EscalationNotifyIDs),
	)
}

// sendSLATextToCustomer sends an SLA-related text message to the customer.
func (p *SLAProcessor) sendSLATextToCustomer(transfer models.AgentTransfer, label, message string) {
	account, err := p.app.resolveWhatsAppAccount(transfer.OrganizationID, transfer.WhatsAppAccount)
	if err != nil {
		p.app.Log.Error("Failed to load WhatsApp account for "+label, "error", err)
		return
	}

	var contact models.Contact
	if err := p.app.DB.Where("id = ?", transfer.ContactID).First(&contact).Error; err != nil {
		p.app.Log.Error("Failed to load contact for "+label, "error", err)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if _, err := p.app.SendOutgoingMessage(ctx, OutgoingMessageRequest{
		Account: account,
		Contact: &contact,
		Type:    models.MessageTypeText,
		Content: message,
	}, SLASendOptions()); err != nil {
		p.app.Log.Error("Failed to send "+label, "error", err, "phone", transfer.PhoneNumber)
		return
	}

	p.app.Log.Info(label+" sent to customer", "phone", transfer.PhoneNumber, "transfer_id", transfer.ID)
}

// broadcastTransferUpdate broadcasts transfer update via WebSocket
func (p *SLAProcessor) broadcastTransferUpdate(transfer models.AgentTransfer, wsType string) {
	// Get contact info
	var contact models.Contact
	p.app.DB.Where("id = ?", transfer.ContactID).First(&contact)

	contactName, phoneNumber := p.app.MaskContactFields(transfer.OrganizationID, contact.ProfileName, contact.PhoneNumber)

	p.app.WSHub.BroadcastToOrg(transfer.OrganizationID, websocket.WSMessage{
		Type: wsType,
		Payload: map[string]any{
			"id":               transfer.ID.String(),
			"contact_id":       transfer.ContactID.String(),
			"contact_name":     contactName,
			"phone_number":     phoneNumber,
			"status":           transfer.Status,
			"escalation_level": transfer.SLA.EscalationLevel,
			"sla_breached":     transfer.SLA.Breached,
		},
	})
}

// customerLastSpokeAt returns when the contact most recently sent an
// incoming message, falling back to the transfer's start when they
// haven't messaged since. Anchors the escalation window at the customer's
// latest activity so escalation only fires when there's an outstanding
// question.
func (p *SLAProcessor) customerLastSpokeAt(transfer models.AgentTransfer) time.Time {
	var t time.Time
	p.app.DB.Model(&models.Message{}).
		Where("contact_id = ? AND direction = ?", transfer.ContactID, models.DirectionIncoming).
		Select("MAX(created_at)").
		Scan(&t)
	if t.Before(transfer.TransferredAt) {
		return transfer.TransferredAt
	}
	return t
}

// agentRespondedSince checks if the assigned agent sent an outgoing message
// after the given timestamp. This is used to detect active agent conversations
// so that SLA deadlines can be extended instead of firing warnings/auto-close.
func (p *SLAProcessor) agentRespondedSince(transfer models.AgentTransfer, since time.Time) bool {
	if transfer.AgentID == nil {
		return false
	}

	var count int64
	p.app.DB.Model(&models.Message{}).
		Where("contact_id = ? AND sent_by_user_id = ? AND direction = ? AND created_at > ?",
			transfer.ContactID, *transfer.AgentID, models.DirectionOutgoing, since,
		).Count(&count)

	return count > 0
}

// SetSLADeadlines sets SLA deadlines on a new transfer based on settings
func (a *App) SetSLADeadlines(transfer *models.AgentTransfer, settings *models.ChatbotSettings) {
	if !settings.SLA.Enabled {
		return
	}

	now := time.Now()

	// Response deadline (time to pick up)
	if settings.SLA.ResponseMinutes > 0 {
		deadline := now.Add(time.Duration(settings.SLA.ResponseMinutes) * time.Minute)
		transfer.SLA.ResponseDeadline = &deadline
	}

	// Resolution deadline
	if settings.SLA.ResolutionMinutes > 0 {
		deadline := now.Add(time.Duration(settings.SLA.ResolutionMinutes) * time.Minute)
		transfer.SLA.ResolutionDeadline = &deadline
	}

	// Escalation deadline
	if settings.SLA.EscalationMinutes > 0 {
		deadline := now.Add(time.Duration(settings.SLA.EscalationMinutes) * time.Minute)
		transfer.SLA.EscalationAt = &deadline
	}

	// Expiry deadline (auto-close)
	if settings.SLA.AutoCloseHours > 0 {
		deadline := now.Add(time.Duration(settings.SLA.AutoCloseHours) * time.Hour)
		transfer.SLA.ExpiresAt = &deadline
	}

	a.Log.Debug("SLA deadlines set",
		"transfer_id", transfer.ID,
		"response_deadline", transfer.SLA.ResponseDeadline,
		"escalation_at", transfer.SLA.EscalationAt,
		"expires_at", transfer.SLA.ExpiresAt,
	)
}

// UpdateSLAOnPickup updates SLA tracking when a transfer is picked up
func (a *App) UpdateSLAOnPickup(transfer *models.AgentTransfer) {
	now := time.Now()
	transfer.SLA.PickedUpAt = &now

	// Check if SLA was breached (picked up after response deadline)
	if transfer.SLA.ResponseDeadline != nil && now.After(*transfer.SLA.ResponseDeadline) {
		transfer.SLA.Breached = true
		transfer.SLA.BreachedAt = &now
	}
}

// UpdateSLAOnFirstResponse updates SLA tracking when agent sends first response
func (a *App) UpdateSLAOnFirstResponse(transfer *models.AgentTransfer) {
	if transfer.SLA.FirstResponseAt != nil {
		return // Already responded
	}

	now := time.Now()
	transfer.SLA.FirstResponseAt = &now
}

// processClientInactivity handles client inactivity reminders and auto-close for chatbot conversations only
func (p *SLAProcessor) processClientInactivity(orgID uuid.UUID, settings models.ChatbotSettings, now time.Time) {
	// Find contacts where chatbot has sent a message and is waiting for client response
	var contacts []models.Contact
	if err := p.app.rootApp().WithTenantApp(orgID, func(scoped *App) error {
		return scoped.DB.Where(
			"organization_id = ? AND chatbot_last_message_at IS NOT NULL AND merged_into_id IS NULL",
			orgID,
		).Find(&contacts).Error
	}); err != nil {
		p.app.Log.Error("Failed to find contacts for client inactivity check", "error", err, "org_id", orgID)
		return
	}

	for _, contact := range contacts {
		// Calculate time since chatbot's last message
		timeSinceChatbotMsg := now.Sub(*contact.ChatbotLastMessageAt)

		// Check if we should auto-close (takes precedence over reminder)
		if settings.ClientInactivity.AutoCloseMinutes > 0 {
			autoCloseThreshold := time.Duration(settings.ClientInactivity.AutoCloseMinutes) * time.Minute
			if timeSinceChatbotMsg >= autoCloseThreshold {
				p.autoCloseChatbotSession(contact, settings)
				continue
			}
		}

		// Check if we should send reminder
		if settings.ClientInactivity.ReminderMinutes > 0 && !contact.ChatbotReminderSent {
			reminderThreshold := time.Duration(settings.ClientInactivity.ReminderMinutes) * time.Minute
			if timeSinceChatbotMsg >= reminderThreshold {
				p.sendChatbotReminder(contact, settings)
			}
		}
	}
}

var errChatbotInactivityGenerationChanged = errors.New("chatbot inactivity generation changed")

// withChatbotInactivityAttempt adds only an organization KEY SHARE lock across
// the callback. The unchanged synchronous SLA sender independently owns its
// existing account/contact/message locks; none are inherited from this guard.
// Policy writers acquire organization UPDATE before those locks, so a committed
// Pause/hold wins before admission and an admitted attempt makes the writer wait.
// This is a policy/generation fence, not an exactly-once timer claim.
func (p *SLAProcessor) withChatbotInactivityAttempt(
	contact models.Contact,
	attempt func(context.Context, *App, *models.Contact) error,
) (bool, error) {
	if p == nil || p.app == nil || p.app.DB == nil || p.app.rootApp().DB == nil || attempt == nil {
		return false, errors.New("chatbot inactivity attempt requires an app database and callback")
	}
	if contact.ID == uuid.Nil || contact.OrganizationID == uuid.Nil ||
		contact.ChatbotLastMessageAt == nil || contact.ChatbotLastMessageAt.IsZero() {
		return false, nil
	}
	// An arbitrary caller transaction can already own a conflicting policy
	// lock or the pool's other connection. Production timers run after their
	// selection transaction commits; reject accidental nested use as well.
	if _, transactional := p.app.DB.Statement.ConnPool.(gorm.TxCommitter); transactional {
		return false, errors.New("chatbot inactivity attempt cannot inherit a caller transaction")
	}
	root := p.app.rootApp()
	if err := database.RequireIndependentAIAttemptConnections(root.DB); err != nil {
		return false, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	completed := false
	err := database.WithTenantReadCommitted(root.DB.WithContext(ctx), contact.OrganizationID, func(guardTx *gorm.DB) error {
		if err := database.LockOrganizationAIAttemptScope(guardTx, contact.OrganizationID); err != nil {
			return err
		}
		if err := requireOrdinaryTenantOrganization(guardTx, contact.OrganizationID); err != nil {
			return err
		}
		current, err := contactutil.ResolveCanonicalContact(guardTx, contact.OrganizationID, contact.ID)
		if err != nil {
			return err
		}
		// Never redirect an old timer onto a merged survivor or a new waiting
		// period. Pause/Resume clears the timestamp; a later bot reply replaces it.
		if current.ID != contact.ID || current.WhatsAppAccount != contact.WhatsAppAccount ||
			current.ChatbotLastMessageAt == nil ||
			!current.ChatbotLastMessageAt.Equal(*contact.ChatbotLastMessageAt) ||
			current.ChatbotReminderSent != contact.ChatbotReminderSent {
			return nil
		}
		policy, err := database.EvaluateContactAutomaticReplyPolicy(guardTx, contact.OrganizationID, current.ID)
		if err != nil {
			return err
		}
		if !policy.Allowed {
			return nil
		}
		if err := attempt(ctx, root.scopedApp(guardTx, contact.OrganizationID), current); err != nil {
			return err
		}
		completed = true
		return nil
	})
	if errors.Is(err, errChatbotInactivityGenerationChanged) {
		return false, nil
	}
	return completed && err == nil, err
}

// updateChatbotInactivityGeneration never retires a later bot reply or a
// generation cleared/replaced while the independently committed sender ran.
// Contact locks are acquired here only after provider I/O has finished.
func updateChatbotInactivityGeneration(tx *gorm.DB, contact *models.Contact, updates map[string]any) error {
	if tx == nil || contact == nil || contact.ChatbotLastMessageAt == nil {
		return errChatbotInactivityGenerationChanged
	}
	result := tx.Model(&models.Contact{}).Where(
		"id = ? AND organization_id = ? AND merged_into_id IS NULL AND whats_app_account = ? AND chatbot_last_message_at = ? AND chatbot_reminder_sent = ?",
		contact.ID, contact.OrganizationID, contact.WhatsAppAccount,
		*contact.ChatbotLastMessageAt, contact.ChatbotReminderSent,
	).Updates(updates)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return errChatbotInactivityGenerationChanged
	}
	return nil
}

// sendChatbotReminder sends a reminder message to an inactive client during chatbot conversation
func (p *SLAProcessor) sendChatbotReminder(contact models.Contact, settings models.ChatbotSettings) {
	if settings.ClientInactivity.ReminderMessage == "" || contact.ChatbotReminderSent {
		return
	}
	completed, err := p.withChatbotInactivityAttempt(contact, func(ctx context.Context, scoped *App, current *models.Contact) error {
		account, err := scoped.resolveWhatsAppAccount(current.OrganizationID, current.WhatsAppAccount)
		if err != nil {
			return err
		}
		if _, err := scoped.rootApp().SendOutgoingMessage(ctx, OutgoingMessageRequest{
			Account: account,
			Contact: current,
			Type:    models.MessageTypeText,
			Content: settings.ClientInactivity.ReminderMessage,
		}, SLASendOptions()); err != nil {
			return err
		}
		return updateChatbotInactivityGeneration(scoped.DB, current, map[string]any{"chatbot_reminder_sent": true})
	})
	if err != nil {
		p.app.Log.Error("Failed to send chatbot reminder message", "error", err, "phone", contact.PhoneNumber)
		return
	}
	if !completed {
		return
	}

	p.app.Log.Info("Chatbot reminder sent",
		"contact_id", contact.ID,
		"phone", contact.PhoneNumber,
		"inactive_since", contact.ChatbotLastMessageAt,
	)
}

// autoCloseChatbotSession closes a chatbot session due to client inactivity
func (p *SLAProcessor) autoCloseChatbotSession(contact models.Contact, settings models.ChatbotSettings) {
	// Save the timestamp before clearing for logging
	var inactiveSince time.Time
	if contact.ChatbotLastMessageAt != nil {
		inactiveSince = *contact.ChatbotLastMessageAt
	}

	completed, err := p.withChatbotInactivityAttempt(contact, func(ctx context.Context, scoped *App, current *models.Contact) error {
		// A configured notice must reach the existing sender before this exact
		// generation can close. Policy or pre-provider errors remain fail-closed.
		if settings.ClientInactivity.AutoCloseMessage != "" {
			account, err := scoped.resolveWhatsAppAccount(current.OrganizationID, current.WhatsAppAccount)
			if err != nil {
				return err
			}
			if _, err := scoped.rootApp().SendOutgoingMessage(ctx, OutgoingMessageRequest{
				Account: account,
				Contact: current,
				Type:    models.MessageTypeText,
				Content: settings.ClientInactivity.AutoCloseMessage,
			}, SLASendOptions()); err != nil {
				return err
			}
		}
		return updateChatbotInactivityGeneration(scoped.DB, current, map[string]any{
			"chatbot_last_message_at": nil,
			"chatbot_reminder_sent":   false,
		})
	})
	if err != nil {
		p.app.Log.Error("Failed to close chatbot session for client inactivity", "error", err, "contact_id", contact.ID)
		return
	}
	if !completed {
		return
	}

	p.app.Log.Info("Chatbot session closed due to client inactivity",
		"contact_id", contact.ID,
		"phone", contact.PhoneNumber,
		"inactive_since", inactiveSince,
	)
}

// UpdateContactChatbotMessage updates the chatbot last message timestamp for a contact
func (a *App) UpdateContactChatbotMessage(contactID uuid.UUID) {
	now := time.Now()
	update := func(tx *gorm.DB) error {
		return tx.Exec(
			"UPDATE contacts SET chatbot_last_message_at = ?, chatbot_reminder_sent = false WHERE id = ?",
			now,
			contactID,
		).Error
	}
	if a.inboundContinuation != nil {
		orgID := a.inboundContinuationOrganizationID()
		if err := a.rootApp().WithCommittedTenantApp(
			orgID,
			func(scoped *App) error { return update(scoped.DB) },
		); err != nil {
			a.Log.Error(
				"Failed to update independently committed chatbot tracking",
				"error", err,
				"contact_id", contactID,
			)
		}
		return
	}
	_ = update(a.DB)
}

// ClearContactChatbotTracking clears chatbot tracking when client replies or is transferred
func (a *App) ClearContactChatbotTracking(contactID uuid.UUID) {
	update := func(tx *gorm.DB) error {
		return tx.Model(&models.Contact{}).
			Where("id = ?", contactID).
			Updates(map[string]any{
				"chatbot_last_message_at": nil,
				"chatbot_reminder_sent":   false,
			}).Error
	}
	if a.inboundContinuation != nil {
		orgID := a.inboundContinuationOrganizationID()
		if err := a.rootApp().WithCommittedTenantApp(
			orgID,
			func(scoped *App) error { return update(scoped.DB) },
		); err != nil {
			a.Log.Error(
				"Failed to clear independently committed chatbot tracking",
				"error", err,
				"contact_id", contactID,
			)
		}
		return
	}
	_ = update(a.DB)
}

const chatbotTrackingClearedThroughMetadataKey = "chatbot_tracking_cleared_through"

func chatbotTrackingClearWatermark(metadata models.JSONB) (time.Time, error) {
	if metadata == nil {
		return time.Time{}, nil
	}
	raw, exists := metadata[chatbotTrackingClearedThroughMetadataKey]
	if !exists {
		return time.Time{}, nil
	}
	value, ok := raw.(string)
	if !ok || value == "" {
		return time.Time{}, errors.New("chatbot tracking clear watermark is invalid")
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil || parsed.IsZero() {
		return time.Time{}, errors.New("chatbot tracking clear watermark is invalid")
	}
	return parsed.UTC(), nil
}

// clearContactChatbotTrackingForInbound clears only chatbot state that is no
// newer than the durable inbound message being processed. This keeps an older
// continuation replay from erasing a later bot reply committed by another
// worker.
func (a *App) clearContactChatbotTrackingForInbound(contactID uuid.UUID) error {
	if a == nil || a.inboundContinuation == nil {
		a.ClearContactChatbotTracking(contactID)
		return nil
	}
	execution := a.inboundContinuation
	organizationID := a.inboundContinuationOrganizationID()
	update := func(tx *gorm.DB) error {
		var inbound models.Message
		if err := tx.Select("id", "created_at", "ingested_at").
			Where(
				"id = ? AND organization_id = ? AND contact_id = ? AND direction = ?",
				execution.MessageID,
				organizationID,
				contactID,
				models.DirectionIncoming,
			).
			First(&inbound).Error; err != nil {
			return err
		}
		inboundAt := inbound.EffectiveIngestedAt()
		if inboundAt.IsZero() {
			return errors.New("durable inbound message has no ordering timestamp")
		}
		var contact models.Contact
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Select("id", "metadata", "chatbot_last_message_at").
			Where("id = ? AND organization_id = ?", contactID, organizationID).
			First(&contact).Error; err != nil {
			return err
		}
		clearedThrough, err := chatbotTrackingClearWatermark(contact.Metadata)
		if err != nil {
			return err
		}
		if !clearedThrough.IsZero() && !inboundAt.After(clearedThrough) {
			// A later (or identical replayed) inbound clear remains authoritative.
			return nil
		}
		metadata := cloneMessageMetadata(contact.Metadata)
		metadata[chatbotTrackingClearedThroughMetadataKey] =
			inboundAt.UTC().Format(time.RFC3339Nano)
		updates := map[string]any{"metadata": metadata}
		if contact.ChatbotLastMessageAt == nil ||
			!contact.ChatbotLastMessageAt.After(inboundAt) {
			updates["chatbot_last_message_at"] = nil
			updates["chatbot_reminder_sent"] = false
		}
		result := tx.Model(&models.Contact{}).
			Where("id = ? AND organization_id = ?", contactID, organizationID).
			Updates(updates)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return errors.New("inbound chatbot tracking contact changed before clear")
		}
		return nil
	}
	if err := a.rootApp().WithCommittedTenantApp(organizationID, func(scoped *App) error {
		return update(scoped.DB)
	}); err != nil {
		return err
	}
	return nil
}
