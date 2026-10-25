package handlers

import (
	"bytes"
	"context"
	"mime/multipart"
	"net/textproto"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/config"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/internal/storage"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
)

type cleanupTestObjectStore struct {
	objects map[string][]byte
	deleted []string
}

func (s *cleanupTestObjectStore) Put(_ context.Context, key string, data []byte, _ string) error {
	if s.objects == nil {
		s.objects = make(map[string][]byte)
	}
	s.objects[key] = append([]byte(nil), data...)
	return nil
}

func (s *cleanupTestObjectStore) Get(_ context.Context, key string) ([]byte, string, error) {
	data, ok := s.objects[key]
	if !ok {
		return nil, "", storage.ErrObjectNotFound
	}
	return append([]byte(nil), data...), "image/jpeg", nil
}

func (s *cleanupTestObjectStore) Delete(_ context.Context, key string) error {
	if _, ok := s.objects[key]; !ok {
		return storage.ErrObjectNotFound
	}
	delete(s.objects, key)
	s.deleted = append(s.deleted, key)
	return nil
}

func (s *cleanupTestObjectStore) ListPrefix(_ context.Context, _ string) ([]storage.ObjectInfo, error) {
	return nil, nil
}

func TestCleanupRejectedMessageMedia(t *testing.T) {
	for _, testCase := range []struct {
		name          string
		messageStatus models.MessageStatus
		deleteMessage bool
		wantPresent   bool
	}{
		{name: "unreferenced rejected upload is removed"},
		{
			name:          "durable failed message keeps its preview",
			messageStatus: models.MessageStatusFailed,
			wantPresent:   true,
		},
		{
			name:          "soft-deleted message keeps its preview",
			messageStatus: models.MessageStatusSent,
			deleteMessage: true,
			wantPresent:   true,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			mediaRoot := t.TempDir()
			app := &App{
				Config: &config.Config{
					Storage: config.StorageConfig{LocalPath: mediaRoot},
				},
				DB:  testutil.SetupTestDB(t),
				Log: testutil.NopLogger(),
			}
			org := testutil.CreateTestOrganization(t, app.DB)
			account := testutil.CreateTestWhatsAppAccount(t, app.DB, org.ID)
			contact := testutil.CreateTestContactWith(
				t,
				app.DB,
				org.ID,
				testutil.WithContactAccount(account.Name),
			)

			mediaPath, err := app.saveMessageMedia(
				context.Background(),
				org.ID,
				[]byte("synthetic-image"),
				"image/jpeg",
				"sample.jpg",
			)
			require.NoError(t, err)
			fullPath := filepath.Join(mediaRoot, filepath.FromSlash(mediaPath))
			require.FileExists(t, fullPath)

			if testCase.messageStatus != "" {
				message := &models.Message{
					BaseModel:       models.BaseModel{ID: uuid.New()},
					OrganizationID:  org.ID,
					WhatsAppAccount: account.Name,
					ContactID:       contact.ID,
					Direction:       models.DirectionOutgoing,
					MessageType:     models.MessageTypeImage,
					MediaURL:        mediaPath,
					Status:          testCase.messageStatus,
				}
				require.NoError(t, app.DB.Create(message).Error)
				if testCase.deleteMessage {
					require.NoError(t, app.DB.Delete(message).Error)
					require.True(t, message.DeletedAt.Valid)
				}
			}

			require.NoError(t, app.cleanupRejectedMessageMedia(org.ID, mediaPath))
			if testCase.wantPresent {
				assert.FileExists(t, fullPath)
			} else {
				assert.NoFileExists(t, fullPath)
			}
		})
	}
}

func TestCleanupRejectedMessageMediaObjectStore(t *testing.T) {
	store := &cleanupTestObjectStore{}
	app := &App{
		Config: &config.Config{
			Storage: config.StorageConfig{Type: "s3"},
		},
		DB:          testutil.SetupTestDB(t),
		Log:         testutil.NopLogger(),
		ObjectStore: store,
	}
	org := testutil.CreateTestOrganization(t, app.DB)

	mediaPath, err := app.saveMessageMedia(
		context.Background(),
		org.ID,
		[]byte("synthetic-object-image"),
		"image/jpeg",
		"sample.jpg",
	)
	require.NoError(t, err)
	require.Contains(t, store.objects, mediaPath)

	require.NoError(t, app.cleanupRejectedMessageMedia(org.ID, mediaPath))
	assert.NotContains(t, store.objects, mediaPath)
	assert.Equal(t, []string{mediaPath}, store.deleted)
}

func TestCleanupRejectedMessageMediaFailsClosed(t *testing.T) {
	mediaRoot := t.TempDir()
	app := &App{
		Config: &config.Config{
			Storage: config.StorageConfig{LocalPath: mediaRoot},
		},
		DB:  testutil.SetupTestDB(t),
		Log: testutil.NopLogger(),
	}
	org := testutil.CreateTestOrganization(t, app.DB)
	mediaPath, err := app.saveMessageMedia(
		context.Background(),
		org.ID,
		[]byte("synthetic-image"),
		"image/jpeg",
		"sample.jpg",
	)
	require.NoError(t, err)
	fullPath := filepath.Join(mediaRoot, filepath.FromSlash(mediaPath))
	require.FileExists(t, fullPath)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.Error(t, app.deleteUnreferencedMessageMedia(ctx, org.ID, mediaPath))
	assert.FileExists(t, fullPath, "an unreadable reference check must not delete media")
}

