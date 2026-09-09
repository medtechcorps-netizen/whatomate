package handlers_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/handlers"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/internal/templateutil"
	"github.com/shridarpatil/whatomate/pkg/whatsapp"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func synchronousNonAISendOptions() handlers.MessageSendOptions {
	options := handlers.ChatbotSendOptions()
	options.AutomaticAI = false
	return options
}

// mockWhatsAppServer creates a mock WhatsApp API server for testing.
// It handles various endpoints and returns configurable responses.
type mockWhatsAppServer struct {
	mu            sync.Mutex
	server        *httptest.Server
	sentMessages  []map[string]any
	sentPaths     []string
	sentAuth      []string
	uploadedMedia []map[string]any
	returnError   bool
	errorMessage  string
	nextMessageID string
	nextMediaID   string
}

func newMockWhatsAppServer() *mockWhatsAppServer {
	m := &mockWhatsAppServer{
		sentMessages:  make([]map[string]any, 0),
		uploadedMedia: make([]map[string]any, 0),
		nextMessageID: "wamid.test-" + uuid.New().String()[:8],
		nextMediaID:   "media-" + uuid.New().String()[:8],
	}

	m.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/v18.0/") && strings.HasSuffix(r.URL.Path, "/messages") {
			m.mu.Lock()
			m.sentPaths = append(m.sentPaths, r.URL.Path)
			m.sentAuth = append(m.sentAuth, r.Header.Get("Authorization"))
			m.mu.Unlock()
		}
		// Check authorization
		auth := r.Header.Get("Authorization")
		if auth != "Bearer test-token" {
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error": map[string]any{
					"message": "Invalid access token",
					"code":    190,
				},
			})
			return
		}

		// Handle different endpoints
		switch {
		case strings.HasPrefix(r.URL.Path, "/v18.0/") && strings.HasSuffix(r.URL.Path, "/messages") && r.Method == http.MethodPost:
			m.handleMessages(w, r)
		case strings.HasPrefix(r.URL.Path, "/v18.0/") && strings.HasSuffix(r.URL.Path, "/media") && r.Method == http.MethodPost:
			m.handleMediaUpload(w, r)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))

	return m
}

func (m *mockWhatsAppServer) handleMessages(w http.ResponseWriter, r *http.Request) {
	if m.returnError {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]any{
				"message": m.errorMessage,
				"code":    100,
			},
		})
		return
	}

	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	m.mu.Lock()
	m.sentMessages = append(m.sentMessages, body)
	m.mu.Unlock()

	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"messages": []map[string]string{{"id": m.nextMessageID}},
	})
}

func (m *mockWhatsAppServer) sentMessageCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.sentMessages)
}

func (m *mockWhatsAppServer) messageRequestSnapshot() ([]string, []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.sentPaths...), append([]string(nil), m.sentAuth...)
}

func (m *mockWhatsAppServer) handleMediaUpload(w http.ResponseWriter, r *http.Request) {
	if m.returnError {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	m.uploadedMedia = append(m.uploadedMedia, map[string]any{
		"content_type": r.Header.Get("Content-Type"),
	})

	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"id": m.nextMediaID,
	})
}

func (m *mockWhatsAppServer) close() {
	m.server.Close()
}

// testServerTransport redirects all requests to the test server
type testServerTransport struct {
	serverURL string
}

func (t *testServerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	testReq := req.Clone(req.Context())
	testReq.URL.Scheme = "http"
	testReq.URL.Host = t.serverURL[7:] // Remove "http://"
	return http.DefaultTransport.RoundTrip(testReq)
}

// newMsgTestApp creates an App instance for message testing with a mock WhatsApp server.
func newMsgTestApp(t *testing.T, mockServer *mockWhatsAppServer) *handlers.App {
	t.Helper()

	log := testutil.NopLogger()
	waClient := whatsapp.NewWithTimeout(log, 5*time.Second)
	waClient.HTTPClient = &http.Client{
		Transport: &testServerTransport{serverURL: mockServer.server.URL},
	}

	return newTestApp(t, withWhatsApp(waClient))
}

var messageTestPhoneSequence atomic.Uint64

// createTestAccount creates a test WhatsApp account in the database.
func createTestAccount(t *testing.T, app *handlers.App, orgID uuid.UUID) *models.WhatsAppAccount {
	t.Helper()
	phoneID := strconv.FormatUint(9_000_000_000_000_000_000+messageTestPhoneSequence.Add(1), 10)

	account := &models.WhatsAppAccount{
		BaseModel:          models.BaseModel{ID: uuid.New()},
		OrganizationID:     orgID,
		Name:               "test-account-" + uuid.New().String()[:8],
		PhoneID:            phoneID,
		BusinessID:         "8000000000000000000",
		AccessToken:        "test-token",
		WebhookVerifyToken: "webhook-token",
		APIVersion:         "v18.0",
		Status:             "active",
	}
	require.NoError(t, app.DB.Create(account).Error)
	return account
}

// --- SendOutgoingMessage Tests ---

