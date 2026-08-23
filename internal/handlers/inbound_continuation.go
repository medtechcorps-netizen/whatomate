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
	"github.com/shridarpatil/whatomate/internal/database"
	"github.com/shridarpatil/whatomate/internal/models"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	inboundContinuationJobKind       = "inbound_message.continuation"
	inboundContinuationActionJobKind = "inbound_message.action"
	defaultInboundContinuationPoll   = 2 * time.Second
	defaultInboundContinuationLease  = 5 * time.Minute
	defaultInboundContinuationBatch  = 50
	defaultInboundContinuationMaxTry = 8

	inboundActionStatePreAttempt           = "pre_attempt"
	inboundActionStateResolved             = "resolved"
	inboundActionStateFailedBeforeDispatch = "failed_before_dispatch"
	inboundActionStateManualReview         = "manual_review"
)

// InboundContinuationProcessor resumes the provider/media/chatbot side of an
// inbound WhatsApp message after the webhook transaction has committed. Jobs
// live in scheduled_jobs so enqueue, claim, lease recovery, and retry remain
// durable across app restarts.
type InboundContinuationProcessor struct {
	app       *App
	interval  time.Duration
	lease     time.Duration
	batchSize int
	workerID  string
	now       func() time.Time
	process   func(context.Context, *App, *persistedIncomingMessage) error

	stopCh   chan struct{}
	stopOnce sync.Once
	runMu    sync.Mutex
}

type inboundContinuationExecution struct {
	MessageID   uuid.UUID
	WAMID       string
	actionScope string
	actionIndex int
}

// inboundContinuationActionClaim is an at-most-once execution token for an
// externally visible continuation effect. Its ScheduledJob row is committed
// through the root connection pool before the provider call starts. A
// pre_attempt row is deliberately never lease-reclaimed: after a process loss
// there is no safe way to know whether the remote system accepted the request.
type inboundContinuationActionClaim struct {
	ID      uuid.UUID
	Key     string
	Kind    string
	State   string
	Execute bool
	Result  models.JSONB
}

type inboundContinuationManualReviewError struct {
	ActionKey string
	Reason    string
}

func (e *inboundContinuationManualReviewError) Error() string {
	if e == nil {
		return "inbound continuation requires manual review"
	}
	reason := strings.TrimSpace(e.Reason)
	if reason == "" {
		reason = "remote action outcome is uncertain"
	}
	if strings.TrimSpace(e.ActionKey) == "" {
		return "inbound continuation requires manual review: " + reason
	}
	return fmt.Sprintf(
		"inbound continuation action %s requires manual review: %s",
		e.ActionKey,
		reason,
	)
}

func inboundContinuationRequiresManualReview(err error) bool {
	var target *inboundContinuationManualReviewError
	return errors.As(err, &target)
}

// NewInboundContinuationProcessor creates a tenant-safe continuation worker.
func NewInboundContinuationProcessor(
	app *App,
	interval time.Duration,
) *InboundContinuationProcessor {
	if interval <= 0 {
		interval = defaultInboundContinuationPoll
	}
	processor := &InboundContinuationProcessor{
		app:       app,
		interval:  interval,
		lease:     defaultInboundContinuationLease,
		batchSize: defaultInboundContinuationBatch,
		workerID:  "inbound-continuation:" + uuid.NewString(),
		now: func() time.Time {
			return time.Now().UTC()
		},
		stopCh: make(chan struct{}),
	}
	processor.process = func(
		ctx context.Context,
		scoped *App,
		work *persistedIncomingMessage,
	) error {
		return scoped.continuePersistedIncomingMessage(ctx, work)
	}
	return processor
}

// Start performs an immediate recovery pass, then polls until stopped.
func (p *InboundContinuationProcessor) Start(ctx context.Context) {
	if p == nil || p.app == nil || p.app.DB == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	p.app.Log.Info("Inbound continuation processor started", "interval", p.interval)
	if err := p.RunOnce(ctx); err != nil && ctx.Err() == nil {
		p.app.Log.Error("Inbound continuation recovery failed", "error", err)
	}

	timer := time.NewTimer(p.interval)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			p.app.Log.Info("Inbound continuation processor stopped by context")
			return
		case <-p.stopCh:
			p.app.Log.Info("Inbound continuation processor stopped")
			return
		case <-timer.C:
			if err := p.RunOnce(ctx); err != nil && ctx.Err() == nil {
				p.app.Log.Error("Inbound continuation run failed", "error", err)
			}
			timer.Reset(p.interval)
		}
	}
}

// Stop is safe to call more than once.
func (p *InboundContinuationProcessor) Stop() {
	if p == nil {
		return
	}
	p.stopOnce.Do(func() {
		close(p.stopCh)
	})
}

