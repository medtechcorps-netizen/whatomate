package handlers

import (
	"context"
	"strings"
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

func h1RecoveryHintSQL(tx *gorm.DB) bool {
	return tx.Statement != nil && strings.Contains(tx.Statement.SQL.String(), "FROM scheduled_jobs AS j") &&
		strings.Contains(tx.Statement.SQL.String(), "ORDER BY j.run_at, j.created_at, j.id")
}

func h1RecoveryHintObserve(t *testing.T, app *App, fn func(*gorm.DB)) {
	t.Helper()
	name := "h1:recovery-hint:" + uuid.NewString()
	require.NoError(t, app.DB.Callback().Row().Before("gorm:row").Register(name, func(tx *gorm.DB) {
		if h1RecoveryHintSQL(tx) {
			fn(tx)
		}
	}))
	t.Cleanup(func() { _ = app.DB.Callback().Row().Remove(name) })
}

// Only the optional recovery statement fails. The transaction really aborts;
// the subsequent authoritative claim and session statements remain untouched.
func h1RecoveryHintFailSQL(t *testing.T, app *App) *atomic.Int32 {
	t.Helper()
	hits := &atomic.Int32{}
	h1RecoveryHintObserve(t, app, func(tx *gorm.DB) {
		hits.Add(1)
		tx.Statement.SQL.Reset()
		tx.Statement.SQL.WriteString("SELECT 1 / 0")
		tx.Statement.Vars = nil
	})
	return hits
}

func h1RecoveryHintPair(t *testing.T) (*h1CapacityLegacyFixture, []models.ScheduledJob) {
	t.Helper()
	f := h1CapacityLegacy(t, 1)
	text := "Synthetic capacity recovery second " + uuid.NewString()
	message := inboundContinuationTextMessage(t, "wamid.recovery-hint-"+uuid.NewString(), "601799009902", text)
	work, duplicate, err := h1RootApp(f.shared, false).persistIncomingMessageBeforeAck(
		f.works[0].Account.PhoneID, message, "Synthetic recovery hint fixture")
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
			Update("run_at", time.Now().UTC().Add(time.Duration(i-2)*time.Minute)).Error)
		jobs[i] = loadInboundContinuationJob(t, &App{DB: f.shared}, work.OrganizationID, f.works[i].Persisted.ID)
	}
	return f, jobs
}

func h1RecoveryHintLoadSession(t *testing.T, f *h1CapacityLegacyFixture, index int) {
	t.Helper()
	var session models.ChatbotSession
	work := f.works[index]
	require.NoError(t, f.shared.Where("organization_id = ? AND contact_id = ? AND whats_app_account = ? AND status = ?",
		work.OrganizationID, work.Contact.ID, work.Account.Name, models.SessionStatusActive).First(&session).Error)
	f.sessions[index] = &session
}

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

func TestH1RecoveryPreparationHint_FirstCandidatePrecedesIdentityQualification(t *testing.T) {
	for _, first := range []string{"existing", "new", "expired-active", "duplicate-active", "missing-message", "foreign-message", "deleted-message", "outgoing-message", "empty-account", "space-account", "deleted-contact", "merged-contact"} {
		t.Run(first, func(t *testing.T) {
			f, jobs := h1RecoveryHintPair(t)
			// The second candidate always qualifies as new. It must not replace
			// an earlier existing or unusable candidate in the advisory query.
			require.NoError(t, f.shared.Delete(f.sessions[1]).Error)
			want := inboundContinuationWriter
			if first != "existing" && first != "expired-active" && first != "duplicate-active" {
				h1ReaderPreparationRemoveSeed(t, f)
			}
			switch first {
			case "new":
				want = inboundContinuationReader
			case "expired-active":
				require.NoError(t, f.shared.Model(&models.ChatbotSession{}).Where("id = ?", f.sessions[0].ID).
					Update("last_activity_at", time.Now().Add(-24*time.Hour)).Error)
			case "duplicate-active":
				duplicate := *f.sessions[0]
				duplicate.ID = uuid.New()
				require.NoError(t, f.shared.Create(&duplicate).Error)
			case "missing-message":
				require.NoError(t, f.shared.Model(&models.ScheduledJob{}).Where("id = ?", jobs[0].ID).
					Update("aggregate_id", uuid.New()).Error)
			case "foreign-message":
				foreign := h1CapacityLegacy(t, 1)
				require.NoError(t, f.shared.Model(&models.ScheduledJob{}).Where("id = ?", jobs[0].ID).
					Update("aggregate_id", foreign.works[0].Persisted.ID).Error)
			case "deleted-message":
				require.NoError(t, f.shared.Delete(&f.works[0].Persisted).Error)
			case "outgoing-message":
				require.NoError(t, f.shared.Model(&models.Message{}).Where("id = ?", f.works[0].Persisted.ID).
					Update("direction", models.DirectionOutgoing).Error)
			case "empty-account", "space-account":
				account := ""
				if first == "space-account" {
					account = "   "
				}
				require.NoError(t, f.shared.Model(&models.Message{}).Where("id = ?", f.works[0].Persisted.ID).
					Update("whats_app_account", account).Error)
			case "deleted-contact":
				require.NoError(t, f.shared.Delete(&f.works[0].Contact).Error)
			case "merged-contact":
				require.NoError(t, f.shared.Model(&models.Contact{}).Where("id = ?", f.works[0].Contact.ID).
					Update("merged_into_id", f.works[1].Contact.ID).Error)
			}
			var before []models.ScheduledJob
			require.NoError(t, f.shared.Where("id IN ?", []uuid.UUID{jobs[0].ID, jobs[1].ID}).Order("id").Find(&before).Error)
			var queries atomic.Int32
			h1RecoveryHintObserve(t, f.app, func(*gorm.DB) { queries.Add(1) })
			p := NewInboundContinuationProcessor(f.app, time.Second)
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			mode, err := p.hintInboundRecoveryPreparationMode(ctx, f.works[0].OrganizationID)
			require.NoError(t, err)
			require.Equal(t, want, mode)
			require.EqualValues(t, 1, queries.Load())
			var after []models.ScheduledJob
			require.NoError(t, f.shared.Where("id IN ?", []uuid.UUID{jobs[0].ID, jobs[1].ID}).Order("id").Find(&after).Error)
			require.Equal(t, before, after, "discovery neither reserves the first job nor skips it by claiming the second")
			h1OrgEmpty(t, &f.app.continuationAdmission)
			require.Zero(t, f.pool.Stats().InUse)
		})
	}
}

