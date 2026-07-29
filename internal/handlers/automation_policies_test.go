package handlers

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
	"github.com/zerodha/fastglue"
	"gorm.io/gorm"
)

func TestAutomationPolicy_SystemRoleContract(t *testing.T) {
	manager := automationStringSet(models.SystemRolePermissions()["manager"])
	agent := automationStringSet(models.SystemRolePermissions()["agent"])
	for _, action := range []string{
		models.ActionRead,
		models.ActionWrite,
		models.ActionDelete,
		models.ActionExecute,
	} {
		require.True(t, manager[models.ResourceCRMAutomations+":"+action])
	}
	require.True(t, agent[models.ResourceCRMAutomations+":"+models.ActionRead])
	require.False(t, agent[models.ResourceCRMAutomations+":"+models.ActionWrite])
	require.False(t, agent[models.ResourceCRMAutomations+":"+models.ActionDelete])
	require.False(t, agent[models.ResourceCRMAutomations+":"+models.ActionExecute])
}

func TestAutomationGraph_SemanticFingerprintIgnoresCanvasSerialization(t *testing.T) {
	left := automationTestConditionalGraph("booking.status_changed", "Follow up")
	right := AutomationGraph{
		Nodes: []AutomationGraphNode{
			{
				ID: "action-copy", Type: automationNodeCreateTask,
				Config: map[string]any{
					"title": "Follow up", "description": "",
					"priority": "normal", "owner": "unassigned",
				},
				Position: &AutomationNodePosition{X: 900, Y: -300},
			},
			{
				ID: "trigger-copy", Type: automationNodeTrigger,
				Config:   map[string]any{"event_types": []string{"booking.status_changed"}},
				Position: &AutomationNodePosition{X: -800, Y: 700},
			},
			{
				ID: "condition-copy", Type: automationNodeCondition,
				Config: map[string]any{
					"field": "event.metadata.to_status", "operator": "equals",
					"value": "completed",
				},
				Position: &AutomationNodePosition{X: 4, Y: 5},
			},
		},
		Edges: []AutomationGraphEdge{
			{Source: "condition-copy", Target: "action-copy", Branch: automationBranchTrue},
			{Source: "trigger-copy", Target: "condition-copy"},
		},
	}
	require.Empty(t, validateAutomationGraph(left, true).Errors)
	require.Empty(t, validateAutomationGraph(right, true).Errors)
	leftFingerprint, err := automationGraphSemanticFingerprint(left)
	require.NoError(t, err)
	rightFingerprint, err := automationGraphSemanticFingerprint(right)
	require.NoError(t, err)
	require.Equal(t, leftFingerprint, rightFingerprint)

	right.Nodes[0].Config["title"] = "Different action"
	changedFingerprint, err := automationGraphSemanticFingerprint(right)
	require.NoError(t, err)
	require.NotEqual(t, leftFingerprint, changedFingerprint)
}

func TestAutomationGraph_StrictActivationContract(t *testing.T) {
	for _, key := range []string{"send_message", "webhook", "script"} {
		graph := automationTestGraph(string(models.CustomerActivityContactCreated), "Task")
		graph.Nodes[1].Config[key] = "forbidden"
		validation := validateAutomationGraph(graph, true)
		requireAutomationValidationCode(t, validation, "unknown_node_config")
	}

	booleanGraph := automationTestConditionalGraph(
		string(models.CustomerActivityContactUpdated), "Opt-out review",
	)
	booleanGraph.Nodes[1].Config = map[string]any{
		"field": "contact.marketing_opt_out", "operator": "equals", "value": true,
	}
	require.Empty(t, validateAutomationGraph(booleanGraph, true).Errors)
	booleanGraph.Nodes[1].Config["value"] = "true"
	requireAutomationValidationCode(
		t, validateAutomationGraph(booleanGraph, true), "invalid_condition_value",
	)
	booleanGraph.Nodes[1].Config["operator"] = "in"
	booleanGraph.Nodes[1].Config["value"] = []any{true}
	requireAutomationValidationCode(
		t, validateAutomationGraph(booleanGraph, true), "invalid_condition_operator",
	)

	delayGraph := AutomationGraph{
		Nodes: []AutomationGraphNode{
			{ID: "trigger", Type: automationNodeTrigger, Config: map[string]any{
				"event_type": string(models.CustomerActivityContactCreated),
			}},
			{ID: "delay-a", Type: automationNodeDelay, Config: map[string]any{"minutes": 30000}},
			{ID: "delay-b", Type: automationNodeDelay, Config: map[string]any{"minutes": 20000}},
			{ID: "task", Type: automationNodeCreateTask, Config: map[string]any{"title": "Task"}},
		},
		Edges: []AutomationGraphEdge{
			{Source: "trigger", Target: "delay-a"},
			{Source: "delay-a", Target: "delay-b"},
			{Source: "delay-b", Target: "task"},
		},
	}
	requireAutomationValidationCode(
		t, validateAutomationGraph(delayGraph, true), "cumulative_delay_exceeded",
	)

	huge := automationTestGraph(string(models.CustomerActivityContactCreated), "Task")
	huge.Nodes[0].Position = &AutomationNodePosition{X: 1e300, Y: 0}
	requireAutomationValidationCode(t, validateAutomationGraph(huge, true), "invalid_position")
}

