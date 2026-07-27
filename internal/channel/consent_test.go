package channel

import (
	"testing"
	"time"

	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidateOutboundPurposeRestrictsMarketing(t *testing.T) {
	t.Parallel()

	require.NoError(t, ValidateOutboundPurpose(models.ChannelPreferencePurposeService))
	require.NoError(t, ValidateOutboundPurpose(models.ChannelPreferencePurposeTransactional))
	assert.Error(t, ValidateOutboundPurpose(models.ChannelPreferencePurposeMarketing))
	assert.Error(t, ValidateOutboundPurpose(""))
}

func TestValidateServiceWindowRequiresInitiationTemplate(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()
	expired := now.Add(-time.Minute)
	capabilities := Capabilities{
		ServiceWindow:      true,
		Templates:          true,
		BusinessInitiation: true,
	}
	assert.Error(t, ValidateServiceWindow(
		capabilities,
		&expired,
		[]MessagePart{{Type: models.MessagePartTypeText, Text: "Hello"}},
		now,
	))
	require.NoError(t, ValidateServiceWindow(
		capabilities,
		&expired,
		[]MessagePart{{Type: models.MessagePartTypeTemplate}},
		now,
	))
}
