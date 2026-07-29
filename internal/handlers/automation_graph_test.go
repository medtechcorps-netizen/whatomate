package handlers

import (
	"encoding/json"
	"math"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func validAutomationGraph() AutomationGraph {
	return AutomationGraph{
		Nodes: []AutomationGraphNode{
			{
				ID:   "trigger",
				Type: automationNodeTrigger,
				Config: map[string]any{
					"event_type": string(models.CustomerActivityBookingStatus),
				},
				Position: &AutomationNodePosition{X: 80, Y: 160},
			},
			{
				ID:   "condition",
				Type: automationNodeCondition,
				Config: map[string]any{
					"field":    "event.metadata.to_status",
					"operator": "equals",
					"value":    "completed",
				},
				Position: &AutomationNodePosition{X: 380, Y: 160},
			},
			{
				ID:   "delay",
				Type: automationNodeDelay,
				Config: map[string]any{
					"minutes": 60,
				},
				Position: &AutomationNodePosition{X: 680, Y: 100},
			},
			{
				ID:   "task",
				Type: automationNodeCreateTask,
				Config: map[string]any{
					"title":             "Check In After Visit",
					"description":       "Review {{event.title}} with {{contact.name}}.",
					"priority":          string(models.FollowUpTaskPriorityHigh),
					"owner":             "contact_owner",
					"due_in_minutes":    120,
					"remind_in_minutes": 30,
				},
				Position: &AutomationNodePosition{X: 980, Y: 100},
			},
		},
		Edges: []AutomationGraphEdge{
			{Source: "trigger", Target: "condition", Branch: automationBranchAlways},
			{Source: "condition", Target: "delay", Branch: automationBranchTrue},
			{Source: "delay", Target: "task", Branch: automationBranchAlways},
		},
	}
}

func automationValidationCodes(validation automationGraphValidation) []string {
	codes := make([]string, 0, len(validation.Errors))
	for _, issue := range validation.Errors {
		codes = append(codes, issue.Code)
	}
	return codes
}

func requireAutomationValidationCode(
	t *testing.T,
	validation automationGraphValidation,
	code string,
) {
	t.Helper()
	assert.Contains(t, automationValidationCodes(validation), code, "issues: %#v", validation.Errors)
}

func requireAutomationGraphValid(t *testing.T, graph AutomationGraph) automationGraphValidation {
	t.Helper()
	validation := validateAutomationGraph(graph, true)
	require.Empty(t, validation.Errors, "unexpected validation errors: %#v", validation.Errors)
	return validation
}

func automationTestEvent(eventType models.CustomerActivityEventType) *models.CustomerActivityEvent {
	return &models.CustomerActivityEvent{
		ID:               uuid.MustParse("11111111-1111-4111-8111-111111111111"),
		OrganizationID:   uuid.MustParse("22222222-2222-4222-8222-222222222222"),
		ContactID:        uuid.MustParse("33333333-3333-4333-8333-333333333333"),
		EventType:        eventType,
		Category:         models.CustomerActivityCategoryBooking,
		Title:            "Annual Review",
		Summary:          "Customer completed their annual review.",
		ActorType:        models.CustomerActivityActorUser,
		SourceObjectType: "booking",
		OccurredAt:       time.Date(2026, time.July, 30, 2, 30, 0, 0, time.UTC),
		Metadata: models.JSONB{
			"from_status": "confirmed",
			"to_status":   "completed",
			"labels":      []any{"care", "priority"},
		},
		IdempotencyKey: "automation-graph-test-event",
	}
}

func automationTestContact() *models.Contact {
	return &models.Contact{
		BaseModel: models.BaseModel{
			ID: uuid.MustParse("33333333-3333-4333-8333-333333333333"),
		},
		OrganizationID: uuid.MustParse("22222222-2222-4222-8222-222222222222"),
		ProfileName:    "Dr Arham",
	}
}

// automationReasonCodes intentionally accepts either an explicit ReasonCode(s)
// field or a reason_code in step output. This keeps the test focused on the
// stable API contract rather than prescribing where the pure evaluator stores
// its machine-readable explanation.
func automationReasonCodes(value any) []string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	var decoded any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		return nil
	}
	var codes []string
	var walk func(any)
	walk = func(current any) {
		switch typed := current.(type) {
		case map[string]any:
			for key, child := range typed {
				normalized := strings.ToLower(strings.ReplaceAll(key, "_", ""))
				switch normalized {
				case "reasoncode", "stopreason":
					if code, ok := child.(string); ok && code != "" {
						codes = append(codes, code)
						continue
					}
				case "reasoncodes":
					if values, ok := child.([]any); ok {
						for _, item := range values {
							if code, ok := item.(string); ok && code != "" {
								codes = append(codes, code)
							}
						}
						continue
					}
				}
				walk(child)
			}
		case []any:
			for _, child := range typed {
				walk(child)
			}
		}
	}
	walk(decoded)
	sort.Strings(codes)
	return codes
}

