package handlers

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/audit"
	"github.com/shridarpatil/whatomate/internal/contactutil"
	"github.com/shridarpatil/whatomate/internal/models"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	defaultAutomationProcessorInterval = 5 * time.Second
	defaultAutomationProcessorLease    = 5 * time.Minute
	defaultAutomationProcessorBatch    = 50
	automationCreateTaskJobKind        = "automation_policy.create_task"
	automationTaskSource               = "automation_policy"
)

// AutomationPolicyProcessor consumes the dedicated activity receipt stream and
// task action jobs. It never claims the shared customer webhook outbox.
type AutomationPolicyProcessor struct {
	app              *App
	interval         time.Duration
	lease            time.Duration
	batchSize        int
	workerID         string
	now              func() time.Time
	databaseNow      func(*gorm.DB) (time.Time, error)
	nextOrganization int

	runMu    sync.Mutex
	stopCh   chan struct{}
	stopOnce sync.Once
}

func NewAutomationPolicyProcessor(app *App, interval time.Duration) *AutomationPolicyProcessor {
	if interval <= 0 {
		interval = defaultAutomationProcessorInterval
	}
	return &AutomationPolicyProcessor{
		app: app, interval: interval, lease: defaultAutomationProcessorLease,
		batchSize: defaultAutomationProcessorBatch,
		workerID:  "automation-policy:" + uuid.NewString(),
		now: func() time.Time {
			return time.Now().UTC()
		},
		databaseNow: automationDatabaseNow,
		stopCh:      make(chan struct{}),
	}
}

func (p *AutomationPolicyProcessor) Start(ctx context.Context) {
	if p == nil || p.app == nil || p.app.DB == nil {
		return
	}
	p.app.Log.Info("Automation policy processor started", "interval", p.interval)
	if err := p.RunOnce(ctx); err != nil && ctx.Err() == nil {
		p.app.Log.Error("Automation policy processor run failed", "error", err)
	}
	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-p.stopCh:
			return
		case <-ticker.C:
			if err := p.RunOnce(ctx); err != nil && ctx.Err() == nil {
				p.app.Log.Error("Automation policy processor run failed", "error", err)
			}
		}
	}
}

func (p *AutomationPolicyProcessor) Stop() {
	if p != nil {
		p.stopOnce.Do(func() { close(p.stopCh) })
	}
}

func (p *AutomationPolicyProcessor) RunOnce(ctx context.Context) error {
	if p == nil || p.app == nil || p.app.DB == nil {
		return errors.New("automation policy processor requires an app database")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	p.runMu.Lock()
	defer p.runMu.Unlock()

	var organizationIDs []uuid.UUID
	if err := p.app.rootApp().DB.WithContext(ctx).
		Model(&models.Organization{}).Order("id").Pluck("id", &organizationIDs).Error; err != nil {
		return fmt.Errorf("list automation organizations: %w", err)
	}
	remaining := p.batchSize
	if remaining <= 0 {
		remaining = defaultAutomationProcessorBatch
	}
	var runErrors []error
	if len(organizationIDs) > 0 {
		start := p.nextOrganization % len(organizationIDs)
		rotated := append([]uuid.UUID{}, organizationIDs[start:]...)
		rotated = append(rotated, organizationIDs[:start]...)
		organizationIDs = rotated
		p.nextOrganization = (start + 1) % len(organizationIDs)
	}
	for remaining > 0 {
		progress := false
		for _, orgID := range organizationIDs {
			if remaining <= 0 || ctx.Err() != nil {
				break
			}
			receipt, claimErr := p.claimReceipt(ctx, orgID)
			if claimErr != nil {
				runErrors = append(runErrors, claimErr)
			} else if receipt != nil {
				progress = true
				remaining--
				if err := p.processReceipt(ctx, orgID, receipt); err != nil {
					runErrors = append(runErrors, err)
				}
			}
			if remaining <= 0 || ctx.Err() != nil {
				break
			}
			job, claimErr := p.claimActionJob(ctx, orgID)
			if claimErr != nil {
				runErrors = append(runErrors, claimErr)
			} else if job != nil {
				progress = true
				remaining--
				if err := p.processActionJob(ctx, orgID, job); err != nil {
					runErrors = append(runErrors, err)
				}
			}
		}
		if !progress {
			break
		}
	}
	if ctx.Err() != nil {
		runErrors = append(runErrors, ctx.Err())
	}
	return errors.Join(runErrors...)
}

func (p *AutomationPolicyProcessor) withTenantTransaction(
	ctx context.Context,
	orgID uuid.UUID,
	fn func(*gorm.DB) error,
) error {
	return p.app.WithTenantApp(orgID, func(scoped *App) error {
		return scoped.DB.WithContext(ctx).Transaction(fn)
	})
}

func (p *AutomationPolicyProcessor) withCanonicalTenantTransaction(
	ctx context.Context,
	orgID uuid.UUID,
	fn func(*gorm.DB) error,
) error {
	var err error
	for attempt := 0; attempt < canonicalContactWriteAttempts; attempt++ {
		err = p.withTenantTransaction(ctx, orgID, fn)
		if !isRetryableCanonicalContactWrite(err) {
			return err
		}
	}
	return err
}

func (p *AutomationPolicyProcessor) claimReceipt(
	ctx context.Context,
	orgID uuid.UUID,
) (*models.AutomationEventReceipt, error) {
	var claimed *models.AutomationEventReceipt
	err := p.withTenantTransaction(ctx, orgID, func(tx *gorm.DB) error {
		now, err := p.databaseNow(tx)
		if err != nil {
			return err
		}
		now = now.UTC()
		staleBefore := now.Add(-p.lease)
		if err := tx.Model(&models.AutomationEventReceipt{}).
			Where(
				"organization_id = ? AND available_at <= ? AND attempts >= max_attempts AND status IN ?",
				orgID, now,
				[]models.AutomationEventReceiptStatus{
					models.AutomationEventReceiptStatusPending,
					models.AutomationEventReceiptStatusProcessing,
				},
			).
			Where("status != ? OR locked_at IS NULL OR locked_at < ?", models.AutomationEventReceiptStatusProcessing, staleBefore).
			Updates(map[string]any{
				"status":     models.AutomationEventReceiptStatusFailed,
				"locked_at":  nil,
				"locked_by":  "",
				"last_error": "maximum attempts exhausted",
			}).Error; err != nil {
			return err
		}
		var candidate models.AutomationEventReceipt
		query := tx.Clauses(clause.Locking{Strength: "UPDATE", Options: "SKIP LOCKED"}).
			Where("organization_id = ? AND available_at <= ? AND attempts < max_attempts", orgID, now).
			Where(
				"status = ? OR (status = ? AND (locked_at IS NULL OR locked_at < ?))",
				models.AutomationEventReceiptStatusPending,
				models.AutomationEventReceiptStatusProcessing,
				staleBefore,
			).
			Order("available_at, ingested_at, id").
			First(&candidate)
		if errors.Is(query.Error, gorm.ErrRecordNotFound) {
			return nil
		}
		if query.Error != nil {
			return query.Error
		}
		token := p.workerID + ":" + uuid.NewString()
		result := tx.Model(&models.AutomationEventReceipt{}).
			Where("id = ? AND organization_id = ?", candidate.ID, orgID).
			Updates(map[string]any{
				"status":     models.AutomationEventReceiptStatusProcessing,
				"attempts":   candidate.Attempts + 1,
				"locked_at":  now,
				"locked_by":  token,
				"last_error": "",
			})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return errors.New("automation receipt claim was lost")
		}
		candidate.Status = models.AutomationEventReceiptStatusProcessing
		candidate.Attempts++
		candidate.LockedAt = &now
		candidate.LockedBy = token
		claimed = &candidate
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("claim automation receipt for %s: %w", orgID, err)
	}
	return claimed, nil
}

func (p *AutomationPolicyProcessor) processReceipt(
	ctx context.Context,
	orgID uuid.UUID,
	receipt *models.AutomationEventReceipt,
) error {
	err := p.withTenantTransaction(ctx, orgID, func(tx *gorm.DB) error {
		// Dispatch state is always the first domain lock. Activation, pause,
		// archive, and fan-out use this same order.
		if err := automationLockDispatchState(tx, orgID); err != nil {
			return err
		}
		var locked models.AutomationEventReceipt
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where(
				"id = ? AND organization_id = ? AND status = ? AND locked_by = ?",
				receipt.ID, orgID, models.AutomationEventReceiptStatusProcessing, receipt.LockedBy,
			).First(&locked).Error; err != nil {
			return err
		}
		var event models.CustomerActivityEvent
		if err := tx.Where("id = ? AND organization_id = ?", locked.ActivityEventID, orgID).
			First(&event).Error; err != nil {
			return err
		}
		canonicalContact, err := contactutil.ResolveCanonicalContact(tx, orgID, event.ContactID)
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return p.completeReceiptInTransaction(tx, orgID, &locked)
		}
		if err != nil {
			return err
		}

		type activationVersion struct {
			ActivationID uuid.UUID
			models.AutomationPolicyVersion
		}
		var candidates []activationVersion
		eventJSON := fmt.Sprintf("[\"%s\"]", string(event.EventType))
		if err := tx.Table("automation_policy_activations AS activation").
			Select("activation.id AS activation_id, version.*").
			Joins(`
				JOIN automation_policy_versions AS version
				  ON version.id = activation.policy_version_id
				 AND version.policy_id = activation.policy_id
				 AND version.organization_id = activation.organization_id
			`).
			Where(`
				activation.organization_id = ?
				AND activation.active_from <= ?
				AND (activation.active_until IS NULL OR ? < activation.active_until)
				AND version.trigger_event_types @> ?::jsonb
			`, orgID, locked.IngestedAt, locked.IngestedAt, eventJSON).
			Order("activation.policy_id, activation.active_from").
			Scan(&candidates).Error; err != nil {
			return err
		}

		for _, candidate := range candidates {
			graph, err := automationGraphFromJSONB(candidate.Graph)
			if err != nil {
				return err
			}
			evaluation, err := evaluateAutomationGraph(
				graph, &event, canonicalContact, locked.IngestedAt.UTC(),
			)
			if err != nil {
				return err
			}
			if err := p.createExecutionFromEvaluation(
				tx, orgID, &locked, &event, &candidate.AutomationPolicyVersion,
				candidate.ActivationID, canonicalContact.ID, evaluation,
			); err != nil {
				return err
			}
		}
		return p.completeReceiptInTransaction(tx, orgID, &locked)
	})
	if err != nil {
		return p.failReceipt(ctx, orgID, receipt, err)
	}
	return nil
}

