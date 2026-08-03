package handlers_test

import (
	"encoding/json"
	"fmt"
	"strings"
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
	"gorm.io/gorm"
)

func createThreadsSupportOverride(
	t *testing.T,
	app *handlers.App,
	organizationID, creatorID uuid.UUID,
	source string,
	enabled, active bool,
	startsAt time.Time,
	reason string,
) models.EntitlementOverride {
	t.Helper()
	override := models.EntitlementOverride{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: organizationID,
		Key:            channelapi.ThreadsPublicEngagementEntitlementKey,
		ValueType:      models.EntitlementValueTypeBoolean,
		Value:          models.JSONB{"value": enabled},
		Source:         source,
		Reason:         reason,
		IsActive:       active,
		StartsAt:       startsAt,
		CreatedByID:    creatorID,
	}
	require.NoError(t, app.DB.Create(&override).Error)
	return override
}

func callThreadsSupportRevoke(
	t *testing.T,
	app *handlers.App,
	selectedOrgID uuid.UUID,
	owner *models.User,
	targetOrgID uuid.UUID,
	payload map[string]any,
) *fastglue.Request {
	t.Helper()
	request := newThreadsSupportRequest(
		t,
		selectedOrgID,
		owner,
		targetOrgID,
		payload,
	)
	require.NoError(t, app.RevokeOrganizationThreadsPublicEngagementSupport(request))
	return request
}

func callThreadsSupportStatus(
	t *testing.T,
	app *handlers.App,
	selectedOrgID uuid.UUID,
	owner *models.User,
	targetOrgID uuid.UUID,
) *fastglue.Request {
	t.Helper()
	request := testutil.NewGETRequest(t)
	testutil.SetFullAuthContext(
		request,
		owner.OrganizationID,
		owner.ID,
		owner.RoleID,
		owner.IsSuperAdmin,
	)
	if selectedOrgID != owner.OrganizationID {
		testutil.SetHeader(request, "X-Organization-ID", selectedOrgID.String())
	}
	testutil.SetPathParam(request, "organization_id", targetOrgID.String())
	require.NoError(t, app.GetOrganizationThreadsPublicEngagementSupportStatus(request))
	return request
}

func decodeThreadsSupportRevokeResponse(
	t *testing.T,
	request *fastglue.Request,
) handlers.RevokeThreadsPublicEngagementSupportResponse {
	t.Helper()
	var envelope struct {
		Data handlers.RevokeThreadsPublicEngagementSupportResponse `json:"data"`
	}
	require.NoError(t, json.Unmarshal(testutil.GetResponseBody(request), &envelope))
	return envelope.Data
}

func decodeThreadsSupportStatusResponse(
	t *testing.T,
	request *fastglue.Request,
) handlers.ThreadsPublicEngagementSupportStatusResponse {
	t.Helper()
	var envelope struct {
		Data handlers.ThreadsPublicEngagementSupportStatusResponse `json:"data"`
	}
	require.NoError(t, json.Unmarshal(testutil.GetResponseBody(request), &envelope))
	return envelope.Data
}

