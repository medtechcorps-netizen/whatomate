package metarelay

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type drainContextKey struct{}

type drainSettlementStore struct {
	*fakeQueueStore
	settlements atomic.Int32
}

func (s *drainSettlementStore) check(ctx context.Context) error {
	if ctx.Err() != nil {
		return errors.New("intake cancellation reached settlement")
	}
	deadline, ok := ctx.Deadline()
	if !ok || time.Until(deadline) <= 0 || time.Until(deadline) > queueSettlementTimeout {
		return errors.New("settlement is not independently bounded")
	}
	if ctx.Value(drainContextKey{}) != "synthetic-scope" {
		return errors.New("settlement lost context values")
	}
	s.settlements.Add(1)
	return nil
}

func (s *drainSettlementStore) CompleteInbound(ctx context.Context, id string) error {
	if err := s.check(ctx); err != nil {
		return err
	}
	return s.fakeQueueStore.CompleteInbound(ctx, id)
}
func (s *drainSettlementStore) DeadInbound(ctx context.Context, id, reason string) error {
	if err := s.check(ctx); err != nil {
		return err
	}
	return s.fakeQueueStore.DeadInbound(ctx, id, reason)
}
func (s *drainSettlementStore) ParkInbound(ctx context.Context, id string, until time.Time) error {
	if err := s.check(ctx); err != nil {
		return err
	}
	return s.fakeQueueStore.ParkInbound(ctx, id, until)
}
func (s *drainSettlementStore) RetryInbound(ctx context.Context, id string, until time.Time, max int, reason string) (int, bool, error) {
	if err := s.check(ctx); err != nil {
		return 0, false, err
	}
	return s.fakeQueueStore.RetryInbound(ctx, id, until, max, reason)
}

type drainRoundTripper func(*http.Request) (*http.Response, error)

func (f drainRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestWorkerObservedForwardOutcomeSettlesAfterIntakeCancellation(t *testing.T) {
	for _, status := range []int{http.StatusAccepted, http.StatusBadRequest, http.StatusServiceUnavailable, 0} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			config := newTestConfig(t)
			account, ok := config.accountByKey("messenger-page")
			require.True(t, ok)
			ctx, cancel := context.WithCancel(context.WithValue(context.Background(), drainContextKey{}, "synthetic-scope"))
			defer cancel()
			store := &drainSettlementStore{fakeQueueStore: &fakeQueueStore{}}
			var forwards atomic.Int32
			client := &http.Client{Transport: drainRoundTripper(func(r *http.Request) (*http.Response, error) {
				forwards.Add(1)
				cancel()
				if status == 0 {
					return nil, context.Canceled
				}
				return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header), Request: r}, nil
			})}
			worker, err := NewWorker(config, store, WithWorkerHTTPClient(client))
			require.NoError(t, err)
			require.NoError(t, worker.processJob(ctx, InboundJob{ID: "claimed", AccountKey: account.Key, Body: []byte(`{"events":[]}`)}))
			require.ErrorIs(t, ctx.Err(), context.Canceled)
			require.EqualValues(t, 1, forwards.Load())
			require.EqualValues(t, 1, store.settlements.Load())
			switch status {
			case http.StatusAccepted:
				require.Equal(t, []string{"claimed"}, store.completed)
				require.Empty(t, store.retried)
				require.Empty(t, store.dead)
			case http.StatusBadRequest:
				require.Equal(t, []string{"claimed"}, store.dead)
				require.Empty(t, store.completed)
				require.Empty(t, store.retried)
			default:
				require.Equal(t, []string{"claimed"}, store.retried)
				require.Empty(t, store.completed)
				require.Empty(t, store.dead)
			}
		})
	}
}

func TestWorkerAlreadyClaimedValidationSettlementSurvivesCancellation(t *testing.T) {
	for _, mode := range []string{"future_version", "unknown_account", "invalid_static_job", "stale_registry", "unavailable_registry"} {
		t.Run(mode, func(t *testing.T) {
			config := newTestConfig(t)
			ctx, cancel := context.WithCancel(context.WithValue(context.Background(), drainContextKey{}, "synthetic-scope"))
			cancel()
			store := &drainSettlementStore{fakeQueueStore: &fakeQueueStore{}}
			var forwards atomic.Int32
			worker, err := NewWorker(config, store, WithWorkerHTTPClient(&http.Client{Transport: drainRoundTripper(func(*http.Request) (*http.Response, error) {
				forwards.Add(1)
				return nil, errors.New("unexpected forwarding")
			})}))
			require.NoError(t, err)
			job := InboundJob{ID: "already-claimed", AccountKey: "absent"}
			switch mode {
			case "future_version":
				job.SchemaVersion = maxInboundJobSchemaVersion + 1
			case "invalid_static_job":
				job.SchemaVersion = staticMessengerInboundJobSchemaVersion
			case "stale_registry", "unavailable_registry":
				account, ok := config.accountByKey("messenger-page")
				require.True(t, ok)
				job.Channel = account.Channel
				job.ExternalAccountID = account.ExternalAccountID
				job.RegistryFence = "synthetic-fence"
				registryErr := ErrRegistryStale
				if mode == "unavailable_registry" {
					registryErr = ErrRegistryUnavailable
				}
				worker.registry = fixedRegistryResolver{err: registryErr}
			}
			require.NoError(t, worker.processJob(ctx, job))
			require.EqualValues(t, 0, forwards.Load())
			require.EqualValues(t, 1, store.settlements.Load())
			switch mode {
			case "future_version", "stale_registry":
				require.Equal(t, []string{job.ID}, store.parked)
			case "unavailable_registry":
				require.Equal(t, []string{job.ID}, store.retried)
			default:
				require.Equal(t, []string{job.ID}, store.dead)
			}
		})
	}
}
