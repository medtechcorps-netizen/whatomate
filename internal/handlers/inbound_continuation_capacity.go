package handlers

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/contactutil"
	"github.com/shridarpatil/whatomate/internal/database"
	"github.com/shridarpatil/whatomate/internal/models"
	"gorm.io/gorm"
)

// preparedIncomingContinuation is an invocation-local selection, not a durable
// checkpoint. No provider work is performed while selecting it. The legacy path
// writes its incoming history only in apply, so a crash has the same history
// rollback boundary as the former outer transaction. Session/tracking/media
// transitions already committed independently before this change.
type preparedIncomingContinuation struct {
	contact         *models.Contact
	settings        *models.ChatbotSettings
	session         *models.ChatbotSession
	isNewSession    bool
	done            bool
	legacyAI        bool
	outsideHours    bool
	keywordResponse *KeywordResponse
	keywordMatched  bool
	activeFlow      *models.ChatbotFlow
	activeFlowErr   error
	triggeredFlow   *models.ChatbotFlow
	nativeSelection *nativeBookingSelection
	incomingHistory *models.ChatbotSessionMessage
	aiInput         *legacyAIInput
}

// A legacy request deliberately uses one stable input snapshot. In particular,
// history committed by another invocation during a context HTTP request is not
// added halfway through this request. Provider-specific message construction,
// configured history limits, and the existing second current-message inclusion
// remain unchanged. No prompt or context is stored in the action ledger.
type legacyAIInput struct {
	sessionID uuid.UUID
	contexts  []models.AIContext
	history   []models.ChatbotSessionMessage
}

type capturedLegacyAI struct {
	result models.JSONB
	err    error
}

// This is a continuation decision for an already owned Reader1 permit, never
// an admission or ownership proof by itself. Its sole production caller has
// completed every preparation DB scope, has not entered a writer fallback, and
// still owns that exact root/organization reader. It neither adds a grant nor
// changes a cohort. Preparation plus model is one nonpreemptive operation;
// ordinary waiters retain FIFO for new grants, not a new queue turn here.
func (a *App) canRetainInboundPreparationForPerform() bool {
	if a == nil || a.inboundContinuation == nil || a.requireInboundContinuationPoolEntry() != nil {
		return false
	}
	organizationID := a.inboundContinuation.OrganizationID
	root := a.rootApp()
	if organizationID == uuid.Nil || a.tenantOrgID != uuid.Nil ||
		root == nil || root.tenantOrgID != uuid.Nil || !root.rlsEnabled() {
		return false
	}
	pool, err := root.DB.DB()
	if err != nil || pool == nil || root.DB.Statement.ConnPool != pool || a.DB.Statement.ConnPool != pool {
		return false
	}
	g := &root.continuationAdmission
	g.mu.Lock()
	defer g.mu.Unlock()
	phase := g.phases[organizationID.String()]
	if g.used < 1 || phase == nil || phase.active == 0 || phase.activeMode != inboundContinuationReader {
		return false
	}
	maximum := pool.Stats().MaxOpenConnections
	if maximum < 0 || (maximum > 0 && maximum < 3) {
		return false
	}
	if maximum > 0 && maximum <= 4 {
		if g.used != 1 {
			return false
		}
	} else if maximum > 0 && g.used > maximum-inboundContinuationProgressAllowance {
		return false
	}
	for _, queued := range g.waiters {
		if queued.priority == inboundContinuationApplyPriority {
			// Yield even to an incompatible application's drain request. A later
			// enqueue occurs after this already admitted operation's boundary.
			return false
		}
	}
	return true
}

// hintInboundContinuationPreparationMode chooses only a local scheduling mode.
// The caller has passed the public entry checks; claim and preparation remain
// authoritative. A failed or stale read falls back to the original reader path.
func (p *InboundContinuationProcessor) hintInboundContinuationPreparationMode(ctx context.Context, organizationID, messageID uuid.UUID) (int, error) {
	if p.process != nil || !p.app.rlsEnabled() {
		return inboundContinuationReader, nil
	}
	root := p.app.rootApp()
	pool, err := root.DB.DB()
	if err != nil || pool == nil {
		return inboundContinuationReader, nil
	}
	maximum := pool.Stats().MaxOpenConnections
	if maximum < 0 || (maximum > 0 && maximum < 3) {
		return inboundContinuationReader, nil
	}
	hintCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	release, err := p.app.acquireInboundContinuationCapacity(hintCtx, p.stopCh, 1)
	if err != nil {
		if waitErr := inboundContinuationAdmissionWaitError(ctx, p.stopCh); waitErr != nil {
			return inboundContinuationReader, waitErr
		}
		return inboundContinuationReader, nil
	}
	var existing bool
	hintErr := func() error {
		defer release()
		return database.WithTenantReadCommitted(root.DB.WithContext(hintCtx), organizationID, func(tx *gorm.DB) error {
			if err := requireOrdinaryTenantOrganization(tx, organizationID); err != nil {
				return err
			}
			now := p.now().UTC()
			staleBefore := now.Add(-p.lease)
			return tx.Raw(`SELECT EXISTS (
				SELECT 1 FROM messages AS m
				JOIN scheduled_jobs AS j
				  ON j.organization_id = m.organization_id AND j.aggregate_id = m.id
				JOIN chatbot_sessions AS s
				  ON s.organization_id = m.organization_id
				 AND s.contact_id = m.contact_id
				 AND s.whats_app_account = m.whats_app_account
				WHERE m.id = ? AND m.organization_id = ? AND m.direction = ?
				  AND s.organization_id = ? AND s.status = ?
				  AND j.organization_id = ? AND j.kind = ? AND j.run_at <= ?
				  AND j.attempts < j.max_attempts
				  AND (j.status = ? OR (j.status = ? AND (j.locked_at IS NULL OR j.locked_at < ?)))
				  AND m.deleted_at IS NULL AND s.deleted_at IS NULL AND j.deleted_at IS NULL
			)`, messageID, organizationID, models.DirectionIncoming, organizationID, models.SessionStatusActive,
				organizationID, inboundContinuationJobKind, now, models.ScheduledJobStatusPending,
				models.ScheduledJobStatusProcessing, staleBefore).Scan(&existing).Error
		})
	}()
	if err := inboundContinuationAdmissionWaitError(ctx, p.stopCh); err != nil {
		return inboundContinuationReader, err
	}
	if hintErr != nil || !existing {
		return inboundContinuationReader, nil
	}
	maximum = pool.Stats().MaxOpenConnections
	if maximum < 0 || (maximum > 0 && maximum < 3) {
		return inboundContinuationReader, nil
	}
	return inboundContinuationWriter, nil
}

