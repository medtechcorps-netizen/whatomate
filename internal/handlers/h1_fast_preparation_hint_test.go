package handlers

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func h1FastHintSQL(tx *gorm.DB) bool {
	return tx.Statement != nil && strings.Contains(tx.Statement.SQL.String(), "SELECT EXISTS") &&
		strings.Contains(tx.Statement.SQL.String(), "JOIN chatbot_sessions AS s")
}

func h1FastHintObserve(t *testing.T, app *App, after bool, fn func(*gorm.DB)) {
	t.Helper()
	name := "h1:fast-hint:" + uuid.NewString()
	callback := app.DB.Callback().Row().Before("gorm:row")
	if after {
		callback = app.DB.Callback().Row().After("gorm:row")
	}
	require.NoError(t, callback.Register(name, func(tx *gorm.DB) {
		if h1FastHintSQL(tx) {
			fn(tx)
		}
	}))
	t.Cleanup(func() { _ = app.DB.Callback().Row().Remove(name) })
}

// Explicitly simulate a conservative stale-negative advisory observation. The
// real tenant transaction still runs; no authoritative preparation query changes.
func h1FastHintNegative(t *testing.T, app *App) *atomic.Int32 {
	t.Helper()
	hits := &atomic.Int32{}
	h1FastHintObserve(t, app, false, func(tx *gorm.DB) {
		hits.Add(1)
		tx.Statement.SQL.Reset()
		tx.Statement.SQL.WriteString("SELECT false")
		tx.Statement.Vars = nil
	})
	return hits
}

func h1FastHintStart(t *testing.T, f *h1CapacityLegacyFixture, p *InboundContinuationProcessor,
	ctx context.Context, cancel context.CancelFunc) <-chan error {
	t.Helper()
	done, joined := make(chan error, 1), make(chan struct{})
	t.Cleanup(func() {
		cancel()
		f.model.unblock()
		select {
		case <-joined:
		case <-time.After(12 * time.Second):
			t.Error("hint processor did not join")
		}
	})
	go func() {
		defer close(joined)
		done <- p.ProcessMessage(ctx, f.works[0].OrganizationID, f.works[0].Persisted.ID)
	}()
	return done
}

func h1FastHintWriterQueued(t *testing.T, f *h1CapacityLegacyFixture) {
	t.Helper()
	g := &f.app.continuationAdmission
	require.Eventually(t, func() bool {
		g.mu.Lock()
		defer g.mu.Unlock()
		return g.used == 1 && len(g.waiters) == 1 &&
			g.waiters[0].organization == f.works[0].OrganizationID.String() &&
			g.waiters[0].mode == inboundContinuationWriter &&
			g.waiters[0].priority == inboundContinuationPerformPriority && !g.waiters[0].intake
	}, 5*time.Second, time.Millisecond)
}

