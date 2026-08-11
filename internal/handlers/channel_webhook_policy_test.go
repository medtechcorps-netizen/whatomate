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

func TestProcessNormalizedChannelEventReusesContactAcrossMessengerAccountRenewal(t *testing.T) {
	db := testutil.SetupTestDB(t)
	org := testutil.CreateTestOrganization(t, db)
	pageID := "page-" + uuid.NewString()
	psid := "psid-" + uuid.NewString()
	acceptedAt := time.Date(2026, time.August, 12, 6, 0, 0, 0, time.UTC)

	oldAccount := createRelayPolicyTestAccount(t, db, org.ID, models.ChannelMessenger)
	require.NoError(t, db.Model(oldAccount).Update("external_account_id", pageID).Error)
	oldAccount.ExternalAccountID = pageID

	first := validPolicyInboundEvent(acceptedAt.Add(-time.Minute))
	first.Message.Sender.ExternalID = psid
	first.Message.Sender.Address = psid
	first.Message.Conversation.ExternalID = psid
	require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
		return processNormalizedChannelEvent(tx, oldAccount, &first, acceptedAt)
	}))

	var oldIdentity models.ContactIdentity
	require.NoError(t, db.Where(
		"organization_id = ? AND channel_account_id = ? AND external_id = ?",
		org.ID,
		oldAccount.ID,
		psid,
	).First(&oldIdentity).Error)
	require.NoError(t, db.Model(oldAccount).Updates(map[string]any{
		"status": models.ChannelAccountStatusDisconnected,
		"metadata": models.JSONB{
			"onboarding_state": "review_deprovisioned",
		},
	}).Error)
	require.NoError(t, db.Delete(oldAccount).Error)

	newAccount := createRelayPolicyTestAccount(t, db, org.ID, models.ChannelMessenger)
	require.NoError(t, db.Model(newAccount).Update("external_account_id", pageID).Error)
	newAccount.ExternalAccountID = pageID

	second := validPolicyInboundEvent(acceptedAt)
	second.Message.Sender.ExternalID = psid
	second.Message.Sender.Address = psid
	second.Message.Conversation.ExternalID = psid
	require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
		return processNormalizedChannelEvent(tx, newAccount, &second, acceptedAt)
	}))

	var contacts []models.Contact
	require.NoError(t, db.Where("organization_id = ?", org.ID).Find(&contacts).Error)
	require.Len(t, contacts, 1, "renewing the same Page must not duplicate its PSID contact")
	require.Equal(t, oldIdentity.ContactID, contacts[0].ID)

	var identityCount int64
	require.NoError(t, db.Model(&models.ContactIdentity{}).Where(
		"organization_id = ? AND external_id = ?",
		org.ID,
		psid,
	).Count(&identityCount).Error)
	require.EqualValues(t, 2, identityCount)

	var renewedIdentity models.ContactIdentity
	require.NoError(t, db.Where(
		"organization_id = ? AND channel_account_id = ? AND external_id = ?",
		org.ID,
		newAccount.ID,
		psid,
	).First(&renewedIdentity).Error)
	require.Equal(t, oldIdentity.ContactID, renewedIdentity.ContactID)

	var conversations []models.InboxConversation
	require.NoError(t, db.Where(
		"organization_id = ? AND contact_id = ?",
		org.ID,
		oldIdentity.ContactID,
	).Find(&conversations).Error)
	require.Len(t, conversations, 2)
	conversationAccounts := map[uuid.UUID]bool{}
	conversationIDs := map[uuid.UUID]bool{}
	for _, conversation := range conversations {
		conversationAccounts[conversation.ChannelAccountID] = true
		conversationIDs[conversation.ID] = true
		require.Equal(t, psid, conversation.ExternalConversationID)
	}
	require.True(t, conversationAccounts[oldAccount.ID])
	require.True(t, conversationAccounts[newAccount.ID])

	var messages []models.Message
	require.NoError(t, db.Where(
		"organization_id = ? AND contact_id = ?",
		org.ID,
		oldIdentity.ContactID,
	).Find(&messages).Error)
	require.Len(t, messages, 2)
	for _, message := range messages {
		require.NotNil(t, message.InboxConversationID)
		require.True(t, conversationIDs[*message.InboxConversationID])
	}
}

func TestFindRenewedMessengerLineageContactRequiresExactTenantAndPage(t *testing.T) {
	db := testutil.SetupTestDB(t)
	org := testutil.CreateTestOrganization(t, db)
	otherOrg := testutil.CreateTestOrganization(t, db)
	pageID := "page-" + uuid.NewString()
	psid := "psid-" + uuid.NewString()
	_, lineageContact := createTombstonedMessengerLineage(
		t,
		db,
		org.ID,
		pageID,
		psid,
		"lineage-"+uuid.NewString(),
	)

	for _, testCase := range []struct {
		name       string
		account    *models.ChannelAccount
		externalID string
	}{
		{
			name: "different Page",
			account: &models.ChannelAccount{
				OrganizationID:    org.ID,
				Channel:           models.ChannelMessenger,
				Provider:          channelapi.RelayProvider,
				ExternalAccountID: "other-page-" + uuid.NewString(),
			},
			externalID: psid,
		},
		{
			name: "different tenant",
			account: &models.ChannelAccount{
				OrganizationID:    otherOrg.ID,
				Channel:           models.ChannelMessenger,
				Provider:          channelapi.RelayProvider,
				ExternalAccountID: pageID,
			},
			externalID: psid,
		},
		{
			name: "blank Page ID",
			account: &models.ChannelAccount{
				OrganizationID: org.ID,
				Channel:        models.ChannelMessenger,
				Provider:       channelapi.RelayProvider,
			},
			externalID: psid,
		},
		{
			name: "blank PSID",
			account: &models.ChannelAccount{
				OrganizationID:    org.ID,
				Channel:           models.ChannelMessenger,
				Provider:          channelapi.RelayProvider,
				ExternalAccountID: pageID,
			},
			externalID: " ",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
				contact, err := findRenewedMessengerLineageContact(
					tx,
					testCase.account,
					testCase.externalID,
				)
				require.ErrorIs(t, err, gorm.ErrRecordNotFound)
				require.Nil(t, contact)
				return nil
			}))
		})
	}

	var stored models.Contact
	require.NoError(t, db.Where(
		"id = ? AND organization_id = ?",
		lineageContact.ID,
		org.ID,
	).First(&stored).Error)
}

