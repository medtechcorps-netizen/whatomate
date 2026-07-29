package handlers

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm/clause"
)

func TestSaveAndFinalizeTransfer_ConcurrentAliasCreatorsUseOneCanonicalTransfer(t *testing.T) {
	app := newProcessorTestApp(t)
	org, account := createProcessorTestOrg(t, app)
	canonical := testutil.CreateTestContact(t, app.DB, org.ID)
	alias := testutil.CreateTestContact(t, app.DB, org.ID)
	require.NoError(t, app.DB.Unscoped().Model(&models.Contact{}).
		Where("id = ? AND organization_id = ?", alias.ID, org.ID).
		Updates(map[string]any{
			"merged_into_id": canonical.ID,
			"deleted_at":     time.Now(),
		}).Error)

	type transferResult struct {
		transfer models.AgentTransfer
		err      error
	}
	const creators = 8
	start := make(chan struct{})
	results := make(chan transferResult, creators)
	for range creators {
		go func() {
			<-start
			staleContact := *alias
			transfer := models.AgentTransfer{
				BaseModel:       models.BaseModel{ID: uuid.New()},
				OrganizationID:  org.ID,
				ContactID:       alias.ID,
				WhatsAppAccount: account.Name,
				PhoneNumber:     alias.PhoneNumber,
				Status:          models.TransferStatusActive,
				Source:          models.TransferSourceFlow,
				TransferredAt:   time.Now(),
			}
			err := app.saveAndFinalizeTransfer(
				&transfer,
				account,
				&staleContact,
				nil,
				false,
			)
			results <- transferResult{transfer: transfer, err: err}
		}()
	}
	close(start)

	successCount := 0
	conflictCount := 0
	for range creators {
		result := <-results
		switch {
		case result.err == nil:
			successCount++
			assert.Equal(t, canonical.ID, result.transfer.ContactID)
		case errors.Is(result.err, errActiveAgentTransferExists):
			conflictCount++
		default:
			require.NoError(t, result.err)
		}
	}
	assert.Equal(t, 1, successCount)
	assert.Equal(t, creators-1, conflictCount)

	var canonicalActive, aliasActive int64
	require.NoError(t, app.DB.Model(&models.AgentTransfer{}).
		Where(
			"organization_id = ? AND contact_id = ? AND status = ?",
			org.ID,
			canonical.ID,
			models.TransferStatusActive,
		).
		Count(&canonicalActive).Error)
	require.NoError(t, app.DB.Model(&models.AgentTransfer{}).
		Where(
			"organization_id = ? AND contact_id = ? AND status = ?",
			org.ID,
			alias.ID,
			models.TransferStatusActive,
		).
		Count(&aliasActive).Error)
	assert.Equal(t, int64(1), canonicalActive)
	assert.Zero(t, aliasActive)
}

func TestSaveAndFinalizeTransfer_WaitsForMergeThenUsesCanonicalContact(t *testing.T) {
	app := newProcessorTestApp(t)
	org, account := createProcessorTestOrg(t, app)
	canonical := testutil.CreateTestContact(t, app.DB, org.ID)
	source := testutil.CreateTestContact(t, app.DB, org.ID)

	mergeTx := app.DB.Begin()
	require.NoError(t, mergeTx.Error)
	t.Cleanup(func() { _ = mergeTx.Rollback().Error })

	var lockedSource models.Contact
	require.NoError(t, mergeTx.Unscoped().
		Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("id = ? AND organization_id = ?", source.ID, org.ID).
		First(&lockedSource).Error)
	require.NoError(t, mergeTx.Unscoped().Model(&models.Contact{}).
		Where("id = ? AND organization_id = ?", source.ID, org.ID).
		Updates(map[string]any{
			"merged_into_id": canonical.ID,
			"deleted_at":     time.Now(),
		}).Error)

	staleContact := *source
	transfer := models.AgentTransfer{
		BaseModel:       models.BaseModel{ID: uuid.New()},
		OrganizationID:  org.ID,
		ContactID:       source.ID,
		WhatsAppAccount: account.Name,
		PhoneNumber:     source.PhoneNumber,
		Status:          models.TransferStatusActive,
		Source:          models.TransferSourceFlow,
		TransferredAt:   time.Now(),
	}
	resultCh := make(chan error, 1)
	go func() {
		resultCh <- app.saveAndFinalizeTransfer(
			&transfer,
			account,
			&staleContact,
			nil,
			false,
		)
	}()

	select {
	case err := <-resultCh:
		t.Fatalf("transfer creator bypassed the merge row lock: %v", err)
	case <-time.After(75 * time.Millisecond):
	}

	require.NoError(t, mergeTx.Commit().Error)
	require.NoError(t, <-resultCh)
	assert.Equal(t, canonical.ID, transfer.ContactID)

	var stored models.AgentTransfer
	require.NoError(t, app.DB.First(&stored, transfer.ID).Error)
	assert.Equal(t, canonical.ID, stored.ContactID)
}

func TestActiveTransferPartialUniqueIndexRejectsConcurrentDuplicates(t *testing.T) {
	app := newProcessorTestApp(t)
	org, account := createProcessorTestOrg(t, app)
	contact := testutil.CreateTestContact(t, app.DB, org.ID)

	start := make(chan struct{})
	results := make(chan error, 2)
	for range 2 {
		go func() {
			<-start
			transfer := models.AgentTransfer{
				BaseModel:       models.BaseModel{ID: uuid.New()},
				OrganizationID:  org.ID,
				ContactID:       contact.ID,
				WhatsAppAccount: account.Name,
				PhoneNumber:     contact.PhoneNumber,
				Status:          models.TransferStatusActive,
				Source:          models.TransferSourceManual,
				TransferredAt:   time.Now(),
			}
			results <- app.DB.Create(&transfer).Error
		}()
	}
	close(start)

	successCount := 0
	uniqueConflictCount := 0
	for range 2 {
		err := <-results
		if err == nil {
			successCount++
			continue
		}
		if isUniqueViolation(err) {
			uniqueConflictCount++
			continue
		}
		require.NoError(t, err)
	}
	assert.Equal(t, 1, successCount)
	assert.Equal(t, 1, uniqueConflictCount)
}
