package handlers

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/audit"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/valyala/fasthttp"
	"github.com/zerodha/fastglue"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const automationPolicyAuditResource = "automation_policy"

type CreateAutomationPolicyRequest struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Graph       AutomationGraph `json:"graph"`
}

type UpdateAutomationPolicyRequest struct {
	Version     int64            `json:"version"`
	Name        *string          `json:"name,omitempty"`
	Description *string          `json:"description,omitempty"`
	Graph       *AutomationGraph `json:"graph,omitempty"`
}

type AutomationPolicyVersionRequest struct {
	Version int64 `json:"version"`
}

type AutomationPreviewEvent struct {
	EventType  string                    `json:"event_type"`
	Category   string                    `json:"category,omitempty"`
	Title      string                    `json:"title,omitempty"`
	Summary    string                    `json:"summary,omitempty"`
	ActorType  string                    `json:"actor_type,omitempty"`
	SourceType string                    `json:"source_type,omitempty"`
	SourceID   *uuid.UUID                `json:"source_id,omitempty"`
	ContactID  *uuid.UUID                `json:"contact_id,omitempty"`
	Contact    *AutomationPreviewContact `json:"contact,omitempty"`
	Metadata   map[string]any            `json:"metadata,omitempty"`
	IngestedAt *time.Time                `json:"ingested_at,omitempty"`
}

// AutomationPreviewContact deliberately exposes only the single contact field
// that V1 conditions may inspect. It must not grow into an arbitrary contact
// impersonation surface.
type AutomationPreviewContact struct {
	MarketingOptOut bool `json:"marketing_opt_out"`
}

type PreviewAutomationPolicyRequest struct {
	Version         int64                   `json:"version,omitempty"`
	Graph           *AutomationGraph        `json:"graph,omitempty"`
	ActivityEventID *uuid.UUID              `json:"activity_event_id,omitempty"`
	Event           *AutomationPreviewEvent `json:"event,omitempty"`
}

type AutomationPolicyResponse struct {
	ID                  uuid.UUID                     `json:"id"`
	Name                string                        `json:"name"`
	Description         string                        `json:"description,omitempty"`
	Status              models.AutomationPolicyStatus `json:"status"`
	Version             int64                         `json:"version"`
	Graph               models.JSONB                  `json:"graph"`
	TriggerEventType    string                        `json:"trigger_event_type,omitempty"`
	TriggerEventTypes   []string                      `json:"trigger_event_types"`
	ActiveVersionID     *uuid.UUID                    `json:"active_version_id,omitempty"`
	ActiveVersionNumber int                           `json:"active_version_number,omitempty"`
	ActivatedAt         *time.Time                    `json:"activated_at,omitempty"`
	PausedAt            *time.Time                    `json:"paused_at,omitempty"`
	CreatedAt           time.Time                     `json:"created_at"`
	UpdatedAt           time.Time                     `json:"updated_at"`
	CreatedByID         *uuid.UUID                    `json:"created_by_id,omitempty"`
	UpdatedByID         *uuid.UUID                    `json:"updated_by_id,omitempty"`
	ValidationErrors    []AutomationValidationIssue   `json:"validation_errors,omitempty"`
	ValidationWarnings  []AutomationValidationIssue   `json:"validation_warnings,omitempty"`
}

type AutomationPreviewStepResponse struct {
	NodeID      string     `json:"node_id"`
	NodeType    string     `json:"node_type"`
	Status      string     `json:"status"`
	Branch      string     `json:"branch,omitempty"`
	ReasonCode  string     `json:"reason_code,omitempty"`
	ScheduledAt *time.Time `json:"scheduled_at,omitempty"`
	Detail      string     `json:"detail,omitempty"`
}

type AutomationPreviewActionResponse struct {
	NodeID      string                      `json:"node_id"`
	Type        string                      `json:"type"`
	Title       string                      `json:"title"`
	Description string                      `json:"description,omitempty"`
	Priority    models.FollowUpTaskPriority `json:"priority"`
	Owner       string                      `json:"owner"`
	ScheduledAt time.Time                   `json:"scheduled_at"`
	DueAt       *time.Time                  `json:"due_at,omitempty"`
	RemindAt    *time.Time                  `json:"remind_at,omitempty"`
}

type AutomationPolicyPreviewResponse struct {
	Valid             bool                              `json:"valid"`
	Version           int64                             `json:"version"`
	Checksum          string                            `json:"checksum,omitempty"`
	TriggerEventType  string                            `json:"trigger_event_type,omitempty"`
	TriggerEventTypes []string                          `json:"trigger_event_types"`
	Errors            []AutomationValidationIssue       `json:"errors"`
	Warnings          []AutomationValidationIssue       `json:"warnings"`
	Steps             []AutomationPreviewStepResponse   `json:"steps"`
	Actions           []AutomationPreviewActionResponse `json:"actions"`
}

type AutomationExecutionResponse struct {
	ID                  uuid.UUID                         `json:"id"`
	PolicyID            uuid.UUID                         `json:"policy_id"`
	PolicyVersionID     uuid.UUID                         `json:"policy_version_id"`
	PolicyVersionNumber int                               `json:"policy_version_number"`
	ActivityEventID     uuid.UUID                         `json:"activity_event_id"`
	ContactID           uuid.UUID                         `json:"contact_id"`
	Status              models.AutomationExecutionStatus  `json:"status"`
	TriggeredAt         time.Time                         `json:"triggered_at"`
	StartedAt           *time.Time                        `json:"started_at,omitempty"`
	CompletedAt         *time.Time                        `json:"completed_at,omitempty"`
	HasError            bool                              `json:"has_error,omitempty"`
	Steps               []AutomationExecutionStepResponse `json:"steps"`
}

type AutomationExecutionStepResponse struct {
	ID          uuid.UUID                            `json:"id"`
	NodeID      string                               `json:"node_id"`
	NodeType    string                               `json:"node_type"`
	Status      models.AutomationExecutionStepStatus `json:"status"`
	ScheduledAt *time.Time                           `json:"scheduled_at,omitempty"`
	StartedAt   *time.Time                           `json:"started_at,omitempty"`
	CompletedAt *time.Time                           `json:"completed_at,omitempty"`
	TaskID      *uuid.UUID                           `json:"task_id,omitempty"`
	ReasonCode  string                               `json:"reason_code,omitempty"`
	Branch      string                               `json:"branch,omitempty"`
	HasError    bool                                 `json:"has_error,omitempty"`
}

