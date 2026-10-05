package handlers

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/calling"
	"github.com/shridarpatil/whatomate/internal/config"
	"github.com/shridarpatil/whatomate/internal/models"
	appwebsocket "github.com/shridarpatil/whatomate/internal/websocket"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Gap A: statuses for WAMIDs that never become a Message.

const nonMessageSendAckLog = "Acknowledged WhatsApp status for a ReReply send that stores no message"

func coexistenceModeName(coexistence bool) string {
	if coexistence {
		return "coexistence"
	}
	return "classic"
}

func countOrganizationMessages(t *testing.T, app *App, organizationID uuid.UUID) int64 {
	t.Helper()
	var count int64
	require.NoError(t, app.DB.Unscoped().Model(&models.Message{}).
		Where("organization_id = ?", organizationID).Count(&count).Error)
	return count
}

// A call permission request is sent through Graph and stored only as a
// CallPermission row; no Message ever carries its WAMID. Its statuses are
// acknowledged as soon as that row exists, at any age and on both account
// kinds, instead of being retried for Meta's whole window on a Coexistence
// account. Before the row commits the status is still retried.
func TestWebhookStatusForCallPermissionRequestIsAcknowledged(t *testing.T) {
	app := webhookTestApp(t)
	for _, coexistence := range []bool{false, true} {
		t.Run(coexistenceModeName(coexistence), func(t *testing.T) {
			account := orphanStatusTestAccount(t, app, coexistence)
			contact := testutil.CreateTestContact(t, app.DB, account.OrganizationID)
			wamid := "wamid.synthetic-permission-" + uuid.NewString()
			now := time.Now()
			sent := orphanStatusBody(t, account, orphanStatusAt(wamid, "sent", now))

			assert.Equal(t, http.StatusServiceUnavailable, signedWebhookStatusCode(t, app, sent),
				"before the permission row commits, the status is still an expected wait")

			require.NoError(t, app.DB.Create(&models.CallPermission{
				BaseModel:       models.BaseModel{ID: uuid.New()},
				OrganizationID:  account.OrganizationID,
				ContactID:       contact.ID,
				WhatsAppAccount: account.Name,
				Status:          models.CallPermissionPending,
				MessageID:       wamid,
			}).Error)
			logs := captureStatusTestLogs(app)
			for attempt := 1; attempt <= 2; attempt++ {
				assert.Equal(t, http.StatusOK, signedWebhookStatusCode(t, app, sent), "attempt %d", attempt)
			}
			assert.Len(t, logs.lines("info", nonMessageSendAckLog), 2)

			failed := orphanStatusBody(t, account, map[string]any{
				"id": wamid, "status": "failed", "timestamp": strconv.FormatInt(now.Unix(), 10),
				"recipient_id": "15550000002",
				"errors":       []any{map[string]any{"code": 131026, "title": "Message undeliverable"}},
			})
			assert.Equal(t, http.StatusOK, signedWebhookStatusCode(t, app, failed))
			assert.Len(t, logs.lines("warn", "Acknowledged failed WhatsApp status for a ReReply send that stores no message"), 1)
			assert.Zero(t, countOrganizationMessages(t, app, account.OrganizationID), "no Message is created")
		})
	}
}

// Another tenant's permission request is no proof for this account.
func TestWebhookStatusCallPermissionLookupIsTenantScoped(t *testing.T) {
	app := webhookTestApp(t)
	account := orphanStatusTestAccount(t, app, true)
	other := orphanStatusTestAccount(t, app, true)
	otherContact := testutil.CreateTestContact(t, app.DB, other.OrganizationID)
	wamid := "wamid.synthetic-permission-other-tenant-" + uuid.NewString()
	require.NoError(t, app.DB.Create(&models.CallPermission{
		BaseModel:       models.BaseModel{ID: uuid.New()},
		OrganizationID:  other.OrganizationID,
		ContactID:       otherContact.ID,
		WhatsAppAccount: other.Name,
		Status:          models.CallPermissionPending,
		MessageID:       wamid,
	}).Error)

	err := app.processStatusUpdate(account.PhoneID, WebhookStatus{
		ID: wamid, Status: "delivered", Timestamp: strconv.FormatInt(time.Now().Unix(), 10), RecipientID: "15550000002",
	})
	require.ErrorIs(t, err, errWhatsAppStatusOwnerPending)
}

// reactionGraphServer answers every reaction send with wamid and records the
// request bodies it received.
type reactionGraphServer struct {
	*httptest.Server
	mu     sync.Mutex
	bodies []map[string]any
}

