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

// prepareIncomingContinuation owns at most one connection at a time. Each read
// callback finishes before any independent tracking/session/media transaction.
// Its receiver is a per-job root-pool App, never a discarded tenant transaction.
func (a *App) prepareIncomingContinuation(ctx context.Context, work *persistedIncomingMessage) (*preparedIncomingContinuation, error) {
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
	prepared.session, prepared.isNewSession, err = a.getOrCreatePreparedIncomingSession(account.OrganizationID,
		prepared.contact.ID, account.Name, work.Message.From, settings.SessionTimeoutMins)
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

// processPreparedIncoming releases preparation ownership before separately
// admitting physical model work or apply. A captured computation is never
// discarded or recomputed merely because its caller's deadline expired while
// waiting to apply its durable result.
func (p *InboundContinuationProcessor) processPreparedIncoming(ctx, ownershipCtx context.Context, execution *App, work *persistedIncomingMessage, job *models.ScheduledJob, releasePreparation func()) error {
	prepared, prepareErr := execution.prepareIncomingContinuation(ctx, work)
	if prepareErr != nil {
		releasePreparation()
		return prepareErr
	}
	releasePreparation()
	var captured *capturedLegacyAI
	if prepared.legacyAI {
		// Preparation has released every DB scope. Re-admit the physical phase
		// before any model action claim; a one-connection suppression/broadcast
		// may finish on a tiny pool, but physical model work must fail closed.
		performCtx := execution.markInboundContinuationContext(ctx)
		phaseErr := func() error {
			releasePerform, err := execution.acquireInboundContinuationCapacity(performCtx, p.stopCh, 1)
			if err != nil {
				return err
			}
			defer releasePerform()
			if err := inboundContinuationAdmissionWaitError(performCtx, p.stopCh); err != nil {
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
	applyCtx, stop := ctx, (<-chan struct{})(p.stopCh)
	if captured != nil {
		applyCtx, stop = ownershipCtx, nil
	}
	applyCtx = execution.markInboundContinuationContext(applyCtx)
	releaseApply, err := execution.acquireInboundContinuationCapacity(applyCtx, stop, 3)
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
