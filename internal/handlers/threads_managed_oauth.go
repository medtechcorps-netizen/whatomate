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
	"github.com/shridarpatil/whatomate/internal/audit"
	channelapi "github.com/shridarpatil/whatomate/internal/channel"
	appcrypto "github.com/shridarpatil/whatomate/internal/crypto"
	"github.com/shridarpatil/whatomate/internal/database"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/internal/threadsplatform"
	"github.com/valyala/fasthttp"
	"github.com/zerodha/fastglue"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	managedThreadsAuthorizationURL           = "https://www.threads.net/oauth/authorize"
	managedThreadsOAuthStatePrefix           = "integration:threads:managed:oauth:state:"
	managedThreadsOAuthStateTTL              = 10 * time.Minute
	managedThreadsOAuthBrowserCookieBase     = "__Host-whm_threads_managed_oauth_"
	managedThreadsOAuthBrowserSecretSize     = 48
	managedThreadsMaxAuthorizationCode       = 8192
	managedThreadsMaxOpaqueState             = 256
	managedThreadsProviderOperationLimit     = 90 * time.Second
	managedThreadsAuthorizationRequiredState = "authorization_required"
	managedThreadsPendingActivationState     = "pending_activation"
)

var (
	errManagedThreadsUnavailable       = errors.New("managed Threads onboarding is unavailable")
	errManagedThreadsForbidden         = errors.New("managed Threads authorization is no longer permitted")
	errManagedThreadsEntitlementDenied = errors.New("managed Threads entitlement is no longer active")
	errManagedThreadsStale             = errors.New("managed Threads configuration changed during authorization")
)

// managedThreadsOAuthState contains only server-owned authorization facts.
// It is one-time Redis state, not a bearer token: the initiating browser must
// also present the verifier held only in its __Host- cookie.
type managedThreadsOAuthState struct {
	OrganizationID                  string    `json:"organization_id"`
	UserID                          string    `json:"user_id"`
	Nonce                           string    `json:"nonce"`
	IntegrationID                   string    `json:"integration_id"`
	IntegrationFingerprint          string    `json:"integration_fingerprint"`
	PlatformAppKey                  string    `json:"platform_app_key"`
	PlatformAppID                   string    `json:"platform_app_id"`
	AppReviewStatus                 string    `json:"app_review_status"`
	ConfigurationGeneration         uint64    `json:"configuration_generation"`
	ReconnectBindingID              string    `json:"reconnect_binding_id,omitempty"`
	ReconnectAccountID              string    `json:"reconnect_account_id,omitempty"`
	ExpectedAuthorizationGeneration uint64    `json:"expected_authorization_generation,omitempty"`
	BrowserBindingDigest            string    `json:"browser_binding_digest"`
	IssuedAt                        time.Time `json:"issued_at"`
	ExpiresAt                       time.Time `json:"expires_at"`
}

type managedThreadsStartSnapshot struct {
	IntegrationID                   uuid.UUID
	IntegrationFingerprint          string
	App                             threadsplatform.AppDescriptor
	ReconnectBindingID              uuid.UUID
	ReconnectAccountID              uuid.UUID
	ExpectedAuthorizationGeneration uint64
}

type managedThreadsProviderResult struct {
	OAuthSubjectID string
	Profile        threadsProfile
	AccessToken    string
	ExpiresIn      int64
	Permissions    channelapi.ThreadsPermissionSnapshot
}

func (a *App) managedThreadsOAuthAvailable(organizationID uuid.UUID) bool {
	root := a.rootApp()
	if root == nil || root.ThreadsManagedRuntime == nil || root.Redis == nil ||
		!root.hasIntegrationEncryptionKey() || !root.ThreadsManagedRuntime.OrganizationAllowed(organizationID) {
		return false
	}
	_, callbackErr := root.ThreadsManagedRuntime.CallbackURL()
	return callbackErr == nil
}

func managedThreadsOAuthStateKey(nonce string) string {
	return managedThreadsOAuthStatePrefix + nonce
}

func managedThreadsOAuthBrowserCookieName(nonce string) string {
	digest := sha256.Sum256([]byte(nonce))
	return managedThreadsOAuthBrowserCookieBase + hex.EncodeToString(digest[:16])
}

func managedThreadsOpaqueEqual(left, right string) bool {
	if left == "" || len(left) != len(right) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(left), []byte(right)) == 1
}

func managedThreadsOAuthBrowserBindingDigest(
	state managedThreadsOAuthState,
	verifier string,
) string {
	mac := hmac.New(sha256.New, []byte(verifier))
	_, _ = mac.Write([]byte(strings.Join([]string{
		"rereply-managed-threads-browser-binding-v1",
		state.OrganizationID,
		state.UserID,
		state.Nonce,
		state.IntegrationID,
		state.IntegrationFingerprint,
		state.PlatformAppKey,
		state.PlatformAppID,
		state.AppReviewStatus,
		strconv.FormatUint(state.ConfigurationGeneration, 10),
		state.ReconnectBindingID,
		state.ReconnectAccountID,
		strconv.FormatUint(state.ExpectedAuthorizationGeneration, 10),
		state.IssuedAt.UTC().Format(time.RFC3339Nano),
		state.ExpiresAt.UTC().Format(time.RFC3339Nano),
	}, "\x00")))
	return hex.EncodeToString(mac.Sum(nil))
}

