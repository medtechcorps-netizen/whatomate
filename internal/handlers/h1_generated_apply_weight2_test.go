package handlers

// Generated-text apply-two admission and ownership regressions. Mixed
// apply-three phases retain their existing accounting and scheduling coverage.

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func h1GeneratedTwoAsync(t *testing.T, ctx context.Context, call func(context.Context) (func(), error)) <-chan h1CapacityAdmissionResult {
	t.Helper()
	ctx, cancel := context.WithCancel(ctx)
	joined := make(chan struct{})
	result := make(chan h1CapacityAdmissionResult, 1)
	go func() {
		defer close(joined)
		release, err := call(ctx)
		result <- h1CapacityAdmissionResult{release: release, err: err}
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-joined:
		case <-time.After(5 * time.Second):
			t.Error("generated admission waiter did not join")
		}
		select {
		case unused := <-result:
			if unused.release != nil {
				unused.release()
			}
		default:
		}
	})
	return result
}

func TestH1GeneratedTwo_PrimitiveRejectsOtherShapes(t *testing.T) {
	for _, tc := range []struct {
		name           string
		priority, mode int
		organization   string
		intake         bool
	}{
		{"fresh", inboundContinuationFreshPriority, inboundContinuationReader, "tenant", false},
		{"perform", inboundContinuationPerformPriority, inboundContinuationReader, "tenant", false},
		{"writer", inboundContinuationApplyPriority, inboundContinuationWriter, "tenant", false},
		{"generic-unscoped", inboundContinuationApplyPriority, inboundContinuationUnscoped, "", false},
		{"empty-organization", inboundContinuationApplyPriority, inboundContinuationReader, "", false},
		{"organization-unscoped", inboundContinuationApplyPriority, inboundContinuationUnscoped, "tenant", false},
		{"intake", inboundContinuationApplyPriority, inboundContinuationReader, "tenant", true},
	} {
		var g inboundContinuationAdmission
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		release, err := g.acquireScopedClass(ctx, nil, 2, tc.priority, func() int { return 10 }, tc.organization, tc.mode, tc.intake)
		cancel()
		if release != nil {
			t.Cleanup(release)
		}
		require.Error(t, err, tc.name)
		require.NotErrorIs(t, err, context.DeadlineExceeded, tc.name+": must reject before waiting")
		require.Nil(t, release, tc.name)
		h1OrgEmpty(t, &g)
	}
	var g inboundContinuationAdmission
	release := h1OrgHold(t, &g, 2, inboundContinuationApplyPriority, func() int { return 10 }, "tenant", inboundContinuationReader)
	h1CapacityWaitQueue(t, &g, 2, 0)
	release()
	release()
	h1OrgEmpty(t, &g)
}

