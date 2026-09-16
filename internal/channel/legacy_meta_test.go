package channel_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	channelapi "github.com/shridarpatil/whatomate/internal/channel"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestEnsureLegacyMetaWhatsAppAccountCreatesOnlyCredentialFreeAccountShadow(t *testing.T) {
	db := testutil.SetupTestDB(t)
	testutil.TruncateTables(db)

	organization := createLegacyMetaTestOrganization(t, db, "account-only")
	otherOrganization := createLegacyMetaTestOrganization(t, db, "account-only-other")
	account := createLegacyMetaTestAccount(t, db, organization.ID, "Identity Review")

	shadow, err := channelapi.EnsureLegacyMetaWhatsAppAccount(db, legacyMetaRef(account))
	require.NoError(t, err)
	require.NotNil(t, shadow)
	assert.Equal(t, organization.ID, shadow.OrganizationID)
	assert.Equal(t, models.ChannelWhatsApp, shadow.Channel)
	assert.Equal(t, channelapi.LegacyMetaProvider, shadow.Provider)
	assert.Equal(t, false, shadow.Config["outbound_enabled"])
	assert.Equal(t, true, shadow.Config["legacy_read_only"])

	replayed, err := channelapi.EnsureLegacyMetaWhatsAppAccount(db, legacyMetaRef(account))
	require.NoError(t, err)
	assert.Equal(t, shadow.ID, replayed.ID)

	for model, label := range map[any]string{
		&models.Contact{}:           "contacts",
		&models.Message{}:           "messages",
		&models.InboxConversation{}: "conversations",
		&models.OutboxJob{}:         "outbox jobs",
		&models.ChannelCredential{}: "channel credentials",
	} {
		var count int64
		require.NoError(t, db.Model(model).Where("organization_id = ?", organization.ID).Count(&count).Error)
		assert.Zero(t, count, "account-only bridge must not create %s", label)
	}

	wrongTenant := legacyMetaRef(account)
	wrongTenant.OrganizationID = otherOrganization.ID
	_, err = channelapi.EnsureLegacyMetaWhatsAppAccount(db, wrongTenant)
	require.Error(t, err)
}

func TestLegacyMetaMirrorIsTenantScopedIdempotentAndDeliveryNeutral(t *testing.T) {
	db := testutil.SetupTestDB(t)

	orgA := createLegacyMetaTestOrganization(t, db, "mirror-a")
	orgB := createLegacyMetaTestOrganization(t, db, "mirror-b")
	accountA := createLegacyMetaTestAccount(t, db, orgA.ID, "Main A")
	accountB := createLegacyMetaTestAccount(t, db, orgB.ID, "Main B")
	contactA := createLegacyMetaTestContact(t, db, orgA.ID, accountA.Name, "601100000001", false)
	contactB := createLegacyMetaTestContact(t, db, orgB.ID, accountB.Name, "601100000001", false)
	messageA := createLegacyMetaTestMessage(
		t,
		db,
		orgA.ID,
		accountA.Name,
		contactA.ID,
		models.DirectionIncoming,
		"tenant A",
	)
	messageB := createLegacyMetaTestMessage(
		t,
		db,
		orgB.ID,
		accountB.Name,
		contactB.ID,
		models.DirectionIncoming,
		"tenant B",
	)

	resultA, err := channelapi.MirrorLegacyWhatsAppMessage(
		db,
		legacyMetaRef(accountA),
		messageA.ID,
	)
	require.NoError(t, err)
	assert.True(t, resultA.Linked)
	resultB, err := channelapi.MirrorLegacyWhatsAppMessage(
		db,
		legacyMetaRef(accountB),
		messageB.ID,
	)
	require.NoError(t, err)
	assert.True(t, resultB.Linked)
	assert.NotEqual(t, resultA.ChannelAccountID, resultB.ChannelAccountID)
	assert.NotEqual(t, resultA.ConversationID, resultB.ConversationID)

	replayed, err := channelapi.MirrorLegacyWhatsAppMessage(
		db,
		legacyMetaRef(accountA),
		messageA.ID,
	)
	require.NoError(t, err)
	assert.False(t, replayed.Linked)
	assert.Equal(t, resultA.ConversationID, replayed.ConversationID)

	var linkedMessage models.Message
	require.NoError(t, db.Where("id = ? AND organization_id = ?", messageA.ID, orgA.ID).
		First(&linkedMessage).Error)
	require.NotNil(t, linkedMessage.InboxConversationID)
	assert.Equal(t, resultA.ConversationID, *linkedMessage.InboxConversationID)

	var conversation models.InboxConversation
	require.NoError(t, db.Where("id = ? AND organization_id = ?", resultA.ConversationID, orgA.ID).
		First(&conversation).Error)
	assert.Equal(t, contactA.ID, conversation.ContactID)
	assert.Equal(t, models.ChannelWhatsApp, conversation.Channel)
	assert.Equal(t, 1, conversation.UnreadCount)
	assert.Equal(t, "tenant A", conversation.LastMessagePreview)

	var account models.ChannelAccount
	require.NoError(t, db.Where("id = ? AND organization_id = ?", resultA.ChannelAccountID, orgA.ID).
		First(&account).Error)
	assert.Equal(t, channelapi.LegacyMetaProvider, account.Provider)
	assert.Equal(t, false, account.Config["outbound_enabled"])
	assert.Equal(t, true, account.Config["legacy_read_only"])

	var credentials int64
	require.NoError(t, db.Model(&models.ChannelCredential{}).
		Where("organization_id = ? AND channel_account_id = ?", orgA.ID, account.ID).
		Count(&credentials).Error)
	assert.Zero(t, credentials)
	var outboxJobs int64
	require.NoError(t, db.Model(&models.OutboxJob{}).
		Where("organization_id = ?", orgA.ID).
		Count(&outboxJobs).Error)
	assert.Zero(t, outboxJobs, "mirroring must never enqueue a second delivery")
	var messages int64
	require.NoError(t, db.Model(&models.Message{}).
		Where("organization_id = ?", orgA.ID).
		Count(&messages).Error)
	assert.EqualValues(t, 1, messages, "mirroring must reuse the legacy message envelope")

	require.NoError(t, channelapi.MarkLegacyWhatsAppConversationRead(db, orgA.ID, contactA.ID))
	require.NoError(t, db.Where("id = ?", resultA.ConversationID).First(&conversation).Error)
	assert.Zero(t, conversation.UnreadCount)
	var otherConversation models.InboxConversation
	require.NoError(t, db.Where("id = ?", resultB.ConversationID).First(&otherConversation).Error)
	assert.Equal(t, 1, otherConversation.UnreadCount, "read mirroring must not cross tenants")
}

