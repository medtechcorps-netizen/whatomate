package graphstub

import (
	"bufio"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// maxPendingStatusSequences bounds the goroutines that deliver scheduled
// status webhooks. A send beyond it is answered normally but gets no statuses,
// and the journal records that.
const maxPendingStatusSequences = 1024

// Server is the Graph stub's HTTP handler. It needs no listener of its own;
// cmd/graph-stub serves it and tests wrap it in httptest.
type Server struct {
	cfg      Config
	log      *slog.Logger
	journal  *journal
	rejected *journal
	nonces   *nonceCache
	webhooks *webhookClient
	appToken string
	now      func() time.Time

	ctx     context.Context
	cancel  context.CancelFunc
	wg      sync.WaitGroup
	pending chan struct{}

	mu         sync.Mutex
	state      *state
	faults     []*Fault
	generation uint64
	closed     bool
}

// New validates cfg and returns a ready handler. Close stops the scheduled
// status deliveries it starts.
func New(cfg Config, logger *slog.Logger) (*Server, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	webhooks, err := newWebhookClient(cfg.CallbackOrigin)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	mac := hmac.New(sha256.New, []byte(cfg.AppSecret))
	mac.Write([]byte("graph-stub app access token"))
	return &Server{
		cfg:      cfg,
		log:      logger,
		journal:  newJournal(JournalCapacity),
		rejected: newJournal(RejectedCapacity),
		nonces:   newNonceCache(),
		webhooks: webhooks,
		appToken: cfg.AppID + "|" + hex.EncodeToString(mac.Sum(nil))[:32],
		now:      time.Now,
		ctx:      ctx,
		cancel:   cancel,
		pending:  make(chan struct{}, maxPendingStatusSequences),
		state:    newState(cfg.Accounts),
	}, nil
}

// Close cancels scheduled status deliveries and waits for them to stop.
func (s *Server) Close() {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	s.cancel()
	s.wg.Wait()
	s.webhooks.close()
}

var knownMethods = map[string]bool{
	http.MethodGet: true, http.MethodHead: true, http.MethodPost: true, http.MethodPut: true,
	http.MethodPatch: true, http.MethodDelete: true, http.MethodOptions: true,
}

// call is one request in flight. Handlers fill entry; ServeHTTP journals it.
// trusted is set once the request has proved a credential (a control MAC, a
// Graph token, the app's client secret or app token) or consumed a fault an
// authenticated control call queued; only then does it reach the journal.
// Everything else goes to the small rejected ring.
type call struct {
	w        *statusWriter
	r        *http.Request
	entry    *Entry
	segments []string
	trusted  bool
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/healthz" || r.URL.Path == "/readyz" {
		// Health probes are neither journaled nor logged: on staging they
		// would push every useful entry out of the ring.
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
		return
	}
	started := s.now()
	entry := &Entry{Kind: KindGraph, Method: "OTHER", Route: "unsupported"}
	if knownMethods[r.Method] {
		entry.Method = r.Method
	}
	c := &call{w: &statusWriter{ResponseWriter: w}, r: r, entry: entry}
	defer func() {
		entry.Status = c.w.status
		if c.trusted || entry.Fault {
			s.journal.add(*entry)
		} else {
			s.rejected.add(*entry)
		}
		// Route names are fixed strings; IDs, paths and bodies stay out of logs.
		s.log.Info("request", "kind", entry.Kind, "method", entry.Method, "route", entry.Route,
			"status", entry.Status, "fault", entry.Fault, "duration_ms", s.now().Sub(started).Milliseconds())
	}()

	if RefusedHost(r.Host) {
		entry.Kind, entry.Route, entry.Error = KindRefused, "host", "production host"
		writeJSON(c.w, http.StatusMisdirectedRequest, map[string]string{"error": "the Graph stub does not serve production hosts"})
		return
	}
	if r.URL.Path == controlPrefix || strings.HasPrefix(r.URL.Path, controlPrefix+"/") {
		entry.Kind, entry.Route = KindControl, "auth"
		s.serveControl(c)
		return
	}
	s.serveGraph(c)
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusWriter) Write(data []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(data)
}

