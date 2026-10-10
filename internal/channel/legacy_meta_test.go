package channel_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	mathrand "math/rand/v2"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	channelapi "github.com/shridarpatil/whatomate/internal/channel"
	"github.com/shridarpatil/whatomate/internal/database"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func TestEnsureLegacyMetaWhatsAppAccountCreatesOnlyCredentialFreeAccountShadow(t *testing.T) {
	db := testutil.SetupTestDB(t)
	testutil.TruncateTables(db)

	organization := createLegacyMetaTestOrganization(t, db, "account-only")
	otherOrganization := createLegacyMetaTestOrganization(t, db, "account-only-other")
	account := createLegacyMetaTestAccount(t, db, organization.ID, "Identity Review")

	shadow, err := ensureFencedLegacyMetaAccountForTest(db, legacyMetaRef(account))
	require.NoError(t, err)
	require.NotNil(t, shadow)
	assert.Equal(t, organization.ID, shadow.OrganizationID)
	assert.Equal(t, models.ChannelWhatsApp, shadow.Channel)
	assert.Equal(t, channelapi.LegacyMetaProvider, shadow.Provider)
	assert.Equal(t, false, shadow.Config["outbound_enabled"])
	assert.Equal(t, true, shadow.Config["legacy_read_only"])

	replayed, err := ensureFencedLegacyMetaAccountForTest(db, legacyMetaRef(account))
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
	_, err = ensureFencedLegacyMetaAccountForTest(db, wrongTenant)
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

// A rename keeps the shadow locked for the rest of its request transaction,
// which later writes organization-scoped rows such as the audit entry. It must
// therefore queue on the organization behind a Coexistence admission's policy
// fence instead of owning the shadow that admission takes next.
func TestLegacyMetaAccountRenameStageQueuesBehindAdmissionFence(t *testing.T) {
	db := testutil.SetupTestDB(t)
	org := createLegacyMetaTestOrganization(t, db, "rename-fence")
	suffix := uuid.NewString()[:8]
	account := createLegacyMetaTestAccount(t, db, org.ID, "Before Fence "+suffix)
	shadow, err := ensureFencedLegacyMetaAccountForTest(db, legacyMetaRef(account))
	require.NoError(t, err)
	nextName := "After Fence " + suffix

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	admission := db.WithContext(ctx).Begin()
	require.NoError(t, admission.Error)
	defer func() { _ = admission.Rollback().Error }()
	require.NoError(t, database.LockOrganizationPolicyScope(admission, org.ID))
	var admissionPID int
	require.NoError(t, admission.Session(&gorm.Session{NewDB: true}).
		Raw("SELECT pg_backend_pid()").Scan(&admissionPID).Error)

	staged := make(chan struct{})
	release := make(chan struct{})
	renamed := make(chan error, 1)
	go func() {
		renamed <- db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			_, stageErr := channelapi.StageLegacyMetaWhatsAppAccountRename(
				tx,
				org.ID,
				account.ID,
				account.Name,
				nextName,
			)
			close(staged)
			if stageErr != nil {
				return stageErr
			}
			// Keep the rename transaction open, as the request transaction does.
			<-release
			return nil
		})
	}()
	var releaseOnce sync.Once
	releaseRename := func() { releaseOnce.Do(func() { close(release) }) }
	defer func() {
		_ = admission.Rollback().Error
		releaseRename()
		cancel()
	}()

	// Either the stage is queued on the admission or it already returned.
	require.Eventually(t, func() bool {
		select {
		case <-staged:
			return true
		default:
		}
		var waiting bool
		return db.Raw(
			"SELECT EXISTS (SELECT 1 FROM pg_catalog.pg_stat_activity WHERE ? = ANY(pg_catalog.pg_blocking_pids(pid)))",
			admissionPID,
		).Scan(&waiting).Error == nil && waiting
	}, 10*time.Second, 10*time.Millisecond, "the rename never reached the admission fence")

	var probed models.ChannelAccount
	require.NoError(t, admission.Clauses(clause.Locking{Strength: "UPDATE", Options: "NOWAIT"}).
		Select("id").
		Where("id = ? AND organization_id = ?", shadow.ID, org.ID).
		First(&probed).Error,
		"the rename owned the shadow while the admission held its policy fence")
	require.NoError(t, admission.Commit().Error)

	select {
	case <-staged:
	case <-time.After(10 * time.Second):
		require.Fail(t, "rename stage did not continue after the admission committed")
	}
	releaseRename()
	select {
	case err := <-renamed:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		require.Fail(t, "rename transaction did not finish")
	}
	var storedShadow models.ChannelAccount
	require.NoError(t, db.Where("id = ? AND organization_id = ?", shadow.ID, org.ID).
		First(&storedShadow).Error)
	assert.Equal(t, nextName, storedShadow.Metadata["legacy_account_name"])
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
			shadow, err := ensureFencedLegacyMetaAccountForTest(db, legacyMetaRef(native))
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
			refreshed, err := ensureFencedLegacyMetaAccountForTest(db, ref)
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
			retained, err := ensureFencedLegacyMetaAccountForTest(db, legacyMetaRef(native))
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
				_, err := ensureFencedLegacyMetaAccountForTest(tx, legacyMetaRef(native))
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
				retained, err := ensureFencedLegacyMetaAccountForTest(db, legacyMetaRef(native))
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
			first, err := ensureFencedLegacyMetaAccountForTest(holder, legacyMetaRef(native))
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
					second, err = ensureFencedLegacyMetaAccountForTest(tx, legacyMetaRef(native))
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

// legacyMetaFenceFixture is one account whose shadow, identities and
// conversations already exist, with pending messages for each of its streams.
func legacyMetaFenceFixture(
	t *testing.T,
	db *gorm.DB,
	label string,
	streams, perStream int,
) (models.Organization, models.WhatsAppAccount, [][]uuid.UUID) {
	t.Helper()
	org := createLegacyMetaTestOrganization(t, db, label)
	account, pending := legacyMetaFenceAccount(t, db, org, label, streams, perStream)
	return org, account, pending
}

// legacyMetaFenceAccount adds such an account to an existing organization.
func legacyMetaFenceAccount(
	t *testing.T,
	db *gorm.DB,
	org models.Organization,
	label string,
	streams, perStream int,
) (models.WhatsAppAccount, [][]uuid.UUID) {
	t.Helper()
	account := createLegacyMetaTestAccount(t, db, org.ID, "Fence "+label+" "+uuid.NewString()[:8])
	pending := make([][]uuid.UUID, streams)
	for stream := range pending {
		contact := createLegacyMetaTestContact(t, db, org.ID, account.Name,
			"60"+testutil.NewTestGraphObjectID()[:10], true)
		warm := createLegacyMetaTestMessage(t, db, org.ID, account.Name, contact.ID,
			models.DirectionIncoming, "warm")
		_, err := channelapi.MirrorLegacyWhatsAppMessage(db, legacyMetaRef(account), warm.ID)
		require.NoError(t, err)
		batch := make([]models.Message, perStream)
		for index := range batch {
			batch[index] = models.Message{
				BaseModel:      models.BaseModel{ID: uuid.New()},
				OrganizationID: org.ID, WhatsAppAccount: account.Name, ContactID: contact.ID,
				Direction: models.DirectionIncoming, MessageType: models.MessageTypeText,
				Content: "pending", Status: models.MessageStatusReceived, Metadata: models.JSONB{},
				WhatsAppMessageID: "wamid.fence-" + uuid.NewString(),
			}
		}
		require.NoError(t, db.CreateInBatches(&batch, 200).Error)
		for _, message := range batch {
			pending[stream] = append(pending[stream], message.ID)
		}
	}
	return account, pending
}

