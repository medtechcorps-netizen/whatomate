package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/config"
	"github.com/shridarpatil/whatomate/internal/database"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/pkg/whatsapp"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A template status event older than the stored template is not applied
// directly, but a genuine one can be older than a later local write of the
// template (updated_at is the server's clock for local writes and Meta's for
// events); when its status differs from the stored one the template's current
// status is read from Meta. Also the bounds on status timestamps and on the
// reaction WAMID lookup of statuses that have no Message.

// Templates.

// metaTemplateState is what the fake Graph API reports for a template ID.
type metaTemplateState struct{ Name, Language, Status string }

// templateGraphServer is a fake Graph API for GET /{version}/{template-id}.
type templateGraphServer struct {
	*httptest.Server
	token       string
	mu          sync.Mutex
	templates   map[string]metaTemplateState
	requests    []string
	failing     bool
	beforeReply func(id string)
}

func newTemplateGraphServer(t *testing.T, token string) *templateGraphServer {
	t.Helper()
	server := &templateGraphServer{token: token, templates: map[string]metaTemplateState{}}
	server.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		id := parts[len(parts)-1]
		server.mu.Lock()
		server.requests = append(server.requests, r.Method+" "+r.URL.Path+"?"+r.URL.RawQuery)
		state, known := server.templates[id]
		failing, hook := server.failing, server.beforeReply
		server.mu.Unlock()
		if hook != nil {
			hook(id)
		}
		w.Header().Set("Content-Type", "application/json")
		if failing || !known || r.Method != http.MethodGet || r.Header.Get("Authorization") != "Bearer "+server.token {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"synthetic Graph failure","type":"OAuthException","code":100}}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": id, "name": state.Name, "language": state.Language, "status": state.Status,
		})
	}))
	t.Cleanup(server.Close)
	return server
}

func (s *templateGraphServer) set(id string, state metaTemplateState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.templates[id] = state
}

func (s *templateGraphServer) setFailing(failing bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failing = failing
}

func (s *templateGraphServer) setBeforeReply(hook func(id string)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.beforeReply = hook
}

func (s *templateGraphServer) madeRequests() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.requests...)
}

// templateGraphApp is a classic account whose WhatsApp client talks to a fake
// Graph API.
func templateGraphApp(t *testing.T) (*App, models.WhatsAppAccount, *templateGraphServer) {
	t.Helper()
	app := webhookTestApp(t)
	account := orphanStatusTestAccount(t, app, false)
	require.NoError(t, app.DB.Model(&models.WhatsAppAccount{}).Where("id = ?", account.ID).
		UpdateColumn("api_version", "v21.0").Error)
	account.APIVersion = "v21.0"
	graph := newTemplateGraphServer(t, "token")
	app.WhatsApp = whatsapp.NewWithBaseURL(testutil.NopLogger(), graph.URL)
	return app, account, graph
}

// metaKnownTemplate is a template of account that Meta knows under a template
// ID, stored with status by a local write (a Sync, an edit or a submission)
// at writtenAt.
func metaKnownTemplate(t *testing.T, app *App, account models.WhatsAppAccount, status string, writtenAt time.Time) models.Template {
	t.Helper()
	template := createReplayGuardTemplate(t, app, account, writtenAt)
	template.MetaTemplateID = testutil.NewTestGraphObjectID()
	template.Status = status
	require.NoError(t, app.DB.Model(&models.Template{}).Where("id = ?", template.ID).UpdateColumns(map[string]any{
		"status": status, "meta_template_id": template.MetaTemplateID, "updated_at": writtenAt,
	}).Error)
	return template
}

