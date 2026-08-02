package handlers

import (
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	channelapi "github.com/shridarpatil/whatomate/internal/channel"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestProcessNormalizedChannelEventRetainsLatestMetaServiceWindowOutOfOrder(t *testing.T) {
	db := testutil.SetupTestDB(t)
	org := testutil.CreateTestOrganization(t, db)
	account := createRelayPolicyTestAccount(
		t,
		db,
		org.ID,
		models.ChannelInstagram,
	)

	acceptedAt := time.Date(2026, time.July, 28, 8, 0, 0, 0, time.UTC)
	newerProviderTime := acceptedAt.Add(-time.Hour)
	olderProviderTime := newerProviderTime.Add(-3 * time.Hour)
	newer := validPolicyInboundEvent(newerProviderTime)
	older := validPolicyInboundEvent(olderProviderTime)
	older.Message.Conversation = newer.Message.Conversation
	older.Message.Sender = newer.Message.Sender

	for _, event := range []*channelapi.InboundEvent{&newer, &older} {
		require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
			return processNormalizedChannelEvent(tx, account, event, acceptedAt)
		}))
	}

	var conversation models.InboxConversation
	require.NoError(t, db.Where(
		"organization_id = ? AND channel_account_id = ? AND external_conversation_id = ?",
		org.ID,
		account.ID,
		newer.Message.Conversation.ExternalID,
	).First(&conversation).Error)
	require.NotNil(t, conversation.ServiceWindowEndsAt)
	require.Equal(
		t,
		newerProviderTime.Add(channelapi.MetaCustomerServiceWindow),
		conversation.ServiceWindowEndsAt.UTC(),
		"a delayed older event must not shorten the newer service window",
	)
}

func TestMonotonicServiceWindowEndsAtSerializesCompetingPostgresUpdates(t *testing.T) {
	db := testutil.SetupTestDB(t)
	org := testutil.CreateTestOrganization(t, db)
	account := createRelayPolicyTestAccount(
		t,
		db,
		org.ID,
		models.ChannelMessenger,
	)
	acceptedAt := time.Date(2026, time.July, 28, 8, 0, 0, 0, time.UTC)
	event := validPolicyInboundEvent(acceptedAt.Add(-time.Hour))

	require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
		return processNormalizedChannelEvent(tx, account, &event, acceptedAt)
	}))

	var conversation models.InboxConversation
	require.NoError(t, db.Where(
		"organization_id = ? AND channel_account_id = ? AND external_conversation_id = ?",
		org.ID,
		account.ID,
		event.Message.Conversation.ExternalID,
	).First(&conversation).Error)

	require.NotNil(t, conversation.ServiceWindowEndsAt)
	earlierEnd := conversation.ServiceWindowEndsAt.UTC().Add(2 * time.Hour)
	laterEnd := earlierEnd.Add(4 * time.Hour)
	laterWriter := db.Begin()
	require.NoError(t, laterWriter.Error)
	t.Cleanup(func() {
		_ = laterWriter.Rollback().Error
	})
	laterResult := laterWriter.Model(&models.InboxConversation{}).
		Where("id = ? AND organization_id = ?", conversation.ID, org.ID).
		Update("service_window_ends_at", monotonicServiceWindowEndsAt(laterEnd))
	require.NoError(t, laterResult.Error)
	require.EqualValues(t, 1, laterResult.RowsAffected)

	earlierDone := make(chan error, 1)
	go func() {
		earlierDone <- db.Session(&gorm.Session{NewDB: true}).
			Model(&models.InboxConversation{}).
			Where("id = ? AND organization_id = ?", conversation.ID, org.ID).
			Update(
				"service_window_ends_at",
				monotonicServiceWindowEndsAt(earlierEnd),
			).Error
	}()

	// The earlier writer targets the row while the later writer still owns its
	// PostgreSQL row lock. It must wait rather than race a client-side read.
	select {
	case err := <-earlierDone:
		require.Failf(
			t,
			"competing update did not wait for the row lock",
			"unexpected result: %v",
			err,
		)
	case <-time.After(100 * time.Millisecond):
	}

	require.NoError(t, laterWriter.Commit().Error)
	select {
	case err := <-earlierDone:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		require.Fail(t, "competing service-window update did not complete")
	}

	require.NoError(t, db.First(&conversation, "id = ?", conversation.ID).Error)
	require.NotNil(t, conversation.ServiceWindowEndsAt)
	require.Equal(
		t,
		laterEnd,
		conversation.ServiceWindowEndsAt.UTC(),
		"the writer released second must compare against the committed later expiry",
	)
}

