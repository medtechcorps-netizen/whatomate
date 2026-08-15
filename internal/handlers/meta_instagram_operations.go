package handlers

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/audit"
	channelapi "github.com/shridarpatil/whatomate/internal/channel"
	appcrypto "github.com/shridarpatil/whatomate/internal/crypto"
	"github.com/shridarpatil/whatomate/internal/database"
	"github.com/shridarpatil/whatomate/internal/metaregistry"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/valyala/fasthttp"
	"github.com/zerodha/fastglue"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type metaInstagramRotateInput struct {
	AccountID                      uuid.UUID
	OrganizationID                 uuid.UUID
	UserID                         uuid.UUID
	Profile                        metaInstagramProfile
	Inspection                     metaInstagramTokenInspection
	AccessToken                    string
	AuthorizationStartedAt         time.Time
	TokenExpiresAt                 *time.Time
	SubscriptionOperationID        uuid.UUID
	SubscriptionOperationExpiresAt time.Time
}

func metaInstagramSubscribedOperation(metadata models.JSONB) (metaMessengerSubscriptionOperation, bool) {
	operation, ok := metaMessengerSubscriptionOperationFromMetadata(metadata)
	return operation, ok &&
		operation.DesiredState == metaMessengerSubscriptionDesiredSubscribed &&
		operation.State == metaMessengerSubscriptionSubscribeComplete &&
		stringConfigValue(metadata, metaMessengerSubscriptionRemoteStateKey) ==
			metaMessengerSubscriptionRemoteSubscribed
}

func metaInstagramSubscribedOperationMatchesCredentials(
	metadata models.JSONB,
	oauth models.ChannelCredential,
	webhook models.ChannelCredential,
) bool {
	operation, ok := metaInstagramSubscribedOperation(metadata)
	return ok && operation.OAuthCredentialID == oauth.ID &&
		operation.OAuthVersion == oauth.Version &&
		operation.WebhookCredentialID == webhook.ID &&
		operation.WebhookVersion == webhook.Version
}

// metaInstagramTeardownBindingReason is deliberately distinct from the normal
// release guard. App-review downgrade/quarantine must never permit new routing,
// but it must leave an already-bound profile able to unsubscribe. Teardown is
// allowed only for the exact deployment-owned app/profile and the exact current
// credential generation. An initial disconnect additionally has to prove that
// this pair is the pair whose subscription was durably confirmed.
func (a *App) metaInstagramTeardownBindingReason(
	account *models.ChannelAccount,
	organizationID uuid.UUID,
	now time.Time,
	claimedOperation *metaMessengerSubscriptionOperation,
	requireSubscribedOperation bool,
) string {
	if a == nil || a.Config == nil || account == nil ||
		!a.metaInstagramOrganizationAllowed(organizationID) ||
		account.OrganizationID != organizationID ||
		!exactManagedInstagramCallbackBinding(
			account,
			strings.TrimSpace(a.Config.MetaInstagram.AppID),
			stringConfigValue(account.Metadata, "meta_authorizing_user_id"),
		) {
		return "managed_instagram_teardown_binding_invalid"
	}
	oauth := currentMetaRegistryCredential(
		account.Credentials, models.ChannelCredentialKindOAuth, now,
	)
	webhook := currentMetaRegistryCredential(
		account.Credentials, models.ChannelCredentialKindWebhook, now,
	)
	currentCount := 0
	for index := range account.Credentials {
		if channelapi.CredentialIsCurrent(&account.Credentials[index], now) {
			currentCount++
		}
	}
	if currentCount != 2 ||
		!metaInstagramCredentialPairGenerationValid(oauth, webhook, now) {
		return "managed_instagram_teardown_credential_generation_invalid"
	}
	if claimedOperation != nil &&
		(claimedOperation.OAuthCredentialID != oauth.ID ||
			claimedOperation.OAuthVersion != oauth.Version ||
			claimedOperation.WebhookCredentialID != webhook.ID ||
			claimedOperation.WebhookVersion != webhook.Version) {
		return "managed_instagram_teardown_operation_generation_invalid"
	}
	if requireSubscribedOperation &&
		!metaInstagramSubscribedOperationMatchesCredentials(
			account.Metadata, *oauth, *webhook,
		) {
		return "managed_instagram_teardown_subscription_invalid"
	}
	return ""
}

func (a *App) metaInstagramSubscribeBindingReason(
	account *models.ChannelAccount,
	organizationID uuid.UUID,
	now time.Time,
	operation metaMessengerSubscriptionOperation,
) string {
	if account == nil || account.Status != models.ChannelAccountStatusPending ||
		stringConfigValue(account.Metadata, "meta_activation_state") !=
			metaMessengerAwaitingRegistryState {
		return "managed_instagram_subscribe_status_invalid"
	}
	if reason := a.metaInstagramReleaseGuardReason(*account, organizationID); reason != "" {
		return reason
	}
	if stringConfigValue(account.Metadata, "meta_ownership_state") != metaregistry.OwnershipVerified {
		return "managed_instagram_subscribe_ownership_invalid"
	}
	checkedAt, err := time.Parse(
		time.RFC3339Nano,
		stringConfigValue(account.Metadata, "meta_ownership_checked_at"),
	)
	if err != nil || checkedAt.IsZero() || checkedAt.After(now.Add(time.Minute)) {
		return "managed_instagram_subscribe_ownership_timestamp_invalid"
	}
	maxAge := time.Duration(a.Config.MetaRegistry.OwnershipMaxAgeMins) * time.Minute
	if maxAge <= 0 || maxAge > 7*24*time.Hour {
		maxAge = 24 * time.Hour
	}
	if now.Sub(checkedAt) > maxAge {
		return "managed_instagram_subscribe_ownership_stale"
	}
	oauth := currentMetaRegistryCredential(
		account.Credentials, models.ChannelCredentialKindOAuth, now,
	)
	webhook := currentMetaRegistryCredential(
		account.Credentials, models.ChannelCredentialKindWebhook, now,
	)
	if !metaInstagramCredentialPairGenerationValid(oauth, webhook, now) ||
		oauth.ID != operation.OAuthCredentialID || oauth.Version != operation.OAuthVersion ||
		webhook.ID != operation.WebhookCredentialID || webhook.Version != operation.WebhookVersion {
		return "managed_instagram_subscribe_generation_invalid"
	}
	return ""
}

