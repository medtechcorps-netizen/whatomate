package handlers_test

import (
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/handlers"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
	"github.com/zerodha/fastglue"
)

func createRoleOperator(
	t *testing.T,
	app *handlers.App,
	orgID uuid.UUID,
	permissionKeys ...string,
) *models.User {
	t.Helper()
	role := testutil.CreateTestRoleWithKeys(t, app.DB, orgID, "role-operator", permissionKeys)
	return testutil.CreateTestUser(t, app.DB, orgID, testutil.WithRoleID(&role.ID))
}

func TestApp_ListRoles_Success(t *testing.T) {
	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	permissions := testutil.GetOrCreateTestPermissions(t, app.DB)

	// Create some roles
	adminRole := testutil.CreateTestRoleExact(t, app.DB, org.ID, "Admin", true, false, permissions)
	agentRole := testutil.CreateTestRoleExact(t, app.DB, org.ID, "Agent", false, true, permissions[:3])

	// Create a user to make the request
	user := testutil.CreateTestUser(t, app.DB, org.ID, testutil.WithEmail(testutil.UniqueEmail("list-roles")), testutil.WithRoleID(&adminRole.ID))

	req := testutil.NewGETRequest(t)
	req.RequestCtx.SetUserValue("user_id", user.ID)
	req.RequestCtx.SetUserValue("organization_id", org.ID)

	err := app.ListRoles(req)
	require.NoError(t, err)
	assert.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req))

	var resp struct {
		Status string `json:"status"`
		Data   struct {
			Roles []handlers.RoleResponse `json:"roles"`
		} `json:"data"`
	}
	err = json.Unmarshal(testutil.GetResponseBody(req), &resp)
	require.NoError(t, err)

	assert.Equal(t, "success", resp.Status)
	assert.Len(t, resp.Data.Roles, 2)

	// Check that roles are sorted (system first, then by name)
	assert.Equal(t, adminRole.Name, resp.Data.Roles[0].Name)
	assert.True(t, resp.Data.Roles[0].IsSystem)
	assert.Equal(t, agentRole.Name, resp.Data.Roles[1].Name)
	assert.True(t, resp.Data.Roles[1].IsDefault)
}

func TestApp_GetRole_Success(t *testing.T) {
	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	permissions := testutil.GetOrCreateTestPermissions(t, app.DB)

	role := testutil.CreateTestRoleExact(t, app.DB, org.ID, "Test Role", false, false, permissions[:2])
	user := createRoleOperator(t, app, org.ID, "roles:read")

	req := testutil.NewGETRequest(t)
	req.RequestCtx.SetUserValue("user_id", user.ID)
	req.RequestCtx.SetUserValue("organization_id", org.ID)
	req.RequestCtx.SetUserValue("id", role.ID.String())

	err := app.GetRole(req)
	require.NoError(t, err)
	assert.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req))

	var resp struct {
		Status string                `json:"status"`
		Data   handlers.RoleResponse `json:"data"`
	}
	err = json.Unmarshal(testutil.GetResponseBody(req), &resp)
	require.NoError(t, err)

	assert.Equal(t, "success", resp.Status)
	assert.Equal(t, role.ID, resp.Data.ID)
	assert.Equal(t, role.Name, resp.Data.Name)
	assert.Len(t, resp.Data.Permissions, 2)
}

func TestApp_GetRole_NotFound(t *testing.T) {
	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	user := createRoleOperator(t, app, org.ID, "roles:read")

	req := testutil.NewGETRequest(t)
	req.RequestCtx.SetUserValue("user_id", user.ID)
	req.RequestCtx.SetUserValue("organization_id", org.ID)
	req.RequestCtx.SetUserValue("id", uuid.New().String())

	err := app.GetRole(req)
	require.NoError(t, err)
	assert.Equal(t, fasthttp.StatusNotFound, testutil.GetResponseStatusCode(req))
}

func TestApp_CreateRole_Success(t *testing.T) {
	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	permissions := testutil.GetOrCreateTestPermissions(t, app.DB)
	user := createRoleOperator(t, app, org.ID, "roles:write")

	reqBody := handlers.RoleRequest{
		Name:        "New Role",
		Description: "A new custom role",
		IsDefault:   false,
		Permissions: []string{"users:read", "users:write"},
	}

	req := testutil.NewJSONRequest(t, reqBody)
	req.RequestCtx.SetUserValue("user_id", user.ID)
	req.RequestCtx.SetUserValue("organization_id", org.ID)

	err := app.CreateRole(req)
	require.NoError(t, err)
	assert.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req))

	var resp struct {
		Status string                `json:"status"`
		Data   handlers.RoleResponse `json:"data"`
	}
	err = json.Unmarshal(testutil.GetResponseBody(req), &resp)
	require.NoError(t, err)

	assert.Equal(t, "success", resp.Status)
	assert.Equal(t, "New Role", resp.Data.Name)
	assert.Equal(t, "A new custom role", resp.Data.Description)
	assert.False(t, resp.Data.IsSystem)
	assert.Len(t, resp.Data.Permissions, 2)

	// Verify permissions were assigned correctly
	var dbRole models.CustomRole
	require.NoError(t, app.DB.Preload("Permissions").First(&dbRole, "id = ?", resp.Data.ID).Error)
	assert.Len(t, dbRole.Permissions, 2)

	// Clean up permissions for next test
	_ = permissions
}

