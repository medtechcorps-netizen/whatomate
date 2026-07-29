package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/contactutil"
	"github.com/shridarpatil/whatomate/internal/models"
	appwebsocket "github.com/shridarpatil/whatomate/internal/websocket"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	defaultCareContinuityInterval      = 30 * time.Second
	defaultCareContinuityLease         = 5 * time.Minute
	defaultCareContinuityBatchSize     = 50
	defaultCareContinuitySweepInterval = 10 * time.Minute
	defaultCareContinuitySweepLimit    = 250
	defaultCarePackageLowThreshold     = 2
	defaultCarePackageExpiryWindow     = 14 * 24 * time.Hour
	careTaskSource                     = "care_automation"
	careTaskReminderKind               = "follow_up_task.reminder"
	careTaskReminderWebSocketType      = "follow_up_task_reminder"
)

var careContinuityOutboxEventTypes = []string{
	string(models.WebhookEventContactCreated),
	string(models.WebhookEventContactUpdated),
	string(models.WebhookEventContactMerged),
	string(models.WebhookEventMessageIncoming),
	string(models.WebhookEventCRMLeadCreated),
	string(models.WebhookEventCRMLeadUpdated),
	string(models.WebhookEventCRMStageMoved),
	string(models.WebhookEventTaskCreated),
	string(models.WebhookEventTaskCompleted),
	string(models.WebhookEventBookingCreated),
	string(models.WebhookEventBookingStatus),
	string(models.WebhookEventPackageSold),
	string(models.WebhookEventPackageLow),
	string(models.WebhookEventPackageExpiring),
	string(models.WebhookEventInvoiceCreated),
	string(models.WebhookEventInvoiceOverdue),
	string(models.WebhookEventInvoicePaid),
	string(models.WebhookEventPaymentRecorded),
}

type careReminderNotification struct {
	OrganizationID uuid.UUID
	TaskID         uuid.UUID
	ContactID      *uuid.UUID
	LeadID         *uuid.UUID
	BookingID      *uuid.UUID
	OwnerUserID    *uuid.UUID
	Title          string
	DueAt          *time.Time
}

type careResolvedEvent struct {
	EventType       string
	SourceType      string
	SourceID        *uuid.UUID
	ContactID       *uuid.UUID
	ActivityEventID *uuid.UUID
	InternalOnly    bool
	WebhookData     map[string]any
}

type careTaskInput struct {
	ContactID      uuid.UUID
	BookingID      *uuid.UUID
	Title          string
	Description    string
	Priority       models.FollowUpTaskPriority
	DueAt          time.Time
	IdempotencyKey string
	AutomationKind string
	Metadata       models.JSONB
}

// CareContinuityProcessor consumes durable customer lifecycle work and creates
// internal agent tasks. It never writes Message or OutboxJob records and
// therefore never automatically contacts a customer.
type CareContinuityProcessor struct {
	app           *App
	interval      time.Duration
	lease         time.Duration
	batchSize     int
	sweepInterval time.Duration
	sweepLimit    int
	workerID      string

	now             func() time.Time
	notifyReminder  func(careReminderNotification) error
	deliverWebhooks func(context.Context, uuid.UUID, string, map[string]any) error

	stopCh    chan struct{}
	stopOnce  sync.Once
	runMu     sync.Mutex
	sweepMu   sync.Mutex
	lastSweep time.Time
}

// NewCareContinuityProcessor creates the tenant-safe lifecycle processor.
func NewCareContinuityProcessor(app *App, interval time.Duration) *CareContinuityProcessor {
	if interval <= 0 {
		interval = defaultCareContinuityInterval
	}
	processor := &CareContinuityProcessor{
		app:           app,
		interval:      interval,
		lease:         defaultCareContinuityLease,
		batchSize:     defaultCareContinuityBatchSize,
		sweepInterval: defaultCareContinuitySweepInterval,
		sweepLimit:    defaultCareContinuitySweepLimit,
		workerID:      "care-continuity:" + uuid.NewString(),
		now: func() time.Time {
			return time.Now().UTC()
		},
		stopCh: make(chan struct{}),
	}
	processor.notifyReminder = processor.broadcastReminder
	processor.deliverWebhooks = processor.deliverConfiguredWebhooks
	return processor
}

// Start runs immediately and then polls until the context is cancelled or Stop
// is called. Processing errors are logged and retried through durable state.
func (p *CareContinuityProcessor) Start(ctx context.Context) {
	if p == nil || p.app == nil || p.app.DB == nil {
		return
	}
	p.app.Log.Info("Care continuity processor started", "interval", p.interval)
	if err := p.RunOnce(ctx); err != nil && ctx.Err() == nil {
		p.app.Log.Error("Care continuity run failed", "error", err)
	}

	timer := time.NewTimer(p.nextPollDelay())
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			p.app.Log.Info("Care continuity processor stopped by context")
			return
		case <-p.stopCh:
			p.app.Log.Info("Care continuity processor stopped")
			return
		case <-timer.C:
			if err := p.RunOnce(ctx); err != nil && ctx.Err() == nil {
				p.app.Log.Error("Care continuity run failed", "error", err)
			}
			timer.Reset(p.nextPollDelay())
		}
	}
}

func (p *CareContinuityProcessor) nextPollDelay() time.Duration {
	if p.interval <= time.Second {
		return p.interval
	}
	jitterWindow := p.interval / 10
	offsetRange := int64((2 * jitterWindow) + 1)
	offset := time.Duration(p.now().UnixNano()%offsetRange) - jitterWindow
	return p.interval + offset
}

// Stop is safe to call more than once.
func (p *CareContinuityProcessor) Stop() {
	if p == nil {
		return
	}
	p.stopOnce.Do(func() {
		close(p.stopCh)
	})
}

