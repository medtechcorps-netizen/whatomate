package gmailrelay

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	channelapi "github.com/shridarpatil/whatomate/internal/channel"
	"github.com/shridarpatil/whatomate/internal/metarelay"
	"github.com/shridarpatil/whatomate/internal/models"
)

const (
	maxOutboundBodyBytes = int64(1 << 20)
	settlementTimeout    = 5 * time.Second
)

type OAuthFlow interface {
	Begin(context.Context) (OAuthStart, error)
	CompleteCallback(context.Context, OAuthCallback) (OAuthResult, error)
}

type ServerStateStore interface {
	Ping(context.Context) error
	GetLastSuccessfulSync(context.Context) (time.Time, error)
}

type ServerGmailAPI interface {
	GmailOutboundAPI
	Profile(context.Context) (GmailProfile, error)
}

type ServerOutboundStore interface {
	metarelay.OutboundStore
	ResolveOutboundAmbiguous(context.Context, string, string, int, []byte) error
}

type ServerOption func(*Server)

func WithServerLogger(logger *slog.Logger) ServerOption {
	return func(server *Server) {
		if logger != nil {
			server.logger = logger
		}
	}
}

type Server struct {
	config   *Config
	state    ServerStateStore
	oauth    OAuthFlow
	gmail    ServerGmailAPI
	outbound ServerOutboundStore
	logger   *slog.Logger
	now      func() time.Time
}

func NewServer(
	config *Config,
	state ServerStateStore,
	oauth OAuthFlow,
	gmail ServerGmailAPI,
	outbound ServerOutboundStore,
	options ...ServerOption,
) (*Server, error) {
	if config == nil || state == nil || oauth == nil || gmail == nil || outbound == nil {
		return nil, errors.New("gmail relay server configuration is incomplete")
	}
	server := &Server{
		config:   config,
		state:    state,
		oauth:    oauth,
		gmail:    gmail,
		outbound: outbound,
		logger:   slog.New(slog.NewTextHandler(discardWriter{}, nil)),
		now:      func() time.Time { return time.Now().UTC() },
	}
	for _, option := range options {
		option(server)
	}
	return server, nil
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /livez", s.handleLive)
	mux.HandleFunc("GET /readyz", s.handleReady)
	mux.HandleFunc("POST /oauth/google/start", s.handleOAuthStart)
	mux.HandleFunc("GET /oauth/google/callback", s.handleOAuthCallback)
	mux.HandleFunc("HEAD /v1/accounts/email/{externalID}", s.handleAccountHealth)
	mux.HandleFunc("POST /v1/accounts/email/{externalID}", s.handleOutbound)
	return gmailSecurityHeaders(mux)
}

