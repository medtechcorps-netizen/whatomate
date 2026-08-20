package threadsplatform

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/config"
	"github.com/shridarpatil/whatomate/internal/database"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func createManagedIntegration(
	t *testing.T,
	db *gorm.DB,
	organizationID uuid.UUID,
	platformAppKey string,
) models.ProviderIntegration {
	t.Helper()
	integration := models.ProviderIntegration{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: organizationID,
		Provider:       "threads",
		ManagementMode: models.ThreadsManagementModePlatformManaged,
		PlatformAppKey: &platformAppKey,
		Enabled:        false,
		Config:         models.JSONB{},
		CredentialData: models.JSONB{},
	}
	require.NoError(t, db.Create(&integration).Error)
	return integration
}

func TestPostgresStoreClaimsGloballyUniqueLiveSubjectAndAssetWithoutTransfer(t *testing.T) {
	db := testutil.SetupTestDB(t)
	testutil.TruncateTables(db)
	organizationA := testutil.CreateTestOrganization(t, db)
	organizationB := testutil.CreateTestOrganization(t, db)
	integrationA := createManagedIntegration(t, db, organizationA.ID, "legacy_alias")
	integrationB := createManagedIntegration(t, db, organizationB.ID, "renamed_alias")
	store := NewPostgresStore(db)

	first, err := store.ClaimBinding(context.Background(), BindingClaim{
		OrganizationID:          organizationA.ID,
		IntegrationID:           integrationA.ID,
		PlatformAppKey:          "legacy_alias",
		PlatformAppID:           "123456789012345",
		OAuthSubjectID:          "200000000000001",
		AuthorityAssetID:        "300000000000001",
		ConfigurationGeneration: 7,
	})
	require.NoError(t, err)
	assert.Equal(t, models.ThreadsPlatformBindingStatusPending, first.Status)

	_, err = store.ClaimBinding(context.Background(), BindingClaim{
		OrganizationID:          organizationB.ID,
		IntegrationID:           integrationB.ID,
		PlatformAppKey:          "renamed_alias",
		PlatformAppID:           "123456789012345",
		OAuthSubjectID:          "200000000000001",
		AuthorityAssetID:        "300000000000002",
		ConfigurationGeneration: 7,
	})
	assert.ErrorIs(t, err, ErrBindingClaimConflict)

	_, err = store.ClaimBinding(context.Background(), BindingClaim{
		OrganizationID:          organizationB.ID,
		IntegrationID:           integrationB.ID,
		PlatformAppKey:          "renamed_alias",
		PlatformAppID:           "123456789012345",
		OAuthSubjectID:          "200000000000002",
		AuthorityAssetID:        "300000000000001",
		ConfigurationGeneration: 7,
	})
	assert.ErrorIs(t, err, ErrBindingClaimConflict)

	var total, organizationBTotal int64
	require.NoError(t, db.Model(&models.ThreadsPlatformBinding{}).Count(&total).Error)
	require.NoError(t, db.Model(&models.ThreadsPlatformBinding{}).
		Where("organization_id = ?", organizationB.ID).
		Count(&organizationBTotal).Error)
	assert.Equal(t, int64(1), total)
	assert.Zero(t, organizationBTotal, "a conflict must not partially persist a foreign claim")

	require.NoError(t, store.ReleaseBinding(
		context.Background(),
		organizationA.ID,
		first.ID,
		"operator_release",
	))
	replacement, err := store.ClaimBinding(context.Background(), BindingClaim{
		OrganizationID:          organizationB.ID,
		IntegrationID:           integrationB.ID,
		PlatformAppKey:          "renamed_alias",
		PlatformAppID:           "123456789012345",
		OAuthSubjectID:          "200000000000001",
		AuthorityAssetID:        "300000000000001",
		ConfigurationGeneration: 7,
	})
	require.NoError(t, err)
	assert.Equal(t, organizationB.ID, replacement.OrganizationID)
}

