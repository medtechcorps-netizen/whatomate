package handlers

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
	"github.com/zerodha/fastglue"
)

func TestAutomationPolicy_PreviewWarnsBeforeExactSemanticDuplicateActivation(t *testing.T) {
	app, org, user := newAutomationDatabaseTestApp(t)
	activeGraph := automationTestGraph(
		string(models.CustomerActivityContactCreated),
		"Welcome {{contact.name}}",
	)
	createActiveAutomationPolicyForTest(
		t,
		app.DB,
		org.ID,
		user.ID,
		"Existing behavior",
		activeGraph,
	)

	// IDs, order, and positions are canvas serialization details. This graph
	// deliberately differs in all three while preserving the active behavior.
	candidateGraph := AutomationGraph{
		Nodes: []AutomationGraphNode{
			{
				ID: "action-copy", Type: automationNodeCreateTask,
				Config: map[string]any{
					"title":    "Welcome {{contact.name}}",
					"priority": "normal",
					"owner":    "unassigned",
				},
				Position: &AutomationNodePosition{X: -900, Y: 700},
			},
			{
				ID: "trigger-copy", Type: automationNodeTrigger,
				Config: map[string]any{
					"event_type": string(models.CustomerActivityContactCreated),
				},
				Position: &AutomationNodePosition{X: 800, Y: -600},
			},
		},
		Edges: []AutomationGraphEdge{{Source: "trigger-copy", Target: "action-copy"}},
	}
	policy := createAutomationPolicyForTest(
		t,
		app,
		org.ID,
		user.ID,
		"Candidate behavior",
		candidateGraph,
	)

	previewRequest := automationRequest(
		t,
		org.ID,
		user.ID,
		policy.ID,
		PreviewAutomationPolicyRequest{
			Version: policy.Version,
			Graph:   &candidateGraph,
			Event: &AutomationPreviewEvent{
				EventType: string(models.CustomerActivityContactCreated),
			},
		},
	)
	require.NoError(t, app.PreviewAutomationPolicy(previewRequest))
	var preview AutomationPolicyPreviewResponse
	testutil.ParseEnvelopeResponse(t, previewRequest, &preview)
	require.True(t, preview.Valid)
	require.Contains(
		t,
		automationRuntimeValidationCodes(preview.Warnings),
		"shared_trigger_active_policy",
	)

	activateRequest := automationRequest(
		t,
		org.ID,
		user.ID,
		policy.ID,
		AutomationPolicyVersionRequest{Version: policy.Version},
	)
	require.NoError(t, app.ActivateAutomationPolicy(activateRequest))
	testutil.AssertErrorResponse(
		t,
		activateRequest,
		fasthttp.StatusConflict,
		"already has the same behavior",
	)
}

