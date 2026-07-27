package channel

import (
	"strings"
	"testing"

	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidateMessagePartsShapeRejectsUnknownAndOversizedParts(t *testing.T) {
	t.Parallel()

	require.NoError(t, ValidateMessagePartsShape([]MessagePart{{
		Type: models.MessagePartTypeText,
		Text: "Hello",
	}}))
	assert.Error(t, ValidateMessagePartsShape([]MessagePart{{
		Type: models.MessagePartType("provider_private"),
	}}))
	assert.Error(t, ValidateMessagePartsShape([]MessagePart{{
		Type: models.MessagePartTypeText,
		Text: strings.Repeat("x", MaxMessageTextRunes+1),
	}}))
	assert.Error(t, ValidateMessagePartsShape([]MessagePart{{
		Type:     models.MessagePartTypeImage,
		MediaURL: "http://cdn.example.test/image.jpg",
	}}))
}