func setManagedThreadsOAuthBrowserCookie(r *fastglue.Request, nonce, verifier string) {
	cookie := fasthttp.AcquireCookie()
	cookie.SetKey(managedThreadsOAuthBrowserCookieName(nonce))
	cookie.SetValue(verifier)
	cookie.SetHTTPOnly(true)
	cookie.SetSecure(true)
	cookie.SetSameSite(fasthttp.CookieSameSiteLaxMode)
	cookie.SetPath("/")
	cookie.SetMaxAge(int(managedThreadsOAuthStateTTL / time.Second))
	cookie.SetExpire(time.Now().UTC().Add(managedThreadsOAuthStateTTL))
	r.RequestCtx.Response.Header.SetCookie(cookie)
	fasthttp.ReleaseCookie(cookie)
}

func clearManagedThreadsOAuthBrowserCookie(r *fastglue.Request, nonce string) {
	cookie := fasthttp.AcquireCookie()
	cookie.SetKey(managedThreadsOAuthBrowserCookieName(nonce))
	cookie.SetValue("")
	cookie.SetHTTPOnly(true)
	cookie.SetSecure(true)
	cookie.SetSameSite(fasthttp.CookieSameSiteLaxMode)
	cookie.SetPath("/")
	cookie.SetMaxAge(-1)
	r.RequestCtx.Response.Header.SetCookie(cookie)
	fasthttp.ReleaseCookie(cookie)
}

var consumeManagedThreadsOAuthStateScript = redis.NewScript(`
local current = redis.call("GET", KEYS[1])
if current and current == ARGV[1] then
  return redis.call("DEL", KEYS[1])
end
return 0
`)

func (a *App) consumeManagedThreadsOAuthState(
	ctx context.Context,
	nonce, browserVerifier string,
) (managedThreadsOAuthState, error) {
	var state managedThreadsOAuthState
	root := a.rootApp()
	if root == nil || root.Redis == nil || len(browserVerifier) != managedThreadsOAuthBrowserSecretSize {
		return state, errors.New("managed Threads OAuth browser binding is missing")
	}
	stateJSON, err := root.Redis.Get(ctx, managedThreadsOAuthStateKey(nonce)).Bytes()
	if err != nil {
		return state, err
	}
	now := time.Now().UTC()
	if json.Unmarshal(stateJSON, &state) != nil ||
		!managedThreadsOpaqueEqual(state.Nonce, nonce) ||
		state.IssuedAt.IsZero() || state.IssuedAt.After(now.Add(time.Minute)) ||
		!state.ExpiresAt.After(state.IssuedAt) ||
		state.ExpiresAt.Sub(state.IssuedAt) > managedThreadsOAuthStateTTL+time.Minute ||
		!state.ExpiresAt.After(now) ||
		!managedThreadsOpaqueEqual(
			state.BrowserBindingDigest,
			managedThreadsOAuthBrowserBindingDigest(state, browserVerifier),
		) {
		return managedThreadsOAuthState{}, errors.New("managed Threads OAuth browser binding is invalid")
	}
	consumed, err := consumeManagedThreadsOAuthStateScript.Run(
		ctx,
		root.Redis,
		[]string{managedThreadsOAuthStateKey(nonce)},
		stateJSON,
	).Int()
	if err != nil || consumed != 1 {
		if err == nil {
			err = errors.New("managed Threads OAuth state was already consumed")
		}
		return managedThreadsOAuthState{}, err
	}
	return state, nil
}

// StartManagedThreadsOAuth is an authenticated, explicitly tenant-bound
// control-plane entry point. It never accepts app credentials, review state,
// callback URLs, or shard identifiers from the clinic.
func (a *App) StartManagedThreadsOAuth(r *fastglue.Request) error {
	organizationID, err := a.requireExplicitOrganization(r)
	if err != nil {
		return nil
	}
	_, userID, err := a.getOrgAndUserID(r)
	if err != nil || userID == uuid.Nil {
		return r.SendErrorEnvelope(fasthttp.StatusUnauthorized, "Unauthorized", nil, "")
	}
	if !a.managedThreadsOAuthAvailable(organizationID) {
		return r.SendErrorEnvelope(
			fasthttp.StatusServiceUnavailable,
			"Managed Threads onboarding is unavailable for this workspace",
			nil,
			"",
		)
	}
	snapshot, err := a.prepareManagedThreadsStart(organizationID, userID)
	if err != nil {
		return a.sendManagedThreadsStartError(r, err)
	}

	now := time.Now().UTC()
	nonce := generateRandomString(48)
	state := managedThreadsOAuthState{
		OrganizationID:                  organizationID.String(),
		UserID:                          userID.String(),
		Nonce:                           nonce,
		IntegrationID:                   snapshot.IntegrationID.String(),
		IntegrationFingerprint:          snapshot.IntegrationFingerprint,
		PlatformAppKey:                  snapshot.App.PlatformAppKey,
		PlatformAppID:                   snapshot.App.AppID,
		AppReviewStatus:                 snapshot.App.AppReviewStatus,
		ConfigurationGeneration:         snapshot.App.ConfigurationGeneration,
		ExpectedAuthorizationGeneration: snapshot.ExpectedAuthorizationGeneration,
		IssuedAt:                        now,
		ExpiresAt:                       now.Add(managedThreadsOAuthStateTTL),
	}
	if snapshot.ReconnectBindingID != uuid.Nil {
		state.ReconnectBindingID = snapshot.ReconnectBindingID.String()
		state.ReconnectAccountID = snapshot.ReconnectAccountID.String()
	}
	browserVerifier := generateRandomString(managedThreadsOAuthBrowserSecretSize)
	state.BrowserBindingDigest = managedThreadsOAuthBrowserBindingDigest(state, browserVerifier)
	payload, err := json.Marshal(state)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to start managed Threads authorization", nil, "")
	}
	stored, err := a.rootApp().Redis.SetNX(
		requestContext(r),
		managedThreadsOAuthStateKey(nonce),
		payload,
		managedThreadsOAuthStateTTL,
	).Result()
	if err != nil || !stored {
		return r.SendErrorEnvelope(fasthttp.StatusServiceUnavailable, "Managed Threads OAuth state storage is unavailable", nil, "")
	}
	callbackURL, err := a.rootApp().ThreadsManagedRuntime.CallbackURL()
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusServiceUnavailable, "Managed Threads callback is unavailable", nil, "")
	}
	authorization, _ := url.Parse(managedThreadsAuthorizationURL)
	query := authorization.Query()
	query.Set("client_id", snapshot.App.AppID)
	query.Set("redirect_uri", callbackURL)
	query.Set("response_type", "code")
	query.Set("scope", strings.Join(threadsRequiredScopes, ","))
	query.Set("state", nonce)
	authorization.RawQuery = query.Encode()
	setManagedThreadsOAuthBrowserCookie(r, nonce, browserVerifier)
	setManagedThreadsOAuthNoStoreHeaders(r)
	return r.SendEnvelope(map[string]any{
		"provider":          integrationProviderThreads,
		"mode":              "managed_oauth",
		"ready":             true,
		"reconnect":         snapshot.ReconnectBindingID != uuid.Nil,
		"authorization_url": authorization.String(),
		"expires_at":        state.ExpiresAt,
	})
}