func TestAutomationGraph_ValidProductionGraph(t *testing.T) {
	validation := requireAutomationGraphValid(t, validAutomationGraph())
	assert.Equal(t, []string{string(models.CustomerActivityBookingStatus)}, validation.TriggerEventTypes)
	assert.Empty(t, validation.OwnerUserIDs)
}

func TestAutomationGraph_JSONBRoundTripAndChecksum(t *testing.T) {
	graph := validAutomationGraph()

	value, err := automationGraphToJSONB(graph)
	require.NoError(t, err)
	roundTripped, err := automationGraphFromJSONB(value)
	require.NoError(t, err)
	graphJSON, err := json.Marshal(graph)
	require.NoError(t, err)
	roundTrippedJSON, err := json.Marshal(roundTripped)
	require.NoError(t, err)
	assert.JSONEq(t, string(graphJSON), string(roundTrippedJSON))

	first, err := automationGraphChecksum(graph)
	require.NoError(t, err)
	second, err := automationGraphChecksum(roundTripped)
	require.NoError(t, err)
	assert.Equal(t, first, second)
	assert.Len(t, first, 64)

	roundTripped.Nodes[3].Config["title"] = "A Different Task"
	changed, err := automationGraphChecksum(roundTripped)
	require.NoError(t, err)
	assert.NotEqual(t, first, changed)
}

func TestAutomationGraph_EncodedSizeIsBounded(t *testing.T) {
	graph := validAutomationGraph()
	graph.Nodes[3].Config["description"] = strings.Repeat("x", automationMaxGraphBytes)

	_, err := automationGraphToJSONB(graph)
	require.ErrorContains(t, err, "maximum encoded size")

	_, err = automationGraphFromJSONB(models.JSONB{
		"nodes": strings.Repeat("x", automationMaxGraphBytes),
	})
	require.ErrorContains(t, err, "maximum encoded size")
}

func TestAutomationGraph_TriggerAllowlist(t *testing.T) {
	allowed := []models.CustomerActivityEventType{
		models.CustomerActivityContactCreated,
		models.CustomerActivityContactUpdated,
		models.CustomerActivityContactMerged,
		models.CustomerActivityMessageIncoming,
		models.CustomerActivityCRMLeadCreated,
		models.CustomerActivityCRMLeadUpdated,
		models.CustomerActivityCRMStageMoved,
		models.CustomerActivityTaskCompleted,
		models.CustomerActivityBookingCreated,
		models.CustomerActivityBookingStatus,
		models.CustomerActivityPackageSold,
		models.CustomerActivityPackageLow,
		models.CustomerActivityPackageExpiring,
		models.CustomerActivityInvoiceCreated,
		models.CustomerActivityInvoiceOverdue,
		models.CustomerActivityInvoicePaid,
		models.CustomerActivityPaymentRecorded,
		models.CustomerActivityConsentOptedOut,
	}
	for _, eventType := range allowed {
		t.Run(string(eventType), func(t *testing.T) {
			graph := validAutomationGraph()
			graph.Nodes[0].Config = map[string]any{"event_type": string(eventType)}
			requireAutomationGraphValid(t, graph)
		})
	}
}