func (a *App) rotateMetaInstagramBinding(
	input metaInstagramRotateInput,
) (metaRegistryProvisionResult, error) {
	if a == nil || a.DB == nil || a.Config == nil ||
		input.OrganizationID != a.tenantOrgID || input.AccountID == uuid.Nil ||
		input.UserID == uuid.Nil || strings.TrimSpace(input.AccessToken) == "" {
		return metaRegistryProvisionResult{}, metaregistry.ErrInvalidRequest
	}
	oauthSubjectID := input.Profile.oauthSubjectID()
	professionalAccountID := input.Profile.professionalAccountID()
	if !validCanonicalMetaID(oauthSubjectID) || !validCanonicalMetaID(professionalAccountID) ||
		input.Inspection.UserID != oauthSubjectID ||
		input.Inspection.AppID != strings.TrimSpace(a.Config.MetaInstagram.AppID) ||
		!metaInstagramOAuthAuthorizationStartValid(input.AuthorizationStartedAt, time.Now().UTC()) {
		return metaRegistryProvisionResult{}, metaregistry.ErrInvalidRequest
	}
	if err := lockChannelAIOrganizationScopeTx(a.DB, input.OrganizationID); err != nil {
		return metaRegistryProvisionResult{}, err
	}
	releaseMode, allowed := a.metaInstagramReleaseMode(
		input.OrganizationID, oauthSubjectID, professionalAccountID,
	)
	if !allowed {
		return metaRegistryProvisionResult{}, metaregistry.ErrInvalidRequest
	}
	var account models.ChannelAccount
	if err := a.DB.Clauses(clause.Locking{Strength: "UPDATE"}).Where(
		"id = ? AND organization_id = ? AND channel = ? AND provider = ?",
		input.AccountID, input.OrganizationID, models.ChannelInstagram, channelapi.RelayProvider,
	).First(&account).Error; err != nil {
		return metaRegistryProvisionResult{}, err
	}
	if !exactMetaRegistryControlPlaneConfig(account.Config) ||
		stringConfigValue(account.Config, "instagram_api_mode") != "instagram_login" ||
		account.ExternalAccountID != professionalAccountID ||
		stringConfigValue(account.Metadata, "meta_platform_app_id") != strings.TrimSpace(a.Config.MetaInstagram.AppID) ||
		!metaMessengerSubscriptionOperationAvailable(account.Metadata, time.Now().UTC()) {
		return metaRegistryProvisionResult{}, errMetaMessengerSubscriptionFence
	}
	if err := a.metaInstagramOAuthGenerationFence(
		input.OrganizationID,
		strings.TrimSpace(a.Config.MetaInstagram.AppID),
		oauthSubjectID,
		input.AuthorizationStartedAt,
		time.Time{},
		account.Metadata,
	); err != nil {
		return metaRegistryProvisionResult{}, err
	}
	now := time.Now().UTC()
	if input.Inspection.CheckedAt.IsZero() || input.Inspection.CheckedAt.Before(now.Add(-10*time.Minute)) ||
		input.Inspection.CheckedAt.After(now.Add(time.Minute)) || input.TokenExpiresAt == nil ||
		!input.TokenExpiresAt.After(now.Add(metaInstagramProviderOperationLimit)) {
		return metaRegistryProvisionResult{}, metaregistry.ErrInvalidRequest
	}
	deletionDigest, deletionIssuedAt, deletionPending, err :=
		parsePendingMetaInstagramDeletionFence(account.Metadata)
	if err != nil {
		return metaRegistryProvisionResult{}, err
	}
	deauthorizationDigest, deauthorizationIssuedAt, deauthorizationPending, err :=
		parsePendingMetaDeauthorizationFence(account.Metadata)
	if err != nil {
		return metaRegistryProvisionResult{}, err
	}
	if deletionPending || deauthorizationPending {
		startedSecond := input.AuthorizationStartedAt.UTC().Truncate(time.Second)
		checkedSecond := input.Inspection.CheckedAt.UTC().Truncate(time.Second)
		if input.AuthorizationStartedAt.IsZero() {
			return metaRegistryProvisionResult{}, errMetaMessengerSubscriptionFence
		}
		for _, issuedAt := range []time.Time{deletionIssuedAt, deauthorizationIssuedAt} {
			if issuedAt.IsZero() {
				continue
			}
			issuedSecond := issuedAt.UTC().Truncate(time.Second)
			if !startedSecond.After(issuedSecond) || !checkedSecond.After(issuedSecond) {
				return metaRegistryProvisionResult{}, errMetaMessengerSubscriptionFence
			}
		}
	}
	if input.SubscriptionOperationID == uuid.Nil {
		input.SubscriptionOperationID = uuid.New()
	}
	if input.SubscriptionOperationExpiresAt.IsZero() {
		input.SubscriptionOperationExpiresAt = now.Add(metaMessengerSubscriptionOperationLease)
	}
	if !input.SubscriptionOperationExpiresAt.After(now.Add(metaInstagramProviderOperationLimit)) {
		return metaRegistryProvisionResult{}, metaregistry.ErrInvalidRequest
	}
	var credentials []models.ChannelCredential
	if err := a.DB.Clauses(clause.Locking{Strength: "UPDATE"}).Where(
		"organization_id = ? AND channel_account_id = ?",
		input.OrganizationID, input.AccountID,
	).Order("version DESC").Find(&credentials).Error; err != nil {
		return metaRegistryProvisionResult{}, err
	}
	encrypt := func(value string) (string, error) {
		ciphertext, err := appcrypto.Encrypt(value, a.Config.App.EncryptionKey)
		if err != nil || !appcrypto.IsEncrypted(ciphertext) {
			return "", errors.New("meta registry credential encryption failed")
		}
		return ciphertext, nil
	}
	encryptedToken, err := encrypt(strings.TrimSpace(input.AccessToken))
	if err != nil {
		return metaRegistryProvisionResult{}, err
	}
	nextOAuthVersion, nextWebhookVersion := 1, 1
	var webhook *models.ChannelCredential
	for index := range credentials {
		credential := &credentials[index]
		switch credential.Kind {
		case models.ChannelCredentialKindOAuth:
			if credential.Version >= nextOAuthVersion {
				nextOAuthVersion = credential.Version + 1
			}
		case models.ChannelCredentialKindWebhook:
			if credential.Version >= nextWebhookVersion {
				nextWebhookVersion = credential.Version + 1
			}
			if webhook == nil && channelapi.CredentialIsCurrent(credential, now) {
				copy := *credential
				webhook = &copy
			}
		}
	}
	oauth := models.ChannelCredential{
		BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: input.OrganizationID,
		ChannelAccountID: account.ID, Kind: models.ChannelCredentialKindOAuth,
		Version: nextOAuthVersion,
		CredentialBlob: models.JSONB{
			"access_token":    encryptedToken,
			"authority_token": encryptedToken,
		},
		Status: models.ChannelCredentialStatusActive, KeyVersion: "app:v1",
		ExpiresAt: input.TokenExpiresAt, LastValidatedAt: &input.Inspection.CheckedAt,
		Metadata: models.JSONB{"refresh_mode": "instagram_long_lived"},
	}
	if err := a.DB.Create(&oauth).Error; err != nil {
		return metaRegistryProvisionResult{}, err
	}
	if err := a.metaInstagramOAuthGenerationFence(
		input.OrganizationID,
		strings.TrimSpace(a.Config.MetaInstagram.AppID),
		oauthSubjectID,
		input.AuthorizationStartedAt,
		oauth.CreatedAt,
		account.Metadata,
	); err != nil {
		return metaRegistryProvisionResult{}, err
	}
	if deletionPending &&
		!oauth.CreatedAt.UTC().Truncate(time.Second).After(deletionIssuedAt.UTC().Truncate(time.Second)) {
		return metaRegistryProvisionResult{}, errMetaMessengerSubscriptionFence
	}
	if deauthorizationPending &&
		!oauth.CreatedAt.UTC().Truncate(time.Second).After(deauthorizationIssuedAt.UTC().Truncate(time.Second)) {
		return metaRegistryProvisionResult{}, errMetaMessengerSubscriptionFence
	}
	if result := a.DB.Model(&models.ChannelCredential{}).Where(
		"organization_id = ? AND channel_account_id = ? AND kind = ? AND id <> ? AND status IN ?",
		input.OrganizationID, account.ID, models.ChannelCredentialKindOAuth, oauth.ID,
		[]models.ChannelCredentialStatus{
			models.ChannelCredentialStatusActive,
			models.ChannelCredentialStatusExpiring,
		},
	).Updates(map[string]any{"status": models.ChannelCredentialStatusRevoked, "revoked_at": now}); result.Error != nil {
		return metaRegistryProvisionResult{}, result.Error
	}
	if err := cancelManagedMetaQueuedWorkForAccountTx(
		a.DB, input.OrganizationID, account.ID, "managed_instagram_authorization_rotated",
	); err != nil {
		return metaRegistryProvisionResult{}, err
	}
	if webhook == nil {
		inbound, err := generateChannelSecret()
		if err != nil {
			return metaRegistryProvisionResult{}, err
		}
		outbound, err := generateChannelSecret()
		if err != nil {
			return metaRegistryProvisionResult{}, err
		}
		encryptedInbound, err := encrypt(inbound)
		if err != nil {
			return metaRegistryProvisionResult{}, err
		}
		encryptedOutbound, err := encrypt(outbound)
		if err != nil {
			return metaRegistryProvisionResult{}, err
		}
		created := models.ChannelCredential{
			BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: input.OrganizationID,
			ChannelAccountID: account.ID, Kind: models.ChannelCredentialKindWebhook,
			Version: nextWebhookVersion,
			CredentialBlob: models.JSONB{
				"inbound_secret": encryptedInbound, "outbound_secret": encryptedOutbound,
			},
			Status: models.ChannelCredentialStatusActive, KeyVersion: "app:v1", Metadata: models.JSONB{},
		}
		if err := a.DB.Create(&created).Error; err != nil {
			return metaRegistryProvisionResult{}, err
		}
		webhook = &created
	}
	operation := metaMessengerSubscriptionOperation{
		ID: input.SubscriptionOperationID, OAuthCredentialID: oauth.ID,
		OAuthVersion: oauth.Version, WebhookCredentialID: webhook.ID,
		WebhookVersion: webhook.Version,
		DesiredState:   metaMessengerSubscriptionDesiredSubscribed,
		State:          metaMessengerSubscriptionSubscribePending,
		ExpiresAt:      input.SubscriptionOperationExpiresAt.UTC(),
	}
	metadata := cloneJSONB(account.Metadata)
	metadata["meta_ownership_state"] = metaregistry.OwnershipVerified
	metadata["meta_ownership_checked_at"] = input.Inspection.CheckedAt.UTC().Format(time.RFC3339Nano)
	metadata["meta_authorizing_user_id"] = input.Inspection.UserID
	metadata[metaInstagramOAuthSubjectIDKey] = input.Inspection.UserID
	metadata[metaMessengerAuthorizationGrantedAtKey] = input.AuthorizationStartedAt.UTC().Format(time.RFC3339Nano)
	metadata["meta_granted_scopes"] = append([]string(nil), input.Inspection.Scopes...)
	metadata["meta_subscription_state"] = metaMessengerVerifyingSubscription
	metadata["meta_activation_state"] = metaMessengerAwaitingRegistryState
	metadata["meta_release_evidence_mode"] = releaseMode
	metadata["meta_release_review_status"] = strings.ToLower(strings.TrimSpace(a.Config.MetaInstagram.AppReviewStatus))
	delete(metadata, "meta_release_profile_id")
	delete(metadata, "meta_release_oauth_subject_id")
	delete(metadata, "meta_release_app_role")
	if releaseMode == "development_app_role" {
		metadata["meta_release_profile_id"] = professionalAccountID
		metadata["meta_release_oauth_subject_id"] = oauthSubjectID
		metadata["meta_release_app_role"] = strings.ToLower(strings.TrimSpace(a.Config.MetaInstagram.DevelopmentAppRole))
	}
	markMetaMessengerAuthorizationDurability(metadata, metaMessengerTokenKindUser, input.TokenExpiresAt)
	delete(metadata, "meta_deauthorized_at")
	if deletionPending {
		delete(metadata, metaInstagramDeletionPendingDigestKey)
		delete(metadata, metaInstagramDeletionPendingIssuedAtKey)
		metadata[metaInstagramDeletionResolvedDigestKey] = deletionDigest
		metadata[metaInstagramDeletionResolvedStateKey] = "newer_authorization_preserved"
		metadata[metaInstagramDeletionResolvedAtKey] = now.Format(time.RFC3339Nano)
	}
	if deauthorizationPending {
		delete(metadata, metaDeauthorizationPendingDigestKey)
		delete(metadata, metaDeauthorizationPendingIssuedKey)
		metadata[metaDeauthorizationResolvedDigestKey] = deauthorizationDigest
		metadata[metaDeauthorizationResolvedStateKey] = "newer_authorization_preserved"
		metadata[metaDeauthorizationResolvedAtKey] = now.Format(time.RFC3339Nano)
	}
	metadata = metadataWithMetaMessengerSubscriptionOperation(
		metadata, operation, metaMessengerSubscriptionRemoteUnknown,
	)
	config := cloneJSONB(account.Config)
	config["outbound_enabled"] = false
	config["ai_reply_enabled"] = false
	if result := a.DB.Model(&models.ChannelAccount{}).Where(
		"id = ? AND organization_id = ?", account.ID, input.OrganizationID,
	).Updates(map[string]any{
		"name":   metaInstagramAccountName(input.Profile.Username, professionalAccountID),
		"status": models.ChannelAccountStatusPending, "metadata": metadata, "config": config,
		"last_health_check_at": nil, "last_error": "", "last_error_at": nil,
		"connected_at": nil, "updated_by_id": input.UserID, "updated_at": now,
	}); result.Error != nil || result.RowsAffected != 1 {
		if result.Error != nil {
			return metaRegistryProvisionResult{}, result.Error
		}
		return metaRegistryProvisionResult{}, errMetaMessengerSubscriptionFence
	}
	account.Name = metaInstagramAccountName(input.Profile.Username, professionalAccountID)
	account.Status = models.ChannelAccountStatusPending
	account.Metadata = metadata
	account.Config = config
	account.LastHealthCheckAt = nil
	account.LastError = ""
	account.ConnectedAt = nil
	account.Credentials = []models.ChannelCredential{oauth, *webhook}
	if err := audit.LogAudit(
		a.DB, input.OrganizationID, input.UserID, audit.GetUserName(a.DB, input.UserID),
		"meta_channel_registry", account.ID, models.AuditActionUpdated,
		nil, map[string]any{"operation": "instagram_reconnect", "status": "pending"},
		map[string]any{"field": "credentials", "old_value": "********", "new_value": "********"},
	); err != nil {
		return metaRegistryProvisionResult{}, err
	}
	return metaRegistryProvisionResult{
		Account: account, OAuthCredentialID: oauth.ID, OAuthVersion: oauth.Version,
		WebhookCredentialID: webhook.ID, WebhookVersion: webhook.Version,
		SubscriptionOperation: operation,
	}, nil
}

