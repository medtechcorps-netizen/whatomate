package handlers

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestH1ReaderPreparation_RetentionChecksPoolAndReceiverWithoutBorrowingSQL(t *testing.T) {
	db, pool := h1OpenPool(t, 10)
	root := h1RootApp(db, true)
	org := uuid.New()
	x := h1NestedApp(root, org)
	g := &root.continuationAdmission
	hold := h1OrgHold(t, g, 1, inboundContinuationFreshPriority, func() int { return 10 }, org.String(), inboundContinuationReader)
	freeSQL := h1HoldConnections(t, pool, 10)
	before := pool.Stats()
	require.True(t, x.canRetainInboundPreparationForPerform())
	for _, name := range []string{"nil-execution", "nil-org", "tenant-receiver", "tenant-root", "nil-db", "no-owned-phase"} {
		y := h1NestedApp(root, org)
		switch name {
		case "nil-execution":
			y.inboundContinuation = nil
		case "nil-org":
			y.inboundContinuation.OrganizationID = uuid.Nil
		case "tenant-receiver":
			y.tenantOrgID = org
		case "tenant-root":
			root.tenantOrgID = org
		case "nil-db":
			y.DB = nil
		case "no-owned-phase":
			y.inboundContinuation.OrganizationID = uuid.New()
		}
		require.False(t, y.canRetainInboundPreparationForPerform(), name)
		root.tenantOrgID = uuid.Nil
	}
	require.Equal(t, before.InUse, pool.Stats().InUse)
	require.Equal(t, before.WaitCount, pool.Stats().WaitCount, "validation cannot borrow from the saturated root pool")
	freeSQL()
	foreign, _ := h1OpenPool(t, 10)
	tx := db.Begin()
	require.NoError(t, tx.Error)
	t.Cleanup(func() { _ = tx.Rollback().Error })
	for _, receiver := range []*gorm.DB{foreign, tx} {
		y := h1NestedApp(root, org)
		y.DB = receiver
		require.False(t, y.canRetainInboundPreparationForPerform())
	}
	require.NoError(t, tx.Rollback().Error)
	require.NoError(t, db.Connection(func(borrowed *gorm.DB) error {
		y := h1NestedApp(root, org)
		y.DB = borrowed
		require.False(t, y.canRetainInboundPreparationForPerform())
		return nil
	}))
	hold()
	h1OrgEmpty(t, g)
}

func TestH1ReaderPreparation_RetentionUsesCurrentPoolLimitWithoutGrantingAgain(t *testing.T) {
	for _, tc := range []struct {
		name            string
		maximum, owners int
		want            bool
	}{
		{"unlimited", 0, 8, true}, {"one", 1, 1, false}, {"two", 2, 1, false},
		{"three-single", 3, 1, true}, {"three-shrunk", 3, 2, false},
		{"four-single", 4, 1, true}, {"four-shrunk", 4, 2, false},
		{"five-at-budget", 5, 3, true}, {"five-shrunk", 5, 4, false},
		{"ten-at-budget", 10, 8, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, pool := h1OpenPool(t, 10)
			root := h1RootApp(db, true)
			org := uuid.New()
			x := h1NestedApp(root, org)
			g := &root.continuationAdmission
			var held []func()
			for range tc.owners {
				held = append(held, h1OrgHold(t, g, 1, inboundContinuationFreshPriority, func() int { return 10 }, org.String(), inboundContinuationReader))
			}
			pool.SetMaxOpenConns(tc.maximum)
			t.Cleanup(func() { pool.SetMaxOpenConns(10) })
			require.Equal(t, tc.want, x.canRetainInboundPreparationForPerform())
			g.mu.Lock()
			used, grants, active := g.used, g.phases[org.String()].normalGrants, g.phases[org.String()].active
			g.mu.Unlock()
			require.Equal(t, tc.owners, used)
			require.Equal(t, tc.owners, grants, "retention is not a second phase grant")
			require.Equal(t, tc.owners, active)
			for _, release := range held {
				release()
			}
			h1OrgEmpty(t, g)
		})
	}
}

