package platformcompliance

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/config"
	"github.com/shridarpatil/whatomate/internal/database"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

const testOperatorRunID = "arham:review-20260821-001"

func TestValidateOptionsRequiresCanonicalIndependentFeaturesAndClinicSeparation(t *testing.T) {
	organizationID := uuid.New()
	tests := []struct {
		name    string
		options Options
		wantErr error
	}{
		{
			name: "canonical UUID required",
			options: Options{OrganizationID: strings.ToUpper(organizationID.String()),
				Features: []Feature{FeatureInstagram}, OperatorRunID: testOperatorRunID},
			wantErr: ErrInvalidOrganizationID,
		},
		{
			name: "operation required",
			options: Options{OrganizationID: organizationID.String(),
				OperatorRunID: testOperatorRunID},
			wantErr: ErrNoFeatures,
		},
		{
			name: "unknown feature rejected",
			options: Options{OrganizationID: organizationID.String(), Features: []Feature{"messenger"},
				OperatorRunID: testOperatorRunID},
			wantErr: ErrInvalidFeature,
		},
		{
			name: "duplicate feature rejected",
			options: Options{OrganizationID: organizationID.String(),
				Features: []Feature{FeatureThreads, FeatureThreads}, OperatorRunID: testOperatorRunID},
			wantErr: ErrDuplicateFeature,
		},
		{
			name: "operator run ID bounded",
			options: Options{OrganizationID: organizationID.String(), Features: []Feature{FeatureThreads},
				OperatorRunID: strings.Repeat("a", 97)},
			wantErr: ErrInvalidOperatorRunID,
		},
		{
			name: "runtime role safe identifier",
			options: Options{OrganizationID: organizationID.String(), Features: []Feature{FeatureThreads},
				OperatorRunID: testOperatorRunID, RuntimeRole: `unsafe"role`},
			wantErr: ErrInvalidRuntimeRole,
		},
		{
			name: "same feature cannot enable and remove",
			options: Options{OrganizationID: organizationID.String(), Features: []Feature{FeatureThreads},
				RemoveFeatures: []Feature{FeatureThreads}, OperatorRunID: testOperatorRunID},
			wantErr: ErrConflictingFeatureOperation,
		},
		{
			name: "creation cannot remove",
			options: Options{OrganizationID: organizationID.String(), CreatePurpose: true,
				RemoveFeatures: []Feature{FeatureThreads}, OperatorRunID: testOperatorRunID},
			wantErr: ErrCreateWithRemoval,
		},
		{
			name: "create with independently selected markers valid",
			options: Options{OrganizationID: organizationID.String(), CreatePurpose: true,
				Features: []Feature{FeatureThreads, FeatureInstagram}, OperatorRunID: testOperatorRunID},
		},
		{
			name: "explicit clinic collision",
			options: Options{OrganizationID: organizationID.String(), CreatePurpose: true,
				OperatorRunID: testOperatorRunID,
				ClinicScopes:  []ClinicScope{{Name: "instagram-active", OrganizationIDs: []string{organizationID.String()}}}},
			wantErr: ErrClinicOrganizationCollision,
		},
		{
			name: "allow-all exact exclusion valid",
			options: Options{OrganizationID: organizationID.String(), CreatePurpose: true,
				OperatorRunID: testOperatorRunID,
				ClinicScopes: []ClinicScope{{Name: "threads-active", AllowAll: true,
					ExcludedOrganizationIDs: []string{organizationID.String()}}}},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if test.options.RuntimeRole == "" {
				test.options.RuntimeRole = "rereply_app"
			}
			validated, err := validateOptions(test.options)
			if err == nil {
				err = validateClinicSeparation(validated.organizationID, validated.clinicScopes)
			}
			if test.wantErr == nil {
				require.NoError(t, err)
				return
			}
			assert.ErrorIs(t, err, test.wantErr)
		})
	}
}

