package handlers

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/config"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/internal/whatsappaccount"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The planned key is stored as media_url before the copy runs, so it must be
// exactly the key saveTenantMedia later returns for the same plan.
func TestLaneEPlanForwardMediaMatchesStorageLayout(t *testing.T) {
	orgID := uuid.New()
	source := &models.Message{MediaURL: "images/source.jpg", MediaMimeType: "image/jpeg", MediaFilename: "scan.jpg"}

	local := &App{Config: &config.Config{Storage: config.StorageConfig{LocalPath: t.TempDir()}}, Log: testutil.NopLogger()}
	plan := local.planForwardMedia(orgID, source)
	assert.Equal(t, "image/jpeg", plan.MimeType)
	assert.True(t, strings.HasPrefix(plan.DestinationKey, "images/") && strings.HasSuffix(plan.DestinationKey, ".jpg"), plan.DestinationKey)
	key, err := local.saveTenantMedia(context.Background(), orgID, plan.ObjectSubdir, plan.LocalSubdir, plan.ObjectName, []byte("x"), plan.MimeType)
	require.NoError(t, err)
	assert.Equal(t, plan.DestinationKey, key)
	_, err = os.Stat(filepath.Join(local.Config.Storage.LocalPath, filepath.FromSlash(key)))
	require.NoError(t, err)

	s3 := &App{Config: &config.Config{Storage: config.StorageConfig{Type: "s3"}}}
	objectPlan := s3.planForwardMedia(orgID, &models.Message{MediaURL: "organizations/x/messages/documents/a", MediaFilename: "Consent.PDF"})
	assert.Equal(t, tenantObjectKey(orgID, "messages", "documents", objectPlan.ObjectName), objectPlan.DestinationKey)
	assert.True(t, strings.HasSuffix(objectPlan.ObjectName, ".pdf"), objectPlan.ObjectName)
	assert.NotContains(t, objectPlan.DestinationKey, "organizations/x/", "the copy never reuses the source key")
}

func TestLaneEForwardedMediaIsAsyncOnly(t *testing.T) {
	app := &App{Log: testutil.NopLogger()}
	account := &models.WhatsAppAccount{OrganizationID: uuid.New(), Status: whatsappaccount.StatusActive}
	_, err := app.SendOutgoingMessage(context.Background(), OutgoingMessageRequest{
		Account:      account,
		Contact:      &models.Contact{BaseModel: models.BaseModel{ID: uuid.New()}},
		Type:         models.MessageTypeImage,
		forwardMedia: &forwardMediaCopy{OrganizationID: account.OrganizationID},
	}, ChatbotSendOptions())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "asynchronous delivery")
}
