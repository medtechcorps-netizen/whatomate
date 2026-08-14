package database_test

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/database"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/require"
)

func TestCreateIndexesRejectsNormalizedLiveWhatsAppPhoneIDDuplicatesWithoutMutation(t *testing.T) {
	db := testutil.SetupTestDB(t)
	testutil.TruncateTables(db)
	t.Cleanup(func() {
		testutil.TruncateTables(db)
		require.NoError(t, database.CreateIndexes(db))
	})

	require.NoError(t, db.Exec(
		"DROP INDEX IF EXISTS public.uq_whatsapp_accounts_live_phone_id",
	).Error)
	orgA := testutil.CreateTestOrganization(t, db)
	orgB := testutil.CreateTestOrganization(t, db)
	phoneID := "migration-phone-" + uuid.NewString()
	accountA := newWhatsAppRoutingAccount(orgA.ID, "tenant-a", "  "+phoneID)
	accountB := newWhatsAppRoutingAccount(orgB.ID, "tenant-b", phoneID+"  ")
	require.NoError(t, db.Create(&accountA).Error)
	require.NoError(t, db.Create(&accountB).Error)

	err := database.CreateIndexes(db)
	require.Error(t, err)
	require.Contains(t, err.Error(), "cannot enforce unique live WhatsApp Phone IDs")
	require.NotContains(t, err.Error(), phoneID)

	var liveCount int64
	require.NoError(t, db.Model(&models.WhatsAppAccount{}).
		Where("id IN ? AND deleted_at IS NULL", []uuid.UUID{accountA.ID, accountB.ID}).
		Count(&liveCount).Error)
	require.Equal(t, int64(2), liveCount)
}

func TestWhatsAppPhoneIDRoutingIndexNormalizesAndIgnoresSoftDeletedRows(t *testing.T) {
	db := testutil.SetupTestDB(t)
	testutil.TruncateTables(db)
	require.NoError(t, database.CreateIndexes(db))
	orgA := testutil.CreateTestOrganization(t, db)
	orgB := testutil.CreateTestOrganization(t, db)
	phoneID := "indexed-phone-" + uuid.NewString()
	accountA := newWhatsAppRoutingAccount(orgA.ID, "tenant-a", " "+phoneID)
	require.NoError(t, db.Create(&accountA).Error)

	accountB := newWhatsAppRoutingAccount(orgB.ID, "tenant-b", phoneID+" ")
	require.Error(t, db.Create(&accountB).Error)

	require.NoError(t, db.Delete(&accountA).Error)
	accountB.ID = uuid.New()
	accountB.OrganizationID = orgA.ID
	accountB.CreatedAt = time.Time{}
	accountB.UpdatedAt = time.Time{}
	require.NoError(t, db.Create(&accountB).Error)
}

func TestCreateIndexesPersistsCanonicalPhoneIDsForRollbackResolver(t *testing.T) {
	db := testutil.SetupTestDB(t)
	testutil.TruncateTables(db)
	require.NoError(t, db.Exec(
		"DROP INDEX IF EXISTS public.uq_whatsapp_accounts_live_phone_id",
	).Error)
	org := testutil.CreateTestOrganization(t, db)
	phoneID := "rollback-phone-" + uuid.NewString()
	account := newWhatsAppRoutingAccount(org.ID, "rollback", "  "+phoneID+"  ")
	require.NoError(t, db.Create(&account).Error)

	require.NoError(t, database.CreateIndexes(db))

	var stored models.WhatsAppAccount
	require.NoError(t, db.Unscoped().Where("id = ?", account.ID).First(&stored).Error)
	require.Equal(t, phoneID, stored.PhoneID)

	var resolvedOrgID string
	require.NoError(t, db.Raw(
		"SELECT organization_id::text FROM whatsapp_accounts WHERE phone_id = ? AND deleted_at IS NULL",
		phoneID,
	).Scan(&resolvedOrgID).Error)
	require.Equal(t, org.ID.String(), resolvedOrgID)
}

func newWhatsAppRoutingAccount(
	organizationID uuid.UUID,
	name string,
	phoneID string,
) models.WhatsAppAccount {
	return models.WhatsAppAccount{
		BaseModel:          models.BaseModel{ID: uuid.New()},
		OrganizationID:     organizationID,
		Name:               name + "-" + uuid.NewString(),
		PhoneID:            phoneID,
		BusinessID:         "waba-" + uuid.NewString(),
		AccessToken:        "encrypted-test-token",
		WebhookVerifyToken: "verify-" + uuid.NewString(),
	}
}
