package handlers

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/shridarpatil/whatomate/internal/audit"
	channelapi "github.com/shridarpatil/whatomate/internal/channel"
	configpkg "github.com/shridarpatil/whatomate/internal/config"
	appcrypto "github.com/shridarpatil/whatomate/internal/crypto"
	"github.com/shridarpatil/whatomate/internal/database"
	"github.com/shridarpatil/whatomate/internal/metaregistry"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/valyala/fasthttp"
	"github.com/zerodha/fastglue"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	metaMessengerStartStatePrefix       = "integration:meta:messenger:onboarding:start:"
	metaMessengerSelectionStatePrefix   = "integration:meta:messenger:onboarding:selection:"
	metaMessengerOnboardingStateTTL     = 10 * time.Minute
	metaMessengerProviderOperationLimit = 90 * time.Second
	metaMessengerOnboardingMode         = "facebook_login_for_business"
	metaMessengerAwaitingRegistryState  = "awaiting_relay_registry"
	metaMessengerVerifyingSubscription  = "verifying_subscription"
	metaMessengerSubscriptionFailed     = "subscription_failed"
	metaMessengerReviewReadyState       = "review_relay_ready"
	metaMessengerManagementMode         = "meta_messenger_oauth"
	metaMessengerMaxAuthorizationCode   = 8192
	metaMessengerMaxOpaqueState         = 256

	metaMessengerSubscriptionDesiredStateKey        = "meta_subscription_desired_state"
	metaMessengerSubscriptionOperationIDKey         = "meta_subscription_operation_id"
	metaMessengerSubscriptionOperationStateKey      = "meta_subscription_operation_state"
	metaMessengerSubscriptionOperationExpiresKey    = "meta_subscription_operation_expires_at"
	metaMessengerSubscriptionOAuthCredentialIDKey   = "meta_subscription_oauth_credential_id"
	metaMessengerSubscriptionOAuthVersionKey        = "meta_subscription_oauth_version"
	metaMessengerSubscriptionWebhookCredentialIDKey = "meta_subscription_webhook_credential_id"
	metaMessengerSubscriptionWebhookVersionKey      = "meta_subscription_webhook_version"
	metaMessengerSubscriptionRemoteStateKey         = "meta_subscription_remote_state"
	metaMessengerSubscriptionRemoteConfirmedAtKey   = "meta_subscription_remote_confirmed_at"
	metaMessengerSubscriptionFencedOperationIDKey   = "meta_subscription_fenced_operation_id"
	metaMessengerSubscriptionFencedOperationEndKey  = "meta_subscription_fenced_operation_expires_at"
	metaMessengerSubscriptionFencedAckKey           = "meta_subscription_fenced_operation_acknowledged"
	metaMessengerSubscriptionFencedAckAtKey         = "meta_subscription_fenced_operation_acknowledged_at"
	metaMessengerAuthorizationTokenKindKey          = "meta_authorization_token_kind"
	metaMessengerAuthorizationGrantedAtKey          = "meta_authorized_at"
	metaMessengerAuthorizationStateKey              = "meta_authorization_state"
	metaMessengerAuthorizationExpiresAtKey          = "meta_authorization_expires_at"
	metaMessengerAuthorizationReconnectRequiredKey  = "meta_authorization_reconnect_required"

	metaMessengerSubscriptionDesiredSubscribed    = "subscribed"
	metaMessengerSubscriptionDesiredUnsubscribed  = "unsubscribed"
	metaMessengerSubscriptionRemoteUnknown        = "unknown"
	metaMessengerSubscriptionRemoteSubscribed     = "subscribed"
	metaMessengerSubscriptionRemoteUnsubscribed   = "unsubscribed"
	metaMessengerSubscriptionSubscribePending     = "subscribe_pending"
	metaMessengerSubscriptionSubscribeComplete    = "subscribe_complete"
	metaMessengerSubscriptionSubscribeFailed      = "subscribe_failed"
	metaMessengerSubscriptionUnsubscribePending   = "unsubscribe_pending"
	metaMessengerSubscriptionUnsubscribeConfirmed = "unsubscribe_confirmed"
	metaMessengerSubscriptionUnsubscribeFailed    = "unsubscribe_failed"

	// This lease is deliberately longer than the maximum provider operation.
	// Deprovisioning will not erase the recovery token or delete the tombstone
	// while an older subscribe request could still be completing at Meta.
	metaMessengerSubscriptionOperationLease = metaMessengerProviderOperationLimit + 30*time.Second
)

var (
	errMetaMessengerOnboardingDisabled = errors.New("managed Messenger onboarding is disabled")
	errMetaMessengerSelectionInvalid   = errors.New("the selected Page is not an owned selectable Page")
	errMetaMessengerSubscriptionFence  = errors.New("the Messenger subscription operation is no longer current")
)

type metaMessengerSubscriptionOperation struct {
	ID                  uuid.UUID
	OAuthCredentialID   uuid.UUID
	OAuthVersion        int
	WebhookCredentialID uuid.UUID
	WebhookVersion      int
	DesiredState        string
	State               string
	ExpiresAt           time.Time
}

type metaMessengerWorkspaceSummary struct {
	OrganizationID   string `json:"organization_id"`
	OrganizationName string `json:"organization_name"`
}

type metaMessengerStartState struct {
	ReconnectAccountID string    `json:"reconnect_account_id,omitempty"`
	OrganizationID     string    `json:"organization_id"`
	UserID             string    `json:"user_id"`
	Nonce              string    `json:"nonce"`
	ConfigFingerprint  string    `json:"config_fingerprint"`
	ExpiresAt          time.Time `json:"expires_at"`
}

type metaMessengerSelectionSession struct {
	ReconnectAccountID string                         `json:"reconnect_account_id,omitempty"`
	OrganizationID     string                         `json:"organization_id"`
	UserID             string                         `json:"user_id"`
	SessionID          string                         `json:"session_id"`
	ConfigFingerprint  string                         `json:"config_fingerprint"`
	Workspace          metaMessengerWorkspaceSummary  `json:"workspace"`
	Platform           metaMessengerPlatformUser      `json:"platform"`
	Businesses         []metaMessengerBusinessSummary `json:"businesses"`
	Pages              []metaMessengerStoredPage      `json:"pages"`
	GrantedScopes      []string                       `json:"granted_scopes"`
	EncryptedUserToken string                         `json:"encrypted_user_token"`
	TokenExpiresAt     *time.Time                     `json:"token_expires_at,omitempty"`
	ExpiresAt          time.Time                      `json:"expires_at"`
}

type startMetaMessengerOnboardingResponse struct {
	Provider     string    `json:"provider"`
	Channel      string    `json:"channel"`
	Mode         string    `json:"mode"`
	Nonce        string    `json:"nonce"`
	ExpiresAt    time.Time `json:"expires_at"`
	PublicConfig struct {
		AppID                       string `json:"app_id"`
		ConfigID                    string `json:"config_id"`
		GraphAPIVersion             string `json:"graph_api_version"`
		ResponseType                string `json:"response_type"`
		OverrideDefaultResponseType bool   `json:"override_default_response_type"`
	} `json:"public_config"`
}

type exchangeMetaMessengerOnboardingRequest struct {
	Code  string `json:"code"`
	Nonce string `json:"nonce"`
}

type exchangeMetaMessengerOnboardingResponse struct {
	SessionID      string                         `json:"session_id"`
	ExpiresAt      time.Time                      `json:"expires_at"`
	Platform       metaMessengerPlatformUser      `json:"platform"`
	Workspace      metaMessengerWorkspaceSummary  `json:"workspace"`
	Businesses     []metaMessengerBusinessSummary `json:"businesses"`
	Pages          []metaMessengerPageSummary     `json:"pages"`
	VerifiedScopes []string                       `json:"verified_scopes"`
}

type selectMetaMessengerOnboardingRequest struct {
	SessionID  string `json:"session_id"`
	BusinessID string `json:"business_id"`
	PageID     string `json:"page_id"`
}

type selectMetaMessengerOnboardingResponse struct {
	Account              ChannelAccountResponse `json:"account"`
	OnboardingState      string                 `json:"onboarding_state"`
	SubscriptionVerified bool                   `json:"subscription_verified"`
	RegistryRecognized   bool                   `json:"registry_recognized"`
}

func (a *App) metaMessengerOnboardingAvailable() bool {
	if a == nil || a.Redis == nil || !a.hasIntegrationEncryptionKey() {
		return false
	}
	_, err := a.metaMessengerOnboardingSettings()
	return err == nil
}

func (a *App) metaMessengerOrganizationAllowed(organizationID uuid.UUID) bool {
	if a == nil || a.Config == nil || organizationID == uuid.Nil {
		return false
	}
	if a.Config.MetaMessenger.AllowAllOrganizations {
		return true
	}
	needle := organizationID.String()
	for _, configured := range strings.Split(a.Config.MetaMessenger.AllowedOrganizationIDs, ",") {
		if strings.TrimSpace(configured) == needle {
			return true
		}
	}
	return false
}