// legacyMetaMirrorStream is one goroutine mirroring its messages in order.
type legacyMetaMirrorStream struct {
	ref        channelapi.LegacyMetaAccountRef
	messageIDs []uuid.UUID
}

// assertLegacyMetaMirrorStreamsLeavePolicyFenceAvailable runs the streams for
// a few seconds against a prober that repeatedly takes
// LockOrganizationPolicyScope, as inbound admissions do, and requires every
// acquisition within 1 s.
func assertLegacyMetaMirrorStreamsLeavePolicyFenceAvailable(
	t *testing.T,
	db *gorm.DB,
	organizationID uuid.UUID,
	streams []legacyMetaMirrorStream,
) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var (
		group        sync.WaitGroup
		mutex        sync.Mutex
		mirrorErrors []error
		fenceErrors  []error
		fenceWaits   []time.Duration
	)
	for _, stream := range streams {
		group.Add(1)
		go func(stream legacyMetaMirrorStream) {
			defer group.Done()
			for _, messageID := range stream.messageIDs {
				if ctx.Err() != nil {
					return
				}
				_, mirrorErr := channelapi.MirrorLegacyWhatsAppMessage(db, stream.ref, messageID)
				if mirrorErr != nil {
					mutex.Lock()
					mirrorErrors = append(mirrorErrors, mirrorErr)
					mutex.Unlock()
				}
			}
		}(stream)
	}
	group.Add(1)
	go func() {
		defer group.Done()
		for ctx.Err() == nil {
			waited, fenceErr := lockLegacyMetaPolicyFenceWithin(
				context.Background(), db, organizationID, time.Second, 2*time.Millisecond)
			mutex.Lock()
			if fenceErr != nil {
				fenceErrors = append(fenceErrors, fenceErr)
			} else {
				fenceWaits = append(fenceWaits, waited)
			}
			mutex.Unlock()
			time.Sleep(5 * time.Millisecond)
		}
	}()
	group.Wait()

	require.Empty(t, fenceErrors, "the policy fence starved behind mirrors")
	require.Empty(t, mirrorErrors)
	require.GreaterOrEqual(t, len(fenceWaits), 20)
	var longest time.Duration
	for _, waited := range fenceWaits {
		longest = max(longest, waited)
	}
	assert.Less(t, longest, time.Second)
	var linked int64
	require.NoError(t, db.Model(&models.Message{}).
		Where("organization_id = ? AND content = 'pending' AND inbox_conversation_id IS NOT NULL", organizationID).
		Count(&linked).Error)
	assert.Positive(t, linked)
}

// lockLegacyMetaPolicyFenceWithin takes LockOrganizationPolicyScope with a
// lock_timeout, so a starved fence fails with 55P03 instead of hanging.
func lockLegacyMetaPolicyFenceWithin(
	ctx context.Context,
	db *gorm.DB,
	organizationID uuid.UUID,
	timeout time.Duration,
	hold time.Duration,
) (time.Duration, error) {
	started := time.Now()
	var waited time.Duration
	err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec(fmt.Sprintf("SET LOCAL lock_timeout = '%dms'", timeout.Milliseconds())).Error; err != nil {
			return err
		}
		if err := database.LockOrganizationPolicyScope(tx, organizationID); err != nil {
			return err
		}
		waited = time.Since(started)
		time.Sleep(hold)
		return nil
	})
	return waited, err
}

// A shadow that another transaction owns across a Meta call (a strict reply's
// provider phase) may queue any number of mirrors, but they must queue holding
// no organization lock: PostgreSQL lets a new FOR SHARE overtake a waiting FOR
// UPDATE, so SHARE-holding waiters would keep every policy fence in the tenant
// (each inbound admission's LockOrganizationPolicyScope) out until the owner
// commits.
func TestLegacyMetaMirrorsQueuedOnBusyShadowLeavePolicyFenceAvailable(t *testing.T) {
	db := testutil.SetupTestDB(t)
	org, account, pending := legacyMetaFenceFixture(t, db, "busy-shadow", 2, 1)
	shadow, err := ensureFencedLegacyMetaAccountForTest(db, legacyMetaRef(account))
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	owner := db.WithContext(ctx).Begin()
	require.NoError(t, owner.Error)
	defer func() { _ = owner.Rollback().Error }()
	var owned models.ChannelAccount
	require.NoError(t, owner.Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("id = ? AND organization_id = ?", shadow.ID, org.ID).
		First(&owned).Error)

	mirrored := make(chan error, len(pending))
	var mirrorPIDs []int
	for _, stream := range pending {
		tx := db.WithContext(ctx).Begin()
		require.NoError(t, tx.Error)
		var pid int
		require.NoError(t, tx.Raw("SELECT pg_backend_pid()").Scan(&pid).Error)
		mirrorPIDs = append(mirrorPIDs, pid)
		go func(messageID uuid.UUID) {
			defer func() { _ = tx.Rollback().Error }()
			_, mirrorErr := channelapi.MirrorLegacyWhatsAppMessage(tx, legacyMetaRef(account), messageID)
			if mirrorErr == nil {
				mirrorErr = tx.Commit().Error
			}
			mirrored <- mirrorErr
		}(stream[0])
	}
	collected := 0
	defer func() {
		_ = owner.Rollback().Error
		for ; collected < len(pending); collected++ {
			select {
			case <-mirrored:
			case <-time.After(10 * time.Second):
				t.Error("queued mirror did not terminate")
				return
			}
		}
	}()
	require.Eventually(t, func() bool {
		var queued int64
		return db.Raw(
			`SELECT count(*) FROM pg_catalog.pg_stat_activity
			  WHERE datname = pg_catalog.current_database()
			    AND wait_event_type = 'Lock'
			    AND query ILIKE '%channel_accounts%'
      AND pid IN ?`, mirrorPIDs,
		).Scan(&queued).Error == nil && queued == int64(len(pending))
	}, 10*time.Second, 10*time.Millisecond, "both mirrors must queue on the busy shadow")

	waited, err := lockLegacyMetaPolicyFenceWithin(ctx, db, org.ID, time.Second, 0)
	require.NoError(t, err, "queued mirrors held the organization row")
	assert.Less(t, waited, time.Second)
	require.NoError(t, owner.Commit().Error)
	for ; collected < len(pending); collected++ {
		select {
		case mirrorErr := <-mirrored:
			require.NoError(t, mirrorErr)
		case <-time.After(10 * time.Second):
			require.Fail(t, "queued mirror did not finish after the owner committed")
		}
	}
	var unlinked int64
	require.NoError(t, db.Model(&models.Message{}).
		Where("organization_id = ? AND inbox_conversation_id IS NULL", org.ID).
		Count(&unlinked).Error)
	assert.Zero(t, unlinked)
}

