package gmailrelay

import (
	"bytes"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"strings"
	"testing"
	"time"
)

func TestParseGmailMessageNestedMultipartPrefersPlainText(t *testing.T) {
	unicodeName := mime.BEncoding.Encode("UTF-8", "Zoë Lim")
	unicodeSubject := mime.QEncoding.Encode("UTF-8", "Temujanji fisioterapi ✓")
	message := GmailRESTMessage{
		ID:           "gmail-message-1",
		ThreadID:     "gmail-thread-1",
		InternalDate: "1710000000123",
		Payload: &GmailRESTPart{
			MimeType: "multipart/mixed",
			Headers: []GmailRESTHeader{
				{Name: "From", Value: unicodeName + " <ZOE.LIM@Example.COM>"},
				{Name: "Reply-To", Value: "Bookings <BOOKINGS@EXAMPLE.COM>"},
				{Name: "Subject", Value: unicodeSubject},
				{Name: "Message-ID", Value: "<incoming-1@example.com>"},
				{Name: "In-Reply-To", Value: "<previous@example.com>"},
				{Name: "References", Value: "<root@example.com> <previous@example.com>"},
			},
			Parts: []GmailRESTPart{
				{
					PartID:   "0",
					MimeType: "multipart/alternative",
					Parts: []GmailRESTPart{
						{
							PartID:   "0.0",
							MimeType: "text/html; charset=UTF-8",
							Body:     GmailRESTBody{Data: EncodeBase64URL([]byte("<p>HTML should not win</p>")), Size: 26},
						},
						{
							PartID:   "0.1",
							MimeType: "text/plain; charset=UTF-8",
							Body:     GmailRESTBody{Data: EncodeBase64URL([]byte("Hello from plain text\r\n")), Size: 23},
						},
					},
				},
				{
					PartID:   "1",
					MimeType: "application/pdf",
					Filename: mime.BEncoding.Encode("UTF-8", "laporan résumé.pdf"),
					Headers: []GmailRESTHeader{
						{Name: "Content-Disposition", Value: "attachment"},
						{Name: "Content-ID", Value: "<document-1>"},
					},
					// Deliberately invalid data: attachment bytes must never be decoded here.
					Body: GmailRESTBody{AttachmentID: "attachment-1", Data: "not base64!", Size: 999},
				},
			},
		},
	}

	parsed, err := ParseGmailMessage(message, MIMEParseLimits{})
	if err != nil {
		t.Fatalf("ParseGmailMessage() error = %v", err)
	}
	if parsed.Subject != "Temujanji fisioterapi ✓" {
		t.Fatalf("Subject = %q", parsed.Subject)
	}
	if parsed.From.Name != "Zoë Lim" || parsed.From.Address != "zoe.lim@example.com" {
		t.Fatalf("From = %#v", parsed.From)
	}
	if got := parsed.ReplyRecipient(); got.Address != "bookings@example.com" || got.Name != "Bookings" {
		t.Fatalf("ReplyRecipient() = %#v", got)
	}
	if parsed.Text != "Hello from plain text\n" {
		t.Fatalf("Text = %q", parsed.Text)
	}
	if parsed.HTML != "<p>HTML should not win</p>" {
		t.Fatalf("HTML = %q", parsed.HTML)
	}
	if parsed.MessageID != "<incoming-1@example.com>" || parsed.InReplyTo != "<previous@example.com>" {
		t.Fatalf("thread headers = Message-ID %q, In-Reply-To %q", parsed.MessageID, parsed.InReplyTo)
	}
	if len(parsed.References) != 2 || parsed.References[0] != "<root@example.com>" {
		t.Fatalf("References = %#v", parsed.References)
	}
	wantReceived := time.UnixMilli(1710000000123).UTC()
	if !parsed.ReceivedAt.Equal(wantReceived) {
		t.Fatalf("ReceivedAt = %s, want %s", parsed.ReceivedAt, wantReceived)
	}
	if len(parsed.Attachments) != 1 {
		t.Fatalf("Attachments = %#v", parsed.Attachments)
	}
	attachment := parsed.Attachments[0]
	if attachment.AttachmentID != "attachment-1" || attachment.Filename != "laporan résumé.pdf" || attachment.ContentID != "document-1" || attachment.Size != 999 {
		t.Fatalf("attachment = %#v", attachment)
	}
}

