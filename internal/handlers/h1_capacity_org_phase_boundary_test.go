package handlers

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/config"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func h1OrgAssertActive(t *testing.T, app *App, org uuid.UUID, mode, active, used int) {
	t.Helper()
	g := &app.rootApp().continuationAdmission
	g.mu.Lock()
	defer g.mu.Unlock()
	phase := g.phases[org.String()]
	require.NotNil(t, phase)
	require.Equal(t, mode, phase.activeMode)
	require.Equal(t, active, phase.active)
	require.Equal(t, used, g.used)
}

func TestH1OrgPhase_ProcessorModeWaitPrecedesClaimAndSQL(t *testing.T) {
	for _, entry := range []string{"ProcessMessage", "RunOnce"} {
		t.Run(entry, func(t *testing.T) {
			f := h1CapacityLegacy(t, 1)
			work := f.works[0]
			processor := NewInboundContinuationProcessor(f.app, time.Second)
			var negativeHint *atomic.Int32
			if entry == "ProcessMessage" {
				negativeHint = h1FastHintNegative(t, f.app)
			}
			org := work.OrganizationID
			if entry == "RunOnce" {
				// Use the fixture tenant's real organization query; even an existing
				// session must wait for initial Reader admission before claim SQL.
				h1RecoveryOnlyFixtureOrganization(t, f)
				organizations, err := processor.listOrganizations(context.Background())
				require.NoError(t, err)
				require.NotEmpty(t, organizations)
				require.Equal(t, []uuid.UUID{work.OrganizationID}, organizations)
				org = organizations[0]
			}
			hold := h1OrgHold(t, &f.app.continuationAdmission, 1, inboundContinuationPerformPriority, func() int { return 10 }, org.String(), inboundContinuationWriter)
			before := loadInboundContinuationJob(t, &App{DB: f.shared}, work.OrganizationID, work.Persisted.ID)
			var jobStatements atomic.Int32
			callback := "h1:org-preclaim:" + uuid.NewString()
			observe := func(tx *gorm.DB) {
				if tx.Statement.Schema != nil && tx.Statement.Schema.Table == "scheduled_jobs" {
					jobStatements.Add(1)
				}
			}
			require.NoError(t, f.app.DB.Callback().Query().Before("gorm:query").Register(callback, observe))
			require.NoError(t, f.app.DB.Callback().Update().Before("gorm:update").Register(callback, observe))
			t.Cleanup(func() {
				_ = f.app.DB.Callback().Query().Remove(callback)
				_ = f.app.DB.Callback().Update().Remove(callback)
			})
			ctx, cancel := context.WithCancel(context.Background())
			done, joined := make(chan error, 1), make(chan struct{})
			t.Cleanup(func() {
				cancel()
				hold()
				f.model.unblock()
				select {
				case <-joined:
				case <-time.After(15 * time.Second):
					t.Error("mode-queued processor did not join")
				}
			})
			go func() {
				defer close(joined)
				if entry == "RunOnce" {
					done <- processor.RunOnce(ctx)
				} else {
					done <- processor.ProcessMessage(ctx, work.OrganizationID, work.Persisted.ID)
				}
			}()
			h1CapacityWaitQueue(t, &f.app.continuationAdmission, 1, 1)
			g := &f.app.continuationAdmission
			g.mu.Lock()
			queuedOrg, queuedMode := g.waiters[0].organization, g.waiters[0].mode
			g.mu.Unlock()
			require.Equal(t, org.String(), queuedOrg)
			wantMode := inboundContinuationReader
			require.Equal(t, wantMode, queuedMode)
			if negativeHint != nil {
				require.EqualValues(t, 1, negativeHint.Load(), "exercise the explicit advisory-negative Reader boundary")
			}
			require.Zero(t, f.pool.Stats().InUse, "an incompatible mode wait owns no application SQL connection")
			require.Zero(t, jobStatements.Load(), "even the first claim statement must wait for organization admission")
			require.Zero(t, f.model.calls.Load())
			require.Equal(t, before, loadInboundContinuationJob(t, &App{DB: f.shared}, work.OrganizationID, work.Persisted.ID))
			cancel()
			select {
			case err := <-done:
				require.ErrorIs(t, err, context.Canceled)
			case <-time.After(5 * time.Second):
				t.Fatal("mode wait did not honor caller cancellation")
			}
			hold()
			h1OrgEmpty(t, g)
			require.Equal(t, before, loadInboundContinuationJob(t, &App{DB: f.shared}, work.OrganizationID, work.Persisted.ID))
			f.model.unblock()
			h1CapacityJoin(t, f.start(t, 0), 1)
			f.assertGenerated(t, 0)
		})
	}
}

