package handlers

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/audit"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/valyala/fasthttp"
	"github.com/zerodha/fastglue"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	productCRMPipelineResource = "crm_pipeline"
	productCRMStageResource    = "crm_pipeline_stage"
	productCRMLeadResource     = "crm_lead"
	productCRMTaskResource     = "follow_up_task"
)

// CRMPipelineStageInput is accepted when a pipeline is seeded with stages.
type CRMPipelineStageInput struct {
	Name         string                      `json:"name"`
	Color        string                      `json:"color,omitempty"`
	DisplayOrder *int                        `json:"display_order,omitempty"`
	Kind         models.CRMPipelineStageKind `json:"kind,omitempty"`
	Probability  *int                        `json:"probability,omitempty"`
	SLAHours     *int                        `json:"sla_hours,omitempty"`
	IsActive     *bool                       `json:"is_active,omitempty"`
}

// CreateCRMPipelineRequest creates a pipeline and either the supplied stages
// or a standard open/won/lost stage set when stages is empty.
type CreateCRMPipelineRequest struct {
	Name         string                  `json:"name"`
	Description  string                  `json:"description,omitempty"`
	IsDefault    bool                    `json:"is_default"`
	IsActive     *bool                   `json:"is_active,omitempty"`
	DisplayOrder int                     `json:"display_order"`
	Stages       []CRMPipelineStageInput `json:"stages,omitempty"`
}

// CreateCRMPipelineStageRequest adds a stage to a pipeline.
type CreateCRMPipelineStageRequest struct {
	Name         string                      `json:"name"`
	Color        string                      `json:"color,omitempty"`
	DisplayOrder *int                        `json:"display_order,omitempty"`
	Kind         models.CRMPipelineStageKind `json:"kind,omitempty"`
	Probability  *int                        `json:"probability,omitempty"`
	SLAHours     *int                        `json:"sla_hours,omitempty"`
	IsActive     *bool                       `json:"is_active,omitempty"`
}

// UpdateCRMPipelineStageRequest updates a stage with optimistic concurrency.
type UpdateCRMPipelineStageRequest struct {
	Version      int64                        `json:"version"`
	Name         *string                      `json:"name,omitempty"`
	Color        *string                      `json:"color,omitempty"`
	DisplayOrder *int                         `json:"display_order,omitempty"`
	Kind         *models.CRMPipelineStageKind `json:"kind,omitempty"`
	Probability  *int                         `json:"probability,omitempty"`
	SLAHours     *int                         `json:"sla_hours,omitempty"`
	IsActive     *bool                        `json:"is_active,omitempty"`
}

// CreateCRMLeadRequest creates a lead in an active tenant pipeline.
type CreateCRMLeadRequest struct {
	ContactID         uuid.UUID            `json:"contact_id"`
	PipelineID        uuid.UUID            `json:"pipeline_id"`
	StageID           *uuid.UUID           `json:"stage_id,omitempty"`
	Title             string               `json:"title"`
	OwnerUserID       *uuid.UUID           `json:"owner_user_id,omitempty"`
	Source            models.CRMLeadSource `json:"source,omitempty"`
	SourceReference   string               `json:"source_reference,omitempty"`
	ValueMinor        int64                `json:"value_minor"`
	Currency          string               `json:"currency,omitempty"`
	NextActionAt      *time.Time           `json:"next_action_at,omitempty"`
	ExpectedCloseDate *time.Time           `json:"expected_close_date,omitempty"`
	IdempotencyKey    string               `json:"idempotency_key,omitempty"`
	Metadata          models.JSONB         `json:"metadata,omitempty"`
}

// UpdateCRMLeadRequest deliberately excludes stage and status. Stage/status
// transitions must use MoveCRMLead so history and the outbox remain atomic.
type UpdateCRMLeadRequest struct {
	Version                int64                 `json:"version"`
	ContactID              *uuid.UUID            `json:"contact_id,omitempty"`
	Title                  *string               `json:"title,omitempty"`
	OwnerUserID            *uuid.UUID            `json:"owner_user_id,omitempty"`
	ClearOwnerUserID       bool                  `json:"clear_owner_user_id,omitempty"`
	Source                 *models.CRMLeadSource `json:"source,omitempty"`
	SourceReference        *string               `json:"source_reference,omitempty"`
	ValueMinor             *int64                `json:"value_minor,omitempty"`
	Currency               *string               `json:"currency,omitempty"`
	NextActionAt           *time.Time            `json:"next_action_at,omitempty"`
	ClearNextActionAt      bool                  `json:"clear_next_action_at,omitempty"`
	ExpectedCloseDate      *time.Time            `json:"expected_close_date,omitempty"`
	ClearExpectedCloseDate bool                  `json:"clear_expected_close_date,omitempty"`
	LostReason             *string               `json:"lost_reason,omitempty"`
	Metadata               *models.JSONB         `json:"metadata,omitempty"`
}

// MoveCRMLeadRequest is the sole contract for changing a lead's stage.
type MoveCRMLeadRequest struct {
	StageID  uuid.UUID    `json:"stage_id"`
	Version  int64        `json:"version"`
	Reason   string       `json:"reason,omitempty"`
	Metadata models.JSONB `json:"metadata,omitempty"`
}

// CreateFollowUpTaskRequest creates a user-owned follow-up task.
type CreateFollowUpTaskRequest struct {
	ContactID      *uuid.UUID                  `json:"contact_id,omitempty"`
	LeadID         *uuid.UUID                  `json:"lead_id,omitempty"`
	BookingID      *uuid.UUID                  `json:"booking_id,omitempty"`
	Title          string                      `json:"title"`
	Description    string                      `json:"description,omitempty"`
	Priority       models.FollowUpTaskPriority `json:"priority,omitempty"`
	OwnerUserID    *uuid.UUID                  `json:"owner_user_id,omitempty"`
	DueAt          *time.Time                  `json:"due_at,omitempty"`
	RemindAt       *time.Time                  `json:"remind_at,omitempty"`
	RecurrenceRule string                      `json:"recurrence_rule,omitempty"`
	ParentTaskID   *uuid.UUID                  `json:"parent_task_id,omitempty"`
	Source         string                      `json:"source,omitempty"`
	IdempotencyKey string                      `json:"idempotency_key,omitempty"`
	Metadata       models.JSONB                `json:"metadata,omitempty"`
}

// UpdateFollowUpTaskRequest supports explicit clear flags because JSON null
// and an omitted pointer both decode to nil in the standard decoder.
type UpdateFollowUpTaskRequest struct {
	Version           int64                        `json:"version"`
	ContactID         *uuid.UUID                   `json:"contact_id,omitempty"`
	ClearContactID    bool                         `json:"clear_contact_id,omitempty"`
	LeadID            *uuid.UUID                   `json:"lead_id,omitempty"`
	ClearLeadID       bool                         `json:"clear_lead_id,omitempty"`
	BookingID         *uuid.UUID                   `json:"booking_id,omitempty"`
	ClearBookingID    bool                         `json:"clear_booking_id,omitempty"`
	Title             *string                      `json:"title,omitempty"`
	Description       *string                      `json:"description,omitempty"`
	Status            *models.FollowUpTaskStatus   `json:"status,omitempty"`
	Priority          *models.FollowUpTaskPriority `json:"priority,omitempty"`
	OwnerUserID       *uuid.UUID                   `json:"owner_user_id,omitempty"`
	ClearOwnerUserID  bool                         `json:"clear_owner_user_id,omitempty"`
	DueAt             *time.Time                   `json:"due_at,omitempty"`
	ClearDueAt        bool                         `json:"clear_due_at,omitempty"`
	RemindAt          *time.Time                   `json:"remind_at,omitempty"`
	ClearRemindAt     bool                         `json:"clear_remind_at,omitempty"`
	RecurrenceRule    *string                      `json:"recurrence_rule,omitempty"`
	ParentTaskID      *uuid.UUID                   `json:"parent_task_id,omitempty"`
	ClearParentTaskID bool                         `json:"clear_parent_task_id,omitempty"`
	Source            *string                      `json:"source,omitempty"`
	Metadata          *models.JSONB                `json:"metadata,omitempty"`
}

// FollowUpTaskTransitionRequest guards complete/reopen operations.
type FollowUpTaskTransitionRequest struct {
	Version int64 `json:"version"`
}

// CRMPipelineStageResponse is the stable stage API shape.
type CRMPipelineStageResponse struct {
	ID           uuid.UUID                   `json:"id"`
	PipelineID   uuid.UUID                   `json:"pipeline_id"`
	Name         string                      `json:"name"`
	Color        string                      `json:"color,omitempty"`
	DisplayOrder int                         `json:"display_order"`
	Kind         models.CRMPipelineStageKind `json:"kind"`
	Probability  int                         `json:"probability"`
	SLAHours     int                         `json:"sla_hours"`
	IsActive     bool                        `json:"is_active"`
	Version      int64                       `json:"version"`
	CreatedAt    time.Time                   `json:"created_at"`
	UpdatedAt    time.Time                   `json:"updated_at"`
}

// CRMPipelineResponse includes stages in deterministic display order.
type CRMPipelineResponse struct {
	ID           uuid.UUID                  `json:"id"`
	Name         string                     `json:"name"`
	Description  string                     `json:"description,omitempty"`
	IsDefault    bool                       `json:"is_default"`
	IsActive     bool                       `json:"is_active"`
	DisplayOrder int                        `json:"display_order"`
	Version      int64                      `json:"version"`
	Stages       []CRMPipelineStageResponse `json:"stages"`
	CreatedAt    time.Time                  `json:"created_at"`
	UpdatedAt    time.Time                  `json:"updated_at"`
}

// CRMContactReference avoids exposing the contact's complete conversation data.
type CRMContactReference struct {
	ID          uuid.UUID `json:"id"`
	ProfileName string    `json:"profile_name"`
	PhoneNumber string    `json:"phone_number"`
}

// CRMUserReference keeps user responses to identity/display data.
type CRMUserReference struct {
	ID       uuid.UUID `json:"id"`
	FullName string    `json:"full_name"`
}

// CRMStageHistoryResponse is the append-only stage transition view.
type CRMStageHistoryResponse struct {
	ID          uuid.UUID                 `json:"id"`
	FromStageID *uuid.UUID                `json:"from_stage_id,omitempty"`
	ToStageID   uuid.UUID                 `json:"to_stage_id"`
	ChangedByID *uuid.UUID                `json:"changed_by_id,omitempty"`
	Reason      string                    `json:"reason,omitempty"`
	Metadata    models.JSONB              `json:"metadata"`
	ChangedAt   time.Time                 `json:"changed_at"`
	FromStage   *CRMPipelineStageResponse `json:"from_stage,omitempty"`
	ToStage     *CRMPipelineStageResponse `json:"to_stage,omitempty"`
	ChangedBy   *CRMUserReference         `json:"changed_by,omitempty"`
}

// CRMLeadResponse is the safe API representation of a lead.
type CRMLeadResponse struct {
	ID                uuid.UUID                 `json:"id"`
	ContactID         uuid.UUID                 `json:"contact_id"`
	PipelineID        uuid.UUID                 `json:"pipeline_id"`
	StageID           uuid.UUID                 `json:"stage_id"`
	Title             string                    `json:"title"`
	Status            models.CRMLeadStatus      `json:"status"`
	OwnerUserID       *uuid.UUID                `json:"owner_user_id,omitempty"`
	Source            models.CRMLeadSource      `json:"source"`
	SourceReference   string                    `json:"source_reference,omitempty"`
	ValueMinor        int64                     `json:"value_minor"`
	Currency          string                    `json:"currency"`
	NextActionAt      *time.Time                `json:"next_action_at,omitempty"`
	ExpectedCloseDate *time.Time                `json:"expected_close_date,omitempty"`
	LastActivityAt    *time.Time                `json:"last_activity_at,omitempty"`
	WonAt             *time.Time                `json:"won_at,omitempty"`
	LostAt            *time.Time                `json:"lost_at,omitempty"`
	LostReason        string                    `json:"lost_reason,omitempty"`
	Metadata          models.JSONB              `json:"metadata"`
	Version           int64                     `json:"version"`
	Contact           *CRMContactReference      `json:"contact,omitempty"`
	Pipeline          *CRMPipelineResponse      `json:"pipeline,omitempty"`
	Stage             *CRMPipelineStageResponse `json:"stage,omitempty"`
	Owner             *CRMUserReference         `json:"owner,omitempty"`
	History           []CRMStageHistoryResponse `json:"history,omitempty"`
	CreatedAt         time.Time                 `json:"created_at"`
	UpdatedAt         time.Time                 `json:"updated_at"`
}

// FollowUpTaskResponse is the stable task API shape.
type FollowUpTaskResponse struct {
	ID             uuid.UUID                   `json:"id"`
	ContactID      *uuid.UUID                  `json:"contact_id,omitempty"`
	LeadID         *uuid.UUID                  `json:"lead_id,omitempty"`
	BookingID      *uuid.UUID                  `json:"booking_id,omitempty"`
	Title          string                      `json:"title"`
	Description    string                      `json:"description,omitempty"`
	Status         models.FollowUpTaskStatus   `json:"status"`
	Priority       models.FollowUpTaskPriority `json:"priority"`
	OwnerUserID    *uuid.UUID                  `json:"owner_user_id,omitempty"`
	DueAt          *time.Time                  `json:"due_at,omitempty"`
	RemindAt       *time.Time                  `json:"remind_at,omitempty"`
	CompletedAt    *time.Time                  `json:"completed_at,omitempty"`
	CompletedByID  *uuid.UUID                  `json:"completed_by_id,omitempty"`
	RecurrenceRule string                      `json:"recurrence_rule,omitempty"`
	ParentTaskID   *uuid.UUID                  `json:"parent_task_id,omitempty"`
	Source         string                      `json:"source,omitempty"`
	Metadata       models.JSONB                `json:"metadata"`
	Version        int64                       `json:"version"`
	Contact        *CRMContactReference        `json:"contact,omitempty"`
	Lead           *CRMLeadResponse            `json:"lead,omitempty"`
	Owner          *CRMUserReference           `json:"owner,omitempty"`
	CompletedBy    *CRMUserReference           `json:"completed_by,omitempty"`
	CreatedAt      time.Time                   `json:"created_at"`
	UpdatedAt      time.Time                   `json:"updated_at"`
}