func TestRevokeOrganizationThreadsPublicEngagementSupportAuthorizationAndRequestGuards(
	t *testing.T,
) {
	app := newTestApp(t)
	controlOrg := testutil.CreateTestOrganization(t, app.DB)
	targetOrg := testutil.CreateTestOrganization(t, app.DB)
	owner := testutil.CreateTestUser(
		t,
		app.DB,
		controlOrg.ID,
		testutil.WithEmail(testutil.UniqueEmail("threads-revoke-owner")),
		testutil.WithSuperAdmin(),
	)
	nonOwner := testutil.CreateTestUser(
		t,
		app.DB,
		controlOrg.ID,
		testutil.WithEmail(testutil.UniqueEmail("threads-revoke-non-owner")),
	)

	unauthorized := testutil.NewJSONRequest(t, map[string]any{
		"override_id": uuid.New(),
		"reason":      "support-ticket-2",
	})
	testutil.SetPathParam(unauthorized, "organization_id", targetOrg.ID.String())
	require.NoError(t, app.RevokeOrganizationThreadsPublicEngagementSupport(unauthorized))
	testutil.AssertErrorResponse(t, unauthorized, fasthttp.StatusUnauthorized, "Unauthorized")

	unauthorizedStatus := testutil.NewGETRequest(t)
	testutil.SetPathParam(unauthorizedStatus, "organization_id", targetOrg.ID.String())
	require.NoError(t, app.GetOrganizationThreadsPublicEngagementSupportStatus(unauthorizedStatus))
	testutil.AssertErrorResponse(
		t,
		unauthorizedStatus,
		fasthttp.StatusUnauthorized,
		"Unauthorized",
	)

	forbidden := callThreadsSupportRevoke(
		t,
		app,
		targetOrg.ID,
		nonOwner,
		targetOrg.ID,
		map[string]any{"override_id": uuid.New(), "reason": "support-ticket-2"},
	)
	testutil.AssertErrorResponse(
		t,
		forbidden,
		fasthttp.StatusForbidden,
		"Platform owner access required",
	)
	forbiddenStatus := callThreadsSupportStatus(
		t,
		app,
		targetOrg.ID,
		nonOwner,
		targetOrg.ID,
	)
	testutil.AssertErrorResponse(
		t,
		forbiddenStatus,
		fasthttp.StatusForbidden,
		"Platform owner access required",
	)

	mismatchedTarget := callThreadsSupportRevoke(
		t,
		app,
		controlOrg.ID,
		owner,
		targetOrg.ID,
		map[string]any{"override_id": uuid.New(), "reason": "support-ticket-2"},
	)
	testutil.AssertErrorResponse(
		t,
		mismatchedTarget,
		fasthttp.StatusConflict,
		"Selected organization does not match target organization",
	)
	mismatchedStatus := callThreadsSupportStatus(
		t,
		app,
		controlOrg.ID,
		owner,
		targetOrg.ID,
	)
	testutil.AssertErrorResponse(
		t,
		mismatchedStatus,
		fasthttp.StatusConflict,
		"Selected organization does not match target organization",
	)

	for name, payload := range map[string]map[string]any{
		"missing override ID": {"reason": "support-ticket-2"},
		"nil override ID": {
			"override_id": uuid.Nil,
			"reason":      "support-ticket-2",
		},
		"malformed override ID": {
			"override_id": "not-a-uuid",
			"reason":      "support-ticket-2",
		},
		"missing reason": {"override_id": uuid.New()},
		"blank reason":   {"override_id": uuid.New(), "reason": "  "},
		"reason too long": {
			"override_id": uuid.New(),
			"reason":      strings.Repeat("x", 2001),
		},
		"unknown field": {
			"override_id": uuid.New(),
			"reason":      "support-ticket-2",
			"enabled":     false,
		},
	} {
		t.Run(name, func(t *testing.T) {
			request := callThreadsSupportRevoke(
				t,
				app,
				targetOrg.ID,
				owner,
				targetOrg.ID,
				payload,
			)
			assert.Equal(t, fasthttp.StatusBadRequest, testutil.GetResponseStatusCode(request))
		})
	}

	missingTarget := callThreadsSupportRevoke(
		t,
		app,
		targetOrg.ID,
		owner,
		targetOrg.ID,
		map[string]any{
			"override_id": uuid.New(),
			"reason":      "support-ticket-2",
		},
	)
	testutil.AssertErrorResponse(
		t,
		missingTarget,
		fasthttp.StatusNotFound,
		"Support Threads public engagement override not found",
	)

	var overrideCount int64
	require.NoError(t, app.DB.Model(&models.EntitlementOverride{}).
		Where("organization_id = ?", targetOrg.ID).
		Count(&overrideCount).Error)
	assert.Zero(t, overrideCount)
}