func (a *App) prepareManagedThreadsStart(
	organizationID, userID uuid.UUID,
) (managedThreadsStartSnapshot, error) {
	var snapshot managedThreadsStartSnapshot
	root := a.rootApp()
	if root == nil || root.ThreadsManagedRuntime == nil {
		return snapshot, errManagedThreadsUnavailable
	}
	err := root.WithCommittedTenantApp(organizationID, func(scoped *App) error {
		if err := lockChannelAIOrganizationScopeTx(scoped.DB, organizationID); err != nil {
			return err
		}
		if err := scoped.authorizeManagedThreadsUser(organizationID, userID, ""); err != nil {
			return err
		}
		var integration models.ProviderIntegration
		lookup := scoped.DB.Clauses(clause.Locking{Strength: "UPDATE"}).Where(
			"organization_id = ? AND provider = ?",
			organizationID,
			integrationProviderThreads,
		).First(&integration)
		isNew := errors.Is(lookup.Error, gorm.ErrRecordNotFound)
		if lookup.Error != nil && !isNew {
			return lookup.Error
		}
		var app threadsplatform.AppDescriptor
		if isNew {
			var appErr error
			app, appErr = root.ThreadsManagedRuntime.SoleApp()
			if appErr != nil {
				return errManagedThreadsUnavailable
			}
			key := app.PlatformAppKey
			integration = models.ProviderIntegration{
				BaseModel:      models.BaseModel{ID: uuid.New()},
				OrganizationID: organizationID,
				Provider:       integrationProviderThreads,
				ManagementMode: models.ThreadsManagementModePlatformManaged,
				PlatformAppKey: &key,
				Enabled:        false,
				Config:         managedThreadsIntegrationConfig(app.ConfigurationGeneration, managedThreadsAuthorizationRequiredState),
				CredentialData: models.JSONB{},
				CreatedByID:    &userID,
				UpdatedByID:    &userID,
			}
			if err := threadsplatform.ValidateIntegrationManagement(&integration); err != nil {
				return errManagedThreadsUnavailable
			}
			if err := scoped.DB.Create(&integration).Error; err != nil {
				return err
			}
		} else {
			if threadsplatform.EffectiveManagementMode(&integration) != models.ThreadsManagementModePlatformManaged {
				return &integrationClientError{
					status:  fasthttp.StatusConflict,
					message: "This workspace already uses its own Threads app; managed onboarding will not replace it",
				}
			}
			if threadsplatform.ValidateIntegrationManagement(&integration) != nil ||
				integration.PlatformAppKey == nil || integration.Enabled {
				return errManagedThreadsStale
			}
			var appErr error
			app, appErr = root.ThreadsManagedRuntime.App(*integration.PlatformAppKey)
			if appErr != nil || managedThreadsConfigGeneration(integration.Config) != app.ConfigurationGeneration {
				return errManagedThreadsStale
			}
		}
		if err := root.ThreadsManagedRuntime.ValidateActivation(threadsplatform.ActivationFacts{
			OrganizationID: organizationID, PlatformAppKey: app.PlatformAppKey,
			HasIntegrationWritePermission: true, HasThreadsEntitlement: true,
		}); err != nil {
			return errManagedThreadsForbidden
		}

		var bindings []models.ThreadsPlatformBinding
		if err := scoped.DB.Clauses(clause.Locking{Strength: "UPDATE"}).Where(
			"organization_id = ? AND integration_id = ? AND status IN ?",
			organizationID,
			integration.ID,
			[]string{
				models.ThreadsPlatformBindingStatusPending,
				models.ThreadsPlatformBindingStatusActive,
				models.ThreadsPlatformBindingStatusQuarantined,
			},
		).Order("created_at ASC, id ASC").Find(&bindings).Error; err != nil {
			return err
		}
		if len(bindings) > 1 {
			return errManagedThreadsStale
		}
		if len(bindings) == 1 {
			binding := bindings[0]
			if binding.ChannelAccountID == nil || *binding.ChannelAccountID == uuid.Nil ||
				binding.PlatformAppKey != app.PlatformAppKey || binding.PlatformAppID != app.AppID ||
				binding.ConfigurationGeneration != app.ConfigurationGeneration ||
				binding.AuthorizationGeneration == 0 {
				return errManagedThreadsStale
			}
			if err := scoped.validateManagedThreadsAccount(
				organizationID,
				*binding.ChannelAccountID,
				binding.AuthorityAssetID,
			); err != nil {
				return errManagedThreadsStale
			}
			snapshot.ReconnectBindingID = binding.ID
			snapshot.ReconnectAccountID = *binding.ChannelAccountID
			snapshot.ExpectedAuthorizationGeneration = binding.AuthorizationGeneration
		}
		// Reload timestamps exactly as PostgreSQL stored them before binding the
		// row fingerprint into Redis state.
		if err := scoped.DB.Where("id = ? AND organization_id = ?", integration.ID, organizationID).
			First(&integration).Error; err != nil {
			return err
		}
		snapshot.IntegrationID = integration.ID
		snapshot.IntegrationFingerprint = managedThreadsIntegrationFingerprint(&integration)
		snapshot.App = app
		return nil
	})
	return snapshot, err
}

