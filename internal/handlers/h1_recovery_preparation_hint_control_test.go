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

func TestH1RecoveryPreparationHint_EligibilityUsesOriginalOrderedCandidate(t *testing.T) {
	for _, state := range []string{"future", "completed", "fresh-lease", "lease-equality", "stale-lease", "null-lease", "exhausted", "deleted-job"} {
		t.Run(state, func(t *testing.T) {
			f, jobs := h1RecoveryHintPair(t)
			require.NoError(t, f.shared.Delete(f.sessions[1]).Error)
			p := NewInboundContinuationProcessor(f.app, time.Second)
			now := time.Now().UTC().Truncate(time.Microsecond).Add(time.Second)
			p.now = func() time.Time { return now }
			updates := map[string]interface{}{}
			want := inboundContinuationReader
			switch state {
			case "future":
				updates["run_at"] = now.Add(time.Hour)
			case "completed":
				updates["status"] = models.ScheduledJobStatusCompleted
			case "fresh-lease", "lease-equality", "stale-lease", "null-lease":
				updates["status"] = models.ScheduledJobStatusProcessing
				updates["locked_at"] = now
				if state == "lease-equality" {
					updates["locked_at"] = now.Add(-p.lease)
				}
				if state == "stale-lease" {
					updates["locked_at"], want = now.Add(-p.lease-time.Second), inboundContinuationWriter
				}
				if state == "null-lease" {
					updates["locked_at"], want = nil, inboundContinuationWriter
				}
			case "exhausted":
				updates["attempts"] = jobs[0].MaxAttempts
			case "deleted-job":
				require.NoError(t, f.shared.Delete(&jobs[0]).Error)
			}
			if len(updates) > 0 {
				require.NoError(t, f.shared.Model(&models.ScheduledJob{}).Where("id = ?", jobs[0].ID).Updates(updates).Error)
			}
			var before models.ScheduledJob
			require.NoError(t, f.shared.Unscoped().Where("id = ?", jobs[0].ID).First(&before).Error)
			var queries atomic.Int32
			h1RecoveryHintObserve(t, f.app, func(*gorm.DB) { queries.Add(1) })
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			mode, err := p.hintInboundRecoveryPreparationMode(ctx, f.works[0].OrganizationID)
			require.NoError(t, err)
			require.Equal(t, want, mode, "only the original eligibility predicates can move selection from existing A to new B")
			require.EqualValues(t, 1, queries.Load())
			var after models.ScheduledJob
			require.NoError(t, f.shared.Unscoped().Where("id = ?", jobs[0].ID).First(&after).Error)
			require.Equal(t, before, after)
			h1OrgEmpty(t, &f.app.continuationAdmission)
		})
	}
}

func TestH1RecoveryPreparationHint_UnknownDoesNotSkipExhaustedTerminalization(t *testing.T) {
	f := h1CapacityLegacy(t, 1)
	h1RecoveryOnlyFixtureOrganization(t, f)
	work := f.works[0]
	job := loadInboundContinuationJob(t, &App{DB: f.shared}, work.OrganizationID, work.Persisted.ID)
	require.NoError(t, f.shared.Model(&models.ScheduledJob{}).Where("id = ?", job.ID).Update("attempts", job.MaxAttempts).Error)
	g := &f.app.continuationAdmission
	release := h1OrgHold(t, g, 1, inboundContinuationPerformPriority, func() int { return 10 }, work.OrganizationID.String(), inboundContinuationReader)
	var queries atomic.Int32
	h1RecoveryHintObserve(t, f.app, func(*gorm.DB) { queries.Add(1) })
	p := NewInboundContinuationProcessor(f.app, time.Second)
	p.batchSize = 1
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	done := h1RecoveryHintRun(t, f, p, ctx, cancel)
	t.Cleanup(func() { cancel(); release() })
	h1RecoveryHintQueued(t, f, 1, inboundContinuationWriter, inboundContinuationFreshPriority)
	require.EqualValues(t, 1, queries.Load())
	require.Zero(t, f.pool.Stats().InUse)
	before := loadInboundContinuationJob(t, &App{DB: f.shared}, work.OrganizationID, work.Persisted.ID)
	require.Equal(t, models.ScheduledJobStatusPending, before.Status)
	release()
	h1CapacityJoin(t, done, 1)
	after := loadInboundContinuationJob(t, &App{DB: f.shared}, work.OrganizationID, work.Persisted.ID)
	require.Equal(t, models.ScheduledJobStatusFailed, after.Status)
	require.Equal(t, "maximum attempts exhausted", after.LastError)
	require.Equal(t, before.Attempts, after.Attempts)
	require.Greater(t, after.Version, before.Version)
	require.Zero(t, f.model.calls.Load())
	h1OrgEmpty(t, g)
}