// GetMetaMessengerOnboardingStatus exposes only the tenant-authorized feature
// readiness bit. It never returns provider identifiers or secrets.
func (a *App) GetMetaMessengerOnboardingStatus(r *fastglue.Request) error {
	orgID, _, _, err := a.requireMetaMessengerOnboardingAuth(r, models.ActionWrite, false)
	if err != nil {
		return nil
	}
	enabled := a.metaMessengerOnboardingAvailable()
	if enabled {
		ctx, cancel := context.WithTimeout(requestContext(r), time.Second)
		defer cancel()
		enabled = a.Redis.Ping(ctx).Err() == nil
	}
	return r.SendEnvelope(struct {
		OrganizationID uuid.UUID `json:"organization_id"`
		Enabled        bool      `json:"enabled"`
	}{OrganizationID: orgID, Enabled: enabled})
}

// StartMetaMessengerOnboarding creates a tenant- and user-bound one-time
// nonce. The browser passes the returned public values to FB.login with no
// scope parameter; app secrets and redirect URLs are never part of this flow.
func (a *App) StartMetaMessengerOnboarding(r *fastglue.Request) error {
	orgID, userID, _, err := a.requireMetaMessengerOnboardingAuth(r, models.ActionWrite, true)
	if err != nil {
		return nil
	}
	if allowed, err := a.requireMetaMessengerRateLimit(r, orgID, userID, "start", 12, time.Minute); !allowed {
		return err
	}
	return a.beginMetaMessengerOnboarding(r, orgID, userID, uuid.Nil)
}

func (a *App) ReconnectMetaMessengerOnboarding(r *fastglue.Request) error {
	orgID, userID, _, err := a.requireMetaMessengerOnboardingAuth(r, models.ActionWrite, true)
	if err != nil {
		return nil
	}
	accountID, err := parsePathUUID(r, "id", "channel account")
	if err != nil {
		return nil
	}
	if allowed, err := a.requireMetaMessengerRateLimit(r, orgID, userID, "reconnect", 8, time.Minute); !allowed {
		return err
	}
	var found bool
	err = database.WithTenantReadCommitted(a.rootApp().DB, orgID, func(tx *gorm.DB) error {
		var account models.ChannelAccount
		if err := tx.Where(
			"id = ? AND organization_id = ? AND channel = ? AND provider = ?",
			accountID, orgID, models.ChannelMessenger, channelapi.RelayProvider,
		).First(&account).Error; err != nil {
			return err
		}
		found = metaRegistryControlPlaneConfig(account.Config) &&
			stringConfigValue(account.Metadata, "meta_platform_app_id") == strings.TrimSpace(a.Config.MetaMessenger.AppID)
		return nil
	})
	if err != nil || !found {
		return r.SendErrorEnvelope(fasthttp.StatusNotFound, "Managed Messenger account not found", nil, "")
	}
	return a.beginMetaMessengerOnboarding(r, orgID, userID, accountID)
}

func (a *App) beginMetaMessengerOnboarding(
	r *fastglue.Request,
	orgID, userID, reconnectAccountID uuid.UUID,
) error {
	settings, err := a.metaMessengerOnboardingSettings()
	if err != nil || !a.metaMessengerOnboardingAvailable() {
		return r.SendErrorEnvelope(fasthttp.StatusServiceUnavailable, "Managed Messenger onboarding is unavailable", nil, "")
	}
	nonce := generateRandomString(48)
	now := time.Now().UTC()
	state := metaMessengerStartState{
		OrganizationID: orgID.String(), UserID: userID.String(), Nonce: nonce,
		ConfigFingerprint: a.metaMessengerOnboardingRuntimeFingerprint(settings),
		ExpiresAt:         now.Add(metaMessengerOnboardingStateTTL),
	}
	if reconnectAccountID != uuid.Nil {
		state.ReconnectAccountID = reconnectAccountID.String()
	}
	payload, err := json.Marshal(state)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to start Messenger onboarding", nil, "")
	}
	stored, err := a.Redis.SetNX(
		requestContext(r), metaMessengerStartStateKey(orgID, userID, nonce),
		payload, metaMessengerOnboardingStateTTL,
	).Result()
	if err != nil || !stored {
		return r.SendErrorEnvelope(fasthttp.StatusServiceUnavailable, "Managed Messenger onboarding is unavailable", nil, "")
	}
	response := startMetaMessengerOnboardingResponse{
		Provider: "meta", Channel: string(models.ChannelMessenger),
		Mode: metaMessengerOnboardingMode, Nonce: nonce, ExpiresAt: state.ExpiresAt,
	}
	response.PublicConfig.AppID = settings.AppID
	response.PublicConfig.ConfigID = settings.ConfigID
	response.PublicConfig.GraphAPIVersion = settings.GraphAPIVersion
	response.PublicConfig.ResponseType = "code"
	response.PublicConfig.OverrideDefaultResponseType = true
	setMetaMessengerNoStoreHeaders(r)
	return r.SendEnvelope(response)
}

// ExchangeMetaMessengerOnboarding consumes the one-time nonce, exchanges the
// JavaScript SDK authorization code server-side, validates the required five
// permissions, and returns only an ownership-aware Page inventory. No access
// token is returned to the browser.
func (a *App) ExchangeMetaMessengerOnboarding(r *fastglue.Request) error {
	orgID, userID, workspaceName, err := a.requireMetaMessengerOnboardingAuth(r, models.ActionWrite, true)
	if err != nil {
		return nil
	}
	settings, err := a.metaMessengerOnboardingSettings()
	if err != nil || a.Redis == nil || !a.hasIntegrationEncryptionKey() {
		return r.SendErrorEnvelope(fasthttp.StatusServiceUnavailable, "Managed Messenger onboarding is unavailable", nil, "")
	}
	var request exchangeMetaMessengerOnboardingRequest
	if err := decodeStrictMetaMessengerRequest(r, &request); err != nil {
		return nil
	}
	request.Code = strings.TrimSpace(request.Code)
	request.Nonce = strings.TrimSpace(request.Nonce)
	if request.Code == "" || len(request.Code) > metaMessengerMaxAuthorizationCode ||
		request.Nonce == "" || len(request.Nonce) > metaMessengerMaxOpaqueState {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "code and nonce are required", nil, "")
	}

	stateJSON, err := a.Redis.GetDel(
		requestContext(r),
		metaMessengerStartStateKey(orgID, userID, request.Nonce),
	).Bytes()
	if err != nil {
		if !errors.Is(err, redis.Nil) {
			a.Log.Warn("Messenger onboarding nonce lookup failed", "organization_id", orgID)
		}
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Messenger onboarding session is invalid or expired", nil, "")
	}
	var state metaMessengerStartState
	if json.Unmarshal(stateJSON, &state) != nil ||
		!metaMessengerOpaqueValuesEqual(state.Nonce, request.Nonce) ||
		state.OrganizationID != orgID.String() || state.UserID != userID.String() ||
		!state.ExpiresAt.After(time.Now().UTC()) {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Messenger onboarding session is invalid or expired", nil, "")
	}
	if !metaMessengerOpaqueValuesEqual(
		state.ConfigFingerprint,
		a.metaMessengerOnboardingRuntimeFingerprint(settings),
	) {
		return r.SendErrorEnvelope(fasthttp.StatusConflict, "Messenger onboarding settings changed; start again", nil, "")
	}

	ctx, cancel := context.WithTimeout(requestContext(r), metaMessengerProviderOperationLimit)
	defer cancel()
	token, err := a.exchangeMetaMessengerAuthorizationCode(ctx, request.Code)
	if err != nil {
		a.Log.Warn("Messenger authorization code exchange failed", "organization_id", orgID)
		return r.SendErrorEnvelope(fasthttp.StatusBadGateway, "Meta could not complete Messenger authorization", nil, "")
	}
	inspection, err := a.inspectMetaMessengerToken(ctx, token.AccessToken, true)
	if err != nil {
		a.Log.Warn("Messenger authorization permission validation failed", "organization_id", orgID)
		return r.SendErrorEnvelope(
			fasthttp.StatusBadRequest,
			metaMessengerAuthorizationValidationResponse(err),
			nil,
			"",
		)
	}
	platform, businesses, pages, err := a.discoverMetaMessengerInventory(
		ctx,
		token.AccessToken,
		inspection,
	)
	if err != nil {
		var staged *metaMessengerRevalidationError
		if errors.As(err, &staged) && staged != nil {
			_ = a.metaMessengerRevalidationFailure(
				orgID,
				metaMessengerStoredPage{},
				staged.Stage,
				staged.cause,
			)
		} else {
			a.Log.Warn("Messenger Page ownership discovery failed", "organization_id", orgID)
		}
		return r.SendErrorEnvelope(fasthttp.StatusBadGateway, "Meta Page ownership could not be verified", nil, "")
	}

	now := time.Now().UTC()
	expiresAt := now.Add(metaMessengerOnboardingStateTTL)
	for _, candidate := range []*time.Time{inspection.ExpiresAt, inspection.DataAccessExpiresAt} {
		if candidate != nil && candidate.Before(expiresAt) {
			expiresAt = candidate.UTC()
		}
	}
	if token.ExpiresIn > 0 {
		tokenExpiry := now.Add(time.Duration(token.ExpiresIn) * time.Second)
		if tokenExpiry.Before(expiresAt) {
			expiresAt = tokenExpiry
		}
	}
	if !expiresAt.After(now.Add(5 * time.Second)) {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Meta authorization expires too soon; start again", nil, "")
	}
	sessionID := generateRandomString(48)
	encryptedUserToken, err := appcrypto.Encrypt(token.AccessToken, a.integrationEncryptionKey())
	if err != nil || !appcrypto.IsEncrypted(encryptedUserToken) {
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Meta authorization could not be protected", nil, "")
	}
	workspace := metaMessengerWorkspaceSummary{
		OrganizationID:   orgID.String(),
		OrganizationName: workspaceName,
	}
	session := metaMessengerSelectionSession{
		ReconnectAccountID: state.ReconnectAccountID,
		OrganizationID:     orgID.String(),
		UserID:             userID.String(),
		SessionID:          sessionID,
		ConfigFingerprint:  a.metaMessengerOnboardingRuntimeFingerprint(settings),
		Workspace:          workspace,
		Platform:           platform,
		Businesses:         businesses,
		Pages:              pages,
		GrantedScopes:      inspection.Scopes,
		EncryptedUserToken: encryptedUserToken,
		TokenExpiresAt:     earliestMetaMessengerExpiry(inspection.ExpiresAt, inspection.DataAccessExpiresAt),
		ExpiresAt:          expiresAt,
	}
	sessionJSON, err := json.Marshal(session)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to prepare Messenger Page selection", nil, "")
	}
	if err := a.Redis.Set(
		requestContext(r),
		metaMessengerSelectionStateKey(orgID, userID, sessionID),
		sessionJSON,
		time.Until(expiresAt),
	).Err(); err != nil {
		a.Log.Error("Failed to store Messenger Page selection", "error", err, "organization_id", orgID)
		return r.SendErrorEnvelope(fasthttp.StatusServiceUnavailable, "Messenger Page selection is unavailable", nil, "")
	}
	pageResponse := make([]metaMessengerPageSummary, 0, len(pages))
	for _, page := range pages {
		pageResponse = append(pageResponse, page.metaMessengerPageSummary)
	}
	setMetaMessengerNoStoreHeaders(r)
	return r.SendEnvelope(exchangeMetaMessengerOnboardingResponse{
		SessionID:      sessionID,
		ExpiresAt:      expiresAt,
		Platform:       platform,
		Workspace:      workspace,
		Businesses:     businesses,
		Pages:          pageResponse,
		VerifiedScopes: append([]string(nil), inspection.Scopes...),
	})
}

