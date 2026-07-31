package handlers

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/audit"
	appcrypto "github.com/shridarpatil/whatomate/internal/crypto"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/valyala/fasthttp"
	"github.com/zerodha/fastglue"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// ChatbotSettingsResponse represents the response for chatbot settings
type ChatbotSettingsResponse struct {
	Enabled                      bool             `json:"enabled"`
	GreetingMessage              string           `json:"greeting_message"`
	GreetingButtons              []map[string]any `json:"greeting_buttons"`
	FallbackMessage              string           `json:"fallback_message"`
	FallbackButtons              []map[string]any `json:"fallback_buttons"`
	SessionTimeoutMinutes        int              `json:"session_timeout_minutes"`
	BusinessHoursEnabled         bool             `json:"business_hours_enabled"`
	BusinessHours                []map[string]any `json:"business_hours"`
	OutOfHoursMessage            string           `json:"out_of_hours_message"`
	AllowAutomatedOutsideHours   bool             `json:"allow_automated_outside_hours"`
	AllowAgentQueuePickup        bool             `json:"allow_agent_queue_pickup"`
	AssignToSameAgent            bool             `json:"assign_to_same_agent"`
	AgentCurrentConversationOnly bool             `json:"agent_current_conversation_only"`
	AIEnabled                    bool             `json:"ai_enabled"`
	AIMaxTokens                  int              `json:"ai_max_tokens"`
	AISystemPrompt               string           `json:"ai_system_prompt"`
	// SLA Settings
	SLAEnabled             bool     `json:"sla_enabled"`
	SLAResponseMinutes     int      `json:"sla_response_minutes"`
	SLAResolutionMinutes   int      `json:"sla_resolution_minutes"`
	SLAEscalationMinutes   int      `json:"sla_escalation_minutes"`
	SLAAutoCloseHours      int      `json:"sla_auto_close_hours"`
	SLAAutoCloseMessage    string   `json:"sla_auto_close_message"`
	SLAWarningMessage      string   `json:"sla_warning_message"`
	SLAEscalationNotifyIDs []string `json:"sla_escalation_notify_ids"`
	// Client Inactivity Settings (Chatbot Only)
	ClientReminderEnabled  bool   `json:"client_reminder_enabled"`
	ClientReminderMinutes  int    `json:"client_reminder_minutes"`
	ClientReminderMessage  string `json:"client_reminder_message"`
	ClientAutoCloseMinutes int    `json:"client_auto_close_minutes"`
	ClientAutoCloseMessage string `json:"client_auto_close_message"`
}

// ChatbotStatsResponse represents chatbot statistics
type ChatbotStatsResponse struct {
	TotalSessions   int64 `json:"total_sessions"`
	ActiveSessions  int64 `json:"active_sessions"`
	MessagesHandled int64 `json:"messages_handled"`
	AIResponses     int64 `json:"ai_responses"`
	AgentTransfers  int64 `json:"agent_transfers"`
	KeywordsCount   int64 `json:"keywords_count"`
	FlowsCount      int64 `json:"flows_count"`
	AIContextsCount int64 `json:"ai_contexts_count"`
}

// KeywordRuleResponse represents a keyword rule for API response
type KeywordRuleResponse struct {
	ID              string              `json:"id"`
	Name            string              `json:"name"`
	Keywords        []string            `json:"keywords"`
	MatchType       models.MatchType    `json:"match_type"`
	ResponseType    models.ResponseType `json:"response_type"`
	ResponseContent json.RawMessage     `json:"response_content"`
	Priority        int                 `json:"priority"`
	Enabled         bool                `json:"enabled"`
	CreatedByName   string              `json:"created_by_name,omitempty"`
	UpdatedByName   string              `json:"updated_by_name,omitempty"`
	CreatedAt       string              `json:"created_at"`
	UpdatedAt       string              `json:"updated_at"`
}

// ChatbotFlowResponse represents a chatbot flow for API response
type ChatbotFlowResponse struct {
	ID              string   `json:"id"`
	Name            string   `json:"name"`
	Description     string   `json:"description"`
	TriggerKeywords []string `json:"trigger_keywords"`
	StepsCount      int      `json:"steps_count"`
	Enabled         bool     `json:"enabled"`
	CreatedAt       string   `json:"created_at"`
}

// AIContextResponse represents an AI context for API response
type AIContextResponse struct {
	ID              string             `json:"id"`
	Name            string             `json:"name"`
	ContextType     models.ContextType `json:"context_type"`
	TriggerKeywords []string           `json:"trigger_keywords"`
	StaticContent   string             `json:"static_content"`
	ApiConfig       models.JSONB       `json:"api_config,omitempty"`
	Enabled         bool               `json:"enabled"`
	Priority        int                `json:"priority"`
	CreatedByName   string             `json:"created_by_name,omitempty"`
	UpdatedByName   string             `json:"updated_by_name,omitempty"`
	CreatedAt       string             `json:"created_at"`
	UpdatedAt       string             `json:"updated_at"`
}

// GetChatbotSettings returns chatbot settings and stats
func (a *App) GetChatbotSettings(r *fastglue.Request) error {
	orgID, _, err := a.requireAuth(
		r,
		models.ResourceSettingsChatbot,
		models.ActionRead,
	)
	if err != nil {
		return nil
	}

	// Get or create default settings
	var settings models.ChatbotSettings
	result := a.DB.Where("organization_id = ? AND whats_app_account = ?", orgID, "").First(&settings)
	if result.Error != nil {
		// Return default settings if none exist
		settings = models.ChatbotSettings{
			IsEnabled:          false,
			DefaultResponse:    "Hello! How can I help you today?",
			SessionTimeoutMins: 30,
			AI:                 models.AIConfig{Enabled: false},
		}
	}

	// Gather stats
	stats := a.getChatbotStats(orgID)

	// Convert button arrays
	greetingButtons := make([]map[string]any, 0)
	if settings.GreetingButtons != nil {
		for _, btn := range settings.GreetingButtons {
			if btnMap, ok := btn.(map[string]any); ok {
				greetingButtons = append(greetingButtons, btnMap)
			}
		}
	}

	fallbackButtons := make([]map[string]any, 0)
	if settings.FallbackButtons != nil {
		for _, btn := range settings.FallbackButtons {
			if btnMap, ok := btn.(map[string]any); ok {
				fallbackButtons = append(fallbackButtons, btnMap)
			}
		}
	}

	// Convert business hours array
	businessHours := make([]map[string]any, 0)
	if settings.BusinessHours.Hours != nil {
		for _, bh := range settings.BusinessHours.Hours {
			if bhMap, ok := bh.(map[string]any); ok {
				businessHours = append(businessHours, bhMap)
			}
		}
	}

	settingsResp := ChatbotSettingsResponse{
		Enabled:               settings.IsEnabled,
		GreetingMessage:       settings.DefaultResponse,
		GreetingButtons:       greetingButtons,
		FallbackMessage:       settings.FallbackMessage,
		FallbackButtons:       fallbackButtons,
		SessionTimeoutMinutes: settings.SessionTimeoutMins,
		// Business Hours
		BusinessHoursEnabled:       settings.BusinessHours.Enabled,
		BusinessHours:              businessHours,
		OutOfHoursMessage:          settings.BusinessHours.OutOfHoursMessage,
		AllowAutomatedOutsideHours: settings.BusinessHours.AllowAutomatedOutside,
		// Agent Assignment
		AllowAgentQueuePickup:        settings.AgentAssignment.AllowQueuePickup,
		AssignToSameAgent:            settings.AgentAssignment.AssignToSameAgent,
		AgentCurrentConversationOnly: settings.AgentAssignment.CurrentConversationOnly,
		// AI
		AIEnabled:      settings.AI.Enabled,
		AIMaxTokens:    settings.AI.MaxTokens,
		AISystemPrompt: settings.AI.SystemPrompt,
		// SLA Settings
		SLAEnabled:             settings.SLA.Enabled,
		SLAResponseMinutes:     settings.SLA.ResponseMinutes,
		SLAResolutionMinutes:   settings.SLA.ResolutionMinutes,
		SLAEscalationMinutes:   settings.SLA.EscalationMinutes,
		SLAAutoCloseHours:      settings.SLA.AutoCloseHours,
		SLAAutoCloseMessage:    settings.SLA.AutoCloseMessage,
		SLAWarningMessage:      settings.SLA.WarningMessage,
		SLAEscalationNotifyIDs: settings.SLA.EscalationNotifyIDs,
		// Client Inactivity Settings
		ClientReminderEnabled:  settings.ClientInactivity.ReminderEnabled,
		ClientReminderMinutes:  settings.ClientInactivity.ReminderMinutes,
		ClientReminderMessage:  settings.ClientInactivity.ReminderMessage,
		ClientAutoCloseMinutes: settings.ClientInactivity.AutoCloseMinutes,
		ClientAutoCloseMessage: settings.ClientInactivity.AutoCloseMessage,
	}
	return r.SendEnvelope(map[string]any{
		"settings": settingsResp,
		"stats":    stats,
	})
}