func TestLegacyMetaBackfillDoesNotCopyCredentialsAndIsIdempotent(t *testing.T) {
	db := testutil.SetupTestDB(t)

	org := createLegacyMetaTestOrganization(t, db, "backfill")
	account := createLegacyMetaTestAccount(t, db, org.ID, "Backfill")
	contact := createLegacyMetaTestContact(t, db, org.ID, account.Name, "601100000002", true)
	createLegacyMetaTestMessage(
		t,
		db,
		org.ID,
		account.Name,
		contact.ID,
		models.DirectionIncoming,
		"hello",
	)
	createLegacyMetaTestMessage(
		t,
		db,
		org.ID,
		account.Name,
		contact.ID,
		models.DirectionOutgoing,
		"welcome",
	)

	stats, err := channelapi.BackfillLegacyWhatsAppInbox(db, 1)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, stats.Accounts, 1)
	assert.Equal(t, 2, stats.Messages)
	assert.Equal(t, 2, stats.Linked)

	var shadow models.ChannelAccount
	require.NoError(t, db.Where(
		"organization_id = ? AND channel = ? AND provider = ?",
		org.ID,
		models.ChannelWhatsApp,
		channelapi.LegacyMetaProvider,
	).First(&shadow).Error)
	serialized, err := json.Marshal(shadow)
	require.NoError(t, err)
	shadowJSON := string(serialized)
	for _, secret := range []string{
		account.AccessToken,
		account.AppSecret,
		account.Pin,
		account.WebhookVerifyToken,
	} {
		assert.NotContains(t, shadowJSON, secret)
	}
	assert.NotContains(t, strings.ToLower(shadowJSON), "access_token")
	assert.NotContains(t, strings.ToLower(shadowJSON), "app_secret")

	var credentialCount int64
	require.NoError(t, db.Model(&models.ChannelCredential{}).
		Where("organization_id = ? AND channel_account_id = ?", org.ID, shadow.ID).
		Count(&credentialCount).Error)
	assert.Zero(t, credentialCount)
	var linkedCount int64
	require.NoError(t, db.Model(&models.Message{}).
		Where("organization_id = ? AND inbox_conversation_id IS NOT NULL", org.ID).
		Count(&linkedCount).Error)
	assert.EqualValues(t, 2, linkedCount)

	replayed, err := channelapi.BackfillLegacyWhatsAppInbox(db, 100)
	require.NoError(t, err)
	assert.Zero(t, replayed.Messages)
	assert.Zero(t, replayed.Linked)
}

