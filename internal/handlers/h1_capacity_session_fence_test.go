package handlers

// Candidate-only SQL fence regressions. Source proposal, not executed.
import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/database"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestH1CapacitySession_NewReaderRowWaitExpiresAsRefundedRetry(t *testing.T) {
	h1RequireSlow(t)
	f := h1CapacityLegacy(t, 1)
	work := f.works[0]
	require.NoError(t, f.shared.Delete(f.sessions[0]).Error)
	f.app.Redis = nil
	settingsRead, allow := make(chan struct{}), make(chan struct{})
	var held atomic.Bool
	var once sync.Once
	releaseRead := func() { once.Do(func() { close(allow) }) }
	callback := "h1:capacity-session-row-wait:" + uuid.NewString()
	require.NoError(t, f.app.DB.Callback().Query().After("gorm:query").Register(callback, func(tx *gorm.DB) {
		if tx.Error == nil && tx.Statement.Schema != nil && tx.Statement.Schema.Table == "chatbot_settings" && held.CompareAndSwap(false, true) {
			close(settingsRead)
			<-allow
		}
	}))
	t.Cleanup(func() { _ = f.app.DB.Callback().Query().Remove(callback) })
	processor := NewInboundContinuationProcessor(f.app, time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Second)
	done, joined := make(chan error, 1), make(chan struct{})
	go func() {
		defer close(joined)
		done <- processor.ProcessMessage(ctx, work.OrganizationID, work.Persisted.ID)
	}()
	var holder *gorm.DB
	t.Cleanup(func() {
		releaseRead()
		if holder != nil {
			_ = holder.Rollback().Error
		}
		f.model.unblock()
		cancel()
		select {
		case <-joined:
		case <-time.After(25 * time.Second):
			t.Error("bounded session reader did not join")
		}
	})
	select {
	case <-settingsRead:
	case <-time.After(10 * time.Second):
		t.Fatal("preparation did not reach settings before session creation")
	}
	// Deliberately model a pre-existing row-lock holder, not a policy writer:
	// the reader must bound contact/session waits after it owns its reader fence.
	holder = f.shared.Begin()
	require.NoError(t, holder.Error)
	var lockedContact struct{ ID uuid.UUID }
	require.NoError(t, holder.Raw("SELECT id FROM contacts WHERE id = ? AND organization_id = ? FOR UPDATE", work.Contact.ID, work.OrganizationID).Scan(&lockedContact).Error)
	require.Equal(t, work.Contact.ID, lockedContact.ID)
	releaseRead()
	h1IngressWaiters(t, f.shared, 1)
	select {
	case err := <-done:
		require.ErrorIs(t, err, context.DeadlineExceeded)
		require.False(t, inboundContinuationStoppedByPolicy(err))
		require.False(t, inboundContinuationRequiresManualReview(err))
	case <-time.After(28 * time.Second):
		t.Fatal("new-session reader exceeded its whole-phase lock budget")
	}
	job := loadInboundContinuationJob(t, &App{DB: f.shared}, work.OrganizationID, work.Persisted.ID)
	require.Equal(t, models.ScheduledJobStatusPending, job.Status)
	require.Zero(t, job.Attempts, "pre-effect lock congestion refunds the claimed attempt")
	deferrals, valid := inboundContinuationExactInteger(job.Payload["wedge_guard_deferrals"])
	require.True(t, valid)
	require.Equal(t, 1, deferrals)
	require.NotEqual(t, true, job.Payload["manual_review_required"])
	require.Zero(t, f.model.calls.Load())
	var sessions, actions int64
	require.NoError(t, f.shared.Model(&models.ChatbotSession{}).Where("organization_id = ? AND contact_id = ? AND status = ?", work.OrganizationID, work.Contact.ID, models.SessionStatusActive).Count(&sessions).Error)
	require.NoError(t, f.shared.Model(&models.ScheduledJob{}).Where("organization_id = ? AND kind = ?", work.OrganizationID, inboundContinuationActionJobKind).Count(&actions).Error)
	require.Zero(t, sessions)
	require.Zero(t, actions)
	require.NoError(t, holder.Rollback().Error)
	holder = nil
	f.model.unblock()
	processor.now = func() time.Time { return job.RunAt.Add(time.Second) }
	require.NoError(t, processor.ProcessMessage(ctx, work.OrganizationID, work.Persisted.ID))
	finished := loadInboundContinuationJob(t, &App{DB: f.shared}, work.OrganizationID, work.Persisted.ID)
	require.Equal(t, models.ScheduledJobStatusCompleted, finished.Status)
	require.EqualValues(t, 1, f.model.calls.Load())
	f.mu.Lock()
	sends := len(f.sent)
	f.mu.Unlock()
	require.Equal(t, 1, sends)
}

