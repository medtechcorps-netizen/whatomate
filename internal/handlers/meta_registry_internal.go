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
	var organizationID uuid.UUID
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
	if err := root.DB.Model(&models.ChannelAccount{}).
		Select("organization_id").
		Where("channel = ? AND provider = ? AND external_account_id = ? AND status IN ?",
			request.Channel, channelapi.RelayProvider, request.ExternalAccountID,
			[]models.ChannelAccountStatus{models.ChannelAccountStatusActive, models.ChannelAccountStatusDegraded}).
		Take(&organizationID).Error; err != nil {
		return uuid.Nil, err
	}
	return organizationID, nil
}

func (a *App) resolveChannelAccountOrganization(accountID uuid.UUID) (uuid.UUID, error) {
	root := a.rootApp()
	var organizationID uuid.UUID
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
	if err := root.DB.Model(&models.ChannelAccount{}).Select("organization_id").
		Where("id = ?", accountID).Take(&organizationID).Error; err != nil {
		return uuid.Nil, err
	}
	return organizationID, nil
}

func (a *App) loadMetaRegistryBinding(request metaregistry.ResolveRequest, now time.Time) (metaregistry.Binding, error) {
	var account models.ChannelAccount
	if err := a.DB.Preload("Credentials", func(db *gorm.DB) *gorm.DB {
		return db.Order("version DESC")
	}).Where(
		"organization_id = ? AND channel = ? AND provider = ? AND external_account_id = ? AND status IN ?",
		a.tenantOrgID, request.Channel, channelapi.RelayProvider, request.ExternalAccountID,
		[]models.ChannelAccountStatus{models.ChannelAccountStatusActive, models.ChannelAccountStatusDegraded},
	).First(&account).Error; err != nil {
		return metaregistry.Binding{}, err
	}
	if account.Status == models.ChannelAccountStatusDegraded {
		return metaregistry.Binding{}, metaregistry.ErrStaleBinding
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
		return metaregistry.Binding{}, errors.New("Meta registry webhook URL is invalid")
	}
	lease := time.Duration(a.Config.MetaRegistry.LeaseSeconds) * time.Second
	if lease < 5*time.Second || lease > 5*time.Minute {
		lease = 30 * time.Second
	}
	binding := metaregistry.Binding{
		SchemaVersion: metaregistry.SchemaVersion, LeaseID: uuid.New(), LeaseExpiresAt: now.Add(lease),
		OrganizationID: account.OrganizationID, ChannelAccountID: account.ID,
		Channel: account.Channel, ExternalAccountID: account.ExternalAccountID,
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
	var credentials []models.ChannelCredential
	if err := a.DB.Clauses(clause.Locking{Strength: "UPDATE"}).Where(
		"organization_id = ? AND channel_account_id = ? AND status IN ? AND ((id = ? AND version = ? AND kind = ?) OR (id = ? AND version = ? AND kind = ?))",
		a.tenantOrgID, account.ID,
		[]models.ChannelCredentialStatus{models.ChannelCredentialStatusActive, models.ChannelCredentialStatusExpiring},
		request.CredentialID, request.CredentialVersion, models.ChannelCredentialKindOAuth,
		request.WebhookCredentialID, request.WebhookCredentialVersion, models.ChannelCredentialKindWebhook,
	).Find(&credentials).Error; err != nil {
		return false, err
	}
	if len(credentials) != 2 {
		return false, metaregistry.ErrNotFound
	}
	metadata := cloneJSONB(account.Metadata)
	metadata["meta_ownership_state"] = outcome
	metadata["meta_ownership_checked_at"] = request.CheckedAt.Format(time.RFC3339Nano)
	if request.Reason != "" {
		metadata["meta_ownership_reason"] = request.Reason
	}
	newStatus := account.Status
	if outcome == metaregistry.OwnershipRevoked {
		metadata["meta_deauthorized_at"] = request.CheckedAt.Format(time.RFC3339Nano)
		newStatus = models.ChannelAccountStatusDisconnected
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
	} else if outcome == metaregistry.OwnershipStale {
		newStatus = models.ChannelAccountStatusDegraded
	} else if outcome == metaregistry.OwnershipVerified {
		newStatus = models.ChannelAccountStatusActive
	}
	updates := map[string]any{"metadata": metadata, "status": newStatus}
	if result := a.DB.Model(&models.ChannelAccount{}).
		Where("id = ? AND organization_id = ?", account.ID, a.tenantOrgID).
		Updates(updates); result.Error != nil || result.RowsAffected != 1 {
		if result.Error != nil {
			return false, result.Error
		}
		return false, metaregistry.ErrNotFound
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

func validateMetaRegistryPlatformBinding(account *models.ChannelAccount) error {
	if account == nil {
		return metaregistry.ErrNotFound
	}
	mode := stringConfigValue(account.Config, "instagram_api_mode")
	wantApp := "messenger"
	requiredScopes := []string{"pages_manage_metadata", "pages_read_engagement", "pages_messaging"}
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
		stringConfigValue(account.Metadata, "meta_business_id") == "" ||
		stringConfigValue(account.Metadata, "meta_authorizing_user_id") == "" {
		return metaregistry.ErrNotFound
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