// Hijack lets a "drop" fault close the connection without a response.
func (w *statusWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hijacker, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("connection cannot be hijacked")
	}
	return hijacker.Hijack()
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	body, err := json.Marshal(value)
	if err != nil {
		status, body = http.StatusInternalServerError, []byte(`{"error":"encoding failed"}`)
	}
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// graphError writes Meta's error envelope, which pkg/whatsapp parses.
func graphError(c *call, status, code, subcode int, kind, message string, transient bool) {
	if c.entry.Error == "" {
		c.entry.Error = "graph_" + strconv.Itoa(code)
	}
	body := map[string]any{
		"message":      message,
		"type":         kind,
		"code":         code,
		"is_transient": transient,
		"fbtrace_id":   "stub" + randomToken(9),
	}
	if subcode != 0 {
		body["error_subcode"] = subcode
	}
	writeJSON(c.w, status, map[string]any{"error": body})
}

func invalidParameter(c *call, message string) {
	graphError(c, http.StatusBadRequest, 100, 0, "OAuthException", "(#100) "+message, false)
}

func unsupported(c *call) {
	graphError(c, http.StatusBadRequest, 100, 33, "GraphMethodException",
		"Unsupported "+strings.ToLower(c.r.Method)+" request. Object does not exist, cannot be loaded due to missing permissions, or does not support this operation.", false)
}

// requestOrigin is the origin the caller used to reach the stub, for URLs the
// stub hands back to that caller (media downloads, paging.next). The scheme is
// https when the connection is TLS or a TLS-terminating proxy says so in
// X-Forwarded-Proto; the host is the request's own Host, which RefusedHost
// has already vetted.
func requestOrigin(r *http.Request) (scheme, host string) {
	scheme = "http"
	if r.TLS != nil {
		scheme = "https"
	} else if forwarded, _, _ := strings.Cut(r.Header.Get("X-Forwarded-Proto"), ","); strings.EqualFold(strings.TrimSpace(forwarded), "https") {
		scheme = "https"
	}
	return scheme, r.Host
}

// bearer returns the Authorization token for the Bearer or OAuth scheme.
func bearer(r *http.Request) string {
	scheme, token, ok := strings.Cut(strings.TrimSpace(r.Header.Get("Authorization")), " ")
	if !ok || (!strings.EqualFold(scheme, "Bearer") && !strings.EqualFold(scheme, "OAuth")) {
		return ""
	}
	return strings.TrimSpace(token)
}

// oneOf compares value with every candidate in constant time per candidate
// and does not stop at the first match.
func oneOf(value string, candidates ...string) bool {
	match := 0
	for _, candidate := range candidates {
		match |= subtle.ConstantTimeCompare([]byte(value), []byte(candidate))
	}
	return value != "" && match == 1
}

// readBody reads at most limit bytes and reports whether the body fit.
func readBody(c *call, limit int64) ([]byte, bool) {
	body, err := readLimited(c.r, limit)
	if errors.Is(err, errBodyTooLarge) {
		c.entry.Error = "body_too_large"
		writeJSON(c.w, http.StatusRequestEntityTooLarge, map[string]string{"error": "request body too large"})
		return nil, false
	}
	if err != nil {
		c.entry.Error = "body_unreadable"
		writeJSON(c.w, http.StatusBadRequest, map[string]string{"error": "request body unreadable"})
		return nil, false
	}
	return body, true
}

var errBodyTooLarge = errors.New("request body too large")

func readLimited(r *http.Request, limit int64) ([]byte, error) {
	if r.Body == nil {
		return nil, nil
	}
	if r.ContentLength > limit {
		return nil, errBodyTooLarge
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, errBodyTooLarge
	}
	return body, nil
}
