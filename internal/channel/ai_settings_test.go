package channel_test

import (
	"testing"

	channelapi "github.com/shridarpatil/whatomate/internal/channel"
	"github.com/shridarpatil/whatomate/internal/models"
	qwenapi "github.com/shridarpatil/whatomate/internal/qwen"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolveAIReplySettingsUsesCentralQwenForDefaultProviderAndPreservesBotCap(t *testing.T) {
	db := testutil.SetupTestDB(t)
	org := testutil.CreateTestOrganization(t, db)
	require.NoError(t, db.Create(&models.CopilotSettings{
		OrganizationID:  org.ID,
		IsEnabled:       true,
		Provider:        models.AIProviderQwen,
		Model:           "qwen-central",
		APIKeyEncrypted: "enc:central-key-placeholder",
		MaxTokens:       900,
		Temperature:     0.2,
	}).Error)
	require.NoError(t, db.Create(&models.ProviderIntegration{
		OrganizationID: org.ID,
		Provider:       "qwen",
		Enabled:        true,
		Config:         models.JSONB{"endpoint_region": qwenapi.RegionUS},
		CredentialData: models.JSONB{},
	}).Error)
	require.NoError(t, db.Create(&models.ChatbotSettings{
		OrganizationID:  org.ID,
		WhatsAppAccount: "instagram-main",
		IsEnabled:       true,
		AI: models.AIConfig{
			Enabled:      true,
			Provider:     "",
			APIKey:       "legacy-key-must-not-win",
			MaxTokens:    123,
			SystemPrompt: "bot-specific prompt",
		},
	}).Error)

	resolved, err := channelapi.ResolveAIReplySettings(db, org.ID, "instagram-main")
	require.NoError(t, err)
	assert.Equal(t, models.AIProviderQwen, resolved.AI.Provider)
	assert.Equal(t, "enc:central-key-placeholder", resolved.AI.APIKey)
	assert.Equal(t, "qwen-central", resolved.AI.Model)
	assert.Equal(t, 123, resolved.AI.MaxTokens)
	assert.Equal(t, "bot-specific prompt", resolved.AI.SystemPrompt)
	assert.Equal(t, "https://dashscope-us.aliyuncs.com/compatible-mode/v1", resolved.AI.BaseURL)
}
