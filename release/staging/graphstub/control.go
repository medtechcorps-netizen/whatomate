package graphstub

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Control API authentication. Every /_control request carries
//
//	X-Stub-Timestamp: Unix seconds
//	X-Stub-Nonce:     16 to 128 characters of [A-Za-z0-9_-], never reused
//	X-Stub-Mac:       hex HMAC-SHA256(STUB_CONTROL_KEY,
//	                  method|path|timestamp|nonce|hex(sha256(body)))
//
// where path is the request path plus "?query" when there is one, exactly as
// the stub receives it.
const (
	HeaderTimestamp = "X-Stub-Timestamp"
	HeaderNonce     = "X-Stub-Nonce"
	HeaderMAC       = "X-Stub-Mac"
	ControlSkew     = 60 * time.Second

	controlPrefix  = "/_control"
	maxControlBody = 8 << 20
	maxNonces      = 65_536
)

var (
	noncePattern     = regexp.MustCompile(`^[A-Za-z0-9_-]{16,128}$`)
	timestampPattern = regexp.MustCompile(`^[0-9]{1,12}$`)
	macPattern       = regexp.MustCompile(`^[0-9a-f]{64}$`)
	wamidPattern     = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)
	waIDPattern      = regexp.MustCompile(`^[0-9]{5,32}$`)
	oauthCodePattern = regexp.MustCompile(`^[A-Za-z0-9._~-]{8,512}$`)
)

// ControlMAC returns the X-Stub-Mac value for one control request.
func ControlMAC(key, method, path, timestamp, nonce string, body []byte) string {
	bodySum := sha256.Sum256(body)
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte(method + "|" + path + "|" + timestamp + "|" + nonce + "|" + hex.EncodeToString(bodySum[:])))
	return hex.EncodeToString(mac.Sum(nil))
}

// SignControlRequest sets the three authentication headers on request, whose
// body must be body, using a fresh random nonce.
func SignControlRequest(request *http.Request, key string, body []byte, now time.Time) {
	timestamp := strconv.FormatInt(now.Unix(), 10)
	nonce := randomToken(24)
	request.Header.Set(HeaderTimestamp, timestamp)
	request.Header.Set(HeaderNonce, nonce)
	request.Header.Set(HeaderMAC, ControlMAC(key, request.Method, request.URL.RequestURI(), timestamp, nonce, body))
}

// nonceCache remembers each accepted nonce for twice the skew, which covers
// every timestamp the skew check can still accept.
type nonceCache struct {
	mu   sync.Mutex
	seen map[string]time.Time
}

func newNonceCache() *nonceCache {
	return &nonceCache{seen: make(map[string]time.Time)}
}

// use records nonce. It returns false when the nonce was seen before or the
// cache is full of live nonces (failing closed).
func (n *nonceCache) use(nonce string, now time.Time) (accepted, full bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if _, replayed := n.seen[nonce]; replayed {
		return false, false
	}
	if len(n.seen) >= maxNonces {
		for value, expires := range n.seen {
			if !expires.After(now) {
				delete(n.seen, value)
			}
		}
		if len(n.seen) >= maxNonces {
			return false, true
		}
	}
	n.seen[nonce] = now.Add(2 * ControlSkew)
	return true, false
}