func TestAutomationGraph_TemplateRenderingIsDeterministicAndBounded(t *testing.T) {
	event := &models.CustomerActivityEvent{
		EventType: models.CustomerActivityContactCreated,
		Title:     "{{contact.name}}",
		Summary:   "",
	}
	contact := &models.Contact{ProfileName: "{{event.title}}"}
	rendered := automationRenderTemplate(
		"{{contact.name}} / {{event.title}} / {{event.summary}}",
		contact,
		event,
	)
	require.Equal(
		t,
		"{{event.title}} / {{contact.name}} / {{contact.name}}",
		rendered,
	)
	require.Len(t, []rune(automationTruncateRunes(
		"客户"+string(make([]rune, 300)),
		255,
	)), 255)

	emptyContact := &models.Contact{}
	require.Equal(
		t,
		"Customer",
		automationRenderTemplate("{{contact.name}}", emptyContact, event),
	)
}

func TestAutomationPolicy_CRUDPreviewActivationAndImmutableIntervals(t *testing.T) {
	app, org, user := newAutomationDatabaseTestApp(t)
	graph := automationTestGraph(string(models.CustomerActivityContactCreated), "Welcome {{contact.name}}")

	policy := createAutomationPolicyForTest(t, app, org.ID, user.ID, "New customer", graph)
	require.Equal(t, models.AutomationPolicyStatusDraft, policy.Status)
	require.EqualValues(t, 1, policy.Version)

	var beforeExecutions, beforeJobs, beforeTasks int64
	require.NoError(t, app.DB.Model(&models.AutomationExecution{}).Count(&beforeExecutions).Error)
	require.NoError(t, app.DB.Model(&models.ScheduledJob{}).Count(&beforeJobs).Error)
	require.NoError(t, app.DB.Model(&models.FollowUpTask{}).Count(&beforeTasks).Error)

	missingVersion := automationRequest(t, org.ID, user.ID, policy.ID, PreviewAutomationPolicyRequest{
		Graph: &graph,
		Event: &AutomationPreviewEvent{EventType: string(models.CustomerActivityContactCreated)},
	})
	require.NoError(t, app.PreviewAutomationPolicy(missingVersion))
	testutil.AssertErrorResponse(t, missingVersion, fasthttp.StatusBadRequest, "version must be at least 1")

	storedID := uuid.New()
	storedPreview := automationRequest(t, org.ID, user.ID, policy.ID, PreviewAutomationPolicyRequest{
		Version: 1, ActivityEventID: &storedID,
	})
	require.NoError(t, app.PreviewAutomationPolicy(storedPreview))
	testutil.AssertErrorResponse(t, storedPreview, fasthttp.StatusBadRequest, "synthetic event data only")

	previewRequest := automationRequest(t, org.ID, user.ID, policy.ID, PreviewAutomationPolicyRequest{
		Version: 1,
		Graph:   &graph,
		Event: &AutomationPreviewEvent{
			EventType: string(models.CustomerActivityContactCreated),
			Title:     "New customer",
			Contact:   &AutomationPreviewContact{MarketingOptOut: false},
		},
	})
	require.NoError(t, app.PreviewAutomationPolicy(previewRequest))
	var preview AutomationPolicyPreviewResponse
	testutil.ParseEnvelopeResponse(t, previewRequest, &preview)
	require.True(t, preview.Valid)
	require.Len(t, preview.Actions, 1)
	require.Contains(t, preview.Actions[0].Title, "Customer")

	var afterExecutions, afterJobs, afterTasks int64
	require.NoError(t, app.DB.Model(&models.AutomationExecution{}).Count(&afterExecutions).Error)
	require.NoError(t, app.DB.Model(&models.ScheduledJob{}).Count(&afterJobs).Error)
	require.NoError(t, app.DB.Model(&models.FollowUpTask{}).Count(&afterTasks).Error)
	require.Equal(t, beforeExecutions, afterExecutions)
	require.Equal(t, beforeJobs, afterJobs)
	require.Equal(t, beforeTasks, afterTasks)

	activateRequest := automationRequest(
		t, org.ID, user.ID, policy.ID,
		AutomationPolicyVersionRequest{Version: policy.Version},
	)
	require.NoError(t, app.ActivateAutomationPolicy(activateRequest))
	var active AutomationPolicyResponse
	testutil.ParseEnvelopeResponse(t, activateRequest, &active)
	require.Equal(t, models.AutomationPolicyStatusActive, active.Status)
	require.NotNil(t, active.ActiveVersionID)

	deleteActive := automationRequest(
		t, org.ID, user.ID, policy.ID,
		AutomationPolicyVersionRequest{Version: active.Version},
	)
	require.NoError(t, app.DeleteAutomationPolicy(deleteActive))
	testutil.AssertErrorResponse(t, deleteActive, fasthttp.StatusConflict, "Only unpublished draft")

	pauseRequest := automationRequest(
		t, org.ID, user.ID, policy.ID,
		AutomationPolicyVersionRequest{Version: active.Version},
	)
	require.NoError(t, app.PauseAutomationPolicy(pauseRequest))
	var paused AutomationPolicyResponse
	testutil.ParseEnvelopeResponse(t, pauseRequest, &paused)
	require.Equal(t, models.AutomationPolicyStatusPaused, paused.Status)

	var activation models.AutomationPolicyActivation
	require.NoError(t, app.DB.Where(
		"organization_id = ? AND policy_id = ?", org.ID, policy.ID,
	).First(&activation).Error)
	require.NotNil(t, activation.ActiveUntil)
	require.Error(t, app.DB.Model(&models.AutomationPolicyActivation{}).
		Where("id = ?", activation.ID).
		Update("active_from", activation.ActiveFrom.Add(-time.Hour)).Error)
	require.Error(t, app.DB.Model(&models.AutomationPolicyActivation{}).
		Where("id = ?", activation.ID).
		Updates(map[string]any{"active_until": nil, "closed_by_id": nil}).Error)
	require.Error(t, app.DB.Delete(&models.AutomationPolicyActivation{}, "id = ?", activation.ID).Error)
}