func (a *App) GetAutomationPolicyCatalog(r *fastglue.Request) error {
	if _, _, err := a.requireAuth(r, models.ResourceCRMAutomations, models.ActionRead); err != nil {
		return nil
	}
	return r.SendEnvelope(map[string]any{
		"event_types": automationAllowedEventTypes,
		"node_types": []string{
			automationNodeTrigger,
			automationNodeCondition,
			automationNodeDelay,
			automationNodeCreateTask,
		},
		"condition_fields":    automationAllowedConditionFields,
		"condition_operators": automationAllowedConditionOperators,
		"branches":            []string{automationBranchTrue, automationBranchFalse},
		"task_priorities": []models.FollowUpTaskPriority{
			models.FollowUpTaskPriorityLow,
			models.FollowUpTaskPriorityNormal,
			models.FollowUpTaskPriorityHigh,
			models.FollowUpTaskPriorityUrgent,
		},
		"task_owner_modes": []string{"unassigned", "contact_owner"},
		"limits": map[string]int{
			"request_bytes":         automationMaxRequestBytes,
			"graph_bytes":           automationMaxGraphBytes,
			"nodes":                 automationMaxNodes,
			"edges":                 automationMaxEdges,
			"actions":               automationMaxActionNodes,
			"delay_minutes":         automationMaxDelayMinutes,
			"task_schedule_minutes": automationMaxTaskMinutes,
		},
	})
}

func (a *App) ListAutomationPolicies(r *fastglue.Request) error {
	orgID, _, err := a.requireAuth(r, models.ResourceCRMAutomations, models.ActionRead)
	if err != nil {
		return nil
	}
	query := a.DB.Model(&models.AutomationPolicy{}).
		Where("organization_id = ?", orgID)
	if status := strings.TrimSpace(string(r.RequestCtx.QueryArgs().Peek("status"))); status != "" {
		query = query.Where("status = ?", status)
	}
	if search := strings.TrimSpace(string(r.RequestCtx.QueryArgs().Peek("search"))); search != "" {
		if len(search) > automationMaxPolicyNameLength {
			return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "search is too long", nil, "")
		}
		query = query.Where("name ILIKE ?", "%"+strings.ReplaceAll(search, "%", "\\%")+"%")
	}
	page, limit, parseErr := automationPageLimit(r)
	if parseErr != nil {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, parseErr.Error(), nil, "")
	}
	var total int64
	if err := query.Count(&total).Error; err != nil {
		a.Log.Error("Failed to count automation policies", "error", err)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to list automation policies", nil, "")
	}
	var policies []models.AutomationPolicy
	if err := query.Order("updated_at DESC, id").
		Offset((page - 1) * limit).
		Limit(limit).
		Find(&policies).Error; err != nil {
		a.Log.Error("Failed to list automation policies", "error", err)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to list automation policies", nil, "")
	}
	response := make([]AutomationPolicyResponse, len(policies))
	for i := range policies {
		response[i] = automationPolicyToResponse(&policies[i])
	}
	return r.SendEnvelope(map[string]any{
		"automation_policies": response,
		"total":               total,
		"page":                page,
		"limit":               limit,
	})
}

func (a *App) GetAutomationPolicy(r *fastglue.Request) error {
	orgID, _, err := a.requireAuth(r, models.ResourceCRMAutomations, models.ActionRead)
	if err != nil {
		return nil
	}
	policyID, err := parsePathUUID(r, "id", "automation policy")
	if err != nil {
		return nil
	}
	var policy models.AutomationPolicy
	if err := a.DB.Where("id = ? AND organization_id = ?", policyID, orgID).
		First(&policy).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return r.SendErrorEnvelope(fasthttp.StatusNotFound, "Automation policy not found", nil, "")
		}
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to load automation policy", nil, "")
	}
	response := automationPolicyToResponse(&policy)
	if graph, graphErr := automationGraphFromJSONB(policy.DraftGraph); graphErr == nil {
		validation := validateAutomationGraph(graph, true)
		response.ValidationErrors = validation.Errors
		response.ValidationWarnings = validation.Warnings
	}
	return r.SendEnvelope(response)
}

func (a *App) CreateAutomationPolicy(r *fastglue.Request) error {
	orgID, userID, err := a.requireAuth(r, models.ResourceCRMAutomations, models.ActionWrite)
	if err != nil {
		return nil
	}
	if !automationRequestSizeAllowed(r) {
		return r.SendErrorEnvelope(fasthttp.StatusRequestEntityTooLarge, "Automation request is too large", nil, "")
	}
	var req CreateAutomationPolicyRequest
	if err := a.decodeRequest(r, &req); err != nil {
		return nil
	}
	req.Name = strings.TrimSpace(req.Name)
	req.Description = strings.TrimSpace(req.Description)
	if err := validateAutomationPolicyText(req.Name, req.Description); err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, err.Error(), nil, "")
	}
	graphValue, err := automationGraphToJSONB(req.Graph)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, err.Error(), nil, "")
	}
	validation := validateAutomationGraph(req.Graph, false)
	if issue := automationDraftBlockingIssue(validation.Errors); issue != nil {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, issue.Message, nil, "")
	}
	policy := models.AutomationPolicy{
		BaseModel:         models.BaseModel{ID: uuid.New()},
		OrganizationID:    orgID,
		Name:              req.Name,
		Description:       req.Description,
		Status:            models.AutomationPolicyStatusDraft,
		DraftGraph:        graphValue,
		TriggerEventTypes: automationJSONBArrayStrings(validation.TriggerEventTypes),
		Version:           1,
		CreatedByID:       &userID,
		UpdatedByID:       &userID,
	}
	err = a.DB.Transaction(func(tx *gorm.DB) error {
		if err := automationEnsureUniquePolicyName(tx, orgID, policy.Name, uuid.Nil); err != nil {
			return err
		}
		if err := tx.Create(&policy).Error; err != nil {
			return err
		}
		return audit.LogAudit(
			tx, orgID, userID, audit.GetUserName(tx, userID),
			automationPolicyAuditResource, policy.ID, models.AuditActionCreated,
			nil, automationPolicyAuditSnapshot(&policy),
		)
	})
	if err != nil {
		return a.sendProductCRMWriteError(r, "create automation policy", err)
	}
	return r.SendEnvelope(automationPolicyToResponse(&policy))
}