// RunOnce is the deterministic processing hook used by the loop and tests.
func (p *CareContinuityProcessor) RunOnce(ctx context.Context) error {
	if p == nil || p.app == nil || p.app.DB == nil {
		return errors.New("care continuity processor requires an app database")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	p.runMu.Lock()
	defer p.runMu.Unlock()

	organizationIDs, err := p.listOrganizations(ctx)
	if err != nil {
		return err
	}
	if len(organizationIDs) == 0 {
		return nil
	}

	var runErrors []error
	remaining := p.batchSize
	if remaining <= 0 {
		remaining = defaultCareContinuityBatchSize
	}

	// Round-robin claims prevent one busy tenant from starving every other
	// organization while keeping each individual run bounded.
	for remaining > 0 {
		madeProgress := false
		for _, organizationID := range organizationIDs {
			if remaining <= 0 || ctx.Err() != nil {
				break
			}

			job, claimErr := p.claimScheduledJob(ctx, organizationID)
			if claimErr != nil {
				runErrors = append(runErrors, claimErr)
			} else if job != nil {
				madeProgress = true
				remaining--
				if processErr := p.processScheduledJob(ctx, organizationID, job); processErr != nil {
					runErrors = append(runErrors, processErr)
				}
			}

			if remaining <= 0 || ctx.Err() != nil {
				break
			}
			event, claimErr := p.claimOutboxEvent(ctx, organizationID)
			if claimErr != nil {
				runErrors = append(runErrors, claimErr)
			} else if event != nil {
				madeProgress = true
				remaining--
				if processErr := p.processOutboxEvent(ctx, organizationID, event); processErr != nil {
					runErrors = append(runErrors, processErr)
				}
			}
		}
		if !madeProgress {
			break
		}
	}

	if ctx.Err() != nil {
		return errors.Join(append(runErrors, ctx.Err())...)
	}

	now := p.now().UTC()
	if p.beginSweep(now) {
		for _, organizationID := range organizationIDs {
			if ctx.Err() != nil {
				runErrors = append(runErrors, ctx.Err())
				break
			}
			if sweepErr := p.sweepOrganization(ctx, organizationID, now); sweepErr != nil {
				runErrors = append(
					runErrors,
					fmt.Errorf("sweep organization %s: %w", organizationID, sweepErr),
				)
			}
		}
	}
	return errors.Join(runErrors...)
}

func (p *CareContinuityProcessor) listOrganizations(ctx context.Context) ([]uuid.UUID, error) {
	var organizationIDs []uuid.UUID
	if err := p.app.rootApp().DB.WithContext(ctx).
		Model(&models.Organization{}).
		Order("id").
		Pluck("id", &organizationIDs).Error; err != nil {
		return nil, fmt.Errorf("list care continuity organizations: %w", err)
	}
	return organizationIDs, nil
}

func (p *CareContinuityProcessor) withTenantTransaction(
	ctx context.Context,
	organizationID uuid.UUID,
	fn func(*gorm.DB) error,
) error {
	return p.app.WithTenantApp(organizationID, func(scoped *App) error {
		return scoped.DB.WithContext(ctx).Transaction(fn)
	})
}

func (p *CareContinuityProcessor) withTenantCanonicalTransaction(
	ctx context.Context,
	organizationID uuid.UUID,
	fn func(*gorm.DB) error,
) error {
	var err error
	for attempt := 0; attempt < canonicalContactWriteAttempts; attempt++ {
		err = p.withTenantTransaction(ctx, organizationID, fn)
		if !isRetryableCanonicalContactWrite(err) {
			return err
		}
	}
	return err
}

func (p *CareContinuityProcessor) entitlementAllowed(
	tx *gorm.DB,
	organizationID uuid.UUID,
	key string,
) (bool, error) {
	scoped := p.app.rootApp().scopedApp(tx, organizationID)
	return scoped.HasProductEntitlement(uuid.Nil, organizationID, key)
}

func (p *CareContinuityProcessor) claimScheduledJob(
	ctx context.Context,
	organizationID uuid.UUID,
) (*models.ScheduledJob, error) {
	var claimed *models.ScheduledJob
	err := p.withTenantTransaction(ctx, organizationID, func(tx *gorm.DB) error {
		now := p.now().UTC()
		staleBefore := now.Add(-p.lease)
		if err := tx.Model(&models.ScheduledJob{}).
			Where(
				"organization_id = ? AND kind = ? AND run_at <= ? AND attempts >= max_attempts AND status IN ?",
				organizationID,
				careTaskReminderKind,
				now,
				[]models.ScheduledJobStatus{
					models.ScheduledJobStatusPending,
					models.ScheduledJobStatusProcessing,
				},
			).
			Where("status != ? OR locked_at IS NULL OR locked_at < ?", models.ScheduledJobStatusProcessing, staleBefore).
			Updates(map[string]any{
				"status":     models.ScheduledJobStatusFailed,
				"locked_at":  nil,
				"locked_by":  "",
				"last_error": "maximum attempts exhausted",
				"version":    gorm.Expr("version + 1"),
			}).Error; err != nil {
			return err
		}

		var candidate models.ScheduledJob
		query := tx.Clauses(clause.Locking{Strength: "UPDATE", Options: "SKIP LOCKED"}).
			Where(
				"organization_id = ? AND kind = ? AND run_at <= ? AND attempts < max_attempts",
				organizationID,
				careTaskReminderKind,
				now,
			).
			Where(
				"status = ? OR (status = ? AND (locked_at IS NULL OR locked_at < ?))",
				models.ScheduledJobStatusPending,
				models.ScheduledJobStatusProcessing,
				staleBefore,
			).
			Order("run_at, created_at").
			First(&candidate)
		if errors.Is(query.Error, gorm.ErrRecordNotFound) {
			return nil
		}
		if query.Error != nil {
			return query.Error
		}

		candidate.Attempts++
		if err := tx.Model(&models.ScheduledJob{}).
			Where("id = ? AND organization_id = ?", candidate.ID, organizationID).
			Updates(map[string]any{
				"status":     models.ScheduledJobStatusProcessing,
				"attempts":   candidate.Attempts,
				"locked_at":  now,
				"locked_by":  p.workerID,
				"last_error": "",
				"version":    gorm.Expr("version + 1"),
			}).Error; err != nil {
			return err
		}
		candidate.Status = models.ScheduledJobStatusProcessing
		candidate.LockedAt = &now
		candidate.LockedBy = p.workerID
		claimed = &candidate
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("claim scheduled job for %s: %w", organizationID, err)
	}
	return claimed, nil
}

func (p *CareContinuityProcessor) claimOutboxEvent(
	ctx context.Context,
	organizationID uuid.UUID,
) (*models.OutboxEvent, error) {
	var claimed *models.OutboxEvent
	err := p.withTenantTransaction(ctx, organizationID, func(tx *gorm.DB) error {
		now := p.now().UTC()
		staleBefore := now.Add(-p.lease)
		if err := tx.Model(&models.OutboxEvent{}).
			Where(
				"organization_id = ? AND event_type IN ? AND available_at <= ? AND attempts >= max_attempts AND status IN ?",
				organizationID,
				careContinuityOutboxEventTypes,
				now,
				[]models.OutboxEventStatus{
					models.OutboxEventStatusPending,
					models.OutboxEventStatusProcessing,
				},
			).
			Where("status != ? OR locked_at IS NULL OR locked_at < ?", models.OutboxEventStatusProcessing, staleBefore).
			Updates(map[string]any{
				"status":     models.OutboxEventStatusFailed,
				"locked_at":  nil,
				"locked_by":  "",
				"last_error": "maximum attempts exhausted",
				"version":    gorm.Expr("version + 1"),
			}).Error; err != nil {
			return err
		}

		var candidate models.OutboxEvent
		query := tx.Clauses(clause.Locking{Strength: "UPDATE", Options: "SKIP LOCKED"}).
			Where(
				"organization_id = ? AND event_type IN ? AND available_at <= ? AND attempts < max_attempts",
				organizationID,
				careContinuityOutboxEventTypes,
				now,
			).
			Where(
				"status = ? OR (status = ? AND (locked_at IS NULL OR locked_at < ?))",
				models.OutboxEventStatusPending,
				models.OutboxEventStatusProcessing,
				staleBefore,
			).
			Order("available_at, created_at").
			First(&candidate)
		if errors.Is(query.Error, gorm.ErrRecordNotFound) {
			return nil
		}
		if query.Error != nil {
			return query.Error
		}

		candidate.Attempts++
		if err := tx.Model(&models.OutboxEvent{}).
			Where("id = ? AND organization_id = ?", candidate.ID, organizationID).
			Updates(map[string]any{
				"status":     models.OutboxEventStatusProcessing,
				"attempts":   candidate.Attempts,
				"locked_at":  now,
				"locked_by":  p.workerID,
				"last_error": "",
				"version":    gorm.Expr("version + 1"),
			}).Error; err != nil {
			return err
		}
		candidate.Status = models.OutboxEventStatusProcessing
		candidate.LockedAt = &now
		candidate.LockedBy = p.workerID
		claimed = &candidate
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("claim outbox event for %s: %w", organizationID, err)
	}
	return claimed, nil
}

func (p *CareContinuityProcessor) processScheduledJob(
	ctx context.Context,
	organizationID uuid.UUID,
	job *models.ScheduledJob,
) error {
	var notification *careReminderNotification
	cancelled := false
	processErr := p.withTenantCanonicalTransaction(ctx, organizationID, func(tx *gorm.DB) error {
		var err error
		notification, cancelled, err = p.prepareTaskReminder(tx, organizationID, job)
		return err
	})
	if processErr != nil {
		return p.failScheduledJob(ctx, organizationID, job, processErr)
	}

	if notification != nil {
		if err := p.notifyReminder(*notification); err != nil {
			return p.failScheduledJob(ctx, organizationID, job, err)
		}
	}

	status := models.ScheduledJobStatusCompleted
	if cancelled {
		status = models.ScheduledJobStatusCancelled
	}
	if err := p.finishScheduledJob(ctx, organizationID, job.ID, status); err != nil {
		return fmt.Errorf("finish scheduled job %s: %w", job.ID, err)
	}
	return nil
}

func (p *CareContinuityProcessor) prepareTaskReminder(
	tx *gorm.DB,
	organizationID uuid.UUID,
	job *models.ScheduledJob,
) (*careReminderNotification, bool, error) {
	crmEnabled, err := p.entitlementAllowed(tx, organizationID, "crm.enabled")
	if err != nil {
		return nil, false, err
	}
	if !crmEnabled {
		return nil, true, nil
	}

	taskID := careJSONUUID(job.Payload, "task_id")
	if job.AggregateID != nil {
		taskID = job.AggregateID
	}
	if taskID == nil {
		return nil, true, nil
	}

	var task models.FollowUpTask
	err = tx.Where("id = ? AND organization_id = ?", *taskID, organizationID).First(&task).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, true, nil
	}
	if err != nil {
		return nil, false, err
	}

	expectedVersion, hasVersion := careJSONInt64(job.Payload, "task_version")
	if hasVersion && expectedVersion != task.Version {
		return nil, true, nil
	}
	if task.Status == models.FollowUpTaskStatusCompleted ||
		task.Status == models.FollowUpTaskStatusCancelled ||
		task.RemindAt == nil ||
		task.RemindAt.UTC().After(p.now().UTC()) {
		return nil, true, nil
	}

	return &careReminderNotification{
		OrganizationID: organizationID,
		TaskID:         task.ID,
		ContactID:      task.ContactID,
		LeadID:         task.LeadID,
		BookingID:      task.BookingID,
		OwnerUserID:    task.OwnerUserID,
		Title:          task.Title,
		DueAt:          task.DueAt,
	}, false, nil
}

func (p *CareContinuityProcessor) broadcastReminder(notification careReminderNotification) error {
	if p.app.WSHub == nil {
		return nil
	}
	message := appwebsocket.WSMessage{
		Type: careTaskReminderWebSocketType,
		Payload: map[string]any{
			"task_id":       notification.TaskID,
			"contact_id":    notification.ContactID,
			"lead_id":       notification.LeadID,
			"booking_id":    notification.BookingID,
			"title":         notification.Title,
			"due_at":        notification.DueAt,
			"owner_user_id": notification.OwnerUserID,
		},
	}
	if notification.OwnerUserID != nil {
		p.app.WSHub.BroadcastToUser(
			notification.OrganizationID,
			*notification.OwnerUserID,
			message,
		)
		return nil
	}
	p.app.WSHub.BroadcastToOrg(notification.OrganizationID, message)
	return nil
}

func (p *CareContinuityProcessor) finishScheduledJob(
	ctx context.Context,
	organizationID, jobID uuid.UUID,
	status models.ScheduledJobStatus,
) error {
	return p.withTenantTransaction(ctx, organizationID, func(tx *gorm.DB) error {
		now := p.now().UTC()
		result := tx.Model(&models.ScheduledJob{}).
			Where(
				"id = ? AND organization_id = ? AND status = ? AND locked_by = ?",
				jobID,
				organizationID,
				models.ScheduledJobStatusProcessing,
				p.workerID,
			).
			Updates(map[string]any{
				"status":       status,
				"completed_at": now,
				"locked_at":    nil,
				"locked_by":    "",
				"last_error":   "",
				"version":      gorm.Expr("version + 1"),
			})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return errors.New("scheduled job lease was lost")
		}
		return nil
	})
}

func (p *CareContinuityProcessor) failScheduledJob(
	ctx context.Context,
	organizationID uuid.UUID,
	job *models.ScheduledJob,
	cause error,
) error {
	updateErr := p.withTenantTransaction(ctx, organizationID, func(tx *gorm.DB) error {
		status := models.ScheduledJobStatusPending
		maxAttempts := job.MaxAttempts
		if maxAttempts <= 0 {
			maxAttempts = 5
		}
		if job.Attempts >= maxAttempts {
			status = models.ScheduledJobStatusFailed
		}
		updates := map[string]any{
			"status":     status,
			"locked_at":  nil,
			"locked_by":  "",
			"last_error": careErrorString(cause),
			"version":    gorm.Expr("version + 1"),
		}
		if status == models.ScheduledJobStatusPending {
			updates["run_at"] = p.now().UTC().Add(careRetryBackoff(job.Attempts))
		}
		result := tx.Model(&models.ScheduledJob{}).
			Where(
				"id = ? AND organization_id = ? AND status = ? AND locked_by = ?",
				job.ID,
				organizationID,
				models.ScheduledJobStatusProcessing,
				p.workerID,
			).
			Updates(updates)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return errors.New("scheduled job lease was lost while recording failure")
		}
		return nil
	})
	if updateErr != nil {
		return errors.Join(cause, updateErr)
	}
	return fmt.Errorf("scheduled job %s: %w", job.ID, cause)
}

