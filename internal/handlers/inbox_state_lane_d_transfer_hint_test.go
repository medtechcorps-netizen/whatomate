package handlers

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// These tests drive the keyword handover through createTransferFromKeyword,
// called as a statement so they compile against both the current signature
// and the H1 one that also returns an outcome.

func laneDActiveTransferCount(t *testing.T, db *gorm.DB, organizationID, contactID uuid.UUID) int64 {
	t.Helper()
	var count int64
	require.NoError(t, db.Model(&models.AgentTransfer{}).
		Where("organization_id = ? AND contact_id = ? AND status = ?", organizationID, contactID, models.TransferStatusActive).
		Count(&count).Error)
	return count
}

// laneDRunContinuationKeywordTransfer mirrors the inbound continuation: the
// per-job App runs on the outer transaction, the outer owner adopts its
// after-commit work, and that work runs only if the outer transaction commits.
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
func TestLaneDIndependentlyCommittedTransferHintSurvivesOuterRollback(t *testing.T) {
	db := testutil.SetupTestDB(t)
	organization := testutil.CreateTestOrganization(t, db)
	account := testutil.CreateTestWhatsAppAccount(t, db, organization.ID)
	contact := testutil.CreateTestContactWith(t, db, organization.ID, testutil.WithContactAccount(account.Name))
	hub, client := laneDHub(t, organization.ID)
	root := &App{DB: db, Log: testutil.NopLogger(), WSHub: hub}

	laneDRunContinuationKeywordTransfer(t, root, db, organization.ID, account, contact, true)

	require.Equal(t, int64(1), laneDActiveTransferCount(t, db, organization.ID, contact.ID),
		"the handover committed independently of the rolled-back outer transaction")
	laneDExpectContactUpdate(t, client, contact.ID, "transfer")
	laneDExpectNoContactUpdate(t, client, 300*time.Millisecond)
}

// An ordinary handover is written by the receiver's own transaction: no hint
// when it rolls back, exactly one after it commits, and none before.
func TestLaneDTransactionOwnedTransferHintFollowsItsTransaction(t *testing.T) {
	db := testutil.SetupTestDB(t)
	organization := testutil.CreateTestOrganization(t, db)
	account := testutil.CreateTestWhatsAppAccount(t, db, organization.ID)
	contact := testutil.CreateTestContactWith(t, db, organization.ID, testutil.WithContactAccount(account.Name))
	hub, client := laneDHub(t, organization.ID)
	root := &App{DB: db, Log: testutil.NopLogger(), WSHub: hub}

	errRollback := errors.New("lane D: request transaction rolled back")
	err := db.Transaction(func(tx *gorm.DB) error {
		request := root.scopedApp(tx, organization.ID)
		request.createTransferFromKeyword(account, contact)
		return errRollback
	})
	require.ErrorIs(t, err, errRollback)
	require.Zero(t, laneDActiveTransferCount(t, db, organization.ID, contact.ID))
	laneDExpectNoContactUpdate(t, client, 300*time.Millisecond)

	var afterCommit []func()
	require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
		request := root.scopedApp(tx, organization.ID)
		request.createTransferFromKeyword(account, contact)
		// Still uncommitted: the hint is queued, not sent.
		laneDExpectNoContactUpdate(t, client, 150*time.Millisecond)
		afterCommit = request.takeAfterCommit()
		return nil
	}))
	require.Equal(t, int64(1), laneDActiveTransferCount(t, db, organization.ID, contact.ID))
	root.runAfterCommit(afterCommit)
	laneDExpectContactUpdate(t, client, contact.ID, "transfer")
	laneDExpectNoContactUpdate(t, client, 300*time.Millisecond)
}

// The continuation job is retried after its outer rollback. The handover it
// committed the first time already exists, so the replay announces nothing,
// whether its own outer transaction rolls back or commits.
func TestLaneDReplayedContinuationDoesNotAnnounceTheTransferAgain(t *testing.T) {
	db := testutil.SetupTestDB(t)
	organization := testutil.CreateTestOrganization(t, db)
	account := testutil.CreateTestWhatsAppAccount(t, db, organization.ID)
	contact := testutil.CreateTestContactWith(t, db, organization.ID, testutil.WithContactAccount(account.Name))
	hub, client := laneDHub(t, organization.ID)
	root := &App{DB: db, Log: testutil.NopLogger(), WSHub: hub}

	laneDRunContinuationKeywordTransfer(t, root, db, organization.ID, account, contact, true)
	laneDExpectContactUpdate(t, client, contact.ID, "transfer")

	laneDRunContinuationKeywordTransfer(t, root, db, organization.ID, account, contact, true)
	laneDRunContinuationKeywordTransfer(t, root, db, organization.ID, account, contact, false)

	require.Equal(t, int64(1), laneDActiveTransferCount(t, db, organization.ID, contact.ID))
	laneDExpectNoContactUpdate(t, client, 300*time.Millisecond)
}
