package gmailrelay

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/quotedprintable"
	"net/mail"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	xhtml "golang.org/x/net/html"
)

const (
	defaultMIMEMaxDepth       = 32
	defaultMIMEMaxParts       = 2048
	defaultMIMEMaxHeaderBytes = 256 << 10
	defaultMIMEMaxTextBytes   = 4 << 20
)

var (
	// ErrMIMEDepthExceeded is returned before descending beyond the configured
	// MIME nesting limit.
	ErrMIMEDepthExceeded = errors.New("gmail MIME nesting limit exceeded")
	// ErrMIMEPartLimitExceeded bounds work on messages with an excessive number
	// of MIME parts.
	ErrMIMEPartLimitExceeded = errors.New("gmail MIME part limit exceeded")
	// ErrMIMESizeExceeded bounds decoded body text and MIME header data.
	ErrMIMESizeExceeded = errors.New("gmail MIME size limit exceeded")
)

// GmailMessage is the subset of a Gmail users.messages resource required by
// ParseInboundMessage. It can be unmarshaled directly from the Gmail REST API.
type GmailMessage struct {
	ID           string        `json:"id"`
	ThreadID     string        `json:"threadId"`
	LabelIDs     []string      `json:"labelIds,omitempty"`
	InternalDate string        `json:"internalDate"`
	Payload      *GmailPayload `json:"payload"`
}

// GmailPayload models a Gmail MessagePart without downloading attachments.
type GmailPayload struct {
	PartID   string         `json:"partId"`
	MimeType string         `json:"mimeType"`
	Filename string         `json:"filename"`
	Headers  []GmailHeader  `json:"headers"`
	Body     GmailPartBody  `json:"body"`
	Parts    []GmailPayload `json:"parts,omitempty"`
}

type GmailHeader struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type GmailPartBody struct {
	AttachmentID string `json:"attachmentId,omitempty"`
	Size         int64  `json:"size,omitempty"`
	Data         string `json:"data,omitempty"`
}

// Compatibility names keep the lower-level bounded parser useful to callers
// that prefer an explicit REST prefix.
type GmailRESTMessage = GmailMessage
type GmailRESTPart = GmailPayload
type GmailRESTHeader = GmailHeader
type GmailRESTBody = GmailPartBody

// MIMEParseLimits bounds recursive parsing and allocation. Zero values select
// conservative defaults rather than disabling a limit.
type MIMEParseLimits struct {
	MaxDepth            int
	MaxParts            int
	MaxHeaderBytes      int64
	MaxDecodedTextBytes int64
}

// Mailbox is a decoded and normalized RFC 5322 mailbox. Address is lower-case
// so the same sender has a stable external identity.
type Mailbox struct {
	Name    string `json:"name,omitempty"`
	Address string `json:"address"`
}

// GmailAttachment identifies an attachment that can be fetched later through
// users.messages.attachments.get. It deliberately contains no attachment data.
type GmailAttachment struct {
	PartID             string `json:"part_id"`
	AttachmentID       string `json:"attachment_id,omitempty"`
	Filename           string `json:"filename,omitempty"`
	MIMEType           string `json:"mime_type,omitempty"`
	Size               int64  `json:"size,omitempty"`
	ContentID          string `json:"content_id,omitempty"`
	ContentDisposition string `json:"content_disposition,omitempty"`
}

// ParsedInboundEmail is a bounded, relay-friendly representation of a Gmail
// message. Text always prefers an inline text/plain part; HTML is converted to
// safe text only when no non-blank plain part exists.
type ParsedInboundEmail struct {
	ID          string
	ThreadID    string
	MessageID   string
	InReplyTo   string
	References  []string
	Subject     string
	From        Mailbox
	ReplyTo     *Mailbox
	Text        string
	HTML        string
	ReceivedAt  time.Time
	Attachments []GmailAttachment
}

type ParsedGmailMessage = ParsedInboundEmail

// ReplyRecipient returns Reply-To when present and From otherwise.
func (m ParsedInboundEmail) ReplyRecipient() Mailbox {
	if m.ReplyTo != nil && m.ReplyTo.Address != "" {
		return *m.ReplyTo
	}
	return m.From
}

