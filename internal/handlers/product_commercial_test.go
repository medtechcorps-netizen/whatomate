package handlers

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/config"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func TestProductCommercialBuiltInWorkspaceTemplates(t *testing.T) {
	t.Parallel()

	require.Len(t, productBuiltInWorkspaceTemplates, 3)
	seen := map[string]bool{}
	for _, template := range productBuiltInWorkspaceTemplates {
		assert.False(t, seen[template.Key], "duplicate built-in key %q", template.Key)
		seen[template.Key] = true
		assert.True(t, productCommercialValidIdentifier(template.Key, 100))
		assert.True(t, productCommercialValidVertical(template.Vertical))
		assert.NotEmpty(t, template.Name)
		assert.NotEmpty(t, template.Highlights)
		assert.Equal(t, template.Vertical, template.Manifest["vertical"])

		data, err := json.Marshal(template.Manifest)
		require.NoError(t, err)
		serialized := strings.ToLower(string(data))
		for _, forbidden := range []string{"password", "secret", "access_token", "private_key"} {
			assert.NotContains(t, serialized, forbidden)
		}
	}
	assert.True(t, seen["clinic"])
	assert.True(t, seen["pharmacy"])
	assert.True(t, seen["wellness"])
}

func TestProductCommercialRejectsRetiredManualPlanPrice(t *testing.T) {
	db := testutil.SetupTestDB(t)
	tx := db.Begin()
	require.NoError(t, tx.Error)
	t.Cleanup(func() { _ = tx.Rollback().Error })

	organization := testutil.CreateTestOrganization(t, tx)
	plan := models.Plan{
		BaseModel:    models.BaseModel{ID: uuid.New()},
		ScopeKey:     "platform",
		Code:         fmt.Sprintf("retired-plan-%s", uuid.NewString()),
		Name:         "Retired price plan",
		Status:       models.CommercialPlanStatusActive,
		Vertical:     "general",
		Metadata:     models.JSONB{},
		DisplayOrder: 1,
	}
	require.NoError(t, tx.Create(&plan).Error)

	price := models.PlanPrice{
		BaseModel:       models.BaseModel{ID: uuid.New()},
		PlanID:          plan.ID,
		Code:            fmt.Sprintf("retired-price-%s", uuid.NewString()),
		Provider:        models.BillingProviderManual,
		Currency:        "MYR",
		UnitAmountMinor: 0,
		Interval:        models.BillingIntervalMonth,
		IntervalCount:   1,
		TaxBehavior:     "exclusive",
		IsActive:        true,
		ProviderData:    models.JSONB{},
		Metadata:        models.JSONB{"assignable": false},
	}
	require.NoError(t, tx.Create(&price).Error)

	req := SetOrganizationSubscriptionRequest{
		PlanID:      &plan.ID,
		PlanPriceID: &price.ID,
	}
	_, _, err := productCommercialFindSubscriptionPlan(
		tx,
		organization,
		&req,
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "retired")
}

func TestProductCommercialPlanCatalogValidation(t *testing.T) {
	t.Parallel()

	prices := []PlanPriceInput{{
		Code:            " Monthly-Growth ",
		Currency:        "myr",
		UnitAmountMinor: 12900,
		Interval:        models.BillingIntervalMonth,
	}}
	entitlements := []PlanEntitlementInput{{
		Key:         " Agents.Max ",
		ValueType:   models.EntitlementValueTypeInteger,
		Value:       float64(10),
		Enforcement: models.EntitlementEnforcementHard,
	}}
	require.NoError(t, productCommercialValidatePlanCatalog(prices, entitlements, true))
	assert.Equal(t, "monthly-growth", prices[0].Code)
	assert.Equal(t, "MYR", prices[0].Currency)
	assert.Equal(t, 1, prices[0].IntervalCount)
	assert.Equal(t, "agents.max", entitlements[0].Key)

	t.Run("requires an initial price", func(t *testing.T) {
		err := productCommercialValidatePlanCatalog(nil, nil, true)
		require.EqualError(t, err, "at least one price is required")
	})

	t.Run("rejects duplicate price codes", func(t *testing.T) {
		duplicates := []PlanPriceInput{
			{
				Code:            "growth",
				Currency:        "MYR",
				UnitAmountMinor: 100,
				Interval:        models.BillingIntervalMonth,
			},
			{
				Code:            "growth",
				Currency:        "MYR",
				UnitAmountMinor: 200,
				Interval:        models.BillingIntervalYear,
			},
		}
		require.EqualError(
			t,
			productCommercialValidatePlanCatalog(duplicates, nil, true),
			"duplicate price code",
		)
	})

	t.Run("rejects fractional integer entitlement", func(t *testing.T) {
		input := PlanEntitlementInput{
			Key:       "messages.max",
			ValueType: models.EntitlementValueTypeInteger,
			Value:     1.5,
		}
		err := productCommercialValidatePlanEntitlement(&input)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "requires an integer value")
	})
}

