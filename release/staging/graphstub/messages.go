package graphstub

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

var (
	recipientPattern = regexp.MustCompile(`^[A-Za-z0-9.:+_@-]{1,64}$`)
	mimeTypePattern  = regexp.MustCompile(`^[a-z0-9][a-z0-9!#$&^_.+-]{0,63}/[a-z0-9][a-z0-9!#$&^_.+-]{0,63}$`)
)

var mediaMessageTypes = map[string]bool{"image": true, "audio": true, "document": true, "video": true, "sticker": true}

var messageTypes = map[string]bool{
	"text": true, "template": true, "interactive": true, "reaction": true, "location": true, "contacts": true,
	"image": true, "audio": true, "document": true, "video": true, "sticker": true,
}

// sendMessage accepts POST /{phone}/messages the way Cloud API does: a send
// is answered with a new WAMID and then progresses through the configured
// status webhooks; a read receipt (status=read) is acknowledged only.
func (s *Server) sendMessage(c *call) {
	phoneID := c.segments[0]
	c.entry.PhoneNumberID = phoneID
	var request map[string]json.RawMessage
	if !decodeJSON(c, &request) {
		return
	}
	text := func(key string) string {
		var value string
		_ = json.Unmarshal(request[key], &value)
		return value
	}
	if text("messaging_product") != "whatsapp" {
		invalidParameter(c, "The parameter messaging_product is required")
		return
	}
	if text("status") == "read" {
		c.entry.MessageType = "read_receipt"
		c.entry.MessageID = text("message_id")
		if c.entry.MessageID == "" {
			invalidParameter(c, "The parameter message_id is required")
			return
		}
		writeJSON(c.w, http.StatusOK, map[string]bool{"success": true})
		return
	}

	recipient := text("to")
	if recipient == "" {
		recipient = text("recipient")
	}
	messageType := text("type")
	if messageType == "" {
		messageType = "text"
	}
	c.entry.To, c.entry.MessageType = recipient, messageType
	if !recipientPattern.MatchString(recipient) {
		invalidParameter(c, "The parameter to is required")
		return
	}
	if !messageTypes[messageType] {
		invalidParameter(c, "Param type is not a supported message type")
		return
	}
	content := map[string]json.RawMessage{}
	if messageType == "contacts" {
		var contacts []json.RawMessage
		if err := json.Unmarshal(request[messageType], &contacts); err != nil || len(contacts) == 0 {
			invalidParameter(c, "The parameter contacts is required")
			return
		}
	} else if err := json.Unmarshal(request[messageType], &content); err != nil || content == nil {
		invalidParameter(c, "The parameter "+messageType+" is required")
		return
	}
	field := func(key string) string {
		var value string
		_ = json.Unmarshal(content[key], &value)
		return value
	}

	account, message, generation, accepted := s.acceptMessage(c, phoneID, recipient, messageType, content, field)
	if !accepted {
		return
	}
	c.entry.MessageID = message.ID
	if !s.scheduleStatuses(account, message, generation) {
		c.entry.Error = "statuses_not_scheduled"
	}
	writeJSON(c.w, http.StatusOK, map[string]any{
		"messaging_product": "whatsapp",
		"contacts":          []any{map[string]string{"input": recipient, "wa_id": recipient}},
		"messages":          []any{map[string]string{"id": message.ID}},
	})
}

// acceptMessage checks the send against the stub's state and records it. It
// writes the Graph error itself when it refuses.
func (s *Server) acceptMessage(
	c *call,
	phoneID, recipient, messageType string,
	content map[string]json.RawMessage,
	field func(string) string,
) (Account, sentMessage, uint64, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	item := s.state.phones[phoneID]
	if item == nil {
		unsupported(c)
		return Account{}, sentMessage{}, 0, false
	}
	account := item.account
	c.entry.BusinessAccountID = account.BusinessAccountID
	switch {
	case messageType == "text":
		body := field("body")
		if body == "" || utf8.RuneCountInString(body) > 4096 {
			invalidParameter(c, "Param text.body must be 1 to 4096 characters")
			return Account{}, sentMessage{}, 0, false
		}
		sum := sha256.Sum256([]byte(body))
		c.entry.TextSHA256 = hex.EncodeToString(sum[:])
	case mediaMessageTypes[messageType]:
		mediaID, link := field("id"), field("link")
		c.entry.MediaID = mediaID
		switch {
		case mediaID != "":
			if s.state.media[mediaID] == nil {
				graphError(c, http.StatusBadRequest, 131053, 0, "OAuthException", "(#131053) Media upload error: the media ID is unknown", false)
				return Account{}, sentMessage{}, 0, false
			}
		case link == "":
			invalidParameter(c, "The parameter "+messageType+".id or "+messageType+".link is required")
			return Account{}, sentMessage{}, 0, false
		}
	case messageType == "template":
		var language struct {
			Code string `json:"code"`
		}
		_ = json.Unmarshal(content["language"], &language)
		c.entry.TemplateName = field("name")
		if !s.templateApproved(account.BusinessAccountID, c.entry.TemplateName, language.Code) {
			graphError(c, http.StatusNotFound, 132001, 0, "OAuthException",
				"(#132001) Template name does not exist in the translation", false)
			return Account{}, sentMessage{}, 0, false
		}
	case messageType == "reaction":
		if field("message_id") == "" {
			invalidParameter(c, "The parameter reaction.message_id is required")
			return Account{}, sentMessage{}, 0, false
		}
	}
	message := sentMessage{ID: newMessageID(), PhoneID: phoneID, To: recipient}
	s.state.recordMessage(message)
	return account, message, s.generation, true
}

