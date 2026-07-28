package handlers

import (
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/channel"
	"github.com/shridarpatil/whatomate/internal/models"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// lockChannelAIOrganizationScopeTx is the tenant-wide linearization point
// shared by AI scheduling and control-plane changes. The lock is held until
// the caller's transaction commits.
func lockChannelAIOrganizationScopeTx(
	tx *gorm.DB,
	organizationID uuid.UUID,
) error {
	if tx == nil || organizationID == uuid.Nil {
		return errors.New("tenant organization AI transaction is required")
	}
	var organization models.Organization
	return tx.Clauses(clause.Locking{Strength: "UPDATE"}).
		Select("id").
		Where("id = ?", organizationID).
		First(&organization).Error
}

// enqueueChannelAIReply creates only a durable scheduled job. Generation and
// all provider network activity belong to the worker, after this webhook
// transaction has committed.
func enqueueChannelAIReply(
	tx *gorm.DB,
	account *models.ChannelAccount,
	conversation *models.InboxConversation,
	message *models.Message,
	parts []channel.MessagePart,
	serviceWindowAt time.Time,
	evaluatedAt time.Time,
) error {
	if tx == nil || account == nil || conversation == nil || message == nil {
		return errors.New("channel AI reply scheduling requires persisted tenant records")
	}
	if account.Channel != models.ChannelInstagram &&
		account.Channel != models.ChannelMessenger {
		return nil
	}
	if account.Provider != channel.RelayProvider ||
		account.Status != models.ChannelAccountStatusActive ||
		!boolConfigValue(account.Config, "outbound_enabled") ||
		!boolConfigValue(account.Config, "ai_reply_enabled") ||
		inboxConversationAIIsPaused(conversation.Config) {
		return nil
	}
	if message.Direction != models.DirectionIncoming || strings.TrimSpace(messagePreview(parts)) == "" {
		return nil
	}

	serviceWindowAt = serviceWindowAt.UTC()
	if serviceWindowAt.IsZero() {
		return errors.New("channel AI reply scheduling requires the provider service-window anchor")
	}
	serviceWindowEndsAt := channel.InboundServiceWindowEndsAt(
		account.Channel,
		serviceWindowAt,
	)
	if evaluatedAt.IsZero() {
		evaluatedAt = time.Now().UTC()
	}
	if serviceWindowEndsAt == nil || !serviceWindowEndsAt.After(evaluatedAt.UTC()) {
		// Persist the customer message for CRM history, but never queue Qwen for
		// a delayed delivery that Meta already considers outside its window.
		return nil
	}
	settings, err := channel.ResolveAIReplySettings(
		tx,
		account.OrganizationID,
		account.Name,
	)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if !settings.IsEnabled ||
		!settings.AI.Enabled ||
		settings.AI.Provider != models.AIProviderQwen ||
		strings.TrimSpace(settings.AI.APIKey) == "" {
		return nil
	}
	payload, err := valueToJSONB(models.ChannelAIReplyJobPayload{
		OrganizationID:   account.OrganizationID,
		ChannelAccountID: account.ID,
		ConversationID:   conversation.ID,
		InboundMessageID: message.ID,
		ServiceWindowAt:  serviceWindowAt,
	})
	if err != nil {
		return err
	}
	messageID := message.ID
	job := models.ScheduledJob{
		OrganizationID: account.OrganizationID,
		Kind:           models.ScheduledJobKindChannelAIReply,
		AggregateType:  models.ChannelAIReplyAggregateType,
		AggregateID:    &messageID,
		RunAt:          time.Now().UTC(),
		Status:         models.ScheduledJobStatusPending,
		MaxAttempts:    5,
		IdempotencyKey: models.ChannelAIReplyIdempotencyKey(message.ID),
		Payload:        payload,
		Version:        1,
	}
	return tx.Clauses(clause.OnConflict{
		Columns: []clause.Column{
			{Name: "organization_id"},
			{Name: "idempotency_key"},
		},
		DoNothing: true,
	}).Create(&job).Error
}

func cancelChannelAIReplyJobsForAccountTx(
	tx *gorm.DB,
	organizationID, channelAccountID uuid.UUID,
	reason string,
) error {
	if tx == nil || organizationID == uuid.Nil || channelAccountID == uuid.Nil {
		return errors.New("tenant channel account transaction is required")
	}
	if err := cancelChannelAIScheduledJobsTx(
		tx,
		organizationID,
		"payload ->> 'channel_account_id' = ?",
		channelAccountID.String(),
		reason,
	); err != nil {
		return err
	}
	return cancelChannelAIOutboxJobsTx(
		tx,
		organizationID,
		"channel_account_id = ?",
		channelAccountID,
		reason,
	)
}

func cancelChannelAIScheduledJobsForConversationTx(
	tx *gorm.DB,
	organizationID, conversationID uuid.UUID,
	reason string,
) error {
	return cancelChannelAIScheduledJobsTx(
		tx,
		organizationID,
		"payload ->> 'conversation_id' = ?",
		conversationID.String(),
		reason,
	)
}

func cancelChannelAIOutboxJobsForConversationTx(
	tx *gorm.DB,
	organizationID, conversationID uuid.UUID,
	reason string,
) error {
	return cancelChannelAIOutboxJobsTx(
		tx,
		organizationID,
		"conversation_id = ?",
		conversationID,
		reason,
	)
}

func cancelChannelAIReplyJobsForOrganizationTx(
	tx *gorm.DB,
	organizationID uuid.UUID,
	reason string,
) error {
	if tx == nil || organizationID == uuid.Nil {
		return errors.New("tenant organization transaction is required")
	}
	if err := cancelChannelAIScheduledJobsForOrganizationTx(
		tx,
		organizationID,
		reason,
	); err != nil {
		return err
	}
	return cancelChannelAIOutboxJobsForOrganizationTx(
		tx,
		organizationID,
		reason,
	)
}

func cancelChannelAIScheduledJobsForOrganizationTx(
	tx *gorm.DB,
	organizationID uuid.UUID,
	reason string,
) error {
	if tx == nil || organizationID == uuid.Nil {
		return errors.New("tenant organization transaction is required")
	}
	return cancelChannelAIScheduledJobsTx(
		tx,
		organizationID,
		"",
		"",
		reason,
	)
}

func cancelChannelAIOutboxJobsForOrganizationTx(
	tx *gorm.DB,
	organizationID uuid.UUID,
	reason string,
) error {
	if tx == nil || organizationID == uuid.Nil {
		return errors.New("tenant organization transaction is required")
	}
	return cancelChannelAIOutboxJobsTx(
		tx,
		organizationID,
		"",
		nil,
		reason,
	)
}

func cancelChannelAIScheduledJobsTx(
	tx *gorm.DB,
	organizationID uuid.UUID,
	bindingPredicate string,
	bindingID string,
	reason string,
) error {
	now := time.Now().UTC()
	query := tx.Model(&models.ScheduledJob{}).
		Where(
			"organization_id = ? AND kind = ? AND status IN ?",
			organizationID,
			models.ScheduledJobKindChannelAIReply,
			[]models.ScheduledJobStatus{
				models.ScheduledJobStatusPending,
				models.ScheduledJobStatusProcessing,
			},
		)
	if strings.TrimSpace(bindingPredicate) != "" {
		query = query.Where(bindingPredicate, bindingID)
	}
	return query.Updates(map[string]any{
		"status":       models.ScheduledJobStatusCancelled,
		"completed_at": now,
		"last_error":   strings.TrimSpace(reason),
		"locked_at":    nil,
		"locked_by":    "",
		"updated_at":   now,
	}).Error
}

func cancelChannelAIOutboxJobsTx(
	tx *gorm.DB,
	organizationID uuid.UUID,
	bindingPredicate string,
	bindingID any,
	reason string,
) error {
	statuses := []models.OutboxJobStatus{
		models.OutboxJobStatusPending,
		models.OutboxJobStatusRetrying,
		models.OutboxJobStatusProcessing,
	}
	filteredJobs := func() *gorm.DB {
		query := tx.Model(&models.OutboxJob{}).
			Where(
				"organization_id = ? AND status IN ? AND idempotency_key LIKE ? AND LOWER(COALESCE(payload->'metadata'->>'ai_generated', 'false')) = 'true'",
				organizationID,
				statuses,
				models.ChannelAIReplyKeyPrefix+"%",
			)
		if strings.TrimSpace(bindingPredicate) != "" {
			query = query.Where(bindingPredicate, bindingID)
		}
		return query
	}

	var messageIDs []uuid.UUID
	if err := filteredJobs().
		Where("message_id IS NOT NULL").
		Pluck("message_id", &messageIDs).Error; err != nil {
		return err
	}

	now := time.Now().UTC()
	result := filteredJobs().Updates(map[string]any{
		"status":          models.OutboxJobStatusCancelled,
		"failed_at":       now,
		"last_error_code": "ai_reply_cancelled",
		"last_error":      strings.TrimSpace(reason),
		"locked_at":       nil,
		"locked_by":       "",
		"updated_at":      now,
	})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 || len(messageIDs) == 0 {
		return nil
	}
	return tx.Model(&models.Message{}).
		Where(
			"organization_id = ? AND id IN ? AND status = ?",
			organizationID,
			messageIDs,
			models.MessageStatusPending,
		).
		Updates(map[string]any{
			"status":        models.MessageStatusFailed,
			"error_message": "Automatic AI reply cancelled before delivery",
			"updated_at":    now,
		}).Error
}
