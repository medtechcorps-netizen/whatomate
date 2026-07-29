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

// getAnalyticsPermissions returns analytics permissions from the full permission set.
func getAnalyticsPermissions(t *testing.T, app *handlers.App) []models.Permission {
	t.Helper()

	allPerms := testutil.GetOrCreateTestPermissions(t, app.DB)

	var analyticsPerms []models.Permission
	for _, p := range allPerms {
		if p.Resource == "analytics" {
			analyticsPerms = append(analyticsPerms, p)
		}
	}
	require.NotEmpty(t, analyticsPerms, "expected analytics permissions in default set")
	return analyticsPerms
}

// createTestWidget creates a test dashboard widget in the database.
func createTestWidget(t *testing.T, app *handlers.App, orgID uuid.UUID, userID *uuid.UUID, name string, isShared, isDefault bool) *models.Widget {
	t.Helper()

	widget := &models.Widget{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: orgID,
		UserID:         userID,
		Name:           name,
		Description:    "Test widget description",
		DataSource:     "messages",
		Metric:         "count",
		DisplayType:    "number",
		ShowChange:     true,
		Color:          "blue",
		Size:           "small",
		DisplayOrder:   1,
		IsShared:       isShared,
		IsDefault:      isDefault,
	}
	require.NoError(t, app.DB.Create(widget).Error)
	return widget
}

func createTestWidgetDefinition(
	t *testing.T,
	app *handlers.App,
	orgID uuid.UUID,
	userID uuid.UUID,
	name, dataSource, metric, field, displayType, groupByField string,
) *models.Widget {
	t.Helper()
	widget := &models.Widget{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: orgID,
		UserID:         &userID,
		Name:           name,
		DataSource:     dataSource,
		Metric:         metric,
		Field:          field,
		DisplayType:    displayType,
		ChartType:      "line",
		GroupByField:   groupByField,
		ShowChange:     true,
		Color:          "blue",
		Size:           "small",
	}
	require.NoError(t, app.DB.Create(widget).Error)
	return widget
}

func getTestWidgetData(
	t *testing.T,
	app *handlers.App,
	orgID, userID, widgetID uuid.UUID,
) (handlers.WidgetDataResponse, int) {
	t.Helper()
	req := testutil.NewGETRequest(t)
	testutil.SetAuthContext(req, orgID, userID)
	testutil.SetPathParam(req, "id", widgetID.String())
	testutil.SetQueryParam(req, "from", time.Now().UTC().AddDate(0, 0, -1).Format("2006-01-02"))
	testutil.SetQueryParam(req, "to", time.Now().UTC().AddDate(0, 0, 1).Format("2006-01-02"))
	require.NoError(t, app.GetWidgetData(req))

	var response struct {
		Data handlers.WidgetDataResponse `json:"data"`
	}
	if testutil.GetResponseStatusCode(req) == fasthttp.StatusOK {
		require.NoError(t, json.Unmarshal(testutil.GetResponseBody(req), &response))
	}
	return response.Data, testutil.GetResponseStatusCode(req)
}

func createWidgetCRMLeads(
	t *testing.T,
	app *handlers.App,
	orgID uuid.UUID,
	values ...int64,
) []*models.CRMLead {
	t.Helper()
	contact := testutil.CreateTestContact(t, app.DB, orgID)
	pipeline := &models.CRMPipeline{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: orgID,
		Name:           "Widget pipeline " + uuid.NewString()[:8],
		IsActive:       true,
		Version:        1,
	}
	require.NoError(t, app.DB.Create(pipeline).Error)
	stage := &models.CRMPipelineStage{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: orgID,
		PipelineID:     pipeline.ID,
		Name:           "Open",
		Kind:           models.CRMPipelineStageKindOpen,
		IsActive:       true,
		Version:        1,
	}
	require.NoError(t, app.DB.Create(stage).Error)

	leads := make([]*models.CRMLead, 0, len(values))
	for index, value := range values {
		status := models.CRMLeadStatusOpen
		if index%2 == 1 {
			status = models.CRMLeadStatusWon
		}
		lead := &models.CRMLead{
			BaseModel:      models.BaseModel{ID: uuid.New()},
			OrganizationID: orgID,
			ContactID:      contact.ID,
			PipelineID:     pipeline.ID,
			StageID:        stage.ID,
			Title:          "Widget lead " + uuid.NewString()[:8],
			Status:         status,
			Source:         models.CRMLeadSourceOther,
			ValueMinor:     value,
			Currency:       "MYR",
			Metadata:       models.JSONB{},
			Version:        1,
		}
		require.NoError(t, app.DB.Create(lead).Error)
		leads = append(leads, lead)
	}
	return leads
}

// --- ListDashboardWidgets Tests ---

func TestApp_ListWidgets_Success(t *testing.T) {
	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	perms := getAnalyticsPermissions(t, app)
	role := testutil.CreateTestRoleExact(t, app.DB, org.ID, "Analytics User", false, false, perms)
	user := testutil.CreateTestUser(t, app.DB, org.ID, testutil.WithEmail(testutil.UniqueEmail("list-widgets")), testutil.WithPassword("password"), testutil.WithRoleID(&role.ID))

	// Create multiple widgets
	createTestWidget(t, app, org.ID, &user.ID, "Widget 1", true, false)
	createTestWidget(t, app, org.ID, &user.ID, "Widget 2", true, false)

	req := testutil.NewGETRequest(t)
	testutil.SetAuthContext(req, org.ID, user.ID)

	err := app.ListWidgets(req)
	require.NoError(t, err)
	assert.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req))

	var resp struct {
		Data struct {
			Widgets []handlers.WidgetResponse `json:"widgets"`
		} `json:"data"`
	}
	err = json.Unmarshal(testutil.GetResponseBody(req), &resp)
	require.NoError(t, err)
	assert.Len(t, resp.Data.Widgets, 2)
}

func TestApp_ListWidgets_NoPermission(t *testing.T) {
	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	// User without analytics permission
	user := testutil.CreateTestUser(t, app.DB, org.ID, testutil.WithEmail(testutil.UniqueEmail("list-no-perm")), testutil.WithPassword("password"))

	req := testutil.NewGETRequest(t)
	testutil.SetAuthContext(req, org.ID, user.ID)

	err := app.ListWidgets(req)
	require.NoError(t, err)
	assert.Equal(t, fasthttp.StatusForbidden, testutil.GetResponseStatusCode(req))
}

func TestApp_ListWidgets_FiltersByOrganization(t *testing.T) {
	app := newTestApp(t)

	// Create two organizations
	org1 := testutil.CreateTestOrganization(t, app.DB)
	org2 := testutil.CreateTestOrganization(t, app.DB)

	perms := getAnalyticsPermissions(t, app)
	role1 := testutil.CreateTestRoleExact(t, app.DB, org1.ID, "Analytics User 1", false, false, perms)
	role2 := testutil.CreateTestRoleExact(t, app.DB, org2.ID, "Analytics User 2", false, false, perms)

	user1 := testutil.CreateTestUser(t, app.DB, org1.ID, testutil.WithEmail(testutil.UniqueEmail("list-org1")), testutil.WithPassword("password"), testutil.WithRoleID(&role1.ID))
	user2 := testutil.CreateTestUser(t, app.DB, org2.ID, testutil.WithEmail(testutil.UniqueEmail("list-org2")), testutil.WithPassword("password"), testutil.WithRoleID(&role2.ID))

	// Create widgets for each org
	createTestWidget(t, app, org1.ID, &user1.ID, "Org1 Widget", true, false)
	createTestWidget(t, app, org2.ID, &user2.ID, "Org2 Widget", true, false)

	// User from org1 should only see org1's widgets
	req := testutil.NewGETRequest(t)
	testutil.SetAuthContext(req, org1.ID, user1.ID)

	err := app.ListWidgets(req)
	require.NoError(t, err)
	assert.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req))

	var resp struct {
		Data struct {
			Widgets []handlers.WidgetResponse `json:"widgets"`
		} `json:"data"`
	}
	err = json.Unmarshal(testutil.GetResponseBody(req), &resp)
	require.NoError(t, err)
	assert.Len(t, resp.Data.Widgets, 1)
	assert.Equal(t, "Org1 Widget", resp.Data.Widgets[0].Name)
}

func TestApp_ListWidgets_Unauthorized(t *testing.T) {
	app := newTestApp(t)

	req := testutil.NewGETRequest(t)
	// No auth context set

	err := app.ListWidgets(req)
	require.NoError(t, err)
	assert.Equal(t, fasthttp.StatusUnauthorized, testutil.GetResponseStatusCode(req))
}

// --- GetDashboardWidget Tests ---

