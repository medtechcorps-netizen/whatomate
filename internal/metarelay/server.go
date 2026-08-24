package metarelay

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
	"log/slog"
	"net/http"
	"net/url"
	pathpkg "path"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	channelapi "github.com/shridarpatil/whatomate/internal/channel"
	"github.com/shridarpatil/whatomate/internal/metaregistry"
	"github.com/shridarpatil/whatomate/internal/models"
)

const (
	defaultFacebookGraphBase  = "https://graph.facebook.com"
	defaultInstagramGraphBase = "https://graph.instagram.com"
	accountHealthTimeout      = 5 * time.Second
	defaultOutboundTimeout    = 15 * time.Second
	defaultSettlementTimeout  = 5 * time.Second
)

type ServerOption func(*Server)

func WithServerHTTPClient(client *http.Client) ServerOption {
	return func(server *Server) {
		if client != nil {
			server.client = client
		}
	}
}

func withGraphBases(facebook, instagram string) ServerOption {
	return func(server *Server) {
		if facebook != "" {
			server.facebookGraphBase = strings.TrimRight(facebook, "/")
		}
		if instagram != "" {
			server.instagramGraphBase = strings.TrimRight(instagram, "/")
		}
	}
}

func WithServerLogger(logger *slog.Logger) ServerOption {
	return func(server *Server) {
		if logger != nil {
			server.logger = logger
		}
	}
}

func withOutboundTimeouts(delivery, settlement time.Duration) ServerOption {
	return func(server *Server) {
		if delivery > 0 {
			server.outboundTimeout = delivery
		}
		if settlement > 0 {
			server.settlementTimeout = settlement
		}
	}
}