func TestProcessNormalizedChannelEventPersistsMetaServiceWindowFromProviderTime(t *testing.T) {
	for _, channel := range []models.Channel{
		models.ChannelInstagram,
		models.ChannelMessenger,
	} {
		channel := channel
		t.Run(string(channel), func(t *testing.T) {
			db := testutil.SetupTestDB(t)
			org := testutil.CreateTestOrganization(t, db)
			createCentralQwenTestSettings(t, db, org.ID)
			account := createRelayPolicyTestAccount(t, db, org.ID, channel)
			settings := &models.ChatbotSettings{
				BaseModel:      models.BaseModel{ID: uuid.New()},
				OrganizationID: org.ID,
				IsEnabled:      true,
				AI: models.AIConfig{
					Enabled:  true,
					Provider: models.AIProviderQwen,
					APIKey:   "test-key",
				},
			}
			require.NoError(t, db.Create(settings).Error)

			acceptedAt := time.Date(2026, time.July, 28, 4, 30, 0, 0, time.UTC)
			// A delayed relay must preserve Meta's original service-window
			// anchor instead of reopening 24 hours at server receipt.
			providerTime := acceptedAt.Add(-3 * time.Hour)
			event := validPolicyInboundEvent(providerTime)

			require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
				return processNormalizedChannelEvent(tx, account, &event, acceptedAt)
			}))
			require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
				return processNormalizedChannelEvent(tx, account, &event, acceptedAt)
			}))

			var conversation models.InboxConversation
			require.NoError(t, db.
				Where(
					"organization_id = ? AND channel_account_id = ? AND external_conversation_id = ?",
					org.ID,
					account.ID,
					event.Message.Conversation.ExternalID,
				).
				First(&conversation).Error)
			require.NotNil(t, conversation.ServiceWindowEndsAt)
			require.Equal(
				t,
				providerTime.Add(channelapi.MetaCustomerServiceWindow),
				conversation.ServiceWindowEndsAt.UTC(),
			)

			var messageCount int64
			require.NoError(t, db.Model(&models.Message{}).
				Where("organization_id = ? AND inbox_conversation_id = ?", org.ID, conversation.ID).
				Count(&messageCount).Error)
			require.EqualValues(t, 1, messageCount)

			var replyJobCount int64
			require.NoError(t, db.Model(&models.ScheduledJob{}).
				Where(
					"organization_id = ? AND kind = ?",
					org.ID,
					models.ScheduledJobKindChannelAIReply,
				).
				Count(&replyJobCount).Error)
			require.EqualValues(t, 1, replyJobCount, "duplicate inbound must not duplicate the AI job")

			var replyJob models.ScheduledJob
			require.NoError(t, db.
				Where(
					"organization_id = ? AND kind = ?",
					org.ID,
					models.ScheduledJobKindChannelAIReply,
				).
				First(&replyJob).Error)
			require.Equal(t, providerTime.Format(time.RFC3339Nano), fmt.Sprint(
				replyJob.Payload["service_window_at"],
			))
		})
	}
}

func TestProcessNormalizedChannelEventDoesNotOpenWindowForOtherChannels(t *testing.T) {
	db := testutil.SetupTestDB(t)
	org := testutil.CreateTestOrganization(t, db)
	account := createRelayPolicyTestAccount(t, db, org.ID, models.ChannelEmail)
	acceptedAt := time.Date(2026, time.July, 28, 4, 30, 0, 0, time.UTC)
	event := validPolicyInboundEvent(acceptedAt)

	require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
		return processNormalizedChannelEvent(tx, account, &event, acceptedAt)
	}))

	var conversation models.InboxConversation
	require.NoError(t, db.
		Where(
			"organization_id = ? AND channel_account_id = ? AND external_conversation_id = ?",
			org.ID,
			account.ID,
			event.Message.Conversation.ExternalID,
		).
		First(&conversation).Error)
	require.Nil(t, conversation.ServiceWindowEndsAt)
	var replyJobCount int64
	require.NoError(t, db.Model(&models.ScheduledJob{}).
		Where(
			"organization_id = ? AND kind = ?",
			org.ID,
			models.ScheduledJobKindChannelAIReply,
		).
		Count(&replyJobCount).Error)
	require.Zero(t, replyJobCount)
}