func TestAutomationPolicy_ExecutionsRequireRelatedReadsAndRedactRuntimeInternals(
	t *testing.T,
) {
	app, org, superAdmin := newAutomationDatabaseTestApp(t)
	contact := testutil.CreateTestContact(t, app.DB, org.ID)
	policy, _ := createActiveAutomationPolicyForTest(
		t,
		app.DB,
		org.ID,
		superAdmin.ID,
		"Execution visibility",
		automationTestGraph(
			string(models.CustomerActivityContactCreated),
			"Call {{contact.name}}",
		),
	)
	event := createAutomationActivityForTest(
		t,
		app.DB,
		org.ID,
		contact.ID,
		models.CustomerActivityContactCreated,
	)
	processor := NewAutomationPolicyProcessor(app, time.Second)
	processor.batchSize = 100
	require.NoError(t, processor.RunOnce(context.Background()))

	executionSecret := "execution-internal-secret-" + uuid.NewString()
	contextSecret := "execution-context-secret-" + uuid.NewString()
	resultSecret := "execution-result-secret-" + uuid.NewString()
	stepSecret := "step-output-secret-" + uuid.NewString()
	stepError := "step-internal-error-" + uuid.NewString()
	require.NoError(t, app.DB.Model(&models.AutomationExecution{}).
		Where(
			"organization_id = ? AND policy_id = ? AND activity_event_id = ?",
			org.ID,
			policy.ID,
			event.ID,
		).
		Updates(map[string]any{
			"last_error": executionSecret,
			"context":    models.JSONB{"secret": contextSecret},
			"result":     models.JSONB{"secret": resultSecret},
		}).Error)
	require.NoError(t, app.DB.Model(&models.AutomationExecutionStep{}).
		Where("organization_id = ? AND node_type = ?", org.ID, automationNodeCreateTask).
		Updates(map[string]any{
			"output":     models.JSONB{"secret": stepSecret},
			"last_error": stepError,
		}).Error)

	automationReadKey := models.ResourceCRMAutomations + ":" + models.ActionRead
	limitedRole := testutil.CreateTestRoleWithKeys(
		t,
		app.DB,
		org.ID,
		"automation-runtime-limited",
		[]string{automationReadKey},
	)
	limitedUser := testutil.CreateTestUser(
		t,
		app.DB,
		org.ID,
		testutil.WithRoleID(&limitedRole.ID),
	)
	denied := testutil.NewGETRequest(t)
	testutil.SetAuthContext(denied, org.ID, limitedUser.ID)
	testutil.SetPathParam(denied, "id", policy.ID.String())
	require.NoError(t, app.ListAutomationPolicyExecutions(denied))
	testutil.AssertErrorResponse(
		t,
		denied,
		fasthttp.StatusForbidden,
		"Contact and task read permissions are required",
	)

	relatedReadRole := testutil.CreateTestRoleWithKeys(
		t,
		app.DB,
		org.ID,
		"automation-runtime-reader",
		[]string{
			automationReadKey,
			models.ResourceContacts + ":" + models.ActionRead,
			models.ResourceTasks + ":" + models.ActionRead,
		},
	)
	relatedReadUser := testutil.CreateTestUser(
		t,
		app.DB,
		org.ID,
		testutil.WithRoleID(&relatedReadRole.ID),
	)
	allowed := testutil.NewGETRequest(t)
	testutil.SetAuthContext(allowed, org.ID, relatedReadUser.ID)
	testutil.SetPathParam(allowed, "id", policy.ID.String())
	require.NoError(t, app.ListAutomationPolicyExecutions(allowed))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(allowed))

	var response struct {
		Executions []map[string]any `json:"executions"`
	}
	testutil.ParseEnvelopeResponse(t, allowed, &response)
	require.Len(t, response.Executions, 1)
	execution := response.Executions[0]
	for _, forbidden := range []string{
		"organization_id",
		"last_error",
		"context",
		"result",
	} {
		require.NotContains(t, execution, forbidden)
	}
	steps, ok := execution["steps"].([]any)
	require.True(t, ok)
	require.NotEmpty(t, steps)
	for _, rawStep := range steps {
		step, ok := rawStep.(map[string]any)
		require.True(t, ok)
		for _, forbidden := range []string{
			"organization_id",
			"execution_id",
			"output",
			"last_error",
			"idempotency_key",
		} {
			require.NotContains(t, step, forbidden)
		}
	}
	body := string(testutil.GetResponseBody(allowed))
	for _, secret := range []string{
		executionSecret,
		contextSecret,
		resultSecret,
		stepSecret,
		stepError,
	} {
		require.NotContains(t, body, secret)
	}
}

