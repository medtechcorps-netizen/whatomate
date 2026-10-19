package handlers_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/handlers"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
)

type laneANoteFixture struct {
	app     *handlers.App
	org     *models.Organization
	creator *models.User
	contact *models.Contact
	note    *models.ConversationNote
}

func newLaneANoteFixture(t *testing.T, content string) *laneANoteFixture {
	t.Helper()
	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	role := chatRWRole(t, app.DB, org.ID)
	creator := testutil.CreateTestUser(t, app.DB, org.ID, testutil.WithRoleID(&role.ID))
	contact := testutil.CreateTestContact(t, app.DB, org.ID)
	note := &models.ConversationNote{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: org.ID,
		ContactID:      contact.ID,
		CreatedByID:    creator.ID,
		Content:        content,
	}
	require.NoError(t, app.DB.Create(note).Error)
	return &laneANoteFixture{app: app, org: org, creator: creator, contact: contact, note: note}
}

func (f *laneANoteFixture) auditLogs(t *testing.T) []models.AuditLog {
	t.Helper()
	var logs []models.AuditLog
	require.NoError(t, f.app.DB.
		Where("organization_id = ? AND resource_type = ? AND resource_id = ?", f.org.ID, "conversation_note", f.note.ID).
		Order("created_at ASC").
		Find(&logs).Error)
	return logs
}

func laneAAuditJSON(t *testing.T, log models.AuditLog) string {
	t.Helper()
	encoded, err := json.Marshal(log.Changes)
	require.NoError(t, err)
	return string(encoded)
}

func TestLaneA_UpdateConversationNote_AuditsWithoutContent(t *testing.T) {
	f := newLaneANoteFixture(t, "BP 140/90, review in 2 weeks")
	time.Sleep(10 * time.Millisecond)

	req := testutil.NewJSONRequest(t, map[string]any{"content": "BP 150/95, review next week"})
	testutil.SetAuthContext(req, f.org.ID, f.creator.ID)
	testutil.SetPathParam(req, "id", f.contact.ID.String())
	testutil.SetPathParam(req, "note_id", f.note.ID.String())
	require.NoError(t, f.app.UpdateConversationNote(req))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req))

	var resp struct {
		Data handlers.ConversationNoteResponse `json:"data"`
	}
	require.NoError(t, json.Unmarshal(testutil.GetResponseBody(req), &resp))
	assert.True(t, resp.Data.UpdatedAt.After(resp.Data.CreatedAt), "an edited note must report updated_at > created_at")

	logs := f.auditLogs(t)
	require.Len(t, logs, 1)
	assert.Equal(t, models.AuditActionUpdated, logs[0].Action)
	assert.Equal(t, f.creator.ID, logs[0].UserID)
	changes := laneAAuditJSON(t, logs[0])
	assert.Contains(t, changes, `"field":"content"`)
	assert.NotContains(t, changes, "BP 140/90")
	assert.NotContains(t, changes, "BP 150/95")
}

func TestLaneA_UpdateConversationNote_UnchangedContentIsNotAudited(t *testing.T) {
	f := newLaneANoteFixture(t, "same text")

	req := testutil.NewJSONRequest(t, map[string]any{"content": "same text"})
	testutil.SetAuthContext(req, f.org.ID, f.creator.ID)
	testutil.SetPathParam(req, "id", f.contact.ID.String())
	testutil.SetPathParam(req, "note_id", f.note.ID.String())
	require.NoError(t, f.app.UpdateConversationNote(req))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req))

	assert.Empty(t, f.auditLogs(t))
}

func TestLaneA_DeleteConversationNote_AuditsWithoutContent(t *testing.T) {
	f := newLaneANoteFixture(t, "Allergic to penicillin")

	req := testutil.NewRequest(t)
	testutil.SetAuthContext(req, f.org.ID, f.creator.ID)
	testutil.SetPathParam(req, "id", f.contact.ID.String())
	testutil.SetPathParam(req, "note_id", f.note.ID.String())
	require.NoError(t, f.app.DeleteConversationNote(req))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req))

	logs := f.auditLogs(t)
	require.Len(t, logs, 1)
	assert.Equal(t, models.AuditActionDeleted, logs[0].Action)
	changes := laneAAuditJSON(t, logs[0])
	assert.Contains(t, changes, f.contact.ID.String())
	assert.False(t, strings.Contains(strings.ToLower(changes), "penicillin"))
}

func TestLaneA_ConversationNote_ForbiddenEditIsNotAudited(t *testing.T) {
	f := newLaneANoteFixture(t, "original")
	role := chatRWRole(t, f.app.DB, f.org.ID)
	other := testutil.CreateTestUser(t, f.app.DB, f.org.ID, testutil.WithRoleID(&role.ID))

	req := testutil.NewJSONRequest(t, map[string]any{"content": "changed"})
	testutil.SetAuthContext(req, f.org.ID, other.ID)
	testutil.SetPathParam(req, "id", f.contact.ID.String())
	testutil.SetPathParam(req, "note_id", f.note.ID.String())
	require.NoError(t, f.app.UpdateConversationNote(req))
	require.Equal(t, fasthttp.StatusForbidden, testutil.GetResponseStatusCode(req))

	assert.Empty(t, f.auditLogs(t))
}
