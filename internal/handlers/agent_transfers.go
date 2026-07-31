package handlers

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/assignment"
	"github.com/shridarpatil/whatomate/internal/contactutil"
	"github.com/shridarpatil/whatomate/internal/database"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/internal/utils"
	"github.com/shridarpatil/whatomate/internal/websocket"
	"github.com/valyala/fasthttp"
	"github.com/zerodha/fastglue"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// agentTransferRow represents a flat row result from the JOINed query
type agentTransferRow struct {
	// AgentTransfer fields
	ID                    uuid.UUID             `gorm:"column:id"`
	OrganizationID        uuid.UUID             `gorm:"column:organization_id"`
	ContactID             uuid.UUID             `gorm:"column:contact_id"`
	WhatsAppAccount       string                `gorm:"column:whatsapp_account"`
	PhoneNumber           string                `gorm:"column:phone_number"`
	Status                models.TransferStatus `gorm:"column:status"`
	Source                models.TransferSource `gorm:"column:source"`
	AgentID               *uuid.UUID            `gorm:"column:agent_id"`
	TeamID                *uuid.UUID            `gorm:"column:team_id"`
	TransferredByUserID   *uuid.UUID            `gorm:"column:transferred_by_user_id"`
	Notes                 string                `gorm:"column:notes"`
	TransferredAt         time.Time             `gorm:"column:transferred_at"`
	ResumedAt             *time.Time            `gorm:"column:resumed_at"`
	ResumedBy             *uuid.UUID            `gorm:"column:resumed_by"`
	SLAResponseDeadline   *time.Time            `gorm:"column:sla_response_deadline"`
	SLAResolutionDeadline *time.Time            `gorm:"column:sla_resolution_deadline"`
	SLABreached           bool                  `gorm:"column:sla_breached"`
	SLABreachedAt         *time.Time            `gorm:"column:sla_breached_at"`
	EscalationLevel       int                   `gorm:"column:escalation_level"`
	EscalatedAt           *time.Time            `gorm:"column:escalated_at"`
	PickedUpAt            *time.Time            `gorm:"column:picked_up_at"`
	ExpiresAt             *time.Time            `gorm:"column:expires_at"`

	// Joined fields
	ContactName       *string `gorm:"column:contact_name"`
	AgentName         *string `gorm:"column:agent_name"`
	TeamName          *string `gorm:"column:team_name"`
	TransferredByName *string `gorm:"column:transferred_by_name"`
	ResumedByName     *string `gorm:"column:resumed_by_name"`
}

// CreateAgentTransferRequest represents the request to create an agent transfer
type CreateAgentTransferRequest struct {
	ContactID       string                `json:"contact_id"`
	WhatsAppAccount string                `json:"whatsapp_account"`
	AgentID         *string               `json:"agent_id"`
	TeamID          *string               `json:"team_id"` // Optional team queue
	Notes           string                `json:"notes"`
	Source          models.TransferSource `json:"source"` // manual, flow, keyword
}

// AssignTransferRequest represents the request to assign a transfer to an agent
type AssignTransferRequest struct {
	AgentID *string `json:"agent_id"` // null or empty string = unassign, UUID = assign to agent
	TeamID  *string `json:"team_id"`  // optional: move to different team queue
}

// AgentTransferResponse represents an agent transfer in API responses
type AgentTransferResponse struct {
	ID                string                `json:"id"`
	ContactID         string                `json:"contact_id"`
	ContactName       string                `json:"contact_name"`
	PhoneNumber       string                `json:"phone_number"`
	WhatsAppAccount   string                `json:"whatsapp_account"`
	Status            models.TransferStatus `json:"status"`
	Source            models.TransferSource `json:"source"`
	AgentID           *string               `json:"agent_id,omitempty"`
	AgentName         *string               `json:"agent_name,omitempty"`
	TeamID            *string               `json:"team_id,omitempty"`
	TeamName          *string               `json:"team_name,omitempty"`
	TransferredBy     *string               `json:"transferred_by,omitempty"`
	TransferredByName *string               `json:"transferred_by_name,omitempty"`
	Notes             string                `json:"notes"`
	TransferredAt     string                `json:"transferred_at"`
	ResumedAt         *string               `json:"resumed_at,omitempty"`
	ResumedBy         *string               `json:"resumed_by,omitempty"`
	ResumedByName     *string               `json:"resumed_by_name,omitempty"`

	// SLA fields
	SLAResponseDeadline   *string `json:"sla_response_deadline,omitempty"`
	SLAResolutionDeadline *string `json:"sla_resolution_deadline,omitempty"`
	SLABreached           bool    `json:"sla_breached"`
	SLABreachedAt         *string `json:"sla_breached_at,omitempty"`
	EscalationLevel       int     `json:"escalation_level"`
	EscalatedAt           *string `json:"escalated_at,omitempty"`
	PickedUpAt            *string `json:"picked_up_at,omitempty"`
	ExpiresAt             *string `json:"expires_at,omitempty"`
}

// transferManagementAccess is deliberately stronger than transfers:write.
// The default Agent role needs transfers:write to create a handoff, but only
// managers with conversation-assignment authority may enumerate or mutate
// arbitrary organization transfers.
func (a *App) transferManagementAccess(
	userID, organizationID uuid.UUID,
) bool {
	return a.HasPermission(
		userID,
		models.ResourceChatAssign,
		models.ActionWrite,
		organizationID,
	)
}