func TestApp_CreateRole_DuplicateName(t *testing.T) {
	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	_ = testutil.GetOrCreateTestPermissions(t, app.DB)

	testutil.CreateTestRoleExact(t, app.DB, org.ID, "Existing Role", false, false, nil)
	user := createRoleOperator(t, app, org.ID, "roles:write")

	reqBody := handlers.RoleRequest{
		Name:        "Existing Role",
		Description: "Trying to create duplicate",
		Permissions: []string{},
	}

	req := testutil.NewJSONRequest(t, reqBody)
	req.RequestCtx.SetUserValue("user_id", user.ID)
	req.RequestCtx.SetUserValue("organization_id", org.ID)

	err := app.CreateRole(req)
	require.NoError(t, err)
	assert.Equal(t, fasthttp.StatusConflict, testutil.GetResponseStatusCode(req))
}

func TestApp_CreateRole_MissingName(t *testing.T) {
	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	user := createRoleOperator(t, app, org.ID, "roles:write")

	reqBody := handlers.RoleRequest{
		Name:        "",
		Description: "Role without name",
		Permissions: []string{},
	}

	req := testutil.NewJSONRequest(t, reqBody)
	req.RequestCtx.SetUserValue("user_id", user.ID)
	req.RequestCtx.SetUserValue("organization_id", org.ID)

	err := app.CreateRole(req)
	require.NoError(t, err)
	assert.Equal(t, fasthttp.StatusBadRequest, testutil.GetResponseStatusCode(req))
}

func TestApp_CreateRole_WithDefaultFlag(t *testing.T) {
	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	_ = testutil.GetOrCreateTestPermissions(t, app.DB)

	// Create an existing default role
	existingDefault := testutil.CreateTestRoleExact(t, app.DB, org.ID, "Old Default", false, true, nil)
	user := createRoleOperator(t, app, org.ID, "roles:write")

	reqBody := handlers.RoleRequest{
		Name:        "New Default Role",
		Description: "This will be the new default",
		IsDefault:   true,
		Permissions: []string{},
	}

	req := testutil.NewJSONRequest(t, reqBody)
	req.RequestCtx.SetUserValue("user_id", user.ID)
	req.RequestCtx.SetUserValue("organization_id", org.ID)

	err := app.CreateRole(req)
	require.NoError(t, err)
	assert.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req))

	// Verify the old default was unset
	var oldDefault models.CustomRole
	require.NoError(t, app.DB.First(&oldDefault, "id = ?", existingDefault.ID).Error)
	assert.False(t, oldDefault.IsDefault)
}

func TestApp_UpdateRole_Success(t *testing.T) {
	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	permissions := testutil.GetOrCreateTestPermissions(t, app.DB)

	role := testutil.CreateTestRoleExact(t, app.DB, org.ID, "Editable Role", false, false, permissions[:1])
	user := createRoleOperator(t, app, org.ID, "roles:write")

	reqBody := handlers.RoleRequest{
		Name:        "Updated Role Name",
		Description: "Updated description",
		Permissions: []string{"users:read", "users:write", "contacts:read"},
	}

	req := testutil.NewJSONRequest(t, reqBody)
	req.RequestCtx.SetUserValue("user_id", user.ID)
	req.RequestCtx.SetUserValue("organization_id", org.ID)
	req.RequestCtx.SetUserValue("id", role.ID.String())

	err := app.UpdateRole(req)
	require.NoError(t, err)
	assert.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req))

	var resp struct {
		Status string                `json:"status"`
		Data   handlers.RoleResponse `json:"data"`
	}
	err = json.Unmarshal(testutil.GetResponseBody(req), &resp)
	require.NoError(t, err)

	assert.Equal(t, "Updated Role Name", resp.Data.Name)
	assert.Equal(t, "Updated description", resp.Data.Description)
	assert.Len(t, resp.Data.Permissions, 3)
}

