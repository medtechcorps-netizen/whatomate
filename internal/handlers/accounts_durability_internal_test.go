package handlers

import (
	"testing"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/config"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestManualWhatsAppStatusCheckpointsBypassRequestTransaction(t *testing.T) {
	db := testutil.SetupTestDB(t)
	organization := testutil.CreateTestOrganization(t, db)
	account := models.WhatsAppAccount{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: organization.ID,
		Name:           "manual durability " + uuid.NewString(),
		PhoneID:        "manual-durability-" + uuid.NewString(),
		BusinessID:     "manual-waba-" + uuid.NewString(),
		AccessToken:    "token",
		APIVersion:     "v21.0",
		Status:         "active",
	}
	require.NoError(t, db.Create(&account).Error)

	root := &App{
		Config: &config.Config{},
		DB:     db,
		Log:    testutil.NopLogger(),
	}
	requestTx := db.Begin()
	require.NoError(t, requestTx.Error)
	scoped := root.scopedApp(requestTx, organization.ID)

	attemptID := uuid.New()
	_, err := scoped.beginWhatsAppAccountAttempt(
		organization.ID,
		account.ID,
		attemptID,
		whatsAppAccountAttemptOptions{
			ExpectedPhoneID: account.PhoneID,
			Updates:         map[string]any{"status": "pending_subscription"},
		},
	)
	require.NoError(t, err)
	var persisted models.WhatsAppAccount
	require.NoError(t, db.First(&persisted, "id = ?", account.ID).Error)
	assert.Equal(t, "pending_subscription", persisted.Status)

	require.NoError(t, scoped.finishWhatsAppAccountAttempt(
		organization.ID,
		account.ID,
		attemptID,
		map[string]any{"status": "active"},
	))
	require.NoError(t, requestTx.Rollback().Error)

	require.NoError(t, db.First(&persisted, "id = ?", account.ID).Error)
	assert.Equal(t, "active", persisted.Status, "final outcome must survive rollback of the request transaction")
}