// Two mirror streams on one account must leave the policy fence available. A
// stream that waited for the shadow while holding the organization FOR SHARE
// let the next mirror's SHARE overtake LockOrganizationPolicyScope every time,
// so with two or more streams the fence starved for as long as they ran.
func TestLegacyMetaConcurrentMirrorStreamsLeavePolicyFenceAvailable(t *testing.T) {
	db := testutil.SetupTestDB(t)
	org, account, pending := legacyMetaFenceFixture(t, db, "mirror-streams", 2, 400)
	streams := make([]legacyMetaMirrorStream, 0, len(pending))
	for _, messageIDs := range pending {
		streams = append(streams, legacyMetaMirrorStream{ref: legacyMetaRef(account), messageIDs: messageIDs})
	}
	assertLegacyMetaMirrorStreamsLeavePolicyFenceAvailable(t, db, org.ID, streams)
}

// Mirrors on different accounts never meet on a shadow, so their FOR SHARE
// windows on the organization overlap. Only the policy fence's shared advisory
// queue keeps those windows from overtaking a waiting
// LockOrganizationPolicyScope for as long as the streams run.
func TestLegacyMetaMirrorStreamsOnDifferentAccountsLeavePolicyFenceAvailable(t *testing.T) {
	db := testutil.SetupTestDB(t)
	org := createLegacyMetaTestOrganization(t, db, "cross-account")
	var streams []legacyMetaMirrorStream
	for index := 0; index < 4; index++ {
		account, pending := legacyMetaFenceAccount(t, db, org, fmt.Sprintf("cross-%d", index), 2, 400)
		for _, messageIDs := range pending {
			streams = append(streams, legacyMetaMirrorStream{ref: legacyMetaRef(account), messageIDs: messageIDs})
		}
	}
	assertLegacyMetaMirrorStreamsLeavePolicyFenceAvailable(t, db, org.ID, streams)
}

// A mirror that starts on another account while a policy fence waits must
// queue behind that fence. PostgreSQL grants a new FOR SHARE without queueing
// behind a waiting FOR UPDATE, so a mirror that went straight to the
// organization row would hold the fence out for as long as such mirrors keep
// overlapping. The queue is bounded in production; a long bound here keeps the
// ordering deterministic on a slow host.
func TestLegacyMetaMirrorOnAnotherAccountQueuesBehindWaitingPolicyFence(t *testing.T) {
	defer channelapi.SetLegacyMetaPolicyFenceQueueWaitForTest(20 * time.Second)()
	db := testutil.SetupTestDB(t)
	org := createLegacyMetaTestOrganization(t, db, "fence-queue")
	first, firstPending := legacyMetaFenceAccount(t, db, org, "fence-first", 1, 1)
	second, secondPending := legacyMetaFenceAccount(t, db, org, "fence-second", 1, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	backendPID := func(tx *gorm.DB) int {
		var pid int
		require.NoError(t, tx.Session(&gorm.Session{NewDB: true}).Raw("SELECT pg_backend_pid()").Scan(&pid).Error)
		return pid
	}
	blocked := func(pid int) bool {
		var waiting bool
		return db.Raw("SELECT cardinality(pg_catalog.pg_blocking_pids(?)) > 0", pid).
			Scan(&waiting).Error == nil && waiting
	}

	// A mirror that has finished its work but not yet committed.
	holder := db.WithContext(ctx).Begin()
	require.NoError(t, holder.Error)
	defer func() { _ = holder.Rollback().Error }()
	_, err := channelapi.MirrorLegacyWhatsAppMessage(holder, legacyMetaRef(first), firstPending[0][0])
	require.NoError(t, err)

	// An inbound admission's policy fence queues behind it.
	fence := db.WithContext(ctx).Begin()
	require.NoError(t, fence.Error)
	defer func() { _ = fence.Rollback().Error }()
	require.NoError(t, fence.Exec("SET LOCAL lock_timeout = '5s'").Error)
	fencePID := backendPID(fence)
	fenced := make(chan error, 1)
	go func() { fenced <- database.LockOrganizationPolicyScope(fence, org.ID) }()
	require.Eventually(t, func() bool { return blocked(fencePID) },
		10*time.Second, 10*time.Millisecond, "the fence must wait for the open mirror")

	// A mirror on the other account starts while the fence waits.
	late := db.WithContext(ctx).Begin()
	require.NoError(t, late.Error)
	defer func() { _ = late.Rollback().Error }()
	latePID := backendPID(late)
	lateDone := make(chan error, 1)
	go func() {
		_, mirrorErr := channelapi.MirrorLegacyWhatsAppMessage(late, legacyMetaRef(second), secondPending[0][0])
		lateDone <- mirrorErr
	}()
	// It polls for the fence key, so it shows no lock wait; its last statement
	// is the shared try-lock.
	polling := func() bool {
		var query string
		return db.Raw("SELECT query FROM pg_catalog.pg_stat_activity WHERE pid = ?", latePID).
			Scan(&query).Error == nil && strings.Contains(query, "pg_try_advisory_xact_lock_shared")
	}
	deadline := time.After(10 * time.Second)
	for !polling() {
		select {
		case <-lateDone:
			require.Fail(t, "the later mirror overtook the waiting policy fence")
		case <-deadline:
			require.Fail(t, "the later mirror never queued for the fence")
		case <-time.After(10 * time.Millisecond):
		}
	}

	require.NoError(t, holder.Commit().Error)
	select {
	case fenceErr := <-fenced:
		require.NoError(t, fenceErr, "the fence must be next once the open mirror commits")
	case <-time.After(10 * time.Second):
		require.Fail(t, "the fence did not acquire after the open mirror committed")
	}
	select {
	case <-lateDone:
		require.Fail(t, "the later mirror finished before the fence it queued behind")
	default:
	}
	require.NoError(t, fence.Commit().Error)
	select {
	case mirrorErr := <-lateDone:
		require.NoError(t, mirrorErr)
	case <-time.After(10 * time.Second):
		require.Fail(t, "the later mirror did not continue after the fence committed")
	}
	require.NoError(t, late.Commit().Error)
	var unlinked int64
	require.NoError(t, db.Model(&models.Message{}).
		Where("organization_id = ? AND inbox_conversation_id IS NULL", org.ID).
		Count(&unlinked).Error)
	assert.Zero(t, unlinked)
}

// holdLegacyMetaPolicyFenceKeyShared stands for a sharer that joined the
// policy fence queue and has not committed yet. A policy fence that arrives
// next waits for it in the queue, which is not stuck.
func holdLegacyMetaPolicyFenceKeyShared(
	ctx context.Context,
	t *testing.T,
	db *gorm.DB,
	organizationID uuid.UUID,
) *gorm.DB {
	t.Helper()
	holder := db.WithContext(ctx).Begin()
	require.NoError(t, holder.Error)
	t.Cleanup(func() { _ = holder.Rollback().Error })
	require.NoError(t, holder.Exec(
		"SELECT pg_catalog.pg_advisory_xact_lock_shared(pg_catalog.hashtextextended(?, 0))",
		database.WhatsAppIdentityReviewContactSelectorFenceKey(organizationID),
	).Error)
	return holder
}

// queueLegacyMetaPolicyFence starts an inbound admission's policy fence on its
// own connection and returns once it waits.
func queueLegacyMetaPolicyFence(
	ctx context.Context,
	t *testing.T,
	db *gorm.DB,
	organizationID uuid.UUID,
) (*gorm.DB, <-chan error) {
	t.Helper()
	fence := db.WithContext(ctx).Begin()
	require.NoError(t, fence.Error)
	t.Cleanup(func() { _ = fence.Rollback().Error })
	var fencePID int
	require.NoError(t, fence.Session(&gorm.Session{NewDB: true}).Raw("SELECT pg_backend_pid()").Scan(&fencePID).Error)
	fenced := make(chan error, 1)
	go func() { fenced <- database.LockOrganizationPolicyScope(fence, organizationID) }()
	require.Eventually(t, func() bool {
		var waiting bool
		return db.Raw("SELECT cardinality(pg_catalog.pg_blocking_pids(?)) > 0", fencePID).
			Scan(&waiting).Error == nil && waiting
	}, 10*time.Second, 5*time.Millisecond, "the policy fence must wait")
	return fence, fenced
}

// holdLegacyMetaShadow takes the shadow FOR UPDATE on its own connection, as a
// strict reply does across its Meta call.
func holdLegacyMetaShadow(
	ctx context.Context,
	t *testing.T,
	db *gorm.DB,
	organizationID, shadowID uuid.UUID,
) *gorm.DB {
	t.Helper()
	owner := db.WithContext(ctx).Begin()
	require.NoError(t, owner.Error)
	t.Cleanup(func() { _ = owner.Rollback().Error })
	var owned models.ChannelAccount
	require.NoError(t, owner.Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("id = ? AND organization_id = ?", shadowID, organizationID).
		First(&owned).Error)
	return owner
}

type legacyMetaMirrored struct {
	err error
	at  time.Time
}

// startLegacyMetaMirrors mirrors the first pending message of every stream,
// each on its own goroutine, and waits until all of them wait for the shadow.
func startLegacyMetaMirrors(
	ctx context.Context,
	t *testing.T,
	db *gorm.DB,
	account models.WhatsAppAccount,
	pending [][]uuid.UUID,
	within time.Duration,
) <-chan legacyMetaMirrored {
	t.Helper()
	results := make(chan legacyMetaMirrored, len(pending))
	pids := make(chan int, len(pending))
	for _, stream := range pending {
		go func(messageID uuid.UUID) {
			mirrorErr := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
				var pid int
				if err := tx.Raw("SELECT pg_backend_pid()").Scan(&pid).Error; err != nil {
					return err
				}
				pids <- pid
				_, err := channelapi.MirrorLegacyWhatsAppMessage(tx, legacyMetaRef(account), messageID)
				return err
			})
			results <- legacyMetaMirrored{err: mirrorErr, at: time.Now()}
		}(stream[0])
	}
	mine := make([]int, 0, len(pending))
	for range pending {
		select {
		case pid := <-pids:
			mine = append(mine, pid)
		case <-time.After(within):
			require.Fail(t, "a mirror never started")
		}
	}
	require.Eventually(t, func() bool {
		var queued int64
		return db.Raw(
			`SELECT count(*) FROM pg_catalog.pg_stat_activity
			  WHERE pid IN ? AND wait_event_type = 'Lock' AND query ILIKE '%channel_accounts%'`,
			mine,
		).Scan(&queued).Error == nil && queued == int64(len(pending))
	}, within, 5*time.Millisecond, "every mirror must end up waiting for the busy shadow")
	return results
}