func TestApp_SendOutgoingMessage_TextMessage_Success(t *testing.T) {
	mockServer := newMockWhatsAppServer()
	defer mockServer.close()

	app := newMsgTestApp(t, mockServer)
	org := testutil.CreateTestOrganization(t, app.DB)
	account := createTestAccount(t, app, org.ID)
	contact := testutil.CreateTestContactWith(t, app.DB, org.ID, testutil.WithContactAccount(account.Name))

	ctx := testutil.TestContext(t)

	req := handlers.OutgoingMessageRequest{
		Account: account,
		Contact: contact,
		Type:    models.MessageTypeText,
		Content: "Hello, World!",
	}

	// Use sync options to wait for result
	opts := synchronousNonAISendOptions()

	msg, err := app.SendOutgoingMessage(ctx, req, opts)

	require.NoError(t, err)
	require.NotNil(t, msg)

	// Verify message was saved to database
	assert.Equal(t, models.MessageTypeText, msg.MessageType)
	assert.Equal(t, "Hello, World!", msg.Content)
	assert.Equal(t, models.DirectionOutgoing, msg.Direction)
	assert.Equal(t, contact.ID, msg.ContactID)
	assert.Equal(t, org.ID, msg.OrganizationID)

	// Verify message was sent to WhatsApp API
	require.Len(t, mockServer.sentMessages, 1)
	sentMsg := mockServer.sentMessages[0]
	assert.Equal(t, "text", sentMsg["type"])
	assert.Equal(t, contact.PhoneNumber, sentMsg["to"])

	textContent := sentMsg["text"].(map[string]any)
	assert.Equal(t, "Hello, World!", textContent["body"])

	// Verify message status was updated in DB
	var dbMsg models.Message
	require.NoError(t, app.DB.First(&dbMsg, msg.ID).Error)
	assert.Equal(t, models.MessageStatusSent, dbMsg.Status)
	assert.Equal(t, mockServer.nextMessageID, dbMsg.WhatsAppMessageID)
}

func TestApp_SendOutgoingMessage_DisconnectedAccountDoesNotCallGraph(t *testing.T) {
	mockServer := newMockWhatsAppServer()
	defer mockServer.close()

	app := newMsgTestApp(t, mockServer)
	org := testutil.CreateTestOrganization(t, app.DB)
	account := createTestAccount(t, app, org.ID)
	contact := testutil.CreateTestContactWith(t, app.DB, org.ID, testutil.WithContactAccount(account.Name))

	// Keep the caller's projection active to exercise the final database fence,
	// as happens when an offboarding webhook invalidates an already-loaded job.
	require.NoError(t, app.DB.Model(&models.WhatsAppAccount{}).
		Where("id = ? AND organization_id = ?", account.ID, org.ID).
		Update("status", "disconnected").Error)
	require.Equal(t, "active", account.Status)

	msg, err := app.SendOutgoingMessage(
		testutil.TestContext(t),
		handlers.OutgoingMessageRequest{
			Account: account,
			Contact: contact,
			Type:    models.MessageTypeText,
			Content: "must stay local after coexistence reconnect",
		},
		synchronousNonAISendOptions(),
	)
	require.ErrorContains(t, err, "not active for outbound messaging")
	assert.Nil(t, msg)

	paths, _ := mockServer.messageRequestSnapshot()
	assert.Empty(t, paths, "a disconnected account must make zero Graph message requests")

	var stored models.Message
	require.NoError(t, app.DB.Where(
		"organization_id = ? AND content = ?",
		org.ID,
		"must stay local after coexistence reconnect",
	).First(&stored).Error)
	assert.Equal(t, models.MessageStatusFailed, stored.Status)
	assert.Contains(t, stored.ErrorMessage, "not active for outbound messaging")
}

func TestApp_SendOutgoingMessage_UsesCredentialFromFinalLockedRow(t *testing.T) {
	mockServer := newMockWhatsAppServer()
	defer mockServer.close()

	app := newMsgTestApp(t, mockServer)
	org := testutil.CreateTestOrganization(t, app.DB)
	account := createTestAccount(t, app, org.ID)
	contact := testutil.CreateTestContactWith(t, app.DB, org.ID, testutil.WithContactAccount(account.Name))

	// Simulate a request/cache projection retained across a completed Embedded
	// Signup credential refresh. The database row contains the current token.
	account.AccessToken = "superseded-token"
	msg, err := app.SendOutgoingMessage(
		testutil.TestContext(t),
		handlers.OutgoingMessageRequest{
			Account: account,
			Contact: contact,
			Type:    models.MessageTypeText,
			Content: "must use the locked credential generation",
		},
		synchronousNonAISendOptions(),
	)
	require.NoError(t, err)
	require.NotNil(t, msg)

	paths, authorizations := mockServer.messageRequestSnapshot()
	require.Len(t, paths, 1)
	require.Equal(t, []string{"Bearer test-token"}, authorizations)
}

func TestApp_SendOutgoingMessage_TextMessage_APIError(t *testing.T) {
	mockServer := newMockWhatsAppServer()
	defer mockServer.close()

	mockServer.returnError = true
	mockServer.errorMessage = "Phone number is invalid"

	app := newMsgTestApp(t, mockServer)
	org := testutil.CreateTestOrganization(t, app.DB)
	account := createTestAccount(t, app, org.ID)
	contact := testutil.CreateTestContactWith(t, app.DB, org.ID, testutil.WithContactAccount(account.Name))

	ctx := testutil.TestContext(t)

	req := handlers.OutgoingMessageRequest{
		Account: account,
		Contact: contact,
		Type:    models.MessageTypeText,
		Content: "Hello!",
	}

	opts := synchronousNonAISendOptions()

	msg, err := app.SendOutgoingMessage(ctx, req, opts)

	// Message is still returned (saved to DB) even if send fails
	require.NoError(t, err)
	require.NotNil(t, msg)

	// Verify message status is failed in DB
	var dbMsg models.Message
	require.NoError(t, app.DB.First(&dbMsg, msg.ID).Error)
	assert.Equal(t, models.MessageStatusFailed, dbMsg.Status)
	assert.Contains(t, dbMsg.ErrorMessage, "Phone number is invalid")
}