// UpdateChatbotSettings updates chatbot settings
// chatbotMessagesSnapshot captures the fields shown on the Chatbot "Messages" tab.
func chatbotMessagesSnapshot(s *models.ChatbotSettings) map[string]any {
	return map[string]any{
		"enabled":                 s.IsEnabled,
		"greeting_message":        s.DefaultResponse,
		"greeting_buttons":        s.GreetingButtons,
		"fallback_message":        s.FallbackMessage,
		"fallback_buttons":        s.FallbackButtons,
		"session_timeout_minutes": s.SessionTimeoutMins,
	}
}

// chatbotAgentsSnapshot captures the fields shown on the Chatbot "Agents" tab.
func chatbotAgentsSnapshot(s *models.ChatbotSettings) map[string]any {
	return map[string]any{
		"allow_agent_queue_pickup":        s.AgentAssignment.AllowQueuePickup,
		"assign_to_same_agent":            s.AgentAssignment.AssignToSameAgent,
		"agent_current_conversation_only": s.AgentAssignment.CurrentConversationOnly,
	}
}

// chatbotHoursSnapshot captures the fields shown on the Chatbot "Business Hours" tab.
func chatbotHoursSnapshot(s *models.ChatbotSettings) map[string]any {
	return map[string]any{
		"business_hours_enabled":        s.BusinessHours.Enabled,
		"business_hours":                s.BusinessHours.Hours,
		"out_of_hours_message":          s.BusinessHours.OutOfHoursMessage,
		"allow_automated_outside_hours": s.BusinessHours.AllowAutomatedOutside,
	}
}

// chatbotSLASnapshot captures the fields shown on the Chatbot "SLA" tab
// (SLA + Client Inactivity live on the same tab in the UI).
func chatbotSLASnapshot(s *models.ChatbotSettings) map[string]any {
	return map[string]any{
		"sla_enabled":               s.SLA.Enabled,
		"sla_response_minutes":      s.SLA.ResponseMinutes,
		"sla_resolution_minutes":    s.SLA.ResolutionMinutes,
		"sla_escalation_minutes":    s.SLA.EscalationMinutes,
		"sla_auto_close_hours":      s.SLA.AutoCloseHours,
		"sla_auto_close_message":    s.SLA.AutoCloseMessage,
		"sla_warning_message":       s.SLA.WarningMessage,
		"sla_escalation_notify_ids": s.SLA.EscalationNotifyIDs,
		"client_reminder_enabled":   s.ClientInactivity.ReminderEnabled,
		"client_reminder_minutes":   s.ClientInactivity.ReminderMinutes,
		"client_reminder_message":   s.ClientInactivity.ReminderMessage,
		"client_auto_close_minutes": s.ClientInactivity.AutoCloseMinutes,
		"client_auto_close_message": s.ClientInactivity.AutoCloseMessage,
	}
}

// chatbotAISnapshot captures the fields shown on the Chatbot "AI" tab.
// The API key is intentionally excluded — it's a secret, not a user-facing
// change the activity log should surface.
func chatbotAISnapshot(s *models.ChatbotSettings) map[string]any {
	return map[string]any{
		"enabled":          s.IsEnabled,
		"ai_enabled":       s.AI.Enabled,
		"ai_max_tokens":    s.AI.MaxTokens,
		"ai_system_prompt": s.AI.SystemPrompt,
	}
}

type updateChatbotSettingsRequest struct {
	Enabled                      *bool              `json:"enabled"`
	GreetingMessage              *string            `json:"greeting_message"`
	GreetingButtons              *[]map[string]any  `json:"greeting_buttons"`
	FallbackMessage              *string            `json:"fallback_message"`
	FallbackButtons              *[]map[string]any  `json:"fallback_buttons"`
	SessionTimeoutMinutes        *int               `json:"session_timeout_minutes"`
	BusinessHoursEnabled         *bool              `json:"business_hours_enabled"`
	BusinessHours                *[]map[string]any  `json:"business_hours"`
	OutOfHoursMessage            *string            `json:"out_of_hours_message"`
	AllowAutomatedOutsideHours   *bool              `json:"allow_automated_outside_hours"`
	AllowAgentQueuePickup        *bool              `json:"allow_agent_queue_pickup"`
	AssignToSameAgent            *bool              `json:"assign_to_same_agent"`
	AgentCurrentConversationOnly *bool              `json:"agent_current_conversation_only"`
	AIEnabled                    *bool              `json:"ai_enabled"`
	AIProvider                   *models.AIProvider `json:"ai_provider"`
	AIAPIKey                     *string            `json:"ai_api_key"`
	AIModel                      *string            `json:"ai_model"`
	AIMaxTokens                  *int               `json:"ai_max_tokens"`
	AISystemPrompt               *string            `json:"ai_system_prompt"`
	// SLA Settings
	SLAEnabled             *bool     `json:"sla_enabled"`
	SLAResponseMinutes     *int      `json:"sla_response_minutes"`
	SLAResolutionMinutes   *int      `json:"sla_resolution_minutes"`
	SLAEscalationMinutes   *int      `json:"sla_escalation_minutes"`
	SLAAutoCloseHours      *int      `json:"sla_auto_close_hours"`
	SLAAutoCloseMessage    *string   `json:"sla_auto_close_message"`
	SLAWarningMessage      *string   `json:"sla_warning_message"`
	SLAEscalationNotifyIDs *[]string `json:"sla_escalation_notify_ids"`
	// Client Inactivity Settings
	ClientReminderEnabled  *bool   `json:"client_reminder_enabled"`
	ClientReminderMinutes  *int    `json:"client_reminder_minutes"`
	ClientReminderMessage  *string `json:"client_reminder_message"`
	ClientAutoCloseMinutes *int    `json:"client_auto_close_minutes"`
	ClientAutoCloseMessage *string `json:"client_auto_close_message"`
}

