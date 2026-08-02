package handlers

import (
	"testing"

	channelapi "github.com/shridarpatil/whatomate/internal/channel"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestThreadsChannelCreationRequiresAdditionalEntitlement(t *testing.T) {
	t.Parallel()

	assert.True(t, validChannelIdentifier(models.ChannelThreads))

	key, required := additionalChannelCreationEntitlement(models.ChannelThreads)
	assert.True(t, required)
	assert.Equal(t, "threads.public_engagement.enabled", key)
	assert.Equal(t, channelapi.ThreadsPublicEngagementEntitlementKey, key)

	key, required = additionalChannelCreationEntitlement(models.ChannelInstagram)
	assert.False(t, required)
	assert.Empty(t, key)
}

func TestUnsupportedChannelCreationFailsClosedUntilAdaptersExist(t *testing.T) {
	t.Parallel()

	threads := ChannelAccountRequest{
		Channel:  models.ChannelThreads,
		Provider: channelapi.RelayProvider,
	}
	err := validateChannelCreationPolicy(threads)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "preparation-only")

	tiktok := ChannelAccountRequest{
		Channel:  models.ChannelTikTok,
		Provider: channelapi.RelayProvider,
	}
	err = validateChannelCreationPolicy(tiktok)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "preparation-only")

	require.NoError(t, validateChannelCreationPolicy(ChannelAccountRequest{
		Channel:  models.ChannelWebChat,
		Provider: channelapi.RelayProvider,
	}))
}

func TestThreadsChannelMetadataStaysPublicEngagementOnly(t *testing.T) {
	t.Parallel()

	config := threadsPublicEngagementConfig(models.JSONB{
		"relay_url":                 "https://relay.example.test/threads",
		"engagement_mode":           "direct_message",
		"direct_messages_supported": true,
	})
	assert.Equal(t, "https://relay.example.test/threads", config["relay_url"])
	assert.Equal(t, threadsPublicEngagementMode, config["engagement_mode"])
	assert.Equal(t, false, config["direct_messages_supported"])
	assert.Equal(t, true, config["beta"])
	assert.Equal(t, false, config["activation_available"])

	capabilities := threadsPublicEngagementCapabilities(models.JSONB{
		"text":                true,
		"business_initiation": true,
		"direct_messages":     true,
	})
	assert.Equal(t, true, capabilities["text"])
	assert.Equal(t, false, capabilities["business_initiation"])
	assert.Equal(t, false, capabilities["direct_messages"])
	assert.Equal(t, false, capabilities["public_replies"])
	assert.Equal(t, false, capabilities["mentions"])
	assert.Equal(t, true, capabilities["reply_target_required"])
}

func TestThreadsChannelHasNoActivatableAdapterDuringBeta(t *testing.T) {
	t.Parallel()

	app := &App{}
	_, err := app.channelAdapter(&models.ChannelAccount{
		Channel:  models.ChannelThreads,
		Provider: channelapi.RelayProvider,
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrChannelAdapterUnavailable)
	assert.Contains(t, err.Error(), "no approved Threads relay adapter")
}

func TestThreadsPublicReplyRequiresExistingReplyOrMentionTarget(t *testing.T) {
	t.Parallel()

	conversation := &models.InboxConversation{
		Channel:                models.ChannelThreads,
		ExternalConversationID: "threads-public-target-123",
		Metadata:               models.JSONB{"engagement_type": "reply"},
		ChannelAccount: &models.ChannelAccount{
			Channel:  models.ChannelThreads,
			Provider: channelapi.RelayProvider,
			Config: models.JSONB{
				"engagement_mode": threadsPublicEngagementMode,
			},
		},
	}
	valid := &SendInboxConversationMessageRequest{
		ReplyToExternalID: "threads-public-target-123",
	}
	require.NoError(t, validateThreadsPublicReplyTarget(conversation, valid))
	assert.Equal(t, "threads-public-target-123", valid.ReplyToExternalID)

	mention := *conversation
	mention.Metadata = models.JSONB{"engagement_type": "mention"}
	require.NoError(t, validateThreadsPublicReplyTarget(&mention, &SendInboxConversationMessageRequest{
		ReplyToExternalID: mention.ExternalConversationID,
	}))

	err := validateThreadsPublicReplyTarget(conversation, &SendInboxConversationMessageRequest{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "existing reply or mention target")

	err = validateThreadsPublicReplyTarget(conversation, &SendInboxConversationMessageRequest{
		ReplyToExternalID: "other-public-target",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "selected public conversation")

	err = validateThreadsPublicReplyTarget(conversation, &SendInboxConversationMessageRequest{
		ReplyToExternalID: " threads-public-target-123 ",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "selected public conversation")

	directMessage := *conversation
	directMessage.Metadata = models.JSONB{"engagement_type": "direct_message"}
	err = validateThreadsPublicReplyTarget(&directMessage, &SendInboxConversationMessageRequest{
		ReplyToExternalID: directMessage.ExternalConversationID,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "direct messages and standalone posts are not supported")

	incompatibleRelay := *conversation
	incompatibleRelay.ChannelAccount = &models.ChannelAccount{
		Channel:  models.ChannelThreads,
		Provider: "meta",
		Config:   conversation.ChannelAccount.Config,
	}
	err = validateThreadsPublicReplyTarget(&incompatibleRelay, &SendInboxConversationMessageRequest{
		ReplyToExternalID: incompatibleRelay.ExternalConversationID,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "compatible signed provider relay")
}