func TestLegacyMetaAccountRenameFinalizerReconcilesShadowCreatedAfterStage(t *testing.T) {
	db := testutil.SetupTestDB(t)
	org := createLegacyMetaTestOrganization(t, db, "rename-race")
	account := createLegacyMetaTestAccount(t, db, org.ID, "Before Rename")
	const nextName = "After Rename"
	var shadowID uuid.UUID

	err := db.Transaction(func(tx *gorm.DB) error {
		prepared, err := channelapi.StageLegacyMetaWhatsAppAccountRename(
			tx,
			org.ID,
			account.ID,
			account.Name,
			nextName,
		)
		if err != nil {
			return err
		}
		if prepared {
			return errors.New("rename unexpectedly found a legacy shadow")
		}
		result := tx.Model(&models.WhatsAppAccount{}).
			Where("id = ? AND organization_id = ? AND name = ?", account.ID, org.ID, account.Name).
			Update("name", nextName)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return errors.New("established account rename was not applied")
		}

		shadow := &models.ChannelAccount{
			BaseModel:         models.BaseModel{ID: uuid.New()},
			OrganizationID:    org.ID,
			Channel:           models.ChannelWhatsApp,
			Provider:          channelapi.LegacyMetaProvider,
			Name:              "stale pre-rename projection",
			ExternalAccountID: "legacy-account:" + account.ID.String(),
			Status:            models.ChannelAccountStatusActive,
			Capabilities:      models.JSONB{"text": true, "replies": true, "service_window": true},
			Config:            models.JSONB{"legacy_read_only": true, "outbound_enabled": false, "reply_route": "chat"},
			Metadata: models.JSONB{
				"legacy_account_id":   account.ID.String(),
				"legacy_account_name": account.Name,
			},
		}
		if err := tx.Create(shadow).Error; err != nil {
			return err
		}
		shadowID = shadow.ID
		return channelapi.FinalizeLegacyMetaWhatsAppAccountRename(
			tx,
			org.ID,
			account.ID,
			account.Name,
			nextName,
		)
	})
	require.NoError(t, err)

	var storedAccount models.WhatsAppAccount
	require.NoError(t, db.Where("id = ? AND organization_id = ?", account.ID, org.ID).
		First(&storedAccount).Error)
	assert.Equal(t, nextName, storedAccount.Name)
	var storedShadow models.ChannelAccount
	require.NoError(t, db.Where("id = ? AND organization_id = ?", shadowID, org.ID).
		First(&storedShadow).Error)
	assert.Equal(t, nextName, storedShadow.Metadata["legacy_account_name"])
	assert.Contains(t, storedShadow.Name, nextName)
}

func TestLegacyMetaMirrorRejectsAccountAndExistingLinkConflicts(t *testing.T) {
	db := testutil.SetupTestDB(t)

	org := createLegacyMetaTestOrganization(t, db, "conflict")
	account := createLegacyMetaTestAccount(t, db, org.ID, "Expected")
	contact := createLegacyMetaTestContact(t, db, org.ID, account.Name, "601100000003", true)
	message := createLegacyMetaTestMessage(
		t,
		db,
		org.ID,
		account.Name,
		contact.ID,
		models.DirectionIncoming,
		"conflict",
	)

	wrongAccount := legacyMetaRef(account)
	wrongAccount.Name = "Another account"
	_, err := channelapi.MirrorLegacyWhatsAppMessage(db, wrongAccount, message.ID)
	require.ErrorIs(t, err, channelapi.ErrLegacyMetaBridgeConflict)

	first, err := channelapi.MirrorLegacyWhatsAppMessage(db, legacyMetaRef(account), message.ID)
	require.NoError(t, err)
	secondConversation := models.InboxConversation{
		OrganizationID:         org.ID,
		ChannelAccountID:       first.ChannelAccountID,
		ContactID:              contact.ID,
		Channel:                models.ChannelWhatsApp,
		ExternalConversationID: "manual-conflict:" + uuid.NewString(),
		Status:                 models.InboxConversationStatusOpen,
		OpenedAt:               message.CreatedAt,
		Config:                 models.JSONB{},
		Metadata:               models.JSONB{},
	}
	require.NoError(t, db.Create(&secondConversation).Error)
	require.NoError(t, db.Model(&models.Message{}).
		Where("id = ? AND organization_id = ?", message.ID, org.ID).
		Update("inbox_conversation_id", secondConversation.ID).Error)

	_, err = channelapi.MirrorLegacyWhatsAppMessage(db, legacyMetaRef(account), message.ID)
	require.ErrorIs(t, err, channelapi.ErrLegacyMetaBridgeConflict)
}