func TestApp_GetWidget_Success(t *testing.T) {
	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	perms := getAnalyticsPermissions(t, app)
	role := testutil.CreateTestRoleExact(t, app.DB, org.ID, "Analytics User", false, false, perms)
	user := testutil.CreateTestUser(t, app.DB, org.ID, testutil.WithEmail(testutil.UniqueEmail("get-widget")), testutil.WithPassword("password"), testutil.WithRoleID(&role.ID))
	widget := createTestWidget(t, app, org.ID, &user.ID, "Test Widget", true, false)

	req := testutil.NewGETRequest(t)
	testutil.SetAuthContext(req, org.ID, user.ID)
	testutil.SetPathParam(req, "id", widget.ID.String())

	err := app.GetWidget(req)
	require.NoError(t, err)
	assert.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req))

	var resp struct {
		Data handlers.WidgetResponse `json:"data"`
	}
	err = json.Unmarshal(testutil.GetResponseBody(req), &resp)
	require.NoError(t, err)
	assert.Equal(t, widget.ID, resp.Data.ID)
	assert.Equal(t, "Test Widget", resp.Data.Name)
}

func TestApp_GetWidget_NoPermission(t *testing.T) {
	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	perms := getAnalyticsPermissions(t, app)
	role := testutil.CreateTestRoleExact(t, app.DB, org.ID, "Analytics User", false, false, perms)
	owner := testutil.CreateTestUser(t, app.DB, org.ID, testutil.WithEmail(testutil.UniqueEmail("owner-get")), testutil.WithPassword("password"), testutil.WithRoleID(&role.ID))
	// User without analytics permission
	otherUser := testutil.CreateTestUser(t, app.DB, org.ID, testutil.WithEmail(testutil.UniqueEmail("no-perm-get")), testutil.WithPassword("password"))

	widget := createTestWidget(t, app, org.ID, &owner.ID, "Test Widget", true, false)

	req := testutil.NewGETRequest(t)
	testutil.SetAuthContext(req, org.ID, otherUser.ID)
	testutil.SetPathParam(req, "id", widget.ID.String())

	err := app.GetWidget(req)
	require.NoError(t, err)
	assert.Equal(t, fasthttp.StatusForbidden, testutil.GetResponseStatusCode(req))
}

func TestApp_GetWidget_NotFound(t *testing.T) {
	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	perms := getAnalyticsPermissions(t, app)
	role := testutil.CreateTestRoleExact(t, app.DB, org.ID, "Analytics User", false, false, perms)
	user := testutil.CreateTestUser(t, app.DB, org.ID, testutil.WithEmail(testutil.UniqueEmail("get-not-found")), testutil.WithPassword("password"), testutil.WithRoleID(&role.ID))

	req := testutil.NewGETRequest(t)
	testutil.SetAuthContext(req, org.ID, user.ID)
	testutil.SetPathParam(req, "id", uuid.New().String())

	err := app.GetWidget(req)
	require.NoError(t, err)
	assert.Equal(t, fasthttp.StatusNotFound, testutil.GetResponseStatusCode(req))
}

func TestApp_GetWidget_InvalidID(t *testing.T) {
	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	perms := getAnalyticsPermissions(t, app)
	role := testutil.CreateTestRoleExact(t, app.DB, org.ID, "Analytics User", false, false, perms)
	user := testutil.CreateTestUser(t, app.DB, org.ID, testutil.WithEmail(testutil.UniqueEmail("get-invalid-id")), testutil.WithPassword("password"), testutil.WithRoleID(&role.ID))

	req := testutil.NewGETRequest(t)
	testutil.SetAuthContext(req, org.ID, user.ID)
	testutil.SetPathParam(req, "id", "not-a-uuid")

	err := app.GetWidget(req)
	require.NoError(t, err)
	assert.Equal(t, fasthttp.StatusBadRequest, testutil.GetResponseStatusCode(req))
}

// --- CreateDashboardWidget Tests ---

func TestApp_CreateWidget_Success(t *testing.T) {
	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	perms := getAnalyticsPermissions(t, app)
	role := testutil.CreateTestRoleExact(t, app.DB, org.ID, "Analytics User", false, false, perms)
	user := testutil.CreateTestUser(t, app.DB, org.ID, testutil.WithEmail(testutil.UniqueEmail("create-widget")), testutil.WithPassword("password"), testutil.WithRoleID(&role.ID))

	req := testutil.NewJSONRequest(t, map[string]any{
		"name":        "New Widget",
		"description": "A test widget",
		"data_source": "messages",
		"metric":      "count",
		"color":       "green",
	})
	testutil.SetAuthContext(req, org.ID, user.ID)

	err := app.CreateWidget(req)
	require.NoError(t, err)
	assert.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req))

	var resp struct {
		Data handlers.WidgetResponse `json:"data"`
	}
	err = json.Unmarshal(testutil.GetResponseBody(req), &resp)
	require.NoError(t, err)
	assert.Equal(t, "New Widget", resp.Data.Name)
	assert.Equal(t, "messages", resp.Data.DataSource)
	assert.Equal(t, "green", resp.Data.Color)
}

func TestApp_CreateWidget_NoPermission(t *testing.T) {
	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	// User without analytics write permission (only read)
	readOnlyPerms := getAnalyticsPermissions(t, app)
	readOnlyRole := testutil.CreateTestRoleExact(t, app.DB, org.ID, "Read Only", false, false, readOnlyPerms[:1]) // Only read permission
	user := testutil.CreateTestUser(t, app.DB, org.ID, testutil.WithEmail(testutil.UniqueEmail("create-no-perm")), testutil.WithPassword("password"), testutil.WithRoleID(&readOnlyRole.ID))

	req := testutil.NewJSONRequest(t, map[string]any{
		"name":        "New Widget",
		"data_source": "messages",
		"metric":      "count",
	})
	testutil.SetAuthContext(req, org.ID, user.ID)

	err := app.CreateWidget(req)
	require.NoError(t, err)
	assert.Equal(t, fasthttp.StatusForbidden, testutil.GetResponseStatusCode(req))
}

func TestApp_CreateWidget_WithFilters(t *testing.T) {
	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	perms := getAnalyticsPermissions(t, app)
	role := testutil.CreateTestRoleExact(t, app.DB, org.ID, "Analytics User", false, false, perms)
	user := testutil.CreateTestUser(t, app.DB, org.ID, testutil.WithEmail(testutil.UniqueEmail("create-with-filters")), testutil.WithPassword("password"), testutil.WithRoleID(&role.ID))

	req := testutil.NewJSONRequest(t, map[string]any{
		"name":        "Filtered Widget",
		"data_source": "messages",
		"metric":      "count",
		"filters": []map[string]any{
			{
				"field":    "direction",
				"operator": "equals",
				"value":    "inbound",
			},
		},
	})
	testutil.SetAuthContext(req, org.ID, user.ID)

	err := app.CreateWidget(req)
	require.NoError(t, err)
	assert.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req))

	var resp struct {
		Data handlers.WidgetResponse `json:"data"`
	}
	err = json.Unmarshal(testutil.GetResponseBody(req), &resp)
	require.NoError(t, err)
	assert.Len(t, resp.Data.Filters, 1)
}

func TestApp_CreateWidget_InvalidDataSource(t *testing.T) {
	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	perms := getAnalyticsPermissions(t, app)
	role := testutil.CreateTestRoleExact(t, app.DB, org.ID, "Analytics User", false, false, perms)
	user := testutil.CreateTestUser(t, app.DB, org.ID, testutil.WithEmail(testutil.UniqueEmail("create-invalid-source")), testutil.WithPassword("password"), testutil.WithRoleID(&role.ID))

	req := testutil.NewJSONRequest(t, map[string]any{
		"name":        "Invalid Widget",
		"data_source": "invalid_source",
		"metric":      "count",
	})
	testutil.SetAuthContext(req, org.ID, user.ID)

	err := app.CreateWidget(req)
	require.NoError(t, err)
	assert.Equal(t, fasthttp.StatusBadRequest, testutil.GetResponseStatusCode(req))
}

func TestApp_CreateWidget_MissingName(t *testing.T) {
	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	perms := getAnalyticsPermissions(t, app)
	role := testutil.CreateTestRoleExact(t, app.DB, org.ID, "Analytics User", false, false, perms)
	user := testutil.CreateTestUser(t, app.DB, org.ID, testutil.WithEmail(testutil.UniqueEmail("create-missing-name")), testutil.WithPassword("password"), testutil.WithRoleID(&role.ID))

	req := testutil.NewJSONRequest(t, map[string]any{
		"data_source": "messages",
		"metric":      "count",
	})
	testutil.SetAuthContext(req, org.ID, user.ID)

	err := app.CreateWidget(req)
	require.NoError(t, err)
	assert.Equal(t, fasthttp.StatusBadRequest, testutil.GetResponseStatusCode(req))
}

