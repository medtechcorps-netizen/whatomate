package metarelay

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/stretchr/testify/require"
)

type fakeQueueStore struct {
	mu sync.Mutex

	jobs         []InboundJob
	requeueCalls int
	claimCalls   int
	claimLimit   int
	claimLease   time.Duration
	completed    []string
	retried      []string
	parked       []string
	dead         []string
	deadReasons  []string
}

func (s *fakeQueueStore) ClaimInbound(
	_ context.Context,
	_ time.Time,
	lease time.Duration,
	limit int,
) ([]InboundJob, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.claimCalls++
	s.claimLimit = limit
	s.claimLease = lease
	if limit > len(s.jobs) {
		limit = len(s.jobs)
	}
	claimed := append([]InboundJob(nil), s.jobs[:limit]...)
	s.jobs = append([]InboundJob(nil), s.jobs[limit:]...)
	return claimed, nil
}

func (s *fakeQueueStore) RequeueExpired(context.Context, time.Time, int) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requeueCalls++
	return 0, nil
}

func (s *fakeQueueStore) CompleteInbound(_ context.Context, jobID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.completed = append(s.completed, jobID)
	return nil
}

func (s *fakeQueueStore) RetryInbound(
	_ context.Context,
	jobID string,
	_ time.Time,
	_ int,
	_ string,
) (int, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.retried = append(s.retried, jobID)
	return 1, false, nil
}

func (s *fakeQueueStore) ParkInbound(_ context.Context, jobID string, _ time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.parked = append(s.parked, jobID)
	return nil
}

func (s *fakeQueueStore) DeadInbound(
	_ context.Context,
	jobID, reason string,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dead = append(s.dead, jobID)
	s.deadReasons = append(s.deadReasons, reason)
	return nil
}

type fixedRegistryResolver struct {
	account *AccountConfig
	err     error
}

func (resolver fixedRegistryResolver) Resolve(context.Context, models.Channel, string, string, bool) (*AccountConfig, error) {
	if resolver.err != nil {
		return nil, resolver.err
	}
	copy := *resolver.account
	return &copy, nil
}

func TestWorkerParksStaleRegistryBindingWithoutConsumingRetryBudget(t *testing.T) {
	config := newTestConfig(t)
	store := &fakeQueueStore{jobs: []InboundJob{{
		ID: "stale-job", AccountKey: "registry.account", Channel: models.ChannelMessenger,
		ExternalAccountID: "page-stale", RegistryFence: "oauth:1:webhook:1",
		Body: []byte(`{"external_account_id":"page-stale","events":[]}`),
	}}}
	worker, err := NewWorker(config, store)
	require.NoError(t, err)
	worker.registry = fixedRegistryResolver{err: ErrRegistryStale}
	require.NoError(t, worker.ProcessOnce(context.Background()))
	require.Equal(t, []string{"stale-job"}, store.parked)
	require.Empty(t, store.retried)
	require.Empty(t, store.dead)
}

func TestWorkerParksUnknownFutureQueueVersion(t *testing.T) {
	config := newTestConfig(t)
	store := &fakeQueueStore{jobs: []InboundJob{{
		SchemaVersion: maxInboundJobSchemaVersion + 1,
		ID:            "future-job", AccountKey: "registry.future", Body: []byte(`{}`),
	}}}
	worker, err := NewWorker(config, store)
	require.NoError(t, err)
	require.NoError(t, worker.ProcessOnce(context.Background()))
	require.Equal(t, []string{"future-job"}, store.parked)
	require.Empty(t, store.dead)
}

func TestWorkerNeverFallsBackToStaticAccountWhenDynamicVersionFenceChanges(t *testing.T) {
	config := newTestConfig(t)
	static, _ := config.accountByKey("messenger-page")
	dynamic := *static
	dynamic.registryManaged = true
	dynamic.registryCredentialID = "oauth-id"
	dynamic.registryCredentialVersion = 2
	dynamic.registryWebhookID = "webhook-id"
	dynamic.registryWebhookVersion = 3
	// A deliberately colliding key proves RegistryFence prevents the static
	// map from becoming a cross-account fallback.
	dynamic.Key = static.Key
	store := &fakeQueueStore{jobs: []InboundJob{{
		ID: "dynamic-job", AccountKey: static.Key, Channel: models.ChannelMessenger,
		ExternalAccountID: dynamic.ExternalAccountID,
		RegistryFence:     "oauth-id:1:webhook-id:3", Body: []byte(`{"events":[]}`),
	}}}
	worker, err := NewWorker(config, store)
	require.NoError(t, err)
	worker.registry = fixedRegistryResolver{account: &dynamic}
	require.NoError(t, worker.ProcessOnce(context.Background()))
	require.Equal(t, []string{"dynamic-job"}, store.dead)
	require.Equal(t, []string{"registry_binding_version_changed"}, store.deadReasons)
	require.Empty(t, store.completed)
}