func (p *CareContinuityProcessor) processOutboxEvent(
	ctx context.Context,
	organizationID uuid.UUID,
	event *models.OutboxEvent,
) error {
	var resolved careResolvedEvent
	processErr := p.withTenantTransaction(ctx, organizationID, func(tx *gorm.DB) error {
		// Serialize legacy task ownership with policy activation and receipt
		// fan-out. This lock is acquired before any contact/domain lock.
		if err := automationLockDispatchState(tx, organizationID); err != nil {
			return err
		}
		var err error
		resolved, err = p.resolveCareEvent(tx, organizationID, event)
		if err != nil {
			return err
		}
		return p.applyCareEvent(tx, organizationID, event, resolved)
	})
	if processErr != nil {
		return p.failOutboxEvent(ctx, organizationID, event, processErr)
	}

	// This deliberately does not use DispatchWebhook: that helper is
	// fire-and-forget and cannot acknowledge delivery. A non-2xx response keeps
	// this durable outbox row pending for bounded retry. Delivery is at-least-
	// once because a crash after a remote 2xx but before finishOutboxEvent can
	// replay the webhook.
	if !resolved.InternalOnly {
		if err := p.deliverWebhooks(
			ctx,
			organizationID,
			resolved.EventType,
			resolved.WebhookData,
		); err != nil {
			return p.failOutboxEvent(ctx, organizationID, event, err)
		}
	}

	if err := p.finishOutboxEvent(ctx, organizationID, event.ID); err != nil {
		return fmt.Errorf("finish outbox event %s: %w", event.ID, err)
	}
	return nil
}