func TestLegacyMetaDelayedMirrorKeepsProviderWindowAndIngestionPreview(t *testing.T) {
	db := testutil.SetupTestDB(t)
	organization := createLegacyMetaTestOrganization(t, db, "delayed-order")
	account := createLegacyMetaTestAccount(t, db, organization.ID, "Delayed")
	contact := createLegacyMetaTestContact(
		t, db, organization.ID, account.Name, "601100000004", false,
	)
	newerProviderAt := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Microsecond)
	olderProviderAt := newerProviderAt.Add(-3 * time.Hour)
	createAt := func(providerAt time.Time, content string) models.Message {
		t.Helper()
		message := models.Message{
			BaseModel: models.BaseModel{
				ID:        uuid.New(),
				CreatedAt: providerAt,
				UpdatedAt: providerAt,
			},
			OrganizationID:    organization.ID,
			WhatsAppAccount:   account.Name,
			ContactID:         contact.ID,
			Direction:         models.DirectionIncoming,
			MessageType:       models.MessageTypeText,
			Content:           content,
			Status:            models.MessageStatusReceived,
			WhatsAppMessageID: "delayed-legacy-" + uuid.NewString(),
			Metadata:          models.JSONB{},
		}
		require.NoError(t, db.Create(&message).Error)
		return message
	}
	newer := createAt(newerProviderAt, "Newest provider preview")
	older := createAt(olderProviderAt, "Delayed older provider message")

	newerResult, err := channelapi.MirrorLegacyWhatsAppMessage(
		db, legacyMetaRef(account), newer.ID,
	)
	require.NoError(t, err)
	olderResult, err := channelapi.MirrorLegacyWhatsAppMessage(
		db, legacyMetaRef(account), older.ID,
	)
	require.NoError(t, err)
	assert.Equal(t, newerResult.ConversationID, olderResult.ConversationID)

	var conversation models.InboxConversation
	require.NoError(t, db.First(&conversation, "id = ?", newerResult.ConversationID).Error)
	assert.Equal(t, "Delayed older provider message", conversation.LastMessagePreview)
	require.NotNil(t, conversation.ServiceWindowEndsAt)
	assert.Equal(t, newerProviderAt.Add(24*time.Hour), conversation.ServiceWindowEndsAt.UTC())

	var linked []models.Message
	require.NoError(t, db.Where(
		"organization_id = ? AND inbox_conversation_id = ?",
		organization.ID,
		conversation.ID,
	).Order("COALESCE(ingested_at, created_at), id").Find(&linked).Error)
	require.Len(t, linked, 2)
	assert.Equal(t, newer.ID, linked[0].ID)
	assert.Equal(t, older.ID, linked[1].ID)
	require.NotNil(t, conversation.LastMessageAt)
	assert.Equal(t, linked[1].EffectiveIngestedAt(), conversation.LastMessageAt.UTC())
}