func TestH1OrgPhase_IngressDiscoveryReleasesSQLAndRevalidatesTenantBinding(t *testing.T) {
	for _, route := range []string{"classic", "coexistence"} {
		t.Run(route, func(t *testing.T) {
			app, shared, pool, account := h1IngressFixture(t, route == "coexistence")
			orgB, accountB := createProcessorTestOrg(t, &App{DB: shared})
			beforeA := h1IngressCounts(t, shared, account.OrganizationID)
			beforeB := h1IngressCounts(t, shared, orgB.ID)
			hold := h1OrgHold(t, &app.continuationAdmission, 1, inboundContinuationPerformPriority, func() int { return 10 }, account.OrganizationID.String(), inboundContinuationReader)
			message := inboundContinuationTextMessage(t, "wamid.org-binding-"+uuid.NewString(), coexistenceAdmissionTestPhone(), "Synthetic phase binding")
			type result struct {
				work      *persistedIncomingMessage
				duplicate bool
				err       error
			}
			persist := func() result {
				var r result
				if route == "coexistence" {
					r.work, r.duplicate, r.err = app.persistAuthenticatedIncomingMessageBeforeAck(account.PhoneID, message, "Synthetic binding", strings.Repeat("a", 64))
				} else {
					r.work, r.duplicate, r.err = app.persistIncomingMessageBeforeAck(account.PhoneID, message, "Synthetic binding")
				}
				return r
			}
			done, joined := make(chan result, 1), make(chan struct{})
			t.Cleanup(func() {
				hold()
				select {
				case <-joined:
				case <-time.After(30 * time.Second):
					t.Error("discovery/scoped ingress did not join")
				}
				h1IngressJoin(t, app)
			})
			go func() { defer close(joined); done <- persist() }()
			require.Eventually(t, func() bool {
				g := &app.continuationAdmission
				g.mu.Lock()
				defer g.mu.Unlock()
				return g.used == 1 && len(g.waiters) == 1 && g.waiters[0].organization == account.OrganizationID.String() &&
					g.waiters[0].mode == inboundContinuationWriter && pool.Stats().InUse == 0
			}, 5*time.Second, time.Millisecond, "discovery must release its credit and real SQL before the scoped writer wait")
			require.Equal(t, beforeA, h1IngressCounts(t, shared, account.OrganizationID))
			require.Equal(t, beforeB, h1IngressCounts(t, shared, orgB.ID))
			// Simulate an independently committed authority change after discovery.
			// The admitted operation must fail in its original tenant, never retarget B.
			require.NoError(t, shared.Model(&models.WhatsAppAccount{}).Where("id = ?", account.ID).Update("phone_id", account.PhoneID+"-retired").Error)
			require.NoError(t, shared.Model(&models.WhatsAppAccount{}).Where("id = ?", accountB.ID).Update("phone_id", account.PhoneID).Error)
			hold()
			select {
			case got := <-done:
				require.Error(t, got.err)
				require.Nil(t, got.work)
				require.False(t, got.duplicate)
			case <-time.After(10 * time.Second):
				t.Fatal("changed discovered binding did not fail closed")
			}
			h1OrgEmpty(t, &app.continuationAdmission)
			require.Equal(t, beforeA, h1IngressCounts(t, shared, account.OrganizationID))
			require.Equal(t, beforeB, h1IngressCounts(t, shared, orgB.ID))
			require.NoError(t, shared.Model(&models.WhatsAppAccount{}).Where("id = ?", accountB.ID).Update("phone_id", accountB.PhoneID).Error)
			require.NoError(t, shared.Model(&models.WhatsAppAccount{}).Where("id = ?", account.ID).Update("phone_id", account.PhoneID).Error)
			accepted := persist()
			require.NoError(t, accepted.err)
			require.NotNil(t, accepted.work)
			require.Equal(t, account.OrganizationID, accepted.work.OrganizationID)
			require.False(t, accepted.duplicate)
			after := h1IngressCounts(t, shared, account.OrganizationID)
			replayed := persist()
			require.NoError(t, replayed.err)
			require.True(t, replayed.duplicate)
			require.Equal(t, after, h1IngressCounts(t, shared, account.OrganizationID))
			require.Equal(t, beforeB, h1IngressCounts(t, shared, orgB.ID))
		})
	}
}