func TestProductCommercialSubscriptionValidationIsManualOnly(t *testing.T) {
	t.Parallel()

	planID := uuid.New()
	priceID := uuid.New()
	valid := SetOrganizationSubscriptionRequest{
		PlanID:          &planID,
		PlanPriceID:     &priceID,
		Status:          models.SubscriptionStatusActive,
		ManualReference: " contract-2026-001 ",
	}
	require.NoError(t, productCommercialValidateSetSubscription(&valid))
	assert.Equal(t, "contract-2026-001", valid.ManualReference)

	missingReference := valid
	missingReference.ManualReference = " "
	require.EqualError(
		t,
		productCommercialValidateSetSubscription(&missingReference),
		"manual_reference is required",
	)

	controlReference := valid
	controlReference.ManualReference = "contract-2026-001\nforged-line"
	require.EqualError(
		t,
		productCommercialValidateSetSubscription(&controlReference),
		"manual_reference must not contain control characters",
	)

	for _, status := range []models.SubscriptionStatus{
		models.SubscriptionStatusPastDue,
		models.SubscriptionStatusCanceled,
		models.SubscriptionStatusIncomplete,
	} {
		request := valid
		request.Status = status
		err := productCommercialValidateSetSubscription(&request)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "active or trialing")
	}

	trialDays := 30
	trial := valid
	trial.Status = models.SubscriptionStatusTrialing
	trial.TrialDays = &trialDays
	require.NoError(t, productCommercialValidateSetSubscription(&trial))

	activeWithTrial := valid
	activeWithTrial.TrialDays = &trialDays
	require.EqualError(
		t,
		productCommercialValidateSetSubscription(&activeWithTrial),
		"trial_days is only valid for a trialing subscription",
	)
}

func TestProductCommercialServerSideEntitlementSemantics(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		value any
		want  bool
	}{
		{"enabled boolean", true, true},
		{"disabled boolean", false, false},
		{"positive quota", float64(10), true},
		{"zero quota", float64(0), false},
		{"enabled object", models.JSONB{"enabled": true}, true},
		{"nested false", models.JSONB{"value": false}, false},
		{"disabled string", "disabled", false},
		{"tier string", "growth", true},
		{"missing value", nil, false},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, productCommercialEntitlementAllows(tt.value))
		})
	}

	now := time.Now().UTC()
	future := now.Add(time.Hour)
	past := now.Add(-time.Hour)
	assert.True(t, productCommercialSubscriptionPermitsFeatures(
		&models.Subscription{
			Status:           models.SubscriptionStatusActive,
			CurrentPeriodEnd: &future,
		},
		now,
	))
	assert.True(t, productCommercialSubscriptionPermitsFeatures(
		&models.Subscription{
			Status:     models.SubscriptionStatusActive,
			GraceUntil: &future,
		},
		now,
	), "a future explicit grace window may bridge a delayed renewal")
	assert.True(t, productCommercialSubscriptionPermitsFeatures(
		&models.Subscription{
			Status:      models.SubscriptionStatusTrialing,
			TrialEndsAt: &future,
		},
		now,
	))
	assert.True(t, productCommercialSubscriptionPermitsFeatures(
		&models.Subscription{
			Status:     models.SubscriptionStatusPastDue,
			GraceUntil: &future,
		},
		now,
	))
	assert.False(t, productCommercialSubscriptionPermitsFeatures(
		&models.Subscription{Status: models.SubscriptionStatusTrialing},
		now,
	), "a trial without an explicit end must fail closed")
	assert.False(t, productCommercialSubscriptionPermitsFeatures(
		&models.Subscription{
			Status:      models.SubscriptionStatusTrialing,
			TrialEndsAt: &past,
		},
		now,
	), "an expired trial must fail even if its status is stale")
	assert.False(t, productCommercialSubscriptionPermitsFeatures(
		&models.Subscription{Status: models.SubscriptionStatusPastDue},
		now,
	), "past_due requires an explicit grace window")
	assert.False(t, productCommercialSubscriptionPermitsFeatures(
		&models.Subscription{
			Status:     models.SubscriptionStatusPastDue,
			GraceUntil: &past,
		},
		now,
	), "an expired grace window must fail closed")
	assert.False(t, productCommercialSubscriptionPermitsFeatures(
		&models.Subscription{Status: models.SubscriptionStatusActive},
		now,
	), "active without an explicit current period must fail closed")
	assert.False(t, productCommercialSubscriptionPermitsFeatures(
		&models.Subscription{
			Status:           models.SubscriptionStatusActive,
			CurrentPeriodEnd: &past,
		},
		now,
	), "an expired active period must fail closed")

	for _, status := range []models.SubscriptionStatus{
		models.SubscriptionStatusIncomplete,
		models.SubscriptionStatusPaused,
		models.SubscriptionStatusCanceled,
		models.SubscriptionStatusExpired,
	} {
		assert.False(t, productCommercialSubscriptionPermitsFeatures(
			&models.Subscription{Status: status},
			now,
		))
	}

	resourceKeys := map[string]string{
		models.ResourceCRMPipelines:  "crm.enabled",
		models.ResourceBookings:      "bookings.enabled",
		models.ResourcePayments:      "commerce.enabled",
		models.ResourceCopilot:       "copilot.enabled",
		models.ResourceConversations: "omnichannel.enabled",
	}
	for resource, expectedKey := range resourceKeys {
		key, gated := ProductEntitlementKeyForResource(resource)
		assert.True(t, gated)
		assert.Equal(t, expectedKey, key)
	}
	_, gated := ProductEntitlementKeyForResource(models.ResourceChat)
	assert.False(t, gated, "the existing WhatsApp inbox must remain outside new module gating")
}

