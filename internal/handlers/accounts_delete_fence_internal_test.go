package handlers

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/config"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDeleteWhatsAppAccountRevokesAttemptAndPreventsLeasedFinalization(t *testing.T) {
	db := testutil.SetupTestDB(t)
	organization := testutil.CreateTestOrganization(t, db)
	attemptID := uuid.New()
	attemptStartedAt := time.Now().UTC()
	account := models.WhatsAppAccount{
		BaseModel:                  models.BaseModel{ID: uuid.New()},
		OrganizationID:             organization.ID,
		Name:                       "delete-fenced-account-" + uuid.NewString(),
		PhoneID:                    "delete-fenced-phone-" + uuid.NewString(),
		BusinessID:                 "delete-fenced-waba-" + uuid.NewString(),
		AccessToken:                "encrypted-token",
		APIVersion:                 "v21.0",
		Status:                     "pending_subscription",
		ConnectionAttemptID:        &attemptID,
		ConnectionAttemptStartedAt: &attemptStartedAt,
	}
	require.NoError(t, db.Create(&account).Error)

	app := &App{
		Config: &config.Config{},
		DB:     db,
		Log:    testutil.NopLogger(),
	}
	staleFinalization := account
	staleFinalization.Status = "active"
	currentPhoneID := "delete-fenced-current-phone-" + uuid.NewString()
	require.NoError(t, db.Model(&models.WhatsAppAccount{}).
		Where("id = ?", account.ID).
		Update("phone_id", currentPhoneID).Error)

	deletedPhoneID, err := app.deleteWhatsAppAccountIndependent(
		organization.ID,
		account.ID,
		account.PhoneID,
	)
	require.NoError(t, err)
	assert.Equal(t, currentPhoneID, deletedPhoneID, "delete must return the phone read under its row lock")
	err = app.persistEmbeddedSignupAccount(
		organization.ID,
		&staleFinalization,
		attemptID,
		true,
	)
	require.ErrorIs(t, err, errEmbeddedSignupAttemptSuperseded)

	var deleted models.WhatsAppAccount
	require.NoError(t, db.Unscoped().First(&deleted, "id = ?", account.ID).Error)
	assert.True(t, deleted.DeletedAt.Valid)
	assert.Nil(t, deleted.ConnectionAttemptID)
	assert.Nil(t, deleted.ConnectionAttemptStartedAt)
	assert.Equal(t, "pending_subscription", deleted.Status)
}

func TestPersistEmbeddedSignupAccountCannotResurrectDeletedLease(t *testing.T) {
	db := testutil.SetupTestDB(t)
	organization := testutil.CreateTestOrganization(t, db)
	attemptID := uuid.New()
	attemptStartedAt := time.Now().UTC()
	account := models.WhatsAppAccount{
		BaseModel:                  models.BaseModel{ID: uuid.New()},
		OrganizationID:             organization.ID,
		Name:                       "deleted-lease-account-" + uuid.NewString(),
		PhoneID:                    "deleted-lease-phone-" + uuid.NewString(),
		BusinessID:                 "deleted-lease-waba-" + uuid.NewString(),
		AccessToken:                "encrypted-token",
		APIVersion:                 "v21.0",
		Status:                     "pending_subscription",
		ConnectionAttemptID:        &attemptID,
		ConnectionAttemptStartedAt: &attemptStartedAt,
	}
	require.NoError(t, db.Create(&account).Error)

	// Model a delete that has already committed while a worker still holds its
	// leased in-memory copy. Even if the deleted row retains that lease value,
	// finalization must not match it or write DeletedAt back to NULL.
	staleFinalization := account
	staleFinalization.Status = "active"
	require.NoError(t, db.Delete(&account).Error)

	app := &App{
		Config: &config.Config{},
		DB:     db,
		Log:    testutil.NopLogger(),
	}
	err := app.persistEmbeddedSignupAccount(
		organization.ID,
		&staleFinalization,
		attemptID,
		true,
	)
	require.ErrorIs(t, err, errEmbeddedSignupAttemptSuperseded)

	var deleted models.WhatsAppAccount
	require.NoError(t, db.Unscoped().First(&deleted, "id = ?", account.ID).Error)
	assert.True(t, deleted.DeletedAt.Valid)
	assert.Equal(t, "pending_subscription", deleted.Status)
	require.NotNil(t, deleted.ConnectionAttemptID)
	assert.Equal(t, attemptID, *deleted.ConnectionAttemptID)
}