func applyChatbotSettingsRequest(
	settings *models.ChatbotSettings,
	req *updateChatbotSettingsRequest,
) {
	if req.Enabled != nil {
		settings.IsEnabled = *req.Enabled
	}
	if req.GreetingMessage != nil {
		settings.DefaultResponse = *req.GreetingMessage
	}
	if req.GreetingButtons != nil {
		buttons := make([]any, len(*req.GreetingButtons))
		for i, btn := range *req.GreetingButtons {
			buttons[i] = btn
		}
		settings.GreetingButtons = buttons
	}
	if req.FallbackMessage != nil {
		settings.FallbackMessage = *req.FallbackMessage
	}
	if req.FallbackButtons != nil {
		buttons := make([]any, len(*req.FallbackButtons))
		for i, btn := range *req.FallbackButtons {
			buttons[i] = btn
		}
		settings.FallbackButtons = buttons
	}
	if req.SessionTimeoutMinutes != nil {
		settings.SessionTimeoutMins = *req.SessionTimeoutMinutes
	}

	if req.BusinessHoursEnabled != nil {
		settings.BusinessHours.Enabled = *req.BusinessHoursEnabled
	}
	if req.BusinessHours != nil {
		hours := make([]any, len(*req.BusinessHours))
		for i, bh := range *req.BusinessHours {
			hours[i] = bh
		}
		settings.BusinessHours.Hours = hours
	}
	if req.OutOfHoursMessage != nil {
		settings.BusinessHours.OutOfHoursMessage = *req.OutOfHoursMessage
	}
	if req.AllowAutomatedOutsideHours != nil {
		settings.BusinessHours.AllowAutomatedOutside = *req.AllowAutomatedOutsideHours
	}

	if req.AllowAgentQueuePickup != nil {
		settings.AgentAssignment.AllowQueuePickup = *req.AllowAgentQueuePickup
	}
	if req.AssignToSameAgent != nil {
		settings.AgentAssignment.AssignToSameAgent = *req.AssignToSameAgent
	}
	if req.AgentCurrentConversationOnly != nil {
		settings.AgentAssignment.CurrentConversationOnly = *req.AgentCurrentConversationOnly
	}

	if req.AIEnabled != nil {
		settings.AI.Enabled = *req.AIEnabled
	}
	if req.AIProvider != nil {
		settings.AI.Provider = *req.AIProvider
	}
	if req.AIAPIKey != nil && *req.AIAPIKey != "" {
		settings.AI.APIKey = *req.AIAPIKey
	}
	if req.AIModel != nil {
		settings.AI.Model = *req.AIModel
	}
	if req.AIMaxTokens != nil {
		settings.AI.MaxTokens = *req.AIMaxTokens
	}
	if req.AISystemPrompt != nil {
		settings.AI.SystemPrompt = *req.AISystemPrompt
	}

	if req.SLAEnabled != nil {
		settings.SLA.Enabled = *req.SLAEnabled
	}
	if req.SLAResponseMinutes != nil {
		settings.SLA.ResponseMinutes = *req.SLAResponseMinutes
	}
	if req.SLAResolutionMinutes != nil {
		settings.SLA.ResolutionMinutes = *req.SLAResolutionMinutes
	}
	if req.SLAEscalationMinutes != nil {
		settings.SLA.EscalationMinutes = *req.SLAEscalationMinutes
	}
	if req.SLAAutoCloseHours != nil {
		settings.SLA.AutoCloseHours = *req.SLAAutoCloseHours
	}
	if req.SLAAutoCloseMessage != nil {
		settings.SLA.AutoCloseMessage = *req.SLAAutoCloseMessage
	}
	if req.SLAWarningMessage != nil {
		settings.SLA.WarningMessage = *req.SLAWarningMessage
	}
	if req.SLAEscalationNotifyIDs != nil {
		settings.SLA.EscalationNotifyIDs = *req.SLAEscalationNotifyIDs
	}

	if req.ClientReminderEnabled != nil {
		settings.ClientInactivity.ReminderEnabled = *req.ClientReminderEnabled
	}
	if req.ClientReminderMinutes != nil {
		settings.ClientInactivity.ReminderMinutes = *req.ClientReminderMinutes
	}
	if req.ClientReminderMessage != nil {
		settings.ClientInactivity.ReminderMessage = *req.ClientReminderMessage
	}
	if req.ClientAutoCloseMinutes != nil {
		settings.ClientInactivity.AutoCloseMinutes = *req.ClientAutoCloseMinutes
	}
	if req.ClientAutoCloseMessage != nil {
		settings.ClientInactivity.AutoCloseMessage = *req.ClientAutoCloseMessage
	}
}