// ListAgentTransfers lists agent transfers for the organization
// Agents see only their assigned transfers + their team queues; Admin see all; Managers see their teams
func (a *App) ListAgentTransfers(r *fastglue.Request) error {
	orgID, userID, err := a.getOrgAndUserID(r)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusUnauthorized, "Unauthorized", nil, "")
	}

	if !a.HasPermission(userID, models.ResourceTransfers, models.ActionRead, orgID) {
		return r.SendErrorEnvelope(fasthttp.StatusForbidden, "You don't have permission to view transfers", nil, "")
	}

	hasFullAccess := a.transferManagementAccess(userID, orgID)

	// Query params
	status := string(r.RequestCtx.QueryArgs().Peek("status"))
	teamIDStr := string(r.RequestCtx.QueryArgs().Peek("team_id"))

	// Pagination params
	limit := 100 // Default limit
	offset := 0
	if limitStr := string(r.RequestCtx.QueryArgs().Peek("limit")); limitStr != "" {
		if parsed, err := strconv.Atoi(limitStr); err == nil && parsed > 0 && parsed <= 100 {
			limit = parsed
		}
	}
	if offsetStr := string(r.RequestCtx.QueryArgs().Peek("offset")); offsetStr != "" {
		if parsed, err := strconv.Atoi(offsetStr); err == nil && parsed >= 0 {
			offset = parsed
		}
	}

	// Lazy loading: parse include parameter for optional relations
	// Example: ?include=contact,agent,team or ?include=all (default: all)
	includeParam := string(r.RequestCtx.QueryArgs().Peek("include"))
	includeAll := includeParam == "" || includeParam == "all"
	includeSet := make(map[string]bool)
	if !includeAll {
		for _, inc := range strings.Split(includeParam, ",") {
			includeSet[strings.TrimSpace(inc)] = true
		}
	}

	// Build SELECT clause based on what relations are needed
	selectCols := []string{"agent_transfers.*"}
	if includeAll || includeSet["contact"] {
		selectCols = append(selectCols, "contacts.profile_name AS contact_name")
	}
	if includeAll || includeSet["agent"] {
		selectCols = append(selectCols, "agent.full_name AS agent_name")
	}
	if includeAll || includeSet["team"] {
		selectCols = append(selectCols, "teams.name AS team_name")
	}
	if includeAll || includeSet["transferred_by"] {
		selectCols = append(selectCols, "transferred_by.full_name AS transferred_by_name")
	}
	if includeAll || includeSet["resumed_by"] {
		selectCols = append(selectCols, "resumed_by.full_name AS resumed_by_name")
	}

	// Active queue uses FIFO (oldest first) so agents pick the longest-waiting
	// transfer; history is browsed by recency, so newest-resumed first.
	orderBy := "agent_transfers.transferred_at ASC" // FIFO for queue
	if status == string(models.TransferStatusResumed) {
		orderBy = "agent_transfers.resumed_at DESC NULLS LAST, agent_transfers.transferred_at DESC"
	}

	// Build query with conditional JOINs for better performance
	query := a.DB.Table("agent_transfers").
		Select(strings.Join(selectCols, ", ")).
		Where("agent_transfers.organization_id = ?", orgID).
		Order(orderBy)

	// Only add JOINs for requested relations (lazy loading)
	if includeAll || includeSet["contact"] {
		query = query.Joins("LEFT JOIN contacts ON contacts.id = agent_transfers.contact_id")
	}
	if includeAll || includeSet["agent"] {
		query = query.Joins("LEFT JOIN users AS agent ON agent.id = agent_transfers.agent_id")
	}
	if includeAll || includeSet["team"] {
		query = query.Joins("LEFT JOIN teams ON teams.id = agent_transfers.team_id")
	}
	if includeAll || includeSet["transferred_by"] {
		query = query.Joins("LEFT JOIN users AS transferred_by ON transferred_by.id = agent_transfers.transferred_by_user_id")
	}
	if includeAll || includeSet["resumed_by"] {
		query = query.Joins("LEFT JOIN users AS resumed_by ON resumed_by.id = agent_transfers.resumed_by")
	}

	// Filter by status if provided
	if status != "" {
		query = query.Where("agent_transfers.status = ?", status)
	}

	// Filter by team if provided
	if teamIDStr != "" {
		if teamIDStr == "general" {
			query = query.Where("agent_transfers.team_id IS NULL")
		} else {
			teamID, err := uuid.Parse(teamIDStr)
			if err != nil {
				return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid team_id", nil, "")
			}
			query = query.Where("agent_transfers.team_id = ?", teamID)
		}
	}

	// Get user's team memberships for filtering (needed for users without full access)
	var userTeamIDs []uuid.UUID
	if !hasFullAccess {
		var memberships []models.TeamMember
		if err := a.DB.Where("user_id = ?", userID).Find(&memberships).Error; err != nil {
			a.Log.Error("Failed to fetch team memberships", "error", err, "user_id", userID)
		}
		for _, m := range memberships {
			userTeamIDs = append(userTeamIDs, m.TeamID)
		}
	}

	// Filter based on permissions
	if !hasFullAccess {
		// Users without full access see their assigned transfers + unassigned in their team queues + general queue
		if len(userTeamIDs) > 0 {
			query = query.Where("agent_transfers.agent_id = ? OR (agent_transfers.agent_id IS NULL AND (agent_transfers.team_id IS NULL OR agent_transfers.team_id IN ?))", userID, userTeamIDs)
		} else {
			// User not in any team - see own transfers + general queue only
			query = query.Where("agent_transfers.agent_id = ? OR (agent_transfers.agent_id IS NULL AND agent_transfers.team_id IS NULL)", userID)
		}
	}
	// Users with full access see all transfers (no filter applied)

	// Get total count before pagination (for frontend to know if more exist)
	var totalCount int64
	countQuery := a.DB.Table("agent_transfers").Where("agent_transfers.organization_id = ?", orgID)
	if status != "" {
		countQuery = countQuery.Where("agent_transfers.status = ?", status)
	}
	if teamIDStr != "" {
		if teamIDStr == "general" {
			countQuery = countQuery.Where("agent_transfers.team_id IS NULL")
		} else if teamID, err := uuid.Parse(teamIDStr); err == nil {
			countQuery = countQuery.Where("agent_transfers.team_id = ?", teamID)
		}
	}
	if !hasFullAccess {
		if len(userTeamIDs) > 0 {
			countQuery = countQuery.Where("agent_transfers.agent_id = ? OR (agent_transfers.agent_id IS NULL AND (agent_transfers.team_id IS NULL OR agent_transfers.team_id IN ?))", userID, userTeamIDs)
		} else {
			countQuery = countQuery.Where("agent_transfers.agent_id = ? OR (agent_transfers.agent_id IS NULL AND agent_transfers.team_id IS NULL)", userID)
		}
	}
	countQuery.Count(&totalCount)

	// Apply pagination
	query = query.Limit(limit).Offset(offset)

	var transfers []agentTransferRow
	if err := query.Scan(&transfers).Error; err != nil {
		a.Log.Error("Failed to fetch transfers", "error", err)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to fetch transfers", nil, "")
	}

	// Get queue counts
	var generalQueueCount int64
	a.DB.Model(&models.AgentTransfer{}).
		Where("organization_id = ? AND status = ? AND agent_id IS NULL AND team_id IS NULL", orgID, models.TransferStatusActive).
		Count(&generalQueueCount)

	// Get team queue counts (filtered by user's teams for non-admin)
	type TeamQueueCount struct {
		TeamID uuid.UUID
		Count  int64
	}
	var teamQueueCounts []TeamQueueCount
	teamCountQuery := a.DB.Model(&models.AgentTransfer{}).
		Select("team_id, COUNT(*) as count").
		Where("organization_id = ? AND status = ? AND agent_id IS NULL AND team_id IS NOT NULL", orgID, models.TransferStatusActive)

	// Filter team counts by user's team membership for users without full access
	if !hasFullAccess && len(userTeamIDs) > 0 {
		teamCountQuery = teamCountQuery.Where("team_id IN ?", userTeamIDs)
	} else if !hasFullAccess && len(userTeamIDs) == 0 {
		// User is not in any team, don't show any team queue counts
		teamQueueCounts = []TeamQueueCount{}
	}

	if hasFullAccess || len(userTeamIDs) > 0 {
		teamCountQuery.Group("team_id").Scan(&teamQueueCounts)
	}

	// Build team counts map
	teamCounts := make(map[string]int64)
	for _, tc := range teamQueueCounts {
		teamCounts[tc.TeamID.String()] = tc.Count
	}

	a.Log.Info("ListAgentTransfers", "org_id", orgID, "has_full_access", hasFullAccess, "user_id", userID, "user_teams", userTeamIDs, "transfers_count", len(transfers), "general_queue", generalQueueCount, "team_queue_counts", teamCounts)

	// Check if phone masking is enabled
	shouldMask := a.ShouldMaskPhoneNumbers(orgID)

	// Queue pickup is an operational policy that transfer readers need in order
	// to render the correct agent controls. Expose only this non-sensitive flag
	// instead of requiring access to the full chatbot settings response.
	//
	// Preserve the historical behaviour for organizations that have not yet
	// created chatbot settings (or if the settings store is temporarily
	// unavailable): queue pickup remains enabled by default.
	allowAgentQueuePickup := true
	if settings, settingsErr := a.getChatbotSettingsCached(orgID, ""); settingsErr == nil && settings != nil {
		allowAgentQueuePickup = settings.AgentAssignment.AllowQueuePickup
	} else if settingsErr != nil && !errors.Is(settingsErr, gorm.ErrRecordNotFound) {
		a.Log.Error(
			"Failed to load chatbot settings for transfer queue policy",
			"error",
			settingsErr,
			"org_id",
			orgID,
		)
	}

	// Build response from flat joined rows
	response := make([]AgentTransferResponse, len(transfers))
	for i, t := range transfers {
		phoneNumber := t.PhoneNumber
		if shouldMask {
			phoneNumber = utils.MaskPhoneNumber(phoneNumber)
		}

		resp := AgentTransferResponse{
			ID:              t.ID.String(),
			ContactID:       t.ContactID.String(),
			PhoneNumber:     phoneNumber,
			WhatsAppAccount: t.WhatsAppAccount,
			Status:          t.Status,
			Source:          t.Source,
			Notes:           t.Notes,
			TransferredAt:   t.TransferredAt.Format(time.RFC3339),
		}

		if t.ContactName != nil {
			contactName := *t.ContactName
			if shouldMask {
				contactName = utils.MaskIfPhoneNumber(contactName)
			}
			resp.ContactName = contactName
		}

		if t.AgentID != nil {
			agentIDStr := t.AgentID.String()
			resp.AgentID = &agentIDStr
			resp.AgentName = t.AgentName
		}

		if t.TransferredByUserID != nil {
			transferredBy := t.TransferredByUserID.String()
			resp.TransferredBy = &transferredBy
			resp.TransferredByName = t.TransferredByName
		}

		if t.TeamID != nil {
			teamIDStr := t.TeamID.String()
			resp.TeamID = &teamIDStr
			resp.TeamName = t.TeamName
		}

		if t.ResumedAt != nil {
			resumedAt := t.ResumedAt.Format(time.RFC3339)
			resp.ResumedAt = &resumedAt
		}

		if t.ResumedBy != nil {
			resumedBy := t.ResumedBy.String()
			resp.ResumedBy = &resumedBy
			resp.ResumedByName = t.ResumedByName
		}

		// SLA fields
		resp.SLABreached = t.SLABreached
		resp.EscalationLevel = t.EscalationLevel
		if t.SLAResponseDeadline != nil {
			deadline := t.SLAResponseDeadline.Format(time.RFC3339)
			resp.SLAResponseDeadline = &deadline
		}
		if t.SLAResolutionDeadline != nil {
			deadline := t.SLAResolutionDeadline.Format(time.RFC3339)
			resp.SLAResolutionDeadline = &deadline
		}
		if t.SLABreachedAt != nil {
			breachedAt := t.SLABreachedAt.Format(time.RFC3339)
			resp.SLABreachedAt = &breachedAt
		}
		if t.EscalatedAt != nil {
			escalatedAt := t.EscalatedAt.Format(time.RFC3339)
			resp.EscalatedAt = &escalatedAt
		}
		if t.PickedUpAt != nil {
			pickedUpAt := t.PickedUpAt.Format(time.RFC3339)
			resp.PickedUpAt = &pickedUpAt
		}
		if t.ExpiresAt != nil {
			expiresAt := t.ExpiresAt.Format(time.RFC3339)
			resp.ExpiresAt = &expiresAt
		}

		response[i] = resp
	}

	return r.SendEnvelope(map[string]any{
		"transfers":                response,
		"general_queue_count":      generalQueueCount,
		"team_queue_counts":        teamCounts,
		"total_count":              totalCount,
		"limit":                    limit,
		"offset":                   offset,
		"allow_agent_queue_pickup": allowAgentQueuePickup,
	})
}

