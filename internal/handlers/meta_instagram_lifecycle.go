package handlers

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/audit"
	channelapi "github.com/shridarpatil/whatomate/internal/channel"
	configpkg "github.com/shridarpatil/whatomate/internal/config"
	appcrypto "github.com/shridarpatil/whatomate/internal/crypto"
	"github.com/shridarpatil/whatomate/internal/database"
	"github.com/shridarpatil/whatomate/internal/metaregistry"
	"github.com/shridarpatil/whatomate/internal/models"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const metaInstagramRevalidationLease = 2 * time.Minute

var errMetaInstagramProviderAttemptFenced = errors.New("managed Instagram provider attempt was fenced")

type metaInstagramRevalidationSnapshot struct {
	OrganizationID uuid.UUID
	Account        models.ChannelAccount
	OAuth          models.ChannelCredential
	Webhook        models.ChannelCredential
	AccessToken    string
	PreviousCheck  time.Time
}

// RunMetaInstagramLifecycle revalidates the exact app/profile/subscription
// binding and refreshes long-lived credentials without changing their queue
// generation fence. Release evidence and the one-workspace gate are checked
// before every provider request.
func (a *App) RunMetaInstagramLifecycle(ctx context.Context) error {
	if a == nil || a.Config == nil || !a.Config.MetaInstagram.Enabled || !a.Config.MetaRegistry.Enabled {
		return nil
	}
	if a.Redis == nil {
		return errors.New("meta Instagram lifecycle requires Redis")
	}
	interval := time.Duration(a.Config.MetaInstagram.SchedulerIntervalSeconds) * time.Second
	if interval < 10*time.Second || interval > 5*time.Minute {
		return errors.New("meta Instagram scheduler interval is outside safe bounds")
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if err := a.revalidateDueMetaInstagramBindings(ctx); err != nil && !errors.Is(err, context.Canceled) {
			a.Log.Warn("Instagram lifecycle sweep failed")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (a *App) revalidateDueMetaInstagramBindings(ctx context.Context) error {
	return a.revalidateDueMetaInstagramBindingsWithJournalCleanup(
		ctx,
		a.cleanupMetaInstagramLifecycleJournals,
	)
}

func (a *App) revalidateDueMetaInstagramBindingsWithJournalCleanup(
	ctx context.Context,
	cleanup func(time.Time) error,
) error {
	if cleanup == nil {
		return errors.New("managed Instagram journal cleanup is unavailable")
	}
	now := time.Now().UTC()
	if a != nil && a.Config != nil && a.Config.MetaInstagram.QuarantineOnly {
		// The emergency downgrade is a safety barrier, not maintenance. Always
		// settle the tenant rows and their cancellable work before journal cleanup
		// can fail this sweep, and never enter a provider-capable path.
		if err := a.reconcileMetaInstagramQuarantineBarrier(ctx, now); err != nil {
			return err
		}
		return cleanup(now)
	}
	after := uuid.Nil
	for {
		organizations, err := a.metaInstagramLifecycleOrganizations(after)
		if err != nil {
			return err
		}
		for _, organizationID := range organizations {
			if err := a.revalidateMetaInstagramOrganization(ctx, organizationID, now); err != nil &&
				!errors.Is(err, context.Canceled) {
				a.Log.Warn("Instagram tenant lifecycle sweep failed", "organization_id", organizationID)
			}
			after = organizationID
		}
		if len(organizations) < 100 {
			break
		}
	}
	return cleanup(now)
}

func (a *App) cleanupMetaInstagramLifecycleJournals(now time.Time) error {
	if err := a.cleanupMetaInstagramDeletionEvents(now); err != nil {
		return err
	}
	// Instagram may be the only managed Meta product enabled in a release, so
	// its sweep also maintains the shared deauthorization journal bound.
	return a.cleanupMetaDeauthorizationEvents(now)
}

// ReconcileMetaInstagramQuarantineStartup is the synchronous, database-only
// kill-switch barrier used by both the API and worker commands before either
// starts serving or claiming work. Configuration validation normally proves
// these inputs, but the barrier validates them again because failure must stop
// startup rather than silently leaving a managed row routable.
func ReconcileMetaInstagramQuarantineStartup(
	ctx context.Context,
	db *gorm.DB,
	cfg *configpkg.Config,
) error {
	if cfg == nil {
		return errors.New("managed Instagram startup barrier requires configuration")
	}
	if !cfg.MetaInstagram.Enabled || !cfg.MetaInstagram.QuarantineOnly {
		return nil
	}
	if db == nil || !cfg.MetaRegistry.Enabled {
		return errors.New("managed Instagram startup barrier is unavailable")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	app := &App{Config: cfg, DB: db.WithContext(ctx)}
	return app.ReconcileMetaInstagramQuarantineStartup(ctx)
}

// ReconcileMetaInstagramQuarantineStartup is also exposed on App so tests and
// alternate process entry points can prove that an installed provider client
// is never touched by this database-only barrier.
func (a *App) ReconcileMetaInstagramQuarantineStartup(ctx context.Context) error {
	if a == nil || a.Config == nil {
		return errors.New("managed Instagram startup barrier requires configuration")
	}
	if !a.Config.MetaInstagram.Enabled || !a.Config.MetaInstagram.QuarantineOnly {
		return nil
	}
	if a.rootApp() == nil || a.rootApp().DB == nil || !a.Config.MetaRegistry.Enabled {
		return errors.New("managed Instagram startup barrier is unavailable")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return a.reconcileMetaInstagramQuarantineBarrier(ctx, time.Now().UTC())
}

func (a *App) reconcileMetaInstagramQuarantineBarrier(
	ctx context.Context,
	checkedAt time.Time,
) error {
	if a == nil || a.Config == nil || a.rootApp() == nil || a.rootApp().DB == nil {
		return errors.New("managed Instagram quarantine barrier is unavailable")
	}
	if !a.Config.MetaInstagram.Enabled || !a.Config.MetaInstagram.QuarantineOnly {
		return nil
	}
	organizationText := strings.TrimSpace(a.Config.MetaInstagram.AllowedOrganizationID)
	organizationID, err := uuid.Parse(organizationText)
	if err != nil || organizationID == uuid.Nil || organizationID.String() != organizationText {
		return errors.New("managed Instagram quarantine barrier tenant is invalid")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	checkedAt = checkedAt.UTC()
	return a.WithCommittedTenantApp(organizationID, func(scoped *App) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := lockChannelAIOrganizationScopeTx(scoped.DB, organizationID); err != nil {
			return err
		}

		var accounts []models.ChannelAccount
		if err := scoped.DB.WithContext(ctx).
			Clauses(clause.Locking{Strength: "UPDATE"}).
			Where(
				"organization_id = ? AND channel = ? AND provider = ? AND status IN ?",
				organizationID,
				models.ChannelInstagram,
				channelapi.RelayProvider,
				[]models.ChannelAccountStatus{
					models.ChannelAccountStatusPending,
					models.ChannelAccountStatusActive,
					models.ChannelAccountStatusDegraded,
				},
			).
			Order("id").
			Find(&accounts).Error; err != nil {
			return err
		}
		for index := range accounts {
			if !managedInstagramControlPlaneIntent(&accounts[index]) {
				continue
			}
			if err := scoped.quarantineMetaInstagramAccountTx(
				&accounts[index],
				"managed_release_evidence_invalid",
				checkedAt,
			); err != nil {
				return err
			}
		}
		return nil
	})
}

func (a *App) metaInstagramLifecycleOrganizations(after uuid.UUID) ([]uuid.UUID, error) {
	root := a.rootApp()
	if root == nil || root.Config == nil {
		return nil, nil
	}
	allowedOrganizationID, err := uuid.Parse(strings.TrimSpace(
		root.Config.MetaInstagram.AllowedOrganizationID,
	))
	if err != nil || allowedOrganizationID == uuid.Nil {
		return nil, nil
	}
	if after != uuid.Nil && allowedOrganizationID.String() <= after.String() {
		return nil, nil
	}
	return []uuid.UUID{allowedOrganizationID}, nil
}

func (a *App) revalidateMetaInstagramOrganization(
	ctx context.Context,
	organizationID uuid.UUID,
	now time.Time,
) error {
	if !a.metaInstagramOrganizationAllowed(organizationID) {
		return nil
	}
	after := uuid.Nil
	for {
		var accountIDs []uuid.UUID
		if err := database.WithTenantReadCommitted(a.rootApp().DB, organizationID, func(tx *gorm.DB) error {
			return tx.Model(&models.ChannelAccount{}).Where(
				"organization_id = ? AND id > ? AND channel = ? AND provider = ? AND status IN ?",
				organizationID, after, models.ChannelInstagram, channelapi.RelayProvider,
				[]models.ChannelAccountStatus{
					models.ChannelAccountStatusPending,
					models.ChannelAccountStatusActive,
					models.ChannelAccountStatusDegraded,
				},
			).Where(`(
				config ->> 'meta_management_mode' = ?
				OR config ->> 'meta_registry_managed' = 'true'
				OR (
					metadata ->> 'meta_platform_app_id' = ?
					AND (
						metadata ->> 'meta_webhook_app' = 'instagram_login'
						OR COALESCE(metadata ->> 'meta_subscription_operation_id', '') <> ''
					)
				)
			)`, metaregistry.ManagementModePlatformOAuth, strings.TrimSpace(a.Config.MetaInstagram.AppID)).
				Order("id").Limit(100).Pluck("id", &accountIDs).Error
		}); err != nil {
			return err
		}
		for _, accountID := range accountIDs {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			leaseKey := "meta-instagram:revalidate:" + accountID.String()
			claimed, err := a.Redis.SetNX(ctx, leaseKey, uuid.NewString(), metaInstagramRevalidationLease).Result()
			if err != nil {
				return err
			}
			if claimed {
				a.revalidateOneMetaInstagramBinding(ctx, organizationID, accountID, now)
			}
			after = accountID
		}
		if len(accountIDs) < 100 {
			return nil
		}
	}
}

func (a *App) revalidateOneMetaInstagramBinding(
	ctx context.Context,
	organizationID, accountID uuid.UUID,
	now time.Time,
) {
	account, err := a.loadMetaInstagramLifecycleAccount(organizationID, accountID)
	if err != nil {
		return
	}
	if reason := a.metaInstagramLifecycleReleaseGuardReason(account, organizationID); reason != "" {
		if err := a.quarantineMetaInstagramAccount(organizationID, accountID, reason, now); err != nil {
			a.Log.Warn("Instagram release quarantine failed", "channel_account_id", accountID)
		}
		return
	}
	operation, subscriptionReady := metaInstagramSubscribedOperation(account.Metadata)
	if !subscriptionReady {
		_, hasOperation := metaMessengerSubscriptionOperationFromMetadata(account.Metadata)
		if hasOperation && (operation.State == metaMessengerSubscriptionSubscribePending ||
			operation.State == metaMessengerSubscriptionSubscribeFailed ||
			operation.State == metaMessengerSubscriptionUnsubscribePending ||
			operation.State == metaMessengerSubscriptionUnsubscribeFailed) {
			return
		}
		if err := a.quarantineMetaInstagramAccount(
			organizationID, accountID, "managed_subscription_fence_invalid", now,
		); err != nil {
			a.Log.Warn("Instagram subscription fence quarantine failed", "channel_account_id", accountID)
		}
		return
	}

	snapshot, err := a.loadMetaInstagramLifecycleRevalidationSnapshot(organizationID, accountID, now)
	if err != nil {
		if quarantineErr := a.quarantineMetaInstagramAccount(
			organizationID, accountID, "managed_credential_fence_invalid", now,
		); quarantineErr != nil {
			a.Log.Warn("Instagram credential quarantine failed", "channel_account_id", accountID)
		}
		return
	}
	maxAge := time.Duration(a.Config.MetaRegistry.OwnershipMaxAgeMins) * time.Minute
	lead := time.Duration(a.Config.MetaInstagram.RevalidationLeadMins) * time.Minute
	refreshLead := time.Duration(a.Config.MetaInstagram.TokenRefreshLeadHours) * time.Hour
	if snapshot.OAuth.ExpiresAt == nil {
		_ = a.quarantineMetaInstagramAccount(organizationID, accountID, "oauth_expiry_missing", now)
		return
	}
	if !snapshot.OAuth.ExpiresAt.After(now) {
		a.applyMetaInstagramLifecycleOutcome(snapshot, metaregistry.OwnershipRevoked, "oauth_credential_expired", now)
		return
	}
	if snapshot.PreviousCheck.After(now.Add(time.Minute)) {
		_ = a.quarantineMetaInstagramAccount(
			organizationID, accountID, "ownership_check_timestamp_in_future", now,
		)
		return
	}
	due := metaDeauthorizationReconciliationPending(snapshot.Account.Metadata) ||
		snapshot.PreviousCheck.IsZero() ||
		snapshot.PreviousCheck.Before(now.Add(-(maxAge - lead)))
	refreshDue := !snapshot.OAuth.ExpiresAt.After(now.Add(refreshLead))
	if !due && !refreshDue {
		return
	}

	providerCtx, cancel := context.WithTimeout(ctx, metaInstagramProviderOperationLimit)
	defer cancel()
	attemptSnapshot := snapshot
	accessToken := snapshot.AccessToken
	var refreshed *metaInstagramTokenResponse
	var inspection metaInstagramTokenInspection
	var expiresAt *time.Time
	err = a.withLockedMetaInstagramProviderAttempt(
		providerCtx, snapshot, true, false,
		func(locked metaInstagramRevalidationSnapshot) error {
			attemptSnapshot = locked
			settings, settingsErr := a.metaInstagramOnboardingSettings()
			if settingsErr != nil {
				return errMetaInstagramProviderAttemptFenced
			}
			accessToken = locked.AccessToken
			lockedRefreshDue := locked.OAuth.ExpiresAt != nil &&
				!locked.OAuth.ExpiresAt.After(time.Now().UTC().Add(refreshLead))
			if lockedRefreshDue {
				value, refreshErr := a.refreshMetaInstagramLongLivedToken(
					providerCtx, settings, accessToken,
				)
				if refreshErr != nil {
					return refreshErr
				}
				accessToken = value.AccessToken
				refreshed = &value
			}
			var inspectErr error
			inspection, inspectErr = a.inspectMetaInstagramToken(
				providerCtx, settings, accessToken,
			)
			if inspectErr != nil {
				return inspectErr
			}
			profile, profileErr := a.fetchMetaInstagramProfile(
				providerCtx, settings, accessToken,
			)
			if profileErr != nil {
				return profileErr
			}
			if inspection.AppID != strings.TrimSpace(a.Config.MetaInstagram.AppID) ||
				inspection.UserID != stringConfigValue(locked.Account.Metadata, "meta_authorizing_user_id") ||
				profile.oauthSubjectID() != inspection.UserID ||
				profile.professionalAccountID() != locked.Account.ExternalAccountID ||
				profile.professionalAccountID() !=
					stringConfigValue(locked.Account.Metadata, "meta_authority_asset_id") {
				return metaInstagramRevokedBinding("instagram_identity_binding_changed")
			}
			subscribed, subscriptionErr := a.metaInstagramHasConfiguredAppSubscription(
				providerCtx, settings, locked.Account.ExternalAccountID, accessToken,
			)
			if subscriptionErr != nil {
				return subscriptionErr
			}
			if !subscribed {
				return metaInstagramStaleBinding("messages_subscription_missing")
			}
			checkedAt := time.Now().UTC()
			if refreshed != nil {
				expiresAt, inspectErr = metaInstagramExpiry(
					checkedAt, refreshed.ExpiresIn, inspection.ExpiresAt,
				)
				if inspectErr != nil {
					return metaInstagramStaleBinding("refreshed_token_expiry_invalid")
				}
			} else {
				expiresAt = locked.OAuth.ExpiresAt
				if inspection.ExpiresAt != nil {
					expiresAt = inspection.ExpiresAt
				}
			}
			if expiresAt == nil ||
				!expiresAt.After(checkedAt.Add(metaInstagramProviderOperationLimit)) {
				return metaInstagramStaleBinding("inspected_token_expiry_invalid")
			}
			return nil
		},
	)
	if err != nil {
		if errors.Is(err, errMetaInstagramProviderAttemptFenced) {
			_ = a.quarantineMetaInstagramAccount(
				organizationID, accountID, "provider_attempt_binding_fenced", time.Now().UTC(),
			)
			return
		}
		a.handleMetaInstagramLifecycleError(attemptSnapshot, err, now)
		return
	}
	checkedAt := time.Now().UTC()
	if err := a.storeMetaInstagramRevalidation(
		attemptSnapshot, accessToken, refreshed != nil, expiresAt, inspection, checkedAt,
	); err != nil {
		a.Log.Warn("Instagram lifecycle mutation was superseded", "channel_account_id", accountID)
	}
}

func (a *App) loadMetaInstagramLifecycleAccount(
	organizationID, accountID uuid.UUID,
) (models.ChannelAccount, error) {
	var account models.ChannelAccount
	if !a.metaInstagramOrganizationAllowed(organizationID) {
		return account, gorm.ErrRecordNotFound
	}
	err := database.WithTenantReadCommitted(a.rootApp().DB, organizationID, func(tx *gorm.DB) error {
		if err := tx.Where(
			"id = ? AND organization_id = ? AND channel = ? AND provider = ?",
			accountID, organizationID, models.ChannelInstagram, channelapi.RelayProvider,
		).First(&account).Error; err != nil {
			return err
		}
		if !managedInstagramControlPlaneIntent(&account) {
			return gorm.ErrRecordNotFound
		}
		return nil
	})
	return account, err
}

func (a *App) metaInstagramReleaseGuardReason(
	account models.ChannelAccount,
	organizationID uuid.UUID,
) string {
	return a.metaInstagramReleaseGuardReasonWithDeauthorization(account, organizationID, false)
}

// metaInstagramLifecycleReleaseGuardReason is the sole internal bypass for an
// ambiguous deauthorization fence. It lets the lifecycle call Graph using the
// exact current credential generation so the fence can be settled. All lease,
// activation, subscription, and onboarding paths use the normal guard above.
func (a *App) metaInstagramLifecycleReleaseGuardReason(
	account models.ChannelAccount,
	organizationID uuid.UUID,
) string {
	return a.metaInstagramReleaseGuardReasonWithDeauthorization(account, organizationID, true)
}

func (a *App) metaInstagramManagedURLBindingReason(account *models.ChannelAccount) string {
	if a == nil || a.Config == nil || account == nil || account.ID == uuid.Nil ||
		!validCanonicalMetaID(account.ExternalAccountID) {
		return "managed_instagram_url_binding_invalid"
	}
	expectedWebhookURL, err := metaRegistryJoinURL(
		a.Config.MetaInstagram.ReReplyBaseURL,
		"api", "webhooks", "channels", account.ID.String(),
	)
	if err != nil {
		return "managed_instagram_webhook_url_invalid"
	}
	expectedRelayURL, err := metaRegistryJoinURL(
		a.Config.MetaInstagram.RelayBaseURL,
		"v1", "accounts", string(models.ChannelInstagram), account.ExternalAccountID,
	)
	if err != nil {
		return "managed_instagram_relay_url_invalid"
	}
	if stringConfigValue(account.Config, "rereply_webhook_url") != expectedWebhookURL {
		return "managed_instagram_webhook_url_mismatch"
	}
	if stringConfigValue(account.Config, "relay_url") != expectedRelayURL {
		return "managed_instagram_relay_url_mismatch"
	}
	if strings.TrimSpace(stringConfigValue(account.Config, "health_url")) != "" {
		return "managed_instagram_health_url_override_forbidden"
	}
	return ""
}

func (a *App) metaInstagramReleaseGuardReasonWithDeauthorization(
	account models.ChannelAccount,
	organizationID uuid.UUID,
	allowPendingDeauthorization bool,
) string {
	if !a.metaInstagramOrganizationAllowed(organizationID) || account.OrganizationID != organizationID {
		return "organization_removed_from_runtime_allowlist"
	}
	if stringConfigValue(account.Metadata, "meta_deauthorized_at") != "" {
		return "managed_instagram_authorization_revoked"
	}
	if metaInstagramDeletionReconciliationPending(account.Metadata) {
		return "data_deletion_reconciliation_required"
	}
	if !allowPendingDeauthorization && metaDeauthorizationReconciliationPending(account.Metadata) {
		return "deauthorization_reconciliation_required"
	}
	if !exactMetaRegistryControlPlaneConfig(account.Config) ||
		stringConfigValue(account.Config, "instagram_api_mode") != "instagram_login" {
		return "managed_instagram_binding_invalid"
	}
	if reason := a.metaInstagramManagedURLBindingReason(&account); reason != "" {
		return reason
	}
	if a.Config == nil || stringConfigValue(account.Metadata, "meta_platform_app_id") !=
		strings.TrimSpace(a.Config.MetaInstagram.AppID) {
		return "managed_platform_app_binding_changed"
	}
	authorizedAt, err := time.Parse(
		time.RFC3339Nano,
		stringConfigValue(account.Metadata, metaMessengerAuthorizationGrantedAtKey),
	)
	if err != nil || authorizedAt.IsZero() || authorizedAt.After(time.Now().UTC().Add(time.Minute)) {
		return "authorization_generation_fence_invalid"
	}
	if !metaAuthorizationTokenAllowed(a.Config, &account) {
		return "managed_release_evidence_invalid"
	}
	if validateMetaRegistryPlatformBinding(&account) != nil {
		return "managed_instagram_binding_invalid"
	}
	return ""
}

func (a *App) loadMetaInstagramRevalidationSnapshot(
	organizationID, accountID uuid.UUID,
	now time.Time,
) (metaInstagramRevalidationSnapshot, error) {
	return a.loadMetaInstagramRevalidationSnapshotWithDeauthorization(
		organizationID, accountID, now, false,
	)
}

func (a *App) loadMetaInstagramLifecycleRevalidationSnapshot(
	organizationID, accountID uuid.UUID,
	now time.Time,
) (metaInstagramRevalidationSnapshot, error) {
	return a.loadMetaInstagramRevalidationSnapshotWithDeauthorization(
		organizationID, accountID, now, true,
	)
}

func (a *App) loadMetaInstagramRevalidationSnapshotWithDeauthorization(
	organizationID, accountID uuid.UUID,
	now time.Time,
	allowPendingDeauthorization bool,
) (metaInstagramRevalidationSnapshot, error) {
	var snapshot metaInstagramRevalidationSnapshot
	snapshot.OrganizationID = organizationID
	err := database.WithTenantReadCommitted(a.rootApp().DB, organizationID, func(tx *gorm.DB) error {
		if err := tx.Preload("Credentials", func(db *gorm.DB) *gorm.DB { return db.Order("version DESC") }).
			Where("id = ? AND organization_id = ? AND channel = ? AND provider = ?",
				accountID, organizationID, models.ChannelInstagram, channelapi.RelayProvider).
			First(&snapshot.Account).Error; err != nil {
			return err
		}
		if a.metaInstagramReleaseGuardReasonWithDeauthorization(
			snapshot.Account, organizationID, allowPendingDeauthorization,
		) != "" {
			return metaregistry.ErrNotFound
		}
		oauth := currentMetaRegistryCredential(
			snapshot.Account.Credentials, models.ChannelCredentialKindOAuth, now,
		)
		webhook := currentMetaRegistryCredential(
			snapshot.Account.Credentials, models.ChannelCredentialKindWebhook, now,
		)
		if !metaInstagramCredentialPairGenerationValid(oauth, webhook, now) {
			return metaregistry.ErrNotFound
		}
		snapshot.OAuth, snapshot.Webhook = *oauth, *webhook
		if !metaInstagramSubscribedOperationMatchesCredentials(
			snapshot.Account.Metadata, snapshot.OAuth, snapshot.Webhook,
		) {
			return metaregistry.ErrNotFound
		}
		var err error
		snapshot.AccessToken, err = decryptRequiredMetaRegistrySecret(
			oauth.CredentialBlob, "access_token", a.Config.App.EncryptionKey,
		)
		if err != nil {
			return err
		}
		checkedAt := stringConfigValue(snapshot.Account.Metadata, "meta_ownership_checked_at")
		if checkedAt == "" {
			return nil
		}
		snapshot.PreviousCheck, err = time.Parse(time.RFC3339Nano, checkedAt)
		return err
	})
	return snapshot, err
}

// withLockedMetaInstagramProviderAttempt makes the organization row the point
// of serialization for lifecycle/approval Graph reads. A control-plane change
// that commits first is observed here and causes zero provider call. If this
// attempt owns the mutex first, the control-plane change waits and then wins at
// the later persistence boundary.
func (a *App) withLockedMetaInstagramProviderAttempt(
	ctx context.Context,
	expected metaInstagramRevalidationSnapshot,
	allowPendingDeauthorization bool,
	requireRuntimeHealthy bool,
	attempt func(metaInstagramRevalidationSnapshot) error,
) error {
	if a == nil || a.DB == nil || a.Config == nil || attempt == nil ||
		expected.OrganizationID == uuid.Nil || expected.Account.ID == uuid.Nil {
		return errMetaInstagramProviderAttemptFenced
	}
	return database.WithTenantReadCommitted(
		a.rootApp().DB, expected.OrganizationID, func(tx *gorm.DB) error {
			if err := lockChannelAIOrganizationScopeTx(tx, expected.OrganizationID); err != nil {
				return err
			}
			var account models.ChannelAccount
			if err := tx.WithContext(ctx).Clauses(clause.Locking{Strength: "UPDATE"}).Where(
				"id = ? AND organization_id = ? AND channel = ? AND provider = ?",
				expected.Account.ID, expected.OrganizationID,
				models.ChannelInstagram, channelapi.RelayProvider,
			).First(&account).Error; err != nil {
				return errMetaInstagramProviderAttemptFenced
			}
			if account.Status != expected.Account.Status ||
				account.ExternalAccountID != expected.Account.ExternalAccountID ||
				stringConfigValue(account.Metadata, "meta_ownership_state") !=
					stringConfigValue(expected.Account.Metadata, "meta_ownership_state") ||
				stringConfigValue(account.Metadata, "meta_ownership_checked_at") !=
					stringConfigValue(expected.Account.Metadata, "meta_ownership_checked_at") {
				return errMetaInstagramProviderAttemptFenced
			}
			var credentials []models.ChannelCredential
			if err := tx.WithContext(ctx).Clauses(clause.Locking{Strength: "UPDATE"}).Where(
				"organization_id = ? AND channel_account_id = ? AND status IN ?",
				expected.OrganizationID, account.ID,
				[]models.ChannelCredentialStatus{
					models.ChannelCredentialStatusActive,
					models.ChannelCredentialStatusExpiring,
				},
			).Order("version DESC, id ASC").Find(&credentials).Error; err != nil {
				return err
			}
			account.Credentials = credentials
			now := time.Now().UTC()
			oauth := currentMetaRegistryCredential(
				credentials, models.ChannelCredentialKindOAuth, now,
			)
			webhook := currentMetaRegistryCredential(
				credentials, models.ChannelCredentialKindWebhook, now,
			)
			if !metaInstagramCredentialPairGenerationValid(oauth, webhook, now) ||
				oauth.ID != expected.OAuth.ID || oauth.Version != expected.OAuth.Version ||
				webhook.ID != expected.Webhook.ID || webhook.Version != expected.Webhook.Version ||
				!metaInstagramSubscribedOperationMatchesCredentials(
					account.Metadata, *oauth, *webhook,
				) {
				return errMetaInstagramProviderAttemptFenced
			}
			if requireRuntimeHealthy {
				if a.metaInstagramCurrentRuntimeBindingReason(
					&account, expected.OrganizationID, now,
				) != "" {
					return errMetaInstagramProviderAttemptFenced
				}
			} else if a.metaInstagramReleaseGuardReasonWithDeauthorization(
				account, expected.OrganizationID, allowPendingDeauthorization,
			) != "" {
				return errMetaInstagramProviderAttemptFenced
			}
			if err := a.rootApp().scopedApp(tx, expected.OrganizationID).
				metaInstagramRevalidationJournalFence(account, *oauth); err != nil {
				return errMetaInstagramProviderAttemptFenced
			}
			accessToken, err := decryptRequiredMetaRegistrySecret(
				oauth.CredentialBlob, "access_token", a.Config.App.EncryptionKey,
			)
			if err != nil {
				return errMetaInstagramProviderAttemptFenced
			}
			locked := metaInstagramRevalidationSnapshot{
				OrganizationID: expected.OrganizationID,
				Account:        account, OAuth: *oauth, Webhook: *webhook,
				AccessToken: accessToken,
			}
			if checkedAt := stringConfigValue(account.Metadata, "meta_ownership_checked_at"); checkedAt != "" {
				locked.PreviousCheck, err = time.Parse(time.RFC3339Nano, checkedAt)
				if err != nil {
					return errMetaInstagramProviderAttemptFenced
				}
			}
			return attempt(locked)
		},
	)
}

func metaInstagramCredentialGenerationValid(
	credential models.ChannelCredential,
	now time.Time,
) bool {
	return credential.ID != uuid.Nil && credential.Version > 0 &&
		credential.OrganizationID != uuid.Nil && credential.ChannelAccountID != uuid.Nil &&
		!credential.CreatedAt.IsZero() && !credential.CreatedAt.After(now.Add(time.Minute))
}

// metaInstagramCredentialPairGenerationValid is the exact Instagram Login
// generation contract used at every provider and persistence boundary. Meta's
// long-lived Instagram access token is still expiring data: a nil/near-expiry
// OAuth timestamp is not permission to route. Webhook credentials may be
// non-expiring, but their persisted generation must be real and current.
func metaInstagramCredentialPairGenerationValid(
	oauth, webhook *models.ChannelCredential,
	now time.Time,
) bool {
	if oauth == nil || webhook == nil || oauth.ID == webhook.ID ||
		oauth.Kind != models.ChannelCredentialKindOAuth ||
		webhook.Kind != models.ChannelCredentialKindWebhook ||
		oauth.OrganizationID != webhook.OrganizationID ||
		oauth.ChannelAccountID != webhook.ChannelAccountID ||
		!channelapi.CredentialIsCurrent(oauth, now) ||
		!channelapi.CredentialIsCurrent(webhook, now) ||
		!metaInstagramCredentialGenerationValid(*oauth, now) ||
		!metaInstagramCredentialGenerationValid(*webhook, now) ||
		oauth.ExpiresAt == nil ||
		!oauth.ExpiresAt.After(now.Add(metaInstagramProviderOperationLimit)) {
		return false
	}
	return true
}

func (a *App) metaInstagramCurrentRuntimeBindingReason(
	account *models.ChannelAccount,
	organizationID uuid.UUID,
	now time.Time,
) string {
	if account == nil {
		return "managed_instagram_binding_missing"
	}
	if reason := a.metaInstagramReleaseGuardReason(*account, organizationID); reason != "" {
		return reason
	}
	if stringConfigValue(account.Metadata, "meta_ownership_state") != metaregistry.OwnershipVerified ||
		stringConfigValue(account.Metadata, "meta_subscription_state") != "verified" {
		return "managed_instagram_ownership_or_subscription_invalid"
	}
	checkedAt, err := time.Parse(
		time.RFC3339Nano,
		stringConfigValue(account.Metadata, "meta_ownership_checked_at"),
	)
	if err != nil || checkedAt.IsZero() || checkedAt.After(now.Add(time.Minute)) {
		return "managed_instagram_ownership_timestamp_invalid"
	}
	maxAge := time.Duration(a.Config.MetaRegistry.OwnershipMaxAgeMins) * time.Minute
	if maxAge <= 0 || maxAge > 7*24*time.Hour {
		maxAge = 24 * time.Hour
	}
	if now.Sub(checkedAt) > maxAge {
		return "managed_instagram_ownership_stale"
	}
	oauth := currentMetaRegistryCredential(
		account.Credentials, models.ChannelCredentialKindOAuth, now,
	)
	webhook := currentMetaRegistryCredential(
		account.Credentials, models.ChannelCredentialKindWebhook, now,
	)
	if !metaInstagramCredentialPairGenerationValid(oauth, webhook, now) {
		return "managed_instagram_credential_generation_invalid"
	}
	if !metaInstagramSubscribedOperationMatchesCredentials(
		account.Metadata, *oauth, *webhook,
	) {
		return "managed_subscription_fence_invalid"
	}
	return ""
}

func (a *App) quarantineMetaInstagramAccount(
	organizationID, accountID uuid.UUID,
	reason string,
	checkedAt time.Time,
) error {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return errors.New("instagram quarantine reason is required")
	}
	return a.WithCommittedTenantApp(organizationID, func(scoped *App) error {
		if err := lockChannelAIOrganizationScopeTx(scoped.DB, organizationID); err != nil {
			return err
		}
		var account models.ChannelAccount
		if err := scoped.DB.Clauses(clause.Locking{Strength: "UPDATE"}).Where(
			"id = ? AND organization_id = ? AND channel = ? AND provider = ?",
			accountID, organizationID, models.ChannelInstagram, channelapi.RelayProvider,
		).First(&account).Error; err != nil {
			return err
		}
		return scoped.quarantineMetaInstagramAccountTx(&account, reason, checkedAt)
	})
}

// quarantineMetaInstagramAccountTx mutates one already locked managed account.
// The caller must hold the tenant organization mutex, which makes the row
// downgrade and every cancellable scheduled/outbox transition one commit.
func (a *App) quarantineMetaInstagramAccountTx(
	account *models.ChannelAccount,
	reason string,
	checkedAt time.Time,
) error {
	if a == nil || a.DB == nil || account == nil || account.ID == uuid.Nil ||
		account.OrganizationID == uuid.Nil {
		return errors.New("managed Instagram quarantine transaction is required")
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return errors.New("instagram quarantine reason is required")
	}
	if account.Status == models.ChannelAccountStatusDisconnected {
		return nil
	}
	checkedAt = checkedAt.UTC()
	if previous, err := time.Parse(
		time.RFC3339Nano, stringConfigValue(account.Metadata, "meta_ownership_checked_at"),
	); err == nil && !checkedAt.After(previous) {
		checkedAt = previous.Add(time.Nanosecond)
	}
	metadata := cloneJSONB(account.Metadata)
	metadata["meta_ownership_state"] = metaregistry.OwnershipStale
	metadata["meta_ownership_checked_at"] = checkedAt.Format(time.RFC3339Nano)
	metadata["meta_ownership_reason"] = reason
	metadata["meta_activation_state"] = "quarantined"
	if metaInstagramDeletionReconciliationPending(account.Metadata) {
		metadata["meta_activation_state"] = "data_deletion_reconciliation_required"
	} else if metaDeauthorizationReconciliationPending(account.Metadata) {
		metadata["meta_activation_state"] = "deauthorization_reconciliation_required"
	}
	accountConfig := cloneJSONB(account.Config)
	accountConfig["outbound_enabled"] = false
	accountConfig["ai_reply_enabled"] = false
	if account.Status == models.ChannelAccountStatusDegraded &&
		stringConfigValue(account.Metadata, "meta_ownership_reason") == reason &&
		!boolConfigValue(account.Config, "outbound_enabled") &&
		!boolConfigValue(account.Config, "ai_reply_enabled") {
		// A seeded legacy row can already look quarantined without ever
		// having run the transactional queue cancellation. Re-run the
		// idempotent cancellation before treating this state as settled.
		return cancelManagedMetaQueuedWorkForAccountTx(
			a.DB, account.OrganizationID, account.ID, "managed_instagram_quarantined",
		)
	}
	result := a.DB.Model(&models.ChannelAccount{}).
		Where("id = ? AND organization_id = ?", account.ID, account.OrganizationID).
		Updates(map[string]any{
			"status": models.ChannelAccountStatusDegraded,
			"config": accountConfig, "metadata": metadata,
			"last_error":    "Managed Instagram binding is quarantined",
			"last_error_at": checkedAt, "connected_at": nil, "updated_at": checkedAt,
		})
	if result.Error != nil || result.RowsAffected != 1 {
		if result.Error != nil {
			return result.Error
		}
		return metaregistry.ErrNotFound
	}
	if err := cancelManagedMetaQueuedWorkForAccountTx(
		a.DB, account.OrganizationID, account.ID, "managed_instagram_quarantined",
	); err != nil {
		return err
	}
	actorID := uuid.Nil
	if account.UpdatedByID != nil {
		actorID = *account.UpdatedByID
	} else if account.CreatedByID != nil {
		actorID = *account.CreatedByID
	}
	if actorID == uuid.Nil {
		// Legacy rows may predate actor columns. Quarantine must still fail
		// closed after a rollback/forward rollout, so use the oldest extant
		// tenant membership as the system audit attribution. This matches the
		// registry mutation path and never grants that member approval powers.
		var membership models.UserOrganization
		if err := a.DB.Where("organization_id = ?", account.OrganizationID).
			Order("created_at").First(&membership).Error; err != nil {
			return err
		}
		actorID = membership.UserID
	}
	return audit.LogAudit(
		a.DB, account.OrganizationID, actorID, metaRegistryAuditActor,
		"meta_channel_registry", account.ID, models.AuditActionUpdated,
		nil, nil,
		map[string]any{
			"field": "ownership_state", "old_value": stringConfigValue(account.Metadata, "meta_ownership_state"),
			"new_value": metaregistry.OwnershipStale,
		},
		map[string]any{"field": "quarantine_reason", "old_value": nil, "new_value": reason},
	)
}

func (a *App) handleMetaInstagramLifecycleError(
	snapshot metaInstagramRevalidationSnapshot,
	err error,
	now time.Time,
) {
	outcome, reason := classifyMetaInstagramRevalidationError(err)
	if outcome == "transient" {
		maxAge := time.Duration(a.Config.MetaRegistry.OwnershipMaxAgeMins) * time.Minute
		if !snapshot.PreviousCheck.IsZero() && now.Sub(snapshot.PreviousCheck) <= maxAge {
			return
		}
		outcome = metaregistry.OwnershipStale
		reason = "provider_revalidation_deadline_exceeded"
	}
	a.applyMetaInstagramLifecycleOutcome(snapshot, outcome, reason, time.Now().UTC())
}

func classifyMetaInstagramRevalidationError(err error) (outcome, reason string) {
	var binding *metaInstagramLifecycleBindingError
	if errors.As(err, &binding) {
		if binding.Revoked {
			return metaregistry.OwnershipRevoked, binding.Reason
		}
		return metaregistry.OwnershipStale, binding.Reason
	}
	var provider *metaInstagramProviderError
	if errors.As(err, &provider) {
		if provider.Code == 190 || provider.StatusCode == 401 || provider.StatusCode == 403 {
			return metaregistry.OwnershipRevoked, "provider_authorization_revoked"
		}
		if provider.StatusCode == 429 || provider.StatusCode >= 500 {
			return "transient", "provider_temporarily_unavailable"
		}
	}
	return "transient", "provider_revalidation_failed"
}

func (a *App) applyMetaInstagramLifecycleOutcome(
	snapshot metaInstagramRevalidationSnapshot,
	outcome, reason string,
	checkedAt time.Time,
) {
	if outcome != metaregistry.OwnershipStale && outcome != metaregistry.OwnershipRevoked {
		return
	}
	if snapshot.PreviousCheck.IsZero() {
		_ = a.quarantineMetaInstagramAccount(
			snapshot.OrganizationID, snapshot.Account.ID, reason, checkedAt,
		)
		return
	}
	if !checkedAt.After(snapshot.PreviousCheck) {
		checkedAt = snapshot.PreviousCheck.Add(time.Nanosecond)
	}
	mutation := metaregistry.MutationRequest{
		ChannelAccountID: snapshot.Account.ID,
		CredentialID:     snapshot.OAuth.ID, CredentialVersion: snapshot.OAuth.Version,
		WebhookCredentialID: snapshot.Webhook.ID, WebhookCredentialVersion: snapshot.Webhook.Version,
		Outcome: outcome, Reason: reason, CheckedAt: checkedAt,
	}
	err := a.WithCommittedTenantApp(snapshot.OrganizationID, func(scoped *App) error {
		if err := lockChannelAIOrganizationScopeTx(
			scoped.DB, snapshot.OrganizationID,
		); err != nil {
			return err
		}
		applied, err := scoped.applyMetaRegistryMutation(mutation, outcome)
		if err != nil {
			return err
		}
		if !applied {
			return errMetaMessengerSubscriptionFence
		}
		if err := scoped.resolvePendingMetaInstagramDeauthorizationRevalidation(
			snapshot, outcome, checkedAt,
		); err != nil {
			return err
		}
		return cancelManagedMetaQueuedWorkForAccountTx(
			scoped.DB, snapshot.OrganizationID, snapshot.Account.ID,
			"managed_instagram_ownership_downgraded",
		)
	})
	if err != nil {
		a.Log.Warn("Instagram lifecycle downgrade was superseded", "channel_account_id", snapshot.Account.ID)
	}
}

func (a *App) storeMetaInstagramRevalidation(
	snapshot metaInstagramRevalidationSnapshot,
	accessToken string,
	refreshed bool,
	expiresAt *time.Time,
	inspection metaInstagramTokenInspection,
	checkedAt time.Time,
) error {
	if strings.TrimSpace(accessToken) == "" || expiresAt == nil ||
		!expiresAt.After(checkedAt.Add(metaInstagramProviderOperationLimit)) ||
		inspection.AppID != strings.TrimSpace(a.Config.MetaInstagram.AppID) ||
		inspection.UserID != stringConfigValue(snapshot.Account.Metadata, "meta_authorizing_user_id") {
		return metaregistry.ErrInvalidRequest
	}
	if !snapshot.PreviousCheck.IsZero() && !checkedAt.After(snapshot.PreviousCheck) {
		checkedAt = snapshot.PreviousCheck.Add(time.Nanosecond)
	}
	return a.WithCommittedTenantApp(snapshot.OrganizationID, func(scoped *App) error {
		if err := lockChannelAIOrganizationScopeTx(
			scoped.DB, snapshot.OrganizationID,
		); err != nil {
			return err
		}
		var account models.ChannelAccount
		if err := scoped.DB.Clauses(clause.Locking{Strength: "UPDATE"}).Where(
			"id = ? AND organization_id = ? AND channel = ? AND provider = ?",
			snapshot.Account.ID, snapshot.OrganizationID,
			models.ChannelInstagram, channelapi.RelayProvider,
		).First(&account).Error; err != nil {
			return err
		}
		if scoped.metaInstagramLifecycleReleaseGuardReason(account, snapshot.OrganizationID) != "" ||
			inspection.UserID != stringConfigValue(account.Metadata, "meta_authorizing_user_id") ||
			!metaInstagramSubscribedOperationMatchesCredentials(
				account.Metadata, snapshot.OAuth, snapshot.Webhook,
			) {
			return errMetaMessengerSubscriptionFence
		}
		if previous, err := time.Parse(
			time.RFC3339Nano, stringConfigValue(account.Metadata, "meta_ownership_checked_at"),
		); err == nil && !checkedAt.After(previous) {
			return errMetaMessengerSubscriptionFence
		}
		var credentials []models.ChannelCredential
		if err := scoped.DB.Clauses(clause.Locking{Strength: "UPDATE"}).Where(
			"organization_id = ? AND channel_account_id = ? AND status IN ?",
			snapshot.OrganizationID, account.ID,
			[]models.ChannelCredentialStatus{
				models.ChannelCredentialStatusActive,
				models.ChannelCredentialStatusExpiring,
			},
		).Order("version DESC, id ASC").Find(&credentials).Error; err != nil {
			return err
		}
		var oauth, webhook *models.ChannelCredential
		for index := range credentials {
			switch credentials[index].Kind {
			case models.ChannelCredentialKindOAuth:
				oauth = &credentials[index]
			case models.ChannelCredentialKindWebhook:
				webhook = &credentials[index]
			}
		}
		if !metaInstagramCredentialPairGenerationValid(
			oauth, webhook, time.Now().UTC(),
		) || oauth.ID != snapshot.OAuth.ID || oauth.Version != snapshot.OAuth.Version ||
			webhook.ID != snapshot.Webhook.ID || webhook.Version != snapshot.Webhook.Version {
			return errMetaMessengerSubscriptionFence
		}
		if err := scoped.metaInstagramRevalidationJournalFence(
			account, *oauth,
		); err != nil {
			return err
		}
		credentialUpdates := map[string]any{
			"expires_at": expiresAt.UTC(), "last_validated_at": checkedAt,
			"status": models.ChannelCredentialStatusActive, "updated_at": checkedAt,
		}
		if refreshed {
			encryptedToken, err := appcrypto.Encrypt(accessToken, a.Config.App.EncryptionKey)
			if err != nil || !appcrypto.IsEncrypted(encryptedToken) {
				return errors.New("instagram refreshed credential encryption failed")
			}
			blob := cloneJSONB(oauth.CredentialBlob)
			blob["access_token"] = encryptedToken
			blob["authority_token"] = encryptedToken
			credentialUpdates["credential_blob"] = blob
			credentialUpdates["rotated_at"] = checkedAt
		}
		result := scoped.DB.Model(&models.ChannelCredential{}).Where(
			"id = ? AND organization_id = ? AND channel_account_id = ? AND version = ? AND status IN ?",
			oauth.ID, snapshot.OrganizationID, account.ID, oauth.Version,
			[]models.ChannelCredentialStatus{
				models.ChannelCredentialStatusActive,
				models.ChannelCredentialStatusExpiring,
			},
		).Updates(credentialUpdates)
		if result.Error != nil || result.RowsAffected != 1 {
			if result.Error != nil {
				return result.Error
			}
			return errMetaMessengerSubscriptionFence
		}

		metadata := cloneJSONB(account.Metadata)
		metadata["meta_ownership_state"] = metaregistry.OwnershipVerified
		metadata["meta_ownership_checked_at"] = checkedAt.Format(time.RFC3339Nano)
		metadata["meta_ownership_reason"] = "scheduled_graph_revalidation"
		metadata["meta_granted_scopes"] = append([]string(nil), inspection.Scopes...)
		metadata["meta_subscription_state"] = "verified"
		accountConfig := cloneJSONB(account.Config)
		updates := map[string]any{
			"metadata": metadata, "updated_at": checkedAt,
		}
		if account.Status == models.ChannelAccountStatusDegraded {
			accountConfig["outbound_enabled"] = false
			accountConfig["ai_reply_enabled"] = false
			metadata["meta_activation_state"] = "awaiting_health"
			for _, key := range []string{
				"meta_health_checked_at", "meta_health_oauth_credential_id",
				"meta_health_oauth_version", "meta_health_webhook_credential_id",
				"meta_health_webhook_version",
			} {
				delete(metadata, key)
			}
			updates["status"] = models.ChannelAccountStatusPending
			updates["config"] = accountConfig
			updates["last_health_check_at"] = nil
			updates["last_error"] = ""
			updates["last_error_at"] = nil
			updates["connected_at"] = nil
		}
		result = scoped.DB.Model(&models.ChannelAccount{}).
			Where("id = ? AND organization_id = ?", account.ID, snapshot.OrganizationID).
			Updates(updates)
		if result.Error != nil || result.RowsAffected != 1 {
			if result.Error != nil {
				return result.Error
			}
			return errMetaMessengerSubscriptionFence
		}
		if err := scoped.resolvePendingMetaInstagramDeauthorizationRevalidation(
			snapshot, metaregistry.OwnershipVerified, checkedAt,
		); err != nil {
			return err
		}
		actorID := uuid.Nil
		if account.UpdatedByID != nil {
			actorID = *account.UpdatedByID
		} else if account.CreatedByID != nil {
			actorID = *account.CreatedByID
		}
		if actorID == uuid.Nil {
			var membership models.UserOrganization
			if err := scoped.DB.Where("organization_id = ?", snapshot.OrganizationID).
				Order("created_at").First(&membership).Error; err != nil {
				return err
			}
			actorID = membership.UserID
		}
		details := []map[string]any{
			{"field": "ownership_state", "old_value": stringConfigValue(account.Metadata, "meta_ownership_state"), "new_value": metaregistry.OwnershipVerified},
		}
		if refreshed {
			details = append(details, map[string]any{
				"field": "oauth_credential_refresh", "old_value": "********", "new_value": "********",
			})
		}
		return audit.LogAudit(
			scoped.DB, snapshot.OrganizationID, actorID, metaRegistryAuditActor,
			"meta_channel_registry", account.ID, models.AuditActionUpdated,
			nil, nil, details...,
		)
	})
}

func (a *App) resolvePendingMetaInstagramDeauthorizationRevalidation(
	snapshot metaInstagramRevalidationSnapshot,
	outcome string,
	checkedAt time.Time,
) error {
	return a.resolvePendingMetaDeauthorizationFence(
		snapshot.Account.Metadata,
		metaDeauthorizationRevalidationFence{
			OrganizationID: snapshot.OrganizationID,
			AccountID:      snapshot.Account.ID,
			Channel:        models.ChannelInstagram,
			PlatformAppID:  stringConfigValue(snapshot.Account.Metadata, "meta_platform_app_id"),
			AuthorizingUserID: stringConfigValue(
				snapshot.Account.Metadata, "meta_authorizing_user_id",
			),
			OAuthCredentialID: snapshot.OAuth.ID, OAuthVersion: snapshot.OAuth.Version,
			WebhookCredentialID: snapshot.Webhook.ID, WebhookVersion: snapshot.Webhook.Version,
		},
		outcome, checkedAt,
	)
}
