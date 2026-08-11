package handlers

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/valyala/fasthttp"
	"github.com/zerodha/fastglue"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	threadsAuthorizationURL            = "https://threads.com/oauth/authorize"
	threadsGraphBaseURL                = "https://graph.threads.net"
	threadsOAuthStatePrefix            = "integration:threads:oauth:state:"
	threadsOAuthStateTTL               = 10 * time.Minute
	threadsOAuthHTTPTimeout            = 30 * time.Second
	threadsOAuthMaxResponseBytes int64 = 1 << 20
)

var (
	threadsRequiredScopes        = channelapi.RequiredThreadsScopes()
	errThreadsOAuthForbidden     = errors.New("threads authorization is no longer permitted")
	errThreadsOAuthStale         = errors.New("threads integration settings changed during authorization")
	errThreadsOAuthNotConfigured = errors.New("threads integration is not configured")
	errThreadsOAuthAccountBound  = errors.New("the dedicated Threads app is already bound to another Threads profile")
)

type threadsOAuthState struct {
	OrganizationID         string    `json:"organization_id"`
	UserID                 string    `json:"user_id"`
	Nonce                  string    `json:"nonce"`
	IntegrationID          string    `json:"integration_id"`
	IntegrationFingerprint string    `json:"integration_fingerprint"`
	ExpiresAt              time.Time `json:"expires_at"`
}

type threadsIntegrationSnapshot struct {
	IntegrationID      uuid.UUID
	AppID              string
	RedirectURI        string
	EncryptedAppSecret string
	AppSecret          string
	Fingerprint        string
}

type threadsTokenResponse struct {
	AccessToken string    `json:"access_token"`
	UserID      threadsID `json:"user_id"`
	TokenType   string    `json:"token_type"`
	ExpiresIn   int64     `json:"expires_in"`
}

type threadsOAuthProviderError struct {
	StatusCode int
	Code       int
	Subcode    int
	Type       string
	TraceID    string
	RequestID  string
}

func (e *threadsOAuthProviderError) Error() string {
	if e == nil {
		return "threads provider request failed"
	}
	parts := []string{fmt.Sprintf("HTTP %d", e.StatusCode)}
	if e.Code != 0 {
		parts = append(parts, fmt.Sprintf("code %d", e.Code))
	}
	if e.Subcode != 0 {
		parts = append(parts, fmt.Sprintf("subcode %d", e.Subcode))
	}
	if e.Type != "" {
		parts = append(parts, "type "+e.Type)
	}
	if e.TraceID != "" {
		parts = append(parts, "trace "+e.TraceID)
	}
	if e.RequestID != "" {
		parts = append(parts, "request "+e.RequestID)
	}
	return "threads provider request failed (" + strings.Join(parts, ", ") + ")"
}

type threadsGraphErrorEnvelope struct {
	Error struct {
		Type         string `json:"type"`
		Code         int    `json:"code"`
		ErrorSubcode int    `json:"error_subcode"`
		FBTraceID    string `json:"fbtrace_id"`
	} `json:"error"`
}

type threadsProfile struct {
	ID                threadsID `json:"id"`
	Username          string    `json:"username"`
	Name              string    `json:"name"`
	ProfilePictureURL string    `json:"threads_profile_picture_url"`
}

type threadsID string

func (id *threadsID) UnmarshalJSON(value []byte) error {
	trimmed := strings.TrimSpace(string(value))
	if trimmed == "" || trimmed == "null" {
		*id = ""
		return nil
	}
	if strings.HasPrefix(trimmed, "\"") {
		var text string
		if err := json.Unmarshal(value, &text); err != nil {
			return err
		}
		text = strings.TrimSpace(text)
		if !validThreadsOAuthID(text) {
			return errors.New("threads ID is invalid")
		}
		*id = threadsID(text)
		return nil
	}
	if !validThreadsOAuthID(trimmed) {
		return errors.New("threads ID is invalid")
	}
	*id = threadsID(trimmed)
	return nil
}

func validThreadsOAuthID(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			return false
		}
	}
	return true
}

func (a *App) threadsOAuthAvailable() bool {
	return a != nil && a.Redis != nil && a.hasIntegrationEncryptionKey()
}

