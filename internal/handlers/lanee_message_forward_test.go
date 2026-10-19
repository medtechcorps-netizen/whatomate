package handlers_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/handlers"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/pkg/whatsapp"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
	"github.com/zerodha/fastglue"
	"gorm.io/gorm"
)

type laneEForwardFixture struct {
	app     *handlers.App
	mock    *mockWhatsAppServer
	org     *models.Organization
	user    *models.User
	account *models.WhatsAppAccount
	source  *models.Contact
	target  *models.Contact
	media   string
}

func laneENewForwardFixture(t *testing.T) *laneEForwardFixture {
	t.Helper()
	mock := newMockWhatsAppServer()
	t.Cleanup(mock.close)
	app := newMsgTestApp(t, mock)
	mediaRoot := t.TempDir()
	app.Config.Storage.LocalPath = mediaRoot
	org := testutil.CreateTestOrganization(t, app.DB)
	adminRole := testutil.CreateAdminRole(t, app.DB, org.ID)
	user := testutil.CreateTestUser(t, app.DB, org.ID, testutil.WithRoleID(&adminRole.ID))
	account := createTestAccount(t, app, org.ID)
	source := testutil.CreateTestContactWith(t, app.DB, org.ID, testutil.WithContactAccount(account.Name))
	target := testutil.CreateTestContactWith(t, app.DB, org.ID, testutil.WithContactAccount(account.Name))
	laneESetLastInbound(t, app.DB, target, time.Now().Add(-time.Hour))
	return &laneEForwardFixture{app, mock, org, user, account, source, target, mediaRoot}
}

func laneESetLastInbound(t *testing.T, db *gorm.DB, contact *models.Contact, at time.Time) {
	t.Helper()
	require.NoError(t, db.Model(&models.Contact{}).Where("id = ?", contact.ID).
		Update("last_inbound_at", at).Error)
}

func (f *laneEForwardFixture) storedMessage(t *testing.T, contact *models.Contact, messageType models.MessageType, content, mediaURL string) *models.Message {
	t.Helper()
	message := &models.Message{
		BaseModel:         models.BaseModel{ID: uuid.New()},
		OrganizationID:    f.org.ID,
		WhatsAppAccount:   f.account.Name,
		ContactID:         contact.ID,
		WhatsAppMessageID: "wamid.source-" + uuid.NewString()[:8],
		Direction:         models.DirectionIncoming,
		MessageType:       messageType,
		Content:           content,
		MediaURL:          mediaURL,
		Status:            models.MessageStatusDelivered,
	}
	if mediaURL != "" {
		message.MediaMimeType = "image/jpeg"
		message.MediaFilename = "scan.jpg"
	}
	require.NoError(t, f.app.DB.Create(message).Error)
	return message
}

func (f *laneEForwardFixture) forward(t *testing.T, userID, targetID uuid.UUID, body map[string]any) *fastglue.Request {
	t.Helper()
	req := testutil.NewJSONRequest(t, body)
	testutil.SetAuthContext(req, f.org.ID, userID)
	testutil.SetPathParam(req, "id", targetID.String())
	require.NoError(t, f.app.SendMessage(req))
	f.app.WaitForBackgroundTasks()
	return req
}

type laneEErrorEnvelope struct {
	Message   string `json:"message"`
	ErrorType string `json:"error_type"`
}

func laneERefusal(t *testing.T, req *fastglue.Request) laneEErrorEnvelope {
	t.Helper()
	var envelope laneEErrorEnvelope
	require.NoError(t, json.Unmarshal(testutil.GetResponseBody(req), &envelope))
	return envelope
}

func (f *laneEForwardFixture) messagesFor(t *testing.T, contactID uuid.UUID) []models.Message {
	t.Helper()
	var messages []models.Message
	require.NoError(t, f.app.DB.Where("organization_id = ? AND contact_id = ?", f.org.ID, contactID).
		Order("created_at ASC").Find(&messages).Error)
	return messages
}

