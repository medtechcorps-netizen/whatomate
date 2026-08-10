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
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/shridarpatil/whatomate/internal/audit"
	channelapi "github.com/shridarpatil/whatomate/internal/channel"
	configpkg "github.com/shridarpatil/whatomate/internal/config"
	appcrypto "github.com/shridarpatil/whatomate/internal/crypto"
	"github.com/shridarpatil/whatomate/internal/database"
	"github.com/shridarpatil/whatomate/internal/metareview"
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

	metaMessengerSubscriptionDesiredStateKey       = "meta_subscription_desired_state"
	metaMessengerSubscriptionOperationIDKey        = "meta_subscription_operation_id"
	metaMessengerSubscriptionOperationStateKey     = "meta_subscription_operation_state"
	metaMessengerSubscriptionOperationExpiresKey   = "meta_subscription_operation_expires_at"
	metaMessengerSubscriptionOAuthCredentialIDKey  = "meta_subscription_oauth_credential_id"
	metaMessengerSubscriptionOAuthVersionKey       = "meta_subscription_oauth_version"
	metaMessengerSubscriptionRemoteStateKey        = "meta_subscription_remote_state"
	metaMessengerSubscriptionRemoteConfirmedAtKey  = "meta_subscription_remote_confirmed_at"
	metaMessengerSubscriptionFencedOperationIDKey  = "meta_subscription_fenced_operation_id"
	metaMessengerSubscriptionFencedOperationEndKey = "meta_subscription_fenced_operation_expires_at"
	metaMessengerSubscriptionFencedAckKey          = "meta_subscription_fenced_operation_acknowledged"
	metaMessengerSubscriptionFencedAckAtKey        = "meta_subscription_fenced_operation_acknowledged_at"

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

	// This lease is deliberately longer than the maximum provider operation.
	// Deprovisioning will not erase the recovery token or delete the tombstone
	// while an older subscribe request could still be completing at Meta.
	metaMessengerSubscriptionOperationLease = metaMessengerProviderOperationLimit + 30*time.Second
)

var (
	errMetaMessengerOnboardingDisabled = errors.New("managed Messenger onboarding is disabled")
	errMetaMessengerSelectionInvalid   = errors.New("the selected Page is not an owned selectable Page")
	errMetaMessengerPageBound          = errors.New("the Meta Page is already bound to a workspace")
	errMetaMessengerBusinessBound      = errors.New("the workspace is already bound to another Meta Business Portfolio")
	errMetaMessengerLegacyBinding      = errors.New("an existing Meta relay account must be migrated to an explicit Business binding first")
	errMetaMessengerSubscriptionFence  = errors.New("the Messenger subscription operation is no longer current")
)

type metaMessengerSubscriptionOperation struct {
	ID                uuid.UUID
	OAuthCredentialID uuid.UUID
	OAuthVersion      int
	DesiredState      string
	State             string
	ExpiresAt         time.Time
}

type metaMessengerWorkspaceSummary struct {
	OrganizationID   string `json:"organization_id"`
	OrganizationName string `json:"organization_name"`
}

type metaMessengerStartState struct {
	OrganizationID    string    `json:"organization_id"`
	UserID            string    `json:"user_id"`
	Nonce             string    `json:"nonce"`
	ConfigFingerprint string    `json:"config_fingerprint"`
	ExpiresAt         time.Time `json:"expires_at"`
}

