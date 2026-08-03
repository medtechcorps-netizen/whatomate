package main

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/config"
	"github.com/shridarpatil/whatomate/internal/handlers"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
	"github.com/zerodha/fastglue"
)

func TestOrganizationCommercialRoutesKeepLoginActiveAndTargetWorkspacesDistinct(t *testing.T) {
	db := testutil.SetupTestDB(t)
	loginOrg := testutil.CreateTestOrganization(t, db)
	activeOrg := testutil.CreateTestOrganization(t, db)
	reseller := testutil.CreateTestReseller(t, db)
	targetOrg := testutil.CreateTestOrganizationForReseller(t, db, reseller.ID)
	owner := testutil.CreateTestUser(
		t,
		db,
		loginOrg.ID,
		testutil.WithEmail(testutil.UniqueEmail("commercial-router-owner")),
		testutil.WithSuperAdmin(),
	)

	planCode := "router-growth-" + uuid.NewString()[:8]
	plan := models.Plan{
		BaseModel:    models.BaseModel{ID: uuid.New()},
		ResellerID:   &reseller.ID,
		ScopeKey:     reseller.ID.String(),
		Code:         planCode,
		Name:         "Router Target Growth",
		Status:       models.CommercialPlanStatusActive,
		Vertical:     "general",
		TrialDays:    14,
		IsPublic:     false,
		Metadata:     models.JSONB{},
		DisplayOrder: 1,
	}
	require.NoError(t, db.Create(&plan).Error)
	price := models.PlanPrice{
		BaseModel:       models.BaseModel{ID: uuid.New()},
		PlanID:          plan.ID,
		Code:            planCode + "-monthly",
		Provider:        models.BillingProviderManual,
		Currency:        "MYR",
		UnitAmountMinor: 60000,
		Interval:        models.BillingIntervalMonth,
		IntervalCount:   1,
		TaxBehavior:     "exclusive",
		IsActive:        true,
		ProviderData:    models.JSONB{},
		Metadata:        models.JSONB{},
	}
	require.NoError(t, db.Create(&price).Error)

	cfg := &config.Config{
		JWT: config.JWTConfig{Secret: testutil.TestJWTSecret},
	}
	app := &handlers.App{
		Config:     cfg,
		DB:         db,
		Log:        testutil.NopLogger(),
		HTTPClient: http.DefaultClient,
	}
	g := fastglue.NewGlue()
	setupRoutes(g, app, app.Log, "", nil, cfg)
	token := testutil.GenerateTestRefreshToken(
		t,
		owner,
		testutil.TestJWTSecret,
		time.Hour,
	)

	plansCtx := invokeAuthenticatedRoute(
		t,
		g,
		fasthttp.MethodGet,
		"/api/admin/organizations/"+targetOrg.ID.String()+"/product/plans",
		token,
		activeOrg.ID,
		nil,
	)
	require.Equal(t, fasthttp.StatusOK, plansCtx.Response.StatusCode(), string(plansCtx.Response.Body()))
	var plansEnvelope struct {
		Data struct {
			Plans []handlers.ProductPlanResponse `json:"plans"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(plansCtx.Response.Body(), &plansEnvelope))
	require.Len(t, plansEnvelope.Data.Plans, 1)
	assert.Equal(t, planCode, plansEnvelope.Data.Plans[0].Code)

	putCtx := invokeAuthenticatedRoute(
		t,
		g,
		fasthttp.MethodPut,
		"/api/admin/organizations/"+targetOrg.ID.String()+"/subscription",
		token,
		activeOrg.ID,
		map[string]any{
			"plan_id":          plan.ID,
			"plan_price_id":    price.ID,
			"status":           models.SubscriptionStatusActive,
			"manual_reference": "ROUTER-TARGET-REGRESSION",
		},
	)
	require.Equal(t, fasthttp.StatusOK, putCtx.Response.StatusCode(), string(putCtx.Response.Body()))

	var subscription models.Subscription
	require.NoError(t, db.Where("organization_id = ?", targetOrg.ID).First(&subscription).Error)
	assert.Equal(t, plan.ID, subscription.PlanID)
	var nonTargetCount int64
	require.NoError(t, db.Model(&models.Subscription{}).
		Where("organization_id IN ?", []uuid.UUID{loginOrg.ID, activeOrg.ID}).
		Count(&nonTargetCount).Error)
	assert.Zero(t, nonTargetCount)

	var auditLog models.AuditLog
	require.NoError(t, db.Where(
		"organization_id = ? AND resource_type = ? AND resource_id = ?",
		targetOrg.ID,
		"commercial_subscription",
		subscription.ID,
	).First(&auditLog).Error)
	var licensedByWorkspace any
	for _, rawChange := range auditLog.Changes {
		change, ok := rawChange.(map[string]any)
		if ok && change["field"] == "licensed_by_workspace" {
			licensedByWorkspace = change["new_value"]
			break
		}
	}
	assert.Equal(t, activeOrg.ID.String(), licensedByWorkspace)

	getCtx := invokeAuthenticatedRoute(
		t,
		g,
		fasthttp.MethodGet,
		"/api/admin/organizations/"+targetOrg.ID.String()+"/subscription",
		token,
		activeOrg.ID,
		nil,
	)
	require.Equal(t, fasthttp.StatusOK, getCtx.Response.StatusCode(), string(getCtx.Response.Body()))
	var subscriptionEnvelope struct {
		Data handlers.ProductSubscriptionResponse `json:"data"`
	}
	require.NoError(t, json.Unmarshal(getCtx.Response.Body(), &subscriptionEnvelope))
	assert.Equal(t, planCode, subscriptionEnvelope.Data.PlanCode)
	assert.Equal(t, string(models.SubscriptionStatusActive), subscriptionEnvelope.Data.Status)
}

func invokeAuthenticatedRoute(
	t *testing.T,
	g *fastglue.Fastglue,
	method string,
	path string,
	token string,
	activeOrgID uuid.UUID,
	body any,
) *fasthttp.RequestCtx {
	t.Helper()
	ctx := &fasthttp.RequestCtx{}
	ctx.Request.Header.SetMethod(method)
	ctx.Request.SetRequestURI(path)
	ctx.Request.Header.Set("Authorization", "Bearer "+token)
	ctx.Request.Header.Set("X-Organization-ID", activeOrgID.String())
	if body != nil {
		encoded, err := json.Marshal(body)
		require.NoError(t, err)
		ctx.Request.Header.SetContentType("application/json")
		ctx.Request.SetBody(encoded)
	}
	g.Handler()(ctx)
	return ctx
}