func TestH1FastPreparationHint_EligibilityAndTenantIsolation(t *testing.T) {
	for _, state := range []string{"pending", "completed", "failed", "future", "fresh-lease", "lease-equality", "stale-lease", "null-lease", "exhausted", "deleted-job", "missing-job", "wrong-kind", "foreign-message", "outgoing-message", "deleted-message", "wrong-account", "expired-session", "duplicate-session", "deleted-session"} {
		t.Run(state, func(t *testing.T) {
			f := h1CapacityLegacy(t, 2)
			work := f.works[0]
			job := loadInboundContinuationJob(t, &App{DB: f.shared}, work.OrganizationID, work.Persisted.ID)
			p := NewInboundContinuationProcessor(f.app, time.Second)
			now := time.Now().UTC().Add(time.Second)
			p.now = func() time.Time { return now }
			messageID, want := work.Persisted.ID, inboundContinuationReader
			updates := map[string]interface{}{}
			switch state {
			case "pending":
				want = inboundContinuationWriter
			case "completed":
				updates["status"] = models.ScheduledJobStatusCompleted
			case "failed":
				updates["status"] = models.ScheduledJobStatusFailed
			case "future":
				updates["run_at"] = now.Add(time.Hour)
			case "fresh-lease":
				updates["status"], updates["locked_at"] = models.ScheduledJobStatusProcessing, now
			case "lease-equality":
				updates["status"], updates["locked_at"] = models.ScheduledJobStatusProcessing, now.Add(-p.lease)
			case "stale-lease":
				updates["status"], updates["locked_at"] = models.ScheduledJobStatusProcessing, now.Add(-p.lease-time.Second)
				want = inboundContinuationWriter
			case "null-lease":
				updates["status"], updates["locked_at"] = models.ScheduledJobStatusProcessing, nil
				want = inboundContinuationWriter
			case "exhausted":
				updates["attempts"] = job.MaxAttempts
			case "deleted-job":
				require.NoError(t, f.shared.Delete(&job).Error)
			case "missing-job":
				require.NoError(t, f.shared.Unscoped().Delete(&job).Error)
			case "wrong-kind":
				updates["kind"] = inboundContinuationActionJobKind
			case "foreign-message":
				messageID = f.works[1].Persisted.ID
			case "outgoing-message":
				require.NoError(t, f.shared.Model(&models.Message{}).Where("id = ?", messageID).Update("direction", models.DirectionOutgoing).Error)
			case "deleted-message":
				require.NoError(t, f.shared.Where("id = ?", messageID).Delete(&models.Message{}).Error)
			case "wrong-account":
				require.NoError(t, f.shared.Model(&models.ChatbotSession{}).Where("id = ?", f.sessions[0].ID).Update("whats_app_account", "synthetic-other-account").Error)
			case "expired-session":
				require.NoError(t, f.shared.Model(&models.ChatbotSession{}).Where("id = ?", f.sessions[0].ID).Update("last_activity_at", now.Add(-24*time.Hour)).Error)
				want = inboundContinuationWriter
			case "duplicate-session":
				duplicate := *f.sessions[0]
				duplicate.ID = uuid.New()
				require.NoError(t, f.shared.Create(&duplicate).Error)
				want = inboundContinuationWriter
			case "deleted-session":
				h1ReaderPreparationRemoveSeed(t, f)
			}
			if len(updates) != 0 {
				require.NoError(t, f.shared.Model(&models.ScheduledJob{}).Where("id = ?", job.ID).Updates(updates).Error)
			}
			var before models.ScheduledJob
			beforeErr := f.shared.Unscoped().Where("id = ?", job.ID).First(&before).Error
			var queries atomic.Int32
			h1FastHintObserve(t, f.app, false, func(*gorm.DB) { queries.Add(1) })
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			mode, err := p.hintInboundContinuationPreparationMode(ctx, work.OrganizationID, messageID)
			require.NoError(t, err)
			require.Equal(t, want, mode)
			require.EqualValues(t, 1, queries.Load(), "exercise the real tenant-scoped advisory query")
			var after models.ScheduledJob
			afterErr := f.shared.Unscoped().Where("id = ?", job.ID).First(&after).Error
			if beforeErr != nil {
				require.ErrorIs(t, beforeErr, gorm.ErrRecordNotFound)
				require.ErrorIs(t, afterErr, gorm.ErrRecordNotFound)
			} else {
				require.NoError(t, afterErr)
				require.Equal(t, before, after, "hint must neither claim nor terminalize exhausted work")
			}
			h1OrgEmpty(t, &f.app.continuationAdmission)
			require.Zero(t, f.pool.Stats().InUse)
		})
	}
}

