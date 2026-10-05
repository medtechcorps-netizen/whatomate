package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/models"
	appwebsocket "github.com/shridarpatil/whatomate/internal/websocket"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Edge cases of the replay guards: event times that cannot be trusted,
// review decisions stamped before a local save, replies processed
// concurrently or naming their request, and reactions in the same second.

const (
	templateReplaySkippedLog = "Template status update not applied: not newer than its stored state"
	templateOlderSkippedLog  = "Template status update older than the stored template was not applied; checking Meta's current status"
	templateUnusableTimeLog  = "Template status update has an unusable event time; applying it unordered"
	permissionIgnoredLog     = "Ignoring replayed or older call permission reply"
	permissionUnusableLog    = "Call permission reply has an unusable timestamp; applying it unordered"
	reactionUnusableLog      = "Reaction has an unusable timestamp; ordering it by its WAMID only"

	templateMetaCheckUnavailableLog = "Cannot check the template's status with Meta (no WhatsApp client or Meta template ID); sync templates"
	templateMetaConfirmsLog         = "Meta confirms the stored template status"
	templateMetaUpdatedLog          = "Updated template status from Meta after a skipped status update"
	templateMetaChangedLog          = "Template changed while its status was read from Meta; keeping the newer state"
	templateMetaFailedLog           = "Failed to check the template's status with Meta; sync templates"
)

func TestUsableWhatsAppEventUnix(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name string
		unix int64
		want int64
	}{
		{"seconds", now.Add(-time.Hour).Unix(), now.Add(-time.Hour).Unix()},
		{"within the clock skew", now.Add(30 * time.Second).Unix(), now.Add(30 * time.Second).Unix()},
		{"the floor", whatsAppEventUnixFloor, whatsAppEventUnixFloor},
		{"milliseconds", now.UnixMilli(), 0},
		{"far future", now.Add(time.Hour).Unix(), 0},
		{"below the floor", 5, 0},
		{"negative", -1, 0},
		{"zero", 0, 0},
	} {
		assert.Equal(t, tc.want, usableWhatsAppEventUnix(tc.unix, now), tc.name)
	}
}

// Templates.

// Meta can decide on a template while the create call is still returning,
// so its decision may be stamped in an earlier second than the PENDING save
// that SubmitTemplate makes afterwards. The decision is applied (a replay of
// it is not), and updated_at never moves backwards.
func TestTemplateReviewDecisionStampedBeforeLocalSaveIsApplied(t *testing.T) {
	for _, decision := range []string{"APPROVED", "REJECTED"} {
		t.Run(decision, func(t *testing.T) {
			app := webhookTestApp(t)
			account := orphanStatusTestAccount(t, app, false)
			saved := time.Now()
			template := createReplayGuardTemplate(t, app, account, saved)
			decidedAt := saved.Add(-time.Second).Unix()
			logs := captureStatusTestLogs(app)

			app.processTemplateStatusUpdate(account.BusinessID, decidedAt, decision, template.Name, template.Language, "NONE")
			stored := storedTemplate(t, app, template.ID)
			assert.Equal(t, decision, stored.Status)
			assert.Equal(t, saved.Unix(), stored.UpdatedAt.Unix(), "updated_at never moves backwards")

			app.processTemplateStatusUpdate(account.BusinessID, decidedAt, decision, template.Name, template.Language, "NONE")
			assert.Len(t, logs.lines("info", retryTestTemplateUpdatedLog), 1, "a replay of the decision is not applied")
			assert.Len(t, logs.lines("info", templateReplaySkippedLog), 1)
		})
	}
}