func TestFollowUpTask_PublicWritesProtectAutomationLineage(t *testing.T) {
	app, org, user := newAutomationDatabaseTestApp(t)

	for _, source := range []string{automationTaskSource, careTaskSource} {
		t.Run("create rejects "+source, func(t *testing.T) {
			request := testutil.NewJSONRequest(t, CreateFollowUpTaskRequest{
				Title:  "Attempt reserved source",
				Source: source,
			})
			testutil.SetAuthContext(request, org.ID, user.ID)
			require.NoError(t, app.CreateFollowUpTask(request))
			testutil.AssertErrorResponse(
				t,
				request,
				fasthttp.StatusBadRequest,
				"source is reserved for server-created tasks",
			)
		})
	}

	lineage := models.JSONB{
		"automation_policy_id":     uuid.NewString(),
		"automation_execution_id":  uuid.NewString(),
		"automation_step_id":       uuid.NewString(),
		"source_activity_event_id": uuid.NewString(),
		"external_message_sent":    false,
		"user_note":                "preserve me",
	}
	task := models.FollowUpTask{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: org.ID,
		Title:          "Automation-created task",
		Status:         models.FollowUpTaskStatusOpen,
		Priority:       models.FollowUpTaskPriorityNormal,
		Source:         automationTaskSource,
		IdempotencyKey: "runtime-lineage-" + uuid.NewString(),
		Metadata:       lineage,
		Version:        1,
	}
	require.NoError(t, app.DB.Create(&task).Error)

	replacementSource := "manual"
	replaceSource := automationRuntimeTaskUpdateRequest(
		t,
		org.ID,
		user.ID,
		task.ID,
		UpdateFollowUpTaskRequest{Version: 1, Source: &replacementSource},
	)
	require.NoError(t, app.UpdateFollowUpTask(replaceSource))
	testutil.AssertErrorResponse(
		t,
		replaceSource,
		fasthttp.StatusConflict,
		"Server-authored task source cannot be changed",
	)

	replacementMetadata := models.JSONB{
		"automation_policy_id":    lineage["automation_policy_id"],
		"automation_execution_id": "forged-execution",
	}
	replaceLineage := automationRuntimeTaskUpdateRequest(
		t,
		org.ID,
		user.ID,
		task.ID,
		UpdateFollowUpTaskRequest{Version: 1, Metadata: &replacementMetadata},
	)
	require.NoError(t, app.UpdateFollowUpTask(replaceLineage))
	testutil.AssertErrorResponse(
		t,
		replaceLineage,
		fasthttp.StatusConflict,
		"Server-authored task lineage metadata cannot be changed",
	)

	updatedTitle := "Agent-confirmed follow-up"
	updatedPriority := models.FollowUpTaskPriorityHigh
	operationalEdit := automationRuntimeTaskUpdateRequest(
		t,
		org.ID,
		user.ID,
		task.ID,
		UpdateFollowUpTaskRequest{
			Version:  1,
			Title:    &updatedTitle,
			Priority: &updatedPriority,
		},
	)
	require.NoError(t, app.UpdateFollowUpTask(operationalEdit))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(operationalEdit))

	var stored models.FollowUpTask
	require.NoError(t, app.DB.Where(
		"id = ? AND organization_id = ?",
		task.ID,
		org.ID,
	).First(&stored).Error)
	require.Equal(t, automationTaskSource, stored.Source)
	require.Equal(t, updatedTitle, stored.Title)
	require.Equal(t, updatedPriority, stored.Priority)
	require.EqualValues(t, 2, stored.Version)
	for key, value := range lineage {
		require.Equal(t, value, stored.Metadata[key])
	}
}

