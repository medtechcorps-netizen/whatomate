package database_test

import (
	"testing"

	"github.com/shridarpatil/whatomate/internal/database"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEnsurePlatformResellerBackfillsOrganizationsAndOwnersIdempotently(t *testing.T) {
	db := testutil.SetupTestDB(t)
	tx := db.Begin()
	require.NoError(t, tx.Error)
	t.Cleanup(func() { tx.Rollback() })

	legacyOrganization := testutil.CreateTestOrganization(t, tx)
	superAdmin := testutil.CreateTestUser(
		t,
		tx,
		legacyOrganization.ID,
		testutil.WithEmail(testutil.UniqueEmail("platform-backfill")),
		testutil.WithSuperAdmin(),
	)

	require.NoError(t, database.EnsurePlatformReseller(tx))
	require.NoError(t, database.EnsurePlatformReseller(tx))

	var platform models.Reseller
	require.NoError(t, tx.Where("slug = ?", "platform-direct").First(&platform).Error)
	assert.Equal(t, models.ResellerPlanEnterprise, platform.Plan)
	assert.Equal(t, models.ResellerStatusActive, platform.Status)

	var updatedOrganization models.Organization
	require.NoError(t, tx.Where("id = ?", legacyOrganization.ID).First(&updatedOrganization).Error)
	require.NotNil(t, updatedOrganization.ResellerID)
	assert.Equal(t, platform.ID, *updatedOrganization.ResellerID)

	var memberships int64
	require.NoError(t, tx.Model(&models.ResellerMember{}).
		Where(
			"reseller_id = ? AND user_id = ? AND role = ? AND is_active = ?",
			platform.ID, superAdmin.ID, models.ResellerRoleOwner, true,
		).
		Count(&memberships).Error)
	assert.EqualValues(t, 1, memberships)
}
