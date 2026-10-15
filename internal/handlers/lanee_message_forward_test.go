package handlers_test

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"os"
	"path/filepath"
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

	messages := f.messagesFor(t, f.target.ID)
	require.Len(t, messages, 1)
	forwarded := messages[0]
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
	missingMedia := f.storedMessage(t, f.source, models.MessageTypeImage, "", "images/missing.jpg")

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
		{"media missing from storage", f.target.ID, map[string]any{"forward_of_message_id": missingMedia.ID.String()}, fasthttp.StatusConflict, "forward_media_unavailable"},
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