func (a *App) lockMetaInstagramSubscriptionOperation(
	organizationID, accountID uuid.UUID,
	operation metaMessengerSubscriptionOperation,
	wantState string,
) (models.ChannelAccount, []models.ChannelCredential, error) {
	if a == nil || a.DB == nil || a.tenantOrgID != organizationID ||
		organizationID == uuid.Nil || accountID == uuid.Nil || operation.ID == uuid.Nil {
		return models.ChannelAccount{}, nil, metaregistry.ErrInvalidRequest
	}
	var account models.ChannelAccount
	if err := a.DB.Clauses(clause.Locking{Strength: "UPDATE"}).Where(
		"id = ? AND organization_id = ? AND channel = ? AND provider = ?",
		accountID, organizationID, models.ChannelInstagram, channelapi.RelayProvider,
	).First(&account).Error; err != nil {
		return models.ChannelAccount{}, nil, err
	}
	if !exactMetaRegistryControlPlaneConfig(account.Config) ||
		stringConfigValue(account.Config, "instagram_api_mode") != "instagram_login" ||
		stringConfigValue(account.Metadata, "meta_platform_app_id") != strings.TrimSpace(a.Config.MetaInstagram.AppID) ||
		!metaMessengerSubscriptionOperationMatches(account.Metadata, operation, wantState) {
		return models.ChannelAccount{}, nil, errMetaMessengerSubscriptionFence
	}
	if (wantState == metaMessengerSubscriptionSubscribePending &&
		operation.DesiredState != metaMessengerSubscriptionDesiredSubscribed) ||
		(wantState == metaMessengerSubscriptionUnsubscribePending &&
			operation.DesiredState != metaMessengerSubscriptionDesiredUnsubscribed) {
		return models.ChannelAccount{}, nil, errMetaMessengerSubscriptionFence
	}
	var credentials []models.ChannelCredential
	if err := a.DB.Clauses(clause.Locking{Strength: "UPDATE"}).Where(
		"organization_id = ? AND channel_account_id = ? AND status IN ?",
		organizationID, account.ID,
		[]models.ChannelCredentialStatus{
			models.ChannelCredentialStatusActive,
			models.ChannelCredentialStatusExpiring,
		},
	).Order("version DESC, id ASC").Find(&credentials).Error; err != nil {
		return models.ChannelAccount{}, nil, err
	}
	account.Credentials = credentials
	now := time.Now().UTC()
	switch operation.DesiredState {
	case metaMessengerSubscriptionDesiredSubscribed:
		if a.metaInstagramSubscribeBindingReason(
			&account, organizationID, now, operation,
		) != "" {
			return models.ChannelAccount{}, nil, errMetaMessengerSubscriptionFence
		}
	case metaMessengerSubscriptionDesiredUnsubscribed:
		if a.metaInstagramTeardownBindingReason(
			&account, organizationID, now, &operation, false,
		) != "" {
			return models.ChannelAccount{}, nil, errMetaMessengerSubscriptionFence
		}
	default:
		return models.ChannelAccount{}, nil, errMetaMessengerSubscriptionFence
	}
	return account, credentials, nil
}