// While a policy fence waits behind a sharer that has not committed, a later
// sharer queues for it at most legacyMetaPolicyFenceQueueWait per call, however
// many rounds a busy shadow costs it. Here four mirrors spend that budget, then
// wait for one shadow; once it is released they must get through one after
// another within shadow work. With the bound applied to each wait instead, every
// mirror that inherited the shadow lost its round to the still-queued fence and
// queued for the whole bound again, so they got through one per bound.
func TestLegacyMetaSharersOnABusyShadowQueueForAWaitingFenceOncePerCall(t *testing.T) {
	const bound = time.Second
	defer channelapi.SetLegacyMetaPolicyFenceQueueWaitForTest(bound)()
	db := testutil.SetupTestDB(t)
	org, account, pending := legacyMetaFenceFixture(t, db, "convoy", 4, 1)
	shadow, err := ensureFencedLegacyMetaAccountForTest(db, legacyMetaRef(account))
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	sharer := holdLegacyMetaPolicyFenceKeyShared(ctx, t, db, org.ID)
	fence, fenced := queueLegacyMetaPolicyFence(ctx, t, db, org.ID)
	owner := holdLegacyMetaShadow(ctx, t, db, org.ID, shadow.ID)
	results := startLegacyMetaMirrors(ctx, t, db, account, pending, 20*time.Second)

	released := time.Now()
	require.NoError(t, owner.Commit().Error)
	for range pending {
		select {
		case result := <-results:
			require.NoError(t, result.err)
			assert.Less(t, result.at.Sub(released), bound,
				"a mirror queued for the policy fence again after its call's deadline")
		case <-time.After(30 * time.Second):
			require.Fail(t, "a mirror did not finish after the shadow was released")
		}
	}
	select {
	case fenceErr := <-fenced:
		require.Failf(t, "the policy fence acquired while a sharer still held the key", "error: %v", fenceErr)
	default:
	}
	require.NoError(t, sharer.Commit().Error)
	select {
	case fenceErr := <-fenced:
		require.NoError(t, fenceErr)
	case <-time.After(10 * time.Second):
		require.Fail(t, "the policy fence did not acquire after the sharer committed")
	}
	require.NoError(t, fence.Commit().Error)
	var unlinked int64
	require.NoError(t, db.Model(&models.Message{}).
		Where("organization_id = ? AND inbox_conversation_id IS NULL", org.ID).
		Count(&unlinked).Error)
	assert.Zero(t, unlinked)
}

// The longer member wait applies only to the fence that holds the key. A fence
// that still waits for the key behind a running member is served once that
// member commits, so a later mirror queues for the queue bound (250 ms) and
// then bypasses; it must not wait for the member wait (20 s here).
func TestLegacyMetaMirrorBehindAFenceWaitingForTheKeyQueuesOnlyForTheQueueBound(t *testing.T) {
	defer channelapi.SetLegacyMetaPolicyFenceMemberWaitForTest(20 * time.Second)()
	db := testutil.SetupTestDB(t)
	org, account, pending := legacyMetaFenceFixture(t, db, "key-waiter", 1, 1)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	sharer := holdLegacyMetaPolicyFenceKeyShared(ctx, t, db, org.ID)
	fence, fenced := queueLegacyMetaPolicyFence(ctx, t, db, org.ID)
	channelapi.ForgetLegacyMetaPolicyFenceStatesForTest()

	started := time.Now()
	_, err := channelapi.MirrorLegacyWhatsAppMessage(db.WithContext(ctx), legacyMetaRef(account), pending[0][0])
	require.NoError(t, err)
	took := time.Since(started)
	assert.GreaterOrEqual(t, took, 250*time.Millisecond, "the mirror must queue behind the waiting fence")
	assert.Less(t, took, 5*time.Second, "the mirror waited for the member wait behind a fence that waits for the key")

	require.NoError(t, sharer.Commit().Error)
	select {
	case fenceErr := <-fenced:
		require.NoError(t, fenceErr)
	case <-time.After(10 * time.Second):
		require.Fail(t, "the policy fence did not acquire after the sharer committed")
	}
	require.NoError(t, fence.Commit().Error)
}

