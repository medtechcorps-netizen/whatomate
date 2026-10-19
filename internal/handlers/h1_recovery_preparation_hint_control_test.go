package handlers

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/stretchr/testify/require"
)

// This is now a public authoritative-claim control, not a duplicated advisory
// eligibility query. Each subcase proves which real ordered job was selected.
func TestH1RecoveryReader_EligibilityUsesOriginalOrderedClaim(t *testing.T) {
	for _, state := range []string{"future", "completed", "fresh-lease", "lease-equality", "stale-lease", "null-lease", "exhausted", "deleted-job"} {
		t.Run(state, func(t *testing.T) {
			f, jobs := h1RecoveryReaderPair(t)
			require.NoError(t, f.shared.Delete(f.sessions[1]).Error)
			p := NewInboundContinuationProcessor(f.app, time.Second)
			p.batchSize = 1
			now := time.Now().UTC().Truncate(time.Microsecond).Add(time.Second)
			p.now = func() time.Time { return now }
			updates := map[string]interface{}{}
			want := 1
			switch state {
			case "future":
				updates["run_at"] = now.Add(time.Hour)
			case "completed":
				updates["status"] = models.ScheduledJobStatusCompleted
			case "fresh-lease", "lease-equality", "stale-lease", "null-lease":
				updates["status"], updates["locked_at"] = models.ScheduledJobStatusProcessing, now
				if state == "lease-equality" {
					updates["locked_at"] = now.Add(-p.lease)
				}
				if state == "stale-lease" {
					updates["locked_at"], want = now.Add(-p.lease-time.Second), 0
				}
				if state == "null-lease" {
					updates["locked_at"], want = nil, 0
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
			order := h1RecoveryReaderClaimOrder(t, f.app)
			f.model.unblock()
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			require.NoError(t, p.RunOnce(ctx))
			select {
			case id := <-order:
				require.Equal(t, jobs[want].ID, id)
			default:
				t.Fatal("no actual selected job")
			}
			select {
			case <-order:
				t.Fatal("batch1 claimed another job")
			default:
			}
			select {
			case request := <-f.model.entered:
				require.Equal(t, f.texts[want], request.Current)
			default:
				t.Fatal("selected job did not execute its actual model")
			}
			h1RecoveryReaderLoadSession(t, f, want)
			f.assertGenerated(t, want)
			require.EqualValues(t, 1, f.model.calls.Load())
			var after models.ScheduledJob
			require.NoError(t, f.shared.Unscoped().Where("id = ?", jobs[0].ID).First(&after).Error)
			if state == "exhausted" {
				require.Equal(t, models.ScheduledJobStatusFailed, after.Status)
				require.Equal(t, "maximum attempts exhausted", after.LastError)
				require.Equal(t, before.Attempts, after.Attempts)
				require.Greater(t, after.Version, before.Version)
			} else if want == 1 {
				require.Equal(t, before, after)
			}
			if want == 0 {
				require.Equal(t, jobs[1], loadInboundContinuationJob(t, &App{DB: f.shared}, f.works[1].OrganizationID, f.works[1].Persisted.ID))
			}
			h1OrgEmpty(t, &f.app.continuationAdmission)
		})
	}
}

func TestH1RecoveryReader_MalformedFirstIsClaimedAndValidatedBeforeLaterJob(t *testing.T) {
	for _, malformed := range []string{"missing-message", "foreign-message"} {
		t.Run(malformed, func(t *testing.T) {
			f, jobs := h1RecoveryReaderPair(t)
			aggregate := uuid.New()
			if malformed == "foreign-message" {
				foreign := h1CapacityLegacy(t, 1)
				aggregate = foreign.works[0].Persisted.ID
			}
			require.NoError(t, f.shared.Model(&models.ScheduledJob{}).Where("id = ?", jobs[0].ID).Update("aggregate_id", aggregate).Error)
			p := NewInboundContinuationProcessor(f.app, time.Second)
			p.batchSize = 1
			order := h1RecoveryReaderClaimOrder(t, f.app)
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			require.Error(t, p.RunOnce(ctx))
			select {
			case id := <-order:
				require.Equal(t, jobs[0].ID, id)
			default:
				t.Fatal("malformed first job was skipped instead of claimed")
			}
			var a models.ScheduledJob
			require.NoError(t, f.shared.Where("id = ?", jobs[0].ID).First(&a).Error)
			require.Equal(t, 1, a.Attempts)
			require.Equal(t, models.ScheduledJobStatusFailed, a.Status)
			require.Equal(t, true, a.Payload["manual_review_required"])
			require.Nil(t, a.LockedAt)
			require.Empty(t, a.LockedBy)
			require.Equal(t, jobs[1], loadInboundContinuationJob(t, &App{DB: f.shared}, f.works[1].OrganizationID, f.works[1].Persisted.ID))
			require.Zero(t, f.model.calls.Load())
			h1OrgEmpty(t, &f.app.continuationAdmission)
		})
	}
}

func TestH1RecoveryReader_NoEligibleJobStillTerminalizesExhaustedWork(t *testing.T) {
	f := h1CapacityLegacy(t, 1)
	h1RecoveryOnlyFixtureOrganization(t, f)
	work := f.works[0]
	job := loadInboundContinuationJob(t, &App{DB: f.shared}, work.OrganizationID, work.Persisted.ID)
	require.NoError(t, f.shared.Model(&models.ScheduledJob{}).Where("id = ?", job.ID).Update("attempts", job.MaxAttempts).Error)
	g := &f.app.continuationAdmission
	release := h1OrgHold(t, g, 1, inboundContinuationPerformPriority, func() int { return 10 }, work.OrganizationID.String(), inboundContinuationWriter)
	p := NewInboundContinuationProcessor(f.app, time.Second)
	p.batchSize = 1
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	done := h1RecoveryReaderRun(t, f, p, ctx, cancel)
	t.Cleanup(func() { cancel(); release() })
	h1RecoveryHintQueued(t, f, 1, inboundContinuationReader, inboundContinuationFreshPriority)
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

func TestH1RecoveryReader_InitialQueueKeepsOriginalContextAndOwnsNoSQL(t *testing.T) {
	f := h1CapacityLegacy(t, 1)
	h1RecoveryOnlyFixtureOrganization(t, f)
	g := &f.app.continuationAdmission
	var held []func()
	for range 8 {
		held = append(held, h1OrgHold(t, g, 1, inboundContinuationFreshPriority, func() int { return 10 }, "", inboundContinuationUnscoped))
	}
	p := NewInboundContinuationProcessor(f.app, time.Second)
	p.batchSize = 1
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	done := h1RecoveryReaderRun(t, f, p, ctx, cancel)
	t.Cleanup(func() {
		cancel()
		for _, release := range held {
			release()
		}
	})
	h1RecoveryHintQueued(t, f, 8, inboundContinuationReader, inboundContinuationFreshPriority)
	require.NoError(t, ctx.Err())
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
	h1OrgEmpty(t, g)
}

func TestH1RecoveryReader_ParentCancellationAndStopPrecedeClaim(t *testing.T) {
	for _, boundary := range []string{"cancel", "stop"} {
		t.Run(boundary, func(t *testing.T) {
			f := h1CapacityLegacy(t, 1)
			h1RecoveryOnlyFixtureOrganization(t, f)
			g := &f.app.continuationAdmission
			release := h1OrgHold(t, g, 1, inboundContinuationPerformPriority, func() int { return 10 }, f.works[0].OrganizationID.String(), inboundContinuationWriter)
			p := NewInboundContinuationProcessor(f.app, time.Second)
			p.batchSize = 1
			ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
			before := loadInboundContinuationJob(t, &App{DB: f.shared}, f.works[0].OrganizationID, f.works[0].Persisted.ID)
			done := h1RecoveryReaderRun(t, f, p, ctx, cancel)
			t.Cleanup(func() { cancel(); release() })
			h1RecoveryHintQueued(t, f, 1, inboundContinuationReader, inboundContinuationFreshPriority)
			if boundary == "stop" {
				p.Stop()
			} else {
				cancel()
			}
			select {
			case err := <-done:
				if boundary == "stop" {
					require.ErrorIs(t, err, errInboundContinuationAdmissionStopped)
				} else {
					require.ErrorIs(t, err, context.Canceled)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("initial Reader wait did not honor cancellation or Stop")
			}
			require.Equal(t, before, loadInboundContinuationJob(t, &App{DB: f.shared}, f.works[0].OrganizationID, f.works[0].Persisted.ID))
			require.Zero(t, f.model.calls.Load())
			release()
			h1OrgEmpty(t, g)
			require.Zero(t, f.pool.Stats().InUse)
		})
	}
}

func TestH1RecoveryReader_PoolAndCompatibilityPolicy(t *testing.T) {
	for _, maximum := range []int{0, 1, 2, 3, 4, 10, 20} {
		t.Run(fmt.Sprintf("pool-%d", maximum), func(t *testing.T) {
			f := h1CapacityLegacy(t, 1)
			h1RecoveryOnlyFixtureOrganization(t, f)
			f.pool.SetMaxOpenConns(maximum)
			t.Cleanup(func() { f.pool.SetMaxOpenConns(10) })
			p := NewInboundContinuationProcessor(f.app, time.Second)
			p.batchSize = 1
			want := inboundContinuationReader
			if maximum == 1 || maximum == 2 {
				want = inboundContinuationWriter
			}
			require.Equal(t, want, p.inboundRecoveryPreparationMode())
			initial := h1RecoveryHintInitialSessionMode(t, f)
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			f.model.unblock()
			err := p.RunOnce(ctx)
			if maximum == 1 || maximum == 2 {
				require.ErrorContains(t, err, "pool limit")
				job := loadInboundContinuationJob(t, &App{DB: f.shared}, f.works[0].OrganizationID, f.works[0].Persisted.ID)
				require.Equal(t, 1, job.Attempts)
				require.Equal(t, models.ScheduledJobStatusPending, job.Status)
				require.Zero(t, f.model.calls.Load())
			} else {
				require.NoError(t, err)
				f.assertGenerated(t, 0)
			}
			select {
			case actual := <-initial:
				require.Equal(t, want, actual)
			default:
				t.Fatal("missing actual first preparation lookup")
			}
			h1OrgEmpty(t, &f.app.continuationAdmission)
		})
	}
	for _, route := range []string{"custom", "non-RLS"} {
		t.Run(route, func(t *testing.T) {
			f := h1CapacityLegacy(t, 1)
			h1RecoveryOnlyFixtureOrganization(t, f)
			p := NewInboundContinuationProcessor(f.app, time.Second)
			p.batchSize = 1
			var calls atomic.Int32
			if route == "custom" {
				p.process = func(_ context.Context, _ *App, work *persistedIncomingMessage) error {
					require.Equal(t, f.works[0].Persisted.ID, work.Persisted.ID)
					calls.Add(1)
					return nil
				}
			} else {
				f.app.Config.Database.RLSEnabled = false
			}
			require.Equal(t, inboundContinuationWriter, p.inboundRecoveryPreparationMode())
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			release, actual, err := p.acquirePreparationCapacity(ctx, f.works[0].OrganizationID, p.inboundRecoveryPreparationMode())
			require.NoError(t, err)
			require.Equal(t, inboundContinuationUnscoped, actual)
			release()
			if route == "custom" {
				require.NoError(t, p.RunOnce(ctx))
				require.EqualValues(t, 1, calls.Load())
			}
			h1OrgEmpty(t, &f.app.continuationAdmission)
		})
	}
}

func TestH1RecoveryReader_FiftyClaimBudgetAndFinalJoinRemainBounded(t *testing.T) {
	f := h1CapacityLegacy(t, 1)
	h1RecoveryOnlyFixtureOrganization(t, f)
	org := f.works[0].OrganizationID
	first := loadInboundContinuationJob(t, &App{DB: f.shared}, org, f.works[0].Persisted.ID)
	ids := make([]uuid.UUID, 51)
	ids[0] = first.ID
	start := time.Now().UTC().Add(-2 * time.Hour)
	// Valid scheduled jobs with deliberately absent aggregate references fail in
	// actual post-claim validation. No model or custom processing shortcut is used.
	require.NoError(t, f.shared.Model(&models.ScheduledJob{}).Where("id = ?", first.ID).
		Updates(map[string]interface{}{"aggregate_id": nil, "max_attempts": 1, "run_at": start}).Error)
	for i := 1; i < len(ids); i++ {
		ids[i] = uuid.New()
		job := models.ScheduledJob{BaseModel: models.BaseModel{ID: ids[i]}, OrganizationID: org, Kind: inboundContinuationJobKind,
			RunAt: start.Add(time.Duration(i) * time.Second), Status: models.ScheduledJobStatusPending, MaxAttempts: 1,
			IdempotencyKey: "recovery-reader-budget:" + uuid.NewString(), Payload: models.JSONB{}, Version: 1}
		require.NoError(t, f.shared.Create(&job).Error)
	}
	p := NewInboundContinuationProcessor(f.app, time.Second)
	require.Equal(t, 50, p.batchSize)
	order := h1RecoveryReaderClaimOrder(t, f.app)
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	require.ErrorContains(t, p.RunOnce(ctx), "message reference is missing")
	for i, id := range ids {
		var job models.ScheduledJob
		require.NoError(t, f.shared.Where("id = ?", id).First(&job).Error)
		if i < 50 {
			select {
			case selected := <-order:
				require.Equal(t, id, selected)
			default:
				t.Fatal("missing actual bounded claim")
			}
			require.Equal(t, models.ScheduledJobStatusFailed, job.Status)
			require.Equal(t, 1, job.Attempts)
			require.Nil(t, job.LockedAt)
			require.Empty(t, job.LockedBy, "RunOnce joins each claimed worker before return")
		} else {
			require.Equal(t, models.ScheduledJobStatusPending, job.Status)
			require.Zero(t, job.Attempts)
		}
	}
	select {
	case <-order:
		t.Fatal("RunOnce exceeded fifty successful claims")
	default:
	}
	require.Zero(t, f.model.calls.Load())
	h1OrgEmpty(t, &f.app.continuationAdmission)
	require.Eventually(t, func() bool { return f.pool.Stats().InUse == 0 }, time.Second, time.Millisecond)
}

func TestH1RecoveryReader_FiftiethPreparationPreservesWaitingWriterTurn(t *testing.T) {
	f, jobs := h1RecoveryReaderPair(t)
	require.NoError(t, f.shared.Delete(f.sessions[0]).Error)
	require.NoError(t, f.shared.Delete(f.sessions[1]).Error)
	g := &f.app.continuationAdmission
	org := f.works[0].OrganizationID.String()
	maximum := func() int { return 10 }
	anchor := h1OrgHold(t, g, 1, inboundContinuationFreshPriority, maximum, org, inboundContinuationReader)
	for range 48 {
		release := h1OrgHold(t, g, 1, inboundContinuationFreshPriority, maximum, org, inboundContinuationReader)
		release()
	}
	writer := h1OrgAcquireAsync(t, g, context.Background(), nil, 1, inboundContinuationFreshPriority, maximum, org, inboundContinuationWriter)
	h1CapacityWaitQueue(t, g, 1, 1)
	p := NewInboundContinuationProcessor(f.app, time.Second)
	p.batchSize = 1
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	done := h1RecoveryReaderRun(t, f, p, ctx, cancel)
	t.Cleanup(func() { cancel(); anchor() })
	select {
	case request := <-f.model.entered:
		require.Equal(t, f.texts[0], request.Current)
	case <-time.After(5 * time.Second):
		t.Fatal("the actual fiftieth Reader preparation did not retain into model")
	}
	h1OrgBlocked(t, writer)
	g.mu.Lock()
	phase := g.phases[org]
	grants := 0
	if phase != nil {
		grants = phase.normalGrants
	}
	g.mu.Unlock()
	require.NotNil(t, phase)
	require.Equal(t, 50, grants)
	next := NewInboundContinuationProcessor(f.app, time.Second)
	next.batchSize = 1
	nextDone := h1RecoveryReaderRun(t, f, next, ctx, cancel)
	t.Cleanup(func() { cancel(); anchor() })
	h1CapacityWaitQueue(t, g, 2, 2)
	require.Equal(t, jobs[1], loadInboundContinuationJob(t, &App{DB: f.shared}, f.works[1].OrganizationID, f.works[1].Persisted.ID), "the fifty-first Reader cannot claim before the waiting Writer turn")
	f.model.unblock()
	h1CapacityJoin(t, done, 1)
	h1CapacityWaitQueue(t, g, 1, 2)
	h1OrgBlocked(t, writer)
	anchor()
	releaseWriter := h1OrgGrant(t, writer)
	h1CapacityWaitQueue(t, g, 1, 1)
	require.Equal(t, jobs[1], loadInboundContinuationJob(t, &App{DB: f.shared}, f.works[1].OrganizationID, f.works[1].Persisted.ID))
	releaseWriter()
	h1CapacityJoin(t, nextDone, 1)
	for i := range jobs {
		h1RecoveryReaderLoadSession(t, f, i)
		f.assertGenerated(t, i)
	}
	require.EqualValues(t, 2, f.model.calls.Load())
	h1OrgEmpty(t, g)
}