// RunOnce claims a bounded, fair batch across all tenants.
func (p *InboundContinuationProcessor) RunOnce(ctx context.Context) error {
	if p == nil || p.app == nil || p.app.DB == nil {
		return errors.New("inbound continuation processor requires an app database")
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

	remaining := p.batchSize
	if remaining <= 0 {
		remaining = defaultInboundContinuationBatch
	}
	var runErrors []error
	for remaining > 0 {
		madeProgress := false
		for _, organizationID := range organizationIDs {
			if remaining <= 0 || ctx.Err() != nil {
				break
			}
			job, claimErr := p.claim(ctx, organizationID, nil)
			if claimErr != nil {
				runErrors = append(runErrors, claimErr)
				continue
			}
			if job == nil {
				continue
			}
			madeProgress = true
			remaining--
			if processErr := p.processClaimed(ctx, organizationID, job); processErr != nil {
				runErrors = append(runErrors, processErr)
			}
		}
		if !madeProgress {
			break
		}
	}
	if ctx.Err() != nil {
		runErrors = append(runErrors, ctx.Err())
	}
	return errors.Join(runErrors...)
}

// ProcessMessage provides the low-latency post-ACK path. The same durable
// claim used by the recovery loop prevents concurrent duplicate execution.
func (p *InboundContinuationProcessor) ProcessMessage(
	ctx context.Context,
	organizationID, messageID uuid.UUID,
) error {
	if p == nil || p.app == nil || p.app.DB == nil {
		return errors.New("inbound continuation processor requires an app database")
	}
	if organizationID == uuid.Nil || messageID == uuid.Nil {
		return errors.New("inbound continuation tenant and message are required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	job, err := p.claim(ctx, organizationID, &messageID)
	if err != nil || job == nil {
		return err
	}
	return p.processClaimed(ctx, organizationID, job)
}

func (p *InboundContinuationProcessor) listOrganizations(
	ctx context.Context,
) ([]uuid.UUID, error) {
	var organizationIDs []uuid.UUID
	if err := p.app.rootApp().DB.WithContext(ctx).
		Scopes(database.ExcludePlatformComplianceOrganizations).
		Model(&models.Organization{}).
		Order("id").
		Pluck("id", &organizationIDs).Error; err != nil {
		return nil, fmt.Errorf("list inbound continuation organizations: %w", err)
	}
	return organizationIDs, nil
}

func (p *InboundContinuationProcessor) withTenantTransaction(
	ctx context.Context,
	organizationID uuid.UUID,
	fn func(*gorm.DB) error,
) error {
	return p.app.WithTenantApp(organizationID, func(scoped *App) error {
		return scoped.DB.WithContext(ctx).Transaction(fn)
	})
}

func (p *InboundContinuationProcessor) claim(
	ctx context.Context,
	organizationID uuid.UUID,
	messageID *uuid.UUID,
) (*models.ScheduledJob, error) {
	var claimed *models.ScheduledJob
	err := p.withTenantTransaction(ctx, organizationID, func(tx *gorm.DB) error {
		now := p.now().UTC()
		staleBefore := now.Add(-p.lease)

		terminalQuery := tx.Model(&models.ScheduledJob{}).
			Where(
				"organization_id = ? AND kind = ? AND run_at <= ? AND attempts >= max_attempts AND status IN ?",
				organizationID,
				inboundContinuationJobKind,
				now,
				[]models.ScheduledJobStatus{
					models.ScheduledJobStatusPending,
					models.ScheduledJobStatusProcessing,
				},
			).
			Where(
				"status != ? OR locked_at IS NULL OR locked_at < ?",
				models.ScheduledJobStatusProcessing,
				staleBefore,
			)
		if messageID != nil {
			terminalQuery = terminalQuery.Where("aggregate_id = ?", *messageID)
		}
		if err := terminalQuery.Updates(map[string]any{
			"status":     models.ScheduledJobStatusFailed,
			"locked_at":  nil,
			"locked_by":  "",
			"last_error": "maximum attempts exhausted",
			"version":    gorm.Expr("version + 1"),
		}).Error; err != nil {
			return err
		}

		query := tx.Clauses(clause.Locking{
			Strength: "UPDATE",
			Options:  "SKIP LOCKED",
		}).Where(
			"organization_id = ? AND kind = ? AND run_at <= ? AND attempts < max_attempts",
			organizationID,
			inboundContinuationJobKind,
			now,
		).Where(
			"status = ? OR (status = ? AND (locked_at IS NULL OR locked_at < ?))",
			models.ScheduledJobStatusPending,
			models.ScheduledJobStatusProcessing,
			staleBefore,
		)
		if messageID != nil {
			query = query.Where("aggregate_id = ?", *messageID)
		}

		var candidate models.ScheduledJob
		result := query.Order("run_at, created_at, id").First(&candidate)
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			return nil
		}
		if result.Error != nil {
			return result.Error
		}

		candidate.Attempts++
		update := tx.Model(&models.ScheduledJob{}).
			Where(
				"id = ? AND organization_id = ? AND kind = ?",
				candidate.ID,
				organizationID,
				inboundContinuationJobKind,
			).
			Updates(map[string]any{
				"status":     models.ScheduledJobStatusProcessing,
				"attempts":   candidate.Attempts,
				"locked_at":  now,
				"locked_by":  p.workerID,
				"last_error": "",
				"version":    gorm.Expr("version + 1"),
			})
		if update.Error != nil {
			return update.Error
		}
		if update.RowsAffected != 1 {
			return errors.New("inbound continuation claim was lost")
		}

		candidate.Status = models.ScheduledJobStatusProcessing
		candidate.LockedAt = &now
		candidate.LockedBy = p.workerID
		claimed = &candidate
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf(
			"claim inbound continuation for tenant %s: %w",
			organizationID,
			err,
		)
	}
	return claimed, nil
}

func (p *InboundContinuationProcessor) processClaimed(
	ctx context.Context,
	organizationID uuid.UUID,
	job *models.ScheduledJob,
) (resultErr error) {
	if job == nil {
		return nil
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			resultErr = p.fail(
				context.Background(),
				organizationID,
				job,
				fmt.Errorf("panic in inbound continuation: %v", recovered),
			)
		}
	}()

	processCtx, cancelProcess := context.WithCancel(ctx)
	defer cancelProcess()
	heartbeatDone := make(chan error, 1)
	go func() {
		heartbeatErr := p.maintainLease(
			processCtx,
			organizationID,
			job.ID,
		)
		if heartbeatErr != nil {
			cancelProcess()
		}
		heartbeatDone <- heartbeatErr
	}()

	var (
		work            *persistedIncomingMessage
		continuationErr error
	)
	scopeErr := p.app.WithTenantApp(organizationID, func(scoped *App) error {
		var loadErr error
		work, loadErr = loadInboundContinuationWork(scoped, organizationID, job)
		if loadErr != nil {
			return loadErr
		}
		if p.process == nil {
			return errors.New("inbound continuation processor is not configured")
		}
		executionApp := scoped.scopedApp(scoped.DB, organizationID)
		// executionApp carries per-job action state, but the surrounding scoped
		// App owns the transaction commit. Move every deferred realtime callback
		// to that owner on all exits; adoptAfterCommit drains the nested queue so
		// it cannot be delivered twice.
		defer scoped.adoptAfterCommit(executionApp)
		executionApp.inboundContinuation = &inboundContinuationExecution{
			MessageID: work.Persisted.ID,
			WAMID:     work.Message.ID,
		}
		alreadyCompleted, checkpointErr := executionApp.
			inboundContinuationAlreadyCompleted(work.Persisted.ID)
		if checkpointErr != nil {
			continuationErr = checkpointErr
			return nil
		}
		if alreadyCompleted {
			return nil
		}
		// Keep durable side-effect checkpoints even when processing reports a
		// retryable error. Returning the error from this callback would roll
		// back a successful provider send and its marker under RLS.
		continuationErr = p.process(processCtx, executionApp, work)
		if continuationErr == nil {
			continuationErr = executionApp.markInboundContinuationCompleted(
				work.Persisted.ID,
			)
		}
		return nil
	})
	cancelProcess()
	heartbeatErr := <-heartbeatDone

	processErr := scopeErr
	if processErr == nil {
		processErr = continuationErr
	}
	if processErr == nil {
		processErr = heartbeatErr
	}
	if processErr != nil {
		return p.fail(ctx, organizationID, job, processErr)
	}
	if err := p.complete(ctx, organizationID, job.ID); err != nil {
		return fmt.Errorf("complete inbound continuation %s: %w", job.ID, err)
	}
	return nil
}

func (p *InboundContinuationProcessor) maintainLease(
	ctx context.Context,
	organizationID, jobID uuid.UUID,
) error {
	heartbeatInterval := p.lease / 3
	if heartbeatInterval < 25*time.Millisecond {
		heartbeatInterval = 25 * time.Millisecond
	}
	if heartbeatInterval > 30*time.Second {
		heartbeatInterval = 30 * time.Second
	}

	timer := time.NewTimer(heartbeatInterval)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-timer.C:
			if err := p.renewLease(ctx, organizationID, jobID); err != nil {
				// Processing may finish while a heartbeat query is in flight.
				// Its context is then cancelled intentionally; do not turn that
				// normal shutdown race into a failed continuation.
				if ctx.Err() != nil {
					return nil
				}
				return err
			}
			timer.Reset(heartbeatInterval)
		}
	}
}