func NewServer(config *Config, store ServerStore, options ...ServerOption) (*Server, error) {
	if config == nil {
		return nil, errors.New("relay config is required")
	}
	if store == nil {
		return nil, errors.New("durable relay store is required")
	}
	client := &http.Client{
		Timeout: 15 * time.Second,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	server := &Server{
		config:             config,
		store:              store,
		client:             client,
		logger:             slog.New(slog.NewTextHandler(io.Discard, nil)),
		now:                time.Now,
		facebookGraphBase:  defaultFacebookGraphBase,
		instagramGraphBase: defaultInstagramGraphBase,
		outboundTimeout:    defaultOutboundTimeout,
		settlementTimeout:  defaultSettlementTimeout,
	}
	for _, option := range options {
		option(server)
	}
	registry, err := NewRegistryClient(config, nil)
	if err != nil {
		return nil, err
	}
	server.registry = registry
	if server.outboundTimeout <= 0 || server.settlementTimeout <= 0 {
		return nil, errors.New("outbound relay timeouts must be positive")
	}
	return server, nil
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /livez", s.handleLive)
	mux.HandleFunc("GET /readyz", s.handleReady)
	mux.HandleFunc(
		"GET /v1/meta/messenger/webhook",
		s.metaVerificationHandler(WebhookAppMessenger),
	)
	mux.HandleFunc(
		"POST /v1/meta/messenger/webhook",
		s.metaWebhookHandler(WebhookAppMessenger),
	)
	mux.HandleFunc(
		"GET /v1/meta/messenger/managed-webhook",
		s.metaVerificationHandler(WebhookAppManagedMessenger),
	)
	mux.HandleFunc(
		"POST /v1/meta/messenger/managed-webhook",
		s.metaWebhookHandler(WebhookAppManagedMessenger),
	)
	for index := range s.config.StaticMessengerApps {
		app := &s.config.StaticMessengerApps[index]
		webhookApp := staticMessengerWebhookApp(app.AppID)
		path := "/v1/meta/messenger/apps/" + app.AppID + "/webhook"
		mux.HandleFunc("GET "+path, s.metaVerificationHandler(webhookApp))
		mux.HandleFunc("POST "+path, s.metaWebhookHandler(webhookApp))
	}
	mux.HandleFunc(
		"GET /v1/meta/instagram/webhook",
		s.metaVerificationHandler(WebhookAppInstagramLogin),
	)
	mux.HandleFunc(
		"POST /v1/meta/instagram/webhook",
		s.metaWebhookHandler(WebhookAppInstagramLogin),
	)
	mux.HandleFunc(
		"GET /v1/meta/instagram/managed-webhook",
		s.metaVerificationHandler(WebhookAppManagedInstagram),
	)
	mux.HandleFunc(
		"POST /v1/meta/instagram/managed-webhook",
		s.metaWebhookHandler(WebhookAppManagedInstagram),
	)
	mux.HandleFunc("HEAD /v1/accounts/{channel}/{externalID}", s.handleAccountHealth)
	mux.HandleFunc("POST /v1/accounts/{channel}/{externalID}", s.handleReReplyOutbound)
	return securityHeaders(rejectNonCanonicalPath(mux))
}

func (s *Server) handleLive(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleReady(w http.ResponseWriter, request *http.Request) {
	ctx, cancel := context.WithTimeout(request.Context(), 2*time.Second)
	defer cancel()
	if err := s.store.Ping(ctx); err != nil {
		writeError(w, http.StatusServiceUnavailable, "not_ready")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) metaVerificationHandler(webhookApp WebhookApp) http.HandlerFunc {
	return func(w http.ResponseWriter, request *http.Request) {
		s.handleMetaVerification(w, request, webhookApp)
	}
}

func (s *Server) handleMetaVerification(
	w http.ResponseWriter,
	request *http.Request,
	webhookApp WebhookApp,
) {
	_, verifyToken, ok := s.webhookCredentials(webhookApp)
	if !ok {
		writeError(w, http.StatusNotFound, "unknown_webhook")
		return
	}
	query := request.URL.Query()
	modes := query["hub.mode"]
	tokens := query["hub.verify_token"]
	challenges := query["hub.challenge"]
	if len(modes) != 1 || modes[0] != "subscribe" ||
		len(tokens) != 1 || !constantTimeEqual(tokens[0], verifyToken) ||
		len(challenges) != 1 || challenges[0] == "" {
		writeError(w, http.StatusForbidden, "verification_failed")
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, challenges[0])
}

func (s *Server) metaWebhookHandler(webhookApp WebhookApp) http.HandlerFunc {
	return func(w http.ResponseWriter, request *http.Request) {
		s.handleMetaWebhook(w, request, webhookApp)
	}
}

func (s *Server) handleMetaWebhook(
	w http.ResponseWriter,
	request *http.Request,
	webhookApp WebhookApp,
) {
	raw, err := readLimitedBody(w, request, defaultInboundBodyLimit)
	if err != nil {
		writeError(w, bodyReadStatus(err), "invalid_request_body")
		return
	}
	appSecret, _, ok := s.webhookCredentials(webhookApp)
	if !ok {
		writeError(w, http.StatusNotFound, "unknown_webhook")
		return
	}
	signatures := request.Header.Values(MetaSignatureHeader)
	if len(signatures) != 1 || !verifySignedBody(appSecret, signatures[0], raw) {
		writeError(w, http.StatusUnauthorized, "invalid_signature")
		return
	}
	// Meta expects acknowledgement within five seconds. Registry resolution
	// and durable acceptance share one four-second budget rather than each
	// receiving an independent timeout.
	ctx, cancel := context.WithTimeout(request.Context(), 4*time.Second)
	defer cancel()
	jobs, err := normalizeInboundResolvedWithLimit(
		ctx, s.config, s.registry, webhookApp, raw,
		channelapi.RelayCanonicalWebhookMaxBodyBytes,
	)
	if err != nil {
		switch {
		case errors.Is(err, ErrUnknownAccount), errors.Is(err, ErrWebhookAppMismatch):
			writeError(w, http.StatusNotFound, "unknown_account")
		case errors.Is(err, ErrUnsupportedObject):
			writeError(w, http.StatusUnprocessableEntity, "unsupported_object")
		case errors.Is(err, ErrCanonicalJobTooLarge):
			writeError(w, http.StatusRequestEntityTooLarge, "canonical_payload_too_large")
		case errors.Is(err, ErrRegistryUnavailable):
			writeError(w, http.StatusServiceUnavailable, "registry_unavailable")
		case errors.Is(err, ErrRegistryStale):
			writeError(w, http.StatusServiceUnavailable, "registry_binding_stale")
		default:
			writeError(w, http.StatusBadRequest, "invalid_payload")
		}
		return
	}

	// Acknowledge only after this atomic Redis operation has durably stored all
	// canonical jobs (or the no-op marker) inside the shared deadline.
	acceptanceID := inboundAcceptanceID(webhookApp, raw)
	if _, err := s.store.AcceptInbound(ctx, acceptanceID, jobs); err != nil {
		s.logger.Error("Meta delivery was not durably accepted", "component", "inbound_accept")
		writeError(w, http.StatusServiceUnavailable, "durable_acceptance_failed")
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, "EVENT_RECEIVED")
}

func (s *Server) webhookCredentials(webhookApp WebhookApp) (string, string, bool) {
	switch webhookApp {
	case WebhookAppMessenger:
		return s.config.MessengerAppSecret, s.config.MessengerVerifyToken, true
	case WebhookAppManagedMessenger:
		if !s.config.RegistryEnabled || strings.TrimSpace(s.config.ManagedMessengerAppSecret) == "" ||
			strings.TrimSpace(s.config.ManagedMessengerVerifyToken) == "" {
			return "", "", false
		}
		return s.config.ManagedMessengerAppSecret, s.config.ManagedMessengerVerifyToken, true
	case WebhookAppInstagramLogin:
		return s.config.InstagramLoginAppSecret, s.config.InstagramLoginVerifyToken, true
	case WebhookAppManagedInstagram:
		if !s.config.RegistryEnabled || strings.TrimSpace(s.config.ManagedInstagramAppSecret) == "" ||
			strings.TrimSpace(s.config.ManagedInstagramVerifyToken) == "" {
			return "", "", false
		}
		return s.config.ManagedInstagramAppSecret, s.config.ManagedInstagramVerifyToken, true
	default:
		app, ok := s.config.staticMessengerAppForRoute(webhookApp)
		if !ok {
			return "", "", false
		}
		return app.appSecret, app.verifyToken, true
	}
}

func (s *Server) handleAccountHealth(w http.ResponseWriter, request *http.Request) {
	account, err := s.accountFromPath(request, metaregistry.ResolvePurposeHealth, false)
	if err != nil {
		if errors.Is(err, ErrRegistryStale) {
			writeError(w, http.StatusServiceUnavailable, "registry_binding_stale")
			return
		}
		if errors.Is(err, ErrRegistryUnavailable) {
			writeError(w, http.StatusServiceUnavailable, "registry_unavailable")
			return
		}
		if errors.Is(err, ErrRegistryUnauthorized) {
			writeError(w, http.StatusUnauthorized, "service_authentication_required")
			return
		}
		writeError(w, http.StatusNotFound, "unknown_account")
		return
	}
	if request.ContentLength != 0 ||
		len(request.TransferEncoding) != 0 ||
		!verifySignedBody(account.reReplyOutboundSecret, request.Header.Get(ReReplySignatureHeader), nil) {
		writeError(w, http.StatusUnauthorized, "invalid_signature")
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), accountHealthTimeout)
	defer cancel()
	if err := s.store.Ping(ctx); err != nil {
		writeError(w, http.StatusServiceUnavailable, "not_ready")
		return
	}
	if err := s.validateGraphBinding(ctx, account); err != nil {
		// The provider response and token are deliberately excluded. Health
		// callers only need a fail-closed signal and can repair credentials
		// through the account configuration path.
		s.logger.Warn("Meta account Graph binding check failed", "account", account.Key)
		writeError(w, http.StatusServiceUnavailable, "account_unhealthy")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type graphBindingResponse struct {
	ID                       string `json:"id"`
	UserID                   string `json:"user_id"`
	InstagramBusinessAccount *struct {
		ID string `json:"id"`
	} `json:"instagram_business_account"`
}

type graphTokenDebugResponse struct {
	Data struct {
		AppID     string `json:"app_id"`
		IsValid   bool   `json:"is_valid"`
		ProfileID string `json:"profile_id"`
		Type      string `json:"type"`
		UserID    string `json:"user_id"`
	} `json:"data"`
}

type instagramLoginGraphBindingEnvelope struct {
	Data []struct {
		UserID string `json:"user_id"`
	} `json:"data"`
}

func (s *Server) validateGraphBinding(ctx context.Context, account *AccountConfig) error {
	if account.usesAppBoundMessenger() {
		if err := s.validateMessengerPageIdentity(ctx, account); err != nil {
			return err
		}
		return s.validateMessengerAppSubscription(ctx, account)
	}

	base, err := s.graphBase(account)
	if err != nil {
		return err
	}
	fields := "id"
	switch {
	case account.Channel == models.ChannelInstagram &&
		account.InstagramAPIMode == InstagramAPIModeInstagramLogin:
		fields = "user_id"
	case account.Channel == models.ChannelInstagram &&
		account.InstagramAPIMode == InstagramAPIModeFacebookLogin:
		fields = "id,instagram_business_account{id}"
	}
	endpoint := fmt.Sprintf(
		"%s/%s/me",
		strings.TrimRight(base, "/"),
		url.PathEscape(s.config.GraphAPIVersion),
	)
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return errors.New("invalid Graph health endpoint")
	}
	query := parsed.Query()
	query.Set("fields", fields)
	if account.registryManaged {
		appSecret, proofErr := s.managedAppSecret(account)
		if proofErr != nil {
			return proofErr
		}
		query.Set("appsecret_proof", metaAccessTokenProof(account.accessToken, appSecret))
	}
	parsed.RawQuery = query.Encode()

	providerRequest, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return errors.New("invalid Graph health request")
	}
	providerRequest.Header.Set("Authorization", "Bearer "+account.accessToken)
	providerRequest.Header.Set("Accept", "application/json")
	providerRequest.Header.Set("User-Agent", "ReReply-Meta-Relay/1.0")

	response, err := s.client.Do(providerRequest)
	if err != nil {
		return errors.New("graph health transport failure")
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
		_ = response.Body.Close()
	}()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return errors.New("graph rejected account health request")
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, 64<<10))
	if account.Channel == models.ChannelInstagram &&
		account.InstagramAPIMode == InstagramAPIModeInstagramLogin {
		decoder.DisallowUnknownFields()
		var envelope instagramLoginGraphBindingEnvelope
		if err := decoder.Decode(&envelope); err != nil || len(envelope.Data) != 1 {
			return errors.New("graph account binding response is invalid")
		}
		if err := rejectTrailingJSON(decoder); err != nil {
			return errors.New("graph account binding response is invalid")
		}
		if strings.TrimSpace(envelope.Data[0].UserID) != account.ExternalAccountID {
			return errors.New("instagram token is bound to a different account")
		}
		if account.registryManaged {
			return s.validateInstagramAppSubscription(ctx, account)
		}
		return nil
	}

	var binding graphBindingResponse
	if err := decoder.Decode(&binding); err != nil {
		return errors.New("graph account binding response is invalid")
	}
	if err := rejectTrailingJSON(decoder); err != nil {
		return errors.New("graph account binding response is invalid")
	}

	switch {
	case account.Channel == models.ChannelMessenger:
		if strings.TrimSpace(binding.ID) != account.ExternalAccountID {
			return errors.New("graph token is bound to a different Page")
		}
		if account.registryManaged {
			return s.validateMessengerAppSubscription(ctx, account)
		}
	case account.Channel == models.ChannelInstagram &&
		account.InstagramAPIMode == InstagramAPIModeFacebookLogin:
		if binding.InstagramBusinessAccount == nil ||
			strings.TrimSpace(binding.InstagramBusinessAccount.ID) != account.ExternalAccountID {
			return errors.New("page token is bound to a different Instagram account")
		}
	default:
		return errors.New("account Graph mode is invalid")
	}
	return nil
}