func TestAIBookingAuthorityIsStrictAndEpochBound(t *testing.T) {
	epoch := time.Date(2026, 1, 1, 0, 0, 0, 123456000, time.UTC)
	newAccount := func() *models.ChannelAccount {
		return &models.ChannelAccount{
			BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: uuid.New(),
			Status: models.ChannelAccountStatusActive,
			Config: models.JSONB{
				models.ChannelConfigAIBookingEnabled:   true,
				models.ChannelConfigAIBookingRevision:  "11111111-1111-4111-8111-111111111111",
				models.ChannelConfigAIBookingEnabledAt: epoch.Format(time.RFC3339Nano),
			},
		}
	}
	account := newAccount()
	_, ok := models.AIBookingAuthorityForInbound(account, epoch)
	require.True(t, ok)
	_, ok = models.AIBookingAuthorityForInbound(account, epoch.Add(-time.Nanosecond))
	require.False(t, ok)
	for _, test := range []struct {
		name   string
		mutate func(*models.ChannelAccount)
	}{
		{"absent", func(a *models.ChannelAccount) { delete(a.Config, models.ChannelConfigAIBookingEnabled) }},
		{"string", func(a *models.ChannelAccount) { a.Config[models.ChannelConfigAIBookingEnabled] = "true" }},
		{"number", func(a *models.ChannelAccount) { a.Config[models.ChannelConfigAIBookingEnabled] = 1 }},
		{"nil", func(a *models.ChannelAccount) { a.Config[models.ChannelConfigAIBookingEnabled] = nil }},
		{"off", func(a *models.ChannelAccount) { a.Config[models.ChannelConfigAIBookingEnabled] = false }},
		{"revision-empty", func(a *models.ChannelAccount) { a.Config[models.ChannelConfigAIBookingRevision] = "" }},
		{"revision-zero", func(a *models.ChannelAccount) { a.Config[models.ChannelConfigAIBookingRevision] = uuid.Nil.String() }},
		{"revision-spaced", func(a *models.ChannelAccount) {
			a.Config[models.ChannelConfigAIBookingRevision] = " 11111111-1111-4111-8111-111111111111"
		}},
		{"epoch-absent", func(a *models.ChannelAccount) { delete(a.Config, models.ChannelConfigAIBookingEnabledAt) }},
		{"epoch-offset", func(a *models.ChannelAccount) {
			a.Config[models.ChannelConfigAIBookingEnabledAt] = "2026-01-01T00:00:00+00:00"
		}},
		{"epoch-noncanonical", func(a *models.ChannelAccount) {
			a.Config[models.ChannelConfigAIBookingEnabledAt] = "2026-01-01T00:00:00.000Z"
		}},
		{"deleted", func(a *models.ChannelAccount) { a.DeletedAt = gorm.DeletedAt{Time: epoch, Valid: true} }},
		{"inactive", func(a *models.ChannelAccount) { a.Status = models.ChannelAccountStatusSuspended }},
		{"no-identity", func(a *models.ChannelAccount) { a.ID = uuid.Nil }},
	} {
		t.Run(test.name, func(t *testing.T) {
			candidate := newAccount()
			test.mutate(candidate)
			_, ok := models.AIBookingAuthority(candidate)
			require.False(t, ok)
		})
	}
	account.UpdatedAt = epoch.Add(time.Hour)
	_, ok = models.AIBookingAuthority(account)
	require.True(t, ok, "volatile UpdatedAt is not a booking revision")
}

