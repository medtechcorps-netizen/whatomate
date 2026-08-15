package handlers

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	configpkg "github.com/shridarpatil/whatomate/internal/config"
)

const (
	metaInstagramGraphResponseLimit int64 = 1 << 20
)

var metaInstagramRequiredScopes = []string{
	"instagram_business_basic",
	"instagram_business_manage_messages",
}

type metaInstagramTokenResponse struct {
	AccessToken string   `json:"access_token"`
	TokenType   string   `json:"token_type"`
	UserID      string   `json:"user_id"`
	ExpiresIn   int64    `json:"expires_in"`
	Permissions []string `json:"-"`
}

type metaInstagramShortTokenEnvelope struct {
	Data []struct {
		AccessToken string `json:"access_token"`
		UserID      string `json:"user_id"`
		Permissions string `json:"permissions"`
	} `json:"data"`
}

type metaInstagramTokenInspection struct {
	AppID     string
	UserID    string
	Scopes    []string
	ExpiresAt *time.Time
	CheckedAt time.Time
}

type metaInstagramProfile struct {
	ID                string `json:"id"`
	UserID            string `json:"user_id"`
	Username          string `json:"username"`
	AccountType       string `json:"account_type"`
	ProfilePictureURL string `json:"profile_picture_url"`
}

type metaInstagramProfileEnvelope struct {
	Data []metaInstagramProfile `json:"data"`
}

func (profile metaInstagramProfile) oauthSubjectID() string {
	return strings.TrimSpace(profile.ID)
}

func (profile metaInstagramProfile) professionalAccountID() string {
	return strings.TrimSpace(profile.UserID)
}

type metaInstagramProviderError struct {
	StatusCode int
	Code       int
	Subcode    int
	Type       string
	TraceID    string
	RequestID  string
}

// metaInstagramLifecycleBindingError marks a syntactically valid provider
// response that deterministically invalidates the current authorization. It
// must never enter the transient grace period used for transport/429/5xx or
// malformed provider responses.
type metaInstagramLifecycleBindingError struct {
	Revoked bool
	Reason  string
}

func (e *metaInstagramLifecycleBindingError) Error() string {
	return "Instagram authorization binding is no longer valid"
}

func metaInstagramRevokedBinding(reason string) error {
	return &metaInstagramLifecycleBindingError{Revoked: true, Reason: reason}
}

func metaInstagramStaleBinding(reason string) error {
	return &metaInstagramLifecycleBindingError{Reason: reason}
}

func (e *metaInstagramProviderError) Error() string {
	if e == nil {
		return "instagram provider request failed"
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
	return "instagram provider request failed (" + strings.Join(parts, ", ") + ")"
}

type metaInstagramGraphErrorEnvelope struct {
	Error struct {
		Type         string `json:"type"`
		Code         int    `json:"code"`
		ErrorSubcode int    `json:"error_subcode"`
		FBTraceID    string `json:"fbtrace_id"`
	} `json:"error"`
	ErrorType string `json:"error_type"`
	Code      int    `json:"code"`
}

func metaInstagramAppSecretProof(accessToken, appSecret string) string {
	mac := hmac.New(sha256.New, []byte(appSecret))
	_, _ = mac.Write([]byte(accessToken))
	return hex.EncodeToString(mac.Sum(nil))
}

func (a *App) exchangeMetaInstagramAuthorizationCode(
	ctx context.Context,
	settings configpkg.MetaInstagramConfig,
	code, redirectURI string,
) (metaInstagramTokenResponse, error) {
	endpoint, err := metaInstagramJoinProviderURL(settings.TokenBaseURL, "oauth", "access_token")
	if err != nil {
		return metaInstagramTokenResponse{}, err
	}
	form := url.Values{
		"client_id":     []string{settings.AppID},
		"client_secret": []string{settings.AppSecret},
		"grant_type":    []string{"authorization_code"},
		"redirect_uri":  []string{redirectURI},
		"code":          []string{code},
	}
	request, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		endpoint,
		strings.NewReader(form.Encode()),
	)
	if err != nil {
		return metaInstagramTokenResponse{}, errors.New("invalid Instagram token request")
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "ReReply-Meta-Instagram/1.0")
	var envelope metaInstagramShortTokenEnvelope
	if err := a.doMetaInstagramJSON(request, &envelope); err != nil {
		return metaInstagramTokenResponse{}, err
	}
	if len(envelope.Data) != 1 {
		return metaInstagramTokenResponse{}, errors.New("instagram short token response is invalid")
	}
	shortToken := envelope.Data[0]
	response := metaInstagramTokenResponse{
		AccessToken: strings.TrimSpace(shortToken.AccessToken),
		UserID:      strings.TrimSpace(shortToken.UserID),
		Permissions: normalizeMetaInstagramScopes(strings.Split(shortToken.Permissions, ",")),
	}
	if response.AccessToken == "" || !validCanonicalMetaID(response.UserID) ||
		len(response.Permissions) == 0 || len(missingMetaInstagramScopes(response.Permissions)) > 0 {
		return metaInstagramTokenResponse{}, errors.New("instagram short token response is invalid")
	}
	return response, nil
}