func (s *Server) validateMessengerPageIdentity(ctx context.Context, account *AccountConfig) error {
	if !account.usesAppBoundMessenger() || strings.TrimSpace(account.ExternalAccountID) == "" {
		return errors.New("app-bound Messenger account binding is invalid")
	}
	appSecret, err := s.messengerAppSecret(account)
	if err != nil {
		return err
	}
	endpoint := fmt.Sprintf(
		"%s/%s/debug_token",
		strings.TrimRight(s.facebookGraphBase, "/"),
		url.PathEscape(s.config.GraphAPIVersion),
	)
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return errors.New("invalid Graph token inspection endpoint")
	}
	query := parsed.Query()
	query.Set("input_token", account.accessToken)
	parsed.RawQuery = query.Encode()

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return errors.New("invalid Graph token inspection request")
	}
	request.Header.Set("Authorization", "Bearer "+account.PlatformAppID+"|"+appSecret)
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "ReReply-Meta-Relay/1.0")
	response, err := s.client.Do(request)
	if err != nil {
		return errors.New("graph token inspection transport failure")
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
		_ = response.Body.Close()
	}()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return errors.New("graph rejected token inspection request")
	}
	var result graphTokenDebugResponse
	decoder := json.NewDecoder(io.LimitReader(response.Body, 64<<10))
	if decoder.Decode(&result) != nil || rejectTrailingJSON(decoder) != nil {
		return errors.New("graph token inspection response is invalid")
	}
	data := result.Data
	if !data.IsValid || strings.TrimSpace(data.AppID) != account.PlatformAppID ||
		!strings.EqualFold(strings.TrimSpace(data.Type), "PAGE") {
		return errors.New("graph Page token binding is invalid")
	}
	// Meta's Debug Token contract binds an impersonated Page token through
	// profile_id. Some Page-token variants expose only user_id, so retain the
	// same fail-closed compatibility fallback used during onboarding and never
	// issue a broader generic Page /me read here.
	boundPageID := strings.TrimSpace(data.ProfileID)
	if boundPageID == "" {
		boundPageID = strings.TrimSpace(data.UserID)
	}
	if boundPageID == "" || boundPageID != strings.TrimSpace(account.ExternalAccountID) {
		return errors.New("graph token is bound to a different Page")
	}
	return nil
}