func TestReconnectBindingTxOnlyRotatesExactClaimAndReturnsItToPending(t *testing.T) {
	db := testutil.SetupTestDB(t)
	testutil.TruncateTables(db)
	organization := testutil.CreateTestOrganization(t, db)
	integration := createManagedIntegration(t, db, organization.ID, "primary")
	account := models.ChannelAccount{
		BaseModel:         models.BaseModel{ID: uuid.New()},
		OrganizationID:    organization.ID,
		Channel:           models.ChannelThreads,
		Provider:          "threads",
		Name:              "Managed Threads",
		ExternalAccountID: "300000000000001",
		Status:            models.ChannelAccountStatusPending,
		Capabilities:      models.JSONB{},
		Config: models.JSONB{
			"management_mode": models.ThreadsManagementModePlatformManaged,
		},
		Metadata: models.JSONB{},
	}
	require.NoError(t, db.Create(&account).Error)
	store := NewPostgresStore(db)
	binding, err := store.ClaimBinding(context.Background(), BindingClaim{
		OrganizationID:          organization.ID,
		IntegrationID:           integration.ID,
		ChannelAccountID:        &account.ID,
		PlatformAppKey:          "primary",
		PlatformAppID:           "123456789012345",
		OAuthSubjectID:          "200000000000001",
		AuthorityAssetID:        account.ExternalAccountID,
		ConfigurationGeneration: 7,
		AuthorizationGeneration: 1,
	})
	require.NoError(t, err)
	require.NoError(t, db.Model(&models.ThreadsPlatformBinding{}).Where(
		"id = ?", binding.ID,
	).Update("status", models.ThreadsPlatformBindingStatusActive).Error)

	reconnect := BindingReconnect{
		OrganizationID:                  organization.ID,
		BindingID:                       binding.ID,
		IntegrationID:                   integration.ID,
		ChannelAccountID:                account.ID,
		PlatformAppKey:                  "primary",
		PlatformAppID:                   "123456789012345",
		OAuthSubjectID:                  "200000000000001",
		AuthorityAssetID:                account.ExternalAccountID,
		ConfigurationGeneration:         7,
		ExpectedAuthorizationGeneration: 1,
	}
	var rotated *models.ThreadsPlatformBinding
	require.NoError(t, database.WithTenant(db, organization.ID, func(tx *gorm.DB) error {
		var rotateErr error
		rotated, rotateErr = ReconnectBindingTx(tx, reconnect)
		return rotateErr
	}))
	require.NotNil(t, rotated)
	assert.Equal(t, uint64(2), rotated.AuthorizationGeneration)
	assert.Equal(t, models.ThreadsPlatformBindingStatusPending, rotated.Status)

	stale := reconnect
	stale.OAuthSubjectID = "200000000000002"
	err = database.WithTenant(db, organization.ID, func(tx *gorm.DB) error {
		_, reconnectErr := ReconnectBindingTx(tx, stale)
		return reconnectErr
	})
	assert.ErrorIs(t, err, ErrBindingReconnectFence)

	var persisted models.ThreadsPlatformBinding
	require.NoError(t, db.First(&persisted, "id = ?", binding.ID).Error)
	assert.Equal(t, uint64(2), persisted.AuthorizationGeneration)
	assert.Equal(t, "200000000000001", persisted.OAuthSubjectID)
	assert.Equal(t, "300000000000001", persisted.AuthorityAssetID)
	assert.Equal(t, models.ThreadsPlatformBindingStatusPending, persisted.Status)
}

func TestPostgresStoreRejectsBYOIntegrationBinding(t *testing.T) {
	db := testutil.SetupTestDB(t)
	testutil.TruncateTables(db)
	organization := testutil.CreateTestOrganization(t, db)
	byo := models.ProviderIntegration{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: organization.ID,
		Provider:       "threads",
		ManagementMode: models.ThreadsManagementModeWorkspaceBYO,
		Config:         models.JSONB{},
		CredentialData: models.JSONB{},
	}
	require.NoError(t, db.Create(&byo).Error)

	_, err := NewPostgresStore(db).ClaimBinding(context.Background(), BindingClaim{
		OrganizationID:          organization.ID,
		IntegrationID:           byo.ID,
		PlatformAppKey:          "primary",
		PlatformAppID:           "123456789012345",
		OAuthSubjectID:          "200000000000001",
		AuthorityAssetID:        "300000000000001",
		ConfigurationGeneration: 7,
	})
	assert.ErrorIs(t, err, ErrIntegrationNotManaged)

	var count int64
	require.NoError(t, db.Model(&models.ThreadsPlatformBinding{}).Count(&count).Error)
	assert.Zero(t, count)
}

