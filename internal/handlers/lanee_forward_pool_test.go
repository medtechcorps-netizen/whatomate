package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/config"
	"github.com/shridarpatil/whatomate/internal/database"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
	"github.com/zerodha/fastglue"
	"github.com/zerodha/logf"
	"gorm.io/gorm"
)

// These are real database/sql waits on a private pool, not BEGIN/UPDATE error
// injection. They cover ordinary non-RLS text forwards. The public 15s budget
// includes the preceding lookups; it is not a fresh 15s acquisition allowance.
type laneEPoolBegin struct {
	started, deadline time.Time
	hasDeadline       bool
	returned          chan struct{}
	err, contextErr   error // Read only after returned closes.
}

// Observe only: unchanged context/options, real *sql.DB.BeginTx, original
// transaction/error. GetDBConn preserves the actual root pool identity.
type laneEPoolObserver struct {
	*sql.DB
	armed    atomic.Bool
	overflow atomic.Bool
	entries  chan *laneEPoolBegin
}

func (p *laneEPoolObserver) GetDBConn() (*sql.DB, error) { return p.DB, nil }

func (p *laneEPoolObserver) BeginTx(ctx context.Context, opts *sql.TxOptions) (gorm.ConnPool, error) {
	var entry *laneEPoolBegin
	if p.armed.Load() {
		entry = &laneEPoolBegin{started: time.Now(), returned: make(chan struct{})}
		entry.deadline, entry.hasDeadline = ctx.Deadline()
		select {
		case p.entries <- entry:
		default:
			p.overflow.Store(true)
		}
	}
	tx, err := p.DB.BeginTx(ctx, opts)
	if entry != nil {
		entry.err, entry.contextErr = err, ctx.Err()
		close(entry.returned)
	}
	return tx, err
}

// logf writes one complete logfmt record at a time. Observe the exact production
// marker timeout before recovery, retaining neither arbitrary logs nor secrets.
type laneEPoolMarkerLog struct {
	mu           sync.Mutex
	org, message uuid.UUID
	matched      bool
	overflow     bool
	timedOut     chan struct{}
}

func (p *laneEPoolMarkerLog) Write(data []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(data) > 8192 {
		p.overflow = true
		return len(data), nil
	}
	line := string(data)
	if p.message != uuid.Nil && !p.matched &&
		strings.Contains(line, `message="Forward provider-phase claim failed; not dispatching"`) &&
		strings.Contains(line, `error="claim forward provider phase: context deadline exceeded"`) &&
		strings.Contains(line, "marker_may_have_committed=false") &&
		strings.Contains(line, "organization_id="+p.org.String()) &&
		strings.Contains(line, "message_id="+p.message.String()) {
		p.matched = true
		close(p.timedOut)
	}
	return len(data), nil
}

type laneEPoolTask struct {
	done chan struct{}
	err  error // Published by closing done.
}

type laneERealPool struct {
	w        *laneEForwardWorld
	ctx      context.Context
	cancel   context.CancelFunc
	app      *App
	db       *gorm.DB
	pool     *sql.DB
	observer *laneEPoolObserver
	logs     *laneEPoolMarkerLog
	held     []*sql.Conn
	tasks    []*laneEPoolTask
	hooks    []func() error
}

