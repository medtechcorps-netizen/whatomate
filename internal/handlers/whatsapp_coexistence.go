package handlers

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	appcrypto "github.com/shridarpatil/whatomate/internal/crypto"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/internal/whatsappaccount"
	"github.com/shridarpatil/whatomate/pkg/whatsapp"
	"github.com/valyala/fasthttp"
	"github.com/zerodha/fastglue"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var (
	errCoexistenceAccountUnavailable = errors.New("account is not an active WhatsApp Business App coexistence account")
	errCoexistenceSyncExpired        = errors.New("the coexistence initial sync window has expired")
	errCoexistenceStageUnavailable   = errors.New("the coexistence sync stage is not safe to retry")
	errCoexistenceStateSuperseded    = errors.New("the coexistence sync state changed")
)

type coexistenceSyncStage string

const (
	coexistenceContactStage coexistenceSyncStage = "contacts"
	coexistenceHistoryStage coexistenceSyncStage = "history"
)

// WhatsAppCoexistenceResponse is the non-sensitive synchronization state
// returned with an account. Provider diagnostics remain server-side.
type WhatsAppCoexistenceResponse struct {
	OnboardingStatus       models.CoexistenceOnboardingStatus `json:"onboarding_status"`
	SyncStatus             models.CoexistenceSyncStatus       `json:"sync_status"`
	SyncStartedAt          *time.Time                         `json:"sync_started_at,omitempty"`
	SyncCompletedAt        *time.Time                         `json:"sync_completed_at,omitempty"`
	SyncDeadlineAt         *time.Time                         `json:"sync_deadline_at,omitempty"`
	ContactSyncStatus      models.CoexistenceSyncStatus       `json:"contact_sync_status"`
	ContactSyncRequestedAt *time.Time                         `json:"contact_sync_requested_at,omitempty"`
	ContactSyncCompletedAt *time.Time                         `json:"contact_sync_completed_at,omitempty"`
	HistoryConsent         models.CoexistenceHistoryConsent   `json:"history_consent"`
	HistorySyncStatus      models.CoexistenceSyncStatus       `json:"history_sync_status"`
	HistorySyncRequestedAt *time.Time                         `json:"history_sync_requested_at,omitempty"`
	HistoryProgressPercent int                                `json:"history_progress_percent"`
	HistoryCompletedAt     *time.Time                         `json:"history_completed_at,omitempty"`
	LifecycleStatus        models.CoexistenceLifecycleStatus  `json:"lifecycle_status"`
	LastLifecycleEvent     string                             `json:"last_lifecycle_event,omitempty"`
	LastLifecycleEventAt   *time.Time                         `json:"last_lifecycle_event_at,omitempty"`
	UpdatedAt              time.Time                          `json:"updated_at"`
}

func coexistenceResponse(state *models.WhatsAppCoexistenceState) *WhatsAppCoexistenceResponse {
	if state == nil {
		return nil
	}
	return &WhatsAppCoexistenceResponse{
		OnboardingStatus:       state.OnboardingStatus,
		SyncStatus:             state.SyncStatus,
		SyncStartedAt:          state.SyncStartedAt,
		SyncCompletedAt:        state.SyncCompletedAt,
		SyncDeadlineAt:         state.SyncDeadlineAt,
		ContactSyncStatus:      state.ContactSyncStatus,
		ContactSyncRequestedAt: state.ContactSyncRequestedAt,
		ContactSyncCompletedAt: state.ContactSyncCompletedAt,
		HistoryConsent:         state.HistoryConsent,
		HistorySyncStatus:      state.HistorySyncStatus,
		HistorySyncRequestedAt: state.HistorySyncRequestedAt,
		HistoryProgressPercent: state.HistoryProgressPercent,
		HistoryCompletedAt:     state.HistoryCompletedAt,
		LifecycleStatus:        state.LifecycleStatus,
		LastLifecycleEvent:     state.LastLifecycleEvent,
		LastLifecycleEventAt:   state.LastLifecycleEventAt,
		UpdatedAt:              state.UpdatedAt,
	}
}

func (a *App) loadCoexistenceState(orgID, accountID uuid.UUID) (*models.WhatsAppCoexistenceState, error) {
	var state models.WhatsAppCoexistenceState
	err := a.DB.Where(
		"organization_id = ? AND whats_app_account_id = ?",
		orgID,
		accountID,
	).First(&state).Error
	if err != nil {
		return nil, err
	}
	return &state, nil
}

// loadCoexistenceStateCommitted is for handlers that deliberately run outside
// the request-long tenant transaction while calling Meta. It establishes the
// same tenant/RLS boundary as the surrounding committed onboarding phases.
func (a *App) loadCoexistenceStateCommitted(orgID, accountID uuid.UUID) (*models.WhatsAppCoexistenceState, error) {
	var state *models.WhatsAppCoexistenceState
	err := a.WithCommittedTenantApp(orgID, func(scoped *App) error {
		var err error
		state, err = scoped.loadCoexistenceState(orgID, accountID)
		return err
	})
	return state, err
}

