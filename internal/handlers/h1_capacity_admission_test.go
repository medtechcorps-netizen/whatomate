package handlers

// Candidate-only source proposal. No database connection is borrowed to make a
// queued caller progress; real preclaim checks use the production app root.
import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type h1CapacityAdmissionResult struct {
	release func()
	err     error
}

func h1CapacityQueueState(g *inboundContinuationAdmission) (int, int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.used, len(g.waiters)
}

func h1CapacityWaitQueue(t *testing.T, g *inboundContinuationAdmission, used, waiters int) {
	t.Helper()
	require.Eventually(t, func() bool { u, w := h1CapacityQueueState(g); return u == used && w == waiters }, 2*time.Second, time.Millisecond)
}

func h1CapacityAcquireAsync(t *testing.T, g *inboundContinuationAdmission, ctx context.Context, stop <-chan struct{}, weight, priority int, maximum func() int) <-chan h1CapacityAdmissionResult {
	t.Helper()
	ctx, cancel := context.WithCancel(ctx)
	joined := make(chan struct{})
	result := make(chan h1CapacityAdmissionResult, 1)
	go func() {
		defer close(joined)
		release, err := g.acquire(ctx, stop, weight, priority, maximum)
		result <- h1CapacityAdmissionResult{release, err}
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-joined:
		case <-time.After(5 * time.Second):
			t.Error("admission waiter did not join")
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

func h1CapacityReceiveAdmission(t *testing.T, result <-chan h1CapacityAdmissionResult) h1CapacityAdmissionResult {
	t.Helper()
	select {
	case got := <-result:
		return got
	case <-time.After(3 * time.Second):
		t.Fatal("admission waiter did not settle")
		return h1CapacityAdmissionResult{}
	}
}

func TestH1CapacityAdmission_CompletionAccumulatesCapacityAheadOfFreshWork(t *testing.T) {
	var g inboundContinuationAdmission
	maximum := func() int { return 10 }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	releases := make([]func(), 8)
	for i := range releases {
		release, err := g.acquire(ctx, nil, 1, inboundContinuationFreshPriority, maximum)
		require.NoError(t, err)
		releases[i] = release
		t.Cleanup(release)
	}
	fresh := h1CapacityAcquireAsync(t, &g, ctx, nil, 1, inboundContinuationFreshPriority, maximum)
	h1CapacityWaitQueue(t, &g, 8, 1)
	perform := h1CapacityAcquireAsync(t, &g, ctx, nil, 1, inboundContinuationPerformPriority, maximum)
	h1CapacityWaitQueue(t, &g, 8, 2)
	apply := h1CapacityAcquireAsync(t, &g, ctx, nil, 3, inboundContinuationApplyPriority, maximum)
	h1CapacityWaitQueue(t, &g, 8, 3)
	releases[0]()
	h1CapacityWaitQueue(t, &g, 7, 3)
	releases[1]()
	h1CapacityWaitQueue(t, &g, 6, 3)
	releases[2]()
	gotApply := h1CapacityReceiveAdmission(t, apply)
	require.NoError(t, gotApply.err)
	t.Cleanup(gotApply.release)
	h1CapacityWaitQueue(t, &g, 8, 2)
	select {
	case <-fresh:
		t.Fatal("fresh claim took the capacity reserved for pending apply")
	default:
	}
	select {
	case <-perform:
		t.Fatal("new model work took the capacity reserved for pending apply")
	default:
	}
	gotApply.release()
	gotPerform := h1CapacityReceiveAdmission(t, perform)
	require.NoError(t, gotPerform.err)
	t.Cleanup(gotPerform.release)
	gotFresh := h1CapacityReceiveAdmission(t, fresh)
	require.NoError(t, gotFresh.err)
	t.Cleanup(gotFresh.release)
	gotPerform.release()
	gotFresh.release()
	for _, release := range releases {
		release()
		release()
	}
	h1CapacityWaitQueue(t, &g, 0, 0)
}

func TestH1CapacityAdmission_FIFOCancellationStopAndResize(t *testing.T) {
	t.Run("fifo", func(t *testing.T) {
		var g inboundContinuationAdmission
		maximum := func() int { return 5 }
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		hold, err := g.acquire(ctx, nil, 3, inboundContinuationFreshPriority, maximum)
		require.NoError(t, err)
		t.Cleanup(hold)
		first := h1CapacityAcquireAsync(t, &g, ctx, nil, 3, inboundContinuationFreshPriority, maximum)
		h1CapacityWaitQueue(t, &g, 3, 1)
		second := h1CapacityAcquireAsync(t, &g, ctx, nil, 3, inboundContinuationFreshPriority, maximum)
		h1CapacityWaitQueue(t, &g, 3, 2)
		hold()
		one := h1CapacityReceiveAdmission(t, first)
		require.NoError(t, one.err)
		t.Cleanup(one.release)
		h1CapacityWaitQueue(t, &g, 3, 1)
		select {
		case <-second:
			t.Fatal("same-priority waiter bypassed FIFO")
		default:
		}
		one.release()
		two := h1CapacityReceiveAdmission(t, second)
		require.NoError(t, two.err)
		two.release()
		h1CapacityWaitQueue(t, &g, 0, 0)
	})
	t.Run("cancel-and-stop", func(t *testing.T) {
		var g inboundContinuationAdmission
		maximum := func() int { return 5 }
		hold, err := g.acquire(context.Background(), nil, 3, inboundContinuationFreshPriority, maximum)
		require.NoError(t, err)
		t.Cleanup(hold)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		stop := make(chan struct{})
		canceled := h1CapacityAcquireAsync(t, &g, ctx, nil, 1, inboundContinuationFreshPriority, maximum)
		stopped := h1CapacityAcquireAsync(t, &g, context.Background(), stop, 1, inboundContinuationFreshPriority, maximum)
		h1CapacityWaitQueue(t, &g, 3, 2)
		cancel()
		close(stop)
		one := h1CapacityReceiveAdmission(t, canceled)
		require.ErrorIs(t, one.err, context.Canceled)
		require.Nil(t, one.release)
		two := h1CapacityReceiveAdmission(t, stopped)
		require.ErrorIs(t, two.err, errInboundContinuationAdmissionStopped)
		require.Nil(t, two.release)
		h1CapacityWaitQueue(t, &g, 3, 0)
		hold()
		hold()
		h1CapacityWaitQueue(t, &g, 0, 0)
	})
	t.Run("shrink-does-not-revoke-but-prevents-new-overcommit", func(t *testing.T) {
		var g inboundContinuationAdmission
		var limit atomic.Int32
		limit.Store(10)
		maximum := func() int { return int(limit.Load()) }
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		one, err := g.acquire(ctx, nil, 3, inboundContinuationFreshPriority, maximum)
		require.NoError(t, err)
		t.Cleanup(one)
		two, err := g.acquire(ctx, nil, 3, inboundContinuationFreshPriority, maximum)
		require.NoError(t, err)
		t.Cleanup(two)
		waiting := h1CapacityAcquireAsync(t, &g, ctx, nil, 3, inboundContinuationApplyPriority, maximum)
		h1CapacityWaitQueue(t, &g, 6, 1)
		limit.Store(5)
		one()
		h1CapacityWaitQueue(t, &g, 3, 1)
		two()
		got := h1CapacityReceiveAdmission(t, waiting)
		require.NoError(t, got.err)
		got.release()
		h1CapacityWaitQueue(t, &g, 0, 0)
	})
}

func TestH1CapacityAdmission_QueuedEntryDoesNotClaimOrOwnSQL(t *testing.T) {
	f := h1CapacityLegacy(t, 2)
	// Reserve the selected normal budget with actors holding no SQL connection.
	// This establishes the queue before observing durable state; it does not
	// depend on admitting ten callbacks or a negative sleep-based assertion.
	var releases []func()
	for _, weight := range []int{3, 3, 1, 1} {
		release, err := f.app.acquireInboundContinuationCapacity(context.Background(), nil, weight)
		require.NoError(t, err)
		releases = append(releases, release)
		t.Cleanup(release)
	}
	before0 := loadInboundContinuationJob(t, &App{DB: f.shared}, f.works[0].OrganizationID, f.works[0].Persisted.ID)
	before1 := loadInboundContinuationJob(t, &App{DB: f.shared}, f.works[1].OrganizationID, f.works[1].Persisted.ID)
	ctx0, cancel0 := context.WithCancel(context.Background())
	defer cancel0()
	ctxRecovery, cancelRecovery := context.WithCancel(context.Background())
	defer cancelRecovery()
	fast := NewInboundContinuationProcessor(f.app, time.Second)
	// A nontransactional scoped App must use the same root-owned admission.
	scoped := f.app.scopedApp(f.app.DB, f.works[1].OrganizationID)
	scopedFast := NewInboundContinuationProcessor(scoped, time.Second)
	recovery := NewInboundContinuationProcessor(f.app, time.Second)
	fastDone, scopedDone, recoveryDone := make(chan error, 1), make(chan error, 1), make(chan error, 1)
	var callers sync.WaitGroup
	t.Cleanup(func() {
		cancel0()
		cancelRecovery()
		scopedFast.Stop()
		for _, release := range releases {
			release()
		}
		joined := make(chan struct{})
		go func() { callers.Wait(); close(joined) }()
		select {
		case <-joined:
		case <-time.After(10 * time.Second):
			t.Error("queued processors did not join")
		}
	})
	callers.Add(1)
	go func() {
		defer callers.Done()
		fastDone <- fast.ProcessMessage(ctx0, f.works[0].OrganizationID, f.works[0].Persisted.ID)
	}()
	h1CapacityWaitQueue(t, &f.app.continuationAdmission, 8, 1)
	callers.Add(1)
	go func() {
		defer callers.Done()
		scopedDone <- scopedFast.ProcessMessage(context.Background(), f.works[1].OrganizationID, f.works[1].Persisted.ID)
	}()
	h1CapacityWaitQueue(t, &f.app.continuationAdmission, 8, 2)
	callers.Add(1)
	go func() { defer callers.Done(); recoveryDone <- recovery.RunOnce(ctxRecovery) }()
	require.Eventually(t, func() bool {
		used, waiting := h1CapacityQueueState(&f.app.continuationAdmission)
		return used == 8 && waiting >= 3
	}, 2*time.Second, time.Millisecond)
	require.Equal(t, before0, loadInboundContinuationJob(t, &App{DB: f.shared}, f.works[0].OrganizationID, f.works[0].Persisted.ID))
	require.Equal(t, before1, loadInboundContinuationJob(t, &App{DB: f.shared}, f.works[1].OrganizationID, f.works[1].Persisted.ID))
	require.Zero(t, f.pool.Stats().InUse, "queued admission owns no application SQL connection")
	require.Zero(t, f.model.calls.Load())
	cancel0()
	cancelRecovery()
	scopedFast.Stop()
	for index, done := range []<-chan error{fastDone, scopedDone, recoveryDone} {
		select {
		case err := <-done:
			if index == 1 {
				require.True(t, errors.Is(err, errInboundContinuationAdmissionStopped), "stopped queued caller must return the admission stop reason")
			} else {
				require.ErrorIs(t, err, context.Canceled)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("queued caller did not honor cancellation/Stop")
		}
	}
	h1CapacityWaitQueue(t, &f.app.continuationAdmission, 8, 0)
	require.Equal(t, before0, loadInboundContinuationJob(t, &App{DB: f.shared}, f.works[0].OrganizationID, f.works[0].Persisted.ID))
	require.Equal(t, before1, loadInboundContinuationJob(t, &App{DB: f.shared}, f.works[1].OrganizationID, f.works[1].Persisted.ID))
	for _, release := range releases {
		release()
	}
	f.model.unblock()
	h1CapacityJoin(t, f.start(t, 0, 1), 2)
	for i := range f.works {
		f.assertGenerated(t, i)
	}
	h1CapacityWaitQueue(t, &f.app.continuationAdmission, 0, 0)
}

func TestH1CapacityAdmission_SmallPoolSerializesAndResizeKeepsActualWeight(t *testing.T) {
	for _, maximum := range []int{3, 4} {
		t.Run(fmt.Sprintf("pool-%d", maximum), func(t *testing.T) {
			var g inboundContinuationAdmission
			limit := func() int { return maximum }
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			first, err := g.acquire(ctx, nil, 1, inboundContinuationFreshPriority, limit)
			require.NoError(t, err)
			t.Cleanup(first)
			second := h1CapacityAcquireAsync(t, &g, ctx, nil, 1, inboundContinuationFreshPriority, limit)
			require.Eventually(t, func() bool { _, waiting := h1CapacityQueueState(&g); return waiting == 1 }, 2*time.Second, time.Millisecond)
			select {
			case <-second:
				t.Fatal("a small pool admitted concurrent model guards instead of serializing phases")
			default:
			}
			first()
			got := h1CapacityReceiveAdmission(t, second)
			require.NoError(t, got.err)
			got.release()
			apply, err := g.acquire(ctx, nil, 3, inboundContinuationApplyPriority, limit)
			require.NoError(t, err)
			apply()
			h1CapacityWaitQueue(t, &g, 0, 0)
		})
	}
	t.Run("growth-preserves-outstanding-three-connection-demand", func(t *testing.T) {
		var g inboundContinuationAdmission
		var size atomic.Int32
		size.Store(3)
		limit := func() int { return int(size.Load()) }
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		first, err := g.acquire(ctx, nil, 3, inboundContinuationApplyPriority, limit)
		require.NoError(t, err)
		t.Cleanup(first)
		size.Store(10)
		second, err := g.acquire(ctx, nil, 3, inboundContinuationApplyPriority, limit)
		require.NoError(t, err)
		t.Cleanup(second)
		third := h1CapacityAcquireAsync(t, &g, ctx, nil, 3, inboundContinuationApplyPriority, limit)
		// Two outstanding weight3 phases consume six of the eight normal units.
		// Repricing the first phase as one merely because it started at pool3
		// would incorrectly admit nine units of physical demand after growth.
		h1CapacityWaitQueue(t, &g, 6, 1)
		first()
		got := h1CapacityReceiveAdmission(t, third)
		require.NoError(t, got.err)
		got.release()
		second()
		h1CapacityWaitQueue(t, &g, 0, 0)
	})
	t.Run("unsupported-and-unlimited", func(t *testing.T) {
		for _, size := range []int{-1, 1, 2} {
			var g inboundContinuationAdmission
			release, err := g.acquire(context.Background(), nil, 3, inboundContinuationApplyPriority, func() int { return size })
			require.Error(t, err)
			require.Nil(t, release)
			h1CapacityWaitQueue(t, &g, 0, 0)
		}
		var g inboundContinuationAdmission
		one, err := g.acquire(context.Background(), nil, 3, inboundContinuationApplyPriority, func() int { return 0 })
		require.NoError(t, err)
		two, err := g.acquire(context.Background(), nil, 3, inboundContinuationApplyPriority, func() int { return 0 })
		require.NoError(t, err)
		one()
		two()
		h1CapacityWaitQueue(t, &g, 0, 0)
	})
}