// An AI attempt fence holds the organization FOR KEY SHARE across its model
// call while its goroutine may wait for a mirror on another connection, and an
// inbound admission's policy fence queues behind it. That fence is stuck behind
// a lock outside the queue, so sharers must not queue for it at all: four
// mirrors on one busy shadow reach the shadow at once and finish within shadow
// work, although the queue bound here is far longer than the test allows.
func TestLegacyMetaSharersOnABusyShadowSkipAFenceStuckBehindAnAttemptFence(t *testing.T) {
	defer channelapi.SetLegacyMetaPolicyFenceQueueWaitForTest(time.Minute)()
	db := testutil.SetupTestDB(t)
	org, account, pending := legacyMetaFenceFixture(t, db, "stuck-fence", 4, 1)
	shadow, err := ensureFencedLegacyMetaAccountForTest(db, legacyMetaRef(account))
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	attempt := db.WithContext(ctx).Begin()
	require.NoError(t, attempt.Error)
	t.Cleanup(func() { _ = attempt.Rollback().Error })
	require.NoError(t, database.LockOrganizationAIAttemptScope(attempt, org.ID))
	fence, fenced := queueLegacyMetaPolicyFence(ctx, t, db, org.ID)
	owner := holdLegacyMetaShadow(ctx, t, db, org.ID, shadow.ID)
	results := startLegacyMetaMirrors(ctx, t, db, account, pending, 10*time.Second)

	released := time.Now()
	require.NoError(t, owner.Commit().Error)
	for range pending {
		select {
		case result := <-results:
			require.NoError(t, result.err)
			assert.Less(t, result.at.Sub(released), 5*time.Second)
		case <-time.After(20 * time.Second):
			require.Fail(t, "a mirror queued for a policy fence stuck behind an attempt fence")
		}
	}
	select {
	case fenceErr := <-fenced:
		require.Failf(t, "the policy fence acquired while the attempt fence was held", "error: %v", fenceErr)
	default:
	}
	require.NoError(t, attempt.Commit().Error)
	select {
	case fenceErr := <-fenced:
		require.NoError(t, fenceErr)
	case <-time.After(10 * time.Second):
		require.Fail(t, "the policy fence did not acquire after the attempt fence committed")
	}
	require.NoError(t, fence.Commit().Error)
	var unlinked int64
	require.NoError(t, db.Model(&models.Message{}).
		Where("organization_id = ? AND inbox_conversation_id IS NULL", org.ID).
		Count(&unlinked).Error)
	assert.Zero(t, unlinked)
}

// A mirror that went past a policy fence stuck behind an AI attempt fence
// holds the bypass key. Once the attempt ends, the fence waits only for such
// mirrors, so it is waiting inside the queue again and a later mirror on
// another account must queue behind it. Taken for a lock outside the queue,
// each bypassing mirror would let the next one bypass too, and overlapping
// streams would keep the fence stuck for as long as they ran.
func TestLegacyMetaMirrorQueuesBehindAFenceThatWaitsOnlyForBypassingMirrors(t *testing.T) {
	defer channelapi.SetLegacyMetaPolicyFenceQueueWaitForTest(20 * time.Second)()
	db := testutil.SetupTestDB(t)
	org := createLegacyMetaTestOrganization(t, db, "bypass-member")
	first, firstPending := legacyMetaFenceAccount(t, db, org, "bypass-first", 1, 1)
	second, secondPending := legacyMetaFenceAccount(t, db, org, "bypass-second", 1, 1)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	backendPID := func(tx *gorm.DB) int {
		var pid int
		require.NoError(t, tx.Session(&gorm.Session{NewDB: true}).Raw("SELECT pg_backend_pid()").Scan(&pid).Error)
		return pid
	}

	attempt := db.WithContext(ctx).Begin()
	require.NoError(t, attempt.Error)
	t.Cleanup(func() { _ = attempt.Rollback().Error })
	require.NoError(t, database.LockOrganizationAIAttemptScope(attempt, org.ID))
	fence, fenced := queueLegacyMetaPolicyFence(ctx, t, db, org.ID)
	var fencePID int
	require.NoError(t, db.Raw(
		`SELECT pid FROM pg_catalog.pg_locks
		  WHERE locktype = 'advisory' AND mode = 'ExclusiveLock' AND granted
		    AND (classid::int8 << 32) | objid::int8 = pg_catalog.hashtextextended(?, 0)`,
		database.WhatsAppIdentityReviewContactSelectorFenceKey(org.ID),
	).Scan(&fencePID).Error)
	require.NotZero(t, fencePID, "the policy fence holds its key while it waits for the attempt fence")

	// This mirror goes past the stuck fence at once and stays open.
	bypassing := db.WithContext(ctx).Begin()
	require.NoError(t, bypassing.Error)
	t.Cleanup(func() { _ = bypassing.Rollback().Error })
	bypassingPID := backendPID(bypassing)
	_, err := channelapi.MirrorLegacyWhatsAppMessage(bypassing, legacyMetaRef(first), firstPending[0][0])
	require.NoError(t, err)

	require.NoError(t, attempt.Commit().Error)
	require.Eventually(t, func() bool {
		var blockers []int64
		return db.Raw("SELECT unnest(pg_catalog.pg_blocking_pids(?))", fencePID).Scan(&blockers).Error == nil &&
			len(blockers) == 1 && blockers[0] == int64(bypassingPID)
	}, 10*time.Second, 5*time.Millisecond, "the fence must now wait only for the bypassing mirror")

	// The bypassing mirror cached "stuck" a moment ago.
	channelapi.ForgetLegacyMetaPolicyFenceStatesForTest()
	late := db.WithContext(ctx).Begin()
	require.NoError(t, late.Error)
	t.Cleanup(func() { _ = late.Rollback().Error })
	latePID := backendPID(late)
	lateDone := make(chan error, 1)
	go func() {
		_, mirrorErr := channelapi.MirrorLegacyWhatsAppMessage(late, legacyMetaRef(second), secondPending[0][0])
		lateDone <- mirrorErr
	}()
	// It polls for the fence key, so it shows no lock wait; its last statement
	// is the shared try-lock.
	polling := func() bool {
		var query string
		return db.Raw("SELECT query FROM pg_catalog.pg_stat_activity WHERE pid = ?", latePID).
			Scan(&query).Error == nil && strings.Contains(query, "pg_try_advisory_xact_lock_shared")
	}
	queuedSince := time.Time{}
	for queuedSince.IsZero() || time.Since(queuedSince) < 200*time.Millisecond {
		select {
		case <-lateDone:
			require.Fail(t, "the later mirror went past a fence that waits only for a bypassing mirror")
		case <-ctx.Done():
			require.Fail(t, "the later mirror never queued for the fence")
		case <-time.After(5 * time.Millisecond):
		}
		if queuedSince.IsZero() && polling() {
			queuedSince = time.Now()
		}
	}

	require.NoError(t, bypassing.Commit().Error)
	select {
	case fenceErr := <-fenced:
		require.NoError(t, fenceErr, "the fence must be next once the bypassing mirror commits")
	case <-time.After(10 * time.Second):
		require.Fail(t, "the fence did not acquire after the bypassing mirror committed")
	}
	select {
	case <-lateDone:
		require.Fail(t, "the later mirror finished before the fence it queued behind")
	default:
	}
	require.NoError(t, fence.Commit().Error)
	select {
	case mirrorErr := <-lateDone:
		require.NoError(t, mirrorErr)
	case <-time.After(10 * time.Second):
		require.Fail(t, "the later mirror did not continue after the fence committed")
	}
	require.NoError(t, late.Commit().Error)
}