func TestAutomationGraph_TriggerEventTypesAreBoundedAndCanonical(t *testing.T) {
	t.Run("bounded list is sorted", func(t *testing.T) {
		graph := validAutomationGraph()
		graph.Nodes[0].Config = map[string]any{
			"event_types": []any{
				string(models.CustomerActivityPackageExpiring),
				string(models.CustomerActivityPackageLow),
			},
		}
		validation := requireAutomationGraphValid(t, graph)
		assert.Equal(t, []string{
			string(models.CustomerActivityPackageLow),
			string(models.CustomerActivityPackageExpiring),
		}, validation.TriggerEventTypes)
	})

	tests := []struct {
		name string
		cfg  map[string]any
		code string
	}{
		{
			name: "task created recursion",
			cfg:  map[string]any{"event_type": string(models.CustomerActivityTaskCreated)},
			code: "recursive_trigger_forbidden",
		},
		{
			name: "unknown event",
			cfg:  map[string]any{"event_type": "made.up"},
			code: "unsupported_trigger_event",
		},
		{
			name: "both singular and plural",
			cfg: map[string]any{
				"event_type":  string(models.CustomerActivityInvoiceOverdue),
				"event_types": []any{string(models.CustomerActivityInvoiceOverdue)},
			},
			code: "ambiguous_trigger_config",
		},
		{
			name: "missing events",
			cfg:  map[string]any{},
			code: "invalid_trigger_events",
		},
		{
			name: "empty events",
			cfg:  map[string]any{"event_types": []any{}},
			code: "invalid_trigger_events",
		},
		{
			name: "too many events",
			cfg: map[string]any{"event_types": []any{
				"contact.created", "contact.updated", "contact.merged",
				"message.incoming", "crm.lead.created", "crm.lead.updated",
				"crm.lead.stage_moved", "task.completed", "booking.created",
				"booking.status_changed", "package.sold",
			}},
			code: "invalid_trigger_events",
		},
		{
			name: "duplicate event",
			cfg: map[string]any{"event_types": []any{
				string(models.CustomerActivityInvoicePaid),
				string(models.CustomerActivityInvoicePaid),
			}},
			code: "duplicate_trigger_event",
		},
		{
			name: "non string event",
			cfg:  map[string]any{"event_types": []any{42}},
			code: "invalid_trigger_events",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			graph := validAutomationGraph()
			graph.Nodes[0].Config = test.cfg
			requireAutomationValidationCode(t, validateAutomationGraph(graph, true), test.code)
		})
	}
}

func TestAutomationGraph_RequiresExactlyOneTriggerAndReachableAction(t *testing.T) {
	t.Run("no trigger", func(t *testing.T) {
		graph := validAutomationGraph()
		graph.Nodes = graph.Nodes[1:]
		graph.Edges = graph.Edges[1:]
		requireAutomationValidationCode(t, validateAutomationGraph(graph, true), "single_trigger_required")
	})

	t.Run("two triggers", func(t *testing.T) {
		graph := validAutomationGraph()
		graph.Nodes = append(graph.Nodes, AutomationGraphNode{
			ID:   "trigger_two",
			Type: automationNodeTrigger,
			Config: map[string]any{
				"event_type": string(models.CustomerActivityInvoiceOverdue),
			},
		})
		requireAutomationValidationCode(t, validateAutomationGraph(graph, true), "single_trigger_required")
	})

	t.Run("no action", func(t *testing.T) {
		graph := AutomationGraph{
			Nodes: []AutomationGraphNode{validAutomationGraph().Nodes[0]},
		}
		requireAutomationValidationCode(t, validateAutomationGraph(graph, true), "action_required")
	})

	t.Run("unreachable action", func(t *testing.T) {
		graph := validAutomationGraph()
		graph.Nodes = append(graph.Nodes, AutomationGraphNode{
			ID:   "orphan_task",
			Type: automationNodeCreateTask,
			Config: map[string]any{
				"title": "Never reached",
				"owner": "unassigned",
			},
		})
		validation := validateAutomationGraph(graph, true)
		requireAutomationValidationCode(t, validation, "unreachable_node")
	})
}

