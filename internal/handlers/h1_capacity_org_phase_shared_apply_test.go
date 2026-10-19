package handlers

// Source proposal only. A real independently owned organization KEY SHARE
// guard spans generated apply and replay. A scheduler permit alone cannot
// satisfy this oracle. No test runs in parallel or contacts a real provider.
import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/stretchr/testify/require"
)

func TestH1OrgPhase_GeneratedApplyCompletesUnderOtherContactPhysicalGuard(t *testing.T) {
	f := h1CapacityLegacy(t, 1)
	work := f.works[0]
	// Create the other durable contact before the model starts. Discovery and
	// contact INSERT are writers; they must not be smuggled into shared apply.
	seed := h1RootApp(f.shared, false)
	holder := h1CapacityReaderPersist(t, seed, &work.Account, "shared-apply-holder")
	require.Equal(t, work.OrganizationID, holder.OrganizationID)
	require.NotEqual(t, work.Contact.ID, holder.Contact.ID)

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	t.Cleanup(cancel)
	f.shared = f.shared.WithContext(ctx)
	externalDB, externalPool := h1OpenPool(t, 3)
	external := h1RootApp(externalDB.WithContext(ctx), true)
	execution := external.scopedApp(external.DB, holder.OrganizationID)
	execution.tenantOrgID = uuid.Nil
	execution.inboundContinuation = &inboundContinuationExecution{
		OrganizationID: holder.OrganizationID, ContactID: holder.Contact.ID,
		MessageID: holder.Persisted.ID, WAMID: holder.Message.ID,
	}
	processor := NewInboundContinuationProcessor(f.app, time.Second)
	require.Nil(t, processor.process, "use ordinary preparation, model capture and apply")
	var fillers []func()
	for _, weight := range []int{3, 3, 1} {
		release, err := f.app.acquireInboundContinuationCapacity(ctx, nil, weight)
		require.NoError(t, err)
		fillers = append(fillers, release)
		t.Cleanup(release)
	}
	processDone, processJoined := make(chan error, 1), make(chan struct{})
	guardEntered, guardRelease := make(chan struct{}), make(chan struct{})
	guardDone, guardJoined := make(chan error, 1), make(chan struct{})
	var releaseOnce sync.Once
	releaseGuard := func() { releaseOnce.Do(func() { close(guardRelease) }) }
	guardStarted := false
	t.Cleanup(func() {
		releaseGuard()
		f.model.unblock()
		for _, release := range fillers {
			release()
		}
		cancel()
		select {
		case <-processJoined:
		case <-time.After(20 * time.Second):
			t.Error("generated apply did not join during shared-guard cleanup")
		}
		if guardStarted {
			select {
			case <-guardJoined:
			case <-time.After(10 * time.Second):
				t.Error("external physical guard did not join")
			}
		}
		h1IngressJoin(t, f.app)
	})
	f.model.unblock()
	go func() {
		defer close(processJoined)
		processDone <- processor.ProcessMessage(ctx, work.OrganizationID, work.Persisted.ID)
	}()
	h1CapacityWaitQueue(t, &f.app.continuationAdmission, 7, 1)
	f.app.continuationAdmission.mu.Lock()
	queued := f.app.continuationAdmission.waiters[0]
	queuedOrganization, queuedMode, queuedPriority := queued.organization, queued.mode, queued.priority
	f.app.continuationAdmission.mu.Unlock()
	require.Equal(t, work.OrganizationID.String(), queuedOrganization)
	require.Equal(t, inboundContinuationReader, queuedMode)
	require.Equal(t, inboundContinuationApplyPriority, queuedPriority)
	require.Zero(t, f.pool.Stats().InUse, "captured apply waits without a database connection")
	require.EqualValues(t, 1, f.model.calls.Load())
	var captured []models.ScheduledJob
	require.NoError(t, f.shared.Where("organization_id = ? AND kind = ? AND aggregate_id = ?", work.OrganizationID, inboundContinuationActionJobKind, work.Persisted.ID).Find(&captured).Error)
	require.Len(t, captured, 1)
	require.Equal(t, "legacy_ai_generate", captured[0].Payload["effect_kind"])
	require.Equal(t, inboundActionStateResolved, captured[0].Payload["state"])
	require.Equal(t, "generated", inboundContinuationActionResult(captured[0].Payload)["outcome"])
	var beforeContact, beforeHolder models.Contact
	var beforeSession models.ChatbotSession
	var beforeHolderMessage models.Message
	require.NoError(t, f.shared.First(&beforeContact, "id = ? AND organization_id = ?", work.Contact.ID, work.OrganizationID).Error)
	require.NoError(t, f.shared.First(&beforeSession, "id = ? AND organization_id = ?", f.sessions[0].ID, work.OrganizationID).Error)
	require.NoError(t, f.shared.First(&beforeHolder, "id = ? AND organization_id = ?", holder.Contact.ID, work.OrganizationID).Error)
	require.NoError(t, f.shared.First(&beforeHolderMessage, "id = ? AND organization_id = ?", holder.Persisted.ID, work.OrganizationID).Error)

	guardStarted = true
	go func() {
		defer close(guardJoined)
		guardDone <- execution.withInboundContinuationPhysicalAIAttempt(ctx, func() error {
			close(guardEntered)
			select {
			case <-guardRelease:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
	}()
	select {
	case <-guardEntered:
	case err := <-guardDone:
		t.Fatalf("external physical guard refused before apply: %v", err)
	case <-time.After(8 * time.Second):
		t.Fatal("external physical guard did not acquire its real database fence")
	}
	assertGuardHeld := func() {
		t.Helper()
		select {
		case <-guardJoined:
			t.Fatal("external physical guard ended before apply/replay completed")
		default:
		}
		require.Equal(t, 1, externalPool.Stats().InUse, "the independently owned physical guard must remain open")
	}
	assertGuardHeld()
	for _, release := range fillers {
		release()
	}
	h1CapacityJoin(t, processDone, 1)
	assertGuardHeld()
	f.assertGenerated(t, 0)

	// The generated path is shared-lock compatible, not read-only. Verify its
	// actual independently committed send, mirror, read-state and SLA facts.
	var outgoing, incoming models.Message
	require.NoError(t, f.shared.Where("organization_id = ? AND contact_id = ? AND direction = ?", work.OrganizationID, work.Contact.ID, models.DirectionOutgoing).First(&outgoing).Error)
	require.Equal(t, models.MessageStatusSent, outgoing.Status)
	require.Equal(t, automaticAIDispatchStateResolved, outgoing.Metadata[automaticAIDispatchStateMetadataKey])
	require.NotEmpty(t, outgoing.Metadata[automaticAIDispatchSettledAtKey])
	require.NotNil(t, outgoing.InboxConversationID)
	var conversation models.InboxConversation
	require.NoError(t, f.shared.First(&conversation, "id = ? AND organization_id = ?", *outgoing.InboxConversationID, work.OrganizationID).Error)
	require.Equal(t, work.Contact.ID, conversation.ContactID)
	require.NotNil(t, conversation.LastOutboundAt)
	require.Equal(t, outgoing.Content, conversation.LastMessagePreview)
	require.Zero(t, conversation.UnreadCount)
	require.NotNil(t, conversation.ContactIdentityID)
	var identity models.ContactIdentity
	require.NoError(t, f.shared.First(&identity, "id = ? AND organization_id = ?", *conversation.ContactIdentityID, work.OrganizationID).Error)
	require.Equal(t, work.Contact.ID, identity.ContactID)
	require.Equal(t, conversation.ChannelAccountID, identity.ChannelAccountID)
	var shadowCount int64
	require.NoError(t, f.shared.Model(&models.ChannelAccount{}).Where("id = ? AND organization_id = ?", conversation.ChannelAccountID, work.OrganizationID).Count(&shadowCount).Error)
	require.EqualValues(t, 1, shadowCount)
	require.NoError(t, f.shared.First(&incoming, "id = ? AND organization_id = ?", work.Persisted.ID, work.OrganizationID).Error)
	require.Equal(t, models.MessageStatusRead, incoming.Status)
	require.Equal(t, true, incoming.Metadata["inbound_continuation_completed"])

	var afterContact, afterHolder models.Contact
	var afterSession models.ChatbotSession
	var afterHolderMessage models.Message
	require.NoError(t, f.shared.First(&afterContact, "id = ? AND organization_id = ?", work.Contact.ID, work.OrganizationID).Error)
	require.NotNil(t, afterContact.ChatbotLastMessageAt)
	require.False(t, afterContact.ChatbotReminderSent)
	require.True(t, afterContact.IsRead)
	// Only these display/read/SLA fields may differ. Identity selectors, consent,
	// assignment, merge/deletion state and metadata must remain unchanged.
	afterContact.UpdatedAt = beforeContact.UpdatedAt
	afterContact.LastMessageAt = beforeContact.LastMessageAt
	afterContact.LastMessagePreview = beforeContact.LastMessagePreview
	afterContact.IsRead = beforeContact.IsRead
	afterContact.ChatbotLastMessageAt = beforeContact.ChatbotLastMessageAt
	afterContact.ChatbotReminderSent = beforeContact.ChatbotReminderSent
	require.Equal(t, beforeContact, afterContact)
	require.NoError(t, f.shared.First(&afterSession, "id = ? AND organization_id = ?", beforeSession.ID, work.OrganizationID).Error)
	require.Equal(t, beforeSession, afterSession, "generated apply must not advance session authority")
	require.NoError(t, f.shared.First(&afterHolder, "id = ? AND organization_id = ?", holder.Contact.ID, work.OrganizationID).Error)
	require.Equal(t, beforeHolder, afterHolder)
	require.NoError(t, f.shared.First(&afterHolderMessage, "id = ? AND organization_id = ?", holder.Persisted.ID, work.OrganizationID).Error)
	require.Equal(t, beforeHolderMessage, afterHolderMessage)
	assertGuardHeld()
	require.NoError(t, processor.ProcessMessage(ctx, work.OrganizationID, work.Persisted.ID))
	assertGuardHeld()
	f.assertGenerated(t, 0)
	require.EqualValues(t, 1, f.model.calls.Load(), "completed replay cannot regenerate")
	f.mu.Lock()
	sends := len(f.sent)
	f.mu.Unlock()
	require.Equal(t, 1, sends, "completed replay cannot resend")
	require.Zero(t, f.model.errors.Load())
	require.Zero(t, f.providerErrors.Load())
	h1OrgEmpty(t, &f.app.continuationAdmission)
	releaseGuard()
	select {
	case err := <-guardDone:
		require.NoError(t, err)
	case <-time.After(8 * time.Second):
		t.Fatal("external physical guard did not settle after explicit release")
	}
	require.Eventually(t, func() bool { return externalPool.Stats().InUse == 0 }, 2*time.Second, time.Millisecond)
}