func TestApp_CreateWidget_Unauthorized(t *testing.T) {
	app := newTestApp(t)

	req := testutil.NewJSONRequest(t, map[string]any{
		"name":        "Widget",
		"data_source": "messages",
		"metric":      "count",
	})
	// No auth context

	err := app.CreateWidget(req)
	require.NoError(t, err)
	assert.Equal(t, fasthttp.StatusUnauthorized, testutil.GetResponseStatusCode(req))
}

// --- UpdateDashboardWidget Tests ---

func TestApp_UpdateWidget_Success(t *testing.T) {
	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	perms := getAnalyticsPermissions(t, app)
	role := testutil.CreateTestRoleExact(t, app.DB, org.ID, "Analytics User", false, false, perms)
	user := testutil.CreateTestUser(t, app.DB, org.ID, testutil.WithEmail(testutil.UniqueEmail("update-widget")), testutil.WithPassword("password"), testutil.WithRoleID(&role.ID))
	widget := createTestWidget(t, app, org.ID, &user.ID, "Original Name", true, false)

	req := testutil.NewJSONRequest(t, map[string]any{
		"name":        "Updated Name",
		"description": "Updated description",
		"color":       "red",
	})
	testutil.SetAuthContext(req, org.ID, user.ID)
	testutil.SetPathParam(req, "id", widget.ID.String())

	err := app.UpdateWidget(req)
	require.NoError(t, err)
	assert.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req))

	var resp struct {
		Data handlers.WidgetResponse `json:"data"`
	}
	err = json.Unmarshal(testutil.GetResponseBody(req), &resp)
	require.NoError(t, err)
	assert.Equal(t, "Updated Name", resp.Data.Name)
	assert.Equal(t, "red", resp.Data.Color)
}

func TestApp_UpdateWidget_NoPermission(t *testing.T) {
	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	perms := getAnalyticsPermissions(t, app)
	role := testutil.CreateTestRoleExact(t, app.DB, org.ID, "Analytics User", false, false, perms)
	owner := testutil.CreateTestUser(t, app.DB, org.ID, testutil.WithEmail(testutil.UniqueEmail("owner-update")), testutil.WithPassword("password"), testutil.WithRoleID(&role.ID))
	// User without analytics write permission
	readOnlyRole := testutil.CreateTestRoleExact(t, app.DB, org.ID, "Read Only", false, false, perms[:1])
	otherUser := testutil.CreateTestUser(t, app.DB, org.ID, testutil.WithEmail(testutil.UniqueEmail("no-perm-update")), testutil.WithPassword("password"), testutil.WithRoleID(&readOnlyRole.ID))

	widget := createTestWidget(t, app, org.ID, &owner.ID, "Test Widget", true, false)

	req := testutil.NewJSONRequest(t, map[string]any{
		"name": "Updated Name",
	})
	testutil.SetAuthContext(req, org.ID, otherUser.ID)
	testutil.SetPathParam(req, "id", widget.ID.String())

	err := app.UpdateWidget(req)
	require.NoError(t, err)
	assert.Equal(t, fasthttp.StatusForbidden, testutil.GetResponseStatusCode(req))
}

func TestApp_UpdateWidget_OnlyOwnerCanEdit(t *testing.T) {
	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	perms := getAnalyticsPermissions(t, app)
	role := testutil.CreateTestRoleExact(t, app.DB, org.ID, "Analytics User", false, false, perms)
	owner := testutil.CreateTestUser(t, app.DB, org.ID, testutil.WithEmail(testutil.UniqueEmail("owner-only")), testutil.WithPassword("password"), testutil.WithRoleID(&role.ID))
	otherUser := testutil.CreateTestUser(t, app.DB, org.ID, testutil.WithEmail(testutil.UniqueEmail("other-user")), testutil.WithPassword("password"), testutil.WithRoleID(&role.ID))

	// Create widget owned by 'owner'
	widget := createTestWidget(t, app, org.ID, &owner.ID, "Owner Widget", true, false)

	// Other user (with write permission) should NOT be able to edit
	req := testutil.NewJSONRequest(t, map[string]any{
		"name": "Attempted Update",
	})
	testutil.SetAuthContext(req, org.ID, otherUser.ID)
	testutil.SetPathParam(req, "id", widget.ID.String())

	err := app.UpdateWidget(req)
	require.NoError(t, err)
	assert.Equal(t, fasthttp.StatusForbidden, testutil.GetResponseStatusCode(req))
}

func TestApp_UpdateWidget_NotFound(t *testing.T) {
	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	perms := getAnalyticsPermissions(t, app)
	role := testutil.CreateTestRoleExact(t, app.DB, org.ID, "Analytics User", false, false, perms)
	user := testutil.CreateTestUser(t, app.DB, org.ID, testutil.WithEmail(testutil.UniqueEmail("update-not-found")), testutil.WithPassword("password"), testutil.WithRoleID(&role.ID))

	req := testutil.NewJSONRequest(t, map[string]any{
		"name": "Updated",
	})
	testutil.SetAuthContext(req, org.ID, user.ID)
	testutil.SetPathParam(req, "id", uuid.New().String())

	err := app.UpdateWidget(req)
	require.NoError(t, err)
	assert.Equal(t, fasthttp.StatusNotFound, testutil.GetResponseStatusCode(req))
}

// --- DeleteDashboardWidget Tests ---

func TestApp_DeleteWidget_Success(t *testing.T) {
	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	perms := getAnalyticsPermissions(t, app)
	role := testutil.CreateTestRoleExact(t, app.DB, org.ID, "Analytics User", false, false, perms)
	user := testutil.CreateTestUser(t, app.DB, org.ID, testutil.WithEmail(testutil.UniqueEmail("delete-widget")), testutil.WithPassword("password"), testutil.WithRoleID(&role.ID))
	widget := createTestWidget(t, app, org.ID, &user.ID, "To Delete", true, false)

	req := testutil.NewGETRequest(t)
	testutil.SetAuthContext(req, org.ID, user.ID)
	testutil.SetPathParam(req, "id", widget.ID.String())

	err := app.DeleteWidget(req)
	require.NoError(t, err)
	assert.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req))

	// Verify widget is deleted
	var count int64
	app.DB.Model(&models.Widget{}).Where("id = ?", widget.ID).Count(&count)
	assert.Equal(t, int64(0), count)
}

func TestApp_DeleteWidget_NoPermission(t *testing.T) {
	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	perms := getAnalyticsPermissions(t, app)
	role := testutil.CreateTestRoleExact(t, app.DB, org.ID, "Analytics User", false, false, perms)
	owner := testutil.CreateTestUser(t, app.DB, org.ID, testutil.WithEmail(testutil.UniqueEmail("owner-del")), testutil.WithPassword("password"), testutil.WithRoleID(&role.ID))
	// User without analytics delete permission (only read and write)
	limitedRole := testutil.CreateTestRoleExact(t, app.DB, org.ID, "Limited", false, false, perms[:2])
	otherUser := testutil.CreateTestUser(t, app.DB, org.ID, testutil.WithEmail(testutil.UniqueEmail("no-del-perm")), testutil.WithPassword("password"), testutil.WithRoleID(&limitedRole.ID))

	widget := createTestWidget(t, app, org.ID, &owner.ID, "Test Widget", true, false)

	req := testutil.NewGETRequest(t)
	testutil.SetAuthContext(req, org.ID, otherUser.ID)
	testutil.SetPathParam(req, "id", widget.ID.String())

	err := app.DeleteWidget(req)
	require.NoError(t, err)
	assert.Equal(t, fasthttp.StatusForbidden, testutil.GetResponseStatusCode(req))
}

func TestApp_DeleteWidget_OnlyOwnerCanDelete(t *testing.T) {
	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	perms := getAnalyticsPermissions(t, app)
	role := testutil.CreateTestRoleExact(t, app.DB, org.ID, "Analytics User", false, false, perms)
	owner := testutil.CreateTestUser(t, app.DB, org.ID, testutil.WithEmail(testutil.UniqueEmail("owner-del-only")), testutil.WithPassword("password"), testutil.WithRoleID(&role.ID))
	otherUser := testutil.CreateTestUser(t, app.DB, org.ID, testutil.WithEmail(testutil.UniqueEmail("other-del-only")), testutil.WithPassword("password"), testutil.WithRoleID(&role.ID))

	// Create widget owned by 'owner'
	widget := createTestWidget(t, app, org.ID, &owner.ID, "Owner Widget", true, false)

	// Other user (with delete permission) should NOT be able to delete someone else's widget
	req := testutil.NewGETRequest(t)
	testutil.SetAuthContext(req, org.ID, otherUser.ID)
	testutil.SetPathParam(req, "id", widget.ID.String())

	err := app.DeleteWidget(req)
	require.NoError(t, err)
	assert.Equal(t, fasthttp.StatusForbidden, testutil.GetResponseStatusCode(req))

	// Widget should still exist
	var count int64
	app.DB.Model(&models.Widget{}).Where("id = ?", widget.ID).Count(&count)
	assert.Equal(t, int64(1), count)
}

