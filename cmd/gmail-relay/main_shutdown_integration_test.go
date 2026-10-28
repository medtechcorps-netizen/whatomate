package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/shridarpatil/whatomate/internal/gmailrelay"
	"github.com/shridarpatil/whatomate/internal/metarelay"
	"github.com/stretchr/testify/require"
)

func gmailDrainStores(t *testing.T) (*gmailrelay.Config, *gmailrelay.RedisStore, *metarelay.RedisStore) {
	t.Helper()
	url := os.Getenv("TEST_REDIS_URL")
	if url == "" {
		t.Skip("TEST_REDIS_URL not set; real Redis drain qualification required")
	}
	config := &gmailrelay.Config{
		RedisURL: url, RedisPrefix: "test:gmail-drain:" + uuid.NewString() + ":",
		Mailbox: "relay-drain@example.test", EncryptionKey: strings.Repeat("k", 32),
		ReReplyWebhookURL: "http://127.0.0.1/unused", ReReplyInboundSecret: strings.Repeat("i", 32),
		ReReplyOutboundSecret: strings.Repeat("o", 32),
		HTTPTimeout:           time.Second, GmailPollInterval: time.Hour, QueuePollInterval: time.Hour,
		ForwardTimeout: 2 * time.Second, ProcessingLease: time.Minute,
		InboundRetention: time.Hour, OutboundRetention: time.Hour, WorkerConcurrency: 1, MaxAttempts: 3,
	}
	state, err := gmailrelay.NewRedisStore(config)
	require.NoError(t, err)
	queue, err := metarelay.NewRedisStore(&metarelay.Config{
		RedisURL: url, RedisPrefix: config.RedisPrefix + "queue:",
		InboundRetention: time.Hour, OutboundRetention: time.Hour,
	})
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, state.Ping(ctx))
	require.NoError(t, queue.Ping(ctx))
	t.Cleanup(func() {
		_ = state.Close()
		_ = queue.Close()
		options, err := redis.ParseURL(url)
		if err != nil {
			t.Error(err)
			return
		}
		client := redis.NewClient(options)
		defer func() { _ = client.Close() }()
		cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		var cursor uint64
		for {
			keys, next, err := client.Scan(cleanup, cursor, config.RedisPrefix+"*", 100).Result()
			if err != nil {
				t.Error(err)
				return
			}
			if len(keys) != 0 {
				if err := client.Del(cleanup, keys...).Err(); err != nil {
					t.Error(err)
					return
				}
			}
			cursor = next
			if cursor == 0 {
				return
			}
		}
	})
	return config, state, queue
}

type settlingGmailDrainQueue struct {
	*metarelay.RedisStore
	entered chan context.Context
	release <-chan struct{}
	settled atomic.Bool
}

func (queue *settlingGmailDrainQueue) CompleteInbound(ctx context.Context, id string) error {
	queue.entered <- ctx
	select {
	case <-queue.release:
	case <-ctx.Done():
		return ctx.Err()
	}
	if err := queue.RedisStore.CompleteInbound(ctx, id); err != nil {
		return err
	}
	queue.settled.Store(true)
	return nil
}