func TestH1FastPreparationHint_StaleSessionObservationRetainsAuthority(t *testing.T) {
	for _, change := range []string{"positive-disappears", "negative-appears"} {
		t.Run(change, func(t *testing.T) {
			f := h1CapacityLegacy(t, 1)
			if change == "negative-appears" {
				h1ReaderPreparationRemoveSeed(t, f)
			}
			stopGuard := h1ReaderPreparationHoldGuard(t, f)
			var changed atomic.Bool
			mutation := make(chan error, 1)
			h1FastHintObserve(t, f.app, true, func(tx *gorm.DB) {
				if tx.Error != nil || !changed.CompareAndSwap(false, true) {
					return
				}
				// The SELECT has its snapshot; a separate committed mutation races
				// the later authoritative preparation without altering its result.
				if change == "positive-disappears" {
					mutation <- f.shared.Where("id = ?", f.sessions[0].ID).Delete(&models.ChatbotSession{}).Error
				} else {
					mutation <- f.shared.Unscoped().Model(&models.ChatbotSession{}).Where("id = ?", f.sessions[0].ID).Update("deleted_at", nil).Error
				}
			})
			p := NewInboundContinuationProcessor(f.app, time.Second)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			done := h1FastHintStart(t, f, p, ctx, cancel)
			t.Cleanup(func() { cancel(); stopGuard() })
			h1FastHintWriterQueued(t, f)
			select {
			case err := <-mutation:
				require.NoError(t, err)
			case <-time.After(time.Second):
				t.Fatal("the real advisory statement did not observe the mutation boundary")
			}
			job := loadInboundContinuationJob(t, &App{DB: f.shared}, f.works[0].OrganizationID, f.works[0].Persisted.ID)
			if change == "positive-disappears" {
				require.Equal(t, models.ScheduledJobStatusPending, job.Status, "stale positive conservatively waits before claiming")
			} else {
				require.Equal(t, models.ScheduledJobStatusProcessing, job.Status, "stale negative must take the real post-claim writer fallback")
			}
			stopGuard()
			f.model.unblock()
			h1CapacityJoin(t, done, 1)
			h1ReaderPreparationLoadSession(t, f)
			f.assertGenerated(t, 0)
			require.NoError(t, p.ProcessMessage(ctx, f.works[0].OrganizationID, f.works[0].Persisted.ID))
			require.EqualValues(t, 1, f.model.calls.Load())
			h1OrgEmpty(t, &f.app.continuationAdmission)
		})
	}
}