func TestApp_DeleteWidget_NotFound(t *testing.T) {
	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	perms := getAnalyticsPermissions(t, app)
	role := testutil.CreateTestRoleExact(t, app.DB, org.ID, "Analytics User", false, false, perms)
	user := testutil.CreateTestUser(t, app.DB, org.ID, testutil.WithEmail(testutil.UniqueEmail("delete-not-found")), testutil.WithPassword("password"), testutil.WithRoleID(&role.ID))

	req := testutil.NewGETRequest(t)
	testutil.SetAuthContext(req, org.ID, user.ID)
	testutil.SetPathParam(req, "id", uuid.New().String())

	err := app.DeleteWidget(req)
	require.NoError(t, err)
	assert.Equal(t, fasthttp.StatusNotFound, testutil.GetResponseStatusCode(req))
}

// --- SaveWidgetLayout Tests ---

func TestApp_SaveWidgetLayout_Success(t *testing.T) {
	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	role := testutil.CreateTestRoleWithKeys(t, app.DB, org.ID, "Widget layout writer", []string{
		models.ResourceAnalytics + ":" + models.ActionWrite,
	})
	user := testutil.CreateTestUser(
		t,
		app.DB,
		org.ID,
		testutil.WithEmail(testutil.UniqueEmail("save-widget-layout")),
		testutil.WithRoleID(&role.ID),
	)
	widget := createTestWidget(t, app, org.ID, &user.ID, "Owned widget", false, false)

	req := testutil.NewJSONRequest(t, map[string]any{
		"layout": []map[string]any{{
			"id":     widget.ID,
			"grid_x": 2,
			"grid_y": 3,
			"grid_w": 4,
			"grid_h": 5,
		}},
	})
	testutil.SetAuthContext(req, org.ID, user.ID)

	require.NoError(t, app.SaveWidgetLayout(req))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req))

	var stored models.Widget
	require.NoError(t, app.DB.First(&stored, "id = ?", widget.ID).Error)
	assert.Equal(t, 2, stored.GridX)
	assert.Equal(t, 3, stored.GridY)
	assert.Equal(t, 4, stored.GridW)
	assert.Equal(t, 5, stored.GridH)
	assert.Equal(t, 0, stored.DisplayOrder)
}

func TestApp_SaveWidgetLayout_RequiresWritePermission(t *testing.T) {
	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	role := testutil.CreateTestRoleWithKeys(t, app.DB, org.ID, "Widget layout reader", []string{
		models.ResourceAnalytics + ":" + models.ActionRead,
	})
	user := testutil.CreateTestUser(
		t,
		app.DB,
		org.ID,
		testutil.WithEmail(testutil.UniqueEmail("save-widget-layout-read-only")),
		testutil.WithRoleID(&role.ID),
	)
	widget := createTestWidget(t, app, org.ID, &user.ID, "Read-only widget", false, false)

	req := testutil.NewJSONRequest(t, map[string]any{
		"layout": []map[string]any{{
			"id":     widget.ID,
			"grid_x": 2,
			"grid_y": 3,
			"grid_w": 4,
			"grid_h": 5,
		}},
	})
	testutil.SetAuthContext(req, org.ID, user.ID)

	require.NoError(t, app.SaveWidgetLayout(req))
	require.Equal(t, fasthttp.StatusForbidden, testutil.GetResponseStatusCode(req))

	var stored models.Widget
	require.NoError(t, app.DB.First(&stored, "id = ?", widget.ID).Error)
	assert.Zero(t, stored.GridX)
	assert.Zero(t, stored.GridY)
	assert.Zero(t, stored.GridW)
	assert.Zero(t, stored.GridH)
}

func TestApp_SaveWidgetLayout_SharedWidgetOwnedByAnotherUserRollsBack(t *testing.T) {
	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	role := testutil.CreateTestRoleWithKeys(t, app.DB, org.ID, "Widget layout owners", []string{
		models.ResourceAnalytics + ":" + models.ActionWrite,
	})
	owner := testutil.CreateTestUser(
		t,
		app.DB,
		org.ID,
		testutil.WithEmail(testutil.UniqueEmail("save-widget-layout-owner")),
		testutil.WithRoleID(&role.ID),
	)
	otherOwner := testutil.CreateTestUser(
		t,
		app.DB,
		org.ID,
		testutil.WithEmail(testutil.UniqueEmail("save-widget-layout-other-owner")),
		testutil.WithRoleID(&role.ID),
	)
	ownedWidget := createTestWidget(t, app, org.ID, &owner.ID, "Owned layout widget", false, false)
	sharedWidget := createTestWidget(t, app, org.ID, &otherOwner.ID, "Other shared widget", true, false)

	req := testutil.NewJSONRequest(t, map[string]any{
		"layout": []map[string]any{
			{
				"id":     ownedWidget.ID,
				"grid_x": 1,
				"grid_y": 2,
				"grid_w": 3,
				"grid_h": 4,
			},
			{
				"id":     sharedWidget.ID,
				"grid_x": 5,
				"grid_y": 6,
				"grid_w": 7,
				"grid_h": 8,
			},
		},
	})
	testutil.SetAuthContext(req, org.ID, owner.ID)

	require.NoError(t, app.SaveWidgetLayout(req))
	require.Equal(t, fasthttp.StatusNotFound, testutil.GetResponseStatusCode(req))

	var storedOwned, storedShared models.Widget
	require.NoError(t, app.DB.First(&storedOwned, "id = ?", ownedWidget.ID).Error)
	require.NoError(t, app.DB.First(&storedShared, "id = ?", sharedWidget.ID).Error)
	assert.Zero(t, storedOwned.GridX)
	assert.Zero(t, storedOwned.GridY)
	assert.Zero(t, storedOwned.GridW)
	assert.Zero(t, storedOwned.GridH)
	assert.Zero(t, storedShared.GridX)
	assert.Zero(t, storedShared.GridY)
	assert.Zero(t, storedShared.GridW)
	assert.Zero(t, storedShared.GridH)
}

func TestApp_SaveWidgetLayout_UnknownWidgetRollsBack(t *testing.T) {
	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	role := testutil.CreateTestRoleWithKeys(t, app.DB, org.ID, "Widget layout rollback writer", []string{
		models.ResourceAnalytics + ":" + models.ActionWrite,
	})
	user := testutil.CreateTestUser(
		t,
		app.DB,
		org.ID,
		testutil.WithEmail(testutil.UniqueEmail("save-widget-layout-rollback")),
		testutil.WithRoleID(&role.ID),
	)
	widget := createTestWidget(t, app, org.ID, &user.ID, "Rollback widget", false, false)

	req := testutil.NewJSONRequest(t, map[string]any{
		"layout": []map[string]any{
			{
				"id":     widget.ID,
				"grid_x": 1,
				"grid_y": 2,
				"grid_w": 3,
				"grid_h": 4,
			},
			{
				"id":     uuid.New(),
				"grid_x": 5,
				"grid_y": 6,
				"grid_w": 7,
				"grid_h": 8,
			},
		},
	})
	testutil.SetAuthContext(req, org.ID, user.ID)

	require.NoError(t, app.SaveWidgetLayout(req))
	require.Equal(t, fasthttp.StatusNotFound, testutil.GetResponseStatusCode(req))

	var stored models.Widget
	require.NoError(t, app.DB.First(&stored, "id = ?", widget.ID).Error)
	assert.Zero(t, stored.GridX)
	assert.Zero(t, stored.GridY)
	assert.Zero(t, stored.GridW)
	assert.Zero(t, stored.GridH)
}

// --- Cross-Organization Isolation Tests ---

