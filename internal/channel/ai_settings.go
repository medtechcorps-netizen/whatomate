package channel

import (
	"errors"
	"strings"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/models"
	qwenapi "github.com/shridarpatil/whatomate/internal/qwen"
	"gorm.io/gorm"
)

// ResolveCentralQwenSettings returns the one organization-level Qwen profile
// managed by Settings > Integrations, plus its optional allowlisted regional
// endpoint. Account-level Copilot rows and legacy ChatbotSettings API keys are
// deliberately not credential fallbacks.
func ResolveCentralQwenSettings(
	tx *gorm.DB,
	organizationID uuid.UUID,
) (models.CopilotSettings, string, error) {
	var central models.CopilotSettings
	if tx == nil || organizationID == uuid.Nil {
		return central, "", errors.New("tenant Qwen settings transaction is required")
	}
	if err := tx.Where(
		"organization_id = ? AND whats_app_account = ''",
		organizationID,
	).Order("updated_at DESC, id ASC").First(&central).Error; err != nil {
		return central, "", err
	}
	if !central.IsEnabled ||
		(central.Provider != "" && central.Provider != models.AIProviderQwen) ||
		strings.TrimSpace(central.APIKeyEncrypted) == "" {
		return central, "", gorm.ErrRecordNotFound
	}

	baseURL := ""
	var integration models.ProviderIntegration
	err := tx.Where(
		"organization_id = ? AND provider = ?",
		organizationID,
		"qwen",
	).First(&integration).Error
	switch {
	case err == nil:
		region, _ := integration.Config["endpoint_region"].(string)
		region = strings.TrimSpace(region)
		if region != "" && region != "platform" {
			resolved, resolveErr := qwenapi.BaseURLForRegion(region)
			if resolveErr != nil {
				return central, "", resolveErr
			}
			baseURL = resolved
		}
	case !errors.Is(err, gorm.ErrRecordNotFound):
		return central, "", err
	}
	return central, baseURL, nil
}

// ApplyCentralQwenSettings overlays runtime Qwen credentials and provider
// tuning onto a chatbot profile while preserving its bot-specific prompt and
// history policy.
func ApplyCentralQwenSettings(
	tx *gorm.DB,
	organizationID uuid.UUID,
	ai *models.AIConfig,
) error {
	if ai == nil || (ai.Provider != "" && ai.Provider != models.AIProviderQwen) {
		return nil
	}
	central, baseURL, err := ResolveCentralQwenSettings(tx, organizationID)
	if err != nil {
		// An empty legacy provider means "use the organization default". It
		// becomes Qwen only when a central Qwen profile actually exists; an
		// explicit qwen selection remains fail-closed when central settings are
		// absent or disabled.
		if ai.Provider == "" && errors.Is(err, gorm.ErrRecordNotFound) {
			return nil
		}
		return err
	}
	ai.Provider = models.AIProviderQwen
	ai.APIKey = central.APIKeyEncrypted
	ai.BaseURL = baseURL
	ai.Model = strings.TrimSpace(central.Model)
	if ai.Model == "" {
		ai.Model = qwenapi.DefaultModel
	}
	// The chatbot automation tab owns its output cap. Use the central value
	// only for legacy/unset profiles; the Integration Center owns credentials,
	// endpoint region, and provider model, not each bot's response-length policy.
	if ai.MaxTokens < 64 || ai.MaxTokens > 4000 {
		ai.MaxTokens = central.MaxTokens
		if ai.MaxTokens < 64 || ai.MaxTokens > 4000 {
			ai.MaxTokens = 700
		}
	}
	ai.Temperature = central.Temperature
	if ai.Temperature < 0 || ai.Temperature > 2 {
		ai.Temperature = 0.3
	}
	return nil
}

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
	if err == nil {
		return settings, ApplyCentralQwenSettings(tx, organizationID, &settings.AI)
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return settings, err
	}
	err = tx.Where(
		"organization_id = ? AND whats_app_account = ''",
		organizationID,
	).Order("updated_at DESC, id ASC").First(&settings).Error
	if err == nil {
		return settings, ApplyCentralQwenSettings(tx, organizationID, &settings.AI)
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return settings, err
	}
	return settings, gorm.ErrRecordNotFound
}
