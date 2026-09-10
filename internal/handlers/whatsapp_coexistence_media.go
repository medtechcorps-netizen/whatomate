package handlers

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/database"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/internal/queue"
	"github.com/shridarpatil/whatomate/internal/whatsappaccount"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	coexistenceMediaHydrationJobKind      = "coexistence_media.hydration"
	defaultCoexistenceMediaPoll           = 2 * time.Second
	defaultCoexistenceMediaLease          = 5 * time.Minute
	defaultCoexistenceMediaProcessTimeout = 2 * time.Minute
	defaultCoexistenceMediaBatch          = 50
	defaultCoexistenceMediaMaxAttempts    = 8
	coexistenceMediaHydratedIDMetadataKey = "coexistence_media_hydrated_id"
	coexistenceMediaProviderIDMetadataKey = "coexistence_media_id"
	coexistenceMediaRevokedMetadataKey    = "coexistence_revoked"
	coexistenceMediaMessageAggregate      = "message"
	coexistenceMediaReviewAggregate       = "whatsapp_identity_review_event"
)

var coexistenceMediaErrorURLPattern = regexp.MustCompile(`(?i)https?://[^\s"'<>]+`)

// CoexistenceMediaProcessor hydrates the short-lived Meta media identifiers
// delivered by history and smb_message_echoes. The job payload contains only
// durable identifiers; credentials are loaded and decrypted immediately before
// each provider call.
type CoexistenceMediaProcessor struct {
	app            *App
	interval       time.Duration
	lease          time.Duration
	processTimeout time.Duration
	batchSize      int
	workerID       string
	now            func() time.Time

	stopCh   chan struct{}
	stopOnce sync.Once
	doneCh   chan struct{}
	doneOnce sync.Once
	runMu    sync.Mutex
}

type coexistenceMediaWork struct {
	Account               models.WhatsAppAccount
	Message               models.Message
	StagedEvent           models.InboundEvent
	AggregateType         string
	AggregateID           uuid.UUID
	MimeType              string
	MediaID               string
	Revision              string
	AccessTokenGeneration [sha256.Size]byte
	Skip                  bool
	CleanupSafe           bool
	CleanupKey            string
	SkipReason            string
}

type coexistenceMediaPersistResult struct {
	Updated     bool
	CleanupSafe bool
	Reason      string
}

var errCoexistenceMediaLeaseLost = errors.New("coexistence media hydration lost its lease")

// NewCoexistenceMediaProcessor creates a tenant-safe durable media worker.
func NewCoexistenceMediaProcessor(app *App, interval time.Duration) *CoexistenceMediaProcessor {
	if interval <= 0 {
		interval = defaultCoexistenceMediaPoll
	}
	return &CoexistenceMediaProcessor{
		app:            app,
		interval:       interval,
		lease:          defaultCoexistenceMediaLease,
		processTimeout: defaultCoexistenceMediaProcessTimeout,
		batchSize:      defaultCoexistenceMediaBatch,
		workerID:       "coexistence-media:" + uuid.NewString(),
		now: func() time.Time {
			return time.Now().UTC()
		},
		stopCh: make(chan struct{}),
		doneCh: make(chan struct{}),
	}
}

// Start performs an immediate recovery pass, then polls until stopped.
func (p *CoexistenceMediaProcessor) Start(ctx context.Context) {
	if p == nil {
		return
	}
	defer p.doneOnce.Do(func() { close(p.doneCh) })
	if p.app == nil || p.app.DB == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-ctx.Done():
		return
	case <-p.stopCh:
		return
	default:
	}
	p.app.Log.Info("Coexistence media processor started", "interval", p.interval)
	if err := p.RunOnce(ctx); err != nil && ctx.Err() == nil {
		p.app.Log.Error("Coexistence media recovery failed", "error", err)
	}

	timer := time.NewTimer(p.interval)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			p.app.Log.Info("Coexistence media processor stopped by context")
			return
		case <-p.stopCh:
			p.app.Log.Info("Coexistence media processor stopped")
			return
		case <-timer.C:
			if err := p.RunOnce(ctx); err != nil && ctx.Err() == nil {
				p.app.Log.Error("Coexistence media run failed", "error", err)
			}
			timer.Reset(p.interval)
		}
	}
}

// Stop is safe to call more than once.
func (p *CoexistenceMediaProcessor) Stop() {
	if p == nil {
		return
	}
	p.stopOnce.Do(func() { close(p.stopCh) })
}