// At the production queue bounds, policy fences must keep acquiring while many
// mirror streams run on other accounts. Each mirror that finds a fence holding
// the key polls; with a short fixed bound, enough overlapping bypassers kept
// the organization row under FOR SHARE without a gap, and a waiting FOR UPDATE
// never got in (about N*H/(bound+H) bypassers hold it at any time). Here 32
// streams run over 8 accounts, closed loop and as Poisson arrivals, while
// admissions take LockOrganizationPolicyScope in turn; every admission must
// acquire within 1 s.
func TestLegacyMetaPolicyFenceKeepsAcquiringUnderManyCrossAccountMirrorStreams(t *testing.T) {
	const (
		accounts          = 8
		streamsPerAccount = 4
		messagesPerStream = 120
		runFor            = 3 * time.Second
		arrivalsPerSecond = 120
	)
	for _, openLoop := range []bool{false, true} {
		t.Run(fmt.Sprintf("open-loop=%v", openLoop), func(t *testing.T) {
			db := testutil.SetupTestDB(t)
			org := createLegacyMetaTestOrganization(t, db, "many-streams")
			var streams []legacyMetaMirrorStream
			for index := 0; index < accounts; index++ {
				account, pending := legacyMetaFenceAccount(
					t, db, org, fmt.Sprintf("many-%d", index), streamsPerAccount, messagesPerStream)
				for _, messageIDs := range pending {
					streams = append(streams, legacyMetaMirrorStream{ref: legacyMetaRef(account), messageIDs: messageIDs})
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), runFor)
			defer cancel()
			var (
				group        sync.WaitGroup
				mutex        sync.Mutex
				mirrorErrors []error
				mirrored     int
			)
			mirror := func(stream legacyMetaMirrorStream, messageID uuid.UUID) {
				_, mirrorErr := channelapi.MirrorLegacyWhatsAppMessage(db, stream.ref, messageID)
				mutex.Lock()
				defer mutex.Unlock()
				if mirrorErr != nil {
					mirrorErrors = append(mirrorErrors, mirrorErr)
				} else {
					mirrored++
				}
			}
			if openLoop {
				// Poisson arrivals, round robin over the streams; one stream
				// (one contact) mirrors one message at a time.
				locks := make([]sync.Mutex, len(streams))
				next := make([]int, len(streams))
				random := mathrand.New(mathrand.NewPCG(227, 6))
				group.Add(1)
				go func() {
					defer group.Done()
					for arrival := 0; ctx.Err() == nil; arrival++ {
						time.Sleep(time.Duration(random.ExpFloat64() / arrivalsPerSecond * float64(time.Second)))
						index := arrival % len(streams)
						if next[index] >= len(streams[index].messageIDs) {
							continue
						}
						messageID := streams[index].messageIDs[next[index]]
						next[index]++
						group.Add(1)
						go func(index int) {
							defer group.Done()
							locks[index].Lock()
							defer locks[index].Unlock()
							mirror(streams[index], messageID)
						}(index)
					}
				}()
			} else {
				for _, stream := range streams {
					group.Add(1)
					go func(stream legacyMetaMirrorStream) {
						defer group.Done()
						for _, messageID := range stream.messageIDs {
							if ctx.Err() != nil {
								return
							}
							mirror(stream, messageID)
						}
					}(stream)
				}
			}

			var waits []time.Duration
			var fenceErrors []error
			time.Sleep(300 * time.Millisecond)
			for ctx.Err() == nil {
				// lock_timeout restarts on every multixact member, so a
				// context bounds a starved admission.
				fenceCtx, cancelFence := context.WithTimeout(context.Background(), 10*time.Second)
				waited, fenceErr := lockLegacyMetaPolicyFenceWithin(fenceCtx, db, org.ID, 10*time.Second, 2*time.Millisecond)
				cancelFence()
				if fenceErr != nil {
					fenceErrors = append(fenceErrors, fenceErr)
				} else {
					waits = append(waits, waited)
				}
				time.Sleep(50 * time.Millisecond)
			}
			group.Wait()

			var longest time.Duration
			for _, waited := range waits {
				longest = max(longest, waited)
			}
			t.Logf("%d admissions, longest wait %v; %d mirrors", len(waits), longest, mirrored)
			require.Empty(t, fenceErrors)
			require.Empty(t, mirrorErrors)
			require.GreaterOrEqual(t, len(waits), 5, "admissions must keep acquiring")
			for index, waited := range waits {
				assert.Less(t, waited, time.Second, "admission %d of %d starved behind the mirror streams", index+1, len(waits))
			}
			assert.Positive(t, mirrored)
		})
	}
}