func (a *App) sendManagedThreadsStartError(r *fastglue.Request, err error) error {
	var clientErr *integrationClientError
	if errors.As(err, &clientErr) {
		return r.SendErrorEnvelope(clientErr.status, clientErr.message, nil, "")
	}
	switch {
	case errors.Is(err, errManagedThreadsEntitlementDenied):
		return r.SendErrorEnvelope(
			fasthttp.StatusPaymentRequired,
			"Threads public replies are not included in this workspace's active plan",
			nil,
			"",
		)
	case errors.Is(err, errManagedThreadsForbidden):
		return r.SendErrorEnvelope(fasthttp.StatusForbidden, "Managed Threads authorization is not permitted", nil, "")
	case errors.Is(err, errManagedThreadsStale):
		return r.SendErrorEnvelope(fasthttp.StatusConflict, "Managed Threads authorization state needs reconciliation", nil, "")
	default:
		return r.SendErrorEnvelope(fasthttp.StatusServiceUnavailable, "Managed Threads onboarding is unavailable", nil, "")
	}
}

// CallbackManagedThreads is the single global provider callback. It has no
// normal request authentication; the server-side one-time state and browser
// verifier identify and re-authorize the exact initiating tenant and admin.
func (a *App) CallbackManagedThreads(r *fastglue.Request) error {
	setManagedThreadsOAuthNoStoreHeaders(r)
	stateNonce := strings.TrimSpace(string(r.RequestCtx.QueryArgs().Peek("state")))
	if stateNonce == "" || len(stateNonce) > managedThreadsMaxOpaqueState || a.rootApp().Redis == nil {
		a.redirectManagedThreadsCallback(r, "error")
		return nil
	}
	browserVerifier := string(
		r.RequestCtx.Request.Header.Cookie(managedThreadsOAuthBrowserCookieName(stateNonce)),
	)
	clearManagedThreadsOAuthBrowserCookie(r, stateNonce)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	state, err := a.consumeManagedThreadsOAuthState(ctx, stateNonce, browserVerifier)
	cancel()
	if err != nil {
		a.redirectManagedThreadsCallback(r, "error")
		return nil
	}
	if strings.TrimSpace(string(r.RequestCtx.QueryArgs().Peek("error"))) != "" {
		a.redirectManagedThreadsCallback(r, "cancelled")
		return nil
	}
	code := strings.TrimSpace(string(r.RequestCtx.QueryArgs().Peek("code")))
	if code == "" || len(code) > managedThreadsMaxAuthorizationCode {
		a.redirectManagedThreadsCallback(r, "error")
		return nil
	}
	if err := a.completeManagedThreadsOAuth(state, code); err != nil {
		a.Log.Warn("Managed Threads OAuth completion failed", "organization_id", state.OrganizationID)
		if errors.Is(err, threadsplatform.ErrBindingClaimConflict) {
			a.redirectManagedThreadsCallback(r, "reconcile")
			return nil
		}
		a.redirectManagedThreadsCallback(r, "error")
		return nil
	}
	a.redirectManagedThreadsCallback(r, "pending")
	return nil
}