func (a *App) UpdateAutomationPolicy(r *fastglue.Request) error {
	orgID, userID, err := a.requireAuth(r, models.ResourceCRMAutomations, models.ActionWrite)
	if err != nil {
		return nil
	}
	if !automationRequestSizeAllowed(r) {
		return r.SendErrorEnvelope(fasthttp.StatusRequestEntityTooLarge, "Automation request is too large", nil, "")
	}
	policyID, err := parsePathUUID(r, "id", "automation policy")
	if err != nil {
		return nil
	}
	var req UpdateAutomationPolicyRequest
	if err := a.decodeRequest(r, &req); err != nil {
		return nil
	}
	if req.Version < 1 {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "version must be at least 1", nil, "")
	}
	var graphValue models.JSONB
	var graphValidation automationGraphValidation
	if req.Graph != nil {
		graphValue, err = automationGraphToJSONB(*req.Graph)
		if err != nil {
			return r.SendErrorEnvelope(fasthttp.StatusBadRequest, err.Error(), nil, "")
		}
		graphValidation = validateAutomationGraph(*req.Graph, false)
		if issue := automationDraftBlockingIssue(graphValidation.Errors); issue != nil {
			return r.SendErrorEnvelope(fasthttp.StatusBadRequest, issue.Message, nil, "")
		}
	}
	var updated models.AutomationPolicy
	err = a.DB.Transaction(func(tx *gorm.DB) error {
		var policy models.AutomationPolicy
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ? AND organization_id = ?", policyID, orgID).
			First(&policy).Error; err != nil {
			return newProductCRMClientError(fasthttp.StatusNotFound, "Automation policy not found")
		}
		if policy.Version != req.Version {
			return newProductCRMClientError(fasthttp.StatusConflict, "Automation policy was modified; refresh and retry")
		}
		if policy.Status == models.AutomationPolicyStatusArchived {
			return newProductCRMClientError(fasthttp.StatusConflict, "Archived automation policies cannot be edited")
		}
		name := policy.Name
		description := policy.Description
		if req.Name != nil {
			name = strings.TrimSpace(*req.Name)
		}
		if req.Description != nil {
			description = strings.TrimSpace(*req.Description)
		}
		if err := validateAutomationPolicyText(name, description); err != nil {
			return newProductCRMClientError(fasthttp.StatusBadRequest, err.Error())
		}
		if err := automationEnsureUniquePolicyName(tx, orgID, name, policy.ID); err != nil {
			return err
		}
		oldSnapshot := automationPolicyAuditSnapshot(&policy)
		updates := map[string]any{
			"name":          name,
			"description":   description,
			"updated_by_id": userID,
			"version":       gorm.Expr("version + 1"),
		}
		if req.Graph != nil {
			updates["draft_graph"] = graphValue
			updates["trigger_event_types"] = automationJSONBArrayStrings(graphValidation.TriggerEventTypes)
		}
		result := tx.Model(&models.AutomationPolicy{}).
			Where("id = ? AND organization_id = ? AND version = ?", policy.ID, orgID, req.Version).
			Updates(updates)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return newProductCRMClientError(fasthttp.StatusConflict, "Automation policy was modified; refresh and retry")
		}
		if err := tx.Where("id = ? AND organization_id = ?", policy.ID, orgID).
			First(&updated).Error; err != nil {
			return err
		}
		return audit.LogAudit(
			tx, orgID, userID, audit.GetUserName(tx, userID),
			automationPolicyAuditResource, policy.ID, models.AuditActionUpdated,
			oldSnapshot, automationPolicyAuditSnapshot(&updated),
		)
	})
	if err != nil {
		return a.sendProductCRMWriteError(r, "update automation policy", err)
	}
	return r.SendEnvelope(automationPolicyToResponse(&updated))
}

