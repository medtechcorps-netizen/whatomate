package handlers

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	channelapi "github.com/shridarpatil/whatomate/internal/channel"
	configpkg "github.com/shridarpatil/whatomate/internal/config"
	"github.com/shridarpatil/whatomate/internal/database"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/valyala/fasthttp"
	"github.com/zerodha/fastglue"
	"gorm.io/gorm"
)

const (
	metaInstagramOAuthStatePrefix       = "integration:meta:instagram:onboarding:state:"
	metaInstagramOAuthStateTTL          = 10 * time.Minute
	metaInstagramOAuthBrowserCookieBase = "__Host-whm_meta_instagram_oauth_"
	metaInstagramOAuthBrowserSecretSize = 48
	metaInstagramProviderOperationLimit = 90 * time.Second
	metaInstagramMaxAuthorizationCode   = 8192
	metaInstagramMaxOpaqueState         = 256
	metaInstagramManagementMode         = "meta_instagram_login_oauth"
)

var (
	errMetaInstagramOnboardingDisabled = errors.New("managed Instagram onboarding is disabled")
	errMetaInstagramOAuthForbidden     = errors.New("instagram authorization is no longer permitted")
)

type metaInstagramOAuthState struct {
	ReconnectAccountID   string    `json:"reconnect_account_id,omitempty"`
	OrganizationID       string    `json:"organization_id"`
	UserID               string    `json:"user_id"`
	Nonce                string    `json:"nonce"`
	ConfigFingerprint    string    `json:"config_fingerprint"`
	BrowserBindingDigest string    `json:"browser_binding_digest"`
	IssuedAt             time.Time `json:"issued_at"`
	ExpiresAt            time.Time `json:"expires_at"`
}

type metaInstagramOnboardingStatus struct {
	OrganizationID  uuid.UUID `json:"organization_id"`
	Configured      bool      `json:"configured"`
	Enabled         bool      `json:"enabled"`
	QuarantineOnly  bool      `json:"quarantine_only"`
	ReviewStatus    string    `json:"app_review_status"`
	RedirectURL     string    `json:"redirect_url"`
	DeauthorizeURL  string    `json:"deauthorize_url"`
	DataDeletionURL string    `json:"data_deletion_url"`
	WebhookURL      string    `json:"managed_webhook_url"`
}

func (a *App) metaInstagramOnboardingAvailable() bool {
	if a == nil || a.Redis == nil || !a.hasIntegrationEncryptionKey() ||
		(a.Config != nil && a.Config.MetaInstagram.QuarantineOnly) {
		return false
	}
	settings, err := a.metaInstagramOnboardingSettings()
	if err != nil {
		return false
	}
	if settings.AppReviewStatus == "approved" {
		return true
	}
	return !strings.EqualFold(strings.TrimSpace(a.Config.App.Environment), "production") &&
		validCanonicalMetaID(settings.DevelopmentTestProfileID) &&
		validCanonicalMetaID(settings.DevelopmentTestOAuthSubjectID) &&
		validMetaInstagramDevelopmentRole(settings.DevelopmentAppRole)
}

func (a *App) metaInstagramOnboardingSettings() (configpkg.MetaInstagramConfig, error) {
	if a == nil || a.Config == nil || !a.Config.MetaInstagram.Enabled ||
		!a.Config.MetaRegistry.Enabled || !a.hasIntegrationEncryptionKey() {
		return configpkg.MetaInstagramConfig{}, errMetaInstagramOnboardingDisabled
	}
	settings := a.Config.MetaInstagram
	settings.AppID = strings.TrimSpace(settings.AppID)
	settings.AppReviewStatus = strings.ToLower(strings.TrimSpace(settings.AppReviewStatus))
	settings.GraphAPIVersion = strings.Trim(strings.TrimSpace(settings.GraphAPIVersion), "/")
	settings.AuthorizationBaseURL = strings.TrimRight(strings.TrimSpace(settings.AuthorizationBaseURL), "/")
	settings.TokenBaseURL = strings.TrimRight(strings.TrimSpace(settings.TokenBaseURL), "/")
	settings.GraphBaseURL = strings.TrimRight(strings.TrimSpace(settings.GraphBaseURL), "/")
	settings.ReReplyBaseURL = strings.TrimRight(strings.TrimSpace(settings.ReReplyBaseURL), "/")
	settings.RelayBaseURL = strings.TrimRight(strings.TrimSpace(settings.RelayBaseURL), "/")
	settings.AllowedOrganizationID = strings.TrimSpace(settings.AllowedOrganizationID)
	settings.DevelopmentTestProfileID = strings.TrimSpace(settings.DevelopmentTestProfileID)
	settings.DevelopmentTestOAuthSubjectID = strings.TrimSpace(settings.DevelopmentTestOAuthSubjectID)
	settings.DevelopmentAppRole = strings.ToLower(strings.TrimSpace(settings.DevelopmentAppRole))
	if !validCanonicalMetaID(settings.AppID) || len(settings.AppSecret) < 32 ||
		settings.GraphAPIVersion == "" || settings.AuthorizationBaseURL == "" ||
		settings.TokenBaseURL == "" || settings.GraphBaseURL == "" ||
		settings.ReReplyBaseURL == "" || settings.RelayBaseURL == "" ||
		settings.AllowedOrganizationID == "" {
		return configpkg.MetaInstagramConfig{}, errMetaInstagramOnboardingDisabled
	}
	return settings, nil
}