func TestH1RecoveryPreparationHint_SQLFailureUsesOriginalFreshWriter(t *testing.T) {
	f := h1CapacityLegacy(t, 1)
	h1RecoveryOnlyFixtureOrganization(t, f)
	h1ReaderPreparationRemoveSeed(t, f)
	queries := h1RecoveryHintFailSQL(t, f.app)
	work := f.works[0]
	g := &f.app.continuationAdmission
	release := h1OrgHold(t, g, 1, inboundContinuationPerformPriority, func() int { return 10 }, work.OrganizationID.String(), inboundContinuationReader)
	p := NewInboundContinuationProcessor(f.app, time.Second)
	p.batchSize = 1
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	done := h1RecoveryHintRun(t, f, p, ctx, cancel)
	t.Cleanup(func() { cancel(); release() })
	h1RecoveryHintQueued(t, f, 1, inboundContinuationWriter, inboundContinuationFreshPriority)
	require.EqualValues(t, 1, queries.Load(), "the optional SQL transaction must really abort")
	require.Zero(t, f.pool.Stats().InUse, "failed transaction closes before actual Writer wait")
	job := loadInboundContinuationJob(t, &App{DB: f.shared}, work.OrganizationID, work.Persisted.ID)
	require.Equal(t, models.ScheduledJobStatusPending, job.Status)
	require.Zero(t, job.Attempts)
	release()
	f.model.unblock()
	h1CapacityJoin(t, done, 1)
	h1ReaderPreparationLoadSession(t, f)
	f.assertGenerated(t, 0)
	h1OrgEmpty(t, g)
}

func TestH1RecoveryPreparationHint_OptionalQueueDeadlineKeepsOriginalContext(t *testing.T) {
	f := h1CapacityLegacy(t, 1)
	h1RecoveryOnlyFixtureOrganization(t, f)
	g := &f.app.continuationAdmission
	var held []func()
	for range 8 {
		held = append(held, h1OrgHold(t, g, 1, inboundContinuationFreshPriority, func() int { return 10 }, "", inboundContinuationUnscoped))
	}
	var queries atomic.Int32
	h1RecoveryHintObserve(t, f.app, func(*gorm.DB) { queries.Add(1) })
	p := NewInboundContinuationProcessor(f.app, time.Second)
	p.batchSize = 1
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
	done := h1RecoveryHintRun(t, f, p, ctx, cancel)
	t.Cleanup(func() {
		cancel()
		for _, release := range held {
			release()
		}
	})
	h1CapacityWaitQueue(t, g, 8, 1)
	g.mu.Lock()
	initialOrg, initialMode, initialPriority := g.waiters[0].organization, g.waiters[0].mode, g.waiters[0].priority
	g.mu.Unlock()
	require.Empty(t, initialOrg)
	require.Equal(t, inboundContinuationUnscoped, initialMode)
	require.Equal(t, inboundContinuationFreshPriority, initialPriority)
	require.Eventually(t, func() bool {
		g.mu.Lock()
		defer g.mu.Unlock()
		return g.used == 8 && len(g.waiters) == 1 && g.waiters[0].organization == f.works[0].OrganizationID.String() &&
			g.waiters[0].mode == inboundContinuationWriter && g.waiters[0].priority == inboundContinuationFreshPriority
	}, 25*time.Second, 10*time.Millisecond, "expired optional discovery must fall through to the original live Writer admission")
	require.NoError(t, ctx.Err())
	require.Zero(t, queries.Load())
	require.Zero(t, f.pool.Stats().InUse)
	job := loadInboundContinuationJob(t, &App{DB: f.shared}, f.works[0].OrganizationID, f.works[0].Persisted.ID)
	require.Equal(t, models.ScheduledJobStatusPending, job.Status)
	require.Zero(t, job.Attempts)
	for _, release := range held {
		release()
	}
	f.model.unblock()
	h1CapacityJoin(t, done, 1)
	f.assertGenerated(t, 0)
	require.Zero(t, queries.Load())
	h1OrgEmpty(t, g)
}