func (a *App) ActivateAutomationPolicy(r *fastglue.Request) error {
	orgID, userID, err := a.requireAuth(r, models.ResourceCRMAutomations, models.ActionExecute)
	if err != nil {
		return nil
	}
	if !a.HasPermission(userID, models.ResourceTasks, models.ActionWrite, orgID) {
		return r.SendErrorEnvelope(fasthttp.StatusForbidden, "Task write permission is required to activate automation", nil, "")
	}
	policyID, err := parsePathUUID(r, "id", "automation policy")
	if err != nil {
		return nil
	}
	var req AutomationPolicyVersionRequest
	if err := a.decodeRequest(r, &req); err != nil {
		return nil
	}
	if req.Version < 1 {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "version must be at least 1", nil, "")
	}
	var activated models.AutomationPolicy
	var activationWarnings []AutomationValidationIssue
	err = a.DB.Transaction(func(tx *gorm.DB) error {
		if err := automationLockDispatchState(tx, orgID); err != nil {
			return err
		}
		var policy models.AutomationPolicy
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ? AND organization_id = ?", policyID, orgID).
			First(&policy).Error; err != nil {
			return newProductCRMClientError(fasthttp.StatusNotFound, "Automation policy not found")
		}
		if policy.Version != req.Version {
			return newProductCRMClientError(fasthttp.StatusConflict, "Automation policy was modified; refresh and retry")
		}
		if policy.Status == models.AutomationPolicyStatusArchived {
			return newProductCRMClientError(fasthttp.StatusConflict, "Archived automation policies cannot be activated")
		}
		graph, err := automationGraphFromJSONB(policy.DraftGraph)
		if err != nil {
			return newProductCRMClientError(fasthttp.StatusBadRequest, "Automation graph is malformed")
		}
		validation := validateAutomationGraph(graph, true)
		if len(validation.Errors) > 0 {
			return newProductCRMClientError(fasthttp.StatusBadRequest, validation.Errors[0].Message)
		}
		if err := automationValidateOwnerUsers(tx, orgID, validation.OwnerUserIDs); err != nil {
			return err
		}
		checksum, err := automationGraphChecksum(graph)
		if err != nil {
			return err
		}
		semanticFingerprint, err := automationGraphSemanticFingerprint(graph)
		if err != nil {
			return err
		}
		activationWarnings, err = automationActivePolicyConflictWarnings(
			tx,
			orgID,
			policy.ID,
			validation.TriggerEventTypes,
			semanticFingerprint,
			true,
		)
		if err != nil {
			return err
		}
		dbNow, err := automationDatabaseNow(tx)
		if err != nil {
			return err
		}
		var version models.AutomationPolicyVersion
		reuseVersion := false
		if policy.ActiveVersionID != nil {
			if err := tx.Where(
				"id = ? AND policy_id = ? AND organization_id = ?",
				*policy.ActiveVersionID, policy.ID, orgID,
			).First(&version).Error; err != nil {
				return err
			}
			reuseVersion = version.Checksum == checksum
		}
		if policy.Status == models.AutomationPolicyStatusActive && reuseVersion {
			activated = policy
			return nil
		}
		closed, err := automationCloseOpenActivation(tx, orgID, policy.ID, userID, dbNow)
		if err != nil {
			return err
		}
		expectedClosed := int64(0)
		if policy.Status == models.AutomationPolicyStatusActive {
			expectedClosed = 1
		}
		if closed != expectedClosed {
			return fmt.Errorf(
				"automation activation interval invariant failed: closed %d, expected %d",
				closed,
				expectedClosed,
			)
		}
		if !reuseVersion {
			var maxNumber int
			if err := tx.Model(&models.AutomationPolicyVersion{}).
				Where("organization_id = ? AND policy_id = ?", orgID, policy.ID).
				Select("COALESCE(MAX(number), 0)").Scan(&maxNumber).Error; err != nil {
				return err
			}
			version = models.AutomationPolicyVersion{
				ID:                uuid.New(),
				OrganizationID:    orgID,
				PolicyID:          policy.ID,
				Number:            maxNumber + 1,
				TriggerEventTypes: automationJSONBArrayStrings(validation.TriggerEventTypes),
				Graph:             policy.DraftGraph,
				Checksum:          checksum,
				CreatedByID:       &userID,
				PublishedAt:       dbNow,
			}
			if err := tx.Create(&version).Error; err != nil {
				return err
			}
		}
		activation := models.AutomationPolicyActivation{
			ID:                  uuid.New(),
			OrganizationID:      orgID,
			PolicyID:            policy.ID,
			PolicyVersionID:     version.ID,
			PolicyVersionNumber: version.Number,
			ActiveFrom:          dbNow,
			CreatedByID:         &userID,
		}
		if err := tx.Create(&activation).Error; err != nil {
			return err
		}
		oldSnapshot := automationPolicyAuditSnapshot(&policy)
		result := tx.Model(&models.AutomationPolicy{}).
			Where("id = ? AND organization_id = ? AND version = ?", policy.ID, orgID, req.Version).
			Updates(map[string]any{
				"status":                models.AutomationPolicyStatusActive,
				"active_version_id":     version.ID,
				"active_version_number": version.Number,
				"trigger_event_types":   automationJSONBArrayStrings(validation.TriggerEventTypes),
				"activated_at":          dbNow,
				"paused_at":             nil,
				"updated_by_id":         userID,
				"version":               gorm.Expr("version + 1"),
			})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return newProductCRMClientError(fasthttp.StatusConflict, "Automation policy was modified; refresh and retry")
		}
		if err := tx.Where("id = ? AND organization_id = ?", policy.ID, orgID).
			First(&activated).Error; err != nil {
			return err
		}
		return audit.LogAudit(
			tx, orgID, userID, audit.GetUserName(tx, userID),
			automationPolicyAuditResource, policy.ID, models.AuditActionUpdated,
			oldSnapshot, automationPolicyAuditSnapshot(&activated),
			map[string]any{"field": "lifecycle", "new_value": "active", "version_id": version.ID},
		)
	})
	if err != nil {
		return a.sendProductCRMWriteError(r, "activate automation policy", err)
	}
	response := automationPolicyToResponse(&activated)
	response.ValidationWarnings = activationWarnings
	return r.SendEnvelope(response)
}

func (a *App) PauseAutomationPolicy(r *fastglue.Request) error {
	return a.transitionAutomationPolicyToPaused(r)
}

func (a *App) transitionAutomationPolicyToPaused(r *fastglue.Request) error {
	orgID, userID, err := a.requireAuth(r, models.ResourceCRMAutomations, models.ActionExecute)
	if err != nil {
		return nil
	}
	policyID, err := parsePathUUID(r, "id", "automation policy")
	if err != nil {
		return nil
	}
	var req AutomationPolicyVersionRequest
	if err := a.decodeRequest(r, &req); err != nil {
		return nil
	}
	if req.Version < 1 {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "version must be at least 1", nil, "")
	}
	var paused models.AutomationPolicy
	err = a.DB.Transaction(func(tx *gorm.DB) error {
		if err := automationLockDispatchState(tx, orgID); err != nil {
			return err
		}
		var policy models.AutomationPolicy
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ? AND organization_id = ?", policyID, orgID).
			First(&policy).Error; err != nil {
			return newProductCRMClientError(fasthttp.StatusNotFound, "Automation policy not found")
		}
		if policy.Version != req.Version {
			return newProductCRMClientError(fasthttp.StatusConflict, "Automation policy was modified; refresh and retry")
		}
		if policy.Status != models.AutomationPolicyStatusActive {
			return newProductCRMClientError(fasthttp.StatusConflict, "Only active automation policies can be paused")
		}
		dbNow, err := automationDatabaseNow(tx)
		if err != nil {
			return err
		}
		closed, err := automationCloseOpenActivation(tx, orgID, policy.ID, userID, dbNow)
		if err != nil {
			return err
		}
		if closed != 1 {
			return errors.New("active automation policy must have exactly one open activation interval")
		}
		oldSnapshot := automationPolicyAuditSnapshot(&policy)
		result := tx.Model(&models.AutomationPolicy{}).
			Where("id = ? AND organization_id = ? AND version = ?", policy.ID, orgID, req.Version).
			Updates(map[string]any{
				"status":        models.AutomationPolicyStatusPaused,
				"paused_at":     dbNow,
				"updated_by_id": userID,
				"version":       gorm.Expr("version + 1"),
			})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return newProductCRMClientError(fasthttp.StatusConflict, "Automation policy was modified; refresh and retry")
		}
		if err := tx.Where("id = ? AND organization_id = ?", policy.ID, orgID).
			First(&paused).Error; err != nil {
			return err
		}
		return audit.LogAudit(
			tx, orgID, userID, audit.GetUserName(tx, userID),
			automationPolicyAuditResource, policy.ID, models.AuditActionUpdated,
			oldSnapshot, automationPolicyAuditSnapshot(&paused),
			map[string]any{"field": "lifecycle", "new_value": "paused"},
		)
	})
	if err != nil {
		return a.sendProductCRMWriteError(r, "pause automation policy", err)
	}
	return r.SendEnvelope(automationPolicyToResponse(&paused))
}