func TestApp_Widget_CrossOrgIsolation(t *testing.T) {
	app := newTestApp(t)

	org1 := testutil.CreateTestOrganization(t, app.DB)
	org2 := testutil.CreateTestOrganization(t, app.DB)

	perms := getAnalyticsPermissions(t, app)
	role1 := testutil.CreateTestRoleExact(t, app.DB, org1.ID, "Analytics User 1", false, false, perms)
	role2 := testutil.CreateTestRoleExact(t, app.DB, org2.ID, "Analytics User 2", false, false, perms)

	user1 := testutil.CreateTestUser(t, app.DB, org1.ID, testutil.WithEmail(testutil.UniqueEmail("cross-widget-1")), testutil.WithPassword("password"), testutil.WithRoleID(&role1.ID))
	user2 := testutil.CreateTestUser(t, app.DB, org2.ID, testutil.WithEmail(testutil.UniqueEmail("cross-widget-2")), testutil.WithPassword("password"), testutil.WithRoleID(&role2.ID))

	// Create widget in org1
	widget1 := createTestWidget(t, app, org1.ID, &user1.ID, "Org1 Widget", true, false)

	// User from org2 tries to access org1's widget
	req := testutil.NewGETRequest(t)
	testutil.SetAuthContext(req, org2.ID, user2.ID)
	testutil.SetPathParam(req, "id", widget1.ID.String())

	err := app.GetWidget(req)
	require.NoError(t, err)
	assert.Equal(t, fasthttp.StatusNotFound, testutil.GetResponseStatusCode(req))
}

func TestApp_Widget_CrossOrg_CannotDelete(t *testing.T) {
	app := newTestApp(t)

	org1 := testutil.CreateTestOrganization(t, app.DB)
	org2 := testutil.CreateTestOrganization(t, app.DB)

	perms := getAnalyticsPermissions(t, app)
	role1 := testutil.CreateTestRoleExact(t, app.DB, org1.ID, "Analytics User 1", false, false, perms)
	role2 := testutil.CreateTestRoleExact(t, app.DB, org2.ID, "Analytics User 2", false, false, perms)

	user1 := testutil.CreateTestUser(t, app.DB, org1.ID, testutil.WithEmail(testutil.UniqueEmail("cross-del-1")), testutil.WithPassword("password"), testutil.WithRoleID(&role1.ID))
	user2 := testutil.CreateTestUser(t, app.DB, org2.ID, testutil.WithEmail(testutil.UniqueEmail("cross-del-2")), testutil.WithPassword("password"), testutil.WithRoleID(&role2.ID))

	// Create widget in org1
	widget1 := createTestWidget(t, app, org1.ID, &user1.ID, "Org1 Widget", true, false)

	// User from org2 tries to delete org1's widget
	req := testutil.NewGETRequest(t)
	testutil.SetAuthContext(req, org2.ID, user2.ID)
	testutil.SetPathParam(req, "id", widget1.ID.String())

	err := app.DeleteWidget(req)
	require.NoError(t, err)
	assert.Equal(t, fasthttp.StatusNotFound, testutil.GetResponseStatusCode(req))

	// Widget should still exist
	var count int64
	app.DB.Model(&models.Widget{}).Where("id = ?", widget1.ID).Count(&count)
	assert.Equal(t, int64(1), count)
}

func TestApp_CreateWidget_StrictAggregateAndIdentifierValidation(t *testing.T) {
	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	perms := getAnalyticsPermissions(t, app)
	role := testutil.CreateTestRoleExact(t, app.DB, org.ID, "Widget validator", false, false, perms)
	user := testutil.CreateTestUser(
		t,
		app.DB,
		org.ID,
		testutil.WithEmail(testutil.UniqueEmail("widget-validation")),
		testutil.WithPassword("password"),
		testutil.WithRoleID(&role.ID),
	)

	validAggregates := []struct {
		source string
		field  string
	}{
		{source: "crm_leads", field: "value_minor"},
		{source: "invoices", field: "total_minor"},
		{source: "invoices", field: "due_minor"},
		{source: "payments", field: "amount_minor"},
		{source: "packages", field: "available_credits"},
	}
	for _, testCase := range validAggregates {
		t.Run("valid_"+testCase.source+"_"+testCase.field, func(t *testing.T) {
			req := testutil.NewJSONRequest(t, map[string]any{
				"name":         "Valid aggregate " + uuid.NewString()[:8],
				"data_source":  testCase.source,
				"metric":       "sum",
				"field":        testCase.field,
				"display_type": "number",
			})
			testutil.SetAuthContext(req, org.ID, user.ID)
			require.NoError(t, app.CreateWidget(req))
			assert.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req))
		})
	}

	invalidDefinitions := []struct {
		name    string
		payload map[string]any
	}{
		{
			name: "aggregate field required",
			payload: map[string]any{
				"data_source": "crm_leads",
				"metric":      "sum",
			},
		},
		{
			name: "aggregate identifier injection",
			payload: map[string]any{
				"data_source": "crm_leads",
				"metric":      "sum",
				"field":       "value_minor) FROM users --",
			},
		},
		{
			name: "group identifier injection",
			payload: map[string]any{
				"data_source":    "crm_leads",
				"metric":         "count",
				"display_type":   "chart",
				"group_by_field": "status, organization_id",
			},
		},
		{
			name: "filter identifier injection",
			payload: map[string]any{
				"data_source": "crm_leads",
				"metric":      "count",
				"filters": []map[string]any{{
					"field":    "status = 'open' OR 1",
					"operator": "equals",
					"value":    "open",
				}},
			},
		},
		{
			name: "filter operator injection",
			payload: map[string]any{
				"data_source": "crm_leads",
				"metric":      "count",
				"filters": []map[string]any{{
					"field":    "status",
					"operator": "equals) OR true --",
					"value":    "open",
				}},
			},
		},
	}
	for _, testCase := range invalidDefinitions {
		t.Run(testCase.name, func(t *testing.T) {
			testCase.payload["name"] = "Invalid aggregate " + uuid.NewString()[:8]
			req := testutil.NewJSONRequest(t, testCase.payload)
			testutil.SetAuthContext(req, org.ID, user.ID)
			require.NoError(t, app.CreateWidget(req))
			assert.Equal(t, fasthttp.StatusBadRequest, testutil.GetResponseStatusCode(req))
		})
	}
}

func TestApp_GetWidgetData_CRMValueAggregateGroupingTableAndTenantIsolation(t *testing.T) {
	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	otherOrg := testutil.CreateTestOrganization(t, app.DB)
	role := testutil.CreateTestRoleWithKeys(t, app.DB, org.ID, "CRM widget reader", []string{
		models.ResourceAnalytics + ":" + models.ActionRead,
		models.ResourceCRMLeads + ":" + models.ActionRead,
	})
	user := testutil.CreateTestUser(
		t,
		app.DB,
		org.ID,
		testutil.WithEmail(testutil.UniqueEmail("widget-crm-aggregate")),
		testutil.WithPassword("password"),
		testutil.WithRoleID(&role.ID),
	)
	enableCRMInsightsTestEntitlements(t, app, org.ID, user.ID, "crm.enabled")

	createWidgetCRMLeads(t, app, org.ID, 100, 300)
	createWidgetCRMLeads(t, app, otherOrg.ID, 999999)

	sumChart := createTestWidgetDefinition(
		t, app, org.ID, user.ID,
		"CRM sum chart", "crm_leads", "sum", "value_minor", "chart", "",
	)
	sumData, status := getTestWidgetData(t, app, org.ID, user.ID, sumChart.ID)
	require.Equal(t, fasthttp.StatusOK, status)
	assert.Equal(t, float64(400), sumData.Value)
	require.NotNil(t, sumData.ChartData)
	require.Len(t, sumData.ChartData, 1)
	assert.Equal(t, float64(400), sumData.ChartData[0].Value)
	assert.NotNil(t, sumData.DataPoints)
	assert.NotNil(t, sumData.TableRows)

	avgNumber := createTestWidgetDefinition(
		t, app, org.ID, user.ID,
		"CRM average", "crm_leads", "avg", "value_minor", "number", "",
	)
	avgData, status := getTestWidgetData(t, app, org.ID, user.ID, avgNumber.ID)
	require.Equal(t, fasthttp.StatusOK, status)
	assert.Equal(t, float64(200), avgData.Value)
	assert.NotNil(t, avgData.ChartData)
	assert.NotNil(t, avgData.DataPoints)
	assert.NotNil(t, avgData.TableRows)

	groupedTable := createTestWidgetDefinition(
		t, app, org.ID, user.ID,
		"CRM grouped table", "crm_leads", "sum", "value_minor", "table", "status",
	)
	groupedData, status := getTestWidgetData(t, app, org.ID, user.ID, groupedTable.ID)
	require.Equal(t, fasthttp.StatusOK, status)
	require.NotNil(t, groupedData.DataPoints)
	require.Len(t, groupedData.DataPoints, 2)
	groupValues := map[string]float64{}
	for _, dataPoint := range groupedData.DataPoints {
		groupValues[dataPoint.Label] = dataPoint.Value
	}
	assert.Equal(t, float64(100), groupValues[string(models.CRMLeadStatusOpen)])
	assert.Equal(t, float64(300), groupValues[string(models.CRMLeadStatusWon)])
	assert.NotNil(t, groupedData.ChartData)
	assert.NotNil(t, groupedData.TableRows)

	rowTable := createTestWidgetDefinition(
		t, app, org.ID, user.ID,
		"CRM rows", "crm_leads", "count", "", "table", "",
	)
	rowData, status := getTestWidgetData(t, app, org.ID, user.ID, rowTable.ID)
	require.Equal(t, fasthttp.StatusOK, status)
	require.NotNil(t, rowData.TableRows)
	require.Len(t, rowData.TableRows, 2)
	for _, row := range rowData.TableRows {
		assert.NotContains(t, row.Label, "999999")
	}
	assert.NotNil(t, rowData.ChartData)
	assert.NotNil(t, rowData.DataPoints)
}