// Organization settings take the organization row FOR UPDATE without the
// policy fence key. Such a writer can wait for a mirror that went past the
// queue while an admission that then takes the key waits behind the writer.
// The admission is not stuck: the writer only passes the wait on to a running
// member, so a later mirror must queue behind the admission rather than
// bypass. Read as stuck, every mirror would bypass at once, and under steady
// traffic the writer and the admission would never get the row.
func TestLegacyMetaMirrorQueuesBehindAFenceThatWaitsForAWriterBlockedByAMember(t *testing.T) {
	channelapi.ForgetLegacyMetaPolicyFenceStatesForTest()
	db := testutil.SetupTestDB(t)
	org := createLegacyMetaTestOrganization(t, db, "org-writer")
	first, firstPending := legacyMetaFenceAccount(t, db, org, "writer-first", 1, 1)
	second, secondPending := legacyMetaFenceAccount(t, db, org, "writer-second", 1, 1)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	backendPID := func(tx *gorm.DB) int {
		var pid int
		require.NoError(t, tx.Session(&gorm.Session{NewDB: true}).Raw("SELECT pg_backend_pid()").Scan(&pid).Error)
		return pid
	}
	blockedBy := func(blocker, waiter int) bool {
		var blocked bool
		return db.Raw("SELECT ? = ANY(pg_catalog.pg_blocking_pids(?))", blocker, waiter).
			Scan(&blocked).Error == nil && blocked
	}

	// A mirror that went past the queue (behind a fence key holder that
	// worked for longer than the queue bound) and has not committed.
	keyHolder := db.WithContext(ctx).Begin()
	require.NoError(t, keyHolder.Error)
	t.Cleanup(func() { _ = keyHolder.Rollback().Error })
	require.NoError(t, keyHolder.Exec(
		"SELECT pg_catalog.pg_advisory_xact_lock(pg_catalog.hashtextextended(?, 0))",
		database.WhatsAppIdentityReviewContactSelectorFenceKey(org.ID),
	).Error)
	member := db.WithContext(ctx).Begin()
	require.NoError(t, member.Error)
	t.Cleanup(func() { _ = member.Rollback().Error })
	memberPID := backendPID(member)
	_, err := channelapi.MirrorLegacyWhatsAppMessage(member, legacyMetaRef(first), firstPending[0][0])
	require.NoError(t, err)
	require.NoError(t, keyHolder.Commit().Error)

	// An organization writer queues behind it, and an admission behind the
	// writer.
	writer := db.WithContext(ctx).Begin()
	require.NoError(t, writer.Error)
	t.Cleanup(func() { _ = writer.Rollback().Error })
	writerPID := backendPID(writer)
	written := make(chan error, 1)
	go func() {
		var locked []models.Organization
		written <- writer.Clauses(clause.Locking{Strength: "UPDATE"}).Select("id").
			Where("id = ?", org.ID).Find(&locked).Error
	}()
	require.Eventually(t, func() bool { return blockedBy(memberPID, writerPID) },
		10*time.Second, 5*time.Millisecond, "the writer must wait for the open mirror")
	fence, fenced := queueLegacyMetaPolicyFence(ctx, t, db, org.ID)
	var fencePID int
	require.NoError(t, db.Raw(
		`SELECT pid FROM pg_catalog.pg_locks
		  WHERE locktype = 'advisory' AND mode = 'ExclusiveLock' AND granted
		    AND (classid::int8 << 32) | objid::int8 = pg_catalog.hashtextextended(?, 0)`,
		database.WhatsAppIdentityReviewContactSelectorFenceKey(org.ID),
	).Scan(&fencePID).Error)
	require.Eventually(t, func() bool { return blockedBy(writerPID, fencePID) },
		10*time.Second, 5*time.Millisecond, "the admission must wait behind the writer")
	channelapi.ForgetLegacyMetaPolicyFenceStatesForTest()

	// The later mirror commits as soon as it is done, so if it wins the row
	// from the writer or the admission it delays them by one short mirror.
	lateDone := make(chan error, 1)
	go func() {
		_, mirrorErr := channelapi.MirrorLegacyWhatsAppMessage(db.WithContext(ctx), legacyMetaRef(second), secondPending[0][0])
		lateDone <- mirrorErr
	}()
	select {
	case <-lateDone:
		require.Fail(t, "the later mirror went past a fence that waits for a writer blocked by a member")
	case <-time.After(500 * time.Millisecond):
	}

	require.NoError(t, member.Commit().Error)
	select {
	case writeErr := <-written:
		require.NoError(t, writeErr)
	case <-time.After(10 * time.Second):
		require.Fail(t, "the writer did not acquire after the mirror committed")
	}
	require.NoError(t, writer.Commit().Error)
	select {
	case fenceErr := <-fenced:
		require.NoError(t, fenceErr)
	case <-time.After(10 * time.Second):
		require.Fail(t, "the admission did not acquire after the writer committed")
	}
	require.NoError(t, fence.Commit().Error)
	select {
	case mirrorErr := <-lateDone:
		require.NoError(t, mirrorErr)
	case <-time.After(10 * time.Second):
		require.Fail(t, "the later mirror did not continue after the admission committed")
	}
}

// A mirror can wait for a contact that a send holds across its Meta call while
// it holds the organization. A fence blocked by that mirror waits for the
// send, so sharers on other accounts gain nothing by queueing behind it: the
// fence is stuck behind the running send, not blocked by a short member. They
// must not pay the member wait (1 s).
func TestLegacyMetaSharersDoNotQueueBehindAFenceWaitingForAMemberThatWaitsForASend(t *testing.T) {
	db := testutil.SetupTestDB(t)
	org := createLegacyMetaTestOrganization(t, db, "member-waits")
	waiting, waitingPending := legacyMetaFenceAccount(t, db, org, "member-waits", 1, 1)
	other, otherPending := legacyMetaFenceAccount(t, db, org, "member-other", 1, 6)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	// A send holds the contact across its Meta call.
	var message models.Message
	require.NoError(t, db.Where("id = ?", waitingPending[0][0]).First(&message).Error)
	send := db.WithContext(ctx).Begin()
	require.NoError(t, send.Error)
	t.Cleanup(func() { _ = send.Rollback().Error })
	var held []models.Contact
	require.NoError(t, send.Clauses(clause.Locking{Strength: "UPDATE"}).Select("id").
		Where("id = ?", message.ContactID).Find(&held).Error)

	// The mirror goes past a queued fence (stuck behind an attempt fence), then
	// waits for the contact while it holds the organization.
	attempt := db.WithContext(ctx).Begin()
	require.NoError(t, attempt.Error)
	t.Cleanup(func() { _ = attempt.Rollback().Error })
	require.NoError(t, database.LockOrganizationAIAttemptScope(attempt, org.ID))
	fence, fenced := queueLegacyMetaPolicyFence(ctx, t, db, org.ID)
	member := db.WithContext(ctx).Begin()
	require.NoError(t, member.Error)
	t.Cleanup(func() { _ = member.Rollback().Error })
	var memberPID int
	require.NoError(t, member.Session(&gorm.Session{NewDB: true}).Raw("SELECT pg_backend_pid()").Scan(&memberPID).Error)
	mirrored := make(chan error, 1)
	go func() {
		_, err := channelapi.MirrorLegacyWhatsAppMessage(member, legacyMetaRef(waiting), message.ID)
		mirrored <- err
	}()
	var sendPID int
	require.NoError(t, send.Session(&gorm.Session{NewDB: true}).Raw("SELECT pg_backend_pid()").Scan(&sendPID).Error)
	require.Eventually(t, func() bool {
		var blocked bool
		return db.Raw("SELECT ? = ANY(pg_catalog.pg_blocking_pids(?))", sendPID, memberPID).
			Scan(&blocked).Error == nil && blocked
	}, 10*time.Second, 5*time.Millisecond, "the mirror must wait for the contact the send holds")
	var fencePID int
	require.NoError(t, db.Raw(
		`SELECT pid FROM pg_catalog.pg_locks
		  WHERE locktype = 'advisory' AND mode = 'ExclusiveLock' AND granted
		    AND (classid::int8 << 32) | objid::int8 = pg_catalog.hashtextextended(?, 0)`,
		database.WhatsAppIdentityReviewContactSelectorFenceKey(org.ID),
	).Scan(&fencePID).Error)
	require.NotZero(t, fencePID)
	require.NoError(t, attempt.Commit().Error)
	require.Eventually(t, func() bool {
		var blocked bool
		return db.Raw("SELECT ? = ANY(pg_catalog.pg_blocking_pids(?))", memberPID, fencePID).
			Scan(&blocked).Error == nil && blocked
	}, 10*time.Second, 5*time.Millisecond, "the fence must now wait for the mirror")
	channelapi.ForgetLegacyMetaPolicyFenceStatesForTest()

	for _, messageID := range otherPending[0] {
		started := time.Now()
		_, err := channelapi.MirrorLegacyWhatsAppMessage(db.WithContext(ctx), legacyMetaRef(other), messageID)
		require.NoError(t, err)
		assert.Less(t, time.Since(started), 600*time.Millisecond,
			"a sharer queued behind a fence that waits for a send")
	}

	require.NoError(t, send.Commit().Error)
	require.NoError(t, <-mirrored)
	require.NoError(t, member.Commit().Error)
	select {
	case fenceErr := <-fenced:
		require.NoError(t, fenceErr)
	case <-time.After(10 * time.Second):
		require.Fail(t, "the policy fence did not acquire after the send and the mirror")
	}
	require.NoError(t, fence.Commit().Error)
}