func metaInstagramOnboardingFingerprint(settings configpkg.MetaInstagramConfig) string {
	secretDigest := sha256.Sum256([]byte(settings.AppSecret))
	digest := sha256.Sum256([]byte(strings.Join([]string{
		settings.AppID,
		strconv.FormatBool(settings.QuarantineOnly),
		hex.EncodeToString(secretDigest[:]),
		settings.AppReviewStatus,
		settings.GraphAPIVersion,
		settings.AuthorizationBaseURL,
		settings.TokenBaseURL,
		settings.GraphBaseURL,
		settings.ReReplyBaseURL,
		settings.RelayBaseURL,
		settings.AllowedOrganizationID,
		settings.DevelopmentTestProfileID,
		settings.DevelopmentTestOAuthSubjectID,
		settings.DevelopmentAppRole,
	}, "\x00")))
	return hex.EncodeToString(digest[:])
}

func (a *App) metaInstagramRuntimeFingerprint(settings configpkg.MetaInstagramConfig) string {
	base := metaInstagramOnboardingFingerprint(settings)
	if a == nil || a.Config == nil {
		return base
	}
	serviceDigest := sha256.Sum256([]byte(a.Config.MetaRegistry.ServiceSecret))
	edgeDigest := sha256.Sum256([]byte(a.Config.MetaRegistry.RelayEdgeSecret))
	digest := sha256.Sum256([]byte(strings.Join([]string{
		base,
		hex.EncodeToString(serviceDigest[:]),
		hex.EncodeToString(edgeDigest[:]),
		a.Config.MetaInstagram.AllowedOrganizationID,
	}, "\x00")))
	return hex.EncodeToString(digest[:])
}

func metaInstagramOpaqueValuesEqual(left, right string) bool {
	if len(left) == 0 || len(left) != len(right) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(left), []byte(right)) == 1
}

func metaInstagramOAuthStateKey(nonce string) string {
	return metaInstagramOAuthStatePrefix + nonce
}

func metaInstagramOAuthBrowserCookieName(nonce string) string {
	digest := sha256.Sum256([]byte(nonce))
	return metaInstagramOAuthBrowserCookieBase + hex.EncodeToString(digest[:16])
}

// metaInstagramOAuthBrowserBindingDigest binds the browser-only verifier to
// every server-owned authorization decision. The verifier itself is never put
// in the provider authorization URL or persisted in Redis.
func metaInstagramOAuthBrowserBindingDigest(state metaInstagramOAuthState, verifier string) string {
	mac := hmac.New(sha256.New, []byte(verifier))
	_, _ = mac.Write([]byte(strings.Join([]string{
		"rereply-meta-instagram-browser-binding-v1",
		state.OrganizationID,
		state.UserID,
		state.Nonce,
		state.ReconnectAccountID,
		state.ConfigFingerprint,
		state.IssuedAt.UTC().Format(time.RFC3339Nano),
		state.ExpiresAt.UTC().Format(time.RFC3339Nano),
	}, "\x00")))
	return hex.EncodeToString(mac.Sum(nil))
}

func setMetaInstagramOAuthBrowserCookie(r *fastglue.Request, nonce, verifier string) {
	cookie := fasthttp.AcquireCookie()
	cookie.SetKey(metaInstagramOAuthBrowserCookieName(nonce))
	cookie.SetValue(verifier)
	cookie.SetHTTPOnly(true)
	cookie.SetSecure(true)
	cookie.SetSameSite(fasthttp.CookieSameSiteLaxMode)
	cookie.SetPath("/")
	cookie.SetMaxAge(int(metaInstagramOAuthStateTTL / time.Second))
	cookie.SetExpire(time.Now().UTC().Add(metaInstagramOAuthStateTTL))
	r.RequestCtx.Response.Header.SetCookie(cookie)
	fasthttp.ReleaseCookie(cookie)
}

