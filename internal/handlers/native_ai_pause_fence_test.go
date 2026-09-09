package handlers

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAutomaticAISendClassificationIsNarrow(t *testing.T) {
	assert.True(t, ChatbotSendOptions().AutomaticAI)
	assert.False(t, DefaultSendOptions().AutomaticAI)
	assert.False(t, APISendOptions().AutomaticAI)
	assert.False(t, SLASendOptions().AutomaticAI)
}

func TestAutomaticAIActionCannotClaimWithoutPhysicalAttemptFence(t *testing.T) {
	app := &App{
		inboundContinuation: &inboundContinuationExecution{
			OrganizationID: uuid.New(),
			ContactID:      uuid.New(),
			MessageID:      uuid.New(),
			WAMID:          "wamid.unfenced",
		},
	}

	claim, err := app.claimInboundContinuationAction(
		context.Background(),
		"text",
		nil,
	)
	require.Error(t, err)
	assert.Nil(t, claim)
	assert.True(t, inboundContinuationStoppedByPolicy(err))
}

func TestAutomaticAISendRejectsUnfencedCallerBeforePersistence(t *testing.T) {
	app := &App{}
	message, err := app.SendOutgoingMessage(
		context.Background(),
		OutgoingMessageRequest{
			Account: &models.WhatsAppAccount{
				BaseModel:      models.BaseModel{ID: uuid.New()},
				OrganizationID: uuid.New(),
				Status:         "active",
			},
			Contact: &models.Contact{BaseModel: models.BaseModel{ID: uuid.New()}},
			Type:    models.MessageTypeText,
			Content: "must not persist",
		},
		ChatbotSendOptions(),
	)
	require.Error(t, err)
	assert.Nil(t, message)
	assert.Contains(t, err.Error(), "physical organization attempt fence")
}