type h1CapacitySessionResult struct {
	session *models.ChatbotSession
	isNew   bool
	err     error
}

// Reuse, expiry, and duplicate cancellation all advance or revoke an existing
// generation. None is eligible for the new-session reader fast path.
func TestH1CapacitySession_ExistingWritersWaitForActualPhysicalGuard(t *testing.T) {
	for _, mode := range []string{"reuse", "timeout", "duplicate"} {
		t.Run(mode, func(t *testing.T) {
			env := h1HandoverFixture(t, h1HandoverOptions{})
			now := time.Now().UTC().Truncate(time.Microsecond)
			activity := now
			if mode == "timeout" {
				activity = now.Add(-time.Hour)
			}
			require.NoError(t, env.shared.Model(env.session).Update("last_activity_at", activity).Error)
			require.NoError(t, env.shared.First(env.session, env.session.ID).Error)
			if mode == "duplicate" {
				other := *env.session
				other.ID, other.LastActivityAt = uuid.New(), activity.Add(-time.Minute)
				require.NoError(t, env.shared.Create(&other).Error)
			}
			var before []models.ChatbotSession
			require.NoError(t, env.shared.Where("organization_id = ? AND contact_id = ?", env.org.ID, env.work.Contact.ID).Order("id").Find(&before).Error)
			entered, allow := make(chan struct{}), make(chan struct{})
			var once sync.Once
			release := func() { once.Do(func() { close(allow) }) }
			ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
			guardDone, guardJoined := make(chan error, 1), make(chan struct{})
			processor := NewInboundContinuationProcessor(env.app, time.Second)
			processor.process = func(ctx context.Context, scoped *App, _ *persistedIncomingMessage) error {
				session := *env.session
				scoped.inboundContinuation.preparedSession = &session
				return scoped.withInboundContinuationPhysicalAIAttempt(ctx, func() error {
					close(entered)
					select {
					case <-allow:
						return nil
					case <-ctx.Done():
						return ctx.Err()
					}
				})
			}
			go func() {
				defer close(guardJoined)
				guardDone <- processor.ProcessMessage(ctx, env.org.ID, env.work.Persisted.ID)
			}()
			t.Cleanup(func() {
				release()
				cancel()
				select {
				case <-guardJoined:
				case <-time.After(10 * time.Second):
					t.Error("guard did not join")
				}
			})
			select {
			case <-entered:
			case <-time.After(10 * time.Second):
				t.Fatal("actual physical guard did not enter")
			}
			writerDB, _ := h1OpenPool(t, 1)
			writer := h1RootApp(writerDB, true)
			writer.inboundContinuation = &inboundContinuationExecution{OrganizationID: env.org.ID, ContactID: env.work.Contact.ID, MessageID: uuid.New()}
			var pid int
			require.NoError(t, writerDB.Raw("SELECT pg_backend_pid()").Scan(&pid).Error)
			writerDone, writerJoined := make(chan h1CapacitySessionResult, 1), make(chan struct{})
			go func() {
				defer close(writerJoined)
				s, n, err := writer.getOrCreatePreparedIncomingSession(env.org.ID, env.work.Contact.ID, env.account.Name, env.work.Contact.PhoneNumber, 30)
				writerDone <- h1CapacitySessionResult{s, n, err}
			}()
			t.Cleanup(func() {
				release()
				select {
				case <-writerJoined:
				case <-time.After(25 * time.Second):
					t.Error("session writer did not join")
				}
			})
			testutil.RequirePostgresBackendWaitingForLock(t, env.shared, pid)
			var during []models.ChatbotSession
			require.NoError(t, env.shared.Where("organization_id = ? AND contact_id = ?", env.org.ID, env.work.Contact.ID).Order("id").Find(&during).Error)
			require.Equal(t, before, during, "a live physical guard protects exact status and generation, including timeout and duplicates")
			select {
			case result := <-writerDone:
				t.Fatalf("session writer crossed guard: %v", result.err)
			default:
			}
			release()
			h1CapacityJoin(t, guardDone, 1)
			var result h1CapacitySessionResult
			select {
			case result = <-writerDone:
			case <-time.After(10 * time.Second):
				t.Fatal("session writer did not resume")
			}
			require.NoError(t, result.err)
			require.NotNil(t, result.session)
			var after []models.ChatbotSession
			require.NoError(t, env.shared.Where("organization_id = ? AND contact_id = ?", env.org.ID, env.work.Contact.ID).Find(&after).Error)
			active, cancelled, timedOut := 0, 0, 0
			for _, s := range after {
				switch s.Status {
				case models.SessionStatusActive:
					active++
				case models.SessionStatusCancelled:
					cancelled++
				case models.SessionStatusTimeout:
					timedOut++
				}
			}
			require.Equal(t, 1, active)
			if mode == "timeout" {
				require.True(t, result.isNew)
				require.NotEqual(t, env.session.ID, result.session.ID)
				require.Equal(t, 1, timedOut)
			} else {
				require.False(t, result.isNew)
				require.Equal(t, env.session.ID, result.session.ID)
				require.True(t, result.session.LastActivityAt.After(activity))
			}
			if mode == "duplicate" {
				require.Equal(t, 1, cancelled)
			}
		})
	}
}