func (p *AutomationPolicyProcessor) createExecutionFromEvaluation(
	tx *gorm.DB,
	orgID uuid.UUID,
	receipt *models.AutomationEventReceipt,
	event *models.CustomerActivityEvent,
	version *models.AutomationPolicyVersion,
	activationID, canonicalContactID uuid.UUID,
	evaluation automationEvaluation,
) error {
	return createAutomationExecutionFromEvaluation(
		tx,
		orgID,
		receipt,
		event,
		version,
		activationID,
		canonicalContactID,
		evaluation,
		p.now().UTC(),
	)
}

func createAutomationExecutionFromEvaluation(
	tx *gorm.DB,
	orgID uuid.UUID,
	receipt *models.AutomationEventReceipt,
	event *models.CustomerActivityEvent,
	version *models.AutomationPolicyVersion,
	activationID, canonicalContactID uuid.UUID,
	evaluation automationEvaluation,
	now time.Time,
) error {
	now = now.UTC()
	status := models.AutomationExecutionStatusProcessing
	completedAt := (*time.Time)(nil)
	if len(evaluation.Actions) == 0 {
		status = models.AutomationExecutionStatusSkipped
		completedAt = &now
	}
	execution := models.AutomationExecution{
		ID:                  uuid.New(),
		OrganizationID:      orgID,
		PolicyID:            version.PolicyID,
		PolicyVersionID:     version.ID,
		PolicyVersionNumber: version.Number,
		ActivityEventID:     event.ID,
		ContactID:           canonicalContactID,
		Status:              status,
		TriggeredAt:         receipt.IngestedAt.UTC(),
		StartedAt:           &now,
		CompletedAt:         completedAt,
		Context: models.JSONB{
			"activation_id":   activationID.String(),
			"receipt_id":      receipt.ID.String(),
			"source_event_id": event.ID.String(),
			"event_type":      string(event.EventType),
		},
		Result: models.JSONB{
			"planned_actions":       len(evaluation.Actions),
			"external_message_sent": false,
		},
	}
	create := tx.Clauses(clause.OnConflict{
		Columns: []clause.Column{
			{Name: "organization_id"},
			{Name: "policy_id"},
			{Name: "activity_event_id"},
		},
		DoNothing: true,
	}).Create(&execution)
	if create.Error != nil {
		return create.Error
	}
	if create.RowsAffected == 0 {
		var existing models.AutomationExecution
		if err := tx.Where(
			"organization_id = ? AND policy_id = ? AND activity_event_id = ?",
			orgID,
			version.PolicyID,
			event.ID,
		).First(&existing).Error; err != nil {
			return err
		}
		if existing.PolicyVersionID != version.ID ||
			existing.PolicyVersionNumber != version.Number ||
			existing.ContactID != canonicalContactID {
			return errors.New("automation execution idempotency collision")
		}
		return nil
	}

	actions := make(map[string]automationPlannedTask, len(evaluation.Actions))
	for _, action := range evaluation.Actions {
		actions[action.NodeID] = action
	}
	for _, evaluatedStep := range evaluation.Steps {
		idempotencyKey := fmt.Sprintf(
			"automation:%s:%s:%s",
			version.ID, event.ID, evaluatedStep.NodeID,
		)
		output := evaluatedStep.Output
		if output == nil {
			output = models.JSONB{}
		}
		var scheduledAt *time.Time
		if action, ok := actions[evaluatedStep.NodeID]; ok {
			value := action.ScheduledAt.UTC()
			scheduledAt = &value
			output["title"] = action.Title
			output["description"] = action.Description
			output["priority"] = string(action.Priority)
			output["owner"] = action.OwnerMode
			output["due_at"] = automationOptionalTimeString(action.DueAt)
			output["remind_at"] = automationOptionalTimeString(action.RemindAt)
		}
		step := models.AutomationExecutionStep{
			ID: uuid.New(), OrganizationID: orgID, ExecutionID: execution.ID,
			NodeID: evaluatedStep.NodeID, NodeType: evaluatedStep.NodeType,
			Status: evaluatedStep.Status, ScheduledAt: scheduledAt,
			Output: output, IdempotencyKey: idempotencyKey,
		}
		if evaluatedStep.Status == models.AutomationExecutionStepStatusCompleted ||
			evaluatedStep.Status == models.AutomationExecutionStepStatusSkipped {
			step.CompletedAt = &now
		}
		if err := tx.Create(&step).Error; err != nil {
			return err
		}
		if action, ok := actions[evaluatedStep.NodeID]; ok {
			job := models.ScheduledJob{
				BaseModel:      models.BaseModel{ID: uuid.New()},
				OrganizationID: orgID,
				Kind:           automationCreateTaskJobKind,
				AggregateType:  "automation_execution_step",
				AggregateID:    &step.ID,
				RunAt:          action.ScheduledAt.UTC(),
				Status:         models.ScheduledJobStatusPending,
				MaxAttempts:    5,
				IdempotencyKey: idempotencyKey,
				Payload: models.JSONB{
					"execution_id": execution.ID.String(),
					"step_id":      step.ID.String(),
					"node_id":      step.NodeID,
				},
				Version: 1,
			}
			if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&job).Error; err != nil {
				return err
			}
		}
	}
	return nil
}