func (a *App) withLockedMetaInstagramSubscriptionProviderAttempt(
	ctx context.Context,
	organizationID, accountID uuid.UUID,
	operation metaMessengerSubscriptionOperation,
	wantState string,
	attempt func(models.ChannelAccount, string) error,
) error {
	if attempt == nil || operation.ExpiresAt.Before(
		time.Now().UTC().Add(metaInstagramProviderOperationLimit),
	) {
		return errMetaMessengerSubscriptionFence
	}
	return database.WithTenantReadCommitted(
		a.rootApp().DB, organizationID, func(tx *gorm.DB) error {
			if err := lockChannelAIOrganizationScopeTx(tx, organizationID); err != nil {
				return err
			}
			scoped := a.rootApp().scopedApp(tx.WithContext(ctx), organizationID)
			account, credentials, err := scoped.lockMetaInstagramSubscriptionOperation(
				organizationID, accountID, operation, wantState,
			)
			if err != nil {
				return err
			}
			oauth := currentMetaRegistryCredential(
				credentials, models.ChannelCredentialKindOAuth, time.Now().UTC(),
			)
			if oauth == nil {
				return errMetaMessengerSubscriptionFence
			}
			if operation.DesiredState == metaMessengerSubscriptionDesiredSubscribed {
				authorizationStartedAt, parseErr := time.Parse(
					time.RFC3339Nano,
					stringConfigValue(account.Metadata, metaMessengerAuthorizationGrantedAtKey),
				)
				if parseErr != nil || scoped.metaInstagramOAuthGenerationFence(
					organizationID,
					strings.TrimSpace(a.Config.MetaInstagram.AppID),
					stringConfigValue(account.Metadata, "meta_authorizing_user_id"),
					authorizationStartedAt,
					oauth.CreatedAt,
					account.Metadata,
				) != nil {
					return errMetaMessengerSubscriptionFence
				}
			}
			accessToken, err := decryptRequiredMetaRegistrySecret(
				oauth.CredentialBlob, "access_token", a.Config.App.EncryptionKey,
			)
			if err != nil {
				return errMetaMessengerSubscriptionFence
			}
			return attempt(account, accessToken)
		},
	)
}