func (p *InboundContinuationProcessor) renewLease(
	ctx context.Context,
	organizationID, jobID uuid.UUID,
) error {
	return p.withTenantTransaction(ctx, organizationID, func(tx *gorm.DB) error {
		result := tx.Model(&models.ScheduledJob{}).
			Where(
				"id = ? AND organization_id = ? AND kind = ? AND status = ? AND locked_by = ?",
				jobID,
				organizationID,
				inboundContinuationJobKind,
				models.ScheduledJobStatusProcessing,
				p.workerID,
			).
			Update("locked_at", p.now().UTC())
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return errors.New("inbound continuation lease heartbeat was lost")
		}
		return nil
	})
}

func loadInboundContinuationWork(
	app *App,
	organizationID uuid.UUID,
	job *models.ScheduledJob,
) (*persistedIncomingMessage, error) {
	if app == nil || job == nil || job.AggregateID == nil {
		return nil, errors.New("inbound continuation message reference is missing")
	}
	if job.OrganizationID != organizationID || job.Kind != inboundContinuationJobKind {
		return nil, errors.New("inbound continuation tenant or kind mismatch")
	}

	phoneNumberID, _ := job.Payload["phone_number_id"].(string)
	rawMessage, exists := job.Payload["message"]
	if strings.TrimSpace(phoneNumberID) == "" || !exists {
		return nil, errors.New("inbound continuation payload is incomplete")
	}
	encodedMessage, err := json.Marshal(rawMessage)
	if err != nil {
		return nil, fmt.Errorf("encode inbound continuation message: %w", err)
	}
	var inbound IncomingTextMessage
	if err := json.Unmarshal(encodedMessage, &inbound); err != nil {
		return nil, fmt.Errorf("decode inbound continuation message: %w", err)
	}
	if strings.TrimSpace(inbound.ID) == "" {
		return nil, errors.New("inbound continuation WAMID is missing")
	}

	account, err := app.getWhatsAppAccountCached(phoneNumberID)
	if err != nil {
		return nil, fmt.Errorf("load inbound continuation account: %w", err)
	}
	if account.OrganizationID != organizationID {
		return nil, errors.New("inbound continuation account tenant mismatch")
	}

	var message models.Message
	if err := app.DB.Where(
		"id = ? AND organization_id = ? AND whats_app_account = ? AND whats_app_message_id = ? AND direction = ?",
		*job.AggregateID,
		organizationID,
		account.Name,
		inbound.ID,
		models.DirectionIncoming,
	).First(&message).Error; err != nil {
		return nil, fmt.Errorf("load inbound continuation message: %w", err)
	}

	contact, err := contactutil.ResolveCanonicalContact(
		app.DB,
		organizationID,
		message.ContactID,
	)
	if err != nil {
		return nil, fmt.Errorf("resolve inbound continuation contact: %w", err)
	}
	return &persistedIncomingMessage{
		OrganizationID: organizationID,
		PhoneNumberID:  phoneNumberID,
		Message:        inbound,
		Account:        *account,
		Contact:        *contact,
		Extracted:      app.extractMessageContentForPersistence(inbound),
		Persisted:      message,
	}, nil
}