func controlUnauthorized(c *call, reason string) {
	c.entry.Error = reason
	writeJSON(c.w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
}

// authenticateControl checks the headers and the clock before it reads the
// body, then the MAC, and only then spends the nonce, so unauthenticated
// callers can neither make the stub buffer a body nor fill the replay cache.
func (s *Server) authenticateControl(c *call) ([]byte, bool) {
	timestamp := c.r.Header.Get(HeaderTimestamp)
	nonce := c.r.Header.Get(HeaderNonce)
	mac := strings.ToLower(c.r.Header.Get(HeaderMAC))
	if timestamp == "" || nonce == "" || mac == "" {
		controlUnauthorized(c, "missing_header")
		return nil, false
	}
	if !timestampPattern.MatchString(timestamp) || !noncePattern.MatchString(nonce) || !macPattern.MatchString(mac) {
		controlUnauthorized(c, "malformed_header")
		return nil, false
	}
	seconds, _ := strconv.ParseInt(timestamp, 10, 64)
	now := s.now()
	if skew := now.Sub(time.Unix(seconds, 0)); skew > ControlSkew || skew < -ControlSkew {
		controlUnauthorized(c, "stale_timestamp")
		return nil, false
	}
	body, ok := readBody(c, maxControlBody)
	if !ok {
		return nil, false
	}
	expected := ControlMAC(s.cfg.ControlKey, c.r.Method, c.r.URL.RequestURI(), timestamp, nonce, body)
	if subtle.ConstantTimeCompare([]byte(mac), []byte(expected)) != 1 {
		controlUnauthorized(c, "bad_mac")
		return nil, false
	}
	accepted, full := s.nonces.use(nonce, now)
	if full {
		c.entry.Error = "replay_cache_full"
		writeJSON(c.w, http.StatusServiceUnavailable, map[string]string{"error": "replay cache full"})
		return nil, false
	}
	if !accepted {
		controlUnauthorized(c, "replayed_nonce")
		return nil, false
	}
	return body, true
}

func (s *Server) serveControl(c *call) {
	body, ok := s.authenticateControl(c)
	if !ok {
		return
	}
	name := strings.TrimPrefix(c.r.URL.Path, controlPrefix+"/")
	c.entry.Route = name
	routes := map[string]map[string]func(*call, []byte){
		"journal":     {http.MethodGet: s.controlJournal},
		"accounts":    {http.MethodGet: s.controlListAccounts, http.MethodPost: s.controlAddAccount},
		"inbound":     {http.MethodPost: s.controlInbound},
		"status":      {http.MethodPost: s.controlStatus},
		"media":       {http.MethodPost: s.controlMedia},
		"templates":   {http.MethodPost: s.controlTemplateStatus},
		"oauth/codes": {http.MethodPost: s.controlOAuthCode},
		"faults":      {http.MethodPost: s.controlAddFault, http.MethodDelete: s.controlClearFaults},
		"reset":       {http.MethodPost: s.controlReset},
	}
	handler := routes[name][c.r.Method]
	if handler == nil {
		c.entry.Route = "unknown"
		writeJSON(c.w, http.StatusNotFound, map[string]string{"error": "unknown control route"})
		return
	}
	handler(c, body)
}

func controlBadRequest(c *call, message string) {
	c.entry.Error = "invalid_request"
	writeJSON(c.w, http.StatusBadRequest, map[string]string{"error": message})
}

// decodeControl decodes a control body strictly, so a misspelt field fails
// loudly instead of being ignored.
func decodeControl(c *call, body []byte, target any) bool {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil || decoder.More() {
		controlBadRequest(c, "body must be one JSON object with known fields")
		return false
	}
	return true
}

func (s *Server) controlJournal(c *call, _ []byte) {
	query := c.r.URL.Query()
	after, limit := uint64(0), 100
	if raw := query.Get("after"); raw != "" {
		value, err := strconv.ParseUint(raw, 10, 64)
		if err != nil {
			controlBadRequest(c, "after must be a sequence number")
			return
		}
		after = value
	}
	if raw := query.Get("limit"); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 1 || value > 1000 {
			controlBadRequest(c, "limit must be 1 to 1000")
			return
		}
		limit = value
	}
	entries, latest := s.journal.since(after, limit)
	writeJSON(c.w, http.StatusOK, map[string]any{"entries": entries, "latest": latest})
}

func (s *Server) controlListAccounts(c *call, _ []byte) {
	s.mu.Lock()
	accounts := make([]Account, 0, len(s.state.phones))
	for _, item := range s.state.phones {
		accounts = append(accounts, item.account)
	}
	s.mu.Unlock()
	sort.Slice(accounts, func(i, j int) bool { return accounts[i].PhoneNumberID < accounts[j].PhoneNumberID })
	writeJSON(c.w, http.StatusOK, map[string]any{"accounts": accounts})
}

// controlAddAccount registers (or renames) a synthetic WABA and phone. It is
// idempotent, so a test run can re-register its fixture after a redeploy.
func (s *Server) controlAddAccount(c *call, body []byte) {
	var request Account
	if !decodeControl(c, body, &request) {
		return
	}
	account, err := normalizeAccount(request)
	if err != nil {
		controlBadRequest(c, err.Error())
		return
	}
	c.entry.PhoneNumberID, c.entry.BusinessAccountID = account.PhoneNumberID, account.BusinessAccountID
	s.mu.Lock()
	err = s.state.upsertAccount(account)
	s.mu.Unlock()
	if err != nil {
		controlBadRequest(c, err.Error())
		return
	}
	writeJSON(c.w, http.StatusOK, map[string]bool{"ok": true})
}

