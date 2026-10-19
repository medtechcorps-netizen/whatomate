package handlers

// Candidate-only lifecycle regressions. Source proposal; not executed.
import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestH1Capacity_AcceptedResultRetainsLeaseAfterCallerCancellationWhileApplyQueued(t *testing.T) {
	f := h1CapacityLegacy(t, 1)
	work := f.works[0]
	processor := NewInboundContinuationProcessor(f.app, time.Second)
	processor.lease = 180 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	joined := make(chan struct{})
	go func() {
		defer close(joined)
		done <- processor.ProcessMessage(ctx, work.OrganizationID, work.Persisted.ID)
	}()
	var fillers []func()
	t.Cleanup(func() {
		f.model.unblock()
		cancel()
		for _, release := range fillers {
			release()
		}
		select {
		case <-joined:
		case <-time.After(15 * time.Second):
			t.Error("accepted-result continuation did not join")
		}
	})
	select {
	case <-f.model.entered:
	case <-time.After(12 * time.Second):
		t.Fatal("real model call did not enter")
	}
	// The real model owns one admission unit. Occupy the remaining seven with
	// actors that own no SQL, then allow the accepted model result to persist.
	for _, weight := range []int{3, 3, 1} {
		release, err := f.app.acquireInboundContinuationCapacity(context.Background(), nil, weight)
		require.NoError(t, err)
		fillers = append(fillers, release)
	}
	f.model.unblock()
	h1CapacityWaitQueue(t, &f.app.continuationAdmission, 7, 1)
	var action models.ScheduledJob
	require.NoError(t, f.shared.Where("organization_id = ? AND kind = ? AND aggregate_id = ? AND payload->>'effect_kind' = ?", work.OrganizationID, inboundContinuationActionJobKind, work.Persisted.ID, "legacy_ai_generate").First(&action).Error)
	require.Equal(t, inboundActionStateResolved, action.Payload["state"])
	initial := loadInboundContinuationJob(t, &App{DB: f.shared}, work.OrganizationID, work.Persisted.ID)
	require.NotNil(t, initial.LockedAt)
	cancel()
	// Positive evidence beyond the original lease, not merely a short sleep or
	// the absence of a competing model request. The caller has ended, but this
	// invocation still owns a captured answer and must maintain its lease.
	var renewed models.ScheduledJob
	require.Eventually(t, func() bool {
		if err := f.shared.Where("id = ? AND organization_id = ?", initial.ID, work.OrganizationID).First(&renewed).Error; err != nil {
			return false
		}
		return renewed.LockedAt != nil && renewed.LockedAt.After(initial.LockedAt.Add(processor.lease))
	}, 3*time.Second, 10*time.Millisecond)
	require.Equal(t, initial.LockedBy, renewed.LockedBy)
	require.Equal(t, models.ScheduledJobStatusProcessing, renewed.Status)
	// A separate root models another process's independent admission gate.
	// It uses the same private application pool and actual claim path, never
	// the fixture observer pool. Its callback would be observable if it stole
	// the lease; correct code cannot enter it.
	competingRoot := &App{Config: f.app.Config, DB: f.app.DB, Log: f.app.Log, Redis: f.app.Redis, WhatsApp: f.app.WhatsApp, HTTPClient: f.app.HTTPClient}
	competitor := NewInboundContinuationProcessor(competingRoot, time.Second)
	competitor.lease = processor.lease
	var stolen atomic.Bool
	competitor.process = func(context.Context, *App, *persistedIncomingMessage) error { stolen.Store(true); return nil }
	competingCtx, cancelCompeting := context.WithTimeout(context.Background(), time.Second)
	require.NoError(t, competitor.ProcessMessage(competingCtx, work.OrganizationID, work.Persisted.ID))
	cancelCompeting()
	require.False(t, stolen.Load())
	stillOwned := loadInboundContinuationJob(t, &App{DB: f.shared}, work.OrganizationID, work.Persisted.ID)
	require.Equal(t, initial.LockedBy, stillOwned.LockedBy)
	require.Equal(t, initial.Attempts, stillOwned.Attempts)
	for _, release := range fillers {
		release()
	}
	h1CapacityJoin(t, done, 1)
	f.assertGenerated(t, 0)
	require.EqualValues(t, 1, f.model.calls.Load())
	f.mu.Lock()
	sends := len(f.sent)
	f.mu.Unlock()
	require.Equal(t, 1, sends)
	h1CapacityWaitQueue(t, &f.app.continuationAdmission, 0, 0)
}