type metaMessengerSelectionSession struct {
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

// StartMetaMessengerOnboarding creates a tenant- and user-bound one-time
// nonce. The browser passes the returned public values to FB.login with no
// scope parameter; app secrets and redirect URLs are never part of this flow.
func (a *App) StartMetaMessengerOnboarding(r *fastglue.Request) error {
	orgID, userID, _, err := a.requireMetaMessengerOnboardingAuth(r)
	if err != nil {
		return nil
	}
	settings, err := a.metaMessengerOnboardingSettings()
	if err != nil || a.Redis == nil || !a.hasIntegrationEncryptionKey() {
		return r.SendErrorEnvelope(
			fasthttp.StatusServiceUnavailable,
			"Managed Messenger onboarding is unavailable",
			nil,
			"",
		)
	}
	nonce := generateRandomString(48)
	now := time.Now().UTC()
	state := metaMessengerStartState{
		OrganizationID:    orgID.String(),
		UserID:            userID.String(),
		Nonce:             nonce,
		ConfigFingerprint: a.metaMessengerOnboardingRuntimeFingerprint(settings),
		ExpiresAt:         now.Add(metaMessengerOnboardingStateTTL),
	}
	payload, err := json.Marshal(state)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to start Messenger onboarding", nil, "")
	}
	if err := a.Redis.Set(
		requestContext(r),
		metaMessengerStartStateKey(orgID, userID, nonce),
		payload,
		metaMessengerOnboardingStateTTL,
	).Err(); err != nil {
		a.Log.Error("Failed to store Messenger onboarding nonce", "error", err, "organization_id", orgID)
		return r.SendErrorEnvelope(fasthttp.StatusServiceUnavailable, "Managed Messenger onboarding is unavailable", nil, "")
	}
	response := startMetaMessengerOnboardingResponse{
		Provider:  "meta",
		Channel:   string(models.ChannelMessenger),
		Mode:      metaMessengerOnboardingMode,
		Nonce:     nonce,
		ExpiresAt: state.ExpiresAt,
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
	orgID, userID, workspaceName, err := a.requireMetaMessengerOnboardingAuth(r)
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
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, err.Error(), nil, "")
	}
	platform, businesses, pages, err := a.discoverMetaMessengerInventory(
		ctx,
		token.AccessToken,
		inspection,
	)
	if err != nil {
		a.Log.Warn("Messenger Page ownership discovery failed", "organization_id", orgID)
		return r.SendErrorEnvelope(fasthttp.StatusBadGateway, "Meta Page ownership could not be verified", nil, "")
	}
	if businesses, pages, err = a.filterMetaMessengerReviewInventory(
		orgID,
		businesses,
		pages,
	); err != nil {
		return r.SendErrorEnvelope(
			fasthttp.StatusBadRequest,
			"The configured review Business and Page were not authorized",
			nil,
			"",
		)
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
	orgID, userID, _, err := a.requireMetaMessengerOnboardingAuth(r)
	if err != nil {
		return nil
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
	if _, reviewTuple, reviewErr := a.metaMessengerReviewSettings(time.Now().UTC()); reviewErr == nil &&
		(orgID.String() != reviewTuple.OrganizationID ||
			request.BusinessID != reviewTuple.MetaBusinessID ||
			request.PageID != reviewTuple.PageID) {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "The selected Page is not available for this review deployment", nil, "")
	}
	sessionJSON, err := a.Redis.GetDel(
		requestContext(r),
		metaMessengerSelectionStateKey(orgID, userID, request.SessionID),
	).Bytes()
	if err != nil {
		if !errors.Is(err, redis.Nil) {
			a.Log.Warn("Messenger Page selection lookup failed", "organization_id", orgID)
		}
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Messenger Page selection is invalid or expired", nil, "")
	}
	var session metaMessengerSelectionSession
	if json.Unmarshal(sessionJSON, &session) != nil ||
		!metaMessengerOpaqueValuesEqual(session.SessionID, request.SessionID) ||
		session.OrganizationID != orgID.String() || session.UserID != userID.String() ||
		!session.ExpiresAt.After(time.Now().UTC()) {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Messenger Page selection is invalid or expired", nil, "")
	}
	if !metaMessengerOpaqueValuesEqual(
		session.ConfigFingerprint,
		a.metaMessengerOnboardingRuntimeFingerprint(settings),
	) {
		return r.SendErrorEnvelope(fasthttp.StatusConflict, "Messenger onboarding settings changed; start again", nil, "")
	}
	selected, err := selectMetaMessengerOwnedPage(session.Pages, request.BusinessID, request.PageID)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "The selected Page is not owned by the selected Business Portfolio", nil, "")
	}
	if !appcrypto.IsEncrypted(strings.TrimSpace(session.EncryptedUserToken)) {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Messenger authorization is invalid; start again", nil, "")
	}
	userToken, err := appcrypto.Decrypt(session.EncryptedUserToken, a.integrationEncryptionKey())
	if err != nil || strings.TrimSpace(userToken) == "" {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Messenger authorization is invalid; start again", nil, "")
	}
	ctx, cancel := context.WithTimeout(requestContext(r), metaMessengerProviderOperationLimit)
	defer cancel()
	freshUserInspection, err := a.inspectMetaMessengerToken(ctx, strings.TrimSpace(userToken), true)
	if err != nil {
		a.Log.Warn("Messenger authorization revalidation failed", "organization_id", orgID)
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Meta authorization changed; start again", nil, "")
	}
	freshPlatform, err := a.fetchMetaMessengerPlatformIdentity(
		ctx,
		strings.TrimSpace(userToken),
		freshUserInspection,
	)
	if err != nil || freshPlatform.UserID != session.Platform.UserID ||
		freshPlatform.TokenKind != session.Platform.TokenKind ||
		freshPlatform.ClientBusinessID != session.Platform.ClientBusinessID {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Meta authorization identity changed; start again", nil, "")
	}
	if freshPlatform.TokenKind == metaMessengerTokenKindSystemUser &&
		request.BusinessID != freshPlatform.ClientBusinessID {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "The selected Business Portfolio no longer matches the authorized integration", nil, "")
	}
	selected, err = a.revalidateMetaMessengerOwnedPage(
		ctx,
		strings.TrimSpace(userToken),
		freshUserInspection,
		selected,
	)
	if err != nil {
		a.Log.Warn("Messenger Page ownership revalidation failed", "organization_id", orgID, "page_id", request.PageID)
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "The selected Page is no longer owned or accessible", nil, "")
	}
	pageToken, err := appcrypto.Decrypt(selected.EncryptedPageToken, a.integrationEncryptionKey())
	if err != nil || strings.TrimSpace(pageToken) == "" {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Messenger Page authorization is invalid; start again", nil, "")
	}
	pageToken = strings.TrimSpace(pageToken)

	pageInspection, pageName, err := a.bindMetaMessengerPageToken(ctx, request.PageID, pageToken)
	if err != nil {
		a.Log.Warn("Messenger Page token binding failed", "organization_id", orgID, "page_id", request.PageID)
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Meta could not verify the selected Page token", nil, "")
	}
	if pageName != "" {
		selected.PageName = pageName
	}
	if _, _, _, err := a.requireMetaMessengerOnboardingAuth(r); err != nil {
		return nil
	}
	account, err := a.persistMetaMessengerPendingAccount(
		r,
		orgID,
		userID,
		selected,
		pageInspection,
		freshUserInspection,
		freshPlatform.ClientBusinessID,
	)
	if err != nil {
		switch {
		case errors.Is(err, errMetaMessengerPageBound),
			errors.Is(err, errMetaMessengerBusinessBound),
			errors.Is(err, errMetaMessengerLegacyBinding):
			return r.SendErrorEnvelope(fasthttp.StatusConflict, err.Error(), nil, "")
		default:
			a.Log.Error("Failed to stage Messenger Page account", "error", err, "organization_id", orgID)
			return r.SendErrorEnvelope(fasthttp.StatusConflict, "The Messenger Page could not be staged for this workspace", nil, "")
		}
	}
	operation, err := metaMessengerSubscriptionOperationFromAccount(account)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusConflict, "The Messenger subscription operation could not be fenced", nil, "")
	}
	stagedAccountID := account.ID

	if err := a.subscribeMetaMessengerPage(ctx, request.PageID, pageToken); err != nil {
		a.Log.Warn("Messenger Page subscription verification failed", "organization_id", orgID, "page_id", request.PageID)
		cleanupErr := a.compensateMetaMessengerSubscribe(
			requestContext(r),
			orgID,
			stagedAccountID,
			operation,
			request.PageID,
			pageToken,
		)
		remoteState := metaMessengerSubscriptionRemoteUnknown
		if cleanupErr == nil {
			remoteState = metaMessengerSubscriptionRemoteUnsubscribed
		}
		_, _ = a.finalizeMetaMessengerPendingAccountOperation(
			orgID,
			userID,
			account.ID,
			operation,
			metaMessengerSubscriptionFailed,
			false,
			"Meta messages webhook subscription could not be verified",
			remoteState,
		)
		return r.SendErrorEnvelope(
			fasthttp.StatusBadGateway,
			"The Page was staged safely, but Meta did not verify its messages webhook subscription",
			nil,
			"",
		)
	}
	account, err = a.finalizeMetaMessengerPendingAccountOperation(
		orgID,
		userID,
		stagedAccountID,
		operation,
		metaMessengerAwaitingRegistryState,
		true,
		"",
		metaMessengerSubscriptionRemoteSubscribed,
	)
	if err != nil {
		compensationErr := a.compensateMetaMessengerSubscribe(
			requestContext(r),
			orgID,
			stagedAccountID,
			operation,
			request.PageID,
			pageToken,
		)
		if compensationErr != nil {
			a.Log.Error("Failed to compensate stale Messenger subscription", "error", compensationErr, "organization_id", orgID, "page_id", request.PageID)
		}
		a.Log.Error("Failed to finalize Messenger Page staging", "error", err, "organization_id", orgID)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "The Page subscription was verified but local staging could not be finalized", nil, "")
	}
	onboardingState := metaMessengerAwaitingRegistryState
	if metaMessengerReviewAccountMarker(account) {
		onboardingState = metaMessengerReviewReadyState
	}
	setMetaMessengerNoStoreHeaders(r)
	return r.SendEnvelope(selectMetaMessengerOnboardingResponse{
		Account:              channelAccountToResponse(account),
		OnboardingState:      onboardingState,
		SubscriptionVerified: true,
		RegistryRecognized:   false,
	})
}

