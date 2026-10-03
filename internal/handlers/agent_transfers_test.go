package handlers_test

import (
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/handlers"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
	"github.com/zerodha/fastglue"
)

// createTestAgent creates a test agent user with agent role in the database.
func createTestAgent(t *testing.T, app *handlers.App, orgID uuid.UUID) *models.User {
	t.Helper()

	role := testutil.CreateAgentRole(t, app.DB, orgID)
	return testutil.CreateTestUser(t, app.DB, orgID,
		testutil.WithRoleID(&role.ID),
		testutil.WithFullName("Test Agent"),
	)
}

// createTestTransfer creates a test agent transfer in the database.
func createTestTransfer(t *testing.T, app *handlers.App, orgID, contactID uuid.UUID, accountName string, status models.TransferStatus, agentID *uuid.UUID) *models.AgentTransfer {
	t.Helper()

	transfer := &models.AgentTransfer{
		BaseModel:       models.BaseModel{ID: uuid.New()},
		OrganizationID:  orgID,
		ContactID:       contactID,
		WhatsAppAccount: accountName,
		PhoneNumber:     "1234567890",
		Status:          status,
		Source:          models.TransferSourceManual,
		AgentID:         agentID,
		TransferredAt:   time.Now(),
	}
	require.NoError(t, app.DB.Create(transfer).Error)
	return transfer
}

// createTestTeam creates a test team with optional members.
func createTestTeam(t *testing.T, app *handlers.App, orgID uuid.UUID, memberIDs ...uuid.UUID) *models.Team {
	t.Helper()

	uniqueID := uuid.New().String()[:8]
	team := &models.Team{
		BaseModel:          models.BaseModel{ID: uuid.New()},
		OrganizationID:     orgID,
		Name:               "Test Team " + uniqueID,
		IsActive:           true,
		AssignmentStrategy: models.AssignmentStrategyRoundRobin,
	}
	require.NoError(t, app.DB.Create(team).Error)

	for _, memberID := range memberIDs {
		member := &models.TeamMember{
			BaseModel: models.BaseModel{ID: uuid.New()},
			TeamID:    team.ID,
			UserID:    memberID,
			Role:      models.TeamRoleAgent,
		}
		require.NoError(t, app.DB.Create(member).Error)
	}

	return team
}

// --- ListAgentTransfers Tests ---

func TestApp_ListAgentTransfers_Success(t *testing.T) {
	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	adminRole := testutil.CreateAdminRole(t, app.DB, org.ID)
	user := testutil.CreateTestUser(t, app.DB, org.ID, testutil.WithRoleID(&adminRole.ID))
	account := testutil.CreateTestWhatsAppAccount(t, app.DB, org.ID)

	contact := testutil.CreateTestContact(t, app.DB, org.ID)
	agent := createTestAgent(t, app, org.ID)

	// Create some transfers
	transfer1 := createTestTransfer(t, app, org.ID, contact.ID, account.Name, models.TransferStatusActive, nil)
	_ = createTestTransfer(t, app, org.ID, contact.ID, account.Name, models.TransferStatusResumed, &agent.ID)

	req := testutil.NewGETRequest(t)
	testutil.SetAuthContext(req, org.ID, user.ID)

	err := app.ListAgentTransfers(req)
	require.NoError(t, err)
	assert.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req))

	var result struct {
		Status string `json:"status"`
		Data   struct {
			Transfers         []handlers.AgentTransferResponse `json:"transfers"`
			GeneralQueueCount int64                            `json:"general_queue_count"`
			TotalCount        int64                            `json:"total_count"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(testutil.GetResponseBody(req), &result))

	assert.Equal(t, "success", result.Status)
	assert.Equal(t, int64(2), result.Data.TotalCount)
	assert.Len(t, result.Data.Transfers, 2)

	// First transfer should be the active unassigned one (FIFO)
	assert.Equal(t, transfer1.ID.String(), result.Data.Transfers[0].ID)
	assert.Equal(t, models.TransferStatusActive, result.Data.Transfers[0].Status)
}

func TestApp_ListAgentTransfers_QueuePickupPolicy(t *testing.T) {
	t.Run("exposes disabled policy without chatbot settings access", func(t *testing.T) {
		app := newTestApp(t)
		org := testutil.CreateTestOrganization(t, app.DB)
		agent := createTestAgent(t, app, org.ID)

		settings := &models.ChatbotSettings{OrganizationID: org.ID}
		require.NoError(t, app.DB.Create(settings).Error)
		require.NoError(t, app.DB.Model(&models.ChatbotSettings{}).
			Where("id = ?", settings.ID).
			Update("allow_agent_queue_pickup", false).Error)
		app.InvalidateChatbotSettingsCache(org.ID)

		req := testutil.NewGETRequest(t)
		testutil.SetAuthContext(req, org.ID, agent.ID)

		require.NoError(t, app.ListAgentTransfers(req))
		require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req))

		var result struct {
			Data struct {
				AllowAgentQueuePickup bool `json:"allow_agent_queue_pickup"`
			} `json:"data"`
		}
		require.NoError(t, json.Unmarshal(testutil.GetResponseBody(req), &result))
		assert.False(t, result.Data.AllowAgentQueuePickup)
	})

	t.Run("defaults enabled when settings are missing", func(t *testing.T) {
		app := newTestApp(t)
		org := testutil.CreateTestOrganization(t, app.DB)
		agent := createTestAgent(t, app, org.ID)

		req := testutil.NewGETRequest(t)
		testutil.SetAuthContext(req, org.ID, agent.ID)

		require.NoError(t, app.ListAgentTransfers(req))
		require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req))

		var result struct {
			Data struct {
				AllowAgentQueuePickup bool `json:"allow_agent_queue_pickup"`
			} `json:"data"`
		}
		require.NoError(t, json.Unmarshal(testutil.GetResponseBody(req), &result))
		assert.True(t, result.Data.AllowAgentQueuePickup)
	})
}

func TestApp_ListAgentTransfers_RequiresReadPermission(t *testing.T) {
	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	user := testutil.CreateTestUser(t, app.DB, org.ID)
	contact := testutil.CreateTestContact(t, app.DB, org.ID)
	createTestTransfer(t, app, org.ID, contact.ID, "permission-test", models.TransferStatusActive, nil)

	req := testutil.NewGETRequest(t)
	testutil.SetAuthContext(req, org.ID, user.ID)

	require.NoError(t, app.ListAgentTransfers(req))
	require.Equal(t, fasthttp.StatusForbidden, testutil.GetResponseStatusCode(req))
}

func TestApp_ListAgentTransfers_FilterByStatus(t *testing.T) {
	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	adminRole := testutil.CreateAdminRole(t, app.DB, org.ID)
	user := testutil.CreateTestUser(t, app.DB, org.ID, testutil.WithRoleID(&adminRole.ID))
	account := testutil.CreateTestWhatsAppAccount(t, app.DB, org.ID)

	contact := testutil.CreateTestContact(t, app.DB, org.ID)
	agent := createTestAgent(t, app, org.ID)

	// Create transfers with different statuses
	_ = createTestTransfer(t, app, org.ID, contact.ID, account.Name, models.TransferStatusActive, nil)
	_ = createTestTransfer(t, app, org.ID, contact.ID, account.Name, models.TransferStatusResumed, &agent.ID)

	req := testutil.NewGETRequest(t)
	testutil.SetAuthContext(req, org.ID, user.ID)
	testutil.SetQueryParam(req, "status", models.TransferStatusActive)

	err := app.ListAgentTransfers(req)
	require.NoError(t, err)
	assert.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req))

	var result struct {
		Status string `json:"status"`
		Data   struct {
			Transfers  []handlers.AgentTransferResponse `json:"transfers"`
			TotalCount int64                            `json:"total_count"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(testutil.GetResponseBody(req), &result))

	assert.Equal(t, "success", result.Status)
	assert.Len(t, result.Data.Transfers, 1)
	assert.Equal(t, models.TransferStatusActive, result.Data.Transfers[0].Status)
}

func TestApp_ListAgentTransfers_AgentRoleFiltering(t *testing.T) {
	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	account := testutil.CreateTestWhatsAppAccount(t, app.DB, org.ID)

	contact := testutil.CreateTestContact(t, app.DB, org.ID)
	agent := createTestAgent(t, app, org.ID)

	// Create another agent
	otherAgent := createTestAgent(t, app, org.ID)
	otherContact := testutil.CreateTestContact(t, app.DB, org.ID)
	queueContact := testutil.CreateTestContact(t, app.DB, org.ID)

	// Create transfers: one assigned to agent, one to other agent, one unassigned
	_ = createTestTransfer(t, app, org.ID, contact.ID, account.Name, models.TransferStatusActive, &agent.ID)
	_ = createTestTransfer(t, app, org.ID, otherContact.ID, account.Name, models.TransferStatusActive, &otherAgent.ID)
	_ = createTestTransfer(t, app, org.ID, queueContact.ID, account.Name, models.TransferStatusActive, nil) // Unassigned (general queue)

	// Agent should only see their assigned transfers + general queue
	req := testutil.NewGETRequest(t)
	testutil.SetAuthContext(req, org.ID, agent.ID)

	err := app.ListAgentTransfers(req)
	require.NoError(t, err)
	assert.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req))

	var result struct {
		Status string `json:"status"`
		Data   struct {
			Transfers  []handlers.AgentTransferResponse `json:"transfers"`
			TotalCount int64                            `json:"total_count"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(testutil.GetResponseBody(req), &result))

	// Agent sees their transfer + general queue (2), not the other agent's transfer
	assert.Equal(t, int64(2), result.Data.TotalCount)
}

func TestApp_ListAgentTransfers_Pagination(t *testing.T) {
	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	adminRole := testutil.CreateAdminRole(t, app.DB, org.ID)
	user := testutil.CreateTestUser(t, app.DB, org.ID, testutil.WithRoleID(&adminRole.ID))
	account := testutil.CreateTestWhatsAppAccount(t, app.DB, org.ID)

	// Create multiple transfers
	for i := 0; i < 5; i++ {
		contact := testutil.CreateTestContact(t, app.DB, org.ID)
		createTestTransfer(t, app, org.ID, contact.ID, account.Name, models.TransferStatusActive, nil)
	}

	// Request with limit and offset
	req := testutil.NewGETRequest(t)
	testutil.SetAuthContext(req, org.ID, user.ID)
	testutil.SetQueryParam(req, "limit", "2")
	testutil.SetQueryParam(req, "offset", "1")

	err := app.ListAgentTransfers(req)
	require.NoError(t, err)
	assert.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req))

	var result struct {
		Status string `json:"status"`
		Data   struct {
			Transfers  []handlers.AgentTransferResponse `json:"transfers"`
			TotalCount int64                            `json:"total_count"`
			Limit      int                              `json:"limit"`
			Offset     int                              `json:"offset"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(testutil.GetResponseBody(req), &result))

	assert.Equal(t, int64(5), result.Data.TotalCount)
	assert.Len(t, result.Data.Transfers, 2)
	assert.Equal(t, 2, result.Data.Limit)
	assert.Equal(t, 1, result.Data.Offset)
}