// Hold the actual second outbound-account lock read, after the independently
// committed dispatch marker and before the physical send. This is later than
// the apply and action-entry ownership checks. A real claim replacement and
// failed heartbeat must deny this still-unstarted provider call.
func TestH1Capacity_LeaseLostDuringProviderPreflightDoesNotSend(t *testing.T) {
	f := h1CapacityLegacy(t, 1)
	work := f.works[0]
	entered, allowProvider := make(chan struct{}), make(chan struct{})
	heartbeatLost := make(chan struct{})
	var held, lost atomic.Bool
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(allowProvider) }) }
	queryCallback := "h1:capacity-provider-owner:" + uuid.NewString()
	require.NoError(t, f.app.DB.Callback().Query().After("gorm:query").Register(queryCallback, func(tx *gorm.DB) {
		if tx.Error != nil || tx.Statement.Schema == nil || tx.Statement.Schema.Table != "whatsapp_accounts" ||
			!strings.Contains(tx.Statement.SQL.String(), "FOR SHARE") || held.Load() {
			return
		}
		var dispatching int64
		if err := f.shared.Model(&models.Message{}).Where("organization_id = ? AND contact_id = ? AND direction = ? AND metadata->>? = ?",
			work.OrganizationID, work.Contact.ID, models.DirectionOutgoing, automaticAIDispatchStateMetadataKey, automaticAIDispatchStateDispatching).Count(&dispatching).Error; err != nil || dispatching != 1 {
			return
		}
		if held.CompareAndSwap(false, true) {
			close(entered)
			<-allowProvider
		}
	}))
	t.Cleanup(func() { _ = f.app.DB.Callback().Query().Remove(queryCallback) })
	updateCallback := "h1:capacity-lost-heartbeat:" + uuid.NewString()
	require.NoError(t, f.app.DB.Callback().Update().After("gorm:update").Register(updateCallback, func(tx *gorm.DB) {
		if tx.Error != nil || tx.Statement.Schema == nil || tx.Statement.Schema.Table != "scheduled_jobs" || tx.RowsAffected != 0 {
			return
		}
		updates, ok := tx.Statement.Dest.(map[string]any)
		if !ok || updates["locked_at"] == nil {
			return
		}
		if lost.CompareAndSwap(false, true) {
			close(heartbeatLost)
		}
	}))
	t.Cleanup(func() { _ = f.app.DB.Callback().Update().Remove(updateCallback) })
	processor := NewInboundContinuationProcessor(f.app, time.Second)
	processor.lease = 180 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	done, joined := make(chan error, 1), make(chan struct{})
	t.Cleanup(func() {
		release()
		f.model.unblock()
		cancel()
		select {
		case <-joined:
		case <-time.After(15 * time.Second):
			t.Error("provider-preflight continuation did not join")
		}
	})
	f.model.unblock()
	go func() {
		defer close(joined)
		done <- processor.ProcessMessage(ctx, work.OrganizationID, work.Persisted.ID)
	}()
	select {
	case <-entered:
	case <-time.After(15 * time.Second):
		t.Fatal("actual provider preflight did not reach the post-claim account lock")
	}
	initial := loadInboundContinuationJob(t, &App{DB: f.shared}, work.OrganizationID, work.Persisted.ID)
	require.Equal(t, models.ScheduledJobStatusProcessing, initial.Status)
	require.NotEmpty(t, initial.LockedBy)
	replacement := "synthetic-replacement-" + uuid.NewString()
	changed := f.shared.Model(&models.ScheduledJob{}).Where("id = ? AND organization_id = ? AND status = ? AND locked_by = ?",
		initial.ID, work.OrganizationID, models.ScheduledJobStatusProcessing, initial.LockedBy).
		Updates(map[string]any{"locked_by": replacement, "locked_at": time.Now().UTC(), "version": gorm.Expr("version + 1")})
	require.NoError(t, changed.Error)
	require.EqualValues(t, 1, changed.RowsAffected, "the original owner must actually lose its durable claim")
	select {
	case <-heartbeatLost:
	case <-time.After(3 * time.Second):
		t.Fatal("original heartbeat did not observe the replacement claim")
	}
	f.mu.Lock()
	before := len(f.sent)
	f.mu.Unlock()
	require.Zero(t, before, "barrier is before the physical provider request")
	release()
	select {
	case err := <-done:
		require.Error(t, err)
		require.True(t, inboundContinuationRequiresManualReview(err), "the durable dispatch marker already exists, so the existing conservative action outcome remains manual review")
	case <-time.After(15 * time.Second):
		t.Fatal("ownership loss did not settle")
	}
	f.mu.Lock()
	after := len(f.sent)
	f.mu.Unlock()
	require.Zero(t, after, "an old worker cannot send after the provider preflight wait")
	require.EqualValues(t, 1, f.model.calls.Load())
	var outgoing []models.Message
	require.NoError(t, f.shared.Where("organization_id = ? AND contact_id = ? AND direction = ?", work.OrganizationID, work.Contact.ID, models.DirectionOutgoing).Find(&outgoing).Error)
	require.Len(t, outgoing, 1)
	require.Empty(t, outgoing[0].WhatsAppMessageID)
	require.Equal(t, automaticAIDispatchStateRejected, outgoing[0].Metadata[automaticAIDispatchStateMetadataKey], "known unstarted send is settled, never left dispatching or called accepted")
	require.Equal(t, models.MessageStatusFailed, outgoing[0].Status)
	var textAction models.ScheduledJob
	require.NoError(t, f.shared.Where("organization_id = ? AND kind = ? AND aggregate_id = ? AND payload->>'effect_kind' = ?", work.OrganizationID, inboundContinuationActionJobKind, work.Persisted.ID, "text").First(&textAction).Error)
	require.Equal(t, inboundActionStateManualReview, textAction.Payload["state"], "a committed outgoing dispatch boundary is not automatically reclaimed")
	owner := loadInboundContinuationJob(t, &App{DB: f.shared}, work.OrganizationID, work.Persisted.ID)
	require.Equal(t, replacement, owner.LockedBy, "the old worker cannot settle the replacement's parent claim")
	require.Equal(t, models.ScheduledJobStatusProcessing, owner.Status)
	var ai models.ScheduledJob
	require.NoError(t, f.shared.Where("organization_id = ? AND kind = ? AND aggregate_id = ? AND payload->>'effect_kind' = ?", work.OrganizationID, inboundContinuationActionJobKind, work.Persisted.ID, "legacy_ai_generate").First(&ai).Error)
	require.Equal(t, inboundActionStateResolved, ai.Payload["state"], "already accepted computation remains durable despite later ownership loss")
	require.Equal(t, "Generated: "+f.texts[0], inboundContinuationActionResult(ai.Payload)["answer"])
}