func (a *App) completeManagedThreadsOAuth(
	state managedThreadsOAuthState,
	code string,
) error {
	organizationID, userID, _, _, err := parseManagedThreadsStateIDs(state)
	if err != nil || strings.TrimSpace(code) == "" || len(code) > managedThreadsMaxAuthorizationCode {
		return errManagedThreadsForbidden
	}
	if err := a.validateManagedThreadsRuntimeState(state); err != nil {
		return err
	}
	providerCtx, cancel := context.WithTimeout(context.Background(), managedThreadsProviderOperationLimit)
	defer cancel()
	var providerResult managedThreadsProviderResult
	err = a.withLockedManagedThreadsProviderAttempt(
		providerCtx,
		state,
		func() error {
			runtime := a.rootApp().ThreadsManagedRuntime
			credentials, credentialErr := runtime.Credentials(state.PlatformAppKey)
			if credentialErr != nil {
				return credentialErr
			}
			callbackURL, callbackErr := runtime.CallbackURL()
			if callbackErr != nil {
				return callbackErr
			}
			snapshot := threadsIntegrationSnapshot{
				AppID:       state.PlatformAppID,
				AppSecret:   strings.TrimSpace(credentials.AppSecret),
				RedirectURI: callbackURL,
			}
			shortToken, exchangeErr := a.exchangeThreadsAuthorizationCode(providerCtx, snapshot, code)
			if exchangeErr != nil {
				return exchangeErr
			}
			oauthSubjectID := strings.TrimSpace(string(shortToken.UserID))
			longToken, exchangeErr := a.exchangeThreadsLongLivedToken(
				providerCtx,
				credentials.AppSecret,
				shortToken.AccessToken,
			)
			if exchangeErr != nil {
				return exchangeErr
			}
			profile, profileErr := a.fetchThreadsProfile(providerCtx, longToken.AccessToken)
			if profileErr != nil {
				return profileErr
			}
			authorityProfileID := strings.TrimSpace(string(profile.ID))
			permissions, permissionErr := channelapi.NewThreadsAdapter(
				a.HTTPClient,
				a.integrationEncryptionKey(),
			).InspectTokenPermissionsForApp(
				providerCtx,
				longToken.AccessToken,
				oauthSubjectID,
				state.PlatformAppID,
			)
			if permissionErr != nil || permissions.UserID != oauthSubjectID {
				if permissionErr != nil {
					return permissionErr
				}
				return errManagedThreadsForbidden
			}
			if identityErr := runtime.ValidateProviderIdentity(
				state.PlatformAppKey,
				oauthSubjectID,
				authorityProfileID,
			); identityErr != nil {
				return errManagedThreadsForbidden
			}
			if longToken.ExpiresIn <= 0 {
				longToken.ExpiresIn = 60 * 24 * 60 * 60
			}
			providerResult = managedThreadsProviderResult{
				OAuthSubjectID: oauthSubjectID,
				Profile:        profile,
				AccessToken:    strings.TrimSpace(longToken.AccessToken),
				ExpiresIn:      longToken.ExpiresIn,
				Permissions:    permissions,
			}
			return nil
		},
	)
	if err != nil {
		return err
	}
	encryptedToken, err := appcrypto.Encrypt(providerResult.AccessToken, a.integrationEncryptionKey())
	if err != nil || !appcrypto.IsEncrypted(encryptedToken) {
		return errors.New("managed Threads authorization could not be protected")
	}
	return a.rootApp().WithCommittedTenantApp(organizationID, func(scoped *App) error {
		if err := scoped.authorizeManagedThreadsStateTx(state, true); err != nil {
			return err
		}
		if err := scoped.rootApp().ThreadsManagedRuntime.ValidateProviderIdentity(
			state.PlatformAppKey,
			providerResult.OAuthSubjectID,
			strings.TrimSpace(string(providerResult.Profile.ID)),
		); err != nil {
			return errManagedThreadsForbidden
		}
		return scoped.persistManagedThreadsPendingTx(
			state,
			userID,
			providerResult,
			encryptedToken,
		)
	})
}

func (a *App) withLockedManagedThreadsProviderAttempt(
	ctx context.Context,
	state managedThreadsOAuthState,
	attempt func() error,
) error {
	organizationID, _, _, _, err := parseManagedThreadsStateIDs(state)
	if err != nil || a.rootApp() == nil || a.rootApp().DB == nil {
		return errManagedThreadsForbidden
	}
	return database.WithTenantReadCommitted(
		a.rootApp().DB.WithContext(ctx),
		organizationID,
		func(tx *gorm.DB) error {
			scoped := a.rootApp().scopedApp(tx, organizationID)
			if err := scoped.authorizeManagedThreadsStateTx(state, true); err != nil {
				return err
			}
			if attempt == nil {
				return nil
			}
			return attempt()
		},
	)
}

func (a *App) authorizeManagedThreadsUser(
	organizationID, userID uuid.UUID,
	platformAppKey string,
) error {
	if !a.HasPermission(userID, models.ResourceSettingsIntegrations, models.ActionWrite, organizationID) ||
		!a.HasPermission(userID, models.ResourceChannelAccounts, models.ActionWrite, organizationID) {
		return errManagedThreadsForbidden
	}
	allowed, err := a.HasProductEntitlement(
		userID,
		organizationID,
		channelapi.ThreadsPublicEngagementEntitlementKey,
	)
	if err != nil {
		return err
	}
	if !allowed {
		return errManagedThreadsEntitlementDenied
	}
	if platformAppKey != "" {
		if err := a.rootApp().ThreadsManagedRuntime.ValidateActivation(threadsplatform.ActivationFacts{
			OrganizationID: organizationID, PlatformAppKey: platformAppKey,
			HasIntegrationWritePermission: true, HasThreadsEntitlement: true,
		}); err != nil {
			return errManagedThreadsForbidden
		}
	}
	return nil
}

