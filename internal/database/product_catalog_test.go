package database_test

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/database"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEnsureReReplyProductCatalogPublishesApprovedPlansIdempotently(t *testing.T) {
	db := testutil.SetupTestDB(t)
	tx := db.Begin()
	require.NoError(t, tx.Error)
	t.Cleanup(func() { _ = tx.Rollback().Error })

	legacyGrowth := models.Plan{
		BaseModel:    models.BaseModel{ID: uuid.New()},
		ScopeKey:     "platform",
		Code:         "rereply-growth",
		Name:         "ReReply Growth",
		Description:  "Trial catalog",
		Status:       models.CommercialPlanStatusActive,
		Vertical:     "general",
		TrialDays:    14,
		DisplayOrder: 1,
		IsPublic:     false,
		Metadata:     models.JSONB{},
	}
	require.NoError(t, tx.Create(&legacyGrowth).Error)

	legacyPrice := models.PlanPrice{
		BaseModel:       models.BaseModel{ID: uuid.New()},
		PlanID:          legacyGrowth.ID,
		Code:            "rereply-growth-myr-month",
		Provider:        models.BillingProviderManual,
		Currency:        "MYR",
		UnitAmountMinor: 0,
		Interval:        models.BillingIntervalMonth,
		IntervalCount:   1,
		TaxBehavior:     "exclusive",
		IsActive:        true,
		ProviderData:    models.JSONB{},
		Metadata:        models.JSONB{},
	}
	require.NoError(t, tx.Create(&legacyPrice).Error)

	promotionalPrice := models.PlanPrice{
		BaseModel:       models.BaseModel{ID: uuid.New()},
		PlanID:          legacyGrowth.ID,
		Code:            "rereply-growth-community-grant",
		Provider:        models.BillingProviderManual,
		Currency:        "MYR",
		UnitAmountMinor: 0,
		Interval:        models.BillingIntervalMonth,
		IntervalCount:   1,
		TaxBehavior:     "exclusive",
		IsActive:        true,
		ProviderData:    models.JSONB{},
		Metadata:        models.JSONB{"campaign": "community"},
	}
	require.NoError(t, tx.Create(&promotionalPrice).Error)

	require.NoError(t, database.EnsureReReplyProductCatalog(tx))
	require.NoError(t, database.EnsureReReplyProductCatalog(tx))

	var plans []models.Plan
	require.NoError(t, tx.Where(
		"scope_key = ? AND code IN ?",
		"platform",
		[]string{"rereply-starter", "rereply-growth"},
	).Order("display_order ASC").Find(&plans).Error)
	require.Len(t, plans, 2)
	assert.Equal(t, "rereply-starter", plans[0].Code)
	assert.Equal(t, "rereply-growth", plans[1].Code)
	assert.Equal(t, legacyGrowth.ID, plans[1].ID)
	for _, plan := range plans {
		assert.Equal(t, models.CommercialPlanStatusActive, plan.Status)
		assert.True(t, plan.IsPublic)
		assert.NotNil(t, plan.PublishedAt)
	}

	assertCatalogPrice := func(planCode, priceCode string, amount int64) models.Plan {
		t.Helper()
		var plan models.Plan
		require.NoError(t, tx.Where(
			"scope_key = ? AND code = ?",
			"platform",
			planCode,
		).First(&plan).Error)

		var prices []models.PlanPrice
		require.NoError(t, tx.Where(
			"plan_id = ? AND is_active = ?",
			plan.ID,
			true,
		).Find(&prices).Error)
		assignablePrices := make([]models.PlanPrice, 0, len(prices))
		for _, price := range prices {
			if assignable, configured := price.Metadata["assignable"].(bool); configured && !assignable {
				continue
			}
			assignablePrices = append(assignablePrices, price)
		}
		var approvedPrice *models.PlanPrice
		for i := range assignablePrices {
			if assignablePrices[i].Code == priceCode {
				approvedPrice = &assignablePrices[i]
				break
			}
		}
		require.NotNil(t, approvedPrice)
		assert.Equal(t, "MYR", approvedPrice.Currency)
		assert.Equal(t, amount, approvedPrice.UnitAmountMinor)
		assert.Zero(t, approvedPrice.SetupAmountMinor)
		assert.Equal(t, models.BillingIntervalMonth, approvedPrice.Interval)
		assert.Equal(t, models.BillingProviderManual, approvedPrice.Provider)
		assert.Equal(t, "exclusive", approvedPrice.TaxBehavior)
		assert.Nil(t, approvedPrice.EffectiveFrom)
		assert.Nil(t, approvedPrice.EffectiveUntil)
		return plan
	}

	starter := assertCatalogPrice(
		"rereply-starter",
		"rereply-starter-myr-month-2026-v1",
		30000,
	)
	growth := assertCatalogPrice(
		"rereply-growth",
		"rereply-growth-myr-month-2026-v1",
		60000,
	)

	var refreshedLegacy models.PlanPrice
	require.NoError(t, tx.Where("id = ?", legacyPrice.ID).First(&refreshedLegacy).Error)
	assert.True(t, refreshedLegacy.IsActive)
	assert.Equal(t, false, refreshedLegacy.Metadata["assignable"])
	assert.Equal(
		t,
		"rereply-growth-myr-month-2026-v1",
		refreshedLegacy.Metadata["replacement_price_code"],
	)
	var refreshedPromotion models.PlanPrice
	require.NoError(t, tx.Where("id = ?", promotionalPrice.ID).First(&refreshedPromotion).Error)
	assert.Equal(t, "community", refreshedPromotion.Metadata["campaign"])
	_, promotionWasRetired := refreshedPromotion.Metadata["assignable"]
	assert.False(t, promotionWasRetired, "an intentional RM0 grant must remain assignable")

	assertEntitlement := func(planID uuid.UUID, key string, enabled bool) {
		t.Helper()
		var entitlement models.PlanEntitlement
		require.NoError(t, tx.Where(
			"plan_id = ? AND key = ?",
			planID,
			key,
		).First(&entitlement).Error)
		assert.Equal(t, models.EntitlementValueTypeBoolean, entitlement.ValueType)
		assert.Equal(t, models.EntitlementEnforcementHard, entitlement.Enforcement)
		assert.Equal(t, enabled, entitlement.Value["value"])
	}

	for _, key := range []string{
		"omnichannel.enabled",
		"crm.enabled",
		"bookings.enabled",
		"commerce.enabled",
		"copilot.enabled",
		"threads.public_engagement.enabled",
	} {
		assertEntitlement(starter.ID, key, false)
	}
	for _, key := range []string{
		"omnichannel.enabled",
		"crm.enabled",
		"bookings.enabled",
		"commerce.enabled",
		"copilot.enabled",
	} {
		assertEntitlement(growth.ID, key, true)
	}
	assertEntitlement(growth.ID, "threads.public_engagement.enabled", false)

	// Once the initial catalog backfill is recorded, supported control-plane
	// changes must survive subsequent application startups.
	now := time.Now().UTC()
	require.NoError(t, tx.Model(&models.Plan{}).
		Where("id = ?", growth.ID).
		Updates(map[string]any{
			"status":      models.CommercialPlanStatusArchived,
			"is_public":   false,
			"archived_at": now,
		}).Error)
	require.NoError(t, tx.Model(&models.PlanPrice{}).
		Where("plan_id = ? AND code = ?", growth.ID, "rereply-growth-myr-month-2026-v1").
		Update("is_active", false).Error)
	require.NoError(t, tx.Model(&models.PlanEntitlement{}).
		Where("plan_id = ? AND key = ?", growth.ID, "copilot.enabled").
		Update("value", models.JSONB{"value": false}).Error)

	require.NoError(t, database.EnsureReReplyProductCatalog(tx))

	var operatorManagedPlan models.Plan
	require.NoError(t, tx.Where("id = ?", growth.ID).First(&operatorManagedPlan).Error)
	assert.Equal(t, models.CommercialPlanStatusArchived, operatorManagedPlan.Status)
	assert.False(t, operatorManagedPlan.IsPublic)
	assert.NotNil(t, operatorManagedPlan.ArchivedAt)

	var operatorManagedPrice models.PlanPrice
	require.NoError(t, tx.Where(
		"plan_id = ? AND code = ?",
		growth.ID,
		"rereply-growth-myr-month-2026-v1",
	).First(&operatorManagedPrice).Error)
	assert.False(t, operatorManagedPrice.IsActive)
	assertEntitlement(growth.ID, "copilot.enabled", false)
}