func (p *InboundContinuationProcessor) complete(
	ctx context.Context,
	organizationID, jobID uuid.UUID,
) error {
	return p.withTenantTransaction(ctx, organizationID, func(tx *gorm.DB) error {
		now := p.now().UTC()
		result := tx.Model(&models.ScheduledJob{}).
			Where(
				"id = ? AND organization_id = ? AND kind = ? AND status = ? AND locked_by = ?",
				jobID,
				organizationID,
				inboundContinuationJobKind,
				models.ScheduledJobStatusProcessing,
				p.workerID,
			).
			Updates(map[string]any{
				"status":       models.ScheduledJobStatusCompleted,
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
			return errors.New("inbound continuation completion lost its lease")
		}
		return nil
	})
}

func (p *InboundContinuationProcessor) fail(
	ctx context.Context,
	organizationID uuid.UUID,
	job *models.ScheduledJob,
	cause error,
) error {
	if ctx == nil || ctx.Err() != nil {
		ctx = context.Background()
	}
	updateErr := p.withTenantTransaction(ctx, organizationID, func(tx *gorm.DB) error {
		status := models.ScheduledJobStatusPending
		maxAttempts := job.MaxAttempts
		if maxAttempts <= 0 {
			maxAttempts = defaultInboundContinuationMaxTry
		}
		manualReview := inboundContinuationRequiresManualReview(cause)
		if manualReview || job.Attempts >= maxAttempts {
			status = models.ScheduledJobStatusFailed
		}
		updates := map[string]any{
			"status":     status,
			"locked_at":  nil,
			"locked_by":  "",
			"last_error": inboundContinuationError(cause),
			"version":    gorm.Expr("version + 1"),
		}
		if manualReview {
			payload := cloneInboundContinuationJSONB(job.Payload)
			payload["manual_review_required"] = true
			payload["manual_review_recorded_at"] = p.now().UTC().Format(
				time.RFC3339Nano,
			)
			updates["payload"] = payload
		}
		if status == models.ScheduledJobStatusPending {
			updates["run_at"] = p.now().UTC().Add(
				inboundContinuationBackoff(job.Attempts),
			)
		}
		result := tx.Model(&models.ScheduledJob{}).
			Where(
				"id = ? AND organization_id = ? AND kind = ? AND status = ? AND locked_by = ?",
				job.ID,
				organizationID,
				inboundContinuationJobKind,
				models.ScheduledJobStatusProcessing,
				p.workerID,
			).
			Updates(updates)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return errors.New("inbound continuation failure update lost its lease")
		}
		return nil
	})
	if updateErr != nil {
		return errors.Join(cause, updateErr)
	}
	return fmt.Errorf("inbound continuation %s: %w", job.ID, cause)
}

func inboundContinuationBackoff(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	if attempt > 8 {
		attempt = 8
	}
	delay := time.Second * time.Duration(1<<(attempt-1))
	if delay > 5*time.Minute {
		return 5 * time.Minute
	}
	return delay
}

func inboundContinuationError(err error) string {
	if err == nil {
		return ""
	}
	const maxLength = 2000
	value := err.Error()
	if len(value) > maxLength {
		return value[:maxLength]
	}
	return value
}

func cloneInboundContinuationJSONB(source models.JSONB) models.JSONB {
	clone := make(models.JSONB, len(source)+1)
	for key, value := range source {
		clone[key] = value
	}
	return clone
}

func (a *App) runInboundContinuationSend(
	kind string,
	payload any,
	send func() (*models.Message, error),
) error {
	if send == nil {
		return errors.New("chatbot send callback is required")
	}

	claim, err := a.claimInboundContinuationAction(
		context.Background(),
		kind,
		payload,
	)
	if err != nil {
		return err
	}
	if claim != nil && !claim.Execute {
		return a.recoverResolvedInboundContinuationSend(claim)
	}

	message, err := send()
	if err != nil {
		if claim != nil && claim.Key != "" {
			if message == nil || message.ID == uuid.Nil {
				resolveErr := a.resolveInboundContinuationAction(
					context.Background(),
					claim,
					inboundActionStateFailedBeforeDispatch,
					nil,
				)
				return errors.Join(err, resolveErr)
			}
			resolveErr := a.resolveInboundContinuationAction(
				context.Background(),
				claim,
				inboundActionStateManualReview,
				models.JSONB{"outgoing_message_id": message.ID.String()},
			)
			return errors.Join(
				&inboundContinuationManualReviewError{
					ActionKey: claim.Key,
					Reason:    "provider dispatch may have started before the send failed",
				},
				resolveErr,
			)
		}
		return err
	}
	if message == nil || message.ID == uuid.Nil {
		missingMessageErr := errors.New(
			"chatbot send did not return a persisted message",
		)
		if claim != nil && claim.Key != "" {
			resolveErr := a.resolveInboundContinuationAction(
				context.Background(),
				claim,
				inboundActionStateFailedBeforeDispatch,
				nil,
			)
			return errors.Join(missingMessageErr, resolveErr)
		}
		return missingMessageErr
	}

	if claim != nil && claim.Key != "" {
		// The metadata marker is useful for operators, but the separately
		// committed action ledger is the replay authority. Keep the marker in
		// the current tenant transaction so it commits with the Message.
		if markerErr := a.markInboundContinuationAction(
			message.ID,
			claim.Key,
		); markerErr != nil {
			a.Log.Error(
				"Failed to annotate chatbot response with inbound action key",
				"error", markerErr,
				"message_id", message.ID,
				"action_key", claim.Key,
			)
		}
	}

	verifyErr := a.verifySynchronousChatbotSend(message.ID)
	if verifyErr != nil {
		if claim == nil || claim.Key == "" {
			return verifyErr
		}
		resolveErr := a.resolveInboundContinuationAction(
			context.Background(),
			claim,
			inboundActionStateManualReview,
			models.JSONB{"outgoing_message_id": message.ID.String()},
		)
		return errors.Join(
			&inboundContinuationManualReviewError{
				ActionKey: claim.Key,
				Reason:    "provider attempt did not reach a confirmed sent state",
			},
			resolveErr,
		)
	}

	if claim == nil || claim.Key == "" {
		return nil
	}
	if resolveErr := a.resolveInboundContinuationAction(
		context.Background(),
		claim,
		inboundActionStateResolved,
		models.JSONB{"outgoing_message_id": message.ID.String()},
	); resolveErr != nil {
		return errors.Join(
			&inboundContinuationManualReviewError{
				ActionKey: claim.Key,
				Reason:    "provider send succeeded but its durable action result is uncertain",
			},
			resolveErr,
		)
	}
	return nil
}

