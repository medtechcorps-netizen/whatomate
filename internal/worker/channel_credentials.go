package worker

import (
	"context"
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
	defaultThreadsCredentialRefreshBatchSize = 20
	defaultThreadsCredentialRefreshInterval  = 6 * time.Hour
	defaultThreadsCredentialRefreshWindow    = 7 * 24 * time.Hour

	threadsCredentialRefreshClaimTTL          = 5 * time.Minute
	threadsCredentialRefreshClaimTokenKey     = "refresh_claim_token"
	threadsCredentialRefreshClaimTimestampKey = "refresh_claimed_at"
)

type threadsCredentialRefreshFunc func(
	context.Context,
	*models.ChannelAccount,
) (channelapi.CredentialRefreshResult, error)

type threadsCredentialRefreshClaim struct {
	OrganizationID    uuid.UUID
	Account           models.ChannelAccount
	Credential        models.ChannelCredential
	CredentialID      uuid.UUID
	CredentialStatus  models.ChannelCredentialStatus
	CredentialVersion int
	Token             string
	AppID             string
}

type threadsCredentialRefreshOutcome struct {
	Result        channelapi.CredentialRefreshResult
	ProviderError error
	InvalidError  error
}

// RunThreadsCredentialRefresh keeps finite-lived Threads grants healthy. A
// short database claim serializes replicas, while provider I/O runs after the
// claim transaction commits so disconnect and reauthorization are never
// blocked behind a remote request.
func (w *Worker) RunThreadsCredentialRefresh(ctx context.Context) error {
	if w == nil || w.DB == nil {
		return errors.New("threads credential refresh worker requires a database")
	}
	ticker := time.NewTicker(defaultThreadsCredentialRefreshInterval)
	defer ticker.Stop()

	for {
		if _, err := w.ProcessThreadsCredentialRefreshBatch(
			ctx,
			defaultThreadsCredentialRefreshBatchSize,
		); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			w.Log.Error("Threads credential refresh batch failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (w *Worker) ProcessThreadsCredentialRefreshBatch(ctx context.Context, limit int) (int, error) {
	if w == nil || w.DB == nil {
		return 0, errors.New("threads credential refresh worker requires a database")
	}
	if limit <= 0 || limit > 100 {
		limit = defaultThreadsCredentialRefreshBatchSize
	}
	refreshBefore := time.Now().UTC().Add(defaultThreadsCredentialRefreshWindow)
	organizationIDs, err := w.listReadyThreadsCredentialOrganizations(limit, refreshBefore)
	if err != nil {
		return 0, err
	}

	processed := 0
	var batchErrors []error
	for _, orgID := range organizationIDs {
		if ctx.Err() != nil || processed >= limit {
			break
		}
		refreshed, refreshErr := w.refreshThreadsCredential(ctx, orgID, refreshBefore)
		if refreshErr != nil {
			batchErrors = append(batchErrors, fmt.Errorf("refresh tenant %s: %w", orgID, refreshErr))
			continue
		}
		if refreshed {
			processed++
		}
	}
	return processed, errors.Join(batchErrors...)
}

func (w *Worker) listReadyThreadsCredentialOrganizations(
	limit int,
	refreshBefore time.Time,
) ([]uuid.UUID, error) {
	var organizationIDs []uuid.UUID
	if w.Config != nil && w.Config.Database.RLSEnabled {
		if err := w.DB.Raw(
			"SELECT * FROM public.rereply_ready_threads_credential_orgs(?, ?, ?)",
			uuid.New(),
			limit,
			refreshBefore,
		).Scan(&organizationIDs).Error; err != nil {
			return nil, fmt.Errorf("list Threads credential tenants: %w", err)
		}
		return organizationIDs, nil
	}

	if err := w.DB.Raw(`
		SELECT DISTINCT credentials.organization_id
		FROM channel_credentials AS credentials
		JOIN channel_accounts AS accounts
		  ON accounts.id = credentials.channel_account_id
		 AND accounts.organization_id = credentials.organization_id
		WHERE credentials.deleted_at IS NULL
		  AND credentials.kind = ?
		  AND credentials.status IN (?, ?)
		  AND credentials.expires_at IS NOT NULL
		  AND credentials.expires_at <= ?
		  AND accounts.deleted_at IS NULL
		  AND accounts.channel = ?
		  AND accounts.provider = ?
		  AND accounts.status = ?
		ORDER BY credentials.organization_id
		LIMIT ?
	`,
		models.ChannelCredentialKindOAuth,
		models.ChannelCredentialStatusActive,
		models.ChannelCredentialStatusExpiring,
		refreshBefore,
		models.ChannelThreads,
		channelapi.ThreadsProvider,
		models.ChannelAccountStatusActive,
		limit,
	).Scan(&organizationIDs).Error; err != nil {
		return nil, fmt.Errorf("list development Threads credential tenants: %w", err)
	}
	return organizationIDs, nil
}

func (w *Worker) refreshThreadsCredential(
	ctx context.Context,
	orgID uuid.UUID,
	refreshBefore time.Time,
) (bool, error) {
	encryptionKey := ""
	if w.Config != nil {
		encryptionKey = w.Config.App.EncryptionKey
	}
	adapter := channelapi.NewThreadsAdapter(&http.Client{Timeout: 30 * time.Second}, encryptionKey)
	return w.refreshThreadsCredentialWithProvider(
		ctx,
		orgID,
		refreshBefore,
		adapter.RefreshCredentials,
	)
}

func (w *Worker) refreshThreadsCredentialWithProvider(
	ctx context.Context,
	orgID uuid.UUID,
	refreshBefore time.Time,
	refresh threadsCredentialRefreshFunc,
) (bool, error) {
	if refresh == nil {
		return false, errors.New("threads credential refresh provider is required")
	}
	claim, processed, err := w.claimThreadsCredentialRefresh(orgID, refreshBefore, time.Now().UTC())
	if err != nil || claim == nil {
		return processed, err
	}

	// The claim transaction has committed. The credential remains active and
	// usable while these provider calls run.
	claim.Account.Credentials = []models.ChannelCredential{claim.Credential}
	result, refreshErr := refresh(ctx, &claim.Account)
	now := time.Now().UTC()
	outcome := threadsCredentialRefreshOutcome{Result: result, ProviderError: refreshErr}
	if refreshErr == nil {
		outcome.InvalidError = validateThreadsCredentialRefreshResult(result, now)
	}
	_, finalizeErr := w.finalizeThreadsCredentialRefresh(claim, outcome, now)
	if outcome.InvalidError != nil {
		return true, errors.Join(outcome.InvalidError, finalizeErr)
	}
	return true, finalizeErr
}

func (w *Worker) claimThreadsCredentialRefresh(
	orgID uuid.UUID,
	refreshBefore time.Time,
	now time.Time,
) (*threadsCredentialRefreshClaim, bool, error) {
	var claim *threadsCredentialRefreshClaim
	processed := false
	err := database.WithTenantReadCommitted(w.DB, orgID, func(tx *gorm.DB) error {
		// Account then credential is the shared serialization order used by
		// disconnect, OAuth rotation, and the refresh finalizer.
		var account models.ChannelAccount
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE", Options: "SKIP LOCKED"}).
			Where(
				"organization_id = ? AND channel = ? AND provider = ? AND status = ?",
				orgID,
				models.ChannelThreads,
				channelapi.ThreadsProvider,
				models.ChannelAccountStatusActive,
			).
			Where(
				"id IN (?)",
				tx.Model(&models.ChannelCredential{}).
					Select("channel_account_id").
					Where(
						"organization_id = ? AND kind = ? AND status IN ? AND expires_at IS NOT NULL AND expires_at <= ?",
						orgID,
						models.ChannelCredentialKindOAuth,
						[]models.ChannelCredentialStatus{
							models.ChannelCredentialStatusActive,
							models.ChannelCredentialStatusExpiring,
						},
						refreshBefore,
					),
			).
			Order("id ASC").
			First(&account).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil
			}
			return err
		}

		var credential models.ChannelCredential
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where(
				"organization_id = ? AND channel_account_id = ? AND kind = ? AND status IN ? AND expires_at IS NOT NULL AND expires_at <= ?",
				orgID,
				account.ID,
				models.ChannelCredentialKindOAuth,
				[]models.ChannelCredentialStatus{
					models.ChannelCredentialStatusActive,
					models.ChannelCredentialStatusExpiring,
				},
				refreshBefore,
			).
			Order("expires_at ASC, version DESC, id ASC").
			First(&credential).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil
			}
			return err
		}

		appID := threadsRefreshJSONBString(account.Metadata, "app_id")
		bindingActive, err := activeThreadsRefreshIntegrationBinding(tx, orgID, appID)
		if err != nil {
			return err
		}
		if !bindingActive {
			return nil
		}
		if threadsRefreshJSONBString(credential.Metadata, "app_id") != appID {
			return nil
		}
		if threadsCredentialRefreshClaimActive(credential.Metadata, now) {
			return nil
		}

		processed = true
		if credential.ExpiresAt == nil || !credential.ExpiresAt.After(now) {
			credential.Metadata = cloneThreadsRefreshJSONB(credential.Metadata)
			clearThreadsCredentialRefreshClaim(&credential)
			credential.Status = models.ChannelCredentialStatusExpired
			credential.ValidationError = "Threads authorization expired before it could be refreshed"
			credential.LastValidatedAt = &now
			if err := tx.Save(&credential).Error; err != nil {
				return err
			}
			return markThreadsRefreshFailure(tx, &account, now, credential.ValidationError, true)
		}

		token := uuid.NewString()
		credential.Metadata = cloneThreadsRefreshJSONB(credential.Metadata)
		credential.Metadata[threadsCredentialRefreshClaimTokenKey] = token
		credential.Metadata[threadsCredentialRefreshClaimTimestampKey] = now.Format(time.RFC3339Nano)
		if err := tx.Model(&credential).Update("metadata", credential.Metadata).Error; err != nil {
			return err
		}
		claim = &threadsCredentialRefreshClaim{
			OrganizationID:    orgID,
			Account:           account,
			Credential:        credential,
			CredentialID:      credential.ID,
			CredentialStatus:  credential.Status,
			CredentialVersion: credential.Version,
			Token:             token,
			AppID:             appID,
		}
		return nil
	})
	return claim, processed, err
}