// ParseInboundMessage is the relay's compact entry point. maxDepth and
// maxBytes must be positive; non-positive values select conservative defaults.
func ParseInboundMessage(message GmailMessage, maxDepth int, maxBytes int64) (ParsedInboundEmail, error) {
	return ParseGmailMessage(message, MIMEParseLimits{
		MaxDepth:            maxDepth,
		MaxDecodedTextBytes: maxBytes,
	})
}

// ParseGmailMessage parses Gmail's JSON message-part representation without
// materializing attachment bytes.
func ParseGmailMessage(message GmailRESTMessage, limits MIMEParseLimits) (ParsedGmailMessage, error) {
	limits = limits.withDefaults()
	if message.Payload == nil {
		return ParsedGmailMessage{}, errors.New("gmail message has no payload")
	}

	receivedMillis, err := strconv.ParseInt(message.InternalDate, 10, 64)
	if err != nil {
		return ParsedGmailMessage{}, fmt.Errorf("parse Gmail internalDate: %w", err)
	}

	state := mimeParseState{limits: limits}
	if err := state.walk(message.Payload, 0); err != nil {
		return ParsedGmailMessage{}, err
	}

	subject, err := decodeRFC2047(headerValue(message.Payload.Headers, "Subject"))
	if err != nil {
		return ParsedGmailMessage{}, fmt.Errorf("decode Subject: %w", err)
	}
	from, err := parseMailboxHeader(headerValue(message.Payload.Headers, "From"))
	if err != nil {
		return ParsedGmailMessage{}, fmt.Errorf("parse From: %w", err)
	}

	var replyTo *Mailbox
	if raw := headerValue(message.Payload.Headers, "Reply-To"); raw != "" {
		decoded, parseErr := parseMailboxHeader(raw)
		if parseErr != nil {
			return ParsedGmailMessage{}, fmt.Errorf("parse Reply-To: %w", parseErr)
		}
		replyTo = &decoded
	}

	textBody := normalizeTextNewlines(state.plain)
	htmlBody := normalizeTextNewlines(state.html)
	if strings.TrimSpace(textBody) == "" && htmlBody != "" {
		textBody, err = HTMLToText(htmlBody)
		if err != nil {
			return ParsedGmailMessage{}, fmt.Errorf("convert HTML body: %w", err)
		}
	}

	inReplyToIDs := extractMessageIDs(headerValue(message.Payload.Headers, "In-Reply-To"))
	inReplyTo := ""
	if len(inReplyToIDs) > 0 {
		inReplyTo = inReplyToIDs[0]
	}

	return ParsedGmailMessage{
		ID:          message.ID,
		ThreadID:    message.ThreadID,
		MessageID:   firstMessageID(headerValue(message.Payload.Headers, "Message-ID")),
		InReplyTo:   inReplyTo,
		References:  extractMessageIDs(headerValue(message.Payload.Headers, "References")),
		Subject:     subject,
		From:        from,
		ReplyTo:     replyTo,
		Text:        textBody,
		HTML:        htmlBody,
		ReceivedAt:  time.UnixMilli(receivedMillis).UTC(),
		Attachments: state.attachments,
	}, nil
}

type mimeParseState struct {
	limits       MIMEParseLimits
	parts        int
	headerBytes  int64
	decodedBytes int64
	plain        string
	html         string
	attachments  []GmailAttachment
}