// The tolerance for a decision older than updated_at applies only to a
// review decision on a PENDING template, and only within
// templateReviewDecisionTolerance. Outside it the decision is skipped and,
// because the stored status differs, logged at Warn and checked against Meta
// (here the App has no WhatsApp client, so the check only logs; see
// TestSkippedTemplateStatusEventIsCheckedWithMeta).
func TestTemplateReviewDecisionToleranceIsBounded(t *testing.T) {
	app := webhookTestApp(t)
	account := orphanStatusTestAccount(t, app, false)
	logs := captureStatusTestLogs(app)
	apply := func(template models.Template, at time.Time, event string) models.Template {
		app.processTemplateStatusUpdate(account.BusinessID, at.Unix(), event, template.Name, template.Language, "NONE")
		return storedTemplate(t, app, template.ID)
	}
	setStatus := func(template models.Template, status string, updatedAt time.Time) {
		require.NoError(t, app.DB.Model(&models.Template{}).Where("id = ?", template.ID).
			UpdateColumns(map[string]any{"status": status, "updated_at": updatedAt}).Error)
	}
	now := time.Now()

	// The PENDING save made half a minute after Meta decided, while the
	// create call was returning.
	edited := createReplayGuardTemplate(t, app, account, now)
	assert.Equal(t, "APPROVED", apply(edited, now.Add(-templateReviewDecisionTolerance/2), "APPROVED").Status)

	// A local save made longer after the decision than the tolerance (for
	// example an edit of the PENDING template): the decision is skipped and
	// reported.
	late := createReplayGuardTemplate(t, app, account, now)
	assert.Equal(t, "PENDING", apply(late, now.Add(-templateReviewDecisionTolerance-time.Minute), "APPROVED").Status)
	require.Len(t, logs.lines("warn", templateOlderSkippedLog), 1)
	assert.Contains(t, logs.lines("warn", templateOlderSkippedLog)[0], "stored_status=PENDING")

	// Only review decisions get the tolerance.
	paused := createReplayGuardTemplate(t, app, account, now)
	assert.Equal(t, "PENDING", apply(paused, now.Add(-time.Minute), "PAUSED").Status)

	// Only a PENDING template gets it: a decision on the version before a
	// local edit (APPROVED became DRAFT) does not overwrite the edit.
	draft := createReplayGuardTemplate(t, app, account, now)
	setStatus(draft, "DRAFT", now)
	assert.Equal(t, "DRAFT", apply(draft, now.Add(-time.Minute), "APPROVED").Status)
	assert.Len(t, logs.lines("warn", templateOlderSkippedLog), 3)
	assert.Len(t, logs.lines("info", retryTestTemplateUpdatedLog), 1)
	assert.Len(t, logs.lines("warn", templateMetaCheckUnavailableLog), 3, "each skip with another status is checked with Meta")
}

// An entry time that is not plausible Unix seconds (milliseconds, the far
// future, a tiny value) is not used to order the update: it applies as it
// did before the guard, is reported, and does not move updated_at, so later
// genuine events still apply.
func TestTemplateStatusUpdateWithUnusableEventTimeIsUnordered(t *testing.T) {
	app := webhookTestApp(t)
	account := orphanStatusTestAccount(t, app, false)
	logs := captureStatusTestLogs(app)
	template := createReplayGuardTemplate(t, app, account, time.Now().Add(-time.Hour))

	for i, tc := range []struct {
		event string
		unix  int64
	}{
		{"APPROVED", time.Now().UnixMilli()},
		{"PAUSED", time.Now().Add(time.Hour).Unix()},
		{"APPROVED", 5},
	} {
		before := time.Now().Add(-time.Second)
		app.processTemplateStatusUpdate(account.BusinessID, tc.unix, tc.event, template.Name, template.Language, "NONE")
		stored := storedTemplate(t, app, template.ID)
		assert.Equal(t, tc.event, stored.Status, "case %d", i)
		assert.WithinRange(t, stored.UpdatedAt, before, time.Now().Add(time.Second), "case %d: updated_at is the processing time", i)
		assert.Len(t, logs.lines("warn", templateUnusableTimeLog), i+1)
	}

	genuine := time.Now().Add(30 * time.Second)
	app.processTemplateStatusUpdate(account.BusinessID, genuine.Unix(), "DISABLED", template.Name, template.Language, "NONE")
	stored := storedTemplate(t, app, template.ID)
	assert.Equal(t, "DISABLED", stored.Status, "a later genuine event still applies")
	assert.Equal(t, genuine.Unix(), stored.UpdatedAt.Unix())
}