func (s *Server) messengerAppSecret(account *AccountConfig) (string, error) {
	if !account.usesAppBoundMessenger() || strings.TrimSpace(account.PlatformAppID) == "" {
		return "", errors.New("messenger platform app binding is invalid")
	}
	if account.registryManaged {
		if account.PlatformAppID != strings.TrimSpace(s.config.ManagedMessengerAppID) ||
			strings.TrimSpace(s.config.ManagedMessengerAppSecret) == "" {
			return "", errors.New("messenger platform app binding is invalid")
		}
		return s.config.ManagedMessengerAppSecret, nil
	}
	if account.MessengerAppID == "" || account.PlatformAppID != account.MessengerAppID {
		return "", errors.New("messenger platform app binding is invalid")
	}
	app, ok := s.config.staticMessengerApp(account.MessengerAppID)
	if !ok || app.AppID != account.PlatformAppID || strings.TrimSpace(app.appSecret) == "" {
		return "", errors.New("messenger platform app binding is invalid")
	}
	return app.appSecret, nil
}

func (s *Server) managedAppSecret(account *AccountConfig) (string, error) {
	if account == nil || !account.registryManaged {
		return "", errors.New("managed Meta account binding is invalid")
	}
	switch account.Channel {
	case models.ChannelMessenger:
		if strings.TrimSpace(account.PlatformAppID) == "" ||
			account.PlatformAppID != strings.TrimSpace(s.config.ManagedMessengerAppID) ||
			strings.TrimSpace(s.config.ManagedMessengerAppSecret) == "" {
			return "", errors.New("messenger platform app binding is invalid")
		}
		return s.config.ManagedMessengerAppSecret, nil
	case models.ChannelInstagram:
		if account.InstagramAPIMode != InstagramAPIModeInstagramLogin ||
			strings.TrimSpace(account.PlatformAppID) == "" ||
			account.PlatformAppID != strings.TrimSpace(s.config.ManagedInstagramAppID) ||
			strings.TrimSpace(s.config.ManagedInstagramAppSecret) == "" {
			return "", errors.New("instagram platform app binding is invalid")
		}
		return s.config.ManagedInstagramAppSecret, nil
	default:
		return "", errors.New("managed Meta account binding is invalid")
	}
}