func TestApp_SendOutgoingMessage_ImageMessage_WithMediaID(t *testing.T) {
	mockServer := newMockWhatsAppServer()
	defer mockServer.close()

	app := newMsgTestApp(t, mockServer)
	org := testutil.CreateTestOrganization(t, app.DB)
	account := createTestAccount(t, app, org.ID)
	contact := testutil.CreateTestContactWith(t, app.DB, org.ID, testutil.WithContactAccount(account.Name))

	ctx := testutil.TestContext(t)

	req := handlers.OutgoingMessageRequest{
		Account:       account,
		Contact:       contact,
		Type:          models.MessageTypeImage,
		MediaID:       "existing-media-id",
		MediaMimeType: "image/jpeg",
		Caption:       "Check this out!",
	}

	opts := synchronousNonAISendOptions()

	msg, err := app.SendOutgoingMessage(ctx, req, opts)

	require.NoError(t, err)
	require.NotNil(t, msg)

	// Verify image message was sent
	require.Len(t, mockServer.sentMessages, 1)
	sentMsg := mockServer.sentMessages[0]
	assert.Equal(t, "image", sentMsg["type"])

	imageContent := sentMsg["image"].(map[string]any)
	assert.Equal(t, "existing-media-id", imageContent["id"])
	assert.Equal(t, "Check this out!", imageContent["caption"])

	// No media upload should have occurred
	assert.Len(t, mockServer.uploadedMedia, 0)
}

func TestApp_SendOutgoingMessage_ImageMessage_WithMediaData(t *testing.T) {
	mockServer := newMockWhatsAppServer()
	defer mockServer.close()

	app := newMsgTestApp(t, mockServer)
	org := testutil.CreateTestOrganization(t, app.DB)
	account := createTestAccount(t, app, org.ID)
	contact := testutil.CreateTestContactWith(t, app.DB, org.ID, testutil.WithContactAccount(account.Name))

	ctx := testutil.TestContext(t)

	req := handlers.OutgoingMessageRequest{
		Account:       account,
		Contact:       contact,
		Type:          models.MessageTypeImage,
		MediaData:     []byte("fake image data"),
		MediaMimeType: "image/jpeg",
		MediaFilename: "photo.jpg",
		Caption:       "Photo caption",
	}

	opts := synchronousNonAISendOptions()

	msg, err := app.SendOutgoingMessage(ctx, req, opts)

	require.NoError(t, err)
	require.NotNil(t, msg)

	// Verify media was uploaded
	require.Len(t, mockServer.uploadedMedia, 1)

	// Verify image message was sent with uploaded media ID
	require.Len(t, mockServer.sentMessages, 1)
	sentMsg := mockServer.sentMessages[0]
	assert.Equal(t, "image", sentMsg["type"])

	imageContent := sentMsg["image"].(map[string]any)
	assert.Equal(t, mockServer.nextMediaID, imageContent["id"])
}

func TestApp_SendOutgoingMessage_DocumentMessage(t *testing.T) {
	mockServer := newMockWhatsAppServer()
	defer mockServer.close()

	app := newMsgTestApp(t, mockServer)
	org := testutil.CreateTestOrganization(t, app.DB)
	account := createTestAccount(t, app, org.ID)
	contact := testutil.CreateTestContactWith(t, app.DB, org.ID, testutil.WithContactAccount(account.Name))

	ctx := testutil.TestContext(t)

	req := handlers.OutgoingMessageRequest{
		Account:       account,
		Contact:       contact,
		Type:          models.MessageTypeDocument,
		MediaID:       "doc-media-id",
		MediaMimeType: "application/pdf",
		MediaFilename: "report.pdf",
		Caption:       "Monthly report",
	}

	opts := synchronousNonAISendOptions()

	msg, err := app.SendOutgoingMessage(ctx, req, opts)

	require.NoError(t, err)
	require.NotNil(t, msg)

	// Verify document message was sent
	require.Len(t, mockServer.sentMessages, 1)
	sentMsg := mockServer.sentMessages[0]
	assert.Equal(t, "document", sentMsg["type"])

	docContent := sentMsg["document"].(map[string]any)
	assert.Equal(t, "doc-media-id", docContent["id"])
	assert.Equal(t, "report.pdf", docContent["filename"])
	assert.Equal(t, "Monthly report", docContent["caption"])
}

func TestApp_SendOutgoingMessage_VideoMessage(t *testing.T) {
	mockServer := newMockWhatsAppServer()
	defer mockServer.close()

	app := newMsgTestApp(t, mockServer)
	org := testutil.CreateTestOrganization(t, app.DB)
	account := createTestAccount(t, app, org.ID)
	contact := testutil.CreateTestContactWith(t, app.DB, org.ID, testutil.WithContactAccount(account.Name))

	ctx := testutil.TestContext(t)

	req := handlers.OutgoingMessageRequest{
		Account:       account,
		Contact:       contact,
		Type:          models.MessageTypeVideo,
		MediaID:       "video-media-id",
		MediaMimeType: "video/mp4",
		Caption:       "Watch this!",
	}

	opts := synchronousNonAISendOptions()

	msg, err := app.SendOutgoingMessage(ctx, req, opts)

	require.NoError(t, err)
	require.NotNil(t, msg)

	require.Len(t, mockServer.sentMessages, 1)
	sentMsg := mockServer.sentMessages[0]
	assert.Equal(t, "video", sentMsg["type"])

	videoContent := sentMsg["video"].(map[string]any)
	assert.Equal(t, "video-media-id", videoContent["id"])
	assert.Equal(t, "Watch this!", videoContent["caption"])
}