func TestH1GeneratedTwo_AccumulatesWithoutNormalOrSmallerApplyBypass(t *testing.T) {
	for _, olderThree := range []bool{false, true} {
		t.Run(fmt.Sprintf("older-three-%t", olderThree), func(t *testing.T) {
			var g inboundContinuationAdmission
			maximum := func() int { return 10 }
			var held []func()
			for range 8 {
				held = append(held, h1OrgHold(t, &g, 1, inboundContinuationFreshPriority, maximum, "", inboundContinuationUnscoped))
			}
			if !olderThree {
				// Normal work is older. An eligible apply must still reserve the
				// next two credits, even while one-credit normal work would fit.
				normal := h1OrgAcquireAsync(t, &g, context.Background(), nil, 1, inboundContinuationFreshPriority, maximum, "", inboundContinuationUnscoped)
				h1CapacityWaitQueue(t, &g, 8, 1)
				apply := h1OrgAcquireAsync(t, &g, context.Background(), nil, 2, inboundContinuationApplyPriority, maximum, "a", inboundContinuationReader)
				h1CapacityWaitQueue(t, &g, 8, 2)
				held[0]()
				h1CapacityWaitQueue(t, &g, 7, 2)
				h1OrgBlocked(t, apply)
				h1OrgBlocked(t, normal)
				held[1]()
				a := h1OrgGrant(t, apply)
				h1CapacityWaitQueue(t, &g, 8, 1)
				h1OrgBlocked(t, normal)
				a()
				a()
				n := h1OrgGrant(t, normal)
				h1CapacityWaitQueue(t, &g, 7, 0)
				n()
			} else {
				// Different organizations make this a global FIFO/credit test,
				// not an accidental same-organization mode exclusion test.
				older := h1OrgAcquireAsync(t, &g, context.Background(), nil, 3, inboundContinuationApplyPriority, maximum, "old", inboundContinuationWriter)
				h1CapacityWaitQueue(t, &g, 8, 1)
				younger := h1OrgAcquireAsync(t, &g, context.Background(), nil, 2, inboundContinuationApplyPriority, maximum, "new", inboundContinuationReader)
				h1CapacityWaitQueue(t, &g, 8, 2)
				held[0]()
				held[1]()
				h1CapacityWaitQueue(t, &g, 6, 2)
				h1OrgBlocked(t, older)
				h1OrgBlocked(t, younger)
				held[2]()
				a := h1OrgGrant(t, older)
				h1CapacityWaitQueue(t, &g, 8, 1)
				h1OrgBlocked(t, younger)
				a()
				b := h1OrgGrant(t, younger)
				h1CapacityWaitQueue(t, &g, 7, 0)
				b()
			}
			for _, release := range held {
				release()
			}
			h1OrgEmpty(t, &g)
		})
	}
}

