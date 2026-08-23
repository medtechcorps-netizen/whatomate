package handlers

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/audit"
	channelapi "github.com/shridarpatil/whatomate/internal/channel"
	"github.com/shridarpatil/whatomate/internal/metaregistry"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/valyala/fasthttp"
	"github.com/zerodha/fastglue"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type approveMetaInstagramRequest struct {
	Approve bool `json:"approve"`
}

type metaInstagramSubscriptionApproval struct {
	ProfileID                string
	CheckedAt                time.Time
	OAuthCredentialID        uuid.UUID
	OAuthCredentialVersion   int
	WebhookCredentialID      uuid.UUID
	WebhookCredentialVersion int
}

// ApproveMetaInstagramActivation is the only pending-to-active transition for
// a managed Instagram Login binding. App Review/development-role approval is
// deployment-owned; this endpoint only accepts an already healthy binding.
func (a *App) ApproveMetaInstagramActivation(r *fastglue.Request) error {
	orgID, userID, _, err := a.requireMetaInstagramOnboardingAuth(r, models.ActionWrite, true, false)
	if err != nil {
		return nil
	}
	if allowed, err := a.requireMetaInstagramRateLimit(r, orgID, userID, "approve", 12, time.Minute); !allowed {
		return err
	}
	accountID, err := parsePathUUID(r, "id", "channel account")
	if err != nil {
		return nil
	}
	var request approveMetaInstagramRequest
	if err := decodeStrictMetaMessengerRequest(r, &request); err != nil {
		return nil
	}
	if !request.Approve {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "approve must be true", nil, "")
	}

	now := time.Now().UTC()
	snapshot, err := a.loadMetaInstagramRevalidationSnapshot(orgID, accountID, now)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusConflict, "Run a fresh successful health test before approving this Instagram account", nil, "")
	}
	providerCtx, cancel := context.WithTimeout(requestContext(r), metaInstagramProviderOperationLimit)
	evidence, providerErr := a.freshMetaInstagramSubscriptionApproval(providerCtx, snapshot)
	cancel()
	if providerErr != nil {
		return r.SendErrorEnvelope(fasthttp.StatusConflict, "Meta no longer reports the required Instagram messages subscription; reconnect and test again", nil, "")
	}
	activated, err := a.activateMetaInstagramAccount(orgID, userID, accountID, time.Now().UTC(), evidence)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusConflict, "Run a fresh successful health test before approving this Instagram account", nil, "")
	}
	return r.SendEnvelope(map[string]any{
		"activated": true,
		"account":   a.channelAccountToResponse(&activated),
	})
}

func (a *App) freshMetaInstagramSubscriptionApproval(
	ctx context.Context,
	snapshot metaInstagramRevalidationSnapshot,
) (metaInstagramSubscriptionApproval, error) {
	settings, err := a.metaInstagramOnboardingSettings()
	if err != nil || !metaAuthorizationTokenAllowed(a.Config, &snapshot.Account) {
		return metaInstagramSubscriptionApproval{}, errors.New("managed Instagram release evidence is unavailable")
	}
	var approval metaInstagramSubscriptionApproval
	err = a.withLockedMetaInstagramProviderAttempt(
		ctx, snapshot, false, true,
		func(locked metaInstagramRevalidationSnapshot) error {
			subscribed, providerErr := a.metaInstagramHasConfiguredAppSubscription(
				ctx, settings, locked.Account.ExternalAccountID, locked.AccessToken,
			)
			if providerErr != nil {
				return providerErr
			}
			if !subscribed {
				return errors.New("required Instagram messages subscription is missing")
			}
			approval = metaInstagramSubscriptionApproval{
				ProfileID:                locked.Account.ExternalAccountID,
				CheckedAt:                time.Now().UTC(),
				OAuthCredentialID:        locked.OAuth.ID,
				OAuthCredentialVersion:   locked.OAuth.Version,
				WebhookCredentialID:      locked.Webhook.ID,
				WebhookCredentialVersion: locked.Webhook.Version,
			}
			return nil
		},
	)
	if err != nil {
		return metaInstagramSubscriptionApproval{}, err
	}
	return approval, nil
}

