package handlers

import (
	"context"
	"testing"
	"time"

	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/stretchr/testify/require"
)

// This public-path witness uses only APIs present on the unmodified baseline.
// An existing-session hint must wait for Writer ownership before claiming;
// the baseline instead claims as Reader and only then queues its Writer.
func TestH1FastPreparationHint_ExistingSessionWaitsAsWriterBeforeClaim(t *testing.T) {
	f := h1CapacityLegacy(t, 1)
	stopGuard := h1ReaderPreparationHoldGuard(t, f)
	work := f.works[0]
	p := NewInboundContinuationProcessor(f.app, time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	done, joined := make(chan error, 1), make(chan struct{})
	t.Cleanup(func() {
		cancel()
		stopGuard()
		f.model.unblock()
		select {
		case <-joined:
		case <-time.After(12 * time.Second):
			t.Error("preclaim hint witness did not join")
		}
	})
	go func() {
		defer close(joined)
		done <- p.ProcessMessage(ctx, work.OrganizationID, work.Persisted.ID)
	}()
	g := &f.app.continuationAdmission
	require.Eventually(t, func() bool {
		g.mu.Lock()
		defer g.mu.Unlock()
		return g.used == 1 && len(g.waiters) == 1 &&
			g.waiters[0].organization == work.OrganizationID.String() &&
			g.waiters[0].mode == inboundContinuationWriter &&
			g.waiters[0].priority == inboundContinuationPerformPriority && !g.waiters[0].intake
	}, 5*time.Second, time.Millisecond, "the actual ordinary Writer must be queued behind the physical Reader")
	job := loadInboundContinuationJob(t, &App{DB: f.shared}, work.OrganizationID, work.Persisted.ID)
	require.Equal(t, models.ScheduledJobStatusPending, job.Status,
		"existing-session scheduling must queue Writer before the durable claim")
	require.Zero(t, job.Attempts)
	require.Nil(t, job.LockedAt)
	require.Empty(t, job.LockedBy)
	require.Eventually(t, func() bool { return f.pool.Stats().InUse == 1 }, time.Second, time.Millisecond,
		"hint transaction must close before waiting; only the real physical holder retains SQL")
	require.Zero(t, f.model.calls.Load())
	stopGuard()
	select {
	case request := <-f.model.entered:
		require.Equal(t, f.texts[0], request.Current)
	case <-time.After(8 * time.Second):
		t.Fatal("claimed existing-session work did not reach the actual model")
	}
	f.model.unblock()
	h1CapacityJoin(t, done, 1)
	f.assertGenerated(t, 0)
	require.NoError(t, p.ProcessMessage(ctx, work.OrganizationID, work.Persisted.ID))
	f.assertGenerated(t, 0)
	require.EqualValues(t, 1, f.model.calls.Load(), "duplicate entry must not replay the accepted model")
	h1OrgEmpty(t, g)
}
