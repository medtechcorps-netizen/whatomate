package handlers

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// With no pending apply, later fresh and perform arrivals must not overtake an
// already queued caller of either class. Only one credit is released at a time;
// freeing several credits would allow both classes through and hide priority
// inversion. CompletionAccumulatesCapacityAheadOfFreshWork separately retains
// the strict apply reservation when three credits must accumulate.
func TestH1CapacityAdmission_NonApplyFIFOProgressWithContinuousFreshAndPerformArrivals(t *testing.T) {
	for _, first := range []struct {
		name     string
		priority int
	}{
		{"fresh-first", inboundContinuationFreshPriority},
		{"perform-first", inboundContinuationPerformPriority},
	} {
		t.Run(first.name, func(t *testing.T) {
			var g inboundContinuationAdmission
			maximum := func() int { return 10 }
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			holds := make([]func(), 8)
			for i := range holds {
				release, err := g.acquire(ctx, nil, 1, inboundContinuationFreshPriority, maximum)
				require.NoError(t, err)
				holds[i] = release
				t.Cleanup(release)
			}
			type entry struct {
				priority int
				result   <-chan h1CapacityAdmissionResult
			}
			var queued []entry
			enqueue := func(priority int) {
				queued = append(queued, entry{priority, h1CapacityAcquireAsync(t, &g, ctx, nil, 1, priority, maximum)})
			}
			other := inboundContinuationPerformPriority
			if first.priority == other {
				other = inboundContinuationFreshPriority
			}
			for i := 0; i < 6; i++ {
				priority := first.priority
				if i%2 != 0 {
					priority = other
				}
				enqueue(priority)
				h1CapacityWaitQueue(t, &g, 8, i+1)
			}

			// Seven original holders stay admitted. This one recyclable credit
			// forces every observed grant to identify the actual queue winner.
			holds[0]()
			grants := map[int]int{}
			const producerRounds = 16
			for index := 0; index < producerRounds; index++ {
				got := h1CapacityReceiveAdmission(t, queued[index].result)
				require.NoError(t, got.err)
				t.Cleanup(got.release)
				grants[queued[index].priority]++
				h1CapacityWaitQueue(t, &g, 8, len(queued)-index-1)

				// A replacement producer arrives before releasing this grant.
				// Both classes therefore remain continuously represented in the
				// queue; no test waits for a quiet gap to let the other class run.
				enqueue(queued[index].priority)
				h1CapacityWaitQueue(t, &g, 8, len(queued)-index-1)
				waitingByClass := map[int]int{}
				for _, waiting := range queued[index+1:] {
					waitingByClass[waiting.priority]++
					select {
					case unexpected := <-waiting.result:
						if unexpected.release != nil {
							unexpected.release()
						}
						t.Fatal("a later waiter settled while the only recyclable credit was held")
					default:
					}
				}
				require.Positive(t, waitingByClass[inboundContinuationFreshPriority])
				require.Positive(t, waitingByClass[inboundContinuationPerformPriority])
				got.release()
			}
			require.Equal(t, producerRounds/2, grants[inboundContinuationFreshPriority])
			require.Equal(t, producerRounds/2, grants[inboundContinuationPerformPriority])

			// Stop both producers, drain their remaining requests in the same
			// global FIFO order, and release every permit before test cleanup.
			for _, waiting := range queued[producerRounds:] {
				got := h1CapacityReceiveAdmission(t, waiting.result)
				require.NoError(t, got.err)
				t.Cleanup(got.release)
				got.release()
			}
			h1CapacityWaitQueue(t, &g, 7, 0)
			for _, release := range holds {
				release()
			}
			h1CapacityWaitQueue(t, &g, 0, 0)
		})
	}
}