func TestClaimEmbeddedSignupAccountRejectsDeleteAfterActivePreflight(t *testing.T) {
	db := testutil.SetupTestDB(t)
	organization := testutil.CreateTestOrganization(t, db)
	account := models.WhatsAppAccount{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: organization.ID,
		Name:           "preclaim-delete-account-" + uuid.NewString(),
		PhoneID:        "preclaim-delete-phone-" + uuid.NewString(),
		BusinessID:     "preclaim-delete-waba-" + uuid.NewString(),
		AccessToken:    "encrypted-token",
		APIVersion:     "v21.0",
		Status:         "active",
	}
	require.NoError(t, db.Create(&account).Error)

	app := &App{
		Config: &config.Config{},
		DB:     db,
		Log:    testutil.NopLogger(),
	}
	var preflight models.WhatsAppAccount
	require.NoError(t, db.First(&preflight, "id = ?", account.ID).Error)
	candidate := preflight
	candidate.Status = "pending_registration"

	_, err := app.deleteWhatsAppAccountIndependent(
		organization.ID,
		account.ID,
		account.PhoneID,
	)
	require.NoError(t, err)

	_, _, err = app.claimEmbeddedSignupAccount(
		organization.ID,
		&candidate,
		uuid.New(),
		&preflight,
	)
	require.ErrorIs(t, err, errEmbeddedSignupAttemptSuperseded)

	var deleted models.WhatsAppAccount
	require.NoError(t, db.Unscoped().First(&deleted, "id = ?", account.ID).Error)
	assert.True(t, deleted.DeletedAt.Valid)
	assert.Nil(t, deleted.ConnectionAttemptID)
	assert.Nil(t, deleted.ConnectionAttemptStartedAt)
}

func TestClaimEmbeddedSignupAccountOnlyRestoresExactPreflightTombstone(t *testing.T) {
	newDeletedAccount := func(t *testing.T) (*App, models.WhatsAppAccount, models.WhatsAppAccount) {
		t.Helper()
		db := testutil.SetupTestDB(t)
		organization := testutil.CreateTestOrganization(t, db)
		account := models.WhatsAppAccount{
			BaseModel:      models.BaseModel{ID: uuid.New()},
			OrganizationID: organization.ID,
			Name:           "preflight-tombstone-account-" + uuid.NewString(),
			PhoneID:        "preflight-tombstone-phone-" + uuid.NewString(),
			BusinessID:     "preflight-tombstone-waba-" + uuid.NewString(),
			AccessToken:    "encrypted-token",
			APIVersion:     "v21.0",
			Status:         "active",
		}
		require.NoError(t, db.Create(&account).Error)
		require.NoError(t, db.Delete(&account).Error)

		var tombstone models.WhatsAppAccount
		require.NoError(t, db.Unscoped().First(&tombstone, "id = ?", account.ID).Error)
		return &App{Config: &config.Config{}, DB: db, Log: testutil.NopLogger()}, account, tombstone
	}

	t.Run("same deletion generation can reconnect", func(t *testing.T) {
		app, account, preflight := newDeletedAccount(t)
		candidate := preflight
		candidate.Status = "pending_registration"
		attemptID := uuid.New()

		existing, _, err := app.claimEmbeddedSignupAccount(
			account.OrganizationID,
			&candidate,
			attemptID,
			&preflight,
		)
		require.NoError(t, err)
		assert.True(t, existing)

		stored := loadWhatsAppLeaseTestAccount(t, app, account.OrganizationID, account.ID)
		assert.False(t, stored.DeletedAt.Valid)
		require.NotNil(t, stored.ConnectionAttemptID)
		assert.Equal(t, attemptID, *stored.ConnectionAttemptID)
	})

	t.Run("newer deletion generation is rejected", func(t *testing.T) {
		app, account, preflight := newDeletedAccount(t)
		newDeletedAt := preflight.DeletedAt.Time.Add(time.Second)
		require.NoError(t, app.DB.Unscoped().Model(&models.WhatsAppAccount{}).
			Where("id = ?", account.ID).
			UpdateColumn("deleted_at", newDeletedAt).Error)

		candidate := preflight
		candidate.Status = "pending_registration"
		_, _, err := app.claimEmbeddedSignupAccount(
			account.OrganizationID,
			&candidate,
			uuid.New(),
			&preflight,
		)
		require.ErrorIs(t, err, errEmbeddedSignupAttemptSuperseded)

		var deleted models.WhatsAppAccount
		require.NoError(t, app.DB.Unscoped().First(&deleted, "id = ?", account.ID).Error)
		assert.True(t, deleted.DeletedAt.Valid)
		assert.True(t, deleted.DeletedAt.Time.Equal(newDeletedAt))
		assert.Nil(t, deleted.ConnectionAttemptID)
	})
}
