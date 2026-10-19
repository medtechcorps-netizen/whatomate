package handlers

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// These neutral fixtures and the public witness compile on unchanged 77d.
// They depend on no advisory-recovery helper or candidate-only product API.
func h1RecoveryReaderPair(t *testing.T) (*h1CapacityLegacyFixture, []models.ScheduledJob) {
	t.Helper()
	f := h1CapacityLegacy(t, 1)
	text := "Synthetic capacity recovery second " + uuid.NewString()
	message := inboundContinuationTextMessage(t, "wamid.recovery-reader-"+uuid.NewString(), "601799009902", text)
	work, duplicate, err := h1RootApp(f.shared, false).persistIncomingMessageBeforeAck(
		f.works[0].Account.PhoneID, message, "Synthetic recovery reader fixture")
	require.NoError(t, err)
	require.False(t, duplicate)
	require.Equal(t, f.works[0].OrganizationID, work.OrganizationID)
	require.NotEqual(t, f.works[0].Contact.ID, work.Contact.ID)
	session := &models.ChatbotSession{
		BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: work.OrganizationID,
		ContactID: work.Contact.ID, WhatsAppAccount: work.Account.Name, PhoneNumber: work.Contact.PhoneNumber,
		Status: models.SessionStatusActive, SessionData: models.JSONB{}, StartedAt: time.Now(), LastActivityAt: time.Now(),
	}
	require.NoError(t, f.shared.Create(session).Error)
	f.works = append(f.works, work)
	f.sessions = append(f.sessions, session)
	f.texts = append(f.texts, text)
	h1RecoveryOnlyFixtureOrganization(t, f)
	jobs := make([]models.ScheduledJob, 2)
	for i := range jobs {
		jobs[i] = loadInboundContinuationJob(t, &App{DB: f.shared}, work.OrganizationID, f.works[i].Persisted.ID)
		require.NoError(t, f.shared.Model(&models.ScheduledJob{}).Where("id = ?", jobs[i].ID).
			Update("run_at", time.Now().UTC().Add(time.Duration(i-2)*time.Hour)).Error)
		jobs[i] = loadInboundContinuationJob(t, &App{DB: f.shared}, work.OrganizationID, f.works[i].Persisted.ID)
	}
	return f, jobs
}

func h1RecoveryReaderLoadSession(t *testing.T, f *h1CapacityLegacyFixture, index int) {
	t.Helper()
	var session models.ChatbotSession
	work := f.works[index]
	require.NoError(t, f.shared.Where("organization_id = ? AND contact_id = ? AND whats_app_account = ? AND status = ?",
		work.OrganizationID, work.Contact.ID, work.Account.Name, models.SessionStatusActive).First(&session).Error)
	f.sessions[index] = &session
}

func h1RecoveryReaderRun(t *testing.T, f *h1CapacityLegacyFixture, p *InboundContinuationProcessor,
	ctx context.Context, cancel context.CancelFunc) <-chan error {
	t.Helper()
	done, joined := make(chan error, 1), make(chan struct{})
	t.Cleanup(func() {
		cancel()
		f.model.unblock()
		select {
		case <-joined:
		case <-time.After(12 * time.Second):
			t.Error("recovery reader processor did not join")
		}
	})
	go func() { defer close(joined); done <- p.RunOnce(ctx) }()
	return done
}

func h1RecoveryReaderClaimOrder(t *testing.T, app *App) <-chan uuid.UUID {
	t.Helper()
	claimed := make(chan uuid.UUID, 64)
	name := "h1:recovery-reader-claim-order:" + uuid.NewString()
	require.NoError(t, app.DB.Callback().Query().After("gorm:query").Register(name, func(tx *gorm.DB) {
		if tx.Error != nil || tx.Statement == nil || tx.Statement.Schema == nil || tx.Statement.Schema.Table != "scheduled_jobs" {
			return
		}
		locking, ok := tx.Statement.Clauses["FOR"].Expression.(clause.Locking)
		if !ok || locking.Strength != "UPDATE" || locking.Options != "SKIP LOCKED" {
			return
		}
		job, ok := tx.Statement.Dest.(*models.ScheduledJob)
		if ok && job != nil && job.ID != uuid.Nil {
			claimed <- job.ID
		}
	}))
	t.Cleanup(func() { _ = app.DB.Callback().Query().Remove(name) })
	return claimed
}