func TestApp_ListAgentTransfers_ExactContactBeyondFirstFIFOPage(t *testing.T) {
	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	adminRole := testutil.CreateAdminRole(t, app.DB, org.ID)
	user := testutil.CreateTestUser(t, app.DB, org.ID, testutil.WithRoleID(&adminRole.ID))
	account := testutil.CreateTestWhatsAppAccount(t, app.DB, org.ID)
	baseTime := time.Now().UTC().Add(-3 * time.Hour)

	for i := 0; i < 100; i++ {
		contact := testutil.CreateTestContactWith(
			t,
			app.DB,
			org.ID,
			testutil.WithPhoneNumber(fmt.Sprintf("+155500%04d", i)),
		)
		transfer := createTestTransfer(t, app, org.ID, contact.ID, account.Name, models.TransferStatusActive, nil)
		require.NoError(t, app.DB.Model(transfer).
			Update("transferred_at", baseTime.Add(time.Duration(i)*time.Second)).Error)
	}

	targetContact := testutil.CreateTestContactWith(
		t,
		app.DB,
		org.ID,
		testutil.WithPhoneNumber("+1555999999"),
	)
	targetTransfer := createTestTransfer(t, app, org.ID, targetContact.ID, account.Name, models.TransferStatusActive, nil)
	require.NoError(t, app.DB.Model(targetTransfer).
		Update("transferred_at", baseTime.Add(2*time.Hour)).Error)

	firstPageReq := testutil.NewGETRequest(t)
	testutil.SetAuthContext(firstPageReq, org.ID, user.ID)
	testutil.SetQueryParam(firstPageReq, "status", string(models.TransferStatusActive))
	require.NoError(t, app.ListAgentTransfers(firstPageReq))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(firstPageReq))

	var firstPage struct {
		Data struct {
			Transfers []handlers.AgentTransferResponse `json:"transfers"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(testutil.GetResponseBody(firstPageReq), &firstPage))
	require.Len(t, firstPage.Data.Transfers, 100)
	for _, transfer := range firstPage.Data.Transfers {
		assert.NotEqual(t, targetTransfer.ID.String(), transfer.ID)
	}

	exactReq := testutil.NewGETRequest(t)
	testutil.SetAuthContext(exactReq, org.ID, user.ID)
	testutil.SetQueryParam(exactReq, "status", string(models.TransferStatusActive))
	testutil.SetQueryParam(exactReq, "contact_id", targetContact.ID.String())
	testutil.SetQueryParam(exactReq, "limit", "1")
	require.NoError(t, app.ListAgentTransfers(exactReq))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(exactReq))

	var exactResult struct {
		Data struct {
			Transfers  []handlers.AgentTransferResponse `json:"transfers"`
			TotalCount int64                            `json:"total_count"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(testutil.GetResponseBody(exactReq), &exactResult))
	require.Len(t, exactResult.Data.Transfers, 1)
	assert.Equal(t, targetTransfer.ID.String(), exactResult.Data.Transfers[0].ID)
	assert.Equal(t, int64(1), exactResult.Data.TotalCount)
}

func TestApp_ListAgentTransfers_ExactContactIsTenantScoped(t *testing.T) {
	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	adminRole := testutil.CreateAdminRole(t, app.DB, org.ID)
	user := testutil.CreateTestUser(t, app.DB, org.ID, testutil.WithRoleID(&adminRole.ID))

	otherOrg := testutil.CreateTestOrganization(t, app.DB)
	otherAccount := testutil.CreateTestWhatsAppAccount(t, app.DB, otherOrg.ID)
	otherContact := testutil.CreateTestContact(t, app.DB, otherOrg.ID)
	createTestTransfer(t, app, otherOrg.ID, otherContact.ID, otherAccount.Name, models.TransferStatusActive, nil)

	req := testutil.NewGETRequest(t)
	testutil.SetAuthContext(req, org.ID, user.ID)
	testutil.SetQueryParam(req, "status", string(models.TransferStatusActive))
	testutil.SetQueryParam(req, "contact_id", otherContact.ID.String())
	require.NoError(t, app.ListAgentTransfers(req))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req))

	var result struct {
		Data struct {
			Transfers  []handlers.AgentTransferResponse `json:"transfers"`
			TotalCount int64                            `json:"total_count"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(testutil.GetResponseBody(req), &result))
	assert.Empty(t, result.Data.Transfers)
	assert.Zero(t, result.Data.TotalCount)
}

func TestApp_ListAgentTransfers_ExactContactIsVisibleToCreatorAfterAssignment(t *testing.T) {
	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	creator := createTestAgent(t, app, org.ID)
	assignedAgent := createTestAgent(t, app, org.ID)
	otherAgent := createTestAgent(t, app, org.ID)
	contact := testutil.CreateTestContact(t, app.DB, org.ID)
	transfer := createTestTransfer(
		t,
		app,
		org.ID,
		contact.ID,
		"creator-visibility",
		models.TransferStatusActive,
		&assignedAgent.ID,
	)
	require.NoError(t, app.DB.Model(transfer).Updates(map[string]any{
		"transferred_by_user_id": creator.ID,
		"source":                 models.TransferSourceManual,
	}).Error)

	exactReq := testutil.NewGETRequest(t)
	testutil.SetAuthContext(exactReq, org.ID, creator.ID)
	testutil.SetQueryParam(exactReq, "status", string(models.TransferStatusActive))
	testutil.SetQueryParam(exactReq, "contact_id", contact.ID.String())
	require.NoError(t, app.ListAgentTransfers(exactReq))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(exactReq))

	var exactResult struct {
		Data struct {
			Transfers  []handlers.AgentTransferResponse `json:"transfers"`
			TotalCount int64                            `json:"total_count"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(testutil.GetResponseBody(exactReq), &exactResult))
	require.Len(t, exactResult.Data.Transfers, 1)
	assert.Equal(t, transfer.ID.String(), exactResult.Data.Transfers[0].ID)
	assert.Equal(t, int64(1), exactResult.Data.TotalCount)

	queueReq := testutil.NewGETRequest(t)
	testutil.SetAuthContext(queueReq, org.ID, creator.ID)
	testutil.SetQueryParam(queueReq, "status", string(models.TransferStatusActive))
	require.NoError(t, app.ListAgentTransfers(queueReq))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(queueReq))
	var queueResult struct {
		Data struct {
			Transfers []handlers.AgentTransferResponse `json:"transfers"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(testutil.GetResponseBody(queueReq), &queueResult))
	assert.Empty(t, queueResult.Data.Transfers)

	otherReq := testutil.NewGETRequest(t)
	testutil.SetAuthContext(otherReq, org.ID, otherAgent.ID)
	testutil.SetQueryParam(otherReq, "status", string(models.TransferStatusActive))
	testutil.SetQueryParam(otherReq, "contact_id", contact.ID.String())
	require.NoError(t, app.ListAgentTransfers(otherReq))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(otherReq))
	var otherResult struct {
		Data struct {
			Transfers []handlers.AgentTransferResponse `json:"transfers"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(testutil.GetResponseBody(otherReq), &otherResult))
	assert.Empty(t, otherResult.Data.Transfers)
}

func TestApp_ListAgentTransfers_RejectsInvalidContactFilter(t *testing.T) {
	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	adminRole := testutil.CreateAdminRole(t, app.DB, org.ID)
	user := testutil.CreateTestUser(t, app.DB, org.ID, testutil.WithRoleID(&adminRole.ID))

	req := testutil.NewGETRequest(t)
	testutil.SetAuthContext(req, org.ID, user.ID)
	testutil.SetQueryParam(req, "contact_id", "not-a-uuid")
	require.NoError(t, app.ListAgentTransfers(req))
	assert.Equal(t, fasthttp.StatusBadRequest, testutil.GetResponseStatusCode(req))
}

// --- CreateAgentTransfer Tests ---

func TestApp_CreateAgentTransfer_Success(t *testing.T) {
	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	adminRole := testutil.CreateAdminRole(t, app.DB, org.ID)
	user := testutil.CreateTestUser(t, app.DB, org.ID, testutil.WithRoleID(&adminRole.ID))
	account := testutil.CreateTestWhatsAppAccount(t, app.DB, org.ID)

	contact := testutil.CreateTestContact(t, app.DB, org.ID)

	req := testutil.NewJSONRequest(t, map[string]any{
		"contact_id":       contact.ID.String(),
		"whatsapp_account": account.Name,
		"notes":            "Test transfer",
		"source":           models.TransferSourceManual,
	})
	testutil.SetAuthContext(req, org.ID, user.ID)

	err := app.CreateAgentTransfer(req)
	require.NoError(t, err)
	assert.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req))

	var result struct {
		Status string `json:"status"`
		Data   struct {
			Transfer handlers.AgentTransferResponse `json:"transfer"`
			Message  string                         `json:"message"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(testutil.GetResponseBody(req), &result))

	assert.Equal(t, "success", result.Status)
	assert.Equal(t, "Transfer created successfully", result.Data.Message)
	assert.Equal(t, contact.ID.String(), result.Data.Transfer.ContactID)
	assert.Equal(t, models.TransferStatusActive, result.Data.Transfer.Status)
	assert.Equal(t, models.TransferSourceManual, result.Data.Transfer.Source)
}

func TestApp_CreateAgentTransfer_RequiresWritePermissionBeforeContactLookup(t *testing.T) {
	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	role := testutil.CreateTestRoleWithKeys(t, app.DB, org.ID, "Transfer reader", []string{
		models.ResourceTransfers + ":" + models.ActionRead,
	})
	user := testutil.CreateTestUser(t, app.DB, org.ID, testutil.WithRoleID(&role.ID))

	req := testutil.NewJSONRequest(t, map[string]any{
		"contact_id":       uuid.New().String(),
		"whatsapp_account": "permission-test",
	})
	testutil.SetAuthContext(req, org.ID, user.ID)

	require.NoError(t, app.CreateAgentTransfer(req))
	require.Equal(t, fasthttp.StatusForbidden, testutil.GetResponseStatusCode(req))

	var count int64
	require.NoError(t, app.DB.Model(&models.AgentTransfer{}).
		Where("organization_id = ?", org.ID).
		Count(&count).Error)
	assert.Zero(t, count)
}

func TestApp_CreateAgentTransfer_WithAgent(t *testing.T) {
	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	adminRole := testutil.CreateAdminRole(t, app.DB, org.ID)
	user := testutil.CreateTestUser(t, app.DB, org.ID, testutil.WithRoleID(&adminRole.ID))
	account := testutil.CreateTestWhatsAppAccount(t, app.DB, org.ID)

	contact := testutil.CreateTestContact(t, app.DB, org.ID)
	agent := createTestAgent(t, app, org.ID)

	req := testutil.NewJSONRequest(t, map[string]any{
		"contact_id":       contact.ID.String(),
		"whatsapp_account": account.Name,
		"agent_id":         agent.ID.String(),
		"notes":            "Assigned to specific agent",
	})
	testutil.SetAuthContext(req, org.ID, user.ID)

	err := app.CreateAgentTransfer(req)
	require.NoError(t, err)
	assert.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req))

	var result struct {
		Status string `json:"status"`
		Data   struct {
			Transfer handlers.AgentTransferResponse `json:"transfer"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(testutil.GetResponseBody(req), &result))

	assert.Equal(t, "success", result.Status)
	assert.NotNil(t, result.Data.Transfer.AgentID)
	assert.Equal(t, agent.ID.String(), *result.Data.Transfer.AgentID)
}