func TestAutomationGraph_RejectsInvalidTopology(t *testing.T) {
	tests := []struct {
		name string
		code string
		edit func(*AutomationGraph)
	}{
		{
			name: "dangling target",
			code: "unknown_edge_node",
			edit: func(graph *AutomationGraph) {
				graph.Edges[0].Target = "missing"
			},
		},
		{
			name: "dangling source",
			code: "unknown_edge_node",
			edit: func(graph *AutomationGraph) {
				graph.Edges[0].Source = "missing"
			},
		},
		{
			name: "self edge",
			code: "self_edge",
			edit: func(graph *AutomationGraph) {
				graph.Edges[0].Target = "trigger"
			},
		},
		{
			name: "cycle",
			code: "cycle_detected",
			edit: func(graph *AutomationGraph) {
				graph.Edges = append(graph.Edges, AutomationGraphEdge{
					Source: "task", Target: "condition", Branch: automationBranchAlways,
				})
			},
		},
		{
			name: "unreachable node",
			code: "unreachable_node",
			edit: func(graph *AutomationGraph) {
				graph.Nodes = append(graph.Nodes, AutomationGraphNode{
					ID: "orphan", Type: automationNodeDelay,
					Config: map[string]any{"minutes": 1},
				})
			},
		},
		{
			name: "incoming trigger edge",
			code: "trigger_has_incoming",
			edit: func(graph *AutomationGraph) {
				graph.Edges = append(graph.Edges, AutomationGraphEdge{
					Source: "task", Target: "trigger", Branch: automationBranchAlways,
				})
			},
		},
		{
			name: "two trigger outputs",
			code: "ambiguous_outgoing_edges",
			edit: func(graph *AutomationGraph) {
				graph.Edges = append(graph.Edges, AutomationGraphEdge{
					Source: "trigger", Target: "delay", Branch: automationBranchAlways,
				})
			},
		},
		{
			name: "two delay outputs",
			code: "ambiguous_outgoing_edges",
			edit: func(graph *AutomationGraph) {
				graph.Edges = append(graph.Edges, AutomationGraphEdge{
					Source: "delay", Target: "condition", Branch: automationBranchAlways,
				})
			},
		},
		{
			name: "action is not terminal",
			code: "action_has_outgoing",
			edit: func(graph *AutomationGraph) {
				graph.Edges = append(graph.Edges, AutomationGraphEdge{
					Source: "task", Target: "condition", Branch: automationBranchAlways,
				})
			},
		},
		{
			name: "condition edge missing branch",
			code: "condition_branch_required",
			edit: func(graph *AutomationGraph) {
				graph.Edges[1].Branch = ""
			},
		},
		{
			name: "condition edge has always branch",
			code: "condition_branch_required",
			edit: func(graph *AutomationGraph) {
				graph.Edges[1].Branch = automationBranchAlways
			},
		},
		{
			name: "duplicate condition branch",
			code: "duplicate_condition_branch",
			edit: func(graph *AutomationGraph) {
				graph.Nodes = append(graph.Nodes, AutomationGraphNode{
					ID: "second_task", Type: automationNodeCreateTask,
					Config: map[string]any{"title": "Second task", "owner": "unassigned"},
				})
				graph.Edges = append(graph.Edges, AutomationGraphEdge{
					Source: "condition", Target: "second_task", Branch: automationBranchTrue,
				})
			},
		},
		{
			name: "non condition true branch",
			code: "unexpected_edge_branch",
			edit: func(graph *AutomationGraph) {
				graph.Edges[0].Branch = automationBranchTrue
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			graph := validAutomationGraph()
			test.edit(&graph)
			requireAutomationValidationCode(t, validateAutomationGraph(graph, true), test.code)
		})
	}
}

func TestAutomationGraph_ConditionFieldWhitelist(t *testing.T) {
	allowed := []string{
		"event.event_type",
		"event.category",
		"event.source_type",
		"event.actor_type",
		"event.title",
		"event.summary",
		"event.metadata.to_status",
		"contact.marketing_opt_out",
	}
	for _, field := range allowed {
		t.Run("allowed_"+strings.ReplaceAll(field, ".", "_"), func(t *testing.T) {
			graph := validAutomationGraph()
			graph.Nodes[1].Config["field"] = field
			if field == "contact.marketing_opt_out" {
				graph.Nodes[1].Config["value"] = true
			}
			requireAutomationGraphValid(t, graph)
		})
	}

	rejected := []string{
		"",
		"event.metadata",
		"event.metadata.any_arbitrary_key",
		"event.metadata.to_status.nested",
		"event.metadata.$secret",
		"event.contact_id",
		"contact.profile_name",
	}
	for _, field := range rejected {
		t.Run("rejected_"+strings.ReplaceAll(field, ".", "_"), func(t *testing.T) {
			graph := validAutomationGraph()
			graph.Nodes[1].Config["field"] = field
			requireAutomationValidationCode(
				t,
				validateAutomationGraph(graph, true),
				"invalid_condition_field",
			)
		})
	}
}

