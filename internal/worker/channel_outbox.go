package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	channelapi "github.com/shridarpatil/whatomate/internal/channel"
	"github.com/shridarpatil/whatomate/internal/database"
	"github.com/shridarpatil/whatomate/internal/models"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	defaultChannelOutboxBatchSize    = 20
	defaultChannelOutboxPollInterval = time.Second
	defaultChannelOutboxLease        = 2 * time.Minute
)

var (
	errChannelOutboxUnlicensed = errors.New("omnichannel entitlement is not active")
	errChannelOutboxConsent    = errors.New("contact consent does not permit delivery")
)

// RunChannelOutbox runs the durable provider-neutral delivery loop until the
// context is cancelled. It is intended to be started alongside Worker.Run.
func (w *Worker) RunChannelOutbox(ctx context.Context) error {
	if w == nil || w.DB == nil {
		return errors.New("channel outbox worker requires a database")
	}
	workerID := "channel-outbox:" + uuid.NewString()
	ticker := time.NewTicker(defaultChannelOutboxPollInterval)
	defer ticker.Stop()

	for {
		if _, err := w.ProcessChannelOutboxBatch(ctx, workerID, defaultChannelOutboxBatchSize); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			w.Log.Error("Channel outbox batch failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// ProcessChannelOutboxBatch claims and processes at most limit jobs. Tenant
// IDs are read from the control plane, then every job query is performed inside
// database.WithTenant so RLS remains fail closed.
func (w *Worker) ProcessChannelOutboxBatch(ctx context.Context, workerID string, limit int) (int, error) {
	if w == nil || w.DB == nil {
		return 0, errors.New("channel outbox worker requires a database")
	}
	if strings.TrimSpace(workerID) == "" {
		return 0, errors.New("channel outbox worker ID is required")
	}
	if limit <= 0 || limit > 100 {
		limit = defaultChannelOutboxBatchSize
	}

	// A narrowly granted SECURITY DEFINER resolver exposes only organization
	// IDs with ready jobs. Actual rows are loaded after entering WithTenant.
	// The random UUID cursor gives every segment a chance without a full
	// organizations scan on each poll.
	var organizationIDs []uuid.UUID
	resolverErr := w.DB.Raw(
		"SELECT * FROM public.rereply_ready_channel_outbox_orgs(?, ?, ?)",
		uuid.New(),
		limit,
		time.Now().UTC().Add(-defaultChannelOutboxLease),
	).Scan(&organizationIDs).Error
	if resolverErr != nil {
		if w.Config != nil && w.Config.Database.RLSEnabled {
			return 0, fmt.Errorf("list channel outbox tenants: %w", resolverErr)
		}
		// Local/dev schemas may not install SECURITY DEFINER helpers. RLS is
		// explicitly disabled here, so a control-plane scan is a safe
		// compatibility fallback; production never falls back.
		if err := w.DB.Model(&models.Organization{}).
			Where("deleted_at IS NULL").
			Order("id").
			Pluck("id", &organizationIDs).Error; err != nil {
			return 0, fmt.Errorf("list development channel outbox tenants: %w", err)
		}
	}

	processed := 0
	var batchErrors []error
	for _, orgID := range organizationIDs {
		if processed >= limit || ctx.Err() != nil {
			break
		}
		jobID, claimed, err := w.claimChannelOutboxJob(orgID, workerID)
		if err != nil {
			batchErrors = append(batchErrors, fmt.Errorf("claim tenant %s: %w", orgID, err))
			continue
		}
		if !claimed {
			continue
		}
		processed++
		if err := w.deliverChannelOutboxJob(ctx, orgID, jobID, workerID); err != nil {
			batchErrors = append(batchErrors, fmt.Errorf("deliver job %s: %w", jobID, err))
		}
	}
	return processed, errors.Join(batchErrors...)
}

func (w *Worker) claimChannelOutboxJob(orgID uuid.UUID, workerID string) (uuid.UUID, bool, error) {
	var jobID uuid.UUID
	claimed := false
	err := database.WithTenant(w.DB, orgID, func(tx *gorm.DB) error {
		now := time.Now().UTC()
		staleBefore := now.Add(-defaultChannelOutboxLease)
		var candidate struct {
			ID uuid.UUID
		}
		if err := tx.Raw(`
			SELECT id
			FROM outbox_jobs
			WHERE organization_id = ?
			  AND deleted_at IS NULL
			  AND available_at <= ?
			  AND (
			    status IN (?, ?)
			    OR (status = ? AND (locked_at IS NULL OR locked_at < ?))
			  )
			ORDER BY priority DESC, available_at, created_at
			FOR UPDATE SKIP LOCKED
			LIMIT 1
		`,
			orgID,
			now,
			models.OutboxJobStatusPending,
			models.OutboxJobStatusRetrying,
			models.OutboxJobStatusProcessing,
			staleBefore,
		).Scan(&candidate).Error; err != nil {
			return err
		}
		if candidate.ID == uuid.Nil {
			return nil
		}
		result := tx.Model(&models.OutboxJob{}).
			Where("id = ? AND organization_id = ?", candidate.ID, orgID).
			Updates(map[string]any{
				"status":     models.OutboxJobStatusProcessing,
				"locked_at":  now,
				"locked_by":  workerID,
				"updated_at": now,
			})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return errors.New("channel outbox claim was lost")
		}
		jobID = candidate.ID
		claimed = true
		return nil
	})
	return jobID, claimed, err
}

func (w *Worker) deliverChannelOutboxJob(
	ctx context.Context,
	orgID, jobID uuid.UUID,
	workerID string,
) error {
	var job models.OutboxJob
	var account models.ChannelAccount
	var conversation models.InboxConversation
	var outbound channelapi.OutboundMessage
	loadErr := database.WithTenant(w.DB, orgID, func(tx *gorm.DB) error {
		if err := tx.
			Where(
				"id = ? AND organization_id = ? AND status = ? AND locked_by = ?",
				jobID,
				orgID,
				models.OutboxJobStatusProcessing,
				workerID,
			).
			First(&job).Error; err != nil {
			return err
		}
		entitled, err := channelapi.HasDurableOmnichannelEntitlement(
			tx,
			orgID,
			time.Now().UTC(),
		)
		if err != nil {
			return fmt.Errorf("evaluate omnichannel entitlement: %w", err)
		}
		if !entitled {
			return errChannelOutboxUnlicensed
		}
		if err := tx.
			Preload(
				"Credentials",
				"organization_id = ? AND status IN ? AND (expires_at IS NULL OR expires_at > ?)",
				orgID,
				[]models.ChannelCredentialStatus{
					models.ChannelCredentialStatusActive,
					models.ChannelCredentialStatusExpiring,
				},
				time.Now().UTC(),
			).
			Where("id = ? AND organization_id = ?", job.ChannelAccountID, orgID).
			First(&account).Error; err != nil {
			return err
		}
		if err := tx.
			Where(
				"id = ? AND organization_id = ? AND channel_account_id = ?",
				job.ConversationID,
				orgID,
				job.ChannelAccountID,
			).
			First(&conversation).Error; err != nil {
			return err
		}
		decoded, decodeErr := channelOutboundMessageForJob(&job)
		if decodeErr != nil {
			return fmt.Errorf("%w: invalid outbox payload", errChannelOutboxConsent)
		}
		outbound = decoded
		allowed, reason, consentErr := channelapi.OutboundConsentAllowed(
			tx,
			orgID,
			conversation.ContactID,
			account.ID,
			account.Channel,
			outbound.Purpose,
			time.Now().UTC(),
		)
		if consentErr != nil {
			return fmt.Errorf("evaluate outbound consent: %w", consentErr)
		}
		if !allowed {
			return fmt.Errorf("%w: %s", errChannelOutboxConsent, reason)
		}
		return nil
	})
	if loadErr != nil {
		if job.ID != uuid.Nil && errors.Is(loadErr, errChannelOutboxUnlicensed) {
			return w.failChannelOutboxJob(orgID, &job, workerID, loadErr, false)
		}
		if job.ID != uuid.Nil && errors.Is(loadErr, errChannelOutboxConsent) {
			return w.failChannelOutboxJob(orgID, &job, workerID, loadErr, false)
		}
		if job.ID != uuid.Nil && errors.Is(loadErr, gorm.ErrRecordNotFound) {
			return w.failChannelOutboxJob(
				orgID,
				&job,
				workerID,
				errors.New("channel account was not found"),
				false,
			)
		}
		return loadErr
	}

	if account.Status != models.ChannelAccountStatusActive ||
		!channelOutboxBool(account.Config, "outbound_enabled") {
		err := errors.New("channel account is not approved for outbound delivery")
		return w.failChannelOutboxJob(orgID, &job, workerID, err, false)
	}
	adapter, err := w.channelOutboxAdapter(&account)
	if err != nil {
		return w.failChannelOutboxJob(orgID, &job, workerID, err, retryableChannelError(err))
	}
	if err := channelapi.ValidateServiceWindow(
		adapter.Capabilities(&account),
		conversation.ServiceWindowEndsAt,
		outbound.Parts,
		time.Now().UTC(),
	); err != nil {
		return w.failChannelOutboxJob(orgID, &job, workerID, err, false)
	}

	result, sendErr := adapter.Send(ctx, &account, outbound)
	if sendErr != nil {
		return w.failChannelOutboxJob(orgID, &job, workerID, sendErr, retryableChannelError(sendErr))
	}
	if len(result.ProviderMessageIDs) == 0 {
		return w.failChannelOutboxJob(
			orgID,
			&job,
			workerID,
			errors.New("provider accepted the request without a message ID"),
			true,
		)
	}
	return w.completeChannelOutboxJob(orgID, &job, &account, workerID, result)
}

func (w *Worker) completeChannelOutboxJob(
	orgID uuid.UUID,
	job *models.OutboxJob,
	account *models.ChannelAccount,
	workerID string,
	result channelapi.SendResult,
) error {
	return database.WithTenant(w.DB, orgID, func(tx *gorm.DB) error {
		now := time.Now().UTC()
		providerMessageID := result.ProviderMessageIDs[0]
		update := tx.Model(&models.OutboxJob{}).
			Where(
				"id = ? AND organization_id = ? AND status = ? AND locked_by = ?",
				job.ID,
				orgID,
				models.OutboxJobStatusProcessing,
				workerID,
			).
			Updates(map[string]any{
				"status":          models.OutboxJobStatusSent,
				"sent_at":         now,
				"last_attempt_at": now,
				"attempt_count":   gorm.Expr("attempt_count + 1"),
				"locked_at":       nil,
				"locked_by":       "",
				"last_error":      "",
				"last_error_code": "",
				"updated_at":      now,
			})
		if update.Error != nil {
			return update.Error
		}
		if update.RowsAffected != 1 {
			return errors.New("channel outbox completion lost its lease")
		}
		if job.MessageID != nil {
			if err := tx.Model(&models.Message{}).
				Where("id = ? AND organization_id = ?", *job.MessageID, orgID).
				Updates(map[string]any{
					"status":               models.MessageStatusSent,
					"whats_app_message_id": providerMessageID,
					"error_message":        "",
					"updated_at":           now,
				}).Error; err != nil {
				return err
			}
		}
		if err := tx.Model(&models.ChannelAccount{}).
			Where("id = ? AND organization_id = ?", account.ID, orgID).
			Updates(map[string]any{
				"last_outbound_at": now,
				"updated_at":       now,
			}).Error; err != nil {
			return err
		}
		messageEvent := models.MessageEvent{
			OrganizationID:    orgID,
			ChannelAccountID:  account.ID,
			ConversationID:    job.ConversationID,
			MessageID:         job.MessageID,
			ProviderEventID:   firstChannelOutboxValue(result.ProviderRequestID, "outbox:"+job.ID.String()+":sent"),
			ExternalMessageID: providerMessageID,
			Type:              models.MessageEventTypeSent,
			OccurredAt:        firstChannelOutboxTime(result.AcceptedAt, now),
			Payload: models.JSONB{
				"provider_message_ids": result.ProviderMessageIDs,
			},
		}
		return tx.Clauses(clause.OnConflict{
			Columns: []clause.Column{
				{Name: "organization_id"},
				{Name: "channel_account_id"},
				{Name: "provider_event_id"},
			},
			DoNothing: true,
		}).Create(&messageEvent).Error
	})
}

func (w *Worker) failChannelOutboxJob(
	orgID uuid.UUID,
	job *models.OutboxJob,
	workerID string,
	deliveryErr error,
	retryable bool,
) error {
	attempt := job.AttemptCount + 1
	maxAttempts := job.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = 8
	}
	deadLetter := !retryable || attempt >= maxAttempts
	nextStatus := models.OutboxJobStatusRetrying
	if deadLetter {
		nextStatus = models.OutboxJobStatusFailed
	}
	errorCode := channelOutboxErrorCode(deliveryErr)
	errorMessage := channelOutboxErrorMessage(deliveryErr)

	persistErr := database.WithTenant(w.DB, orgID, func(tx *gorm.DB) error {
		now := time.Now().UTC()
		updates := map[string]any{
			"status":          nextStatus,
			"attempt_count":   attempt,
			"last_attempt_at": now,
			"last_error_code": errorCode,
			"last_error":      errorMessage,
			"locked_at":       nil,
			"locked_by":       "",
			"updated_at":      now,
		}
		if deadLetter {
			updates["failed_at"] = now
		} else {
			updates["available_at"] = now.Add(channelOutboxBackoff(attempt))
		}
		result := tx.Model(&models.OutboxJob{}).
			Where(
				"id = ? AND organization_id = ? AND status = ? AND locked_by = ?",
				job.ID,
				orgID,
				models.OutboxJobStatusProcessing,
				workerID,
			).
			Updates(updates)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return errors.New("channel outbox failure update lost its lease")
		}
		if !deadLetter {
			return nil
		}
		if job.MessageID != nil {
			if err := tx.Model(&models.Message{}).
				Where("id = ? AND organization_id = ?", *job.MessageID, orgID).
				Updates(map[string]any{
					"status":        models.MessageStatusFailed,
					"error_message": errorMessage,
					"updated_at":    now,
				}).Error; err != nil {
				return err
			}
		}
		messageEvent := models.MessageEvent{
			OrganizationID:   orgID,
			ChannelAccountID: job.ChannelAccountID,
			ConversationID:   job.ConversationID,
			MessageID:        job.MessageID,
			ProviderEventID:  fmt.Sprintf("outbox:%s:failed:%d", job.ID, attempt),
			Type:             models.MessageEventTypeFailed,
			OccurredAt:       now,
			ErrorCode:        errorCode,
			ErrorMessage:     errorMessage,
			Payload:          models.JSONB{},
		}
		return tx.Clauses(clause.OnConflict{
			Columns: []clause.Column{
				{Name: "organization_id"},
				{Name: "channel_account_id"},
				{Name: "provider_event_id"},
			},
			DoNothing: true,
		}).Create(&messageEvent).Error
	})
	if persistErr != nil {
		return errors.Join(deliveryErr, persistErr)
	}
	// Delivery errors are expected job outcomes once durably scheduled or dead
	// lettered; returning nil prevents the outer loop from double-reporting them.
	return nil
}

