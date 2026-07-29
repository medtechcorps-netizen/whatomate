package handlers

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/models"
)

const (
	automationMaxRequestBytes       = 256 * 1024
	automationMaxGraphBytes         = 128 * 1024
	automationMaxNodes              = 50
	automationMaxEdges              = 100
	automationMaxActionNodes        = 10
	automationMaxNodeIDLength       = 100
	automationMaxPolicyNameLength   = 150
	automationMaxDescriptionLength  = 4000
	automationMaxConditionValueSize = 500
	automationMaxDelayMinutes       = 30 * 24 * 60
	automationMaxTaskMinutes        = 365 * 24 * 60
	automationMaxCanvasCoordinate   = 100000

	automationNodeTrigger    = "trigger"
	automationNodeCondition  = "condition"
	automationNodeDelay      = "delay"
	automationNodeCreateTask = "create_task"

	automationBranchTrue   = "true"
	automationBranchFalse  = "false"
	automationBranchAlways = "always"
)

var automationAllowedEventTypes = []string{
	string(models.CustomerActivityContactCreated),
	string(models.CustomerActivityContactUpdated),
	string(models.CustomerActivityContactMerged),
	string(models.CustomerActivityMessageIncoming),
	string(models.CustomerActivityCRMLeadCreated),
	string(models.CustomerActivityCRMLeadUpdated),
	string(models.CustomerActivityCRMStageMoved),
	string(models.CustomerActivityTaskCompleted),
	string(models.CustomerActivityBookingCreated),
	string(models.CustomerActivityBookingStatus),
	string(models.CustomerActivityPackageSold),
	string(models.CustomerActivityPackageLow),
	string(models.CustomerActivityPackageExpiring),
	string(models.CustomerActivityInvoiceCreated),
	string(models.CustomerActivityInvoiceOverdue),
	string(models.CustomerActivityInvoicePaid),
	string(models.CustomerActivityPaymentRecorded),
	string(models.CustomerActivityConsentOptedOut),
}

var automationAllowedConditionFields = []string{
	"event.event_type",
	"event.category",
	"event.source_type",
	"event.actor_type",
	"event.title",
	"event.summary",
	"event.metadata.to_status",
	"contact.marketing_opt_out",
}

var automationAllowedConditionOperators = []string{
	"equals",
	"not_equals",
	"exists",
	"in",
}

var automationAllowedTemplateTokens = []string{
	"{{event.title}}",
	"{{event.summary}}",
	"{{event.type}}",
	"{{contact.name}}",
}

type AutomationGraph struct {
	Nodes []AutomationGraphNode `json:"nodes"`
	Edges []AutomationGraphEdge `json:"edges"`
}

type AutomationGraphNode struct {
	ID       string                  `json:"id"`
	Type     string                  `json:"type"`
	Config   map[string]any          `json:"config,omitempty"`
	Position *AutomationNodePosition `json:"position,omitempty"`
}

type AutomationNodePosition struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
}

type AutomationGraphEdge struct {
	Source string `json:"source"`
	Target string `json:"target"`
	Branch string `json:"branch,omitempty"`
}

type AutomationValidationIssue struct {
	Code    string `json:"code"`
	NodeID  string `json:"node_id,omitempty"`
	Edge    int    `json:"edge,omitempty"`
	Message string `json:"message"`
}

type automationGraphValidation struct {
	Errors            []AutomationValidationIssue
	Warnings          []AutomationValidationIssue
	TriggerEventTypes []string
	OwnerUserIDs      []uuid.UUID
}

type automationEvaluationStep struct {
	NodeID      string
	NodeType    string
	Status      models.AutomationExecutionStepStatus
	Branch      string
	ScheduledAt *time.Time
	Detail      string
	Output      models.JSONB
}

type automationPlannedTask struct {
	NodeID      string
	Title       string
	Description string
	Priority    models.FollowUpTaskPriority
	OwnerMode   string
	OwnerUserID *uuid.UUID
	ScheduledAt time.Time
	DueAt       *time.Time
	RemindAt    *time.Time
}

type automationEvaluation struct {
	Steps   []automationEvaluationStep
	Actions []automationPlannedTask
}

func automationGraphFromJSONB(value models.JSONB) (AutomationGraph, error) {
	var graph AutomationGraph
	encoded, err := json.Marshal(value)
	if err != nil {
		return graph, err
	}
	if len(encoded) > automationMaxGraphBytes {
		return graph, errors.New("graph exceeds the maximum encoded size")
	}
	if err := json.Unmarshal(encoded, &graph); err != nil {
		return graph, err
	}
	return graph, nil
}

func automationGraphToJSONB(graph AutomationGraph) (models.JSONB, error) {
	encoded, err := json.Marshal(graph)
	if err != nil {
		return nil, err
	}
	if len(encoded) > automationMaxGraphBytes {
		return nil, errors.New("graph exceeds the maximum encoded size")
	}
	var value models.JSONB
	if err := json.Unmarshal(encoded, &value); err != nil {
		return nil, err
	}
	return value, nil
}

func automationGraphChecksum(graph AutomationGraph) (string, error) {
	encoded, err := json.Marshal(graph)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}