func TestProcessNormalizedChannelEventFailsClosedOnAmbiguousMessengerRenewalLineage(t *testing.T) {
	db := testutil.SetupTestDB(t)
	org := testutil.CreateTestOrganization(t, db)
	pageID := "page-" + uuid.NewString()
	psid := "psid-" + uuid.NewString()
	createTombstonedMessengerLineage(
		t,
		db,
		org.ID,
		pageID,
		psid,
		"lineage-a-"+uuid.NewString(),
	)
	createTombstonedMessengerLineage(
		t,
		db,
		org.ID,
		pageID,
		psid,
		"lineage-b-"+uuid.NewString(),
	)

	newAccount := createRelayPolicyTestAccount(t, db, org.ID, models.ChannelMessenger)
	require.NoError(t, db.Model(newAccount).Update("external_account_id", pageID).Error)
	newAccount.ExternalAccountID = pageID
	acceptedAt := time.Date(2026, time.August, 12, 7, 0, 0, 0, time.UTC)
	event := validPolicyInboundEvent(acceptedAt)
	event.Message.Sender.ExternalID = psid
	event.Message.Sender.Address = psid
	event.Message.Conversation.ExternalID = psid

	err := db.Transaction(func(tx *gorm.DB) error {
		return processNormalizedChannelEvent(tx, newAccount, &event, acceptedAt)
	})
	require.ErrorContains(t, err, "lineage maps to multiple contacts")

	var currentIdentityCount int64
	require.NoError(t, db.Model(&models.ContactIdentity{}).Where(
		"organization_id = ? AND channel_account_id = ?",
		org.ID,
		newAccount.ID,
	).Count(&currentIdentityCount).Error)
	require.Zero(t, currentIdentityCount)

	var conversationCount int64
	require.NoError(t, db.Model(&models.InboxConversation{}).Where(
		"organization_id = ? AND channel_account_id = ?",
		org.ID,
		newAccount.ID,
	).Count(&conversationCount).Error)
	require.Zero(t, conversationCount)

	var messageCount int64
	require.NoError(t, db.Model(&models.Message{}).Where(
		"organization_id = ?",
		org.ID,
	).Count(&messageCount).Error)
	require.Zero(t, messageCount)

	var inboundEventCount int64
	require.NoError(t, db.Model(&models.InboundEvent{}).Where(
		"organization_id = ? AND channel_account_id = ?",
		org.ID,
		newAccount.ID,
	).Count(&inboundEventCount).Error)
	require.Zero(t, inboundEventCount)
}

func createTombstonedMessengerLineage(
	t *testing.T,
	db *gorm.DB,
	organizationID uuid.UUID,
	pageID string,
	psid string,
	contactAddress string,
) (*models.ChannelAccount, *models.Contact) {
	t.Helper()

	account := createRelayPolicyTestAccount(t, db, organizationID, models.ChannelMessenger)
	require.NoError(t, db.Model(account).Update("external_account_id", pageID).Error)
	account.ExternalAccountID = pageID
	contact := &models.Contact{
		BaseModel:       models.BaseModel{ID: uuid.New()},
		OrganizationID:  organizationID,
		PhoneNumber:     contactAddress,
		ProfileName:     "Lineage contact",
		WhatsAppAccount: account.Name,
		Tags:            models.JSONBArray{},
		Metadata: models.JSONB{
			"channel":  models.ChannelMessenger,
			"provider": channelapi.RelayProvider,
		},
	}
	require.NoError(t, db.Create(contact).Error)
	now := time.Now().UTC()
	require.NoError(t, db.Create(&models.ContactIdentity{
		BaseModel:        models.BaseModel{ID: uuid.New()},
		OrganizationID:   organizationID,
		ContactID:        contact.ID,
		ChannelAccountID: account.ID,
		Channel:          models.ChannelMessenger,
		ExternalID:       psid,
		Address:          contactAddress,
		DisplayName:      contact.ProfileName,
		IsPrimary:        true,
		FirstSeenAt:      &now,
		LastSeenAt:       &now,
		Metadata:         models.JSONB{},
	}).Error)
	require.NoError(t, db.Model(account).Updates(map[string]any{
		"status": models.ChannelAccountStatusDisconnected,
		"metadata": models.JSONB{
			"onboarding_state": "review_deprovisioned",
		},
	}).Error)
	require.NoError(t, db.Delete(account).Error)
	return account, contact
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