func TestApp_SendOutgoingMessage_AudioMessage(t *testing.T) {
	mockServer := newMockWhatsAppServer()
	defer mockServer.close()

	app := newMsgTestApp(t, mockServer)
	org := testutil.CreateTestOrganization(t, app.DB)
	account := createTestAccount(t, app, org.ID)
	contact := testutil.CreateTestContactWith(t, app.DB, org.ID, testutil.WithContactAccount(account.Name))

	ctx := testutil.TestContext(t)

	req := handlers.OutgoingMessageRequest{
		Account:       account,
		Contact:       contact,
		Type:          models.MessageTypeAudio,
		MediaID:       "audio-media-id",
		MediaMimeType: "audio/ogg",
	}

	opts := synchronousNonAISendOptions()

	msg, err := app.SendOutgoingMessage(ctx, req, opts)

	require.NoError(t, err)
	require.NotNil(t, msg)

	require.Len(t, mockServer.sentMessages, 1)
	sentMsg := mockServer.sentMessages[0]
	assert.Equal(t, "audio", sentMsg["type"])

	audioContent := sentMsg["audio"].(map[string]any)
	assert.Equal(t, "audio-media-id", audioContent["id"])
}

func TestApp_SendOutgoingMessage_InteractiveButtons(t *testing.T) {
	mockServer := newMockWhatsAppServer()
	defer mockServer.close()

	app := newMsgTestApp(t, mockServer)
	org := testutil.CreateTestOrganization(t, app.DB)
	account := createTestAccount(t, app, org.ID)
	contact := testutil.CreateTestContactWith(t, app.DB, org.ID, testutil.WithContactAccount(account.Name))

	ctx := testutil.TestContext(t)

	req := handlers.OutgoingMessageRequest{
		Account:         account,
		Contact:         contact,
		Type:            models.MessageTypeInteractive,
		InteractiveType: "button",
		BodyText:        "Choose an option:",
		Buttons: []whatsapp.Button{
			{ID: "btn_yes", Title: "Yes"},
			{ID: "btn_no", Title: "No"},
		},
	}

	opts := synchronousNonAISendOptions()

	msg, err := app.SendOutgoingMessage(ctx, req, opts)

	require.NoError(t, err)
	require.NotNil(t, msg)

	// Verify interactive message was saved
	assert.Equal(t, models.MessageTypeInteractive, msg.MessageType)
	assert.Equal(t, "Choose an option:", msg.Content)

	// Verify interactive message was sent
	require.Len(t, mockServer.sentMessages, 1)
	sentMsg := mockServer.sentMessages[0]
	assert.Equal(t, "interactive", sentMsg["type"])

	interactive := sentMsg["interactive"].(map[string]any)
	assert.Equal(t, "button", interactive["type"])

	body := interactive["body"].(map[string]any)
	assert.Equal(t, "Choose an option:", body["text"])

	action := interactive["action"].(map[string]any)
	buttons := action["buttons"].([]any)
	assert.Len(t, buttons, 2)
}

func TestApp_SendOutgoingMessage_InteractiveCTAURL(t *testing.T) {
	mockServer := newMockWhatsAppServer()
	defer mockServer.close()

	app := newMsgTestApp(t, mockServer)
	org := testutil.CreateTestOrganization(t, app.DB)
	account := createTestAccount(t, app, org.ID)
	contact := testutil.CreateTestContactWith(t, app.DB, org.ID, testutil.WithContactAccount(account.Name))

	ctx := testutil.TestContext(t)

	req := handlers.OutgoingMessageRequest{
		Account:         account,
		Contact:         contact,
		Type:            models.MessageTypeInteractive,
		InteractiveType: "cta_url",
		BodyText:        "Visit our website",
		ButtonText:      "Visit Now",
		URL:             "https://example.com",
	}

	opts := synchronousNonAISendOptions()

	msg, err := app.SendOutgoingMessage(ctx, req, opts)

	require.NoError(t, err)
	require.NotNil(t, msg)

	// Verify message content and interactive data
	assert.Equal(t, "Visit our website", msg.Content)
	assert.NotNil(t, msg.InteractiveData)
	assert.Equal(t, "cta_url", msg.InteractiveData["type"])

	// Verify CTA URL message was sent
	require.Len(t, mockServer.sentMessages, 1)
	sentMsg := mockServer.sentMessages[0]
	assert.Equal(t, "interactive", sentMsg["type"])

	interactive := sentMsg["interactive"].(map[string]any)
	assert.Equal(t, "cta_url", interactive["type"])
}