// automationGraphSemanticFingerprint identifies execution behavior rather than
// canvas serialization. It deliberately ignores generated node IDs, positions,
// node/edge array order, and equivalent default configuration.
func automationGraphSemanticFingerprint(graph AutomationGraph) (string, error) {
	nodes := make(map[string]AutomationGraphNode, len(graph.Nodes))
	outgoing := make(map[string][]AutomationGraphEdge, len(graph.Nodes))
	triggerID := ""
	for _, node := range graph.Nodes {
		nodes[node.ID] = node
		if node.Type == automationNodeTrigger {
			triggerID = node.ID
		}
	}
	for _, edge := range graph.Edges {
		if edge.Branch == "" {
			edge.Branch = automationBranchAlways
		}
		outgoing[edge.Source] = append(outgoing[edge.Source], edge)
	}
	if triggerID == "" {
		return "", errors.New("semantic fingerprint requires a trigger")
	}
	active := map[string]bool{}
	var build func(string) (map[string]any, error)
	build = func(nodeID string) (map[string]any, error) {
		if active[nodeID] {
			return nil, errors.New("semantic fingerprint cannot encode a cycle")
		}
		node, ok := nodes[nodeID]
		if !ok {
			return nil, errors.New("semantic fingerprint references an unknown node")
		}
		active[nodeID] = true
		defer delete(active, nodeID)

		config := map[string]any{}
		switch node.Type {
		case automationNodeTrigger:
			eventTypes, issue := automationTriggerEventTypes(node)
			if issue != nil {
				return nil, errors.New(issue.Message)
			}
			config["event_types"] = eventTypes
		case automationNodeCondition:
			field, _ := automationConfigNormalizedString(node.Config, "field")
			operator, _ := automationConfigNormalizedString(node.Config, "operator")
			config["field"] = field
			config["operator"] = operator
			if value, exists := node.Config["value"]; exists {
				config["value"] = value
			}
		case automationNodeDelay:
			minutes, _ := automationConfigInt(node.Config, "minutes")
			config["minutes"] = minutes
		case automationNodeCreateTask:
			title, _ := automationConfigRawString(node.Config, "title")
			description, _ := automationConfigRawString(node.Config, "description")
			title = strings.TrimSpace(title)
			description = strings.TrimSpace(description)
			priority, _ := automationConfigNormalizedString(node.Config, "priority")
			if priority == "" {
				priority = string(models.FollowUpTaskPriorityNormal)
			}
			owner, _ := automationConfigNormalizedString(node.Config, "owner")
			if owner == "" {
				owner = "unassigned"
			}
			config["title"] = title
			config["description"] = description
			config["priority"] = priority
			config["owner"] = owner
			if minutes, ok := automationConfigInt(node.Config, "due_in_minutes"); ok {
				config["due_in_minutes"] = minutes
			}
			if minutes, ok := automationConfigInt(node.Config, "remind_in_minutes"); ok {
				config["remind_in_minutes"] = minutes
			}
		default:
			return nil, errors.New("semantic fingerprint contains an unsupported node")
		}

		children := make([]map[string]any, 0, len(outgoing[nodeID]))
		for _, edge := range outgoing[nodeID] {
			child, err := build(edge.Target)
			if err != nil {
				return nil, err
			}
			children = append(children, map[string]any{
				"branch": edge.Branch,
				"node":   child,
			})
		}
		sort.Slice(children, func(i, j int) bool {
			left, _ := json.Marshal(children[i])
			right, _ := json.Marshal(children[j])
			return string(left) < string(right)
		})
		return map[string]any{
			"type":     node.Type,
			"config":   config,
			"children": children,
		}, nil
	}
	semantic, err := build(triggerID)
	if err != nil {
		return "", err
	}
	encoded, err := json.Marshal(semantic)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}