func TestApp_UpdateRole_SystemRoleOnlyDescription(t *testing.T) {
	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	permissions := testutil.GetOrCreateTestPermissions(t, app.DB)

	// Create a system role
	systemRole := testutil.CreateTestRoleExact(t, app.DB, org.ID, "System Admin", true, false, permissions)
	user := createRoleOperator(t, app, org.ID, "roles:write")

	reqBody := handlers.RoleRequest{
		Name:        "Changed Name",         // Should be ignored for system roles
		Description: "Updated description",  // Only this should be updated
		Permissions: []string{"users:read"}, // Should be ignored for system roles
	}

	req := testutil.NewJSONRequest(t, reqBody)
	req.RequestCtx.SetUserValue("user_id", user.ID)
	req.RequestCtx.SetUserValue("organization_id", org.ID)
	req.RequestCtx.SetUserValue("id", systemRole.ID.String())

	err := app.UpdateRole(req)
	require.NoError(t, err)
	assert.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req))

	var resp struct {
		Status string                `json:"status"`
		Data   handlers.RoleResponse `json:"data"`
	}
	err = json.Unmarshal(testutil.GetResponseBody(req), &resp)
	require.NoError(t, err)

	// Name should not change for system roles
	assert.Equal(t, "System Admin", resp.Data.Name)
	assert.Equal(t, "Updated description", resp.Data.Description)
	// Permissions should remain the same
	assert.Len(t, resp.Data.Permissions, len(permissions))
}

func TestApp_UpdateRole_NotFound(t *testing.T) {
	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	user := createRoleOperator(t, app, org.ID, "roles:write")

	reqBody := handlers.RoleRequest{
		Name: "Updated Name",
	}

	req := testutil.NewJSONRequest(t, reqBody)
	req.RequestCtx.SetUserValue("user_id", user.ID)
	req.RequestCtx.SetUserValue("organization_id", org.ID)
	req.RequestCtx.SetUserValue("id", uuid.New().String())

	err := app.UpdateRole(req)
	require.NoError(t, err)
	assert.Equal(t, fasthttp.StatusNotFound, testutil.GetResponseStatusCode(req))
}

func TestApp_DeleteRole_Success(t *testing.T) {
	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)

	role := testutil.CreateTestRoleExact(t, app.DB, org.ID, "Deletable Role", false, false, nil)
	user := createRoleOperator(t, app, org.ID, "roles:delete")

	req := testutil.NewGETRequest(t)
	req.RequestCtx.Request.Header.SetMethod("DELETE")
	req.RequestCtx.SetUserValue("user_id", user.ID)
	req.RequestCtx.SetUserValue("organization_id", org.ID)
	req.RequestCtx.SetUserValue("id", role.ID.String())

	err := app.DeleteRole(req)
	require.NoError(t, err)
	assert.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req))

	// Verify role was deleted
	var dbRole models.CustomRole
	err = app.DB.First(&dbRole, "id = ?", role.ID).Error
	assert.Error(t, err) // Should be not found
}

func TestApp_DeleteRole_SystemRole(t *testing.T) {
	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)

	systemRole := testutil.CreateTestRoleExact(t, app.DB, org.ID, "System Role", true, false, nil)
	user := createRoleOperator(t, app, org.ID, "roles:delete")

	req := testutil.NewGETRequest(t)
	req.RequestCtx.Request.Header.SetMethod("DELETE")
	req.RequestCtx.SetUserValue("user_id", user.ID)
	req.RequestCtx.SetUserValue("organization_id", org.ID)
	req.RequestCtx.SetUserValue("id", systemRole.ID.String())

	err := app.DeleteRole(req)
	require.NoError(t, err)
	assert.Equal(t, fasthttp.StatusBadRequest, testutil.GetResponseStatusCode(req))

	// Verify role still exists
	var dbRole models.CustomRole
	require.NoError(t, app.DB.First(&dbRole, "id = ?", systemRole.ID).Error)
}

func TestApp_DeleteRole_WithAssignedUsers(t *testing.T) {
	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)

	role := testutil.CreateTestRoleExact(t, app.DB, org.ID, "Role With Users", false, false, nil)
	// Create a user with this role
	testutil.CreateTestUser(t, app.DB, org.ID, testutil.WithEmail(testutil.UniqueEmail("assigned-user")), testutil.WithRoleID(&role.ID))
	adminUser := createRoleOperator(t, app, org.ID, "roles:delete")

	req := testutil.NewGETRequest(t)
	req.RequestCtx.Request.Header.SetMethod("DELETE")
	req.RequestCtx.SetUserValue("user_id", adminUser.ID)
	req.RequestCtx.SetUserValue("organization_id", org.ID)
	req.RequestCtx.SetUserValue("id", role.ID.String())

	err := app.DeleteRole(req)
	require.NoError(t, err)
	assert.Equal(t, fasthttp.StatusBadRequest, testutil.GetResponseStatusCode(req))
}

