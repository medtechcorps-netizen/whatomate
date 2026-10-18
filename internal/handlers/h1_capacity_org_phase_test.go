package handlers

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/stretchr/testify/require"
)

// These tests observe grants and retained ownership. Queue inspection is only
// a scheduling barrier; no assertion depends on the selector's implementation.
// All existing generic-admission and database regression tests remain intact.
func h1OrgAcquireAsync(t *testing.T, g *inboundContinuationAdmission, ctx context.Context, stop <-chan struct{}, weight, priority int, maximum func() int, organization string, mode int) <-chan h1CapacityAdmissionResult {
	t.Helper()
	ctx, cancel := context.WithCancel(ctx)
	joined := make(chan struct{})
	result := make(chan h1CapacityAdmissionResult, 1)
	go func() {
		defer close(joined)
		release, err := g.acquireScoped(ctx, stop, weight, priority, maximum, organization, mode)
		result <- h1CapacityAdmissionResult{release: release, err: err}
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-joined:
		case <-time.After(5 * time.Second):
			t.Error("scoped admission waiter did not join")
		}
		select {
		case unconsumed := <-result:
			if unconsumed.release != nil {
				unconsumed.release()
			}
		default:
		}
	})
	return result
}

func h1OrgGrant(t *testing.T, result <-chan h1CapacityAdmissionResult) func() {
	t.Helper()
	got := h1CapacityReceiveAdmission(t, result)
	require.NoError(t, got.err)
	require.NotNil(t, got.release)
	t.Cleanup(got.release)
	return got.release
}

func h1OrgHold(t *testing.T, g *inboundContinuationAdmission, weight, priority int, maximum func() int, organization string, mode int) func() {
	t.Helper()
	return h1OrgGrant(t, h1OrgAcquireAsync(t, g, context.Background(), nil, weight, priority, maximum, organization, mode))
}

func h1OrgBlocked(t *testing.T, result <-chan h1CapacityAdmissionResult) {
	t.Helper()
	select {
	case got := <-result:
		if got.release != nil {
			got.release()
		}
		t.Fatalf("blocked organization waiter settled: %v", got.err)
	default:
	}
}

func h1OrgEmpty(t *testing.T, g *inboundContinuationAdmission) {
	t.Helper()
	require.Eventually(t, func() bool {
		g.mu.Lock()
		defer g.mu.Unlock()
		return g.used == 0 && len(g.waiters) == 0 && len(g.phases) == 0
	}, 2*time.Second, time.Millisecond)
}

func TestH1OrgPhase_ReadersOverlapWriterIsExclusiveAndBlockedTenantOwnsNoCredits(t *testing.T) {
	var g inboundContinuationAdmission
	maximum := func() int { return 10 }
	a1 := h1OrgHold(t, &g, 1, inboundContinuationPerformPriority, maximum, "tenant-a", inboundContinuationReader)
	a2 := h1OrgHold(t, &g, 1, inboundContinuationPerformPriority, maximum, "tenant-a", inboundContinuationReader)
	writer := h1OrgAcquireAsync(t, &g, context.Background(), nil, 3, inboundContinuationApplyPriority, maximum, "tenant-a", inboundContinuationWriter)
	h1CapacityWaitQueue(t, &g, 2, 1)

	// An older but organization-ineligible weight3 apply must not reserve the
	// global pool against an already enqueued independent tenant.
	b := h1OrgHold(t, &g, 3, inboundContinuationApplyPriority, maximum, "tenant-b", inboundContinuationWriter)
	h1CapacityWaitQueue(t, &g, 5, 1)
	h1OrgBlocked(t, writer)
	a1()
	h1CapacityWaitQueue(t, &g, 4, 1)
	h1OrgBlocked(t, writer)
	a2()
	w := h1OrgGrant(t, writer)
	h1CapacityWaitQueue(t, &g, 6, 0)
	reader := h1OrgAcquireAsync(t, &g, context.Background(), nil, 1, inboundContinuationPerformPriority, maximum, "tenant-a", inboundContinuationReader)
	nextWriter := h1OrgAcquireAsync(t, &g, context.Background(), nil, 1, inboundContinuationFreshPriority, maximum, "tenant-a", inboundContinuationWriter)
	h1CapacityWaitQueue(t, &g, 6, 2)
	b()
	h1CapacityWaitQueue(t, &g, 3, 2)
	h1OrgBlocked(t, reader)
	h1OrgBlocked(t, nextWriter)
	w()
	// The writer apply was an override of the prior reader cohort. It does
	// not create a fresh writer cohort or spend/reset the normal reader quota.
	r := h1OrgGrant(t, reader)
	h1CapacityWaitQueue(t, &g, 1, 1)
	h1OrgBlocked(t, nextWriter)
	r()
	w2 := h1OrgGrant(t, nextWriter)
	w2()
	h1OrgEmpty(t, &g)
}