func TestAutomationGraph_ConditionOperatorContract(t *testing.T) {
	valid := []struct {
		operator string
		value    any
	}{
		{operator: "equals", value: "completed"},
		{operator: "not_equals", value: "cancelled"},
		{operator: "exists"},
		{operator: "in", value: []any{"completed", "checked_out"}},
	}
	for _, test := range valid {
		t.Run("valid_"+test.operator, func(t *testing.T) {
			graph := validAutomationGraph()
			graph.Nodes[1].Config = map[string]any{
				"field":    "event.metadata.to_status",
				"operator": test.operator,
			}
			if test.value != nil {
				graph.Nodes[1].Config["value"] = test.value
			}
			requireAutomationGraphValid(t, graph)
		})
	}

	rejected := []struct {
		name     string
		operator string
		value    any
	}{
		{name: "contains is outside v1", operator: "contains", value: "complete"},
		{name: "not exists is outside v1", operator: "not_exists"},
		{name: "regex is unsafe", operator: "regex", value: ".*"},
		{name: "in needs a list", operator: "in", value: "completed"},
		{name: "in list cannot be empty", operator: "in", value: []any{}},
		{name: "in entries must be scalar strings", operator: "in", value: []any{"completed", map[string]any{"x": 1}}},
	}
	for _, test := range rejected {
		t.Run(test.name, func(t *testing.T) {
			graph := validAutomationGraph()
			graph.Nodes[1].Config = map[string]any{
				"field":    "event.metadata.to_status",
				"operator": test.operator,
			}
			if test.value != nil {
				graph.Nodes[1].Config["value"] = test.value
			}
			require.NotEmpty(t, validateAutomationGraph(graph, true).Errors)
		})
	}

	t.Run("in list is bounded", func(t *testing.T) {
		graph := validAutomationGraph()
		values := make([]any, 101)
		for i := range values {
			values[i] = "value"
		}
		graph.Nodes[1].Config = map[string]any{
			"field": "event.metadata.to_status", "operator": "in", "value": values,
		}
		require.NotEmpty(t, validateAutomationGraph(graph, true).Errors)
	})

	t.Run("exists rejects a value", func(t *testing.T) {
		graph := validAutomationGraph()
		graph.Nodes[1].Config = map[string]any{
			"field": "event.metadata.to_status", "operator": "exists", "value": "completed",
		}
		requireAutomationValidationCode(
			t,
			validateAutomationGraph(graph, true),
			"unexpected_condition_value",
		)
	})

	t.Run("scalar value is bounded", func(t *testing.T) {
		graph := validAutomationGraph()
		graph.Nodes[1].Config["value"] = strings.Repeat("x", automationMaxConditionValueSize+1)
		requireAutomationValidationCode(
			t,
			validateAutomationGraph(graph, true),
			"condition_value_too_large",
		)
	})
}

func TestAutomationGraph_DelayAndTaskBounds(t *testing.T) {
	for _, minutes := range []any{0, 1, automationMaxDelayMinutes} {
		t.Run("valid_delay", func(t *testing.T) {
			graph := validAutomationGraph()
			graph.Nodes[2].Config["minutes"] = minutes
			requireAutomationGraphValid(t, graph)
		})
	}
	for _, minutes := range []any{-1, automationMaxDelayMinutes + 1, 1.5, "60"} {
		t.Run("invalid_delay", func(t *testing.T) {
			graph := validAutomationGraph()
			graph.Nodes[2].Config["minutes"] = minutes
			requireAutomationValidationCode(
				t,
				validateAutomationGraph(graph, true),
				"invalid_delay",
			)
		})
	}

	t.Run("non finite position", func(t *testing.T) {
		graph := validAutomationGraph()
		graph.Nodes[0].Position.X = math.Inf(1)
		requireAutomationValidationCode(
			t,
			validateAutomationGraph(graph, true),
			"invalid_position",
		)
	})

	t.Run("task title required and bounded", func(t *testing.T) {
		for _, title := range []string{"", strings.Repeat("x", 256)} {
			graph := validAutomationGraph()
			graph.Nodes[3].Config["title"] = title
			requireAutomationValidationCode(
				t,
				validateAutomationGraph(graph, true),
				"invalid_task_title",
			)
		}
	})

	t.Run("task description is bounded", func(t *testing.T) {
		graph := validAutomationGraph()
		graph.Nodes[3].Config["description"] = strings.Repeat("x", automationMaxDescriptionLength+1)
		requireAutomationValidationCode(
			t,
			validateAutomationGraph(graph, true),
			"invalid_task_description",
		)
	})

	for _, key := range []string{"due_in_minutes", "remind_in_minutes"} {
		t.Run(key+"_is_bounded", func(t *testing.T) {
			for _, value := range []any{-1, automationMaxTaskMinutes + 1, 1.5, "15"} {
				graph := validAutomationGraph()
				graph.Nodes[3].Config[key] = value
				requireAutomationValidationCode(
					t,
					validateAutomationGraph(graph, true),
					"invalid_task_schedule",
				)
			}
		})
	}

	t.Run("reminder cannot be after due time", func(t *testing.T) {
		graph := validAutomationGraph()
		graph.Nodes[3].Config["due_in_minutes"] = 30
		graph.Nodes[3].Config["remind_in_minutes"] = 31
		require.NotEmpty(
			t,
			validateAutomationGraph(graph, true).Errors,
			"an activatable graph must not defer this deterministic schedule error to runtime",
		)
	})
}

