package handlers

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/audit"
	channelapi "github.com/shridarpatil/whatomate/internal/channel"
	configpkg "github.com/shridarpatil/whatomate/internal/config"
	appcrypto "github.com/shridarpatil/whatomate/internal/crypto"
	"github.com/shridarpatil/whatomate/internal/metaregistry"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/zerodha/fastglue"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	metaRegistryMaxBodyBytes = 32 << 10
	metaRegistryAuditActor   = "Meta Registry Service"
)

// ResolveMetaRegistryBinding is a private, service-authenticated broker. It
// returns a short lease and never exposes credentials to a browser or tenant
// API. The global lookup yields only one organization ID; all secret reads then
// happen inside that exact tenant's RLS transaction.
func (a *App) ResolveMetaRegistryBinding(r *fastglue.Request) error {
	raw, nonce, ok := a.authenticateMetaRegistryRequest(r, metaregistry.ResolvePath)
	if !ok {
		return nil
	}
	var request metaregistry.ResolveRequest
	if decodeMetaRegistryJSON(raw, &request) != nil {
		return a.sendMetaRegistryResponse(r, nonce, http.StatusBadRequest, map[string]string{"error": "invalid_request"})
	}
	request, err := metaregistry.NormalizeResolveRequest(request)
	if err != nil {
		return a.sendMetaRegistryResponse(r, nonce, http.StatusBadRequest, map[string]string{"error": "invalid_request"})
	}
	organizationID, err := a.resolveMetaRegistryOrganization(request)
	if err != nil || organizationID == uuid.Nil {
		return a.sendMetaRegistryResponse(r, nonce, http.StatusNotFound, map[string]string{"error": "binding_not_found"})
	}

	var binding metaregistry.Binding
	err = a.WithCommittedTenantApp(organizationID, func(scoped *App) error {
		var resolveErr error
		binding, resolveErr = scoped.loadMetaRegistryBinding(request, time.Now().UTC())
		return resolveErr
	})
	if err != nil {
		if errors.Is(err, metaregistry.ErrStaleBinding) {
			return a.sendMetaRegistryResponse(r, nonce, http.StatusConflict, map[string]string{"error": "binding_stale"})
		}
		if errors.Is(err, gorm.ErrRecordNotFound) || errors.Is(err, metaregistry.ErrNotFound) {
			return a.sendMetaRegistryResponse(r, nonce, http.StatusNotFound, map[string]string{"error": "binding_not_found"})
		}
		a.Log.Warn("Meta registry binding resolution failed", "channel", request.Channel)
		return a.sendMetaRegistryResponse(r, nonce, http.StatusServiceUnavailable, map[string]string{"error": "registry_unavailable"})
	}
	return a.sendMetaRegistryResponse(r, nonce, http.StatusOK, binding)
}

func (a *App) RevokeMetaRegistryBinding(r *fastglue.Request) error {
	return a.mutateMetaRegistryBinding(r, metaregistry.RevokePath, true)
}

func (a *App) RecordMetaRegistryRevalidation(r *fastglue.Request) error {
	return a.mutateMetaRegistryBinding(r, metaregistry.ReviewPath, false)
}

func (a *App) mutateMetaRegistryBinding(r *fastglue.Request, path string, forceRevoke bool) error {
	raw, nonce, ok := a.authenticateMetaRegistryRequest(r, path)
	if !ok {
		return nil
	}
	var request metaregistry.MutationRequest
	if decodeMetaRegistryJSON(raw, &request) != nil || request.ChannelAccountID == uuid.Nil ||
		request.CredentialID == uuid.Nil || request.CredentialVersion < 1 ||
		request.WebhookCredentialID == uuid.Nil || request.WebhookCredentialVersion < 1 {
		return a.sendMetaRegistryResponse(r, nonce, http.StatusBadRequest, map[string]string{"error": "invalid_request"})
	}
	request.Reason = strings.TrimSpace(request.Reason)
	if len(request.Reason) > 200 {
		return a.sendMetaRegistryResponse(r, nonce, http.StatusBadRequest, map[string]string{"error": "invalid_request"})
	}
	if request.CheckedAt.IsZero() {
		request.CheckedAt = time.Now().UTC()
	} else {
		request.CheckedAt = request.CheckedAt.UTC()
	}
	if request.CheckedAt.After(time.Now().UTC().Add(time.Minute)) {
		return a.sendMetaRegistryResponse(r, nonce, http.StatusBadRequest, map[string]string{"error": "invalid_request"})
	}
	outcome := strings.ToLower(strings.TrimSpace(request.Outcome))
	if forceRevoke {
		outcome = metaregistry.OwnershipRevoked
	}
	if outcome != metaregistry.OwnershipVerified && outcome != metaregistry.OwnershipStale &&
		outcome != metaregistry.OwnershipRevoked {
		return a.sendMetaRegistryResponse(r, nonce, http.StatusBadRequest, map[string]string{"error": "invalid_request"})
	}

	organizationID, err := a.resolveChannelAccountOrganization(request.ChannelAccountID)
	if err != nil || organizationID == uuid.Nil {
		return a.sendMetaRegistryResponse(r, nonce, http.StatusNotFound, map[string]string{"error": "binding_not_found"})
	}
	applied := false
	err = a.WithCommittedTenantApp(organizationID, func(scoped *App) error {
		var mutationErr error
		applied, mutationErr = scoped.applyMetaRegistryMutation(request, outcome)
		return mutationErr
	})
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) || errors.Is(err, metaregistry.ErrNotFound) {
			return a.sendMetaRegistryResponse(r, nonce, http.StatusNotFound, map[string]string{"error": "binding_not_found"})
		}
		a.Log.Warn("Meta registry state mutation failed", "channel_account_id", request.ChannelAccountID)
		return a.sendMetaRegistryResponse(r, nonce, http.StatusServiceUnavailable, map[string]string{"error": "registry_unavailable"})
	}
	return a.sendMetaRegistryResponse(r, nonce, http.StatusOK, metaregistry.MutationResponse{Applied: applied})
}

