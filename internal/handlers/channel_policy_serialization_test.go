package handlers

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/database"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestRecordConsentTransactionWaitsForContactPolicyLock(t *testing.T) {
	db := testutil.SetupTestDB(t)
	org := testutil.CreateTestOrganization(t, db)
	user := testutil.CreateTestUser(t, db, org.ID)
	contact := testutil.CreateTestContact(t, db, org.ID)

	blockingTx := db.Begin()
	require.NoError(t, blockingTx.Error)
	t.Cleanup(func() {
		_ = blockingTx.Rollback().Error
	})
	require.NoError(t, database.LockContactPolicyScope(
		blockingTx,
		org.ID,
		contact.ID,
	))

	now := time.Now().UTC()
	event := &models.ConsentEvent{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: org.ID,
		ContactID:      &contact.ID,
		SubjectType:    "contact",
		SubjectKey:     contact.ID.String(),
		Purpose:        string(models.ChannelPreferencePurposeService),
		Channel:        string(models.ChannelInstagram),
		Action:         models.ConsentActionWithdrawn,
		Source:         "test",
		ActorUserID:    &user.ID,
		Evidence:       models.JSONB{},
		CapturedAt:     now,
	}
	state := &models.ConsentState{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: org.ID,
		ContactID:      &contact.ID,
		SubjectType:    event.SubjectType,
		SubjectKey:     event.SubjectKey,
		Purpose:        event.Purpose,
		Channel:        event.Channel,
		Status:         models.ConsentStatusWithdrawn,
		LatestEventID:  event.ID,
		EffectiveAt:    now,
		Metadata:       models.JSONB{},
	}

	writerPID := make(chan int, 1)
	writerDone := make(chan error, 1)
	go func() {
		writerDone <- db.Connection(func(connection *gorm.DB) error {
			var backendPID int
			if err := connection.Raw("SELECT pg_backend_pid()").
				Scan(&backendPID).Error; err != nil {
				writerPID <- 0
				return err
			}
			writerPID <- backendPID
			return connection.Transaction(func(tx *gorm.DB) error {
				return productCommercialRecordConsentTx(
					tx,
					event,
					state,
					user.ID,
				)
			})
		})
	}()
	backendPID := <-writerPID
	require.Positive(t, backendPID)
	testutil.RequirePostgresBackendWaitingForLock(t, db, backendPID)

	var eventCount int64
	require.NoError(t, db.Model(&models.ConsentEvent{}).
		Where("id = ?", event.ID).
		Count(&eventCount).Error)
	assert.Zero(t, eventCount, "the consent event must not precede its policy lock")

	require.NoError(t, blockingTx.Commit().Error)
	select {
	case err := <-writerDone:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		require.Fail(t, "consent writer did not resume after the policy lock was released")
	}

	var persisted models.ConsentState
	require.NoError(t, db.Where(
		"organization_id = ? AND contact_id = ? AND purpose = ? AND channel = ?",
		org.ID,
		contact.ID,
		event.Purpose,
		event.Channel,
	).First(&persisted).Error)
	assert.Equal(t, models.ConsentStatusWithdrawn, persisted.Status)
}

func TestCreateActiveAgentTransferSerializesConcurrentDuplicates(t *testing.T) {
	db := testutil.SetupTestDB(t)
	org := testutil.CreateTestOrganization(t, db)
	contact := testutil.CreateTestContact(t, db, org.ID)

	first := &models.AgentTransfer{
		BaseModel:       models.BaseModel{ID: uuid.New()},
		OrganizationID:  org.ID,
		ContactID:       contact.ID,
		WhatsAppAccount: contact.WhatsAppAccount,
		PhoneNumber:     contact.PhoneNumber,
		Status:          models.TransferStatusActive,
		Source:          models.TransferSourceManual,
		TransferredAt:   time.Now().UTC(),
	}
	firstTx := db.Begin()
	require.NoError(t, firstTx.Error)
	t.Cleanup(func() {
		_ = firstTx.Rollback().Error
	})
	require.NoError(t, createActiveAgentTransferTx(firstTx, first))

	second := *first
	second.BaseModel = models.BaseModel{ID: uuid.New()}
	second.TransferredAt = time.Now().UTC()
	secondPID := make(chan int, 1)
	secondDone := make(chan error, 1)
	go func() {
		secondDone <- db.Connection(func(connection *gorm.DB) error {
			var backendPID int
			if err := connection.Raw("SELECT pg_backend_pid()").
				Scan(&backendPID).Error; err != nil {
				secondPID <- 0
				return err
			}
			secondPID <- backendPID
			return connection.Transaction(func(tx *gorm.DB) error {
				return createActiveAgentTransferTx(tx, &second)
			})
		})
	}()
	backendPID := <-secondPID
	require.Positive(t, backendPID)
	testutil.RequirePostgresBackendWaitingForLock(t, db, backendPID)

	require.NoError(t, firstTx.Commit().Error)
	select {
	case err := <-secondDone:
		require.ErrorIs(t, err, errActiveAgentTransferExists)
	case <-time.After(5 * time.Second):
		require.Fail(t, "duplicate handover writer did not resume after commit")
	}

	var activeCount int64
	require.NoError(t, db.Model(&models.AgentTransfer{}).
		Where(
			"organization_id = ? AND contact_id = ? AND status = ?",
			org.ID,
			contact.ID,
			models.TransferStatusActive,
		).
		Count(&activeCount).Error)
	assert.EqualValues(t, 1, activeCount)
}