func TestH1OrgPhase_DiscoveryQueryAndWriterQueueShareOriginalDeadline(t *testing.T) {
	app, shared, pool, account := h1IngressFixture(t, false)
	queryContext := make(chan context.Context, 1)
	callback := "h1:org-discovery-context:" + uuid.NewString()
	require.NoError(t, app.DB.Callback().Row().Before("gorm:row").Register(callback, func(tx *gorm.DB) {
		if strings.Contains(tx.Statement.SQL.String(), "rereply_resolve_whatsapp_org") {
			select {
			case queryContext <- tx.Statement.Context:
			default:
			}
		}
	}))
	t.Cleanup(func() { _ = app.DB.Callback().Row().Remove(callback) })
	hold := h1OrgHold(t, &app.continuationAdmission, 1, inboundContinuationPerformPriority, func() int { return 10 }, account.OrganizationID.String(), inboundContinuationReader)
	lockCtx, cancelLock := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelLock()
	tx := shared.WithContext(lockCtx).Begin()
	require.NoError(t, tx.Error)
	var once sync.Once
	releaseTable := func() { once.Do(func() { _ = tx.Rollback().Error }) }
	t.Cleanup(releaseTable)
	require.NoError(t, tx.Exec("LOCK TABLE whatsapp_accounts IN ACCESS EXCLUSIVE MODE").Error)
	type result struct {
		organization uuid.UUID
		release      func()
		err          error
	}
	done, joined := make(chan result, 1), make(chan struct{})
	started := time.Now()
	t.Cleanup(func() {
		releaseTable()
		hold()
		select {
		case <-joined:
		case <-time.After(30 * time.Second):
			t.Error("bounded discovery did not join")
		}
		select {
		case pending := <-done:
			if pending.release != nil {
				pending.release()
			}
		default:
		}
	})
	go func() {
		defer close(joined)
		org, release, err := app.acquireIncomingMessageOrganization(account.PhoneID)
		done <- result{org, release, err}
	}()
	var actualQueryContext context.Context
	select {
	case actualQueryContext = <-queryContext:
	case <-time.After(5 * time.Second):
		t.Fatal("actual discovery query context was not observed")
	}
	deadline, bounded := actualQueryContext.Deadline()
	require.True(t, bounded, "the actual root SQL call must carry the ingress deadline")
	require.WithinDuration(t, started.Add(20*time.Second), deadline, time.Second)
	require.Eventually(t, func() bool {
		var waiting int64
		err := shared.Raw("SELECT count(*) FROM pg_stat_activity WHERE application_name='h1_pool_bound_test' AND wait_event_type='Lock' AND query LIKE '%rereply_resolve_whatsapp_org%'").Scan(&waiting).Error
		return err == nil && waiting == 1
	}, 5*time.Second, 10*time.Millisecond, "the actual root discovery SQL must wait on a real database lock")
	h1CapacityWaitQueue(t, &app.continuationAdmission, 2, 0)
	// Spend twelve seconds in the real discovery query, then in the scoped
	// writer queue. Resetting a new 20s budget after discovery would exceed 28s.
	<-time.After(time.Until(started.Add(12 * time.Second)))
	releaseTable()
	require.Eventually(t, func() bool {
		u, w := h1CapacityQueueState(&app.continuationAdmission)
		return u == 1 && w == 1 && pool.Stats().InUse == 0
	}, 4*time.Second, 10*time.Millisecond)
	select {
	case got := <-done:
		if got.release != nil {
			got.release()
		}
		require.ErrorIs(t, got.err, context.DeadlineExceeded)
		require.Equal(t, uuid.Nil, got.organization)
		require.Nil(t, got.release)
	case <-time.After(time.Until(started.Add(28 * time.Second))):
		t.Fatal("discovery and scoped admission reset the original 20s budget")
	}
	require.ErrorIs(t, actualQueryContext.Err(), context.DeadlineExceeded)
	require.GreaterOrEqual(t, time.Since(started), 19*time.Second)
	require.Less(t, time.Since(started), 28*time.Second)
	hold()
	h1OrgEmpty(t, &app.continuationAdmission)
	require.Eventually(t, func() bool { return pool.Stats().InUse == 0 }, 2*time.Second, time.Millisecond)
}