// A genuine template event stamped before a later local write of the template
// (a Sync whose read from Meta preceded the event, an edit made while the
// event was in flight, or a server clock ahead of Meta's) is older than
// updated_at and so is not applied directly. Because its status differs from
// the stored one, the template's current status is read from Meta and stored,
// as a template Sync would, without moving updated_at. These are the cases in
// which the status differed from main's before this check.
func TestSkippedTemplateStatusEventIsCheckedWithMeta(t *testing.T) {
	for _, tc := range []struct {
		name   string
		stored string
		event  string
		before time.Duration
		meta   string
	}{
		{"PAUSED stamped before a Sync wrote APPROVED", "APPROVED", "PAUSED", 3 * time.Second, "PAUSED"},
		{"DISABLED stamped before a Sync wrote APPROVED", "APPROVED", "DISABLED", 2 * time.Second, "DISABLED"},
		{"FLAGGED stamped before a Sync wrote APPROVED", "APPROVED", "FLAGGED", 2 * time.Second, "FLAGGED"},
		{"REINSTATED stamped before an edit of a PAUSED template", "PAUSED", "REINSTATED", 3 * time.Second, "APPROVED"},
		{"PAUSED stamped before a save of a PENDING template", "PENDING", "PAUSED", 2 * time.Second, "PAUSED"},
		{"REJECTED stamped six minutes before an edit of a PENDING template", "PENDING", "REJECTED", 6 * time.Minute, "REJECTED"},
		{"APPROVED stamped two minutes before an edit of a PENDING template", "PENDING", "APPROVED", 2 * time.Minute, "APPROVED"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app, account, graph := templateGraphApp(t)
			logs := captureStatusTestLogs(app)
			writtenAt := time.Now()
			template := metaKnownTemplate(t, app, account, tc.stored, writtenAt)
			graph.set(template.MetaTemplateID, metaTemplateState{template.Name, template.Language, tc.meta})

			app.processTemplateStatusUpdate(account.BusinessID, writtenAt.Add(-tc.before).Unix(), tc.event,
				template.Name, template.Language, "NONE")

			stored := storedTemplate(t, app, template.ID)
			assert.Equal(t, tc.meta, stored.Status)
			assert.Equal(t, writtenAt.UnixMicro(), stored.UpdatedAt.UnixMicro(), "updated_at is not moved")
			assert.Len(t, logs.lines("warn", templateOlderSkippedLog), 1)
			assert.Len(t, logs.lines("info", templateMetaUpdatedLog), 1)
			assert.Equal(t, []string{"GET /v21.0/" + template.MetaTemplateID + "?fields=id,name,language,status"},
				graph.madeRequests(), "one read of that template")

			// The next event is still ordered against the local write.
			app.processTemplateStatusUpdate(account.BusinessID, writtenAt.Add(time.Second).Unix(), "DISABLED",
				template.Name, template.Language, "NONE")
			assert.Equal(t, "DISABLED", storedTemplate(t, app, template.ID).Status)
		})
	}
}

// A replay whose status differs from the stored one is checked too; Meta then
// confirms the stored status and nothing is written. A replay of the stored
// status is not checked at all.
func TestTemplateStatusReplayCheckedWithMetaKeepsTheStoredStatus(t *testing.T) {
	app, account, graph := templateGraphApp(t)
	logs := captureStatusTestLogs(app)
	writtenAt := time.Now().Add(-time.Minute)
	template := metaKnownTemplate(t, app, account, "APPROVED", writtenAt)
	graph.set(template.MetaTemplateID, metaTemplateState{template.Name, template.Language, "APPROVED"})

	app.processTemplateStatusUpdate(account.BusinessID, writtenAt.Add(-time.Hour).Unix(), "PAUSED",
		template.Name, template.Language, "NONE")
	stored := storedTemplate(t, app, template.ID)
	assert.Equal(t, "APPROVED", stored.Status)
	assert.Equal(t, writtenAt.UnixMicro(), stored.UpdatedAt.UnixMicro())
	assert.Len(t, logs.lines("info", templateMetaConfirmsLog), 1)
	assert.Len(t, graph.madeRequests(), 1)

	app.processTemplateStatusUpdate(account.BusinessID, writtenAt.Add(-time.Hour).Unix(), "APPROVED",
		template.Name, template.Language, "NONE")
	assert.Len(t, logs.lines("info", templateReplaySkippedLog), 1)
	assert.Len(t, graph.madeRequests(), 1, "a replay of the stored status is not checked with Meta")
}