func TestParseGmailMessageFallsBackToSafeHTMLText(t *testing.T) {
	htmlBody := `<html><head><style>.secret{display:block}</style></head><body><p>Hello <b>world</b> &amp; team.</p><script>alert("no")</script><div>Second line</div></body></html>`
	message := GmailRESTMessage{
		ID:           "message-html",
		ThreadID:     "thread-html",
		InternalDate: "1710000000000",
		Payload: &GmailRESTPart{
			MimeType: "text/html",
			Headers:  []GmailRESTHeader{{Name: "From", Value: "sender@example.com"}},
			Body: GmailRESTBody{
				Data: EncodeBase64URL([]byte(htmlBody)),
				Size: int64(len(htmlBody)),
			},
		},
	}

	parsed, err := ParseGmailMessage(message, MIMEParseLimits{})
	if err != nil {
		t.Fatalf("ParseGmailMessage() error = %v", err)
	}
	if parsed.Text != "Hello world & team.\nSecond line" {
		t.Fatalf("safe HTML text = %q", parsed.Text)
	}
	if strings.Contains(parsed.Text, "alert") || strings.Contains(parsed.Text, "secret") || strings.Contains(parsed.Text, "<") {
		t.Fatalf("unsafe content survived: %q", parsed.Text)
	}
}

func TestParseGmailMessageEnforcesDepthAndDecodedSize(t *testing.T) {
	nested := GmailRESTPart{MimeType: "multipart/mixed", Parts: []GmailRESTPart{{
		MimeType: "multipart/mixed", Parts: []GmailRESTPart{{
			MimeType: "text/plain", Body: GmailRESTBody{Data: EncodeBase64URL([]byte("body")), Size: 4},
		}},
	}}}
	message := GmailRESTMessage{
		InternalDate: "1710000000000",
		Payload: &GmailRESTPart{
			MimeType: "multipart/mixed",
			Headers:  []GmailRESTHeader{{Name: "From", Value: "sender@example.com"}},
			Parts:    []GmailRESTPart{nested},
		},
	}
	if _, err := ParseGmailMessage(message, MIMEParseLimits{MaxDepth: 2}); !errors.Is(err, ErrMIMEDepthExceeded) {
		t.Fatalf("depth error = %v, want %v", err, ErrMIMEDepthExceeded)
	}

	message.Payload.Parts = []GmailRESTPart{{
		MimeType: "text/plain",
		Body:     GmailRESTBody{Data: EncodeBase64URL([]byte("too large")), Size: 9},
	}}
	if _, err := ParseGmailMessage(message, MIMEParseLimits{MaxDecodedTextBytes: 4}); !errors.Is(err, ErrMIMESizeExceeded) {
		t.Fatalf("size error = %v, want %v", err, ErrMIMESizeExceeded)
	}
}