func TestConfiguredClinicScopesAndReleaseReferences(t *testing.T) {
	messenger := uuid.NewString()
	instagramActive := uuid.NewString()
	instagramQuarantine := uuid.NewString()
	instagramCompliance := uuid.NewString()
	threadsActive := uuid.NewString()
	threadsCompliance := uuid.NewString()
	threadsDevelopment := uuid.NewString()
	cfg := &config.Config{
		MetaMessenger: config.MetaMessengerConfig{AllowedOrganizationIDs: messenger},
		MetaInstagram: config.MetaInstagramConfig{
			AllowedOrganizationIDs: instagramActive, QuarantinedOrganizationIDs: instagramQuarantine,
			DataDeletionComplianceOrganizationID: instagramCompliance,
		},
		ThreadsManaged: config.ThreadsManagedConfig{
			AllowedOrganizationIDs: threadsActive, ComplianceOrganizationID: threadsCompliance,
		},
		ThreadsAppReview: config.ThreadsAppReviewConfig{DevelopmentOrganizationID: threadsDevelopment},
	}

	scopes := ConfiguredClinicScopes(cfg)
	require.Len(t, scopes, 5)
	assert.Equal(t, []string{messenger}, scopes[0].OrganizationIDs)
	assert.Equal(t, []string{instagramActive}, scopes[1].OrganizationIDs)
	assert.Equal(t, []string{instagramQuarantine}, scopes[2].OrganizationIDs)
	assert.Equal(t, []string{threadsActive}, scopes[3].OrganizationIDs)
	assert.Equal(t, []string{threadsCompliance}, scopes[3].ExcludedOrganizationIDs)
	assert.Equal(t, []string{threadsDevelopment}, scopes[4].OrganizationIDs)

	references := ConfiguredReleaseReferences(cfg)
	require.Len(t, references, 2)
	assert.Equal(t, ReleaseReference{Name: "instagram-data-deletion-compliance",
		Feature: FeatureInstagram, OrganizationID: instagramCompliance}, references[0])
	assert.Equal(t, ReleaseReference{Name: "threads-managed-compliance",
		Feature: FeatureThreads, OrganizationID: threadsCompliance}, references[1])
}

func TestBootstrapAtomicallyCreatesPurposeOrganizationAndIsExactlyIdempotent(t *testing.T) {
	db, runtimeRole := preparePlatformComplianceRLS(t)
	organizationID := uuid.New()
	options := Options{
		OrganizationID: organizationID.String(), CreatePurpose: true,
		Features:      []Feature{FeatureThreads, FeatureInstagram},
		OperatorRunID: testOperatorRunID, RuntimeRole: runtimeRole,
	}

	dryRun, err := Bootstrap(context.Background(), db, options)
	require.NoError(t, err)
	assert.True(t, dryRun.PurposeCreated)
	assert.False(t, dryRun.ApplyRequested)
	assert.False(t, dryRun.AuditWritten)
	assert.Len(t, dryRun.Changed, 2)
	assertOrganizationAbsent(t, db, organizationID)

	options.Apply = true
	applied, err := Bootstrap(context.Background(), db, options)
	require.NoError(t, err)
	assert.True(t, applied.PurposeCreated)
	assert.True(t, applied.AuditWritten)

	var organization models.Organization
	require.NoError(t, db.Unscoped().Where("id = ?", organizationID).First(&organization).Error)
	assert.Equal(t, atomicOrganizationName(organizationID), organization.Name)
	assert.Equal(t, atomicOrganizationSlug(organizationID), organization.Slug)
	assert.Equal(t, models.JSONB{
		PurposeMarkerKey: true, InstagramMarkerKey: true, ThreadsMarkerKey: true,
	}, organization.Settings)
	assertAuditCount(t, db, organizationID, 1)

	// Idempotent dry-run performs no row lock: it remains readable while a
	// concurrent transaction owns the reseller row lock, and writes no evidence.
	holder := db.Begin()
	require.NoError(t, holder.Error)
	require.NoError(t, holder.Exec(
		"SELECT id FROM public.resellers WHERE id = ? FOR UPDATE", organization.ResellerID,
	).Error)
	dryRunContext, cancelDryRun := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancelDryRun()
	options.Apply = false
	idempotentDryRun, err := Bootstrap(dryRunContext, db, options)
	require.NoError(t, err)
	assert.True(t, idempotentDryRun.PurposeUnchanged)
	assert.False(t, idempotentDryRun.AuditWritten)
	require.NoError(t, holder.Rollback().Error)
	assertAuditCount(t, db, organizationID, 1)

	options.Apply = true
	repeated, err := Bootstrap(context.Background(), db, options)
	require.NoError(t, err)
	assert.False(t, repeated.PurposeCreated)
	assert.True(t, repeated.PurposeUnchanged)
	assert.False(t, repeated.AuditWritten)
	assert.Len(t, repeated.Unchanged, 2)
	assertAuditCount(t, db, organizationID, 1)

	options.OperatorRunID = "arham:review-20260821-different"
	_, err = Bootstrap(context.Background(), db, options)
	assert.ErrorIs(t, err, ErrOrganizationIdentityUsed)
	assertAuditCount(t, db, organizationID, 1)
}