func TestAutomationProcessor_ConcurrentWorkersCreateOneDurableTask(t *testing.T) {
	app, org, user := newAutomationDatabaseTestApp(t)
	contact := testutil.CreateTestContact(t, app.DB, org.ID)
	policy, _ := createActiveAutomationPolicyForTest(
		t,
		app.DB,
		org.ID,
		user.ID,
		"Concurrent workers",
		automationTestGraph(
			string(models.CustomerActivityContactCreated),
			"Concurrency follow-up",
		),
	)
	event := createAutomationActivityForTest(
		t,
		app.DB,
		org.ID,
		contact.ID,
		models.CustomerActivityContactCreated,
	)

	const workerCount = 4
	start := make(chan struct{})
	errs := make(chan error, workerCount)
	var workers sync.WaitGroup
	for range workerCount {
		processor := NewAutomationPolicyProcessor(app, time.Second)
		processor.batchSize = 100
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			errs <- processor.RunOnce(context.Background())
		}()
	}
	close(start)
	workers.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}

	var task models.FollowUpTask
	require.NoError(t, app.DB.Where(
		"organization_id = ? AND source = ?",
		org.ID,
		automationTaskSource,
	).First(&task).Error)
	require.NotNil(t, task.ContactID)
	require.Equal(t, contact.ID, *task.ContactID)

	var taskCount, executionCount, taskActivityCount int64
	require.NoError(t, app.DB.Model(&models.FollowUpTask{}).
		Where(
			"organization_id = ? AND source = ?",
			org.ID,
			automationTaskSource,
		).
		Count(&taskCount).Error)
	require.NoError(t, app.DB.Model(&models.AutomationExecution{}).
		Where(
			"organization_id = ? AND policy_id = ? AND activity_event_id = ?",
			org.ID,
			policy.ID,
			event.ID,
		).
		Count(&executionCount).Error)
	require.NoError(t, app.DB.Model(&models.CustomerActivityEvent{}).
		Where(
			"organization_id = ? AND event_type = ? AND source_object_id = ?",
			org.ID,
			models.CustomerActivityTaskCreated,
			task.ID,
		).
		Count(&taskActivityCount).Error)
	require.EqualValues(t, 1, taskCount)
	require.EqualValues(t, 1, executionCount)
	require.EqualValues(t, 1, taskActivityCount)

	// Simulate a previously failed delivery attempt being reclaimed. The
	// immutable server-authored provenance must still suppress every webhook.
	var outbox models.OutboxEvent
	require.NoError(t, app.DB.Where(
		"organization_id = ? AND aggregate_id = ? AND payload @> ?::jsonb",
		org.ID,
		task.ID,
		`{"automation_internal_only":true}`,
	).First(&outbox).Error)
	care := NewCareContinuityProcessor(app, time.Second)
	var deliveries atomic.Int64
	care.deliverWebhooks = func(context.Context, uuid.UUID, string, map[string]any) error {
		deliveries.Add(1)
		return errors.New("configured webhook must not be called")
	}
	lockedAt := time.Now().UTC()
	require.NoError(t, app.DB.Model(&models.OutboxEvent{}).
		Where("id = ? AND organization_id = ?", outbox.ID, org.ID).
		Updates(map[string]any{
			"status":     models.OutboxEventStatusProcessing,
			"attempts":   2,
			"locked_at":  lockedAt,
			"locked_by":  care.workerID,
			"last_error": "simulated prior delivery failure",
		}).Error)
	outbox.Status = models.OutboxEventStatusProcessing
	outbox.Attempts = 2
	outbox.LockedAt = &lockedAt
	outbox.LockedBy = care.workerID
	outbox.LastError = "simulated prior delivery failure"
	require.NoError(t, care.processOutboxEvent(context.Background(), org.ID, &outbox))
	require.Zero(t, deliveries.Load())
	require.NoError(t, app.DB.First(&outbox, "id = ?", outbox.ID).Error)
	require.Equal(t, models.OutboxEventStatusPublished, outbox.Status)
}

