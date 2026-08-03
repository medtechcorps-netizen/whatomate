package gmailrelay

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"testing"
	"time"

	channelapi "github.com/shridarpatil/whatomate/internal/channel"
	"github.com/shridarpatil/whatomate/internal/metarelay"
	"github.com/shridarpatil/whatomate/internal/models"
)

const syncContractMailbox = "realignphysiolates@gmail.com"

type syncContractState struct {
	cursor       string
	cursorWrites []string
	lastSuccess  time.Time
	marked       []time.Time
	leaseHeld    bool
	renewals     int
	renewErr     error
	loseOnRenew  bool
}

func (s *syncContractState) GetHistoryCursor(context.Context) (string, error) {
	return s.cursor, nil
}

func (s *syncContractState) SetHistoryCursor(_ context.Context, cursor string) error {
	s.cursor = cursor
	s.cursorWrites = append(s.cursorWrites, cursor)
	return nil
}

func (s *syncContractState) GetLastSuccessfulSync(context.Context) (time.Time, error) {
	return s.lastSuccess, nil
}

func (s *syncContractState) MarkSuccessfulSync(_ context.Context, at time.Time) error {
	s.lastSuccess = at
	s.marked = append(s.marked, at)
	return nil
}

func (s *syncContractState) TryAcquireSyncLease(context.Context, string, time.Duration) (bool, error) {
	if s.leaseHeld {
		return false, nil
	}
	s.leaseHeld = true
	return true, nil
}

func (s *syncContractState) RenewSyncLease(context.Context, string, time.Duration) (bool, error) {
	s.renewals++
	if s.renewErr != nil {
		return false, s.renewErr
	}
	return s.leaseHeld && !s.loseOnRenew, nil
}

func (s *syncContractState) ReleaseSyncLease(context.Context, string) error {
	s.leaseHeld = false
	return nil
}

type syncContractGmail struct {
	profile      GmailProfile
	profileErr   error
	history      func(startHistoryID, pageToken string) (GmailHistoryPage, error)
	search       func(query string, maxResults int, pageToken string) (GmailMessageList, error)
	messages     map[string]GmailRESTMessage
	messageErrs  map[string]error
	profileCalls int
	historyCalls [][2]string
	searchCalls  []struct {
		query      string
		maxResults int
		pageToken  string
	}
	messageCalls []string
}

func (g *syncContractGmail) Profile(context.Context) (GmailProfile, error) {
	g.profileCalls++
	return g.profile, g.profileErr
}

func (g *syncContractGmail) History(_ context.Context, startHistoryID, pageToken string) (GmailHistoryPage, error) {
	g.historyCalls = append(g.historyCalls, [2]string{startHistoryID, pageToken})
	if g.history == nil {
		return GmailHistoryPage{}, errors.New("unexpected Gmail history call")
	}
	return g.history(startHistoryID, pageToken)
}

func (g *syncContractGmail) GetMessage(_ context.Context, messageID string) (GmailRESTMessage, error) {
	g.messageCalls = append(g.messageCalls, messageID)
	if err := g.messageErrs[messageID]; err != nil {
		return GmailRESTMessage{}, err
	}
	message, ok := g.messages[messageID]
	if !ok {
		return GmailRESTMessage{}, fmt.Errorf("unexpected Gmail message %q", messageID)
	}
	return message, nil
}

func (g *syncContractGmail) SearchMessagePage(_ context.Context, query string, maxResults int, pageToken string) (GmailMessageList, error) {
	g.searchCalls = append(g.searchCalls, struct {
		query      string
		maxResults int
		pageToken  string
	}{query: query, maxResults: maxResults, pageToken: pageToken})
	if g.search == nil {
		return GmailMessageList{}, errors.New("unexpected Gmail search call")
	}
	return g.search(query, maxResults, pageToken)
}

type syncContractAcceptance struct {
	calls []struct {
		acceptanceID string
		jobs         []metarelay.InboundJob
	}
	accept func(acceptanceID string, jobs []metarelay.InboundJob) (bool, error)
}