func (p *AutomationPolicyProcessor) completeReceiptInTransaction(
	tx *gorm.DB,
	orgID uuid.UUID,
	receipt *models.AutomationEventReceipt,
) error {
	now := p.now().UTC()
	result := tx.Model(&models.AutomationEventReceipt{}).
		Where(
			"id = ? AND organization_id = ? AND status = ? AND locked_by = ?",
			receipt.ID, orgID, models.AutomationEventReceiptStatusProcessing, receipt.LockedBy,
		).
		Updates(map[string]any{
			"status":       models.AutomationEventReceiptStatusCompleted,
			"completed_at": now,
			"locked_at":    nil,
			"locked_by":    "",
			"last_error":   "",
		})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return errors.New("automation receipt lease was lost")
	}
	return nil
}

func (p *AutomationPolicyProcessor) failReceipt(
	ctx context.Context,
	orgID uuid.UUID,
	receipt *models.AutomationEventReceipt,
	cause error,
) error {
	updateErr := p.withTenantTransaction(ctx, orgID, func(tx *gorm.DB) error {
		status := models.AutomationEventReceiptStatusPending
		if receipt.Attempts >= receipt.MaxAttempts {
			status = models.AutomationEventReceiptStatusFailed
		}
		updates := map[string]any{
			"status": status, "locked_at": nil, "locked_by": "",
			"last_error": careErrorString(cause),
		}
		if status == models.AutomationEventReceiptStatusPending {
			updates["available_at"] = p.now().UTC().Add(careRetryBackoff(receipt.Attempts))
		}
		result := tx.Model(&models.AutomationEventReceipt{}).
			Where(
				"id = ? AND organization_id = ? AND status = ? AND locked_by = ?",
				receipt.ID, orgID, models.AutomationEventReceiptStatusProcessing, receipt.LockedBy,
			).Updates(updates)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return errors.New("automation receipt lease was lost while recording failure")
		}
		return nil
	})
	return errors.Join(fmt.Errorf("automation receipt %s: %w", receipt.ID, cause), updateErr)
}

func (p *AutomationPolicyProcessor) claimActionJob(
	ctx context.Context,
	orgID uuid.UUID,
) (*models.ScheduledJob, error) {
	var claimed *models.ScheduledJob
	err := p.withTenantTransaction(ctx, orgID, func(tx *gorm.DB) error {
		now, err := p.databaseNow(tx)
		if err != nil {
			return err
		}
		now = now.UTC()
		staleBefore := now.Add(-p.lease)
		if err := p.reapExhaustedActionJobsInTransaction(
			tx,
			orgID,
			now,
			staleBefore,
		); err != nil {
			return err
		}
		var candidate models.ScheduledJob
		query := tx.Clauses(clause.Locking{Strength: "UPDATE", Options: "SKIP LOCKED"}).
			Where(
				"organization_id = ? AND kind = ? AND run_at <= ? AND attempts < max_attempts",
				orgID, automationCreateTaskJobKind, now,
			).
			Where(
				"status = ? OR (status = ? AND (locked_at IS NULL OR locked_at < ?))",
				models.ScheduledJobStatusPending, models.ScheduledJobStatusProcessing, staleBefore,
			).
			Order("run_at, created_at, id").First(&candidate)
		if errors.Is(query.Error, gorm.ErrRecordNotFound) {
			return nil
		}
		if query.Error != nil {
			return query.Error
		}
		token := p.workerID + ":" + uuid.NewString()
		result := tx.Model(&models.ScheduledJob{}).
			Where("id = ? AND organization_id = ? AND kind = ?", candidate.ID, orgID, automationCreateTaskJobKind).
			Updates(map[string]any{
				"status": models.ScheduledJobStatusProcessing, "attempts": candidate.Attempts + 1,
				"locked_at": now, "locked_by": token, "last_error": "",
				"version": gorm.Expr("version + 1"),
			})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return errors.New("automation action job claim was lost")
		}
		candidate.Status = models.ScheduledJobStatusProcessing
		candidate.Attempts++
		candidate.LockedAt = &now
		candidate.LockedBy = token
		claimed = &candidate
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("claim automation action for %s: %w", orgID, err)
	}
	return claimed, nil
}

