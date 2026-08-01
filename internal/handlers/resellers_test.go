package handlers_test

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/handlers"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
	"github.com/zerodha/fastglue"
)

type resellerUsageTestData struct {
	ResellerID    uuid.UUID `json:"reseller_id"`
	Organizations []struct {
		handlers.OrganizationResponse
		Subscription handlers.ProductSubscriptionResponse `json:"subscription"`
	} `json:"organizations"`
	Page              int   `json:"page"`
	Limit             int   `json:"limit"`
	Total             int64 `json:"total"`
	OrganizationCount int64 `json:"organization_count"`
	UserCount         int64 `json:"user_count"`
	WhatsAppAccounts  int64 `json:"whatsapp_accounts"`
	Contacts          int64 `json:"contacts"`
	Messages          int64 `json:"messages"`
}

func parseResellerUsageTestData(
	t *testing.T,
	request *fastglue.Request,
) resellerUsageTestData {
	t.Helper()
	var response struct {
		Data resellerUsageTestData `json:"data"`
	}
	require.NoError(t, json.Unmarshal(testutil.GetResponseBody(request), &response))
	return response.Data
}

func TestResellerUsageIncludesBulkOrganizationSubscriptions(t *testing.T) {
	app := newTestApp(t)
	baseOrganization := testutil.CreateTestOrganization(t, app.DB)
	platformOwner := testutil.CreateTestUser(
		t,
		app.DB,
		baseOrganization.ID,
		testutil.WithSuperAdmin(),
	)
	reseller := testutil.CreateTestReseller(t, app.DB)
	licensedOrganization := testutil.CreateTestOrganizationForReseller(t, app.DB, reseller.ID)
	unlicensedOrganization := testutil.CreateTestOrganizationForReseller(t, app.DB, reseller.ID)
	require.NoError(t, app.DB.Model(unlicensedOrganization).Updates(map[string]any{
		"name": "A Unlicensed Workspace",
		"slug": "a-unlicensed-" + uuid.NewString()[:8],
	}).Error)
	require.NoError(t, app.DB.Model(licensedOrganization).Updates(map[string]any{
		"name": "B Licensed Workspace",
		"slug": "b-licensed-" + uuid.NewString()[:8],
	}).Error)
	plan := createCatalogPlan(
		t,
		app,
		&reseller.ID,
		"portfolio-growth-"+uuid.NewString()[:8],
		"Portfolio Growth",
		models.CommercialPlanStatusActive,
	)
	billingAccount := models.BillingAccount{
		BaseModel:       models.BaseModel{ID: uuid.New()},
		OrganizationID:  licensedOrganization.ID,
		ResellerID:      &reseller.ID,
		Provider:        models.BillingProviderManual,
		Status:          models.BillingAccountStatusActive,
		DefaultCurrency: "MYR",
		BillingProfile:  models.JSONB{},
		ProviderData:    models.JSONB{},
		Metadata:        models.JSONB{},
	}
	require.NoError(t, app.DB.Create(&billingAccount).Error)
	now := time.Now().UTC()
	periodEnd := now.AddDate(0, 1, 0)
	subscription := models.Subscription{
		BaseModel:            models.BaseModel{ID: uuid.New()},
		OrganizationID:       licensedOrganization.ID,
		BillingAccountID:     billingAccount.ID,
		PlanID:               plan.ID,
		Provider:             models.BillingProviderManual,
		Status:               models.SubscriptionStatusActive,
		Quantity:             1,
		CollectionMethod:     "manual",
		EntitlementsSnapshot: models.JSONB{"crm.enabled": true},
		ProviderData:         models.JSONB{},
		CurrentPeriodStart:   &now,
		CurrentPeriodEnd:     &periodEnd,
		CreatedByID:          &platformOwner.ID,
	}
	require.NoError(t, app.DB.Create(&subscription).Error)

	loadPage := func(page string) resellerUsageTestData {
		t.Helper()
		request := testutil.NewGETRequest(t)
		testutil.SetFullAuthContext(
			request,
			baseOrganization.ID,
			platformOwner.ID,
			platformOwner.RoleID,
			true,
		)
		testutil.SetPathParam(request, "id", reseller.ID.String())
		testutil.SetQueryParam(request, "page", page)
		testutil.SetQueryParam(request, "limit", "1")
		require.NoError(t, app.GetResellerUsage(request))
		require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(request))
		return parseResellerUsageTestData(t, request)
	}

	unlicensedPage := loadPage("1")
	require.Equal(t, 1, unlicensedPage.Page)
	require.Equal(t, 1, unlicensedPage.Limit)
	require.EqualValues(t, 2, unlicensedPage.Total)
	require.EqualValues(t, 2, unlicensedPage.OrganizationCount)
	require.Len(t, unlicensedPage.Organizations, 1)
	assert.Equal(t, unlicensedOrganization.ID, unlicensedPage.Organizations[0].ID)
	assert.Equal(t, "unlicensed", unlicensedPage.Organizations[0].Subscription.Status)

	licensedPage := loadPage("2")
	require.Equal(t, 2, licensedPage.Page)
	require.Equal(t, 1, licensedPage.Limit)
	require.EqualValues(t, 2, licensedPage.Total)
	require.EqualValues(t, 2, licensedPage.OrganizationCount)
	require.Len(t, licensedPage.Organizations, 1)
	assert.Equal(t, licensedOrganization.ID, licensedPage.Organizations[0].ID)
	require.NotNil(t, licensedPage.Organizations[0].Subscription.PlanID)
	assert.Equal(t, plan.ID, *licensedPage.Organizations[0].Subscription.PlanID)
	assert.Equal(t, "Portfolio Growth", licensedPage.Organizations[0].Subscription.PlanName)
}

