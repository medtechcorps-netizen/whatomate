package database_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	databasepkg "github.com/shridarpatil/whatomate/internal/database"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestAutomaticReplyPolicyFailsClosedOnIncompleteIdentity(t *testing.T) {
	policy, err := databasepkg.EvaluateContactAutomaticReplyPolicy(
		nil,
		uuid.Nil,
		uuid.Nil,
	)
	require.Error(t, err)
	assert.False(t, policy.Allowed)
	require.Error(t, databasepkg.LockOrganizationPolicyScope(nil, uuid.New()))
	require.Error(t, databasepkg.LockOrganizationAIAttemptScope(nil, uuid.New()))
}

func TestAIAttemptFenceRequiresIndependentConnectionCapacity(t *testing.T) {
	db := testutil.SetupTestDB(t)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	previous := sqlDB.Stats().MaxOpenConnections
	t.Cleanup(func() { sqlDB.SetMaxOpenConns(previous) })

	sqlDB.SetMaxOpenConns(1)
	require.Error(t, databasepkg.RequireIndependentAIAttemptConnections(db))
	sqlDB.SetMaxOpenConns(2)
	require.NoError(t, databasepkg.RequireIndependentAIAttemptConnections(db))
	sqlDB.SetMaxOpenConns(0)
	require.NoError(t, databasepkg.RequireIndependentAIAttemptConnections(db))
}

func TestOrganizationAIAttemptFenceAllowsFKSettlementAndBlocksPolicyWriter(
	t *testing.T,
) {
	db := testutil.SetupTestDB(t)
	testutil.TruncateTables(db)
	organization := testutil.CreateTestOrganization(t, db)
	// Install the production write guards after creating the ordinary tenant.
	// The extra compliance tenant is not involved in the lock assertions.
	testutil.CreateTestPlatformComplianceOrganization(t, db, false, false)

	var scheduledJobGuardEnabled bool
	require.NoError(t, db.Raw(`
		SELECT pg_catalog.count(*) = 1
			AND pg_catalog.bool_and(trigger.tgenabled = 'O')
		FROM pg_catalog.pg_trigger AS trigger
		JOIN pg_catalog.pg_class AS relation ON relation.oid = trigger.tgrelid
		JOIN pg_catalog.pg_namespace AS namespace ON namespace.oid = relation.relnamespace
		WHERE namespace.nspname = 'public'
		  AND relation.relname = 'scheduled_jobs'
		  AND trigger.tgname = 'rereply_platform_compliance_write_guard'
		  AND NOT trigger.tgisinternal
	`).Scan(&scheduledJobGuardEnabled).Error)
	require.True(t, scheduledJobGuardEnabled, "scheduled-job compliance guard must be active")

	sqlDB, err := db.DB()
	require.NoError(t, err)
	previous := sqlDB.Stats().MaxOpenConnections
	t.Cleanup(func() { sqlDB.SetMaxOpenConns(previous) })
	sqlDB.SetMaxOpenConns(4)

	releaseAttempt := make(chan struct{})
	attemptLocked := make(chan struct{})
	attemptDone := make(chan error, 1)
	go func() {
		attemptDone <- db.Transaction(func(tx *gorm.DB) error {
			if err := databasepkg.LockOrganizationAIAttemptScope(tx, organization.ID); err != nil {
				return err
			}
			close(attemptLocked)
			<-releaseAttempt
			return nil
		})
	}()
	<-attemptLocked

	// A scheduled-job insert takes both the organization FK KEY SHARE lock and
	// the platform-compliance guard's SHARE lock. Neither may be stalled by the
	// attempt fence while a provider call is in flight.
	scheduledJobDone := make(chan error, 1)
	go func() {
		scheduledJobDone <- db.WithContext(context.Background()).Create(&models.ScheduledJob{
			BaseModel:      models.BaseModel{ID: uuid.New()},
			OrganizationID: organization.ID,
			Kind:           models.ScheduledJobKindChannelAIReply,
			AggregateType:  models.ChannelAIReplyAggregateType,
			RunAt:          time.Now().UTC(),
			Status:         models.ScheduledJobStatusPending,
			MaxAttempts:    5,
			IdempotencyKey: "ai-fence-" + uuid.NewString(),
			Payload:        models.JSONB{},
		}).Error
	}()
	select {
	case err := <-scheduledJobDone:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		close(releaseAttempt)
		require.NoError(t, <-attemptDone)
		t.Fatal("scheduled-job compliance guard was blocked by the AI fence")
	}

	writerPID := make(chan int, 1)
	writerDone := make(chan error, 1)
	go func() {
		writerDone <- db.Transaction(func(tx *gorm.DB) error {
			var pid int
			if err := tx.Raw("SELECT pg_backend_pid()").Scan(&pid).Error; err != nil {
				return err
			}
			writerPID <- pid
			return databasepkg.LockOrganizationPolicyScope(tx, organization.ID)
		})
	}()
	pid := <-writerPID
	testutil.RequirePostgresBackendWaitingForLock(t, db, pid)

	close(releaseAttempt)
	require.NoError(t, <-attemptDone)
	require.NoError(t, <-writerDone)
}

func TestAutomaticReplyPolicyBlocksHumanHandover(t *testing.T) {
	db := testutil.SetupTestDB(t)
	testutil.TruncateTables(db)
	organization := testutil.CreateTestOrganization(t, db)
	contact := testutil.CreateTestContact(t, db, organization.ID)

	policy, err := databasepkg.EvaluateContactAutomaticReplyPolicy(
		db,
		organization.ID,
		contact.ID,
	)
	require.NoError(t, err)
	assert.True(t, policy.Allowed)

	require.NoError(t, db.Create(&models.AgentTransfer{
		BaseModel:       models.BaseModel{ID: uuid.New()},
		OrganizationID:  organization.ID,
		ContactID:       contact.ID,
		WhatsAppAccount: "test-account",
		PhoneNumber:     contact.PhoneNumber,
		Status:          models.TransferStatusActive,
		Source:          models.TransferSourceManual,
	}).Error)
	policy, err = databasepkg.EvaluateContactAutomaticReplyPolicy(
		db,
		organization.ID,
		contact.ID,
	)
	require.NoError(t, err)
	assert.False(t, policy.Allowed)
	assert.Equal(t, databasepkg.AutomaticReplyBlockedHumanHandover, policy.Reason)
}