func (a *App) startThreadsOAuth(r *fastglue.Request, orgID, userID uuid.UUID) error {
	if !a.threadsOAuthAvailable() {
		return r.SendErrorEnvelope(fasthttp.StatusServiceUnavailable, "Threads OAuth state or credential storage is unavailable", nil, "")
	}
	allowed, err := a.HasProductEntitlement(userID, orgID, channelapi.ThreadsPublicEngagementEntitlementKey)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusServiceUnavailable, "Threads public engagement entitlement could not be evaluated", nil, "")
	}
	if !allowed {
		return r.SendErrorEnvelope(fasthttp.StatusPaymentRequired, "Threads public replies are not included in this workspace's active plan", nil, "")
	}
	snapshot, err := a.loadThreadsIntegrationSnapshot(orgID, true)
	if err != nil {
		if errors.Is(err, errThreadsOAuthNotConfigured) {
			return r.SendErrorEnvelope(fasthttp.StatusConflict, "Enable Threads and configure its App ID, redirect URI, and App Secret first", nil, "")
		}
		return r.SendErrorEnvelope(fasthttp.StatusServiceUnavailable, "Threads credentials are unavailable", nil, "")
	}
	nonce := generateRandomString(48)
	state := threadsOAuthState{
		OrganizationID:         orgID.String(),
		UserID:                 userID.String(),
		Nonce:                  nonce,
		IntegrationID:          snapshot.IntegrationID.String(),
		IntegrationFingerprint: snapshot.Fingerprint,
		ExpiresAt:              time.Now().UTC().Add(threadsOAuthStateTTL),
	}
	payload, err := json.Marshal(state)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to prepare Threads authorization", nil, "")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := a.Redis.Set(ctx, threadsOAuthStatePrefix+nonce, payload, threadsOAuthStateTTL).Err(); err != nil {
		a.Log.Error("Failed to store Threads OAuth state", "error", err, "organization_id", orgID)
		return r.SendErrorEnvelope(fasthttp.StatusServiceUnavailable, "OAuth state storage is unavailable", nil, "")
	}
	authorization, _ := url.Parse(threadsAuthorizationURL)
	query := authorization.Query()
	query.Set("client_id", snapshot.AppID)
	query.Set("redirect_uri", snapshot.RedirectURI)
	query.Set("response_type", "code")
	query.Set("scope", strings.Join(threadsRequiredScopes, ","))
	query.Set("state", nonce)
	authorization.RawQuery = query.Encode()
	r.RequestCtx.Response.Header.Set("Cache-Control", "no-store")
	return r.SendEnvelope(map[string]any{
		"provider":          integrationProviderThreads,
		"ready":             true,
		"mode":              "oauth",
		"authorization_url": authorization.String(),
	})
}

