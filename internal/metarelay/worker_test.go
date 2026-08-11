package metarelay

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	channelapi "github.com/shridarpatil/whatomate/internal/channel"
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
	dead         []string
}

func TestWorkerForwardsAccountAndDeploymentProofsOverExactBody(t *testing.T) {
	config := newTestConfig(t)
	account, _ := config.accountByKey("messenger-page")
	body := []byte(`{"external_account_id":"100000000000010","events":[]}`)
	rereply := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		received, err := io.ReadAll(request.Body)
		if err != nil {
			t.Errorf("read forwarded body: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if string(received) != string(body) {
			t.Errorf("forwarded body changed: %s", received)
		}
		if request.Header.Get(ReReplySignatureHeader) != signBody(account.reReplyInboundSecret, body) {
			t.Error("missing account-scoped inbound HMAC")
		}
		if request.Header.Get(channelapi.RelayMetaProviderProofHeader) !=
			channelapi.SignMetaProviderInboundProof(config.ReReplyProviderProofSecret, body) {
			t.Error("missing deployment-held provider proof")
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer rereply.Close()
	account.ReReplyWebhookURL = rereply.URL
	worker, err := NewWorker(config, &fakeQueueStore{})
	if err != nil {
		t.Fatalf("new worker: %v", err)
	}
	outcome := worker.forwardToReReply(context.Background(), account, body)
	if !outcome.success {
		t.Fatalf("forward failed: %+v", outcome)
	}
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

func (s *fakeQueueStore) DeadInbound(
	_ context.Context,
	jobID, _ string,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dead = append(s.dead, jobID)
	return nil
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
			Body:       []byte(`{"external_account_id":"100000000000010","events":[]}`),
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
				Body:       []byte(`{"external_account_id":"100000000000010","events":[]}`),
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