type inboundRequest struct {
	PhoneNumberID    string `json:"phone_number_id"`
	From             string `json:"from"`
	ProfileName      string `json:"profile_name"`
	Type             string `json:"type"`
	Text             string `json:"text"`
	MediaID          string `json:"media_id"`
	Caption          string `json:"caption"`
	Filename         string `json:"filename"`
	MessageID        string `json:"message_id"`
	ContextMessageID string `json:"context_message_id"`
	Timestamp        int64  `json:"timestamp"`
}

var inboundTypes = map[string]bool{"text": true, "image": true, "document": true, "audio": true, "video": true}

// controlInbound delivers one signed inbound customer message to the product,
// shaped like the payload the CRM canary signs (frontend/canary-driver), and
// answers with the product's HTTP status once the delivery completes.
func (s *Server) controlInbound(c *call, body []byte) {
	var request inboundRequest
	if !decodeControl(c, body, &request) {
		return
	}
	if request.Type == "" {
		request.Type = "text"
	}
	if request.ProfileName == "" {
		request.ProfileName = "Graph Stub Customer"
	}
	if request.MessageID == "" {
		request.MessageID = newMessageID()
	}
	if request.Timestamp == 0 {
		request.Timestamp = s.now().Unix()
	}
	switch {
	case !inboundTypes[request.Type]:
		controlBadRequest(c, "type must be text, image, document, audio or video")
		return
	case !waIDPattern.MatchString(request.From):
		controlBadRequest(c, "from must be a WhatsApp ID of 5 to 32 digits")
		return
	case !printableNamePattern.MatchString(request.ProfileName):
		controlBadRequest(c, "profile_name must be 1 to 96 printable characters")
		return
	case !wamidPattern.MatchString(request.MessageID):
		controlBadRequest(c, "message_id is not WAMID-shaped")
		return
	case request.ContextMessageID != "" && !wamidPattern.MatchString(request.ContextMessageID):
		controlBadRequest(c, "context_message_id is not WAMID-shaped")
		return
	case request.Type == "text" && (request.Text == "" || utf8.RuneCountInString(request.Text) > 4096):
		controlBadRequest(c, "text must be 1 to 4096 characters")
		return
	case utf8.RuneCountInString(request.Caption) > 1024 || utf8.RuneCountInString(request.Filename) > 240:
		controlBadRequest(c, "caption or filename is too long")
		return
	}

	message := map[string]any{
		"from":      request.From,
		"id":        request.MessageID,
		"timestamp": strconv.FormatInt(request.Timestamp, 10),
		"type":      request.Type,
	}
	entry := Entry{Route: "inbound", MessageID: request.MessageID, MessageType: request.Type, To: request.From}
	s.mu.Lock()
	item := s.state.phones[request.PhoneNumberID]
	var account Account
	if item != nil {
		account = item.account
	}
	var stored *media
	if request.Type != "text" {
		stored = s.state.media[request.MediaID]
	}
	s.mu.Unlock()
	if item == nil {
		controlBadRequest(c, "phone_number_id is not a stub account")
		return
	}
	if request.Type == "text" {
		message["text"] = map[string]string{"body": request.Text}
		sum := sha256.Sum256([]byte(request.Text))
		entry.TextSHA256 = hex.EncodeToString(sum[:])
	} else {
		if stored == nil {
			controlBadRequest(c, "media_id is not stored in the stub; upload it with /_control/media")
			return
		}
		content := map[string]string{"id": stored.ID, "mime_type": stored.MimeType, "sha256": stored.SHA256}
		if request.Caption != "" && request.Type != "audio" {
			content["caption"] = request.Caption
		}
		if request.Filename != "" && request.Type == "document" {
			content["filename"] = request.Filename
		}
		message[request.Type] = content
		entry.MediaID = stored.ID
	}
	if request.ContextMessageID != "" {
		message["context"] = map[string]string{"from": digitsOnly(account.DisplayPhoneNumber), "id": request.ContextMessageID}
	}
	value := messagesValue(account)
	value["contacts"] = []any{map[string]any{"profile": map[string]string{"name": request.ProfileName}, "wa_id": request.From}}
	value["messages"] = []any{message}
	entry.PhoneNumberID, entry.BusinessAccountID = account.PhoneNumberID, account.BusinessAccountID
	c.entry.PhoneNumberID, c.entry.MessageID = account.PhoneNumberID, request.MessageID
	s.deliverForControl(c, account.PhoneNumberID, webhookEnvelope(account.BusinessAccountID, "messages", value), entry,
		map[string]any{"message_id": request.MessageID})
}