// FollowUpTaskSummary provides dashboard-ready task counts.
type FollowUpTaskSummary struct {
	Total        int64 `json:"total"`
	Open         int64 `json:"open"`
	InProgress   int64 `json:"in_progress"`
	Completed    int64 `json:"completed"`
	Cancelled    int64 `json:"cancelled"`
	Overdue      int64 `json:"overdue"`
	DueToday     int64 `json:"due_today"`
	AssignedToMe int64 `json:"assigned_to_me"`
}

type productCRMClientError struct {
	status  int
	message string
}

func (e *productCRMClientError) Error() string {
	return e.message
}

func newProductCRMClientError(status int, message string) error {
	return &productCRMClientError{status: status, message: message}
}

func (a *App) sendProductCRMWriteError(r *fastglue.Request, operation string, err error) error {
	var clientErr *productCRMClientError
	if errors.As(err, &clientErr) {
		return r.SendErrorEnvelope(clientErr.status, clientErr.message, nil, "")
	}
	a.Log.Error("CRM operation failed", "operation", operation, "error", err)
	return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to "+operation, nil, "")
}

// ListCRMPipelines lists tenant pipelines and their ordered stages.
func (a *App) ListCRMPipelines(r *fastglue.Request) error {
	orgID, _, err := a.requireAuth(r, models.ResourceCRMPipelines, models.ActionRead)
	if err != nil {
		return nil
	}

	pg := parsePagination(r)
	query := a.DB.Model(&models.CRMPipeline{}).Where("organization_id = ?", orgID)

	if raw := strings.TrimSpace(string(r.RequestCtx.QueryArgs().Peek("active"))); raw != "" {
		active, parseErr := strconv.ParseBool(raw)
		if parseErr != nil {
			return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "active must be true or false", nil, "")
		}
		query = query.Where("is_active = ?", active)
	}
	if search := strings.TrimSpace(string(r.RequestCtx.QueryArgs().Peek("search"))); search != "" {
		if utf8.RuneCountInString(search) > 150 {
			return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "search must be at most 150 characters", nil, "")
		}
		query = query.Where("name ILIKE ?", productCRMSearchPattern(search))
	}

	var total int64
	if err := query.Count(&total).Error; err != nil {
		a.Log.Error("Failed to count CRM pipelines", "error", err)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to list CRM pipelines", nil, "")
	}

	var pipelines []models.CRMPipeline
	err = pg.Apply(query).
		Preload("Stages", func(db *gorm.DB) *gorm.DB {
			return db.Where("organization_id = ?", orgID).
				Order("display_order ASC, created_at ASC")
		}).
		Order("display_order ASC, created_at ASC").
		Find(&pipelines).Error
	if err != nil {
		a.Log.Error("Failed to list CRM pipelines", "error", err)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to list CRM pipelines", nil, "")
	}

	response := make([]CRMPipelineResponse, len(pipelines))
	for i := range pipelines {
		response[i] = crmPipelineToResponse(&pipelines[i])
	}
	return r.SendEnvelope(listEnvelope("pipelines", response, total, pg))
}

// CreateCRMPipeline creates a pipeline and seeds its stages transactionally.
func (a *App) CreateCRMPipeline(r *fastglue.Request) error {
	orgID, userID, err := a.requireAuth(r, models.ResourceCRMPipelines, models.ActionWrite)
	if err != nil {
		return nil
	}

	var req CreateCRMPipelineRequest
	if err := a.decodeRequest(r, &req); err != nil {
		return nil
	}
	if err := validateCreateCRMPipelineRequest(&req); err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, err.Error(), nil, "")
	}

	stageInputs := req.Stages
	if len(stageInputs) == 0 {
		stageInputs = defaultCRMPipelineStages()
	}
	active := true
	if req.IsActive != nil {
		active = *req.IsActive
	}

	pipeline := models.CRMPipeline{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: orgID,
		Name:           strings.TrimSpace(req.Name),
		Description:    strings.TrimSpace(req.Description),
		IsDefault:      req.IsDefault,
		IsActive:       active,
		DisplayOrder:   req.DisplayOrder,
		Version:        1,
		CreatedByID:    &userID,
		UpdatedByID:    &userID,
	}

	err = a.DB.Transaction(func(tx *gorm.DB) error {
		var duplicateCount int64
		if err := tx.Model(&models.CRMPipeline{}).
			Where("organization_id = ? AND LOWER(name) = LOWER(?)", orgID, pipeline.Name).
			Count(&duplicateCount).Error; err != nil {
			return err
		}
		if duplicateCount > 0 {
			return newProductCRMClientError(fasthttp.StatusConflict, "A CRM pipeline with this name already exists")
		}

		if pipeline.IsDefault {
			if err := tx.Model(&models.CRMPipeline{}).
				Where("organization_id = ? AND is_default = ?", orgID, true).
				Updates(map[string]any{
					"is_default":    false,
					"updated_by_id": userID,
					"version":       gorm.Expr("version + 1"),
				}).Error; err != nil {
				return err
			}
		}
		if err := tx.Create(&pipeline).Error; err != nil {
			return err
		}

		pipeline.Stages = make([]models.CRMPipelineStage, 0, len(stageInputs))
		for i := range stageInputs {
			stage := crmStageFromInput(orgID, userID, pipeline.ID, stageInputs[i], i)
			if err := tx.Create(&stage).Error; err != nil {
				return err
			}
			pipeline.Stages = append(pipeline.Stages, stage)
		}

		return audit.LogAudit(
			tx,
			orgID,
			userID,
			audit.GetUserName(tx, userID),
			productCRMPipelineResource,
			pipeline.ID,
			models.AuditActionCreated,
			nil,
			crmPipelineAuditSnapshot(&pipeline),
		)
	})
	if err != nil {
		return a.sendProductCRMWriteError(r, "create CRM pipeline", err)
	}
	return r.SendEnvelope(crmPipelineToResponse(&pipeline))
}

// CreateCRMPipelineStage adds a stage to the pipeline identified by
// pipeline_id. If display_order is omitted, the stage is appended.
func (a *App) CreateCRMPipelineStage(r *fastglue.Request) error {
	orgID, userID, err := a.requireAuth(r, models.ResourceCRMPipelines, models.ActionWrite)
	if err != nil {
		return nil
	}
	pipelineID, err := parsePathUUID(r, "pipeline_id", "pipeline")
	if err != nil {
		return nil
	}

	var req CreateCRMPipelineStageRequest
	if err := a.decodeRequest(r, &req); err != nil {
		return nil
	}
	input := CRMPipelineStageInput(req)
	if err := validateCRMPipelineStageInput(&input); err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, err.Error(), nil, "")
	}

	var stage models.CRMPipelineStage
	err = a.DB.Transaction(func(tx *gorm.DB) error {
		var pipeline models.CRMPipeline
		if err := tx.Where("id = ? AND organization_id = ?", pipelineID, orgID).
			First(&pipeline).Error; err != nil {
			return newProductCRMClientError(fasthttp.StatusNotFound, "CRM pipeline not found")
		}

		var duplicateCount int64
		if err := tx.Model(&models.CRMPipelineStage{}).
			Where(
				"organization_id = ? AND pipeline_id = ? AND LOWER(name) = LOWER(?)",
				orgID,
				pipelineID,
				strings.TrimSpace(input.Name),
			).
			Count(&duplicateCount).Error; err != nil {
			return err
		}
		if duplicateCount > 0 {
			return newProductCRMClientError(fasthttp.StatusConflict, "A stage with this name already exists in the pipeline")
		}

		fallbackOrder := 0
		if input.DisplayOrder == nil {
			var maxOrder *int
			if err := tx.Model(&models.CRMPipelineStage{}).
				Where("organization_id = ? AND pipeline_id = ?", orgID, pipelineID).
				Select("MAX(display_order)").
				Scan(&maxOrder).Error; err != nil {
				return err
			}
			if maxOrder != nil {
				fallbackOrder = *maxOrder + 1
			}
		}
		stage = crmStageFromInput(orgID, userID, pipelineID, input, fallbackOrder)
		if err := tx.Create(&stage).Error; err != nil {
			return err
		}

		return audit.LogAudit(
			tx,
			orgID,
			userID,
			audit.GetUserName(tx, userID),
			productCRMStageResource,
			stage.ID,
			models.AuditActionCreated,
			nil,
			crmStageAuditSnapshot(&stage),
		)
	})
	if err != nil {
		return a.sendProductCRMWriteError(r, "create CRM pipeline stage", err)
	}
	return r.SendEnvelope(crmStageToResponse(&stage))
}

// UpdateCRMPipelineStage updates a stage using its current version.
func (a *App) UpdateCRMPipelineStage(r *fastglue.Request) error {
	orgID, userID, err := a.requireAuth(r, models.ResourceCRMPipelines, models.ActionWrite)
	if err != nil {
		return nil
	}
	pipelineID, err := parsePathUUID(r, "pipeline_id", "pipeline")
	if err != nil {
		return nil
	}
	stageID, err := parsePathUUID(r, "id", "stage")
	if err != nil {
		return nil
	}

	var req UpdateCRMPipelineStageRequest
	if err := a.decodeRequest(r, &req); err != nil {
		return nil
	}
	if err := validateUpdateCRMPipelineStageRequest(&req); err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, err.Error(), nil, "")
	}

	var updated models.CRMPipelineStage
	err = a.DB.Transaction(func(tx *gorm.DB) error {
		var stage models.CRMPipelineStage
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where(
				"id = ? AND organization_id = ? AND pipeline_id = ?",
				stageID,
				orgID,
				pipelineID,
			).
			First(&stage).Error
		if err != nil {
			return newProductCRMClientError(fasthttp.StatusNotFound, "CRM pipeline stage not found")
		}
		if stage.Version != req.Version {
			return newProductCRMClientError(
				fasthttp.StatusConflict,
				"CRM pipeline stage was modified; refresh and retry",
			)
		}

		if req.Name != nil {
			var duplicateCount int64
			if err := tx.Model(&models.CRMPipelineStage{}).
				Where(
					"organization_id = ? AND pipeline_id = ? AND id <> ? AND LOWER(name) = LOWER(?)",
					orgID,
					pipelineID,
					stageID,
					strings.TrimSpace(*req.Name),
				).
				Count(&duplicateCount).Error; err != nil {
				return err
			}
			if duplicateCount > 0 {
				return newProductCRMClientError(
					fasthttp.StatusConflict,
					"A stage with this name already exists in the pipeline",
				)
			}
		}

		if req.Kind != nil && *req.Kind != stage.Kind {
			var leadCount int64
			if err := tx.Model(&models.CRMLead{}).
				Where("organization_id = ? AND stage_id = ?", orgID, stageID).
				Count(&leadCount).Error; err != nil {
				return err
			}
			if leadCount > 0 {
				return newProductCRMClientError(
					fasthttp.StatusConflict,
					"Move leads out of this stage before changing its kind",
				)
			}
		}

		oldSnapshot := crmStageAuditSnapshot(&stage)
		updates := map[string]any{
			"updated_by_id": userID,
			"version":       gorm.Expr("version + 1"),
		}
		if req.Name != nil {
			updates["name"] = strings.TrimSpace(*req.Name)
		}
		if req.Color != nil {
			updates["color"] = strings.TrimSpace(*req.Color)
		}
		if req.DisplayOrder != nil {
			updates["display_order"] = *req.DisplayOrder
		}
		if req.Kind != nil {
			updates["kind"] = *req.Kind
		}
		if req.Probability != nil {
			updates["probability"] = *req.Probability
		}
		if req.SLAHours != nil {
			updates["sla_hours"] = *req.SLAHours
		}
		if req.IsActive != nil {
			updates["is_active"] = *req.IsActive
		}

		result := tx.Model(&models.CRMPipelineStage{}).
			Where("id = ? AND organization_id = ? AND version = ?", stageID, orgID, req.Version).
			Updates(updates)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return newProductCRMClientError(
				fasthttp.StatusConflict,
				"CRM pipeline stage was modified; refresh and retry",
			)
		}
		if err := tx.Where("id = ? AND organization_id = ?", stageID, orgID).
			First(&updated).Error; err != nil {
			return err
		}

		return audit.LogAudit(
			tx,
			orgID,
			userID,
			audit.GetUserName(tx, userID),
			productCRMStageResource,
			stageID,
			models.AuditActionUpdated,
			oldSnapshot,
			crmStageAuditSnapshot(&updated),
		)
	})
	if err != nil {
		return a.sendProductCRMWriteError(r, "update CRM pipeline stage", err)
	}
	return r.SendEnvelope(crmStageToResponse(&updated))
}