func TestApp_ListPermissions_Success(t *testing.T) {
	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	permissions := testutil.GetOrCreateTestPermissions(t, app.DB)
	user := createRoleOperator(t, app, org.ID, "roles:read")

	req := testutil.NewGETRequest(t)
	req.RequestCtx.SetUserValue("user_id", user.ID)
	req.RequestCtx.SetUserValue("organization_id", org.ID)

	err := app.ListPermissions(req)
	require.NoError(t, err)
	assert.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req))

	var resp struct {
		Status string `json:"status"`
		Data   struct {
			Permissions []handlers.PermissionResponse `json:"permissions"`
		} `json:"data"`
	}
	err = json.Unmarshal(testutil.GetResponseBody(req), &resp)
	require.NoError(t, err)

	assert.Equal(t, "success", resp.Status)
	assert.GreaterOrEqual(t, len(resp.Data.Permissions), len(permissions))

	// Verify permission format
	for _, perm := range resp.Data.Permissions {
		assert.NotEmpty(t, perm.Resource)
		assert.NotEmpty(t, perm.Action)
		assert.Equal(t, perm.Resource+":"+perm.Action, perm.Key)
	}
}

func TestApp_RoleReadEndpoints_RequireReadPermission(t *testing.T) {
	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	role := testutil.CreateTestRoleWithKeys(t, app.DB, org.ID, "target-role", []string{"contacts:read"})
	user := testutil.CreateTestUser(t, app.DB, org.ID, testutil.WithRoleID(&role.ID))

	tests := []struct {
		name string
		call func(*handlers.App, *fastglue.Request) error
		path bool
	}{
		{name: "list roles", call: (*handlers.App).ListRoles},
		{name: "get role", call: (*handlers.App).GetRole, path: true},
		{name: "list permissions", call: (*handlers.App).ListPermissions},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := testutil.NewGETRequest(t)
			testutil.SetAuthContext(req, org.ID, user.ID)
			if tt.path {
				req.RequestCtx.SetUserValue("id", role.ID.String())
			}

			require.NoError(t, tt.call(app, req))
			assert.Equal(t, fasthttp.StatusForbidden, testutil.GetResponseStatusCode(req))
		})
	}
}

func TestApp_CreateRole_RequiresWritePermission(t *testing.T) {
	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	user := createRoleOperator(t, app, org.ID, "roles:read")

	req := testutil.NewJSONRequest(t, handlers.RoleRequest{
		Name:        "Unauthorized Role",
		Permissions: []string{"users:write"},
	})
	testutil.SetAuthContext(req, org.ID, user.ID)

	require.NoError(t, app.CreateRole(req))
	assert.Equal(t, fasthttp.StatusForbidden, testutil.GetResponseStatusCode(req))

	var count int64
	require.NoError(t, app.DB.Model(&models.CustomRole{}).
		Where("organization_id = ? AND name = ?", org.ID, "Unauthorized Role").
		Count(&count).Error)
	assert.Zero(t, count)
}

func TestApp_UpdateRole_PreventsCustomRoleSelfEscalation(t *testing.T) {
	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	role := testutil.CreateTestRoleWithKeys(t, app.DB, org.ID, "reviewer", []string{"roles:read"})
	user := testutil.CreateTestUser(t, app.DB, org.ID, testutil.WithRoleID(&role.ID))

	req := testutil.NewJSONRequest(t, handlers.RoleRequest{
		Name:        "Escalated Reviewer",
		Permissions: []string{"roles:read", "roles:write", "users:write"},
	})
	testutil.SetAuthContext(req, org.ID, user.ID)
	req.RequestCtx.SetUserValue("id", role.ID.String())

	require.NoError(t, app.UpdateRole(req))
	assert.Equal(t, fasthttp.StatusForbidden, testutil.GetResponseStatusCode(req))

	var persisted models.CustomRole
	require.NoError(t, app.DB.Preload("Permissions").First(&persisted, "id = ?", role.ID).Error)
	assert.Equal(t, role.Name, persisted.Name)
	require.Len(t, persisted.Permissions, 1)
	assert.Equal(t, "roles:read", persisted.Permissions[0].Resource+":"+persisted.Permissions[0].Action)
}

func TestApp_DeleteRole_RequiresDeletePermission(t *testing.T) {
	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	target := testutil.CreateTestRoleExact(t, app.DB, org.ID, "Protected Role", false, false, nil)
	user := createRoleOperator(t, app, org.ID, "roles:read")

	req := testutil.NewGETRequest(t)
	req.RequestCtx.Request.Header.SetMethod("DELETE")
	testutil.SetAuthContext(req, org.ID, user.ID)
	req.RequestCtx.SetUserValue("id", target.ID.String())

	require.NoError(t, app.DeleteRole(req))
	assert.Equal(t, fasthttp.StatusForbidden, testutil.GetResponseStatusCode(req))

	var persisted models.CustomRole
	require.NoError(t, app.DB.First(&persisted, "id = ?", target.ID).Error)
}
