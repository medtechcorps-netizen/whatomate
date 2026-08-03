package handlers_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	channelapi "github.com/shridarpatil/whatomate/internal/channel"
	"github.com/shridarpatil/whatomate/internal/handlers"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
	"github.com/zerodha/fastglue"
)

func createThreadsSupportSubscription(
	t *testing.T,
	app *handlers.App,
	organization *models.Organization,
	plan *models.Plan,
	status models.SubscriptionStatus,
	expiresAt time.Time,
	omnichannelEnabled bool,
) models.Subscription {
	t.Helper()
	billingAccount := models.BillingAccount{
		BaseModel:       models.BaseModel{ID: uuid.New()},
		OrganizationID:  organization.ID,
		ResellerID:      organization.ResellerID,
		Provider:        models.BillingProviderManual,
		Status:          models.BillingAccountStatusActive,
		DefaultCurrency: "MYR",
		BillingProfile:  models.JSONB{},
		ProviderData:    models.JSONB{},
		Metadata:        models.JSONB{},
	}
	require.NoError(t, app.DB.Create(&billingAccount).Error)

	now := time.Now().UTC()
	subscription := models.Subscription{
		BaseModel:        models.BaseModel{ID: uuid.New()},
		OrganizationID:   organization.ID,
		BillingAccountID: billingAccount.ID,
		PlanID:           plan.ID,
		Provider:         models.BillingProviderManual,
		Status:           status,
		Quantity:         1,
		CollectionMethod: "manual",
		EntitlementsSnapshot: models.JSONB{
			channelapi.OmnichannelEntitlementKey:             omnichannelEnabled,
			channelapi.ThreadsPublicEngagementEntitlementKey: false,
		},
		ProviderData:       models.JSONB{},
		CurrentPeriodStart: &now,
		CurrentPeriodEnd:   &expiresAt,
	}
	if status == models.SubscriptionStatusTrialing {
		subscription.TrialEndsAt = &expiresAt
	}
	require.NoError(t, app.DB.Create(&subscription).Error)
	return subscription
}

func newThreadsSupportRequest(
	t *testing.T,
	controlOrgID uuid.UUID,
	owner *models.User,
	targetOrgID uuid.UUID,
	payload map[string]any,
) *fastglue.Request {
	t.Helper()
	request := testutil.NewJSONRequest(t, payload)
	testutil.SetFullAuthContext(
		request,
		controlOrgID,
		owner.ID,
		owner.RoleID,
		owner.IsSuperAdmin,
	)
	testutil.SetPathParam(request, "organization_id", targetOrgID.String())
	return request
}

func TestEnableOrganizationThreadsPublicEngagementAuthorizationAndRequestGuards(t *testing.T) {
	app := newTestApp(t)
	controlOrg := testutil.CreateTestOrganization(t, app.DB)
	owner := testutil.CreateTestUser(
		t,
		app.DB,
		controlOrg.ID,
		testutil.WithEmail(testutil.UniqueEmail("threads-support-owner")),
		testutil.WithSuperAdmin(),
	)
	nonOwner := testutil.CreateTestUser(
		t,
		app.DB,
		controlOrg.ID,
		testutil.WithEmail(testutil.UniqueEmail("threads-support-non-owner")),
	)
	targetOrg := testutil.CreateTestOrganization(t, app.DB)

	forbidden := newThreadsSupportRequest(
		t,
		controlOrg.ID,
		nonOwner,
		targetOrg.ID,
		map[string]any{"reason": "support-ticket-1"},
	)
	require.NoError(t, app.EnableOrganizationThreadsPublicEngagement(forbidden))
	testutil.AssertErrorResponse(
		t,
		forbidden,
		fasthttp.StatusForbidden,
		"Platform owner access required",
	)

	for name, payload := range map[string]map[string]any{
		"missing reason": {},
		"blank reason":   {"reason": "   "},
		"arbitrary key": {
			"reason": "support-ticket-1",
			"key":    "crm.enabled",
		},
		"disable value": {
			"reason":  "support-ticket-1",
			"enabled": false,
		},
	} {
		t.Run(name, func(t *testing.T) {
			request := newThreadsSupportRequest(
				t,
				controlOrg.ID,
				owner,
				targetOrg.ID,
				payload,
			)
			require.NoError(t, app.EnableOrganizationThreadsPublicEngagement(request))
			assert.Equal(t, fasthttp.StatusBadRequest, testutil.GetResponseStatusCode(request))
		})
	}

	var overrideCount int64
	require.NoError(t, app.DB.Model(&models.EntitlementOverride{}).
		Where("organization_id = ?", targetOrg.ID).
		Count(&overrideCount).Error)
	assert.Zero(t, overrideCount)
}