func validateAutomationGraph(graph AutomationGraph, activation bool) automationGraphValidation {
	result := automationGraphValidation{}
	addError := func(code, nodeID, message string) {
		result.Errors = append(result.Errors, AutomationValidationIssue{
			Code: code, NodeID: nodeID, Message: message,
		})
	}
	if len(graph.Nodes) > automationMaxNodes {
		addError("too_many_nodes", "", fmt.Sprintf("graph cannot contain more than %d nodes", automationMaxNodes))
	}
	if len(graph.Edges) > automationMaxEdges {
		addError("too_many_edges", "", fmt.Sprintf("graph cannot contain more than %d edges", automationMaxEdges))
	}
	if activation && len(graph.Nodes) == 0 {
		addError("nodes_required", "", "graph must contain nodes")
	}

	nodes := make(map[string]AutomationGraphNode, len(graph.Nodes))
	nodeOrder := make([]string, 0, len(graph.Nodes))
	triggerIDs := make([]string, 0, 1)
	actionCount := 0
	ownerIDs := map[uuid.UUID]struct{}{}
	for i := range graph.Nodes {
		node := graph.Nodes[i]
		rawID := node.ID
		rawType := node.Type
		node.ID = strings.TrimSpace(node.ID)
		node.Type = strings.ToLower(strings.TrimSpace(node.Type))
		if rawID != node.ID || rawType != node.Type {
			addError("noncanonical_node", node.ID, "node id and type must already be trimmed lowercase canonical values")
			continue
		}
		if node.ID == "" || len(node.ID) > automationMaxNodeIDLength {
			addError("invalid_node_id", node.ID, fmt.Sprintf("node id is required and cannot exceed %d characters", automationMaxNodeIDLength))
			continue
		}
		if _, duplicate := nodes[node.ID]; duplicate {
			addError("duplicate_node_id", node.ID, "node ids must be unique")
			continue
		}
		if node.Position != nil &&
			(math.IsNaN(node.Position.X) || math.IsInf(node.Position.X, 0) ||
				math.IsNaN(node.Position.Y) || math.IsInf(node.Position.Y, 0) ||
				math.Abs(node.Position.X) > automationMaxCanvasCoordinate ||
				math.Abs(node.Position.Y) > automationMaxCanvasCoordinate) {
			addError(
				"invalid_position",
				node.ID,
				fmt.Sprintf(
					"node position must be finite and within +/- %d",
					automationMaxCanvasCoordinate,
				),
			)
		}
		nodes[node.ID] = node
		nodeOrder = append(nodeOrder, node.ID)
		switch node.Type {
		case automationNodeTrigger:
			if key := automationUnknownConfigKey(node.Config, "event_type", "event_types"); key != "" {
				addError("unknown_node_config", node.ID, "trigger contains unsupported config key "+key)
			}
			triggerIDs = append(triggerIDs, node.ID)
			eventTypes, issue := automationTriggerEventTypes(node)
			if issue != nil {
				addError(issue.Code, node.ID, issue.Message)
			} else {
				result.TriggerEventTypes = append(result.TriggerEventTypes, eventTypes...)
			}
		case automationNodeCondition:
			if key := automationUnknownConfigKey(node.Config, "field", "operator", "value"); key != "" {
				addError("unknown_node_config", node.ID, "condition contains unsupported config key "+key)
			}
			if issue := validateAutomationConditionNode(node); issue != nil {
				addError(issue.Code, node.ID, issue.Message)
			}
		case automationNodeDelay:
			if key := automationUnknownConfigKey(node.Config, "minutes"); key != "" {
				addError("unknown_node_config", node.ID, "delay contains unsupported config key "+key)
			}
			if issue := validateAutomationDelayNode(node); issue != nil {
				addError(issue.Code, node.ID, issue.Message)
			}
		case automationNodeCreateTask:
			if key := automationUnknownConfigKey(
				node.Config,
				"title",
				"description",
				"priority",
				"due_in_minutes",
				"remind_in_minutes",
				"owner",
			); key != "" {
				addError("unknown_node_config", node.ID, "create_task contains unsupported config key "+key)
			}
			actionCount++
			ownerID, issue := validateAutomationCreateTaskNode(node)
			if issue != nil {
				addError(issue.Code, node.ID, issue.Message)
			}
			if ownerID != nil {
				ownerIDs[*ownerID] = struct{}{}
			}
			if automationNodeUsesDynamicTemplate(node) {
				result.Warnings = append(result.Warnings, AutomationValidationIssue{
					Code:    "dynamic_template_truncation",
					NodeID:  node.ID,
					Message: "dynamic task text is safely truncated if customer data exceeds field limits",
				})
			}
		default:
			addError("unsupported_node_type", node.ID, "node type must be trigger, condition, delay, or create_task")
		}
	}
	if actionCount > automationMaxActionNodes {
		addError("too_many_actions", "", fmt.Sprintf("graph cannot contain more than %d create_task nodes", automationMaxActionNodes))
	}
	if activation && len(triggerIDs) != 1 {
		addError("single_trigger_required", "", "graph must contain exactly one trigger node")
	}
	if activation && actionCount == 0 {
		addError("action_required", "", "graph must contain at least one create_task node")
	}

	incoming := make(map[string]int, len(nodes))
	outgoing := make(map[string][]AutomationGraphEdge, len(nodes))
	for index, rawEdge := range graph.Edges {
		edge := rawEdge
		edge.Source = strings.TrimSpace(edge.Source)
		edge.Target = strings.TrimSpace(edge.Target)
		edge.Branch = strings.ToLower(strings.TrimSpace(edge.Branch))
		if rawEdge.Source != edge.Source || rawEdge.Target != edge.Target ||
			rawEdge.Branch != edge.Branch {
			result.Errors = append(result.Errors, AutomationValidationIssue{
				Code: "noncanonical_edge", Edge: index,
				Message: "edge source, target, and branch must use canonical values",
			})
			continue
		}
		source, sourceOK := nodes[edge.Source]
		_, targetOK := nodes[edge.Target]
		if !sourceOK || !targetOK {
			result.Errors = append(result.Errors, AutomationValidationIssue{
				Code: "unknown_edge_node", Edge: index,
				Message: "edge source and target must reference existing nodes",
			})
			continue
		}
		if edge.Source == edge.Target {
			result.Errors = append(result.Errors, AutomationValidationIssue{
				Code: "self_edge", Edge: index, Message: "a node cannot connect to itself",
			})
			continue
		}
		if source.Type == automationNodeCondition {
			if edge.Branch != automationBranchTrue && edge.Branch != automationBranchFalse {
				result.Errors = append(result.Errors, AutomationValidationIssue{
					Code: "condition_branch_required", Edge: index,
					Message: "condition edges must declare branch true or false",
				})
			}
		} else if edge.Branch != "" && edge.Branch != automationBranchAlways {
			result.Errors = append(result.Errors, AutomationValidationIssue{
				Code: "unexpected_edge_branch", Edge: index,
				Message: "only condition edges can declare true or false branches",
			})
		}
		incoming[edge.Target]++
		outgoing[edge.Source] = append(outgoing[edge.Source], edge)
	}

	for nodeID, node := range nodes {
		if node.Type == automationNodeTrigger && incoming[nodeID] != 0 {
			addError("trigger_has_incoming", nodeID, "trigger cannot have an incoming edge")
		}
		if node.Type != automationNodeTrigger && activation && incoming[nodeID] != 1 {
			addError("invalid_incoming_count", nodeID, "every non-trigger node must have exactly one incoming edge")
		}
		edges := outgoing[nodeID]
		switch node.Type {
		case automationNodeTrigger, automationNodeDelay:
			if len(edges) > 1 || (activation && len(edges) != 1) {
				addError("ambiguous_outgoing_edges", nodeID, "trigger and delay nodes must have exactly one outgoing edge")
			}
		case automationNodeCreateTask:
			if len(edges) != 0 {
				addError("action_has_outgoing", nodeID, "create_task nodes cannot have outgoing edges")
			}
		case automationNodeCondition:
			seen := map[string]bool{}
			for _, edge := range edges {
				if seen[edge.Branch] {
					addError("duplicate_condition_branch", nodeID, "a condition can have at most one true and one false edge")
				}
				seen[edge.Branch] = true
			}
			if len(edges) > 2 {
				addError("too_many_condition_branches", nodeID, "a condition can have at most two outgoing edges")
			}
			if activation && len(edges) == 0 {
				addError("condition_branch_required", nodeID, "a condition must have at least one outgoing branch")
			}
		}
	}

	if activation && len(triggerIDs) == 1 {
		visited := map[string]bool{}
		active := map[string]bool{}
		var walk func(string)
		walk = func(nodeID string) {
			if active[nodeID] {
				addError("cycle_detected", nodeID, "graph must not contain a cycle")
				return
			}
			if visited[nodeID] {
				return
			}
			active[nodeID] = true
			for _, edge := range outgoing[nodeID] {
				walk(edge.Target)
			}
			active[nodeID] = false
			visited[nodeID] = true
		}
		walk(triggerIDs[0])
		for _, nodeID := range nodeOrder {
			if !visited[nodeID] {
				addError("unreachable_node", nodeID, "every node must be reachable from the trigger")
			}
		}

		// A delay node is individually bounded, but a path may contain more
		// than one delay. Reject that graph before publication so the runtime
		// can never discover an invalid cumulative schedule after an event has
		// already been accepted.
		// Compute the longest delay to every node once in topological order.
		// This keeps validation O(nodes + edges), including malformed draft
		// graphs with many converging paths.
		remainingIncoming := make(map[string]int, len(nodes))
		queue := make([]string, 0, len(nodes))
		for _, nodeID := range nodeOrder {
			remainingIncoming[nodeID] = incoming[nodeID]
			if incoming[nodeID] == 0 {
				queue = append(queue, nodeID)
			}
		}
		bestDelay := map[string]int{triggerIDs[0]: 0}
		delayReachable := map[string]bool{triggerIDs[0]: true}
		for len(queue) > 0 {
			nodeID := queue[0]
			queue = queue[1:]
			cumulative := bestDelay[nodeID]
			reachable := delayReachable[nodeID]
			if reachable && nodes[nodeID].Type == automationNodeDelay {
				if minutes, ok := automationConfigInt(nodes[nodeID].Config, "minutes"); ok {
					cumulative += minutes
					if cumulative > automationMaxDelayMinutes {
						addError(
							"cumulative_delay_exceeded",
							nodeID,
							fmt.Sprintf("cumulative delay on every path cannot exceed %d minutes", automationMaxDelayMinutes),
						)
					}
				}
			}
			for _, edge := range outgoing[nodeID] {
				if reachable {
					delayReachable[edge.Target] = true
					if cumulative > bestDelay[edge.Target] {
						bestDelay[edge.Target] = cumulative
					}
				}
				remainingIncoming[edge.Target]--
				if remainingIncoming[edge.Target] == 0 {
					queue = append(queue, edge.Target)
				}
			}
		}
	}

	result.TriggerEventTypes = automationUniqueSortedStrings(result.TriggerEventTypes)
	for ownerID := range ownerIDs {
		result.OwnerUserIDs = append(result.OwnerUserIDs, ownerID)
	}
	sort.Slice(result.OwnerUserIDs, func(i, j int) bool {
		return result.OwnerUserIDs[i].String() < result.OwnerUserIDs[j].String()
	})
	return result
}

