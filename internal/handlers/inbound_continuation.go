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
	defaultInboundAIAttemptTimeout   = 2 * time.Minute

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
	OrganizationID uuid.UUID
	ContactID      uuid.UUID
	MessageID      uuid.UUID
	WAMID          string
	actionScope    string
	actionIndex    int
	attemptGuarded bool
	// Only the audited captured generated-text route sends without an outer
	// history transaction. Its verification and event reads still enter tenant scope.
	detachedGeneratedApply bool
	// Booking commits session proposals independently before provider I/O.
	// Its graph checkpoint must therefore not reuse an older outer snapshot.
	nativeBookingCheckpoint bool
	// Only a prepared nonlegacy route supplies this job-local session. Graph
	// persistence advances the same object before its next provider boundary.
	preparedSession *models.ChatbotSession
	// Ownership survives the initiating caller, but ends on an actual lease
	// failure. It gates new attempts without cancelling an accepted request or
	// its independent result checkpoint.
	ownershipCtx context.Context
	jobID        uuid.UUID
	workerID     string
	// Bound only during one physical guard; the callback reuses that existing
	// READ COMMITTED transaction after the independent action claim commits.
	attemptOwnershipCheck func() error
}

type inboundContinuationPolicyStop struct {
	Reason string
}

func (e *inboundContinuationPolicyStop) Error() string {
	if e == nil || strings.TrimSpace(e.Reason) == "" {
		return "automatic reply stopped by current physical-contact policy"
	}
	return "automatic reply stopped by current physical-contact policy: " +
		strings.TrimSpace(e.Reason)
}

func inboundContinuationStoppedByPolicy(err error) bool {
	var target *inboundContinuationPolicyStop
	return errors.As(err, &target)
}