func TestBootstrapNeverClassifiesAnExistingOrdinaryOrganization(t *testing.T) {
	db, runtimeRole := preparePlatformComplianceRLS(t)
	reseller := createPlatformReseller(t, db)
	organization := models.Organization{
		BaseModel: models.BaseModel{ID: uuid.New()}, ResellerID: &reseller.ID,
		Name: "Committed ordinary organization", Slug: "ordinary-" + uuid.NewString(), Settings: models.JSONB{},
	}
	require.NoError(t, db.Create(&organization).Error)

	_, err := Bootstrap(context.Background(), db, Options{
		OrganizationID: organization.ID.String(), CreatePurpose: true,
		Features: []Feature{FeatureInstagram}, OperatorRunID: testOperatorRunID,
		RuntimeRole: runtimeRole, Apply: true,
	})
	assert.ErrorIs(t, err, ErrOrganizationIdentityUsed)

	var persisted models.Organization
	require.NoError(t, db.Where("id = ?", organization.ID).First(&persisted).Error)
	_, exists := persisted.Settings[PurposeMarkerKey]
	assert.False(t, exists)
	assertAuditCount(t, db, organization.ID, 0)
}

func TestBootstrapFeatureMarkersAreIndependentAuditedAndRemovalSafe(t *testing.T) {
	db, runtimeRole := preparePlatformComplianceRLS(t)
	organizationID := uuid.New()
	create := Options{
		OrganizationID: organizationID.String(), CreatePurpose: true,
		Features: []Feature{FeatureInstagram}, OperatorRunID: testOperatorRunID,
		RuntimeRole: runtimeRole, Apply: true,
	}
	_, err := Bootstrap(context.Background(), db, create)
	require.NoError(t, err)

	enableThreads := Options{
		OrganizationID: organizationID.String(), Features: []Feature{FeatureInstagram, FeatureThreads},
		OperatorRunID: "arham:review-20260821-enable-threads", RuntimeRole: runtimeRole,
	}
	dryRun, err := Bootstrap(context.Background(), db, enableThreads)
	require.NoError(t, err)
	assert.Equal(t, []MarkerResult{{Feature: FeatureThreads, Key: ThreadsMarkerKey,
		Operation: OperationEnable}}, dryRun.Changed)
	assert.Equal(t, []MarkerResult{{Feature: FeatureInstagram, Key: InstagramMarkerKey,
		Operation: OperationEnable}}, dryRun.Unchanged)
	assert.False(t, dryRun.AuditWritten)
	assertAuditCount(t, db, organizationID, 1)

	enableThreads.Apply = true
	report, err := Bootstrap(context.Background(), db, enableThreads)
	require.NoError(t, err)
	assert.True(t, report.PurposeUnchanged)
	assert.True(t, report.AuditWritten)
	assert.Equal(t, []MarkerResult{{Feature: FeatureThreads, Key: ThreadsMarkerKey,
		Operation: OperationEnable}}, report.Changed)
	assert.Equal(t, []MarkerResult{{Feature: FeatureInstagram, Key: InstagramMarkerKey,
		Operation: OperationEnable}}, report.Unchanged)
	assertAuditCount(t, db, organizationID, 2)

	removeThreads := Options{
		OrganizationID: organizationID.String(), RemoveFeatures: []Feature{FeatureThreads},
		OperatorRunID: "arham:review-20260821-remove-threads", RuntimeRole: runtimeRole, Apply: true,
		ReleaseReferences: []ReleaseReference{{Name: "threads-release", Feature: FeatureThreads,
			OrganizationID: organizationID.String()}},
	}
	_, err = Bootstrap(context.Background(), db, removeThreads)
	assert.ErrorIs(t, err, ErrRemovalStillConfigured)
	assertAuditCount(t, db, organizationID, 2)

	removeThreads.ReleaseReferences = nil
	report, err = Bootstrap(context.Background(), db, removeThreads)
	require.NoError(t, err)
	assert.True(t, report.AuditWritten)
	assert.Equal(t, []MarkerResult{{Feature: FeatureThreads, Key: ThreadsMarkerKey,
		Operation: OperationRemove}}, report.Changed)
	assertAuditCount(t, db, organizationID, 3)

	var settings models.JSONB
	require.NoError(t, db.Raw("SELECT settings FROM public.organizations WHERE id = ?", organizationID).Scan(&settings).Error)
	assert.Equal(t, true, settings[PurposeMarkerKey])
	assert.Equal(t, true, settings[InstagramMarkerKey])
	_, threadsExists := settings[ThreadsMarkerKey]
	assert.False(t, threadsExists)
}