// The same through a signed POST whose entry time is in milliseconds.
func TestWebhookTemplateStatusWithMillisecondEntryTimeIsApplied(t *testing.T) {
	app := webhookTestApp(t)
	account := orphanStatusTestAccount(t, app, false)
	template := createReplayGuardTemplate(t, app, account, time.Now().Add(-time.Hour))
	body, err := json.Marshal(map[string]any{
		"object": "whatsapp_business_account",
		"entry": []any{map[string]any{
			"id": account.BusinessID, "time": time.Now().UnixMilli(),
			"changes": []any{retryPOSTTemplateStatusChange(template, "APPROVED")},
		}},
	})
	require.NoError(t, err)

	assert.Equal(t, http.StatusOK, signedWebhookStatusCode(t, app, body))
	require.Eventually(t, func() bool { return storedTemplate(t, app, template.ID).Status == "APPROVED" },
		10*time.Second, 20*time.Millisecond)
	assert.Less(t, storedTemplate(t, app, template.ID).UpdatedAt.Unix(), time.Now().Add(time.Minute).Unix())
}

// Call permission replies.

func createReviewCallPermission(t *testing.T, app *App, account models.WhatsAppAccount, contact *models.Contact, label string, requestedAt time.Time) models.CallPermission {
	t.Helper()
	permission := models.CallPermission{
		BaseModel:       models.BaseModel{ID: uuid.New()},
		OrganizationID:  account.OrganizationID,
		ContactID:       contact.ID,
		WhatsAppAccount: account.Name,
		Status:          models.CallPermissionPending,
		MessageID:       "wamid.synthetic-permission-" + label + "-" + uuid.NewString(),
	}
	require.NoError(t, app.DB.Create(&permission).Error)
	require.NoError(t, app.DB.Model(&models.CallPermission{}).Where("id = ?", permission.ID).
		UpdateColumns(map[string]any{"requested_at": requestedAt, "created_at": requestedAt}).Error)
	return permission
}

// callPermissionUpdatesWaiting counts the backends of this database that are
// waiting for a lock while updating call_permissions.
func callPermissionUpdatesWaiting(t *testing.T, app *App) int64 {
	t.Helper()
	var waiting int64
	require.NoError(t, app.DB.Raw(`SELECT count(*) FROM pg_stat_activity
		WHERE datname = current_database() AND wait_event_type = 'Lock'
		AND query ILIKE 'UPDATE%call_permissions%'`).Scan(&waiting).Error)
	return waiting
}