func TestProductCommercialNoSubscriptionFailsClosed(t *testing.T) {
	db := testutil.SetupTestDB(t)
	organization := testutil.CreateTestOrganization(t, db)
	user := testutil.CreateTestUser(t, db, organization.ID)
	now := time.Now().UTC()
	override := models.EntitlementOverride{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: organization.ID,
		Key:            "crm.enabled",
		ValueType:      models.EntitlementValueTypeBoolean,
		Value:          models.JSONB{"value": true},
		Source:         "support",
		Reason:         "No-subscription override must not unlock modules",
		IsActive:       true,
		StartsAt:       now.Add(-time.Minute),
		CreatedByID:    user.ID,
	}
	require.NoError(t, db.Create(&override).Error)

	app := &App{DB: db, Log: testutil.NopLogger()}
	decision, err := app.EvaluateProductEntitlement(
		user.ID,
		organization.ID,
		"crm.enabled",
	)
	require.NoError(t, err)
	assert.False(t, decision.Allowed)
	assert.Equal(t, "unlicensed", decision.Mode)
	assert.False(t, decision.Overridden)
	assert.Contains(t, decision.Reason, "No commercial subscription")

	require.NoError(t, db.Model(&models.User{}).
		Where("id = ?", user.ID).
		Update("is_super_admin", true).Error)
	decision, err = app.EvaluateProductEntitlement(
		user.ID,
		organization.ID,
		"crm.enabled",
	)
	require.NoError(t, err)
	assert.False(t, decision.Allowed, "platform administrators must not bypass tenant licensing")
	assert.Equal(t, "unlicensed", decision.Mode)
}

func TestGetTenantSupportHealthIncludesOperationalSignals(t *testing.T) {
	db := testutil.SetupTestDB(t)
	organization := testutil.CreateTestOrganization(t, db)
	user := testutil.CreateTestUser(t, db, organization.ID, testutil.WithSuperAdmin())
	app := &App{DB: db, Log: testutil.NopLogger()}

	req := testutil.NewGETRequest(t)
	testutil.SetAuthContext(req, organization.ID, user.ID)
	require.NoError(t, app.GetTenantSupportHealth(req))
	assert.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req))

	var response TenantSupportHealthResponse
	testutil.ParseEnvelopeResponse(t, req, &response)
	keys := make(map[string]bool, len(response.Checks))
	for _, check := range response.Checks {
		keys[check.Key] = true
	}
	for _, key := range []string{
		"database",
		"channels",
		"subscription",
		"payments",
		"privacy",
		"recovery",
		"support",
	} {
		assert.True(t, keys[key], "missing tenant health check %q", key)
	}
}

func TestTenantSupportHealthWarnsWhenOmnichannelHasOnlyWhatsApp(t *testing.T) {
	db := testutil.SetupTestDB(t)
	organization := testutil.CreateTestOrganization(t, db)
	user := testutil.CreateTestUser(t, db, organization.ID, testutil.WithSuperAdmin())
	app := &App{DB: db, Log: testutil.NopLogger()}
	enableBookingCommerceTestEntitlement(
		t,
		db,
		organization.ID,
		user.ID,
		"omnichannel.enabled",
	)
	testutil.CreateTestWhatsAppAccount(t, db, organization.ID)

	request := testutil.NewGETRequest(t)
	testutil.SetAuthContext(request, organization.ID, user.ID)
	require.NoError(t, app.GetTenantSupportHealth(request))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(request))

	var response TenantSupportHealthResponse
	testutil.ParseEnvelopeResponse(t, request, &response)
	var channelHealth *TenantHealthCheckResponse
	for index := range response.Checks {
		if response.Checks[index].Key == "channels" {
			channelHealth = &response.Checks[index]
			break
		}
	}
	require.NotNil(t, channelHealth)
	assert.Equal(t, "warn", channelHealth.Status)
	assert.Contains(t, channelHealth.Detail, "omnichannel workspace")
	assert.Contains(t, channelHealth.Detail, "Instagram")
	assert.Contains(t, channelHealth.Detail, "Messenger")
}