func laneENewRealPool(t *testing.T) *laneERealPool {
	t.Helper()
	// Required qualification: do not turn missing DB configuration into a skip.
	require.NotEmpty(t, os.Getenv("TEST_DATABASE_URL"))
	w := laneENewForwardWorld(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	f := &laneERealPool{w: w, ctx: ctx, cancel: cancel}
	t.Cleanup(func() { f.close(t) })
	parsed, err := url.Parse(os.Getenv("TEST_DATABASE_URL"))
	require.NoError(t, err)
	require.Contains(t, []string{"postgres", "postgresql"}, parsed.Scheme)
	query := parsed.Query()
	query.Set("application_name", "lanee_real_pool_"+uuid.NewString())
	query.Set("statement_cache_capacity", "0")
	query.Set("default_query_exec_mode", "describe_exec")
	parsed.RawQuery = query.Encode()
	connectCtx, stopConnect := context.WithTimeout(ctx, 5*time.Second)
	db, err := database.NewPostgresWithContext(connectCtx, &config.DatabaseConfig{
		URL: parsed.String(), MaxOpenConns: 2, MaxIdleConns: 2, ConnMaxLifetime: 300,
	}, false)
	stopConnect()
	require.NoError(t, err)
	f.pool, err = db.DB()
	require.NoError(t, err)
	f.observer = &laneEPoolObserver{DB: f.pool, entries: make(chan *laneEPoolBegin, 32)}
	f.db = db.Session(&gorm.Session{NewDB: true, Context: ctx})
	f.db.Statement.ConnPool = f.observer
	f.logs = &laneEPoolMarkerLog{org: w.org.ID, timedOut: make(chan struct{})}
	// No shared App mutex/wg copy and no queue/hub/Redis side workers. The local
	// Graph client and permissions are real; optional rate limiting is unchanged.
	f.app = &App{DB: f.db, Config: w.app.Config, WhatsApp: w.app.WhatsApp,
		HTTPClient: w.app.HTTPClient, Log: logf.New(logf.Opts{Writer: f.logs, Level: logf.ErrorLevel})}
	actual, err := f.app.DB.DB()
	require.NoError(t, err)
	require.Same(t, f.pool, actual)
	require.Same(t, f.app, f.app.rootApp())
	require.False(t, f.app.rlsEnabled())
	return f
}

func (f *laneERealPool) start(fn func() error) *laneEPoolTask {
	task := &laneEPoolTask{done: make(chan struct{})}
	f.tasks = append(f.tasks, task)
	go func() {
		defer close(task.done)
		task.err = fn() // No assertions or Fatal in a worker.
	}()
	return task
}

func (f *laneERealPool) await(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-f.ctx.Done():
		t.Fatal("owned forward operation did not join within the fixture bound")
	}
}

func (f *laneERealPool) join(t *testing.T, task *laneEPoolTask) {
	t.Helper()
	f.await(t, task.done)
	require.NoError(t, task.err)
}

func (f *laneERealPool) hold(t *testing.T) sql.DBStats {
	t.Helper()
	require.Empty(t, f.held)
	for i := 0; i < 2; i++ {
		ctx, cancel := context.WithTimeout(f.ctx, 5*time.Second)
		conn, err := f.pool.Conn(ctx)
		cancel()
		require.NoError(t, err)
		f.held = append(f.held, conn)
	}
	s := f.pool.Stats()
	require.Equal(t, 2, s.MaxOpenConnections)
	require.Equal(t, 2, s.OpenConnections)
	require.Equal(t, 2, s.InUse)
	require.Zero(t, s.Idle)
	return s
}

func (f *laneERealPool) release() error {
	var errs []error
	for _, conn := range f.held {
		errs = append(errs, conn.Close())
	}
	f.held = nil
	return errors.Join(errs...)
}

func (f *laneERealPool) beginAndWait(t *testing.T, before sql.DBStats) *laneEPoolBegin {
	t.Helper()
	var entry *laneEPoolBegin
	select {
	case entry = <-f.observer.entries:
	case <-f.ctx.Done():
		t.Fatal("expected real BeginTx was not entered")
	}
	require.True(t, entry.hasDeadline)
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		s := f.pool.Stats()
		if s.WaitCount > before.WaitCount {
			require.Equal(t, before.WaitCount+1, s.WaitCount, "no unrelated pool waiter")
			require.Equal(t, 2, s.InUse)
			require.Zero(t, s.Idle)
			select {
			case <-entry.returned:
				t.Fatalf("BeginTx returned before the required live pool wait: %v", entry.err)
			default:
			}
			return entry
		}
		select {
		case <-entry.returned:
			t.Fatalf("BeginTx never waited for capacity: %v", entry.err)
		case <-f.ctx.Done():
			t.Fatal("real pool wait never observed")
		case <-ticker.C:
		}
	}
}

func (f *laneERealPool) request(t *testing.T) *fastglue.Request {
	t.Helper()
	r := testutil.NewJSONRequest(t, map[string]any{"forward_of_message_id": f.w.source.ID.String()})
	testutil.SetAuthContext(r, f.w.org.ID, f.w.user.ID)
	testutil.SetPathParam(r, "id", f.w.target.ID.String())
	return r
}

func (f *laneERealPool) response(t *testing.T, r *fastglue.Request) MessageResponse {
	t.Helper()
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(r), string(testutil.GetResponseBody(r)))
	var response struct {
		Data MessageResponse `json:"data"`
	}
	require.NoError(t, json.Unmarshal(testutil.GetResponseBody(r), &response))
	require.NotEqual(t, uuid.Nil, response.Data.ID)
	require.True(t, response.Data.IsForwarded)
	require.Equal(t, f.w.target.ID, response.Data.ContactID)
	return response.Data
}

