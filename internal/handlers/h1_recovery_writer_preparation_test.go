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
	"gorm.io/gorm/clause"
)

// The package shares one fixture database. Restrict only this private pool's
// real organization-ID enumeration, so batchSize=1 cannot select an older test's
// pending job. Claims, session SQL and public RunOnce still execute normally.
func h1RecoveryOnlyFixtureOrganization(t *testing.T, f *h1CapacityLegacyFixture) {
	t.Helper()
	organizationID := f.works[0].OrganizationID
	callback := "h1:recovery-fixture-organizations:" + uuid.NewString()
	var constrained atomic.Int32
	require.NoError(t, f.app.DB.Callback().Query().Before("gorm:query").Register(callback, func(tx *gorm.DB) {
		statement := tx.Statement
		if statement == nil || statement.Schema == nil || statement.Schema.Table != "organizations" {
			return
		}
		ids, ok := statement.Dest.(*[]uuid.UUID)
		if !ok || ids == nil {
			return
		}
		selection, ok := statement.Clauses["SELECT"].Expression.(clause.Select)
		if !ok || len(selection.Columns) != 1 || selection.Columns[0].Name != "id" || selection.Columns[0].Raw {
			return
		}
		statement.AddClause(clause.Where{Exprs: []clause.Expression{clause.Eq{
			Column: clause.Column{Table: clause.CurrentTable, Name: "id"}, Value: organizationID,
		}}})
		constrained.Add(1)
	}))
	t.Cleanup(func() { _ = f.app.DB.Callback().Query().Remove(callback) })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	organizations, err := NewInboundContinuationProcessor(f.app, time.Second).listOrganizations(ctx)
	require.NoError(t, err)
	require.Equal(t, []uuid.UUID{organizationID}, organizations)
	require.EqualValues(t, 1, constrained.Load(), "the actual organization-ID query must be constrained")
}

func TestH1RecoveryPreparation_InitialWriterYieldsBeforeModelAndRenewsClaim(t *testing.T) {
	for _, route := range []string{"new-session", "existing-session"} {
		t.Run(route, func(t *testing.T) {
			f := h1CapacityLegacy(t, 1)
			h1RecoveryOnlyFixtureOrganization(t, f)
			var hintFailures *atomic.Int32
			if route == "new-session" {
				h1ReaderPreparationRemoveSeed(t, f)
				// Keep this original Writer ownership/heartbeat witness on the
				// conservative route by failing only its optional recovery query.
				hintFailures = h1RecoveryHintFailSQL(t, f.app)
			}
			work := f.works[0]
			g := &f.app.continuationAdmission
			var fillers []func()
			for range 6 {
				fillers = append(fillers, h1OrgHold(t, g, 1, inboundContinuationFreshPriority,
					func() int { return 10 }, "", inboundContinuationUnscoped))
			}
			type ownership struct{ mode, active, used int }
			entered, finish := make(chan ownership, 1), make(chan struct{})
			var once, closeOnce sync.Once
			unblock := func() { closeOnce.Do(func() { close(finish) }) }
			callback := "h1:recovery-writer-session:" + uuid.NewString()
			observe := func(tx *gorm.DB) {
				if tx.Statement.Schema == nil || tx.Statement.Schema.Table != "chatbot_sessions" || tx.Error != nil {
					return
				}
				once.Do(func() {
					g.mu.Lock()
					state := ownership{used: g.used}
					if phase := g.phases[work.OrganizationID.String()]; phase != nil {
						state.mode, state.active = phase.activeMode, phase.active
					}
					g.mu.Unlock()
					entered <- state
					select {
					case <-finish:
					case <-tx.Statement.Context.Done():
					}
				})
			}
			require.NoError(t, f.app.DB.Callback().Create().After("gorm:create").Register(callback, observe))
			require.NoError(t, f.app.DB.Callback().Update().After("gorm:update").Register(callback, observe))
			t.Cleanup(func() {
				_ = f.app.DB.Callback().Create().Remove(callback)
				_ = f.app.DB.Callback().Update().Remove(callback)
			})
			p := NewInboundContinuationProcessor(f.app, time.Second)
			p.batchSize = 1
			p.lease = 600 * time.Millisecond
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			done, joined := make(chan error, 1), make(chan struct{})
			t.Cleanup(func() {
				unblock()
				cancel()
				p.Stop()
				f.model.unblock()
				for _, release := range fillers {
					release()
				}
				select {
				case <-joined:
				case <-time.After(15 * time.Second):
					t.Error("recovery preparation worker did not join")
				}
			})
			go func() { defer close(joined); done <- p.RunOnce(ctx) }()
			select {
			case state := <-entered:
				require.Equal(t, ownership{mode: inboundContinuationWriter, active: 1, used: 7}, state,
					"both session routes must use the writer already acquired before the durable claim")
			case <-time.After(5 * time.Second):
				t.Fatal("recovery did not reach session preparation without reacquiring its own writer")
			}
			if hintFailures != nil {
				require.EqualValues(t, 1, hintFailures.Load(), "the optional recovery SQL failure must actually select original Writer preparation")
			}
			apply := h1OrgAcquireAsync(t, g, context.Background(), nil, 2, inboundContinuationApplyPriority,
				func() int { return 10 }, work.OrganizationID.String(), inboundContinuationReader)
			h1CapacityWaitQueue(t, g, 7, 1)
			unblock()
			releaseApply := h1OrgGrant(t, apply)
			h1CapacityWaitQueue(t, g, 8, 1)
			g.mu.Lock()
			queuedOrg, queuedMode, queuedPriority := g.waiters[0].organization, g.waiters[0].mode, g.waiters[0].priority
			g.mu.Unlock()
			require.Equal(t, work.OrganizationID.String(), queuedOrg)
			require.Equal(t, inboundContinuationReader, queuedMode)
			require.Equal(t, inboundContinuationPerformPriority, queuedPriority)
			require.Zero(t, f.model.calls.Load(), "accepted apply takes the released writer credit before model admission")
			require.Eventually(t, func() bool { return f.pool.Stats().InUse == 0 }, time.Second, time.Millisecond,
				"writer SQL must be fully released before the ordinary perform wait")
			before := loadInboundContinuationJob(t, &App{DB: f.shared}, work.OrganizationID, work.Persisted.ID)
			require.Equal(t, models.ScheduledJobStatusProcessing, before.Status)
			require.NotNil(t, before.LockedAt)
			require.Eventually(t, func() bool {
				now := loadInboundContinuationJob(t, &App{DB: f.shared}, work.OrganizationID, work.Persisted.ID)
				return now.LockedAt != nil && now.LockedAt.After(before.LockedAt.Add(650*time.Millisecond))
			}, 3*time.Second, 20*time.Millisecond, "recovery retains its real lease during the credit-free perform wait")
			competitor := NewInboundContinuationProcessor(f.app, time.Second)
			competitor.lease = p.lease
			claimed, err := competitor.claim(ctx, work.OrganizationID, &work.Persisted.ID)
			require.NoError(t, err)
			require.Nil(t, claimed)
			releaseApply()
			select {
			case request := <-f.model.entered:
				require.Equal(t, f.texts[0], request.Current)
			case <-time.After(5 * time.Second):
				t.Fatal("recovery model did not enter after apply released")
			}
			h1OrgAssertActive(t, f.app, work.OrganizationID, inboundContinuationReader, 1, 7)
			require.Eventually(t, func() bool { return f.pool.Stats().InUse == 1 }, time.Second, time.Millisecond,
				"only the real model's physical guard remains between short heartbeat statements")
			for _, release := range fillers {
				release()
			}
			f.model.unblock()
			h1CapacityJoin(t, done, 1)
			if route == "new-session" {
				h1ReaderPreparationLoadSession(t, f)
			}
			f.assertGenerated(t, 0)
			require.NoError(t, p.RunOnce(ctx))
			f.assertGenerated(t, 0)
			require.EqualValues(t, 1, f.model.calls.Load())
			f.mu.Lock()
			sends := len(f.sent)
			f.mu.Unlock()
			require.Equal(t, 1, sends, "recovery replay does not repeat an accepted provider result")
			h1OrgEmpty(t, g)
		})
	}
}