// CallbackThreads completes OAuth without normal request authentication. Its
// one-time Redis state binds both the tenant and initiating admin, and the
// integration fingerprint prevents a mid-flow app credential swap.
func (a *App) CallbackThreads(r *fastglue.Request) error {
	r.RequestCtx.Response.Header.Set("Cache-Control", "no-store")
	stateNonce := strings.TrimSpace(string(r.RequestCtx.QueryArgs().Peek("state")))
	if stateNonce == "" || len(stateNonce) > 128 || a.Redis == nil {
		a.redirectThreadsCallback(r, "error")
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	stateJSON, err := a.Redis.GetDel(ctx, threadsOAuthStatePrefix+stateNonce).Bytes()
	cancel()
	if err != nil {
		a.redirectThreadsCallback(r, "error")
		return nil
	}
	var state threadsOAuthState
	if err := json.Unmarshal(stateJSON, &state); err != nil || state.Nonce != stateNonce ||
		state.IntegrationFingerprint == "" || time.Now().UTC().After(state.ExpiresAt) {
		a.redirectThreadsCallback(r, "error")
		return nil
	}
	orgID, orgErr := uuid.Parse(state.OrganizationID)
	userID, userErr := uuid.Parse(state.UserID)
	integrationID, integrationErr := uuid.Parse(state.IntegrationID)
	if orgErr != nil || userErr != nil || integrationErr != nil || orgID == uuid.Nil || userID == uuid.Nil || integrationID == uuid.Nil {
		a.redirectThreadsCallback(r, "error")
		return nil
	}
	if providerError := strings.TrimSpace(string(r.RequestCtx.QueryArgs().Peek("error"))); providerError != "" {
		a.redirectThreadsCallback(r, "cancelled")
		return nil
	}
	code := strings.TrimSpace(string(r.RequestCtx.QueryArgs().Peek("code")))
	if code == "" || len(code) > 8192 {
		a.redirectThreadsCallback(r, "error")
		return nil
	}

	var snapshot threadsIntegrationSnapshot
	err = a.WithTenantApp(orgID, func(scoped *App) error {
		if !scoped.HasPermission(userID, models.ResourceSettingsIntegrations, models.ActionWrite, orgID) {
			return errThreadsOAuthForbidden
		}
		allowed, entitlementErr := scoped.HasProductEntitlement(userID, orgID, channelapi.ThreadsPublicEngagementEntitlementKey)
		if entitlementErr != nil || !allowed {
			return errThreadsOAuthForbidden
		}
		loaded, loadErr := scoped.loadThreadsIntegrationSnapshot(orgID, true)
		if loadErr != nil {
			return loadErr
		}
		if loaded.IntegrationID != integrationID || loaded.Fingerprint != state.IntegrationFingerprint {
			return errThreadsOAuthStale
		}
		snapshot = loaded
		return nil
	})
	if err != nil {
		a.redirectThreadsCallback(r, "error")
		return nil
	}

	exchangeCtx, exchangeCancel := context.WithTimeout(context.Background(), threadsOAuthHTTPTimeout)
	defer exchangeCancel()
	shortToken, err := a.exchangeThreadsAuthorizationCode(exchangeCtx, snapshot, code)
	if err != nil {
		a.Log.Error("Threads OAuth code exchange failed", "error", err, "organization_id", orgID)
		a.redirectThreadsCallback(r, "error")
		return nil
	}
	longToken, err := a.exchangeThreadsLongLivedToken(exchangeCtx, snapshot.AppSecret, shortToken.AccessToken)
	if err != nil {
		a.Log.Error("Threads long-lived token exchange failed", "error", err, "organization_id", orgID)
		a.redirectThreadsCallback(r, "error")
		return nil
	}
	permissionAdapter := channelapi.NewThreadsAdapter(a.HTTPClient, a.integrationEncryptionKey())
	permissions, err := permissionAdapter.InspectTokenPermissions(
		exchangeCtx,
		longToken.AccessToken,
		strings.TrimSpace(string(shortToken.UserID)),
	)
	if err != nil {
		a.Log.Error("Threads OAuth permission verification failed", "organization_id", orgID)
		a.redirectThreadsCallback(r, "error")
		return nil
	}
	profile, err := a.fetchThreadsProfile(exchangeCtx, longToken.AccessToken)
	if err != nil || strings.TrimSpace(string(profile.ID)) == "" {
		a.Log.Error("Threads profile discovery failed", "organization_id", orgID)
		a.redirectThreadsCallback(r, "error")
		return nil
	}
	if shortID := strings.TrimSpace(string(shortToken.UserID)); shortID != "" && shortID != strings.TrimSpace(string(profile.ID)) {
		a.redirectThreadsCallback(r, "error")
		return nil
	}
	if longToken.ExpiresIn <= 0 {
		longToken.ExpiresIn = 60 * 24 * 60 * 60
	}
	encryptedToken, err := appcrypto.Encrypt(strings.TrimSpace(longToken.AccessToken), a.integrationEncryptionKey())
	if err != nil || !appcrypto.IsEncrypted(encryptedToken) {
		a.redirectThreadsCallback(r, "error")
		return nil
	}

	err = a.WithTenantApp(orgID, func(scoped *App) error {
		if !scoped.HasPermission(userID, models.ResourceSettingsIntegrations, models.ActionWrite, orgID) {
			return errThreadsOAuthForbidden
		}
		allowed, entitlementErr := scoped.HasProductEntitlement(userID, orgID, channelapi.ThreadsPublicEngagementEntitlementKey)
		if entitlementErr != nil || !allowed {
			return errThreadsOAuthForbidden
		}
		return scoped.persistThreadsConnection(
			orgID,
			userID,
			integrationID,
			state.IntegrationFingerprint,
			profile,
			encryptedToken,
			longToken.ExpiresIn,
			permissions,
		)
	})
	if err != nil {
		a.Log.Error("Failed to persist Threads authorization", "error", err, "organization_id", orgID)
		a.redirectThreadsCallback(r, "error")
		return nil
	}
	a.redirectThreadsCallback(r, "connected")
	return nil
}

func (a *App) loadThreadsIntegrationSnapshot(orgID uuid.UUID, requireEnabled bool) (threadsIntegrationSnapshot, error) {
	var row models.ProviderIntegration
	query := a.DB.Where("organization_id = ? AND provider = ?", orgID, integrationProviderThreads)
	if requireEnabled {
		query = query.Where("enabled = ?", true)
	}
	if err := query.First(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return threadsIntegrationSnapshot{}, errThreadsOAuthNotConfigured
		}
		return threadsIntegrationSnapshot{}, err
	}
	appID := stringJSONValue(row.Config, "app_id")
	redirectURI := stringJSONValue(row.Config, "redirect_uri")
	encryptedSecret, _ := row.CredentialData["app_secret"].(string)
	if appID == "" || row.ThreadsAppID == nil || strings.TrimSpace(*row.ThreadsAppID) != appID ||
		redirectURI == "" || !appcrypto.IsEncrypted(strings.TrimSpace(encryptedSecret)) || validateIntegrationRedirectURI(redirectURI) != nil {
		return threadsIntegrationSnapshot{}, errThreadsOAuthNotConfigured
	}
	secret, err := appcrypto.Decrypt(encryptedSecret, a.integrationEncryptionKey())
	if err != nil || strings.TrimSpace(secret) == "" {
		return threadsIntegrationSnapshot{}, errThreadsOAuthNotConfigured
	}
	fingerprint := sha256.Sum256([]byte(strings.Join([]string{
		row.ID.String(),
		appID,
		redirectURI,
		encryptedSecret,
		row.UpdatedAt.UTC().Format(time.RFC3339Nano),
	}, "\x00")))
	return threadsIntegrationSnapshot{
		IntegrationID:      row.ID,
		AppID:              appID,
		RedirectURI:        redirectURI,
		EncryptedAppSecret: encryptedSecret,
		AppSecret:          strings.TrimSpace(secret),
		Fingerprint:        hex.EncodeToString(fingerprint[:]),
	}, nil
}