func TestLegacyMetaAIBookingMirrorPreservesOnlyCurrentLiveRoute(t *testing.T) {
	db := testutil.SetupTestDB(t)
	for _, mutation := range []string{"ordinary", "rename", "route", "business-route", "revive", "recreate", "suspend", "forged-supplied-route"} {
		t.Run(mutation, func(t *testing.T) {
			org := createLegacyMetaTestOrganization(t, db, "ai-booking-"+mutation)
			native := createLegacyMetaTestAccount(t, db, org.ID, "Native "+mutation)
			shadow, err := channelapi.EnsureLegacyMetaWhatsAppAccount(db, legacyMetaRef(native))
			require.NoError(t, err)
			require.Equal(t, false, shadow.Config[models.ChannelConfigAIBookingEnabled])
			revision := uuid.NewString()
			enabledAt := time.Now().UTC().Add(-time.Minute).Format(time.RFC3339Nano)
			shadow.Config[models.ChannelConfigAIBookingEnabled] = true
			shadow.Config[models.ChannelConfigAIBookingRevision] = revision
			shadow.Config[models.ChannelConfigAIBookingEnabledAt] = enabledAt
			shadow.Config[models.ChannelConfigAIBookingRouteBinding] = channelapi.LegacyMetaAIBookingRouteBinding(&native)
			shadow.Config["arbitrary_untrusted"] = true
			require.NoError(t, db.Model(shadow).Update("config", shadow.Config).Error)
			originalID := shadow.ID
			switch mutation {
			case "rename":
				native.Name = "Renamed " + mutation
				require.NoError(t, db.Model(&native).Update("name", native.Name).Error)
			case "route":
				native.PhoneID += "-changed"
				require.NoError(t, db.Model(&native).Update("phone_id", native.PhoneID).Error)
				_, allowed := channelapi.LegacyMetaAIBookingAuthority(shadow, &native)
				require.False(t, allowed, "routing mismatch fails even before the next mirror")
			case "business-route":
				native.BusinessID += "-changed"
				require.NoError(t, db.Model(&native).Update("business_id", native.BusinessID).Error)
				_, allowed := channelapi.LegacyMetaAIBookingAuthority(shadow, &native)
				require.False(t, allowed, "business routing mismatch fails before the next mirror")
			case "revive":
				require.NoError(t, db.Delete(shadow).Error)
			case "recreate":
				require.NoError(t, db.Unscoped().Delete(shadow).Error)
			case "suspend":
				native.Status = "inactive"
				require.NoError(t, db.Model(&native).Update("status", native.Status).Error)
			}
			ref := legacyMetaRef(native)
			if mutation == "forged-supplied-route" {
				ref.PhoneID = "forged-phone"
				ref.BusinessID = "forged-business"
			}
			refreshed, err := channelapi.EnsureLegacyMetaWhatsAppAccount(db, ref)
			require.NoError(t, err)
			require.NotContains(t, refreshed.Config, "arbitrary_untrusted")
			require.Equal(t, false, refreshed.Config["outbound_enabled"])
			require.Equal(t, true, refreshed.Config["legacy_read_only"])
			_, allowed := channelapi.LegacyMetaAIBookingAuthority(refreshed, &native)
			if mutation == "ordinary" || mutation == "rename" || mutation == "forged-supplied-route" {
				require.True(t, allowed)
				require.Equal(t, revision, refreshed.Config[models.ChannelConfigAIBookingRevision])
				require.Equal(t, enabledAt, refreshed.Config[models.ChannelConfigAIBookingEnabledAt])
				require.Equal(t, originalID, refreshed.ID)
				require.Equal(t, channelapi.LegacyMetaAIBookingRouteBinding(&native),
					refreshed.Config[models.ChannelConfigAIBookingRouteBinding], "only persisted routing facts are authoritative")
			} else {
				require.False(t, allowed)
				require.Equal(t, false, refreshed.Config[models.ChannelConfigAIBookingEnabled])
				require.NotContains(t, refreshed.Config, models.ChannelConfigAIBookingEnabledAt)
				if mutation == "recreate" {
					require.NotEqual(t, originalID, refreshed.ID)
				}
				if mutation == "revive" {
					require.Equal(t, originalID, refreshed.ID, "revival retains the original shadow identity")
					require.NotEqual(t, revision, refreshed.Config[models.ChannelConfigAIBookingRevision])
				}
			}
			var rows int64
			require.NoError(t, db.Unscoped().Model(&models.ChannelAccount{}).
				Where("organization_id = ? AND external_account_id = ?", org.ID, shadow.ExternalAccountID).
				Count(&rows).Error)
			require.EqualValues(t, 1, rows, "ordinary refresh/revival must not insert another shadow")
		})
	}

	for _, shape := range []string{"historical-and-live", "historical-only"} {
		t.Run("ambiguous-"+shape, func(t *testing.T) {
			org := createLegacyMetaTestOrganization(t, db, "ambiguous-"+shape)
			native := createLegacyMetaTestAccount(t, db, org.ID, "Ambiguous "+shape)
			retained, err := channelapi.EnsureLegacyMetaWhatsAppAccount(db, legacyMetaRef(native))
			require.NoError(t, err)
			require.NoError(t, db.Delete(retained).Error)
			replacement := *retained
			replacement.BaseModel = models.BaseModel{ID: uuid.New()}
			require.NoError(t, db.Create(&replacement).Error)
			if shape == "historical-only" {
				require.NoError(t, db.Delete(&replacement).Error)
			}
			var before, after []models.ChannelAccount
			query := func() *gorm.DB {
				return db.Unscoped().Where("organization_id = ? AND external_account_id = ?", org.ID, retained.ExternalAccountID).Order("id")
			}
			require.NoError(t, query().Find(&before).Error)
			require.Len(t, before, 2)
			err = db.Transaction(func(tx *gorm.DB) error {
				_, err := channelapi.EnsureLegacyMetaWhatsAppAccount(tx, legacyMetaRef(native))
				return err
			})
			require.ErrorIs(t, err, channelapi.ErrLegacyMetaBridgeConflict)
			require.NoError(t, query().Find(&after).Error)
			require.Equal(t, before, after, "ambiguous retained/live rows must remain byte-for-field unchanged")
		})
	}

	for _, lifecycle := range []string{"fresh", "revive"} {
		t.Run("concurrent-"+lifecycle, func(t *testing.T) {
			org := createLegacyMetaTestOrganization(t, db, "concurrent-"+lifecycle)
			native := createLegacyMetaTestAccount(t, db, org.ID, "Concurrent "+lifecycle)
			var retainedID uuid.UUID
			var oldRevision string
			if lifecycle == "revive" {
				retained, err := channelapi.EnsureLegacyMetaWhatsAppAccount(db, legacyMetaRef(native))
				require.NoError(t, err)
				retainedID, oldRevision = retained.ID, uuid.NewString()
				retained.Config[models.ChannelConfigAIBookingEnabled] = true
				retained.Config[models.ChannelConfigAIBookingRevision] = oldRevision
				retained.Config[models.ChannelConfigAIBookingEnabledAt] = time.Now().UTC().Add(-time.Minute).Format(time.RFC3339Nano)
				retained.Config[models.ChannelConfigAIBookingRouteBinding] = channelapi.LegacyMetaAIBookingRouteBinding(&native)
				require.NoError(t, db.Model(retained).Update("config", retained.Config).Error)
				require.NoError(t, db.Delete(retained).Error)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			holder := db.WithContext(ctx).Begin()
			require.NoError(t, holder.Error)
			defer holder.Rollback()
			first, err := channelapi.EnsureLegacyMetaWhatsAppAccount(holder, legacyMetaRef(native))
			require.NoError(t, err)
			var holderPID int
			require.NoError(t, holder.Session(&gorm.Session{NewDB: true}).Raw("SELECT pg_backend_pid()").Scan(&holderPID).Error)
			type outcome struct {
				shadow *models.ChannelAccount
				err    error
			}
			started := make(chan int, 1)
			done := make(chan outcome, 1)
			consumed := false
			go func() {
				var second *models.ChannelAccount
				err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
					var pid int
					// A fresh statement retains this transaction's pinned connection.
					if err := tx.Session(&gorm.Session{NewDB: true}).Raw("SELECT pg_backend_pid()").Scan(&pid).Error; err != nil {
						return err
					}
					started <- pid
					var err error
					second, err = channelapi.EnsureLegacyMetaWhatsAppAccount(tx, legacyMetaRef(native))
					return err
				})
				done <- outcome{shadow: second, err: err}
			}()
			defer func() {
				_ = holder.Rollback().Error
				cancel()
				if !consumed {
					select {
					case <-done:
					case <-time.After(5 * time.Second):
						t.Error("concurrent mirror did not terminate after cancellation")
					}
				}
			}()
			var workerPID int
			select {
			case workerPID = <-started:
			case early := <-done:
				consumed = true
				t.Fatalf("concurrent mirror stopped before announcing its transaction: %v", early.err)
			case <-time.After(5 * time.Second):
				t.Fatal("concurrent mirror did not start")
			}
			require.Eventually(t, func() bool {
				var blocked bool
				return db.WithContext(ctx).Raw("SELECT ? = ANY(pg_blocking_pids(?))", holderPID, workerPID).Scan(&blocked).Error == nil && blocked
			}, 5*time.Second, 10*time.Millisecond, "second mirror must wait on the exact holder transaction")
			require.NoError(t, holder.Commit().Error)
			var second outcome
			select {
			case second = <-done:
				consumed = true
			case <-time.After(5 * time.Second):
				t.Fatal("concurrent mirror did not finish after the holder committed")
			}
			require.NoError(t, second.err)
			require.NotNil(t, second.shadow)
			require.Equal(t, first.ID, second.shadow.ID, "concurrent mirrors converge on the same retained identity")
			_, allowed := channelapi.LegacyMetaAIBookingAuthority(second.shadow, &native)
			require.False(t, allowed, "fresh creation/revival never grants booking")
			require.Equal(t, false, second.shadow.Config[models.ChannelConfigAIBookingEnabled])
			require.NotContains(t, second.shadow.Config, models.ChannelConfigAIBookingEnabledAt)
			if lifecycle == "revive" {
				require.Equal(t, retainedID, second.shadow.ID)
				require.NotEqual(t, oldRevision, second.shadow.Config[models.ChannelConfigAIBookingRevision])
				require.Equal(t, first.Config[models.ChannelConfigAIBookingRevision], second.shadow.Config[models.ChannelConfigAIBookingRevision])
			}
			var rows int64
			require.NoError(t, db.Unscoped().Model(&models.ChannelAccount{}).
				Where("organization_id = ? AND external_account_id = ?", org.ID, first.ExternalAccountID).Count(&rows).Error)
			require.EqualValues(t, 1, rows)
		})
	}
}

