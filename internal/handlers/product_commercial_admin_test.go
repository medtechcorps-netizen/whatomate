package handlers_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/handlers"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
)

func createCatalogPlan(
	t *testing.T,
	app *handlers.App,
	resellerID *uuid.UUID,
	code string,
	name string,
	status models.CommercialPlanStatus,
) models.Plan {
	t.Helper()
	scopeKey := "platform"
	if resellerID != nil {
		scopeKey = resellerID.String()
	}
	plan := models.Plan{
		BaseModel:    models.BaseModel{ID: uuid.New()},
		ResellerID:   resellerID,
		ScopeKey:     scopeKey,
		Code:         code,
		Name:         name,
		Status:       status,
		Vertical:     "general",
		TrialDays:    14,
		IsPublic:     false,
		Metadata:     models.JSONB{},
		DisplayOrder: 1,
	}
	require.NoError(t, app.DB.Create(&plan).Error)
	return plan
}

func createCatalogPrice(
	t *testing.T,
	app *handlers.App,
	planID uuid.UUID,
	provider models.BillingProvider,
	interval models.BillingInterval,
	active bool,
	effectiveFrom *time.Time,
) models.PlanPrice {
	t.Helper()
	price := models.PlanPrice{
		BaseModel:       models.BaseModel{ID: uuid.New()},
		PlanID:          planID,
		Code:            "shared-price",
		Provider:        provider,
		Currency:        "MYR",
		UnitAmountMinor: 29900,
		Interval:        interval,
		IntervalCount:   1,
		TaxBehavior:     "exclusive",
		IsActive:        active,
		EffectiveFrom:   effectiveFrom,
		ProviderData:    models.JSONB{},
		Metadata:        models.JSONB{},
	}
	require.NoError(t, app.DB.Create(&price).Error)
	return price
}