func TestH1RecoveryPreparationHint_LockedCandidateDoesNotConstrainActualClaim(t *testing.T) {
	for _, direction := range []string{"new-A-existing-B", "existing-A-new-B"} {
		t.Run(direction, func(t *testing.T) {
			f, jobs := h1RecoveryHintPair(t)
			wantMode := inboundContinuationWriter
			if direction == "new-A-existing-B" {
				h1ReaderPreparationRemoveSeed(t, f)
				wantMode = inboundContinuationReader
			} else {
				require.NoError(t, f.shared.Delete(f.sessions[1]).Error)
			}
			unlock := h1RecoveryHintLockJob(t, f, jobs[0])
			initialMode := h1RecoveryHintInitialSessionMode(t, f)
			p := NewInboundContinuationProcessor(f.app, time.Second)
			p.batchSize = 1
			ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
			done := h1RecoveryHintRun(t, f, p, ctx, cancel)
			t.Cleanup(func() { cancel(); unlock() })
			select {
			case mode := <-initialMode:
				require.Equal(t, wantMode, mode, "the real first snapshot routes admission; it does not select the claim")
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
			h1RecoveryHintLoadSession(t, f, 1)
			f.assertGenerated(t, 1)
			unlock()
			require.NoError(t, p.RunOnce(ctx))
			h1RecoveryHintLoadSession(t, f, 0)
			f.assertGenerated(t, 0)
			require.NoError(t, p.RunOnce(ctx))
			require.EqualValues(t, 2, f.model.calls.Load(), "both actual claims resolve once; replay does not recompute")
			h1OrgEmpty(t, &f.app.continuationAdmission)
		})
	}
}

func TestH1RecoveryPreparationHint_NegativeReaderShrinkRefusesReplacementBeforeMutation(t *testing.T) {
	f, jobs := h1RecoveryHintPair(t)
	h1ReaderPreparationRemoveSeed(t, f)
	unlock := h1RecoveryHintLockJob(t, f, jobs[0])
	var before models.ChatbotSession
	require.NoError(t, f.shared.Where("id = ?", f.sessions[1].ID).First(&before).Error)
	g := &f.app.continuationAdmission
	release := h1OrgHold(t, g, 1, inboundContinuationFreshPriority,
		func() int { return f.pool.Stats().MaxOpenConnections }, f.works[0].OrganizationID.String(), inboundContinuationWriter)
	var queries atomic.Int32
	h1RecoveryHintObserve(t, f.app, func(*gorm.DB) { queries.Add(1) })
	initialMode := h1RecoveryHintInitialSessionMode(t, f)
	p := NewInboundContinuationProcessor(f.app, time.Second)
	p.batchSize = 1
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	done := h1RecoveryHintRun(t, f, p, ctx, cancel)
	t.Cleanup(func() { cancel(); release(); unlock(); f.pool.SetMaxOpenConns(10) })
	h1RecoveryHintQueued(t, f, 1, inboundContinuationReader, inboundContinuationFreshPriority)
	require.EqualValues(t, 1, queries.Load())
	require.Zero(t, f.pool.Stats().InUse, "discovery is fully closed before actual Reader admission")
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
