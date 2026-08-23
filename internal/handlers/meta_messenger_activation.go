package handlers

import (
	"context"
	"errors"
	"strings"
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

type approveMetaMessengerRequest struct {
	Approve bool `json:"approve"`
}

type metaMessengerSubscriptionApproval struct {
	PageID                     string
	CheckedAt                  time.Time
	BusinessAuthorityCheckedAt time.Time
	OAuthCredentialID          uuid.UUID
	OAuthCredentialVersion     int
	WebhookCredentialID        uuid.UUID
	WebhookCredentialVersion   int
}

// ApproveMetaMessengerActivation is the only pending-to-active transition for
// platform-managed Messenger accounts. A successful relay/Graph health result
// is necessary but never activates an account by itself.
func (a *App) ApproveMetaMessengerActivation(r *fastglue.Request) error {
	orgID, userID, _, err := a.requireMetaMessengerOnboardingAuth(r, models.ActionWrite, true)
	if err != nil {
		return nil
	}
	if allowed, err := a.requireMetaMessengerRateLimit(r, orgID, userID, "approve", 12, time.Minute); !allowed {
		return err
	}
	accountID, err := parsePathUUID(r, "id", "channel account")
	if err != nil {
		return nil
	}
	var request approveMetaMessengerRequest
	if err := decodeStrictMetaMessengerRequest(r, &request); err != nil {
		return nil
	}
	if !request.Approve {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "approve must be true", nil, "")
	}
	now := time.Now().UTC()
	snapshot, err := a.loadMetaMessengerRevalidationSnapshot(orgID, accountID, now)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusConflict, "Run a fresh successful health test before approving this Messenger account", nil, "")
	}
	providerCtx, cancel := context.WithTimeout(requestContext(r), metaMessengerProviderOperationLimit)
	evidence, subscriptionErr := a.freshMetaMessengerSubscriptionApproval(providerCtx, snapshot)
	cancel()
	if subscriptionErr != nil {
		return r.SendErrorEnvelope(fasthttp.StatusConflict, "Meta no longer reports the required Page messages subscription; reconnect and test again", nil, "")
	}
	now = time.Now().UTC()
	activated, err := a.activateMetaMessengerAccount(orgID, userID, accountID, now, evidence)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusConflict, "Run a fresh successful health test before approving this Messenger account", nil, "")
	}
	return r.SendEnvelope(map[string]any{
		"activated": true,
		"account":   a.channelAccountToResponse(&activated),
	})
}

func (a *App) freshMetaMessengerSubscriptionApproval(
	ctx context.Context,
	snapshot metaMessengerRevalidationSnapshot,
) (metaMessengerSubscriptionApproval, error) {
	businessAuthorityCheckedAt := time.Time{}
	usesExactBusinessAuthority := metaregistry.MessengerUsesExactSystemUserBusinessAuthority(
		snapshot.Account.Metadata,
	)
	tokenKind := strings.ToUpper(strings.TrimSpace(stringConfigValue(
		snapshot.Account.Metadata,
		metaMessengerAuthorizationTokenKindKey,
	)))
	if tokenKind == metaMessengerTokenKindSystemUser {
		if !metaregistry.MessengerBusinessAuthorityCurrent(
			snapshot.Account.Metadata,
			snapshot.Account.ExternalAccountID,
			snapshot.OAuth.ID,
			snapshot.OAuth.Version,
		) {
			return metaMessengerSubscriptionApproval{}, errors.New("stored Messenger business authority is invalid")
		}
		outcome, _, businessAuthorityVerified := a.checkMetaMessengerOwnership(ctx, snapshot)
		if outcome != "" || !businessAuthorityVerified {
			return metaMessengerSubscriptionApproval{}, errors.New("messenger business authority is no longer current")
		}
		if usesExactBusinessAuthority {
			businessAuthorityCheckedAt = time.Now().UTC()
		}
	}
	subscribed, err := a.metaMessengerPageHasConfiguredAppSubscription(
		ctx,
		snapshot.Account.ExternalAccountID,
		snapshot.PageToken,
	)
	if err != nil {
		return metaMessengerSubscriptionApproval{}, err
	}
	if !subscribed {
		return metaMessengerSubscriptionApproval{}, errors.New("required Page messages subscription is missing")
	}
	return metaMessengerSubscriptionApproval{
		PageID: snapshot.Account.ExternalAccountID, CheckedAt: time.Now().UTC(),
		BusinessAuthorityCheckedAt: businessAuthorityCheckedAt,
		OAuthCredentialID:          snapshot.OAuth.ID, OAuthCredentialVersion: snapshot.OAuth.Version,
		WebhookCredentialID: snapshot.Webhook.ID, WebhookCredentialVersion: snapshot.Webhook.Version,
	}, nil
}