func (a *App) nextInboundContinuationActionKey(
	kind string,
	payload any,
) (string, error) {
	execution := a.inboundContinuation
	if execution == nil {
		return "", nil
	}
	if execution.MessageID == uuid.Nil {
		return "", errors.New("inbound action message identity is missing")
	}
	kind = strings.TrimSpace(kind)
	if kind == "" {
		return "", errors.New("inbound action kind is required")
	}
	// Provider payloads can contain customer content or credentials and are
	// intentionally excluded from both the key and the ledger. Graph callers
	// set actionScope to a stable flow/node identity; legacy paths use a stable
	// execution-local ordinal.
	_ = payload
	scope := strings.TrimSpace(execution.actionScope)
	if scope == "" {
		scope = "continuation"
	}
	index := execution.actionIndex
	execution.actionIndex++
	fingerprint := fmt.Sprintf(
		"%s:%d:%s",
		scope,
		index,
		kind,
	)
	return "inbound-action:" +
		uuid.NewSHA1(execution.MessageID, []byte(fingerprint)).String(), nil
}

func (a *App) pushInboundContinuationActionScope(scope string) func() {
	execution := a.inboundContinuation
	if execution == nil {
		return func() {}
	}
	previousScope := execution.actionScope
	previousIndex := execution.actionIndex
	execution.actionScope = strings.TrimSpace(scope)
	execution.actionIndex = 0
	return func() {
		execution.actionScope = previousScope
		execution.actionIndex = previousIndex
	}
}

func (a *App) claimInboundContinuationAction(
	ctx context.Context,
	kind string,
	payload any,
) (*inboundContinuationActionClaim, error) {
	actionKey, err := a.nextInboundContinuationActionKey(kind, payload)
	if err != nil {
		return nil, err
	}
	if actionKey == "" {
		return &inboundContinuationActionClaim{
			Kind:    strings.TrimSpace(kind),
			Execute: true,
		}, nil
	}

	execution := a.inboundContinuation
	organizationID := a.inboundContinuationOrganizationID()
	if execution == nil ||
		execution.MessageID == uuid.Nil ||
		organizationID == uuid.Nil {
		return nil, errors.New("inbound action tenant identity is incomplete")
	}
	if ctx == nil {
		ctx = context.Background()
	}

	now := time.Now().UTC()
	var claimed *inboundContinuationActionClaim
	err = a.withIndependentInboundContinuationAction(
		ctx,
		organizationID,
		func(tx *gorm.DB) error {
			actionPayload := models.JSONB{
				"inbound_message_id": execution.MessageID.String(),
				"effect_kind":        strings.TrimSpace(kind),
				"state":              inboundActionStatePreAttempt,
				"claimed_at":         now.Format(time.RFC3339Nano),
			}
			candidate := models.ScheduledJob{
				BaseModel:      models.BaseModel{ID: uuid.New()},
				OrganizationID: organizationID,
				Kind:           inboundContinuationActionJobKind,
				AggregateType:  "inbound_message_action",
				AggregateID:    &execution.MessageID,
				RunAt:          now,
				Status:         models.ScheduledJobStatusProcessing,
				Attempts:       1,
				MaxAttempts:    1,
				IdempotencyKey: actionKey,
				Payload:        actionPayload,
				LockedAt:       &now,
				LockedBy:       "at-most-once",
				Version:        1,
			}
			create := tx.Clauses(clause.OnConflict{DoNothing: true}).
				Create(&candidate)
			if create.Error != nil {
				return create.Error
			}
			if create.RowsAffected == 1 {
				claimed = &inboundContinuationActionClaim{
					ID:      candidate.ID,
					Key:     actionKey,
					Kind:    strings.TrimSpace(kind),
					State:   inboundActionStatePreAttempt,
					Execute: true,
				}
				return nil
			}

			var existing models.ScheduledJob
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
				Where(
					"organization_id = ? AND idempotency_key = ?",
					organizationID,
					actionKey,
				).
				First(&existing).Error; err != nil {
				return err
			}
			if existing.Kind != inboundContinuationActionJobKind ||
				existing.AggregateID == nil ||
				*existing.AggregateID != execution.MessageID {
				return errors.New("inbound action idempotency key collision")
			}

			state, _ := existing.Payload["state"].(string)
			state = strings.TrimSpace(state)
			if state == "" &&
				existing.Status == models.ScheduledJobStatusCompleted {
				state = inboundActionStateResolved
			}
			result := inboundContinuationActionResult(existing.Payload)
			switch state {
			case inboundActionStateResolved:
				claimed = &inboundContinuationActionClaim{
					ID:      existing.ID,
					Key:     actionKey,
					Kind:    strings.TrimSpace(kind),
					State:   state,
					Execute: false,
					Result:  result,
				}
				return nil
			case inboundActionStateFailedBeforeDispatch:
				retryPayload := cloneInboundContinuationJSONB(existing.Payload)
				delete(retryPayload, "result")
				delete(retryPayload, "resolved_at")
				delete(retryPayload, "manual_review_required")
				retryPayload["state"] = inboundActionStatePreAttempt
				retryPayload["claimed_at"] = now.Format(time.RFC3339Nano)
				update := tx.Model(&models.ScheduledJob{}).
					Where(
						"id = ? AND organization_id = ?",
						existing.ID,
						organizationID,
					).
					Updates(map[string]any{
						"status":       models.ScheduledJobStatusProcessing,
						"attempts":     gorm.Expr("attempts + 1"),
						"payload":      retryPayload,
						"locked_at":    now,
						"locked_by":    "at-most-once",
						"completed_at": nil,
						"last_error":   "",
						"version":      gorm.Expr("version + 1"),
					})
				if update.Error != nil {
					return update.Error
				}
				if update.RowsAffected != 1 {
					return errors.New("inbound action retry claim was lost")
				}
				claimed = &inboundContinuationActionClaim{
					ID:      existing.ID,
					Key:     actionKey,
					Kind:    strings.TrimSpace(kind),
					State:   inboundActionStatePreAttempt,
					Execute: true,
				}
				return nil
			default:
				return &inboundContinuationManualReviewError{
					ActionKey: actionKey,
					Reason:    "a committed pre-attempt has no safely replayable result",
				}
			}
		},
	)
	if err != nil {
		return nil, fmt.Errorf("claim inbound continuation action: %w", err)
	}
	return claimed, nil
}