func TestApp_SendOutgoingMessage_TemplateMessage(t *testing.T) {
	mockServer := newMockWhatsAppServer()
	defer mockServer.close()

	app := newMsgTestApp(t, mockServer)
	org := testutil.CreateTestOrganization(t, app.DB)
	account := createTestAccount(t, app, org.ID)
	contact := testutil.CreateTestContactWith(t, app.DB, org.ID, testutil.WithContactAccount(account.Name))

	// Create a test template
	template := &models.Template{
		BaseModel:       models.BaseModel{ID: uuid.New()},
		OrganizationID:  org.ID,
		WhatsAppAccount: account.Name,
		Name:            "hello_world",
		DisplayName:     "Hello World Template",
		MetaTemplateID:  "meta-123",
		Category:        "MARKETING",
		Language:        "en",
		Status:          string(models.TemplateStatusApproved),
		BodyContent:     "Hello {{1}}! Your order {{2}} is ready.",
	}
	require.NoError(t, app.DB.Create(template).Error)

	ctx := testutil.TestContext(t)

	req := handlers.OutgoingMessageRequest{
		Account:    account,
		Contact:    contact,
		Type:       models.MessageTypeTemplate,
		Template:   template,
		BodyParams: map[string]string{"1": "John", "2": "ORD-123"},
	}

	opts := synchronousNonAISendOptions()

	msg, err := app.SendOutgoingMessage(ctx, req, opts)

	require.NoError(t, err)
	require.NotNil(t, msg)

	// Verify template message was saved with rendered content
	assert.Equal(t, models.MessageTypeTemplate, msg.MessageType)
	assert.Equal(t, "Hello John! Your order ORD-123 is ready.", msg.Content)

	// Verify template metadata
	assert.NotNil(t, msg.Metadata)
	assert.Equal(t, "hello_world", msg.Metadata["template_name"])

	// Verify template message was sent
	require.Len(t, mockServer.sentMessages, 1)
	sentMsg := mockServer.sentMessages[0]
	assert.Equal(t, "template", sentMsg["type"])

	templateData := sentMsg["template"].(map[string]any)
	assert.Equal(t, "hello_world", templateData["name"])
	assert.Equal(t, "en", templateData["language"].(map[string]any)["code"])
}

func TestApp_SendOutgoingMessage_TemplateMessage_UploadsRawHeaderInsideDelivery(t *testing.T) {
	mockServer := newMockWhatsAppServer()
	defer mockServer.close()

	app := newMsgTestApp(t, mockServer)
	org := testutil.CreateTestOrganization(t, app.DB)
	account := createTestAccount(t, app, org.ID)
	contact := testutil.CreateTestContactWith(
		t,
		app.DB,
		org.ID,
		testutil.WithContactAccount(account.Name),
	)
	template := &models.Template{
		BaseModel:       models.BaseModel{ID: uuid.New()},
		OrganizationID:  org.ID,
		WhatsAppAccount: account.Name,
		Name:            "raw_header_" + uuid.NewString()[:8],
		DisplayName:     "Raw Header",
		Category:        "UTILITY",
		Language:        "en",
		Status:          string(models.TemplateStatusApproved),
		HeaderType:      "IMAGE",
		BodyContent:     "Header delivery",
	}
	require.NoError(t, app.DB.Create(template).Error)

	message, err := app.SendOutgoingMessage(
		testutil.TestContext(t),
		handlers.OutgoingMessageRequest{
			Account:             account,
			Contact:             contact,
			Type:                models.MessageTypeTemplate,
			Template:            template,
			MediaData:           []byte("synthetic header image"),
			MediaMimeType:       "image/png",
			HeaderMediaFilename: "header.png",
		},
		synchronousNonAISendOptions(),
	)
	require.NoError(t, err)
	require.NotNil(t, message)
	require.Len(t, mockServer.uploadedMedia, 1)
	require.Len(t, mockServer.sentMessages, 1)

	templatePayload := mockServer.sentMessages[0]["template"].(map[string]any)
	components := templatePayload["components"].([]any)
	require.Len(t, components, 1)
	header := components[0].(map[string]any)
	assert.Equal(t, "header", header["type"])
	parameters := header["parameters"].([]any)
	require.Len(t, parameters, 1)
	image := parameters[0].(map[string]any)["image"].(map[string]any)
	assert.Equal(t, mockServer.nextMediaID, image["id"])

	var stored models.Message
	require.NoError(t, app.DB.First(&stored, "id = ?", message.ID).Error)
	assert.Equal(t, models.MessageStatusSent, stored.Status)
}

func TestApp_SendTemplateMessage_MarketingOptOutMakesNoHeaderUpload(t *testing.T) {
	mockServer := newMockWhatsAppServer()
	defer mockServer.close()
	var mediaFetches atomic.Int64
	mediaServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mediaFetches.Add(1)
		w.Header().Set("Content-Type", "image/png")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("synthetic header image"))
	}))
	defer mediaServer.Close()

	app := newMsgTestApp(t, mockServer)
	app.HTTPClient = testutil.NewHTTPSRewriteClient(t, map[string]*httptest.Server{
		"https://media.example.com": mediaServer,
	})
	org := testutil.CreateTestOrganization(t, app.DB)
	adminRole := testutil.CreateAdminRole(t, app.DB, org.ID)
	user := testutil.CreateTestUser(t, app.DB, org.ID, testutil.WithRoleID(&adminRole.ID))
	account := createTestAccount(t, app, org.ID)
	contact := testutil.CreateTestContactWith(
		t,
		app.DB,
		org.ID,
		testutil.WithContactAccount(account.Name),
	)
	require.NoError(t, app.DB.Model(contact).Update("marketing_opt_out", true).Error)
	template := &models.Template{
		BaseModel:       models.BaseModel{ID: uuid.New()},
		OrganizationID:  org.ID,
		WhatsAppAccount: account.Name,
		Name:            "opted_out_header_" + uuid.NewString()[:8],
		DisplayName:     "Opted Out Header",
		Category:        "MARKETING",
		Language:        "en",
		Status:          string(models.TemplateStatusApproved),
		HeaderType:      "IMAGE",
		BodyContent:     "Must not upload",
	}
	require.NoError(t, app.DB.Create(template).Error)

	req := testutil.NewJSONRequest(t, map[string]any{
		"contact_id":       contact.ID.String(),
		"template_name":    template.Name,
		"header_media_url": "https://media.example.com/header.png",
	})
	testutil.SetAuthContext(req, org.ID, user.ID)

	require.NoError(t, app.SendTemplateMessage(req))
	assert.Equal(t, http.StatusBadRequest, testutil.GetResponseStatusCode(req))
	app.WaitForBackgroundTasks()
	assert.EqualValues(t, 1, mediaFetches.Load(), "the bounded source read may finish before policy evaluation")
	assert.Empty(t, mockServer.uploadedMedia, "policy rejection must happen before any Meta upload")
	assert.Equal(t, 0, mockServer.sentMessageCount())
}