func TestApp_CreateAgentTransfer_ContactNotFound(t *testing.T) {
	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	adminRole := testutil.CreateAdminRole(t, app.DB, org.ID)
	user := testutil.CreateTestUser(t, app.DB, org.ID, testutil.WithRoleID(&adminRole.ID))
	account := testutil.CreateTestWhatsAppAccount(t, app.DB, org.ID)

	req := testutil.NewJSONRequest(t, map[string]any{
		"contact_id":       uuid.New().String(), // Non-existent contact
		"whatsapp_account": account.Name,
	})
	testutil.SetAuthContext(req, org.ID, user.ID)

	err := app.CreateAgentTransfer(req)
	require.NoError(t, err)
	assert.Equal(t, fasthttp.StatusNotFound, testutil.GetResponseStatusCode(req))

	var result map[string]any
	require.NoError(t, json.Unmarshal(testutil.GetResponseBody(req), &result))
	assert.Equal(t, "Contact not found", result["message"])
}

func TestApp_CreateAgentTransfer_DuplicateTransfer(t *testing.T) {
	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	adminRole := testutil.CreateAdminRole(t, app.DB, org.ID)
	user := testutil.CreateTestUser(t, app.DB, org.ID, testutil.WithRoleID(&adminRole.ID))
	account := testutil.CreateTestWhatsAppAccount(t, app.DB, org.ID)

	contact := testutil.CreateTestContact(t, app.DB, org.ID)

	// Create an existing active transfer
	createTestTransfer(t, app, org.ID, contact.ID, account.Name, models.TransferStatusActive, nil)

	req := testutil.NewJSONRequest(t, map[string]any{
		"contact_id":       contact.ID.String(),
		"whatsapp_account": account.Name,
	})
	testutil.SetAuthContext(req, org.ID, user.ID)

	err := app.CreateAgentTransfer(req)
	require.NoError(t, err)
	assert.Equal(t, fasthttp.StatusConflict, testutil.GetResponseStatusCode(req))

	var result map[string]any
	require.NoError(t, json.Unmarshal(testutil.GetResponseBody(req), &result))
	assert.Equal(t, "Contact already has an active transfer", result["message"])
}

func TestApp_CreateAgentTransfer_ResolvesMergedAlias(t *testing.T) {
	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	adminRole := testutil.CreateAdminRole(t, app.DB, org.ID)
	user := testutil.CreateTestUser(t, app.DB, org.ID, testutil.WithRoleID(&adminRole.ID))
	account := testutil.CreateTestWhatsAppAccount(t, app.DB, org.ID)
	canonical := testutil.CreateTestContact(t, app.DB, org.ID)
	alias := testutil.CreateTestContact(t, app.DB, org.ID)
	require.NoError(t, app.DB.Unscoped().Model(&models.Contact{}).
		Where("id = ? AND organization_id = ?", alias.ID, org.ID).
		Updates(map[string]any{
			"merged_into_id": canonical.ID,
			"deleted_at":     time.Now(),
		}).Error)

	req := testutil.NewJSONRequest(t, map[string]any{
		"contact_id":       alias.ID.String(),
		"whatsapp_account": account.Name,
	})
	testutil.SetAuthContext(req, org.ID, user.ID)

	require.NoError(t, app.CreateAgentTransfer(req))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req))

	var result struct {
		Data struct {
			Transfer handlers.AgentTransferResponse `json:"transfer"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(testutil.GetResponseBody(req), &result))
	assert.Equal(t, canonical.ID.String(), result.Data.Transfer.ContactID)

	var stored models.AgentTransfer
	require.NoError(t, app.DB.First(&stored, "id = ?", result.Data.Transfer.ID).Error)
	assert.Equal(t, canonical.ID, stored.ContactID)
}

func TestApp_CreateAgentTransfer_ConcurrentRequestsReturnConflict(t *testing.T) {
	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	adminRole := testutil.CreateAdminRole(t, app.DB, org.ID)
	user := testutil.CreateTestUser(t, app.DB, org.ID, testutil.WithRoleID(&adminRole.ID))
	account := testutil.CreateTestWhatsAppAccount(t, app.DB, org.ID)
	contact := testutil.CreateTestContact(t, app.DB, org.ID)

	requests := make([]*fastglue.Request, 2)
	for i := range requests {
		requests[i] = testutil.NewJSONRequest(t, map[string]any{
			"contact_id":       contact.ID.String(),
			"whatsapp_account": account.Name,
		})
		testutil.SetAuthContext(requests[i], org.ID, user.ID)
	}

	type requestResult struct {
		status int
		err    error
	}
	start := make(chan struct{})
	results := make(chan requestResult, len(requests))
	for i := range requests {
		req := requests[i]
		go func() {
			<-start
			err := app.CreateAgentTransfer(req)
			results <- requestResult{
				status: testutil.GetResponseStatusCode(req),
				err:    err,
			}
		}()
	}
	close(start)

	okCount := 0
	conflictCount := 0
	for range requests {
		result := <-results
		require.NoError(t, result.err)
		switch result.status {
		case fasthttp.StatusOK:
			okCount++
		case fasthttp.StatusConflict:
			conflictCount++
		default:
			t.Fatalf("unexpected concurrent create status: %d", result.status)
		}
	}
	assert.Equal(t, 1, okCount)
	assert.Equal(t, 1, conflictCount)

	var activeCount int64
	require.NoError(t, app.DB.Model(&models.AgentTransfer{}).
		Where(
			"organization_id = ? AND contact_id = ? AND status = ?",
			org.ID,
			contact.ID,
			models.TransferStatusActive,
		).
		Count(&activeCount).Error)
	assert.Equal(t, int64(1), activeCount)
}

func TestApp_CreateAgentTransfer_MissingContactID(t *testing.T) {
	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	adminRole := testutil.CreateAdminRole(t, app.DB, org.ID)
	user := testutil.CreateTestUser(t, app.DB, org.ID, testutil.WithRoleID(&adminRole.ID))
	account := testutil.CreateTestWhatsAppAccount(t, app.DB, org.ID)

	req := testutil.NewJSONRequest(t, map[string]any{
		"whatsapp_account": account.Name,
	})
	testutil.SetAuthContext(req, org.ID, user.ID)

	err := app.CreateAgentTransfer(req)
	require.NoError(t, err)
	assert.Equal(t, fasthttp.StatusBadRequest, testutil.GetResponseStatusCode(req))

	var result map[string]any
	require.NoError(t, json.Unmarshal(testutil.GetResponseBody(req), &result))
	assert.Equal(t, "contact_id is required", result["message"])
}

func TestApp_CreateAgentTransfer_AgentUnavailable(t *testing.T) {
	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	adminRole := testutil.CreateAdminRole(t, app.DB, org.ID)
	user := testutil.CreateTestUser(t, app.DB, org.ID, testutil.WithRoleID(&adminRole.ID))
	account := testutil.CreateTestWhatsAppAccount(t, app.DB, org.ID)

	contact := testutil.CreateTestContact(t, app.DB, org.ID)
	agent := createTestAgent(t, app, org.ID)

	// Make agent unavailable
	require.NoError(t, app.DB.Model(agent).Update("is_available", false).Error)

	req := testutil.NewJSONRequest(t, map[string]any{
		"contact_id":       contact.ID.String(),
		"whatsapp_account": account.Name,
		"agent_id":         agent.ID.String(),
	})
	testutil.SetAuthContext(req, org.ID, user.ID)

	err := app.CreateAgentTransfer(req)
	require.NoError(t, err)
	assert.Equal(t, fasthttp.StatusBadRequest, testutil.GetResponseStatusCode(req))

	var result map[string]any
	require.NoError(t, json.Unmarshal(testutil.GetResponseBody(req), &result))
	assert.Equal(t, "Agent is currently away", result["message"])
}

// --- ResumeFromTransfer Tests ---

func TestApp_ResumeFromTransfer_Success(t *testing.T) {
	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	adminRole := testutil.CreateAdminRole(t, app.DB, org.ID)
	user := testutil.CreateTestUser(t, app.DB, org.ID, testutil.WithRoleID(&adminRole.ID))
	account := testutil.CreateTestWhatsAppAccount(t, app.DB, org.ID)

	contact := testutil.CreateTestContact(t, app.DB, org.ID)
	transfer := createTestTransfer(t, app, org.ID, contact.ID, account.Name, models.TransferStatusActive, nil)

	req := testutil.NewJSONRequest(t, nil)
	testutil.SetAuthContext(req, org.ID, user.ID)
	testutil.SetPathParam(req, "id", transfer.ID.String())

	err := app.ResumeFromTransfer(req)
	require.NoError(t, err)
	assert.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req))

	var result struct {
		Status string `json:"status"`
		Data   struct {
			Message string `json:"message"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(testutil.GetResponseBody(req), &result))

	assert.Equal(t, "success", result.Status)
	assert.Contains(t, result.Data.Message, "resumed")

	// Verify transfer status updated
	var updatedTransfer models.AgentTransfer
	require.NoError(t, app.DB.First(&updatedTransfer, transfer.ID).Error)
	assert.Equal(t, models.TransferStatusResumed, updatedTransfer.Status)
	assert.NotNil(t, updatedTransfer.ResumedAt)
	assert.Equal(t, user.ID, *updatedTransfer.ResumedBy)
}

func TestApp_ResumeFromTransfer_RequiresWritePermissionWithoutMutation(t *testing.T) {
	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	role := testutil.CreateTestRoleWithKeys(t, app.DB, org.ID, "Transfer reader cannot resume", []string{
		models.ResourceTransfers + ":" + models.ActionRead,
	})
	user := testutil.CreateTestUser(t, app.DB, org.ID, testutil.WithRoleID(&role.ID))
	contact := testutil.CreateTestContact(t, app.DB, org.ID)
	transfer := createTestTransfer(t, app, org.ID, contact.ID, "permission-test", models.TransferStatusActive, nil)

	req := testutil.NewJSONRequest(t, nil)
	testutil.SetAuthContext(req, org.ID, user.ID)
	testutil.SetPathParam(req, "id", transfer.ID.String())

	require.NoError(t, app.ResumeFromTransfer(req))
	require.Equal(t, fasthttp.StatusForbidden, testutil.GetResponseStatusCode(req))

	var stored models.AgentTransfer
	require.NoError(t, app.DB.First(&stored, "id = ?", transfer.ID).Error)
	assert.Equal(t, models.TransferStatusActive, stored.Status)
	assert.Nil(t, stored.ResumedAt)
	assert.Nil(t, stored.ResumedBy)
}