func (a *App) metaMessengerOnboardingSettings() (configpkg.MetaMessengerOnboardingConfig, error) {
	if a == nil || a.Config == nil {
		return configpkg.MetaMessengerOnboardingConfig{}, errMetaMessengerOnboardingDisabled
	}
	settings := a.Config.MetaMessengerOnboarding
	if strings.EqualFold(strings.TrimSpace(a.Config.App.Environment), "production") {
		return configpkg.MetaMessengerOnboardingConfig{}, errMetaMessengerOnboardingDisabled
	}
	settings.AppID = strings.TrimSpace(settings.AppID)
	settings.ConfigID = strings.TrimSpace(settings.ConfigID)
	settings.OwnerBusinessID = strings.TrimSpace(settings.OwnerBusinessID)
	settings.GraphAPIVersion = strings.Trim(strings.TrimSpace(settings.GraphAPIVersion), "/")
	settings.TrustedRelayBaseURL = strings.TrimSpace(settings.TrustedRelayBaseURL)
	if !settings.Enabled || !validCanonicalMetaID(settings.AppID) ||
		!validCanonicalMetaID(settings.ConfigID) || !validCanonicalMetaID(settings.OwnerBusinessID) ||
		settings.AppSecret == "" || strings.TrimSpace(settings.AppSecret) != settings.AppSecret ||
		settings.GraphAPIVersion == "" || strings.TrimSpace(settings.GraphBaseURL) == "" || settings.TrustedRelayBaseURL == "" {
		return configpkg.MetaMessengerOnboardingConfig{}, errMetaMessengerOnboardingDisabled
	}
	canonicalOnboardingRelay, err := configpkg.CanonicalMetaRelayBaseURL(
		settings.TrustedRelayBaseURL,
		a.Config.App.Environment,
	)
	if err != nil {
		return configpkg.MetaMessengerOnboardingConfig{}, errMetaMessengerOnboardingDisabled
	}
	if a.Config.MetaMessengerReviewRelay.Enabled {
		if a.Config.App.Environment != "staging" ||
			a.Config.MetaMessengerReviewRelay.Mode != metareview.Mode {
			return configpkg.MetaMessengerOnboardingConfig{}, errMetaMessengerOnboardingDisabled
		}
		canonicalReviewRelay, err := configpkg.CanonicalMetaRelayBaseURL(
			a.Config.MetaMessengerReviewRelay.RelayBaseURL,
			a.Config.App.Environment,
		)
		if err != nil || canonicalOnboardingRelay != canonicalReviewRelay {
			return configpkg.MetaMessengerOnboardingConfig{}, errMetaMessengerOnboardingDisabled
		}
	} else {
		canonicalProtectedRelay, err := configpkg.CanonicalMetaRelayBaseURL(
			a.Config.MetaRelay.BaseURL,
			a.Config.App.Environment,
		)
		if err != nil || canonicalOnboardingRelay != canonicalProtectedRelay {
			return configpkg.MetaMessengerOnboardingConfig{}, errMetaMessengerOnboardingDisabled
		}
	}
	settings.TrustedRelayBaseURL = canonicalOnboardingRelay
	return settings, nil
}

func metaMessengerOnboardingFingerprint(settings configpkg.MetaMessengerOnboardingConfig) string {
	digest := sha256.Sum256([]byte(strings.Join([]string{
		settings.AppID,
		settings.ConfigID,
		settings.OwnerBusinessID,
		settings.AppSecret,
		settings.GraphAPIVersion,
		settings.GraphBaseURL,
		settings.TrustedRelayBaseURL,
	}, "\x00")))
	return hex.EncodeToString(digest[:])
}