func (s *Server) validateMessengerAppSubscription(ctx context.Context, account *AccountConfig) error {
	if !account.usesAppBoundMessenger() || strings.TrimSpace(account.PlatformAppID) == "" {
		return errors.New("messenger platform app binding is invalid")
	}
	appSecret, err := s.messengerAppSecret(account)
	if err != nil {
		return err
	}
	endpoint := fmt.Sprintf(
		"%s/%s/%s/subscribed_apps",
		strings.TrimRight(s.facebookGraphBase, "/"),
		url.PathEscape(s.config.GraphAPIVersion),
		url.PathEscape(account.ExternalAccountID),
	)
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return errors.New("invalid Graph subscription health endpoint")
	}
	query := parsed.Query()
	query.Set("fields", "id,subscribed_fields")
	query.Set("appsecret_proof", metaAccessTokenProof(account.accessToken, appSecret))
	parsed.RawQuery = query.Encode()

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return errors.New("invalid Graph subscription health request")
	}
	request.Header.Set("Authorization", "Bearer "+account.accessToken)
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "ReReply-Meta-Relay/1.0")
	response, err := s.client.Do(request)
	if err != nil {
		return errors.New("graph subscription health transport failure")
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
		_ = response.Body.Close()
	}()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return errors.New("graph rejected subscription health request")
	}
	var result struct {
		Data []struct {
			ID               string   `json:"id"`
			SubscribedFields []string `json:"subscribed_fields"`
		} `json:"data"`
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, 64<<10))
	if decoder.Decode(&result) != nil || rejectTrailingJSON(decoder) != nil {
		return errors.New("graph subscription health response is invalid")
	}
	for _, subscription := range result.Data {
		if strings.TrimSpace(subscription.ID) != account.PlatformAppID {
			continue
		}
		for _, field := range subscription.SubscribedFields {
			if strings.EqualFold(strings.TrimSpace(field), "messages") {
				return nil
			}
		}
	}
	return errors.New("messenger platform app is not subscribed to messages")
}

func (s *Server) validateInstagramAppSubscription(ctx context.Context, account *AccountConfig) error {
	appSecret, err := s.managedAppSecret(account)
	if err != nil {
		return err
	}
	endpoint := fmt.Sprintf(
		"%s/%s/%s/subscribed_apps",
		strings.TrimRight(s.instagramGraphBase, "/"),
		url.PathEscape(s.config.GraphAPIVersion),
		url.PathEscape(account.ExternalAccountID),
	)
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return errors.New("invalid Instagram subscription health endpoint")
	}
	query := parsed.Query()
	query.Set("fields", "id,subscribed_fields")
	query.Set("appsecret_proof", metaAccessTokenProof(account.accessToken, appSecret))
	parsed.RawQuery = query.Encode()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return errors.New("invalid Instagram subscription health request")
	}
	request.Header.Set("Authorization", "Bearer "+account.accessToken)
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "ReReply-Meta-Relay/1.0")
	response, err := s.client.Do(request)
	if err != nil {
		return errors.New("instagram subscription health transport failure")
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
		_ = response.Body.Close()
	}()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return errors.New("instagram rejected subscription health request")
	}
	var result struct {
		Data []struct {
			ID               string   `json:"id"`
			SubscribedFields []string `json:"subscribed_fields"`
		} `json:"data"`
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, 64<<10))
	if decoder.Decode(&result) != nil || rejectTrailingJSON(decoder) != nil {
		return errors.New("instagram subscription health response is invalid")
	}
	for _, subscription := range result.Data {
		if strings.TrimSpace(subscription.ID) != account.PlatformAppID {
			continue
		}
		for _, field := range subscription.SubscribedFields {
			if strings.EqualFold(strings.TrimSpace(field), "messages") {
				return nil
			}
		}
	}
	return errors.New("instagram platform app is not subscribed to messages")
}

func metaAccessTokenProof(accessToken, appSecret string) string {
	mac := hmac.New(sha256.New, []byte(appSecret))
	_, _ = mac.Write([]byte(accessToken))
	return hex.EncodeToString(mac.Sum(nil))
}