func TestApp_ResumeFromTransfer_DefaultAgentCanOnlyResumeOwnTransfer(t *testing.T) {
	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	agent := createTestAgent(t, app, org.ID)
	otherAgent := createTestAgent(t, app, org.ID)
	contact := testutil.CreateTestContact(t, app.DB, org.ID)
	otherTransfer := createTestTransfer(
		t,
		app,
		org.ID,
		contact.ID,
		"permission-test",
		models.TransferStatusActive,
		&otherAgent.ID,
	)

	req := testutil.NewJSONRequest(t, nil)
	testutil.SetAuthContext(req, org.ID, agent.ID)
	testutil.SetPathParam(req, "id", otherTransfer.ID.String())
	require.NoError(t, app.ResumeFromTransfer(req))
	require.Equal(t, fasthttp.StatusForbidden, testutil.GetResponseStatusCode(req))

	var stored models.AgentTransfer
	require.NoError(t, app.DB.First(&stored, "id = ?", otherTransfer.ID).Error)
	assert.Equal(t, models.TransferStatusActive, stored.Status)
	assert.Nil(t, stored.ResumedAt)

	ownContact := testutil.CreateTestContact(t, app.DB, org.ID)
	ownTransfer := createTestTransfer(
		t,
		app,
		org.ID,
		ownContact.ID,
		"permission-test",
		models.TransferStatusActive,
		&agent.ID,
	)
	ownReq := testutil.NewJSONRequest(t, nil)
	testutil.SetAuthContext(ownReq, org.ID, agent.ID)
	testutil.SetPathParam(ownReq, "id", ownTransfer.ID.String())
	require.NoError(t, app.ResumeFromTransfer(ownReq))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(ownReq))
}

func TestApp_ResumeFromTransfer_DefaultAgentCanResumeOwnUnassignedManualTransfer(t *testing.T) {
	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	agent := createTestAgent(t, app, org.ID)
	otherAgent := createTestAgent(t, app, org.ID)
	account := testutil.CreateTestWhatsAppAccount(t, app.DB, org.ID)
	contact := testutil.CreateTestContact(t, app.DB, org.ID)

	createReq := testutil.NewJSONRequest(t, map[string]any{
		"contact_id":       contact.ID.String(),
		"whatsapp_account": account.Name,
		"source":           models.TransferSourceManual,
	})
	testutil.SetAuthContext(createReq, org.ID, agent.ID)
	require.NoError(t, app.CreateAgentTransfer(createReq))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(createReq))

	var created struct {
		Data struct {
			Transfer handlers.AgentTransferResponse `json:"transfer"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(testutil.GetResponseBody(createReq), &created))
	require.Nil(t, created.Data.Transfer.AgentID)
	require.NotNil(t, created.Data.Transfer.TransferredBy)
	require.Equal(t, agent.ID.String(), *created.Data.Transfer.TransferredBy)

	otherReq := testutil.NewJSONRequest(t, nil)
	testutil.SetAuthContext(otherReq, org.ID, otherAgent.ID)
	testutil.SetPathParam(otherReq, "id", created.Data.Transfer.ID)
	require.NoError(t, app.ResumeFromTransfer(otherReq))
	require.Equal(t, fasthttp.StatusForbidden, testutil.GetResponseStatusCode(otherReq))

	var stillActive models.AgentTransfer
	require.NoError(t, app.DB.First(&stillActive, "id = ?", created.Data.Transfer.ID).Error)
	require.Equal(t, models.TransferStatusActive, stillActive.Status)

	creatorReq := testutil.NewJSONRequest(t, nil)
	testutil.SetAuthContext(creatorReq, org.ID, agent.ID)
	testutil.SetPathParam(creatorReq, "id", created.Data.Transfer.ID)
	require.NoError(t, app.ResumeFromTransfer(creatorReq))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(creatorReq))

	var resumed models.AgentTransfer
	require.NoError(t, app.DB.First(&resumed, "id = ?", created.Data.Transfer.ID).Error)
	assert.Equal(t, models.TransferStatusResumed, resumed.Status)
	require.NotNil(t, resumed.ResumedBy)
	assert.Equal(t, agent.ID, *resumed.ResumedBy)
}

func TestApp_ResumeFromTransfer_DefaultAgentCannotResumeOwnManualTransferAssignedToAnotherAgent(t *testing.T) {
	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	creator := createTestAgent(t, app, org.ID)
	assignedAgent := createTestAgent(t, app, org.ID)
	contact := testutil.CreateTestContact(t, app.DB, org.ID)
	transfer := createTestTransfer(
		t,
		app,
		org.ID,
		contact.ID,
		"permission-test",
		models.TransferStatusActive,
		&assignedAgent.ID,
	)
	require.NoError(t, app.DB.Model(transfer).Updates(map[string]any{
		"transferred_by_user_id": creator.ID,
		"source":                 models.TransferSourceManual,
	}).Error)

	req := testutil.NewJSONRequest(t, nil)
	testutil.SetAuthContext(req, org.ID, creator.ID)
	testutil.SetPathParam(req, "id", transfer.ID.String())
	require.NoError(t, app.ResumeFromTransfer(req))
	require.Equal(t, fasthttp.StatusForbidden, testutil.GetResponseStatusCode(req))

	var stored models.AgentTransfer
	require.NoError(t, app.DB.First(&stored, "id = ?", transfer.ID).Error)
	assert.Equal(t, models.TransferStatusActive, stored.Status)
}

func TestApp_ResumeFromTransfer_NotFound(t *testing.T) {
	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	adminRole := testutil.CreateAdminRole(t, app.DB, org.ID)
	user := testutil.CreateTestUser(t, app.DB, org.ID, testutil.WithRoleID(&adminRole.ID))

	req := testutil.NewJSONRequest(t, nil)
	testutil.SetAuthContext(req, org.ID, user.ID)
	testutil.SetPathParam(req, "id", uuid.New().String())

	err := app.ResumeFromTransfer(req)
	require.NoError(t, err)
	assert.Equal(t, fasthttp.StatusNotFound, testutil.GetResponseStatusCode(req))

	var result map[string]any
	require.NoError(t, json.Unmarshal(testutil.GetResponseBody(req), &result))
	assert.Equal(t, "Transfer not found", result["message"])
}

func TestApp_ResumeFromTransfer_NotActive(t *testing.T) {
	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	adminRole := testutil.CreateAdminRole(t, app.DB, org.ID)
	user := testutil.CreateTestUser(t, app.DB, org.ID, testutil.WithRoleID(&adminRole.ID))
	account := testutil.CreateTestWhatsAppAccount(t, app.DB, org.ID)

	contact := testutil.CreateTestContact(t, app.DB, org.ID)
	transfer := createTestTransfer(t, app, org.ID, contact.ID, account.Name, models.TransferStatusResumed, nil) // Already resumed

	req := testutil.NewJSONRequest(t, nil)
	testutil.SetAuthContext(req, org.ID, user.ID)
	testutil.SetPathParam(req, "id", transfer.ID.String())

	err := app.ResumeFromTransfer(req)
	require.NoError(t, err)
	assert.Equal(t, fasthttp.StatusBadRequest, testutil.GetResponseStatusCode(req))

	var result map[string]any
	require.NoError(t, json.Unmarshal(testutil.GetResponseBody(req), &result))
	assert.Equal(t, "Transfer is not active", result["message"])
}

// --- AssignAgentTransfer Tests ---

func TestApp_AssignAgentTransfer_Success(t *testing.T) {
	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	adminRole := testutil.CreateAdminRole(t, app.DB, org.ID)
	user := testutil.CreateTestUser(t, app.DB, org.ID, testutil.WithRoleID(&adminRole.ID))
	account := testutil.CreateTestWhatsAppAccount(t, app.DB, org.ID)

	contact := testutil.CreateTestContact(t, app.DB, org.ID)
	agent := createTestAgent(t, app, org.ID)
	transfer := createTestTransfer(t, app, org.ID, contact.ID, account.Name, models.TransferStatusActive, nil)

	req := testutil.NewJSONRequest(t, map[string]any{
		"agent_id": agent.ID.String(),
	})
	testutil.SetAuthContext(req, org.ID, user.ID)
	testutil.SetPathParam(req, "id", transfer.ID.String())

	err := app.AssignAgentTransfer(req)
	require.NoError(t, err)
	assert.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req))

	var result struct {
		Status string `json:"status"`
		Data   struct {
			Message string     `json:"message"`
			AgentID *uuid.UUID `json:"agent_id"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(testutil.GetResponseBody(req), &result))

	assert.Equal(t, "success", result.Status)
	assert.Equal(t, "Transfer assigned successfully", result.Data.Message)
	assert.Equal(t, agent.ID, *result.Data.AgentID)

	// Verify transfer updated
	var updatedTransfer models.AgentTransfer
	require.NoError(t, app.DB.First(&updatedTransfer, transfer.ID).Error)
	assert.Equal(t, agent.ID, *updatedTransfer.AgentID)
}

func TestApp_AssignAgentTransfer_RequiresManagementPermissionForSelfAssignment(t *testing.T) {
	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	account := testutil.CreateTestWhatsAppAccount(t, app.DB, org.ID)

	contact := testutil.CreateTestContact(t, app.DB, org.ID)
	agent := createTestAgent(t, app, org.ID)
	transfer := createTestTransfer(t, app, org.ID, contact.ID, account.Name, models.TransferStatusActive, nil)

	// Omitted agent_id previously bypassed the write gate and self-assigned.
	req := testutil.NewJSONRequest(t, map[string]any{})
	testutil.SetAuthContext(req, org.ID, agent.ID)
	testutil.SetPathParam(req, "id", transfer.ID.String())

	err := app.AssignAgentTransfer(req)
	require.NoError(t, err)
	assert.Equal(t, fasthttp.StatusForbidden, testutil.GetResponseStatusCode(req))

	// Verify the transfer remains in the queue.
	var updatedTransfer models.AgentTransfer
	require.NoError(t, app.DB.First(&updatedTransfer, transfer.ID).Error)
	assert.Nil(t, updatedTransfer.AgentID)
}