func TestLegacyMetaAIBookingAuthorityRejectsIdentityAndRouteDrift(t *testing.T) {
	native := &models.WhatsAppAccount{
		BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: uuid.New(),
		PhoneID: "synthetic-phone", BusinessID: "synthetic-business", Name: "Native", Status: "active",
	}
	shadow := &models.ChannelAccount{
		BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: native.OrganizationID,
		Channel: models.ChannelWhatsApp, Provider: channelapi.LegacyMetaProvider,
		ExternalAccountID: "legacy-account:" + native.ID.String(), Status: models.ChannelAccountStatusActive,
		Metadata: models.JSONB{"legacy_account_id": native.ID.String()},
		Config: models.JSONB{
			"legacy_read_only": true, "outbound_enabled": false, "reply_route": "chat",
			models.ChannelConfigAIBookingEnabled:      true,
			models.ChannelConfigAIBookingRevision:     uuid.NewString(),
			models.ChannelConfigAIBookingEnabledAt:    "2026-01-01T00:00:00Z",
			models.ChannelConfigAIBookingRouteBinding: channelapi.LegacyMetaAIBookingRouteBinding(native),
		},
	}
	_, allowed := channelapi.LegacyMetaAIBookingAuthority(shadow, native)
	require.True(t, allowed)
	renamed := *native
	renamed.Name = "Renamed"
	_, allowed = channelapi.LegacyMetaAIBookingAuthority(shadow, &renamed)
	require.True(t, allowed)
	for _, change := range []struct {
		name  string
		apply func(*models.WhatsAppAccount)
	}{
		{"phone", func(a *models.WhatsAppAccount) { a.PhoneID += "-new" }},
		{"business", func(a *models.WhatsAppAccount) { a.BusinessID += "-new" }},
		{"native-id", func(a *models.WhatsAppAccount) { a.ID = uuid.New() }},
		{"tenant", func(a *models.WhatsAppAccount) { a.OrganizationID = uuid.New() }},
		{"deleted", func(a *models.WhatsAppAccount) { a.DeletedAt = gorm.DeletedAt{Time: time.Now(), Valid: true} }},
		{"inactive", func(a *models.WhatsAppAccount) { a.Status = "inactive" }},
	} {
		t.Run(change.name, func(t *testing.T) {
			candidate := *native
			change.apply(&candidate)
			_, allowed := channelapi.LegacyMetaAIBookingAuthority(shadow, &candidate)
			require.False(t, allowed)
		})
	}
	shadow.Config["outbound_enabled"] = true
	_, allowed = channelapi.LegacyMetaAIBookingAuthority(shadow, native)
	require.False(t, allowed, "booking cannot bless a generic outbound shadow")
}

