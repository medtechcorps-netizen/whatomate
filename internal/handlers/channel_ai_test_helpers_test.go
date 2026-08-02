package handlers

import (
	"testing"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func createCentralQwenTestSettings(
	t *testing.T,
	db *gorm.DB,
	organizationID uuid.UUID,
) {
	t.Helper()
	require.NoError(t, db.Create(&models.CopilotSettings{
		BaseModel:       models.BaseModel{ID: uuid.New()},
		OrganizationID:  organizationID,
		WhatsAppAccount: "",
		IsEnabled:       true,
		Provider:        models.AIProviderQwen,
		APIKeyEncrypted: "central-test-key",
		Model:           "qwen-plus",
		MaxTokens:       700,
		Temperature:     0.3,
	}).Error)
}