// SelectMetaMessengerOnboarding consumes the ownership inventory, revalidates
// the Page token, persists a disabled pending account, and proves the exact
// configured provider app is subscribed to the Page's messages field.
// Protected relay inventory remains a separate operator-controlled activation gate.
func (a *App) SelectMetaMessengerOnboarding(r *fastglue.Request) error {
	orgID, userID, _, err := a.requireMetaMessengerOnboardingAuth(r, models.ActionWrite, true)
	if err != nil {
		return nil
	}
	if allowed, err := a.requireMetaMessengerRateLimit(r, orgID, userID, "select", 8, time.Minute); !allowed {
		return err
	}
	settings, err := a.metaMessengerOnboardingSettings()
	if err != nil || a.Redis == nil || !a.hasIntegrationEncryptionKey() {
		return r.SendErrorEnvelope(fasthttp.StatusServiceUnavailable, "Managed Messenger onboarding is unavailable", nil, "")
	}
	var request selectMetaMessengerOnboardingRequest
	if err := decodeStrictMetaMessengerRequest(r, &request); err != nil {
		return nil
	}
	request.SessionID = strings.TrimSpace(request.SessionID)
	request.BusinessID = strings.TrimSpace(request.BusinessID)
	request.PageID = strings.TrimSpace(request.PageID)
	if request.SessionID == "" || len(request.SessionID) > metaMessengerMaxOpaqueState ||
		!validCanonicalMetaID(request.BusinessID) || !validCanonicalMetaID(request.PageID) {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "session_id, business_id, and page_id are required", nil, "")
	}
	sessionJSON, err := a.Redis.GetDel(
		requestContext(r), metaMessengerSelectionStateKey(orgID, userID, request.SessionID),
	).Bytes()
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Messenger Page selection is invalid or expired", nil, "")
	}
	var session metaMessengerSelectionSession
	if json.Unmarshal(sessionJSON, &session) != nil ||
		!metaMessengerOpaqueValuesEqual(session.SessionID, request.SessionID) ||
		session.OrganizationID != orgID.String() || session.UserID != userID.String() ||
		!session.ExpiresAt.After(time.Now().UTC()) ||
		!metaMessengerOpaqueValuesEqual(session.ConfigFingerprint, a.metaMessengerOnboardingRuntimeFingerprint(settings)) {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Messenger Page selection is invalid or expired", nil, "")
	}
	selected, err := selectMetaMessengerOwnedPage(session.Pages, request.BusinessID, request.PageID)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "The selected Page is not owned by the selected Business Portfolio", nil, "")
	}
	userToken, err := appcrypto.Decrypt(session.EncryptedUserToken, a.integrationEncryptionKey())
	if err != nil || strings.TrimSpace(userToken) == "" {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Messenger authorization is invalid; start again", nil, "")
	}
	ctx, cancel := context.WithTimeout(requestContext(r), metaMessengerProviderOperationLimit)
	defer cancel()
	freshUserInspection, err := a.inspectMetaMessengerToken(ctx, userToken, true)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Meta authorization changed; start again", nil, "")
	}
	freshPlatform, err := a.fetchMetaMessengerPlatformIdentity(ctx, userToken, freshUserInspection)
	if err != nil || freshPlatform.UserID != session.Platform.UserID ||
		freshPlatform.TokenKind != session.Platform.TokenKind ||
		freshPlatform.ClientBusinessID != session.Platform.ClientBusinessID {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Meta authorization identity changed; start again", nil, "")
	}
	if freshPlatform.TokenKind == metaMessengerTokenKindSystemUser &&
		request.BusinessID != freshPlatform.ClientBusinessID {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "The Business Portfolio no longer matches the authorized integration", nil, "")
	}
	selected, err = a.revalidateMetaMessengerOwnedPage(ctx, orgID, userToken, freshUserInspection, selected)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "The selected Page is no longer owned or accessible", nil, "")
	}
	pageToken, err := appcrypto.Decrypt(selected.EncryptedPageToken, a.integrationEncryptionKey())
	if err != nil || strings.TrimSpace(pageToken) == "" {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Messenger Page authorization is invalid; start again", nil, "")
	}
	pageInspection, pageName, err := a.bindMetaMessengerPageToken(
		ctx,
		request.PageID,
		pageToken,
		freshUserInspection,
	)
	if err != nil {
		_ = a.metaMessengerRevalidationFailure(
			orgID,
			selected,
			metaMessengerPageBindingStage(err),
			err,
		)
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Meta could not verify the selected Page token", nil, "")
	}
	if pageName != "" {
		selected.PageName = pageName
	}
	tokenExpiry := earliestMetaMessengerExpiry(
		freshUserInspection.ExpiresAt, freshUserInspection.DataAccessExpiresAt,
		pageInspection.ExpiresAt, pageInspection.DataAccessExpiresAt,
	)
	var result metaRegistryProvisionResult
	reconnectID, _ := uuid.Parse(session.ReconnectAccountID)
	operationID := uuid.New()
	operationExpiresAt := time.Now().UTC().Add(metaMessengerSubscriptionOperationLease)
	err = a.WithCommittedTenantApp(orgID, func(scoped *App) error {
		if reconnectID != uuid.Nil {
			var rotateErr error
			result, rotateErr = scoped.rotateMetaMessengerBinding(metaMessengerRotateInput{
				AccountID: reconnectID, OrganizationID: orgID, UserID: userID,
				Page: selected, Platform: freshPlatform, Inspection: freshUserInspection,
				PageToken: pageToken, AuthorityToken: userToken, TokenExpiresAt: tokenExpiry,
				SubscriptionOperationID: operationID, SubscriptionOperationExpiresAt: operationExpiresAt,
			})
			return rotateErr
		}
		var provisionErr error
		result, provisionErr = scoped.provisionMetaRegistryBinding(metaRegistryProvisionInput{
			OrganizationID: orgID, UserID: userID, Channel: models.ChannelMessenger,
			Name:              metaMessengerAccountName(selected.PageName, selected.PageID),
			ExternalAccountID: selected.PageID, WebhookApp: "messenger",
			PlatformAppID: settings.AppID, MetaBusinessID: selected.BusinessID,
			AuthorizingMetaUserID:  freshPlatform.UserID,
			AuthorizationTokenKind: freshPlatform.TokenKind,
			GrantedScopes:          freshUserInspection.Scopes, AccessToken: pageToken,
			AuthorityToken: userToken,
			TokenExpiresAt: tokenExpiry, OwnershipCheckedAt: pageInspection.CheckedAt,
			ReReplyBaseURL: settings.ReReplyBaseURL, RelayBaseURL: settings.RelayBaseURL,
			SubscriptionOperationID: operationID, SubscriptionOperationExpiresAt: operationExpiresAt,
		})
		return provisionErr
	})
	if err != nil {
		a.Log.Warn("Managed Messenger Page claim failed", "organization_id", orgID, "page_id", request.PageID)
		return r.SendErrorEnvelope(fasthttp.StatusConflict, "The Messenger Page could not be staged for this workspace", nil, "")
	}
	operation := result.SubscriptionOperation
	if err := a.preflightMetaMessengerSubscriptionOperation(
		orgID,
		result.Account.ID,
		operation,
		metaMessengerSubscriptionSubscribePending,
		time.Now().UTC(),
	); err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusConflict, "The Messenger Page subscription was superseded; reload before retrying", nil, "")
	}
	if err := a.subscribeMetaMessengerPage(ctx, request.PageID, pageToken); err != nil {
		_ = a.WithCommittedTenantApp(orgID, func(scoped *App) error {
			return scoped.recordMetaMessengerSubscriptionOperationFailure(
				orgID,
				result.Account.ID,
				operation,
				metaMessengerSubscriptionSubscribePending,
				metaMessengerSubscriptionSubscribeFailed,
				time.Now().UTC(),
			)
		})
		return r.SendErrorEnvelope(fasthttp.StatusBadGateway, "Meta did not verify the Page messages subscription", nil, "")
	}
	confirmedAt := time.Now().UTC()
	err = a.WithCommittedTenantApp(orgID, func(scoped *App) error {
		var finalizeErr error
		result, finalizeErr = scoped.finalizeMetaMessengerSubscribeOperation(
			orgID,
			userID,
			result.Account.ID,
			operation,
			confirmedAt,
		)
		return finalizeErr
	})
	if err != nil {
		a.Log.Warn(
			"Managed Messenger subscription finalization failed",
			"organization_id",
			orgID,
			"page_id",
			request.PageID,
			"operation_id",
			operation.ID,
		)
		return r.SendErrorEnvelope(fasthttp.StatusConflict, "The Messenger Page subscription is pending reconciliation", nil, "")
	}
	setMetaMessengerNoStoreHeaders(r)
	return r.SendEnvelope(selectMetaMessengerOnboardingResponse{
		Account:              channelAccountToResponse(&result.Account),
		OnboardingState:      "awaiting_health_and_approval",
		SubscriptionVerified: true, RegistryRecognized: true,
	})
}