func (p *CareContinuityProcessor) resolveCareEvent(
	tx *gorm.DB,
	organizationID uuid.UUID,
	event *models.OutboxEvent,
) (careResolvedEvent, error) {
	resolved := careResolvedEvent{
		EventType:   strings.TrimSpace(event.EventType),
		SourceType:  strings.TrimSpace(event.AggregateType),
		SourceID:    event.AggregateID,
		WebhookData: make(map[string]any, len(event.Payload)+6),
	}
	for key, value := range event.Payload {
		resolved.WebhookData[key] = value
	}
	// This top-level outbox provenance is written only by the server in the
	// same transaction as an automation task. Outbox payloads are not publicly
	// editable, so it remains authoritative even if a user later edits the
	// task's mutable presentation fields before delivery.
	resolved.InternalOnly = careJSONBool(event.Payload, "automation_internal_only")
	if value := careJSONString(event.Payload, "event_type"); value != "" {
		resolved.EventType = value
	}
	if value := careJSONString(event.Payload, "source_type"); value != "" {
		resolved.SourceType = value
	}
	if value := careJSONUUID(event.Payload, "source_id"); value != nil {
		resolved.SourceID = value
	}
	resolved.ContactID = careJSONUUID(event.Payload, "contact_id")
	resolved.ActivityEventID = careJSONUUID(event.Payload, "activity_event_id")

	activityID := resolved.ActivityEventID
	if activityID == nil && event.AggregateType == "customer_activity" {
		activityID = event.AggregateID
	}
	if activityID != nil {
		var activity models.CustomerActivityEvent
		err := tx.Where(
			"id = ? AND organization_id = ?",
			*activityID,
			organizationID,
		).First(&activity).Error
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return careResolvedEvent{}, err
		}
		if err == nil {
			resolved.ActivityEventID = &activity.ID
			resolved.EventType = string(activity.EventType)
			resolved.SourceType = activity.SourceObjectType
			resolved.SourceID = activity.SourceObjectID
			resolved.ContactID = &activity.ContactID
		}
	}

	resolved.WebhookData["outbox_event_id"] = event.ID.String()
	resolved.WebhookData["event_type"] = resolved.EventType
	resolved.WebhookData["source_type"] = resolved.SourceType
	resolved.WebhookData["source_id"] = careUUIDString(resolved.SourceID)
	resolved.WebhookData["contact_id"] = careUUIDString(resolved.ContactID)
	if resolved.ActivityEventID != nil {
		resolved.WebhookData["activity_event_id"] = resolved.ActivityEventID.String()
	}
	return resolved, nil
}

func (p *CareContinuityProcessor) applyCareEvent(
	tx *gorm.DB,
	organizationID uuid.UUID,
	event *models.OutboxEvent,
	resolved careResolvedEvent,
) error {
	if resolved.ActivityEventID != nil {
		var activity models.CustomerActivityEvent
		if err := tx.Where(
			"id = ? AND organization_id = ?",
			*resolved.ActivityEventID,
			organizationID,
		).First(&activity).Error; err != nil {
			if !errors.Is(err, gorm.ErrRecordNotFound) {
				return err
			}
		} else {
			owned, err := automationPolicyOwnsCareEvent(
				tx,
				organizationID,
				&activity,
				time.Time{},
			)
			if err != nil {
				return err
			}
			if owned {
				return nil
			}
		}
	}
	switch resolved.EventType {
	case string(models.WebhookEventBookingStatus):
		allowed, err := p.careTaskEntitlementsAllowed(
			tx,
			organizationID,
			"bookings.enabled",
		)
		if err != nil || !allowed {
			return err
		}
		return p.applyBookingCareEvent(tx, organizationID, event, resolved)
	case string(models.WebhookEventPackageLow):
		allowed, err := p.careTaskEntitlementsAllowed(
			tx,
			organizationID,
			"commerce.enabled",
		)
		if err != nil || !allowed {
			return err
		}
		if resolved.SourceID == nil {
			return nil
		}
		return p.ensurePackageLowTask(tx, organizationID, *resolved.SourceID, event.ID)
	case string(models.WebhookEventPackageExpiring):
		allowed, err := p.careTaskEntitlementsAllowed(
			tx,
			organizationID,
			"commerce.enabled",
		)
		if err != nil || !allowed {
			return err
		}
		if resolved.SourceID == nil {
			return nil
		}
		return p.ensurePackageExpiringTask(tx, organizationID, *resolved.SourceID, event.ID)
	case string(models.WebhookEventInvoiceOverdue):
		allowed, err := p.careTaskEntitlementsAllowed(
			tx,
			organizationID,
			"commerce.enabled",
		)
		if err != nil || !allowed {
			return err
		}
		if resolved.SourceID == nil {
			return nil
		}
		return p.ensureInvoiceOverdueTask(tx, organizationID, *resolved.SourceID, event.ID)
	default:
		return nil
	}
}

func (p *CareContinuityProcessor) careTaskEntitlementsAllowed(
	tx *gorm.DB,
	organizationID uuid.UUID,
	sourceEntitlement string,
) (bool, error) {
	crmEnabled, err := p.entitlementAllowed(tx, organizationID, "crm.enabled")
	if err != nil || !crmEnabled {
		return crmEnabled, err
	}
	sourceEnabled, err := p.entitlementAllowed(tx, organizationID, sourceEntitlement)
	if err != nil {
		return false, err
	}
	return sourceEnabled, nil
}

