package metarelay

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	channelapi "github.com/shridarpatil/whatomate/internal/channel"
	"github.com/shridarpatil/whatomate/internal/metareview"
)

func TestReviewWorkerRevalidatesEveryJobAndEmitsBothProofs(t *testing.T) {
	config := newReviewTestConfig(t)
	now := time.Now().UTC()
	body := []byte(`{"external_account_id":"211573738714332","events":[]}`)
	binding := validReviewBinding(config, now)
	var forwardCalls atomic.Int32
	rereply := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		forwardCalls.Add(1)
		received, err := io.ReadAll(request.Body)
		if err != nil {
			t.Errorf("read body: %v", err)
		}
		if string(received) != string(body) {
			t.Errorf("canonical body changed: %s", received)
		}
		if request.Header.Get(ReReplySignatureHeader) != signBody(binding.InboundSecret, body) {
			t.Error("missing account inbound HMAC")
		}
		if request.Header.Get(channelapi.RelayMetaProviderProofHeader) !=
			channelapi.SignMetaProviderInboundProof(config.ReReplyProviderProofSecret, body) {
			t.Error("missing generic Meta provider proof")
		}
		if request.Header.Get(metareview.GenerationHeader) != binding.Tuple.Generation ||
			request.Header.Get(metareview.CredentialIDHeader) != binding.CredentialID ||
			request.Header.Get(metareview.CredentialVersionHeader) != strconv.Itoa(binding.CredentialVersion) {
			t.Error("missing exact review credential fence headers")
		}
		if err := metareview.VerifyInboundProof(
			config.ReReplyProviderProofSecret,
			binding.Tuple,
			binding.CredentialID,
			binding.CredentialVersion,
			received,
			request.Header.Get(metareview.ReviewProofHeader),
		); err != nil {
			t.Errorf("invalid domain-separated review proof: %v", err)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer rereply.Close()
	config.ReReplyBaseURL = rereply.URL
	config.allowInsecureTestEndpoints = true
	binding.ReReplyWebhookURL = rereply.URL + "/api/webhooks/channels/" + config.ReviewChannelAccountID
	resolver := &fakeReviewResolver{binding: binding}
	store := &fakeQueueStore{}
	for index := 0; index < 2; index++ {
		store.jobs = append(store.jobs, InboundJob{
			ID:                      "review-job-" + strconv.Itoa(index),
			AccountKey:              config.ReviewChannelAccountID,
			Body:                    body,
			ReviewGeneration:        config.ReviewGeneration,
			ReviewCredentialID:      binding.CredentialID,
			ReviewCredentialVersion: binding.CredentialVersion,
		})
	}
	config.WorkerConcurrency = 2
	worker, err := NewWorker(
		config,
		store,
		WithWorkerReviewBindingResolver(resolver),
	)
	if err != nil {
		t.Fatalf("new review worker: %v", err)
	}
	worker.now = func() time.Time { return now }
	if err := worker.ProcessOnce(context.Background()); err != nil {
		t.Fatalf("process review jobs: %v", err)
	}
	if resolver.callCount() != 2 {
		t.Fatalf("binding resolutions=%d, want one per job", resolver.callCount())
	}
	if forwardCalls.Load() != 2 || len(store.completed) != 2 {
		t.Fatalf("forwarded=%d completed=%d, want 2/2", forwardCalls.Load(), len(store.completed))
	}
}

func TestReviewWorkerDeadLettersChangedBindingBeforeForward(t *testing.T) {
	config := newReviewTestConfig(t)
	now := time.Now().UTC()
	binding := validReviewBinding(config, now)
	job := InboundJob{
		ID:                      "review-job-changed",
		AccountKey:              config.ReviewChannelAccountID,
		Body:                    []byte(`{"external_account_id":"211573738714332","events":[]}`),
		ReviewGeneration:        config.ReviewGeneration,
		ReviewCredentialID:      binding.CredentialID,
		ReviewCredentialVersion: binding.CredentialVersion,
	}
	binding.CredentialVersion++
	resolver := &fakeReviewResolver{binding: binding}
	store := &fakeQueueStore{jobs: []InboundJob{job}}
	var forwarded atomic.Int32
	worker, err := NewWorker(
		config,
		store,
		WithWorkerReviewBindingResolver(resolver),
		WithWorkerHTTPClient(&http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			forwarded.Add(1)
			return nil, errTestTransport
		})}),
	)
	if err != nil {
		t.Fatalf("new review worker: %v", err)
	}
	worker.now = func() time.Time { return now }
	if err := worker.ProcessOnce(context.Background()); err != nil {
		t.Fatalf("process changed binding: %v", err)
	}
	if forwarded.Load() != 0 || len(store.dead) != 1 || len(store.retried) != 0 {
		t.Fatalf("forwarded=%d dead=%d retried=%d", forwarded.Load(), len(store.dead), len(store.retried))
	}
}

func TestReviewWorkerRetriesUnavailableBrokerWithoutForward(t *testing.T) {
	config := newReviewTestConfig(t)
	binding := validReviewBinding(config, time.Now().UTC())
	store := &fakeQueueStore{jobs: []InboundJob{{
		ID:                      "review-job-unavailable",
		AccountKey:              config.ReviewChannelAccountID,
		Body:                    []byte(`{"external_account_id":"211573738714332","events":[]}`),
		ReviewGeneration:        config.ReviewGeneration,
		ReviewCredentialID:      binding.CredentialID,
		ReviewCredentialVersion: binding.CredentialVersion,
	}}}
	worker, err := NewWorker(
		config,
		store,
		WithWorkerReviewBindingResolver(&fakeReviewResolver{err: ErrReviewBindingUnavailable}),
	)
	if err != nil {
		t.Fatalf("new review worker: %v", err)
	}
	if err := worker.ProcessOnce(context.Background()); err != nil {
		t.Fatalf("process unavailable broker: %v", err)
	}
	if len(store.retried) != 1 || len(store.dead) != 0 || len(store.completed) != 0 {
		t.Fatalf("retried=%d dead=%d completed=%d", len(store.retried), len(store.dead), len(store.completed))
	}
}