// The same with a mirror that joined the key, because no fence was queued when
// it started: an admission then waits for the key behind that mirror, which
// waits for the send. Sharers must see that the admission is stuck behind the
// send and not queue for the queue bound (raised here to 5 s).
func TestLegacyMetaSharersDoNotQueueBehindAFenceWaitingForAJoinedMemberThatWaitsForASend(t *testing.T) {
	defer channelapi.SetLegacyMetaPolicyFenceQueueWaitForTest(5 * time.Second)()
	db := testutil.SetupTestDB(t)
	org := createLegacyMetaTestOrganization(t, db, "joined-waits")
	waiting, waitingPending := legacyMetaFenceAccount(t, db, org, "joined-waits", 1, 1)
	other, otherPending := legacyMetaFenceAccount(t, db, org, "joined-other", 1, 4)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	blockedBy := func(blocker, waiter int) bool {
		var blocked bool
		return db.Raw("SELECT ? = ANY(pg_catalog.pg_blocking_pids(?))", blocker, waiter).
			Scan(&blocked).Error == nil && blocked
	}

	var message models.Message
	require.NoError(t, db.Where("id = ?", waitingPending[0][0]).First(&message).Error)
	send := db.WithContext(ctx).Begin()
	require.NoError(t, send.Error)
	t.Cleanup(func() { _ = send.Rollback().Error })
	var sendPID int
	require.NoError(t, send.Session(&gorm.Session{NewDB: true}).Raw("SELECT pg_backend_pid()").Scan(&sendPID).Error)
	var held []models.Contact
	require.NoError(t, send.Clauses(clause.Locking{Strength: "UPDATE"}).Select("id").
		Where("id = ?", message.ContactID).Find(&held).Error)

	member := db.WithContext(ctx).Begin()
	require.NoError(t, member.Error)
	t.Cleanup(func() { _ = member.Rollback().Error })
	var memberPID int
	require.NoError(t, member.Session(&gorm.Session{NewDB: true}).Raw("SELECT pg_backend_pid()").Scan(&memberPID).Error)
	mirrored := make(chan error, 1)
	go func() {
		_, err := channelapi.MirrorLegacyWhatsAppMessage(member, legacyMetaRef(waiting), message.ID)
		mirrored <- err
	}()
	require.Eventually(t, func() bool { return blockedBy(sendPID, memberPID) },
		10*time.Second, 5*time.Millisecond, "the mirror must wait for the contact the send holds")
	fence, fenced := queueLegacyMetaPolicyFence(ctx, t, db, org.ID)
	channelapi.ForgetLegacyMetaPolicyFenceStatesForTest()

	for _, messageID := range otherPending[0] {
		started := time.Now()
		_, err := channelapi.MirrorLegacyWhatsAppMessage(db.WithContext(ctx), legacyMetaRef(other), messageID)
		require.NoError(t, err)
		assert.Less(t, time.Since(started), time.Second,
			"a sharer queued behind an admission that waits for a send")
	}

	require.NoError(t, send.Commit().Error)
	require.NoError(t, <-mirrored)
	require.NoError(t, member.Commit().Error)
	select {
	case fenceErr := <-fenced:
		require.NoError(t, fenceErr)
	case <-time.After(10 * time.Second):
		require.Fail(t, "the policy fence did not acquire after the send and the mirror")
	}
	require.NoError(t, fence.Commit().Error)
}

// Waiting sharers of one organization share one lock-table query per
// legacyMetaPolicyFenceStateTTL: the query reads every lock in the cluster.
func TestLegacyMetaPolicyFenceStateIsQueriedOncePerTTL(t *testing.T) {
	db := testutil.SetupTestDB(t)
	org := createLegacyMetaTestOrganization(t, db, "state-queries")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	// A fence that holds the key and works keeps sharers polling for the
	// whole queue bound.
	fence := db.WithContext(ctx).Begin()
	require.NoError(t, fence.Error)
	t.Cleanup(func() { _ = fence.Rollback().Error })
	require.NoError(t, fence.Exec(
		"SELECT pg_catalog.pg_advisory_xact_lock(pg_catalog.hashtextextended(?, 0))",
		database.WhatsAppIdentityReviewContactSelectorFenceKey(org.ID),
	).Error)

	before := channelapi.LegacyMetaPolicyFenceStateQueriesForTest()
	const sharers = 32
	var group sync.WaitGroup
	errs := make(chan error, sharers)
	for range sharers {
		group.Add(1)
		go func() {
			defer group.Done()
			errs <- db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
				return channelapi.LockLegacyMetaOrganization(tx, org.ID)
			})
		}()
	}
	group.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	queries := channelapi.LegacyMetaPolicyFenceStateQueriesForTest() - before
	t.Logf("%d sharers polled for the queue bound with %d state queries", sharers, queries)
	assert.LessOrEqual(t, queries, int64(25), "waiting sharers must share the state query")
}

// A strict reply's pre-provider transaction takes the organization and the
// shadow, then mirrors in the same transaction, which takes them again. When
// the first acquisition went past a fence that is still waiting, the second
// must not queue for that fence again.
func TestLegacyMetaReentryAfterGoingPastAWaitingFenceDoesNotQueueAgain(t *testing.T) {
	const bound = time.Second
	defer channelapi.SetLegacyMetaPolicyFenceQueueWaitForTest(bound)()
	db := testutil.SetupTestDB(t)
	org, account, pending := legacyMetaFenceFixture(t, db, "reentry", 1, 1)
	shadow, err := ensureFencedLegacyMetaAccountForTest(db, legacyMetaRef(account))
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	sharer := holdLegacyMetaPolicyFenceKeyShared(ctx, t, db, org.ID)
	fence, fenced := queueLegacyMetaPolicyFence(ctx, t, db, org.ID)
	var first, reentry time.Duration
	require.NoError(t, db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		started := time.Now()
		if err := channelapi.LockLegacyMetaOrganizationAndShadow(tx, org.ID, shadow.ID); err != nil {
			return err
		}
		first = time.Since(started)
		started = time.Now()
		if _, err := channelapi.MirrorLegacyWhatsAppMessage(tx, legacyMetaRef(account), pending[0][0]); err != nil {
			return err
		}
		reentry = time.Since(started)
		return nil
	}))
	assert.GreaterOrEqual(t, first, bound, "the first acquisition queues behind the waiting fence")
	assert.Less(t, reentry, bound/2, "the re-entry queued for the fence again")

	require.NoError(t, sharer.Commit().Error)
	select {
	case fenceErr := <-fenced:
		require.NoError(t, fenceErr)
	case <-time.After(10 * time.Second):
		require.Fail(t, "the policy fence did not acquire after the sharer committed")
	}
	require.NoError(t, fence.Commit().Error)
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