func (p *CareContinuityProcessor) applyBookingCareEvent(
	tx *gorm.DB,
	organizationID uuid.UUID,
	event *models.OutboxEvent,
	resolved careResolvedEvent,
) error {
	if resolved.SourceID == nil {
		return nil
	}
	var booking models.Booking
	err := tx.Preload("Event").
		Where("id = ? AND organization_id = ?", *resolved.SourceID, organizationID).
		First(&booking).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if booking.Status != models.BookingStatusCompleted &&
		booking.Status != models.BookingStatusNoShow &&
		booking.Status != models.BookingStatusCancelled {
		return nil
	}

	canonicalContact, err := contactutil.ResolveCanonicalContactForUpdate(
		tx,
		organizationID,
		booking.ContactID,
	)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	contactFamilyIDs, err := customerWorkspaceContactFamilyIDs(
		tx,
		organizationID,
		canonicalContact.ID,
	)
	if err != nil {
		return err
	}

	hasLaterBooking, err := careHasLaterBooking(
		tx,
		organizationID,
		contactFamilyIDs,
		&booking,
	)
	if err != nil {
		return err
	}
	if hasLaterBooking {
		return nil
	}

	now := p.now().UTC()
	priority := models.FollowUpTaskPriorityNormal
	dueAt := now
	title := "Rebook after completed appointment"
	description := "Review the completed booking and arrange the next appropriate follow-up."
	automationKind := "booking_completed"
	switch booking.Status {
	case models.BookingStatusCompleted:
		dueAt = now.Add(24 * time.Hour)
	case models.BookingStatusNoShow:
		title = "Recover no-show booking"
		description = "Review the missed booking and arrange a suitable next step."
		priority = models.FollowUpTaskPriorityHigh
		automationKind = "booking_no_show"
	case models.BookingStatusCancelled:
		title = "Follow up cancelled booking"
		description = "Review the cancellation and arrange a suitable next step if needed."
		automationKind = "booking_cancelled"
	}

	bookingID := booking.ID
	return p.ensureCareTask(tx, organizationID, careTaskInput{
		ContactID:   canonicalContact.ID,
		BookingID:   &bookingID,
		Title:       title,
		Description: description,
		Priority:    priority,
		DueAt:       dueAt,
		IdempotencyKey: fmt.Sprintf(
			"care:booking:%s:%s:v%d",
			booking.ID,
			booking.Status,
			booking.Version,
		),
		AutomationKind: automationKind,
		Metadata: models.JSONB{
			"booking_id":      booking.ID.String(),
			"booking_status":  string(booking.Status),
			"source_event_id": event.ID.String(),
		},
	})
}

func careHasLaterBooking(
	tx *gorm.DB,
	organizationID uuid.UUID,
	contactIDs []uuid.UUID,
	booking *models.Booking,
) (bool, error) {
	if booking.Event == nil || len(contactIDs) == 0 {
		return false, nil
	}
	var count int64
	err := tx.Table("bookings AS later").
		Joins(`
			JOIN booking_events AS later_event
			  ON later_event.id = later.event_id
			 AND later_event.organization_id = later.organization_id
			 AND later_event.deleted_at IS NULL
		`).
		Where(`
			later.organization_id = ?
			AND later.contact_id IN ?
			AND later.id != ?
			AND later.deleted_at IS NULL
			AND later.status IN ?
			AND later_event.starts_at > ?
		`,
			organizationID,
			contactIDs,
			booking.ID,
			[]models.BookingStatus{
				models.BookingStatusReserved,
				models.BookingStatusConfirmed,
				models.BookingStatusWaitlisted,
				models.BookingStatusCheckedIn,
				models.BookingStatusCompleted,
			},
			booking.Event.StartsAt,
		).
		Count(&count).Error
	return count > 0, err
}

func (p *CareContinuityProcessor) ensurePackageLowTask(
	tx *gorm.DB,
	organizationID, contactPackageID, sourceEventID uuid.UUID,
) error {
	var contactPackage models.ContactPackage
	err := tx.Where(
		"id = ? AND organization_id = ?",
		contactPackageID,
		organizationID,
	).First(&contactPackage).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if contactPackage.Status != models.ContactPackageStatusActive {
		return nil
	}

	var balance struct {
		FiniteCount int64
		Available   int64
	}
	if err := tx.Table("credit_balances AS cb").
		Select("COUNT(*) AS finite_count, COALESCE(SUM(cb.available), 0) AS available").
		Joins(`
			JOIN package_entitlements AS pe
			  ON pe.id = cb.package_entitlement_id
			 AND pe.organization_id = cb.organization_id
			 AND pe.deleted_at IS NULL
		`).
		Where(`
			cb.organization_id = ?
			AND cb.contact_package_id = ?
			AND cb.deleted_at IS NULL
			AND pe.is_unlimited = FALSE
		`, organizationID, contactPackage.ID).
		Scan(&balance).Error; err != nil {
		return err
	}
	if balance.FiniteCount == 0 || balance.Available > defaultCarePackageLowThreshold {
		return nil
	}

	canonical, err := contactutil.ResolveCanonicalContactForUpdate(
		tx,
		organizationID,
		contactPackage.ContactID,
	)
	if err != nil {
		return err
	}
	canonicalContactID := canonical.ID

	if sourceEventID == uuid.Nil {
		activity, err := recordCustomerActivity(tx, organizationID, customerActivityInput{
			ContactID:        canonicalContactID,
			EventType:        models.CustomerActivityPackageLow,
			Category:         models.CustomerActivityCategoryPackage,
			Title:            "Package balance low",
			Summary:          "Finite package credits require follow-up",
			ActorType:        models.CustomerActivityActorSystem,
			SourceObjectType: contactPackageAuditResource,
			SourceObjectID:   &contactPackage.ID,
			OccurredAt:       p.now().UTC(),
			Metadata: models.JSONB{
				"available_credits": balance.Available,
				"threshold":         defaultCarePackageLowThreshold,
				"policy_version":    "care-v1",
			},
			IdempotencyKey: fmt.Sprintf(
				"care-package-low:%s:%d",
				contactPackage.ID,
				defaultCarePackageLowThreshold,
			),
		})
		if err != nil {
			return err
		}
		owned, err := automationPolicyOwnsCareEvent(
			tx,
			organizationID,
			activity,
			time.Time{},
		)
		if err != nil {
			return err
		}
		if owned {
			return nil
		}
		sourceEventID = activity.ID
	}

	return p.ensureCareTask(tx, organizationID, careTaskInput{
		ContactID:   canonicalContactID,
		Title:       "Package credits running low",
		Description: "Review the remaining finite package credits and discuss continuity options.",
		Priority:    models.FollowUpTaskPriorityHigh,
		DueAt:       p.now().UTC(),
		IdempotencyKey: fmt.Sprintf(
			"care:package-low:%s:%d",
			contactPackage.ID,
			defaultCarePackageLowThreshold,
		),
		AutomationKind: "package_low",
		Metadata: models.JSONB{
			"contact_package_id": contactPackage.ID.String(),
			"available_credits":  balance.Available,
			"threshold":          defaultCarePackageLowThreshold,
			"source_event_id":    careOptionalUUIDString(sourceEventID),
		},
	})
}