func createActiveAgentTransferTx(
	tx *gorm.DB,
	transfer *models.AgentTransfer,
) error {
	if tx == nil || transfer == nil {
		return errors.New("complete agent transfer transaction state is required")
	}
	if err := database.LockContactPolicyScope(
		tx,
		transfer.OrganizationID,
		transfer.ContactID,
	); err != nil {
		return fmt.Errorf("lock transfer policy scope: %w", err)
	}

	var existingCount int64
	if err := tx.Model(&models.AgentTransfer{}).
		Where(
			"organization_id = ? AND contact_id = ? AND status = ?",
			transfer.OrganizationID,
			transfer.ContactID,
			models.TransferStatusActive,
		).
		Count(&existingCount).Error; err != nil {
		return err
	}
	if existingCount > 0 {
		return errActiveAgentTransferExists
	}
	return tx.Create(transfer).Error
}

// CreateAgentTransfer creates a new agent transfer
func (a *App) CreateAgentTransfer(r *fastglue.Request) error {
	orgID, userID, err := a.getOrgAndUserID(r)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusUnauthorized, "Unauthorized", nil, "")
	}

	if !a.HasPermission(userID, models.ResourceTransfers, models.ActionWrite, orgID) {
		return r.SendErrorEnvelope(fasthttp.StatusForbidden, "You don't have permission to create transfers", nil, "")
	}

	var req CreateAgentTransferRequest
	if err := json.Unmarshal(r.RequestCtx.PostBody(), &req); err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid request body", nil, "")
	}

	if req.ContactID == "" {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "contact_id is required", nil, "")
	}

	contactID, err := uuid.Parse(req.ContactID)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid contact_id", nil, "")
	}

	// Resolve aliases for request validation and assignment decisions. The
	// canonical contact is resolved again under row locks in the creation
	// transaction so a merge racing this request cannot leave a stale FK.
	contact, err := contactutil.ResolveCanonicalContact(a.DB, orgID, contactID)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return r.SendErrorEnvelope(fasthttp.StatusNotFound, "Contact not found", nil, "")
	}
	if err != nil {
		a.Log.Error("Failed to resolve transfer contact", "error", err, "contact_id", contactID)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to resolve contact", nil, "")
	}

	// Get chatbot settings to check AssignToSameAgent (use cache)
	settings, _ := a.getChatbotSettingsCached(orgID, req.WhatsAppAccount)

	// Parse team_id if provided
	var teamID *uuid.UUID
	if req.TeamID != nil && *req.TeamID != "" {
		parsedTeamID, err := uuid.Parse(*req.TeamID)
		if err != nil {
			return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid team_id", nil, "")
		}
		// Verify team exists and is active
		var team models.Team
		if err := a.DB.Where("id = ? AND organization_id = ? AND is_active = ?", parsedTeamID, orgID, true).First(&team).Error; err != nil {
			return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Team not found or inactive", nil, "")
		}
		teamID = &parsedTeamID
	}

	// Determine agent assignment
	var agentID *uuid.UUID

	// First, try explicit agent from request
	if req.AgentID != nil && *req.AgentID != "" {
		parsedAgentID, err := uuid.Parse(*req.AgentID)
		if err != nil {
			return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid agent_id", nil, "")
		}
		// Verify agent exists and is available
		agent, err := findByIDAndOrg[models.User](a.DB, r, parsedAgentID, orgID, "Agent")
		if err != nil {
			return nil
		}
		if !agent.IsAvailable {
			return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Agent is currently away", nil, "")
		}
		agentID = &parsedAgentID
	} else if teamID != nil && a.Assigner != nil {
		// Apply team's assignment strategy
		agentID = a.Assigner.AssignToTeam(*teamID, orgID, nil, assignment.ChatLoadCounter)
	} else if settings != nil && settings.AgentAssignment.AssignToSameAgent && contact.AssignedUserID != nil {
		// Auto-assign to contact's existing assigned agent (if setting enabled and agent is available)
		var assignedAgent models.User
		if a.DB.Where("id = ?", contact.AssignedUserID).First(&assignedAgent).Error == nil && assignedAgent.IsAvailable {
			agentID = contact.AssignedUserID
		}
		// If agent is not available, falls through to queue (agentID remains nil)
	}
	// Otherwise, agentID remains nil (goes to queue)

	// Determine source
	source := req.Source
	if source == "" {
		source = models.TransferSourceManual
	}

	// Create transfer
	transfer := models.AgentTransfer{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: orgID,
		// Retain the requested ID until saveAndFinalizeTransfer locks and
		// revalidates its redirect path.
		ContactID:           contactID,
		WhatsAppAccount:     req.WhatsAppAccount,
		PhoneNumber:         contact.PhoneNumber,
		Status:              models.TransferStatusActive,
		Source:              source,
		AgentID:             agentID,
		TeamID:              teamID,
		TransferredByUserID: &userID,
		Notes:               req.Notes,
		TransferredAt:       time.Now(),
	}

	account := &models.WhatsAppAccount{
		OrganizationID: orgID,
		Name:           req.WhatsAppAccount,
	}
	if err := a.saveAndFinalizeTransfer(&transfer, account, contact, settings, true); err != nil {
		if errors.Is(err, errActiveAgentTransferExists) {
			return r.SendErrorEnvelope(fasthttp.StatusConflict, "Contact already has an active transfer", nil, "")
		}
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return r.SendErrorEnvelope(fasthttp.StatusNotFound, "Contact not found", nil, "")
		}
		a.Log.Error("Failed to create agent transfer", "error", err)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to create transfer", nil, "")
	}

	// Dispatch webhook for transfer created
	var agentIDStr *string
	var agentName *string
	if transfer.AgentID != nil {
		idStr := transfer.AgentID.String()
		agentIDStr = &idStr
	}
	a.DispatchWebhook(orgID, models.WebhookEventTransferCreated, TransferEventData{
		TransferID:      transfer.ID.String(),
		ContactID:       contact.ID.String(),
		ContactPhone:    contact.PhoneNumber,
		ContactName:     contact.ProfileName,
		Source:          transfer.Source,
		Reason:          transfer.Notes,
		AgentID:         agentIDStr,
		AgentName:       agentName,
		WhatsAppAccount: transfer.WhatsAppAccount,
	})

	// Load relations for response
	a.DB.Preload("Agent").Preload("Team").Preload("TransferredByUser").First(&transfer, transfer.ID)

	// Apply phone masking if enabled
	contactName, phoneNumber := a.MaskContactFields(orgID, contact.ProfileName, transfer.PhoneNumber)

	resp := AgentTransferResponse{
		ID:              transfer.ID.String(),
		ContactID:       transfer.ContactID.String(),
		ContactName:     contactName,
		PhoneNumber:     phoneNumber,
		WhatsAppAccount: transfer.WhatsAppAccount,
		Status:          transfer.Status,
		Source:          transfer.Source,
		Notes:           transfer.Notes,
		TransferredAt:   transfer.TransferredAt.Format(time.RFC3339),
	}

	if transfer.AgentID != nil {
		agentIDStr := transfer.AgentID.String()
		resp.AgentID = &agentIDStr
		if transfer.Agent != nil {
			resp.AgentName = &transfer.Agent.FullName
		}
	}

	if transfer.TeamID != nil {
		teamIDStr := transfer.TeamID.String()
		resp.TeamID = &teamIDStr
		if transfer.Team != nil {
			resp.TeamName = &transfer.Team.Name
		}
	}

	if transfer.TransferredByUserID != nil {
		transferredBy := transfer.TransferredByUserID.String()
		resp.TransferredBy = &transferredBy
		if transfer.TransferredByUser != nil {
			resp.TransferredByName = &transfer.TransferredByUser.FullName
		}
	}

	// SLA fields
	resp.SLABreached = transfer.SLA.Breached
	resp.EscalationLevel = transfer.SLA.EscalationLevel
	if transfer.SLA.ResponseDeadline != nil {
		deadline := transfer.SLA.ResponseDeadline.Format(time.RFC3339)
		resp.SLAResponseDeadline = &deadline
	}
	if transfer.SLA.ResolutionDeadline != nil {
		deadline := transfer.SLA.ResolutionDeadline.Format(time.RFC3339)
		resp.SLAResolutionDeadline = &deadline
	}
	if transfer.SLA.PickedUpAt != nil {
		pickedUpAt := transfer.SLA.PickedUpAt.Format(time.RFC3339)
		resp.PickedUpAt = &pickedUpAt
	}
	if transfer.SLA.ExpiresAt != nil {
		expiresAt := transfer.SLA.ExpiresAt.Format(time.RFC3339)
		resp.ExpiresAt = &expiresAt
	}

	return r.SendEnvelope(map[string]any{
		"transfer": resp,
		"message":  "Transfer created successfully",
	})
}

