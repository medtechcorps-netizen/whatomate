package handlers

import (
	"errors"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/audit"
	channelapi "github.com/shridarpatil/whatomate/internal/channel"
	appcrypto "github.com/shridarpatil/whatomate/internal/crypto"
	"github.com/shridarpatil/whatomate/internal/metaregistry"
	"github.com/shridarpatil/whatomate/internal/models"
)

var errMetaInstagramProfileAlreadyRegistered = errors.New(
	"instagram profile already has a relay account; reconnect or migrate that account",
)

// metaRegistryProvisionInput is the narrow persistence boundary for future
// Messenger and Instagram OAuth completion handlers. Callers must complete
// provider token/scopes/ownership verification before entering the committed
// tenant transaction that invokes this helper.
type metaRegistryProvisionInput struct {
	OrganizationID                 uuid.UUID
	UserID                         uuid.UUID
	Channel                        models.Channel
	Name                           string
	ExternalAccountID              string
	InstagramAPIMode               string
	WebhookApp                     string
	PlatformAppID                  string
	MetaBusinessID                 string
	MetaAuthorityAssetID           string
	AuthorizingMetaUserID          string
	AuthorizationTokenKind         string
	BusinessAuthorityVerified      bool
	GrantedScopes                  []string
	AccessToken                    string
	AuthorityToken                 string
	TokenExpiresAt                 *time.Time
	OwnershipCheckedAt             time.Time
	AuthorizationStartedAt         time.Time
	ReReplyBaseURL                 string
	RelayBaseURL                   string
	SubscriptionOperationID        uuid.UUID
	SubscriptionOperationExpiresAt time.Time
}

type metaRegistryProvisionResult struct {
	Account               models.ChannelAccount
	OAuthCredentialID     uuid.UUID
	OAuthVersion          int
	WebhookCredentialID   uuid.UUID
	WebhookVersion        int
	SubscriptionOperation metaMessengerSubscriptionOperation
}

