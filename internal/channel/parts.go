package channel

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"unicode/utf8"

	"github.com/shridarpatil/whatomate/internal/models"
)

const (
	MaxMessageParts            = 20
	MaxMessageTextRunes        = 10_000
	MaxMessageCaptionRunes     = 4_096
	MaxMessageMediaURLRunes    = 4_096
	MaxMessagePartPayloadBytes = 64 << 10
	MaxMessagePartBytes        = int64(100 << 20)
)

// ValidateMessagePartsShape applies provider-independent bounds before message
// content is persisted or forwarded to an adapter.
func ValidateMessagePartsShape(parts []MessagePart) error {
	if len(parts) == 0 {
		return errors.New("at least one message part is required")
	}
	if len(parts) > MaxMessageParts {
		return fmt.Errorf("message contains more than %d parts", MaxMessageParts)
	}
	for index, part := range parts {
		switch part.Type {
		case models.MessagePartTypeText,
			models.MessagePartTypeHTML,
			models.MessagePartTypeImage,
			models.MessagePartTypeVideo,
			models.MessagePartTypeAudio,
			models.MessagePartTypeDocument,
			models.MessagePartTypeLocation,
			models.MessagePartTypeContact,
			models.MessagePartTypeInteractive,
			models.MessagePartTypeTemplate,
			models.MessagePartTypeReaction:
		default:
			return fmt.Errorf("message part %d type %q is not supported", index, part.Type)
		}
		if utf8.RuneCountInString(part.Text) > MaxMessageTextRunes {
			return fmt.Errorf("message part %d text is too long", index)
		}
		if utf8.RuneCountInString(part.Caption) > MaxMessageCaptionRunes {
			return fmt.Errorf("message part %d caption is too long", index)
		}
		if utf8.RuneCountInString(part.MediaURL) > MaxMessageMediaURLRunes ||
			utf8.RuneCountInString(part.MimeType) > 255 ||
			utf8.RuneCountInString(part.Filename) > 512 ||
			utf8.RuneCountInString(part.ProviderMediaRef) > 512 {
			return fmt.Errorf("message part %d contains an overlong field", index)
		}
		if part.SizeBytes < 0 || part.SizeBytes > MaxMessagePartBytes {
			return fmt.Errorf("message part %d has an invalid size", index)
		}
		if part.MediaURL != "" {
			parsed, err := url.Parse(strings.TrimSpace(part.MediaURL))
			if err != nil ||
				!strings.EqualFold(parsed.Scheme, "https") ||
				parsed.Hostname() == "" ||
				parsed.User != nil {
				return fmt.Errorf("message part %d media_url must be an HTTPS URL without user credentials", index)
			}
		}
		payload, err := json.Marshal(part.Payload)
		if err != nil {
			return fmt.Errorf("message part %d payload cannot be encoded", index)
		}
		if len(payload) > MaxMessagePartPayloadBytes {
			return fmt.Errorf("message part %d payload is too large", index)
		}
	}
	return nil
}