func (a *App) exchangeMetaInstagramLongLivedToken(
	ctx context.Context,
	settings configpkg.MetaInstagramConfig,
	shortToken string,
) (metaInstagramTokenResponse, error) {
	endpoint, err := metaInstagramJoinProviderURL(settings.GraphBaseURL, "access_token")
	if err != nil {
		return metaInstagramTokenResponse{}, err
	}
	parsed, _ := url.Parse(endpoint)
	query := parsed.Query()
	query.Set("grant_type", "ig_exchange_token")
	query.Set("client_secret", settings.AppSecret)
	query.Set("access_token", shortToken)
	query.Set("appsecret_proof", metaInstagramAppSecretProof(shortToken, settings.AppSecret))
	parsed.RawQuery = query.Encode()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return metaInstagramTokenResponse{}, errors.New("invalid Instagram long-lived token request")
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "ReReply-Meta-Instagram/1.0")
	var response metaInstagramTokenResponse
	if err := a.doMetaInstagramJSON(request, &response); err != nil {
		return metaInstagramTokenResponse{}, err
	}
	response.AccessToken = strings.TrimSpace(response.AccessToken)
	if response.AccessToken == "" || response.ExpiresIn <= 0 {
		return metaInstagramTokenResponse{}, errors.New("instagram long-lived token response is invalid")
	}
	return response, nil
}

func (a *App) refreshMetaInstagramLongLivedToken(
	ctx context.Context,
	settings configpkg.MetaInstagramConfig,
	currentToken string,
) (metaInstagramTokenResponse, error) {
	endpoint, err := metaInstagramJoinProviderURL(settings.GraphBaseURL, "refresh_access_token")
	if err != nil {
		return metaInstagramTokenResponse{}, err
	}
	parsed, _ := url.Parse(endpoint)
	query := parsed.Query()
	query.Set("grant_type", "ig_refresh_token")
	query.Set("access_token", currentToken)
	query.Set("appsecret_proof", metaInstagramAppSecretProof(currentToken, settings.AppSecret))
	parsed.RawQuery = query.Encode()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return metaInstagramTokenResponse{}, errors.New("invalid Instagram refresh request")
	}
	request.Header.Set("Authorization", "Bearer "+currentToken)
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "ReReply-Meta-Instagram/1.0")
	var response metaInstagramTokenResponse
	if err := a.doMetaInstagramJSON(request, &response); err != nil {
		return metaInstagramTokenResponse{}, err
	}
	response.AccessToken = strings.TrimSpace(response.AccessToken)
	if response.AccessToken == "" || response.ExpiresIn <= 0 {
		return metaInstagramTokenResponse{}, errors.New("instagram refresh response is invalid")
	}
	return response, nil
}

