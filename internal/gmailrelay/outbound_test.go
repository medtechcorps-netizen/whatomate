package gmailrelay

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/quotedprintable"
	"net/http"
	"net/http/httptest"
	"net/mail"
	"strconv"
	"strings"
	"testing"
	"time"

	channelapi "github.com/shridarpatil/whatomate/internal/channel"
	"github.com/shridarpatil/whatomate/internal/models"
)

type outboundContractGmail struct {
	thread      GmailThread
	threadErr   error
	search      func(call int, query string, maxResults int) (GmailMessageList, error)
	send        func(threadID string, raw []byte) (GmailSendResponse, error)
	threadCalls []string
	searchCalls []struct {
		query      string
		maxResults int
	}
	sendCalls []struct {
		threadID string
		raw      []byte
	}
	sequence []string
}

func (g *outboundContractGmail) GetThread(_ context.Context, threadID string) (GmailThread, error) {
	g.threadCalls = append(g.threadCalls, threadID)
	g.sequence = append(g.sequence, "thread")
	return g.thread, g.threadErr
}

func (g *outboundContractGmail) SearchMessages(_ context.Context, query string, maxResults int) (GmailMessageList, error) {
	call := len(g.searchCalls)
	g.searchCalls = append(g.searchCalls, struct {
		query      string
		maxResults int
	}{query: query, maxResults: maxResults})
	g.sequence = append(g.sequence, "search")
	if g.search == nil {
		return GmailMessageList{}, nil
	}
	return g.search(call, query, maxResults)
}

func (g *outboundContractGmail) SendRaw(_ context.Context, threadID string, raw []byte) (GmailSendResponse, error) {
	rawCopy := append([]byte(nil), raw...)
	g.sendCalls = append(g.sendCalls, struct {
		threadID string
		raw      []byte
	}{threadID: threadID, raw: rawCopy})
	g.sequence = append(g.sequence, "send")
	if g.send == nil {
		return GmailSendResponse{ID: "sent-message-1", ThreadID: threadID}, nil
	}
	return g.send(threadID, rawCopy)
}

func outboundContractMessage(
	id string,
	threadID string,
	from string,
	replyTo string,
	subject string,
	messageID string,
	inReplyTo string,
	references []string,
	body string,
	received time.Time,
) GmailRESTMessage {
	headers := []GmailRESTHeader{
		{Name: "From", Value: from},
		{Name: "Subject", Value: subject},
		{Name: "Message-ID", Value: messageID},
	}
	if replyTo != "" {
		headers = append(headers, GmailRESTHeader{Name: "Reply-To", Value: replyTo})
	}
	if inReplyTo != "" {
		headers = append(headers, GmailRESTHeader{Name: "In-Reply-To", Value: inReplyTo})
	}
	if len(references) > 0 {
		headers = append(headers, GmailRESTHeader{Name: "References", Value: strings.Join(references, " ")})
	}
	return GmailRESTMessage{
		ID:           id,
		ThreadID:     threadID,
		InternalDate: strconv.FormatInt(received.UnixMilli(), 10),
		Payload: &GmailRESTPart{
			MimeType: "text/plain; charset=UTF-8",
			Headers:  headers,
			Body: GmailRESTBody{
				Data: EncodeBase64URL([]byte(body)),
				Size: int64(len(body)),
			},
		},
	}
}

func outboundContractThread() GmailThread {
	threadID := "gmail-thread-42"
	firstReceived := time.Date(2026, time.August, 3, 8, 0, 0, 0, time.UTC)
	return GmailThread{
		ID: threadID,
		Messages: []GmailRESTMessage{
			outboundContractMessage(
				"customer-message-1",
				threadID,
				"Patient One <patient@example.com>",
				"Appointments <appointments@example.com>",
				"Physio appointment",
				"<customer-1@example.com>",
				"",
				nil,
				"Can I change my appointment?",
				firstReceived,
			),
			outboundContractMessage(
				"agent-message-1",
				threadID,
				syncContractMailbox,
				"",
				"Re: Physio appointment",
				"<agent-1@gmail.com>",
				"<customer-1@example.com>",
				[]string{"<root@example.com>", "<customer-1@example.com>"},
				"Of course.",
				firstReceived.Add(time.Minute),
			),
			outboundContractMessage(
				"customer-message-2",
				threadID,
				"Patient One <patient@example.com>",
				"Appointments <appointments@example.com>",
				"Re: Physio appointment",
				"<customer-2@example.com>",
				"<agent-1@gmail.com>",
				[]string{"<root@example.com>", "<customer-1@example.com>", "<agent-1@gmail.com>"},
				"Tomorrow morning, please.",
				firstReceived.Add(2*time.Minute),
			),
		},
	}
}