func TestAutomationPolicy_CrossTenantAndEntitlementIsolation(t *testing.T) {
	db := testutil.SetupTestDB(t)
	app := &App{DB: db, Log: testutil.NopLogger()}
	orgA := testutil.CreateTestOrganization(t, db)
	orgB := testutil.CreateTestOrganization(t, db)
	userA := testutil.CreateTestUser(t, db, orgA.ID, testutil.WithSuperAdmin())
	userB := testutil.CreateTestUser(t, db, orgB.ID, testutil.WithSuperAdmin())
	enableBookingCommerceTestEntitlement(t, db, orgA.ID, userA.ID, "crm.enabled")
	enableBookingCommerceTestEntitlement(t, db, orgB.ID, userB.ID, "crm.enabled")
	policyB := createAutomationPolicyForTest(
		t, app, orgB.ID, userB.ID, "Tenant B", automationTestGraph(
			string(models.CustomerActivityContactCreated), "B",
		),
	)

	get := testutil.NewGETRequest(t)
	testutil.SetAuthContext(get, orgA.ID, userA.ID)
	testutil.SetPathParam(get, "id", policyB.ID.String())
	require.NoError(t, app.GetAutomationPolicy(get))
	testutil.AssertErrorResponse(t, get, fasthttp.StatusNotFound, "not found")

	orgNoPlan := testutil.CreateTestOrganization(t, db)
	userNoPlan := testutil.CreateTestUser(t, db, orgNoPlan.ID, testutil.WithSuperAdmin())
	list := testutil.NewGETRequest(t)
	testutil.SetAuthContext(list, orgNoPlan.ID, userNoPlan.ID)
	require.NoError(t, app.ListAutomationPolicies(list))
	require.Equal(t, fasthttp.StatusPaymentRequired, testutil.GetResponseStatusCode(list))
}