func TestTenantSupportHealthPrioritizesDeliveryIncidentOverChannelSetupWarning(t *testing.T) {
	db := testutil.SetupTestDB(t)
	organization := testutil.CreateTestOrganization(t, db)
	user := testutil.CreateTestUser(t, db, organization.ID, testutil.WithSuperAdmin())
	app := &App{DB: db, Log: testutil.NopLogger()}
	enableBookingCommerceTestEntitlement(
		t,
		db,
		organization.ID,
		user.ID,
		"omnichannel.enabled",
	)
	whatsApp := testutil.CreateTestWhatsAppAccount(t, db, organization.ID)
	contact := testutil.CreateTestContact(t, db, organization.ID)
	now := time.Now().UTC()
	account := models.ChannelAccount{
		BaseModel:         models.BaseModel{ID: uuid.New()},
		OrganizationID:    organization.ID,
		Channel:           models.ChannelWhatsApp,
		Provider:          "meta_legacy",
		Name:              "WhatsApp inbox mirror",
		ExternalAccountID: whatsApp.PhoneID,
		Status:            models.ChannelAccountStatusActive,
		Capabilities:      models.JSONB{},
		Config:            models.JSONB{},
		Metadata:          models.JSONB{},
	}
	require.NoError(t, db.Create(&account).Error)
	conversation := models.InboxConversation{
		BaseModel:              models.BaseModel{ID: uuid.New()},
		OrganizationID:         organization.ID,
		ChannelAccountID:       account.ID,
		ContactID:              contact.ID,
		Channel:                models.ChannelWhatsApp,
		ExternalConversationID: "health-priority-conversation",
		Status:                 models.InboxConversationStatusOpen,
		OpenedAt:               now,
		Config:                 models.JSONB{},
		Metadata:               models.JSONB{},
	}
	require.NoError(t, db.Create(&conversation).Error)
	job := models.OutboxJob{
		BaseModel:        models.BaseModel{ID: uuid.New()},
		OrganizationID:   organization.ID,
		ChannelAccountID: account.ID,
		ConversationID:   conversation.ID,
		IdempotencyKey:   "health-priority:" + uuid.NewString(),
		PayloadDigest:    "health-priority",
		Purpose:          models.ChannelPreferencePurposeService,
		Status:           models.OutboxJobStatusRetrying,
		AvailableAt:      now.Add(-time.Hour),
		MaxAttempts:      8,
		ProviderState:    models.JSONB{},
		Payload:          models.JSONB{},
	}
	require.NoError(t, db.Create(&job).Error)

	request := testutil.NewGETRequest(t)
	testutil.SetAuthContext(request, organization.ID, user.ID)
	require.NoError(t, app.GetTenantSupportHealth(request))

	var response TenantSupportHealthResponse
	testutil.ParseEnvelopeResponse(t, request, &response)
	for index := range response.Checks {
		if response.Checks[index].Key == "channels" {
			assert.Equal(t, "warn", response.Checks[index].Status)
			assert.Contains(t, response.Checks[index].Detail, "outbound job(s) are retrying")
			assert.NotContains(t, response.Checks[index].Detail, "no tested Instagram")
			return
		}
	}
	t.Fatal("channels health check not returned")
}

func TestTenantSupportHealthCountsActiveNonLegacyWhatsAppChannel(t *testing.T) {
	db := testutil.SetupTestDB(t)
	organization := testutil.CreateTestOrganization(t, db)
	user := testutil.CreateTestUser(t, db, organization.ID, testutil.WithSuperAdmin())
	app := &App{DB: db, Log: testutil.NopLogger()}
	now := time.Now().UTC()
	account := models.ChannelAccount{
		BaseModel:         models.BaseModel{ID: uuid.New()},
		OrganizationID:    organization.ID,
		Channel:           models.ChannelWhatsApp,
		Provider:          "relay",
		Name:              "Direct WhatsApp",
		ExternalAccountID: "direct-whatsapp",
		Status:            models.ChannelAccountStatusActive,
		Capabilities:      models.JSONB{},
		Config:            models.JSONB{"outbound_enabled": true},
		Metadata:          models.JSONB{},
		LastHealthCheckAt: &now,
	}
	require.NoError(t, db.Create(&account).Error)

	request := testutil.NewGETRequest(t)
	testutil.SetAuthContext(request, organization.ID, user.ID)
	require.NoError(t, app.GetTenantSupportHealth(request))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(request))

	var response TenantSupportHealthResponse
	testutil.ParseEnvelopeResponse(t, request, &response)
	for index := range response.Checks {
		if response.Checks[index].Key == "channels" {
			assert.Equal(t, "pass", response.Checks[index].Status)
			assert.NotContains(t, response.Checks[index].Detail, "No active")
			return
		}
	}
	t.Fatal("channels health check not returned")
}

func TestTenantSupportHealthExplainsEmptyIntendedChannelPolicy(t *testing.T) {
	db := testutil.SetupTestDB(t)
	organization := testutil.CreateTestOrganization(t, db)
	user := testutil.CreateTestUser(t, db, organization.ID, testutil.WithSuperAdmin())
	app := &App{DB: db, Log: testutil.NopLogger()}
	require.NoError(t, db.Create(&models.OrganizationOnboarding{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: organization.ID,
		Status:         models.OnboardingStatusInProgress,
		Checklist:      models.JSONB{},
		Input: models.JSONB{
			"business_name":     "Future clinic",
			"timezone":          "Asia/Kuala_Lumpur",
			"intended_channels": []string{},
		},
		Metadata: models.JSONB{},
	}).Error)

	request := testutil.NewGETRequest(t)
	testutil.SetAuthContext(request, organization.ID, user.ID)
	require.NoError(t, app.GetTenantSupportHealth(request))

	var response TenantSupportHealthResponse
	testutil.ParseEnvelopeResponse(t, request, &response)
	for index := range response.Checks {
		if response.Checks[index].Key == "channels" {
			assert.Equal(t, "warn", response.Checks[index].Status)
			assert.Contains(t, response.Checks[index].Detail, "Select at least one intended launch channel")
			return
		}
	}
	t.Fatal("channels health check not returned")
}