func TestApp_GetWidgetData_BookingInvoicePaymentAndFinitePackageAggregates(t *testing.T) {
	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	role := testutil.CreateTestRoleWithKeys(t, app.DB, org.ID, "Commerce widget reader", []string{
		models.ResourceAnalytics + ":" + models.ActionRead,
		models.ResourceBookings + ":" + models.ActionRead,
		models.ResourcePayments + ":" + models.ActionRead,
		models.ResourcePackages + ":" + models.ActionRead,
	})
	user := testutil.CreateTestUser(
		t,
		app.DB,
		org.ID,
		testutil.WithEmail(testutil.UniqueEmail("widget-commerce-aggregate")),
		testutil.WithPassword("password"),
		testutil.WithRoleID(&role.ID),
	)
	enableCRMInsightsTestEntitlements(
		t,
		app,
		org.ID,
		user.ID,
		"bookings.enabled",
		"commerce.enabled",
	)

	now := time.Now().UTC()
	contact := testutil.CreateTestContact(t, app.DB, org.ID)
	invoices := []*models.CommerceInvoice{
		{
			BaseModel:      models.BaseModel{ID: uuid.New(), CreatedAt: now, UpdatedAt: now},
			OrganizationID: org.ID,
			ContactID:      contact.ID,
			InvoiceNumber:  "INV-" + uuid.NewString()[:8],
			IdempotencyKey: "widget-invoice-" + uuid.NewString(),
			Status:         models.CommerceInvoiceStatusOpen,
			Currency:       "MYR",
			SubtotalMinor:  1000,
			TotalMinor:     1000,
			PaidMinor:      600,
			DueMinor:       400,
			Metadata:       models.JSONB{},
			Version:        1,
		},
		{
			BaseModel:      models.BaseModel{ID: uuid.New(), CreatedAt: now, UpdatedAt: now},
			OrganizationID: org.ID,
			ContactID:      contact.ID,
			InvoiceNumber:  "INV-" + uuid.NewString()[:8],
			IdempotencyKey: "widget-invoice-" + uuid.NewString(),
			Status:         models.CommerceInvoiceStatusOpen,
			Currency:       "MYR",
			SubtotalMinor:  3000,
			TotalMinor:     3000,
			PaidMinor:      1000,
			DueMinor:       2000,
			Metadata:       models.JSONB{},
			Version:        1,
		},
	}
	for _, invoice := range invoices {
		require.NoError(t, app.DB.Create(invoice).Error)
	}

	providerAccount := &models.PaymentProviderAccount{
		BaseModel:         models.BaseModel{ID: uuid.New()},
		OrganizationID:    org.ID,
		Name:              "Widget test provider",
		Provider:          "manual-test",
		ExternalAccountID: "widget-" + uuid.NewString(),
		Environment:       models.PaymentEnvironmentTest,
		PublicConfig:      models.JSONB{},
		IsActive:          true,
		Metadata:          models.JSONB{},
		Version:           1,
	}
	require.NoError(t, app.DB.Create(providerAccount).Error)
	for _, amount := range []int64{500, 700} {
		transaction := &models.PaymentTransaction{
			ID:                    uuid.New(),
			OrganizationID:        org.ID,
			ProviderAccountID:     providerAccount.ID,
			Type:                  models.PaymentTransactionTypeCharge,
			ProviderTransactionID: "widget-txn-" + uuid.NewString(),
			IdempotencyKey:        "widget-txn-" + uuid.NewString(),
			AmountMinor:           amount,
			Currency:              "MYR",
			Status:                models.PaymentTransactionStatusSucceeded,
			Metadata:              models.JSONB{},
			OccurredAt:            now,
			CreatedAt:             now,
		}
		require.NoError(t, app.DB.Create(transaction).Error)
	}

	service := &models.BookingService{
		BaseModel:       models.BaseModel{ID: uuid.New()},
		OrganizationID:  org.ID,
		Name:            "Widget service",
		Kind:            models.BookingServiceKindAppointment,
		DurationMinutes: 30,
		DefaultCapacity: 1,
		Currency:        "MYR",
		ReminderPolicy:  models.JSONB{},
		IsActive:        true,
		Metadata:        models.JSONB{},
		Version:         1,
	}
	require.NoError(t, app.DB.Create(service).Error)
	unlimitedService := *service
	unlimitedService.ID = uuid.New()
	unlimitedService.Name = "Unlimited widget service"
	require.NoError(t, app.DB.Create(&unlimitedService).Error)
	resource := &models.BookingResource{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: org.ID,
		Name:           "Widget resource",
		Kind:           models.BookingResourceKindPractitioner,
		Timezone:       "UTC",
		IsActive:       true,
		Metadata:       models.JSONB{},
		Version:        1,
	}
	require.NoError(t, app.DB.Create(resource).Error)
	event := &models.BookingEvent{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: org.ID,
		ServiceID:      service.ID,
		ResourceID:     resource.ID,
		StartsAt:       now.Add(time.Hour),
		EndsAt:         now.Add(90 * time.Minute),
		Capacity:       1,
		Status:         models.BookingEventStatusScheduled,
		Metadata:       models.JSONB{},
		Version:        1,
	}
	require.NoError(t, app.DB.Create(event).Error)
	booking := &models.Booking{
		BaseModel:      models.BaseModel{ID: uuid.New(), CreatedAt: now, UpdatedAt: now},
		OrganizationID: org.ID,
		EventID:        event.ID,
		ContactID:      contact.ID,
		Status:         models.BookingStatusConfirmed,
		Quantity:       1,
		Source:         models.BookingSourceAgent,
		IdempotencyKey: "widget-booking-" + uuid.NewString(),
		Metadata:       models.JSONB{},
		Version:        1,
	}
	require.NoError(t, app.DB.Create(booking).Error)

	definition := &models.PackageDefinition{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: org.ID,
		Name:           "Widget package",
		PriceMinor:     1000,
		Currency:       "MYR",
		ValidityDays:   30,
		IsActive:       true,
		Metadata:       models.JSONB{},
		Version:        1,
	}
	require.NoError(t, app.DB.Create(definition).Error)
	finiteEntitlement := &models.PackageEntitlement{
		BaseModel:           models.BaseModel{ID: uuid.New()},
		OrganizationID:      org.ID,
		PackageDefinitionID: definition.ID,
		BookingServiceID:    service.ID,
		Credits:             10,
		Version:             1,
	}
	require.NoError(t, app.DB.Create(finiteEntitlement).Error)
	unlimitedEntitlement := &models.PackageEntitlement{
		BaseModel:           models.BaseModel{ID: uuid.New()},
		OrganizationID:      org.ID,
		PackageDefinitionID: definition.ID,
		BookingServiceID:    unlimitedService.ID,
		IsUnlimited:         true,
		Version:             1,
	}
	require.NoError(t, app.DB.Create(unlimitedEntitlement).Error)
	contactPackage := &models.ContactPackage{
		BaseModel:           models.BaseModel{ID: uuid.New(), CreatedAt: now, UpdatedAt: now},
		OrganizationID:      org.ID,
		ContactID:           contact.ID,
		PackageDefinitionID: definition.ID,
		Status:              models.ContactPackageStatusActive,
		PurchaseAmountMinor: 1000,
		Currency:            "MYR",
		IdempotencyKey:      "widget-package-" + uuid.NewString(),
		Metadata:            models.JSONB{},
		Version:             1,
	}
	require.NoError(t, app.DB.Create(contactPackage).Error)
	for entitlementID, available := range map[uuid.UUID]int{
		finiteEntitlement.ID:    4,
		unlimitedEntitlement.ID: 999,
	} {
		balance := &models.CreditBalance{
			BaseModel:            models.BaseModel{ID: uuid.New()},
			OrganizationID:       org.ID,
			ContactPackageID:     contactPackage.ID,
			PackageEntitlementID: entitlementID,
			Granted:              available,
			Available:            available,
			Version:              1,
		}
		require.NoError(t, app.DB.Create(balance).Error)
	}

	testCases := []struct {
		name        string
		source      string
		metric      string
		field       string
		displayType string
		want        float64
	}{
		{name: "invoice total", source: "invoices", metric: "sum", field: "total_minor", displayType: "number", want: 4000},
		{name: "invoice due average", source: "invoices", metric: "avg", field: "due_minor", displayType: "number", want: 1200},
		{name: "payment amount", source: "payments", metric: "sum", field: "amount_minor", displayType: "chart", want: 1200},
		{name: "finite package credits", source: "packages", metric: "sum", field: "available_credits", displayType: "number", want: 4},
		{name: "booking count", source: "bookings", metric: "count", displayType: "table", want: 1},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			widget := createTestWidgetDefinition(
				t,
				app,
				org.ID,
				user.ID,
				"Widget "+testCase.name,
				testCase.source,
				testCase.metric,
				testCase.field,
				testCase.displayType,
				"",
			)
			data, status := getTestWidgetData(t, app, org.ID, user.ID, widget.ID)
			require.Equal(t, fasthttp.StatusOK, status)
			assert.NotNil(t, data.ChartData)
			assert.NotNil(t, data.DataPoints)
			assert.NotNil(t, data.TableRows)
			if testCase.displayType == "table" {
				require.Len(t, data.TableRows, int(testCase.want))
			} else {
				assert.Equal(t, testCase.want, data.Value)
			}
		})
	}
}