// hintInboundRecoveryPreparationMode observes only a scheduling opportunity.
// It checks the first eligible job before message/session qualification, without
// reserving it. Unknown observations retain recovery's original Writer1 route;
// claim and preparation remain authoritative even when Reader1 is requested.
func (p *InboundContinuationProcessor) hintInboundRecoveryPreparationMode(ctx context.Context, organizationID uuid.UUID) (int, error) {
	if p.process != nil || !p.app.rlsEnabled() {
		return inboundContinuationWriter, nil
	}
	root := p.app.rootApp()
	pool, err := root.DB.DB()
	if err != nil || pool == nil {
		return inboundContinuationWriter, nil
	}
	maximum := pool.Stats().MaxOpenConnections
	if maximum < 0 || (maximum > 0 && maximum < 3) {
		return inboundContinuationWriter, nil
	}
	hintCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	release, err := p.app.acquireInboundContinuationCapacity(hintCtx, p.stopCh, 1)
	if err != nil {
		if waitErr := inboundContinuationAdmissionWaitError(ctx, p.stopCh); waitErr != nil {
			return inboundContinuationWriter, waitErr
		}
		return inboundContinuationWriter, nil
	}
	var readerCompatible bool
	hintErr := func() error {
		defer release()
		return database.WithTenantReadCommitted(root.DB.WithContext(hintCtx), organizationID, func(tx *gorm.DB) error {
			if err := requireOrdinaryTenantOrganization(tx, organizationID); err != nil {
				return err
			}
			now := p.now().UTC()
			staleBefore := now.Add(-p.lease)
			return tx.Raw(`SELECT EXISTS (
				SELECT 1 FROM (
					SELECT j.organization_id, j.aggregate_id
					FROM scheduled_jobs AS j
					WHERE j.organization_id = ? AND j.kind = ? AND j.run_at <= ?
					  AND j.attempts < j.max_attempts
					  AND (j.status = ? OR (j.status = ? AND (j.locked_at IS NULL OR j.locked_at < ?)))
					  AND j.deleted_at IS NULL
					ORDER BY j.run_at, j.created_at, j.id
					LIMIT 1
				) AS candidate
				JOIN messages AS m
				  ON m.organization_id = candidate.organization_id AND m.id = candidate.aggregate_id
				JOIN contacts AS c
				  ON c.id = m.contact_id AND c.organization_id = m.organization_id
				WHERE m.organization_id = ? AND m.direction = ? AND m.deleted_at IS NULL
				  AND m.contact_id IS NOT NULL AND m.contact_id <> ?
				  AND c.deleted_at IS NULL AND c.merged_into_id IS NULL
				  AND BTRIM(m.whats_app_account) <> ''
				  AND NOT EXISTS (
					SELECT 1 FROM chatbot_sessions AS s
					WHERE s.organization_id = ? AND s.organization_id = m.organization_id
					  AND s.contact_id = m.contact_id AND s.whats_app_account = m.whats_app_account
					  AND s.status = ? AND s.deleted_at IS NULL
				  )
			)`, organizationID, inboundContinuationJobKind, now, models.ScheduledJobStatusPending,
				models.ScheduledJobStatusProcessing, staleBefore, organizationID, models.DirectionIncoming,
				uuid.Nil, organizationID, models.SessionStatusActive).Scan(&readerCompatible).Error
		})
	}()
	if err := inboundContinuationAdmissionWaitError(ctx, p.stopCh); err != nil {
		return inboundContinuationWriter, err
	}
	if hintErr != nil || !readerCompatible {
		return inboundContinuationWriter, nil
	}
	maximum = pool.Stats().MaxOpenConnections
	if maximum < 0 || (maximum > 0 && maximum < 3) {
		return inboundContinuationWriter, nil
	}
	return inboundContinuationReader, nil
}