func (a *App) metaMessengerOnboardingSettings() (configpkg.MetaMessengerConfig, error) {
	if a == nil || a.Config == nil || !a.Config.MetaMessenger.Enabled ||
		!a.Config.MetaRegistry.Enabled || !a.hasIntegrationEncryptionKey() {
		return configpkg.MetaMessengerConfig{}, errMetaMessengerOnboardingDisabled
	}
	settings := a.Config.MetaMessenger
	settings.AppID = strings.TrimSpace(settings.AppID)
	settings.ConfigID = strings.TrimSpace(settings.ConfigID)
	settings.GraphAPIVersion = strings.Trim(strings.TrimSpace(settings.GraphAPIVersion), "/")
	settings.GraphBaseURL = strings.TrimRight(strings.TrimSpace(settings.GraphBaseURL), "/")
	settings.ReReplyBaseURL = strings.TrimRight(strings.TrimSpace(settings.ReReplyBaseURL), "/")
	settings.RelayBaseURL = strings.TrimRight(strings.TrimSpace(settings.RelayBaseURL), "/")
	if !validCanonicalMetaID(settings.AppID) || !validCanonicalMetaID(settings.ConfigID) ||
		settings.AppSecret == "" || settings.GraphAPIVersion == "" ||
		settings.GraphBaseURL == "" || settings.ReReplyBaseURL == "" || settings.RelayBaseURL == "" {
		return configpkg.MetaMessengerConfig{}, errMetaMessengerOnboardingDisabled
	}
	return settings, nil
}

func metaMessengerOnboardingFingerprint(settings configpkg.MetaMessengerConfig) string {
	digest := sha256.Sum256([]byte(strings.Join([]string{
		settings.AppID,
		settings.ConfigID,
		settings.AppSecret,
		settings.GraphAPIVersion,
		settings.GraphBaseURL,
		settings.ReReplyBaseURL,
		settings.RelayBaseURL,
	}, "\x00")))
	return hex.EncodeToString(digest[:])
}

func (a *App) metaMessengerOnboardingRuntimeFingerprint(settings configpkg.MetaMessengerConfig) string {
	return metaMessengerOnboardingFingerprint(settings)
}

func metaMessengerStartStateKey(orgID, userID uuid.UUID, nonce string) string {
	return metaMessengerStartStatePrefix + orgID.String() + ":" + userID.String() + ":" + nonce
}

func metaMessengerSelectionStateKey(orgID, userID uuid.UUID, sessionID string) string {
	return metaMessengerSelectionStatePrefix + orgID.String() + ":" + userID.String() + ":" + sessionID
}

func metaMessengerOpaqueValuesEqual(left, right string) bool {
	if len(left) == 0 || len(left) != len(right) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(left), []byte(right)) == 1
}

func earliestMetaMessengerExpiry(values ...*time.Time) *time.Time {
	var earliest *time.Time
	for _, value := range values {
		if value == nil {
			continue
		}
		candidate := value.UTC()
		if earliest == nil || candidate.Before(*earliest) {
			copy := candidate
			earliest = &copy
		}
	}
	return earliest
}

func selectMetaMessengerOwnedPage(
	pages []metaMessengerStoredPage,
	businessID, pageID string,
) (metaMessengerStoredPage, error) {
	for _, page := range pages {
		if page.BusinessID == businessID && page.PageID == pageID &&
			page.Ownership == metaMessengerOwnershipOwned && page.Selectable &&
			!page.OwnershipVerifiedAt.IsZero() &&
			appcrypto.IsEncrypted(strings.TrimSpace(page.EncryptedPageToken)) {
			return page, nil
		}
	}
	return metaMessengerStoredPage{}, errMetaMessengerSelectionInvalid
}

func decodeStrictMetaMessengerRequest(r *fastglue.Request, destination any) error {
	if r == nil || r.RequestCtx == nil {
		return errEnvelopeSent
	}
	body := r.RequestCtx.PostBody()
	if len(body) == 0 || len(body) > 16<<10 {
		_ = r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid request body", nil, "")
		return errEnvelopeSent
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		_ = r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid request body", nil, "")
		return errEnvelopeSent
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		_ = r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid request body", nil, "")
		return errEnvelopeSent
	}
	return nil
}

func setMetaMessengerNoStoreHeaders(r *fastglue.Request) {
	if r == nil || r.RequestCtx == nil {
		return
	}
	r.RequestCtx.Response.Header.Set("Cache-Control", "no-store")
	r.RequestCtx.Response.Header.Set("Pragma", "no-cache")
	r.RequestCtx.Response.Header.Set("Referrer-Policy", "no-referrer")
}

func (a *App) requireMetaMessengerOnboardingAuth(
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
		_ = r.SendErrorEnvelope(fasthttp.StatusServiceUnavailable, "Messenger onboarding authorization is unavailable", nil, "")
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
				r,
				userID,
				models.ResourceSettingsIntegrations,
				models.ActionWrite,
			)
			if authErr != nil {
				return nil
			}
		}
		return tx.Model(&models.Organization{}).
			Where("id = ?", orgID).
			Pluck("name", &workspaceName).Error
	}); err != nil {
		a.Log.Error("Failed to authorize Messenger onboarding", "error", err, "organization_id", orgID)
		_ = r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to authorize Messenger onboarding", nil, "")
		return uuid.Nil, uuid.Nil, "", errEnvelopeSent
	}
	if authErr != nil {
		return uuid.Nil, uuid.Nil, "", authErr
	}
	if !a.metaMessengerOrganizationAllowed(orgID) {
		_ = r.SendErrorEnvelope(fasthttp.StatusNotFound, "Managed Messenger onboarding is not available for this workspace", nil, "")
		return uuid.Nil, uuid.Nil, "", errEnvelopeSent
	}
	workspaceName = strings.TrimSpace(workspaceName)
	if workspaceName == "" {
		workspaceName = "Workspace"
	}
	return orgID, userID, workspaceName, nil
}

type metaMessengerRotateInput struct {
	AccountID, OrganizationID, UserID uuid.UUID
	Page                              metaMessengerStoredPage
	Platform                          metaMessengerPlatformUser
	Inspection                        metaMessengerTokenInspection
	PageToken                         string
	AuthorityToken                    string
	TokenExpiresAt                    *time.Time
	SubscriptionOperationID           uuid.UUID
	SubscriptionOperationExpiresAt    time.Time
}

func metadataWithMetaMessengerSubscriptionOperation(
	metadata models.JSONB,
	operation metaMessengerSubscriptionOperation,
	remoteState string,
) models.JSONB {
	result := cloneJSONB(metadata)
	result[metaMessengerSubscriptionDesiredStateKey] = operation.DesiredState
	result[metaMessengerSubscriptionOperationIDKey] = operation.ID.String()
	result[metaMessengerSubscriptionOperationStateKey] = operation.State
	result[metaMessengerSubscriptionOperationExpiresKey] = operation.ExpiresAt.UTC().Format(time.RFC3339Nano)
	result[metaMessengerSubscriptionOAuthCredentialIDKey] = operation.OAuthCredentialID.String()
	result[metaMessengerSubscriptionOAuthVersionKey] = operation.OAuthVersion
	result[metaMessengerSubscriptionWebhookCredentialIDKey] = operation.WebhookCredentialID.String()
	result[metaMessengerSubscriptionWebhookVersionKey] = operation.WebhookVersion
	result[metaMessengerSubscriptionRemoteStateKey] = remoteState
	delete(result, metaMessengerSubscriptionRemoteConfirmedAtKey)
	clearMetaMessengerSubscriptionFence(result)
	return result
}