func clearMetaInstagramOAuthBrowserCookie(r *fastglue.Request, nonce string) {
	cookie := fasthttp.AcquireCookie()
	cookie.SetKey(metaInstagramOAuthBrowserCookieName(nonce))
	cookie.SetValue("")
	cookie.SetHTTPOnly(true)
	cookie.SetSecure(true)
	cookie.SetSameSite(fasthttp.CookieSameSiteLaxMode)
	cookie.SetPath("/")
	cookie.SetMaxAge(-1)
	r.RequestCtx.Response.Header.SetCookie(cookie)
	fasthttp.ReleaseCookie(cookie)
}

var consumeMetaInstagramOAuthStateScript = redis.NewScript(`
local current = redis.call("GET", KEYS[1])
if current and current == ARGV[1] then
  return redis.call("DEL", KEYS[1])
end
return 0
`)

func (a *App) consumeMetaInstagramOAuthState(
	ctx context.Context,
	nonce, browserVerifier string,
) (metaInstagramOAuthState, error) {
	var state metaInstagramOAuthState
	if len(browserVerifier) != metaInstagramOAuthBrowserSecretSize {
		return state, errors.New("instagram OAuth browser binding is missing")
	}
	stateJSON, err := a.Redis.Get(ctx, metaInstagramOAuthStateKey(nonce)).Bytes()
	if err != nil {
		return state, err
	}
	now := time.Now().UTC()
	if json.Unmarshal(stateJSON, &state) != nil ||
		!metaInstagramOpaqueValuesEqual(state.Nonce, nonce) ||
		state.IssuedAt.IsZero() || state.IssuedAt.After(now.Add(time.Minute)) ||
		!state.ExpiresAt.After(state.IssuedAt) ||
		state.ExpiresAt.Sub(state.IssuedAt) > metaInstagramOAuthStateTTL+time.Minute ||
		!state.ExpiresAt.After(now) ||
		!metaInstagramOpaqueValuesEqual(
			state.BrowserBindingDigest,
			metaInstagramOAuthBrowserBindingDigest(state, browserVerifier),
		) {
		return metaInstagramOAuthState{}, errors.New("instagram OAuth browser binding is invalid")
	}
	consumed, err := consumeMetaInstagramOAuthStateScript.Run(
		ctx, a.Redis, []string{metaInstagramOAuthStateKey(nonce)}, stateJSON,
	).Int()
	if err != nil || consumed != 1 {
		if err == nil {
			err = errors.New("instagram OAuth state was already consumed")
		}
		return metaInstagramOAuthState{}, err
	}
	return state, nil
}

func (a *App) GetMetaInstagramOnboardingStatus(r *fastglue.Request) error {
	orgID, _, _, err := a.requireMetaInstagramOnboardingAuth(r, models.ActionWrite, false)
	if err != nil {
		return nil
	}
	settings, settingsErr := a.metaInstagramOnboardingSettings()
	status := metaInstagramOnboardingStatus{
		OrganizationID: orgID,
		Enabled:        settingsErr == nil && a.metaInstagramOnboardingAvailable(),
		ReviewStatus:   "not_configured",
	}
	if settingsErr == nil {
		status.Configured = true
		status.QuarantineOnly = settings.QuarantineOnly
		status.ReviewStatus = settings.AppReviewStatus
		status.RedirectURL, _ = a.metaInstagramCallbackURL(settings)
		status.DeauthorizeURL, _ = metaRegistryJoinURL(
			settings.ReReplyBaseURL,
			"api", "integrations", "meta", "instagram", "deauthorize",
		)
		status.DataDeletionURL, _ = metaRegistryJoinURL(
			settings.ReReplyBaseURL,
			"api", "integrations", "meta", "instagram", "data-deletion",
		)
		status.WebhookURL, _ = metaRegistryJoinURL(
			settings.RelayBaseURL,
			"v1", "meta", "instagram", "managed-webhook",
		)
	}
	setMetaMessengerNoStoreHeaders(r)
	return r.SendEnvelope(status)
}

func (a *App) StartMetaInstagramOnboarding(r *fastglue.Request) error {
	orgID, userID, _, err := a.requireMetaInstagramOnboardingAuth(r, models.ActionWrite, true)
	if err != nil {
		return nil
	}
	return a.beginMetaInstagramOnboarding(r, orgID, userID, uuid.Nil)
}