func TestLaneEForwardTextCreatesOrdinaryOutboundWithProvenance(t *testing.T) {
	f := laneENewForwardFixture(t)
	source := f.storedMessage(t, f.source, models.MessageTypeText, "Please send my lab results", "")

	req := f.forward(t, f.user.ID, f.target.ID, map[string]any{
		"type":                  "text",
		"content":               map[string]string{"body": ""},
		"forward_of_message_id": source.ID.String(),
	})
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req), string(testutil.GetResponseBody(req)))

	var resp struct {
		Data handlers.MessageResponse `json:"data"`
	}
	require.NoError(t, json.Unmarshal(testutil.GetResponseBody(req), &resp))
	assert.True(t, resp.Data.IsForwarded)
	assert.Equal(t, f.target.ID, resp.Data.ContactID)
	assert.Equal(t, models.MessageTypeText, resp.Data.MessageType)
	assert.Equal(t, models.DirectionOutgoing, resp.Data.Direction)

	var stored models.Message
	require.NoError(t, f.app.DB.First(&stored, "id = ?", resp.Data.ID).Error)
	assert.Equal(t, "Please send my lab results", stored.Content)
	assert.False(t, stored.IsReply)
	provenance, ok := stored.Metadata["forwarded_from"].(map[string]any)
	require.True(t, ok, "metadata.forwarded_from must be recorded")
	assert.Equal(t, source.ID.String(), provenance["message_id"])
	assert.Equal(t, f.source.ID.String(), provenance["contact_id"])

	// The customer receives a normal text: no context, no forward marker.
	require.Equal(t, 1, f.mock.sentMessageCount())
	f.mock.mu.Lock()
	sent := f.mock.sentMessages[0]
	f.mock.mu.Unlock()
	assert.Equal(t, "text", sent["type"])
	assert.NotContains(t, sent, "context")
	assert.Equal(t, "Please send my lab results", sent["text"].(map[string]any)["body"])

	var audits []models.AuditLog
	require.NoError(t, f.app.DB.Where("organization_id = ? AND resource_type = ? AND resource_id = ?",
		f.org.ID, "message", stored.ID).Find(&audits).Error)
	require.Len(t, audits, 1)
	assert.Equal(t, models.AuditActionCreated, audits[0].Action)
	auditJSON, err := json.Marshal(audits[0].Changes)
	require.NoError(t, err)
	assert.Contains(t, string(auditJSON), source.ID.String())
	assert.Contains(t, string(auditJSON), f.target.ID.String())
	assert.NotContains(t, string(auditJSON), "lab results", "audit must not copy message content")

	list := testutil.NewGETRequest(t)
	testutil.SetAuthContext(list, f.org.ID, f.user.ID)
	testutil.SetPathParam(list, "id", f.target.ID.String())
	require.NoError(t, f.app.GetMessages(list))
	var listed struct {
		Data struct {
			Messages []handlers.MessageResponse `json:"messages"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(testutil.GetResponseBody(list), &listed))
	require.Len(t, listed.Data.Messages, 1)
	assert.True(t, listed.Data.Messages[0].IsForwarded)
}

func TestLaneEForwardMediaCopiesStoredBytesAndUploadsAgain(t *testing.T) {
	f := laneENewForwardFixture(t)
	require.NoError(t, os.MkdirAll(filepath.Join(f.media, "images"), 0o755))
	original := []byte("jpeg-bytes-from-tenant-storage")
	require.NoError(t, os.WriteFile(filepath.Join(f.media, "images", "source.jpg"), original, 0o600))
	source := f.storedMessage(t, f.source, models.MessageTypeImage, "X-ray caption", "images/source.jpg")

	req := f.forward(t, f.user.ID, f.target.ID, map[string]any{
		"type":                  "text",
		"forward_of_message_id": source.ID.String(),
	})
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req), string(testutil.GetResponseBody(req)))

	var resp struct {
		Data handlers.MessageResponse `json:"data"`
	}
	require.NoError(t, json.Unmarshal(testutil.GetResponseBody(req), &resp))

	messages := f.messagesFor(t, f.target.ID)
	require.Len(t, messages, 1)
	forwarded := messages[0]
	assert.Equal(t, resp.Data.MediaURL, forwarded.MediaURL, "the planned key is stored up front; no later write")
	assert.Equal(t, models.MessageTypeImage, forwarded.MessageType)
	assert.Equal(t, "X-ray caption", forwarded.Content)
	assert.Equal(t, "image/jpeg", forwarded.MediaMimeType)
	require.NotEmpty(t, forwarded.MediaURL)
	assert.NotEqual(t, source.MediaURL, forwarded.MediaURL, "the forward owns an independent stored copy")
	copied, err := os.ReadFile(filepath.Join(f.media, filepath.FromSlash(forwarded.MediaURL)))
	require.NoError(t, err)
	assert.Equal(t, original, copied)

	// Bytes were uploaded again; the inbound WhatsApp message/media identity is never resent.
	require.Len(t, f.mock.uploadedMedia, 1)
	require.Equal(t, 1, f.mock.sentMessageCount())
	f.mock.mu.Lock()
	sent := f.mock.sentMessages[0]
	f.mock.mu.Unlock()
	image := sent["image"].(map[string]any)
	assert.Equal(t, f.mock.nextMediaID, image["id"])
	assert.Equal(t, "X-ray caption", image["caption"])
	assert.NotContains(t, sent, "context")
	assert.Equal(t, models.MessageStatusSent, forwarded.Status)
}

func TestLaneEForwardRefusesBeforePersistingOrSending(t *testing.T) {
	f := laneENewForwardFixture(t)
	text := f.storedMessage(t, f.source, models.MessageTypeText, "hello", "")
	pendingMedia := f.storedMessage(t, f.source, models.MessageTypeImage, "", "")

	closed := testutil.CreateTestContactWith(t, f.app.DB, f.org.ID, testutil.WithContactAccount(f.account.Name))
	expired := testutil.CreateTestContactWith(t, f.app.DB, f.org.ID, testutil.WithContactAccount(f.account.Name))
	laneESetLastInbound(t, f.app.DB, expired, time.Now().Add(-25*time.Hour))

	otherOrg := testutil.CreateTestOrganization(t, f.app.DB)
	foreignContact := testutil.CreateTestContact(t, f.app.DB, otherOrg.ID)
	foreign := &models.Message{
		BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: otherOrg.ID, ContactID: foreignContact.ID,
		Direction: models.DirectionIncoming, MessageType: models.MessageTypeText, Content: "other tenant",
		Status: models.MessageStatusDelivered,
	}
	require.NoError(t, f.app.DB.Create(foreign).Error)

	cases := []struct {
		name      string
		target    uuid.UUID
		body      map[string]any
		status    int
		errorType string
	}{
		{"invalid id", f.target.ID, map[string]any{"forward_of_message_id": "nope"}, fasthttp.StatusBadRequest, "forward_invalid"},
		{"with reply", f.target.ID, map[string]any{"forward_of_message_id": text.ID.String(), "reply_to_message_id": text.ID.String()}, fasthttp.StatusBadRequest, "forward_invalid"},
		{"with new text", f.target.ID, map[string]any{"forward_of_message_id": text.ID.String(), "content": map[string]string{"body": "extra"}}, fasthttp.StatusBadRequest, "forward_invalid"},
		{"unknown source", f.target.ID, map[string]any{"forward_of_message_id": uuid.NewString()}, fasthttp.StatusNotFound, "forward_source_not_found"},
		{"other organization source", f.target.ID, map[string]any{"forward_of_message_id": foreign.ID.String()}, fasthttp.StatusNotFound, "forward_source_not_found"},
		{"media not hydrated", f.target.ID, map[string]any{"forward_of_message_id": pendingMedia.ID.String()}, fasthttp.StatusConflict, "forward_media_pending"},
		{"window never opened", closed.ID, map[string]any{"forward_of_message_id": text.ID.String()}, fasthttp.StatusConflict, "forward_window_closed"},
		{"window expired", expired.ID, map[string]any{"forward_of_message_id": text.ID.String()}, fasthttp.StatusConflict, "forward_window_closed"},
		{"unknown target", uuid.New(), map[string]any{"forward_of_message_id": text.ID.String()}, fasthttp.StatusNotFound, "forward_target_not_found"},
	}
	for _, unsupported := range []models.MessageType{
		models.MessageType("sticker"), models.MessageTypeLocation, models.MessageType("contacts"),
		models.MessageTypeContact, models.MessageTypeInteractive, models.MessageTypeTemplate,
		models.MessageTypeFlow, models.MessageTypeReaction, models.MessageType("button_reply"),
	} {
		message := f.storedMessage(t, f.source, unsupported, "body", "")
		cases = append(cases, struct {
			name      string
			target    uuid.UUID
			body      map[string]any
			status    int
			errorType string
		}{"unsupported " + string(unsupported), f.target.ID, map[string]any{"forward_of_message_id": message.ID.String()}, fasthttp.StatusUnprocessableEntity, "forward_unsupported_type"})
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := len(f.messagesFor(t, tc.target))
			req := f.forward(t, f.user.ID, tc.target, tc.body)
			assert.Equal(t, tc.status, testutil.GetResponseStatusCode(req), string(testutil.GetResponseBody(req)))
			assert.Equal(t, tc.errorType, laneERefusal(t, req).ErrorType)
			assert.Len(t, f.messagesFor(t, tc.target), before, "a refused forward persists nothing")
		})
	}
	assert.Zero(t, f.mock.sentMessageCount(), "a refused forward never reaches Meta")
	assert.Empty(t, f.mock.uploadedMedia)
}

func TestLaneEForwardRefusesTargetUnderIdentityReview(t *testing.T) {
	f := laneENewForwardFixture(t)
	text := f.storedMessage(t, f.source, models.MessageTypeText, "hello", "")
	require.NoError(t, f.app.DB.Create(&models.WhatsAppCoexistenceState{
		ID: uuid.New(), OrganizationID: f.org.ID, WhatsAppAccountID: f.account.ID,
		OnboardingStatus: models.CoexistenceOnboardingStatusConnected, OnboardingCycle: 1,
		SyncStatus:        models.CoexistenceSyncStatusNotRequested,
		ContactSyncStatus: models.CoexistenceSyncStatusNotRequested,
		HistoryConsent:    models.CoexistenceHistoryConsentUnknown,
		HistorySyncStatus: models.CoexistenceSyncStatusNotRequested,
		LifecycleStatus:   models.CoexistenceLifecycleStatusConnected,
		LifecycleMetadata: models.JSONB{}, Version: 1,
	}).Error)
	principal := "forward-review-" + uuid.NewString()
	require.NoError(t, f.app.DB.Model(&models.Contact{}).Where("id = ?", f.target.ID).Update("bs_uid", principal).Error)
	claim := handlers.WhatsAppIdentityReviewClaim{
		OrganizationID:          f.org.ID,
		WhatsAppAccountID:       f.account.ID,
		OnboardingCycle:         1,
		DirectPrimaryBSUID:      principal,
		VerifiedEventDigest:     strings.Repeat("a", 64),
		VerifiedEventProvenance: handlers.WhatsAppIdentityReviewVerifiedMetaEvent,
		SelectorBodyDigest:      strings.Repeat("b", 64),
	}
	require.NoError(t, f.app.DB.Transaction(func(tx *gorm.DB) error {
		_, created, err := f.app.CreateOrReuseWhatsAppIdentityReviewHold(tx, &claim)
		if err == nil {
			require.True(t, created)
		}
		return err
	}))
	state, err := f.app.GetWhatsAppIdentityReviewEffectiveState(f.app.DB, f.org.ID, f.target.ID)
	require.NoError(t, err)
	require.True(t, state.Blocked)

	req := f.forward(t, f.user.ID, f.target.ID, map[string]any{"forward_of_message_id": text.ID.String()})
	assert.Equal(t, fasthttp.StatusConflict, testutil.GetResponseStatusCode(req))
	assert.Equal(t, "forward_identity_review", laneERefusal(t, req).ErrorType)
	assert.Empty(t, f.messagesFor(t, f.target.ID))
	assert.Zero(t, f.mock.sentMessageCount())
}

func TestLaneEForwardHidesSourcesTheAgentCannotSee(t *testing.T) {
	f := laneENewForwardFixture(t)
	role := testutil.CreateTestRoleWithKeys(t, f.app.DB, f.org.ID, "forward-agent", []string{"chat:read", "chat:write"})
	agent := testutil.CreateTestUser(t, f.app.DB, f.org.ID, testutil.WithRoleID(&role.ID))
	require.NoError(t, f.app.DB.Model(&models.Contact{}).Where("id = ?", f.target.ID).
		Update("assigned_user_id", agent.ID).Error)
	hidden := f.storedMessage(t, f.source, models.MessageTypeText, "not assigned to the agent", "")

	req := f.forward(t, agent.ID, f.target.ID, map[string]any{"forward_of_message_id": hidden.ID.String()})
	assert.Equal(t, fasthttp.StatusNotFound, testutil.GetResponseStatusCode(req))
	assert.Equal(t, "forward_source_not_found", laneERefusal(t, req).ErrorType)

	require.NoError(t, f.app.DB.Model(&models.Contact{}).Where("id = ?", f.source.ID).
		Update("assigned_user_id", agent.ID).Error)
	req = f.forward(t, agent.ID, f.target.ID, map[string]any{"forward_of_message_id": hidden.ID.String()})
	assert.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req), string(testutil.GetResponseBody(req)))

	writeOnly := testutil.CreateTestRoleWithKeys(t, f.app.DB, f.org.ID, "forward-write-only", []string{"chat:write", "contacts:read"})
	writer := testutil.CreateTestUser(t, f.app.DB, f.org.ID, testutil.WithRoleID(&writeOnly.ID))
	req = f.forward(t, writer.ID, f.target.ID, map[string]any{"forward_of_message_id": hidden.ID.String()})
	assert.Equal(t, fasthttp.StatusForbidden, testutil.GetResponseStatusCode(req))
}

func TestLaneESendMediaMessageCarriesReplyContext(t *testing.T) {
	f := laneENewForwardFixture(t)
	original := f.storedMessage(t, f.target, models.MessageTypeText, "Which form?", "")

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	require.NoError(t, writer.WriteField("contact_id", f.target.ID.String()))
	require.NoError(t, writer.WriteField("type", "document"))
	require.NoError(t, writer.WriteField("reply_to_message_id", original.ID.String()))
	part, err := writer.CreateFormFile("file", "consent.pdf")
	require.NoError(t, err)
	_, err = part.Write([]byte("%PDF-1.4 consent"))
	require.NoError(t, err)
	require.NoError(t, writer.Close())

	req := testutil.NewRequest(t)
	req.RequestCtx.Request.Header.SetMethod("POST")
	req.RequestCtx.Request.Header.SetContentType(writer.FormDataContentType())
	req.RequestCtx.Request.SetBody(body.Bytes())
	testutil.SetAuthContext(req, f.org.ID, f.user.ID)
	require.NoError(t, f.app.SendMediaMessage(req))
	f.app.WaitForBackgroundTasks()
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req), string(testutil.GetResponseBody(req)))

	var resp struct {
		Data handlers.MessageResponse `json:"data"`
	}
	require.NoError(t, json.Unmarshal(testutil.GetResponseBody(req), &resp))
	assert.True(t, resp.Data.IsReply)
	require.NotNil(t, resp.Data.ReplyToMessage)
	assert.Equal(t, original.ID.String(), resp.Data.ReplyToMessage.ID)

	require.Equal(t, 1, f.mock.sentMessageCount())
	f.mock.mu.Lock()
	sent := f.mock.sentMessages[0]
	f.mock.mu.Unlock()
	assert.Equal(t, "document", sent["type"])
	assert.Equal(t, map[string]any{"message_id": original.WhatsAppMessageID}, sent["context"])
}

func TestLaneESendMediaMessageRejectsForeignReply(t *testing.T) {
	f := laneENewForwardFixture(t)
	elsewhere := f.storedMessage(t, f.source, models.MessageTypeText, "other chat", "")

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	require.NoError(t, writer.WriteField("contact_id", f.target.ID.String()))
	require.NoError(t, writer.WriteField("type", "image"))
	require.NoError(t, writer.WriteField("reply_to_message_id", elsewhere.ID.String()))
	part, err := writer.CreateFormFile("file", "photo.jpg")
	require.NoError(t, err)
	_, err = part.Write([]byte("jpeg"))
	require.NoError(t, err)
	require.NoError(t, writer.Close())

	req := testutil.NewRequest(t)
	req.RequestCtx.Request.Header.SetMethod("POST")
	req.RequestCtx.Request.Header.SetContentType(writer.FormDataContentType())
	req.RequestCtx.Request.SetBody(body.Bytes())
	testutil.SetAuthContext(req, f.org.ID, f.user.ID)
	require.NoError(t, f.app.SendMediaMessage(req))
	f.app.WaitForBackgroundTasks()
	assert.Equal(t, fasthttp.StatusBadRequest, testutil.GetResponseStatusCode(req))
	assert.Zero(t, f.mock.sentMessageCount())
}

// Storage and Meta I/O run after the request commits, so a source object that
// vanished fails the new message asynchronously, before any provider call.
func TestLaneEForwardMediaMissingFromStorageFailsBeforeMeta(t *testing.T) {
	f := laneENewForwardFixture(t)
	missing := f.storedMessage(t, f.source, models.MessageTypeImage, "", "images/missing.jpg")

	req := f.forward(t, f.user.ID, f.target.ID, map[string]any{"forward_of_message_id": missing.ID.String()})
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req), string(testutil.GetResponseBody(req)))

	messages := f.messagesFor(t, f.target.ID)
	require.Len(t, messages, 1)
	assert.Equal(t, models.MessageStatusFailed, messages[0].Status)
	assert.Contains(t, messages[0].ErrorMessage, "no longer available")
	assert.Empty(t, f.mock.uploadedMedia)
	assert.Zero(t, f.mock.sentMessageCount())
}

// laneEStrictGraph behaves like Graph for uploads: a file part without a
// file name is rejected, as Graph treats it as no file at all.
type laneEStrictGraph struct {
	mu           sync.Mutex
	uploads      []string
	contentTypes []string
	sends        int
}

func (g *laneEStrictGraph) snapshot() ([]string, int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.uploads...), g.sends
}

func (g *laneEStrictGraph) uploadTypes() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.contentTypes...)
}

func laneEUseStrictGraph(t *testing.T, f *laneEForwardFixture) *laneEStrictGraph {
	t.Helper()
	graph := &laneEStrictGraph{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/media"):
			reader, err := r.MultipartReader()
			if err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			for {
				part, err := reader.NextPart()
				if err != nil {
					break
				}
				if part.FormName() != "file" {
					continue
				}
				graph.mu.Lock()
				graph.uploads = append(graph.uploads, part.FileName())
				graph.contentTypes = append(graph.contentTypes, part.Header.Get("Content-Type"))
				graph.mu.Unlock()
				if part.FileName() == "" {
					w.WriteHeader(http.StatusBadRequest)
					_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": "file is required", "code": 100}})
					return
				}
				// Graph matches bare media types; parameters are rejected.
				if strings.Contains(part.Header.Get("Content-Type"), ";") {
					w.WriteHeader(http.StatusBadRequest)
					_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": "unsupported type", "code": 131053}})
					return
				}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "media-strict"})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/messages"):
			graph.mu.Lock()
			graph.sends++
			graph.mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"messages": []map[string]string{{"id": "wamid.strict"}}})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	client := whatsapp.NewWithTimeout(testutil.NopLogger(), 5*time.Second)
	client.HTTPClient = &http.Client{Transport: &testServerTransport{serverURL: server.URL}}
	f.app.WhatsApp = client
	return graph
}

// Patient photos, videos and voice notes are stored without a file name.
func TestLaneEForwardUploadsUnnamedMediaWithAFileName(t *testing.T) {
	f := laneENewForwardFixture(t)
	graph := laneEUseStrictGraph(t, f)
	require.NoError(t, os.MkdirAll(filepath.Join(f.media, "audio"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(f.media, "audio", "voice.ogg"), []byte("ogg-bytes"), 0o600))
	voice := f.storedMessage(t, f.source, models.MessageTypeAudio, "", "audio/voice.ogg")
	require.NoError(t, f.app.DB.Model(&models.Message{}).Where("id = ?", voice.ID).
		Updates(map[string]any{"media_filename": "", "media_mime_type": "audio/ogg; codecs=opus"}).Error)

	req := f.forward(t, f.user.ID, f.target.ID, map[string]any{"forward_of_message_id": voice.ID.String()})
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req), string(testutil.GetResponseBody(req)))

	uploads, sends := graph.snapshot()
	require.Len(t, uploads, 1)
	assert.NotEmpty(t, uploads[0])
	assert.True(t, strings.HasSuffix(uploads[0], ".ogg"), uploads[0])
	assert.Equal(t, []string{"audio/ogg"}, graph.uploadTypes(), "uploaded with the bare media type")
	assert.Equal(t, 1, sends)
	messages := f.messagesFor(t, f.target.ID)
	require.Len(t, messages, 1)
	assert.Equal(t, models.MessageStatusSent, messages[0].Status, messages[0].ErrorMessage)
	assert.Empty(t, messages[0].MediaFilename, "the forwarded row keeps the source's (empty) display name")
	assert.Equal(t, "audio/ogg; codecs=opus", messages[0].MediaMimeType, "the stored type is unchanged")
}

// WhatsApp's media limits are checked before anything is uploaded.
func TestLaneEForwardRefusesMediaAboveWhatsAppLimits(t *testing.T) {
	f := laneENewForwardFixture(t)
	graph := laneEUseStrictGraph(t, f)
	require.NoError(t, os.MkdirAll(filepath.Join(f.media, "images"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(f.media, "images", "big.jpg"), make([]byte, 5<<20+1), 0o600))
	big := f.storedMessage(t, f.source, models.MessageTypeImage, "", "images/big.jpg")

	req := f.forward(t, f.user.ID, f.target.ID, map[string]any{"forward_of_message_id": big.ID.String()})
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req), string(testutil.GetResponseBody(req)))

	messages := f.messagesFor(t, f.target.ID)
	require.Len(t, messages, 1)
	assert.Equal(t, models.MessageStatusFailed, messages[0].Status)
	assert.Contains(t, messages[0].ErrorMessage, "larger than WhatsApp allows (5 MB)")
	uploads, sends := graph.snapshot()
	assert.Empty(t, uploads)
	assert.Zero(t, sends)
}

func TestLaneEForwardRefusesDeletedSource(t *testing.T) {
	f := laneENewForwardFixture(t)
	tombstone := f.storedMessage(t, f.source, models.MessageTypeText, "[Message deleted from WhatsApp Business App]", "")
	require.NoError(t, f.app.DB.Model(&models.Message{}).Where("id = ?", tombstone.ID).
		Update("metadata", models.JSONB{"coexistence_revoked": true}).Error)
	softDeleted := f.storedMessage(t, f.source, models.MessageTypeText, "removed", "")
	require.NoError(t, f.app.DB.Delete(&models.Message{}, "id = ?", softDeleted.ID).Error)

	req := f.forward(t, f.user.ID, f.target.ID, map[string]any{"forward_of_message_id": tombstone.ID.String()})
	assert.Equal(t, fasthttp.StatusGone, testutil.GetResponseStatusCode(req))
	assert.Equal(t, "forward_source_deleted", laneERefusal(t, req).ErrorType)

	req = f.forward(t, f.user.ID, f.target.ID, map[string]any{"forward_of_message_id": softDeleted.ID.String()})
	assert.Equal(t, fasthttp.StatusNotFound, testutil.GetResponseStatusCode(req))
	assert.Empty(t, f.messagesFor(t, f.target.ID))
}

// Q5 (definite rejection): a failure before anything is persisted answers 503
// forward_temporarily_unavailable; nothing is saved and nothing reaches Meta.
// The failure is injected on the forward's own reads, after authorization:
// an expired context on the whole DB would fail the request's tenant check
// first (SendMessage answers 401 for that, which is not the forward path).
func TestLaneEForwardDefiniteRejectionIsRetryable(t *testing.T) {
	f := laneENewForwardFixture(t)
	text := f.storedMessage(t, f.source, models.MessageTypeText, "first", "")
	// One ordinary forward first, so the counts below have a baseline.
	req := f.forward(t, f.user.ID, f.target.ID, map[string]any{"forward_of_message_id": text.ID.String()})
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req), string(testutil.GetResponseBody(req)))
	sentBefore := f.mock.sentMessageCount()

	// The source-message lookup hits a database deadline. The hook counts its
	// hits and records whether the matched query had already failed, so the
	// test proves the injected deadline (not some earlier error) was the cause.
	sourceID := text.ID
	var (
		hookMu        sync.Mutex
		sourceHits    int
		priorQueryErr []error
	)
	sourceHook := "laneE:source_lookup_" + uuid.NewString()
	require.NoError(t, f.app.DB.Callback().Query().After("gorm:query").Register(sourceHook, func(db *gorm.DB) {
		if db.Statement.Table != "messages" {
			return
		}
		for _, v := range db.Statement.Vars {
			if v == any(sourceID) {
				hookMu.Lock()
				sourceHits++
				priorQueryErr = append(priorQueryErr, db.Error)
				hookMu.Unlock()
				_ = db.AddError(context.DeadlineExceeded)
				return
			}
		}
	}))
	removeSourceHook := func() { _ = f.app.DB.Callback().Query().Remove(sourceHook) }
	t.Cleanup(removeSourceHook)
	req = f.forward(t, f.user.ID, f.target.ID, map[string]any{"forward_of_message_id": text.ID.String()})
	removeSourceHook()

	hookMu.Lock()
	assert.Equal(t, 1, sourceHits, "exactly one source-message lookup, after authorization")
	assert.Equal(t, []error{nil}, priorQueryErr, "the matched query had not already failed")
	hookMu.Unlock()
	assert.Equal(t, fasthttp.StatusServiceUnavailable, testutil.GetResponseStatusCode(req), string(testutil.GetResponseBody(req)))
	assert.Equal(t, "forward_temporarily_unavailable", laneERefusal(t, req).ErrorType)
	assert.Len(t, f.messagesFor(t, f.target.ID), 1, "only the warm-up forward exists")
	assert.Equal(t, sentBefore, f.mock.sentMessageCount())

	// E2-R1: the account lookup itself hits a database deadline. Nothing has
	// been saved yet, so this is also a definite 503 (not the 400 a missing or
	// unusable account gets).
	org, accountName := f.org.ID, f.account.Name
	name := "laneE:account_lookup_" + uuid.NewString()
	require.NoError(t, f.app.DB.Callback().Query().After("gorm:query").Register(name, func(db *gorm.DB) {
		if db.Statement.Table != "whatsapp_accounts" {
			return
		}
		hasOrg, hasName := false, false
		for _, v := range db.Statement.Vars {
			hasOrg = hasOrg || v == any(org)
			hasName = hasName || v == any(accountName)
		}
		if hasOrg && hasName {
			_ = db.AddError(context.DeadlineExceeded)
		}
	}))
	t.Cleanup(func() { _ = f.app.DB.Callback().Query().Remove(name) })

	req = f.forward(t, f.user.ID, f.target.ID, map[string]any{"forward_of_message_id": text.ID.String()})
	assert.Equal(t, fasthttp.StatusServiceUnavailable, testutil.GetResponseStatusCode(req), string(testutil.GetResponseBody(req)))
	assert.Equal(t, "forward_temporarily_unavailable", laneERefusal(t, req).ErrorType)
	assert.Len(t, f.messagesFor(t, f.target.ID), 1)
	assert.Equal(t, sentBefore, f.mock.sentMessageCount())
}

// Q5: the pending-row INSERT fails inside its own transaction, which rolls
// back before COMMIT: definitely not accepted (503), one attempt only. The
// lost-COMMIT case (ambiguous) is in lanee_forward_internal_test.go.
func TestLaneEForwardFailedInsertIsDefiniteAndNotRetried(t *testing.T) {
	f := laneENewForwardFixture(t)
	text := f.storedMessage(t, f.source, models.MessageTypeText, "insert fails", "")
	f.app.WaitForBackgroundTasks()
	attempts := 0
	target := f.target.ID
	name := "laneE:failed_forward_insert_" + uuid.NewString()
	require.NoError(t, f.app.DB.Callback().Create().Before("gorm:create").Register(name, func(db *gorm.DB) {
		if message, ok := db.Statement.Dest.(*models.Message); ok && message.ContactID == target {
			attempts++
			_ = db.AddError(errors.New("synthetic: the pending-message INSERT failed before COMMIT"))
		}
	}))
	t.Cleanup(func() { _ = f.app.DB.Callback().Create().Remove(name) })

	req := f.forward(t, f.user.ID, f.target.ID, map[string]any{"forward_of_message_id": text.ID.String()})
	assert.Equal(t, fasthttp.StatusServiceUnavailable, testutil.GetResponseStatusCode(req), string(testutil.GetResponseBody(req)))
	assert.Equal(t, "forward_temporarily_unavailable", laneERefusal(t, req).ErrorType)
	assert.Equal(t, 1, attempts, "never a second automatic attempt")
	assert.Empty(t, f.messagesFor(t, f.target.ID))
	assert.Zero(t, f.mock.sentMessageCount())
}

// Item 8: a missing account is a definite refusal that would repeat: 400.
func TestLaneEForwardUnknownAccountIsNotRetryable(t *testing.T) {
	f := laneENewForwardFixture(t)
	text := f.storedMessage(t, f.source, models.MessageTypeText, "unknown account", "")
	req := f.forward(t, f.user.ID, f.target.ID, map[string]any{
		"forward_of_message_id": text.ID.String(),
		"whatsapp_account":      "no-such-account",
	})
	assert.Equal(t, fasthttp.StatusBadRequest, testutil.GetResponseStatusCode(req), string(testutil.GetResponseBody(req)))
	envelope := laneERefusal(t, req)
	assert.Equal(t, "forward_account_unavailable", envelope.ErrorType)
	assert.Equal(t, "Failed to resolve WhatsApp account", envelope.Message, "the message an ordinary send returns")
	assert.Empty(t, f.messagesFor(t, f.target.ID))
}

// Q6, projection only: GetMessages exposes the derived state of stalled
// forwards. The rows are seeded to look like what an exit leaves behind; the
// real exit boundaries are exercised in lanee_forward_internal_test.go. Also
// checks that a delivered forward recorded the provider phase.
func TestLaneEForwardStalledStatesProjection(t *testing.T) {
	f := laneENewForwardFixture(t)
	text := f.storedMessage(t, f.source, models.MessageTypeText, "delivered normally", "")
	req := f.forward(t, f.user.ID, f.target.ID, map[string]any{"forward_of_message_id": text.ID.String()})
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req))
	delivered := f.messagesFor(t, f.target.ID)
	require.Len(t, delivered, 1)
	assert.Equal(t, models.MessageStatusSent, delivered[0].Status)
	assert.Equal(t, "provider", delivered[0].Metadata["forward_dispatch"], "the provider phase is recorded before the send")

	longAgo := time.Now().Add(-10 * time.Minute)
	seed := func(dispatch models.JSONB) uuid.UUID {
		metadata := models.JSONB{"forwarded_from": map[string]any{"message_id": text.ID.String(), "contact_id": f.source.ID.String()}}
		for key, value := range dispatch {
			metadata[key] = value
		}
		row := &models.Message{
			BaseModel:       models.BaseModel{ID: uuid.New(), CreatedAt: longAgo, UpdatedAt: longAgo},
			OrganizationID:  f.org.ID,
			WhatsAppAccount: f.account.Name,
			ContactID:       f.target.ID,
			Direction:       models.DirectionOutgoing,
			MessageType:     models.MessageTypeText,
			Content:         "stalled",
			Status:          models.MessageStatusPending,
			SentByUserID:    &f.user.ID,
			Metadata:        metadata,
		}
		require.NoError(t, f.app.DB.Create(row).Error)
		require.NoError(t, f.app.DB.Model(&models.Message{}).Where("id = ?", row.ID).
			UpdateColumns(map[string]any{"created_at": longAgo, "updated_at": longAgo}).Error)
		return row.ID
	}
	// Still queued: delayed, status unknown.
	delayed := seed(models.JSONB{"forward_dispatch": "queued"})
	// Provider phase recorded without a result: may have been sent.
	uncertain := seed(models.JSONB{"forward_dispatch": "provider", "forward_dispatch_at": longAgo.UTC().Format(time.RFC3339Nano)})

	list := testutil.NewGETRequest(t)
	testutil.SetAuthContext(list, f.org.ID, f.user.ID)
	testutil.SetPathParam(list, "id", f.target.ID.String())
	require.NoError(t, f.app.GetMessages(list))
	var listed struct {
		Data struct {
			Messages []handlers.MessageResponse `json:"messages"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(testutil.GetResponseBody(list), &listed))
	states := map[uuid.UUID]string{}
	dispatch := map[uuid.UUID]handlers.MessageResponse{}
	for _, message := range listed.Data.Messages {
		states[message.ID] = message.ForwardDeliveryState
		dispatch[message.ID] = message
	}
	assert.Equal(t, "delayed", states[delayed])
	assert.Equal(t, "uncertain", states[uncertain])
	// The marker comes with the message, so an open chat can apply the rule.
	assert.Equal(t, "queued", dispatch[delayed].ForwardDispatch)
	assert.Equal(t, "provider", dispatch[uncertain].ForwardDispatch)
	assert.NotEmpty(t, dispatch[uncertain].ForwardDispatchAt)
	assert.Equal(t, "", states[delivered[0].ID])
	// Neither stalled row is resent by anything on the read path.
	assert.Equal(t, 1, f.mock.sentMessageCount())
}