// Each reply runs in its own goroutine, so a delivery can read the row before
// another reply's write commits. The order is decided inside the UPDATE:
// a newer reply is applied even when an older one committed after it read
// the row, and an older reply or a copy of the committed one is ignored.
// The committed reply is written by a transaction that holds the row lock
// until the racing delivery has read the still unanswered row and is waiting
// in its UPDATE.
func TestCallPermissionReplyOrderingHoldsUnderConcurrentDelivery(t *testing.T) {
	for _, tc := range []struct {
		name            string
		committed       models.CallPermissionStatus
		committedAfter  time.Duration
		racing          string
		racingAfter     time.Duration
		want            models.CallPermissionStatus
		racingIsApplied bool
	}{
		{"an older reply commits first and the newer one still applies",
			models.CallPermissionAccepted, time.Minute, "reject", 2 * time.Minute, models.CallPermissionDeclined, true},
		{"a newer reply commits first and the older one is ignored",
			models.CallPermissionDeclined, 2 * time.Minute, "accept", time.Minute, models.CallPermissionDeclined, false},
		{"a copy of the committed reply is ignored",
			models.CallPermissionAccepted, time.Minute, "accept", time.Minute, models.CallPermissionAccepted, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app := webhookTestApp(t)
			account := orphanStatusTestAccount(t, app, false)
			hub, client := replayGuardHub(t, account.OrganizationID)
			app.WSHub = hub
			contact := testutil.CreateTestContact(t, app.DB, account.OrganizationID)
			base := time.Now().Add(-time.Hour).Truncate(time.Second)
			permission := createReviewCallPermission(t, app, account, contact, "concurrent", base)
			logs := captureStatusTestLogs(app)

			lock := app.DB.Begin()
			require.NoError(t, lock.Error)
			require.NoError(t, lock.Exec("UPDATE call_permissions SET status = ?, responded_at = ? WHERE id = ?",
				string(tc.committed), base.Add(tc.committedAfter), permission.ID).Error)
			done := make(chan struct{})
			go func() {
				defer close(done)
				app.processCallPermissionReply(account.PhoneID, contact.PhoneNumber, &CallPermissionReplyData{
					Response: tc.racing, ResponseSource: "user_action", RepliedAt: base.Add(tc.racingAfter).Unix(),
				})
			}()
			require.Eventually(t, func() bool { return callPermissionUpdatesWaiting(t, app) == 1 },
				10*time.Second, 10*time.Millisecond, "the racing reply waits in its UPDATE")
			require.NoError(t, lock.Commit().Error)
			<-done

			stored := storedCallPermission(t, app, permission.ID)
			assert.Equal(t, tc.want, stored.Status)
			require.NotNil(t, stored.RespondedAt)
			if tc.racingIsApplied {
				assert.Equal(t, base.Add(tc.racingAfter).Unix(), stored.RespondedAt.Unix())
				assert.Equal(t, []string{appwebsocket.TypeCallPermissionUpdate}, wsTypes(t, client, 300*time.Millisecond))
				assert.Empty(t, logs.lines("info", permissionIgnoredLog))
			} else {
				assert.Equal(t, base.Add(tc.committedAfter).Unix(), stored.RespondedAt.Unix())
				assert.Empty(t, wsTypes(t, client, 300*time.Millisecond))
				assert.Len(t, logs.lines("info", permissionIgnoredLog), 1)
			}
		})
	}
}

