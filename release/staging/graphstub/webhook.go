package graphstub

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// SignatureHeader carries Meta's webhook signature, which the product checks
// before it reads anything else from a webhook (internal/handlers/webhook.go).
const SignatureHeader = "X-Hub-Signature-256"

const (
	webhookTimeout          = 10 * time.Second
	maxWebhookResponseBytes = 64 << 10
)

var errDialRefused = errors.New("graph stub dials only STUB_CALLBACK_ORIGIN")

// SignWebhook returns the X-Hub-Signature-256 value Meta sends for body.
func SignWebhook(appSecret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(appSecret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// webhookClient posts only to the configured callback origin. The URL is
// always built from that origin, and the dialer refuses any other address as
// a second line of defence; redirects are never followed and proxies from the
// environment are ignored.
type webhookClient struct {
	origin    *url.URL
	transport *http.Transport
	client    *http.Client
}

func newWebhookClient(origin string) (*webhookClient, error) {
	parsed, err := url.Parse(origin)
	if err != nil || parsed.Host == "" {
		return nil, errors.New("STUB_CALLBACK_ORIGIN is invalid")
	}
	allowed := strings.ToLower(net.JoinHostPort(parsed.Hostname(), effectivePort(parsed)))
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	transport := &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			if strings.ToLower(address) != allowed {
				return nil, errDialRefused
			}
			return dialer.DialContext(ctx, network, address)
		},
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: webhookTimeout,
		MaxIdleConns:          4,
		IdleConnTimeout:       30 * time.Second,
	}
	return &webhookClient{
		origin:    parsed,
		transport: transport,
		client: &http.Client{
			Transport: transport,
			Timeout:   webhookTimeout,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}, nil
}

func (w *webhookClient) close() {
	w.transport.CloseIdleConnections()
}

func effectivePort(parsed *url.URL) string {
	if port := parsed.Port(); port != "" {
		return port
	}
	if parsed.Scheme == "https" {
		return "443"
	}
	return "80"
}

// target joins the callback origin with the product path. A phone's webhook
// override contributes only its path and query: whatever host it names, the
// stub still posts to STUB_CALLBACK_ORIGIN.
func (w *webhookClient) target(overrideURL string) string {
	path := WebhookPath
	if overrideURL != "" {
		if parsed, err := url.Parse(overrideURL); err == nil && parsed.EscapedPath() != "" {
			path = parsed.EscapedPath()
			if parsed.RawQuery != "" {
				path += "?" + parsed.RawQuery
			}
		}
	}
	return w.origin.Scheme + "://" + w.origin.Host + path
}

// post delivers one signed webhook and returns the product's HTTP status.
func (w *webhookClient) post(ctx context.Context, target, appSecret string, payload any) (int, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return 0, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("User-Agent", "graph-stub")
	request.Header.Set(SignatureHeader, SignWebhook(appSecret, body))
	response, err := w.client.Do(request)
	if err != nil {
		return 0, err
	}
	defer func() { _ = response.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxWebhookResponseBytes))
	return response.StatusCode, nil
}

// deliver posts payload for phoneID (empty for WABA-level events) and journals
// the attempt. It never journals or logs the payload.
func (s *Server) deliver(ctx context.Context, phoneID string, payload any, entry Entry) (int, error) {
	s.mu.Lock()
	override := ""
	if item, ok := s.state.phones[phoneID]; ok {
		override = item.overrideURL
	}
	s.mu.Unlock()
	status, err := s.webhooks.post(ctx, s.webhooks.target(override), s.cfg.AppSecret, payload)
	entry.Kind, entry.Method, entry.Status = KindWebhook, http.MethodPost, status
	if err != nil {
		entry.Error = "delivery_failed"
		if errors.Is(err, errDialRefused) {
			entry.Error = "dial_refused"
		}
	}
	s.journal.add(entry)
	s.log.Info("webhook", "route", entry.Route, "status", status, "error", entry.Error)
	return status, err
}

func webhookEnvelope(wabaID, field string, value map[string]any) map[string]any {
	return map[string]any{
		"object": "whatsapp_business_account",
		"entry": []any{map[string]any{
			"id":      wabaID,
			"changes": []any{map[string]any{"field": field, "value": value}},
		}},
	}
}

func messagesValue(account Account) map[string]any {
	return map[string]any{
		"messaging_product": "whatsapp",
		"metadata": map[string]any{
			"display_phone_number": account.DisplayPhoneNumber,
			"phone_number_id":      account.PhoneNumberID,
		},
	}
}

// statusPayload is the webhook Meta sends as an outbound message progresses.
func statusPayload(account Account, message sentMessage, status string, at time.Time) map[string]any {
	item := map[string]any{
		"id":           message.ID,
		"status":       status,
		"timestamp":    strconv.FormatInt(at.Unix(), 10),
		"recipient_id": message.To,
	}
	if status == "failed" {
		item["errors"] = []any{map[string]any{
			"code":       131026,
			"title":      "Message undeliverable",
			"message":    "Message undeliverable",
			"error_data": map[string]any{"details": "Synthetic failure from the Graph stub"},
		}}
	} else {
		item["conversation"] = map[string]any{
			"id":     hex.EncodeToString(sha256Sum([]byte(account.PhoneNumberID + "|" + message.To))[:16]),
			"origin": map[string]any{"type": "service"},
		}
		item["pricing"] = map[string]any{"billable": false, "pricing_model": "PMP", "category": "service"}
	}
	value := messagesValue(account)
	value["statuses"] = []any{item}
	return webhookEnvelope(account.BusinessAccountID, "messages", value)
}

func sha256Sum(data []byte) []byte {
	sum := sha256.Sum256(data)
	return sum[:]
}

// scheduleStatuses delivers the configured status sequence for one accepted
// send. A reset or Close stops the sequence before its next delivery.
func (s *Server) scheduleStatuses(account Account, message sentMessage, generation uint64) bool {
	sequence := s.cfg.StatusSequence
	if len(sequence) == 0 {
		return true
	}
	select {
	case s.pending <- struct{}{}:
	default:
		return false
	}
	// Add under mu so it can never race Close's Wait.
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		<-s.pending
		return false
	}
	s.wg.Add(1)
	s.mu.Unlock()
	go func() {
		defer s.wg.Done()
		defer func() { <-s.pending }()
		timer := time.NewTimer(s.cfg.StatusInterval)
		defer timer.Stop()
		for index, status := range sequence {
			if index > 0 {
				timer.Reset(s.cfg.StatusInterval)
			}
			select {
			case <-s.ctx.Done():
				return
			case <-timer.C:
			}
			s.mu.Lock()
			current := s.generation == generation
			s.mu.Unlock()
			if !current {
				return
			}
			ctx, cancel := context.WithTimeout(s.ctx, webhookTimeout)
			_, _ = s.deliver(ctx, account.PhoneNumberID, statusPayload(account, message, status, s.now()), Entry{
				Route:             "status",
				PhoneNumberID:     account.PhoneNumberID,
				BusinessAccountID: account.BusinessAccountID,
				MessageID:         message.ID,
				DeliveryStatus:    status,
			})
			cancel()
		}
	}()
	return true
}