func (w *Worker) finalizeThreadsCredentialRefresh(
	claim *threadsCredentialRefreshClaim,
	outcome threadsCredentialRefreshOutcome,
	now time.Time,
) (bool, error) {
	if claim == nil {
		return false, errors.New("threads credential refresh claim is required")
	}
	applied := false
	err := database.WithTenantReadCommitted(w.DB, claim.OrganizationID, func(tx *gorm.DB) error {
		var account models.ChannelAccount
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where(
				"id = ? AND organization_id = ? AND channel = ? AND provider = ?",
				claim.Account.ID,
				claim.OrganizationID,
				models.ChannelThreads,
				channelapi.ThreadsProvider,
			).
			First(&account).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil
			}
			return err
		}

		var credential models.ChannelCredential
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where(
				"id = ? AND organization_id = ? AND channel_account_id = ? AND kind = ?",
				claim.CredentialID,
				claim.OrganizationID,
				account.ID,
				models.ChannelCredentialKindOAuth,
			).
			First(&credential).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil
			}
			return err
		}
		if credential.Version != claim.CredentialVersion ||
			threadsRefreshJSONBString(credential.Metadata, threadsCredentialRefreshClaimTokenKey) != claim.Token {
			return nil
		}

		// We own this exact claim, so every handled outcome may release it.
		credential.Metadata = cloneThreadsRefreshJSONB(credential.Metadata)
		clearThreadsCredentialRefreshClaim(&credential)

		accountAppID := threadsRefreshJSONBString(account.Metadata, "app_id")
		bindingActive, err := activeThreadsRefreshIntegrationBinding(
			tx,
			claim.OrganizationID,
			accountAppID,
		)
		if err != nil {
			return err
		}
		stateCurrent := account.Status == models.ChannelAccountStatusActive &&
			account.ExternalAccountID == claim.Account.ExternalAccountID &&
			credential.Status == claim.CredentialStatus &&
			accountAppID != "" && accountAppID == claim.AppID &&
			bindingActive
		if !stateCurrent || outcome.InvalidError != nil {
			if err := tx.Model(&credential).Update("metadata", credential.Metadata).Error; err != nil {
				return err
			}
			applied = stateCurrent
			return nil
		}

		if outcome.ProviderError != nil {
			message := strings.TrimSpace(channelOutboxErrorMessage(outcome.ProviderError))
			credential.ValidationError = message
			credential.LastValidatedAt = &now
			permanent := !retryableChannelError(outcome.ProviderError)
			if permanent {
				credential.Status = models.ChannelCredentialStatusInvalid
			}
			if err := tx.Save(&credential).Error; err != nil {
				return err
			}
			applied = true
			return markThreadsRefreshFailure(tx, &account, now, message, permanent)
		}

		var maximumVersion int
		if err := tx.Model(&models.ChannelCredential{}).
			Where(
				"organization_id = ? AND channel_account_id = ? AND kind = ?",
				claim.OrganizationID,
				account.ID,
				models.ChannelCredentialKindOAuth,
			).
			Select("COALESCE(MAX(version), 0)").
			Scan(&maximumVersion).Error; err != nil {
			return err
		}
		credential.Status = models.ChannelCredentialStatusRevoked
		credential.RevokedAt = &now
		credential.RotatedAt = &now
		credential.ValidationError = ""
		credential.LastValidatedAt = &now
		if err := tx.Save(&credential).Error; err != nil {
			return err
		}

		refreshedMetadata := cloneThreadsRefreshJSONB(outcome.Result.Metadata)
		delete(refreshedMetadata, threadsCredentialRefreshClaimTokenKey)
		delete(refreshedMetadata, threadsCredentialRefreshClaimTimestampKey)
		refreshedMetadata["app_id"] = claim.AppID
		refreshed := models.ChannelCredential{
			BaseModel:        models.BaseModel{ID: uuid.New()},
			OrganizationID:   claim.OrganizationID,
			ChannelAccountID: account.ID,
			Kind:             models.ChannelCredentialKindOAuth,
			Version:          maximumVersion + 1,
			CredentialBlob:   cloneThreadsRefreshJSONB(outcome.Result.CredentialBlob),
			Status:           models.ChannelCredentialStatusActive,
			KeyVersion:       outcome.Result.KeyVersion,
			ExpiresAt:        outcome.Result.ExpiresAt,
			LastValidatedAt:  &now,
			Metadata:         refreshedMetadata,
		}
		if err := tx.Create(&refreshed).Error; err != nil {
			return err
		}

		account.Status = models.ChannelAccountStatusActive
		if account.Config == nil {
			account.Config = models.JSONB{}
		}
		account.Config["outbound_enabled"] = true
		if account.Metadata == nil {
			account.Metadata = models.JSONB{}
		}
		if scopes, ok := outcome.Result.Metadata["granted_scopes"]; ok {
			account.Metadata["granted_scopes"] = scopes
		}
		if checkedAt, ok := outcome.Result.Metadata["permissions_checked_at"]; ok {
			account.Metadata["permissions_verified_at"] = checkedAt
		}
		if dataAccessExpiresAt, ok := outcome.Result.Metadata["data_access_expires_at"]; ok {
			account.Metadata["data_access_expires_at"] = dataAccessExpiresAt
		} else {
			delete(account.Metadata, "data_access_expires_at")
		}
		account.LastHealthCheckAt = &now
		account.LastErrorAt = nil
		account.LastError = ""
		if err := tx.Save(&account).Error; err != nil {
			return err
		}
		applied = true
		return nil
	})
	return applied, err
}