func (a *App) threadsWebhookCredentials(orgID uuid.UUID) (string, string, error) {
	if a.rlsEnabled() && !a.hasTenantScope() {
		var appSecret, verifyToken string
		err := a.WithTenantApp(orgID, func(scoped *App) error {
			var resolveErr error
			appSecret, verifyToken, resolveErr = scoped.threadsWebhookCredentials(orgID)
			return resolveErr
		})
		return appSecret, verifyToken, err
	}
	if !a.hasIntegrationEncryptionKey() {
		return "", "", errThreadsOAuthNotConfigured
	}
	var row models.ProviderIntegration
	if err := a.DB.Where(
		"organization_id = ? AND provider = ? AND enabled = ?",
		orgID,
		integrationProviderThreads,
		true,
	).First(&row).Error; err != nil {
		return "", "", err
	}
	decrypt := func(name string) (string, error) {
		stored, _ := row.CredentialData[name].(string)
		if !appcrypto.IsEncrypted(strings.TrimSpace(stored)) {
			return "", errThreadsOAuthNotConfigured
		}
		plaintext, err := appcrypto.Decrypt(stored, a.integrationEncryptionKey())
		if err != nil || strings.TrimSpace(plaintext) == "" {
			return "", errThreadsOAuthNotConfigured
		}
		return strings.TrimSpace(plaintext), nil
	}
	appSecret, err := decrypt("app_secret")
	if err != nil {
		return "", "", err
	}
	verifyToken, err := decrypt("webhook_verify_token")
	if err != nil {
		return "", "", err
	}
	return appSecret, verifyToken, nil
}