func TestRevokeOrganizationThreadsPublicEngagementSupportConflictsAreFailClosed(
	t *testing.T,
) {
	app := newTestApp(t)
	controlOrg := testutil.CreateTestOrganization(t, app.DB)
	owner := testutil.CreateTestUser(
		t,
		app.DB,
		controlOrg.ID,
		testutil.WithEmail(testutil.UniqueEmail("threads-revoke-conflict-owner")),
		testutil.WithSuperAdmin(),
	)
	now := time.Now().UTC()

	tests := []struct {
		name      string
		overrides []struct {
			source   string
			enabled  bool
			startsAt time.Time
		}
	}{
		{
			name: "different source",
			overrides: []struct {
				source   string
				enabled  bool
				startsAt time.Time
			}{{source: "manual", enabled: true, startsAt: now.Add(-time.Minute)}},
		},
		{
			name: "future support grant",
			overrides: []struct {
				source   string
				enabled  bool
				startsAt time.Time
			}{{source: "support", enabled: true, startsAt: now.Add(time.Hour)}},
		},
		{
			name: "support restriction",
			overrides: []struct {
				source   string
				enabled  bool
				startsAt time.Time
			}{{source: "support", enabled: false, startsAt: now.Add(-time.Minute)}},
		},
		{
			name: "second active override",
			overrides: []struct {
				source   string
				enabled  bool
				startsAt time.Time
			}{
				{source: "support", enabled: true, startsAt: now.Add(-time.Minute)},
				{source: "support", enabled: true, startsAt: now.Add(time.Hour)},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			targetOrg := testutil.CreateTestOrganization(t, app.DB)
			var requestedOverrideID uuid.UUID
			for i, candidate := range tt.overrides {
				override := createThreadsSupportOverride(
					t,
					app,
					targetOrg.ID,
					owner.ID,
					candidate.source,
					candidate.enabled,
					true,
					candidate.startsAt,
					fmt.Sprintf("existing-%d", i),
				)
				if i == 0 {
					requestedOverrideID = override.ID
				}
			}

			statusRequest := callThreadsSupportStatus(
				t,
				app,
				targetOrg.ID,
				owner,
				targetOrg.ID,
			)
			testutil.AssertErrorResponse(
				t,
				statusRequest,
				fasthttp.StatusConflict,
				"The active Threads public engagement override is not a revocable support grant",
			)

			request := callThreadsSupportRevoke(
				t,
				app,
				targetOrg.ID,
				owner,
				targetOrg.ID,
				map[string]any{
					"override_id": requestedOverrideID,
					"reason":      "support-ticket-conflict",
				},
			)
			testutil.AssertErrorResponse(
				t,
				request,
				fasthttp.StatusConflict,
				"The active Threads public engagement override is not a revocable support grant",
			)

			var activeCount int64
			require.NoError(t, app.DB.Model(&models.EntitlementOverride{}).
				Where("organization_id = ? AND is_active = ?", targetOrg.ID, true).
				Count(&activeCount).Error)
			assert.EqualValues(t, len(tt.overrides), activeCount)
		})
	}
}