func (a *App) DeleteAutomationPolicy(r *fastglue.Request) error {
	orgID, userID, err := a.requireAuth(r, models.ResourceCRMAutomations, models.ActionDelete)
	if err != nil {
		return nil
	}
	policyID, err := parsePathUUID(r, "id", "automation policy")
	if err != nil {
		return nil
	}
	var req AutomationPolicyVersionRequest
	if err := a.decodeRequest(r, &req); err != nil {
		return nil
	}
	if req.Version < 1 {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "version must be at least 1", nil, "")
	}
	err = a.DB.Transaction(func(tx *gorm.DB) error {
		if err := automationLockDispatchState(tx, orgID); err != nil {
			return err
		}
		var policy models.AutomationPolicy
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ? AND organization_id = ?", policyID, orgID).
			First(&policy).Error; err != nil {
			return newProductCRMClientError(fasthttp.StatusNotFound, "Automation policy not found")
		}
		if policy.Version != req.Version {
			return newProductCRMClientError(fasthttp.StatusConflict, "Automation policy was modified; refresh and retry")
		}
		if policy.Status != models.AutomationPolicyStatusDraft || policy.ActiveVersionID != nil {
			return newProductCRMClientError(
				fasthttp.StatusConflict,
				"Only unpublished draft automation policies can be deleted",
			)
		}
		var publishedVersions int64
		if err := tx.Model(&models.AutomationPolicyVersion{}).
			Where("organization_id = ? AND policy_id = ?", orgID, policy.ID).
			Count(&publishedVersions).Error; err != nil {
			return err
		}
		if publishedVersions != 0 {
			return newProductCRMClientError(
				fasthttp.StatusConflict,
				"Published automation policies cannot be deleted",
			)
		}
		dbNow, err := automationDatabaseNow(tx)
		if err != nil {
			return err
		}
		oldSnapshot := automationPolicyAuditSnapshot(&policy)
		result := tx.Model(&models.AutomationPolicy{}).
			Where("id = ? AND organization_id = ? AND version = ?", policy.ID, orgID, req.Version).
			Updates(map[string]any{
				"status":        models.AutomationPolicyStatusArchived,
				"paused_at":     dbNow,
				"updated_by_id": userID,
				"version":       gorm.Expr("version + 1"),
				"deleted_at":    dbNow,
			})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return newProductCRMClientError(
				fasthttp.StatusConflict,
				"Automation policy was modified; refresh and retry",
			)
		}
		return audit.LogAudit(
			tx, orgID, userID, audit.GetUserName(tx, userID),
			automationPolicyAuditResource, policy.ID, models.AuditActionDeleted,
			oldSnapshot, map[string]any{"status": models.AutomationPolicyStatusArchived},
		)
	})
	if err != nil {
		return a.sendProductCRMWriteError(r, "archive automation policy", err)
	}
	return r.SendEnvelope(map[string]any{"success": true})
}

func (a *App) ListAutomationPolicyVersions(r *fastglue.Request) error {
	orgID, _, err := a.requireAuth(r, models.ResourceCRMAutomations, models.ActionRead)
	if err != nil {
		return nil
	}
	policyID, err := parsePathUUID(r, "id", "automation policy")
	if err != nil {
		return nil
	}
	if err := automationRequirePolicyExists(a.DB, orgID, policyID); err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return r.SendErrorEnvelope(fasthttp.StatusNotFound, "Automation policy not found", nil, "")
		}
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to load automation policy", nil, "")
	}
	var versions []models.AutomationPolicyVersion
	if err := a.DB.Where("organization_id = ? AND policy_id = ?", orgID, policyID).
		Order("number DESC").Find(&versions).Error; err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to list automation versions", nil, "")
	}
	return r.SendEnvelope(map[string]any{"versions": versions})
}