func automationTriggerEventTypes(node AutomationGraphNode) ([]string, *AutomationValidationIssue) {
	rawTypes, hasTypes := node.Config["event_types"]
	rawType, hasType := node.Config["event_type"]
	if hasTypes && hasType {
		return nil, &AutomationValidationIssue{
			Code: "ambiguous_trigger_config", Message: "trigger must use event_types or event_type, not both",
		}
	}
	var values []string
	if hasTypes {
		items, ok := rawTypes.([]any)
		if !ok {
			if typed, stringOK := rawTypes.([]string); stringOK {
				values = append(values, typed...)
			} else {
				return nil, &AutomationValidationIssue{
					Code: "invalid_trigger_events", Message: "trigger event_types must be an array",
				}
			}
		} else {
			for _, item := range items {
				value, ok := item.(string)
				if !ok {
					return nil, &AutomationValidationIssue{
						Code: "invalid_trigger_events", Message: "trigger event_types entries must be strings",
					}
				}
				values = append(values, value)
			}
		}
	} else if hasType {
		value, ok := rawType.(string)
		if !ok {
			return nil, &AutomationValidationIssue{
				Code: "invalid_trigger_events", Message: "trigger event_type must be a string",
			}
		}
		values = []string{value}
	}
	if len(values) == 0 || len(values) > 10 {
		return nil, &AutomationValidationIssue{
			Code: "invalid_trigger_events", Message: "trigger requires between 1 and 10 event types",
		}
	}
	allowed := automationStringSet(automationAllowedEventTypes)
	seen := map[string]bool{}
	for i := range values {
		values[i] = strings.ToLower(strings.TrimSpace(values[i]))
		if values[i] == string(models.CustomerActivityTaskCreated) {
			return nil, &AutomationValidationIssue{
				Code: "recursive_trigger_forbidden", Message: "task.created cannot trigger a create_task automation",
			}
		}
		if !allowed[values[i]] {
			return nil, &AutomationValidationIssue{
				Code: "unsupported_trigger_event", Message: "trigger contains an unsupported customer activity event type",
			}
		}
		if seen[values[i]] {
			return nil, &AutomationValidationIssue{
				Code: "duplicate_trigger_event", Message: "trigger event types must be unique",
			}
		}
		seen[values[i]] = true
	}
	sort.Strings(values)
	return values, nil
}

