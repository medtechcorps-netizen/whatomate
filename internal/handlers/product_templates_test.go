package handlers

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestProductCommercialBuiltInTemplateProvisioningDefinitions(t *testing.T) {
	t.Parallel()

	for _, template := range productBuiltInWorkspaceTemplates {
		template := template
		t.Run(template.Key, func(t *testing.T) {
			t.Parallel()

			require.NotEmpty(t, template.Pipeline.Key)
			require.NotEmpty(t, template.Pipeline.Stages)
			require.NotEmpty(t, template.Services)

			seenStageKeys := map[string]bool{}
			won := 0
			lost := 0
			for order, stage := range template.Pipeline.Stages {
				require.False(t, seenStageKeys[stage.Key], "duplicate stage key %q", stage.Key)
				seenStageKeys[stage.Key] = true
				assert.NotEmpty(t, stage.Name)
				assert.GreaterOrEqual(t, stage.Probability, 0)
				assert.LessOrEqual(t, stage.Probability, 100)
				if stage.Kind == models.CRMPipelineStageKindWon {
					won++
					assert.Equal(t, 100, stage.Probability)
				}
				if stage.Kind == models.CRMPipelineStageKindLost {
					lost++
				}
				if order > 0 && stage.Kind == models.CRMPipelineStageKindOpen {
					assert.GreaterOrEqual(
						t,
						stage.Probability,
						template.Pipeline.Stages[order-1].Probability,
					)
				}
			}
			assert.Equal(t, 1, won)
			assert.Equal(t, 1, lost)

			seenServiceKeys := map[string]bool{}
			for _, service := range template.Services {
				require.False(t, seenServiceKeys[service.Key], "duplicate service key %q", service.Key)
				seenServiceKeys[service.Key] = true
				assert.NotEmpty(t, service.Name)
				assert.Greater(t, service.DurationMinutes, 0)
				assert.Greater(t, service.DefaultCapacity, 0)
			}

			assert.Equal(t, "workspace.v2", template.Manifest["schema"])
			manifest, err := json.Marshal(template.Manifest)
			require.NoError(t, err)
			assert.Contains(t, string(manifest), template.Pipeline.Key)
			for key := range seenServiceKeys {
				assert.Contains(t, string(manifest), key)
			}
		})
	}
}

func TestProductCommercialTemplateCurrency(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "MYR", productCommercialTemplateCurrency(nil))
	assert.Equal(t, "MYR", productCommercialTemplateCurrency(models.JSONB{"currency": "myr"}))
	assert.Equal(t, "USD", productCommercialTemplateCurrency(models.JSONB{"currency": " usd "}))
	assert.Equal(t, "MYR", productCommercialTemplateCurrency(models.JSONB{"currency": "US1"}))
	assert.Equal(t, "MYR", productCommercialTemplateCurrency(models.JSONB{"currency": "RINGGIT"}))
}

func TestProductCommercialProvisionBuiltInTemplateResources(t *testing.T) {
	db := testutil.SetupTestDB(t)
	organization := testutil.CreateTestOrganization(t, db)
	organization.Settings = models.JSONB{"currency": "USD"}
	require.NoError(t, db.Model(&models.Organization{}).
		Where("id = ?", organization.ID).
		Update("settings", organization.Settings).Error)
	user := testutil.CreateTestUser(t, db, organization.ID, testutil.WithSuperAdmin())
	template, ok := productCommercialBuiltInTemplate("wellness")
	require.True(t, ok)

	application, version := createProductCommercialTemplateApplication(
		t,
		db,
		organization.ID,
		user.ID,
		"wellness",
	)

	var summary productTemplateProvisioningSummary
	require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
		var err error
		summary, err = productCommercialProvisionBuiltInTemplateResources(
			tx,
			*organization,
			application,
			version,
			template,
			user.ID,
		)
		return err
	}))
	assert.True(t, summary.PipelineCreated)
	assert.Equal(t, len(template.Pipeline.Stages), summary.StagesCreated)
	assert.Equal(t, len(template.Services), summary.ServicesCreated)

	var pipeline models.CRMPipeline
	require.NoError(t, db.Where("organization_id = ?", organization.ID).
		Preload("Stages").
		First(&pipeline).Error)
	assert.True(t, pipeline.IsDefault)
	assert.Len(t, pipeline.Stages, len(template.Pipeline.Stages))

	var services []models.BookingService
	require.NoError(t, db.Where("organization_id = ?", organization.ID).
		Order("name ASC").
		Find(&services).Error)
	require.Len(t, services, len(template.Services))
	for _, service := range services {
		assert.False(t, service.IsActive, "starter service must require tenant review")
		assert.Zero(t, service.PriceMinor)
		assert.Equal(t, "USD", service.Currency)
		assert.Equal(t, true, service.Metadata["requires_review"])
	}

	var resourceMaps []models.WorkspaceTemplateResourceMap
	require.NoError(t, db.Where(
		"organization_id = ? AND application_id = ?",
		organization.ID,
		application.ID,
	).Find(&resourceMaps).Error)
	assert.Len(t, resourceMaps, 1+len(template.Pipeline.Stages)+len(template.Services))
}