func outboundContractMessageRequest(recipient string) channelapi.OutboundMessage {
	return channelapi.OutboundMessage{
		IdempotencyKey: "outbox-42",
		Conversation: channelapi.ConversationRef{
			ExternalID: "gmail-thread-42",
		},
		Recipient: channelapi.Participant{
			Address:     recipient,
			DisplayName: "Untrusted display name",
			Role:        models.ConversationParticipantRoleCustomer,
		},
		Parts: []channelapi.MessagePart{{
			Type: models.MessagePartTypeText,
			Text: "Your appointment has been moved to 9:00 AM.",
		}},
		ReplyToExternalID: "customer-message-2",
	}
}

func TestDeliverOutboundReplyBindsThreadRecipientAndHeadersAndReconcilesIdempotently(t *testing.T) {
	fixedNow := time.Date(2026, time.August, 3, 9, 15, 0, 0, time.UTC)
	config := syncContractConfig()
	request := outboundContractMessageRequest("APPOINTMENTS@EXAMPLE.COM")
	digest := sha256.Sum256([]byte(syncContractMailbox + "\n" + request.IdempotencyKey))
	wantMessageID := fmt.Sprintf("<rereply.%x@gmail.com>", digest)
	wantSearchMessageID := strings.Trim(wantMessageID, "<>")
	gmail := &outboundContractGmail{thread: outboundContractThread()}
	gmail.search = func(call int, query string, maxResults int) (GmailMessageList, error) {
		wantQuery := "in:sent rfc822msgid:" + wantSearchMessageID
		if query != wantQuery || maxResults != 10 {
			t.Fatalf("SearchMessages(%q, %d), want (%q, 10)", query, maxResults, wantQuery)
		}
		if call == 0 {
			return GmailMessageList{}, nil
		}
		return GmailMessageList{Messages: []GmailMessageRef{{ID: "sent-message-1", ThreadID: "gmail-thread-42"}}}, nil
	}

	first, err := deliverOutboundReply(context.Background(), config, gmail, request, fixedNow)
	if err != nil {
		t.Fatalf("first deliverOutboundReply() error = %v", err)
	}
	if first.reconciled || first.messageID != wantMessageID || first.response.ID != "sent-message-1" || first.response.ThreadID != "gmail-thread-42" {
		t.Fatalf("first delivery = %#v", first)
	}
	if len(gmail.sendCalls) != 1 || gmail.sendCalls[0].threadID != "gmail-thread-42" {
		t.Fatalf("send calls = %#v", gmail.sendCalls)
	}
	if got := strings.Join(gmail.sequence, ","); got != "thread,search,send" {
		t.Fatalf("first delivery call order = %q, want thread,search,send", got)
	}

	rawMessage, err := mail.ReadMessage(bytes.NewReader(gmail.sendCalls[0].raw))
	if err != nil {
		t.Fatalf("sent raw MIME cannot be decoded: %v\n%s", err, gmail.sendCalls[0].raw)
	}
	if got := rawMessage.Header.Get("Subject"); got != "Re: Physio appointment" {
		t.Fatalf("Subject = %q", got)
	}
	if got := rawMessage.Header.Get("Message-ID"); got != wantMessageID {
		t.Fatalf("Message-ID = %q, want %q", got, wantMessageID)
	}
	if got := rawMessage.Header.Get("In-Reply-To"); got != "<customer-2@example.com>" {
		t.Fatalf("In-Reply-To = %q", got)
	}
	wantReferences := "<root@example.com> <customer-1@example.com> <agent-1@gmail.com> <customer-2@example.com>"
	if got := rawMessage.Header.Get("References"); got != wantReferences {
		t.Fatalf("References = %q, want %q", got, wantReferences)
	}
	from, err := mail.ParseAddress(rawMessage.Header.Get("From"))
	if err != nil || from.Address != syncContractMailbox {
		t.Fatalf("From = %#v, error = %v", from, err)
	}
	to, err := mail.ParseAddress(rawMessage.Header.Get("To"))
	if err != nil || to.Address != "appointments@example.com" || to.Name != "Appointments" {
		t.Fatalf("thread-bound To = %#v, error = %v", to, err)
	}
	if got := rawMessage.Header.Get("Date"); got != fixedNow.Format(time.RFC1123Z) {
		t.Fatalf("Date = %q, want %q", got, fixedNow.Format(time.RFC1123Z))
	}
	body, err := io.ReadAll(quotedprintable.NewReader(rawMessage.Body))
	if err != nil {
		t.Fatalf("decode MIME body: %v", err)
	}
	if got := strings.TrimSpace(string(body)); got != request.Parts[0].Text {
		t.Fatalf("decoded body = %q, want %q", got, request.Parts[0].Text)
	}

	second, err := deliverOutboundReply(context.Background(), config, gmail, request, fixedNow.Add(time.Minute))
	if err != nil {
		t.Fatalf("second deliverOutboundReply() error = %v", err)
	}
	if !second.reconciled || second.messageID != wantMessageID || second.response.ID != "sent-message-1" {
		t.Fatalf("reconciled delivery = %#v", second)
	}
	if len(gmail.sendCalls) != 1 {
		t.Fatalf("idempotent retry sent %d messages, want 1", len(gmail.sendCalls))
	}
	if got := strings.Join(gmail.sequence, ","); got != "thread,search,send,thread,search" {
		t.Fatalf("retry call order = %q", got)
	}
}