func (a *App) exchangeThreadsAuthorizationCode(
	ctx context.Context,
	snapshot threadsIntegrationSnapshot,
	code string,
) (threadsTokenResponse, error) {
	values := url.Values{
		"client_id":     {snapshot.AppID},
		"client_secret": {snapshot.AppSecret},
		"code":          {code},
		"grant_type":    {"authorization_code"},
		"redirect_uri":  {snapshot.RedirectURI},
	}
	var response threadsTokenResponse
	err := a.doThreadsOAuthJSON(ctx, http.MethodPost, threadsGraphBaseURL+"/oauth/access_token", values, "", &response)
	if err != nil {
		return threadsTokenResponse{}, fmt.Errorf("threads code exchange: %w", err)
	}
	if strings.TrimSpace(response.AccessToken) == "" || strings.TrimSpace(string(response.UserID)) == "" {
		return threadsTokenResponse{}, errors.New("threads code exchange failed")
	}
	return response, nil
}

func (a *App) exchangeThreadsLongLivedToken(
	ctx context.Context,
	appSecret, shortAccessToken string,
) (threadsTokenResponse, error) {
	values := url.Values{
		"grant_type":    {"th_exchange_token"},
		"client_secret": {appSecret},
		"access_token":  {shortAccessToken},
	}
	var response threadsTokenResponse
	err := a.doThreadsOAuthJSON(ctx, http.MethodGet, threadsGraphBaseURL+"/access_token", values, "", &response)
	if err != nil {
		return threadsTokenResponse{}, fmt.Errorf("threads long-lived token exchange: %w", err)
	}
	if strings.TrimSpace(response.AccessToken) == "" {
		return threadsTokenResponse{}, errors.New("threads long-lived token exchange failed")
	}
	return response, nil
}

func (a *App) fetchThreadsProfile(ctx context.Context, accessToken string) (threadsProfile, error) {
	var profile threadsProfile
	err := a.doThreadsOAuthJSON(ctx, http.MethodGet, threadsGraphBaseURL+"/v1.0/me", url.Values{
		"fields": {"id,username,name,threads_profile_picture_url"},
	}, accessToken, &profile)
	return profile, err
}

