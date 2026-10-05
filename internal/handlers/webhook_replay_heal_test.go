package handlers

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/config"
	"github.com/shridarpatil/whatomate/internal/models"
	appwebsocket "github.com/shridarpatil/whatomate/internal/websocket"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// The replay guards must not cost an account a genuine first delivery that
// the code before them applied: a call's end still ends a call that an
// earlier error failed, and a permission reply always lands on a request and
// is announced unless the contact has a newer reply. The reaction metadata
// writers lock the message row, so none of them drops another's write.

// Calls.

// An error on an earlier event of an incoming call marks the call log failed.
// The call's own terminate is then still applied once, as before the replay
// guards: it records the duration and the end, takes the final status and
// announces call_ended; the error message stays. Its replays, and replays of
// the failed event, change nothing and are not announced.
func TestIncomingCallTerminateAfterAnEarlierErrorIsAppliedOnce(t *testing.T) {
	for _, tc := range []struct {
		name          string
		terminateErr  map[string]any
		wantStatus    models.CallStatus
		wantErrorText string
	}{
		{"terminate without an error", nil, models.CallStatusMissed, "synthetic connect failure"},
		{"terminate with another error", map[string]any{"code": 138001, "message": "synthetic end failure"},
			models.CallStatusFailed, "synthetic end failure"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app := webhookTestApp(t)
			account := orphanStatusTestAccount(t, app, false)
			hub, client := replayGuardHub(t, account.OrganizationID)
			app.WSHub = hub
			app.CallManager = newReplayGuardCallManager(app, hub)
			callID := "wacid.synthetic-error-then-end-" + uuid.NewString()
			caller := "60" + testutil.NewTestGraphObjectID()
			start := time.Now().Add(-time.Minute)
			post := func(event string, at time.Time, callErr map[string]any) []byte {
				call := map[string]any{
					"id": callID, "from": caller, "to": "15550000001", "event": event,
					"timestamp": strconv.FormatInt(at.Unix(), 10),
				}
				if callErr != nil {
					call["error"] = callErr
				}
				return replayPOSTWithPendingStatus(t, account, callsChange(account, []any{call}, nil))
			}
			ringing := post("ringing", start, nil)
			failedConnect := post("connect", start.Add(5*time.Second),
				map[string]any{"code": 138000, "message": "synthetic connect failure"})
			terminate := post("terminate", start.Add(40*time.Second), tc.terminateErr)
			stored := func() models.CallLog { return storedCallLogByCallID(t, app, account.OrganizationID, callID) }
			const quiet = 300 * time.Millisecond

			deliverReplayedPOST(t, app, ringing, 1)
			deliverReplayedPOST(t, app, failedConnect, 1)
			assert.Equal(t, []string{appwebsocket.TypeCallIncoming, appwebsocket.TypeCallAnswered}, wsTypes(t, client, quiet))
			failed := stored()
			require.Equal(t, models.CallStatusFailed, failed.Status)
			require.NotNil(t, failed.EndedAt)
			require.NotNil(t, failed.AnsweredAt)
			answeredAt := time.Now().Add(-30 * time.Second)
			require.NoError(t, app.DB.Model(&models.CallLog{}).Where("id = ?", failed.ID).
				UpdateColumn("answered_at", answeredAt).Error)
			time.Sleep(10 * time.Millisecond)

			deliverReplayedPOST(t, app, terminate, 3)
			assert.Equal(t, []string{appwebsocket.TypeCallEnded}, wsTypes(t, client, quiet), "the end is announced once")
			ended := stored()
			assert.Equal(t, tc.wantStatus, ended.Status)
			assert.Equal(t, tc.wantErrorText, ended.ErrorMessage)
			assert.GreaterOrEqual(t, ended.Duration, 30, "the duration is recorded")
			require.NotNil(t, ended.EndedAt)
			assert.True(t, ended.EndedAt.After(*failed.EndedAt), "the end is recorded")
			assert.Equal(t, models.DisconnectedBySystem, ended.DisconnectedBy)

			time.Sleep(10 * time.Millisecond)
			for _, body := range [][]byte{terminate, failedConnect, ringing, terminate} {
				deliverReplayedPOST(t, app, body, 1)
			}
			assert.Empty(t, wsTypes(t, client, quiet), "no replay is announced")
			final := stored()
			assert.Equal(t, ended.Status, final.Status)
			assert.Equal(t, ended.ErrorMessage, final.ErrorMessage)
			assert.Equal(t, ended.Duration, final.Duration)
			assert.True(t, ended.EndedAt.Equal(*final.EndedAt), "ended_at does not move")
		})
	}
}