func (w *Worker) channelOutboxAdapter(account *models.ChannelAccount) (channelapi.Adapter, error) {
	if account == nil || account.Channel == models.ChannelTikTok {
		return nil, &channelapi.ProviderError{
			Operation: "resolve_adapter",
			Code:      "approval_required",
			Message:   "channel provider is approval-gated",
			Retryable: false,
		}
	}
	if !strings.EqualFold(account.Provider, channelapi.RelayProvider) {
		return nil, &channelapi.ProviderError{
			Operation: "resolve_adapter",
			Provider:  account.Provider,
			Code:      "adapter_unavailable",
			Message:   "channel provider adapter is unavailable",
			Retryable: false,
		}
	}
	encryptionKey := ""
	if w.Config != nil {
		encryptionKey = w.Config.App.EncryptionKey
	}
	if encryptionKey == "" {
		return nil, &channelapi.ProviderError{
			Operation: "resolve_adapter",
			Provider:  account.Provider,
			Code:      "encryption_not_configured",
			Message:   "channel credential encryption is not configured",
			Retryable: true,
		}
	}
	return channelapi.NewRelayAdapter(
		account.Channel,
		&http.Client{Timeout: 30 * time.Second},
		encryptionKey,
	), nil
}

func decodeChannelOutboxPayload(payload models.JSONB, destination any) error {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode channel outbox payload: %w", err)
	}
	if err := json.Unmarshal(encoded, destination); err != nil {
		return fmt.Errorf("decode channel outbox payload: %w", err)
	}
	return nil
}