// ResumeFromTransfer resumes chatbot processing for a transferred contact
func (a *App) ResumeFromTransfer(r *fastglue.Request) error {
	orgID, userID, err := a.getOrgAndUserID(r)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusUnauthorized, "Unauthorized", nil, "")
	}

	hasManagementAccess := a.transferManagementAccess(userID, orgID)
	if !hasManagementAccess &&
		!a.HasPermission(userID, models.ResourceTransfers, models.ActionWrite, orgID) {
		return r.SendErrorEnvelope(fasthttp.StatusForbidden, "You don't have permission to resume transfers", nil, "")
	}

	transferID, err := parsePathUUID(r, "id", "transfer")
	if err != nil {
		return nil
	}

	errTransferNotActive := errors.New("transfer is not active")
	errTransferAccessDenied := errors.New("transfer access denied")
	var transfer models.AgentTransfer
	txErr := a.DB.Transaction(func(tx *gorm.DB) error {
		if lockErr := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ? AND organization_id = ?", transferID, orgID).
			First(&transfer).Error; lockErr != nil {
			return lockErr
		}
		if transfer.Status != models.TransferStatusActive {
			return errTransferNotActive
		}
		if !hasManagementAccess &&
			(transfer.AgentID == nil || *transfer.AgentID != userID) {
			return errTransferAccessDenied
		}

		now := time.Now().UTC()
		result := tx.Model(&models.AgentTransfer{}).
			Where(
				"id = ? AND organization_id = ? AND status = ?",
				transfer.ID,
				orgID,
				models.TransferStatusActive,
			).
			Updates(map[string]any{
				"status":     models.TransferStatusResumed,
				"resumed_at": now,
				"resumed_by": userID,
			})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return errTransferNotActive
		}
		transfer.Status = models.TransferStatusResumed
		transfer.ResumedAt = &now
		transfer.ResumedBy = &userID
		return nil
	})
	switch {
	case errors.Is(txErr, gorm.ErrRecordNotFound):
		return r.SendErrorEnvelope(fasthttp.StatusNotFound, "Transfer not found", nil, "")
	case errors.Is(txErr, errTransferNotActive):
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Transfer is not active", nil, "")
	case errors.Is(txErr, errTransferAccessDenied):
		return r.SendErrorEnvelope(fasthttp.StatusForbidden, "You can only resume a transfer assigned to you", nil, "")
	case txErr != nil:
		a.Log.Error("Failed to resume transfer", "error", txErr, "transfer_id", transferID)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to resume transfer", nil, "")
	}

	// Clear chatbot tracking so client inactivity SLA doesn't trigger after transfer is closed
	a.ClearContactChatbotTracking(transfer.ContactID)

	// Broadcast WebSocket notification
	a.broadcastTransferResumed(&transfer)

	// Get contact for webhook data
	var contact models.Contact
	a.DB.Where("id = ? AND organization_id = ?", transfer.ContactID, orgID).First(&contact)

	// Dispatch webhook for transfer resumed
	a.DispatchWebhook(orgID, models.WebhookEventTransferResumed, TransferEventData{
		TransferID:      transfer.ID.String(),
		ContactID:       contact.ID.String(),
		ContactPhone:    contact.PhoneNumber,
		ContactName:     contact.ProfileName,
		Source:          transfer.Source,
		WhatsAppAccount: transfer.WhatsAppAccount,
	})

	return r.SendEnvelope(map[string]any{
		"message": "Transfer resumed, chatbot is now active for this contact",
	})
}