func (a *App) provisionMetaRegistryBinding(input metaRegistryProvisionInput) (metaRegistryProvisionResult, error) {
	if a == nil || a.DB == nil || a.Config == nil || a.tenantOrgID == uuid.Nil ||
		input.OrganizationID != a.tenantOrgID || input.UserID == uuid.Nil ||
		strings.TrimSpace(a.Config.App.EncryptionKey) == "" {
		return metaRegistryProvisionResult{}, errors.New("meta registry tenant scope is unavailable")
	}
	input.Name = strings.TrimSpace(input.Name)
	input.ExternalAccountID = strings.TrimSpace(input.ExternalAccountID)
	input.AccessToken = strings.TrimSpace(input.AccessToken)
	input.AuthorityToken = strings.TrimSpace(input.AuthorityToken)
	input.MetaBusinessID = strings.TrimSpace(input.MetaBusinessID)
	input.MetaAuthorityAssetID = strings.TrimSpace(input.MetaAuthorityAssetID)
	input.AuthorizingMetaUserID = strings.TrimSpace(input.AuthorizingMetaUserID)
	input.AuthorizationTokenKind = strings.ToUpper(strings.TrimSpace(input.AuthorizationTokenKind))
	input.WebhookApp = strings.TrimSpace(input.WebhookApp)
	input.PlatformAppID = strings.TrimSpace(input.PlatformAppID)
	input.InstagramAPIMode = strings.TrimSpace(input.InstagramAPIMode)
	now := time.Now().UTC()
	if input.SubscriptionOperationID == uuid.Nil {
		input.SubscriptionOperationID = uuid.New()
	}
	if input.SubscriptionOperationExpiresAt.IsZero() {
		input.SubscriptionOperationExpiresAt = now.Add(metaMessengerSubscriptionOperationLease)
	} else {
		input.SubscriptionOperationExpiresAt = input.SubscriptionOperationExpiresAt.UTC()
	}
	validPlatformBinding := false
	switch input.Channel {
	case models.ChannelMessenger:
		validPlatformBinding = input.InstagramAPIMode == "" && input.WebhookApp == "messenger" &&
			input.MetaBusinessID != "" &&
			((input.AuthorizationTokenKind == metaMessengerTokenKindSystemUser &&
				input.BusinessAuthorityVerified) ||
				(input.AuthorizationTokenKind == metaMessengerTokenKindUser &&
					!input.BusinessAuthorityVerified))
	case models.ChannelInstagram:
		validPlatformBinding = input.InstagramAPIMode == "instagram_login" &&
			input.WebhookApp == "instagram_login" &&
			input.MetaAuthorityAssetID == input.ExternalAccountID &&
			validCanonicalMetaID(input.AuthorizingMetaUserID) &&
			input.PlatformAppID == strings.TrimSpace(a.Config.MetaInstagram.AppID)
	}
	if input.Name == "" || utf8.RuneCountInString(input.Name) > 100 ||
		!channelExternalAccountIdentifier.MatchString(input.ExternalAccountID) ||
		input.AccessToken == "" || input.AuthorityToken == "" || input.AuthorizingMetaUserID == "" ||
		(input.AuthorizationTokenKind != metaMessengerTokenKindSystemUser &&
			input.AuthorizationTokenKind != metaMessengerTokenKindUser) ||
		input.PlatformAppID == "" || !validPlatformBinding ||
		input.OwnershipCheckedAt.IsZero() || input.OwnershipCheckedAt.After(now.Add(time.Minute)) ||
		input.OwnershipCheckedAt.Before(now.Add(-10*time.Minute)) ||
		(input.TokenExpiresAt != nil && !input.TokenExpiresAt.After(now.Add(5*time.Second))) ||
		(input.Channel == models.ChannelInstagram &&
			(input.TokenExpiresAt == nil || !input.TokenExpiresAt.After(now.Add(metaInstagramProviderOperationLimit)) ||
				!metaInstagramOAuthAuthorizationStartValid(input.AuthorizationStartedAt, now))) ||
		!input.SubscriptionOperationExpiresAt.After(now) {
		return metaRegistryProvisionResult{}, metaregistry.ErrInvalidRequest
	}
	if input.Channel == models.ChannelInstagram {
		if err := lockMetaInstagramIdentityScopeTx(
			a.DB, input.PlatformAppID, input.AuthorizingMetaUserID,
		); err != nil {
			return metaRegistryProvisionResult{}, err
		}
		if err := lockMetaInstagramProfessionalScopeTx(
			a.DB, input.PlatformAppID, input.ExternalAccountID,
		); err != nil {
			return metaRegistryProvisionResult{}, err
		}
		// The organization row is the gap-lock surrogate for a profile that does
		// not exist yet. It serializes two valid OAuth callbacks before either can
		// create an account and also protects the exact static/managed collision
		// query from a concurrent account create.
		if err := lockChannelAIOrganizationScopeTx(a.DB, input.OrganizationID); err != nil {
			return metaRegistryProvisionResult{}, err
		}
		if err := a.metaInstagramOAuthGenerationFence(
			input.OrganizationID,
			input.PlatformAppID,
			input.AuthorizingMetaUserID,
			input.AuthorizationStartedAt,
			time.Time{},
			nil,
		); err != nil {
			return metaRegistryProvisionResult{}, err
		}
		if err := a.ensureMetaInstagramProfileAvailableTx(
			input.OrganizationID, uuid.Nil, input.ExternalAccountID,
		); err != nil {
			return metaRegistryProvisionResult{}, err
		}
	}
	var membership models.UserOrganization
	if err := a.DB.Where("user_id = ? AND organization_id = ?", input.UserID, input.OrganizationID).
		First(&membership).Error; err != nil {
		return metaRegistryProvisionResult{}, metaregistry.ErrInvalidRequest
	}
	accountID := uuid.New()
	rereplyWebhookURL, err := metaRegistryJoinURL(input.ReReplyBaseURL, "api", "webhooks", "channels", accountID.String())
	if err != nil {
		return metaRegistryProvisionResult{}, err
	}
	relayURL, err := metaRegistryJoinURL(
		input.RelayBaseURL, "v1", "accounts", string(input.Channel), input.ExternalAccountID,
	)
	if err != nil {
		return metaRegistryProvisionResult{}, err
	}
	authorizationGrantedAt := input.OwnershipCheckedAt.UTC()
	if input.Channel == models.ChannelInstagram {
		authorizationGrantedAt = input.AuthorizationStartedAt.UTC()
	}
	account := models.ChannelAccount{
		BaseModel: models.BaseModel{ID: accountID}, OrganizationID: input.OrganizationID,
		Channel: input.Channel, Provider: channelapi.RelayProvider, Name: input.Name,
		ExternalAccountID: input.ExternalAccountID, Status: models.ChannelAccountStatusPending,
		Capabilities: models.JSONB{"text": true, "replies": true, "service_window": true},
		Config: models.JSONB{
			"relay_url": relayURL, "rereply_webhook_url": rereplyWebhookURL,
			"meta_registry_managed": true, "meta_management_mode": metaregistry.ManagementModePlatformOAuth,
			"instagram_api_mode": input.InstagramAPIMode,
			"outbound_enabled":   false, "ai_reply_enabled": false,
		},
		Metadata: models.JSONB{
			"meta_ownership_state":                 metaregistry.OwnershipVerified,
			"meta_ownership_checked_at":            input.OwnershipCheckedAt.UTC().Format(time.RFC3339Nano),
			metaMessengerAuthorizationGrantedAtKey: authorizationGrantedAt.Format(time.RFC3339Nano),
			"meta_webhook_app":                     input.WebhookApp,
			"meta_platform_app_id":                 input.PlatformAppID,
			"meta_business_id":                     input.MetaBusinessID,
			"meta_authority_asset_id":              input.MetaAuthorityAssetID,
			"meta_authorizing_user_id":             input.AuthorizingMetaUserID,
			metaMessengerAuthorizationTokenKindKey: input.AuthorizationTokenKind,
			"meta_granted_scopes":                  append([]string(nil), input.GrantedScopes...),
			"meta_subscription_state":              metaMessengerVerifyingSubscription,
			"meta_activation_state":                metaMessengerAwaitingRegistryState,
		},
		CreatedByID: &input.UserID, UpdatedByID: &input.UserID,
	}
	if input.Channel == models.ChannelInstagram {
		account.Metadata[metaInstagramOAuthSubjectIDKey] = input.AuthorizingMetaUserID
		releaseMode, allowed := a.metaInstagramReleaseMode(
			input.OrganizationID, input.AuthorizingMetaUserID, input.ExternalAccountID,
		)
		if !allowed {
			return metaRegistryProvisionResult{}, metaregistry.ErrInvalidRequest
		}
		account.Metadata["meta_release_evidence_mode"] = releaseMode
		account.Metadata["meta_release_review_status"] = strings.ToLower(strings.TrimSpace(a.Config.MetaInstagram.AppReviewStatus))
		if releaseMode == "development_app_role" {
			account.Metadata["meta_release_profile_id"] = input.ExternalAccountID
			account.Metadata["meta_release_oauth_subject_id"] = input.AuthorizingMetaUserID
			account.Metadata["meta_release_app_role"] = strings.ToLower(strings.TrimSpace(a.Config.MetaInstagram.DevelopmentAppRole))
		}
	}
	markMetaMessengerAuthorizationDurability(account.Metadata, input.AuthorizationTokenKind, input.TokenExpiresAt)
	inboundSecret, err := generateChannelSecret()
	if err != nil {
		return metaRegistryProvisionResult{}, err
	}
	outboundSecret, err := generateChannelSecret()
	if err != nil {
		return metaRegistryProvisionResult{}, err
	}
	encrypt := func(value string) (string, error) {
		ciphertext, encryptErr := appcrypto.Encrypt(value, a.Config.App.EncryptionKey)
		if encryptErr != nil || !appcrypto.IsEncrypted(ciphertext) {
			return "", errors.New("meta registry credential encryption failed")
		}
		return ciphertext, nil
	}
	encryptedToken, err := encrypt(input.AccessToken)
	if err != nil {
		return metaRegistryProvisionResult{}, err
	}
	encryptedAuthorityToken, err := encrypt(input.AuthorityToken)
	if err != nil {
		return metaRegistryProvisionResult{}, err
	}
	encryptedInbound, err := encrypt(inboundSecret)
	if err != nil {
		return metaRegistryProvisionResult{}, err
	}
	encryptedOutbound, err := encrypt(outboundSecret)
	if err != nil {
		return metaRegistryProvisionResult{}, err
	}
	oauth := models.ChannelCredential{
		BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: input.OrganizationID,
		ChannelAccountID: account.ID, Kind: models.ChannelCredentialKindOAuth, Version: 1,
		CredentialBlob: models.JSONB{"access_token": encryptedToken, "authority_token": encryptedAuthorityToken},
		Status:         models.ChannelCredentialStatusActive, KeyVersion: "app:v1",
		ExpiresAt: input.TokenExpiresAt, LastValidatedAt: &input.OwnershipCheckedAt, Metadata: models.JSONB{},
	}
	webhook := models.ChannelCredential{
		BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: input.OrganizationID,
		ChannelAccountID: account.ID, Kind: models.ChannelCredentialKindWebhook, Version: 1,
		CredentialBlob: models.JSONB{"inbound_secret": encryptedInbound, "outbound_secret": encryptedOutbound},
		Status:         models.ChannelCredentialStatusActive, KeyVersion: "app:v1", Metadata: models.JSONB{},
	}
	operation := metaMessengerSubscriptionOperation{
		ID:                  input.SubscriptionOperationID,
		OAuthCredentialID:   oauth.ID,
		OAuthVersion:        oauth.Version,
		WebhookCredentialID: webhook.ID,
		WebhookVersion:      webhook.Version,
		DesiredState:        metaMessengerSubscriptionDesiredSubscribed,
		State:               metaMessengerSubscriptionSubscribePending,
		ExpiresAt:           input.SubscriptionOperationExpiresAt,
	}
	account.Metadata = metadataWithMetaMessengerSubscriptionOperation(
		account.Metadata,
		operation,
		metaMessengerSubscriptionRemoteUnknown,
	)
	if input.Channel == models.ChannelMessenger &&
		input.AuthorizationTokenKind == metaMessengerTokenKindSystemUser &&
		!metaMessengerScopeGranted(input.GrantedScopes, "business_management") {
		setMetaMessengerBusinessAuthorityEvidence(
			account.Metadata,
			input.PlatformAppID,
			input.AuthorizingMetaUserID,
			input.MetaBusinessID,
			input.ExternalAccountID,
			input.OwnershipCheckedAt,
			oauth.ID,
			oauth.Version,
		)
	}
	validationAccount := account
	validationAccount.Credentials = []models.ChannelCredential{oauth, webhook}
	if err := validateMetaRegistryPlatformBinding(&validationAccount); err != nil {
		return metaRegistryProvisionResult{}, err
	}
	if err := a.DB.Create(&account).Error; err != nil {
		return metaRegistryProvisionResult{}, err
	}
	if err := a.DB.Create(&oauth).Error; err != nil {
		return metaRegistryProvisionResult{}, err
	}
	if input.Channel == models.ChannelInstagram {
		if err := a.metaInstagramOAuthGenerationFence(
			input.OrganizationID,
			input.PlatformAppID,
			input.AuthorizingMetaUserID,
			input.AuthorizationStartedAt,
			oauth.CreatedAt,
			account.Metadata,
		); err != nil {
			return metaRegistryProvisionResult{}, err
		}
	}
	if err := a.DB.Create(&webhook).Error; err != nil {
		return metaRegistryProvisionResult{}, err
	}
	if err := audit.LogAudit(
		a.DB, input.OrganizationID, input.UserID, audit.GetUserName(a.DB, input.UserID),
		"meta_channel_registry", account.ID, models.AuditActionCreated,
		nil, map[string]any{
			"channel": account.Channel, "external_account_id": account.ExternalAccountID,
			"management_mode": metaregistry.ManagementModePlatformOAuth,
			"ownership_state": metaregistry.OwnershipVerified,
		},
		map[string]any{"field": "credentials", "old_value": nil, "new_value": "********"},
	); err != nil {
		return metaRegistryProvisionResult{}, err
	}
	account.Credentials = []models.ChannelCredential{oauth, webhook}
	return metaRegistryProvisionResult{
		Account: account, OAuthCredentialID: oauth.ID, OAuthVersion: oauth.Version,
		WebhookCredentialID: webhook.ID, WebhookVersion: webhook.Version,
		SubscriptionOperation: operation,
	}, nil
}