func (a *App) ReconnectMetaInstagramOnboarding(r *fastglue.Request) error {
	orgID, userID, _, err := a.requireMetaInstagramOnboardingAuth(r, models.ActionWrite, true)
	if err != nil {
		return nil
	}
	accountID, err := parsePathUUID(r, "id", "channel account")
	if err != nil {
		return nil
	}
	var found bool
	err = database.WithTenantReadCommitted(a.rootApp().DB, orgID, func(tx *gorm.DB) error {
		var account models.ChannelAccount
		if err := tx.Where(
			"id = ? AND organization_id = ? AND channel = ? AND provider = ?",
			accountID, orgID, models.ChannelInstagram, channelapi.RelayProvider,
		).First(&account).Error; err != nil {
			return err
		}
		found = exactMetaRegistryControlPlaneConfig(account.Config) &&
			stringConfigValue(account.Config, "instagram_api_mode") == "instagram_login" &&
			stringConfigValue(account.Metadata, "meta_platform_app_id") == strings.TrimSpace(a.Config.MetaInstagram.AppID) &&
			validateMetaRegistryPlatformBinding(&account) == nil &&
			a.metaInstagramManagedURLBindingReason(&account) == ""
		return nil
	})
	if err != nil || !found {
		return r.SendErrorEnvelope(fasthttp.StatusNotFound, "Managed Instagram account not found", nil, "")
	}
	return a.beginMetaInstagramOnboarding(r, orgID, userID, accountID)
}

func (a *App) beginMetaInstagramOnboarding(
	r *fastglue.Request,
	orgID, userID, reconnectAccountID uuid.UUID,
) error {
	settings, err := a.metaInstagramOnboardingSettings()
	if err != nil || !a.metaInstagramOnboardingAvailable() {
		return r.SendErrorEnvelope(fasthttp.StatusServiceUnavailable, "Managed Instagram onboarding is unavailable until App Review is approved", nil, "")
	}
	if allowed, err := a.requireMetaInstagramRateLimit(r, orgID, userID, "start", 12, time.Minute); !allowed {
		return err
	}
	nonce := generateRandomString(48)
	now := time.Now().UTC()
	state := metaInstagramOAuthState{
		OrganizationID: orgID.String(), UserID: userID.String(), Nonce: nonce,
		ConfigFingerprint: a.metaInstagramRuntimeFingerprint(settings),
		IssuedAt:          now,
		ExpiresAt:         now.Add(metaInstagramOAuthStateTTL),
	}
	if reconnectAccountID != uuid.Nil {
		state.ReconnectAccountID = reconnectAccountID.String()
	}
	browserVerifier := generateRandomString(metaInstagramOAuthBrowserSecretSize)
	state.BrowserBindingDigest = metaInstagramOAuthBrowserBindingDigest(state, browserVerifier)
	payload, err := json.Marshal(state)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to start Instagram onboarding", nil, "")
	}
	stored, err := a.Redis.SetNX(
		requestContext(r), metaInstagramOAuthStateKey(nonce), payload, metaInstagramOAuthStateTTL,
	).Result()
	if err != nil || !stored {
		return r.SendErrorEnvelope(fasthttp.StatusServiceUnavailable, "Instagram OAuth state storage is unavailable", nil, "")
	}
	redirectURI, err := a.metaInstagramCallbackURL(settings)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusServiceUnavailable, "Instagram onboarding callback is unavailable", nil, "")
	}
	authorizationURL, err := metaInstagramJoinProviderURL(settings.AuthorizationBaseURL, "oauth", "authorize")
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusServiceUnavailable, "Instagram authorization is unavailable", nil, "")
	}
	parsed, _ := url.Parse(authorizationURL)
	query := parsed.Query()
	query.Set("client_id", settings.AppID)
	query.Set("redirect_uri", redirectURI)
	query.Set("response_type", "code")
	query.Set("scope", strings.Join(metaInstagramRequiredScopes, ","))
	query.Set("state", nonce)
	parsed.RawQuery = query.Encode()
	setMetaInstagramOAuthBrowserCookie(r, nonce, browserVerifier)
	setMetaMessengerNoStoreHeaders(r)
	return r.SendEnvelope(map[string]any{
		"provider": "meta", "channel": models.ChannelInstagram,
		"mode": "instagram_login", "authorization_url": parsed.String(),
		"expires_at": state.ExpiresAt,
	})
}