func TestBootstrapClinicCollisionIsReadOnlyAndFailClosed(t *testing.T) {
	db, runtimeRole := preparePlatformComplianceRLS(t)
	organizationID := uuid.New()
	_, err := Bootstrap(context.Background(), db, Options{
		OrganizationID: organizationID.String(), CreatePurpose: true,
		Features: []Feature{FeatureInstagram}, OperatorRunID: testOperatorRunID,
		RuntimeRole:  runtimeRole,
		ClinicScopes: []ClinicScope{{Name: "instagram-active", OrganizationIDs: []string{organizationID.String()}}},
	})
	assert.ErrorIs(t, err, ErrClinicOrganizationCollision)
	assertOrganizationAbsent(t, db, organizationID)
	assertAuditCount(t, db, organizationID, 0)
}

func TestBootstrapRejectsGuardDriftWithoutCreationOrAudit(t *testing.T) {
	db, runtimeRole := preparePlatformComplianceRLS(t)
	tests := []struct {
		name   string
		mutate string
	}{
		{
			name: "missing organization audit trigger",
			mutate: `DROP TRIGGER rereply_platform_compliance_creation_audit
				ON public.organizations`,
		},
		{
			name: "disabled organization classification trigger",
			mutate: `ALTER TABLE public.organizations
				DISABLE TRIGGER rereply_platform_compliance_classification_guard`,
		},
		{
			name: "tampered classification function",
			mutate: `CREATE OR REPLACE FUNCTION public.rereply_platform_compliance_classification_guard()
				RETURNS trigger LANGUAGE plpgsql SECURITY DEFINER
				SET search_path = pg_catalog, public
				AS $function$ BEGIN RETURN NEW; END $function$`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			organizationID := uuid.New()
			require.NoError(t, db.Exec(test.mutate).Error)
			t.Cleanup(func() {
				if err := database.ApplyTenantRLS(db, runtimeRole); err != nil {
					t.Errorf("repair platform compliance guard contract: %v", err)
				}
			})
			_, err := Bootstrap(context.Background(), db, Options{
				OrganizationID: organizationID.String(), CreatePurpose: true,
				Features: []Feature{FeatureInstagram}, OperatorRunID: testOperatorRunID,
				RuntimeRole: runtimeRole, Apply: true,
			})
			require.Error(t, err)
			assertOrganizationAbsent(t, db, organizationID)
			assertAuditCount(t, db, organizationID, 0)
			require.NoError(t, database.ApplyTenantRLS(db, runtimeRole))
		})
	}
}

func preparePlatformComplianceRLS(t *testing.T) (*gorm.DB, string) {
	t.Helper()
	db := testutil.SetupTestDB(t)
	testutil.TruncateTables(db)
	require.NoError(t, database.RemoveTenantRLS(db))
	runtimeRole := "rereply_compliance_test_" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	require.NoError(t, db.Exec(
		"CREATE ROLE "+runtimeRole+" NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT NOBYPASSRLS",
	).Error)
	t.Cleanup(func() {
		testutil.TruncateTables(db)
		require.NoError(t, database.RemoveTenantRLS(db))
		require.NoError(t, db.Exec("DROP OWNED BY "+runtimeRole).Error)
		require.NoError(t, db.Exec("DROP ROLE IF EXISTS "+runtimeRole).Error)
	})
	require.NoError(t, database.ApplyTenantRLS(db, runtimeRole))
	createPlatformReseller(t, db)
	return db, runtimeRole
}

func createPlatformReseller(t *testing.T, db *gorm.DB) models.Reseller {
	t.Helper()
	var existing models.Reseller
	err := db.Where("slug = ?", database.PlatformResellerSlug).First(&existing).Error
	if err == nil {
		return existing
	}
	require.ErrorIs(t, err, gorm.ErrRecordNotFound)
	reseller := models.Reseller{
		BaseModel: models.BaseModel{ID: uuid.New()}, Name: "Synthetic Platform Direct",
		Slug: database.PlatformResellerSlug, Status: models.ResellerStatusActive,
		Plan: models.ResellerPlanEnterprise, MaxOrganizations: 100, Settings: models.JSONB{},
	}
	require.NoError(t, db.Create(&reseller).Error)
	return reseller
}

func assertOrganizationAbsent(t *testing.T, db *gorm.DB, organizationID uuid.UUID) {
	t.Helper()
	var count int64
	require.NoError(t, db.Unscoped().Model(&models.Organization{}).
		Where("id = ?", organizationID).Count(&count).Error)
	assert.Zero(t, count)
}

func assertAuditCount(t *testing.T, db *gorm.DB, organizationID uuid.UUID, want int64) {
	t.Helper()
	var count int64
	require.NoError(t, db.Model(&models.AuditLog{}).
		Where("organization_id = ? AND resource_type = ?", organizationID, auditResourceType).
		Count(&count).Error)
	assert.Equal(t, want, count)
}