func TestApp_AssignAgentTransfer_RequiresManagementPermissionToUnassign(t *testing.T) {
	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	account := testutil.CreateTestWhatsAppAccount(t, app.DB, org.ID)
	contact := testutil.CreateTestContact(t, app.DB, org.ID)
	caller := createTestAgent(t, app, org.ID)
	assignedAgent := createTestAgent(t, app, org.ID)
	transfer := createTestTransfer(
		t,
		app,
		org.ID,
		contact.ID,
		account.Name,
		models.TransferStatusActive,
		&assignedAgent.ID,
	)

	// An explicitly empty agent_id previously bypassed the write gate and
	// unassigned an arbitrary active transfer.
	req := testutil.NewJSONRequest(t, map[string]any{"agent_id": ""})
	testutil.SetAuthContext(req, org.ID, caller.ID)
	testutil.SetPathParam(req, "id", transfer.ID.String())

	require.NoError(t, app.AssignAgentTransfer(req))
	require.Equal(t, fasthttp.StatusForbidden, testutil.GetResponseStatusCode(req))

	var stored models.AgentTransfer
	require.NoError(t, app.DB.First(&stored, "id = ?", transfer.ID).Error)
	require.NotNil(t, stored.AgentID)
	assert.Equal(t, assignedAgent.ID, *stored.AgentID)
}

func TestApp_AssignAgentTransfer_AgentCannotAssignToOthers(t *testing.T) {
	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	account := testutil.CreateTestWhatsAppAccount(t, app.DB, org.ID)

	contact := testutil.CreateTestContact(t, app.DB, org.ID)
	agent := createTestAgent(t, app, org.ID)
	otherAgent := createTestAgent(t, app, org.ID)
	transfer := createTestTransfer(t, app, org.ID, contact.ID, account.Name, models.TransferStatusActive, nil)

	// Agent tries to assign to another agent - should fail
	req := testutil.NewJSONRequest(t, map[string]any{
		"agent_id": otherAgent.ID.String(),
	})
	testutil.SetAuthContext(req, org.ID, agent.ID)
	testutil.SetPathParam(req, "id", transfer.ID.String())

	err := app.AssignAgentTransfer(req)
	require.NoError(t, err)
	assert.Equal(t, fasthttp.StatusForbidden, testutil.GetResponseStatusCode(req))

	var result map[string]any
	require.NoError(t, json.Unmarshal(testutil.GetResponseBody(req), &result))
	assert.Equal(t, "You don't have permission to assign transfers", result["message"])
}

func TestApp_AssignAndResumeTransfer_SerializeWithoutReopening(t *testing.T) {
	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	adminRole := testutil.CreateAdminRole(t, app.DB, org.ID)
	manager := testutil.CreateTestUser(t, app.DB, org.ID, testutil.WithRoleID(&adminRole.ID))
	targetAgent := createTestAgent(t, app, org.ID)

	for attempt := 0; attempt < 8; attempt++ {
		contact := testutil.CreateTestContact(t, app.DB, org.ID)
		transfer := createTestTransfer(
			t,
			app,
			org.ID,
			contact.ID,
			"concurrency-test",
			models.TransferStatusActive,
			nil,
		)
		assignReq := testutil.NewJSONRequest(t, map[string]any{
			"agent_id": targetAgent.ID.String(),
		})
		testutil.SetAuthContext(assignReq, org.ID, manager.ID)
		testutil.SetPathParam(assignReq, "id", transfer.ID.String())
		resumeReq := testutil.NewJSONRequest(t, nil)
		testutil.SetAuthContext(resumeReq, org.ID, manager.ID)
		testutil.SetPathParam(resumeReq, "id", transfer.ID.String())

		start := make(chan struct{})
		errs := make(chan error, 2)
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			errs <- app.AssignAgentTransfer(assignReq)
		}()
		go func() {
			defer wg.Done()
			<-start
			errs <- app.ResumeFromTransfer(resumeReq)
		}()
		close(start)
		wg.Wait()
		close(errs)
		for handlerErr := range errs {
			require.NoError(t, handlerErr)
		}

		var stored models.AgentTransfer
		require.NoError(t, app.DB.First(&stored, "id = ?", transfer.ID).Error)
		assert.Equal(t, models.TransferStatusResumed, stored.Status)
		assert.NotNil(t, stored.ResumedAt)
		assert.NotNil(t, stored.ResumedBy)
		assert.Contains(
			t,
			[]int{
				fasthttp.StatusOK,
				fasthttp.StatusBadRequest,
			},
			testutil.GetResponseStatusCode(assignReq),
		)
		assert.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(resumeReq))
	}
}

func TestApp_AssignAgentTransfer_NotActive(t *testing.T) {
	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	adminRole := testutil.CreateAdminRole(t, app.DB, org.ID)
	user := testutil.CreateTestUser(t, app.DB, org.ID, testutil.WithRoleID(&adminRole.ID))
	account := testutil.CreateTestWhatsAppAccount(t, app.DB, org.ID)

	contact := testutil.CreateTestContact(t, app.DB, org.ID)
	agent := createTestAgent(t, app, org.ID)
	transfer := createTestTransfer(t, app, org.ID, contact.ID, account.Name, models.TransferStatusResumed, nil) // Not active

	req := testutil.NewJSONRequest(t, map[string]any{
		"agent_id": agent.ID.String(),
	})
	testutil.SetAuthContext(req, org.ID, user.ID)
	testutil.SetPathParam(req, "id", transfer.ID.String())

	err := app.AssignAgentTransfer(req)
	require.NoError(t, err)
	assert.Equal(t, fasthttp.StatusBadRequest, testutil.GetResponseStatusCode(req))

	var result map[string]any
	require.NoError(t, json.Unmarshal(testutil.GetResponseBody(req), &result))
	assert.Equal(t, "Transfer is not active", result["message"])
}

// --- PickNextTransfer Tests ---

func TestApp_PickNextTransfer_Success(t *testing.T) {
	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	account := testutil.CreateTestWhatsAppAccount(t, app.DB, org.ID)

	contact := testutil.CreateTestContact(t, app.DB, org.ID)
	agent := createTestAgent(t, app, org.ID)

	// Create unassigned transfer in general queue
	transfer := createTestTransfer(t, app, org.ID, contact.ID, account.Name, models.TransferStatusActive, nil)

	req := testutil.NewJSONRequest(t, nil)
	testutil.SetAuthContext(req, org.ID, agent.ID)

	err := app.PickNextTransfer(req)
	require.NoError(t, err)
	assert.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req))

	var result struct {
		Status string `json:"status"`
		Data   struct {
			Message  string                          `json:"message"`
			Transfer *handlers.AgentTransferResponse `json:"transfer"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(testutil.GetResponseBody(req), &result))

	assert.Equal(t, "success", result.Status)
	assert.Equal(t, "Transfer picked successfully", result.Data.Message)
	assert.NotNil(t, result.Data.Transfer)
	assert.Equal(t, transfer.ID.String(), result.Data.Transfer.ID)
	assert.Equal(t, agent.ID.String(), *result.Data.Transfer.AgentID)

	// Verify transfer updated in DB
	var updatedTransfer models.AgentTransfer
	require.NoError(t, app.DB.First(&updatedTransfer, transfer.ID).Error)
	assert.Equal(t, agent.ID, *updatedTransfer.AgentID)
}

func TestApp_PickNextTransfer_RequiresPickupOrWritePermissionWithoutMutation(t *testing.T) {
	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	role := testutil.CreateTestRoleWithKeys(t, app.DB, org.ID, "Transfer reader cannot pick", []string{
		models.ResourceTransfers + ":" + models.ActionRead,
	})
	user := testutil.CreateTestUser(t, app.DB, org.ID, testutil.WithRoleID(&role.ID))
	contact := testutil.CreateTestContact(t, app.DB, org.ID)
	transfer := createTestTransfer(t, app, org.ID, contact.ID, "permission-test", models.TransferStatusActive, nil)

	req := testutil.NewJSONRequest(t, nil)
	testutil.SetAuthContext(req, org.ID, user.ID)

	require.NoError(t, app.PickNextTransfer(req))
	require.Equal(t, fasthttp.StatusForbidden, testutil.GetResponseStatusCode(req))

	var stored models.AgentTransfer
	require.NoError(t, app.DB.First(&stored, "id = ?", transfer.ID).Error)
	assert.Nil(t, stored.AgentID)
}

func TestApp_PickNextTransfer_EmptyQueue(t *testing.T) {
	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)

	agent := createTestAgent(t, app, org.ID)

	// No transfers in queue
	req := testutil.NewJSONRequest(t, nil)
	testutil.SetAuthContext(req, org.ID, agent.ID)

	err := app.PickNextTransfer(req)
	require.NoError(t, err)
	assert.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req))

	var result struct {
		Status string `json:"status"`
		Data   struct {
			Message  string `json:"message"`
			Transfer any    `json:"transfer"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(testutil.GetResponseBody(req), &result))

	assert.Equal(t, "success", result.Status)
	assert.Equal(t, "No transfers in queue", result.Data.Message)
	assert.Nil(t, result.Data.Transfer)
}

func TestApp_PickNextTransfer_FIFO(t *testing.T) {
	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	account := testutil.CreateTestWhatsAppAccount(t, app.DB, org.ID)

	contact := testutil.CreateTestContact(t, app.DB, org.ID)
	newerContact := testutil.CreateTestContact(t, app.DB, org.ID)
	agent := createTestAgent(t, app, org.ID)

	// Create multiple transfers with different times
	transfer1 := &models.AgentTransfer{
		OrganizationID:  org.ID,
		ContactID:       contact.ID,
		WhatsAppAccount: account.Name,
		PhoneNumber:     "1111111111",
		Status:          models.TransferStatusActive,
		Source:          models.TransferSourceManual,
		TransferredAt:   time.Now().Add(-2 * time.Hour), // Oldest
	}
	require.NoError(t, app.DB.Create(transfer1).Error)

	transfer2 := &models.AgentTransfer{
		OrganizationID:  org.ID,
		ContactID:       newerContact.ID,
		WhatsAppAccount: account.Name,
		PhoneNumber:     "2222222222",
		Status:          models.TransferStatusActive,
		Source:          models.TransferSourceManual,
		TransferredAt:   time.Now().Add(-1 * time.Hour), // Newer
	}
	require.NoError(t, app.DB.Create(transfer2).Error)

	req := testutil.NewJSONRequest(t, nil)
	testutil.SetAuthContext(req, org.ID, agent.ID)

	err := app.PickNextTransfer(req)
	require.NoError(t, err)
	assert.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req))

	var result struct {
		Status string `json:"status"`
		Data   struct {
			Transfer *handlers.AgentTransferResponse `json:"transfer"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(testutil.GetResponseBody(req), &result))

	// Should pick the oldest transfer (FIFO)
	assert.Equal(t, transfer1.ID.String(), result.Data.Transfer.ID)
}

func TestApp_PickNextTransfer_TeamFiltering(t *testing.T) {
	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	account := testutil.CreateTestWhatsAppAccount(t, app.DB, org.ID)

	contact := testutil.CreateTestContact(t, app.DB, org.ID)
	generalContact := testutil.CreateTestContact(t, app.DB, org.ID)
	agent := createTestAgent(t, app, org.ID)

	// Create a team and add agent as member
	team := createTestTeam(t, app, org.ID, agent.ID)

	// Create transfer in team queue
	teamTransfer := &models.AgentTransfer{
		OrganizationID:  org.ID,
		ContactID:       contact.ID,
		WhatsAppAccount: account.Name,
		PhoneNumber:     "1111111111",
		Status:          models.TransferStatusActive,
		Source:          models.TransferSourceManual,
		TeamID:          &team.ID,
		TransferredAt:   time.Now(),
	}
	require.NoError(t, app.DB.Create(teamTransfer).Error)

	// Create transfer in general queue
	generalTransfer := createTestTransfer(t, app, org.ID, generalContact.ID, account.Name, models.TransferStatusActive, nil)

	// Pick from team queue specifically
	req := testutil.NewJSONRequest(t, nil)
	testutil.SetAuthContext(req, org.ID, agent.ID)
	testutil.SetQueryParam(req, "team_id", team.ID.String())

	err := app.PickNextTransfer(req)
	require.NoError(t, err)
	assert.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req))

	var result struct {
		Status string `json:"status"`
		Data   struct {
			Transfer *handlers.AgentTransferResponse `json:"transfer"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(testutil.GetResponseBody(req), &result))

	// Should pick from team queue, not general queue
	assert.Equal(t, teamTransfer.ID.String(), result.Data.Transfer.ID)
	assert.NotEqual(t, generalTransfer.ID.String(), result.Data.Transfer.ID)
}