func TestDeliverOutboundReplyRejectsRecipientOutsideGmailThread(t *testing.T) {
	gmail := &outboundContractGmail{thread: outboundContractThread()}
	request := outboundContractMessageRequest("attacker@example.net")

	_, err := deliverOutboundReply(context.Background(), syncContractConfig(), gmail, request, time.Now())
	if err == nil || !strings.Contains(err.Error(), "recipient does not match") {
		t.Fatalf("deliverOutboundReply() error = %v, want recipient mismatch", err)
	}
	var deliveryErr *outboundDeliveryError
	if !errors.As(err, &deliveryErr) || deliveryErr.sendAttempted {
		t.Fatalf("delivery error = %#v, want pre-send rejection", err)
	}
	if len(gmail.searchCalls) != 0 || len(gmail.sendCalls) != 0 {
		t.Fatalf("recipient mismatch reached search/send: search=%v send=%v", gmail.searchCalls, gmail.sendCalls)
	}
	if got := strings.Join(gmail.sequence, ","); got != "thread" {
		t.Fatalf("recipient mismatch call order = %q, want thread only", got)
	}
}

func TestReconcileOutboundReplyRejectsDeterministicMessageIDInDifferentThread(t *testing.T) {
	config := syncContractConfig()
	request := outboundContractMessageRequest("appointments@example.com")
	gmail := &outboundContractGmail{thread: outboundContractThread()}
	gmail.search = func(_ int, _ string, _ int) (GmailMessageList, error) {
		return GmailMessageList{Messages: []GmailMessageRef{{ID: "collision", ThreadID: "different-thread"}}}, nil
	}

	response, found, err := reconcileOutboundReply(context.Background(), config, gmail, request)
	if err == nil || !strings.Contains(err.Error(), "different thread") {
		t.Fatalf("reconcileOutboundReply() = (%#v, %v, %v), want cross-thread collision", response, found, err)
	}
	if found || response.ID != "" {
		t.Fatalf("collision was treated as reconciled: response=%#v found=%v", response, found)
	}
}

type outboundContractToken string

func (token outboundContractToken) AccessToken(context.Context) (string, error) {
	return string(token), nil
}

func TestGmailClientSendRawEncodesRequestThatRoundTripsExactly(t *testing.T) {
	raw := []byte("From: realignphysiolates@gmail.com\r\nTo: patient@example.com\r\nSubject: Re: Test\r\nMessage-ID: <test@gmail.com>\r\n\r\nHello\r\n")
	var captured struct {
		Raw      string `json:"raw"`
		ThreadID string `json:"threadId"`
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != "/users/me/messages/send" {
			t.Errorf("request = %s %s", request.Method, request.URL.Path)
			http.NotFound(w, request)
			return
		}
		if authorization := request.Header.Get("Authorization"); authorization != "Bearer access-token" {
			t.Errorf("Authorization = %q", authorization)
		}
		if err := json.NewDecoder(request.Body).Decode(&captured); err != nil {
			t.Errorf("decode Gmail send request: %v", err)
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"gmail-sent-1","threadId":"gmail-thread-1"}`)
	}))
	defer server.Close()

	client, err := NewGmailClient(&Config{
		GmailAPIBaseURL: server.URL,
		HTTPTimeout:     5 * time.Second,
	}, outboundContractToken("access-token"), server.Client())
	if err != nil {
		t.Fatalf("NewGmailClient() error = %v", err)
	}
	response, err := client.SendRaw(context.Background(), "gmail-thread-1", raw)
	if err != nil {
		t.Fatalf("SendRaw() error = %v", err)
	}
	if response.ID != "gmail-sent-1" || response.ThreadID != "gmail-thread-1" || captured.ThreadID != "gmail-thread-1" {
		t.Fatalf("response = %#v, captured thread = %q", response, captured.ThreadID)
	}
	decoded, err := DecodeBase64URL(captured.Raw)
	if err != nil {
		t.Fatalf("decode Gmail raw request: %v", err)
	}
	if !bytes.Equal(decoded, raw) {
		t.Fatalf("Gmail raw request did not round-trip\n got: %q\nwant: %q", decoded, raw)
	}
}