func validateAutomationConditionNode(node AutomationGraphNode) *AutomationValidationIssue {
	field, ok := automationConfigNormalizedString(node.Config, "field")
	if !ok || !automationValidConditionField(field) {
		return &AutomationValidationIssue{
			Code: "invalid_condition_field", Message: "condition field is not supported",
		}
	}
	operator, ok := automationConfigNormalizedString(node.Config, "operator")
	if !ok || !automationStringSet(automationAllowedConditionOperators)[operator] {
		return &AutomationValidationIssue{
			Code: "invalid_condition_operator", Message: "condition operator is not supported",
		}
	}
	value, hasValue := node.Config["value"]
	if operator == "exists" {
		if hasValue && value != nil {
			return &AutomationValidationIssue{
				Code: "unexpected_condition_value", Message: "exists operators cannot include a value",
			}
		}
		return nil
	}
	if field == "contact.marketing_opt_out" {
		if operator == "in" {
			return &AutomationValidationIssue{
				Code:    "invalid_condition_operator",
				Message: "contact.marketing_opt_out supports equals, not_equals, or exists",
			}
		}
		if !hasValue {
			return &AutomationValidationIssue{
				Code: "condition_value_required", Message: "condition requires a boolean value",
			}
		}
		if _, ok := value.(bool); !ok {
			return &AutomationValidationIssue{
				Code:    "invalid_condition_value",
				Message: "contact.marketing_opt_out requires a boolean value",
			}
		}
		return nil
	}
	if operator == "in" {
		values, ok := value.([]any)
		if !ok || len(values) == 0 || len(values) > 100 {
			return &AutomationValidationIssue{
				Code: "invalid_condition_list", Message: "in requires between 1 and 100 scalar string values",
			}
		}
		for _, item := range values {
			text, ok := item.(string)
			if !ok || len(text) > automationMaxConditionValueSize {
				return &AutomationValidationIssue{
					Code: "invalid_condition_list", Message: "in requires bounded scalar string values",
				}
			}
		}
		return nil
	}
	if !hasValue || !automationIsScalar(value) {
		return &AutomationValidationIssue{
			Code: "condition_value_required", Message: "condition requires a scalar value",
		}
	}
	if _, ok := value.(string); !ok {
		return &AutomationValidationIssue{
			Code: "invalid_condition_value", Message: "event condition fields require a string value",
		}
	}
	encoded, _ := json.Marshal(value)
	if len(encoded) > automationMaxConditionValueSize {
		return &AutomationValidationIssue{
			Code: "condition_value_too_large", Message: "condition value is too large",
		}
	}
	return nil
}

func validateAutomationDelayNode(node AutomationGraphNode) *AutomationValidationIssue {
	minutes, ok := automationConfigInt(node.Config, "minutes")
	if !ok || minutes < 0 || minutes > automationMaxDelayMinutes {
		return &AutomationValidationIssue{
			Code: "invalid_delay", Message: fmt.Sprintf("delay minutes must be an integer between 0 and %d", automationMaxDelayMinutes),
		}
	}
	return nil
}