func TestRevokeOrganizationThreadsPublicEngagementSupportIsTargetedRLSAuditedAndIdempotent(
	t *testing.T,
) {
	app := newTestApp(t)
	app.Config.Database.RLSEnabled = true
	controlOrg := testutil.CreateTestOrganization(t, app.DB)
	targetOrg := testutil.CreateTestOrganization(t, app.DB)
	otherOrg := testutil.CreateTestOrganization(t, app.DB)
	owner := testutil.CreateTestUser(
		t,
		app.DB,
		controlOrg.ID,
		testutil.WithEmail(testutil.UniqueEmail("threads-revoke-success-owner")),
		testutil.WithSuperAdmin(),
	)
	growthPlan := ensureThreadsSupportCatalogPlan(
		t,
		app,
		"rereply-growth",
		"ReReply Growth",
	)
	subscription := createThreadsSupportSubscription(
		t,
		app,
		targetOrg,
		&growthPlan,
		models.SubscriptionStatusActive,
		time.Now().UTC().Add(30*24*time.Hour),
		true,
	)
	subscription.EntitlementsSnapshot[channelapi.ThreadsPublicEngagementEntitlementKey] = true
	require.NoError(t, app.DB.Model(&models.Subscription{}).
		Where("id = ?", subscription.ID).
		Update("entitlements_snapshot", subscription.EntitlementsSnapshot).Error)

	targetOverride := createThreadsSupportOverride(
		t,
		app,
		targetOrg.ID,
		owner.ID,
		"support",
		true,
		true,
		time.Now().UTC().Add(-time.Minute),
		"original-support-ticket",
	)
	otherOverride := createThreadsSupportOverride(
		t,
		app,
		otherOrg.ID,
		owner.ID,
		"support",
		true,
		true,
		time.Now().UTC().Add(-time.Minute),
		"other-org-support-ticket",
	)

	callbackName := "test:threads-support-revoke-tenant:" + uuid.NewString()
	expectedTenant := targetOrg.ID.String()
	assertTenantContext := func(tx *gorm.DB) {
		if tx.Statement.Schema == nil {
			return
		}
		table := tx.Statement.Schema.Table
		if table != "entitlement_overrides" && table != "audit_logs" {
			return
		}
		var currentTenant string
		lookup := tx.Session(&gorm.Session{NewDB: true}).
			Raw("SELECT current_setting('app.current_organization_id', true)").
			Scan(&currentTenant)
		if lookup.Error != nil {
			_ = tx.AddError(lookup.Error)
			return
		}
		if currentTenant != expectedTenant {
			_ = tx.AddError(fmt.Errorf(
				"mutation tenant context is %q; expected %q",
				currentTenant,
				expectedTenant,
			))
		}
	}
	require.NoError(t, app.DB.Callback().Update().
		Before("gorm:update").
		Register(callbackName+":update", assertTenantContext))
	require.NoError(t, app.DB.Callback().Create().
		Before("gorm:create").
		Register(callbackName+":create", assertTenantContext))
	t.Cleanup(func() {
		_ = app.DB.Callback().Update().Remove(callbackName + ":update")
		_ = app.DB.Callback().Create().Remove(callbackName + ":create")
	})

	call := func(reason string) handlers.RevokeThreadsPublicEngagementSupportResponse {
		t.Helper()
		request := callThreadsSupportRevoke(
			t,
			app,
			targetOrg.ID,
			owner,
			targetOrg.ID,
			map[string]any{
				"override_id": targetOverride.ID,
				"reason":      reason,
			},
		)
		require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(request))
		return decodeThreadsSupportRevokeResponse(t, request)
	}

	activeStatusRequest := callThreadsSupportStatus(
		t,
		app,
		targetOrg.ID,
		owner,
		targetOrg.ID,
	)
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(activeStatusRequest))
	activeStatus := decodeThreadsSupportStatusResponse(t, activeStatusRequest)
	assert.True(t, activeStatus.Active)
	require.NotNil(t, activeStatus.OverrideID)
	assert.Equal(t, targetOverride.ID, *activeStatus.OverrideID)
	assert.Equal(t, "support", activeStatus.Source)
	require.NotNil(t, activeStatus.StartsAt)

	first := call("  approved support revocation  ")
	assert.Equal(t, targetOrg.ID, first.OrganizationID)
	assert.Equal(t, channelapi.ThreadsPublicEngagementEntitlementKey, first.EntitlementKey)
	assert.Equal(t, targetOverride.ID, first.OverrideID)
	assert.Equal(t, "support", first.Source)
	assert.True(t, first.Revoked)
	assert.True(t, first.EffectiveEnabled, "the plan snapshot remains enabled")
	assert.False(t, first.RevokedAt.IsZero())

	inactiveStatusRequest := callThreadsSupportStatus(
		t,
		app,
		targetOrg.ID,
		owner,
		targetOrg.ID,
	)
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(inactiveStatusRequest))
	inactiveStatus := decodeThreadsSupportStatusResponse(t, inactiveStatusRequest)
	assert.False(t, inactiveStatus.Active)
	assert.Nil(t, inactiveStatus.OverrideID)
	assert.Empty(t, inactiveStatus.Source)
	assert.Nil(t, inactiveStatus.StartsAt)

	var stored models.EntitlementOverride
	require.NoError(t, app.DB.First(&stored, "id = ?", targetOverride.ID).Error)
	assert.False(t, stored.IsActive)
	assert.Equal(t, "original-support-ticket", stored.Reason)
	assert.Equal(t, "approved support revocation", stored.RevocationReason)
	require.NotNil(t, stored.RevokedByID)
	assert.Equal(t, owner.ID, *stored.RevokedByID)
	require.NotNil(t, stored.RevokedAt)

	var untouched models.EntitlementOverride
	require.NoError(t, app.DB.First(&untouched, "id = ?", otherOverride.ID).Error)
	assert.True(t, untouched.IsActive)
	assert.Nil(t, untouched.RevokedAt)
	assert.Empty(t, untouched.RevocationReason)

	decision, err := app.EvaluateProductEntitlement(
		owner.ID,
		targetOrg.ID,
		channelapi.ThreadsPublicEngagementEntitlementKey,
	)
	require.NoError(t, err)
	assert.True(t, decision.Allowed)
	assert.False(t, decision.Overridden)

	var auditEntry models.AuditLog
	require.NoError(t, app.DB.Where(
		"organization_id = ? AND resource_type = ? AND resource_id = ? AND user_id = ? AND action = ?",
		targetOrg.ID,
		"entitlement_override",
		targetOverride.ID,
		owner.ID,
		models.AuditActionUpdated,
	).First(&auditEntry).Error)
	changes := map[string]map[string]any{}
	for _, raw := range auditEntry.Changes {
		change, ok := raw.(map[string]any)
		require.True(t, ok)
		field, ok := change["field"].(string)
		require.True(t, ok)
		changes[field] = change
	}
	require.Contains(t, changes, "state")
	assert.Equal(t, "active", changes["state"]["old_value"])
	assert.Equal(t, "revoked", changes["state"]["new_value"])
	require.Contains(t, changes, "is_active")
	assert.Equal(t, true, changes["is_active"]["old_value"])
	assert.Equal(t, false, changes["is_active"]["new_value"])
	require.Contains(t, changes, "revocation_reason")
	assert.Equal(t, "approved support revocation", changes["revocation_reason"]["new_value"])

	second := call("approved support revocation")
	assert.False(t, second.Revoked, "exact reason is the retry identity")
	assert.Equal(t, first.OverrideID, second.OverrideID)
	assert.WithinDuration(t, first.RevokedAt, second.RevokedAt, time.Microsecond)
	assert.True(t, second.EffectiveEnabled)

	var auditCount int64
	require.NoError(t, app.DB.Model(&models.AuditLog{}).
		Where(
			"organization_id = ? AND resource_type = ? AND resource_id = ? AND action = ?",
			targetOrg.ID,
			"entitlement_override",
			targetOverride.ID,
			models.AuditActionUpdated,
		).
		Count(&auditCount).Error)
	assert.EqualValues(t, 1, auditCount)

	differentReason := callThreadsSupportRevoke(
		t,
		app,
		targetOrg.ID,
		owner,
		targetOrg.ID,
		map[string]any{
			"override_id": targetOverride.ID,
			"reason":      "a different retry reason",
		},
	)
	testutil.AssertErrorResponse(
		t,
		differentReason,
		fasthttp.StatusConflict,
		"Revocation retry does not match the recorded operation",
	)
}