func TestAutomationProcessor_ReceiptToTaskIsIdempotentAndWebhookSilent(t *testing.T) {
	app, org, user := newAutomationDatabaseTestApp(t)
	contact := testutil.CreateTestContact(t, app.DB, org.ID)
	graph := automationTestGraph(string(models.CustomerActivityContactCreated), "Call {{contact.name}}")
	createActiveAutomationPolicyForTest(t, app.DB, org.ID, user.ID, "Receipt policy", graph)
	event := createAutomationActivityForTest(
		t, app.DB, org.ID, contact.ID, models.CustomerActivityContactCreated,
	)

	var receipt models.AutomationEventReceipt
	require.NoError(t, app.DB.Where(
		"organization_id = ? AND activity_event_id = ?", org.ID, event.ID,
	).First(&receipt).Error)
	require.Equal(t, models.AutomationEventReceiptStatusPending, receipt.Status)

	processor := NewAutomationPolicyProcessor(app, time.Second)
	processor.batchSize = 500
	require.NoError(t, processor.RunOnce(context.Background()))
	require.NoError(t, processor.RunOnce(context.Background()))

	var task models.FollowUpTask
	require.NoError(t, app.DB.Where(
		"organization_id = ? AND source = ?", org.ID, automationTaskSource,
	).First(&task).Error)
	require.Equal(t, contact.ID, *task.ContactID)
	require.Equal(t, false, task.Metadata["external_message_sent"])
	var taskCount, executionCount int64
	require.NoError(t, app.DB.Model(&models.FollowUpTask{}).
		Where("organization_id = ? AND source = ?", org.ID, automationTaskSource).
		Count(&taskCount).Error)
	require.NoError(t, app.DB.Model(&models.AutomationExecution{}).
		Where("organization_id = ? AND activity_event_id = ?", org.ID, event.ID).
		Count(&executionCount).Error)
	require.EqualValues(t, 1, taskCount)
	require.EqualValues(t, 1, executionCount)

	var outbox models.OutboxEvent
	require.NoError(t, app.DB.Where(
		"organization_id = ? AND aggregate_id = ? AND payload @> ?::jsonb",
		org.ID, task.ID, `{"automation_internal_only":true}`,
	).First(&outbox).Error)
	// User-facing task fields may change before durable outbox delivery; the
	// server-authored top-level outbox provenance remains authoritative.
	require.NoError(t, app.DB.Model(&models.FollowUpTask{}).
		Where("id = ? AND organization_id = ?", task.ID, org.ID).
		Updates(map[string]any{"source": "edited", "metadata": models.JSONB{}}).Error)

	care := NewCareContinuityProcessor(app, time.Second)
	var deliveries atomic.Int64
	care.deliverWebhooks = func(context.Context, uuid.UUID, string, map[string]any) error {
		deliveries.Add(1)
		return nil
	}
	now := time.Now().UTC()
	require.NoError(t, app.DB.Model(&models.OutboxEvent{}).
		Where("id = ? AND organization_id = ?", outbox.ID, org.ID).
		Updates(map[string]any{
			"status":    models.OutboxEventStatusProcessing,
			"locked_by": care.workerID, "locked_at": now, "attempts": 1,
		}).Error)
	outbox.Status = models.OutboxEventStatusProcessing
	outbox.LockedBy = care.workerID
	outbox.LockedAt = &now
	outbox.Attempts = 1
	require.NoError(t, care.processOutboxEvent(context.Background(), org.ID, &outbox))
	require.Zero(t, deliveries.Load())
	require.NoError(t, app.DB.First(&outbox, "id = ?", outbox.ID).Error)
	require.Equal(t, models.OutboxEventStatusPublished, outbox.Status)
}

