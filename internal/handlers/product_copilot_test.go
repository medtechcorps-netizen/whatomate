package handlers

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestParseCopilotTaskIsAllowlisted(t *testing.T) {
	t.Parallel()

	for _, raw := range []string{"reply", " SUMMARY ", "qualify", "extract_actions"} {
		task, ok := parseCopilotTask(raw)
		require.True(t, ok, raw)
		assert.NotEmpty(t, task)
	}
	_, ok := parseCopilotTask("send_message")
	assert.False(t, ok, "Copilot must not expose a send operation")
}

func TestBuildCopilotContextIsGroundedAndDoesNotIncludePhone(t *testing.T) {
	t.Parallel()

	contact := &models.Contact{
		BaseModel:   models.BaseModel{ID: uuid.New()},
		ProfileName: "Alya",
		PhoneNumber: "+60123456789",
	}
	messages := []models.Message{
		{
			BaseModel:   models.BaseModel{ID: uuid.New(), CreatedAt: time.Now()},
			Direction:   models.DirectionIncoming,
			MessageType: models.MessageTypeText,
			Content:     "Do you have a Pilates class on Saturday?",
		},
		{
			BaseModel:   models.BaseModel{ID: uuid.New(), CreatedAt: time.Now()},
			Direction:   models.DirectionOutgoing,
			MessageType: models.MessageTypeText,
			Content:     "Let me check the schedule.",
		},
	}
	contexts := []models.AIContext{{
		BaseModel:     models.BaseModel{ID: uuid.New()},
		Name:          "Studio schedule policy",
		ContextType:   models.ContextTypeStatic,
		StaticContent: "Only confirm a class after checking live availability.",
	}}

	got := buildCopilotContext(contact, messages, contexts)

	assert.Contains(t, got, "Alya")
	assert.Contains(t, got, "Customer: Do you have")
	assert.Contains(t, got, "Business: Let me check")
	assert.Contains(t, got, "Studio schedule policy")
	assert.Contains(t, got, "human agent")
	assert.NotContains(t, got, contact.PhoneNumber)
}

func TestParseCopilotStructuredResult(t *testing.T) {
	t.Parallel()

	got := parseCopilotStructuredResult(
		models.CopilotTaskTypeQualify,
		"```json\n{\"intent\":\"pilates\",\"urgency\":\"normal\"}\n```",
	)
	assert.Equal(t, "pilates", got["intent"])
	assert.Equal(t, "normal", got["urgency"])

	assert.Empty(t, parseCopilotStructuredResult(models.CopilotTaskTypeReply, `{"should":"stay text"}`))
	assert.Empty(t, parseCopilotStructuredResult(models.CopilotTaskTypeExtractActions, "not json"))
}

func TestBuildCopilotTaskPromptAlwaysKeepsHumanInControl(t *testing.T) {
	t.Parallel()

	reply := buildCopilotTaskPrompt(models.CopilotTaskTypeReply, runCopilotRequest{
		Tone:        "warm",
		Language:    "Bahasa Melayu",
		Instruction: "Mention the introductory offer.",
	})
	assert.Contains(t, reply, "human agent to review")
	assert.Contains(t, reply, "Bahasa Melayu")
	assert.Contains(t, reply, "warm")

	for _, task := range []models.CopilotTaskType{
		models.CopilotTaskTypeReply,
		models.CopilotTaskTypeSummary,
		models.CopilotTaskTypeQualify,
		models.CopilotTaskTypeExtractActions,
	} {
		assert.NotContains(t, strings.ToLower(buildCopilotTaskPrompt(task, runCopilotRequest{})), "send automatically")
	}
}

func TestResolveCopilotSettingsDoesNotLeakAcrossAccountScopes(t *testing.T) {
	db := testutil.SetupTestDB(t)
	organization := testutil.CreateTestOrganization(t, db)
	require.NoError(t, db.Create(&models.CopilotSettings{
		BaseModel:           models.BaseModel{ID: uuid.New()},
		OrganizationID:      organization.ID,
		WhatsAppAccount:     "other-account",
		IsEnabled:           true,
		Provider:            models.AIProviderQwen,
		Model:               "qwen3.7-plus",
		APIKeyEncrypted:     "other-account-key",
		MaxTokens:           500,
		Temperature:         0.3,
		RetentionDays:       30,
		AllowedCapabilities: models.StringArray{},
		Guardrails:          models.JSONB{},
	}).Error)

	app := &App{DB: db, Log: testutil.NopLogger()}
	_, err := app.resolveCopilotQwenSettings(organization.ID, "target-account")
	require.Error(t, err)
	assert.True(t, errors.Is(err, gorm.ErrRecordNotFound))

	require.NoError(t, db.Create(&models.CopilotSettings{
		BaseModel:           models.BaseModel{ID: uuid.New()},
		OrganizationID:      organization.ID,
		WhatsAppAccount:     "",
		IsEnabled:           true,
		Provider:            models.AIProviderQwen,
		Model:               "qwen3.7-plus",
		APIKeyEncrypted:     "global-key",
		MaxTokens:           500,
		Temperature:         0.3,
		RetentionDays:       30,
		AllowedCapabilities: models.StringArray{},
		Guardrails:          models.JSONB{},
	}).Error)

	resolved, err := app.resolveCopilotQwenSettings(organization.ID, "target-account")
	require.NoError(t, err)
	assert.Equal(t, "global-key", resolved.chatbot.AI.APIKey)
}

func TestCopilotContextAccessRequiresEveryDependentReadPermission(t *testing.T) {
	db := testutil.SetupTestDB(t)
	organization := testutil.CreateTestOrganization(t, db)
	app := &App{DB: db, Log: testutil.NopLogger()}

	executeOnlyRole := testutil.CreateTestRoleWithKeys(
		t,
		db,
		organization.ID,
		"copilot-execute-only",
		[]string{"copilot:execute"},
	)
	executeOnlyUser := testutil.CreateTestUser(
		t,
		db,
		organization.ID,
		testutil.WithRoleID(&executeOnlyRole.ID),
	)
	assert.False(
		t,
		app.hasCopilotContextAccess(executeOnlyUser.ID, organization.ID),
		"copilot execute must not implicitly expose customer or knowledge context",
	)

	groundedRole := testutil.CreateTestRoleWithKeys(
		t,
		db,
		organization.ID,
		"copilot-grounded",
		[]string{
			"copilot:execute",
			"contacts:read",
			"chat:read",
			"chatbot.ai:read",
		},
	)
	groundedUser := testutil.CreateTestUser(
		t,
		db,
		organization.ID,
		testutil.WithRoleID(&groundedRole.ID),
	)
	assert.True(t, app.hasCopilotContextAccess(groundedUser.ID, organization.ID))
}