// CallbackMetaInstagram completes the server-side OAuth flow from one
// tenant/user/config-bound state and its initiator-browser-only proof. The
// account remains pending after a verified messages subscription; health and
// explicit admin approval are separate.
func (a *App) CallbackMetaInstagram(r *fastglue.Request) error {
	setMetaMessengerNoStoreHeaders(r)
	stateNonce := strings.TrimSpace(string(r.RequestCtx.QueryArgs().Peek("state")))
	if stateNonce == "" || len(stateNonce) > metaInstagramMaxOpaqueState || a.Redis == nil {
		a.redirectMetaInstagramCallback(r, "error")
		return nil
	}
	browserVerifier := string(
		r.RequestCtx.Request.Header.Cookie(metaInstagramOAuthBrowserCookieName(stateNonce)),
	)
	clearMetaInstagramOAuthBrowserCookie(r, stateNonce)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	state, err := a.consumeMetaInstagramOAuthState(ctx, stateNonce, browserVerifier)
	cancel()
	if err != nil {
		a.redirectMetaInstagramCallback(r, "error")
		return nil
	}
	orgID, orgErr := uuid.Parse(state.OrganizationID)
	userID, userErr := uuid.Parse(state.UserID)
	reconnectID, reconnectErr := uuid.Parse(state.ReconnectAccountID)
	if state.ReconnectAccountID == "" {
		reconnectID = uuid.Nil
		reconnectErr = nil
	}
	if orgErr != nil || userErr != nil || reconnectErr != nil || orgID == uuid.Nil || userID == uuid.Nil {
		a.redirectMetaInstagramCallback(r, "error")
		return nil
	}
	if providerError := strings.TrimSpace(string(r.RequestCtx.QueryArgs().Peek("error"))); providerError != "" {
		a.redirectMetaInstagramCallback(r, "cancelled")
		return nil
	}
	code := strings.TrimSpace(string(r.RequestCtx.QueryArgs().Peek("code")))
	if code == "" || len(code) > metaInstagramMaxAuthorizationCode {
		a.redirectMetaInstagramCallback(r, "error")
		return nil
	}
	settings, err := a.metaInstagramOnboardingSettings()
	if err != nil || !a.metaInstagramOnboardingAvailable() ||
		!metaInstagramOpaqueValuesEqual(state.ConfigFingerprint, a.metaInstagramRuntimeFingerprint(settings)) {
		a.redirectMetaInstagramCallback(r, "error")
		return nil
	}
	if err := a.authorizeMetaInstagramCallback(orgID, userID, reconnectID); err != nil {
		a.redirectMetaInstagramCallback(r, "error")
		return nil
	}
	redirectURI, err := a.metaInstagramCallbackURL(settings)
	if err != nil {
		a.redirectMetaInstagramCallback(r, "error")
		return nil
	}
	providerCtx, providerCancel := context.WithTimeout(context.Background(), metaInstagramProviderOperationLimit)
	defer providerCancel()
	shortToken, err := a.exchangeMetaInstagramAuthorizationCode(providerCtx, settings, code, redirectURI)
	if err != nil {
		a.Log.Warn("Instagram authorization code exchange failed", "organization_id", orgID)
		a.redirectMetaInstagramCallback(r, "error")
		return nil
	}
	if !a.metaInstagramReleaseSubjectAllowed(orgID, shortToken.UserID) {
		// Development/test evidence is an exact server-owned profile tuple. Stop
		// before exchanging or inspecting a token for any other app-role user.
		a.Log.Warn("Instagram short-token profile is outside release evidence", "organization_id", orgID)
		a.redirectMetaInstagramCallback(r, "error")
		return nil
	}
	longToken, err := a.exchangeMetaInstagramLongLivedToken(providerCtx, settings, shortToken.AccessToken)
	if err != nil {
		a.Log.Warn("Instagram long-lived token exchange failed", "organization_id", orgID)
		a.redirectMetaInstagramCallback(r, "error")
		return nil
	}
	inspection, err := a.inspectMetaInstagramToken(providerCtx, settings, longToken.AccessToken)
	if err != nil {
		a.Log.Warn("Instagram token inspection failed", "organization_id", orgID)
		a.redirectMetaInstagramCallback(r, "error")
		return nil
	}
	profile, err := a.fetchMetaInstagramProfile(providerCtx, settings, longToken.AccessToken)
	oauthSubjectID := profile.oauthSubjectID()
	professionalAccountID := profile.professionalAccountID()
	if err != nil || oauthSubjectID != shortToken.UserID || oauthSubjectID != inspection.UserID ||
		!validCanonicalMetaID(professionalAccountID) {
		a.Log.Warn("Instagram token and profile identities did not match", "organization_id", orgID)
		a.redirectMetaInstagramCallback(r, "error")
		return nil
	}
	if _, allowed := a.metaInstagramReleaseMode(
		orgID, oauthSubjectID, professionalAccountID,
	); !allowed {
		a.Log.Warn("Instagram profile is outside the deployment-owned release evidence", "organization_id", orgID)
		a.redirectMetaInstagramCallback(r, "error")
		return nil
	}
	now := time.Now().UTC()
	expiresAt, err := metaInstagramExpiry(now, longToken.ExpiresIn, inspection.ExpiresAt)
	if err != nil {
		a.redirectMetaInstagramCallback(r, "error")
		return nil
	}
	operationID := uuid.New()
	operationExpiresAt := now.Add(metaMessengerSubscriptionOperationLease)
	var result metaRegistryProvisionResult
	if !metaInstagramOpaqueValuesEqual(state.ConfigFingerprint, a.metaInstagramRuntimeFingerprint(settings)) {
		a.redirectMetaInstagramCallback(r, "error")
		return nil
	}
	err = a.WithCommittedTenantApp(orgID, func(scoped *App) error {
		if !scoped.HasPermission(userID, models.ResourceSettingsIntegrations, models.ActionWrite, orgID) ||
			!scoped.HasPermission(userID, models.ResourceChannelAccounts, models.ActionWrite, orgID) ||
			!scoped.metaInstagramOrganizationAllowed(orgID) {
			return errMetaInstagramOAuthForbidden
		}
		if reconnectID != uuid.Nil {
			var rotateErr error
			result, rotateErr = scoped.rotateMetaInstagramBinding(metaInstagramRotateInput{
				AccountID: reconnectID, OrganizationID: orgID, UserID: userID,
				Profile: profile, Inspection: inspection, AccessToken: longToken.AccessToken,
				AuthorizationStartedAt: state.IssuedAt,
				TokenExpiresAt:         expiresAt, SubscriptionOperationID: operationID,
				SubscriptionOperationExpiresAt: operationExpiresAt,
			})
			return rotateErr
		}
		var provisionErr error
		result, provisionErr = scoped.provisionMetaRegistryBinding(metaRegistryProvisionInput{
			OrganizationID: orgID, UserID: userID, Channel: models.ChannelInstagram,
			Name:              metaInstagramAccountName(profile.Username, professionalAccountID),
			ExternalAccountID: professionalAccountID, InstagramAPIMode: "instagram_login",
			WebhookApp: "instagram_login", PlatformAppID: settings.AppID,
			MetaAuthorityAssetID: professionalAccountID, AuthorizingMetaUserID: oauthSubjectID,
			AuthorizationTokenKind: metaMessengerTokenKindUser,
			GrantedScopes:          inspection.Scopes, AccessToken: longToken.AccessToken,
			AuthorityToken: longToken.AccessToken, TokenExpiresAt: expiresAt,
			OwnershipCheckedAt:     inspection.CheckedAt,
			AuthorizationStartedAt: state.IssuedAt,
			ReReplyBaseURL:         settings.ReReplyBaseURL, RelayBaseURL: settings.RelayBaseURL,
			SubscriptionOperationID:        operationID,
			SubscriptionOperationExpiresAt: operationExpiresAt,
		})
		return provisionErr
	})
	if err != nil {
		a.Log.Warn("Managed Instagram account claim failed", "organization_id", orgID)
		if errors.Is(err, errMetaInstagramProfileAlreadyRegistered) {
			a.redirectMetaInstagramCallback(r, "reconcile")
			return nil
		}
		a.redirectMetaInstagramCallback(r, "error")
		return nil
	}
	operation := result.SubscriptionOperation
	if err := a.withLockedMetaInstagramSubscriptionProviderAttempt(
		providerCtx, orgID, result.Account.ID, operation,
		metaMessengerSubscriptionSubscribePending,
		func(account models.ChannelAccount, accessToken string) error {
			return a.subscribeMetaInstagramMessages(
				providerCtx, settings, account.ExternalAccountID, accessToken,
			)
		},
	); err != nil {
		if errors.Is(err, errMetaMessengerSubscriptionFence) {
			a.redirectMetaInstagramCallback(r, "reconcile")
			return nil
		}
		_ = a.WithCommittedTenantApp(orgID, func(scoped *App) error {
			return scoped.recordMetaInstagramSubscriptionOperationFailure(
				orgID, result.Account.ID, operation,
				metaMessengerSubscriptionSubscribePending,
				metaMessengerSubscriptionSubscribeFailed,
				time.Now().UTC(),
			)
		})
		a.redirectMetaInstagramCallback(r, "reconcile")
		return nil
	}
	err = a.WithCommittedTenantApp(orgID, func(scoped *App) error {
		var finalizeErr error
		result, finalizeErr = scoped.finalizeMetaInstagramSubscribeOperation(
			orgID, userID, result.Account.ID, operation, time.Now().UTC(),
		)
		return finalizeErr
	})
	if err != nil {
		a.redirectMetaInstagramCallback(r, "reconcile")
		return nil
	}
	a.redirectMetaInstagramCallback(r, "pending")
	return nil
}