func TestGmailRelayAcceptedForwardSettlesBeforeClose(t *testing.T) {
	config, state, queue := gmailDrainStores(t)
	var providerCalls atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		providerCalls.Add(1)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer provider.Close()
	config.ReReplyWebhookURL = provider.URL
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	job := metarelay.InboundJob{ID: "accepted-forward", AccountKey: config.Mailbox, Body: []byte("{\"synthetic\":true}")}
	accepted, err := queue.AcceptInbound(ctx, job.ID, []metarelay.InboundJob{job})
	require.NoError(t, err)
	require.True(t, accepted)
	gate, release := relayGate(t)
	settling := &settlingGmailDrainQueue{RedisStore: queue, entered: make(chan context.Context, 1), release: gate}
	forwarder, err := gmailrelay.NewForwardWorker(config, settling)
	require.NoError(t, err)
	root, cancelRoot := context.WithCancel(context.Background())
	defer cancelRoot()
	httpStop, intakeStopped := make(chan struct{}), make(chan struct{})
	var closeCount atomic.Int32
	done := make(chan gmailRelayOutcome, 1)
	go func() {
		done <- runGmailRelay(root, 5*time.Second, gmailRelayRuntime{
			sync:     func(ctx context.Context) error { <-ctx.Done(); close(intakeStopped); return ctx.Err() },
			forward:  forwarder.Run,
			serve:    func() error { <-httpStop; return http.ErrServerClosed },
			shutdown: func(context.Context) error { close(httpStop); return nil },
			stop:     func() {},
			closeState: func() error {
				if !settling.settled.Load() {
					return errors.New("state closed before durable settlement")
				}
				closeCount.Add(1)
				return state.Close()
			},
			closeQueue: func() error {
				if !settling.settled.Load() {
					return errors.New("queue closed before durable settlement")
				}
				closeCount.Add(1)
				return queue.Close()
			},
			report: func(gmailRelayOutcome) {},
		})
	}()
	var settlement context.Context
	select {
	case settlement = <-settling.entered:
	case <-ctx.Done():
		t.Fatal("forwarder did not reach accepted-result settlement")
	}
	require.EqualValues(t, 1, providerCalls.Load())
	cancelRoot()
	waitRelaySignal(t, intakeStopped)
	require.NoError(t, settlement.Err(), "intake cancellation must not cancel accepted settlement")
	deadline, bounded := settlement.Deadline()
	require.True(t, bounded)
	require.True(t, time.Until(deadline) > 0 && time.Until(deadline) <= 5*time.Second)
	require.Zero(t, closeCount.Load())
	require.NoError(t, state.Ping(ctx))
	require.NoError(t, queue.Ping(ctx))
	release()
	result := receiveRelayOutcome(t, done)
	require.True(t, result.complete)
	require.True(t, result.reportJoined)
	require.False(t, result.failed)
	require.EqualValues(t, 2, closeCount.Load())
	// A fresh production worker and Redis connection must not resend the
	// provider-accepted job after the owned settlement and close completed.
	reopened, err := metarelay.NewRedisStore(&metarelay.Config{
		RedisURL: config.RedisURL, RedisPrefix: config.RedisPrefix + "queue:",
		InboundRetention: time.Hour, OutboundRetention: time.Hour,
	})
	require.NoError(t, err)
	defer func() { _ = reopened.Close() }()
	fresh, err := gmailrelay.NewForwardWorker(config, reopened)
	require.NoError(t, err)
	require.NoError(t, fresh.ProcessOnce(ctx))
	require.EqualValues(t, 1, providerCalls.Load())
}

type interruptedGmailDrainState struct {
	*gmailrelay.RedisStore
	commitEntered  chan struct{}
	releaseEntered chan context.Context
	release        <-chan struct{}
	released       atomic.Bool
}

func (state *interruptedGmailDrainState) CommitSuccessfulSync(ctx context.Context, _, _ string, _ time.Time) error {
	close(state.commitEntered)
	<-ctx.Done()
	return ctx.Err()
}

func (state *interruptedGmailDrainState) ReleaseSyncLease(ctx context.Context, token string) error {
	state.releaseEntered <- ctx
	select {
	case <-state.release:
	case <-ctx.Done():
		return ctx.Err()
	}
	if err := state.RedisStore.ReleaseSyncLease(ctx, token); err != nil {
		return err
	}
	state.released.Store(true)
	return nil
}

type gmailDrainToken struct{}

func (gmailDrainToken) AccessToken(context.Context) (string, error) {
	return "synthetic-access-token", nil
}