func (s *mimeParseState) walk(part *GmailRESTPart, depth int) error {
	if depth > s.limits.MaxDepth {
		return ErrMIMEDepthExceeded
	}
	s.parts++
	if s.parts > s.limits.MaxParts {
		return ErrMIMEPartLimitExceeded
	}
	for _, h := range part.Headers {
		s.headerBytes += int64(len(h.Name) + len(h.Value))
		if s.headerBytes > s.limits.MaxHeaderBytes {
			return ErrMIMESizeExceeded
		}
	}

	disposition := headerValue(part.Headers, "Content-Disposition")
	isAttachment := part.Body.AttachmentID != "" || part.Filename != "" ||
		strings.EqualFold(dispositionType(disposition), "attachment")
	if isAttachment {
		filename := part.Filename
		if decoded, err := decodeRFC2047(filename); err == nil {
			filename = decoded
		}
		s.attachments = append(s.attachments, GmailAttachment{
			PartID:             part.PartID,
			AttachmentID:       part.Body.AttachmentID,
			Filename:           filename,
			MIMEType:           normalizedMediaType(part.MimeType),
			Size:               part.Body.Size,
			ContentID:          strings.Trim(headerValue(part.Headers, "Content-ID"), "<> \t"),
			ContentDisposition: disposition,
		})
	} else if part.Body.Data != "" {
		mediaType := normalizedMediaType(part.MimeType)
		if mediaType == "text/plain" || mediaType == "text/html" {
			remaining := s.limits.MaxDecodedTextBytes - s.decodedBytes
			if remaining < 0 || part.Body.Size > remaining || encodedCouldExceed(part.Body.Data, remaining) {
				return ErrMIMESizeExceeded
			}
			decoded, err := DecodeBase64URL(part.Body.Data)
			if err != nil {
				return fmt.Errorf("decode Gmail part %q: %w", part.PartID, err)
			}
			s.decodedBytes += int64(len(decoded))
			if s.decodedBytes > s.limits.MaxDecodedTextBytes {
				return ErrMIMESizeExceeded
			}
			body := strings.ToValidUTF8(string(decoded), "\uFFFD")
			if mediaType == "text/plain" && s.plain == "" && strings.TrimSpace(body) != "" {
				s.plain = body
			}
			if mediaType == "text/html" && s.html == "" && strings.TrimSpace(body) != "" {
				s.html = body
			}
		}
	}

	for i := range part.Parts {
		if err := s.walk(&part.Parts[i], depth+1); err != nil {
			return err
		}
	}
	return nil
}

func (l MIMEParseLimits) withDefaults() MIMEParseLimits {
	if l.MaxDepth <= 0 {
		l.MaxDepth = defaultMIMEMaxDepth
	}
	if l.MaxParts <= 0 {
		l.MaxParts = defaultMIMEMaxParts
	}
	if l.MaxHeaderBytes <= 0 {
		l.MaxHeaderBytes = defaultMIMEMaxHeaderBytes
	}
	if l.MaxDecodedTextBytes <= 0 {
		l.MaxDecodedTextBytes = defaultMIMEMaxTextBytes
	}
	return l
}

func encodedCouldExceed(encoded string, remaining int64) bool {
	if remaining < 0 {
		return true
	}
	// Base64 grows by 4/3. Allow one quantum for padding and exact-length
	// overestimation; the decoded length is checked again after decoding.
	maxEncoded := (remaining+2)/3*4 + 4
	return int64(len(encoded)) > maxEncoded
}

func normalizedMediaType(value string) string {
	mediaType, _, err := mime.ParseMediaType(value)
	if err == nil {
		return strings.ToLower(mediaType)
	}
	if before, _, ok := strings.Cut(value, ";"); ok {
		value = before
	}
	return strings.ToLower(strings.TrimSpace(value))
}

func dispositionType(value string) string {
	mediaType, _, err := mime.ParseMediaType(value)
	if err == nil {
		return mediaType
	}
	if before, _, ok := strings.Cut(value, ";"); ok {
		value = before
	}
	return strings.TrimSpace(value)
}

func headerValue(headers []GmailRESTHeader, name string) string {
	for _, h := range headers {
		if strings.EqualFold(h.Name, name) {
			return strings.TrimSpace(h.Value)
		}
	}
	return ""
}

func decodeRFC2047(value string) (string, error) {
	if value == "" {
		return "", nil
	}
	decoded, err := new(mime.WordDecoder).DecodeHeader(value)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(decoded), nil
}

func parseMailboxHeader(value string) (Mailbox, error) {
	decoded, err := decodeRFC2047(value)
	if err != nil {
		return Mailbox{}, err
	}
	if decoded == "" {
		return Mailbox{}, errors.New("mailbox header is empty")
	}
	addresses, err := mail.ParseAddressList(decoded)
	if err != nil || len(addresses) == 0 {
		if err == nil {
			err = errors.New("mailbox header contains no address")
		}
		return Mailbox{}, err
	}
	return normalizeMailbox(Mailbox{Name: addresses[0].Name, Address: addresses[0].Address}), nil
}

func normalizeMailbox(mailbox Mailbox) Mailbox {
	mailbox.Name = strings.TrimSpace(mailbox.Name)
	mailbox.Address = strings.ToLower(strings.TrimSpace(mailbox.Address))
	return mailbox
}

var messageIDPattern = regexp.MustCompile(`<[^<>\s]+@[^<>\s]+>`)

func extractMessageIDs(value string) []string {
	return messageIDPattern.FindAllString(value, -1)
}