func TestTenantSupportHealthNamesMissingIntendedChannelsWithoutActiveAccounts(t *testing.T) {
	db := testutil.SetupTestDB(t)
	organization := testutil.CreateTestOrganization(t, db)
	user := testutil.CreateTestUser(t, db, organization.ID, testutil.WithSuperAdmin())
	app := &App{DB: db, Log: testutil.NopLogger()}
	require.NoError(t, db.Create(&models.OrganizationOnboarding{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: organization.ID,
		Status:         models.OnboardingStatusInProgress,
		Checklist:      models.JSONB{},
		Input: models.JSONB{
			"business_name":     "Future clinic",
			"timezone":          "Asia/Kuala_Lumpur",
			"intended_channels": []string{"instagram", "messenger"},
		},
		Metadata: models.JSONB{},
	}).Error)

	request := testutil.NewGETRequest(t)
	testutil.SetAuthContext(request, organization.ID, user.ID)
	require.NoError(t, app.GetTenantSupportHealth(request))

	var response TenantSupportHealthResponse
	testutil.ParseEnvelopeResponse(t, request, &response)
	for index := range response.Checks {
		if response.Checks[index].Key == "channels" {
			assert.Equal(t, "warn", response.Checks[index].Status)
			assert.Contains(t, response.Checks[index].Detail, "Instagram")
			assert.Contains(t, response.Checks[index].Detail, "Messenger")
			return
		}
	}
	t.Fatal("channels health check not returned")
}

func TestProductCommercialProfileRejectsCredentials(t *testing.T) {
	t.Parallel()

	valid := models.JSONB{
		"vertical": "Clinic",
		"timezone": "Asia/Kuala_Lumpur",
		"business": map[string]any{"name": "ReAlign"},
	}
	require.NoError(t, productCommercialValidateProfile(valid))
	assert.Equal(t, "clinic", valid["vertical"])

	for _, key := range []string{"password", "api_secret", "access_token", "private_key", "credentials"} {
		profile := models.JSONB{
			"vertical": "clinic",
			"nested":   map[string]any{key: "must-not-be-stored"},
		}
		err := productCommercialValidateProfile(profile)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "must not contain credentials or secrets")
	}
}

func TestProductCommercialConsentValidationAndEvidenceRedactionBoundary(t *testing.T) {
	t.Parallel()

	contactID := uuid.New()
	request := RecordConsentRequest{
		ContactID: &contactID,
		Purpose:   "marketing",
		Channel:   "whatsapp",
		Action:    models.ConsentActionGranted,
		Evidence:  models.JSONB{"source_message_id": "wamid.123"},
	}
	evidence, err := productCommercialValidateConsent(&request)
	require.NoError(t, err)
	assert.NotEmpty(t, evidence)
	assert.Equal(t, "contact", request.SubjectType)
	assert.Equal(t, "manual", request.Source)

	request.Evidence = models.JSONB{"access_token": "should-never-be-persisted"}
	_, err = productCommercialValidateConsent(&request)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "must not contain credentials or secrets")
}

