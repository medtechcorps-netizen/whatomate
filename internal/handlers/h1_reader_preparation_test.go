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
)

// This holder owns both an actual physical guard and a permit on the same
// root as the processor. A separate root alone would not expose the old local
// writer-admission blockage for an otherwise compatible new-session insert.
func h1ReaderPreparationHoldGuard(t *testing.T, f *h1CapacityLegacyFixture) func() {
	t.Helper()
	work := f.works[0]
	holder := h1CapacityReaderPersist(t, h1RootApp(f.shared, false), &work.Account, "reader-preparation-holder")
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	release, err := f.app.continuationAdmission.acquireScoped(ctx, nil, 1, inboundContinuationPerformPriority,
		func() int { return f.pool.Stats().MaxOpenConnections }, work.OrganizationID.String(), inboundContinuationReader)
	require.NoError(t, err)
	execution := f.app.scopedApp(f.app.DB, work.OrganizationID)
	execution.tenantOrgID = uuid.Nil
	execution.inboundContinuation = &inboundContinuationExecution{
		OrganizationID: holder.OrganizationID, ContactID: holder.Contact.ID,
		MessageID: holder.Persisted.ID, WAMID: holder.Message.ID,
	}
	entered, finish, joined := make(chan struct{}), make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	var once sync.Once
	var guardErr error
	stop := func() {
		once.Do(func() {
			close(finish)
			select {
			case <-joined:
				if guardErr != nil {
					t.Errorf("physical preparation holder failed: %v", guardErr)
				}
			case <-time.After(10 * time.Second):
				t.Error("physical preparation holder did not join")
			}
			release()
			cancel()
		})
	}
	t.Cleanup(stop)
	go func() {
		defer close(joined)
		guardErr = execution.withInboundContinuationPhysicalAIAttempt(ctx, func() error {
			close(entered)
			select {
			case <-finish:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
		done <- guardErr
	}()
	select {
	case <-entered:
	case err := <-done:
		t.Fatalf("real physical holder refused: %v", err)
	case <-time.After(8 * time.Second):
		t.Fatal("real physical holder did not enter")
	}
	require.Equal(t, 1, f.pool.Stats().InUse)
	return stop
}

func h1ReaderPreparationRemoveSeed(t *testing.T, f *h1CapacityLegacyFixture) {
	t.Helper()
	result := f.shared.Where("id = ? AND organization_id = ?", f.sessions[0].ID, f.works[0].OrganizationID).Delete(&models.ChatbotSession{})
	require.NoError(t, result.Error)
	require.EqualValues(t, 1, result.RowsAffected)
}

func h1ReaderPreparationLoadSession(t *testing.T, f *h1CapacityLegacyFixture) {
	t.Helper()
	session := &models.ChatbotSession{}
	require.NoError(t, f.shared.Where("organization_id = ? AND contact_id = ? AND whats_app_account = ? AND status = ?",
		f.works[0].OrganizationID, f.works[0].Contact.ID, f.works[0].Account.Name, models.SessionStatusActive).First(session).Error)
	f.sessions[0] = session
}

func TestH1ReaderPreparation_NewSessionCompletesUnderSameRootPhysicalReader(t *testing.T) {
	f := h1CapacityLegacy(t, 1)
	h1ReaderPreparationRemoveSeed(t, f)
	stopGuard := h1ReaderPreparationHoldGuard(t, f)
	done := f.start(t, 0)
	select {
	case request := <-f.model.entered:
		require.Equal(t, f.texts[0], request.Current)
	case <-time.After(5 * time.Second):
		h1CapacityWaitQueue(t, &f.app.continuationAdmission, 1, 1)
		require.Equal(t, 1, f.pool.Stats().InUse, "the actual physical reader remains held at the baseline refusal")
		t.Fatal("new-session ProcessMessage did not reach its actual model while the physical reader remained held")
	}
	require.Equal(t, 2, f.pool.Stats().InUse, "holder and actual model guards; no outer preparation SQL remains")
	g := &f.app.continuationAdmission
	g.mu.Lock()
	phase := g.phases[f.works[0].OrganizationID.String()]
	used, active, grants := g.used, phase.active, phase.normalGrants
	g.mu.Unlock()
	require.Equal(t, 2, used)
	require.Equal(t, 2, active)
	require.Equal(t, 2, grants, "one holder plus one combined preparation/model grant")
	f.model.unblock()
	h1CapacityJoin(t, done, 1)
	require.Equal(t, 1, f.pool.Stats().InUse, "the original physical holder still spans all effects")
	h1ReaderPreparationLoadSession(t, f)
	f.assertGenerated(t, 0)
	processor := NewInboundContinuationProcessor(f.app, time.Second)
	require.NoError(t, processor.ProcessMessage(context.Background(), f.works[0].OrganizationID, f.works[0].Persisted.ID))
	f.assertGenerated(t, 0)
	require.EqualValues(t, 1, f.model.calls.Load())
	f.mu.Lock()
	sends := len(f.sent)
	f.mu.Unlock()
	require.Equal(t, 1, sends)
	stopGuard()
	h1OrgEmpty(t, g)
}

func TestH1ReaderPreparation_ExistingSessionFallbackReleasesReaderAndRenewsClaim(t *testing.T) {
	f := h1CapacityLegacy(t, 1)
	negativeHint := h1FastHintNegative(t, f.app) // Preserve the actual post-claim fallback and heartbeat route.
	stopGuard := h1ReaderPreparationHoldGuard(t, f)
	p := NewInboundContinuationProcessor(f.app, time.Second)
	p.lease = 600 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	done, joined := make(chan error, 1), make(chan struct{})
	t.Cleanup(func() {
		cancel()
		stopGuard()
		f.model.unblock()
		select {
		case <-joined:
		case <-time.After(10 * time.Second):
			t.Error("writer fallback processor did not join")
		}
	})
	work := f.works[0]
	go func() { defer close(joined); done <- p.ProcessMessage(ctx, work.OrganizationID, work.Persisted.ID) }()
	g := &f.app.continuationAdmission
	h1CapacityWaitQueue(t, g, 1, 1)
	g.mu.Lock()
	mode := g.waiters[0].mode
	g.mu.Unlock()
	require.Equal(t, inboundContinuationWriter, mode)
	before := loadInboundContinuationJob(t, &App{DB: f.shared}, work.OrganizationID, work.Persisted.ID)
	require.Equal(t, models.ScheduledJobStatusProcessing, before.Status, "fallback follows an actual claim")
	require.EqualValues(t, 1, negativeHint.Load(), "exercise the explicit advisory-negative fallback fixture")
	require.NotNil(t, before.LockedAt)
	require.NotEmpty(t, before.LockedBy)
	require.Eventually(t, func() bool {
		now := loadInboundContinuationJob(t, &App{DB: f.shared}, work.OrganizationID, work.Persisted.ID)
		return now.LockedAt != nil && now.LockedAt.After(before.LockedAt.Add(650*time.Millisecond))
	}, 3*time.Second, 20*time.Millisecond, "heartbeat must outlive the original lease while no preparation permit is held")
	require.Eventually(t, func() bool { return f.pool.Stats().InUse == 1 }, time.Second, time.Millisecond,
		"only the real holder may retain SQL while fallback waits; heartbeat statements are short")
	require.Zero(t, f.model.calls.Load())
	competitor := NewInboundContinuationProcessor(f.app, time.Second)
	competitor.lease = p.lease
	claimed, err := competitor.claim(ctx, work.OrganizationID, &work.Persisted.ID)
	require.NoError(t, err)
	require.Nil(t, claimed, "the waiting processor retains its real durable lease")
	stopGuard()
	f.model.unblock()
	h1CapacityJoin(t, done, 1)
	f.assertGenerated(t, 0)
	h1OrgEmpty(t, g)
}

func TestH1ReaderPreparation_StopOrCancelBeforeModelDoesNotCreateAction(t *testing.T) {
	for _, action := range []string{"stop", "cancel"} {
		t.Run(action, func(t *testing.T) {
			f := h1CapacityLegacy(t, 1)
			h1ReaderPreparationRemoveSeed(t, f)
			p := NewInboundContinuationProcessor(f.app, time.Second)
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			t.Cleanup(cancel)
			name := "h1:reader-preparation-stop:" + uuid.NewString()
			var once sync.Once
			triggered := false
			require.NoError(t, f.app.DB.Callback().Create().After("gorm:create").Register(name, func(tx *gorm.DB) {
				if tx.Statement.Schema == nil || tx.Statement.Schema.Table != "chatbot_sessions" || tx.Error != nil {
					return
				}
				once.Do(func() {
					triggered = true
					if action == "stop" {
						p.Stop()
					} else {
						cancel()
					}
				})
			}))
			t.Cleanup(func() { _ = f.app.DB.Callback().Create().Remove(name) })
			err := p.ProcessMessage(ctx, f.works[0].OrganizationID, f.works[0].Persisted.ID)
			require.True(t, triggered, "exercise the boundary after real new-session creation")
			require.Error(t, err)
			require.Zero(t, f.model.calls.Load())
			var actions int64
			require.NoError(t, f.shared.Model(&models.ScheduledJob{}).Where("organization_id = ? AND kind = ? AND aggregate_id = ?",
				f.works[0].OrganizationID, inboundContinuationActionJobKind, f.works[0].Persisted.ID).Count(&actions).Error)
			require.Zero(t, actions, "retained preparation cannot bypass cancellation into model/action ownership")
			f.mu.Lock()
			sends := len(f.sent)
			f.mu.Unlock()
			require.Zero(t, sends)
			h1OrgEmpty(t, &f.app.continuationAdmission)
		})
	}
}