func (a *App) ensureMetaInstagramProfileAvailableTx(
	requestOrganizationID, permittedAccountID uuid.UUID,
	professionalAccountID string,
) error {
	if a == nil || a.DB == nil || a.Config == nil ||
		requestOrganizationID == uuid.Nil ||
		!validCanonicalMetaID(strings.TrimSpace(professionalAccountID)) {
		return metaregistry.ErrInvalidRequest
	}
	for _, organizationText := range a.Config.MetaInstagram.ManagedOrganizationIDs() {
		organizationID, err := uuid.Parse(organizationText)
		if err != nil || organizationID == uuid.Nil || organizationID.String() != organizationText {
			return metaregistry.ErrInvalidRequest
		}
		if err := a.setMetaInstagramTenantContextTx(a.DB, organizationID); err != nil {
			return err
		}
		var accountIDs []uuid.UUID
		if err := a.DB.Model(&models.ChannelAccount{}).Where(
			"organization_id = ? AND channel = ? AND provider = ? AND external_account_id = ?",
			organizationID, models.ChannelInstagram,
			channelapi.RelayProvider, strings.TrimSpace(professionalAccountID),
		).Order("id").Limit(2).Pluck("id", &accountIDs).Error; err != nil {
			return err
		}
		for _, accountID := range accountIDs {
			if permittedAccountID == uuid.Nil || accountID != permittedAccountID ||
				organizationID != requestOrganizationID {
				return errMetaInstagramProfileAlreadyRegistered
			}
		}
	}
	return a.setMetaInstagramTenantContextTx(a.DB, requestOrganizationID)
}

func metaRegistryJoinURL(base string, path ...string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(base))
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.User != nil ||
		parsed.Fragment != "" || parsed.RawQuery != "" {
		return "", metaregistry.ErrInvalidRequest
	}
	joined, err := url.JoinPath(parsed.String(), path...)
	if err != nil {
		return "", metaregistry.ErrInvalidRequest
	}
	return joined, nil
}