func (p *AutomationPolicyProcessor) reapExhaustedActionJobsInTransaction(
	tx *gorm.DB,
	orgID uuid.UUID,
	now, staleBefore time.Time,
) error {
	const reapLimit = 10
	for reaped := 0; reaped < reapLimit; reaped++ {
		var job models.ScheduledJob
		query := tx.Clauses(clause.Locking{Strength: "UPDATE", Options: "SKIP LOCKED"}).
			Where(
				"organization_id = ? AND kind = ? AND run_at <= ? AND attempts >= max_attempts",
				orgID,
				automationCreateTaskJobKind,
				now,
			).
			Where(
				"status = ? OR (status = ? AND (locked_at IS NULL OR locked_at < ?))",
				models.ScheduledJobStatusPending,
				models.ScheduledJobStatusProcessing,
				staleBefore,
			).
			Order("run_at, created_at, id").
			First(&job)
		if errors.Is(query.Error, gorm.ErrRecordNotFound) {
			return nil
		}
		if query.Error != nil {
			return query.Error
		}
		result := tx.Model(&models.ScheduledJob{}).
			Where(
				"id = ? AND organization_id = ? AND kind = ? AND status IN ?",
				job.ID,
				orgID,
				automationCreateTaskJobKind,
				[]models.ScheduledJobStatus{
					models.ScheduledJobStatusPending,
					models.ScheduledJobStatusProcessing,
				},
			).
			Updates(map[string]any{
				"status": models.ScheduledJobStatusFailed, "locked_at": nil,
				"locked_by": "", "last_error": "maximum attempts exhausted",
				"completed_at": now, "version": gorm.Expr("version + 1"),
			})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return errors.New("exhausted automation action job reap was lost")
		}
		if err := automationTerminalizeActionLink(
			tx,
			orgID,
			&job,
			now,
			"maximum attempts exhausted",
		); err != nil {
			return err
		}
	}
	return nil
}

func automationTerminalizeActionLink(
	tx *gorm.DB,
	orgID uuid.UUID,
	job *models.ScheduledJob,
	now time.Time,
	reason string,
) error {
	if job == nil {
		return nil
	}
	payloadStepID := careJSONUUID(job.Payload, "step_id")
	payloadExecutionID := careJSONUUID(job.Payload, "execution_id")
	var linkedStep models.AutomationExecutionStep
	var linkQuery *gorm.DB
	if job.AggregateType == "automation_execution_step" && job.AggregateID != nil {
		linkQuery = tx.Select("id", "execution_id", "status").
			Where("id = ? AND organization_id = ?", *job.AggregateID, orgID).
			First(&linkedStep)
	} else if payloadStepID != nil && payloadExecutionID != nil {
		linkQuery = tx.Select("id", "execution_id", "status").
			Where(
				"id = ? AND execution_id = ? AND organization_id = ?",
				*payloadStepID,
				*payloadExecutionID,
				orgID,
			).
			First(&linkedStep)
	}
	if linkQuery == nil || errors.Is(linkQuery.Error, gorm.ErrRecordNotFound) {
		return tx.Model(&models.ScheduledJob{}).
			Where("id = ? AND organization_id = ?", job.ID, orgID).
			Update(
				"last_error",
				reason+"; automation action linkage is invalid",
			).Error
	}
	if linkQuery.Error != nil {
		return linkQuery.Error
	}
	stepID := linkedStep.ID
	executionID := linkedStep.ExecutionID
	if (payloadStepID != nil && *payloadStepID != stepID) ||
		(payloadExecutionID != nil && *payloadExecutionID != executionID) {
		if err := tx.Model(&models.ScheduledJob{}).
			Where("id = ? AND organization_id = ?", job.ID, orgID).
			Update(
				"last_error",
				reason+"; payload linkage mismatch recovered from aggregate",
			).Error; err != nil {
			return err
		}
	}
	if stepID == uuid.Nil || executionID == uuid.Nil {
		return nil
	}
	stepResult := tx.Model(&models.AutomationExecutionStep{}).
		Where(
			"id = ? AND execution_id = ? AND organization_id = ? AND status IN ?",
			stepID,
			executionID,
			orgID,
			[]models.AutomationExecutionStepStatus{
				models.AutomationExecutionStepStatusPending,
				models.AutomationExecutionStepStatusProcessing,
			},
		).
		Updates(map[string]any{
			"status":       models.AutomationExecutionStepStatusFailed,
			"completed_at": now, "last_error": reason,
		})
	if stepResult.Error != nil {
		return stepResult.Error
	}
	if stepResult.RowsAffected == 0 {
		var existing models.AutomationExecutionStep
		if err := tx.Select("status").
			Where(
				"id = ? AND execution_id = ? AND organization_id = ?",
				stepID,
				executionID,
				orgID,
			).
			First(&existing).Error; err != nil {
			return err
		}
		if existing.Status != models.AutomationExecutionStepStatusFailed &&
			existing.Status != models.AutomationExecutionStepStatusCompleted &&
			existing.Status != models.AutomationExecutionStepStatusCancelled {
			return errors.New("exhausted automation action step was not terminalized")
		}
	}
	executionResult := tx.Model(&models.AutomationExecution{}).
		Where(
			"id = ? AND organization_id = ? AND EXISTS ("+
				"SELECT 1 FROM automation_execution_steps AS step "+
				"WHERE step.id = ? AND step.execution_id = automation_executions.id "+
				"AND step.organization_id = automation_executions.organization_id"+
				") AND status NOT IN ?",
			executionID,
			orgID,
			stepID,
			[]models.AutomationExecutionStatus{
				models.AutomationExecutionStatusCompleted,
				models.AutomationExecutionStatusCancelled,
			},
		).
		Updates(map[string]any{
			"status":       models.AutomationExecutionStatusFailed,
			"completed_at": now, "last_error": reason,
		})
	if executionResult.Error != nil {
		return executionResult.Error
	}
	if executionResult.RowsAffected == 0 {
		var existing models.AutomationExecution
		if err := tx.Select("status").
			Where("id = ? AND organization_id = ?", executionID, orgID).
			First(&existing).Error; err != nil {
			return err
		}
		if existing.Status != models.AutomationExecutionStatusFailed &&
			existing.Status != models.AutomationExecutionStatusCompleted &&
			existing.Status != models.AutomationExecutionStatusCancelled {
			return errors.New("exhausted automation execution was not terminalized")
		}
	}
	return nil
}