func (a *App) withIndependentInboundContinuationAction(
	ctx context.Context,
	organizationID uuid.UUID,
	fn func(*gorm.DB) error,
) error {
	if a == nil || a.rootApp() == nil || fn == nil {
		return errors.New("independent inbound action transaction is required")
	}
	root := a.rootApp()
	return root.WithTenantApp(organizationID, func(scoped *App) error {
		return scoped.DB.WithContext(ctx).Transaction(fn)
	})
}

func (a *App) resolveInboundContinuationAction(
	ctx context.Context,
	claim *inboundContinuationActionClaim,
	state string,
	result models.JSONB,
) error {
	if claim == nil || claim.Key == "" {
		return nil
	}
	organizationID := a.inboundContinuationOrganizationID()
	if organizationID == uuid.Nil {
		return errors.New("resolve inbound action: tenant identity is missing")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	now := time.Now().UTC()

	return a.withIndependentInboundContinuationAction(
		ctx,
		organizationID,
		func(tx *gorm.DB) error {
			var stored models.ScheduledJob
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
				Where(
					"id = ? AND organization_id = ? AND kind = ?",
					claim.ID,
					organizationID,
					inboundContinuationActionJobKind,
				).
				First(&stored).Error; err != nil {
				return err
			}
			currentState, _ := stored.Payload["state"].(string)
			if strings.TrimSpace(currentState) != inboundActionStatePreAttempt ||
				stored.Status != models.ScheduledJobStatusProcessing {
				return fmt.Errorf(
					"inbound action %s is no longer an active pre-attempt",
					claim.Key,
				)
			}

			updatedPayload := cloneInboundContinuationJSONB(stored.Payload)
			updatedPayload["state"] = state
			updatedPayload["resolved_at"] = now.Format(time.RFC3339Nano)
			if len(result) > 0 {
				updatedPayload["result"] = result
			} else {
				delete(updatedPayload, "result")
			}

			status := models.ScheduledJobStatusCompleted
			lastError := ""
			completedAt := any(now)
			switch state {
			case inboundActionStateResolved:
			case inboundActionStateFailedBeforeDispatch:
				status = models.ScheduledJobStatusFailed
				lastError = "action failed before provider dispatch"
				completedAt = nil
			case inboundActionStateManualReview:
				status = models.ScheduledJobStatusFailed
				lastError = "remote action outcome requires manual review"
				completedAt = nil
				updatedPayload["manual_review_required"] = true
			default:
				return fmt.Errorf("unsupported inbound action resolution %q", state)
			}

			update := tx.Model(&models.ScheduledJob{}).
				Where(
					"id = ? AND organization_id = ? AND kind = ? AND status = ?",
					claim.ID,
					organizationID,
					inboundContinuationActionJobKind,
					models.ScheduledJobStatusProcessing,
				).
				Updates(map[string]any{
					"status":       status,
					"payload":      updatedPayload,
					"locked_at":    nil,
					"locked_by":    "",
					"completed_at": completedAt,
					"last_error":   lastError,
					"version":      gorm.Expr("version + 1"),
				})
			if update.Error != nil {
				return update.Error
			}
			if update.RowsAffected != 1 {
				return errors.New("inbound action resolution was lost")
			}
			claim.State = state
			claim.Result = result
			claim.Execute = false
			return nil
		},
	)
}

func inboundContinuationActionResult(payload models.JSONB) models.JSONB {
	raw, exists := payload["result"]
	if !exists || raw == nil {
		return nil
	}
	switch value := raw.(type) {
	case models.JSONB:
		return cloneInboundContinuationJSONB(value)
	case map[string]any:
		result := make(models.JSONB, len(value))
		for key, item := range value {
			result[key] = item
		}
		return result
	default:
		return nil
	}
}

// runInboundContinuationCapturedAction executes a remote computation once and
// stores only its caller-selected, non-request result fields. It is used for
// provider calls whose output is needed by later deterministic continuation
// steps (for example, an AI answer followed by a WhatsApp send).
func (a *App) runInboundContinuationCapturedAction(
	kind string,
	action func() models.JSONB,
) (models.JSONB, error) {
	if action == nil {
		return nil, errors.New("captured inbound action callback is required")
	}
	claim, err := a.claimInboundContinuationAction(
		context.Background(),
		kind,
		nil,
	)
	if err != nil {
		return nil, err
	}
	if !claim.Execute {
		return claim.Result, nil
	}

	result := action()
	if result == nil {
		result = models.JSONB{}
	}
	if resolveErr := a.resolveInboundContinuationAction(
		context.Background(),
		claim,
		inboundActionStateResolved,
		result,
	); resolveErr != nil {
		return nil, errors.Join(
			&inboundContinuationManualReviewError{
				ActionKey: claim.Key,
				Reason:    "remote computation ran but its durable result is uncertain",
			},
			resolveErr,
		)
	}
	return result, nil
}

func (a *App) recoverResolvedInboundContinuationSend(
	claim *inboundContinuationActionClaim,
) error {
	if claim == nil || claim.Key == "" {
		return nil
	}
	messageIDText, _ := claim.Result["outgoing_message_id"].(string)
	messageID, err := uuid.Parse(strings.TrimSpace(messageIDText))
	if err != nil {
		return &inboundContinuationManualReviewError{
			ActionKey: claim.Key,
			Reason:    "resolved send has no valid persisted message reference",
		}
	}

	var message models.Message
	err = a.DB.Unscoped().
		Select("id", "status", "error_message").
		Where(
			"id = ? AND organization_id = ? AND direction = ?",
			messageID,
			a.inboundContinuationOrganizationID(),
			models.DirectionOutgoing,
		).
		First(&message).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return &inboundContinuationManualReviewError{
			ActionKey: claim.Key,
			Reason:    "provider send was resolved but its local message transaction did not commit",
		}
	}
	if err != nil {
		return fmt.Errorf("recover resolved inbound send: %w", err)
	}
	switch message.Status {
	case models.MessageStatusSent,
		models.MessageStatusDelivered,
		models.MessageStatusRead:
		return nil
	default:
		return &inboundContinuationManualReviewError{
			ActionKey: claim.Key,
			Reason:    "resolved send does not have a confirmed local sent state",
		}
	}
}