func TestRevokeOrganizationThreadsPublicEngagementSupportRetryUsesImmutableOverrideID(
	t *testing.T,
) {
	app := newTestApp(t)
	controlOrg := testutil.CreateTestOrganization(t, app.DB)
	targetOrg := testutil.CreateTestOrganization(t, app.DB)
	owner := testutil.CreateTestUser(
		t,
		app.DB,
		controlOrg.ID,
		testutil.WithEmail(testutil.UniqueEmail("threads-revoke-retry-owner")),
		testutil.WithSuperAdmin(),
	)
	now := time.Now().UTC()
	var targetOverrideID uuid.UUID
	for i := 0; i < 2; i++ {
		override := createThreadsSupportOverride(
			t,
			app,
			targetOrg.ID,
			owner.ID,
			"support",
			true,
			false,
			now.Add(-time.Duration(i+1)*time.Hour),
			fmt.Sprintf("historical-support-%d", i),
		)
		if i == 0 {
			targetOverrideID = override.ID
		}
		revokedAt := now.Add(-time.Duration(i+1) * time.Minute)
		require.NoError(t, app.DB.Model(&models.EntitlementOverride{}).
			Where("id = ?", override.ID).
			Updates(map[string]any{
				"revoked_at":        revokedAt,
				"revoked_by_id":     owner.ID,
				"revocation_reason": "shared-retry-reason",
			}).Error)
	}

	request := callThreadsSupportRevoke(
		t,
		app,
		targetOrg.ID,
		owner,
		targetOrg.ID,
		map[string]any{
			"override_id": targetOverrideID,
			"reason":      "shared-retry-reason",
		},
	)
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(request))
	response := decodeThreadsSupportRevokeResponse(t, request)
	assert.False(t, response.Revoked)
	assert.Equal(t, targetOverrideID, response.OverrideID)
}