func TestH1RecoveryPreparation_InitialWriterUnwindsOnPreparationFailure(t *testing.T) {
	for _, fault := range []string{"error", "panic", "stop", "cancel"} {
		t.Run(fault, func(t *testing.T) {
			f := h1CapacityLegacy(t, 1)
			h1RecoveryOnlyFixtureOrganization(t, f)
			p := NewInboundContinuationProcessor(f.app, time.Second)
			p.batchSize = 1
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			t.Cleanup(cancel)
			var injected atomic.Bool
			callback := "h1:recovery-writer-unwind:" + uuid.NewString()
			const sentinel = "synthetic initial recovery writer failure"
			require.NoError(t, f.app.DB.Callback().Update().Before("gorm:update").Register(callback, func(tx *gorm.DB) {
				if tx.Statement.Schema == nil || tx.Statement.Schema.Table != "chatbot_sessions" {
					return
				}
				g := &f.app.continuationAdmission
				g.mu.Lock()
				phase := g.phases[f.works[0].OrganizationID.String()]
				writer := phase != nil && phase.activeMode == inboundContinuationWriter && phase.active == 1 && g.used == 1
				g.mu.Unlock()
				if !writer || !injected.CompareAndSwap(false, true) {
					return
				}
				switch fault {
				case "error":
					_ = tx.AddError(errors.New(sentinel))
				case "panic":
					panic(sentinel)
				case "stop":
					p.Stop()
				case "cancel":
					cancel()
				}
			}))
			t.Cleanup(func() { _ = f.app.DB.Callback().Update().Remove(callback) })
			err := p.RunOnce(ctx)
			require.True(t, injected.Load(), "the fault must occur while the actual initial writer is owned")
			switch fault {
			case "error", "panic":
				require.ErrorContains(t, err, sentinel)
			case "stop":
				require.ErrorIs(t, err, errInboundContinuationAdmissionStopped)
			case "cancel":
				require.ErrorIs(t, err, context.Canceled)
			}
			require.Zero(t, f.model.calls.Load())
			h1OrgEmpty(t, &f.app.continuationAdmission)
			require.Eventually(t, func() bool { return f.pool.Stats().InUse == 0 }, time.Second, time.Millisecond)
			work := f.works[0]
			job := loadInboundContinuationJob(t, &App{DB: f.shared}, work.OrganizationID, work.Persisted.ID)
			require.Equal(t, models.ScheduledJobStatusPending, job.Status)
			require.Empty(t, job.LockedBy)
			require.Nil(t, job.LockedAt)
			var actions int64
			require.NoError(t, f.shared.Model(&models.ScheduledJob{}).Where("organization_id = ? AND kind = ? AND aggregate_id = ?",
				work.OrganizationID, inboundContinuationActionJobKind, work.Persisted.ID).Count(&actions).Error)
			require.Zero(t, actions, "failed preparation cannot start a durable external action")
		})
	}
}
