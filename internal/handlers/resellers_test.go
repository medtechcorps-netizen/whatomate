package handlers_test

import (
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/handlers"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
)

func createResellerAdminRole(t *testing.T, app *handlers.App, orgID uuid.UUID) *models.CustomRole {
	t.Helper()
	return testutil.CreateTestRoleExact(t, app.DB, orgID, "admin", true, false, testutil.GetOrCreateTestPermissions(t, app.DB))
}

func TestResellerPortfolioIsolationAndRevocation(t *testing.T) {
	app := newTestApp(t)
	resellerA := testutil.CreateTestReseller(t, app.DB)
	resellerB := testutil.CreateTestReseller(t, app.DB)
	orgA := testutil.CreateTestOrganizationForReseller(t, app.DB, resellerA.ID)
	orgA2 := testutil.CreateTestOrganizationForReseller(t, app.DB, resellerA.ID)
	orgB := testutil.CreateTestOrganizationForReseller(t, app.DB, resellerB.ID)
	roleA := createResellerAdminRole(t, app, orgA.ID)
	roleA2 := createResellerAdminRole(t, app, orgA2.ID)
	createResellerAdminRole(t, app, orgB.ID)

	userA := testutil.CreateTestUser(
		t,
		app.DB,
		orgA.ID,
		testutil.WithEmail(testutil.UniqueEmail("partner-a")),
		testutil.WithRoleID(&roleA.ID),
	)

	addRequest := testutil.NewJSONRequest(t, map[string]any{
		"email": userA.Email,
		"role":  models.ResellerRoleAdmin,
	})
	testutil.SetAuthContext(addRequest, orgA.ID, userA.ID)
	testutil.SetPathParam(addRequest, "id", resellerA.ID.String())
	// Bootstrap the first assignment as a platform owner.
	require.NoError(t, app.DB.Model(&userA).Update("is_super_admin", true).Error)
	require.NoError(t, app.AddResellerMember(addRequest))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(addRequest))
	require.NoError(t, app.DB.Model(&userA).Update("is_super_admin", false).Error)
	app.InvalidateUserPermissionsCache(userA.ID)

	var resellerMember models.ResellerMember
	require.NoError(t, app.DB.Where(
		"reseller_id = ? AND user_id = ?", resellerA.ID, userA.ID,
	).First(&resellerMember).Error)
	assert.False(t, userA.IsSuperAdmin, "reseller access must not grant the global super-admin flag")

	var inherited models.UserOrganization
	require.NoError(t, app.DB.Where(
		"user_id = ? AND organization_id = ?", userA.ID, orgA2.ID,
	).First(&inherited).Error)
	assert.Equal(t, models.MembershipSourceReseller, inherited.Source)
	require.NotNil(t, inherited.ResellerMemberID)
	assert.Equal(t, resellerMember.ID, *inherited.ResellerMemberID)
	assert.Equal(t, roleA2.ID, *inherited.RoleID)

	listResellersRequest := testutil.NewGETRequest(t)
	testutil.SetAuthContext(listResellersRequest, orgA.ID, userA.ID)
	require.NoError(t, app.ListResellers(listResellersRequest))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(listResellersRequest))
	var resellerList struct {
		Data struct {
			Resellers []models.Reseller `json:"resellers"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(testutil.GetResponseBody(listResellersRequest), &resellerList))
	require.Len(t, resellerList.Data.Resellers, 1)
	assert.Equal(t, resellerA.ID, resellerList.Data.Resellers[0].ID)

	getOtherRequest := testutil.NewGETRequest(t)
	testutil.SetAuthContext(getOtherRequest, orgA.ID, userA.ID)
	testutil.SetPathParam(getOtherRequest, "id", resellerB.ID.String())
	require.NoError(t, app.GetReseller(getOtherRequest))
	assert.Equal(t, fasthttp.StatusForbidden, testutil.GetResponseStatusCode(getOtherRequest))

	listOrganizationsRequest := testutil.NewGETRequest(t)
	testutil.SetAuthContext(listOrganizationsRequest, orgA.ID, userA.ID)
	require.NoError(t, app.ListOrganizations(listOrganizationsRequest))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(listOrganizationsRequest))
	var organizationList struct {
		Data struct {
			Organizations []handlers.OrganizationResponse `json:"organizations"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(testutil.GetResponseBody(listOrganizationsRequest), &organizationList))
	organizationIDs := make(map[uuid.UUID]bool)
	for _, organization := range organizationList.Data.Organizations {
		organizationIDs[organization.ID] = true
	}
	assert.True(t, organizationIDs[orgA.ID])
	assert.True(t, organizationIDs[orgA2.ID])
	assert.False(t, organizationIDs[orgB.ID], "another reseller's organization must never be listed")

	platformOwner := testutil.CreateTestUser(
		t,
		app.DB,
		orgA.ID,
		testutil.WithEmail(testutil.UniqueEmail("revoke-platform-owner")),
		testutil.WithSuperAdmin(),
	)
	removeRequest := testutil.NewJSONRequest(t, nil)
	testutil.SetFullAuthContext(removeRequest, orgA.ID, platformOwner.ID, platformOwner.RoleID, true)
	testutil.SetPathParam(removeRequest, "id", resellerA.ID.String())
	testutil.SetPathParam(removeRequest, "member_id", resellerMember.ID.String())
	require.NoError(t, app.RemoveResellerMember(removeRequest))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(removeRequest))

	var derivedCount, directCount int64
	require.NoError(t, app.DB.Model(&models.UserOrganization{}).
		Where("user_id = ? AND source = ?", userA.ID, models.MembershipSourceReseller).
		Count(&derivedCount).Error)
	require.NoError(t, app.DB.Model(&models.UserOrganization{}).
		Where("user_id = ? AND organization_id = ? AND source = ?", userA.ID, orgA.ID, models.MembershipSourceDirect).
		Count(&directCount).Error)
	assert.Zero(t, derivedCount, "revocation must delete every reseller-derived membership")
	assert.EqualValues(t, 1, directCount, "direct organization access must be preserved")
}