func (a *App) authorizeManagedThreadsStateTx(
	state managedThreadsOAuthState,
	lock bool,
) error {
	organizationID, userID, bindingID, accountID, err := parseManagedThreadsStateIDs(state)
	if err != nil || a.rootApp().ThreadsManagedRuntime == nil {
		return errManagedThreadsForbidden
	}
	if err := a.validateManagedThreadsRuntimeState(state); err != nil {
		return err
	}
	if lock {
		if err := lockChannelAIOrganizationScopeTx(a.DB, organizationID); err != nil {
			return err
		}
	}
	if err := a.authorizeManagedThreadsUser(organizationID, userID, state.PlatformAppKey); err != nil {
		return err
	}
	// Use fresh statement sessions for each model. Reusing a chained GORM
	// handle here can carry the integration WHERE clause into the reconnect
	// binding lookup, making every otherwise valid reconnect look stale.
	integrationQuery := a.DB.Session(&gorm.Session{})
	if lock {
		integrationQuery = integrationQuery.Clauses(clause.Locking{Strength: "UPDATE"})
	}
	var integration models.ProviderIntegration
	if err := integrationQuery.Where(
		"id = ? AND organization_id = ? AND provider = ?",
		state.IntegrationID,
		organizationID,
		integrationProviderThreads,
	).First(&integration).Error; err != nil {
		return errManagedThreadsStale
	}
	if threadsplatform.ValidateIntegrationManagement(&integration) != nil ||
		threadsplatform.EffectiveManagementMode(&integration) != models.ThreadsManagementModePlatformManaged ||
		integration.PlatformAppKey == nil || *integration.PlatformAppKey != state.PlatformAppKey ||
		integration.Enabled || managedThreadsConfigGeneration(integration.Config) != state.ConfigurationGeneration ||
		!managedThreadsOpaqueEqual(
			managedThreadsIntegrationFingerprint(&integration),
			state.IntegrationFingerprint,
		) {
		return errManagedThreadsStale
	}
	if bindingID == uuid.Nil {
		var live int64
		if err := a.DB.Model(&models.ThreadsPlatformBinding{}).Where(
			"organization_id = ? AND integration_id = ? AND status IN ?",
			organizationID,
			integration.ID,
			[]string{
				models.ThreadsPlatformBindingStatusPending,
				models.ThreadsPlatformBindingStatusActive,
				models.ThreadsPlatformBindingStatusQuarantined,
			},
		).Count(&live).Error; err != nil || live != 0 {
			return errManagedThreadsStale
		}
		return nil
	}
	bindingQuery := a.DB.Session(&gorm.Session{})
	if lock {
		bindingQuery = bindingQuery.Clauses(clause.Locking{Strength: "UPDATE"})
	}
	var binding models.ThreadsPlatformBinding
	if err := bindingQuery.Where(
		"id = ? AND organization_id = ? AND integration_id = ? AND channel_account_id = ?",
		bindingID,
		organizationID,
		integration.ID,
		accountID,
	).First(&binding).Error; err != nil {
		return errManagedThreadsStale
	}
	if binding.PlatformAppKey != state.PlatformAppKey ||
		binding.PlatformAppID != state.PlatformAppID ||
		binding.ConfigurationGeneration != state.ConfigurationGeneration ||
		binding.AuthorizationGeneration != state.ExpectedAuthorizationGeneration ||
		(binding.Status != models.ThreadsPlatformBindingStatusPending &&
			binding.Status != models.ThreadsPlatformBindingStatusActive &&
			binding.Status != models.ThreadsPlatformBindingStatusQuarantined) {
		return errManagedThreadsStale
	}
	return a.validateManagedThreadsAccount(organizationID, accountID, binding.AuthorityAssetID)
}

func (a *App) validateManagedThreadsRuntimeState(state managedThreadsOAuthState) error {
	runtime := a.rootApp().ThreadsManagedRuntime
	if runtime == nil || state.ConfigurationGeneration == 0 ||
		!runtime.OrganizationAllowed(mustParseManagedThreadsOrganizationID(state.OrganizationID)) {
		return errManagedThreadsForbidden
	}
	app, err := runtime.App(state.PlatformAppKey)
	if err != nil || app.AppID != state.PlatformAppID ||
		app.AppReviewStatus != state.AppReviewStatus ||
		app.ConfigurationGeneration != state.ConfigurationGeneration {
		return errManagedThreadsStale
	}
	return nil
}

func (a *App) validateManagedThreadsAccount(
	organizationID, accountID uuid.UUID,
	authorityAssetID string,
) error {
	var account models.ChannelAccount
	if err := a.DB.Where(
		"id = ? AND organization_id = ? AND channel = ? AND provider = ? AND external_account_id = ?",
		accountID,
		organizationID,
		models.ChannelThreads,
		channelapi.ThreadsProvider,
		authorityAssetID,
	).First(&account).Error; err != nil {
		return err
	}
	if stringConfigValue(account.Config, "management_mode") != models.ThreadsManagementModePlatformManaged ||
		managedThreadsConfigBool(account.Config, "outbound_enabled") ||
		managedThreadsConfigBool(account.Config, "activation_available") ||
		managedThreadsConfigBool(account.Config, "routing_enabled") ||
		account.IsDefaultIncoming || account.IsDefaultOutgoing {
		return errManagedThreadsStale
	}
	return nil
}