func (a *App) UpdateChatbotSettings(r *fastglue.Request) error {
	orgID, userID, err := a.getOrgAndUserID(r)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusUnauthorized, "Unauthorized", nil, "")
	}

	var req updateChatbotSettingsRequest

	if err := json.Unmarshal(r.RequestCtx.PostBody(), &req); err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid request body", nil, "")
	}
	// The top-level switch gates every automated response, including AI. Treat
	// it as AI-sensitive so a general settings writer cannot reactivate Qwen.
	aiTouched := req.Enabled != nil || req.AIEnabled != nil ||
		req.AIProvider != nil || req.AIAPIKey != nil ||
		req.AIModel != nil || req.AIMaxTokens != nil || req.AISystemPrompt != nil
	providerConfigurationTouched := req.AIProvider != nil ||
		req.AIAPIKey != nil ||
		req.AIModel != nil
	aiAPIKeyChanged := req.AIAPIKey != nil && *req.AIAPIKey != ""
	if !a.HasPermission(
		userID,
		models.ResourceSettingsChatbot,
		models.ActionWrite,
		orgID,
	) {
		return r.SendErrorEnvelope(
			fasthttp.StatusForbidden,
			"Insufficient permissions",
			nil,
			"",
		)
	}
	if aiTouched &&
		!a.HasPermission(userID, models.ResourceChatbotAI, models.ActionWrite, orgID) {
		return r.SendErrorEnvelope(
			fasthttp.StatusForbidden,
			"Insufficient permissions",
			nil,
			"",
		)
	}
	if providerConfigurationTouched && !a.IsSuperAdmin(userID) {
		return r.SendErrorEnvelope(
			fasthttp.StatusForbidden,
			"Only a platform owner can manage the AI service configuration",
			nil,
			"",
		)
	}

	// Track which tabs the request touched so we only write audit entries
	// for tabs the user actually submitted.
	messagesTouched := req.Enabled != nil || req.GreetingMessage != nil ||
		req.GreetingButtons != nil || req.FallbackMessage != nil ||
		req.FallbackButtons != nil || req.SessionTimeoutMinutes != nil
	agentsTouched := req.AllowAgentQueuePickup != nil || req.AssignToSameAgent != nil ||
		req.AgentCurrentConversationOnly != nil
	hoursTouched := req.BusinessHoursEnabled != nil || req.BusinessHours != nil ||
		req.OutOfHoursMessage != nil || req.AllowAutomatedOutsideHours != nil
	slaTouched := req.SLAEnabled != nil || req.SLAResponseMinutes != nil ||
		req.SLAResolutionMinutes != nil || req.SLAEscalationMinutes != nil ||
		req.SLAAutoCloseHours != nil || req.SLAAutoCloseMessage != nil ||
		req.SLAWarningMessage != nil || req.SLAEscalationNotifyIDs != nil ||
		req.ClientReminderEnabled != nil || req.ClientReminderMinutes != nil ||
		req.ClientReminderMessage != nil || req.ClientAutoCloseMinutes != nil ||
		req.ClientAutoCloseMessage != nil

	encryptionKey := ""
	if a.Config != nil {
		encryptionKey = a.Config.App.EncryptionKey
	}

	var settings models.ChatbotSettings
	var oldMessages, oldAgents, oldHours, oldSLA, oldAI map[string]any
	userName := audit.GetUserName(a.DB, userID)
	if err := a.DB.Transaction(func(tx *gorm.DB) error {
		// The organization row is the per-tenant mutex for the singleton default
		// settings record. It also serializes the first-create case, where there
		// is no ChatbotSettings row to lock yet.
		if err := lockChannelAIOrganizationScopeTx(tx, orgID); err != nil {
			return err
		}

		// Scheduled work must be cancelled before locking ChatbotSettings. A
		// finalizer already owns its ScheduledJob row while it creates the
		// Message/Outbox rows, and dispatch owns the Contact policy row before
		// reading ChatbotSettings. Taking the settings lock first would allow a
		// settings -> scheduled job -> contact -> settings deadlock.
		if aiTouched {
			if err := cancelChannelAIScheduledJobsForOrganizationTx(
				tx,
				orgID,
				"chatbot_ai_settings_changed",
			); err != nil {
				return err
			}
		}

		isNew := false
		result := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("organization_id = ? AND whats_app_account = ?", orgID, "").
			First(&settings)
		switch {
		case errors.Is(result.Error, gorm.ErrRecordNotFound):
			isNew = true
			settings = models.ChatbotSettings{
				BaseModel:      models.BaseModel{ID: uuid.New()},
				OrganizationID: orgID,
			}
		case result.Error != nil:
			return result.Error
		}

		// Snapshot and mutate only after reloading the latest committed row while
		// holding its lock. This prevents a concurrent non-AI edit from saving a
		// stale copy over an authorized Qwen configuration change.
		oldMessages = chatbotMessagesSnapshot(&settings)
		oldAgents = chatbotAgentsSnapshot(&settings)
		oldHours = chatbotHoursSnapshot(&settings)
		oldSLA = chatbotSLASnapshot(&settings)
		oldAI = chatbotAISnapshot(&settings)
		applyChatbotSettingsRequest(&settings, &req)

		if err := appcrypto.EncryptFields(encryptionKey, &settings.AI.APIKey); err != nil {
			return err
		}
		if err := tx.Save(&settings).Error; err != nil {
			return err
		}

		// GORM skips false (zero-value) bool fields on INSERT when the column has
		// a database default of true, so the DB default wins. After creating the
		// row we explicitly set any default:true bool columns that were requested
		// as false.
		if isNew {
			zeroOverrides := map[string]any{}
			if req.AllowAutomatedOutsideHours != nil && !*req.AllowAutomatedOutsideHours {
				zeroOverrides["allow_automated_outside_hours"] = false
			}
			if req.AllowAgentQueuePickup != nil && !*req.AllowAgentQueuePickup {
				zeroOverrides["allow_agent_queue_pickup"] = false
			}
			if req.AssignToSameAgent != nil && !*req.AssignToSameAgent {
				zeroOverrides["assign_to_same_agent"] = false
			}
			if len(zeroOverrides) > 0 {
				if err := tx.Model(&settings).Updates(zeroOverrides).Error; err != nil {
					return err
				}
			}
		}

		// A finalizer that was already processing before scheduled-job
		// cancellation may have created an outbox row from the old settings.
		// Cancel outbox work only after the new settings are saved, while the
		// same organization mutex is still held.
		if aiTouched {
			if err := cancelChannelAIOutboxJobsForOrganizationTx(
				tx,
				orgID,
				"chatbot_ai_settings_changed",
			); err != nil {
				return err
			}
			var sensitiveChanges []map[string]any
			if aiAPIKeyChanged {
				sensitiveChanges = append(sensitiveChanges, map[string]any{
					"field":     "ai_api_key",
					"old_value": "********",
					"new_value": "********",
				})
			}
			// AI configuration and its compliance record commit atomically.
			// In particular, an API-key rotation must fail closed if its masked
			// audit marker cannot be persisted.
			if err := audit.LogAudit(
				tx,
				orgID,
				userID,
				userName,
				models.ResourceSettingsChatbotAI,
				orgID,
				models.AuditActionUpdated,
				oldAI,
				chatbotAISnapshot(&settings),
				sensitiveChanges...,
			); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		a.Log.Error("Failed to save settings", "error", err)
		return r.SendErrorEnvelope(
			fasthttp.StatusInternalServerError,
			"Failed to save settings",
			nil,
			"",
		)
	}

	// Invalidate caches
	a.InvalidateChatbotSettingsCache(orgID)
	a.InvalidateSLASettingsCache() // SLA settings are part of chatbot settings

	// Emit non-AI per-tab audit entries. AI changes are fail-closed and were
	// recorded atomically with the settings transaction above.
	if messagesTouched {
		if err := audit.LogAudit(a.DB, orgID, userID, userName,
			models.ResourceSettingsChatbotMessages, orgID, models.AuditActionUpdated,
			oldMessages, chatbotMessagesSnapshot(&settings)); err != nil {
			a.Log.Warn("Failed to record chatbot messages audit entry", "error", err)
		}
	}
	if agentsTouched {
		if err := audit.LogAudit(a.DB, orgID, userID, userName,
			models.ResourceSettingsChatbotAgents, orgID, models.AuditActionUpdated,
			oldAgents, chatbotAgentsSnapshot(&settings)); err != nil {
			a.Log.Warn("Failed to record chatbot agents audit entry", "error", err)
		}
	}
	if hoursTouched {
		if err := audit.LogAudit(a.DB, orgID, userID, userName,
			models.ResourceSettingsChatbotHours, orgID, models.AuditActionUpdated,
			oldHours, chatbotHoursSnapshot(&settings)); err != nil {
			a.Log.Warn("Failed to record chatbot hours audit entry", "error", err)
		}
	}
	if slaTouched {
		if err := audit.LogAudit(a.DB, orgID, userID, userName,
			models.ResourceSettingsChatbotSLA, orgID, models.AuditActionUpdated,
			oldSLA, chatbotSLASnapshot(&settings)); err != nil {
			a.Log.Warn("Failed to record chatbot SLA audit entry", "error", err)
		}
	}
	return r.SendEnvelope(map[string]any{
		"message": "Settings updated successfully",
	})
}

// ListKeywordRules lists all keyword rules for the organization
func (a *App) ListKeywordRules(r *fastglue.Request) error {
	orgID, err := a.getOrgID(r)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusUnauthorized, "Unauthorized", nil, "")
	}

	pg := parsePagination(r)
	search := string(r.RequestCtx.QueryArgs().Peek("search"))

	query := a.DB.Model(&models.KeywordRule{}).Where("organization_id = ?", orgID)

	// Apply search filter - search by name or keywords
	if search != "" {
		searchPattern := "%" + search + "%"
		// Search in name (case-insensitive) or in keywords JSONB array
		query = query.Where("name ILIKE ? OR keywords::text ILIKE ?", searchPattern, searchPattern)
	}

	var total int64
	query.Count(&total)

	var rules []models.KeywordRule
	if err := pg.Apply(query.Preload("CreatedBy").Preload("UpdatedBy").Order("priority DESC, created_at DESC")).
		Find(&rules).Error; err != nil {
		a.Log.Error("Failed to fetch keyword rules", "error", err)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to fetch keyword rules", nil, "")
	}

	response := make([]KeywordRuleResponse, len(rules))
	for i, rule := range rules {
		responseContent, _ := json.Marshal(rule.ResponseContent)
		resp := KeywordRuleResponse{
			ID:              rule.ID.String(),
			Name:            rule.Name,
			Keywords:        rule.Keywords,
			MatchType:       rule.MatchType,
			ResponseType:    rule.ResponseType,
			ResponseContent: responseContent,
			Priority:        rule.Priority,
			Enabled:         rule.IsEnabled,
			CreatedAt:       rule.CreatedAt.Format(time.RFC3339),
			UpdatedAt:       rule.UpdatedAt.Format(time.RFC3339),
		}
		if rule.CreatedBy != nil {
			resp.CreatedByName = rule.CreatedBy.FullName
		}
		if rule.UpdatedBy != nil {
			resp.UpdatedByName = rule.UpdatedBy.FullName
		}
		response[i] = resp
	}

	return r.SendEnvelope(listEnvelope("rules", response, total, pg))
}