// DeleteCRMPipelineStage soft-deletes an unused stage.
func (a *App) DeleteCRMPipelineStage(r *fastglue.Request) error {
	orgID, userID, err := a.requireAuth(r, models.ResourceCRMPipelines, models.ActionDelete)
	if err != nil {
		return nil
	}
	pipelineID, err := parsePathUUID(r, "pipeline_id", "pipeline")
	if err != nil {
		return nil
	}
	stageID, err := parsePathUUID(r, "id", "stage")
	if err != nil {
		return nil
	}

	err = a.DB.Transaction(func(tx *gorm.DB) error {
		var stage models.CRMPipelineStage
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where(
				"id = ? AND organization_id = ? AND pipeline_id = ?",
				stageID,
				orgID,
				pipelineID,
			).
			First(&stage).Error; err != nil {
			return newProductCRMClientError(fasthttp.StatusNotFound, "CRM pipeline stage not found")
		}

		var leadCount int64
		if err := tx.Model(&models.CRMLead{}).
			Where("organization_id = ? AND stage_id = ?", orgID, stageID).
			Count(&leadCount).Error; err != nil {
			return err
		}
		if leadCount > 0 {
			return newProductCRMClientError(
				fasthttp.StatusConflict,
				"Move leads out of this stage before deleting it",
			)
		}

		var stageCount int64
		if err := tx.Model(&models.CRMPipelineStage{}).
			Where("organization_id = ? AND pipeline_id = ?", orgID, pipelineID).
			Count(&stageCount).Error; err != nil {
			return err
		}
		if stageCount <= 1 {
			return newProductCRMClientError(
				fasthttp.StatusConflict,
				"A CRM pipeline must retain at least one stage",
			)
		}

		if err := tx.Delete(&stage).Error; err != nil {
			return err
		}
		return audit.LogAudit(
			tx,
			orgID,
			userID,
			audit.GetUserName(tx, userID),
			productCRMStageResource,
			stageID,
			models.AuditActionDeleted,
			crmStageAuditSnapshot(&stage),
			nil,
		)
	})
	if err != nil {
		return a.sendProductCRMWriteError(r, "delete CRM pipeline stage", err)
	}
	return r.SendEnvelope(map[string]string{"message": "CRM pipeline stage deleted"})
}

// ListCRMLeads lists leads with tenant-safe filters.
func (a *App) ListCRMLeads(r *fastglue.Request) error {
	orgID, userID, err := a.requireAuth(r, models.ResourceCRMLeads, models.ActionRead)
	if err != nil {
		return nil
	}

	pg := parsePagination(r)
	query := a.DB.Model(&models.CRMLead{}).Where("crm_leads.organization_id = ?", orgID)

	for _, filter := range []struct {
		param  string
		column string
		label  string
	}{
		{"pipeline_id", "pipeline_id", "pipeline"},
		{"stage_id", "stage_id", "stage"},
		{"contact_id", "contact_id", "contact"},
		{"owner_user_id", "owner_user_id", "owner user"},
	} {
		value, parseErr := productCRMQueryUUID(r, filter.param)
		if parseErr != nil {
			return r.SendErrorEnvelope(
				fasthttp.StatusBadRequest,
				"Invalid "+filter.label+" ID",
				nil,
				"",
			)
		}
		if value != nil {
			query = query.Where(filter.column+" = ?", *value)
		}
	}

	if raw := strings.TrimSpace(string(r.RequestCtx.QueryArgs().Peek("mine"))); raw != "" {
		mine, parseErr := strconv.ParseBool(raw)
		if parseErr != nil {
			return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "mine must be true or false", nil, "")
		}
		if mine {
			query = query.Where("owner_user_id = ?", userID)
		}
	}
	if raw := strings.TrimSpace(string(r.RequestCtx.QueryArgs().Peek("status"))); raw != "" {
		status := models.CRMLeadStatus(raw)
		if !validCRMLeadStatus(status) {
			return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid lead status", nil, "")
		}
		query = query.Where("status = ?", status)
	}
	if raw := strings.TrimSpace(string(r.RequestCtx.QueryArgs().Peek("source"))); raw != "" {
		source := models.CRMLeadSource(raw)
		if !validCRMLeadSource(source) {
			return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid lead source", nil, "")
		}
		query = query.Where("source = ?", source)
	}
	if search := strings.TrimSpace(string(r.RequestCtx.QueryArgs().Peek("search"))); search != "" {
		if utf8.RuneCountInString(search) > 255 {
			return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "search must be at most 255 characters", nil, "")
		}
		pattern := productCRMSearchPattern(search)
		query = query.Where(
			"(title ILIKE ? OR source_reference ILIKE ?)",
			pattern,
			pattern,
		)
	}

	for _, dateFilter := range []struct {
		param     string
		column    string
		endOfDate bool
	}{
		{"created_from", "crm_leads.created_at", false},
		{"created_to", "crm_leads.created_at", true},
		{"next_action_from", "next_action_at", false},
		{"next_action_to", "next_action_at", true},
	} {
		value, parseErr := productCRMOptionalDate(r, dateFilter.param)
		if parseErr != nil {
			return r.SendErrorEnvelope(
				fasthttp.StatusBadRequest,
				dateFilter.param+" must use YYYY-MM-DD",
				nil,
				"",
			)
		}
		if value != nil {
			operator := ">="
			date := *value
			if dateFilter.endOfDate {
				operator = "<="
				date = endOfDay(date)
			}
			query = query.Where(dateFilter.column+" "+operator+" ?", date)
		}
	}

	var total int64
	if err := query.Count(&total).Error; err != nil {
		a.Log.Error("Failed to count CRM leads", "error", err)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to list CRM leads", nil, "")
	}

	var leads []models.CRMLead
	err = productCRMLeadPreloads(pg.Apply(query), orgID, false).
		Order("crm_leads.updated_at DESC, crm_leads.created_at DESC").
		Find(&leads).Error
	if err != nil {
		a.Log.Error("Failed to list CRM leads", "error", err)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to list CRM leads", nil, "")
	}

	response := make([]CRMLeadResponse, len(leads))
	for i := range leads {
		response[i] = crmLeadToResponse(&leads[i])
	}
	return r.SendEnvelope(listEnvelope("leads", response, total, pg))
}

// CreateCRMLead validates every tenant-owned reference before insertion.
func (a *App) CreateCRMLead(r *fastglue.Request) error {
	orgID, userID, err := a.requireAuth(r, models.ResourceCRMLeads, models.ActionWrite)
	if err != nil {
		return nil
	}

	var req CreateCRMLeadRequest
	if err := a.decodeRequest(r, &req); err != nil {
		return nil
	}
	if err := validateCreateCRMLeadRequest(&req); err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, err.Error(), nil, "")
	}

	if req.Source == "" {
		req.Source = models.CRMLeadSourceOther
	}
	if req.Currency == "" {
		req.Currency = "MYR"
	} else {
		req.Currency = strings.ToUpper(strings.TrimSpace(req.Currency))
	}
	if req.Metadata == nil {
		req.Metadata = models.JSONB{}
	}
	if req.OwnerUserID == nil {
		req.OwnerUserID = &userID
	}
	req.NextActionAt = productCRMUTC(req.NextActionAt)
	req.ExpectedCloseDate = productCRMUTC(req.ExpectedCloseDate)
	requestFingerprint, err := productCRMRequestFingerprint(req)
	if err != nil {
		a.Log.Error("Failed to fingerprint CRM lead request", "error", err)
		return r.SendErrorEnvelope(
			fasthttp.StatusInternalServerError,
			"Failed to create CRM lead",
			nil,
			"",
		)
	}

	var lead models.CRMLead
	err = a.DB.Transaction(func(tx *gorm.DB) error {
		if req.IdempotencyKey != "" {
			err := tx.Where(
				"organization_id = ? AND idempotency_key = ?",
				orgID,
				req.IdempotencyKey,
			).First(&lead).Error
			if err == nil {
				return validateProductCRMReplay(
					lead.RequestFingerprint,
					requestFingerprint,
					"CRM lead",
				)
			}
			if !errors.Is(err, gorm.ErrRecordNotFound) {
				return err
			}
		}

		pipeline, stage, refErr := validateCRMLeadCreateReferences(tx, orgID, &req)
		if refErr != nil {
			return refErr
		}
		_ = pipeline

		now := time.Now().UTC()
		status, wonAt, lostAt := crmLeadStateForStage(stage.Kind, now)
		lead = models.CRMLead{
			BaseModel:          models.BaseModel{ID: uuid.New()},
			OrganizationID:     orgID,
			ContactID:          req.ContactID,
			PipelineID:         req.PipelineID,
			StageID:            stage.ID,
			Title:              strings.TrimSpace(req.Title),
			Status:             status,
			OwnerUserID:        req.OwnerUserID,
			Source:             req.Source,
			SourceReference:    strings.TrimSpace(req.SourceReference),
			ValueMinor:         req.ValueMinor,
			Currency:           req.Currency,
			NextActionAt:       req.NextActionAt,
			ExpectedCloseDate:  req.ExpectedCloseDate,
			LastActivityAt:     &now,
			WonAt:              wonAt,
			LostAt:             lostAt,
			IdempotencyKey:     strings.TrimSpace(req.IdempotencyKey),
			RequestFingerprint: requestFingerprint,
			Metadata:           req.Metadata,
			Version:            1,
			CreatedByID:        &userID,
			UpdatedByID:        &userID,
		}
		var create *gorm.DB
		if req.IdempotencyKey == "" {
			create = tx.Create(&lead)
		} else {
			create = tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&lead)
		}
		if create.Error != nil {
			return create.Error
		}
		if create.RowsAffected == 0 {
			var existing models.CRMLead
			if err := tx.Where(
				"organization_id = ? AND idempotency_key = ?",
				orgID,
				req.IdempotencyKey,
			).First(&existing).Error; err != nil {
				return err
			}
			if err := validateProductCRMReplay(
				existing.RequestFingerprint,
				requestFingerprint,
				"CRM lead",
			); err != nil {
				return err
			}
			lead = existing
			return nil
		}

		history := models.CRMStageHistory{
			ID:             uuid.New(),
			OrganizationID: orgID,
			LeadID:         lead.ID,
			ToStageID:      stage.ID,
			ChangedByID:    &userID,
			Reason:         "Lead created",
			Metadata:       models.JSONB{"source": string(req.Source)},
			ChangedAt:      now,
		}
		if err := tx.Create(&history).Error; err != nil {
			return err
		}

		return audit.LogAudit(
			tx,
			orgID,
			userID,
			audit.GetUserName(tx, userID),
			productCRMLeadResource,
			lead.ID,
			models.AuditActionCreated,
			nil,
			crmLeadAuditSnapshot(&lead),
		)
	})
	if err != nil {
		return a.sendProductCRMWriteError(r, "create CRM lead", err)
	}

	if err := a.loadCRMLead(&lead, orgID, true); err != nil {
		a.Log.Error("Failed to reload CRM lead", "error", err, "lead_id", lead.ID)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to load CRM lead", nil, "")
	}
	return r.SendEnvelope(crmLeadToResponse(&lead))
}

// GetCRMLead returns one tenant-scoped lead with ordered history.
func (a *App) GetCRMLead(r *fastglue.Request) error {
	orgID, _, err := a.requireAuth(r, models.ResourceCRMLeads, models.ActionRead)
	if err != nil {
		return nil
	}
	leadID, err := parsePathUUID(r, "id", "lead")
	if err != nil {
		return nil
	}

	lead := models.CRMLead{BaseModel: models.BaseModel{ID: leadID}}
	if err := a.loadCRMLead(&lead, orgID, true); err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return r.SendErrorEnvelope(fasthttp.StatusNotFound, "CRM lead not found", nil, "")
		}
		a.Log.Error("Failed to get CRM lead", "error", err, "lead_id", leadID)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to get CRM lead", nil, "")
	}
	return r.SendEnvelope(crmLeadToResponse(&lead))
}

// UpdateCRMLead updates non-stage lead fields with optimistic concurrency.
func (a *App) UpdateCRMLead(r *fastglue.Request) error {
	orgID, userID, err := a.requireAuth(r, models.ResourceCRMLeads, models.ActionWrite)
	if err != nil {
		return nil
	}
	leadID, err := parsePathUUID(r, "id", "lead")
	if err != nil {
		return nil
	}

	var req UpdateCRMLeadRequest
	if err := a.decodeRequest(r, &req); err != nil {
		return nil
	}
	if err := validateUpdateCRMLeadRequest(&req); err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, err.Error(), nil, "")
	}

	var updated models.CRMLead
	err = a.DB.Transaction(func(tx *gorm.DB) error {
		var lead models.CRMLead
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ? AND organization_id = ?", leadID, orgID).
			First(&lead).Error; err != nil {
			return newProductCRMClientError(fasthttp.StatusNotFound, "CRM lead not found")
		}
		if lead.Version != req.Version {
			return newProductCRMClientError(
				fasthttp.StatusConflict,
				"CRM lead was modified; refresh and retry",
			)
		}
		if req.LostReason != nil && lead.Status != models.CRMLeadStatusLost {
			return newProductCRMClientError(
				fasthttp.StatusBadRequest,
				"lost_reason can only be edited while the lead is lost",
			)
		}

		if req.ContactID != nil {
			if err := ensureProductCRMTenantRecord(
				tx,
				&models.Contact{},
				orgID,
				*req.ContactID,
				"contact_id",
			); err != nil {
				return err
			}
		}
		if req.OwnerUserID != nil {
			if err := ensureProductCRMUserMembership(tx, orgID, *req.OwnerUserID, "owner_user_id"); err != nil {
				return err
			}
		}

		oldSnapshot := crmLeadAuditSnapshot(&lead)
		now := time.Now().UTC()
		updates := map[string]any{
			"last_activity_at": now,
			"updated_by_id":    userID,
			"version":          gorm.Expr("version + 1"),
		}
		if req.ContactID != nil {
			updates["contact_id"] = *req.ContactID
		}
		if req.Title != nil {
			updates["title"] = strings.TrimSpace(*req.Title)
		}
		if req.ClearOwnerUserID {
			updates["owner_user_id"] = nil
		} else if req.OwnerUserID != nil {
			updates["owner_user_id"] = *req.OwnerUserID
		}
		if req.Source != nil {
			updates["source"] = *req.Source
		}
		if req.SourceReference != nil {
			updates["source_reference"] = strings.TrimSpace(*req.SourceReference)
		}
		if req.ValueMinor != nil {
			updates["value_minor"] = *req.ValueMinor
		}
		if req.Currency != nil {
			updates["currency"] = strings.ToUpper(strings.TrimSpace(*req.Currency))
		}
		if req.ClearNextActionAt {
			updates["next_action_at"] = nil
		} else if req.NextActionAt != nil {
			updates["next_action_at"] = req.NextActionAt.UTC()
		}
		if req.ClearExpectedCloseDate {
			updates["expected_close_date"] = nil
		} else if req.ExpectedCloseDate != nil {
			updates["expected_close_date"] = req.ExpectedCloseDate.UTC()
		}
		if req.LostReason != nil {
			updates["lost_reason"] = strings.TrimSpace(*req.LostReason)
		}
		if req.Metadata != nil {
			updates["metadata"] = *req.Metadata
		}

		result := tx.Model(&models.CRMLead{}).
			Where("id = ? AND organization_id = ? AND version = ?", leadID, orgID, req.Version).
			Updates(updates)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return newProductCRMClientError(
				fasthttp.StatusConflict,
				"CRM lead was modified; refresh and retry",
			)
		}
		if err := tx.Where("id = ? AND organization_id = ?", leadID, orgID).
			First(&updated).Error; err != nil {
			return err
		}

		return audit.LogAudit(
			tx,
			orgID,
			userID,
			audit.GetUserName(tx, userID),
			productCRMLeadResource,
			leadID,
			models.AuditActionUpdated,
			oldSnapshot,
			crmLeadAuditSnapshot(&updated),
		)
	})
	if err != nil {
		return a.sendProductCRMWriteError(r, "update CRM lead", err)
	}
	if err := a.loadCRMLead(&updated, orgID, false); err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to load CRM lead", nil, "")
	}
	return r.SendEnvelope(crmLeadToResponse(&updated))
}