func (s *Server) handleLive(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleReady(w http.ResponseWriter, request *http.Request) {
	ctx, cancel := context.WithTimeout(request.Context(), 2*time.Second)
	defer cancel()
	if err := s.state.Ping(ctx); err != nil {
		writeRelayError(w, http.StatusServiceUnavailable, "not_ready")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleOAuthStart(w http.ResponseWriter, request *http.Request) {
	if !constantTimeSecretEqual(request.Header.Get(SetupKeyHeader), s.config.SetupKey) {
		writeRelayError(w, http.StatusUnauthorized, "setup_authentication_required")
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), s.config.HTTPTimeout)
	defer cancel()
	start, err := s.oauth.Begin(ctx)
	if err != nil {
		writeRelayError(w, http.StatusServiceUnavailable, "oauth_start_failed")
		return
	}
	writeRelayJSON(w, http.StatusOK, start)
}

func (s *Server) handleOAuthCallback(w http.ResponseWriter, request *http.Request) {
	ctx, cancel := context.WithTimeout(request.Context(), s.config.HTTPTimeout)
	defer cancel()
	result, err := s.oauth.CompleteCallback(ctx, OAuthCallback{
		State:         request.URL.Query().Get("state"),
		Code:          request.URL.Query().Get("code"),
		ProviderError: request.URL.Query().Get("error"),
	})
	if err != nil {
		writeOAuthHTML(w, http.StatusBadRequest, "Gmail connection was not completed", "Return to ReReply and start the connection again.")
		return
	}
	writeOAuthHTML(
		w,
		http.StatusOK,
		"Gmail connected",
		"You can close this window. ReReply is authorized for "+result.Mailbox+".",
	)
}

func (s *Server) handleAccountHealth(w http.ResponseWriter, request *http.Request) {
	if !s.pathMatchesMailbox(request) {
		writeRelayError(w, http.StatusNotFound, "unknown_account")
		return
	}
	if !s.config.Paired() || request.ContentLength != 0 || len(request.TransferEncoding) != 0 ||
		!verifyRelayBody(s.config.ReReplyOutboundSecret, request.Header.Get(ReReplySignatureHeader), nil) {
		writeRelayError(w, http.StatusUnauthorized, "invalid_signature")
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), s.config.HTTPTimeout)
	defer cancel()
	if err := s.state.Ping(ctx); err != nil {
		writeRelayError(w, http.StatusServiceUnavailable, "account_unhealthy")
		return
	}
	profile, err := s.gmail.Profile(ctx)
	if err != nil || strings.ToLower(strings.TrimSpace(profile.EmailAddress)) != s.config.Mailbox {
		writeRelayError(w, http.StatusServiceUnavailable, "account_unhealthy")
		return
	}
	lastSync, err := s.state.GetLastSuccessfulSync(ctx)
	if err != nil {
		writeRelayError(w, http.StatusServiceUnavailable, "account_unhealthy")
		return
	}
	if lastSync.IsZero() || s.now().UTC().Sub(lastSync) > max(3*s.config.GmailPollInterval, 5*time.Minute) {
		writeRelayError(w, http.StatusServiceUnavailable, "account_unhealthy")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleOutbound(w http.ResponseWriter, request *http.Request) {
	if !s.pathMatchesMailbox(request) {
		writeRelayError(w, http.StatusNotFound, "unknown_account")
		return
	}
	if !s.config.Paired() {
		writeRelayError(w, http.StatusServiceUnavailable, "account_not_paired")
		return
	}
	raw, err := readRelayBody(w, request, maxOutboundBodyBytes)
	if err != nil {
		writeRelayError(w, relayBodyStatus(err), "invalid_request_body")
		return
	}
	if !verifyRelayBody(s.config.ReReplyOutboundSecret, request.Header.Get(ReReplySignatureHeader), raw) {
		writeRelayError(w, http.StatusUnauthorized, "invalid_signature")
		return
	}
	envelope, message, err := decodeRelayOutbound(raw)
	if err != nil || envelope.Channel != models.ChannelEmail || envelope.ExternalAccountID != s.config.Mailbox {
		writeRelayError(w, http.StatusUnprocessableEntity, "unsupported_or_mismatched_delivery")
		return
	}
	switch envelope.Type {
	case "read", "subscribe":
		w.WriteHeader(http.StatusNoContent)
		return
	case "message":
		if err := validateGmailOutbound(message); err != nil {
			writeRelayError(w, http.StatusUnprocessableEntity, "unsupported_message")
			return
		}
	default:
		writeRelayError(w, http.StatusUnprocessableEntity, "unsupported_delivery")
		return
	}

	digest := relayDigest(raw)
	storeKey := s.config.Mailbox + "\n" + message.IdempotencyKey
	claimCtx, cancelClaim := s.settlementContext(request.Context())
	claim, err := s.outbound.ClaimOutbound(claimCtx, storeKey, digest, s.now().UTC())
	cancelClaim()
	if err != nil {
		writeRelayError(w, http.StatusServiceUnavailable, "idempotency_store_unavailable")
		return
	}
	switch claim.State {
	case metarelay.OutboundClaimCompleted, metarelay.OutboundClaimRejected:
		writeRawRelayJSON(w, claim.Status, claim.Result)
		return
	case metarelay.OutboundClaimCollision:
		writeRelayError(w, http.StatusConflict, "idempotency_key_collision")
		return
	case metarelay.OutboundClaimInFlight:
		w.Header().Set("Retry-After", "5")
		writeRelayError(w, http.StatusServiceUnavailable, "delivery_in_flight")
		return
	case metarelay.OutboundClaimAmbiguous:
		s.handleAmbiguousReplay(w, request, storeKey, digest, message)
		return
	case metarelay.OutboundClaimAcquired:
		// Continue to the only provider side effect.
	default:
		writeRelayError(w, http.StatusServiceUnavailable, "idempotency_store_unavailable")
		return
	}

	deliveryCtx, cancelDelivery := context.WithTimeout(context.WithoutCancel(request.Context()), s.config.HTTPTimeout)
	delivery, deliveryErr := deliverOutboundReply(deliveryCtx, s.config, s.gmail, message, s.now().UTC())
	cancelDelivery()
	if deliveryErr != nil {
		s.settleDeliveryFailure(w, request, storeKey, digest, deliveryErr)
		return
	}
	responseBody := relaySendResponseJSON(delivery.response)
	settleCtx, cancelSettle := s.settlementContext(request.Context())
	err = s.outbound.CompleteOutbound(settleCtx, storeKey, digest, http.StatusOK, responseBody, false)
	cancelSettle()
	if err != nil {
		s.markAmbiguous(request.Context(), storeKey, digest)
		w.Header().Set("Retry-After", "5")
		writeRelayError(w, http.StatusServiceUnavailable, "delivery_outcome_ambiguous")
		return
	}
	if delivery.response.ThreadID != message.Conversation.ExternalID {
		s.logger.Error("Gmail accepted reply into a different thread", "component", "outbound_delivery")
	}
	writeRawRelayJSON(w, http.StatusOK, responseBody)
}

func (s *Server) handleAmbiguousReplay(
	w http.ResponseWriter,
	request *http.Request,
	storeKey, digest string,
	message channelapi.OutboundMessage,
) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(request.Context()), s.config.HTTPTimeout)
	response, found, err := reconcileOutboundReply(ctx, s.config, s.gmail, message)
	cancel()
	if err != nil {
		w.Header().Set("Retry-After", "5")
		writeRelayError(w, http.StatusServiceUnavailable, "delivery_reconciliation_unavailable")
		return
	}
	if !found {
		w.Header().Set("Retry-After", "5")
		writeRelayError(w, http.StatusServiceUnavailable, "delivery_outcome_ambiguous")
		return
	}
	body := relaySendResponseJSON(response)
	settleCtx, cancelSettle := s.settlementContext(request.Context())
	err = s.outbound.ResolveOutboundAmbiguous(settleCtx, storeKey, digest, http.StatusOK, body)
	cancelSettle()
	if err != nil {
		writeRelayError(w, http.StatusServiceUnavailable, "idempotency_store_unavailable")
		return
	}
	writeRawRelayJSON(w, http.StatusOK, body)
}

func (s *Server) settleDeliveryFailure(
	w http.ResponseWriter,
	request *http.Request,
	storeKey, digest string,
	deliveryErr error,
) {
	var detail *outboundDeliveryError
	if !errors.As(deliveryErr, &detail) {
		detail = &outboundDeliveryError{err: deliveryErr}
	}
	var apiErr *GmailAPIError
	errors.As(deliveryErr, &apiErr)

	if detail.sendAttempted && (apiErr == nil || apiErr.StatusCode == 0 || apiErr.StatusCode >= 500) {
		s.markAmbiguous(request.Context(), storeKey, digest)
		w.Header().Set("Retry-After", "5")
		writeRelayError(w, http.StatusServiceUnavailable, "delivery_outcome_ambiguous")
		return
	}
	if apiErr != nil && apiErr.StatusCode == http.StatusTooManyRequests {
		if apiErr.RetryAfter > 0 {
			w.Header().Set("Retry-After", strconvSeconds(apiErr.RetryAfter))
		}
		s.releaseClaim(request.Context(), storeKey, digest)
		writeRelayError(w, http.StatusTooManyRequests, "gmail_rate_limited")
		return
	}
	if apiErr != nil && (apiErr.StatusCode == 0 || apiErr.StatusCode == 401 || apiErr.StatusCode == 403 || apiErr.StatusCode >= 500) {
		s.releaseClaim(request.Context(), storeKey, digest)
		writeRelayError(w, http.StatusServiceUnavailable, "gmail_unavailable")
		return
	}

	body := relayErrorJSON("unsupported_message")
	settleCtx, cancelSettle := s.settlementContext(request.Context())
	err := s.outbound.CompleteOutbound(settleCtx, storeKey, digest, http.StatusUnprocessableEntity, body, true)
	cancelSettle()
	if err != nil {
		writeRelayError(w, http.StatusServiceUnavailable, "idempotency_store_unavailable")
		return
	}
	writeRawRelayJSON(w, http.StatusUnprocessableEntity, body)
}

func (s *Server) releaseClaim(parent context.Context, storeKey, digest string) {
	ctx, cancel := s.settlementContext(parent)
	defer cancel()
	if err := s.outbound.ReleaseOutbound(ctx, storeKey, digest); err != nil {
		s.logger.Error("Gmail outbound retry claim could not be released", "component", "outbound_settlement")
	}
}

func (s *Server) markAmbiguous(parent context.Context, storeKey, digest string) {
	ctx, cancel := s.settlementContext(parent)
	defer cancel()
	if err := s.outbound.MarkOutboundAmbiguous(ctx, storeKey, digest); err != nil {
		s.logger.Error("Gmail outbound ambiguity could not be persisted", "component", "outbound_settlement")
	}
}

func (s *Server) settlementContext(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(parent), settlementTimeout)
}