func (a *App) finalizeMetaInstagramSubscribeOperation(
	organizationID, userID, accountID uuid.UUID,
	operation metaMessengerSubscriptionOperation,
	confirmedAt time.Time,
) (metaRegistryProvisionResult, error) {
	account, credentials, err := a.lockMetaInstagramSubscriptionOperation(
		organizationID, accountID, operation, metaMessengerSubscriptionSubscribePending,
	)
	if err != nil {
		return metaRegistryProvisionResult{}, err
	}
	confirmedAt = confirmedAt.UTC()
	metadata := cloneJSONB(account.Metadata)
	metadata[metaMessengerSubscriptionOperationStateKey] = metaMessengerSubscriptionSubscribeComplete
	metadata[metaMessengerSubscriptionRemoteStateKey] = metaMessengerSubscriptionRemoteSubscribed
	metadata[metaMessengerSubscriptionRemoteConfirmedAtKey] = confirmedAt.Format(time.RFC3339Nano)
	metadata["meta_subscription_state"] = "verified"
	metadata["meta_activation_state"] = "awaiting_health"
	clearMetaMessengerSubscriptionFence(metadata)
	config := cloneJSONB(account.Config)
	config["outbound_enabled"] = false
	config["ai_reply_enabled"] = false
	result := a.DB.Model(&models.ChannelAccount{}).Where(
		"id = ? AND organization_id = ?", account.ID, organizationID,
	).Updates(map[string]any{
		"status": models.ChannelAccountStatusPending, "metadata": metadata, "config": config,
		"last_health_check_at": nil, "last_error": "", "last_error_at": nil,
		"connected_at": nil, "updated_by_id": userID, "updated_at": confirmedAt,
	})
	if result.Error != nil || result.RowsAffected != 1 {
		if result.Error != nil {
			return metaRegistryProvisionResult{}, result.Error
		}
		return metaRegistryProvisionResult{}, errMetaMessengerSubscriptionFence
	}
	if err := audit.LogAudit(
		a.DB, organizationID, userID, audit.GetUserName(a.DB, userID),
		"meta_channel_registry", account.ID, models.AuditActionUpdated,
		nil, map[string]any{"operation": operation.ID.String(), "subscription": "verified"},
	); err != nil {
		return metaRegistryProvisionResult{}, err
	}
	account.Status = models.ChannelAccountStatusPending
	account.Metadata = metadata
	account.Config = config
	account.LastHealthCheckAt = nil
	account.LastError = ""
	account.ConnectedAt = nil
	account.Credentials = credentials
	return metaRegistryProvisionResult{
		Account: account, OAuthCredentialID: operation.OAuthCredentialID,
		OAuthVersion:          operation.OAuthVersion,
		WebhookCredentialID:   operation.WebhookCredentialID,
		WebhookVersion:        operation.WebhookVersion,
		SubscriptionOperation: operation,
	}, nil
}

func (a *App) recordMetaInstagramSubscriptionOperationFailure(
	organizationID, accountID uuid.UUID,
	operation metaMessengerSubscriptionOperation,
	wantState, failureState string,
	failedAt time.Time,
) error {
	account, _, err := a.lockMetaInstagramSubscriptionOperation(
		organizationID, accountID, operation, wantState,
	)
	if err != nil {
		return err
	}
	metadata := cloneJSONB(account.Metadata)
	metadata[metaMessengerSubscriptionOperationStateKey] = failureState
	metadata[metaMessengerSubscriptionRemoteStateKey] = metaMessengerSubscriptionRemoteUnknown
	metadata[metaMessengerSubscriptionFencedOperationIDKey] = operation.ID.String()
	metadata[metaMessengerSubscriptionFencedOperationEndKey] = operation.ExpiresAt.UTC().Format(time.RFC3339Nano)
	metadata[metaMessengerSubscriptionFencedAckKey] = false
	delete(metadata, metaMessengerSubscriptionFencedAckAtKey)
	metadata["meta_subscription_state"] = metaMessengerSubscriptionFailed
	config := cloneJSONB(account.Config)
	config["outbound_enabled"] = false
	config["ai_reply_enabled"] = false
	result := a.DB.Model(&models.ChannelAccount{}).Where(
		"id = ? AND organization_id = ?", account.ID, organizationID,
	).Updates(map[string]any{
		"status": models.ChannelAccountStatusPending, "config": config, "metadata": metadata,
		"last_error":    "Meta subscription operation was not confirmed",
		"last_error_at": failedAt.UTC(), "updated_at": failedAt.UTC(),
	})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return errMetaMessengerSubscriptionFence
	}
	return nil
}

type metaInstagramReconciliationClaim struct {
	Account     models.ChannelAccount
	Operation   metaMessengerSubscriptionOperation
	AccessToken string
}

const metaInstagramReconciliationAttemptLease = 2*metaInstagramProviderOperationLimit + 30*time.Second

func (a *App) renewMetaInstagramReconciliationOperation(
	organizationID, userID, accountID uuid.UUID,
	operation metaMessengerSubscriptionOperation,
	now time.Time,
) (metaMessengerSubscriptionOperation, error) {
	if err := lockChannelAIOrganizationScopeTx(a.DB, organizationID); err != nil {
		return metaMessengerSubscriptionOperation{}, err
	}
	account, _, err := a.lockMetaInstagramSubscriptionOperation(
		organizationID, accountID, operation, operation.State,
	)
	if err != nil {
		return metaMessengerSubscriptionOperation{}, err
	}
	renewed := operation
	renewed.ExpiresAt = now.UTC().Add(metaInstagramReconciliationAttemptLease)
	metadata := cloneJSONB(account.Metadata)
	metadata[metaMessengerSubscriptionOperationExpiresKey] = renewed.ExpiresAt.Format(time.RFC3339Nano)
	metadata[metaMessengerSubscriptionFencedOperationIDKey] = renewed.ID.String()
	metadata[metaMessengerSubscriptionFencedOperationEndKey] = renewed.ExpiresAt.Format(time.RFC3339Nano)
	metadata[metaMessengerSubscriptionFencedAckKey] = false
	delete(metadata, metaMessengerSubscriptionFencedAckAtKey)
	result := a.DB.Model(&models.ChannelAccount{}).Where(
		"id = ? AND organization_id = ?", account.ID, organizationID,
	).Updates(map[string]any{
		"metadata": metadata, "updated_by_id": userID, "updated_at": now.UTC(),
	})
	if result.Error != nil || result.RowsAffected != 1 {
		if result.Error != nil {
			return metaMessengerSubscriptionOperation{}, result.Error
		}
		return metaMessengerSubscriptionOperation{}, errMetaMessengerSubscriptionFence
	}
	if err := audit.LogAudit(
		a.DB, organizationID, userID, audit.GetUserName(a.DB, userID),
		"meta_channel_registry", account.ID, models.AuditActionUpdated,
		nil, map[string]any{
			"operation": renewed.ID.String(),
			"instagram_subscription_reconciliation_released_until": renewed.ExpiresAt.Format(time.RFC3339Nano),
		},
	); err != nil {
		return metaMessengerSubscriptionOperation{}, err
	}
	return renewed, nil
}