func TestEnableOrganizationThreadsPublicEngagementCommercialGuards(t *testing.T) {
	app := newTestApp(t)
	controlOrg := testutil.CreateTestOrganization(t, app.DB)
	owner := testutil.CreateTestUser(
		t,
		app.DB,
		controlOrg.ID,
		testutil.WithEmail(testutil.UniqueEmail("threads-guard-owner")),
		testutil.WithSuperAdmin(),
	)
	now := time.Now().UTC()
	growthPlan := createCatalogPlan(
		t,
		app,
		nil,
		"rereply-growth",
		"ReReply Growth",
		models.CommercialPlanStatusActive,
	)
	starterPlan := createCatalogPlan(
		t,
		app,
		nil,
		"rereply-starter",
		"ReReply Starter",
		models.CommercialPlanStatusActive,
	)

	tests := []struct {
		name        string
		plan        *models.Plan
		status      models.SubscriptionStatus
		expiresAt   time.Time
		omnichannel bool
		wantMessage string
	}{
		{
			name:        "wrong plan",
			plan:        &starterPlan,
			status:      models.SubscriptionStatusActive,
			expiresAt:   now.Add(24 * time.Hour),
			omnichannel: true,
			wantMessage: "An active, unexpired ReReply Growth subscription is required",
		},
		{
			name:        "expired trial",
			plan:        &growthPlan,
			status:      models.SubscriptionStatusTrialing,
			expiresAt:   now.Add(-time.Hour),
			omnichannel: true,
			wantMessage: "An active, unexpired ReReply Growth subscription is required",
		},
		{
			name:        "past due is not eligible",
			plan:        &growthPlan,
			status:      models.SubscriptionStatusPastDue,
			expiresAt:   now.Add(24 * time.Hour),
			omnichannel: true,
			wantMessage: "A current ReReply Growth subscription is required",
		},
		{
			name:        "omnichannel disabled",
			plan:        &growthPlan,
			status:      models.SubscriptionStatusTrialing,
			expiresAt:   now.Add(24 * time.Hour),
			omnichannel: false,
			wantMessage: "Effective omnichannel entitlement must be enabled first",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			targetOrg := testutil.CreateTestOrganization(t, app.DB)
			createThreadsSupportSubscription(
				t,
				app,
				targetOrg,
				tt.plan,
				tt.status,
				tt.expiresAt,
				tt.omnichannel,
			)
			request := newThreadsSupportRequest(
				t,
				controlOrg.ID,
				owner,
				targetOrg.ID,
				map[string]any{"reason": "support-guard-check"},
			)
			require.NoError(t, app.EnableOrganizationThreadsPublicEngagement(request))
			testutil.AssertErrorResponse(
				t,
				request,
				fasthttp.StatusConflict,
				tt.wantMessage,
			)
		})
	}

	t.Run("conflicting active override", func(t *testing.T) {
		targetOrg := testutil.CreateTestOrganization(t, app.DB)
		createThreadsSupportSubscription(
			t,
			app,
			targetOrg,
			&growthPlan,
			models.SubscriptionStatusTrialing,
			now.Add(24*time.Hour),
			true,
		)
		require.NoError(t, app.DB.Create(&models.EntitlementOverride{
			BaseModel:      models.BaseModel{ID: uuid.New()},
			OrganizationID: targetOrg.ID,
			Key:            channelapi.ThreadsPublicEngagementEntitlementKey,
			ValueType:      models.EntitlementValueTypeBoolean,
			Value:          models.JSONB{"value": false},
			Source:         "support",
			Reason:         "existing-conflict",
			IsActive:       true,
			StartsAt:       now.Add(-time.Minute),
			CreatedByID:    owner.ID,
		}).Error)

		request := newThreadsSupportRequest(
			t,
			controlOrg.ID,
			owner,
			targetOrg.ID,
			map[string]any{"reason": "support-guard-check"},
		)
		require.NoError(t, app.EnableOrganizationThreadsPublicEngagement(request))
		testutil.AssertErrorResponse(
			t,
			request,
			fasthttp.StatusConflict,
			"An active Threads public engagement override already exists",
		)
	})

	t.Run("future scheduled active override", func(t *testing.T) {
		targetOrg := testutil.CreateTestOrganization(t, app.DB)
		createThreadsSupportSubscription(
			t,
			app,
			targetOrg,
			&growthPlan,
			models.SubscriptionStatusTrialing,
			now.Add(24*time.Hour),
			true,
		)
		require.NoError(t, app.DB.Create(&models.EntitlementOverride{
			BaseModel:      models.BaseModel{ID: uuid.New()},
			OrganizationID: targetOrg.ID,
			Key:            channelapi.ThreadsPublicEngagementEntitlementKey,
			ValueType:      models.EntitlementValueTypeBoolean,
			Value:          models.JSONB{"value": true},
			Source:         "support",
			Reason:         "scheduled-future-grant",
			IsActive:       true,
			StartsAt:       now.Add(time.Hour),
			CreatedByID:    owner.ID,
		}).Error)

		request := newThreadsSupportRequest(
			t,
			controlOrg.ID,
			owner,
			targetOrg.ID,
			map[string]any{"reason": "support-guard-check"},
		)
		require.NoError(t, app.EnableOrganizationThreadsPublicEngagement(request))
		testutil.AssertErrorResponse(
			t,
			request,
			fasthttp.StatusConflict,
			"An active Threads public engagement override already exists",
		)

		var overrideCount int64
		require.NoError(t, app.DB.Model(&models.EntitlementOverride{}).
			Where(
				"organization_id = ? AND key = ?",
				targetOrg.ID,
				channelapi.ThreadsPublicEngagementEntitlementKey,
			).
			Count(&overrideCount).Error)
		assert.EqualValues(t, 1, overrideCount)
	})
}