// The same through a signed POST whose entry time is two seconds before the
// local write.
func TestWebhookTemplateStatusStampedBeforeALocalWriteIsCheckedWithMeta(t *testing.T) {
	app, account, graph := templateGraphApp(t)
	writtenAt := time.Now()
	template := metaKnownTemplate(t, app, account, "APPROVED", writtenAt)
	graph.set(template.MetaTemplateID, metaTemplateState{template.Name, template.Language, "PAUSED"})
	body := templateEntryBody(t, account, writtenAt.Add(-2*time.Second), retryPOSTTemplateStatusChange(template, "PAUSED"))

	assert.Equal(t, http.StatusOK, signedWebhookStatusCode(t, app, body))
	require.Eventually(t, func() bool { return storedTemplate(t, app, template.ID).Status == "PAUSED" },
		10*time.Second, 20*time.Millisecond)
	assert.Equal(t, writtenAt.UnixMicro(), storedTemplate(t, app, template.ID).UpdatedAt.UnixMicro())
}

// The status read from Meta is written only while the template still has the
// state the skipped event found: an event, edit or Sync that committed while
// Meta was being read is kept.
func TestTemplateStatusFromMetaNeverOverwritesANewerWrite(t *testing.T) {
	app, account, graph := templateGraphApp(t)
	logs := captureStatusTestLogs(app)
	writtenAt := time.Now().Add(-time.Minute)
	template := metaKnownTemplate(t, app, account, "APPROVED", writtenAt)
	graph.set(template.MetaTemplateID, metaTemplateState{template.Name, template.Language, "PAUSED"})
	newer := time.Now().Truncate(time.Second)
	graph.setBeforeReply(func(string) {
		app.processTemplateStatusUpdate(account.BusinessID, newer.Unix(), "DISABLED", template.Name, template.Language, "NONE")
	})

	app.processTemplateStatusUpdate(account.BusinessID, writtenAt.Add(-time.Hour).Unix(), "PAUSED",
		template.Name, template.Language, "NONE")
	stored := storedTemplate(t, app, template.ID)
	assert.Equal(t, "DISABLED", stored.Status, "the newer event committed while Meta was read is kept")
	assert.Equal(t, newer.Unix(), stored.UpdatedAt.Unix())
	assert.Len(t, logs.lines("info", templateMetaChangedLog), 1)
	assert.Empty(t, logs.lines("info", templateMetaUpdatedLog))
}

// A failed read from Meta, a template without a Meta template ID and an ID
// that Meta reports for another template all keep the stored status and are
// logged.
func TestTemplateStatusMetaCheckFailuresKeepTheStoredStatus(t *testing.T) {
	app, account, graph := templateGraphApp(t)
	logs := captureStatusTestLogs(app)
	writtenAt := time.Now().Add(-time.Minute)
	skip := func(template models.Template) models.Template {
		app.processTemplateStatusUpdate(account.BusinessID, writtenAt.Add(-time.Hour).Unix(), "PAUSED",
			template.Name, template.Language, "NONE")
		return storedTemplate(t, app, template.ID)
	}

	failing := metaKnownTemplate(t, app, account, "APPROVED", writtenAt)
	graph.set(failing.MetaTemplateID, metaTemplateState{failing.Name, failing.Language, "PAUSED"})
	graph.setFailing(true)
	assert.Equal(t, "APPROVED", skip(failing).Status)
	assert.Len(t, logs.lines("error", templateMetaFailedLog), 1)
	graph.setFailing(false)

	unknown := createReplayGuardTemplate(t, app, account, writtenAt)
	assert.Equal(t, "PENDING", skip(unknown).Status)
	assert.Len(t, logs.lines("warn", templateMetaCheckUnavailableLog), 1)

	renamed := metaKnownTemplate(t, app, account, "APPROVED", writtenAt)
	graph.set(renamed.MetaTemplateID, metaTemplateState{renamed.Name + "_other", renamed.Language, "PAUSED"})
	assert.Equal(t, "APPROVED", skip(renamed).Status)
	assert.Len(t, logs.lines("warn", "Meta returned another template, or no status, for the stored Meta template ID; sync templates"), 1)
	assert.Len(t, graph.madeRequests(), 2, "a template without a Meta template ID is not read")
}