// MoveCRMLead atomically changes stage/status, appends history, writes the
// audit record, and emits a transactional outbox event.
func (a *App) MoveCRMLead(r *fastglue.Request) error {
	orgID, userID, err := a.requireAuth(r, models.ResourceCRMLeads, models.ActionWrite)
	if err != nil {
		return nil
	}
	leadID, err := parsePathUUID(r, "id", "lead")
	if err != nil {
		return nil
	}

	var req MoveCRMLeadRequest
	if err := a.decodeRequest(r, &req); err != nil {
		return nil
	}
	if err := validateMoveCRMLeadRequest(&req); err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, err.Error(), nil, "")
	}

	var updated models.CRMLead
	err = a.DB.Transaction(func(tx *gorm.DB) error {
		var lead models.CRMLead
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ? AND organization_id = ?", leadID, orgID).
			First(&lead).Error; err != nil {
			return newProductCRMClientError(fasthttp.StatusNotFound, "CRM lead not found")
		}
		if lead.Version != req.Version {
			return newProductCRMClientError(
				fasthttp.StatusConflict,
				"CRM lead was modified; refresh and retry",
			)
		}
		if lead.StageID == req.StageID {
			return newProductCRMClientError(
				fasthttp.StatusConflict,
				"CRM lead is already in the requested stage",
			)
		}

		var targetStage models.CRMPipelineStage
		if err := tx.Where(
			"id = ? AND organization_id = ? AND pipeline_id = ? AND is_active = ?",
			req.StageID,
			orgID,
			lead.PipelineID,
			true,
		).First(&targetStage).Error; err != nil {
			return newProductCRMClientError(
				fasthttp.StatusBadRequest,
				"stage_id does not belong to the lead's active pipeline",
			)
		}

		oldSnapshot := crmLeadAuditSnapshot(&lead)
		now := time.Now().UTC()
		status, wonAt, lostAt := crmLeadStateForStage(targetStage.Kind, now)
		newVersion := lead.Version + 1
		updates := map[string]any{
			"stage_id":         targetStage.ID,
			"status":           status,
			"won_at":           wonAt,
			"lost_at":          lostAt,
			"last_activity_at": now,
			"updated_by_id":    userID,
			"version":          newVersion,
		}
		if status == models.CRMLeadStatusLost {
			updates["lost_reason"] = strings.TrimSpace(req.Reason)
		} else {
			updates["lost_reason"] = ""
		}

		result := tx.Model(&models.CRMLead{}).
			Where("id = ? AND organization_id = ? AND version = ?", leadID, orgID, req.Version).
			Updates(updates)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return newProductCRMClientError(
				fasthttp.StatusConflict,
				"CRM lead was modified; refresh and retry",
			)
		}

		fromStageID := lead.StageID
		history := models.CRMStageHistory{
			ID:             uuid.New(),
			OrganizationID: orgID,
			LeadID:         lead.ID,
			FromStageID:    &fromStageID,
			ToStageID:      targetStage.ID,
			ChangedByID:    &userID,
			Reason:         strings.TrimSpace(req.Reason),
			Metadata:       req.Metadata,
			ChangedAt:      now,
		}
		if history.Metadata == nil {
			history.Metadata = models.JSONB{}
		}
		if err := tx.Create(&history).Error; err != nil {
			return err
		}

		outbox := models.OutboxEvent{
			BaseModel:      models.BaseModel{ID: uuid.New()},
			OrganizationID: orgID,
			EventType:      "crm.lead.stage_moved",
			AggregateType:  productCRMLeadResource,
			AggregateID:    &lead.ID,
			Payload: models.JSONB{
				"lead_id":       lead.ID.String(),
				"pipeline_id":   lead.PipelineID.String(),
				"from_stage_id": fromStageID.String(),
				"to_stage_id":   targetStage.ID.String(),
				"status":        string(status),
				"version":       newVersion,
				"changed_by_id": userID.String(),
				"reason":        strings.TrimSpace(req.Reason),
			},
			AvailableAt:    now,
			Status:         models.OutboxEventStatusPending,
			Attempts:       0,
			MaxAttempts:    10,
			IdempotencyKey: fmt.Sprintf("crm-lead-stage-move:%s:%d", lead.ID, newVersion),
			Version:        1,
		}
		if err := tx.Create(&outbox).Error; err != nil {
			return err
		}

		if err := tx.Where("id = ? AND organization_id = ?", leadID, orgID).
			First(&updated).Error; err != nil {
			return err
		}
		return audit.LogAudit(
			tx,
			orgID,
			userID,
			audit.GetUserName(tx, userID),
			productCRMLeadResource,
			lead.ID,
			models.AuditActionUpdated,
			oldSnapshot,
			crmLeadAuditSnapshot(&updated),
			map[string]any{
				"field":     "stage_transition_reason",
				"old_value": nil,
				"new_value": strings.TrimSpace(req.Reason),
			},
		)
	})
	if err != nil {
		return a.sendProductCRMWriteError(r, "move CRM lead", err)
	}

	if err := a.loadCRMLead(&updated, orgID, true); err != nil {
		a.Log.Error("Failed to reload moved CRM lead", "error", err, "lead_id", leadID)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to load CRM lead", nil, "")
	}
	return r.SendEnvelope(crmLeadToResponse(&updated))
}

// ListFollowUpTasks lists tenant tasks with ownership, status, overdue, and due
// date filters.
func (a *App) ListFollowUpTasks(r *fastglue.Request) error {
	orgID, userID, err := a.requireAuth(r, models.ResourceTasks, models.ActionRead)
	if err != nil {
		return nil
	}

	pg := parsePagination(r)
	query := a.DB.Model(&models.FollowUpTask{}).
		Where("follow_up_tasks.organization_id = ?", orgID)

	for _, filter := range []struct {
		param  string
		column string
		label  string
	}{
		{"contact_id", "contact_id", "contact"},
		{"lead_id", "lead_id", "lead"},
		{"booking_id", "booking_id", "booking"},
		{"owner_user_id", "owner_user_id", "owner user"},
	} {
		value, parseErr := productCRMQueryUUID(r, filter.param)
		if parseErr != nil {
			return r.SendErrorEnvelope(
				fasthttp.StatusBadRequest,
				"Invalid "+filter.label+" ID",
				nil,
				"",
			)
		}
		if value != nil {
			query = query.Where(filter.column+" = ?", *value)
		}
	}

	if raw := strings.TrimSpace(string(r.RequestCtx.QueryArgs().Peek("mine"))); raw != "" {
		mine, parseErr := strconv.ParseBool(raw)
		if parseErr != nil {
			return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "mine must be true or false", nil, "")
		}
		if mine {
			query = query.Where("owner_user_id = ?", userID)
		}
	}
	if raw := strings.TrimSpace(string(r.RequestCtx.QueryArgs().Peek("overdue"))); raw != "" {
		overdue, parseErr := strconv.ParseBool(raw)
		if parseErr != nil {
			return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "overdue must be true or false", nil, "")
		}
		if overdue {
			query = query.
				Where("due_at < ?", time.Now().UTC()).
				Where("status IN ?", productCRMActiveTaskStatuses())
		}
	}
	if raw := strings.TrimSpace(string(r.RequestCtx.QueryArgs().Peek("status"))); raw != "" {
		status := models.FollowUpTaskStatus(raw)
		if !validFollowUpTaskStatus(status) {
			return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid task status", nil, "")
		}
		query = query.Where("status = ?", status)
	}
	if raw := strings.TrimSpace(string(r.RequestCtx.QueryArgs().Peek("priority"))); raw != "" {
		priority := models.FollowUpTaskPriority(raw)
		if !validFollowUpTaskPriority(priority) {
			return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid task priority", nil, "")
		}
		query = query.Where("priority = ?", priority)
	}
	if search := strings.TrimSpace(string(r.RequestCtx.QueryArgs().Peek("search"))); search != "" {
		if utf8.RuneCountInString(search) > 255 {
			return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "search must be at most 255 characters", nil, "")
		}
		pattern := productCRMSearchPattern(search)
		query = query.Where("(title ILIKE ? OR description ILIKE ?)", pattern, pattern)
	}

	for _, dateFilter := range []struct {
		param     string
		operator  string
		endOfDate bool
	}{
		{"due_from", ">=", false},
		{"due_to", "<=", true},
	} {
		value, parseErr := productCRMOptionalDate(r, dateFilter.param)
		if parseErr != nil {
			return r.SendErrorEnvelope(
				fasthttp.StatusBadRequest,
				dateFilter.param+" must use YYYY-MM-DD",
				nil,
				"",
			)
		}
		if value != nil {
			date := *value
			if dateFilter.endOfDate {
				date = endOfDay(date)
			}
			query = query.Where("due_at "+dateFilter.operator+" ?", date)
		}
	}

	var total int64
	if err := query.Count(&total).Error; err != nil {
		a.Log.Error("Failed to count follow-up tasks", "error", err)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to list follow-up tasks", nil, "")
	}

	var tasks []models.FollowUpTask
	err = productCRMTaskPreloads(pg.Apply(query), orgID).
		Order("due_at ASC NULLS LAST, follow_up_tasks.created_at DESC").
		Find(&tasks).Error
	if err != nil {
		a.Log.Error("Failed to list follow-up tasks", "error", err)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to list follow-up tasks", nil, "")
	}

	response := make([]FollowUpTaskResponse, len(tasks))
	for i := range tasks {
		response[i] = followUpTaskToResponse(&tasks[i])
	}
	return r.SendEnvelope(listEnvelope("tasks", response, total, pg))
}

// GetFollowUpTaskSummary returns organization-wide and current-user task counts.
func (a *App) GetFollowUpTaskSummary(r *fastglue.Request) error {
	orgID, userID, err := a.requireAuth(r, models.ResourceTasks, models.ActionRead)
	if err != nil {
		return nil
	}

	now := time.Now().UTC()
	todayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	todayEnd := endOfDay(todayStart)
	var summary FollowUpTaskSummary
	err = a.DB.Model(&models.FollowUpTask{}).
		Where("organization_id = ?", orgID).
		Select(`
			COUNT(*) AS total,
			COUNT(*) FILTER (WHERE status = ?) AS open,
			COUNT(*) FILTER (WHERE status = ?) AS in_progress,
			COUNT(*) FILTER (WHERE status = ?) AS completed,
			COUNT(*) FILTER (WHERE status = ?) AS cancelled,
			COUNT(*) FILTER (
				WHERE due_at < ? AND status IN (?, ?)
			) AS overdue,
			COUNT(*) FILTER (
				WHERE due_at >= ? AND due_at <= ? AND status IN (?, ?)
			) AS due_today,
			COUNT(*) FILTER (
				WHERE owner_user_id = ? AND status IN (?, ?)
			) AS assigned_to_me
		`,
			models.FollowUpTaskStatusOpen,
			models.FollowUpTaskStatusInProgress,
			models.FollowUpTaskStatusCompleted,
			models.FollowUpTaskStatusCancelled,
			now,
			models.FollowUpTaskStatusOpen,
			models.FollowUpTaskStatusInProgress,
			todayStart,
			todayEnd,
			models.FollowUpTaskStatusOpen,
			models.FollowUpTaskStatusInProgress,
			userID,
			models.FollowUpTaskStatusOpen,
			models.FollowUpTaskStatusInProgress,
		).
		Scan(&summary).Error
	if err != nil {
		a.Log.Error("Failed to summarize follow-up tasks", "error", err)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to summarize follow-up tasks", nil, "")
	}
	return r.SendEnvelope(summary)
}