func TestAutomationProcessor_ReapsExhaustedActionAndTerminalizesRun(t *testing.T) {
	app, org, user := newAutomationDatabaseTestApp(t)
	contact := testutil.CreateTestContact(t, app.DB, org.ID)
	graph := AutomationGraph{
		Nodes: []AutomationGraphNode{
			{ID: "trigger", Type: automationNodeTrigger, Config: map[string]any{
				"event_type": string(models.CustomerActivityContactCreated),
			}},
			{ID: "delay", Type: automationNodeDelay, Config: map[string]any{"minutes": 60}},
			{ID: "task", Type: automationNodeCreateTask, Config: map[string]any{
				"title": "Delayed", "priority": "normal", "owner": "unassigned",
			}},
		},
		Edges: []AutomationGraphEdge{
			{Source: "trigger", Target: "delay"},
			{Source: "delay", Target: "task"},
		},
	}
	createActiveAutomationPolicyForTest(t, app.DB, org.ID, user.ID, "Delayed", graph)
	event := createAutomationActivityForTest(
		t, app.DB, org.ID, contact.ID, models.CustomerActivityContactCreated,
	)
	processor := NewAutomationPolicyProcessor(app, time.Second)
	receipt, err := processor.claimReceipt(context.Background(), org.ID)
	require.NoError(t, err)
	require.NotNil(t, receipt)
	require.NoError(t, processor.processReceipt(context.Background(), org.ID, receipt))

	var job models.ScheduledJob
	require.NoError(t, app.DB.Where(
		"organization_id = ? AND kind = ?", org.ID, automationCreateTaskJobKind,
	).First(&job).Error)
	stale := time.Now().UTC().Add(-processor.lease - time.Minute)
	require.NoError(t, app.DB.Model(&models.ScheduledJob{}).
		Where("id = ?", job.ID).
		Updates(map[string]any{
			"status":   models.ScheduledJobStatusProcessing,
			"attempts": job.MaxAttempts, "locked_at": stale,
			"locked_by": "crashed-worker", "run_at": stale,
		}).Error)
	claimed, err := processor.claimActionJob(context.Background(), org.ID)
	require.NoError(t, err)
	require.Nil(t, claimed)

	var execution models.AutomationExecution
	require.NoError(t, app.DB.Where(
		"organization_id = ? AND activity_event_id = ?", org.ID, event.ID,
	).First(&execution).Error)
	require.Equal(t, models.AutomationExecutionStatusFailed, execution.Status)
	var step models.AutomationExecutionStep
	require.NoError(t, app.DB.Where(
		"organization_id = ? AND execution_id = ? AND node_type = ?",
		org.ID, execution.ID, automationNodeCreateTask,
	).First(&step).Error)
	require.Equal(t, models.AutomationExecutionStepStatusFailed, step.Status)
}

func newAutomationDatabaseTestApp(
	t *testing.T,
) (*App, *models.Organization, *models.User) {
	t.Helper()
	db := testutil.SetupTestDB(t)
	app := &App{DB: db, Log: testutil.NopLogger()}
	org := testutil.CreateTestOrganization(t, db)
	user := testutil.CreateTestUser(t, db, org.ID, testutil.WithSuperAdmin())
	enableBookingCommerceTestEntitlement(t, db, org.ID, user.ID, "crm.enabled")
	return app, org, user
}

func automationTestGraph(eventType, title string) AutomationGraph {
	return AutomationGraph{
		Nodes: []AutomationGraphNode{
			{
				ID: "trigger", Type: automationNodeTrigger,
				Config:   map[string]any{"event_type": eventType},
				Position: &AutomationNodePosition{X: 0, Y: 0},
			},
			{
				ID: "task", Type: automationNodeCreateTask,
				Config: map[string]any{
					"title": title, "priority": "normal", "owner": "unassigned",
				},
				Position: &AutomationNodePosition{X: 300, Y: 0},
			},
		},
		Edges: []AutomationGraphEdge{{Source: "trigger", Target: "task"}},
	}
}

func automationTestConditionalGraph(eventType, title string) AutomationGraph {
	return AutomationGraph{
		Nodes: []AutomationGraphNode{
			{
				ID: "trigger", Type: automationNodeTrigger,
				Config:   map[string]any{"event_type": eventType},
				Position: &AutomationNodePosition{X: 0, Y: 0},
			},
			{
				ID: "condition", Type: automationNodeCondition,
				Config: map[string]any{
					"field":    "event.metadata.to_status",
					"operator": "equals", "value": "completed",
				},
				Position: &AutomationNodePosition{X: 200, Y: 0},
			},
			{
				ID: "task", Type: automationNodeCreateTask,
				Config:   map[string]any{"title": title},
				Position: &AutomationNodePosition{X: 400, Y: 0},
			},
		},
		Edges: []AutomationGraphEdge{
			{Source: "trigger", Target: "condition"},
			{Source: "condition", Target: "task", Branch: automationBranchTrue},
		},
	}
}