// A decision on the previous version of a template, replayed after the
// template was edited and resubmitted 70 seconds later, is older than the
// resubmission by more than templateReviewDecisionTolerance: it is skipped,
// and Meta, still reviewing the new version, confirms PENDING.
func TestTemplateDecisionReplayAfterResubmissionIsNotApplied(t *testing.T) {
	app, account, graph := templateGraphApp(t)
	logs := captureStatusTestLogs(app)
	submitted := time.Now().Add(-10 * time.Minute).Truncate(time.Second)
	template := metaKnownTemplate(t, app, account, "PENDING", submitted)
	graph.set(template.MetaTemplateID, metaTemplateState{template.Name, template.Language, "REJECTED"})
	rejectedAt := submitted.Add(20 * time.Second)
	app.processTemplateStatusUpdate(account.BusinessID, rejectedAt.Unix(), "REJECTED", template.Name, template.Language, "NONE")
	require.Equal(t, "REJECTED", storedTemplate(t, app, template.ID).Status)

	resubmitted := rejectedAt.Add(70 * time.Second)
	require.Greater(t, resubmitted.Sub(rejectedAt), templateReviewDecisionTolerance)
	require.NoError(t, app.DB.Model(&models.Template{}).Where("id = ?", template.ID).
		UpdateColumns(map[string]any{"status": "PENDING", "updated_at": resubmitted}).Error)
	graph.set(template.MetaTemplateID, metaTemplateState{template.Name, template.Language, "PENDING"})

	app.processTemplateStatusUpdate(account.BusinessID, rejectedAt.Unix(), "REJECTED", template.Name, template.Language, "NONE")
	assert.Equal(t, "PENDING", storedTemplate(t, app, template.ID).Status)
	assert.Len(t, logs.lines("warn", templateOlderSkippedLog), 1)
	assert.Len(t, logs.lines("info", templateMetaConfirmsLog), 1)
}

