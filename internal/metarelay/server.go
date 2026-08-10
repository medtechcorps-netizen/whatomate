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
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	channelapi "github.com/shridarpatil/whatomate/internal/channel"
	"github.com/shridarpatil/whatomate/internal/models"
)

const (
	defaultFacebookGraphBase  = "https://graph.facebook.com"
	defaultInstagramGraphBase = "https://graph.instagram.com"
	accountHealthTimeout      = 5 * time.Second
	reviewReadinessTimeout    = 2 * time.Second
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

func WithServerReviewBindingResolver(resolver ReviewBindingResolver) ServerOption {
	return func(server *Server) {
		if resolver != nil {
			server.reviewResolver = resolver
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
	if server.outboundTimeout <= 0 || server.settlementTimeout <= 0 {
		return nil, errors.New("outbound relay timeouts must be positive")
	}
	if config.stagingMessengerReview() {
		if config.DeploymentEnvironment != "staging" {
			return nil, errors.New("Messenger review relay is staging-only")
		}
		if server.reviewResolver == nil {
			return nil, errors.New("Messenger review binding resolver is required")
		}
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
	if s.config.stagingMessengerReview() {
		// Keep /readyz as service-level readiness so a first review deployment
		// can become healthy before onboarding creates the broker binding.
		// Operators and review automation use /reviewz for the exact dynamic
		// binding attestation.
		mux.HandleFunc("GET /reviewz", s.handleReviewReady)
		return securityHeaders(mux)
	}
	mux.HandleFunc(
		"GET /v1/meta/instagram/webhook",
		s.metaVerificationHandler(WebhookAppInstagramLogin),
	)
	mux.HandleFunc(
		"POST /v1/meta/instagram/webhook",
		s.metaWebhookHandler(WebhookAppInstagramLogin),
	)
	mux.HandleFunc("HEAD /v1/accounts/{channel}/{externalID}", s.handleAccountHealth)
	mux.HandleFunc("POST /v1/accounts/{channel}/{externalID}", s.handleReReplyOutbound)
	return securityHeaders(mux)
}

func (s *Server) handleReviewReady(w http.ResponseWriter, request *http.Request) {
	if !s.config.stagingMessengerReview() || request.Method != http.MethodGet {
		writeError(w, http.StatusNotFound, "unknown_route")
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), reviewReadinessTimeout)
	defer cancel()

	if err := s.store.Ping(ctx); err != nil {
		writeError(w, http.StatusServiceUnavailable, "not_ready")
		return
	}
	binding, err := resolveFreshReviewBinding(ctx, s.reviewResolver)
	if err != nil || validateReviewBinding(s.config, binding, s.now().UTC()) != nil {
		writeError(w, http.StatusServiceUnavailable, "not_ready")
		return
	}
	if _, err := binding.account(); err != nil {
		writeError(w, http.StatusServiceUnavailable, "not_ready")
		return
	}
	keyID, err := channelapi.MetaProviderProofKeyID(
		s.config.ReReplyProviderProofSecret,
	)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "not_ready")
		return
	}
	w.Header().Set(channelapi.RelayMetaProviderProofKeyIDHeader, keyID)
	w.WriteHeader(http.StatusNoContent)
}

type freshReviewBindingResolver interface {
	resolveReviewBindingFresh(ctx context.Context) (ReviewBinding, error)
}

func resolveFreshReviewBinding(
	ctx context.Context,
	resolver ReviewBindingResolver,
) (ReviewBinding, error) {
	if fresh, ok := resolver.(freshReviewBindingResolver); ok {
		return fresh.resolveReviewBindingFresh(ctx)
	}
	return resolver.ResolveReviewBinding(ctx)
}

func (s *Server) handleLive(w http.ResponseWriter, request *http.Request) {
	if s.config.stagingMessengerReview() && request.Method != http.MethodGet {
		writeError(w, http.StatusNotFound, "unknown_route")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleReady(w http.ResponseWriter, request *http.Request) {
	if s.config.stagingMessengerReview() && request.Method != http.MethodGet {
		writeError(w, http.StatusNotFound, "unknown_route")
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 2*time.Second)
	defer cancel()
	if err := s.store.Ping(ctx); err != nil {
		writeError(w, http.StatusServiceUnavailable, "not_ready")
		return
	}
	keyID, err := channelapi.MetaProviderProofKeyID(
		s.config.ReReplyProviderProofSecret,
	)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "not_ready")
		return
	}
	w.Header().Set(channelapi.RelayMetaProviderProofKeyIDHeader, keyID)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) metaVerificationHandler(webhookApp WebhookApp) http.HandlerFunc {
	return func(w http.ResponseWriter, request *http.Request) {
		if s.config.stagingMessengerReview() && request.Method != http.MethodGet {
			writeError(w, http.StatusNotFound, "unknown_webhook")
			return
		}
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
	if query.Get("hub.mode") != "subscribe" ||
		!constantTimeEqual(query.Get("hub.verify_token"), verifyToken) ||
		query.Get("hub.challenge") == "" {
		writeError(w, http.StatusForbidden, "verification_failed")
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, query.Get("hub.challenge"))
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
	if !verifySignedBody(appSecret, request.Header.Get(MetaSignatureHeader), raw) {
		writeError(w, http.StatusUnauthorized, "invalid_signature")
		return
	}
	if s.config.stagingMessengerReview() {
		s.handleReviewMetaWebhook(w, request, webhookApp, raw)
		return
	}
	jobs, err := NormalizeInbound(s.config, webhookApp, raw)
	if err != nil {
		switch {
		case errors.Is(err, ErrUnknownAccount), errors.Is(err, ErrWebhookAppMismatch):
			writeError(w, http.StatusNotFound, "unknown_account")
		case errors.Is(err, ErrUnsupportedObject):
			writeError(w, http.StatusUnprocessableEntity, "unsupported_object")
		case errors.Is(err, ErrCanonicalJobTooLarge):
			writeError(w, http.StatusRequestEntityTooLarge, "canonical_payload_too_large")
		default:
			writeError(w, http.StatusBadRequest, "invalid_payload")
		}
		return
	}

	// Meta requires acknowledgement within five seconds. We reserve one second
	// for parsing/network overhead and only acknowledge after this atomic Redis
	// operation has durably stored all canonical jobs (or the no-op marker).
	ctx, cancel := context.WithTimeout(request.Context(), 4*time.Second)
	defer cancel()
	acceptanceID := digestHex(raw)
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
	case WebhookAppInstagramLogin:
		if s.config.stagingMessengerReview() {
			return "", "", false
		}
		return s.config.InstagramLoginAppSecret, s.config.InstagramLoginVerifyToken, true
	default:
		return "", "", false
	}
}

func (s *Server) handleAccountHealth(w http.ResponseWriter, request *http.Request) {
	if s.config.stagingMessengerReview() {
		writeError(w, http.StatusNotFound, "review_mode_inbound_only")
		return
	}
	// A successful response contains account-specific readiness attestations.
	// Never permit an intermediary to replay them after a token, subscription,
	// or protected mapping has been revoked.
	w.Header().Set("Cache-Control", "no-store, private")
	account, ok := s.accountFromPath(request)
	if !ok {
		writeError(w, http.StatusNotFound, "unknown_account")
		return
	}
	if request.ContentLength != 0 ||
		len(request.TransferEncoding) != 0 ||
		!verifySignedBody(account.reReplyOutboundSecret, request.Header.Get(ReReplySignatureHeader), nil) {
		writeError(w, http.StatusUnauthorized, "invalid_signature")
		return
	}
	if err := s.config.validateCurrentGovernance(account, s.now().UTC()); err != nil {
		s.logger.Warn("Meta app governance review is no longer current", "account", account.Key)
		writeError(w, http.StatusServiceUnavailable, "governance_review_stale")
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
	if err := s.validateWebhookSubscription(ctx, account); err != nil {
		s.logger.Warn("Meta account webhook subscription check failed", "account", account.Key)
		writeError(w, http.StatusServiceUnavailable, "account_unhealthy")
		return
	}
	keyID, err := channelapi.MetaProviderProofKeyID(
		s.config.ReReplyProviderProofSecret,
	)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "not_ready")
		return
	}
	w.Header().Set(channelapi.RelayReadinessHeader, channelapi.RelayReadinessVersion)
	w.Header().Set(channelapi.RelayChannelHeader, string(account.Channel))
	w.Header().Set(channelapi.RelayExternalAccountHeader, account.ExternalAccountID)
	w.Header().Set(channelapi.RelayChannelAccountHeader, account.reReplyChannelAccountID)
	w.Header().Set(channelapi.RelayOrganizationHeader, account.OrganizationID)
	w.Header().Set(channelapi.RelayMetaBusinessHeader, account.MetaBusinessID)
	w.Header().Set(channelapi.RelayMetaProviderProofKeyIDHeader, keyID)
	w.Header().Set(
		channelapi.RelayMetaProviderProofHeader,
		channelapi.SignMetaProviderReadinessProof(
			s.config.ReReplyProviderProofSecret,
			account.Channel,
			account.ExternalAccountID,
			account.reReplyChannelAccountID,
			account.OrganizationID,
			account.MetaBusinessID,
		),
	)
	w.WriteHeader(http.StatusNoContent)
}