// callLogUpdatesWaiting counts the backends of this database that are waiting
// for a lock while updating call_logs.
func callLogUpdatesWaiting(t *testing.T, app *App) int64 {
	t.Helper()
	var waiting int64
	require.NoError(t, app.DB.Raw(`SELECT count(*) FROM pg_stat_activity
		WHERE datname = current_database() AND wait_event_type = 'Lock'
		AND query ILIKE 'UPDATE%call_logs%'`).Scan(&waiting).Error)
	return waiting
}

// The end claim on a failed call log is decided inside its UPDATE. A copy of a
// terminate whose own error failed the call can read the call log before the
// first delivery's end and error commit; its UPDATE then finds that error and
// claims nothing, and the copy neither announces the end nor rewrites the
// error or ended_at.
func TestIncomingCallEndCopyRacingItsOwnErrorChangesNothing(t *testing.T) {
	app := webhookTestApp(t)
	account := orphanStatusTestAccount(t, app, false)
	hub, client := replayGuardHub(t, account.OrganizationID)
	app.WSHub = hub
	app.CallManager = newReplayGuardCallManager(app, hub)
	callID := "wacid.synthetic-end-copy-" + uuid.NewString()
	caller := "60" + testutil.NewTestGraphObjectID()
	event := func(name string, callErr map[string]any) {
		call := map[string]any{
			"id": callID, "from": caller, "to": "15550000001", "event": name,
			"timestamp": strconv.FormatInt(time.Now().Unix(), 10),
		}
		if callErr != nil {
			call["error"] = callErr
		}
		app.processCallWebhook(account.PhoneID, call)
	}
	const quiet = 300 * time.Millisecond
	event("ringing", nil)
	event("connect", nil)
	assert.Len(t, wsTypes(t, client, quiet), 2)
	callLog := storedCallLogByCallID(t, app, account.OrganizationID, callID)
	endedAt := time.Now().Add(-time.Second).Truncate(time.Microsecond)

	lock := app.DB.Begin()
	require.NoError(t, lock.Error)
	require.NoError(t, lock.Exec(`UPDATE call_logs SET status = ?, error_message = ?, ended_at = ?, duration = 30,
		disconnected_by = ? WHERE id = ?`, models.CallStatusFailed, "synthetic end failure", endedAt,
		models.DisconnectedBySystem, callLog.ID).Error)
	done := make(chan struct{})
	go func() {
		defer close(done)
		event("terminate", map[string]any{"code": 138001, "message": "synthetic end failure"})
	}()
	require.Eventually(t, func() bool { return callLogUpdatesWaiting(t, app) == 1 },
		10*time.Second, 10*time.Millisecond, "the copy waits in its end claim")
	require.NoError(t, lock.Commit().Error)
	<-done

	assert.Empty(t, wsTypes(t, client, quiet), "the copy announces nothing")
	final := storedCallLogByCallID(t, app, account.OrganizationID, callID)
	assert.Equal(t, models.CallStatusFailed, final.Status)
	assert.Equal(t, "synthetic end failure", final.ErrorMessage)
	assert.Equal(t, 30, final.Duration)
	require.NotNil(t, final.EndedAt)
	assert.True(t, endedAt.Equal(*final.EndedAt), "ended_at does not move")
}

// Call permission replies.

// wsPermissionStatuses returns the status of every call_permission_update
// received until the client is quiet.
func wsPermissionStatuses(t *testing.T, client *appwebsocket.Client, quiet time.Duration) []string {
	t.Helper()
	var statuses []string
	for {
		select {
		case data := <-client.SendChan():
			var envelope terminalWSEnvelope
			require.NoError(t, json.Unmarshal(data, &envelope))
			if envelope.Type == appwebsocket.TypeCallPermissionUpdate {
				status, _ := envelope.Payload["status"].(string)
				statuses = append(statuses, status)
			}
		case <-time.After(quiet):
			return statuses
		}
	}
}

