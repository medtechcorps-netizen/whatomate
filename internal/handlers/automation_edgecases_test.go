package handlers

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
	"gorm.io/gorm"
)

func TestAutomationPolicy_Edge_SharedTriggerWarningsAndSemanticDuplicate(t *testing.T) {
	app, org, user := newAutomationDatabaseTestApp(t)
	eventType := string(models.CustomerActivityContactCreated)

	originalGraph := automationTestGraph(eventType, "Call new customer")
	original := createAutomationPolicyForTest(
		t,
		app,
		org.ID,
		user.ID,
		"Original behavior",
		originalGraph,
	)
	activateOriginal := automationRequest(
		t,
		org.ID,
		user.ID,
		original.ID,
		AutomationPolicyVersionRequest{Version: original.Version},
	)
	require.NoError(t, app.ActivateAutomationPolicy(activateOriginal))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(activateOriginal))

	// This graph is deliberately serialized differently: node IDs, node order,
	// edge endpoints, and canvas positions all changed while behavior did not.
	semanticDuplicate := AutomationGraph{
		Nodes: []AutomationGraphNode{
			{
				ID:   "action-copy",
				Type: automationNodeCreateTask,
				Config: map[string]any{
					"title":    "Call new customer",
					"priority": "normal",
					"owner":    "unassigned",
				},
				Position: &AutomationNodePosition{X: 925, Y: -410},
			},
			{
				ID:       "trigger-copy",
				Type:     automationNodeTrigger,
				Config:   map[string]any{"event_type": eventType},
				Position: &AutomationNodePosition{X: -725, Y: 360},
			},
		},
		Edges: []AutomationGraphEdge{
			{Source: "trigger-copy", Target: "action-copy"},
		},
	}
	duplicate := createAutomationPolicyForTest(
		t,
		app,
		org.ID,
		user.ID,
		"Semantic duplicate",
		semanticDuplicate,
	)

	previewDuplicate := automationRequest(
		t,
		org.ID,
		user.ID,
		duplicate.ID,
		PreviewAutomationPolicyRequest{
			Version: duplicate.Version,
			Graph:   &semanticDuplicate,
			Event: &AutomationPreviewEvent{
				EventType: eventType,
			},
		},
	)
	require.NoError(t, app.PreviewAutomationPolicy(previewDuplicate))
	var duplicatePreview AutomationPolicyPreviewResponse
	testutil.ParseEnvelopeResponse(t, previewDuplicate, &duplicatePreview)
	require.True(t, duplicatePreview.Valid)
	requireAutomationEdgeWarningCode(
		t,
		duplicatePreview.Warnings,
		"shared_trigger_active_policy",
	)

	activateDuplicate := automationRequest(
		t,
		org.ID,
		user.ID,
		duplicate.ID,
		AutomationPolicyVersionRequest{Version: duplicate.Version},
	)
	require.NoError(t, app.ActivateAutomationPolicy(activateDuplicate))
	testutil.AssertErrorResponse(
		t,
		activateDuplicate,
		fasthttp.StatusConflict,
		"already has the same behavior",
	)

	differentGraph := automationTestGraph(eventType, "Escalate new customer")
	different := createAutomationPolicyForTest(
		t,
		app,
		org.ID,
		user.ID,
		"Different behavior",
		differentGraph,
	)
	activateDifferent := automationRequest(
		t,
		org.ID,
		user.ID,
		different.ID,
		AutomationPolicyVersionRequest{Version: different.Version},
	)
	require.NoError(t, app.ActivateAutomationPolicy(activateDifferent))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(activateDifferent))
	var activeDifferent AutomationPolicyResponse
	testutil.ParseEnvelopeResponse(t, activateDifferent, &activeDifferent)
	require.Equal(t, models.AutomationPolicyStatusActive, activeDifferent.Status)
	requireAutomationEdgeWarningCode(
		t,
		activeDifferent.ValidationWarnings,
		"shared_trigger_active_policy",
	)
}

