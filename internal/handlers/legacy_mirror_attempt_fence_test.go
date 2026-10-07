package handlers

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	channelapi "github.com/shridarpatil/whatomate/internal/channel"
	"github.com/shridarpatil/whatomate/internal/database"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// An AI attempt fence (organizations FOR KEY SHARE) can be held by a goroutine
// that is itself waiting for a mirror or recovery on another connection. If an
// inbound admission's policy fence queues behind that attempt, a sharer that
// queued behind the admission without limit would close a cycle through Go that
// PostgreSQL cannot detect, freezing every policy fence in the tenant until the
// attempt's context expires. These tests require each sharer to get past the
// queued fence well within that time.

func legacyAttemptFenceBackendPID(t *testing.T, tx *gorm.DB) int {
	t.Helper()
	var pid int
	require.NoError(t, tx.Session(&gorm.Session{NewDB: true}).Raw("SELECT pg_backend_pid()").Scan(&pid).Error)
	return pid
}

func legacyAttemptFenceBlocked(db *gorm.DB, pid int) bool {
	var blocked bool
	return db.Raw("SELECT cardinality(pg_catalog.pg_blocking_pids(?)) > 0", pid).
		Scan(&blocked).Error == nil && blocked
}

// queueLegacyPolicyFence starts an inbound admission's policy fence on its own
// connection and returns once it is waiting.
func queueLegacyPolicyFence(
	ctx context.Context,
	t *testing.T,
	db *gorm.DB,
	organizationID uuid.UUID,
) (*gorm.DB, <-chan error) {
	t.Helper()
	fence := db.WithContext(ctx).Begin()
	require.NoError(t, fence.Error)
	t.Cleanup(func() { _ = fence.Rollback().Error })
	fencePID := legacyAttemptFenceBackendPID(t, fence)
	fenced := make(chan error, 1)
	go func() { fenced <- database.LockOrganizationPolicyScope(fence, organizationID) }()
	require.Eventually(t, func() bool { return legacyAttemptFenceBlocked(db, fencePID) },
		10*time.Second, 10*time.Millisecond, "the policy fence must queue behind the attempt fence")
	return fence, fenced
}

// pauseLegacyOutgoingInserts holds back the tenant's outgoing Message inserts
// until release, so a send can be stopped between its attempt fence and its
// mirror. It returns the holding backend's pid.
func pauseLegacyOutgoingInserts(t *testing.T, db *gorm.DB, organizationID uuid.UUID) (int, func()) {
	t.Helper()
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	key := "test-pause-outgoing:" + suffix
	functionName := "pause_outgoing_message_" + suffix
	triggerName := functionName + "_trigger"
	holder := db.Begin()
	require.NoError(t, holder.Error)
	var once sync.Once
	release := func() { once.Do(func() { _ = holder.Commit().Error }) }
	// Registered before anything is created, so a failed CREATE cannot leak
	// the function. A paused send still owns its Message insert until release
	// lets it finish, and DROP TRIGGER needs ACCESS EXCLUSIVE on messages, so
	// the drop retries under a short lock_timeout instead of queueing every
	// other messages query behind it.
	t.Cleanup(func() {
		release()
		require.NoError(t, dropLegacyTestTrigger(db, "messages", triggerName, functionName))
	})
	require.NoError(t, holder.Exec(
		"SELECT pg_catalog.pg_advisory_xact_lock(pg_catalog.hashtextextended(?, 0))", key,
	).Error)
	holderPID := legacyAttemptFenceBackendPID(t, holder)
	require.NoError(t, db.Exec(fmt.Sprintf(`
		CREATE FUNCTION %s() RETURNS trigger
		LANGUAGE plpgsql
		AS $$
		BEGIN
			IF NEW.direction = 'outgoing' AND NEW.organization_id = '%s'::uuid THEN
				PERFORM pg_catalog.pg_advisory_xact_lock(pg_catalog.hashtextextended('%s', 0));
			END IF;
			RETURN NEW;
		END;
		$$`, functionName, organizationID, key)).Error)
	require.NoError(t, db.Exec(fmt.Sprintf(
		"CREATE TRIGGER %s BEFORE INSERT ON messages FOR EACH ROW EXECUTE FUNCTION %s()",
		triggerName, functionName,
	)).Error)
	return holderPID, release
}