func TestH1OrgPhase_RealGeneratedGraphAndNativeDispatchKeepSingleApplyMode(t *testing.T) {
	for _, route := range []string{"generated-legacy", "graph", "native-confirmation"} {
		t.Run(route, func(t *testing.T) {
			var app *App
			var shared *gorm.DB
			var work *persistedIncomingMessage
			wantMode := inboundContinuationWriter
			wantWeight := 3
			var verify func()
			switch route {
			case "generated-legacy":
				f := h1CapacityLegacy(t, 1)
				app, shared, work = f.app, f.shared, f.works[0]
				wantMode = inboundContinuationReader
				wantWeight = 2
				f.model.unblock()
				verify = func() { f.assertGenerated(t, 0) }
			case "graph":
				env := h1HandoverFixture(t, h1HandoverOptions{})
				app, shared, work = env.app, env.shared, env.work
				app.Redis = nil
				graph := models.JSONB{"version": 2, "entry_node": "m1", "nodes": []any{map[string]any{"id": "m1", "type": "message", "config": map[string]any{"message": "Synthetic scoped graph reply"}}}, "edges": []any{}}
				require.NoError(t, shared.Model(env.flow).Update("graph", graph).Error)
				require.NoError(t, shared.Model(env.session).Updates(map[string]any{"current_flow_id": env.flow.ID, "current_step": "m1"}).Error)
				verify = func() { require.EqualValues(t, 1, env.calls.Load()) }
			case "native-confirmation":
				f := nativeBookingFixture(t)
				_, offer := f.offer(t, true)
				require.NotEmpty(t, offer.Choices)
				app, shared = f.app, f.app.DB
				app.Redis = nil
				settings := &models.ChatbotSettings{BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: f.org.ID, WhatsAppAccount: f.account.Name, IsEnabled: true, SessionTimeoutMins: 30}
				require.NoError(t, shared.Create(settings).Error)
				app.inboundContinuation = nil
				message := inboundContinuationTextMessage(t, "wamid.org-native-"+uuid.NewString(), f.contact.PhoneNumber, "BOOK "+offer.Choices[0].Token)
				var duplicate bool
				var err error
				work, duplicate, err = app.persistIncomingMessageBeforeAck(f.account.PhoneID, message, "Synthetic scoped native confirmation")
				require.NoError(t, err)
				require.False(t, duplicate)
				require.Equal(t, f.contact.ID, work.Contact.ID)
				private, _ := h1OpenPool(t, 10)
				app.DB = private
				app.Config = &config.Config{Database: config.DatabaseConfig{RLSEnabled: true}}
				calls := nativePausePolicyProvider(t, app, nil)
				verify = func() {
					require.EqualValues(t, 1, calls.Load())
					var bookings int64
					require.NoError(t, shared.Model(&models.Booking{}).Where("organization_id = ?", f.org.ID).Count(&bookings).Error)
					require.EqualValues(t, 1, bookings)
				}
			}
			entered, allow := make(chan struct{}), make(chan struct{})
			var held atomic.Bool
			var once sync.Once
			releaseProvider := func() { once.Do(func() { close(allow) }) }
			callback := "h1:org-provider-mode:" + uuid.NewString()
			require.NoError(t, app.DB.Callback().Query().After("gorm:query").Register(callback, func(tx *gorm.DB) {
				if tx.Error != nil || tx.Statement.Schema == nil || tx.Statement.Schema.Table != "whatsapp_accounts" || !strings.Contains(tx.Statement.SQL.String(), "FOR SHARE") || held.Load() {
					return
				}
				var dispatching int64
				if err := shared.Model(&models.Message{}).Where("organization_id = ? AND contact_id = ? AND direction = ? AND metadata->>? = ?", work.OrganizationID, work.Contact.ID, models.DirectionOutgoing, automaticAIDispatchStateMetadataKey, automaticAIDispatchStateDispatching).Count(&dispatching).Error; err != nil || dispatching != 1 {
					return
				}
				if held.CompareAndSwap(false, true) {
					close(entered)
					<-allow
				}
			}))
			t.Cleanup(func() { _ = app.DB.Callback().Query().Remove(callback) })
			processor := NewInboundContinuationProcessor(app, time.Second)
			require.Nil(t, processor.process, "exercise the ordinary production phase path")
			ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
			done, joined := make(chan error, 1), make(chan struct{})
			t.Cleanup(func() {
				releaseProvider()
				cancel()
				select {
				case <-joined:
				case <-time.After(15 * time.Second):
					t.Error("actual apply route did not join")
				}
				h1IngressJoin(t, app)
			})
			go func() {
				defer close(joined)
				done <- processor.ProcessMessage(ctx, work.OrganizationID, work.Persisted.ID)
			}()
			select {
			case <-entered:
			case <-time.After(15 * time.Second):
				t.Fatal("actual prepared route did not reach its independently committed provider boundary")
			}
			h1OrgAssertActive(t, app, work.OrganizationID, wantMode, 1, wantWeight)
			opposite := inboundContinuationReader
			if wantMode == inboundContinuationReader {
				compatible := h1OrgHold(t, &app.continuationAdmission, 1, inboundContinuationPerformPriority, func() int { return 10 }, work.OrganizationID.String(), inboundContinuationReader)
				h1OrgAssertActive(t, app, work.OrganizationID, wantMode, 2, wantWeight+1)
				compatible()
				opposite = inboundContinuationWriter
			}
			waiting := h1OrgAcquireAsync(t, &app.continuationAdmission, context.Background(), nil, 1, inboundContinuationFreshPriority, func() int { return 10 }, work.OrganizationID.String(), opposite)
			h1CapacityWaitQueue(t, &app.continuationAdmission, wantWeight, 1)
			h1OrgBlocked(t, waiting)
			releaseProvider()
			h1CapacityJoin(t, done, 1)
			later := h1OrgGrant(t, waiting)
			later()
			h1OrgEmpty(t, &app.continuationAdmission)
			job := loadInboundContinuationJob(t, &App{DB: shared}, work.OrganizationID, work.Persisted.ID)
			require.Equal(t, models.ScheduledJobStatusCompleted, job.Status)
			require.NotEqual(t, true, job.Payload["manual_review_required"])
			var actions []models.ScheduledJob
			require.NoError(t, shared.Where("organization_id = ? AND kind = ? AND aggregate_id = ?", work.OrganizationID, inboundContinuationActionJobKind, work.Persisted.ID).Find(&actions).Error)
			require.NotEmpty(t, actions)
			for _, action := range actions {
				require.Equal(t, inboundActionStateResolved, action.Payload["state"])
			}
			verify()
			require.NoError(t, processor.ProcessMessage(ctx, work.OrganizationID, work.Persisted.ID))
			verify()
		})
	}
}