func TestH1RecoveryPreparationHint_OptionalSQLDeadlineKeepsOriginalContext(t *testing.T) {
	f := h1CapacityLegacy(t, 1)
	h1RecoveryOnlyFixtureOrganization(t, f)
	h1ReaderPreparationRemoveSeed(t, f)
	work := f.works[0]
	g := &f.app.continuationAdmission
	release := h1OrgHold(t, g, 1, inboundContinuationPerformPriority, func() int { return 10 }, work.OrganizationID.String(), inboundContinuationReader)
	var queries atomic.Int32
	h1RecoveryHintObserve(t, f.app, func(tx *gorm.DB) {
		queries.Add(1)
		// Only optional discovery sleeps. Its actual 20-second child must
		// cancel this 30-second server statement while the caller stays live.
		tx.Statement.SQL.Reset()
		tx.Statement.SQL.WriteString("SELECT false FROM pg_sleep(30)")
		tx.Statement.Vars = nil
	})
	p := NewInboundContinuationProcessor(f.app, time.Second)
	p.batchSize = 1
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
	done := h1RecoveryHintRun(t, f, p, ctx, cancel)
	t.Cleanup(func() { cancel(); release() })
	require.Eventually(t, func() bool {
		g.mu.Lock()
		defer g.mu.Unlock()
		return g.used == 1 && len(g.waiters) == 1 && g.waiters[0].organization == work.OrganizationID.String() &&
			g.waiters[0].mode == inboundContinuationWriter && g.waiters[0].priority == inboundContinuationFreshPriority
	}, 25*time.Second, 10*time.Millisecond, "the real SQL child deadline must yield to original live Writer admission before 30-second SQL can finish")
	require.NoError(t, ctx.Err())
	require.EqualValues(t, 1, queries.Load())
	require.Eventually(t, func() bool { return f.pool.Stats().InUse == 0 }, time.Second, time.Millisecond,
		"the aborted optional transaction must close before authoritative admission can proceed")
	job := loadInboundContinuationJob(t, &App{DB: f.shared}, work.OrganizationID, work.Persisted.ID)
	require.Equal(t, models.ScheduledJobStatusPending, job.Status)
	require.Zero(t, job.Attempts)
	release()
	f.model.unblock()
	h1CapacityJoin(t, done, 1)
	h1ReaderPreparationLoadSession(t, f)
	f.assertGenerated(t, 0)
	h1OrgEmpty(t, g)
}

func TestH1RecoveryPreparationHint_ParentCancellationAndBoundedStopPrecedeClaim(t *testing.T) {
	for _, boundary := range []string{"queue-cancel", "queue-stop", "sql-cancel", "sql-stop"} {
		t.Run(boundary, func(t *testing.T) {
			f := h1CapacityLegacy(t, 1)
			h1RecoveryOnlyFixtureOrganization(t, f)
			g := &f.app.continuationAdmission
			p := NewInboundContinuationProcessor(f.app, time.Second)
			p.batchSize = 1
			ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
			trigger := cancel
			if strings.HasSuffix(boundary, "stop") {
				trigger = p.Stop
			}
			before := loadInboundContinuationJob(t, &App{DB: f.shared}, f.works[0].OrganizationID, f.works[0].Persisted.ID)
			queue := strings.HasPrefix(boundary, "queue")
			var sqlReturned atomic.Bool
			returnedCallback := "h1:recovery-hint-stop-sql-return:" + uuid.NewString()
			require.NoError(t, f.app.DB.Callback().Row().After("gorm:row").Register(returnedCallback, func(tx *gorm.DB) {
				if tx.Statement.SQL.String() == "SELECT false FROM pg_sleep(0.05)" && tx.Error == nil {
					sqlReturned.Store(true)
				}
			}))
			t.Cleanup(func() { _ = f.app.DB.Callback().Row().Remove(returnedCallback) })
			var held []func()
			if queue {
				for range 8 {
					held = append(held, h1OrgHold(t, g, 1, inboundContinuationFreshPriority, func() int { return 10 }, "", inboundContinuationUnscoped))
				}
			}
			var queries atomic.Int32
			h1RecoveryHintObserve(t, f.app, func(tx *gorm.DB) {
				queries.Add(1)
				if !queue {
					// Trigger at the Row callback before statement dispatch, with
					// the tenant transaction active. This is not evidence of
					// interrupting an already-executing statement. Finite SQL still
					// returns after Stop; the original check follows its unwind.
					tx.Statement.SQL.Reset()
					tx.Statement.SQL.WriteString("SELECT false FROM pg_sleep(0.05)")
					tx.Statement.Vars = nil
					trigger()
				}
			})
			done := h1RecoveryHintRun(t, f, p, ctx, cancel)
			t.Cleanup(func() {
				cancel()
				for _, release := range held {
					release()
				}
			})
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
				t.Fatal("cancelled or stopped recovery probe did not unwind")
			}
			if queue {
				require.Zero(t, queries.Load())
			} else {
				require.EqualValues(t, 1, queries.Load())
			}
			if boundary == "sql-stop" {
				require.True(t, sqlReturned.Load(), "Stop is checked after the real finite optional statement returns")
			}
			require.Equal(t, before, loadInboundContinuationJob(t, &App{DB: f.shared}, f.works[0].OrganizationID, f.works[0].Persisted.ID))
			require.Zero(t, f.model.calls.Load())
			for _, release := range held {
				release()
			}
			h1OrgEmpty(t, g)
			require.Eventually(t, func() bool { return f.pool.Stats().InUse == 0 }, time.Second, time.Millisecond)
		})
	}
}