// AssignAgentTransfer assigns a transfer to a specific agent
func (a *App) AssignAgentTransfer(r *fastglue.Request) error {
	orgID, userID, err := a.getOrgAndUserID(r)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusUnauthorized, "Unauthorized", nil, "")
	}

	if !a.transferManagementAccess(userID, orgID) {
		return r.SendErrorEnvelope(fasthttp.StatusForbidden, "You don't have permission to assign transfers", nil, "")
	}

	transferID, err := parsePathUUID(r, "id", "transfer")
	if err != nil {
		return nil
	}

	var req AssignTransferRequest
	if err := a.decodeRequest(r, &req); err != nil {
		return nil
	}

	// Determine target agent
	var targetAgentID *uuid.UUID

	if req.AgentID != nil && *req.AgentID != "" {
		parsedAgentID, err := uuid.Parse(*req.AgentID)
		if err != nil {
			return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid agent_id", nil, "")
		}

		// Verify agent exists and is available
		agent, err := findByIDAndOrg[models.User](a.DB, r, parsedAgentID, orgID, "Agent")
		if err != nil {
			return nil
		}
		if !agent.IsAvailable {
			return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Agent is currently away", nil, "")
		}
		targetAgentID = &parsedAgentID
	}

	var targetTeamID *uuid.UUID
	if req.TeamID != nil && *req.TeamID != "" {
		parsedTeamID, err := uuid.Parse(*req.TeamID)
		if err != nil {
			return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid team_id", nil, "")
		}
		var team models.Team
		if err := a.DB.Where("id = ? AND organization_id = ?", parsedTeamID, orgID).First(&team).Error; err != nil {
			return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Team not found", nil, "")
		}
		targetTeamID = &parsedTeamID
	}

	// Resolve and lock the canonical contact before the transfer row, matching
	// merge lock order. The conditional active-status update prevents a stale
	// assignment request from reopening a concurrently resumed transfer.
	var transferHint struct {
		ContactID uuid.UUID
	}
	if err := a.DB.Model(&models.AgentTransfer{}).
		Select("contact_id").
		Where("id = ? AND organization_id = ?", transferID, orgID).
		Take(&transferHint).Error; err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusNotFound, "Transfer not found", nil, "")
	}

	settings, _ := a.getChatbotSettingsCached(orgID, "")
	errTransferNotActive := errors.New("transfer is not active")
	var transfer models.AgentTransfer
	txErr := canonicalContactWriteTransaction(a.DB, func(tx *gorm.DB) error {
		canonicalContact, resolveErr := contactutil.ResolveCanonicalContactForUpdate(
			tx,
			orgID,
			transferHint.ContactID,
		)
		if resolveErr != nil {
			return resolveErr
		}
		if lockErr := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ? AND organization_id = ?", transferID, orgID).
			First(&transfer).Error; lockErr != nil {
			return lockErr
		}
		if transfer.ContactID != canonicalContact.ID {
			return contactutil.ErrCanonicalContactChanged
		}
		if transfer.Status != models.TransferStatusActive {
			return errTransferNotActive
		}

		previousAgentID := transfer.AgentID
		if req.TeamID != nil {
			transfer.TeamID = targetTeamID
		}
		transfer.AgentID = targetAgentID
		if targetAgentID != nil && transfer.SLA.PickedUpAt == nil {
			a.UpdateSLAOnPickup(&transfer)
		}

		result := tx.Model(&models.AgentTransfer{}).
			Where(
				"id = ? AND organization_id = ? AND status = ?",
				transfer.ID,
				orgID,
				models.TransferStatusActive,
			).
			Updates(map[string]any{
				"agent_id":        transfer.AgentID,
				"team_id":         transfer.TeamID,
				"picked_up_at":    transfer.SLA.PickedUpAt,
				"sla_breached":    transfer.SLA.Breached,
				"sla_breached_at": transfer.SLA.BreachedAt,
			})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return errTransferNotActive
		}

		if targetAgentID != nil &&
			settings != nil &&
			settings.AgentAssignment.AssignToSameAgent {
			if updateErr := tx.Model(&models.Contact{}).
				Where(
					"id = ? AND organization_id = ? AND assigned_user_id IS NULL",
					canonicalContact.ID,
					orgID,
				).
				Update("assigned_user_id", targetAgentID).Error; updateErr != nil {
				return updateErr
			}
		} else if targetAgentID == nil && previousAgentID != nil {
			if updateErr := tx.Model(&models.Contact{}).
				Where(
					"id = ? AND organization_id = ? AND assigned_user_id = ?",
					canonicalContact.ID,
					orgID,
					*previousAgentID,
				).
				Update("assigned_user_id", nil).Error; updateErr != nil {
				return updateErr
			}
		}
		return nil
	})
	switch {
	case errors.Is(txErr, gorm.ErrRecordNotFound):
		return r.SendErrorEnvelope(fasthttp.StatusNotFound, "Transfer not found", nil, "")
	case errors.Is(txErr, errTransferNotActive):
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Transfer is not active", nil, "")
	case txErr != nil:
		a.Log.Error("Failed to assign transfer", "error", txErr, "transfer_id", transferID)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to assign transfer", nil, "")
	}

	// Reload contact details after commit for notification payloads.
	var contact models.Contact
	if contactErr := a.DB.Where(
		"id = ? AND organization_id = ?",
		transfer.ContactID,
		orgID,
	).First(&contact).Error; contactErr == nil {
		transfer.Contact = &contact
	}
	// Broadcast WebSocket notification
	a.broadcastTransferAssigned(&transfer)

	// Dispatch webhook for transfer assigned
	var agentIDStr *string
	var agentName *string
	if targetAgentID != nil {
		idStr := targetAgentID.String()
		agentIDStr = &idStr
		// Get agent name
		var agent models.User
		if a.DB.Where("id = ?", targetAgentID).First(&agent).Error == nil {
			agentName = &agent.FullName
		}
	}
	contactPhone := ""
	contactName := ""
	if transfer.Contact != nil {
		contactPhone = transfer.Contact.PhoneNumber
		contactName = transfer.Contact.ProfileName
	}
	a.DispatchWebhook(orgID, models.WebhookEventTransferAssigned, TransferEventData{
		TransferID:      transfer.ID.String(),
		ContactID:       transfer.ContactID.String(),
		ContactPhone:    contactPhone,
		ContactName:     contactName,
		Source:          transfer.Source,
		AgentID:         agentIDStr,
		AgentName:       agentName,
		WhatsAppAccount: transfer.WhatsAppAccount,
	})

	return r.SendEnvelope(map[string]any{
		"message":  "Transfer assigned successfully",
		"agent_id": targetAgentID,
	})
}