func TestApp_GetWidgetData_RequiresUnderlyingPermissionAndEntitlement(t *testing.T) {
	t.Run("underlying permission", func(t *testing.T) {
		app := newTestApp(t)
		org := testutil.CreateTestOrganization(t, app.DB)
		role := testutil.CreateTestRoleWithKeys(t, app.DB, org.ID, "Analytics only", []string{
			models.ResourceAnalytics + ":" + models.ActionRead,
		})
		user := testutil.CreateTestUser(
			t,
			app.DB,
			org.ID,
			testutil.WithEmail(testutil.UniqueEmail("widget-no-source-permission")),
			testutil.WithPassword("password"),
			testutil.WithRoleID(&role.ID),
		)
		widget := createTestWidgetDefinition(
			t, app, org.ID, user.ID,
			"Protected CRM widget", "crm_leads", "count", "", "number", "",
		)

		_, status := getTestWidgetData(t, app, org.ID, user.ID, widget.ID)
		assert.Equal(t, fasthttp.StatusForbidden, status)
	})

	t.Run("product entitlement", func(t *testing.T) {
		app := newTestApp(t)
		org := testutil.CreateTestOrganization(t, app.DB)
		role := testutil.CreateTestRoleWithKeys(t, app.DB, org.ID, "Unlicensed CRM reader", []string{
			models.ResourceAnalytics + ":" + models.ActionRead,
			models.ResourceCRMLeads + ":" + models.ActionRead,
		})
		user := testutil.CreateTestUser(
			t,
			app.DB,
			org.ID,
			testutil.WithEmail(testutil.UniqueEmail("widget-no-entitlement")),
			testutil.WithPassword("password"),
			testutil.WithRoleID(&role.ID),
		)
		widget := createTestWidgetDefinition(
			t, app, org.ID, user.ID,
			"Unlicensed CRM widget", "crm_leads", "count", "", "number", "",
		)

		_, status := getTestWidgetData(t, app, org.ID, user.ID, widget.ID)
		assert.Equal(t, fasthttp.StatusPaymentRequired, status)
	})
}

func TestApp_GetWidgetDataSources_OmitsUnauthorizedAndUnentitledSources(t *testing.T) {
	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	role := testutil.CreateTestRoleWithKeys(t, app.DB, org.ID, "CRM discovery reader", []string{
		models.ResourceAnalytics + ":" + models.ActionRead,
		models.ResourceCRMLeads + ":" + models.ActionRead,
	})
	user := testutil.CreateTestUser(
		t,
		app.DB,
		org.ID,
		testutil.WithEmail(testutil.UniqueEmail("widget-source-discovery")),
		testutil.WithPassword("password"),
		testutil.WithRoleID(&role.ID),
	)

	type sourceInfo struct {
		Name            string   `json:"name"`
		Fields          []string `json:"fields"`
		AggregateFields []string `json:"aggregate_fields"`
	}
	loadSources := func() []sourceInfo {
		req := testutil.NewGETRequest(t)
		testutil.SetAuthContext(req, org.ID, user.ID)
		require.NoError(t, app.GetWidgetDataSources(req))
		require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req))
		var response struct {
			Data struct {
				DataSources []sourceInfo `json:"data_sources"`
			} `json:"data"`
		}
		require.NoError(t, json.Unmarshal(testutil.GetResponseBody(req), &response))
		return response.Data.DataSources
	}
	findSource := func(sources []sourceInfo, name string) *sourceInfo {
		for index := range sources {
			if sources[index].Name == name {
				return &sources[index]
			}
		}
		return nil
	}

	unlicensedSources := loadSources()
	assert.Nil(t, findSource(unlicensedSources, "crm_leads"))
	assert.Nil(t, findSource(unlicensedSources, "bookings"))
	assert.Nil(t, findSource(unlicensedSources, "invoices"))
	assert.Nil(t, findSource(unlicensedSources, "payments"))
	assert.Nil(t, findSource(unlicensedSources, "packages"))

	enableCRMInsightsTestEntitlements(t, app, org.ID, user.ID, "crm.enabled")
	licensedSources := loadSources()
	crmSource := findSource(licensedSources, "crm_leads")
	require.NotNil(t, crmSource)
	assert.Equal(t, []string{"value_minor"}, crmSource.AggregateFields)
	assert.Nil(t, findSource(licensedSources, "bookings"))
	assert.Nil(t, findSource(licensedSources, "invoices"))
	assert.Nil(t, findSource(licensedSources, "payments"))
	assert.Nil(t, findSource(licensedSources, "packages"))
}

func TestApp_GetAllWidgetsData_ReportsPerWidgetAuthorizationErrorsWithoutDataLeak(t *testing.T) {
	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	role := testutil.CreateTestRoleWithKeys(t, app.DB, org.ID, "Partial widget reader", []string{
		models.ResourceAnalytics + ":" + models.ActionRead,
		models.ResourceCRMLeads + ":" + models.ActionRead,
	})
	user := testutil.CreateTestUser(
		t,
		app.DB,
		org.ID,
		testutil.WithEmail(testutil.UniqueEmail("widget-batch-access")),
		testutil.WithPassword("password"),
		testutil.WithRoleID(&role.ID),
	)
	enableCRMInsightsTestEntitlements(t, app, org.ID, user.ID, "crm.enabled")
	authorized := createTestWidgetDefinition(
		t, app, org.ID, user.ID,
		"Authorized CRM widget", "crm_leads", "count", "", "number", "",
	)
	unauthorized := createTestWidgetDefinition(
		t, app, org.ID, user.ID,
		"Unauthorized chat widget", "messages", "count", "", "number", "",
	)

	req := testutil.NewGETRequest(t)
	testutil.SetAuthContext(req, org.ID, user.ID)
	testutil.SetQueryParam(req, "from", time.Now().UTC().AddDate(0, 0, -1).Format("2006-01-02"))
	testutil.SetQueryParam(req, "to", time.Now().UTC().AddDate(0, 0, 1).Format("2006-01-02"))
	require.NoError(t, app.GetAllWidgetsData(req))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req))

	var response struct {
		Data struct {
			Data   map[string]handlers.WidgetDataResponse `json:"data"`
			Errors map[string]handlers.WidgetDataError    `json:"errors"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(testutil.GetResponseBody(req), &response))
	authorizedData, exists := response.Data.Data[authorized.ID.String()]
	require.True(t, exists)
	assert.NotNil(t, authorizedData.ChartData)
	assert.NotNil(t, authorizedData.DataPoints)
	assert.NotNil(t, authorizedData.TableRows)
	_, leaked := response.Data.Data[unauthorized.ID.String()]
	assert.False(t, leaked)
	require.Contains(t, response.Data.Errors, unauthorized.ID.String())
	assert.Equal(t, fasthttp.StatusForbidden, response.Data.Errors[unauthorized.ID.String()].Status)
}

func TestApp_MessageTableWidgetRequiresContactWideReadPermission(t *testing.T) {
	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	role := testutil.CreateTestRoleWithKeys(t, app.DB, org.ID, "Assignment-scoped chat analytics", []string{
		models.ResourceAnalytics + ":" + models.ActionRead,
		models.ResourceChat + ":" + models.ActionRead,
	})
	user := testutil.CreateTestUser(
		t,
		app.DB,
		org.ID,
		testutil.WithEmail(testutil.UniqueEmail("widget-message-table-no-contacts")),
		testutil.WithPassword("password"),
		testutil.WithRoleID(&role.ID),
	)
	numberWidget := createTestWidgetDefinition(
		t, app, org.ID, user.ID,
		"Message count", "messages", "count", "", "number", "",
	)
	tableWidget := createTestWidgetDefinition(
		t, app, org.ID, user.ID,
		"Message previews", "messages", "count", "", "table", "",
	)

	_, numberStatus := getTestWidgetData(t, app, org.ID, user.ID, numberWidget.ID)
	require.Equal(t, fasthttp.StatusOK, numberStatus)
	_, tableStatus := getTestWidgetData(t, app, org.ID, user.ID, tableWidget.ID)
	require.Equal(t, fasthttp.StatusForbidden, tableStatus)

	req := testutil.NewGETRequest(t)
	testutil.SetAuthContext(req, org.ID, user.ID)
	testutil.SetQueryParam(req, "from", time.Now().UTC().AddDate(0, 0, -1).Format("2006-01-02"))
	testutil.SetQueryParam(req, "to", time.Now().UTC().AddDate(0, 0, 1).Format("2006-01-02"))
	require.NoError(t, app.GetAllWidgetsData(req))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req))

	var response struct {
		Data struct {
			Data   map[string]handlers.WidgetDataResponse `json:"data"`
			Errors map[string]handlers.WidgetDataError    `json:"errors"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(testutil.GetResponseBody(req), &response))
	require.Contains(t, response.Data.Data, numberWidget.ID.String())
	require.NotContains(t, response.Data.Data, tableWidget.ID.String())
	require.Contains(t, response.Data.Errors, tableWidget.ID.String())
	assert.Equal(t, fasthttp.StatusForbidden, response.Data.Errors[tableWidget.ID.String()].Status)
}