func TestH1CapacitySession_ConcurrentFirstMessagesKeepOneOriginAndGreeting(t *testing.T) {
	f := h1CapacityLegacy(t, 1)
	first := f.works[0]
	h1IngressResolver(t, f.shared)
	require.NoError(t, f.shared.Delete(f.sessions[0]).Error)
	require.NoError(t, f.shared.Model(&models.ChatbotSettings{}).Where("organization_id = ?", first.OrganizationID).
		Updates(map[string]any{"ai_enabled": false, "default_response": "Synthetic first greeting", "fallback_message": ""}).Error)
	f.app.Redis = nil
	message := inboundContinuationTextMessage(t, "wamid.capacity-first-"+uuid.NewString(), first.Contact.PhoneNumber, "Synthetic capacity second first message")
	second, duplicate, err := f.app.persistIncomingMessageBeforeAck(first.Account.PhoneID, message, "Synthetic concurrent first contact")
	require.NoError(t, err)
	require.False(t, duplicate)
	works := []*persistedIncomingMessage{first, second}
	start := make(chan struct{})
	done := make(chan struct {
		index int
		h1CapacitySessionResult
	}, 2)
	var running sync.WaitGroup
	for i, work := range works {
		running.Add(1)
		go func() {
			defer running.Done()
			<-start
			execution := f.app.scopedApp(f.app.DB, work.OrganizationID)
			execution.inboundContinuation = &inboundContinuationExecution{OrganizationID: work.OrganizationID, ContactID: work.Contact.ID, MessageID: work.Persisted.ID, WAMID: work.Persisted.WhatsAppMessageID}
			s, n, err := execution.getOrCreatePreparedIncomingSession(work.OrganizationID, work.Contact.ID, work.Account.Name, work.Contact.PhoneNumber, 30)
			done <- struct {
				index int
				h1CapacitySessionResult
			}{i, h1CapacitySessionResult{s, n, err}}
		}()
	}
	close(start)
	joined := make(chan struct{})
	go func() { running.Wait(); close(joined) }()
	t.Cleanup(func() {
		select {
		case <-joined:
		case <-time.After(25 * time.Second):
			t.Error("first-session creators did not join")
		}
	})
	winner := -1
	var sessionID uuid.UUID
	for range 2 {
		select {
		case result := <-done:
			require.NoError(t, result.err)
			require.NotNil(t, result.session)
			if sessionID == uuid.Nil {
				sessionID = result.session.ID
			}
			require.Equal(t, sessionID, result.session.ID)
			if result.isNew {
				require.Equal(t, -1, winner)
				winner = result.index
			}
		case <-time.After(15 * time.Second):
			t.Fatal("first-session creator did not finish")
		}
	}
	require.NotEqual(t, -1, winner)
	var sessions []models.ChatbotSession
	require.NoError(t, f.shared.Where("organization_id = ? AND contact_id = ? AND status = ?", first.OrganizationID, first.Contact.ID, models.SessionStatusActive).Find(&sessions).Error)
	require.Len(t, sessions, 1)
	require.Equal(t, works[winner].Persisted.ID.String(), sessions[0].SessionData["__inbound_session_origin"])
	processor := NewInboundContinuationProcessor(f.app, time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, index := range []int{winner, 1 - winner, winner, 1 - winner} {
		work := works[index]
		require.NoError(t, processor.ProcessMessage(ctx, work.OrganizationID, work.Persisted.ID))
	}
	f.mu.Lock()
	sent := append([]h1CapacitySent(nil), f.sent...)
	f.mu.Unlock()
	require.Len(t, sent, 1)
	require.Equal(t, "Synthetic first greeting", sent[0].Body)
	require.Zero(t, f.model.calls.Load())
	var actions []models.ScheduledJob
	require.NoError(t, f.shared.Where("organization_id = ? AND kind = ?", first.OrganizationID, inboundContinuationActionJobKind).Find(&actions).Error)
	require.Len(t, actions, 1)
	require.Equal(t, works[winner].Persisted.ID, *actions[0].AggregateID)
	require.Equal(t, inboundActionStateResolved, actions[0].Payload["state"])
}

// A fresh physical check must reject a generation changed after the short
// preapply validation. No session history/FK lock is held by this boundary test.
func TestH1CapacitySession_GenerationChangedAfterPreapplyDeniesPhysicalEffect(t *testing.T) {
	env := h1HandoverFixture(t, h1HandoverOptions{})
	processor := NewInboundContinuationProcessor(env.app, time.Second)
	var reached bool
	processor.process = func(ctx context.Context, scoped *App, work *persistedIncomingMessage) error {
		var prepared models.ChatbotSession
		if err := scoped.DB.Where("id = ? AND organization_id = ?", env.session.ID, env.org.ID).First(&prepared).Error; err != nil {
			return err
		}
		if err := scoped.requirePreparedContinuationSessionTx(scoped.DB, &prepared); err != nil {
			return err
		}
		scoped.inboundContinuation.preparedSession = &prepared
		writerDB, _ := h1OpenPool(t, 2)
		writer := h1RootApp(writerDB, true)
		_, _, err := writer.getOrCreateSession(env.org.ID, work.Contact.ID, env.account.Name, work.Contact.PhoneNumber, 30)
		if err != nil {
			return err
		}
		return scoped.withInboundContinuationPhysicalAIAttempt(ctx, func() error { reached = true; return nil })
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	err := processor.ProcessMessage(ctx, env.org.ID, env.work.Persisted.ID)
	require.ErrorContains(t, err, "session")
	require.False(t, inboundContinuationStoppedByPolicy(err), "generation conflict remains retryable")
	require.False(t, inboundContinuationRequiresManualReview(err))
	require.False(t, reached)
	job := loadInboundContinuationJob(t, &App{DB: env.shared}, env.org.ID, env.work.Persisted.ID)
	require.Equal(t, models.ScheduledJobStatusPending, job.Status)
	require.Equal(t, 1, job.Attempts)
	var actions int64
	require.NoError(t, env.shared.Model(&models.ScheduledJob{}).Where("organization_id = ? AND kind = ? AND aggregate_id = ?", env.org.ID, inboundContinuationActionJobKind, env.work.Persisted.ID).Count(&actions).Error)
	require.Zero(t, actions)
	require.Zero(t, env.calls.Load())
}

func TestH1CapacitySession_ReaderObservesCommittedPauseWithRepeatableReadPool(t *testing.T) {
	for _, isolation := range []string{"read committed", "repeatable read"} {
		t.Run(isolation, func(t *testing.T) {
			f := h1CapacityLegacy(t, 1)
			work := f.works[0]
			require.NoError(t, f.shared.Delete(f.sessions[0]).Error)
			readerDB, _ := h1OpenPool(t, 1)
			require.NoError(t, readerDB.Exec("SET default_transaction_isolation = '"+isolation+"'").Error)
			var configured string
			require.NoError(t, readerDB.Raw("SHOW default_transaction_isolation").Scan(&configured).Error)
			require.Equal(t, isolation, configured)
			reader := h1RootApp(readerDB, true)
			var pid int
			require.NoError(t, readerDB.Raw("SELECT pg_backend_pid()").Scan(&pid).Error)
			writer := f.shared.Begin()
			require.NoError(t, writer.Error)
			t.Cleanup(func() { _ = writer.Rollback().Error })
			require.NoError(t, database.LockOrganizationPolicyScope(writer, work.OrganizationID))
			pause := &models.AgentTransfer{BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: work.OrganizationID, ContactID: work.Contact.ID,
				WhatsAppAccount: work.Account.Name, PhoneNumber: work.Contact.PhoneNumber, Status: models.TransferStatusActive, Source: models.TransferSourceManual, TransferredAt: time.Now().UTC()}
			contact := work.Contact
			writerApp := &App{DB: writer, Log: f.app.Log}
			require.NoError(t, writerApp.saveAndFinalizeTransfer(pause, &work.Account, &contact, nil, true, nil))
			done, joined := make(chan h1CapacitySessionResult, 1), make(chan struct{})
			go func() {
				defer close(joined)
				s, n, e := reader.getOrCreatePreparedIncomingSession(work.OrganizationID, work.Contact.ID, work.Account.Name, work.Contact.PhoneNumber, 30)
				done <- h1CapacitySessionResult{s, n, e}
			}()
			t.Cleanup(func() {
				_ = writer.Rollback().Error
				select {
				case <-joined:
				case <-time.After(25 * time.Second):
					t.Error("reader did not join")
				}
			})
			testutil.RequirePostgresBackendWaitingForLock(t, f.shared, pid)
			require.NoError(t, writer.Commit().Error)
			select {
			case result := <-done:
				require.True(t, inboundContinuationStoppedByPolicy(result.err), "reader waiting under a repeatable-read default must observe the committed Pause")
				require.Nil(t, result.session)
			case <-time.After(10 * time.Second):
				t.Fatal("reader did not observe committed writer")
			}
			var sessions int64
			require.NoError(t, f.shared.Model(&models.ChatbotSession{}).Where("organization_id = ? AND contact_id = ? AND status = ?", work.OrganizationID, work.Contact.ID, models.SessionStatusActive).Count(&sessions).Error)
			require.Zero(t, sessions)
			require.Zero(t, f.model.calls.Load())
		})
	}
}