func TestApp_PickNextTransfer_RejectsMalformedTeamWithoutMutation(t *testing.T) {
	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	account := testutil.CreateTestWhatsAppAccount(t, app.DB, org.ID)
	agent := createTestAgent(t, app, org.ID)
	otherTeam := createTestTeam(t, app, org.ID)
	contact := testutil.CreateTestContact(t, app.DB, org.ID)
	transfer := createTestTransfer(
		t,
		app,
		org.ID,
		contact.ID,
		account.Name,
		models.TransferStatusActive,
		nil,
	)
	require.NoError(t, app.DB.Model(&models.AgentTransfer{}).
		Where("id = ?", transfer.ID).
		Update("team_id", otherTeam.ID).Error)

	req := testutil.NewJSONRequest(t, nil)
	testutil.SetAuthContext(req, org.ID, agent.ID)
	testutil.SetQueryParam(req, "team_id", "not-a-uuid")
	require.NoError(t, app.PickNextTransfer(req))
	require.Equal(t, fasthttp.StatusBadRequest, testutil.GetResponseStatusCode(req))

	var stored models.AgentTransfer
	require.NoError(t, app.DB.First(&stored, "id = ?", transfer.ID).Error)
	assert.Nil(t, stored.AgentID)
	require.NotNil(t, stored.TeamID)
	assert.Equal(t, otherTeam.ID, *stored.TeamID)
}

// --- Cross-Organization Isolation Tests ---

func TestApp_AgentTransfers_CrossOrgIsolation(t *testing.T) {
	app := newTestApp(t)

	// Create two organizations
	org1 := testutil.CreateTestOrganization(t, app.DB)
	org2 := testutil.CreateTestOrganization(t, app.DB)

	adminRole1 := testutil.CreateAdminRole(t, app.DB, org1.ID)
	adminRole2 := testutil.CreateAdminRole(t, app.DB, org2.ID)
	user1 := testutil.CreateTestUser(t, app.DB, org1.ID, testutil.WithRoleID(&adminRole1.ID))
	user2 := testutil.CreateTestUser(t, app.DB, org2.ID, testutil.WithRoleID(&adminRole2.ID))

	account1 := testutil.CreateTestWhatsAppAccount(t, app.DB, org1.ID)
	account2 := testutil.CreateTestWhatsAppAccount(t, app.DB, org2.ID)

	contact1 := testutil.CreateTestContact(t, app.DB, org1.ID)
	contact2 := testutil.CreateTestContact(t, app.DB, org2.ID)

	// Create transfers in each org
	transfer1 := createTestTransfer(t, app, org1.ID, contact1.ID, account1.Name, models.TransferStatusActive, nil)
	transfer2 := createTestTransfer(t, app, org2.ID, contact2.ID, account2.Name, models.TransferStatusActive, nil)

	// User1 should only see org1's transfers
	req1 := testutil.NewGETRequest(t)
	testutil.SetAuthContext(req1, org1.ID, user1.ID)

	err := app.ListAgentTransfers(req1)
	require.NoError(t, err)
	assert.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req1))

	var result1 struct {
		Data struct {
			Transfers []handlers.AgentTransferResponse `json:"transfers"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(testutil.GetResponseBody(req1), &result1))

	assert.Len(t, result1.Data.Transfers, 1)
	assert.Equal(t, transfer1.ID.String(), result1.Data.Transfers[0].ID)

	// User2 should only see org2's transfers
	req2 := testutil.NewGETRequest(t)
	testutil.SetAuthContext(req2, org2.ID, user2.ID)

	err = app.ListAgentTransfers(req2)
	require.NoError(t, err)
	assert.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req2))

	var result2 struct {
		Data struct {
			Transfers []handlers.AgentTransferResponse `json:"transfers"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(testutil.GetResponseBody(req2), &result2))

	assert.Len(t, result2.Data.Transfers, 1)
	assert.Equal(t, transfer2.ID.String(), result2.Data.Transfers[0].ID)

	// User1 cannot resume org2's transfer
	req3 := testutil.NewJSONRequest(t, nil)
	testutil.SetAuthContext(req3, org1.ID, user1.ID)
	testutil.SetPathParam(req3, "id", transfer2.ID.String())

	err = app.ResumeFromTransfer(req3)
	require.NoError(t, err)
	assert.Equal(t, fasthttp.StatusNotFound, testutil.GetResponseStatusCode(req3))
}

// --- ReturnAgentTransfersToQueue Tests ---

func TestApp_ReturnAgentTransfersToQueue(t *testing.T) {
	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	account := testutil.CreateTestWhatsAppAccount(t, app.DB, org.ID)

	contact := testutil.CreateTestContact(t, app.DB, org.ID)
	secondContact := testutil.CreateTestContact(t, app.DB, org.ID)
	agent := createTestAgent(t, app, org.ID)

	// Create transfers assigned to the agent
	transfer1 := createTestTransfer(t, app, org.ID, contact.ID, account.Name, models.TransferStatusActive, &agent.ID)
	transfer2 := createTestTransfer(t, app, org.ID, secondContact.ID, account.Name, models.TransferStatusActive, &agent.ID)

	// Return transfers to queue
	count := app.ReturnAgentTransfersToQueue(agent.ID, org.ID)

	assert.Equal(t, 2, count)

	// Verify transfers are unassigned
	var updatedTransfer1, updatedTransfer2 models.AgentTransfer
	require.NoError(t, app.DB.First(&updatedTransfer1, transfer1.ID).Error)
	require.NoError(t, app.DB.First(&updatedTransfer2, transfer2.ID).Error)

	assert.Nil(t, updatedTransfer1.AgentID)
	assert.Nil(t, updatedTransfer2.AgentID)
}

// --- Pickup / assign respect AssignToSameAgent setting ---
//
// These tests pin down the rule shared by PickNextTransfer,
// AssignAgentTransfer and saveAndFinalizeTransfer: contact.assigned_user_id
// is only written when AssignToSameAgent is enabled and no relationship
// manager is already set. That rule was introduced to stop pickup from
// silently making the agent the contact's permanent owner.

// upsertChatbotSettings creates / updates default chatbot settings for an
// org with the AssignToSameAgent toggle.
//
// gorm gotcha: AgentAssignmentConfig columns carry `default:true` tags, so
// passing AssignToSameAgent=false through a struct INSERT silently falls
// back to the column DEFAULT (Go zero value === missing in the SQL). We
// raw-update both flags explicitly so the test sees the value we asked for.
func upsertChatbotSettings(t *testing.T, app *handlers.App, orgID uuid.UUID, assignToSameAgent bool) {
	t.Helper()
	require.NoError(t, app.DB.Where("organization_id = ? AND whats_app_account = ?", orgID, "").
		Delete(&models.ChatbotSettings{}).Error)
	settings := &models.ChatbotSettings{OrganizationID: orgID}
	require.NoError(t, app.DB.Create(settings).Error)
	require.NoError(t, app.DB.Model(&models.ChatbotSettings{}).
		Where("id = ?", settings.ID).
		Updates(map[string]any{
			"assign_to_same_agent":     assignToSameAgent,
			"allow_agent_queue_pickup": true,
		}).Error)
	app.InvalidateChatbotSettingsCache(orgID)
}

// readContactAssignedUser returns the current assigned_user_id for a contact.
func readContactAssignedUser(t *testing.T, app *handlers.App, contactID uuid.UUID) *uuid.UUID {
	t.Helper()
	var contact models.Contact
	require.NoError(t, app.DB.Where("id = ?", contactID).First(&contact).Error)
	return contact.AssignedUserID
}

func TestApp_PickNextTransfer_AssignToSameAgentTrue_PinsRelationshipManager(t *testing.T) {
	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	account := testutil.CreateTestWhatsAppAccount(t, app.DB, org.ID)
	upsertChatbotSettings(t, app, org.ID, true)

	contact := testutil.CreateTestContact(t, app.DB, org.ID)
	agent := createTestAgent(t, app, org.ID)
	createTestTransfer(t, app, org.ID, contact.ID, account.Name, models.TransferStatusActive, nil)

	req := testutil.NewJSONRequest(t, nil)
	testutil.SetAuthContext(req, org.ID, agent.ID)
	require.NoError(t, app.PickNextTransfer(req))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req))

	got := readContactAssignedUser(t, app, contact.ID)
	require.NotNil(t, got, "expected contact to be pinned to the agent under AssignToSameAgent=true")
	assert.Equal(t, agent.ID, *got)
}

func TestApp_PickNextTransfer_AssignToSameAgentFalse_LeavesContactUnassigned(t *testing.T) {
	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	account := testutil.CreateTestWhatsAppAccount(t, app.DB, org.ID)
	upsertChatbotSettings(t, app, org.ID, false)

	contact := testutil.CreateTestContact(t, app.DB, org.ID)
	agent := createTestAgent(t, app, org.ID)
	createTestTransfer(t, app, org.ID, contact.ID, account.Name, models.TransferStatusActive, nil)

	req := testutil.NewJSONRequest(t, nil)
	testutil.SetAuthContext(req, org.ID, agent.ID)
	require.NoError(t, app.PickNextTransfer(req))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req))

	// The contact must NOT be pinned. Visibility during the active transfer
	// comes from agent_transfers; once the transfer resumes the agent should
	// lose access cleanly.
	assert.Nil(t, readContactAssignedUser(t, app, contact.ID),
		"AssignToSameAgent=false: pickup must not write contact.assigned_user_id")
}