func TestSendTemplateMessageCleansMediaAfterConsentRejection(t *testing.T) {
	store := &cleanupTestObjectStore{}
	app := &App{
		Config: &config.Config{
			App: config.AppConfig{
				EncryptionKey: "test-encryption-key-for-handlers-longer-than-32-chars",
			},
			Storage: config.StorageConfig{Type: "s3"},
		},
		DB:          testutil.SetupTestDB(t),
		Redis:       testutil.SetupTestRedis(t),
		Log:         testutil.NopLogger(),
		ObjectStore: store,
	}
	org := testutil.CreateTestOrganization(t, app.DB)
	adminRole := testutil.CreateAdminRole(t, app.DB, org.ID)
	user := testutil.CreateTestUser(
		t,
		app.DB,
		org.ID,
		testutil.WithRoleID(&adminRole.ID),
	)
	account := testutil.CreateTestWhatsAppAccount(t, app.DB, org.ID)
	contact := testutil.CreateTestContactWith(
		t,
		app.DB,
		org.ID,
		testutil.WithContactAccount(account.Name),
	)
	contact.MarketingOptOut = true
	require.NoError(t, app.DB.Save(contact).Error)
	template := &models.Template{
		BaseModel:       models.BaseModel{ID: uuid.New()},
		OrganizationID:  org.ID,
		WhatsAppAccount: account.Name,
		MetaTemplateID:  "synthetic-marketing-template",
		Name:            "synthetic_marketing_template",
		DisplayName:     "Synthetic marketing template",
		Language:        "en_US",
		Category:        "MARKETING",
		Status:          "APPROVED",
		HeaderType:      "IMAGE",
		BodyContent:     "Synthetic body",
	}
	require.NoError(t, app.DB.Create(template).Error)

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	require.NoError(t, writer.WriteField("contact_id", contact.ID.String()))
	require.NoError(t, writer.WriteField("template_name", template.Name))
	partHeader := textproto.MIMEHeader{}
	partHeader.Set("Content-Disposition", `form-data; name="header_file"; filename="header.jpg"`)
	partHeader.Set("Content-Type", "image/jpeg")
	part, err := writer.CreatePart(partHeader)
	require.NoError(t, err)
	_, err = part.Write([]byte("synthetic-template-header"))
	require.NoError(t, err)
	require.NoError(t, writer.Close())

	req := testutil.NewRequest(t)
	req.RequestCtx.Request.Header.SetMethod("POST")
	req.RequestCtx.Request.Header.SetContentType(writer.FormDataContentType())
	req.RequestCtx.Request.SetBody(body.Bytes())
	testutil.SetAuthContext(req, org.ID, user.ID)
	require.NoError(t, app.SendTemplateMessage(req))
	assert.Equal(t, fasthttp.StatusBadRequest, testutil.GetResponseStatusCode(req))
	assert.Empty(t, store.objects)
	assert.Len(t, store.deleted, 1)
}

func TestSendMediaMessageCleansMediaAfterInvalidReply(t *testing.T) {
	store := &cleanupTestObjectStore{}
	app := &App{
		Config: &config.Config{
			App: config.AppConfig{
				EncryptionKey: "test-encryption-key-for-handlers-longer-than-32-chars",
			},
			Storage: config.StorageConfig{Type: "s3"},
		},
		DB:          testutil.SetupTestDB(t),
		Redis:       testutil.SetupTestRedis(t),
		Log:         testutil.NopLogger(),
		ObjectStore: store,
	}
	org := testutil.CreateTestOrganization(t, app.DB)
	adminRole := testutil.CreateAdminRole(t, app.DB, org.ID)
	user := testutil.CreateTestUser(
		t,
		app.DB,
		org.ID,
		testutil.WithRoleID(&adminRole.ID),
	)
	account := testutil.CreateTestWhatsAppAccount(t, app.DB, org.ID)
	contact := testutil.CreateTestContactWith(
		t,
		app.DB,
		org.ID,
		testutil.WithContactAccount(account.Name),
	)

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	require.NoError(t, writer.WriteField("contact_id", contact.ID.String()))
	require.NoError(t, writer.WriteField("type", string(models.MessageTypeImage)))
	require.NoError(t, writer.WriteField("reply_to_message_id", uuid.New().String()))
	part, err := writer.CreateFormFile("file", "sample.jpg")
	require.NoError(t, err)
	_, err = part.Write([]byte("synthetic-image"))
	require.NoError(t, err)
	require.NoError(t, writer.Close())

	req := testutil.NewRequest(t)
	req.RequestCtx.Request.Header.SetMethod("POST")
	req.RequestCtx.Request.Header.SetContentType(writer.FormDataContentType())
	req.RequestCtx.Request.SetBody(body.Bytes())
	testutil.SetAuthContext(req, org.ID, user.ID)
	require.NoError(t, app.SendMediaMessage(req))
	assert.Equal(t, fasthttp.StatusBadRequest, testutil.GetResponseStatusCode(req))
	assert.Empty(t, store.objects)
	assert.Len(t, store.deleted, 1)
}