func firstMessageID(value string) string {
	ids := extractMessageIDs(value)
	if len(ids) == 0 {
		return ""
	}
	return ids[0]
}

// HTMLToText returns visible text from an HTML email. Script, style, template,
// head, and noscript content is omitted; tags and active content never survive.
func HTMLToText(value string) (string, error) {
	doc, err := xhtml.Parse(strings.NewReader(value))
	if err != nil {
		return "", err
	}
	renderer := htmlTextRenderer{}
	renderer.walk(doc, false)
	return renderer.String(), nil
}

type htmlTextRenderer struct {
	value []byte
}

func (r *htmlTextRenderer) walk(node *xhtml.Node, hidden bool) {
	if node.Type == xhtml.ElementNode {
		switch strings.ToLower(node.Data) {
		case "script", "style", "template", "head", "noscript":
			hidden = true
		case "br", "hr":
			if !hidden {
				r.breakLine()
			}
		}
	}
	if node.Type == xhtml.TextNode && !hidden {
		r.writeText(node.Data)
	}
	for child := node.FirstChild; child != nil; child = child.NextSibling {
		r.walk(child, hidden)
	}
	if node.Type == xhtml.ElementNode && !hidden && isBlockElement(node.Data) {
		r.breakLine()
	}
}

func (r *htmlTextRenderer) writeText(value string) {
	words := strings.Fields(value)
	for _, word := range words {
		if len(r.value) > 0 && r.value[len(r.value)-1] != '\n' && r.value[len(r.value)-1] != ' ' {
			r.value = append(r.value, ' ')
		}
		r.value = append(r.value, word...)
	}
}

func (r *htmlTextRenderer) breakLine() {
	for len(r.value) > 0 && (r.value[len(r.value)-1] == ' ' || r.value[len(r.value)-1] == '\t') {
		r.value = r.value[:len(r.value)-1]
	}
	if len(r.value) > 0 && (len(r.value) < 2 || r.value[len(r.value)-2] != '\n' || r.value[len(r.value)-1] != '\n') {
		r.value = append(r.value, '\n')
	}
}

func (r *htmlTextRenderer) String() string {
	lines := strings.Split(normalizeTextNewlines(string(r.value)), "\n")
	for i := range lines {
		lines[i] = strings.TrimSpace(lines[i])
	}
	var output []string
	blank := false
	for _, line := range lines {
		if line == "" {
			if len(output) > 0 && !blank {
				output = append(output, "")
			}
			blank = true
			continue
		}
		output = append(output, line)
		blank = false
	}
	return strings.TrimSpace(strings.Join(output, "\n"))
}

func isBlockElement(name string) bool {
	switch strings.ToLower(name) {
	case "address", "article", "aside", "blockquote", "div", "dl", "fieldset", "figcaption", "figure", "footer", "form", "h1", "h2", "h3", "h4", "h5", "h6", "header", "li", "main", "nav", "ol", "p", "pre", "section", "table", "tr", "ul":
		return true
	default:
		return false
	}
}

// ReplyInput describes a raw RFC 2822 reply accepted by Gmail messages.send.
// MessageID must be deterministic for safe retries, or IdempotencyKey and
// MessageIDDomain can be supplied to derive one.
type ReplyInput struct {
	From            Mailbox
	To              []Mailbox
	Cc              []Mailbox
	Subject         string
	Date            time.Time
	MessageID       string
	IdempotencyKey  string
	MessageIDDomain string
	InReplyTo       string
	References      []string
	Text            string
	HTML            string
}

type ReplyOptions = ReplyInput

// BuildReply is the relay-facing name for BuildGmailReply.
func BuildReply(input ReplyInput) ([]byte, error) {
	return BuildGmailReply(input)
}

// DeterministicMessageID derives a stable, non-reversible RFC Message-ID from
// a relay idempotency key.
func DeterministicMessageID(idempotencyKey, domain string) (string, error) {
	domain = strings.ToLower(strings.TrimSpace(domain))
	if idempotencyKey == "" {
		return "", errors.New("idempotency key is required")
	}
	if domain == "" || hasHeaderControl(domain) || strings.ContainsAny(domain, " <>@/\\") {
		return "", errors.New("invalid Message-ID domain")
	}
	digest := sha256.Sum256([]byte(idempotencyKey))
	return fmt.Sprintf("<rereply.%x@%s>", digest[:], domain), nil
}