func (p *AutomationPolicyProcessor) processActionJob(
	ctx context.Context,
	orgID uuid.UUID,
	job *models.ScheduledJob,
) error {
	executionID := careJSONUUID(job.Payload, "execution_id")
	stepID := careJSONUUID(job.Payload, "step_id")
	if executionID == nil || stepID == nil ||
		job.AggregateType != "automation_execution_step" ||
		job.AggregateID == nil || *job.AggregateID != *stepID {
		return p.failActionJob(ctx, orgID, job, errors.New("automation action payload is invalid"))
	}
	var hint models.AutomationExecution
	err := p.withTenantTransaction(ctx, orgID, func(tx *gorm.DB) error {
		return tx.Where("id = ? AND organization_id = ?", *executionID, orgID).First(&hint).Error
	})
	if err != nil {
		return p.failActionJob(ctx, orgID, job, err)
	}

	processErr := p.withCanonicalTenantTransaction(ctx, orgID, func(tx *gorm.DB) error {
		canonical, err := contactutil.ResolveCanonicalContactForUpdate(tx, orgID, hint.ContactID)
		if err != nil {
			return err
		}
		var lockedJob models.ScheduledJob
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where(
				"id = ? AND organization_id = ? AND kind = ? AND status = ? AND locked_by = ?",
				job.ID, orgID, automationCreateTaskJobKind,
				models.ScheduledJobStatusProcessing, job.LockedBy,
			).First(&lockedJob).Error; err != nil {
			return err
		}
		var execution models.AutomationExecution
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ? AND organization_id = ?", *executionID, orgID).
			First(&execution).Error; err != nil {
			return err
		}
		var step models.AutomationExecutionStep
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ? AND execution_id = ? AND organization_id = ?", *stepID, execution.ID, orgID).
			First(&step).Error; err != nil {
			return err
		}
		if step.Status == models.AutomationExecutionStepStatusCompleted {
			return p.completeActionJobInTransaction(tx, orgID, &lockedJob)
		}
		allowed, err := p.app.rootApp().scopedApp(tx, orgID).
			HasProductEntitlement(uuid.Nil, orgID, "crm.enabled")
		if err != nil {
			return err
		}
		if !allowed {
			return errors.New("CRM entitlement is not active")
		}
		relevant, reason, err := automationActionStillRelevant(
			tx,
			orgID,
			&execution,
			canonical,
			p.now().UTC(),
		)
		if err != nil {
			return err
		}
		if !relevant {
			return p.skipAutomationTaskInTransaction(
				tx,
				orgID,
				&execution,
				&step,
				&lockedJob,
				reason,
			)
		}
		return p.createAutomationTaskInTransaction(
			tx, orgID, canonical, &execution, &step, &lockedJob,
		)
	})
	if processErr != nil {
		return p.failActionJob(ctx, orgID, job, processErr)
	}
	return nil
}

func automationActionStillRelevant(
	tx *gorm.DB,
	orgID uuid.UUID,
	execution *models.AutomationExecution,
	contact *models.Contact,
	now time.Time,
) (bool, string, error) {
	var event models.CustomerActivityEvent
	if err := tx.Where(
		"id = ? AND organization_id = ?",
		execution.ActivityEventID,
		orgID,
	).First(&event).Error; err != nil {
		return false, "", err
	}
	if event.SourceObjectID == nil {
		return true, "", nil
	}
	switch event.EventType {
	case models.CustomerActivityBookingStatus:
		var booking models.Booking
		err := tx.Preload("Event").Where(
			"id = ? AND organization_id = ?",
			*event.SourceObjectID,
			orgID,
		).First(&booking).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return false, "source_booking_missing", nil
		}
		if err != nil {
			return false, "", err
		}
		expected := careJSONString(event.Metadata, "to_status")
		if expected != "" && string(booking.Status) != expected {
			return false, "booking_status_changed", nil
		}
		switch booking.Status {
		case models.BookingStatusCompleted,
			models.BookingStatusNoShow,
			models.BookingStatusCancelled:
			contactIDs, err := customerWorkspaceContactFamilyIDs(tx, orgID, contact.ID)
			if err != nil {
				return false, "", err
			}
			hasLater, err := careHasLaterBooking(tx, orgID, contactIDs, &booking)
			if err != nil {
				return false, "", err
			}
			if hasLater {
				return false, "later_booking_exists", nil
			}
		}
	case models.CustomerActivityPackageLow:
		var contactPackage models.ContactPackage
		err := tx.Where(
			"id = ? AND organization_id = ?",
			*event.SourceObjectID,
			orgID,
		).First(&contactPackage).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return false, "source_package_missing", nil
		}
		if err != nil {
			return false, "", err
		}
		if contactPackage.Status != models.ContactPackageStatusActive {
			return false, "package_not_active", nil
		}
		var balance struct {
			FiniteCount int64
			Available   int64
		}
		if err := tx.Table("credit_balances AS balance").
			Select("COUNT(*) AS finite_count, COALESCE(SUM(balance.available), 0) AS available").
			Joins(`
				JOIN package_entitlements AS entitlement
				  ON entitlement.id = balance.package_entitlement_id
				 AND entitlement.organization_id = balance.organization_id
				 AND entitlement.deleted_at IS NULL
			`).
			Where(
				"balance.organization_id = ? AND balance.contact_package_id = ? "+
					"AND balance.deleted_at IS NULL AND entitlement.is_unlimited = FALSE",
				orgID,
				contactPackage.ID,
			).
			Scan(&balance).Error; err != nil {
			return false, "", err
		}
		if balance.FiniteCount == 0 || balance.Available > defaultCarePackageLowThreshold {
			return false, "package_balance_recovered", nil
		}
	case models.CustomerActivityPackageExpiring:
		var contactPackage models.ContactPackage
		err := tx.Where(
			"id = ? AND organization_id = ?",
			*event.SourceObjectID,
			orgID,
		).First(&contactPackage).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return false, "source_package_missing", nil
		}
		if err != nil {
			return false, "", err
		}
		if contactPackage.Status != models.ContactPackageStatusActive ||
			contactPackage.ExpiresAt == nil ||
			!contactPackage.ExpiresAt.After(now) ||
			contactPackage.ExpiresAt.After(now.Add(defaultCarePackageExpiryWindow)) {
			return false, "package_no_longer_expiring", nil
		}
		var newer int64
		if err := tx.Model(&models.ContactPackage{}).
			Where(
				"organization_id = ? AND contact_id = ? AND package_definition_id = ? "+
					"AND id != ? AND status = ? AND expires_at IS NOT NULL AND expires_at > ?",
				orgID,
				contactPackage.ContactID,
				contactPackage.PackageDefinitionID,
				contactPackage.ID,
				models.ContactPackageStatusActive,
				*contactPackage.ExpiresAt,
			).
			Count(&newer).Error; err != nil {
			return false, "", err
		}
		if newer > 0 {
			return false, "newer_package_exists", nil
		}
	case models.CustomerActivityInvoiceOverdue:
		var invoice models.CommerceInvoice
		err := tx.Where(
			"id = ? AND organization_id = ?",
			*event.SourceObjectID,
			orgID,
		).First(&invoice).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return false, "source_invoice_missing", nil
		}
		if err != nil {
			return false, "", err
		}
		if invoice.Status != models.CommerceInvoiceStatusOpen ||
			invoice.DueMinor <= 0 ||
			invoice.DueAt == nil ||
			!invoice.DueAt.Before(now) {
			return false, "invoice_no_longer_overdue", nil
		}
	}
	return true, "", nil
}

