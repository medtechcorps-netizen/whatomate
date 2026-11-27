package main

import (
	"context"
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
	"github.com/shridarpatil/whatomate/internal/metarelay"
	"github.com/stretchr/testify/require"
)

func metaDrainStore(t *testing.T, endpoint string) (*metarelay.Config, *metarelay.RedisStore, *redis.Client) {
	t.Helper()
	url := os.Getenv("TEST_REDIS_URL")
	if url == "" {
		t.Skip("TEST_REDIS_URL not set; real Redis drain qualification required")
	}
	// Exercise production configuration validation with synthetic credentials.
	// Inherited relay settings must not select a real account or registry.
	for _, item := range os.Environ() {
		name, _, _ := strings.Cut(item, "=")
		if strings.HasPrefix(name, "META_RELAY_") {
			t.Setenv(name, "")
		}
	}
	accounts, err := json.Marshal([]map[string]string{{
		"key": "page", "channel": "messenger", "external_account_id": "page-1",
		"rereply_webhook_url": endpoint, "access_token_env": "DRAIN_PAGE_TOKEN",
		"rereply_inbound_secret_env": "DRAIN_PAGE_INBOUND", "rereply_outbound_secret_env": "DRAIN_PAGE_OUTBOUND",
	}})
	require.NoError(t, err)
	for name, value := range map[string]string{
		"META_RELAY_REDIS_URL": url, "META_RELAY_REDIS_PREFIX": "test:meta-drain:" + uuid.NewString() + ":",
		"META_RELAY_MESSENGER_APP_SECRET": "synthetic-messenger-secret", "META_RELAY_MESSENGER_VERIFY_TOKEN": "synthetic-messenger-verify",
		"META_RELAY_INSTAGRAM_APP_SECRET": "synthetic-instagram-secret", "META_RELAY_INSTAGRAM_VERIFY_TOKEN": "synthetic-instagram-verify",
		"META_RELAY_GRAPH_API_VERSION": "v25.0", "META_RELAY_ACCOUNTS_JSON": string(accounts),
		"META_RELAY_FORWARD_TIMEOUT": "2s", "META_RELAY_PROCESSING_LEASE": "1m",
		"META_RELAY_POLL_INTERVAL": "1h", "META_RELAY_WORKER_CONCURRENCY": "1", "META_RELAY_MAX_ATTEMPTS": "3",
		"DRAIN_PAGE_TOKEN": "synthetic-page-token", "DRAIN_PAGE_INBOUND": "synthetic-page-inbound", "DRAIN_PAGE_OUTBOUND": "synthetic-page-outbound",
	} {
		t.Setenv(name, value)
	}
	config, err := metarelay.LoadConfig()
	require.NoError(t, err)
	store, err := metarelay.NewRedisStore(config)
	require.NoError(t, err)
	options, err := redis.ParseURL(url)
	require.NoError(t, err)
	observer := redis.NewClient(options)
	t.Cleanup(func() {
		_ = store.Close()
		defer func() { _ = observer.Close() }()
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		var cursor uint64
		for {
			keys, next, err := observer.Scan(cleanup, cursor, config.RedisPrefix+"*", 100).Result()
			if err != nil {
				t.Error(err)
				return
			}
			if len(keys) != 0 {
				if err := observer.Del(cleanup, keys...).Err(); err != nil {
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
	return config, store, observer
}

type settlingMetaDrainStore struct {
	*metarelay.RedisStore
	entered chan context.Context
	release <-chan struct{}
	settled atomic.Bool
}

func (store *settlingMetaDrainStore) wait(ctx context.Context) error {
	store.entered <- ctx
	select {
	case <-store.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (store *settlingMetaDrainStore) CompleteInbound(ctx context.Context, id string) error {
	if err := store.wait(ctx); err != nil {
		return err
	}
	if err := store.RedisStore.CompleteInbound(ctx, id); err != nil {
		return err
	}
	store.settled.Store(true)
	return nil
}

func (store *settlingMetaDrainStore) RetryInbound(ctx context.Context, id string, next time.Time, max int, reason string) (int, bool, error) {
	if err := store.wait(ctx); err != nil {
		return 0, false, err
	}
	attempts, dead, err := store.RedisStore.RetryInbound(ctx, id, next, max, reason)
	if err == nil {
		store.settled.Store(true)
	}
	return attempts, dead, err
}

func TestMetaRelayRealRedisSettlementSurvivesShutdown(t *testing.T) {
	for _, firstStatus := range []int{http.StatusAccepted, http.StatusServiceUnavailable} {
		t.Run(http.StatusText(firstStatus), func(t *testing.T) {
			var forwards atomic.Int32
			provider := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				status := http.StatusAccepted
				if forwards.Add(1) == 1 {
					status = firstStatus
				}
				w.WriteHeader(status)
			}))
			defer provider.Close()
			config, queue, observer := metaDrainStore(t, provider.URL)
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			require.NoError(t, queue.Ping(ctx))
			job := metarelay.InboundJob{ID: "shutdown-forward", AccountKey: "page", Body: []byte(`{"events":[]}`)}
			accepted, err := queue.AcceptInbound(ctx, job.ID, []metarelay.InboundJob{job})
			require.NoError(t, err)
			require.True(t, accepted)
			gate, release := metaDrainGate(t)
			settling := &settlingMetaDrainStore{RedisStore: queue, entered: make(chan context.Context, 1), release: gate}
			worker, err := metarelay.NewWorker(config, settling, metarelay.WithWorkerHTTPClient(provider.Client()))
			require.NoError(t, err)
			root, cancelRoot := context.WithCancel(context.Background())
			defer cancelRoot()
			httpStop, intakeStopped := make(chan struct{}), make(chan struct{})
			var closes atomic.Int32
			done := make(chan metaRelayOutcome, 1)
			go func() {
				done <- runMetaRelay(root, 5*time.Second, metaRelayRuntime{
					worker:   worker.Run,
					serve:    func() error { <-httpStop; return http.ErrServerClosed },
					shutdown: func(context.Context) error { close(httpStop); return nil },
					stop:     func() { close(intakeStopped) },
					closeStore: func() error {
						if !settling.settled.Load() {
							return errors.New("store closed before durable settlement")
						}
						closes.Add(1)
						return queue.Close()
					},
					report: func(metaRelayOutcome) {},
				})
			}()
			var settlement context.Context
			select {
			case settlement = <-settling.entered:
			case <-ctx.Done():
				t.Fatal("worker did not reach observed-result settlement")
			}
			require.EqualValues(t, 1, forwards.Load())
			cancelRoot()
			metaDrainWait(t, intakeStopped)
			require.NoError(t, settlement.Err(), "intake cancellation must not cancel settlement")
			deadline, bounded := settlement.Deadline()
			require.True(t, bounded)
			require.True(t, time.Until(deadline) > 0 && time.Until(deadline) <= 5*time.Second)
			require.Zero(t, closes.Load())
			require.NoError(t, queue.Ping(ctx))
			release()
			result := metaDrainResult(t, done)
			require.True(t, result.complete)
			require.True(t, result.reportJoined)
			require.False(t, result.failed)
			require.EqualValues(t, 1, closes.Load())

			// Inspect durable state using a separate Redis connection, then use
			// a fresh production worker to exercise restart recovery.
			processing, err := observer.ZCard(ctx, config.RedisPrefix+"inbound:processing").Result()
			require.NoError(t, err)
			require.Zero(t, processing)
			reopened, err := metarelay.NewRedisStore(config)
			require.NoError(t, err)
			defer func() { _ = reopened.Close() }()
			fresh, err := metarelay.NewWorker(config, reopened, metarelay.WithWorkerHTTPClient(provider.Client()))
			require.NoError(t, err)
			if firstStatus == http.StatusServiceUnavailable {
				attempts, err := observer.HGet(ctx, config.RedisPrefix+"inbound:attempts", job.ID).Int()
				require.NoError(t, err)
				require.Equal(t, 1, attempts)
				due, err := observer.ZScore(ctx, config.RedisPrefix+"inbound:due", job.ID).Result()
				require.NoError(t, err)
				require.Greater(t, due, float64(time.Now().Add(-time.Second).UnixMilli()))
				// Honor the production backoff; no queue timestamp is rewritten.
				require.Eventually(t, func() bool {
					if err := fresh.ProcessOnce(ctx); err != nil {
						t.Error(err)
						return false
					}
					return forwards.Load() == 2
				}, 10*time.Second, 20*time.Millisecond)
			}
			require.NoError(t, fresh.ProcessOnce(ctx))
			expectedCalls := int32(1)
			if firstStatus == http.StatusServiceUnavailable {
				expectedCalls++
			}
			require.Equal(t, expectedCalls, forwards.Load())
			completed, err := observer.Get(ctx, config.RedisPrefix+"inbound:completed:"+job.ID).Result()
			require.NoError(t, err)
			require.Equal(t, "1", completed)
			dueCount, err := observer.ZCard(ctx, config.RedisPrefix+"inbound:due").Result()
			require.NoError(t, err)
			require.Zero(t, dueCount)
		})
	}
}
