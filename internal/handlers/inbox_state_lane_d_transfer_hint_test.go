package handlers

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/models"
	appwebsocket "github.com/shridarpatil/whatomate/internal/websocket"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// These tests drive the keyword handover through createTransferFromKeyword ->
// saveAndFinalizeTransfer -> broadcastTransferCreated. createTransferFromKeyword
// is called as a statement so the tests compile against both main's signature
// and #295's, which also returns an outcome.
//
// Each test seeds an organization-level ChatbotSettings row. Without one,
// getChatbotSettingsCached returns ErrRecordNotFound: main ignores it, but
// #295 returns early, before any transfer exists. Each test also proves the
// path ran: the transfer row it expects, and broadcastTransferCreated's own
// agent_transfer frame, which that function sends immediately whatever the
// transaction.

func laneDSeedChatbotSettings(t *testing.T, db *gorm.DB, organizationID uuid.UUID) {
	t.Helper()
	// Business hours and AssignToSameAgent stay off, so the keyword handover
	// always creates an unassigned transfer and sends no customer message.
	require.NoError(t, db.Create(&models.ChatbotSettings{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: organizationID,
	}).Error)
}

func laneDActiveTransferCount(t *testing.T, db *gorm.DB, organizationID, contactID uuid.UUID) int64 {
	t.Helper()
	var count int64
	require.NoError(t, db.Model(&models.AgentTransfer{}).
		Where("organization_id = ? AND contact_id = ? AND status = ?", organizationID, contactID, models.TransferStatusActive).
		Count(&count).Error)
	return count
}

// laneDCollectRealtime drains every frame the client receives within wait and
// groups the payloads by message type.
func laneDCollectRealtime(t *testing.T, client *appwebsocket.Client, wait time.Duration) map[string][]map[string]any {
	t.Helper()
	collected := map[string][]map[string]any{}
	deadline := time.After(wait)
	for {
		select {
		case data := <-client.SendChan():
			var envelope struct {
				Type    string         `json:"type"`
				Payload map[string]any `json:"payload"`
			}
			require.NoError(t, json.Unmarshal(data, &envelope))
			collected[envelope.Type] = append(collected[envelope.Type], envelope.Payload)
		case <-deadline:
			return collected
		}
	}
}

// laneDRequireTransferAnnounced asserts that broadcastTransferCreated ran
// exactly once for the contact.
func laneDRequireTransferAnnounced(t *testing.T, frames map[string][]map[string]any, contactID uuid.UUID) {
	t.Helper()
	announced := frames[appwebsocket.TypeAgentTransfer]
	require.Len(t, announced, 1, "broadcastTransferCreated must have run once: %v", frames)
	require.Equal(t, contactID.String(), announced[0]["contact_id"])
}

// laneDRequireOneTransferHint asserts exactly one identifier-only transfer
// contact_update for the contact.
func laneDRequireOneTransferHint(t *testing.T, frames map[string][]map[string]any, contactID uuid.UUID) {
	t.Helper()
	require.Equal(t,
		[]map[string]any{{"contact_id": contactID.String(), "kind": "transfer"}},
		frames[appwebsocket.TypeContactUpdate],
		"frames: %v", frames)
}

// laneDRunContinuationKeywordTransfer mirrors the inbound continuation: the
// per-job App (inboundContinuation set) runs on the outer transaction, the
// outer owner adopts its after-commit work, and that work runs only if the
// outer transaction commits.
func laneDRunContinuationKeywordTransfer(
	t *testing.T,
	root *App,
	db *gorm.DB,
	organizationID uuid.UUID,
	account *models.WhatsAppAccount,
	contact *models.Contact,
	rollback bool,
) {
	t.Helper()
	errOuterRollback := errors.New("lane D: outer continuation transaction rolled back")
	var afterCommit []func()
	err := db.Transaction(func(tx *gorm.DB) error {
		outer := root.scopedApp(tx, organizationID)
		execution := outer.scopedApp(tx, organizationID)
		execution.inboundContinuation = &inboundContinuationExecution{
			OrganizationID: organizationID,
			ContactID:      contact.ID,
			MessageID:      uuid.New(),
			WAMID:          "wamid.lane-d-transfer." + uuid.NewString(),
		}
		execution.createTransferFromKeyword(account, contact)
		outer.adoptAfterCommit(execution)
		if rollback {
			return errOuterRollback
		}
		afterCommit = outer.takeAfterCommit()
		return nil
	})
	if rollback {
		require.ErrorIs(t, err, errOuterRollback)
		return
	}
	require.NoError(t, err)
	root.runAfterCommit(afterCommit)
}

// A continuation's handover commits in its own transaction. A later rollback
// (or retry) of the continuation's outer transaction must not drop the hint.
//
// Fails before the fix (a38b4c57): broadcastTransferCreated queued the hint
// on the per-job App, the outer owner adopted it, and the outer rollback
// discarded it, so no contact_update is collected.
func TestLaneDIndependentlyCommittedTransferHintSurvivesOuterRollback(t *testing.T) {
	db := testutil.SetupTestDB(t)
	organization := testutil.CreateTestOrganization(t, db)
	laneDSeedChatbotSettings(t, db, organization.ID)
	account := testutil.CreateTestWhatsAppAccount(t, db, organization.ID)
	contact := testutil.CreateTestContactWith(t, db, organization.ID, testutil.WithContactAccount(account.Name))
	hub, client := laneDHub(t, organization.ID)
	root := &App{DB: db, Log: testutil.NopLogger(), WSHub: hub}

	laneDRunContinuationKeywordTransfer(t, root, db, organization.ID, account, contact, true)

	// Only the continuation branch of saveAndFinalizeTransfer commits
	// independently, so a transfer surviving the outer rollback proves it ran.
	require.Equal(t, int64(1), laneDActiveTransferCount(t, db, organization.ID, contact.ID),
		"the handover committed independently of the rolled-back outer transaction")
	frames := laneDCollectRealtime(t, client, 600*time.Millisecond)
	laneDRequireTransferAnnounced(t, frames, contact.ID)
	laneDRequireOneTransferHint(t, frames, contact.ID)
}