func (q *syncContractAcceptance) AcceptInbound(_ context.Context, acceptanceID string, jobs []metarelay.InboundJob) (bool, error) {
	jobsCopy := make([]metarelay.InboundJob, len(jobs))
	copy(jobsCopy, jobs)
	for index := range jobsCopy {
		jobsCopy[index].Body = append([]byte(nil), jobsCopy[index].Body...)
	}
	q.calls = append(q.calls, struct {
		acceptanceID string
		jobs         []metarelay.InboundJob
	}{acceptanceID: acceptanceID, jobs: jobsCopy})
	if q.accept != nil {
		return q.accept(acceptanceID, jobsCopy)
	}
	return true, nil
}

func syncContractConfig() *Config {
	return &Config{
		Mailbox:               syncContractMailbox,
		ReReplyWebhookURL:     "https://app.rereply.app/api/webhooks/channels/account-1",
		ReReplyInboundSecret:  "inbound-secret",
		ReReplyOutboundSecret: "outbound-secret",
		HTTPTimeout:           10 * time.Second,
		GmailPollInterval:     30 * time.Second,
	}
}

func syncContractHistoryRecord(id string, messages ...GmailMessageRef) GmailHistoryRecord {
	record := GmailHistoryRecord{ID: id}
	for _, message := range messages {
		record.MessagesAdded = append(record.MessagesAdded, struct {
			Message GmailMessageRef `json:"message"`
		}{Message: message})
	}
	return record
}

func syncContractLabelAddedRecord(id string, message GmailMessageRef, labels ...string) GmailHistoryRecord {
	record := GmailHistoryRecord{ID: id}
	record.LabelsAdded = append(record.LabelsAdded, struct {
		Message  GmailMessageRef `json:"message"`
		LabelIDs []string        `json:"labelIds"`
	}{Message: message, LabelIDs: labels})
	return record
}