func TestAutomationPolicy_Edge_LegacyOwnershipPinsMarketingOptOutDecision(t *testing.T) {
	testCases := []struct {
		name         string
		initialValue bool
		owned        bool
		status       models.AutomationExecutionStatus
		jobCount     int64
		taskCount    int64
	}{
		{
			name:         "false to true retains the planned task",
			initialValue: false,
			owned:        true,
			status:       models.AutomationExecutionStatusProcessing,
			jobCount:     1,
			taskCount:    1,
		},
		{
			name:         "true to false retains the skipped decision",
			initialValue: true,
			owned:        false,
			status:       models.AutomationExecutionStatusSkipped,
			jobCount:     0,
			taskCount:    0,
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			app, org, user := newAutomationDatabaseTestApp(t)
			contact := testutil.CreateTestContact(t, app.DB, org.ID)
			require.NoError(t, app.DB.Model(&models.Contact{}).
				Where("id = ? AND organization_id = ?", contact.ID, org.ID).
				Update("marketing_opt_out", testCase.initialValue).Error)

			graph := automationMarketingOptOutGraph(
				string(models.CustomerActivityContactCreated),
			)
			policy, _ := createActiveAutomationPolicyForTest(
				t,
				app.DB,
				org.ID,
				user.ID,
				"Marketing preference",
				graph,
			)
			event := createAutomationActivityForTest(
				t,
				app.DB,
				org.ID,
				contact.ID,
				models.CustomerActivityContactCreated,
			)
			var receipt models.AutomationEventReceipt
			require.NoError(t, app.DB.Where(
				"organization_id = ? AND activity_event_id = ?",
				org.ID,
				event.ID,
			).First(&receipt).Error)

			var initiallyOwned bool
			require.NoError(t, app.DB.Transaction(func(tx *gorm.DB) error {
				var err error
				initiallyOwned, err = automationPolicyOwnsCareEvent(
					tx,
					org.ID,
					event,
					receipt.IngestedAt,
				)
				return err
			}))
			require.Equal(t, testCase.owned, initiallyOwned)

			var initialExecution models.AutomationExecution
			require.NoError(t, app.DB.Where(
				"organization_id = ? AND policy_id = ? AND activity_event_id = ?",
				org.ID,
				policy.ID,
				event.ID,
			).First(&initialExecution).Error)
			require.Equal(t, testCase.status, initialExecution.Status)

			// Flip the mutable contact flag after the legacy decision has
			// materialized. Receipt processing must keep that decision pinned.
			require.NoError(t, app.DB.Model(&models.Contact{}).
				Where("id = ? AND organization_id = ?", contact.ID, org.ID).
				Update("marketing_opt_out", !testCase.initialValue).Error)

			processor := NewAutomationPolicyProcessor(app, time.Second)
			claimedReceipt, err := processor.claimReceipt(context.Background(), org.ID)
			require.NoError(t, err)
			require.NotNil(t, claimedReceipt)
			require.Equal(t, receipt.ID, claimedReceipt.ID)
			require.NoError(t, processor.processReceipt(
				context.Background(),
				org.ID,
				claimedReceipt,
			))

			var executionCount int64
			require.NoError(t, app.DB.Model(&models.AutomationExecution{}).
				Where(
					"organization_id = ? AND policy_id = ? AND activity_event_id = ?",
					org.ID,
					policy.ID,
					event.ID,
				).
				Count(&executionCount).Error)
			require.EqualValues(t, 1, executionCount)

			var pinnedExecution models.AutomationExecution
			require.NoError(t, app.DB.Where(
				"organization_id = ? AND policy_id = ? AND activity_event_id = ?",
				org.ID,
				policy.ID,
				event.ID,
			).First(&pinnedExecution).Error)
			require.Equal(t, testCase.status, pinnedExecution.Status)

			var jobCount int64
			require.NoError(t, app.DB.Model(&models.ScheduledJob{}).
				Where(
					"organization_id = ? AND kind = ?",
					org.ID,
					automationCreateTaskJobKind,
				).
				Count(&jobCount).Error)
			require.Equal(t, testCase.jobCount, jobCount)

			var ownedAfterFlip bool
			require.NoError(t, app.DB.Transaction(func(tx *gorm.DB) error {
				var err error
				ownedAfterFlip, err = automationPolicyOwnsCareEvent(
					tx,
					org.ID,
					event,
					receipt.IngestedAt,
				)
				return err
			}))
			require.Equal(t, testCase.owned, ownedAfterFlip)

			if testCase.jobCount == 1 {
				job, err := processor.claimActionJob(context.Background(), org.ID)
				require.NoError(t, err)
				require.NotNil(t, job)
				require.NoError(t, processor.processActionJob(
					context.Background(),
					org.ID,
					job,
				))
			} else {
				job, err := processor.claimActionJob(context.Background(), org.ID)
				require.NoError(t, err)
				require.Nil(t, job)
			}

			var taskCount int64
			require.NoError(t, app.DB.Model(&models.FollowUpTask{}).
				Where(
					"organization_id = ? AND source = ?",
					org.ID,
					automationTaskSource,
				).
				Count(&taskCount).Error)
			require.Equal(t, testCase.taskCount, taskCount)
		})
	}
}