func (a *App) loadMetaInstagramReconciliationClaim(
	organizationID, accountID uuid.UUID,
) (metaInstagramReconciliationClaim, error) {
	if a == nil || a.DB == nil || a.Config == nil || a.tenantOrgID != organizationID ||
		organizationID == uuid.Nil || accountID == uuid.Nil {
		return metaInstagramReconciliationClaim{}, metaregistry.ErrInvalidRequest
	}
	var account models.ChannelAccount
	if err := a.DB.Preload("Credentials", func(db *gorm.DB) *gorm.DB {
		return db.Order("version DESC")
	}).Where(
		"id = ? AND organization_id = ? AND channel = ? AND provider = ?",
		accountID, organizationID, models.ChannelInstagram, channelapi.RelayProvider,
	).First(&account).Error; err != nil {
		return metaInstagramReconciliationClaim{}, err
	}
	if !exactMetaRegistryControlPlaneConfig(account.Config) ||
		stringConfigValue(account.Config, "instagram_api_mode") != "instagram_login" ||
		stringConfigValue(account.Metadata, "meta_platform_app_id") != strings.TrimSpace(a.Config.MetaInstagram.AppID) {
		return metaInstagramReconciliationClaim{}, metaregistry.ErrNotFound
	}
	operation, ok := metaMessengerSubscriptionOperationFromMetadata(account.Metadata)
	if !ok {
		return metaInstagramReconciliationClaim{}, errMetaMessengerSubscriptionFence
	}
	switch operation.State {
	case metaMessengerSubscriptionSubscribePending,
		metaMessengerSubscriptionSubscribeFailed,
		metaMessengerSubscriptionUnsubscribePending,
		metaMessengerSubscriptionUnsubscribeFailed:
	default:
		return metaInstagramReconciliationClaim{}, errMetaMessengerSubscriptionFence
	}
	now := time.Now().UTC()
	oauth := currentMetaRegistryCredential(account.Credentials, models.ChannelCredentialKindOAuth, now)
	webhook := currentMetaRegistryCredential(account.Credentials, models.ChannelCredentialKindWebhook, now)
	if !metaInstagramCredentialPairGenerationValid(oauth, webhook, now) ||
		oauth.ID != operation.OAuthCredentialID ||
		oauth.Version != operation.OAuthVersion || webhook.ID != operation.WebhookCredentialID ||
		webhook.Version != operation.WebhookVersion {
		return metaInstagramReconciliationClaim{}, errMetaMessengerSubscriptionFence
	}
	switch operation.DesiredState {
	case metaMessengerSubscriptionDesiredSubscribed:
		if a.metaInstagramSubscribeBindingReason(
			&account, organizationID, now, operation,
		) != "" {
			return metaInstagramReconciliationClaim{}, errMetaMessengerSubscriptionFence
		}
	case metaMessengerSubscriptionDesiredUnsubscribed:
		if a.metaInstagramTeardownBindingReason(
			&account, organizationID, now, &operation, false,
		) != "" {
			return metaInstagramReconciliationClaim{}, errMetaMessengerSubscriptionFence
		}
	default:
		return metaInstagramReconciliationClaim{}, errMetaMessengerSubscriptionFence
	}
	accessToken, err := decryptRequiredMetaRegistrySecret(
		oauth.CredentialBlob, "access_token", a.Config.App.EncryptionKey,
	)
	if err != nil {
		return metaInstagramReconciliationClaim{}, err
	}
	return metaInstagramReconciliationClaim{
		Account: account, Operation: operation, AccessToken: accessToken,
	}, nil
}

func (a *App) acknowledgeMetaInstagramSubscriptionReconciliation(
	organizationID, userID, accountID uuid.UUID,
	operation metaMessengerSubscriptionOperation,
	confirmedAt time.Time,
) error {
	account, _, err := a.lockMetaInstagramSubscriptionOperation(
		organizationID, accountID, operation, operation.State,
	)
	if err != nil {
		return err
	}
	confirmedAt = confirmedAt.UTC()
	metadata := cloneJSONB(account.Metadata)
	metadata[metaMessengerSubscriptionFencedOperationIDKey] = operation.ID.String()
	metadata[metaMessengerSubscriptionFencedOperationEndKey] = operation.ExpiresAt.UTC().Format(time.RFC3339Nano)
	metadata[metaMessengerSubscriptionFencedAckKey] = true
	metadata[metaMessengerSubscriptionFencedAckAtKey] = confirmedAt.Format(time.RFC3339Nano)
	metadata[metaMessengerSubscriptionRemoteConfirmedAtKey] = confirmedAt.Format(time.RFC3339Nano)
	switch operation.DesiredState {
	case metaMessengerSubscriptionDesiredSubscribed:
		metadata[metaMessengerSubscriptionOperationStateKey] = metaMessengerSubscriptionSubscribePending
		metadata[metaMessengerSubscriptionRemoteStateKey] = metaMessengerSubscriptionRemoteSubscribed
	case metaMessengerSubscriptionDesiredUnsubscribed:
		metadata[metaMessengerSubscriptionOperationStateKey] = metaMessengerSubscriptionUnsubscribePending
		metadata[metaMessengerSubscriptionRemoteStateKey] = metaMessengerSubscriptionRemoteUnsubscribed
	default:
		return errMetaMessengerSubscriptionFence
	}
	result := a.DB.Model(&models.ChannelAccount{}).Where(
		"id = ? AND organization_id = ?", account.ID, organizationID,
	).Updates(map[string]any{"metadata": metadata, "updated_by_id": userID, "updated_at": confirmedAt})
	if result.Error != nil || result.RowsAffected != 1 {
		if result.Error != nil {
			return result.Error
		}
		return errMetaMessengerSubscriptionFence
	}
	return audit.LogAudit(
		a.DB, organizationID, userID, audit.GetUserName(a.DB, userID),
		"meta_channel_registry", account.ID, models.AuditActionUpdated,
		nil, map[string]any{
			"operation":                         operation.ID.String(),
			"instagram_subscription_reconciled": operation.DesiredState,
		},
	)
}

