package channel

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/database"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestEnsureLegacyMetaAccountRefreshIsAdditiveAndReturnsFreshProjection(t *testing.T) {
	db := testutil.SetupTestDB(t)
	organization := testutil.CreateTestOrganization(t, db)
	legacy := models.WhatsAppAccount{
		BaseModel:          models.BaseModel{ID: uuid.New()},
		OrganizationID:     organization.ID,
		Name:               "compat-" + uuid.NewString(),
		PhoneID:            "9" + uuid.NewString()[0:15],
		BusinessID:         "business-" + uuid.NewString(),
		AccessToken:        "test-token",
		WebhookVerifyToken: "verify-" + uuid.NewString(),
		Status:             "active",
	}
	require.NoError(t, db.Create(&legacy).Error)
	ref := LegacyMetaAccountRef{
		ID: legacy.ID, OrganizationID: organization.ID, Name: legacy.Name, Status: legacy.Status,
	}
	initial, err := ensureLegacyMetaAccount(db, ref)
	require.NoError(t, err)

	legacyConfig := models.JSONB{
		"legacy_read_only": true,
		"outbound_enabled": false,
		"reply_route":      "chat",
	}
	legacyCapabilities := models.JSONB{
		"text": true, "template": true, "mark_read": true, "attachments": true,
	}
	require.NoError(t, db.Model(&models.ChannelAccount{}).Where("id = ?", initial.ID).Updates(map[string]any{
		"config": legacyConfig, "capabilities": legacyCapabilities,
	}).Error)

	refreshed, err := ensureLegacyMetaAccount(db, ref)
	require.NoError(t, err)
	var persisted models.ChannelAccount
	require.NoError(t, db.Where("id = ? AND organization_id = ?", refreshed.ID, organization.ID).First(&persisted).Error)
	assert.Equal(t, persisted.Config, refreshed.Config)
	assert.Equal(t, persisted.Capabilities, refreshed.Capabilities)
	assert.Equal(t, "chat", refreshed.Config["reply_route"])
	for _, key := range []string{
		"template", "mark_read", "attachments",
		"text", "media", "replies", "templates", "service_window", "read_receipts",
	} {
		assert.Equal(t, true, refreshed.Capabilities[key], "missing compatible capability %q", key)
	}
	assert.NotContains(t, refreshed.Capabilities, "legacy_text_reply_endpoint", "rollout marker must remain response-only")
}

type legacyMetaTestSQLState string

func (code legacyMetaTestSQLState) Error() string    { return "synthetic SQLSTATE " + string(code) }
func (code legacyMetaTestSQLState) SQLState() string { return string(code) }

// Only a NOWAIT conflict loses a round. A 55P03 from a blocking step is the
// caller's lock_timeout and must surface at once instead of being retried as
// a busy shadow.
func TestLockLegacyMetaOrganizationThenShadowRetriesOnlyNowaitConflicts(t *testing.T) {
	db := testutil.SetupTestDB(t)
	organization := testutil.CreateTestOrganization(t, db)
	busy := legacyMetaTestSQLState("55P03")

	var calls []bool
	err := db.Transaction(func(tx *gorm.DB) error {
		return lockLegacyMetaOrganizationThenShadow(tx, organization.ID, func(_ *gorm.DB, wait legacyMetaRowWait) error {
			calls = append(calls, wait == legacyMetaRowsNowait)
			if wait == legacyMetaRowsNowait {
				return busy
			}
			return nil
		})
	})
	require.NoError(t, err, "a busy shadow is waited for in step 2, then the organization is taken")
	assert.Equal(t, []bool{true, false}, calls)

	calls = nil
	err = db.Transaction(func(tx *gorm.DB) error {
		return lockLegacyMetaOrganizationThenShadow(tx, organization.ID, func(_ *gorm.DB, wait legacyMetaRowWait) error {
			calls = append(calls, wait == legacyMetaRowsNowait)
			return busy
		})
	})
	require.ErrorIs(t, err, busy, "a lock_timeout on the blocking shadow wait is returned")
	assert.Equal(t, []bool{true, false}, calls)
}