// CreateKeywordRule creates a new keyword rule
func (a *App) CreateKeywordRule(r *fastglue.Request) error {
	orgID, userID, err := a.getOrgAndUserID(r)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusUnauthorized, "Unauthorized", nil, "")
	}

	var req struct {
		Name            string              `json:"name"`
		Keywords        []string            `json:"keywords"`
		MatchType       models.MatchType    `json:"match_type"`
		ResponseType    models.ResponseType `json:"response_type"`
		ResponseContent map[string]any      `json:"response_content"`
		Priority        int                 `json:"priority"`
		Enabled         bool                `json:"enabled"`
	}

	if err := json.Unmarshal(r.RequestCtx.PostBody(), &req); err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid request body", nil, "")
	}

	if len(req.Keywords) == 0 {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "At least one keyword is required", nil, "")
	}

	// Set defaults
	if req.MatchType == "" {
		req.MatchType = models.MatchTypeContains
	}
	if req.ResponseType == "" {
		req.ResponseType = models.ResponseTypeText
	}
	if req.Name == "" {
		req.Name = req.Keywords[0]
	}

	rule := models.KeywordRule{
		BaseModel:       models.BaseModel{ID: uuid.New()},
		OrganizationID:  orgID,
		Name:            req.Name,
		Keywords:        req.Keywords,
		MatchType:       req.MatchType,
		ResponseType:    req.ResponseType,
		ResponseContent: models.JSONB(req.ResponseContent),
		Priority:        req.Priority,
		IsEnabled:       req.Enabled,
		CreatedByID:     &userID,
		UpdatedByID:     &userID,
	}

	if err := a.DB.Create(&rule).Error; err != nil {
		a.Log.Error("Failed to create keyword rule", "error", err)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to create keyword rule", nil, "")
	}

	// Invalidate cache
	a.InvalidateKeywordRulesCache(orgID)

	a.logAudit(orgID, userID, "keyword_rule", rule.ID, models.AuditActionCreated, nil, &rule)

	return r.SendEnvelope(map[string]any{
		"id":      rule.ID.String(),
		"message": "Keyword rule created successfully",
	})
}

// GetKeywordRule gets a single keyword rule
func (a *App) GetKeywordRule(r *fastglue.Request) error {
	orgID, err := a.getOrgID(r)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusUnauthorized, "Unauthorized", nil, "")
	}

	id, err := parsePathUUID(r, "id", "rule")
	if err != nil {
		return nil
	}

	var rule models.KeywordRule
	if err := a.DB.Where("id = ? AND organization_id = ?", id, orgID).
		Preload("CreatedBy").Preload("UpdatedBy").
		First(&rule).Error; err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusNotFound, "Keyword rule not found", nil, "")
	}

	responseContent, _ := json.Marshal(rule.ResponseContent)
	response := KeywordRuleResponse{
		ID:              rule.ID.String(),
		Name:            rule.Name,
		Keywords:        rule.Keywords,
		MatchType:       rule.MatchType,
		ResponseType:    rule.ResponseType,
		ResponseContent: responseContent,
		Priority:        rule.Priority,
		Enabled:         rule.IsEnabled,
		CreatedAt:       rule.CreatedAt.Format(time.RFC3339),
		UpdatedAt:       rule.UpdatedAt.Format(time.RFC3339),
	}
	if rule.CreatedBy != nil {
		response.CreatedByName = rule.CreatedBy.FullName
	}
	if rule.UpdatedBy != nil {
		response.UpdatedByName = rule.UpdatedBy.FullName
	}

	return r.SendEnvelope(response)
}

// UpdateKeywordRule updates a keyword rule
func (a *App) UpdateKeywordRule(r *fastglue.Request) error {
	orgID, userID, err := a.getOrgAndUserID(r)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusUnauthorized, "Unauthorized", nil, "")
	}

	id, err := parsePathUUID(r, "id", "rule")
	if err != nil {
		return nil
	}

	rule, err := findByIDAndOrg[models.KeywordRule](a.DB, r, id, orgID, "Keyword rule")
	if err != nil {
		return nil
	}

	// Capture old state for audit
	oldRule := *rule

	var req struct {
		Name            *string              `json:"name"`
		Keywords        []string             `json:"keywords"`
		MatchType       *models.MatchType    `json:"match_type"`
		ResponseType    *models.ResponseType `json:"response_type"`
		ResponseContent map[string]any       `json:"response_content"`
		Priority        *int                 `json:"priority"`
		Enabled         *bool                `json:"enabled"`
	}

	if err := json.Unmarshal(r.RequestCtx.PostBody(), &req); err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid request body", nil, "")
	}

	// Update fields if provided
	if req.Name != nil {
		rule.Name = *req.Name
	}
	if len(req.Keywords) > 0 {
		rule.Keywords = req.Keywords
	}
	if req.MatchType != nil {
		rule.MatchType = *req.MatchType
	}
	if req.ResponseType != nil {
		rule.ResponseType = *req.ResponseType
	}
	if req.ResponseContent != nil {
		rule.ResponseContent = models.JSONB(req.ResponseContent)
	}
	if req.Priority != nil {
		rule.Priority = *req.Priority
	}
	if req.Enabled != nil {
		rule.IsEnabled = *req.Enabled
	}
	rule.UpdatedByID = &userID

	if err := a.DB.Save(rule).Error; err != nil {
		a.Log.Error("Failed to update keyword rule", "error", err)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to update keyword rule", nil, "")
	}

	// Invalidate cache
	a.InvalidateKeywordRulesCache(orgID)

	a.logAudit(orgID, userID, "keyword_rule", rule.ID, models.AuditActionUpdated, &oldRule, rule)

	return r.SendEnvelope(map[string]any{
		"message": "Keyword rule updated successfully",
	})
}

// DeleteKeywordRule deletes a keyword rule
func (a *App) DeleteKeywordRule(r *fastglue.Request) error {
	orgID, userID, err := a.getOrgAndUserID(r)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusUnauthorized, "Unauthorized", nil, "")
	}

	id, err := parsePathUUID(r, "id", "rule")
	if err != nil {
		return nil
	}

	// Load the rule before deleting for audit
	var rule models.KeywordRule
	if err := a.DB.Where("id = ? AND organization_id = ?", id, orgID).First(&rule).Error; err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusNotFound, "Keyword rule not found", nil, "")
	}

	if err := a.DB.Delete(&rule).Error; err != nil {
		a.Log.Error("Failed to delete keyword rule", "error", err)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to delete keyword rule", nil, "")
	}

	// Invalidate cache
	a.InvalidateKeywordRulesCache(orgID)

	a.logAudit(orgID, userID, "keyword_rule", id, models.AuditActionDeleted, &rule, nil)

	return r.SendEnvelope(map[string]any{
		"message": "Keyword rule deleted successfully",
	})
}