func (a *App) inspectMetaInstagramToken(
	ctx context.Context,
	settings configpkg.MetaInstagramConfig,
	accessToken string,
) (metaInstagramTokenInspection, error) {
	endpoint, err := metaInstagramJoinProviderURL(settings.GraphBaseURL, "debug_token")
	if err != nil {
		return metaInstagramTokenInspection{}, err
	}
	appAccessToken := settings.AppID + "|" + settings.AppSecret
	parsed, _ := url.Parse(endpoint)
	query := parsed.Query()
	query.Set("input_token", accessToken)
	query.Set("appsecret_proof", metaInstagramAppSecretProof(appAccessToken, settings.AppSecret))
	parsed.RawQuery = query.Encode()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return metaInstagramTokenInspection{}, errors.New("invalid Instagram token inspection request")
	}
	request.Header.Set("Authorization", "Bearer "+appAccessToken)
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "ReReply-Meta-Instagram/1.0")
	var response struct {
		Data struct {
			AppID     string   `json:"app_id"`
			IsValid   bool     `json:"is_valid"`
			UserID    string   `json:"user_id"`
			Scopes    []string `json:"scopes"`
			ExpiresAt int64    `json:"expires_at"`
		} `json:"data"`
	}
	if err := a.doMetaInstagramJSON(request, &response); err != nil {
		return metaInstagramTokenInspection{}, err
	}
	if !response.Data.IsValid || strings.TrimSpace(response.Data.AppID) != settings.AppID ||
		!validCanonicalMetaID(strings.TrimSpace(response.Data.UserID)) {
		return metaInstagramTokenInspection{}, metaInstagramRevokedBinding("instagram_token_binding_invalid")
	}
	scopes := normalizeMetaInstagramScopes(response.Data.Scopes)
	missing := missingMetaInstagramScopes(scopes)
	if len(missing) > 0 {
		return metaInstagramTokenInspection{}, metaInstagramStaleBinding("instagram_required_permissions_missing")
	}
	now := time.Now().UTC()
	var expiresAt *time.Time
	if response.Data.ExpiresAt > 0 {
		value := time.Unix(response.Data.ExpiresAt, 0).UTC()
		if !value.After(now.Add(5 * time.Second)) {
			return metaInstagramTokenInspection{}, metaInstagramRevokedBinding("instagram_authorization_expired")
		}
		expiresAt = &value
	}
	return metaInstagramTokenInspection{
		AppID: settings.AppID, UserID: strings.TrimSpace(response.Data.UserID),
		Scopes: scopes, ExpiresAt: expiresAt, CheckedAt: now,
	}, nil
}

func (a *App) fetchMetaInstagramProfile(
	ctx context.Context,
	settings configpkg.MetaInstagramConfig,
	accessToken string,
) (metaInstagramProfile, error) {
	endpoint, err := metaInstagramVersionedURL(settings, "me")
	if err != nil {
		return metaInstagramProfile{}, err
	}
	parsed, _ := url.Parse(endpoint)
	query := parsed.Query()
	query.Set("fields", "id,user_id,username,account_type,profile_picture_url")
	query.Set("appsecret_proof", metaInstagramAppSecretProof(accessToken, settings.AppSecret))
	parsed.RawQuery = query.Encode()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return metaInstagramProfile{}, errors.New("invalid Instagram profile request")
	}
	request.Header.Set("Authorization", "Bearer "+accessToken)
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "ReReply-Meta-Instagram/1.0")
	var envelope metaInstagramProfileEnvelope
	if err := a.doMetaInstagramJSON(request, &envelope); err != nil {
		return metaInstagramProfile{}, err
	}
	if len(envelope.Data) != 1 {
		return metaInstagramProfile{}, metaInstagramRevokedBinding("instagram_profile_identity_invalid")
	}
	profile := envelope.Data[0]
	profile.ID = strings.TrimSpace(profile.ID)
	profile.UserID = strings.TrimSpace(profile.UserID)
	profile.Username = strings.TrimSpace(profile.Username)
	profile.AccountType = strings.ToUpper(strings.TrimSpace(profile.AccountType))
	if !validCanonicalMetaID(profile.oauthSubjectID()) ||
		!validCanonicalMetaID(profile.professionalAccountID()) {
		return metaInstagramProfile{}, metaInstagramRevokedBinding("instagram_profile_identity_invalid")
	}
	if profile.AccountType != "BUSINESS" && profile.AccountType != "MEDIA_CREATOR" {
		return metaInstagramProfile{}, metaInstagramStaleBinding("instagram_professional_account_required")
	}
	if profile.Username == "" || len([]rune(profile.Username)) > 100 {
		return metaInstagramProfile{}, metaInstagramStaleBinding("instagram_profile_username_invalid")
	}
	return profile, nil
}