func TestH1OrgPhase_BothModesProgressWithContinuouslyQueuedNormalWork(t *testing.T) {
	for _, first := range []struct {
		name string
		mode int
	}{{"reader-first", inboundContinuationReader}, {"writer-first", inboundContinuationWriter}} {
		t.Run(first.name, func(t *testing.T) {
			var g inboundContinuationAdmission
			maximum := func() int { return 5 }
			// Two generic holders leave exactly one recyclable credit. This
			// prevents coincident grants from hiding a mode handoff or bypass.
			g1 := h1OrgHold(t, &g, 1, inboundContinuationFreshPriority, maximum, "", inboundContinuationUnscoped)
			g2 := h1OrgHold(t, &g, 1, inboundContinuationFreshPriority, maximum, "", inboundContinuationUnscoped)
			other := inboundContinuationReader
			if first.mode == other {
				other = inboundContinuationWriter
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			current := h1OrgHold(t, &g, 1, inboundContinuationFreshPriority, maximum, "tenant", first.mode)
			queued := map[int]<-chan h1CapacityAdmissionResult{}
			for _, mode := range []int{other, first.mode} {
				queued[mode] = h1OrgAcquireAsync(t, &g, ctx, nil, 1, inboundContinuationFreshPriority, maximum, "tenant", mode)
			}
			h1CapacityWaitQueue(t, &g, 3, 2)
			granted := map[int]int{first.mode: 1}
			// The prototype's selected finite bound is the existing batch50,
			// independent of pool size. Both mode queues stay nonempty throughout
			// two full phases; no quiet gap is manufactured for the other mode.
			const cohort = 50
			for ordinal := 2; ordinal <= 2*cohort+1; ordinal++ {
				want := first.mode
				if ordinal > cohort && ordinal <= 2*cohort {
					want = other
				}
				current()
				current = h1OrgGrant(t, queued[want])
				granted[want]++
				delete(queued, want)
				for _, waiting := range queued {
					h1OrgBlocked(t, waiting)
				}
				queued[want] = h1OrgAcquireAsync(t, &g, ctx, nil, 1, inboundContinuationFreshPriority, maximum, "tenant", want)
				h1CapacityWaitQueue(t, &g, 3, 2)
			}
			require.Equal(t, cohort+1, granted[first.mode])
			require.Equal(t, cohort, granted[other])
			cancel()
			for _, waiting := range queued {
				got := h1CapacityReceiveAdmission(t, waiting)
				require.ErrorIs(t, got.err, context.Canceled)
				require.Nil(t, got.release)
			}
			current()
			g1()
			g2()
			h1OrgEmpty(t, &g)
		})
	}
}

func TestH1OrgPhase_CanceledAndStoppedWaitersDoNotSpendNormalQuota(t *testing.T) {
	var g inboundContinuationAdmission
	maximum := func() int { return 5 }
	g1 := h1OrgHold(t, &g, 1, inboundContinuationFreshPriority, maximum, "", inboundContinuationUnscoped)
	g2 := h1OrgHold(t, &g, 1, inboundContinuationFreshPriority, maximum, "", inboundContinuationUnscoped)
	reader := h1OrgHold(t, &g, 1, inboundContinuationPerformPriority, maximum, "tenant", inboundContinuationReader)
	writer := h1OrgAcquireAsync(t, &g, context.Background(), nil, 1, inboundContinuationFreshPriority, maximum, "tenant", inboundContinuationWriter)
	h1CapacityWaitQueue(t, &g, 3, 1)
	for i := 0; i < 60; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		stop := make(chan struct{})
		waiting := h1OrgAcquireAsync(t, &g, ctx, stop, 1, inboundContinuationPerformPriority, maximum, "tenant", inboundContinuationReader)
		h1CapacityWaitQueue(t, &g, 3, 2)
		if i%2 == 0 {
			cancel()
		} else {
			close(stop)
		}
		got := h1CapacityReceiveAdmission(t, waiting)
		if i%2 == 0 {
			require.ErrorIs(t, got.err, context.Canceled)
		} else {
			require.ErrorIs(t, got.err, errInboundContinuationAdmissionStopped)
		}
		cancel()
		require.Nil(t, got.release)
		h1CapacityWaitQueue(t, &g, 3, 1)
	}
	next := h1OrgAcquireAsync(t, &g, context.Background(), nil, 1, inboundContinuationPerformPriority, maximum, "tenant", inboundContinuationReader)
	h1CapacityWaitQueue(t, &g, 3, 2)
	reader()
	r := h1OrgGrant(t, next)
	h1OrgBlocked(t, writer)
	r()
	w := h1OrgGrant(t, writer)
	w()
	g1()
	g2()
	h1OrgEmpty(t, &g)
}