func validateAutomationCreateTaskNode(node AutomationGraphNode) (*uuid.UUID, *AutomationValidationIssue) {
	title, ok := automationConfigRawString(node.Config, "title")
	if !ok || strings.TrimSpace(title) == "" || utf8.RuneCountInString(title) > 255 {
		return nil, &AutomationValidationIssue{
			Code: "invalid_task_title", Message: "create_task title is required and cannot exceed 255 characters",
		}
	}
	if !automationTemplateTokensValid(title) {
		return nil, &AutomationValidationIssue{
			Code: "invalid_task_template", Message: "create_task title contains an unsupported template token",
		}
	}
	if description, exists := node.Config["description"]; exists {
		value, ok := description.(string)
		if !ok || utf8.RuneCountInString(value) > automationMaxDescriptionLength {
			return nil, &AutomationValidationIssue{
				Code: "invalid_task_description", Message: fmt.Sprintf("create_task description cannot exceed %d characters", automationMaxDescriptionLength),
			}
		}
		if !automationTemplateTokensValid(value) {
			return nil, &AutomationValidationIssue{
				Code: "invalid_task_template", Message: "create_task description contains an unsupported template token",
			}
		}
	}
	if priority, exists := node.Config["priority"]; exists {
		value, ok := priority.(string)
		if !ok || !automationValidTaskPriority(strings.ToLower(strings.TrimSpace(value))) {
			return nil, &AutomationValidationIssue{
				Code: "invalid_task_priority", Message: "create_task priority is invalid",
			}
		}
	}
	for _, key := range []string{"due_in_minutes", "remind_in_minutes"} {
		if _, exists := node.Config[key]; !exists {
			continue
		}
		value, ok := automationConfigInt(node.Config, key)
		if !ok || value < 0 || value > automationMaxTaskMinutes {
			return nil, &AutomationValidationIssue{
				Code: "invalid_task_schedule", Message: fmt.Sprintf("%s must be an integer between 0 and %d", key, automationMaxTaskMinutes),
			}
		}
	}
	dueMinutes, hasDue := automationConfigInt(node.Config, "due_in_minutes")
	remindMinutes, hasRemind := automationConfigInt(node.Config, "remind_in_minutes")
	if hasDue && hasRemind && remindMinutes > dueMinutes {
		return nil, &AutomationValidationIssue{
			Code: "invalid_task_schedule", Message: "remind_in_minutes cannot be after due_in_minutes",
		}
	}
	ownerMode := "unassigned"
	if value, exists := node.Config["owner"]; exists {
		owner, ok := value.(string)
		if !ok {
			return nil, &AutomationValidationIssue{Code: "invalid_task_owner", Message: "create_task owner is invalid"}
		}
		ownerMode = strings.ToLower(strings.TrimSpace(owner))
	}
	if ownerMode != "unassigned" && ownerMode != "contact_owner" {
		return nil, &AutomationValidationIssue{
			Code: "invalid_task_owner", Message: "create_task owner must be unassigned or contact_owner",
		}
	}
	if _, exists := node.Config["owner_user_id"]; exists {
		return nil, &AutomationValidationIssue{
			Code: "invalid_task_owner", Message: "owner_user_id is outside the v1 automation contract",
		}
	}
	return nil, nil
}

func automationValidConditionField(field string) bool {
	field = strings.ToLower(strings.TrimSpace(field))
	return automationStringSet(automationAllowedConditionFields)[field]
}

func automationUnknownConfigKey(config map[string]any, allowed ...string) string {
	allowedSet := automationStringSet(allowed)
	unknown := make([]string, 0)
	for key := range config {
		if !allowedSet[key] {
			unknown = append(unknown, key)
		}
	}
	sort.Strings(unknown)
	if len(unknown) == 0 {
		return ""
	}
	return unknown[0]
}

func automationNodeUsesDynamicTemplate(node AutomationGraphNode) bool {
	title, _ := automationConfigRawString(node.Config, "title")
	description, _ := automationConfigRawString(node.Config, "description")
	for _, token := range automationAllowedTemplateTokens {
		if strings.Contains(title, token) || strings.Contains(description, token) {
			return true
		}
	}
	return false
}

func automationValidTaskPriority(value string) bool {
	switch models.FollowUpTaskPriority(value) {
	case models.FollowUpTaskPriorityLow,
		models.FollowUpTaskPriorityNormal,
		models.FollowUpTaskPriorityHigh,
		models.FollowUpTaskPriorityUrgent:
		return true
	default:
		return false
	}
}