func TestListAssignableProductPlansIsOwnerOnlyAndTargetScoped(t *testing.T) {
	app := newTestApp(t)
	controlOrg := testutil.CreateTestOrganization(t, app.DB)
	owner := testutil.CreateTestUser(
		t,
		app.DB,
		controlOrg.ID,
		testutil.WithEmail(testutil.UniqueEmail("catalog-owner")),
		testutil.WithSuperAdmin(),
	)
	reseller := testutil.CreateTestReseller(t, app.DB)
	otherReseller := testutil.CreateTestReseller(t, app.DB)
	targetOrg := testutil.CreateTestOrganizationForReseller(t, app.DB, reseller.ID)

	globalPlan := createCatalogPlan(
		t, app, nil, "shared-growth", "Global private", models.CommercialPlanStatusActive,
	)
	globalPrice := createCatalogPrice(
		t, app, globalPlan.ID, models.BillingProviderManual, models.BillingIntervalMonth, true, nil,
	)
	resellerPlan := createCatalogPlan(
		t, app, &reseller.ID, "shared-growth", "Reseller private", models.CommercialPlanStatusActive,
	)
	resellerPrice := createCatalogPrice(
		t, app, resellerPlan.ID, models.BillingProviderManual, models.BillingIntervalYear, true, nil,
	)

	foreignPlan := createCatalogPlan(
		t, app, &otherReseller.ID, "foreign", "Foreign reseller", models.CommercialPlanStatusActive,
	)
	createCatalogPrice(
		t, app, foreignPlan.ID, models.BillingProviderManual, models.BillingIntervalMonth, true, nil,
	)
	draftPlan := createCatalogPlan(
		t, app, &reseller.ID, "draft", "Draft", models.CommercialPlanStatusDraft,
	)
	createCatalogPrice(
		t, app, draftPlan.ID, models.BillingProviderManual, models.BillingIntervalMonth, true, nil,
	)
	oneTimePlan := createCatalogPlan(
		t, app, &reseller.ID, "one-time", "One time", models.CommercialPlanStatusActive,
	)
	createCatalogPrice(
		t, app, oneTimePlan.ID, models.BillingProviderManual, models.BillingIntervalOneTime, true, nil,
	)
	providerPlan := createCatalogPlan(
		t, app, &reseller.ID, "provider", "Provider managed", models.CommercialPlanStatusActive,
	)
	createCatalogPrice(
		t, app, providerPlan.ID, models.BillingProviderStripe, models.BillingIntervalMonth, true, nil,
	)
	future := time.Now().UTC().Add(time.Hour)
	futurePlan := createCatalogPlan(
		t, app, &reseller.ID, "future", "Future", models.CommercialPlanStatusActive,
	)
	createCatalogPrice(
		t, app, futurePlan.ID, models.BillingProviderManual, models.BillingIntervalMonth, true, &future,
	)

	request := testutil.NewGETRequest(t)
	testutil.SetFullAuthContext(request, controlOrg.ID, owner.ID, owner.RoleID, true)
	testutil.SetPathParam(request, "organization_id", targetOrg.ID.String())
	require.NoError(t, app.ListAssignableProductPlans(request))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(request))

	var response struct {
		Data struct {
			Plans []handlers.ProductPlanResponse `json:"plans"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(testutil.GetResponseBody(request), &response))
	require.Len(t, response.Data.Plans, 2)
	planIDs := make(map[uuid.UUID]bool, len(response.Data.Plans))
	priceIDs := make(map[uuid.UUID]bool, len(response.Data.Plans))
	for _, plan := range response.Data.Plans {
		require.NotNil(t, plan.ID)
		require.Len(t, plan.Prices, 1)
		require.NotNil(t, plan.Prices[0].ID)
		assert.False(t, plan.IsPublic)
		planIDs[*plan.ID] = true
		priceIDs[*plan.Prices[0].ID] = true
	}
	assert.Equal(t, map[uuid.UUID]bool{
		globalPlan.ID:   true,
		resellerPlan.ID: true,
	}, planIDs)
	assert.Equal(t, map[uuid.UUID]bool{
		globalPrice.ID:   true,
		resellerPrice.ID: true,
	}, priceIDs)

	resellerAdmin := testutil.CreateTestUser(
		t,
		app.DB,
		targetOrg.ID,
		testutil.WithEmail(testutil.UniqueEmail("catalog-reseller-admin")),
	)
	require.NoError(t, app.DB.Create(&models.ResellerMember{
		BaseModel:  models.BaseModel{ID: uuid.New()},
		ResellerID: reseller.ID,
		UserID:     resellerAdmin.ID,
		Role:       models.ResellerRoleAdmin,
		IsActive:   true,
	}).Error)
	forbidden := testutil.NewGETRequest(t)
	testutil.SetAuthContext(forbidden, targetOrg.ID, resellerAdmin.ID)
	testutil.SetPathParam(forbidden, "organization_id", targetOrg.ID.String())
	require.NoError(t, app.ListAssignableProductPlans(forbidden))
	assert.Equal(t, fasthttp.StatusForbidden, testutil.GetResponseStatusCode(forbidden))
}

func TestSetOrganizationSubscriptionIsOwnerOnlyAndRequiresReference(t *testing.T) {
	app := newTestApp(t)
	controlOrg := testutil.CreateTestOrganization(t, app.DB)
	owner := testutil.CreateTestUser(
		t,
		app.DB,
		controlOrg.ID,
		testutil.WithEmail(testutil.UniqueEmail("license-owner")),
		testutil.WithSuperAdmin(),
	)
	reseller := testutil.CreateTestReseller(t, app.DB)
	targetOrg := testutil.CreateTestOrganizationForReseller(t, app.DB, reseller.ID)
	resellerAdmin := testutil.CreateTestUser(
		t,
		app.DB,
		targetOrg.ID,
		testutil.WithEmail(testutil.UniqueEmail("license-reseller-admin")),
	)
	require.NoError(t, app.DB.Create(&models.ResellerMember{
		BaseModel:  models.BaseModel{ID: uuid.New()},
		ResellerID: reseller.ID,
		UserID:     resellerAdmin.ID,
		Role:       models.ResellerRoleAdmin,
		IsActive:   true,
	}).Error)

	planID := uuid.New()
	priceID := uuid.New()
	payload := map[string]any{
		"plan_id":          planID,
		"plan_price_id":    priceID,
		"status":           models.SubscriptionStatusActive,
		"manual_reference": "contract-2026-001",
	}

	forbidden := testutil.NewJSONRequest(t, payload)
	testutil.SetAuthContext(forbidden, targetOrg.ID, resellerAdmin.ID)
	testutil.SetPathParam(forbidden, "organization_id", targetOrg.ID.String())
	require.NoError(t, app.SetOrganizationSubscription(forbidden))
	testutil.AssertErrorResponse(
		t,
		forbidden,
		fasthttp.StatusForbidden,
		"Platform owner access required",
	)

	delete(payload, "manual_reference")
	missingReference := testutil.NewJSONRequest(t, payload)
	testutil.SetFullAuthContext(
		missingReference,
		controlOrg.ID,
		owner.ID,
		owner.RoleID,
		true,
	)
	testutil.SetPathParam(missingReference, "organization_id", targetOrg.ID.String())
	require.NoError(t, app.SetOrganizationSubscription(missingReference))
	testutil.AssertErrorResponse(
		t,
		missingReference,
		fasthttp.StatusBadRequest,
		"manual_reference is required",
	)
}

func TestSetOrganizationSubscriptionAuditsSanitizedManualReference(t *testing.T) {
	app := newTestApp(t)
	controlOrg := testutil.CreateTestOrganization(t, app.DB)
	owner := testutil.CreateTestUser(
		t,
		app.DB,
		controlOrg.ID,
		testutil.WithEmail(testutil.UniqueEmail("license-audit-owner")),
		testutil.WithSuperAdmin(),
	)
	reseller := testutil.CreateTestReseller(t, app.DB)
	targetOrg := testutil.CreateTestOrganizationForReseller(t, app.DB, reseller.ID)
	plan := createCatalogPlan(
		t,
		app,
		&reseller.ID,
		"audit-growth",
		"Audit Growth",
		models.CommercialPlanStatusActive,
	)
	price := createCatalogPrice(
		t,
		app,
		plan.ID,
		models.BillingProviderManual,
		models.BillingIntervalMonth,
		true,
		nil,
	)

	assign := func(reference string) {
		t.Helper()
		request := testutil.NewJSONRequest(t, map[string]any{
			"plan_id":          plan.ID,
			"plan_price_id":    price.ID,
			"status":           models.SubscriptionStatusActive,
			"manual_reference": reference,
		})
		testutil.SetFullAuthContext(
			request,
			controlOrg.ID,
			owner.ID,
			owner.RoleID,
			true,
		)
		testutil.SetPathParam(request, "organization_id", targetOrg.ID.String())
		require.NoError(t, app.SetOrganizationSubscription(request))
		require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(request))
	}

	assign("  contract-2026-001  ")
	assign("contract-2026-002")

	var subscription models.Subscription
	require.NoError(t, app.DB.Where("organization_id = ?", targetOrg.ID).
		First(&subscription).Error)
	assert.Equal(t, "contract-2026-002", subscription.ProviderData["manual_reference"])

	var auditLog models.AuditLog
	require.NoError(t, app.DB.Where(
		"organization_id = ? AND resource_type = ? AND resource_id = ? AND action = ?",
		targetOrg.ID,
		"commercial_subscription",
		subscription.ID,
		models.AuditActionUpdated,
	).First(&auditLog).Error)

	var referenceChange map[string]any
	for _, rawChange := range auditLog.Changes {
		change, ok := rawChange.(map[string]any)
		if ok && change["field"] == "manual_reference" {
			referenceChange = change
			break
		}
	}
	require.NotNil(t, referenceChange)
	assert.Equal(t, "contract-2026-001", referenceChange["old_value"])
	assert.Equal(t, "contract-2026-002", referenceChange["new_value"])
	for _, rawChange := range auditLog.Changes {
		change, ok := rawChange.(map[string]any)
		if ok {
			assert.NotEqual(t, "provider_data", change["field"])
		}
	}
}