// An ordinary handover is written by the receiver's own transaction: no hint
// when it rolls back, none before it commits, exactly one after.
//
// This is a guard, not a fix detector: it passes before and after the fix. A
// fix that sent every transfer hint immediately would fail it twice (a hint
// after the rollback, and one before the commit).
func TestLaneDTransactionOwnedTransferHintFollowsItsTransaction(t *testing.T) {
	db := testutil.SetupTestDB(t)
	organization := testutil.CreateTestOrganization(t, db)
	laneDSeedChatbotSettings(t, db, organization.ID)
	account := testutil.CreateTestWhatsAppAccount(t, db, organization.ID)
	contact := testutil.CreateTestContactWith(t, db, organization.ID, testutil.WithContactAccount(account.Name))
	hub, client := laneDHub(t, organization.ID)
	root := &App{DB: db, Log: testutil.NopLogger(), WSHub: hub}

	errRollback := errors.New("lane D: request transaction rolled back")
	err := db.Transaction(func(tx *gorm.DB) error {
		request := root.scopedApp(tx, organization.ID)
		request.createTransferFromKeyword(account, contact)
		// The handover ran on this transaction: its transfer is visible here.
		require.Equal(t, int64(1), laneDActiveTransferCount(t, tx, organization.ID, contact.ID))
		return errRollback
	})
	require.ErrorIs(t, err, errRollback)
	require.Zero(t, laneDActiveTransferCount(t, db, organization.ID, contact.ID))
	frames := laneDCollectRealtime(t, client, 600*time.Millisecond)
	laneDRequireTransferAnnounced(t, frames, contact.ID)
	require.Empty(t, frames[appwebsocket.TypeContactUpdate], "the hint is discarded with its transaction")

	var afterCommit []func()
	require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
		request := root.scopedApp(tx, organization.ID)
		request.createTransferFromKeyword(account, contact)
		require.Equal(t, int64(1), laneDActiveTransferCount(t, tx, organization.ID, contact.ID))
		uncommitted := laneDCollectRealtime(t, client, 300*time.Millisecond)
		laneDRequireTransferAnnounced(t, uncommitted, contact.ID)
		require.Empty(t, uncommitted[appwebsocket.TypeContactUpdate], "the hint is queued, not sent, before the commit")
		afterCommit = request.takeAfterCommit()
		return nil
	}))
	require.Equal(t, int64(1), laneDActiveTransferCount(t, db, organization.ID, contact.ID))
	root.runAfterCommit(afterCommit)
	committed := laneDCollectRealtime(t, client, 600*time.Millisecond)
	require.Empty(t, committed[appwebsocket.TypeAgentTransfer])
	laneDRequireOneTransferHint(t, committed, contact.ID)
}

// The continuation job is retried after its outer rollback. The handover it
// committed the first time already exists, so the replay announces nothing,
// whether its own outer transaction rolls back or commits.
//
// Fails before the fix (a38b4c57) at its first step, for the same reason as
// the independent-commit test: the first run's hint is lost. A fix that
// re-announced an existing transfer would fail its replay checks.
func TestLaneDReplayedContinuationDoesNotAnnounceTheTransferAgain(t *testing.T) {
	db := testutil.SetupTestDB(t)
	organization := testutil.CreateTestOrganization(t, db)
	laneDSeedChatbotSettings(t, db, organization.ID)
	account := testutil.CreateTestWhatsAppAccount(t, db, organization.ID)
	contact := testutil.CreateTestContactWith(t, db, organization.ID, testutil.WithContactAccount(account.Name))
	hub, client := laneDHub(t, organization.ID)
	root := &App{DB: db, Log: testutil.NopLogger(), WSHub: hub}

	laneDRunContinuationKeywordTransfer(t, root, db, organization.ID, account, contact, true)
	require.Equal(t, int64(1), laneDActiveTransferCount(t, db, organization.ID, contact.ID))
	first := laneDCollectRealtime(t, client, 600*time.Millisecond)
	laneDRequireTransferAnnounced(t, first, contact.ID)
	laneDRequireOneTransferHint(t, first, contact.ID)

	for _, rollback := range []bool{true, false} {
		laneDRunContinuationKeywordTransfer(t, root, db, organization.ID, account, contact, rollback)
		require.Equal(t, int64(1), laneDActiveTransferCount(t, db, organization.ID, contact.ID))
		replay := laneDCollectRealtime(t, client, 600*time.Millisecond)
		require.Empty(t, replay[appwebsocket.TypeAgentTransfer], "replay (rollback=%v) re-announced the transfer", rollback)
		require.Empty(t, replay[appwebsocket.TypeContactUpdate], "replay (rollback=%v) re-sent the hint", rollback)
	}
}