func TestAutomationGraph_TaskOwnerAndPriorityAreBoundedEnums(t *testing.T) {
	for _, owner := range []string{"unassigned", "contact_owner"} {
		t.Run("owner_"+owner, func(t *testing.T) {
			graph := validAutomationGraph()
			graph.Nodes[3].Config["owner"] = owner
			requireAutomationGraphValid(t, graph)
		})
	}
	t.Run("owner defaults to unassigned", func(t *testing.T) {
		graph := validAutomationGraph()
		delete(graph.Nodes[3].Config, "owner")
		requireAutomationGraphValid(t, graph)
	})
	for _, owner := range []any{"event_actor", "specific_user", 42} {
		t.Run("owner_rejected", func(t *testing.T) {
			graph := validAutomationGraph()
			graph.Nodes[3].Config["owner"] = owner
			requireAutomationValidationCode(
				t,
				validateAutomationGraph(graph, true),
				"invalid_task_owner",
			)
		})
	}
	t.Run("arbitrary owner user id is outside v1", func(t *testing.T) {
		graph := validAutomationGraph()
		graph.Nodes[3].Config["owner"] = "unassigned"
		graph.Nodes[3].Config["owner_user_id"] = uuid.NewString()
		require.NotEmpty(t, validateAutomationGraph(graph, true).Errors)
	})

	for _, priority := range []models.FollowUpTaskPriority{
		models.FollowUpTaskPriorityLow,
		models.FollowUpTaskPriorityNormal,
		models.FollowUpTaskPriorityHigh,
		models.FollowUpTaskPriorityUrgent,
	} {
		t.Run("priority_"+string(priority), func(t *testing.T) {
			graph := validAutomationGraph()
			graph.Nodes[3].Config["priority"] = string(priority)
			requireAutomationGraphValid(t, graph)
		})
	}
	t.Run("priority defaults to normal", func(t *testing.T) {
		graph := validAutomationGraph()
		delete(graph.Nodes[3].Config, "priority")
		requireAutomationGraphValid(t, graph)
	})
	t.Run("priority rejects unknown values", func(t *testing.T) {
		graph := validAutomationGraph()
		graph.Nodes[3].Config["priority"] = "critical"
		requireAutomationValidationCode(
			t,
			validateAutomationGraph(graph, true),
			"invalid_task_priority",
		)
	})
}

func TestAutomationGraph_NodeEdgeAndActionCountsAreBounded(t *testing.T) {
	t.Run("nodes", func(t *testing.T) {
		graph := validAutomationGraph()
		for len(graph.Nodes) <= automationMaxNodes {
			graph.Nodes = append(graph.Nodes, AutomationGraphNode{
				ID:     "extra_" + strings.Repeat("x", len(graph.Nodes)),
				Type:   automationNodeDelay,
				Config: map[string]any{"minutes": 1},
			})
		}
		requireAutomationValidationCode(t, validateAutomationGraph(graph, true), "too_many_nodes")
	})

	t.Run("edges", func(t *testing.T) {
		graph := validAutomationGraph()
		for len(graph.Edges) <= automationMaxEdges {
			graph.Edges = append(graph.Edges, AutomationGraphEdge{
				Source: "trigger", Target: "condition", Branch: automationBranchAlways,
			})
		}
		requireAutomationValidationCode(t, validateAutomationGraph(graph, true), "too_many_edges")
	})

	t.Run("actions", func(t *testing.T) {
		graph := validAutomationGraph()
		for index := 0; index < automationMaxActionNodes; index++ {
			graph.Nodes = append(graph.Nodes, AutomationGraphNode{
				ID:     "extra_task_" + strings.Repeat("x", index+1),
				Type:   automationNodeCreateTask,
				Config: map[string]any{"title": "Extra task", "owner": "unassigned"},
			})
		}
		requireAutomationValidationCode(t, validateAutomationGraph(graph, true), "too_many_actions")
	})

	t.Run("node id", func(t *testing.T) {
		graph := validAutomationGraph()
		graph.Nodes[0].ID = strings.Repeat("x", automationMaxNodeIDLength+1)
		requireAutomationValidationCode(t, validateAutomationGraph(graph, true), "invalid_node_id")
	})
}

