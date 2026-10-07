package handlers

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	channelapi "github.com/shridarpatil/whatomate/internal/channel"
	"github.com/shridarpatil/whatomate/internal/config"
	"github.com/shridarpatil/whatomate/internal/database"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// The legacy bridge prefix is ChannelAccount -> ContactIdentity ->
// InboxConversation -> Contact -> Message, followed by participant persistence.
// This regression holds each prefix row in turn and proves the strict reply
// path has not acquired the later Conversation lock while it waits.
func TestStrictLegacyReplyUsesBridgePrefixBeforeConversation(t *testing.T) {
	db := testutil.SetupTestDB(t)
	organization := testutil.CreateTestOrganization(t, db)
	account := testutil.CreateTestWhatsAppAccount(t, db, organization.ID)
	contact := testutil.CreateTestContactWith(
		t,
		db,
		organization.ID,
		testutil.WithContactAccount(account.Name),
	)
	message := models.Message{
		BaseModel:         models.BaseModel{ID: uuid.New()},
		OrganizationID:    organization.ID,
		WhatsAppAccount:   account.Name,
		ContactID:         contact.ID,
		Direction:         models.DirectionOutgoing,
		MessageType:       models.MessageTypeText,
		Content:           "lock-order probe",
		Status:            models.MessageStatusPending,
		WhatsAppMessageID: "wamid." + uuid.NewString(),
		ConversationID:    uuid.NewString(),
		InteractiveData:   models.JSONB{},
		Metadata:          models.JSONB{},
	}
	require.NoError(t, db.Create(&message).Error)
	mirror, err := channelapi.MirrorLegacyWhatsAppMessage(
		db,
		channelapi.LegacyMetaAccountRef{
			ID:             account.ID,
			OrganizationID: organization.ID,
			Name:           account.Name,
			Status:         account.Status,
		},
		message.ID,
	)
	require.NoError(t, err)

	policy := &legacyWhatsAppReplyPolicy{
		ConversationID:    mirror.ConversationID,
		ChannelAccountID:  mirror.ChannelAccountID,
		WhatsAppAccountID: account.ID,
	}
	holder := db.Begin()
	require.NoError(t, holder.Error)
	t.Cleanup(func() { _ = holder.Rollback().Error })
	var shadow models.ChannelAccount
	require.NoError(t, holder.Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("id = ? AND organization_id = ?", mirror.ChannelAccountID, organization.ID).
		First(&shadow).Error)

	waiterDone := make(chan error, 1)
	go func() {
		waiter := db.Session(&gorm.Session{NewDB: true}).Begin()
		if waiter.Error != nil {
			waiterDone <- waiter.Error
			return
		}
		defer func() { _ = waiter.Rollback().Error }()
		waiterDone <- lockStrictLegacyReplyOrder(waiter, organization.ID, policy)
	}()
	select {
	case err := <-waiterDone:
		require.Failf(t, "strict lock bypassed held shadow account", "result: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	// If the waiter acquired Conversation before blocking on ChannelAccount,
	// this same-order acquisition would wait or deadlock. It must remain free.
	lockContext, cancelLock := context.WithTimeout(context.Background(), time.Second)
	defer cancelLock()
	var conversation models.InboxConversation
	require.NoError(t, holder.WithContext(lockContext).
		Clauses(clause.Locking{Strength: "UPDATE"}).
		Select("id").
		Where("id = ? AND organization_id = ?", mirror.ConversationID, organization.ID).
		First(&conversation).Error)
	require.NoError(t, holder.Commit().Error)

	select {
	case err := <-waiterDone:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		require.Fail(t, "strict reply lock order did not complete after shadow release")
	}

	var persistedConversation models.InboxConversation
	require.NoError(t, db.Select("id, contact_identity_id").
		Where("id = ? AND organization_id = ?", mirror.ConversationID, organization.ID).
		First(&persistedConversation).Error)
	require.NotNil(t, persistedConversation.ContactIdentityID)
	identityHolder := db.Begin()
	require.NoError(t, identityHolder.Error)
	t.Cleanup(func() { _ = identityHolder.Rollback().Error })
	var identity models.ContactIdentity
	require.NoError(t, identityHolder.Clauses(clause.Locking{Strength: "UPDATE"}).
		Select("id").
		Where("id = ? AND organization_id = ?", *persistedConversation.ContactIdentityID, organization.ID).
		First(&identity).Error)

	identityWaiterDone := make(chan error, 1)
	go func() {
		waiter := db.Session(&gorm.Session{NewDB: true}).Begin()
		if waiter.Error != nil {
			identityWaiterDone <- waiter.Error
			return
		}
		defer func() { _ = waiter.Rollback().Error }()
		identityWaiterDone <- lockStrictLegacyReplyOrder(waiter, organization.ID, policy)
	}()
	select {
	case err := <-identityWaiterDone:
		require.Failf(t, "strict lock bypassed held contact identity", "result: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	// The strict waiter must not own Conversation while it waits for Identity.
	lockContext, cancelLock = context.WithTimeout(context.Background(), time.Second)
	defer cancelLock()
	require.NoError(t, identityHolder.WithContext(lockContext).
		Clauses(clause.Locking{Strength: "UPDATE"}).
		Select("id").
		Where("id = ? AND organization_id = ?", mirror.ConversationID, organization.ID).
		First(&conversation).Error)
	require.NoError(t, identityHolder.Commit().Error)
	select {
	case err := <-identityWaiterDone:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		require.Fail(t, "strict reply identity lock order did not complete")
	}
}

// Coexistence admission takes the organization policy fence before the legacy
// ChannelAccount shadow. The deferred mirror of an earlier admission writes
// organization-scoped rows whose foreign keys and platform-compliance write
// guard lock that same organization row, so it must queue on the organization
// before it owns the shadow. Otherwise the mirror waits on the fence while the
// admission waits on the shadow and PostgreSQL aborts one of them: a 503 that
// Meta redelivers, or a mirror attempt that is lost unless its retry wins. On
// the shared test schema the first mirror for a sender reaches the
// organization through its new ContactIdentity foreign key.
func TestDeferredLegacyMirrorQueuesBehindCoexistenceAdmissionFence(t *testing.T) {
	app, account, contact := whatsappIdentityFixture(t)
	require.NoError(t, app.DB.Model(&models.Contact{}).Where(
		"organization_id = ? AND id = ?",
		account.OrganizationID,
		contact.ID,
	).Updates(map[string]any{
		"bs_uid":       "US.fence-" + uuid.NewString(),
		"phone_number": "60" + testutil.NewTestGraphObjectID()[:10],
	}).Error)

	assertDeferredLegacyMirrorQueuesBehindAdmissionFence(t, app, app.DB, account, contact.ID, false)
}

// The runtime-role variant installs the production platform-compliance write
// guard. Its BEFORE INSERT trigger locks the organization on every mirror's
// ContactIdentity upsert, even when ON CONFLICT then skips the row, so it also
// covers a sender whose identity and conversation already exist. It is not
// parallel: applying and removing RLS policies changes shared schema.
func TestDeferredLegacyMirrorQueuesBehindCoexistenceAdmissionFenceWithRuntimeRoleRLS(t *testing.T) {
	adminDB := testutil.SetupTestDB(t)
	testutil.TruncateTables(adminDB)
	runtimeRole := "rereply_fence_" + uuid.NewString()[:8]
	runtimePassword := "synthetic" + uuid.NewString()[:8]
	require.NoError(t, adminDB.Exec(fmt.Sprintf(
		"CREATE ROLE %s LOGIN PASSWORD '%s' NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT NOBYPASSRLS",
		runtimeRole,
		runtimePassword,
	)).Error)
	t.Cleanup(func() {
		// Cleanup is best-effort so a test failure is not hidden by teardown.
		_ = database.RemoveTenantRLS(adminDB)
		_ = adminDB.Exec("DROP OWNED BY " + runtimeRole).Error
		_ = adminDB.Exec("DROP ROLE IF EXISTS " + runtimeRole).Error
		testutil.TruncateTables(adminDB)
	})

	reseller := testutil.CreateTestReseller(t, adminDB)
	organization := testutil.CreateTestOrganizationForReseller(t, adminDB, reseller.ID)
	account := testutil.CreateTestWhatsAppAccount(t, adminDB, organization.ID)
	account.IsSMB = true
	account.BusinessID = testutil.NewTestGraphObjectID()
	require.NoError(t, adminDB.Save(account).Error)
	require.NoError(t, adminDB.Create(&models.WhatsAppCoexistenceState{
		ID:                uuid.New(),
		OrganizationID:    organization.ID,
		WhatsAppAccountID: account.ID,
		OnboardingStatus:  models.CoexistenceOnboardingStatusConnected,
		OnboardingCycle:   1,
		LifecycleStatus:   models.CoexistenceLifecycleStatusConnected,
		LifecycleMetadata: models.JSONB{},
		Version:           1,
	}).Error)
	contact := testutil.CreateTestContactWith(
		t,
		adminDB,
		organization.ID,
		testutil.WithContactAccount(account.Name),
		testutil.WithPhoneNumber("60"+testutil.NewTestGraphObjectID()[:10]),
	)
	require.NoError(t, adminDB.Model(&models.Contact{}).Where(
		"organization_id = ? AND id = ?",
		organization.ID,
		contact.ID,
	).Update("bs_uid", "US.fence-"+uuid.NewString()).Error)

	require.NoError(t, database.ApplyTenantRLS(adminDB, runtimeRole))
	runtimeDB := openRuntimeRoleTestDB(t, runtimeRole, runtimePassword)
	require.NoError(t, database.VerifyTenantRLS(runtimeDB, runtimeRole))
	app := newProcessorTestApp(t)
	app.DB = runtimeDB
	app.Config = &config.Config{
		App: config.AppConfig{EncryptionKey: "test-encryption-key-32-bytes-long"},
	}
	app.Config.Database.RLSEnabled = true
	app.Config.Database.RuntimeRole = runtimeRole

	assertDeferredLegacyMirrorQueuesBehindAdmissionFence(t, app, adminDB, account, contact.ID, true)
}

// assertDeferredLegacyMirrorQueuesBehindAdmissionFence holds the exact lock
// prefix of persistAuthenticatedIncomingMessageBeforeAck (WAMID fence, then the
// organization policy fence), proves the real after-commit mirror is waiting on
// that transaction, and only then lets the admission continue to the shadow.
func assertDeferredLegacyMirrorQueuesBehindAdmissionFence(
	t *testing.T,
	app *App,
	observer *gorm.DB,
	account *models.WhatsAppAccount,
	contactID uuid.UUID,
	existingConversation bool,
) {
	t.Helper()
	organizationID := account.OrganizationID
	ref := channelapi.LegacyMetaAccountRef{
		ID:             account.ID,
		OrganizationID: organizationID,
		Name:           account.Name,
		Status:         account.Status,
	}
	createIncoming := func(content string) models.Message {
		message := models.Message{
			BaseModel:         models.BaseModel{ID: uuid.New()},
			OrganizationID:    organizationID,
			WhatsAppAccount:   account.Name,
			ContactID:         contactID,
			Direction:         models.DirectionIncoming,
			MessageType:       models.MessageTypeText,
			Content:           content,
			Status:            models.MessageStatusReceived,
			WhatsAppMessageID: "wamid.fence-" + uuid.NewString(),
			ConversationID:    uuid.NewString(),
			InteractiveData:   models.JSONB{},
			Metadata:          models.JSONB{},
		}
		require.NoError(t, observer.Create(&message).Error)
		return message
	}

	// An earlier admission already committed the shadow and its Message; only
	// its deferred mirror is still pending.
	shadow, err := ensureFencedLegacyMetaAccountForTest(observer, ref)
	require.NoError(t, err)
	if existingConversation {
		earlier := createIncoming("already mirrored")
		_, err = channelapi.MirrorLegacyWhatsAppMessage(observer, ref, earlier.ID)
		require.NoError(t, err)
	}
	pending := createIncoming("deferred mirror")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	admission := app.DB.WithContext(ctx).Begin()
	require.NoError(t, admission.Error)
	defer func() { _ = admission.Rollback().Error }()
	require.NoError(t, database.SetTenantContext(admission, organizationID))
	require.NoError(t, database.LockWhatsAppWAMIDScopes(
		admission,
		organizationID,
		"wamid.fence-next-"+uuid.NewString(),
	))
	require.NoError(t, database.LockOrganizationPolicyScope(admission, organizationID))
	var admissionPID int
	require.NoError(t, admission.Session(&gorm.Session{NewDB: true}).
		Raw("SELECT pg_backend_pid()").Scan(&admissionPID).Error)

	mirrored := make(chan struct{})
	go func() {
		defer close(mirrored)
		app.mirrorLegacyWhatsAppMessageAfterCommit(account, pending.ID)
	}()
	defer func() {
		_ = admission.Rollback().Error
		select {
		case <-mirrored:
		case <-time.After(10 * time.Second):
			t.Error("deferred mirror did not terminate after the admission ended")
		}
	}()
	var waitingQuery string
	require.Eventually(t, func() bool {
		var waiters []struct{ Query string }
		if observer.Raw(
			`SELECT query FROM pg_catalog.pg_stat_activity
			  WHERE ? = ANY(pg_catalog.pg_blocking_pids(pid))`,
			admissionPID,
		).Scan(&waiters).Error != nil || len(waiters) != 1 {
			return false
		}
		waitingQuery = waiters[0].Query
		return true
	}, 10*time.Second, 10*time.Millisecond, "the deferred mirror must wait on the admission's policy fence")

	// A waiting mirror that already owned the shadow would make the admission's
	// next step a lock cycle. Probe with NOWAIT so the order is proven directly:
	// the outcome cannot depend on which side PostgreSQL aborts or on the
	// mirror's retry of retryable aborts. Then continue exactly like admission.
	var probed models.ChannelAccount
	require.NoError(t, admission.Clauses(clause.Locking{Strength: "UPDATE", Options: "NOWAIT"}).
		Select("id").
		Where("id = ? AND organization_id = ?", shadow.ID, organizationID).
		First(&probed).Error,
		"the deferred mirror owned the shadow while waiting in %q", waitingQuery)
	require.NoError(t, admission.Exec("SET LOCAL lock_timeout = '10s'").Error)
	_, err = ensureFencedLegacyMetaAccountForTest(admission, ref)
	require.NoError(t, err)
	require.NoError(t, admission.Commit().Error)
	select {
	case <-mirrored:
	case <-time.After(10 * time.Second):
		require.Fail(t, "deferred mirror did not finish after the admission committed")
	}

	var linked models.Message
	require.NoError(t, observer.Select("id, inbox_conversation_id").
		Where("id = ? AND organization_id = ?", pending.ID, organizationID).
		First(&linked).Error)
	require.NotNil(t, linked.InboxConversationID,
		"the deferred mirror must still write the InboxConversation projection")
	var conversation models.InboxConversation
	require.NoError(t, observer.Where(
		"id = ? AND organization_id = ? AND contact_id = ?",
		*linked.InboxConversationID,
		organizationID,
		contactID,
	).First(&conversation).Error)
	require.NotNil(t, conversation.ContactIdentityID)
}
