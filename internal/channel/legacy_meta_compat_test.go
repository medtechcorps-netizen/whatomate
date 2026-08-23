package channel

import (
	"testing"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEnsureLegacyMetaAccountRefreshIsAdditiveAndReturnsFreshProjection(t *testing.T) {
	db := testutil.SetupTestDB(t)
	organization := testutil.CreateTestOrganization(t, db)
	legacy := models.WhatsAppAccount{
		BaseModel:          models.BaseModel{ID: uuid.New()},
		OrganizationID:     organization.ID,
		Name:               "compat-" + uuid.NewString(),
		PhoneID:            "9" + uuid.NewString()[0:15],
		BusinessID:         "business-" + uuid.NewString(),
		AccessToken:        "test-token",
		WebhookVerifyToken: "verify-" + uuid.NewString(),
		Status:             "active",
	}
	require.NoError(t, db.Create(&legacy).Error)
	ref := LegacyMetaAccountRef{
		ID: legacy.ID, OrganizationID: organization.ID, Name: legacy.Name, Status: legacy.Status,
	}
	initial, err := ensureLegacyMetaAccount(db, ref)
	require.NoError(t, err)

	legacyConfig := models.JSONB{
		"legacy_read_only": true,
		"outbound_enabled": false,
		"reply_route":      "chat",
	}
	legacyCapabilities := models.JSONB{
		"text": true, "template": true, "mark_read": true, "attachments": true,
	}
	require.NoError(t, db.Model(&models.ChannelAccount{}).Where("id = ?", initial.ID).Updates(map[string]any{
		"config": legacyConfig, "capabilities": legacyCapabilities,
	}).Error)

	refreshed, err := ensureLegacyMetaAccount(db, ref)
	require.NoError(t, err)
	var persisted models.ChannelAccount
	require.NoError(t, db.Where("id = ? AND organization_id = ?", refreshed.ID, organization.ID).First(&persisted).Error)
	assert.Equal(t, persisted.Config, refreshed.Config)
	assert.Equal(t, persisted.Capabilities, refreshed.Capabilities)
	assert.Equal(t, "chat", refreshed.Config["reply_route"])
	for _, key := range []string{
		"template", "mark_read", "attachments",
		"text", "media", "replies", "templates", "service_window", "read_receipts",
	} {
		assert.Equal(t, true, refreshed.Capabilities[key], "missing compatible capability %q", key)
	}
	assert.NotContains(t, refreshed.Capabilities, "legacy_text_reply_endpoint", "rollout marker must remain response-only")
}