// BuildGmailReply builds an RFC 2822 message with stable MIME boundaries and a
// deterministic Message-ID. The returned bytes can be passed to
// EncodeBase64URL and then Gmail users.messages.send.
func BuildGmailReply(options ReplyOptions) ([]byte, error) {
	if err := validateHeaderValue("Subject", options.Subject); err != nil {
		return nil, err
	}
	from, err := formatMailbox(options.From)
	if err != nil {
		return nil, fmt.Errorf("From: %w", err)
	}
	to, err := formatMailboxList(options.To)
	if err != nil {
		return nil, fmt.Errorf("To: %w", err)
	}
	if to == "" {
		return nil, errors.New("at least one To recipient is required")
	}
	cc, err := formatMailboxList(options.Cc)
	if err != nil {
		return nil, fmt.Errorf("Cc: %w", err)
	}

	messageID := strings.TrimSpace(options.MessageID)
	if messageID == "" {
		messageID, err = DeterministicMessageID(options.IdempotencyKey, options.MessageIDDomain)
		if err != nil {
			return nil, fmt.Errorf("Message-ID: %w", err)
		}
	}
	messageID, err = canonicalMessageID(messageID)
	if err != nil {
		return nil, fmt.Errorf("Message-ID: %w", err)
	}

	inReplyTo := ""
	if strings.TrimSpace(options.InReplyTo) != "" {
		inReplyTo, err = canonicalMessageID(options.InReplyTo)
		if err != nil {
			return nil, fmt.Errorf("In-Reply-To: %w", err)
		}
	}
	references := make([]string, 0, len(options.References)+1)
	seenReferences := make(map[string]struct{}, len(options.References)+1)
	for _, raw := range options.References {
		id, referenceErr := canonicalMessageID(raw)
		if referenceErr != nil {
			return nil, fmt.Errorf("References: %w", referenceErr)
		}
		if _, exists := seenReferences[id]; !exists {
			references = append(references, id)
			seenReferences[id] = struct{}{}
		}
	}
	if inReplyTo != "" {
		if _, exists := seenReferences[inReplyTo]; !exists {
			references = append(references, inReplyTo)
		}
	}

	date := options.Date
	if date.IsZero() {
		date = time.Now()
	}
	textBody := options.Text
	if strings.TrimSpace(textBody) == "" && strings.TrimSpace(options.HTML) != "" {
		textBody, err = HTMLToText(options.HTML)
		if err != nil {
			return nil, fmt.Errorf("derive plain-text body: %w", err)
		}
	}

	var raw bytes.Buffer
	writeRFCHeader(&raw, "From", from)
	writeRFCHeader(&raw, "To", to)
	if cc != "" {
		writeRFCHeader(&raw, "Cc", cc)
	}
	writeRFCHeader(&raw, "Subject", encodeHeaderText(options.Subject))
	writeRFCHeader(&raw, "Date", date.Format(time.RFC1123Z))
	writeRFCHeader(&raw, "Message-ID", messageID)
	if inReplyTo != "" {
		writeRFCHeader(&raw, "In-Reply-To", inReplyTo)
	}
	if len(references) > 0 {
		writeRFCHeader(&raw, "References", strings.Join(references, " "))
	}
	writeRFCHeader(&raw, "MIME-Version", "1.0")

	if strings.TrimSpace(options.HTML) == "" {
		writeRFCHeader(&raw, "Content-Type", "text/plain; charset=UTF-8")
		writeRFCHeader(&raw, "Content-Transfer-Encoding", "quoted-printable")
		raw.WriteString("\r\n")
		if err := writeQuotedPrintable(&raw, textBody); err != nil {
			return nil, err
		}
		return raw.Bytes(), nil
	}

	boundarySeed := sha256.Sum256([]byte(messageID + "\x00" + textBody + "\x00" + options.HTML))
	boundary := fmt.Sprintf("rereply_%x", boundarySeed[:16])
	writeRFCHeader(&raw, "Content-Type", fmt.Sprintf("multipart/alternative; boundary=\"%s\"", boundary))
	raw.WriteString("\r\n")

	if err := writeMIMEAlternativePart(&raw, boundary, "text/plain; charset=UTF-8", textBody); err != nil {
		return nil, err
	}
	if err := writeMIMEAlternativePart(&raw, boundary, "text/html; charset=UTF-8", options.HTML); err != nil {
		return nil, err
	}
	fmt.Fprintf(&raw, "--%s--\r\n", boundary)
	return raw.Bytes(), nil
}