func (a *App) ListAutomationPolicyExecutions(r *fastglue.Request) error {
	orgID, userID, err := a.requireAuth(r, models.ResourceCRMAutomations, models.ActionRead)
	if err != nil {
		return nil
	}
	if !a.HasPermission(userID, models.ResourceContacts, models.ActionRead, orgID) ||
		!a.HasPermission(userID, models.ResourceTasks, models.ActionRead, orgID) {
		return r.SendErrorEnvelope(
			fasthttp.StatusForbidden,
			"Contact and task read permissions are required to view automation executions",
			nil,
			"",
		)
	}
	policyID, err := parsePathUUID(r, "id", "automation policy")
	if err != nil {
		return nil
	}
	if err := automationRequirePolicyExists(a.DB, orgID, policyID); err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return r.SendErrorEnvelope(fasthttp.StatusNotFound, "Automation policy not found", nil, "")
		}
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to load automation policy", nil, "")
	}
	var executions []models.AutomationExecution
	limit, parseErr := automationLimit(r, 50, 100)
	if parseErr != nil {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, parseErr.Error(), nil, "")
	}
	if err := a.DB.Where("organization_id = ? AND policy_id = ?", orgID, policyID).
		Order("triggered_at DESC, id DESC").Limit(limit).Find(&executions).Error; err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to list automation executions", nil, "")
	}
	responses := make([]AutomationExecutionResponse, len(executions))
	executionIDs := make([]uuid.UUID, len(executions))
	for i := range executions {
		executionIDs[i] = executions[i].ID
		responses[i] = AutomationExecutionResponse{
			ID:                  executions[i].ID,
			PolicyID:            executions[i].PolicyID,
			PolicyVersionID:     executions[i].PolicyVersionID,
			PolicyVersionNumber: executions[i].PolicyVersionNumber,
			ActivityEventID:     executions[i].ActivityEventID,
			ContactID:           executions[i].ContactID,
			Status:              executions[i].Status,
			TriggeredAt:         executions[i].TriggeredAt,
			StartedAt:           executions[i].StartedAt,
			CompletedAt:         executions[i].CompletedAt,
			HasError:            strings.TrimSpace(executions[i].LastError) != "",
			Steps:               []AutomationExecutionStepResponse{},
		}
	}
	var steps []models.AutomationExecutionStep
	if len(executionIDs) > 0 {
		if err := a.DB.Where(
			"organization_id = ? AND execution_id IN ?", orgID, executionIDs,
		).Order("created_at, node_id").Find(&steps).Error; err != nil {
			return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to list automation execution steps", nil, "")
		}
	}
	responseIndex := make(map[uuid.UUID]int, len(responses))
	for index := range responses {
		responseIndex[responses[index].ID] = index
	}
	for stepIndex := range steps {
		index, ok := responseIndex[steps[stepIndex].ExecutionID]
		if !ok {
			continue
		}
		reasonCode := careJSONString(steps[stepIndex].Output, "stop_reason")
		if reasonCode == "" {
			reasonCode = careJSONString(steps[stepIndex].Output, "reason_code")
		}
		branch := ""
		switch reasonCode {
		case "condition_true":
			branch = automationBranchTrue
		case "condition_false":
			branch = automationBranchFalse
		}
		responses[index].Steps = append(responses[index].Steps, AutomationExecutionStepResponse{
			ID:          steps[stepIndex].ID,
			NodeID:      steps[stepIndex].NodeID,
			NodeType:    steps[stepIndex].NodeType,
			Status:      steps[stepIndex].Status,
			ScheduledAt: steps[stepIndex].ScheduledAt,
			StartedAt:   steps[stepIndex].StartedAt,
			CompletedAt: steps[stepIndex].CompletedAt,
			TaskID:      steps[stepIndex].TaskID,
			ReasonCode:  reasonCode,
			Branch:      branch,
			HasError:    strings.TrimSpace(steps[stepIndex].LastError) != "",
		})
	}
	return r.SendEnvelope(map[string]any{"executions": responses})
}

func (a *App) PreviewAutomationPolicy(r *fastglue.Request) error {
	orgID, _, err := a.requireAuth(r, models.ResourceCRMAutomations, models.ActionExecute)
	if err != nil {
		return nil
	}
	if !automationRequestSizeAllowed(r) {
		return r.SendErrorEnvelope(fasthttp.StatusRequestEntityTooLarge, "Automation request is too large", nil, "")
	}
	policyID, err := parsePathUUID(r, "id", "automation policy")
	if err != nil {
		return nil
	}
	var req PreviewAutomationPolicyRequest
	if err := a.decodeRequest(r, &req); err != nil {
		return nil
	}
	if req.Version < 1 {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "version must be at least 1", nil, "")
	}
	if req.ActivityEventID != nil || (req.Event != nil && req.Event.ContactID != nil) {
		return r.SendErrorEnvelope(
			fasthttp.StatusBadRequest,
			"V1 automation preview accepts synthetic event data only",
			nil,
			"",
		)
	}
	var policy models.AutomationPolicy
	if err := a.DB.Where("id = ? AND organization_id = ?", policyID, orgID).
		First(&policy).Error; err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusNotFound, "Automation policy not found", nil, "")
	}
	if req.Version != policy.Version {
		return r.SendErrorEnvelope(
			fasthttp.StatusConflict,
			"Automation policy was modified; refresh and retry",
			nil,
			"",
		)
	}
	var graph AutomationGraph
	if req.Graph != nil {
		graph = *req.Graph
	} else {
		graph, err = automationGraphFromJSONB(policy.DraftGraph)
		if err != nil {
			return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Automation graph is malformed", nil, "")
		}
	}
	validation := validateAutomationGraph(graph, true)
	checksum, checksumErr := automationGraphChecksum(graph)
	if checksumErr != nil {
		validation.Errors = append(validation.Errors, AutomationValidationIssue{
			Code: "checksum_failed", Message: "graph could not be checksummed",
		})
	}
	response := AutomationPolicyPreviewResponse{
		Valid:             len(validation.Errors) == 0,
		Version:           req.Version,
		Checksum:          checksum,
		TriggerEventTypes: validation.TriggerEventTypes,
		Errors:            validation.Errors,
		Warnings:          validation.Warnings,
		Steps:             []AutomationPreviewStepResponse{},
		Actions:           []AutomationPreviewActionResponse{},
	}
	if len(response.TriggerEventTypes) > 0 {
		response.TriggerEventType = response.TriggerEventTypes[0]
	}
	if !response.Valid {
		return r.SendEnvelope(response)
	}
	semanticFingerprint, fingerprintErr := automationGraphSemanticFingerprint(graph)
	if fingerprintErr != nil {
		response.Valid = false
		response.Errors = append(response.Errors, AutomationValidationIssue{
			Code:    "semantic_fingerprint_failed",
			Message: "graph behavior could not be fingerprinted",
		})
		return r.SendEnvelope(response)
	}
	conflictWarnings, conflictErr := automationActivePolicyConflictWarnings(
		a.DB,
		orgID,
		policy.ID,
		validation.TriggerEventTypes,
		semanticFingerprint,
		false,
	)
	if conflictErr != nil {
		return r.SendErrorEnvelope(
			fasthttp.StatusInternalServerError,
			"Failed to check active automation overlaps",
			nil,
			"",
		)
	}
	response.Warnings = append(response.Warnings, conflictWarnings...)
	if err := automationValidateOwnerUsers(a.DB, orgID, validation.OwnerUserIDs); err != nil {
		var clientErr *productCRMClientError
		if errors.As(err, &clientErr) {
			return r.SendErrorEnvelope(clientErr.status, clientErr.message, nil, "")
		}
		return r.SendErrorEnvelope(
			fasthttp.StatusInternalServerError,
			"Failed to validate automation owners",
			nil,
			"",
		)
	}

	event, contact, baseTime, contextWarning, loadErr := a.automationPreviewContext(req)
	if loadErr != nil {
		var clientErr *productCRMClientError
		if errors.As(loadErr, &clientErr) {
			return r.SendErrorEnvelope(clientErr.status, clientErr.message, nil, "")
		}
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to prepare automation preview", nil, "")
	}
	if contextWarning != nil {
		response.Warnings = append(response.Warnings, *contextWarning)
	}
	if event == nil {
		event = &models.CustomerActivityEvent{
			EventType: models.CustomerActivityEventType(response.TriggerEventTypes[0]),
			Category:  models.CustomerActivityCategoryContact,
			ActorType: models.CustomerActivityActorSystem,
			Title:     "Synthetic preview event",
			Metadata:  models.JSONB{},
		}
		response.Warnings = append(response.Warnings, AutomationValidationIssue{
			Code: "synthetic_event", Message: "Preview used a synthetic event and did not write any records",
		})
	}
	evaluation, err := evaluateAutomationGraph(graph, event, contact, baseTime)
	if err != nil {
		response.Valid = false
		response.Errors = append(response.Errors, AutomationValidationIssue{
			Code: "evaluation_failed", Message: err.Error(),
		})
		return r.SendEnvelope(response)
	}
	for _, step := range evaluation.Steps {
		reasonCode := careJSONString(step.Output, "stop_reason")
		if reasonCode == "" {
			reasonCode = careJSONString(step.Output, "reason_code")
		}
		response.Steps = append(response.Steps, AutomationPreviewStepResponse{
			NodeID: step.NodeID, NodeType: step.NodeType, Status: string(step.Status),
			Branch: step.Branch, ReasonCode: reasonCode,
			ScheduledAt: step.ScheduledAt, Detail: step.Detail,
		})
	}
	for _, action := range evaluation.Actions {
		response.Actions = append(response.Actions, AutomationPreviewActionResponse{
			NodeID: action.NodeID, Type: automationNodeCreateTask,
			Title: action.Title, Description: action.Description,
			Priority: action.Priority, Owner: action.OwnerMode,
			ScheduledAt: action.ScheduledAt, DueAt: action.DueAt, RemindAt: action.RemindAt,
		})
	}
	return r.SendEnvelope(response)
}

