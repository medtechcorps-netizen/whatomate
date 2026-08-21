package handlers

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	channelapi "github.com/shridarpatil/whatomate/internal/channel"
	appcrypto "github.com/shridarpatil/whatomate/internal/crypto"
	"github.com/shridarpatil/whatomate/internal/database"
	"github.com/shridarpatil/whatomate/internal/metaregistry"
	"github.com/shridarpatil/whatomate/internal/models"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const metaMessengerRevalidationLease = 2 * time.Minute

type metaMessengerRevalidationSnapshot struct {
	OrganizationID uuid.UUID
	Account        models.ChannelAccount
	OAuth          models.ChannelCredential
	Webhook        models.ChannelCredential
	AuthorityToken string
	PageToken      string
	PreviousCheck  time.Time
}

// RunMetaMessengerLifecycle periodically revalidates every managed binding
// before its ownership lease expires. Only Messenger has a registered provider
// validator in this release; Instagram remains intentionally disabled.
func (a *App) RunMetaMessengerLifecycle(ctx context.Context) error {
	if a == nil || a.Config == nil || !a.Config.MetaMessenger.Enabled || !a.Config.MetaRegistry.Enabled {
		return nil
	}
	if a.Redis == nil {
		return errors.New("meta Messenger lifecycle requires Redis")
	}
	interval := time.Duration(a.Config.MetaMessenger.SchedulerIntervalSeconds) * time.Second
	if interval < 10*time.Second || interval > 5*time.Minute {
		return errors.New("meta Messenger scheduler interval is outside safe bounds")
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if err := a.revalidateDueMetaMessengerBindings(ctx); err != nil && !errors.Is(err, context.Canceled) {
			a.Log.Warn("Messenger lifecycle sweep failed")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (a *App) revalidateDueMetaMessengerBindings(ctx context.Context) error {
	now := time.Now().UTC()
	if err := a.cleanupMetaDeauthorizationEvents(now); err != nil {
		return err
	}
	if err := a.quarantineMetaMessengerOrganizationsOutsideRuntimeAllowlist(now); err != nil {
		return err
	}
	maxAge := time.Duration(a.Config.MetaRegistry.OwnershipMaxAgeMins) * time.Minute
	lead := time.Duration(a.Config.MetaMessenger.RevalidationLeadMins) * time.Minute
	dueBefore := now.Add(-(maxAge - lead))
	after := uuid.Nil
	for {
		organizations, err := a.metaMessengerLifecycleOrganizations(after, dueBefore)
		if err != nil {
			return err
		}
		for _, organizationID := range organizations {
			if err := a.revalidateMetaMessengerOrganization(ctx, organizationID, dueBefore, now); err != nil &&
				!errors.Is(err, context.Canceled) {
				a.Log.Warn("Messenger tenant lifecycle sweep failed", "organization_id", organizationID)
			}
			after = organizationID
		}
		if len(organizations) < 100 {
			return nil
		}
	}
}

func (a *App) quarantineMetaMessengerOrganizationsOutsideRuntimeAllowlist(now time.Time) error {
	if a == nil || a.Config == nil || a.Config.MetaMessenger.AllowAllOrganizations {
		return nil
	}
	// This pilot-only scan intentionally includes every managed Messenger
	// workspace, not only accounts whose ownership proof is due. It makes an
	// allowlist removal an immediate runtime kill switch on the next sweep.
	allManagedBefore := now.UTC().Add(100 * 365 * 24 * time.Hour)
	after := uuid.Nil
	for {
		organizations, err := a.metaMessengerLifecycleOrganizations(after, allManagedBefore)
		if err != nil {
			return err
		}
		for _, organizationID := range organizations {
			if !a.metaMessengerOrganizationAllowed(organizationID) {
				if err := a.quarantineMetaMessengerOrganization(organizationID, now); err != nil {
					return err
				}
			}
			after = organizationID
		}
		if len(organizations) < 100 {
			return nil
		}
	}
}

func (a *App) quarantineMetaMessengerOrganization(organizationID uuid.UUID, checkedAt time.Time) error {
	if a.metaMessengerOrganizationAllowed(organizationID) {
		return nil
	}
	return a.WithCommittedTenantApp(organizationID, func(scoped *App) error {
		var accounts []models.ChannelAccount
		if err := scoped.DB.Preload("Credentials", func(db *gorm.DB) *gorm.DB {
			return db.Order("version DESC")
		}).Where(
			"organization_id = ? AND channel = ? AND provider = ? AND status IN ? AND config ->> 'meta_management_mode' = ?",
			organizationID, models.ChannelMessenger, channelapi.RelayProvider,
			[]models.ChannelAccountStatus{models.ChannelAccountStatusPending, models.ChannelAccountStatusActive, models.ChannelAccountStatusDegraded},
			metaregistry.ManagementModePlatformOAuth,
		).Order("id").Find(&accounts).Error; err != nil {
			return err
		}
		for index := range accounts {
			account := &accounts[index]
			if account.Status == models.ChannelAccountStatusDegraded &&
				stringConfigValue(account.Metadata, "meta_ownership_state") == metaregistry.OwnershipStale &&
				stringConfigValue(account.Metadata, "meta_ownership_reason") == "organization_removed_from_runtime_allowlist" &&
				!boolConfigValue(account.Config, "outbound_enabled") &&
				!boolConfigValue(account.Config, "ai_reply_enabled") {
				continue
			}
			oauth := currentMetaRegistryCredential(account.Credentials, models.ChannelCredentialKindOAuth, checkedAt)
			webhook := currentMetaRegistryCredential(account.Credentials, models.ChannelCredentialKindWebhook, checkedAt)
			if oauth == nil || webhook == nil {
				return errors.New("managed Messenger allowlist quarantine has no credential fence")
			}
			mutationTime := checkedAt.UTC()
			if previous, err := time.Parse(time.RFC3339Nano, stringConfigValue(account.Metadata, "meta_ownership_checked_at")); err == nil &&
				!mutationTime.After(previous) {
				mutationTime = previous.Add(time.Nanosecond)
			}
			applied, err := scoped.applyMetaRegistryMutation(metaregistry.MutationRequest{
				ChannelAccountID: account.ID,
				CredentialID:     oauth.ID, CredentialVersion: oauth.Version,
				WebhookCredentialID: webhook.ID, WebhookCredentialVersion: webhook.Version,
				Outcome: metaregistry.OwnershipStale, Reason: "organization_removed_from_runtime_allowlist",
				CheckedAt: mutationTime,
			}, metaregistry.OwnershipStale)
			if err != nil {
				return err
			}
			if !applied {
				return errMetaMessengerSubscriptionFence
			}
		}
		return nil
	})
}

func (a *App) metaMessengerLifecycleOrganizations(after uuid.UUID, dueBefore time.Time) ([]uuid.UUID, error) {
	root := a.rootApp()
	var organizations []uuid.UUID
	if a.rlsEnabled() {
		if err := root.DB.Raw(
			"SELECT public.rereply_ready_meta_lifecycle_orgs(?, ?, ?)",
			after, 100, dueBefore,
		).Scan(&organizations).Error; err != nil {
			return nil, err
		}
		return organizations, nil
	}
	if err := root.DB.Model(&models.ChannelAccount{}).Distinct("organization_id").
		Where("channel = ? AND provider = ? AND status IN ? AND config ->> 'meta_management_mode' = ? AND (metadata ->> ? IS NOT NULL OR metadata ->> 'meta_ownership_checked_at' IS NULL OR metadata ->> 'meta_ownership_checked_at' <= ?)",
			models.ChannelMessenger, channelapi.RelayProvider,
			[]models.ChannelAccountStatus{models.ChannelAccountStatusPending, models.ChannelAccountStatusActive, models.ChannelAccountStatusDegraded},
			metaregistry.ManagementModePlatformOAuth, metaDeauthorizationPendingDigestKey, dueBefore.Format(time.RFC3339Nano)).
		Where("organization_id > ?", after).
		Order("organization_id").
		Limit(100).Pluck("organization_id", &organizations).Error; err != nil {
		return nil, err
	}
	return organizations, nil
}

func (a *App) revalidateMetaMessengerOrganization(
	ctx context.Context, organizationID uuid.UUID, dueBefore, now time.Time,
) error {
	after := uuid.Nil
	for {
		var accountIDs []uuid.UUID
		if err := database.WithTenantReadCommitted(a.rootApp().DB, organizationID, func(tx *gorm.DB) error {
			return tx.Model(&models.ChannelAccount{}).Where(
				"organization_id = ? AND id > ? AND channel = ? AND provider = ? AND status IN ? AND config ->> 'meta_management_mode' = ? AND (metadata ->> ? IS NOT NULL OR metadata ->> 'meta_ownership_checked_at' IS NULL OR metadata ->> 'meta_ownership_checked_at' <= ?)",
				organizationID, after, models.ChannelMessenger, channelapi.RelayProvider,
				[]models.ChannelAccountStatus{models.ChannelAccountStatusPending, models.ChannelAccountStatusActive, models.ChannelAccountStatusDegraded},
				metaregistry.ManagementModePlatformOAuth, metaDeauthorizationPendingDigestKey, dueBefore.Format(time.RFC3339Nano),
			).Order("id").Limit(100).Pluck("id", &accountIDs).Error
		}); err != nil {
			return err
		}
		for _, accountID := range accountIDs {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			leaseKey := "meta-messenger:revalidate:" + accountID.String()
			claimed, err := a.Redis.SetNX(ctx, leaseKey, uuid.NewString(), metaMessengerRevalidationLease).Result()
			if err != nil {
				return err
			}
			if claimed {
				a.revalidateOneMetaMessengerBinding(ctx, organizationID, accountID, now)
			}
			after = accountID
		}
		if len(accountIDs) < 100 {
			return nil
		}
	}
}

func (a *App) revalidateOneMetaMessengerBinding(
	ctx context.Context, organizationID, accountID uuid.UUID, now time.Time,
) {
	if !a.metaMessengerOrganizationAllowed(organizationID) {
		if err := a.quarantineMetaMessengerOrganization(organizationID, now); err != nil {
			a.Log.Warn("Messenger allowlist quarantine failed", "organization_id", organizationID)
		}
		return
	}
	snapshot, err := a.loadMetaMessengerRevalidationSnapshot(organizationID, accountID, now)
	if err != nil {
		return
	}
	providerCtx, cancel := context.WithTimeout(ctx, metaMessengerProviderOperationLimit)
	defer cancel()
	outcome, reason := a.checkMetaMessengerOwnership(providerCtx, snapshot)
	checkedAt := time.Now().UTC()
	if !checkedAt.After(snapshot.PreviousCheck) {
		checkedAt = snapshot.PreviousCheck.Add(time.Nanosecond)
	}
	maxAge := time.Duration(a.Config.MetaRegistry.OwnershipMaxAgeMins) * time.Minute
	if outcome == "transient" {
		if now.Sub(snapshot.PreviousCheck) <= maxAge {
			return
		}
		outcome = metaregistry.OwnershipStale
		reason = "provider_revalidation_deadline_exceeded"
	}
	if outcome == "" {
		outcome = metaregistry.OwnershipVerified
	}
	mutation := metaregistry.MutationRequest{
		ChannelAccountID: snapshot.Account.ID,
		CredentialID:     snapshot.OAuth.ID, CredentialVersion: snapshot.OAuth.Version,
		WebhookCredentialID: snapshot.Webhook.ID, WebhookCredentialVersion: snapshot.Webhook.Version,
		Outcome: outcome, Reason: reason, CheckedAt: checkedAt,
	}
	if err := a.WithCommittedTenantApp(organizationID, func(scoped *App) error {
		applied, applyErr := scoped.applyMetaRegistryMutation(mutation, outcome)
		if applyErr != nil {
			return applyErr
		}
		if !applied {
			return errMetaMessengerSubscriptionFence
		}
		return scoped.resolvePendingMetaDeauthorizationRevalidation(snapshot, outcome, checkedAt)
	}); err != nil {
		a.Log.Warn("Messenger lifecycle mutation was superseded", "channel_account_id", accountID)
	}
}

func (a *App) resolvePendingMetaDeauthorizationRevalidation(
	snapshot metaMessengerRevalidationSnapshot,
	outcome string,
	checkedAt time.Time,
) error {
	return a.resolvePendingMetaDeauthorizationFence(
		snapshot.Account.Metadata,
		metaDeauthorizationRevalidationFence{
			OrganizationID: snapshot.OrganizationID,
			AccountID:      snapshot.Account.ID,
			Channel:        snapshot.Account.Channel,
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

type metaDeauthorizationRevalidationFence struct {
	OrganizationID      uuid.UUID
	AccountID           uuid.UUID
	Channel             models.Channel
	PlatformAppID       string
	AuthorizingUserID   string
	OAuthCredentialID   uuid.UUID
	OAuthVersion        int
	WebhookCredentialID uuid.UUID
	WebhookVersion      int
}

// resolvePendingMetaDeauthorizationFence settles an ambiguous same-second
// callback only after a provider revalidation is fenced to the exact current
// credential generation and channel/app/profile binding. Both managed Meta
// lifecycles use this helper; stale outcomes deliberately leave the callback
// pending and non-routable.
func (a *App) resolvePendingMetaDeauthorizationFence(
	snapshotMetadata models.JSONB,
	fence metaDeauthorizationRevalidationFence,
	outcome string,
	checkedAt time.Time,
) error {
	digest := stringConfigValue(snapshotMetadata, metaDeauthorizationPendingDigestKey)
	issuedAt := stringConfigValue(snapshotMetadata, metaDeauthorizationPendingIssuedKey)
	if digest == "" || issuedAt == "" || outcome == metaregistry.OwnershipStale {
		return nil
	}
	if outcome != metaregistry.OwnershipVerified && outcome != metaregistry.OwnershipRevoked {
		return nil
	}
	if fence.OrganizationID == uuid.Nil || fence.AccountID == uuid.Nil ||
		(fence.Channel != models.ChannelMessenger && fence.Channel != models.ChannelInstagram) ||
		!validCanonicalMetaID(strings.TrimSpace(fence.PlatformAppID)) ||
		!validCanonicalMetaID(strings.TrimSpace(fence.AuthorizingUserID)) ||
		fence.OAuthCredentialID == uuid.Nil || fence.OAuthVersion < 1 ||
		fence.WebhookCredentialID == uuid.Nil || fence.WebhookVersion < 1 {
		return errMetaMessengerSubscriptionFence
	}
	var account models.ChannelAccount
	if err := a.DB.Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("id = ? AND organization_id = ? AND channel = ? AND provider = ?",
			fence.AccountID, fence.OrganizationID, fence.Channel, channelapi.RelayProvider).
		First(&account).Error; err != nil {
		return err
	}
	if !metaRegistryControlPlaneConfig(account.Config) ||
		stringConfigValue(account.Metadata, "meta_platform_app_id") != strings.TrimSpace(fence.PlatformAppID) ||
		stringConfigValue(account.Metadata, "meta_authorizing_user_id") != strings.TrimSpace(fence.AuthorizingUserID) ||
		(fence.Channel == models.ChannelInstagram &&
			stringConfigValue(account.Config, "instagram_api_mode") != "instagram_login") {
		return errMetaMessengerSubscriptionFence
	}
	if stringConfigValue(account.Metadata, metaDeauthorizationPendingDigestKey) != digest ||
		stringConfigValue(account.Metadata, metaDeauthorizationPendingIssuedKey) != issuedAt {
		return errMetaMessengerSubscriptionFence
	}
	if outcome == metaregistry.OwnershipVerified {
		var current int64
		if err := a.DB.Model(&models.ChannelCredential{}).
			Where("organization_id = ? AND channel_account_id = ? AND status IN ? AND ((id = ? AND version = ? AND kind = ?) OR (id = ? AND version = ? AND kind = ?))",
				fence.OrganizationID, fence.AccountID,
				[]models.ChannelCredentialStatus{models.ChannelCredentialStatusActive, models.ChannelCredentialStatusExpiring},
				fence.OAuthCredentialID, fence.OAuthVersion, models.ChannelCredentialKindOAuth,
				fence.WebhookCredentialID, fence.WebhookVersion, models.ChannelCredentialKindWebhook).
			Count(&current).Error; err != nil {
			return err
		}
		if current != 2 {
			return errMetaMessengerSubscriptionFence
		}
	}
	metadata := cloneJSONB(account.Metadata)
	delete(metadata, metaDeauthorizationPendingDigestKey)
	delete(metadata, metaDeauthorizationPendingIssuedKey)
	metadata[metaDeauthorizationResolvedDigestKey] = digest
	metadata[metaDeauthorizationResolvedAtKey] = checkedAt.UTC().Format(time.RFC3339Nano)
	updates := map[string]any{"metadata": metadata, "updated_at": checkedAt.UTC()}
	if outcome == metaregistry.OwnershipRevoked {
		metadata[metaDeauthorizationResolvedStateKey] = "authorization_revoked"
		metadata[metaDeauthorizationEventDigestKey] = digest
		metadata[metaDeauthorizationEventIssuedAtKey] = issuedAt
	} else {
		metadata[metaDeauthorizationResolvedStateKey] = "current_authorization_verified"
		metadata["meta_activation_state"] = "awaiting_health"
		for _, key := range []string{
			"meta_health_checked_at",
			"meta_health_oauth_credential_id",
			"meta_health_oauth_version",
			"meta_health_webhook_credential_id",
			"meta_health_webhook_version",
		} {
			delete(metadata, key)
		}
		updates["last_health_check_at"] = nil
		updates["last_error"] = ""
		updates["last_error_at"] = nil
		updates["connected_at"] = nil
	}
	result := a.DB.Model(&models.ChannelAccount{}).
		Where("id = ? AND organization_id = ?", account.ID, fence.OrganizationID).
		Updates(updates)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return errMetaMessengerSubscriptionFence
	}
	return nil
}

func (a *App) loadMetaMessengerRevalidationSnapshot(
	organizationID, accountID uuid.UUID, now time.Time,
) (metaMessengerRevalidationSnapshot, error) {
	var snapshot metaMessengerRevalidationSnapshot
	snapshot.OrganizationID = organizationID
	err := database.WithTenantReadCommitted(a.rootApp().DB, organizationID, func(tx *gorm.DB) error {
		if err := tx.Preload("Credentials", func(db *gorm.DB) *gorm.DB { return db.Order("version DESC") }).
			Where("id = ? AND organization_id = ? AND channel = ? AND provider = ?",
				accountID, organizationID, models.ChannelMessenger, channelapi.RelayProvider).
			First(&snapshot.Account).Error; err != nil {
			return err
		}
		oauth := latestActiveMetaCredential(snapshot.Account.Credentials, models.ChannelCredentialKindOAuth)
		webhook := latestActiveMetaCredential(snapshot.Account.Credentials, models.ChannelCredentialKindWebhook)
		if oauth == nil || webhook == nil {
			return metaregistry.ErrNotFound
		}
		snapshot.OAuth, snapshot.Webhook = *oauth, *webhook
		var err error
		snapshot.PageToken, err = decryptRequiredMetaRegistrySecret(oauth.CredentialBlob, "access_token", a.Config.App.EncryptionKey)
		if err != nil {
			return err
		}
		snapshot.AuthorityToken, err = decryptRequiredMetaRegistrySecret(oauth.CredentialBlob, "authority_token", a.Config.App.EncryptionKey)
		if err != nil {
			return err
		}
		snapshot.PreviousCheck, err = time.Parse(time.RFC3339Nano, stringConfigValue(snapshot.Account.Metadata, "meta_ownership_checked_at"))
		return err
	})
	return snapshot, err
}

func latestActiveMetaCredential(
	credentials []models.ChannelCredential, kind models.ChannelCredentialKind,
) *models.ChannelCredential {
	for index := range credentials {
		credential := &credentials[index]
		if credential.Kind == kind && (credential.Status == models.ChannelCredentialStatusActive ||
			credential.Status == models.ChannelCredentialStatusExpiring) {
			return credential
		}
	}
	return nil
}

func (a *App) checkMetaMessengerOwnership(
	ctx context.Context, snapshot metaMessengerRevalidationSnapshot,
) (outcome, reason string) {
	if !metaMessengerAuthorizationTokenAllowed(a.Config, snapshot.Account.Metadata) {
		return metaregistry.OwnershipStale, "authorization_token_kind_not_allowed"
	}
	if snapshot.OAuth.ExpiresAt != nil && !snapshot.OAuth.ExpiresAt.After(time.Now().UTC()) {
		return metaregistry.OwnershipRevoked, "oauth_credential_expired"
	}
	inspection, err := a.inspectMetaMessengerToken(ctx, snapshot.AuthorityToken, true)
	if err != nil {
		return classifyMetaMessengerRevalidationError(err)
	}
	platform, err := a.fetchMetaMessengerPlatformIdentity(ctx, snapshot.AuthorityToken, inspection)
	if err != nil {
		return classifyMetaMessengerRevalidationError(err)
	}
	if platform.UserID != stringConfigValue(snapshot.Account.Metadata, "meta_authorizing_user_id") {
		return metaregistry.OwnershipRevoked, "authorizing_identity_changed"
	}
	businessID := stringConfigValue(snapshot.Account.Metadata, "meta_business_id")
	if platform.TokenKind == metaMessengerTokenKindSystemUser && platform.ClientBusinessID != businessID {
		return metaregistry.OwnershipRevoked, "authorizing_business_changed"
	}
	selected := metaMessengerStoredPage{
		metaMessengerPageSummary: metaMessengerPageSummary{
			BusinessID: businessID, PageID: snapshot.Account.ExternalAccountID,
			Ownership: metaMessengerOwnershipOwned, Selectable: true, Tasks: []string{"MESSAGING"},
		},
		OwnershipVerifiedAt: inspection.CheckedAt,
	}
	fresh, err := a.revalidateMetaMessengerOwnedPage(
		ctx, snapshot.OrganizationID, snapshot.AuthorityToken, inspection, selected,
	)
	if err != nil {
		if errors.Is(err, errMetaMessengerSelectionInvalid) {
			return metaregistry.OwnershipRevoked, "page_ownership_or_task_removed"
		}
		return classifyMetaMessengerRevalidationError(err)
	}
	freshPageToken, err := appcrypto.Decrypt(fresh.EncryptedPageToken, a.Config.App.EncryptionKey)
	if err != nil || strings.TrimSpace(freshPageToken) == "" {
		return metaregistry.OwnershipStale, "fresh_page_token_unavailable"
	}
	if !metaMessengerOpaqueValuesEqual(strings.TrimSpace(freshPageToken), strings.TrimSpace(snapshot.PageToken)) {
		return metaregistry.OwnershipStale, "page_token_rotation_requires_reconnect"
	}
	if _, _, err := a.bindMetaMessengerPageToken(
		ctx,
		snapshot.Account.ExternalAccountID,
		snapshot.PageToken,
		inspection,
	); err != nil {
		return classifyMetaMessengerRevalidationError(err)
	}
	subscribed, err := a.metaMessengerPageHasConfiguredAppSubscription(ctx, snapshot.Account.ExternalAccountID, snapshot.PageToken)
	if err != nil {
		return classifyMetaMessengerRevalidationError(err)
	}
	if !subscribed {
		return metaregistry.OwnershipStale, "messages_subscription_missing"
	}
	return "", "scheduled_graph_revalidation"
}

func classifyMetaMessengerRevalidationError(err error) (outcome, reason string) {
	var provider *metaMessengerProviderError
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