func TestAutomationGraph_ConditionOmittedBranchIsCleanStop(t *testing.T) {
	graph := validAutomationGraph()
	requireAutomationGraphValid(t, graph)
	event := automationTestEvent(models.CustomerActivityBookingStatus)
	event.Metadata["to_status"] = "cancelled"

	evaluation, err := evaluateAutomationGraph(
		graph,
		event,
		automationTestContact(),
		time.Date(2026, time.July, 30, 4, 0, 0, 0, time.UTC),
	)
	require.NoError(t, err)
	require.Len(t, evaluation.Steps, 2)
	assert.Equal(t, []string{"trigger", "condition"}, []string{
		evaluation.Steps[0].NodeID,
		evaluation.Steps[1].NodeID,
	})
	assert.Equal(t, automationBranchFalse, evaluation.Steps[1].Branch)
	assert.Empty(t, evaluation.Actions)
	assert.NotEmpty(t, automationReasonCodes(evaluation), "clean stop needs a stable reason code")
}

func TestAutomationGraph_EvaluationFollowsOnlyTheSelectedConditionBranch(t *testing.T) {
	graph := validAutomationGraph()
	graph.Nodes = append(graph.Nodes, AutomationGraphNode{
		ID:   "false_task",
		Type: automationNodeCreateTask,
		Config: map[string]any{
			"title":    "Handle Non-Matching Visit",
			"priority": "low",
			"owner":    "unassigned",
		},
	})
	graph.Edges = append(graph.Edges, AutomationGraphEdge{
		Source: "condition", Target: "false_task", Branch: automationBranchFalse,
	})
	requireAutomationGraphValid(t, graph)

	baseTime := time.Date(2026, time.July, 30, 4, 0, 0, 0, time.UTC)
	event := automationTestEvent(models.CustomerActivityBookingStatus)
	event.Metadata["to_status"] = "cancelled"
	evaluation, err := evaluateAutomationGraph(graph, event, automationTestContact(), baseTime)
	require.NoError(t, err)
	require.Len(t, evaluation.Actions, 1)
	assert.Equal(t, "false_task", evaluation.Actions[0].NodeID)
	assert.Equal(t, baseTime, evaluation.Actions[0].ScheduledAt)
	assert.Equal(t, []string{"trigger", "condition", "false_task"}, []string{
		evaluation.Steps[0].NodeID,
		evaluation.Steps[1].NodeID,
		evaluation.Steps[2].NodeID,
	})
}

func TestAutomationGraph_EvaluationProjectsDeterministicTaskTimingAndText(t *testing.T) {
	graph := validAutomationGraph()
	baseTime := time.Date(2026, time.July, 30, 4, 0, 0, 0, time.UTC)
	event := automationTestEvent(models.CustomerActivityBookingStatus)
	contact := automationTestContact()

	first, err := evaluateAutomationGraph(graph, event, contact, baseTime)
	require.NoError(t, err)
	second, err := evaluateAutomationGraph(graph, event, contact, baseTime)
	require.NoError(t, err)
	assert.Equal(t, first, second, "preview must be deterministic for one graph/event/base time")

	require.Len(t, first.Steps, 4)
	assert.Equal(t, []string{"trigger", "condition", "delay", "task"}, []string{
		first.Steps[0].NodeID,
		first.Steps[1].NodeID,
		first.Steps[2].NodeID,
		first.Steps[3].NodeID,
	})
	assert.Equal(t, automationBranchTrue, first.Steps[1].Branch)
	require.NotNil(t, first.Steps[2].ScheduledAt)
	assert.Equal(t, baseTime.Add(time.Hour), *first.Steps[2].ScheduledAt)

	require.Len(t, first.Actions, 1)
	task := first.Actions[0]
	assert.Equal(t, "Check In After Visit", task.Title, "authored text casing must be preserved")
	assert.Equal(t, "Review Annual Review with Dr Arham.", task.Description)
	assert.Equal(t, models.FollowUpTaskPriorityHigh, task.Priority)
	assert.Equal(t, "contact_owner", task.OwnerMode)
	assert.Nil(t, task.OwnerUserID)
	assert.Equal(t, baseTime.Add(time.Hour), task.ScheduledAt)
	require.NotNil(t, task.DueAt)
	assert.Equal(t, baseTime.Add(3*time.Hour), *task.DueAt)
	require.NotNil(t, task.RemindAt)
	assert.Equal(t, baseTime.Add(90*time.Minute), *task.RemindAt)
}