func clearMetaMessengerSubscriptionFence(metadata models.JSONB) {
	delete(metadata, metaMessengerSubscriptionFencedOperationIDKey)
	delete(metadata, metaMessengerSubscriptionFencedOperationEndKey)
	delete(metadata, metaMessengerSubscriptionFencedAckKey)
	delete(metadata, metaMessengerSubscriptionFencedAckAtKey)
}

func markMetaMessengerAuthorizationDurability(
	metadata models.JSONB,
	tokenKind string,
	expiresAt *time.Time,
) {
	if metadata == nil {
		return
	}
	tokenKind = strings.ToUpper(strings.TrimSpace(tokenKind))
	metadata[metaMessengerAuthorizationTokenKindKey] = tokenKind
	if tokenKind == metaMessengerTokenKindUser {
		metadata[metaMessengerAuthorizationStateKey] = "expiring"
		metadata[metaMessengerAuthorizationReconnectRequiredKey] = true
		if expiresAt != nil {
			metadata[metaMessengerAuthorizationExpiresAtKey] = expiresAt.UTC().Format(time.RFC3339Nano)
		} else {
			delete(metadata, metaMessengerAuthorizationExpiresAtKey)
		}
		return
	}
	metadata[metaMessengerAuthorizationStateKey] = "durable"
	metadata[metaMessengerAuthorizationReconnectRequiredKey] = false
	delete(metadata, metaMessengerAuthorizationExpiresAtKey)
}

func metaMessengerSubscriptionOperationFromMetadata(
	metadata models.JSONB,
) (metaMessengerSubscriptionOperation, bool) {
	operationID, operationErr := uuid.Parse(stringConfigValue(metadata, metaMessengerSubscriptionOperationIDKey))
	oauthCredentialID, oauthErr := uuid.Parse(stringConfigValue(metadata, metaMessengerSubscriptionOAuthCredentialIDKey))
	webhookCredentialID, webhookErr := uuid.Parse(stringConfigValue(metadata, metaMessengerSubscriptionWebhookCredentialIDKey))
	expiresAt, expiresErr := time.Parse(
		time.RFC3339Nano,
		stringConfigValue(metadata, metaMessengerSubscriptionOperationExpiresKey),
	)
	operation := metaMessengerSubscriptionOperation{
		ID:                  operationID,
		OAuthCredentialID:   oauthCredentialID,
		OAuthVersion:        intConfigValue(metadata, metaMessengerSubscriptionOAuthVersionKey),
		WebhookCredentialID: webhookCredentialID,
		WebhookVersion:      intConfigValue(metadata, metaMessengerSubscriptionWebhookVersionKey),
		DesiredState:        stringConfigValue(metadata, metaMessengerSubscriptionDesiredStateKey),
		State:               stringConfigValue(metadata, metaMessengerSubscriptionOperationStateKey),
		ExpiresAt:           expiresAt.UTC(),
	}
	return operation, operationErr == nil && oauthErr == nil && webhookErr == nil && expiresErr == nil &&
		operation.ID != uuid.Nil && operation.OAuthCredentialID != uuid.Nil && operation.OAuthVersion > 0 &&
		operation.WebhookCredentialID != uuid.Nil && operation.WebhookVersion > 0 &&
		operation.DesiredState != "" && operation.State != ""
}

func metaMessengerSubscriptionOperationMatches(
	metadata models.JSONB,
	want metaMessengerSubscriptionOperation,
	wantState string,
) bool {
	stored, ok := metaMessengerSubscriptionOperationFromMetadata(metadata)
	return ok && stored.ID == want.ID && stored.OAuthCredentialID == want.OAuthCredentialID &&
		stored.OAuthVersion == want.OAuthVersion && stored.WebhookCredentialID == want.WebhookCredentialID &&
		stored.WebhookVersion == want.WebhookVersion && stored.DesiredState == want.DesiredState &&
		stored.State == wantState && stored.ExpiresAt.Equal(want.ExpiresAt.UTC())
}

func metaMessengerSubscriptionOperationAvailable(metadata models.JSONB, _ time.Time) bool {
	if stringConfigValue(metadata, metaMessengerSubscriptionFencedOperationIDKey) != "" {
		// Only the privileged provider reconciliation path may clear a fence.
		// A timestamp or an acknowledgement bit alone never enables a generic
		// opposite operation.
		return false
	}
	if stringConfigValue(metadata, metaMessengerSubscriptionOperationIDKey) == "" {
		return true
	}
	stored, ok := metaMessengerSubscriptionOperationFromMetadata(metadata)
	if !ok {
		return false
	}
	switch stored.State {
	case metaMessengerSubscriptionSubscribeComplete, metaMessengerSubscriptionUnsubscribeConfirmed:
		return true
	default:
		// A committed pending claim may already have reached Meta before this
		// process crashed, and a failed state is explicitly ambiguous. Neither
		// becomes safe merely because the local lease expired. Only exact,
		// audited provider reconciliation may acknowledge it.
		return false
	}
}