func TestApp_PickNextTransfer_DoesNotOverwriteExistingRelationshipManager(t *testing.T) {
	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	account := testutil.CreateTestWhatsAppAccount(t, app.DB, org.ID)
	upsertChatbotSettings(t, app, org.ID, true)

	contact := testutil.CreateTestContact(t, app.DB, org.ID)
	managerRole := testutil.CreateAdminRole(t, app.DB, org.ID)
	manager := testutil.CreateTestUser(t, app.DB, org.ID, testutil.WithRoleID(&managerRole.ID))

	// Pin the contact to a manager up-front (e.g. via /api/contacts/.../assign).
	require.NoError(t, app.DB.Model(&models.Contact{}).Where("id = ?", contact.ID).
		Update("assigned_user_id", manager.ID).Error)

	agent := createTestAgent(t, app, org.ID)
	createTestTransfer(t, app, org.ID, contact.ID, account.Name, models.TransferStatusActive, nil)

	req := testutil.NewJSONRequest(t, nil)
	testutil.SetAuthContext(req, org.ID, agent.ID)
	require.NoError(t, app.PickNextTransfer(req))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req))

	got := readContactAssignedUser(t, app, contact.ID)
	require.NotNil(t, got)
	assert.Equal(t, manager.ID, *got, "pickup must not overwrite a manually set relationship manager")
}

func TestApp_ReturnAgentTransfersToQueue_DoesNotClearManualAssignment(t *testing.T) {
	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	account := testutil.CreateTestWhatsAppAccount(t, app.DB, org.ID)

	contact := testutil.CreateTestContact(t, app.DB, org.ID)
	agent := createTestAgent(t, app, org.ID)
	managerRole := testutil.CreateAdminRole(t, app.DB, org.ID)
	manager := testutil.CreateTestUser(t, app.DB, org.ID, testutil.WithRoleID(&managerRole.ID))

	// Manager is the relationship manager. Agent is currently handling a
	// transfer (different person). When the agent goes offline the manager
	// pointer must survive.
	require.NoError(t, app.DB.Model(&models.Contact{}).Where("id = ?", contact.ID).
		Update("assigned_user_id", manager.ID).Error)
	createTestTransfer(t, app, org.ID, contact.ID, account.Name, models.TransferStatusActive, &agent.ID)

	count := app.ReturnAgentTransfersToQueue(agent.ID, org.ID)
	assert.Equal(t, 1, count)

	got := readContactAssignedUser(t, app, contact.ID)
	require.NotNil(t, got, "manual relationship manager must not be cleared")
	assert.Equal(t, manager.ID, *got)
}

func TestApp_ReturnAgentTransfersToQueue_ClearsAssignmentWhenItPointsAtAgent(t *testing.T) {
	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	account := testutil.CreateTestWhatsAppAccount(t, app.DB, org.ID)

	contact := testutil.CreateTestContact(t, app.DB, org.ID)
	agent := createTestAgent(t, app, org.ID)

	// Agent is both the transfer's owner and the contact's stale RM (e.g.
	// from an earlier pickup with AssignToSameAgent=true). When they go
	// offline the contact must return to "no manager" so the queue is the
	// authoritative routing path.
	require.NoError(t, app.DB.Model(&models.Contact{}).Where("id = ?", contact.ID).
		Update("assigned_user_id", agent.ID).Error)
	createTestTransfer(t, app, org.ID, contact.ID, account.Name, models.TransferStatusActive, &agent.ID)

	count := app.ReturnAgentTransfersToQueue(agent.ID, org.ID)
	assert.Equal(t, 1, count)

	assert.Nil(t, readContactAssignedUser(t, app, contact.ID),
		"assignment pointing at the offline agent must be cleared")
}

// --- Cross-organization membership (Pause AI root cause) ---
//
// users.organization_id is only a user's home organization. Members who work
// in another organization are recorded in user_organizations, so agent lookups
// must resolve membership there, and must still reject reseller-derived
// memberships whose reseller assignment is no longer active.

// addDirectOrgMembership gives an existing user a direct membership in a
// second organization, as AddOrganizationMember or tenant creation does.
func addDirectOrgMembership(t *testing.T, app *handlers.App, userID, orgID uuid.UUID, roleID *uuid.UUID) *models.UserOrganization {
	t.Helper()

	membership := &models.UserOrganization{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		UserID:         userID,
		OrganizationID: orgID,
		RoleID:         roleID,
		Source:         models.MembershipSourceDirect,
	}
	require.NoError(t, app.DB.Create(membership).Error)
	return membership
}

// addResellerDerivedMembership makes userID an active reseller admin and
// materializes the reseller-derived membership in orgID, which must belong to
// the reseller.
func addResellerDerivedMembership(t *testing.T, app *handlers.App, userID uuid.UUID, reseller *models.Reseller, orgID uuid.UUID, roleID *uuid.UUID) *models.UserOrganization {
	t.Helper()

	member := &models.ResellerMember{
		BaseModel:  models.BaseModel{ID: uuid.New()},
		ResellerID: reseller.ID,
		UserID:     userID,
		Role:       models.ResellerRoleAdmin,
		IsActive:   true,
	}
	require.NoError(t, app.DB.Create(member).Error)

	membership := &models.UserOrganization{
		BaseModel:        models.BaseModel{ID: uuid.New()},
		UserID:           userID,
		OrganizationID:   orgID,
		RoleID:           roleID,
		Source:           models.MembershipSourceReseller,
		ResellerMemberID: &member.ID,
	}
	require.NoError(t, app.DB.Create(membership).Error)
	return membership
}

func suspendTestReseller(t *testing.T, app *handlers.App, resellerID uuid.UUID) {
	t.Helper()
	require.NoError(t, app.DB.Model(&models.Reseller{}).
		Where("id = ?", resellerID).
		Update("status", models.ResellerStatusSuspended).Error)
}

func createTransferRequest(t *testing.T, orgID, userID, contactID uuid.UUID, accountName string, agentID uuid.UUID) *fastglue.Request {
	t.Helper()
	req := testutil.NewJSONRequest(t, map[string]any{
		"contact_id":       contactID.String(),
		"whatsapp_account": accountName,
		"agent_id":         agentID.String(),
	})
	testutil.SetAuthContext(req, orgID, userID)
	return req
}

func assertNotFoundMessage(t *testing.T, req *fastglue.Request, message string) {
	t.Helper()
	assert.Equal(t, fasthttp.StatusNotFound, testutil.GetResponseStatusCode(req))
	var result map[string]any
	require.NoError(t, json.Unmarshal(testutil.GetResponseBody(req), &result))
	assert.Equal(t, message, result["message"])
}