func (a *App) markInboundContinuationAction(
	messageID uuid.UUID,
	actionKey string,
) error {
	execution := a.inboundContinuation
	if execution == nil {
		return nil
	}
	organizationID := a.inboundContinuationOrganizationID()
	return a.DB.Transaction(func(tx *gorm.DB) error {
		var message models.Message
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where(
				"id = ? AND organization_id = ? AND direction = ?",
				messageID,
				organizationID,
				models.DirectionOutgoing,
			).
			First(&message).Error; err != nil {
			return fmt.Errorf("load chatbot response for action marker: %w", err)
		}
		metadata := make(models.JSONB, len(message.Metadata)+4)
		for key, value := range message.Metadata {
			metadata[key] = value
		}
		metadata["inbound_continuation_action_key"] = actionKey
		metadata["inbound_continuation_message_id"] = execution.MessageID.String()
		metadata["inbound_continuation_wamid"] = execution.WAMID
		metadata["inbound_continuation_marked_at"] = time.Now().UTC().Format(
			time.RFC3339Nano,
		)
		return tx.Model(&models.Message{}).
			Where("id = ? AND organization_id = ?", messageID, organizationID).
			Update("metadata", metadata).Error
	})
}

func (a *App) verifySynchronousChatbotSend(messageID uuid.UUID) error {
	var message models.Message
	if err := a.DB.Select("id", "status", "error_message").
		Where("id = ?", messageID).
		First(&message).Error; err != nil {
		return fmt.Errorf("verify synchronous chatbot send: %w", err)
	}
	switch message.Status {
	case models.MessageStatusSent,
		models.MessageStatusDelivered,
		models.MessageStatusRead:
		return nil
	case models.MessageStatusFailed:
		if strings.TrimSpace(message.ErrorMessage) == "" {
			return errors.New("chatbot provider send failed")
		}
		return fmt.Errorf("chatbot provider send failed: %s", message.ErrorMessage)
	default:
		return fmt.Errorf(
			"chatbot provider send finished with unresolved status %q",
			message.Status,
		)
	}
}

func (a *App) inboundContinuationAlreadyCompleted(
	messageID uuid.UUID,
) (bool, error) {
	var message models.Message
	if err := a.DB.Select("id", "metadata").
		Where(
			"id = ? AND organization_id = ? AND direction = ?",
			messageID,
			a.inboundContinuationOrganizationID(),
			models.DirectionIncoming,
		).
		First(&message).Error; err != nil {
		return false, fmt.Errorf("load inbound completion checkpoint: %w", err)
	}
	completed, _ := message.Metadata["inbound_continuation_completed"].(bool)
	return completed, nil
}