// A reply names the request it answers (context.id), so it updates exactly
// that request: a replay of the reply to an earlier request, delivered after
// a newer request was sent, and a reply tapped on the earlier request's
// message later on, both answer the earlier request only. A reply without a
// context, or naming a request ReReply did not record, answers the newest
// request as before.
func TestCallPermissionReplyAnswersTheRequestItNames(t *testing.T) {
	app := webhookTestApp(t)
	account := orphanStatusTestAccount(t, app, false)
	hub, client := replayGuardHub(t, account.OrganizationID)
	app.WSHub = hub
	contact := testutil.CreateTestContact(t, app.DB, account.OrganizationID)
	base := time.Now().Add(-time.Hour).Truncate(time.Second)
	first := createReviewCallPermission(t, app, account, contact, "first", base)
	second := createReviewCallPermission(t, app, account, contact, "second", base.Add(80*time.Second))
	reply := func(response string, at time.Time, requestWAMID string) {
		app.processCallPermissionReply(account.PhoneID, contact.PhoneNumber, &CallPermissionReplyData{
			Response: response, ResponseSource: "user_action", RepliedAt: at.Unix(), RequestMessageID: requestWAMID,
		})
	}
	statuses := func() (models.CallPermissionStatus, models.CallPermissionStatus) {
		return storedCallPermission(t, app, first.ID).Status, storedCallPermission(t, app, second.ID).Status
	}
	const quiet = 300 * time.Millisecond

	reply("accept", base.Add(20*time.Second), first.MessageID)
	assert.Len(t, wsTypes(t, client, quiet), 1)
	firstStatus, secondStatus := statuses()
	assert.Equal(t, models.CallPermissionAccepted, firstStatus)
	assert.Equal(t, models.CallPermissionPending, secondStatus, "a reply to the first request never answers the second")

	reply("accept", base.Add(20*time.Second), first.MessageID)
	assert.Empty(t, wsTypes(t, client, quiet), "a replay is ignored")
	firstStatus, secondStatus = statuses()
	assert.Equal(t, models.CallPermissionAccepted, firstStatus)
	assert.Equal(t, models.CallPermissionPending, secondStatus)

	reply("reject", base.Add(100*time.Second), first.MessageID)
	assert.Len(t, wsTypes(t, client, quiet), 1)
	firstStatus, secondStatus = statuses()
	assert.Equal(t, models.CallPermissionDeclined, firstStatus)
	assert.Equal(t, models.CallPermissionPending, secondStatus)

	reply("accept", base.Add(110*time.Second), second.MessageID)
	firstStatus, secondStatus = statuses()
	assert.Equal(t, models.CallPermissionDeclined, firstStatus)
	assert.Equal(t, models.CallPermissionAccepted, secondStatus)

	reply("reject", base.Add(120*time.Second), "")
	firstStatus, secondStatus = statuses()
	assert.Equal(t, models.CallPermissionDeclined, firstStatus)
	assert.Equal(t, models.CallPermissionDeclined, secondStatus, "without a context the newest request is answered")

	reply("accept", base.Add(130*time.Second), "wamid.synthetic-permission-unrecorded-"+uuid.NewString())
	firstStatus, secondStatus = statuses()
	assert.Equal(t, models.CallPermissionDeclined, firstStatus)
	assert.Equal(t, models.CallPermissionAccepted, secondStatus, "an unrecorded request falls back to the newest one")

	// The other answer in the same second is applied; its replay is not.
	reply("reject", base.Add(130*time.Second), second.MessageID)
	reply("reject", base.Add(130*time.Second), second.MessageID)
	assert.Equal(t, models.CallPermissionDeclined, storedCallPermission(t, app, second.ID).Status)
	assert.Len(t, wsTypes(t, client, quiet), 4)
}

// The webhook passes the reply's context.id through.
func TestWebhookCallPermissionReplyAnswersTheRequestInItsContext(t *testing.T) {
	app := webhookTestApp(t)
	account := orphanStatusTestAccount(t, app, false)
	contact := testutil.CreateTestContact(t, app.DB, account.OrganizationID)
	base := time.Now().Add(-10 * time.Minute).Truncate(time.Second)
	first := createReviewCallPermission(t, app, account, contact, "context-first", base)
	second := createReviewCallPermission(t, app, account, contact, "context-second", base.Add(time.Minute))
	repliedAt := base.Add(90 * time.Second)
	body := retryPOSTBody(t, account, []map[string]any{inboundMessagesChange(account, map[string]any{
		"from": contact.PhoneNumber, "id": "wamid.synthetic-permission-context-reply-" + uuid.NewString(),
		"timestamp": strconv.FormatInt(repliedAt.Unix(), 10), "type": "interactive",
		"context": map[string]any{"from": account.PhoneID, "id": first.MessageID},
		"interactive": map[string]any{
			"type": "call_permission_reply",
			"call_permission_reply": map[string]any{
				"response": "accept", "is_permanent": false,
				"expiration_timestamp": repliedAt.Add(7 * 24 * time.Hour).Unix(),
				"response_source":      "user_action",
			},
		},
	})})

	assert.Equal(t, http.StatusOK, signedWebhookStatusCode(t, app, body))
	require.Eventually(t, func() bool {
		return storedCallPermission(t, app, first.ID).Status == models.CallPermissionAccepted
	}, 10*time.Second, 20*time.Millisecond)
	assert.Equal(t, models.CallPermissionPending, storedCallPermission(t, app, second.ID).Status)
}

