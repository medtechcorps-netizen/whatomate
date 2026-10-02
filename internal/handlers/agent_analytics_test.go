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

// Agent analytics must resolve agents through organization membership
// (home org or user_organizations), not only users.organization_id.

func createAgentAnalyticsViewer(t *testing.T, app *handlers.App, orgID uuid.UUID) *models.User {
	t.Helper()
	perms := getAnalyticsPermissions(t, app)
	role := testutil.CreateTestRoleExact(t, app.DB, orgID, "Analytics Viewer "+uuid.New().String()[:8], false, false, perms)
	return testutil.CreateTestUser(t, app.DB, orgID, testutil.WithRoleID(&role.ID))
}

func TestApp_GetAgentDetails_CrossOrgMember(t *testing.T) {
	app := newTestApp(t)
	homeOrg := testutil.CreateTestOrganization(t, app.DB)
	workOrg := testutil.CreateTestOrganization(t, app.DB)
	viewer := createAgentAnalyticsViewer(t, app, workOrg.ID)

	agent := testutil.CreateTestUser(t, app.DB, homeOrg.ID, testutil.WithFullName("Cross Org Analyst Agent"))
	addDirectOrgMembership(t, app, agent.ID, workOrg.ID, nil)

	contact := testutil.CreateTestContact(t, app.DB, workOrg.ID)
	now := time.Now().UTC()
	resumedAt := currentMonthTestTime(now, -10*time.Minute)
	createTestAgentTransfer(t, app, workOrg.ID, contact.ID, &agent.ID,
		models.TransferStatusResumed, models.TransferSourceManual,
		currentMonthTestTime(now, -1*time.Hour), &resumedAt)

	req := testutil.NewGETRequest(t)
	testutil.SetAuthContext(req, workOrg.ID, viewer.ID)
	testutil.SetPathParam(req, "id", agent.ID.String())

	require.NoError(t, app.GetAgentDetails(req))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req), string(testutil.GetResponseBody(req)))

	var resp struct {
		Data struct {
			Agent handlers.AgentPerformanceStats `json:"agent"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(testutil.GetResponseBody(req), &resp))
	assert.Equal(t, agent.ID.String(), resp.Data.Agent.AgentID)
	assert.Equal(t, "Cross Org Analyst Agent", resp.Data.Agent.AgentName)
	assert.Equal(t, int64(1), resp.Data.Agent.TransfersHandled)
}

func TestApp_GetAgentDetails_RejectsNonMemberAndSuspendedResellerMembership(t *testing.T) {
	app := newTestApp(t)
	reseller := testutil.CreateTestReseller(t, app.DB)
	homeOrg := testutil.CreateTestOrganization(t, app.DB)
	clinicOrg := testutil.CreateTestOrganizationForReseller(t, app.DB, reseller.ID)
	viewer := createAgentAnalyticsViewer(t, app, clinicOrg.ID)

	outsider := testutil.CreateTestUser(t, app.DB, homeOrg.ID)
	resellerAdmin := testutil.CreateTestUser(t, app.DB, homeOrg.ID)
	addResellerDerivedMembership(t, app, resellerAdmin.ID, reseller, clinicOrg.ID, nil)
	suspendTestReseller(t, app, reseller.ID)

	for _, candidate := range []uuid.UUID{outsider.ID, resellerAdmin.ID} {
		req := testutil.NewGETRequest(t)
		testutil.SetAuthContext(req, clinicOrg.ID, viewer.ID)
		testutil.SetPathParam(req, "id", candidate.String())

		require.NoError(t, app.GetAgentDetails(req))
		assertNotFoundMessage(t, req, "Agent not found")
	}
}

func TestApp_GetAgentDetails_RejectsRemovedHomeOrgMember(t *testing.T) {
	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	viewer := createAgentAnalyticsViewer(t, app, org.ID)

	// Removed with RemoveOrganizationMember: membership row soft-deleted,
	// users.organization_id unchanged.
	removed := testutil.CreateTestUser(t, app.DB, org.ID)
	require.NoError(t, app.DB.
		Where("user_id = ? AND organization_id = ?", removed.ID, org.ID).
		Delete(&models.UserOrganization{}).Error)

	req := testutil.NewGETRequest(t)
	testutil.SetAuthContext(req, org.ID, viewer.ID)
	testutil.SetPathParam(req, "id", removed.ID.String())

	require.NoError(t, app.GetAgentDetails(req))
	assertNotFoundMessage(t, req, "Agent not found")
}

func TestApp_GetAgentDetails_DeactivatedHomeOrgAgentKeepsHistory(t *testing.T) {
	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	viewer := createAgentAnalyticsViewer(t, app, org.ID)
	deactivated := testutil.CreateTestUser(t, app.DB, org.ID, testutil.WithInactive())

	req := testutil.NewGETRequest(t)
	testutil.SetAuthContext(req, org.ID, viewer.ID)
	testutil.SetPathParam(req, "id", deactivated.ID.String())

	require.NoError(t, app.GetAgentDetails(req))
	assert.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req))
}

func TestApp_GetAgentComparison_IncludesCrossOrgTeamAgents(t *testing.T) {
	app := newTestApp(t)
	reseller := testutil.CreateTestReseller(t, app.DB)
	homeOrg := testutil.CreateTestOrganization(t, app.DB)
	otherOrg := testutil.CreateTestOrganization(t, app.DB)
	clinicOrg := testutil.CreateTestOrganizationForReseller(t, app.DB, reseller.ID)
	viewer := createAgentAnalyticsViewer(t, app, clinicOrg.ID)

	// Home-org agent in a clinic team: included (unchanged).
	homeAgent := testutil.CreateTestUser(t, app.DB, clinicOrg.ID, testutil.WithFullName("Home Agent"))
	createTestTeamWithAgent(t, app, clinicOrg.ID, homeAgent.ID)

	// Cross-org direct member in a clinic team: now included.
	crossAgent := testutil.CreateTestUser(t, app.DB, homeOrg.ID, testutil.WithFullName("Cross Agent"))
	addDirectOrgMembership(t, app, crossAgent.ID, clinicOrg.ID, nil)
	createTestTeamWithAgent(t, app, clinicOrg.ID, crossAgent.ID)

	// Suspended reseller-derived member in a clinic team: excluded.
	suspendedAgent := testutil.CreateTestUser(t, app.DB, homeOrg.ID, testutil.WithFullName("Suspended Agent"))
	addResellerDerivedMembership(t, app, suspendedAgent.ID, reseller, clinicOrg.ID, nil)
	createTestTeamWithAgent(t, app, clinicOrg.ID, suspendedAgent.ID)
	suspendTestReseller(t, app, reseller.ID)

	// Clinic-org user who is only an agent in another organization's team:
	// not an agent of this organization's teams.
	elsewhereAgent := testutil.CreateTestUser(t, app.DB, clinicOrg.ID, testutil.WithFullName("Elsewhere Agent"))
	createTestTeamWithAgent(t, app, otherOrg.ID, elsewhereAgent.ID)

	// Non-member placed in a clinic team row directly: excluded.
	outsider := testutil.CreateTestUser(t, app.DB, otherOrg.ID, testutil.WithFullName("Outsider Agent"))
	createTestTeamWithAgent(t, app, clinicOrg.ID, outsider.ID)

	// Agent removed from a clinic team (team_members row soft-deleted):
	// still included so past-period stats stay stable.
	leftTeamAgent := testutil.CreateTestUser(t, app.DB, clinicOrg.ID, testutil.WithFullName("Left Team Agent"))
	_, leftMember := createTestTeamWithAgent(t, app, clinicOrg.ID, leftTeamAgent.ID)
	require.NoError(t, app.DB.Delete(leftMember).Error)

	// Agent of a clinic team that was deleted: still included.
	deletedTeamAgent := testutil.CreateTestUser(t, app.DB, clinicOrg.ID, testutil.WithFullName("Deleted Team Agent"))
	deletedTeam, deletedMember := createTestTeamWithAgent(t, app, clinicOrg.ID, deletedTeamAgent.ID)
	require.NoError(t, app.DB.Delete(deletedMember).Error)
	require.NoError(t, app.DB.Delete(deletedTeam).Error)

	// Home-org agent removed from the organization (membership row
	// soft-deleted, users.organization_id unchanged): excluded, as the auth
	// layer no longer lets them in.
	removedAgent := testutil.CreateTestUser(t, app.DB, clinicOrg.ID, testutil.WithFullName("Removed Agent"))
	createTestTeamWithAgent(t, app, clinicOrg.ID, removedAgent.ID)
	require.NoError(t, app.DB.
		Where("user_id = ? AND organization_id = ?", removedAgent.ID, clinicOrg.ID).
		Delete(&models.UserOrganization{}).Error)

	req := testutil.NewGETRequest(t)
	testutil.SetAuthContext(req, clinicOrg.ID, viewer.ID)

	require.NoError(t, app.GetAgentComparison(req))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req), string(testutil.GetResponseBody(req)))

	var resp struct {
		Data struct {
			Agents []handlers.AgentPerformanceStats `json:"agents"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(testutil.GetResponseBody(req), &resp))

	ids := make([]string, 0, len(resp.Data.Agents))
	for _, agent := range resp.Data.Agents {
		ids = append(ids, agent.AgentID)
	}
	assert.ElementsMatch(t, []string{
		homeAgent.ID.String(), crossAgent.ID.String(),
		leftTeamAgent.ID.String(), deletedTeamAgent.ID.String(),
	}, ids)
}