func (p *CareContinuityProcessor) ensurePackageExpiringTask(
	tx *gorm.DB,
	organizationID, contactPackageID, sourceEventID uuid.UUID,
) error {
	var contactPackage models.ContactPackage
	err := tx.Where(
		"id = ? AND organization_id = ?",
		contactPackageID,
		organizationID,
	).First(&contactPackage).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	now := p.now().UTC()
	if contactPackage.Status != models.ContactPackageStatusActive ||
		contactPackage.ExpiresAt == nil ||
		!contactPackage.ExpiresAt.After(now) ||
		contactPackage.ExpiresAt.After(now.Add(defaultCarePackageExpiryWindow)) {
		return nil
	}

	expiresAt := contactPackage.ExpiresAt.UTC()
	var newerPackageCount int64
	if err := tx.Model(&models.ContactPackage{}).
		Where(`
			organization_id = ?
			AND contact_id = ?
			AND package_definition_id = ?
			AND id != ?
			AND status = ?
			AND expires_at IS NOT NULL
			AND expires_at > ?
		`,
			organizationID,
			contactPackage.ContactID,
			contactPackage.PackageDefinitionID,
			contactPackage.ID,
			models.ContactPackageStatusActive,
			expiresAt,
		).
		Count(&newerPackageCount).Error; err != nil {
		return err
	}
	if newerPackageCount > 0 {
		return nil
	}

	canonical, err := contactutil.ResolveCanonicalContactForUpdate(
		tx,
		organizationID,
		contactPackage.ContactID,
	)
	if err != nil {
		return err
	}
	canonicalContactID := canonical.ID

	if sourceEventID == uuid.Nil {
		activity, err := recordCustomerActivity(tx, organizationID, customerActivityInput{
			ContactID:        canonicalContactID,
			EventType:        models.CustomerActivityPackageExpiring,
			Category:         models.CustomerActivityCategoryPackage,
			Title:            "Package expiring",
			Summary:          "Active package is approaching its expiry date",
			ActorType:        models.CustomerActivityActorSystem,
			SourceObjectType: contactPackageAuditResource,
			SourceObjectID:   &contactPackage.ID,
			OccurredAt:       now,
			Metadata: models.JSONB{
				"expires_at":     expiresAt.Format(time.RFC3339),
				"policy_version": "care-v1",
			},
			IdempotencyKey: fmt.Sprintf(
				"care-package-expiring:%s:%d",
				contactPackage.ID,
				expiresAt.Unix(),
			),
		})
		if err != nil {
			return err
		}
		owned, err := automationPolicyOwnsCareEvent(
			tx,
			organizationID,
			activity,
			time.Time{},
		)
		if err != nil {
			return err
		}
		if owned {
			return nil
		}
		sourceEventID = activity.ID
	}

	return p.ensureCareTask(tx, organizationID, careTaskInput{
		ContactID:   canonicalContactID,
		Title:       "Package expiring soon",
		Description: "Review the active package before it expires and discuss continuity options.",
		Priority:    models.FollowUpTaskPriorityNormal,
		DueAt:       now,
		IdempotencyKey: fmt.Sprintf(
			"care:package-expiring:%s:%d",
			contactPackage.ID,
			expiresAt.Unix(),
		),
		AutomationKind: "package_expiring",
		Metadata: models.JSONB{
			"contact_package_id": contactPackage.ID.String(),
			"expires_at":         expiresAt.Format(time.RFC3339),
			"source_event_id":    careOptionalUUIDString(sourceEventID),
		},
	})
}

func (p *CareContinuityProcessor) ensureInvoiceOverdueTask(
	tx *gorm.DB,
	organizationID, invoiceID, sourceEventID uuid.UUID,
) error {
	var invoice models.CommerceInvoice
	err := tx.Where(
		"id = ? AND organization_id = ?",
		invoiceID,
		organizationID,
	).First(&invoice).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	now := p.now().UTC()
	if invoice.Status != models.CommerceInvoiceStatusOpen ||
		invoice.DueMinor <= 0 ||
		invoice.DueAt == nil ||
		!invoice.DueAt.Before(now) {
		return nil
	}

	dueAt := invoice.DueAt.UTC()
	canonical, err := contactutil.ResolveCanonicalContactForUpdate(
		tx,
		organizationID,
		invoice.ContactID,
	)
	if err != nil {
		return err
	}
	canonicalContactID := canonical.ID

	if sourceEventID == uuid.Nil {
		activity, err := recordCustomerActivity(tx, organizationID, customerActivityInput{
			ContactID:        canonicalContactID,
			EventType:        models.CustomerActivityInvoiceOverdue,
			Category:         models.CustomerActivityCategoryInvoice,
			Title:            "Invoice overdue",
			Summary:          invoice.InvoiceNumber,
			ActorType:        models.CustomerActivityActorSystem,
			SourceObjectType: commerceInvoiceAuditResource,
			SourceObjectID:   &invoice.ID,
			OccurredAt:       now,
			Metadata: models.JSONB{
				"due_at":         dueAt.Format(time.RFC3339),
				"due_minor":      invoice.DueMinor,
				"currency":       invoice.Currency,
				"policy_version": "care-v1",
			},
			IdempotencyKey: fmt.Sprintf(
				"care-invoice-overdue:%s:%d",
				invoice.ID,
				dueAt.Unix(),
			),
		})
		if err != nil {
			return err
		}
		owned, err := automationPolicyOwnsCareEvent(
			tx,
			organizationID,
			activity,
			time.Time{},
		)
		if err != nil {
			return err
		}
		if owned {
			return nil
		}
		sourceEventID = activity.ID
	}

	return p.ensureCareTask(tx, organizationID, careTaskInput{
		ContactID:   canonicalContactID,
		Title:       "Invoice overdue",
		Description: "Review the overdue invoice and arrange an appropriate internal follow-up.",
		Priority:    models.FollowUpTaskPriorityHigh,
		DueAt:       now,
		IdempotencyKey: fmt.Sprintf(
			"care:invoice-overdue:%s:%d",
			invoice.ID,
			dueAt.Unix(),
		),
		AutomationKind: "invoice_overdue",
		Metadata: models.JSONB{
			"invoice_id":      invoice.ID.String(),
			"invoice_number":  invoice.InvoiceNumber,
			"due_at":          dueAt.Format(time.RFC3339),
			"due_minor":       invoice.DueMinor,
			"currency":        invoice.Currency,
			"source_event_id": careOptionalUUIDString(sourceEventID),
		},
	})
}