func TestResellerUsagePaginatesOrganizationsButKeepsPortfolioTotals(t *testing.T) {
	app := newTestApp(t)
	baseOrganization := testutil.CreateTestOrganization(t, app.DB)
	platformOwner := testutil.CreateTestUser(
		t,
		app.DB,
		baseOrganization.ID,
		testutil.WithSuperAdmin(),
	)
	reseller := testutil.CreateTestReseller(t, app.DB)
	organizations := make([]*models.Organization, 0, 51)
	for index := 0; index < 51; index++ {
		organization := testutil.CreateTestOrganizationForReseller(t, app.DB, reseller.ID)
		name := fmt.Sprintf("Customer Workspace %03d", index)
		slug := fmt.Sprintf("customer-workspace-%03d-%s", index, uuid.NewString()[:8])
		require.NoError(t, app.DB.Model(organization).Updates(map[string]any{
			"name": name,
			"slug": slug,
		}).Error)
		organization.Name = name
		organization.Slug = slug
		organizations = append(organizations, organization)
	}

	testutil.CreateTestWhatsAppAccount(t, app.DB, organizations[0].ID)
	firstContact := testutil.CreateTestContact(t, app.DB, organizations[0].ID)
	for index := 0; index < 2; index++ {
		createTestMessage(
			t,
			app,
			organizations[0].ID,
			firstContact.ID,
			models.DirectionIncoming,
			time.Now().UTC(),
		)
	}
	testutil.CreateTestUser(t, app.DB, organizations[0].ID)

	for index := 0; index < 2; index++ {
		testutil.CreateTestWhatsAppAccount(t, app.DB, organizations[50].ID)
	}
	lastContacts := []*models.Contact{
		testutil.CreateTestContact(t, app.DB, organizations[50].ID),
		testutil.CreateTestContact(t, app.DB, organizations[50].ID),
	}
	for index := 0; index < 3; index++ {
		createTestMessage(
			t,
			app,
			organizations[50].ID,
			lastContacts[index%len(lastContacts)].ID,
			models.DirectionIncoming,
			time.Now().UTC(),
		)
	}
	testutil.CreateTestUser(t, app.DB, organizations[50].ID)

	// A neighboring reseller has data too; none of it may enter this portfolio's
	// organization list or aggregate counters.
	otherReseller := testutil.CreateTestReseller(t, app.DB)
	otherOrganization := testutil.CreateTestOrganizationForReseller(
		t,
		app.DB,
		otherReseller.ID,
	)
	testutil.CreateTestWhatsAppAccount(t, app.DB, otherOrganization.ID)
	otherContact := testutil.CreateTestContact(t, app.DB, otherOrganization.ID)
	createTestMessage(
		t,
		app,
		otherOrganization.ID,
		otherContact.ID,
		models.DirectionIncoming,
		time.Now().UTC(),
	)
	testutil.CreateTestUser(t, app.DB, otherOrganization.ID)

	loadUsage := func(query map[string]string) resellerUsageTestData {
		t.Helper()
		request := testutil.NewGETRequest(t)
		testutil.SetFullAuthContext(
			request,
			baseOrganization.ID,
			platformOwner.ID,
			platformOwner.RoleID,
			true,
		)
		testutil.SetPathParam(request, "id", reseller.ID.String())
		for key, value := range query {
			testutil.SetQueryParam(request, key, value)
		}
		require.NoError(t, app.GetResellerUsage(request))
		require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(request))
		return parseResellerUsageTestData(t, request)
	}
	assertPortfolioTotals := func(usage resellerUsageTestData) {
		t.Helper()
		assert.Equal(t, reseller.ID, usage.ResellerID)
		assert.EqualValues(t, 51, usage.Total)
		assert.EqualValues(t, 51, usage.OrganizationCount)
		assert.EqualValues(t, 2, usage.UserCount)
		assert.EqualValues(t, 3, usage.WhatsAppAccounts)
		assert.EqualValues(t, 3, usage.Contacts)
		assert.EqualValues(t, 5, usage.Messages)
	}

	defaultPage := loadUsage(nil)
	assertPortfolioTotals(defaultPage)
	assert.Equal(t, 1, defaultPage.Page)
	assert.Equal(t, 50, defaultPage.Limit)
	require.Len(t, defaultPage.Organizations, 50)
	assert.Equal(t, organizations[0].ID, defaultPage.Organizations[0].ID)
	assert.Equal(t, organizations[49].ID, defaultPage.Organizations[49].ID)

	secondPage := loadUsage(map[string]string{"page": "2", "limit": "50"})
	assertPortfolioTotals(secondPage)
	assert.Equal(t, 2, secondPage.Page)
	assert.Equal(t, 50, secondPage.Limit)
	require.Len(t, secondPage.Organizations, 1)
	assert.Equal(t, organizations[50].ID, secondPage.Organizations[0].ID)

	maximumLimitEmptyPage := loadUsage(map[string]string{"page": "99", "limit": "100"})
	assertPortfolioTotals(maximumLimitEmptyPage)
	assert.Equal(t, 99, maximumLimitEmptyPage.Page)
	assert.Equal(t, 100, maximumLimitEmptyPage.Limit)
	assert.Empty(t, maximumLimitEmptyPage.Organizations)

	invalidBounds := loadUsage(map[string]string{"page": "0", "limit": "101"})
	assertPortfolioTotals(invalidBounds)
	assert.Equal(t, 1, invalidBounds.Page)
	assert.Equal(t, 50, invalidBounds.Limit)
	require.Len(t, invalidBounds.Organizations, 50)
}

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

	otherUsageRequest := testutil.NewGETRequest(t)
	testutil.SetAuthContext(otherUsageRequest, orgA.ID, userA.ID)
	testutil.SetPathParam(otherUsageRequest, "id", resellerB.ID.String())
	require.NoError(t, app.GetResellerUsage(otherUsageRequest))
	assert.Equal(t, fasthttp.StatusForbidden, testutil.GetResponseStatusCode(otherUsageRequest))

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