func (s *Server) handleReReplyOutbound(w http.ResponseWriter, request *http.Request) {
	account, err := s.accountFromPath(request, metaregistry.ResolvePurposeOutbound, false)
	if err != nil {
		if errors.Is(err, ErrRegistryStale) {
			writeError(w, http.StatusServiceUnavailable, "registry_binding_stale")
			return
		}
		if errors.Is(err, ErrRegistryUnavailable) {
			writeError(w, http.StatusServiceUnavailable, "registry_unavailable")
			return
		}
		if errors.Is(err, ErrRegistryUnauthorized) {
			writeError(w, http.StatusUnauthorized, "service_authentication_required")
			return
		}
		writeError(w, http.StatusNotFound, "unknown_account")
		return
	}
	raw, err := readLimitedBody(w, request, defaultOutboundBodyLimit)
	if err != nil {
		writeError(w, bodyReadStatus(err), "invalid_request_body")
		return
	}
	if !verifySignedBody(account.reReplyOutboundSecret, request.Header.Get(ReReplySignatureHeader), raw) {
		writeError(w, http.StatusUnauthorized, "invalid_signature")
		return
	}

	envelope, message, err := decodeOutbound(raw)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_relay_envelope")
		return
	}
	if envelope.Type != "message" ||
		envelope.Channel != account.Channel ||
		envelope.ExternalAccountID != account.ExternalAccountID {
		writeError(w, http.StatusUnprocessableEntity, "unsupported_or_mismatched_delivery")
		return
	}
	if err := validateOutboundMessage(account.Channel, message); err != nil {
		writeError(w, http.StatusUnprocessableEntity, "unsupported_message")
		return
	}

	digest := digestHex(raw)
	idempotencyKey := account.Key + "\n" + message.IdempotencyKey
	claimCtx, cancelClaim := s.outboundSettlementContext(request.Context())
	claim, err := s.store.ClaimOutbound(
		claimCtx,
		idempotencyKey,
		digest,
		s.now().UTC(),
	)
	cancelClaim()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "idempotency_store_unavailable")
		return
	}
	switch claim.State {
	case OutboundClaimCompleted, OutboundClaimRejected:
		writeRawJSON(w, claim.Status, claim.Result)
		return
	case OutboundClaimCollision:
		writeError(w, http.StatusConflict, "idempotency_key_collision")
		return
	case OutboundClaimInFlight:
		w.Header().Set("Retry-After", "5")
		writeError(w, http.StatusServiceUnavailable, "delivery_in_flight")
		return
	case OutboundClaimAmbiguous:
		writeError(w, http.StatusConflict, "delivery_outcome_ambiguous")
		return
	case OutboundClaimAcquired:
		// Continue to the only provider side effect.
	default:
		writeError(w, http.StatusServiceUnavailable, "idempotency_store_unavailable")
		return
	}

	// Once the request is authenticated and its idempotency claim is durable,
	// a peer disconnect must not abort the provider call or the Redis
	// settlement. Explicit bounds keep the handler finite, and http.Server
	// Shutdown still waits for the active handler within its shutdown budget.
	deliveryCtx, cancelDelivery := context.WithTimeout(
		context.WithoutCancel(request.Context()),
		s.outboundTimeout,
	)
	result := s.sendGraph(deliveryCtx, account, message)
	cancelDelivery()
	if result.ambiguous {
		if err := s.markOutboundAmbiguous(
			request.Context(),
			idempotencyKey,
			digest,
		); err != nil {
			s.logger.Error(
				"Meta outbound ambiguity could not be persisted",
				"component",
				"outbound_settlement",
				"account",
				account.Key,
			)
		}
		writeError(w, http.StatusConflict, "delivery_outcome_ambiguous")
		return
	}
	if result.retryable {
		settleCtx, cancelSettle := s.outboundSettlementContext(request.Context())
		err := s.store.ReleaseOutbound(settleCtx, idempotencyKey, digest)
		cancelSettle()
		if err != nil {
			writeError(w, http.StatusServiceUnavailable, "idempotency_store_unavailable")
			return
		}
		if result.retryAfter > 0 {
			w.Header().Set("Retry-After", strconv.Itoa(int(result.retryAfter.Round(time.Second).Seconds())))
		}
		writeRawJSON(w, result.status, result.body)
		return
	}

	rejected := result.status < http.StatusOK || result.status >= http.StatusMultipleChoices
	settleCtx, cancelSettle := s.outboundSettlementContext(request.Context())
	err = s.store.CompleteOutbound(
		settleCtx,
		idempotencyKey,
		digest,
		result.status,
		result.body,
		rejected,
	)
	cancelSettle()
	if err != nil {
		// Graph may already have accepted the message. Never retry blindly when
		// the completion record could not be made durable.
		if ambiguousErr := s.markOutboundAmbiguous(
			request.Context(),
			idempotencyKey,
			digest,
		); ambiguousErr != nil {
			s.logger.Error(
				"Meta outbound completion and ambiguity settlement both failed",
				"component",
				"outbound_settlement",
				"account",
				account.Key,
			)
		}
		writeError(w, http.StatusConflict, "delivery_outcome_ambiguous")
		return
	}
	writeRawJSON(w, result.status, result.body)
}

func (s *Server) outboundSettlementContext(parent context.Context) (context.Context, context.CancelFunc) {
	if parent == nil {
		parent = context.Background()
	}
	return context.WithTimeout(context.WithoutCancel(parent), s.settlementTimeout)
}

func (s *Server) markOutboundAmbiguous(
	parent context.Context,
	key, digest string,
) error {
	settleCtx, cancelSettle := s.outboundSettlementContext(parent)
	defer cancelSettle()
	return s.store.MarkOutboundAmbiguous(settleCtx, key, digest)
}

func (s *Server) accountFromPath(request *http.Request, purpose string, allowCache bool) (*AccountConfig, error) {
	channel := models.Channel(strings.ToLower(strings.TrimSpace(request.PathValue("channel"))))
	externalID := strings.TrimSpace(request.PathValue("externalID"))
	if account, ok := s.config.account(channel, externalID); ok {
		return account, nil
	}
	if s.registry == nil {
		return nil, ErrRegistryNotFound
	}
	if len(s.config.RegistryEdgeSecret) < 32 || !constantTimeEqual(
		request.Header.Get(RelayServiceTokenHeader),
		s.config.RegistryEdgeSecret,
	) {
		return nil, ErrRegistryUnauthorized
	}
	return s.registry.Resolve(request.Context(), channel, externalID, purpose, allowCache)
}

type graphResult struct {
	status     int
	body       []byte
	retryable  bool
	ambiguous  bool
	retryAfter time.Duration
}