func (a *App) rotateMetaMessengerBinding(input metaMessengerRotateInput) (metaRegistryProvisionResult, error) {
	var account models.ChannelAccount
	if input.OrganizationID != a.tenantOrgID || input.AccountID == uuid.Nil || input.UserID == uuid.Nil {
		return metaRegistryProvisionResult{}, metaregistry.ErrInvalidRequest
	}
	input.Platform.TokenKind = strings.ToUpper(strings.TrimSpace(input.Platform.TokenKind))
	if input.Platform.TokenKind != metaMessengerTokenKindSystemUser &&
		input.Platform.TokenKind != metaMessengerTokenKindUser {
		return metaRegistryProvisionResult{}, metaregistry.ErrInvalidRequest
	}
	if err := a.DB.Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("id = ? AND organization_id = ? AND channel = ? AND provider = ?",
			input.AccountID, input.OrganizationID, models.ChannelMessenger, channelapi.RelayProvider).
		First(&account).Error; err != nil {
		return metaRegistryProvisionResult{}, err
	}
	if !metaRegistryControlPlaneConfig(account.Config) ||
		account.ExternalAccountID != input.Page.PageID ||
		stringConfigValue(account.Metadata, "meta_business_id") != input.Page.BusinessID ||
		stringConfigValue(account.Metadata, "meta_platform_app_id") != strings.TrimSpace(a.Config.MetaMessenger.AppID) {
		return metaRegistryProvisionResult{}, metaregistry.ErrNotFound
	}
	now := time.Now().UTC()
	if input.Inspection.CheckedAt.IsZero() || input.Inspection.CheckedAt.Before(now.Add(-10*time.Minute)) ||
		input.Inspection.CheckedAt.After(now.Add(time.Minute)) {
		return metaRegistryProvisionResult{}, metaregistry.ErrInvalidRequest
	}
	if !metaMessengerSubscriptionOperationAvailable(account.Metadata, now) {
		return metaRegistryProvisionResult{}, errMetaMessengerSubscriptionFence
	}
	if input.SubscriptionOperationID == uuid.Nil {
		input.SubscriptionOperationID = uuid.New()
	}
	if input.SubscriptionOperationExpiresAt.IsZero() {
		input.SubscriptionOperationExpiresAt = now.Add(metaMessengerSubscriptionOperationLease)
	} else {
		input.SubscriptionOperationExpiresAt = input.SubscriptionOperationExpiresAt.UTC()
	}
	if !input.SubscriptionOperationExpiresAt.After(now) {
		return metaRegistryProvisionResult{}, metaregistry.ErrInvalidRequest
	}
	var credentials []models.ChannelCredential
	if err := a.DB.Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("organization_id = ? AND channel_account_id = ?", input.OrganizationID, input.AccountID).
		Order("version DESC").Find(&credentials).Error; err != nil {
		return metaRegistryProvisionResult{}, err
	}
	encrypt := func(value string) (string, error) {
		ciphertext, err := appcrypto.Encrypt(value, a.Config.App.EncryptionKey)
		if err != nil || !appcrypto.IsEncrypted(ciphertext) {
			return "", errors.New("meta registry credential encryption failed")
		}
		return ciphertext, nil
	}
	encryptedToken, err := encrypt(strings.TrimSpace(input.PageToken))
	if err != nil {
		return metaRegistryProvisionResult{}, err
	}
	encryptedAuthorityToken, err := encrypt(strings.TrimSpace(input.AuthorityToken))
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
		Version:        nextOAuthVersion,
		CredentialBlob: models.JSONB{"access_token": encryptedToken, "authority_token": encryptedAuthorityToken},
		Status:         models.ChannelCredentialStatusActive, KeyVersion: "app:v1",
		ExpiresAt: input.TokenExpiresAt, LastValidatedAt: &input.Inspection.CheckedAt, Metadata: models.JSONB{},
	}
	if err := a.DB.Create(&oauth).Error; err != nil {
		return metaRegistryProvisionResult{}, err
	}
	if result := a.DB.Model(&models.ChannelCredential{}).
		Where("organization_id = ? AND channel_account_id = ? AND kind = ? AND id <> ? AND status IN ?",
			input.OrganizationID, account.ID, models.ChannelCredentialKindOAuth, oauth.ID,
			[]models.ChannelCredentialStatus{models.ChannelCredentialStatusActive, models.ChannelCredentialStatusExpiring}).
		Updates(map[string]any{"status": models.ChannelCredentialStatusRevoked, "revoked_at": now}); result.Error != nil {
		return metaRegistryProvisionResult{}, result.Error
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
			Version:        nextWebhookVersion,
			CredentialBlob: models.JSONB{"inbound_secret": encryptedInbound, "outbound_secret": encryptedOutbound},
			Status:         models.ChannelCredentialStatusActive, KeyVersion: "app:v1", Metadata: models.JSONB{},
		}
		if err := a.DB.Create(&created).Error; err != nil {
			return metaRegistryProvisionResult{}, err
		}
		webhook = &created
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
	metadata := cloneJSONB(account.Metadata)
	metadata["meta_ownership_state"] = metaregistry.OwnershipVerified
	metadata["meta_ownership_checked_at"] = input.Inspection.CheckedAt.UTC().Format(time.RFC3339Nano)
	metadata["meta_authorizing_user_id"] = input.Platform.UserID
	metadata[metaMessengerAuthorizationGrantedAtKey] = input.Inspection.CheckedAt.UTC().Format(time.RFC3339Nano)
	metadata["meta_granted_scopes"] = append([]string(nil), input.Inspection.Scopes...)
	metadata["meta_subscription_state"] = metaMessengerVerifyingSubscription
	metadata["meta_activation_state"] = metaMessengerAwaitingRegistryState
	markMetaMessengerAuthorizationDurability(metadata, input.Platform.TokenKind, input.TokenExpiresAt)
	delete(metadata, "meta_deauthorized_at")
	metadata = metadataWithMetaMessengerSubscriptionOperation(
		metadata,
		operation,
		metaMessengerSubscriptionRemoteUnknown,
	)
	config := cloneJSONB(account.Config)
	config["outbound_enabled"] = false
	config["ai_reply_enabled"] = false
	if err := a.DB.Model(&models.ChannelAccount{}).
		Where("id = ? AND organization_id = ?", account.ID, input.OrganizationID).
		Updates(map[string]any{
			"status": models.ChannelAccountStatusPending, "metadata": metadata, "config": config,
			"last_health_check_at": nil, "last_error": "", "last_error_at": nil,
			"connected_at": nil, "updated_by_id": input.UserID, "updated_at": now,
		}).Error; err != nil {
		return metaRegistryProvisionResult{}, err
	}
	account.Status = models.ChannelAccountStatusPending
	account.Metadata = metadata
	account.Config = config
	account.LastHealthCheckAt = nil
	account.LastError = ""
	account.Credentials = []models.ChannelCredential{oauth, *webhook}
	if err := audit.LogAudit(
		a.DB, input.OrganizationID, input.UserID, audit.GetUserName(a.DB, input.UserID),
		"meta_channel_registry", account.ID, models.AuditActionUpdated,
		nil, map[string]any{"operation": "reconnect", "status": "pending"},
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

func (a *App) lockMetaMessengerSubscriptionOperation(
	organizationID uuid.UUID,
	accountID uuid.UUID,
	operation metaMessengerSubscriptionOperation,
	wantState string,
) (models.ChannelAccount, []models.ChannelCredential, error) {
	if a == nil || a.DB == nil || a.tenantOrgID == uuid.Nil || organizationID != a.tenantOrgID ||
		accountID == uuid.Nil || operation.ID == uuid.Nil {
		return models.ChannelAccount{}, nil, metaregistry.ErrInvalidRequest
	}
	var account models.ChannelAccount
	if err := a.DB.Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("id = ? AND organization_id = ? AND channel = ? AND provider = ?",
			accountID, organizationID, models.ChannelMessenger, channelapi.RelayProvider).
		First(&account).Error; err != nil {
		return models.ChannelAccount{}, nil, err
	}
	if !metaRegistryControlPlaneConfig(account.Config) ||
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
		"organization_id = ? AND channel_account_id = ? AND status IN ? AND ((id = ? AND version = ? AND kind = ?) OR (id = ? AND version = ? AND kind = ?))",
		organizationID,
		account.ID,
		[]models.ChannelCredentialStatus{
			models.ChannelCredentialStatusActive,
			models.ChannelCredentialStatusExpiring,
		},
		operation.OAuthCredentialID,
		operation.OAuthVersion,
		models.ChannelCredentialKindOAuth,
		operation.WebhookCredentialID,
		operation.WebhookVersion,
		models.ChannelCredentialKindWebhook,
	).Find(&credentials).Error; err != nil {
		return models.ChannelAccount{}, nil, err
	}
	if len(credentials) != 2 {
		return models.ChannelAccount{}, nil, errMetaMessengerSubscriptionFence
	}
	return account, credentials, nil
}

func (a *App) preflightMetaMessengerSubscriptionOperation(
	organizationID uuid.UUID,
	accountID uuid.UUID,
	operation metaMessengerSubscriptionOperation,
	wantState string,
	now time.Time,
) error {
	now = now.UTC()
	if operation.ExpiresAt.Before(now.Add(metaMessengerProviderOperationLimit)) {
		return errMetaMessengerSubscriptionFence
	}
	return a.WithCommittedTenantApp(organizationID, func(scoped *App) error {
		_, _, err := scoped.lockMetaMessengerSubscriptionOperation(
			organizationID,
			accountID,
			operation,
			wantState,
		)
		return err
	})
}