func TestAutomationProcessor_VisualPolicyWinsLegacyCareOwnershipRace(t *testing.T) {
	app, org, user := newAutomationDatabaseTestApp(t)
	entitlementUpdate := app.DB.Exec(
		`UPDATE subscriptions
		    SET entitlements_snapshot =
		        entitlements_snapshot || '{"bookings.enabled":true}'::jsonb
		  WHERE organization_id = ?`,
		org.ID,
	)
	require.NoError(t, entitlementUpdate.Error)
	require.Positive(t, entitlementUpdate.RowsAffected)
	contact := testutil.CreateTestContact(t, app.DB, org.ID)
	booking := createCareContinuityBooking(
		t,
		app.DB,
		org.ID,
		contact.ID,
		models.BookingStatusNoShow,
		time.Now().UTC().Add(-24*time.Hour),
	)
	policy, _ := createActiveAutomationPolicyForTest(
		t,
		app.DB,
		org.ID,
		user.ID,
		"Visual no-show ownership",
		automationTestGraph(
			string(models.CustomerActivityBookingStatus),
			"Visual no-show follow-up",
		),
	)
	activity, outbox := createCareContinuityActivity(
		t,
		app.DB,
		org.ID,
		contact.ID,
		models.CustomerActivityBookingStatus,
		models.CustomerActivityCategoryBooking,
		"booking",
		&booking.ID,
	)

	visual := NewAutomationPolicyProcessor(app, time.Second)
	visual.batchSize = 100
	care := NewCareContinuityProcessor(app, time.Second)
	care.batchSize = 100
	care.lastSweep = time.Now().UTC()
	care.sweepInterval = 24 * time.Hour
	care.deliverWebhooks = func(context.Context, uuid.UUID, string, map[string]any) error {
		return nil
	}

	start := make(chan struct{})
	results := make(chan error, 2)
	var processors sync.WaitGroup
	processors.Add(2)
	go func() {
		defer processors.Done()
		<-start
		results <- visual.RunOnce(context.Background())
	}()
	go func() {
		defer processors.Done()
		<-start
		results <- care.RunOnce(context.Background())
	}()
	close(start)
	processors.Wait()
	close(results)
	for err := range results {
		require.NoError(t, err)
	}
	// If care pinned the visual execution, its action job was created after the
	// first visual poll; a second bounded run deterministically drains it.
	require.NoError(t, visual.RunOnce(context.Background()))

	var visualTaskCount, careTaskCount, executionCount, taskActivityCount int64
	require.NoError(t, app.DB.Model(&models.FollowUpTask{}).
		Where(
			"organization_id = ? AND source = ?",
			org.ID,
			automationTaskSource,
		).
		Count(&visualTaskCount).Error)
	require.NoError(t, app.DB.Model(&models.FollowUpTask{}).
		Where(
			"organization_id = ? AND source = ?",
			org.ID,
			careTaskSource,
		).
		Count(&careTaskCount).Error)
	require.NoError(t, app.DB.Model(&models.AutomationExecution{}).
		Where(
			"organization_id = ? AND policy_id = ? AND activity_event_id = ?",
			org.ID,
			policy.ID,
			activity.ID,
		).
		Count(&executionCount).Error)
	require.NoError(t, app.DB.Model(&models.CustomerActivityEvent{}).
		Joins(`
			JOIN follow_up_tasks AS task
			  ON task.id = customer_activity_events.source_object_id
			 AND task.organization_id = customer_activity_events.organization_id
		`).
		Where(
			"customer_activity_events.organization_id = ? AND customer_activity_events.event_type = ? AND task.source = ?",
			org.ID,
			models.CustomerActivityTaskCreated,
			automationTaskSource,
		).
		Count(&taskActivityCount).Error)
	require.EqualValues(t, 1, visualTaskCount)
	require.Zero(t, careTaskCount)
	require.EqualValues(t, 1, executionCount)
	require.EqualValues(t, 1, taskActivityCount)

	require.NoError(t, app.DB.First(&outbox, "id = ?", outbox.ID).Error)
	require.Equal(t, models.OutboxEventStatusPublished, outbox.Status)
}

func automationRuntimeValidationCodes(
	issues []AutomationValidationIssue,
) []string {
	codes := make([]string, 0, len(issues))
	for _, issue := range issues {
		codes = append(codes, issue.Code)
	}
	return codes
}

func automationRuntimeTaskUpdateRequest(
	t *testing.T,
	orgID, userID, taskID uuid.UUID,
	body UpdateFollowUpTaskRequest,
) *fastglue.Request {
	t.Helper()
	request := testutil.NewJSONRequest(t, body)
	testutil.SetAuthContext(request, orgID, userID)
	testutil.SetPathParam(request, "id", taskID.String())
	return request
}