func TestRecordConsentWaitsForMergeAndWritesCanonicalIdentity(t *testing.T) {
	db := testutil.SetupTestDB(t)
	organization := testutil.CreateTestOrganization(t, db)
	adminRole := testutil.CreateAdminRole(t, db, organization.ID)
	user := testutil.CreateTestUser(
		t,
		db,
		organization.ID,
		testutil.WithRoleID(&adminRole.ID),
	)
	canonical := testutil.CreateTestContact(t, db, organization.ID)
	alias := testutil.CreateTestContact(t, db, organization.ID)
	app := &App{DB: db, Log: testutil.NopLogger()}

	priorCapturedAt := time.Now().UTC().Add(-time.Hour)
	priorEvent := models.ConsentEvent{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: organization.ID,
		ContactID:      &alias.ID,
		SubjectType:    "contact",
		SubjectKey:     alias.ID.String(),
		Purpose:        string(models.ChannelPreferencePurposeService),
		Channel:        string(models.ChannelWhatsApp),
		Action:         models.ConsentActionGranted,
		Source:         "pre-merge-test",
		ActorUserID:    &user.ID,
		Evidence:       models.JSONB{},
		CapturedAt:     priorCapturedAt,
	}
	require.NoError(t, db.Create(&priorEvent).Error)

	mergeTx := db.Begin()
	require.NoError(t, mergeTx.Error)
	t.Cleanup(func() {
		_ = mergeTx.Rollback().Error
	})
	var locked []models.Contact
	require.NoError(t, mergeTx.Unscoped().
		Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("organization_id = ? AND id IN ?", organization.ID, []uuid.UUID{
			canonical.ID,
			alias.ID,
		}).
		Order("id").
		Find(&locked).Error)
	require.Len(t, locked, 2)
	mergedAt := time.Now().UTC()
	require.NoError(t, mergeTx.Unscoped().Model(&models.Contact{}).
		Where("id = ? AND organization_id = ?", alias.ID, organization.ID).
		Updates(map[string]any{
			"merged_into_id": canonical.ID,
			"merged_at":      mergedAt,
			"merged_by_id":   user.ID,
			"deleted_at":     mergedAt,
		}).Error)

	request := testutil.NewJSONRequest(t, RecordConsentRequest{
		ContactID: &alias.ID,
		Purpose:   string(models.ChannelPreferencePurposeService),
		Channel:   string(models.ChannelWhatsApp),
		Action:    models.ConsentActionDenied,
		Source:    "post-merge-test",
		Evidence:  models.JSONB{"case": "stale-alias"},
	})
	testutil.SetAuthContext(request, organization.ID, user.ID)
	recorded := make(chan error, 1)
	go func() {
		recorded <- app.RecordConsent(request)
	}()

	select {
	case err := <-recorded:
		require.NoError(t, err)
		t.Fatal("RecordConsent completed before the contact merge released its row locks")
	case <-time.After(150 * time.Millisecond):
	}

	require.NoError(t, mergeTx.Commit().Error)
	select {
	case err := <-recorded:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("RecordConsent did not resume after the contact merge committed")
	}
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(request))

	var storedPrior models.ConsentEvent
	require.NoError(t, db.First(&storedPrior, priorEvent.ID).Error)
	require.NotNil(t, storedPrior.ContactID)
	require.Equal(t, alias.ID, *storedPrior.ContactID,
		"immutable consent history must retain its original contact identity")

	var currentEvent models.ConsentEvent
	require.NoError(t, db.Where(
		"organization_id = ? AND source = ?",
		organization.ID,
		"post-merge-test",
	).First(&currentEvent).Error)
	require.NotNil(t, currentEvent.ContactID)
	require.Equal(t, canonical.ID, *currentEvent.ContactID)
	require.Equal(t, canonical.ID.String(), currentEvent.SubjectKey)

	var state models.ConsentState
	require.NoError(t, db.Where(
		"organization_id = ? AND latest_event_id = ?",
		organization.ID,
		currentEvent.ID,
	).First(&state).Error)
	require.NotNil(t, state.ContactID)
	require.Equal(t, canonical.ID, *state.ContactID)
	require.Equal(t, canonical.ID.String(), state.SubjectKey)

	var aliasStateCount int64
	require.NoError(t, db.Model(&models.ConsentState{}).
		Where("organization_id = ? AND contact_id = ?", organization.ID, alias.ID).
		Count(&aliasStateCount).Error)
	require.Zero(t, aliasStateCount)
}

func TestProductCommercialPlanAuditUsesExplicitTenantContext(t *testing.T) {
	db := testutil.SetupTestDB(t)
	auditOrganization := testutil.CreateTestOrganization(t, db)
	owner := testutil.CreateTestUser(
		t,
		db,
		auditOrganization.ID,
		testutil.WithEmail(testutil.UniqueEmail("plan-audit-owner")),
		testutil.WithSuperAdmin(),
	)
	app := &App{
		Config: &config.Config{
			Database: config.DatabaseConfig{RLSEnabled: true},
		},
		DB:  db,
		Log: testutil.NopLogger(),
	}

	callbackName := "test:product-plan-audit-tenant:" + uuid.NewString()
	expectedTenant := auditOrganization.ID.String()
	require.NoError(t, db.Callback().Create().
		Before("gorm:create").
		Register(callbackName, func(tx *gorm.DB) {
			if tx.Statement.Schema == nil || tx.Statement.Schema.Table != "audit_logs" {
				return
			}
			var currentTenant string
			lookup := tx.Session(&gorm.Session{NewDB: true}).
				Raw(
					"SELECT current_setting('app.current_organization_id', true)",
				).
				Scan(&currentTenant)
			if lookup.Error != nil {
				_ = tx.AddError(lookup.Error)
				return
			}
			if currentTenant != expectedTenant {
				_ = tx.AddError(fmt.Errorf(
					"audit tenant context is %q; expected %q",
					currentTenant,
					expectedTenant,
				))
			}
		}))
	t.Cleanup(func() {
		_ = db.Callback().Create().Remove(callbackName)
	})

	planCode := "audit-tenant-" + uuid.NewString()
	createRequest := testutil.NewJSONRequest(t, map[string]any{
		"code":       planCode,
		"name":       "Audit tenant plan",
		"vertical":   "general",
		"status":     models.CommercialPlanStatusActive,
		"trial_days": 14,
		"is_public":  false,
		"prices": []map[string]any{{
			"code":              planCode + "-myr-month",
			"currency":          "MYR",
			"unit_amount_minor": 0,
			"interval":          models.BillingIntervalMonth,
			"interval_count":    1,
		}},
		"entitlements": []map[string]any{{
			"key":         "omnichannel.enabled",
			"value_type":  models.EntitlementValueTypeBoolean,
			"value":       true,
			"enforcement": models.EntitlementEnforcementHard,
		}},
	})
	testutil.SetFullAuthContext(
		createRequest,
		auditOrganization.ID,
		owner.ID,
		owner.RoleID,
		true,
	)
	require.NoError(t, app.CreateProductPlan(createRequest))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(createRequest))

	var created struct {
		Data ProductPlanResponse `json:"data"`
	}
	require.NoError(t, json.Unmarshal(testutil.GetResponseBody(createRequest), &created))
	require.NotNil(t, created.Data.ID)
	planID := *created.Data.ID
	t.Cleanup(func() {
		require.NoError(t, db.Unscoped().
			Where("organization_id = ? AND resource_type = ? AND resource_id = ?",
				auditOrganization.ID,
				productPlanAuditResource,
				planID,
			).
			Delete(&models.AuditLog{}).Error)
		require.NoError(t, db.Unscoped().
			Where("plan_id = ?", planID).
			Delete(&models.PlanEntitlement{}).Error)
		require.NoError(t, db.Unscoped().
			Where("plan_id = ?", planID).
			Delete(&models.PlanPrice{}).Error)
		require.NoError(t, db.Unscoped().
			Where("id = ?", planID).
			Delete(&models.Plan{}).Error)
	})

	updatedName := "Audit tenant plan updated"
	updateRequest := testutil.NewJSONRequest(t, map[string]any{
		"name": updatedName,
	})
	testutil.SetFullAuthContext(
		updateRequest,
		auditOrganization.ID,
		owner.ID,
		owner.RoleID,
		true,
	)
	testutil.SetPathParam(updateRequest, "id", planID.String())
	require.NoError(t, app.UpdateProductPlan(updateRequest))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(updateRequest))

	var auditCount int64
	require.NoError(t, db.Model(&models.AuditLog{}).
		Where(
			"organization_id = ? AND resource_type = ? AND resource_id = ?",
			auditOrganization.ID,
			productPlanAuditResource,
			planID,
		).
		Count(&auditCount).Error)
	require.EqualValues(t, 2, auditCount)
}