func (p *AutomationPolicyProcessor) skipAutomationTaskInTransaction(
	tx *gorm.DB,
	orgID uuid.UUID,
	execution *models.AutomationExecution,
	step *models.AutomationExecutionStep,
	job *models.ScheduledJob,
	reason string,
) error {
	now := p.now().UTC()
	output := step.Output
	if output == nil {
		output = models.JSONB{}
	}
	output["reason_code"] = reason
	result := tx.Model(&models.AutomationExecutionStep{}).
		Where(
			"id = ? AND execution_id = ? AND organization_id = ? AND status = ?",
			step.ID,
			execution.ID,
			orgID,
			models.AutomationExecutionStepStatusPending,
		).
		Updates(map[string]any{
			"status":     models.AutomationExecutionStepStatusSkipped,
			"started_at": now, "completed_at": now, "output": output,
		})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return errors.New("stale automation action step transition was lost")
	}
	if err := p.completeActionJobInTransaction(tx, orgID, job); err != nil {
		return err
	}
	executionResult := tx.Model(&models.AutomationExecution{}).
		Where(
			"id = ? AND organization_id = ? AND status NOT IN ?",
			execution.ID,
			orgID,
			[]models.AutomationExecutionStatus{
				models.AutomationExecutionStatusFailed,
				models.AutomationExecutionStatusCancelled,
			},
		).
		Updates(map[string]any{
			"status":       models.AutomationExecutionStatusCompleted,
			"completed_at": now, "last_error": "",
			"result": models.JSONB{
				"external_message_sent": false,
				"task_first":            false,
				"skipped_stale_action":  true,
				"reason_code":           reason,
			},
		})
	if executionResult.Error != nil {
		return executionResult.Error
	}
	if executionResult.RowsAffected != 1 {
		return errors.New("stale automation execution completion was lost")
	}
	return nil
}