func TestWorkerClaimsOnlyImmediateConcurrencyAndStartsJobsTogether(t *testing.T) {
	config := newTestConfig(t)
	config.WorkerConcurrency = 4
	config.ForwardTimeout = 2 * time.Second
	config.ProcessingLease = 10 * time.Second
	account, _ := config.accountByKey("messenger-page")

	started := make(chan struct{}, config.WorkerConcurrency)
	release := make(chan struct{})
	var active atomic.Int32
	var maximum atomic.Int32
	rereply := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		current := active.Add(1)
		defer active.Add(-1)
		for {
			previous := maximum.Load()
			if current <= previous || maximum.CompareAndSwap(previous, current) {
				break
			}
		}
		started <- struct{}{}
		select {
		case <-release:
			w.WriteHeader(http.StatusNoContent)
		case <-request.Context().Done():
		}
	}))
	defer rereply.Close()
	account.ReReplyWebhookURL = rereply.URL

	store := &fakeQueueStore{}
	for index := 0; index < 20; index++ {
		store.jobs = append(store.jobs, InboundJob{
			ID:         string(rune('a' + index)),
			AccountKey: account.Key,
			Body:       []byte(`{"external_account_id":"page-1","events":[]}`),
		})
	}
	worker, err := NewWorker(config, store)
	if err != nil {
		t.Fatalf("new worker: %v", err)
	}
	result := make(chan error, 1)
	go func() {
		result <- worker.ProcessOnce(context.Background())
	}()

	for index := 0; index < config.WorkerConcurrency; index++ {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatalf("only %d jobs started before timeout", index)
		}
	}
	close(release)
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("process once: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("worker did not settle claimed jobs")
	}

	store.mu.Lock()
	defer store.mu.Unlock()
	if store.claimLimit != config.WorkerConcurrency {
		t.Fatalf("claim limit=%d, want %d", store.claimLimit, config.WorkerConcurrency)
	}
	if store.claimLease != config.ProcessingLease {
		t.Fatalf("claim lease=%s, want %s", store.claimLease, config.ProcessingLease)
	}
	if len(store.completed) != config.WorkerConcurrency || len(store.jobs) != 16 {
		t.Fatalf("completed=%d remaining=%d, want 4/16", len(store.completed), len(store.jobs))
	}
	if maximum.Load() != int32(config.WorkerConcurrency) {
		t.Fatalf("maximum concurrency=%d, want %d", maximum.Load(), config.WorkerConcurrency)
	}
	if store.requeueCalls != 1 {
		t.Fatalf("expired lease requeue calls=%d, want 1", store.requeueCalls)
	}
}

func TestWorkerRejectsUnsafeLeaseOrConcurrency(t *testing.T) {
	config := newTestConfig(t)
	config.ProcessingLease = config.ForwardTimeout + queueSettlementTimeout
	if _, err := NewWorker(config, &fakeQueueStore{}); err == nil {
		t.Fatal("unsafe processing lease was accepted")
	}

	config = newTestConfig(t)
	config.RegistryURL = "https://registry.example.test/internal/meta-registry/v1/resolve"
	config.RegistrySecret = registryClientTestSecret
	config.RegistryTimeout = 10 * time.Second
	config.RegistryCacheTTL = time.Second
	config.RegistryEnabled = true
	config.ProcessingLease = config.ForwardTimeout + queueSettlementTimeout +
		leaseSafetyMargin + config.RegistryTimeout - time.Millisecond
	if _, err := NewWorker(config, &fakeQueueStore{}); err == nil {
		t.Fatal("processing lease that omits part of registry lookup was accepted")
	}

	config = newTestConfig(t)
	config.WorkerConcurrency = 0
	if _, err := NewWorker(config, &fakeQueueStore{}); err == nil {
		t.Fatal("zero worker concurrency was accepted")
	}
}

func TestWorkerRetriesTransientReReplyFailureAndDeadLettersPermanentFailure(t *testing.T) {
	for _, testCase := range []struct {
		name      string
		status    int
		wantRetry int
		wantDead  int
	}{
		{name: "transient", status: http.StatusServiceUnavailable, wantRetry: 1},
		{name: "permanent", status: http.StatusBadRequest, wantDead: 1},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			config := newTestConfig(t)
			account, _ := config.accountByKey("messenger-page")
			rereply := httptest.NewServer(http.HandlerFunc(
				func(w http.ResponseWriter, _ *http.Request) {
					w.WriteHeader(testCase.status)
				},
			))
			defer rereply.Close()
			account.ReReplyWebhookURL = rereply.URL
			store := &fakeQueueStore{jobs: []InboundJob{{
				ID:         "job-1",
				AccountKey: account.Key,
				Body:       []byte(`{"external_account_id":"page-1","events":[]}`),
			}}}
			worker, err := NewWorker(config, store)
			if err != nil {
				t.Fatalf("new worker: %v", err)
			}
			if err := worker.ProcessOnce(context.Background()); err != nil {
				t.Fatalf("process once: %v", err)
			}
			if len(store.retried) != testCase.wantRetry || len(store.dead) != testCase.wantDead {
				t.Fatalf(
					"retried=%d dead=%d, want %d/%d",
					len(store.retried),
					len(store.dead),
					testCase.wantRetry,
					testCase.wantDead,
				)
			}
		})
	}
}