// PickNextTransfer allows an agent to pick the next unassigned transfer from the queue
func (a *App) PickNextTransfer(r *fastglue.Request) error {
	orgID, userID, err := a.getOrgAndUserID(r)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusUnauthorized, "Unauthorized", nil, "")
	}

	hasFullAccess := a.transferManagementAccess(userID, orgID)
	hasPickupPermission := a.HasPermission(userID, models.ResourceTransfers, models.ActionPickup, orgID)

	// Check if agent queue pickup is allowed (use cache)
	settings, err := a.getChatbotSettingsCached(orgID, "")
	if err != nil {
		a.Log.Error("Failed to load chatbot settings for queue pickup check", "error", err, "org_id", orgID)
	}

	// Users without full access need AllowQueuePickup enabled when settings exist.
	// If settings haven't been configured yet (nil), allow pickup by default.
	if !hasFullAccess && settings != nil && !settings.AgentAssignment.AllowQueuePickup {
		return r.SendErrorEnvelope(fasthttp.StatusForbidden, "Queue pickup is not allowed", nil, "")
	}

	// Users without full access need pickup permission
	if !hasFullAccess && !hasPickupPermission {
		return r.SendErrorEnvelope(fasthttp.StatusForbidden, "You don't have permission to pick up transfers", nil, "")
	}

	// Get optional team filter
	teamIDStr := string(r.RequestCtx.QueryArgs().Peek("team_id"))
	var requestedTeamID *uuid.UUID
	if teamIDStr != "" && teamIDStr != "general" {
		teamID, parseErr := uuid.Parse(teamIDStr)
		if parseErr != nil {
			return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid team_id", nil, "")
		}
		requestedTeamID = &teamID
	}

	// Get user's team memberships
	var userTeamIDs []uuid.UUID
	var memberships []models.TeamMember
	if err := a.DB.Where("user_id = ?", userID).Find(&memberships).Error; err != nil {
		a.Log.Error("Failed to fetch team memberships for pick", "error", err, "user_id", userID)
	}
	for _, m := range memberships {
		userTeamIDs = append(userTeamIDs, m.TeamID)
	}

	var transfer models.AgentTransfer
	errTeamAccess := errors.New("agent is not a member of the requested team")
	pickErr := a.DB.Transaction(func(tx *gorm.DB) error {
		// Build query for picking transfer with row-level locking.
		query := tx.Clauses(clause.Locking{Strength: "UPDATE", Options: "SKIP LOCKED"}).
			Where("organization_id = ? AND status = ? AND agent_id IS NULL", orgID, models.TransferStatusActive).
			Order("transferred_at ASC")

		if teamIDStr != "" {
			if teamIDStr == "general" {
				query = query.Where("team_id IS NULL")
			} else {
				teamID := *requestedTeamID
				if !hasFullAccess {
					found := false
					for _, candidateID := range userTeamIDs {
						if candidateID == teamID {
							found = true
							break
						}
					}
					if !found {
						return errTeamAccess
					}
				}
				query = query.Where("team_id = ?", teamID)
			}
		} else if !hasFullAccess {
			if len(userTeamIDs) > 0 {
				query = query.Where("team_id IS NULL OR team_id IN ?", userTeamIDs)
			} else {
				query = query.Where("team_id IS NULL")
			}
		}

		if err := query.First(&transfer).Error; err != nil {
			return err
		}

		transfer.AgentID = &userID
		if transfer.TransferredByUserID == nil {
			transfer.TransferredByUserID = &userID
		}
		a.UpdateSLAOnPickup(&transfer)

		if err := tx.Save(&transfer).Error; err != nil {
			return err
		}

		if settings != nil && settings.AgentAssignment.AssignToSameAgent {
			var contact models.Contact
			if err := tx.Where("id = ?", transfer.ContactID).First(&contact).Error; err == nil && contact.AssignedUserID == nil {
				if err := tx.Model(&models.Contact{}).
					Where("id = ?", transfer.ContactID).
					Update("assigned_user_id", userID).Error; err != nil {
					return err
				}
			}
		}
		return nil
	})
	if errors.Is(pickErr, errTeamAccess) {
		return r.SendErrorEnvelope(fasthttp.StatusForbidden, "You are not a member of this team", nil, "")
	}
	if errors.Is(pickErr, gorm.ErrRecordNotFound) {
		return r.SendEnvelope(map[string]any{
			"message":  "No transfers in queue",
			"transfer": nil,
		})
	}
	if pickErr != nil {
		a.Log.Error("Failed to pick transfer", "error", pickErr, "transfer_id", transfer.ID)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to pick transfer", nil, "")
	}

	// Load related data for response after the savepoint is released.
	a.DB.Where("id = ?", transfer.ContactID).First(&transfer.Contact)
	if transfer.TeamID != nil {
		a.DB.Where("id = ?", transfer.TeamID).First(&transfer.Team)
	}

	// Load agent info
	var agent models.User
	a.DB.First(&agent, userID)

	// Broadcast WebSocket notification
	a.broadcastTransferAssigned(&transfer)

	// Apply phone masking if enabled
	shouldMask := a.ShouldMaskPhoneNumbers(orgID)
	phoneNumber := transfer.PhoneNumber
	if shouldMask {
		phoneNumber = utils.MaskPhoneNumber(phoneNumber)
	}

	resp := AgentTransferResponse{
		ID:              transfer.ID.String(),
		ContactID:       transfer.ContactID.String(),
		PhoneNumber:     phoneNumber,
		WhatsAppAccount: transfer.WhatsAppAccount,
		Status:          transfer.Status,
		Source:          transfer.Source,
		Notes:           transfer.Notes,
		TransferredAt:   transfer.TransferredAt.Format(time.RFC3339),
	}

	if transfer.Contact != nil {
		contactName := transfer.Contact.ProfileName
		if shouldMask {
			contactName = utils.MaskIfPhoneNumber(contactName)
		}
		resp.ContactName = contactName
	}

	agentIDStr := userID.String()
	resp.AgentID = &agentIDStr
	resp.AgentName = &agent.FullName

	if transfer.TeamID != nil {
		teamIDStr := transfer.TeamID.String()
		resp.TeamID = &teamIDStr
		if transfer.Team != nil {
			resp.TeamName = &transfer.Team.Name
		}
	}

	// Set TransferredBy (self-pick)
	if transfer.TransferredByUserID != nil {
		transferredBy := transfer.TransferredByUserID.String()
		resp.TransferredBy = &transferredBy
		resp.TransferredByName = &agent.FullName
	}

	// SLA fields
	resp.SLABreached = transfer.SLA.Breached
	resp.EscalationLevel = transfer.SLA.EscalationLevel
	if transfer.SLA.ResponseDeadline != nil {
		deadline := transfer.SLA.ResponseDeadline.Format(time.RFC3339)
		resp.SLAResponseDeadline = &deadline
	}
	if transfer.SLA.ResolutionDeadline != nil {
		deadline := transfer.SLA.ResolutionDeadline.Format(time.RFC3339)
		resp.SLAResolutionDeadline = &deadline
	}
	if transfer.SLA.BreachedAt != nil {
		breachedAt := transfer.SLA.BreachedAt.Format(time.RFC3339)
		resp.SLABreachedAt = &breachedAt
	}
	if transfer.SLA.PickedUpAt != nil {
		pickedUpAt := transfer.SLA.PickedUpAt.Format(time.RFC3339)
		resp.PickedUpAt = &pickedUpAt
	}
	if transfer.SLA.ExpiresAt != nil {
		expiresAt := transfer.SLA.ExpiresAt.Format(time.RFC3339)
		resp.ExpiresAt = &expiresAt
	}

	return r.SendEnvelope(map[string]any{
		"message":  "Transfer picked successfully",
		"transfer": resp,
	})
}