func TestGmailRelayInterruptedSyncRetainsRecoverableCursor(t *testing.T) {
	config, state, queue := gmailDrainStores(t)
	var historyCalls atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Header.Get("Authorization") != "Bearer synthetic-access-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		var payload any
		switch r.URL.Path {
		case "/users/me/history":
			historyCalls.Add(1)
			if r.URL.Query().Get("startHistoryId") != "100" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			payload = map[string]any{
				"historyId": "101",
				"history":   []any{map[string]any{"id": "101", "messagesAdded": []any{map[string]any{"message": map[string]string{"id": "drain-message", "threadId": "drain-thread"}}}}},
			}
		case "/users/me/messages/drain-message":
			payload = gmailrelay.GmailRESTMessage{
				ID: "drain-message", ThreadID: "drain-thread", LabelIDs: []string{"INBOX"},
				InternalDate: "1793100000000",
				Payload: &gmailrelay.GmailRESTPart{
					MimeType: "text/plain",
					Headers:  []gmailrelay.GmailRESTHeader{{Name: "From", Value: "sender@example.test"}, {Name: "To", Value: config.Mailbox}},
					Body:     gmailrelay.GmailRESTBody{Size: 5, Data: base64.RawURLEncoding.EncodeToString([]byte("hello"))},
				},
			}
		default:
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if err := json.NewEncoder(w).Encode(payload); err != nil {
			t.Errorf("synthetic response: %v", err)
		}
	}))
	defer provider.Close()
	config.GmailAPIBaseURL = provider.URL
	gmail, err := gmailrelay.NewGmailClient(config, gmailDrainToken{}, provider.Client())
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	require.NoError(t, state.SetHistoryCursor(ctx, "100"))
	releaseGate, allowRelease := relayGate(t)
	interrupted := &interruptedGmailDrainState{
		RedisStore: state, commitEntered: make(chan struct{}),
		releaseEntered: make(chan context.Context, 1), release: releaseGate,
	}
	syncer, err := gmailrelay.NewSyncer(config, gmail, interrupted, queue)
	require.NoError(t, err)
	root, cancelRoot := context.WithCancel(context.Background())
	defer cancelRoot()
	httpStop := make(chan struct{})
	var closeCount atomic.Int32
	done := make(chan gmailRelayOutcome, 1)
	go func() {
		done <- runGmailRelay(root, 5*time.Second, gmailRelayRuntime{
			sync:     syncer.Run,
			forward:  func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() },
			serve:    func() error { <-httpStop; return http.ErrServerClosed },
			shutdown: func(context.Context) error { close(httpStop); return nil },
			stop:     func() {},
			closeState: func() error {
				if !interrupted.released.Load() {
					return errors.New("state closed before lease release")
				}
				closeCount.Add(1)
				return state.Close()
			},
			closeQueue: func() error {
				if !interrupted.released.Load() {
					return errors.New("queue closed before sync join")
				}
				closeCount.Add(1)
				return queue.Close()
			},
			report: func(gmailRelayOutcome) {},
		})
	}()
	waitRelaySignal(t, interrupted.commitEntered)
	// Queue acceptance is durable before the blocked cursor commit.
	options, err := redis.ParseURL(config.RedisURL)
	require.NoError(t, err)
	observer := redis.NewClient(options)
	defer func() { _ = observer.Close() }()
	require.EqualValues(t, 1, observer.ZCard(ctx, config.RedisPrefix+"queue:inbound:due").Val())
	cancelRoot()
	var releaseContext context.Context
	select {
	case releaseContext = <-interrupted.releaseEntered:
	case <-ctx.Done():
		t.Fatal("syncer did not finish heartbeat and enter lease release")
	}
	require.NoError(t, releaseContext.Err(), "intake cancellation must not cancel lease release")
	require.Zero(t, closeCount.Load())
	cursor, err := state.GetHistoryCursor(ctx)
	require.NoError(t, err)
	require.Equal(t, "100", cursor)
	require.NoError(t, queue.Ping(ctx))
	allowRelease()
	result := receiveRelayOutcome(t, done)
	require.True(t, result.complete)
	require.True(t, result.reportJoined)
	require.False(t, result.failed)
	require.EqualValues(t, 2, closeCount.Load())

	reopenedState, err := gmailrelay.NewRedisStore(config)
	require.NoError(t, err)
	defer func() { _ = reopenedState.Close() }()
	reopenedQueue, err := metarelay.NewRedisStore(&metarelay.Config{
		RedisURL: config.RedisURL, RedisPrefix: config.RedisPrefix + "queue:",
		InboundRetention: time.Hour, OutboundRetention: time.Hour,
	})
	require.NoError(t, err)
	defer func() { _ = reopenedQueue.Close() }()
	fresh, err := gmailrelay.NewSyncer(config, gmail, reopenedState, reopenedQueue)
	require.NoError(t, err)
	require.NoError(t, fresh.ProcessOnce(ctx))
	cursor, err = reopenedState.GetHistoryCursor(ctx)
	require.NoError(t, err)
	require.Equal(t, "101", cursor)
	require.EqualValues(t, 2, historyCalls.Load())
	jobs, err := reopenedQueue.ClaimInbound(ctx, time.Now(), time.Minute, 10)
	require.NoError(t, err)
	require.Len(t, jobs, 1, "fresh sync must deduplicate the already accepted deterministic queue ID")
	require.Equal(t, "gmail:message_added:drain-message", jobs[0].ID)
}