func TestRevokeOrganizationThreadsPublicEngagementSupportStaleReplayCannotRevokeNewGrant(
	t *testing.T,
) {
	app := newTestApp(t)
	controlOrg := testutil.CreateTestOrganization(t, app.DB)
	targetOrg := testutil.CreateTestOrganization(t, app.DB)
	owner := testutil.CreateTestUser(
		t,
		app.DB,
		controlOrg.ID,
		testutil.WithEmail(testutil.UniqueEmail("threads-revoke-replay-owner")),
		testutil.WithSuperAdmin(),
	)
	growthPlan := ensureThreadsSupportCatalogPlan(
		t,
		app,
		"rereply-growth",
		"ReReply Growth",
	)
	createThreadsSupportSubscription(
		t,
		app,
		targetOrg,
		&growthPlan,
		models.SubscriptionStatusActive,
		time.Now().UTC().Add(30*24*time.Hour),
		true,
	)

	grantA := createThreadsSupportOverride(
		t,
		app,
		targetOrg.ID,
		owner.ID,
		"support",
		true,
		true,
		time.Now().UTC().Add(-time.Minute),
		"support-grant-a",
	)
	revokeA := func() *fastglue.Request {
		return callThreadsSupportRevoke(
			t,
			app,
			targetOrg.ID,
			owner,
			targetOrg.ID,
			map[string]any{
				"override_id": grantA.ID,
				"reason":      "revoke-operation-a",
			},
		)
	}

	firstRevoke := revokeA()
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(firstRevoke))

	enableBRequest := newThreadsSupportRequest(
		t,
		targetOrg.ID,
		owner,
		targetOrg.ID,
		map[string]any{"reason": "support-grant-b"},
	)
	require.NoError(t, app.EnableOrganizationThreadsPublicEngagement(enableBRequest))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(enableBRequest))
	var enableEnvelope struct {
		Data handlers.EnableThreadsPublicEngagementResponse `json:"data"`
	}
	require.NoError(t, json.Unmarshal(testutil.GetResponseBody(enableBRequest), &enableEnvelope))
	grantBID := enableEnvelope.Data.OverrideID
	require.NotEqual(t, grantA.ID, grantBID)

	delayedReplay := revokeA()
	testutil.AssertErrorResponse(
		t,
		delayedReplay,
		fasthttp.StatusConflict,
		"Active support override changed; refresh before revoking",
	)

	var storedA models.EntitlementOverride
	require.NoError(t, app.DB.First(&storedA, "id = ?", grantA.ID).Error)
	assert.False(t, storedA.IsActive)
	assert.Equal(t, "revoke-operation-a", storedA.RevocationReason)

	var storedB models.EntitlementOverride
	require.NoError(t, app.DB.First(&storedB, "id = ?", grantBID).Error)
	assert.True(t, storedB.IsActive)
	assert.Nil(t, storedB.RevokedAt)
	assert.Empty(t, storedB.RevocationReason)

	statusRequest := callThreadsSupportStatus(
		t,
		app,
		targetOrg.ID,
		owner,
		targetOrg.ID,
	)
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(statusRequest))
	status := decodeThreadsSupportStatusResponse(t, statusRequest)
	assert.True(t, status.Active)
	require.NotNil(t, status.OverrideID)
	assert.Equal(t, grantBID, *status.OverrideID)
}
