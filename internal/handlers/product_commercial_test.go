package handlers

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
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
		PlanID:      &planID,
		PlanPriceID: &priceID,
		Status:      models.SubscriptionStatusActive,
	}
	require.NoError(t, productCommercialValidateSetSubscription(&valid))

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
