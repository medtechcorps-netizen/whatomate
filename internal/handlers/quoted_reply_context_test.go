package handlers

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

type replyContextProjectionWire struct {
	Original *struct {
		ID            string            `json:"id"`
		Content       map[string]string `json:"content"`
		ImageRevision string            `json:"image_revision"`
		ScopedContext bool              `json:"scoped_context"`
	} `json:"reply_to_message"`
	Unavailable bool `json:"reply_context_unavailable"`
}

func replyContextTestMessages() (models.Message, models.Message) {
	orgID, contactID := uuid.New(), uuid.New()
	createdAt := time.Date(2026, time.November, 26, 1, 0, 0, 0, time.UTC)
	parent := models.Message{
		BaseModel:      models.BaseModel{ID: uuid.New(), CreatedAt: createdAt, UpdatedAt: createdAt},
		OrganizationID: orgID, ContactID: contactID, WhatsAppAccount: "synthetic-account",
		Direction: models.DirectionOutgoing, MessageType: models.MessageTypeImage,
		Content: "Stored image caption", MediaMimeType: "image/png",
	}
	parent.MediaURL = tenantObjectKey(orgID, "messages", "images", "stored-photo.png")
	child := models.Message{
		BaseModel:      models.BaseModel{ID: uuid.New(), CreatedAt: createdAt.Add(time.Minute)},
		OrganizationID: orgID, ContactID: contactID, WhatsAppAccount: parent.WhatsAppAccount,
		Direction: models.DirectionIncoming, MessageType: models.MessageTypeText,
		IsReply: true, ReplyToMessageID: &parent.ID, ReplyToMessage: &parent,
	}
	return child, parent
}

func decodeReplyContextProjection(t *testing.T, response MessageResponse) (replyContextProjectionWire, []byte) {
	t.Helper()
	raw, err := json.Marshal(response)
	require.NoError(t, err)
	var wire replyContextProjectionWire
	require.NoError(t, json.Unmarshal(raw, &wire))
	return wire, raw
}