// A reply timestamp that is not plausible Unix seconds is not stored as
// responded_at: the reply applies unordered with the processing time, and a
// later genuine reply still applies.
func TestCallPermissionReplyWithUnusableTimestampIsUnordered(t *testing.T) {
	app := webhookTestApp(t)
	account := orphanStatusTestAccount(t, app, false)
	contact := testutil.CreateTestContact(t, app.DB, account.OrganizationID)
	permission := createReviewCallPermission(t, app, account, contact, "unusable", time.Now().Add(-time.Hour))
	logs := captureStatusTestLogs(app)

	before := time.Now().Add(-time.Second)
	app.processCallPermissionReply(account.PhoneID, contact.PhoneNumber, &CallPermissionReplyData{
		Response: "accept", ResponseSource: "user_action", RepliedAt: time.Now().UnixMilli(),
		RequestMessageID: permission.MessageID,
	})
	stored := storedCallPermission(t, app, permission.ID)
	assert.Equal(t, models.CallPermissionAccepted, stored.Status)
	require.NotNil(t, stored.RespondedAt)
	assert.WithinRange(t, *stored.RespondedAt, before, time.Now().Add(time.Second))
	assert.Len(t, logs.lines("warn", permissionUnusableLog), 1)

	app.processCallPermissionReply(account.PhoneID, contact.PhoneNumber, &CallPermissionReplyData{
		Response: "reject", ResponseSource: "user_action", RepliedAt: time.Now().Add(30 * time.Second).Unix(),
		RequestMessageID: permission.MessageID,
	})
	assert.Equal(t, models.CallPermissionDeclined, storedCallPermission(t, app, permission.ID).Status)
}

// Reactions.

func reactionReviewTarget(t *testing.T, coexistence bool) (*App, *models.WhatsAppAccount, *models.Contact, models.Message, string) {
	t.Helper()
	app, account, contact := outgoingReceiptFixture(t, coexistence)
	target := "wamid.synthetic-reaction-review-target-" + uuid.NewString()
	message, err := app.SendOutgoingMessage(t.Context(), OutgoingMessageRequest{
		Account: account, Contact: contact, Type: models.MessageTypeText,
		Content: "synthetic reaction review target",
		deliveryOverride: func(_ context.Context, _ *models.Contact) (string, error) {
			return target, nil
		},
	}, replayGuardSendOptions())
	require.NoError(t, err)
	return app, account, contact, *message, target
}

// Two different reactions from one phone in the same second (a quick change
// of emoji, or a removal) are both applied in the order they arrive. A replay
// of the first one, delivered after the second was applied, is recognised by
// its WAMID and neither restores it nor is announced again.
func TestIncomingReactionSameSecondReplayIsIgnored(t *testing.T) {
	for _, coexistence := range []bool{false, true} {
		t.Run(coexistenceModeName(coexistence), func(t *testing.T) {
			app, account, contact, message, target := reactionReviewTarget(t, coexistence)
			hub, client := replayGuardHub(t, account.OrganizationID)
			app.WSHub = hub
			at := time.Now().Add(-time.Hour).Unix()
			react := func(emoji, id string, unix int64) {
				app.handleIncomingReactionEvent(account, contact.PhoneNumber, target, emoji, "Patient", id,
					strconv.FormatInt(unix, 10))
			}
			const quiet = 300 * time.Millisecond

			react("A", "wamid.synthetic-same-second-1", at)
			react("B", "wamid.synthetic-same-second-2", at)
			assert.Len(t, wsTypes(t, client, quiet), 2, "both reactions of the second are applied")
			assert.Equal(t, []string{"B"}, storedReactionEmojis(t, app, message.ID))

			react("A", "wamid.synthetic-same-second-1", at)
			react("B", "wamid.synthetic-same-second-2", at)
			assert.Empty(t, wsTypes(t, client, quiet), "replays are not announced")
			assert.Equal(t, []string{"B"}, storedReactionEmojis(t, app, message.ID),
				"a replay of the earlier reaction of the same second does not restore it")

			react("", "wamid.synthetic-same-second-3", at+1)
			react("C", "wamid.synthetic-same-second-4", at+1)
			react("", "wamid.synthetic-same-second-5", at+1)
			assert.Len(t, wsTypes(t, client, quiet), 3)
			assert.Empty(t, storedReactionEmojis(t, app, message.ID))
			for _, replay := range []struct {
				emoji, id string
				unix      int64
			}{
				{"C", "wamid.synthetic-same-second-4", at + 1},
				{"B", "wamid.synthetic-same-second-2", at},
				{"A", "wamid.synthetic-same-second-1", at},
			} {
				react(replay.emoji, replay.id, replay.unix)
			}
			assert.Empty(t, wsTypes(t, client, quiet))
			assert.Empty(t, storedReactionEmojis(t, app, message.ID),
				"a replay does not bring back a reaction removed in the same second")

			var stored models.Message
			require.NoError(t, app.DB.First(&stored, "id = ?", message.ID).Error)
			lastAt, ids := incomingReactionLastEvent(stored.Metadata, normalizeCoexistencePhone(contact.PhoneNumber))
			assert.Equal(t, at+1, lastAt)
			assert.Equal(t, []string{"wamid.synthetic-same-second-3", "wamid.synthetic-same-second-4", "wamid.synthetic-same-second-5"}, ids,
				"a newer second replaces the recorded WAMIDs")
		})
	}
}