func (a *App) ReconcileMetaInstagramSubscription(r *fastglue.Request) error {
	orgID, userID, _, err := a.requireMetaInstagramOnboardingAuth(r, models.ActionDelete, true)
	if err != nil {
		return nil
	}
	accountID, err := parsePathUUID(r, "id", "channel account")
	if err != nil {
		return nil
	}
	if allowed, err := a.requireMetaInstagramRateLimit(r, orgID, userID, "reconcile", 8, time.Minute); !allowed {
		return err
	}
	settings, err := a.metaInstagramOnboardingSettings()
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusServiceUnavailable, "Managed Instagram reconciliation is unavailable", nil, "")
	}
	var claim metaInstagramReconciliationClaim
	err = database.WithTenantReadCommitted(a.rootApp().DB, orgID, func(tx *gorm.DB) error {
		var loadErr error
		claim, loadErr = a.rootApp().scopedApp(tx, orgID).loadMetaInstagramReconciliationClaim(orgID, accountID)
		return loadErr
	})
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusConflict, "Instagram subscription reconciliation is not available", nil, "")
	}
	err = a.WithCommittedTenantApp(orgID, func(scoped *App) error {
		var renewErr error
		claim.Operation, renewErr = scoped.renewMetaInstagramReconciliationOperation(
			orgID, userID, accountID, claim.Operation, time.Now().UTC(),
		)
		return renewErr
	})
	if err != nil {
		return r.SendErrorEnvelope(
			fasthttp.StatusConflict,
			"Instagram subscription reconciliation is fenced by the current binding",
			nil, "",
		)
	}
	ctx, cancel := context.WithTimeout(requestContext(r), metaInstagramProviderOperationLimit)
	defer cancel()
	err = a.withLockedMetaInstagramSubscriptionProviderAttempt(
		ctx, orgID, accountID, claim.Operation, claim.Operation.State,
		func(account models.ChannelAccount, accessToken string) error {
			switch claim.Operation.DesiredState {
			case metaMessengerSubscriptionDesiredSubscribed:
				return a.subscribeMetaInstagramMessages(
					ctx, settings, account.ExternalAccountID, accessToken,
				)
			case metaMessengerSubscriptionDesiredUnsubscribed:
				return a.unsubscribeMetaInstagramMessages(
					ctx, settings, account.ExternalAccountID, accessToken,
				)
			default:
				return errMetaMessengerSubscriptionFence
			}
		},
	)
	if err != nil {
		if errors.Is(err, errMetaMessengerSubscriptionFence) {
			return r.SendErrorEnvelope(
				fasthttp.StatusConflict,
				"Instagram subscription reconciliation is fenced by the current binding",
				nil, "",
			)
		}
		return r.SendErrorEnvelope(fasthttp.StatusBadGateway, "Instagram subscription state is still ambiguous", nil, "")
	}
	confirmedAt := time.Now().UTC()
	err = a.WithCommittedTenantApp(orgID, func(scoped *App) error {
		return scoped.acknowledgeMetaInstagramSubscriptionReconciliation(
			orgID, userID, accountID, claim.Operation, confirmedAt,
		)
	})
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusConflict, "Instagram subscription reconciliation was superseded", nil, "")
	}
	if claim.Operation.DesiredState == metaMessengerSubscriptionDesiredSubscribed {
		err = a.WithCommittedTenantApp(orgID, func(scoped *App) error {
			_, finalizeErr := scoped.finalizeMetaInstagramSubscribeOperation(
				orgID, userID, accountID, claim.Operation, confirmedAt,
			)
			return finalizeErr
		})
	} else {
		err = a.WithCommittedTenantApp(orgID, func(scoped *App) error {
			return scoped.finalizeMetaInstagramDisconnect(orgID, claim.Operation, accountID, confirmedAt)
		})
	}
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusConflict, "Instagram subscription reconciliation needs another retry", nil, "")
	}
	return r.SendEnvelope(map[string]any{
		"reconciled": true, "account_id": accountID,
		"subscription_state": claim.Operation.DesiredState,
	})
}

type disconnectMetaInstagramRequest struct {
	ConfirmAccountID string `json:"confirm_account_id"`
}

type metaInstagramDisconnectClaim struct {
	Account             models.ChannelAccount
	Operation           metaMessengerSubscriptionOperation
	AccessToken         string
	AlreadyDisconnected bool
}

func (a *App) claimMetaInstagramDisconnect(
	organizationID, userID, accountID uuid.UUID,
	confirmAccountID string,
	operationID uuid.UUID,
	expiresAt time.Time,
) (metaInstagramDisconnectClaim, error) {
	if a == nil || a.DB == nil || a.Config == nil || a.tenantOrgID != organizationID ||
		organizationID == uuid.Nil || userID == uuid.Nil || accountID == uuid.Nil || operationID == uuid.Nil {
		return metaInstagramDisconnectClaim{}, metaregistry.ErrInvalidRequest
	}
	if err := lockChannelAIOrganizationScopeTx(a.DB, organizationID); err != nil {
		return metaInstagramDisconnectClaim{}, err
	}
	var account models.ChannelAccount
	if err := a.DB.Clauses(clause.Locking{Strength: "UPDATE"}).Where(
		"id = ? AND organization_id = ? AND channel = ? AND provider = ?",
		accountID, organizationID, models.ChannelInstagram, channelapi.RelayProvider,
	).First(&account).Error; err != nil {
		return metaInstagramDisconnectClaim{}, err
	}
	if !exactMetaRegistryControlPlaneConfig(account.Config) ||
		stringConfigValue(account.Config, "instagram_api_mode") != "instagram_login" ||
		strings.TrimSpace(confirmAccountID) != account.ExternalAccountID ||
		stringConfigValue(account.Metadata, "meta_platform_app_id") != strings.TrimSpace(a.Config.MetaInstagram.AppID) {
		return metaInstagramDisconnectClaim{}, metaregistry.ErrNotFound
	}
	if account.Status == models.ChannelAccountStatusDisconnected {
		return metaInstagramDisconnectClaim{Account: account, AlreadyDisconnected: true}, nil
	}
	now := time.Now().UTC()
	if !expiresAt.After(now.Add(metaInstagramProviderOperationLimit)) ||
		!metaMessengerSubscriptionOperationAvailable(account.Metadata, now) {
		return metaInstagramDisconnectClaim{}, errMetaMessengerSubscriptionFence
	}
	var credentials []models.ChannelCredential
	if err := a.DB.Clauses(clause.Locking{Strength: "UPDATE"}).Where(
		"organization_id = ? AND channel_account_id = ?",
		organizationID, account.ID,
	).Order("version DESC").Find(&credentials).Error; err != nil {
		return metaInstagramDisconnectClaim{}, err
	}
	oauth := currentMetaRegistryCredential(credentials, models.ChannelCredentialKindOAuth, now)
	webhook := currentMetaRegistryCredential(credentials, models.ChannelCredentialKindWebhook, now)
	account.Credentials = credentials
	if a.metaInstagramTeardownBindingReason(
		&account, organizationID, now, nil, true,
	) != "" {
		return metaInstagramDisconnectClaim{}, errMetaMessengerSubscriptionFence
	}
	accessToken, err := decryptRequiredMetaRegistrySecret(
		oauth.CredentialBlob, "access_token", a.Config.App.EncryptionKey,
	)
	if err != nil {
		return metaInstagramDisconnectClaim{}, err
	}
	operation := metaMessengerSubscriptionOperation{
		ID: operationID, OAuthCredentialID: oauth.ID, OAuthVersion: oauth.Version,
		WebhookCredentialID: webhook.ID, WebhookVersion: webhook.Version,
		DesiredState: metaMessengerSubscriptionDesiredUnsubscribed,
		State:        metaMessengerSubscriptionUnsubscribePending,
		ExpiresAt:    expiresAt.UTC(),
	}
	metadata := metadataWithMetaMessengerSubscriptionOperation(
		account.Metadata, operation, metaMessengerSubscriptionRemoteUnknown,
	)
	metadata["meta_subscription_state"] = metaMessengerSubscriptionUnsubscribePending
	metadata["meta_activation_state"] = "disconnecting"
	config := cloneJSONB(account.Config)
	config["outbound_enabled"] = false
	config["ai_reply_enabled"] = false
	result := a.DB.Model(&models.ChannelAccount{}).Where(
		"id = ? AND organization_id = ?", account.ID, organizationID,
	).Updates(map[string]any{
		"status": models.ChannelAccountStatusPending, "config": config, "metadata": metadata,
		"last_error":    "Instagram disconnect is awaiting provider confirmation",
		"last_error_at": now, "updated_by_id": userID, "updated_at": now,
	})
	if result.Error != nil || result.RowsAffected != 1 {
		if result.Error != nil {
			return metaInstagramDisconnectClaim{}, result.Error
		}
		return metaInstagramDisconnectClaim{}, errMetaMessengerSubscriptionFence
	}
	if err := cancelManagedMetaQueuedWorkForAccountTx(
		a.DB, organizationID, account.ID, "managed_instagram_disconnect_claimed",
	); err != nil {
		return metaInstagramDisconnectClaim{}, err
	}
	account.Metadata = metadata
	account.Status = models.ChannelAccountStatusPending
	account.Config = config
	account.LastError = "Instagram disconnect is awaiting provider confirmation"
	account.LastErrorAt = &now
	return metaInstagramDisconnectClaim{
		Account: account, Operation: operation, AccessToken: accessToken,
	}, nil
}