func channelOutboundMessageForJob(job *models.OutboxJob) (channelapi.OutboundMessage, error) {
	if job == nil {
		return channelapi.OutboundMessage{}, errors.New("channel outbox job is required")
	}
	var outbound channelapi.OutboundMessage
	if err := decodeChannelOutboxPayload(job.Payload, &outbound); err != nil {
		return channelapi.OutboundMessage{}, err
	}
	// The persisted job key is authoritative even if an old or malicious
	// payload omitted or attempted to replace it.
	outbound.OrganizationID = job.OrganizationID
	outbound.IdempotencyKey = job.IdempotencyKey
	outbound.Purpose = job.Purpose
	if job.MessageID != nil {
		outbound.MessageID = *job.MessageID
	}
	return outbound, nil
}

func retryableChannelError(err error) bool {
	var providerErr *channelapi.ProviderError
	if errors.As(err, &providerErr) {
		return providerErr.Retryable
	}
	return true
}

func channelOutboxBackoff(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	if attempt > 20 {
		attempt = 20
	}
	delay := time.Second * time.Duration(1<<attempt)
	if delay > time.Hour {
		return time.Hour
	}
	return delay
}

func channelOutboxErrorCode(err error) string {
	var providerErr *channelapi.ProviderError
	if errors.As(err, &providerErr) && providerErr.Code != "" {
		return providerErr.Code
	}
	return "delivery_failed"
}

func channelOutboxErrorMessage(err error) string {
	if err == nil {
		return ""
	}
	message := err.Error()
	const maxLength = 1000
	if len(message) > maxLength {
		return message[:maxLength]
	}
	return message
}

func channelOutboxBool(config models.JSONB, key string) bool {
	value, _ := config[key].(bool)
	return value
}

func firstChannelOutboxValue(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func firstChannelOutboxTime(value, fallback time.Time) time.Time {
	if value.IsZero() {
		return fallback
	}
	return value
}