// Under the restricted runtime role and RLS (as in production) the check runs
// after the tenant transaction and writes through its own tenant scope; a
// template of the same name in another tenant is not touched.
func TestSkippedTemplateStatusIsCheckedWithMetaUnderRLS(t *testing.T) {
	adminDB := testutil.SetupTestDB(t)
	testutil.TruncateTables(adminDB)
	runtimeRole := "rereply_tplheal_" + uuid.NewString()[:8]
	runtimePassword := "synthetic" + uuid.NewString()[:8]
	require.NoError(t, adminDB.Exec(fmt.Sprintf(
		"CREATE ROLE %s LOGIN PASSWORD '%s' NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT NOBYPASSRLS",
		runtimeRole, runtimePassword)).Error)
	t.Cleanup(func() {
		_ = database.RemoveTenantRLS(adminDB)
		_ = adminDB.Exec("DROP OWNED BY " + runtimeRole).Error
		_ = adminDB.Exec("DROP ROLE IF EXISTS " + runtimeRole).Error
		testutil.TruncateTables(adminDB)
	})

	reseller := testutil.CreateTestReseller(t, adminDB)
	orgA := testutil.CreateTestOrganizationForReseller(t, adminDB, reseller.ID)
	orgB := testutil.CreateTestOrganizationForReseller(t, adminDB, reseller.ID)
	accountA := testutil.CreateTestWhatsAppAccount(t, adminDB, orgA.ID)
	accountB := testutil.CreateTestWhatsAppAccount(t, adminDB, orgB.ID)
	name := "synthetic_rls_heal_" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	writtenAt := time.Now()
	create := func(account *models.WhatsAppAccount) models.Template {
		template := models.Template{
			BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: account.OrganizationID,
			WhatsAppAccount: account.Name, MetaTemplateID: testutil.NewTestGraphObjectID(),
			Name: name, Language: "en", BodyContent: "Synthetic {{1}}", Status: "APPROVED",
		}
		require.NoError(t, adminDB.Create(&template).Error)
		require.NoError(t, adminDB.Model(&models.Template{}).Where("id = ?", template.ID).
			UpdateColumn("updated_at", writtenAt).Error)
		return template
	}
	templateA := create(accountA)
	templateB := create(accountB)

	require.NoError(t, database.ApplyTenantRLS(adminDB, runtimeRole))
	runtimeDB := openRuntimeRoleTestDB(t, runtimeRole, runtimePassword)
	require.NoError(t, database.VerifyTenantRLS(runtimeDB, runtimeRole))
	graph := newTemplateGraphServer(t, accountA.AccessToken)
	graph.set(templateA.MetaTemplateID, metaTemplateState{name, "en", "PAUSED"})
	app := &App{
		Config: &config.Config{
			App:      config.AppConfig{EncryptionKey: "test-encryption-key-32-bytes-long"},
			WhatsApp: config.WhatsAppConfig{AppSecret: webhookTestAppSecret},
		},
		DB:       runtimeDB,
		Log:      testutil.NopLogger(),
		WhatsApp: whatsapp.NewWithBaseURL(testutil.NopLogger(), graph.URL),
	}
	app.Config.Database.RLSEnabled = true
	app.Config.Database.RuntimeRole = runtimeRole
	logs := captureStatusTestLogs(app)

	app.processTemplateStatusUpdate(accountA.BusinessID, writtenAt.Add(-3*time.Second).Unix(), "PAUSED", name, "en", "NONE")
	var storedA, storedB models.Template
	require.NoError(t, adminDB.First(&storedA, "id = ?", templateA.ID).Error)
	require.NoError(t, adminDB.First(&storedB, "id = ?", templateB.ID).Error)
	assert.Equal(t, "PAUSED", storedA.Status)
	assert.Equal(t, writtenAt.UnixMicro(), storedA.UpdatedAt.UnixMicro())
	assert.Equal(t, "APPROVED", storedB.Status, "another tenant's template is not touched")
	assert.Len(t, logs.lines("info", templateMetaUpdatedLog), 1)
	assert.Equal(t, []string{"GET /v18.0/" + templateA.MetaTemplateID + "?fields=id,name,language,status"}, graph.madeRequests())
}

// Reactions.

func reactionLookupTarget(t *testing.T, app *App, account models.WhatsAppAccount, contact *models.Contact, createdAt time.Time) models.Message {
	t.Helper()
	target := models.Message{
		BaseModel:         models.BaseModel{ID: uuid.New()},
		OrganizationID:    account.OrganizationID,
		WhatsAppAccount:   account.Name,
		ContactID:         contact.ID,
		WhatsAppMessageID: "wamid.synthetic-lookup-target-" + uuid.NewString(),
		Direction:         models.DirectionIncoming,
		MessageType:       models.MessageTypeText,
		Content:           "synthetic lookup target",
		Status:            models.MessageStatusDelivered,
		Metadata:          models.JSONB{"reactions": []any{}, "synthetic_keep": "kept"},
	}
	require.NoError(t, app.DB.Create(&target).Error)
	require.NoError(t, app.DB.Model(&models.Message{}).Where("id = ?", target.ID).
		UpdateColumns(map[string]any{"created_at": createdAt, "updated_at": createdAt}).Error)
	target.CreatedAt = createdAt
	target.UpdatedAt = createdAt
	return target
}