func TestH1Capacity_LeaseLossAfterAcceptedSendPreservesIndependentResult(t *testing.T) {
	f := h1CapacityLegacy(t, 1)
	work := f.works[0]
	accepted, allowSettlement := make(chan struct{}), make(chan struct{})
	heartbeatLost := make(chan struct{})
	var held, lost atomic.Bool
	var once sync.Once
	release := func() { once.Do(func() { close(allowSettlement) }) }
	callback := "h1:capacity-accepted-settlement:" + uuid.NewString()
	require.NoError(t, f.app.DB.Callback().Update().Before("gorm:update").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Schema == nil || tx.Statement.Schema.Table != "messages" || held.Load() {
			return
		}
		updates, ok := tx.Statement.Dest.(map[string]any)
		if !ok {
			return
		}
		metadata, ok := updates["metadata"].(models.JSONB)
		if !ok || metadata[automaticAIDispatchStateMetadataKey] != automaticAIDispatchStateResolved || updates["whats_app_message_id"] == "" {
			return
		}
		if held.CompareAndSwap(false, true) {
			close(accepted)
			<-allowSettlement
		}
	}))
	t.Cleanup(func() { _ = f.app.DB.Callback().Update().Remove(callback) })
	heartbeatCallback := "h1:capacity-settlement-heartbeat:" + uuid.NewString()
	require.NoError(t, f.app.DB.Callback().Update().After("gorm:update").Register(heartbeatCallback, func(tx *gorm.DB) {
		if tx.Error != nil || tx.Statement.Schema == nil || tx.Statement.Schema.Table != "scheduled_jobs" || tx.RowsAffected != 0 {
			return
		}
		updates, ok := tx.Statement.Dest.(map[string]any)
		if ok && updates["locked_at"] != nil && lost.CompareAndSwap(false, true) {
			close(heartbeatLost)
		}
	}))
	t.Cleanup(func() { _ = f.app.DB.Callback().Update().Remove(heartbeatCallback) })
	processor := NewInboundContinuationProcessor(f.app, time.Second)
	processor.lease = 180 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	done, joined := make(chan error, 1), make(chan struct{})
	t.Cleanup(func() {
		release()
		f.model.unblock()
		cancel()
		select {
		case <-joined:
		case <-time.After(15 * time.Second):
			t.Error("accepted-send settlement did not join")
		}
	})
	f.model.unblock()
	go func() {
		defer close(joined)
		done <- processor.ProcessMessage(ctx, work.OrganizationID, work.Persisted.ID)
	}()
	select {
	case <-accepted:
	case <-time.After(15 * time.Second):
		t.Fatal("successful provider result did not reach independent settlement")
	}
	f.mu.Lock()
	sent := append([]h1CapacitySent(nil), f.sent...)
	f.mu.Unlock()
	require.Len(t, sent, 1)
	require.Equal(t, "Generated: "+f.texts[0], sent[0].Body)
	initial := loadInboundContinuationJob(t, &App{DB: f.shared}, work.OrganizationID, work.Persisted.ID)
	replacement := "synthetic-post-send-owner-" + uuid.NewString()
	changed := f.shared.Model(&models.ScheduledJob{}).Where("id = ? AND organization_id = ? AND status = ? AND locked_by = ?", initial.ID, work.OrganizationID, models.ScheduledJobStatusProcessing, initial.LockedBy).
		Updates(map[string]any{"locked_by": replacement, "locked_at": time.Now().UTC(), "version": gorm.Expr("version + 1")})
	require.NoError(t, changed.Error)
	require.EqualValues(t, 1, changed.RowsAffected)
	select {
	case <-heartbeatLost:
	case <-time.After(3 * time.Second):
		t.Fatal("heartbeat did not lose the old claim while accepted settlement was pending")
	}
	release()
	select {
	case err := <-done:
		require.Error(t, err, "the original worker must not complete a replacement claim")
	case <-time.After(15 * time.Second):
		t.Fatal("accepted result was not settled after ownership loss")
	}
	var outgoing []models.Message
	require.NoError(t, f.shared.Where("organization_id = ? AND contact_id = ? AND direction = ?", work.OrganizationID, work.Contact.ID, models.DirectionOutgoing).Find(&outgoing).Error)
	require.Len(t, outgoing, 1)
	require.Equal(t, sent[0].WAMID, outgoing[0].WhatsAppMessageID)
	require.Equal(t, automaticAIDispatchStateResolved, outgoing[0].Metadata[automaticAIDispatchStateMetadataKey])
	require.Equal(t, models.MessageStatusSent, outgoing[0].Status)
	var textAction models.ScheduledJob
	require.NoError(t, f.shared.Where("organization_id = ? AND kind = ? AND aggregate_id = ? AND payload->>'effect_kind' = ?", work.OrganizationID, inboundContinuationActionJobKind, work.Persisted.ID, "text").First(&textAction).Error)
	require.Equal(t, inboundActionStateResolved, textAction.Payload["state"], "accepted send remains replayable even after its parent lease is lost")
	require.Equal(t, outgoing[0].ID.String(), inboundContinuationActionResult(textAction.Payload)["outgoing_message_id"])
	owner := loadInboundContinuationJob(t, &App{DB: f.shared}, work.OrganizationID, work.Persisted.ID)
	require.Equal(t, replacement, owner.LockedBy)
	require.Equal(t, models.ScheduledJobStatusProcessing, owner.Status)
	f.mu.Lock()
	sends := len(f.sent)
	f.mu.Unlock()
	require.Equal(t, 1, sends)
}