func TestProductCommercialTemplatePreservesExistingDefaultPipeline(t *testing.T) {
	db := testutil.SetupTestDB(t)
	organization := testutil.CreateTestOrganization(t, db)
	user := testutil.CreateTestUser(t, db, organization.ID, testutil.WithSuperAdmin())
	existing := models.CRMPipeline{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: organization.ID,
		Name:           "Customer-owned default",
		IsDefault:      true,
		IsActive:       true,
		Version:        1,
	}
	require.NoError(t, db.Create(&existing).Error)

	template, ok := productCommercialBuiltInTemplate("clinic")
	require.True(t, ok)
	application, version := createProductCommercialTemplateApplication(
		t,
		db,
		organization.ID,
		user.ID,
		"clinic",
	)
	require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
		_, err := productCommercialProvisionBuiltInTemplateResources(
			tx,
			*organization,
			application,
			version,
			template,
			user.ID,
		)
		return err
	}))

	var customerDefault models.CRMPipeline
	require.NoError(t, db.Where("id = ?", existing.ID).First(&customerDefault).Error)
	assert.True(t, customerDefault.IsDefault)

	var templatePipeline models.CRMPipeline
	require.NoError(t, db.Where(
		"organization_id = ? AND id <> ?",
		organization.ID,
		existing.ID,
	).First(&templatePipeline).Error)
	assert.False(t, templatePipeline.IsDefault)
}

func createProductCommercialTemplateApplication(
	t *testing.T,
	db *gorm.DB,
	organizationID, userID uuid.UUID,
	vertical string,
) (models.WorkspaceTemplateApplication, models.WorkspaceTemplateVersion) {
	t.Helper()

	suffix := uuid.NewString()
	template := models.WorkspaceTemplate{
		BaseModel:   models.BaseModel{ID: uuid.New()},
		ScopeKey:    "test-" + suffix,
		Slug:        vertical + "-" + suffix,
		Name:        "Test " + vertical + " template",
		Vertical:    vertical,
		Status:      models.WorkspaceTemplateStatusPublished,
		Tags:        models.StringArray{},
		Settings:    models.JSONB{},
		CreatedByID: &userID,
	}
	require.NoError(t, db.Create(&template).Error)

	version := models.WorkspaceTemplateVersion{
		BaseModel:     models.BaseModel{ID: uuid.New()},
		TemplateID:    template.ID,
		Version:       1,
		Status:        models.WorkspaceTemplateStatusPublished,
		SchemaVersion: "workspace.v2",
		Manifest:      models.JSONB{},
		Checksum:      "test:" + vertical + ":v1:" + suffix,
		CreatedByID:   &userID,
	}
	require.NoError(t, db.Create(&version).Error)

	application := models.WorkspaceTemplateApplication{
		BaseModel:         models.BaseModel{ID: uuid.New()},
		OrganizationID:    organizationID,
		TemplateID:        template.ID,
		TemplateVersionID: version.ID,
		Mode:              "install",
		Status:            models.TemplateApplicationStatusApplying,
		ManifestSnapshot:  models.JSONB{},
		RequestedByID:     &userID,
		RequestedAt:       time.Now().UTC(),
	}
	require.NoError(t, db.Create(&application).Error)

	return application, version
}