func (p *AutomationPolicyProcessor) createAutomationTaskInTransaction(
	tx *gorm.DB,
	orgID uuid.UUID,
	contact *models.Contact,
	execution *models.AutomationExecution,
	step *models.AutomationExecutionStep,
	job *models.ScheduledJob,
) error {
	title := careJSONString(step.Output, "title")
	description := careJSONString(step.Output, "description")
	priority := models.FollowUpTaskPriority(careJSONString(step.Output, "priority"))
	ownerMode := careJSONString(step.Output, "owner")
	if title == "" || !automationValidTaskPriority(string(priority)) {
		return errors.New("automation action output is invalid")
	}
	var ownerUserID *uuid.UUID
	if ownerMode == "contact_owner" && contact.AssignedUserID != nil {
		var memberCount int64
		if err := tx.Table("user_organizations AS membership").
			Joins("JOIN users AS actor ON actor.id = membership.user_id AND actor.deleted_at IS NULL").
			Where(
				"membership.organization_id = ? AND membership.user_id = ? AND membership.deleted_at IS NULL AND actor.is_active = ?",
				orgID, *contact.AssignedUserID, true,
			).Count(&memberCount).Error; err != nil {
			return err
		}
		if memberCount == 1 {
			value := *contact.AssignedUserID
			ownerUserID = &value
		}
	}
	dueAt := automationOutputTime(step.Output, "due_at")
	remindAt := automationOutputTime(step.Output, "remind_at")
	if dueAt != nil && remindAt != nil && remindAt.After(*dueAt) {
		return errors.New("automation task reminder is after due time")
	}
	var leadID *uuid.UUID
	var lead models.CRMLead
	if err := tx.Select("id").Where(
		"organization_id = ? AND contact_id = ? AND status = ?",
		orgID, contact.ID, models.CRMLeadStatusOpen,
	).Order("COALESCE(last_activity_at, created_at) DESC").First(&lead).Error; err == nil {
		leadID = &lead.ID
	} else if !errors.Is(err, gorm.ErrRecordNotFound) {
		return err
	}
	metadata := models.JSONB{
		"automation_policy_id":          execution.PolicyID.String(),
		"automation_policy_version_id":  execution.PolicyVersionID.String(),
		"automation_policy_version":     execution.PolicyVersionNumber,
		"automation_execution_id":       execution.ID.String(),
		"automation_step_id":            step.ID.String(),
		"automation_node_id":            step.NodeID,
		"source_activity_event_id":      execution.ActivityEventID.String(),
		"requires_contact_policy_check": true,
		"external_message_sent":         false,
	}
	task := models.FollowUpTask{
		BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: orgID,
		ContactID: &contact.ID, LeadID: leadID, Title: title,
		Description: description, Status: models.FollowUpTaskStatusOpen,
		Priority: priority, OwnerUserID: ownerUserID, DueAt: dueAt, RemindAt: remindAt,
		Source: automationTaskSource, IdempotencyKey: step.IdempotencyKey,
		Metadata: metadata, Version: 1,
	}
	create := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&task)
	if create.Error != nil {
		return create.Error
	}
	if create.RowsAffected == 0 {
		if err := tx.Where(
			"organization_id = ? AND idempotency_key = ?",
			orgID, step.IdempotencyKey,
		).First(&task).Error; err != nil {
			return err
		}
		if task.Source != automationTaskSource || task.ContactID == nil || *task.ContactID != contact.ID {
			return errors.New("automation task idempotency key collision")
		}
		expectedLineage := map[string]string{
			"automation_policy_id":         execution.PolicyID.String(),
			"automation_policy_version_id": execution.PolicyVersionID.String(),
			"automation_execution_id":      execution.ID.String(),
			"automation_step_id":           step.ID.String(),
			"automation_node_id":           step.NodeID,
			"source_activity_event_id":     execution.ActivityEventID.String(),
		}
		for key, expected := range expectedLineage {
			if careJSONString(task.Metadata, key) != expected {
				return errors.New("automation task idempotency lineage collision")
			}
		}
		if version, ok := careJSONInt64(task.Metadata, "automation_policy_version"); !ok ||
			version != int64(execution.PolicyVersionNumber) {
			return errors.New("automation task idempotency version collision")
		}
		if !careJSONBool(task.Metadata, "requires_contact_policy_check") ||
			!careJSONExplicitFalseValue(task.Metadata, "external_message_sent") {
			return errors.New("automation task idempotency policy collision")
		}
	} else {
		now := p.now().UTC()
		if err := syncFollowUpTaskReminder(tx, &task, now); err != nil {
			return err
		}
		if err := touchProductCRMLead(tx, orgID, leadID, now); err != nil {
			return err
		}
		if _, err := recordCustomerActivity(tx, orgID, customerActivityInput{
			ContactID: contact.ID, LeadID: leadID,
			EventType: models.CustomerActivityTaskCreated,
			Category:  models.CustomerActivityCategoryTask,
			Title:     "Follow-up task created", Summary: task.Title,
			ActorType:        models.CustomerActivityActorSystem,
			SourceObjectType: "follow_up_task", SourceObjectID: &task.ID,
			OccurredAt: now,
			Metadata: models.JSONB{
				"task_id": task.ID.String(), "automation_execution_id": execution.ID.String(),
				"automation_policy_id":     execution.PolicyID.String(),
				"automation_node_id":       step.NodeID,
				"automation_internal_only": true,
				"external_message_sent":    false,
			},
			WebhookData: models.JSONB{
				"automation_internal_only": true,
				"automation_task_id":       task.ID.String(),
			},
			IdempotencyKey: "automation-task-created:" + step.IdempotencyKey,
		}); err != nil {
			return err
		}
		var version models.AutomationPolicyVersion
		if err := tx.Select("created_by_id").
			Where("id = ? AND policy_id = ? AND organization_id = ?", execution.PolicyVersionID, execution.PolicyID, orgID).
			First(&version).Error; err != nil {
			return err
		}
		actorID := uuid.Nil
		if version.CreatedByID != nil {
			actorID = *version.CreatedByID
		}
		if err := audit.LogAudit(
			tx, orgID, actorID, "Automation policy",
			productCRMTaskResource, task.ID, models.AuditActionCreated,
			nil, followUpTaskAuditSnapshot(&task),
			map[string]any{"field": "automation_execution_id", "new_value": execution.ID},
		); err != nil {
			return err
		}
	}
	now := p.now().UTC()
	stepOutput := step.Output
	stepOutput["task_id"] = task.ID.String()
	stepResult := tx.Model(&models.AutomationExecutionStep{}).
		Where(
			"id = ? AND execution_id = ? AND organization_id = ? AND status = ?",
			step.ID, execution.ID, orgID, models.AutomationExecutionStepStatusPending,
		).
		Updates(map[string]any{
			"status":     models.AutomationExecutionStepStatusCompleted,
			"started_at": now, "completed_at": now, "task_id": task.ID,
			"output": stepOutput, "last_error": "",
		})
	if stepResult.Error != nil {
		return stepResult.Error
	}
	if stepResult.RowsAffected != 1 {
		return errors.New("automation action step completion was lost")
	}
	if err := p.completeActionJobInTransaction(tx, orgID, job); err != nil {
		return err
	}
	var outstanding int64
	if err := tx.Model(&models.AutomationExecutionStep{}).
		Where(
			"organization_id = ? AND execution_id = ? AND status IN ?",
			orgID, execution.ID,
			[]models.AutomationExecutionStepStatus{
				models.AutomationExecutionStepStatusPending,
				models.AutomationExecutionStepStatusProcessing,
			},
		).Count(&outstanding).Error; err != nil {
		return err
	}
	if outstanding == 0 {
		var failed int64
		if err := tx.Model(&models.AutomationExecutionStep{}).
			Where(
				"organization_id = ? AND execution_id = ? AND status = ?",
				orgID, execution.ID, models.AutomationExecutionStepStatusFailed,
			).Count(&failed).Error; err != nil {
			return err
		}
		if failed > 0 {
			result := tx.Model(&models.AutomationExecution{}).
				Where("id = ? AND organization_id = ?", execution.ID, orgID).
				Updates(map[string]any{
					"status":       models.AutomationExecutionStatusFailed,
					"completed_at": now,
				})
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected != 1 {
				return errors.New("automation execution failure finalization was lost")
			}
		} else {
			// Failed and cancelled executions are terminal. A successful
			// sibling action must never erase a prior terminal outcome.
			result := tx.Model(&models.AutomationExecution{}).
				Where(
					"id = ? AND organization_id = ? AND status NOT IN ?",
					execution.ID,
					orgID,
					[]models.AutomationExecutionStatus{
						models.AutomationExecutionStatusFailed,
						models.AutomationExecutionStatusCancelled,
					},
				).
				Updates(map[string]any{
					"status":       models.AutomationExecutionStatusCompleted,
					"completed_at": now, "last_error": "",
					"result": models.JSONB{
						"external_message_sent": false,
						"task_first":            true,
					},
				})
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected != 1 {
				return errors.New("automation execution completion was lost")
			}
		}
	}
	return nil
}

func (p *AutomationPolicyProcessor) completeActionJobInTransaction(
	tx *gorm.DB,
	orgID uuid.UUID,
	job *models.ScheduledJob,
) error {
	now := p.now().UTC()
	result := tx.Model(&models.ScheduledJob{}).
		Where(
			"id = ? AND organization_id = ? AND kind = ? AND status = ? AND locked_by = ?",
			job.ID, orgID, automationCreateTaskJobKind,
			models.ScheduledJobStatusProcessing, job.LockedBy,
		).
		Updates(map[string]any{
			"status": models.ScheduledJobStatusCompleted, "completed_at": now,
			"locked_at": nil, "locked_by": "", "last_error": "",
			"version": gorm.Expr("version + 1"),
		})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return errors.New("automation action lease was lost")
	}
	return nil
}

