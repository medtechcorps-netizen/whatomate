package channel

import (
	"errors"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/models"
	"gorm.io/gorm"
)

// ResolveAIReplySettings returns the authoritative Qwen settings profile for a
// social channel. Callers that schedule work must hold the tenant AI-control
// mutex before calling it; workers re-evaluate the same precedence before
// generation and finalization.
func ResolveAIReplySettings(
	tx *gorm.DB,
	organizationID uuid.UUID,
	socialAccountName string,
) (models.ChatbotSettings, error) {
	var settings models.ChatbotSettings
	if tx == nil || organizationID == uuid.Nil {
		return settings, errors.New("tenant AI settings transaction is required")
	}
	err := tx.Where(
		"organization_id = ? AND whats_app_account = ?",
		organizationID,
		socialAccountName,
	).Order("updated_at DESC, id ASC").First(&settings).Error
	if err == nil || !errors.Is(err, gorm.ErrRecordNotFound) {
		return settings, err
	}
	err = tx.Where(
		"organization_id = ? AND whats_app_account = ''",
		organizationID,
	).Order("updated_at DESC, id ASC").First(&settings).Error
	if err == nil || !errors.Is(err, gorm.ErrRecordNotFound) {
		return settings, err
	}
	err = tx.Where(
		"organization_id = ? AND is_enabled = ? AND ai_enabled = ? AND ai_provider = ? AND ai_api_key <> ''",
		organizationID,
		true,
		true,
		models.AIProviderQwen,
	).Order("updated_at DESC, id ASC").First(&settings).Error
	return settings, err
}