func (a *App) automationPreviewContext(
	req PreviewAutomationPolicyRequest,
) (*models.CustomerActivityEvent, *models.Contact, time.Time, *AutomationValidationIssue, error) {
	if req.Event == nil {
		return nil, nil, time.Now().UTC(), nil, nil
	}
	input := req.Event
	if !automationStringSet(automationAllowedEventTypes)[strings.TrimSpace(input.EventType)] {
		return nil, nil, time.Time{}, nil, newProductCRMClientError(fasthttp.StatusBadRequest, "Preview event type is not supported")
	}
	event := &models.CustomerActivityEvent{
		EventType:        models.CustomerActivityEventType(strings.TrimSpace(input.EventType)),
		Category:         models.CustomerActivityCategory(strings.TrimSpace(input.Category)),
		Title:            strings.TrimSpace(input.Title),
		Summary:          strings.TrimSpace(input.Summary),
		ActorType:        models.CustomerActivityActorType(strings.TrimSpace(input.ActorType)),
		SourceObjectType: strings.TrimSpace(input.SourceType),
		SourceObjectID:   input.SourceID,
		Metadata:         models.JSONB(input.Metadata),
	}
	if event.Category == "" {
		event.Category = models.CustomerActivityCategoryContact
	}
	if event.ActorType == "" {
		event.ActorType = models.CustomerActivityActorSystem
	}
	var contact *models.Contact
	if input.Contact != nil {
		contact = &models.Contact{
			MarketingOptOut: input.Contact.MarketingOptOut,
		}
	}
	baseTime := time.Now().UTC()
	if input.IngestedAt != nil {
		baseTime = input.IngestedAt.UTC()
	}
	return event, contact, baseTime, nil, nil
}

func automationPolicyToResponse(policy *models.AutomationPolicy) AutomationPolicyResponse {
	triggerTypes := automationJSONBArrayToStrings(policy.TriggerEventTypes)
	response := AutomationPolicyResponse{
		ID: policy.ID, Name: policy.Name, Description: policy.Description,
		Status: policy.Status, Version: policy.Version, Graph: policy.DraftGraph,
		TriggerEventTypes: triggerTypes, ActiveVersionID: policy.ActiveVersionID,
		ActiveVersionNumber: policy.ActiveVersionNumber,
		ActivatedAt:         policy.ActivatedAt, PausedAt: policy.PausedAt,
		CreatedAt: policy.CreatedAt, UpdatedAt: policy.UpdatedAt,
		CreatedByID: policy.CreatedByID, UpdatedByID: policy.UpdatedByID,
	}
	if len(triggerTypes) > 0 {
		response.TriggerEventType = triggerTypes[0]
	}
	return response
}

func validateAutomationPolicyText(name, description string) error {
	if name == "" || len(name) > automationMaxPolicyNameLength {
		return fmt.Errorf("name is required and cannot exceed %d characters", automationMaxPolicyNameLength)
	}
	if len(description) > automationMaxDescriptionLength {
		return fmt.Errorf("description cannot exceed %d characters", automationMaxDescriptionLength)
	}
	return nil
}

func automationRequestSizeAllowed(r *fastglue.Request) bool {
	return r != nil && len(r.RequestCtx.PostBody()) <= automationMaxRequestBytes
}

func automationPageLimit(r *fastglue.Request) (int, int, error) {
	page := 1
	if raw := strings.TrimSpace(string(r.RequestCtx.QueryArgs().Peek("page"))); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 1 {
			return 0, 0, errors.New("page must be a positive integer")
		}
		page = value
	}
	limit, err := automationLimit(r, 50, 100)
	return page, limit, err
}