func (a *App) persistManagedThreadsPendingTx(
	state managedThreadsOAuthState,
	userID uuid.UUID,
	provider managedThreadsProviderResult,
	encryptedToken string,
) error {
	organizationID, _, bindingID, accountID, err := parseManagedThreadsStateIDs(state)
	if err != nil {
		return errManagedThreadsForbidden
	}
	profileID := strings.TrimSpace(string(provider.Profile.ID))
	if !validThreadsOAuthID(provider.OAuthSubjectID) || !validThreadsOAuthID(profileID) ||
		!appcrypto.IsEncrypted(encryptedToken) {
		return errManagedThreadsForbidden
	}
	now := time.Now().UTC()
	expiresAt := now.Add(time.Duration(provider.ExpiresIn) * time.Second)
	for _, providerExpiry := range []*time.Time{
		provider.Permissions.ExpiresAt,
		provider.Permissions.DataAccessExpiresAt,
	} {
		if providerExpiry != nil && providerExpiry.Before(expiresAt) {
			expiresAt = providerExpiry.UTC()
		}
	}
	var account models.ChannelAccount
	if bindingID == uuid.Nil {
		var collision int64
		if err := a.DB.Unscoped().Model(&models.ChannelAccount{}).Where(
			"organization_id = ? AND channel = ? AND provider = ? AND external_account_id = ?",
			organizationID,
			models.ChannelThreads,
			channelapi.ThreadsProvider,
			profileID,
		).Count(&collision).Error; err != nil {
			return err
		}
		if collision != 0 {
			return threadsplatform.ErrBindingClaimConflict
		}
		account = models.ChannelAccount{
			BaseModel:         models.BaseModel{ID: uuid.New()},
			OrganizationID:    organizationID,
			Channel:           models.ChannelThreads,
			Provider:          channelapi.ThreadsProvider,
			ExternalAccountID: profileID,
			CreatedByID:       &userID,
		}
	} else {
		if err := a.DB.Clauses(clause.Locking{Strength: "UPDATE"}).Where(
			"id = ? AND organization_id = ? AND channel = ? AND provider = ? AND external_account_id = ?",
			accountID,
			organizationID,
			models.ChannelThreads,
			channelapi.ThreadsProvider,
			profileID,
		).First(&account).Error; err != nil {
			return errManagedThreadsStale
		}
	}
	username := strings.TrimSpace(provider.Profile.Username)
	accountName := "Threads"
	if username != "" {
		accountName = "Threads @" + username
	}
	account.Name = truncateChannelRunes(accountName, 120)
	account.Status = models.ChannelAccountStatusPending
	account.Config = models.JSONB{
		"management_mode":      models.ThreadsManagementModePlatformManaged,
		"authorization_state":  managedThreadsPendingActivationState,
		"outbound_enabled":     false,
		"activation_available": false,
		"routing_enabled":      false,
		"webhook_enabled":      false,
		"ai_reply_enabled":     false,
	}
	account.Capabilities = models.JSONB{
		"text": false, "replies": false, "public_replies": false, "mentions": false,
	}
	account.Metadata = models.JSONB{
		"username":                username,
		"display_name":            strings.TrimSpace(provider.Profile.Name),
		"profile_picture_url":     strings.TrimSpace(provider.Profile.ProfilePictureURL),
		"granted_scopes":          append([]string(nil), provider.Permissions.Scopes...),
		"permissions_verified_at": provider.Permissions.CheckedAt.UTC().Format(time.RFC3339Nano),
	}
	account.IsDefaultIncoming = false
	account.IsDefaultOutgoing = false
	account.ConnectedAt = nil
	account.LastHealthCheckAt = nil
	account.LastError = ""
	account.LastErrorAt = nil
	account.UpdatedByID = &userID
	if bindingID == uuid.Nil {
		if err := a.DB.Create(&account).Error; err != nil {
			return err
		}
	} else if err := a.DB.Save(&account).Error; err != nil {
		return err
	}

	var maximumVersion int
	if err := a.DB.Model(&models.ChannelCredential{}).Where(
		"organization_id = ? AND channel_account_id = ? AND kind = ?",
		organizationID,
		account.ID,
		models.ChannelCredentialKindOAuth,
	).Select("COALESCE(MAX(version), 0)").Scan(&maximumVersion).Error; err != nil {
		return err
	}
	if err := a.DB.Model(&models.ChannelCredential{}).Where(
		"organization_id = ? AND channel_account_id = ? AND kind = ? AND status IN ?",
		organizationID,
		account.ID,
		models.ChannelCredentialKindOAuth,
		[]models.ChannelCredentialStatus{
			models.ChannelCredentialStatusActive,
			models.ChannelCredentialStatusExpiring,
		},
	).Updates(map[string]any{
		"status":     models.ChannelCredentialStatusRevoked,
		"revoked_at": now,
		"rotated_at": now,
		"updated_at": now,
	}).Error; err != nil {
		return err
	}
	credential := models.ChannelCredential{
		BaseModel:        models.BaseModel{ID: uuid.New()},
		OrganizationID:   organizationID,
		ChannelAccountID: account.ID,
		Kind:             models.ChannelCredentialKindOAuth,
		Version:          maximumVersion + 1,
		CredentialBlob:   models.JSONB{"access_token": encryptedToken},
		Status:           models.ChannelCredentialStatusActive,
		KeyVersion:       "app:v1",
		ExpiresAt:        &expiresAt,
		LastValidatedAt:  &now,
		Metadata: models.JSONB{
			"token_type":               "long_lived",
			"configuration_generation": state.ConfigurationGeneration,
			"granted_scopes":           append([]string(nil), provider.Permissions.Scopes...),
			"permissions_checked_at":   provider.Permissions.CheckedAt.UTC().Format(time.RFC3339Nano),
		},
	}
	if err := a.DB.Create(&credential).Error; err != nil {
		return err
	}
	if bindingID == uuid.Nil {
		if _, err := threadsplatform.ClaimBindingTx(a.DB, threadsplatform.BindingClaim{
			OrganizationID: organizationID, IntegrationID: mustParseManagedThreadsIntegrationID(state.IntegrationID),
			ChannelAccountID: &account.ID, PlatformAppKey: state.PlatformAppKey,
			PlatformAppID: state.PlatformAppID, OAuthSubjectID: provider.OAuthSubjectID,
			AuthorityAssetID: profileID, ConfigurationGeneration: state.ConfigurationGeneration,
			AuthorizationGeneration: 1, ClaimedAt: now,
		}); err != nil {
			return err
		}
	} else {
		if _, err := threadsplatform.ReconnectBindingTx(a.DB, threadsplatform.BindingReconnect{
			OrganizationID: organizationID, BindingID: bindingID,
			IntegrationID: mustParseManagedThreadsIntegrationID(state.IntegrationID), ChannelAccountID: account.ID,
			PlatformAppKey: state.PlatformAppKey, PlatformAppID: state.PlatformAppID,
			OAuthSubjectID: provider.OAuthSubjectID, AuthorityAssetID: profileID,
			ConfigurationGeneration:         state.ConfigurationGeneration,
			ExpectedAuthorizationGeneration: state.ExpectedAuthorizationGeneration,
		}); err != nil {
			return err
		}
	}
	var integration models.ProviderIntegration
	if err := a.DB.Clauses(clause.Locking{Strength: "UPDATE"}).Where(
		"id = ? AND organization_id = ? AND provider = ?",
		state.IntegrationID,
		organizationID,
		integrationProviderThreads,
	).First(&integration).Error; err != nil {
		return err
	}
	integration.Enabled = false
	integration.Config = managedThreadsIntegrationConfig(
		state.ConfigurationGeneration,
		managedThreadsPendingActivationState,
	)
	integration.CredentialData = models.JSONB{}
	integration.LastTestedAt = &now
	integration.LastSuccessfulAt = &now
	integration.LastErrorCode = ""
	integration.LastErrorMessage = ""
	integration.ValidationToken = ""
	integration.UpdatedByID = &userID
	if err := a.DB.Save(&integration).Error; err != nil {
		return err
	}
	return audit.LogAudit(
		a.DB,
		organizationID,
		userID,
		audit.GetUserName(a.DB, userID),
		models.ResourceSettingsIntegrations,
		integration.ID,
		models.AuditActionUpdated,
		map[string]any{"provider": integrationProviderThreads, "managed_authorization": "requested"},
		map[string]any{
			"provider":              integrationProviderThreads,
			"managed_authorization": managedThreadsPendingActivationState,
			"account_id":            account.ID,
		},
		map[string]any{"field": "access_token", "old_value": "********", "new_value": "********"},
	)
}

