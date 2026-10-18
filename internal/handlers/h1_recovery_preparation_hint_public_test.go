package handlers

import (
	"context"
	"testing"
	"time"

	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/stretchr/testify/require"
)

// This file deliberately uses only the pre-hint public processor and existing
// fixtures, so the same witness can be compiled on the unchanged parent.
func h1RecoveryHintRun(t *testing.T, f *h1CapacityLegacyFixture, p *InboundContinuationProcessor,
	ctx context.Context, cancel context.CancelFunc) <-chan error {
	t.Helper()
	done, joined := make(chan error, 1), make(chan struct{})
	t.Cleanup(func() {
		cancel()
		f.model.unblock()
		select {
		case <-joined:
		case <-time.After(12 * time.Second):
			t.Error("recovery hint processor did not join")
		}
	})
	go func() {
		defer close(joined)
		done <- p.RunOnce(ctx)
	}()
	return done
}

func TestH1RecoveryPreparationHint_NewSessionReachesModelUnderPhysicalReader(t *testing.T) {
	f := h1CapacityLegacy(t, 1)
	h1RecoveryOnlyFixtureOrganization(t, f)
	h1ReaderPreparationRemoveSeed(t, f)
	stopGuard := h1ReaderPreparationHoldGuard(t, f)
	work := f.works[0]
	// The holder persists its own message. Put the actual target first rather
	// than assuming random IDs or fixture creation timestamps determine order.
	job := loadInboundContinuationJob(t, &App{DB: f.shared}, work.OrganizationID, work.Persisted.ID)
	require.NoError(t, f.shared.Model(&models.ScheduledJob{}).Where("id = ?", job.ID).
		Update("run_at", time.Now().UTC().Add(-time.Hour)).Error)
	p := NewInboundContinuationProcessor(f.app, time.Second)
	p.batchSize = 1
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	done := h1RecoveryHintRun(t, f, p, ctx, cancel)
	t.Cleanup(func() { cancel(); stopGuard() })
	select {
	case request := <-f.model.entered:
		require.Equal(t, f.texts[0], request.Current)
	case <-time.After(5 * time.Second):
		h1CapacityWaitQueue(t, &f.app.continuationAdmission, 1, 1)
		require.Equal(t, 1, f.pool.Stats().InUse, "the real physical holder is still live")
		pending := loadInboundContinuationJob(t, &App{DB: f.shared}, work.OrganizationID, work.Persisted.ID)
		require.Equal(t, models.ScheduledJobStatusPending, pending.Status)
		require.Zero(t, pending.Attempts, "the old Writer route remains before its authoritative claim")
		t.Fatal("new-session RunOnce did not reach its actual model while the physical reader remained held")
	}
	g := &f.app.continuationAdmission
	g.mu.Lock()
	phase := g.phases[work.OrganizationID.String()]
	used, active, grants := g.used, 0, 0
	if phase != nil {
		active, grants = phase.active, phase.normalGrants
	}
	g.mu.Unlock()
	require.NotNil(t, phase, "actual model must still own its organization Reader phase")
	require.Equal(t, 2, used)
	require.Equal(t, 2, active)
	require.Equal(t, 2, grants, "one holder and one retained preparation/model grant")
	require.Equal(t, 2, f.pool.Stats().InUse, "two actual physical guards; discovery and preparation SQL closed")
	f.model.unblock()
	h1CapacityJoin(t, done, 1)
	h1ReaderPreparationLoadSession(t, f)
	f.assertGenerated(t, 0)
	require.EqualValues(t, 1, f.model.calls.Load())
	require.Equal(t, 1, f.pool.Stats().InUse, "the original guard spans completed effects")
	stopGuard()
	h1OrgEmpty(t, g)
}