func (a *App) markInboundContinuationCompleted(messageID uuid.UUID) error {
	organizationID := a.inboundContinuationOrganizationID()
	return a.DB.Transaction(func(tx *gorm.DB) error {
		var message models.Message
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where(
				"id = ? AND organization_id = ? AND direction = ?",
				messageID,
				organizationID,
				models.DirectionIncoming,
			).
			First(&message).Error; err != nil {
			return fmt.Errorf("load inbound message for completion checkpoint: %w", err)
		}
		metadata := make(models.JSONB, len(message.Metadata)+2)
		for key, value := range message.Metadata {
			metadata[key] = value
		}
		metadata["inbound_continuation_completed"] = true
		metadata["inbound_continuation_completed_at"] = time.Now().UTC().Format(
			time.RFC3339Nano,
		)
		return tx.Model(&models.Message{}).
			Where("id = ? AND organization_id = ?", messageID, organizationID).
			Update("metadata", metadata).Error
	})
}

func (a *App) inboundContinuationOrganizationID() uuid.UUID {
	if a == nil {
		return uuid.Nil
	}
	if a.tenantOrgID != uuid.Nil {
		return a.tenantOrgID
	}
	if a.inboundContinuation == nil {
		return uuid.Nil
	}
	var message models.Message
	if err := a.DB.Select("organization_id").
		Where("id = ?", a.inboundContinuation.MessageID).
		First(&message).Error; err == nil {
		return message.OrganizationID
	}
	return uuid.Nil
}

func (a *App) ensureInboundContinuationJob(
	account *models.WhatsAppAccount,
	message *models.Message,
	inbound IncomingTextMessage,
	profileName string,
) error {
	if a == nil || a.DB == nil || account == nil || message == nil {
		return errors.New("inbound continuation job requires an app, account, and message")
	}
	if account.OrganizationID == uuid.Nil ||
		message.ID == uuid.Nil ||
		strings.TrimSpace(inbound.ID) == "" {
		return errors.New("inbound continuation job identity is incomplete")
	}
	if message.OrganizationID != account.OrganizationID ||
		message.WhatsAppAccount != account.Name ||
		message.WhatsAppMessageID != inbound.ID ||
		message.Direction != models.DirectionIncoming {
		return errors.New("inbound continuation job does not match its message")
	}

	encoded, err := json.Marshal(inbound)
	if err != nil {
		return fmt.Errorf("encode inbound continuation payload: %w", err)
	}
	var rawMessage map[string]any
	if err := json.Unmarshal(encoded, &rawMessage); err != nil {
		return fmt.Errorf("normalize inbound continuation payload: %w", err)
	}

	now := time.Now().UTC()
	idempotencyKey := "inbound-message-continuation:" +
		uuid.NewSHA1(account.ID, []byte(strings.TrimSpace(inbound.ID))).String()
	payload := models.JSONB{
		"phone_number_id": account.PhoneID,
		"profile_name":    profileName,
		"wamid":           inbound.ID,
		"message_id":      message.ID.String(),
		"message":         rawMessage,
	}
	job := models.ScheduledJob{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: account.OrganizationID,
		Kind:           inboundContinuationJobKind,
		AggregateType:  "message",
		AggregateID:    &message.ID,
		RunAt:          now,
		Status:         models.ScheduledJobStatusPending,
		MaxAttempts:    defaultInboundContinuationMaxTry,
		IdempotencyKey: idempotencyKey,
		Payload:        payload,
		Version:        1,
	}
	if err := a.DB.Clauses(clause.OnConflict{DoNothing: true}).
		Create(&job).Error; err != nil {
		return fmt.Errorf("enqueue inbound continuation: %w", err)
	}

	var existing models.ScheduledJob
	if err := a.DB.Where(
		"organization_id = ? AND idempotency_key = ?",
		account.OrganizationID,
		idempotencyKey,
	).First(&existing).Error; err != nil {
		return fmt.Errorf("load inbound continuation job: %w", err)
	}
	if existing.Kind != inboundContinuationJobKind ||
		existing.AggregateID == nil ||
		*existing.AggregateID != message.ID {
		return errors.New("inbound continuation idempotency key collision")
	}

	switch existing.Status {
	case models.ScheduledJobStatusCompleted:
		return nil
	case models.ScheduledJobStatusProcessing:
		// A live claimant owns this job. A stale lease is reclaimed by the
		// processor; resetting it here could run a duplicate concurrently.
		return nil
	case models.ScheduledJobStatusPending:
		return a.DB.Model(&models.ScheduledJob{}).
			Where(
				"id = ? AND organization_id = ? AND status = ?",
				existing.ID,
				account.OrganizationID,
				models.ScheduledJobStatusPending,
			).
			Updates(map[string]any{
				"run_at":  now,
				"payload": payload,
				"version": gorm.Expr("version + 1"),
			}).Error
	case models.ScheduledJobStatusFailed, models.ScheduledJobStatusCancelled:
		manualReview, _ := existing.Payload["manual_review_required"].(bool)
		if manualReview {
			// Meta redelivery must not turn an uncertain remote action into an
			// automatic replay. An operator can inspect the action ledger and
			// explicitly resolve/requeue the message after reconciliation.
			return nil
		}
		result := a.DB.Model(&models.ScheduledJob{}).
			Where(
				"id = ? AND organization_id = ? AND status IN ?",
				existing.ID,
				account.OrganizationID,
				[]models.ScheduledJobStatus{
					models.ScheduledJobStatusFailed,
					models.ScheduledJobStatusCancelled,
				},
			).
			Updates(map[string]any{
				"status":       models.ScheduledJobStatusPending,
				"attempts":     0,
				"run_at":       now,
				"locked_at":    nil,
				"locked_by":    "",
				"completed_at": nil,
				"last_error":   "",
				"payload":      payload,
				"version":      gorm.Expr("version + 1"),
			})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return errors.New("inbound continuation retry enqueue lost a race")
		}
		return nil
	default:
		return fmt.Errorf(
			"unsupported inbound continuation status %q",
			existing.Status,
		)
	}
}