func TestH1RecoveryPreparationHint_TinyAndCompatibilityRoutesSkipDiscovery(t *testing.T) {
	for _, route := range []string{"tiny-one", "tiny-two", "custom", "non-RLS"} {
		t.Run(route, func(t *testing.T) {
			f := h1CapacityLegacy(t, 1)
			h1RecoveryOnlyFixtureOrganization(t, f)
			p := NewInboundContinuationProcessor(f.app, time.Second)
			p.batchSize = 1
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			t.Cleanup(func() { f.pool.SetMaxOpenConns(10) })
			var queries atomic.Int32
			h1RecoveryHintObserve(t, f.app, func(*gorm.DB) { queries.Add(1) })
			if route == "tiny-one" {
				f.pool.SetMaxOpenConns(1)
			}
			if route == "tiny-two" {
				f.pool.SetMaxOpenConns(2)
			}
			if route == "non-RLS" {
				f.app.Config.Database.RLSEnabled = false
			}
			var calls atomic.Int32
			if route == "custom" {
				p.process = func(_ context.Context, _ *App, work *persistedIncomingMessage) error {
					require.Equal(t, f.works[0].Persisted.ID, work.Persisted.ID)
					calls.Add(1)
					return nil
				}
			}
			mode, err := p.hintInboundRecoveryPreparationMode(ctx, f.works[0].OrganizationID)
			require.NoError(t, err)
			require.Equal(t, inboundContinuationWriter, mode)
			if route == "non-RLS" {
				release, actual, err := p.acquirePreparationCapacity(ctx, f.works[0].OrganizationID, mode)
				require.NoError(t, err)
				require.Equal(t, inboundContinuationUnscoped, actual)
				release()
			} else if route == "custom" {
				require.NoError(t, p.RunOnce(ctx))
				require.EqualValues(t, 1, calls.Load())
			} else {
				initialMode := h1RecoveryHintInitialSessionMode(t, f)
				err = p.RunOnce(ctx)
				require.ErrorContains(t, err, "pool limit")
				select {
				case got := <-initialMode:
					require.Equal(t, inboundContinuationWriter, got)
				default:
					t.Fatal("tiny Fresh Writer must claim and prepare before ordinary Perform refuses")
				}
				job := loadInboundContinuationJob(t, &App{DB: f.shared}, f.works[0].OrganizationID, f.works[0].Persisted.ID)
				require.Equal(t, 1, job.Attempts)
				require.Equal(t, models.ScheduledJobStatusPending, job.Status)
				require.Zero(t, f.model.calls.Load())
			}
			require.Zero(t, queries.Load(), "compatibility and steady tiny routes must not execute optional SQL")
			h1OrgEmpty(t, &f.app.continuationAdmission)
			require.Eventually(t, func() bool { return f.pool.Stats().InUse == 0 }, time.Second, time.Millisecond)
		})
	}
}