func createAutomationPolicyForTest(
	t *testing.T,
	app *App,
	orgID, userID uuid.UUID,
	name string,
	graph AutomationGraph,
) AutomationPolicyResponse {
	t.Helper()
	request := testutil.NewJSONRequest(t, CreateAutomationPolicyRequest{
		Name:  name + " " + uuid.NewString()[:6],
		Graph: graph,
	})
	testutil.SetAuthContext(request, orgID, userID)
	require.NoError(t, app.CreateAutomationPolicy(request))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(request))
	var policy AutomationPolicyResponse
	testutil.ParseEnvelopeResponse(t, request, &policy)
	return policy
}

func automationRequest(
	t *testing.T,
	orgID, userID, policyID uuid.UUID,
	body any,
) *fastglue.Request {
	t.Helper()
	request := testutil.NewJSONRequest(t, body)
	testutil.SetAuthContext(request, orgID, userID)
	testutil.SetPathParam(request, "id", policyID.String())
	return request
}

func createActiveAutomationPolicyForTest(
	t *testing.T,
	db *gorm.DB,
	orgID, userID uuid.UUID,
	name string,
	graph AutomationGraph,
) (*models.AutomationPolicy, *models.AutomationPolicyVersion) {
	t.Helper()
	require.Empty(t, validateAutomationGraph(graph, true).Errors)
	graphValue, err := automationGraphToJSONB(graph)
	require.NoError(t, err)
	checksum, err := automationGraphChecksum(graph)
	require.NoError(t, err)
	validation := validateAutomationGraph(graph, true)
	policy := &models.AutomationPolicy{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: orgID, Name: name + " " + uuid.NewString()[:6],
		Status: models.AutomationPolicyStatusDraft, DraftGraph: graphValue,
		TriggerEventTypes: automationJSONBArrayStrings(validation.TriggerEventTypes),
		Version:           1, CreatedByID: &userID, UpdatedByID: &userID,
	}
	require.NoError(t, db.Create(policy).Error)
	now := time.Now().UTC()
	version := &models.AutomationPolicyVersion{
		ID: uuid.New(), OrganizationID: orgID, PolicyID: policy.ID, Number: 1,
		TriggerEventTypes: automationJSONBArrayStrings(validation.TriggerEventTypes),
		Graph:             graphValue, Checksum: checksum, CreatedByID: &userID,
		PublishedAt: now,
	}
	require.NoError(t, db.Create(version).Error)
	activation := models.AutomationPolicyActivation{
		ID: uuid.New(), OrganizationID: orgID, PolicyID: policy.ID,
		PolicyVersionID: version.ID, PolicyVersionNumber: version.Number,
		ActiveFrom: now.Add(-time.Second), CreatedByID: &userID,
	}
	require.NoError(t, db.Create(&activation).Error)
	require.NoError(t, db.Model(&models.AutomationPolicy{}).
		Where("id = ? AND organization_id = ?", policy.ID, orgID).
		Updates(map[string]any{
			"status":                models.AutomationPolicyStatusActive,
			"active_version_id":     version.ID,
			"active_version_number": version.Number,
			"activated_at":          now,
		}).Error)
	return policy, version
}

func createAutomationActivityForTest(
	t *testing.T,
	db *gorm.DB,
	orgID, contactID uuid.UUID,
	eventType models.CustomerActivityEventType,
) *models.CustomerActivityEvent {
	t.Helper()
	event := &models.CustomerActivityEvent{
		ID: uuid.New(), OrganizationID: orgID, ContactID: contactID,
		EventType: eventType, Category: models.CustomerActivityCategoryContact,
		Title: "Automation test event", ActorType: models.CustomerActivityActorSystem,
		SourceObjectType: "contact", SourceObjectID: &contactID,
		OccurredAt: time.Now().UTC(), Metadata: models.JSONB{},
		IdempotencyKey: fmt.Sprintf("automation-test:%s", uuid.NewString()),
	}
	require.NoError(t, db.Create(event).Error)
	return event
}