func TestApp_SendOutgoingMessage_TemplateMessage_MissingTemplate(t *testing.T) {
	mockServer := newMockWhatsAppServer()
	defer mockServer.close()

	app := newMsgTestApp(t, mockServer)
	org := testutil.CreateTestOrganization(t, app.DB)
	account := createTestAccount(t, app, org.ID)
	contact := testutil.CreateTestContactWith(t, app.DB, org.ID, testutil.WithContactAccount(account.Name))

	ctx := testutil.TestContext(t)

	req := handlers.OutgoingMessageRequest{
		Account:    account,
		Contact:    contact,
		Type:       models.MessageTypeTemplate,
		Template:   nil, // Missing template
		BodyParams: map[string]string{"1": "param1"},
	}

	opts := synchronousNonAISendOptions()

	msg, err := app.SendOutgoingMessage(ctx, req, opts)

	// Message is created but send fails
	require.NoError(t, err)
	require.NotNil(t, msg)

	// Verify message status is failed
	var dbMsg models.Message
	require.NoError(t, app.DB.First(&dbMsg, msg.ID).Error)
	assert.Equal(t, models.MessageStatusFailed, dbMsg.Status)
	assert.Contains(t, dbMsg.ErrorMessage, "template is required")
}

func TestApp_SendOutgoingMessage_AsyncOption(t *testing.T) {
	mockServer := newMockWhatsAppServer()
	defer mockServer.close()

	app := newMsgTestApp(t, mockServer)
	org := testutil.CreateTestOrganization(t, app.DB)
	account := createTestAccount(t, app, org.ID)
	contact := testutil.CreateTestContactWith(t, app.DB, org.ID, testutil.WithContactAccount(account.Name))

	ctx := testutil.TestContext(t)

	req := handlers.OutgoingMessageRequest{
		Account: account,
		Contact: contact,
		Type:    models.MessageTypeText,
		Content: "Async message",
	}

	// Use async options
	opts := handlers.DefaultSendOptions()
	assert.True(t, opts.Async)

	msg, err := app.SendOutgoingMessage(ctx, req, opts)

	require.NoError(t, err)
	require.NotNil(t, msg)

	// Wait for async send to complete
	app.WaitForBackgroundTasks()

	// Now verify message was sent and status updated in DB
	var dbMsg models.Message
	require.NoError(t, app.DB.First(&dbMsg, msg.ID).Error)
	assert.Equal(t, models.MessageStatusSent, dbMsg.Status)
	assert.NotEmpty(t, dbMsg.WhatsAppMessageID)
}

func TestApp_SendOutgoingMessage_SyncOption(t *testing.T) {
	mockServer := newMockWhatsAppServer()
	defer mockServer.close()

	app := newMsgTestApp(t, mockServer)
	org := testutil.CreateTestOrganization(t, app.DB)
	account := createTestAccount(t, app, org.ID)
	contact := testutil.CreateTestContactWith(t, app.DB, org.ID, testutil.WithContactAccount(account.Name))

	ctx := testutil.TestContext(t)

	req := handlers.OutgoingMessageRequest{
		Account: account,
		Contact: contact,
		Type:    models.MessageTypeText,
		Content: "Sync message",
	}

	// Use synchronous non-AI options for this generic delivery test.
	opts := synchronousNonAISendOptions()
	assert.False(t, opts.Async)

	msg, err := app.SendOutgoingMessage(ctx, req, opts)

	require.NoError(t, err)
	require.NotNil(t, msg)

	// Message status should be updated immediately (sync)
	var dbMsg models.Message
	require.NoError(t, app.DB.First(&dbMsg, msg.ID).Error)
	assert.Equal(t, models.MessageStatusSent, dbMsg.Status)
}

func TestApp_SendOutgoingMessage_WithSentByUser(t *testing.T) {
	mockServer := newMockWhatsAppServer()
	defer mockServer.close()

	app := newMsgTestApp(t, mockServer)
	org := testutil.CreateTestOrganization(t, app.DB)
	account := createTestAccount(t, app, org.ID)
	contact := testutil.CreateTestContactWith(t, app.DB, org.ID, testutil.WithContactAccount(account.Name))

	// Create a test user (required due to foreign key constraint)
	user := &models.User{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: org.ID,
		Email:          "agent-" + uuid.New().String()[:8] + "@test.com",
		FullName:       "Test Agent",
		IsActive:       true,
	}
	require.NoError(t, app.DB.Create(user).Error)
	userID := user.ID
	require.NoError(t, app.DB.Model(contact).Update("assigned_user_id", userID).Error)

	ctx := testutil.TestContext(t)

	req := handlers.OutgoingMessageRequest{
		Account: account,
		Contact: contact,
		Type:    models.MessageTypeText,
		Content: "Message from agent",
	}

	opts := handlers.DefaultSendOptions()
	opts.SentByUserID = &userID

	msg, err := app.SendOutgoingMessage(ctx, req, opts)

	require.NoError(t, err)
	require.NotNil(t, msg)

	// Wait for async send
	app.WaitForBackgroundTasks()

	// Verify sent by user is recorded
	var dbMsg models.Message
	require.NoError(t, app.DB.First(&dbMsg, msg.ID).Error)
	require.NotNil(t, dbMsg.SentByUserID)
	assert.Equal(t, userID, *dbMsg.SentByUserID)
}

