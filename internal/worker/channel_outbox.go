package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	channelapi "github.com/shridarpatil/whatomate/internal/channel"
	"github.com/shridarpatil/whatomate/internal/config"
	"github.com/shridarpatil/whatomate/internal/database"
	"github.com/shridarpatil/whatomate/internal/metaregistry"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/internal/threadsreview"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	defaultChannelOutboxBatchSize    = 20
	defaultChannelOutboxPollInterval = time.Second
	defaultChannelOutboxLease        = 2 * time.Minute
)

var (
	errChannelOutboxUnlicensed        = errors.New("omnichannel entitlement is not active")
	errChannelOutboxThreadsUnlicensed = errors.New("threads public engagement entitlement is not active")
	errChannelOutboxThreadsBinding    = errors.New("threads app binding is not active")
	errChannelOutboxConsent           = errors.New("contact consent does not permit delivery")
	errChannelOutboxAIPolicy          = errors.New("automatic AI reply is no longer eligible")
	errChannelOutboxManagedMetaFence  = errors.New("managed Meta delivery authorization is no longer current")
)

const managedInstagramChannelOutboxProviderOperationLimit = 90 * time.Second

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

	now := time.Now().UTC()
	organizationIDs, err := w.listReadyChannelOutboxOrganizations(
		limit,
		now,
		now.Add(-defaultChannelOutboxLease),
	)
	if err != nil {
		return 0, err
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

func (w *Worker) listReadyChannelOutboxOrganizations(
	limit int,
	now time.Time,
	staleBefore time.Time,
) ([]uuid.UUID, error) {
	cursor := uuid.New()
	var organizationIDs []uuid.UUID

	if w.Config != nil && w.Config.Database.RLSEnabled {
		// RLS workers may only discover tenant IDs through this narrowly
		// granted SECURITY DEFINER resolver. A missing resolver fails closed.
		if err := w.DB.Raw(
			"SELECT * FROM public.rereply_ready_channel_outbox_orgs(?, ?, ?)",
			cursor,
			limit,
			staleBefore,
		).Scan(&organizationIDs).Error; err != nil {
			return nil, fmt.Errorf("list channel outbox tenants: %w", err)
		}
		return organizationIDs, nil
	}

	// Development and CI intentionally run without RLS or its routing
	// functions. Query only ready outbox tenants directly instead of invoking
	// an RLS-only resolver and recovering from a predictable database error.
	if err := w.DB.Raw(`
		WITH ready AS (
			SELECT DISTINCT organization_id
			FROM outbox_jobs
			WHERE deleted_at IS NULL
			  AND available_at <= ?
			  AND (
			    status IN (?, ?)
			    OR (
			      status IN (?, ?)
			      AND (locked_at IS NULL OR locked_at < ?)
			    )
			  )
		)
		SELECT organization_id
		FROM ready
		ORDER BY (organization_id > ?) DESC, organization_id
		LIMIT ?
	`,
		now,
		models.OutboxJobStatusPending,
		models.OutboxJobStatusRetrying,
		models.OutboxJobStatusProcessing,
		models.OutboxJobStatusDispatching,
		staleBefore,
		cursor,
		limit,
	).Scan(&organizationIDs).Error; err != nil {
		return nil, fmt.Errorf("list development channel outbox tenants: %w", err)
	}
	return organizationIDs, nil
}

func (w *Worker) claimChannelOutboxJob(orgID uuid.UUID, workerID string) (uuid.UUID, bool, error) {
	var jobID uuid.UUID
	claimed := false
	err := database.WithTenant(w.DB, orgID, func(tx *gorm.DB) error {
		now := time.Now().UTC()
		staleBefore := now.Add(-defaultChannelOutboxLease)
		var candidate struct {
			ID        uuid.UUID
			Status    models.OutboxJobStatus
			MessageID *uuid.UUID
		}
		if err := tx.Raw(`
			SELECT id, status, message_id
			FROM outbox_jobs
			WHERE organization_id = ?
			  AND deleted_at IS NULL
			  AND available_at <= ?
			  AND (
			    status IN (?, ?)
			    OR (status IN (?, ?) AND (locked_at IS NULL OR locked_at < ?))
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
			models.OutboxJobStatusDispatching,
			staleBefore,
		).Scan(&candidate).Error; err != nil {
			return err
		}
		if candidate.ID == uuid.Nil {
			return nil
		}
		if candidate.Status == models.OutboxJobStatusDispatching {
			// A process that dies after crossing the delivery fence may have
			// reached the provider. Retrying would risk a duplicate, so recover
			// the abandoned row as an explicit ambiguous terminal failure.
			result := tx.Model(&models.OutboxJob{}).
				Where(
					"id = ? AND organization_id = ? AND status = ?",
					candidate.ID,
					orgID,
					models.OutboxJobStatusDispatching,
				).
				Updates(map[string]any{
					"status":          models.OutboxJobStatusFailed,
					"failed_at":       now,
					"last_attempt_at": now,
					"last_error_code": "delivery_state_unknown",
					"last_error":      "Provider delivery state is unknown after an interrupted dispatch",
					"locked_at":       nil,
					"locked_by":       "",
					"updated_at":      now,
				})
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected != 1 {
				return errors.New("abandoned channel outbox dispatch recovery lost its lease")
			}
			if candidate.MessageID != nil {
				if err := tx.Model(&models.Message{}).
					Where(
						"id = ? AND organization_id = ? AND status = ?",
						*candidate.MessageID,
						orgID,
						models.MessageStatusPending,
					).
					Updates(map[string]any{
						"status":        models.MessageStatusFailed,
						"error_message": "Provider delivery state is unknown; review before retrying",
						"updated_at":    now,
					}).Error; err != nil {
					return err
				}
			}
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
		now := time.Now().UTC()
		if err := tx.
			Preload("Credentials", func(credentials *gorm.DB) *gorm.DB {
				return credentials.
					Where(
						"organization_id = ? AND status IN ? AND (expires_at IS NULL OR expires_at > ?)",
						orgID,
						[]models.ChannelCredentialStatus{
							models.ChannelCredentialStatusActive,
							models.ChannelCredentialStatusExpiring,
						},
						now,
					).
					Order("version DESC").
					Order("id ASC")
			}).
			Where("id = ? AND organization_id = ?", job.ChannelAccountID, orgID).
			First(&account).Error; err != nil {
			return err
		}
		if account.Channel == models.ChannelThreads {
			threadsEntitled, err := hasDurableChannelOutboxEntitlement(
				tx,
				orgID,
				channelapi.ThreadsPublicEngagementEntitlementKey,
				now,
			)
			if err != nil {
				return fmt.Errorf("evaluate Threads public engagement entitlement: %w", err)
			}
			if !threadsEntitled {
				return errChannelOutboxThreadsUnlicensed
			}
			if err := validateThreadsChannelOutboxBinding(w.Config, tx, orgID, &account); err != nil {
				return err
			}
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
		if job.ID != uuid.Nil && (errors.Is(loadErr, errChannelOutboxUnlicensed) ||
			errors.Is(loadErr, errChannelOutboxThreadsUnlicensed) ||
			errors.Is(loadErr, errChannelOutboxThreadsBinding)) {
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
	if preparedSender, ok := adapter.(channelapi.PreparedSender); ok {
		return w.deliverPreparedChannelOutboxJob(
			ctx,
			orgID,
			&job,
			&account,
			workerID,
			outbound,
			preparedSender,
		)
	}
	if isChannelAIReplyOutbox(&job, outbound) {
		fencedAccount, err := w.recheckChannelAIOutboxDispatchWithAccount(
			orgID,
			job.ID,
			workerID,
			&account,
		)
		if err != nil {
			return w.cancelChannelAIOutboxJob(
				orgID,
				&job,
				workerID,
				err,
			)
		}
		account = *fencedAccount
		job.Status = models.OutboxJobStatusDispatching
	} else if isManagedMetaChannelOutboxAccount(&account) {
		// This is the provider-attempt boundary for ordinary managed-Meta
		// messages. Re-read the runtime binding and exact credential generation
		// under the shared organization mutex, then atomically cross processing
		// to dispatching immediately before the network call.
		fencedAccount, fenceErr := w.markManagedMetaChannelOutboxDispatching(
			orgID,
			job.ID,
			workerID,
			&account,
		)
		if fenceErr != nil {
			return w.failManagedMetaChannelOutboxFence(
				orgID,
				&job,
				workerID,
				fenceErr,
			)
		}
		account = *fencedAccount
		job.Status = models.OutboxJobStatusDispatching
	}

	result, sendErr := adapter.Send(ctx, &account, outbound)
	if sendErr != nil {
		return w.failChannelOutboxProviderAttempt(
			orgID,
			&job,
			&account,
			workerID,
			sendErr,
			retryableChannelError(sendErr),
		)
	}
	if len(result.ProviderMessageIDs) == 0 {
		return w.failChannelOutboxProviderAttempt(
			orgID,
			&job,
			&account,
			workerID,
			errors.New("provider accepted the request without a message ID"),
			true,
		)
	}
	return w.completeChannelOutboxJob(orgID, &job, &account, workerID, result)
}

func validateThreadsChannelOutboxBinding(
	cfg *config.Config,
	tx *gorm.DB,
	orgID uuid.UUID,
	account *models.ChannelAccount,
) error {
	if tx == nil || account == nil || account.Channel != models.ChannelThreads ||
		account.Provider != channelapi.ThreadsProvider {
		return errChannelOutboxThreadsBinding
	}
	var integration models.ProviderIntegration
	if err := tx.Where(
		"organization_id = ? AND provider = ? AND enabled = ?",
		orgID,
		channelapi.ThreadsProvider,
		true,
	).First(&integration).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return errChannelOutboxThreadsBinding
		}
		return fmt.Errorf("load Threads app binding: %w", err)
	}
	integrationAppID := strings.TrimSpace(channelOutboxText(integration.Config["app_id"]))
	accountAppID := strings.TrimSpace(channelOutboxText(account.Metadata["app_id"]))
	if integration.ThreadsAppID == nil || integrationAppID == "" || accountAppID == "" ||
		strings.TrimSpace(*integration.ThreadsAppID) != integrationAppID ||
		accountAppID != integrationAppID || len(account.Credentials) == 0 {
		return errChannelOutboxThreadsBinding
	}
	if threadsreview.AccessMode(
		cfg,
		orgID,
		integration.Config,
		integrationAppID,
		account.ExternalAccountID,
	) == threadsreview.ModeBlocked {
		return errChannelOutboxThreadsBinding
	}
	credentialAppID := strings.TrimSpace(channelOutboxText(account.Credentials[0].Metadata["app_id"]))
	if credentialAppID == "" || credentialAppID != integrationAppID {
		return errChannelOutboxThreadsBinding
	}
	return nil
}

type managedMetaCredentialGeneration struct {
	OAuthID               uuid.UUID
	OAuthVersion          int
	OAuthCreatedAt        time.Time
	OAuthExpiresAt        time.Time
	OAuthHasExpiry        bool
	OAuthOrganizationID   uuid.UUID
	OAuthAccountID        uuid.UUID
	WebhookID             uuid.UUID
	WebhookVersion        int
	WebhookCreatedAt      time.Time
	WebhookOrganizationID uuid.UUID
	WebhookAccountID      uuid.UUID
}

// isManagedMetaChannelOutboxAccount detects reserved managed intent. Either
// marker is enough to enter the fail-closed fence; only
// exactManagedMetaChannelOutboxBinding may authorize provider dispatch.
func isManagedMetaChannelOutboxAccount(account *models.ChannelAccount) bool {
	if account == nil || account.Provider != channelapi.RelayProvider ||
		(account.Channel != models.ChannelInstagram && account.Channel != models.ChannelMessenger) {
		return false
	}
	if channelOutboxBool(account.Config, "meta_registry_managed") ||
		channelOutboxText(account.Config["meta_management_mode"]) ==
			metaregistry.ManagementModePlatformOAuth {
		return true
	}
	if account.Channel != models.ChannelInstagram ||
		!managedMetaCanonicalNumericID(channelOutboxText(account.Metadata["meta_platform_app_id"])) ||
		!managedMetaCanonicalNumericID(account.ExternalAccountID) {
		return false
	}
	identityResidue := channelOutboxText(account.Config["instagram_api_mode"]) == "instagram_login" &&
		channelOutboxText(account.Metadata["meta_webhook_app"]) == "instagram_login"
	subscriptionResidue := channelOutboxText(
		account.Metadata["meta_subscription_operation_id"],
	) != ""
	managedURLResidue := strings.HasSuffix(
		channelOutboxText(account.Config["rereply_webhook_url"]),
		"/api/webhooks/channels/"+account.ID.String(),
	) && strings.HasSuffix(
		channelOutboxText(account.Config["relay_url"]),
		"/v1/accounts/instagram/"+account.ExternalAccountID,
	)
	return identityResidue || subscriptionResidue || managedURLResidue
}

func managedMetaCanonicalNumericID(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return false
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			return false
		}
	}
	return true
}

func exactManagedMetaChannelOutboxBinding(account *models.ChannelAccount) bool {
	return isManagedMetaChannelOutboxAccount(account) &&
		channelOutboxBool(account.Config, "meta_registry_managed") &&
		channelOutboxText(account.Config["meta_management_mode"]) ==
			metaregistry.ManagementModePlatformOAuth
}

func managedMetaCurrentCredentialGeneration(
	credentials []models.ChannelCredential,
	now time.Time,
) (managedMetaCredentialGeneration, bool) {
	var generation managedMetaCredentialGeneration
	current := 0
	for index := range credentials {
		credential := &credentials[index]
		if !channelapi.CredentialIsCurrent(credential, now) {
			continue
		}
		current++
		switch credential.Kind {
		case models.ChannelCredentialKindOAuth:
			if generation.OAuthID != uuid.Nil {
				return managedMetaCredentialGeneration{}, false
			}
			generation.OAuthID = credential.ID
			generation.OAuthVersion = credential.Version
			generation.OAuthCreatedAt = credential.CreatedAt
			generation.OAuthOrganizationID = credential.OrganizationID
			generation.OAuthAccountID = credential.ChannelAccountID
			if credential.ExpiresAt != nil {
				generation.OAuthHasExpiry = true
				generation.OAuthExpiresAt = credential.ExpiresAt.UTC()
			}
		case models.ChannelCredentialKindWebhook:
			if generation.WebhookID != uuid.Nil {
				return managedMetaCredentialGeneration{}, false
			}
			generation.WebhookID = credential.ID
			generation.WebhookVersion = credential.Version
			generation.WebhookCreatedAt = credential.CreatedAt
			generation.WebhookOrganizationID = credential.OrganizationID
			generation.WebhookAccountID = credential.ChannelAccountID
		default:
			return managedMetaCredentialGeneration{}, false
		}
	}
	return generation, current == 2 && generation.OAuthID != uuid.Nil &&
		generation.OAuthVersion > 0 && generation.WebhookID != uuid.Nil &&
		generation.WebhookVersion > 0
}

func sameManagedMetaCredentialGeneration(
	left, right managedMetaCredentialGeneration,
) bool {
	return left.OAuthID == right.OAuthID && left.OAuthVersion == right.OAuthVersion &&
		left.WebhookID == right.WebhookID && left.WebhookVersion == right.WebhookVersion
}

func managedInstagramCredentialGenerationValid(
	generation managedMetaCredentialGeneration,
	organizationID, accountID uuid.UUID,
	now time.Time,
) bool {
	return generation.OAuthID != uuid.Nil && generation.OAuthVersion > 0 &&
		generation.WebhookID != uuid.Nil && generation.WebhookVersion > 0 &&
		generation.OAuthID != generation.WebhookID &&
		generation.OAuthOrganizationID == organizationID &&
		generation.WebhookOrganizationID == organizationID &&
		generation.OAuthAccountID == accountID && generation.WebhookAccountID == accountID &&
		!generation.OAuthCreatedAt.IsZero() &&
		!generation.OAuthCreatedAt.After(now.Add(time.Minute)) &&
		!generation.WebhookCreatedAt.IsZero() &&
		!generation.WebhookCreatedAt.After(now.Add(time.Minute)) &&
		generation.OAuthHasExpiry && generation.OAuthExpiresAt.After(
		now.Add(managedInstagramChannelOutboxProviderOperationLimit),
	)
}

// markManagedMetaChannelOutboxDispatching is the last local authorization
// check before an ordinary relay adapter may send. Its first lock is the exact
// organizations.id FOR UPDATE mutex used by
// handlers.lockChannelAIOrganizationScopeTx. Lifecycle changes and dispatch
// therefore have a deterministic winner with lock order organization -> job ->
// account -> credentials.
func (w *Worker) markManagedMetaChannelOutboxDispatching(
	orgID, jobID uuid.UUID,
	workerID string,
	expectedAccount *models.ChannelAccount,
) (*models.ChannelAccount, error) {
	if w == nil || w.DB == nil || expectedAccount == nil || orgID == uuid.Nil ||
		jobID == uuid.Nil || strings.TrimSpace(workerID) == "" {
		return nil, errChannelOutboxManagedMetaFence
	}
	expectedAt := time.Now().UTC()
	expectedGeneration, ok := managedMetaCurrentCredentialGeneration(
		expectedAccount.Credentials,
		expectedAt,
	)
	if !ok || !isManagedMetaChannelOutboxAccount(expectedAccount) {
		return nil, errChannelOutboxManagedMetaFence
	}
	if expectedAccount.Channel == models.ChannelInstagram &&
		!managedInstagramCredentialGenerationValid(
			expectedGeneration, orgID, expectedAccount.ID, expectedAt,
		) {
		return nil, errChannelOutboxManagedMetaFence
	}

	var fencedAccount models.ChannelAccount
	err := database.WithTenantReadCommitted(w.DB, orgID, func(tx *gorm.DB) error {
		if err := lockChannelOutboxOrganizationScopeTx(tx, orgID); err != nil {
			return err
		}
		var job models.OutboxJob
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Select("id", "organization_id", "channel_account_id", "status", "locked_by").
			Where(
				"id = ? AND organization_id = ? AND channel_account_id = ? AND status = ? AND locked_by = ?",
				jobID,
				orgID,
				expectedAccount.ID,
				models.OutboxJobStatusProcessing,
				workerID,
			).
			First(&job).Error; err != nil {
			return fmt.Errorf("%w: delivery job lease changed", errChannelOutboxManagedMetaFence)
		}
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ? AND organization_id = ?", expectedAccount.ID, orgID).
			First(&fencedAccount).Error; err != nil {
			return fmt.Errorf("%w: channel account is unavailable", errChannelOutboxManagedMetaFence)
		}
		if fencedAccount.Channel != expectedAccount.Channel ||
			fencedAccount.Provider != expectedAccount.Provider ||
			fencedAccount.ExternalAccountID != expectedAccount.ExternalAccountID {
			return fmt.Errorf("%w: channel account binding changed", errChannelOutboxManagedMetaFence)
		}
		now := time.Now().UTC()
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where(
				"organization_id = ? AND channel_account_id = ? AND status IN ? AND (expires_at IS NULL OR expires_at > ?)",
				orgID,
				fencedAccount.ID,
				[]models.ChannelCredentialStatus{
					models.ChannelCredentialStatusActive,
					models.ChannelCredentialStatusExpiring,
				},
				now,
			).
			Order("version DESC").
			Order("id ASC").
			Find(&fencedAccount.Credentials).Error; err != nil {
			return err
		}
		currentGeneration, ok := managedMetaCurrentCredentialGeneration(
			fencedAccount.Credentials,
			now,
		)
		if !ok || !sameManagedMetaCredentialGeneration(expectedGeneration, currentGeneration) ||
			!w.managedMetaChannelOutboxRuntimeAllowed(&fencedAccount, currentGeneration, now) {
			return errChannelOutboxManagedMetaFence
		}
		result := tx.Model(&models.OutboxJob{}).
			Where(
				"id = ? AND organization_id = ? AND channel_account_id = ? AND status = ? AND locked_by = ?",
				job.ID,
				orgID,
				fencedAccount.ID,
				models.OutboxJobStatusProcessing,
				workerID,
			).
			Updates(map[string]any{
				"status":     models.OutboxJobStatusDispatching,
				"locked_at":  now,
				"updated_at": now,
			})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return fmt.Errorf("%w: delivery fence lost its lease", errChannelOutboxManagedMetaFence)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &fencedAccount, nil
}

func lockChannelOutboxOrganizationScopeTx(tx *gorm.DB, organizationID uuid.UUID) error {
	if tx == nil || organizationID == uuid.Nil {
		return errors.New("tenant organization outbox transaction is required")
	}
	var organization models.Organization
	return tx.Clauses(clause.Locking{Strength: "UPDATE"}).
		Select("id").
		Where("id = ?", organizationID).
		First(&organization).Error
}

func (w *Worker) failManagedMetaChannelOutboxFence(
	orgID uuid.UUID,
	job *models.OutboxJob,
	workerID string,
	fenceErr error,
) error {
	settleErr := w.failChannelOutboxJob(orgID, job, workerID, fenceErr, false)
	if settleErr == nil {
		return nil
	}
	// A lifecycle transaction that won the organization mutex also cancels the
	// processing row and clears its lease. In that expected race the failed CAS
	// is already durably settled; do not turn it into a noisy worker retry.
	var status models.OutboxJobStatus
	lookupErr := database.WithTenant(w.DB, orgID, func(tx *gorm.DB) error {
		return tx.Model(&models.OutboxJob{}).
			Where("id = ? AND organization_id = ?", job.ID, orgID).
			Pluck("status", &status).Error
	})
	if lookupErr == nil && (status == models.OutboxJobStatusCancelled ||
		status == models.OutboxJobStatusFailed) {
		return nil
	}
	return settleErr
}

func (w *Worker) managedMetaChannelOutboxRuntimeAllowed(
	account *models.ChannelAccount,
	generation managedMetaCredentialGeneration,
	now time.Time,
) bool {
	if w == nil || w.Config == nil || !w.Config.MetaRegistry.Enabled ||
		!exactManagedMetaChannelOutboxBinding(account) ||
		account.Status != models.ChannelAccountStatusActive ||
		!channelOutboxBool(account.Config, "outbound_enabled") ||
		channelOutboxText(account.Metadata["meta_ownership_state"]) != metaregistry.OwnershipVerified ||
		channelOutboxText(account.Metadata["meta_deauthorized_at"]) != "" ||
		channelOutboxText(account.Metadata["meta_subscription_desired_state"]) != "subscribed" ||
		channelOutboxText(account.Metadata["meta_subscription_operation_state"]) != "subscribe_complete" ||
		channelOutboxText(account.Metadata["meta_subscription_remote_state"]) != "subscribed" {
		return false
	}
	checkedAt, err := time.Parse(
		time.RFC3339Nano,
		channelOutboxText(account.Metadata["meta_ownership_checked_at"]),
	)
	if err != nil {
		return false
	}
	maxAge := time.Duration(w.Config.MetaRegistry.OwnershipMaxAgeMins) * time.Minute
	if maxAge <= 0 || maxAge > 7*24*time.Hour {
		maxAge = 24 * time.Hour
	}
	if checkedAt.After(now.Add(time.Minute)) || now.Sub(checkedAt) > maxAge {
		return false
	}

	switch account.Channel {
	case models.ChannelInstagram:
		return w.managedInstagramChannelOutboxRuntimeAllowed(account, generation, now)
	case models.ChannelMessenger:
		return w.managedMessengerChannelOutboxRuntimeAllowed(account)
	default:
		return false
	}
}

func (w *Worker) managedInstagramChannelOutboxRuntimeAllowed(
	account *models.ChannelAccount,
	generation managedMetaCredentialGeneration,
	now time.Time,
) bool {
	settings := w.Config.MetaInstagram
	authorizedAt, authorizedAtErr := time.Parse(
		time.RFC3339Nano,
		channelOutboxText(account.Metadata["meta_authorized_at"]),
	)
	if !settings.Enabled || !settings.OrganizationReleased(account.OrganizationID.String()) ||
		!managedMetaCanonicalID(strings.TrimSpace(settings.AppID)) ||
		!managedMetaCanonicalID(account.ExternalAccountID) ||
		channelOutboxText(account.Config["instagram_api_mode"]) != "instagram_login" ||
		channelOutboxText(account.Metadata["meta_platform_app_id"]) != strings.TrimSpace(settings.AppID) ||
		channelOutboxText(account.Metadata["meta_webhook_app"]) != "instagram_login" ||
		!managedMetaCanonicalID(channelOutboxText(account.Metadata["meta_authorizing_user_id"])) ||
		channelOutboxText(account.Metadata["meta_oauth_subject_id"]) !=
			channelOutboxText(account.Metadata["meta_authorizing_user_id"]) ||
		channelOutboxText(account.Metadata["meta_authority_asset_id"]) != account.ExternalAccountID ||
		strings.ToUpper(channelOutboxText(account.Metadata["meta_authorization_token_kind"])) != "USER" ||
		authorizedAtErr != nil || authorizedAt.IsZero() || authorizedAt.After(now.Add(time.Minute)) ||
		!managedInstagramCredentialGenerationValid(
			generation, account.OrganizationID, account.ID, now,
		) ||
		!managedInstagramChannelOutboxURLsAllowed(settings, account) ||
		channelOutboxText(account.Metadata["meta_data_deletion_pending_digest"]) != "" ||
		channelOutboxText(account.Metadata["meta_data_deletion_pending_issued_at"]) != "" ||
		channelOutboxText(account.Metadata["meta_deauthorization_pending_digest"]) != "" ||
		channelOutboxText(account.Metadata["meta_deauthorization_pending_issued_at"]) != "" ||
		!channelOutboxScopesInclude(
			account.Metadata["meta_granted_scopes"],
			"instagram_business_basic",
			"instagram_business_manage_messages",
		) ||
		!managedInstagramSubscriptionGenerationMatches(account.Metadata, generation) {
		return false
	}

	reviewStatus := strings.ToLower(strings.TrimSpace(settings.AppReviewStatus))
	mode := channelOutboxText(account.Metadata["meta_release_evidence_mode"])
	if reviewStatus == "approved" {
		return mode == "app_review_approved" &&
			channelOutboxText(account.Metadata["meta_release_review_status"]) == "approved"
	}
	if strings.EqualFold(strings.TrimSpace(w.Config.App.Environment), "production") ||
		mode != "development_app_role" ||
		!managedInstagramDevelopmentRoleAllowed(settings.DevelopmentAppRole) {
		return false
	}
	profileID := strings.TrimSpace(settings.DevelopmentTestProfileID)
	oauthSubjectID := strings.TrimSpace(settings.DevelopmentTestOAuthSubjectID)
	return managedMetaCanonicalID(profileID) && managedMetaCanonicalID(oauthSubjectID) &&
		account.ExternalAccountID == profileID &&
		channelOutboxText(account.Metadata["meta_authorizing_user_id"]) == oauthSubjectID &&
		channelOutboxText(account.Metadata["meta_release_profile_id"]) == profileID &&
		channelOutboxText(account.Metadata["meta_release_oauth_subject_id"]) == oauthSubjectID &&
		channelOutboxText(account.Metadata["meta_release_app_role"]) ==
			strings.ToLower(strings.TrimSpace(settings.DevelopmentAppRole)) &&
		channelOutboxText(account.Metadata["meta_release_review_status"]) == reviewStatus
}

func managedInstagramChannelOutboxURLsAllowed(
	settings config.MetaInstagramConfig,
	account *models.ChannelAccount,
) bool {
	if account == nil || account.ID == uuid.Nil || !managedMetaCanonicalID(account.ExternalAccountID) {
		return false
	}
	expectedWebhook, ok := managedInstagramDerivedChannelURL(
		settings.ReReplyBaseURL,
		"api", "webhooks", "channels", account.ID.String(),
	)
	if !ok {
		return false
	}
	expectedRelay, ok := managedInstagramDerivedChannelURL(
		settings.RelayBaseURL,
		"v1", "accounts", string(models.ChannelInstagram), account.ExternalAccountID,
	)
	if !ok {
		return false
	}
	webhook, webhookOK := account.Config["rereply_webhook_url"].(string)
	relay, relayOK := account.Config["relay_url"].(string)
	return webhookOK && relayOK && managedInstagramExactChannelURL(webhook, expectedWebhook) &&
		managedInstagramExactChannelURL(relay, expectedRelay)
}

func managedInstagramDerivedChannelURL(base string, segments ...string) (string, bool) {
	parsed, err := url.Parse(strings.TrimSpace(base))
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.User != nil ||
		parsed.Fragment != "" || parsed.RawQuery != "" || parsed.RawPath != "" || parsed.ForceQuery ||
		parsed.Opaque != "" {
		return "", false
	}
	joined, err := url.JoinPath(parsed.String(), segments...)
	if err != nil {
		return "", false
	}
	return joined, true
}

func managedInstagramExactChannelURL(actual, expected string) bool {
	if actual == "" || actual != expected {
		return false
	}
	parsed, err := url.Parse(actual)
	return err == nil && parsed.Scheme == "https" && parsed.Hostname() != "" &&
		parsed.User == nil && parsed.Fragment == "" && parsed.RawQuery == "" &&
		parsed.RawPath == "" && !parsed.ForceQuery && parsed.Opaque == ""
}

func (w *Worker) managedMessengerChannelOutboxRuntimeAllowed(
	account *models.ChannelAccount,
) bool {
	settings := w.Config.MetaMessenger
	if !settings.Enabled || !managedMetaCanonicalID(strings.TrimSpace(settings.AppID)) ||
		channelOutboxText(account.Metadata["meta_platform_app_id"]) != strings.TrimSpace(settings.AppID) ||
		channelOutboxText(account.Metadata["meta_webhook_app"]) != "messenger" ||
		channelOutboxText(account.Metadata["meta_authorizing_user_id"]) == "" ||
		channelOutboxText(account.Metadata["meta_business_id"]) == "" ||
		!channelOutboxScopesInclude(
			account.Metadata["meta_granted_scopes"],
			"public_profile",
			"pages_show_list",
			"pages_messaging",
			"pages_manage_metadata",
			"business_management",
		) {
		return false
	}
	allowedOrganization := settings.AllowAllOrganizations
	if !allowedOrganization {
		for _, configured := range strings.Split(settings.AllowedOrganizationIDs, ",") {
			if strings.TrimSpace(configured) == account.OrganizationID.String() {
				allowedOrganization = true
				break
			}
		}
	}
	if !allowedOrganization {
		return false
	}
	tokenKind := strings.ToUpper(channelOutboxText(account.Metadata["meta_authorization_token_kind"]))
	if tokenKind == "SYSTEM_USER" {
		return true
	}
	return tokenKind == "USER" && settings.AllowDevelopmentUserToken &&
		!settings.AllowAllOrganizations &&
		!strings.EqualFold(strings.TrimSpace(w.Config.App.Environment), "production")
}

func managedInstagramSubscriptionGenerationMatches(
	metadata models.JSONB,
	generation managedMetaCredentialGeneration,
) bool {
	operationID, operationErr := uuid.Parse(channelOutboxText(metadata["meta_subscription_operation_id"]))
	oauthID, oauthErr := uuid.Parse(channelOutboxText(metadata["meta_subscription_oauth_credential_id"]))
	webhookID, webhookErr := uuid.Parse(channelOutboxText(metadata["meta_subscription_webhook_credential_id"]))
	_, expiresErr := time.Parse(
		time.RFC3339Nano,
		channelOutboxText(metadata["meta_subscription_operation_expires_at"]),
	)
	return operationErr == nil && operationID != uuid.Nil && oauthErr == nil && webhookErr == nil &&
		oauthID == generation.OAuthID &&
		channelOutboxInt(metadata["meta_subscription_oauth_version"]) == generation.OAuthVersion &&
		webhookID == generation.WebhookID &&
		channelOutboxInt(metadata["meta_subscription_webhook_version"]) == generation.WebhookVersion &&
		expiresErr == nil
}

func channelOutboxScopesInclude(value any, required ...string) bool {
	granted := make(map[string]bool)
	switch values := value.(type) {
	case []string:
		for _, scope := range values {
			granted[strings.TrimSpace(scope)] = true
		}
	case []any:
		for _, raw := range values {
			scope, _ := raw.(string)
			granted[strings.TrimSpace(scope)] = true
		}
	default:
		return false
	}
	for _, scope := range required {
		if !granted[scope] {
			return false
		}
	}
	return true
}

func channelOutboxInt(value any) int {
	switch typed := value.(type) {
	case int:
		return typed
	case int32:
		return int(typed)
	case int64:
		return int(typed)
	case float64:
		return int(typed)
	case json.Number:
		parsed, _ := typed.Int64()
		return int(parsed)
	default:
		return 0
	}
}

func managedInstagramDevelopmentRoleAllowed(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "administrator", "developer", "tester":
		return true
	default:
		return false
	}
}

func managedMetaCanonicalID(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 64 {
		return false
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			return false
		}
	}
	return true
}

func channelOutboxText(value any) string {
	if value == nil {
		return ""
	}
	text, _ := value.(string)
	return text
}

func (w *Worker) deliverPreparedChannelOutboxJob(
	ctx context.Context,
	orgID uuid.UUID,
	job *models.OutboxJob,
	account *models.ChannelAccount,
	workerID string,
	outbound channelapi.OutboundMessage,
	sender channelapi.PreparedSender,
) error {
	if len(job.ProviderState) == 0 {
		providerState, prepareErr := sender.PrepareSend(ctx, account, outbound)
		if prepareErr != nil {
			return w.failChannelOutboxJob(
				orgID,
				job,
				workerID,
				prepareErr,
				retryableChannelError(prepareErr),
			)
		}
		if len(providerState) == 0 {
			return w.failChannelOutboxJob(
				orgID,
				job,
				workerID,
				&channelapi.ProviderError{
					Operation: "prepare_send",
					Provider:  account.Provider,
					Code:      "prepared_state_missing",
					Message:   "provider preparation completed without durable state",
					Retryable: false,
				},
				false,
			)
		}
		if err := w.persistChannelOutboxProviderState(
			orgID,
			job.ID,
			workerID,
			providerState,
		); err != nil {
			return err
		}
		job.ProviderState = providerState
	}

	if isChannelAIReplyOutbox(job, outbound) {
		fencedAccount, err := w.recheckChannelAIOutboxDispatchWithAccount(
			orgID,
			job.ID,
			workerID,
			account,
		)
		if err != nil {
			return w.cancelChannelAIOutboxJob(orgID, job, workerID, err)
		}
		account = fencedAccount
	} else {
		fencedAccount, err := w.markChannelOutboxDispatching(
			orgID,
			job.ID,
			account.ID,
			workerID,
		)
		if err != nil {
			return err
		}
		account = fencedAccount
	}
	job.Status = models.OutboxJobStatusDispatching

	result, publishErr := sender.PublishPrepared(
		ctx,
		account,
		outbound,
		job.ProviderState,
	)
	if publishErr != nil {
		// Publication is the externally visible step. Any transport error can
		// mean that the provider committed even though its response was lost,
		// so retrying here would risk creating a duplicate Threads reply.
		return w.failChannelOutboxProviderAttempt(
			orgID,
			job,
			account,
			workerID,
			publishErr,
			false,
		)
	}
	if len(result.ProviderMessageIDs) == 0 {
		return w.failChannelOutboxProviderAttempt(
			orgID,
			job,
			account,
			workerID,
			errors.New("provider published the prepared request without a message ID"),
			false,
		)
	}
	return w.completeChannelOutboxJob(orgID, job, account, workerID, result)
}

func (w *Worker) persistChannelOutboxProviderState(
	orgID, jobID uuid.UUID,
	workerID string,
	providerState models.JSONB,
) error {
	if len(providerState) == 0 {
		return errors.New("channel outbox provider state is required")
	}
	return database.WithTenant(w.DB, orgID, func(tx *gorm.DB) error {
		now := time.Now().UTC()
		result := tx.Model(&models.OutboxJob{}).
			Where(
				"id = ? AND organization_id = ? AND status = ? AND locked_by = ?",
				jobID,
				orgID,
				models.OutboxJobStatusProcessing,
				workerID,
			).
			Updates(map[string]any{
				"provider_state": providerState,
				"locked_at":      now,
				"updated_at":     now,
			})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return errors.New("channel outbox preparation persistence lost its lease")
		}
		return nil
	})
}

func (w *Worker) markChannelOutboxDispatching(
	orgID, jobID, accountID uuid.UUID,
	workerID string,
) (*models.ChannelAccount, error) {
	var fencedAccount models.ChannelAccount
	err := database.WithTenantReadCommitted(w.DB, orgID, func(tx *gorm.DB) error {
		now := time.Now().UTC()
		// Disconnect, OAuth rotation, refresh finalization, and dispatch all
		// serialize on the account row before touching credentials or the job.
		// Whichever transaction obtains this lock first owns the delivery fence.
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ? AND organization_id = ?", accountID, orgID).
			First(&fencedAccount).Error; err != nil {
			return err
		}
		if fencedAccount.Status != models.ChannelAccountStatusActive ||
			!channelOutboxBool(fencedAccount.Config, "outbound_enabled") {
			return errors.New("channel account is no longer active for outbound delivery")
		}

		if fencedAccount.Channel == models.ChannelThreads {
			if !strings.EqualFold(fencedAccount.Provider, channelapi.ThreadsProvider) {
				return errChannelOutboxThreadsBinding
			}
			entitled, err := channelapi.HasDurableOmnichannelEntitlement(tx, orgID, now)
			if err != nil {
				return fmt.Errorf("evaluate omnichannel entitlement at dispatch fence: %w", err)
			}
			if !entitled {
				return errChannelOutboxUnlicensed
			}
			threadsEntitled, err := hasDurableChannelOutboxEntitlement(
				tx,
				orgID,
				channelapi.ThreadsPublicEngagementEntitlementKey,
				now,
			)
			if err != nil {
				return fmt.Errorf("evaluate Threads entitlement at dispatch fence: %w", err)
			}
			if !threadsEntitled {
				return errChannelOutboxThreadsUnlicensed
			}
			if err := tx.Where(
				"organization_id = ? AND channel_account_id = ? AND status IN ? AND (expires_at IS NULL OR expires_at > ?)",
				orgID,
				fencedAccount.ID,
				[]models.ChannelCredentialStatus{
					models.ChannelCredentialStatusActive,
					models.ChannelCredentialStatusExpiring,
				},
				now,
			).
				Order("version DESC").
				Order("id ASC").
				Find(&fencedAccount.Credentials).Error; err != nil {
				return err
			}
			if len(fencedAccount.Credentials) != 1 ||
				fencedAccount.Credentials[0].Kind != models.ChannelCredentialKindOAuth {
				return errChannelOutboxThreadsBinding
			}
			if err := validateThreadsChannelOutboxBinding(w.Config, tx, orgID, &fencedAccount); err != nil {
				return err
			}
		}

		result := tx.Model(&models.OutboxJob{}).
			Where(
				"id = ? AND organization_id = ? AND channel_account_id = ? AND status = ? AND locked_by = ?",
				jobID,
				orgID,
				fencedAccount.ID,
				models.OutboxJobStatusProcessing,
				workerID,
			).
			Where("provider_state IS NOT NULL AND provider_state <> '{}'::jsonb").
			Updates(map[string]any{
				"status":     models.OutboxJobStatusDispatching,
				"locked_at":  now,
				"updated_at": now,
			})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return errors.New("channel outbox dispatch fence lost its lease")
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &fencedAccount, nil
}

func isChannelAIReplyOutbox(
	job *models.OutboxJob,
	outbound channelapi.OutboundMessage,
) bool {
	if job == nil ||
		!strings.HasPrefix(job.IdempotencyKey, models.ChannelAIReplyKeyPrefix) {
		return false
	}
	aiGenerated, _ := outbound.Metadata["ai_generated"].(bool)
	senderRole, _ := outbound.Metadata["sender_role"].(string)
	return aiGenerated &&
		senderRole == string(models.ConversationParticipantRoleBot)
}

// recheckChannelAIOutboxDispatch closes the gap between Qwen finalization and
// provider dispatch and then atomically crosses a delivery fence. Policy
// transactions may cancel pending/retrying/processing jobs. Once this compare-
// and-swap commits dispatching, the provider attempt has won that race and
// later policy changes apply to subsequent messages.
func (w *Worker) recheckChannelAIOutboxDispatch(
	orgID, jobID uuid.UUID,
	workerID string,
	expectedAccounts ...*models.ChannelAccount,
) error {
	_, err := w.recheckChannelAIOutboxDispatchWithAccount(
		orgID,
		jobID,
		workerID,
		expectedAccounts...,
	)
	return err
}

func (w *Worker) recheckChannelAIOutboxDispatchWithAccount(
	orgID, jobID uuid.UUID,
	workerID string,
	expectedAccounts ...*models.ChannelAccount,
) (*models.ChannelAccount, error) {
	var expectedAccount *models.ChannelAccount
	if len(expectedAccounts) > 0 {
		expectedAccount = expectedAccounts[0]
	}
	expectedManaged := isManagedMetaChannelOutboxAccount(expectedAccount)
	var expectedGeneration managedMetaCredentialGeneration
	if expectedManaged {
		var ok bool
		expectedAt := time.Now().UTC()
		expectedGeneration, ok = managedMetaCurrentCredentialGeneration(
			expectedAccount.Credentials,
			expectedAt,
		)
		if !ok || (expectedAccount.Channel == models.ChannelInstagram &&
			!managedInstagramCredentialGenerationValid(
				expectedGeneration, orgID, expectedAccount.ID, expectedAt,
			)) {
			return nil, fmt.Errorf("%w: managed Meta credential generation is invalid", errChannelOutboxAIPolicy)
		}
	}
	var fencedAccount models.ChannelAccount
	err := database.WithTenantReadCommitted(w.DB, orgID, func(tx *gorm.DB) error {
		// Use the same organizations.id transaction mutex as the control plane
		// before taking the job/account locks. This makes a committed lifecycle
		// cancellation deterministically beat the dispatch CAS when it owns the
		// tenant mutex first.
		if err := lockChannelOutboxOrganizationScopeTx(tx, orgID); err != nil {
			return err
		}
		var job models.OutboxJob
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where(
			"id = ? AND organization_id = ? AND status = ? AND locked_by = ?",
			jobID,
			orgID,
			models.OutboxJobStatusProcessing,
			workerID,
		).First(&job).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return fmt.Errorf("%w: delivery was cancelled", errChannelOutboxAIPolicy)
			}
			return err
		}

		var account models.ChannelAccount
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where(
				"id = ? AND organization_id = ?",
				job.ChannelAccountID,
				orgID,
			).
			First(&account).Error; err != nil {
			return err
		}
		if account.Status != models.ChannelAccountStatusActive ||
			(account.Channel != models.ChannelInstagram &&
				account.Channel != models.ChannelMessenger) ||
			account.Provider != channelapi.RelayProvider ||
			!channelOutboxBool(account.Config, "outbound_enabled") ||
			!channelOutboxBool(account.Config, "ai_reply_enabled") {
			return fmt.Errorf("%w: account AI delivery is disabled", errChannelOutboxAIPolicy)
		}
		freshManaged := isManagedMetaChannelOutboxAccount(&account)
		if expectedManaged || freshManaged {
			if !freshManaged || (expectedAccount != nil && (!expectedManaged ||
				expectedAccount.ID != account.ID ||
				expectedAccount.Channel != account.Channel ||
				expectedAccount.ExternalAccountID != account.ExternalAccountID)) {
				return fmt.Errorf("%w: managed Meta account binding changed", errChannelOutboxAIPolicy)
			}
			now := time.Now().UTC()
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
				Where(
					"organization_id = ? AND channel_account_id = ? AND status IN ? AND (expires_at IS NULL OR expires_at > ?)",
					orgID,
					account.ID,
					[]models.ChannelCredentialStatus{
						models.ChannelCredentialStatusActive,
						models.ChannelCredentialStatusExpiring,
					},
					now,
				).
				Order("version DESC").
				Order("id ASC").
				Find(&account.Credentials).Error; err != nil {
				return err
			}
			currentGeneration, ok := managedMetaCurrentCredentialGeneration(
				account.Credentials,
				now,
			)
			if expectedAccount == nil {
				expectedGeneration = currentGeneration
			}
			if !ok || !sameManagedMetaCredentialGeneration(expectedGeneration, currentGeneration) ||
				!w.managedMetaChannelOutboxRuntimeAllowed(&account, currentGeneration, now) {
				return fmt.Errorf("%w: %v", errChannelOutboxAIPolicy, errChannelOutboxManagedMetaFence)
			}
		}

		var conversation models.InboxConversation
		if err := tx.Select(
			"id",
			"organization_id",
			"channel_account_id",
			"contact_id",
			"config",
			"service_window_ends_at",
		).Where(
			"id = ? AND organization_id = ? AND channel_account_id = ?",
			job.ConversationID,
			orgID,
			account.ID,
		).First(&conversation).Error; err != nil {
			return err
		}
		if channelAIReplyBoolValue(
			conversation.Config[models.ConversationConfigAIPaused],
		) {
			return fmt.Errorf("%w: conversation AI is paused", errChannelOutboxAIPolicy)
		}

		// Consent withdrawal and human handover creation acquire this same
		// contact-row lock. Whichever transaction commits first becomes the
		// policy decision for this single provider attempt.
		if err := database.LockContactPolicyScope(
			tx,
			orgID,
			conversation.ContactID,
		); err != nil {
			return fmt.Errorf("lock contact AI policy scope: %w", err)
		}

		var outbound channelapi.OutboundMessage
		if err := decodeChannelOutboxPayload(job.Payload, &outbound); err != nil {
			return fmt.Errorf("%w: invalid AI outbox payload", errChannelOutboxAIPolicy)
		}
		settingsID, err := uuid.Parse(strings.TrimSpace(
			fmt.Sprint(outbound.Metadata["ai_settings_id"]),
		))
		if err != nil || settingsID == uuid.Nil {
			return fmt.Errorf("%w: ai settings binding is invalid", errChannelOutboxAIPolicy)
		}
		var settings models.ChatbotSettings
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Select(
				"id",
				"organization_id",
				"is_enabled",
				"ai_enabled",
				"ai_provider",
			).
			Where(
				"id = ? AND organization_id = ?",
				settingsID,
				orgID,
			).
			First(&settings).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return fmt.Errorf("%w: ai settings binding is unavailable", errChannelOutboxAIPolicy)
			}
			return err
		}
		if err := channelapi.ApplyCentralQwenSettings(
			tx,
			orgID,
			&settings.AI,
		); err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return fmt.Errorf("%w: qwen settings are disabled", errChannelOutboxAIPolicy)
			}
			return fmt.Errorf("resolve central qwen settings at dispatch fence: %w", err)
		}
		if !settings.IsEnabled ||
			!settings.AI.Enabled ||
			settings.AI.Provider != models.AIProviderQwen ||
			strings.TrimSpace(settings.AI.APIKey) == "" {
			return fmt.Errorf("%w: qwen settings are disabled", errChannelOutboxAIPolicy)
		}
		now := time.Now().UTC()
		consentAllowed, consentReason, consentErr := channelapi.OutboundConsentAllowed(
			tx,
			orgID,
			conversation.ContactID,
			account.ID,
			account.Channel,
			outbound.Purpose,
			now,
		)
		if consentErr != nil {
			return fmt.Errorf("re-evaluate outbound consent at dispatch fence: %w", consentErr)
		}
		if !consentAllowed {
			return fmt.Errorf(
				"%w: outbound consent changed: %s",
				errChannelOutboxAIPolicy,
				consentReason,
			)
		}

		var handoverCount int64
		if err := tx.Model(&models.AgentTransfer{}).
			Where(
				"organization_id = ? AND contact_id = ? AND status = ?",
				orgID,
				conversation.ContactID,
				models.TransferStatusActive,
			).
			Count(&handoverCount).Error; err != nil {
			return err
		}
		if handoverCount > 0 {
			return fmt.Errorf("%w: human handover is active", errChannelOutboxAIPolicy)
		}

		if outbound.ServiceWindowEndsAt == nil ||
			!outbound.ServiceWindowEndsAt.After(now) ||
			conversation.ServiceWindowEndsAt == nil ||
			!conversation.ServiceWindowEndsAt.After(now) {
			return fmt.Errorf("%w: Meta service window expired", errChannelOutboxAIPolicy)
		}
		inboundID, err := uuid.Parse(strings.TrimSpace(
			fmt.Sprint(outbound.Metadata["inbound_message_id"]),
		))
		if err != nil || inboundID == uuid.Nil {
			return fmt.Errorf("%w: inbound binding is invalid", errChannelOutboxAIPolicy)
		}
		var inbound models.Message
		if err := tx.Select("id", "created_at").
			Where(
				"id = ? AND organization_id = ? AND inbox_conversation_id = ? AND direction = ?",
				inboundID,
				orgID,
				conversation.ID,
				models.DirectionIncoming,
			).
			First(&inbound).Error; err != nil {
			return fmt.Errorf("%w: inbound binding is unavailable", errChannelOutboxAIPolicy)
		}
		var newerHumanReplyCount int64
		if err := tx.Model(&models.Message{}).
			Where(
				"organization_id = ? AND inbox_conversation_id = ? AND direction = ? AND sent_by_user_id IS NOT NULL AND created_at > ?",
				orgID,
				conversation.ID,
				models.DirectionOutgoing,
				inbound.CreatedAt,
			).
			Count(&newerHumanReplyCount).Error; err != nil {
			return err
		}
		if newerHumanReplyCount > 0 {
			return fmt.Errorf("%w: a newer human reply exists", errChannelOutboxAIPolicy)
		}
		fencedAt := time.Now().UTC()
		result := tx.Model(&models.OutboxJob{}).
			Where(
				"id = ? AND organization_id = ? AND status = ? AND locked_by = ?",
				job.ID,
				orgID,
				channelOutboxLeaseStatus(&job),
				workerID,
			).
			Where(
				"? > clock_timestamp()",
				outbound.ServiceWindowEndsAt.UTC(),
			).
			Where(`
				EXISTS (
					SELECT 1
					FROM channel_accounts AS ca
					WHERE ca.id = outbox_jobs.channel_account_id
					  AND ca.organization_id = outbox_jobs.organization_id
					  AND ca.deleted_at IS NULL
					  AND ca.status = ?
					  AND ca.channel IN (?, ?)
					  AND ca.provider = ?
					  AND LOWER(COALESCE(ca.config->>'outbound_enabled', 'false')) IN ('true', '1', 'yes')
					  AND LOWER(COALESCE(ca.config->>'ai_reply_enabled', 'false')) IN ('true', '1', 'yes')
				)
				AND EXISTS (
					SELECT 1
					FROM inbox_conversations AS ic
					WHERE ic.id = outbox_jobs.conversation_id
					  AND ic.organization_id = outbox_jobs.organization_id
					  AND ic.channel_account_id = outbox_jobs.channel_account_id
					  AND ic.deleted_at IS NULL
					  AND LOWER(COALESCE(ic.config->>?, 'false')) NOT IN ('true', '1', 'yes')
					  AND ic.service_window_ends_at > clock_timestamp()
				)
				AND NOT EXISTS (
					SELECT 1
					FROM agent_transfers AS at
					WHERE at.organization_id = outbox_jobs.organization_id
					  AND at.contact_id = ?
					  AND at.status = ?
					  AND at.deleted_at IS NULL
				)
				AND EXISTS (
					SELECT 1
					FROM messages AS inbound
					WHERE inbound.id = ?
					  AND inbound.organization_id = outbox_jobs.organization_id
					  AND inbound.inbox_conversation_id = outbox_jobs.conversation_id
					  AND inbound.direction = ?
					  AND inbound.deleted_at IS NULL
				)
				AND NOT EXISTS (
					SELECT 1
					FROM messages AS human_reply
					WHERE human_reply.organization_id = outbox_jobs.organization_id
					  AND human_reply.inbox_conversation_id = outbox_jobs.conversation_id
					  AND human_reply.direction = ?
					  AND human_reply.sent_by_user_id IS NOT NULL
					  AND human_reply.created_at > ?
					  AND human_reply.deleted_at IS NULL
				)
			`,
				models.ChannelAccountStatusActive,
				models.ChannelInstagram,
				models.ChannelMessenger,
				channelapi.RelayProvider,
				models.ConversationConfigAIPaused,
				conversation.ContactID,
				models.TransferStatusActive,
				inbound.ID,
				models.DirectionIncoming,
				models.DirectionOutgoing,
				inbound.CreatedAt,
			).
			Updates(map[string]any{
				"status":     models.OutboxJobStatusDispatching,
				"locked_at":  fencedAt,
				"updated_at": fencedAt,
			})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return fmt.Errorf(
				"%w: delivery fence lost to a committed policy change",
				errChannelOutboxAIPolicy,
			)
		}
		if freshManaged || expectedAccount == nil {
			fencedAccount = account
		} else {
			// Legacy/static AI delivery keeps the fully preloaded credential
			// snapshot; only managed Meta needs the freshly locked pair above.
			fencedAccount = *expectedAccount
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &fencedAccount, nil
}

func (w *Worker) cancelChannelAIOutboxJob(
	orgID uuid.UUID,
	job *models.OutboxJob,
	workerID string,
	policyErr error,
) error {
	if job == nil || job.ID == uuid.Nil {
		return policyErr
	}
	reason := channelOutboxErrorMessage(policyErr)
	return database.WithTenant(w.DB, orgID, func(tx *gorm.DB) error {
		now := time.Now().UTC()
		result := tx.Model(&models.OutboxJob{}).
			Where(
				"id = ? AND organization_id = ? AND status = ? AND locked_by = ?",
				job.ID,
				orgID,
				channelOutboxLeaseStatus(job),
				workerID,
			).
			Updates(map[string]any{
				"status":          models.OutboxJobStatusCancelled,
				"failed_at":       now,
				"last_attempt_at": now,
				"last_error_code": "ai_reply_cancelled",
				"last_error":      reason,
				"locked_at":       nil,
				"locked_by":       "",
				"updated_at":      now,
			})
		if result.Error != nil {
			return result.Error
		}
		// A control-plane transaction may already have cancelled the row and
		// cleared its lease. That is the desired terminal state.
		if result.RowsAffected == 0 {
			return nil
		}
		if job.MessageID == nil {
			return nil
		}
		return tx.Model(&models.Message{}).
			Where(
				"id = ? AND organization_id = ? AND status = ?",
				*job.MessageID,
				orgID,
				models.MessageStatusPending,
			).
			Updates(map[string]any{
				"status":        models.MessageStatusFailed,
				"error_message": "Automatic AI reply cancelled before delivery",
				"updated_at":    now,
			}).Error
	})
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
				channelOutboxLeaseStatus(job),
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

// failChannelOutboxProviderAttempt terminally settles every managed Instagram
// error after the dispatching CAS. A transport error or an accepted response
// without a message ID is ambiguous: the provider may already have delivered
// generation N. Retrying under N (or a later reconnect generation N+1) would
// risk a duplicate or an authorization replay. Static Instagram and all
// Messenger jobs retain the ordinary retry policy.
func (w *Worker) failChannelOutboxProviderAttempt(
	orgID uuid.UUID,
	job *models.OutboxJob,
	account *models.ChannelAccount,
	workerID string,
	deliveryErr error,
	retryable bool,
) error {
	managedInstagramDispatch := job != nil && account != nil &&
		job.Status == models.OutboxJobStatusDispatching &&
		account.Channel == models.ChannelInstagram &&
		isManagedMetaChannelOutboxAccount(account)
	if !managedInstagramDispatch {
		return w.failChannelOutboxJob(
			orgID,
			job,
			workerID,
			deliveryErr,
			retryable,
		)
	}

	ambiguousErr := &channelapi.ProviderError{
		Operation: "send",
		Provider:  account.Provider,
		Code:      "managed_instagram_delivery_state_ambiguous",
		Message: "Managed Instagram provider delivery state is ambiguous; automatic retry is disabled: " +
			channelOutboxErrorMessage(deliveryErr),
		Retryable: false,
		Cause:     deliveryErr,
	}
	attempt := job.AttemptCount + 1
	persistErr := database.WithTenantReadCommitted(w.DB, orgID, func(tx *gorm.DB) error {
		// Serialize the terminal provider outcome with reconnect/rotation. The
		// winner cannot leave a retryable row for a later credential generation.
		if err := lockChannelOutboxOrganizationScopeTx(tx, orgID); err != nil {
			return err
		}
		now := time.Now().UTC()
		errorMessage := channelOutboxErrorMessage(ambiguousErr)
		result := tx.Model(&models.OutboxJob{}).
			Where(
				"id = ? AND organization_id = ? AND channel_account_id = ? AND status = ? AND locked_by = ?",
				job.ID,
				orgID,
				account.ID,
				models.OutboxJobStatusDispatching,
				workerID,
			).
			Updates(map[string]any{
				"status":          models.OutboxJobStatusFailed,
				"attempt_count":   attempt,
				"last_attempt_at": now,
				"failed_at":       now,
				"last_error_code": ambiguousErr.Code,
				"last_error":      errorMessage,
				"locked_at":       nil,
				"locked_by":       "",
				"updated_at":      now,
			})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			var current models.OutboxJob
			if err := tx.Select("status").
				Where("id = ? AND organization_id = ?", job.ID, orgID).
				First(&current).Error; err != nil {
				return err
			}
			if current.Status == models.OutboxJobStatusFailed ||
				current.Status == models.OutboxJobStatusCancelled {
				return nil
			}
			return errors.New("managed Instagram provider failure settlement lost its lease")
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
			ErrorCode:        ambiguousErr.Code,
			ErrorMessage:     errorMessage,
			Payload:          models.JSONB{"provider_state_ambiguous": true},
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
		return errors.Join(ambiguousErr, persistErr)
	}
	return nil
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
				channelOutboxLeaseStatus(job),
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
	if w != nil && w.channelAdapterFactory != nil {
		return w.channelAdapterFactory(account)
	}
	if account == nil || account.Channel == models.ChannelTikTok {
		return nil, &channelapi.ProviderError{
			Operation: "resolve_adapter",
			Code:      "approval_required",
			Message:   "channel provider is approval-gated",
			Retryable: false,
		}
	}
	isThreads := account.Channel == models.ChannelThreads &&
		strings.EqualFold(account.Provider, channelapi.ThreadsProvider)
	isRelay := account.Channel != models.ChannelThreads &&
		strings.EqualFold(account.Provider, channelapi.RelayProvider)
	if !isThreads && !isRelay {
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
	client := &http.Client{Timeout: 30 * time.Second}
	if isThreads {
		return channelapi.NewThreadsAdapter(client, encryptionKey), nil
	}
	return channelapi.NewRelayAdapter(
		account.Channel,
		client,
		encryptionKey,
	).WithServiceToken(w.Config.MetaRegistry.RelayEdgeSecret), nil
}

// hasDurableChannelOutboxEntitlement mirrors the durable commercial check used
// by the request path, but accepts the product key needed by provider-specific
// outbox jobs. The subscription must itself be current; an override cannot
// reopen an expired subscription.
func hasDurableChannelOutboxEntitlement(
	db *gorm.DB,
	organizationID uuid.UUID,
	key string,
	now time.Time,
) (bool, error) {
	if db == nil {
		return false, errors.New("database is required")
	}
	if organizationID == uuid.Nil {
		return false, errors.New("organization ID is required")
	}
	if strings.TrimSpace(key) == "" {
		return false, errors.New("entitlement key is required")
	}
	now = now.UTC()

	var subscription models.Subscription
	err := db.
		Where("organization_id = ?", organizationID).
		Order("created_at DESC").
		First(&subscription).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !channelOutboxSubscriptionPermitsEntitlement(&subscription, now) {
		return false, nil
	}

	value, exists := subscription.EntitlementsSnapshot[key]
	allowed := exists && channelOutboxEntitlementAllows(value)

	var override models.EntitlementOverride
	err = db.
		Where(
			"organization_id = ? AND key = ? AND is_active = ? AND starts_at <= ? AND (expires_at IS NULL OR expires_at > ?)",
			organizationID,
			key,
			true,
			now,
			now,
		).
		Order("starts_at DESC, created_at DESC").
		First(&override).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return allowed, nil
	}
	if err != nil {
		return false, err
	}
	return channelOutboxEntitlementAllows(
		channelOutboxEntitlementOverrideValue(override.Value),
	), nil
}

func channelOutboxSubscriptionPermitsEntitlement(
	subscription *models.Subscription,
	now time.Time,
) bool {
	if subscription == nil {
		return false
	}
	switch subscription.Status {
	case models.SubscriptionStatusActive:
		return (subscription.CurrentPeriodEnd != nil && subscription.CurrentPeriodEnd.After(now)) ||
			(subscription.GraceUntil != nil && subscription.GraceUntil.After(now))
	case models.SubscriptionStatusTrialing:
		return subscription.TrialEndsAt != nil && subscription.TrialEndsAt.After(now)
	case models.SubscriptionStatusPastDue:
		return subscription.GraceUntil != nil && subscription.GraceUntil.After(now)
	default:
		return false
	}
}

func channelOutboxEntitlementOverrideValue(value models.JSONB) any {
	if len(value) == 1 {
		if scalar, ok := value["value"]; ok {
			return scalar
		}
	}
	return value
}

func channelOutboxEntitlementAllows(value any) bool {
	switch typed := value.(type) {
	case bool:
		return typed
	case int:
		return typed > 0
	case int32:
		return typed > 0
	case int64:
		return typed > 0
	case float32:
		return !math.IsNaN(float64(typed)) && typed > 0
	case float64:
		return !math.IsNaN(typed) && typed > 0
	case json.Number:
		number, err := typed.Float64()
		return err == nil && !math.IsNaN(number) && number > 0
	case string:
		normalized := strings.ToLower(strings.TrimSpace(typed))
		return normalized != "" &&
			normalized != "0" &&
			normalized != "false" &&
			normalized != "disabled" &&
			normalized != "none"
	case models.JSONB:
		if nested, ok := typed["value"]; ok {
			return channelOutboxEntitlementAllows(nested)
		}
		if enabled, ok := typed["enabled"]; ok {
			return channelOutboxEntitlementAllows(enabled)
		}
		return len(typed) > 0
	case map[string]any:
		return channelOutboxEntitlementAllows(models.JSONB(typed))
	default:
		return value != nil
	}
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

func channelOutboxLeaseStatus(job *models.OutboxJob) models.OutboxJobStatus {
	if job != nil && job.Status == models.OutboxJobStatusDispatching {
		return models.OutboxJobStatusDispatching
	}
	return models.OutboxJobStatusProcessing
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