func TestH1OrgPhase_AcceptedApplyDrainsClosedReaderPhaseWithoutReopeningIt(t *testing.T) {
	var g inboundContinuationAdmission
	maximum := func() int { return 10 }
	anchor := h1OrgHold(t, &g, 1, inboundContinuationPerformPriority, maximum, "tenant-a", inboundContinuationReader)
	for i := 1; i < 50; i++ {
		r := h1OrgHold(t, &g, 1, inboundContinuationPerformPriority, maximum, "tenant-a", inboundContinuationReader)
		r()
	}
	writer := h1OrgAcquireAsync(t, &g, context.Background(), nil, 1, inboundContinuationFreshPriority, maximum, "tenant-a", inboundContinuationWriter)
	reader := h1OrgAcquireAsync(t, &g, context.Background(), nil, 1, inboundContinuationPerformPriority, maximum, "tenant-a", inboundContinuationReader)
	h1CapacityWaitQueue(t, &g, 1, 2)
	// This finite backlog represents results from work admitted before phase
	// closure. It is not an assertion of fairness under infinite apply input.
	apply1 := h1OrgHold(t, &g, 3, inboundContinuationApplyPriority, maximum, "tenant-a", inboundContinuationReader)
	apply2 := h1OrgHold(t, &g, 3, inboundContinuationApplyPriority, maximum, "tenant-a", inboundContinuationReader)
	h1CapacityWaitQueue(t, &g, 7, 2)
	h1OrgBlocked(t, reader)
	anchor()
	apply1()
	h1CapacityWaitQueue(t, &g, 3, 2)
	h1OrgBlocked(t, writer)
	h1OrgBlocked(t, reader)
	apply2()
	w := h1OrgGrant(t, writer)
	h1CapacityWaitQueue(t, &g, 1, 1)
	h1OrgBlocked(t, reader)
	w()
	r := h1OrgGrant(t, reader)
	r()
	h1OrgEmpty(t, &g)
}