func managedThreadsIntegrationConfig(generation uint64, state string) models.JSONB {
	return models.JSONB{
		"management_mode":          models.ThreadsManagementModePlatformManaged,
		"configuration_generation": generation,
		"authorization_state":      state,
		"routing_enabled":          false,
		"outbound_enabled":         false,
		"activation_available":     false,
	}
}

func managedThreadsConfigGeneration(config models.JSONB) uint64 {
	value, ok := config["configuration_generation"]
	if !ok {
		return 0
	}
	switch typed := value.(type) {
	case uint64:
		return typed
	case uint:
		return uint64(typed)
	case int:
		if typed > 0 {
			return uint64(typed)
		}
	case int64:
		if typed > 0 {
			return uint64(typed)
		}
	case float64:
		if typed > 0 && typed == float64(uint64(typed)) {
			return uint64(typed)
		}
	case json.Number:
		parsed, err := strconv.ParseUint(string(typed), 10, 64)
		if err == nil {
			return parsed
		}
	}
	return 0
}

func managedThreadsConfigBool(config models.JSONB, key string) bool {
	value, _ := config[key].(bool)
	return value
}

func managedThreadsIntegrationFingerprint(integration *models.ProviderIntegration) string {
	if integration == nil || integration.PlatformAppKey == nil {
		return ""
	}
	digest := sha256.Sum256([]byte(strings.Join([]string{
		integration.ID.String(),
		integration.OrganizationID.String(),
		integration.Provider,
		threadsplatform.EffectiveManagementMode(integration),
		*integration.PlatformAppKey,
		strconv.FormatUint(managedThreadsConfigGeneration(integration.Config), 10),
		strconv.FormatBool(integration.Enabled),
		integration.UpdatedAt.UTC().Format(time.RFC3339Nano),
	}, "\x00")))
	return hex.EncodeToString(digest[:])
}

func parseManagedThreadsStateIDs(
	state managedThreadsOAuthState,
) (organizationID, userID, bindingID, accountID uuid.UUID, err error) {
	organizationID, organizationErr := uuid.Parse(state.OrganizationID)
	userID, userErr := uuid.Parse(state.UserID)
	integrationID, integrationErr := uuid.Parse(state.IntegrationID)
	if organizationErr != nil || userErr != nil || integrationErr != nil ||
		organizationID == uuid.Nil || userID == uuid.Nil || integrationID == uuid.Nil ||
		state.IntegrationFingerprint == "" || !validThreadsOAuthID(state.PlatformAppID) ||
		state.PlatformAppKey == "" || state.ConfigurationGeneration == 0 {
		return uuid.Nil, uuid.Nil, uuid.Nil, uuid.Nil, errManagedThreadsForbidden
	}
	if state.ReconnectBindingID == "" && state.ReconnectAccountID == "" &&
		state.ExpectedAuthorizationGeneration == 0 {
		return organizationID, userID, uuid.Nil, uuid.Nil, nil
	}
	bindingID, bindingErr := uuid.Parse(state.ReconnectBindingID)
	accountID, accountErr := uuid.Parse(state.ReconnectAccountID)
	if bindingErr != nil || accountErr != nil || bindingID == uuid.Nil || accountID == uuid.Nil ||
		state.ExpectedAuthorizationGeneration == 0 {
		return uuid.Nil, uuid.Nil, uuid.Nil, uuid.Nil, errManagedThreadsForbidden
	}
	return organizationID, userID, bindingID, accountID, nil
}

func mustParseManagedThreadsOrganizationID(value string) uuid.UUID {
	id, err := uuid.Parse(value)
	if err != nil {
		return uuid.Nil
	}
	return id
}

func mustParseManagedThreadsIntegrationID(value string) uuid.UUID {
	id, err := uuid.Parse(value)
	if err != nil {
		return uuid.Nil
	}
	return id
}

func (a *App) redirectManagedThreadsCallback(r *fastglue.Request, status string) {
	basePath := ""
	if a != nil && a.Config != nil {
		basePath = strings.TrimRight(sanitizeRedirectPath(strings.TrimSpace(a.Config.Server.BasePath)), "/")
	}
	destination := basePath + "/settings/integrations?threads_managed=" + url.QueryEscape(status)
	setManagedThreadsOAuthNoStoreHeaders(r)
	r.RequestCtx.Response.Header.Set("Location", destination)
	r.RequestCtx.Response.SetStatusCode(fasthttp.StatusSeeOther)
}

func setManagedThreadsOAuthNoStoreHeaders(r *fastglue.Request) {
	if r == nil || r.RequestCtx == nil {
		return
	}
	r.RequestCtx.Response.Header.Set("Cache-Control", "no-store")
	r.RequestCtx.Response.Header.Set("Pragma", "no-cache")
	r.RequestCtx.Response.Header.Set("Referrer-Policy", "no-referrer")
}