func TestApp_SendOutgoingMessage_UnsupportedType(t *testing.T) {
	mockServer := newMockWhatsAppServer()
	defer mockServer.close()

	app := newMsgTestApp(t, mockServer)
	org := testutil.CreateTestOrganization(t, app.DB)
	account := createTestAccount(t, app, org.ID)
	contact := testutil.CreateTestContactWith(t, app.DB, org.ID, testutil.WithContactAccount(account.Name))

	ctx := testutil.TestContext(t)

	req := handlers.OutgoingMessageRequest{
		Account: account,
		Contact: contact,
		Type:    "unknown_type",
		Content: "Some content",
	}

	opts := synchronousNonAISendOptions()

	msg, err := app.SendOutgoingMessage(ctx, req, opts)

	require.NoError(t, err)
	require.NotNil(t, msg)

	// Verify message status is failed due to unsupported type
	var dbMsg models.Message
	require.NoError(t, app.DB.First(&dbMsg, msg.ID).Error)
	assert.Equal(t, models.MessageStatusFailed, dbMsg.Status)
	assert.Contains(t, dbMsg.ErrorMessage, "unsupported message type")
}

// --- Options Preset Tests ---

func TestDefaultSendOptions(t *testing.T) {
	opts := handlers.DefaultSendOptions()

	assert.True(t, opts.BroadcastWebSocket)
	assert.True(t, opts.DispatchWebhook)
	assert.False(t, opts.TrackSLA)
	assert.True(t, opts.Async)
	assert.Nil(t, opts.SentByUserID)
}

func TestChatbotSendOptions(t *testing.T) {
	opts := handlers.ChatbotSendOptions()

	assert.True(t, opts.BroadcastWebSocket)
	assert.False(t, opts.DispatchWebhook)
	assert.True(t, opts.TrackSLA)
	assert.False(t, opts.Async)
	assert.Nil(t, opts.SentByUserID)
}

func TestAPISendOptions(t *testing.T) {
	opts := handlers.APISendOptions()

	assert.False(t, opts.BroadcastWebSocket)
	assert.True(t, opts.DispatchWebhook)
	assert.False(t, opts.TrackSLA)
	assert.True(t, opts.Async)
	assert.Nil(t, opts.SentByUserID)
}

func TestSLASendOptions(t *testing.T) {
	opts := handlers.SLASendOptions()

	assert.True(t, opts.BroadcastWebSocket)
	assert.False(t, opts.DispatchWebhook)
	assert.False(t, opts.TrackSLA)
	assert.False(t, opts.Async)
	assert.Nil(t, opts.SentByUserID)
}

// --- Message Preview Tests ---

func TestApp_SendOutgoingMessage_ContactLastMessageUpdated(t *testing.T) {
	mockServer := newMockWhatsAppServer()
	defer mockServer.close()

	app := newMsgTestApp(t, mockServer)
	org := testutil.CreateTestOrganization(t, app.DB)
	account := createTestAccount(t, app, org.ID)
	contact := testutil.CreateTestContactWith(t, app.DB, org.ID, testutil.WithContactAccount(account.Name))

	ctx := testutil.TestContext(t)

	req := handlers.OutgoingMessageRequest{
		Account: account,
		Contact: contact,
		Type:    models.MessageTypeText,
		Content: "This is a test message for preview",
	}

	opts := synchronousNonAISendOptions()

	_, err := app.SendOutgoingMessage(ctx, req, opts)
	require.NoError(t, err)

	// Verify contact's last message was updated
	var updatedContact models.Contact
	require.NoError(t, app.DB.First(&updatedContact, contact.ID).Error)
	assert.NotNil(t, updatedContact.LastMessageAt)
	assert.Equal(t, "This is a test message for preview", updatedContact.LastMessagePreview)
}

func TestApp_SendOutgoingMessage_MediaPreview(t *testing.T) {
	mockServer := newMockWhatsAppServer()
	defer mockServer.close()

	app := newMsgTestApp(t, mockServer)
	org := testutil.CreateTestOrganization(t, app.DB)
	account := createTestAccount(t, app, org.ID)
	contact := testutil.CreateTestContactWith(t, app.DB, org.ID, testutil.WithContactAccount(account.Name))

	ctx := testutil.TestContext(t)

	// Test image without caption
	req := handlers.OutgoingMessageRequest{
		Account:       account,
		Contact:       contact,
		Type:          models.MessageTypeImage,
		MediaID:       "media-123",
		MediaMimeType: "image/jpeg",
	}

	opts := synchronousNonAISendOptions()

	_, err := app.SendOutgoingMessage(ctx, req, opts)
	require.NoError(t, err)

	var updatedContact models.Contact
	require.NoError(t, app.DB.First(&updatedContact, contact.ID).Error)
	assert.Equal(t, "[Image]", updatedContact.LastMessagePreview)
}