func automationLimit(r *fastglue.Request, defaultValue, maximum int) (int, error) {
	raw := strings.TrimSpace(string(r.RequestCtx.QueryArgs().Peek("limit")))
	if raw == "" {
		return defaultValue, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < 1 || value > maximum {
		return 0, fmt.Errorf("limit must be an integer between 1 and %d", maximum)
	}
	return value, nil
}

func automationDraftBlockingIssue(issues []AutomationValidationIssue) *AutomationValidationIssue {
	blocking := map[string]bool{
		"too_many_nodes": true, "too_many_edges": true, "invalid_node_id": true,
		"duplicate_node_id": true, "invalid_position": true, "unsupported_node_type": true,
		"unknown_edge_node": true, "self_edge": true,
	}
	for i := range issues {
		if blocking[issues[i].Code] {
			return &issues[i]
		}
	}
	return nil
}

func automationEnsureUniquePolicyName(
	tx *gorm.DB,
	orgID uuid.UUID,
	name string,
	excludeID uuid.UUID,
) error {
	query := tx.Model(&models.AutomationPolicy{}).
		Where("organization_id = ? AND LOWER(name) = LOWER(?)", orgID, name)
	if excludeID != uuid.Nil {
		query = query.Where("id != ?", excludeID)
	}
	var count int64
	if err := query.Count(&count).Error; err != nil {
		return err
	}
	if count > 0 {
		return newProductCRMClientError(fasthttp.StatusConflict, "An automation policy with this name already exists")
	}
	return nil
}

func automationRequirePolicyExists(
	tx *gorm.DB,
	orgID, policyID uuid.UUID,
) error {
	var policy models.AutomationPolicy
	return tx.Select("id").
		Where("id = ? AND organization_id = ?", policyID, orgID).
		First(&policy).Error
}

func automationLockDispatchState(tx *gorm.DB, orgID uuid.UUID) error {
	if tx == nil || orgID == uuid.Nil {
		return errors.New("automation dispatch lock requires a tenant transaction")
	}
	state := models.AutomationDispatchState{OrganizationID: orgID}
	if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&state).Error; err != nil {
		return err
	}
	return tx.Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("organization_id = ?", orgID).
		First(&state).Error
}

func automationDatabaseNow(tx *gorm.DB) (time.Time, error) {
	var now time.Time
	err := tx.Raw("SELECT clock_timestamp()").Scan(&now).Error
	return now.UTC(), err
}

func automationCloseOpenActivation(
	tx *gorm.DB,
	orgID, policyID, userID uuid.UUID,
	at time.Time,
) (int64, error) {
	result := tx.Model(&models.AutomationPolicyActivation{}).
		Where(
			"organization_id = ? AND policy_id = ? AND active_until IS NULL",
			orgID, policyID,
		).
		Updates(map[string]any{
			"active_until": at,
			"closed_by_id": userID,
		})
	return result.RowsAffected, result.Error
}

func automationValidateOwnerUsers(
	tx *gorm.DB,
	orgID uuid.UUID,
	userIDs []uuid.UUID,
) error {
	for _, userID := range userIDs {
		if err := ensureProductCRMUserMembership(tx, orgID, userID, "owner_user_id"); err != nil {
			return err
		}
	}
	return nil
}

func automationActivePolicyConflictWarnings(
	tx *gorm.DB,
	orgID, excludedPolicyID uuid.UUID,
	triggerEventTypes []string,
	semanticFingerprint string,
	rejectExact bool,
) ([]AutomationValidationIssue, error) {
	type activePolicyVersion struct {
		PolicyID          uuid.UUID
		PolicyName        string
		Graph             []byte
		TriggerEventTypes []byte
	}
	var active []activePolicyVersion
	if err := tx.Table("automation_policy_activations AS activation").
		Select(`
			activation.policy_id,
			policy.name AS policy_name,
			version.graph,
			version.trigger_event_types
		`).
		Joins(`
			JOIN automation_policy_versions AS version
			  ON version.id = activation.policy_version_id
			 AND version.policy_id = activation.policy_id
			 AND version.organization_id = activation.organization_id
		`).
		Joins(`
			JOIN automation_policies AS policy
			  ON policy.id = activation.policy_id
			 AND policy.organization_id = activation.organization_id
			 AND policy.deleted_at IS NULL
		`).
		Where(
			"activation.organization_id = ? AND activation.policy_id != ? AND activation.active_until IS NULL",
			orgID,
			excludedPolicyID,
		).
		Scan(&active).Error; err != nil {
		return nil, err
	}

	triggerSet := automationStringSet(triggerEventTypes)
	overlapCount := 0
	for _, candidate := range active {
		overlaps := false
		var candidateEventTypes []string
		if err := json.Unmarshal(candidate.TriggerEventTypes, &candidateEventTypes); err != nil {
			return nil, err
		}
		for _, eventType := range candidateEventTypes {
			if triggerSet[eventType] {
				overlaps = true
				break
			}
		}
		if !overlaps {
			continue
		}
		overlapCount++
		var graph AutomationGraph
		if err := json.Unmarshal(candidate.Graph, &graph); err != nil {
			return nil, err
		}
		fingerprint, err := automationGraphSemanticFingerprint(graph)
		if err != nil {
			return nil, err
		}
		if rejectExact && fingerprint == semanticFingerprint {
			return nil, newProductCRMClientError(
				fasthttp.StatusConflict,
				fmt.Sprintf(
					"An active automation policy (%s) already has the same behavior",
					candidate.PolicyName,
				),
			)
		}
	}
	if overlapCount == 0 {
		return nil, nil
	}
	return []AutomationValidationIssue{{
		Code: "shared_trigger_active_policy",
		Message: fmt.Sprintf(
			"This policy shares trigger events with %d active automation policy; each workflow will run independently",
			overlapCount,
		),
	}}, nil
}

func automationPolicyAuditSnapshot(policy *models.AutomationPolicy) map[string]any {
	if policy == nil {
		return nil
	}
	return map[string]any{
		"id":                    policy.ID,
		"name":                  policy.Name,
		"description":           policy.Description,
		"status":                policy.Status,
		"active_version_id":     policy.ActiveVersionID,
		"active_version_number": policy.ActiveVersionNumber,
		"trigger_event_types":   policy.TriggerEventTypes,
		"version":               policy.Version,
	}
}