func (f *laneERealPool) stored(t *testing.T, id uuid.UUID) models.Message {
	t.Helper()
	var rows []models.Message
	require.NoError(t, f.w.app.DB.WithContext(f.ctx).Where(
		"organization_id = ? AND contact_id = ?", f.w.org.ID, f.w.target.ID).Find(&rows).Error)
	require.Len(t, rows, 1)
	m := rows[0]
	require.Equal(t, id, m.ID)
	assert.Equal(t, f.w.source.Content, m.Content)
	assert.Equal(t, models.DirectionOutgoing, m.Direction)
	provenance, ok := m.Metadata["forwarded_from"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, f.w.source.ID.String(), provenance["message_id"])
	assert.Equal(t, f.w.source.ContactID.String(), provenance["contact_id"])
	var contact models.Contact
	require.NoError(t, f.w.app.DB.WithContext(f.ctx).First(&contact, "id = ?", f.w.target.ID).Error)
	assert.Equal(t, f.w.source.Content, contact.LastMessagePreview)
	var audits int64
	require.NoError(t, f.w.app.DB.WithContext(f.ctx).Model(&models.AuditLog{}).Where(
		"organization_id = ? AND user_id = ? AND resource_type = ? AND resource_id = ?",
		f.w.org.ID, f.w.user.ID, "message", id).Count(&audits).Error)
	assert.EqualValues(t, 1, audits)
	return m
}

