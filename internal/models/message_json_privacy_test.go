package models

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMessageJSONDoesNotExposePrivateBookmarkMetadata(t *testing.T) {
	message := Message{
		Content: "synthetic",
		Metadata: JSONB{
			"existing": "ordinary metadata",
			MessageBookmarksMetadataKey: map[string]any{
				"pinned": true,
				"stars":  map[string]any{"private-viewer": true},
			},
		},
	}
	raw, err := json.Marshal(struct{ Message Message }{Message: message})
	require.NoError(t, err)
	require.NotContains(t, string(raw), MessageBookmarksMetadataKey)
	require.NotContains(t, string(raw), "private-viewer")
	require.Contains(t, string(raw), "ordinary metadata")
	require.Contains(t, string(raw), "synthetic")
	require.Contains(t, message.Metadata, MessageBookmarksMetadataKey)
}