func (a *App) metaMessengerOnboardingRuntimeFingerprint(
	settings configpkg.MetaMessengerOnboardingConfig,
) string {
	base := metaMessengerOnboardingFingerprint(settings)
	if a == nil || a.Config == nil || !a.Config.MetaMessengerReviewRelay.Enabled {
		return base
	}
	review := a.Config.MetaMessengerReviewRelay
	digest := sha256.Sum256([]byte(strings.Join([]string{
		base,
		review.Mode,
		review.OrganizationID,
		review.MetaBusinessID,
		review.PageID,
		review.ChannelAccountID,
		review.Generation,
		review.ExpiresAt,
		review.RelayBaseURL,
		review.ReReplyBaseURL,
		review.BrokerAuthSecret,
		review.BrokerWrapSecret,
		review.ProviderProofSecret,
	}, "\x00")))
	return hex.EncodeToString(digest[:])
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
) (uuid.UUID, uuid.UUID, string, error) {
	orgID, userID, err := a.getOrgAndUserID(r)
	if err != nil || orgID == uuid.Nil || userID == uuid.Nil {
		_ = r.SendErrorEnvelope(fasthttp.StatusUnauthorized, "Unauthorized", nil, "")
		return uuid.Nil, uuid.Nil, "", errEnvelopeSent
	}
	if a != nil && a.Config != nil && a.Config.MetaMessengerReviewRelay.Enabled {
		if a.Config.App.Environment != "staging" ||
			a.Config.MetaMessengerReviewRelay.Mode != metareview.Mode ||
			orgID.String() != a.Config.MetaMessengerReviewRelay.OrganizationID {
			_ = r.SendErrorEnvelope(fasthttp.StatusForbidden, "Messenger review onboarding is restricted to its configured workspace", nil, "")
			return uuid.Nil, uuid.Nil, "", errEnvelopeSent
		}
		if _, _, reviewErr := a.metaMessengerReviewSettings(time.Now().UTC()); reviewErr != nil {
			_ = r.SendErrorEnvelope(fasthttp.StatusServiceUnavailable, "Messenger review onboarding authority is unavailable or expired", nil, "")
			return uuid.Nil, uuid.Nil, "", errEnvelopeSent
		}
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
		_, _, authErr = scoped.requireAuth(r, models.ResourceChannelAccounts, models.ActionWrite)
		if authErr != nil {
			return nil
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
	workspaceName = strings.TrimSpace(workspaceName)
	if workspaceName == "" {
		workspaceName = "Workspace"
	}
	return orgID, userID, workspaceName, nil
}

func (a *App) persistMetaMessengerPendingAccount(
	r *fastglue.Request,
	orgID, userID uuid.UUID,
	page metaMessengerStoredPage,
	pageInspection metaMessengerTokenInspection,
	authorizationInspection metaMessengerTokenInspection,
	clientBusinessID string,
) (*models.ChannelAccount, error) {
	settings, err := a.metaMessengerOnboardingSettings()
	if err != nil {
		return nil, err
	}
	reviewAccountID := uuid.Nil
	if a.Config != nil && a.Config.MetaMessengerReviewRelay.Enabled {
		_, reviewTuple, reviewErr := a.metaMessengerReviewSettings(time.Now().UTC())
		if reviewErr != nil {
			return nil, errMetaMessengerOnboardingDisabled
		}
		if orgID.String() != reviewTuple.OrganizationID ||
			page.BusinessID != reviewTuple.MetaBusinessID ||
			page.PageID != reviewTuple.PageID {
			return nil, errMetaMessengerSelectionInvalid
		}
		reviewAccountID, err = uuid.Parse(reviewTuple.ChannelAccountID)
		if err != nil || reviewAccountID == uuid.Nil {
			return nil, errMetaMessengerSelectionInvalid
		}
	}
	authorizationInspection.Type = strings.ToUpper(strings.TrimSpace(authorizationInspection.Type))
	clientBusinessID = strings.TrimSpace(clientBusinessID)
	if authorizationInspection.Type != metaMessengerTokenKindUser &&
		authorizationInspection.Type != metaMessengerTokenKindSystemUser {
		return nil, errMetaMessengerSelectionInvalid
	}
	if authorizationInspection.Type == metaMessengerTokenKindSystemUser {
		if !validCanonicalMetaID(clientBusinessID) || clientBusinessID != page.BusinessID {
			return nil, errMetaMessengerSelectionInvalid
		}
	} else {
		clientBusinessID = ""
	}
	if page.Ownership != metaMessengerOwnershipOwned || !page.Selectable ||
		!validCanonicalMetaID(page.BusinessID) || !validCanonicalMetaID(page.PageID) ||
		page.OwnershipVerifiedAt.IsZero() ||
		!metaMessengerHasMessagingTask(page.Tasks) ||
		!appcrypto.IsEncrypted(strings.TrimSpace(page.EncryptedPageToken)) {
		return nil, errMetaMessengerSelectionInvalid
	}
	tokenExpiry := earliestMetaMessengerExpiry(
		authorizationInspection.ExpiresAt,
		authorizationInspection.DataAccessExpiresAt,
		pageInspection.ExpiresAt,
		pageInspection.DataAccessExpiresAt,
	)

	root := a.rootApp()
	if root == nil || root.DB == nil {
		return nil, errors.New("messenger onboarding database is unavailable")
	}
	var account *models.ChannelAccount
	var webhookCredential models.ChannelCredential
	var oauthCredential models.ChannelCredential
	err = database.WithTenantReadCommitted(root.DB, orgID, func(tx *gorm.DB) error {
		scoped := root.scopedApp(tx, orgID)
		_, _, authErr := scoped.requireAuth(r, models.ResourceChannelAccounts, models.ActionWrite)
		if authErr != nil {
			return authErr
		}
		if err := lockChannelAIOrganizationScopeTx(tx, orgID); err != nil {
			return err
		}
		var existing []models.ChannelAccount
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("organization_id = ? AND channel IN ? AND provider = ?", orgID, []models.Channel{models.ChannelMessenger, models.ChannelInstagram}, channelapi.RelayProvider).
			Find(&existing).Error; err != nil {
			return err
		}
		exactIndex := -1
		for index := range existing {
			businessID, businessErr := scoped.metaMessengerExistingBusinessID(&existing[index])
			if businessErr != nil {
				return businessErr
			}
			if businessID != page.BusinessID {
				return errMetaMessengerBusinessBound
			}
			if existing[index].Channel == models.ChannelMessenger &&
				existing[index].ExternalAccountID == page.PageID {
				exactIndex = index
			}
		}

		isNew := exactIndex < 0
		var oldAccount *models.ChannelAccount
		if isNew {
			accountID := uuid.New()
			if reviewAccountID != uuid.Nil {
				accountID = reviewAccountID
			}
			account = &models.ChannelAccount{
				BaseModel:      models.BaseModel{ID: accountID},
				OrganizationID: orgID,
				CreatedByID:    &userID,
			}
		} else {
			if reviewAccountID != uuid.Nil && existing[exactIndex].ID != reviewAccountID {
				return errMetaMessengerPageBound
			}
			if !metaMessengerPendingResumeAllowed(&existing[exactIndex]) {
				return errMetaMessengerPageBound
			}
			account = &existing[exactIndex]
			// Once protected inventory recognizes this exact managed binding,
			// provisioning owns the credential handoff. A browser retry must not
			// rotate the Page token and desynchronize already-provisioned relay
			// secrets, even if the persisted staging flags still say false.
			if _, bindingErr := scoped.trustedMetaRelayBinding(account); bindingErr == nil {
				return errMetaMessengerPageBound
			}
			oldCopy := *account
			oldCopy.Config = cloneJSONB(account.Config)
			oldCopy.Metadata = cloneJSONB(account.Metadata)
			oldCopy.Capabilities = cloneJSONB(account.Capabilities)
			oldAccount = &oldCopy
		}
		if err := applyMetaMessengerPendingAccount(
			account,
			orgID,
			userID,
			page,
			pageInspection,
			authorizationInspection,
			clientBusinessID,
			settings,
		); err != nil {
			return err
		}
		if reviewAccountID != uuid.Nil {
			account.Config["review_only"] = true
		}
		// This insert is the cross-tenant staging gate. PostgreSQL enforces
		// uq_channel_accounts_global_routable_identity globally even when RLS
		// intentionally hides another tenant's row from the early count above.
		// The provider subscription happens only after this transaction commits,
		// so a losing workspace cannot create a remote side effect.
		if isNew {
			if err := tx.Create(account).Error; err != nil {
				return err
			}
		} else if err := tx.Save(account).Error; err != nil {
			return err
		}
		var webhookCreated bool
		webhookCredential, webhookCreated, err = scoped.ensureMetaMessengerWebhookCredentialTx(
			tx,
			orgID,
			account.ID,
			reviewAccountID != uuid.Nil,
		)
		if err != nil {
			return err
		}
		oauthCredential, err = rotateMetaMessengerOAuthCredentialTx(
			tx,
			orgID,
			account.ID,
			page,
			pageInspection,
			settings.AppID,
			tokenExpiry,
		)
		if err != nil {
			return err
		}
		if reviewAccountID != uuid.Nil {
			if err := scrubSupersededMetaMessengerReviewCredentialsTx(
				tx,
				orgID,
				account.ID,
				webhookCredential.ID,
				oauthCredential.ID,
				time.Now().UTC(),
			); err != nil {
				return err
			}
		}
		operation := newMetaMessengerSubscriptionOperation(oauthCredential, time.Now().UTC())
		account.Metadata = cloneJSONB(account.Metadata)
		writeMetaMessengerSubscriptionOperation(account.Metadata, operation)
		account.Metadata[metaMessengerSubscriptionRemoteStateKey] = metaMessengerSubscriptionRemoteUnknown
		delete(account.Metadata, metaMessengerSubscriptionRemoteConfirmedAtKey)
		delete(account.Metadata, metaMessengerSubscriptionFencedOperationIDKey)
		delete(account.Metadata, metaMessengerSubscriptionFencedOperationEndKey)
		delete(account.Metadata, metaMessengerSubscriptionFencedAckKey)
		delete(account.Metadata, metaMessengerSubscriptionFencedAckAtKey)
		if err := tx.Save(account).Error; err != nil {
			return err
		}
		var priorSecret any
		if !isNew {
			priorSecret = "********"
		}
		credentialChanges := []map[string]any{
			{"field": "page_access_token", "old_value": priorSecret, "new_value": "********"},
		}
		if webhookCreated {
			credentialChanges = append(credentialChanges, map[string]any{
				"field": "relay_hmac_credentials", "old_value": priorSecret, "new_value": "********",
			})
		}
		action := models.AuditActionUpdated
		if isNew {
			action = models.AuditActionCreated
		}
		if err := audit.LogAudit(
			tx,
			orgID,
			userID,
			audit.GetUserName(tx, userID),
			"channel_account",
			account.ID,
			action,
			oldAccount,
			account,
			credentialChanges...,
		); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	account.Credentials = []models.ChannelCredential{webhookCredential, oauthCredential}
	return account, nil
}

func applyMetaMessengerPendingAccount(
	account *models.ChannelAccount,
	orgID, userID uuid.UUID,
	page metaMessengerStoredPage,
	pageInspection, authorizationInspection metaMessengerTokenInspection,
	clientBusinessID string,
	settings configpkg.MetaMessengerOnboardingConfig,
) error {
	if account == nil || account.ID == uuid.Nil {
		return errors.New("messenger account identity is invalid")
	}
	relayURL, err := metaMessengerRelayURL(settings.TrustedRelayBaseURL, page.PageID)
	if err != nil {
		return err
	}
	account.OrganizationID = orgID
	account.Channel = models.ChannelMessenger
	account.Provider = channelapi.RelayProvider
	account.Name = metaMessengerAccountName(page.PageName, page.PageID)
	account.ExternalAccountID = page.PageID
	account.Status = models.ChannelAccountStatusPending
	account.Capabilities = models.JSONB{"text": true, "service_window": true}
	account.Config = models.JSONB{
		"relay_url":             relayURL,
		"identity_confirmed_id": page.PageID,
		// Informational compatibility only. Business trust is always derived
		// from server-owned Metadata or protected legacy inventory.
		"meta_business_id":      page.BusinessID,
		"outbound_enabled":      false,
		"ai_reply_enabled":      false,
		"onboarding_state":      metaMessengerVerifyingSubscription,
		"registry_recognized":   false,
		"management_mode":       metaMessengerManagementMode,
		"webhook_callback_path": "/api/webhooks/channels/" + account.ID.String(),
	}
	account.Metadata = models.JSONB{
		"meta_app_id":                settings.AppID,
		"meta_app_owner_business_id": settings.OwnerBusinessID,
		// Config ID is an onboarding-session fingerprint/audit fact, not a
		// protected runtime trust anchor.
		"meta_login_configuration_id": settings.ConfigID,
		"meta_business_id":            page.BusinessID,
		"meta_business_name":          page.BusinessName,
		"meta_page_name":              page.PageName,
		"meta_token_kind":             authorizationInspection.Type,
		"management_mode":             metaMessengerManagementMode,
		"onboarding_state":            metaMessengerVerifyingSubscription,
		"ownership_evidence_version":  "owned_pages_v1",
		"ownership_verified_at":       page.OwnershipVerifiedAt.UTC().Format(time.RFC3339Nano),
		"subscription_verified":       false,
		"registry_recognized":         false,
		"granted_scopes":              normalizedMetaMessengerValues(authorizationInspection.Scopes),
		"permissions_verified_at":     authorizationInspection.CheckedAt.UTC().Format(time.RFC3339Nano),
		"page_token_verified_at":      pageInspection.CheckedAt.UTC().Format(time.RFC3339Nano),
	}
	if clientBusinessID != "" {
		account.Metadata["meta_client_business_id"] = clientBusinessID
	}
	account.IsDefaultIncoming = false
	account.IsDefaultOutgoing = false
	account.ConnectedAt = nil
	account.LastHealthCheckAt = nil
	account.LastInboundAt = nil
	account.LastOutboundAt = nil
	account.LastErrorAt = nil
	account.LastError = ""
	account.UpdatedByID = &userID
	return nil
}

func (a *App) metaMessengerExistingBusinessID(account *models.ChannelAccount) (string, error) {
	if account == nil {
		return "", errMetaMessengerLegacyBinding
	}
	if strings.TrimSpace(stringConfigValue(account.Metadata, "management_mode")) == metaMessengerManagementMode {
		businessID := strings.TrimSpace(stringConfigValue(account.Metadata, "meta_business_id"))
		verifiedAt := strings.TrimSpace(stringConfigValue(account.Metadata, "ownership_verified_at"))
		if account.Channel != models.ChannelMessenger ||
			!validCanonicalMetaID(businessID) ||
			stringConfigValue(account.Metadata, "ownership_evidence_version") != "owned_pages_v1" ||
			verifiedAt == "" {
			return "", errMetaMessengerLegacyBinding
		}
		if _, err := time.Parse(time.RFC3339Nano, verifiedAt); err != nil {
			return "", errMetaMessengerLegacyBinding
		}
		return businessID, nil
	}
	// Legacy Config is tenant-editable and is never ownership evidence.
	binding, err := a.trustedMetaRelayBinding(account)
	if err != nil || !validCanonicalMetaID(binding.MetaBusinessID) {
		return "", errMetaMessengerLegacyBinding
	}
	return binding.MetaBusinessID, nil
}

func metaMessengerPendingResumeAllowed(account *models.ChannelAccount) bool {
	if account == nil || account.Channel != models.ChannelMessenger ||
		!strings.EqualFold(strings.TrimSpace(account.Provider), channelapi.RelayProvider) ||
		account.Status != models.ChannelAccountStatusPending || account.ConnectedAt != nil ||
		stringConfigValue(account.Metadata, "management_mode") != metaMessengerManagementMode {
		return false
	}
	operation, operationErr := metaMessengerSubscriptionOperationFromAccount(account)
	if operationErr == nil &&
		operation.DesiredState == metaMessengerSubscriptionDesiredSubscribed &&
		operation.State == metaMessengerSubscriptionSubscribePending &&
		operation.ExpiresAt.After(time.Now().UTC()) {
		return false
	}
	// Review rows have no safe legacy operation state. Keep the existing
	// fail-closed behavior for malformed review metadata, while ordinary managed
	// rows without lifecycle metadata remain eligible for the legacy recovery
	// path once no valid active operation is present.
	if operationErr != nil && boolConfigValue(account.Config, "review_only") {
		return false
	}
	if metaMessengerReviewAccountMarker(account) || boolConfigValue(account.Metadata, "review_ready") {
		return false
	}
	registryRecognized, ok := account.Metadata["registry_recognized"].(bool)
	if !ok || registryRecognized {
		return false
	}
	switch stringConfigValue(account.Metadata, "onboarding_state") {
	case metaMessengerVerifyingSubscription,
		metaMessengerSubscriptionFailed,
		metaMessengerAwaitingRegistryState:
		return true
	default:
		return false
	}
}

func (a *App) ensureMetaMessengerWebhookCredentialTx(
	tx *gorm.DB,
	orgID, accountID uuid.UUID,
	inboundOnly bool,
) (models.ChannelCredential, bool, error) {
	var credentials []models.ChannelCredential
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
		Where(
			"organization_id = ? AND channel_account_id = ? AND kind = ?",
			orgID,
			accountID,
			models.ChannelCredentialKindWebhook,
		).
		Order("version DESC, id ASC").
		Find(&credentials).Error; err != nil {
		return models.ChannelCredential{}, false, err
	}
	now := time.Now().UTC()
	maximumVersion := 0
	keepIndex := -1
	for index := range credentials {
		if credentials[index].Version > maximumVersion {
			maximumVersion = credentials[index].Version
		}
		if keepIndex < 0 && a.metaMessengerWebhookCredentialValid(&credentials[index], now, inboundOnly) {
			keepIndex = index
		}
	}
	keepID := uuid.Nil
	if keepIndex >= 0 {
		keepID = credentials[keepIndex].ID
	}
	if err := revokeOtherCurrentMetaMessengerCredentialsTx(tx, credentials, keepID, now); err != nil {
		return models.ChannelCredential{}, false, err
	}
	if keepIndex >= 0 {
		return credentials[keepIndex], false, nil
	}
	inboundSecret, err := generateChannelSecret()
	if err != nil {
		return models.ChannelCredential{}, false, err
	}
	encryptedInbound, err := appcrypto.Encrypt(inboundSecret, a.integrationEncryptionKey())
	if err != nil || !appcrypto.IsEncrypted(encryptedInbound) {
		return models.ChannelCredential{}, false, errors.New("messenger inbound credential could not be protected")
	}
	credentialBlob := models.JSONB{"inbound_secret": encryptedInbound}
	if !inboundOnly {
		outboundSecret, err := generateChannelSecret()
		if err != nil {
			return models.ChannelCredential{}, false, err
		}
		if metaMessengerOpaqueValuesEqual(inboundSecret, outboundSecret) {
			return models.ChannelCredential{}, false, errors.New("generated Messenger relay credentials collided")
		}
		encryptedOutbound, err := appcrypto.Encrypt(outboundSecret, a.integrationEncryptionKey())
		if err != nil || !appcrypto.IsEncrypted(encryptedOutbound) {
			return models.ChannelCredential{}, false, errors.New("messenger outbound credential could not be protected")
		}
		credentialBlob["outbound_secret"] = encryptedOutbound
	}
	credential := models.ChannelCredential{
		BaseModel:        models.BaseModel{ID: uuid.New()},
		OrganizationID:   orgID,
		ChannelAccountID: accountID,
		Kind:             models.ChannelCredentialKindWebhook,
		Version:          maximumVersion + 1,
		CredentialBlob:   credentialBlob,
		Status:           models.ChannelCredentialStatusActive,
		KeyVersion:       "app:v1",
		Metadata:         models.JSONB{"management_mode": metaMessengerManagementMode},
	}
	if err := tx.Create(&credential).Error; err != nil {
		return models.ChannelCredential{}, false, err
	}
	return credential, true, nil
}

func (a *App) metaMessengerWebhookCredentialValid(
	credential *models.ChannelCredential,
	now time.Time,
	inboundOnly bool,
) bool {
	if credential == nil || credential.Kind != models.ChannelCredentialKindWebhook ||
		(credential.Status != models.ChannelCredentialStatusActive &&
			credential.Status != models.ChannelCredentialStatusExpiring) ||
		(credential.ExpiresAt != nil && !credential.ExpiresAt.After(now)) {
		return false
	}
	inbound, inboundOK := credential.CredentialBlob["inbound_secret"].(string)
	if !inboundOK || !appcrypto.IsEncrypted(inbound) {
		return false
	}
	inboundPlaintext, inboundErr := appcrypto.Decrypt(inbound, a.integrationEncryptionKey())
	if inboundErr != nil || inboundPlaintext == "" {
		return false
	}
	outbound, outboundOK := credential.CredentialBlob["outbound_secret"].(string)
	if inboundOnly {
		return !outboundOK || strings.TrimSpace(outbound) == ""
	}
	if !outboundOK || !appcrypto.IsEncrypted(outbound) {
		return false
	}
	outboundPlaintext, outboundErr := appcrypto.Decrypt(outbound, a.integrationEncryptionKey())
	return outboundErr == nil && outboundPlaintext != "" &&
		!metaMessengerOpaqueValuesEqual(inboundPlaintext, outboundPlaintext)
}

func revokeOtherCurrentMetaMessengerCredentialsTx(
	tx *gorm.DB,
	credentials []models.ChannelCredential,
	keepID uuid.UUID,
	now time.Time,
) error {
	ids := make([]uuid.UUID, 0, len(credentials))
	for index := range credentials {
		credential := &credentials[index]
		if credential.ID == keepID ||
			(credential.Status != models.ChannelCredentialStatusActive &&
				credential.Status != models.ChannelCredentialStatusExpiring) {
			continue
		}
		ids = append(ids, credential.ID)
	}
	if len(ids) == 0 {
		return nil
	}
	return tx.Model(&models.ChannelCredential{}).
		Where("id IN ?", ids).
		Updates(map[string]any{
			"status":     models.ChannelCredentialStatusRevoked,
			"revoked_at": now,
			"rotated_at": now,
			"updated_at": now,
		}).Error
}

func rotateMetaMessengerOAuthCredentialTx(
	tx *gorm.DB,
	orgID, accountID uuid.UUID,
	page metaMessengerStoredPage,
	pageInspection metaMessengerTokenInspection,
	appID string,
	expiresAt *time.Time,
) (models.ChannelCredential, error) {
	var credentials []models.ChannelCredential
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
		Where(
			"organization_id = ? AND channel_account_id = ? AND kind = ?",
			orgID,
			accountID,
			models.ChannelCredentialKindOAuth,
		).
		Order("version DESC, id ASC").
		Find(&credentials).Error; err != nil {
		return models.ChannelCredential{}, err
	}
	maximumVersion := 0
	for index := range credentials {
		if credentials[index].Version > maximumVersion {
			maximumVersion = credentials[index].Version
		}
	}
	now := time.Now().UTC()
	if err := revokeOtherCurrentMetaMessengerCredentialsTx(tx, credentials, uuid.Nil, now); err != nil {
		return models.ChannelCredential{}, err
	}
	checkedAt := pageInspection.CheckedAt.UTC()
	credential := models.ChannelCredential{
		BaseModel:        models.BaseModel{ID: uuid.New()},
		OrganizationID:   orgID,
		ChannelAccountID: accountID,
		Kind:             models.ChannelCredentialKindOAuth,
		Version:          maximumVersion + 1,
		CredentialBlob:   models.JSONB{"access_token": page.EncryptedPageToken},
		Status:           models.ChannelCredentialStatusActive,
		KeyVersion:       "app:v1",
		ExpiresAt:        expiresAt,
		LastValidatedAt:  &checkedAt,
		Metadata: models.JSONB{
			"app_id":           appID,
			"page_id":          page.PageID,
			"meta_business_id": page.BusinessID,
			"token_type":       "page",
		},
	}
	if err := tx.Create(&credential).Error; err != nil {
		return models.ChannelCredential{}, err
	}
	return credential, nil
}

// scrubSupersededMetaMessengerReviewCredentialsTx makes review conversion
// irreversible from an egress perspective. No historical webhook row is
// allowed to retain an outbound HMAC, and only the exact current OAuth row
// keeps a Page token for bounded cleanup recovery.
func scrubSupersededMetaMessengerReviewCredentialsTx(
	tx *gorm.DB,
	organizationID, accountID, currentWebhookID, currentOAuthID uuid.UUID,
	now time.Time,
) error {
	if tx == nil || organizationID == uuid.Nil || accountID == uuid.Nil ||
		currentWebhookID == uuid.Nil || currentOAuthID == uuid.Nil || now.IsZero() {
		return errors.New("review credential scrub binding is invalid")
	}
	for _, target := range []struct {
		kind   models.ChannelCredentialKind
		keepID uuid.UUID
	}{
		{kind: models.ChannelCredentialKindWebhook, keepID: currentWebhookID},
		{kind: models.ChannelCredentialKindOAuth, keepID: currentOAuthID},
	} {
		if err := tx.Model(&models.ChannelCredential{}).
			Where(
				"organization_id = ? AND channel_account_id = ? AND kind = ? AND id <> ?",
				organizationID,
				accountID,
				target.kind,
				target.keepID,
			).
			Updates(map[string]any{
				"status":          models.ChannelCredentialStatusRevoked,
				"credential_blob": models.JSONB{},
				"revoked_at":      now,
				"rotated_at":      now,
				"updated_at":      now,
			}).Error; err != nil {
			return err
		}
	}
	return nil
}

func newMetaMessengerSubscriptionOperation(
	oauth models.ChannelCredential,
	now time.Time,
) metaMessengerSubscriptionOperation {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	return metaMessengerSubscriptionOperation{
		ID:                uuid.New(),
		OAuthCredentialID: oauth.ID,
		OAuthVersion:      oauth.Version,
		DesiredState:      metaMessengerSubscriptionDesiredSubscribed,
		State:             metaMessengerSubscriptionSubscribePending,
		ExpiresAt:         now.UTC().Add(metaMessengerSubscriptionOperationLease),
	}
}

func writeMetaMessengerSubscriptionOperation(
	metadata models.JSONB,
	operation metaMessengerSubscriptionOperation,
) {
	if metadata == nil {
		return
	}
	metadata[metaMessengerSubscriptionDesiredStateKey] = operation.DesiredState
	metadata[metaMessengerSubscriptionOperationIDKey] = operation.ID.String()
	metadata[metaMessengerSubscriptionOperationStateKey] = operation.State
	metadata[metaMessengerSubscriptionOperationExpiresKey] = operation.ExpiresAt.UTC().Format(time.RFC3339Nano)
	metadata[metaMessengerSubscriptionOAuthCredentialIDKey] = operation.OAuthCredentialID.String()
	metadata[metaMessengerSubscriptionOAuthVersionKey] = strconv.Itoa(operation.OAuthVersion)
}

func metaMessengerSubscriptionOperationFromAccount(
	account *models.ChannelAccount,
) (metaMessengerSubscriptionOperation, error) {
	var operation metaMessengerSubscriptionOperation
	if account == nil || account.Metadata == nil {
		return operation, errMetaMessengerSubscriptionFence
	}
	operationID := stringConfigValue(account.Metadata, metaMessengerSubscriptionOperationIDKey)
	oauthID := stringConfigValue(account.Metadata, metaMessengerSubscriptionOAuthCredentialIDKey)
	versionText := stringConfigValue(account.Metadata, metaMessengerSubscriptionOAuthVersionKey)
	expiresText := stringConfigValue(account.Metadata, metaMessengerSubscriptionOperationExpiresKey)
	var err error
	operation.ID, err = uuid.Parse(operationID)
	if err != nil || operation.ID == uuid.Nil || operation.ID.String() != operationID {
		return metaMessengerSubscriptionOperation{}, errMetaMessengerSubscriptionFence
	}
	operation.OAuthCredentialID, err = uuid.Parse(oauthID)
	if err != nil || operation.OAuthCredentialID == uuid.Nil || operation.OAuthCredentialID.String() != oauthID {
		return metaMessengerSubscriptionOperation{}, errMetaMessengerSubscriptionFence
	}
	operation.OAuthVersion, err = strconv.Atoi(versionText)
	if err != nil || operation.OAuthVersion <= 0 || strconv.Itoa(operation.OAuthVersion) != versionText {
		return metaMessengerSubscriptionOperation{}, errMetaMessengerSubscriptionFence
	}
	operation.ExpiresAt, err = time.Parse(time.RFC3339Nano, expiresText)
	if err != nil || operation.ExpiresAt.IsZero() {
		return metaMessengerSubscriptionOperation{}, errMetaMessengerSubscriptionFence
	}
	operation.DesiredState = stringConfigValue(account.Metadata, metaMessengerSubscriptionDesiredStateKey)
	operation.State = stringConfigValue(account.Metadata, metaMessengerSubscriptionOperationStateKey)
	return operation, nil
}

func metaMessengerSubscriptionOperationMatches(
	current, expected metaMessengerSubscriptionOperation,
) bool {
	return current.ID != uuid.Nil && current.ID == expected.ID &&
		current.OAuthCredentialID != uuid.Nil && current.OAuthCredentialID == expected.OAuthCredentialID &&
		current.OAuthVersion > 0 && current.OAuthVersion == expected.OAuthVersion &&
		current.DesiredState == expected.DesiredState
}

// compensateMetaMessengerSubscribe is the second half of the lifecycle CAS.
// A provider-side subscribe that can no longer finalize locally is always
// followed by an idempotent unsubscribe/absence check. The acknowledgement is
// persisted against the fenced operation so deprovisioning knows when it is
// safe to erase the only recovery token.
func (a *App) compensateMetaMessengerSubscribe(
	ctx context.Context,
	organizationID, accountID uuid.UUID,
	expected metaMessengerSubscriptionOperation,
	pageID, pageToken string,
) error {
	if ctx == nil {
		ctx = context.Background()
	}
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	cleanupErr := a.unsubscribeMetaMessengerPage(cleanupCtx, pageID, pageToken)
	persistErr := a.recordMetaMessengerSubscribeCompensation(
		organizationID,
		accountID,
		expected,
		cleanupErr == nil,
	)
	return errors.Join(cleanupErr, persistErr)
}

func (a *App) recordMetaMessengerSubscribeCompensation(
	organizationID, accountID uuid.UUID,
	expected metaMessengerSubscriptionOperation,
	cleanupConfirmed bool,
) error {
	root := a.rootApp()
	if root == nil || root.DB == nil || organizationID == uuid.Nil ||
		accountID == uuid.Nil || expected.ID == uuid.Nil {
		return errMetaMessengerSubscriptionFence
	}
	return database.WithTenantReadCommitted(root.DB, organizationID, func(tx *gorm.DB) error {
		var account models.ChannelAccount
		if err := tx.Unscoped().Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ? AND organization_id = ?", accountID, organizationID).
			First(&account).Error; err != nil {
			return err
		}
		account.Metadata = cloneJSONB(account.Metadata)
		desiredState := stringConfigValue(account.Metadata, metaMessengerSubscriptionDesiredStateKey)
		currentOperationID := stringConfigValue(account.Metadata, metaMessengerSubscriptionOperationIDKey)
		fencedOperationID := stringConfigValue(account.Metadata, metaMessengerSubscriptionFencedOperationIDKey)
		expectedID := expected.ID.String()
		if desiredState == metaMessengerSubscriptionDesiredUnsubscribed && fencedOperationID == expectedID {
			account.Metadata[metaMessengerSubscriptionFencedAckKey] = cleanupConfirmed
			if cleanupConfirmed {
				now := time.Now().UTC()
				account.Metadata[metaMessengerSubscriptionFencedAckAtKey] = now.Format(time.RFC3339Nano)
				account.Metadata[metaMessengerSubscriptionRemoteStateKey] = metaMessengerSubscriptionRemoteUnsubscribed
				account.Metadata[metaMessengerSubscriptionRemoteConfirmedAtKey] = now.Format(time.RFC3339Nano)
			} else {
				account.Metadata[metaMessengerSubscriptionRemoteStateKey] = metaMessengerSubscriptionRemoteUnknown
				delete(account.Metadata, metaMessengerSubscriptionRemoteConfirmedAtKey)
				account.Config = cloneJSONB(account.Config)
				account.Config["onboarding_state"] = "review_remote_cleanup_pending"
				account.Metadata["onboarding_state"] = "review_remote_cleanup_pending"
			}
		} else if desiredState == metaMessengerSubscriptionDesiredSubscribed && currentOperationID == expectedID {
			account.Metadata[metaMessengerSubscriptionOperationStateKey] = metaMessengerSubscriptionSubscribeFailed
			if cleanupConfirmed {
				now := time.Now().UTC()
				account.Metadata[metaMessengerSubscriptionRemoteStateKey] = metaMessengerSubscriptionRemoteUnsubscribed
				account.Metadata[metaMessengerSubscriptionRemoteConfirmedAtKey] = now.Format(time.RFC3339Nano)
			} else {
				account.Metadata[metaMessengerSubscriptionRemoteStateKey] = metaMessengerSubscriptionRemoteUnknown
				delete(account.Metadata, metaMessengerSubscriptionRemoteConfirmedAtKey)
			}
		} else {
			return errMetaMessengerSubscriptionFence
		}
		return tx.Unscoped().Save(&account).Error
	})
}

func (a *App) finalizeMetaMessengerPendingAccount(
	orgID, userID, accountID uuid.UUID,
	state string,
	subscriptionVerified bool,
	lastError string,
) (*models.ChannelAccount, error) {
	root := a.rootApp()
	if root == nil || root.DB == nil {
		return nil, errors.New("messenger onboarding database is unavailable")
	}
	var operation metaMessengerSubscriptionOperation
	err := database.WithTenantReadCommitted(root.DB, orgID, func(tx *gorm.DB) error {
		current, err := loadChannelAccount(tx, orgID, accountID, true)
		if err != nil {
			return err
		}
		operation, err = metaMessengerSubscriptionOperationFromAccount(current)
		return err
	})
	if err != nil {
		return nil, err
	}
	remoteState := metaMessengerSubscriptionRemoteUnknown
	if subscriptionVerified {
		remoteState = metaMessengerSubscriptionRemoteSubscribed
	}
	return a.finalizeMetaMessengerPendingAccountOperation(
		orgID,
		userID,
		accountID,
		operation,
		state,
		subscriptionVerified,
		lastError,
		remoteState,
	)
}

func (a *App) finalizeMetaMessengerPendingAccountOperation(
	orgID, userID, accountID uuid.UUID,
	expected metaMessengerSubscriptionOperation,
	state string,
	subscriptionVerified bool,
	lastError, remoteState string,
) (*models.ChannelAccount, error) {
	root := a.rootApp()
	if root == nil || root.DB == nil || expected.ID == uuid.Nil ||
		expected.OAuthCredentialID == uuid.Nil || expected.OAuthVersion <= 0 ||
		expected.DesiredState != metaMessengerSubscriptionDesiredSubscribed {
		return nil, errMetaMessengerSubscriptionFence
	}
	var account *models.ChannelAccount
	err := database.WithTenantReadCommitted(root.DB, orgID, func(tx *gorm.DB) error {
		current, err := loadChannelAccount(tx, orgID, accountID, true)
		if err != nil {
			return err
		}
		operation, err := metaMessengerSubscriptionOperationFromAccount(current)
		if err != nil || !metaMessengerSubscriptionOperationMatches(operation, expected) ||
			operation.DesiredState != metaMessengerSubscriptionDesiredSubscribed {
			return errMetaMessengerSubscriptionFence
		}
		var oauth models.ChannelCredential
		if err := tx.Where(
			"id = ? AND organization_id = ? AND channel_account_id = ? AND kind = ? AND version = ? AND status IN ?",
			expected.OAuthCredentialID,
			orgID,
			accountID,
			models.ChannelCredentialKindOAuth,
			expected.OAuthVersion,
			[]models.ChannelCredentialStatus{
				models.ChannelCredentialStatusActive,
				models.ChannelCredentialStatusExpiring,
			},
		).First(&oauth).Error; err != nil ||
			stringConfigValue(oauth.Metadata, "app_id") != stringConfigValue(current.Metadata, "meta_app_id") ||
			stringConfigValue(oauth.Metadata, "page_id") != current.ExternalAccountID ||
			stringConfigValue(oauth.Metadata, "meta_business_id") != stringConfigValue(current.Metadata, "meta_business_id") {
			return errMetaMessengerSubscriptionFence
		}
		old := *current
		old.Config = cloneJSONB(current.Config)
		old.Metadata = cloneJSONB(current.Metadata)
		old.Capabilities = cloneJSONB(current.Capabilities)
		reviewConfigured := root.configuredMetaMessengerReviewAccount(current)
		if reviewConfigured && subscriptionVerified {
			state = metaMessengerReviewReadyState
		}
		current.Status = models.ChannelAccountStatusPending
		current.Config = cloneJSONB(current.Config)
		current.Config["onboarding_state"] = state
		current.Config["registry_recognized"] = false
		current.Config["outbound_enabled"] = false
		current.Config["ai_reply_enabled"] = false
		current.IsDefaultOutgoing = false
		current.Metadata = cloneJSONB(current.Metadata)
		current.Metadata["onboarding_state"] = state
		current.Metadata["subscription_verified"] = subscriptionVerified
		current.Metadata["registry_recognized"] = false
		now := time.Now().UTC()
		if subscriptionVerified {
			current.Metadata[metaMessengerSubscriptionOperationStateKey] = metaMessengerSubscriptionSubscribeComplete
			current.Metadata[metaMessengerSubscriptionRemoteStateKey] = metaMessengerSubscriptionRemoteSubscribed
			current.Metadata[metaMessengerSubscriptionRemoteConfirmedAtKey] = now.Format(time.RFC3339Nano)
		} else {
			current.Metadata[metaMessengerSubscriptionOperationStateKey] = metaMessengerSubscriptionSubscribeFailed
			if remoteState != metaMessengerSubscriptionRemoteUnsubscribed {
				remoteState = metaMessengerSubscriptionRemoteUnknown
			}
			current.Metadata[metaMessengerSubscriptionRemoteStateKey] = remoteState
			if remoteState == metaMessengerSubscriptionRemoteUnsubscribed {
				current.Metadata[metaMessengerSubscriptionRemoteConfirmedAtKey] = now.Format(time.RFC3339Nano)
			} else {
				delete(current.Metadata, metaMessengerSubscriptionRemoteConfirmedAtKey)
			}
		}
		if reviewConfigured {
			current.Config["review_only"] = true
			current.Metadata["review_ready"] = subscriptionVerified
			if subscriptionVerified {
				review := root.Config.MetaMessengerReviewRelay
				current.Metadata["review_relay_mode"] = metareview.Marker
				current.Metadata["review_generation"] = review.Generation
				current.Metadata["review_expires_at"] = review.ExpiresAt
				current.Metadata["review_ready_at"] = now.Format(time.RFC3339Nano)
			} else {
				delete(current.Metadata, "review_relay_mode")
				delete(current.Metadata, "review_generation")
				delete(current.Metadata, "review_expires_at")
				delete(current.Metadata, "review_ready_at")
			}
		}
		if subscriptionVerified {
			current.Metadata["subscription_verified_at"] = now.Format(time.RFC3339Nano)
			current.LastError = ""
			current.LastErrorAt = nil
		} else {
			current.LastError = lastError
			current.LastErrorAt = &now
		}
		current.UpdatedByID = &userID
		if err := tx.Save(current).Error; err != nil {
			return err
		}
		if err := audit.LogAudit(
			tx,
			orgID,
			userID,
			audit.GetUserName(tx, userID),
			"channel_account",
			current.ID,
			models.AuditActionUpdated,
			&old,
			current,
		); err != nil {
			return err
		}
		account = current
		return nil
	})
	return account, err
}

func metaMessengerRelayURL(baseURL, pageID string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || !strings.EqualFold(parsed.Scheme, "https") || parsed.Hostname() == "" ||
		parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || !validCanonicalMetaID(pageID) {
		return "", errors.New("trusted Messenger relay URL is invalid")
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/") + "/v1/accounts/messenger/" + url.PathEscape(pageID)
	return parsed.String(), nil
}

func metaMessengerAccountName(pageName, pageID string) string {
	pageName = strings.TrimSpace(pageName)
	if pageName == "" {
		pageName = "Page"
	}
	suffix := " · " + pageID
	maximumNameRunes := 120 - utf8.RuneCountInString("Messenger · ") - utf8.RuneCountInString(suffix)
	if maximumNameRunes < 1 {
		return truncateChannelRunes("Messenger"+suffix, 120)
	}
	return "Messenger · " + truncateChannelRunes(pageName, maximumNameRunes) + suffix
}