func (f *laneERealPool) visible(t *testing.T, id uuid.UUID) MessageResponse {
	t.Helper()
	r := testutil.NewGETRequest(t)
	testutil.SetAuthContext(r, f.w.org.ID, f.w.user.ID)
	testutil.SetPathParam(r, "id", f.w.target.ID.String())
	testutil.SetQueryParam(r, "acknowledge", false)
	task := f.start(func() error { return f.app.GetMessages(r) })
	f.join(t, task)
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(r))
	var response struct {
		Data struct {
			Messages []MessageResponse `json:"messages"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(testutil.GetResponseBody(r), &response))
	require.Len(t, response.Data.Messages, 1)
	m := response.Data.Messages[0]
	assert.Equal(t, id, m.ID)
	assert.True(t, m.IsForwarded)
	assert.Empty(t, m.ForwardDeliveryState)
	return m
}

func (f *laneERealPool) sent(t *testing.T, id uuid.UUID) {
	t.Helper()
	m := f.stored(t, id)
	assert.Equal(t, models.MessageStatusSent, m.Status, m.ErrorMessage)
	assert.Equal(t, "wamid.canonical-test", m.WhatsAppMessageID)
	assert.Equal(t, forwardDispatchProvider, m.Metadata[forwardDispatchMetadataKey])
	assert.NotEmpty(t, m.Metadata[forwardDispatchAtMetadataKey])
	calls := f.w.capture.snapshot()
	require.Len(t, calls, 1)
	assert.Equal(t, f.w.target.PhoneNumber, calls[0]["to"])
	text, ok := calls[0]["text"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, f.w.source.Content, text["body"])
	assert.NotContains(t, calls[0], "context")
	visible := f.visible(t, id)
	assert.Equal(t, models.MessageStatusSent, visible.Status)
	assert.Equal(t, forwardDispatchProvider, visible.ForwardDispatch)
	assert.NotEmpty(t, visible.ForwardDispatchAt)
}

func (f *laneERealPool) close(t *testing.T) {
	t.Helper()
	// Each stage is attempted even when another cleanup stage fails. Cancelling
	// the test context releases the lookup barrier; production background marker
	// and recovery contexts retain their own bounded deadlines.
	assert.NoError(t, f.release())
	f.cancel()
	joinCtx, cancelJoin := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancelJoin()
	for _, task := range f.tasks {
		select {
		case <-task.done:
		case <-joinCtx.Done():
			t.Error("owned forward task/worker did not join during cleanup")
		}
	}
	for _, remove := range f.hooks {
		assert.NoError(t, remove())
	}
	if f.pool != nil {
		closed := make(chan error, 1)
		go func() { closed <- f.pool.Close() }()
		select {
		case err := <-closed:
			assert.NoError(t, err)
		case <-time.After(10 * time.Second):
			t.Error("owned SQL pool did not close")
		}
	}
	if f.observer != nil {
		assert.False(t, f.observer.overflow.Load(), "transaction observations overflowed")
	}
	if f.logs != nil {
		f.logs.mu.Lock()
		assert.False(t, f.logs.overflow, "marker log observation overflowed")
		f.logs.mu.Unlock()
	}
}

func TestLaneEForwardPoolSaturationBeforePendingCommit(t *testing.T) {
	f := laneENewRealPool(t)
	var before models.Contact
	require.NoError(t, f.w.app.DB.WithContext(f.ctx).First(&before, "id = ?", f.w.target.ID).Error)
	type boundary struct {
		deadline    time.Time
		hasDeadline bool
		stats       sql.DBStats
		rows        int64
		err         error
	}
	reached := make(chan boundary, 1)
	resume := make(chan struct{})
	var used atomic.Bool
	hook := "lanee:real_pool_account:" + uuid.NewString()
	// GORM Query returns (and closes its sql.Rows) before AfterQuery callbacks.
	// InUse==0 at this exact successful account lookup proves no held request
	// connection is being mistaken for external pool pressure.
	require.NoError(t, f.db.Callback().Query().After("gorm:after_query").Register(hook, func(tx *gorm.DB) {
		account, ok := tx.Statement.Dest.(*models.WhatsAppAccount)
		if !ok || account.ID != f.w.account.ID || account.OrganizationID != f.w.org.ID || !used.CompareAndSwap(false, true) {
			return
		}
		deadline, hasDeadline := tx.Statement.Context.Deadline()
		reached <- boundary{deadline, hasDeadline, f.pool.Stats(), tx.RowsAffected, tx.Error}
		select {
		case <-resume:
		case <-f.ctx.Done():
		}
	}))
	f.hooks = append(f.hooks, func() error { return f.db.Callback().Query().Remove(hook) })
	r := f.request(t)
	task := f.start(func() error { err := f.app.SendMessage(r); f.app.WaitForBackgroundTasks(); return err })
	var at boundary
	select {
	case at = <-reached:
	case <-f.ctx.Done():
		t.Fatal("final account lookup barrier was not reached")
	}
	require.NoError(t, at.err)
	require.EqualValues(t, 1, at.rows)
	require.True(t, at.hasDeadline)
	require.True(t, time.Now().Before(at.deadline), "request must have budget left at the boundary")
	require.Zero(t, at.stats.InUse)
	baseline := f.hold(t)
	f.observer.armed.Store(true)
	close(resume)
	entry := f.beginAndWait(t, baseline)
	require.Equal(t, at.deadline, entry.deadline, "same whole-request deadline, not a fresh acquisition allowance")
	f.join(t, task)
	f.await(t, entry.returned)
	require.ErrorIs(t, entry.err, context.DeadlineExceeded)
	require.ErrorIs(t, entry.contextErr, context.DeadlineExceeded)
	require.Greater(t, f.pool.Stats().WaitDuration, baseline.WaitDuration)
	require.Equal(t, 2, f.pool.Stats().InUse)
	require.Equal(t, fasthttp.StatusServiceUnavailable, testutil.GetResponseStatusCode(r))
	var refusal struct {
		ErrorType string `json:"error_type"`
	}
	require.NoError(t, json.Unmarshal(testutil.GetResponseBody(r), &refusal))
	assert.Equal(t, string(forwardErrorTemporarilyUnavailable), refusal.ErrorType)
	var rows, audits int64
	require.NoError(t, f.w.app.DB.WithContext(f.ctx).Model(&models.Message{}).
		Where("organization_id = ? AND contact_id = ?", f.w.org.ID, f.w.target.ID).Count(&rows).Error)
	require.NoError(t, f.w.app.DB.WithContext(f.ctx).Model(&models.AuditLog{}).
		Where("organization_id = ? AND user_id = ? AND resource_type = ?", f.w.org.ID, f.w.user.ID, "message").Count(&audits).Error)
	assert.Zero(t, rows)
	assert.Zero(t, audits)
	var after models.Contact
	require.NoError(t, f.w.app.DB.WithContext(f.ctx).First(&after, "id = ?", f.w.target.ID).Error)
	assert.Equal(t, before.LastMessagePreview, after.LastMessagePreview)
	assert.Empty(t, f.w.capture.snapshot())
	require.NoError(t, f.release())
	// An explicit new user request proves the fixture remains sendable. No
	// automatic retry, callback replay, or second provider effect is introduced.
	retry := f.request(t)
	f.join(t, f.start(func() error { err := f.app.SendMessage(retry); f.app.WaitForBackgroundTasks(); return err }))
	f.sent(t, f.response(t, retry).ID)
}

func TestLaneEForwardProviderMarkerPoolSaturation(t *testing.T) {
	for _, name := range []string{"release_before_marker_deadline", "marker_deadline_then_fresh_settlement"} {
		t.Run(name, func(t *testing.T) {
			f := laneENewRealPool(t)
			// Capture every after-commit callback using the established scoped
			// seam, while SendMessage still performs a real root-pool commit.
			scoped := f.app.scopedApp(f.app.DB, f.w.org.ID)
			r := f.request(t)
			f.join(t, f.start(func() error { return scoped.SendMessage(r) }))
			response := f.response(t, r)
			assert.Equal(t, models.MessageStatusPending, response.Status)
			m := f.stored(t, response.ID)
			require.Equal(t, models.MessageStatusPending, m.Status)
			require.Equal(t, forwardDispatchQueued, m.Metadata[forwardDispatchMetadataKey])
			require.NotContains(t, m.Metadata, forwardDispatchAtMetadataKey)
			require.Empty(t, f.w.capture.snapshot())
			callbacks := scoped.takeAfterCommit()
			require.NotEmpty(t, callbacks)
			require.Empty(t, scoped.takeAfterCommit(), "destructive take: no duplicated callbacks")
			f.logs.mu.Lock()
			f.logs.message = m.ID
			f.logs.mu.Unlock()
			baseline := f.hold(t)
			f.observer.armed.Store(true)
			ran := 0
			worker := f.start(func() error {
				for _, callback := range callbacks {
					f.app.runAfterCommit([]func(){callback})
					ran++
				}
				f.app.WaitForBackgroundTasks()
				return nil
			})
			entry := f.beginAndWait(t, baseline)
			require.True(t, entry.deadline.After(entry.started))
			require.LessOrEqual(t, entry.deadline.Sub(entry.started), forwardAccountCheckTimeout)
			if name == "release_before_marker_deadline" {
				require.NoError(t, f.release())
				f.await(t, entry.returned)
				require.NoError(t, entry.err)
				require.NoError(t, entry.contextErr)
				f.join(t, worker)
				f.sent(t, m.ID)
				f.logs.mu.Lock()
				matched := f.logs.matched
				f.logs.mu.Unlock()
				assert.False(t, matched)
			} else {
				// The production log is emitted after the real acquisition timeout
				// and before recovery. Release here, never after a guessed sleep.
				f.await(t, f.logs.timedOut)
				f.await(t, entry.returned)
				require.ErrorIs(t, entry.err, context.DeadlineExceeded)
				require.ErrorIs(t, entry.contextErr, context.DeadlineExceeded)
				require.Equal(t, 2, f.pool.Stats().InUse)
				require.NoError(t, f.release())
				f.join(t, worker)
				m = f.stored(t, m.ID)
				assert.Equal(t, models.MessageStatusFailed, m.Status)
				assert.Equal(t, forwardServerBusyError().Error(), m.ErrorMessage)
				assert.Equal(t, forwardDispatchQueued, m.Metadata[forwardDispatchMetadataKey])
				assert.NotContains(t, m.Metadata, forwardDispatchAtMetadataKey)
				assert.Empty(t, f.w.capture.snapshot())
				visible := f.visible(t, m.ID)
				assert.Equal(t, models.MessageStatusFailed, visible.Status)
				assert.Equal(t, forwardServerBusyError().Error(), visible.Error)
				assert.Equal(t, forwardDispatchQueued, visible.ForwardDispatch)
			}
			assert.Equal(t, len(callbacks), ran, "every captured callback ran once")
			assert.Greater(t, f.pool.Stats().WaitDuration, baseline.WaitDuration)
			beforeCalls := len(f.w.capture.snapshot())
			claimed, err := f.app.claimForwardProviderPhase(f.w.org.ID, m.ID)
			require.NoError(t, err)
			assert.False(t, claimed, "settled forward cannot be claimed or resent")
			assert.Len(t, f.w.capture.snapshot(), beforeCalls)
		})
	}
}
