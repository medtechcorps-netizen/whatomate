package channel

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/database"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// These values encode the reviewed starvation margin, rather than merely
// convenient test timings. The writer regression also runs at these bounds.
func TestLegacyMetaPolicyFenceProductionBounds(t *testing.T) {
	require.Equal(t, 250*time.Millisecond, legacyMetaPolicyFenceQueueWait)
	require.Equal(t, time.Second, legacyMetaPolicyFenceMemberWait)
	require.Equal(t, 25*time.Millisecond, legacyMetaPolicyFenceStateTTL)
	require.Equal(t, 5*time.Millisecond, legacyMetaPolicyFencePollInterval)
	require.Equal(t, 4, legacyMetaPolicyFenceWalkDepth)
}

func TestLegacyMetaPolicyFenceBusyRefreshDiscardsExpiredSample(t *testing.T) {
	defer SetLegacyMetaPolicyFenceStateTTLForTest(time.Second)()
	for _, test := range []struct {
		name string
		age  time.Duration
		want legacyMetaPolicyFenceState
	}{
		{"recent refresh in flight", 1500 * time.Millisecond, legacyMetaPolicyFenceStuck},
		{"expired refresh in flight", 3 * time.Second, legacyMetaPolicyFenceWaiting},
	} {
		t.Run(test.name, func(t *testing.T) {
			orgID := uuid.New()
			cache := &legacyMetaPolicyFenceStateCache{}
			cache.sample.Store(&legacyMetaPolicyFenceStateSample{state: legacyMetaPolicyFenceStuck, at: time.Now().Add(-test.age)})
			legacyMetaPolicyFenceStates.Store(orgID, cache)
			defer legacyMetaPolicyFenceStates.Delete(orgID)
			cache.refresh.Lock()
			defer cache.refresh.Unlock()
			require.Equal(t, test.want, legacyMetaPolicyFenceStateOf(nil, orgID))
		})
	}
}

func TestLegacyMetaLockHelpersRequireTransactionAndEnsureRequiresFence(t *testing.T) {
	require.Error(t, LockLegacyMetaOrganization(nil, uuid.New()))
	require.Error(t, LockLegacyMetaOrganizationAndShadow(nil, uuid.New(), uuid.New()))
	require.Error(t, LockLegacyMetaOrganizationAndWhatsAppAccount(nil, uuid.New(), uuid.New()))
	_, nilEnsureErr := EnsureLegacyMetaWhatsAppAccount(nil, LegacyMetaAccountRef{ID: uuid.New(), OrganizationID: uuid.New()})
	require.Error(t, nilEnsureErr)
	_, nilRenameErr := StageLegacyMetaWhatsAppAccountRename(nil, uuid.New(), uuid.New(), "before", "after")
	require.Error(t, nilRenameErr)
	db := testutil.SetupTestDB(t)
	org := testutil.CreateTestOrganization(t, db)
	account := testutil.CreateTestWhatsAppAccount(t, db, org.ID)
	ref := LegacyMetaAccountRef{ID: account.ID, OrganizationID: org.ID, Name: account.Name, Status: account.Status}
	require.ErrorContains(t, LockLegacyMetaOrganization(db, org.ID), "inside a transaction")
	require.ErrorContains(t, LockLegacyMetaOrganizationAndShadow(db, org.ID, uuid.New()), "inside a transaction")
	require.ErrorContains(t, LockLegacyMetaOrganizationAndWhatsAppAccount(db, org.ID, account.ID), "inside a transaction")
	_, err := StageLegacyMetaWhatsAppAccountRename(db, org.ID, account.ID, account.Name, "renamed")
	require.ErrorContains(t, err, "inside a transaction")
	_, err = EnsureLegacyMetaWhatsAppAccount(db, ref)
	require.ErrorContains(t, err, "inside a transaction")
	require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
		_, err := EnsureLegacyMetaWhatsAppAccount(tx, ref)
		require.ErrorContains(t, err, "own the organization policy fence")
		if err := database.LockOrganizationPolicyScope(tx, org.ID); err != nil {
			return err
		}
		_, err = EnsureLegacyMetaWhatsAppAccount(tx, ref)
		return err
	}))
}

// K -> writer -> member -> send is three levels below the key holder.
// Every edge is observed in pg_blocking_pids before checking classification;
// a shallower walk misses the running non-member and must fail this test.
func TestLegacyMetaPolicyFenceWalkFindsSendThroughWriterAndMember(t *testing.T) {
	db := testutil.SetupTestDB(t)
	org := testutil.CreateTestOrganization(t, db)
	contact := testutil.CreateTestContact(t, db, org.ID)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	start := func() (*gorm.DB, int) {
		tx := db.WithContext(ctx).Begin()
		require.NoError(t, tx.Error)
		var pid int
		require.NoError(t, tx.Raw("SELECT pg_backend_pid()").Scan(&pid).Error)
		return tx, pid
	}
	blockedBy := func(waiter, blocker int) {
		require.Eventually(t, func() bool {
			var blocked bool
			return db.Raw("SELECT ? = ANY(pg_catalog.pg_blocking_pids(?))", blocker, waiter).Scan(&blocked).Error == nil && blocked
		}, 5*time.Second, 5*time.Millisecond)
	}
	send, sendPID := start()
	defer func() { _ = send.Rollback().Error }()
	require.NoError(t, send.Exec("SELECT id FROM contacts WHERE id = ? FOR UPDATE", contact.ID).Error)
	member, memberPID := start()
	defer func() { _ = member.Rollback().Error }()
	require.NoError(t, member.Exec("SELECT pg_advisory_xact_lock_shared(hashtextextended(?, 0))", legacyMetaPolicyFenceBypassKey(org.ID)).Error)
	require.NoError(t, member.Exec("SELECT id FROM organizations WHERE id = ? FOR SHARE", org.ID).Error)
	writer, writerPID := start()
	writerDone := make(chan error, 1)
	go func() {
		err := writer.Exec("SELECT id FROM organizations WHERE id = ? FOR UPDATE", org.ID).Error
		_ = writer.Rollback().Error
		writerDone <- err
	}()
	defer func() { _ = send.Rollback().Error; _ = member.Rollback().Error; <-writerDone }()
	blockedBy(writerPID, memberPID)
	fence, fencePID := start()
	fenceDone := make(chan error, 1)
	go func() {
		err := database.LockOrganizationPolicyScope(fence, org.ID)
		_ = fence.Rollback().Error
		fenceDone <- err
	}()
	defer func() { _ = send.Rollback().Error; _ = member.Rollback().Error; <-fenceDone }()
	blockedBy(fencePID, writerPID)
	memberDone := make(chan error, 1)
	go func() { memberDone <- member.Exec("SELECT id FROM contacts WHERE id = ? FOR UPDATE", contact.ID).Error }()
	defer func() { _ = send.Rollback().Error; <-memberDone }()
	blockedBy(memberPID, sendPID)
	ForgetLegacyMetaPolicyFenceStatesForTest()
	require.NoError(t, db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		require.Equal(t, legacyMetaPolicyFenceStuck, legacyMetaPolicyFenceStateOf(tx, org.ID))
		return nil
	}))

}