func digitsOnly(value string) string {
	return strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' {
			return r
		}
		return -1
	}, value)
}

// deliverForControl posts one webhook synchronously and reports the product's
// status, or 502 when the product could not be reached.
func (s *Server) deliverForControl(c *call, phoneID string, payload any, entry Entry, response map[string]any) {
	ctx, cancel := context.WithTimeout(c.r.Context(), webhookTimeout)
	defer cancel()
	status, err := s.deliver(ctx, phoneID, payload, entry)
	if err != nil {
		c.entry.Error = "callback_unreachable"
		response["error"] = "callback delivery failed"
		writeJSON(c.w, http.StatusBadGateway, response)
		return
	}
	response["callback_status"] = status
	writeJSON(c.w, http.StatusOK, response)
}

// controlStatus delivers one status webhook for a message the stub accepted.
func (s *Server) controlStatus(c *call, body []byte) {
	var request struct {
		MessageID string `json:"message_id"`
		Status    string `json:"status"`
	}
	if !decodeControl(c, body, &request) {
		return
	}
	if !scheduledStatuses[request.Status] {
		controlBadRequest(c, "status must be sent, delivered, read or failed")
		return
	}
	s.mu.Lock()
	message, known := s.state.messages[request.MessageID]
	var account Account
	if item := s.state.phones[message.PhoneID]; known && item != nil {
		account = item.account
	}
	s.mu.Unlock()
	if account.PhoneNumberID == "" {
		controlBadRequest(c, "message_id is not a message the stub accepted")
		return
	}
	c.entry.MessageID = message.ID
	s.deliverForControl(c, account.PhoneNumberID, statusPayload(account, message, request.Status, s.now()), Entry{
		Route:             "status",
		PhoneNumberID:     account.PhoneNumberID,
		BusinessAccountID: account.BusinessAccountID,
		MessageID:         message.ID,
		DeliveryStatus:    request.Status,
	}, map[string]any{"message_id": message.ID})
}

// controlMedia stores media a synthetic customer "sends", for an inbound
// image, document, audio or video message.
func (s *Server) controlMedia(c *call, body []byte) {
	var request struct {
		PhoneNumberID string `json:"phone_number_id"`
		MimeType      string `json:"mime_type"`
		DataBase64    string `json:"data_base64"`
	}
	if !decodeControl(c, body, &request) {
		return
	}
	data, err := base64.StdEncoding.DecodeString(request.DataBase64)
	switch {
	case err != nil || len(data) == 0:
		controlBadRequest(c, "data_base64 must be non-empty standard base64")
		return
	case len(data) > maxMediaBytes:
		controlBadRequest(c, "media is too large")
		return
	case !mimeTypePattern.MatchString(request.MimeType):
		controlBadRequest(c, "mime_type must be a lowercase media type")
		return
	}
	s.mu.Lock()
	known := s.state.phones[request.PhoneNumberID] != nil
	var item *media
	if known {
		item = newMedia(request.PhoneNumberID, request.MimeType, data)
		s.state.storeMedia(item)
	}
	s.mu.Unlock()
	if !known {
		controlBadRequest(c, "phone_number_id is not a stub account")
		return
	}
	c.entry.MediaID = item.ID
	writeJSON(c.w, http.StatusOK, map[string]any{"id": item.ID, "sha256": item.SHA256, "file_size": len(item.Data), "mime_type": item.MimeType})
}

var templateStatuses = map[string]bool{"APPROVED": true, "REJECTED": true, "PENDING": true, "PAUSED": true, "DISABLED": true}