func (a *App) loadCoexistenceStates(orgID uuid.UUID, accountIDs []uuid.UUID) (map[uuid.UUID]models.WhatsAppCoexistenceState, error) {
	statesByAccount := make(map[uuid.UUID]models.WhatsAppCoexistenceState)
	if len(accountIDs) == 0 {
		return statesByAccount, nil
	}
	var states []models.WhatsAppCoexistenceState
	if err := a.DB.Where(
		"organization_id = ? AND whats_app_account_id IN ?",
		orgID,
		accountIDs,
	).Find(&states).Error; err != nil {
		return nil, err
	}
	for _, state := range states {
		statesByAccount[state.WhatsAppAccountID] = state
	}
	return statesByAccount, nil
}

// initializeCoexistenceState starts a fresh one-time sync cycle after a
// completed Embedded Signup. Upsert is required because the same number can be
// legitimately re-onboarded after an account_update lifecycle disconnect.
func (a *App) initializeCoexistenceState(
	orgID, accountID uuid.UUID,
	onboardedAt time.Time,
	businessPhoneNumber string,
) (*models.WhatsAppCoexistenceState, error) {
	onboardedAt = onboardedAt.UTC()
	businessPhoneNumber = strings.TrimSpace(businessPhoneNumber)
	deadline := onboardedAt.Add(models.CoexistenceSyncWindow)
	now := time.Now().UTC()
	// Meta account_update entry timestamps have one-second precision. Store the
	// synthetic onboarding fence at the same precision so a legitimate event
	// emitted later in the same second is not misclassified as stale.
	lifecycleAt := onboardedAt.Truncate(time.Second)
	state := models.WhatsAppCoexistenceState{
		ID:                   uuid.New(),
		OrganizationID:       orgID,
		WhatsAppAccountID:    accountID,
		BusinessPhoneNumber:  businessPhoneNumber,
		OnboardingStatus:     models.CoexistenceOnboardingStatusSyncing,
		OnboardedAt:          &onboardedAt,
		OnboardingCycle:      1,
		SyncStatus:           models.CoexistenceSyncStatusPending,
		SyncStartedAt:        &now,
		SyncDeadlineAt:       &deadline,
		ContactSyncStatus:    models.CoexistenceSyncStatusPending,
		HistoryConsent:       models.CoexistenceHistoryConsentUnknown,
		HistorySyncStatus:    models.CoexistenceSyncStatusPending,
		LifecycleStatus:      models.CoexistenceLifecycleStatusConnected,
		LastLifecycleEvent:   "EMBEDDED_SIGNUP_COMPLETED",
		LastLifecycleEventAt: &lifecycleAt,
		LifecycleMetadata:    models.JSONB{},
		Version:              1,
	}
	updates := map[string]any{
		"onboarding_status":            models.CoexistenceOnboardingStatusSyncing,
		"onboarding_error_code":        "",
		"onboarding_error_message":     "",
		"onboarded_at":                 onboardedAt,
		"onboarding_cycle":             gorm.Expr("whatsapp_coexistence_states.onboarding_cycle + 1"),
		"sync_status":                  models.CoexistenceSyncStatusPending,
		"sync_started_at":              now,
		"sync_completed_at":            nil,
		"sync_deadline_at":             deadline,
		"contact_sync_status":          models.CoexistenceSyncStatusPending,
		"contact_sync_attempts":        0,
		"contact_sync_request_id":      "",
		"contact_sync_last_attempt_at": nil,
		"contact_sync_requested_at":    nil,
		"contact_sync_completed_at":    nil,
		"contact_sync_error_code":      "",
		"contact_sync_error_message":   "",
		"history_consent":              models.CoexistenceHistoryConsentUnknown,
		"history_consent_at":           nil,
		"history_sync_status":          models.CoexistenceSyncStatusPending,
		"history_sync_attempts":        0,
		"history_sync_request_id":      "",
		"history_sync_last_attempt_at": nil,
		"history_sync_requested_at":    nil,
		"history_last_phase":           nil,
		"history_last_chunk_order":     nil,
		"history_progress_percent":     0,
		"history_progress_at":          nil,
		"history_completed_at":         nil,
		"history_sync_error_code":      "",
		"history_sync_error_message":   "",
		"lifecycle_status":             models.CoexistenceLifecycleStatusConnected,
		"last_lifecycle_event":         "EMBEDDED_SIGNUP_COMPLETED",
		"last_lifecycle_event_at":      lifecycleAt,
		"lifecycle_metadata":           models.JSONB{},
		"disconnected_at":              nil,
		"offboarded_at":                nil,
		"reconnected_at":               onboardedAt,
		"disconnect_reason_code":       "",
		"disconnect_reason_message":    "",
		"version":                      gorm.Expr("whatsapp_coexistence_states.version + 1"),
		"updated_at":                   now,
	}
	if businessPhoneNumber != "" {
		updates["business_phone_number"] = businessPhoneNumber
	}
	var existing models.WhatsAppCoexistenceState
	loadErr := a.DB.Clauses(clause.Locking{Strength: "UPDATE"}).Where(
		"organization_id = ? AND whats_app_account_id = ?",
		orgID,
		accountID,
	).First(&existing).Error
	if errors.Is(loadErr, gorm.ErrRecordNotFound) {
		if err := a.DB.Create(&state).Error; err != nil {
			return nil, err
		}
		return a.loadCoexistenceState(orgID, accountID)
	}
	if loadErr != nil {
		return nil, loadErr
	}

	// A lifecycle webhook that happened after this Embedded Signup began is
	// authoritative. A newer ACCOUNT_RECONNECTED is compatible with the fresh
	// credential claim, but its watermark must remain intact; a newer removal
	// or offboard event blocks initialization and keeps the account fail-closed.
	if existing.LastLifecycleEventAt != nil &&
		!existing.LastLifecycleEventAt.UTC().Before(lifecycleAt) &&
		existing.LastLifecycleEvent != "EMBEDDED_SIGNUP_COMPLETED" {
		if existing.LastLifecycleEvent != "ACCOUNT_RECONNECTED" {
			return nil, errCoexistenceStateSuperseded
		}
		for _, column := range []string{
			"lifecycle_status",
			"last_lifecycle_event",
			"last_lifecycle_event_at",
			"lifecycle_metadata",
			"disconnected_at",
			"offboarded_at",
			"reconnected_at",
			"disconnect_reason_code",
			"disconnect_reason_message",
		} {
			delete(updates, column)
		}
	}
	if err := a.DB.Model(&models.WhatsAppCoexistenceState{}).Where(
		"id = ? AND organization_id = ? AND whats_app_account_id = ?",
		existing.ID,
		orgID,
		accountID,
	).Updates(updates).Error; err != nil {
		return nil, err
	}
	return a.loadCoexistenceState(orgID, accountID)
}