func TestProductCommercialWorkflowTransitions(t *testing.T) {
	t.Parallel()

	assert.True(t, productCommercialPrivacyTransitionAllowed(
		models.PrivacyRequestStatusAwaitingVerification,
		models.PrivacyRequestStatusVerified,
	))
	assert.True(t, productCommercialPrivacyTransitionAllowed(
		models.PrivacyRequestStatusInProgress,
		models.PrivacyRequestStatusCompleted,
	))
	assert.False(t, productCommercialPrivacyTransitionAllowed(
		models.PrivacyRequestStatusCompleted,
		models.PrivacyRequestStatusInProgress,
	))

	assert.True(t, productCommercialSupportTransitionAllowed(
		models.SupportCaseStatusOpen,
		models.SupportCaseStatusInvestigating,
	))
	assert.True(t, productCommercialSupportTransitionAllowed(
		models.SupportCaseStatusResolved,
		models.SupportCaseStatusOpen,
	))
	assert.False(t, productCommercialSupportTransitionAllowed(
		models.SupportCaseStatusClosed,
		models.SupportCaseStatusOpen,
	))

	assert.False(t, productCommercialPrivacyCompletionHasEvidence("", nil))
	blank := "  "
	assert.False(t, productCommercialPrivacyCompletionHasEvidence("prior evidence", &blank))
	evidence := "Export archive delivered through the approved secure channel."
	assert.True(t, productCommercialPrivacyCompletionHasEvidence("", &evidence))
	assert.True(t, productCommercialPrivacyCompletionHasEvidence("Existing fulfillment record", nil))
}

func TestProductCommercialOnboardingStepPolicy(t *testing.T) {
	t.Parallel()

	assert.True(t, productCommercialManualOnboardingStep("workspace_profile"))
	assert.True(t, productCommercialManualOnboardingStep("go_live"))
	assert.False(t, productCommercialManualOnboardingStep("license_assigned"))
	assert.False(t, productCommercialManualOnboardingStep("channel_connected"))
	assert.False(t, productCommercialManualOnboardingStep("privacy_baseline"))
	assert.False(t, productCommercialManualOnboardingStep("made_up"))
}

func TestProductCommercialProfileCompletionRequiresIdentityAndTimezone(t *testing.T) {
	t.Parallel()

	assert.False(t, productCommercialProfileComplete(models.JSONB{"vertical": "clinic"}))
	assert.False(t, productCommercialProfileComplete(models.JSONB{
		"business": map[string]any{"name": "ReAlign"},
		"timezone": "Not/A_Timezone",
	}))
	assert.True(t, productCommercialProfileComplete(models.JSONB{
		"business": map[string]any{"name": "ReAlign"},
		"timezone": "Asia/Kuala_Lumpur",
	}))
	assert.False(t, productCommercialProfileComplete(models.JSONB{
		"business":          map[string]any{"name": "New clinic"},
		"timezone":          "Asia/Kuala_Lumpur",
		"intended_channels": []string{},
	}))
	assert.True(t, productCommercialProfileComplete(models.JSONB{
		"business":          map[string]any{"name": "New clinic"},
		"timezone":          "Asia/Kuala_Lumpur",
		"intended_channels": []any{"messenger", "whatsapp"},
	}))
}