func (a *App) activateMetaMessengerAccount(
	orgID, userID, accountID uuid.UUID, now time.Time,
	subscription metaMessengerSubscriptionApproval,
) (models.ChannelAccount, error) {
	var activated models.ChannelAccount
	err := a.WithCommittedTenantApp(orgID, func(scoped *App) error {
		var account models.ChannelAccount
		if err := scoped.DB.Clauses(clause.Locking{Strength: "UPDATE"}).
			Preload("Credentials", func(db *gorm.DB) *gorm.DB { return db.Order("version DESC") }).
			Where("id = ? AND organization_id = ? AND channel = ? AND provider = ?",
				accountID, orgID, models.ChannelMessenger, channelapi.RelayProvider).
			First(&account).Error; err != nil {
			return err
		}
		if account.Status != models.ChannelAccountStatusPending ||
			!metaRegistryControlPlaneConfig(account.Config) ||
			!metaMessengerAuthorizationTokenAllowed(a.Config, account.Metadata) ||
			stringConfigValue(account.Metadata, "meta_platform_app_id") != a.Config.MetaMessenger.AppID ||
			stringConfigValue(account.Metadata, "meta_ownership_state") != metaregistry.OwnershipVerified ||
			stringConfigValue(account.Metadata, "meta_subscription_state") != "verified" ||
			stringConfigValue(account.Metadata, "meta_activation_state") != "awaiting_admin_approval" ||
			account.LastHealthCheckAt == nil || account.LastError != "" {
			return errors.New("managed Messenger account is not ready for approval")
		}
		healthCheckedAt, err := time.Parse(time.RFC3339Nano, stringConfigValue(account.Metadata, "meta_health_checked_at"))
		if err != nil || healthCheckedAt.After(now.Add(time.Minute)) ||
			now.Sub(healthCheckedAt) > time.Duration(a.Config.MetaMessenger.HealthApprovalMaxAgeMins)*time.Minute ||
			!channelHealthTimestampsMatch(*account.LastHealthCheckAt, healthCheckedAt) {
			return errors.New("managed Messenger health approval is stale")
		}
		ownershipCheckedAt, err := time.Parse(time.RFC3339Nano, stringConfigValue(account.Metadata, "meta_ownership_checked_at"))
		if err != nil || ownershipCheckedAt.After(now.Add(time.Minute)) ||
			now.Sub(ownershipCheckedAt) > time.Duration(a.Config.MetaRegistry.OwnershipMaxAgeMins)*time.Minute {
			return errors.New("managed Messenger ownership approval is stale")
		}
		oauth := currentMetaRegistryCredential(account.Credentials, models.ChannelCredentialKindOAuth, now)
		webhook := currentMetaRegistryCredential(account.Credentials, models.ChannelCredentialKindWebhook, now)
		if oauth == nil || webhook == nil ||
			stringConfigValue(account.Metadata, "meta_health_oauth_credential_id") != oauth.ID.String() ||
			intConfigValue(account.Metadata, "meta_health_oauth_version") != oauth.Version ||
			stringConfigValue(account.Metadata, "meta_health_webhook_credential_id") != webhook.ID.String() ||
			intConfigValue(account.Metadata, "meta_health_webhook_version") != webhook.Version {
			return errors.New("managed Messenger health approval was superseded")
		}
		if !metaregistry.MessengerBusinessAuthorityCurrent(
			account.Metadata,
			account.ExternalAccountID,
			oauth.ID,
			oauth.Version,
		) {
			return errors.New("managed Messenger business authority is invalid")
		}
		if metaregistry.MessengerUsesExactSystemUserBusinessAuthority(account.Metadata) &&
			(subscription.BusinessAuthorityCheckedAt.IsZero() ||
				subscription.BusinessAuthorityCheckedAt.After(now.Add(time.Minute)) ||
				now.Sub(subscription.BusinessAuthorityCheckedAt) > time.Minute) {
			return errors.New("managed Messenger business authority approval is stale")
		}
		if subscription.PageID != account.ExternalAccountID || subscription.CheckedAt.IsZero() ||
			subscription.CheckedAt.After(now.Add(time.Minute)) || now.Sub(subscription.CheckedAt) > time.Minute ||
			subscription.OAuthCredentialID != oauth.ID || subscription.OAuthCredentialVersion != oauth.Version ||
			subscription.WebhookCredentialID != webhook.ID || subscription.WebhookCredentialVersion != webhook.Version {
			return errors.New("managed Messenger subscription approval was superseded")
		}
		oldAccount := account
		oldAccount.Config = cloneJSONB(account.Config)
		oldAccount.Metadata = cloneJSONB(account.Metadata)
		config := cloneJSONB(account.Config)
		config["outbound_enabled"] = true
		config["ai_reply_enabled"] = false
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
		account.Config = config
		account.Metadata = metadata
		account.ConnectedAt = &now
		account.UpdatedByID = &userID
		if result := scoped.DB.Model(&models.ChannelAccount{}).
			Where("id = ? AND organization_id = ? AND status = ?",
				account.ID, orgID, models.ChannelAccountStatusPending).
			Updates(map[string]any{
				"status": account.Status, "config": config, "metadata": metadata,
				"connected_at": now, "updated_by_id": userID, "updated_at": now,
			}); result.Error != nil || result.RowsAffected != 1 {
			if result.Error != nil {
				return result.Error
			}
			return errors.New("managed Messenger activation was superseded")
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

func intConfigValue(config models.JSONB, key string) int {
	switch value := config[key].(type) {
	case int:
		return value
	case int32:
		return int(value)
	case int64:
		return int(value)
	case float64:
		return int(value)
	default:
		return 0
	}
}