func TestEnsureReReplyProductCatalogRejectsCommercialPriceDriftAndRollsBack(t *testing.T) {
	db := testutil.SetupTestDB(t)
	tx := db.Begin()
	require.NoError(t, tx.Error)
	t.Cleanup(func() { _ = tx.Rollback().Error })

	conflictingGrowth := models.Plan{
		BaseModel:    models.BaseModel{ID: uuid.New()},
		ScopeKey:     "platform",
		Code:         "rereply-growth",
		Name:         "Unapproved Growth",
		Description:  "Must survive a failed catalog migration",
		Status:       models.CommercialPlanStatusDraft,
		Vertical:     "general",
		DisplayOrder: 99,
		Metadata:     models.JSONB{},
	}
	require.NoError(t, tx.Create(&conflictingGrowth).Error)
	conflictingPrice := models.PlanPrice{
		BaseModel:        models.BaseModel{ID: uuid.New()},
		PlanID:           conflictingGrowth.ID,
		Code:             "rereply-growth-myr-month-2026-v1",
		Provider:         models.BillingProviderManual,
		Currency:         "MYR",
		UnitAmountMinor:  60000,
		SetupAmountMinor: 10000,
		Interval:         models.BillingIntervalMonth,
		IntervalCount:    1,
		TaxBehavior:      "exclusive",
		IsActive:         true,
		ProviderData:     models.JSONB{},
		Metadata:         models.JSONB{},
	}
	require.NoError(t, tx.Create(&conflictingPrice).Error)

	err := database.EnsureReReplyProductCatalog(tx)
	require.ErrorContains(t, err, "does not match the approved immutable price")

	var starterCount int64
	require.NoError(t, tx.Model(&models.Plan{}).
		Where("scope_key = ? AND code = ?", "platform", "rereply-starter").
		Count(&starterCount).Error)
	assert.Zero(t, starterCount, "the Starter insert must roll back with the failed catalog")

	var unchangedGrowth models.Plan
	require.NoError(t, tx.Where("id = ?", conflictingGrowth.ID).First(&unchangedGrowth).Error)
	assert.Equal(t, "Unapproved Growth", unchangedGrowth.Name)
	assert.Equal(t, models.CommercialPlanStatusDraft, unchangedGrowth.Status)
	assert.Equal(t, 99, unchangedGrowth.DisplayOrder)
	_, backfillRecorded := unchangedGrowth.Metadata["rereply_catalog_seed_version"]
	assert.False(t, backfillRecorded)
}

func TestEnsureReReplyProductCatalogValidatesCompleteImmutablePrice(t *testing.T) {
	testCases := []struct {
		name   string
		update map[string]any
	}{
		{name: "setup amount", update: map[string]any{"setup_amount_minor": int64(1)}},
		{name: "tax behavior", update: map[string]any{"tax_behavior": "inclusive"}},
		{name: "effective from", update: map[string]any{"effective_from": time.Now().UTC()}},
		{name: "effective until", update: map[string]any{"effective_until": time.Now().UTC().Add(time.Hour)}},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			db := testutil.SetupTestDB(t)
			tx := db.Begin()
			require.NoError(t, tx.Error)
			t.Cleanup(func() { _ = tx.Rollback().Error })

			require.NoError(t, database.EnsureReReplyProductCatalog(tx))
			require.NoError(t, tx.Model(&models.PlanPrice{}).
				Where("code = ?", "rereply-starter-myr-month-2026-v1").
				Updates(testCase.update).Error)

			err := database.EnsureReReplyProductCatalog(tx)
			require.ErrorContains(t, err, "does not match the approved immutable price")
		})
	}
}