func TestApp_TransferWidgetsRequireOrganizationWideWritePermission(t *testing.T) {
	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	readRole := testutil.CreateTestRoleWithKeys(t, app.DB, org.ID, "Transfer widget scoped reader", []string{
		models.ResourceAnalytics + ":" + models.ActionRead,
		models.ResourceTransfers + ":" + models.ActionRead,
	})
	readUser := testutil.CreateTestUser(
		t,
		app.DB,
		org.ID,
		testutil.WithEmail(testutil.UniqueEmail("widget-transfer-read")),
		testutil.WithPassword("password"),
		testutil.WithRoleID(&readRole.ID),
	)
	readWidget := createTestWidgetDefinition(
		t, app, org.ID, readUser.ID,
		"Scoped transfer widget", "transfers", "count", "", "number", "",
	)
	_, status := getTestWidgetData(t, app, org.ID, readUser.ID, readWidget.ID)
	require.Equal(t, fasthttp.StatusForbidden, status)

	readDiscovery := testutil.NewGETRequest(t)
	testutil.SetAuthContext(readDiscovery, org.ID, readUser.ID)
	require.NoError(t, app.GetWidgetDataSources(readDiscovery))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(readDiscovery))
	var readResponse struct {
		Data struct {
			DataSources []struct {
				Name string `json:"name"`
			} `json:"data_sources"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(testutil.GetResponseBody(readDiscovery), &readResponse))
	for _, source := range readResponse.Data.DataSources {
		require.NotEqual(t, "transfers", source.Name)
	}

	writeRole := testutil.CreateTestRoleWithKeys(t, app.DB, org.ID, "Transfer widget full reader", []string{
		models.ResourceAnalytics + ":" + models.ActionRead,
		models.ResourceTransfers + ":" + models.ActionWrite,
	})
	writeUser := testutil.CreateTestUser(
		t,
		app.DB,
		org.ID,
		testutil.WithEmail(testutil.UniqueEmail("widget-transfer-write")),
		testutil.WithPassword("password"),
		testutil.WithRoleID(&writeRole.ID),
	)
	writeWidget := createTestWidgetDefinition(
		t, app, org.ID, writeUser.ID,
		"Full transfer widget", "transfers", "count", "", "number", "",
	)
	data, status := getTestWidgetData(t, app, org.ID, writeUser.ID, writeWidget.ID)
	require.Equal(t, fasthttp.StatusOK, status)
	require.Zero(t, data.Value)

	writeDiscovery := testutil.NewGETRequest(t)
	testutil.SetAuthContext(writeDiscovery, org.ID, writeUser.ID)
	require.NoError(t, app.GetWidgetDataSources(writeDiscovery))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(writeDiscovery))
	var writeResponse struct {
		Data struct {
			DataSources []struct {
				Name string `json:"name"`
			} `json:"data_sources"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(testutil.GetResponseBody(writeDiscovery), &writeResponse))
	foundTransfers := false
	for _, source := range writeResponse.Data.DataSources {
		if source.Name == "transfers" {
			foundTransfers = true
			break
		}
	}
	require.True(t, foundTransfers)
}

func TestApp_WidgetTableRowsMaskContactPhoneLabels(t *testing.T) {
	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	require.NoError(t, app.DB.Model(org).Update(
		"settings",
		models.JSONB{"mask_phone_numbers": true},
	).Error)
	role := testutil.CreateTestRoleWithKeys(t, app.DB, org.ID, "Masked widget reader", []string{
		models.ResourceAnalytics + ":" + models.ActionRead,
		models.ResourceContacts + ":" + models.ActionRead,
		models.ResourceChat + ":" + models.ActionRead,
		models.ResourceTransfers + ":" + models.ActionWrite,
		models.ResourceFlowsChatbot + ":" + models.ActionRead,
	})
	user := testutil.CreateTestUser(
		t,
		app.DB,
		org.ID,
		testutil.WithEmail(testutil.UniqueEmail("widget-mask")),
		testutil.WithPassword("password"),
		testutil.WithRoleID(&role.ID),
	)
	const rawPhone = "+60123456789"
	now := time.Now().UTC()
	contact := testutil.CreateTestContactWith(
		t,
		app.DB,
		org.ID,
		testutil.WithPhoneNumber(rawPhone),
	)
	require.NoError(t, app.DB.Model(contact).Updates(map[string]any{
		"profile_name":    rawPhone,
		"last_message_at": now,
	}).Error)
	message := &models.Message{
		BaseModel:         models.BaseModel{ID: uuid.New(), CreatedAt: now, UpdatedAt: now},
		OrganizationID:    org.ID,
		WhatsAppAccount:   "widget-mask-account",
		ContactID:         contact.ID,
		WhatsAppMessageID: "widget-mask-" + uuid.NewString(),
		Direction:         models.DirectionIncoming,
		MessageType:       models.MessageTypeText,
		Content:           "Message content remains visible",
		Status:            models.MessageStatusReceived,
		Metadata:          models.JSONB{},
	}
	require.NoError(t, app.DB.Create(message).Error)
	transfer := &models.AgentTransfer{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: org.ID,
		ContactID:      contact.ID,
		PhoneNumber:    rawPhone,
		Status:         models.TransferStatusActive,
		Source:         models.TransferSourceManual,
		TransferredAt:  now,
	}
	require.NoError(t, app.DB.Create(transfer).Error)
	session := &models.ChatbotSession{
		BaseModel:       models.BaseModel{ID: uuid.New(), CreatedAt: now, UpdatedAt: now},
		OrganizationID:  org.ID,
		ContactID:       contact.ID,
		WhatsAppAccount: "widget-mask-account",
		PhoneNumber:     rawPhone,
		Status:          models.SessionStatusActive,
		SessionData:     models.JSONB{},
		StartedAt:       now,
		LastActivityAt:  now,
	}
	require.NoError(t, app.DB.Create(session).Error)

	testCases := []struct {
		source       string
		wantSubLabel string
		maskSubLabel bool
	}{
		{source: "contacts", maskSubLabel: true},
		{source: "messages", wantSubLabel: message.Content},
		{source: "transfers", wantSubLabel: string(models.TransferSourceManual)},
		{source: "sessions", wantSubLabel: string(models.SessionStatusActive)},
	}
	for _, testCase := range testCases {
		t.Run(testCase.source, func(t *testing.T) {
			widget := createTestWidgetDefinition(
				t,
				app,
				org.ID,
				user.ID,
				"Masked "+testCase.source+" table",
				testCase.source,
				"count",
				"",
				"table",
				"",
			)
			data, status := getTestWidgetData(t, app, org.ID, user.ID, widget.ID)
			require.Equal(t, fasthttp.StatusOK, status)
			require.Len(t, data.TableRows, 1)
			require.NotEqual(t, rawPhone, data.TableRows[0].Label)
			require.NotContains(t, data.TableRows[0].Label, rawPhone)
			if testCase.maskSubLabel {
				require.NotEqual(t, rawPhone, data.TableRows[0].SubLabel)
			} else {
				require.Equal(t, testCase.wantSubLabel, data.TableRows[0].SubLabel)
			}
		})
	}
}