func (s *Server) sendGraph(
	ctx context.Context,
	account *AccountConfig,
	message channelapi.OutboundMessage,
) graphResult {
	payload := map[string]any{
		"recipient": map[string]string{"id": message.Recipient.ExternalID},
		"message":   map[string]any{"text": message.Parts[0].Text},
	}
	if message.ReplyToExternalID != "" {
		payload["message"].(map[string]any)["reply_to"] = map[string]string{
			"mid": message.ReplyToExternalID,
		}
	}
	base, err := s.graphBase(account)
	if err != nil {
		return graphResult{
			status: http.StatusInternalServerError,
			body:   errorJSON("provider_request_failed"),
		}
	}
	if account.Channel == models.ChannelMessenger {
		payload["messaging_type"] = "RESPONSE"
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return graphResult{
			status:    http.StatusInternalServerError,
			body:      errorJSON("provider_request_failed"),
			ambiguous: false,
		}
	}
	endpoint := fmt.Sprintf(
		"%s/%s/%s/messages",
		strings.TrimRight(base, "/"),
		url.PathEscape(s.config.GraphAPIVersion),
		url.PathEscape(account.ExternalAccountID),
	)
	if account.registryManaged || account.usesAppBoundMessenger() {
		parsed, parseErr := url.Parse(endpoint)
		if parseErr != nil {
			return graphResult{status: http.StatusInternalServerError, body: errorJSON("provider_request_failed")}
		}
		var appSecret string
		var proofErr error
		if account.Channel == models.ChannelMessenger {
			appSecret, proofErr = s.messengerAppSecret(account)
		} else {
			appSecret, proofErr = s.managedAppSecret(account)
		}
		if proofErr != nil {
			return graphResult{status: http.StatusInternalServerError, body: errorJSON("provider_request_failed")}
		}
		query := parsed.Query()
		query.Set("appsecret_proof", metaAccessTokenProof(account.accessToken, appSecret))
		parsed.RawQuery = query.Encode()
		endpoint = parsed.String()
	}
	providerRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(raw))
	if err != nil {
		return graphResult{
			status: http.StatusInternalServerError,
			body:   errorJSON("provider_request_failed"),
		}
	}
	providerRequest.Header.Set("Authorization", "Bearer "+account.accessToken)
	providerRequest.Header.Set("Content-Type", "application/json")
	providerRequest.Header.Set("User-Agent", "ReReply-Meta-Relay/1.0")

	response, err := s.client.Do(providerRequest)
	if err != nil {
		// A transport error can occur after Meta accepted the request. Marking
		// this ambiguous prevents an automatic duplicate send.
		return graphResult{
			status:    http.StatusConflict,
			body:      errorJSON("delivery_outcome_ambiguous"),
			ambiguous: true,
		}
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
		_ = response.Body.Close()
	}()

	if response.StatusCode >= http.StatusOK && response.StatusCode < http.StatusMultipleChoices {
		var graphResponse graphSendResponse
		decoder := json.NewDecoder(io.LimitReader(response.Body, 64<<10))
		if err := decoder.Decode(&graphResponse); err != nil ||
			rejectTrailingJSON(decoder) != nil ||
			strings.TrimSpace(graphResponse.MessageID) == "" {
			return graphResult{
				status:    http.StatusConflict,
				body:      errorJSON("delivery_outcome_ambiguous"),
				ambiguous: true,
			}
		}
		safeResponse, err := json.Marshal(relaySendResponse{
			ProviderMessageIDs:     []string{graphResponse.MessageID},
			ExternalConversationID: message.Recipient.ExternalID,
			ProviderRequestID:      safeProviderRequestID(response.Header),
		})
		if err != nil {
			return graphResult{
				status:    http.StatusConflict,
				body:      errorJSON("delivery_outcome_ambiguous"),
				ambiguous: true,
			}
		}
		return graphResult{status: http.StatusOK, body: safeResponse}
	}

	if response.StatusCode >= http.StatusInternalServerError {
		// A provider 5xx can be generated after the message side effect has
		// committed. Persist ambiguity so the same idempotency key can never
		// trigger a blind duplicate send.
		return graphResult{
			status:    http.StatusConflict,
			body:      errorJSON("delivery_outcome_ambiguous"),
			ambiguous: true,
		}
	}
	if response.StatusCode == http.StatusTooManyRequests {
		return graphResult{
			status:     response.StatusCode,
			body:       errorJSONWithStatus("graph_retryable", response.StatusCode),
			retryable:  true,
			retryAfter: parseRetryAfter(response.Header.Get("Retry-After"), s.now().UTC()),
		}
	}
	return graphResult{
		status: response.StatusCode,
		body:   errorJSONWithStatus("graph_rejected", response.StatusCode),
	}
}

func (s *Server) graphBase(account *AccountConfig) (string, error) {
	if account == nil {
		return "", errors.New("account is required")
	}
	switch account.Channel {
	case models.ChannelMessenger:
		if account.InstagramAPIMode != "" {
			return "", errors.New("messenger account has an Instagram API mode")
		}
		return s.facebookGraphBase, nil
	case models.ChannelInstagram:
		switch account.InstagramAPIMode {
		case InstagramAPIModeInstagramLogin:
			return s.instagramGraphBase, nil
		case InstagramAPIModeFacebookLogin:
			return s.facebookGraphBase, nil
		default:
			return "", errors.New("instagram account API mode is invalid")
		}
	default:
		return "", errors.New("account channel is invalid")
	}
}