func activeThreadsRefreshIntegrationBinding(
	tx *gorm.DB,
	organizationID uuid.UUID,
	expectedAppID string,
) (bool, error) {
	expectedAppID = strings.TrimSpace(expectedAppID)
	if expectedAppID == "" {
		return false, nil
	}
	var integration models.ProviderIntegration
	err := tx.Where(
		"organization_id = ? AND provider = ? AND enabled = ?",
		organizationID,
		channelapi.ThreadsProvider,
		true,
	).First(&integration).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if integration.ThreadsAppID == nil || strings.TrimSpace(*integration.ThreadsAppID) != expectedAppID {
		return false, nil
	}
	return threadsRefreshJSONBString(integration.Config, "app_id") == expectedAppID, nil
}

func validateThreadsCredentialRefreshResult(
	result channelapi.CredentialRefreshResult,
	now time.Time,
) error {
	if len(result.CredentialBlob) == 0 || strings.TrimSpace(result.KeyVersion) == "" ||
		result.ExpiresAt == nil || !result.ExpiresAt.After(now) {
		return errors.New("threads credential refresh returned invalid durable state")
	}
	return nil
}

func threadsCredentialRefreshClaimActive(metadata models.JSONB, now time.Time) bool {
	token := threadsRefreshJSONBString(metadata, threadsCredentialRefreshClaimTokenKey)
	claimedAtValue := threadsRefreshJSONBString(metadata, threadsCredentialRefreshClaimTimestampKey)
	if token == "" || claimedAtValue == "" {
		return false
	}
	claimedAt, err := time.Parse(time.RFC3339Nano, claimedAtValue)
	if err != nil || claimedAt.After(now.Add(time.Minute)) {
		return false
	}
	return claimedAt.Add(threadsCredentialRefreshClaimTTL).After(now)
}