func TestApp_SendOutgoingMessage_DocumentPreview(t *testing.T) {
	mockServer := newMockWhatsAppServer()
	defer mockServer.close()

	app := newMsgTestApp(t, mockServer)
	org := testutil.CreateTestOrganization(t, app.DB)
	account := createTestAccount(t, app, org.ID)
	contact := testutil.CreateTestContactWith(t, app.DB, org.ID, testutil.WithContactAccount(account.Name))

	ctx := testutil.TestContext(t)

	req := handlers.OutgoingMessageRequest{
		Account:       account,
		Contact:       contact,
		Type:          models.MessageTypeDocument,
		MediaID:       "media-123",
		MediaFilename: "report.pdf",
	}

	opts := synchronousNonAISendOptions()

	_, err := app.SendOutgoingMessage(ctx, req, opts)
	require.NoError(t, err)

	var updatedContact models.Contact
	require.NoError(t, app.DB.First(&updatedContact, contact.ID).Error)
	assert.Equal(t, "[Document: report.pdf]", updatedContact.LastMessagePreview)
}

// --- Template Parameter Tests ---

func TestExtractParamNamesFromContent_Positional(t *testing.T) {
	content := "Hello {{1}}! Your order {{2}} is ready for pickup at {{3}}."
	names := templateutil.ExtParamNames(content)

	require.Len(t, names, 3)
	assert.Equal(t, "1", names[0])
	assert.Equal(t, "2", names[1])
	assert.Equal(t, "3", names[2])
}

func TestExtractParamNamesFromContent_Named(t *testing.T) {
	content := "Hi {{customer_name}}, your order {{order_id}} will arrive on {{delivery_date}}."
	names := templateutil.ExtParamNames(content)

	require.Len(t, names, 3)
	assert.Equal(t, "customer_name", names[0])
	assert.Equal(t, "order_id", names[1])
	assert.Equal(t, "delivery_date", names[2])
}

func TestExtractParamNamesFromContent_Mixed(t *testing.T) {
	content := "Hello {{name}}, your code is {{1}}."
	names := templateutil.ExtParamNames(content)

	require.Len(t, names, 2)
	assert.Equal(t, "name", names[0])
	assert.Equal(t, "1", names[1])
}

func TestExtractParamNamesFromContent_NoParams(t *testing.T) {
	content := "This is a static message with no parameters."
	names := templateutil.ExtParamNames(content)

	assert.Nil(t, names)
}

func TestExtractParamNamesFromContent_DuplicateParams(t *testing.T) {
	content := "Hello {{name}}, {{name}} is a great name!"
	names := templateutil.ExtParamNames(content)

	// Should deduplicate
	require.Len(t, names, 1)
	assert.Equal(t, "name", names[0])
}

func TestResolveParams_NamedMatch(t *testing.T) {
	paramNames := []string{"customer_name", "order_id"}
	params := map[string]string{
		"customer_name": "John",
		"order_id":      "ORD-123",
	}

	result := templateutil.ResolveParamsFromMap(paramNames, params)

	require.Len(t, result, 2)
	assert.Equal(t, "John", result[0])
	assert.Equal(t, "ORD-123", result[1])
}

func TestResolveParams_PositionalMatch(t *testing.T) {
	paramNames := []string{"1", "2"}
	params := map[string]string{
		"1": "First",
		"2": "Second",
	}

	result := templateutil.ResolveParamsFromMap(paramNames, params)

	require.Len(t, result, 2)
	assert.Equal(t, "First", result[0])
	assert.Equal(t, "Second", result[1])
}

func TestResolveParams_FallbackToPositional(t *testing.T) {
	// Template has named params, but user sends positional
	paramNames := []string{"name", "code"}
	params := map[string]string{
		"1": "John",
		"2": "ABC123",
	}

	result := templateutil.ResolveParamsFromMap(paramNames, params)

	require.Len(t, result, 2)
	assert.Equal(t, "John", result[0])
	assert.Equal(t, "ABC123", result[1])
}

func TestResolveParams_MissingParams(t *testing.T) {
	paramNames := []string{"name", "order_id", "date"}
	params := map[string]string{
		"name": "John",
		// order_id and date are missing
	}

	result := templateutil.ResolveParamsFromMap(paramNames, params)

	require.Len(t, result, 3)
	assert.Equal(t, "John", result[0])
	assert.Equal(t, "", result[1]) // Missing - defaults to empty
	assert.Equal(t, "", result[2]) // Missing - defaults to empty
}

func TestResolveParams_EmptyInputs(t *testing.T) {
	// Empty param names
	result1 := templateutil.ResolveParamsFromMap([]string{}, map[string]string{"a": "b"})
	assert.Nil(t, result1)

	// Empty params map
	result2 := templateutil.ResolveParamsFromMap([]string{"a"}, map[string]string{})
	assert.Nil(t, result2)

	// Both empty
	result3 := templateutil.ResolveParamsFromMap([]string{}, map[string]string{})
	assert.Nil(t, result3)
}

func TestResolveParams_WrongParamNames(t *testing.T) {
	// User sends different param names than template expects
	paramNames := []string{"1", "2"} // Template expects positional
	params := map[string]string{
		"lastrec":  "Nifty",     // Wrong name
		"misstime": "banknifty", // Wrong name
	}

	result := templateutil.ResolveParamsFromMap(paramNames, params)

	require.Len(t, result, 2)
	// Both should be empty since names don't match
	assert.Equal(t, "", result[0])
	assert.Equal(t, "", result[1])
}