// legacyMetaLostRounds is what runLegacyMetaLostRounds observed.
type legacyMetaLostRounds struct {
	nowaitCalls, waitCalls int
	// Advisory locks this transaction held in shared mode during the
	// fallback's shadow wait.
	fenceKeys, bypassKeys int64
	// A FOR UPDATE NOWAIT probe of the organization during that wait.
	organizationProbe error
	// This backend's subtransactions during that wait, seen from another
	// session: lost rounds must not leave nesting levels behind.
	subtransactions           int
	subtransactionsOverflowed bool
}

// runLegacyMetaLostRounds makes every round of lockLegacyMetaOrganizationThenShadow
// lose and records the plain-order fallback. Every NOWAIT shadow attempt is
// busy. During every step-2 shadow wait another session takes the organization
// FOR NO KEY UPDATE, so the following FOR SHARE NOWAIT fails with 55P03, and it
// lets go only once this transaction waits for the organization in the next
// round (or in the fallback), so the rounds play out the same on any host.
func runLegacyMetaLostRounds(t *testing.T, db *gorm.DB, organizationID uuid.UUID) legacyMetaLostRounds {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	busy := legacyMetaTestSQLState("55P03")
	var observed legacyMetaLostRounds
	var releasers sync.WaitGroup
	defer releasers.Wait()
	releaseErrors := make(chan error, lockLegacyMetaShadowRounds)
	err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var mainPID int
		if err := tx.Session(&gorm.Session{NewDB: true}).Raw("SELECT pg_backend_pid()").Scan(&mainPID).Error; err != nil {
			return err
		}
		return lockLegacyMetaOrganizationThenShadow(tx, organizationID, func(_ *gorm.DB, wait legacyMetaRowWait) error {
			if wait == legacyMetaRowsNowait {
				observed.nowaitCalls++
				return busy
			}
			observed.waitCalls++
			if observed.waitCalls <= lockLegacyMetaShadowRounds {
				blocker := db.WithContext(ctx).Begin()
				if blocker.Error != nil {
					return blocker.Error
				}
				var blockerPID int
				if err := blocker.Session(&gorm.Session{NewDB: true}).
					Raw("SELECT pg_backend_pid()").Scan(&blockerPID).Error; err != nil {
					_ = blocker.Rollback().Error
					return err
				}
				if err := blocker.Exec(
					"SELECT id FROM organizations WHERE id = ? FOR NO KEY UPDATE", organizationID,
				).Error; err != nil {
					_ = blocker.Rollback().Error
					return err
				}
				releasers.Add(1)
				go func() {
					defer releasers.Done()
					defer func() { _ = blocker.Rollback().Error }()
					for ctx.Err() == nil {
						var waiting bool
						if db.WithContext(ctx).Raw(
							"SELECT ? = ANY(pg_catalog.pg_blocking_pids(?))", blockerPID, mainPID,
						).Scan(&waiting).Error == nil && waiting {
							return
						}
						time.Sleep(time.Millisecond)
					}
					releaseErrors <- ctx.Err()
				}()
				return nil
			}
			// The fallback's shadow wait: whatever passed the fence queue and
			// the organization are already held by this transaction.
			if err := tx.Raw(
				`SELECT count(*) FROM pg_catalog.pg_locks
				  WHERE pid = pg_catalog.pg_backend_pid()
				    AND locktype = 'advisory' AND mode = 'ShareLock' AND granted
				    AND (classid::int8 << 32) | objid::int8 = pg_catalog.hashtextextended(?, 0)`,
				database.WhatsAppIdentityReviewContactSelectorFenceKey(organizationID),
			).Scan(&observed.fenceKeys).Error; err != nil {
				return err
			}
			if err := tx.Raw(
				`SELECT count(*) FROM pg_catalog.pg_locks
				  WHERE pid = pg_catalog.pg_backend_pid()
				    AND locktype = 'advisory' AND mode = 'ShareLock' AND granted
				    AND (classid::int8 << 32) | objid::int8 = pg_catalog.hashtextextended(?, 0)`,
				legacyMetaPolicyFenceBypassKey(organizationID),
			).Scan(&observed.bypassKeys).Error; err != nil {
				return err
			}
			probe := db.Begin()
			defer func() { _ = probe.Rollback().Error }()
			observed.organizationProbe = probe.Exec(
				"SELECT id FROM organizations WHERE id = ? FOR UPDATE NOWAIT",
				organizationID,
			).Error
			var subtransactions struct {
				Count      int  `gorm:"column:subxact_count"`
				Overflowed bool `gorm:"column:subxact_overflowed"`
			}
			if err := db.Raw(
				`SELECT s.subxact_count, s.subxact_overflowed
				   FROM pg_catalog.pg_stat_get_backend_idset() AS b(id)
				   CROSS JOIN LATERAL pg_catalog.pg_stat_get_backend_subxact(b.id) AS s
				  WHERE pg_catalog.pg_stat_get_backend_pid(b.id) = ?`,
				mainPID,
			).Scan(&subtransactions).Error; err != nil {
				return err
			}
			observed.subtransactions = subtransactions.Count
			observed.subtransactionsOverflowed = subtransactions.Overflowed
			return nil
		})
	})
	require.NoError(t, err)
	releasers.Wait()
	close(releaseErrors)
	for releaseErr := range releaseErrors {
		require.NoError(t, releaseErr, "this transaction never waited for the organization blocker")
	}
	require.Equal(t, lockLegacyMetaShadowRounds, observed.nowaitCalls)
	require.Equal(t, lockLegacyMetaShadowRounds+1, observed.waitCalls)
	// GORM's nested Transaction never releases its savepoint: 64 lost rounds
	// left 128 nested subtransactions, past the 64 a backend caches.
	assert.LessOrEqual(t, observed.subtransactions, 1, "lost rounds left nested subtransactions behind")
	assert.False(t, observed.subtransactionsOverflowed)
	return observed
}