func TestStoredReplyContextNativeProjection(t *testing.T) {
	app := &App{}
	t.Run("protected caption and opaque revision", func(t *testing.T) {
		child, parent := replyContextTestMessages()
		child.ReplyToMessage = &parent
		resp := app.buildTranscriptMessagesResponse([]models.Message{child}, child.OrganizationID, child.ContactID, nil)
		wire, raw := decodeReplyContextProjection(t, resp[0])
		require.NotNil(t, wire.Original)
		require.Equal(t, parent.ID.String(), wire.Original.ID)
		require.Equal(t, "Stored image caption", wire.Original.Content["body"])
		require.Regexp(t, "^[0-9a-f]{64}$", wire.Original.ImageRevision)
		require.False(t, wire.Unavailable)
		require.True(t, wire.Original.ScopedContext)
		require.NotContains(t, string(raw), parent.MediaURL)

		unscoped, _ := decodeReplyContextProjection(t, app.buildMessagesResponse([]models.Message{child})[0])
		require.NotNil(t, unscoped.Original)
		require.Empty(t, unscoped.Original.ImageRevision)
		require.False(t, unscoped.Original.ScopedContext)
		require.False(t, unscoped.Unavailable)
	})

	t.Run("unsupported original retains only the ordinary native preview", func(t *testing.T) {
		child, parent := replyContextTestMessages()
		parent.MessageType = models.MessageTypeVideo
		child.ReplyToMessage = &parent
		ordinary := app.buildMessagesResponse([]models.Message{child})[0]
		response := app.buildTranscriptMessagesResponse([]models.Message{child}, child.OrganizationID, child.ContactID, nil)[0]
		require.NotNil(t, ordinary.ReplyToMessage)
		require.Equal(t, ordinary.ReplyToMessage, response.ReplyToMessage)
		require.False(t, response.ReplyToMessage.ScopedContext)
		require.Empty(t, response.ReplyToMessage.ImageRevision)
		require.False(t, response.ReplyContextUnavailable)
	})

	t.Run("caption is rune bounded", func(t *testing.T) {
		child, parent := replyContextTestMessages()
		parent.Content = strings.Repeat("文", 700)
		child.ReplyToMessage = &parent
		resp := app.buildTranscriptMessagesResponse([]models.Message{child}, child.OrganizationID, child.ContactID, nil)
		wire, _ := decodeReplyContextProjection(t, resp[0])
		require.NotNil(t, wire.Original)
		require.Equal(t, strings.Repeat("文", 500), wire.Original.Content["body"])
	})

	for _, tc := range []struct {
		name   string
		change func(*models.Message, *models.Message) *time.Time
	}{
		{"other organization", func(_ *models.Message, parent *models.Message) *time.Time {
			parent.OrganizationID = uuid.New()
			return nil
		}},
		{"other contact", func(_ *models.Message, parent *models.Message) *time.Time {
			parent.ContactID = uuid.New()
			return nil
		}},
		{"other account", func(_ *models.Message, parent *models.Message) *time.Time {
			parent.WhatsAppAccount = "different-account"
			return nil
		}},
		{"deleted original", func(_ *models.Message, parent *models.Message) *time.Time {
			parent.DeletedAt = gorm.DeletedAt{Time: parent.CreatedAt, Valid: true}
			return nil
		}},
		{"revoked original", func(_ *models.Message, parent *models.Message) *time.Time {
			parent.Metadata = models.JSONB{coexistenceMediaRevokedMetadataKey: true}
			return nil
		}},
		{"cutoff excludes original", func(_ *models.Message, parent *models.Message) *time.Time {
			cutoff := parent.CreatedAt.Add(time.Second)
			return &cutoff
		}},
		{"self quote", func(child, parent *models.Message) *time.Time {
			parent.ID = child.ID
			return nil
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			child, parent := replyContextTestMessages()
			cutoff := tc.change(&child, &parent)
			child.ReplyToMessageID = &parent.ID
			child.ReplyToMessage = &parent
			resp := app.buildTranscriptMessagesResponse([]models.Message{child}, child.OrganizationID, child.ContactID, cutoff)
			wire, raw := decodeReplyContextProjection(t, resp[0])
			require.Nil(t, wire.Original)
			require.True(t, wire.Unavailable)
			require.NotContains(t, string(raw), parent.Content)
			require.NotContains(t, string(raw), parent.MediaURL)
		})
	}

	t.Run("unproved populated bridges retain only the ordinary preview", func(t *testing.T) {
		child, parent := replyContextTestMessages()
		bridge := uuid.New()
		child.InboxConversationID, parent.InboxConversationID = &bridge, &bridge
		child.ReplyToMessage = &parent
		resp := app.buildTranscriptMessagesResponse([]models.Message{child}, child.OrganizationID, child.ContactID, nil)
		wire, _ := decodeReplyContextProjection(t, resp[0])
		require.NotNil(t, wire.Original)
		require.Equal(t, parent.ID.String(), wire.Original.ID)
		require.False(t, wire.Original.ScopedContext)
		require.Empty(t, wire.Original.ImageRevision)
		require.False(t, wire.Unavailable)
	})

	t.Run("unsafe image storage omits the revision", func(t *testing.T) {
		child, parent := replyContextTestMessages()
		parent.MediaURL = "https://untrusted.invalid/attachment.png"
		child.ReplyToMessage = &parent
		resp := app.buildTranscriptMessagesResponse([]models.Message{child}, child.OrganizationID, child.ContactID, nil)
		wire, raw := decodeReplyContextProjection(t, resp[0])
		require.NotNil(t, wire.Original)
		require.Equal(t, parent.Content, wire.Original.Content["body"])
		require.Empty(t, wire.Original.ImageRevision)
		require.NotContains(t, string(raw), parent.MediaURL)
	})
}

func TestQuotedReplyImageBindsNativeAccountAcrossReads(t *testing.T) {
	child, parent := replyContextTestMessages()
	account := models.WhatsAppAccount{
		BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: child.OrganizationID,
		Name: child.WhatsAppAccount, PhoneID: " synthetic-phone ",
	}
	before, ok := prepareQuotedReplyImage(&child, &parent, &account)
	require.True(t, ok)
	require.Equal(t, "synthetic-phone", before.nativePhoneID)

	for _, replaced := range []models.WhatsAppAccount{
		{BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: account.OrganizationID, Name: account.Name, PhoneID: account.PhoneID},
		{BaseModel: account.BaseModel, OrganizationID: account.OrganizationID, Name: account.Name, PhoneID: "rotated-phone"},
	} {
		after, ok := prepareQuotedReplyImage(&child, &parent, &replaced)
		require.True(t, ok)
		require.Equal(t, before.revision, after.revision, "internal account authority does not change the opaque public format")
		require.NotEqual(t, before, after, "the final load cannot reuse the pre-change authorization")
	}
}