func TestH1FastPreparationHint_SQLFailureFallsBackWithoutClaimLoss(t *testing.T) {
	f := h1CapacityLegacy(t, 1)
	stopGuard := h1ReaderPreparationHoldGuard(t, f)
	var queries atomic.Int32
	h1FastHintObserve(t, f.app, false, func(tx *gorm.DB) {
		queries.Add(1)
		tx.Statement.SQL.Reset()
		tx.Statement.SQL.WriteString("SELECT 1 / 0")
		tx.Statement.Vars = nil
	})
	p := NewInboundContinuationProcessor(f.app, time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	done := h1FastHintStart(t, f, p, ctx, cancel)
	t.Cleanup(func() { cancel(); stopGuard() })
	h1FastHintWriterQueued(t, f)
	require.EqualValues(t, 1, queries.Load())
	job := loadInboundContinuationJob(t, &App{DB: f.shared}, f.works[0].OrganizationID, f.works[0].Persisted.ID)
	require.Equal(t, models.ScheduledJobStatusProcessing, job.Status, "an optional SQL abort falls back to original claim and writer path")
	require.Eventually(t, func() bool { return f.pool.Stats().InUse == 1 }, time.Second, time.Millisecond)
	stopGuard()
	f.model.unblock()
	h1CapacityJoin(t, done, 1)
	f.assertGenerated(t, 0)
	h1OrgEmpty(t, &f.app.continuationAdmission)
}

func TestH1FastPreparationHint_OptionalQueueDeadlineReleasesOnlyItsWaiter(t *testing.T) {
	f := h1CapacityLegacy(t, 1)
	g := &f.app.continuationAdmission
	var held []func()
	for range 8 {
		held = append(held, h1OrgHold(t, g, 1, inboundContinuationFreshPriority, func() int { return 10 }, "", inboundContinuationUnscoped))
	}
	var queries atomic.Int32
	h1FastHintObserve(t, f.app, false, func(*gorm.DB) { queries.Add(1) })
	p := NewInboundContinuationProcessor(f.app, time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
	done := h1FastHintStart(t, f, p, ctx, cancel)
	t.Cleanup(func() {
		cancel()
		for _, release := range held {
			release()
		}
	})
	h1CapacityWaitQueue(t, g, 8, 1)
	g.mu.Lock()
	priority, mode, org := g.waiters[0].priority, g.waiters[0].mode, g.waiters[0].organization
	g.mu.Unlock()
	require.Equal(t, inboundContinuationFreshPriority, priority)
	require.Equal(t, inboundContinuationUnscoped, mode)
	require.Empty(t, org)
	require.Eventually(t, func() bool {
		g.mu.Lock()
		defer g.mu.Unlock()
		return g.used == 8 && len(g.waiters) == 1 && g.waiters[0].organization == f.works[0].OrganizationID.String() && g.waiters[0].mode == inboundContinuationReader && g.waiters[0].priority == inboundContinuationFreshPriority
	}, 25*time.Second, 10*time.Millisecond, "expired optional queue must yield to actual Reader admission on the live original context")
	require.NoError(t, ctx.Err())
	require.Zero(t, queries.Load(), "optional discovery never owned capacity, so it must not query")
	job := loadInboundContinuationJob(t, &App{DB: f.shared}, f.works[0].OrganizationID, f.works[0].Persisted.ID)
	require.Equal(t, models.ScheduledJobStatusPending, job.Status)
	require.Zero(t, job.Attempts)
	require.Zero(t, f.pool.Stats().InUse)
	for _, release := range held {
		release()
	}
	f.model.unblock()
	h1CapacityJoin(t, done, 1)
	f.assertGenerated(t, 0)
	require.Zero(t, queries.Load(), "the authoritative path must not restart optional discovery")
	h1OrgEmpty(t, g)
}

func TestH1FastPreparationHint_ParentCancellationAndStopPrecedeClaim(t *testing.T) {
	for _, boundary := range []string{"queue-cancel", "queue-stop", "sql-cancel", "sql-stop"} {
		t.Run(boundary, func(t *testing.T) {
			f := h1CapacityLegacy(t, 1)
			g := &f.app.continuationAdmission
			p := NewInboundContinuationProcessor(f.app, time.Second)
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			trigger := cancel
			if strings.HasSuffix(boundary, "stop") {
				trigger = p.Stop
			}
			before := loadInboundContinuationJob(t, &App{DB: f.shared}, f.works[0].OrganizationID, f.works[0].Persisted.ID)
			var held []func()
			queue := strings.HasPrefix(boundary, "queue")
			if queue {
				for range 8 {
					held = append(held, h1OrgHold(t, g, 1, inboundContinuationFreshPriority, func() int { return 10 }, "", inboundContinuationUnscoped))
				}
			}
			var queries atomic.Int32
			h1FastHintObserve(t, f.app, false, func(*gorm.DB) {
				queries.Add(1)
				if !queue {
					trigger()
				}
			})
			done := h1FastHintStart(t, f, p, ctx, cancel)
			if queue {
				h1CapacityWaitQueue(t, g, 8, 1)
				trigger()
			}
			select {
			case err := <-done:
				if strings.HasSuffix(boundary, "stop") {
					require.ErrorIs(t, err, errInboundContinuationAdmissionStopped)
				} else {
					require.ErrorIs(t, err, context.Canceled)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("cancelled optional probe did not settle")
			}
			if queue {
				require.Zero(t, queries.Load())
			} else {
				require.EqualValues(t, 1, queries.Load())
			}
			require.Equal(t, before, loadInboundContinuationJob(t, &App{DB: f.shared}, f.works[0].OrganizationID, f.works[0].Persisted.ID))
			require.Zero(t, f.model.calls.Load())
			require.Eventually(t, func() bool { return f.pool.Stats().InUse == 0 }, time.Second, time.Millisecond,
				"cancelled probe must finish releasing its SQL transaction")
			for _, release := range held {
				release()
			}
			h1OrgEmpty(t, g)
		})
	}
}

func TestH1FastPreparationHint_TinyLimitsAndQueuedResize(t *testing.T) {
	for _, scenario := range []string{"steady-one", "steady-two", "shrink-after-query", "shrink-while-writer-queued"} {
		t.Run(scenario, func(t *testing.T) {
			f := h1CapacityLegacy(t, 1)
			p := NewInboundContinuationProcessor(f.app, time.Second)
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			t.Cleanup(cancel)
			t.Cleanup(func() { f.pool.SetMaxOpenConns(10) })
			var queries atomic.Int32
			h1FastHintObserve(t, f.app, true, func(*gorm.DB) {
				queries.Add(1)
				if scenario == "shrink-after-query" {
					f.pool.SetMaxOpenConns(2)
				}
			})
			if scenario == "shrink-while-writer-queued" {
				stopGuard := h1ReaderPreparationHoldGuard(t, f)
				done := h1FastHintStart(t, f, p, ctx, cancel)
				t.Cleanup(func() { cancel(); stopGuard() })
				h1FastHintWriterQueued(t, f)
				before := loadInboundContinuationJob(t, &App{DB: f.shared}, f.works[0].OrganizationID, f.works[0].Persisted.ID)
				require.Equal(t, models.ScheduledJobStatusPending, before.Status)
				f.pool.SetMaxOpenConns(2)
				select {
				case err := <-done:
					require.Error(t, err)
					require.False(t, inboundContinuationRequiresManualReview(err))
				case <-time.After(5 * time.Second):
					t.Fatal("marked hinted Writer did not observe tiny resize")
				}
				require.Equal(t, before, loadInboundContinuationJob(t, &App{DB: f.shared}, f.works[0].OrganizationID, f.works[0].Persisted.ID))
				require.EqualValues(t, 1, queries.Load())
				require.Zero(t, f.model.calls.Load())
				stopGuard()
			} else {
				if scenario == "steady-one" {
					f.pool.SetMaxOpenConns(1)
				}
				if scenario == "steady-two" {
					f.pool.SetMaxOpenConns(2)
				}
				mode, err := p.hintInboundContinuationPreparationMode(ctx, f.works[0].OrganizationID, f.works[0].Persisted.ID)
				require.NoError(t, err)
				require.Equal(t, inboundContinuationReader, mode)
				if scenario == "shrink-after-query" {
					require.EqualValues(t, 1, queries.Load())
				} else {
					require.Zero(t, queries.Load(), "steady tiny pool takes the original Reader route without a probe")
				}
			}
			h1OrgEmpty(t, &f.app.continuationAdmission)
			require.Zero(t, f.pool.Stats().InUse)
		})
	}
}

func TestH1FastPreparationHint_CustomNonRLSAndRecursiveEntrySkipProbe(t *testing.T) {
	for _, scenario := range []string{"custom", "non-RLS", "recursive-context", "nested-execution"} {
		t.Run(scenario, func(t *testing.T) {
			f := h1CapacityLegacy(t, 1)
			p := NewInboundContinuationProcessor(f.app, time.Second)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			var queries atomic.Int32
			h1FastHintObserve(t, f.app, false, func(*gorm.DB) { queries.Add(1) })
			before := loadInboundContinuationJob(t, &App{DB: f.shared}, f.works[0].OrganizationID, f.works[0].Persisted.ID)
			switch scenario {
			case "custom":
				var called atomic.Int32
				p.process = func(context.Context, *App, *persistedIncomingMessage) error { called.Add(1); return nil }
				require.NoError(t, p.ProcessMessage(ctx, f.works[0].OrganizationID, f.works[0].Persisted.ID))
				require.EqualValues(t, 1, called.Load())
			case "non-RLS":
				f.app.Config.Database.RLSEnabled = false
				mode, err := p.hintInboundContinuationPreparationMode(ctx, f.works[0].OrganizationID, f.works[0].Persisted.ID)
				require.NoError(t, err)
				require.Equal(t, inboundContinuationReader, mode)
			case "recursive-context":
				err := p.ProcessMessage(f.app.markInboundContinuationContext(ctx), f.works[0].OrganizationID, f.works[0].Persisted.ID)
				require.ErrorContains(t, err, "recursive inbound continuation")
			case "nested-execution":
				x := f.app.scopedApp(f.app.DB, f.works[0].OrganizationID)
				x.inboundContinuation = &inboundContinuationExecution{OrganizationID: f.works[0].OrganizationID}
				err := NewInboundContinuationProcessor(x, time.Second).ProcessMessage(ctx, f.works[0].OrganizationID, f.works[0].Persisted.ID)
				require.ErrorContains(t, err, "nested inbound continuation")
			}
			require.Zero(t, queries.Load())
			if scenario != "custom" {
				require.Equal(t, before, loadInboundContinuationJob(t, &App{DB: f.shared}, f.works[0].OrganizationID, f.works[0].Persisted.ID))
			}
			h1OrgEmpty(t, &f.app.continuationAdmission)
			require.Zero(t, f.pool.Stats().InUse)
		})
	}
}