func (p *CareContinuityProcessor) ensureCareTask(
	tx *gorm.DB,
	organizationID uuid.UUID,
	input careTaskInput,
) error {
	if input.ContactID == uuid.Nil || strings.TrimSpace(input.IdempotencyKey) == "" {
		return errors.New("care task requires contact and idempotency key")
	}
	ownerUserID, leadID, err := careTaskOwner(tx, organizationID, input.ContactID)
	if err != nil {
		return err
	}

	metadata := models.JSONB{
		"automation_kind":               input.AutomationKind,
		"policy_version":                "care-v1",
		"requires_contact_policy_check": true,
		"external_message_sent":         false,
	}
	for key, value := range input.Metadata {
		metadata[key] = value
	}

	task := models.FollowUpTask{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: organizationID,
		ContactID:      &input.ContactID,
		LeadID:         leadID,
		BookingID:      input.BookingID,
		Title:          strings.TrimSpace(input.Title),
		Description:    strings.TrimSpace(input.Description),
		Status:         models.FollowUpTaskStatusOpen,
		Priority:       input.Priority,
		OwnerUserID:    ownerUserID,
		DueAt:          &input.DueAt,
		Source:         careTaskSource,
		IdempotencyKey: input.IdempotencyKey,
		Metadata:       metadata,
		Version:        1,
	}
	result := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&task)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return nil
	}

	_, err = recordCustomerActivity(tx, organizationID, customerActivityInput{
		ContactID:        input.ContactID,
		LeadID:           leadID,
		EventType:        models.CustomerActivityTaskCreated,
		Category:         models.CustomerActivityCategoryTask,
		Title:            "Follow-up task created",
		Summary:          task.Title,
		ActorType:        models.CustomerActivityActorSystem,
		SourceObjectType: "follow_up_task",
		SourceObjectID:   &task.ID,
		OccurredAt:       p.now().UTC(),
		Metadata: models.JSONB{
			"task_id":               task.ID.String(),
			"automation_kind":       input.AutomationKind,
			"priority":              string(task.Priority),
			"due_at":                input.DueAt.UTC().Format(time.RFC3339),
			"external_message_sent": false,
		},
		IdempotencyKey: "care-task-created:" + task.ID.String(),
	})
	return err
}

func careTaskOwner(
	tx *gorm.DB,
	organizationID, contactID uuid.UUID,
) (*uuid.UUID, *uuid.UUID, error) {
	var lead models.CRMLead
	err := tx.Select("id", "owner_user_id").
		Where(
			"organization_id = ? AND contact_id = ? AND status = ?",
			organizationID,
			contactID,
			models.CRMLeadStatusOpen,
		).
		Order("(owner_user_id IS NULL), COALESCE(last_activity_at, created_at) DESC").
		First(&lead).Error
	var ownerUserID *uuid.UUID
	var leadID *uuid.UUID
	if err == nil {
		leadID = &lead.ID
		ownerUserID = lead.OwnerUserID
	} else if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil, err
	}

	if ownerUserID == nil {
		var contact models.Contact
		err := tx.Select("assigned_user_id").
			Where("id = ? AND organization_id = ?", contactID, organizationID).
			First(&contact).Error
		if err != nil {
			return nil, nil, err
		}
		ownerUserID = contact.AssignedUserID
	}
	return ownerUserID, leadID, nil
}

func (p *CareContinuityProcessor) deliverConfiguredWebhooks(
	ctx context.Context,
	organizationID uuid.UUID,
	eventType string,
	data map[string]any,
) error {
	var webhooks []models.Webhook
	if err := p.app.WithTenantApp(organizationID, func(scoped *App) error {
		return scoped.DB.WithContext(ctx).
			Where("organization_id = ? AND is_active = ?", organizationID, true).
			Find(&webhooks).Error
	}); err != nil {
		return fmt.Errorf("load configured webhooks: %w", err)
	}

	matching := make([]models.Webhook, 0)
	for _, webhook := range webhooks {
		if containsEvent(webhook.Events, eventType) {
			matching = append(matching, webhook)
		}
	}
	if len(matching) == 0 {
		return nil
	}
	if p.app.HTTPClient == nil {
		return errors.New("webhook delivery requires an HTTP client")
	}

	payload := OutboundWebhookPayload{
		Event:     eventType,
		Timestamp: p.now().UTC(),
		Data:      data,
	}
	jsonData, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal webhook payload: %w", err)
	}

	deliveryContext, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	var deliveryErrors []error
	for _, webhook := range matching {
		if err := p.app.sendWebhookRequest(deliveryContext, webhook, jsonData); err != nil {
			deliveryErrors = append(
				deliveryErrors,
				fmt.Errorf("webhook %s delivery: %w", webhook.ID, err),
			)
		}
	}
	return errors.Join(deliveryErrors...)
}

func (p *CareContinuityProcessor) finishOutboxEvent(
	ctx context.Context,
	organizationID, eventID uuid.UUID,
) error {
	return p.withTenantTransaction(ctx, organizationID, func(tx *gorm.DB) error {
		now := p.now().UTC()
		result := tx.Model(&models.OutboxEvent{}).
			Where(
				"id = ? AND organization_id = ? AND status = ? AND locked_by = ?",
				eventID,
				organizationID,
				models.OutboxEventStatusProcessing,
				p.workerID,
			).
			Updates(map[string]any{
				"status":       models.OutboxEventStatusPublished,
				"published_at": now,
				"locked_at":    nil,
				"locked_by":    "",
				"last_error":   "",
				"version":      gorm.Expr("version + 1"),
			})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return errors.New("outbox event lease was lost")
		}
		return nil
	})
}

func (p *CareContinuityProcessor) failOutboxEvent(
	ctx context.Context,
	organizationID uuid.UUID,
	event *models.OutboxEvent,
	cause error,
) error {
	updateErr := p.withTenantTransaction(ctx, organizationID, func(tx *gorm.DB) error {
		status := models.OutboxEventStatusPending
		maxAttempts := event.MaxAttempts
		if maxAttempts <= 0 {
			maxAttempts = 10
		}
		if event.Attempts >= maxAttempts {
			status = models.OutboxEventStatusFailed
		}
		updates := map[string]any{
			"status":     status,
			"locked_at":  nil,
			"locked_by":  "",
			"last_error": careErrorString(cause),
			"version":    gorm.Expr("version + 1"),
		}
		if status == models.OutboxEventStatusPending {
			updates["available_at"] = p.now().UTC().Add(careRetryBackoff(event.Attempts))
		}
		result := tx.Model(&models.OutboxEvent{}).
			Where(
				"id = ? AND organization_id = ? AND status = ? AND locked_by = ?",
				event.ID,
				organizationID,
				models.OutboxEventStatusProcessing,
				p.workerID,
			).
			Updates(updates)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return errors.New("outbox event lease was lost while recording failure")
		}
		return nil
	})
	if updateErr != nil {
		return errors.Join(cause, updateErr)
	}
	return fmt.Errorf("outbox event %s: %w", event.ID, cause)
}

func (p *CareContinuityProcessor) beginSweep(now time.Time) bool {
	p.sweepMu.Lock()
	defer p.sweepMu.Unlock()
	if !p.lastSweep.IsZero() && p.sweepInterval > 0 &&
		now.Sub(p.lastSweep) < p.sweepInterval {
		return false
	}
	p.lastSweep = now
	return true
}