// CreateFollowUpTask creates a task, reminder job, lead activity update, and
// audit record in one transaction.
func (a *App) CreateFollowUpTask(r *fastglue.Request) error {
	orgID, userID, err := a.requireAuth(r, models.ResourceTasks, models.ActionWrite)
	if err != nil {
		return nil
	}

	var req CreateFollowUpTaskRequest
	if err := a.decodeRequest(r, &req); err != nil {
		return nil
	}
	if err := validateCreateFollowUpTaskRequest(&req); err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, err.Error(), nil, "")
	}
	if req.Priority == "" {
		req.Priority = models.FollowUpTaskPriorityNormal
	}
	if req.Metadata == nil {
		req.Metadata = models.JSONB{}
	}
	if req.OwnerUserID == nil {
		req.OwnerUserID = &userID
	}
	req.DueAt = productCRMUTC(req.DueAt)
	req.RemindAt = productCRMUTC(req.RemindAt)
	requestFingerprint, err := productCRMRequestFingerprint(req)
	if err != nil {
		a.Log.Error("Failed to fingerprint follow-up task request", "error", err)
		return r.SendErrorEnvelope(
			fasthttp.StatusInternalServerError,
			"Failed to create follow-up task",
			nil,
			"",
		)
	}

	var task models.FollowUpTask
	err = a.DB.Transaction(func(tx *gorm.DB) error {
		if req.IdempotencyKey != "" {
			err := tx.Where(
				"organization_id = ? AND idempotency_key = ?",
				orgID,
				req.IdempotencyKey,
			).First(&task).Error
			if err == nil {
				return validateProductCRMReplay(
					task.RequestFingerprint,
					requestFingerprint,
					"follow-up task",
				)
			}
			if !errors.Is(err, gorm.ErrRecordNotFound) {
				return err
			}
		}

		if err := validateFollowUpTaskReferences(
			tx,
			orgID,
			req.ContactID,
			req.LeadID,
			req.BookingID,
			req.OwnerUserID,
			req.ParentTaskID,
			uuid.Nil,
		); err != nil {
			return err
		}
		if req.LeadID != nil && req.ContactID == nil {
			var lead models.CRMLead
			if err := tx.Select("contact_id").
				Where("id = ? AND organization_id = ?", *req.LeadID, orgID).
				First(&lead).Error; err != nil {
				return err
			}
			req.ContactID = &lead.ContactID
		} else if req.BookingID != nil && req.ContactID == nil {
			var booking models.Booking
			if err := tx.Select("contact_id").
				Where("id = ? AND organization_id = ?", *req.BookingID, orgID).
				First(&booking).Error; err != nil {
				return err
			}
			req.ContactID = &booking.ContactID
		}
		if err := validateFollowUpTaskSchedule(req.DueAt, req.RemindAt); err != nil {
			return newProductCRMClientError(fasthttp.StatusBadRequest, err.Error())
		}

		now := time.Now().UTC()
		task = models.FollowUpTask{
			BaseModel:          models.BaseModel{ID: uuid.New()},
			OrganizationID:     orgID,
			ContactID:          req.ContactID,
			LeadID:             req.LeadID,
			BookingID:          req.BookingID,
			Title:              strings.TrimSpace(req.Title),
			Description:        strings.TrimSpace(req.Description),
			Status:             models.FollowUpTaskStatusOpen,
			Priority:           req.Priority,
			OwnerUserID:        req.OwnerUserID,
			DueAt:              productCRMUTC(req.DueAt),
			RemindAt:           productCRMUTC(req.RemindAt),
			RecurrenceRule:     strings.TrimSpace(req.RecurrenceRule),
			ParentTaskID:       req.ParentTaskID,
			Source:             strings.TrimSpace(req.Source),
			IdempotencyKey:     strings.TrimSpace(req.IdempotencyKey),
			RequestFingerprint: requestFingerprint,
			Metadata:           req.Metadata,
			Version:            1,
			CreatedByID:        &userID,
			UpdatedByID:        &userID,
		}
		var create *gorm.DB
		if req.IdempotencyKey == "" {
			create = tx.Create(&task)
		} else {
			create = tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&task)
		}
		if create.Error != nil {
			return create.Error
		}
		if create.RowsAffected == 0 {
			var existing models.FollowUpTask
			if err := tx.Where(
				"organization_id = ? AND idempotency_key = ?",
				orgID,
				req.IdempotencyKey,
			).First(&existing).Error; err != nil {
				return err
			}
			if err := validateProductCRMReplay(
				existing.RequestFingerprint,
				requestFingerprint,
				"follow-up task",
			); err != nil {
				return err
			}
			task = existing
			return nil
		}
		if err := syncFollowUpTaskReminder(tx, &task, now); err != nil {
			return err
		}
		if err := touchProductCRMLead(tx, orgID, task.LeadID, now); err != nil {
			return err
		}
		return audit.LogAudit(
			tx,
			orgID,
			userID,
			audit.GetUserName(tx, userID),
			productCRMTaskResource,
			task.ID,
			models.AuditActionCreated,
			nil,
			followUpTaskAuditSnapshot(&task),
		)
	})
	if err != nil {
		return a.sendProductCRMWriteError(r, "create follow-up task", err)
	}

	if err := a.loadFollowUpTask(&task, orgID); err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to load follow-up task", nil, "")
	}
	return r.SendEnvelope(followUpTaskToResponse(&task))
}

// UpdateFollowUpTask updates a task, atomically refreshing its reminder job and
// the linked lead's activity timestamp.
func (a *App) UpdateFollowUpTask(r *fastglue.Request) error {
	orgID, userID, err := a.requireAuth(r, models.ResourceTasks, models.ActionWrite)
	if err != nil {
		return nil
	}
	taskID, err := parsePathUUID(r, "id", "task")
	if err != nil {
		return nil
	}

	var req UpdateFollowUpTaskRequest
	if err := a.decodeRequest(r, &req); err != nil {
		return nil
	}
	if err := validateUpdateFollowUpTaskRequest(&req); err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, err.Error(), nil, "")
	}

	var updated models.FollowUpTask
	err = a.DB.Transaction(func(tx *gorm.DB) error {
		var task models.FollowUpTask
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ? AND organization_id = ?", taskID, orgID).
			First(&task).Error; err != nil {
			return newProductCRMClientError(fasthttp.StatusNotFound, "Follow-up task not found")
		}
		if task.Version != req.Version {
			return newProductCRMClientError(
				fasthttp.StatusConflict,
				"Follow-up task was modified; refresh and retry",
			)
		}

		finalContactID := task.ContactID
		finalLeadID := task.LeadID
		finalBookingID := task.BookingID
		finalOwnerID := task.OwnerUserID
		finalParentID := task.ParentTaskID
		finalDueAt := task.DueAt
		finalRemindAt := task.RemindAt

		if req.ClearContactID {
			finalContactID = nil
		} else if req.ContactID != nil {
			finalContactID = req.ContactID
		}
		if req.ClearLeadID {
			finalLeadID = nil
		} else if req.LeadID != nil {
			finalLeadID = req.LeadID
		}
		if req.ClearBookingID {
			finalBookingID = nil
		} else if req.BookingID != nil {
			finalBookingID = req.BookingID
		}
		if req.ClearOwnerUserID {
			finalOwnerID = nil
		} else if req.OwnerUserID != nil {
			finalOwnerID = req.OwnerUserID
		}
		if req.ClearParentTaskID {
			finalParentID = nil
		} else if req.ParentTaskID != nil {
			finalParentID = req.ParentTaskID
		}
		if req.ClearDueAt {
			finalDueAt = nil
		} else if req.DueAt != nil {
			finalDueAt = productCRMUTC(req.DueAt)
		}
		if req.ClearRemindAt {
			finalRemindAt = nil
		} else if req.RemindAt != nil {
			finalRemindAt = productCRMUTC(req.RemindAt)
		}

		if err := validateFollowUpTaskReferences(
			tx,
			orgID,
			finalContactID,
			finalLeadID,
			finalBookingID,
			finalOwnerID,
			finalParentID,
			task.ID,
		); err != nil {
			return err
		}
		if req.Status != nil &&
			(task.Status == models.FollowUpTaskStatusCompleted ||
				task.Status == models.FollowUpTaskStatusCancelled) {
			return newProductCRMClientError(
				fasthttp.StatusConflict,
				"Use the reopen task endpoint before changing a terminal task's status",
			)
		}
		if finalLeadID != nil && finalContactID == nil {
			var lead models.CRMLead
			if err := tx.Select("contact_id").
				Where("id = ? AND organization_id = ?", *finalLeadID, orgID).
				First(&lead).Error; err != nil {
				return err
			}
			finalContactID = &lead.ContactID
		}
		if err := validateFollowUpTaskSchedule(finalDueAt, finalRemindAt); err != nil {
			return newProductCRMClientError(fasthttp.StatusBadRequest, err.Error())
		}

		oldSnapshot := followUpTaskAuditSnapshot(&task)
		now := time.Now().UTC()
		updates := map[string]any{
			"contact_id":     finalContactID,
			"lead_id":        finalLeadID,
			"booking_id":     finalBookingID,
			"owner_user_id":  finalOwnerID,
			"parent_task_id": finalParentID,
			"due_at":         finalDueAt,
			"remind_at":      finalRemindAt,
			"updated_by_id":  userID,
			"version":        gorm.Expr("version + 1"),
		}
		if req.Title != nil {
			updates["title"] = strings.TrimSpace(*req.Title)
		}
		if req.Description != nil {
			updates["description"] = strings.TrimSpace(*req.Description)
		}
		if req.Status != nil {
			updates["status"] = *req.Status
			if *req.Status != models.FollowUpTaskStatusCompleted {
				updates["completed_at"] = nil
				updates["completed_by_id"] = nil
			}
		}
		if req.Priority != nil {
			updates["priority"] = *req.Priority
		}
		if req.RecurrenceRule != nil {
			updates["recurrence_rule"] = strings.TrimSpace(*req.RecurrenceRule)
		}
		if req.Source != nil {
			updates["source"] = strings.TrimSpace(*req.Source)
		}
		if req.Metadata != nil {
			updates["metadata"] = *req.Metadata
		}

		result := tx.Model(&models.FollowUpTask{}).
			Where("id = ? AND organization_id = ? AND version = ?", taskID, orgID, req.Version).
			Updates(updates)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return newProductCRMClientError(
				fasthttp.StatusConflict,
				"Follow-up task was modified; refresh and retry",
			)
		}
		if err := tx.Where("id = ? AND organization_id = ?", taskID, orgID).
			First(&updated).Error; err != nil {
			return err
		}

		if err := syncFollowUpTaskReminder(tx, &updated, now); err != nil {
			return err
		}
		if err := touchProductCRMLead(tx, orgID, task.LeadID, now); err != nil {
			return err
		}
		if finalLeadID != nil && (task.LeadID == nil || *finalLeadID != *task.LeadID) {
			if err := touchProductCRMLead(tx, orgID, finalLeadID, now); err != nil {
				return err
			}
		}
		return audit.LogAudit(
			tx,
			orgID,
			userID,
			audit.GetUserName(tx, userID),
			productCRMTaskResource,
			taskID,
			models.AuditActionUpdated,
			oldSnapshot,
			followUpTaskAuditSnapshot(&updated),
		)
	})
	if err != nil {
		return a.sendProductCRMWriteError(r, "update follow-up task", err)
	}

	if err := a.loadFollowUpTask(&updated, orgID); err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to load follow-up task", nil, "")
	}
	return r.SendEnvelope(followUpTaskToResponse(&updated))
}

// CompleteFollowUpTask marks an active task completed.
func (a *App) CompleteFollowUpTask(r *fastglue.Request) error {
	return a.transitionFollowUpTask(r, models.FollowUpTaskStatusCompleted)
}

// ReopenFollowUpTask returns a completed or cancelled task to open.
func (a *App) ReopenFollowUpTask(r *fastglue.Request) error {
	return a.transitionFollowUpTask(r, models.FollowUpTaskStatusOpen)
}

func (a *App) transitionFollowUpTask(r *fastglue.Request, targetStatus models.FollowUpTaskStatus) error {
	orgID, userID, err := a.requireAuth(r, models.ResourceTasks, models.ActionWrite)
	if err != nil {
		return nil
	}
	taskID, err := parsePathUUID(r, "id", "task")
	if err != nil {
		return nil
	}

	var req FollowUpTaskTransitionRequest
	if err := a.decodeRequest(r, &req); err != nil {
		return nil
	}
	if req.Version < 1 {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "version must be at least 1", nil, "")
	}

	var updated models.FollowUpTask
	err = a.DB.Transaction(func(tx *gorm.DB) error {
		var task models.FollowUpTask
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ? AND organization_id = ?", taskID, orgID).
			First(&task).Error; err != nil {
			return newProductCRMClientError(fasthttp.StatusNotFound, "Follow-up task not found")
		}
		if task.Version != req.Version {
			return newProductCRMClientError(
				fasthttp.StatusConflict,
				"Follow-up task was modified; refresh and retry",
			)
		}

		switch targetStatus {
		case models.FollowUpTaskStatusCompleted:
			if task.Status == models.FollowUpTaskStatusCompleted {
				return newProductCRMClientError(fasthttp.StatusConflict, "Follow-up task is already completed")
			}
			if task.Status == models.FollowUpTaskStatusCancelled {
				return newProductCRMClientError(fasthttp.StatusConflict, "Reopen the cancelled task before completing it")
			}
		case models.FollowUpTaskStatusOpen:
			if task.Status != models.FollowUpTaskStatusCompleted &&
				task.Status != models.FollowUpTaskStatusCancelled {
				return newProductCRMClientError(fasthttp.StatusConflict, "Only completed or cancelled tasks can be reopened")
			}
		}

		oldSnapshot := followUpTaskAuditSnapshot(&task)
		now := time.Now().UTC()
		updates := map[string]any{
			"status":        targetStatus,
			"updated_by_id": userID,
			"version":       gorm.Expr("version + 1"),
		}
		if targetStatus == models.FollowUpTaskStatusCompleted {
			updates["completed_at"] = now
			updates["completed_by_id"] = userID
		} else {
			updates["completed_at"] = nil
			updates["completed_by_id"] = nil
		}

		result := tx.Model(&models.FollowUpTask{}).
			Where("id = ? AND organization_id = ? AND version = ?", taskID, orgID, req.Version).
			Updates(updates)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return newProductCRMClientError(
				fasthttp.StatusConflict,
				"Follow-up task was modified; refresh and retry",
			)
		}
		if err := tx.Where("id = ? AND organization_id = ?", taskID, orgID).
			First(&updated).Error; err != nil {
			return err
		}
		if err := syncFollowUpTaskReminder(tx, &updated, now); err != nil {
			return err
		}
		if err := touchProductCRMLead(tx, orgID, updated.LeadID, now); err != nil {
			return err
		}
		return audit.LogAudit(
			tx,
			orgID,
			userID,
			audit.GetUserName(tx, userID),
			productCRMTaskResource,
			taskID,
			models.AuditActionUpdated,
			oldSnapshot,
			followUpTaskAuditSnapshot(&updated),
		)
	})
	if err != nil {
		operation := "complete follow-up task"
		if targetStatus == models.FollowUpTaskStatusOpen {
			operation = "reopen follow-up task"
		}
		return a.sendProductCRMWriteError(r, operation, err)
	}

	if err := a.loadFollowUpTask(&updated, orgID); err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to load follow-up task", nil, "")
	}
	return r.SendEnvelope(followUpTaskToResponse(&updated))
}

