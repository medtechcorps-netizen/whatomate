package handlers

import (
	"context"
	"testing"
	"time"

	"github.com/shridarpatil/whatomate/internal/queue"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
)

func TestReadyCheckRequiresFreshWorkerHeartbeat(t *testing.T) {
	db := testutil.SetupTestDB(t)
	rdb := testutil.SetupTestRedis(t)
	if rdb == nil {
		t.Skip("TEST_REDIS_URL not set")
	}
	ctx := context.Background()
	require.NoError(t, rdb.Del(ctx, queue.WorkerHeartbeatKey).Err())
	t.Cleanup(func() {
		_ = rdb.Del(ctx, queue.WorkerHeartbeatKey).Err()
	})

	app := &App{DB: db, Redis: rdb, Log: testutil.NopLogger()}

	missing := testutil.NewGETRequest(t)
	require.NoError(t, app.ReadyCheck(missing))
	testutil.AssertErrorResponse(
		t,
		missing,
		fasthttp.StatusServiceUnavailable,
		"Worker heartbeat unavailable",
	)

	stale := time.Now().UTC().Add(-2 * queue.WorkerHeartbeatTTL)
	require.NoError(t, rdb.Set(
		ctx,
		queue.WorkerHeartbeatKey,
		stale.Format(time.RFC3339Nano),
		queue.WorkerHeartbeatTTL,
	).Err())
	staleReq := testutil.NewGETRequest(t)
	require.NoError(t, app.ReadyCheck(staleReq))
	testutil.AssertErrorResponse(
		t,
		staleReq,
		fasthttp.StatusServiceUnavailable,
		"Worker heartbeat stale",
	)

	require.NoError(t, rdb.Set(
		ctx,
		queue.WorkerHeartbeatKey,
		time.Now().UTC().Format(time.RFC3339Nano),
		queue.WorkerHeartbeatTTL,
	).Err())
	ready := testutil.NewGETRequest(t)
	require.NoError(t, app.ReadyCheck(ready))
	assert.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(ready))
}