func TestBuildGmailReplyUnicodeAndThreading(t *testing.T) {
	date := time.Date(2026, time.August, 3, 9, 30, 0, 0, time.FixedZone("MYT", 8*60*60))
	options := ReplyOptions{
		From:            Mailbox{Name: "Klinik Réalign", Address: "realignphysiolates@gmail.com"},
		To:              []Mailbox{{Name: "Zoë Lim", Address: "zoe@example.com"}},
		Cc:              []Mailbox{{Name: "Dr Åsa", Address: "asa@example.com"}},
		Subject:         "Re: Temujanji fisioterapi ✓",
		Date:            date,
		IdempotencyKey:  "outbox-42",
		MessageIDDomain: "rereply.app",
		InReplyTo:       "<incoming-2@example.com>",
		References:      []string{"<root@example.com>", "<incoming-2@example.com>"},
		Text:            "Terima kasih, Zoë.\nJumpa nanti.",
		HTML:            "<p>Terima kasih, <strong>Zoë</strong>.</p><p>Jumpa nanti.</p>",
	}

	raw, err := BuildGmailReply(options)
	if err != nil {
		t.Fatalf("BuildGmailReply() error = %v", err)
	}
	rawAgain, err := BuildGmailReply(options)
	if err != nil {
		t.Fatalf("second BuildGmailReply() error = %v", err)
	}
	if !bytes.Equal(raw, rawAgain) {
		t.Fatal("same inputs did not produce deterministic raw MIME")
	}
	if bytes.Contains(raw, []byte("\n")) && bytes.Contains(bytes.ReplaceAll(raw, []byte("\r\n"), nil), []byte("\n")) {
		t.Fatal("raw reply contains a bare LF")
	}

	message, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("mail.ReadMessage() error = %v\n%s", err, raw)
	}
	decodedSubject, err := new(mime.WordDecoder).DecodeHeader(message.Header.Get("Subject"))
	if err != nil || decodedSubject != options.Subject {
		t.Fatalf("decoded Subject = %q, error = %v", decodedSubject, err)
	}
	from, err := mail.ParseAddress(message.Header.Get("From"))
	if err != nil || from.Name != "Klinik Réalign" {
		t.Fatalf("parsed From = %#v, error = %v", from, err)
	}
	messageID, err := DeterministicMessageID("outbox-42", "rereply.app")
	if err != nil {
		t.Fatal(err)
	}
	if message.Header.Get("Message-ID") != messageID {
		t.Fatalf("Message-ID = %q, want %q", message.Header.Get("Message-ID"), messageID)
	}
	if message.Header.Get("In-Reply-To") != "<incoming-2@example.com>" {
		t.Fatalf("In-Reply-To = %q", message.Header.Get("In-Reply-To"))
	}
	if message.Header.Get("References") != "<root@example.com> <incoming-2@example.com>" {
		t.Fatalf("References = %q", message.Header.Get("References"))
	}

	mediaType, params, err := mime.ParseMediaType(message.Header.Get("Content-Type"))
	if err != nil || mediaType != "multipart/alternative" {
		t.Fatalf("Content-Type = %q, params = %#v, error = %v", mediaType, params, err)
	}
	reader := multipart.NewReader(message.Body, params["boundary"])
	var bodies []string
	for {
		part, partErr := reader.NextPart()
		if errors.Is(partErr, io.EOF) {
			break
		}
		if partErr != nil {
			t.Fatalf("NextPart() error = %v", partErr)
		}
		decoded, readErr := io.ReadAll(quotedprintable.NewReader(part))
		if readErr != nil {
			t.Fatalf("read MIME part: %v", readErr)
		}
		bodies = append(bodies, string(decoded))
	}
	if len(bodies) != 2 || !strings.Contains(bodies[0], "Terima kasih, Zoë.") || !strings.Contains(bodies[1], "<strong>Zoë</strong>") {
		t.Fatalf("multipart bodies = %#v", bodies)
	}
}

func TestBuildGmailReplyRejectsHeaderInjection(t *testing.T) {
	base := ReplyOptions{
		From:      Mailbox{Name: "Clinic", Address: "clinic@example.com"},
		To:        []Mailbox{{Address: "patient@example.com"}},
		Subject:   "Hello",
		Date:      time.Unix(1, 0).UTC(),
		MessageID: "<reply@example.com>",
		Text:      "Body",
	}
	tests := []struct {
		name   string
		mutate func(*ReplyOptions)
	}{
		{name: "subject CRLF", mutate: func(o *ReplyOptions) { o.Subject = "Hello\r\nBcc: attacker@example.com" }},
		{name: "from name newline", mutate: func(o *ReplyOptions) { o.From.Name = "Clinic\nBcc: attacker@example.com" }},
		{name: "recipient newline", mutate: func(o *ReplyOptions) { o.To[0].Address = "patient@example.com\r\nBcc: attacker@example.com" }},
		{name: "message ID newline", mutate: func(o *ReplyOptions) { o.MessageID = "<reply@example.com>\r\nBcc: attacker@example.com" }},
		{name: "thread reference newline", mutate: func(o *ReplyOptions) { o.References = []string{"<root@example.com>\nBcc: attacker@example.com"} }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			options := base
			options.To = append([]Mailbox(nil), base.To...)
			test.mutate(&options)
			if _, err := BuildGmailReply(options); err == nil {
				t.Fatal("BuildGmailReply() accepted injected header")
			}
		})
	}
}

func TestBase64URLHelpersAcceptPaddedAndUnpadded(t *testing.T) {
	original := []byte("Gmail raw MIME: ✓")
	encoded := EncodeBase64URL(original)
	decoded, err := DecodeBase64URL(encoded)
	if err != nil || !bytes.Equal(decoded, original) {
		t.Fatalf("unpadded round trip = %q, %v", decoded, err)
	}
	padded := encoded + strings.Repeat("=", (4-len(encoded)%4)%4)
	decoded, err = DecodeBase64URL(padded)
	if err != nil || !bytes.Equal(decoded, original) {
		t.Fatalf("padded round trip = %q, %v", decoded, err)
	}
}