func (a *App) finalizeMetaInstagramDisconnect(
	organizationID uuid.UUID,
	operation metaMessengerSubscriptionOperation,
	accountID uuid.UUID,
	confirmedAt time.Time,
) error {
	if err := lockChannelAIOrganizationScopeTx(a.DB, organizationID); err != nil {
		return err
	}
	account, _, err := a.lockMetaInstagramSubscriptionOperation(
		organizationID, accountID, operation, metaMessengerSubscriptionUnsubscribePending,
	)
	if err != nil {
		return err
	}
	confirmedAt = confirmedAt.UTC()
	if previous, parseErr := time.Parse(
		time.RFC3339Nano, stringConfigValue(account.Metadata, "meta_ownership_checked_at"),
	); parseErr == nil && !confirmedAt.After(previous) {
		confirmedAt = previous.Add(time.Nanosecond)
	}
	applied, err := a.applyMetaRegistryMutation(metaregistry.MutationRequest{
		ChannelAccountID: account.ID,
		CredentialID:     operation.OAuthCredentialID, CredentialVersion: operation.OAuthVersion,
		WebhookCredentialID:      operation.WebhookCredentialID,
		WebhookCredentialVersion: operation.WebhookVersion,
		Outcome:                  metaregistry.OwnershipRevoked, Reason: "admin_disconnect",
		CheckedAt: confirmedAt,
	}, metaregistry.OwnershipRevoked)
	if err != nil || !applied {
		if err != nil {
			return err
		}
		return errMetaMessengerSubscriptionFence
	}
	if err := a.DB.Clauses(clause.Locking{Strength: "UPDATE"}).Where(
		"id = ? AND organization_id = ?", account.ID, organizationID,
	).First(&account).Error; err != nil {
		return err
	}
	metadata := cloneJSONB(account.Metadata)
	metadata[metaMessengerSubscriptionOperationStateKey] = metaMessengerSubscriptionUnsubscribeConfirmed
	metadata[metaMessengerSubscriptionRemoteStateKey] = metaMessengerSubscriptionRemoteUnsubscribed
	metadata[metaMessengerSubscriptionRemoteConfirmedAtKey] = confirmedAt.Format(time.RFC3339Nano)
	metadata["meta_subscription_state"] = metaMessengerSubscriptionRemoteUnsubscribed
	clearMetaMessengerSubscriptionFence(metadata)
	result := a.DB.Model(&models.ChannelAccount{}).Where(
		"id = ? AND organization_id = ?", account.ID, organizationID,
	).Update("metadata", metadata)
	if result.Error != nil || result.RowsAffected != 1 {
		if result.Error != nil {
			return result.Error
		}
		return errMetaMessengerSubscriptionFence
	}
	return nil
}

func (a *App) DisconnectMetaInstagram(r *fastglue.Request) error {
	orgID, userID, _, err := a.requireMetaInstagramOnboardingAuth(r, models.ActionDelete, true)
	if err != nil {
		return nil
	}
	if allowed, err := a.requireMetaInstagramRateLimit(r, orgID, userID, "disconnect", 8, time.Minute); !allowed {
		return err
	}
	accountID, err := parsePathUUID(r, "id", "channel account")
	if err != nil {
		return nil
	}
	var request disconnectMetaInstagramRequest
	if err := decodeStrictMetaMessengerRequest(r, &request); err != nil {
		return nil
	}
	settings, settingsErr := a.metaInstagramOnboardingSettings()
	if settingsErr != nil {
		return r.SendErrorEnvelope(fasthttp.StatusServiceUnavailable, "Managed Instagram disconnect is unavailable", nil, "")
	}
	operationID := uuid.New()
	operationExpiresAt := time.Now().UTC().Add(metaMessengerSubscriptionOperationLease)
	var claim metaInstagramDisconnectClaim
	err = a.WithCommittedTenantApp(orgID, func(scoped *App) error {
		var claimErr error
		claim, claimErr = scoped.claimMetaInstagramDisconnect(
			orgID, userID, accountID, strings.TrimSpace(request.ConfirmAccountID),
			operationID, operationExpiresAt,
		)
		return claimErr
	})
	if err != nil {
		if errors.Is(err, errMetaMessengerSubscriptionFence) {
			return r.SendErrorEnvelope(fasthttp.StatusConflict, "Another Instagram subscription operation is already in progress", nil, "")
		}
		return r.SendErrorEnvelope(fasthttp.StatusNotFound, "Managed Instagram account not found", nil, "")
	}
	if claim.AlreadyDisconnected {
		return r.SendEnvelope(map[string]any{"disconnected": true, "account_id": claim.Account.ID})
	}
	ctx, cancel := context.WithTimeout(requestContext(r), metaInstagramProviderOperationLimit)
	defer cancel()
	if err := a.withLockedMetaInstagramSubscriptionProviderAttempt(
		ctx, orgID, claim.Account.ID, claim.Operation,
		metaMessengerSubscriptionUnsubscribePending,
		func(account models.ChannelAccount, accessToken string) error {
			return a.unsubscribeMetaInstagramMessages(
				ctx, settings, account.ExternalAccountID, accessToken,
			)
		},
	); err != nil {
		if errors.Is(err, errMetaMessengerSubscriptionFence) {
			return r.SendErrorEnvelope(
				fasthttp.StatusConflict, "The Instagram disconnect was superseded", nil, "",
			)
		}
		_ = a.WithCommittedTenantApp(orgID, func(scoped *App) error {
			return scoped.recordMetaInstagramSubscriptionOperationFailure(
				orgID, claim.Account.ID, claim.Operation,
				metaMessengerSubscriptionUnsubscribePending,
				metaMessengerSubscriptionUnsubscribeFailed,
				time.Now().UTC(),
			)
		})
		return r.SendErrorEnvelope(
			fasthttp.StatusBadGateway,
			"Instagram unsubscription is ambiguous; the account is quarantined for reconciliation",
			nil, "",
		)
	}
	err = a.WithCommittedTenantApp(orgID, func(scoped *App) error {
		return scoped.finalizeMetaInstagramDisconnect(
			orgID, claim.Operation, claim.Account.ID, time.Now().UTC(),
		)
	})
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusConflict, "The Instagram disconnect was superseded", nil, "")
	}
	return r.SendEnvelope(map[string]any{"disconnected": true, "account_id": claim.Account.ID})
}