func TestH1RecoveryReader_ExistingFirstDoesNotHideNewSecondBehindPhysicalReader(t *testing.T) {
	f, jobs := h1RecoveryReaderPair(t)
	require.NoError(t, f.shared.Delete(f.sessions[1]).Error)
	var generation models.ChatbotSession
	require.NoError(t, f.shared.Where("id = ?", f.sessions[0].ID).First(&generation).Error)
	stopGuard := h1ReaderPreparationHoldGuard(t, f)
	p := NewInboundContinuationProcessor(f.app, time.Second)
	p.batchSize = 2
	claimOrder := h1RecoveryReaderClaimOrder(t, f.app)
	p.lease = 600 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	done := h1RecoveryReaderRun(t, f, p, ctx, cancel)
	// LIFO releases the independent physical holder before the run cleanup joins.
	t.Cleanup(func() { cancel(); stopGuard() })
	select {
	case request := <-f.model.entered:
		require.Equal(t, f.texts[1], request.Current, "the later new job, not the earlier existing job, must enter first")
	case <-time.After(5 * time.Second):
		h1CapacityWaitQueue(t, &f.app.continuationAdmission, 1, 1)
		require.Equal(t, 1, f.pool.Stats().InUse, "the independent physical Reader remains held")
		for i := range jobs {
			current := loadInboundContinuationJob(t, &App{DB: f.shared}, f.works[i].OrganizationID, f.works[i].Persisted.ID)
			require.Equal(t, models.ScheduledJobStatusPending, current.Status)
			require.Zero(t, current.Attempts, "baseline blocks on the first existing job before either claim")
		}
		t.Fatal("ordered new second job did not reach its model while existing first waited behind the physical Reader")
	}
	for _, job := range jobs {
		select {
		case actual := <-claimOrder:
			require.Equal(t, job.ID, actual, "actual SKIP LOCKED selections preserve original order")
		default:
			t.Fatal("missing actual ordered claim")
		}
	}
	g := &f.app.continuationAdmission
	h1CapacityWaitQueue(t, g, 2, 1)
	g.mu.Lock()
	queuedOrg, queuedMode, queuedPriority := g.waiters[0].organization, g.waiters[0].mode, g.waiters[0].priority
	phase := g.phases[f.works[0].OrganizationID.String()]
	active, activeMode := 0, inboundContinuationUnscoped
	if phase != nil {
		active, activeMode = phase.active, phase.activeMode
	}
	g.mu.Unlock()
	require.NotNil(t, phase)
	require.Equal(t, f.works[0].OrganizationID.String(), queuedOrg)
	require.Equal(t, inboundContinuationWriter, queuedMode)
	require.Equal(t, inboundContinuationPerformPriority, queuedPriority)
	require.Equal(t, inboundContinuationReader, activeMode)
	require.Equal(t, 2, active, "only the holder and second job's actual model retain Reader ownership")
	require.Eventually(t, func() bool { return f.pool.Stats().InUse == 2 }, time.Second, time.Millisecond,
		"the waiting first job owns neither prior Reader credit nor SQL")
	first := loadInboundContinuationJob(t, &App{DB: f.shared}, f.works[0].OrganizationID, f.works[0].Persisted.ID)
	require.Equal(t, models.ScheduledJobStatusProcessing, first.Status)
	require.Equal(t, 1, first.Attempts)
	require.NotNil(t, first.LockedAt)
	require.Eventually(t, func() bool {
		current := loadInboundContinuationJob(t, &App{DB: f.shared}, f.works[0].OrganizationID, f.works[0].Persisted.ID)
		return current.LockedAt != nil && current.LockedAt.After(first.LockedAt.Add(650*time.Millisecond))
	}, 3*time.Second, 20*time.Millisecond, "the first claim keeps its lease while waiting for authoritative Writer")
	competitor := NewInboundContinuationProcessor(f.app, time.Second)
	competitor.lease = p.lease
	claimed, err := competitor.claim(ctx, f.works[0].OrganizationID, &f.works[0].Persisted.ID)
	require.NoError(t, err)
	require.Nil(t, claimed)
	var untouched models.ChatbotSession
	require.NoError(t, f.shared.Where("id = ?", generation.ID).First(&untouched).Error)
	require.Equal(t, generation, untouched, "existing generation cannot mutate while the physical Reader excludes Writer")
	select {
	case <-done:
		t.Fatal("RunOnce returned before joining its first worker")
	default:
	}
	f.model.unblock()
	require.Eventually(t, func() bool {
		return loadInboundContinuationJob(t, &App{DB: f.shared}, f.works[1].OrganizationID, f.works[1].Persisted.ID).Status == models.ScheduledJobStatusCompleted
	}, 5*time.Second, time.Millisecond)
	stopGuard()
	h1CapacityJoin(t, done, 1)
	select {
	case request := <-f.model.entered:
		require.Equal(t, f.texts[0], request.Current)
	default:
		t.Fatal("the original existing job did not reach the actual model after Writer became available")
	}
	for i := range jobs {
		h1RecoveryReaderLoadSession(t, f, i)
		f.assertGenerated(t, i)
		require.NoError(t, p.ProcessMessage(ctx, f.works[i].OrganizationID, f.works[i].Persisted.ID))
		f.assertGenerated(t, i)
	}
	require.EqualValues(t, 2, f.model.calls.Load(), "ordered claims and replay produce each accepted result once")
	h1OrgEmpty(t, g)
}