func (s *Server) templateApproved(wabaID, name, language string) bool {
	owner := s.state.wabas[wabaID]
	if owner == nil {
		return false
	}
	for _, item := range owner.templates {
		if item.Name == name && item.Language == language && item.Status == "APPROVED" {
			return true
		}
	}
	return false
}

// uploadMedia accepts the multipart upload pkg/whatsapp sends to
// POST /{phone}/media and returns a media ID.
func (s *Server) uploadMedia(c *call) {
	phoneID := c.segments[0]
	c.entry.PhoneNumberID = phoneID
	body, ok := readBody(c, maxMediaBytes+64<<10)
	if !ok {
		return
	}
	mediaType, params, err := mime.ParseMediaType(c.r.Header.Get("Content-Type"))
	if err != nil || mediaType != "multipart/form-data" || params["boundary"] == "" {
		invalidParameter(c, "The request must be multipart/form-data")
		return
	}
	reader := multipart.NewReader(bytes.NewReader(body), params["boundary"])
	product, fileType := "", ""
	var data []byte
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			invalidParameter(c, "The multipart body is malformed")
			return
		}
		value, err := io.ReadAll(io.LimitReader(part, maxMediaBytes+1))
		if err != nil || len(value) > maxMediaBytes {
			graphError(c, http.StatusBadRequest, 131052, 0, "OAuthException", "(#131052) Media file is too large", false)
			return
		}
		switch part.FormName() {
		case "messaging_product":
			product = string(value)
		case "file":
			data = value
			fileType = strings.ToLower(strings.TrimSpace(part.Header.Get("Content-Type")))
		case "type":
			if fileType == "" {
				fileType = strings.ToLower(strings.TrimSpace(string(value)))
			}
		}
	}
	if product != "whatsapp" {
		invalidParameter(c, "The parameter messaging_product is required")
		return
	}
	if len(data) == 0 || !mimeTypePattern.MatchString(fileType) {
		invalidParameter(c, "The parameter file is required")
		return
	}
	s.mu.Lock()
	known := s.state.phones[phoneID] != nil
	var item *media
	if known {
		item = newMedia(phoneID, fileType, data)
		s.state.storeMedia(item)
	}
	s.mu.Unlock()
	if !known {
		unsupported(c)
		return
	}
	c.entry.MediaID = item.ID
	writeJSON(c.w, http.StatusOK, map[string]string{"id": item.ID})
}

func newMedia(phoneID, mimeType string, data []byte) *media {
	sum := sha256.Sum256(data)
	return &media{
		ID:       newGraphID(),
		PhoneID:  phoneID,
		MimeType: mimeType,
		SHA256:   hex.EncodeToString(sum[:]),
		Data:     append([]byte(nil), data...),
	}
}

// getMedia answers GET /{media_id} with a download URL on the origin the
// caller used (scheme included, see requestOrigin), which is what pkg/whatsapp
// requires of a non-Meta Graph base (validatedMediaDownloadURL) before it
// sends its bearer there.
func (s *Server) getMedia(c *call) {
	id := c.segments[0]
	c.entry.MediaID = id
	s.mu.Lock()
	item := s.state.media[id]
	s.mu.Unlock()
	if item == nil {
		unsupported(c)
		return
	}
	scheme, host := requestOrigin(c.r)
	download := url.URL{Scheme: scheme, Host: host, Path: "/" + mediaDownloadSegment + "/" + item.ID}
	writeJSON(c.w, http.StatusOK, map[string]any{
		"messaging_product": "whatsapp",
		"id":                item.ID,
		"url":               download.String(),
		"mime_type":         item.MimeType,
		"sha256":            item.SHA256,
		"file_size":         len(item.Data),
	})
}

func (s *Server) deleteMedia(c *call) {
	id := c.segments[0]
	c.entry.MediaID = id
	s.mu.Lock()
	deleted := s.state.deleteMedia(id)
	s.mu.Unlock()
	if !deleted {
		unsupported(c)
		return
	}
	writeJSON(c.w, http.StatusOK, map[string]bool{"success": true})
}

func (s *Server) downloadMedia(c *call) {
	id := c.segments[1]
	c.entry.MediaID = id
	s.mu.Lock()
	item := s.state.media[id]
	s.mu.Unlock()
	if item == nil {
		c.entry.Error = "unknown_media"
		writeJSON(c.w, http.StatusNotFound, map[string]string{"error": "media not found"})
		return
	}
	c.w.Header().Set("Content-Type", item.MimeType)
	c.w.Header().Set("Content-Length", strconv.Itoa(len(item.Data)))
	c.w.Header().Set("Cache-Control", "no-store")
	c.w.WriteHeader(http.StatusOK)
	_, _ = c.w.Write(item.Data)
}