func countContactCallPermissions(t *testing.T, app *App, contactID uuid.UUID) int64 {
	t.Helper()
	var count int64
	require.NoError(t, app.DB.Model(&models.CallPermission{}).Where("contact_id = ?", contactID).Count(&count).Error)
	return count
}

// A reply that does not name its request, delivered after a newer request was
// sent three minutes after the reply, answers the request sent before it and
// is announced; its replay finds that request already answered.
func TestCallPermissionReplyWithoutContextAnswersTheRequestSentBeforeIt(t *testing.T) {
	app := webhookTestApp(t)
	account := orphanStatusTestAccount(t, app, false)
	hub, client := replayGuardHub(t, account.OrganizationID)
	app.WSHub = hub
	contact := testutil.CreateTestContact(t, app.DB, account.OrganizationID)
	base := time.Now().Add(-time.Hour).Truncate(time.Second)
	first := createReviewCallPermission(t, app, account, contact, "before-first", base)
	second := createReviewCallPermission(t, app, account, contact, "before-second", base.Add(3*time.Minute+5*time.Second))
	logs := captureStatusTestLogs(app)
	reply := func() {
		app.processCallPermissionReply(account.PhoneID, contact.PhoneNumber, &CallPermissionReplyData{
			Response: "accept", ResponseSource: "user_action", RepliedAt: base.Add(5 * time.Second).Unix(),
		})
	}
	const quiet = 300 * time.Millisecond

	reply()
	assert.Equal(t, []string{string(models.CallPermissionAccepted)}, wsPermissionStatuses(t, client, quiet))
	assert.Equal(t, models.CallPermissionAccepted, storedCallPermission(t, app, first.ID).Status)
	assert.Equal(t, models.CallPermissionPending, storedCallPermission(t, app, second.ID).Status,
		"the request sent after the reply is not answered by it")

	reply()
	assert.Empty(t, wsPermissionStatuses(t, client, quiet))
	assert.Len(t, logs.lines("info", permissionIgnoredLog), 1)
	assert.Equal(t, models.CallPermissionPending, storedCallPermission(t, app, second.ID).Status)
	assert.EqualValues(t, 2, countContactCallPermissions(t, app, contact.ID))
}

// requested_at is the server's clock and a reply's time is Meta's. With the
// server 150 seconds ahead, a reply that does not name its request finds no
// request sent before it. It is recorded as an out-of-band reply (requested_at
// is the reply's time) and announced, rather than dropped; its replays find
// that row and create nothing more.
func TestCallPermissionReplyBeforeEveryRequestIsRecordedOutOfBand(t *testing.T) {
	app := webhookTestApp(t)
	account := orphanStatusTestAccount(t, app, false)
	hub, client := replayGuardHub(t, account.OrganizationID)
	app.WSHub = hub
	contact := testutil.CreateTestContact(t, app.DB, account.OrganizationID)
	requestedAt := time.Now().Add(-time.Hour).Truncate(time.Second)
	request := createReviewCallPermission(t, app, account, contact, "skewed", requestedAt)
	repliedAt := requestedAt.Add(-150 * time.Second).Add(10 * time.Second)
	logs := captureStatusTestLogs(app)
	reply := func() {
		app.processCallPermissionReply(account.PhoneID, contact.PhoneNumber, &CallPermissionReplyData{
			Response: "accept", ResponseSource: "user_action", RepliedAt: repliedAt.Unix(),
		})
	}
	const quiet = 300 * time.Millisecond

	reply()
	assert.Equal(t, []string{string(models.CallPermissionAccepted)}, wsPermissionStatuses(t, client, quiet))
	assert.EqualValues(t, 2, countContactCallPermissions(t, app, contact.ID))
	var recorded models.CallPermission
	require.NoError(t, app.DB.Where("contact_id = ? AND id <> ?", contact.ID, request.ID).First(&recorded).Error)
	assert.Equal(t, models.CallPermissionAccepted, recorded.Status)
	assert.Equal(t, repliedAt.Unix(), recorded.RequestedAt.Unix())
	require.NotNil(t, recorded.RespondedAt)
	assert.Equal(t, repliedAt.Unix(), recorded.RespondedAt.Unix())
	assert.Equal(t, models.CallPermissionPending, storedCallPermission(t, app, request.ID).Status)

	reply()
	reply()
	assert.Empty(t, wsPermissionStatuses(t, client, quiet))
	assert.Len(t, logs.lines("info", permissionIgnoredLog), 2)
	assert.EqualValues(t, 2, countContactCallPermissions(t, app, contact.ID), "a replay creates no further row")
}