// ListChatbotFlows lists all chatbot flows
func (a *App) ListChatbotFlows(r *fastglue.Request) error {
	orgID, userID, err := a.getOrgAndUserID(r)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusUnauthorized, "Unauthorized", nil, "")
	}

	if !a.HasPermission(userID, models.ResourceFlowsChatbot, models.ActionRead, orgID) {
		return r.SendErrorEnvelope(fasthttp.StatusForbidden, "Permission denied", nil, "")
	}

	pg := parsePagination(r)
	search := string(r.RequestCtx.QueryArgs().Peek("search"))

	query := a.DB.Model(&models.ChatbotFlow{}).Where("organization_id = ?", orgID)

	// Apply search filter - search by name, description, or trigger keywords
	if search != "" {
		searchPattern := "%" + search + "%"
		query = query.Where("name ILIKE ? OR description ILIKE ? OR trigger_keywords::text ILIKE ?", searchPattern, searchPattern, searchPattern)
	}

	var total int64
	query.Count(&total)

	var flows []models.ChatbotFlow
	if err := pg.Apply(query.Order("created_at DESC")).
		Find(&flows).Error; err != nil {
		a.Log.Error("Failed to fetch flows", "error", err)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to fetch flows", nil, "")
	}

	response := make([]ChatbotFlowResponse, len(flows))
	for i, flow := range flows {
		response[i] = ChatbotFlowResponse{
			ID:              flow.ID.String(),
			Name:            flow.Name,
			Description:     flow.Description,
			TriggerKeywords: flow.TriggerKeywords,
			StepsCount:      chatbotFlowStepCount(flow.Graph),
			Enabled:         flow.IsEnabled,
			CreatedAt:       flow.CreatedAt.Format(time.RFC3339),
		}
	}

	return r.SendEnvelope(listEnvelope("flows", response, total, pg))
}

func chatbotFlowStepCount(raw models.JSONB) int {
	graph, err := parseChatGraph(raw)
	if err != nil || graph == nil {
		return 0
	}

	count := 0
	for _, node := range graph.Nodes {
		if node.Type != ChatNodeStart {
			count++
		}
	}
	return count
}

// CreateChatbotFlow creates a new chatbot flow
func (a *App) CreateChatbotFlow(r *fastglue.Request) error {
	orgID, userID, err := a.getOrgAndUserID(r)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusUnauthorized, "Unauthorized", nil, "")
	}

	if !a.HasPermission(userID, models.ResourceFlowsChatbot, models.ActionWrite, orgID) {
		return r.SendErrorEnvelope(fasthttp.StatusForbidden, "Permission denied", nil, "")
	}

	var req struct {
		Name              string         `json:"name"`
		Description       string         `json:"description"`
		TriggerKeywords   []string       `json:"trigger_keywords"`
		InitialMessage    string         `json:"initial_message"`
		CompletionMessage string         `json:"completion_message"`
		OnCompleteAction  string         `json:"on_complete_action"`
		CompletionConfig  map[string]any `json:"completion_config"`
		PanelConfig       map[string]any `json:"panel_config"`
		Graph             map[string]any `json:"graph"`
		Enabled           bool           `json:"enabled"`
	}

	if err := json.Unmarshal(r.RequestCtx.PostBody(), &req); err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid request body", nil, "")
	}

	if req.Name == "" {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Name is required", nil, "")
	}

	flow := models.ChatbotFlow{
		BaseModel:         models.BaseModel{ID: uuid.New()},
		OrganizationID:    orgID,
		Name:              req.Name,
		Description:       req.Description,
		TriggerKeywords:   req.TriggerKeywords,
		InitialMessage:    req.InitialMessage,
		CompletionMessage: req.CompletionMessage,
		OnCompleteAction:  req.OnCompleteAction,
		CompletionConfig:  models.JSONB(req.CompletionConfig),
		PanelConfig:       models.JSONB(req.PanelConfig),
		Graph:             models.JSONB(req.Graph),
		IsEnabled:         req.Enabled,
		CreatedByID:       &userID,
		UpdatedByID:       &userID,
	}

	if err := a.DB.Create(&flow).Error; err != nil {
		a.Log.Error("Failed to create flow", "error", err)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to create flow", nil, "")
	}

	// Invalidate cache
	a.InvalidateChatbotFlowsCache(orgID)

	a.logAudit(orgID, userID,
		"chatbot_flow", flow.ID, models.AuditActionCreated, nil, &flow)

	return r.SendEnvelope(map[string]any{
		"id":      flow.ID.String(),
		"message": "Flow created successfully",
	})
}

// GetChatbotFlow gets a single chatbot flow with steps
func (a *App) GetChatbotFlow(r *fastglue.Request) error {
	orgID, userID, err := a.getOrgAndUserID(r)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusUnauthorized, "Unauthorized", nil, "")
	}

	if !a.HasPermission(userID, models.ResourceFlowsChatbot, models.ActionRead, orgID) {
		return r.SendErrorEnvelope(fasthttp.StatusForbidden, "Permission denied", nil, "")
	}

	id, err := parsePathUUID(r, "id", "flow")
	if err != nil {
		return nil
	}

	var flow models.ChatbotFlow
	if err := a.DB.Where("id = ? AND organization_id = ?", id, orgID).
		Preload("CreatedBy").Preload("UpdatedBy").
		First(&flow).Error; err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusNotFound, "Flow not found", nil, "")
	}

	return r.SendEnvelope(flow)
}

// UpdateChatbotFlow updates a chatbot flow
func (a *App) UpdateChatbotFlow(r *fastglue.Request) error {
	orgID, userID, err := a.getOrgAndUserID(r)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusUnauthorized, "Unauthorized", nil, "")
	}

	if !a.HasPermission(userID, models.ResourceFlowsChatbot, models.ActionWrite, orgID) {
		return r.SendErrorEnvelope(fasthttp.StatusForbidden, "Permission denied", nil, "")
	}

	id, err := parsePathUUID(r, "id", "flow")
	if err != nil {
		return nil
	}

	flow, err := findByIDAndOrg[models.ChatbotFlow](a.DB, r, id, orgID, "Flow")
	if err != nil {
		return nil
	}

	oldFlow := *flow // value copy for audit

	var req struct {
		Name              *string        `json:"name"`
		Description       *string        `json:"description"`
		TriggerKeywords   []string       `json:"trigger_keywords"`
		InitialMessage    *string        `json:"initial_message"`
		CompletionMessage *string        `json:"completion_message"`
		OnCompleteAction  *string        `json:"on_complete_action"`
		CompletionConfig  map[string]any `json:"completion_config"`
		PanelConfig       map[string]any `json:"panel_config"`
		Graph             map[string]any `json:"graph"`
		Enabled           *bool          `json:"enabled"`
	}

	if err := json.Unmarshal(r.RequestCtx.PostBody(), &req); err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid request body", nil, "")
	}

	if req.Name != nil {
		flow.Name = *req.Name
	}
	if req.Description != nil {
		flow.Description = *req.Description
	}
	if len(req.TriggerKeywords) > 0 {
		flow.TriggerKeywords = req.TriggerKeywords
	}
	if req.InitialMessage != nil {
		flow.InitialMessage = *req.InitialMessage
	}
	if req.CompletionMessage != nil {
		flow.CompletionMessage = *req.CompletionMessage
	}
	if req.OnCompleteAction != nil {
		flow.OnCompleteAction = *req.OnCompleteAction
	}
	if req.CompletionConfig != nil {
		flow.CompletionConfig = models.JSONB(req.CompletionConfig)
	}
	if req.PanelConfig != nil {
		flow.PanelConfig = models.JSONB(req.PanelConfig)
	}
	if req.Graph != nil {
		flow.Graph = models.JSONB(req.Graph)
	}
	if req.Enabled != nil {
		flow.IsEnabled = *req.Enabled
	}
	flow.UpdatedByID = &userID

	if err := a.DB.Save(flow).Error; err != nil {
		a.Log.Error("Failed to update flow", "error", err)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to update flow", nil, "")
	}

	a.InvalidateChatbotFlowsCache(orgID)

	a.logAudit(orgID, userID,
		"chatbot_flow", flow.ID, models.AuditActionUpdated, &oldFlow, flow)

	return r.SendEnvelope(map[string]any{
		"message": "Flow updated successfully",
	})
}

