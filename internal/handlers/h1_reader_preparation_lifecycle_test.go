package handlers

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestH1ReaderPreparation_WriterFallbackBeginsAfterReaderSQLAndBeforeMedia(t *testing.T) {
	for _, route := range []string{"media", "existing-session"} {
		t.Run(route, func(t *testing.T) {
			f := h1CapacityLegacy(t, 1)
			work := *f.works[0]
			x := f.app.scopedApp(f.app.DB, work.OrganizationID)
			x.tenantOrgID = uuid.Nil
			x.inboundContinuation = &inboundContinuationExecution{OrganizationID: work.OrganizationID,
				ContactID: work.Contact.ID, MessageID: work.Persisted.ID, WAMID: work.Message.ID}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			reader := h1OrgHold(t, &f.app.continuationAdmission, 1, inboundContinuationFreshPriority,
				func() int { return 10 }, work.OrganizationID.String(), inboundContinuationReader)
			const sentinel = "synthetic writer admission cancelled before ownership"
			calls := 0
			acquireWriter := func() error {
				calls++
				require.Zero(t, f.pool.Stats().InUse, "the reader transaction must commit before requesting writer admission")
				reader()
				h1OrgEmpty(t, &f.app.continuationAdmission)
				return errors.New(sentinel)
			}
			var err error
			if route == "media" {
				work.Extracted.Media = &MediaInfo{MediaURL: "synthetic-media-id", MediaMimeType: "image/jpeg"}
				_, err = x.prepareIncomingContinuation(ctx, &work, acquireWriter)
			} else {
				_, _, err = x.getOrCreatePreparedIncomingSessionWithWriter(work.OrganizationID, work.Contact.ID,
					work.Account.Name, work.Message.From, 30, acquireWriter)
			}
			require.ErrorContains(t, err, sentinel)
			require.Equal(t, 1, calls)
			require.Zero(t, f.model.calls.Load())
			require.Zero(t, f.pool.Stats().InUse)
			h1OrgEmpty(t, &f.app.continuationAdmission)
		})
	}
}

func TestH1ReaderPreparation_ReplacementWriterIsReleasedAfterPanic(t *testing.T) {
	f := h1CapacityLegacy(t, 1)
	var injected atomic.Bool
	callback := "h1:reader-writer-panic:" + uuid.NewString()
	require.NoError(t, f.app.DB.Callback().Update().Before("gorm:update").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Schema == nil || tx.Statement.Schema.Table != "chatbot_sessions" {
			return
		}
		g := &f.app.continuationAdmission
		g.mu.Lock()
		phase := g.phases[f.works[0].OrganizationID.String()]
		writer := phase != nil && phase.activeMode == inboundContinuationWriter && phase.active == 1 && g.used == 1
		g.mu.Unlock()
		if writer && injected.CompareAndSwap(false, true) {
			panic("synthetic replacement-writer panic")
		}
	}))
	t.Cleanup(func() { _ = f.app.DB.Callback().Update().Remove(callback) })
	p := NewInboundContinuationProcessor(f.app, time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	err := p.ProcessMessage(ctx, f.works[0].OrganizationID, f.works[0].Persisted.ID)
	require.True(t, injected.Load(), "panic must occur with the replacement writer actually owned")
	require.ErrorContains(t, err, "synthetic replacement-writer panic")
	require.Zero(t, f.model.calls.Load())
	h1OrgEmpty(t, &f.app.continuationAdmission)
	require.Eventually(t, func() bool { return f.pool.Stats().InUse == 0 }, time.Second, time.Millisecond)
}