func TestPostgresStoreJournalIsDigestOnlyAndIdempotent(t *testing.T) {
	db := testutil.SetupTestDB(t)
	testutil.TruncateTables(db)
	store := NewPostgresStore(db)
	receipt := JournalReceipt{
		PlatformAppKey:          "primary",
		PlatformAppID:           "123456789012345",
		EventDigest:             strings.Repeat("d", 64),
		EventType:               models.ThreadsPlatformEventDataDeletion,
		SubjectDigest:           strings.Repeat("e", 64),
		ConfigurationGeneration: 7,
	}
	created, err := store.RecordJournalReceipt(context.Background(), receipt)
	require.NoError(t, err)
	assert.True(t, created)
	created, err = store.RecordJournalReceipt(context.Background(), receipt)
	require.NoError(t, err)
	assert.False(t, created)

	var events []models.ThreadsPlatformEventJournal
	require.NoError(t, db.Find(&events).Error)
	require.Len(t, events, 1)
	assert.Equal(t, models.ThreadsPlatformRoutingReceived, events[0].RoutingState)
	assert.Equal(t, receipt.SubjectDigest, events[0].SubjectDigest)

	_, err = store.RecordJournalReceipt(context.Background(), JournalReceipt{
		PlatformAppKey:          "primary",
		PlatformAppID:           "123456789012345",
		EventDigest:             strings.Repeat("f", 64),
		EventType:               models.ThreadsPlatformEventDataDeletion,
		SubjectDigest:           "raw-subject-id",
		ConfigurationGeneration: 7,
	})
	assert.ErrorIs(t, err, ErrInvalidJournalReceipt)
}

func TestRuntimeRequiresMarkedPlatformOwnedComplianceOrganization(t *testing.T) {
	db := testutil.SetupTestDB(t)
	testutil.TruncateTables(db)
	reseller := models.Reseller{
		BaseModel:        models.BaseModel{ID: uuid.New()},
		Name:             "Synthetic Platform Direct",
		Slug:             database.PlatformResellerSlug,
		Status:           models.ResellerStatusActive,
		Plan:             models.ResellerPlanEnterprise,
		MaxOrganizations: 10,
		Settings:         models.JSONB{},
	}
	require.NoError(t, db.Create(&reseller).Error)
	complianceOrganization := models.Organization{
		BaseModel:  models.BaseModel{ID: runtimeComplianceID},
		ResellerID: &reseller.ID,
		Name:       "Synthetic Threads Compliance",
		Slug:       "synthetic-threads-compliance",
		Settings:   models.JSONB{},
	}
	require.NoError(t, db.Create(&complianceOrganization).Error)
	runtime, err := NewRuntime(runtimeManagedConfig(), config.ThreadsAppReviewConfig{}, "staging")
	require.NoError(t, err)
	_, err = runtime.ValidateComplianceOrganization(context.Background(), db)
	assert.ErrorIs(t, err, ErrComplianceOrganizationInvalid)

	complianceOrganization.Settings[ComplianceOrganizationMarkerKey] = true
	require.NoError(t, db.Model(&complianceOrganization).
		Update("settings", complianceOrganization.Settings).Error)
	validated, err := runtime.ValidateComplianceOrganization(context.Background(), db)
	require.NoError(t, err)
	credentials, err := validated.Credentials("primary")
	require.NoError(t, err)
	assert.Equal(t, "[REDACTED]", fmt.Sprint(credentials))
	assert.Equal(t, "[REDACTED]", fmt.Sprintf("%+v", credentials))
	assert.Equal(t, "[REDACTED]", fmt.Sprintf("%#v", credentials))
	encodedCredentials, err := json.Marshal(credentials)
	require.NoError(t, err)
	assert.Equal(t, `{}`, string(encodedCredentials))

	require.NoError(t, db.Model(&reseller).Update("slug", "clinic-reseller").Error)
	_, err = runtime.ValidateComplianceOrganization(context.Background(), db)
	assert.ErrorIs(t, err, ErrComplianceOrganizationInvalid)
}
