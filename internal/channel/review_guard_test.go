package channel

import (
	"testing"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/metareview"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/stretchr/testify/assert"
)

func TestIsStagingMessengerReviewMarkedIsStrictAndDenialOnly(t *testing.T) {
	t.Parallel()

	account := &models.ChannelAccount{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: uuid.New(),
		Channel:        models.ChannelMessenger,
		Provider:       RelayProvider,
		Metadata: models.JSONB{
			"management_mode":   metaMessengerOAuthManagementMode,
			"review_relay_mode": metareview.Marker,
			"review_generation": uuid.NewString(),
		},
	}
	assert.True(t, IsStagingMessengerReviewMarked(account))

	for _, key := range []string{"management_mode", "review_relay_mode"} {
		copy := *account
		copy.Metadata = models.JSONB{}
		for name, value := range account.Metadata {
			copy.Metadata[name] = value
		}
		delete(copy.Metadata, key)
		assert.False(t, IsStagingMessengerReviewMarked(&copy), key)
	}

	corruptGeneration := *account
	corruptGeneration.Metadata = models.JSONB{}
	for name, value := range account.Metadata {
		corruptGeneration.Metadata[name] = value
	}
	corruptGeneration.Metadata["review_generation"] = "corrupt"
	assert.True(t, IsStagingMessengerReviewMarked(&corruptGeneration))
}