func decodeOutbound(raw []byte) (relayOutboundEnvelope, channelapi.OutboundMessage, error) {
	var envelope relayOutboundEnvelope
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&envelope); err != nil {
		return envelope, channelapi.OutboundMessage{}, err
	}
	if err := rejectTrailingJSON(decoder); err != nil {
		return envelope, channelapi.OutboundMessage{}, err
	}
	var message channelapi.OutboundMessage
	if len(envelope.Data) == 0 || string(envelope.Data) == "null" {
		return envelope, message, errors.New("relay data is required")
	}
	if err := json.Unmarshal(envelope.Data, &message); err != nil {
		return envelope, message, err
	}
	return envelope, message, nil
}

func validateOutboundMessage(channel models.Channel, message channelapi.OutboundMessage) error {
	if strings.TrimSpace(message.IdempotencyKey) == "" || len(message.IdempotencyKey) > 255 {
		return errors.New("idempotency_key is required")
	}
	if strings.TrimSpace(message.Recipient.ExternalID) == "" || len(message.Recipient.ExternalID) > 255 {
		return errors.New("recipient external_id is required")
	}
	if len(message.Parts) != 1 ||
		message.Parts[0].Type != models.MessagePartTypeText ||
		strings.TrimSpace(message.Parts[0].Text) == "" {
		return errors.New("exactly one text part is required")
	}
	text := message.Parts[0].Text
	if !utf8.ValidString(text) {
		return errors.New("text is not valid UTF-8")
	}
	tooLong := utf8.RuneCountInString(text) > 2000
	if channel == models.ChannelInstagram {
		tooLong = len(text) > 1000
	}
	if tooLong {
		return errors.New("text exceeds provider limit")
	}
	if message.Template != nil || len(message.CC) > 0 || len(message.ReplyToExternalID) > 512 {
		return errors.New("unsupported Phase 1 message feature")
	}
	return nil
}

func readLimitedBody(w http.ResponseWriter, request *http.Request, limit int64) ([]byte, error) {
	request.Body = http.MaxBytesReader(w, request.Body, limit)
	raw, err := io.ReadAll(request.Body)
	if err != nil {
		return nil, err
	}
	return raw, nil
}

func bodyReadStatus(err error) int {
	var maxBytesError *http.MaxBytesError
	if errors.As(err, &maxBytesError) {
		return http.StatusRequestEntityTooLarge
	}
	return http.StatusBadRequest
}

func signBody(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(body)
	return signaturePrefix + hex.EncodeToString(mac.Sum(nil))
}

func verifySignedBody(secret, signature string, body []byte) bool {
	if secret == "" || len(signature) != len(signaturePrefix)+sha256.Size*2 ||
		!strings.HasPrefix(signature, signaturePrefix) {
		return false
	}
	digest := strings.TrimPrefix(signature, signaturePrefix)
	provided, err := hex.DecodeString(digest)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(body)
	return hmac.Equal(mac.Sum(nil), provided)
}

func constantTimeEqual(left, right string) bool {
	return hmac.Equal([]byte(left), []byte(right))
}

func parseRetryAfter(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(value); err == nil && seconds > 0 {
		return min(time.Duration(seconds)*time.Second, time.Hour)
	}
	if parsed, err := http.ParseTime(value); err == nil && parsed.After(now) {
		return min(parsed.Sub(now), time.Hour)
	}
	return 0
}

func safeProviderRequestID(header http.Header) string {
	value := strings.TrimSpace(header.Get("X-FB-Request-ID"))
	if value == "" {
		value = strings.TrimSpace(header.Get("X-FB-Trace-ID"))
	}
	if len(value) > 255 {
		return ""
	}
	for _, char := range value {
		if char < 0x21 || char > 0x7e {
			return ""
		}
	}
	return value
}

func rejectNonCanonicalPath(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL == nil || request.URL.Opaque != "" || request.URL.RawPath != "" ||
			request.URL.Path == "" || request.URL.EscapedPath() != request.URL.Path ||
			strings.Contains(request.URL.Path, "//") ||
			pathpkg.Clean(request.URL.Path) != request.URL.Path {
			writeError(w, http.StatusNotFound, "not_found")
			return
		}
		next.ServeHTTP(w, request)
	})
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		next.ServeHTTP(w, request)
	})
}

func writeError(w http.ResponseWriter, status int, code string) {
	writeRawJSON(w, status, errorJSON(code))
}

func errorJSON(code string) []byte {
	body, _ := json.Marshal(map[string]string{"error": code})
	return body
}

func errorJSONWithStatus(code string, status int) []byte {
	body, _ := json.Marshal(struct {
		Error  string `json:"error"`
		Status int    `json:"status"`
	}{Error: code, Status: status})
	return body
}

func writeRawJSON(w http.ResponseWriter, status int, body []byte) {
	if status < 100 || status > 599 {
		status = http.StatusInternalServerError
		body = errorJSON("invalid_stored_response")
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if len(body) > 0 {
		_, _ = w.Write(body)
	}
}