func (a *App) doThreadsOAuthJSON(
	ctx context.Context,
	method, endpoint string,
	values url.Values,
	accessToken string,
	destination any,
) error {
	target, err := url.Parse(endpoint)
	if err != nil || target.Scheme != "https" || target.Host == "" {
		return errors.New("threads endpoint is invalid")
	}
	var body io.Reader
	if method == http.MethodPost {
		body = bytes.NewBufferString(values.Encode())
	} else {
		target.RawQuery = values.Encode()
	}
	request, err := http.NewRequestWithContext(ctx, method, target.String(), body)
	if err != nil {
		return errors.New("threads request could not be prepared")
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "ReReply-Threads-OAuth/1.0")
	if method == http.MethodPost {
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if strings.TrimSpace(accessToken) != "" {
		request.Header.Set("Authorization", "Bearer "+strings.TrimSpace(accessToken))
	}
	client := oauthHTTPClientWithoutRedirects(a.HTTPClient)
	client.Timeout = threadsOAuthHTTPTimeout
	response, err := client.Do(request)
	if err != nil {
		return errors.New("threads request failed")
	}
	defer func() { _ = response.Body.Close() }()
	payload, err := io.ReadAll(io.LimitReader(response.Body, threadsOAuthMaxResponseBytes+1))
	if err != nil || int64(len(payload)) > threadsOAuthMaxResponseBytes {
		return errors.New("threads response was invalid")
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		var envelope threadsGraphErrorEnvelope
		_ = json.Unmarshal(payload, &envelope)
		traceID := safeThreadsOAuthDiagnostic(envelope.Error.FBTraceID)
		if traceID == "" {
			traceID = safeThreadsOAuthDiagnostic(response.Header.Get("X-FB-Trace-ID"))
		}
		return &threadsOAuthProviderError{
			StatusCode: response.StatusCode,
			Code:       envelope.Error.Code,
			Subcode:    envelope.Error.ErrorSubcode,
			Type:       safeThreadsOAuthDiagnostic(envelope.Error.Type),
			TraceID:    traceID,
			RequestID:  safeThreadsOAuthDiagnostic(response.Header.Get("X-FB-Request-ID")),
		}
	}
	if destination == nil {
		return nil
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	if err := decoder.Decode(destination); err != nil {
		return errors.New("threads response was invalid")
	}
	return nil
}

func safeThreadsOAuthDiagnostic(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 128 {
		return ""
	}
	for _, character := range value {
		if (character >= 'a' && character <= 'z') ||
			(character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') ||
			strings.ContainsRune("_.:-", character) {
			continue
		}
		return ""
	}
	return value
}

func (a *App) persistThreadsConnection(
	orgID, userID, integrationID uuid.UUID,
	expectedFingerprint string,
	profile threadsProfile,
	encryptedToken string,
	expiresIn int64,
	permissions channelapi.ThreadsPermissionSnapshot,
) error {
	return a.DB.Transaction(func(tx *gorm.DB) error {
		if err := lockChannelAIOrganizationScopeTx(tx, orgID); err != nil {
			return err
		}
		txApp := a.scopedApp(tx, orgID)
		if !txApp.HasPermission(userID, models.ResourceSettingsIntegrations, models.ActionWrite, orgID) {
			return errThreadsOAuthForbidden
		}
		var integration models.ProviderIntegration
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ? AND organization_id = ? AND provider = ?", integrationID, orgID, integrationProviderThreads).
			First(&integration).Error; err != nil {
			return err
		}
		currentSnapshot, err := txApp.loadThreadsIntegrationSnapshot(orgID, true)
		if err != nil || currentSnapshot.IntegrationID != integrationID || currentSnapshot.Fingerprint != expectedFingerprint {
			return errThreadsOAuthStale
		}

		now := time.Now().UTC()
		externalID := strings.TrimSpace(string(profile.ID))
		var conflictingAccounts int64
		if err := tx.Unscoped().Model(&models.ChannelAccount{}).
			Where(
				"organization_id = ? AND channel = ? AND provider = ? AND external_account_id <> ?",
				orgID,
				models.ChannelThreads,
				channelapi.ThreadsProvider,
				externalID,
			).
			Count(&conflictingAccounts).Error; err != nil {
			return err
		}
		if conflictingAccounts > 0 {
			return errThreadsOAuthAccountBound
		}
		var account models.ChannelAccount
		find := tx.Unscoped().Clauses(clause.Locking{Strength: "UPDATE"}).Where(
			"organization_id = ? AND channel = ? AND provider = ? AND external_account_id = ?",
			orgID,
			models.ChannelThreads,
			channelapi.ThreadsProvider,
			externalID,
		).First(&account)
		isNew := errors.Is(find.Error, gorm.ErrRecordNotFound)
		if find.Error != nil && !isNew {
			return find.Error
		}
		username := strings.TrimSpace(profile.Username)
		accountName := "Threads"
		if username != "" {
			accountName = "Threads @" + username
		}
		if isNew {
			account = models.ChannelAccount{
				BaseModel:      models.BaseModel{ID: uuid.New()},
				OrganizationID: orgID,
				CreatedByID:    &userID,
			}
		}
		account.Channel = models.ChannelThreads
		account.Provider = channelapi.ThreadsProvider
		account.Name = truncateChannelRunes(accountName, 120)
		account.ExternalAccountID = externalID
		account.Status = models.ChannelAccountStatusActive
		account.Config = threadsPublicEngagementConfig(account.Config)
		account.Config["outbound_enabled"] = true
		account.Config["ai_reply_enabled"] = false
		account.Config["activation_available"] = true
		account.Config["beta"] = false
		account.Config["webhook_callback_path"] = "/api/webhooks/channels/" + account.ID.String()
		account.Capabilities = threadsPublicEngagementCapabilities(models.JSONB{
			"text":    true,
			"replies": true,
		})
		account.Capabilities["text"] = true
		account.Capabilities["replies"] = true
		account.Capabilities["public_replies"] = true
		account.Capabilities["mentions"] = true
		account.Metadata = cloneJSONB(account.Metadata)
		account.Metadata["username"] = username
		account.Metadata["app_id"] = currentSnapshot.AppID
		account.Metadata["display_name"] = strings.TrimSpace(profile.Name)
		account.Metadata["profile_picture_url"] = strings.TrimSpace(profile.ProfilePictureURL)
		account.Metadata["granted_scopes"] = permissions.Scopes
		account.Metadata["permissions_verified_at"] = permissions.CheckedAt
		if permissions.DataAccessExpiresAt != nil {
			account.Metadata["data_access_expires_at"] = permissions.DataAccessExpiresAt.UTC().Format(time.RFC3339)
		} else {
			delete(account.Metadata, "data_access_expires_at")
		}
		account.IsDefaultIncoming = false
		account.IsDefaultOutgoing = false
		account.ConnectedAt = &now
		account.LastHealthCheckAt = &now
		account.LastError = ""
		account.LastErrorAt = nil
		account.UpdatedByID = &userID
		if isNew {
			if err := tx.Create(&account).Error; err != nil {
				return err
			}
		} else {
			account.DeletedAt = gorm.DeletedAt{}
			if err := tx.Unscoped().Save(&account).Error; err != nil {
				return err
			}
		}

		var maximumVersion int
		if err := tx.Model(&models.ChannelCredential{}).
			Where("organization_id = ? AND channel_account_id = ? AND kind = ?", orgID, account.ID, models.ChannelCredentialKindOAuth).
			Select("COALESCE(MAX(version), 0)").Scan(&maximumVersion).Error; err != nil {
			return err
		}
		if err := tx.Model(&models.ChannelCredential{}).
			Where("organization_id = ? AND channel_account_id = ? AND kind = ? AND status IN ?", orgID, account.ID, models.ChannelCredentialKindOAuth, []models.ChannelCredentialStatus{models.ChannelCredentialStatusActive, models.ChannelCredentialStatusExpiring}).
			Updates(map[string]any{"status": models.ChannelCredentialStatusRevoked, "revoked_at": now, "rotated_at": now, "updated_at": now}).Error; err != nil {
			return err
		}
		expiresAt := now.Add(time.Duration(expiresIn) * time.Second)
		for _, permissionExpiry := range []*time.Time{permissions.ExpiresAt, permissions.DataAccessExpiresAt} {
			if permissionExpiry != nil && permissionExpiry.Before(expiresAt) {
				expiresAt = permissionExpiry.UTC()
			}
		}
		credential := models.ChannelCredential{
			BaseModel:        models.BaseModel{ID: uuid.New()},
			OrganizationID:   orgID,
			ChannelAccountID: account.ID,
			Kind:             models.ChannelCredentialKindOAuth,
			Version:          maximumVersion + 1,
			CredentialBlob:   models.JSONB{"access_token": encryptedToken},
			Status:           models.ChannelCredentialStatusActive,
			KeyVersion:       "app:v1",
			ExpiresAt:        &expiresAt,
			LastValidatedAt:  &now,
			Metadata: models.JSONB{
				"token_type":             "long_lived",
				"app_id":                 currentSnapshot.AppID,
				"granted_scopes":         permissions.Scopes,
				"permissions_checked_at": permissions.CheckedAt,
			},
		}
		if err := tx.Create(&credential).Error; err != nil {
			return err
		}
		integration.Enabled = true
		integration.LastTestedAt = &now
		integration.LastSuccessfulAt = &now
		integration.LastErrorCode = ""
		integration.LastErrorMessage = ""
		integration.ValidationToken = ""
		integration.UpdatedByID = &userID
		if err := tx.Save(&integration).Error; err != nil {
			return err
		}
		return audit.LogAudit(
			tx,
			orgID,
			userID,
			audit.GetUserName(tx, userID),
			models.ResourceSettingsIntegrations,
			integration.ID,
			models.AuditActionUpdated,
			map[string]any{"provider": integrationProviderThreads, "connected": !isNew},
			map[string]any{"provider": integrationProviderThreads, "connected": true, "account_id": account.ID},
			map[string]any{"field": "access_token", "old_value": "********", "new_value": "********"},
		)
	})
}

func (a *App) redirectThreadsCallback(r *fastglue.Request, status string) {
	basePath := ""
	if a != nil && a.Config != nil {
		basePath = strings.TrimRight(sanitizeRedirectPath(strings.TrimSpace(a.Config.Server.BasePath)), "/")
	}
	destination := basePath + "/settings/integrations?threads=" + url.QueryEscape(status)
	r.RequestCtx.Response.Header.Set("Cache-Control", "no-store")
	r.RequestCtx.Response.Header.Set("Location", destination)
	r.RequestCtx.Response.SetStatusCode(fasthttp.StatusSeeOther)
}
