package handlers

import (
	"testing"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/config"
	"github.com/shridarpatil/whatomate/internal/database"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
	"github.com/zerodha/fastglue"
	"gorm.io/gorm"
)

func tenantAdmissionTestApp(dbConfig config.DatabaseConfig, t *testing.T) *App {
	t.Helper()
	return &App{
		DB:     testutil.SetupTestDB(t),
		Log:    testutil.NopLogger(),
		Config: &config.Config{Database: dbConfig},
	}
}

func TestTenantAndBackgroundAdmissionRejectPlatformComplianceOrganization(t *testing.T) {
	base := tenantAdmissionTestApp(config.DatabaseConfig{}, t)
	ordinary := testutil.CreateTestOrganization(t, base.DB)
	_, purpose := createMetaInstagramPlatformComplianceOrganization(t, base.DB)

	for _, rlsEnabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "rls_disabled", true: "rls_enabled"}[rlsEnabled], func(t *testing.T) {
			app := &App{
				DB: base.DB, Log: testutil.NopLogger(),
				Config: &config.Config{Database: config.DatabaseConfig{RLSEnabled: rlsEnabled}},
			}

			t.Run("request", func(t *testing.T) {
				called := false
				request := testutil.NewGETRequest(t)
				testutil.SetAuthContext(request, purpose.ID, uuid.New())
				handler := app.Tenant(func(*App, *fastglue.Request) error {
					called = true
					return nil
				})
				require.NoError(t, handler(request))
				assert.False(t, called)
				assert.Equal(t, fasthttp.StatusUnauthorized, testutil.GetResponseStatusCode(request))
			})

			for _, entry := range []struct {
				name string
				run  func(uuid.UUID, func(*App) error) error
			}{
				{name: "background", run: app.WithTenantApp},
				{name: "committed_background", run: app.WithCommittedTenantApp},
			} {
				t.Run(entry.name, func(t *testing.T) {
					ordinaryCalled := false
					require.NoError(t, entry.run(ordinary.ID, func(*App) error {
						ordinaryCalled = true
						return nil
					}))
					assert.True(t, ordinaryCalled)

					purposeCalled := false
					err := entry.run(purpose.ID, func(*App) error {
						purposeCalled = true
						return nil
					})
					require.ErrorIs(t, err, database.ErrPlatformComplianceTenant)
					assert.False(t, purposeCalled)
				})
			}
		})
	}
}

func TestPlatformComplianceInstagramAdmissionIsExplicitAndFeatureBound(t *testing.T) {
	base := tenantAdmissionTestApp(config.DatabaseConfig{}, t)
	ordinary := testutil.CreateTestOrganization(t, base.DB)
	platformReseller, instagram := createMetaInstagramPlatformComplianceOrganization(t, base.DB)
	threadsID := uuid.New()
	require.NoError(t, base.DB.Transaction(func(tx *gorm.DB) error {
		return database.CreatePlatformComplianceOrganization(
			tx,
			threadsID,
			"handlers-threads-feature-"+uuid.NewString(),
			false,
			true,
		)
	}))

	for _, rlsEnabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "rls_disabled", true: "rls_enabled"}[rlsEnabled], func(t *testing.T) {
			app := &App{
				DB: base.DB, Log: testutil.NopLogger(),
				Config: &config.Config{Database: config.DatabaseConfig{RLSEnabled: rlsEnabled}},
			}
			ordinaryCalled := false
			err := app.withPlatformComplianceInstagramTenantApp(ordinary.ID, func(*App) error {
				ordinaryCalled = true
				return nil
			})
			require.ErrorIs(t, err, errPlatformComplianceInstagramTenantRequired)
			assert.False(t, ordinaryCalled)

			wrongFeatureCalled := false
			err = app.withPlatformComplianceInstagramTenantApp(threadsID, func(*App) error {
				wrongFeatureCalled = true
				return nil
			})
			require.ErrorIs(t, err, errPlatformComplianceInstagramTenantRequired)
			assert.False(t, wrongFeatureCalled)

			instagramCalled := false
			require.NoError(t, app.withPlatformComplianceInstagramTenantApp(
				instagram.ID,
				func(scoped *App) error {
					instagramCalled = true
					assert.Equal(t, instagram.ID, scoped.tenantOrgID)
					return nil
				},
			))
			assert.True(t, instagramCalled)

			t.Run("inactive_platform_owner", func(t *testing.T) {
				require.NoError(t, base.DB.Model(platformReseller).
					Update("status", models.ResellerStatusSuspended).Error)
				t.Cleanup(func() {
					require.NoError(t, base.DB.Model(platformReseller).
						Update("status", models.ResellerStatusActive).Error)
				})
				inactiveCalled := false
				err := app.withPlatformComplianceInstagramTenantApp(instagram.ID, func(*App) error {
					inactiveCalled = true
					return nil
				})
				require.ErrorIs(t, err, errPlatformComplianceInstagramTenantRequired)
				assert.False(t, inactiveCalled)
			})
		})
	}
}