func (a *App) subscribeMetaInstagramMessages(
	ctx context.Context,
	settings configpkg.MetaInstagramConfig,
	accountID, accessToken string,
) error {
	endpoint, err := metaInstagramVersionedURL(settings, accountID, "subscribed_apps")
	if err != nil {
		return err
	}
	parsed, _ := url.Parse(endpoint)
	query := parsed.Query()
	query.Set("subscribed_fields", "messages")
	query.Set("appsecret_proof", metaInstagramAppSecretProof(accessToken, settings.AppSecret))
	parsed.RawQuery = query.Encode()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, parsed.String(), bytes.NewReader(nil))
	if err != nil {
		return errors.New("invalid Instagram subscription request")
	}
	request.Header.Set("Authorization", "Bearer "+accessToken)
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("User-Agent", "ReReply-Meta-Instagram/1.0")
	var result struct {
		Success bool `json:"success"`
	}
	if err := a.doMetaInstagramJSON(request, &result); err != nil {
		return err
	}
	if !result.Success {
		return errors.New("instagram messages subscription was not confirmed")
	}
	subscribed, err := a.metaInstagramHasConfiguredAppSubscription(ctx, settings, accountID, accessToken)
	if err != nil {
		return err
	}
	if !subscribed {
		return errors.New("instagram messages subscription is missing")
	}
	return nil
}

func (a *App) unsubscribeMetaInstagramMessages(
	ctx context.Context,
	settings configpkg.MetaInstagramConfig,
	accountID, accessToken string,
) error {
	endpoint, err := metaInstagramVersionedURL(settings, accountID, "subscribed_apps")
	if err != nil {
		return err
	}
	parsed, _ := url.Parse(endpoint)
	query := parsed.Query()
	query.Set("appsecret_proof", metaInstagramAppSecretProof(accessToken, settings.AppSecret))
	parsed.RawQuery = query.Encode()
	request, err := http.NewRequestWithContext(ctx, http.MethodDelete, parsed.String(), nil)
	if err != nil {
		return errors.New("invalid Instagram unsubscription request")
	}
	request.Header.Set("Authorization", "Bearer "+accessToken)
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "ReReply-Meta-Instagram/1.0")
	var result struct {
		Success bool `json:"success"`
	}
	if err := a.doMetaInstagramJSON(request, &result); err != nil {
		return err
	}
	if !result.Success {
		return errors.New("instagram unsubscription was not confirmed")
	}
	subscribed, err := a.metaInstagramHasConfiguredAppSubscription(ctx, settings, accountID, accessToken)
	if err != nil {
		return err
	}
	if subscribed {
		return errors.New("instagram messages subscription still exists")
	}
	return nil
}

func (a *App) metaInstagramHasConfiguredAppSubscription(
	ctx context.Context,
	settings configpkg.MetaInstagramConfig,
	accountID, accessToken string,
) (bool, error) {
	endpoint, err := metaInstagramVersionedURL(settings, accountID, "subscribed_apps")
	if err != nil {
		return false, err
	}
	parsed, _ := url.Parse(endpoint)
	query := parsed.Query()
	query.Set("fields", "id,subscribed_fields")
	query.Set("appsecret_proof", metaInstagramAppSecretProof(accessToken, settings.AppSecret))
	parsed.RawQuery = query.Encode()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return false, errors.New("invalid Instagram subscription inspection request")
	}
	request.Header.Set("Authorization", "Bearer "+accessToken)
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "ReReply-Meta-Instagram/1.0")
	var result struct {
		Data []struct {
			ID               string   `json:"id"`
			SubscribedFields []string `json:"subscribed_fields"`
		} `json:"data"`
	}
	if err := a.doMetaInstagramJSON(request, &result); err != nil {
		return false, err
	}
	for _, subscription := range result.Data {
		if strings.TrimSpace(subscription.ID) != settings.AppID {
			continue
		}
		for _, field := range subscription.SubscribedFields {
			if strings.EqualFold(strings.TrimSpace(field), "messages") {
				return true, nil
			}
		}
	}
	return false, nil
}