func writeMIMEAlternativePart(output *bytes.Buffer, boundary, contentType, body string) error {
	fmt.Fprintf(output, "--%s\r\n", boundary)
	writeRFCHeader(output, "Content-Type", contentType)
	writeRFCHeader(output, "Content-Transfer-Encoding", "quoted-printable")
	output.WriteString("\r\n")
	return writeQuotedPrintable(output, body)
}

func writeQuotedPrintable(output io.Writer, value string) error {
	var encoded bytes.Buffer
	writer := quotedprintable.NewWriter(&encoded)
	if _, err := io.WriteString(writer, normalizeCRLF(value)); err != nil {
		return fmt.Errorf("encode quoted-printable body: %w", err)
	}
	if err := writer.Close(); err != nil {
		return fmt.Errorf("close quoted-printable body: %w", err)
	}
	if !bytes.HasSuffix(encoded.Bytes(), []byte("\r\n")) {
		encoded.WriteString("\r\n")
	}
	_, err := io.Copy(output, &encoded)
	return err
}

func writeRFCHeader(output *bytes.Buffer, name, value string) {
	fmt.Fprintf(output, "%s: %s\r\n", name, value)
}

func formatMailboxList(mailboxes []Mailbox) (string, error) {
	formatted := make([]string, 0, len(mailboxes))
	for _, mailbox := range mailboxes {
		value, err := formatMailbox(mailbox)
		if err != nil {
			return "", err
		}
		formatted = append(formatted, value)
	}
	return strings.Join(formatted, ", "), nil
}

func formatMailbox(mailbox Mailbox) (string, error) {
	if err := validateHeaderValue("display name", mailbox.Name); err != nil {
		return "", err
	}
	if err := validateHeaderValue("address", mailbox.Address); err != nil {
		return "", err
	}
	parsed, err := mail.ParseAddress(strings.TrimSpace(mailbox.Address))
	if err != nil {
		return "", err
	}
	if parsed.Name != "" || !strings.EqualFold(parsed.Address, strings.TrimSpace(mailbox.Address)) {
		return "", errors.New("address must not contain a display name")
	}
	return (&mail.Address{Name: strings.TrimSpace(mailbox.Name), Address: parsed.Address}).String(), nil
}

func canonicalMessageID(value string) (string, error) {
	value = strings.TrimSpace(value)
	if hasHeaderControl(value) || !messageIDPattern.MatchString(value) || messageIDPattern.FindString(value) != value {
		return "", errors.New("invalid RFC Message-ID")
	}
	return value, nil
}

func validateHeaderValue(name, value string) error {
	if hasHeaderControl(value) {
		return fmt.Errorf("%s contains a control character", name)
	}
	return nil
}

func hasHeaderControl(value string) bool {
	for _, r := range value {
		if r < 32 || r == 127 {
			return true
		}
	}
	return false
}

func encodeHeaderText(value string) string {
	if value == "" || isASCIIHeaderText(value) {
		return value
	}
	return mime.QEncoding.Encode("UTF-8", value)
}

func isASCIIHeaderText(value string) bool {
	for _, r := range value {
		if r > 127 {
			return false
		}
	}
	return true
}

func normalizeTextNewlines(value string) string {
	value = strings.ReplaceAll(value, "\r\n", "\n")
	return strings.ReplaceAll(value, "\r", "\n")
}

func normalizeCRLF(value string) string {
	return strings.ReplaceAll(normalizeTextNewlines(value), "\n", "\r\n")
}

// EncodeBase64URL encodes bytes using Gmail's unpadded URL-safe base64 form.
func EncodeBase64URL(value []byte) string {
	return base64.RawURLEncoding.EncodeToString(value)
}

// DecodeBase64URL accepts both padded and unpadded URL-safe base64 returned by
// Gmail REST endpoints.
func DecodeBase64URL(value string) ([]byte, error) {
	if value == "" {
		return []byte{}, nil
	}
	if !utf8.ValidString(value) {
		return nil, errors.New("base64url input is not valid UTF-8")
	}
	if strings.Contains(value, "=") {
		return base64.URLEncoding.DecodeString(value)
	}
	return base64.RawURLEncoding.DecodeString(value)
}
