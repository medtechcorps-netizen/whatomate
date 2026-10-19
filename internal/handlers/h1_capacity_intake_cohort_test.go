package handlers

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Intake and ordinary helpers share the same grant/ownership oracle. Queue
// inspection establishes ordering only; tests never change scheduler state.
func h1IntakeAsync(t *testing.T, g *inboundContinuationAdmission, ctx context.Context, stop <-chan struct{}, maximum func() int, organization string) <-chan h1CapacityAdmissionResult {
	t.Helper()
	ctx, cancel := context.WithCancel(ctx)
	joined := make(chan struct{})
	done := make(chan h1CapacityAdmissionResult, 1)
	go func() {
		defer close(joined)
		release, err := g.acquireIntake(ctx, stop, maximum, organization)
		done <- h1CapacityAdmissionResult{release: release, err: err}
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-joined:
		case <-time.After(5 * time.Second):
			t.Error("intake admission waiter did not join")
		}
		select {
		case got := <-done:
			if got.release != nil {
				got.release()
			}
		default:
		}
	})
	return done
}

func h1IntakeNormal(t *testing.T, g *inboundContinuationAdmission, maximum func() int, mode int) <-chan h1CapacityAdmissionResult {
	t.Helper()
	return h1OrgAcquireAsync(t, g, context.Background(), nil, 1, inboundContinuationFreshPriority, maximum, "tenant", mode)
}

func TestH1IntakeCohort_SnapshotPrecedesPreparationWithoutSpendingQuota(t *testing.T) {
	var g inboundContinuationAdmission
	maximum := func() int { return 10 }
	anchor := h1OrgHold(t, &g, 1, inboundContinuationPerformPriority, maximum, "tenant", inboundContinuationReader)
	preparation := h1IntakeNormal(t, &g, maximum, inboundContinuationWriter)
	h1CapacityWaitQueue(t, &g, 1, 1)
	first := h1IntakeAsync(t, &g, context.Background(), nil, maximum, "tenant")
	h1CapacityWaitQueue(t, &g, 1, 2)
	second := h1IntakeAsync(t, &g, context.Background(), nil, maximum, "tenant")
	h1CapacityWaitQueue(t, &g, 1, 3)
	anchor()
	a := h1OrgGrant(t, first) // This actual normal grant seals both intake identities.
	h1CapacityWaitQueue(t, &g, 1, 2)
	h1OrgBlocked(t, preparation)
	late := []<-chan h1CapacityAdmissionResult{h1IntakeAsync(t, &g, context.Background(), nil, maximum, "tenant")}
	reader := h1IntakeNormal(t, &g, maximum, inboundContinuationReader)
	h1CapacityWaitQueue(t, &g, 1, 4)
	a()
	b := h1OrgGrant(t, second)
	h1OrgBlocked(t, preparation)
	h1OrgBlocked(t, late[0])
	b()
	current := h1OrgGrant(t, preparation)
	// Both normal classes stay queued. New ingress arrives during every
	// preparation grant but cannot extend the sealed writer-turn intake set.
	for ordinal := 2; ordinal <= 50; ordinal++ {
		late = append(late, h1IntakeAsync(t, &g, context.Background(), nil, maximum, "tenant"))
		next := h1IntakeNormal(t, &g, maximum, inboundContinuationWriter)
		h1CapacityWaitQueue(t, &g, 1, len(late)+2)
		current()
		current = h1OrgGrant(t, next)
		h1OrgBlocked(t, reader)
		for _, waiting := range late {
			h1OrgBlocked(t, waiting)
		}
	}
	current()
	r := h1OrgGrant(t, reader)
	for _, waiting := range late {
		h1OrgBlocked(t, waiting)
	}
	r()
	// A later genuine writer turn serves the finite late set in queue order.
	for _, waiting := range late {
		release := h1OrgGrant(t, waiting)
		release()
	}
	h1OrgEmpty(t, &g)
}