func TestCreateOrganizationSynchronizesActiveResellerAdministrators(t *testing.T) {
	app := newTestApp(t)
	reseller := testutil.CreateTestReseller(t, app.DB)
	baseOrg := testutil.CreateTestOrganization(t, app.DB)
	platformOwner := testutil.CreateTestUser(
		t,
		app.DB,
		baseOrg.ID,
		testutil.WithEmail(testutil.UniqueEmail("platform-owner")),
		testutil.WithSuperAdmin(),
	)
	adminUser := testutil.CreateTestUser(
		t,
		app.DB,
		baseOrg.ID,
		testutil.WithEmail(testutil.UniqueEmail("future-admin")),
	)
	member := models.ResellerMember{
		BaseModel:  models.BaseModel{ID: uuid.New()},
		ResellerID: reseller.ID,
		UserID:     adminUser.ID,
		Role:       models.ResellerRoleAdmin,
		IsActive:   true,
	}
	require.NoError(t, app.DB.Create(&member).Error)

	request := testutil.NewJSONRequest(t, map[string]any{
		"name":        "Future Customer Clinic",
		"reseller_id": reseller.ID,
	})
	testutil.SetFullAuthContext(request, baseOrg.ID, platformOwner.ID, platformOwner.RoleID, true)
	require.NoError(t, app.CreateOrganization(request))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(request))

	var response struct {
		Data handlers.OrganizationResponse `json:"data"`
	}
	require.NoError(t, json.Unmarshal(testutil.GetResponseBody(request), &response))
	require.NotNil(t, response.Data.ResellerID)
	assert.Equal(t, reseller.ID, *response.Data.ResellerID)

	var membership models.UserOrganization
	require.NoError(t, app.DB.Where(
		"user_id = ? AND organization_id = ?", adminUser.ID, response.Data.ID,
	).First(&membership).Error)
	assert.Equal(t, models.MembershipSourceReseller, membership.Source)
	require.NotNil(t, membership.ResellerMemberID)
	assert.Equal(t, member.ID, *membership.ResellerMemberID)
}

func TestCreateResellerDoesNotCreateAnotherGlobalSuperAdmin(t *testing.T) {
	app := newTestApp(t)
	baseOrg := testutil.CreateTestOrganization(t, app.DB)
	platformOwner := testutil.CreateTestUser(
		t,
		app.DB,
		baseOrg.ID,
		testutil.WithEmail(testutil.UniqueEmail("create-partner")),
		testutil.WithSuperAdmin(),
	)
	var superAdminsBefore int64
	require.NoError(t, app.DB.Model(&models.User{}).
		Where("is_super_admin = ?", true).
		Count(&superAdminsBefore).Error)

	request := testutil.NewJSONRequest(t, map[string]any{
		"name":           "Independent Software Partner",
		"brand_name":     "Independent CRM",
		"workspace_name": "Independent HQ",
		"plan":           models.ResellerPlanGrowth,
	})
	testutil.SetFullAuthContext(request, baseOrg.ID, platformOwner.ID, platformOwner.RoleID, true)
	require.NoError(t, app.CreateReseller(request))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(request))

	var response struct {
		Data struct {
			Reseller struct {
				models.Reseller
			} `json:"reseller"`
			Workspace handlers.OrganizationResponse `json:"workspace"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(testutil.GetResponseBody(request), &response))
	assert.Equal(t, "Independent CRM", response.Data.Reseller.BrandName)
	require.NotNil(t, response.Data.Workspace.ResellerID)
	assert.Equal(t, response.Data.Reseller.ID, *response.Data.Workspace.ResellerID)

	var superAdminsAfter int64
	require.NoError(t, app.DB.Model(&models.User{}).
		Where("is_super_admin = ?", true).
		Count(&superAdminsAfter).Error)
	assert.Equal(t, superAdminsBefore, superAdminsAfter)
}