func TestAutomationGraph_EvaluationSupportsInOperator(t *testing.T) {
	graph := validAutomationGraph()
	graph.Nodes[1].Config = map[string]any{
		"field":    "event.metadata.to_status",
		"operator": "in",
		"value":    []any{"completed", "checked_out"},
	}
	requireAutomationGraphValid(t, graph)

	event := automationTestEvent(models.CustomerActivityBookingStatus)
	evaluation, err := evaluateAutomationGraph(
		graph,
		event,
		automationTestContact(),
		time.Date(2026, time.July, 30, 4, 0, 0, 0, time.UTC),
	)
	require.NoError(t, err)
	require.Len(t, evaluation.Actions, 1)
	assert.Equal(t, "task", evaluation.Actions[0].NodeID)
}

func TestAutomationGraph_EvaluationSkipsNonMatchingTriggerWithReasonCode(t *testing.T) {
	graph := validAutomationGraph()
	requireAutomationGraphValid(t, graph)

	evaluation, err := evaluateAutomationGraph(
		graph,
		automationTestEvent(models.CustomerActivityInvoicePaid),
		automationTestContact(),
		time.Date(2026, time.July, 30, 4, 0, 0, 0, time.UTC),
	)
	require.NoError(t, err)
	require.Len(t, evaluation.Steps, 1)
	assert.Equal(t, models.AutomationExecutionStepStatusSkipped, evaluation.Steps[0].Status)
	assert.Empty(t, evaluation.Actions)
	assert.NotEmpty(t, automationReasonCodes(evaluation), "trigger mismatch needs a stable reason code")
}

func TestAutomationGraph_ValidationAndEvaluationCannotDisagreeOnNormalization(t *testing.T) {
	tests := []struct {
		name string
		edit func(*AutomationGraph)
	}{
		{
			name: "node type whitespace and case",
			edit: func(graph *AutomationGraph) {
				graph.Nodes[0].Type = " Trigger "
			},
		},
		{
			name: "condition branch whitespace and case",
			edit: func(graph *AutomationGraph) {
				graph.Edges[1].Branch = " TRUE "
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			graph := validAutomationGraph()
			test.edit(&graph)
			validation := validateAutomationGraph(graph, true)
			if len(validation.Errors) > 0 {
				return // Rejecting non-canonical input is a safe contract.
			}

			evaluation, err := evaluateAutomationGraph(
				graph,
				automationTestEvent(models.CustomerActivityBookingStatus),
				automationTestContact(),
				time.Date(2026, time.July, 30, 4, 0, 0, 0, time.UTC),
			)
			require.NoError(t, err)
			require.Len(
				t,
				evaluation.Actions,
				1,
				"if validation normalizes a graph, evaluation must use the same normalized graph",
			)
		})
	}
}

func TestAutomationGraph_EvaluatorRejectsInvalidGraphBeforeWalkingIt(t *testing.T) {
	graph := validAutomationGraph()
	graph.Edges[0].Target = "missing"

	evaluation, err := evaluateAutomationGraph(
		graph,
		automationTestEvent(models.CustomerActivityBookingStatus),
		automationTestContact(),
		time.Now(),
	)
	require.ErrorContains(t, err, "invalid")
	assert.Empty(t, evaluation.Steps)
	assert.Empty(t, evaluation.Actions)
}

func TestAutomationGraph_EvaluatorDoesNotMutateTheGraphOrEvent(t *testing.T) {
	graph := validAutomationGraph()
	event := automationTestEvent(models.CustomerActivityBookingStatus)
	graphBefore, err := json.Marshal(graph)
	require.NoError(t, err)
	eventBefore := *event
	eventBefore.Metadata = models.JSONB{}
	for key, value := range event.Metadata {
		eventBefore.Metadata[key] = value
	}

	_, err = evaluateAutomationGraph(
		graph,
		event,
		automationTestContact(),
		time.Date(2026, time.July, 30, 4, 0, 0, 0, time.UTC),
	)
	require.NoError(t, err)
	graphAfter, err := json.Marshal(graph)
	require.NoError(t, err)
	assert.JSONEq(t, string(graphBefore), string(graphAfter))
	assert.True(t, reflect.DeepEqual(eventBefore, *event))
}