func evaluateAutomationGraph(
	graph AutomationGraph,
	event *models.CustomerActivityEvent,
	contact *models.Contact,
	baseTime time.Time,
) (automationEvaluation, error) {
	validation := validateAutomationGraph(graph, true)
	if len(validation.Errors) > 0 {
		return automationEvaluation{}, errors.New("automation graph is invalid")
	}
	nodes := make(map[string]AutomationGraphNode, len(graph.Nodes))
	outgoing := make(map[string][]AutomationGraphEdge, len(graph.Nodes))
	var trigger AutomationGraphNode
	for _, node := range graph.Nodes {
		nodes[node.ID] = node
		if node.Type == automationNodeTrigger {
			trigger = node
		}
	}
	for _, edge := range graph.Edges {
		if edge.Branch == "" {
			edge.Branch = automationBranchAlways
		}
		outgoing[edge.Source] = append(outgoing[edge.Source], edge)
	}
	eventTypes, _ := automationTriggerEventTypes(trigger)
	if event == nil || !automationStringSet(eventTypes)[string(event.EventType)] {
		return automationEvaluation{
			Steps: []automationEvaluationStep{{
				NodeID: trigger.ID, NodeType: trigger.Type,
				Status: models.AutomationExecutionStepStatusSkipped,
				Detail: "event type does not match the trigger",
				Output: models.JSONB{"reason_code": "trigger_mismatch"},
			}},
		}, nil
	}
	if baseTime.IsZero() {
		baseTime = time.Now().UTC()
	}
	type queueItem struct {
		NodeID       string
		DelayMinutes int
	}
	queue := []queueItem{{NodeID: trigger.ID}}
	evaluation := automationEvaluation{}
	for len(queue) > 0 {
		item := queue[0]
		queue = queue[1:]
		node := nodes[item.NodeID]
		step := automationEvaluationStep{
			NodeID: node.ID, NodeType: node.Type,
			Status: models.AutomationExecutionStepStatusCompleted,
			Output: models.JSONB{},
		}
		nextEdges := outgoing[node.ID]
		switch node.Type {
		case automationNodeTrigger:
			step.Detail = "trigger matched " + string(event.EventType)
		case automationNodeCondition:
			matched, err := evaluateAutomationCondition(node, event, contact)
			if err != nil {
				return automationEvaluation{}, err
			}
			step.Branch = strconv.FormatBool(matched)
			step.Output["matched"] = matched
			step.Output["reason_code"] = "condition_" + step.Branch
			step.Detail = "condition evaluated " + step.Branch
			selected := nextEdges[:0]
			for _, edge := range nextEdges {
				if edge.Branch == step.Branch {
					selected = append(selected, edge)
				}
			}
			nextEdges = selected
			if len(nextEdges) == 0 {
				step.Output["stop_reason"] = "condition_branch_omitted"
			}
		case automationNodeDelay:
			minutes, _ := automationConfigInt(node.Config, "minutes")
			item.DelayMinutes += minutes
			if item.DelayMinutes > automationMaxDelayMinutes {
				return automationEvaluation{}, errors.New("cumulative delay exceeds maximum")
			}
			scheduledAt := baseTime.Add(time.Duration(item.DelayMinutes) * time.Minute).UTC()
			step.ScheduledAt = &scheduledAt
			step.Output["delay_minutes"] = minutes
			step.Output["cumulative_delay_minutes"] = item.DelayMinutes
			step.Detail = fmt.Sprintf("wait %d minutes", minutes)
		case automationNodeCreateTask:
			task, err := automationTaskFromNode(node, contact, event, baseTime, item.DelayMinutes)
			if err != nil {
				return automationEvaluation{}, err
			}
			evaluation.Actions = append(evaluation.Actions, task)
			step.Status = models.AutomationExecutionStepStatusPending
			step.ScheduledAt = &task.ScheduledAt
			step.Detail = "task scheduled"
		}
		evaluation.Steps = append(evaluation.Steps, step)
		for _, edge := range nextEdges {
			queue = append(queue, queueItem{
				NodeID: edge.Target, DelayMinutes: item.DelayMinutes,
			})
		}
	}
	return evaluation, nil
}

func evaluateAutomationCondition(
	node AutomationGraphNode,
	event *models.CustomerActivityEvent,
	contact *models.Contact,
) (bool, error) {
	field, _ := automationConfigNormalizedString(node.Config, "field")
	operator, _ := automationConfigNormalizedString(node.Config, "operator")
	actual, exists := automationConditionFieldValue(field, event, contact)
	switch operator {
	case "exists":
		return exists && actual != nil, nil
	}
	expected := node.Config["value"]
	switch operator {
	case "equals":
		return automationScalarEqual(actual, expected), nil
	case "not_equals":
		return !automationScalarEqual(actual, expected), nil
	case "in":
		values, ok := expected.([]any)
		if !ok {
			return false, errors.New("condition in value is invalid")
		}
		for _, item := range values {
			if automationScalarEqual(actual, item) {
				return true, nil
			}
		}
		return false, nil
	default:
		return false, errors.New("unsupported condition operator")
	}
}

func automationConditionFieldValue(
	field string,
	event *models.CustomerActivityEvent,
	contact *models.Contact,
) (any, bool) {
	switch strings.ToLower(strings.TrimSpace(field)) {
	case "event.event_type":
		return string(event.EventType), true
	case "event.category":
		return string(event.Category), true
	case "event.source_type":
		return event.SourceObjectType, true
	case "event.actor_type":
		return string(event.ActorType), true
	case "event.title":
		return event.Title, true
	case "event.summary":
		return event.Summary, true
	case "contact.marketing_opt_out":
		if contact == nil {
			return nil, false
		}
		return contact.MarketingOptOut, true
	}
	const prefix = "event.metadata."
	if strings.HasPrefix(strings.ToLower(field), prefix) {
		value, ok := event.Metadata[field[len(prefix):]]
		return value, ok
	}
	return nil, false
}