func (p *AutomationPolicyProcessor) failActionJob(
	ctx context.Context,
	orgID uuid.UUID,
	job *models.ScheduledJob,
	cause error,
) error {
	updateErr := p.withTenantTransaction(ctx, orgID, func(tx *gorm.DB) error {
		status := models.ScheduledJobStatusPending
		if job.Attempts >= job.MaxAttempts {
			status = models.ScheduledJobStatusFailed
		}
		updates := map[string]any{
			"status": status, "locked_at": nil, "locked_by": "",
			"last_error": careErrorString(cause), "version": gorm.Expr("version + 1"),
		}
		if status == models.ScheduledJobStatusPending {
			updates["run_at"] = p.now().UTC().Add(careRetryBackoff(job.Attempts))
		}
		result := tx.Model(&models.ScheduledJob{}).
			Where(
				"id = ? AND organization_id = ? AND kind = ? AND status = ? AND locked_by = ?",
				job.ID, orgID, automationCreateTaskJobKind,
				models.ScheduledJobStatusProcessing, job.LockedBy,
			).Updates(updates)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return errors.New("automation action lease was lost while recording failure")
		}
		if status == models.ScheduledJobStatusFailed {
			return automationTerminalizeActionLink(
				tx,
				orgID,
				job,
				p.now().UTC(),
				careErrorString(cause),
			)
		}
		stepID := careJSONUUID(job.Payload, "step_id")
		executionID := careJSONUUID(job.Payload, "execution_id")
		validLink := stepID != nil && executionID != nil &&
			job.AggregateType == "automation_execution_step" &&
			job.AggregateID != nil && *job.AggregateID == *stepID
		if validLink {
			stepResult := tx.Model(&models.AutomationExecutionStep{}).
				Where(
					"id = ? AND execution_id = ? AND organization_id = ?",
					*stepID, *executionID, orgID,
				).
				Update("last_error", careErrorString(cause))
			if stepResult.Error != nil {
				return stepResult.Error
			}
			if stepResult.RowsAffected != 1 {
				return errors.New("automation action failure did not match its execution step")
			}
		}
		return nil
	})
	return errors.Join(fmt.Errorf("automation action %s: %w", job.ID, cause), updateErr)
}

func automationOptionalTimeString(value *time.Time) string {
	if value == nil {
		return ""
	}
	return value.UTC().Format(time.RFC3339Nano)
}

func automationOutputTime(output models.JSONB, key string) *time.Time {
	value := careJSONString(output, key)
	if value == "" {
		return nil
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return nil
	}
	parsed = parsed.UTC()
	return &parsed
}

func careJSONExplicitFalseValue(payload models.JSONB, key string) bool {
	if payload == nil {
		return false
	}
	value, ok := payload[key].(bool)
	return ok && !value
}

// automationPolicyOwnsCareEvent uses the same pure graph evaluator as the
// dispatcher. It is called by legacy care automation before creating a task.
func automationPolicyOwnsCareEvent(
	tx *gorm.DB,
	orgID uuid.UUID,
	event *models.CustomerActivityEvent,
	ingestedAt time.Time,
) (bool, error) {
	if tx == nil || event == nil || event.ID == uuid.Nil {
		return false, nil
	}
	// Pin the visual decision while legacy care holds the same tenant dispatch
	// lock used by receipt fan-out. Later contact edits cannot make care and the
	// visual worker evaluate opposite branches for the same immutable event.
	if err := automationLockDispatchState(tx, orgID); err != nil {
		return false, err
	}
	var executionCount int64
	if err := tx.Model(&models.AutomationExecution{}).
		Where(
			"organization_id = ? AND activity_event_id = ? AND status != ?",
			orgID, event.ID, models.AutomationExecutionStatusSkipped,
		).Count(&executionCount).Error; err != nil {
		return false, err
	}
	if executionCount > 0 {
		return true, nil
	}
	var receipt models.AutomationEventReceipt
	err := tx.Where(
		"organization_id = ? AND activity_event_id = ?",
		orgID, event.ID,
	).First(&receipt).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if receipt.IngestedAt.IsZero() {
		receipt.IngestedAt = ingestedAt.UTC()
	} else {
		ingestedAt = receipt.IngestedAt.UTC()
	}
	eventJSON := fmt.Sprintf("[\"%s\"]", string(event.EventType))
	type activationVersion struct {
		ActivationID uuid.UUID
		models.AutomationPolicyVersion
	}
	var versions []activationVersion
	if err := tx.Table("automation_policy_activations AS activation").
		Select("activation.id AS activation_id, version.*").
		Joins(`
			JOIN automation_policy_versions AS version
			  ON version.id = activation.policy_version_id
			 AND version.policy_id = activation.policy_id
			 AND version.organization_id = activation.organization_id
		`).
		Where(`
			activation.organization_id = ?
			AND activation.active_from <= ?
			AND (activation.active_until IS NULL OR ? < activation.active_until)
			AND version.trigger_event_types @> ?::jsonb
		`, orgID, ingestedAt, ingestedAt, eventJSON).
		Find(&versions).Error; err != nil {
		return false, err
	}
	if len(versions) == 0 {
		return false, nil
	}
	contact, err := contactutil.ResolveCanonicalContact(tx, orgID, event.ContactID)
	if err != nil {
		return false, err
	}
	now, err := automationDatabaseNow(tx)
	if err != nil {
		return false, err
	}
	for _, candidate := range versions {
		version := candidate.AutomationPolicyVersion
		graph, err := automationGraphFromJSONB(version.Graph)
		if err != nil {
			return false, err
		}
		evaluation, err := evaluateAutomationGraph(graph, event, contact, ingestedAt)
		if err != nil {
			return false, err
		}
		if err := createAutomationExecutionFromEvaluation(
			tx,
			orgID,
			&receipt,
			event,
			&version,
			candidate.ActivationID,
			contact.ID,
			evaluation,
			now,
		); err != nil {
			return false, err
		}
	}
	executionCount = 0
	if err := tx.Model(&models.AutomationExecution{}).
		Where(
			"organization_id = ? AND activity_event_id = ? AND status != ?",
			orgID, event.ID, models.AutomationExecutionStatusSkipped,
		).Count(&executionCount).Error; err != nil {
		return false, err
	}
	return executionCount > 0, nil
}
