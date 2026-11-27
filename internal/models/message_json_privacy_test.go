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

func TestMessageJSONDoesNotExposePendingQuoteMetadata(t *testing.T) {
	const pendingKey = "coexistence_pending_quote_v1"
	parent := Message{
		Content: "synthetic parent",
		Metadata: JSONB{
			pendingKey: "synthetic-parent-sealed-value",
			"public":   "parent public metadata",
		},
	}
	child := Message{
		Content: "synthetic child",
		Metadata: JSONB{
			pendingKey:                  "synthetic-child-sealed-value",
			MessageBookmarksMetadataKey: "synthetic private bookmark",
			"public":                    "child public metadata",
		},
		ReplyToMessage: &parent,
	}

	raw, err := json.Marshal(struct {
		Messages []Message `json:"messages"`
	}{Messages: []Message{child}})
	require.NoError(t, err)
	require.NotContains(t, string(raw), pendingKey)
	require.NotContains(t, string(raw), "synthetic-parent-sealed-value")
	require.NotContains(t, string(raw), "synthetic-child-sealed-value")
	require.NotContains(t, string(raw), MessageBookmarksMetadataKey)
	require.NotContains(t, string(raw), "synthetic private bookmark")
	require.Contains(t, string(raw), "parent public metadata")
	require.Contains(t, string(raw), "child public metadata")
	require.Contains(t, string(raw), "synthetic parent")
	require.Contains(t, string(raw), "synthetic child")
	require.Equal(t, "synthetic-parent-sealed-value", parent.Metadata[pendingKey])
	require.Equal(t, "synthetic-child-sealed-value", child.Metadata[pendingKey])
	require.Contains(t, child.Metadata, MessageBookmarksMetadataKey)
}

func TestMessageJSONKeepsPrivateMetadataOutOfNestedReplies(t *testing.T) {
	private := JSONB{
		"public":                    "ordinary metadata",
		MessageBookmarksMetadataKey: map[string]any{"synthetic-viewer": true},
		MessageCoexistencePendingQuoteMetadataKey: "synthetic-sealed-context",
		MessageNativeMediaRetryMetadataKey: map[string]any{
			"identity": map[string]any{"content": "synthetic-retained-caption", "media_id": "synthetic-provider-object"},
		},
	}
	reply := Message{Content: "synthetic reply", Metadata: private}
	message := Message{Content: "synthetic message", Metadata: private, ReplyToMessage: &reply}

	raw, err := json.Marshal(struct {
		Message Message `json:"message"`
	}{Message: message})
	require.NoError(t, err)
	var envelope struct {
		Message struct {
			Content        string         `json:"content"`
			Metadata       map[string]any `json:"metadata"`
			ReplyToMessage struct {
				Content  string         `json:"content"`
				Metadata map[string]any `json:"metadata"`
			} `json:"reply_to_message"`
		} `json:"message"`
	}
	require.NoError(t, json.Unmarshal(raw, &envelope))
	require.Equal(t, "synthetic message", envelope.Message.Content)
	require.Equal(t, "synthetic reply", envelope.Message.ReplyToMessage.Content)
	require.Equal(t, map[string]any{"public": "ordinary metadata"}, envelope.Message.Metadata)
	require.Equal(t, map[string]any{"public": "ordinary metadata"}, envelope.Message.ReplyToMessage.Metadata)
	require.Len(t, private, 4)
	require.Equal(t, "synthetic-sealed-context", private[MessageCoexistencePendingQuoteMetadataKey])
	require.Contains(t, private, MessageNativeMediaRetryMetadataKey)
	require.Contains(t, private, MessageBookmarksMetadataKey)
}