type inboundContinuationActionIdentity struct {
	Key     string
	Scope   string
	Ordinal int
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
	Scope   string
	Ordinal int
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
	// A nil override selects the production phased processor. Tests that supply
	// process retain the ordinary transactional callback boundary.
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

	if err := p.app.checkInboundContinuationEntry(ctx); err != nil {
		return err
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
	var running sync.WaitGroup
	errorsCh := make(chan error, remaining+1)
	var runErrors []error
	// Admission precedes each claim. The dispatcher therefore never parks a
	// claimed, unheartbeated batch behind a local queue. Each claimed invocation
	// has a distinct owner, including retries overlapping a lost lease.
	for remaining > 0 && ctx.Err() == nil {
		madeProgress := false
		for _, organizationID := range organizationIDs {
			if remaining <= 0 || ctx.Err() != nil {
				break
			}
			release, preparationMode, admitErr := p.acquirePreparationCapacity(ctx, organizationID, inboundContinuationWriter)
			if admitErr != nil {
				runErrors = append(runErrors, admitErr)
				remaining = 0
				break
			}
			worker := p.claimWorker()
			job, claimErr := worker.claim(ctx, organizationID, nil)
			if claimErr != nil {
				release()
				runErrors = append(runErrors, claimErr)
				continue
			}
			if job == nil {
				release()
				continue
			}
			madeProgress = true
			remaining--
			if p.process != nil || !p.app.rlsEnabled() {
				if err := worker.processClaimed(p.app.markInboundContinuationContext(ctx), organizationID, job, release, preparationMode); err != nil {
					runErrors = append(runErrors, err)
				}
				continue
			}
			running.Add(1)
			go func() {
				defer running.Done()
				if err := worker.processClaimed(p.app.markInboundContinuationContext(ctx), organizationID, job, release, preparationMode); err != nil {
					errorsCh <- err
				}
			}()
		}
		if !madeProgress {
			break
		}
	}
	running.Wait()
	close(errorsCh)
	for err := range errorsCh {
		runErrors = append(runErrors, err)
	}
	if ctx.Err() != nil {
		runErrors = append(runErrors, ctx.Err())
	}
	return errors.Join(runErrors...)
}

func (p *InboundContinuationProcessor) claimWorker() *InboundContinuationProcessor {
	return &InboundContinuationProcessor{app: p.app, interval: p.interval, lease: p.lease,
		batchSize: p.batchSize, workerID: p.workerID + ":" + uuid.NewString(), now: p.now,
		process: p.process, stopCh: p.stopCh}
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
	if err := p.app.checkInboundContinuationEntry(ctx); err != nil {
		return err
	}
	preparationMode, err := p.hintInboundContinuationPreparationMode(ctx, organizationID, messageID)
	if err != nil {
		return err
	}
	preparationCtx := ctx
	if preparationMode == inboundContinuationWriter {
		// A hinted writer retains the existing replacement-writer eligibility,
		// including dynamic tiny-pool refusal, without marking the claim context.
		preparationCtx = p.app.markInboundContinuationContext(ctx)
	}
	release, preparationMode, err := p.acquirePreparationCapacity(preparationCtx, organizationID, preparationMode)
	if err != nil {
		return err
	}
	worker := p.claimWorker()
	job, err := worker.claim(ctx, organizationID, &messageID)
	if err != nil || job == nil {
		release()
		return err
	}
	return worker.processClaimed(p.app.markInboundContinuationContext(ctx), organizationID, job, release, preparationMode)
}

// Recovery preparation owns Writer1 from before the claim through preparation;
// the fast path uses an advisory hint, with Reader1 and writer fallback otherwise.
// Both release preparation before ordinary model admission unless the fast path
// can safely retain its reader. Custom callbacks retain generic admission.
// RunOnce still enqueues tenants serially and claims at most one bounded batch.
func (p *InboundContinuationProcessor) acquirePreparationCapacity(ctx context.Context, organizationID uuid.UUID, mode int) (func(), int, error) {
	if p.process != nil || !p.app.rlsEnabled() {
		release, err := p.app.acquireInboundContinuationCapacity(ctx, p.stopCh, 1)
		return release, inboundContinuationUnscoped, err
	}
	release, err := p.app.acquireInboundContinuationPhase(ctx, p.stopCh, 1, organizationID.String(), mode)
	return release, mode, err
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
	releasePreparation func(),
	preparationMode int,
) (resultErr error) {
	defer releasePreparation()
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
	// An accepted result may need to finish applying after the initiating HTTP
	// fast path expires. Its durable owner must keep renewing until that work is
	// done; only an actual lease failure cancels this independent ownership scope.
	ownershipCtx, cancelOwnership := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelOwnership()
	heartbeatDone := make(chan error, 1)
	go func() {
		heartbeatErr := p.maintainLease(
			ownershipCtx,
			organizationID,
			job.ID,
		)
		if heartbeatErr != nil {
			cancelOwnership()
			cancelProcess()
		}
		heartbeatDone <- heartbeatErr
	}()

	var (
		work            *persistedIncomingMessage
		continuationErr error
	)
	// Revalidate durable identity and repair the one proven display projection
	// in a short committed phase. Authority/contact/message locks must be
	// released before the provider scope and its independent action ledger.
	phaseOneFenceExpired := false
	scopeErr := func() error {
		if p.process == nil && p.app.rlsEnabled() {
			return p.app.withContinuationReaderApp(organizationID, func(scoped *App) error {
				var err error
				work, err = loadInboundContinuationWorkMode(scoped, organizationID, job, true)
				return err
			})
		}
		return p.app.WithCommittedTenantApp(organizationID, func(scoped *App) error {
			// H1: this phase holds a pooled connection while it waits for the organization
			// fence. Every in-flight AI-attempt guard holds that fence's KEY SHARE, and a guard
			// holder may itself be waiting for a pooled connection, so an unbounded wait here
			// closes a pool+lock cycle. Nothing has been claimed yet: bound only the fence
			// statements (the COMMIT keeps the transaction context) so this waiter yields
			// (20s) before any post-claim wait inside the fence (45s) and retries cleanly.
			fenceParent := scoped.DB.Statement.Context
			if fenceParent == nil {
				fenceParent = context.Background()
			}
			fenceCtx, cancelFence := context.WithTimeout(fenceParent, 20*time.Second)
			fenceErr := database.LockOrganizationPolicyScope(scoped.DB.WithContext(fenceCtx), organizationID)
			phaseOneFenceExpired = fenceErr != nil && errors.Is(fenceCtx.Err(), context.DeadlineExceeded)
			cancelFence()
			if fenceErr != nil {
				return fenceErr
			}
			var loadErr error
			work, loadErr = loadInboundContinuationWork(scoped, organizationID, job)
			return loadErr
		})
	}()
	if scopeErr != nil && phaseOneFenceExpired {
		scopeErr = fmt.Errorf("wedge guard: organization policy fence not acquired within 20s before the continuation: %w: %w", context.DeadlineExceeded, scopeErr)
	}
	if scopeErr == nil && p.process == nil && p.app.rlsEnabled() {
		executionApp := p.app.rootApp().scopedApp(p.app.rootApp().DB, organizationID)
		// No transaction is owned by this App. Every preparation read explicitly
		// enters tenant scope, and the remote phase uses its independent guard.
		executionApp.tenantOrgID = uuid.Nil
		executionApp.inboundContinuation = &inboundContinuationExecution{
			OrganizationID: work.OrganizationID, ContactID: work.Contact.ID,
			MessageID: work.Persisted.ID, WAMID: work.Message.ID,
			ownershipCtx: ownershipCtx, jobID: job.ID, workerID: p.workerID,
		}
		alreadyCompleted := false
		scopeErr = executionApp.WithTenantApp(organizationID, func(scoped *App) error {
			var err error
			alreadyCompleted, err = scoped.inboundContinuationAlreadyCompleted(work.Persisted.ID)
			return err
		})
		if scopeErr == nil && !alreadyCompleted {
			continuationErr = p.processPreparedIncoming(processCtx, ownershipCtx, executionApp, work, job, releasePreparation, preparationMode)
			if inboundContinuationStoppedByPolicy(continuationErr) {
				continuationErr = executionApp.WithTenantApp(organizationID, func(scoped *App) error {
					return scoped.markInboundContinuationCompleted(work.Persisted.ID)
				})
			}
		}
	}
	if scopeErr == nil && (p.process != nil || !p.app.rlsEnabled()) {
		releasePreparation()
		releaseApply, admissionErr := p.app.acquireInboundContinuationCapacity(processCtx, p.stopCh, 3)
		if admissionErr != nil {
			scopeErr = admissionErr
		} else {
			defer releaseApply()
			scopeErr = p.app.WithTenantApp(organizationID, func(scoped *App) error {
				executionApp := scoped.scopedApp(scoped.DB, organizationID)
				// executionApp carries per-job action state, but the surrounding scoped
				// App owns the transaction commit. Move every deferred realtime callback
				// to that owner on all exits; adoptAfterCommit drains the nested queue so
				// it cannot be delivered twice.
				defer scoped.adoptAfterCommit(executionApp)
				executionApp.inboundContinuation = &inboundContinuationExecution{
					OrganizationID: work.OrganizationID,
					ContactID:      work.Contact.ID,
					MessageID:      work.Persisted.ID,
					WAMID:          work.Message.ID,
					ownershipCtx:   ownershipCtx,
					jobID:          job.ID,
					workerID:       p.workerID,
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
				if p.process != nil {
					continuationErr = p.process(processCtx, executionApp, work)
				} else {
					continuationErr = executionApp.continuePersistedIncomingMessage(processCtx, work)
				}
				if inboundContinuationStoppedByPolicy(continuationErr) {
					// Policy stops are terminal successful consumption of this inbound
					// job. Sticky suppression remains on the inbound Message and Resume
					// never resurrects any cancelled or skipped provider action.
					continuationErr = nil
				}
				if continuationErr == nil {
					continuationErr = executionApp.markInboundContinuationCompleted(
						work.Persisted.ID,
					)
				}
				return nil
			})
		}
	}
	cancelProcess()
	cancelOwnership()
	heartbeatErr := <-heartbeatDone

	processErr := scopeErr
	if processErr == nil {
		processErr = continuationErr
	}
	if processErr == nil {
		processErr = heartbeatErr
	}
	// H1: the job's terminal bookkeeping must not inherit the run's deadline. The
	// fast-path ctx (2 min) can expire during these writes' pool wait; the inner write
	// then fails and the job stays 'processing' until its 5-minute lease goes stale.
	// Both writes are lease-guarded (locked_by), so an uncancellable write is safe.
	settleCtx := context.WithoutCancel(ctx)
	if processErr != nil {
		return p.fail(settleCtx, organizationID, job, processErr)
	}
	if err := p.complete(settleCtx, organizationID, job.ID); err != nil {
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
	return loadInboundContinuationWorkMode(app, organizationID, job, false)
}

func loadInboundContinuationWorkMode(app *App, organizationID uuid.UUID, job *models.ScheduledJob, continuationValidation bool) (*persistedIncomingMessage, error) {
	if app == nil || app.DB == nil || job == nil || job.AggregateID == nil {
		return nil, errors.New("inbound continuation message reference is missing")
	}
	if organizationID == uuid.Nil || job.OrganizationID != organizationID || job.Kind != inboundContinuationJobKind {
		return nil, errors.New("inbound continuation tenant or kind mismatch")
	}
	if manualReview, _ := job.Payload["manual_review_required"].(bool); manualReview {
		return nil, &inboundContinuationManualReviewError{Reason: "inbound continuation requires prior action reconciliation"}
	}

	phoneNumberID, _ := job.Payload["phone_number_id"].(string)
	phoneNumberID = strings.TrimSpace(phoneNumberID)
	wamid, _ := job.Payload["wamid"].(string)
	if phoneNumberID == "" {
		return nil, errors.New("inbound continuation payload is incomplete")
	}
	// A global phone cache can retain a prior name or tenant binding. The
	// persisted job must prove the currently owned account independently.
	var accounts []models.WhatsAppAccount
	if err := app.DB.Where("organization_id = ? AND BTRIM(phone_id) = ?", organizationID, phoneNumberID).
		Limit(2).Find(&accounts).Error; err != nil {
		return nil, fmt.Errorf("load inbound continuation account: %w", err)
	}
	if len(accounts) != 1 {
		return nil, &inboundContinuationManualReviewError{Reason: "inbound continuation account is missing or ambiguous"}
	}
	account := &accounts[0]
	if err := app.prepareWhatsAppMessageAuthority(account); err != nil {
		return nil, fmt.Errorf("prepare inbound continuation authority: %w", err)
	}
	inbound, err := validateInboundContinuationJobProof(job, account, *job.AggregateID, wamid)
	if err != nil {
		return nil, &inboundContinuationManualReviewError{Reason: err.Error()}
	}
	resolved, err := app.resolveWhatsAppMessage(account, whatsAppMessageLookup{
		WAMID: inbound.ID, MessageID: *job.AggregateID,
		Direction: models.DirectionIncoming,
		Identity: coexistenceContactIdentity{
			Phone: inbound.From, UserID: inbound.FromUserID, ParentUserID: inbound.FromParentUserID,
		},
		Lock: true, RepairProjection: true, continuationValidation: continuationValidation,
	})
	if err != nil {
		return nil, fmt.Errorf("resolve inbound continuation message: %w", err)
	}
	if resolved == nil || resolved.Message.ID != *job.AggregateID {
		return nil, &inboundContinuationManualReviewError{Reason: "inbound continuation message identity was not proven"}
	}
	if resolved.Continuation == nil || resolved.Continuation.ID != job.ID ||
		resolved.Continuation.Status != models.ScheduledJobStatusProcessing ||
		resolved.Continuation.LockedBy != job.LockedBy {
		return nil, errors.New("inbound continuation durable claim changed before execution")
	}
	if manualReview, _ := resolved.Continuation.Payload["manual_review_required"].(bool); manualReview {
		return nil, &inboundContinuationManualReviewError{Reason: "inbound continuation requires prior action reconciliation"}
	}
	if err := app.prepareWhatsAppAccountForRuntime(account); err != nil {
		return nil, fmt.Errorf("prepare inbound continuation credentials: %w", err)
	}
	return &persistedIncomingMessage{
		OrganizationID: organizationID,
		PhoneNumberID:  phoneNumberID,
		Message:        inbound,
		Account:        *account,
		Contact:        resolved.Contact,
		Extracted:      app.extractMessageContentForPersistence(inbound),
		Persisted:      resolved.Message,
	}, nil
}

// validateInboundContinuationJobProof checks the durable account-derived job
// authority without invoking the message resolver. Names and contact fields
// alone are not account provenance; the resolver validates contact agreement.
func validateInboundContinuationJobProof(
	job *models.ScheduledJob,
	account *models.WhatsAppAccount,
	messageID uuid.UUID,
	wamid string,
) (IncomingTextMessage, error) {
	var inbound IncomingTextMessage
	if job == nil || account == nil || account.ID == uuid.Nil || account.OrganizationID == uuid.Nil ||
		messageID == uuid.Nil || strings.TrimSpace(wamid) == "" || strings.TrimSpace(account.PhoneID) == "" {
		return inbound, errors.New("inbound continuation proof identity is incomplete")
	}
	expectedKey := "inbound-message-continuation:" + uuid.NewSHA1(account.ID, []byte(strings.TrimSpace(wamid))).String()
	if job.OrganizationID != account.OrganizationID || job.Kind != inboundContinuationJobKind ||
		job.AggregateType != "message" || job.AggregateID == nil || *job.AggregateID != messageID ||
		job.IdempotencyKey != expectedKey {
		return inbound, errors.New("inbound continuation durable job identity mismatch")
	}
	payloadMessageID, _ := job.Payload["message_id"].(string)
	parsedMessageID, err := uuid.Parse(payloadMessageID)
	payloadWAMID, _ := job.Payload["wamid"].(string)
	phoneID, _ := job.Payload["phone_number_id"].(string)
	if err != nil || parsedMessageID != messageID || payloadWAMID != wamid ||
		strings.TrimSpace(phoneID) != strings.TrimSpace(account.PhoneID) {
		return inbound, errors.New("inbound continuation payload identity mismatch")
	}
	rawMessage, exists := job.Payload["message"]
	if !exists || rawMessage == nil {
		return inbound, errors.New("inbound continuation raw message is missing")
	}
	encoded, err := json.Marshal(rawMessage)
	if err != nil {
		return inbound, fmt.Errorf("encode inbound continuation raw message: %w", err)
	}
	if err := json.Unmarshal(encoded, &inbound); err != nil {
		return inbound, fmt.Errorf("decode inbound continuation raw message: %w", err)
	}
	if inbound.ID != wamid || (strings.TrimSpace(inbound.From) == "" &&
		strings.TrimSpace(inbound.FromUserID) == "" && strings.TrimSpace(inbound.FromParentUserID) == "") {
		return inbound, errors.New("inbound continuation raw message identity mismatch")
	}
	return inbound, nil
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
		// H1: a "wedge guard" timeout is pool/lock congestion met while this job held
		// its continuation transaction, not a fault of the job. Refund the attempt (at
		// most 16 times per job) so a busy period cannot exhaust MaxAttempts; the
		// backoff below still grows with the deferrals.
		wedgeDeferrals, _ := inboundContinuationExactInteger(job.Payload["wedge_guard_deferrals"])
		wedgeGuard := !manualReview && wedgeDeferrals < 16 &&
			errors.Is(cause, context.DeadlineExceeded) &&
			strings.Contains(cause.Error(), "wedge guard")
		if manualReview || (job.Attempts >= maxAttempts && !wedgeGuard) {
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
		if wedgeGuard {
			deferredPayload := cloneInboundContinuationJSONB(job.Payload)
			deferredPayload["wedge_guard_deferrals"] = wedgeDeferrals + 1
			updates["payload"] = deferredPayload
			updates["attempts"] = gorm.Expr("GREATEST(attempts - 1, 0)")
		}
		if status == models.ScheduledJobStatusPending {
			updates["run_at"] = p.now().UTC().Add(
				inboundContinuationBackoff(job.Attempts + wedgeDeferrals),
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
	// Rune-safe so a stored error never contains a split multi-byte character.
	return truncateString(err.Error(), maxLength)
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
	return a.withInboundContinuationPhysicalAIAttempt(
		context.Background(),
		func() error {
			return a.runInboundContinuationSendGuarded(kind, payload, send)
		},
	)
}

func (a *App) runInboundContinuationSendGuarded(
	kind string,
	payload any,
	send func() (*models.Message, error),
) error {

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
	if err := a.requireInboundContinuationAttemptOwnership(); err != nil {
		return errors.Join(err, a.resolveInboundContinuationAction(context.Background(), claim,
			inboundActionStateFailedBeforeDispatch, nil))
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
		// committed action ledger is the replay authority. H1: the marker
		// commits in its own short transaction, never on the continuation
		// transaction (see markInboundContinuationAction).
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

// withInboundContinuationPhysicalAIAttempt holds only the organization row
// across one external provider attempt. The action claim and result use fresh
// root-pool transactions, so pre_attempt/result/uncertainty remain durable
// even if the surrounding continuation/session transaction later rolls back.
func (a *App) withInboundContinuationPhysicalAIAttempt(
	ctx context.Context,
	attempt func() error,
) error {
	if a == nil || a.rootApp() == nil || a.rootApp().DB == nil || attempt == nil {
		return errors.New("automatic reply attempt requires an app database and callback")
	}
	execution := a.inboundContinuation
	if execution == nil || execution.OrganizationID == uuid.Nil ||
		execution.ContactID == uuid.Nil || execution.MessageID == uuid.Nil {
		return &inboundContinuationPolicyStop{Reason: "durable inbound attempt identity is unavailable"}
	}
	if execution.attemptGuarded {
		return errors.New("nested automatic reply provider attempt is forbidden")
	}
	root := a.rootApp()
	if err := database.RequireIndependentAIAttemptConnections(root.DB); err != nil {
		return &inboundContinuationPolicyStop{Reason: err.Error()}
	}
	if ctx == nil {
		ctx = context.Background()
	}
	attemptCtx, cancel := context.WithTimeout(ctx, defaultInboundAIAttemptTimeout)
	defer cancel()

	return a.withBoundedRootConnection(attemptCtx, 0, func(guardDB *gorm.DB) error {
		return database.WithTenantReadCommitted(guardDB, execution.OrganizationID, func(guardTx *gorm.DB) error {
			// H1: bound only the organization-lock wait. Expiry means a policy writer held
			// the fence (possibly while waiting on this continuation's own transaction):
			// retry, never a silent policy stop.
			lockCtx, cancelLock := context.WithTimeout(attemptCtx, 30*time.Second)
			lockErr := database.LockOrganizationAIAttemptScope(guardTx.WithContext(lockCtx), execution.OrganizationID)
			lockExpired := lockCtx.Err() != nil
			cancelLock()
			if lockErr != nil {
				if lockExpired {
					return fmt.Errorf("wedge guard: organization AI attempt fence not acquired within 30s: %w: %w", context.DeadlineExceeded, lockErr)
				}
				return &inboundContinuationPolicyStop{Reason: lockErr.Error()}
			}
			if err := a.requireInboundContinuationPolicyTx(guardTx); err != nil {
				return err
			}
			if err := a.requirePreparedContinuationSessionTx(guardTx, execution.preparedSession); err != nil {
				return err
			}
			if err := a.requireInboundContinuationOwnershipTx(guardTx); err != nil {
				return err
			}
			execution.attemptGuarded = true
			execution.attemptOwnershipCheck = func() error {
				return a.requireInboundContinuationOwnershipTx(guardTx)
			}
			defer func() {
				execution.attemptGuarded = false
				execution.attemptOwnershipCheck = nil
			}()
			return attempt()
		})
	})
}

// requireInboundContinuationPolicyTx shares the current-contact and sticky
// inbound cutoff checks with local booking preparation. It never acquires a
// new transaction or upgrades the caller's organization fence.
func (a *App) requireInboundContinuationPolicyTx(tx *gorm.DB) error {
	execution := a.inboundContinuation
	if execution == nil || execution.OrganizationID == uuid.Nil || execution.ContactID == uuid.Nil || execution.MessageID == uuid.Nil {
		return &inboundContinuationPolicyStop{Reason: "durable inbound identity is unavailable"}
	}
	policy, err := database.EvaluateContactAutomaticReplyPolicy(tx, execution.OrganizationID, execution.ContactID)
	if err != nil {
		return &inboundContinuationPolicyStop{Reason: err.Error()}
	}
	if !policy.Allowed {
		return &inboundContinuationPolicyStop{Reason: policy.Reason}
	}
	var inbound models.Message
	if err := tx.Where("id = ? AND organization_id = ?", execution.MessageID, execution.OrganizationID).First(&inbound).Error; err != nil {
		return &inboundContinuationPolicyStop{Reason: "durable inbound message is unavailable"}
	}
	if inbound.ContactID != execution.ContactID || inbound.Direction != models.DirectionIncoming ||
		inbound.WhatsAppMessageID != execution.WAMID || strings.TrimSpace(execution.WAMID) == "" {
		return &inboundContinuationPolicyStop{Reason: "durable inbound message identity changed"}
	}
	for _, key := range []string{incomingAutomaticAISuppressedKey, "inbound_continuation_completed"} {
		if raw, exists := inbound.Metadata[key]; exists {
			stopped, valid := raw.(bool)
			if !valid || stopped {
				return &inboundContinuationPolicyStop{Reason: "durable inbound message is suppressed or terminal"}
			}
		}
	}
	return nil
}

func (a *App) nextInboundContinuationActionKey(
	kind string,
	payload any,
) (inboundContinuationActionIdentity, error) {
	execution := a.inboundContinuation
	if execution == nil {
		return inboundContinuationActionIdentity{}, nil
	}
	if execution.MessageID == uuid.Nil {
		return inboundContinuationActionIdentity{}, errors.New("inbound action message identity is missing")
	}
	kind = strings.TrimSpace(kind)
	if kind == "" {
		return inboundContinuationActionIdentity{}, errors.New("inbound action kind is required")
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
	return inboundContinuationActionIdentity{
		Key:     "inbound-action:" + uuid.NewSHA1(execution.MessageID, []byte(fingerprint)).String(),
		Scope:   scope,
		Ordinal: index,
	}, nil
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
	identity, err := a.nextInboundContinuationActionKey(kind, payload)
	if err != nil {
		return nil, err
	}
	actionKey := identity.Key
	if actionKey == "" {
		return &inboundContinuationActionClaim{
			Kind:    strings.TrimSpace(kind),
			Scope:   identity.Scope,
			Ordinal: identity.Ordinal,
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
	if !execution.attemptGuarded {
		return nil, &inboundContinuationPolicyStop{Reason: "provider action has no organization attempt guard"}
	}
	// H1: a claim precedes every provider effect, so an expired wait here is a clean
	// retry that frees this goroutine's continuation and guard connections. Give it
	// the shortest budget so claim waiters yield before post-claim waiters.
	claimCtx, cancelClaim := context.WithTimeout(ctx, 10*time.Second)
	defer cancelClaim()

	now := time.Now().UTC()
	var claimed *inboundContinuationActionClaim
	err = a.withIndependentInboundContinuationAction(
		claimCtx,
		organizationID,
		func(tx *gorm.DB) error {
			actionPayload := models.JSONB{
				"inbound_message_id": execution.MessageID.String(),
				"effect_kind":        strings.TrimSpace(kind),
				"action_scope":       identity.Scope,
				"action_ordinal":     identity.Ordinal,
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
					Scope:   identity.Scope,
					Ordinal: identity.Ordinal,
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
			if err := validateInboundContinuationActionProof(&existing, organizationID, execution.MessageID, actionKey, kind, identity.Scope, identity.Ordinal); err != nil {
				return &inboundContinuationManualReviewError{ActionKey: actionKey, Reason: err.Error()}
			}
			manualReview, _ := existing.Payload["manual_review_required"].(bool)
			if manualReview {
				return &inboundContinuationManualReviewError{ActionKey: actionKey, Reason: "remote action outcome requires manual review"}
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
					Scope:   identity.Scope,
					Ordinal: identity.Ordinal,
					State:   state,
					Execute: false,
					Result:  result,
				}
				return nil
			case inboundActionStateFailedBeforeDispatch:
				if existing.Status != models.ScheduledJobStatusFailed {
					return &inboundContinuationManualReviewError{ActionKey: actionKey, Reason: "inbound action retry state does not match its durable status"}
				}
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
					Scope:   identity.Scope,
					Ordinal: identity.Ordinal,
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
	if organizationID == uuid.Nil {
		return database.ErrMissingTenant
	}
	if ctx == nil {
		ctx = context.Background()
	}
	// H1: enter through this nested App so the pool wait is bounded. A deadline on ctx is
	// the explicit WAIT budget only; the ledger transaction itself runs without it, so its
	// COMMIT can never be cancelled into an ambiguous outcome.
	wait := time.Duration(0)
	if deadline, hasDeadline := ctx.Deadline(); hasDeadline {
		if wait = time.Until(deadline); wait <= 0 {
			return fmt.Errorf("wedge guard: inbound action ledger wait budget already spent: %w", context.DeadlineExceeded)
		}
		ctx = context.WithoutCancel(ctx)
	}
	return a.withBoundedRootConnection(ctx, wait, func(db *gorm.DB) error {
		if !a.rlsEnabled() {
			if err := requireOrdinaryTenantOrganization(db, organizationID); err != nil {
				return err
			}
			return db.Transaction(fn)
		}
		return database.WithTenant(db, organizationID, func(tx *gorm.DB) error {
			if err := requireOrdinaryTenantOrganization(tx, organizationID); err != nil {
				return err
			}
			return tx.Transaction(fn) // savepoint, as today
		})
	})
}

// withBoundedRootConnection runs run on its own root-pool connection (H1 pool-wedge
// guard). A goroutine that may already own a pooled connection (its App's DB is a
// transaction, or it carries an inbound continuation) must never wait forever for
// another. Only the WAIT is bounded; the transaction run begins uses ctx (or the root
// context) exactly as before. wait > 0 is an explicit budget; wait == 0 derives it:
// 15s nested, 45s inside the AI-attempt fence, unbounded for a root receiver.
// run must finish every transaction it begins before returning; callers run
// after-commit callbacks only after this returns (connection already released).
func (a *App) withBoundedRootConnection(ctx context.Context, wait time.Duration, run func(db *gorm.DB) error) error {
	root := a.rootApp()
	if root == nil || root.DB == nil || root.DB.Statement == nil || run == nil {
		return errors.New("database connection is required")
	}
	txDB := root.DB
	if ctx != nil {
		txDB = root.DB.WithContext(ctx)
	}
	if _, rootIsTx := root.DB.Statement.ConnPool.(gorm.TxCommitter); rootIsTx {
		return run(txDB) // transaction-rooted test App: savepoints, no pool wait
	}
	if wait <= 0 {
		execution := a.inboundContinuation
		nested := execution != nil
		if a.DB != nil && a.DB.Statement != nil {
			if _, ownsTx := a.DB.Statement.ConnPool.(gorm.TxCommitter); ownsTx {
				nested = true
			}
		}
		if !nested {
			return run(txDB) // root receiver holds nothing: unchanged, unbounded
		}
		wait = 15 * time.Second
		if execution != nil && execution.attemptGuarded {
			wait = 45 * time.Second // post-claim waits here end in manual review
		}
	}
	txCtx := txDB.Statement.Context
	if txCtx == nil {
		txCtx = context.Background()
	}
	waitCtx, cancelWait := context.WithTimeout(txCtx, wait)
	defer cancelWait()
	acquired := false
	err := txDB.WithContext(waitCtx).Connection(func(pinned *gorm.DB) error {
		acquired = true
		cancelWait() // *sql.Conn keeps no reference to waitCtx
		return run(pinned.WithContext(txCtx))
	})
	if err != nil && !acquired && errors.Is(err, context.DeadlineExceeded) && (ctx == nil || ctx.Err() == nil) {
		return fmt.Errorf("wedge guard: no pooled database connection within %s while this goroutine may hold another: %w", wait, err)
	}
	return err
}

func validateInboundContinuationActionProof(
	job *models.ScheduledJob,
	organizationID, messageID uuid.UUID,
	actionKey, kind, scope string,
	ordinal int,
) error {
	if job == nil || organizationID == uuid.Nil || messageID == uuid.Nil || actionKey == "" ||
		job.OrganizationID != organizationID || job.Kind != inboundContinuationActionJobKind ||
		job.AggregateType != "inbound_message_action" || job.AggregateID == nil ||
		*job.AggregateID != messageID || job.IdempotencyKey != actionKey {
		return errors.New("inbound action durable identity mismatch")
	}
	payloadMessageID, _ := job.Payload["inbound_message_id"].(string)
	parsedMessageID, err := uuid.Parse(payloadMessageID)
	effectKind, _ := job.Payload["effect_kind"].(string)
	actionScope, _ := job.Payload["action_scope"].(string)
	actionOrdinal, ordinalOK := inboundContinuationExactInteger(job.Payload["action_ordinal"])
	if err != nil || parsedMessageID != messageID || effectKind != strings.TrimSpace(kind) ||
		actionScope != strings.TrimSpace(scope) || !ordinalOK || actionOrdinal != ordinal {
		return errors.New("inbound action payload identity mismatch")
	}
	return nil
}

func inboundContinuationExactInteger(value any) (int, bool) {
	switch typed := value.(type) {
	case int:
		return typed, true
	case int64:
		return int(typed), int64(int(typed)) == typed
	case float64:
		converted := int(typed)
		return converted, float64(converted) == typed
	default:
		return 0, false
	}
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
	if organizationID == uuid.Nil || a.inboundContinuation == nil {
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
			if err := validateInboundContinuationActionProof(
				&stored,
				organizationID,
				a.inboundContinuation.MessageID,
				claim.Key,
				claim.Kind,
				claim.Scope,
				claim.Ordinal,
			); err != nil {
				return &inboundContinuationManualReviewError{ActionKey: claim.Key, Reason: err.Error()}
			}
			if manualReview, _ := stored.Payload["manual_review_required"].(bool); manualReview {
				return &inboundContinuationManualReviewError{ActionKey: claim.Key, Reason: "remote action outcome requires manual review"}
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
	var result models.JSONB
	err := a.withInboundContinuationPhysicalAIAttempt(
		context.Background(),
		func() error {
			var guardedErr error
			result, guardedErr = a.runInboundContinuationCapturedActionGuarded(
				kind,
				action,
			)
			return guardedErr
		},
	)
	return result, err
}

func (a *App) runInboundContinuationCapturedActionGuarded(
	kind string,
	action func() models.JSONB,
) (models.JSONB, error) {
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
	if err := a.requireInboundContinuationAttemptOwnership(); err != nil {
		return nil, errors.Join(err, a.resolveInboundContinuationAction(context.Background(), claim,
			inboundActionStateFailedBeforeDispatch, nil))
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

// withInboundContinuationDetachedApplyRead is limited to send verification,
// resolved-send lookup and realtime payload reads. Close this short read scope
// before recovery opens its independent SLA restoration transaction. Ordinary
// callers retain their existing transaction and snapshot.
func (a *App) withInboundContinuationDetachedApplyRead(read func(*gorm.DB) error) error {
	if a == nil || a.DB == nil || read == nil {
		return errors.New("inbound send verification requires an app database and callback")
	}
	execution := a.inboundContinuation
	if execution == nil || !execution.detachedGeneratedApply {
		return read(a.DB)
	}
	root := a.rootApp()
	if !execution.attemptGuarded || execution.OrganizationID == uuid.Nil ||
		a.tenantOrgID != uuid.Nil || root == nil || root.DB == nil ||
		a.DB.Statement == nil || root.DB.Statement == nil ||
		a.DB.Statement.ConnPool != root.DB.Statement.ConnPool {
		return errors.New("detached generated send verification requires its guarded root pool")
	}
	if err := a.requireInboundContinuationPoolEntry(); err != nil {
		return err
	}
	// Keep the root's uncancelled context, independently of the physical
	// guard deadline, just as the former outer verification transaction did.
	return a.WithTenantApp(execution.OrganizationID, func(scoped *App) error {
		return read(scoped.DB)
	})
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
	err = a.withInboundContinuationDetachedApplyRead(func(db *gorm.DB) error {
		return db.Unscoped().
			Select("id", "organization_id", "contact_id", "created_at", "status", "error_message", "metadata").
			Where(
				"id = ? AND organization_id = ? AND direction = ?",
				messageID,
				a.inboundContinuationOrganizationID(),
				models.DirectionOutgoing,
			).
			First(&message).Error
	})
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
		execution := a.inboundContinuation
		if execution == nil || execution.ContactID == uuid.Nil ||
			message.ContactID != execution.ContactID || message.CreatedAt.IsZero() {
			return &inboundContinuationManualReviewError{
				ActionKey: claim.Key,
				Reason:    "resolved send has an invalid persisted contact binding",
			}
		}
		state, _ := message.Metadata[automaticAIDispatchStateMetadataKey].(string)
		settledAtText, _ := message.Metadata[automaticAIDispatchSettledAtKey].(string)
		settledAt, parseErr := time.Parse(time.RFC3339Nano, strings.TrimSpace(settledAtText))
		if state != automaticAIDispatchStateResolved || parseErr != nil ||
			settledAt.IsZero() || settledAt.Before(message.CreatedAt) {
			return &inboundContinuationManualReviewError{
				ActionKey: claim.Key,
				Reason:    "resolved send has no valid canonical settlement time",
			}
		}

		organizationID := a.inboundContinuationOrganizationID()
		restoreErr := a.WithCommittedTenantApp(
			organizationID,
			func(scoped *App) error {
				var contact models.Contact
				contactErr := scoped.DB.Clauses(clause.Locking{Strength: "UPDATE"}).
					Select("id", "metadata", "chatbot_last_message_at").
					Where("id = ? AND organization_id = ?", execution.ContactID, organizationID).
					First(&contact).Error
				if errors.Is(contactErr, gorm.ErrRecordNotFound) {
					return &inboundContinuationManualReviewError{
						ActionKey: claim.Key,
						Reason:    "resolved send contact disappeared before SLA recovery",
					}
				}
				if contactErr != nil {
					return contactErr
				}
				if contact.ChatbotLastMessageAt != nil &&
					contact.ChatbotLastMessageAt.After(settledAt) {
					// A later committed bot reply remains the SLA authority.
					return nil
				}
				clearedThrough, watermarkErr := chatbotTrackingClearWatermark(contact.Metadata)
				if watermarkErr != nil {
					return &inboundContinuationManualReviewError{
						ActionKey: claim.Key,
						Reason:    "resolved send contact has an invalid SLA clear watermark",
					}
				}
				if !clearedThrough.IsZero() && !clearedThrough.Before(settledAt) {
					// A customer reply at or after this send intentionally cleared SLA.
					return nil
				}
				update := scoped.DB.Model(&models.Contact{}).
					Where("id = ? AND organization_id = ?", execution.ContactID, organizationID).
					Updates(map[string]any{
						"chatbot_last_message_at": settledAt,
						"chatbot_reminder_sent":   false,
					})
				if update.Error != nil {
					return update.Error
				}
				if update.RowsAffected != 1 {
					return &inboundContinuationManualReviewError{
						ActionKey: claim.Key,
						Reason:    "resolved send contact disappeared before SLA recovery",
					}
				}
				return nil
			},
		)
		if restoreErr != nil {
			var manualReview *inboundContinuationManualReviewError
			if errors.As(restoreErr, &manualReview) {
				return restoreErr
			}
			return fmt.Errorf("restore resolved inbound send SLA state: %w", restoreErr)
		}
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
	mark := func(tx *gorm.DB) error {
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
	}
	// H1: commit this operator marker in its own short transaction, never on the
	// continuation transaction. A FOR UPDATE lock on the independently committed
	// response, held until the continuation commits, inverts the global
	// contact-then-message order: a status receipt for this response locks the contact
	// and waits for the row, and a later handover or send of this same continuation then
	// waits for that contact while the continuation waits for it in Go (a cycle
	// PostgreSQL cannot see). The marker is best effort (its caller only logs) and the
	// action ledger stays the replay authority, so inside the AI-attempt fence it waits
	// at most 2s for a connection, like the other best-effort writes there.
	markWait := time.Duration(0) // derived: 15s nested
	if execution.attemptGuarded {
		markWait = 2 * time.Second
	}
	// The root pool's own context, as in WithTenantApp (never a nil Context).
	rootCtx := context.Background()
	if root := a.rootApp(); root != nil && root.DB != nil && root.DB.Statement != nil && root.DB.Statement.Context != nil {
		rootCtx = root.DB.Statement.Context
	}
	return a.withBoundedRootConnection(rootCtx, markWait, func(db *gorm.DB) error {
		if organizationID == uuid.Nil {
			return database.ErrMissingTenant
		}
		if a.rlsEnabled() {
			return database.WithTenant(db, organizationID, mark)
		}
		return db.Transaction(mark)
	})
}

func (a *App) verifySynchronousChatbotSend(messageID uuid.UUID) error {
	var message models.Message
	if err := a.withInboundContinuationDetachedApplyRead(func(db *gorm.DB) error {
		return db.Select("id", "status", "error_message").
			Where("id = ?", messageID).
			First(&message).Error
	}); err != nil {
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
	if a.inboundContinuation != nil &&
		a.inboundContinuation.OrganizationID != uuid.Nil {
		if a.tenantOrgID != uuid.Nil &&
			a.tenantOrgID != a.inboundContinuation.OrganizationID {
			return uuid.Nil
		}
		return a.inboundContinuation.OrganizationID
	}
	if a.tenantOrgID != uuid.Nil {
		return a.tenantOrgID
	}
	if a.inboundContinuation == nil || a.DB == nil {
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
	// Ordinary inbound callers already own the prepared authority and contact
	// locks. A direct caller still needs one atomic proof/enqueue transaction.
	return a.DB.Transaction(func(tx *gorm.DB) error {
		scoped := a.scopedApp(tx, account.OrganizationID)
		currentAccount := *account
		if err := scoped.prepareWhatsAppMessageAuthority(&currentAccount); err != nil {
			return fmt.Errorf("prepare inbound continuation enqueue authority: %w", err)
		}
		resolved, err := scoped.resolveWhatsAppMessage(&currentAccount, whatsAppMessageLookup{
			WAMID: inbound.ID, MessageID: message.ID, ContactID: message.ContactID,
			Direction: models.DirectionIncoming,
			Identity: coexistenceContactIdentity{
				Phone: inbound.From, UserID: inbound.FromUserID, ParentUserID: inbound.FromParentUserID,
			},
			Lock: true, RepairProjection: true,
		})
		if err != nil {
			return fmt.Errorf("resolve inbound continuation enqueue message: %w", err)
		}
		if resolved == nil || resolved.Message.ID != message.ID {
			return errors.New("inbound continuation enqueue message identity was not proven")
		}
		if err := scoped.ensureInboundContinuationJobForMessage(&currentAccount, &resolved.Message, inbound, profileName); err != nil {
			return err
		}
		*message = resolved.Message
		return nil
	})
}

func (a *App) ensureInboundContinuationJobForMessage(
	account *models.WhatsAppAccount,
	message *models.Message,
	inbound IncomingTextMessage,
	profileName string,
) error {
	if account.OrganizationID == uuid.Nil ||
		message.ID == uuid.Nil ||
		strings.TrimSpace(inbound.ID) == "" {
		return errors.New("inbound continuation job identity is incomplete")
	}
	if message.OrganizationID != account.OrganizationID ||
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
	if proof, ok := message.Metadata[coexistencePhoneAdmissionKey].(string); ok {
		payload[coexistencePhoneAdmissionKey] = proof
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
	if err := a.DB.Clauses(clause.Locking{Strength: "UPDATE"}).Where(
		"organization_id = ? AND idempotency_key = ?",
		account.OrganizationID,
		idempotencyKey,
	).First(&existing).Error; err != nil {
		return fmt.Errorf("load inbound continuation job: %w", err)
	}
	if _, err := validateInboundContinuationJobProof(&existing, account, message.ID, inbound.ID); err != nil {
		return fmt.Errorf("inbound continuation idempotency key collision: %w", err)
	}
	manualReview, _ := existing.Payload["manual_review_required"].(bool)
	if manualReview {
		// Preserve uncertain outcomes regardless of the job status; redelivery
		// is not evidence authorizing an external action to run again.
		return nil
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