// Discovery and the scoped writer wait share the original ingress admission
// deadline. Discovery owns one globally accounted credit, releases it and its
// query fully, then queues for the atomic organization+credit grant. The caller
// must revalidate the phone/account inside its existing committed tenant scope.
// Canceling this wait context after grant never cancels that owning transaction.
func (a *App) acquireIncomingMessageOrganization(phoneID string) (uuid.UUID, func(), error) {
	if !a.rlsEnabled() {
		organizationID, err := a.resolveWhatsAppOrganization(phoneID)
		return organizationID, func() {}, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	releaseDiscovery, err := a.acquireInboundContinuationCapacity(ctx, nil, 1)
	if err != nil {
		return uuid.Nil, nil, fmt.Errorf("incoming message capacity admission: %w", err)
	}
	var organizationID uuid.UUID
	err = func() error {
		defer releaseDiscovery()
		var discoverErr error
		organizationID, discoverErr = a.resolveWhatsAppOrganizationWithDB(a.rootApp().DB.WithContext(ctx), phoneID)
		return discoverErr
	}()
	if err != nil {
		return uuid.Nil, nil, err
	}
	release, err := a.acquireIncomingMessageIntake(ctx, organizationID.String())
	if err != nil {
		return uuid.Nil, nil, fmt.Errorf("incoming message capacity admission: %w", err)
	}
	return organizationID, release, nil
}

// prepareIncomingContinuation owns at most one connection at a time. Each read
// callback finishes before any independent tracking/session/media transaction.
// Its receiver is a per-job root-pool App, never a discarded tenant transaction.
// acquireWriter is called only between completed DB scopes, before either
// operation that needs an organization UPDATE fence. It must release the reader
// permit before waiting and retain writer ownership for the remaining preparation.
func (a *App) prepareIncomingContinuation(ctx context.Context, work *persistedIncomingMessage, acquireWriter func() error) (*preparedIncomingContinuation, error) {
	if acquireWriter == nil {
		return nil, errors.New("inbound preparation requires an explicit writer transition")
	}
	prepared := &preparedIncomingContinuation{}
	read := func(fn func(*App) error) error { return a.WithTenantApp(work.OrganizationID, fn) }
	if err := read(func(scoped *App) error {
		var err error
		prepared.contact, err = contactutil.ResolveCanonicalContact(scoped.DB, work.OrganizationID, work.Contact.ID)
		return err
	}); err != nil {
		return nil, fmt.Errorf("resolve inbound continuation contact: %w", err)
	}
	account := &work.Account
	if work.Extracted.Media != nil {
		if err := acquireWriter(); err != nil {
			return nil, err
		}
	}
	if err := a.hydratePersistedIncomingMedia(ctx, work, account); err != nil {
		return nil, err
	}
	if work.Persisted.Metadata[coexistenceMediaRevokedMetadataKey] == true {
		prepared.done = true
		return prepared, nil
	}
	if err := read(func(scoped *App) error {
		scoped.broadcastNewMessage(work.OrganizationID, &work.Persisted, prepared.contact)
		return nil
	}); err != nil {
		return nil, err
	}
	if incomingMessageAutomaticAISuppressed(&work.Persisted) {
		reason, _ := work.Persisted.Metadata[incomingAutomaticAISuppressionReasonKey].(string)
		return nil, &inboundContinuationPolicyStop{Reason: reason}
	}
	if err := a.clearContactChatbotTrackingForInbound(prepared.contact.ID); err != nil {
		return nil, fmt.Errorf("clear inbound chatbot tracking: %w", err)
	}
	if err := read(func(scoped *App) error {
		if scoped.hasActiveAgentTransfer(account.OrganizationID, prepared.contact.ID) {
			prepared.done = true
			return nil
		}
		var err error
		prepared.settings, err = scoped.getChatbotSettingsCached(account.OrganizationID, account.Name)
		return err
	}); err != nil {
		return nil, fmt.Errorf("load inbound continuation chatbot settings: %w", err)
	}
	if prepared.done {
		return prepared, nil
	}
	settings := prepared.settings
	prepared.outsideHours = settings.BusinessHours.Enabled && len(settings.BusinessHours.Hours) > 0 && !a.isWithinBusinessHours(settings.BusinessHours.Hours)
	// These routes return before session acquisition in the ordinary processor.
	if !settings.IsEnabled || work.Extracted.Text == "" ||
		(prepared.outsideHours && !settings.BusinessHours.AllowAutomatedOutside) {
		return prepared, nil
	}
	var err error
	prepared.session, prepared.isNewSession, err = a.getOrCreatePreparedIncomingSessionWithWriter(account.OrganizationID,
		prepared.contact.ID, account.Name, work.Message.From, settings.SessionTimeoutMins, acquireWriter)
	if err != nil || prepared.session == nil {
		if err == nil {
			err = errors.New("chatbot session was not returned")
		}
		return nil, fmt.Errorf("get inbound continuation chatbot session: %w", err)
	}
	prepared.session.SessionData = cloneInboundContinuationJSONB(prepared.session.SessionData)
	prepared.incomingHistory = &models.ChatbotSessionMessage{
		BaseModel: models.BaseModel{ID: uuid.New(), CreatedAt: a.DB.NowFunc().UTC().Truncate(time.Microsecond)},
		SessionID: prepared.session.ID, Direction: models.DirectionIncoming,
		Message: work.Extracted.Text, StepName: "keyword_check",
	}
	if isNativeBookingConfirmation(work.Extracted.Text) {
		return prepared, nil
	}
	if err := read(func(scoped *App) error {
		prepared.keywordResponse, prepared.keywordMatched = scoped.matchKeywordRules(account.OrganizationID, account.Name, work.Extracted.Text)
		if prepared.keywordMatched && prepared.keywordResponse.ResponseType == models.ResponseTypeTransfer {
			return nil
		}
		if prepared.session.CurrentFlowID != nil {
			prepared.activeFlow, prepared.activeFlowErr = scoped.getChatbotFlowByIDCached(account.OrganizationID, *prepared.session.CurrentFlowID)
			return nil
		}
		prepared.triggeredFlow = scoped.matchFlowTrigger(account.OrganizationID, work.Extracted.Text)
		return nil
	}); err != nil {
		return nil, err
	}
	if (prepared.keywordMatched && prepared.keywordResponse.ResponseType == models.ResponseTypeTransfer) ||
		prepared.session.CurrentFlowID != nil || prepared.triggeredFlow != nil {
		return prepared, nil
	}
	// Native recovery/authority is selected exactly once, including the unhandled
	// decision. A newly enabled native route cannot replace a selected legacy
	// computation halfway through this invocation.
	restore := a.pushInboundContinuationActionScope("native-booking:" + prepared.session.ID.String() + ":" + a.inboundContinuation.actionScope)
	identity, identityErr := a.nextInboundContinuationActionKey(nativeBookingEffect, nil)
	if identityErr != nil {
		prepared.nativeSelection = &nativeBookingSelection{loadErr: identityErr}
	} else {
		prepared.nativeSelection = a.selectNativeBooking(account, prepared.session, identity)
	}
	restore()
	selection := prepared.nativeSelection
	if selection.loadErr != nil || selection.existing != nil || selection.authorityErr == nil ||
		!inboundContinuationStoppedByPolicy(selection.authorityErr) {
		return prepared, nil
	}
	if (prepared.isNewSession && settings.DefaultResponse != "") || prepared.keywordMatched ||
		!settings.AI.Enabled || settings.AI.Provider == "" || settings.AI.APIKey == "" {
		return prepared, nil
	}
	prepared.legacyAI = true
	prepared.aiInput = &legacyAIInput{sessionID: prepared.session.ID}
	if err := read(func(scoped *App) error {
		prepared.aiInput.contexts, _ = scoped.getAIContextsCached(settings.OrganizationID, prepared.session.WhatsAppAccount)
		if settings.AI.IncludeHistory {
			history := scoped.getSessionHistory(prepared.session.ID, settings.AI.HistoryLimit)
			prepared.aiInput.history = projectIncomingSessionHistory(history, *prepared.incomingHistory, settings.AI.HistoryLimit)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	return prepared, nil
}

// This reader uses the same selector advisory namespace as policy writers, then
// KEY SHARE. It can validate proven identity and create a genuinely new session;
// selector enrichment and existing-session transitions must use the original
// UPDATE-fenced writers. READ COMMITTED observes a writer that commits while the
// reader waits, even when the pool defaults to REPEATABLE READ.
func (a *App) withContinuationReaderApp(organizationID uuid.UUID, fn func(*App) error) error {
	root := a.rootApp()
	if root == nil || root.DB == nil || fn == nil {
		return errors.New("continuation reader requires a root database and callback")
	}
	var afterCommit []func()
	err := a.withBoundedRootConnection(context.Background(), 15*time.Second, func(db *gorm.DB) error {
		return database.WithTenantReadCommitted(db, organizationID, func(tx *gorm.DB) error {
			// The shared organization fence permits another contact's attempt to
			// run concurrently. Bound all subsequent contact/message/session waits
			// too: a waiting reader must not retain the advisory fence indefinitely
			// against a handover needed by a transaction it is waiting on.
			phaseCtx, cancel := context.WithTimeout(tx.Statement.Context, 20*time.Second)
			defer cancel()
			phaseDB := tx.WithContext(phaseCtx)
			phaseErr := phaseDB.Exec("SELECT pg_catalog.pg_advisory_xact_lock(pg_catalog.hashtextextended(?, 0))",
				database.WhatsAppIdentityReviewContactSelectorFenceKey(organizationID)).Error
			if phaseErr == nil {
				phaseErr = database.LockOrganizationAIAttemptScope(phaseDB, organizationID)
			}
			if phaseErr == nil {
				phaseErr = requireOrdinaryTenantOrganization(phaseDB, organizationID)
			}
			scoped := a.scopedApp(phaseDB, organizationID)
			if phaseErr == nil {
				phaseErr = fn(scoped)
			}
			if phaseErr != nil {
				if errors.Is(phaseCtx.Err(), context.DeadlineExceeded) {
					return fmt.Errorf("wedge guard: continuation validation/session locks not acquired within 20s: %w: %w", context.DeadlineExceeded, phaseErr)
				}
				return phaseErr
			}
			afterCommit = scoped.takeAfterCommit()
			return nil
		})
	})
	if err != nil {
		return err
	}
	a.runAfterCommit(afterCommit)
	return nil
}

// The fast path inserts only when there is no active session. A reusable,
// expired, or duplicate active row makes it release every reader/contact lock
// and restart the original UPDATE-fenced acquisition. It never upgrades locks.
func (a *App) getOrCreatePreparedIncomingSession(orgID, contactID uuid.UUID, accountName, phoneNumber string, timeoutMins int) (*models.ChatbotSession, bool, error) {
	return a.getOrCreatePreparedIncomingSessionWithWriter(orgID, contactID, accountName, phoneNumber, timeoutMins, nil)
}

// The explicit transition is used by reader-admitted production preparation.
// The compatibility wrapper above retains its caller-owned admission boundary.
func (a *App) getOrCreatePreparedIncomingSessionWithWriter(orgID, contactID uuid.UUID, accountName, phoneNumber string, timeoutMins int, acquireWriter func() error) (*models.ChatbotSession, bool, error) {
	var created *models.ChatbotSession
	err := a.withContinuationReaderApp(orgID, func(scoped *App) error {
		canonical, err := contactutil.ResolveCanonicalContactForUpdate(scoped.DB, orgID, contactID)
		if err != nil {
			return err
		}
		policy, err := database.EvaluateContactAutomaticReplyPolicy(scoped.DB, orgID, canonical.ID)
		if err != nil {
			return err
		}
		if !policy.Allowed {
			return &inboundContinuationPolicyStop{Reason: policy.Reason}
		}
		var active models.ChatbotSession
		err = scoped.DB.Select("id").Where("organization_id = ? AND contact_id = ? AND whats_app_account = ? AND status = ?",
			orgID, canonical.ID, accountName, models.SessionStatusActive).Take(&active).Error
		if err == nil {
			return nil
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		now := time.Now().UTC().Truncate(time.Microsecond)
		canonicalPhone := canonical.PhoneNumber
		if canonicalPhone == "" {
			canonicalPhone = phoneNumber
		}
		data := models.JSONB{}
		if a.inboundContinuation != nil && a.inboundContinuation.MessageID != uuid.Nil {
			data["__inbound_session_origin"] = a.inboundContinuation.MessageID.String()
		}
		session := &models.ChatbotSession{BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: orgID,
			ContactID: canonical.ID, WhatsAppAccount: accountName, PhoneNumber: canonicalPhone,
			Status: models.SessionStatusActive, SessionData: data, StartedAt: now, LastActivityAt: now}
		if err := scoped.DB.Create(session).Error; err != nil {
			return err
		}
		created = session
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	if created != nil {
		return created, true, nil
	}
	// withContinuationReaderApp has committed and released every selector,
	// organization and contact lock. Never upgrade while retaining those locks
	// or a reader permit. The original writer revalidates current session state.
	if acquireWriter != nil {
		if err := acquireWriter(); err != nil {
			return nil, false, err
		}
	}
	return a.getOrCreateSession(orgID, contactID, accountName, phoneNumber, timeoutMins)
}

func projectIncomingSessionHistory(history []models.ChatbotSessionMessage, incoming models.ChatbotSessionMessage, limit int) []models.ChatbotSessionMessage {
	if limit == 0 {
		return nil
	}
	rows := append(append([]models.ChatbotSessionMessage(nil), history...), incoming)
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].CreatedAt.After(rows[j].CreatedAt) })
	if limit > 0 && len(rows) > limit {
		rows = rows[:limit]
	}
	for i, j := 0, len(rows)-1; i < j; i, j = i+1, j-1 {
		rows[i], rows[j] = rows[j], rows[i]
	}
	return rows
}

func (a *App) logPreparedIncomingHistory(prepared *preparedIncomingContinuation) {
	if prepared.incomingHistory == nil {
		return
	}
	if err := a.DB.Create(prepared.incomingHistory).Error; err != nil {
		a.Log.Error("Failed to log session message", "error", err)
	}
}

func (a *App) incomingWithinBusinessHours(hours models.JSONBArray) bool {
	if a.preparedIncoming != nil {
		return !a.preparedIncoming.outsideHours
	}
	return a.isWithinBusinessHours(hours)
}

// Reader-compatible preparation and its legacy model may share one admitted
// Reader1 operation, with no DB scope spanning the transition. This consumes one
// finite reader grant, rather than two. Queued apply work or a reduced pool makes
// the operation yield and re-enter the ordinary perform queue. Writer fallbacks
// always release reader ownership first; every route releases before apply.
// Captured computations are never discarded merely because the caller expires.
func (p *InboundContinuationProcessor) processPreparedIncoming(ctx, ownershipCtx context.Context, execution *App, work *persistedIncomingMessage, job *models.ScheduledJob, releasePreparation func(), preparationMode int) error {
	initialWriter := preparationMode == inboundContinuationWriter
	var releaseWriter func()
	releaseAllPreparation := func() {
		releasePreparation()
		if releaseWriter != nil {
			releaseWriter()
		}
	}
	// processClaimed owns the original preparation release in either mode.
	// Replacement writer ownership also unwinds on preparation errors and panics.
	defer releaseAllPreparation()
	acquireWriter := func() error {
		if initialWriter || releaseWriter != nil {
			return nil
		}
		releasePreparation()
		var err error
		releaseWriter, err = execution.acquireInboundContinuationPhase(
			execution.markInboundContinuationContext(ctx), p.stopCh, 1,
			work.OrganizationID.String(), inboundContinuationWriter)
		return err
	}
	prepared, prepareErr := execution.prepareIncomingContinuation(ctx, work, acquireWriter)
	if prepareErr != nil {
		return prepareErr
	}
	retainReader := preparationMode == inboundContinuationReader && prepared.legacyAI && releaseWriter == nil && execution.canRetainInboundPreparationForPerform()
	if !retainReader {
		releaseAllPreparation()
	}
	var captured *capturedLegacyAI
	if prepared.legacyAI {
		// Tiny pools and queued applications use the original perform admission
		// path. Retention is possible only for this still-owned reader permit.
		performCtx := execution.markInboundContinuationContext(ctx)
		phaseErr := func() error {
			releasePerform := releasePreparation
			if !retainReader {
				var err error
				releasePerform, err = execution.acquireInboundContinuationPhase(performCtx, p.stopCh, 1, work.OrganizationID.String(), inboundContinuationReader)
				if err != nil {
					return err
				}
			}
			defer releasePerform()
			if err := inboundContinuationAdmissionWaitError(performCtx, p.stopCh); err != nil {
				return err
			}
			if err := ownershipCtx.Err(); err != nil {
				return err
			}
			execution.legacyAIInput = prepared.aiInput
			defer func() { execution.legacyAIInput = nil }()
			result, err := execution.captureLegacyAIResult(prepared.settings, prepared.session, work.Extracted.Text)
			captured = &capturedLegacyAI{result: result, err: err}
			return nil
		}()
		if phaseErr != nil {
			return phaseErr
		}
	}
	releaseAllPreparation()
	applyCtx, stop := ctx, (<-chan struct{})(p.stopCh)
	if captured != nil {
		applyCtx, stop = ownershipCtx, nil
	}
	// Classification happens after perform has released every scope/credit and
	// before apply owns anything. Nested helpers inherit this mode; they never
	// acquire or upgrade local phase ownership while holding a transaction.
	applyMode := preparedIncomingApplyMode(prepared, captured)
	applyCtx = execution.markInboundContinuationContext(applyCtx)
	if applyMode == inboundContinuationReader {
		releaseApply, err := execution.acquireInboundContinuationGeneratedApply(applyCtx, stop, work, prepared, captured)
		if err != nil {
			return err
		}
		defer releaseApply()
		return p.processGeneratedIncomingApply(ownershipCtx, execution, work, job, prepared, captured)
	}
	releaseApply, err := execution.acquireInboundContinuationPhase(applyCtx, stop, 3, work.OrganizationID.String(), applyMode)
	if err != nil {
		return err
	}
	defer releaseApply()
	var continuationErr error
	err = execution.WithTenantApp(work.OrganizationID, func(scoped *App) error {
		scoped.inboundContinuation = execution.inboundContinuation
		scoped.preparedIncoming = prepared
		if err := ownershipCtx.Err(); err != nil {
			continuationErr = err
			return nil
		}
		var owners int64
		if err := scoped.DB.WithContext(ownershipCtx).Model(&models.ScheduledJob{}).
			Where("id = ? AND organization_id = ? AND kind = ? AND status = ? AND locked_by = ?",
				job.ID, work.OrganizationID, inboundContinuationJobKind, models.ScheduledJobStatusProcessing, p.workerID).
			Count(&owners).Error; err != nil {
			continuationErr = err
			return nil
		}
		if owners != 1 {
			continuationErr = errors.New("inbound continuation apply lost its lease")
			return nil
		}
		if prepared.legacyAI {
			scoped.logPreparedIncomingHistory(prepared)
			continuationErr = scoped.continueLegacyAIResponse(&work.Account, prepared.contact, prepared.session,
				prepared.settings, work.Extracted.Text, prepared.isNewSession, captured)
		} else {
			// Recheck after the preparation/admission gap. Do not convert an
			// obsolete selected route into successful consumption of the inbound.
			if err := scoped.requirePreparedContinuationSessionTx(scoped.DB.WithContext(ctx), prepared.session); err != nil {
				continuationErr = err
				return nil
			}
			execution.inboundContinuation.preparedSession = prepared.session
			continuationErr = scoped.continuePersistedIncomingMessage(ctx, work)
		}
		if inboundContinuationStoppedByPolicy(continuationErr) {
			continuationErr = nil
		}
		if continuationErr == nil {
			continuationErr = scoped.markInboundContinuationCompleted(work.Persisted.ID)
		}
		return nil
	})
	if err != nil {
		return err
	}
	return continuationErr
}

// processGeneratedIncomingApply keeps the existing physical send guard and all
// independently durable send/ledger phases. Only invocation-local history and
// completion wait until that guard has returned; their final transaction keeps
// its ordinary uncancelled context and owns the deferred realtime callbacks.
// This deliberately changes the local-history failure boundary: a failed
// history insert cannot poison verification of an already accepted send.
// Session deletion between send and finalization can fail the history write;
// replay must use the durable action result, never repeat the physical effect.
func (p *InboundContinuationProcessor) processGeneratedIncomingApply(
	ownershipCtx context.Context,
	execution *App,
	work *persistedIncomingMessage,
	job *models.ScheduledJob,
	prepared *preparedIncomingContinuation,
	captured *capturedLegacyAI,
) error {
	if preparedIncomingApplyMode(prepared, captured) != inboundContinuationReader ||
		execution.inboundContinuation == nil || execution.inboundContinuation.detachedGeneratedApply {
		return errors.New("detached apply requires one captured generated-text continuation")
	}
	// Preserve the original pre-send ordinary-tenant and parent-lease check.
	// Fully release this read before acquiring the unchanged physical guard.
	var ownershipErr error
	err := execution.WithTenantApp(work.OrganizationID, func(scoped *App) error {
		if err := ownershipCtx.Err(); err != nil {
			ownershipErr = err
			return nil
		}
		var owners int64
		if err := scoped.DB.WithContext(ownershipCtx).Model(&models.ScheduledJob{}).
			Where("id = ? AND organization_id = ? AND kind = ? AND status = ? AND locked_by = ?",
				job.ID, work.OrganizationID, inboundContinuationJobKind, models.ScheduledJobStatusProcessing, p.workerID).
			Count(&owners).Error; err != nil {
			ownershipErr = err
			return nil
		}
		if owners != 1 {
			ownershipErr = errors.New("inbound continuation apply lost its lease")
		}
		return nil
	})
	if err != nil {
		return err
	}
	if ownershipErr != nil {
		return ownershipErr
	}
	answer := captured.result["answer"].(string) // validated by the exact route classifier
	execution.Log.Info("Attempting AI response", "provider", prepared.settings.AI.Provider, "model", prepared.settings.AI.Model)
	execution.Log.Info("AI response generated successfully", "response_length", len(answer))
	continuationErr := func() error {
		execution.inboundContinuation.detachedGeneratedApply = true
		defer func() { execution.inboundContinuation.detachedGeneratedApply = false }()
		if err := execution.sendAndSaveTextMessage(&work.Account, prepared.contact, answer); err != nil {
			execution.Log.Error("Failed to send AI response", "error", err, "contact", prepared.contact.PhoneNumber)
			return fmt.Errorf("send AI response: %w", err)
		}
		return nil
	}()
	sent := continuationErr == nil
	err = execution.WithTenantApp(work.OrganizationID, func(scoped *App) error {
		scoped.inboundContinuation = execution.inboundContinuation
		scoped.preparedIncoming = prepared
		// Capture callbacks on success AND guard error. They may run only when
		// this original history/completion owner successfully commits.
		scoped.adoptAfterCommit(execution)
		scoped.logPreparedIncomingHistory(prepared)
		if sent {
			scoped.logSessionMessage(prepared.session.ID, models.DirectionOutgoing, answer, "ai_response")
		}
		if inboundContinuationStoppedByPolicy(continuationErr) {
			continuationErr = nil
		}
		if continuationErr == nil {
			continuationErr = scoped.markInboundContinuationCompleted(work.Persisted.ID)
		}
		return nil
	})
	if err != nil {
		return err
	}
	return continuationErr
}

// Only the audited captured generated-text route is organization-reader
// compatible. It still writes independent history/send/receipt facts and keeps
// every database guard. Fallback, error, graph and native routes stay mixed.
func preparedIncomingApplyMode(prepared *preparedIncomingContinuation, captured *capturedLegacyAI) int {
	if prepared == nil || !prepared.legacyAI || prepared.settings == nil ||
		!prepared.settings.AI.Enabled || prepared.settings.AI.Provider == "" || prepared.settings.AI.APIKey == "" ||
		captured == nil || captured.err != nil {
		return inboundContinuationWriter
	}
	outcome, outcomeOK := captured.result["outcome"].(string)
	answer, answerOK := captured.result["answer"].(string)
	if outcomeOK && outcome == "generated" && answerOK && answer != "" {
		return inboundContinuationReader
	}
	return inboundContinuationWriter
}

// The generated-text path releases each independent send/read scope before its
// final history transaction, so its physical guard plus one independent scope
// require two connections. Keep this admission seam tied to that exact route;
// mixed routes retain three and admission never borrows a SQL connection.
func (a *App) acquireInboundContinuationGeneratedApply(ctx context.Context, stop <-chan struct{}, work *persistedIncomingMessage, prepared *preparedIncomingContinuation, captured *capturedLegacyAI) (func(), error) {
	if a == nil || a.inboundContinuation == nil || work == nil || prepared == nil ||
		prepared.contact == nil || prepared.session == nil || prepared.settings == nil ||
		preparedIncomingApplyMode(prepared, captured) != inboundContinuationReader {
		return nil, errors.New("generated apply admission requires a captured generated-text continuation")
	}
	organizationID := a.inboundContinuation.OrganizationID
	if organizationID == uuid.Nil || work.OrganizationID != organizationID ||
		prepared.contact.OrganizationID != organizationID || prepared.session.OrganizationID != organizationID ||
		prepared.settings.OrganizationID != organizationID {
		return nil, errors.New("generated apply admission requires matching continuation organizations")
	}
	root := a.rootApp()
	if a.tenantOrgID != uuid.Nil || root == nil || root.tenantOrgID != uuid.Nil {
		return nil, errors.New("generated apply admission requires its root connection pool")
	}
	if err := a.requireInboundContinuationPoolEntry(); err != nil {
		return nil, err
	}
	pool, err := root.DB.DB()
	if err != nil || pool == nil || root.DB.Statement.ConnPool != pool || a.DB.Statement.ConnPool != pool {
		return nil, errors.New("generated apply admission requires its exact root SQL pool")
	}
	if !root.rlsEnabled() {
		return func() {}, nil
	}
	if ctx == nil || ctx.Value(inboundContinuationAdmissionContextKey{}) != root {
		return nil, errors.New("generated apply admission requires its marked continuation context")
	}
	return a.acquireInboundContinuationPhaseClass(ctx, stop, 2, inboundContinuationApplyPriority,
		organizationID.String(), inboundContinuationReader, false)
}

// This check takes no session lock across provider I/O. At the physical attempt
// boundary it runs while the existing organization KEY SHARE guard is held;
// the original UPDATE-fenced session writers therefore cannot supersede this
// generation until that attempt ends. A legacy captured result is deliberately
// not bound to a newly invented session-generation predicate.
func (a *App) requirePreparedContinuationSessionTx(tx *gorm.DB, session *models.ChatbotSession) error {
	if session == nil {
		return nil
	}
	if session.ID == uuid.Nil || session.OrganizationID == uuid.Nil || session.ContactID == uuid.Nil || session.LastActivityAt.IsZero() {
		return errors.New("prepared chatbot session identity is incomplete")
	}
	var count int64
	if err := tx.Model(&models.ChatbotSession{}).Where(
		"id = ? AND organization_id = ? AND contact_id = ? AND whats_app_account = ? AND status = ? AND last_activity_at = ?",
		session.ID, session.OrganizationID, session.ContactID, session.WhatsAppAccount,
		models.SessionStatusActive, session.LastActivityAt).Count(&count).Error; err != nil {
		return err
	}
	if count != 1 {
		return errors.New("prepared chatbot session changed before execution; retry with current state")
	}
	return nil
}

// A current claim is required before each new provider boundary. These reads
// use a transaction already owned by the guard/provider path; they never wait
// for another pool slot. The ownership context is query-local, so losing the
// lease after a request starts cannot discard its accepted-result checkpoint.
func (a *App) requireInboundContinuationOwnershipTx(tx *gorm.DB) error {
	if a == nil || a.inboundContinuation == nil {
		return nil
	}
	execution := a.inboundContinuation
	if execution.ownershipCtx == nil && execution.jobID == uuid.Nil && execution.workerID == "" {
		// Direct non-worker invocations retain their existing physical policy and
		// action-ledger guards without inventing a parent worker claim.
		return nil
	}
	if execution.ownershipCtx == nil || execution.jobID == uuid.Nil || execution.workerID == "" {
		return errors.New("inbound continuation ownership identity is incomplete")
	}
	if err := execution.ownershipCtx.Err(); err != nil {
		return fmt.Errorf("inbound continuation lease ownership ended: %w", err)
	}
	parent := tx.Statement.Context
	if parent == nil {
		parent = context.Background()
	}
	queryCtx, cancelQuery := context.WithCancel(parent)
	stopOwnershipCancellation := context.AfterFunc(execution.ownershipCtx, cancelQuery)
	defer func() {
		stopOwnershipCancellation()
		cancelQuery()
	}()
	var owners int64
	if err := tx.WithContext(queryCtx).Model(&models.ScheduledJob{}).
		Where("id = ? AND organization_id = ? AND kind = ? AND aggregate_id = ? AND status = ? AND locked_by = ?",
			execution.jobID, execution.OrganizationID, inboundContinuationJobKind, execution.MessageID,
			models.ScheduledJobStatusProcessing, execution.workerID).Count(&owners).Error; err != nil {
		return err
	}
	if owners != 1 {
		return errors.New("inbound continuation lost its lease before provider attempt")
	}
	if err := execution.ownershipCtx.Err(); err != nil {
		return fmt.Errorf("inbound continuation lease ownership ended: %w", err)
	}
	return nil
}

func (a *App) requireInboundContinuationAttemptOwnership() error {
	if a == nil || a.inboundContinuation == nil || a.inboundContinuation.attemptOwnershipCheck == nil {
		return nil
	}
	return a.inboundContinuation.attemptOwnershipCheck()
}