func (a *App) authorizeMetaInstagramCallback(
	organizationID, userID, reconnectAccountID uuid.UUID,
) error {
	if !a.metaInstagramOrganizationAllowed(organizationID) {
		return errMetaInstagramOAuthForbidden
	}
	return a.WithTenantApp(organizationID, func(scoped *App) error {
		if !scoped.HasPermission(userID, models.ResourceSettingsIntegrations, models.ActionWrite, organizationID) ||
			!scoped.HasPermission(userID, models.ResourceChannelAccounts, models.ActionWrite, organizationID) {
			return errMetaInstagramOAuthForbidden
		}
		if reconnectAccountID == uuid.Nil {
			return nil
		}
		var account models.ChannelAccount
		if err := scoped.DB.Where(
			"id = ? AND organization_id = ? AND channel = ? AND provider = ?",
			reconnectAccountID, organizationID, models.ChannelInstagram, channelapi.RelayProvider,
		).First(&account).Error; err != nil {
			return err
		}
		if !exactMetaRegistryControlPlaneConfig(account.Config) ||
			stringConfigValue(account.Config, "instagram_api_mode") != "instagram_login" ||
			stringConfigValue(account.Metadata, "meta_platform_app_id") != strings.TrimSpace(a.Config.MetaInstagram.AppID) ||
			validateMetaRegistryPlatformBinding(&account) != nil ||
			a.metaInstagramManagedURLBindingReason(&account) != "" {
			return errMetaInstagramOAuthForbidden
		}
		return nil
	})
}