// After lockLegacyMetaShadowRounds lost rounds the helper waits in plain order:
// the policy fence key in shared mode, then the organization FOR SHARE, then the
// shadow.
func TestLockLegacyMetaOrganizationThenShadowFallsBackToPlainOrder(t *testing.T) {
	db := testutil.SetupTestDB(t)
	organization := testutil.CreateTestOrganization(t, db)

	observed := runLegacyMetaLostRounds(t, db, organization.ID)
	assert.EqualValues(t, 1, observed.fenceKeys, "the fallback holds the shared fence key while it waits for the shadow")
	assert.Zero(t, observed.bypassKeys)
	assert.True(t, legacyMetaLockNotAvailable(observed.organizationProbe),
		"the fallback holds the organization while it waits for the shadow: %v", observed.organizationProbe)
}

// The same fallback once the call's queue deadline has passed while a fence
// holds the key throughout: every round and the fallback go past the queue
// without the key, so the fallback holds the bypass key and the organization
// FOR SHARE while it waits for the shadow. A step-2 loss after the deadline
// comes only from the organization row, never from the queued fence.
func TestLockLegacyMetaOrganizationThenShadowFallsBackPastAnExpiredQueue(t *testing.T) {
	defer SetLegacyMetaPolicyFenceQueueWaitForTest(20 * time.Millisecond)()
	db := testutil.SetupTestDB(t)
	organization := testutil.CreateTestOrganization(t, db)
	fence := db.Begin()
	require.NoError(t, fence.Error)
	defer func() { _ = fence.Rollback().Error }()
	require.NoError(t, fence.Exec(
		"SELECT pg_catalog.pg_advisory_xact_lock(pg_catalog.hashtextextended(?, 0))",
		database.WhatsAppIdentityReviewContactSelectorFenceKey(organization.ID),
	).Error)

	observed := runLegacyMetaLostRounds(t, db, organization.ID)
	assert.Zero(t, observed.fenceKeys)
	assert.EqualValues(t, 1, observed.bypassKeys, "the fallback went past the expired queue")
	assert.True(t, legacyMetaLockNotAvailable(observed.organizationProbe),
		"the fallback holds the organization while it waits for the shadow: %v", observed.organizationProbe)
}
