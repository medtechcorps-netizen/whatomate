package handlers_test

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/shridarpatil/whatomate/internal/handlers"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
)

// laneAMePolicy reads the inactivity policy every member's browser gets: the
// X-Session-Idle-Minutes header on GET /api/me.
func laneAMePolicy(t *testing.T, app *handlers.App, orgID, userID any) string {
	t.Helper()
	req := testutil.NewGETRequest(t)
	req.RequestCtx.SetUserValue("organization_id", orgID)
	req.RequestCtx.SetUserValue("user_id", userID)
	require.NoError(t, app.GetCurrentUser(req))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req), string(testutil.GetResponseBody(req)))
	return string(req.RequestCtx.Response.Header.Peek("X-Session-Idle-Minutes"))
}

// laneASessionPolicy reads the full organization settings.
func laneASessionPolicy(t *testing.T, app *handlers.App, orgID, userID any) (int, map[string]any) {
	t.Helper()
	req := testutil.NewGETRequest(t)
	req.RequestCtx.SetUserValue("organization_id", orgID)
	req.RequestCtx.SetUserValue("user_id", userID)
	require.NoError(t, app.GetOrganizationSettings(req))
	status := testutil.GetResponseStatusCode(req)
	var resp struct {
		Data struct {
			Settings map[string]any `json:"settings"`
		} `json:"data"`
	}
	if status == fasthttp.StatusOK {
		require.NoError(t, json.Unmarshal(testutil.GetResponseBody(req), &resp))
	}
	return status, resp.Data.Settings
}

func laneAPutSessionPolicy(t *testing.T, app *handlers.App, orgID, userID any, body map[string]any) int {
	t.Helper()
	req := testutil.NewJSONRequest(t, body)
	req.RequestCtx.SetUserValue("organization_id", orgID)
	req.RequestCtx.SetUserValue("user_id", userID)
	require.NoError(t, app.UpdateOrganizationSettings(req))
	return testutil.GetResponseStatusCode(req)
}

func TestLaneA_SessionIdlePolicy_DefaultsToFourHoursAndIsReadableByMembers(t *testing.T) {
	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	agentRole := testutil.CreateAgentRole(t, app.DB, org.ID)
	agent := testutil.CreateTestUser(t, app.DB, org.ID, testutil.WithRoleID(&agentRole.ID))

	assert.Equal(t, "240", laneAMePolicy(t, app, org.ID, agent.ID))

	// The full settings stay behind settings.general read.
	status, _ := laneASessionPolicy(t, app, org.ID, agent.ID)
	assert.Equal(t, fasthttp.StatusForbidden, status)
}

func TestLaneA_SessionIdlePolicy_AdminCanSetEveryOption(t *testing.T) {
	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	adminRole := testutil.CreateAdminRole(t, app.DB, org.ID)
	admin := testutil.CreateTestUser(t, app.DB, org.ID, testutil.WithRoleID(&adminRole.ID))

	for _, minutes := range []int{0, 30, 60, 120, 240, 480} {
		require.Equal(t, fasthttp.StatusOK, laneAPutSessionPolicy(t, app, org.ID, admin.ID,
			map[string]any{"session_idle_timeout_minutes": minutes}))

		assert.Equal(t, fmt.Sprint(minutes), laneAMePolicy(t, app, org.ID, admin.ID))

		status, full := laneASessionPolicy(t, app, org.ID, admin.ID)
		require.Equal(t, fasthttp.StatusOK, status)
		assert.Equal(t, float64(minutes), full["session_idle_timeout_minutes"])
	}

	// Saving another General field leaves the policy alone.
	require.Equal(t, fasthttp.StatusOK, laneAPutSessionPolicy(t, app, org.ID, admin.ID,
		map[string]any{"timezone": "Asia/Kuala_Lumpur"}))
	assert.Equal(t, "480", laneAMePolicy(t, app, org.ID, admin.ID))

	var logs []models.AuditLog
	require.NoError(t, app.DB.Where(
		"organization_id = ? AND resource_type = ?", org.ID, models.ResourceSettingsGeneral,
	).Find(&logs).Error)
	found := false
	for _, log := range logs {
		for _, raw := range log.Changes {
			if change, ok := raw.(map[string]any); ok && change["field"] == "session_idle_timeout_minutes" {
				found = true
			}
		}
	}
	assert.True(t, found, "policy changes must be audited with the General settings")
}

func TestLaneA_SessionIdlePolicy_RejectsOtherValues(t *testing.T) {
	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	adminRole := testutil.CreateAdminRole(t, app.DB, org.ID)
	admin := testutil.CreateTestUser(t, app.DB, org.ID, testutil.WithRoleID(&adminRole.ID))

	for _, minutes := range []int{-1, 1, 15, 45, 1440} {
		assert.Equal(t, fasthttp.StatusBadRequest, laneAPutSessionPolicy(t, app, org.ID, admin.ID,
			map[string]any{"session_idle_timeout_minutes": minutes}), "minutes=%d", minutes)
	}
	assert.Equal(t, "240", laneAMePolicy(t, app, org.ID, admin.ID))
}

func TestLaneA_SessionIdlePolicy_RequiresGeneralWrite(t *testing.T) {
	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	managerRole := testutil.CreateTestRoleWithKeys(t, app.DB, org.ID, "manager",
		models.SystemRolePermissions()["manager"])
	manager := testutil.CreateTestUser(t, app.DB, org.ID, testutil.WithRoleID(&managerRole.ID))

	assert.Equal(t, fasthttp.StatusForbidden, laneAPutSessionPolicy(t, app, org.ID, manager.ID,
		map[string]any{"session_idle_timeout_minutes": 0}))
	assert.Equal(t, "240", laneAMePolicy(t, app, org.ID, manager.ID))
}

func TestLaneA_SessionIdlePolicy_IgnoresInvalidStoredValue(t *testing.T) {
	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	user := testutil.CreateTestUser(t, app.DB, org.ID)
	org.Settings = models.JSONB{"session_idle_timeout_minutes": 17}
	require.NoError(t, app.DB.Save(org).Error)

	assert.Equal(t, "240", laneAMePolicy(t, app, org.ID, user.ID))
}