func TestH1ReaderPreparation_QueuedApplyForcesYieldEvenWhenIncompatible(t *testing.T) {
	for _, mode := range []struct {
		name         string
		mode, weight int
	}{
		{"eligible-reader-apply", inboundContinuationReader, 2},
		{"incompatible-writer-apply", inboundContinuationWriter, 3},
		{"foreign-incompatible-writer-apply", inboundContinuationWriter, 3},
	} {
		t.Run(mode.name, func(t *testing.T) {
			db, pool := h1OpenPool(t, 10)
			root := h1RootApp(db, true)
			org := uuid.New()
			x := h1NestedApp(root, org)
			g := &root.continuationAdmission
			maximum := func() int { return pool.Stats().MaxOpenConnections }
			current := h1OrgHold(t, g, 1, inboundContinuationFreshPriority, maximum, org.String(), inboundContinuationReader)
			var filler []func()
			for range 6 {
				filler = append(filler, h1OrgHold(t, g, 1, inboundContinuationFreshPriority, maximum, "", inboundContinuationUnscoped))
			}
			applyOrg, expectedUsed := org.String(), 7
			if mode.name == "foreign-incompatible-writer-apply" {
				applyOrg = uuid.NewString()
				filler = append(filler, h1OrgHold(t, g, 1, inboundContinuationFreshPriority, maximum, applyOrg, inboundContinuationReader))
				expectedUsed++
			}
			require.True(t, x.canRetainInboundPreparationForPerform())
			applyCtx, cancel := context.WithCancel(context.Background())
			defer cancel()
			apply := h1OrgAcquireAsync(t, g, applyCtx, nil, mode.weight, inboundContinuationApplyPriority, maximum, applyOrg, mode.mode)
			h1CapacityWaitQueue(t, g, expectedUsed, 1)
			require.False(t, x.canRetainInboundPreparationForPerform())
			h1OrgBlocked(t, apply)
			cancel()
			got := h1CapacityReceiveAdmission(t, apply)
			require.ErrorIs(t, got.err, context.Canceled)
			require.Nil(t, got.release)
			h1CapacityWaitQueue(t, g, expectedUsed, 0)
			require.True(t, x.canRetainInboundPreparationForPerform(), "cancelled apply no longer prevents the existing reader's transition")
			current()
			for _, release := range filler {
				release()
			}
			h1OrgEmpty(t, g)
		})
	}
}

func TestH1ReaderPreparation_RetainedFiftiethGrantDoesNotReopenReaderCohort(t *testing.T) {
	db, _ := h1OpenPool(t, 10)
	root := h1RootApp(db, true)
	org := uuid.New()
	x := h1NestedApp(root, org)
	g := &root.continuationAdmission
	maximum := func() int { return 10 }
	anchor := h1OrgHold(t, g, 1, inboundContinuationFreshPriority, maximum, org.String(), inboundContinuationReader)
	for range 48 {
		release := h1OrgHold(t, g, 1, inboundContinuationFreshPriority, maximum, org.String(), inboundContinuationReader)
		release()
	}
	last := h1OrgHold(t, g, 1, inboundContinuationFreshPriority, maximum, org.String(), inboundContinuationReader)
	writer := h1OrgAcquireAsync(t, g, context.Background(), nil, 1, inboundContinuationFreshPriority, maximum, org.String(), inboundContinuationWriter)
	h1CapacityWaitQueue(t, g, 2, 1)
	require.True(t, x.canRetainInboundPreparationForPerform(), "already-owned fiftieth operation may proceed without a new grant")
	reader := h1OrgAcquireAsync(t, g, context.Background(), nil, 1, inboundContinuationPerformPriority, maximum, org.String(), inboundContinuationReader)
	h1CapacityWaitQueue(t, g, 2, 2)
	h1OrgBlocked(t, writer)
	h1OrgBlocked(t, reader)
	anchor()
	h1CapacityWaitQueue(t, g, 1, 2)
	h1OrgBlocked(t, writer)
	h1OrgBlocked(t, reader)
	last()
	w := h1OrgGrant(t, writer)
	h1CapacityWaitQueue(t, g, 1, 1)
	h1OrgBlocked(t, reader)
	w()
	r := h1OrgGrant(t, reader)
	r()
	h1OrgEmpty(t, g)
}