func (a *App) metaInstagramCallbackURL(settings configpkg.MetaInstagramConfig) (string, error) {
	return metaRegistryJoinURL(
		settings.ReReplyBaseURL,
		"api", "integrations", "meta", "instagram", "callback",
	)
}

func validMetaInstagramDevelopmentRole(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "administrator", "developer", "tester":
		return true
	default:
		return false
	}
}

func validMetaInstagramDevelopmentEnvironment(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "development", "staging":
		return true
	default:
		return false
	}
}

func (a *App) metaInstagramReleaseSubjectAllowed(
	organizationID uuid.UUID,
	oauthSubjectID string,
) bool {
	if a == nil || a.Config == nil || !a.Config.MetaInstagram.Enabled ||
		a.Config.MetaInstagram.QuarantineOnly ||
		organizationID == uuid.Nil ||
		strings.TrimSpace(a.Config.MetaInstagram.AllowedOrganizationID) != organizationID.String() {
		return false
	}
	reviewStatus := strings.ToLower(strings.TrimSpace(a.Config.MetaInstagram.AppReviewStatus))
	if reviewStatus == "approved" {
		return validCanonicalMetaID(strings.TrimSpace(oauthSubjectID))
	}
	return validMetaInstagramDevelopmentEnvironment(a.Config.App.Environment) &&
		validCanonicalMetaID(strings.TrimSpace(oauthSubjectID)) &&
		strings.TrimSpace(a.Config.MetaInstagram.DevelopmentTestOAuthSubjectID) ==
			strings.TrimSpace(oauthSubjectID) &&
		validMetaInstagramDevelopmentRole(a.Config.MetaInstagram.DevelopmentAppRole)
}

func (a *App) metaInstagramReleaseMode(
	organizationID uuid.UUID,
	oauthSubjectID, professionalAccountID string,
) (string, bool) {
	if !a.metaInstagramReleaseSubjectAllowed(organizationID, oauthSubjectID) ||
		!validCanonicalMetaID(strings.TrimSpace(professionalAccountID)) {
		return "", false
	}
	if strings.EqualFold(strings.TrimSpace(a.Config.MetaInstagram.AppReviewStatus), "approved") {
		return "app_review_approved", true
	}
	if strings.TrimSpace(a.Config.MetaInstagram.DevelopmentTestProfileID) !=
		strings.TrimSpace(professionalAccountID) {
		return "", false
	}
	return "development_app_role", true
}