func (s *Server) pathMatchesMailbox(request *http.Request) bool {
	return strings.EqualFold(strings.TrimSpace(request.PathValue("externalID")), s.config.Mailbox)
}

type relayOutboundEnvelope struct {
	Type              string          `json:"type"`
	Channel           models.Channel  `json:"channel"`
	ExternalAccountID string          `json:"external_account_id"`
	Data              json.RawMessage `json:"data"`
}

func decodeRelayOutbound(raw []byte) (relayOutboundEnvelope, channelapi.OutboundMessage, error) {
	var envelope relayOutboundEnvelope
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&envelope); err != nil {
		return envelope, channelapi.OutboundMessage{}, err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return envelope, channelapi.OutboundMessage{}, errors.New("multiple JSON values are not allowed")
	}
	var message channelapi.OutboundMessage
	if envelope.Type == "message" {
		if len(envelope.Data) == 0 || string(envelope.Data) == "null" {
			return envelope, message, errors.New("relay message data is required")
		}
		if err := json.Unmarshal(envelope.Data, &message); err != nil {
			return envelope, message, err
		}
	}
	return envelope, message, nil
}

func validateGmailOutbound(message channelapi.OutboundMessage) error {
	if strings.TrimSpace(message.IdempotencyKey) == "" || len(message.IdempotencyKey) > 255 {
		return errors.New("idempotency key is required")
	}
	if strings.TrimSpace(message.Conversation.ExternalID) == "" || len(message.Conversation.ExternalID) > 255 {
		return errors.New("gmail thread ID is required")
	}
	if len(message.Parts) != 1 || message.Parts[0].Type != models.MessagePartTypeText ||
		strings.TrimSpace(message.Parts[0].Text) == "" || utf8.RuneCountInString(message.Parts[0].Text) > 10_000 {
		return errors.New("exactly one bounded text part is required")
	}
	if message.Template != nil || len(message.CC) > 0 || len(message.ReplyToExternalID) > 255 {
		return errors.New("unsupported email message feature")
	}
	_, err := normalizeRecipient(message.Recipient)
	return err
}