// DeleteChatbotFlow deletes a chatbot flow
func (a *App) DeleteChatbotFlow(r *fastglue.Request) error {
	orgID, userID, err := a.getOrgAndUserID(r)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusUnauthorized, "Unauthorized", nil, "")
	}

	if !a.HasPermission(userID, models.ResourceFlowsChatbot, models.ActionDelete, orgID) {
		return r.SendErrorEnvelope(fasthttp.StatusForbidden, "Permission denied", nil, "")
	}

	id, err := parsePathUUID(r, "id", "flow")
	if err != nil {
		return nil
	}

	// Load flow for audit before deleting
	var flowForAudit models.ChatbotFlow
	a.DB.Where("id = ? AND organization_id = ?", id, orgID).First(&flowForAudit)

	// Delete flow and legacy steps in a nested transaction/savepoint. Using
	// GORM's Transaction helper works both with and without the outer RLS
	// request transaction.
	deleteErr := a.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("flow_id = ?", id).Delete(&models.ChatbotFlowStep{}).Error; err != nil {
			return err
		}
		result := tx.Where("id = ? AND organization_id = ?", id, orgID).Delete(&models.ChatbotFlow{})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return gorm.ErrRecordNotFound
		}
		return nil
	})
	if deleteErr != nil {
		if deleteErr == gorm.ErrRecordNotFound {
			return r.SendErrorEnvelope(fasthttp.StatusNotFound, "Flow not found", nil, "")
		}
		a.Log.Error("Failed to delete flow", "error", deleteErr)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to delete flow", nil, "")
	}

	// Invalidate cache
	a.InvalidateChatbotFlowsCache(orgID)

	a.logAudit(orgID, userID,
		"chatbot_flow", id, models.AuditActionDeleted, &flowForAudit, nil)

	return r.SendEnvelope(map[string]any{
		"message": "Flow deleted successfully",
	})
}

// ListAIContexts lists all AI contexts
func (a *App) ListAIContexts(r *fastglue.Request) error {
	orgID, err := a.getOrgID(r)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusUnauthorized, "Unauthorized", nil, "")
	}

	pg := parsePagination(r)
	search := string(r.RequestCtx.QueryArgs().Peek("search"))

	query := a.DB.Model(&models.AIContext{}).Where("organization_id = ?", orgID)

	// Apply search filter - search by name, static content, or trigger keywords
	if search != "" {
		searchPattern := "%" + search + "%"
		query = query.Where("name ILIKE ? OR static_content ILIKE ? OR trigger_keywords::text ILIKE ?", searchPattern, searchPattern, searchPattern)
	}

	var total int64
	query.Count(&total)

	var contexts []models.AIContext
	if err := pg.Apply(query.Preload("CreatedBy").Preload("UpdatedBy").Order("priority DESC, created_at DESC")).
		Find(&contexts).Error; err != nil {
		a.Log.Error("Failed to fetch AI contexts", "error", err)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to fetch AI contexts", nil, "")
	}

	response := make([]AIContextResponse, len(contexts))
	for i, ctx := range contexts {
		resp := AIContextResponse{
			ID:              ctx.ID.String(),
			Name:            ctx.Name,
			ContextType:     ctx.ContextType,
			TriggerKeywords: ctx.TriggerKeywords,
			StaticContent:   ctx.StaticContent,
			ApiConfig:       ctx.ApiConfig,
			Enabled:         ctx.IsEnabled,
			Priority:        ctx.Priority,
			CreatedAt:       ctx.CreatedAt.Format(time.RFC3339),
			UpdatedAt:       ctx.UpdatedAt.Format(time.RFC3339),
		}
		if ctx.CreatedBy != nil {
			resp.CreatedByName = ctx.CreatedBy.FullName
		}
		if ctx.UpdatedBy != nil {
			resp.UpdatedByName = ctx.UpdatedBy.FullName
		}
		response[i] = resp
	}

	return r.SendEnvelope(listEnvelope("contexts", response, total, pg))
}

// CreateAIContext creates a new AI context
func (a *App) CreateAIContext(r *fastglue.Request) error {
	orgID, userID, err := a.getOrgAndUserID(r)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusUnauthorized, "Unauthorized", nil, "")
	}

	var req struct {
		Name            string             `json:"name"`
		ContextType     models.ContextType `json:"context_type"`
		TriggerKeywords []string           `json:"trigger_keywords"`
		StaticContent   string             `json:"static_content"`
		ApiConfig       models.JSONB       `json:"api_config"`
		Priority        int                `json:"priority"`
		Enabled         bool               `json:"enabled"`
	}

	if err := json.Unmarshal(r.RequestCtx.PostBody(), &req); err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid request body", nil, "")
	}

	if req.Name == "" {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Name is required", nil, "")
	}
	if req.ContextType == "" {
		req.ContextType = models.ContextTypeStatic
	}

	ctx := models.AIContext{
		BaseModel:       models.BaseModel{ID: uuid.New()},
		OrganizationID:  orgID,
		Name:            req.Name,
		ContextType:     req.ContextType,
		TriggerKeywords: req.TriggerKeywords,
		StaticContent:   req.StaticContent,
		ApiConfig:       req.ApiConfig,
		Priority:        req.Priority,
		IsEnabled:       req.Enabled,
		CreatedByID:     &userID,
		UpdatedByID:     &userID,
	}

	if err := a.DB.Create(&ctx).Error; err != nil {
		a.Log.Error("Failed to create AI context", "error", err)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to create AI context", nil, "")
	}

	// Invalidate cache
	a.InvalidateAIContextsCache(orgID)

	a.logAudit(orgID, userID, "ai_context", ctx.ID, models.AuditActionCreated, nil, &ctx)

	return r.SendEnvelope(map[string]any{
		"id":      ctx.ID.String(),
		"message": "AI context created successfully",
	})
}

// GetAIContext gets a single AI context
func (a *App) GetAIContext(r *fastglue.Request) error {
	orgID, err := a.getOrgID(r)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusUnauthorized, "Unauthorized", nil, "")
	}

	id, err := parsePathUUID(r, "id", "context")
	if err != nil {
		return nil
	}

	var aiCtx models.AIContext
	if err := a.DB.Where("id = ? AND organization_id = ?", id, orgID).
		Preload("CreatedBy").Preload("UpdatedBy").
		First(&aiCtx).Error; err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusNotFound, "AI context not found", nil, "")
	}

	response := AIContextResponse{
		ID:              aiCtx.ID.String(),
		Name:            aiCtx.Name,
		ContextType:     aiCtx.ContextType,
		TriggerKeywords: aiCtx.TriggerKeywords,
		StaticContent:   aiCtx.StaticContent,
		ApiConfig:       aiCtx.ApiConfig,
		Enabled:         aiCtx.IsEnabled,
		Priority:        aiCtx.Priority,
		CreatedAt:       aiCtx.CreatedAt.Format(time.RFC3339),
		UpdatedAt:       aiCtx.UpdatedAt.Format(time.RFC3339),
	}
	if aiCtx.CreatedBy != nil {
		response.CreatedByName = aiCtx.CreatedBy.FullName
	}
	if aiCtx.UpdatedBy != nil {
		response.UpdatedByName = aiCtx.UpdatedBy.FullName
	}

	return r.SendEnvelope(response)
}