func TestH1OrgPhase_EligibleApplyAccumulatesCreditsAndCancellationReleasesReservation(t *testing.T) {
	var g inboundContinuationAdmission
	maximum := func() int { return 10 }
	holds := make([]func(), 8)
	for i := range holds {
		holds[i] = h1OrgHold(t, &g, 1, inboundContinuationFreshPriority, maximum, "", inboundContinuationUnscoped)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	apply := h1OrgAcquireAsync(t, &g, ctx, nil, 3, inboundContinuationApplyPriority, maximum, "tenant-a", inboundContinuationWriter)
	h1CapacityWaitQueue(t, &g, 8, 1)
	small := h1OrgAcquireAsync(t, &g, context.Background(), nil, 1, inboundContinuationFreshPriority, maximum, "tenant-b", inboundContinuationWriter)
	h1CapacityWaitQueue(t, &g, 8, 2)
	holds[0]()
	h1CapacityWaitQueue(t, &g, 7, 2)
	h1OrgBlocked(t, small)
	holds[1]()
	h1CapacityWaitQueue(t, &g, 6, 2)
	h1OrgBlocked(t, small)
	cancel()
	got := h1CapacityReceiveAdmission(t, apply)
	require.ErrorIs(t, got.err, context.Canceled)
	require.Nil(t, got.release)
	b := h1OrgGrant(t, small)
	h1CapacityWaitQueue(t, &g, 7, 0)
	b()
	// Cancellation before a grant must not leave phantom writer ownership.
	a := h1OrgHold(t, &g, 1, inboundContinuationPerformPriority, maximum, "tenant-a", inboundContinuationReader)
	a()
	for _, release := range holds {
		release()
	}
	h1OrgEmpty(t, &g)
}

func TestH1OrgPhase_CancelOppositeTurnAndIdempotentReleasePermitCurrentMode(t *testing.T) {
	var g inboundContinuationAdmission
	maximum := func() int { return 10 }
	anchor := h1OrgHold(t, &g, 1, inboundContinuationPerformPriority, maximum, "tenant", inboundContinuationReader)
	for i := 1; i < 50; i++ {
		r := h1OrgHold(t, &g, 1, inboundContinuationPerformPriority, maximum, "tenant", inboundContinuationReader)
		r()
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	writer := h1OrgAcquireAsync(t, &g, ctx, nil, 1, inboundContinuationFreshPriority, maximum, "tenant", inboundContinuationWriter)
	reader := h1OrgAcquireAsync(t, &g, context.Background(), nil, 1, inboundContinuationPerformPriority, maximum, "tenant", inboundContinuationReader)
	h1CapacityWaitQueue(t, &g, 1, 2)
	cancel()
	got := h1CapacityReceiveAdmission(t, writer)
	require.ErrorIs(t, got.err, context.Canceled)
	r := h1OrgGrant(t, reader)
	h1CapacityWaitQueue(t, &g, 2, 0)
	anchor()
	anchor()
	r()
	r()
	h1OrgEmpty(t, &g)
	// A wholly drained organization starts a fresh phase with no inherited
	// quota, owner or mandatory abandoned turn.
	w := h1OrgHold(t, &g, 1, inboundContinuationFreshPriority, maximum, "tenant", inboundContinuationWriter)
	w()
	h1OrgEmpty(t, &g)
}

func TestH1OrgPhase_OppositeAcceptedApplyStopsReplenishmentAndPreservesNormalQuota(t *testing.T) {
	for _, scenario := range []struct {
		name string
		mode int
	}{{"reader-apply-during-writer-phase", inboundContinuationWriter}, {"writer-apply-during-reader-phase", inboundContinuationReader}} {
		t.Run(scenario.name, func(t *testing.T) {
			var g inboundContinuationAdmission
			maximum := func() int { return 10 }
			generic := make([]func(), 7)
			for i := range generic {
				generic[i] = h1OrgHold(t, &g, 1, inboundContinuationFreshPriority, maximum, "", inboundContinuationUnscoped)
			}
			other := inboundContinuationReader
			if scenario.mode == other {
				other = inboundContinuationWriter
			}
			current := h1OrgHold(t, &g, 1, inboundContinuationFreshPriority, maximum, "tenant", scenario.mode)
			oppositeNormal := h1OrgAcquireAsync(t, &g, context.Background(), nil, 1, inboundContinuationFreshPriority, maximum, "tenant", other)
			// Keep both mode queues represented and charge exactly49 normal
			// grants. The final grant is still active when the late result arrives.
			for count := 2; count <= 49; count++ {
				next := h1OrgAcquireAsync(t, &g, context.Background(), nil, 1, inboundContinuationFreshPriority, maximum, "tenant", scenario.mode)
				h1CapacityWaitQueue(t, &g, 8, 2)
				current()
				current = h1OrgGrant(t, next)
			}
			normal50 := h1OrgAcquireAsync(t, &g, context.Background(), nil, 1, inboundContinuationFreshPriority, maximum, "tenant", scenario.mode)
			apply := h1OrgAcquireAsync(t, &g, context.Background(), nil, 3, inboundContinuationApplyPriority, maximum, "tenant", other)
			h1CapacityWaitQueue(t, &g, 8, 3)
			for _, release := range generic {
				release()
			}
			// Spare credits cannot admit normal work while the incompatible
			// accepted result waits for current ownership to drain.
			h1CapacityWaitQueue(t, &g, 1, 3)
			h1OrgBlocked(t, normal50)
			h1OrgBlocked(t, oppositeNormal)
			current()
			a := h1OrgGrant(t, apply)
			h1CapacityWaitQueue(t, &g, 3, 2)
			// An apply-only override must not admit normal work of its own mode,
			// even though there are five spare global credits.
			h1OrgBlocked(t, oppositeNormal)
			h1OrgBlocked(t, normal50)
			a()
			last := h1OrgGrant(t, normal50)
			h1OrgBlocked(t, oppositeNormal)
			normal51 := h1OrgAcquireAsync(t, &g, context.Background(), nil, 1, inboundContinuationFreshPriority, maximum, "tenant", scenario.mode)
			h1CapacityWaitQueue(t, &g, 1, 2)
			last()
			o := h1OrgGrant(t, oppositeNormal)
			h1CapacityWaitQueue(t, &g, 1, 1)
			h1OrgBlocked(t, normal51)
			o()
			n := h1OrgGrant(t, normal51)
			n()
			h1OrgEmpty(t, &g)
		})
	}
}

func TestH1OrgPhase_ResizeAndUnlimitedPreserveOwnershipAndActualWeight(t *testing.T) {
	var g inboundContinuationAdmission
	var limit atomic.Int32
	limit.Store(3)
	maximum := func() int { return int(limit.Load()) }
	apply := h1OrgHold(t, &g, 3, inboundContinuationApplyPriority, maximum, "tenant-a", inboundContinuationWriter)
	limit.Store(10)
	b := h1OrgHold(t, &g, 3, inboundContinuationApplyPriority, maximum, "tenant-b", inboundContinuationWriter)
	c := h1OrgHold(t, &g, 1, inboundContinuationPerformPriority, maximum, "tenant-c", inboundContinuationReader)
	d := h1OrgHold(t, &g, 1, inboundContinuationPerformPriority, maximum, "tenant-d", inboundContinuationReader)
	h1CapacityWaitQueue(t, &g, 8, 0)
	waiting := h1OrgAcquireAsync(t, &g, context.Background(), nil, 1, inboundContinuationFreshPriority, maximum, "tenant-e", inboundContinuationWriter)
	h1CapacityWaitQueue(t, &g, 8, 1)
	limit.Store(5)
	apply()
	c()
	d()
	h1CapacityWaitQueue(t, &g, 3, 1)
	h1OrgBlocked(t, waiting)
	b()
	e := h1OrgGrant(t, waiting)
	e()
	h1OrgEmpty(t, &g)
	limit.Store(0)
	w := h1OrgHold(t, &g, 3, inboundContinuationApplyPriority, maximum, "tenant-a", inboundContinuationWriter)
	r := h1OrgAcquireAsync(t, &g, context.Background(), nil, 1, inboundContinuationPerformPriority, maximum, "tenant-a", inboundContinuationReader)
	h1CapacityWaitQueue(t, &g, 3, 1)
	other := h1OrgHold(t, &g, 3, inboundContinuationApplyPriority, maximum, "tenant-b", inboundContinuationReader)
	h1CapacityWaitQueue(t, &g, 6, 1)
	h1OrgBlocked(t, r)
	w()
	a := h1OrgGrant(t, r)
	a()
	other()
	h1OrgEmpty(t, &g)
}

func TestH1OrgPhase_TinyPoolAndInvalidScopeFailWithoutOwnership(t *testing.T) {
	for _, pool := range []int{1, 2} {
		t.Run(fmt.Sprintf("pool-%d", pool), func(t *testing.T) {
			var g inboundContinuationAdmission
			maximum := func() int { return pool }
			w := h1OrgHold(t, &g, 1, inboundContinuationFreshPriority, maximum, "tenant", inboundContinuationWriter)
			for _, request := range []struct{ weight, priority int }{{1, inboundContinuationPerformPriority}, {3, inboundContinuationApplyPriority}} {
				got := h1CapacityReceiveAdmission(t, h1OrgAcquireAsync(t, &g, context.Background(), nil, request.weight, request.priority, maximum, "other", inboundContinuationReader))
				require.Error(t, got.err)
				require.Nil(t, got.release)
				h1CapacityWaitQueue(t, &g, 1, 0)
			}
			w()
			h1OrgEmpty(t, &g)
		})
	}
	for _, pool := range []int{3, 4} {
		t.Run(fmt.Sprintf("pool-%d", pool), func(t *testing.T) {
			var g inboundContinuationAdmission
			maximum := func() int { return pool }
			w := h1OrgHold(t, &g, 1, inboundContinuationFreshPriority, maximum, "tenant", inboundContinuationWriter)
			perform := h1OrgAcquireAsync(t, &g, context.Background(), nil, 1, inboundContinuationPerformPriority, maximum, "other", inboundContinuationReader)
			apply := h1OrgAcquireAsync(t, &g, context.Background(), nil, 3, inboundContinuationApplyPriority, maximum, "tenant", inboundContinuationWriter)
			h1CapacityWaitQueue(t, &g, 1, 2)
			w()
			a := h1OrgGrant(t, apply)
			h1CapacityWaitQueue(t, &g, 3, 1)
			h1OrgBlocked(t, perform)
			a()
			r := h1OrgGrant(t, perform)
			r()
			h1OrgEmpty(t, &g)
		})
	}
	for _, scope := range []struct {
		name, organization string
		mode               int
	}{{"missing-reader-organization", "", inboundContinuationReader}, {"missing-writer-organization", "", inboundContinuationWriter}, {"organization-without-mode", "tenant", inboundContinuationUnscoped}, {"unknown-mode", "tenant", 99}} {
		t.Run(scope.name, func(t *testing.T) {
			var g inboundContinuationAdmission
			got := h1CapacityReceiveAdmission(t, h1OrgAcquireAsync(t, &g, context.Background(), nil, 1, inboundContinuationFreshPriority, func() int { return 10 }, scope.organization, scope.mode))
			require.Error(t, got.err)
			require.Nil(t, got.release)
			h1OrgEmpty(t, &g)
		})
	}
}

func TestH1OrgPhase_ApplyClassificationRequiresExactCapturedLegacyText(t *testing.T) {
	newInput := func() (*preparedIncomingContinuation, *capturedLegacyAI) {
		settings := &models.ChatbotSettings{}
		settings.AI.Enabled, settings.AI.Provider, settings.AI.APIKey = true, "synthetic-provider", "synthetic-key"
		return &preparedIncomingContinuation{legacyAI: true, settings: settings}, &capturedLegacyAI{result: models.JSONB{"outcome": "generated", "answer": "synthetic answer"}}
	}
	for _, scenario := range []struct {
		name   string
		change func(**preparedIncomingContinuation, **capturedLegacyAI)
		want   int
	}{
		{"generated-text", func(_ **preparedIncomingContinuation, _ **capturedLegacyAI) {}, inboundContinuationReader},
		{"whitespace-is-nonempty-text", func(_ **preparedIncomingContinuation, c **capturedLegacyAI) { (*c).result["answer"] = " \n\t" }, inboundContinuationReader},
		{"no-preparation", func(p **preparedIncomingContinuation, _ **capturedLegacyAI) { *p = nil }, inboundContinuationWriter},
		{"graph-or-native-route", func(p **preparedIncomingContinuation, _ **capturedLegacyAI) { (*p).legacyAI = false }, inboundContinuationWriter},
		{"no-settings", func(p **preparedIncomingContinuation, _ **capturedLegacyAI) { (*p).settings = nil }, inboundContinuationWriter},
		{"ai-disabled", func(p **preparedIncomingContinuation, _ **capturedLegacyAI) { (*p).settings.AI.Enabled = false }, inboundContinuationWriter},
		{"no-provider", func(p **preparedIncomingContinuation, _ **capturedLegacyAI) { (*p).settings.AI.Provider = "" }, inboundContinuationWriter},
		{"no-key", func(p **preparedIncomingContinuation, _ **capturedLegacyAI) { (*p).settings.AI.APIKey = "" }, inboundContinuationWriter},
		{"no-capture", func(_ **preparedIncomingContinuation, c **capturedLegacyAI) { *c = nil }, inboundContinuationWriter},
		{"capture-error-even-with-answer", func(_ **preparedIncomingContinuation, c **capturedLegacyAI) {
			(*c).err = errors.New("synthetic capture failure")
		}, inboundContinuationWriter},
		{"no-result", func(_ **preparedIncomingContinuation, c **capturedLegacyAI) { (*c).result = nil }, inboundContinuationWriter},
		{"fallback-error", func(_ **preparedIncomingContinuation, c **capturedLegacyAI) { (*c).result["outcome"] = "error" }, inboundContinuationWriter},
		{"fallback-empty", func(_ **preparedIncomingContinuation, c **capturedLegacyAI) { (*c).result["outcome"] = "empty" }, inboundContinuationWriter},
		{"unknown-outcome", func(_ **preparedIncomingContinuation, c **capturedLegacyAI) { (*c).result["outcome"] = "other" }, inboundContinuationWriter},
		{"non-string-outcome", func(_ **preparedIncomingContinuation, c **capturedLegacyAI) { (*c).result["outcome"] = true }, inboundContinuationWriter},
		{"missing-outcome", func(_ **preparedIncomingContinuation, c **capturedLegacyAI) { delete((*c).result, "outcome") }, inboundContinuationWriter},
		{"empty-answer", func(_ **preparedIncomingContinuation, c **capturedLegacyAI) { (*c).result["answer"] = "" }, inboundContinuationWriter},
		{"non-string-answer", func(_ **preparedIncomingContinuation, c **capturedLegacyAI) { (*c).result["answer"] = 123 }, inboundContinuationWriter},
		{"missing-answer", func(_ **preparedIncomingContinuation, c **capturedLegacyAI) { delete((*c).result, "answer") }, inboundContinuationWriter},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			prepared, captured := newInput()
			scenario.change(&prepared, &captured)
			require.Equal(t, scenario.want, preparedIncomingApplyMode(prepared, captured))
		})
	}
}
