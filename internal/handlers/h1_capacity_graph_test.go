package handlers

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// An independently committed session writer represents another process outside
// this root's local phase scheduler. It advances the generation after the first
// graph was prepared; stale apply must retry before any effect. The next inbound
// then executes the current graph exactly once, including both replay checks.
func TestH1CapacityGraph_CompetingPreparedSessionRetriesBeforeDuplicateEffect(t *testing.T) {
	env := h1HandoverFixture(t, h1HandoverOptions{})
	require.Empty(t, env.flow.TriggerKeywords, "the completed graph must not auto-trigger again on the older message retry")
	h1IngressResolver(t, env.shared)
	env.app.Redis = nil // Observe real preparation reads rather than a Redis hit.
	graph := models.JSONB{
		"version": 2, "entry_node": "m1",
		"nodes": []any{map[string]any{"id": "m1", "type": "message", "config": map[string]any{"message": "Synthetic graph reply"}}},
		"edges": []any{},
	}
	require.NoError(t, env.shared.Model(env.flow).Update("graph", graph).Error)
	require.NoError(t, env.shared.Model(env.session).Updates(map[string]any{"current_flow_id": env.flow.ID, "current_step": "m1"}).Error)
	secondMessage := inboundContinuationTextMessage(t, "wamid.capacity-graph-"+uuid.NewString(), env.work.Contact.PhoneNumber, "Synthetic second graph inbound")
	second, duplicate, err := env.app.persistIncomingMessageBeforeAck(env.account.PhoneID, secondMessage, "Synthetic same-contact graph")
	require.NoError(t, err)
	require.False(t, duplicate)
	require.Equal(t, env.work.Contact.ID, second.Contact.ID)
	var fillers []func()
	for range 2 {
		release, err := env.app.acquireInboundContinuationCapacity(context.Background(), nil, 3)
		require.NoError(t, err)
		fillers = append(fillers, release)
		t.Cleanup(release)
	}
	selected := []chan struct{}{make(chan struct{}), make(chan struct{})}
	allowRead := []chan struct{}{make(chan struct{}), make(chan struct{})}
	var releaseOnce sync.Once
	releaseReads := func() { releaseOnce.Do(func() { close(allowRead[0]); close(allowRead[1]) }) }
	var selectionCount atomic.Int32
	callback := "h1:capacity-graph-select:" + uuid.NewString()
	require.NoError(t, env.app.DB.Callback().Query().After("gorm:query").Register(callback, func(tx *gorm.DB) {
		if tx.Error != nil || tx.Statement.Schema == nil || tx.Statement.Schema.Table != "chatbot_flows" {
			return
		}
		index := int(selectionCount.Add(1)) - 1
		if index < 2 {
			close(selected[index])
			if index == 0 {
				<-allowRead[index]
			}
		}
	}))
	t.Cleanup(func() { _ = env.app.DB.Callback().Query().Remove(callback) })
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	done := []chan error{make(chan error, 1), make(chan error, 1)}
	var running sync.WaitGroup
	t.Cleanup(func() {
		releaseReads()
		cancel()
		for _, release := range fillers {
			release()
		}
		joined := make(chan struct{})
		go func() { running.Wait(); close(joined) }()
		select {
		case <-joined:
		case <-time.After(20 * time.Second):
			t.Error("same-contact graph continuations did not join")
		}
	})
	start := func(index int, work *persistedIncomingMessage) {
		running.Add(1)
		go func() {
			defer running.Done()
			processor := NewInboundContinuationProcessor(env.app, time.Second)
			done[index] <- processor.ProcessMessage(ctx, work.OrganizationID, work.Persisted.ID)
		}()
	}
	start(0, env.work)
	select {
	case <-selected[0]:
	case <-time.After(12 * time.Second):
		t.Fatal("first invocation did not select its active graph")
	}
	var beforeWriter models.ChatbotSession
	require.NoError(t, env.shared.Where("id = ? AND organization_id = ?", env.session.ID, env.org.ID).First(&beforeWriter).Error)
	writerDB, _ := h1OpenPool(t, 2)
	writer := h1RootApp(writerDB.WithContext(ctx), true)
	advanced, _, writerErr := writer.getOrCreateSession(env.org.ID, env.work.Contact.ID, env.account.Name, env.work.Contact.PhoneNumber, 30)
	require.NoError(t, writerErr)
	require.NotNil(t, advanced)
	require.Equal(t, beforeWriter.ID, advanced.ID)
	require.True(t, advanced.LastActivityAt.After(beforeWriter.LastActivityAt), "the existing UPDATE-fenced writer must commit a new generation")
	releaseReads()
	// The first apply is still queued without SQL while the original six
	// generic credits remain. A second same-root preparation is intentionally
	// not demanded while writer-mode apply has priority over those credits.
	h1CapacityWaitQueue(t, &env.app.continuationAdmission, 6, 1)
	for _, release := range fillers {
		release()
	}
	select {
	case err := <-done[0]:
		require.ErrorContains(t, err, "session")
		require.False(t, inboundContinuationRequiresManualReview(err))
		var policyStop *inboundContinuationPolicyStop
		require.NotErrorAs(t, err, &policyStop, "stale preparation must retry, not be consumed as a policy stop")
	case <-time.After(30 * time.Second):
		t.Fatal("older prepared graph did not settle")
	}
	start(1, second)
	select {
	case <-selected[1]:
	case <-time.After(12 * time.Second):
		t.Fatal("second invocation did not select the current graph after stale application retried")
	}
	h1CapacityJoin(t, done[1], 1)
	older := loadInboundContinuationJob(t, &App{DB: env.shared}, env.org.ID, env.work.Persisted.ID)
	newer := loadInboundContinuationJob(t, &App{DB: env.shared}, env.org.ID, second.Persisted.ID)
	require.Equal(t, models.ScheduledJobStatusPending, older.Status, "stale prepared generation retries; it must not be consumed as PolicyStop")
	require.Equal(t, 1, older.Attempts)
	require.NotEqual(t, true, older.Payload["manual_review_required"])
	require.Contains(t, older.LastError, "session")
	require.Equal(t, models.ScheduledJobStatusCompleted, newer.Status)
	require.EqualValues(t, 1, env.calls.Load())
	var oldActions int64
	require.NoError(t, env.shared.Model(&models.ScheduledJob{}).Where("organization_id = ? AND kind = ? AND aggregate_id = ?", env.org.ID, inboundContinuationActionJobKind, env.work.Persisted.ID).Count(&oldActions).Error)
	require.Zero(t, oldActions, "stale graph cannot claim or dispatch its old node")
	var oldInbound models.Message
	require.NoError(t, env.shared.Where("id = ? AND organization_id = ?", env.work.Persisted.ID, env.org.ID).First(&oldInbound).Error)
	require.NotEqual(t, true, oldInbound.Metadata["inbound_continuation_completed"])
	processor := NewInboundContinuationProcessor(env.app, time.Second)
	require.NoError(t, processor.ProcessMessage(ctx, env.org.ID, second.Persisted.ID))
	require.EqualValues(t, 1, env.calls.Load(), "completed new invocation cannot duplicate the graph send")
	processor.now = func() time.Time { return older.RunAt.Add(time.Second) }
	require.NoError(t, processor.ProcessMessage(ctx, env.org.ID, env.work.Persisted.ID))
	retried := loadInboundContinuationJob(t, &App{DB: env.shared}, env.org.ID, env.work.Persisted.ID)
	require.Equal(t, models.ScheduledJobStatusCompleted, retried.Status)
	require.EqualValues(t, 1, env.calls.Load(), "retry observes the now-completed graph instead of replaying a stale node")
	h1CapacityWaitQueue(t, &env.app.continuationAdmission, 0, 0)
}
