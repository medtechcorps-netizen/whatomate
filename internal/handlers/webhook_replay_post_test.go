package handlers

import (
	"context"
	"net/http"
	"strconv"
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

// Replays of a whole webhook POST. Meta resends an unacknowledged POST with
// the identical body, so every change ahead of a status that must be retried
// runs again on every attempt (see statusRetryCarried in WebhookHandler).
// These tests deliver each guarded event type in such a POST, again and again,
// interleaved with the later events of the same object, and check that it is
// applied and announced once.

// replayPOSTWithPendingStatus builds one POST whose guarded change comes first
// and is followed by a status that has to be retried (its WAMID is never
// stored), so every delivery of the POST is answered 503 and replayed.
func replayPOSTWithPendingStatus(t *testing.T, account models.WhatsAppAccount, guarded map[string]any) []byte {
	t.Helper()
	pending := "wamid.synthetic-replay-pending-" + uuid.NewString()
	return retryPOSTBody(t, account, []map[string]any{
		guarded,
		retryPOSTStatusChange(account, orphanStatusAt(pending, "read", time.Now())),
	})
}

// deliverReplayedPOST delivers body attempts times, as Meta does until it is
// acknowledged, checks that each attempt asks Meta to retry, and waits for the
// asynchronous work each attempt started.
func deliverReplayedPOST(t *testing.T, app *App, body []byte, attempts int) {
	t.Helper()
	for attempt := 1; attempt <= attempts; attempt++ {
		require.Equal(t, http.StatusServiceUnavailable, signedWebhookStatusCode(t, app, body), "attempt %d", attempt)
		app.rootApp().wg.Wait()
	}
}

// callsChange is a "calls" change carrying call events and call statuses.
func callsChange(account models.WhatsAppAccount, calls []any, statuses []any) map[string]any {
	value := map[string]any{
		"messaging_product": "whatsapp",
		"metadata":          map[string]any{"phone_number_id": account.PhoneID},
	}
	if len(calls) > 0 {
		value["calls"] = calls
	}
	if len(statuses) > 0 {
		value["statuses"] = statuses
	}
	return map[string]any{"field": "calls", "value": value}
}

// inboundMessagesChange is a "messages" change carrying one inbound message.
func inboundMessagesChange(account models.WhatsAppAccount, message map[string]any) map[string]any {
	return map[string]any{
		"field": "messages",
		"value": map[string]any{
			"messaging_product": "whatsapp",
			"metadata":          map[string]any{"phone_number_id": account.PhoneID},
			"messages":          []any{message},
		},
	}
}

// signedReplayFixture is outgoingReceiptFixture configured to accept signed
// webhook POSTs.
func signedReplayFixture(t *testing.T, coexistence bool) (*App, *models.WhatsAppAccount, *models.Contact) {
	t.Helper()
	app, account, contact := outgoingReceiptFixture(t, coexistence)
	app.Config = &config.Config{WhatsApp: config.WhatsAppConfig{AppSecret: webhookTestAppSecret}}
	account.AppSecret = webhookTestAppSecret
	require.NoError(t, app.DB.Model(&models.WhatsAppAccount{}).Where(
		"id = ? AND organization_id = ?", account.ID, account.OrganizationID,
	).Update("app_secret", account.AppSecret).Error)
	app.InvalidateWhatsAppAccountCache(account.PhoneID)
	return app, account, contact
}

func newReplayGuardCallManager(app *App, hub *appwebsocket.Hub) *calling.Manager {
	return calling.NewManager(&config.CallingConfig{}, nil, app.DB, app.Redis, nil, hub, nil, nil, "", testutil.NopLogger())
}

func replayGuardSendOptions() MessageSendOptions {
	options := DefaultSendOptions()
	options.Async = false
	options.BroadcastWebSocket = false
	options.DispatchWebhook = false
	options.TrackSLA = false
	return options
}

// An incoming call's ringing, connect and terminate, each replayed in its own
// POST before and after the later events of the call: agents are rung once,
// the answer is recorded and announced once, the end is recorded and
// announced once, and nothing reopens or re-rings the ended call.
func TestWebhookReplayedPOSTAppliesIncomingCallEventsOnce(t *testing.T) {
	app := webhookTestApp(t)
	account := orphanStatusTestAccount(t, app, true)
	hub, client := replayGuardHub(t, account.OrganizationID)
	app.WSHub = hub
	app.CallManager = newReplayGuardCallManager(app, hub)
	callID := "wacid.synthetic-replayed-post-incoming-" + uuid.NewString()
	caller := "60" + testutil.NewTestGraphObjectID()
	start := time.Now().Add(-time.Minute)
	post := func(event string, at time.Time) []byte {
		return replayPOSTWithPendingStatus(t, account, callsChange(account, []any{map[string]any{
			"id": callID, "from": caller, "to": "15550000001", "event": event,
			"timestamp": strconv.FormatInt(at.Unix(), 10),
		}}, nil))
	}
	ringing := post("ringing", start)
	connect := post("connect", start.Add(5*time.Second))
	terminate := post("terminate", start.Add(30*time.Second))
	stored := func() models.CallLog { return storedCallLogByCallID(t, app, account.OrganizationID, callID) }
	const quiet = 300 * time.Millisecond

	deliverReplayedPOST(t, app, ringing, 3)
	assert.Equal(t, []string{appwebsocket.TypeCallIncoming}, wsTypes(t, client, quiet), "agents are rung once")

	deliverReplayedPOST(t, app, connect, 2)
	deliverReplayedPOST(t, app, ringing, 1)
	deliverReplayedPOST(t, app, connect, 1)
	assert.Equal(t, []string{appwebsocket.TypeCallAnswered}, wsTypes(t, client, quiet), "the answer is announced once")
	answered := stored()
	require.NotNil(t, answered.AnsweredAt)
	assert.Equal(t, models.CallStatusAnswered, answered.Status)

	deliverReplayedPOST(t, app, terminate, 2)
	assert.Equal(t, []string{appwebsocket.TypeCallEnded}, wsTypes(t, client, quiet), "the end is announced once")
	ended := stored()
	require.NotNil(t, ended.EndedAt)

	time.Sleep(10 * time.Millisecond)
	for _, body := range [][]byte{ringing, connect, terminate, connect, ringing} {
		deliverReplayedPOST(t, app, body, 1)
	}
	assert.Empty(t, wsTypes(t, client, quiet), "no replay of an ended call is announced")
	final := stored()
	assert.Equal(t, ended.Status, final.Status)
	assert.True(t, ended.EndedAt.Equal(*final.EndedAt), "ended_at does not move")
	assert.True(t, answered.AnsweredAt.Equal(*final.AnsweredAt), "answered_at does not move")
	assert.Equal(t, ended.Duration, final.Duration)
	assert.Nil(t, app.CallManager.GetSession(callID), "no session is started for the ended call")
	var logs int64
	require.NoError(t, app.DB.Model(&models.CallLog{}).
		Where("organization_id = ? AND whatsapp_call_id = ?", account.OrganizationID, callID).Count(&logs).Error)
	assert.EqualValues(t, 1, logs)
}

// A business-initiated call's connect event and RINGING and ACCEPTED
// statuses, each replayed in its own POST in and out of Meta's order, are
// each applied and announced once, and the answered_at of the ACCEPTED
// status stands.
func TestWebhookReplayedPOSTAppliesOutgoingCallEventsOnce(t *testing.T) {
	app := webhookTestApp(t)
	account := orphanStatusTestAccount(t, app, true)
	hub, client := replayGuardHub(t, account.OrganizationID)
	app.WSHub = hub
	manager := newReplayGuardCallManager(app, hub)
	app.CallManager = manager
	contact := testutil.CreateTestContact(t, app.DB, account.OrganizationID)
	callLog := createOutgoingCallWebhookTestLog(t, app.DB, account.OrganizationID, contact, "synthetic-replayed-post-outgoing")
	installCallWebhookTestSession(t, manager, &calling.CallSession{
		ID:             callLog.WhatsAppCallID,
		OrganizationID: account.OrganizationID,
		ContactID:      contact.ID,
		CallLogID:      callLog.ID,
		Status:         models.CallStatusInitiating,
		Direction:      models.CallDirectionOutgoing,
		TargetPhone:    contact.PhoneNumber,
	})
	now := time.Now()
	connect := replayPOSTWithPendingStatus(t, account, callsChange(account, []any{map[string]any{
		"id": callLog.WhatsAppCallID, "event": "connect", "direction": "BUSINESS_INITIATED",
		"timestamp": strconv.FormatInt(now.Unix(), 10),
	}}, nil))
	status := func(value string) []byte {
		return replayPOSTWithPendingStatus(t, account, callsChange(account, nil, []any{map[string]any{
			"id": callLog.WhatsAppCallID, "status": value, "timestamp": strconv.FormatInt(now.Unix(), 10),
			"recipient_id": contact.PhoneNumber,
		}}))
	}
	ringing := status("RINGING")
	accepted := status("ACCEPTED")
	stored := func() models.CallLog {
		var current models.CallLog
		require.NoError(t, app.DB.First(&current, "id = ?", callLog.ID).Error)
		return current
	}
	const quiet = 300 * time.Millisecond

	deliverReplayedPOST(t, app, connect, 2)
	assert.Equal(t, []string{appwebsocket.TypeOutgoingCallAnswered}, wsTypes(t, client, quiet))
	deliverReplayedPOST(t, app, ringing, 2)
	deliverReplayedPOST(t, app, connect, 1)
	assert.Equal(t, []string{appwebsocket.TypeOutgoingCallRinging}, wsTypes(t, client, quiet))
	assert.Equal(t, models.CallStatusRinging, stored().Status)

	time.Sleep(10 * time.Millisecond)
	deliverReplayedPOST(t, app, accepted, 2)
	assert.Equal(t, []string{appwebsocket.TypeOutgoingCallAnswered}, wsTypes(t, client, quiet))
	answered := stored()
	require.NotNil(t, answered.AnsweredAt)

	time.Sleep(10 * time.Millisecond)
	for _, body := range [][]byte{ringing, connect, accepted, ringing} {
		deliverReplayedPOST(t, app, body, 1)
	}
	assert.Empty(t, wsTypes(t, client, quiet), "no replay is announced")
	final := stored()
	assert.Equal(t, models.CallStatusAnswered, final.Status)
	assert.True(t, answered.AnsweredAt.Equal(*final.AnsweredAt))
}

// A terminate for a business-initiated call whose session is gone, replayed
// in its POST, ends the call log once.
func TestWebhookReplayedPOSTAppliesOrphanedOutgoingTerminateOnce(t *testing.T) {
	app := webhookTestApp(t)
	account := orphanStatusTestAccount(t, app, true)
	hub, client := replayGuardHub(t, account.OrganizationID)
	app.WSHub = hub
	contact := testutil.CreateTestContact(t, app.DB, account.OrganizationID)
	callLog := createOutgoingCallWebhookTestLog(t, app.DB, account.OrganizationID, contact, "synthetic-replayed-post-orphan")
	body := replayPOSTWithPendingStatus(t, account, retryPOSTCallTerminateChange(account, callLog.WhatsAppCallID, time.Now()))
	const quiet = 300 * time.Millisecond

	deliverReplayedPOST(t, app, body, 1)
	assert.Equal(t, []string{appwebsocket.TypeOutgoingCallEnded}, wsTypes(t, client, quiet))
	var first models.CallLog
	require.NoError(t, app.DB.First(&first, "id = ?", callLog.ID).Error)
	require.NotNil(t, first.EndedAt)

	time.Sleep(10 * time.Millisecond)
	deliverReplayedPOST(t, app, body, 3)
	assert.Empty(t, wsTypes(t, client, quiet))
	var final models.CallLog
	require.NoError(t, app.DB.First(&final, "id = ?", callLog.ID).Error)
	assert.True(t, first.EndedAt.Equal(*final.EndedAt))
	assert.Equal(t, 42, final.Duration)
	assert.Equal(t, models.CallStatusMissed, final.Status)
}

// A connect or RINGING that arrives for the first time only after the
// ACCEPTED status (its own POST was held back behind a retried status, for
// example) is late: it neither moves the answered call back to ringing nor
// replaces the answered_at of the pick-up.
func TestOutgoingCallLateEventsAfterAcceptedAreIgnored(t *testing.T) {
	app := webhookTestApp(t)
	account := orphanStatusTestAccount(t, app, true)
	hub, client := replayGuardHub(t, account.OrganizationID)
	app.WSHub = hub
	manager := newReplayGuardCallManager(app, hub)
	app.CallManager = manager
	contact := testutil.CreateTestContact(t, app.DB, account.OrganizationID)
	callLog := createOutgoingCallWebhookTestLog(t, app.DB, account.OrganizationID, contact, "synthetic-outgoing-late")
	installCallWebhookTestSession(t, manager, &calling.CallSession{
		ID:             callLog.WhatsAppCallID,
		OrganizationID: account.OrganizationID,
		ContactID:      contact.ID,
		CallLogID:      callLog.ID,
		Status:         models.CallStatusInitiating,
		Direction:      models.CallDirectionOutgoing,
		TargetPhone:    contact.PhoneNumber,
	})
	stored := func() models.CallLog {
		var current models.CallLog
		require.NoError(t, app.DB.First(&current, "id = ?", callLog.ID).Error)
		return current
	}
	const quiet = 300 * time.Millisecond

	app.processCallStatusWebhook(account.PhoneID, WebhookStatus{ID: callLog.WhatsAppCallID, Status: "ACCEPTED"})
	assert.Equal(t, []string{appwebsocket.TypeOutgoingCallAnswered}, wsTypes(t, client, quiet))
	accepted := stored()
	require.NotNil(t, accepted.AnsweredAt)

	time.Sleep(10 * time.Millisecond)
	app.processCallStatusWebhook(account.PhoneID, WebhookStatus{ID: callLog.WhatsAppCallID, Status: "RINGING"})
	app.processCallWebhook(account.PhoneID, map[string]any{
		"id": callLog.WhatsAppCallID, "event": "connect", "direction": "BUSINESS_INITIATED",
	})
	assert.Empty(t, wsTypes(t, client, quiet), "late events are not announced")
	final := stored()
	assert.Equal(t, models.CallStatusAnswered, final.Status)
	assert.True(t, accepted.AnsweredAt.Equal(*final.AnsweredAt))
	assert.Equal(t, models.CallStatusAnswered, manager.GetSession(callLog.WhatsAppCallID).Status)
}

// An incoming call event that carries an error fails the call once. Its
// replay changes nothing: the failure, its message and its ended_at stay as
// the first delivery recorded them, and nothing is announced again. A replay
// of the end event whose own error failed the call is stopped by the
// ended-call guard, before any claim.
func TestIncomingCallErrorEventReplayKeepsFailure(t *testing.T) {
	app := webhookTestApp(t)
	account := orphanStatusTestAccount(t, app, true)
	hub, client := replayGuardHub(t, account.OrganizationID)
	app.WSHub = hub
	app.CallManager = newReplayGuardCallManager(app, hub)
	callID := "wacid.synthetic-incoming-error-" + uuid.NewString()
	caller := "60" + testutil.NewTestGraphObjectID()
	ringing := replayPOSTWithPendingStatus(t, account, callsChange(account, []any{map[string]any{
		"id": callID, "from": caller, "to": "15550000001", "event": "ringing",
		"timestamp": strconv.FormatInt(time.Now().Unix(), 10),
	}}, nil))
	failed := replayPOSTWithPendingStatus(t, account, callsChange(account, []any{map[string]any{
		"id": callID, "from": caller, "to": "15550000001", "event": "terminate",
		"timestamp": strconv.FormatInt(time.Now().Unix(), 10),
		"error":     map[string]any{"code": 138000, "message": "synthetic call failure"},
	}}, nil))
	const quiet = 300 * time.Millisecond
	logs := captureStatusTestLogs(app)

	deliverReplayedPOST(t, app, ringing, 1)
	deliverReplayedPOST(t, app, failed, 1)
	assert.Equal(t, []string{appwebsocket.TypeCallIncoming, appwebsocket.TypeCallEnded}, wsTypes(t, client, quiet))
	first := storedCallLogByCallID(t, app, account.OrganizationID, callID)
	assert.Equal(t, models.CallStatusFailed, first.Status)
	assert.Equal(t, "synthetic call failure", first.ErrorMessage)
	require.NotNil(t, first.EndedAt)

	time.Sleep(10 * time.Millisecond)
	deliverReplayedPOST(t, app, failed, 2)
	deliverReplayedPOST(t, app, ringing, 1)
	assert.Empty(t, wsTypes(t, client, quiet))
	final := storedCallLogByCallID(t, app, account.OrganizationID, callID)
	assert.Equal(t, models.CallStatusFailed, final.Status)
	assert.Equal(t, first.ErrorMessage, final.ErrorMessage)
	assert.True(t, first.EndedAt.Equal(*final.EndedAt), "a replayed error does not move ended_at")
	assert.Len(t, logs.lines("info", "Ignoring call event for a call that has already ended"), 3)
	assert.Empty(t, logs.lines("info", "Ignoring repeated call end event"))
}

// A template status update replayed in its POST is applied once and moves
// updated_at once; a replay after a newer event never overwrites it
// (TestWebhookTemplateStatusReplayAfterNewerEventIsIgnored).
func TestWebhookReplayedPOSTAppliesTemplateStatusOnce(t *testing.T) {
	app := webhookTestApp(t)
	account := orphanStatusTestAccount(t, app, true)
	template := createReplayGuardTemplate(t, app, account, time.Now().Add(-time.Hour))
	logs := captureStatusTestLogs(app)
	eventAt := time.Now().Add(-time.Minute)
	pending := "wamid.synthetic-template-replay-" + uuid.NewString()
	body := templateEntryBody(t, account, eventAt,
		retryPOSTTemplateStatusChange(template, "APPROVED"),
		retryPOSTStatusChange(account, orphanStatusAt(pending, "read", time.Now())),
	)
	const notApplied = templateReplaySkippedLog

	deliverReplayedPOST(t, app, body, 1)
	require.Eventually(t, func() bool { return storedTemplate(t, app, template.ID).Status == "APPROVED" },
		10*time.Second, 20*time.Millisecond)
	assert.Equal(t, eventAt.Unix(), storedTemplate(t, app, template.ID).UpdatedAt.Unix())

	deliverReplayedPOST(t, app, body, 3)
	require.Eventually(t, func() bool { return len(logs.lines("info", notApplied)) == 3 },
		10*time.Second, 20*time.Millisecond, "every replay is recognised")
	stored := storedTemplate(t, app, template.ID)
	assert.Equal(t, "APPROVED", stored.Status)
	assert.Equal(t, eventAt.Unix(), stored.UpdatedAt.Unix())
	assert.Len(t, logs.lines("info", retryTestTemplateUpdatedLog), 1, "the update is applied once")
}

// A call permission reply replayed in its POST answers the request once; a
// newer reply then wins over every replay of the older one.
func TestWebhookReplayedPOSTAppliesCallPermissionReplyOnce(t *testing.T) {
	app := webhookTestApp(t)
	account := orphanStatusTestAccount(t, app, true)
	hub, client := replayGuardHub(t, account.OrganizationID)
	app.WSHub = hub
	contact := testutil.CreateTestContact(t, app.DB, account.OrganizationID)
	requestedAt := time.Now().Add(-10 * time.Minute).Truncate(time.Second)
	permission := models.CallPermission{
		BaseModel:       models.BaseModel{ID: uuid.New()},
		OrganizationID:  account.OrganizationID,
		ContactID:       contact.ID,
		WhatsAppAccount: account.Name,
		Status:          models.CallPermissionPending,
		MessageID:       "wamid.synthetic-replayed-post-permission-" + uuid.NewString(),
	}
	require.NoError(t, app.DB.Create(&permission).Error)
	require.NoError(t, app.DB.Model(&models.CallPermission{}).Where("id = ?", permission.ID).
		UpdateColumns(map[string]any{"requested_at": requestedAt, "created_at": requestedAt}).Error)
	reply := func(response string, at time.Time) []byte {
		return replayPOSTWithPendingStatus(t, account, inboundMessagesChange(account, map[string]any{
			"from": contact.PhoneNumber, "id": "wamid.synthetic-permission-reply-" + uuid.NewString(),
			"timestamp": strconv.FormatInt(at.Unix(), 10), "type": "interactive",
			"interactive": map[string]any{
				"type": "call_permission_reply",
				"call_permission_reply": map[string]any{
					"response": response, "is_permanent": false,
					"expiration_timestamp": at.Add(7 * 24 * time.Hour).Unix(),
					"response_source":      "user_action",
				},
			},
		}))
	}
	// Each delivery runs the reply in its own goroutine, which the handler does
	// not wait for; it logs exactly one of these lines.
	logs := captureStatusTestLogs(app)
	handled := func() int {
		return len(logs.lines("info", "Ignoring replayed or older call permission reply")) +
			len(logs.lines("info", "Call permission accepted")) +
			len(logs.lines("info", "Call permission declined"))
	}
	deliver := func(body []byte, attempts int) {
		t.Helper()
		before := handled()
		deliverReplayedPOST(t, app, body, attempts)
		require.Eventually(t, func() bool { return handled() == before+attempts },
			10*time.Second, 10*time.Millisecond, "every delivery runs the reply")
	}
	const quiet = 300 * time.Millisecond
	accepted := reply("accept", requestedAt.Add(2*time.Minute))
	declined := reply("reject", requestedAt.Add(5*time.Minute))

	deliver(accepted, 1)
	assert.Equal(t, []string{appwebsocket.TypeCallPermissionUpdate}, wsTypes(t, client, quiet))
	deliver(accepted, 2)
	assert.Empty(t, wsTypes(t, client, quiet), "a replayed reply is not announced again")
	stored := storedCallPermission(t, app, permission.ID)
	assert.Equal(t, models.CallPermissionAccepted, stored.Status)
	require.NotNil(t, stored.RespondedAt)
	assert.Equal(t, requestedAt.Add(2*time.Minute).Unix(), stored.RespondedAt.Unix())

	deliver(declined, 1)
	assert.Equal(t, []string{appwebsocket.TypeCallPermissionUpdate}, wsTypes(t, client, quiet))
	deliver(accepted, 2)
	deliver(declined, 1)
	assert.Empty(t, wsTypes(t, client, quiet))
	assert.Len(t, logs.lines("info", "Ignoring replayed or older call permission reply"), 5,
		"every replay is recognised as one")
	assert.Equal(t, models.CallPermissionDeclined, storedCallPermission(t, app, permission.ID).Status,
		"a replay of the older reply does not overwrite the newer one")
}

// An incoming reaction replayed in its POST is applied and announced once; a
// replay of an older reaction neither replaces a newer one nor restores a
// removed one.
func TestWebhookReplayedPOSTAppliesReactionOnce(t *testing.T) {
	for _, coexistence := range []bool{false, true} {
		t.Run(coexistenceModeName(coexistence), func(t *testing.T) {
			app, account, contact := signedReplayFixture(t, coexistence)
			hub, client := replayGuardHub(t, account.OrganizationID)
			app.WSHub = hub
			target := "wamid.synthetic-replayed-post-reaction-target-" + uuid.NewString()
			message, err := app.SendOutgoingMessage(t.Context(), OutgoingMessageRequest{
				Account: account, Contact: contact, Type: models.MessageTypeText,
				Content: "synthetic replayed reaction target",
				deliveryOverride: func(_ context.Context, _ *models.Contact) (string, error) {
					return target, nil
				},
			}, replayGuardSendOptions())
			require.NoError(t, err)
			_ = wsTypes(t, client, 100*time.Millisecond)
			base := time.Now().Add(-time.Hour)
			react := func(emoji string, at time.Time) []byte {
				return replayPOSTWithPendingStatus(t, *account, inboundMessagesChange(*account, map[string]any{
					"from": contact.PhoneNumber, "id": "wamid.synthetic-replayed-reaction-" + uuid.NewString(),
					"timestamp": strconv.FormatInt(at.Unix(), 10), "type": "reaction",
					"reaction": map[string]any{"message_id": target, "emoji": emoji},
				}))
			}
			thumbs := react("\U0001F44D", base.Add(10*time.Second))
			heart := react("❤️", base.Add(20*time.Second))
			removed := react("", base.Add(30*time.Second))
			const quiet = 300 * time.Millisecond

			deliverReplayedPOST(t, app, thumbs, 3)
			assert.Equal(t, []string{"reaction_update"}, wsTypes(t, client, quiet), "the reaction is announced once")
			assert.Equal(t, []string{"\U0001F44D"}, storedReactionEmojis(t, app, message.ID))

			deliverReplayedPOST(t, app, heart, 1)
			deliverReplayedPOST(t, app, thumbs, 1)
			deliverReplayedPOST(t, app, heart, 1)
			assert.Equal(t, []string{"reaction_update"}, wsTypes(t, client, quiet))
			assert.Equal(t, []string{"❤️"}, storedReactionEmojis(t, app, message.ID),
				"a replayed older reaction does not replace the newer one")

			deliverReplayedPOST(t, app, removed, 1)
			assert.Equal(t, []string{"reaction_update"}, wsTypes(t, client, quiet))
			for _, body := range [][]byte{thumbs, heart, removed} {
				deliverReplayedPOST(t, app, body, 1)
			}
			assert.Empty(t, wsTypes(t, client, quiet))
			assert.Empty(t, storedReactionEmojis(t, app, message.ID), "a replay does not restore the removed reaction")
		})
	}
}

// The case that blocked the switch to Coexistence, in one window: on a
// Coexistence account the statuses of a reaction and of a call permission
// request that ReReply sent are acknowledged at once, while an early receipt
// for a ReReply Message whose WAMID has not committed yet, delivered in the
// very same window and even in the same POST, is still retried; Meta's replay
// of the same bodies then applies it to the Message.
func TestWebhookCoexistenceAcknowledgesNonMessageSendsWhileEarlyReceiptRetries(t *testing.T) {
	app, account, contact := signedReplayFixture(t, true)
	require.True(t, account.IsSMB)
	logs := captureStatusTestLogs(app)

	// A reaction ReReply sent to the customer's message, recorded from the
	// Graph response.
	reacted := models.Message{
		BaseModel:         models.BaseModel{ID: uuid.New()},
		OrganizationID:    account.OrganizationID,
		WhatsAppAccount:   account.Name,
		ContactID:         contact.ID,
		WhatsAppMessageID: "wamid.synthetic-switch-reacted-" + uuid.NewString(),
		Direction:         models.DirectionIncoming,
		MessageType:       models.MessageTypeText,
		Content:           "synthetic customer message",
		Status:            models.MessageStatusDelivered,
		Metadata:          models.JSONB{},
	}
	require.NoError(t, app.DB.Create(&reacted).Error)
	reactionWAMID := "wamid.synthetic-switch-reaction-" + uuid.NewString()
	graph := newReactionGraphServer(t, reactionWAMID)
	app.Config.WhatsApp.BaseURL = graph.URL
	app.HTTPClient = graph.Client()
	require.NoError(t, app.sendWhatsAppReactionGuarded(account, contact, &reacted, "\U0001F44D"))
	require.Equal(t, []string{reactionWAMID}, storedSentReactionWAMIDs(t, app, reacted.ID))

	// A call permission request ReReply sent, stored only as its row.
	permissionWAMID := "wamid.synthetic-switch-permission-" + uuid.NewString()
	require.NoError(t, app.DB.Create(&models.CallPermission{
		BaseModel:       models.BaseModel{ID: uuid.New()},
		OrganizationID:  account.OrganizationID,
		ContactID:       contact.ID,
		WhatsAppAccount: account.Name,
		Status:          models.CallPermissionPending,
		MessageID:       permissionWAMID,
	}).Error)

	now := time.Now()
	status := func(wamid, value string) map[string]any {
		item := orphanStatusAt(wamid, value, now)
		item["recipient_id"] = contact.PhoneNumber
		return item
	}
	messageWAMID := "wamid.synthetic-switch-message-" + uuid.NewString()
	nonMessageSends := orphanStatusBody(t, *account,
		status(reactionWAMID, "sent"),
		status(permissionWAMID, "delivered"),
		status(permissionWAMID, "read"),
	)
	mixed := orphanStatusBody(t, *account,
		status(reactionWAMID, "sent"),
		status(messageWAMID, "delivered"),
		status(permissionWAMID, "read"),
	)
	earlyReceipt := orphanStatusBody(t, *account, status(messageWAMID, "delivered"))

	var inFlight struct {
		nonMessageSends, mixed, earlyReceipt int
		messageStored                        bool
	}
	message, err := app.SendOutgoingMessage(t.Context(), OutgoingMessageRequest{
		Account: account, Contact: contact, Type: models.MessageTypeText,
		Content: "synthetic message whose receipt overtakes its WAMID",
		deliveryOverride: func(context.Context, *models.Contact) (string, error) {
			// Meta delivers these while the Graph send of the Message is still
			// in flight: its WAMID is not stored yet.
			inFlight.nonMessageSends = signedWebhookStatusCode(t, app, nonMessageSends)
			inFlight.mixed = signedWebhookStatusCode(t, app, mixed)
			inFlight.earlyReceipt = signedWebhookStatusCode(t, app, earlyReceipt)
			inFlight.messageStored = countWAMIDMessages(t, app, *account, messageWAMID) > 0
			return messageWAMID, nil
		},
	}, replayGuardSendOptions())
	require.NoError(t, err)
	require.False(t, inFlight.messageStored, "the receipts arrived before the Message's WAMID was stored")
	assert.Equal(t, http.StatusOK, inFlight.nonMessageSends,
		"the reaction and call permission statuses are acknowledged on a Coexistence account")
	assert.Equal(t, http.StatusServiceUnavailable, inFlight.mixed,
		"a POST that also carries the early receipt is retried")
	assert.Equal(t, http.StatusServiceUnavailable, inFlight.earlyReceipt, "the early receipt is retried")
	assert.Len(t, logs.lines("info", nonMessageSendAckLog), 5,
		"each reaction and permission status is acknowledged, also beside the early receipt")
	assert.Len(t, logs.lines("info", statusTestDeferredLog), 2, "the early receipt is an expected wait")

	var pending models.Message
	require.NoError(t, app.DB.First(&pending, "id = ?", message.ID).Error)
	assert.Equal(t, messageWAMID, pending.WhatsAppMessageID)
	assert.NotEqual(t, models.MessageStatusDelivered, pending.Status, "nothing applied the early receipt yet")

	// Meta replays the identical bodies once the WAMID is stored.
	assert.Equal(t, http.StatusOK, signedWebhookStatusCode(t, app, mixed))
	assert.Equal(t, http.StatusOK, signedWebhookStatusCode(t, app, earlyReceipt))
	var delivered models.Message
	require.NoError(t, app.DB.First(&delivered, "id = ?", message.ID).Error)
	assert.Equal(t, models.MessageStatusDelivered, delivered.Status)
	assert.Len(t, logs.lines("info", nonMessageSendAckLog), 7)

	for _, wamid := range []string{reactionWAMID, permissionWAMID} {
		assert.Zero(t, countWAMIDMessages(t, app, *account, wamid), "no Message is created for %s", wamid)
	}
	assert.Empty(t, logs.lines("warn", "Acknowledged WhatsApp status for a message ReReply never stored"),
		"no status needed the Coexistence backstop")
}