// Agents hold one permission state per contact. A reply to an earlier request
// that is older than a reply already recorded for another request of the
// contact is recorded on its own request but not announced over the newer
// answer; a newer reply is announced again.
func TestCallPermissionOlderReplyToAnotherRequestIsNotAnnounced(t *testing.T) {
	app := webhookTestApp(t)
	account := orphanStatusTestAccount(t, app, false)
	hub, client := replayGuardHub(t, account.OrganizationID)
	app.WSHub = hub
	contact := testutil.CreateTestContact(t, app.DB, account.OrganizationID)
	base := time.Now().Add(-time.Hour).Truncate(time.Second)
	first := createReviewCallPermission(t, app, account, contact, "order-first", base)
	second := createReviewCallPermission(t, app, account, contact, "order-second", base.Add(time.Minute))
	logs := captureStatusTestLogs(app)
	reply := func(response string, after time.Duration, request models.CallPermission) {
		app.processCallPermissionReply(account.PhoneID, contact.PhoneNumber, &CallPermissionReplyData{
			Response: response, ResponseSource: "user_action", RepliedAt: base.Add(after).Unix(),
			RequestMessageID: request.MessageID,
		})
	}
	const quiet = 300 * time.Millisecond

	reply("reject", 100*time.Second, second)
	assert.Equal(t, []string{string(models.CallPermissionDeclined)}, wsPermissionStatuses(t, client, quiet))

	reply("accept", 90*time.Second, first)
	assert.Empty(t, wsPermissionStatuses(t, client, quiet), "the older answer does not replace the newer one")
	assert.Equal(t, models.CallPermissionAccepted, storedCallPermission(t, app, first.ID).Status, "it is recorded on its request")
	assert.Len(t, logs.lines("info", "Call permission reply recorded but not announced: the contact has a newer reply"), 1)

	reply("accept", 120*time.Second, first)
	assert.Equal(t, []string{string(models.CallPermissionAccepted)}, wsPermissionStatuses(t, client, quiet))
}