// dropLegacyTestTrigger drops a test trigger and its function, retrying under a
// short lock_timeout for up to three minutes, the longest context of the tests
// that pause a send.
func dropLegacyTestTrigger(db *gorm.DB, table, triggerName, functionName string) error {
	deadline := time.Now().Add(3 * time.Minute)
	for {
		err := db.Transaction(func(tx *gorm.DB) error {
			if err := tx.Exec("SET LOCAL lock_timeout = '1s'").Error; err != nil {
				return err
			}
			if err := tx.Exec(fmt.Sprintf("DROP TRIGGER IF EXISTS %s ON %s", triggerName, table)).Error; err != nil {
				return err
			}
			return tx.Exec(fmt.Sprintf("DROP FUNCTION IF EXISTS %s()", functionName)).Error
		})
		if err == nil || time.Now().After(deadline) {
			return err
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func waitForLegacyBackendBlockedBy(t *testing.T, db *gorm.DB, holderPID int) {
	t.Helper()
	require.Eventually(t, func() bool {
		var waiting bool
		return db.Raw(
			"SELECT EXISTS (SELECT 1 FROM pg_catalog.pg_stat_activity WHERE ? = ANY(pg_catalog.pg_blocking_pids(pid)))",
			holderPID,
		).Scan(&waiting).Error == nil && waiting
	}, 10*time.Second, 10*time.Millisecond, "the send never reached its paused Message insert")
}

func requireLegacyStepWithin(t *testing.T, limit time.Duration, name string, step func() error) {
	t.Helper()
	started := time.Now()
	done := make(chan error, 1)
	go func() { done <- step() }()
	select {
	case err := <-done:
		require.NoError(t, err, name)
		assert.Less(t, time.Since(started), limit, name)
	case <-time.After(limit):
		require.Failf(t, "sharer stalled behind a fence queued on an attempt fence", "%s did not finish within %v", name, limit)
	}
}

var errLegacyAttemptFenceRollback = errors.New("roll back the probe transaction")

func TestLegacySharersDoNotQueueBehindAPolicyFenceWaitingOnAnAttemptFence(t *testing.T) {
	app := newProcessorTestApp(t)
	organization, account := createProcessorTestOrg(t, app)
	contact := testutil.CreateTestContactWith(t, app.DB, organization.ID, testutil.WithContactAccount(account.Name))
	shadow, err := channelapi.EnsureLegacyMetaWhatsAppAccount(app.DB, channelapi.LegacyMetaAccountRef{
		ID: account.ID, OrganizationID: organization.ID, Name: account.Name, Status: account.Status,
	})
	require.NoError(t, err)
	outgoing := models.Message{
		BaseModel:         models.BaseModel{ID: uuid.New()},
		OrganizationID:    organization.ID,
		WhatsAppAccount:   account.Name,
		ContactID:         contact.ID,
		Direction:         models.DirectionOutgoing,
		MessageType:       models.MessageTypeText,
		Content:           "attempt-fence mirror probe",
		Status:            models.MessageStatusPending,
		WhatsAppMessageID: "",
		ConversationID:    uuid.NewString(),
		InteractiveData:   models.JSONB{},
		Metadata:          models.JSONB{},
	}
	require.NoError(t, app.DB.Create(&outgoing).Error)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	attempt := app.DB.WithContext(ctx).Begin()
	require.NoError(t, attempt.Error)
	t.Cleanup(func() { _ = attempt.Rollback().Error })
	require.NoError(t, database.LockOrganizationAIAttemptScope(attempt, organization.ID))
	fence, fenced := queueLegacyPolicyFence(ctx, t, app.DB, organization.ID)

	inTransaction := func(step func(tx *gorm.DB) error) func() error {
		return func() error {
			err := app.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
				if err := step(tx); err != nil {
					return err
				}
				return errLegacyAttemptFenceRollback
			})
			if errors.Is(err, errLegacyAttemptFenceRollback) {
				return nil
			}
			return err
		}
	}
	requireLegacyStepWithin(t, 2*time.Second, "outbound mirror", func() error {
		return app.requireLegacyWhatsAppMessageMirror(ctx, account, outgoing.ID)
	})
	requireLegacyStepWithin(t, 2*time.Second, "delivery recovery organization lock", inTransaction(func(tx *gorm.DB) error {
		return channelapi.LockLegacyMetaOrganization(tx, organization.ID)
	}))
	requireLegacyStepWithin(t, 2*time.Second, "strict reply organization and shadow", inTransaction(func(tx *gorm.DB) error {
		return channelapi.LockLegacyMetaOrganizationAndShadow(tx, organization.ID, shadow.ID)
	}))
	requireLegacyStepWithin(t, 2*time.Second, "rename stage", inTransaction(func(tx *gorm.DB) error {
		_, err := channelapi.StageLegacyMetaWhatsAppAccountRename(tx, organization.ID, account.ID, account.Name, account.Name+" renamed")
		return err
	}))

	require.NoError(t, attempt.Commit().Error)
	select {
	case fenceErr := <-fenced:
		require.NoError(t, fenceErr)
	case <-time.After(10 * time.Second):
		require.Fail(t, "the policy fence did not acquire after the attempt fence committed")
	}
	require.NoError(t, fence.Commit().Error)
	var mirrored models.Message
	require.NoError(t, app.DB.Where("id = ? AND organization_id = ?", outgoing.ID, organization.ID).First(&mirrored).Error)
	assert.NotNil(t, mirrored.InboxConversationID)
}

// An SLA chatbot reminder holds its inactivity attempt fence while it sends. An
// inbound admission that arrives during the send queues its policy fence behind
// that attempt, and the reminder's own outbound mirror must still get through.
func TestSLAChatbotReminderDeliversWhileAPolicyFenceQueuesBehindItsAttempt(t *testing.T) {
	app, account, selected, settings := nativePausePolicyTimerFixture(t)
	calls := nativePausePolicyProvider(t, app, nil)
	holderPID, release := pauseLegacyOutgoingInserts(t, app.DB, account.OrganizationID)

	processor := NewSLAProcessor(app, time.Second)
	reminded := make(chan struct{})
	go func() {
		defer close(reminded)
		processor.sendChatbotReminder(selected, settings)
	}()
	waitForLegacyBackendBlockedBy(t, app.DB, holderPID)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	fence, fenced := queueLegacyPolicyFence(ctx, t, app.DB, account.OrganizationID)
	release()

	select {
	case <-reminded:
	case <-time.After(10 * time.Second):
		require.Fail(t, "the reminder stalled behind the policy fence queued on its own attempt")
	}
	assert.EqualValues(t, 1, calls.Load())
	select {
	case fenceErr := <-fenced:
		require.NoError(t, fenceErr)
	case <-time.After(10 * time.Second):
		require.Fail(t, "the policy fence did not acquire after the reminder")
	}
	require.NoError(t, fence.Commit().Error)
	var stored models.Contact
	require.NoError(t, app.DB.First(&stored, "id = ?", selected.ID).Error)
	assert.True(t, stored.ChatbotReminderSent)
}

// An inbound continuation sends its chatbot reply under the continuation's
// attempt fence. A second admission that arrives during that send queues its
// policy fence behind the attempt, and the reply's outbound mirror must still
// get through.
func TestInboundContinuationReplyDeliversWhileAPolicyFenceQueuesBehindItsAttempt(t *testing.T) {
	app := newProcessorTestApp(t)
	organization, account := createProcessorTestOrg(t, app)
	calls := nativePausePolicyProvider(t, app, nil)
	settings := models.ChatbotSettings{
		BaseModel:          models.BaseModel{ID: uuid.New()},
		OrganizationID:     organization.ID,
		WhatsAppAccount:    account.Name,
		IsEnabled:          true,
		DefaultResponse:    "Welcome from the durable chatbot",
		SessionTimeoutMins: 30,
		AI:                 models.AIConfig{Enabled: false},
	}
	require.NoError(t, app.DB.Create(&settings).Error)
	work, duplicate, err := app.persistIncomingMessageBeforeAck(
		account.PhoneID,
		inboundContinuationTextMessage(t, "wamid.attempt-fence-"+uuid.NewString(), "6022"+uuid.NewString()[:8], "Hello"),
		"Attempt fence patient",
	)
	require.NoError(t, err)
	require.False(t, duplicate)
	holderPID, release := pauseLegacyOutgoingInserts(t, app.DB, organization.ID)

	processor := NewInboundContinuationProcessor(app, time.Second)
	replied := make(chan error, 1)
	go func() {
		replied <- processor.ProcessMessage(context.Background(), organization.ID, work.Persisted.ID)
	}()
	waitForLegacyBackendBlockedBy(t, app.DB, holderPID)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	fence, fenced := queueLegacyPolicyFence(ctx, t, app.DB, organization.ID)
	release()

	select {
	case replyErr := <-replied:
		require.NoError(t, replyErr)
	case <-time.After(10 * time.Second):
		require.Fail(t, "the reply stalled behind the policy fence queued on its own attempt")
	}
	assert.EqualValues(t, 1, calls.Load())
	select {
	case fenceErr := <-fenced:
		require.NoError(t, fenceErr)
	case <-time.After(10 * time.Second):
		require.Fail(t, "the policy fence did not acquire after the reply")
	}
	require.NoError(t, fence.Commit().Error)
	var sent int64
	require.NoError(t, app.DB.Model(&models.Message{}).Where(
		"organization_id = ? AND direction = ? AND status = ? AND inbox_conversation_id IS NOT NULL",
		organization.ID, models.DirectionOutgoing, models.MessageStatusSent,
	).Count(&sent).Error)
	assert.EqualValues(t, 1, sent)
}