func TestH1ReaderPreparation_TransitionYieldsToQueuedApplyAndRefusesTinyResize(t *testing.T) {
	for _, change := range []string{"queued-apply", "shrink-to-two"} {
		t.Run(change, func(t *testing.T) {
			f := h1CapacityLegacy(t, 1)
			h1ReaderPreparationRemoveSeed(t, f)
			g := &f.app.continuationAdmission
			var fillers []func()
			if change == "queued-apply" {
				for range 6 {
					fillers = append(fillers, h1OrgHold(t, g, 1, inboundContinuationFreshPriority, func() int { return 10 }, "", inboundContinuationUnscoped))
				}
			}
			entered, finish := make(chan struct{}), make(chan struct{})
			var once, closeOnce sync.Once
			unblock := func() { closeOnce.Do(func() { close(finish) }) }
			callback := "h1:reader-transition:" + uuid.NewString()
			require.NoError(t, f.app.DB.Callback().Create().After("gorm:create").Register(callback, func(tx *gorm.DB) {
				if tx.Statement.Schema == nil || tx.Statement.Schema.Table != "chatbot_sessions" || tx.Error != nil {
					return
				}
				once.Do(func() {
					close(entered)
					select {
					case <-finish:
					case <-tx.Statement.Context.Done():
					}
				})
			}))
			t.Cleanup(func() { _ = f.app.DB.Callback().Create().Remove(callback) })
			done := f.start(t, 0)
			t.Cleanup(unblock)
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("new-session preparation did not reach the transition barrier")
			}
			if change == "queued-apply" {
				apply := h1OrgAcquireAsync(t, g, context.Background(), nil, 2, inboundContinuationApplyPriority,
					func() int { return 10 }, f.works[0].OrganizationID.String(), inboundContinuationReader)
				h1CapacityWaitQueue(t, g, 7, 1)
				unblock()
				a := h1OrgGrant(t, apply)
				h1CapacityWaitQueue(t, g, 8, 1)
				require.Zero(t, f.model.calls.Load(), "the queued apply gets the released preparation credit before a new perform grant")
				a()
				select {
				case <-f.model.entered:
				case <-time.After(5 * time.Second):
					t.Fatal("model did not resume after accepted apply released")
				}
				f.model.unblock()
				h1CapacityJoin(t, done, 1)
				h1ReaderPreparationLoadSession(t, f)
				f.assertGenerated(t, 0)
			} else {
				f.pool.SetMaxOpenConns(2)
				t.Cleanup(func() { f.pool.SetMaxOpenConns(10) })
				unblock()
				select {
				case err := <-done:
					require.Error(t, err)
					require.False(t, inboundContinuationRequiresManualReview(err))
				case <-time.After(5 * time.Second):
					t.Fatal("tiny resize did not refuse model admission")
				}
				require.Zero(t, f.model.calls.Load())
				var actions int64
				require.NoError(t, f.shared.Model(&models.ScheduledJob{}).Where("organization_id = ? AND kind = ? AND aggregate_id = ?",
					f.works[0].OrganizationID, inboundContinuationActionJobKind, f.works[0].Persisted.ID).Count(&actions).Error)
				require.Zero(t, actions)
				job := loadInboundContinuationJob(t, &App{DB: f.shared}, f.works[0].OrganizationID, f.works[0].Persisted.ID)
				require.Equal(t, models.ScheduledJobStatusPending, job.Status)
			}
			for _, release := range fillers {
				release()
			}
			h1OrgEmpty(t, g)
		})
	}
}

func TestH1ReaderPreparation_NewSessionAcceptedResultsSurviveCompletionRetry(t *testing.T) {
	f := h1CapacityLegacy(t, 1)
	h1ReaderPreparationRemoveSeed(t, f)
	const sentinel = "synthetic combined-reader completion rejected once"
	var injected atomic.Bool
	callback := "h1:reader-completion:" + uuid.NewString()
	require.NoError(t, f.app.DB.Callback().Update().Before("gorm:update").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Schema == nil || tx.Statement.Schema.Table != "messages" {
			return
		}
		updates, ok := tx.Statement.Dest.(map[string]interface{})
		if !ok {
			return
		}
		metadata, ok := updates["metadata"].(models.JSONB)
		if ok && metadata["inbound_continuation_completed"] == true && injected.CompareAndSwap(false, true) {
			_ = tx.AddError(errors.New(sentinel))
		}
	}))
	t.Cleanup(func() { _ = f.app.DB.Callback().Update().Remove(callback) })
	f.model.unblock()
	select {
	case err := <-f.start(t, 0):
		require.ErrorContains(t, err, sentinel)
		require.False(t, inboundContinuationRequiresManualReview(err))
	case <-time.After(30 * time.Second):
		t.Fatal("combined-reader completion fault did not settle")
	}
	require.True(t, injected.Load())
	work := f.works[0]
	job := loadInboundContinuationJob(t, &App{DB: f.shared}, work.OrganizationID, work.Persisted.ID)
	require.Equal(t, models.ScheduledJobStatusPending, job.Status)
	var actions []models.ScheduledJob
	require.NoError(t, f.shared.Where("organization_id = ? AND kind = ? AND aggregate_id = ?", work.OrganizationID, inboundContinuationActionJobKind, work.Persisted.ID).Find(&actions).Error)
	require.Len(t, actions, 2)
	for _, action := range actions {
		require.Equal(t, inboundActionStateResolved, action.Payload["state"])
	}
	p := NewInboundContinuationProcessor(f.app, time.Second)
	p.now = func() time.Time { return job.RunAt.Add(time.Second) }
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	require.NoError(t, p.ProcessMessage(ctx, work.OrganizationID, work.Persisted.ID))
	finished := loadInboundContinuationJob(t, &App{DB: f.shared}, work.OrganizationID, work.Persisted.ID)
	require.Equal(t, models.ScheduledJobStatusCompleted, finished.Status)
	require.EqualValues(t, 1, f.model.calls.Load())
	f.mu.Lock()
	sends := len(f.sent)
	f.mu.Unlock()
	require.Equal(t, 1, sends, "retry reuses the independently accepted result across writer fallback")
	h1OrgEmpty(t, &f.app.continuationAdmission)
}