func (a *App) redirectMetaInstagramCallback(r *fastglue.Request, status string) {
	status = strings.ToLower(strings.TrimSpace(status))
	switch status {
	case "pending", "cancelled", "reconcile", "error":
	default:
		status = "error"
	}
	basePath := ""
	if a != nil && a.Config != nil {
		basePath = strings.TrimRight(
			sanitizeRedirectPath(strings.TrimSpace(a.Config.Server.BasePath)),
			"/",
		)
	}
	destination := basePath + "/channels?managed_instagram=" + url.QueryEscape(status)
	r.RequestCtx.Redirect(destination, fasthttp.StatusSeeOther)
}

func (a *App) requireMetaInstagramOnboardingAuth(
	r *fastglue.Request,
	channelAction string,
	requireIntegrationWrite bool,
) (uuid.UUID, uuid.UUID, string, error) {
	selectedOrgID, err := a.requireExplicitOrganization(r)
	if err != nil {
		return uuid.Nil, uuid.Nil, "", err
	}
	orgID, userID, err := a.getOrgAndUserID(r)
	if err != nil || orgID == uuid.Nil || userID == uuid.Nil {
		_ = r.SendErrorEnvelope(fasthttp.StatusUnauthorized, "Unauthorized", nil, "")
		return uuid.Nil, uuid.Nil, "", errEnvelopeSent
	}
	if orgID != selectedOrgID {
		_ = r.SendErrorEnvelope(fasthttp.StatusForbidden, "Selected organization is not available", nil, "")
		return uuid.Nil, uuid.Nil, "", errEnvelopeSent
	}
	root := a.rootApp()
	if root == nil || root.DB == nil {
		_ = r.SendErrorEnvelope(fasthttp.StatusServiceUnavailable, "Instagram onboarding authorization is unavailable", nil, "")
		return uuid.Nil, uuid.Nil, "", errEnvelopeSent
	}
	var authErr error
	var workspaceName string
	if err := database.WithTenantReadCommitted(root.DB, orgID, func(tx *gorm.DB) error {
		scoped := root.scopedApp(tx, orgID)
		_, _, authErr = scoped.requireAuth(r, models.ResourceChannelAccounts, channelAction)
		if authErr != nil {
			return nil
		}
		if requireIntegrationWrite {
			authErr = scoped.requirePermission(
				r, userID, models.ResourceSettingsIntegrations, models.ActionWrite,
			)
			if authErr != nil {
				return nil
			}
		}
		return tx.Model(&models.Organization{}).Where("id = ?", orgID).Pluck("name", &workspaceName).Error
	}); err != nil {
		_ = r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to authorize Instagram onboarding", nil, "")
		return uuid.Nil, uuid.Nil, "", errEnvelopeSent
	}
	if authErr != nil {
		return uuid.Nil, uuid.Nil, "", authErr
	}
	if !a.metaInstagramOrganizationAllowed(orgID) {
		_ = r.SendErrorEnvelope(fasthttp.StatusNotFound, "Managed Instagram onboarding is not available for this workspace", nil, "")
		return uuid.Nil, uuid.Nil, "", errEnvelopeSent
	}
	workspaceName = strings.TrimSpace(workspaceName)
	if workspaceName == "" {
		workspaceName = "Workspace"
	}
	return orgID, userID, workspaceName, nil
}

func (a *App) requireMetaInstagramRateLimit(
	r *fastglue.Request,
	orgID, userID uuid.UUID,
	action string,
	max int64,
	window time.Duration,
) (bool, error) {
	if a == nil || a.Redis == nil {
		return false, r.SendErrorEnvelope(fasthttp.StatusServiceUnavailable, "Instagram lifecycle protection is unavailable", nil, "")
	}
	key := "meta-instagram:rate:" + action + ":" + orgID.String() + ":" + userID.String()
	script := redis.NewScript(`
local n = redis.call("INCR", KEYS[1])
if n == 1 then redis.call("PEXPIRE", KEYS[1], ARGV[1]) end
return n
`)
	count, err := script.Run(requestContext(r), a.Redis, []string{key}, window.Milliseconds()).Int64()
	if err != nil {
		return false, r.SendErrorEnvelope(fasthttp.StatusServiceUnavailable, "Instagram lifecycle protection is unavailable", nil, "")
	}
	if count > max {
		return false, r.SendErrorEnvelope(fasthttp.StatusTooManyRequests, "Too many Instagram onboarding attempts; try again shortly", nil, "")
	}
	return true, nil
}