// A reaction timestamp that is not plausible Unix seconds is ordered by its
// WAMID only and does not record a far-future second that would hide every
// later reaction.
func TestIncomingReactionWithUnusableTimestampDoesNotHideLaterReactions(t *testing.T) {
	app, account, contact, message, target := reactionReviewTarget(t, false)
	logs := captureStatusTestLogs(app)
	react := func(emoji, id, timestamp string) {
		app.handleIncomingReactionEvent(account, contact.PhoneNumber, target, emoji, "Patient", id, timestamp)
	}

	untimed := strconv.FormatInt(time.Now().UnixMilli(), 10)
	react("A", "wamid.synthetic-unusable-reaction-1", untimed)
	assert.Equal(t, []string{"A"}, storedReactionEmojis(t, app, message.ID))
	assert.Len(t, logs.lines("warn", reactionUnusableLog), 1)
	react("", "wamid.synthetic-unusable-reaction-2", untimed)
	react("A", "wamid.synthetic-unusable-reaction-1", untimed)
	assert.Empty(t, storedReactionEmojis(t, app, message.ID), "the replay is recognised by its WAMID")

	react("B", "wamid.synthetic-unusable-reaction-3", strconv.FormatInt(time.Now().Unix(), 10))
	assert.Equal(t, []string{"B"}, storedReactionEmojis(t, app, message.ID), "a later genuine reaction applies")
}

func TestNextIncomingReactionEventBoundsTheSecondsWAMIDs(t *testing.T) {
	const phone = "15550000003"
	metadata := models.JSONB{}
	for i := range incomingReactionEventIDsLimit + 4 {
		metadata[incomingReactionEventsMetadataKey] = map[string]any{
			phone: nextIncomingReactionEvent(metadata, phone, "wamid.synthetic-bound-"+strconv.Itoa(i), 1_700_000_000),
		}
	}
	lastAt, ids := incomingReactionLastEvent(metadata, phone)
	assert.Equal(t, int64(1_700_000_000), lastAt)
	require.Len(t, ids, incomingReactionEventIDsLimit)
	assert.Equal(t, "wamid.synthetic-bound-4", ids[0])
	assert.Equal(t, "wamid.synthetic-bound-"+strconv.Itoa(incomingReactionEventIDsLimit+3), ids[len(ids)-1])

	// An event without a timestamp joins the recorded second; a newer second
	// replaces it.
	record := nextIncomingReactionEvent(metadata, phone, "wamid.synthetic-bound-untimed", 0)
	assert.Equal(t, int64(1_700_000_000), record["timestamp"])
	record = nextIncomingReactionEvent(metadata, phone, "wamid.synthetic-bound-newer", 1_700_000_001)
	assert.Equal(t, []string{"wamid.synthetic-bound-newer"}, record["ids"])
}