type graphBindingResponse struct {
	ID                       string `json:"id"`
	UserID                   string `json:"user_id"`
	InstagramBusinessAccount *struct {
		ID string `json:"id"`
	} `json:"instagram_business_account"`
}

func (s *Server) validateGraphBinding(ctx context.Context, account *AccountConfig) error {
	if s.config.stagingMessengerReview() {
		return errors.New("Messenger review mode is inbound-only")
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
	var binding graphBindingResponse
	decoder := json.NewDecoder(io.LimitReader(response.Body, 64<<10))
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
	case account.Channel == models.ChannelInstagram &&
		account.InstagramAPIMode == InstagramAPIModeInstagramLogin:
		if strings.TrimSpace(binding.UserID) != account.ExternalAccountID {
			return errors.New("instagram token is bound to a different account")
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

type graphSubscriptionsResponse struct {
	Data []struct {
		ID               string   `json:"id"`
		SubscribedFields []string `json:"subscribed_fields"`
	} `json:"data"`
}

// validateWebhookSubscription proves that the exact app configured for the
// account's webhook route is currently installed on the exact Page or
// Instagram professional account and subscribed to the messages field. App
// callback verification alone is not enough: Meta also requires this
// per-asset subscription before customer messages are delivered.
func (s *Server) validateWebhookSubscription(ctx context.Context, account *AccountConfig) error {
	if s.config.stagingMessengerReview() {
		return errors.New("Messenger review mode is inbound-only")
	}
	base, err := s.graphBase(account)
	if err != nil {
		return err
	}
	endpoint := fmt.Sprintf(
		"%s/%s/%s/subscribed_apps",
		strings.TrimRight(base, "/"),
		url.PathEscape(s.config.GraphAPIVersion),
		url.PathEscape(account.ExternalAccountID),
	)
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return errors.New("invalid Graph subscription endpoint")
	}
	query := parsed.Query()
	query.Set("fields", "id,subscribed_fields")
	parsed.RawQuery = query.Encode()

	providerRequest, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return errors.New("invalid Graph subscription request")
	}
	providerRequest.Header.Set("Authorization", "Bearer "+account.accessToken)
	providerRequest.Header.Set("Accept", "application/json")
	providerRequest.Header.Set("User-Agent", "ReReply-Meta-Relay/1.0")

	response, err := s.client.Do(providerRequest)
	if err != nil {
		return errors.New("graph subscription transport failure")
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
		_ = response.Body.Close()
	}()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return errors.New("graph rejected subscription health request")
	}
	var subscriptions graphSubscriptionsResponse
	decoder := json.NewDecoder(io.LimitReader(response.Body, 64<<10))
	if err := decoder.Decode(&subscriptions); err != nil || rejectTrailingJSON(decoder) != nil {
		return errors.New("graph subscription response is invalid")
	}
	expectedAppID := strings.TrimSpace(s.config.appID(account.webhookApp()))
	for _, subscription := range subscriptions.Data {
		if strings.TrimSpace(subscription.ID) != expectedAppID {
			continue
		}
		for _, field := range subscription.SubscribedFields {
			if strings.EqualFold(strings.TrimSpace(field), "messages") {
				return nil
			}
		}
	}
	return errors.New("required messages webhook subscription is missing")
}

