package handlers

import (
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/database"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/valyala/fasthttp"
	"github.com/zerodha/fastglue"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type SetInboxConversationAIStateRequest struct {
	Paused bool `json:"paused"`
}

// SetInboxConversationAIState explicitly pauses or resumes automatic AI
// replies for one tenant conversation. Resuming applies to future inbound
// messages; policy-cancelled jobs are never resurrected.
func (a *App) SetInboxConversationAIState(r *fastglue.Request) error {
	organizationID, userID, err := a.requireAuth(
		r,
		models.ResourceConversations,
		models.ActionWrite,
	)
	if err != nil {
		return nil
	}
	conversationID, err := parsePathUUID(r, "id", "conversation")
	if err != nil {
		return nil
	}
	var request SetInboxConversationAIStateRequest
	if err := a.decodeRequest(r, &request); err != nil {
		return nil
	}

	var config models.JSONB
	err = a.DB.Transaction(func(tx *gorm.DB) error {
		var updateErr error
		config, updateErr = setInboxConversationAIStateTx(
			tx,
			organizationID,
			conversationID,
			request.Paused,
			&userID,
			map[bool]string{true: "manual_pause", false: "manual_resume"}[request.Paused],
		)
		return updateErr
	})
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return r.SendErrorEnvelope(
			fasthttp.StatusNotFound,
			"Conversation not found",
			nil,
			"",
		)
	}
	if err != nil {
		a.Log.Error(
			"Failed to update conversation AI state",
			"error",
			err,
			"organization_id",
			organizationID,
			"conversation_id",
			conversationID,
		)
		return r.SendErrorEnvelope(
			fasthttp.StatusInternalServerError,
			"Conversation AI state could not be updated",
			nil,
			"",
		)
	}
	return r.SendEnvelope(map[string]any{
		"conversation_id": conversationID,
		"ai_paused":       inboxConversationAIIsPaused(config),
		"ai_pause_reason": inboxConversationAIString(
			config,
			models.ConversationConfigAIPauseReason,
		),
	})
}

// setInboxConversationAIStateTx locks and updates the conversation config in
// the caller's transaction. Manual message persistence invokes this before
// creating its Message and OutboxJob, so handover always pauses AI atomically.
func setInboxConversationAIStateTx(
	tx *gorm.DB,
	organizationID, conversationID uuid.UUID,
	paused bool,
	userID *uuid.UUID,
	reason string,
) (models.JSONB, error) {
	if tx == nil || organizationID == uuid.Nil || conversationID == uuid.Nil {
		return nil, errors.New("tenant conversation transaction is required")
	}
	if err := lockChannelAIOrganizationScopeTx(tx, organizationID); err != nil {
		return nil, err
	}
	if paused {
		// ScheduledJob comes first because the finalizer already holds that row
		// before creating its Message/Outbox foreign keys. After it settles, the
		// contact policy lock linearizes against dispatch, and the outbox scan
		// catches any row that finalizer created while cancellation waited.
		if err := cancelChannelAIScheduledJobsForConversationTx(
			tx,
			organizationID,
			conversationID,
			"conversation_ai_paused",
		); err != nil {
			return nil, err
		}
		var policyScope models.InboxConversation
		if err := tx.Select("id", "organization_id", "contact_id").
			Where("id = ? AND organization_id = ?", conversationID, organizationID).
			First(&policyScope).Error; err != nil {
			return nil, err
		}
		if err := database.LockContactPolicyScope(
			tx,
			organizationID,
			policyScope.ContactID,
		); err != nil {
			return nil, err
		}
		if err := cancelChannelAIOutboxJobsForConversationTx(
			tx,
			organizationID,
			conversationID,
			"conversation_ai_paused",
		); err != nil {
			return nil, err
		}
	}
	var conversation models.InboxConversation
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
		Select("id", "organization_id", "config").
		Where("id = ? AND organization_id = ?", conversationID, organizationID).
		First(&conversation).Error; err != nil {
		return nil, err
	}
	config := cloneJSONB(conversation.Config)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	config[models.ConversationConfigAIPaused] = paused
	if paused {
		config[models.ConversationConfigAIPauseReason] = strings.TrimSpace(reason)
		config[models.ConversationConfigAIPausedAt] = now
		delete(config, models.ConversationConfigAIResumedAt)
		delete(config, models.ConversationConfigAIResumedByUserID)
		if userID != nil && *userID != uuid.Nil {
			config[models.ConversationConfigAIPausedByUserID] = userID.String()
		} else {
			delete(config, models.ConversationConfigAIPausedByUserID)
		}
	} else {
		config[models.ConversationConfigAIPauseReason] = ""
		config[models.ConversationConfigAIResumedAt] = now
		if userID != nil && *userID != uuid.Nil {
			config[models.ConversationConfigAIResumedByUserID] = userID.String()
		} else {
			delete(config, models.ConversationConfigAIResumedByUserID)
		}
	}
	result := tx.Model(&models.InboxConversation{}).
		Where("id = ? AND organization_id = ?", conversationID, organizationID).
		Updates(map[string]any{
			"config":     config,
			"updated_at": time.Now().UTC(),
		})
	if result.Error != nil {
		return nil, result.Error
	}
	if result.RowsAffected != 1 {
		return nil, gorm.ErrRecordNotFound
	}
	return config, nil
}

func inboxConversationAIIsPaused(config models.JSONB) bool {
	value, exists := config[models.ConversationConfigAIPaused]
	if !exists {
		return false
	}
	switch typed := value.(type) {
	case bool:
		return typed
	case string:
		normalized := strings.ToLower(strings.TrimSpace(typed))
		return normalized == "true" || normalized == "1" || normalized == "yes"
	default:
		return false
	}
}

func inboxConversationAIString(config models.JSONB, key string) string {
	value, _ := config[key].(string)
	return strings.TrimSpace(value)
}