func automationTaskFromNode(
	node AutomationGraphNode,
	contact *models.Contact,
	event *models.CustomerActivityEvent,
	baseTime time.Time,
	delayMinutes int,
) (automationPlannedTask, error) {
	title, _ := automationConfigRawString(node.Config, "title")
	description, _ := automationConfigRawString(node.Config, "description")
	priorityText, _ := automationConfigNormalizedString(node.Config, "priority")
	if priorityText == "" {
		priorityText = string(models.FollowUpTaskPriorityNormal)
	}
	ownerMode, _ := automationConfigNormalizedString(node.Config, "owner")
	if ownerMode == "" {
		ownerMode = "unassigned"
	}
	var ownerUserID *uuid.UUID
	scheduledAt := baseTime.Add(time.Duration(delayMinutes) * time.Minute).UTC()
	var dueAt, remindAt *time.Time
	if minutes, ok := automationConfigInt(node.Config, "due_in_minutes"); ok {
		value := scheduledAt.Add(time.Duration(minutes) * time.Minute).UTC()
		dueAt = &value
	}
	if minutes, ok := automationConfigInt(node.Config, "remind_in_minutes"); ok {
		value := scheduledAt.Add(time.Duration(minutes) * time.Minute).UTC()
		remindAt = &value
	}
	if dueAt != nil && remindAt != nil && remindAt.After(*dueAt) {
		return automationPlannedTask{}, errors.New("task reminder cannot be after its due time")
	}
	renderedTitle := automationTruncateRunes(
		automationRenderTemplate(title, contact, event),
		255,
	)
	renderedDescription := automationTruncateRunes(
		automationRenderTemplate(description, contact, event),
		automationMaxDescriptionLength,
	)
	if renderedTitle == "" {
		return automationPlannedTask{}, errors.New("rendered task title is empty")
	}
	return automationPlannedTask{
		NodeID:      node.ID,
		Title:       renderedTitle,
		Description: renderedDescription,
		Priority:    models.FollowUpTaskPriority(priorityText),
		OwnerMode:   ownerMode,
		OwnerUserID: ownerUserID,
		ScheduledAt: scheduledAt,
		DueAt:       dueAt,
		RemindAt:    remindAt,
	}, nil
}

func automationTruncateRunes(value string, maximum int) string {
	if maximum <= 0 {
		return ""
	}
	runes := []rune(value)
	if len(runes) <= maximum {
		return value
	}
	return string(runes[:maximum])
}

func automationRenderTemplate(
	value string,
	contact *models.Contact,
	event *models.CustomerActivityEvent,
) string {
	eventType := string(event.EventType)
	eventTitle := strings.TrimSpace(event.Title)
	if eventTitle == "" {
		eventTitle = eventType
	}
	eventSummary := strings.TrimSpace(event.Summary)
	if eventSummary == "" {
		eventSummary = eventTitle
	}
	contactName := "Customer"
	if contact != nil {
		if value := strings.TrimSpace(contact.ProfileName); value != "" {
			contactName = value
		} else if value := strings.TrimSpace(contact.PhoneNumber); value != "" {
			contactName = value
		}
	}
	return strings.TrimSpace(strings.NewReplacer(
		"{{event.title}}", eventTitle,
		"{{event.summary}}", eventSummary,
		"{{event.type}}", eventType,
		"{{contact.name}}", contactName,
	).Replace(value))
}

func automationConfigRawString(config map[string]any, key string) (string, bool) {
	value, exists := config[key]
	if !exists {
		return "", false
	}
	text, ok := value.(string)
	if !ok {
		return "", false
	}
	return strings.TrimSpace(text), true
}

func automationConfigNormalizedString(config map[string]any, key string) (string, bool) {
	text, ok := automationConfigRawString(config, key)
	return strings.ToLower(text), ok
}

func automationConfigInt(config map[string]any, key string) (int, bool) {
	value, exists := config[key]
	if !exists {
		return 0, false
	}
	switch typed := value.(type) {
	case int:
		return typed, true
	case int32:
		return int(typed), true
	case int64:
		if typed > math.MaxInt || typed < math.MinInt {
			return 0, false
		}
		return int(typed), true
	case float64:
		if math.IsNaN(typed) || math.IsInf(typed, 0) || typed != math.Trunc(typed) ||
			typed > math.MaxInt || typed < math.MinInt {
			return 0, false
		}
		return int(typed), true
	case json.Number:
		parsed, err := strconv.ParseInt(string(typed), 10, 64)
		if err != nil || parsed > math.MaxInt || parsed < math.MinInt {
			return 0, false
		}
		return int(parsed), true
	default:
		return 0, false
	}
}

func automationIsScalar(value any) bool {
	switch typed := value.(type) {
	case nil, bool, string, json.Number:
		return true
	case float64:
		return !math.IsNaN(typed) && !math.IsInf(typed, 0)
	case int, int32, int64:
		return true
	default:
		return false
	}
}

func automationScalarEqual(left, right any) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	if reflect.TypeOf(left) != reflect.TypeOf(right) {
		return false
	}
	return reflect.DeepEqual(left, right)
}

func automationTemplateTokensValid(value string) bool {
	allowed := automationStringSet(automationAllowedTemplateTokens)
	for {
		start := strings.Index(value, "{{")
		if start < 0 {
			return !strings.Contains(value, "}}")
		}
		endOffset := strings.Index(value[start+2:], "}}")
		if endOffset < 0 {
			return false
		}
		end := start + 2 + endOffset + 2
		if !allowed[value[start:end]] {
			return false
		}
		value = value[end:]
	}
}

func automationStringSet(values []string) map[string]bool {
	set := make(map[string]bool, len(values))
	for _, value := range values {
		set[value] = true
	}
	return set
}

func automationUniqueSortedStrings(values []string) []string {
	set := map[string]bool{}
	for _, value := range values {
		if value != "" {
			set[value] = true
		}
	}
	values = values[:0]
	for value := range set {
		values = append(values, value)
	}
	sort.Strings(values)
	return values
}

func automationJSONBArrayStrings(values []string) models.JSONBArray {
	result := make(models.JSONBArray, 0, len(values))
	for _, value := range values {
		result = append(result, value)
	}
	return result
}

func automationJSONBArrayToStrings(values models.JSONBArray) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		if text, ok := value.(string); ok {
			result = append(result, text)
		}
	}
	return result
}