func TestAutomationProcessor_Edge_FairnessBeyondTwentyFiveBusyTenants(t *testing.T) {
	db := testutil.SetupTestDB(t)
	app := &App{DB: db, Log: testutil.NopLogger()}

	const tenantCount = 30
	organizationIDs := make([]uuid.UUID, 0, tenantCount)
	for index := 0; index < tenantCount; index++ {
		org := testutil.CreateTestOrganization(t, db)
		organizationIDs = append(organizationIDs, org.ID)
		contact := testutil.CreateTestContact(t, db, org.ID)
		createAutomationActivityForTest(
			t,
			db,
			org.ID,
			contact.ID,
			models.CustomerActivityContactCreated,
		)
	}

	var pendingBefore int64
	require.NoError(t, db.Model(&models.AutomationEventReceipt{}).
		Where(
			"organization_id IN ? AND status = ?",
			organizationIDs,
			models.AutomationEventReceiptStatusPending,
		).
		Count(&pendingBefore).Error)
	require.EqualValues(t, tenantCount, pendingBefore)

	processor := NewAutomationPolicyProcessor(app, time.Second)
	processor.batchSize = 1000
	require.NoError(t, processor.RunOnce(context.Background()))

	var completedAfter int64
	require.NoError(t, db.Model(&models.AutomationEventReceipt{}).
		Where(
			"organization_id IN ? AND status = ?",
			organizationIDs,
			models.AutomationEventReceiptStatusCompleted,
		).
		Count(&completedAfter).Error)
	require.EqualValues(t, tenantCount, completedAfter)
}

func automationMarketingOptOutGraph(eventType string) AutomationGraph {
	return AutomationGraph{
		Nodes: []AutomationGraphNode{
			{
				ID:     "trigger",
				Type:   automationNodeTrigger,
				Config: map[string]any{"event_type": eventType},
			},
			{
				ID:   "preference",
				Type: automationNodeCondition,
				Config: map[string]any{
					"field":    "contact.marketing_opt_out",
					"operator": "equals",
					"value":    false,
				},
			},
			{
				ID:   "task",
				Type: automationNodeCreateTask,
				Config: map[string]any{
					"title":    "Call opted-in customer",
					"priority": "normal",
					"owner":    "unassigned",
				},
			},
		},
		Edges: []AutomationGraphEdge{
			{Source: "trigger", Target: "preference"},
			{Source: "preference", Target: "task", Branch: automationBranchTrue},
		},
	}
}

func requireAutomationEdgeWarningCode(
	t *testing.T,
	warnings []AutomationValidationIssue,
	code string,
) {
	t.Helper()
	for _, warning := range warnings {
		if warning.Code == code {
			return
		}
	}
	require.Failf(t, "missing validation warning", "expected warning code %q", code)
}