func legacyMetaRef(account models.WhatsAppAccount) channelapi.LegacyMetaAccountRef {
	return channelapi.LegacyMetaAccountRef{
		ID:             account.ID,
		OrganizationID: account.OrganizationID,
		Name:           account.Name,
		Status:         account.Status,
	}
}

func createLegacyMetaTestOrganization(
	t *testing.T,
	db *gorm.DB,
	suffix string,
) models.Organization {
	t.Helper()
	organization := models.Organization{
		Name:     "Legacy Meta " + suffix,
		Slug:     "legacy-meta-" + suffix + "-" + uuid.NewString(),
		Settings: models.JSONB{},
	}
	require.NoError(t, db.Create(&organization).Error)
	return organization
}

func createLegacyMetaTestAccount(
	t *testing.T,
	db *gorm.DB,
	organizationID uuid.UUID,
	name string,
) models.WhatsAppAccount {
	t.Helper()
	unique := uuid.NewString()
	account := models.WhatsAppAccount{
		OrganizationID:     organizationID,
		Name:               name,
		AppID:              "app-" + unique,
		PhoneID:            "phone-" + unique,
		BusinessID:         "business-" + unique,
		AccessToken:        "secret-access-" + unique,
		AppSecret:          "secret-app-" + unique,
		WebhookVerifyToken: "secret-webhook-" + unique,
		APIVersion:         "v21.0",
		Status:             "active",
		Pin:                "secret-pin-" + unique,
	}
	require.NoError(t, db.Create(&account).Error)
	return account
}

func createLegacyMetaTestContact(
	t *testing.T,
	db *gorm.DB,
	organizationID uuid.UUID,
	accountName string,
	phone string,
	isRead bool,
) models.Contact {
	t.Helper()
	contact := models.Contact{
		OrganizationID:  organizationID,
		PhoneNumber:     phone,
		ProfileName:     "Test Contact",
		WhatsAppAccount: accountName,
		IsRead:          true,
		Tags:            models.JSONBArray{},
		Metadata:        models.JSONB{},
	}
	require.NoError(t, db.Create(&contact).Error)
	require.NoError(t, db.Model(&models.Contact{}).
		Where("id = ? AND organization_id = ?", contact.ID, organizationID).
		Update("is_read", isRead).Error)
	contact.IsRead = isRead
	return contact
}

func createLegacyMetaTestMessage(
	t *testing.T,
	db *gorm.DB,
	organizationID uuid.UUID,
	accountName string,
	contactID uuid.UUID,
	direction models.Direction,
	content string,
) models.Message {
	t.Helper()
	status := models.MessageStatusReceived
	if direction == models.DirectionOutgoing {
		status = models.MessageStatusSent
	}
	message := models.Message{
		OrganizationID:  organizationID,
		WhatsAppAccount: accountName,
		ContactID:       contactID,
		Direction:       direction,
		MessageType:     models.MessageTypeText,
		Content:         content,
		Status:          status,
		Metadata:        models.JSONB{},
	}
	require.NoError(t, db.Create(&message).Error)
	return message
}