// controlTemplateStatus plays Meta's template review: it sets the status of
// a template (every language, unless one is named) and delivers the
// message_template_status_update webhook for each.
func (s *Server) controlTemplateStatus(c *call, body []byte) {
	var request struct {
		BusinessAccountID string `json:"business_account_id"`
		Name              string `json:"name"`
		Language          string `json:"language"`
		Status            string `json:"status"`
		Reason            string `json:"reason"`
	}
	if !decodeControl(c, body, &request) {
		return
	}
	if !templateStatuses[request.Status] {
		controlBadRequest(c, "status must be APPROVED, REJECTED, PENDING, PAUSED or DISABLED")
		return
	}
	if request.Reason == "" {
		request.Reason = "NONE"
	}
	if !printableNamePattern.MatchString(request.Reason) {
		controlBadRequest(c, "reason must be 1 to 96 printable characters")
		return
	}
	c.entry.BusinessAccountID, c.entry.TemplateName = request.BusinessAccountID, request.Name
	var updated []template
	s.mu.Lock()
	if owner := s.state.wabas[request.BusinessAccountID]; owner != nil {
		for _, item := range owner.templates {
			if item.Name == request.Name && (request.Language == "" || item.Language == request.Language) {
				item.Status = request.Status
				updated = append(updated, *item)
			}
		}
	}
	s.mu.Unlock()
	if len(updated) == 0 {
		controlBadRequest(c, "no such template in that business account")
		return
	}
	statuses := make([]int, 0, len(updated))
	for _, item := range updated {
		id, _ := strconv.ParseInt(item.ID, 10, 64)
		ctx, cancel := context.WithTimeout(c.r.Context(), webhookTimeout)
		status, err := s.deliver(ctx, "", webhookEnvelope(request.BusinessAccountID, "message_template_status_update", map[string]any{
			"event":                     request.Status,
			"message_template_id":       id,
			"message_template_name":     item.Name,
			"message_template_language": item.Language,
			"reason":                    request.Reason,
		}), Entry{Route: "template_status", BusinessAccountID: request.BusinessAccountID, TemplateName: item.Name, DeliveryStatus: request.Status})
		cancel()
		if err != nil {
			c.entry.Error = "callback_unreachable"
			writeJSON(c.w, http.StatusBadGateway, map[string]any{"error": "callback delivery failed", "updated": len(updated)})
			return
		}
		statuses = append(statuses, status)
	}
	writeJSON(c.w, http.StatusOK, map[string]any{"updated": len(updated), "callback_statuses": statuses})
}

// controlOAuthCode issues a single-use Embedded Signup code that exchanges for
// one of the configured access tokens.
func (s *Server) controlOAuthCode(c *call, body []byte) {
	var request struct {
		Code        string `json:"code"`
		AccessToken string `json:"access_token"`
	}
	if !decodeControl(c, body, &request) {
		return
	}
	if !oauthCodePattern.MatchString(request.Code) {
		controlBadRequest(c, "code must be 8 to 512 URL-safe characters")
		return
	}
	if !oneOf(request.AccessToken, s.cfg.AccessTokens...) {
		controlBadRequest(c, "access_token must be one of STUB_ACCESS_TOKENS")
		return
	}
	s.mu.Lock()
	err := s.state.addCode(request.Code, request.AccessToken, s.now())
	s.mu.Unlock()
	if err != nil {
		controlBadRequest(c, err.Error())
		return
	}
	writeJSON(c.w, http.StatusOK, map[string]any{"ok": true, "expires_in": int(oauthCodeLifetime.Seconds())})
}

func (s *Server) controlAddFault(c *call, body []byte) {
	var fault Fault
	if !decodeControl(c, body, &fault) {
		return
	}
	if err := fault.normalize(); err != nil {
		controlBadRequest(c, err.Error())
		return
	}
	s.mu.Lock()
	full := len(s.faults) >= maxQueuedFaults
	if !full {
		s.faults = append(s.faults, &fault)
	}
	s.mu.Unlock()
	if full {
		controlBadRequest(c, "too many queued faults")
		return
	}
	writeJSON(c.w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) controlClearFaults(c *call, _ []byte) {
	s.mu.Lock()
	cleared := len(s.faults)
	s.faults = nil
	s.mu.Unlock()
	writeJSON(c.w, http.StatusOK, map[string]int{"cleared": cleared})
}

// controlReset returns the stub to its startup state: configured accounts
// only, no media, messages, templates, codes or faults, an empty journal, and
// no further deliveries from status sequences already scheduled.
func (s *Server) controlReset(c *call, _ []byte) {
	s.mu.Lock()
	s.state = newState(s.cfg.Accounts)
	s.faults = nil
	s.generation++
	s.mu.Unlock()
	s.journal.reset()
	writeJSON(c.w, http.StatusOK, map[string]bool{"ok": true})
}