func clearThreadsCredentialRefreshClaim(credential *models.ChannelCredential) {
	if credential == nil || credential.Metadata == nil {
		return
	}
	delete(credential.Metadata, threadsCredentialRefreshClaimTokenKey)
	delete(credential.Metadata, threadsCredentialRefreshClaimTimestampKey)
}

func threadsRefreshJSONBString(value models.JSONB, key string) string {
	if value == nil {
		return ""
	}
	text, _ := value[key].(string)
	return strings.TrimSpace(text)
}

func cloneThreadsRefreshJSONB(value models.JSONB) models.JSONB {
	cloned := make(models.JSONB, len(value)+1)
	for key, entry := range value {
		cloned[key] = entry
	}
	return cloned
}

func markThreadsRefreshFailure(
	tx *gorm.DB,
	account *models.ChannelAccount,
	now time.Time,
	message string,
	permanent bool,
) error {
	if account == nil {
		return errors.New("threads account is required")
	}
	account.LastHealthCheckAt = &now
	account.LastErrorAt = &now
	account.LastError = strings.TrimSpace(message)
	if permanent {
		account.Status = models.ChannelAccountStatusDegraded
		if account.Config == nil {
			account.Config = models.JSONB{}
		}
		account.Config["outbound_enabled"] = false
	}
	return tx.Save(account).Error
}