type coexistenceRequestSnapshot struct {
	stateID     uuid.UUID
	accountID   uuid.UUID
	onboardedAt time.Time
	attempt     int
	stage       coexistenceSyncStage
}

// coexistenceProviderPreflightError proves that the account lifecycle or
// credential check failed before the provider callback could start. Unlike a
// transport error, this outcome is locally certain and leaves the one-time
// request safe to retry.
type coexistenceProviderPreflightError struct {
	err error
}

func (e *coexistenceProviderPreflightError) Error() string {
	if e == nil || e.err == nil {
		return "coexistence provider preflight failed"
	}
	return e.err.Error()
}

func (e *coexistenceProviderPreflightError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.err
}

func coexistenceRetryableStatus(status models.CoexistenceSyncStatus) bool {
	switch status {
	case models.CoexistenceSyncStatusNotRequested,
		models.CoexistenceSyncStatusPending,
		models.CoexistenceSyncStatusFailed:
		return true
	default:
		return false
	}
}

func (a *App) prepareCoexistenceRequest(
	orgID, accountID uuid.UUID,
	stage coexistenceSyncStage,
	expectedOnboardedAt time.Time,
) (*coexistenceRequestSnapshot, error) {
	var snapshot *coexistenceRequestSnapshot
	expired := false
	err := a.WithCommittedTenantApp(orgID, func(scoped *App) error {
		var state models.WhatsAppCoexistenceState
		if err := scoped.DB.Clauses(clause.Locking{Strength: "UPDATE"}).Where(
			"organization_id = ? AND whats_app_account_id = ?",
			orgID,
			accountID,
		).First(&state).Error; err != nil {
			return err
		}
		if state.OnboardedAt == nil || !state.OnboardedAt.UTC().Equal(expectedOnboardedAt.UTC()) {
			return errCoexistenceStateSuperseded
		}
		if state.LifecycleStatus != models.CoexistenceLifecycleStatusConnected ||
			state.OnboardingStatus == models.CoexistenceOnboardingStatusOffboarded {
			return errCoexistenceAccountUnavailable
		}
		status := state.ContactSyncStatus
		statusColumn := "contact_sync_status"
		attemptsColumn := "contact_sync_attempts"
		attemptedAtColumn := "contact_sync_last_attempt_at"
		errorCodeColumn := "contact_sync_error_code"
		errorMessageColumn := "contact_sync_error_message"
		if stage == coexistenceHistoryStage {
			status = state.HistorySyncStatus
			statusColumn = "history_sync_status"
			attemptsColumn = "history_sync_attempts"
			attemptedAtColumn = "history_sync_last_attempt_at"
			errorCodeColumn = "history_sync_error_code"
			errorMessageColumn = "history_sync_error_message"
		}
		// The deadline limits starting a new request. It must not expire a
		// request already accepted by Meta or overwrite a completed stage.
		if !coexistenceRetryableStatus(status) {
			return errCoexistenceStageUnavailable
		}
		// Meta account_update timestamps have only second precision. Keep the
		// synthetic refresh watermark at that precision so a reconnect emitted
		// later in the same second remains fail-closed instead of looking stale.
		now := time.Now().UTC().Truncate(time.Second)
		if state.SyncDeadlineAt == nil || !now.Before(*state.SyncDeadlineAt) {
			updates := map[string]any{
				"onboarding_status": models.CoexistenceOnboardingStatusExpired,
				"sync_status":       models.CoexistenceSyncStatusExpired,
				"version":           gorm.Expr("version + 1"),
			}
			if stage == coexistenceContactStage && coexistenceRetryableStatus(state.ContactSyncStatus) {
				updates["contact_sync_status"] = models.CoexistenceSyncStatusExpired
			}
			if stage == coexistenceHistoryStage && coexistenceRetryableStatus(state.HistorySyncStatus) {
				updates["history_sync_status"] = models.CoexistenceSyncStatusExpired
			}
			if err := scoped.DB.Model(&state).Updates(updates).Error; err != nil {
				return err
			}
			// Expiry is a durable state transition, not a transaction failure.
			// Return the public sentinel only after the enclosing commit succeeds.
			expired = true
			return nil
		}

		result := scoped.DB.Model(&models.WhatsAppCoexistenceState{}).Where(
			"id = ? AND organization_id = ? AND whats_app_account_id = ? AND version = ? AND "+statusColumn+" = ?",
			state.ID,
			orgID,
			accountID,
			state.Version,
			status,
		).Updates(map[string]any{
			statusColumn:        models.CoexistenceSyncStatusRequesting,
			attemptsColumn:      gorm.Expr(attemptsColumn + " + 1"),
			attemptedAtColumn:   now,
			errorCodeColumn:     "",
			errorMessageColumn:  "",
			"onboarding_status": models.CoexistenceOnboardingStatusSyncing,
			"sync_status":       models.CoexistenceSyncStatusInProgress,
			"version":           gorm.Expr("version + 1"),
		})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return errCoexistenceStateSuperseded
		}
		snapshot = &coexistenceRequestSnapshot{
			stateID:     state.ID,
			accountID:   accountID,
			onboardedAt: state.OnboardedAt.UTC(),
			attempt:     int(state.ContactSyncAttempts) + 1,
			stage:       stage,
		}
		if stage == coexistenceHistoryStage {
			snapshot.attempt = int(state.HistorySyncAttempts) + 1
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if expired {
		return nil, errCoexistenceSyncExpired
	}
	return snapshot, nil
}

func coexistenceProviderDiagnostic(err error) (string, string) {
	if err == nil {
		return "", ""
	}
	var preflightErr *coexistenceProviderPreflightError
	if errors.As(err, &preflightErr) {
		if errors.Is(err, whatsappaccount.ErrOutboundInactive) {
			return "account_inactive", "The WhatsApp account became inactive before the Coexistence sync request started."
		}
		return "local_preflight_failed", "The Coexistence sync request did not start because its live account credentials were unavailable."
	}
	var metaErr *whatsapp.MetaHTTPError
	if errors.As(err, &metaErr) {
		if metaErr.MetaCode > 0 {
			if whatsapp.IsDefiniteProviderRejection(err) {
				return strconv.Itoa(metaErr.MetaCode), "Meta rejected the Coexistence sync request."
			}
			return strconv.Itoa(metaErr.MetaCode), "Meta did not confirm whether the Coexistence sync request was accepted."
		}
		return "meta_http_" + strconv.Itoa(metaErr.StatusCode), "Meta returned an unconfirmed Coexistence sync response."
	}
	if errors.Is(err, context.Canceled) {
		return "request_canceled", "The Coexistence sync request ended before Meta confirmed it."
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "request_timeout", "The Coexistence sync request timed out before Meta confirmed it."
	}
	return "ambiguous_result", "Meta did not confirm whether the Coexistence sync request was accepted."
}

func (a *App) finalizeCoexistenceRequest(
	orgID uuid.UUID,
	snapshot *coexistenceRequestSnapshot,
	response *whatsapp.CoexistenceSyncResponse,
	providerErr error,
) error {
	if snapshot == nil {
		return errors.New("coexistence request snapshot is required")
	}
	return a.WithCommittedTenantApp(orgID, func(scoped *App) error {
		statusColumn := "contact_sync_status"
		attemptsColumn := "contact_sync_attempts"
		requestIDColumn := "contact_sync_request_id"
		requestedAtColumn := "contact_sync_requested_at"
		errorCodeColumn := "contact_sync_error_code"
		errorMessageColumn := "contact_sync_error_message"
		if snapshot.stage == coexistenceHistoryStage {
			statusColumn = "history_sync_status"
			attemptsColumn = "history_sync_attempts"
			requestIDColumn = "history_sync_request_id"
			requestedAtColumn = "history_sync_requested_at"
			errorCodeColumn = "history_sync_error_code"
			errorMessageColumn = "history_sync_error_message"
		}
		now := time.Now().UTC()
		updates := map[string]any{
			"version": gorm.Expr("version + 1"),
		}
		if providerErr == nil && response != nil {
			updates[statusColumn] = models.CoexistenceSyncStatusRequested
			updates[requestIDColumn] = response.RequestID
			updates[requestedAtColumn] = now
			updates[errorCodeColumn] = ""
			updates[errorMessageColumn] = ""
		} else {
			code, message := coexistenceProviderDiagnostic(providerErr)
			updates[errorCodeColumn] = code
			updates[errorMessageColumn] = message
			var preflightErr *coexistenceProviderPreflightError
			if errors.As(providerErr, &preflightErr) ||
				whatsapp.IsDefiniteProviderRejection(providerErr) {
				updates[statusColumn] = models.CoexistenceSyncStatusFailed
			}
		}
		result := scoped.DB.Model(&models.WhatsAppCoexistenceState{}).Where(
			"id = ? AND organization_id = ? AND whats_app_account_id = ? AND onboarded_at = ? AND "+attemptsColumn+" = ? AND "+statusColumn+" = ?",
			snapshot.stateID,
			orgID,
			snapshot.accountID,
			snapshot.onboardedAt,
			snapshot.attempt,
			models.CoexistenceSyncStatusRequesting,
		).Updates(updates)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			var current models.WhatsAppCoexistenceState
			if err := scoped.DB.Where(
				"id = ? AND organization_id = ? AND whats_app_account_id = ?",
				snapshot.stateID,
				orgID,
				snapshot.accountID,
			).First(&current).Error; err != nil {
				return err
			}
			if current.OnboardedAt == nil || !current.OnboardedAt.UTC().Equal(snapshot.onboardedAt.UTC()) {
				return errCoexistenceStateSuperseded
			}
			// A synchronous webhook from this same cycle can advance the stage
			// before the Graph response reaches us. Its later state wins.
		}
		return nil
	})
}

func (a *App) recomputeCoexistenceAggregate(orgID, accountID uuid.UUID) (*models.WhatsAppCoexistenceState, error) {
	var result models.WhatsAppCoexistenceState
	err := a.WithCommittedTenantApp(orgID, func(scoped *App) error {
		if err := scoped.DB.Clauses(clause.Locking{Strength: "UPDATE"}).Where(
			"organization_id = ? AND whats_app_account_id = ?",
			orgID,
			accountID,
		).First(&result).Error; err != nil {
			return err
		}
		onboardingStatus := models.CoexistenceOnboardingStatusSyncing
		syncStatus := models.CoexistenceSyncStatusInProgress
		// Meta provides a request_id when it accepts contact synchronization but
		// does not provide a request-correlated completion marker. The same
		// smb_app_state_sync field is also used indefinitely for ordinary contact
		// deltas, so Requested is the durable terminal success state for this
		// one-time request. Completed remains accepted for older rows.
		contactComplete := result.ContactSyncStatus == models.CoexistenceSyncStatusRequested ||
			result.ContactSyncStatus == models.CoexistenceSyncStatusCompleted
		historyAccepted := result.HistorySyncStatus == models.CoexistenceSyncStatusRequested
		historyComplete := result.HistorySyncStatus == models.CoexistenceSyncStatusCompleted ||
			result.HistorySyncStatus == models.CoexistenceSyncStatusDeclined
		if result.LifecycleStatus == models.CoexistenceLifecycleStatusOffboarded {
			onboardingStatus = models.CoexistenceOnboardingStatusOffboarded
			syncStatus = models.CoexistenceSyncStatusFailed
		} else if result.LifecycleStatus == models.CoexistenceLifecycleStatusDisconnected {
			onboardingStatus = models.CoexistenceOnboardingStatusFailed
			syncStatus = models.CoexistenceSyncStatusFailed
		} else if contactComplete && historyComplete {
			onboardingStatus = models.CoexistenceOnboardingStatusReady
			syncStatus = models.CoexistenceSyncStatusCompleted
		} else if contactComplete && historyAccepted {
			// Meta may emit no history webhook when there is no eligible data.
			// Both Graph acknowledgements are therefore a stable successful
			// onboarding state, while Requested remains distinct from an
			// explicitly completed import.
			onboardingStatus = models.CoexistenceOnboardingStatusConnected
			syncStatus = models.CoexistenceSyncStatusRequested
		} else if result.ContactSyncStatus == models.CoexistenceSyncStatusFailed ||
			result.HistorySyncStatus == models.CoexistenceSyncStatusFailed {
			onboardingStatus = models.CoexistenceOnboardingStatusFailed
			syncStatus = models.CoexistenceSyncStatusFailed
		} else if result.ContactSyncStatus == models.CoexistenceSyncStatusExpired ||
			result.HistorySyncStatus == models.CoexistenceSyncStatusExpired {
			onboardingStatus = models.CoexistenceOnboardingStatusExpired
			syncStatus = models.CoexistenceSyncStatusExpired
		}
		updates := map[string]any{
			"onboarding_status": onboardingStatus,
			"sync_status":       syncStatus,
			"version":           gorm.Expr("version + 1"),
		}
		if syncStatus == models.CoexistenceSyncStatusCompleted && result.SyncCompletedAt == nil {
			now := time.Now().UTC()
			updates["sync_completed_at"] = now
		}
		if err := scoped.DB.Model(&result).Updates(updates).Error; err != nil {
			return err
		}
		return scoped.DB.Where("id = ? AND organization_id = ?", result.ID, orgID).First(&result).Error
	})
	if err != nil {
		return nil, err
	}
	return &result, nil
}

func (a *App) runCoexistenceSyncRequests(
	ctx context.Context,
	orgID, accountID uuid.UUID,
	providerAccount *whatsapp.Account,
) (*models.WhatsAppCoexistenceState, bool, error) {
	if providerAccount == nil || a.WhatsApp == nil {
		return nil, false, errCoexistenceAccountUnavailable
	}
	stateAtStart, err := a.loadCoexistenceStateCommitted(orgID, accountID)
	if err != nil {
		return nil, false, err
	}
	if stateAtStart.OnboardedAt == nil {
		return nil, false, errCoexistenceStateSuperseded
	}
	runOnboardedAt := stateAtStart.OnboardedAt.UTC()
	attempted := false
	for _, stage := range []coexistenceSyncStage{coexistenceContactStage, coexistenceHistoryStage} {
		if ctx != nil {
			if err := ctx.Err(); err != nil {
				return nil, attempted, err
			}
		}
		snapshot, err := a.prepareCoexistenceRequest(orgID, accountID, stage, runOnboardedAt)
		if errors.Is(err, errCoexistenceStageUnavailable) {
			continue
		}
		if err != nil {
			return nil, attempted, err
		}
		attempted = true
		var response *whatsapp.CoexistenceSyncResponse
		var providerErr error
		providerAttempted := false
		// Keep settlement independent of browser cancellation, but never extend
		// the operation deadline after the durable stage is marked requesting.
		// A timed-out attempt remains ambiguous and must not be replayed.
		providerBase := context.Background()
		providerDeadline := time.Now().Add(30 * time.Second)
		if ctx != nil {
			providerBase = context.WithoutCancel(ctx)
			if deadline, ok := ctx.Deadline(); ok && deadline.Before(providerDeadline) {
				providerDeadline = deadline
			}
		}
		providerCtx, cancel := context.WithDeadline(providerBase, providerDeadline)
		providerErr = a.withLockedWhatsAppAccountForOutbound(
			providerCtx,
			orgID,
			accountID,
			func(lockedAccount *models.WhatsAppAccount) error {
				if !lockedAccount.IsSMB ||
					(strings.TrimSpace(providerAccount.PhoneID) != "" &&
						strings.TrimSpace(lockedAccount.PhoneID) != strings.TrimSpace(providerAccount.PhoneID)) {
					return whatsappaccount.ErrOutboundInactive
				}
				providerAttempted = true
				var requestErr error
				if stage == coexistenceContactStage {
					response, requestErr = a.WhatsApp.RequestCoexistenceContactSync(
						providerCtx,
						lockedAccount.ToWAAccount(),
					)
				} else {
					response, requestErr = a.WhatsApp.RequestCoexistenceHistorySync(
						providerCtx,
						lockedAccount.ToWAAccount(),
					)
				}
				return requestErr
			},
		)
		cancel()
		localPreflightFailed := providerErr != nil && !providerAttempted
		if localPreflightFailed {
			providerErr = &coexistenceProviderPreflightError{err: providerErr}
		}
		if err := a.finalizeCoexistenceRequest(orgID, snapshot, response, providerErr); err != nil {
			return nil, attempted, err
		}
		if localPreflightFailed {
			// The stage was durably claimed but no provider request began. Settle
			// it as a definite, retryable local failure and stop this request so
			// the next stage cannot run after the same lifecycle downgrade.
			state, aggregateErr := a.recomputeCoexistenceAggregate(orgID, accountID)
			return state, attempted, aggregateErr
		}
	}
	state, err := a.recomputeCoexistenceAggregate(orgID, accountID)
	return state, attempted, err
}

func (a *App) beginCoexistenceOnboarding(
	ctx context.Context,
	orgID uuid.UUID,
	accountID uuid.UUID,
	providerAccount *whatsapp.Account,
	onboardedAt time.Time,
	businessPhoneNumber string,
) (*models.WhatsAppCoexistenceState, error) {
	if _, err := a.initializeCoexistenceStateCommitted(
		orgID,
		accountID,
		onboardedAt,
		businessPhoneNumber,
	); err != nil {
		return nil, err
	}
	state, _, err := a.runCoexistenceSyncRequests(ctx, orgID, accountID, providerAccount)
	return state, err
}

func (a *App) initializeCoexistenceStateCommitted(
	orgID, accountID uuid.UUID,
	onboardedAt time.Time,
	businessPhoneNumber string,
) (*models.WhatsAppCoexistenceState, error) {
	var state *models.WhatsAppCoexistenceState
	err := a.WithCommittedTenantApp(orgID, func(scoped *App) error {
		var err error
		state, err = scoped.initializeCoexistenceState(orgID, accountID, onboardedAt, businessPhoneNumber)
		return err
	})
	return state, err
}

// restoreCoexistenceAfterCredentialExchange marks the Cloud API side usable
// after a successful Embedded Signup refresh without manufacturing a new
// one-time sync cycle. All request IDs, attempts, consent, progress, and the
// original deadline remain untouched.
func (a *App) restoreCoexistenceAfterCredentialExchange(
	orgID, accountID uuid.UUID,
) (*models.WhatsAppCoexistenceState, error) {
	var restored models.WhatsAppCoexistenceState
	err := a.WithCommittedTenantApp(orgID, func(scoped *App) error {
		var account models.WhatsAppAccount
		if err := scoped.DB.Clauses(clause.Locking{Strength: "UPDATE"}).Where(
			"id = ? AND organization_id = ?",
			accountID,
			orgID,
		).First(&account).Error; err != nil {
			return err
		}
		if !account.IsSMB || strings.TrimSpace(account.Status) != "active" {
			return errCoexistenceAccountUnavailable
		}
		if err := scoped.DB.Clauses(clause.Locking{Strength: "UPDATE"}).Where(
			"organization_id = ? AND whats_app_account_id = ?",
			orgID,
			accountID,
		).First(&restored).Error; err != nil {
			return err
		}

		syncStatus, onboardingStatus, _ := coexistenceSyncRollup(
			restored.ContactSyncStatus,
			restored.HistorySyncStatus,
		)
		// Meta account_update timestamps have one-second precision. Keep this
		// credential-refresh fence at that precision so a legitimate reconnect
		// later in the same second is processed and fails outbound closed.
		now := time.Now().UTC().Truncate(time.Second)
		updates := map[string]any{
			"onboarding_status":         onboardingStatus,
			"sync_status":               syncStatus,
			"lifecycle_status":          models.CoexistenceLifecycleStatusConnected,
			"last_lifecycle_event":      "EMBEDDED_SIGNUP_CREDENTIAL_REFRESH",
			"last_lifecycle_event_at":   now,
			"reconnected_at":            now,
			"disconnect_reason_code":    "",
			"disconnect_reason_message": "",
			"version":                   gorm.Expr("version + 1"),
		}
		if err := scoped.DB.Model(&models.WhatsAppCoexistenceState{}).Where(
			"id = ? AND organization_id = ? AND whats_app_account_id = ?",
			restored.ID,
			orgID,
			accountID,
		).Updates(updates).Error; err != nil {
			return err
		}
		return scoped.DB.Where(
			"id = ? AND organization_id = ?",
			restored.ID,
			orgID,
		).First(&restored).Error
	})
	if err != nil {
		return nil, err
	}
	return &restored, nil
}

func coexistenceWarning(state *models.WhatsAppCoexistenceState) string {
	if state == nil {
		return "WhatsApp is connected, but the initial mobile-app sync could not be recorded. Retry it from the account list within 24 hours."
	}
	if state.OnboardingStatus == models.CoexistenceOnboardingStatusFailed {
		return "WhatsApp is connected, but Meta rejected at least one initial mobile-app sync request. Retry the failed stage within 24 hours."
	}
	if state.OnboardingStatus == models.CoexistenceOnboardingStatusExpired {
		return "The 24-hour window for the initial mobile-app sync has expired. Offboard the number, then complete Embedded Signup again to start a new Coexistence onboarding."
	}
	if state.ContactSyncStatus == models.CoexistenceSyncStatusRequesting ||
		state.HistorySyncStatus == models.CoexistenceSyncStatusRequesting {
		return "Meta may have accepted an initial sync request without returning a confirmation. ReReply will wait for its webhook and will not replay the one-time request."
	}
	return ""
}

// RetryCoexistenceSync resumes only a stage that was never attempted or was
// definitively rejected. A requesting state is intentionally not replayed:
// loss of the Graph response is ambiguous and Meta permits only one request.
func (a *App) RetryCoexistenceSync(r *fastglue.Request) error {
	orgID, err := a.requireExplicitOrganization(r)
	if err != nil {
		return nil
	}
	accountID, err := parsePathUUID(r, "id", "account")
	if err != nil {
		return nil
	}

	var account models.WhatsAppAccount
	err = a.WithCommittedTenantApp(orgID, func(scoped *App) error {
		resolvedOrgID, _, authErr := scoped.requireAuth(r, models.ResourceAccounts, models.ActionWrite)
		if authErr != nil {
			return authErr
		}
		if resolvedOrgID != orgID {
			_ = r.SendErrorEnvelope(fasthttp.StatusForbidden, "Selected organization is not available", nil, "")
			return errEnvelopeSent
		}
		if err := scoped.DB.Clauses(clause.Locking{Strength: "UPDATE"}).Where(
			"id = ? AND organization_id = ?",
			accountID,
			orgID,
		).First(&account).Error; err != nil {
			return err
		}
		if !account.IsSMB || strings.TrimSpace(account.Status) != "active" {
			return errCoexistenceAccountUnavailable
		}
		if account.AccessTokenExpiresAt != nil && !account.AccessTokenExpiresAt.After(time.Now().UTC()) {
			return errRegistrationRecoveryCredentials
		}
		encryptionKey := strings.TrimSpace(scoped.integrationEncryptionKey())
		if encryptionKey == "" {
			return errAccountEncryptionUnavailable
		}
		accessToken, decryptErr := appcrypto.Decrypt(account.AccessToken, encryptionKey)
		if decryptErr != nil || strings.TrimSpace(accessToken) == "" || appcrypto.IsEncrypted(accessToken) {
			return errRegistrationRecoveryCredentials
		}
		account.AccessToken = strings.TrimSpace(accessToken)
		var existing models.WhatsAppCoexistenceState
		stateErr := scoped.DB.Where(
			"organization_id = ? AND whats_app_account_id = ?",
			orgID,
			account.ID,
		).First(&existing).Error
		if errors.Is(stateErr, gorm.ErrRecordNotFound) {
			// Legacy rows do not carry the exact Embedded Signup completion
			// timestamp. CreatedAt is the only conservative deadline anchor;
			// UpdatedAt can move for unrelated edits and must not reopen Meta's
			// one-time 24-hour synchronization window.
			onboardedAt := account.CreatedAt.UTC()
			if !time.Now().UTC().Before(onboardedAt.Add(models.CoexistenceSyncWindow)) {
				return errCoexistenceSyncExpired
			}
			_, stateErr = scoped.initializeCoexistenceState(orgID, account.ID, onboardedAt, "")
		}
		return stateErr
	})
	if errors.Is(err, errEnvelopeSent) {
		return nil
	}
	if err != nil {
		switch {
		case errors.Is(err, gorm.ErrRecordNotFound):
			return r.SendErrorEnvelope(fasthttp.StatusNotFound, "Account not found", nil, "")
		case errors.Is(err, errCoexistenceAccountUnavailable):
			return r.SendErrorEnvelope(fasthttp.StatusConflict, "This account is not an active WhatsApp Business App Coexistence connection", nil, "")
		case errors.Is(err, errCoexistenceSyncExpired):
			return r.SendErrorEnvelope(fasthttp.StatusConflict, "The 24-hour initial sync window has expired; offboard the number, then complete Embedded Signup again", nil, "")
		case errors.Is(err, errAccountEncryptionUnavailable):
			return r.SendErrorEnvelope(fasthttp.StatusServiceUnavailable, "Account credential storage is unavailable", nil, "")
		case errors.Is(err, errRegistrationRecoveryCredentials):
			return r.SendErrorEnvelope(fasthttp.StatusConflict, "WhatsApp account credentials are unavailable", nil, "")
		default:
			a.Log.Error("Failed to prepare WhatsApp Coexistence sync", "account_id", accountID, "organization_id", orgID)
			return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to prepare WhatsApp Coexistence sync", nil, "")
		}
	}

	ctx, cancel := context.WithTimeout(requestContext(r), 45*time.Second)
	defer cancel()
	state, attempted, err := a.runCoexistenceSyncRequests(ctx, orgID, account.ID, account.ToWAAccount())
	if err != nil {
		switch {
		case errors.Is(err, errCoexistenceSyncExpired):
			return r.SendErrorEnvelope(fasthttp.StatusConflict, "The 24-hour initial sync window has expired; offboard the number, then complete Embedded Signup again", nil, "")
		case errors.Is(err, errCoexistenceStageUnavailable):
			return r.SendErrorEnvelope(fasthttp.StatusConflict, "No Coexistence sync stage is safe to retry", nil, "")
		default:
			a.Log.Error("Failed to persist WhatsApp Coexistence sync", "account_id", account.ID, "organization_id", orgID)
			return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to persist WhatsApp Coexistence sync", nil, "")
		}
	}
	if !attempted {
		return r.SendErrorEnvelope(fasthttp.StatusConflict, "No Coexistence sync stage is safe to retry", nil, "")
	}
	return r.SendEnvelope(map[string]any{
		"success":     true,
		"coexistence": coexistenceResponse(state),
		"warning":     coexistenceWarning(state),
	})
}

func accountToResponseWithCoexistence(account models.WhatsAppAccount, state *models.WhatsAppCoexistenceState) AccountResponse {
	response := accountToResponse(account)
	if account.IsSMB {
		response.Coexistence = coexistenceResponse(state)
	}
	return response
}