// hasActiveAgentTransfer checks if a contact has an active agent transfer
func (a *App) hasActiveAgentTransfer(orgID, contactID uuid.UUID) bool {
	var count int64
	a.DB.Model(&models.AgentTransfer{}).
		Where("organization_id = ? AND contact_id = ? AND status = ?", orgID, contactID, models.TransferStatusActive).
		Count(&count)
	return count > 0
}

// WebSocket broadcast helpers

func (a *App) broadcastTransferCreated(transfer *models.AgentTransfer, contact *models.Contact) {
	if a.WSHub == nil {
		return
	}

	contactName, phoneNumber := a.MaskContactFields(transfer.OrganizationID, contact.ProfileName, transfer.PhoneNumber)

	payload := map[string]any{
		"id":               transfer.ID.String(),
		"contact_id":       transfer.ContactID.String(),
		"contact_name":     contactName,
		"phone_number":     phoneNumber,
		"whatsapp_account": transfer.WhatsAppAccount,
		"status":           transfer.Status,
		"source":           transfer.Source,
		"notes":            transfer.Notes,
		"transferred_at":   transfer.TransferredAt.Format(time.RFC3339),
	}

	if transfer.AgentID != nil {
		payload["agent_id"] = transfer.AgentID.String()
	}

	if transfer.TeamID != nil {
		payload["team_id"] = transfer.TeamID.String()
	}

	a.WSHub.BroadcastToOrg(transfer.OrganizationID, websocket.WSMessage{
		Type:    websocket.TypeAgentTransfer,
		Payload: payload,
	})
}

func (a *App) broadcastTransferResumed(transfer *models.AgentTransfer) {
	if a.WSHub == nil {
		return
	}

	payload := map[string]any{
		"id":         transfer.ID.String(),
		"contact_id": transfer.ContactID.String(),
		"status":     transfer.Status,
	}

	if transfer.ResumedAt != nil {
		payload["resumed_at"] = transfer.ResumedAt.Format(time.RFC3339)
	}
	if transfer.ResumedBy != nil {
		payload["resumed_by"] = transfer.ResumedBy.String()
	}

	a.WSHub.BroadcastToOrg(transfer.OrganizationID, websocket.WSMessage{
		Type:    websocket.TypeAgentTransferResume,
		Payload: payload,
	})
}

func (a *App) broadcastTransferAssigned(transfer *models.AgentTransfer) {
	if a.WSHub == nil {
		return
	}

	payload := map[string]any{
		"id":         transfer.ID.String(),
		"contact_id": transfer.ContactID.String(),
		"status":     transfer.Status,
	}

	if transfer.AgentID != nil {
		payload["agent_id"] = transfer.AgentID.String()
	} else {
		payload["agent_id"] = nil
	}

	if transfer.TeamID != nil {
		payload["team_id"] = transfer.TeamID.String()
	} else {
		payload["team_id"] = nil
	}

	a.WSHub.BroadcastToOrg(transfer.OrganizationID, websocket.WSMessage{
		Type:    websocket.TypeAgentTransferAssign,
		Payload: payload,
	})
}

// saveAndFinalizeTransfer handles the common post-creation steps for agent transfers:
// sets SLA deadlines, saves to DB, updates contact assignment, optionally ends chatbot sessions, and broadcasts.
func (a *App) saveAndFinalizeTransfer(transfer *models.AgentTransfer, account *models.WhatsAppAccount, contact *models.Contact, settings *models.ChatbotSettings, endChatbotSession bool) error {
	// Set SLA deadlines
	if settings != nil {
		a.SetSLADeadlines(transfer, settings)
	}

	// If agent is already assigned, mark as picked up
	if transfer.AgentID != nil {
		a.UpdateSLAOnPickup(transfer)
	}

	requestedContactID := transfer.ContactID
	baseTransfer := *transfer
	var savedTransfer models.AgentTransfer
	var canonicalContact models.Contact

	err := canonicalContactWriteTransaction(a.DB, func(tx *gorm.DB) error {
		canonical, err := contactutil.ResolveCanonicalContactForUpdate(
			tx,
			baseTransfer.OrganizationID,
			requestedContactID,
		)
		if err != nil {
			return err
		}

		candidate := baseTransfer
		candidate.ContactID = canonical.ID
		candidate.PhoneNumber = canonical.PhoneNumber
		if err := createActiveAgentTransferTx(tx, &candidate); err != nil {
			return err
		}

		// Update contact assignment if agent assigned, but only when
		// AssignToSameAgent is enabled and no relationship manager is already
		// set. The canonical contact row remains locked throughout.
		if candidate.AgentID != nil &&
			settings != nil &&
			settings.AgentAssignment.AssignToSameAgent &&
			canonical.AssignedUserID == nil {
			if err := tx.Model(&models.Contact{}).
				Where(
					"id = ? AND organization_id = ? AND assigned_user_id IS NULL",
					canonical.ID,
					baseTransfer.OrganizationID,
				).
				Update("assigned_user_id", candidate.AgentID).Error; err != nil {
				return err
			}
			canonical.AssignedUserID = candidate.AgentID
		}

		// End any active chatbot session atomically with the handover.
		if endChatbotSession {
			now := time.Now().UTC()
			if err := tx.Model(&models.ChatbotSession{}).
				Where(
					"organization_id = ? AND contact_id = ? AND status = ?",
					baseTransfer.OrganizationID,
					canonical.ID,
					models.SessionStatusActive,
				).
				Updates(map[string]any{
					"status":       models.SessionStatusCancelled,
					"completed_at": now,
				}).Error; err != nil {
				return err
			}
		}

		savedTransfer = candidate
		canonicalContact = *canonical
		return nil
	})
	if err != nil {
		return err
	}

	*transfer = savedTransfer
	*contact = canonicalContact

	// Broadcast to WebSocket
	a.broadcastTransferCreated(transfer, contact)

	return nil
}

// createTransferToQueue creates an unassigned agent transfer that goes to the queue
func (a *App) createTransferToQueue(account *models.WhatsAppAccount, contact *models.Contact, source models.TransferSource) {
	if a.hasActiveAgentTransfer(account.OrganizationID, contact.ID) {
		a.Log.Debug("Contact already has active transfer, skipping", "contact_id", contact.ID, "source", source)
		return
	}

	settings, _ := a.getChatbotSettingsCached(account.OrganizationID, account.Name)

	// Suppress transfers outside business hours — flow steps and the
	// chatbot-disabled fallback would otherwise hand off to a human at
	// 11pm. createTransferFromKeyword already does this; mirror it here.
	if settings != nil && settings.BusinessHours.Enabled && len(settings.BusinessHours.Hours) > 0 {
		if !a.isWithinBusinessHours(settings.BusinessHours.Hours) {
			a.Log.Info("Outside business hours, sending out-of-hours message instead of queue transfer", "contact_id", contact.ID, "source", source)
			if settings.BusinessHours.OutOfHoursMessage != "" {
				_ = a.sendAndSaveTextMessage(account, contact, settings.BusinessHours.OutOfHoursMessage)
			}
			return
		}
	}

	transfer := models.AgentTransfer{
		BaseModel:       models.BaseModel{ID: uuid.New()},
		OrganizationID:  account.OrganizationID,
		ContactID:       contact.ID,
		WhatsAppAccount: account.Name,
		PhoneNumber:     contact.PhoneNumber,
		Status:          models.TransferStatusActive,
		Source:          source,
		TransferredAt:   time.Now(),
	}

	if err := a.saveAndFinalizeTransfer(&transfer, account, contact, settings, false); err != nil {
		if errors.Is(err, errActiveAgentTransferExists) {
			a.Log.Debug("Concurrent active transfer already exists, skipping queue transfer", "contact_id", contact.ID, "source", source)
			return
		}
		a.Log.Error("Failed to create transfer to queue", "error", err, "contact_id", contact.ID, "source", string(source))
		return
	}

	a.Log.Info("Transfer created to agent queue", "transfer_id", transfer.ID, "contact_id", contact.ID, "source", source)
}