func (a *App) finalizeMetaMessengerSubscribeOperation(
	organizationID uuid.UUID,
	userID uuid.UUID,
	accountID uuid.UUID,
	operation metaMessengerSubscriptionOperation,
	confirmedAt time.Time,
) (metaRegistryProvisionResult, error) {
	account, credentials, err := a.lockMetaMessengerSubscriptionOperation(
		organizationID,
		accountID,
		operation,
		metaMessengerSubscriptionSubscribePending,
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
	result := a.DB.Model(&models.ChannelAccount{}).
		Where("id = ? AND organization_id = ?", account.ID, organizationID).
		Updates(map[string]any{
			"status":               models.ChannelAccountStatusPending,
			"metadata":             metadata,
			"config":               config,
			"last_health_check_at": nil,
			"last_error":           "",
			"last_error_at":        nil,
			"connected_at":         nil,
			"updated_by_id":        userID,
			"updated_at":           confirmedAt,
		})
	if result.Error != nil {
		return metaRegistryProvisionResult{}, result.Error
	}
	if result.RowsAffected != 1 {
		return metaRegistryProvisionResult{}, errMetaMessengerSubscriptionFence
	}
	if err := audit.LogAudit(
		a.DB,
		organizationID,
		userID,
		audit.GetUserName(a.DB, userID),
		"meta_channel_registry",
		account.ID,
		models.AuditActionUpdated,
		nil,
		map[string]any{"operation": operation.ID.String(), "subscription": "verified"},
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
		Account:               account,
		OAuthCredentialID:     operation.OAuthCredentialID,
		OAuthVersion:          operation.OAuthVersion,
		WebhookCredentialID:   operation.WebhookCredentialID,
		WebhookVersion:        operation.WebhookVersion,
		SubscriptionOperation: operation,
	}, nil
}

func (a *App) recordMetaMessengerSubscriptionOperationFailure(
	organizationID uuid.UUID,
	accountID uuid.UUID,
	operation metaMessengerSubscriptionOperation,
	wantState string,
	failureState string,
	failedAt time.Time,
) error {
	account, _, err := a.lockMetaMessengerSubscriptionOperation(
		organizationID,
		accountID,
		operation,
		wantState,
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
	result := a.DB.Model(&models.ChannelAccount{}).
		Where("id = ? AND organization_id = ?", account.ID, organizationID).
		Updates(map[string]any{
			"status":        models.ChannelAccountStatusPending,
			"config":        config,
			"metadata":      metadata,
			"last_error":    "Meta subscription operation was not confirmed",
			"last_error_at": failedAt.UTC(),
			"updated_at":    failedAt.UTC(),
		})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return errMetaMessengerSubscriptionFence
	}
	return nil
}

type metaMessengerReconciliationClaim struct {
	Account   models.ChannelAccount
	Operation metaMessengerSubscriptionOperation
	PageToken string
}

func (a *App) loadMetaMessengerReconciliationClaim(
	organizationID uuid.UUID,
	accountID uuid.UUID,
) (metaMessengerReconciliationClaim, error) {
	if a == nil || a.DB == nil || a.Config == nil || a.tenantOrgID != organizationID ||
		organizationID == uuid.Nil || accountID == uuid.Nil {
		return metaMessengerReconciliationClaim{}, metaregistry.ErrInvalidRequest
	}
	var account models.ChannelAccount
	if err := a.DB.Preload("Credentials", func(db *gorm.DB) *gorm.DB { return db.Order("version DESC") }).
		Where("id = ? AND organization_id = ? AND channel = ? AND provider = ?",
			accountID, organizationID, models.ChannelMessenger, channelapi.RelayProvider).
		First(&account).Error; err != nil {
		return metaMessengerReconciliationClaim{}, err
	}
	if !metaRegistryControlPlaneConfig(account.Config) ||
		stringConfigValue(account.Metadata, "meta_platform_app_id") != strings.TrimSpace(a.Config.MetaMessenger.AppID) {
		return metaMessengerReconciliationClaim{}, metaregistry.ErrNotFound
	}
	operation, ok := metaMessengerSubscriptionOperationFromMetadata(account.Metadata)
	if !ok {
		return metaMessengerReconciliationClaim{}, errMetaMessengerSubscriptionFence
	}
	switch operation.State {
	case metaMessengerSubscriptionSubscribePending,
		metaMessengerSubscriptionSubscribeFailed,
		metaMessengerSubscriptionUnsubscribePending,
		metaMessengerSubscriptionUnsubscribeFailed:
	default:
		return metaMessengerReconciliationClaim{}, errMetaMessengerSubscriptionFence
	}
	now := time.Now().UTC()
	oauth := currentMetaRegistryCredential(account.Credentials, models.ChannelCredentialKindOAuth, now)
	webhook := currentMetaRegistryCredential(account.Credentials, models.ChannelCredentialKindWebhook, now)
	if oauth == nil || webhook == nil || oauth.ID != operation.OAuthCredentialID ||
		oauth.Version != operation.OAuthVersion || webhook.ID != operation.WebhookCredentialID ||
		webhook.Version != operation.WebhookVersion {
		return metaMessengerReconciliationClaim{}, errMetaMessengerSubscriptionFence
	}
	pageToken, err := decryptRequiredMetaRegistrySecret(
		oauth.CredentialBlob,
		"access_token",
		a.Config.App.EncryptionKey,
	)
	if err != nil {
		return metaMessengerReconciliationClaim{}, err
	}
	return metaMessengerReconciliationClaim{Account: account, Operation: operation, PageToken: pageToken}, nil
}

func (a *App) acknowledgeMetaMessengerSubscriptionReconciliation(
	organizationID uuid.UUID,
	userID uuid.UUID,
	accountID uuid.UUID,
	operation metaMessengerSubscriptionOperation,
	confirmedAt time.Time,
) error {
	account, _, err := a.lockMetaMessengerSubscriptionOperation(
		organizationID,
		accountID,
		operation,
		operation.State,
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
	result := a.DB.Model(&models.ChannelAccount{}).
		Where("id = ? AND organization_id = ?", account.ID, organizationID).
		Updates(map[string]any{"metadata": metadata, "updated_by_id": userID, "updated_at": confirmedAt})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return errMetaMessengerSubscriptionFence
	}
	return audit.LogAudit(
		a.DB,
		organizationID,
		userID,
		audit.GetUserName(a.DB, userID),
		"meta_channel_registry",
		account.ID,
		models.AuditActionUpdated,
		nil,
		map[string]any{"operation": operation.ID.String(), "subscription_reconciled": operation.DesiredState},
	)
}

// ReconcileMetaMessengerSubscription repeats only the already-committed
// desired provider mutation, verifies the exact app/messages state, then
// acknowledges and finalizes the same credential/operation generation. It is
// the sole recovery path for crash/timeout ambiguity; generic opposite
// operations remain fenced until this succeeds.
func (a *App) ReconcileMetaMessengerSubscription(r *fastglue.Request) error {
	orgID, userID, _, err := a.requireMetaMessengerOnboardingAuth(r, models.ActionDelete, true)
	if err != nil {
		return nil
	}
	var writePermissionErr error
	if err := database.WithTenantReadCommitted(a.rootApp().DB, orgID, func(tx *gorm.DB) error {
		writePermissionErr = a.rootApp().scopedApp(tx, orgID).requirePermission(
			r,
			userID,
			models.ResourceChannelAccounts,
			models.ActionWrite,
		)
		return nil
	}); err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to authorize Messenger reconciliation", nil, "")
	}
	if writePermissionErr != nil {
		return nil
	}
	accountID, err := parsePathUUID(r, "id", "channel account")
	if err != nil {
		return nil
	}
	if allowed, err := a.requireMetaMessengerRateLimit(r, orgID, userID, "reconcile", 8, time.Minute); !allowed {
		return err
	}
	var claim metaMessengerReconciliationClaim
	err = database.WithTenantReadCommitted(a.rootApp().DB, orgID, func(tx *gorm.DB) error {
		var loadErr error
		claim, loadErr = a.rootApp().scopedApp(tx, orgID).
			loadMetaMessengerReconciliationClaim(orgID, accountID)
		return loadErr
	})
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusConflict, "Messenger subscription reconciliation is not available", nil, "")
	}
	ctx, cancel := context.WithTimeout(requestContext(r), metaMessengerProviderOperationLimit)
	defer cancel()
	switch claim.Operation.DesiredState {
	case metaMessengerSubscriptionDesiredSubscribed:
		err = a.subscribeMetaMessengerPage(ctx, claim.Account.ExternalAccountID, claim.PageToken)
	case metaMessengerSubscriptionDesiredUnsubscribed:
		err = a.unsubscribeMetaMessengerPage(ctx, claim.Account.ExternalAccountID, claim.PageToken)
	default:
		err = errMetaMessengerSubscriptionFence
	}
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusBadGateway, "Meta subscription state is still ambiguous", nil, "")
	}
	confirmedAt := time.Now().UTC()
	err = a.WithCommittedTenantApp(orgID, func(scoped *App) error {
		return scoped.acknowledgeMetaMessengerSubscriptionReconciliation(
			orgID, userID, accountID, claim.Operation, confirmedAt,
		)
	})
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusConflict, "Messenger subscription reconciliation was superseded", nil, "")
	}
	operation := claim.Operation
	if operation.DesiredState == metaMessengerSubscriptionDesiredSubscribed {
		err = a.WithCommittedTenantApp(orgID, func(scoped *App) error {
			_, finalizeErr := scoped.finalizeMetaMessengerSubscribeOperation(
				orgID, userID, accountID, operation, confirmedAt,
			)
			return finalizeErr
		})
	} else {
		err = a.WithCommittedTenantApp(orgID, func(scoped *App) error {
			return scoped.finalizeMetaMessengerDisconnect(orgID, operation, accountID, confirmedAt)
		})
	}
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusConflict, "Messenger subscription reconciliation needs another retry", nil, "")
	}
	return r.SendEnvelope(map[string]any{
		"reconciled":         true,
		"account_id":         accountID,
		"subscription_state": operation.DesiredState,
	})
}

type disconnectMetaMessengerRequest struct {
	ConfirmPageID string `json:"confirm_page_id"`
}

type metaMessengerDisconnectClaim struct {
	Account             models.ChannelAccount
	Operation           metaMessengerSubscriptionOperation
	PageToken           string
	AlreadyDisconnected bool
}

func (a *App) claimMetaMessengerDisconnect(
	organizationID uuid.UUID,
	userID uuid.UUID,
	accountID uuid.UUID,
	confirmPageID string,
	operationID uuid.UUID,
	expiresAt time.Time,
) (metaMessengerDisconnectClaim, error) {
	if a == nil || a.DB == nil || a.Config == nil || a.tenantOrgID == uuid.Nil ||
		organizationID != a.tenantOrgID || userID == uuid.Nil || accountID == uuid.Nil || operationID == uuid.Nil {
		return metaMessengerDisconnectClaim{}, metaregistry.ErrInvalidRequest
	}
	var account models.ChannelAccount
	if err := a.DB.Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("id = ? AND organization_id = ? AND channel = ? AND provider = ?",
			accountID, organizationID, models.ChannelMessenger, channelapi.RelayProvider).
		First(&account).Error; err != nil {
		return metaMessengerDisconnectClaim{}, err
	}
	if !metaRegistryControlPlaneConfig(account.Config) ||
		strings.TrimSpace(confirmPageID) != account.ExternalAccountID ||
		stringConfigValue(account.Metadata, "meta_platform_app_id") != strings.TrimSpace(a.Config.MetaMessenger.AppID) {
		return metaMessengerDisconnectClaim{}, metaregistry.ErrNotFound
	}
	if account.Status == models.ChannelAccountStatusDisconnected {
		return metaMessengerDisconnectClaim{Account: account, AlreadyDisconnected: true}, nil
	}
	now := time.Now().UTC()
	if !expiresAt.After(now) || !metaMessengerSubscriptionOperationAvailable(account.Metadata, now) {
		return metaMessengerDisconnectClaim{}, errMetaMessengerSubscriptionFence
	}
	var credentials []models.ChannelCredential
	if err := a.DB.Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("organization_id = ? AND channel_account_id = ?", organizationID, account.ID).
		Order("version DESC").Find(&credentials).Error; err != nil {
		return metaMessengerDisconnectClaim{}, err
	}
	oauth := currentMetaRegistryCredential(credentials, models.ChannelCredentialKindOAuth, now)
	webhook := currentMetaRegistryCredential(credentials, models.ChannelCredentialKindWebhook, now)
	if oauth == nil || webhook == nil {
		return metaMessengerDisconnectClaim{}, metaregistry.ErrNotFound
	}
	token, err := decryptRequiredMetaRegistrySecret(
		oauth.CredentialBlob,
		"access_token",
		a.Config.App.EncryptionKey,
	)
	if err != nil {
		return metaMessengerDisconnectClaim{}, err
	}
	operation := metaMessengerSubscriptionOperation{
		ID:                  operationID,
		OAuthCredentialID:   oauth.ID,
		OAuthVersion:        oauth.Version,
		WebhookCredentialID: webhook.ID,
		WebhookVersion:      webhook.Version,
		DesiredState:        metaMessengerSubscriptionDesiredUnsubscribed,
		State:               metaMessengerSubscriptionUnsubscribePending,
		ExpiresAt:           expiresAt.UTC(),
	}
	metadata := metadataWithMetaMessengerSubscriptionOperation(
		account.Metadata,
		operation,
		metaMessengerSubscriptionRemoteUnknown,
	)
	metadata["meta_subscription_state"] = metaMessengerSubscriptionUnsubscribePending
	metadata["meta_activation_state"] = "disconnecting"
	config := cloneJSONB(account.Config)
	config["outbound_enabled"] = false
	config["ai_reply_enabled"] = false
	result := a.DB.Model(&models.ChannelAccount{}).
		Where("id = ? AND organization_id = ?", account.ID, organizationID).
		Updates(map[string]any{
			"status":        models.ChannelAccountStatusPending,
			"config":        config,
			"metadata":      metadata,
			"last_error":    "Messenger disconnect is awaiting provider confirmation",
			"last_error_at": now,
			"updated_by_id": userID,
			"updated_at":    now,
		})
	if result.Error != nil {
		return metaMessengerDisconnectClaim{}, result.Error
	}
	if result.RowsAffected != 1 {
		return metaMessengerDisconnectClaim{}, errMetaMessengerSubscriptionFence
	}
	account.Metadata = metadata
	account.Status = models.ChannelAccountStatusPending
	account.Config = config
	account.LastError = "Messenger disconnect is awaiting provider confirmation"
	account.LastErrorAt = &now
	account.UpdatedByID = &userID
	return metaMessengerDisconnectClaim{
		Account:   account,
		Operation: operation,
		PageToken: token,
	}, nil
}