func TestH1IntakeCohort_CancelRejoinLosesSnapshotMembership(t *testing.T) {
	var g inboundContinuationAdmission
	maximum := func() int { return 10 }
	anchor := h1OrgHold(t, &g, 1, inboundContinuationPerformPriority, maximum, "tenant", inboundContinuationReader)
	first := h1IntakeAsync(t, &g, context.Background(), nil, maximum, "tenant")
	h1CapacityWaitQueue(t, &g, 1, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	oldAttempt := h1IntakeAsync(t, &g, ctx, nil, maximum, "tenant")
	h1CapacityWaitQueue(t, &g, 1, 2)
	anchor()
	a := h1OrgGrant(t, first)
	cancel()
	got := h1CapacityReceiveAdmission(t, oldAttempt)
	require.ErrorIs(t, got.err, context.Canceled)
	require.Nil(t, got.release)
	h1CapacityWaitQueue(t, &g, 1, 0)
	retry := h1IntakeAsync(t, &g, context.Background(), nil, maximum, "tenant")
	preparation := h1IntakeNormal(t, &g, maximum, inboundContinuationWriter)
	reader := h1IntakeNormal(t, &g, maximum, inboundContinuationReader)
	h1CapacityWaitQueue(t, &g, 1, 3)
	a()
	p := h1OrgGrant(t, preparation)
	h1OrgBlocked(t, retry)
	p()
	r := h1OrgGrant(t, reader)
	h1OrgBlocked(t, retry)
	r()
	newAttempt := h1OrgGrant(t, retry)
	newAttempt()
	h1OrgEmpty(t, &g)
}

func TestH1IntakeCohort_NoReaderRenewalServesPreparationBeforeLateIntake(t *testing.T) {
	var g inboundContinuationAdmission
	maximum := func() int { return 10 }
	first := h1OrgGrant(t, h1IntakeAsync(t, &g, context.Background(), nil, maximum, "tenant"))
	late := h1IntakeAsync(t, &g, context.Background(), nil, maximum, "tenant")
	preparation := h1IntakeNormal(t, &g, maximum, inboundContinuationWriter)
	h1CapacityWaitQueue(t, &g, 1, 2)
	first()
	current := h1OrgGrant(t, preparation)
	for ordinal := 2; ordinal <= 50; ordinal++ {
		next := h1IntakeNormal(t, &g, maximum, inboundContinuationWriter)
		h1CapacityWaitQueue(t, &g, 1, 2)
		current()
		current = h1OrgGrant(t, next)
		h1OrgBlocked(t, late)
	}
	// There is still preparation to do. A genuine normal-turn renewal must
	// now serve the late intake instead of extending preparation indefinitely.
	nextPreparation := h1IntakeNormal(t, &g, maximum, inboundContinuationWriter)
	h1CapacityWaitQueue(t, &g, 1, 2)
	current()
	renewed := h1OrgGrant(t, late)
	h1OrgBlocked(t, nextPreparation)
	reader := h1IntakeNormal(t, &g, maximum, inboundContinuationReader)
	h1CapacityWaitQueue(t, &g, 1, 2)
	renewed()
	current = h1OrgGrant(t, nextPreparation)
	// Renewal, unlike an apply override, starts a fresh preparation quota.
	for ordinal := 2; ordinal <= 50; ordinal++ {
		next := h1IntakeNormal(t, &g, maximum, inboundContinuationWriter)
		h1CapacityWaitQueue(t, &g, 1, 2)
		current()
		current = h1OrgGrant(t, next)
		h1OrgBlocked(t, reader)
	}
	current()
	r := h1OrgGrant(t, reader)
	r()
	h1OrgEmpty(t, &g)
}

func TestH1IntakeCohort_ExhaustedSnapshotRenewsForIngressOnly(t *testing.T) {
	for _, scenario := range []string{"remaining-members-canceled", "members-served"} {
		t.Run(scenario, func(t *testing.T) {
			var g inboundContinuationAdmission
			maximum := func() int { return 10 }
			anchor := h1OrgHold(t, &g, 1, inboundContinuationPerformPriority, maximum, "tenant", inboundContinuationReader)
			first := h1IntakeAsync(t, &g, context.Background(), nil, maximum, "tenant")
			h1CapacityWaitQueue(t, &g, 1, 1)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			second := h1IntakeAsync(t, &g, ctx, nil, maximum, "tenant")
			h1CapacityWaitQueue(t, &g, 1, 2)
			stop := make(chan struct{})
			third := h1IntakeAsync(t, &g, context.Background(), stop, maximum, "tenant")
			h1CapacityWaitQueue(t, &g, 1, 3)
			anchor()
			a := h1OrgGrant(t, first)
			late := h1IntakeAsync(t, &g, context.Background(), nil, maximum, "tenant")
			h1CapacityWaitQueue(t, &g, 1, 3)
			if scenario == "remaining-members-canceled" {
				cancel()
				close(stop)
				got := h1CapacityReceiveAdmission(t, second)
				require.ErrorIs(t, got.err, context.Canceled)
				require.Nil(t, got.release)
				got = h1CapacityReceiveAdmission(t, third)
				require.ErrorIs(t, got.err, errInboundContinuationAdmissionStopped)
				require.Nil(t, got.release)
				h1CapacityWaitQueue(t, &g, 1, 1)
				a()
			} else {
				a()
				b := h1OrgGrant(t, second)
				h1OrgBlocked(t, late)
				b()
				c := h1OrgGrant(t, third)
				h1OrgBlocked(t, late)
				c()
			}
			// Duplicate/disabled routes may never enqueue any reader. This is a
			// new normal turn, not permission to strand or revive old identities.
			d := h1OrgGrant(t, late)
			d()
			h1OrgEmpty(t, &g)
		})
	}
}

func TestH1IntakeCohort_ApplyOverridePreservesSnapshotAndPreparationCount(t *testing.T) {
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
	late := h1IntakeAsync(t, &g, context.Background(), nil, maximum, "tenant")
	lastPreparation := h1IntakeNormal(t, &g, maximum, inboundContinuationWriter)
	apply := h1OrgAcquireAsync(t, &g, context.Background(), nil, 3, inboundContinuationApplyPriority, maximum, "tenant", inboundContinuationReader)
	reader := h1IntakeNormal(t, &g, maximum, inboundContinuationReader)
	h1CapacityWaitQueue(t, &g, 1, 5)
	a()
	accepted := h1OrgGrant(t, apply)
	h1CapacityWaitQueue(t, &g, 3, 4)
	h1OrgBlocked(t, second)
	h1OrgBlocked(t, late)
	h1OrgBlocked(t, lastPreparation)
	accepted()
	b := h1OrgGrant(t, second)
	h1OrgBlocked(t, late)
	b()
	p := h1OrgGrant(t, lastPreparation)
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

func TestH1IntakeCohort_ApplyOnlyWriterDoesNotSealSnapshot(t *testing.T) {
	var g inboundContinuationAdmission
	maximum := func() int { return 10 }
	anchor := h1OrgHold(t, &g, 1, inboundContinuationPerformPriority, maximum, "tenant", inboundContinuationReader)
	first := h1IntakeAsync(t, &g, context.Background(), nil, maximum, "tenant")
	h1CapacityWaitQueue(t, &g, 1, 1)
	apply := h1OrgAcquireAsync(t, &g, context.Background(), nil, 3, inboundContinuationApplyPriority, maximum, "tenant", inboundContinuationWriter)
	h1CapacityWaitQueue(t, &g, 1, 2)
	reader := h1IntakeNormal(t, &g, maximum, inboundContinuationReader)
	h1CapacityWaitQueue(t, &g, 1, 3)
	anchor()
	accepted := h1OrgGrant(t, apply)
	second := h1IntakeAsync(t, &g, context.Background(), nil, maximum, "tenant")
	h1CapacityWaitQueue(t, &g, 3, 3)
	accepted()
	r := h1OrgGrant(t, reader)
	h1OrgBlocked(t, first)
	r()
	a := h1OrgGrant(t, first)
	late := h1IntakeAsync(t, &g, context.Background(), nil, maximum, "tenant")
	nextReader := h1IntakeNormal(t, &g, maximum, inboundContinuationReader)
	h1CapacityWaitQueue(t, &g, 1, 3)
	a()
	b := h1OrgGrant(t, second) // It arrived during apply, before actual snapshot seal.
	h1OrgBlocked(t, late)
	b()
	next := h1OrgGrant(t, nextReader)
	h1OrgBlocked(t, late)
	next()
	c := h1OrgGrant(t, late)
	c()
	h1OrgEmpty(t, &g)
}

func TestH1IntakeCohort_PreservesOtherTenantFIFOAndBlockedCreditHeadroom(t *testing.T) {
	var g inboundContinuationAdmission
	maximum := func() int { return 5 }
	fill1 := h1OrgHold(t, &g, 1, inboundContinuationFreshPriority, maximum, "", inboundContinuationUnscoped)
	fill2 := h1OrgHold(t, &g, 1, inboundContinuationFreshPriority, maximum, "", inboundContinuationUnscoped)
	anchor := h1OrgHold(t, &g, 1, inboundContinuationPerformPriority, maximum, "tenant-a", inboundContinuationReader)
	olderB := h1OrgAcquireAsync(t, &g, context.Background(), nil, 1, inboundContinuationFreshPriority, maximum, "tenant-b", inboundContinuationWriter)
	h1CapacityWaitQueue(t, &g, 3, 1)
	intakeA := h1IntakeAsync(t, &g, context.Background(), nil, maximum, "tenant-a")
	h1CapacityWaitQueue(t, &g, 3, 2)
	anchor()
	b := h1OrgGrant(t, olderB)
	h1OrgBlocked(t, intakeA)
	b()
	a := h1OrgGrant(t, intakeA)
	lateA := h1IntakeAsync(t, &g, context.Background(), nil, maximum, "tenant-a")
	h1CapacityWaitQueue(t, &g, 3, 1)
	readerB := h1OrgAcquireAsync(t, &g, context.Background(), nil, 1, inboundContinuationPerformPriority, maximum, "tenant-b", inboundContinuationReader)
	h1CapacityWaitQueue(t, &g, 3, 2)
	fill1()
	r := h1OrgGrant(t, readerB)
	h1CapacityWaitQueue(t, &g, 3, 1)
	h1OrgBlocked(t, lateA)
	r()
	a()
	late := h1OrgGrant(t, lateA)
	late()
	fill2()
	h1OrgEmpty(t, &g)
}

func TestH1IntakeCohort_UnsealedIntakeAtPreparationLimitPrecedesReader(t *testing.T) {
	var g inboundContinuationAdmission
	maximum := func() int { return 10 }
	current := h1OrgHold(t, &g, 1, inboundContinuationFreshPriority, maximum, "tenant", inboundContinuationWriter)
	for ordinal := 2; ordinal <= 50; ordinal++ {
		next := h1IntakeNormal(t, &g, maximum, inboundContinuationWriter)
		h1CapacityWaitQueue(t, &g, 1, 1)
		current()
		current = h1OrgGrant(t, next)
	}
	reader := h1IntakeNormal(t, &g, maximum, inboundContinuationReader)
	h1CapacityWaitQueue(t, &g, 1, 1)
	first := h1IntakeAsync(t, &g, context.Background(), nil, maximum, "tenant")
	h1CapacityWaitQueue(t, &g, 1, 2)
	current()
	intake := h1OrgGrant(t, first)
	h1OrgBlocked(t, reader)
	late := h1IntakeAsync(t, &g, context.Background(), nil, maximum, "tenant")
	h1CapacityWaitQueue(t, &g, 1, 2)
	intake()
	r := h1OrgGrant(t, reader)
	h1OrgBlocked(t, late)
	r()
	l := h1OrgGrant(t, late)
	l()
	h1OrgEmpty(t, &g)
}

func TestH1IntakeCohort_InvalidIntakeFailsBeforeOwnership(t *testing.T) {
	for _, tc := range []struct {
		name         string
		weight       int
		priority     int
		organization string
		mode         int
	}{
		{"empty-organization", 1, inboundContinuationFreshPriority, "", inboundContinuationWriter},
		{"weight-three", 3, inboundContinuationFreshPriority, "tenant", inboundContinuationWriter},
		{"perform-priority", 1, inboundContinuationPerformPriority, "tenant", inboundContinuationWriter},
		{"apply-priority", 1, inboundContinuationApplyPriority, "tenant", inboundContinuationWriter},
		{"reader-mode", 1, inboundContinuationFreshPriority, "tenant", inboundContinuationReader},
		{"unscoped-mode", 1, inboundContinuationFreshPriority, "", inboundContinuationUnscoped},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var g inboundContinuationAdmission
			var release func()
			var err error
			maximum := func() int { return 10 }
			if tc.name == "empty-organization" {
				release, err = g.acquireIntake(context.Background(), nil, maximum, "")
			} else {
				release, err = g.acquireScopedClass(context.Background(), nil, tc.weight, tc.priority, maximum, tc.organization, tc.mode, true)
			}
			if release != nil {
				t.Cleanup(release)
			}
			require.Error(t, err)
			require.Nil(t, release)
			h1OrgEmpty(t, &g)
		})
	}
}