func validateCreateCRMPipelineRequest(req *CreateCRMPipelineRequest) error {
	req.Name = strings.TrimSpace(req.Name)
	req.Description = strings.TrimSpace(req.Description)
	if err := productCRMRequiredString("name", req.Name, 150); err != nil {
		return err
	}
	if err := productCRMOptionalString("description", req.Description, 4000); err != nil {
		return err
	}
	if req.DisplayOrder < 0 {
		return errors.New("display_order cannot be negative")
	}
	if len(req.Stages) > 50 {
		return errors.New("stages cannot contain more than 50 entries")
	}
	seenStageNames := make(map[string]struct{}, len(req.Stages))
	for i := range req.Stages {
		if err := validateCRMPipelineStageInput(&req.Stages[i]); err != nil {
			return fmt.Errorf("stages[%d]: %w", i, err)
		}
		normalizedName := strings.ToLower(req.Stages[i].Name)
		if _, exists := seenStageNames[normalizedName]; exists {
			return fmt.Errorf("stages[%d]: duplicate stage name", i)
		}
		seenStageNames[normalizedName] = struct{}{}
	}
	return nil
}

func validateCRMPipelineStageInput(input *CRMPipelineStageInput) error {
	input.Name = strings.TrimSpace(input.Name)
	input.Color = strings.TrimSpace(input.Color)
	if err := productCRMRequiredString("name", input.Name, 150); err != nil {
		return err
	}
	if err := productCRMOptionalString("color", input.Color, 20); err != nil {
		return err
	}
	if input.DisplayOrder != nil && *input.DisplayOrder < 0 {
		return errors.New("display_order cannot be negative")
	}
	if input.Kind == "" {
		input.Kind = models.CRMPipelineStageKindOpen
	}
	if !validCRMPipelineStageKind(input.Kind) {
		return errors.New("kind must be open, won, or lost")
	}
	if input.Probability != nil && (*input.Probability < 0 || *input.Probability > 100) {
		return errors.New("probability must be between 0 and 100")
	}
	if input.SLAHours != nil && (*input.SLAHours < 0 || *input.SLAHours > 87600) {
		return errors.New("sla_hours must be between 0 and 87600")
	}
	return nil
}

func validateUpdateCRMPipelineStageRequest(req *UpdateCRMPipelineStageRequest) error {
	if req.Version < 1 {
		return errors.New("version must be at least 1")
	}
	if req.Name == nil &&
		req.Color == nil &&
		req.DisplayOrder == nil &&
		req.Kind == nil &&
		req.Probability == nil &&
		req.SLAHours == nil &&
		req.IsActive == nil {
		return errors.New("at least one stage field must be supplied")
	}
	if req.Name != nil {
		trimmed := strings.TrimSpace(*req.Name)
		req.Name = &trimmed
		if err := productCRMRequiredString("name", trimmed, 150); err != nil {
			return err
		}
	}
	if req.Color != nil {
		trimmed := strings.TrimSpace(*req.Color)
		req.Color = &trimmed
		if err := productCRMOptionalString("color", trimmed, 20); err != nil {
			return err
		}
	}
	if req.DisplayOrder != nil && *req.DisplayOrder < 0 {
		return errors.New("display_order cannot be negative")
	}
	if req.Kind != nil && !validCRMPipelineStageKind(*req.Kind) {
		return errors.New("kind must be open, won, or lost")
	}
	if req.Probability != nil && (*req.Probability < 0 || *req.Probability > 100) {
		return errors.New("probability must be between 0 and 100")
	}
	if req.SLAHours != nil && (*req.SLAHours < 0 || *req.SLAHours > 87600) {
		return errors.New("sla_hours must be between 0 and 87600")
	}
	return nil
}

func validateCreateCRMLeadRequest(req *CreateCRMLeadRequest) error {
	req.Title = strings.TrimSpace(req.Title)
	req.SourceReference = strings.TrimSpace(req.SourceReference)
	req.IdempotencyKey = strings.TrimSpace(req.IdempotencyKey)
	if req.ContactID == uuid.Nil {
		return errors.New("contact_id is required")
	}
	if req.PipelineID == uuid.Nil {
		return errors.New("pipeline_id is required")
	}
	if req.StageID != nil && *req.StageID == uuid.Nil {
		return errors.New("stage_id must be a valid UUID")
	}
	if req.OwnerUserID != nil && *req.OwnerUserID == uuid.Nil {
		return errors.New("owner_user_id must be a valid UUID")
	}
	if err := productCRMRequiredString("title", req.Title, 255); err != nil {
		return err
	}
	if req.Source != "" && !validCRMLeadSource(req.Source) {
		return errors.New("invalid lead source")
	}
	if err := productCRMOptionalString("source_reference", req.SourceReference, 255); err != nil {
		return err
	}
	if req.ValueMinor < 0 {
		return errors.New("value_minor cannot be negative")
	}
	if req.Currency != "" {
		if _, err := normalizeProductCRMCurrency(req.Currency); err != nil {
			return err
		}
	}
	if err := productCRMOptionalString("idempotency_key", req.IdempotencyKey, 255); err != nil {
		return err
	}
	return nil
}

func validateUpdateCRMLeadRequest(req *UpdateCRMLeadRequest) error {
	if req.Version < 1 {
		return errors.New("version must be at least 1")
	}
	if req.OwnerUserID != nil && req.ClearOwnerUserID {
		return errors.New("owner_user_id and clear_owner_user_id cannot be used together")
	}
	if req.NextActionAt != nil && req.ClearNextActionAt {
		return errors.New("next_action_at and clear_next_action_at cannot be used together")
	}
	if req.ExpectedCloseDate != nil && req.ClearExpectedCloseDate {
		return errors.New("expected_close_date and clear_expected_close_date cannot be used together")
	}
	if req.ContactID != nil && *req.ContactID == uuid.Nil {
		return errors.New("contact_id must be a valid UUID")
	}
	if req.OwnerUserID != nil && *req.OwnerUserID == uuid.Nil {
		return errors.New("owner_user_id must be a valid UUID")
	}
	if req.Title != nil {
		trimmed := strings.TrimSpace(*req.Title)
		req.Title = &trimmed
		if err := productCRMRequiredString("title", trimmed, 255); err != nil {
			return err
		}
	}
	if req.Source != nil && !validCRMLeadSource(*req.Source) {
		return errors.New("invalid lead source")
	}
	if req.SourceReference != nil {
		trimmed := strings.TrimSpace(*req.SourceReference)
		req.SourceReference = &trimmed
		if err := productCRMOptionalString("source_reference", trimmed, 255); err != nil {
			return err
		}
	}
	if req.ValueMinor != nil && *req.ValueMinor < 0 {
		return errors.New("value_minor cannot be negative")
	}
	if req.Currency != nil {
		currency, err := normalizeProductCRMCurrency(*req.Currency)
		if err != nil {
			return err
		}
		req.Currency = &currency
	}
	if req.LostReason != nil {
		trimmed := strings.TrimSpace(*req.LostReason)
		req.LostReason = &trimmed
		if err := productCRMOptionalString("lost_reason", trimmed, 2000); err != nil {
			return err
		}
	}
	if !hasCRMLeadUpdate(req) {
		return errors.New("at least one lead field must be supplied")
	}
	return nil
}

func validateMoveCRMLeadRequest(req *MoveCRMLeadRequest) error {
	req.Reason = strings.TrimSpace(req.Reason)
	if req.StageID == uuid.Nil {
		return errors.New("stage_id is required")
	}
	if req.Version < 1 {
		return errors.New("version must be at least 1")
	}
	if err := productCRMOptionalString("reason", req.Reason, 2000); err != nil {
		return err
	}
	if req.Metadata == nil {
		req.Metadata = models.JSONB{}
	}
	return nil
}

func hasCRMLeadUpdate(req *UpdateCRMLeadRequest) bool {
	return req.ContactID != nil ||
		req.Title != nil ||
		req.OwnerUserID != nil ||
		req.ClearOwnerUserID ||
		req.Source != nil ||
		req.SourceReference != nil ||
		req.ValueMinor != nil ||
		req.Currency != nil ||
		req.NextActionAt != nil ||
		req.ClearNextActionAt ||
		req.ExpectedCloseDate != nil ||
		req.ClearExpectedCloseDate ||
		req.LostReason != nil ||
		req.Metadata != nil
}

func validateCreateFollowUpTaskRequest(req *CreateFollowUpTaskRequest) error {
	req.Title = strings.TrimSpace(req.Title)
	req.Description = strings.TrimSpace(req.Description)
	req.RecurrenceRule = strings.TrimSpace(req.RecurrenceRule)
	req.Source = strings.TrimSpace(req.Source)
	req.IdempotencyKey = strings.TrimSpace(req.IdempotencyKey)

	if err := productCRMRequiredString("title", req.Title, 255); err != nil {
		return err
	}
	if err := productCRMOptionalString("description", req.Description, 5000); err != nil {
		return err
	}
	if req.Priority != "" && !validFollowUpTaskPriority(req.Priority) {
		return errors.New("invalid task priority")
	}
	if err := productCRMUUIDPointer("contact_id", req.ContactID); err != nil {
		return err
	}
	if err := productCRMUUIDPointer("lead_id", req.LeadID); err != nil {
		return err
	}
	if err := productCRMUUIDPointer("booking_id", req.BookingID); err != nil {
		return err
	}
	if err := productCRMUUIDPointer("owner_user_id", req.OwnerUserID); err != nil {
		return err
	}
	if err := productCRMUUIDPointer("parent_task_id", req.ParentTaskID); err != nil {
		return err
	}
	if err := productCRMOptionalString("recurrence_rule", req.RecurrenceRule, 500); err != nil {
		return err
	}
	if err := productCRMOptionalString("source", req.Source, 50); err != nil {
		return err
	}
	if err := productCRMOptionalString("idempotency_key", req.IdempotencyKey, 255); err != nil {
		return err
	}
	return validateFollowUpTaskSchedule(req.DueAt, req.RemindAt)
}

func validateUpdateFollowUpTaskRequest(req *UpdateFollowUpTaskRequest) error {
	if req.Version < 1 {
		return errors.New("version must be at least 1")
	}
	if !hasFollowUpTaskUpdate(req) {
		return errors.New("at least one task field must be supplied")
	}
	for _, conflict := range []struct {
		value bool
		clear bool
		name  string
	}{
		{req.ContactID != nil, req.ClearContactID, "contact_id"},
		{req.LeadID != nil, req.ClearLeadID, "lead_id"},
		{req.BookingID != nil, req.ClearBookingID, "booking_id"},
		{req.OwnerUserID != nil, req.ClearOwnerUserID, "owner_user_id"},
		{req.DueAt != nil, req.ClearDueAt, "due_at"},
		{req.RemindAt != nil, req.ClearRemindAt, "remind_at"},
		{req.ParentTaskID != nil, req.ClearParentTaskID, "parent_task_id"},
	} {
		if conflict.value && conflict.clear {
			return fmt.Errorf("%s and its clear flag cannot be used together", conflict.name)
		}
	}

	if err := productCRMUUIDPointer("contact_id", req.ContactID); err != nil {
		return err
	}
	if err := productCRMUUIDPointer("lead_id", req.LeadID); err != nil {
		return err
	}
	if err := productCRMUUIDPointer("booking_id", req.BookingID); err != nil {
		return err
	}
	if err := productCRMUUIDPointer("owner_user_id", req.OwnerUserID); err != nil {
		return err
	}
	if err := productCRMUUIDPointer("parent_task_id", req.ParentTaskID); err != nil {
		return err
	}
	if req.Title != nil {
		trimmed := strings.TrimSpace(*req.Title)
		req.Title = &trimmed
		if err := productCRMRequiredString("title", trimmed, 255); err != nil {
			return err
		}
	}
	if req.Description != nil {
		trimmed := strings.TrimSpace(*req.Description)
		req.Description = &trimmed
		if err := productCRMOptionalString("description", trimmed, 5000); err != nil {
			return err
		}
	}
	if req.Status != nil {
		if !validFollowUpTaskStatus(*req.Status) {
			return errors.New("invalid task status")
		}
		if *req.Status == models.FollowUpTaskStatusCompleted {
			return errors.New("use the complete task endpoint to mark a task completed")
		}
	}
	if req.Priority != nil && !validFollowUpTaskPriority(*req.Priority) {
		return errors.New("invalid task priority")
	}
	if req.RecurrenceRule != nil {
		trimmed := strings.TrimSpace(*req.RecurrenceRule)
		req.RecurrenceRule = &trimmed
		if err := productCRMOptionalString("recurrence_rule", trimmed, 500); err != nil {
			return err
		}
	}
	if req.Source != nil {
		trimmed := strings.TrimSpace(*req.Source)
		req.Source = &trimmed
		if err := productCRMOptionalString("source", trimmed, 50); err != nil {
			return err
		}
	}
	return nil
}