// createTransferFromKeyword creates an agent transfer triggered by a keyword rule
func (a *App) createTransferFromKeyword(account *models.WhatsAppAccount, contact *models.Contact) {
	if a.hasActiveAgentTransfer(account.OrganizationID, contact.ID) {
		a.Log.Info("Contact already has active transfer, skipping keyword transfer", "contact_id", contact.ID)
		return
	}

	settings, _ := a.getChatbotSettingsCached(account.OrganizationID, account.Name)

	// Check business hours - if outside hours, send out of hours message instead of transfer
	if settings != nil && settings.BusinessHours.Enabled && len(settings.BusinessHours.Hours) > 0 {
		if !a.isWithinBusinessHours(settings.BusinessHours.Hours) {
			a.Log.Info("Outside business hours, sending out of hours message instead of transfer", "contact_id", contact.ID)
			if settings.BusinessHours.OutOfHoursMessage != "" {
				_ = a.sendAndSaveTextMessage(account, contact, settings.BusinessHours.OutOfHoursMessage)
			}
			return
		}
	}

	// Determine agent assignment
	var agentID *uuid.UUID
	if settings != nil && settings.AgentAssignment.AssignToSameAgent && contact.AssignedUserID != nil {
		var assignedAgent models.User
		if a.DB.Where("id = ?", contact.AssignedUserID).First(&assignedAgent).Error == nil && assignedAgent.IsAvailable {
			agentID = contact.AssignedUserID
		}
	}

	transfer := models.AgentTransfer{
		BaseModel:       models.BaseModel{ID: uuid.New()},
		OrganizationID:  account.OrganizationID,
		ContactID:       contact.ID,
		WhatsAppAccount: account.Name,
		PhoneNumber:     contact.PhoneNumber,
		Status:          models.TransferStatusActive,
		Source:          models.TransferSourceKeyword,
		AgentID:         agentID,
		TransferredAt:   time.Now(),
	}

	if err := a.saveAndFinalizeTransfer(&transfer, account, contact, settings, true); err != nil {
		if errors.Is(err, errActiveAgentTransferExists) {
			a.Log.Debug("Concurrent active transfer already exists, skipping keyword transfer", "contact_id", contact.ID)
			return
		}
		a.Log.Error("Failed to create keyword-triggered transfer", "error", err, "contact_id", contact.ID)
		return
	}

	var agentIDStr string
	if agentID != nil {
		agentIDStr = agentID.String()
	}
	a.Log.Info("Agent transfer created from keyword rule",
		"transfer_id", transfer.ID,
		"contact_id", contact.ID,
		"agent_id", agentIDStr,
	)
}

// createTransferToTeam creates an agent transfer to a specific team with appropriate assignment
func (a *App) createTransferToTeam(account *models.WhatsAppAccount, contact *models.Contact, teamID uuid.UUID, notes string, source models.TransferSource) {
	if a.hasActiveAgentTransfer(account.OrganizationID, contact.ID) {
		a.Log.Debug("Contact already has active transfer, skipping team transfer", "contact_id", contact.ID, "team_id", teamID)
		return
	}

	settings, _ := a.getChatbotSettingsCached(account.OrganizationID, account.Name)

	// Suppress transfers outside business hours (same reason as
	// createTransferToQueue / createTransferFromKeyword).
	if settings != nil && settings.BusinessHours.Enabled && len(settings.BusinessHours.Hours) > 0 {
		if !a.isWithinBusinessHours(settings.BusinessHours.Hours) {
			a.Log.Info("Outside business hours, sending out-of-hours message instead of team transfer", "contact_id", contact.ID, "team_id", teamID, "source", source)
			if settings.BusinessHours.OutOfHoursMessage != "" {
				_ = a.sendAndSaveTextMessage(account, contact, settings.BusinessHours.OutOfHoursMessage)
			}
			return
		}
	}

	var agentID *uuid.UUID
	if a.Assigner != nil {
		agentID = a.Assigner.AssignToTeam(teamID, account.OrganizationID, nil, assignment.ChatLoadCounter)
	}

	transfer := models.AgentTransfer{
		BaseModel:       models.BaseModel{ID: uuid.New()},
		OrganizationID:  account.OrganizationID,
		ContactID:       contact.ID,
		WhatsAppAccount: account.Name,
		PhoneNumber:     contact.PhoneNumber,
		Status:          models.TransferStatusActive,
		Source:          source,
		AgentID:         agentID,
		TeamID:          &teamID,
		Notes:           notes,
		TransferredAt:   time.Now(),
	}

	if err := a.saveAndFinalizeTransfer(&transfer, account, contact, settings, true); err != nil {
		if errors.Is(err, errActiveAgentTransferExists) {
			a.Log.Debug("Concurrent active transfer already exists, skipping team transfer", "contact_id", contact.ID, "team_id", teamID)
			return
		}
		a.Log.Error("Failed to create team transfer", "error", err, "contact_id", contact.ID, "team_id", teamID)
		return
	}

	var agentIDStrLog string
	if agentID != nil {
		agentIDStrLog = agentID.String()
	}
	a.Log.Info("Agent transfer created to team",
		"transfer_id", transfer.ID,
		"contact_id", contact.ID,
		"team_id", teamID,
		"agent_id", agentIDStrLog,
		"source", source,
	)
}

// ReturnAgentTransfersToQueue returns all active transfers assigned to an agent back to their team queues
// Called when an agent goes offline/unavailable
func (a *App) ReturnAgentTransfersToQueue(userID, orgID uuid.UUID) int {
	var transfers []models.AgentTransfer
	if err := a.DB.Where("agent_id = ? AND organization_id = ? AND status = ?", userID, orgID, models.TransferStatusActive).
		Preload("Contact").Find(&transfers).Error; err != nil {
		a.Log.Error("Failed to find agent transfers for queue return", "error", err, "user_id", userID)
		return 0
	}

	if len(transfers) == 0 {
		return 0
	}

	// Return each transfer to its team queue (or general queue)
	for i := range transfers {
		transfer := &transfers[i]
		previousAgentID := transfer.AgentID
		transfer.AgentID = nil

		if err := a.DB.Save(transfer).Error; err != nil {
			a.Log.Error("Failed to return transfer to queue", "error", err, "transfer_id", transfer.ID)
			continue
		}

		// Clear the contact's relationship-manager pointer only if it was
		// pointing at the agent we just removed. Don't blow away a manually
		// set manager assigned via /api/contacts/<id>/assign.
		if transfer.ContactID != uuid.Nil && previousAgentID != nil && transfer.Contact != nil &&
			transfer.Contact.AssignedUserID != nil && *transfer.Contact.AssignedUserID == *previousAgentID {
			a.DB.Model(&models.Contact{}).Where("id = ?", transfer.ContactID).Update("assigned_user_id", nil)
		}

		// Broadcast the unassignment
		a.broadcastTransferAssigned(transfer)
	}

	a.Log.Info("Returned agent transfers to queue",
		"user_id", userID,
		"count", len(transfers),
	)

	return len(transfers)
}