func newReactionGraphServer(t *testing.T, wamid string) *reactionGraphServer {
	t.Helper()
	server := &reactionGraphServer{}
	server.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		server.mu.Lock()
		server.bodies = append(server.bodies, body)
		server.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"messaging_product":"whatsapp","messages":[{"id":"` + wamid + `"}]}`))
	}))
	t.Cleanup(server.Close)
	return server
}

func storedSentReactionWAMIDs(t *testing.T, app *App, messageID uuid.UUID) []string {
	t.Helper()
	var stored models.Message
	require.NoError(t, app.DB.Unscoped().First(&stored, "id = ?", messageID).Error)
	raw, _ := stored.Metadata[sentReactionWAMIDsMetadataKey].([]any)
	result := make([]string, 0, len(raw))
	for _, value := range raw {
		text, _ := value.(string)
		result = append(result, text)
	}
	return result
}

// A reaction sent from ReReply produces no Message either. Its WAMID, taken
// from the Graph response, is recorded in the metadata of the message that
// was reacted to, and its statuses are then acknowledged at once on a
// Coexistence account. A status whose recipient is not that message's contact
// is not recognised and keeps the retry contract.
func TestWebhookStatusForSentReactionIsAcknowledged(t *testing.T) {
	app := webhookTestApp(t)
	account := orphanStatusTestAccount(t, app, true)
	contact := testutil.CreateTestContact(t, app.DB, account.OrganizationID)
	target := models.Message{
		BaseModel:         models.BaseModel{ID: uuid.New()},
		OrganizationID:    account.OrganizationID,
		WhatsAppAccount:   account.Name,
		ContactID:         contact.ID,
		WhatsAppMessageID: "wamid.synthetic-reaction-target-" + uuid.NewString(),
		Direction:         models.DirectionIncoming,
		MessageType:       models.MessageTypeText,
		Content:           "synthetic message to react to",
		Status:            models.MessageStatusDelivered,
		Metadata:          models.JSONB{"reactions": []any{}},
	}
	require.NoError(t, app.DB.Create(&target).Error)
	reactionWAMID := "wamid.synthetic-sent-reaction-" + uuid.NewString()
	graph := newReactionGraphServer(t, reactionWAMID)
	app.Config.WhatsApp.BaseURL = graph.URL
	app.HTTPClient = graph.Client()

	recipient := strings.TrimPrefix(contact.PhoneNumber, "+")
	status := func(recipientID string) []byte {
		value := orphanStatusAt(reactionWAMID, "sent", time.Now())
		value["recipient_id"] = recipientID
		return orphanStatusBody(t, account, value)
	}
	assert.Equal(t, http.StatusServiceUnavailable, signedWebhookStatusCode(t, app, status(recipient)),
		"before the reaction is recorded, its status is an expected wait")

	require.NoError(t, app.sendWhatsAppReactionGuarded(&account, contact, &target, "\U0001F44D"))
	require.Len(t, graph.bodies, 1)
	reaction, _ := graph.bodies[0]["reaction"].(map[string]any)
	assert.Equal(t, target.WhatsAppMessageID, reaction["message_id"])
	assert.Equal(t, []string{reactionWAMID}, storedSentReactionWAMIDs(t, app, target.ID))

	logs := captureStatusTestLogs(app)
	for attempt := 1; attempt <= 2; attempt++ {
		assert.Equal(t, http.StatusOK, signedWebhookStatusCode(t, app, status(recipient)), "attempt %d", attempt)
	}
	assert.Len(t, logs.lines("info", nonMessageSendAckLog), 2)
	assert.Equal(t, http.StatusOK, signedWebhookStatusCode(t, app, status("+"+recipient)),
		"a recipient with a leading plus is the same number")
	assert.Equal(t, http.StatusServiceUnavailable, signedWebhookStatusCode(t, app, status("15550009999")),
		"another recipient's status is not this reaction's")
	assert.Equal(t, http.StatusServiceUnavailable, signedWebhookStatusCode(t, app, status("")),
		"a status without a recipient phone is not recognised")
	assert.EqualValues(t, 1, countOrganizationMessages(t, app, account.OrganizationID), "no Message is created")
}

// The record keeps the most recent sentReactionWAMIDsLimit WAMIDs, once each,
// and preserves the rest of the metadata.
func TestRecordSentWhatsAppReactionWAMIDIsBounded(t *testing.T) {
	app := webhookTestApp(t)
	account := orphanStatusTestAccount(t, app, true)
	contact := testutil.CreateTestContact(t, app.DB, account.OrganizationID)
	target := models.Message{
		BaseModel:         models.BaseModel{ID: uuid.New()},
		OrganizationID:    account.OrganizationID,
		WhatsAppAccount:   account.Name,
		ContactID:         contact.ID,
		WhatsAppMessageID: "wamid.synthetic-bounded-target-" + uuid.NewString(),
		Direction:         models.DirectionOutgoing,
		MessageType:       models.MessageTypeText,
		Content:           "synthetic bounded target",
		Status:            models.MessageStatusSent,
		Metadata:          models.JSONB{"reactions": []any{map[string]any{"emoji": "x", "from_user": "u"}}},
	}
	require.NoError(t, app.DB.Create(&target).Error)

	total := sentReactionWAMIDsLimit + 8
	for i := 0; i < total; i++ {
		require.NoError(t, app.recordSentWhatsAppReactionWAMID(account.OrganizationID, target.ID, "wamid.synthetic-bounded-"+strconv.Itoa(i)))
	}
	require.NoError(t, app.recordSentWhatsAppReactionWAMID(account.OrganizationID, target.ID, "wamid.synthetic-bounded-10"))
	recorded := storedSentReactionWAMIDs(t, app, target.ID)
	require.Len(t, recorded, sentReactionWAMIDsLimit)
	assert.Equal(t, "wamid.synthetic-bounded-10", recorded[len(recorded)-1], "a repeated WAMID moves to the end once")
	assert.NotContains(t, recorded, "wamid.synthetic-bounded-0", "the oldest WAMIDs are dropped")
	occurrences := 0
	for _, value := range recorded {
		if value == "wamid.synthetic-bounded-10" {
			occurrences++
		}
	}
	assert.Equal(t, 1, occurrences)

	var stored models.Message
	require.NoError(t, app.DB.First(&stored, "id = ?", target.ID).Error)
	assert.NotEmpty(t, stored.Metadata["reactions"], "other metadata is kept")
}

// The Coexistence backstop: a status for a WAMID that no Message carries and
// that no ReReply send recorded is retried for whatsAppCoexistenceOrphanStatusGrace
// and then acknowledged, but only with a usable timestamp and the same absence
// proof as on a classic account.
func TestWebhookStatusCoexistenceOrphanBackstop(t *testing.T) {
	app := webhookTestApp(t)
	account := orphanStatusTestAccount(t, app, true)
	wamid := "wamid.synthetic-coexistence-backstop-" + uuid.NewString()
	logs := captureStatusTestLogs(app)

	inside := orphanStatusBody(t, account, orphanStatusAt(wamid, "delivered",
		time.Now().Add(-whatsAppCoexistenceOrphanStatusGrace+time.Minute)))
	assert.Equal(t, http.StatusServiceUnavailable, signedWebhookStatusCode(t, app, inside))
	assert.Len(t, logs.lines("warn", statusTestDeferredLog), 1, "a Coexistence status past the classic grace still warns")

	untimed := orphanStatusBody(t, account, map[string]any{"id": wamid, "status": "delivered"})
	assert.Equal(t, http.StatusServiceUnavailable, signedWebhookStatusCode(t, app, untimed))

	// A tiny timestamp (here 1970) is not a usable time, however old it
	// reads: it is retried, never settled.
	tiny := orphanStatusBody(t, account, map[string]any{"id": wamid, "status": "delivered", "timestamp": "5"})
	assert.Equal(t, http.StatusServiceUnavailable, signedWebhookStatusCode(t, app, tiny))
	assert.Len(t, logs.lines("warn", statusTestUnusableLog), 2, "the untimed and the tiny status are both unusable")
	err := app.processStatusUpdate(account.PhoneID, WebhookStatus{ID: wamid, Status: "read", Timestamp: "5", RecipientID: "15550000002"})
	require.ErrorIs(t, err, errWhatsAppStatusOwnerPending)

	past := orphanStatusBody(t, account, orphanStatusAt(wamid, "read",
		time.Now().Add(-whatsAppCoexistenceOrphanStatusGrace-time.Minute)))
	assert.Equal(t, http.StatusOK, signedWebhookStatusCode(t, app, past))
	assert.Equal(t, http.StatusOK, signedWebhookStatusCode(t, app, past), "a replay stays acknowledged")
	assert.Len(t, logs.lines("warn", "Acknowledged WhatsApp status for a message ReReply never stored"), 2)
	assert.Zero(t, countOrganizationMessages(t, app, account.OrganizationID))

	// A row that carries the WAMID but is no resolver candidate still fails
	// closed past the backstop.
	foreignWAMID := "wamid.synthetic-coexistence-backstop-foreign-" + uuid.NewString()
	createLinkedForeignWAMIDRow(t, app.DB, account, foreignWAMID)
	foreign := orphanStatusBody(t, account, orphanStatusAt(foreignWAMID, "read",
		time.Now().Add(-whatsAppCoexistenceOrphanStatusGrace-time.Hour)))
	assert.Equal(t, http.StatusServiceUnavailable, signedWebhookStatusCode(t, app, foreign))
}

// Gap B: replay guards.

// templateEntryBody is one webhook POST whose single entry carries time at and
// the given changes.
func templateEntryBody(t *testing.T, account models.WhatsAppAccount, at time.Time, changes ...map[string]any) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"object": "whatsapp_business_account",
		"entry": []any{map[string]any{
			"id": account.BusinessID, "time": at.Unix(), "changes": changes,
		}},
	})
	require.NoError(t, err)
	return body
}

func storedTemplate(t *testing.T, app *App, id uuid.UUID) models.Template {
	t.Helper()
	var stored models.Template
	require.NoError(t, app.DB.First(&stored, "id = ?", id).Error)
	return stored
}

func createReplayGuardTemplate(t *testing.T, app *App, account models.WhatsAppAccount, updatedAt time.Time) models.Template {
	t.Helper()
	template := models.Template{
		BaseModel:       models.BaseModel{ID: uuid.New()},
		OrganizationID:  account.OrganizationID,
		WhatsAppAccount: account.Name,
		Name:            "synthetic_order_" + strings.ReplaceAll(uuid.NewString()[:8], "-", ""),
		Language:        "en",
		BodyContent:     "Synthetic {{1}}",
		Status:          "PENDING",
	}
	require.NoError(t, app.DB.Create(&template).Error)
	require.NoError(t, app.DB.Model(&models.Template{}).Where("id = ?", template.ID).
		UpdateColumn("updated_at", updatedAt).Error)
	return template
}

// A template status update is applied only when it is newer than the stored
// state: a replay, an older event, or an event older than a local change is
// ignored, an event in the same second with another status is applied, and
// an event without a time applies as before.
func TestTemplateStatusUpdateIsOrderedByEventTime(t *testing.T) {
	app := webhookTestApp(t)
	account := orphanStatusTestAccount(t, app, false)
	base := time.Now().Add(-time.Hour).Truncate(time.Second)
	template := createReplayGuardTemplate(t, app, account, base)
	apply := func(at time.Time, event string) models.Template {
		var unix int64
		if !at.IsZero() {
			unix = at.Unix()
		}
		app.processTemplateStatusUpdate(account.BusinessID, unix, event, template.Name, template.Language, "NONE")
		return storedTemplate(t, app, template.ID)
	}

	stored := apply(base.Add(10*time.Second), "APPROVED")
	assert.Equal(t, "APPROVED", stored.Status)
	assert.Equal(t, base.Add(10*time.Second).Unix(), stored.UpdatedAt.Unix(), "updated_at moves to the event's second")

	stored = apply(base.Add(10*time.Second), "APPROVED")
	assert.Equal(t, base.Add(10*time.Second).Unix(), stored.UpdatedAt.Unix(), "a replay changes nothing")

	stored = apply(base.Add(20*time.Second), "PAUSED")
	assert.Equal(t, "PAUSED", stored.Status)

	stored = apply(base.Add(10*time.Second), "APPROVED")
	assert.Equal(t, "PAUSED", stored.Status, "a replayed older event never overwrites a newer status")
	assert.Equal(t, base.Add(20*time.Second).Unix(), stored.UpdatedAt.Unix())

	stored = apply(base.Add(20*time.Second), "DISABLED")
	assert.Equal(t, "DISABLED", stored.Status, "a different status in the same second is applied")

	stored = apply(time.Time{}, "FLAGGED")
	assert.Equal(t, "FLAGGED", stored.Status, "an event without a time applies as before")

	// A local change (edit, submission or sync) after an event wins over it.
	require.NoError(t, app.DB.Model(&models.Template{}).Where("id = ?", template.ID).
		Updates(map[string]any{"status": "DRAFT"}).Error)
	stored = apply(time.Now().Add(-30*time.Second), "APPROVED")
	assert.Equal(t, "DRAFT", stored.Status, "an event older than the local change is ignored")
	stored = apply(time.Now().Add(time.Minute), "APPROVED")
	assert.Equal(t, "APPROVED", stored.Status)
}

// A template status update batched before a status that must be retried runs
// on every attempt. Once a newer event has been applied, Meta's replays of
// the older POST no longer overwrite it.
func TestWebhookTemplateStatusReplayAfterNewerEventIsIgnored(t *testing.T) {
	app := webhookTestApp(t)
	account := orphanStatusTestAccount(t, app, true)
	template := createReplayGuardTemplate(t, app, account, time.Now().Add(-time.Hour))
	pending := "wamid.synthetic-template-replay-pending-" + uuid.NewString()
	older := time.Now().Add(-10 * time.Minute)
	newer := time.Now().Add(-5 * time.Minute)
	logs := captureStatusTestLogs(app)

	first := templateEntryBody(t, account, older,
		retryPOSTTemplateStatusChange(template, "APPROVED"),
		retryPOSTStatusChange(account, orphanStatusAt(pending, "read", time.Now())),
	)
	assert.Equal(t, http.StatusServiceUnavailable, signedWebhookStatusCode(t, app, first))
	require.Eventually(t, func() bool { return storedTemplate(t, app, template.ID).Status == "APPROVED" },
		10*time.Second, 20*time.Millisecond)

	second := templateEntryBody(t, account, newer, retryPOSTTemplateStatusChange(template, "REJECTED"))
	assert.Equal(t, http.StatusOK, signedWebhookStatusCode(t, app, second))
	require.Eventually(t, func() bool { return storedTemplate(t, app, template.ID).Status == "REJECTED" },
		10*time.Second, 20*time.Millisecond)

	assert.Equal(t, http.StatusServiceUnavailable, signedWebhookStatusCode(t, app, first))
	require.Eventually(t, func() bool {
		return len(logs.lines("warn", templateOlderSkippedLog)) == 1
	}, 10*time.Second, 20*time.Millisecond)
	assert.Equal(t, "REJECTED", storedTemplate(t, app, template.ID).Status)
}

// wsTypes collects the WebSocket message types sent to client until quiet
// passes without one.
func wsTypes(t *testing.T, client *appwebsocket.Client, quiet time.Duration) []string {
	t.Helper()
	var types []string
	for {
		select {
		case data := <-client.SendChan():
			var envelope terminalWSEnvelope
			require.NoError(t, json.Unmarshal(data, &envelope))
			types = append(types, envelope.Type)
		case <-time.After(quiet):
			return types
		}
	}
}

func replayGuardHub(t *testing.T, organizationID uuid.UUID) (*appwebsocket.Hub, *appwebsocket.Client) {
	t.Helper()
	hub := appwebsocket.NewHub(testutil.NopLogger())
	go hub.Run()
	client := appwebsocket.NewClient(hub, nil, uuid.New(), organizationID)
	hub.Register(client)
	testutil.AssertEventually(t, func() bool { return hub.GetClientCount() == 1 }, 2*time.Second, "websocket client registered")
	return hub, client
}

func storedCallLogByCallID(t *testing.T, app *App, organizationID uuid.UUID, callID string) models.CallLog {
	t.Helper()
	var stored models.CallLog
	require.NoError(t, app.DB.Where("organization_id = ? AND whatsapp_call_id = ?", organizationID, callID).First(&stored).Error)
	return stored
}

// Every event of an incoming call applies once. A replayed ringing does not
// ring agents again, a replayed connect does not reset answered_at, and once
// the call has ended no replayed event reopens it, moves ended_at, or rings
// or answers it again.
func TestIncomingCallWebhookReplayGuard(t *testing.T) {
	app := webhookTestApp(t)
	account := orphanStatusTestAccount(t, app, false)
	hub, client := replayGuardHub(t, account.OrganizationID)
	app.WSHub = hub
	app.CallManager = calling.NewManager(&config.CallingConfig{}, nil, app.DB, app.Redis, nil, hub, nil, nil, "", testutil.NopLogger())
	callID := "wacid.synthetic-incoming-" + uuid.NewString()
	caller := "60" + testutil.NewTestGraphObjectID()
	event := func(name string) {
		app.processCallWebhook(account.PhoneID, map[string]any{
			"id": callID, "from": caller, "to": "15550000001", "event": name,
			"timestamp": strconv.FormatInt(time.Now().Unix(), 10),
		})
	}
	const quiet = 300 * time.Millisecond

	event("ringing")
	assert.Equal(t, []string{appwebsocket.TypeCallIncoming}, wsTypes(t, client, quiet))
	event("ringing")
	assert.Empty(t, wsTypes(t, client, quiet), "a replayed ringing does not ring agents again")

	event("connect")
	assert.Equal(t, []string{appwebsocket.TypeCallAnswered}, wsTypes(t, client, quiet))
	answered := storedCallLogByCallID(t, app, account.OrganizationID, callID)
	require.NotNil(t, answered.AnsweredAt)
	assert.Equal(t, models.CallStatusAnswered, answered.Status)
	time.Sleep(10 * time.Millisecond)
	event("connect")
	event("in_call")
	assert.Empty(t, wsTypes(t, client, quiet))
	assert.True(t, answered.AnsweredAt.Equal(*storedCallLogByCallID(t, app, account.OrganizationID, callID).AnsweredAt),
		"a replayed connect does not reset answered_at")

	event("terminate")
	assert.Equal(t, []string{appwebsocket.TypeCallEnded}, wsTypes(t, client, quiet))
	ended := storedCallLogByCallID(t, app, account.OrganizationID, callID)
	require.NotNil(t, ended.EndedAt)
	assert.Equal(t, models.CallStatusMissed, ended.Status)

	time.Sleep(10 * time.Millisecond)
	for _, replay := range []string{"terminate", "connect", "in_call", "ringing", "missed"} {
		event(replay)
	}
	assert.Empty(t, wsTypes(t, client, quiet), "no replayed event of an ended call is announced")
	final := storedCallLogByCallID(t, app, account.OrganizationID, callID)
	assert.Equal(t, models.CallStatusMissed, final.Status)
	assert.True(t, ended.EndedAt.Equal(*final.EndedAt), "ended_at does not move")
	assert.True(t, answered.AnsweredAt.Equal(*final.AnsweredAt))
	assert.Equal(t, ended.Duration, final.Duration)
	assert.Nil(t, app.CallManager.GetSession(callID), "no session is started for an ended call")
	var logs int64
	require.NoError(t, app.DB.Model(&models.CallLog{}).
		Where("organization_id = ? AND whatsapp_call_id = ?", account.OrganizationID, callID).Count(&logs).Error)
	assert.EqualValues(t, 1, logs)
}

// A business-initiated call's events apply once each, in Meta's documented
// order: the connect webhook (with the SDP answer) as soon as the call is
// ready to connect, then the RINGING status, then the ACCEPTED status when the
// user picks up. The first deliveries apply exactly as before the guard (the
// connect answers, the RINGING that follows it still rings, the ACCEPTED sets
// the final answered_at); replays of any of them, or a connect or RINGING
// after the ACCEPTED, change nothing and are not announced.
func TestOutgoingCallStatusReplayGuard(t *testing.T) {
	app := webhookTestApp(t)
	account := orphanStatusTestAccount(t, app, false)
	hub, client := replayGuardHub(t, account.OrganizationID)
	app.WSHub = hub
	manager := calling.NewManager(&config.CallingConfig{}, nil, app.DB, app.Redis, nil, hub, nil, nil, "", testutil.NopLogger())
	app.CallManager = manager
	contact := testutil.CreateTestContact(t, app.DB, account.OrganizationID)
	callLog := createOutgoingCallWebhookTestLog(t, app.DB, account.OrganizationID, contact, "synthetic-outgoing-replay")
	session := &calling.CallSession{
		ID:             callLog.WhatsAppCallID,
		OrganizationID: account.OrganizationID,
		ContactID:      contact.ID,
		CallLogID:      callLog.ID,
		Status:         models.CallStatusInitiating,
		Direction:      models.CallDirectionOutgoing,
		TargetPhone:    contact.PhoneNumber,
	}
	installCallWebhookTestSession(t, manager, session)
	status := func(value string) {
		app.processCallStatusWebhook(account.PhoneID, WebhookStatus{ID: callLog.WhatsAppCallID, Status: value})
	}
	connect := func() {
		app.processCallWebhook(account.PhoneID, map[string]any{
			"id": callLog.WhatsAppCallID, "event": "connect", "direction": "BUSINESS_INITIATED",
		})
	}
	stored := func() models.CallLog {
		var current models.CallLog
		require.NoError(t, app.DB.First(&current, "id = ?", callLog.ID).Error)
		return current
	}
	const quiet = 300 * time.Millisecond

	connect()
	assert.Equal(t, []string{appwebsocket.TypeOutgoingCallAnswered}, wsTypes(t, client, quiet),
		"the connect answers the call, as before the guard")
	connected := stored()
	require.NotNil(t, connected.AnsweredAt)
	connect()
	assert.Empty(t, wsTypes(t, client, quiet), "a replayed connect is not announced")
	assert.True(t, connected.AnsweredAt.Equal(*stored().AnsweredAt))

	status("RINGING")
	assert.Equal(t, []string{appwebsocket.TypeOutgoingCallRinging}, wsTypes(t, client, quiet),
		"the RINGING that follows the connect still applies")
	assert.Equal(t, models.CallStatusRinging, stored().Status)
	status("RINGING")
	assert.Empty(t, wsTypes(t, client, quiet), "a replayed RINGING is not announced")

	time.Sleep(10 * time.Millisecond)
	status("ACCEPTED")
	assert.Equal(t, []string{appwebsocket.TypeOutgoingCallAnswered}, wsTypes(t, client, quiet))
	accepted := stored()
	require.NotNil(t, accepted.AnsweredAt)
	assert.Equal(t, models.CallStatusAnswered, accepted.Status)
	assert.True(t, accepted.AnsweredAt.After(*connected.AnsweredAt), "the ACCEPTED status sets the final answered_at")

	time.Sleep(10 * time.Millisecond)
	status("ACCEPTED")
	status("RINGING")
	connect()
	assert.Empty(t, wsTypes(t, client, quiet), "replayed events are not announced")
	final := stored()
	assert.Equal(t, models.CallStatusAnswered, final.Status, "a replayed RINGING does not move the call back")
	assert.True(t, accepted.AnsweredAt.Equal(*final.AnsweredAt), "a replayed ACCEPTED or connect does not move answered_at")
	assert.Equal(t, models.CallStatusAnswered, manager.GetSession(callLog.WhatsAppCallID).Status)
}

// A terminate for a business-initiated call whose session is gone applies
// once; its replay does not move ended_at or announce the end again. When the
// call had already ended (an agent hangup recorded it, or the live session's
// terminate did), Meta's terminate only corrects the duration, keeps the first
// ended_at, and is announced once.
func TestOrphanedOutgoingCallTerminateReplayGuard(t *testing.T) {
	app := webhookTestApp(t)
	account := orphanStatusTestAccount(t, app, false)
	hub, client := replayGuardHub(t, account.OrganizationID)
	app.WSHub = hub
	contact := testutil.CreateTestContact(t, app.DB, account.OrganizationID)
	const quiet = 300 * time.Millisecond
	terminate := func(callID string, duration int) {
		app.processCallWebhook(account.PhoneID, map[string]any{
			"id": callID, "event": "terminate", "direction": "BUSINESS_INITIATED", "duration": duration,
		})
	}
	stored := func(id uuid.UUID) models.CallLog {
		var current models.CallLog
		require.NoError(t, app.DB.First(&current, "id = ?", id).Error)
		return current
	}

	t.Run("unanswered", func(t *testing.T) {
		callLog := createOutgoingCallWebhookTestLog(t, app.DB, account.OrganizationID, contact, "synthetic-orphan-replay")
		terminate(callLog.WhatsAppCallID, 42)
		assert.Equal(t, []string{appwebsocket.TypeOutgoingCallEnded}, wsTypes(t, client, quiet))
		first := stored(callLog.ID)
		require.NotNil(t, first.EndedAt)
		assert.Equal(t, 42, first.Duration)
		assert.Equal(t, models.CallStatusMissed, first.Status)

		time.Sleep(10 * time.Millisecond)
		terminate(callLog.WhatsAppCallID, 42)
		terminate(callLog.WhatsAppCallID, 0)
		assert.Empty(t, wsTypes(t, client, quiet))
		replayed := stored(callLog.ID)
		assert.True(t, first.EndedAt.Equal(*replayed.EndedAt))
		assert.Equal(t, first.Status, replayed.Status)
		assert.Equal(t, 42, replayed.Duration)
	})

	t.Run("after an agent hangup", func(t *testing.T) {
		callLog := createOutgoingCallWebhookTestLog(t, app.DB, account.OrganizationID, contact, "synthetic-orphan-hangup")
		answeredAt := time.Now().Add(-time.Minute).Truncate(time.Microsecond)
		endedAt := time.Now().Add(-10 * time.Second).Truncate(time.Microsecond)
		require.NoError(t, app.DB.Model(&models.CallLog{}).Where("id = ?", callLog.ID).Updates(map[string]any{
			"status":          models.CallStatusCompleted,
			"answered_at":     answeredAt,
			"ended_at":        endedAt,
			"duration":        49,
			"disconnected_by": models.DisconnectedByAgent,
		}).Error)

		terminate(callLog.WhatsAppCallID, 50)
		assert.Equal(t, []string{appwebsocket.TypeOutgoingCallEnded}, wsTypes(t, client, quiet),
			"Meta's duration corrects the one the hangup recorded")
		corrected := stored(callLog.ID)
		assert.Equal(t, 50, corrected.Duration)
		assert.Equal(t, models.CallStatusCompleted, corrected.Status)
		assert.Equal(t, models.DisconnectedByAgent, corrected.DisconnectedBy)
		require.NotNil(t, corrected.EndedAt)
		assert.True(t, endedAt.Equal(*corrected.EndedAt), "the first ended_at is kept")

		terminate(callLog.WhatsAppCallID, 50)
		assert.Empty(t, wsTypes(t, client, quiet), "the replay is not announced again")
		assert.Equal(t, 50, stored(callLog.ID).Duration)
	})
}

func storedCallPermission(t *testing.T, app *App, id uuid.UUID) models.CallPermission {
	t.Helper()
	var stored models.CallPermission
	require.NoError(t, app.DB.First(&stored, "id = ?", id).Error)
	return stored
}

// A call permission reply applies to the newest request only when it is newer
// than the stored response and not older than the request: a replay, an
// older reply, or a reply that predates a newer request is ignored.
func TestCallPermissionReplyReplayGuard(t *testing.T) {
	app := webhookTestApp(t)
	account := orphanStatusTestAccount(t, app, false)
	hub, client := replayGuardHub(t, account.OrganizationID)
	app.WSHub = hub
	contact := testutil.CreateTestContact(t, app.DB, account.OrganizationID)
	base := time.Now().Add(-time.Hour).Truncate(time.Second)
	first := models.CallPermission{
		BaseModel:       models.BaseModel{ID: uuid.New()},
		OrganizationID:  account.OrganizationID,
		ContactID:       contact.ID,
		WhatsAppAccount: account.Name,
		Status:          models.CallPermissionPending,
		MessageID:       "wamid.synthetic-permission-first-" + uuid.NewString(),
	}
	require.NoError(t, app.DB.Create(&first).Error)
	require.NoError(t, app.DB.Model(&models.CallPermission{}).Where("id = ?", first.ID).
		UpdateColumns(map[string]any{"requested_at": base, "created_at": base}).Error)
	reply := func(response string, at time.Time) {
		var unix int64
		if !at.IsZero() {
			unix = at.Unix()
		}
		app.processCallPermissionReply(account.PhoneID, contact.PhoneNumber, &CallPermissionReplyData{
			Response: response, ResponseSource: "user_action", RepliedAt: unix,
		})
	}
	const quiet = 300 * time.Millisecond

	reply("accept", base.Add(time.Minute))
	assert.Equal(t, []string{appwebsocket.TypeCallPermissionUpdate}, wsTypes(t, client, quiet))
	stored := storedCallPermission(t, app, first.ID)
	assert.Equal(t, models.CallPermissionAccepted, stored.Status)
	require.NotNil(t, stored.RespondedAt)
	assert.Equal(t, base.Add(time.Minute).Unix(), stored.RespondedAt.Unix(), "responded_at is the reply's own time")

	reply("accept", base.Add(time.Minute))
	reply("reject", base.Add(30*time.Second))
	assert.Empty(t, wsTypes(t, client, quiet), "a replay or an older reply is not announced")
	assert.Equal(t, models.CallPermissionAccepted, storedCallPermission(t, app, first.ID).Status)

	reply("reject", base.Add(2*time.Minute))
	assert.Equal(t, []string{appwebsocket.TypeCallPermissionUpdate}, wsTypes(t, client, quiet))
	assert.Equal(t, models.CallPermissionDeclined, storedCallPermission(t, app, first.ID).Status)

	// A newer request: the replayed replies to the first one must not answer it.
	second := models.CallPermission{
		BaseModel:       models.BaseModel{ID: uuid.New()},
		OrganizationID:  account.OrganizationID,
		ContactID:       contact.ID,
		WhatsAppAccount: account.Name,
		Status:          models.CallPermissionPending,
		MessageID:       "wamid.synthetic-permission-second-" + uuid.NewString(),
	}
	require.NoError(t, app.DB.Create(&second).Error)
	reply("accept", base.Add(time.Minute))
	reply("reject", base.Add(2*time.Minute))
	assert.Empty(t, wsTypes(t, client, quiet))
	assert.Equal(t, models.CallPermissionPending, storedCallPermission(t, app, second.ID).Status)

	reply("accept", time.Now())
	assert.Equal(t, []string{appwebsocket.TypeCallPermissionUpdate}, wsTypes(t, client, quiet))
	assert.Equal(t, models.CallPermissionAccepted, storedCallPermission(t, app, second.ID).Status)

	reply("reject", time.Time{})
	assert.Equal(t, []string{appwebsocket.TypeCallPermissionUpdate}, wsTypes(t, client, quiet),
		"a reply without a timestamp applies as before")
	assert.Equal(t, models.CallPermissionDeclined, storedCallPermission(t, app, second.ID).Status)
}

func storedReactionEmojis(t *testing.T, app *App, messageID uuid.UUID) []string {
	t.Helper()
	var stored models.Message
	require.NoError(t, app.DB.First(&stored, "id = ?", messageID).Error)
	raw, _ := stored.Metadata["reactions"].([]any)
	emojis := make([]string, 0, len(raw))
	for _, value := range raw {
		reaction, _ := value.(map[string]any)
		emoji, _ := reaction["emoji"].(string)
		emojis = append(emojis, emoji)
	}
	return emojis
}

// A reaction is ordered by its own WAMID and timestamp per reacting phone: a
// replay or an older reaction never restores a reaction that a newer one
// replaced or removed, and is not announced again.
func TestIncomingReactionReplayGuard(t *testing.T) {
	for _, coexistence := range []bool{false, true} {
		t.Run(coexistenceModeName(coexistence), func(t *testing.T) {
			app, account, contact := outgoingReceiptFixture(t, coexistence)
			hub, client := replayGuardHub(t, account.OrganizationID)
			app.WSHub = hub
			options := DefaultSendOptions()
			options.Async = false
			options.BroadcastWebSocket = false
			options.DispatchWebhook = false
			options.TrackSLA = false
			target := "wamid.synthetic-reaction-replay-target-" + uuid.NewString()
			message, err := app.SendOutgoingMessage(t.Context(), OutgoingMessageRequest{
				Account: account, Contact: contact, Type: models.MessageTypeText,
				Content: "synthetic reaction replay target",
				deliveryOverride: func(_ context.Context, _ *models.Contact) (string, error) {
					return target, nil
				},
			}, options)
			require.NoError(t, err)
			_ = wsTypes(t, client, 100*time.Millisecond)
			base := time.Now().Add(-time.Hour).Unix()
			react := func(emoji, id string, at int64) {
				app.handleIncomingReactionEvent(account, contact.PhoneNumber, target, emoji, "Patient", id,
					strconv.FormatInt(at, 10))
			}
			const quiet = 300 * time.Millisecond

			react("\U0001F44D", "wamid.synthetic-reaction-1", base+10)
			assert.Len(t, wsTypes(t, client, quiet), 1)
			assert.Equal(t, []string{"\U0001F44D"}, storedReactionEmojis(t, app, message.ID))

			react("\U0001F44D", "wamid.synthetic-reaction-1", base+10)
			assert.Empty(t, wsTypes(t, client, quiet), "a replay is not announced again")

			react("❤️", "wamid.synthetic-reaction-2", base+20)
			assert.Len(t, wsTypes(t, client, quiet), 1)
			react("\U0001F44D", "wamid.synthetic-reaction-1", base+10)
			assert.Empty(t, wsTypes(t, client, quiet))
			assert.Equal(t, []string{"❤️"}, storedReactionEmojis(t, app, message.ID),
				"a replayed older reaction does not replace a newer one")

			react("", "wamid.synthetic-reaction-3", base+30)
			assert.Len(t, wsTypes(t, client, quiet), 1)
			assert.Empty(t, storedReactionEmojis(t, app, message.ID))
			react("❤️", "wamid.synthetic-reaction-2", base+20)
			assert.Empty(t, storedReactionEmojis(t, app, message.ID), "a removed reaction is not restored by a replay")

			app.handleIncomingReaction(account, contact.PhoneNumber, target, "\U0001F525", "Patient")
			assert.Equal(t, []string{"\U0001F525"}, storedReactionEmojis(t, app, message.ID),
				"a reaction without an event identity applies as before")
		})
	}
}