func TestWhatsAppReactionStatusSearchFrom(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	statusAt := now.Add(-time.Hour)
	stamped := strconv.FormatInt(statusAt.Unix(), 10)
	assert.Equal(t, statusAt.Add(-whatsAppReactionStatusLookback),
		whatsAppReactionStatusSearchFrom(WebhookStatus{Status: "delivered", Timestamp: stamped}, now))
	assert.Equal(t, now.Add(-whatsAppStatusRetryHorizon-whatsAppReactionStatusLookback),
		whatsAppReactionStatusSearchFrom(WebhookStatus{Status: "read", Timestamp: "5"}, now),
		"without a usable timestamp the search starts from Meta's retry horizon")
	assert.True(t, whatsAppReactionStatusSearchFrom(WebhookStatus{Status: "failed", Timestamp: stamped}, now).IsZero(),
		"a failed status searches every message")
	assert.True(t, whatsAppReactionStatusSearchFrom(WebhookStatus{
		Status: "sent", Timestamp: stamped, Errors: []WebhookStatusError{{Code: 131009}},
	}, now).IsZero())
}

// The reaction lookup searches only messages that a delivered reaction can
// name (created within whatsAppReactionStatusLookback before its status), so
// it can use idx_messages_contact_created. A failed status still searches
// every message: Meta accepts a reaction to an older message and reports the
// failure only in the status.
func TestReactionStatusLookupIsBoundedByTheReactedMessageAge(t *testing.T) {
	app := webhookTestApp(t)
	account := orphanStatusTestAccount(t, app, true)
	contact := testutil.CreateTestContact(t, app.DB, account.OrganizationID)
	recipient := strings.TrimPrefix(contact.PhoneNumber, "+")
	logs := captureStatusTestLogs(app)
	recent := reactionLookupTarget(t, app, account, contact, time.Now().Add(-20*24*time.Hour))
	old := reactionLookupTarget(t, app, account, contact, time.Now().Add(-60*24*time.Hour))
	recentWAMID := "wamid.synthetic-lookup-recent-" + uuid.NewString()
	oldWAMID := "wamid.synthetic-lookup-old-" + uuid.NewString()
	require.NoError(t, app.recordSentWhatsAppReactionWAMID(account.OrganizationID, recent.ID, recentWAMID))
	require.NoError(t, app.recordSentWhatsAppReactionWAMID(account.OrganizationID, old.ID, oldWAMID))
	now := strconv.FormatInt(time.Now().Unix(), 10)

	require.NoError(t, app.processStatusUpdate(account.PhoneID, WebhookStatus{
		ID: recentWAMID, Status: "delivered", Timestamp: now, RecipientID: recipient,
	}))
	err := app.processStatusUpdate(account.PhoneID, WebhookStatus{
		ID: oldWAMID, Status: "delivered", Timestamp: now, RecipientID: recipient,
	})
	require.ErrorIs(t, err, errWhatsAppStatusOwnerPending, "a delivered reaction cannot name a message that old")
	require.NoError(t, app.processStatusUpdate(account.PhoneID, WebhookStatus{
		ID: oldWAMID, Status: "failed", Timestamp: now, RecipientID: recipient,
		Errors: []WebhookStatusError{{Code: 131009, Title: "Parameter value is not valid"}},
	}))
	assert.Len(t, logs.lines("warn", "Acknowledged failed WhatsApp status for a ReReply send that stores no message"), 1)
	assert.Len(t, logs.lines("info", nonMessageSendAckLog), 1)
}

// Recording a sent reaction's WAMID keeps the reacted-to message's updated_at
// and every other metadata key, and only that tenant can record it.
