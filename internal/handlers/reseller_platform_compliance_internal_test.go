package handlers

import (
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/config"
	"github.com/shridarpatil/whatomate/internal/database"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
)

func TestResellerPortfolioExcludesPlatformComplianceOrganizations(t *testing.T) {
	for _, rlsEnabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "rls_disabled", true: "rls_enabled"}[rlsEnabled], func(t *testing.T) {
			db := testutil.SetupTestDB(t)
			app := &App{
				DB: db, Log: testutil.NopLogger(),
				Config: &config.Config{Database: config.DatabaseConfig{RLSEnabled: rlsEnabled}},
			}

			platformReseller, purpose := createMetaInstagramPlatformComplianceOrganization(t, db)
			ordinary := testutil.CreateTestOrganizationForReseller(t, db, platformReseller.ID)
			testutil.CreateTestRoleExact(t, db, ordinary.ID, "admin", true, false, nil)
			malformed := testutil.CreateTestOrganizationForReseller(t, db, platformReseller.ID)
			updateMetaInstagramComplianceOrganizationFixture(t, db, malformed.ID, map[string]any{
				"settings": models.JSONB{database.PlatformComplianceThreadsMarkerKey: "true"},
			})
			_, err := database.IsPlatformComplianceOrganization(db, malformed.ID)
			require.ErrorIs(t, err, database.ErrPlatformComplianceMarkerInvalid)

			baseOrganization := testutil.CreateTestOrganization(t, db)
			memberUser := testutil.CreateTestUser(
				t, db, baseOrganization.ID,
				testutil.WithEmail(testutil.UniqueEmail("platform-portfolio-member")),
				testutil.WithSuperAdmin(),
			)
			addRequest := testutil.NewJSONRequest(t, map[string]any{
				"email": memberUser.Email,
				"role":  models.ResellerRoleAdmin,
			})
			testutil.SetAuthContext(addRequest, baseOrganization.ID, memberUser.ID)
			testutil.SetPathParam(addRequest, "id", platformReseller.ID.String())
			require.NoError(t, app.AddResellerMember(addRequest))
			require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(addRequest))

			var member models.ResellerMember
			require.NoError(t, db.Where(
				"reseller_id = ? AND user_id = ?", platformReseller.ID, memberUser.ID,
			).First(&member).Error)
			var ordinaryMembership models.UserOrganization
			require.NoError(t, db.Where(
				"user_id = ? AND organization_id = ?", memberUser.ID, ordinary.ID,
			).First(&ordinaryMembership).Error)
			assert.Equal(t, models.MembershipSourceReseller, ordinaryMembership.Source)
			assert.Equal(t, &member.ID, ordinaryMembership.ResellerMemberID)
			for _, excludedID := range []uuid.UUID{purpose.ID, malformed.ID} {
				var membershipCount int64
				require.NoError(t, db.Model(&models.UserOrganization{}).
					Where("user_id = ? AND organization_id = ?", memberUser.ID, excludedID).
					Count(&membershipCount).Error)
				assert.Zero(t, membershipCount)
			}

			require.NoError(t, db.Model(memberUser).Update("is_super_admin", false).Error)
			app.InvalidateUserPermissionsCache(memberUser.ID)

			summary := app.resellerSummary(*platformReseller)
			assert.EqualValues(t, 1, summary.OrganizationCount)

			listRequest := testutil.NewGETRequest(t)
			testutil.SetAuthContext(listRequest, baseOrganization.ID, memberUser.ID)
			require.NoError(t, app.WithTenantApp(baseOrganization.ID, func(scoped *App) error {
				return scoped.ListOrganizations(listRequest)
			}))
			require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(listRequest))
			var listed struct {
				Data struct {
					Organizations []OrganizationResponse `json:"organizations"`
				} `json:"data"`
			}
			require.NoError(t, json.Unmarshal(testutil.GetResponseBody(listRequest), &listed))
			listedIDs := make(map[uuid.UUID]bool, len(listed.Data.Organizations))
			for _, organization := range listed.Data.Organizations {
				listedIDs[organization.ID] = true
			}
			assert.True(t, listedIDs[ordinary.ID])
			assert.False(t, listedIDs[purpose.ID])
			assert.False(t, listedIDs[malformed.ID])

			usageRequest := testutil.NewGETRequest(t)
			testutil.SetAuthContext(usageRequest, baseOrganization.ID, memberUser.ID)
			testutil.SetPathParam(usageRequest, "id", platformReseller.ID.String())
			require.NoError(t, app.GetResellerUsage(usageRequest))
			require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(usageRequest))
			var usage struct {
				Data resellerUsageResponse `json:"data"`
			}
			require.NoError(t, json.Unmarshal(testutil.GetResponseBody(usageRequest), &usage))
			assert.EqualValues(t, 1, usage.Data.OrganizationCount)
			assert.EqualValues(t, 1, usage.Data.Total)
			require.Len(t, usage.Data.Organizations, 1)
			assert.Equal(t, ordinary.ID, usage.Data.Organizations[0].ID)

			require.NoError(t, db.Model(memberUser).Update("is_super_admin", true).Error)
			app.InvalidateUserPermissionsCache(memberUser.ID)
			deleteRequest := testutil.NewJSONRequest(t, nil)
			testutil.SetAuthContext(deleteRequest, baseOrganization.ID, memberUser.ID)
			testutil.SetPathParam(deleteRequest, "id", ordinary.ID.String())
			require.NoError(t, app.DeleteOrganization(deleteRequest))
			require.Equal(t, fasthttp.StatusConflict, testutil.GetResponseStatusCode(deleteRequest))
			var retained models.Organization
			require.NoError(t, db.Where("id = ?", ordinary.ID).First(&retained).Error)

			require.NoError(t, db.Model(platformReseller).Update("max_organizations", 2).Error)
			platformReseller.MaxOrganizations = 2
			createRequest := testutil.NewJSONRequest(t, map[string]any{
				"name":        "Second Ordinary Platform Workspace",
				"reseller_id": platformReseller.ID,
			})
			testutil.SetAuthContext(createRequest, baseOrganization.ID, memberUser.ID)
			require.NoError(t, app.CreateOrganization(createRequest))
			require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(createRequest))
			assert.EqualValues(t, 2, app.resellerSummary(*platformReseller).OrganizationCount)
		})
	}
}