func hasFollowUpTaskUpdate(req *UpdateFollowUpTaskRequest) bool {
	return req.ContactID != nil ||
		req.ClearContactID ||
		req.LeadID != nil ||
		req.ClearLeadID ||
		req.BookingID != nil ||
		req.ClearBookingID ||
		req.Title != nil ||
		req.Description != nil ||
		req.Status != nil ||
		req.Priority != nil ||
		req.OwnerUserID != nil ||
		req.ClearOwnerUserID ||
		req.DueAt != nil ||
		req.ClearDueAt ||
		req.RemindAt != nil ||
		req.ClearRemindAt ||
		req.RecurrenceRule != nil ||
		req.ParentTaskID != nil ||
		req.ClearParentTaskID ||
		req.Source != nil ||
		req.Metadata != nil
}

func validateFollowUpTaskSchedule(dueAt, remindAt *time.Time) error {
	if dueAt != nil && dueAt.IsZero() {
		return errors.New("due_at must be a valid timestamp")
	}
	if remindAt != nil && remindAt.IsZero() {
		return errors.New("remind_at must be a valid timestamp")
	}
	if dueAt != nil && remindAt != nil && remindAt.After(*dueAt) {
		return errors.New("remind_at cannot be after due_at")
	}
	return nil
}

func productCRMRequiredString(field, value string, max int) error {
	if value == "" {
		return fmt.Errorf("%s is required", field)
	}
	if utf8.RuneCountInString(value) > max {
		return fmt.Errorf("%s must be at most %d characters", field, max)
	}
	return nil
}

func productCRMOptionalString(field, value string, max int) error {
	if utf8.RuneCountInString(value) > max {
		return fmt.Errorf("%s must be at most %d characters", field, max)
	}
	return nil
}

func productCRMUUIDPointer(field string, value *uuid.UUID) error {
	if value != nil && *value == uuid.Nil {
		return fmt.Errorf("%s must be a valid UUID", field)
	}
	return nil
}

func normalizeProductCRMCurrency(value string) (string, error) {
	currency := strings.ToUpper(strings.TrimSpace(value))
	if len(currency) != 3 {
		return "", errors.New("currency must be a three-letter ISO code")
	}
	for _, ch := range currency {
		if ch < 'A' || ch > 'Z' {
			return "", errors.New("currency must be a three-letter ISO code")
		}
	}
	return currency, nil
}

func defaultCRMPipelineStages() []CRMPipelineStageInput {
	open := models.CRMPipelineStageKindOpen
	won := models.CRMPipelineStageKindWon
	lost := models.CRMPipelineStageKindLost
	probability0 := 0
	probability25 := 25
	probability60 := 60
	probability100 := 100
	return []CRMPipelineStageInput{
		{Name: "New", Color: "#64748B", Kind: open, Probability: &probability0},
		{Name: "Contacted", Color: "#3B82F6", Kind: open, Probability: &probability25},
		{Name: "Qualified", Color: "#8B5CF6", Kind: open, Probability: &probability60},
		{Name: "Won", Color: "#10B981", Kind: won, Probability: &probability100},
		{Name: "Lost", Color: "#EF4444", Kind: lost, Probability: &probability0},
	}
}

func crmStageFromInput(
	orgID, userID, pipelineID uuid.UUID,
	input CRMPipelineStageInput,
	fallbackOrder int,
) models.CRMPipelineStage {
	displayOrder := fallbackOrder
	if input.DisplayOrder != nil {
		displayOrder = *input.DisplayOrder
	}
	probability := 0
	if input.Probability != nil {
		probability = *input.Probability
	} else if input.Kind == models.CRMPipelineStageKindWon {
		probability = 100
	}
	slaHours := 0
	if input.SLAHours != nil {
		slaHours = *input.SLAHours
	}
	active := true
	if input.IsActive != nil {
		active = *input.IsActive
	}
	kind := input.Kind
	if kind == "" {
		kind = models.CRMPipelineStageKindOpen
	}
	return models.CRMPipelineStage{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: orgID,
		PipelineID:     pipelineID,
		Name:           strings.TrimSpace(input.Name),
		Color:          strings.TrimSpace(input.Color),
		DisplayOrder:   displayOrder,
		Kind:           kind,
		Probability:    probability,
		SLAHours:       slaHours,
		IsActive:       active,
		Version:        1,
		CreatedByID:    &userID,
		UpdatedByID:    &userID,
	}
}

func validCRMPipelineStageKind(value models.CRMPipelineStageKind) bool {
	switch value {
	case models.CRMPipelineStageKindOpen,
		models.CRMPipelineStageKindWon,
		models.CRMPipelineStageKindLost:
		return true
	default:
		return false
	}
}

func validCRMLeadStatus(value models.CRMLeadStatus) bool {
	switch value {
	case models.CRMLeadStatusOpen,
		models.CRMLeadStatusWon,
		models.CRMLeadStatusLost,
		models.CRMLeadStatusArchived:
		return true
	default:
		return false
	}
}

func validCRMLeadSource(value models.CRMLeadSource) bool {
	switch value {
	case models.CRMLeadSourceWhatsApp,
		models.CRMLeadSourceCampaign,
		models.CRMLeadSourceReferral,
		models.CRMLeadSourceWalkIn,
		models.CRMLeadSourceAPI,
		models.CRMLeadSourceImport,
		models.CRMLeadSourceOther:
		return true
	default:
		return false
	}
}

func validFollowUpTaskStatus(value models.FollowUpTaskStatus) bool {
	switch value {
	case models.FollowUpTaskStatusOpen,
		models.FollowUpTaskStatusInProgress,
		models.FollowUpTaskStatusCompleted,
		models.FollowUpTaskStatusCancelled:
		return true
	default:
		return false
	}
}

func validFollowUpTaskPriority(value models.FollowUpTaskPriority) bool {
	switch value {
	case models.FollowUpTaskPriorityLow,
		models.FollowUpTaskPriorityNormal,
		models.FollowUpTaskPriorityHigh,
		models.FollowUpTaskPriorityUrgent:
		return true
	default:
		return false
	}
}

func productCRMActiveTaskStatuses() []models.FollowUpTaskStatus {
	return []models.FollowUpTaskStatus{
		models.FollowUpTaskStatusOpen,
		models.FollowUpTaskStatusInProgress,
	}
}

func crmLeadStateForStage(
	kind models.CRMPipelineStageKind,
	now time.Time,
) (models.CRMLeadStatus, *time.Time, *time.Time) {
	switch kind {
	case models.CRMPipelineStageKindWon:
		wonAt := now
		return models.CRMLeadStatusWon, &wonAt, nil
	case models.CRMPipelineStageKindLost:
		lostAt := now
		return models.CRMLeadStatusLost, nil, &lostAt
	default:
		return models.CRMLeadStatusOpen, nil, nil
	}
}

func validateCRMLeadCreateReferences(
	tx *gorm.DB,
	orgID uuid.UUID,
	req *CreateCRMLeadRequest,
) (*models.CRMPipeline, *models.CRMPipelineStage, error) {
	if err := ensureProductCRMTenantRecord(
		tx,
		&models.Contact{},
		orgID,
		req.ContactID,
		"contact_id",
	); err != nil {
		return nil, nil, err
	}
	if req.OwnerUserID != nil {
		if err := ensureProductCRMUserMembership(tx, orgID, *req.OwnerUserID, "owner_user_id"); err != nil {
			return nil, nil, err
		}
	}

	var pipeline models.CRMPipeline
	if err := tx.Where(
		"id = ? AND organization_id = ? AND is_active = ?",
		req.PipelineID,
		orgID,
		true,
	).First(&pipeline).Error; err != nil {
		return nil, nil, newProductCRMClientError(
			fasthttp.StatusBadRequest,
			"pipeline_id does not belong to the organization or is inactive",
		)
	}

	var stage models.CRMPipelineStage
	stageQuery := tx.Where(
		"organization_id = ? AND pipeline_id = ? AND is_active = ?",
		orgID,
		pipeline.ID,
		true,
	)
	if req.StageID != nil {
		stageQuery = stageQuery.Where("id = ?", *req.StageID)
	}
	if err := stageQuery.Order("display_order ASC, created_at ASC").First(&stage).Error; err != nil {
		return nil, nil, newProductCRMClientError(
			fasthttp.StatusBadRequest,
			"stage_id does not belong to the active pipeline",
		)
	}
	return &pipeline, &stage, nil
}

func validateFollowUpTaskReferences(
	tx *gorm.DB,
	orgID uuid.UUID,
	contactID, leadID, bookingID, ownerUserID, parentTaskID *uuid.UUID,
	currentTaskID uuid.UUID,
) error {
	var selectedContactID *uuid.UUID
	if contactID != nil {
		if err := ensureProductCRMTenantRecord(
			tx,
			&models.Contact{},
			orgID,
			*contactID,
			"contact_id",
		); err != nil {
			return err
		}
		selectedContactID = contactID
	}
	if leadID != nil {
		var lead models.CRMLead
		if err := tx.Select("id", "contact_id").
			Where("id = ? AND organization_id = ?", *leadID, orgID).
			First(&lead).Error; err != nil {
			return newProductCRMClientError(
				fasthttp.StatusBadRequest,
				"lead_id does not belong to the organization",
			)
		}
		if selectedContactID != nil && lead.ContactID != *selectedContactID {
			return newProductCRMClientError(
				fasthttp.StatusBadRequest,
				"contact_id does not match the selected lead",
			)
		}
		selectedContactID = &lead.ContactID
	}
	if bookingID != nil {
		var booking models.Booking
		if err := tx.Select("id", "contact_id").
			Where("id = ? AND organization_id = ?", *bookingID, orgID).
			First(&booking).Error; err != nil {
			return newProductCRMClientError(
				fasthttp.StatusBadRequest,
				"booking_id does not belong to the organization",
			)
		}
		if selectedContactID != nil && booking.ContactID != *selectedContactID {
			return newProductCRMClientError(
				fasthttp.StatusBadRequest,
				"contact_id does not match the selected booking",
			)
		}
	}
	if ownerUserID != nil {
		if err := ensureProductCRMUserMembership(tx, orgID, *ownerUserID, "owner_user_id"); err != nil {
			return err
		}
	}
	if parentTaskID != nil {
		if currentTaskID != uuid.Nil && *parentTaskID == currentTaskID {
			return newProductCRMClientError(
				fasthttp.StatusBadRequest,
				"A task cannot be its own parent",
			)
		}
		if err := ensureProductCRMTenantRecord(
			tx,
			&models.FollowUpTask{},
			orgID,
			*parentTaskID,
			"parent_task_id",
		); err != nil {
			return err
		}
	}
	return nil
}

func ensureProductCRMTenantRecord(
	tx *gorm.DB,
	model any,
	orgID, recordID uuid.UUID,
	field string,
) error {
	var count int64
	if err := tx.Model(model).
		Where("id = ? AND organization_id = ?", recordID, orgID).
		Count(&count).Error; err != nil {
		return err
	}
	if count != 1 {
		return newProductCRMClientError(
			fasthttp.StatusBadRequest,
			field+" does not belong to the organization",
		)
	}
	return nil
}

func ensureProductCRMUserMembership(
	tx *gorm.DB,
	orgID, userID uuid.UUID,
	field string,
) error {
	var count int64
	err := tx.Table("user_organizations AS uo").
		Joins("JOIN users AS u ON u.id = uo.user_id AND u.deleted_at IS NULL").
		Where(
			"uo.organization_id = ? AND uo.user_id = ? AND uo.deleted_at IS NULL AND u.is_active = ?",
			orgID,
			userID,
			true,
		).
		Count(&count).Error
	if err != nil {
		return err
	}
	if count != 1 {
		return newProductCRMClientError(
			fasthttp.StatusBadRequest,
			field+" does not belong to an active organization member",
		)
	}
	return nil
}

func syncFollowUpTaskReminder(tx *gorm.DB, task *models.FollowUpTask, now time.Time) error {
	if err := tx.Model(&models.ScheduledJob{}).
		Where(
			"organization_id = ? AND aggregate_type = ? AND aggregate_id = ? AND status = ?",
			task.OrganizationID,
			productCRMTaskResource,
			task.ID,
			models.ScheduledJobStatusPending,
		).
		Updates(map[string]any{
			"status":  models.ScheduledJobStatusCancelled,
			"version": gorm.Expr("version + 1"),
		}).Error; err != nil {
		return err
	}

	if task.RemindAt == nil ||
		task.Status == models.FollowUpTaskStatusCompleted ||
		task.Status == models.FollowUpTaskStatusCancelled {
		return nil
	}
	runAt := task.RemindAt.UTC()
	job := models.ScheduledJob{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: task.OrganizationID,
		Kind:           "follow_up_task.reminder",
		AggregateType:  productCRMTaskResource,
		AggregateID:    &task.ID,
		RunAt:          runAt,
		Status:         models.ScheduledJobStatusPending,
		Attempts:       0,
		MaxAttempts:    5,
		IdempotencyKey: fmt.Sprintf(
			"follow-up-task-reminder:%s:%d:%d",
			task.ID,
			task.Version,
			runAt.UnixNano(),
		),
		Payload: models.JSONB{
			"task_id":         task.ID.String(),
			"owner_user_id":   productCRMUUIDString(task.OwnerUserID),
			"scheduled_at":    now.Format(time.RFC3339Nano),
			"task_version":    task.Version,
			"organization_id": task.OrganizationID.String(),
		},
		Version: 1,
	}
	return tx.Create(&job).Error
}