func syncContractMessage(
	id string,
	threadID string,
	labels []string,
	from string,
	replyTo string,
	subject string,
	body string,
	received time.Time,
) GmailRESTMessage {
	headers := []GmailRESTHeader{
		{Name: "From", Value: from},
		{Name: "Subject", Value: subject},
		{Name: "Message-ID", Value: "<" + id + "@example.com>"},
	}
	if replyTo != "" {
		headers = append(headers, GmailRESTHeader{Name: "Reply-To", Value: replyTo})
	}
	return GmailRESTMessage{
		ID:           id,
		ThreadID:     threadID,
		LabelIDs:     labels,
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

func TestGmailSyncInitialBaselineDoesNotImportExistingMail(t *testing.T) {
	fixedNow := time.Date(2026, time.August, 3, 9, 0, 0, 0, time.UTC)
	state := &syncContractState{}
	gmail := &syncContractGmail{
		profile:  GmailProfile{EmailAddress: syncContractMailbox, HistoryID: "baseline-100"},
		messages: map[string]GmailRESTMessage{},
	}
	queue := &syncContractAcceptance{}
	syncer, err := NewSyncer(syncContractConfig(), gmail, state, queue)
	if err != nil {
		t.Fatalf("NewSyncer() error = %v", err)
	}
	syncer.now = func() time.Time { return fixedNow }

	if err := syncer.ProcessOnce(context.Background()); err != nil {
		t.Fatalf("ProcessOnce() error = %v", err)
	}
	if gmail.profileCalls != 1 || len(gmail.historyCalls) != 0 || len(gmail.messageCalls) != 0 || len(gmail.searchCalls) != 0 {
		t.Fatalf("baseline touched existing mail: profile=%d history=%v messages=%v search=%v", gmail.profileCalls, gmail.historyCalls, gmail.messageCalls, gmail.searchCalls)
	}
	if len(queue.calls) != 0 {
		t.Fatalf("baseline queued %d inbound jobs, want 0", len(queue.calls))
	}
	if !reflect.DeepEqual(state.cursorWrites, []string{"baseline-100"}) {
		t.Fatalf("cursor writes = %#v", state.cursorWrites)
	}
	if len(state.marked) != 1 || !state.marked[0].Equal(fixedNow) {
		t.Fatalf("successful sync marks = %#v, want %s", state.marked, fixedNow)
	}
}

func TestGmailSyncMapsIncomingMessageToExactCanonicalEnvelopeBeforeAdvancingCursor(t *testing.T) {
	received := time.Date(2026, time.August, 3, 8, 15, 30, 123_000_000, time.UTC)
	fixedNow := time.Date(2026, time.August, 3, 8, 16, 0, 0, time.UTC)
	state := &syncContractState{cursor: "history-100"}
	message := syncContractMessage(
		"gmail-message-1",
		"gmail-thread-1",
		[]string{"INBOX", "IMPORTANT"},
		"Patient One <PATIENT@Example.com>",
		"Appointments <reply@example.com>",
		"Physio appointment",
		"Hello, I need to change my appointment.\r\n",
		received,
	)
	gmail := &syncContractGmail{
		messages: map[string]GmailRESTMessage{message.ID: message},
		history: func(startHistoryID, pageToken string) (GmailHistoryPage, error) {
			if startHistoryID != "history-100" || pageToken != "" {
				t.Fatalf("History(%q, %q), want history-100 and empty page token", startHistoryID, pageToken)
			}
			return GmailHistoryPage{
				History:   []GmailHistoryRecord{syncContractHistoryRecord("101", GmailMessageRef{ID: message.ID, ThreadID: message.ThreadID})},
				HistoryID: "history-101",
			}, nil
		},
	}
	queue := &syncContractAcceptance{}
	queue.accept = func(acceptanceID string, jobs []metarelay.InboundJob) (bool, error) {
		if state.cursor != "history-100" || len(state.cursorWrites) != 0 {
			t.Fatalf("history cursor advanced before queue acceptance: cursor=%q writes=%v", state.cursor, state.cursorWrites)
		}
		if acceptanceID != "gmail:message_added:gmail-message-1" || len(jobs) != 1 {
			t.Fatalf("acceptance = %q, jobs = %#v", acceptanceID, jobs)
		}
		return true, nil
	}
	syncer, err := NewSyncer(syncContractConfig(), gmail, state, queue)
	if err != nil {
		t.Fatalf("NewSyncer() error = %v", err)
	}
	syncer.now = func() time.Time { return fixedNow }

	if err := syncer.ProcessOnce(context.Background()); err != nil {
		t.Fatalf("ProcessOnce() error = %v", err)
	}
	if len(queue.calls) != 1 || len(queue.calls[0].jobs) != 1 {
		t.Fatalf("queue calls = %#v", queue.calls)
	}

	wantEnvelope := struct {
		ExternalAccountID string                    `json:"external_account_id"`
		Events            []channelapi.InboundEvent `json:"events"`
	}{
		ExternalAccountID: syncContractMailbox,
		Events: []channelapi.InboundEvent{{
			DedupeKey:       "gmail:message_added:gmail-message-1",
			ProviderEventID: "gmail:message_added:gmail-message-1",
			Type:            channelapi.NormalizedEventTypeMessage,
			OccurredAt:      received,
			Message: &channelapi.InboundMessage{
				ExternalMessageID: "gmail-message-1",
				Conversation: channelapi.ConversationRef{
					ExternalID: "gmail-thread-1",
					Subject:    "Physio appointment",
				},
				Sender: channelapi.Participant{
					ExternalID:  "patient@example.com",
					Address:     "reply@example.com",
					DisplayName: "Patient One",
					Role:        models.ConversationParticipantRoleCustomer,
				},
				Direction: models.DirectionIncoming,
				Parts: []channelapi.MessagePart{{
					Type: models.MessagePartTypeText,
					Text: "Hello, I need to change my appointment.",
				}},
				SentAt:     received,
				ReceivedAt: received,
			},
		}},
	}
	wantBody, err := json.Marshal(wantEnvelope)
	if err != nil {
		t.Fatal(err)
	}
	gotJob := queue.calls[0].jobs[0]
	if gotJob.ID != queue.calls[0].acceptanceID || gotJob.AccountKey != syncContractMailbox {
		t.Fatalf("queued job = %#v", gotJob)
	}
	if !bytes.Equal(gotJob.Body, wantBody) {
		t.Fatalf("canonical envelope mismatch\n got: %s\nwant: %s", gotJob.Body, wantBody)
	}
	if !reflect.DeepEqual(state.cursorWrites, []string{"history-101"}) || state.cursor != "history-101" {
		t.Fatalf("cursor = %q, writes = %#v", state.cursor, state.cursorWrites)
	}
	if len(state.marked) != 1 || !state.marked[0].Equal(fixedNow) {
		t.Fatalf("successful sync marks = %#v", state.marked)
	}
}

func TestGmailSyncKeepsStableDedupeAndFiltersSentMail(t *testing.T) {
	received := time.Date(2026, time.August, 3, 9, 30, 0, 0, time.UTC)
	incoming := syncContractMessage("incoming-1", "thread-1", []string{"INBOX"}, "patient@example.com", "", "Question", "Can you help?", received)
	sent := syncContractMessage("sent-1", "thread-1", []string{"INBOX", "SENT"}, syncContractMailbox, "", "Re: Question", "Yes", received.Add(time.Minute))
	selfEcho := syncContractMessage("self-echo-1", "thread-1", []string{"INBOX"}, syncContractMailbox, "", "Re: Question", "Yes again", received.Add(2*time.Minute))
	state := &syncContractState{cursor: "history-200"}
	gmail := &syncContractGmail{
		messages: map[string]GmailRESTMessage{
			incoming.ID: incoming,
			sent.ID:     sent,
			selfEcho.ID: selfEcho,
		},
	}
	gmail.history = func(startHistoryID, _ string) (GmailHistoryPage, error) {
		switch startHistoryID {
		case "history-200":
			return GmailHistoryPage{
				History: []GmailHistoryRecord{
					syncContractHistoryRecord("201", GmailMessageRef{ID: incoming.ID}, GmailMessageRef{ID: incoming.ID}),
					syncContractHistoryRecord("202", GmailMessageRef{ID: sent.ID}, GmailMessageRef{ID: selfEcho.ID}),
				},
				HistoryID: "history-202",
			}, nil
		case "history-202":
			return GmailHistoryPage{
				History:   []GmailHistoryRecord{syncContractHistoryRecord("203", GmailMessageRef{ID: incoming.ID})},
				HistoryID: "history-203",
			}, nil
		default:
			return GmailHistoryPage{}, fmt.Errorf("unexpected cursor %q", startHistoryID)
		}
	}
	accepted := make(map[string]struct{})
	queue := &syncContractAcceptance{accept: func(acceptanceID string, _ []metarelay.InboundJob) (bool, error) {
		_, duplicate := accepted[acceptanceID]
		accepted[acceptanceID] = struct{}{}
		return !duplicate, nil
	}}
	syncer, err := NewSyncer(syncContractConfig(), gmail, state, queue)
	if err != nil {
		t.Fatalf("NewSyncer() error = %v", err)
	}

	if err := syncer.ProcessOnce(context.Background()); err != nil {
		t.Fatalf("first ProcessOnce() error = %v", err)
	}
	if err := syncer.ProcessOnce(context.Background()); err != nil {
		t.Fatalf("second ProcessOnce() error = %v", err)
	}
	if len(queue.calls) != 2 {
		t.Fatalf("queue calls = %d, want one incoming delivery attempt per history cycle", len(queue.calls))
	}
	if queue.calls[0].acceptanceID != "gmail:message_added:incoming-1" || queue.calls[1].acceptanceID != queue.calls[0].acceptanceID {
		t.Fatalf("unstable acceptance IDs = %q, %q", queue.calls[0].acceptanceID, queue.calls[1].acceptanceID)
	}
	if !bytes.Equal(queue.calls[0].jobs[0].Body, queue.calls[1].jobs[0].Body) {
		t.Fatal("the same Gmail message produced different canonical bodies")
	}
	if len(accepted) != 1 {
		t.Fatalf("durable acceptance keys = %#v, want one stable key", accepted)
	}
	if state.cursor != "history-203" {
		t.Fatalf("cursor = %q, want history-203", state.cursor)
	}
}

func TestGmailSyncDoesNotAdvanceCursorWhenQueueAcceptanceFails(t *testing.T) {
	received := time.Date(2026, time.August, 3, 10, 0, 0, 0, time.UTC)
	message := syncContractMessage("incoming-failure", "thread-failure", []string{"INBOX"}, "patient@example.com", "", "Question", "Hello", received)
	state := &syncContractState{cursor: "history-300"}
	gmail := &syncContractGmail{
		messages: map[string]GmailRESTMessage{message.ID: message},
		history: func(_, _ string) (GmailHistoryPage, error) {
			return GmailHistoryPage{
				History:   []GmailHistoryRecord{syncContractHistoryRecord("301", GmailMessageRef{ID: message.ID})},
				HistoryID: "history-301",
			}, nil
		},
	}
	queueErr := errors.New("queue unavailable")
	queue := &syncContractAcceptance{accept: func(string, []metarelay.InboundJob) (bool, error) {
		return false, queueErr
	}}
	syncer, err := NewSyncer(syncContractConfig(), gmail, state, queue)
	if err != nil {
		t.Fatalf("NewSyncer() error = %v", err)
	}

	err = syncer.ProcessOnce(context.Background())
	if !errors.Is(err, queueErr) {
		t.Fatalf("ProcessOnce() error = %v, want %v", err, queueErr)
	}
	if state.cursor != "history-300" || len(state.cursorWrites) != 0 || len(state.marked) != 0 {
		t.Fatalf("failed queue acceptance advanced state: cursor=%q writes=%v marks=%v", state.cursor, state.cursorWrites, state.marked)
	}
}

func TestGmailSyncProcessesMessagesMovedIntoInboxByLabelAdded(t *testing.T) {
	received := time.Date(2026, time.August, 3, 10, 30, 0, 0, time.UTC)
	inboxMessage := syncContractMessage("moved-to-inbox", "thread-inbox", []string{"INBOX"}, "patient@example.com", "", "Moved", "Please reply", received)
	nonInboxMessage := syncContractMessage("starred-only", "thread-starred", []string{"STARRED"}, "patient@example.com", "", "Starred", "Ignore", received)
	state := &syncContractState{cursor: "history-310"}
	gmail := &syncContractGmail{
		messages: map[string]GmailRESTMessage{
			inboxMessage.ID:    inboxMessage,
			nonInboxMessage.ID: nonInboxMessage,
		},
		history: func(_, _ string) (GmailHistoryPage, error) {
			return GmailHistoryPage{
				History: []GmailHistoryRecord{
					syncContractLabelAddedRecord("311", GmailMessageRef{ID: inboxMessage.ID, ThreadID: inboxMessage.ThreadID}, "IMPORTANT", "INBOX"),
					syncContractLabelAddedRecord("312", GmailMessageRef{ID: nonInboxMessage.ID, ThreadID: nonInboxMessage.ThreadID}, "STARRED"),
				},
				HistoryID: "history-312",
			}, nil
		},
	}
	queue := &syncContractAcceptance{}
	syncer, err := NewSyncer(syncContractConfig(), gmail, state, queue)
	if err != nil {
		t.Fatalf("NewSyncer() error = %v", err)
	}

	if err := syncer.ProcessOnce(context.Background()); err != nil {
		t.Fatalf("ProcessOnce() error = %v", err)
	}
	if !reflect.DeepEqual(gmail.messageCalls, []string{inboxMessage.ID}) {
		t.Fatalf("GetMessage calls = %#v, want only the message with an INBOX labelAdded event", gmail.messageCalls)
	}
	if len(queue.calls) != 1 || queue.calls[0].acceptanceID != "gmail:message_added:"+inboxMessage.ID {
		t.Fatalf("queue calls = %#v", queue.calls)
	}
	if state.cursor != "history-312" {
		t.Fatalf("cursor = %q, want history-312", state.cursor)
	}
}

func TestGmailSyncSkipsMessageGet404AndAdvancesCursor(t *testing.T) {
	received := time.Date(2026, time.August, 3, 10, 45, 0, 0, time.UTC)
	available := syncContractMessage("available", "thread-available", []string{"INBOX"}, "patient@example.com", "", "Available", "Still here", received)
	state := &syncContractState{cursor: "history-320"}
	gmail := &syncContractGmail{
		messages:    map[string]GmailRESTMessage{available.ID: available},
		messageErrs: map[string]error{"deleted": &GmailAPIError{Operation: "message_get", StatusCode: 404}},
		history: func(_, _ string) (GmailHistoryPage, error) {
			return GmailHistoryPage{
				History: []GmailHistoryRecord{syncContractHistoryRecord(
					"321",
					GmailMessageRef{ID: "deleted", ThreadID: "thread-deleted"},
					GmailMessageRef{ID: available.ID, ThreadID: available.ThreadID},
				)},
				HistoryID: "history-321",
			}, nil
		},
	}
	queue := &syncContractAcceptance{}
	syncer, err := NewSyncer(syncContractConfig(), gmail, state, queue)
	if err != nil {
		t.Fatalf("NewSyncer() error = %v", err)
	}

	if err := syncer.ProcessOnce(context.Background()); err != nil {
		t.Fatalf("ProcessOnce() error = %v", err)
	}
	if len(queue.calls) != 1 || queue.calls[0].acceptanceID != "gmail:message_added:"+available.ID {
		t.Fatalf("queue calls = %#v", queue.calls)
	}
	if state.cursor != "history-321" || !reflect.DeepEqual(state.cursorWrites, []string{"history-321"}) {
		t.Fatalf("cursor = %q, writes = %#v", state.cursor, state.cursorWrites)
	}
}

func TestGmailSyncUsesSafeVisibleFallbackForMalformedMIME(t *testing.T) {
	received := time.Date(2026, time.August, 3, 11, 0, 0, 0, time.UTC)
	message := syncContractMessage("malformed-mime", "thread-malformed", []string{"INBOX"}, "patient@example.com", "", "Cannot decode", "ignored", received)
	message.Payload.Body.Data = "%%%not-base64url%%%"
	state := &syncContractState{cursor: "history-330"}
	gmail := &syncContractGmail{
		messages: map[string]GmailRESTMessage{message.ID: message},
		history: func(_, _ string) (GmailHistoryPage, error) {
			return GmailHistoryPage{
				History:   []GmailHistoryRecord{syncContractHistoryRecord("331", GmailMessageRef{ID: message.ID, ThreadID: message.ThreadID})},
				HistoryID: "history-331",
			}, nil
		},
	}
	queue := &syncContractAcceptance{}
	syncer, err := NewSyncer(syncContractConfig(), gmail, state, queue)
	if err != nil {
		t.Fatalf("NewSyncer() error = %v", err)
	}

	if err := syncer.ProcessOnce(context.Background()); err != nil {
		t.Fatalf("ProcessOnce() error = %v", err)
	}
	if len(queue.calls) != 1 || len(queue.calls[0].jobs) != 1 {
		t.Fatalf("queue calls = %#v", queue.calls)
	}
	var envelope struct {
		ExternalAccountID string                    `json:"external_account_id"`
		Events            []channelapi.InboundEvent `json:"events"`
	}
	if err := json.Unmarshal(queue.calls[0].jobs[0].Body, &envelope); err != nil {
		t.Fatalf("decode fallback envelope: %v", err)
	}
	if envelope.ExternalAccountID != syncContractMailbox || len(envelope.Events) != 1 || envelope.Events[0].Message == nil {
		t.Fatalf("fallback envelope = %#v", envelope)
	}
	event := envelope.Events[0]
	if event.OccurredAt != received || event.Message.ReceivedAt != received || event.Message.SentAt != received {
		t.Fatalf("fallback timestamps = event %s sent %s received %s, want %s", event.OccurredAt, event.Message.SentAt, event.Message.ReceivedAt, received)
	}
	if event.Message.Conversation.ExternalID != message.ThreadID || event.Message.Conversation.Subject != "[Unreadable email]" {
		t.Fatalf("fallback conversation = %#v", event.Message.Conversation)
	}
	if event.Message.Sender.ExternalID != "gmail-unparsed:"+message.ID || event.Message.Sender.Address != "" {
		t.Fatalf("fallback sender = %#v", event.Message.Sender)
	}
	if len(event.Message.Parts) != 1 || event.Message.Parts[0].Text != "[Email could not be decoded safely. Open Gmail to view the original message.]" {
		t.Fatalf("fallback parts = %#v", event.Message.Parts)
	}
	if state.cursor != "history-331" {
		t.Fatalf("cursor = %q, want history-331", state.cursor)
	}
}

func TestGmailSyncRecoversStaleCursorWithoutLosingRecentIncomingMail(t *testing.T) {
	fixedNow := time.Date(2026, time.August, 3, 12, 0, 0, 0, time.UTC)
	lastSuccess := fixedNow.Add(-20 * time.Minute)
	received := fixedNow.Add(-10 * time.Minute)
	message := syncContractMessage("recovered-1", "recovered-thread", []string{"INBOX"}, "patient@example.com", "", "Recovered", "Please call me", received)
	state := &syncContractState{cursor: "stale-history", lastSuccess: lastSuccess}
	gmail := &syncContractGmail{
		profile:  GmailProfile{EmailAddress: syncContractMailbox, HistoryID: "fresh-history"},
		messages: map[string]GmailRESTMessage{message.ID: message},
		history: func(_, _ string) (GmailHistoryPage, error) {
			return GmailHistoryPage{}, &GmailAPIError{Operation: "history", StatusCode: 404}
		},
	}
	wantQuery := "in:inbox after:" + strconv.FormatInt(lastSuccess.Add(-5*time.Minute).Unix(), 10)
	gmail.search = func(query string, maxResults int, pageToken string) (GmailMessageList, error) {
		if query != wantQuery || maxResults != 100 || pageToken != "" {
			t.Fatalf("SearchMessagePage(%q, %d, %q), want (%q, 100, empty)", query, maxResults, pageToken, wantQuery)
		}
		return GmailMessageList{Messages: []GmailMessageRef{{ID: message.ID, ThreadID: message.ThreadID}}}, nil
	}
	queue := &syncContractAcceptance{accept: func(_ string, _ []metarelay.InboundJob) (bool, error) {
		if state.cursor != "stale-history" {
			t.Fatalf("stale cursor replaced before recovery job acceptance: %q", state.cursor)
		}
		return true, nil
	}}
	syncer, err := NewSyncer(syncContractConfig(), gmail, state, queue)
	if err != nil {
		t.Fatalf("NewSyncer() error = %v", err)
	}
	syncer.now = func() time.Time { return fixedNow }

	if err := syncer.ProcessOnce(context.Background()); err != nil {
		t.Fatalf("ProcessOnce() error = %v", err)
	}
	if gmail.profileCalls != 1 || len(gmail.searchCalls) != 1 || len(queue.calls) != 1 {
		t.Fatalf("recovery calls: profile=%d search=%d queue=%d", gmail.profileCalls, len(gmail.searchCalls), len(queue.calls))
	}
	if state.cursor != "fresh-history" || !reflect.DeepEqual(state.cursorWrites, []string{"fresh-history"}) {
		t.Fatalf("recovered cursor = %q, writes = %#v", state.cursor, state.cursorWrites)
	}
	if len(state.marked) != 1 || !state.marked[0].Equal(fixedNow) {
		t.Fatalf("successful sync marks = %#v", state.marked)
	}
}