// UpdateAIContext updates an AI context
func (a *App) UpdateAIContext(r *fastglue.Request) error {
	orgID, userID, err := a.getOrgAndUserID(r)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusUnauthorized, "Unauthorized", nil, "")
	}

	id, err := parsePathUUID(r, "id", "context")
	if err != nil {
		return nil
	}

	aiCtx, err := findByIDAndOrg[models.AIContext](a.DB, r, id, orgID, "AI context")
	if err != nil {
		return nil
	}

	// Capture old state for audit
	oldCtx := *aiCtx

	var req struct {
		Name            *string             `json:"name"`
		ContextType     *models.ContextType `json:"context_type"`
		TriggerKeywords []string            `json:"trigger_keywords"`
		StaticContent   *string             `json:"static_content"`
		ApiConfig       *models.JSONB       `json:"api_config"`
		Priority        *int                `json:"priority"`
		Enabled         *bool               `json:"enabled"`
	}

	if err := json.Unmarshal(r.RequestCtx.PostBody(), &req); err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid request body", nil, "")
	}

	if req.Name != nil {
		aiCtx.Name = *req.Name
	}
	if req.ContextType != nil {
		aiCtx.ContextType = *req.ContextType
	}
	if len(req.TriggerKeywords) > 0 {
		aiCtx.TriggerKeywords = req.TriggerKeywords
	}
	if req.StaticContent != nil {
		aiCtx.StaticContent = *req.StaticContent
	}
	if req.ApiConfig != nil {
		aiCtx.ApiConfig = *req.ApiConfig
	}
	if req.Priority != nil {
		aiCtx.Priority = *req.Priority
	}
	if req.Enabled != nil {
		aiCtx.IsEnabled = *req.Enabled
	}
	aiCtx.UpdatedByID = &userID

	if err := a.DB.Save(aiCtx).Error; err != nil {
		a.Log.Error("Failed to update AI context", "error", err)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to update AI context", nil, "")
	}

	// Invalidate cache
	a.InvalidateAIContextsCache(orgID)

	a.logAudit(orgID, userID, "ai_context", aiCtx.ID, models.AuditActionUpdated, &oldCtx, aiCtx)

	return r.SendEnvelope(map[string]any{
		"message": "AI context updated successfully",
	})
}

// DeleteAIContext deletes an AI context
func (a *App) DeleteAIContext(r *fastglue.Request) error {
	orgID, userID, err := a.getOrgAndUserID(r)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusUnauthorized, "Unauthorized", nil, "")
	}

	id, err := parsePathUUID(r, "id", "context")
	if err != nil {
		return nil
	}

	// Load the context before deleting for audit
	var aiCtx models.AIContext
	if err := a.DB.Where("id = ? AND organization_id = ?", id, orgID).First(&aiCtx).Error; err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusNotFound, "AI context not found", nil, "")
	}

	if err := a.DB.Delete(&aiCtx).Error; err != nil {
		a.Log.Error("Failed to delete AI context", "error", err)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to delete AI context", nil, "")
	}

	// Invalidate cache
	a.InvalidateAIContextsCache(orgID)

	a.logAudit(orgID, userID, "ai_context", id, models.AuditActionDeleted, &aiCtx, nil)

	return r.SendEnvelope(map[string]any{
		"message": "AI context deleted successfully",
	})
}

// ListChatbotSessions lists chatbot sessions
func (a *App) ListChatbotSessions(r *fastglue.Request) error {
	orgID, err := a.getOrgID(r)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusUnauthorized, "Unauthorized", nil, "")
	}

	status := string(r.RequestCtx.QueryArgs().Peek("status"))

	query := a.DB.Where("organization_id = ?", orgID).
		Preload("Contact").
		Order("last_activity_at DESC")

	if status != "" {
		query = query.Where("status = ?", status)
	}

	var sessions []models.ChatbotSession
	if err := query.Limit(100).Find(&sessions).Error; err != nil {
		a.Log.Error("Failed to fetch sessions", "error", err)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to fetch sessions", nil, "")
	}

	return r.SendEnvelope(map[string]any{
		"sessions": sessions,
	})
}

// GetChatbotSession gets a single chatbot session with messages
func (a *App) GetChatbotSession(r *fastglue.Request) error {
	orgID, err := a.getOrgID(r)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusUnauthorized, "Unauthorized", nil, "")
	}

	id, err := parsePathUUID(r, "id", "session")
	if err != nil {
		return nil
	}

	var session models.ChatbotSession
	if err := a.DB.Where("id = ? AND organization_id = ?", id, orgID).
		Preload("Contact").
		Preload("Messages").
		First(&session).Error; err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusNotFound, "Session not found", nil, "")
	}

	return r.SendEnvelope(session)
}

// getChatbotStats returns chatbot statistics for an organization
func (a *App) getChatbotStats(orgID uuid.UUID) ChatbotStatsResponse {
	var stats ChatbotStatsResponse

	// Total sessions
	a.DB.Model(&models.ChatbotSession{}).
		Where("organization_id = ?", orgID).
		Count(&stats.TotalSessions)

	// Active sessions
	a.DB.Model(&models.ChatbotSession{}).
		Where("organization_id = ? AND status = ?", orgID, models.SessionStatusActive).
		Count(&stats.ActiveSessions)

	// Messages handled (from chatbot_session_messages)
	a.DB.Model(&models.ChatbotSessionMessage{}).
		Joins("JOIN chatbot_sessions ON chatbot_sessions.id = chatbot_session_messages.session_id").
		Where("chatbot_sessions.organization_id = ?", orgID).
		Count(&stats.MessagesHandled)

	// AI responses are stored as session messages and inherit tenancy through
	// their parent session. Legacy executions used the literal step name,
	// while v2 graph executions persist the ID of the ai_response node.
	a.DB.Model(&models.ChatbotSessionMessage{}).
		Joins("JOIN chatbot_sessions ON chatbot_sessions.id = chatbot_session_messages.session_id").
		Joins(`
			LEFT JOIN chatbot_flows
				ON chatbot_flows.id = chatbot_sessions.current_flow_id
				AND chatbot_flows.organization_id = chatbot_sessions.organization_id
				AND chatbot_flows.deleted_at IS NULL
		`).
		Where(
			`
				chatbot_sessions.organization_id = ?
				AND chatbot_sessions.deleted_at IS NULL
				AND (
					chatbot_session_messages.step_name = ?
					OR EXISTS (
						SELECT 1
						FROM jsonb_array_elements(
							CASE
								WHEN jsonb_typeof(chatbot_flows.graph->'nodes') = 'array'
								THEN chatbot_flows.graph->'nodes'
								ELSE '[]'::jsonb
							END
						) AS node
						WHERE node->>'type' = 'ai_response'
							AND node->>'id' = chatbot_session_messages.step_name
					)
				)
			`,
			orgID,
			"ai_response",
		).
		Count(&stats.AIResponses)

	// Agent transfers
	a.DB.Model(&models.AgentTransfer{}).
		Where("organization_id = ?", orgID).
		Count(&stats.AgentTransfers)

	// Keywords count
	a.DB.Model(&models.KeywordRule{}).
		Where("organization_id = ?", orgID).
		Count(&stats.KeywordsCount)

	// Flows count
	a.DB.Model(&models.ChatbotFlow{}).
		Where("organization_id = ?", orgID).
		Count(&stats.FlowsCount)

	// AI contexts count
	a.DB.Model(&models.AIContext{}).
		Where("organization_id = ?", orgID).
		Count(&stats.AIContextsCount)

	return stats
}