// Wait blocks until Start has returned or until ctx expires. Shutdown callers
// use a bounded context so a filesystem or provider implementation that ignores
// cancellation cannot hold process termination indefinitely.
func (p *CoexistenceMediaProcessor) Wait(ctx context.Context) error {
	if p == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-p.doneCh:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// RunOnce claims a bounded, fair batch across ordinary tenants.
func (p *CoexistenceMediaProcessor) RunOnce(ctx context.Context) error {
	if p == nil || p.app == nil || p.app.DB == nil {
		return errors.New("coexistence media processor requires an app database")
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
		remaining = defaultCoexistenceMediaBatch
	}
	var runErrors []error
	for remaining > 0 {
		madeProgress := false
		for _, organizationID := range organizationIDs {
			if remaining <= 0 || ctx.Err() != nil {
				break
			}
			job, claimErr := p.claim(ctx, organizationID, nil, "")
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

// ProcessMessage provides a low-latency path while retaining the same durable
// claim used by recovery polling.
func (p *CoexistenceMediaProcessor) ProcessMessage(
	ctx context.Context,
	organizationID, messageID uuid.UUID,
) error {
	return p.processAggregate(ctx, organizationID, messageID, coexistenceMediaMessageAggregate)
}

// ProcessStagedEvent is the low-latency entry point for contact-free identity
// review media. It uses the same durable claim loop while retaining a distinct
// aggregate type, object namespace, and final persistence branch.
func (p *CoexistenceMediaProcessor) ProcessStagedEvent(
	ctx context.Context,
	organizationID, eventID uuid.UUID,
) error {
	return p.processAggregate(ctx, organizationID, eventID, coexistenceMediaReviewAggregate)
}

func (p *CoexistenceMediaProcessor) processAggregate(
	ctx context.Context,
	organizationID, aggregateID uuid.UUID,
	aggregateType string,
) error {
	if p == nil || p.app == nil || p.app.DB == nil {
		return errors.New("coexistence media processor requires an app database")
	}
	if organizationID == uuid.Nil || aggregateID == uuid.Nil ||
		(aggregateType != coexistenceMediaMessageAggregate && aggregateType != coexistenceMediaReviewAggregate) {
		return errors.New("coexistence media tenant and aggregate are required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	job, err := p.claim(ctx, organizationID, &aggregateID, aggregateType)
	if err != nil || job == nil {
		return err
	}
	return p.processClaimed(ctx, organizationID, job)
}

func (p *CoexistenceMediaProcessor) listOrganizations(ctx context.Context) ([]uuid.UUID, error) {
	var organizationIDs []uuid.UUID
	if err := p.app.rootApp().DB.WithContext(ctx).
		Scopes(database.ExcludePlatformComplianceOrganizations).
		Model(&models.Organization{}).
		Order("id").
		Pluck("id", &organizationIDs).Error; err != nil {
		return nil, fmt.Errorf("list coexistence media organizations: %w", err)
	}
	return organizationIDs, nil
}

func (p *CoexistenceMediaProcessor) claim(
	ctx context.Context,
	organizationID uuid.UUID,
	aggregateID *uuid.UUID,
	aggregateType string,
) (*models.ScheduledJob, error) {
	var claimed *models.ScheduledJob
	err := p.app.WithCommittedTenantApp(organizationID, func(scoped *App) error {
		now := p.now().UTC()
		staleBefore := now.Add(-p.lease)
		db := scoped.DB.WithContext(ctx)

		terminalQuery := db.Model(&models.ScheduledJob{}).
			Where(
				"organization_id = ? AND kind = ? AND run_at <= ? AND attempts >= max_attempts AND status IN ?",
				organizationID,
				coexistenceMediaHydrationJobKind,
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
		if aggregateID != nil {
			terminalQuery = terminalQuery.Where("aggregate_id = ? AND aggregate_type = ?", *aggregateID, aggregateType)
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

		query := db.Clauses(clause.Locking{
			Strength: "UPDATE",
			Options:  "SKIP LOCKED",
		}).Where(
			"organization_id = ? AND kind = ? AND run_at <= ? AND attempts < max_attempts",
			organizationID,
			coexistenceMediaHydrationJobKind,
			now,
		).Where(
			"status = ? OR (status = ? AND (locked_at IS NULL OR locked_at < ?))",
			models.ScheduledJobStatusPending,
			models.ScheduledJobStatusProcessing,
			staleBefore,
		)
		if aggregateID != nil {
			query = query.Where("aggregate_id = ? AND aggregate_type = ?", *aggregateID, aggregateType)
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
		update := db.Model(&models.ScheduledJob{}).
			Where(
				"id = ? AND organization_id = ? AND kind = ?",
				candidate.ID,
				organizationID,
				coexistenceMediaHydrationJobKind,
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
			return errors.New("coexistence media claim was lost")
		}

		candidate.Status = models.ScheduledJobStatusProcessing
		candidate.LockedAt = &now
		candidate.LockedBy = p.workerID
		candidate.Version++
		claimed = &candidate
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("claim coexistence media for tenant %s: %w", organizationID, err)
	}
	return claimed, nil
}

func (p *CoexistenceMediaProcessor) processClaimed(
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
				organizationID,
				job,
				fmt.Errorf("panic in coexistence media hydration: %v", recovered),
			)
		}
	}()

	processTimeout := p.processTimeout
	if processTimeout <= 0 {
		processTimeout = defaultCoexistenceMediaProcessTimeout
	}
	processCtx, cancelProcess := context.WithTimeout(ctx, processTimeout)
	defer cancelProcess()

	work, err := p.loadWork(processCtx, organizationID, job)
	if err != nil {
		return p.fail(organizationID, job, err)
	}
	if work.Skip {
		if work.CleanupSafe {
			if cleanupErr := p.deleteDiscardedMedia(
				organizationID,
				*job.AggregateID,
				work.CleanupKey,
			); cleanupErr != nil {
				return p.fail(
					organizationID,
					job,
					fmt.Errorf("remove discarded coexistence media object: %w", cleanupErr),
				)
			}
		}
		if err := p.complete(organizationID, job); err != nil {
			return p.fail(
				organizationID,
				job,
				fmt.Errorf("complete skipped coexistence media %s: %w", job.ID, err),
			)
		}
		return nil
	}

	// Reacquire the live account and hold its shared lifecycle lock across both
	// Meta reads. Object storage remains outside that transaction so a slow put
	// cannot delay offboarding; final persistence rechecks the exact ciphertext
	// generation captured by this provider fence.
	data, err := p.downloadActiveCoexistenceMedia(processCtx, organizationID, work)
	if err != nil {
		if errors.Is(err, whatsappaccount.ErrOutboundInactive) {
			// The provider callback cannot have started when the lifecycle gate
			// returns this sentinel. Complete the obsolete job without retrying.
			if completeErr := p.complete(organizationID, job); completeErr != nil {
				return p.fail(
					organizationID,
					job,
					fmt.Errorf("complete inactive coexistence media %s: %w", job.ID, completeErr),
				)
			}
			return nil
		}
		return p.fail(organizationID, job, err)
	}
	filename, err := coexistenceMediaFilename(
		coexistenceMediaStorageRevisionID(work.AggregateID, work.Revision),
		work.MediaID,
	)
	if err != nil {
		return p.fail(organizationID, job, err)
	}
	mediaURL, err := p.app.rootApp().saveTenantMedia(
		processCtx,
		organizationID,
		path.Join("messages", "coexistence"),
		"coexistence",
		filename,
		data,
		work.MimeType,
	)
	if err != nil {
		return p.fail(organizationID, job, err)
	}

	var persisted coexistenceMediaPersistResult
	if work.AggregateType == coexistenceMediaReviewAggregate {
		persisted, err = p.persistStagedHydration(
			organizationID, job, work.Account.ID, strings.TrimSpace(work.Account.PhoneID),
			work.AccessTokenGeneration, work.AggregateID, work.MediaID, work.Revision, mediaURL,
		)
	} else {
		persisted, err = p.persistHydration(
			organizationID, job, work.Account.ID, strings.TrimSpace(work.Account.PhoneID),
			work.AccessTokenGeneration, work.AggregateID, work.MediaID, work.Revision, mediaURL,
		)
	}
	if err != nil {
		return p.fail(organizationID, job, err)
	}
	if persisted.CleanupSafe {
		if cleanupErr := p.deleteDiscardedMedia(
			organizationID,
			work.AggregateID,
			mediaURL,
		); cleanupErr != nil {
			// persistHydration renewed the claim and copied its new version into
			// job. Failing with that exact generation makes deletion retryable
			// without allowing a stale owner to reset somebody else's lease.
			return p.fail(
				organizationID,
				job,
				fmt.Errorf("remove discarded coexistence media object: %w", cleanupErr),
			)
		}
	}
	if err := p.complete(organizationID, job); err != nil {
		return p.fail(
			organizationID,
			job,
			fmt.Errorf("complete coexistence media %s: %w", job.ID, err),
		)
	}
	if !persisted.Updated {
		p.app.Log.Info(
			"Discarded superseded coexistence media hydration",
			"organization_id", organizationID,
			"aggregate_type", work.AggregateType,
			"aggregate_id", work.AggregateID,
			"reason", persisted.Reason,
		)
	}
	return nil
}

// downloadActiveCoexistenceMedia binds both Meta requests to the current
// account row. If a disconnect/rotation commits first, the provider sees either
// no request or the new credential; if this shared lock wins, the lifecycle
// writer waits until both provider reads have completed.
func (p *CoexistenceMediaProcessor) downloadActiveCoexistenceMedia(
	ctx context.Context,
	organizationID uuid.UUID,
	work *coexistenceMediaWork,
) ([]byte, error) {
	if p == nil || p.app == nil || work == nil ||
		organizationID == uuid.Nil || work.Account.ID == uuid.Nil {
		return nil, whatsappaccount.ErrOutboundInactive
	}
	var data []byte
	err := p.app.WithCommittedTenantApp(organizationID, func(scoped *App) error {
		lockedAccount, err := whatsappaccount.LockAndLoadActiveForOutbound(
			scoped.DB.WithContext(ctx),
			organizationID,
			work.Account.ID,
		)
		if err != nil {
			return err
		}
		if !lockedAccount.IsSMB ||
			strings.TrimSpace(lockedAccount.PhoneID) != strings.TrimSpace(work.Account.PhoneID) {
			return whatsappaccount.ErrOutboundInactive
		}

		// Capture the at-rest generation before runtime preparation decrypts it.
		work.AccessTokenGeneration = sha256.Sum256([]byte(lockedAccount.AccessToken))
		if err := scoped.prepareWhatsAppAccountForRuntime(lockedAccount); err != nil {
			return err
		}
		work.Account = *lockedAccount
		client := scoped.whatsAppClient()
		mediaURL, err := client.GetMediaURL(ctx, work.MediaID, lockedAccount.ToWAAccount())
		if err != nil {
			return fmt.Errorf("failed to get media URL: %w", err)
		}
		data, err = client.DownloadMedia(ctx, mediaURL, lockedAccount.AccessToken)
		if err != nil {
			return fmt.Errorf("failed to download media: %w", err)
		}
		return nil
	})
	return data, err
}

func (p *CoexistenceMediaProcessor) deleteDiscardedMedia(
	organizationID, messageID uuid.UUID,
	key string,
) error {
	cleanupCtx, cancelCleanup := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelCleanup()
	if err := p.app.rootApp().deleteTenantMedia(cleanupCtx, organizationID, key); err != nil {
		return fmt.Errorf("delete media for message %s: %w", messageID, err)
	}
	return nil
}

func (p *CoexistenceMediaProcessor) loadWork(
	ctx context.Context,
	organizationID uuid.UUID,
	job *models.ScheduledJob,
) (*coexistenceMediaWork, error) {
	if err := validateCoexistenceMediaJobIdentity(job, organizationID); err != nil {
		return nil, err
	}
	if job.AggregateType == coexistenceMediaReviewAggregate {
		return p.loadStagedWork(ctx, organizationID, job)
	}
	accountID, err := coexistenceMediaPayloadUUID(job.Payload, "account_id")
	if err != nil {
		return nil, err
	}
	messageID, err := coexistenceMediaPayloadUUID(job.Payload, "message_id")
	if err != nil {
		return nil, err
	}
	if messageID != *job.AggregateID {
		return nil, errors.New("coexistence media aggregate does not match its payload")
	}
	phoneNumberID := coexistenceMediaPayloadString(job.Payload, "phone_number_id")
	wamid := coexistenceMediaPayloadString(job.Payload, "wamid")
	mediaID := coexistenceMediaPayloadString(job.Payload, "media_id")
	revision := coexistenceMediaPayloadString(job.Payload, "media_revision")

	cleanupKey, err := p.app.rootApp().coexistenceMediaStorageKey(
		organizationID,
		coexistenceMediaStorageRevisionID(messageID, revision),
		mediaID,
	)
	if err != nil {
		return nil, err
	}
	work := &coexistenceMediaWork{
		AggregateType: coexistenceMediaMessageAggregate,
		AggregateID:   messageID,
		MediaID:       mediaID,
		Revision:      revision,
		CleanupKey:    cleanupKey,
	}
	var committedJob *models.ScheduledJob
	err = p.app.WithCommittedTenantApp(organizationID, func(scoped *App) error {
		db := scoped.DB.WithContext(ctx)
		accountErr := db.Where(
			"id = ? AND organization_id = ? AND BTRIM(phone_id) = BTRIM(?)",
			accountID,
			organizationID,
			phoneNumberID,
		).First(&work.Account).Error
		accountFound := accountErr == nil
		if accountErr != nil && !errors.Is(accountErr, gorm.ErrRecordNotFound) {
			return fmt.Errorf("load coexistence media account: %w", accountErr)
		}
		now := p.now().UTC()
		accountUsable := accountFound && work.Account.IsSMB &&
			strings.TrimSpace(work.Account.Status) == "active" &&
			(work.Account.AccessTokenExpiresAt == nil ||
				work.Account.AccessTokenExpiresAt.After(now))
		// Match final persistence/webhook order: message then exact job claim.
		// The committed snapshot fences both a provider download and any cleanup
		// decision, without holding database locks during external I/O.
		messageErr := db.Clauses(clause.Locking{Strength: "UPDATE"}).Where(
			"id = ? AND organization_id = ?",
			messageID,
			organizationID,
		).First(&work.Message).Error
		messageFound := messageErr == nil
		if messageErr != nil && !errors.Is(messageErr, gorm.ErrRecordNotFound) {
			return fmt.Errorf("load coexistence media message: %w", messageErr)
		}
		if messageFound && work.Message.WhatsAppMessageID != wamid {
			return errors.New("coexistence media message no longer matches its claimed WAMID")
		}
		if messageFound {
			work.MimeType = work.Message.MediaMimeType
		}
		storedJob, err := p.lockCoexistenceMediaClaim(db, organizationID, job)
		if err != nil {
			return err
		}
		committedJob = storedJob
		cleanupOnly, _ := storedJob.Payload["cleanup_only"].(bool)
		currentMediaID, currentRevision, supported := coexistenceMediaRevision(&work.Message)
		hydratedMediaID := coexistenceMediaMetadataString(work.Message.Metadata, coexistenceMediaHydratedIDMetadataKey)
		revoked, _ := work.Message.Metadata[coexistenceMediaRevokedMetadataKey].(bool)

		// A lifecycle webhook disconnects the account before this worker can use
		// its credentials. Treat that as a terminal no-op, while re-deriving and
		// removing any unreferenced object left by an earlier failed cleanup.
		switch {
		case !messageFound:
			work.SkipReason = "message_unavailable"
		case cleanupOnly:
			work.SkipReason = "discard_cleanup"
		case !accountUsable:
			if accountFound {
				work.SkipReason = "account_inactive"
			} else {
				work.SkipReason = "account_unavailable"
			}
		case revoked:
			work.SkipReason = "message_revoked"
		case !supported:
			work.SkipReason = "message_not_media"
		case currentMediaID != mediaID || currentRevision != revision:
			work.SkipReason = "media_superseded"
		case work.Message.MediaURL != "" &&
			(hydratedMediaID == "" || hydratedMediaID == mediaID):
			work.SkipReason = "already_hydrated"
		}
		work.Skip = work.SkipReason != ""
		if work.Skip {
			work.CleanupSafe = strings.TrimSpace(work.Message.MediaURL) != cleanupKey
			return p.recordCoexistenceMediaDiscard(db, storedJob)
		}
		// Capture before runtime preparation replaces ciphertext with plaintext.
		// Cleanup-only jobs never decrypt or reuse even freshly rotated credentials.
		work.AccessTokenGeneration = sha256.Sum256([]byte(work.Account.AccessToken))
		if err := scoped.prepareWhatsAppAccountForRuntime(&work.Account); err != nil {
			return fmt.Errorf("prepare coexistence media account: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	*job = *committedJob
	return work, nil
}

func (p *CoexistenceMediaProcessor) loadStagedWork(
	ctx context.Context,
	organizationID uuid.UUID,
	job *models.ScheduledJob,
) (*coexistenceMediaWork, error) {
	accountID, err := coexistenceMediaPayloadUUID(job.Payload, "account_id")
	if err != nil {
		return nil, err
	}
	eventID, err := coexistenceMediaPayloadUUID(job.Payload, "event_id")
	if err != nil || eventID != *job.AggregateID {
		return nil, errors.New("staged coexistence media aggregate does not match its payload")
	}
	phoneNumberID := coexistenceMediaPayloadString(job.Payload, "phone_number_id")
	wamid := coexistenceMediaPayloadString(job.Payload, "wamid")
	mediaID := coexistenceMediaPayloadString(job.Payload, "media_id")
	revision := coexistenceMediaPayloadString(job.Payload, "media_revision")
	generation, ok := coexistenceMediaPayloadUint64(job.Payload, "media_generation")
	if !ok || generation == 0 {
		return nil, errors.New("staged coexistence media generation is invalid")
	}
	cleanupKey, err := p.app.rootApp().coexistenceMediaStorageKey(
		organizationID,
		coexistenceMediaStorageRevisionID(eventID, revision),
		mediaID,
	)
	if err != nil {
		return nil, err
	}
	work := &coexistenceMediaWork{
		AggregateType: coexistenceMediaReviewAggregate,
		AggregateID:   eventID,
		MediaID:       mediaID,
		Revision:      revision,
		CleanupKey:    cleanupKey,
	}
	var committedJob *models.ScheduledJob
	err = p.app.WithCommittedTenantApp(organizationID, func(scoped *App) error {
		db := scoped.DB.WithContext(ctx)
		accountErr := db.Where(
			"id = ? AND organization_id = ? AND BTRIM(phone_id) = BTRIM(?)",
			accountID,
			organizationID,
			phoneNumberID,
		).First(&work.Account).Error
		accountFound := accountErr == nil
		if accountErr != nil && !errors.Is(accountErr, gorm.ErrRecordNotFound) {
			return fmt.Errorf("load staged coexistence media account: %w", accountErr)
		}
		now := p.now().UTC()
		accountUsable := accountFound && work.Account.IsSMB &&
			strings.TrimSpace(work.Account.Status) == "active" &&
			(work.Account.AccessTokenExpiresAt == nil || work.Account.AccessTokenExpiresAt.After(now))

		eventErr := db.Clauses(clause.Locking{Strength: "UPDATE"}).Where(
			"id = ? AND organization_id = ?",
			eventID,
			organizationID,
		).First(&work.StagedEvent).Error
		eventFound := eventErr == nil
		if eventErr != nil && !errors.Is(eventErr, gorm.ErrRecordNotFound) {
			return fmt.Errorf("load staged coexistence media event: %w", eventErr)
		}
		if eventFound && (work.StagedEvent.Protocol != models.WhatsAppIdentityReviewInboundProtocol ||
			work.StagedEvent.EventType != models.WhatsAppIdentityReviewPendingEvent ||
			work.StagedEvent.ReviewHoldID == nil || work.StagedEvent.ProviderEventID != wamid) {
			return errors.New("staged coexistence media event authority changed")
		}
		storedJob, err := p.lockCoexistenceMediaClaim(db, organizationID, job)
		if err != nil {
			return err
		}
		committedJob = storedJob
		cleanupOnly, _ := storedJob.Payload["cleanup_only"].(bool)
		currentMediaID, currentRevision, currentMime, currentGeneration, supported :=
			coexistenceStagedMediaRevision(&work.StagedEvent)
		work.MimeType = currentMime
		hydratedMediaID := coexistenceMediaPayloadString(work.StagedEvent.Payload, "media_hydrated_id")
		mediaURL := coexistenceMediaPayloadString(work.StagedEvent.Payload, "media_url")

		switch {
		case !eventFound:
			work.SkipReason = "review_event_unavailable"
		case cleanupOnly:
			work.SkipReason = "discard_cleanup"
		case !accountUsable:
			if accountFound {
				work.SkipReason = "account_inactive"
			} else {
				work.SkipReason = "account_unavailable"
			}
		case !supported:
			work.SkipReason = "review_event_not_media"
		case currentMediaID != mediaID || currentRevision != revision || currentGeneration != generation:
			work.SkipReason = "media_superseded"
		case mediaURL != "" && (hydratedMediaID == "" || hydratedMediaID == mediaID):
			work.SkipReason = "already_hydrated"
		}
		work.Skip = work.SkipReason != ""
		if work.Skip {
			work.CleanupSafe = mediaURL != cleanupKey
			return p.recordCoexistenceMediaDiscard(db, storedJob)
		}
		work.AccessTokenGeneration = sha256.Sum256([]byte(work.Account.AccessToken))
		if err := scoped.prepareWhatsAppAccountForRuntime(&work.Account); err != nil {
			return fmt.Errorf("prepare staged coexistence media account: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	*job = *committedJob
	return work, nil
}

func (p *CoexistenceMediaProcessor) lockCoexistenceMediaClaim(
	db *gorm.DB, organizationID uuid.UUID, job *models.ScheduledJob,
) (*models.ScheduledJob, error) {
	var stored models.ScheduledJob
	if err := db.Clauses(clause.Locking{Strength: "UPDATE"}).Where(
		"id = ? AND organization_id = ? AND kind = ?", job.ID, organizationID, coexistenceMediaHydrationJobKind,
	).First(&stored).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, errCoexistenceMediaLeaseLost
		}
		return nil, fmt.Errorf("lock coexistence media job: %w", err)
	}
	lease := p.lease
	if lease <= 0 {
		lease = defaultCoexistenceMediaLease
	}
	if stored.Status != models.ScheduledJobStatusProcessing || stored.LockedBy != p.workerID ||
		stored.Attempts != job.Attempts || stored.Version != job.Version || stored.LockedAt == nil ||
		stored.LockedAt.Before(p.now().UTC().Add(-lease)) {
		return nil, errCoexistenceMediaLeaseLost
	}
	if err := validateCoexistenceMediaJobIdentity(&stored, organizationID); err != nil {
		return nil, err
	}
	if stored.IdempotencyKey != job.IdempotencyKey || *stored.AggregateID != *job.AggregateID ||
		coexistenceMediaPayloadString(stored.Payload, "phone_number_id") != coexistenceMediaPayloadString(job.Payload, "phone_number_id") {
		return nil, errors.New("coexistence media claim payload changed")
	}
	return &stored, nil
}

// Record a terminal no-download decision while message and claim are locked.
// The marker retains the original revision/key identity through cleanup failure,
// credential rotation and webhook replay. Publish this generation only on commit.
func (p *CoexistenceMediaProcessor) recordCoexistenceMediaDiscard(db *gorm.DB, job *models.ScheduledJob) error {
	payload := cloneMessageMetadata(job.Payload)
	payload["cleanup_only"] = true
	now := p.now().UTC()
	result := db.Model(&models.ScheduledJob{}).Where(
		"id = ? AND organization_id = ? AND status = ? AND locked_by = ? AND attempts = ? AND version = ?",
		job.ID, job.OrganizationID, models.ScheduledJobStatusProcessing, p.workerID, job.Attempts, job.Version,
	).Updates(map[string]any{"payload": payload, "locked_at": now, "version": gorm.Expr("version + 1")})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return errCoexistenceMediaLeaseLost
	}
	job.Payload = payload
	job.LockedAt = &now
	job.Version++
	return nil
}

func (p *CoexistenceMediaProcessor) persistHydration(
	organizationID uuid.UUID,
	job *models.ScheduledJob,
	accountID uuid.UUID,
	phoneNumberID string,
	accessTokenGeneration [sha256.Size]byte,
	messageID uuid.UUID,
	mediaID, revision, mediaURL string,
) (coexistenceMediaPersistResult, error) {
	result := coexistenceMediaPersistResult{}
	if job == nil || accountID == uuid.Nil || messageID == uuid.Nil ||
		strings.TrimSpace(phoneNumberID) == "" || strings.TrimSpace(mediaURL) == "" {
		return result, errors.New("coexistence media persistence identity is incomplete")
	}
	if err := validateCoexistenceMediaJobIdentity(job, organizationID); err != nil {
		return result, err
	}
	if *job.AggregateID != messageID ||
		coexistenceMediaPayloadString(job.Payload, "account_id") != accountID.String() ||
		coexistenceMediaPayloadString(job.Payload, "phone_number_id") != phoneNumberID ||
		coexistenceMediaPayloadString(job.Payload, "media_id") != mediaID ||
		coexistenceMediaPayloadString(job.Payload, "media_revision") != revision {
		return result, errors.New("coexistence media persistence does not match its claimed revision")
	}
	expectedKey, err := p.app.rootApp().coexistenceMediaStorageKey(
		organizationID, coexistenceMediaStorageRevisionID(messageID, revision), mediaID,
	)
	if err != nil {
		return result, err
	}
	if mediaURL != expectedKey {
		return result, errors.New("coexistence media object does not match its claimed revision")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var committedJob *models.ScheduledJob
	err = p.app.WithCommittedTenantApp(organizationID, func(scoped *App) error {
		db := scoped.DB.WithContext(ctx)
		// Match webhook mutation order: message first, then its scheduled job.
		// Holding both rows prevents an edit from changing the provider media ID
		// between the final state check and the media_url update.
		var message models.Message
		if err := db.Clauses(clause.Locking{Strength: "UPDATE"}).Where(
			"id = ? AND organization_id = ?",
			messageID,
			organizationID,
		).First(&message).Error; err != nil {
			return fmt.Errorf("lock coexistence media message: %w", err)
		}

		storedJob, err := p.lockCoexistenceMediaClaim(db, organizationID, job)
		if err != nil {
			return err
		}
		if message.WhatsAppMessageID != coexistenceMediaPayloadString(storedJob.Payload, "wamid") {
			return errors.New("coexistence media message no longer matches its claimed WAMID")
		}
		committedJob = storedJob
		now := p.now().UTC()
		// Renew the exact claim while both rows are locked. This leaves a full
		// lease window for bounded cleanup and the terminal job transition.
		renew := db.Model(&models.ScheduledJob{}).Where(
			"id = ? AND organization_id = ? AND kind = ? AND status = ? AND locked_by = ? AND attempts = ? AND version = ?",
			storedJob.ID,
			organizationID,
			coexistenceMediaHydrationJobKind,
			models.ScheduledJobStatusProcessing,
			p.workerID,
			storedJob.Attempts,
			storedJob.Version,
		).Updates(map[string]any{
			"locked_at": now,
			"version":   gorm.Expr("version + 1"),
		})
		if renew.Error != nil {
			return renew.Error
		}
		if renew.RowsAffected != 1 {
			return errCoexistenceMediaLeaseLost
		}
		storedJob.LockedAt = &now
		storedJob.Version++
		discard := func(reason string) error {
			result.CleanupSafe = strings.TrimSpace(message.MediaURL) != mediaURL
			result.Reason = reason
			return p.recordCoexistenceMediaDiscard(db, storedJob)
		}
		if cleanupOnly, _ := storedJob.Payload["cleanup_only"].(bool); cleanupOnly {
			return discard("discard_cleanup")
		}

		var currentAccount models.WhatsAppAccount
		accountErr := db.Clauses(clause.Locking{Strength: "SHARE"}).Where(
			"id = ? AND organization_id = ? AND BTRIM(phone_id) = BTRIM(?)",
			accountID,
			organizationID,
			phoneNumberID,
		).First(&currentAccount).Error
		if errors.Is(accountErr, gorm.ErrRecordNotFound) {
			return discard("account_unavailable")
		}
		if accountErr != nil {
			return fmt.Errorf("lock coexistence media account: %w", accountErr)
		}
		if sha256.Sum256([]byte(currentAccount.AccessToken)) != accessTokenGeneration {
			return discard("credentials_rotated")
		}
		if !currentAccount.IsSMB || strings.TrimSpace(currentAccount.Status) != "active" ||
			(currentAccount.AccessTokenExpiresAt != nil &&
				!currentAccount.AccessTokenExpiresAt.After(now)) {
			return discard("account_inactive")
		}

		currentMediaID, currentRevision, supported := coexistenceMediaRevision(&message)
		hydratedMediaID := coexistenceMediaMetadataString(
			message.Metadata,
			coexistenceMediaHydratedIDMetadataKey,
		)
		revoked, _ := message.Metadata[coexistenceMediaRevokedMetadataKey].(bool)
		if revoked {
			return discard("message_revoked")
		}
		if !supported {
			return discard("message_not_media")
		}
		if currentMediaID != mediaID || currentRevision != revision {
			return discard("media_superseded")
		}
		if message.MediaURL != "" && (hydratedMediaID == "" || hydratedMediaID == mediaID) {
			return discard("already_hydrated")
		}

		metadata := cloneMessageMetadata(message.Metadata)
		metadata[coexistenceMediaHydratedIDMetadataKey] = mediaID
		update := db.Model(&models.Message{}).Where(
			"id = ? AND organization_id = ?",
			message.ID,
			organizationID,
		).Updates(map[string]any{
			"media_url": mediaURL,
			"metadata":  metadata,
		})
		if update.Error != nil {
			return update.Error
		}
		if update.RowsAffected != 1 {
			return errors.New("coexistence media message update was lost")
		}
		result.Updated = true
		result.Reason = "media_ready"
		messageIDCopy := message.ID
		contactIDCopy := message.ContactID
		scoped.publishRealtimeEvent(queue.RealtimeEvent{
			OrganizationID: organizationID,
			Kind:           queue.RealtimeEventConversationChanged,
			ContactID:      &contactIDCopy,
			MessageID:      &messageIDCopy,
			Status:         "media_ready",
			EventCount:     1,
			OccurredAt:     p.now().UTC(),
		}, nil)
		return nil
	})
	if err != nil {
		return result, fmt.Errorf("persist coexistence media hydration for job %s: %w", job.ID, err)
	}
	*job = *committedJob
	return result, nil
}

func (p *CoexistenceMediaProcessor) persistStagedHydration(
	organizationID uuid.UUID,
	job *models.ScheduledJob,
	accountID uuid.UUID,
	phoneNumberID string,
	accessTokenGeneration [sha256.Size]byte,
	eventID uuid.UUID,
	mediaID, revision, mediaURL string,
) (coexistenceMediaPersistResult, error) {
	result := coexistenceMediaPersistResult{}
	if job == nil || accountID == uuid.Nil || eventID == uuid.Nil ||
		strings.TrimSpace(phoneNumberID) == "" || strings.TrimSpace(mediaURL) == "" {
		return result, errors.New("staged coexistence media persistence identity is incomplete")
	}
	if err := validateCoexistenceMediaJobIdentity(job, organizationID); err != nil {
		return result, err
	}
	if job.AggregateType != coexistenceMediaReviewAggregate || *job.AggregateID != eventID ||
		coexistenceMediaPayloadString(job.Payload, "account_id") != accountID.String() ||
		coexistenceMediaPayloadString(job.Payload, "phone_number_id") != phoneNumberID ||
		coexistenceMediaPayloadString(job.Payload, "media_id") != mediaID ||
		coexistenceMediaPayloadString(job.Payload, "media_revision") != revision {
		return result, errors.New("staged coexistence media persistence does not match its claimed revision")
	}
	expectedKey, err := p.app.rootApp().coexistenceMediaStorageKey(
		organizationID, coexistenceMediaStorageRevisionID(eventID, revision), mediaID,
	)
	if err != nil {
		return result, err
	}
	if mediaURL != expectedKey {
		return result, errors.New("staged coexistence media object does not match its claimed revision")
	}
	expectedGeneration, _ := coexistenceMediaPayloadUint64(job.Payload, "media_generation")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var committedJob *models.ScheduledJob
	err = p.app.WithCommittedTenantApp(organizationID, func(scoped *App) error {
		db := scoped.DB.WithContext(ctx)
		var event models.InboundEvent
		if err := db.Clauses(clause.Locking{Strength: "UPDATE"}).Where(
			"id = ? AND organization_id = ?",
			eventID,
			organizationID,
		).First(&event).Error; err != nil {
			return fmt.Errorf("lock staged coexistence media event: %w", err)
		}
		storedJob, err := p.lockCoexistenceMediaClaim(db, organizationID, job)
		if err != nil {
			return err
		}
		committedJob = storedJob
		now := p.now().UTC()
		renew := db.Model(&models.ScheduledJob{}).Where(
			"id = ? AND organization_id = ? AND kind = ? AND status = ? AND locked_by = ? AND attempts = ? AND version = ?",
			storedJob.ID, organizationID, coexistenceMediaHydrationJobKind,
			models.ScheduledJobStatusProcessing, p.workerID, storedJob.Attempts, storedJob.Version,
		).Updates(map[string]any{"locked_at": now, "version": gorm.Expr("version + 1")})
		if renew.Error != nil {
			return renew.Error
		}
		if renew.RowsAffected != 1 {
			return errCoexistenceMediaLeaseLost
		}
		storedJob.LockedAt = &now
		storedJob.Version++
		discard := func(reason string) error {
			result.CleanupSafe = coexistenceMediaPayloadString(event.Payload, "media_url") != mediaURL
			result.Reason = reason
			return p.recordCoexistenceMediaDiscard(db, storedJob)
		}
		if cleanupOnly, _ := storedJob.Payload["cleanup_only"].(bool); cleanupOnly {
			return discard("discard_cleanup")
		}
		if event.Protocol != models.WhatsAppIdentityReviewInboundProtocol ||
			event.EventType != models.WhatsAppIdentityReviewPendingEvent || event.ReviewHoldID == nil ||
			event.ProviderEventID != coexistenceMediaPayloadString(storedJob.Payload, "wamid") {
			return discard("review_event_unavailable")
		}

		var currentAccount models.WhatsAppAccount
		accountErr := db.Clauses(clause.Locking{Strength: "SHARE"}).Where(
			"id = ? AND organization_id = ? AND BTRIM(phone_id) = BTRIM(?)",
			accountID, organizationID, phoneNumberID,
		).First(&currentAccount).Error
		if errors.Is(accountErr, gorm.ErrRecordNotFound) {
			return discard("account_unavailable")
		}
		if accountErr != nil {
			return fmt.Errorf("lock staged coexistence media account: %w", accountErr)
		}
		if sha256.Sum256([]byte(currentAccount.AccessToken)) != accessTokenGeneration {
			return discard("credentials_rotated")
		}
		if !currentAccount.IsSMB || strings.TrimSpace(currentAccount.Status) != "active" ||
			(currentAccount.AccessTokenExpiresAt != nil && !currentAccount.AccessTokenExpiresAt.After(now)) {
			return discard("account_inactive")
		}
		currentID, currentRevision, _, currentGeneration, supported := coexistenceStagedMediaRevision(&event)
		if !supported {
			return discard("review_event_not_media")
		}
		if currentID != mediaID || currentRevision != revision || currentGeneration != expectedGeneration {
			return discard("media_superseded")
		}
		hydratedMediaID := coexistenceMediaPayloadString(event.Payload, "media_hydrated_id")
		currentURL := coexistenceMediaPayloadString(event.Payload, "media_url")
		if currentURL != "" && (hydratedMediaID == "" || hydratedMediaID == mediaID) {
			return discard("already_hydrated")
		}

		payload := cloneMessageMetadata(event.Payload)
		payload["media_url"] = mediaURL
		payload["media_hydrated_id"] = mediaID
		payload["media_status"] = "ready"
		update := db.Model(&models.InboundEvent{}).Where(
			"id = ? AND organization_id = ? AND protocol = ? AND event_type = ? AND review_hold_id IS NOT NULL",
			event.ID,
			organizationID,
			models.WhatsAppIdentityReviewInboundProtocol,
			models.WhatsAppIdentityReviewPendingEvent,
		).Update("payload", payload)
		if update.Error != nil {
			return update.Error
		}
		if update.RowsAffected != 1 {
			return errors.New("staged coexistence media event update was lost")
		}
		result.Updated = true
		result.Reason = "media_ready"
		return nil
	})
	if err != nil {
		return result, fmt.Errorf("persist staged coexistence media hydration for job %s: %w", job.ID, err)
	}
	*job = *committedJob
	return result, nil
}

func (p *CoexistenceMediaProcessor) complete(
	organizationID uuid.UUID,
	job *models.ScheduledJob,
) error {
	if job == nil {
		return errors.New("coexistence media completion requires a job")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return p.app.WithCommittedTenantApp(organizationID, func(scoped *App) error {
		now := p.now().UTC()
		lease := p.lease
		if lease <= 0 {
			lease = defaultCoexistenceMediaLease
		}
		result := scoped.DB.WithContext(ctx).Model(&models.ScheduledJob{}).
			Where(
				"id = ? AND organization_id = ? AND kind = ? AND status = ? AND locked_by = ? AND attempts = ? AND version = ? AND locked_at IS NOT NULL AND locked_at >= ?",
				job.ID,
				organizationID,
				coexistenceMediaHydrationJobKind,
				models.ScheduledJobStatusProcessing,
				p.workerID,
				job.Attempts,
				job.Version,
				now.Add(-lease),
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
			return errors.New("coexistence media completion lost its lease")
		}
		return nil
	})
}

func (p *CoexistenceMediaProcessor) fail(
	organizationID uuid.UUID,
	job *models.ScheduledJob,
	cause error,
) error {
	safeCause := errors.New(coexistenceMediaErrorText(cause))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	updateErr := p.app.WithCommittedTenantApp(organizationID, func(scoped *App) error {
		status := models.ScheduledJobStatusPending
		maxAttempts := job.MaxAttempts
		if maxAttempts <= 0 {
			maxAttempts = defaultCoexistenceMediaMaxAttempts
		}
		updates := map[string]any{
			"status":     status,
			"locked_at":  nil,
			"locked_by":  "",
			"last_error": coexistenceMediaErrorText(safeCause),
			"version":    gorm.Expr("version + 1"),
		}
		if job.Attempts >= maxAttempts {
			status = models.ScheduledJobStatusFailed
			updates["status"] = status
		} else {
			updates["run_at"] = p.now().UTC().Add(inboundContinuationBackoff(job.Attempts))
		}
		result := scoped.DB.WithContext(ctx).Model(&models.ScheduledJob{}).
			Where(
				"id = ? AND organization_id = ? AND kind = ? AND status = ? AND locked_by = ? AND attempts = ? AND version = ?",
				job.ID,
				organizationID,
				coexistenceMediaHydrationJobKind,
				models.ScheduledJobStatusProcessing,
				p.workerID,
				job.Attempts,
				job.Version,
			).
			Updates(updates)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return errors.New("coexistence media failure update lost its lease")
		}
		return nil
	})
	if updateErr != nil {
		return errors.Join(safeCause, errors.New(coexistenceMediaErrorText(updateErr)))
	}
	return fmt.Errorf("coexistence media hydration %s: %w", job.ID, safeCause)
}

func coexistenceMediaErrorText(err error) string {
	if err == nil {
		return ""
	}
	value := coexistenceMediaErrorURLPattern.ReplaceAllString(err.Error(), "<redacted_url>")
	const maxLength = 2000
	if len(value) > maxLength {
		return value[:maxLength]
	}
	return value
}

func coexistenceMediaPayloadString(payload models.JSONB, key string) string {
	value, _ := payload[key].(string)
	return strings.TrimSpace(value)
}

func coexistenceMediaPayloadUUID(payload models.JSONB, key string) (uuid.UUID, error) {
	value := coexistenceMediaPayloadString(payload, key)
	parsed, err := uuid.Parse(value)
	if err != nil || parsed == uuid.Nil {
		return uuid.Nil, fmt.Errorf("coexistence media payload %s is invalid", key)
	}
	return parsed, nil
}

func coexistenceMediaPayloadUint64(payload models.JSONB, key string) (uint64, bool) {
	switch value := payload[key].(type) {
	case uint64:
		return value, true
	case uint:
		return uint64(value), true
	case int:
		if value > 0 {
			return uint64(value), true
		}
	case int64:
		if value > 0 {
			return uint64(value), true
		}
	case float64:
		if value > 0 && value <= float64(^uint64(0)) && value == float64(uint64(value)) {
			return uint64(value), true
		}
	case json.Number:
		parsed, err := strconv.ParseUint(string(value), 10, 64)
		return parsed, err == nil && parsed > 0
	case string:
		parsed, err := strconv.ParseUint(strings.TrimSpace(value), 10, 64)
		return parsed, err == nil && parsed > 0
	}
	return 0, false
}

func coexistenceMediaMetadataString(metadata models.JSONB, key string) string {
	value, _ := metadata[key].(string)
	return strings.TrimSpace(value)
}

func coexistenceStagedMediaRevision(
	event *models.InboundEvent,
) (mediaID, revision, mimeType string, generation uint64, supported bool) {
	if event == nil || event.Protocol != models.WhatsAppIdentityReviewInboundProtocol ||
		event.EventType != models.WhatsAppIdentityReviewPendingEvent || event.ReviewHoldID == nil {
		return "", "", "", 0, false
	}
	messageType := coexistenceMediaPayloadString(event.Payload, "message_type")
	switch models.MessageType(messageType) {
	case models.MessageTypeImage, models.MessageTypeDocument, models.MessageTypeVideo,
		models.MessageTypeAudio, models.MessageType("sticker"):
	default:
		return "", "", "", 0, false
	}
	mediaID = coexistenceMediaPayloadString(event.Payload, "media_id")
	mimeType = coexistenceMediaPayloadString(event.Payload, "media_mime_type")
	generation, supported = coexistenceMediaPayloadUint64(event.Payload, "media_generation")
	if mediaID == "" || !supported || generation == 0 {
		return "", "", "", 0, false
	}
	if revoked, _ := event.Payload["media_revoked"].(bool); revoked {
		return "", "", "", 0, false
	}
	revision = coexistenceStagedMediaRevisionDigest(event.Payload, generation)
	if stored := coexistenceMediaPayloadString(event.Payload, "media_revision"); stored != revision {
		return "", "", "", 0, false
	}
	return mediaID, revision, mimeType, generation, true
}

func coexistenceStagedMediaRevisionDigest(payload models.JSONB, generation uint64) string {
	encoded, _ := json.Marshal([]any{
		coexistenceMediaPayloadString(payload, "message_type"),
		coexistenceMediaPayloadString(payload, "media_id"),
		coexistenceMediaPayloadString(payload, "media_sha256"),
		coexistenceMediaPayloadString(payload, "media_mime_type"),
		coexistenceMediaPayloadString(payload, "media_filename"),
		coexistenceMediaPayloadString(payload, "content"),
		generation,
	})
	return fmt.Sprintf("%x", sha256.Sum256(encoded))
}

// coexistenceMediaRevision describes committed media content, not hydration
// progress. In particular URL/hydrated-ID/UpdatedAt must not change this fence.
func coexistenceMediaRevision(message *models.Message) (mediaID, revision string, supported bool) {
	if message == nil {
		return "", "", false
	}
	switch message.MessageType {
	case models.MessageTypeImage, models.MessageTypeDocument, models.MessageTypeVideo,
		models.MessageTypeAudio, models.MessageType("sticker"):
	default:
		return "", "", false
	}
	mediaID = coexistenceMediaMetadataString(message.Metadata, coexistenceMediaProviderIDMetadataKey)
	revoked, _ := message.Metadata[coexistenceMediaRevokedMetadataKey].(bool)
	if mediaID == "" || revoked {
		return "", "", false
	}
	editID, edited := message.Metadata["coexistence_edit_event_id"].(string)
	encoded, _ := json.Marshal([]any{
		string(message.MessageType), mediaID,
		coexistenceMediaMetadataString(message.Metadata, "coexistence_media_sha256"),
		message.MediaMimeType, message.MediaFilename, message.Content, edited, editID,
	})
	return mediaID, fmt.Sprintf("%x", sha256.Sum256(encoded)), true
}

// This UUID is ONLY an object-storage namespace, never a Message identity.
// Revision-specific keys prevent overlapping edits with the same provider media
// ID from overwriting or deleting each other's bytes through the stable writer.
func coexistenceMediaStorageRevisionID(messageID uuid.UUID, revision string) uuid.UUID {
	return uuid.NewSHA1(messageID, []byte("coexistence-media-revision:"+revision))
}

func coexistenceMediaJobKey(accountID uuid.UUID, wamid, mediaID, revision string) string {
	encoded, _ := json.Marshal([]string{wamid, mediaID, revision})
	return "coexistence-media-hydration:" + uuid.NewSHA1(accountID, encoded).String()
}

func coexistenceStagedMediaJobKey(accountID uuid.UUID, wamid, mediaID, revision string, generation uint64) string {
	encoded, _ := json.Marshal([]any{wamid, mediaID, revision, generation})
	return "coexistence-staged-media-hydration:" + uuid.NewSHA1(accountID, encoded).String()
}

// Jobs without a revision are deliberately not upgraded from mutable current
// message state. They fail closed through the existing bounded retry ledger.
func validateCoexistenceMediaJobIdentity(job *models.ScheduledJob, organizationID uuid.UUID) error {
	if job == nil || job.OrganizationID != organizationID || job.AggregateID == nil ||
		job.Kind != coexistenceMediaHydrationJobKind {
		return errors.New("coexistence media job identity is invalid")
	}
	accountID, err := coexistenceMediaPayloadUUID(job.Payload, "account_id")
	if err != nil {
		return err
	}
	wamid := coexistenceMediaPayloadString(job.Payload, "wamid")
	mediaID := coexistenceMediaPayloadString(job.Payload, "media_id")
	revision := coexistenceMediaPayloadString(job.Payload, "media_revision")
	if coexistenceMediaPayloadString(job.Payload, "phone_number_id") == "" || wamid == "" ||
		mediaID == "" || len(revision) != sha256.Size*2 {
		return errors.New("coexistence media payload lacks exact revision identity")
	}
	switch job.AggregateType {
	case coexistenceMediaMessageAggregate:
		messageID, err := coexistenceMediaPayloadUUID(job.Payload, "message_id")
		if err != nil || messageID != *job.AggregateID {
			return errors.New("coexistence media aggregate does not match its payload")
		}
		if job.IdempotencyKey != coexistenceMediaJobKey(accountID, wamid, mediaID, revision) {
			return errors.New("coexistence media job idempotency identity is invalid")
		}
	case coexistenceMediaReviewAggregate:
		eventID, err := coexistenceMediaPayloadUUID(job.Payload, "event_id")
		generation, generationOK := coexistenceMediaPayloadUint64(job.Payload, "media_generation")
		if err != nil || eventID != *job.AggregateID || !generationOK || generation == 0 {
			return errors.New("staged coexistence media aggregate does not match its payload")
		}
		if job.IdempotencyKey != coexistenceStagedMediaJobKey(accountID, wamid, mediaID, revision, generation) {
			return errors.New("staged coexistence media job idempotency identity is invalid")
		}
	default:
		return errors.New("coexistence media aggregate type is invalid")
	}
	return nil
}

// ensureCoexistenceMediaJob runs in the same tenant transaction that stores
// the message/media identifier. A webhook ACK therefore cannot race ahead of
// an undurable hydration request.
func (a *App) ensureCoexistenceMediaJob(
	account *models.WhatsAppAccount,
	messageID uuid.UUID,
) error {
	if a == nil || a.DB == nil || account == nil || account.ID == uuid.Nil ||
		account.OrganizationID == uuid.Nil || messageID == uuid.Nil {
		return errors.New("coexistence media job identity is incomplete")
	}

	var message models.Message
	if err := a.DB.Clauses(clause.Locking{Strength: "UPDATE"}).Where(
		"id = ? AND organization_id = ?",
		messageID,
		account.OrganizationID,
	).First(&message).Error; err != nil {
		return fmt.Errorf("load coexistence media message for enqueue: %w", err)
	}
	mediaID, revision, supported := coexistenceMediaRevision(&message)
	if !supported {
		return nil
	}
	hydratedMediaID := coexistenceMediaMetadataString(
		message.Metadata,
		coexistenceMediaHydratedIDMetadataKey,
	)
	if message.MediaURL != "" && (hydratedMediaID == "" || hydratedMediaID == mediaID) {
		return nil
	}

	now := time.Now().UTC()
	idempotencyKey := coexistenceMediaJobKey(account.ID, strings.TrimSpace(message.WhatsAppMessageID), mediaID, revision)
	payload := models.JSONB{
		"account_id":      account.ID.String(),
		"phone_number_id": strings.TrimSpace(account.PhoneID),
		"message_id":      message.ID.String(),
		"wamid":           strings.TrimSpace(message.WhatsAppMessageID),
		"media_id":        mediaID,
		"media_revision":  revision,
	}
	job := models.ScheduledJob{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: account.OrganizationID,
		Kind:           coexistenceMediaHydrationJobKind,
		AggregateType:  "message",
		AggregateID:    &message.ID,
		RunAt:          now,
		Status:         models.ScheduledJobStatusPending,
		MaxAttempts:    defaultCoexistenceMediaMaxAttempts,
		IdempotencyKey: idempotencyKey,
		Payload:        payload,
		Version:        1,
	}
	if err := a.DB.Clauses(clause.OnConflict{DoNothing: true}).Create(&job).Error; err != nil {
		return fmt.Errorf("enqueue coexistence media hydration: %w", err)
	}

	var existing models.ScheduledJob
	if err := a.DB.Where(
		"organization_id = ? AND idempotency_key = ?",
		account.OrganizationID,
		idempotencyKey,
	).First(&existing).Error; err != nil {
		return fmt.Errorf("load coexistence media hydration job: %w", err)
	}
	if validateCoexistenceMediaJobIdentity(&existing, account.OrganizationID) != nil ||
		*existing.AggregateID != message.ID ||
		coexistenceMediaPayloadString(existing.Payload, "account_id") != account.ID.String() ||
		coexistenceMediaPayloadString(existing.Payload, "phone_number_id") != strings.TrimSpace(account.PhoneID) ||
		coexistenceMediaPayloadString(existing.Payload, "wamid") != strings.TrimSpace(message.WhatsAppMessageID) ||
		coexistenceMediaPayloadString(existing.Payload, "media_id") != mediaID ||
		coexistenceMediaPayloadString(existing.Payload, "media_revision") != revision {
		return errors.New("coexistence media hydration idempotency key collision")
	}

	// Cleanup is a committed terminal decision for this exact revision. Replays
	// must not turn its cleanup retries (or exhausted/manual-review ledger) back
	// into another provider attempt.
	if cleanupOnly, _ := existing.Payload["cleanup_only"].(bool); cleanupOnly {
		return nil
	}
	switch existing.Status {
	case models.ScheduledJobStatusProcessing:
		return nil
	case models.ScheduledJobStatusPending:
		return a.DB.Model(&models.ScheduledJob{}).Where(
			"id = ? AND organization_id = ? AND status = ? AND version = ?",
			existing.ID,
			account.OrganizationID,
			models.ScheduledJobStatusPending,
			existing.Version,
		).Updates(map[string]any{
			"run_at":  now,
			"payload": payload,
			"version": gorm.Expr("version + 1"),
		}).Error
	case models.ScheduledJobStatusCompleted,
		models.ScheduledJobStatusFailed,
		models.ScheduledJobStatusCancelled:
		return nil
	default:
		return fmt.Errorf("unsupported coexistence media status %q", existing.Status)
	}
}

// ensureCoexistenceStagedMediaJob publishes one generation-bound hydration job
// in the same transaction as the contact-free review receipt. A semantic A→B→A
// sequence receives three server generations even though A's content hash
// repeats; terminal jobs are therefore never revived or reinterpreted.
func (a *App) ensureCoexistenceStagedMediaJob(
	account *models.WhatsAppAccount,
	eventID uuid.UUID,
) error {
	if a == nil || a.DB == nil || account == nil || account.ID == uuid.Nil ||
		account.OrganizationID == uuid.Nil || eventID == uuid.Nil {
		return errors.New("staged coexistence media job identity is incomplete")
	}
	var event models.InboundEvent
	if err := a.DB.Clauses(clause.Locking{Strength: "UPDATE"}).Where(
		"id = ? AND organization_id = ?",
		eventID,
		account.OrganizationID,
	).First(&event).Error; err != nil {
		return fmt.Errorf("load staged coexistence media event for enqueue: %w", err)
	}
	if event.Protocol != models.WhatsAppIdentityReviewInboundProtocol ||
		event.EventType != models.WhatsAppIdentityReviewPendingEvent || event.ReviewHoldID == nil {
		return errors.New("staged coexistence media event is not a review receipt")
	}
	mediaID, revision, _, generation, supported := coexistenceStagedMediaRevision(&event)
	if !supported {
		return nil
	}
	hydratedMediaID := coexistenceMediaPayloadString(event.Payload, "media_hydrated_id")
	if coexistenceMediaPayloadString(event.Payload, "media_url") != "" &&
		(hydratedMediaID == "" || hydratedMediaID == mediaID) {
		return nil
	}

	now := time.Now().UTC()
	wamid := strings.TrimSpace(event.ProviderEventID)
	idempotencyKey := coexistenceStagedMediaJobKey(account.ID, wamid, mediaID, revision, generation)
	payload := models.JSONB{
		"account_id":       account.ID.String(),
		"phone_number_id":  strings.TrimSpace(account.PhoneID),
		"event_id":         event.ID.String(),
		"wamid":            wamid,
		"media_id":         mediaID,
		"media_revision":   revision,
		"media_generation": generation,
	}
	job := models.ScheduledJob{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: account.OrganizationID,
		Kind:           coexistenceMediaHydrationJobKind,
		AggregateType:  coexistenceMediaReviewAggregate,
		AggregateID:    &event.ID,
		RunAt:          now,
		Status:         models.ScheduledJobStatusPending,
		MaxAttempts:    defaultCoexistenceMediaMaxAttempts,
		IdempotencyKey: idempotencyKey,
		Payload:        payload,
		Version:        1,
	}
	if err := a.DB.Clauses(clause.OnConflict{DoNothing: true}).Create(&job).Error; err != nil {
		return fmt.Errorf("enqueue staged coexistence media hydration: %w", err)
	}
	var existing models.ScheduledJob
	if err := a.DB.Where(
		"organization_id = ? AND idempotency_key = ?",
		account.OrganizationID,
		idempotencyKey,
	).First(&existing).Error; err != nil {
		return fmt.Errorf("load staged coexistence media hydration job: %w", err)
	}
	if validateCoexistenceMediaJobIdentity(&existing, account.OrganizationID) != nil ||
		existing.AggregateID == nil || *existing.AggregateID != event.ID ||
		coexistenceMediaPayloadString(existing.Payload, "account_id") != account.ID.String() ||
		coexistenceMediaPayloadString(existing.Payload, "phone_number_id") != strings.TrimSpace(account.PhoneID) ||
		coexistenceMediaPayloadString(existing.Payload, "wamid") != wamid ||
		coexistenceMediaPayloadString(existing.Payload, "media_id") != mediaID ||
		coexistenceMediaPayloadString(existing.Payload, "media_revision") != revision {
		return errors.New("staged coexistence media hydration idempotency key collision")
	}
	if storedGeneration, ok := coexistenceMediaPayloadUint64(existing.Payload, "media_generation"); !ok || storedGeneration != generation {
		return errors.New("staged coexistence media hydration generation collision")
	}
	if cleanupOnly, _ := existing.Payload["cleanup_only"].(bool); cleanupOnly {
		return nil
	}
	switch existing.Status {
	case models.ScheduledJobStatusProcessing:
		return nil
	case models.ScheduledJobStatusPending:
		return a.DB.Model(&models.ScheduledJob{}).Where(
			"id = ? AND organization_id = ? AND status = ? AND version = ?",
			existing.ID,
			account.OrganizationID,
			models.ScheduledJobStatusPending,
			existing.Version,
		).Updates(map[string]any{
			"run_at":  now,
			"payload": payload,
			"version": gorm.Expr("version + 1"),
		}).Error
	case models.ScheduledJobStatusCompleted,
		models.ScheduledJobStatusFailed,
		models.ScheduledJobStatusCancelled:
		return nil
	default:
		return fmt.Errorf("unsupported staged coexistence media status %q", existing.Status)
	}
}
