package handlers

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func h1RecoveryHintLockJob(t *testing.T, f *h1CapacityLegacyFixture, job models.ScheduledJob) func() {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
	t.Cleanup(cancel)
	tx := f.shared.WithContext(ctx).Begin()
	require.NoError(t, tx.Error)
	var once sync.Once
	unblock := func() {
		once.Do(func() {
			if err := tx.Rollback().Error; err != nil {
				t.Errorf("release independent job lock: %v", err)
			}
			cancel()
		})
	}
	t.Cleanup(unblock)
	var locked models.ScheduledJob
	require.NoError(t, tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ?", job.ID).First(&locked).Error)
	require.Equal(t, job, locked)
	return unblock
}

func h1RecoveryHintInitialSessionMode(t *testing.T, f *h1CapacityLegacyFixture) <-chan int {
	t.Helper()
	observed := make(chan int, 1)
	var once sync.Once
	name := "h1:recovery-hint-initial-mode:" + uuid.NewString()
	require.NoError(t, f.app.DB.Callback().Query().Before("gorm:query").Register(name, func(tx *gorm.DB) {
		if tx.Statement.Schema == nil || tx.Statement.Schema.Table != "chatbot_sessions" {
			return
		}
		once.Do(func() {
			g := &f.app.continuationAdmission
			g.mu.Lock()
			mode := inboundContinuationUnscoped
			if phase := g.phases[f.works[0].OrganizationID.String()]; phase != nil && phase.active == 1 {
				mode = phase.activeMode
			}
			g.mu.Unlock()
			observed <- mode
		})
	}))
	t.Cleanup(func() { _ = f.app.DB.Callback().Query().Remove(name) })
	return observed
}

func h1RecoveryHintQueued(t *testing.T, f *h1CapacityLegacyFixture, used, mode, priority int) {
	t.Helper()
	g := &f.app.continuationAdmission
	require.Eventually(t, func() bool {
		g.mu.Lock()
		defer g.mu.Unlock()
		return g.used == used && len(g.waiters) == 1 &&
			g.waiters[0].organization == f.works[0].OrganizationID.String() &&
			g.waiters[0].mode == mode && g.waiters[0].priority == priority && !g.waiters[0].intake
	}, 5*time.Second, time.Millisecond)
}

func TestH1RecoveryReader_LockedCandidateDoesNotConstrainActualClaim(t *testing.T) {
	for _, direction := range []string{"new-A-existing-B", "existing-A-new-B"} {
		t.Run(direction, func(t *testing.T) {
			f, jobs := h1RecoveryReaderPair(t)
			wantMode := inboundContinuationReader
			if direction == "new-A-existing-B" {
				h1ReaderPreparationRemoveSeed(t, f)
			} else {
				require.NoError(t, f.shared.Delete(f.sessions[1]).Error)
			}
			unlock := h1RecoveryHintLockJob(t, f, jobs[0])
			initialMode := h1RecoveryHintInitialSessionMode(t, f)
			p := NewInboundContinuationProcessor(f.app, time.Second)
			p.batchSize = 1
			ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
			done := h1RecoveryReaderRun(t, f, p, ctx, cancel)
			t.Cleanup(func() { cancel(); unlock() })
			select {
			case mode := <-initialMode:
				require.Equal(t, wantMode, mode, "ordinary recovery validates under Reader; real SKIP LOCKED selects B")
			case <-time.After(5 * time.Second):
				t.Fatal("SKIP LOCKED did not proceed to B while A remained locked")
			}
			select {
			case request := <-f.model.entered:
				require.Equal(t, f.texts[1], request.Current, "the actual claimed B controls model input")
			case <-time.After(5 * time.Second):
				t.Fatal("claimed B did not reach its actual guarded model")
			}
			require.Equal(t, jobs[0], loadInboundContinuationJob(t, &App{DB: f.shared}, f.works[0].OrganizationID, f.works[0].Persisted.ID))
			h1OrgAssertActive(t, f.app, f.works[0].OrganizationID, inboundContinuationReader, 1, 1)
			require.Equal(t, 1, f.pool.Stats().InUse, "only B's physical model guard remains on the private pool")
			f.model.unblock()
			h1CapacityJoin(t, done, 1)
			h1RecoveryReaderLoadSession(t, f, 1)
			f.assertGenerated(t, 1)
			unlock()
			require.NoError(t, p.RunOnce(ctx))
			h1RecoveryReaderLoadSession(t, f, 0)
			f.assertGenerated(t, 0)
			require.NoError(t, p.RunOnce(ctx))
			require.EqualValues(t, 2, f.model.calls.Load(), "both actual claims resolve once; replay does not recompute")
			h1OrgEmpty(t, &f.app.continuationAdmission)
		})
	}
}

func TestH1RecoveryReader_QueuedShrinkRefusesReplacementBeforeMutation(t *testing.T) {
	f, jobs := h1RecoveryReaderPair(t)
	h1ReaderPreparationRemoveSeed(t, f)
	unlock := h1RecoveryHintLockJob(t, f, jobs[0])
	var before models.ChatbotSession
	require.NoError(t, f.shared.Where("id = ?", f.sessions[1].ID).First(&before).Error)
	g := &f.app.continuationAdmission
	release := h1OrgHold(t, g, 1, inboundContinuationFreshPriority,
		func() int { return f.pool.Stats().MaxOpenConnections }, f.works[0].OrganizationID.String(), inboundContinuationWriter)
	initialMode := h1RecoveryHintInitialSessionMode(t, f)
	p := NewInboundContinuationProcessor(f.app, time.Second)
	p.batchSize = 1
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	done := h1RecoveryReaderRun(t, f, p, ctx, cancel)
	t.Cleanup(func() { cancel(); release(); unlock(); f.pool.SetMaxOpenConns(10) })
	h1RecoveryHintQueued(t, f, 1, inboundContinuationReader, inboundContinuationFreshPriority)
	require.Zero(t, f.pool.Stats().InUse, "initial Reader admission owns no SQL before its actual grant")
	f.pool.SetMaxOpenConns(2)
	release()
	select {
	case mode := <-initialMode:
		require.Equal(t, inboundContinuationReader, mode)
	case <-time.After(5 * time.Second):
		t.Fatal("Fresh Reader did not claim B at the shrunken limit")
	}
	select {
	case err := <-done:
		require.ErrorContains(t, err, "pool limit 2")
		require.False(t, inboundContinuationRequiresManualReview(err))
	case <-time.After(5 * time.Second):
		t.Fatal("marked replacement Writer did not refuse tiny pool")
	}
	var after models.ChatbotSession
	require.NoError(t, f.shared.Where("id = ?", before.ID).First(&after).Error)
	require.Equal(t, before, after, "no authoritative Writer update or session generation change before refusal")
	b := loadInboundContinuationJob(t, &App{DB: f.shared}, f.works[1].OrganizationID, f.works[1].Persisted.ID)
	require.Equal(t, models.ScheduledJobStatusPending, b.Status)
	require.Equal(t, 1, b.Attempts)
	require.Nil(t, b.LockedAt)
	require.Empty(t, b.LockedBy)
	require.Equal(t, jobs[0], loadInboundContinuationJob(t, &App{DB: f.shared}, f.works[0].OrganizationID, f.works[0].Persisted.ID))
	require.Zero(t, f.model.calls.Load())
	h1OrgEmpty(t, g)
	require.Eventually(t, func() bool { return f.pool.Stats().InUse == 0 }, time.Second, time.Millisecond)
}