func (a *App) authenticateMetaRegistryRequest(r *fastglue.Request, expectedPath string) ([]byte, string, bool) {
	if a == nil || a.Config == nil || a.Redis == nil || r == nil || r.RequestCtx == nil {
		return nil, "", false
	}
	secret := strings.TrimSpace(a.Config.MetaRegistry.ServiceSecret)
	raw := append([]byte(nil), r.RequestCtx.PostBody()...)
	nonce := strings.TrimSpace(string(r.RequestCtx.Request.Header.Peek(metaregistry.NonceHeader)))
	if len(raw) > metaRegistryMaxBodyBytes || metaregistry.VerifyRequest(
		secret,
		string(r.RequestCtx.Method()),
		expectedPath,
		string(r.RequestCtx.Request.Header.Peek(metaregistry.TimestampHeader)),
		nonce,
		string(r.RequestCtx.Request.Header.Peek(metaregistry.SignatureHeader)),
		raw,
		time.Now().UTC(),
	) != nil {
		_ = a.sendMetaRegistryResponse(r, nonce, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return nil, nonce, false
	}
	window := time.Duration(a.Config.MetaRegistry.ReplayWindowSeconds) * time.Second
	if window < metaregistry.ReplayWindowFloor || window > 10*time.Minute {
		window = 5 * time.Minute
	}
	replayKey := "meta-registry:nonce:" + nonce
	accepted, err := a.Redis.SetNX(requestContext(r), replayKey, "1", window).Result()
	if err != nil || !accepted {
		_ = a.sendMetaRegistryResponse(r, nonce, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return nil, nonce, false
	}
	return raw, nonce, true
}

func (a *App) sendMetaRegistryResponse(r *fastglue.Request, nonce string, status int, value any) error {
	body, err := json.Marshal(value)
	if err != nil {
		status = http.StatusInternalServerError
		body = []byte(`{"error":"registry_unavailable"}`)
	}
	secret := ""
	if a != nil && a.Config != nil {
		secret = strings.TrimSpace(a.Config.MetaRegistry.ServiceSecret)
	}
	r.RequestCtx.Response.Header.SetContentType("application/json; charset=utf-8")
	r.RequestCtx.Response.Header.Set("Cache-Control", "no-store")
	r.RequestCtx.Response.Header.Set(
		metaregistry.ResponseHeader,
		metaregistry.SignResponse(secret, nonce, status, body),
	)
	r.RequestCtx.Response.SetStatusCode(status)
	r.RequestCtx.Response.SetBody(body)
	return nil
}

func decodeMetaRegistryJSON(raw []byte, value any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("invalid trailing JSON")
	}
	return nil
}

func (a *App) resolveMetaRegistryOrganization(request metaregistry.ResolveRequest) (uuid.UUID, error) {
	root := a.rootApp()
	type organizationOwner struct {
		OrganizationID uuid.UUID `gorm:"column:organization_id"`
	}
	if request.Channel == models.ChannelInstagram {
		if a == nil || a.Config == nil || !a.Config.MetaInstagram.Enabled {
			return uuid.Nil, gorm.ErrRecordNotFound
		}
		allowedOrganizationID, err := uuid.Parse(
			strings.TrimSpace(a.Config.MetaInstagram.AllowedOrganizationID),
		)
		if err != nil || allowedOrganizationID == uuid.Nil ||
			!validCanonicalMetaID(strings.TrimSpace(a.Config.MetaInstagram.AppID)) {
			return uuid.Nil, gorm.ErrRecordNotFound
		}
		// The deployment config is the sole tenant router for managed Instagram.
		// Exact account/app/profile cardinality is checked only after entering the
		// tenant RLS transaction; no cross-tenant SECURITY DEFINER lookup exists.
		return allowedOrganizationID, nil
	}
	if a.rlsEnabled() {
		var raw string
		if err := root.DB.Raw(
			"SELECT COALESCE(public.rereply_resolve_meta_channel_org(?, ?)::text, '')",
			string(request.Channel), request.ExternalAccountID,
		).Scan(&raw).Error; err != nil {
			return uuid.Nil, err
		}
		if raw == "" {
			return uuid.Nil, gorm.ErrRecordNotFound
		}
		return uuid.Parse(raw)
	}
	var owner organizationOwner
	if err := root.DB.Model(&models.ChannelAccount{}).
		Select("organization_id").
		Where("channel = ? AND provider = ? AND external_account_id = ? AND status IN ?",
			request.Channel, channelapi.RelayProvider, request.ExternalAccountID,
			[]models.ChannelAccountStatus{models.ChannelAccountStatusPending, models.ChannelAccountStatusActive, models.ChannelAccountStatusDegraded}).
		Take(&owner).Error; err != nil {
		return uuid.Nil, err
	}
	return owner.OrganizationID, nil
}

func (a *App) resolveChannelAccountOrganization(accountID uuid.UUID) (uuid.UUID, error) {
	root := a.rootApp()
	if a.rlsEnabled() {
		var raw string
		if err := root.DB.Raw(
			"SELECT COALESCE(public.rereply_resolve_channel_org(?)::text, '')", accountID,
		).Scan(&raw).Error; err != nil {
			return uuid.Nil, err
		}
		if raw == "" {
			return uuid.Nil, gorm.ErrRecordNotFound
		}
		return uuid.Parse(raw)
	}
	var owner struct {
		OrganizationID uuid.UUID `gorm:"column:organization_id"`
	}
	if err := root.DB.Model(&models.ChannelAccount{}).Select("organization_id").
		Where("id = ?", accountID).Take(&owner).Error; err != nil {
		return uuid.Nil, err
	}
	return owner.OrganizationID, nil
}

func (a *App) loadMetaRegistryBinding(request metaregistry.ResolveRequest, now time.Time) (metaregistry.Binding, error) {
	var account models.ChannelAccount
	query := a.DB.Preload("Credentials", func(db *gorm.DB) *gorm.DB {
		return db.Order("version DESC")
	})
	statuses := []models.ChannelAccountStatus{
		models.ChannelAccountStatusPending,
		models.ChannelAccountStatusActive,
		models.ChannelAccountStatusDegraded,
	}
	if request.Channel == models.ChannelInstagram {
		if a.Config == nil {
			return metaregistry.Binding{}, metaregistry.ErrNotFound
		}
		var accounts []models.ChannelAccount
		if err := query.Where(
			"organization_id = ? AND channel = ? AND provider = ? AND external_account_id = ? AND status IN ? AND config ->> 'meta_registry_managed' = ? AND config ->> 'meta_management_mode' = ? AND config ->> 'instagram_api_mode' = ? AND metadata ->> 'meta_platform_app_id' = ? AND metadata ->> 'meta_webhook_app' = ? AND metadata ->> 'meta_authority_asset_id' = ?",
			a.tenantOrgID, request.Channel, channelapi.RelayProvider,
			request.ExternalAccountID, statuses,
			"true", metaregistry.ManagementModePlatformOAuth, "instagram_login",
			strings.TrimSpace(a.Config.MetaInstagram.AppID), "instagram_login",
			request.ExternalAccountID,
		).Limit(2).Find(&accounts).Error; err != nil {
			return metaregistry.Binding{}, err
		}
		if len(accounts) != 1 {
			return metaregistry.Binding{}, metaregistry.ErrNotFound
		}
		account = accounts[0]
	} else if err := query.Where(
		"organization_id = ? AND channel = ? AND provider = ? AND external_account_id = ? AND status IN ?",
		a.tenantOrgID, request.Channel, channelapi.RelayProvider, request.ExternalAccountID, statuses,
	).First(&account).Error; err != nil {
		return metaregistry.Binding{}, err
	}
	if account.Channel == models.ChannelMessenger &&
		!a.metaMessengerOrganizationAllowed(account.OrganizationID) {
		// The deployment allowlist is also the pilot runtime kill switch. A
		// removed workspace cannot receive a lease or decrypt credentials for
		// inbound, outbound, worker, or health traffic.
		return metaregistry.Binding{}, metaregistry.ErrStaleBinding
	}
	if account.Channel == models.ChannelInstagram &&
		!a.metaInstagramOrganizationAllowed(account.OrganizationID) {
		return metaregistry.Binding{}, metaregistry.ErrStaleBinding
	}
	if account.Channel == models.ChannelInstagram &&
		metaInstagramDeletionReconciliationPending(account.Metadata) {
		return metaregistry.Binding{}, metaregistry.ErrStaleBinding
	}
	if account.Channel == models.ChannelInstagram &&
		metaDeauthorizationReconciliationPending(account.Metadata) {
		return metaregistry.Binding{}, metaregistry.ErrStaleBinding
	}
	if (account.Channel == models.ChannelMessenger || account.Channel == models.ChannelInstagram) &&
		stringConfigValue(account.Metadata, metaMessengerSubscriptionDesiredStateKey) ==
			metaMessengerSubscriptionDesiredUnsubscribed &&
		stringConfigValue(account.Metadata, metaMessengerSubscriptionOperationStateKey) !=
			metaMessengerSubscriptionUnsubscribeConfirmed {
		// A disconnect claim becomes non-routable before the provider DELETE.
		// Health, inbound, and outbound all stay parked while the remote result
		// is pending or ambiguous; only exact reconciliation can clear it.
		return metaregistry.Binding{}, metaregistry.ErrStaleBinding
	}
	if account.Status == models.ChannelAccountStatusPending && request.Purpose != metaregistry.ResolvePurposeHealth {
		return metaregistry.Binding{}, metaregistry.ErrStaleBinding
	}
	if account.Status == models.ChannelAccountStatusDegraded {
		return metaregistry.Binding{}, metaregistry.ErrStaleBinding
	}
	if request.Purpose == metaregistry.ResolvePurposeOutbound &&
		!boolConfigValue(account.Config, "outbound_enabled") {
		return metaregistry.Binding{}, metaregistry.ErrStaleBinding
	}
	if (request.Purpose == metaregistry.ResolvePurposeInbound ||
		request.Purpose == metaregistry.ResolvePurposeWorker ||
		request.Purpose == metaregistry.ResolvePurposeOutbound) &&
		account.Channel == models.ChannelInstagram {
		if _, ready := metaInstagramSubscribedOperation(account.Metadata); !ready {
			return metaregistry.Binding{}, metaregistry.ErrStaleBinding
		}
	}
	if !boolConfigValue(account.Config, "meta_registry_managed") ||
		stringConfigValue(account.Config, "meta_management_mode") != metaregistry.ManagementModePlatformOAuth ||
		stringConfigValue(account.Metadata, "meta_ownership_state") != metaregistry.OwnershipVerified ||
		stringConfigValue(account.Metadata, "meta_deauthorized_at") != "" {
		return metaregistry.Binding{}, metaregistry.ErrNotFound
	}
	if err := validateMetaRegistryPlatformBinding(&account); err != nil {
		return metaregistry.Binding{}, metaregistry.ErrNotFound
	}
	if !metaAuthorizationTokenAllowed(a.Config, &account) {
		return metaregistry.Binding{}, metaregistry.ErrNotFound
	}
	if account.Channel == models.ChannelMessenger && (a.Config == nil ||
		stringConfigValue(account.Metadata, "meta_platform_app_id") != strings.TrimSpace(a.Config.MetaMessenger.AppID)) {
		return metaregistry.Binding{}, metaregistry.ErrNotFound
	}
	if account.Channel == models.ChannelInstagram {
		if a.Config == nil ||
			stringConfigValue(account.Metadata, "meta_platform_app_id") != strings.TrimSpace(a.Config.MetaInstagram.AppID) ||
			a.metaInstagramReleaseGuardReason(account, account.OrganizationID) != "" {
			return metaregistry.Binding{}, metaregistry.ErrNotFound
		}
	}
	checkedAt, err := time.Parse(time.RFC3339Nano, stringConfigValue(account.Metadata, "meta_ownership_checked_at"))
	if err != nil {
		return metaregistry.Binding{}, metaregistry.ErrStaleBinding
	}
	maxAge := time.Duration(a.Config.MetaRegistry.OwnershipMaxAgeMins) * time.Minute
	if maxAge <= 0 || maxAge > 7*24*time.Hour {
		maxAge = 24 * time.Hour
	}
	if checkedAt.After(now.Add(time.Minute)) || now.Sub(checkedAt) > maxAge {
		return metaregistry.Binding{}, metaregistry.ErrStaleBinding
	}

	oauthCredential := currentMetaRegistryCredential(account.Credentials, models.ChannelCredentialKindOAuth, now)
	webhookCredential := currentMetaRegistryCredential(account.Credentials, models.ChannelCredentialKindWebhook, now)
	if oauthCredential == nil || webhookCredential == nil {
		return metaregistry.Binding{}, metaregistry.ErrNotFound
	}
	if account.Channel == models.ChannelInstagram &&
		!metaInstagramCredentialPairGenerationValid(oauthCredential, webhookCredential, now) {
		return metaregistry.Binding{}, metaregistry.ErrStaleBinding
	}
	if account.Channel == models.ChannelInstagram &&
		!metaInstagramSubscribedOperationMatchesCredentials(
			account.Metadata, *oauthCredential, *webhookCredential,
		) {
		return metaregistry.Binding{}, metaregistry.ErrStaleBinding
	}
	accessToken, err := decryptRequiredMetaRegistrySecret(oauthCredential.CredentialBlob, "access_token", a.Config.App.EncryptionKey)
	if err != nil {
		return metaregistry.Binding{}, err
	}
	inboundSecret, err := decryptRequiredMetaRegistrySecret(webhookCredential.CredentialBlob, "inbound_secret", a.Config.App.EncryptionKey)
	if err != nil {
		return metaregistry.Binding{}, err
	}
	outboundSecret, err := decryptRequiredMetaRegistrySecret(webhookCredential.CredentialBlob, "outbound_secret", a.Config.App.EncryptionKey)
	if err != nil {
		return metaregistry.Binding{}, err
	}
	webhookURL := stringConfigValue(account.Config, "rereply_webhook_url")
	parsed, err := url.Parse(webhookURL)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil ||
		parsed.Fragment != "" || parsed.RawQuery != "" {
		return metaregistry.Binding{}, errors.New("meta registry webhook URL is invalid")
	}
	lease := time.Duration(a.Config.MetaRegistry.LeaseSeconds) * time.Second
	if lease < 5*time.Second || lease > 5*time.Minute {
		lease = 30 * time.Second
	}
	binding := metaregistry.Binding{
		SchemaVersion: metaregistry.SchemaVersion, LeaseID: uuid.New(), LeaseExpiresAt: now.Add(lease),
		OrganizationID: account.OrganizationID, ChannelAccountID: account.ID,
		Channel: account.Channel, ExternalAccountID: account.ExternalAccountID,
		PlatformAppID:     stringConfigValue(account.Metadata, "meta_platform_app_id"),
		InstagramAPIMode:  stringConfigValue(account.Config, "instagram_api_mode"),
		ReReplyWebhookURL: webhookURL, AccessToken: accessToken,
		InboundSecret: inboundSecret, OutboundSecret: outboundSecret,
		CredentialID: oauthCredential.ID, CredentialVersion: oauthCredential.Version,
		WebhookCredentialID: webhookCredential.ID, WebhookCredentialVersion: webhookCredential.Version,
		OwnershipCheckedAt: checkedAt.UTC(),
	}
	if err := binding.Validate(now); err != nil {
		return metaregistry.Binding{}, err
	}
	return binding, nil
}

func metaAuthorizationTokenAllowed(config *configpkg.Config, account *models.ChannelAccount) bool {
	if config == nil || account == nil {
		return false
	}
	kind := strings.ToUpper(strings.TrimSpace(stringConfigValue(account.Metadata, metaMessengerAuthorizationTokenKindKey)))
	if account.Channel == models.ChannelInstagram {
		return kind == metaMessengerTokenKindUser &&
			metaInstagramReleaseEvidenceAllowed(config, account)
	}
	if kind == metaMessengerTokenKindSystemUser {
		return true
	}
	if kind != metaMessengerTokenKindUser || config == nil ||
		!config.MetaMessenger.AllowDevelopmentUserToken || config.MetaMessenger.AllowAllOrganizations {
		return false
	}
	return !strings.EqualFold(strings.TrimSpace(config.App.Environment), "production")
}

func metaInstagramReleaseEvidenceAllowed(
	config *configpkg.Config,
	account *models.ChannelAccount,
) bool {
	if config == nil || account == nil || account.Channel != models.ChannelInstagram ||
		!config.MetaInstagram.Enabled ||
		config.MetaInstagram.QuarantineOnly ||
		strings.TrimSpace(config.MetaInstagram.AllowedOrganizationID) != account.OrganizationID.String() ||
		stringConfigValue(account.Config, "instagram_api_mode") != "instagram_login" ||
		stringConfigValue(account.Metadata, "meta_platform_app_id") != strings.TrimSpace(config.MetaInstagram.AppID) {
		return false
	}
	reviewStatus := strings.ToLower(strings.TrimSpace(config.MetaInstagram.AppReviewStatus))
	mode := stringConfigValue(account.Metadata, "meta_release_evidence_mode")
	if reviewStatus == "approved" {
		return mode == "app_review_approved" &&
			stringConfigValue(account.Metadata, "meta_release_review_status") == "approved"
	}
	if !validMetaInstagramDevelopmentEnvironment(config.App.Environment) ||
		mode != "development_app_role" ||
		!validMetaInstagramDevelopmentRole(config.MetaInstagram.DevelopmentAppRole) {
		return false
	}
	profileID := strings.TrimSpace(config.MetaInstagram.DevelopmentTestProfileID)
	oauthSubjectID := strings.TrimSpace(config.MetaInstagram.DevelopmentTestOAuthSubjectID)
	return validCanonicalMetaID(profileID) && validCanonicalMetaID(oauthSubjectID) &&
		account.ExternalAccountID == profileID &&
		stringConfigValue(account.Metadata, "meta_authorizing_user_id") == oauthSubjectID &&
		stringConfigValue(account.Metadata, "meta_release_profile_id") == profileID &&
		stringConfigValue(account.Metadata, "meta_release_oauth_subject_id") == oauthSubjectID &&
		stringConfigValue(account.Metadata, "meta_release_app_role") ==
			strings.ToLower(strings.TrimSpace(config.MetaInstagram.DevelopmentAppRole)) &&
		stringConfigValue(account.Metadata, "meta_release_review_status") == reviewStatus
}

// metaMessengerAuthorizationTokenAllowed remains the Messenger-specific
// compatibility wrapper used by its existing lifecycle checks.
func metaMessengerAuthorizationTokenAllowed(config *configpkg.Config, metadata models.JSONB) bool {
	kind := strings.ToUpper(strings.TrimSpace(stringConfigValue(metadata, metaMessengerAuthorizationTokenKindKey)))
	if kind == metaMessengerTokenKindSystemUser {
		return true
	}
	if kind != metaMessengerTokenKindUser || config == nil ||
		!config.MetaMessenger.AllowDevelopmentUserToken || config.MetaMessenger.AllowAllOrganizations {
		return false
	}
	return !strings.EqualFold(strings.TrimSpace(config.App.Environment), "production")
}

func (a *App) metaInstagramOrganizationAllowed(organizationID uuid.UUID) bool {
	return a != nil && a.Config != nil && a.Config.MetaInstagram.Enabled &&
		organizationID != uuid.Nil &&
		strings.TrimSpace(a.Config.MetaInstagram.AllowedOrganizationID) == organizationID.String()
}

func currentMetaRegistryCredential(credentials []models.ChannelCredential, kind models.ChannelCredentialKind, now time.Time) *models.ChannelCredential {
	for _, index := range channelapi.CredentialIndexesByPriority(credentials) {
		credential := &credentials[index]
		if credential.Kind == kind && channelapi.CredentialIsCurrent(credential, now) {
			return credential
		}
	}
	return nil
}

func decryptRequiredMetaRegistrySecret(blob models.JSONB, key, encryptionKey string) (string, error) {
	ciphertext, _ := blob[key].(string)
	if strings.TrimSpace(ciphertext) == "" || !appcrypto.IsEncrypted(ciphertext) || strings.TrimSpace(encryptionKey) == "" {
		return "", errors.New("encrypted Meta registry secret is unavailable")
	}
	plaintext, err := appcrypto.Decrypt(ciphertext, encryptionKey)
	if err != nil || strings.TrimSpace(plaintext) == "" {
		return "", errors.New("encrypted Meta registry secret is unavailable")
	}
	return plaintext, nil
}

func (a *App) applyMetaRegistryMutation(request metaregistry.MutationRequest, outcome string) (bool, error) {
	if a == nil || a.DB == nil || a.tenantOrgID == uuid.Nil {
		return false, metaregistry.ErrInvalidRequest
	}
	if err := lockChannelAIOrganizationScopeTx(a.DB, a.tenantOrgID); err != nil {
		return false, err
	}
	var account models.ChannelAccount
	if err := a.DB.Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("id = ? AND organization_id = ? AND provider = ? AND channel IN ?",
			request.ChannelAccountID, a.tenantOrgID, channelapi.RelayProvider,
			[]models.Channel{models.ChannelMessenger, models.ChannelInstagram}).
		First(&account).Error; err != nil {
		return false, err
	}
	if !boolConfigValue(account.Config, "meta_registry_managed") {
		return false, metaregistry.ErrNotFound
	}
	if previousCheckedAt, err := time.Parse(
		time.RFC3339Nano,
		stringConfigValue(account.Metadata, "meta_ownership_checked_at"),
	); err != nil || !request.CheckedAt.After(previousCheckedAt) {
		return false, metaregistry.ErrNotFound
	}
	var currentCredentials []models.ChannelCredential
	if err := a.DB.Clauses(clause.Locking{Strength: "UPDATE"}).Where(
		"organization_id = ? AND channel_account_id = ? AND status IN ?",
		a.tenantOrgID, account.ID,
		[]models.ChannelCredentialStatus{
			models.ChannelCredentialStatusActive,
			models.ChannelCredentialStatusExpiring,
		},
	).Order("version DESC").Find(&currentCredentials).Error; err != nil {
		return false, err
	}
	now := time.Now().UTC()
	oauth := currentMetaRegistryCredential(
		currentCredentials, models.ChannelCredentialKindOAuth, now,
	)
	webhook := currentMetaRegistryCredential(
		currentCredentials, models.ChannelCredentialKindWebhook, now,
	)
	if account.Channel == models.ChannelInstagram {
		account.Credentials = currentCredentials
		if !a.metaInstagramRegistryMutationBindingValid(
			&account, oauth, webhook, request, now,
		) {
			return false, metaregistry.ErrNotFound
		}
	} else if oauth == nil || webhook == nil || oauth.ID != request.CredentialID ||
		oauth.Version != request.CredentialVersion || webhook.ID != request.WebhookCredentialID ||
		webhook.Version != request.WebhookCredentialVersion {
		return false, metaregistry.ErrNotFound
	}
	credentials := []models.ChannelCredential{*oauth, *webhook}
	if account.Channel == models.ChannelInstagram && outcome == metaregistry.OwnershipVerified {
		if a.metaInstagramReleaseGuardReason(account, a.tenantOrgID) != "" ||
			!metaInstagramSubscribedOperationMatchesCredentials(
				account.Metadata, *oauth, *webhook,
			) {
			return false, metaregistry.ErrNotFound
		}
	}
	metadata := cloneJSONB(account.Metadata)
	metadata["meta_ownership_state"] = outcome
	metadata["meta_ownership_checked_at"] = request.CheckedAt.Format(time.RFC3339Nano)
	if request.Reason != "" {
		metadata["meta_ownership_reason"] = request.Reason
	}
	newStatus := account.Status
	accountConfig := cloneJSONB(account.Config)
	switch outcome {
	case metaregistry.OwnershipRevoked:
		metadata["meta_deauthorized_at"] = request.CheckedAt.Format(time.RFC3339Nano)
		newStatus = models.ChannelAccountStatusDisconnected
		accountConfig["outbound_enabled"] = false
		accountConfig["ai_reply_enabled"] = false
		for i := range credentials {
			result := a.DB.Model(&models.ChannelCredential{}).
				Where("id = ? AND organization_id = ? AND version = ? AND status IN ?",
					credentials[i].ID, a.tenantOrgID, credentials[i].Version,
					[]models.ChannelCredentialStatus{models.ChannelCredentialStatusActive, models.ChannelCredentialStatusExpiring}).
				Updates(map[string]any{"status": models.ChannelCredentialStatusRevoked, "revoked_at": request.CheckedAt})
			if result.Error != nil {
				return false, result.Error
			}
			if result.RowsAffected != 1 {
				return false, metaregistry.ErrNotFound
			}
		}
	case metaregistry.OwnershipStale:
		newStatus = models.ChannelAccountStatusDegraded
		accountConfig["outbound_enabled"] = false
		accountConfig["ai_reply_enabled"] = false
	case metaregistry.OwnershipVerified:
		if account.Status == models.ChannelAccountStatusDegraded {
			newStatus = models.ChannelAccountStatusPending
			accountConfig["outbound_enabled"] = false
			accountConfig["ai_reply_enabled"] = false
		}
	}
	updates := map[string]any{"metadata": metadata, "status": newStatus, "config": accountConfig}
	if result := a.DB.Model(&models.ChannelAccount{}).
		Where("id = ? AND organization_id = ?", account.ID, a.tenantOrgID).
		Updates(updates); result.Error != nil || result.RowsAffected != 1 {
		if result.Error != nil {
			return false, result.Error
		}
		return false, metaregistry.ErrNotFound
	}
	if account.Channel == models.ChannelInstagram &&
		(outcome == metaregistry.OwnershipStale ||
			outcome == metaregistry.OwnershipRevoked ||
			(outcome == metaregistry.OwnershipVerified &&
				account.Status == models.ChannelAccountStatusDegraded)) {
		if err := cancelManagedMetaQueuedWorkForAccountTx(
			a.DB, a.tenantOrgID, account.ID, "managed_instagram_registry_mutation",
		); err != nil {
			return false, err
		}
	}
	actorID := uuid.Nil
	if account.UpdatedByID != nil {
		actorID = *account.UpdatedByID
	} else if account.CreatedByID != nil {
		actorID = *account.CreatedByID
	}
	if actorID == uuid.Nil {
		var membership models.UserOrganization
		if err := a.DB.Where("organization_id = ?", a.tenantOrgID).Order("created_at").First(&membership).Error; err != nil {
			return false, err
		}
		actorID = membership.UserID
	}
	if err := audit.LogAudit(
		a.DB, a.tenantOrgID, actorID, metaRegistryAuditActor,
		"meta_channel_registry", account.ID, models.AuditActionUpdated,
		nil, nil,
		map[string]any{"field": "ownership_state", "old_value": stringConfigValue(account.Metadata, "meta_ownership_state"), "new_value": outcome},
		map[string]any{"field": "credential_version_fence", "old_value": nil, "new_value": fmt.Sprintf("%d/%d", request.CredentialVersion, request.WebhookCredentialVersion)},
	); err != nil {
		return false, err
	}
	return true, nil
}

// metaInstagramRegistryMutationBindingValid is the common persistence fence
// for verified, stale, and revoked registry outcomes. Release downgrades and
// quarantine must still be able to disable an exact existing binding, but
// they must not grant the registry authority over a foreign or drifted row.
func (a *App) metaInstagramRegistryMutationBindingValid(
	account *models.ChannelAccount,
	oauth, webhook *models.ChannelCredential,
	request metaregistry.MutationRequest,
	now time.Time,
) bool {
	configuredProfileID := ""
	configuredOAuthSubjectID := ""
	if a != nil && a.Config != nil {
		configuredProfileID = strings.TrimSpace(
			a.Config.MetaInstagram.DevelopmentTestProfileID,
		)
		configuredOAuthSubjectID = strings.TrimSpace(
			a.Config.MetaInstagram.DevelopmentTestOAuthSubjectID,
		)
	}
	if a == nil || a.Config == nil || account == nil ||
		account.OrganizationID != a.tenantOrgID ||
		!a.metaInstagramOrganizationAllowed(account.OrganizationID) ||
		(configuredProfileID != "" &&
			account.ExternalAccountID != configuredProfileID) ||
		(configuredOAuthSubjectID != "" &&
			stringConfigValue(account.Metadata, "meta_authorizing_user_id") != configuredOAuthSubjectID) ||
		!exactManagedInstagramCallbackBinding(
			account,
			strings.TrimSpace(a.Config.MetaInstagram.AppID),
			stringConfigValue(account.Metadata, "meta_authorizing_user_id"),
		) ||
		!metaInstagramCredentialPairGenerationValid(oauth, webhook, now) ||
		oauth.ID != request.CredentialID ||
		oauth.Version != request.CredentialVersion ||
		webhook.ID != request.WebhookCredentialID ||
		webhook.Version != request.WebhookCredentialVersion {
		return false
	}
	currentGenerationCount := 0
	for index := range account.Credentials {
		if channelapi.CredentialIsCurrent(&account.Credentials[index], now) {
			currentGenerationCount++
		}
	}
	return currentGenerationCount == 2
}

func validateMetaRegistryPlatformBinding(account *models.ChannelAccount) error {
	if account == nil || !exactMetaRegistryControlPlaneConfig(account.Config) {
		return metaregistry.ErrNotFound
	}
	mode := stringConfigValue(account.Config, "instagram_api_mode")
	wantApp := "messenger"
	requiredScopes := metaMessengerRequiredScopes
	if account.Channel == models.ChannelInstagram {
		switch mode {
		case "instagram_login":
			wantApp = "instagram_login"
			requiredScopes = []string{"instagram_business_basic", "instagram_business_manage_messages"}
		case "facebook_login":
			requiredScopes = []string{"instagram_basic", "instagram_manage_messages", "pages_manage_metadata"}
		default:
			return metaregistry.ErrNotFound
		}
	} else if account.Channel != models.ChannelMessenger || mode != "" {
		return metaregistry.ErrNotFound
	}
	if stringConfigValue(account.Metadata, "meta_webhook_app") != wantApp ||
		stringConfigValue(account.Metadata, "meta_platform_app_id") == "" ||
		stringConfigValue(account.Metadata, "meta_authorizing_user_id") == "" {
		return metaregistry.ErrNotFound
	}
	if account.Channel == models.ChannelMessenger &&
		stringConfigValue(account.Metadata, "meta_business_id") == "" {
		return metaregistry.ErrNotFound
	}
	if account.Channel == models.ChannelInstagram {
		if !validCanonicalMetaID(account.ExternalAccountID) ||
			!validCanonicalMetaID(stringConfigValue(account.Metadata, "meta_authorizing_user_id")) ||
			stringConfigValue(account.Metadata, metaInstagramOAuthSubjectIDKey) !=
				stringConfigValue(account.Metadata, "meta_authorizing_user_id") ||
			stringConfigValue(account.Metadata, "meta_authority_asset_id") != account.ExternalAccountID {
			return metaregistry.ErrNotFound
		}
	}
	granted := make(map[string]bool)
	switch values := account.Metadata["meta_granted_scopes"].(type) {
	case []string:
		for _, value := range values {
			granted[strings.TrimSpace(value)] = true
		}
	case []any:
		for _, raw := range values {
			value, _ := raw.(string)
			granted[strings.TrimSpace(value)] = true
		}
	default:
		return metaregistry.ErrNotFound
	}
	for _, required := range requiredScopes {
		if !granted[required] {
			return metaregistry.ErrNotFound
		}
	}
	return nil
}