func relaySendResponseJSON(response GmailSendResponse) []byte {
	body, _ := json.Marshal(struct {
		ProviderMessageIDs     []string `json:"provider_message_ids"`
		ExternalConversationID string   `json:"external_conversation_id"`
	}{
		ProviderMessageIDs:     []string{response.ID},
		ExternalConversationID: response.ThreadID,
	})
	return body
}

func relayDigest(body []byte) string {
	digest := sha256.Sum256(body)
	return hex.EncodeToString(digest[:])
}

func readRelayBody(w http.ResponseWriter, request *http.Request, limit int64) ([]byte, error) {
	request.Body = http.MaxBytesReader(w, request.Body, limit)
	return io.ReadAll(request.Body)
}

func relayBodyStatus(err error) int {
	var maxBytesErr *http.MaxBytesError
	if errors.As(err, &maxBytesErr) {
		return http.StatusRequestEntityTooLarge
	}
	return http.StatusBadRequest
}

func writeRelayJSON(w http.ResponseWriter, status int, value any) {
	body, err := json.Marshal(value)
	if err != nil {
		writeRelayError(w, http.StatusInternalServerError, "response_encoding_failed")
		return
	}
	writeRawRelayJSON(w, status, body)
}

func writeRawRelayJSON(w http.ResponseWriter, status int, body []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if len(body) > 0 {
		_, _ = w.Write(body)
	}
}

func writeRelayError(w http.ResponseWriter, status int, code string) {
	writeRawRelayJSON(w, status, relayErrorJSON(code))
}

func relayErrorJSON(code string) []byte {
	body, _ := json.Marshal(map[string]string{"error": code})
	return body
}

func writeOAuthHTML(w http.ResponseWriter, status int, title, detail string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, "<!doctype html><html><head><meta charset=\"utf-8\"><meta name=\"viewport\" content=\"width=device-width\"><title>"+html.EscapeString(title)+"</title></head><body><main><h1>"+html.EscapeString(title)+"</h1><p>"+html.EscapeString(detail)+"</p></main></body></html>")
}

func gmailSecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; base-uri 'none'; frame-ancestors 'none'")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		next.ServeHTTP(w, request)
	})
}

func strconvSeconds(value time.Duration) string {
	seconds := int(value.Round(time.Second).Seconds())
	if seconds < 1 {
		seconds = 1
	}
	return fmt.Sprintf("%d", seconds)
}