// A reply whose write fails is still announced, as before the replay guards:
// it runs after the POST was acknowledged, so Meta does not deliver it again.
func TestCallPermissionReplyIsAnnouncedWhenItsWriteFails(t *testing.T) {
	app := webhookTestApp(t)
	account := orphanStatusTestAccount(t, app, false)
	hub, client := replayGuardHub(t, account.OrganizationID)
	app.WSHub = hub
	contact := testutil.CreateTestContact(t, app.DB, account.OrganizationID)
	permission := createReviewCallPermission(t, app, account, contact, "write-fails", time.Now().Add(-time.Hour))
	logs := captureStatusTestLogs(app)

	sqlDB, err := app.DB.DB()
	require.NoError(t, err)
	failingDB, err := gorm.Open(postgres.New(postgres.Config{Conn: sqlDB}), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	require.NoError(t, err)
	require.NoError(t, failingDB.Callback().Update().Before("gorm:update").Register("test:fail_call_permission_update",
		func(db *gorm.DB) {
			if db.Statement.Table == "call_permissions" {
				_ = db.AddError(errors.New("synthetic call permission write failure"))
			}
		}))
	originalDB := app.DB
	app.DB = failingDB

	app.processCallPermissionReply(account.PhoneID, contact.PhoneNumber, &CallPermissionReplyData{
		Response: "accept", ResponseSource: "user_action", RepliedAt: time.Now().Add(-time.Minute).Unix(),
		RequestMessageID: permission.MessageID,
	})
	assert.Equal(t, []string{string(models.CallPermissionAccepted)}, wsPermissionStatuses(t, client, 300*time.Millisecond))
	assert.Len(t, logs.lines("error", "Failed to update call permission from reply; announcing it anyway"), 1)
	app.DB = originalDB
	assert.Equal(t, models.CallPermissionPending, storedCallPermission(t, app, permission.ID).Status, "nothing was written")
}

// A reply whose context names another contact's request does not touch that
// request; it answers the replying contact's own request.
func TestCallPermissionReplyNamingAnotherContactsRequestLeavesItAlone(t *testing.T) {
	app := webhookTestApp(t)
	account := orphanStatusTestAccount(t, app, false)
	one := testutil.CreateTestContact(t, app.DB, account.OrganizationID)
	two := testutil.CreateTestContact(t, app.DB, account.OrganizationID)
	base := time.Now().Add(-time.Hour).Truncate(time.Second)
	requestOne := createReviewCallPermission(t, app, account, one, "contact-one", base)
	requestTwo := createReviewCallPermission(t, app, account, two, "contact-two", base)

	app.processCallPermissionReply(account.PhoneID, two.PhoneNumber, &CallPermissionReplyData{
		Response: "reject", ResponseSource: "user_action", RepliedAt: base.Add(20 * time.Second).Unix(),
		RequestMessageID: requestOne.MessageID,
	})
	assert.Equal(t, models.CallPermissionPending, storedCallPermission(t, app, requestOne.ID).Status)
	assert.Equal(t, models.CallPermissionDeclined, storedCallPermission(t, app, requestTwo.ID).Status)
}

// Reactions.

func TestRecordSentReactionWAMIDKeepsUpdatedAtAndOtherMetadata(t *testing.T) {
	app := webhookTestApp(t)
	account := orphanStatusTestAccount(t, app, true)
	contact := testutil.CreateTestContact(t, app.DB, account.OrganizationID)
	writtenAt := time.Now().Add(-48 * time.Hour).Truncate(time.Second)
	target := reactionLookupTarget(t, app, account, contact, writtenAt)

	require.NoError(t, app.recordSentWhatsAppReactionWAMID(account.OrganizationID, target.ID, "wamid.synthetic-keep-1"))
	var stored models.Message
	require.NoError(t, app.DB.First(&stored, "id = ?", target.ID).Error)
	assert.Equal(t, writtenAt.Unix(), stored.UpdatedAt.Unix(), "updated_at is unchanged")
	assert.Equal(t, "kept", stored.Metadata["synthetic_keep"])
	assert.Equal(t, []string{"wamid.synthetic-keep-1"}, storedSentReactionWAMIDs(t, app, target.ID))

	other := orphanStatusTestAccount(t, app, true)
	require.Error(t, app.recordSentWhatsAppReactionWAMID(other.OrganizationID, target.ID, "wamid.synthetic-keep-2"))
	assert.Equal(t, []string{"wamid.synthetic-keep-1"}, storedSentReactionWAMIDs(t, app, target.ID))
}

// Everything that rewrites a message's metadata for reactions does so under
// the message's row lock: reactions sent by agents (SendReaction, whose sends
// then record their WAMIDs), WAMID records, customers' reactions and their
// replay record, and the message's own receipts. Run concurrently, none of
// them drops another's write.
func TestReactionMetadataWritersDoNotLoseEachOthersWrites(t *testing.T) {
	app, account, contact, message, target := reactionReviewTarget(t, false)
	var sendCounter atomic.Int64
	graph := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"messaging_product":"whatsapp","messages":[{"id":"wamid.synthetic-agent-reaction-%02d"}]}`,
			sendCounter.Add(1))
	}))
	t.Cleanup(graph.Close)
	app.Config = &config.Config{
		App:      config.AppConfig{EncryptionKey: "test-encryption-key-32-bytes-long"},
		WhatsApp: config.WhatsAppConfig{BaseURL: graph.URL},
	}
	app.HTTPClient = graph.Client()
	logs := captureStatusTestLogs(app)
	adminRole := testutil.CreateAdminRole(t, app.DB, account.OrganizationID)
	const (
		records   = 12
		incoming  = 12
		agents    = 8
		receipts  = 6
		emojiBase = 0x1F600
	)
	users := make([]*models.User, agents)
	for i := range users {
		users[i] = testutil.CreateTestUser(t, app.DB, account.OrganizationID, testutil.WithRoleID(&adminRole.ID))
	}
	base := time.Now().Add(-time.Hour).Unix()
	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make(chan error, records+agents+receipts)
	for i := 0; i < records; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			if err := app.recordSentWhatsAppReactionWAMID(account.OrganizationID, message.ID,
				fmt.Sprintf("wamid.synthetic-recorded-reaction-%02d", i)); err != nil {
				errs <- fmt.Errorf("record %d: %w", i, err)
			}
		}(i)
	}
	for i := 0; i < incoming; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			app.handleIncomingReactionEvent(account, contact.PhoneNumber, target, "C"+strconv.Itoa(i), "Patient",
				fmt.Sprintf("wamid.synthetic-customer-reaction-%02d", i), strconv.FormatInt(base+int64(i), 10))
		}(i)
	}
	for i, user := range users {
		wg.Add(1)
		go func(i int, user *models.User) {
			defer wg.Done()
			<-start
			req := testutil.NewJSONRequest(t, map[string]any{"emoji": string(rune(emojiBase + i))})
			testutil.SetAuthContext(req, account.OrganizationID, user.ID)
			testutil.SetPathParam(req, "id", contact.ID.String())
			testutil.SetPathParam(req, "message_id", message.ID.String())
			if err := app.SendReaction(req); err != nil {
				errs <- fmt.Errorf("send reaction %d: %w", i, err)
			} else if code := req.RequestCtx.Response.StatusCode(); code != http.StatusOK {
				errs <- fmt.Errorf("send reaction %d: status %d", i, code)
			}
		}(i, user)
	}
	for i := 0; i < receipts; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			if err := app.processStatusUpdate(account.PhoneID, WebhookStatus{
				ID: target, Status: []string{"sent", "delivered", "read"}[i%3],
				Timestamp: strconv.FormatInt(time.Now().Unix(), 10), RecipientID: strings.TrimPrefix(contact.PhoneNumber, "+"),
			}); err != nil {
				errs <- fmt.Errorf("receipt %d: %w", i, err)
			}
		}(i)
	}
	close(start)
	wg.Wait()
	app.WaitForBackgroundTasks()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	assert.Empty(t, logs.lines("error", "Failed to update message reaction"))
	assert.Empty(t, logs.lines("warn", "Failed to record sent reaction WAMID"))

	want := make([]string, 0, records+agents)
	for i := 0; i < records; i++ {
		want = append(want, fmt.Sprintf("wamid.synthetic-recorded-reaction-%02d", i))
	}
	for i := 1; i <= agents; i++ {
		want = append(want, fmt.Sprintf("wamid.synthetic-agent-reaction-%02d", i))
	}
	require.LessOrEqual(t, len(want), sentReactionWAMIDsLimit)
	recorded := storedSentReactionWAMIDs(t, app, message.ID)
	slices.Sort(recorded)
	slices.Sort(want)
	assert.Equal(t, want, recorded, "no reaction WAMID is lost")

	var stored models.Message
	require.NoError(t, app.DB.First(&stored, "id = ?", message.ID).Error)
	lastAt, ids := incomingReactionLastEvent(stored.Metadata, normalizeCoexistencePhone(contact.PhoneNumber))
	assert.Equal(t, base+incoming-1, lastAt, "the customer's replay record is not lost")
	assert.Equal(t, []string{fmt.Sprintf("wamid.synthetic-customer-reaction-%02d", incoming-1)}, ids)
	emojis := storedReactionEmojis(t, app, message.ID)
	assert.Contains(t, emojis, "C"+strconv.Itoa(incoming-1), "the customer's latest reaction is kept")
	for i := 0; i < agents; i++ {
		assert.Contains(t, emojis, string(rune(emojiBase+i)), "agent %d's reaction is kept", i)
	}
	assert.Len(t, emojis, agents+1)
}
