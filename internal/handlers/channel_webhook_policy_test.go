package handlers

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	channelapi "github.com/shridarpatil/whatomate/internal/channel"
	"github.com/shridarpatil/whatomate/internal/config"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
	"gorm.io/gorm"
)

func TestMetaWebhookTenantHMACAloneCannotCreateAcceptedOrFreshInbound(t *testing.T) {
	db := testutil.SetupTestDB(t)
	org := testutil.CreateTestOrganization(t, db)
	accountID := uuid.New()
	externalID := "17841400000000001"
	const tenantInboundSecret = "tenant-visible-inbound-secret"
	const providerProofSecret = "handler-provider-proof-secret-at-least-32-bytes"
	relayURL := "https://relay.example.test/meta/v1/accounts/instagram/" + externalID
	account := &models.ChannelAccount{
		BaseModel:         models.BaseModel{ID: accountID},
		OrganizationID:    org.ID,
		Channel:           models.ChannelInstagram,
		Provider:          channelapi.RelayProvider,
		Name:              "provider-proof-webhook",
		ExternalAccountID: externalID,
		Status:            models.ChannelAccountStatusActive,
		Config:            models.JSONB{"relay_url": relayURL},
		Metadata:          models.JSONB{},
	}
	require.NoError(t, db.Create(account).Error)
	require.NoError(t, db.Create(&models.ChannelCredential{
		BaseModel:        models.BaseModel{ID: uuid.New()},
		OrganizationID:   org.ID,
		ChannelAccountID: account.ID,
		Kind:             models.ChannelCredentialKindWebhook,
		Version:          1,
		CredentialBlob: models.JSONB{
			"inbound_secret": tenantInboundSecret,
		},
		Status:   models.ChannelCredentialStatusActive,
		Metadata: models.JSONB{},
	}).Error)
	app := &App{
		DB:  db,
		Log: testutil.NopLogger(),
		Config: &config.Config{
			App: config.AppConfig{
				Environment:   "production",
				EncryptionKey: "handler-test-encryption-key",
			},
			MetaRelay: config.MetaRelayConfig{
				BaseURL:             "https://relay.example.test/meta",
				ProviderProofSecret: providerProofSecret,
				ExpectedAccountsJSON: fmt.Sprintf(`{"accounts":[{
					"organization_id":%q,
					"meta_business_id":"200000000000001",
					"channel":"instagram",
					"external_account_id":%q,
					"rereply_account_id":%q
				}]}`, org.ID.String(), externalID, account.ID.String()),
			},
		},
	}
	request := testutil.NewJSONRequest(t, map[string]any{
		"external_account_id": externalID,
		"events":              []any{},
	})
	request.RequestCtx.Request.Header.SetMethod(fasthttp.MethodPost)
	testutil.SetPathParam(request, "channel_account_id", account.ID.String())
	body := request.RequestCtx.PostBody()
	mac := hmac.New(sha256.New, []byte(tenantInboundSecret))
	_, _ = mac.Write(body)
	request.RequestCtx.Request.Header.Set(
		channelapi.RelaySignatureHeader,
		"sha256="+hex.EncodeToString(mac.Sum(nil)),
	)

	require.NoError(t, app.RelayChannelWebhook(request))
	require.Equal(t, fasthttp.StatusUnauthorized, testutil.GetResponseStatusCode(request))

	var acceptedEvents int64
	require.NoError(t, db.Model(&models.InboundEvent{}).
		Where("organization_id = ? AND channel_account_id = ?", org.ID, account.ID).
		Count(&acceptedEvents).Error)
	require.Zero(t, acceptedEvents)
	require.NoError(t, db.First(account, "id = ?", account.ID).Error)
	require.Nil(t, account.LastInboundAt)
}

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

func TestProcessNormalizedChannelEventAdvancesAccountAnchorOnlyForNewCustomerMessage(t *testing.T) {
	db := testutil.SetupTestDB(t)
	org := testutil.CreateTestOrganization(t, db)
	account := createRelayPolicyTestAccount(t, db, org.ID, models.ChannelMessenger)
	acceptedAt := time.Date(2026, time.July, 28, 8, 0, 0, 0, time.UTC)
	providerTime := acceptedAt.Add(-2 * time.Hour)
	event := validPolicyInboundEvent(providerTime)

	var anchor time.Time
	require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
		return processNormalizedChannelEvent(tx, account, &event, acceptedAt, &anchor)
	}))
	require.Equal(t, providerTime, anchor)

	anchor = time.Time{}
	require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
		return processNormalizedChannelEvent(tx, account, &event, acceptedAt, &anchor)
	}))
	require.True(t, anchor.IsZero(), "duplicate canonical event must not advance account freshness")

	sameMessageNewCanonical := event
	sameMessageNewCanonical.DedupeKey = "different-dedupe-" + uuid.NewString()
	sameMessageNewCanonical.ProviderEventID = "different-event-" + uuid.NewString()
	anchor = time.Time{}
	require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
		return processNormalizedChannelEvent(
			tx,
			account,
			&sameMessageNewCanonical,
			acceptedAt,
			&anchor,
		)
	}))
	require.True(t, anchor.IsZero(), "duplicate external_message_id must not advance account freshness")

	status := channelapi.InboundEvent{
		DedupeKey:       "status-" + uuid.NewString(),
		ProviderEventID: "status-event-" + uuid.NewString(),
		Type:            channelapi.NormalizedEventTypeMessageStatus,
		OccurredAt:      acceptedAt,
		MessageStatus: &channelapi.MessageStatusUpdate{
			ExternalMessageID: event.Message.ExternalMessageID,
			Type:              models.MessageEventTypeDelivered,
			OccurredAt:        acceptedAt,
		},
	}
	anchor = time.Time{}
	require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
		return processNormalizedChannelEvent(tx, account, &status, acceptedAt, &anchor)
	}))
	require.True(t, anchor.IsZero(), "status/read/reaction events must not advance account freshness")

	emptyMessage := validPolicyInboundEvent(providerTime)
	emptyMessage.Message.Parts = nil
	anchor = time.Time{}
	err := db.Transaction(func(tx *gorm.DB) error {
		return processNormalizedChannelEvent(tx, account, &emptyMessage, acceptedAt, &anchor)
	})
	require.ErrorContains(t, err, "at least one message part")
	require.True(t, anchor.IsZero(), "empty message must not advance account freshness")

	var messageCount int64
	require.NoError(t, db.Model(&models.Message{}).
		Where("organization_id = ?", org.ID).
		Count(&messageCount).Error)
	require.EqualValues(t, 1, messageCount)
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