func TestH1GeneratedTwo_CancelOrStopReleasesSelectedReservation(t *testing.T) {
	for _, stopInstead := range []bool{false, true} {
		t.Run(fmt.Sprintf("stop-%t", stopInstead), func(t *testing.T) {
			var g inboundContinuationAdmission
			maximum := func() int { return 10 }
			var held []func()
			for range 7 {
				held = append(held, h1OrgHold(t, &g, 1, inboundContinuationFreshPriority, maximum, "", inboundContinuationUnscoped))
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			stop := make(chan struct{})
			apply := h1OrgAcquireAsync(t, &g, ctx, stop, 2, inboundContinuationApplyPriority, maximum, "tenant", inboundContinuationReader)
			h1CapacityWaitQueue(t, &g, 7, 1)
			normal := h1OrgAcquireAsync(t, &g, context.Background(), nil, 1, inboundContinuationFreshPriority, maximum, "", inboundContinuationUnscoped)
			h1CapacityWaitQueue(t, &g, 7, 2)
			h1OrgBlocked(t, normal)
			if stopInstead {
				close(stop)
			} else {
				cancel()
			}
			failed := h1CapacityReceiveAdmission(t, apply)
			if failed.release != nil {
				t.Cleanup(failed.release)
			}
			if stopInstead {
				require.ErrorIs(t, failed.err, errInboundContinuationAdmissionStopped)
			} else {
				require.ErrorIs(t, failed.err, context.Canceled)
			}
			require.Nil(t, failed.release)
			n := h1OrgGrant(t, normal)
			h1CapacityWaitQueue(t, &g, 8, 0)
			n()
			n()
			for _, release := range held {
				release()
			}
			h1OrgEmpty(t, &g)
		})
	}
}

func TestH1GeneratedTwo_PoolLimitsAndResizeKeepActualCharge(t *testing.T) {
	for _, limit := range []int{1, 2, 3, 4} {
		t.Run(fmt.Sprintf("tiny-%d", limit), func(t *testing.T) {
			var g inboundContinuationAdmission
			maximum := func() int { return limit }
			if limit < 3 {
				release, err := g.acquireScoped(context.Background(), nil, 2, inboundContinuationApplyPriority, maximum, "tenant", inboundContinuationReader)
				if release != nil {
					t.Cleanup(release)
				}
				require.Error(t, err)
				require.Nil(t, release)
			} else {
				a := h1OrgHold(t, &g, 2, inboundContinuationApplyPriority, maximum, "tenant", inboundContinuationReader)
				next := h1OrgAcquireAsync(t, &g, context.Background(), nil, 1, inboundContinuationFreshPriority, maximum, "", inboundContinuationUnscoped)
				h1CapacityWaitQueue(t, &g, 2, 1)
				h1OrgBlocked(t, next)
				a()
				a()
				n := h1OrgGrant(t, next)
				n()
			}
			h1OrgEmpty(t, &g)
		})
	}
	t.Run("resize", func(t *testing.T) {
		var g inboundContinuationAdmission
		var limit atomic.Int64
		limit.Store(3)
		maximum := func() int { return int(limit.Load()) }
		a := h1OrgHold(t, &g, 2, inboundContinuationApplyPriority, maximum, "a", inboundContinuationReader)
		limit.Store(10)
		b := h1OrgHold(t, &g, 3, inboundContinuationApplyPriority, maximum, "b", inboundContinuationWriter)
		c := h1OrgHold(t, &g, 3, inboundContinuationApplyPriority, maximum, "c", inboundContinuationWriter)
		h1CapacityWaitQueue(t, &g, 8, 0)
		next := h1OrgAcquireAsync(t, &g, context.Background(), nil, 1, inboundContinuationFreshPriority, maximum, "", inboundContinuationUnscoped)
		h1CapacityWaitQueue(t, &g, 8, 1)
		limit.Store(5) // Reserve remains two; existing ownership may exceed budget.
		b()
		a()
		a()
		h1CapacityWaitQueue(t, &g, 3, 1)
		h1OrgBlocked(t, next)
		c()
		n := h1OrgGrant(t, next)
		n()
		h1OrgEmpty(t, &g)
	})
	t.Run("unlimited-still-fences-organizations", func(t *testing.T) {
		var g inboundContinuationAdmission
		maximum := func() int { return 0 }
		a := h1OrgHold(t, &g, 2, inboundContinuationApplyPriority, maximum, "a", inboundContinuationReader)
		writer := h1OrgAcquireAsync(t, &g, context.Background(), nil, 1, inboundContinuationPerformPriority, maximum, "a", inboundContinuationWriter)
		h1CapacityWaitQueue(t, &g, 2, 1)
		b := h1OrgHold(t, &g, 2, inboundContinuationApplyPriority, maximum, "b", inboundContinuationReader)
		h1CapacityWaitQueue(t, &g, 4, 1)
		h1OrgBlocked(t, writer)
		a()
		w := h1OrgGrant(t, writer)
		h1CapacityWaitQueue(t, &g, 3, 0)
		w()
		b()
		h1OrgEmpty(t, &g)
	})
}

func TestH1GeneratedTwo_OverridePreservesIntakeMembersAndFiftiethPreparation(t *testing.T) {
	var g inboundContinuationAdmission
	maximum := func() int { return 10 }
	current := h1OrgHold(t, &g, 1, inboundContinuationFreshPriority, maximum, "tenant", inboundContinuationWriter)
	for ordinal := 2; ordinal <= 49; ordinal++ {
		next := h1IntakeNormal(t, &g, maximum, inboundContinuationWriter)
		h1CapacityWaitQueue(t, &g, 1, 1)
		current()
		current = h1OrgGrant(t, next)
	}
	first := h1IntakeAsync(t, &g, context.Background(), nil, maximum, "tenant")
	h1CapacityWaitQueue(t, &g, 1, 1)
	second := h1IntakeAsync(t, &g, context.Background(), nil, maximum, "tenant")
	h1CapacityWaitQueue(t, &g, 1, 2)
	current()
	a := h1OrgGrant(t, first)
	// Snapshot identity is observed, never manufactured or mutated by the test.
	g.mu.Lock()
	members := make(map[*inboundContinuationAdmissionWaiter]struct{})
	for member := range g.phases["tenant"].intakeCohort {
		members[member] = struct{}{}
	}
	beforeGrants := g.phases["tenant"].normalGrants
	g.mu.Unlock()
	require.Len(t, members, 1)
	require.Equal(t, 49, beforeGrants)
	late := h1IntakeAsync(t, &g, context.Background(), nil, maximum, "tenant")
	last := h1IntakeNormal(t, &g, maximum, inboundContinuationWriter)
	apply := h1OrgAcquireAsync(t, &g, context.Background(), nil, 2, inboundContinuationApplyPriority, maximum, "tenant", inboundContinuationReader)
	reader := h1IntakeNormal(t, &g, maximum, inboundContinuationReader)
	h1CapacityWaitQueue(t, &g, 1, 5)
	a()
	accepted := h1OrgGrant(t, apply)
	h1CapacityWaitQueue(t, &g, 2, 4)
	g.mu.Lock()
	retained := make(map[*inboundContinuationAdmissionWaiter]struct{})
	for member := range g.phases["tenant"].intakeCohort {
		retained[member] = struct{}{}
	}
	grants := g.phases["tenant"].normalGrants
	mode, activeMode := g.phases["tenant"].mode, g.phases["tenant"].activeMode
	g.mu.Unlock()
	require.Equal(t, members, retained)
	require.Equal(t, 49, grants)
	require.Equal(t, inboundContinuationWriter, mode)
	require.Equal(t, inboundContinuationReader, activeMode)
	h1OrgBlocked(t, second)
	h1OrgBlocked(t, late)
	h1OrgBlocked(t, last)
	h1OrgBlocked(t, reader)
	other := h1OrgHold(t, &g, 2, inboundContinuationApplyPriority, maximum, "other", inboundContinuationReader)
	h1CapacityWaitQueue(t, &g, 4, 4)
	other()
	accepted()
	b := h1OrgGrant(t, second)
	h1OrgBlocked(t, late)
	b()
	p := h1OrgGrant(t, last)
	g.mu.Lock()
	grants = g.phases["tenant"].normalGrants
	g.mu.Unlock()
	require.Equal(t, 50, grants)
	h1OrgBlocked(t, reader)
	h1OrgBlocked(t, late)
	p()
	r := h1OrgGrant(t, reader)
	h1OrgBlocked(t, late)
	r()
	l := h1OrgGrant(t, late)
	l()
	h1OrgEmpty(t, &g)
}

func h1GeneratedTwoInputs(org uuid.UUID) (*persistedIncomingMessage, *preparedIncomingContinuation, *capturedLegacyAI) {
	contact := &models.Contact{OrganizationID: org}
	contact.ID = uuid.New()
	settings := &models.ChatbotSettings{OrganizationID: org}
	settings.AI.Enabled = true
	settings.AI.Provider, settings.AI.APIKey = "synthetic-provider", "synthetic-key"
	session := &models.ChatbotSession{OrganizationID: org, ContactID: contact.ID}
	session.ID = uuid.New()
	work := &persistedIncomingMessage{OrganizationID: org, Contact: *contact}
	prepared := &preparedIncomingContinuation{contact: contact, settings: settings, session: session, legacyAI: true}
	captured := &capturedLegacyAI{result: models.JSONB{"outcome": "generated", "answer": "synthetic answer"}}
	return work, prepared, captured
}

func TestH1GeneratedTwo_DedicatedHelperBindsOwnershipBeforeQueueOrSQL(t *testing.T) {
	db, sqlPool := h1OpenPool(t, 10)
	root := h1RootApp(db, true)
	org, otherOrg := uuid.New(), uuid.New()
	execution := h1NestedApp(root, org)
	work, prepared, captured := h1GeneratedTwoInputs(org)
	marked := root.markInboundContinuationContext(context.Background())
	// Admission must not borrow SQL, even when no physical connection is free.
	// These are real held connections, separate from the seven queue credits.
	freePhysical := h1HoldConnections(t, sqlPool, 10)
	before := sqlPool.Stats()
	var held []func()
	for range 7 {
		held = append(held, h1OrgHold(t, &root.continuationAdmission, 1, inboundContinuationFreshPriority, func() int { return 10 }, "", inboundContinuationUnscoped))
	}
	apply := h1GeneratedTwoAsync(t, marked, func(ctx context.Context) (func(), error) {
		return execution.acquireInboundContinuationGeneratedApply(ctx, nil, work, prepared, captured)
	})
	h1CapacityWaitQueue(t, &root.continuationAdmission, 7, 1)
	root.continuationAdmission.mu.Lock()
	queued := *root.continuationAdmission.waiters[0]
	root.continuationAdmission.mu.Unlock()
	require.Equal(t, org.String(), queued.organization)
	require.Equal(t, inboundContinuationReader, queued.mode)
	require.Equal(t, inboundContinuationApplyPriority, queued.priority)
	require.False(t, queued.intake)
	normal := h1OrgAcquireAsync(t, &root.continuationAdmission, context.Background(), nil, 1, inboundContinuationFreshPriority, func() int { return 10 }, "", inboundContinuationUnscoped)
	h1CapacityWaitQueue(t, &root.continuationAdmission, 7, 2)
	h1OrgBlocked(t, normal)
	held[0]()
	a := h1OrgGrant(t, apply)
	h1CapacityWaitQueue(t, &root.continuationAdmission, 8, 1)
	require.False(t, execution.inboundContinuation.attemptGuarded)
	require.False(t, execution.inboundContinuation.detachedGeneratedApply)
	require.Equal(t, before.InUse, sqlPool.Stats().InUse)
	require.Equal(t, before.WaitCount, sqlPool.Stats().WaitCount)
	a()
	a()
	n := h1OrgGrant(t, normal)
	n()
	for _, release := range held {
		release()
	}
	h1OrgEmpty(t, &root.continuationAdmission)

	// Existing classifier tests cover its complete input matrix. These cases
	// add the dedicated ownership boundary and representative mixed captures.
	for _, name := range []string{"nil-work", "work-org", "contact-org", "session-org", "settings-org", "nil-prepared", "nil-contact", "nil-session", "nil-settings", "nil-capture", "errored-capture", "mixed-route", "empty-answer", "no-execution", "empty-execution-org", "tenant-receiver", "unmarked", "wrong-root-context"} {
		x := h1NestedApp(root, org)
		w, p, c := h1GeneratedTwoInputs(org)
		ctx := marked
		switch name {
		case "nil-work":
			w = nil
		case "work-org":
			w.OrganizationID = otherOrg
		case "contact-org":
			p.contact.OrganizationID = otherOrg
		case "session-org":
			p.session.OrganizationID = otherOrg
		case "settings-org":
			p.settings.OrganizationID = otherOrg
		case "nil-prepared":
			p = nil
		case "nil-contact":
			p.contact = nil
		case "nil-session":
			p.session = nil
		case "nil-settings":
			p.settings = nil
		case "nil-capture":
			c = nil
		case "errored-capture":
			c.err = errors.New("synthetic capture failure")
		case "mixed-route":
			p.legacyAI = false
		case "empty-answer":
			c.result["answer"] = ""
		case "no-execution":
			x.inboundContinuation = nil
		case "empty-execution-org":
			x.inboundContinuation.OrganizationID = uuid.Nil
		case "tenant-receiver":
			x.tenantOrgID = org
		case "unmarked":
			ctx = context.Background()
		case "wrong-root-context":
			ctx = context.WithValue(context.Background(), inboundContinuationAdmissionContextKey{}, h1RootApp(db, true))
		}
		deadline, cancel := context.WithTimeout(ctx, time.Second)
		release, err := x.acquireInboundContinuationGeneratedApply(deadline, nil, w, p, c)
		cancel()
		if release != nil {
			t.Cleanup(release)
		}
		require.Error(t, err, name)
		require.NotErrorIs(t, err, context.DeadlineExceeded, name+": validation must not wait")
		require.Nil(t, release, name)
		h1OrgEmpty(t, &root.continuationAdmission)
	}
	require.Equal(t, before.InUse, sqlPool.Stats().InUse)
	require.Equal(t, before.WaitCount, sqlPool.Stats().WaitCount)
	freePhysical()
	// A shared root pointer does not make a different SQL receiver safe.
	foreign, _ := h1OpenPool(t, 10)
	transaction := db.Begin()
	require.NoError(t, transaction.Error)
	t.Cleanup(func() { _ = transaction.Rollback().Error })
	for _, receiver := range []*gorm.DB{foreign, transaction} {
		x := h1NestedApp(root, org)
		x.DB = receiver
		release, err := x.acquireInboundContinuationGeneratedApply(marked, nil, work, prepared, captured)
		if release != nil {
			t.Cleanup(release)
		}
		require.Error(t, err)
		require.Nil(t, release)
		h1OrgEmpty(t, &root.continuationAdmission)
	}
	require.NoError(t, transaction.Rollback().Error)
	require.NoError(t, db.Connection(func(pinned *gorm.DB) error {
		x := h1NestedApp(root, org)
		x.DB = pinned
		release, err := x.acquireInboundContinuationGeneratedApply(marked, nil, work, prepared, captured)
		if release != nil {
			t.Cleanup(release)
		}
		require.Error(t, err)
		require.Nil(t, release)
		h1OrgEmpty(t, &root.continuationAdmission)
		return nil
	}))
	for _, call := range []func() (func(), error){
		func() (func(), error) { return execution.acquireInboundContinuationCapacity(marked, nil, 2) },
		func() (func(), error) {
			return execution.acquireInboundContinuationPhase(marked, nil, 2, org.String(), inboundContinuationReader)
		},
		func() (func(), error) {
			return execution.acquireInboundContinuationPhase(context.Background(), nil, 2, org.String(), inboundContinuationReader)
		},
		func() (func(), error) { return execution.acquireIncomingMessageIntake(marked, org.String()) },
	} {
		release, err := call()
		if release != nil {
			t.Cleanup(release)
		}
		require.Error(t, err)
		require.Nil(t, release)
		h1OrgEmpty(t, &root.continuationAdmission)
	}
	// No new blanket rejection is added to the existing non-RLS wrappers.
	nonRLS := h1RootApp(db, false)
	x := h1NestedApp(nonRLS, org)
	plain := nonRLS.markInboundContinuationContext(context.Background()) // intentionally unmarked
	for _, call := range []func() (func(), error){
		func() (func(), error) { return x.acquireInboundContinuationCapacity(plain, nil, 2) },
		func() (func(), error) {
			return x.acquireInboundContinuationPhase(plain, nil, 2, "", inboundContinuationWriter)
		},
		func() (func(), error) {
			return x.acquireInboundContinuationGeneratedApply(plain, nil, work, prepared, captured)
		},
	} {
		release, err := call()
		if release != nil {
			t.Cleanup(release)
		}
		require.NoError(t, err)
		require.NotNil(t, release)
		release()
		release()
		h1OrgEmpty(t, &nonRLS.continuationAdmission)
	}
	badWork, p, c := h1GeneratedTwoInputs(otherOrg)
	release, err := x.acquireInboundContinuationGeneratedApply(plain, nil, badWork, p, c)
	if release != nil {
		t.Cleanup(release)
	}
	require.Error(t, err)
	require.Nil(t, release)
	h1OrgEmpty(t, &nonRLS.continuationAdmission)
}