func (a *App) activateMetaInstagramAccount(
	orgID, userID, accountID uuid.UUID,
	now time.Time,
	subscription metaInstagramSubscriptionApproval,
) (models.ChannelAccount, error) {
	var activated models.ChannelAccount
	err := a.WithCommittedTenantApp(orgID, func(scoped *App) error {
		if err := lockChannelAIOrganizationScopeTx(scoped.DB, orgID); err != nil {
			return err
		}
		var account models.ChannelAccount
		if err := scoped.DB.Clauses(clause.Locking{Strength: "UPDATE"}).
			Preload("Credentials", func(db *gorm.DB) *gorm.DB { return db.Order("version DESC") }).
			Where("id = ? AND organization_id = ? AND channel = ? AND provider = ?",
				accountID, orgID, models.ChannelInstagram, channelapi.RelayProvider).
			First(&account).Error; err != nil {
			return err
		}
		if account.Status != models.ChannelAccountStatusPending ||
			!exactMetaRegistryControlPlaneConfig(account.Config) ||
			stringConfigValue(account.Config, "instagram_api_mode") != "instagram_login" ||
			scoped.metaInstagramReleaseGuardReason(account, orgID) != "" ||
			stringConfigValue(account.Metadata, "meta_ownership_state") != metaregistry.OwnershipVerified ||
			stringConfigValue(account.Metadata, "meta_subscription_state") != "verified" ||
			stringConfigValue(account.Metadata, "meta_activation_state") != "awaiting_admin_approval" ||
			account.LastHealthCheckAt == nil || account.LastError != "" {
			return errors.New("managed Instagram account is not ready for approval")
		}
		healthCheckedAt, err := time.Parse(time.RFC3339Nano, stringConfigValue(account.Metadata, "meta_health_checked_at"))
		if err != nil || healthCheckedAt.After(now.Add(time.Minute)) ||
			now.Sub(healthCheckedAt) > time.Duration(a.Config.MetaInstagram.HealthApprovalMaxAgeMins)*time.Minute ||
			!channelHealthTimestampsMatch(*account.LastHealthCheckAt, healthCheckedAt) {
			return errors.New("managed Instagram health approval is stale")
		}
		ownershipCheckedAt, err := time.Parse(time.RFC3339Nano, stringConfigValue(account.Metadata, "meta_ownership_checked_at"))
		if err != nil || ownershipCheckedAt.After(now.Add(time.Minute)) ||
			now.Sub(ownershipCheckedAt) > time.Duration(a.Config.MetaRegistry.OwnershipMaxAgeMins)*time.Minute {
			return errors.New("managed Instagram ownership approval is stale")
		}
		oauth := currentMetaRegistryCredential(account.Credentials, models.ChannelCredentialKindOAuth, now)
		webhook := currentMetaRegistryCredential(account.Credentials, models.ChannelCredentialKindWebhook, now)
		if !metaInstagramCredentialPairGenerationValid(oauth, webhook, now) ||
			!metaInstagramSubscribedOperationMatchesCredentials(account.Metadata, *oauth, *webhook) ||
			stringConfigValue(account.Metadata, "meta_health_oauth_credential_id") != oauth.ID.String() ||
			intConfigValue(account.Metadata, "meta_health_oauth_version") != oauth.Version ||
			stringConfigValue(account.Metadata, "meta_health_webhook_credential_id") != webhook.ID.String() ||
			intConfigValue(account.Metadata, "meta_health_webhook_version") != webhook.Version {
			return errors.New("managed Instagram health approval was superseded")
		}
		if subscription.ProfileID != account.ExternalAccountID || subscription.CheckedAt.IsZero() ||
			subscription.CheckedAt.After(now.Add(time.Minute)) || now.Sub(subscription.CheckedAt) > time.Minute ||
			subscription.OAuthCredentialID != oauth.ID || subscription.OAuthCredentialVersion != oauth.Version ||
			subscription.WebhookCredentialID != webhook.ID || subscription.WebhookCredentialVersion != webhook.Version {
			return errors.New("managed Instagram subscription approval was superseded")
		}
		allowed, entitlementErr := scoped.HasProductEntitlement(
			uuid.Nil, orgID, channelapi.OmnichannelEntitlementKey,
		)
		if entitlementErr != nil {
			return entitlementErr
		}
		if !allowed {
			return errMetaInstagramEntitlementDenied
		}

		oldAccount := account
		oldAccount.Config = cloneJSONB(account.Config)
		oldAccount.Metadata = cloneJSONB(account.Metadata)
		accountConfig := cloneJSONB(account.Config)
		accountConfig["outbound_enabled"] = true
		accountConfig["ai_reply_enabled"] = false
		metadata := cloneJSONB(account.Metadata)
		metadata["meta_activation_state"] = "active"
		metadata["meta_activated_at"] = now.Format(time.RFC3339Nano)
		metadata["meta_activated_by"] = userID.String()
		metadata["meta_activation_subscription_checked_at"] = subscription.CheckedAt.Format(time.RFC3339Nano)
		metadata["meta_activation_subscription_oauth_credential_id"] = oauth.ID.String()
		metadata["meta_activation_subscription_oauth_version"] = oauth.Version
		metadata["meta_activation_subscription_webhook_credential_id"] = webhook.ID.String()
		metadata["meta_activation_subscription_webhook_version"] = webhook.Version
		account.Status = models.ChannelAccountStatusActive
		account.Config = accountConfig
		account.Metadata = metadata
		account.ConnectedAt = &now
		account.UpdatedByID = &userID
		result := scoped.DB.Model(&models.ChannelAccount{}).
			Where("id = ? AND organization_id = ? AND status = ?", account.ID, orgID, models.ChannelAccountStatusPending).
			Updates(map[string]any{
				"status": account.Status, "config": accountConfig, "metadata": metadata,
				"connected_at": now, "updated_by_id": userID, "updated_at": now,
			})
		if result.Error != nil || result.RowsAffected != 1 {
			if result.Error != nil {
				return result.Error
			}
			return errors.New("managed Instagram activation was superseded")
		}
		if err := audit.LogAudit(
			scoped.DB, orgID, userID, audit.GetUserName(scoped.DB, userID),
			"meta_channel_registry", account.ID, models.AuditActionUpdated,
			&oldAccount, &account,
			map[string]any{"field": "activation", "old_value": "pending", "new_value": "active"},
		); err != nil {
			return err
		}
		activated = account
		return nil
	})
	if err != nil {
		return models.ChannelAccount{}, err
	}
	return activated, nil
}