func TestApp_CreateAgentTransfer_SelfPauseAsCrossOrgMember(t *testing.T) {
	app := newTestApp(t)
	homeOrg := testutil.CreateTestOrganization(t, app.DB)
	workOrg := testutil.CreateTestOrganization(t, app.DB)
	workAdminRole := testutil.CreateAdminRole(t, app.DB, workOrg.ID)

	// Home org A; direct membership in org B where the user actually works.
	user := testutil.CreateTestUser(t, app.DB, homeOrg.ID)
	addDirectOrgMembership(t, app, user.ID, workOrg.ID, &workAdminRole.ID)

	account := testutil.CreateTestWhatsAppAccount(t, app.DB, workOrg.ID)
	contact := testutil.CreateTestContact(t, app.DB, workOrg.ID)

	req := createTransferRequest(t, workOrg.ID, user.ID, contact.ID, account.Name, user.ID)
	require.NoError(t, app.CreateAgentTransfer(req))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req), string(testutil.GetResponseBody(req)))

	var result struct {
		Data struct {
			Transfer handlers.AgentTransferResponse `json:"transfer"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(testutil.GetResponseBody(req), &result))
	require.NotNil(t, result.Data.Transfer.AgentID)
	assert.Equal(t, user.ID.String(), *result.Data.Transfer.AgentID)

	var stored models.AgentTransfer
	require.NoError(t, app.DB.Where("organization_id = ? AND contact_id = ?", workOrg.ID, contact.ID).First(&stored).Error)
	require.NotNil(t, stored.AgentID)
	assert.Equal(t, user.ID, *stored.AgentID)
	assert.Equal(t, models.TransferStatusActive, stored.Status)
}

func TestApp_CreateAgentTransfer_CrossOrgMemberStillNeedsAvailability(t *testing.T) {
	app := newTestApp(t)
	homeOrg := testutil.CreateTestOrganization(t, app.DB)
	workOrg := testutil.CreateTestOrganization(t, app.DB)
	workAdminRole := testutil.CreateAdminRole(t, app.DB, workOrg.ID)

	user := testutil.CreateTestUser(t, app.DB, homeOrg.ID)
	addDirectOrgMembership(t, app, user.ID, workOrg.ID, &workAdminRole.ID)
	require.NoError(t, app.DB.Model(user).Update("is_available", false).Error)

	account := testutil.CreateTestWhatsAppAccount(t, app.DB, workOrg.ID)
	contact := testutil.CreateTestContact(t, app.DB, workOrg.ID)

	// Policy: the availability check is unchanged, also for a self-pause.
	req := createTransferRequest(t, workOrg.ID, user.ID, contact.ID, account.Name, user.ID)
	require.NoError(t, app.CreateAgentTransfer(req))
	assert.Equal(t, fasthttp.StatusBadRequest, testutil.GetResponseStatusCode(req))
	var result map[string]any
	require.NoError(t, json.Unmarshal(testutil.GetResponseBody(req), &result))
	assert.Equal(t, "Agent is currently away", result["message"])
}

func TestApp_CreateAgentTransfer_RejectsSuspendedResellerMembership(t *testing.T) {
	app := newTestApp(t)
	reseller := testutil.CreateTestReseller(t, app.DB)
	homeOrg := testutil.CreateTestOrganization(t, app.DB)
	clinicOrg := testutil.CreateTestOrganizationForReseller(t, app.DB, reseller.ID)
	clinicAdminRole := testutil.CreateAdminRole(t, app.DB, clinicOrg.ID)
	clinicAdmin := testutil.CreateTestUser(t, app.DB, clinicOrg.ID, testutil.WithRoleID(&clinicAdminRole.ID))

	resellerAdmin := testutil.CreateTestUser(t, app.DB, homeOrg.ID)
	addResellerDerivedMembership(t, app, resellerAdmin.ID, reseller, clinicOrg.ID, &clinicAdminRole.ID)

	account := testutil.CreateTestWhatsAppAccount(t, app.DB, clinicOrg.ID)
	activeContact := testutil.CreateTestContact(t, app.DB, clinicOrg.ID)
	suspendedContact := testutil.CreateTestContact(t, app.DB, clinicOrg.ID)

	// Control: while the reseller is active the derived membership counts.
	activeReq := createTransferRequest(t, clinicOrg.ID, clinicAdmin.ID, activeContact.ID, account.Name, resellerAdmin.ID)
	require.NoError(t, app.CreateAgentTransfer(activeReq))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(activeReq), string(testutil.GetResponseBody(activeReq)))

	suspendTestReseller(t, app, reseller.ID)

	suspendedReq := createTransferRequest(t, clinicOrg.ID, clinicAdmin.ID, suspendedContact.ID, account.Name, resellerAdmin.ID)
	require.NoError(t, app.CreateAgentTransfer(suspendedReq))
	assertNotFoundMessage(t, suspendedReq, "Agent not found")

	var count int64
	require.NoError(t, app.DB.Model(&models.AgentTransfer{}).
		Where("organization_id = ? AND contact_id = ?", clinicOrg.ID, suspendedContact.ID).
		Count(&count).Error)
	assert.Zero(t, count, "a rejected agent must not create a transfer")
}

func TestApp_CreateAgentTransfer_RejectsNonMemberAgent(t *testing.T) {
	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	otherOrg := testutil.CreateTestOrganization(t, app.DB)
	adminRole := testutil.CreateAdminRole(t, app.DB, org.ID)
	admin := testutil.CreateTestUser(t, app.DB, org.ID, testutil.WithRoleID(&adminRole.ID))
	outsider := testutil.CreateTestUser(t, app.DB, otherOrg.ID)

	account := testutil.CreateTestWhatsAppAccount(t, app.DB, org.ID)
	contact := testutil.CreateTestContact(t, app.DB, org.ID)

	req := createTransferRequest(t, org.ID, admin.ID, contact.ID, account.Name, outsider.ID)
	require.NoError(t, app.CreateAgentTransfer(req))
	assertNotFoundMessage(t, req, "Agent not found")
}

func TestApp_CreateAgentTransfer_RejectsRemovedCrossOrgMember(t *testing.T) {
	app := newTestApp(t)
	homeOrg := testutil.CreateTestOrganization(t, app.DB)
	workOrg := testutil.CreateTestOrganization(t, app.DB)
	adminRole := testutil.CreateAdminRole(t, app.DB, workOrg.ID)
	admin := testutil.CreateTestUser(t, app.DB, workOrg.ID, testutil.WithRoleID(&adminRole.ID))

	former := testutil.CreateTestUser(t, app.DB, homeOrg.ID)
	membership := addDirectOrgMembership(t, app, former.ID, workOrg.ID, nil)
	// RemoveOrganizationMember soft-deletes the membership row.
	require.NoError(t, app.DB.Delete(membership).Error)

	account := testutil.CreateTestWhatsAppAccount(t, app.DB, workOrg.ID)
	contact := testutil.CreateTestContact(t, app.DB, workOrg.ID)

	req := createTransferRequest(t, workOrg.ID, admin.ID, contact.ID, account.Name, former.ID)
	require.NoError(t, app.CreateAgentTransfer(req))
	assertNotFoundMessage(t, req, "Agent not found")
}

func TestApp_CreateAgentTransfer_RejectsInactiveAgent(t *testing.T) {
	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	adminRole := testutil.CreateAdminRole(t, app.DB, org.ID)
	admin := testutil.CreateTestUser(t, app.DB, org.ID, testutil.WithRoleID(&adminRole.ID))
	inactive := testutil.CreateTestUser(t, app.DB, org.ID, testutil.WithInactive())

	account := testutil.CreateTestWhatsAppAccount(t, app.DB, org.ID)
	contact := testutil.CreateTestContact(t, app.DB, org.ID)

	req := createTransferRequest(t, org.ID, admin.ID, contact.ID, account.Name, inactive.ID)
	require.NoError(t, app.CreateAgentTransfer(req))
	assertNotFoundMessage(t, req, "Agent not found")
}

func TestApp_CreateAgentTransfer_HomeOrgAgentWithoutMembershipRowUnchanged(t *testing.T) {
	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	adminRole := testutil.CreateAdminRole(t, app.DB, org.ID)
	admin := testutil.CreateTestUser(t, app.DB, org.ID, testutil.WithRoleID(&adminRole.ID))
	legacyAgent := createTestAgent(t, app, org.ID)
	// Legacy account: home organization only, no user_organizations row.
	require.NoError(t, app.DB.Unscoped().
		Where("user_id = ? AND organization_id = ?", legacyAgent.ID, org.ID).
		Delete(&models.UserOrganization{}).Error)

	account := testutil.CreateTestWhatsAppAccount(t, app.DB, org.ID)
	contact := testutil.CreateTestContact(t, app.DB, org.ID)

	req := createTransferRequest(t, org.ID, admin.ID, contact.ID, account.Name, legacyAgent.ID)
	require.NoError(t, app.CreateAgentTransfer(req))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req), string(testutil.GetResponseBody(req)))
}

func TestApp_AssignAgentTransfer_CrossOrgMember(t *testing.T) {
	app := newTestApp(t)
	homeOrg := testutil.CreateTestOrganization(t, app.DB)
	workOrg := testutil.CreateTestOrganization(t, app.DB)
	adminRole := testutil.CreateAdminRole(t, app.DB, workOrg.ID)
	admin := testutil.CreateTestUser(t, app.DB, workOrg.ID, testutil.WithRoleID(&adminRole.ID))
	agentRole := testutil.CreateAgentRole(t, app.DB, workOrg.ID)

	agent := testutil.CreateTestUser(t, app.DB, homeOrg.ID, testutil.WithFullName("Cross Org Agent"))
	addDirectOrgMembership(t, app, agent.ID, workOrg.ID, &agentRole.ID)

	account := testutil.CreateTestWhatsAppAccount(t, app.DB, workOrg.ID)
	contact := testutil.CreateTestContact(t, app.DB, workOrg.ID)
	transfer := createTestTransfer(t, app, workOrg.ID, contact.ID, account.Name, models.TransferStatusActive, nil)

	req := testutil.NewJSONRequest(t, map[string]any{"agent_id": agent.ID.String()})
	testutil.SetAuthContext(req, workOrg.ID, admin.ID)
	testutil.SetPathParam(req, "id", transfer.ID.String())

	require.NoError(t, app.AssignAgentTransfer(req))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req), string(testutil.GetResponseBody(req)))

	var updated models.AgentTransfer
	require.NoError(t, app.DB.First(&updated, transfer.ID).Error)
	require.NotNil(t, updated.AgentID)
	assert.Equal(t, agent.ID, *updated.AgentID)
}

func TestApp_AssignAgentTransfer_RejectsSuspendedResellerMembership(t *testing.T) {
	app := newTestApp(t)
	reseller := testutil.CreateTestReseller(t, app.DB)
	homeOrg := testutil.CreateTestOrganization(t, app.DB)
	clinicOrg := testutil.CreateTestOrganizationForReseller(t, app.DB, reseller.ID)
	adminRole := testutil.CreateAdminRole(t, app.DB, clinicOrg.ID)
	admin := testutil.CreateTestUser(t, app.DB, clinicOrg.ID, testutil.WithRoleID(&adminRole.ID))

	resellerAdmin := testutil.CreateTestUser(t, app.DB, homeOrg.ID)
	addResellerDerivedMembership(t, app, resellerAdmin.ID, reseller, clinicOrg.ID, &adminRole.ID)
	suspendTestReseller(t, app, reseller.ID)

	account := testutil.CreateTestWhatsAppAccount(t, app.DB, clinicOrg.ID)
	contact := testutil.CreateTestContact(t, app.DB, clinicOrg.ID)
	transfer := createTestTransfer(t, app, clinicOrg.ID, contact.ID, account.Name, models.TransferStatusActive, nil)

	req := testutil.NewJSONRequest(t, map[string]any{"agent_id": resellerAdmin.ID.String()})
	testutil.SetAuthContext(req, clinicOrg.ID, admin.ID)
	testutil.SetPathParam(req, "id", transfer.ID.String())

	require.NoError(t, app.AssignAgentTransfer(req))
	assertNotFoundMessage(t, req, "Agent not found")

	var unchanged models.AgentTransfer
	require.NoError(t, app.DB.First(&unchanged, transfer.ID).Error)
	assert.Nil(t, unchanged.AgentID)
}

func TestApp_CreateAgentTransfer_RejectsRemovedHomeOrgMember(t *testing.T) {
	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	adminRole := testutil.CreateAdminRole(t, app.DB, org.ID)
	admin := testutil.CreateTestUser(t, app.DB, org.ID, testutil.WithRoleID(&adminRole.ID))

	// RemoveOrganizationMember soft-deletes the membership row but leaves
	// users.organization_id pointing at the organization. The auth layer
	// rejects such a user, so the agent lookup must too.
	former := createTestAgent(t, app, org.ID)
	require.NoError(t, app.DB.
		Where("user_id = ? AND organization_id = ?", former.ID, org.ID).
		Delete(&models.UserOrganization{}).Error)

	account := testutil.CreateTestWhatsAppAccount(t, app.DB, org.ID)
	contact := testutil.CreateTestContact(t, app.DB, org.ID)

	req := createTransferRequest(t, org.ID, admin.ID, contact.ID, account.Name, former.ID)
	require.NoError(t, app.CreateAgentTransfer(req))
	assertNotFoundMessage(t, req, "Agent not found")
}

// Documented policy: a super admin gets no bypass in the agent lookup. A
// super admin working in an organization where they have no membership row
// (and which is not their home organization) cannot self-pause with their own
// agent_id; the frontend falls back to an unassigned pause.
func TestApp_CreateAgentTransfer_SuperAdminSelfPauseWithoutMembershipIsNotFound(t *testing.T) {
	app := newTestApp(t)
	homeOrg := testutil.CreateTestOrganization(t, app.DB)
	clinicOrg := testutil.CreateTestOrganization(t, app.DB)
	superAdmin := testutil.CreateTestUser(t, app.DB, homeOrg.ID, testutil.WithSuperAdmin())

	account := testutil.CreateTestWhatsAppAccount(t, app.DB, clinicOrg.ID)
	contact := testutil.CreateTestContact(t, app.DB, clinicOrg.ID)

	req := createTransferRequest(t, clinicOrg.ID, superAdmin.ID, contact.ID, account.Name, superAdmin.ID)
	require.NoError(t, app.CreateAgentTransfer(req))
	assertNotFoundMessage(t, req, "Agent not found")

	var count int64
	require.NoError(t, app.DB.Model(&models.AgentTransfer{}).
		Where("organization_id = ? AND contact_id = ?", clinicOrg.ID, contact.ID).
		Count(&count).Error)
	assert.Zero(t, count, "a rejected agent must not create a transfer")
}