func TestH1Capacity_NativeSelectionFailurePrecedesPreparedRecovery(t *testing.T) {
	app := &App{Log: testutil.NopLogger(), inboundContinuation: &inboundContinuationExecution{
		OrganizationID: uuid.New(), ContactID: uuid.New(), MessageID: uuid.New(), WAMID: "wamid.synthetic-selection-error",
	}}
	account := &models.WhatsAppAccount{BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: app.inboundContinuation.OrganizationID}
	contact := &models.Contact{BaseModel: models.BaseModel{ID: app.inboundContinuation.ContactID}, OrganizationID: app.inboundContinuation.OrganizationID}
	session := &models.ChatbotSession{BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: account.OrganizationID, ContactID: contact.ID}
	restore := app.pushInboundContinuationActionScope("native-booking:" + session.ID.String() + ":" + app.inboundContinuation.actionScope)
	identity, err := app.nextInboundContinuationActionKey(nativeBookingEffect, nil)
	require.NoError(t, err)
	restore()
	sentinel := errors.New("synthetic selection transaction failed after loading a prepared row")
	selection := &nativeBookingSelection{key: identity.Key, loadErr: sentinel, existing: &nativeBookingPrepared{Claim: &inboundContinuationActionClaim{Execute: false}}}
	handled, awaiting, err := app.processNativeAIBookingSelected(account, contact, session, nil, "Synthetic selection failure", selection)
	require.True(t, handled)
	require.False(t, awaiting)
	require.ErrorIs(t, err, sentinel, "a read/commit failure must be returned before dispatch or recovery is attempted")
	require.False(t, app.inboundContinuation.nativeBookingCheckpoint, "failed selection must not install a usable native checkpoint")
}