func (s *Server) handleReReplyOutbound(w http.ResponseWriter, request *http.Request) {
	if s.config.stagingMessengerReview() {
		writeError(w, http.StatusNotFound, "review_mode_inbound_only")
		return
	}
	account, ok := s.accountFromPath(request)
	if !ok {
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
	if !constantTimeEqual(
		strings.TrimSpace(request.Header.Get(channelapi.RelayMetaProviderProofHeader)),
		channelapi.SignMetaProviderOutboundProof(s.config.ReReplyProviderProofSecret, raw),
	) {
		writeError(w, http.StatusUnauthorized, "invalid_provider_proof")
		return
	}
	if err := s.config.validateCurrentGovernance(account, s.now().UTC()); err != nil {
		s.logger.Warn("Blocked Meta outbound after governance review expired", "account", account.Key)
		writeError(w, http.StatusServiceUnavailable, "governance_review_stale")
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

func (s *Server) accountFromPath(request *http.Request) (*AccountConfig, bool) {
	channel := models.Channel(strings.ToLower(strings.TrimSpace(request.PathValue("channel"))))
	externalID := strings.TrimSpace(request.PathValue("externalID"))
	return s.config.account(channel, externalID)
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
	if s.config.stagingMessengerReview() {
		return graphResult{
			status: http.StatusNotFound,
			body:   errorJSON("review_mode_inbound_only"),
		}
	}
	if err := s.config.validateCurrentGovernance(account, s.now().UTC()); err != nil {
		return graphResult{
			status:    http.StatusServiceUnavailable,
			body:      errorJSON("governance_review_stale"),
			retryable: true,
		}
	}
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
	maxRunes := 2000
	if channel == models.ChannelInstagram {
		maxRunes = 1000
	}
	if utf8.RuneCountInString(message.Parts[0].Text) > maxRunes {
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
	if secret == "" || !strings.HasPrefix(signature, signaturePrefix) {
		return false
	}
	provided, err := hex.DecodeString(strings.TrimPrefix(signature, signaturePrefix))
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