func (p *CareContinuityProcessor) sweepOrganization(
	ctx context.Context,
	organizationID uuid.UUID,
	now time.Time,
) error {
	limit := p.sweepLimit
	if limit <= 0 {
		limit = defaultCareContinuitySweepLimit
	}

	var overdueInvoiceIDs []uuid.UUID
	var activePackageIDs []uuid.UUID
	if err := p.withTenantTransaction(ctx, organizationID, func(tx *gorm.DB) error {
		allowed, err := p.careTaskEntitlementsAllowed(
			tx,
			organizationID,
			"commerce.enabled",
		)
		if err != nil || !allowed {
			return err
		}

		if err := tx.Table("commerce_invoices AS ci").
			Where(`
				ci.organization_id = ?
				AND ci.deleted_at IS NULL
				AND ci.status = ?
				AND ci.due_minor > 0
				AND ci.due_at IS NOT NULL
				AND ci.due_at < ?
				AND NOT EXISTS (
					SELECT 1
					FROM follow_up_tasks AS ft
					WHERE ft.organization_id = ci.organization_id
					  AND ft.deleted_at IS NULL
					  AND ft.source = ?
					  AND ft.metadata->>'automation_kind' = 'invoice_overdue'
					  AND ft.metadata->>'invoice_id' = ci.id::text
				)
			`,
				organizationID,
				models.CommerceInvoiceStatusOpen,
				now,
				careTaskSource,
			).
			Order("ci.due_at, ci.id").
			Limit(limit).
			Pluck("ci.id", &overdueInvoiceIDs).Error; err != nil {
			return err
		}

		if err := tx.Table("contact_packages AS cp").
			Where(`
				cp.organization_id = ?
				AND cp.deleted_at IS NULL
				AND cp.status = ?
				AND (
					(
						cp.expires_at IS NOT NULL
						AND cp.expires_at > ?
						AND cp.expires_at <= ?
						AND NOT EXISTS (
							SELECT 1
							FROM follow_up_tasks AS expiry_task
							WHERE expiry_task.organization_id = cp.organization_id
							  AND expiry_task.deleted_at IS NULL
							  AND expiry_task.source = ?
							  AND expiry_task.metadata->>'automation_kind' = 'package_expiring'
							  AND expiry_task.metadata->>'contact_package_id' = cp.id::text
						)
					)
					OR
					(
						EXISTS (
							SELECT 1
							FROM credit_balances AS cb
							JOIN package_entitlements AS pe
							  ON pe.id = cb.package_entitlement_id
							 AND pe.organization_id = cb.organization_id
							 AND pe.deleted_at IS NULL
							WHERE cb.organization_id = cp.organization_id
							  AND cb.contact_package_id = cp.id
							  AND cb.deleted_at IS NULL
							  AND pe.is_unlimited = FALSE
							GROUP BY cb.contact_package_id
							HAVING COALESCE(SUM(cb.available), 0) <= ?
						)
						AND NOT EXISTS (
							SELECT 1
							FROM follow_up_tasks AS low_task
							WHERE low_task.organization_id = cp.organization_id
							  AND low_task.deleted_at IS NULL
							  AND low_task.source = ?
							  AND low_task.metadata->>'automation_kind' = 'package_low'
							  AND low_task.metadata->>'contact_package_id' = cp.id::text
						)
					)
				)
			`,
				organizationID,
				models.ContactPackageStatusActive,
				now,
				now.Add(defaultCarePackageExpiryWindow),
				careTaskSource,
				defaultCarePackageLowThreshold,
				careTaskSource,
			).
			Order("cp.id").
			Limit(limit).
			Pluck("cp.id", &activePackageIDs).Error; err != nil {
			return err
		}
		return nil
	}); err != nil {
		return err
	}

	// Each commercial source is processed in its own canonical-contact
	// transaction. This keeps the contact lock set bounded to one merge family
	// and uses the same deterministic retry behavior as interactive writers.
	for _, invoiceID := range overdueInvoiceIDs {
		if err := p.withTenantCanonicalTransaction(
			ctx,
			organizationID,
			func(tx *gorm.DB) error {
				if err := automationLockDispatchState(tx, organizationID); err != nil {
					return err
				}
				return p.ensureInvoiceOverdueTask(
					tx,
					organizationID,
					invoiceID,
					uuid.Nil,
				)
			},
		); err != nil {
			return err
		}
	}
	for _, contactPackageID := range activePackageIDs {
		if err := p.withTenantCanonicalTransaction(
			ctx,
			organizationID,
			func(tx *gorm.DB) error {
				if err := automationLockDispatchState(tx, organizationID); err != nil {
					return err
				}
				if err := p.ensurePackageLowTask(
					tx,
					organizationID,
					contactPackageID,
					uuid.Nil,
				); err != nil {
					return err
				}
				if err := p.ensurePackageExpiringTask(
					tx,
					organizationID,
					contactPackageID,
					uuid.Nil,
				); err != nil {
					return err
				}
				return nil
			},
		); err != nil {
			return err
		}
	}
	return nil
}

func careRetryBackoff(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	shift := attempt - 1
	if shift > 8 {
		shift = 8
	}
	backoff := 5 * time.Second * time.Duration(1<<shift)
	if backoff > 15*time.Minute {
		return 15 * time.Minute
	}
	return backoff
}

func careErrorString(err error) string {
	if err == nil {
		return ""
	}
	value := err.Error()
	if len(value) > 4000 {
		return value[:4000]
	}
	return value
}

func careJSONString(payload models.JSONB, key string) string {
	if payload == nil {
		return ""
	}
	switch value := payload[key].(type) {
	case string:
		return strings.TrimSpace(value)
	case fmt.Stringer:
		return strings.TrimSpace(value.String())
	default:
		return ""
	}
}

func careJSONUUID(payload models.JSONB, key string) *uuid.UUID {
	value := careJSONString(payload, key)
	if value == "" {
		return nil
	}
	parsed, err := uuid.Parse(value)
	if err != nil || parsed == uuid.Nil {
		return nil
	}
	return &parsed
}

func careJSONInt64(payload models.JSONB, key string) (int64, bool) {
	if payload == nil {
		return 0, false
	}
	switch value := payload[key].(type) {
	case int:
		return int64(value), true
	case int64:
		return value, true
	case float64:
		return int64(value), true
	case json.Number:
		parsed, err := value.Int64()
		return parsed, err == nil
	default:
		return 0, false
	}
}

func careJSONBool(payload models.JSONB, key string) bool {
	if payload == nil {
		return false
	}
	value, ok := payload[key].(bool)
	return ok && value
}

func careUUIDString(value *uuid.UUID) string {
	if value == nil {
		return ""
	}
	return value.String()
}

func careOptionalUUIDString(value uuid.UUID) string {
	if value == uuid.Nil {
		return ""
	}
	return value.String()
}