func touchProductCRMLead(
	tx *gorm.DB,
	orgID uuid.UUID,
	leadID *uuid.UUID,
	at time.Time,
) error {
	if leadID == nil {
		return nil
	}
	result := tx.Model(&models.CRMLead{}).
		Where("id = ? AND organization_id = ?", *leadID, orgID).
		Updates(map[string]any{
			"last_activity_at": at,
			"version":          gorm.Expr("version + 1"),
		})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return newProductCRMClientError(
			fasthttp.StatusBadRequest,
			"lead_id does not belong to the organization",
		)
	}
	return nil
}

func productCRMQueryUUID(r *fastglue.Request, param string) (*uuid.UUID, error) {
	raw := strings.TrimSpace(string(r.RequestCtx.QueryArgs().Peek(param)))
	if raw == "" {
		return nil, nil
	}
	value, err := uuid.Parse(raw)
	if err != nil {
		return nil, err
	}
	return &value, nil
}

func productCRMOptionalDate(r *fastglue.Request, param string) (*time.Time, error) {
	raw := strings.TrimSpace(string(r.RequestCtx.QueryArgs().Peek(param)))
	if raw == "" {
		return nil, nil
	}
	value, err := time.Parse("2006-01-02", raw)
	if err != nil {
		return nil, err
	}
	return &value, nil
}

func productCRMSearchPattern(value string) string {
	replacer := strings.NewReplacer(
		`\`, `\\`,
		`%`, `\%`,
		`_`, `\_`,
	)
	return "%" + replacer.Replace(value) + "%"
}

func productCRMUTC(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	utc := value.UTC()
	return &utc
}

func productCRMRequestFingerprint(request any) (string, error) {
	payload, err := json.Marshal(request)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(payload)
	return fmt.Sprintf("%x", sum), nil
}

func validateProductCRMReplay(existing, requested, resource string) error {
	if existing == "" || existing != requested {
		return newProductCRMClientError(
			fasthttp.StatusConflict,
			"idempotency_key was already used with a different "+resource+" payload",
		)
	}
	return nil
}

func productCRMUUIDString(value *uuid.UUID) string {
	if value == nil {
		return ""
	}
	return value.String()
}

func productCRMLeadPreloads(query *gorm.DB, orgID uuid.UUID, includeHistory bool) *gorm.DB {
	query = query.
		Preload("Contact", "organization_id = ?", orgID).
		Preload("Pipeline", "organization_id = ?", orgID).
		Preload("Stage", "organization_id = ?", orgID).
		Preload("Owner", func(db *gorm.DB) *gorm.DB {
			return productCRMUserPreloadScope(db, orgID)
		})
	if includeHistory {
		query = query.
			Preload("History", func(db *gorm.DB) *gorm.DB {
				return db.Where("organization_id = ?", orgID).
					Order("changed_at DESC")
			}).
			Preload("History.FromStage", "organization_id = ?", orgID).
			Preload("History.ToStage", "organization_id = ?", orgID).
			Preload("History.ChangedBy", func(db *gorm.DB) *gorm.DB {
				return productCRMUserPreloadScope(db, orgID)
			})
	}
	return query
}

func productCRMTaskPreloads(query *gorm.DB, orgID uuid.UUID) *gorm.DB {
	return query.
		Preload("Contact", "organization_id = ?", orgID).
		Preload("Lead", "organization_id = ?", orgID).
		Preload("Lead.Pipeline", "organization_id = ?", orgID).
		Preload("Lead.Stage", "organization_id = ?", orgID).
		Preload("Owner", func(db *gorm.DB) *gorm.DB {
			return productCRMUserPreloadScope(db, orgID)
		}).
		Preload("CompletedBy", func(db *gorm.DB) *gorm.DB {
			return productCRMUserPreloadScope(db, orgID)
		})
}

func productCRMUserPreloadScope(db *gorm.DB, orgID uuid.UUID) *gorm.DB {
	return db.Where(
		`EXISTS (
			SELECT 1
			FROM user_organizations AS product_crm_uo
			WHERE product_crm_uo.user_id = users.id
			  AND product_crm_uo.organization_id = ?
			  AND product_crm_uo.deleted_at IS NULL
		)`,
		orgID,
	)
}

func (a *App) loadCRMLead(lead *models.CRMLead, orgID uuid.UUID, includeHistory bool) error {
	query := a.DB.Where("crm_leads.id = ? AND crm_leads.organization_id = ?", lead.ID, orgID)
	return productCRMLeadPreloads(query, orgID, includeHistory).First(lead).Error
}

func (a *App) loadFollowUpTask(task *models.FollowUpTask, orgID uuid.UUID) error {
	query := a.DB.Where(
		"follow_up_tasks.id = ? AND follow_up_tasks.organization_id = ?",
		task.ID,
		orgID,
	)
	return productCRMTaskPreloads(query, orgID).First(task).Error
}

func crmPipelineToResponse(pipeline *models.CRMPipeline) CRMPipelineResponse {
	response := CRMPipelineResponse{
		ID:           pipeline.ID,
		Name:         pipeline.Name,
		Description:  pipeline.Description,
		IsDefault:    pipeline.IsDefault,
		IsActive:     pipeline.IsActive,
		DisplayOrder: pipeline.DisplayOrder,
		Version:      pipeline.Version,
		Stages:       make([]CRMPipelineStageResponse, len(pipeline.Stages)),
		CreatedAt:    pipeline.CreatedAt,
		UpdatedAt:    pipeline.UpdatedAt,
	}
	for i := range pipeline.Stages {
		response.Stages[i] = crmStageToResponse(&pipeline.Stages[i])
	}
	return response
}

func crmStageToResponse(stage *models.CRMPipelineStage) CRMPipelineStageResponse {
	return CRMPipelineStageResponse{
		ID:           stage.ID,
		PipelineID:   stage.PipelineID,
		Name:         stage.Name,
		Color:        stage.Color,
		DisplayOrder: stage.DisplayOrder,
		Kind:         stage.Kind,
		Probability:  stage.Probability,
		SLAHours:     stage.SLAHours,
		IsActive:     stage.IsActive,
		Version:      stage.Version,
		CreatedAt:    stage.CreatedAt,
		UpdatedAt:    stage.UpdatedAt,
	}
}

func crmContactReference(contact *models.Contact) *CRMContactReference {
	if contact == nil {
		return nil
	}
	return &CRMContactReference{
		ID:          contact.ID,
		ProfileName: contact.ProfileName,
		PhoneNumber: contact.PhoneNumber,
	}
}

func crmUserReference(user *models.User) *CRMUserReference {
	if user == nil {
		return nil
	}
	return &CRMUserReference{ID: user.ID, FullName: user.FullName}
}

func crmLeadToResponse(lead *models.CRMLead) CRMLeadResponse {
	response := CRMLeadResponse{
		ID:                lead.ID,
		ContactID:         lead.ContactID,
		PipelineID:        lead.PipelineID,
		StageID:           lead.StageID,
		Title:             lead.Title,
		Status:            lead.Status,
		OwnerUserID:       lead.OwnerUserID,
		Source:            lead.Source,
		SourceReference:   lead.SourceReference,
		ValueMinor:        lead.ValueMinor,
		Currency:          lead.Currency,
		NextActionAt:      lead.NextActionAt,
		ExpectedCloseDate: lead.ExpectedCloseDate,
		LastActivityAt:    lead.LastActivityAt,
		WonAt:             lead.WonAt,
		LostAt:            lead.LostAt,
		LostReason:        lead.LostReason,
		Metadata:          lead.Metadata,
		Version:           lead.Version,
		Contact:           crmContactReference(lead.Contact),
		Owner:             crmUserReference(lead.Owner),
		CreatedAt:         lead.CreatedAt,
		UpdatedAt:         lead.UpdatedAt,
	}
	if response.Metadata == nil {
		response.Metadata = models.JSONB{}
	}
	if lead.Pipeline != nil {
		pipeline := crmPipelineToResponse(lead.Pipeline)
		response.Pipeline = &pipeline
	}
	if lead.Stage != nil {
		stage := crmStageToResponse(lead.Stage)
		response.Stage = &stage
	}
	if len(lead.History) > 0 {
		response.History = make([]CRMStageHistoryResponse, len(lead.History))
		for i := range lead.History {
			item := &lead.History[i]
			history := CRMStageHistoryResponse{
				ID:          item.ID,
				FromStageID: item.FromStageID,
				ToStageID:   item.ToStageID,
				ChangedByID: item.ChangedByID,
				Reason:      item.Reason,
				Metadata:    item.Metadata,
				ChangedAt:   item.ChangedAt,
				ChangedBy:   crmUserReference(item.ChangedBy),
			}
			if history.Metadata == nil {
				history.Metadata = models.JSONB{}
			}
			if item.FromStage != nil {
				fromStage := crmStageToResponse(item.FromStage)
				history.FromStage = &fromStage
			}
			if item.ToStage != nil {
				toStage := crmStageToResponse(item.ToStage)
				history.ToStage = &toStage
			}
			response.History[i] = history
		}
	}
	return response
}

func followUpTaskToResponse(task *models.FollowUpTask) FollowUpTaskResponse {
	response := FollowUpTaskResponse{
		ID:             task.ID,
		ContactID:      task.ContactID,
		LeadID:         task.LeadID,
		BookingID:      task.BookingID,
		Title:          task.Title,
		Description:    task.Description,
		Status:         task.Status,
		Priority:       task.Priority,
		OwnerUserID:    task.OwnerUserID,
		DueAt:          task.DueAt,
		RemindAt:       task.RemindAt,
		CompletedAt:    task.CompletedAt,
		CompletedByID:  task.CompletedByID,
		RecurrenceRule: task.RecurrenceRule,
		ParentTaskID:   task.ParentTaskID,
		Source:         task.Source,
		Metadata:       task.Metadata,
		Version:        task.Version,
		Contact:        crmContactReference(task.Contact),
		Owner:          crmUserReference(task.Owner),
		CompletedBy:    crmUserReference(task.CompletedBy),
		CreatedAt:      task.CreatedAt,
		UpdatedAt:      task.UpdatedAt,
	}
	if response.Metadata == nil {
		response.Metadata = models.JSONB{}
	}
	if task.Lead != nil {
		lead := crmLeadToResponse(task.Lead)
		response.Lead = &lead
	}
	return response
}

func crmPipelineAuditSnapshot(pipeline *models.CRMPipeline) map[string]any {
	if pipeline == nil {
		return nil
	}
	stageNames := make([]string, len(pipeline.Stages))
	for i := range pipeline.Stages {
		stageNames[i] = pipeline.Stages[i].Name
	}
	return map[string]any{
		"name":          pipeline.Name,
		"description":   pipeline.Description,
		"is_default":    pipeline.IsDefault,
		"is_active":     pipeline.IsActive,
		"display_order": pipeline.DisplayOrder,
		"stage_names":   stageNames,
		"version":       pipeline.Version,
	}
}

func crmStageAuditSnapshot(stage *models.CRMPipelineStage) map[string]any {
	if stage == nil {
		return nil
	}
	return map[string]any{
		"pipeline_id":   stage.PipelineID,
		"name":          stage.Name,
		"color":         stage.Color,
		"display_order": stage.DisplayOrder,
		"kind":          stage.Kind,
		"probability":   stage.Probability,
		"sla_hours":     stage.SLAHours,
		"is_active":     stage.IsActive,
		"version":       stage.Version,
	}
}

func crmLeadAuditSnapshot(lead *models.CRMLead) map[string]any {
	if lead == nil {
		return nil
	}
	return map[string]any{
		"contact_id":          lead.ContactID,
		"pipeline_id":         lead.PipelineID,
		"stage_id":            lead.StageID,
		"title":               lead.Title,
		"status":              lead.Status,
		"owner_user_id":       lead.OwnerUserID,
		"source":              lead.Source,
		"source_reference":    lead.SourceReference,
		"value_minor":         lead.ValueMinor,
		"currency":            lead.Currency,
		"next_action_at":      lead.NextActionAt,
		"expected_close_date": lead.ExpectedCloseDate,
		"won_at":              lead.WonAt,
		"lost_at":             lead.LostAt,
		"lost_reason":         lead.LostReason,
		"version":             lead.Version,
	}
}

func followUpTaskAuditSnapshot(task *models.FollowUpTask) map[string]any {
	if task == nil {
		return nil
	}
	return map[string]any{
		"contact_id":      task.ContactID,
		"lead_id":         task.LeadID,
		"booking_id":      task.BookingID,
		"title":           task.Title,
		"description":     task.Description,
		"status":          task.Status,
		"priority":        task.Priority,
		"owner_user_id":   task.OwnerUserID,
		"due_at":          task.DueAt,
		"remind_at":       task.RemindAt,
		"completed_at":    task.CompletedAt,
		"completed_by_id": task.CompletedByID,
		"recurrence_rule": task.RecurrenceRule,
		"parent_task_id":  task.ParentTaskID,
		"source":          task.Source,
		"version":         task.Version,
	}
}