func TestDelayedMetaInboundDoesNotReopenWindowOrQueueAI(t *testing.T) {
	db := testutil.SetupTestDB(t)
	org := testutil.CreateTestOrganization(t, db)
	account := createRelayPolicyTestAccount(
		t,
		db,
		org.ID,
		models.ChannelInstagram,
	)
	serverAcceptedAt := time.Date(
		2026,
		time.July,
		28,
		4,
		30,
		0,
		0,
		time.UTC,
	)
	providerTime := serverAcceptedAt.Add(
		-channelapi.MetaCustomerServiceWindow - time.Minute,
	)
	event := validPolicyInboundEvent(providerTime)

	require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
		return processNormalizedChannelEvent(
			tx,
			account,
			&event,
			serverAcceptedAt,
		)
	}))

	var conversation models.InboxConversation
	require.NoError(t, db.Where(
		"organization_id = ? AND channel_account_id = ?",
		org.ID,
		account.ID,
	).First(&conversation).Error)
	require.NotNil(t, conversation.ServiceWindowEndsAt)
	require.Equal(
		t,
		providerTime.Add(channelapi.MetaCustomerServiceWindow),
		conversation.ServiceWindowEndsAt.UTC(),
	)
	require.False(t, conversation.ServiceWindowEndsAt.After(serverAcceptedAt))

	var messageCount int64
	require.NoError(t, db.Model(&models.Message{}).
		Where("organization_id = ?", org.ID).
		Count(&messageCount).Error)
	require.EqualValues(t, 1, messageCount, "the CRM must retain the delayed inbound")

	var replyJobCount int64
	require.NoError(t, db.Model(&models.ScheduledJob{}).
		Where(
			"organization_id = ? AND kind = ?",
			org.ID,
			models.ScheduledJobKindChannelAIReply,
		).
		Count(&replyJobCount).Error)
	require.Zero(t, replyJobCount, "an expired Meta window must not queue Qwen")
}

func TestProcessNormalizedChannelEventRejectsOutgoingEchoBeforePersistence(t *testing.T) {
	db := testutil.SetupTestDB(t)
	org := testutil.CreateTestOrganization(t, db)
	account := createRelayPolicyTestAccount(t, db, org.ID, models.ChannelInstagram)
	acceptedAt := time.Date(2026, time.July, 28, 4, 30, 0, 0, time.UTC)
	event := validPolicyInboundEvent(acceptedAt)
	event.Message.Direction = models.DirectionOutgoing
	event.Message.Sender.Role = models.ConversationParticipantRoleBot

	err := db.Transaction(func(tx *gorm.DB) error {
		return processNormalizedChannelEvent(tx, account, &event, acceptedAt)
	})
	require.ErrorContains(t, err, "direction must be incoming")

	var messageCount int64
	require.NoError(t, db.Model(&models.Message{}).
		Where("organization_id = ?", org.ID).
		Count(&messageCount).Error)
	require.Zero(t, messageCount)

	var conversationCount int64
	require.NoError(t, db.Model(&models.InboxConversation{}).
		Where("organization_id = ?", org.ID).
		Count(&conversationCount).Error)
	require.Zero(t, conversationCount)

	var canonicalEventCount int64
	require.NoError(t, db.Model(&models.InboundEvent{}).
		Where("organization_id = ?", org.ID).
		Count(&canonicalEventCount).Error)
	require.Zero(t, canonicalEventCount)

	var replyJobCount int64
	require.NoError(t, db.Model(&models.ScheduledJob{}).
		Where(
			"organization_id = ? AND kind = ?",
			org.ID,
			models.ScheduledJobKindChannelAIReply,
		).
		Count(&replyJobCount).Error)
	require.Zero(t, replyJobCount, "outgoing echo must never schedule a reply")
}

func createRelayPolicyTestAccount(
	t *testing.T,
	db *gorm.DB,
	organizationID uuid.UUID,
	channel models.Channel,
) *models.ChannelAccount {
	t.Helper()

	account := &models.ChannelAccount{
		BaseModel:         models.BaseModel{ID: uuid.New()},
		OrganizationID:    organizationID,
		Channel:           channel,
		Provider:          channelapi.RelayProvider,
		Name:              fmt.Sprintf("policy-%s-%s", channel, uuid.NewString()[:8]),
		ExternalAccountID: "external-" + uuid.NewString(),
		Status:            models.ChannelAccountStatusActive,
		Capabilities: models.JSONB{
			"service_window": false,
		},
		Config: models.JSONB{
			"outbound_enabled": true,
			"ai_reply_enabled": true,
		},
		Metadata: models.JSONB{},
	}
	require.NoError(t, db.Create(account).Error)
	return account
}

func validPolicyInboundEvent(providerTime time.Time) channelapi.InboundEvent {
	messageID := "message-" + uuid.NewString()
	return channelapi.InboundEvent{
		DedupeKey:       "dedupe-" + messageID,
		ProviderEventID: "event-" + messageID,
		Type:            channelapi.NormalizedEventTypeMessage,
		OccurredAt:      providerTime,
		Message: &channelapi.InboundMessage{
			ExternalMessageID: messageID,
			Conversation: channelapi.ConversationRef{
				ExternalID: "conversation-" + uuid.NewString(),
			},
			Sender: channelapi.Participant{
				ExternalID:  "customer-" + uuid.NewString(),
				Address:     "customer@example.test",
				DisplayName: "Policy Customer",
				Role:        models.ConversationParticipantRoleCustomer,
			},
			Direction: models.DirectionIncoming,
			Parts: []channelapi.MessagePart{
				{
					Type: models.MessagePartTypeText,
					Text: "Hello",
				},
			},
			SentAt:     providerTime,
			ReceivedAt: providerTime,
		},
	}
}
