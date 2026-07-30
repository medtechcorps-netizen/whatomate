package handlers

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
)

func TestProductCommercialSubscriptionResponsePreservesPlanPriceIdentity(t *testing.T) {
	planID := uuid.New()
	priceID := uuid.New()
	subscription := models.Subscription{
		BaseModel:   models.BaseModel{ID: uuid.New()},
		PlanID:      planID,
		PlanPriceID: &priceID,
		Provider:    models.BillingProviderManual,
		Status:      models.SubscriptionStatusActive,
	}
	response := productCommercialSubscriptionToResponse(
		&subscription,
		&models.Plan{BaseModel: models.BaseModel{ID: planID}, Code: "growth", Name: "Growth"},
	)

	require.NotNil(t, response.PlanID)
	require.NotNil(t, response.PlanPriceID)
	assert.Equal(t, planID, *response.PlanID)
	assert.Equal(t, priceID, *response.PlanPriceID)
}

func TestProductCommercialSubscriptionResponseUsesEffectiveExpiredStatus(t *testing.T) {
	now := time.Now().UTC()
	effectiveEnd := now.Add(-time.Minute)
	tests := []models.Subscription{
		{
			BaseModel:        models.BaseModel{ID: uuid.New()},
			PlanID:           uuid.New(),
			Provider:         models.BillingProviderManual,
			Status:           models.SubscriptionStatusActive,
			CurrentPeriodEnd: &effectiveEnd,
		},
		{
			BaseModel:   models.BaseModel{ID: uuid.New()},
			PlanID:      uuid.New(),
			Provider:    models.BillingProviderManual,
			Status:      models.SubscriptionStatusTrialing,
			TrialEndsAt: &effectiveEnd,
		},
	}

	for _, subscription := range tests {
		storedStatus := subscription.Status
		t.Run(string(storedStatus), func(t *testing.T) {
			response := productCommercialSubscriptionToResponseAt(
				&subscription,
				nil,
				now,
			)

			assert.Equal(t, string(models.SubscriptionStatusExpired), response.Status)
			assert.Equal(
				t,
				storedStatus,
				subscription.Status,
				"effective projection must not rewrite billing history",
			)
		})
	}
}

func TestProductEntitlementResponsesUseEffectiveExpiredStatus(t *testing.T) {
	db := testutil.SetupTestDB(t)
	organization := testutil.CreateTestOrganization(t, db)
	user := testutil.CreateTestUser(t, db, organization.ID)
	enableBookingCommerceTestEntitlement(
		t,
		db,
		organization.ID,
		user.ID,
		"crm.enabled",
	)
	periodEnd := time.Now().UTC().Add(-time.Minute)
	require.NoError(t, db.Model(&models.Subscription{}).
		Where("organization_id = ?", organization.ID).
		Update("current_period_end", periodEnd).Error)

	app := &App{DB: db, Log: testutil.NopLogger()}
	request := testutil.NewGETRequest(t)
	testutil.SetAuthContext(request, organization.ID, user.ID)
	require.NoError(t, app.GetProductEntitlements(request))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(request))

	var response ProductEntitlementsResponse
	testutil.ParseEnvelopeResponse(t, request, &response)
	assert.Equal(t, models.SubscriptionStatusExpired, response.SubscriptionStatus)
	assert.Equal(t, "suspended", response.Mode)
	assert.Empty(t, response.Entitlements)

	decision, err := app.EvaluateProductEntitlement(
		user.ID,
		organization.ID,
		"crm.enabled",
	)
	require.NoError(t, err)
	assert.Equal(t, models.SubscriptionStatusExpired, decision.SubscriptionStatus)
	assert.False(t, decision.Allowed)

	var stored models.Subscription
	require.NoError(t, db.Where("organization_id = ?", organization.ID).
		First(&stored).Error)
	assert.Equal(t, models.SubscriptionStatusActive, stored.Status)
}