func (a *App) finalizeMetaMessengerDisconnect(
	organizationID uuid.UUID,
	operation metaMessengerSubscriptionOperation,
	accountID uuid.UUID,
	confirmedAt time.Time,
) error {
	if err := lockChannelAIOrganizationScopeTx(a.DB, organizationID); err != nil {
		return err
	}
	account, _, err := a.lockMetaMessengerSubscriptionOperation(
		organizationID,
		accountID,
		operation,
		metaMessengerSubscriptionUnsubscribePending,
	)
	if err != nil {
		return err
	}
	confirmedAt = confirmedAt.UTC()
	if previous, parseErr := time.Parse(
		time.RFC3339Nano,
		stringConfigValue(account.Metadata, "meta_ownership_checked_at"),
	); parseErr == nil && !confirmedAt.After(previous) {
		confirmedAt = previous.Add(time.Nanosecond)
	}
	mutation := metaregistry.MutationRequest{
		ChannelAccountID:         account.ID,
		CredentialID:             operation.OAuthCredentialID,
		CredentialVersion:        operation.OAuthVersion,
		WebhookCredentialID:      operation.WebhookCredentialID,
		WebhookCredentialVersion: operation.WebhookVersion,
		Outcome:                  metaregistry.OwnershipRevoked,
		Reason:                   "admin_disconnect",
		CheckedAt:                confirmedAt,
	}
	applied, err := a.applyMetaRegistryMutation(mutation, metaregistry.OwnershipRevoked)
	if err != nil {
		return err
	}
	if !applied {
		return errMetaMessengerSubscriptionFence
	}
	if err := a.DB.Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("id = ? AND organization_id = ?", account.ID, organizationID).
		First(&account).Error; err != nil {
		return err
	}
	metadata := cloneJSONB(account.Metadata)
	metadata[metaMessengerSubscriptionOperationStateKey] = metaMessengerSubscriptionUnsubscribeConfirmed
	metadata[metaMessengerSubscriptionRemoteStateKey] = metaMessengerSubscriptionRemoteUnsubscribed
	metadata[metaMessengerSubscriptionRemoteConfirmedAtKey] = confirmedAt.Format(time.RFC3339Nano)
	metadata["meta_subscription_state"] = metaMessengerSubscriptionRemoteUnsubscribed
	clearMetaMessengerSubscriptionFence(metadata)
	result := a.DB.Model(&models.ChannelAccount{}).
		Where("id = ? AND organization_id = ?", account.ID, organizationID).
		Update("metadata", metadata)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return errMetaMessengerSubscriptionFence
	}
	return nil
}

func (a *App) DisconnectMetaMessenger(r *fastglue.Request) error {
	orgID, userID, _, err := a.requireMetaMessengerOnboardingAuth(r, models.ActionDelete, true)
	if err != nil {
		return nil
	}
	if allowed, err := a.requireMetaMessengerRateLimit(r, orgID, userID, "disconnect", 8, time.Minute); !allowed {
		return err
	}
	accountID, err := parsePathUUID(r, "id", "channel account")
	if err != nil {
		return nil
	}
	var request disconnectMetaMessengerRequest
	if err := decodeStrictMetaMessengerRequest(r, &request); err != nil {
		return nil
	}
	request.ConfirmPageID = strings.TrimSpace(request.ConfirmPageID)
	operationID := uuid.New()
	operationExpiresAt := time.Now().UTC().Add(metaMessengerSubscriptionOperationLease)
	var claim metaMessengerDisconnectClaim
	err = a.WithCommittedTenantApp(orgID, func(scoped *App) error {
		var claimErr error
		claim, claimErr = scoped.claimMetaMessengerDisconnect(
			orgID,
			userID,
			accountID,
			request.ConfirmPageID,
			operationID,
			operationExpiresAt,
		)
		return claimErr
	})
	if err != nil {
		if errors.Is(err, errMetaMessengerSubscriptionFence) {
			return r.SendErrorEnvelope(fasthttp.StatusConflict, "Another Messenger subscription operation is already in progress", nil, "")
		}
		return r.SendErrorEnvelope(fasthttp.StatusNotFound, "Managed Messenger account not found", nil, "")
	}
	if claim.AlreadyDisconnected {
		return r.SendEnvelope(map[string]any{"disconnected": true, "account_id": claim.Account.ID})
	}
	if err := a.preflightMetaMessengerSubscriptionOperation(
		orgID,
		claim.Account.ID,
		claim.Operation,
		metaMessengerSubscriptionUnsubscribePending,
		time.Now().UTC(),
	); err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusConflict, "The disconnect was superseded; reload the account before retrying", nil, "")
	}
	ctx, cancel := context.WithTimeout(requestContext(r), metaMessengerProviderOperationLimit)
	defer cancel()
	if err := a.unsubscribeMetaMessengerPage(ctx, claim.Account.ExternalAccountID, claim.PageToken); err != nil {
		_ = a.WithCommittedTenantApp(orgID, func(scoped *App) error {
			return scoped.recordMetaMessengerSubscriptionOperationFailure(
				orgID,
				claim.Account.ID,
				claim.Operation,
				metaMessengerSubscriptionUnsubscribePending,
				metaMessengerSubscriptionUnsubscribeFailed,
				time.Now().UTC(),
			)
		})
		return r.SendErrorEnvelope(fasthttp.StatusBadGateway, "Meta could not confirm Page unsubscription; the account is safely quarantined for reconciliation", nil, "")
	}
	err = a.WithCommittedTenantApp(orgID, func(scoped *App) error {
		return scoped.finalizeMetaMessengerDisconnect(
			orgID,
			claim.Operation,
			claim.Account.ID,
			time.Now().UTC(),
		)
	})
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusConflict, "The disconnect was superseded; reload the account before retrying", nil, "")
	}
	return r.SendEnvelope(map[string]any{"disconnected": true, "account_id": claim.Account.ID})
}

func (a *App) requireMetaMessengerRateLimit(
	r *fastglue.Request, orgID, userID uuid.UUID, action string, max int64, window time.Duration,
) (bool, error) {
	if a == nil || a.Redis == nil {
		return false, r.SendErrorEnvelope(fasthttp.StatusServiceUnavailable, "Messenger lifecycle protection is unavailable", nil, "")
	}
	key := "meta-messenger:rate:" + action + ":" + orgID.String() + ":" + userID.String()
	script := redis.NewScript(`
local n = redis.call("INCR", KEYS[1])
if n == 1 then redis.call("PEXPIRE", KEYS[1], ARGV[1]) end
return n
`)
	count, err := script.Run(requestContext(r), a.Redis, []string{key}, window.Milliseconds()).Int64()
	if err != nil {
		return false, r.SendErrorEnvelope(fasthttp.StatusServiceUnavailable, "Messenger lifecycle protection is unavailable", nil, "")
	}
	if count > max {
		return false, r.SendErrorEnvelope(fasthttp.StatusTooManyRequests, "Too many Messenger onboarding attempts; try again shortly", nil, "")
	}
	return true, nil
}

func metaMessengerAccountName(pageName, pageID string) string {
	pageName = strings.TrimSpace(pageName)
	if pageName == "" {
		pageName = "Facebook Page " + pageID
	}
	if len([]rune(pageName)) > 100 {
		pageName = string([]rune(pageName)[:100])
	}
	return pageName
}