func TestEnableOrganizationThreadsPublicEngagementSuccessIsTargetedAuditedAndIdempotent(
	t *testing.T,
) {
	app := newTestApp(t)
	controlOrg := testutil.CreateTestOrganization(t, app.DB)
	owner := testutil.CreateTestUser(
		t,
		app.DB,
		controlOrg.ID,
		testutil.WithEmail(testutil.UniqueEmail("threads-success-owner")),
		testutil.WithSuperAdmin(),
	)
	targetOrg := testutil.CreateTestOrganization(t, app.DB)
	otherOrg := testutil.CreateTestOrganization(t, app.DB)
	growthPlan := createCatalogPlan(
		t,
		app,
		nil,
		"rereply-growth",
		"ReReply Growth",
		models.CommercialPlanStatusActive,
	)
	createThreadsSupportSubscription(
		t,
		app,
		targetOrg,
		&growthPlan,
		models.SubscriptionStatusTrialing,
		time.Now().UTC().Add(14*24*time.Hour),
		true,
	)

	call := func() handlers.EnableThreadsPublicEngagementResponse {
		t.Helper()
		request := newThreadsSupportRequest(
			t,
			controlOrg.ID,
			owner,
			targetOrg.ID,
			map[string]any{"reason": "  support-ticket-threads-001  "},
		)
		require.NoError(t, app.EnableOrganizationThreadsPublicEngagement(request))
		require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(request))
		var envelope struct {
			Data handlers.EnableThreadsPublicEngagementResponse `json:"data"`
		}
		require.NoError(t, json.Unmarshal(testutil.GetResponseBody(request), &envelope))
		return envelope.Data
	}

	first := call()
	assert.True(t, first.Created)
	assert.True(t, first.EffectiveEnabled)
	assert.Equal(t, targetOrg.ID, first.OrganizationID)
	assert.Equal(t, channelapi.ThreadsPublicEngagementEntitlementKey, first.EntitlementKey)
	assert.Equal(t, models.SubscriptionStatusTrialing, first.SubscriptionStatus)
	assert.Equal(t, "rereply-growth", first.PlanCode)

	decision, err := app.EvaluateProductEntitlement(
		owner.ID,
		targetOrg.ID,
		channelapi.ThreadsPublicEngagementEntitlementKey,
	)
	require.NoError(t, err)
	assert.True(t, decision.Allowed)
	assert.True(t, decision.Overridden)

	var override models.EntitlementOverride
	require.NoError(t, app.DB.Where(
		"organization_id = ? AND key = ?",
		targetOrg.ID,
		channelapi.ThreadsPublicEngagementEntitlementKey,
	).First(&override).Error)
	assert.Equal(t, first.OverrideID, override.ID)
	assert.Equal(t, owner.ID, override.CreatedByID)
	assert.Equal(t, "support", override.Source)
	assert.Equal(t, "support-ticket-threads-001", override.Reason)
	assert.True(t, override.IsActive)
	assert.Equal(t, true, override.Value["value"])

	var otherOverrideCount int64
	require.NoError(t, app.DB.Model(&models.EntitlementOverride{}).
		Where("organization_id = ?", otherOrg.ID).
		Count(&otherOverrideCount).Error)
	assert.Zero(t, otherOverrideCount)

	var auditCount int64
	require.NoError(t, app.DB.Model(&models.AuditLog{}).
		Where(
			"organization_id = ? AND resource_type = ? AND resource_id = ? AND user_id = ? AND action = ?",
			targetOrg.ID,
			"entitlement_override",
			override.ID,
			owner.ID,
			models.AuditActionCreated,
		).
		Count(&auditCount).Error)
	assert.EqualValues(t, 1, auditCount)

	second := call()
	assert.False(t, second.Created)
	assert.True(t, second.EffectiveEnabled)
	assert.Equal(t, first.OverrideID, second.OverrideID)

	var overrideCount int64
	require.NoError(t, app.DB.Model(&models.EntitlementOverride{}).
		Where(
			"organization_id = ? AND key = ?",
			targetOrg.ID,
			channelapi.ThreadsPublicEngagementEntitlementKey,
		).
		Count(&overrideCount).Error)
	assert.EqualValues(t, 1, overrideCount)
	require.NoError(t, app.DB.Model(&models.AuditLog{}).
		Where(
			"organization_id = ? AND resource_type = ? AND resource_id = ?",
			targetOrg.ID,
			"entitlement_override",
			override.ID,
		).
		Count(&auditCount).Error)
	assert.EqualValues(t, 1, auditCount)
}