func TestProductSubscriptionReadersPreferLiveSubscriptionOverNewerHistory(t *testing.T) {
	db := testutil.SetupTestDB(t)
	organization := testutil.CreateTestOrganization(t, db)
	user := testutil.CreateTestUser(t, db, organization.ID, testutil.WithSuperAdmin())
	plan := models.Plan{
		BaseModel: models.BaseModel{ID: uuid.New()},
		ScopeKey:  "test-" + uuid.NewString(),
		Code:      "live-growth",
		Name:      "Live Growth",
		Status:    models.CommercialPlanStatusActive,
		Vertical:  "general",
		IsPublic:  false,
		Metadata:  models.JSONB{},
	}
	require.NoError(t, db.Create(&plan).Error)
	account := models.BillingAccount{
		BaseModel:       models.BaseModel{ID: uuid.New()},
		OrganizationID:  organization.ID,
		Provider:        models.BillingProviderManual,
		Status:          models.BillingAccountStatusActive,
		DefaultCurrency: "MYR",
		BillingProfile:  models.JSONB{},
		ProviderData:    models.JSONB{},
		Metadata:        models.JSONB{},
	}
	require.NoError(t, db.Create(&account).Error)

	now := time.Now().UTC()
	periodStart := now.Add(-time.Hour)
	periodEnd := now.AddDate(0, 1, 0)
	live := models.Subscription{
		BaseModel: models.BaseModel{
			ID:        uuid.New(),
			CreatedAt: now.Add(-24 * time.Hour),
		},
		OrganizationID:       organization.ID,
		BillingAccountID:     account.ID,
		PlanID:               plan.ID,
		Provider:             models.BillingProviderManual,
		Status:               models.SubscriptionStatusActive,
		Quantity:             1,
		CollectionMethod:     "manual",
		EntitlementsSnapshot: models.JSONB{"crm.enabled": true},
		ProviderData:         models.JSONB{},
		CurrentPeriodStart:   &periodStart,
		CurrentPeriodEnd:     &periodEnd,
		CreatedByID:          &user.ID,
	}
	require.NoError(t, db.Create(&live).Error)
	canceledAt := now.Add(-time.Minute)
	history := models.Subscription{
		BaseModel: models.BaseModel{
			ID:        uuid.New(),
			CreatedAt: now,
		},
		OrganizationID:       organization.ID,
		BillingAccountID:     account.ID,
		PlanID:               plan.ID,
		Provider:             models.BillingProviderManual,
		Status:               models.SubscriptionStatusCanceled,
		Quantity:             1,
		CollectionMethod:     "manual",
		EntitlementsSnapshot: models.JSONB{"crm.enabled": false},
		ProviderData:         models.JSONB{},
		CanceledAt:           &canceledAt,
		EndedAt:              &canceledAt,
		CreatedByID:          &user.ID,
	}
	require.NoError(t, db.Create(&history).Error)

	app := &App{DB: db, Log: testutil.NopLogger()}
	subscriptionRequest := testutil.NewGETRequest(t)
	testutil.SetAuthContext(subscriptionRequest, organization.ID, user.ID)
	require.NoError(t, app.GetProductSubscription(subscriptionRequest))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(subscriptionRequest))
	var subscriptionResponse ProductSubscriptionResponse
	testutil.ParseEnvelopeResponse(t, subscriptionRequest, &subscriptionResponse)
	require.NotNil(t, subscriptionResponse.ID)
	assert.Equal(t, live.ID, *subscriptionResponse.ID)
	assert.Equal(t, string(models.SubscriptionStatusActive), subscriptionResponse.Status)
	assert.Equal(t, plan.Code, subscriptionResponse.PlanCode)

	entitlementsRequest := testutil.NewGETRequest(t)
	testutil.SetAuthContext(entitlementsRequest, organization.ID, user.ID)
	require.NoError(t, app.GetProductEntitlements(entitlementsRequest))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(entitlementsRequest))
	var entitlementsResponse ProductEntitlementsResponse
	testutil.ParseEnvelopeResponse(t, entitlementsRequest, &entitlementsResponse)
	assert.Equal(t, "subscription", entitlementsResponse.Mode)
	assert.Equal(t, models.SubscriptionStatusActive, entitlementsResponse.SubscriptionStatus)
	assert.Equal(t, plan.Code, entitlementsResponse.PlanCode)
	assert.Equal(t, true, entitlementsResponse.Entitlements["crm.enabled"])

	decision, err := app.EvaluateProductEntitlement(user.ID, organization.ID, "crm.enabled")
	require.NoError(t, err)
	require.NotNil(t, decision.SubscriptionID)
	assert.Equal(t, live.ID, *decision.SubscriptionID)
	assert.True(t, decision.Allowed)
}

func TestProductCommercialFindSubscriptionPlanRejectsFuturePrice(t *testing.T) {
	db := testutil.SetupTestDB(t)
	organization := testutil.CreateTestOrganization(t, db)
	plan := models.Plan{
		BaseModel: models.BaseModel{ID: uuid.New()},
		ScopeKey:  "platform",
		Code:      "future-growth",
		Name:      "Future Growth",
		Status:    models.CommercialPlanStatusActive,
		Vertical:  "general",
		IsPublic:  true,
		Metadata:  models.JSONB{},
		TrialDays: 14,
	}
	require.NoError(t, db.Create(&plan).Error)
	effectiveFrom := time.Now().UTC().Add(time.Hour)
	price := models.PlanPrice{
		BaseModel:       models.BaseModel{ID: uuid.New()},
		PlanID:          plan.ID,
		Code:            "future-growth-month",
		Provider:        models.BillingProviderManual,
		Currency:        "MYR",
		UnitAmountMinor: 1000,
		Interval:        models.BillingIntervalMonth,
		IntervalCount:   1,
		TaxBehavior:     "exclusive",
		IsActive:        true,
		EffectiveFrom:   &effectiveFrom,
		ProviderData:    models.JSONB{},
		Metadata:        models.JSONB{},
	}
	require.NoError(t, db.Create(&price).Error)

	_, _, err := productCommercialFindSubscriptionPlan(db, organization, &SetOrganizationSubscriptionRequest{
		PlanID:      &plan.ID,
		PlanPriceID: &price.ID,
		Status:      models.SubscriptionStatusActive,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Active manual plan price not found")
}