func TestProductCommercialIntendedChannelsAreValidatedAndNormalized(t *testing.T) {
	t.Parallel()

	profile := models.JSONB{
		"intended_channels": []any{" Messenger ", "whatsapp", "messenger"},
	}
	require.NoError(t, productCommercialValidateProfile(profile))
	assert.Equal(t, []string{"whatsapp", "messenger"}, profile["intended_channels"])

	assert.Error(t, productCommercialValidateProfile(models.JSONB{
		"intended_channels": []any{},
	}))
	assert.Error(t, productCommercialValidateProfile(models.JSONB{
		"intended_channels": []any{"facebook"},
	}))
}

func TestProductCommercialOnboardingRequiresLicenseAndEveryIntendedChannel(t *testing.T) {
	db := testutil.SetupTestDB(t)
	organization := testutil.CreateTestOrganization(t, db)
	user := testutil.CreateTestUser(t, db, organization.ID, testutil.WithSuperAdmin())
	onboarding := models.OrganizationOnboarding{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: organization.ID,
		Status:         models.OnboardingStatusInProgress,
		Checklist:      models.JSONB{},
		Input: models.JSONB{
			"business_name":     "Future clinic",
			"timezone":          "Asia/Kuala_Lumpur",
			"intended_channels": []string{"whatsapp", "instagram"},
		},
		Metadata: models.JSONB{},
	}
	require.NoError(t, db.Create(&onboarding).Error)
	testutil.CreateTestWhatsAppAccount(t, db, organization.ID)

	signals, err := productCommercialOnboardingSignals(db, organization.ID, &onboarding)
	require.NoError(t, err)
	assert.False(t, signals["license_assigned"])
	assert.False(t, signals["channel_connected"])

	enableBookingCommerceTestEntitlement(
		t,
		db,
		organization.ID,
		user.ID,
		"omnichannel.enabled",
	)
	instagram := models.ChannelAccount{
		BaseModel:         models.BaseModel{ID: uuid.New()},
		OrganizationID:    organization.ID,
		Channel:           models.ChannelInstagram,
		Provider:          "relay",
		Name:              "Future clinic Instagram",
		ExternalAccountID: "future-clinic-instagram",
		Status:            models.ChannelAccountStatusPending,
		Capabilities:      models.JSONB{},
		Config:            models.JSONB{"outbound_enabled": false},
		Metadata:          models.JSONB{},
	}
	require.NoError(t, db.Create(&instagram).Error)

	signals, err = productCommercialOnboardingSignals(db, organization.ID, &onboarding)
	require.NoError(t, err)
	assert.True(t, signals["license_assigned"])
	assert.False(t, signals["channel_connected"])

	now := time.Now().UTC()
	instagram.Status = models.ChannelAccountStatusActive
	instagram.Config["outbound_enabled"] = true
	instagram.LastHealthCheckAt = &now
	require.NoError(t, db.Save(&instagram).Error)

	signals, err = productCommercialOnboardingSignals(db, organization.ID, &onboarding)
	require.NoError(t, err)
	assert.True(t, signals["channel_connected"])
}

func TestProductCommercialOnboardingRequiresThreadsEntitlement(t *testing.T) {
	db := testutil.SetupTestDB(t)
	organization := testutil.CreateTestOrganization(t, db)
	user := testutil.CreateTestUser(t, db, organization.ID, testutil.WithSuperAdmin())
	onboarding := models.OrganizationOnboarding{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: organization.ID,
		Status:         models.OnboardingStatusInProgress,
		Checklist:      models.JSONB{},
		Input: models.JSONB{
			"business_name":     "Threads clinic",
			"timezone":          "Asia/Kuala_Lumpur",
			"intended_channels": []string{"threads"},
		},
		Metadata: models.JSONB{},
	}
	require.NoError(t, db.Create(&onboarding).Error)
	enableBookingCommerceTestEntitlement(
		t,
		db,
		organization.ID,
		user.ID,
		"omnichannel.enabled",
	)

	signals, err := productCommercialOnboardingSignals(db, organization.ID, &onboarding)
	require.NoError(t, err)
	assert.False(t, signals["license_assigned"], "omnichannel alone must not license Threads")

	var subscription models.Subscription
	require.NoError(t, productCommercialLoadCurrentSubscription(db, organization.ID, &subscription))
	subscription.EntitlementsSnapshot["threads.public_engagement.enabled"] = true
	require.NoError(t, db.Model(&subscription).Update(
		"entitlements_snapshot",
		subscription.EntitlementsSnapshot,
	).Error)

	signals, err = productCommercialOnboardingSignals(db, organization.ID, &onboarding)
	require.NoError(t, err)
	assert.True(t, signals["license_assigned"])
}

func TestProductCommercialHealthAggregation(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "healthy", productCommercialHealthStatus([]TenantHealthCheckResponse{
		{Status: "pass"},
		{Status: "pass"},
	}))
	assert.Equal(t, "attention", productCommercialHealthStatus([]TenantHealthCheckResponse{
		{Status: "pass"},
		{Status: "warn"},
	}))
	assert.Equal(t, "degraded", productCommercialHealthStatus([]TenantHealthCheckResponse{
		{Status: "not_configured"},
		{Status: "fail"},
	}))
}
