package worker

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/config"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestConsumerDoesNotReportJoinedBeforeHeartbeat(t *testing.T) {
	heartbeatStarted := make(chan struct{})
	heartbeatCancelled := make(chan struct{})
	release := make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	want := errors.New("consumer stopped")
	done := make(chan error, 1)
	go func() {
		done <- consumeWithHeartbeat(context.Background(), func(context.Context) error { <-heartbeatStarted; return want }, func(ctx context.Context) { close(heartbeatStarted); <-ctx.Done(); close(heartbeatCancelled); <-release })
	}()
	select {
	case <-heartbeatCancelled:
	case <-time.After(time.Second):
		t.Fatal("heartbeat cancellation not requested")
	}
	select {
	case <-done:
		t.Fatal("consumer returned before heartbeat joined")
	default:
	}
	close(release)
	select {
	case err := <-done:
		require.ErrorIs(t, err, want)
	case <-time.After(time.Second):
		t.Fatal("heartbeat did not join")
	}
}

// These cases use their own real PostgreSQL pool, never altering the shared
// fixture's capacity. Holding its only connection reproduces shutdown during
// a pool wait; cancellation must reach the driver before any SQL/provider work.
func TestWorkerIntakeCancellationReleasesSaturatedPoolWait(t *testing.T) {
	testutil.SetupTestDB(t)
	db, err := gorm.Open(postgres.Open(os.Getenv("TEST_DATABASE_URL")), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	pool, err := db.DB()
	require.NoError(t, err)
	pool.SetMaxOpenConns(1)
	pool.SetMaxIdleConns(1)
	t.Cleanup(func() { require.NoError(t, pool.Close()) })
	w := &Worker{DB: db, Config: &config.Config{}, Log: testutil.NopLogger()}
	cases := []struct {
		name string
		run  func(context.Context) error
	}{
		{"outbox_discovery", func(ctx context.Context) error {
			_, err := w.ProcessChannelOutboxBatch(ctx, "shutdown-test", 1)
			return err
		}},
		{"ai_discovery", func(ctx context.Context) error {
			_, err := w.ProcessChannelAIReplyBatch(ctx, "shutdown-test", 1)
			return err
		}},
		{"credential_discovery", func(ctx context.Context) error { _, err := w.ProcessThreadsCredentialRefreshBatch(ctx, 1); return err }},
		{"outbox_claim", func(ctx context.Context) error {
			_, _, err := w.intakeContext(ctx).claimChannelOutboxJob(uuid.New(), "shutdown-test")
			return err
		}},
		{"ai_claim", func(ctx context.Context) error {
			_, _, err := w.intakeContext(ctx).claimChannelAIReplyJob(uuid.New(), "shutdown-test")
			return err
		}},
		{"credential_claim", func(ctx context.Context) error {
			_, _, err := w.intakeContext(ctx).claimThreadsCredentialRefresh(uuid.New(), time.Now(), time.Now())
			return err
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			connection, err := pool.Conn(context.Background())
			require.NoError(t, err)
			defer connection.Close() //nolint:errcheck
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			waits := pool.Stats().WaitCount
			done := make(chan error, 1)
			go func() { done <- tc.run(ctx) }()
			require.Eventually(t, func() bool { return pool.Stats().WaitCount > waits }, time.Second, 5*time.Millisecond, "statement reached pool wait")
			cancel()
			select {
			case err := <-done:
				require.ErrorIs(t, err, context.Canceled)
			case <-time.After(time.Second):
				_ = connection.Close()
				t.Fatal("intake ignored shutdown cancellation")
			}
			require.NoError(t, w.DB.Statement.Context.Err(), "original finalization DB context remains live")
		})
	}
}