func (a *App) doMetaInstagramJSON(request *http.Request, destination any) error {
	if a == nil || request == nil || request.URL == nil || destination == nil {
		return errors.New("instagram provider request is invalid")
	}
	client := a.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	copyClient := *client
	copyClient.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}
	response, err := copyClient.Do(request)
	if err != nil {
		return errors.New("instagram provider request failed")
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, metaInstagramGraphResponseLimit))
		_ = response.Body.Close()
	}()
	requestID := strings.TrimSpace(response.Header.Get("x-fb-request-id"))
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		var envelope metaInstagramGraphErrorEnvelope
		decoder := json.NewDecoder(io.LimitReader(response.Body, metaInstagramGraphResponseLimit))
		_ = decoder.Decode(&envelope)
		providerErr := &metaInstagramProviderError{
			StatusCode: response.StatusCode, Code: envelope.Error.Code,
			Subcode: envelope.Error.ErrorSubcode, Type: strings.TrimSpace(envelope.Error.Type),
			TraceID: strings.TrimSpace(envelope.Error.FBTraceID), RequestID: requestID,
		}
		if providerErr.Code == 0 {
			providerErr.Code = envelope.Code
		}
		if providerErr.Type == "" {
			providerErr.Type = strings.TrimSpace(envelope.ErrorType)
		}
		return providerErr
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, metaInstagramGraphResponseLimit))
	if err := decoder.Decode(destination); err != nil {
		return errors.New("instagram provider response is invalid")
	}
	if err := rejectTrailingInstagramJSON(decoder); err != nil {
		return errors.New("instagram provider response is invalid")
	}
	return nil
}

func rejectTrailingInstagramJSON(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); errors.Is(err, io.EOF) {
		return nil
	} else if err != nil {
		return err
	}
	return errors.New("multiple JSON values are not allowed")
}

func missingMetaInstagramScopes(scopes []string) []string {
	granted := make(map[string]struct{}, len(scopes))
	for _, scope := range scopes {
		granted[scope] = struct{}{}
	}
	missing := make([]string, 0)
	for _, required := range metaInstagramRequiredScopes {
		if _, ok := granted[required]; !ok {
			missing = append(missing, required)
		}
	}
	return missing
}

func normalizeMetaInstagramScopes(scopes []string) []string {
	seen := make(map[string]struct{}, len(scopes))
	values := make([]string, 0, len(scopes))
	for _, scope := range scopes {
		scope = strings.ToLower(strings.TrimSpace(scope))
		if scope == "" {
			continue
		}
		if _, exists := seen[scope]; exists {
			continue
		}
		seen[scope] = struct{}{}
		values = append(values, scope)
	}
	sort.Strings(values)
	return values
}

func metaInstagramVersionedURL(
	settings configpkg.MetaInstagramConfig,
	path ...string,
) (string, error) {
	segments := append([]string{strings.Trim(settings.GraphAPIVersion, "/")}, path...)
	return metaInstagramJoinProviderURL(settings.GraphBaseURL, segments...)
}

func metaInstagramJoinProviderURL(base string, path ...string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(base))
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil ||
		parsed.RawQuery != "" || parsed.Fragment != "" || parsed.RawPath != "" ||
		parsed.ForceQuery || parsed.Opaque != "" {
		return "", errors.New("instagram provider URL is invalid")
	}
	joined, err := url.JoinPath(parsed.String(), path...)
	if err != nil {
		return "", errors.New("instagram provider URL is invalid")
	}
	return joined, nil
}

func metaInstagramExpiry(now time.Time, expiresIn int64, inspected *time.Time) (*time.Time, error) {
	if expiresIn <= 0 {
		return nil, errors.New("instagram token expiry is invalid")
	}
	expiresAt := now.UTC().Add(time.Duration(expiresIn) * time.Second)
	if inspected != nil && inspected.Before(expiresAt) {
		expiresAt = inspected.UTC()
	}
	if !expiresAt.After(now.UTC().Add(5 * time.Second)) {
		return nil, errors.New("instagram token expires too soon")
	}
	return &expiresAt, nil
}

func metaInstagramAccountName(username, accountID string) string {
	username = strings.TrimSpace(username)
	if username == "" {
		username = "Instagram Professional Account"
	}
	name := "Instagram @" + strings.TrimPrefix(username, "@")
	if len([]rune(name)) > 100 {
		name = string([]rune(name)[:100])
	}
	return name
}
